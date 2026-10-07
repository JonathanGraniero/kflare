/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// Deleting resources in any order — for example `kubectl delete -k` on a
// directory holding a Secret, its CloudflareAccount and the Zones using it —
// must not strand a resource that still needs the API token for its own
// cleanup. Two finalizers chain the deletions:
//
//   - A CloudflareAccount keeps reconciler.Finalizer until no Zone, Tunnel or
//     WorkerScript references it (see protection.go). DNSRecords, WorkerRoutes
//     and TunnelConfigurations reach the account through their Zone or
//     Tunnel, which in turn waits for them.
//   - The account's token Secret keeps tokenSecretFinalizer until no
//     CloudflareAccount references it.

// tokenSecretFinalizer protects a CloudflareAccount's token Secret from
// deletion while an account references it.
const tokenSecretFinalizer = "kflare.dev/token-protection"

// reconcileDelete releases the account once no resource uses it: it frees
// the token Secret and removes the finalizer. Until then it reports
// Ready=False with reason InUse, naming what it is waiting for.
func (r *CloudflareAccountReconciler) reconcileDelete(
	ctx context.Context,
	account *cloudflarev1alpha1.CloudflareAccount,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(account, reconciler.Finalizer) {
		return ctrl.Result{}, nil
	}

	dependents, err := dependentsOf(ctx, r.Client, "", account.Name, accountDependents)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(dependents) > 0 {
		return ctrl.Result{}, reportInUse(ctx, r.Client, account, dependents)
	}

	for _, ref := range protectedSecrets(account) {
		if err := r.releaseTokenSecret(ctx, ref, account.Name); err != nil {
			return ctrl.Result{}, err
		}
	}
	_, err = reconciler.RemoveFinalizer(ctx, r.Client, account)
	return ctrl.Result{}, err
}

// deletingAccountOf maps the deletion of a Zone, Tunnel or WorkerScript to
// its account while that account is being deleted.
func (r *CloudflareAccountReconciler) deletingAccountOf(ctx context.Context, obj client.Object) []reconcile.Request {
	return deletingParentOf(ctx, r.Client, obj,
		func() client.Object { return &cloudflarev1alpha1.CloudflareAccount{} }, false, accountDependents)
}

// protectTokenSecret adds tokenSecretFinalizer to the Secret named by
// spec.tokenSecretRef and records it in status.protectedTokenSecret,
// releasing the previously protected Secret if the reference changed. A
// missing Secret is left for the token lookup to report; the Secret watch
// brings the account back once it exists.
func (r *CloudflareAccountReconciler) protectTokenSecret(
	ctx context.Context,
	account *cloudflarev1alpha1.CloudflareAccount,
) error {
	desired := corev1.SecretReference{
		Name:      account.Spec.TokenSecretRef.Name,
		Namespace: account.Spec.TokenSecretRef.Namespace,
	}
	if previous := account.Status.ProtectedTokenSecret; previous != nil && *previous != desired {
		if err := r.releaseTokenSecret(ctx, *previous, account.Name); err != nil {
			return err
		}
		account.Status.ProtectedTokenSecret = nil
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !controllerutil.ContainsFinalizer(secret, tokenSecretFinalizer) {
		patch := client.MergeFrom(secret.DeepCopy())
		controllerutil.AddFinalizer(secret, tokenSecretFinalizer)
		if err := r.Patch(ctx, secret, patch); err != nil {
			return err
		}
	}
	account.Status.ProtectedTokenSecret = &desired
	return nil
}

// releaseTokenSecret removes tokenSecretFinalizer from the Secret ref unless
// a CloudflareAccount other than releasingAccount still references it.
func (r *CloudflareAccountReconciler) releaseTokenSecret(
	ctx context.Context,
	ref corev1.SecretReference,
	releasingAccount string,
) error {
	accounts := &cloudflarev1alpha1.CloudflareAccountList{}
	if err := r.List(ctx, accounts); err != nil {
		return err
	}
	for _, other := range accounts.Items {
		otherRef := other.Spec.TokenSecretRef
		if other.Name != releasingAccount && otherRef.Name == ref.Name && otherRef.Namespace == ref.Namespace {
			return nil
		}
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !controllerutil.ContainsFinalizer(secret, tokenSecretFinalizer) {
		return nil
	}
	patch := client.MergeFrom(secret.DeepCopy())
	controllerutil.RemoveFinalizer(secret, tokenSecretFinalizer)
	if err := r.Patch(ctx, secret, patch); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	log.FromContext(ctx).Info("Released token Secret", "secret", ref.Namespace+"/"+ref.Name)
	return nil
}

// protectedSecrets returns the Secrets the account may hold a finalizer on:
// the one recorded in status, and the one in spec in case the status update
// that recorded it never landed.
func protectedSecrets(account *cloudflarev1alpha1.CloudflareAccount) []corev1.SecretReference {
	refs := []corev1.SecretReference{{
		Name:      account.Spec.TokenSecretRef.Name,
		Namespace: account.Spec.TokenSecretRef.Namespace,
	}}
	if p := account.Status.ProtectedTokenSecret; p != nil && *p != refs[0] {
		refs = append(refs, *p)
	}
	return refs
}
