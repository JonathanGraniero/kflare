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
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// The specs every generated kind runs. kflare-gen writes a table per kind
// (zz_generated.<kind>_test.go): a fake of its cloudflare-go v7 service on
// fakeCRUDStore, a valid resource from the kind's sample, and edits of it.
// describeCRUDKind runs the shared CRUD reconciler's lifecycle against them:
// the finalizer, credentials and the Zone, creation and its claim, an idle
// reconcile that writes nothing, adoption and its conflict, recreation,
// updates in place, the references to other kinds, errors, deletion, its
// blockers and the retain policy. It also checks the CRD: every immutable
// field, and values its validation refuses.

// fakeCRUDStore is an in-memory Cloudflare account's, or zone's, objects of
// one kind. Each generated fake converts its service's params into calls on
// it. Like Cloudflare, it returns 404 for an unknown ID, refuses a page larger
// than pageSize and a second object under a key that is its ID (an R2
// bucket's name), and keeps objects apart by their address (an R2 bucket's
// jurisdiction); like cloudflare-go, it refuses a call without its parent's
// ID.
type fakeCRUDStore[R any] struct {
	// objects are the objects at the address the sample names, scope, by ID;
	// elsewhere holds those at other addresses, by scope.
	objects   map[string]R
	elsewhere map[string]map[string]R
	scope     string
	nextID    int
	pageSize  int64
	// pageCap, when set, is the most objects a page holds, however many a list
	// asks for: Cloudflare reports the per_page it served.
	pageCap int64

	createErr, getErr, listErr, updateErr, deleteErr error

	// parentIDs holds the account's or zone's ID of each call, and listPages
	// the page each list asks for: 1 for a list that is never paged.
	parentIDs []string
	listPages []int64
	writes    int
	// sent holds the params of each New, in order.
	sent []any
	// extra holds what a kind's hooks' fake keeps.
	extra any
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

// parent records the account's or zone's ID a call names.
func (s *fakeCRUDStore[R]) parent(id string) error {
	s.parentIDs = append(s.parentIDs, id)
	if id == "" {
		return errors.New("missing required parent ID")
	}
	return nil
}

func fakeCRUDNotFound(method, parentID, id string) error {
	return v7APIError(method, fmt.Sprintf("parents/%s/objects/%s", parentID, id), http.StatusNotFound, 1000,
		"not found")
}

// create stores the object build returns at scope, under id, or a new ID
// when id is empty.
func (s *fakeCRUDStore[R]) create(parentID, scope, id string, build func(id string) R) (*R, error) {
	if err := s.parent(parentID); err != nil {
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
		return nil, v7APIError(http.MethodPost, fmt.Sprintf("parents/%s/objects", parentID), http.StatusConflict,
			1000, "already exists")
	}
	object := build(id)
	objects[id] = object
	return &object, nil
}

func (s *fakeCRUDStore[R]) get(parentID, scope, id string) (*R, error) {
	if err := s.parent(parentID); err != nil {
		return nil, err
	}
	if s.getErr != nil {
		return nil, s.getErr
	}
	object, ok := s.at(scope)[id]
	if !ok {
		return nil, fakeCRUDNotFound(http.MethodGet, parentID, id)
	}
	return &object, nil
}

// fakeList returns a page of s's objects at scope, in the order of their IDs,
// each as listed shows it.
func fakeList[R any, L any](s *fakeCRUDStore[R], parentID, scope string, page, perPage int64,
	listed func(*R) L) (*pagination.V4PagePaginationArray[L], error) {
	if err := s.parent(parentID); err != nil {
		return nil, err
	}
	s.listPages = append(s.listPages, page)
	if s.listErr != nil {
		return nil, s.listErr
	}
	if perPage > s.pageSize {
		return nil, v7APIError(http.MethodGet, fmt.Sprintf("parents/%s/objects", parentID), http.StatusBadRequest,
			1000, fmt.Sprintf("per_page must be no greater than %d", s.pageSize))
	}
	if s.pageCap > 0 {
		perPage = min(perPage, s.pageCap)
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

// fakeListAll returns all of s's objects at scope, in the order of their IDs,
// each as listed shows it, in the one page of a list that is never paged.
func fakeListAll[R any, L any](s *fakeCRUDStore[R], parentID, scope string,
	listed func(*R) L) (*pagination.SinglePage[L], error) {
	if err := s.parent(parentID); err != nil {
		return nil, err
	}
	s.listPages = append(s.listPages, 1)
	if s.listErr != nil {
		return nil, s.listErr
	}
	objects := s.at(scope)
	res := &pagination.SinglePage[L]{}
	for _, id := range slices.Sorted(maps.Keys(objects)) {
		object := objects[id]
		res.Result = append(res.Result, listed(&object))
	}
	return res, nil
}

// update applies apply to the object id at scope.
func (s *fakeCRUDStore[R]) update(parentID, scope, id string, apply func(*R)) (*R, error) {
	if err := s.parent(parentID); err != nil {
		return nil, err
	}
	s.writes++
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	objects := s.at(scope)
	object, ok := objects[id]
	if !ok {
		return nil, fakeCRUDNotFound(http.MethodPut, parentID, id)
	}
	apply(&object)
	objects[id] = object
	return &object, nil
}

func (s *fakeCRUDStore[R]) delete(parentID, scope, id string) error {
	if err := s.parent(parentID); err != nil {
		return err
	}
	s.writes++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	objects := s.at(scope)
	if _, ok := objects[id]; !ok {
		return fakeCRUDNotFound(http.MethodDelete, parentID, id)
	}
	delete(objects, id)
	return nil
}

// clearedStatus returns obj without its status, as a resource whose first
// status update failed has none.
func clearedStatus[O conditionedObject, A any, R any, L any](k crudResourceKind[O, A, R, L], obj O) O {
	cleared := k.newObject()
	cleared.SetName(obj.GetName())
	cleared.SetNamespace(obj.GetNamespace())
	cleared.SetResourceVersion(obj.GetResourceVersion())
	return cleared
}

// crudKindReconciler is a generated kind's reconciler.
type crudKindReconciler[O conditionedObject, A any, R any, L any] interface {
	reconcile.Reconciler
	SetupWithManager(mgr ctrl.Manager) error
	crud() *crudResourceReconciler[O, A, R, L]
}

// crudSpecEdit is an edit of a generated kind's sample spec: of field, as
// what describes it. setup, when set, prepares the resource the edit starts
// from, before it is created.
type crudSpecEdit[O any] struct {
	field, what string
	setup, edit func(O)
}

// crudScopeEdit is an edit of a field that names a resource's object in every
// call, besides the account and the ID, and the fake's scope for the address
// it leads to.
type crudScopeEdit[O any] struct {
	crudSpecEdit[O]
	scope string
}

// crudRefSpec is a spec field of a generated kind that references a resource
// of another kind: field, referencing a kind, as the table describes it.
type crudRefSpec[O any] struct {
	field, kind string
	// name is the resource the sample references: "" when the sample leaves
	// the reference unset, which only an optional one can be.
	name     string
	optional bool
	// set points a resource's reference at the resource name, or unsets it
	// for "".
	set func(obj O, name string)
}

// crudReferencedKinds builds, by kind, a valid resource at key that generated
// kinds reference, using the CloudflareAccount account, and returns it with
// the value a reference to it gives. That value differs from its name, as a
// Worker's name may differ from its WorkerScript's.
var crudReferencedKinds = map[string]func(key types.NamespacedName, account string) (conditionedObject, string){
	"WorkerScript": func(key types.NamespacedName, account string) (conditionedObject, string) {
		ws := &cloudflarev1alpha1.WorkerScript{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec: cloudflarev1alpha1.WorkerScriptSpec{
				AccountRef: corev1.LocalObjectReference{Name: account},
				WorkerScriptSpecExtras: cloudflarev1alpha1.WorkerScriptSpecExtras{
					Name:   "worker-" + key.Name,
					Script: new("export default { fetch() { return new Response('ok') } }"),
				},
			},
		}
		return ws, ws.Spec.Name
	},
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
	// claimLabel and conflictReason are the entry's, copies of the kind's
	// own.
	claimLabel, conflictReason string
	// newReconciler returns the kind's reconciler, with newAPI as its
	// factory: nil for the production one.
	newReconciler func(c client.Client, newAPI func(string) (A, error)) crudKindReconciler[O, A, R, L]
	// newFake returns the kind's fake service on store.
	newFake func(store *fakeCRUDStore[R]) A
	// defaultAPI is the production factory, and service an empty value of
	// the cloudflare-go v7 service it returns.
	defaultAPI func(token string) (A, error)
	service    any
	// newResource returns a resource at key, in parent, the CloudflareAccount
	// or, for a zone's kind, the Zone it names, with the sample's spec.
	newResource func(key types.NamespacedName, parent string) O
	// seed returns an object with id and key, as Cloudflare would hold it for
	// the sample, given what its references give (refs).
	seed func(id, key string, refs map[string]string) R
	// created reports whether an object holds every field of a resource's
	// spec that Cloudflare reports, those set at creation only included, and
	// what its references give.
	created func(obj O, refs map[string]string, current *R) bool
	// sent reports whether New's params hold every field of a resource's spec,
	// those the object does not report included, what its references give
	// and the fixed values; observed whether status records the object;
	// defaulted, nil for a spec without defaults, whether the API server
	// applied them.
	sent      func(obj O, refs map[string]string, params any) bool
	observed  func(obj O, current *R) bool
	defaulted func(obj O) bool
	// changes edit each field Update takes, immutable each field it does not
	// (accountRef or zoneRef first), and invalid set values the CRD refuses.
	// elsewhere moves the sample to another address: nil for a kind whose
	// objects the parent and the ID name alone.
	changes, immutable, invalid []crudSpecEdit[O]
	elsewhere                   *crudScopeEdit[O]
	// references are the spec's references to resources of other kinds.
	references []crudRefSpec[O]
	// deletionBlockers are the entry's, a copy of the kind's own.
	deletionBlockers []deletionBlocker
}

// crudSpecNamespace is where the shared specs create resources, and
// crudSpecFullPage the most objects they seed to fill a page of a list.
const (
	crudSpecNamespace = "default"
	crudSpecFullPage  = 1000
)

// crudSpecEnv is what the groups of a generated kind's shared specs share: its
// table, the names its resources and their parents use, the fake store and the
// reconciler of the running spec, what the sample's references give, and the
// helpers that set resources up and reconcile them. describeCRUDKind builds
// it.
type crudSpecEnv[O conditionedObject, A any, R any, L any] struct {
	t   crudKindSpecs[O, A, R, L]
	ctx context.Context
	// key and otherKey name the resources, after base. A resource names
	// parentName: the CloudflareAccount accountName, or, for a zone's kind
	// (zoned), the Zone zoneName of the zone zoneID. otherAccountName and
	// otherZoneName are second ones, for claims made through another one.
	base                               string
	key, otherKey                      types.NamespacedName
	accountName, secretName, accountID string
	zoneName, zoneID                   string
	otherAccountName, otherZoneName    string
	zoned                              bool
	// parentName is what a resource names, parentID the ID its calls send
	// and parentField the spec field. otherParent is the second parent,
	// parentKind its kind, and cloudflareParent what it is in Cloudflare.
	parentName, parentID, parentField         string
	otherParent, parentKind, cloudflareParent string

	store *fakeCRUDStore[R]
	r     crudKindReconciler[O, A, R, L]
	// refs holds what the sample's references give, by their field, once
	// createParent has created the resources they reference.
	refs map[string]string

	// describeCRUDKind's helpers: create creates a resource that carries the
	// finalizer, get reads it and ready its Ready condition, reconcileIt
	// reconciles it, and synced creates it and the object it manages.
	create      func(at types.NamespacedName, annotations map[string]string) O
	get         func() O
	ready       func() metav1.Condition
	reconcileIt func() (ctrl.Result, error)
	synced      func(annotations map[string]string) string
}

// newCRUDSpecEnv names the resources of t's shared specs.
func newCRUDSpecEnv[O conditionedObject, A any, R any, L any](t crudKindSpecs[O, A, R, L]) *crudSpecEnv[O, A, R, L] {
	base := "gen-" + strings.ToLower(t.kind.name)
	e := &crudSpecEnv[O, A, R, L]{
		t: t, ctx: context.Background(), base: base,
		key:         types.NamespacedName{Name: base, Namespace: crudSpecNamespace},
		otherKey:    types.NamespacedName{Name: base + "-other", Namespace: crudSpecNamespace},
		accountName: base + "-account", secretName: base + "-token", accountID: "acct-" + base,
		zoneName: base + "-zone", zoneID: "zone-" + base,
		otherAccountName: base + "-other-account", otherZoneName: base + "-other-zone",
		zoned: t.kind.zoneRef != nil,
	}
	e.parentName, e.parentID, e.parentField = e.accountName, e.accountID, "accountRef"
	e.otherParent, e.parentKind, e.cloudflareParent = e.otherAccountName, "CloudflareAccount", "Cloudflare account"
	if e.zoned {
		e.parentName, e.parentID, e.parentField = e.zoneName, e.zoneID, "zoneRef"
		e.otherParent, e.parentKind, e.cloudflareParent = e.otherZoneName, "Zone", "Cloudflare zone"
	}
	return e
}

// reset gives a spec a new fake store and a reconciler that calls it, and
// forgets what the sample's references gave.
func (e *crudSpecEnv[O, A, R, L]) reset() {
	e.store = newFakeCRUDStore[R](e.t.pageSize, e.t.scope)
	e.r = e.t.newReconciler(k8sClient, func(string) (A, error) { return e.t.newFake(e.store), nil })
	e.refs = map[string]string{}
	for _, ref := range e.t.references {
		e.refs[ref.field] = ""
	}
}

// matches reports whether current is what obj's spec asks for, as the kind
// compares them: by its hook, if it has one.
func (e *crudSpecEnv[O, A, R, L]) matches(obj O, current *R) bool {
	if m := e.t.kind.hooks.matches; m != nil {
		return m(obj, current, e.refs)
	}
	return e.t.kind.matches(obj, current, e.refs)
}

// seed stores an object with key, as Cloudflare would hold it, under id, or
// under its key for a kind whose objects their key identifies, and returns
// the ID it is stored under.
func (e *crudSpecEnv[O, A, R, L]) seed(id, key string) string {
	if e.t.kind.list == nil && e.t.kind.listAll == nil {
		id = key
	}
	e.store.objects[id] = e.t.seed(id, key, e.refs)
	return id
}

// createAccount creates the CloudflareAccount, ready or not, and its token.
func (e *crudSpecEnv[O, A, R, L]) createAccount(ready bool) {
	account := &cloudflarev1alpha1.CloudflareAccount{
		ObjectMeta: metav1.ObjectMeta{Name: e.accountName},
		Spec: cloudflarev1alpha1.CloudflareAccountSpec{
			AccountID: e.accountID,
			TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: e.secretName, Namespace: crudSpecNamespace,
				Key: "CF_API_TOKEN"},
		},
	}
	Expect(k8sClient.Create(e.ctx, account)).To(Succeed())
	if ready {
		account.Status.Conditions = []metav1.Condition{{
			Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Validated", LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(e.ctx, account)).To(Succeed())
	}
	Expect(k8sClient.Create(e.ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: e.secretName, Namespace: crudSpecNamespace},
		Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
	})).To(Succeed())
}

// createZone creates, at at, a Zone of the zone id, which uses the
// CloudflareAccount, ready or not.
func (e *crudSpecEnv[O, A, R, L]) createZone(at types.NamespacedName, id string, ready bool) {
	zone := &cloudflarev1alpha1.Zone{
		ObjectMeta: metav1.ObjectMeta{Name: at.Name, Namespace: at.Namespace},
		Spec: cloudflarev1alpha1.ZoneSpec{
			AccountRef: corev1.LocalObjectReference{Name: e.accountName},
			ZoneSpecExtras: cloudflarev1alpha1.ZoneSpecExtras{
				Name: e.base + ".example.com",
			},
		},
	}
	Expect(k8sClient.Create(e.ctx, zone)).To(Succeed())
	if ready {
		zone.Status.CloudflareMetadata.ZoneID = id
		zone.Status.Conditions = []metav1.Condition{{
			Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Synced", LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(e.ctx, zone)).To(Succeed())
	}
}

// createReferenced creates a resource of the kind ref refers to, named name,
// which uses the CloudflareAccount account, ready or not, and returns the
// value a reference to it gives. The spec deletes it again.
func (e *crudSpecEnv[O, A, R, L]) createReferenced(ref crudRefSpec[O], name, account string, ready bool) string {
	build, ok := crudReferencedKinds[ref.kind]
	Expect(ok).To(BeTrue(), "crudReferencedKinds has no %s", ref.kind)
	obj, value := build(types.NamespacedName{Name: name, Namespace: crudSpecNamespace}, account)
	Expect(k8sClient.Create(e.ctx, obj)).To(Succeed())
	if ready {
		obj.SetConditions([]metav1.Condition{{
			Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Synced", LastTransitionTime: metav1.Now(),
		}})
		Expect(k8sClient.Status().Update(e.ctx, obj)).To(Succeed())
	}
	DeferCleanup(forceDelete, e.ctx, client.Object(obj), types.NamespacedName{Name: name, Namespace: crudSpecNamespace})
	return value
}

// createParent creates what a resource needs before it calls Cloudflare: its
// CloudflareAccount, ready when accountReady, with its token; for a zone's
// kind, its Zone, ready; and the resources the sample references, ready,
// whose values it records in refs.
func (e *crudSpecEnv[O, A, R, L]) createParent(accountReady bool) {
	e.createAccount(accountReady)
	if e.zoned {
		e.createZone(types.NamespacedName{Name: e.zoneName, Namespace: crudSpecNamespace}, e.zoneID, true)
	}
	for _, ref := range e.t.references {
		if ref.name != "" {
			e.refs[ref.field] = e.createReferenced(ref, ref.name, e.accountName, true)
		}
	}
}

// createOtherParent creates a second CloudflareAccount, or Zone for a zone's
// kind, for the same Cloudflare account or zone as the resource's (same), or
// for another one.
func (e *crudSpecEnv[O, A, R, L]) createOtherParent(same bool) {
	if e.zoned {
		id := "zone-elsewhere"
		if same {
			id = e.zoneID
		}
		e.createZone(types.NamespacedName{Name: e.otherZoneName, Namespace: crudSpecNamespace}, id, true)
		return
	}
	id := "acct-elsewhere"
	if same {
		id = e.accountID
	}
	Expect(k8sClient.Create(e.ctx, &cloudflarev1alpha1.CloudflareAccount{
		ObjectMeta: metav1.ObjectMeta{Name: e.otherAccountName},
		Spec: cloudflarev1alpha1.CloudflareAccountSpec{
			AccountID: id,
			TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: e.secretName, Namespace: crudSpecNamespace,
				Key: "CF_API_TOKEN"},
		},
	})).To(Succeed())
}

// reconcileStale reconciles the resource with a reconciler whose cache gives
// stale for it, as a manager's cache can before it has seen what the last
// reconcile wrote, and whose API reader is apiReader: the API server itself
// when nil.
func (e *crudSpecEnv[O, A, R, L]) reconcileStale(stale O, apiReader client.Reader) (ctrl.Result, error) {
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
	Expect(err).NotTo(HaveOccurred())
	cache := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if key == e.key {
				reflect.ValueOf(obj).Elem().Set(reflect.ValueOf(stale.DeepCopyObject()).Elem())
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r := e.t.newReconciler(cache, func(string) (A, error) { return e.t.newFake(e.store), nil }).crud()
	r.apiReader = k8sClient
	if apiReader != nil {
		r.apiReader = apiReader
	}
	return r.reconcile(e.ctx, reconcile.Request{NamespacedName: e.key})
}

// failGets gives the spec a reconciler whose reads of an object named name
// fail with err.
func (e *crudSpecEnv[O, A, R, L]) failGets(name string, err error) {
	base, clientErr := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
	Expect(clientErr).NotTo(HaveOccurred())
	failing := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if key.Name == name {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	e.r = e.t.newReconciler(failing, func(string) (A, error) { return e.t.newFake(e.store), nil })
}

// runManager runs the kind's reconciler in a manager, on the spec's fake
// store, until the spec ends, so that its watches reconcile the resource.
func (e *crudSpecEnv[O, A, R, L]) runManager() {
	mgr, err := manager.New(cfg, manager.Options{
		Controller: config.Controller{SkipNameValidation: new(true)},
		Metrics:    metricsserver.Options{BindAddress: "0"},
	})
	Expect(err).NotTo(HaveOccurred())
	store := e.store
	Expect(e.t.newReconciler(mgr.GetClient(), func(string) (A, error) { return e.t.newFake(store), nil }).
		SetupWithManager(mgr)).To(Succeed())
	ctx, stop := context.WithCancel(e.ctx)
	stopped := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(stopped)
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
	// Stop it before the next spec, which reconciles the kind itself.
	DeferCleanup(func() {
		stop()
		Eventually(stopped).Should(BeClosed())
	})
}

// eventuallyReady waits until the resource reports Ready with reason.
func (e *crudSpecEnv[O, A, R, L]) eventuallyReady(reason string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		obj := e.t.kind.newObject()
		g.Expect(k8sClient.Get(e.ctx, e.key, obj)).To(Succeed())
		cond := meta.FindStatusCondition(obj.GetConditions(), cloudflarev1alpha1.ConditionReady)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Reason).To(Equal(reason), cond.Message)
	}).WithTimeout(10 * time.Second).Should(Succeed())
}

// describeCRUDZone runs the specs of a zone's kind: its resources wait for
// their Zone, delete their objects from the zone their status records, and
// leave them when the Zone is gone.
func describeCRUDZone[O conditionedObject, A any, R any, L any](e *crudSpecEnv[O, A, R, L]) {
	k, ctx, ns := e.t.kind, e.ctx, crudSpecNamespace
	key, zoneName, zoneID := e.key, e.zoneName, e.zoneID
	startDeletion := func() {
		Expect(k8sClient.Delete(ctx, e.get())).To(Succeed())
	}
	expectGone := func() {
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, k.newObject()))).To(BeTrue())
	}

	Describe("its Zone", func() {
		It("is reconciled once its Zone is ready, which it watches", func() {
			e.createAccount(true)
			for _, ref := range e.t.references {
				if ref.name != "" {
					e.createReferenced(ref, ref.name, e.accountName, true)
				}
			}
			e.create(key, nil)
			e.runManager()
			e.eventuallyReady("ZoneNotFound")
			e.createZone(types.NamespacedName{Name: zoneName, Namespace: ns}, zoneID, true)
			e.eventuallyReady("Synced")
		})

		It("adopts a "+k.noun+" that a resource in another namespace claims through a Zone of the same name for "+
			"another zone", func() {
			e.createParent(true)
			elsewhere := e.base + "-elsewhere"
			err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: elsewhere}})
			Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue(), "%v", err)
			e.createZone(types.NamespacedName{Name: zoneName, Namespace: elsewhere}, "zone-elsewhere", true)
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.Zone{},
				types.NamespacedName{Name: zoneName, Namespace: elsewhere})
			claimant := types.NamespacedName{Name: e.base, Namespace: elsewhere}
			other := e.t.newResource(claimant, zoneName)
			Expect(k8sClient.Create(ctx, other)).To(Succeed())
			DeferCleanup(forceDelete, ctx, k.newObject(), claimant)
			other.SetLabels(map[string]string{k.claimLabel: e.seed("taken", k.key(other))})
			Expect(k8sClient.Update(ctx, other)).To(Succeed())
			e.create(key, map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt})
			_, err = e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ready().Status).To(Equal(metav1.ConditionTrue), e.ready().Message)
			Expect(e.get().GetAnnotations()).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))
			Expect(e.store.writes).To(BeZero())
		})

		It("waits for its Zone without calling Cloudflare", func() {
			e.createAccount(true)
			e.create(key, nil)
			result, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue(), "the Zone watch reconciles it once the Zone is ready")
			Expect(e.ready().Reason).To(Equal("ZoneNotFound"))
			Expect(e.ready().Message).To(ContainSubstring(zoneName))

			By("waiting while the Zone is not ready")
			e.createZone(types.NamespacedName{Name: zoneName, Namespace: ns}, zoneID, false)
			_, err = e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ready().Reason).To(Equal("ZoneNotReady"))
			Expect(e.store.parentIDs).To(BeEmpty())
		})

		It("skips Cloudflare when its Zone is already gone", func() {
			id := e.synced(nil)
			forceDelete(ctx, &cloudflarev1alpha1.Zone{}, types.NamespacedName{Name: zoneName, Namespace: ns})
			calls := len(e.store.parentIDs)
			startDeletion()
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.store.objects).To(HaveKey(id), "there are no credentials to delete it with")
			Expect(e.store.parentIDs).To(HaveLen(calls))
			expectGone()
		})

		It("deletes the "+k.noun+" from the zone its status records", func() {
			e.synced(nil)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: zoneName, Namespace: ns}, zone)).To(Succeed())
			zone.Status.CloudflareMetadata.ZoneID = "zone-moved"
			Expect(k8sClient.Status().Update(ctx, zone)).To(Succeed())
			startDeletion()
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.store.parentIDs[len(e.store.parentIDs)-1]).To(Equal(zoneID))
			Expect(e.store.objects).To(BeEmpty())
			expectGone()
		})

		It("keeps the finalizer when its Zone cannot be read", func() {
			e.synced(nil)
			e.failGets(zoneName, errors.New("zone unavailable"))
			startDeletion()
			_, err := e.reconcileIt()
			Expect(err).To(MatchError("zone unavailable"))
			Expect(e.get().GetFinalizers()).To(ContainElement(reconciler.Finalizer))
			Expect(e.store.objects).To(HaveLen(1))
		})

		It("maps a Zone to the resources in its zone", func() {
			e.create(key, nil)
			zone := func(name, namespace string) client.Object {
				return &cloudflarev1alpha1.Zone{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
			}
			Expect(e.r.crud().resourcesForZone(ctx, zone(zoneName, ns))).To(
				ConsistOf(reconcile.Request{NamespacedName: key}))
			Expect(e.r.crud().resourcesForZone(ctx, zone(zoneName+"-other", ns))).To(BeEmpty())
			Expect(e.r.crud().resourcesForZone(ctx, zone(zoneName, "kube-system"))).To(BeEmpty())

			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			Expect(e.r.crud().resourcesForZone(cancelled, zone(zoneName, ns))).To(BeNil(), "the list fails")
		})

		It("counts as a user of its Zone, which names the CloudflareAccount", func() {
			e.create(key, nil)
			deps, err := dependentsOf(ctx, k8sClient, ns, zoneName, zoneDependents)
			Expect(err).NotTo(HaveOccurred())
			Expect(deps).To(ConsistOf(k.name + " " + key.String()))
			Expect(slices.ContainsFunc(accountDependents, func(d dependentKind) bool { return d.kind == k.name })).
				To(BeFalse(), "it reaches its account through its Zone")
		})
	})
}

// describeCRUDReferences runs the specs of a kind's references to resources of
// other kinds: a resource waits for each, sends what it gives, follows it to
// another resource or, unset, sends nothing.
func describeCRUDReferences[O conditionedObject, A any, R any, L any](e *crudSpecEnv[O, A, R, L]) {
	t, k, ctx, ns := e.t, e.t.kind, e.ctx, crudSpecNamespace
	key, base, accountName, otherAccountName, parentName := e.key, e.base, e.accountName, e.otherAccountName, e.parentName

	Describe("its references", func() {
		It("references what its entry says", func() {
			kinds, entry := make([]string, 0, len(k.references)), make([]string, 0, len(t.references))
			for _, ref := range k.references {
				kinds = append(kinds, ref.field+" "+ref.kind)
			}
			for _, ref := range t.references {
				entry = append(entry, ref.field+" "+ref.kind)
			}
			Expect(kinds).To(Equal(entry))
		})

		for i, ref := range t.references {
			// target is a resource of the kind ref refers to, other than the
			// sample's.
			target := base + "-" + strings.ToLower(ref.kind)

			// createReferencing creates the resource with ref naming target.
			createReferencing := func() {
				obj := t.newResource(key, parentName)
				obj.SetFinalizers([]string{reconciler.Finalizer})
				ref.set(obj, target)
				Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			}

			It("waits for the "+ref.kind+" its "+ref.field+" names, without calling Cloudflare", func() {
				e.createParent(true)
				createReferencing()
				result, err := e.reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(result.IsZero()).To(BeTrue(), "the "+ref.kind+" watch reconciles it once it is ready")
				Expect(e.ready().Reason).To(Equal(ref.kind + "NotFound"))
				Expect(e.ready().Message).To(ContainSubstring(target))

				By("waiting while the " + ref.kind + " is not ready")
				e.createReferenced(ref, target, accountName, false)
				_, err = e.reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.ready().Reason).To(Equal(ref.kind + "NotReady"))
				Expect(e.store.parentIDs).To(BeEmpty())
			})

			It("refuses a "+ref.kind+" that uses another CloudflareAccount", func() {
				e.createParent(true)
				e.createReferenced(ref, target, otherAccountName, true)
				createReferencing()
				_, err := e.reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.ready().Reason).To(Equal(reasonAccountMismatch))
				Expect(e.ready().Message).To(ContainSubstring(otherAccountName))
				Expect(e.ready().Message).To(ContainSubstring(accountName))
				Expect(e.store.parentIDs).To(BeEmpty())
			})

			It("points the "+k.noun+" at another "+ref.kind+" in place", func() {
				id := e.synced(nil)
				writes := e.store.writes
				changed := maps.Clone(e.refs)
				changed[ref.field] = e.createReferenced(ref, target, accountName, true)
				obj := e.get()
				ref.set(obj, target)
				Expect(k8sClient.Update(ctx, obj)).To(Succeed())
				_, err := e.reconcileIt()
				Expect(err).NotTo(HaveOccurred())

				object := e.store.objects[id]
				Expect(k.statusID(e.get())).To(Equal(id))
				Expect(t.created(e.get(), changed, &object)).To(BeTrue(), "the object holds what the new "+ref.kind+
					" gives")
				Expect(t.observed(e.get(), &object)).To(BeTrue(), "status records it")
				Expect(e.store.writes).To(Equal(writes + 1))
			})

			if ref.optional {
				It("creates the "+k.noun+" without what its "+ref.field+" gives while it is unset", func() {
					e.createParent(true)
					obj := t.newResource(key, parentName)
					obj.SetFinalizers([]string{reconciler.Finalizer})
					ref.set(obj, "")
					Expect(k8sClient.Create(ctx, obj)).To(Succeed())
					_, err := e.reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					unset := maps.Clone(e.refs)
					unset[ref.field] = ""
					object := e.store.objects[k.statusID(e.get())]
					Expect(t.sent(e.get(), unset, e.store.sent[0])).To(BeTrue(), "New is sent nothing for it")
					Expect(t.created(e.get(), unset, &object)).To(BeTrue())
					Expect(e.store.writes).To(Equal(1))
					Expect(e.ready().Status).To(Equal(metav1.ConditionTrue))
				})

				It("clears what its "+ref.field+" gave once it is unset", func() {
					e.createParent(true)
					given := maps.Clone(e.refs)
					given[ref.field] = e.createReferenced(ref, target, accountName, true)
					createReferencing()
					_, err := e.reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					id := k.statusID(e.get())
					object := e.store.objects[id]
					Expect(t.created(e.get(), given, &object)).To(BeTrue())
					Expect(t.sent(e.get(), given, e.store.sent[0])).To(BeTrue(), "New is sent what the "+ref.kind+
						" gives")

					writes := e.store.writes
					obj := e.get()
					ref.set(obj, "")
					Expect(k8sClient.Update(ctx, obj)).To(Succeed())
					_, err = e.reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					unset := maps.Clone(e.refs)
					unset[ref.field] = ""
					object = e.store.objects[id]
					Expect(t.created(e.get(), unset, &object)).To(BeTrue(), "the object holds nothing it gave")
					Expect(e.store.writes).To(Equal(writes + 1))
					Expect(e.ready().Status).To(Equal(metav1.ConditionTrue))
				})
			}

			It("is reconciled once the "+ref.kind+" its "+ref.field+" names is ready, which it watches", func() {
				e.createParent(true)
				createReferencing()
				e.runManager()
				e.eventuallyReady(ref.kind + "NotFound")
				e.createReferenced(ref, target, accountName, true)
				e.eventuallyReady("Synced")
			})

			It("maps a "+ref.kind+" to the resources whose "+ref.field+" names it", func() {
				createReferencing()
				mapper := e.r.crud().resourcesReferencing(k.references[i])
				named := func(name, namespace string) client.Object {
					obj, _ := crudReferencedKinds[ref.kind](types.NamespacedName{Name: name, Namespace: namespace},
						accountName)
					return obj
				}
				Expect(mapper(ctx, named(target, ns))).To(ConsistOf(reconcile.Request{NamespacedName: key}))
				Expect(mapper(ctx, named(target+"-other", ns))).To(BeEmpty())
				Expect(mapper(ctx, named(target, "kube-system"))).To(BeEmpty())

				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				Expect(mapper(cancelled, named(target, ns))).To(BeNil(), "the list fails")
			})
		}

	})
}

// describeCRUDKind runs the shared specs on a generated kind's table.
func describeCRUDKind[O conditionedObject, A any, R any, L any](t crudKindSpecs[O, A, R, L]) bool {
	return Describe(t.kind.name+" (generated specs)", func() {
		k := t.kind
		e := newCRUDSpecEnv(t)
		ctx, ns := e.ctx, crudSpecNamespace
		key, otherKey, accountName := e.key, e.otherKey, e.accountName
		zoneName, otherAccountName, otherZoneName := e.zoneName, e.otherAccountName, e.otherZoneName
		// byKey is set for a kind whose objects their key identifies: it reads
		// them by their key and never lists them. singlePage is set for one
		// whose list is never paged.
		byKey := k.list == nil && k.listAll == nil
		singlePage := k.listAll != nil
		// zoned is set for a zone's kind. Its resources name a Zone, which
		// names the CloudflareAccount; parentName is what a resource names,
		// parentID the ID its calls send, and parentField the spec field.
		zoned, parentName, parentID, parentField := e.zoned, e.parentName, e.parentID, e.parentField
		// addressField is the field that names an object besides the parent
		// and the ID, if the kind has one.
		addressField := ""
		if t.elsewhere != nil {
			addressField = t.elsewhere.field
		}

		// create creates a resource that already carries the finalizer.
		create := func(at types.NamespacedName, annotations map[string]string) O {
			obj := t.newResource(at, parentName)
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
			return e.r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		}

		// adopt and adoptOrCreate are the adoption policies' annotations.
		adopt := map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt}
		adoptOrCreate := map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdoptOrCreate}

		seed := e.seed

		// findErr is the error of the call that finds the object a resource
		// would adopt: the list's, or the read by its key.
		findErr := func() *error {
			if byKey {
				return &e.store.getErr
			}
			return &e.store.listErr
		}

		// synced creates a resource and the object it manages.
		synced := func(annotations map[string]string) string {
			e.createParent(true)
			create(key, annotations)
			_, err := reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.store.objects).To(HaveLen(1))
			return k.statusID(get())
		}
		e.create, e.get, e.ready, e.reconcileIt, e.synced = create, get, ready, reconcileIt, synced

		BeforeEach(e.reset)

		AfterEach(func() {
			forceDelete(ctx, k.newObject(), key)
			forceDelete(ctx, k.newObject(), otherKey)
			forceDelete(ctx, &cloudflarev1alpha1.Zone{}, types.NamespacedName{Name: zoneName, Namespace: ns})
			forceDelete(ctx, &cloudflarev1alpha1.Zone{}, types.NamespacedName{Name: otherZoneName, Namespace: ns})
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: otherAccountName})
			forceDelete(ctx, &corev1.Secret{}, types.NamespacedName{Name: e.secretName, Namespace: ns})
			// envtest runs no garbage collector, so what a kind's hooks made for
			// a resource outlives it.
			secrets := &corev1.SecretList{}
			Expect(k8sClient.List(ctx, secrets, client.InNamespace(ns))).To(Succeed())
			for i := range secrets.Items {
				if len(secrets.Items[i].OwnerReferences) > 0 {
					Expect(k8sClient.Delete(ctx, &secrets.Items[i])).To(Succeed())
				}
			}
		})

		if zoned {
			describeCRUDZone(e)
		}
		describeCRUDReferences(e)

		Describe("Reconcile", func() {
			It("returns no error when the resource does not exist", func() {
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
			})

			It("adds the finalizer on the first reconcile without calling Cloudflare", func() {
				Expect(k8sClient.Create(ctx, t.newResource(key, parentName))).To(Succeed())
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(get().GetFinalizers()).To(ContainElement(reconciler.Finalizer))
				Expect(e.store.parentIDs).To(BeEmpty())
			})

			It("retries after a minute when the account is not ready", func() {
				e.createParent(false)
				create(key, nil)
				result, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
				Expect(ready().Reason).To(Equal("AccountNotReady"))
			})

			It("reports InvalidToken when no Cloudflare client can be built", func() {
				e.createParent(true)
				create(key, nil)
				e.r = t.newReconciler(k8sClient, func(string) (A, error) { return *new(A), errors.New("bad token") })
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(ready().Reason).To(Equal("InvalidToken"))
			})

			It("creates the "+k.noun+", claims it and reports its ID", func() {
				id := synced(nil)
				Expect(e.store.objects).To(HaveKey(id))
				Expect(e.store.parentIDs).To(HaveEach(parentID))
				if byKey {
					Expect(e.store.listPages).To(BeEmpty(), "the "+k.noun+" is read by its "+t.keyField+", never listed")
				} else {
					Expect(e.store.listPages).To(Equal([]int64{1}), "one page holds every object of the account")
				}
				Expect(e.store.writes).To(Equal(1), "the new object is what the spec asks for: nothing to update")
				obj := get()
				object := e.store.objects[id]
				Expect(t.sent(obj, e.refs, e.store.sent[0])).To(BeTrue(), "New is sent every field of the spec")
				Expect(t.created(obj, e.refs, &object)).To(BeTrue(), "the new object holds every field of the spec")
				Expect(t.observed(obj, &object)).To(BeTrue(), "status records the new object")
				if zoned {
					Expect(*k.statusZoneID(obj)).To(Equal(e.zoneID), "status records the zone")
				}
				Expect(e.matches(obj, &object)).To(BeTrue(), "the new object is what the spec asks for")
				Expect(obj.GetLabels()).To(HaveKeyWithValue(k.claimLabel, id))
				Expect(ready().Status).To(Equal(metav1.ConditionTrue))
			})

			It("creates the "+k.noun+" with every field the spec sets", func() {
				e.createParent(true)
				obj := t.newResource(key, parentName)
				obj.SetFinalizers([]string{reconciler.Finalizer})
				// The address stays the sample's, where the store's objects are:
				// the elsewhere specs move it.
				for _, edit := range slices.Concat(t.changes, t.immutable) {
					if edit.field != parentField && edit.field != addressField {
						edit.edit(obj)
					}
				}
				Expect(k8sClient.Create(ctx, obj)).To(Succeed())
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.store.writes).To(Equal(1))
				object := e.store.objects[k.statusID(get())]
				Expect(t.sent(get(), e.refs, e.store.sent[0])).To(BeTrue(), "New is sent every field of the spec")
				Expect(t.created(get(), e.refs, &object)).To(BeTrue(), "the new object holds every field of the spec")
			})

			It("claims with its entry's label, and reports its entry's conflict reason", func() {
				Expect(k.claimLabel).To(Equal(t.claimLabel))
				Expect(k.conflictReason).To(Equal(t.conflictReason))
			})

			if t.defaulted != nil {
				It("takes the API's defaults for the fields its sample leaves unset", func() {
					create(key, nil)
					Expect(t.defaulted(get())).To(BeTrue())
				})
			}

			It("writes nothing to Cloudflare once the "+k.noun+" is in sync", func() {
				synced(nil)
				writes, pages := e.store.writes, len(e.store.listPages)
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.store.writes).To(Equal(writes))
				Expect(e.store.listPages).To(HaveLen(pages), "the object is read by its ID, not found in the list")
			})

			It("schedules a drift check after a sync, once the manager sets an interval", func() {
				synced(nil)
				writes := e.store.writes
				expectDriftCheck(reconcileIt)
				Expect(e.store.writes).To(Equal(writes))
			})

			if byKey {
				It("adopts the "+k.noun+" with the same "+t.keyField+" by reading it, without listing", func() {
					e.createParent(true)
					obj := create(key, adopt)
					id := seed("", k.key(obj))
					_, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(e.store.listPages).To(BeEmpty())
					Expect(k.statusID(get())).To(Equal(id))
					Expect(get().GetLabels()).To(HaveKeyWithValue(k.claimLabel, id))
					Expect(get().GetAnnotations()).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))
					Expect(e.store.writes).To(BeZero(), "the adopted object is what the spec asks for")
				})
			} else if singlePage {
				It("adopts the "+k.noun+" with the same "+t.keyField+" from its list, which is never paged", func() {
					e.createParent(true)
					obj := create(key, adopt)
					for i := range 3 {
						seed(fmt.Sprintf("a-%d", i), fmt.Sprintf("other-%d", i))
					}
					seed("z-last", k.key(obj))
					_, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(e.store.listPages).To(Equal([]int64{1}))
					Expect(e.store.objects).To(HaveLen(4), "nothing is created")
					Expect(k.statusID(get())).To(Equal("z-last"))
					Expect(get().GetLabels()).To(HaveKeyWithValue(k.claimLabel, "z-last"))
					Expect(get().GetAnnotations()).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))
					Expect(e.store.writes).To(BeZero(), "the adopted object is what the spec asks for")
				})
			} else {
				It("adopts the "+k.noun+" with the same "+t.keyField+" from a later page of the list", func() {
					e.createParent(true)
					obj := create(key, adopt)
					// A full first page of other objects; the one to adopt sorts last.
					// A page of a list that takes a very large per_page holds fewer,
					// as Cloudflare reports.
					e.store.pageCap = min(t.pageSize, crudSpecFullPage)
					for i := range e.store.pageCap {
						seed(fmt.Sprintf("a-%04d", i), fmt.Sprintf("other-%04d", i))
					}
					seed("z-last", k.key(obj))
					_, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(e.store.listPages[:2]).To(Equal([]int64{1, 2}))
					Expect(e.store.objects).To(HaveLen(int(e.store.pageCap)+1), "nothing is created")
					Expect(k.statusID(get())).To(Equal("z-last"))
					Expect(get().GetLabels()).To(HaveKeyWithValue(k.claimLabel, "z-last"))
					Expect(get().GetAnnotations()).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))
					Expect(e.store.writes).To(BeZero(), "the adopted object is what the spec asks for")
				})
			}

			It("reports AlreadyExists for a "+k.noun+" with the same "+t.keyField+" without an adoption policy, "+
				"and leaves it alone", func() {
				e.createParent(true)
				obj := create(key, nil)
				seed("existing", k.key(obj))
				result, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
				Expect(ready().Reason).To(Equal(reasonAlreadyExists))
				Expect(ready().Message).To(ContainSubstring(k.key(obj)))
				Expect(ready().Message).To(ContainSubstring(reconciler.AdoptionPolicyAnnotation))
				Expect(e.store.writes).To(BeZero())
				Expect(e.store.objects).To(HaveLen(1))
				Expect(k.statusID(get())).To(BeEmpty())
				Expect(get().GetLabels()).NotTo(HaveKey(k.claimLabel))
			})

			It("reports NotFound with the adopt policy when there is no "+k.noun+" to adopt, and creates none",
				func() {
					e.createParent(true)
					create(key, adopt)
					result, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
					Expect(ready().Reason).To(Equal(reasonNotFound))
					Expect(e.store.writes).To(BeZero())
					Expect(e.store.objects).To(BeEmpty())
				})

			It("reports an unknown adoption policy instead of creating or adopting", func() {
				e.createParent(true)
				obj := create(key, map[string]string{reconciler.AdoptionPolicyAnnotation: "Adopt"})
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(ready().Reason).To(Equal(reasonInvalidAdoptionPolicy))
				Expect(e.store.objects).To(BeEmpty(), "nothing is created")

				seed("existing", k.key(obj))
				_, err = reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(ready().Reason).To(Equal(reasonInvalidAdoptionPolicy))
				Expect(e.store.writes).To(BeZero())
				Expect(get().GetLabels()).NotTo(HaveKey(k.claimLabel), "nothing is adopted")
			})

			It("creates the "+k.noun+" with the adopt-or-create policy when there is none to adopt", func() {
				e.createParent(true)
				create(key, adoptOrCreate)
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.store.objects).To(HaveLen(1))
				Expect(get().GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation))
				Expect(ready().Status).To(Equal(metav1.ConditionTrue))
			})

			It("marks the "+k.noun+" it is about to create, so a failed create leaves the mark", func() {
				e.createParent(true)
				obj := create(key, nil)
				e.store.createErr = errors.New("unavailable")
				_, err := reconcileIt()
				Expect(err).To(MatchError("unavailable"))
				Expect(get().GetAnnotations()).To(HaveKeyWithValue(reconciler.CreatingAnnotation, k.key(obj)))

				By("creating it on the next reconcile, which drops the mark with the claim")
				e.store.createErr = nil
				_, err = reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(get().GetAnnotations()).NotTo(HaveKey(reconciler.CreatingAnnotation))
				Expect(get().GetLabels()).To(HaveKeyWithValue(k.claimLabel, k.statusID(get())))
			})

			It("finds the "+k.noun+" it was creating when neither its claim nor its status was recorded", func() {
				e.createParent(true)
				obj := t.newResource(key, parentName)
				id := seed("created", k.key(obj))
				create(key, map[string]string{reconciler.CreatingAnnotation: k.key(obj)})
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(ready().Status).To(Equal(metav1.ConditionTrue))
				Expect(k.statusID(get())).To(Equal(id))
				Expect(e.store.writes).To(BeZero(), "nothing is created")
				Expect(get().GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation), "it is not adopted")
				Expect(get().GetAnnotations()).NotTo(HaveKey(reconciler.CreatingAnnotation))
			})

			It("finds the "+k.noun+" it claimed when its status was not recorded", func() {
				e.createParent(true)
				obj := create(key, nil)
				id := seed("claimed", k.key(obj))
				obj.SetLabels(map[string]string{k.claimLabel: id})
				Expect(k8sClient.Update(ctx, obj)).To(Succeed())
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(ready().Status).To(Equal(metav1.ConditionTrue))
				Expect(k.statusID(get())).To(Equal(id))
				Expect(e.store.writes).To(BeZero())
				Expect(get().GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation))
			})

			It("acts on no stale read of a resource whose status records no "+k.noun, func() {
				e.createParent(true)
				create(key, adoptOrCreate)
				stale := get()
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				version, calls, writes := get().GetResourceVersion(), len(e.store.parentIDs), e.store.writes

				By("stopping when the cache has not seen what the reconcile that created it wrote")
				result, err := e.reconcileStale(stale, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.IsZero()).To(BeTrue(), "the watch event that brings the cache up to date reconciles it")
				Expect(e.store.parentIDs).To(HaveLen(calls), "Cloudflare is not called")
				Expect(get().GetResourceVersion()).To(Equal(version), "the resource is left as it is")
				Expect(get().GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation),
					"the "+k.noun+" it created is not taken for one to adopt")

				By("reconciling as the API server has it once the cache has caught up")
				_, err = reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.store.writes).To(Equal(writes))
			})

			It("returns a failed read of the resource from the API server for back-off", func() {
				e.createParent(true)
				obj := create(key, nil)
				base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
				Expect(err).NotTo(HaveOccurred())
				apiServer := interceptor.NewClient(base, interceptor.Funcs{
					Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
						return errors.New("unavailable")
					},
				})
				_, err = e.reconcileStale(obj, apiServer)
				Expect(err).To(MatchError("unavailable"))
				Expect(e.store.parentIDs).To(BeEmpty(), "Cloudflare is not called")
			})

			It("stops when the resource is gone from the API server but not yet from the cache", func() {
				e.createParent(true)
				stale := create(key, nil)
				forceDelete(ctx, k.newObject(), key)
				result, err := e.reconcileStale(stale, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.IsZero()).To(BeTrue())
				Expect(e.store.parentIDs).To(BeEmpty(), "Cloudflare is not called")
			})

			if !byKey {
				It("creates the "+k.noun+" when the one it would adopt is gone before it is read", func() {
					e.createParent(true)
					obj := create(key, adoptOrCreate)
					seed("listed", k.key(obj))
					e.store.getErr = fakeCRUDNotFound(http.MethodGet, parentID, "listed")
					_, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					id := k.statusID(get())
					Expect(id).NotTo(Equal("listed"))
					Expect(e.store.objects).To(HaveKey(id))
					Expect(ready().Status).To(Equal(metav1.ConditionTrue))
					Expect(get().GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation), "it created this one")
				})
			}

			It("returns a failed read of the "+k.noun+" it would adopt for back-off", func() {
				e.createParent(true)
				obj := create(key, adopt)
				seed("listed", k.key(obj))
				e.store.getErr = errors.New("unavailable")
				_, err := reconcileIt()
				Expect(err).To(MatchError("unavailable"))
				Expect(e.store.objects).To(HaveLen(1), "nothing is created")
			})

			// claimedBy creates, at otherKey, a resource that names parent, a
			// CloudflareAccount or a Zone, and claims the object with the
			// sample's key, and stores that object.
			claimedBy := func(parent string) {
				other := t.newResource(otherKey, parent)
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
				Expect(e.store.writes).To(BeZero())
			}

			// expectAdopted reconciles the resource, and expects it to adopt the
			// object with the sample's key.
			expectAdopted := func() {
				GinkgoHelper()
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(ready().Status).To(Equal(metav1.ConditionTrue), ready().Message)
				Expect(get().GetAnnotations()).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))
				Expect(e.store.writes).To(BeZero())
			}

			// otherParent is a second CloudflareAccount, or Zone for a zone's
			// kind, which e.createOtherParent creates.
			otherParent, parentKind, cloudflareParent := e.otherParent, e.parentKind, e.cloudflareParent

			if !k.sharedObjects {
				It("reports "+k.conflictReason+" instead of taking over a "+k.noun+" another "+k.name+" manages", func() {
					e.createParent(true)
					claimedBy(parentName)
					create(key, nil)
					expectConflict()
				})

				It("reports "+k.conflictReason+" for a claim made through another "+parentKind+" for the same "+
					cloudflareParent, func() {
					e.createParent(true)
					e.createOtherParent(true)
					claimedBy(otherParent)
					create(key, adopt)
					expectConflict()
				})

				It("returns a failed read of a claimant's "+parentKind+" for back-off", func() {
					e.createParent(true)
					claimedBy(otherParent)
					create(key, adopt)
					e.failGets(otherParent, errors.New("parent unavailable"))
					_, err := reconcileIt()
					Expect(err).To(MatchError("parent unavailable"))
					Expect(e.store.writes).To(BeZero())
				})
			}

			It("adopts a "+k.noun+" whose ID a resource claims in another "+cloudflareParent, func() {
				e.createParent(true)
				e.createOtherParent(false)
				claimedBy(otherParent)
				create(key, adopt)
				expectAdopted()
			})

			It("adopts a "+k.noun+" that a resource whose "+parentKind+" is gone claims", func() {
				e.createParent(true)
				claimedBy(otherParent)
				create(key, adopt)
				expectAdopted()
			})

			It("adopts a "+k.noun+" though another "+k.name+" claims another", func() {
				e.createParent(true)
				other := t.newResource(otherKey, parentName)
				other.SetLabels(map[string]string{k.claimLabel: "another"})
				Expect(k8sClient.Create(ctx, other)).To(Succeed())
				obj := create(key, adopt)
				seed("existing", k.key(obj))
				expectAdopted()
			})

			if move := t.elsewhere; move != nil {
				// createElsewhere creates the resource with the sample's spec
				// moved to another address, and annotations.
				createElsewhere := func(annotations map[string]string) O {
					obj := t.newResource(key, parentName)
					obj.SetAnnotations(annotations)
					obj.SetFinalizers([]string{reconciler.Finalizer})
					move.edit(obj)
					Expect(k8sClient.Create(ctx, obj)).To(Succeed())
					return obj
				}

				It("adopts the "+k.noun+" with the same "+t.keyField+" after "+move.what+", though another "+k.name+
					" claims the one at the sample's", func() {
					e.createParent(true)
					claimedBy(parentName)
					obj := createElsewhere(adopt)
					id := k.key(obj)
					if !byKey {
						id = "elsewhere"
					}
					e.store.at(move.scope)[id] = t.seed(id, k.key(obj), e.refs)
					expectAdopted()
					Expect(k.statusID(get())).To(Equal(id))
					Expect(e.store.objects).To(HaveLen(1), "the claimed one is left alone")
				})

				It("creates its own "+k.noun+" after "+move.what+", rather than adopting the one at the sample's",
					func() {
						e.createParent(true)
						seed("existing", k.key(t.newResource(key, parentName)))
						createElsewhere(adoptOrCreate)
						_, err := reconcileIt()
						Expect(err).NotTo(HaveOccurred())
						Expect(ready().Status).To(Equal(metav1.ConditionTrue), ready().Message)
						Expect(get().GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation))
						Expect(e.store.at(move.scope)).To(HaveKey(k.statusID(get())))
						Expect(e.store.objects).To(HaveLen(1), "the one at the sample's is left alone")
					})
			}

			// An object identified by its key comes back under the same ID;
			// another kind's under a new one.
			expectRecreated := func(id string, writes int) {
				GinkgoHelper()
				newID := k.statusID(get())
				Expect(e.store.writes).To(Equal(writes+1), "one create")
				if !byKey {
					Expect(newID).NotTo(Equal(id))
				}
				Expect(e.store.objects).To(HaveKey(newID))
				Expect(get().GetLabels()).To(HaveKeyWithValue(k.claimLabel, newID))
			}

			It("recreates the "+k.noun+" when it was deleted outside kflare", func() {
				id := synced(nil)
				writes := e.store.writes
				delete(e.store.objects, id)
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				expectRecreated(id, writes)
			})

			It("recreates an adopted "+k.noun+" deleted outside kflare as its own with adopt-or-create", func() {
				e.createParent(true)
				obj := create(key, adoptOrCreate)
				id := seed("existing", k.key(obj))
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(get().GetAnnotations()).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))

				writes := e.store.writes
				delete(e.store.objects, id)
				_, err = reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				expectRecreated(id, writes)
				Expect(get().GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation), "kflare created this one")
			})

			It("reports NotFound with the adopt policy when an adopted "+k.noun+" is deleted outside kflare",
				func() {
					e.createParent(true)
					obj := create(key, adopt)
					id := seed("existing", k.key(obj))
					_, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())

					delete(e.store.objects, id)
					writes := e.store.writes
					_, err = reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(ready().Reason).To(Equal(reasonNotFound))
					Expect(e.store.writes).To(Equal(writes), "nothing is created")
				})

			for _, change := range t.changes {
				It("updates the "+k.noun+" in place after "+change.what, func() {
					id := synced(nil)
					writes := e.store.writes
					obj := get()
					change.edit(obj)
					Expect(k8sClient.Update(ctx, obj)).To(Succeed())
					_, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())

					obj = get()
					Expect(k.statusID(obj)).To(Equal(id))
					object := e.store.objects[id]
					Expect(e.matches(obj, &object)).To(BeTrue(), "the object is what the edited spec asks for")
					Expect(e.store.writes).To(Equal(writes + 1))
					Expect(ready().Status).To(Equal(metav1.ConditionTrue))
				})
			}

			if len(t.changes) > 0 {
				change := t.changes[0]
				It("sets TerminalError when Cloudflare refuses an update", func() {
					synced(nil)
					e.store.updateErr = v7APIError(http.MethodPut, "parents/"+parentID+"/objects", http.StatusBadRequest,
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

				It("updates nothing on a stale read of its spec", func() {
					id := synced(nil)
					stale := get()
					obj := get()
					change.edit(obj)
					Expect(k8sClient.Update(ctx, obj)).To(Succeed())
					_, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					updated, version, writes := e.store.objects[id], get().GetResourceVersion(), e.store.writes

					result, err := e.reconcileStale(stale, nil)
					Expect(err).NotTo(HaveOccurred())
					Expect(result.IsZero()).To(BeTrue(), "the watch event that brings the cache up to date reconciles it")
					Expect(e.store.writes).To(Equal(writes), "the "+k.noun+" is not taken back to the spec before "+
						change.what)
					Expect(e.store.objects[id]).To(Equal(updated))
					Expect(get().GetResourceVersion()).To(Equal(version), "the resource is left as it is")
				})

				It("returns a failed update for back-off", func() {
					id := synced(nil)
					before := e.store.objects[id]
					e.store.updateErr = errors.New("unavailable")
					obj := get()
					change.edit(obj)
					Expect(k8sClient.Update(ctx, obj)).To(Succeed())
					_, err := reconcileIt()
					Expect(err).To(MatchError("unavailable"))
					Expect(e.store.objects[id]).To(Equal(before))
				})
			}

			It("returns retryable errors for back-off", func() {
				e.createParent(true)
				create(key, nil)
				*findErr() = errors.New("connection reset")
				_, err := reconcileIt()
				Expect(err).To(MatchError("connection reset"))
				Expect(ready().Reason).To(Equal("APIError"))

				*findErr() = nil
				e.store.createErr = errors.New("unavailable")
				_, err = reconcileIt()
				Expect(err).To(MatchError("unavailable"))
			})

			It("records an Event when Ready changes, and none on a reconcile that changes nothing", func() {
				recorder := events.NewFakeRecorder(10)
				SetEventRecorder(recorder)
				DeferCleanup(func() { SetEventRecorder(nil) })
				e.createParent(true)
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
				e.store.getErr = v7APIError(http.MethodGet, "parents/"+parentID+"/objects/1",
					http.StatusServiceUnavailable, 0, "")
				_, err := reconcileIt()
				Expect(err).To(MatchError(e.store.getErr))
				Expect(ready().Reason).To(Equal("APIError"))
				Expect(ready().Message).To(Equal("Cloudflare API error: HTTP 503 Service Unavailable"))
			})

			It("keeps its error paths as they are with a drift check interval", func() {
				turnOnDriftChecks()
				synced(nil)

				By("returning a retryable error for back-off, with nothing scheduled")
				e.store.getErr = v7APIError(http.MethodGet, "parents/"+parentID+"/objects/1",
					http.StatusServiceUnavailable, 0, "")
				result, err := reconcileIt()
				Expect(err).To(MatchError(e.store.getErr))
				Expect(result).To(BeZero())
				Expect(ready().Reason).To(Equal("APIError"))

				By("waiting for a change after a terminal error")
				e.store.getErr = v7APIError(http.MethodGet, "parents/"+parentID+"/objects/1", http.StatusForbidden,
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
				Expect(e.store.objects).To(BeEmpty())
				expectGone()
			})

			It("deletes the "+k.noun+" its claim names when no status recorded it", func() {
				synced(nil)
				// A reconcile claims the object before it writes status, and may
				// fail in between.
				Expect(k8sClient.Status().Update(ctx, clearedStatus(k, get()))).To(Succeed())
				Expect(k.statusID(get())).To(BeEmpty())
				startDeletion()
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.store.objects).To(BeEmpty())
				expectGone()
			})

			It("keeps the "+k.noun+" with the retain policy", func() {
				synced(map[string]string{reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain})
				startDeletion()
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.store.objects).To(HaveLen(1))
				expectGone()
			})

			It("keeps the "+k.noun+" it created when the default deletion policy is retain", func() {
				Expect(reconciler.SetDefaultDeletionPolicy(reconciler.DeletionPolicyRetain)).To(Succeed())
				DeferCleanup(reconciler.SetDefaultDeletionPolicy, reconciler.DeletionPolicyDelete)
				synced(nil)
				writes := e.store.writes
				startDeletion()
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.store.objects).To(HaveLen(1))
				Expect(e.store.writes).To(Equal(writes))
				expectGone()
			})

			// adopted creates a resource with annotations that adopts an
			// existing object.
			adopted := func(annotations map[string]string) string {
				e.createParent(true)
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
				Expect(e.store.objects).To(HaveKey(id))
				Expect(e.store.writes).To(BeZero())
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
				Expect(e.store.objects).To(BeEmpty())
				expectGone()
			})

			It("finishes when the "+k.noun+" is already gone from Cloudflare", func() {
				id := synced(nil)
				delete(e.store.objects, id)
				startDeletion()
				_, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				expectGone()
			})

			It("refuses deletion as its entry's blockers say", func() {
				Expect(k.deletionBlockers).To(Equal(t.deletionBlockers), "the kind's blockers are not its entry's")
			})

			for _, blocker := range t.deletionBlockers {
				It("keeps the "+k.noun+" and reports "+blocker.reason+" while Cloudflare refuses the delete with "+
					fmt.Sprint(blocker.code)+", and deletes it once Cloudflare no longer does", func() {
					id := synced(nil)
					e.store.deleteErr = v7APIError(http.MethodDelete, "parents/"+parentID+"/objects/"+id,
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
					Expect(e.store.objects).To(HaveKey(id))

					By("deleting it once Cloudflare takes the delete")
					e.store.deleteErr = nil
					_, err = reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(e.store.objects).To(BeEmpty())
					expectGone()
				})
			}

			It("keeps the finalizer when Cloudflare fails, so deletion is retried", func() {
				synced(nil)
				e.store.deleteErr = errors.New("unavailable")
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
				e.r = t.newReconciler(k8sClient, func(string) (A, error) { return *new(A), errors.New("bad token") })
				startDeletion()
				_, err := reconcileIt()
				Expect(err).To(MatchError("bad token"))
			})

			It("does nothing for a resource without the finalizer", func() {
				result, err := e.r.crud().reconcileDelete(ctx, k.newObject())
				Expect(err).NotTo(HaveOccurred())
				Expect(result.IsZero()).To(BeTrue())
			})
		})

		Describe("pausing", func() {
			// reconcilePaused reconciles the resource, and checks that it made no
			// Cloudflare call, which the fake records the parent's ID of, and
			// scheduled nothing.
			reconcilePaused := func() {
				GinkgoHelper()
				calls := len(e.store.parentIDs)
				result, err := reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeZero())
				Expect(e.store.parentIDs).To(HaveLen(calls), "a paused resource called Cloudflare")
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
				object := e.store.objects[id]
				Expect(k.matches(get(), &object, e.refs)).To(BeTrue(), "the object is what the spec asks for")
				Expect(ready().Reason).To(Equal("Synced"))
				Expect(ready().ObservedGeneration).To(Equal(get().GetGeneration()))
			})

			It("keeps a paused resource's "+k.noun+" and finalizer when it is deleted, and deletes both once unpaused",
				func() {
					id := synced(nil)
					setPaused(ctx, get(), true)
					Expect(k8sClient.Delete(ctx, get())).To(Succeed())
					reconcilePaused()
					Expect(e.store.objects).To(HaveKey(id))
					Expect(get().GetFinalizers()).To(ContainElement(reconciler.Finalizer))
					Expect(ready().Message).To(Equal(pausedDeletionMessage))

					By("deleting both once unpaused")
					setPaused(ctx, get(), false)
					_, err := reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(e.store.objects).NotTo(HaveKey(id))
					Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, k.newObject()))).To(BeTrue())
				})

			It("adds no finalizer to a resource created paused", func() {
				e.createParent(true)
				obj := t.newResource(key, parentName)
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
					obj := t.newResource(key, parentName)
					if change.setup != nil {
						change.setup(obj)
					}
					Expect(k8sClient.Create(ctx, obj)).To(Succeed())
					obj = get()
					change.edit(obj)
					Expect(k8sClient.Update(ctx, obj)).To(MatchError(ContainSubstring(change.field + " is immutable")))
				})
			}

			for _, invalid := range t.invalid {
				It("refuses "+invalid.what, func() {
					obj := t.newResource(key, parentName)
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

			It("registers with a manager, whose API reader reads past its cache", func() {
				mgr, err := manager.New(cfg, manager.Options{
					Controller: config.Controller{SkipNameValidation: new(true)},
				})
				Expect(err).NotTo(HaveOccurred())
				r := t.newReconciler(mgr.GetClient(), nil)
				Expect(r.SetupWithManager(mgr)).To(Succeed())
				Expect(r.crud().apiReader).To(BeIdenticalTo(mgr.GetAPIReader()))
			})

			It("reads the API server through its client without a manager", func() {
				Expect(t.newReconciler(k8sClient, nil).crud().apiReader).To(BeIdenticalTo(k8sClient))
			})

			if !zoned {
				It("maps a CloudflareAccount to the resources that use it", func() {
					create(key, nil)
					account := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Name: accountName}}
					Expect(e.r.crud().resourcesForAccount(ctx, account)).To(ConsistOf(reconcile.Request{NamespacedName: key}))
					other := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Name: accountName + "-other"}}
					Expect(e.r.crud().resourcesForAccount(ctx, other)).To(BeEmpty())

					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					Expect(e.r.crud().resourcesForAccount(cancelled, account)).To(BeNil(), "the list fails")
				})

				It("counts as a user of its CloudflareAccount", func() {
					create(key, nil)
					deps, err := dependentsOf(ctx, k8sClient, "", accountName, accountDependents)
					Expect(err).NotTo(HaveOccurred())
					Expect(deps).To(ConsistOf(k.name + " " + key.String()))
				})
			}
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
		// credentials directly (account CRUD kinds), reference a Zone (zone
		// CRUD kinds, rule and settings kinds), or reference a Tunnel, which
		// gives their account (TunnelConfiguration).
		want := make([]string, 0, len(generatedAccountDependents)+len(generatedZoneDependents)+1)
		for _, d := range slices.Concat(generatedAccountDependents, generatedZoneDependents) {
			want = append(want, strings.ToLower(d.kind))
		}
		Expect(names).To(ConsistOf(append(want, "tunnelconfiguration")))
	})
})
