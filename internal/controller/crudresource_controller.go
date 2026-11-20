/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go/v7/packages/pagination"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// A CRUD kind's resources each manage one object in a Cloudflare account, or
// in a zone, that the API creates, reads, updates and deletes by ID: the kinds
// kflare-gen generates from a cloudflare-go v7 service, such as KVNamespace
// and WorkerRoute. They reconcile the same way. A resource resolves its
// account's token, through its Zone for a zone's kind, and reads its object by
// the ID in its status, creating a new one if it was deleted outside kflare.
// Without one, it creates one, or, as its adoption policy allows
// (adoption.go), adopts the account's or zone's object with the same key,
// unless another resource of the kind claims it. It claims the object, updates
// it when it differs from the spec, and deletes it with the resource once
// nothing depends on it, unless it is retained, or another resource in its
// namespace describes the same object: a renamed resource's object goes to its
// successor instead (handOff). A kind supplies only what differs: a
// crudResourceKind, which calls the kind's cloudflare-go v7 service.
//
// Some objects have no ID of their own: their key is their ID, as an R2
// bucket's name is. Such a kind reads the object by its key, where another
// finds the object in its list: in its pages, or in the one page of a list
// that is never paged, as a zone's Worker routes are.
//
// A zone's kind names its Zone in its own namespace. Some may also name one in
// another namespace (zoneNamespace), which must allow the resource's namespace
// and hostname (zonereference.go).
//
// A kind's spec may reference resources of other kinds in its namespace, as a
// WorkerRoute names its WorkerScript: a resource waits for each to be ready,
// and sends the value it gives, such as the Worker's name.

// crudResourceKind is what the shared CRUD reconciler needs to know about a
// kind. O is the kind's API type, A the narrow interface to the
// cloudflare-go v7 service it calls, R the Cloudflare object that service
// returns, and L the object as a page of its list shows it: R itself for a
// KV namespace, a shorter type for a D1 database, and R for a kind that never
// lists, such as R2Bucket.
type crudResourceKind[O conditionedObject, A any, R any, L any] struct {
	// name is the kind's name, and noun what messages call its Cloudflare
	// object, as in "KVNamespace" and "KV namespace".
	name, noun string
	newObject  func() O
	newList    func() client.ObjectList

	// accountRef returns the name of the CloudflareAccount a resource of an
	// account's kind uses. A zone's kind has zoneRef instead, which returns
	// the name of the Zone, in the resource's namespace, whose zone holds its
	// object, and statusZoneID, which returns where status records that
	// zone's ID.
	accountRef   func(O) string
	zoneRef      func(O) string
	statusZoneID func(O) *string
	// zoneNamespace, hostname and objectHostname are set for a zone's kind
	// whose resources may name a Zone in another namespace. zoneNamespace
	// returns the namespace a resource's zoneRef names, "" for its own;
	// hostname the hostname its spec asks the Zone for, and objectHostname the
	// one an object holds, each with its wildcard if it has one. Such a Zone
	// must allow both (checkZoneReference).
	zoneNamespace  func(O) string
	hostname       func(O) string
	objectHostname func(*R) string
	// key returns the value a resource adopts an existing object by, such as
	// a title that no two objects in an account share.
	key func(O) string
	// claimLabel holds the ID of the object a resource manages. A resource
	// does not adopt an object that another one claims in the same Cloudflare
	// account: it reports conflictReason instead, unless the kind's objects are
	// shared (sharedObjects).
	claimLabel, conflictReason string
	// sharedObjects is set for a kind whose resources may name the same object:
	// two Zones in different namespaces can point at one Cloudflare zone, and
	// their namespaces' resources work in it side by side.
	sharedObjects bool
	// address returns what, besides the account and the ID, names a
	// resource's object in every call: an R2 bucket's jurisdiction, in which
	// another bucket may have the same name. Two resources claim the same
	// object only when their addresses match. nil for a kind whose objects
	// the account and the ID name alone.
	address func(O) []string
	// statusID returns the ID of the object recorded in a resource's status,
	// and observe records an object there, its ID included.
	statusID func(O) string
	observe  func(O, *R)

	// id returns an object's ID, and differences the spec fields whose value
	// the object does not hold, given what the resource's references give
	// (refs): none when the object is what the spec asks for (matches).
	// listedID and listedKey return the ID and the key of an object a page of
	// the list shows.
	id          func(*R) string
	differences func(obj O, current *R, refs map[string]string) []string
	listedID    func(*L) string
	listedKey   func(*L) string

	// The kind's calls to Cloudflare for the resource obj, in the account or
	// zone parentID; obj gives the object's address, and refs the values its
	// references give, by the spec field that holds each. list returns one
	// page of the parent's objects, counting from 1, and listAll, for a kind
	// whose list is never paged, all of them; the other one is nil. Both are
	// nil for a kind whose objects their key identifies, together with
	// listedID and listedKey: such a kind reads an object by its key instead.
	get     func(ctx context.Context, api A, obj O, parentID, id string) (*R, error)
	list    func(ctx context.Context, api A, obj O, parentID string, page int64) (*pagination.V4PagePaginationArray[L], error)
	listAll func(ctx context.Context, api A, obj O, parentID string) (*pagination.SinglePage[L], error)
	create  func(ctx context.Context, api A, parentID string, obj O, refs map[string]string) (*R, error)
	update  func(ctx context.Context, api A, obj O, parentID, id string, refs map[string]string) (*R, error)
	delete  func(ctx context.Context, api A, obj O, parentID, id string) error

	// references are the spec's references to resources of other kinds.
	references []crudReference[O]

	// deletionBlockers are the refusals that keep an object from being
	// deleted until someone acts, such as emptying an R2 bucket.
	deletionBlockers []deletionBlocker

	// dependents use a resource of the kind, which waits for them before it is
	// deleted.
	dependents []dependentKind

	// hooks are what the kind does beyond the shared loop.
	hooks crudHooks[O, A, R]
}

// matches reports whether current is what obj's spec asks for: no field
// differs.
func (k crudResourceKind[O, A, R, L]) matches(obj O, current *R, refs map[string]string) bool {
	return len(k.differences(obj, current, refs)) == 0
}

// observable reports whether a resource of the kind can be observed
// (observeOnly): unless its hooks compare an object themselves, the kind's
// differences say what differs.
func (k crudResourceKind[O, A, R, L]) observable() bool {
	return k.hooks.matches == nil || k.hooks.differences != nil
}

// crudHooks are a kind's hooks: hand-written functions, in its <kind>_hooks.go,
// for what its entry cannot say and Cloudflare's API does not describe. Every
// one of them is optional.
type crudHooks[O conditionedObject, A any, R any] struct {
	// account returns the CloudflareAccount a resource uses, for a kind whose
	// spec names none, or says why the resource waits. gone is set when what
	// leads to the account no longer exists, so that deleting the resource
	// leaves its object: a tunnel's configuration, without its Tunnel.
	account func(ctx context.Context, c client.Client, obj O) (name string, gone bool, condErr *conditionError)
	// references are the kind's references that no field of its service
	// holds, such as a DNSRecord's tunnelRef; their values reach the kind's
	// calls and matches as the generated ones' do. The hooks' create and update
	// use them.
	references []crudReference[O]
	// resolve adds to refs what the resource's calls need besides its spec,
	// or says why the resource waits, such as a Worker's code in a ConfigMap:
	// until something it watches changes, or a retry, if the error asks for
	// one.
	resolve func(ctx context.Context, c client.Client, obj O, refs map[string]string) *conditionError
	// key replaces the kind's key where what names the object comes from a
	// reference: a tunnel's configuration goes by the tunnel's ID.
	key func(obj O, refs map[string]string) string
	// gone reports that an object Cloudflare still returns was deleted, as a
	// deleted tunnel stays readable. A resource then takes it for missing.
	gone func(current *R) bool
	// find replaces the search for the parent's object a resource adopts or
	// finds as its own: it returns the object's ID, "" for none, and the object
	// when it read it. refs is what the references and resolve gave.
	find func(ctx context.Context, c client.Client, api A, obj O, parentID string, refs map[string]string) (string, *R, error)
	// refuse says why a resource cannot take the object it found under its
	// key, such as a list of another kind with the same name.
	refuse func(found *R) *conditionError
	// refuseAdoption says why a resource cannot adopt the object it found,
	// which is not its own, as its spec stands: adopting overwrites the object
	// to match the spec, and a kind may hold back a change the spec never
	// spelled out, as a DNSRecord does with a proxied record.
	refuseAdoption func(obj O, found *R, refs map[string]string) *conditionError
	// matches replaces the comparison of the object with the spec. A kind
	// with it can be observed only with differences too.
	matches func(obj O, current *R, refs map[string]string) bool
	// sourceChanged reports that what the resource's calls send changed since
	// its last write, its spec staying the same, as a Worker's code does in a
	// ConfigMap. An update then follows that change, rather than correct what
	// changed in Cloudflare (recordDriftCorrection).
	sourceChanged func(obj O, refs map[string]string) bool
	// synced runs once the object is as the spec asks and status records it.
	// It does what the service's calls cannot, such as an IPList's items. Set
	// stop to end the reconcile with what it returns, rather than report the
	// resource ready.
	// readsStatus says that synced decides from what status records, as an
	// IPList's running operation: it then runs only once the cache holds the
	// resource's last write (upToDate), at the cost of a read of the API
	// server on each reconcile.
	readsStatus bool
	synced      func(ctx context.Context, c client.Client, api A, obj O, parentID string, current *R) (result ctrl.Result,
		stop bool, err error)
	// describes replaces the comparison of two resources' keys that tells
	// whether other describes the object obj manages, and so takes it over when
	// obj is deleted (successor): a kind whose key alone does not name its
	// object, as a DNS record's content tells it from the others of its name.
	describes func(ctx context.Context, c client.Client, obj, other O) (bool, error)
	// beforeDelete runs before the object is deleted from Cloudflare, with the
	// retain policy unset. An object that is already gone is no failure.
	beforeDelete func(ctx context.Context, api A, obj O, parentID, id string) error
	// differences replaces the kind's differences when observing
	// (observeOnly): the spec fields whose value the object does not hold, the
	// ones the kind's synced hook would correct included.
	differences func(obj O, current *R, refs map[string]string) []string
	// syncedDifferences adds the spec fields synced would correct that only a
	// call to Cloudflare tells, as an IPList's items.
	syncedDifferences func(ctx context.Context, api A, obj O, parentID string, current *R) ([]string, error)
	// watch adds the kind's own watches.
	watch func(b *builder.Builder, c client.Client) *builder.Builder
}

// crudReference is a spec field of a kind that references a resource of
// another kind in the resource's namespace, such as a WorkerRoute's
// workerScriptRef. A resource waits until the referenced resource exists, uses
// the resource's CloudflareAccount and is ready, then sends the value it
// gives: the WorkerScript's Worker name.
type crudReference[O any] struct {
	// field is the spec field, and kind the referenced kind.
	field, kind string
	// name returns the referenced resource's name: "" when the spec leaves
	// the reference unset, which gives no value.
	name      func(O) string
	newObject func() conditionedObject
	// account returns the CloudflareAccount a referenced resource uses, and
	// value what it gives.
	account, value func(conditionedObject) string
}

// reference builds a crudReference to resources of type P (a *T).
func reference[O any, T any, P interface {
	*T
	conditionedObject
}](field, kind string, name func(O) string, account, value func(P) string) crudReference[O] {
	return crudReference[O]{
		field: field, kind: kind, name: name,
		newObject: func() conditionedObject { return P(new(T)) },
		account:   func(obj conditionedObject) string { return account(obj.(P)) },
		value:     func(obj conditionedObject) string { return value(obj.(P)) },
	}
}

// deletionBlocker is a refusal to delete an object that lasts until someone
// acts: Cloudflare refuses the delete with code, and the resource reports
// reason, with what is wrong (message), and is checked again after a minute.
// The objects are not watched: kflare only sees the change on a retry.
type deletionBlocker struct {
	code            int
	reason, message string
}

// crudResourceReconciler reconciles the resources of one CRUD kind. Each
// kind's exported reconciler delegates to one.
type crudResourceReconciler[O conditionedObject, A any, R any, L any] struct {
	client.Client
	// apiReader reads from the API server, past the manager's cache, which
	// Client reads from (see upToDate and claimant).
	apiReader client.Reader
	kind      crudResourceKind[O, A, R, L]
	newAPI    func(token string) (A, error)
}

func (r *crudResourceReconciler[O, A, R, L]) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := r.kind.newObject()
	// reconcileDelete leaves an observed resource's object itself, once
	// nothing uses the resource, so startReconcile is not asked to.
	if done, result, err := startReconcile(ctx, r.Client, req, obj, true, func() (ctrl.Result, error) {
		return r.reconcileDelete(ctx, obj)
	}); done {
		return result, err
	}
	// startReconcile refused an unknown policy.
	if observed, _ := reconciler.Observed(obj); observed && !r.kind.observable() {
		return ctrl.Result{}, updateNotReady(ctx, r.Client, obj, reasonObserveNotSupported, observeNotSupportedMessage)
	}

	// A zone's kind waits for its Zone (the Zone watch re-triggers this
	// reconcile when it is ready) and calls Cloudflare with its account.
	// The account is used from the resource's namespace, or, through a Zone,
	// from the Zone's: what the account allows is asked of that one.
	accountName, whose, usedFrom := "", "this "+r.kind.name, obj.GetNamespace()
	var zone *cloudflarev1alpha1.Zone
	if r.kind.zoneRef != nil {
		var condErr *conditionError
		if zone, condErr = r.zone(ctx, obj); condErr != nil {
			return ctrl.Result{}, updateNotReady(ctx, r.Client, obj, condErr.Reason, condErr.Message)
		}
		accountName, whose, usedFrom = zone.Spec.AccountRef.Name, zoneLabel(obj, client.ObjectKeyFromObject(zone)),
			zone.Namespace
	} else if r.kind.hooks.account != nil {
		name, _, condErr := r.kind.hooks.account(ctx, r.Client, obj)
		if condErr != nil {
			return ctrl.Result{}, updateNotReady(ctx, r.Client, obj, condErr.Reason, condErr.Message)
		}
		accountName = name
	} else {
		accountName = r.kind.accountRef(obj)
	}
	refs, condErr := r.resolveReferences(ctx, obj, accountName, whose)
	if condErr != nil {
		return ctrl.Result{}, updateNotReady(ctx, r.Client, obj, condErr.Reason, condErr.Message)
	}

	account, token, credErr := resolveAccountToken(ctx, r.Client, usedFrom, accountName, true)
	if credErr != nil {
		return notReadyRetryAfter(ctx, r.Client, obj, credErr)
	}
	if wait := admit(account, token, obj); wait > 0 {
		return rateLimited(ctx, r.Client, obj, wait)
	}
	if r.kind.hooks.resolve != nil {
		if condErr := r.kind.hooks.resolve(ctx, r.Client, obj, refs); condErr != nil {
			if condErr.retry {
				return notReadyRetryAfter(ctx, r.Client, obj, condErr)
			}
			return ctrl.Result{}, updateNotReady(ctx, r.Client, obj, condErr.Reason, condErr.Message)
		}
	}
	api, err := r.newAPI(token)
	if err != nil {
		return invalidToken(ctx, r.Client, obj, err)
	}

	parentID := account.Spec.AccountID
	var sharedZone *cloudflarev1alpha1.Zone
	if zone != nil {
		parentID = zone.Status.CloudflareMetadata.ZoneID
		if zone.Namespace != obj.GetNamespace() {
			sharedZone = zone
		}
	}
	return r.sync(ctx, obj, account.Spec.AccountID, parentID, refs, api, sharedZone)
}

// zoneKey returns the Zone a resource of a zone's kind names: one in its own
// namespace, unless its kind's resources may name another and it does.
func (r *crudResourceReconciler[O, A, R, L]) zoneKey(obj O) types.NamespacedName {
	key := types.NamespacedName{Name: r.kind.zoneRef(obj), Namespace: obj.GetNamespace()}
	if r.kind.zoneNamespace != nil {
		if namespace := r.kind.zoneNamespace(obj); namespace != "" {
			key.Namespace = namespace
		}
	}
	return key
}

// zoneLabel is what messages call the Zone key that obj names: by its name,
// with its namespace when that is not obj's.
func zoneLabel(obj client.Object, key types.NamespacedName) string {
	if key.Namespace != obj.GetNamespace() {
		return fmt.Sprintf("Zone %q", key.Namespace+"/"+key.Name)
	}
	return fmt.Sprintf("Zone %q", key.Name)
}

// zone returns the ready Zone a resource of a zone's kind belongs to, or why
// the resource waits for it. A Zone in another namespace must allow the
// resource's namespace and the hostname its spec asks for, which comes before
// its readiness: a namespace it leaves out learns nothing more about it.
func (r *crudResourceReconciler[O, A, R, L]) zone(ctx context.Context, obj O) (*cloudflarev1alpha1.Zone, *conditionError) {
	zone := &cloudflarev1alpha1.Zone{}
	key := r.zoneKey(obj)
	what := zoneLabel(obj, key)
	if condErr := getReference(ctx, r.Client, key, zone, what, "ZoneNotFound"); condErr != nil {
		return nil, condErr
	}
	if key.Namespace != obj.GetNamespace() {
		if condErr := checkZoneReference(ctx, r.Client, zone, obj.GetNamespace(), r.kind.hostname(obj)); condErr != nil {
			return nil, condErr
		}
	}
	observed, _ := reconciler.Observed(obj)
	if !isZoneReady(zone, observed) {
		return nil, &conditionError{Reason: "ZoneNotReady", Message: what + " is not ready"}
	}
	return zone, nil
}

// checkObjectHostname returns why sharedZone, a Zone in another namespace,
// does not allow obj to manage the object current, nil if it does: the
// object's hostname, not only the spec's, must be one the Zone allows obj's
// namespace. obj names the object by an ID, which its status or its claim
// label holds, and whoever may edit obj may write that label.
func (r *crudResourceReconciler[O, A, R, L]) checkObjectHostname(ctx context.Context, obj O,
	sharedZone *cloudflarev1alpha1.Zone, current *R) *conditionError {
	hostname := r.kind.objectHostname(current)
	condErr := checkZoneReference(ctx, r.Client, sharedZone, obj.GetNamespace(), hostname)
	if condErr == nil {
		return nil
	}
	return &conditionError{
		Reason: condErr.Reason,
		Message: fmt.Sprintf("%s %s, which this %s names by its ID, has the hostname %q. %s", r.kind.noun,
			r.kind.id(current), r.kind.name, hostname, condErr.Message),
	}
}

// resolveReferences returns the value each of the kind's references gives, by
// the spec field holding it, or why the resource waits. A referenced resource
// must exist, be ready and use the CloudflareAccount accountName, which
// whose uses: a Worker route can only run a Worker of its zone's account. An
// unset reference gives "".
func (r *crudResourceReconciler[O, A, R, L]) resolveReferences(ctx context.Context, obj O,
	accountName, whose string) (map[string]string, *conditionError) {
	refs := map[string]string{}
	for _, ref := range r.references() {
		name := ref.name(obj)
		if name == "" {
			refs[ref.field] = ""
			continue
		}
		target := ref.newObject()
		key := types.NamespacedName{Name: name, Namespace: obj.GetNamespace()}
		if condErr := getReference(ctx, r.Client, key, target, fmt.Sprintf("%s %q", ref.kind, name),
			ref.kind+"NotFound"); condErr != nil {
			return nil, condErr
		}
		if account := ref.account(target); account != accountName {
			return nil, &conditionError{
				Reason: reasonAccountMismatch,
				Message: fmt.Sprintf("%s %q uses CloudflareAccount %q but %s uses %q; %s can only use %s of its "+
					"own account", ref.kind, name, account, whose, accountName, r.kind.noun, ref.kind),
			}
		}
		if !isReady(target) {
			return nil, &conditionError{
				Reason:  ref.kind + "NotReady",
				Message: fmt.Sprintf("%s %q is not ready", ref.kind, name),
			}
		}
		refs[ref.field] = ref.value(target)
	}
	return refs, nil
}

// references returns the kind's references, its hooks' included.
func (r *crudResourceReconciler[O, A, R, L]) references() []crudReference[O] {
	return slices.Concat(r.kind.references, r.kind.hooks.references)
}

// sync drives the desired→observed→delta→reconcile loop for one resource, in
// the account or zone parentID, of the Cloudflare account accountID.
// sharedZone is the resource's Zone when it is in another namespace, and nil
// otherwise: the object status names must then be one that Zone allows
// (checkObjectHostname). An object the resource finds, adopts or creates is
// one of the hostname its spec asks for, which the Zone already allowed.
func (r *crudResourceReconciler[O, A, R, L]) sync(ctx context.Context, obj O, accountID, parentID string,
	refs map[string]string, api A, sharedZone *cloudflarev1alpha1.Zone) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	k := r.kind
	// startReconcile refused an unknown policy.
	if observed, _ := reconciler.Observed(obj); observed {
		return r.observeOnly(ctx, obj, parentID, refs, api, sharedZone)
	}

	// Observe the object by the ID in status. If it is gone, it was deleted
	// outside kflare; a new one is created.
	var current *R
	if id := k.statusID(obj); id != "" {
		got, err := k.get(ctx, api, obj, parentID, id)
		switch {
		case cfpkg.IsNotFound(err):
			logger.Info(k.noun+" deleted outside kflare, recreating", "id", id)
		case err != nil:
			return handleCloudflareError(ctx, r.Client, obj, err)
		case k.hooks.gone != nil && k.hooks.gone(got):
			logger.Info(k.noun+" deleted outside kflare, recreating", "id", id)
		default:
			if sharedZone != nil {
				if condErr := r.checkObjectHostname(ctx, obj, sharedZone, got); condErr != nil {
					return ctrl.Result{}, updateNotReady(ctx, r.Client, obj, condErr.Reason, condErr.Message)
				}
			}
			// Status names an object the resource does not claim: one it
			// recorded while kflare only observed it (observeOnly), which
			// another resource may manage. It claims the object below, so it
			// holds the key and looks for a claim first, as findOrCreate does.
			if !ownsID(obj, k.claimLabel, id) {
				release, err := r.lockObjectKey(ctx, obj, parentID, refs)
				if err != nil {
					return ctrl.Result{}, err
				}
				defer release()
				owner, err := r.claimant(ctx, obj, parentID, id)
				if err != nil {
					return ctrl.Result{}, err
				}
				if owner != "" {
					return notReadyRetryAfter(ctx, r.Client, obj, &conditionError{
						Reason: k.conflictReason,
						Message: fmt.Sprintf("%s %q is already managed by %s %s", k.noun, r.objectKey(obj, refs), k.name,
							owner),
					})
				}
			}
			current = got
		}
	}

	// Without an object, the resource finds, adopts or creates one, unless
	// it is a stale read (upToDate). It holds its key until the sync ends,
	// its claim and status written (lockObjectKey).
	adopting := false
	if current == nil {
		release, err := r.lockObjectKey(ctx, obj, parentID, refs)
		if err != nil {
			return ctrl.Result{}, err
		}
		defer release()
		if fresh, err := upToDate(ctx, r.apiReader, obj, k.newObject()); !fresh || err != nil {
			return ctrl.Result{}, err
		}
		found, adopted, done, result, err := r.findOrCreate(ctx, obj, accountID, parentID, refs, api)
		if done {
			return result, err
		}
		current, adopting = found, adopted
	}

	// Claim the object before anything else can fail, so no other resource of
	// the kind adopts it in the meantime.
	if err := claimID(ctx, r.Client, obj, k.claimLabel, k.id(current), adopting); err != nil {
		return ctrl.Result{}, err
	}

	// Drift: update the object in place when it is not what the spec asks
	// for, unless the resource is a stale read: of its spec, or of what its
	// status records, which a kind's matches hook may compare (a Worker's
	// hash).
	matches := k.matches
	if k.hooks.matches != nil {
		matches = k.hooks.matches
	}
	if !matches(obj, current, refs) {
		if fresh, err := upToDate(ctx, r.apiReader, obj, k.newObject()); !fresh || err != nil {
			return ctrl.Result{}, err
		}
		updated, err := k.update(ctx, api, obj, parentID, k.id(current), refs)
		if err != nil {
			return handleCloudflareError(ctx, r.Client, obj, err)
		}
		if k.hooks.sourceChanged == nil || !k.hooks.sourceChanged(obj, refs) {
			recordDriftCorrection(k.name, obj)
		}
		current = updated
		logger.Info("Updated "+k.noun, "id", k.id(current))
	}

	if k.statusZoneID != nil {
		*k.statusZoneID(obj) = parentID
	}
	k.observe(obj, current)
	if result, stop, err := r.synced(ctx, api, obj, parentID, current); stop {
		return result, err
	}
	return readyWithDriftCheck(ctx, r.Client, obj, "Synced", k.noun+" is synced with Cloudflare")
}

// synced runs the kind's synced hook, if it has one, once the object is as
// the spec asks: only once the cache holds the resource's last write, when the
// hook decides from status (readsStatus). stop ends the reconcile with result
// and err, as the hook's does.
func (r *crudResourceReconciler[O, A, R, L]) synced(ctx context.Context, api A, obj O, parentID string,
	current *R) (result ctrl.Result, stop bool, err error) {
	k := r.kind
	if k.hooks.synced == nil {
		return ctrl.Result{}, false, nil
	}
	if k.hooks.readsStatus {
		if fresh, err := upToDate(ctx, r.apiReader, obj, k.newObject()); !fresh || err != nil {
			return ctrl.Result{}, true, err
		}
	}
	return k.hooks.synced(ctx, r.Client, api, obj, parentID, current)
}

// observeOnly is sync for a resource kflare only observes
// (reconciler.Observed): it reads the object status names, or else the
// parent's object with the resource's key, as the adopt policy finds it,
// records it in status, and reports InSync, DriftDetected with the spec fields
// that differ (never their values, which may be secret), or NotFound. It
// writes nothing to Cloudflare and claims nothing, so it needs no lock. An
// object that is not the resource's own is marked adopted on the resource, so
// that a later switch to manage keeps it on deletion, as an adopted one.
func (r *crudResourceReconciler[O, A, R, L]) observeOnly(ctx context.Context, obj O, parentID string,
	refs map[string]string, api A, sharedZone *cloudflarev1alpha1.Zone) (ctrl.Result, error) {
	k := r.kind
	key := r.objectKey(obj, refs)
	what := fmt.Sprintf("%s %q", k.noun, key)

	var current *R
	id := k.statusID(obj)
	for _, byKey := range []bool{false, true} {
		if byKey {
			var err error
			if id, current, err = r.find(ctx, api, obj, parentID, key, refs); err != nil {
				return handleCloudflareError(ctx, r.Client, obj, err)
			}
		}
		if id != "" && current == nil {
			got, err := k.get(ctx, api, obj, parentID, id)
			if err != nil && !cfpkg.IsNotFound(err) {
				return handleCloudflareError(ctx, r.Client, obj, err)
			}
			if err == nil {
				current = got
			}
		}
		if current != nil && k.hooks.gone != nil && k.hooks.gone(current) {
			current = nil
		}
		if current != nil {
			break
		}
	}
	if current == nil {
		return observedNotReady(ctx, r.Client, obj, &conditionError{
			Reason:  reasonNotFound,
			Message: what + " does not exist; kflare only observes this resource, and creates none",
		})
	}
	if k.hooks.refuse != nil {
		if condErr := k.hooks.refuse(current); condErr != nil {
			return notReadyRetryAfter(ctx, r.Client, obj, condErr)
		}
	}
	if sharedZone != nil {
		if condErr := r.checkObjectHostname(ctx, obj, sharedZone, current); condErr != nil {
			return ctrl.Result{}, updateNotReady(ctx, r.Client, obj, condErr.Reason, condErr.Message)
		}
	}
	// An object the resource was creating when it stopped is its own too
	// (creating): marking it adopted would keep it when the resource goes.
	if !ownsID(obj, k.claimLabel, k.id(current)) && !creating(obj, key) {
		if err := markAdopted(ctx, r.Client, obj); err != nil {
			return ctrl.Result{}, err
		}
	}

	if k.statusZoneID != nil {
		*k.statusZoneID(obj) = parentID
	}
	k.observe(obj, current)
	differences := k.differences
	if k.hooks.differences != nil {
		differences = k.hooks.differences
	}
	fields := differences(obj, current, refs)
	if k.hooks.syncedDifferences != nil {
		more, err := k.hooks.syncedDifferences(ctx, api, obj, parentID, current)
		if err != nil {
			return handleCloudflareError(ctx, r.Client, obj, err)
		}
		fields = append(fields, more...)
	}
	if len(fields) > 0 {
		return observedNotReady(ctx, r.Client, obj, &conditionError{
			Reason: reasonDriftDetected,
			Message: fmt.Sprintf("%s differs from the spec in: %s. kflare only observes this resource, and changes "+
				"nothing in Cloudflare", what, strings.Join(fields, ", ")),
		})
	}
	return readyWithDriftCheck(ctx, r.Client, obj, reasonInSync, what+" matches the spec; kflare only observes it")
}

// observedNotReady reports condErr on an observed resource whose object is
// missing or differs from its spec, and reads the object again at the next
// drift check, as for one that matches, or after credentialsRetryInterval
// without a drift check interval. A migration starts with most resources in
// this state, and reading each every minute would spend their token's budget.
func observedNotReady(ctx context.Context, c client.StatusClient, obj conditionedObject,
	condErr *conditionError) (ctrl.Result, error) {
	retry := reconciler.NextDriftCheck()
	if retry == 0 {
		retry = credentialsRetryInterval
	}
	return ctrl.Result{RequeueAfter: retry}, updateNotReady(ctx, c, obj, condErr.Reason, condErr.Message)
}

// upToDate reports whether obj, as the cache gives it, is the version the API
// server holds, reading that through apiReader into current, a new object of
// obj's kind. The shared reconcilers ask before they write to Cloudflare on
// what the resource records: a reconcile queued by the previous one's writes
// may read the resource before its last status write reached the cache, and
// would create, adopt or upload again. When the cache is behind, the reconcile
// stops; the watch event that catches the cache up reconciles the resource
// again.
func upToDate(ctx context.Context, apiReader client.Reader, obj, current client.Object) (bool, error) {
	if err := apiReader.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if current.GetResourceVersion() != obj.GetResourceVersion() {
		log.FromContext(ctx).V(1).Info("Waiting for the cache, which is behind the API server",
			"cached", obj.GetResourceVersion(), "current", current.GetResourceVersion())
		return false, nil
	}
	return true, nil
}

// findOrCreate returns the object of a resource whose status records none:
// the parent's object with its key, which is the resource's own when its
// claim or creating annotation says so, or else adopted as its adoption policy
// allows, unless another resource claims it. Without one, it creates the
// object unless the policy is adopt. A listed object is read by its ID for
// what the list leaves out. done ends the reconcile with result and err.
func (r *crudResourceReconciler[O, A, R, L]) findOrCreate(ctx context.Context, obj O, accountID, parentID string,
	refs map[string]string, api A) (current *R, adopting, done bool, result ctrl.Result, err error) {
	logger := log.FromContext(ctx)
	k := r.kind
	stop := func(result ctrl.Result, err error) (*R, bool, bool, ctrl.Result, error) {
		return nil, false, true, result, err
	}

	key := r.objectKey(obj, refs)
	what := fmt.Sprintf("%s %q", k.noun, key)
	id, found, err := r.find(ctx, api, obj, parentID, key, refs)
	if err != nil {
		return stop(handleCloudflareError(ctx, r.Client, obj, err))
	}
	if id != "" {
		owner, err := r.claimant(ctx, obj, parentID, id)
		if err != nil {
			return stop(ctrl.Result{}, err)
		}
		if owner != "" {
			return stop(notReadyRetryAfter(ctx, r.Client, obj, &conditionError{
				Reason:  k.conflictReason,
				Message: fmt.Sprintf("%s is already managed by %s %s", what, k.name, owner),
			}))
		}
		own := ownsID(obj, k.claimLabel, id) || creating(obj, key)
		if !own {
			elsewhere, err := watchScopeFrom(ctx).accountsElsewhere(ctx, r.Client, accountID)
			if err != nil {
				return stop(ctrl.Result{}, err)
			}
			if condErr := mayAdopt(obj, what, elsewhere); condErr != nil {
				return stop(notReadyRetryAfter(ctx, r.Client, obj, condErr))
			}
		}
		if found == nil {
			found, err = k.get(ctx, api, obj, parentID, id)
			switch {
			case cfpkg.IsNotFound(err):
				logger.Info(k.noun+" deleted since it was listed", "key", key, "id", id)
			case err != nil:
				return stop(handleCloudflareError(ctx, r.Client, obj, err))
			}
		}
		if found != nil && k.hooks.refuse != nil {
			if condErr := k.hooks.refuse(found); condErr != nil {
				return stop(notReadyRetryAfter(ctx, r.Client, obj, condErr))
			}
		}
		if found != nil && !own && k.hooks.refuseAdoption != nil {
			if condErr := k.hooks.refuseAdoption(obj, found, refs); condErr != nil {
				return stop(notReadyRetryAfter(ctx, r.Client, obj, condErr))
			}
		}
		switch {
		case found == nil:
		case own:
			current = found
			logger.Info("Found the "+k.noun+" this resource created", "key", key, "id", id)
		default:
			current, adopting = found, true
			logger.Info("Adopted existing "+k.noun, "key", key, "id", id)
		}
	}
	if current == nil {
		if condErr := mayCreate(obj, what); condErr != nil {
			return stop(notReadyRetryAfter(ctx, r.Client, obj, condErr))
		}
		if err := markCreating(ctx, r.Client, obj, key); err != nil {
			return stop(ctrl.Result{}, err)
		}
		current, err = k.create(ctx, api, parentID, obj, refs)
		if err != nil {
			return stop(handleCloudflareError(ctx, r.Client, obj, err))
		}
		recordDriftCorrection(k.name, obj)
		logger.Info("Created "+k.noun, "key", key, "id", k.id(current))
	}
	return current, adopting, false, ctrl.Result{}, nil
}

// objectKey returns the key obj finds, adopts or creates its object by: its
// kind's key, or what the kind's key hook makes of it with refs.
func (r *crudResourceReconciler[O, A, R, L]) objectKey(obj O, refs map[string]string) string {
	if r.kind.hooks.key != nil {
		return r.kind.hooks.key(obj, refs)
	}
	return r.kind.key(obj)
}

// crudObjectLocks holds a key per kind, parent and object key that a resource
// is finding, adopting or creating an object by (lockObjectKey).
var crudObjectLocks keyLock

// lockObjectKey waits until obj is the only resource of its kind finding,
// adopting or creating the parent's object with its key, and returns the
// release. Two at once could both create, or both own one object; one at a
// time, the second sees the first's claim and reports the conflict. Hold it
// until the sync, status write included, ends.
func (r *crudResourceReconciler[O, A, R, L]) lockObjectKey(ctx context.Context, obj O, parentID string,
	refs map[string]string) (func(), error) {
	return crudObjectLocks.lock(ctx, r.kind.name+"/"+parentID+"/"+r.objectKey(obj, refs))
}

// find returns the ID of the parent's object with key, or "" when there is
// none. A kind's find hook decides, if it has one. A kind whose objects their
// key identifies reads the object by its key, and also returns what it read;
// another finds the object in its list.
func (r *crudResourceReconciler[O, A, R, L]) find(ctx context.Context, api A, obj O, parentID, key string,
	refs map[string]string) (string, *R, error) {
	k := r.kind
	if k.hooks.find != nil {
		return k.hooks.find(ctx, r.Client, api, obj, parentID, refs)
	}
	match := func(listed *L) bool { return k.listedKey(listed) == key }
	var listed *L
	switch {
	case k.listAll != nil:
		res, err := k.listAll(ctx, api, obj, parentID)
		if err != nil {
			return "", nil, err
		}
		if i := slices.IndexFunc(res.Result, func(l L) bool { return match(&l) }); i >= 0 {
			listed = &res.Result[i]
		}
	case k.list != nil:
		var err error
		listed, err = findInPages(func(page int64) (*pagination.V4PagePaginationArray[L], error) {
			return k.list(ctx, api, obj, parentID, page)
		}, match)
		if err != nil {
			return "", nil, err
		}
	default:
		// The key is the object's ID.
		found, err := k.get(ctx, api, obj, parentID, key)
		switch {
		case cfpkg.IsNotFound(err):
			return "", nil, nil
		case err != nil:
			return "", nil, err
		}
		return key, found, nil
	}
	if listed == nil {
		return "", nil, nil
	}
	return k.listedID(listed), nil, nil
}

// claimant returns the "namespace/name" of another resource of the kind, in
// any namespace, that claims the object id at obj's address in the same
// parent (an ID is unique only there), or "". It reads the claims from the API
// server: the resource that held the key just before obj (lockObjectKey) may
// have claimed the object a moment ago, before the cache saw it.
func (r *crudResourceReconciler[O, A, R, L]) claimant(ctx context.Context, obj O, parentID, id string) (string, error) {
	k := r.kind
	if k.sharedObjects {
		return "", nil
	}
	items, err := watchScopeFrom(ctx).listUncached(ctx, r.apiReader, k.newList, labels.Set{k.claimLabel: id})
	if err != nil {
		return "", err
	}
	for _, item := range items {
		other, ok := item.(O)
		if !ok {
			return "", fmt.Errorf("unexpected list item %T", item)
		}
		if other.GetUID() == obj.GetUID() || (k.address != nil && !slices.Equal(k.address(other), k.address(obj))) {
			continue
		}
		same, err := r.sameParent(ctx, obj, other, parentID)
		if err != nil {
			return "", err
		}
		if same {
			return other.GetNamespace() + "/" + other.GetName(), nil
		}
	}
	return "", nil
}

// sameParent reports whether other's object is in obj's Cloudflare account,
// or zone, parentID: other names obj's CloudflareAccount (or Zone), or another
// one for the same Cloudflare account (or zone). A resource whose
// CloudflareAccount or Zone is gone manages nothing, so its object is in none.
func (r *crudResourceReconciler[O, A, R, L]) sameParent(ctx context.Context, obj, other O, parentID string) (bool, error) {
	k := r.kind
	if k.zoneRef != nil {
		if r.zoneKey(other) == r.zoneKey(obj) {
			return true, nil
		}
		zone := &cloudflarev1alpha1.Zone{}
		err := r.Get(ctx, r.zoneKey(other), zone)
		switch {
		case apierrors.IsNotFound(err):
			return false, nil
		case err != nil:
			return false, err
		}
		return zone.Status.CloudflareMetadata.ZoneID == parentID, nil
	}
	// An ID that a kind's resources reach through another resource is unique.
	if k.accountRef == nil || k.accountRef(other) == k.accountRef(obj) {
		return true, nil
	}
	account := &cloudflarev1alpha1.CloudflareAccount{}
	err := r.Get(ctx, types.NamespacedName{Name: k.accountRef(other)}, account)
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, err
	}
	return account.Spec.AccountID == parentID, nil
}

// findInPages pages through a list of objects, from page 1, and returns the
// first object match accepts, or nil if there is none. A page that holds no
// object, or fewer than the page size Cloudflare reports, is the last.
func findInPages[R any](
	list func(page int64) (*pagination.V4PagePaginationArray[R], error),
	match func(*R) bool,
) (*R, error) {
	for page := int64(1); ; page++ {
		res, err := list(page)
		if err != nil {
			return nil, err
		}
		for i := range res.Result {
			if match(&res.Result[i]) {
				return &res.Result[i], nil
			}
		}
		if len(res.Result) == 0 || int64(len(res.Result)) < res.ResultInfo.PerPage {
			return nil, nil
		}
	}
}

// reconcileDelete handles the deletion lifecycle. Once nothing depends on the
// resource, it hands the object to the resource's successor, if it has one
// (handOff), or else deletes it from Cloudflare (unless the retain policy is
// set), once the rename grace period is over (successorWait), then removes
// the finalizer. An object that one of the kind's deletion blockers keeps is
// kept with the finalizer, and the resource reports why until the delete goes
// through.
func (r *crudResourceReconciler[O, A, R, L]) reconcileDelete(ctx context.Context, obj O) (ctrl.Result, error) {
	// Safety check: if the finalizer is already gone, there is nothing to do.
	if !controllerutil.ContainsFinalizer(obj, reconciler.Finalizer) {
		return ctrl.Result{}, nil
	}

	dependents, err := dependentsOf(ctx, r.Client, obj.GetNamespace(), obj.GetName(), r.kind.dependents)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(dependents) > 0 {
		return ctrl.Result{}, reportInUse(ctx, r.Client, obj, dependents)
	}

	// The object is the one in status, or, when a reconcile failed before it
	// wrote status, the one its claim names.
	// An observed resource's object stays, with whatever claims it.
	id := r.kind.statusID(obj)
	if id == "" {
		id = obj.GetLabels()[r.kind.claimLabel]
	}
	observed, _ := reconciler.Observed(obj)
	if id != "" && !observed {
		handed, err := r.handOff(ctx, obj, id)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !handed && !reconciler.RetainOnDelete(obj) {
			if wait := r.successorWait(obj); wait > 0 {
				k := r.kind
				recordNormal(obj, reasonAwaitingSuccessor, "Delete", fmt.Sprintf("%s %s/%s keeps %s %s for %s after its "+
					"deletion, for a resource that replaces it", k.name, obj.GetNamespace(), obj.GetName(), k.noun, id,
					reconciler.RenameGracePeriod()))
				log.FromContext(ctx).Info("Keeping the "+k.noun+" for a resource that may replace this one", "id", id,
					"for", wait)
				return ctrl.Result{RequeueAfter: wait}, nil
			}
			from, gone, err := r.deletionParent(ctx, obj)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !gone {
				if result, err := r.deleteObject(ctx, obj, from, id); err != nil || !result.IsZero() {
					return result, err
				}
			}
		}
	}

	if observed && !r.kind.observable() {
		recordWarning(obj, reasonObserveNotSupported, "Delete", observeNotSupportedMessage+
			"; its Cloudflare object stays in Cloudflare")
	}
	// Remove the finalizer to allow Kubernetes to complete the deletion.
	_, err = reconciler.RemoveFinalizer(ctx, r.Client, obj)
	return ctrl.Result{}, err
}

// reasonHandedOff is the reason of the Events of a hand-off (handOff), on the
// resource that gave its object and on the one that took it.
const reasonHandedOff = "HandedOff"

// reasonAwaitingSuccessor is the reason of the Event of a resource that is
// being deleted and keeps its object for a successor (successorWait).
const reasonAwaitingSuccessor = "AwaitingSuccessor"

// successorWait returns how long obj, being deleted without a successor,
// still keeps its object for one: what is left of the rename grace period
// since its deletion, so that a rename that deletes first (Argo CD prunes
// before it applies) does not delete and recreate the object. The new
// resource's creation ends the wait (resourcesBeingDeleted). Kinds whose
// resources share objects never wait.
func (r *crudResourceReconciler[O, A, R, L]) successorWait(obj O) time.Duration {
	if r.kind.sharedObjects {
		return 0
	}
	return reconciler.RenameGracePeriod() - time.Since(obj.GetDeletionTimestamp().Time)
}

// handOff gives the object id of obj, which is being deleted, to obj's
// successor, and reports whether there was one. A rename is a new resource
// with the same spec plus the old one's deletion; deleting the object for the
// new one to recreate would drop a DNS record for a while, or a KV
// namespace's data for good. The successor's claim replaces obj's, keeping
// whether kflare created or adopted it, whatever obj's deletion policy.
// Nothing is written to Cloudflare. The claim is written before obj's
// finalizer goes, so a hand-off that fails halfway is made again.
func (r *crudResourceReconciler[O, A, R, L]) handOff(ctx context.Context, obj O, id string) (bool, error) {
	k := r.kind
	successor, found, err := r.successor(ctx, obj, id)
	if err != nil || !found {
		return false, err
	}
	if err := handClaim(ctx, r.Client, obj, successor, k.claimLabel, id); err != nil {
		return false, err
	}
	message := fmt.Sprintf("%s %s/%s handed %s %s to %s %s/%s", k.name, obj.GetNamespace(), obj.GetName(), k.noun, id,
		k.name, successor.GetNamespace(), successor.GetName())
	recordNormal(obj, reasonHandedOff, "Delete", message)
	recordNormal(successor, reasonHandedOff, "Claim", message)
	log.FromContext(ctx).Info("Handed the "+k.noun+" to the resource that replaces this one", "id", id,
		"successor", successor.GetName())
	return true, nil
}

// successor returns the resource that takes over the object id when obj is
// deleted: another resource of the kind in obj's namespace (never another
// namespace, whose owner may differ), not being deleted, that describes the
// same object in the same parent and manages no other. The oldest wins, then
// the name. Kinds whose resources share objects have none.
func (r *crudResourceReconciler[O, A, R, L]) successor(ctx context.Context, obj O, id string) (O, bool, error) {
	k := r.kind
	var none O
	if k.sharedObjects {
		return none, false, nil
	}
	others, err := reconciler.List[O](ctx, r.Client, k.newList(), client.InNamespace(obj.GetNamespace()))
	if err != nil {
		return none, false, err
	}
	var candidates []O
	for _, other := range others {
		// obj itself is being deleted.
		if !other.GetDeletionTimestamp().IsZero() ||
			(k.address != nil && !slices.Equal(k.address(other), k.address(obj))) {
			continue
		}
		// A resource that manages another object keeps that one.
		if claim := other.GetLabels()[k.claimLabel]; claim != "" && claim != id {
			continue
		}
		if recorded := k.statusID(other); recorded != "" && recorded != id {
			continue
		}
		describes, err := r.describes(ctx, obj, other)
		if err != nil {
			return none, false, err
		}
		if describes {
			candidates = append(candidates, other)
		}
	}
	if len(candidates) == 0 {
		return none, false, nil
	}
	// A resource whose parent is gone manages nothing, so it hands nothing on.
	// An ID that a kind's resources reach through another resource is unique,
	// and needs no parent to compare (sameParent).
	parentID := ""
	if k.zoneRef != nil || k.accountRef != nil {
		if parentID, err = r.parentID(ctx, obj); err != nil || parentID == "" {
			return none, false, err
		}
	}
	slices.SortFunc(candidates, func(a, b O) int {
		return cmp.Or(a.GetCreationTimestamp().Compare(b.GetCreationTimestamp().Time), cmp.Compare(a.GetName(), b.GetName()))
	})
	for _, candidate := range candidates {
		same, err := r.sameParent(ctx, obj, candidate, parentID)
		if err != nil {
			return none, false, err
		}
		if same {
			return candidate, true, nil
		}
	}
	return none, false, nil
}

// describes reports whether other's spec describes the object obj manages:
// by the kind's describes hook, or else when their keys are the same.
func (r *crudResourceReconciler[O, A, R, L]) describes(ctx context.Context, obj, other O) (bool, error) {
	if r.kind.hooks.describes != nil {
		return r.kind.hooks.describes(ctx, r.Client, obj, other)
	}
	return r.kind.key(other) == r.kind.key(obj), nil
}

// parentID returns the ID of the Cloudflare account, or zone, that holds the
// object of obj, which is being deleted: the zone its status records, or its
// Zone's; the account its CloudflareAccount names. It is "" when that one is
// gone, or has no zone yet.
func (r *crudResourceReconciler[O, A, R, L]) parentID(ctx context.Context, obj O) (string, error) {
	k := r.kind
	if k.zoneRef == nil {
		account := &cloudflarev1alpha1.CloudflareAccount{}
		err := r.Get(ctx, types.NamespacedName{Name: k.accountRef(obj)}, account)
		return account.Spec.AccountID, client.IgnoreNotFound(err)
	}
	if id := *k.statusZoneID(obj); id != "" {
		return id, nil
	}
	zone := &cloudflarev1alpha1.Zone{}
	err := r.Get(ctx, r.zoneKey(obj), zone)
	return zone.Status.CloudflareMetadata.ZoneID, client.IgnoreNotFound(err)
}

// deletionParent is where a resource's object is deleted from: the
// CloudflareAccount whose token deletes it, the namespace that uses the
// account (the resource's, or its Zone's), and, for a zone's kind, the zone
// its status records. sharedZone is the resource's Zone when it is in another
// namespace, which must allow the object's hostname.
type deletionParent struct {
	accountName, usedFrom, zoneID string
	sharedZone                    *cloudflarev1alpha1.Zone
}

// deleteObject deletes a resource's object id from Cloudflare, from where
// from says. While one of the kind's deletion blockers keeps the object, it
// reports why and returns a retry. Through a Zone in another namespace, it
// reads the object first, and leaves one whose hostname the Zone does not
// allow the resource's namespace, with a Warning Event: the ID may come from
// the resource's claim label, which whoever may edit the resource may write.
func (r *crudResourceReconciler[O, A, R, L]) deleteObject(ctx context.Context, obj O, from deletionParent,
	id string) (ctrl.Result, error) {
	account, token, credErr := resolveAccountToken(ctx, r.Client, from.usedFrom, from.accountName, false)
	if credErr != nil {
		return ctrl.Result{}, leaveCloudflareObject(ctx, obj, credErr)
	}
	if wait := admit(account, token, obj); wait > 0 {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	api, err := r.newAPI(token)
	if err != nil {
		return ctrl.Result{}, err
	}
	parentID := account.Spec.AccountID
	if r.kind.zoneRef != nil {
		parentID = from.zoneID
	}
	// Without status, the object is the one the claim label names: one a
	// reconcile claimed before it failed to record it, or one a person
	// named, which another resource may manage. The label is anyone's to
	// write, so the object goes only when no other resource claims it.
	if r.kind.statusID(obj) == "" {
		owner, err := r.claimant(ctx, obj, parentID, id)
		if err != nil {
			return ctrl.Result{}, err
		}
		if owner != "" {
			k := r.kind
			recordWarning(obj, k.conflictReason, "Delete", fmt.Sprintf("%s %s, which this resource's %s label names, "+
				"is managed by %s %s; its Cloudflare object stays in Cloudflare", k.noun, id, k.claimLabel, k.name, owner))
			return ctrl.Result{}, nil
		}
	}
	if from.sharedZone != nil {
		current, err := r.kind.get(ctx, api, obj, parentID, id)
		switch {
		case cfpkg.IsNotFound(err):
			return ctrl.Result{}, nil
		case err != nil:
			return retryCleanup(err)
		}
		if condErr := r.checkObjectHostname(ctx, obj, from.sharedZone, current); condErr != nil {
			return ctrl.Result{}, leaveCloudflareObject(ctx, obj, condErr)
		}
	}
	if before := r.kind.hooks.beforeDelete; before != nil {
		if err := before(ctx, api, obj, parentID, id); err != nil && !cfpkg.IsNotFound(err) {
			return retryCleanup(err)
		}
	}
	err = r.kind.delete(ctx, api, obj, parentID, id)
	for _, blocker := range r.kind.deletionBlockers {
		if cfpkg.HasErrorCode(err, blocker.code) {
			return notReadyRetryAfter(ctx, r.Client, obj, &conditionError{
				Reason: blocker.reason,
				Message: fmt.Sprintf("%s %q %s, or set the %s: %s annotation to keep it", r.kind.noun,
					r.kind.key(obj), blocker.message, reconciler.DeletionPolicyAnnotation,
					reconciler.DeletionPolicyRetain),
			})
		}
	}
	if err != nil && !cfpkg.IsNotFound(err) {
		return retryCleanup(err)
	}
	return ctrl.Result{}, nil
}

// deletionParent returns where a resource's object is deleted from. A zone's
// kind's Zone waits for its resources before it is deleted, so it is normally
// still there; if it is gone anyway (its finalizer was removed by hand), there
// are no credentials to delete with, the object stays in Cloudflare and gone
// is set. A parent outside what the installation watches ends the same way,
// with a Warning Event (leaveCloudflareObject), and so does a Zone in another
// namespace that no longer allows the resource's namespace and hostname.
func (r *crudResourceReconciler[O, A, R, L]) deletionParent(ctx context.Context, obj O) (from deletionParent,
	gone bool, err error) {
	from.usedFrom = obj.GetNamespace()
	if account := r.kind.hooks.account; account != nil {
		name, gone, condErr := account(ctx, r.Client, obj)
		if condErr != nil && condErr.Reason == reasonNotWatched {
			return from, true, leaveCloudflareObject(ctx, obj, condErr)
		}
		if condErr != nil && !gone {
			return from, false, condErr
		}
		from.accountName = name
		return from, gone, nil
	}
	if r.kind.zoneRef == nil {
		from.accountName = r.kind.accountRef(obj)
		return from, false, nil
	}
	zone := &cloudflarev1alpha1.Zone{}
	key := r.zoneKey(obj)
	err = r.Get(ctx, key, zone)
	if condErr := outsideReference(ctx, key, zone, zoneLabel(obj, key), err); condErr != nil {
		return from, true, leaveCloudflareObject(ctx, obj, condErr)
	}
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).Info("Zone is gone, skipping Cloudflare deletion", "zone", key.String())
			return from, true, nil
		}
		return from, false, err
	}
	if key.Namespace != obj.GetNamespace() {
		if condErr := checkZoneReference(ctx, r.Client, zone, obj.GetNamespace(), r.kind.hostname(obj)); condErr != nil {
			return from, true, leaveCloudflareObject(ctx, obj, condErr)
		}
		from.sharedZone = zone
	}
	// A reconcile that failed before it wrote status recorded no zone: the
	// Zone still names it.
	from.zoneID = *r.kind.statusZoneID(obj)
	if from.zoneID == "" {
		from.zoneID = zone.Status.CloudflareMetadata.ZoneID
	}
	from.accountName, from.usedFrom = zone.Spec.AccountRef.Name, zone.Namespace
	return from, false, nil
}

// setup registers the kind's controller, with rec as its reconciler. Each
// watch re-triggers a resource waiting on what it watches: its account or
// Zone, its namespace's labels (allowedNamespaces, or a shared Zone's
// allowedReferences), the kinds it references, its dependents' deletions,
// and its own kind's deletions (a freed object) and creations (a successor).
func (r *crudResourceReconciler[O, A, R, L]) setup(mgr ctrl.Manager, rec reconcile.Reconciler) error {
	b := ctrl.NewControllerManagedBy(mgr).For(r.kind.newObject())
	switch {
	case r.kind.zoneRef != nil:
		b = b.Watches(&cloudflarev1alpha1.Zone{}, handler.EnqueueRequestsFromMapFunc(r.resourcesForZone))
		if r.kind.zoneNamespace != nil {
			b = b.Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.resourcesOfSharedZones),
				builder.WithPredicates(predicate.LabelChangedPredicate{}))
		}
	case r.kind.accountRef != nil:
		b = b.Watches(&cloudflarev1alpha1.CloudflareAccount{}, handler.EnqueueRequestsFromMapFunc(r.resourcesForAccount))
		b = b.Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.resourcesInNamespace),
			builder.WithPredicates(predicate.LabelChangedPredicate{}))
	}
	for _, ref := range r.references() {
		b = b.Watches(ref.newObject(), handler.EnqueueRequestsFromMapFunc(r.resourcesReferencing(ref)))
	}
	for _, dependent := range r.kind.dependents {
		b = b.Watches(dependent.newObject(), handler.EnqueueRequestsFromMapFunc(r.deletingResourcesOf),
			builder.WithPredicates(deletesOnly))
	}
	if !r.kind.sharedObjects {
		b = b.Watches(r.kind.newObject(), handler.EnqueueRequestsFromMapFunc(r.resourcesInConflict),
			builder.WithPredicates(deletesOnly))
		b = b.Watches(r.kind.newObject(), handler.EnqueueRequestsFromMapFunc(r.resourcesBeingDeleted),
			builder.WithPredicates(createsOnly))
	}
	if r.kind.hooks.watch != nil {
		b = r.kind.hooks.watch(b, r.Client)
	}
	return b.Complete(withControllerName(r.kind.name, rec))
}

// resourcesForAccount maps a CloudflareAccount event to the kind's resources
// that use it.
func (r *crudResourceReconciler[O, A, R, L]) resourcesForAccount(ctx context.Context, account client.Object) []reconcile.Request {
	return r.resourcesWhere(ctx, nil, func(obj O) bool { return r.kind.accountRef(obj) == account.GetName() })
}

// resourcesInNamespace maps a change to a Namespace's labels to the kind's
// resources there whose account's spec.allowedNamespaces could care. Without
// such an account, relabelling a namespace costs no Cloudflare call.
func (r *crudResourceReconciler[O, A, R, L]) resourcesInNamespace(ctx context.Context,
	namespace client.Object) []reconcile.Request {
	accounts := &cloudflarev1alpha1.CloudflareAccountList{}
	if err := r.List(ctx, accounts); err != nil {
		return nil
	}
	restricted := map[string]bool{}
	for i := range accounts.Items {
		restricted[accounts.Items[i].Name] = restrictsNamespaces(&accounts.Items[i])
	}
	return r.resourcesWhere(ctx, []client.ListOption{client.InNamespace(namespace.GetName())},
		func(obj O) bool { return restricted[r.kind.accountRef(obj)] })
}

// resourcesForZone maps a Zone event to the kind's resources in its zone:
// those in its namespace, and, for a kind whose resources may name a Zone in
// another namespace, those in any namespace that name it.
func (r *crudResourceReconciler[O, A, R, L]) resourcesForZone(ctx context.Context, zone client.Object) []reconcile.Request {
	var opts []client.ListOption
	if r.kind.zoneNamespace == nil {
		opts = append(opts, client.InNamespace(zone.GetNamespace()))
	}
	return r.resourcesWhere(ctx, opts, func(obj O) bool { return r.zoneKey(obj) == client.ObjectKeyFromObject(zone) })
}

// resourcesOfSharedZones maps a change to a Namespace's labels to the kind's
// resources there that name a Zone in another namespace, whose
// spec.allowedReferences may pick namespaces by their labels. A resource of
// its own namespace's Zone has nothing to ask the labels.
func (r *crudResourceReconciler[O, A, R, L]) resourcesOfSharedZones(ctx context.Context,
	namespace client.Object) []reconcile.Request {
	return r.resourcesWhere(ctx, []client.ListOption{client.InNamespace(namespace.GetName())},
		func(obj O) bool { return r.zoneKey(obj).Namespace != obj.GetNamespace() })
}

// resourcesReferencing returns what maps an event of a resource of the kind
// ref refers to to the kind's resources that reference it.
func (r *crudResourceReconciler[O, A, R, L]) resourcesReferencing(ref crudReference[O]) handler.MapFunc {
	return func(ctx context.Context, target client.Object) []reconcile.Request {
		return r.resourcesWhere(ctx, []client.ListOption{client.InNamespace(target.GetNamespace())},
			func(obj O) bool { return ref.name(obj) == target.GetName() })
	}
}

// resourcesInConflict maps the deletion of a resource of the kind to the
// kind's resources that report its conflict reason, in every namespace: one of
// them may have waited for the deleted resource's object, which is now its
// own (handOff), or free to adopt or to create again. Without the watch it
// would only find out on its retry, a minute later.
func (r *crudResourceReconciler[O, A, R, L]) resourcesInConflict(ctx context.Context, _ client.Object) []reconcile.Request {
	return r.resourcesWhere(ctx, nil, func(obj O) bool {
		ready := meta.FindStatusCondition(obj.GetConditions(), cloudflarev1alpha1.ConditionReady)
		return ready != nil && ready.Reason == r.kind.conflictReason
	})
}

// createsOnly passes only create events.
var createsOnly = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return true },
	UpdateFunc:  func(event.UpdateEvent) bool { return false },
	DeleteFunc:  func(event.DeleteEvent) bool { return false },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// resourcesBeingDeleted maps the creation of a resource of the kind to the
// kind's resources in its namespace that are being deleted: one of them may
// keep its object for a successor (successorWait), which created may be.
// Without the watch it would hand the object on only when its wait is over. A
// resource older than the rename grace period, as every one is that a
// starting manager lists, is one that any wait began with.
func (r *crudResourceReconciler[O, A, R, L]) resourcesBeingDeleted(ctx context.Context,
	created client.Object) []reconcile.Request {
	if time.Since(created.GetCreationTimestamp().Time) > reconciler.RenameGracePeriod() {
		return nil
	}
	return r.resourcesWhere(ctx, []client.ListOption{client.InNamespace(created.GetNamespace())},
		func(obj O) bool { return !obj.GetDeletionTimestamp().IsZero() })
}

// resourcesWhere returns a request for each of the kind's resources that opts
// list and match accepts.
func (r *crudResourceReconciler[O, A, R, L]) resourcesWhere(ctx context.Context, opts []client.ListOption,
	match func(O) bool) []reconcile.Request {
	return reconciler.RequestsWhere(ctx, r.Client, r.kind.newList(), opts, match)
}

// deletingResourcesOf maps the deletion of a dependent to the kind's resources
// it uses that are themselves being deleted.
func (r *crudResourceReconciler[O, A, R, L]) deletingResourcesOf(ctx context.Context, dependent client.Object) []reconcile.Request {
	return deletingParentOf(ctx, r.Client, dependent, func() client.Object { return r.kind.newObject() }, true,
		r.kind.dependents)
}
