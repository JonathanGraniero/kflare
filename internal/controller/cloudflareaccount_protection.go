/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
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
//     WorkerScript references it. DNSRecords and TunnelConfigurations reach
//     the account through their Zone or Tunnel, which waits for them or
//     skips cleanup once it is gone.
//   - The account's token Secret keeps tokenSecretFinalizer until no
//     CloudflareAccount references it.

// tokenSecretFinalizer protects a CloudflareAccount's token Secret from
// deletion while an account references it.
const tokenSecretFinalizer = "cloudflare.k8s.io/token-protection"

// deletesOnly passes only delete events, which are all a CloudflareAccount
// needs to hear about its dependents.
var deletesOnly = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return false },
	UpdateFunc:  func(event.UpdateEvent) bool { return false },
	DeleteFunc:  func(event.DeleteEvent) bool { return true },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// maxListedDependents caps how many blocking resources the InUse message names.
const maxListedDependents = 5

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

	dependents, err := r.accountDependents(ctx, account.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(dependents) > 0 {
		listed := dependents
		if len(listed) > maxListedDependents {
			listed = append(listed[:maxListedDependents:maxListedDependents], "...")
		}
		return ctrl.Result{}, updateNotReady(ctx, r.Client, account, "InUse",
			fmt.Sprintf("Deletion is waiting for %d resource(s) that use this account: %s",
				len(dependents), strings.Join(listed, ", ")))
	}

	for _, ref := range protectedSecrets(account) {
		if err := r.releaseTokenSecret(ctx, ref, account.Name); err != nil {
			return ctrl.Result{}, err
		}
	}
	_, err = reconciler.RemoveFinalizer(ctx, r.Client, account)
	return ctrl.Result{}, err
}

// accountDependents returns the Zones, Tunnels and WorkerScripts that
// reference accountName, as sorted "Kind namespace/name" strings.
func (r *CloudflareAccountReconciler) accountDependents(ctx context.Context, accountName string) ([]string, error) {
	var dependents []string
	add := func(kind string, obj client.Object, ref string) {
		if ref == accountName {
			dependents = append(dependents, fmt.Sprintf("%s %s/%s", kind, obj.GetNamespace(), obj.GetName()))
		}
	}

	zones := &cloudflarev1alpha1.ZoneList{}
	if err := r.List(ctx, zones); err != nil {
		return nil, err
	}
	for i := range zones.Items {
		add("Zone", &zones.Items[i], zones.Items[i].Spec.AccountRef.Name)
	}
	tunnels := &cloudflarev1alpha1.TunnelList{}
	if err := r.List(ctx, tunnels); err != nil {
		return nil, err
	}
	for i := range tunnels.Items {
		add("Tunnel", &tunnels.Items[i], tunnels.Items[i].Spec.AccountRef.Name)
	}
	workers := &cloudflarev1alpha1.WorkerScriptList{}
	if err := r.List(ctx, workers); err != nil {
		return nil, err
	}
	for i := range workers.Items {
		add("WorkerScript", &workers.Items[i], workers.Items[i].Spec.AccountRef.Name)
	}

	sort.Strings(dependents)
	return dependents, nil
}

// deletingAccountOf maps the deletion of a Zone, Tunnel or WorkerScript to
// its account, but only while that account is itself being deleted: an
// account in normal use has nothing to do when a dependent goes away.
func (r *CloudflareAccountReconciler) deletingAccountOf(ctx context.Context, obj client.Object) []reconcile.Request {
	var accountName string
	switch o := obj.(type) {
	case *cloudflarev1alpha1.Zone:
		accountName = o.Spec.AccountRef.Name
	case *cloudflarev1alpha1.Tunnel:
		accountName = o.Spec.AccountRef.Name
	case *cloudflarev1alpha1.WorkerScript:
		accountName = o.Spec.AccountRef.Name
	default:
		return nil
	}

	account := &cloudflarev1alpha1.CloudflareAccount{}
	if err := r.Get(ctx, types.NamespacedName{Name: accountName}, account); err != nil {
		return nil
	}
	if account.DeletionTimestamp.IsZero() {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: accountName}}}
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
