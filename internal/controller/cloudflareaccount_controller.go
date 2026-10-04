/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cf "github.com/cloudflare/cloudflare-go"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

// CloudflareAccountAPI is the subset of the Cloudflare API used by this
// controller.  Declaring a narrow interface here (rather than depending on
// *cfpkg.Client directly) keeps unit tests simple: tests inject a fake that
// implements only Account().  In production, *cfpkg.Client satisfies this
// interface because it embeds *cf.API.
type CloudflareAccountAPI interface {
	Account(ctx context.Context, accountID string) (cf.Account, cf.ResultInfo, error)
}

// CloudflareAccountReconciler reconciles a CloudflareAccount object.
type CloudflareAccountReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewCFClient constructs a CloudflareAccountAPI from a raw API token.
	// Defaults to cfpkg.New; overridden in tests to inject a fake.
	NewCFClient func(token string) (CloudflareAccountAPI, error)
}

//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=cloudflareaccounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=cloudflareaccounts/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=cloudflareaccounts/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *CloudflareAccountReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	account := &cloudflarev1alpha1.CloudflareAccount{}
	if err := r.Get(ctx, req.NamespacedName, account); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Fetch the API token from the referenced Secret. The Secret watch
	// re-triggers this reconcile once the Secret is created or fixed.
	token, credErr := accountToken(ctx, r.Client, account)
	if credErr != nil {
		return ctrl.Result{}, updateNotReady(ctx, r.Client, account, credErr.Reason, credErr.Message)
	}

	// Build the Cloudflare client.
	newClient := r.NewCFClient
	if newClient == nil {
		newClient = defaultCFClient
	}
	cfClient, err := newClient(token)
	if err != nil {
		return invalidToken(ctx, r.Client, account, err)
	}

	cfAccount, _, err := cfClient.Account(ctx, account.Spec.AccountID)
	if err != nil {
		return handleCloudflareError(ctx, r.Client, account, err)
	}

	logger.Info("Cloudflare account validated", "accountName", cfAccount.Name)
	account.Status.AccountName = cfAccount.Name
	return ctrl.Result{}, updateReady(ctx, r.Client, account, "Validated",
		"Credentials are valid and account is reachable")
}

// SetupWithManager sets up the controller with the Manager. It also watches
// Secrets, so creating, fixing or rotating an account's API token
// re-validates the account straight away.
func (r *CloudflareAccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudflarev1alpha1.CloudflareAccount{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.accountsForSecret)).
		Complete(r)
}

// accountsForSecret maps a Secret event to reconcile.Requests for every
// CloudflareAccount whose token is stored in that Secret.
func (r *CloudflareAccountReconciler) accountsForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &cloudflarev1alpha1.CloudflareAccountList{}
	if err := r.List(ctx, list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, a := range list.Items {
		ref := a.Spec.TokenSecretRef
		if ref.Name == obj.GetName() && ref.Namespace == obj.GetNamespace() {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: a.Name}})
		}
	}
	return reqs
}

// defaultCFClient is the production factory: it delegates to cfpkg.New so
// that the returned *cfpkg.Client (which embeds *cf.API) satisfies
// CloudflareAccountAPI.
func defaultCFClient(token string) (CloudflareAccountAPI, error) {
	return cfpkg.New(token)
}
