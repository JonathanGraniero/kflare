/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

// Package reconciler provides shared helpers used across all kflare
// controllers.  Centralising these utilities keeps individual reconcilers
// focused on resource-specific logic and ensures consistent behaviour for
// condition management, finalizer lifecycle, and error classification.
package reconciler

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// Finalizer is the finalizer added to every kflare-managed resource.
	// Its presence prevents Kubernetes from deleting the object until the
	// controller has had a chance to clean up the corresponding Cloudflare
	// resource (unless the deletion-policy annotation says "retain").
	Finalizer = "cloudflare.k8s.io/finalizer"

	// DeletionPolicyAnnotation chooses what happens to the Cloudflare resource
	// when its Kubernetes resource is deleted. Any value other than
	// DeletionPolicyRetain, including no annotation, deletes it.
	DeletionPolicyAnnotation = "cloudflare.k8s.io/deletion-policy"

	// DeletionPolicyRetain keeps the Cloudflare resource when its Kubernetes
	// resource is deleted.
	DeletionPolicyRetain = "retain"
)

// RetainOnDelete reports whether obj's deletion-policy annotation asks to
// keep the Cloudflare resource when obj is deleted.
func RetainOnDelete(obj client.Object) bool {
	return obj.GetAnnotations()[DeletionPolicyAnnotation] == DeletionPolicyRetain
}

// SetCondition upserts a standard Kubernetes condition on the provided slice.
// It is a thin, typed wrapper around meta.SetStatusCondition that ensures
// ObservedGeneration is always populated, which is required for correct
// server-side apply and status conflict detection.
//
// condType must match the Type of an existing condition in the slice for an
// update; otherwise a new condition is appended.
func SetCondition(
	conditions *[]metav1.Condition,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
	generation int64,
) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// EnsureFinalizer adds the kflare finalizer to obj if it is not already
// present and persists the change via a Kubernetes Update call.
//
// Returns (true, nil) when the finalizer was added and the object updated.
// Returns (false, nil) when the finalizer was already present (no-op).
// Returns (false, err) when the Update call failed.
func EnsureFinalizer(ctx context.Context, c client.Client, obj client.Object) (added bool, err error) {
	if controllerutil.ContainsFinalizer(obj, Finalizer) {
		return false, nil
	}
	controllerutil.AddFinalizer(obj, Finalizer)
	return true, c.Update(ctx, obj)
}

// RemoveFinalizer removes the kflare finalizer from obj and persists the
// change via a Kubernetes Update call.  Typically called at the end of a
// deletion reconcile, after the corresponding Cloudflare resource has been
// deleted (or retained per policy).
//
// Returns (true, nil) when the finalizer was removed and the object updated.
// Returns (false, nil) when the finalizer was not present (no-op).
// Returns (false, err) when the Update call failed.
func RemoveFinalizer(ctx context.Context, c client.Client, obj client.Object) (removed bool, err error) {
	if !controllerutil.ContainsFinalizer(obj, Finalizer) {
		return false, nil
	}
	controllerutil.RemoveFinalizer(obj, Finalizer)
	return true, c.Update(ctx, obj)
}
