/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"time"

	"github.com/cloudflare/cloudflare-go/v7/rules"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

//+kubebuilder:rbac:groups=kflare.dev,resources=wafcustomrules;ratelimitrules,verbs=get;list;watch

const (
	// ipListOperationPollInterval is how soon an IPList checks again on the
	// bulk operation replacing its items. Cloudflare finishes one within a
	// few seconds, even for 10,000 items (verified live).
	ipListOperationPollInterval = 2 * time.Second

	// ipListItemsPageSize is the most items Cloudflare returns in one page
	// (verified live; its default is 25).
	ipListItemsPageSize = 500
)

// IPListHooksAPI is what an IPList's hooks call: a list's items, and the bulk
// operations that replace them. accountID is the list's account.
type IPListHooksAPI interface {
	ListItems(ctx context.Context, accountID, listID string) ([]rules.ListItemListResponse, error)
	ReplaceItems(ctx context.Context, accountID, listID string, items []rules.ListItemUpdateParamsBodyUnion) (string, error)
	Operation(ctx context.Context, accountID, operationID string) (*rules.ListBulkOperationGetResponse, error)
}

// ipListService is cloudflare-go v7's list service with the calls an IPList's
// hooks make, through its services of items and bulk operations.
type ipListService struct {
	*rules.ListService
}

func (s *ipListService) ListItems(ctx context.Context, accountID, listID string) ([]rules.ListItemListResponse, error) {
	params := rules.ListItemListParams{}
	params.AccountID.Value, params.AccountID.Present = accountID, true
	params.PerPage.Value, params.PerPage.Present = ipListItemsPageSize, true
	var items []rules.ListItemListResponse
	pager := s.Items.ListAutoPaging(ctx, listID, params)
	for pager.Next() {
		items = append(items, pager.Current())
	}
	return items, pager.Err()
}

func (s *ipListService) ReplaceItems(ctx context.Context, accountID, listID string,
	items []rules.ListItemUpdateParamsBodyUnion) (string, error) {
	params := rules.ListItemUpdateParams{Body: items}
	params.AccountID.Value, params.AccountID.Present = accountID, true
	res, err := s.Items.Update(ctx, listID, params)
	if err != nil {
		return "", err
	}
	return res.OperationID, nil
}

func (s *ipListService) Operation(ctx context.Context, accountID, operationID string) (*rules.ListBulkOperationGetResponse, error) {
	params := rules.ListBulkOperationGetParams{}
	params.AccountID.Value, params.AccountID.Present = accountID, true
	return s.BulkOperations.Get(ctx, operationID, params)
}

// ipListHooks are what an IPList does beyond the shared loop: its items, the
// lists of other kinds that share its account's names, and the rules that keep
// Cloudflare from deleting it.
var ipListHooks = crudHooks[*cloudflarev1alpha1.IPList, IPListAPI, rules.ListGetResponse]{
	refuse: refuseListOfOtherKind,
	synced: syncIPListItems,
	watch:  watchRulesUsingLists,
}

// refuseListOfOtherKind refuses a list that holds an IPList's name in its
// account but is not an IP list: names are unique across list kinds.
func refuseListOfOtherKind(found *rules.ListGetResponse) *conditionError {
	if found.Kind == rules.ListGetResponseKindIP {
		return nil
	}
	return &conditionError{
		Reason:  reasonNameConflict,
		Message: fmt.Sprintf("The account already has a %s list named %q", found.Kind, found.Name),
	}
}

// syncIPListItems makes a list's items the spec's.
//
// Cloudflare replaces a list's items in a bulk operation that it applies in
// the background, and checks the items before it starts one, so an invalid
// item is refused straight away (verified live). The IPList records the
// operation in status and checks on it shortly, rather than blocking a worker
// until it is done, and starts no other until it has finished.
func syncIPListItems(ctx context.Context, c client.Client, api IPListAPI, ipList *cloudflarev1alpha1.IPList,
	accountID string, current *rules.ListGetResponse) (ctrl.Result, bool, error) {
	metadata := &ipList.Status.CloudflareMetadata
	if id := metadata.OperationID; id != "" {
		operation, err := api.Operation(ctx, accountID, id)
		switch {
		case cfpkg.IsNotFound(err):
			// Cloudflare no longer knows the operation, so it ended long ago.
			// Comparing the items below shows whether it worked.
		case err != nil:
			result, err := handleCloudflareError(ctx, c, ipList, err)
			return result, true, err
		case operation.Status == rules.ListBulkOperationGetResponseStatusPending,
			operation.Status == rules.ListBulkOperationGetResponseStatusRunning:
			return ctrl.Result{RequeueAfter: ipListOperationPollInterval}, true, nil
		case operation.Status == rules.ListBulkOperationGetResponseStatusFailed:
			// Retried with back-off: the next attempt replaces the items again.
			metadata.OperationID = ""
			result, err := handleCloudflareError(ctx, c, ipList,
				fmt.Errorf("replacing the list's items failed in Cloudflare: %s", operation.Error))
			return result, true, err
		}
	}
	metadata.OperationID = ""

	items, err := api.ListItems(ctx, accountID, current.ID)
	if err != nil {
		result, err := handleCloudflareError(ctx, c, ipList, err)
		return result, true, err
	}
	if !ipListItemsMatch(ipList.Spec.Items, items) {
		id, err := api.ReplaceItems(ctx, accountID, current.ID, ipListItemRequests(ipList.Spec.Items))
		if err != nil {
			result, err := handleCloudflareError(ctx, c, ipList, err)
			return result, true, err
		}
		metadata.OperationID = id
		recordDriftCorrection("IPList", ipList)
		log.FromContext(ctx).Info("Replacing IP list items", "listID", current.ID, "operationID", id)
		return ctrl.Result{RequeueAfter: ipListOperationPollInterval}, true,
			updateNotReady(ctx, c, ipList, "ReplacingItems", "Cloudflare is replacing the list's items")
	}
	metadata.NumItems = int32(len(items))
	return ctrl.Result{}, false, nil
}

// ipListItemRequests is what Cloudflare is sent to replace a list's items:
// the spec's items in order, so a position in Cloudflare's error messages is
// an index into spec.items. For no items it is an empty array, which empties
// the list (verified live).
func ipListItemRequests(items []cloudflarev1alpha1.IPListItem) []rules.ListItemUpdateParamsBodyUnion {
	requests := make([]rules.ListItemUpdateParamsBodyUnion, 0, len(items))
	for _, item := range items {
		request := rules.ListItemUpdateParamsBody{}
		request.IP.Value, request.IP.Present = item.IP, true
		if item.Comment != "" {
			request.Comment.Value, request.Comment.Present = item.Comment, true
		}
		requests = append(requests, request)
	}
	return requests
}

// ipListItemsMatch reports whether a list's items in Cloudflare are the
// spec's: the same addresses and ranges with the same comments, in any order.
// Addresses are compared in canonical form, because Cloudflare stores them
// that way. A spec listing one address twice, in two forms, never matches:
// Cloudflare refuses such a list (code 10028), and the IPList reports that.
func ipListItemsMatch(want []cloudflarev1alpha1.IPListItem, got []rules.ListItemListResponse) bool {
	comments := make(map[string]string, len(want))
	for _, item := range want {
		comments[canonicalIP(item.IP)] = item.Comment
	}
	if len(got) != len(want) {
		return false
	}
	for _, item := range got {
		comment, ok := comments[canonicalIP(item.IP)]
		if !ok || comment != item.Comment {
			return false
		}
	}
	return true
}

// canonicalIP returns an address or CIDR range in the form Cloudflare stores
// it: a range of one address is that address, and IPv6 is in lowercase with
// zeros compressed, so 203.0.113.7/32 is 203.0.113.7 and 2001:DB8:0::/64 is
// 2001:db8::/64 (verified live). A value that does not parse is returned as
// it is, for Cloudflare to refuse.
func canonicalIP(value string) string {
	if prefix, err := netip.ParsePrefix(value); err == nil {
		if prefix.IsSingleIP() {
			return prefix.Addr().String()
		}
		return prefix.String()
	}
	if addr, err := netip.ParseAddr(value); err == nil {
		return addr.String()
	}
	return value
}

// listReference matches a custom list in a rule expression, such as
// $office_ips in `ip.src in $office_ips`.
var listReference = regexp.MustCompile(`\$([a-z0-9_]+)`)

// listsInExpression returns the names of the custom lists a rule expression
// uses. It may also return names that are not lists, for example from a
// string such as "/$x": callers only use it to decide what to reconcile.
func listsInExpression(expression string) []string {
	matches := listReference.FindAllStringSubmatch(expression, -1)
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, match[1])
	}
	return names
}

// watchRulesUsingLists watches deleted rules of every kind, so a list that
// Cloudflare would not delete while a rule used it goes as soon as that rule
// is gone.
func watchRulesUsingLists(b *builder.Builder, c client.Client) *builder.Builder {
	for _, kind := range rulesetRuleKinds {
		b = b.Watches(kind.newObject(), handler.EnqueueRequestsFromMapFunc(deletingListsUsedBy(c)),
			builder.WithPredicates(deletesOnly))
	}
	return b
}

// deletingListsUsedBy returns what maps the deletion of a rule to the IPLists
// being deleted whose list the rule's expression uses, in any namespace: a
// list belongs to the account, not to a namespace.
func deletingListsUsedBy(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		rule, ok := obj.(rulesetRuleObject)
		if !ok {
			return nil
		}
		names := listsInExpression(rule.GetRulesetRuleSpec().Expression)
		if len(names) == 0 {
			return nil
		}
		lists := &cloudflarev1alpha1.IPListList{}
		if err := c.List(ctx, lists); err != nil {
			return nil
		}
		var reqs []reconcile.Request
		for i := range lists.Items {
			list := &lists.Items[i]
			if !list.DeletionTimestamp.IsZero() && slices.Contains(names, list.Spec.Name) {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(list)})
			}
		}
		return reqs
	}
}
