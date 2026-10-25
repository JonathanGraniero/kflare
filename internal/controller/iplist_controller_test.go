/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"strings"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// The IPList specs run the reconciler in envtest against fakeIPListAPI
// (iplist_fake_test.go), the in-memory Lists API that follows the live API's
// behavior.
var _ = Describe("IPList Controller", func() {
	const (
		ns          = "default"
		accountName = "iplist-test-account"
		secretName  = "iplist-test-api-token"
	)

	ctx := context.Background()
	var fake *fakeIPListAPI
	var r *IPListReconciler

	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: ns} }

	// create creates obj and deletes it again after the spec.
	create := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(forceDelete, ctx, obj.DeepCopyObject().(client.Object), client.ObjectKeyFromObject(obj))
	}
	createAccount := func(ready bool) {
		create(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
		})
		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      ipListTestAccount,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
			},
		}
		create(acct)
		if ready {
			acct.Status.Conditions = []metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Validated", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, acct)).To(Succeed())
		}
	}
	// createList creates IPList <name> for the list kflare_<name>, holding
	// 192.0.2.1 and 198.51.100.0/24 and already carrying the finalizer;
	// change adjusts its spec first.
	createList := func(name string, change func(*cloudflarev1alpha1.IPListSpec)) {
		list := &cloudflarev1alpha1.IPList{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.IPListSpec{
				AccountRef:  corev1.LocalObjectReference{Name: accountName},
				Name:        "kflare_" + name,
				Description: "office " + name,
				Items: []cloudflarev1alpha1.IPListItem{
					{IP: "192.0.2.1", Comment: "front door"},
					{IP: "198.51.100.0/24"},
				},
			},
		}
		if change != nil {
			change(&list.Spec)
		}
		create(list)
	}
	getList := func(k types.NamespacedName) *cloudflarev1alpha1.IPList {
		list := &cloudflarev1alpha1.IPList{}
		Expect(k8sClient.Get(ctx, k, list)).To(Succeed())
		return list
	}
	// annotate sets annotations on the IPList at k.
	annotate := func(k types.NamespacedName, annotations map[string]string) {
		list := getList(k)
		list.Annotations = annotations
		Expect(k8sClient.Update(ctx, list)).To(Succeed())
	}
	readyCondition := func(k types.NamespacedName) metav1.Condition {
		cond := meta.FindStatusCondition(getList(k).Status.Conditions, cloudflarev1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil(), "no Ready condition")
		return *cond
	}
	reconcileList := func(k types.NamespacedName) (ctrl.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: k})
	}
	// syncList reconciles the IPList at k until it is synced, which takes a
	// reconcile to replace the items and one to see the replace finished.
	syncList := func(k types.NamespacedName) {
		for range 2 {
			_, err := reconcileList(k)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(readyCondition(k).Reason).To(Equal("Synced"))
	}

	BeforeEach(func() {
		fake = newFakeIPListAPI()
		r = &IPListReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			NewIPListAPI: func(string) (IPListAPI, error) {
				return fake, nil
			},
		}
	})

	Describe("Reconcile", func() {
		It("returns no error when the IPList does not exist", func() {
			_, err := reconcileList(key("a"))
			Expect(err).NotTo(HaveOccurred())
		})

		It("adds the finalizer on the first reconcile without calling Cloudflare", func() {
			fake.before = func(method string) { Fail("Cloudflare was called: " + method) }
			create(&cloudflarev1alpha1.IPList{
				ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: ns},
				Spec: cloudflarev1alpha1.IPListSpec{
					AccountRef: corev1.LocalObjectReference{Name: accountName}, Name: "kflare_a",
				},
			})
			_, err := reconcileList(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(getList(key("a")).Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("retries after a minute when the account is not ready", func() {
			fake.before = func(method string) { Fail("Cloudflare was called: " + method) }
			createAccount(false)
			createList("a", nil)
			result, err := reconcileList(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(readyCondition(key("a")).Reason).To(Equal("AccountNotReady"))
		})

		It("reports InvalidToken when no Cloudflare client can be built", func() {
			createAccount(true)
			createList("a", nil)
			r.NewIPListAPI = func(string) (IPListAPI, error) { return nil, errors.New("bad token") }
			_, err := reconcileList(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(readyCondition(key("a")).Reason).To(Equal("InvalidToken"))
		})

		Context("with a ready account", func() {
			BeforeEach(func() { createAccount(true) })

			It("creates the list, waits for Cloudflare to replace its items, then reports Synced", func() {
				fake.polls = 1
				createList("a", nil)

				By("creating the list and starting the replace")
				result, err := reconcileList(key("a"))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(ipListOperationPollInterval))
				cfList := fake.listNamed("kflare_a")
				Expect(cfList).NotTo(BeNil())
				Expect(cfList.Kind).To(Equal(cf.ListTypeIP))
				Expect(cfList.Description).To(Equal("office a"))
				list := getList(key("a"))
				Expect(list.Labels).To(HaveKeyWithValue(cloudflarev1alpha1.IPListIDLabel, cfList.ID))
				Expect(list.Status.CloudflareMetadata.ListID).To(Equal(cfList.ID))
				Expect(list.Status.CloudflareMetadata.OperationID).NotTo(BeEmpty())
				Expect(readyCondition(key("a")).Reason).To(Equal("ReplacingItems"))
				Expect(fake.storedItems()).To(BeEmpty(), "Cloudflare has not applied the replace yet")

				By("waiting while Cloudflare is still replacing the items")
				writes := fake.writes
				result, err = reconcileList(key("a"))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(ipListOperationPollInterval))
				Expect(fake.writes).To(Equal(writes))

				By("reporting the list once the replace is done")
				result, err = reconcileList(key("a"))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.IsZero()).To(BeTrue())
				Expect(fake.storedItems()).To(ConsistOf("192.0.2.1 front door", "198.51.100.0/24"))
				list = getList(key("a"))
				Expect(list.Status.CloudflareMetadata).To(Equal(cloudflarev1alpha1.IPListCloudflareMetadata{
					ListID: cfList.ID, NumItems: 2,
				}))
				Expect(readyCondition(key("a")).Status).To(Equal(metav1.ConditionTrue))
				Expect(fake.writes).To(Equal(writes), "the replace is not repeated")
			})

			It("writes nothing once the list is in sync, whatever form the spec gives addresses in", func() {
				createList("a", func(spec *cloudflarev1alpha1.IPListSpec) {
					spec.Items = []cloudflarev1alpha1.IPListItem{
						{IP: "203.0.113.7/32"}, {IP: "2001:DB8::1"}, {IP: "2001:db8:0:0::/64", Comment: "v6 range"},
					}
				})
				syncList(key("a"))
				Expect(fake.storedItems()).To(ConsistOf("203.0.113.7", "2001:db8::1", "2001:db8::/64 v6 range"))

				writes := fake.writes
				_, err := reconcileList(key("a"))
				Expect(err).NotTo(HaveOccurred())
				Expect(fake.writes).To(Equal(writes))
				Expect(readyCondition(key("a")).Reason).To(Equal("Synced"))
			})

			It("schedules a drift check after a sync, once the manager sets an interval", func() {
				createList("a", nil)
				syncList(key("a"))
				writes := fake.writes
				expectDriftCheck(func() (reconcile.Result, error) { return reconcileList(key("a")) })
				Expect(fake.writes).To(Equal(writes))
			})

			It("updates the description and replaces the items when the spec changes, keeping the list", func() {
				createList("a", nil)
				syncList(key("a"))
				listID := fake.listNamed("kflare_a").ID

				list := getList(key("a"))
				list.Spec.Description = "new office"
				list.Spec.Items = []cloudflarev1alpha1.IPListItem{
					{IP: "192.0.2.1", Comment: "side door"}, {IP: "2001:db8::/48"},
				}
				Expect(k8sClient.Update(ctx, list)).To(Succeed())
				syncList(key("a"))

				Expect(fake.listNamed("kflare_a").ID).To(Equal(listID))
				Expect(fake.listNamed("kflare_a").Description).To(Equal("new office"))
				Expect(fake.storedItems()).To(ConsistOf("192.0.2.1 side door", "2001:db8::/48"))
			})

			It("empties the list when the spec has no items", func() {
				createList("a", nil)
				syncList(key("a"))

				list := getList(key("a"))
				list.Spec.Items = nil
				Expect(k8sClient.Update(ctx, list)).To(Succeed())
				syncList(key("a"))

				Expect(fake.storedItems()).To(BeEmpty())
				sent := fake.replaced[len(fake.replaced)-1]
				Expect(sent).NotTo(BeNil(), "an empty array, not null")
				Expect(sent).To(BeEmpty())
				Expect(getList(key("a")).Status.CloudflareMetadata.NumItems).To(BeZero())
			})

			It("adopts an existing IP list with the same name, and corrects it", func() {
				id := fake.seed("kflare_a", cf.ListTypeIP, "", cf.ListItem{IP: new("10.0.0.0/8")})
				createList("a", nil)
				annotate(key("a"), map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt})
				syncList(key("a"))

				Expect(fake.lists).To(HaveLen(1), "nothing created")
				Expect(getList(key("a")).Labels).To(HaveKeyWithValue(cloudflarev1alpha1.IPListIDLabel, id))
				Expect(getList(key("a")).Annotations).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))
				Expect(fake.lists[id].Description).To(Equal("office a"))
				Expect(fake.storedItems()).To(ConsistOf("192.0.2.1 front door", "198.51.100.0/24"))
			})

			It("reports AlreadyExists for an IP list with its name without an adoption policy, and leaves it", func() {
				id := fake.seed("kflare_a", cf.ListTypeIP, "", cf.ListItem{IP: new("10.0.0.0/8")})
				createList("a", nil)
				result, err := reconcileList(key("a"))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
				Expect(readyCondition(key("a")).Reason).To(Equal("AlreadyExists"))
				Expect(readyCondition(key("a")).Message).To(ContainSubstring("kflare_a"))
				Expect(fake.writes).To(BeZero())
				Expect(fake.storedItems()).To(ConsistOf("10.0.0.0/8"))
				Expect(getList(key("a")).Labels).NotTo(HaveKey(cloudflarev1alpha1.IPListIDLabel))
				Expect(fake.lists).To(HaveKey(id))
			})

			It("reports NotFound with the adopt policy when the account has no list with its name", func() {
				createList("a", nil)
				annotate(key("a"), map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt})
				_, err := reconcileList(key("a"))
				Expect(err).NotTo(HaveOccurred())
				Expect(readyCondition(key("a")).Reason).To(Equal("NotFound"))
				Expect(fake.lists).To(BeEmpty())
			})

			It("finds the list it claimed when its status was not recorded", func() {
				id := fake.seed("kflare_a", cf.ListTypeIP, "office a")
				createList("a", nil)
				list := getList(key("a"))
				list.Labels = map[string]string{cloudflarev1alpha1.IPListIDLabel: id}
				Expect(k8sClient.Update(ctx, list)).To(Succeed())
				syncList(key("a"))
				Expect(getList(key("a")).Status.CloudflareMetadata.ListID).To(Equal(id))
				Expect(fake.lists).To(HaveLen(1), "nothing else is created")
				Expect(getList(key("a")).Annotations).NotTo(HaveKey(reconciler.AdoptedAnnotation))
			})

			It("marks the list it is about to create, and finds it on the next reconcile as its own", func() {
				createList("a", nil)
				fake.errs = map[string]error{"CreateList": errors.New("lost response")}
				_, err := reconcileList(key("a"))
				Expect(err).To(HaveOccurred())
				Expect(getList(key("a")).Annotations).To(HaveKeyWithValue(reconciler.CreatingAnnotation, "kflare_a"))

				By("finding the list the lost create made, without an adoption policy")
				fake.errs = nil
				id := fake.seed("kflare_a", cf.ListTypeIP, "office a")
				syncList(key("a"))
				Expect(getList(key("a")).Status.CloudflareMetadata.ListID).To(Equal(id))
				Expect(fake.lists).To(HaveLen(1), "nothing else is created")
				Expect(getList(key("a")).Annotations).NotTo(HaveKey(reconciler.AdoptedAnnotation))
				Expect(getList(key("a")).Annotations).NotTo(HaveKey(reconciler.CreatingAnnotation))
			})

			It("does not adopt a list that another IPList manages", func() {
				id := fake.seed("kflare_a", cf.ListTypeIP, "")
				create(&cloudflarev1alpha1.IPList{
					ObjectMeta: metav1.ObjectMeta{
						Name: "owner", Namespace: "kube-public",
						Labels: map[string]string{cloudflarev1alpha1.IPListIDLabel: id},
					},
					Spec: cloudflarev1alpha1.IPListSpec{
						AccountRef: corev1.LocalObjectReference{Name: accountName}, Name: "kflare_a",
					},
				})
				createList("a", nil)
				result, err := reconcileList(key("a"))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
				Expect(readyCondition(key("a")).Reason).To(Equal("NameConflict"))
				Expect(readyCondition(key("a")).Message).To(ContainSubstring("kube-public/owner"))
				Expect(fake.writes).To(BeZero())
			})

			It("does not adopt a list of another kind that holds the name", func() {
				fake.seed("kflare_a", cf.ListTypeHostname, "")
				createList("a", nil)
				result, err := reconcileList(key("a"))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
				Expect(readyCondition(key("a")).Reason).To(Equal("NameConflict"))
				Expect(readyCondition(key("a")).Message).To(ContainSubstring("hostname list"))
				Expect(fake.writes).To(BeZero())
			})

			It("recreates a list deleted outside kflare", func() {
				createList("a", nil)
				syncList(key("a"))
				oldID := fake.listNamed("kflare_a").ID
				delete(fake.lists, oldID)

				syncList(key("a"))
				newID := fake.listNamed("kflare_a").ID
				Expect(newID).NotTo(Equal(oldID))
				Expect(getList(key("a")).Labels).To(HaveKeyWithValue(cloudflarev1alpha1.IPListIDLabel, newID))
				Expect(fake.storedItems()).To(HaveLen(2))
			})

			It("reports TerminalError without requeueing when Cloudflare refuses an item", func() {
				createList("a", func(spec *cloudflarev1alpha1.IPListSpec) {
					spec.Items = append(spec.Items, cloudflarev1alpha1.IPListItem{IP: "203.0.113.5/24"})
				})
				result, err := reconcileList(key("a"))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.IsZero()).To(BeTrue())
				Expect(readyCondition(key("a")).Reason).To(Equal("TerminalError"))
				Expect(getList(key("a")).Status.CloudflareMetadata.OperationID).To(BeEmpty())
			})

			It("reports TerminalError when the account cannot have another list", func() {
				fake.maxLists = 0
				createList("a", nil)
				result, err := reconcileList(key("a"))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.IsZero()).To(BeTrue())
				Expect(readyCondition(key("a")).Reason).To(Equal("TerminalError"))
			})

			It("retries a failed replace with back-off, starting it again", func() {
				fake.failOperations = "internal error"
				createList("a", nil)
				_, err := reconcileList(key("a"))
				Expect(err).NotTo(HaveOccurred())

				_, err = reconcileList(key("a"))
				Expect(err).To(MatchError(ContainSubstring("internal error")))
				Expect(readyCondition(key("a")).Reason).To(Equal("APIError"))
				Expect(getList(key("a")).Status.CloudflareMetadata.OperationID).To(BeEmpty())

				fake.failOperations = ""
				syncList(key("a"))
				Expect(fake.replaced).To(HaveLen(2))
			})

			It("compares the items when Cloudflare no longer knows the operation", func() {
				createList("a", nil)
				syncList(key("a"))
				list := getList(key("a"))
				list.Status.CloudflareMetadata.OperationID = "forgotten"
				Expect(k8sClient.Status().Update(ctx, list)).To(Succeed())

				_, err := reconcileList(key("a"))
				Expect(err).NotTo(HaveOccurred())
				Expect(readyCondition(key("a")).Reason).To(Equal("Synced"))
				Expect(getList(key("a")).Status.CloudflareMetadata.OperationID).To(BeEmpty())
			})

			// startReplace lets the controller create list a and start replacing
			// its items; changeDescription syncs it and then changes its
			// description.
			startReplace := func() {
				_, err := reconcileList(key("a"))
				Expect(err).NotTo(HaveOccurred())
			}
			changeDescription := func() {
				syncList(key("a"))
				list := getList(key("a"))
				list.Spec.Description = "changed"
				Expect(k8sClient.Update(ctx, list)).To(Succeed())
			}

			DescribeTable("returns retryable errors for back-off",
				func(method string, prepare func()) {
					createList("a", nil)
					if prepare != nil {
						prepare()
					}
					fake.errs[method] = errors.New("connection reset")
					_, err := reconcileList(key("a"))
					Expect(err).To(MatchError("connection reset"))
					Expect(readyCondition(key("a")).Reason).To(Equal("APIError"))
				},
				Entry("listing the lists", "ListLists", nil),
				Entry("creating the list", "CreateList", nil),
				Entry("listing the items", "ListListItems", nil),
				Entry("replacing the items", "ReplaceListItemsAsync", nil),
				Entry("checking on the replace", "GetListBulkOperation", startReplace),
				Entry("updating the description", "UpdateList", changeDescription),
			)
		})
	})

	Describe("deletion", func() {
		BeforeEach(func() { createAccount(true) })

		// syncedList creates list a, lets the controller sync it, and deletes it.
		syncedList := func(annotations map[string]string) string {
			createList("a", nil)
			syncList(key("a"))
			list := getList(key("a"))
			if annotations != nil {
				list.Annotations = annotations
				Expect(k8sClient.Update(ctx, list)).To(Succeed())
			}
			Expect(k8sClient.Delete(ctx, list)).To(Succeed())
			return list.Status.CloudflareMetadata.ListID
		}
		expectGone := func() {
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key("a"), &cloudflarev1alpha1.IPList{}))).To(BeTrue())
		}

		It("deletes the list from Cloudflare and removes the finalizer", func() {
			syncedList(nil)
			_, err := reconcileList(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.lists).To(BeEmpty())
			expectGone()
		})

		It("keeps a list it adopted", func() {
			syncedList(map[string]string{reconciler.AdoptedAnnotation: "true"})
			_, err := reconcileList(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.lists).To(HaveLen(1))
			expectGone()
		})

		It("deletes a list it adopted with the delete policy", func() {
			syncedList(map[string]string{
				reconciler.AdoptedAnnotation:        "true",
				reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyDelete,
			})
			_, err := reconcileList(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.lists).To(BeEmpty())
			expectGone()
		})

		It("keeps the list in Cloudflare with the retain policy", func() {
			syncedList(map[string]string{reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain})
			_, err := reconcileList(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.lists).To(HaveLen(1))
			expectGone()
		})

		It("reports ListInUse while a rule uses the list, checking again every minute", func() {
			id := syncedList(nil)
			fake.inUse[id] = true
			result, err := reconcileList(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(readyCondition(key("a")).Reason).To(Equal("ListInUse"))
			Expect(readyCondition(key("a")).Message).To(ContainSubstring("$kflare_a"))
			Expect(getList(key("a")).Finalizers).To(ContainElement(reconciler.Finalizer))

			delete(fake.inUse, id)
			_, err = reconcileList(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.lists).To(BeEmpty())
			expectGone()
		})

		It("finds the list through its claim when its status was never written", func() {
			syncedList(nil)
			list := getList(key("a"))
			list.Status.CloudflareMetadata = cloudflarev1alpha1.IPListCloudflareMetadata{}
			Expect(k8sClient.Status().Update(ctx, list)).To(Succeed())
			_, err := reconcileList(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.lists).To(BeEmpty())
			expectGone()
		})

		It("skips Cloudflare for a list that never reached it", func() {
			createList("a", nil)
			fake.before = func(method string) { Fail("Cloudflare was called: " + method) }
			Expect(k8sClient.Delete(ctx, getList(key("a")))).To(Succeed())
			_, err := reconcileList(key("a"))
			Expect(err).NotTo(HaveOccurred())
			expectGone()
		})

		It("keeps the finalizer when Cloudflare fails, so deletion is retried", func() {
			syncedList(nil)
			fake.errs["DeleteList"] = errors.New("unavailable")
			_, err := reconcileList(key("a"))
			Expect(err).To(MatchError("unavailable"))
			Expect(getList(key("a")).Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("keeps the finalizer when the credentials cannot be resolved or used", func() {
			syncedList(nil)
			r.NewIPListAPI = func(string) (IPListAPI, error) { return nil, errors.New("bad token") }
			_, err := reconcileList(key("a"))
			Expect(err).To(MatchError("bad token"))

			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
			_, err = reconcileList(key("a"))
			Expect(err).To(HaveOccurred())
			Expect(getList(key("a")).Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("does nothing for a list without the finalizer", func() {
			result, err := r.reconcileDelete(ctx, &cloudflarev1alpha1.IPList{})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
		})
	})

	Describe("Validation", func() {
		// validate creates an IPList changed by change, and deletes it again.
		validate := func(change func(*cloudflarev1alpha1.IPListSpec)) error {
			list := &cloudflarev1alpha1.IPList{
				ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: ns},
				Spec: cloudflarev1alpha1.IPListSpec{
					AccountRef: corev1.LocalObjectReference{Name: accountName}, Name: "kflare_a",
				},
			}
			change(&list.Spec)
			err := k8sClient.Create(ctx, list)
			forceDelete(ctx, &cloudflarev1alpha1.IPList{}, key("a"))
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

		DescribeTable("rejects changing an identifying field",
			func(change func(*cloudflarev1alpha1.IPList), message string) {
				createList("a", nil)
				list := getList(key("a"))
				change(list)
				Expect(k8sClient.Update(ctx, list)).To(MatchError(ContainSubstring(message)))
			},
			Entry("the name", func(list *cloudflarev1alpha1.IPList) { list.Spec.Name = "kflare_b" },
				"name is immutable"),
			Entry("the account", func(list *cloudflarev1alpha1.IPList) { list.Spec.AccountRef.Name = "other" },
				"accountRef is immutable"),
		)

		DescribeTable("rejects an invalid spec",
			func(change func(*cloudflarev1alpha1.IPListSpec), message string) {
				Expect(validate(change)).To(MatchError(ContainSubstring(message)))
			},
			Entry("an uppercase name", func(spec *cloudflarev1alpha1.IPListSpec) { spec.Name = "Office" },
				"spec.name"),
			Entry("a name with a hyphen", func(spec *cloudflarev1alpha1.IPListSpec) { spec.Name = "office-ips" },
				"spec.name"),
			Entry("a name over 50 characters", func(spec *cloudflarev1alpha1.IPListSpec) {
				spec.Name = strings.Repeat("a", 51)
			}, "spec.name"),
			Entry("a description over 500 characters", func(spec *cloudflarev1alpha1.IPListSpec) {
				spec.Description = strings.Repeat("d", 501)
			}, "spec.description"),
			Entry("an item that is not an address", func(spec *cloudflarev1alpha1.IPListSpec) {
				spec.Items = []cloudflarev1alpha1.IPListItem{{IP: "office"}}
			}, "spec.items[0].ip"),
			Entry("an address with a zone", func(spec *cloudflarev1alpha1.IPListSpec) {
				spec.Items = []cloudflarev1alpha1.IPListItem{{IP: "fe80::1%eth0"}}
			}, "spec.items[0].ip"),
			Entry("a comment over 500 characters", func(spec *cloudflarev1alpha1.IPListSpec) {
				spec.Items = []cloudflarev1alpha1.IPListItem{{IP: "192.0.2.1", Comment: strings.Repeat("c", 501)}}
			}, "spec.items[0].comment"),
			Entry("an address listed twice", func(spec *cloudflarev1alpha1.IPListSpec) {
				spec.Items = []cloudflarev1alpha1.IPListItem{{IP: "192.0.2.1"}, {IP: "192.0.2.1"}}
			}, "Duplicate value"),
		)
	})
})
