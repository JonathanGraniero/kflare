/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	cf "github.com/cloudflare/cloudflare-go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// WorkerRouteAPI is the subset of the Cloudflare API used by this controller.
// Declaring a narrow interface keeps unit tests simple: tests inject a fake
// that implements only these methods. In production, *cfpkg.Client satisfies
// this interface because it embeds *cf.API.
type WorkerRouteAPI interface {
	CreateWorkerRoute(ctx context.Context, rc *cf.ResourceContainer, params cf.CreateWorkerRouteParams) (cf.WorkerRouteResponse, error)
	GetWorkerRoute(ctx context.Context, rc *cf.ResourceContainer, routeID string) (cf.WorkerRouteResponse, error)
	ListWorkerRoutes(ctx context.Context, rc *cf.ResourceContainer, params cf.ListWorkerRoutesParams) (cf.WorkerRoutesResponse, error)
	UpdateWorkerRoute(ctx context.Context, rc *cf.ResourceContainer, params cf.UpdateWorkerRouteParams) (cf.WorkerRouteResponse, error)
	DeleteWorkerRoute(ctx context.Context, rc *cf.ResourceContainer, routeID string) (cf.WorkerRouteResponse, error)
}

// WorkerRouteReconciler reconciles a WorkerRoute object.
type WorkerRouteReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewWorkerRouteAPI constructs a WorkerRouteAPI from a raw API token.
	// Defaults to defaultWorkerRouteAPI; overridden in tests to inject a fake.
	NewWorkerRouteAPI func(token string) (WorkerRouteAPI, error)
}

//+kubebuilder:rbac:groups=kflare.dev,resources=workerroutes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kflare.dev,resources=workerroutes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kflare.dev,resources=workerroutes/finalizers,verbs=update
//+kubebuilder:rbac:groups=kflare.dev,resources=zones,verbs=get;list;watch
//+kubebuilder:rbac:groups=kflare.dev,resources=workerscripts,verbs=get;list;watch
//+kubebuilder:rbac:groups=kflare.dev,resources=cloudflareaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *WorkerRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	route := &cloudflarev1alpha1.WorkerRoute{}
	if err := r.Get(ctx, req.NamespacedName, route); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion before anything else.
	if !route.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, route)
	}

	// Ensure the finalizer is present before doing any work.
	// On the very first reconcile the finalizer is absent — add it and return.
	// controller-runtime will re-enqueue the object once the update is visible.
	added, err := reconciler.EnsureFinalizer(ctx, r.Client, route)
	if err != nil {
		return ctrl.Result{}, err
	}
	if added {
		logger.Info("Added finalizer", "workerroute", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// The Zone and WorkerScript watches re-trigger this reconcile when either
	// appears or becomes ready.
	zone := &cloudflarev1alpha1.Zone{}
	zoneKey := types.NamespacedName{Name: route.Spec.ZoneRef.Name, Namespace: route.Namespace}
	if err := r.Get(ctx, zoneKey, zone); err != nil {
		return ctrl.Result{}, updateNotReady(ctx, r.Client, route, "ZoneNotFound",
			fmt.Sprintf("Zone %q not found: %v", zoneKey.Name, err))
	}
	if !isZoneReady(zone) {
		return ctrl.Result{}, updateNotReady(ctx, r.Client, route, "ZoneNotReady",
			fmt.Sprintf("Zone %q is not ready", zoneKey.Name))
	}

	// Cloudflare rejects a pattern outside the zone; say so without a call.
	if !patternInZone(route.Spec.Pattern, zone.Spec.Name) {
		return ctrl.Result{}, updateNotReady(ctx, r.Client, route, "PatternOutsideZone",
			fmt.Sprintf("Pattern %q is not in zone %q", route.Spec.Pattern, zone.Spec.Name))
	}

	script, condErr := r.routedScript(ctx, route, zone)
	if condErr != nil {
		return ctrl.Result{}, updateNotReady(ctx, r.Client, route, condErr.Reason, condErr.Message)
	}

	_, token, credErr := resolveAccountToken(ctx, r.Client, zone.Spec.AccountRef.Name, true)
	if credErr != nil {
		return notReadyRetryAfter(ctx, r.Client, route, credErr)
	}

	cfAPI, err := r.newWorkerRouteAPI(token)
	if err != nil {
		return invalidToken(ctx, r.Client, route, err)
	}

	return r.syncRoute(ctx, route, zone.Status.CloudflareMetadata.ZoneID, script, cfAPI)
}

// routedScript returns the Cloudflare script name the route should send
// requests to: the referenced WorkerScript's, or "" for a route without one.
//
// Cloudflare refuses a route to a Worker that does not exist, so the
// WorkerScript must have uploaded it (be ready). It must also belong to the
// zone's account, since routes only reach Workers in that account.
func (r *WorkerRouteReconciler) routedScript(
	ctx context.Context,
	route *cloudflarev1alpha1.WorkerRoute,
	zone *cloudflarev1alpha1.Zone,
) (string, *conditionError) {
	ref := route.Spec.WorkerScriptRef
	if ref == nil {
		return "", nil
	}

	ws := &cloudflarev1alpha1.WorkerScript{}
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: route.Namespace}, ws); err != nil {
		return "", &conditionError{
			Reason:  "WorkerScriptNotFound",
			Message: fmt.Sprintf("WorkerScript %q not found: %v", ref.Name, err),
		}
	}
	if ws.Spec.AccountRef.Name != zone.Spec.AccountRef.Name {
		return "", &conditionError{
			Reason: "AccountMismatch",
			Message: fmt.Sprintf("WorkerScript %q uses CloudflareAccount %q but Zone %q uses %q; "+
				"a route can only run a Worker from the zone's account",
				ws.Name, ws.Spec.AccountRef.Name, zone.Name, zone.Spec.AccountRef.Name),
		}
	}
	if !isWorkerScriptReady(ws) {
		return "", &conditionError{
			Reason:  "WorkerScriptNotReady",
			Message: fmt.Sprintf("WorkerScript %q is not ready", ws.Name),
		}
	}
	return ws.Spec.Name, nil
}

// syncRoute drives the desired→observed→delta→reconcile loop for a WorkerRoute.
func (r *WorkerRouteReconciler) syncRoute(
	ctx context.Context,
	route *cloudflarev1alpha1.WorkerRoute,
	zoneID, script string,
	cfAPI WorkerRouteAPI,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	rc := cf.ZoneIdentifier(zoneID)

	// If we already have a route ID, fetch the current state. Cloudflare
	// deletes a Worker's routes together with the Worker, so a route that is
	// gone is recreated once its WorkerScript is uploaded again.
	var current cf.WorkerRoute
	if routeID := route.Status.CloudflareMetadata.RouteID; routeID != "" {
		got, err := cfAPI.GetWorkerRoute(ctx, rc, routeID)
		switch {
		case err == nil:
			current = got.WorkerRoute
		case cfpkg.IsNotFound(err):
			logger.Info("Worker route deleted outside kflare, recreating", "routeID", routeID)
		default:
			return handleCloudflareError(ctx, r.Client, route, err)
		}
	}

	// No route yet: adopt the route with this pattern (patterns are unique
	// within a zone) unless another WorkerRoute manages it, or create one.
	if current.ID == "" {
		routes, err := cfAPI.ListWorkerRoutes(ctx, rc, cf.ListWorkerRoutesParams{})
		if err != nil {
			return handleCloudflareError(ctx, r.Client, route, err)
		}
		claimed, err := claimedIDs(ctx, r.Client, &cloudflarev1alpha1.WorkerRouteList{},
			cloudflarev1alpha1.WorkerRouteIDLabel, route)
		if err != nil {
			return ctrl.Result{}, err
		}
		if existing, ok := routeWithPattern(routes.Routes, route.Spec.Pattern); ok {
			if owner, taken := claimed[existing.ID]; taken {
				return notReadyRetryAfter(ctx, r.Client, route, &conditionError{
					Reason:  "PatternConflict",
					Message: fmt.Sprintf("Pattern %q is already managed by WorkerRoute %s", route.Spec.Pattern, owner),
				})
			}
			current = existing
			logger.Info("Adopted existing Worker route", "pattern", existing.Pattern, "routeID", existing.ID)
		} else {
			created, err := cfAPI.CreateWorkerRoute(ctx, rc, cf.CreateWorkerRouteParams{
				Pattern: route.Spec.Pattern,
				Script:  script,
			})
			if err != nil {
				return handleCloudflareError(ctx, r.Client, route, err)
			}
			current = created.WorkerRoute
			logger.Info("Created Worker route", "pattern", current.Pattern, "routeID", current.ID)
		}
	}

	// Claim the route before anything else can fail, so no other WorkerRoute
	// adopts it in the meantime.
	if err := claimID(ctx, r.Client, route, cloudflarev1alpha1.WorkerRouteIDLabel, current.ID); err != nil {
		return ctrl.Result{}, err
	}

	// Drift: the pattern or the Worker it runs.
	if current.Pattern != route.Spec.Pattern || current.ScriptName != script {
		updated, err := cfAPI.UpdateWorkerRoute(ctx, rc, cf.UpdateWorkerRouteParams{
			ID:      current.ID,
			Pattern: route.Spec.Pattern,
			Script:  script,
		})
		if err != nil {
			return handleCloudflareError(ctx, r.Client, route, err)
		}
		current = updated.WorkerRoute
		logger.Info("Updated Worker route", "routeID", current.ID, "pattern", current.Pattern, "script", current.ScriptName)
	}

	route.Status.CloudflareMetadata.RouteID = current.ID
	route.Status.CloudflareMetadata.ZoneID = zoneID
	route.Status.CloudflareMetadata.Script = current.ScriptName
	return ctrl.Result{}, updateReady(ctx, r.Client, route, "Synced", "Worker route is synced with Cloudflare")
}

// routeWithPattern returns the route whose pattern is pattern.
func routeWithPattern(routes []cf.WorkerRoute, pattern string) (cf.WorkerRoute, bool) {
	for _, rt := range routes {
		if rt.Pattern == pattern {
			return rt, true
		}
	}
	return cf.WorkerRoute{}, false
}

// patternInZone reports whether the hostname of a route pattern (everything
// before the first "/", without a leading "*" or "*.") is zone or one of its
// subdomains.
func patternInZone(pattern, zone string) bool {
	host, _, _ := strings.Cut(strings.ToLower(pattern), "/")
	host = strings.TrimPrefix(strings.TrimPrefix(host, "*"), ".")
	zone = strings.ToLower(zone)
	return host == zone || strings.HasSuffix(host, "."+zone)
}

// isWorkerScriptReady returns true if the WorkerScript has a Ready=True
// condition and has been uploaded.
func isWorkerScriptReady(ws *cloudflarev1alpha1.WorkerScript) bool {
	return ws.Status.CloudflareMetadata.ModifiedOn != "" &&
		meta.IsStatusConditionTrue(ws.Status.Conditions, cloudflarev1alpha1.ConditionReady)
}

// reconcileDelete handles the deletion lifecycle: unless the retain policy is
// set, it deletes the route from Cloudflare, then removes the finalizer.
//
// The Zone waits for its routes before it is deleted, so it is normally still
// there. If it is gone anyway (its finalizer was removed by hand), there are
// no credentials to delete with and the route is left in Cloudflare.
func (r *WorkerRouteReconciler) reconcileDelete(ctx context.Context, route *cloudflarev1alpha1.WorkerRoute) (ctrl.Result, error) {
	// Safety check: if the finalizer is already gone, there is nothing to do.
	if !controllerutil.ContainsFinalizer(route, reconciler.Finalizer) {
		return ctrl.Result{}, nil
	}

	routeID := route.Status.CloudflareMetadata.RouteID
	if routeID != "" && !reconciler.RetainOnDelete(route) {
		if err := r.deleteCloudflareRoute(ctx, route, routeID); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Remove the finalizer to allow Kubernetes to complete the deletion.
	_, err := reconciler.RemoveFinalizer(ctx, r.Client, route)
	return ctrl.Result{}, err
}

// deleteCloudflareRoute deletes routeID from the zone of route's Zone
// resource, resolving Zone → Account → Secret for credentials.
func (r *WorkerRouteReconciler) deleteCloudflareRoute(
	ctx context.Context,
	route *cloudflarev1alpha1.WorkerRoute,
	routeID string,
) error {
	zone := &cloudflarev1alpha1.Zone{}
	zoneKey := types.NamespacedName{Name: route.Spec.ZoneRef.Name, Namespace: route.Namespace}
	if err := r.Get(ctx, zoneKey, zone); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).Info("Zone is gone, skipping Cloudflare deletion",
				"zone", zoneKey.Name, "routeID", routeID)
			return nil
		}
		return err
	}

	_, token, credErr := resolveAccountToken(ctx, r.Client, zone.Spec.AccountRef.Name, false)
	if credErr != nil {
		return credErr
	}
	cfAPI, err := r.newWorkerRouteAPI(token)
	if err != nil {
		return err
	}

	rc := cf.ZoneIdentifier(zone.Status.CloudflareMetadata.ZoneID)
	if _, err := cfAPI.DeleteWorkerRoute(ctx, rc, routeID); err != nil && !cfpkg.IsNotFound(err) {
		return err
	}
	return nil
}

// SetupWithManager registers WorkerRouteReconciler with the manager. It
// watches Zones and WorkerScripts, so a route waiting for either is
// reconciled as soon as it is ready, and a route whose Worker was uploaded
// again is recreated.
func (r *WorkerRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudflarev1alpha1.WorkerRoute{}).
		Watches(&cloudflarev1alpha1.Zone{}, handler.EnqueueRequestsFromMapFunc(r.routesForZone)).
		Watches(&cloudflarev1alpha1.WorkerScript{}, handler.EnqueueRequestsFromMapFunc(r.routesForWorkerScript)).
		Complete(r)
}

// routesMatching returns a reconcile.Request for every WorkerRoute in
// namespace for which match returns true.
func (r *WorkerRouteReconciler) routesMatching(
	ctx context.Context,
	namespace string,
	match func(*cloudflarev1alpha1.WorkerRoute) bool,
) []reconcile.Request {
	list := &cloudflarev1alpha1.WorkerRouteList{}
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if match(&list.Items[i]) {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: list.Items[i].Name, Namespace: list.Items[i].Namespace},
			})
		}
	}
	return reqs
}

// routesForZone maps a Zone event to the WorkerRoutes in that zone.
func (r *WorkerRouteReconciler) routesForZone(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.routesMatching(ctx, obj.GetNamespace(), func(rt *cloudflarev1alpha1.WorkerRoute) bool {
		return rt.Spec.ZoneRef.Name == obj.GetName()
	})
}

// routesForWorkerScript maps a WorkerScript event to the WorkerRoutes that run it.
func (r *WorkerRouteReconciler) routesForWorkerScript(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.routesMatching(ctx, obj.GetNamespace(), func(rt *cloudflarev1alpha1.WorkerRoute) bool {
		return rt.Spec.WorkerScriptRef != nil && rt.Spec.WorkerScriptRef.Name == obj.GetName()
	})
}

// newWorkerRouteAPI builds a WorkerRouteAPI with the injected factory,
// falling back to the production client.
func (r *WorkerRouteReconciler) newWorkerRouteAPI(token string) (WorkerRouteAPI, error) {
	if r.NewWorkerRouteAPI != nil {
		return r.NewWorkerRouteAPI(token)
	}
	return defaultWorkerRouteAPI(token)
}

// defaultWorkerRouteAPI is the production factory: it delegates to cfpkg.New
// so that the returned *cfpkg.Client (which embeds *cf.API) satisfies
// WorkerRouteAPI.
func defaultWorkerRouteAPI(token string) (WorkerRouteAPI, error) {
	return cfpkg.New(token)
}
