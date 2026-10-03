/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

const (
	// tunnelConfigSource makes new tunnels remotely managed: their ingress
	// rules live in Cloudflare and are pushed through the API rather than read
	// from a config file next to cloudflared.
	tunnelConfigSource = "cloudflare"

	// tunnelSecretBytes is the length of the random secret sent when creating
	// a tunnel. Cloudflare requires at least 32 bytes.
	tunnelSecretBytes = 32

	// credentialsRetryInterval is how long to wait before retrying when the
	// tunnel cannot proceed because of something outside its own spec: the
	// account credentials, or a Secret kflare does not own. Neither the token
	// Secret nor a conflicting Secret is watched, so a timed retry is what
	// notices that it has been fixed.
	credentialsRetryInterval = time.Minute
)

// TunnelAPI is the subset of the Cloudflare API used by this controller.
// Declaring a narrow interface keeps unit tests simple: tests inject a fake
// that implements only these methods. In production, *cfpkg.Client satisfies
// this interface because it embeds *cf.API.
//
// UpdateTunnel is deliberately absent: in cloudflare-go v0.89 it omits the
// tunnel ID from the request path, so tunnels cannot be renamed. spec.name is
// immutable instead.
type TunnelAPI interface {
	CreateTunnel(ctx context.Context, rc *cf.ResourceContainer, params cf.TunnelCreateParams) (cf.Tunnel, error)
	GetTunnel(ctx context.Context, rc *cf.ResourceContainer, tunnelID string) (cf.Tunnel, error)
	ListTunnels(ctx context.Context, rc *cf.ResourceContainer, params cf.TunnelListParams) ([]cf.Tunnel, *cf.ResultInfo, error)
	GetTunnelToken(ctx context.Context, rc *cf.ResourceContainer, tunnelID string) (string, error)
	CleanupTunnelConnections(ctx context.Context, rc *cf.ResourceContainer, tunnelID string) error
	DeleteTunnel(ctx context.Context, rc *cf.ResourceContainer, tunnelID string) error
}

// TunnelReconciler reconciles a Tunnel object.
type TunnelReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewTunnelAPI constructs a TunnelAPI from a raw API token.
	// Defaults to defaultTunnelAPI; overridden in tests to inject a fake.
	NewTunnelAPI func(token string) (TunnelAPI, error)
}

//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=tunnels,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=tunnels/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=tunnels/finalizers,verbs=update
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=cloudflareaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *TunnelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	tunnel := &cloudflarev1alpha1.Tunnel{}
	if err := r.Get(ctx, req.NamespacedName, tunnel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion before anything else.
	if !tunnel.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, tunnel)
	}

	// Ensure the finalizer is present before doing any work.
	// On the very first reconcile the finalizer is absent — add it and return.
	// controller-runtime will re-enqueue the object once the update is visible.
	added, err := reconciler.EnsureFinalizer(ctx, r.Client, tunnel)
	if err != nil {
		return ctrl.Result{}, err
	}
	if added {
		logger.Info("Added finalizer", "tunnel", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	account, token, credErr := resolveAccountToken(ctx, r.Client, tunnel.Spec.AccountRef.Name, true)
	if credErr != nil {
		reconciler.SetCondition(&tunnel.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, credErr.Reason, credErr.Message, tunnel.Generation)
		return ctrl.Result{RequeueAfter: credentialsRetryInterval}, r.Status().Update(ctx, tunnel)
	}

	cfAPI, err := r.newTunnelAPI(token)
	if err != nil {
		reconciler.SetCondition(&tunnel.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "InvalidToken",
			fmt.Sprintf("Failed to create Cloudflare client: %v", err),
			tunnel.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, tunnel)
	}

	return r.syncTunnel(ctx, tunnel, account, cfAPI)
}

// syncTunnel drives the desired→observed→delta→reconcile loop for a Tunnel:
// find, adopt or create the tunnel, then write its token to the credentials
// Secret.
func (r *TunnelReconciler) syncTunnel(
	ctx context.Context,
	tunnel *cloudflarev1alpha1.Tunnel,
	account *cloudflarev1alpha1.CloudflareAccount,
	cfAPI TunnelAPI,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	rc := cf.AccountIdentifier(account.Spec.AccountID)

	var cfTunnel cf.Tunnel

	// If we already have a tunnel ID, try to fetch the current state.
	// Cloudflare keeps deleted tunnels readable with deleted_at set, so a
	// successful Get can still mean the tunnel is gone.
	if tunnelID := tunnel.Status.CloudflareMetadata.TunnelID; tunnelID != "" {
		got, err := cfAPI.GetTunnel(ctx, rc, tunnelID)
		switch {
		case err == nil && got.DeletedAt == nil:
			cfTunnel = got
		case err == nil || cfpkg.IsNotFound(err):
			logger.Info("Tunnel deleted externally, recreating", "tunnelID", tunnelID)
		default:
			return r.handleCFError(ctx, tunnel, err)
		}
	}

	// If we don't have a tunnel yet (no ID, or externally deleted), find or create.
	// Adopting by name also recovers a tunnel created by an earlier reconcile
	// whose status update never landed.
	if cfTunnel.ID == "" {
		tunnels, _, err := cfAPI.ListTunnels(ctx, rc, cf.TunnelListParams{
			Name:      tunnel.Spec.Name,
			IsDeleted: cf.BoolPtr(false),
		})
		if err != nil {
			return r.handleCFError(ctx, tunnel, err)
		}
		if len(tunnels) > 0 {
			cfTunnel = tunnels[0]
			logger.Info("Adopted existing tunnel", "name", tunnel.Spec.Name, "tunnelID", cfTunnel.ID)
		} else {
			secret, err := generateTunnelSecret()
			if err != nil {
				return ctrl.Result{}, err
			}
			created, err := cfAPI.CreateTunnel(ctx, rc, cf.TunnelCreateParams{
				Name:      tunnel.Spec.Name,
				Secret:    secret,
				ConfigSrc: tunnelConfigSource,
			})
			if err != nil {
				return r.handleCFError(ctx, tunnel, err)
			}
			cfTunnel = created
			logger.Info("Created tunnel", "name", tunnel.Spec.Name, "tunnelID", cfTunnel.ID)
		}
	}

	// Record the tunnel before anything else can fail, so every status update
	// below (including error paths) persists its ID.
	tunnel.Status.CloudflareMetadata.TunnelID = cfTunnel.ID
	tunnel.Status.CloudflareMetadata.Status = cfTunnel.Status

	// Fetch the token every reconcile so the Secret follows a secret rotated
	// outside kflare.
	token, err := cfAPI.GetTunnelToken(ctx, rc, cfTunnel.ID)
	if err != nil {
		return r.handleCFError(ctx, tunnel, err)
	}

	if err := r.ensureCredentialsSecret(ctx, tunnel, token); err != nil {
		var conflict *secretConflictError
		if !errors.As(err, &conflict) {
			return ctrl.Result{}, err
		}
		reconciler.SetCondition(&tunnel.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "CredentialsSecretConflict", conflict.Error(), tunnel.Generation)
		return ctrl.Result{RequeueAfter: credentialsRetryInterval}, r.Status().Update(ctx, tunnel)
	}

	if err := r.deleteStaleCredentialsSecret(ctx, tunnel); err != nil {
		return ctrl.Result{}, err
	}
	tunnel.Status.CredentialsSecretName = tunnel.Spec.CredentialsSecretRef.Name

	reconciler.SetCondition(&tunnel.Status.Conditions, cloudflarev1alpha1.ConditionReady,
		metav1.ConditionTrue, "Synced",
		"Tunnel is synced with Cloudflare",
		tunnel.Generation)
	return ctrl.Result{}, r.Status().Update(ctx, tunnel)
}

// secretConflictError reports that the credentials Secret exists but belongs
// to something other than this Tunnel.
type secretConflictError struct {
	namespace, name string
}

func (e *secretConflictError) Error() string {
	return fmt.Sprintf("Secret %s/%s already exists and is not owned by this Tunnel; "+
		"delete it or choose another credentialsSecretRef", e.namespace, e.name)
}

// ensureCredentialsSecret creates or updates the Secret named by
// spec.credentialsSecretRef so that it holds token under TUNNEL_TOKEN and is
// controlled by tunnel. A Secret that exists but is not controlled by tunnel
// is left untouched and reported as a *secretConflictError.
func (r *TunnelReconciler) ensureCredentialsSecret(
	ctx context.Context,
	tunnel *cloudflarev1alpha1.Tunnel,
	token string,
) error {
	key := types.NamespacedName{Name: tunnel.Spec.CredentialsSecretRef.Name, Namespace: tunnel.Namespace}

	secret := &corev1.Secret{}
	err := r.Get(ctx, key, secret)
	if apierrors.IsNotFound(err) {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{cloudflarev1alpha1.TunnelTokenKey: []byte(token)},
		}
		if err := controllerutil.SetControllerReference(tunnel, secret, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, secret)
	}
	if err != nil {
		return err
	}

	if !metav1.IsControlledBy(secret, tunnel) {
		return &secretConflictError{namespace: key.Namespace, name: key.Name}
	}
	if bytes.Equal(secret.Data[cloudflarev1alpha1.TunnelTokenKey], []byte(token)) {
		return nil
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[cloudflarev1alpha1.TunnelTokenKey] = []byte(token)
	return r.Update(ctx, secret)
}

// deleteStaleCredentialsSecret removes the Secret recorded in
// status.credentialsSecretName when spec.credentialsSecretRef now names a
// different one, so an old copy of the token does not linger. A Secret this
// Tunnel does not control is never deleted.
func (r *TunnelReconciler) deleteStaleCredentialsSecret(ctx context.Context, tunnel *cloudflarev1alpha1.Tunnel) error {
	previous := tunnel.Status.CredentialsSecretName
	if previous == "" || previous == tunnel.Spec.CredentialsSecretRef.Name {
		return nil
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: previous, Namespace: tunnel.Namespace}, secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(secret, tunnel) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, secret))
}

// handleCFError sets the appropriate condition based on whether the Cloudflare
// error is terminal (stop requeuing) or retryable (let controller-runtime
// back off and retry).
func (r *TunnelReconciler) handleCFError(ctx context.Context, tunnel *cloudflarev1alpha1.Tunnel, err error) (ctrl.Result, error) {
	if cfpkg.IsTerminalError(err) {
		reconciler.SetCondition(&tunnel.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "TerminalError",
			fmt.Sprintf("Terminal Cloudflare API error: %v", err),
			tunnel.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, tunnel)
	}
	reconciler.SetCondition(&tunnel.Status.Conditions, cloudflarev1alpha1.ConditionReady,
		metav1.ConditionFalse, "APIError",
		fmt.Sprintf("Cloudflare API error: %v", err),
		tunnel.Generation)
	if statusErr := r.Status().Update(ctx, tunnel); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	return ctrl.Result{}, err
}

// reconcileDelete handles the deletion lifecycle: unless the retain policy is
// set, it clears the tunnel's inactive connections and deletes the tunnel
// from Cloudflare, then removes the finalizer. The credentials Secret is
// garbage-collected through its owner reference in either case.
//
// Cloudflare refuses to delete a tunnel that still has active connectors, so
// deletion keeps retrying until cloudflared is stopped.
func (r *TunnelReconciler) reconcileDelete(ctx context.Context, tunnel *cloudflarev1alpha1.Tunnel) (ctrl.Result, error) {
	// Safety check: if the finalizer is already gone, there is nothing to do.
	if !controllerutil.ContainsFinalizer(tunnel, reconciler.Finalizer) {
		return ctrl.Result{}, nil
	}

	tunnelID := tunnel.Status.CloudflareMetadata.TunnelID
	policy := tunnel.Annotations["cloudflare.k8s.io/deletion-policy"]

	if tunnelID != "" && policy != "retain" {
		account, token, credErr := resolveAccountToken(ctx, r.Client, tunnel.Spec.AccountRef.Name, false)
		if credErr != nil {
			return ctrl.Result{}, credErr
		}
		cfAPI, err := r.newTunnelAPI(token)
		if err != nil {
			return ctrl.Result{}, err
		}

		rc := cf.AccountIdentifier(account.Spec.AccountID)
		if err := cfAPI.CleanupTunnelConnections(ctx, rc, tunnelID); err != nil && !cfpkg.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err := cfAPI.DeleteTunnel(ctx, rc, tunnelID); err != nil && !cfpkg.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// Remove the finalizer to allow Kubernetes to complete the deletion.
	_, err := reconciler.RemoveFinalizer(ctx, r.Client, tunnel)
	return ctrl.Result{}, err
}

// SetupWithManager registers TunnelReconciler with the manager. It watches the
// Secrets it owns, so a deleted or edited token Secret is restored, and
// CloudflareAccounts, so a change in account credentials re-triggers every
// tunnel that references the account.
func (r *TunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudflarev1alpha1.Tunnel{}).
		Owns(&corev1.Secret{}).
		Watches(
			&cloudflarev1alpha1.CloudflareAccount{},
			handler.EnqueueRequestsFromMapFunc(r.tunnelsForAccount),
		).
		Complete(r)
}

// tunnelsForAccount maps a CloudflareAccount event to reconcile.Requests for
// all Tunnel objects that reference it.
func (r *TunnelReconciler) tunnelsForAccount(ctx context.Context, obj client.Object) []reconcile.Request {
	tunnelList := &cloudflarev1alpha1.TunnelList{}
	if err := r.List(ctx, tunnelList); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, t := range tunnelList.Items {
		if t.Spec.AccountRef.Name == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: t.Name, Namespace: t.Namespace},
			})
		}
	}
	return reqs
}

// newTunnelAPI builds a TunnelAPI with the injected factory, falling back to
// the production client.
func (r *TunnelReconciler) newTunnelAPI(token string) (TunnelAPI, error) {
	if r.NewTunnelAPI != nil {
		return r.NewTunnelAPI(token)
	}
	return defaultTunnelAPI(token)
}

// generateTunnelSecret returns a random base64-encoded tunnel secret. The
// connector authenticates with the token Cloudflare derives from it, so the
// secret itself is never stored.
func generateTunnelSecret() (string, error) {
	b := make([]byte, tunnelSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating tunnel secret: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// defaultTunnelAPI is the production factory: it delegates to cfpkg.New so
// that the returned *cfpkg.Client (which embeds *cf.API) satisfies TunnelAPI.
func defaultTunnelAPI(token string) (TunnelAPI, error) {
	return cfpkg.New(token)
}
