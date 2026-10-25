/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"

	"github.com/cloudflare/cloudflare-go/v7/packages/pagination"
	"k8s.io/apimachinery/pkg/api/meta"
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

// A CRUD kind's resources each manage one object in a Cloudflare account that
// the API creates, reads, updates and deletes by ID: KVNamespace, and the
// kinds kflare-gen is to generate. They reconcile the same way. A resource
// resolves its account's token and reads its object by the ID in its status,
// creating a new one if it was deleted outside kflare. Without one, it creates
// one, or, as its adoption policy allows (adoption.go), adopts the account's
// object with the same key, unless another resource of the kind claims it.
// It claims the object, updates it when it differs from the spec, and deletes
// it with the resource once nothing depends on it, unless it is retained.
// A kind supplies only what differs: a crudResourceKind, which calls the
// kind's cloudflare-go v7 service.

// crudResourceKind is what the shared CRUD reconciler needs to know about a
// kind. O is the kind's API type, A the narrow interface to the
// cloudflare-go v7 service it calls, R the Cloudflare object that service
// returns, and L the object as a page of its list shows it: R itself for a
// KV namespace, a shorter type for a D1 database.
type crudResourceKind[O conditionedObject, A any, R any, L any] struct {
	// name is the kind's name, and noun what messages call its Cloudflare
	// object, as in "KVNamespace" and "KV namespace".
	name, noun string
	newObject  func() O
	newList    func() client.ObjectList

	// accountRef returns the name of the CloudflareAccount a resource uses.
	accountRef func(O) string
	// key returns the value a resource adopts an existing object by, such as
	// a title that no two objects in an account share.
	key func(O) string
	// claimLabel holds the ID of the object a resource manages. A resource
	// does not adopt an object that another one claims: it reports
	// conflictReason instead.
	claimLabel, conflictReason string
	// statusID returns the ID of the object recorded in a resource's status,
	// and observe records an object there, its ID included.
	statusID func(O) string
	observe  func(O, *R)

	// id returns an object's ID, and matches whether the object is what a
	// resource's spec asks for. listedID and listedKey return the ID and the
	// key of an object a page of the list shows.
	id        func(*R) string
	matches   func(O, *R) bool
	listedID  func(*L) string
	listedKey func(*L) string

	// The kind's calls to Cloudflare, in the account accountID. list returns
	// one page of the account's objects, counting from 1.
	get    func(ctx context.Context, api A, accountID, id string) (*R, error)
	list   func(ctx context.Context, api A, accountID string, page int64) (*pagination.V4PagePaginationArray[L], error)
	create func(ctx context.Context, api A, accountID string, obj O) (*R, error)
	update func(ctx context.Context, api A, accountID, id string, obj O) (*R, error)
	delete func(ctx context.Context, api A, accountID, id string) error

	// dependents use a resource of the kind, which waits for them before it is
	// deleted.
	dependents []dependentKind
}

// crudResourceReconciler reconciles the resources of one CRUD kind. Each
// kind's exported reconciler delegates to one.
type crudResourceReconciler[O conditionedObject, A any, R any, L any] struct {
	client.Client
	kind   crudResourceKind[O, A, R, L]
	newAPI func(token string) (A, error)
}

func (r *crudResourceReconciler[O, A, R, L]) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	obj := r.kind.newObject()
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion before anything else.
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

	account, token, credErr := resolveAccountToken(ctx, r.Client, r.kind.accountRef(obj), true)
	if credErr != nil {
		return notReadyRetryAfter(ctx, r.Client, obj, credErr)
	}
	api, err := r.newAPI(token)
	if err != nil {
		return invalidToken(ctx, r.Client, obj, err)
	}

	return r.sync(ctx, obj, account.Spec.AccountID, api)
}

// sync drives the desired→observed→delta→reconcile loop for one resource.
func (r *crudResourceReconciler[O, A, R, L]) sync(ctx context.Context, obj O, accountID string, api A) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	k := r.kind

	// Observe the object by the ID in status. If it is gone, it was deleted
	// outside kflare; a new one is created.
	var current *R
	if id := k.statusID(obj); id != "" {
		got, err := k.get(ctx, api, accountID, id)
		switch {
		case cfpkg.IsNotFound(err):
			logger.Info(k.noun+" deleted outside kflare, recreating", "id", id)
		case err != nil:
			return handleCloudflareError(ctx, r.Client, obj, err)
		default:
			current = got
		}
	}

	// No object yet: find the account's object with this key. It is this
	// resource's own when its claim or its creating annotation says so (an
	// earlier reconcile did not record it in status); another resource of the
	// kind may claim it; otherwise the resource adopts it only if its adoption
	// policy allows. Without one, it creates the object, unless the policy is
	// adopt. An object found in the list is read by its ID, for all that the
	// list leaves out.
	adopting := false
	if current == nil {
		key := k.key(obj)
		what := fmt.Sprintf("%s %q", k.noun, key)
		existing, err := findInPages(func(page int64) (*pagination.V4PagePaginationArray[L], error) {
			return k.list(ctx, api, accountID, page)
		}, func(listed *L) bool { return k.listedKey(listed) == key })
		if err != nil {
			return handleCloudflareError(ctx, r.Client, obj, err)
		}
		if existing != nil {
			id := k.listedID(existing)
			claimed, err := claimedIDs(ctx, r.Client, k.newList(), k.claimLabel, obj)
			if err != nil {
				return ctrl.Result{}, err
			}
			if owner, taken := claimed[id]; taken {
				return notReadyRetryAfter(ctx, r.Client, obj, &conditionError{
					Reason:  k.conflictReason,
					Message: fmt.Sprintf("%s is already managed by %s %s", what, k.name, owner),
				})
			}
			own := ownsID(obj, k.claimLabel, id) || creating(obj, key)
			if !own {
				if condErr := mayAdopt(obj, what); condErr != nil {
					return notReadyRetryAfter(ctx, r.Client, obj, condErr)
				}
			}
			found, err := k.get(ctx, api, accountID, id)
			switch {
			case cfpkg.IsNotFound(err):
				logger.Info(k.noun+" deleted since it was listed", "key", key, "id", id)
			case err != nil:
				return handleCloudflareError(ctx, r.Client, obj, err)
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
				return notReadyRetryAfter(ctx, r.Client, obj, condErr)
			}
			if err := markCreating(ctx, r.Client, obj, key); err != nil {
				return ctrl.Result{}, err
			}
			current, err = k.create(ctx, api, accountID, obj)
			if err != nil {
				return handleCloudflareError(ctx, r.Client, obj, err)
			}
			logger.Info("Created "+k.noun, "key", key, "id", k.id(current))
		}
	}

	// Claim the object before anything else can fail, so no other resource of
	// the kind adopts it in the meantime.
	if err := claimID(ctx, r.Client, obj, k.claimLabel, k.id(current), adopting); err != nil {
		return ctrl.Result{}, err
	}

	// Drift: update the object in place when it is not what the spec asks for.
	if !k.matches(obj, current) {
		updated, err := k.update(ctx, api, accountID, k.id(current), obj)
		if err != nil {
			return handleCloudflareError(ctx, r.Client, obj, err)
		}
		current = updated
		logger.Info("Updated "+k.noun, "id", k.id(current))
	}

	k.observe(obj, current)
	return readyWithDriftCheck(ctx, r.Client, obj, "Synced", k.noun+" is synced with Cloudflare")
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
// set), then removes the finalizer.
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

	if id := r.kind.statusID(obj); id != "" && !reconciler.RetainOnDelete(obj) {
		account, token, credErr := resolveAccountToken(ctx, r.Client, r.kind.accountRef(obj), false)
		if credErr != nil {
			return ctrl.Result{}, credErr
		}
		api, err := r.newAPI(token)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.kind.delete(ctx, api, account.Spec.AccountID, id); err != nil && !cfpkg.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// Remove the finalizer to allow Kubernetes to complete the deletion.
	_, err = reconciler.RemoveFinalizer(ctx, r.Client, obj)
	return ctrl.Result{}, err
}

// setup registers the kind's controller, with rec as its reconciler. It
// watches CloudflareAccounts, so a resource waiting for its account is
// reconciled once the account is ready, and the deletion of the kind's
// dependents, so a resource waiting for them is released once they are gone.
func (r *crudResourceReconciler[O, A, R, L]) setup(mgr ctrl.Manager, rec reconcile.Reconciler) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(r.kind.newObject()).
		Watches(&cloudflarev1alpha1.CloudflareAccount{}, handler.EnqueueRequestsFromMapFunc(r.resourcesForAccount))
	for _, dependent := range r.kind.dependents {
		b = b.Watches(dependent.newObject(), handler.EnqueueRequestsFromMapFunc(r.deletingResourcesOf),
			builder.WithPredicates(deletesOnly))
	}
	return b.Complete(rec)
}

// resourcesForAccount maps a CloudflareAccount event to the kind's resources
// that use it.
func (r *crudResourceReconciler[O, A, R, L]) resourcesForAccount(ctx context.Context, account client.Object) []reconcile.Request {
	list := r.kind.newList()
	if err := r.List(ctx, list); err != nil {
		return nil
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, item := range items {
		if obj, ok := item.(O); ok && r.kind.accountRef(obj) == account.GetName() {
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
