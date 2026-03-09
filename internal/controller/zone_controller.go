/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"

	cf "github.com/cloudflare/cloudflare-go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// ZoneAPI is the subset of the Cloudflare API used by this controller.
// Declaring a narrow interface keeps unit tests simple: tests inject a fake
// that implements only the methods used here.  In production, *cfpkg.Client
// satisfies this interface because it embeds *cf.API.
type ZoneAPI interface {
	CreateZone(ctx context.Context, name string, jumpstart bool, account cf.Account, zoneType string) (cf.Zone, error)
	ZoneDetails(ctx context.Context, zoneID string) (cf.Zone, error)
	ListZones(ctx context.Context, z ...string) ([]cf.Zone, error)
	DeleteZone(ctx context.Context, zoneID string) (cf.ZoneID, error)
	EditZone(ctx context.Context, zoneID string, zoneOpts cf.ZoneOptions) (cf.Zone, error)
}

// ZoneReconciler reconciles a Zone object.
type ZoneReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewZoneAPI constructs a ZoneAPI from a raw API token.
	// Defaults to defaultZoneAPI; overridden in tests to inject a fake.
	NewZoneAPI func(token string) (ZoneAPI, error)
}

//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=zones,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=zones/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=zones/finalizers,verbs=update
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=cloudflareaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *ZoneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	zone := &cloudflarev1alpha1.Zone{}
	if err := r.Get(ctx, req.NamespacedName, zone); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion before anything else.
	if !zone.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, zone)
	}

	// Ensure the finalizer is present before doing any work.
	// On the very first reconcile the finalizer is absent — add it and return.
	// controller-runtime will re-enqueue the object once the update is visible.
	added, err := reconciler.EnsureFinalizer(ctx, r.Client, zone)
	if err != nil {
		return ctrl.Result{}, err
	}
	if added {
		logger.Info("Added finalizer", "zone", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// Fetch the CloudflareAccount that this zone belongs to.
	account := &cloudflarev1alpha1.CloudflareAccount{}
	if err := r.Get(ctx, types.NamespacedName{Name: zone.Spec.AccountRef.Name}, account); err != nil {
		reconciler.SetCondition(&zone.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "AccountNotFound",
			fmt.Sprintf("CloudflareAccount %q not found: %v", zone.Spec.AccountRef.Name, err),
			zone.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, zone)
	}

	// The account must be ready before we can use its credentials.
	if !isAccountReady(account) {
		reconciler.SetCondition(&zone.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "AccountNotReady",
			fmt.Sprintf("CloudflareAccount %q is not ready", zone.Spec.AccountRef.Name),
			zone.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, zone)
	}

	// Fetch the API token from the secret referenced by the account.
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      account.Spec.TokenSecretRef.Name,
		Namespace: account.Spec.TokenSecretRef.Namespace,
	}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		reconciler.SetCondition(&zone.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "SecretNotFound",
			fmt.Sprintf("Secret %s/%s not found: %v", secretKey.Namespace, secretKey.Name, err),
			zone.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, zone)
	}

	tokenKey := account.Spec.TokenSecretRef.Key
	if tokenKey == "" {
		tokenKey = "CF_API_TOKEN"
	}
	tokenBytes, ok := secret.Data[tokenKey]
	if !ok {
		reconciler.SetCondition(&zone.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "TokenKeyMissing",
			fmt.Sprintf("Key %q not found in secret %s/%s", tokenKey, secretKey.Namespace, secretKey.Name),
			zone.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, zone)
	}

	// Build the Cloudflare client.
	newAPI := r.NewZoneAPI
	if newAPI == nil {
		newAPI = defaultZoneAPI
	}
	cfAPI, err := newAPI(string(tokenBytes))
	if err != nil {
		reconciler.SetCondition(&zone.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "InvalidToken",
			fmt.Sprintf("Failed to create Cloudflare client: %v", err),
			zone.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, zone)
	}

	return r.syncZone(ctx, logger, zone, account, cfAPI)
}

// syncZone drives the desired→observed→delta→reconcile loop for a Zone.
func (r *ZoneReconciler) syncZone(
	ctx context.Context,
	logger interface {
		Info(msg string, keysAndValues ...interface{})
	},
	zone *cloudflarev1alpha1.Zone,
	account *cloudflarev1alpha1.CloudflareAccount,
	cfAPI ZoneAPI,
) (ctrl.Result, error) {
	var cfZone cf.Zone

	// If we already have a zone ID, try to fetch the current state.
	if zone.Status.CloudflareMetadata.ZoneID != "" {
		got, err := cfAPI.ZoneDetails(ctx, zone.Status.CloudflareMetadata.ZoneID)
		if err != nil {
			if !cfpkg.IsNotFound(err) {
				return r.handleCFError(ctx, zone, err)
			}
			// Zone was deleted externally — fall through to find or recreate it.
			logger.Info("Zone deleted externally, recreating",
				"zoneID", zone.Status.CloudflareMetadata.ZoneID)
		} else {
			cfZone = got
		}
	}

	// If we don't have a zone yet (no ID, or externally deleted), find or create.
	if cfZone.ID == "" {
		zones, listErr := cfAPI.ListZones(ctx, zone.Spec.Name)
		if listErr != nil {
			return r.handleCFError(ctx, zone, listErr)
		}
		if len(zones) > 0 {
			// Adopt the pre-existing zone.
			cfZone = zones[0]
			logger.Info("Adopted existing zone", "name", zone.Spec.Name, "zoneID", cfZone.ID)
		} else {
			// Create a brand-new zone.
			zoneType := zone.Spec.Type
			if zoneType == "" {
				zoneType = "full"
			}
			created, createErr := cfAPI.CreateZone(
				ctx,
				zone.Spec.Name,
				true,
				cf.Account{ID: account.Spec.AccountID},
				zoneType,
			)
			if createErr != nil {
				return r.handleCFError(ctx, zone, createErr)
			}
			cfZone = created
			logger.Info("Created zone", "name", zone.Spec.Name, "zoneID", cfZone.ID)
		}
	}

	// Drift detection: zone type.
	desiredType := zone.Spec.Type
	if desiredType == "" {
		desiredType = "full"
	}
	if cfZone.Type != desiredType {
		updated, editErr := cfAPI.EditZone(ctx, cfZone.ID, cf.ZoneOptions{Type: desiredType})
		if editErr != nil {
			return r.handleCFError(ctx, zone, editErr)
		}
		cfZone = updated
		logger.Info("Updated zone type", "zoneID", cfZone.ID, "type", desiredType)
	}

	// Sync status from Cloudflare.
	zone.Status.CloudflareMetadata.ZoneID = cfZone.ID
	zone.Status.CloudflareMetadata.NameServers = cfZone.NameServers
	zone.Status.CloudflareMetadata.Status = cfZone.Status
	reconciler.SetCondition(&zone.Status.Conditions, cloudflarev1alpha1.ConditionReady,
		metav1.ConditionTrue, "Synced",
		"Zone is synced with Cloudflare",
		zone.Generation)
	return ctrl.Result{}, r.Status().Update(ctx, zone)
}

// handleCFError sets the appropriate condition based on whether the Cloudflare
// error is terminal (stop requeuing) or retryable (let controller-runtime
// back off and retry).
func (r *ZoneReconciler) handleCFError(ctx context.Context, zone *cloudflarev1alpha1.Zone, err error) (ctrl.Result, error) {
	if cfpkg.IsTerminalError(err) {
		reconciler.SetCondition(&zone.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "TerminalError",
			fmt.Sprintf("Terminal Cloudflare API error: %v", err),
			zone.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, zone)
	}
	reconciler.SetCondition(&zone.Status.Conditions, cloudflarev1alpha1.ConditionReady,
		metav1.ConditionFalse, "APIError",
		fmt.Sprintf("Cloudflare API error: %v", err),
		zone.Generation)
	if statusErr := r.Status().Update(ctx, zone); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	return ctrl.Result{}, err
}

// reconcileDelete handles the deletion lifecycle: optionally removes the zone
// from Cloudflare (unless the retain policy is set), then removes the finalizer.
func (r *ZoneReconciler) reconcileDelete(ctx context.Context, zone *cloudflarev1alpha1.Zone) (ctrl.Result, error) {
	// Safety check: if the finalizer is already gone, there is nothing to do.
	if !controllerutil.ContainsFinalizer(zone, reconciler.Finalizer) {
		return ctrl.Result{}, nil
	}

	zoneID := zone.Status.CloudflareMetadata.ZoneID
	policy := zone.Annotations["cloudflare.k8s.io/deletion-policy"]

	if zoneID != "" && policy != "retain" {
		// Resolve credentials to call the Cloudflare API.
		account := &cloudflarev1alpha1.CloudflareAccount{}
		if err := r.Get(ctx, types.NamespacedName{Name: zone.Spec.AccountRef.Name}, account); err != nil {
			return ctrl.Result{}, err
		}

		secret := &corev1.Secret{}
		secretKey := types.NamespacedName{
			Name:      account.Spec.TokenSecretRef.Name,
			Namespace: account.Spec.TokenSecretRef.Namespace,
		}
		if err := r.Get(ctx, secretKey, secret); err != nil {
			return ctrl.Result{}, err
		}

		tokenKey := account.Spec.TokenSecretRef.Key
		if tokenKey == "" {
			tokenKey = "CF_API_TOKEN"
		}
		tokenBytes, ok := secret.Data[tokenKey]
		if !ok {
			return ctrl.Result{}, fmt.Errorf("key %q not found in secret %s/%s",
				tokenKey, secretKey.Namespace, secretKey.Name)
		}

		newAPI := r.NewZoneAPI
		if newAPI == nil {
			newAPI = defaultZoneAPI
		}
		cfAPI, err := newAPI(string(tokenBytes))
		if err != nil {
			return ctrl.Result{}, err
		}

		if _, err := cfAPI.DeleteZone(ctx, zoneID); err != nil && !cfpkg.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// Remove the finalizer to allow Kubernetes to complete the deletion.
	_, err := reconciler.RemoveFinalizer(ctx, r.Client, zone)
	return ctrl.Result{}, err
}

// SetupWithManager registers ZoneReconciler with the manager and adds a watch
// on CloudflareAccount so that a change in account credentials automatically
// re-triggers reconciliation of all zones that reference that account.
func (r *ZoneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudflarev1alpha1.Zone{}).
		Watches(
			&cloudflarev1alpha1.CloudflareAccount{},
			handler.EnqueueRequestsFromMapFunc(r.zonesForAccount),
		).
		Complete(r)
}

// zonesForAccount maps a CloudflareAccount event to reconcile.Requests for
// all Zone objects that reference it.
func (r *ZoneReconciler) zonesForAccount(ctx context.Context, obj client.Object) []reconcile.Request {
	zoneList := &cloudflarev1alpha1.ZoneList{}
	if err := r.List(ctx, zoneList); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, z := range zoneList.Items {
		if z.Spec.AccountRef.Name == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: z.Name, Namespace: z.Namespace},
			})
		}
	}
	return reqs
}

// isAccountReady returns true if the CloudflareAccount has a Ready=True condition.
func isAccountReady(account *cloudflarev1alpha1.CloudflareAccount) bool {
	for _, c := range account.Status.Conditions {
		if c.Type == cloudflarev1alpha1.ConditionReady {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

// defaultZoneAPI is the production factory: it delegates to cfpkg.New so
// that the returned *cfpkg.Client (which embeds *cf.API) satisfies ZoneAPI.
func defaultZoneAPI(token string) (ZoneAPI, error) {
	return cfpkg.New(token)
}
