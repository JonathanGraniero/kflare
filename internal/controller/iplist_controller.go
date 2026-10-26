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

	cf "github.com/cloudflare/cloudflare-go"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

const (
	// ipListOperationPollInterval is how soon an IPList checks again on the
	// bulk operation replacing its items. Cloudflare finishes one within a
	// few seconds, even for 10,000 items (verified live).
	ipListOperationPollInterval = 2 * time.Second

	// ipListItemsPageSize is the most items Cloudflare returns in one page
	// (verified live; its default is 25).
	ipListItemsPageSize = 500

	// ipListInUseCode is the Cloudflare error code (with HTTP 400) for
	// deleting a list that a rule's expression uses.
	ipListInUseCode = 10041
)

// IPListAPI is the subset of the Cloudflare API used by this controller.
// Declaring a narrow interface keeps unit tests simple: tests inject a fake
// that implements only these methods. In production, *cfpkg.Client satisfies
// this interface because it embeds *cf.API.
type IPListAPI interface {
	ListLists(ctx context.Context, rc *cf.ResourceContainer, params cf.ListListsParams) ([]cf.List, error)
	CreateList(ctx context.Context, rc *cf.ResourceContainer, params cf.ListCreateParams) (cf.List, error)
	UpdateList(ctx context.Context, rc *cf.ResourceContainer, params cf.ListUpdateParams) (cf.List, error)
	DeleteList(ctx context.Context, rc *cf.ResourceContainer, listID string) (cf.ListDeleteResponse, error)
	ListListItems(ctx context.Context, rc *cf.ResourceContainer, params cf.ListListItemsParams) ([]cf.ListItem, error)
	ReplaceListItemsAsync(ctx context.Context, rc *cf.ResourceContainer, params cf.ListReplaceItemsParams) (cf.ListItemCreateResponse, error)
	GetListBulkOperation(ctx context.Context, rc *cf.ResourceContainer, id string) (cf.ListBulkOperation, error)
}

var _ IPListAPI = (*cfpkg.Client)(nil)

// IPListReconciler reconciles an IPList object.
type IPListReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewIPListAPI constructs an IPListAPI from a raw API token.
	// Defaults to defaultIPListAPI; overridden in tests to inject a fake.
	NewIPListAPI func(token string) (IPListAPI, error)
}

//+kubebuilder:rbac:groups=kflare.dev,resources=iplists,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kflare.dev,resources=iplists/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kflare.dev,resources=iplists/finalizers,verbs=update
//+kubebuilder:rbac:groups=kflare.dev,resources=cloudflareaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups=kflare.dev,resources=wafcustomrules;ratelimitrules,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *IPListReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	ipList := &cloudflarev1alpha1.IPList{}
	if err := r.Get(ctx, req.NamespacedName, ipList); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A paused resource is left as it is, deletion included, until its
	// kflare.dev/paused annotation is removed.
	if reconciler.Paused(ipList) {
		return ctrl.Result{}, updatePaused(ctx, r.Client, ipList)
	}

	// Then handle deletion, before anything else.
	if !ipList.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, ipList)
	}

	// Ensure the finalizer is present before doing any work. On the first
	// reconcile it is absent: add it and return; the update re-triggers this
	// reconcile.
	added, err := reconciler.EnsureFinalizer(ctx, r.Client, ipList)
	if err != nil {
		return ctrl.Result{}, err
	}
	if added {
		logger.Info("Added finalizer", "iplist", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	account, token, credErr := resolveAccountToken(ctx, r.Client, ipList.Spec.AccountRef.Name, true)
	if credErr != nil {
		return notReadyRetryAfter(ctx, r.Client, ipList, credErr)
	}
	cfAPI, err := r.newIPListAPI(token)
	if err != nil {
		return invalidToken(ctx, r.Client, ipList, err)
	}

	return r.syncList(ctx, ipList, cf.AccountIdentifier(account.Spec.AccountID), cfAPI)
}

// syncList drives the desired→observed→delta→reconcile loop for an IPList.
//
// Cloudflare replaces a list's items in a bulk operation that it applies in
// the background, and checks the items before it starts one, so an invalid
// item is refused straight away (verified live). The IPList records the
// operation in status and checks on it shortly, rather than blocking a worker
// until it is done, and starts no other until it has finished.
func (r *IPListReconciler) syncList(
	ctx context.Context,
	ipList *cloudflarev1alpha1.IPList,
	rc *cf.ResourceContainer,
	cfAPI IPListAPI,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if id := ipList.Status.CloudflareMetadata.OperationID; id != "" {
		operation, err := cfAPI.GetListBulkOperation(ctx, rc, id)
		switch {
		case cfpkg.IsNotFound(err):
			// Cloudflare no longer knows the operation, so it ended long ago.
			// Comparing the items below shows whether it worked.
		case err != nil:
			return handleCloudflareError(ctx, r.Client, ipList, err)
		case operation.Status == "pending" || operation.Status == "running":
			return ctrl.Result{RequeueAfter: ipListOperationPollInterval}, nil
		case operation.Status == "failed":
			// Retried with back-off: the next attempt replaces the items again.
			ipList.Status.CloudflareMetadata.OperationID = ""
			return handleCloudflareError(ctx, r.Client, ipList,
				fmt.Errorf("replacing the list's items failed in Cloudflare: %s", operation.Error))
		}
	}

	lists, err := cfAPI.ListLists(ctx, rc, cf.ListListsParams{})
	if err != nil {
		return handleCloudflareError(ctx, r.Client, ipList, err)
	}

	// Observe the list by the ID in status. If it is gone, it was deleted
	// outside kflare; a new one is created and the items written to it.
	current, found := listWithID(lists, ipList.Status.CloudflareMetadata.ListID)
	if !found && ipList.Status.CloudflareMetadata.ListID != "" {
		logger.Info("IP list deleted outside kflare, recreating", "listID", ipList.Status.CloudflareMetadata.ListID)
	}

	// No list yet: find the one with this name (names are unique in an
	// account, across list kinds). It is this resource's own when its claim
	// or its creating annotation says so; another IPList may manage it;
	// otherwise the resource adopts it only if its adoption policy allows.
	// Without one, create it, unless the policy is adopt.
	adopting := false
	if !found {
		claimed, err := claimedIDs(ctx, r.Client, &cloudflarev1alpha1.IPListList{},
			cloudflarev1alpha1.IPListIDLabel, ipList)
		if err != nil {
			return ctrl.Result{}, err
		}
		what := fmt.Sprintf("IP list %q", ipList.Spec.Name)
		if existing, ok := listWithName(lists, ipList.Spec.Name); ok {
			if existing.Kind != cf.ListTypeIP {
				return notReadyRetryAfter(ctx, r.Client, ipList, &conditionError{
					Reason:  reasonNameConflict,
					Message: fmt.Sprintf("The account already has a %s list named %q", existing.Kind, existing.Name),
				})
			}
			if owner, taken := claimed[existing.ID]; taken {
				return notReadyRetryAfter(ctx, r.Client, ipList, &conditionError{
					Reason:  reasonNameConflict,
					Message: fmt.Sprintf("%s is already managed by IPList %s", what, owner),
				})
			}
			if ownsID(ipList, cloudflarev1alpha1.IPListIDLabel, existing.ID) || creating(ipList, ipList.Spec.Name) {
				logger.Info("Found the IP list this resource created", "name", existing.Name, "listID", existing.ID)
			} else {
				if condErr := mayAdopt(ipList, what); condErr != nil {
					return notReadyRetryAfter(ctx, r.Client, ipList, condErr)
				}
				adopting = true
				logger.Info("Adopted existing IP list", "name", existing.Name, "listID", existing.ID)
			}
			current = existing
		} else {
			if condErr := mayCreate(ipList, what); condErr != nil {
				return notReadyRetryAfter(ctx, r.Client, ipList, condErr)
			}
			if err := markCreating(ctx, r.Client, ipList, ipList.Spec.Name); err != nil {
				return ctrl.Result{}, err
			}
			current, err = cfAPI.CreateList(ctx, rc, cf.ListCreateParams{
				Name:        ipList.Spec.Name,
				Description: ipList.Spec.Description,
				Kind:        cf.ListTypeIP,
			})
			if err != nil {
				return handleCloudflareError(ctx, r.Client, ipList, err)
			}
			logger.Info("Created IP list", "name", current.Name, "listID", current.ID)
		}
	}

	// Claim the list before anything else can fail, so no other IPList adopts
	// it in the meantime. The claim may re-read the IPList, so status is set
	// only after it.
	if err := claimID(ctx, r.Client, ipList, cloudflarev1alpha1.IPListIDLabel, current.ID, adopting); err != nil {
		return ctrl.Result{}, err
	}
	ipList.Status.CloudflareMetadata.ListID = current.ID
	ipList.Status.CloudflareMetadata.OperationID = ""

	// Drift: the description. Only the description of a list can change.
	if current.Description != ipList.Spec.Description {
		if _, err := cfAPI.UpdateList(ctx, rc, cf.ListUpdateParams{
			ID:          current.ID,
			Description: ipList.Spec.Description,
		}); err != nil {
			return handleCloudflareError(ctx, r.Client, ipList, err)
		}
		logger.Info("Updated IP list description", "listID", current.ID)
	}

	// Drift: the items.
	items, err := cfAPI.ListListItems(ctx, rc, cf.ListListItemsParams{ID: current.ID, PerPage: ipListItemsPageSize})
	if err != nil {
		return handleCloudflareError(ctx, r.Client, ipList, err)
	}
	if !ipListItemsMatch(ipList.Spec.Items, items) {
		replace, err := cfAPI.ReplaceListItemsAsync(ctx, rc, cf.ListReplaceItemsParams{
			ID:    current.ID,
			Items: ipListItemRequests(ipList.Spec.Items),
		})
		if err != nil {
			return handleCloudflareError(ctx, r.Client, ipList, err)
		}
		ipList.Status.CloudflareMetadata.OperationID = replace.Result.OperationID
		logger.Info("Replacing IP list items", "listID", current.ID, "operationID", replace.Result.OperationID)
		return ctrl.Result{RequeueAfter: ipListOperationPollInterval},
			updateNotReady(ctx, r.Client, ipList, "ReplacingItems", "Cloudflare is replacing the list's items")
	}

	ipList.Status.CloudflareMetadata.NumItems = int32(len(items))
	return readyWithDriftCheck(ctx, r.Client, ipList, "Synced", "IP list is synced with Cloudflare")
}

// listWithID returns the list with id, which must not be empty.
func listWithID(lists []cf.List, id string) (cf.List, bool) {
	if id == "" {
		return cf.List{}, false
	}
	for _, list := range lists {
		if list.ID == id {
			return list, true
		}
	}
	return cf.List{}, false
}

// listWithName returns the list named name.
func listWithName(lists []cf.List, name string) (cf.List, bool) {
	for _, list := range lists {
		if list.Name == name {
			return list, true
		}
	}
	return cf.List{}, false
}

// ipListItemRequests is what Cloudflare is sent to replace a list's items:
// the spec's items in order, so a position in Cloudflare's error messages is
// an index into spec.items. For no items it is an empty array, which empties
// the list (verified live).
func ipListItemRequests(items []cloudflarev1alpha1.IPListItem) []cf.ListItemCreateRequest {
	requests := make([]cf.ListItemCreateRequest, 0, len(items))
	for _, item := range items {
		requests = append(requests, cf.ListItemCreateRequest{IP: &item.IP, Comment: item.Comment})
	}
	return requests
}

// ipListItemsMatch reports whether a list's items in Cloudflare are the
// spec's: the same addresses and ranges with the same comments, in any order.
// Addresses are compared in canonical form, because Cloudflare stores them
// that way. A spec listing one address twice, in two forms, never matches:
// Cloudflare refuses such a list (code 10028), and the IPList reports that.
func ipListItemsMatch(want []cloudflarev1alpha1.IPListItem, got []cf.ListItem) bool {
	comments := make(map[string]string, len(want))
	for _, item := range want {
		comments[canonicalIP(item.IP)] = item.Comment
	}
	if len(comments) != len(want) || len(got) != len(want) {
		return false
	}
	for _, item := range got {
		if item.IP == nil {
			return false
		}
		comment, ok := comments[canonicalIP(*item.IP)]
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

// reconcileDelete handles the deletion lifecycle: unless the retain policy is
// set, it deletes the list from Cloudflare, then removes the finalizer.
//
// Cloudflare refuses to delete a list that a rule's expression uses (code
// 10041, verified live). The IPList then reports ListInUse and checks again
// every minute, and sooner once a kflare rule that uses it is deleted.
func (r *IPListReconciler) reconcileDelete(ctx context.Context, ipList *cloudflarev1alpha1.IPList) (ctrl.Result, error) {
	// Safety check: if the finalizer is already gone, there is nothing to do.
	if !controllerutil.ContainsFinalizer(ipList, reconciler.Finalizer) {
		return ctrl.Result{}, nil
	}

	// The list is the one in status. A list created by a reconcile that failed
	// before writing status is found through the claim.
	listID := ipList.Status.CloudflareMetadata.ListID
	if listID == "" {
		listID = ipList.Labels[cloudflarev1alpha1.IPListIDLabel]
	}
	if listID != "" && !reconciler.RetainOnDelete(ipList) {
		account, token, credErr := resolveAccountToken(ctx, r.Client, ipList.Spec.AccountRef.Name, false)
		if credErr != nil {
			return ctrl.Result{}, credErr
		}
		cfAPI, err := r.newIPListAPI(token)
		if err != nil {
			return ctrl.Result{}, err
		}
		_, err = cfAPI.DeleteList(ctx, cf.AccountIdentifier(account.Spec.AccountID), listID)
		if cfpkg.HasErrorCode(err, ipListInUseCode) {
			return notReadyRetryAfter(ctx, r.Client, ipList, &conditionError{
				Reason: "ListInUse",
				Message: fmt.Sprintf("Cloudflare does not delete IP list %q while a rule uses it: remove $%s from "+
					"the rules' expressions, or set the %s: %s annotation to keep the list", ipList.Spec.Name,
					ipList.Spec.Name, reconciler.DeletionPolicyAnnotation, reconciler.DeletionPolicyRetain),
			})
		}
		if err != nil && !cfpkg.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// Remove the finalizer to allow Kubernetes to complete the deletion.
	_, err := reconciler.RemoveFinalizer(ctx, r.Client, ipList)
	return ctrl.Result{}, err
}

// SetupWithManager registers IPListReconciler with the manager. It watches
// CloudflareAccounts, so a list waiting for its account is reconciled once
// the account is ready, and deleted rules of every kind, so a list that
// Cloudflare would not delete while a rule used it goes as soon as that rule
// is gone.
func (r *IPListReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&cloudflarev1alpha1.IPList{}).
		Watches(&cloudflarev1alpha1.CloudflareAccount{}, handler.EnqueueRequestsFromMapFunc(r.listsForAccount))
	for _, kind := range rulesetRuleKinds {
		b = b.Watches(kind.newObject(), handler.EnqueueRequestsFromMapFunc(r.deletingListsUsedBy),
			builder.WithPredicates(deletesOnly))
	}
	return b.Complete(r)
}

// listsForAccount maps a CloudflareAccount event to the IPLists that use it.
func (r *IPListReconciler) listsForAccount(ctx context.Context, obj client.Object) []reconcile.Request {
	lists := &cloudflarev1alpha1.IPListList{}
	if err := r.List(ctx, lists); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, list := range lists.Items {
		if list.Spec.AccountRef.Name == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: list.Name, Namespace: list.Namespace}})
		}
	}
	return reqs
}

// deletingListsUsedBy maps the deletion of a rule to the IPLists being
// deleted whose list the rule's expression uses, in any namespace: a list
// belongs to the account, not to a namespace.
func (r *IPListReconciler) deletingListsUsedBy(ctx context.Context, obj client.Object) []reconcile.Request {
	rule, ok := obj.(rulesetRuleObject)
	if !ok {
		return nil
	}
	names := listsInExpression(rule.GetRulesetRuleSpec().Expression)
	if len(names) == 0 {
		return nil
	}
	lists := &cloudflarev1alpha1.IPListList{}
	if err := r.List(ctx, lists); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, list := range lists.Items {
		if !list.DeletionTimestamp.IsZero() && slices.Contains(names, list.Spec.Name) {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: list.Name, Namespace: list.Namespace}})
		}
	}
	return reqs
}

// newIPListAPI builds an IPListAPI with the injected factory, falling back to
// the production client.
func (r *IPListReconciler) newIPListAPI(token string) (IPListAPI, error) {
	if r.NewIPListAPI != nil {
		return r.NewIPListAPI(token)
	}
	return defaultIPListAPI(token)
}

// defaultIPListAPI is the production factory: it delegates to cfpkg.New so
// that the returned *cfpkg.Client (which embeds *cf.API) satisfies IPListAPI.
func defaultIPListAPI(token string) (IPListAPI, error) {
	return cfpkg.New(token)
}
