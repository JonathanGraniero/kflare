/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"slices"

	"github.com/cloudflare/cloudflare-go/v7/packages/pagination"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
// nothing depends on it, unless it is retained. A kind supplies only what
// differs: a crudResourceKind, which calls the kind's cloudflare-go v7
// service.
//
// Some objects have no ID of their own: their key is their ID, as an R2
// bucket's name is. Such a kind reads the object by its key, where another
// finds the object in its list: in its pages, or in the one page of a list
// that is never paged, as a zone's Worker routes are.
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

	// id returns an object's ID, and matches whether the object is what a
	// resource's spec asks for, given what its references give (refs).
	// listedID and listedKey return the ID and the key of an object a page of
	// the list shows.
	id        func(*R) string
	matches   func(obj O, current *R, refs map[string]string) bool
	listedID  func(*L) string
	listedKey func(*L) string

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
	// matches replaces the comparison of the object with the spec.
	matches func(obj O, current *R, refs map[string]string) bool
	// synced runs once the object is as the spec asks and status records it.
	// It does what the service's calls cannot, such as an IPList's items. Set
	// stop to end the reconcile with what it returns, rather than report the
	// resource ready.
	synced func(ctx context.Context, c client.Client, api A, obj O, parentID string, current *R) (result ctrl.Result,
		stop bool, err error)
	// beforeDelete runs before the object is deleted from Cloudflare, with the
	// retain policy unset. An object that is already gone is no failure.
	beforeDelete func(ctx context.Context, api A, obj O, parentID, id string) error
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
	// Client reads from (see upToDate).
	apiReader client.Reader
	kind      crudResourceKind[O, A, R, L]
	newAPI    func(token string) (A, error)
}

func (r *crudResourceReconciler[O, A, R, L]) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	obj := r.kind.newObject()
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A paused resource is left as it is, deletion included, until its
	// kflare.dev/paused annotation is removed.
	if reconciler.Paused(obj) {
		return ctrl.Result{}, updatePaused(ctx, r.Client, obj)
	}

	// Then handle deletion, before anything else.
	if !obj.GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, obj)
	}

	// Ensure the finalizer is present before doing any work. On the first
	// reconcile it is absent: add it and return; the update re-triggers this
	// reconcile.
	added, err := reconciler.EnsureFinalizer(ctx, r.Client, obj)
	if err != nil {
		return ctrl.Result{}, err
	}
	if added {
		logger.Info("Added finalizer", "resource", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// A zone's kind waits for its Zone (the Zone watch re-triggers this
	// reconcile when it is ready) and calls Cloudflare with its account.
	accountName, whose := "", "this "+r.kind.name
	var zone *cloudflarev1alpha1.Zone
	if r.kind.zoneRef != nil {
		var condErr *conditionError
		if zone, condErr = r.zone(ctx, obj); condErr != nil {
			return ctrl.Result{}, updateNotReady(ctx, r.Client, obj, condErr.Reason, condErr.Message)
		}
		accountName, whose = zone.Spec.AccountRef.Name, fmt.Sprintf("Zone %q", zone.Name)
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

	account, token, credErr := resolveAccountToken(ctx, r.Client, obj.GetNamespace(), accountName, true)
	if credErr != nil {
		return notReadyRetryAfter(ctx, r.Client, obj, credErr)
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
	if zone != nil {
		parentID = zone.Status.CloudflareMetadata.ZoneID
	}
	return r.sync(ctx, obj, parentID, refs, api)
}

// zone returns the ready Zone a resource of a zone's kind belongs to, or why
// the resource waits for it.
func (r *crudResourceReconciler[O, A, R, L]) zone(ctx context.Context, obj O) (*cloudflarev1alpha1.Zone, *conditionError) {
	zone := &cloudflarev1alpha1.Zone{}
	key := types.NamespacedName{Name: r.kind.zoneRef(obj), Namespace: obj.GetNamespace()}
	if err := r.Get(ctx, key, zone); err != nil {
		return nil, &conditionError{Reason: "ZoneNotFound", Message: fmt.Sprintf("Zone %q not found: %v", key.Name, err)}
	}
	if !isZoneReady(zone) {
		return nil, &conditionError{Reason: "ZoneNotReady", Message: fmt.Sprintf("Zone %q is not ready", key.Name)}
	}
	return zone, nil
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
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: obj.GetNamespace()}, target); err != nil {
			return nil, &conditionError{
				Reason:  ref.kind + "NotFound",
				Message: fmt.Sprintf("%s %q not found: %v", ref.kind, name, err),
			}
		}
		if account := ref.account(target); account != accountName {
			return nil, &conditionError{
				Reason: reasonAccountMismatch,
				Message: fmt.Sprintf("%s %q uses CloudflareAccount %q but %s uses %q; %s can only use %s of its "+
					"own account", ref.kind, name, account, whose, accountName, r.kind.noun, ref.kind),
			}
		}
		if !meta.IsStatusConditionTrue(target.GetConditions(), cloudflarev1alpha1.ConditionReady) {
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
// the account or zone parentID.
func (r *crudResourceReconciler[O, A, R, L]) sync(ctx context.Context, obj O, parentID string,
	refs map[string]string, api A) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	k := r.kind

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
			current = got
		}
	}

	// Without an object, the resource finds, adopts or creates one, unless
	// it is a stale read (upToDate).
	adopting := false
	if current == nil {
		if fresh, err := r.upToDate(ctx, obj); !fresh || err != nil {
			return ctrl.Result{}, err
		}
		found, adopted, done, result, err := r.findOrCreate(ctx, obj, parentID, refs, api)
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
		if fresh, err := r.upToDate(ctx, obj); !fresh || err != nil {
			return ctrl.Result{}, err
		}
		updated, err := k.update(ctx, api, obj, parentID, k.id(current), refs)
		if err != nil {
			return handleCloudflareError(ctx, r.Client, obj, err)
		}
		current = updated
		logger.Info("Updated "+k.noun, "id", k.id(current))
	}

	if k.statusZoneID != nil {
		*k.statusZoneID(obj) = parentID
	}
	k.observe(obj, current)
	if k.hooks.synced != nil {
		if result, stop, err := k.hooks.synced(ctx, r.Client, api, obj, parentID, current); stop {
			return result, err
		}
	}
	return readyWithDriftCheck(ctx, r.Client, obj, "Synced", k.noun+" is synced with Cloudflare")
}

// upToDate reports whether obj, as the manager's cache gives it, is the
// resource the API server holds. sync asks before it writes to Cloudflare on
// what the resource says: before it finds, adopts or creates an object, and
// before it updates one. A reconcile's own writes (the creating mark, the
// claim) queue the next reconcile, as other events do while it runs, and the
// status it writes last may not have reached the cache when that one reads
// the resource. Reading no ID, or one of an object it has just recreated, the
// next reconcile would find, adopt or create an object again; reading a
// WorkerScript's previous hash, it would upload the Worker again. A newer
// version, or a resource already gone, means the cache is behind: the
// reconcile stops, and the watch event that brings the cache up to date
// reconciles the resource again.
func (r *crudResourceReconciler[O, A, R, L]) upToDate(ctx context.Context, obj O) (bool, error) {
	current := r.kind.newObject()
	if err := r.apiReader.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
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
// the parent's object with its key, which is the resource's own when its claim
// or its creating annotation says so (an earlier reconcile did not record it in
// status), or which it adopts, as its adoption policy allows, unless another
// resource of the kind claims it. Without one, it creates the object, unless the
// policy is adopt. An object found in the list is read by its ID, for all that
// the list leaves out. done ends the reconcile with result and err.
func (r *crudResourceReconciler[O, A, R, L]) findOrCreate(ctx context.Context, obj O, parentID string,
	refs map[string]string, api A) (current *R, adopting, done bool, result ctrl.Result, err error) {
	logger := log.FromContext(ctx)
	k := r.kind
	stop := func(result ctrl.Result, err error) (*R, bool, bool, ctrl.Result, error) {
		return nil, false, true, result, err
	}

	key := k.key(obj)
	if k.hooks.key != nil {
		key = k.hooks.key(obj, refs)
	}
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
			if condErr := mayAdopt(obj, what); condErr != nil {
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
		logger.Info("Created "+k.noun, "key", key, "id", k.id(current))
	}
	return current, adopting, false, ctrl.Result{}, nil
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

// claimant returns the "namespace/name" of another resource of the kind that
// claims the object id in the Cloudflare account or zone parentID, at obj's
// address, or "" if none does. Resources in every namespace count, and an ID
// is unique only within its parent (an R2 bucket's name), so a claim counts
// only from a resource whose object is in the same parent (sameParent).
func (r *crudResourceReconciler[O, A, R, L]) claimant(ctx context.Context, obj O, parentID, id string) (string, error) {
	k := r.kind
	if k.sharedObjects {
		return "", nil
	}
	list := k.newList()
	if err := r.List(ctx, list, client.MatchingLabels{k.claimLabel: id}); err != nil {
		return "", err
	}
	items, err := meta.ExtractList(list)
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
		if other.GetNamespace() == obj.GetNamespace() && k.zoneRef(other) == k.zoneRef(obj) {
			return true, nil
		}
		zone := &cloudflarev1alpha1.Zone{}
		err := r.Get(ctx, types.NamespacedName{Name: k.zoneRef(other), Namespace: other.GetNamespace()}, zone)
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
// resource, it deletes the object from Cloudflare (unless the retain policy is
// set), then removes the finalizer. An object that one of the kind's deletion
// blockers keeps is kept with the finalizer, and the resource reports why
// until the delete goes through.
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
	id := r.kind.statusID(obj)
	if id == "" {
		id = obj.GetLabels()[r.kind.claimLabel]
	}
	if id != "" && !reconciler.RetainOnDelete(obj) {
		accountName, zoneID, gone, err := r.deletionParent(ctx, obj)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !gone {
			if result, err := r.deleteObject(ctx, obj, accountName, zoneID, id); err != nil || !result.IsZero() {
				return result, err
			}
		}
	}

	// Remove the finalizer to allow Kubernetes to complete the deletion.
	_, err = reconciler.RemoveFinalizer(ctx, r.Client, obj)
	return ctrl.Result{}, err
}

// deleteObject deletes a resource's object id from Cloudflare, with the token
// of the CloudflareAccount accountName, from its account or, for a zone's
// kind, from the zone zoneID. While one of the kind's deletion blockers keeps
// the object, it reports why and returns a retry.
func (r *crudResourceReconciler[O, A, R, L]) deleteObject(ctx context.Context, obj O, accountName, zoneID,
	id string) (ctrl.Result, error) {
	account, token, credErr := resolveAccountToken(ctx, r.Client, obj.GetNamespace(), accountName, false)
	if credErr != nil {
		return ctrl.Result{}, leaveCloudflareObject(ctx, obj, credErr)
	}
	api, err := r.newAPI(token)
	if err != nil {
		return ctrl.Result{}, err
	}
	parentID := account.Spec.AccountID
	if r.kind.zoneRef != nil {
		parentID = zoneID
	}
	if before := r.kind.hooks.beforeDelete; before != nil {
		if err := before(ctx, api, obj, parentID, id); err != nil && !cfpkg.IsNotFound(err) {
			return ctrl.Result{}, err
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
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// deletionParent returns where a resource's object is deleted from: the
// CloudflareAccount whose token deletes it and, for a zone's kind, the zone
// its status records. A zone's kind's Zone waits for its resources before it
// is deleted, so it is normally still there; if it is gone anyway (its
// finalizer was removed by hand), there are no credentials to delete with,
// the object stays in Cloudflare and gone is set.
func (r *crudResourceReconciler[O, A, R, L]) deletionParent(ctx context.Context, obj O) (accountName, zoneID string,
	gone bool, err error) {
	if account := r.kind.hooks.account; account != nil {
		name, gone, condErr := account(ctx, r.Client, obj)
		if condErr != nil && !gone {
			return "", "", false, condErr
		}
		return name, "", gone, nil
	}
	if r.kind.zoneRef == nil {
		return r.kind.accountRef(obj), "", false, nil
	}
	zone := &cloudflarev1alpha1.Zone{}
	key := types.NamespacedName{Name: r.kind.zoneRef(obj), Namespace: obj.GetNamespace()}
	if err := r.Get(ctx, key, zone); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).Info("Zone is gone, skipping Cloudflare deletion", "zone", key.Name)
			return "", "", true, nil
		}
		return "", "", false, err
	}
	// A reconcile that failed before it wrote status recorded no zone: the
	// Zone still names it.
	zoneID = *r.kind.statusZoneID(obj)
	if zoneID == "" {
		zoneID = zone.Status.CloudflareMetadata.ZoneID
	}
	return zone.Spec.AccountRef.Name, zoneID, false, nil
}

// setup registers the kind's controller, with rec as its reconciler. It
// watches CloudflareAccounts, or Zones for a zone's kind, so a resource
// waiting for its account or Zone is reconciled once it is ready; the labels
// of Namespaces, so a kind that names its account follows what the account's
// spec.allowedNamespaces says of them (a zone's kind follows its Zone); the
// kinds its spec references, so a resource waiting for one is reconciled once
// it is ready; and the deletion of the kind's dependents, so a resource
// waiting for them is released once they are gone.
func (r *crudResourceReconciler[O, A, R, L]) setup(mgr ctrl.Manager, rec reconcile.Reconciler) error {
	b := ctrl.NewControllerManagedBy(mgr).For(r.kind.newObject())
	switch {
	case r.kind.zoneRef != nil:
		b = b.Watches(&cloudflarev1alpha1.Zone{}, handler.EnqueueRequestsFromMapFunc(r.resourcesForZone))
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
	if r.kind.hooks.watch != nil {
		b = r.kind.hooks.watch(b, r.Client)
	}
	return b.Complete(rec)
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

// resourcesForZone maps a Zone event to the kind's resources in its zone.
func (r *crudResourceReconciler[O, A, R, L]) resourcesForZone(ctx context.Context, zone client.Object) []reconcile.Request {
	return r.resourcesWhere(ctx, []client.ListOption{client.InNamespace(zone.GetNamespace())},
		func(obj O) bool { return r.kind.zoneRef(obj) == zone.GetName() })
}

// resourcesReferencing returns what maps an event of a resource of the kind
// ref refers to to the kind's resources that reference it.
func (r *crudResourceReconciler[O, A, R, L]) resourcesReferencing(ref crudReference[O]) handler.MapFunc {
	return func(ctx context.Context, target client.Object) []reconcile.Request {
		return r.resourcesWhere(ctx, []client.ListOption{client.InNamespace(target.GetNamespace())},
			func(obj O) bool { return ref.name(obj) == target.GetName() })
	}
}

// resourcesWhere returns a request for each of the kind's resources that opts
// list and match accepts.
func (r *crudResourceReconciler[O, A, R, L]) resourcesWhere(ctx context.Context, opts []client.ListOption,
	match func(O) bool) []reconcile.Request {
	list := r.kind.newList()
	if err := r.List(ctx, list, opts...); err != nil {
		return nil
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, item := range items {
		if obj, ok := item.(O); ok && match(obj) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)})
		}
	}
	return reqs
}

// deletingResourcesOf maps the deletion of a dependent to the kind's resources
// it uses that are themselves being deleted.
func (r *crudResourceReconciler[O, A, R, L]) deletingResourcesOf(ctx context.Context, dependent client.Object) []reconcile.Request {
	return deletingParentOf(ctx, r.Client, dependent, func() client.Object { return r.kind.newObject() }, true,
		r.kind.dependents)
}
