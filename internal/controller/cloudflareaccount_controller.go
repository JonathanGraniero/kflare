/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cf "github.com/cloudflare/cloudflare-go"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
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

	// Fetch the API token from the referenced Secret.
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      account.Spec.TokenSecretRef.Name,
		Namespace: account.Spec.TokenSecretRef.Namespace,
	}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		reconciler.SetCondition(&account.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "SecretNotFound",
			fmt.Sprintf("Secret %s/%s not found: %v", secretKey.Namespace, secretKey.Name, err),
			account.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, account)
	}

	tokenKey := account.Spec.TokenSecretRef.Key
	if tokenKey == "" {
		tokenKey = "CF_API_TOKEN"
	}
	token, ok := secret.Data[tokenKey]
	if !ok {
		reconciler.SetCondition(&account.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "TokenKeyMissing",
			fmt.Sprintf("Key %q not found in secret %s/%s", tokenKey, secretKey.Namespace, secretKey.Name),
			account.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, account)
	}

	// Build the Cloudflare client.
	newClient := r.NewCFClient
	if newClient == nil {
		newClient = defaultCFClient
	}
	cfClient, err := newClient(string(token))
	if err != nil {
		reconciler.SetCondition(&account.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "InvalidToken",
			fmt.Sprintf("Failed to create Cloudflare client: %v", err),
			account.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, account)
	}

	cfAccount, _, err := cfClient.Account(ctx, account.Spec.AccountID)
	if err != nil {
		reconciler.SetCondition(&account.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "APIError",
			fmt.Sprintf("Cloudflare API error: %v", err),
			account.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, account)
	}

	logger.Info("Cloudflare account validated", "accountName", cfAccount.Name)
	account.Status.AccountName = cfAccount.Name
	reconciler.SetCondition(&account.Status.Conditions, cloudflarev1alpha1.ConditionReady,
		metav1.ConditionTrue, "Validated",
		"Credentials are valid and account is reachable",
		account.Generation)

	return ctrl.Result{}, r.Status().Update(ctx, account)
}

// SetupWithManager sets up the controller with the Manager.
func (r *CloudflareAccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudflarev1alpha1.CloudflareAccount{}).
		Complete(r)
}

// defaultCFClient is the production factory: it delegates to cfpkg.New so
// that the returned *cfpkg.Client (which embeds *cf.API) satisfies
// CloudflareAccountAPI.
func defaultCFClient(token string) (CloudflareAccountAPI, error) {
	return cfpkg.New(token)
}
