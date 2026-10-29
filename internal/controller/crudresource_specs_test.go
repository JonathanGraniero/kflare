/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/cloudflare/cloudflare-go/v7/packages/pagination"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// The specs every generated kind runs. kflare-gen writes a table per kind
// (zz_generated.<kind>_test.go): a fake of its cloudflare-go v7 service on
// fakeCRUDStore, a valid resource from the kind's sample, and edits of it.
// describeCRUDKind runs the shared CRUD reconciler's lifecycle against them:
// the finalizer, credentials, creation and its claim, an idle reconcile that
// writes nothing, adoption and its conflict, recreation, updates in place,
// errors, deletion, its blockers and the retain policy. It also checks the
// CRD: every immutable field, and values its validation refuses.

// fakeCRUDStore is an in-memory Cloudflare account's objects of one kind.
// Each generated fake converts its service's params into calls on it. Like
// Cloudflare, it returns 404 for an unknown ID, refuses a page larger than
// pageSize and a second object under a key that is its ID (an R2 bucket's
// name), and keeps objects apart by their address (an R2 bucket's
// jurisdiction); like cloudflare-go, it refuses a call without an account ID.
type fakeCRUDStore[R any] struct {
	// objects are the objects at the address the sample names, scope, by ID;
	// elsewhere holds those at other addresses, by scope.
	objects   map[string]R
	elsewhere map[string]map[string]R
	scope     string
	nextID    int
	pageSize  int64

	createErr, getErr, listErr, updateErr, deleteErr error

	accountIDs []string
	listPages  []int64
	writes     int
}

func newFakeCRUDStore[R any](pageSize int64, scope string) *fakeCRUDStore[R] {
	return &fakeCRUDStore[R]{objects: map[string]R{}, elsewhere: map[string]map[string]R{}, scope: scope,
		pageSize: pageSize}
}

// at returns the objects at the address scope.
func (s *fakeCRUDStore[R]) at(scope string) map[string]R {
	if scope == s.scope {
		return s.objects
	}
	if s.elsewhere[scope] == nil {
		s.elsewhere[scope] = map[string]R{}
	}
	return s.elsewhere[scope]
}

// account records the account ID a call names.
func (s *fakeCRUDStore[R]) account(id string) error {
	s.accountIDs = append(s.accountIDs, id)
	if id == "" {
		return errors.New("missing required account_id parameter")
	}
	return nil
}

func fakeCRUDNotFound(method, accountID, id string) error {
	return v7APIError(method, fmt.Sprintf("accounts/%s/objects/%s", accountID, id), http.StatusNotFound, 1000,
		"not found")
}

// create stores the object build returns at scope, under id, or a new ID
// when id is empty.
func (s *fakeCRUDStore[R]) create(accountID, scope, id string, build func(id string) R) (*R, error) {
	if err := s.account(accountID); err != nil {
		return nil, err
	}
	s.writes++
	if s.createErr != nil {
		return nil, s.createErr
	}
	objects := s.at(scope)
	if id == "" {
		s.nextID++
		id = fmt.Sprintf("id-%d", s.nextID)
	} else if _, taken := objects[id]; taken {
		return nil, v7APIError(http.MethodPost, fmt.Sprintf("accounts/%s/objects", accountID), http.StatusConflict,
			1000, "already exists")
	}
	object := build(id)
	objects[id] = object
	return &object, nil
}

func (s *fakeCRUDStore[R]) get(accountID, scope, id string) (*R, error) {
	if err := s.account(accountID); err != nil {
		return nil, err
	}
	if s.getErr != nil {
		return nil, s.getErr
	}
	object, ok := s.at(scope)[id]
	if !ok {
		return nil, fakeCRUDNotFound(http.MethodGet, accountID, id)
	}
	return &object, nil
}

// fakeList returns a page of s's objects at scope, in the order of their IDs,
// each as listed shows it.
func fakeList[R any, L any](s *fakeCRUDStore[R], accountID, scope string, page, perPage int64,
	listed func(*R) L) (*pagination.V4PagePaginationArray[L], error) {
	if err := s.account(accountID); err != nil {
		return nil, err
	}
	s.listPages = append(s.listPages, page)
	if s.listErr != nil {
		return nil, s.listErr
	}
	if perPage > s.pageSize {
		return nil, v7APIError(http.MethodGet, fmt.Sprintf("accounts/%s/objects", accountID), http.StatusBadRequest,
			1000, fmt.Sprintf("per_page must be no greater than %d", s.pageSize))
	}
	objects := s.at(scope)
	ids := slices.Sorted(maps.Keys(objects))
	start := min(max(page-1, 0)*perPage, int64(len(ids)))
	end := min(start+perPage, int64(len(ids)))
	res := &pagination.V4PagePaginationArray[L]{
		ResultInfo: pagination.V4PagePaginationArrayResultInfo{Page: page, PerPage: perPage},
	}
	for _, id := range ids[start:end] {
		object := objects[id]
		res.Result = append(res.Result, listed(&object))
	}
	return res, nil
}

// update applies apply to the object id at scope.
func (s *fakeCRUDStore[R]) update(accountID, scope, id string, apply func(*R)) (*R, error) {
	if err := s.account(accountID); err != nil {
		return nil, err
	}
	s.writes++
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	objects := s.at(scope)
	object, ok := objects[id]
	if !ok {
		return nil, fakeCRUDNotFound(http.MethodPut, accountID, id)
	}
	apply(&object)
	objects[id] = object
	return &object, nil
}

func (s *fakeCRUDStore[R]) delete(accountID, scope, id string) error {
	if err := s.account(accountID); err != nil {
		return err
	}
	s.writes++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	objects := s.at(scope)
	if _, ok := objects[id]; !ok {
		return fakeCRUDNotFound(http.MethodDelete, accountID, id)
	}
	delete(objects, id)
	return nil
}

// crudKindReconciler is a generated kind's reconciler.
type crudKindReconciler[O conditionedObject, A any, R any, L any] interface {
	reconcile.Reconciler
	SetupWithManager(mgr ctrl.Manager) error
	crud() *crudResourceReconciler[O, A, R, L]
}

// crudSpecEdit is an edit of a generated kind's sample spec: of field, as
// what describes it.
type crudSpecEdit[O any] struct {
	field, what string
	edit        func(O)
}

// crudScopeEdit is an edit of a field that names a resource's object in every
// call, besides the account and the ID, and the fake's scope for the address
// it leads to.
type crudScopeEdit[O any] struct {
	crudSpecEdit[O]
	scope string
}

// crudKindSpecs is the table kflare-gen writes for a generated kind.
type crudKindSpecs[O conditionedObject, A any, R any, L any] struct {
	kind crudResourceKind[O, A, R, L]
	// keyField is the spec field a resource adopts an object by, and
	// pageSize the most objects a page of the list holds: 0 for a kind that
	// never lists. scope is the address of the sample's object, as the fake
	// keeps objects apart by it.
	keyField string
	pageSize int64
	scope    string
	// newReconciler returns the kind's reconciler, with newAPI as its
	// factory: nil for the production one.
	newReconciler func(c client.Client, newAPI func(string) (A, error)) crudKindReconciler[O, A, R, L]
	// newFake returns the kind's fake service on store.
	newFake func(store *fakeCRUDStore[R]) A
	// defaultAPI is the production factory, and service an empty value of
	// the cloudflare-go v7 service it returns.
	defaultAPI func(token string) (A, error)
	service    any
	// newResource returns a resource at key, using account, with the
	// sample's spec.
	newResource func(key types.NamespacedName, account string) O
	// seed returns an object with id and key, as Cloudflare would hold it.
	seed func(id, key string) R
	// created reports whether an object holds every field of a resource's
	// spec that Cloudflare reports, those set at creation only included.
	created func(obj O, current *R) bool
	// changes edit each field Update takes, immutable each field it does not
	// (accountRef first), and invalid set values the CRD refuses. elsewhere
	// moves the sample to another address, a field at a time.
	changes, immutable, invalid []crudSpecEdit[O]
	elsewhere                   []crudScopeEdit[O]
}

// describeCRUDKind runs the shared specs on a generated kind's table.
func describeCRUDKind[O conditionedObject, A any, R any, L any](t crudKindSpecs[O, A, R, L]) bool {
	return Describe(t.kind.name+" (generated specs)", func() {
		k := t.kind
		base := "gen-" + strings.ToLower(k.name)
		const ns = "default"
		var (
			key         = types.NamespacedName{Name: base, Namespace: ns}
			otherKey    = types.NamespacedName{Name: base + "-other", Namespace: ns}
			accountName = base + "-account"
			secretName  = base + "-token"
			accountID   = "acct-" + base
			// otherAccountName is a second CloudflareAccount, for claims made
			// through another one.
			otherAccountName = base + "-other-account"
		)
		ctx := context.Background()
		// byKey is set for a kind whose objects their key identifies: it reads
		// them by their key and never lists them.
		byKey := k.list == nil
		// addressFields are the fields that name an object besides the account
		// and the ID.
		addressFields := map[string]bool{}
		for _, e := range t.elsewhere {
			addressFields[e.field] = true
		}

		var store *fakeCRUDStore[R]
		var r crudKindReconciler[O, A, R, L]

		createAccount := func(ready bool) {
			account := &cloudflarev1alpha1.CloudflareAccount{
				ObjectMeta: metav1.ObjectMeta{Name: accountName},
				Spec: cloudflarev1alpha1.CloudflareAccountSpec{
					AccountID:      accountID,
					TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
				},
			}
			Expect(k8sClient.Create(ctx, account)).To(Succeed())
			if ready {
				account.Status.Conditions = []metav1.Condition{{
					Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
					Reason: "Validated", LastTransitionTime: metav1.Now(),
				}}
				Expect(k8sClient.Status().Update(ctx, account)).To(Succeed())
			}
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
				Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
			})).To(Succeed())
		}

		// create creates a resource that already carries the finalizer.
		create := func(at types.NamespacedName, annotations map[string]string) O {
			obj := t.newResource(at, accountName)
			obj.SetAnnotations(annotations)
			obj.SetFinalizers([]string{reconciler.Finalizer})
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			return obj
		}

		get := func() O {
			obj := k.newObject()
			Expect(k8sClient.Get(ctx, key, obj)).To(Succeed())
			return obj
		}

		ready := func() metav1.Condition {
			cond := meta.FindStatusCondition(get().GetConditions(), cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			return *cond
		}

		reconcileIt := func() (ctrl.Result, error) {
			return r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		}

		// adopt and adoptOrCreate are the adoption policies' annotations.
		adopt := map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt}
		adoptOrCreate := map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdoptOrCreate}

		// seed stores an object with key, as Cloudflare would hold it, under id,
		// or under its key for a kind whose objects their key identifies, and
		// returns the ID it is stored under.
		seed := func(id, key string) string {
			if byKey {
				id = key
			}
			store.objects[id] = t.seed(id, key)
			return id
		}

		// findErr is the error of the call that finds the object a resource
		// would adopt: the list's, or the read by its key.
		findErr := func() *error {
			if byKey {
				return &store.getErr
			}
			return &store.listErr
		}

		// synced creates a resource and the object it manages.
		synced := func(annotations map[string]string) string {
			createAccount(true)
			create(key, annotations)
			_, err := reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(store.objects).To(HaveLen(1))
			return k.statusID(get())
		}

		BeforeEach(func() {
			store = newFakeCRUDStore[R](t.pageSize, t.scope)
			r = t.newReconciler(k8sClient, func(string) (A, error) { return t.newFake(store), nil })
		})

		AfterEach(func() {
			forceDelete(ctx, k.newObject(), key)
			forceDelete(ctx, k.newObject(), otherKey)
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: otherAccountName})
			forceDelete(ctx, &corev1.Secret{}, types.NamespacedName{Name: secretName, Namespace: ns})
		})

		Describe("Reconcile", func() {
			It("returns no error when the resource does not exist", func() {
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
			})

			It("adds the finalizer on the first reconcile without calling Cloudflare", func() {
				Expect(k8sClient.Create(ctx, t.newResource(key, accountName))).To(Succeed())
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(get().GetFinalizers()).To(ContainElement(reconciler.Finalizer))
				Expect(store.accountIDs).To(BeEmpty())
			})

			It("retries after a minute when the account is not ready", func() {
				createAccount(false)
				create(key, nil)
				result, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
				Expect(ready().Reason).To(Equal("AccountNotReady"))
			})

			It("reports InvalidToken when no Cloudflare client can be built", func() {
				createAccount(true)
				create(key, nil)
				r = t.newReconciler(k8sClient, func(string) (A, error) { return *new(A), errors.New("bad token") })
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(ready().Reason).To(Equal("InvalidToken"))
			})

			It("creates the "+k.noun+", claims it and reports its ID", func() {
				id := synced(nil)
				Expect(store.objects).To(HaveKey(id))
				Expect(store.accountIDs).To(HaveEach(accountID))
				if byKey {
					Expect(store.listPages).To(BeEmpty(), "the "+k.noun+" is read by its "+t.keyField+", never listed")
				} else {
					Expect(store.listPages).To(Equal([]int64{1}), "one page holds every object of the account")
				}
				Expect(store.writes).To(Equal(1), "the new object is what the spec asks for: nothing to update")
				obj := get()
				object := store.objects[id]
				Expect(t.created(obj, &object)).To(BeTrue(), "the new object holds every field of the spec")
				Expect(k.matches(obj, &object)).To(BeTrue(), "the new object is what the spec asks for")
				Expect(obj.GetLabels()).To(HaveKeyWithValue(k.claimLabel, id))
				Expect(ready().Status).To(Equal(metav1.ConditionTrue))
			})

			It("creates the "+k.noun+" with every field the spec sets", func() {
				createAccount(true)
				obj := t.newResource(key, accountName)
				obj.SetFinalizers([]string{reconciler.Finalizer})
				// The address stays the sample's, where the store's objects are:
				// the elsewhere specs move it.
				for _, edit := range slices.Concat(t.changes, t.immutable) {
					if edit.field != "accountRef" && !addressFields[edit.field] {
						edit.edit(obj)
					}
				}
				Expect(k8sClient.Create(ctx, obj)).To(Succeed())
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.writes).To(Equal(1))
				object := store.objects[k.statusID(get())]
				Expect(t.created(get(), &object)).To(BeTrue(), "the new object holds every field of the spec")
			})

			It("writes nothing to Cloudflare once the "+k.noun+" is in sync", func() {
				synced(nil)
				writes, pages := store.writes, len(store.listPages)
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.writes).To(Equal(writes))
				Expect(store.listPages).To(HaveLen(pages), "the object is read by its ID, not found in the list")
			})

			It("schedules a drift check after a sync, once the manager sets an interval", func() {
				synced(nil)
				writes := store.writes
				expectDriftCheck(reconcileIt)
				Expect(store.writes).To(Equal(writes))
			})

			if byKey {
				It("adopts the "+k.noun+" with the same "+t.keyField+" by reading it, without listing", func() {
					createAccount(true)
					obj := create(key, adopt)
					id := seed("", k.key(obj))
					_, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(store.listPages).To(BeEmpty())
					Expect(k.statusID(get())).To(Equal(id))
					Expect(get().GetLabels()).To(HaveKeyWithValue(k.claimLabel, id))
					Expect(get().GetAnnotations()).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))
					Expect(store.writes).To(BeZero(), "the adopted object is what the spec asks for")
				})
			} else {
				It("adopts the "+k.noun+" with the same "+t.keyField+" from a later page of the list", func() {
					createAccount(true)
					obj := create(key, adopt)
					// A full first page of other objects; the one to adopt sorts last.
					for i := range t.pageSize {
						seed(fmt.Sprintf("a-%04d", i), fmt.Sprintf("other-%04d", i))
					}
					seed("z-last", k.key(obj))
					_, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(store.listPages[:2]).To(Equal([]int64{1, 2}))
					Expect(store.objects).To(HaveLen(int(t.pageSize)+1), "nothing is created")
					Expect(k.statusID(get())).To(Equal("z-last"))
					Expect(get().GetLabels()).To(HaveKeyWithValue(k.claimLabel, "z-last"))
					Expect(get().GetAnnotations()).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))
					Expect(store.writes).To(BeZero(), "the adopted object is what the spec asks for")
				})
			}

			It("reports AlreadyExists for a "+k.noun+" with the same "+t.keyField+" without an adoption policy, "+
				"and leaves it alone", func() {
				createAccount(true)
				obj := create(key, nil)
				seed("existing", k.key(obj))
				result, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
				Expect(ready().Reason).To(Equal(reasonAlreadyExists))
				Expect(ready().Message).To(ContainSubstring(k.key(obj)))
				Expect(ready().Message).To(ContainSubstring(reconciler.AdoptionPolicyAnnotation))
				Expect(store.writes).To(BeZero())
				Expect(store.objects).To(HaveLen(1))
				Expect(k.statusID(get())).To(BeEmpty())
				Expect(get().GetLabels()).NotTo(HaveKey(k.claimLabel))
			})

			It("reports NotFound with the adopt policy when there is no "+k.noun+" to adopt, and creates none",
				func() {
					createAccount(true)
					create(key, adopt)
					result, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
					Expect(ready().Reason).To(Equal(reasonNotFound))
					Expect(store.writes).To(BeZero())
					Expect(store.objects).To(BeEmpty())
				})

			It("reports an unknown adoption policy instead of creating or adopting", func() {
				createAccount(true)
				obj := create(key, map[string]string{reconciler.AdoptionPolicyAnnotation: "Adopt"})
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(ready().Reason).To(Equal(reasonInvalidAdoptionPolicy))
				Expect(store.objects).To(BeEmpty(), "nothing is created")

				seed("existing", k.key(obj))
				_, err = reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(ready().Reason).To(Equal(reasonInvalidAdoptionPolicy))
				Expect(store.writes).To(BeZero())
				Expect(get().GetLabels()).NotTo(HaveKey(k.claimLabel), "nothing is adopted")
			})

			It("creates the "+k.noun+" with the adopt-or-create policy when there is none to adopt", func() {
				createAccount(true)
				create(key, adoptOrCreate)
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.objects).To(HaveLen(1))
				Expect(get().GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation))
				Expect(ready().Status).To(Equal(metav1.ConditionTrue))
			})

			It("marks the "+k.noun+" it is about to create, so a failed create leaves the mark", func() {
				createAccount(true)
				obj := create(key, nil)
				store.createErr = errors.New("unavailable")
				_, err := reconcileIt()
				Expect(err).To(MatchError("unavailable"))
				Expect(get().GetAnnotations()).To(HaveKeyWithValue(reconciler.CreatingAnnotation, k.key(obj)))

				By("creating it on the next reconcile, which drops the mark with the claim")
				store.createErr = nil
				_, err = reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(get().GetAnnotations()).NotTo(HaveKey(reconciler.CreatingAnnotation))
				Expect(get().GetLabels()).To(HaveKeyWithValue(k.claimLabel, k.statusID(get())))
			})

			It("finds the "+k.noun+" it was creating when neither its claim nor its status was recorded", func() {
				createAccount(true)
				obj := t.newResource(key, accountName)
				id := seed("created", k.key(obj))
				create(key, map[string]string{reconciler.CreatingAnnotation: k.key(obj)})
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(ready().Status).To(Equal(metav1.ConditionTrue))
				Expect(k.statusID(get())).To(Equal(id))
				Expect(store.writes).To(BeZero(), "nothing is created")
				Expect(get().GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation), "it is not adopted")
				Expect(get().GetAnnotations()).NotTo(HaveKey(reconciler.CreatingAnnotation))
			})

			It("finds the "+k.noun+" it claimed when its status was not recorded", func() {
				createAccount(true)
				obj := create(key, nil)
				id := seed("claimed", k.key(obj))
				obj.SetLabels(map[string]string{k.claimLabel: id})
				Expect(k8sClient.Update(ctx, obj)).To(Succeed())
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(ready().Status).To(Equal(metav1.ConditionTrue))
				Expect(k.statusID(get())).To(Equal(id))
				Expect(store.writes).To(BeZero())
				Expect(get().GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation))
			})

			if !byKey {
				It("creates the "+k.noun+" when the one it would adopt is gone before it is read", func() {
					createAccount(true)
					obj := create(key, adoptOrCreate)
					seed("listed", k.key(obj))
					store.getErr = fakeCRUDNotFound(http.MethodGet, accountID, "listed")
					_, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					id := k.statusID(get())
					Expect(id).NotTo(Equal("listed"))
					Expect(store.objects).To(HaveKey(id))
					Expect(ready().Status).To(Equal(metav1.ConditionTrue))
					Expect(get().GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation), "it created this one")
				})
			}

			It("returns a failed read of the "+k.noun+" it would adopt for back-off", func() {
				createAccount(true)
				obj := create(key, adopt)
				seed("listed", k.key(obj))
				store.getErr = errors.New("unavailable")
				_, err := reconcileIt()
				Expect(err).To(MatchError("unavailable"))
				Expect(store.objects).To(HaveLen(1), "nothing is created")
			})

			// claimedBy creates, at otherKey, a resource that uses the
			// CloudflareAccount account and claims the object with the
			// sample's key, and stores that object.
			claimedBy := func(account string) {
				other := t.newResource(otherKey, account)
				other.SetFinalizers([]string{reconciler.Finalizer})
				Expect(k8sClient.Create(ctx, other)).To(Succeed())
				other.SetLabels(map[string]string{k.claimLabel: seed("taken", k.key(other))})
				Expect(k8sClient.Update(ctx, other)).To(Succeed())
			}

			// expectConflict reconciles the resource, and expects it to report
			// that the resource at otherKey manages its object.
			expectConflict := func() {
				GinkgoHelper()
				result, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
				Expect(ready().Reason).To(Equal(k.conflictReason))
				Expect(ready().Message).To(ContainSubstring(otherKey.String()))
				Expect(store.writes).To(BeZero())
			}

			// expectAdopted reconciles the resource, and expects it to adopt the
			// object with the sample's key.
			expectAdopted := func() {
				GinkgoHelper()
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(ready().Status).To(Equal(metav1.ConditionTrue), ready().Message)
				Expect(get().GetAnnotations()).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))
				Expect(store.writes).To(BeZero())
			}

			It("reports "+k.conflictReason+" instead of taking over a "+k.noun+" another "+k.name+" manages", func() {
				createAccount(true)
				claimedBy(accountName)
				create(key, nil)
				expectConflict()
			})

			It("reports "+k.conflictReason+" for a claim made through another CloudflareAccount for the same "+
				"Cloudflare account", func() {
				createAccount(true)
				Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.CloudflareAccount{
					ObjectMeta: metav1.ObjectMeta{Name: otherAccountName},
					Spec: cloudflarev1alpha1.CloudflareAccountSpec{
						AccountID:      accountID,
						TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
					},
				})).To(Succeed())
				claimedBy(otherAccountName)
				create(key, adopt)
				expectConflict()
			})

			It("adopts a "+k.noun+" whose ID a resource claims in another Cloudflare account", func() {
				createAccount(true)
				Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.CloudflareAccount{
					ObjectMeta: metav1.ObjectMeta{Name: otherAccountName},
					Spec: cloudflarev1alpha1.CloudflareAccountSpec{
						AccountID:      "acct-elsewhere",
						TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
					},
				})).To(Succeed())
				claimedBy(otherAccountName)
				create(key, adopt)
				expectAdopted()
			})

			It("adopts a "+k.noun+" that a resource whose CloudflareAccount is gone claims", func() {
				createAccount(true)
				claimedBy(otherAccountName)
				create(key, adopt)
				expectAdopted()
			})

			// createElsewhere creates the resource with the sample's spec moved
			// to another address by e, and annotations.
			createElsewhere := func(e crudScopeEdit[O], annotations map[string]string) O {
				obj := t.newResource(key, accountName)
				obj.SetAnnotations(annotations)
				obj.SetFinalizers([]string{reconciler.Finalizer})
				e.edit(obj)
				Expect(k8sClient.Create(ctx, obj)).To(Succeed())
				return obj
			}

			for _, e := range t.elsewhere {
				It("adopts the "+k.noun+" with the same "+t.keyField+" after "+e.what+", though another "+k.name+
					" claims the one at the sample's", func() {
					createAccount(true)
					claimedBy(accountName)
					obj := createElsewhere(e, adopt)
					id := k.key(obj)
					if !byKey {
						id = "elsewhere"
					}
					store.at(e.scope)[id] = t.seed(id, k.key(obj))
					expectAdopted()
					Expect(k.statusID(get())).To(Equal(id))
					Expect(store.objects).To(HaveLen(1), "the claimed one is left alone")
				})

				It("creates its own "+k.noun+" after "+e.what+", rather than adopting the one at the sample's",
					func() {
						createAccount(true)
						seed("existing", k.key(t.newResource(key, accountName)))
						createElsewhere(e, adoptOrCreate)
						_, err := reconcileIt()
						Expect(err).NotTo(HaveOccurred())
						Expect(ready().Status).To(Equal(metav1.ConditionTrue), ready().Message)
						Expect(get().GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation))
						Expect(store.at(e.scope)).To(HaveKey(k.statusID(get())))
						Expect(store.objects).To(HaveLen(1), "the one at the sample's is left alone")
					})
			}

			// An object identified by its key comes back under the same ID;
			// another kind's under a new one.
			expectRecreated := func(id string, writes int) {
				GinkgoHelper()
				newID := k.statusID(get())
				Expect(store.writes).To(Equal(writes+1), "one create")
				if !byKey {
					Expect(newID).NotTo(Equal(id))
				}
				Expect(store.objects).To(HaveKey(newID))
				Expect(get().GetLabels()).To(HaveKeyWithValue(k.claimLabel, newID))
			}

			It("recreates the "+k.noun+" when it was deleted outside kflare", func() {
				id := synced(nil)
				writes := store.writes
				delete(store.objects, id)
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				expectRecreated(id, writes)
			})

			It("recreates an adopted "+k.noun+" deleted outside kflare as its own with adopt-or-create", func() {
				createAccount(true)
				obj := create(key, adoptOrCreate)
				id := seed("existing", k.key(obj))
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(get().GetAnnotations()).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))

				writes := store.writes
				delete(store.objects, id)
				_, err = reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				expectRecreated(id, writes)
				Expect(get().GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation), "kflare created this one")
			})

			It("reports NotFound with the adopt policy when an adopted "+k.noun+" is deleted outside kflare",
				func() {
					createAccount(true)
					obj := create(key, adopt)
					id := seed("existing", k.key(obj))
					_, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())

					delete(store.objects, id)
					writes := store.writes
					_, err = reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(ready().Reason).To(Equal(reasonNotFound))
					Expect(store.writes).To(Equal(writes), "nothing is created")
				})

			for _, change := range t.changes {
				It("updates the "+k.noun+" in place after "+change.what, func() {
					id := synced(nil)
					writes := store.writes
					obj := get()
					change.edit(obj)
					Expect(k8sClient.Update(ctx, obj)).To(Succeed())
					_, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())

					obj = get()
					Expect(k.statusID(obj)).To(Equal(id))
					object := store.objects[id]
					Expect(k.matches(obj, &object)).To(BeTrue(), "the object is what the edited spec asks for")
					Expect(store.writes).To(Equal(writes + 1))
					Expect(ready().Status).To(Equal(metav1.ConditionTrue))
				})
			}

			if len(t.changes) > 0 {
				change := t.changes[0]
				It("sets TerminalError when Cloudflare refuses an update", func() {
					synced(nil)
					store.updateErr = v7APIError(http.MethodPut, "accounts/"+accountID+"/objects", http.StatusBadRequest,
						1000, "refused")
					obj := get()
					change.edit(obj)
					Expect(k8sClient.Update(ctx, obj)).To(Succeed())
					result, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(result.IsZero()).To(BeTrue())
					Expect(ready().Reason).To(Equal("TerminalError"))
					Expect(ready().Message).To(Equal("Terminal Cloudflare API error: refused (1000)"))
				})

				It("returns a failed update for back-off", func() {
					id := synced(nil)
					before := store.objects[id]
					store.updateErr = errors.New("unavailable")
					obj := get()
					change.edit(obj)
					Expect(k8sClient.Update(ctx, obj)).To(Succeed())
					_, err := reconcileIt()
					Expect(err).To(MatchError("unavailable"))
					Expect(store.objects[id]).To(Equal(before))
				})
			}

			It("returns retryable errors for back-off", func() {
				createAccount(true)
				create(key, nil)
				*findErr() = errors.New("connection reset")
				_, err := reconcileIt()
				Expect(err).To(MatchError("connection reset"))
				Expect(ready().Reason).To(Equal("APIError"))

				*findErr() = nil
				store.createErr = errors.New("unavailable")
				_, err = reconcileIt()
				Expect(err).To(MatchError("unavailable"))
			})

			It("records an Event when Ready changes, and none on a reconcile that changes nothing", func() {
				recorder := events.NewFakeRecorder(10)
				SetEventRecorder(recorder)
				DeferCleanup(func() { SetEventRecorder(nil) })
				createAccount(true)
				create(key, nil)
				*findErr() = errors.New("connection reset")
				_, err := reconcileIt()
				Expect(err).To(MatchError("connection reset"))
				Expect(recorder.Events).To(Receive(Equal("Warning APIError " + ready().Message)))

				*findErr() = nil
				_, err = reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				cond := ready()
				Expect(cond.Status).To(Equal(metav1.ConditionTrue))
				Expect(recorder.Events).To(Receive(Equal("Normal " + cond.Reason + " " + cond.Message)))

				_, err = reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(recorder.Events).To(BeEmpty(), "a reconcile that changes nothing records nothing")
			})

			It("returns a failed read of the "+k.noun+" for back-off", func() {
				synced(nil)
				store.getErr = v7APIError(http.MethodGet, "accounts/"+accountID+"/objects/1",
					http.StatusServiceUnavailable, 0, "")
				_, err := reconcileIt()
				Expect(err).To(MatchError(store.getErr))
				Expect(ready().Reason).To(Equal("APIError"))
				Expect(ready().Message).To(Equal("Cloudflare API error: HTTP 503 Service Unavailable"))
			})

			It("keeps its error paths as they are with a drift check interval", func() {
				turnOnDriftChecks()
				synced(nil)

				By("returning a retryable error for back-off, with nothing scheduled")
				store.getErr = v7APIError(http.MethodGet, "accounts/"+accountID+"/objects/1",
					http.StatusServiceUnavailable, 0, "")
				result, err := reconcileIt()
				Expect(err).To(MatchError(store.getErr))
				Expect(result).To(BeZero())
				Expect(ready().Reason).To(Equal("APIError"))

				By("waiting for a change after a terminal error")
				store.getErr = v7APIError(http.MethodGet, "accounts/"+accountID+"/objects/1", http.StatusForbidden,
					10000, "forbidden")
				result, err = reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeZero())
				Expect(ready().Reason).To(Equal("TerminalError"))
			})
		})

		Describe("deletion", func() {
			startDeletion := func() {
				Expect(k8sClient.Delete(ctx, get())).To(Succeed())
			}
			expectGone := func() {
				Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, k.newObject()))).To(BeTrue())
			}

			It("deletes the "+k.noun+" from Cloudflare and removes the finalizer", func() {
				synced(nil)
				startDeletion()
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.objects).To(BeEmpty())
				expectGone()
			})

			It("keeps the "+k.noun+" with the retain policy", func() {
				synced(map[string]string{reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain})
				startDeletion()
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.objects).To(HaveLen(1))
				expectGone()
			})

			It("keeps the "+k.noun+" it created when the default deletion policy is retain", func() {
				Expect(reconciler.SetDefaultDeletionPolicy(reconciler.DeletionPolicyRetain)).To(Succeed())
				DeferCleanup(reconciler.SetDefaultDeletionPolicy, reconciler.DeletionPolicyDelete)
				synced(nil)
				writes := store.writes
				startDeletion()
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.objects).To(HaveLen(1))
				Expect(store.writes).To(Equal(writes))
				expectGone()
			})

			// adopted creates a resource with annotations that adopts an
			// existing object.
			adopted := func(annotations map[string]string) string {
				createAccount(true)
				obj := create(key, annotations)
				id := seed("existing", k.key(obj))
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(k.statusID(get())).To(Equal(id))
				return id
			}

			It("keeps a "+k.noun+" it adopted", func() {
				id := adopted(adopt)
				startDeletion()
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.objects).To(HaveKey(id))
				Expect(store.writes).To(BeZero())
				expectGone()
			})

			It("deletes a "+k.noun+" it adopted with the delete policy", func() {
				adopted(map[string]string{
					reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt,
					reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyDelete,
				})
				startDeletion()
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.objects).To(BeEmpty())
				expectGone()
			})

			It("finishes when the "+k.noun+" is already gone from Cloudflare", func() {
				id := synced(nil)
				delete(store.objects, id)
				startDeletion()
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				expectGone()
			})

			for _, blocker := range k.deletionBlockers {
				It("keeps the "+k.noun+" and reports "+blocker.reason+" while Cloudflare refuses the delete with "+
					fmt.Sprint(blocker.code)+", and deletes it once Cloudflare no longer does", func() {
					id := synced(nil)
					store.deleteErr = v7APIError(http.MethodDelete, "accounts/"+accountID+"/objects/"+id,
						http.StatusConflict, blocker.code, "refused")
					startDeletion()
					result, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval), "the object is not watched")
					Expect(ready().Reason).To(Equal(blocker.reason))
					Expect(ready().Message).To(ContainSubstring(k.key(get())))
					Expect(ready().Message).To(ContainSubstring(blocker.message))
					Expect(ready().Message).To(ContainSubstring(reconciler.DeletionPolicyAnnotation + ": " +
						reconciler.DeletionPolicyRetain))
					Expect(get().GetFinalizers()).To(ContainElement(reconciler.Finalizer))
					Expect(store.objects).To(HaveKey(id))

					By("deleting it once Cloudflare takes the delete")
					store.deleteErr = nil
					_, err = reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(store.objects).To(BeEmpty())
					expectGone()
				})
			}

			It("keeps the finalizer when Cloudflare fails, so deletion is retried", func() {
				synced(nil)
				store.deleteErr = errors.New("unavailable")
				startDeletion()
				_, err := reconcileIt()
				Expect(err).To(MatchError("unavailable"))
				Expect(get().GetFinalizers()).To(ContainElement(reconciler.Finalizer))
			})

			It("keeps the finalizer when the credentials cannot be resolved", func() {
				synced(nil)
				forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
				startDeletion()
				_, err := reconcileIt()
				Expect(err).To(HaveOccurred())
				Expect(get().GetFinalizers()).To(ContainElement(reconciler.Finalizer))
			})

			It("returns the error when no Cloudflare client can be built", func() {
				synced(nil)
				r = t.newReconciler(k8sClient, func(string) (A, error) { return *new(A), errors.New("bad token") })
				startDeletion()
				_, err := reconcileIt()
				Expect(err).To(MatchError("bad token"))
			})

			It("does nothing for a resource without the finalizer", func() {
				result, err := r.crud().reconcileDelete(ctx, k.newObject())
				Expect(err).NotTo(HaveOccurred())
				Expect(result.IsZero()).To(BeTrue())
			})
		})

		Describe("pausing", func() {
			// reconcilePaused reconciles the resource, and checks that it made no
			// Cloudflare call, which the fake records the account of, and
			// scheduled nothing.
			reconcilePaused := func() {
				GinkgoHelper()
				calls := len(store.accountIDs)
				result, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeZero())
				Expect(store.accountIDs).To(HaveLen(calls), "a paused resource called Cloudflare")
				Expect(ready().Reason).To(Equal(reasonPaused))
			}

			It("calls Cloudflare for nothing while paused, and applies the spec once unpaused", func() {
				id := synced(nil)
				observed := ready().ObservedGeneration
				setPaused(ctx, get(), true)
				obj := get()
				for _, change := range t.changes {
					change.edit(obj)
				}
				Expect(k8sClient.Update(ctx, obj)).To(Succeed())
				reconcilePaused()
				Expect(ready().Status).To(Equal(metav1.ConditionTrue))
				Expect(ready().ObservedGeneration).To(Equal(observed), "the spec changed while paused is not observed")

				By("applying the spec once unpaused")
				setPaused(ctx, get(), false)
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				object := store.objects[id]
				Expect(k.matches(get(), &object)).To(BeTrue(), "the object is what the spec asks for")
				Expect(ready().Reason).To(Equal("Synced"))
				Expect(ready().ObservedGeneration).To(Equal(get().GetGeneration()))
			})

			It("keeps a paused resource's "+k.noun+" and finalizer when it is deleted, and deletes both once unpaused",
				func() {
					id := synced(nil)
					setPaused(ctx, get(), true)
					Expect(k8sClient.Delete(ctx, get())).To(Succeed())
					reconcilePaused()
					Expect(store.objects).To(HaveKey(id))
					Expect(get().GetFinalizers()).To(ContainElement(reconciler.Finalizer))
					Expect(ready().Message).To(Equal(pausedDeletionMessage))

					By("deleting both once unpaused")
					setPaused(ctx, get(), false)
					_, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(store.objects).NotTo(HaveKey(id))
					Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, k.newObject()))).To(BeTrue())
				})

			It("adds no finalizer to a resource created paused", func() {
				createAccount(true)
				obj := t.newResource(key, accountName)
				obj.SetAnnotations(map[string]string{reconciler.PausedAnnotation: "true"})
				Expect(k8sClient.Create(ctx, obj)).To(Succeed())
				reconcilePaused()
				Expect(get().GetFinalizers()).To(BeEmpty())
				Expect(ready().Status).To(Equal(metav1.ConditionUnknown))
			})
		})

		Describe("validation", func() {
			for _, change := range t.immutable {
				It("refuses "+change.what, func() {
					create(key, nil)
					obj := get()
					change.edit(obj)
					Expect(k8sClient.Update(ctx, obj)).To(MatchError(ContainSubstring(change.field + " is immutable")))
				})
			}

			for _, invalid := range t.invalid {
				It("refuses "+invalid.what, func() {
					obj := t.newResource(key, accountName)
					invalid.edit(obj)
					Expect(k8sClient.Create(ctx, obj)).To(MatchError(ContainSubstring("spec." + invalid.field)))
				})
			}
		})

		Describe("setup", func() {
			It("builds cloudflare-go v7's service from a token, and refuses an empty one", func() {
				api, err := t.defaultAPI("token")
				Expect(err).NotTo(HaveOccurred())
				Expect(api).To(BeAssignableToTypeOf(t.service))
				_, err = t.defaultAPI("")
				Expect(err).To(HaveOccurred())
			})

			It("uses the production factory when none is injected", func() {
				_, err := t.newReconciler(k8sClient, nil).crud().newAPI("")
				Expect(err).To(HaveOccurred())
			})

			It("registers with a manager", func() {
				mgr, err := manager.New(cfg, manager.Options{
					Controller: config.Controller{SkipNameValidation: new(true)},
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(t.newReconciler(mgr.GetClient(), nil).SetupWithManager(mgr)).To(Succeed())
			})

			It("maps a CloudflareAccount to the resources that use it", func() {
				create(key, nil)
				account := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Name: accountName}}
				Expect(r.crud().resourcesForAccount(ctx, account)).To(ConsistOf(reconcile.Request{NamespacedName: key}))
				other := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Name: accountName + "-other"}}
				Expect(r.crud().resourcesForAccount(ctx, other)).To(BeEmpty())

				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				Expect(r.crud().resourcesForAccount(cancelled, account)).To(BeNil(), "the list fails")
			})

			It("counts as a user of its CloudflareAccount", func() {
				create(key, nil)
				deps, err := dependentsOf(ctx, k8sClient, "", accountName, accountDependents)
				Expect(err).NotTo(HaveOccurred())
				Expect(deps).To(ConsistOf(k.name + " " + key.String()))
			})
		})
	})
}

// recordingManager records the runnables added to a manager, among them the
// controllers a setup registers.
type recordingManager struct {
	manager.Manager
	added []manager.Runnable
}

func (m *recordingManager) Add(r manager.Runnable) error {
	m.added = append(m.added, r)
	return m.Manager.Add(r)
}

var _ = Describe("SetupGeneratedReconcilers", func() {
	It("registers the reconciler of every generated kind", func() {
		mgr, err := manager.New(cfg, manager.Options{
			Controller: config.Controller{SkipNameValidation: new(true)},
		})
		Expect(err).NotTo(HaveOccurred())
		recording := &recordingManager{Manager: mgr}
		Expect(SetupGeneratedReconcilers(recording)).To(Succeed())

		// controller-runtime names a controller after its kind, in lower case,
		// in the Name field of a type it does not export.
		names := make([]string, 0, len(recording.added))
		for _, r := range recording.added {
			v := reflect.Indirect(reflect.ValueOf(r))
			Expect(v.Kind()).To(Equal(reflect.Struct), "%T is not a controller", r)
			name := v.FieldByName("Name")
			Expect(name.IsValid()).To(BeTrue(), "%T has no Name field", r)
			names = append(names, name.String())
		}
		// Every generated kind's resources use a CloudflareAccount's
		// credentials directly (CRUD kinds), or reference a Zone (rule and
		// settings kinds).
		want := make([]string, 0, len(generatedAccountDependents)+len(generatedZoneDependents))
		for _, d := range slices.Concat(generatedAccountDependents, generatedZoneDependents) {
			want = append(want, strings.ToLower(d.kind))
		}
		Expect(names).To(ConsistOf(want))
	})
})
