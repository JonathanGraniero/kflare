/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cf "github.com/cloudflare/cloudflare-go"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// CloudflareAccountAPI is the subset of the Cloudflare API used by this
// controller.  Declaring a narrow interface here (rather than depending on
// *cfpkg.Client directly) keeps unit tests simple: tests inject a fake that
// implements only these methods.  In production, *cfpkg.Client satisfies this
// interface: it embeds *cf.API, and verifies its token with cloudflare-go v7.
type CloudflareAccountAPI interface {
	Account(ctx context.Context, accountID string) (cf.Account, cf.ResultInfo, error)
	VerifyToken(ctx context.Context, accountID string) (cfpkg.TokenVerification, error)
}

// The reasons of an account whose token Cloudflare reports as unusable. kflare
// checks again at the account's next validation (nextValidation), since the
// token can be enabled again, or its expiry moved, in Cloudflare, which no
// watch sees.
const (
	reasonTokenExpired  = "TokenExpired"
	reasonTokenDisabled = "TokenDisabled"
)

// reasonTokenExpiresSoon is the reason of the Warning Event an account records
// at each validation while its token expires within the manager's
// --token-expiry-warning.
const reasonTokenExpiresSoon = "TokenExpiresSoon"

// expiryRecheckDelay is how long after its token's expiry an account is
// validated again, when that comes before its next scheduled validation, so
// that Ready turns False once the token lapses rather than at the next
// check. Cloudflare already reported a token expired 15 seconds after its
// expires_on (checked live on 2026-11-09, the first check after it); the delay
// leaves room for clocks that differ.
const expiryRecheckDelay = 30 * time.Second

// CloudflareAccountReconciler reconciles a CloudflareAccount object.
type CloudflareAccountReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewCFClient constructs a CloudflareAccountAPI from a raw API token.
	// Defaults to cfpkg.New; overridden in tests to inject a fake.
	NewCFClient func(token string) (CloudflareAccountAPI, error)

	// TokenCheckInterval is how long after a validation the account is
	// validated again (--token-check-interval), so that a token revoked,
	// disabled or expired in Cloudflare, which changes nothing in the
	// cluster, shows within about that long. 0 schedules nothing.
	TokenCheckInterval time.Duration

	// TokenExpiryWarning is how long before its token expires an account
	// starts to record a TokenExpiresSoon Warning Event at each validation
	// (--token-expiry-warning). 0 records none.
	TokenExpiryWarning time.Duration
}

//+kubebuilder:rbac:groups=kflare.dev,resources=cloudflareaccounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kflare.dev,resources=cloudflareaccounts/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kflare.dev,resources=cloudflareaccounts/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;update;patch

func (r *CloudflareAccountReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	account := &cloudflarev1alpha1.CloudflareAccount{}
	if err := r.Get(ctx, req.NamespacedName, account); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Every installation caches every account, and manages only those in its
	// scope: another installation's is left to it, status, finalizer and
	// token Secret included.
	if why := watchScopeFrom(ctx).accountOutside(account); why != "" {
		logger.V(1).Info("Leaving a CloudflareAccount outside what this installation watches", "reason", why)
		return ctrl.Result{}, nil
	}

	// A paused resource is left as it is, deletion included, until its
	// kflare.dev/paused annotation is removed.
	if reconciler.Paused(account) {
		return ctrl.Result{}, updatePaused(ctx, r.Client, account)
	}

	if !account.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, account)
	}

	// The finalizer keeps the account, and with it the credentials its
	// dependents need for their own cleanup, until nothing uses it. The
	// update refreshes account, so the reconcile can carry on.
	if _, err := reconciler.EnsureFinalizer(ctx, r.Client, account); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.protectTokenSecret(ctx, account); err != nil {
		return ctrl.Result{}, err
	}

	// A selector the CRD's schema cannot check allows no namespace, which the
	// account says here, where its owner looks, rather than only in each
	// resource that uses it. A change to the spec re-triggers this reconcile.
	if allowed := account.Spec.AllowedNamespaces; allowed != nil {
		if _, err := metav1.LabelSelectorAsSelector(allowed.Selector); err != nil {
			return ctrl.Result{}, updateNotReady(ctx, r.Client, account, "InvalidAllowedNamespaces",
				fmt.Sprintf("spec.allowedNamespaces.selector is invalid: %v", err))
		}
	}

	// Fetch the API token from the referenced Secret. The Secret watch
	// re-triggers this reconcile once the Secret is created or fixed.
	token, credErr := accountToken(ctx, r.Client, account)
	if credErr != nil {
		forgetToken(account)
		return ctrl.Result{}, updateNotReady(ctx, r.Client, account, credErr.Reason, credErr.Message)
	}

	if wait := admit(account, token, account); wait > 0 {
		return rateLimited(ctx, r.Client, account, wait)
	}

	// Build the Cloudflare client.
	newClient := r.NewCFClient
	if newClient == nil {
		newClient = defaultCFClient
	}
	cfClient, err := newClient(token)
	if err != nil {
		forgetToken(account)
		return invalidToken(ctx, r.Client, account, err)
	}

	// The token's own status first: reading the account with a disabled or
	// expired token fails with "Invalid access token", and only the token's
	// verification says why.
	verification, err := cfClient.VerifyToken(ctx, account.Spec.AccountID)
	if err != nil {
		if cfpkg.IsTerminalError(err) {
			forgetToken(account)
		}
		return handleCloudflareError(ctx, r.Client, account, err)
	}
	now := time.Now()
	recordToken(account, verification)
	secret := account.Spec.TokenSecretRef.Namespace + "/" + account.Spec.TokenSecretRef.Name
	switch verification.Status {
	case cfpkg.TokenExpired:
		return ctrl.Result{RequeueAfter: r.nextValidation(account, now)},
			updateNotReady(ctx, r.Client, account, reasonTokenExpired, expiredMessage(account, secret))
	case cfpkg.TokenDisabled:
		return ctrl.Result{RequeueAfter: r.nextValidation(account, now)},
			updateNotReady(ctx, r.Client, account, reasonTokenDisabled, fmt.Sprintf(
				"Cloudflare reports the API token in Secret %s as disabled: enable it again in Cloudflare, or put "+
					"a new token in the Secret", secret))
	}

	cfAccount, _, err := cfClient.Account(ctx, account.Spec.AccountID)
	if err != nil {
		return handleCloudflareError(ctx, r.Client, account, err)
	}

	logger.Info("Cloudflare account validated", "accountName", cfAccount.Name, "tokenStatus",
		verification.Status, "tokenExpiresAt", account.Status.TokenExpiresAt)
	account.Status.AccountName = cfAccount.Name
	if err := updateReady(ctx, r.Client, account, "Validated",
		"Credentials are valid and account is reachable"); err != nil {
		return ctrl.Result{}, err
	}
	r.warnOfExpiry(account, secret, now)
	return ctrl.Result{RequeueAfter: r.nextValidation(account, now)}, nil
}

// recordToken records what Cloudflare reported about account's token in
// account's status, in memory: the status write that ends the reconcile
// carries it.
func recordToken(account *cloudflarev1alpha1.CloudflareAccount, verification cfpkg.TokenVerification) {
	account.Status.TokenStatus = verification.Status
	account.Status.TokenExpiresAt = nil
	if !verification.ExpiresOn.IsZero() {
		account.Status.TokenExpiresAt = new(metav1.NewTime(verification.ExpiresOn))
	}
}

// forgetToken clears what account's status says about its token, in memory,
// once there is no token to say it of: the Secret holds none, or Cloudflare
// refuses the one it holds, which may not be the token verified before.
func forgetToken(account *cloudflarev1alpha1.CloudflareAccount) {
	account.Status.TokenStatus = ""
	account.Status.TokenExpiresAt = nil
}

// expiredMessage is the Ready message of account, whose token in secret has
// expired. It names the expiry, which stays the same from one check to the
// next.
func expiredMessage(account *cloudflarev1alpha1.CloudflareAccount, secret string) string {
	expired := "has expired"
	if at := account.Status.TokenExpiresAt; at != nil {
		expired = "expired at " + at.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("The API token in Secret %s %s: put a new token in the Secret, or extend the token's "+
		"expiry in Cloudflare", secret, expired)
}

// warnOfExpiry records a TokenExpiresSoon Warning Event on account if its
// token, in secret, expires within r.TokenExpiryWarning of now. It does so at
// each validation: the API server keeps an Event for an hour by default, so
// an Event recorded once would be gone long before the token.
func (r *CloudflareAccountReconciler) warnOfExpiry(account *cloudflarev1alpha1.CloudflareAccount, secret string,
	now time.Time) {
	at := account.Status.TokenExpiresAt
	if at == nil {
		return
	}
	if left := at.Sub(now); left > 0 && left <= r.TokenExpiryWarning {
		recordWarning(account, reasonTokenExpiresSoon, "Validate", fmt.Sprintf(
			"The API token in Secret %s expires at %s: put a new token in the Secret before then", secret,
			at.UTC().Format(time.RFC3339)))
	}
}

// nextValidation returns how long after now account is validated again: the
// token check interval or the next drift check (reconciler.NextDriftCheck),
// whichever comes first, or expiryRecheckDelay after its token expires, if
// that is sooner and still to come. 0 schedules nothing.
func (r *CloudflareAccountReconciler) nextValidation(account *cloudflarev1alpha1.CloudflareAccount,
	now time.Time) time.Duration {
	next := r.TokenCheckInterval
	sooner := func(d time.Duration) {
		if d > 0 && (next == 0 || d < next) {
			next = d
		}
	}
	sooner(reconciler.NextDriftCheck())
	if at := account.Status.TokenExpiresAt; at != nil {
		sooner(at.Sub(now) + expiryRecheckDelay)
	}
	return next
}

// SetupWithManager sets up the controller with the Manager. It also watches
// Secrets, so creating, fixing or rotating an account's API token
// re-validates the account straight away. An account outside the
// installation's scope is reconciled on its events all the same, and left
// alone (Reconcile): no predicate has the context that carries the scope.
//
// While an account is being deleted, the deletion of each resource that uses
// it re-triggers the account, so it is released as soon as the last one is
// gone.
func (r *CloudflareAccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&cloudflarev1alpha1.CloudflareAccount{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.accountsForSecret))
	for _, dependent := range accountDependents {
		b = b.Watches(dependent.newObject(), handler.EnqueueRequestsFromMapFunc(r.deletingAccountOf),
			builder.WithPredicates(deletesOnly))
	}
	return b.Complete(withControllerName("CloudflareAccount", r))
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
