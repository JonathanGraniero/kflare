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
	"fmt"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// Finalizer is the finalizer added to every kflare-managed resource.
	// Its presence prevents Kubernetes from deleting the object until the
	// controller has had a chance to clean up the corresponding Cloudflare
	// resource (unless its deletion policy keeps it).
	Finalizer = "kflare.dev/finalizer"

	// DeletionPolicyAnnotation chooses what happens to the Cloudflare resource
	// when its Kubernetes resource is deleted. Without it, a resource keeps
	// the Cloudflare resource it adopted, and deletes one it created unless
	// the default deletion policy is retain (SetDefaultDeletionPolicy).
	DeletionPolicyAnnotation = "kflare.dev/deletion-policy"

	// DeletionPolicyRetain keeps the Cloudflare resource when its Kubernetes
	// resource is deleted.
	DeletionPolicyRetain = "retain"

	// DeletionPolicyDelete deletes the Cloudflare resource when its Kubernetes
	// resource is deleted, even one the resource adopted. An annotation with
	// any other value counts as none.
	DeletionPolicyDelete = "delete"

	// AdoptionPolicyAnnotation lets a resource take over an existing
	// Cloudflare resource that kflare did not create. Without it, a resource
	// only creates: it leaves an existing one alone and reports it.
	AdoptionPolicyAnnotation = "kflare.dev/adoption-policy"

	// AdoptionPolicyAdopt adopts an existing Cloudflare resource and never
	// creates one.
	AdoptionPolicyAdopt = "adopt"

	// AdoptionPolicyAdoptOrCreate adopts an existing Cloudflare resource, or
	// creates one when there is none.
	AdoptionPolicyAdoptOrCreate = "adopt-or-create"

	// AdoptedAnnotation is set to "true" by kflare on a resource that adopted
	// its Cloudflare resource rather than creating it.
	AdoptedAnnotation = "kflare.dev/adopted"

	// CreatingAnnotation holds the key of the Cloudflare resource kflare is
	// about to create for a resource, from just before the create call until
	// the resource records what it created. A later reconcile that finds an
	// object with that key knows it as its own, not as one to adopt.
	CreatingAnnotation = "kflare.dev/creating"

	// PausedAnnotation, set to "true", tells kflare to leave a resource alone
	// until the annotation is removed: kflare makes no Cloudflare call for
	// it, and does not finish deleting it.
	PausedAnnotation = "kflare.dev/paused"
)

// Adopted reports whether kflare adopted obj's Cloudflare resource.
func Adopted(obj client.Object) bool {
	return obj.GetAnnotations()[AdoptedAnnotation] == "true"
}

// Paused reports whether obj's PausedAnnotation is "true". Any other value,
// "false" included, counts as none.
func Paused(obj client.Object) bool {
	return obj.GetAnnotations()[PausedAnnotation] == "true"
}

// retainByDefault is the default deletion policy: whether a resource without
// a deletion-policy annotation keeps the Cloudflare resource it created.
var retainByDefault atomic.Bool

// SetDefaultDeletionPolicy sets the deletion policy of resources whose
// annotation sets none: DeletionPolicyDelete, the default, or
// DeletionPolicyRetain, with which deleting a resource keeps the Cloudflare
// resource it created too. The manager calls it once, before its controllers
// start (--default-deletion-policy).
func SetDefaultDeletionPolicy(policy string) error {
	switch policy {
	case DeletionPolicyDelete:
		retainByDefault.Store(false)
	case DeletionPolicyRetain:
		retainByDefault.Store(true)
	default:
		return fmt.Errorf("unknown deletion policy %q: want %q or %q",
			policy, DeletionPolicyDelete, DeletionPolicyRetain)
	}
	return nil
}

// RetainOnDelete reports whether the Cloudflare resource is kept when obj is
// deleted. obj's deletion-policy annotation decides when it says retain or
// delete. Otherwise an adopted Cloudflare resource is kept, and a created one
// follows the default deletion policy. Deleting something kflare did not
// create is the one step that cannot be undone, so an adopted resource is
// only deleted on request.
func RetainOnDelete(obj client.Object) bool {
	switch obj.GetAnnotations()[DeletionPolicyAnnotation] {
	case DeletionPolicyRetain:
		return true
	case DeletionPolicyDelete:
		return false
	}
	return Adopted(obj) || retainByDefault.Load()
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
