/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/rules"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// fakeIPListItems is an account's list items and bulk operations. As in
// Cloudflare, an operation replaces the items only once it has finished: it is
// pending for polls reads.
type fakeIPListItems struct {
	items      map[string][]rules.ListItemListResponse
	operations map[string]*fakeIPListOperation
	nextID     int
	polls      int
	// replaces counts the replacements started, and failNext makes the next
	// operation fail, as Cloudflare reports one that went wrong.
	replaces, reads                    int
	failNext                           string
	itemsErr, replaceErr, operationErr error
	forgetOperations                   bool
}

type fakeIPListOperation struct {
	listID string
	items  []rules.ListItemListResponse
	polls  int
	failed string
}

// fakeIPListHooksAPI is an IPList's hooks' calls, on what its store's fake
// keeps across reconciles.
type fakeIPListHooksAPI struct {
	*fakeIPListItems
}

func newFakeIPListHooksAPI(store *fakeCRUDStore[rules.ListGetResponse]) fakeIPListHooksAPI {
	if store.extra == nil {
		store.extra = &fakeIPListItems{items: map[string][]rules.ListItemListResponse{},
			operations: map[string]*fakeIPListOperation{}}
	}
	return fakeIPListHooksAPI{store.extra.(*fakeIPListItems)}
}

func (f fakeIPListHooksAPI) ListItems(_ context.Context, _, listID string) ([]rules.ListItemListResponse, error) {
	f.reads++
	if f.itemsErr != nil {
		return nil, f.itemsErr
	}
	return f.items[listID], nil
}

func (f fakeIPListHooksAPI) ReplaceItems(_ context.Context, _, listID string,
	items []rules.ListItemUpdateParamsBodyUnion) (string, error) {
	f.replaces++
	if f.replaceErr != nil {
		return "", f.replaceErr
	}
	if items == nil {
		return "", errors.New("the replacement of a list's items is an array: null is refused")
	}
	stored := make([]rules.ListItemListResponse, 0, len(items))
	for _, item := range items {
		body := item.(rules.ListItemUpdateParamsBody)
		stored = append(stored, rules.ListItemListResponse{IP: canonicalIP(body.IP.Value), Comment: body.Comment.Value})
	}
	f.nextID++
	id := fmt.Sprintf("operation-%d", f.nextID)
	f.operations[id] = &fakeIPListOperation{listID: listID, items: stored, polls: f.polls, failed: f.failNext}
	f.failNext = ""
	return id, nil
}

func (f fakeIPListHooksAPI) Operation(_ context.Context, _, operationID string) (*rules.ListBulkOperationGetResponse, error) {
	if f.operationErr != nil {
		return nil, f.operationErr
	}
	operation, ok := f.operations[operationID]
	if !ok || f.forgetOperations {
		return nil, fakeCRUDNotFound(http.MethodGet, "account", operationID)
	}
	res := &rules.ListBulkOperationGetResponse{ID: operationID}
	switch {
	case operation.polls > 0:
		operation.polls--
		res.Status = rules.ListBulkOperationGetResponseStatusRunning
	case operation.failed != "":
		res.Status, res.Error = rules.ListBulkOperationGetResponseStatusFailed, operation.failed
	default:
		f.items[operation.listID] = operation.items
		res.Status = rules.ListBulkOperationGetResponseStatusCompleted
	}
	return res, nil
}

// fakeListInUse refuses to delete a list while a rule uses it, as Cloudflare
// does.
type fakeListInUse struct {
	fakeIPListAPI
	inUse *atomic.Bool
}

func (f fakeListInUse) Delete(ctx context.Context, id string, params rules.ListDeleteParams,
	opts ...option.RequestOption) (*rules.ListDeleteResponse, error) {
	if f.inUse.Load() {
		return nil, v7APIError(http.MethodDelete, "lists", http.StatusBadRequest, 10041, "the list is in use")
	}
	return f.fakeIPListAPI.Delete(ctx, id, params, opts...)
}

var _ = Describe("IPList hooks", func() {
	const (
		ns          = "default"
		accountName = "iplist-hooks-account"
		secretName  = "iplist-hooks-token"
		accountID   = "iplist-hooks-account-id"
	)
	ctx := context.Background()
	key := types.NamespacedName{Name: "office", Namespace: ns}
	var store *fakeCRUDStore[rules.ListGetResponse]
	var items *fakeIPListItems
	var r *IPListReconciler

	// create creates obj and deletes it again after the spec.
	create := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(forceDelete, ctx, obj.DeepCopyObject().(client.Object), client.ObjectKeyFromObject(obj))
	}
	createList := func(change func(*cloudflarev1alpha1.IPListSpec)) {
		list := &cloudflarev1alpha1.IPList{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.IPListSpec{
				AccountRef:  corev1.LocalObjectReference{Name: accountName},
				Name:        "office_ips",
				Description: "office",
				IPListSpecExtras: cloudflarev1alpha1.IPListSpecExtras{Items: []cloudflarev1alpha1.IPListItem{
					{IP: "192.0.2.1", Comment: "front door"},
					{IP: "198.51.100.0/24"},
				}},
			},
		}
		if change != nil {
			change(&list.Spec)
		}
		create(list)
	}
	getList := func() *cloudflarev1alpha1.IPList {
		list := &cloudflarev1alpha1.IPList{}
		Expect(k8sClient.Get(ctx, key, list)).To(Succeed())
		return list
	}
	condition := func() metav1.Condition {
		cond := meta.FindStatusCondition(getList().Status.Conditions, cloudflarev1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil(), "no Ready condition")
		return *cond
	}
	reconcileList := func() (ctrl.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	}
	// syncList reconciles until the list is synced: a reconcile starts the
	// replacement, and the next one sees it finished.
	syncList := func() {
		for range 2 {
			_, err := reconcileList()
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(condition().Reason).To(Equal("Synced"))
	}
	stored := func() []string {
		listed := items.items[getList().Status.CloudflareMetadata.ListID]
		out := make([]string, 0, len(listed))
		for _, item := range listed {
			out = append(out, strings.TrimSpace(item.IP+" "+item.Comment))
		}
		return out
	}

	BeforeEach(func() {
		store = newFakeCRUDStore[rules.ListGetResponse](0, "")
		hooks := newFakeIPListHooksAPI(store)
		items = hooks.fakeIPListItems
		r = &IPListReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			NewIPListAPI: func(string) (IPListAPI, error) { return fakeIPListAPI{store, hooks}, nil }}
		create(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
		})
		account := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      accountID,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
			},
		}
		create(account)
		account.Status.Conditions = []metav1.Condition{{
			Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Validated", LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, account)).To(Succeed())
	})

	It("creates the list, waits for Cloudflare to replace its items, then reports Synced", func() {
		items.polls = 1
		createList(nil)

		By("creating the list and starting the replacement")
		result, err := reconcileList()
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(ipListOperationPollInterval))
		list := getList()
		listID := list.Status.CloudflareMetadata.ListID
		Expect(store.objects).To(HaveKey(listID))
		Expect(string(store.objects[listID].Kind)).To(Equal("ip"))
		Expect(list.Status.CloudflareMetadata.OperationID).NotTo(BeEmpty())
		Expect(condition().Reason).To(Equal("ReplacingItems"))
		Expect(stored()).To(BeEmpty(), "Cloudflare has not applied the replacement yet")

		By("waiting while Cloudflare is still replacing the items")
		result, err = reconcileList()
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(ipListOperationPollInterval))
		Expect(items.replaces).To(Equal(1))

		By("reporting the list once the replacement is done")
		result, err = reconcileList()
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())
		Expect(stored()).To(ConsistOf("192.0.2.1 front door", "198.51.100.0/24"))
		metadata := getList().Status.CloudflareMetadata
		Expect(metadata.NumItems).To(Equal(int32(2)))
		Expect(metadata.OperationID).To(BeEmpty())
		Expect(condition().Status).To(Equal(metav1.ConditionTrue))
		Expect(items.replaces).To(Equal(1), "the replacement is not repeated")
	})

	It("writes nothing once the list is in sync, whatever form the spec gives addresses in", func() {
		createList(func(spec *cloudflarev1alpha1.IPListSpec) {
			spec.Items = []cloudflarev1alpha1.IPListItem{{IP: "203.0.113.7/32"}, {IP: "2001:DB8:0::/64"}}
		})
		syncList()
		Expect(stored()).To(ConsistOf("203.0.113.7", "2001:db8::/64"))
		_, err := reconcileList()
		Expect(err).NotTo(HaveOccurred())
		Expect(items.replaces).To(Equal(1))
		Expect(condition().Reason).To(Equal("Synced"))
	})

	It("replaces the items when the spec changes, keeping the list", func() {
		createList(nil)
		syncList()
		list := getList()
		listID := list.Status.CloudflareMetadata.ListID
		list.Spec.Items = []cloudflarev1alpha1.IPListItem{{IP: "192.0.2.2", Comment: "back door"}}
		Expect(k8sClient.Update(ctx, list)).To(Succeed())
		syncList()
		Expect(getList().Status.CloudflareMetadata.ListID).To(Equal(listID))
		Expect(stored()).To(ConsistOf("192.0.2.2 back door"))
		Expect(getList().Status.CloudflareMetadata.NumItems).To(Equal(int32(1)))
	})

	It("corrects a comment changed outside kflare", func() {
		createList(nil)
		syncList()
		id := getList().Status.CloudflareMetadata.ListID
		items.items[id][0].Comment = "changed by hand"
		syncList()
		Expect(stored()).To(ConsistOf("192.0.2.1 front door", "198.51.100.0/24"))
	})

	It("empties the list when the spec has no items", func() {
		createList(nil)
		syncList()
		list := getList()
		list.Spec.Items = nil
		Expect(k8sClient.Update(ctx, list)).To(Succeed())
		syncList()
		Expect(stored()).To(BeEmpty())
		Expect(getList().Status.CloudflareMetadata.NumItems).To(BeZero())
	})

	It("corrects the items of a list it adopts", func() {
		store.objects["existing"] = rules.ListGetResponse{ID: "existing", Name: "office_ips", Kind: "ip",
			Description: "office"}
		items.items["existing"] = []rules.ListItemListResponse{{IP: "10.0.0.1"}}
		createList(nil)
		list := getList()
		list.Annotations = map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt}
		Expect(k8sClient.Update(ctx, list)).To(Succeed())
		syncList()
		Expect(getList().Status.CloudflareMetadata.ListID).To(Equal("existing"))
		Expect(stored()).To(ConsistOf("192.0.2.1 front door", "198.51.100.0/24"))
	})

	It("does not adopt a list of another kind that holds the name", func() {
		store.objects["redirects"] = rules.ListGetResponse{ID: "redirects", Name: "office_ips", Kind: "redirect"}
		createList(nil)
		list := getList()
		list.Annotations = map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt}
		Expect(k8sClient.Update(ctx, list)).To(Succeed())
		result, err := reconcileList()
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
		cond := condition()
		Expect(cond.Reason).To(Equal(reasonNameConflict))
		Expect(cond.Message).To(ContainSubstring("redirect list"))
		Expect(getList().Status.CloudflareMetadata.ListID).To(BeEmpty())
	})

	It("reports TerminalError without requeueing when Cloudflare refuses an item", func() {
		createList(nil)
		items.replaceErr = v7APIError(http.MethodPut, "items", http.StatusBadRequest, 10028, "duplicate item")
		result, err := reconcileList()
		Expect(err).NotTo(HaveOccurred())
		Expect(result.IsZero()).To(BeTrue())
		Expect(condition().Reason).To(Equal("TerminalError"))
		Expect(getList().Status.CloudflareMetadata.ListID).NotTo(BeEmpty(), "status keeps the list")
	})

	It("retries a failed replacement with back-off, starting it again", func() {
		items.failNext = "the items conflict"
		createList(nil)
		_, err := reconcileList()
		Expect(err).NotTo(HaveOccurred())

		_, err = reconcileList()
		Expect(err).To(MatchError(ContainSubstring("the items conflict")))
		Expect(getList().Status.CloudflareMetadata.OperationID).To(BeEmpty())

		syncList()
		Expect(items.replaces).To(Equal(2))
		Expect(stored()).To(HaveLen(2))
	})

	It("compares the items when Cloudflare no longer knows the operation", func() {
		createList(nil)
		_, err := reconcileList()
		Expect(err).NotTo(HaveOccurred())
		items.forgetOperations = true
		_, err = reconcileList()
		Expect(err).NotTo(HaveOccurred())
		Expect(items.replaces).To(Equal(2), "the items never arrived, so they are replaced again")
	})

	It("fails with the error of a read of the items or the operation", func() {
		createList(nil)
		items.itemsErr = errors.New("unavailable")
		_, err := reconcileList()
		Expect(err).To(MatchError("unavailable"))
		items.itemsErr = nil
		_, err = reconcileList()
		Expect(err).NotTo(HaveOccurred())
		items.operationErr = errors.New("unavailable")
		_, err = reconcileList()
		Expect(err).To(MatchError("unavailable"))
		Expect(getList().Status.CloudflareMetadata.OperationID).NotTo(BeEmpty(), "the operation is checked again")
	})

	It("deletes a list as soon as the rule that used it is gone, in a running manager", func() {
		createList(nil)
		syncList()
		rule := &cloudflarev1alpha1.WAFCustomRule{
			ObjectMeta: metav1.ObjectMeta{Name: "uses-office", Namespace: ns},
			Spec: cloudflarev1alpha1.WAFCustomRuleSpec{
				RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
					ZoneRef: corev1.LocalObjectReference{Name: "zone"}, Expression: `ip.src in $office_ips`,
				},
				Action: "block",
			},
		}
		create(rule)
		Expect(k8sClient.Delete(ctx, getList())).To(Succeed())

		inUse := &atomic.Bool{}
		inUse.Store(true)
		api := fakeListInUse{fakeIPListAPI{store, fakeIPListHooksAPI{items}}, inUse}
		startManager(ctx, func(mgr manager.Manager) error {
			return (&IPListReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
				NewIPListAPI: func(string) (IPListAPI, error) { return api, nil }}).SetupWithManager(mgr)
		})
		Eventually(func() string { return condition().Reason }, 10*time.Second).Should(Equal("ListInUse"))

		inUse.Store(false)
		Expect(k8sClient.Delete(ctx, rule)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &cloudflarev1alpha1.IPList{}))
		}, 10*time.Second).Should(BeTrue(), "well before the minute a retry takes")
	})

	It("reads items through the production client's services", func() {
		service := &ipListService{ListService: rules.NewListService()}
		_, err := service.ListItems(ctx, "", "list")
		Expect(err).To(HaveOccurred(), "a call without the account's ID is refused")
		_, err = service.ReplaceItems(ctx, "", "list", nil)
		Expect(err).To(HaveOccurred())
		_, err = service.Operation(ctx, "", "operation")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("ipListItemsMatch", func() {
	item := func(ip, comment string) rules.ListItemListResponse {
		return rules.ListItemListResponse{IP: ip, Comment: comment}
	}
	DescribeTable("compares in canonical form, in any order",
		func(want []cloudflarev1alpha1.IPListItem, got []rules.ListItemListResponse, match bool) {
			Expect(ipListItemsMatch(want, got)).To(Equal(match))
		},
		Entry("nothing", nil, nil, true),
		Entry("another order", []cloudflarev1alpha1.IPListItem{{IP: "192.0.2.1"}, {IP: "192.0.2.2"}},
			[]rules.ListItemListResponse{item("192.0.2.2", ""), item("192.0.2.1", "")}, true),
		Entry("a range of one address", []cloudflarev1alpha1.IPListItem{{IP: "192.0.2.1/32"}},
			[]rules.ListItemListResponse{item("192.0.2.1", "")}, true),
		Entry("another comment", []cloudflarev1alpha1.IPListItem{{IP: "192.0.2.1", Comment: "a"}},
			[]rules.ListItemListResponse{item("192.0.2.1", "b")}, false),
		Entry("one address in two forms, which Cloudflare refuses",
			[]cloudflarev1alpha1.IPListItem{{IP: "192.0.2.1"}, {IP: "192.0.2.1/32"}},
			[]rules.ListItemListResponse{item("192.0.2.1", "")}, false),
		Entry("Cloudflare's copy in another form", []cloudflarev1alpha1.IPListItem{{IP: "2001:db8::1"}},
			[]rules.ListItemListResponse{item("2001:DB8:0::1", "")}, true),
	)
})

var _ = Describe("IPLists and the rules that use them", func() {
	ctx := context.Background()

	create := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(forceDelete, ctx, obj.DeepCopyObject().(client.Object), client.ObjectKeyFromObject(obj))
	}
	createList := func(namespace, name, listName string, synced bool) *cloudflarev1alpha1.IPList {
		list := &cloudflarev1alpha1.IPList{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.IPListSpec{
				AccountRef: corev1.LocalObjectReference{Name: "uses-account"}, Name: listName,
			},
		}
		create(list)
		if synced {
			list.Status.CloudflareMetadata.ListID = "id-" + listName
			Expect(k8sClient.Status().Update(ctx, list)).To(Succeed())
		}
		return list
	}
	wafRule := func(namespace, name, expression string) *cloudflarev1alpha1.WAFCustomRule {
		return &cloudflarev1alpha1.WAFCustomRule{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: cloudflarev1alpha1.WAFCustomRuleSpec{
				RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
					ZoneRef: corev1.LocalObjectReference{Name: "uses-zone"}, Expression: expression,
				},
				Action: "block",
			},
		}
	}
	request := func(namespace, name string) reconcile.Request {
		return reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}
	}

	It("reconciles a rule that uses a list once the list exists in Cloudflare, in any namespace", func() {
		create(wafRule("default", "uses-a", `ip.src in $office_ips and http.request.uri.path eq "/admin"`))
		create(wafRule("kube-public", "uses-b", `not ip.src in $office_ips`))
		create(wafRule("default", "uses-other", `ip.src in $other_ips`))
		rules := (&WAFCustomRuleReconciler{Client: k8sClient}).rules()

		list := createList("default", "office", "office_ips", false)
		Expect(rules.rulesUsingList(ctx, list)).To(BeEmpty(), "the list is not in Cloudflare yet")

		list = createList("default", "office-synced", "office_ips", true)
		Expect(rules.rulesUsingList(ctx, list)).To(ConsistOf(
			request("default", "uses-a"), request("kube-public", "uses-b"),
		))
		Expect(rules.rulesUsingList(ctx, &corev1.Secret{})).To(BeEmpty())
	})

	It("does not reconcile a rule when the rules cannot be listed", func() {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		list := &cloudflarev1alpha1.IPList{Spec: cloudflarev1alpha1.IPListSpec{Name: "office_ips"}}
		list.Status.CloudflareMetadata.ListID = "id"
		Expect((&WAFCustomRuleReconciler{Client: k8sClient}).rules().rulesUsingList(cancelled, list)).To(BeNil())
	})

	It("reconciles a list being deleted when a rule that uses it is deleted", func() {
		mapRule := deletingListsUsedBy(k8sClient)
		deleting := createList("default", "deleting", "office_ips", true)
		Expect(k8sClient.Delete(ctx, deleting)).To(Succeed())
		deletingElsewhere := createList("kube-public", "deleting", "office_ips", true)
		Expect(k8sClient.Delete(ctx, deletingElsewhere)).To(Succeed())
		createList("default", "kept", "office_ips", true)
		unused := createList("default", "unused", "other_ips", true)
		Expect(k8sClient.Delete(ctx, unused)).To(Succeed())

		Expect(mapRule(ctx, wafRule("default", "rule", `ip.src in $office_ips`))).To(ConsistOf(
			request("default", "deleting"), request("kube-public", "deleting"),
		))
		Expect(mapRule(ctx, &cloudflarev1alpha1.RateLimitRule{
			Spec: cloudflarev1alpha1.RateLimitRuleSpec{
				RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{Expression: `ip.src in $office_ips`},
			},
		})).To(HaveLen(2), "every rule kind")
		Expect(mapRule(ctx, wafRule("default", "rule", `http.host eq "example.com"`))).To(BeEmpty())
		Expect(mapRule(ctx, &corev1.Secret{})).To(BeEmpty())

		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		Expect(mapRule(cancelled, wafRule("default", "rule", `ip.src in $office_ips`))).To(BeNil())
	})
})

var _ = Describe("IPList validation", func() {
	ctx := context.Background()
	const ns = "default"

	validate := func(change func(*cloudflarev1alpha1.IPListSpec)) error {
		list := &cloudflarev1alpha1.IPList{
			ObjectMeta: metav1.ObjectMeta{Name: "validated", Namespace: ns},
			Spec: cloudflarev1alpha1.IPListSpec{
				AccountRef: corev1.LocalObjectReference{Name: "account"}, Name: "kflare_a",
			},
		}
		change(&list.Spec)
		err := k8sClient.Create(ctx, list)
		forceDelete(ctx, &cloudflarev1alpha1.IPList{}, types.NamespacedName{Name: "validated", Namespace: ns})
		return err
	}

	It("accepts addresses and ranges of both families, and names with digits and underscores", func() {
		Expect(validate(func(spec *cloudflarev1alpha1.IPListSpec) {
			spec.Name = "1_" + strings.Repeat("a", 48)
			spec.Items = []cloudflarev1alpha1.IPListItem{
				{IP: "192.0.2.1"}, {IP: "198.51.100.0/24"}, {IP: "2001:DB8::1"},
				{IP: "2001:db8::/32", Comment: strings.Repeat("c", 500)},
			}
		})).To(Succeed())
	})

	DescribeTable("rejects an invalid item",
		func(items []cloudflarev1alpha1.IPListItem, message string) {
			Expect(validate(func(spec *cloudflarev1alpha1.IPListSpec) { spec.Items = items })).
				To(MatchError(ContainSubstring(message)))
		},
		Entry("not an address", []cloudflarev1alpha1.IPListItem{{IP: "office"}}, "spec.items[0].ip"),
		Entry("an address with a zone", []cloudflarev1alpha1.IPListItem{{IP: "fe80::1%eth0"}}, "spec.items[0].ip"),
		Entry("a comment over 500 characters",
			[]cloudflarev1alpha1.IPListItem{{IP: "192.0.2.1", Comment: strings.Repeat("c", 501)}},
			"spec.items[0].comment"),
		Entry("an address listed twice",
			[]cloudflarev1alpha1.IPListItem{{IP: "192.0.2.1"}, {IP: "192.0.2.1"}}, "Duplicate value"),
	)
})
