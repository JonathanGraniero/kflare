/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"

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

// KVNamespaceAPI is the subset of the Cloudflare API used by this controller.
// Declaring a narrow interface keeps unit tests simple: tests inject a fake
// that implements only these methods. In production, *cfpkg.Client satisfies
// this interface because it embeds *cf.API.
//
// cloudflare-go v0.89 has no call to read one namespace, so the controller
// lists them (the SDK pages through all of them) and matches by ID.
type KVNamespaceAPI interface {
	CreateWorkersKVNamespace(ctx context.Context, rc *cf.ResourceContainer, params cf.CreateWorkersKVNamespaceParams) (cf.WorkersKVNamespaceResponse, error)
	ListWorkersKVNamespaces(ctx context.Context, rc *cf.ResourceContainer, params cf.ListWorkersKVNamespacesParams) ([]cf.WorkersKVNamespace, *cf.ResultInfo, error)
	UpdateWorkersKVNamespace(ctx context.Context, rc *cf.ResourceContainer, params cf.UpdateWorkersKVNamespaceParams) (cf.Response, error)
	DeleteWorkersKVNamespace(ctx context.Context, rc *cf.ResourceContainer, namespaceID string) (cf.Response, error)
}

// KVNamespaceReconciler reconciles a KVNamespace object.
type KVNamespaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewKVNamespaceAPI constructs a KVNamespaceAPI from a raw API token.
	// Defaults to defaultKVNamespaceAPI; overridden in tests to inject a fake.
	NewKVNamespaceAPI func(token string) (KVNamespaceAPI, error)
}

//+kubebuilder:rbac:groups=kflare.dev,resources=kvnamespaces,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kflare.dev,resources=kvnamespaces/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kflare.dev,resources=kvnamespaces/finalizers,verbs=update
//+kubebuilder:rbac:groups=kflare.dev,resources=cloudflareaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups=kflare.dev,resources=workerscripts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *KVNamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	kv := &cloudflarev1alpha1.KVNamespace{}
	if err := r.Get(ctx, req.NamespacedName, kv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion before anything else.
	if !kv.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, kv)
	}

	// Ensure the finalizer is present before doing any work.
	// On the very first reconcile the finalizer is absent — add it and return.
	// controller-runtime will re-enqueue the object once the update is visible.
	added, err := reconciler.EnsureFinalizer(ctx, r.Client, kv)
	if err != nil {
		return ctrl.Result{}, err
	}
	if added {
		logger.Info("Added finalizer", "kvnamespace", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	account, token, credErr := resolveAccountToken(ctx, r.Client, kv.Spec.AccountRef.Name, true)
	if credErr != nil {
		return notReadyRetryAfter(ctx, r.Client, kv, credErr)
	}

	cfAPI, err := r.newKVNamespaceAPI(token)
	if err != nil {
		return invalidToken(ctx, r.Client, kv, err)
	}

	return r.syncNamespace(ctx, kv, account, cfAPI)
}

// syncNamespace drives the desired→observed→delta→reconcile loop for a KVNamespace.
func (r *KVNamespaceReconciler) syncNamespace(
	ctx context.Context,
	kv *cloudflarev1alpha1.KVNamespace,
	account *cloudflarev1alpha1.CloudflareAccount,
	cfAPI KVNamespaceAPI,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	rc := cf.AccountIdentifier(account.Spec.AccountID)

	namespaces, _, err := cfAPI.ListWorkersKVNamespaces(ctx, rc, cf.ListWorkersKVNamespacesParams{})
	if err != nil {
		return handleCloudflareError(ctx, r.Client, kv, err)
	}

	// Observe the namespace by the ID in status. If it is gone, it was deleted
	// outside kflare together with its data; a new, empty one is created.
	current, found := kvNamespaceWithID(namespaces, kv.Status.CloudflareMetadata.NamespaceID)
	if !found && kv.Status.CloudflareMetadata.NamespaceID != "" {
		logger.Info("KV namespace deleted outside kflare, recreating",
			"namespaceID", kv.Status.CloudflareMetadata.NamespaceID)
	}

	// No namespace yet: adopt the one with this title (titles are unique in
	// an account) unless another KVNamespace manages it, or create one.
	if !found {
		claimed, err := claimedIDs(ctx, r.Client, &cloudflarev1alpha1.KVNamespaceList{},
			cloudflarev1alpha1.KVNamespaceIDLabel, kv)
		if err != nil {
			return ctrl.Result{}, err
		}
		if existing, ok := kvNamespaceWithTitle(namespaces, kv.Spec.Title); ok {
			if owner, taken := claimed[existing.ID]; taken {
				return notReadyRetryAfter(ctx, r.Client, kv, &conditionError{
					Reason:  "TitleConflict",
					Message: fmt.Sprintf("KV namespace %q is already managed by KVNamespace %s", kv.Spec.Title, owner),
				})
			}
			current = existing
			logger.Info("Adopted existing KV namespace", "title", existing.Title, "namespaceID", existing.ID)
		} else {
			created, err := cfAPI.CreateWorkersKVNamespace(ctx, rc, cf.CreateWorkersKVNamespaceParams{Title: kv.Spec.Title})
			if err != nil {
				return handleCloudflareError(ctx, r.Client, kv, err)
			}
			current = created.Result
			logger.Info("Created KV namespace", "title", current.Title, "namespaceID", current.ID)
		}
	}

	// Claim the namespace before anything else can fail, so no other
	// KVNamespace adopts it in the meantime.
	if err := claimID(ctx, r.Client, kv, cloudflarev1alpha1.KVNamespaceIDLabel, current.ID); err != nil {
		return ctrl.Result{}, err
	}

	// Drift: the title. A rename keeps the namespace and its data.
	if current.Title != kv.Spec.Title {
		if _, err := cfAPI.UpdateWorkersKVNamespace(ctx, rc, cf.UpdateWorkersKVNamespaceParams{
			NamespaceID: current.ID,
			Title:       kv.Spec.Title,
		}); err != nil {
			return handleCloudflareError(ctx, r.Client, kv, err)
		}
		logger.Info("Renamed KV namespace", "namespaceID", current.ID, "from", current.Title, "to", kv.Spec.Title)
	}

	kv.Status.CloudflareMetadata.NamespaceID = current.ID
	return ctrl.Result{}, updateReady(ctx, r.Client, kv, "Synced", "KV namespace is synced with Cloudflare")
}

// kvNamespaceWithID returns the namespace with id, which must not be empty.
func kvNamespaceWithID(namespaces []cf.WorkersKVNamespace, id string) (cf.WorkersKVNamespace, bool) {
	if id == "" {
		return cf.WorkersKVNamespace{}, false
	}
	for _, ns := range namespaces {
		if ns.ID == id {
			return ns, true
		}
	}
	return cf.WorkersKVNamespace{}, false
}

// kvNamespaceWithTitle returns the namespace titled title.
func kvNamespaceWithTitle(namespaces []cf.WorkersKVNamespace, title string) (cf.WorkersKVNamespace, bool) {
	for _, ns := range namespaces {
		if ns.Title == title {
			return ns, true
		}
	}
	return cf.WorkersKVNamespace{}, false
}

// reconcileDelete handles the deletion lifecycle. Once no WorkerScript binds
// the namespace, it deletes the namespace and all its data from Cloudflare
// (unless the retain policy is set), then removes the finalizer.
//
// Cloudflare deletes a namespace that a Worker is still bound to, but every
// later upload of that Worker then fails, so bound WorkerScripts go first.
func (r *KVNamespaceReconciler) reconcileDelete(ctx context.Context, kv *cloudflarev1alpha1.KVNamespace) (ctrl.Result, error) {
	// Safety check: if the finalizer is already gone, there is nothing to do.
	if !controllerutil.ContainsFinalizer(kv, reconciler.Finalizer) {
		return ctrl.Result{}, nil
	}

	dependents, err := dependentsOf(ctx, r.Client, kv.Namespace, kv.Name, kvNamespaceDependents)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(dependents) > 0 {
		return ctrl.Result{}, reportInUse(ctx, r.Client, kv, dependents)
	}

	namespaceID := kv.Status.CloudflareMetadata.NamespaceID
	if namespaceID != "" && !reconciler.RetainOnDelete(kv) {
		account, token, credErr := resolveAccountToken(ctx, r.Client, kv.Spec.AccountRef.Name, false)
		if credErr != nil {
			return ctrl.Result{}, credErr
		}
		cfAPI, err := r.newKVNamespaceAPI(token)
		if err != nil {
			return ctrl.Result{}, err
		}
		_, err = cfAPI.DeleteWorkersKVNamespace(ctx, cf.AccountIdentifier(account.Spec.AccountID), namespaceID)
		if err != nil && !cfpkg.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// Remove the finalizer to allow Kubernetes to complete the deletion.
	_, err = reconciler.RemoveFinalizer(ctx, r.Client, kv)
	return ctrl.Result{}, err
}

// SetupWithManager registers KVNamespaceReconciler with the manager. It
// watches CloudflareAccounts, so a namespace waiting for its account is
// reconciled once the account is ready, and deleted WorkerScripts, so a
// namespace waiting for the Workers bound to it is released.
func (r *KVNamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudflarev1alpha1.KVNamespace{}).
		Watches(&cloudflarev1alpha1.CloudflareAccount{}, handler.EnqueueRequestsFromMapFunc(r.namespacesForAccount)).
		Watches(&cloudflarev1alpha1.WorkerScript{},
			handler.EnqueueRequestsFromMapFunc(r.deletingKVNamespacesOf), builder.WithPredicates(deletesOnly)).
		Complete(r)
}

// namespacesForAccount maps a CloudflareAccount event to the KVNamespaces that use it.
func (r *KVNamespaceReconciler) namespacesForAccount(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &cloudflarev1alpha1.KVNamespaceList{}
	if err := r.List(ctx, list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, kv := range list.Items {
		if kv.Spec.AccountRef.Name == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: kv.Name, Namespace: kv.Namespace}})
		}
	}
	return reqs
}

// deletingKVNamespacesOf maps the deletion of a WorkerScript to the
// KVNamespaces it binds that are themselves being deleted.
func (r *KVNamespaceReconciler) deletingKVNamespacesOf(ctx context.Context, obj client.Object) []reconcile.Request {
	return deletingParentOf(ctx, r.Client, obj, func() client.Object { return &cloudflarev1alpha1.KVNamespace{} },
		true, kvNamespaceDependents)
}

// newKVNamespaceAPI builds a KVNamespaceAPI with the injected factory,
// falling back to the production client.
func (r *KVNamespaceReconciler) newKVNamespaceAPI(token string) (KVNamespaceAPI, error) {
	if r.NewKVNamespaceAPI != nil {
		return r.NewKVNamespaceAPI(token)
	}
	return defaultKVNamespaceAPI(token)
}

// defaultKVNamespaceAPI is the production factory: it delegates to cfpkg.New
// so that the returned *cfpkg.Client (which embeds *cf.API) satisfies
// KVNamespaceAPI.
func defaultKVNamespaceAPI(token string) (KVNamespaceAPI, error) {
	return cfpkg.New(token)
}
