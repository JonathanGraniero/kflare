/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
	// defaultTunnelService is the catch-all service used when
	// spec.defaultService is empty, and the only rule left after deletion.
	// Cloudflare rejects a configuration without a final catch-all rule.
	defaultTunnelService = "http_status:404"

	// tunnelConflictRetryInterval is how long a TunnelConfiguration that lost
	// ownership of its Tunnel waits before checking whether it can take over.
	tunnelConflictRetryInterval = time.Minute
)

// TunnelConfigurationAPI is the subset of the Cloudflare API used by this
// controller. Declaring a narrow interface keeps unit tests simple: tests
// inject a fake that implements only these methods. In production,
// *cfpkg.Client satisfies this interface because it embeds *cf.API.
type TunnelConfigurationAPI interface {
	GetTunnelConfiguration(ctx context.Context, rc *cf.ResourceContainer, tunnelID string) (cf.TunnelConfigurationResult, error)
	UpdateTunnelConfiguration(ctx context.Context, rc *cf.ResourceContainer, params cf.TunnelConfigurationParams) (cf.TunnelConfigurationResult, error)
}

// TunnelConfigurationReconciler reconciles a TunnelConfiguration object.
type TunnelConfigurationReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewTunnelConfigurationAPI constructs a TunnelConfigurationAPI from a raw
	// API token. Defaults to defaultTunnelConfigurationAPI; overridden in tests.
	NewTunnelConfigurationAPI func(token string) (TunnelConfigurationAPI, error)
}

//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=tunnelconfigurations,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=tunnelconfigurations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=tunnelconfigurations/finalizers,verbs=update
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=tunnels,verbs=get;list;watch
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=cloudflareaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *TunnelConfigurationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	tc := &cloudflarev1alpha1.TunnelConfiguration{}
	if err := r.Get(ctx, req.NamespacedName, tc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion before anything else.
	if !tc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, tc)
	}

	// Ensure the finalizer is present before doing any work.
	// On the very first reconcile the finalizer is absent — add it and return.
	// controller-runtime will re-enqueue the object once the update is visible.
	added, err := reconciler.EnsureFinalizer(ctx, r.Client, tc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if added {
		logger.Info("Added finalizer", "tunnelconfiguration", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// Fetch the Tunnel this configuration belongs to (same namespace). The
	// Tunnel watch re-triggers this reconcile once it exists and is ready.
	tunnel := &cloudflarev1alpha1.Tunnel{}
	tunnelKey := types.NamespacedName{Name: tc.Spec.TunnelRef.Name, Namespace: tc.Namespace}
	if err := r.Get(ctx, tunnelKey, tunnel); err != nil {
		reconciler.SetCondition(&tc.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "TunnelNotFound",
			fmt.Sprintf("Tunnel %q not found: %v", tc.Spec.TunnelRef.Name, err),
			tc.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, tc)
	}
	if !isTunnelReady(tunnel) {
		reconciler.SetCondition(&tc.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "TunnelNotReady",
			fmt.Sprintf("Tunnel %q is not ready", tc.Spec.TunnelRef.Name),
			tc.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, tc)
	}

	// Only one TunnelConfiguration may own a tunnel's ingress rules.
	owner, err := r.configurationOwner(ctx, tc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if owner != tc.Name {
		reconciler.SetCondition(&tc.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "TunnelAlreadyConfigured",
			fmt.Sprintf("Tunnel %q is already configured by TunnelConfiguration %q", tc.Spec.TunnelRef.Name, owner),
			tc.Generation)
		return ctrl.Result{RequeueAfter: tunnelConflictRetryInterval}, r.Status().Update(ctx, tc)
	}

	account, token, credErr := resolveAccountToken(ctx, r.Client, tunnel.Spec.AccountRef.Name, true)
	if credErr != nil {
		reconciler.SetCondition(&tc.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, credErr.Reason, credErr.Message, tc.Generation)
		return ctrl.Result{RequeueAfter: credentialsRetryInterval}, r.Status().Update(ctx, tc)
	}

	cfAPI, err := r.newTunnelConfigurationAPI(token)
	if err != nil {
		reconciler.SetCondition(&tc.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "InvalidToken",
			fmt.Sprintf("Failed to create Cloudflare client: %v", err),
			tc.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, tc)
	}

	return r.syncConfiguration(ctx, tc, account, tunnel.Status.CloudflareMetadata.TunnelID, cfAPI)
}

// syncConfiguration drives the desired→observed→delta→reconcile loop: it
// reads the tunnel's configuration and writes the desired one only when they
// differ.
func (r *TunnelConfigurationReconciler) syncConfiguration(
	ctx context.Context,
	tc *cloudflarev1alpha1.TunnelConfiguration,
	account *cloudflarev1alpha1.CloudflareAccount,
	tunnelID string,
	cfAPI TunnelConfigurationAPI,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	rc := cf.AccountIdentifier(account.Spec.AccountID)
	desired := desiredTunnelConfiguration(tc)

	current, err := cfAPI.GetTunnelConfiguration(ctx, rc, tunnelID)
	if err != nil {
		return r.handleCFError(ctx, tc, err)
	}

	if !tunnelConfigurationMatches(desired, current.Config) {
		updated, err := cfAPI.UpdateTunnelConfiguration(ctx, rc, cf.TunnelConfigurationParams{
			TunnelID: tunnelID,
			Config:   desired,
		})
		if err != nil {
			return r.handleCFError(ctx, tc, err)
		}
		current = updated
		logger.Info("Updated tunnel configuration", "tunnelID", tunnelID, "version", updated.Version)
	}

	tc.Status.CloudflareMetadata.TunnelID = tunnelID
	tc.Status.CloudflareMetadata.Version = current.Version
	reconciler.SetCondition(&tc.Status.Conditions, cloudflarev1alpha1.ConditionReady,
		metav1.ConditionTrue, "Synced",
		"Tunnel configuration is synced with Cloudflare",
		tc.Generation)
	return ctrl.Result{}, r.Status().Update(ctx, tc)
}

// configurationOwner returns the name of the TunnelConfiguration that owns
// the ingress rules of tc's Tunnel: the oldest one referencing it, with the
// name breaking ties. Configurations still being deleted keep ownership until
// they are gone, so they cannot clear rules another configuration just wrote.
func (r *TunnelConfigurationReconciler) configurationOwner(
	ctx context.Context,
	tc *cloudflarev1alpha1.TunnelConfiguration,
) (string, error) {
	list := &cloudflarev1alpha1.TunnelConfigurationList{}
	if err := r.List(ctx, list, client.InNamespace(tc.Namespace)); err != nil {
		return "", err
	}
	owner := tc
	for i := range list.Items {
		other := &list.Items[i]
		if other.Spec.TunnelRef.Name != tc.Spec.TunnelRef.Name {
			continue
		}
		if olderThan(other, owner) {
			owner = other
		}
	}
	return owner.Name, nil
}

// olderThan orders TunnelConfigurations by creation time, then by name.
func olderThan(a, b *cloudflarev1alpha1.TunnelConfiguration) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

// desiredTunnelConfiguration builds the configuration to send to Cloudflare:
// the spec's rules followed by the catch-all rule.
func desiredTunnelConfiguration(tc *cloudflarev1alpha1.TunnelConfiguration) cf.TunnelConfiguration {
	rules := make([]cf.UnvalidatedIngressRule, 0, len(tc.Spec.Ingress)+1)
	for _, rule := range tc.Spec.Ingress {
		rules = append(rules, cf.UnvalidatedIngressRule{
			Hostname:      rule.Hostname,
			Path:          rule.Path,
			Service:       rule.Service,
			OriginRequest: originRequestConfig(rule.OriginRequest),
		})
	}
	defaultService := tc.Spec.DefaultService
	if defaultService == "" {
		defaultService = defaultTunnelService
	}
	rules = append(rules, cf.UnvalidatedIngressRule{Service: defaultService})
	return cf.TunnelConfiguration{Ingress: rules}
}

// originRequestConfig converts the spec's origin settings to the SDK type.
func originRequestConfig(o *cloudflarev1alpha1.TunnelOriginRequest) *cf.OriginRequestConfig {
	if o == nil {
		return nil
	}
	return &cf.OriginRequestConfig{
		HTTPHostHeader:         o.HTTPHostHeader,
		OriginServerName:       o.OriginServerName,
		NoTLSVerify:            o.NoTLSVerify,
		Http2Origin:            o.HTTP2Origin,
		DisableChunkedEncoding: o.DisableChunkedEncoding,
	}
}

// tunnelConfigurationMatches reports whether actual already equals desired.
// kflare owns the whole configuration, so any rule, origin setting or
// top-level option that differs — including ones the spec cannot express,
// such as settings added in the dashboard — counts as drift and is
// overwritten.
func tunnelConfigurationMatches(desired, actual cf.TunnelConfiguration) bool {
	if !reflect.DeepEqual(normalizeIngress(desired.Ingress), normalizeIngress(actual.Ingress)) {
		return false
	}
	if !reflect.DeepEqual(actual.OriginRequest, cf.OriginRequestConfig{}) {
		return false
	}
	// Cloudflare reports warp-routing as disabled when it was never set.
	return actual.WarpRouting == nil || !actual.WarpRouting.Enabled
}

// normalizeIngress treats an empty originRequest object the same as an
// absent one, so equivalent rules compare equal however the API encoded them.
func normalizeIngress(rules []cf.UnvalidatedIngressRule) []cf.UnvalidatedIngressRule {
	out := make([]cf.UnvalidatedIngressRule, len(rules))
	for i, rule := range rules {
		if rule.OriginRequest != nil && reflect.DeepEqual(*rule.OriginRequest, cf.OriginRequestConfig{}) {
			rule.OriginRequest = nil
		}
		out[i] = rule
	}
	return out
}

// handleCFError sets the appropriate condition based on whether the Cloudflare
// error is terminal (stop requeuing) or retryable (let controller-runtime
// back off and retry).
func (r *TunnelConfigurationReconciler) handleCFError(
	ctx context.Context,
	tc *cloudflarev1alpha1.TunnelConfiguration,
	err error,
) (ctrl.Result, error) {
	if cfpkg.IsTerminalError(err) {
		reconciler.SetCondition(&tc.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "TerminalError",
			fmt.Sprintf("Terminal Cloudflare API error: %v", err),
			tc.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, tc)
	}
	reconciler.SetCondition(&tc.Status.Conditions, cloudflarev1alpha1.ConditionReady,
		metav1.ConditionFalse, "APIError",
		fmt.Sprintf("Cloudflare API error: %v", err),
		tc.Generation)
	if statusErr := r.Status().Update(ctx, tc); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	return ctrl.Result{}, err
}

// reconcileDelete handles the deletion lifecycle: unless the retain policy is
// set, it resets the tunnel this configuration last wrote to so that only the
// catch-all rule remains (Cloudflare does not allow an empty configuration),
// then removes the finalizer.
//
// If the Tunnel resource is already gone there is nothing to reset: the
// tunnel was either deleted from Cloudflare with it or deliberately retained.
func (r *TunnelConfigurationReconciler) reconcileDelete(
	ctx context.Context,
	tc *cloudflarev1alpha1.TunnelConfiguration,
) (ctrl.Result, error) {
	// Safety check: if the finalizer is already gone, there is nothing to do.
	if !controllerutil.ContainsFinalizer(tc, reconciler.Finalizer) {
		return ctrl.Result{}, nil
	}

	tunnelID := tc.Status.CloudflareMetadata.TunnelID
	if tunnelID != "" && !reconciler.RetainOnDelete(tc) {
		if err := r.resetTunnelConfiguration(ctx, tc, tunnelID); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Remove the finalizer to allow Kubernetes to complete the deletion.
	_, err := reconciler.RemoveFinalizer(ctx, r.Client, tc)
	return ctrl.Result{}, err
}

// resetTunnelConfiguration replaces the configuration of tunnelID with a
// single catch-all rule.
func (r *TunnelConfigurationReconciler) resetTunnelConfiguration(
	ctx context.Context,
	tc *cloudflarev1alpha1.TunnelConfiguration,
	tunnelID string,
) error {
	tunnel := &cloudflarev1alpha1.Tunnel{}
	tunnelKey := types.NamespacedName{Name: tc.Spec.TunnelRef.Name, Namespace: tc.Namespace}
	if err := r.Get(ctx, tunnelKey, tunnel); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	account, token, credErr := resolveAccountToken(ctx, r.Client, tunnel.Spec.AccountRef.Name, false)
	if credErr != nil {
		return credErr
	}
	cfAPI, err := r.newTunnelConfigurationAPI(token)
	if err != nil {
		return err
	}

	_, err = cfAPI.UpdateTunnelConfiguration(ctx, cf.AccountIdentifier(account.Spec.AccountID), cf.TunnelConfigurationParams{
		TunnelID: tunnelID,
		Config: cf.TunnelConfiguration{
			Ingress: []cf.UnvalidatedIngressRule{{Service: defaultTunnelService}},
		},
	})
	if err != nil && !cfpkg.IsNotFound(err) {
		return err
	}
	return nil
}

// SetupWithManager registers TunnelConfigurationReconciler with the manager
// and watches Tunnels, so a Tunnel becoming ready (or being recreated with a
// new ID) re-triggers every configuration that references it.
func (r *TunnelConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudflarev1alpha1.TunnelConfiguration{}).
		Watches(
			&cloudflarev1alpha1.Tunnel{},
			handler.EnqueueRequestsFromMapFunc(r.configurationsForTunnel),
		).
		Complete(r)
}

// configurationsForTunnel maps a Tunnel event to reconcile.Requests for all
// TunnelConfigurations in the same namespace that reference it.
func (r *TunnelConfigurationReconciler) configurationsForTunnel(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &cloudflarev1alpha1.TunnelConfigurationList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, tc := range list.Items {
		if tc.Spec.TunnelRef.Name == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: tc.Name, Namespace: tc.Namespace},
			})
		}
	}
	return reqs
}

// isTunnelReady returns true if the Tunnel has a Ready=True condition and a tunnel ID.
func isTunnelReady(tunnel *cloudflarev1alpha1.Tunnel) bool {
	return tunnel.Status.CloudflareMetadata.TunnelID != "" &&
		meta.IsStatusConditionTrue(tunnel.Status.Conditions, cloudflarev1alpha1.ConditionReady)
}

// newTunnelConfigurationAPI builds a TunnelConfigurationAPI with the injected
// factory, falling back to the production client.
func (r *TunnelConfigurationReconciler) newTunnelConfigurationAPI(token string) (TunnelConfigurationAPI, error) {
	if r.NewTunnelConfigurationAPI != nil {
		return r.NewTunnelConfigurationAPI(token)
	}
	return defaultTunnelConfigurationAPI(token)
}

// defaultTunnelConfigurationAPI is the production factory: it delegates to
// cfpkg.New so that the returned *cfpkg.Client (which embeds *cf.API)
// satisfies TunnelConfigurationAPI.
func defaultTunnelConfigurationAPI(token string) (TunnelConfigurationAPI, error) {
	return cfpkg.New(token)
}
