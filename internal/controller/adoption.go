/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"maps"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// A resource creates its Cloudflare object. It takes over an existing object
// that kflare did not create only when its kflare.dev/adoption-policy
// annotation asks for it: adopt, or adopt-or-create. Without the annotation,
// an existing object is left untouched and the resource reports
// AlreadyExists; with adopt, a missing object is reported as NotFound rather
// than created. kflare marks a resource that adopted its object with
// kflare.dev/adopted, and keeps an adopted object when the resource is
// deleted unless its deletion policy says delete (reconciler.RetainOnDelete).
//
// An object a resource created must never count as one to adopt, even when
// the reconcile that created it failed before recording it. So, just before
// the create call, kflare records the object's key in kflare.dev/creating, and
// an object found with that key belongs to the resource. The annotation goes
// once the resource records what it created: with its claim label, in the
// same patch, or for kinds without one, on a later reconcile that finds the
// object by the ID in status.

const (
	// reasonAlreadyExists reports an existing Cloudflare object that the
	// resource may not adopt: it has no adoption policy.
	reasonAlreadyExists = "AlreadyExists"
	// reasonNotFound reports that the adopt policy found nothing to adopt.
	reasonNotFound = "NotFound"
	// reasonInvalidAdoptionPolicy reports an adoption-policy annotation kflare
	// does not know.
	reasonInvalidAdoptionPolicy = "InvalidAdoptionPolicy"
)

// adoptionPolicy returns obj's adoption policy: "" (create only),
// reconciler.AdoptionPolicyAdopt or reconciler.AdoptionPolicyAdoptOrCreate.
// Any other value is an error, reported rather than read as one of those,
// since a misspelt policy would otherwise create or adopt against the user's
// intent.
func adoptionPolicy(obj client.Object) (string, *conditionError) {
	switch policy := obj.GetAnnotations()[reconciler.AdoptionPolicyAnnotation]; policy {
	case "", reconciler.AdoptionPolicyAdopt, reconciler.AdoptionPolicyAdoptOrCreate:
		return policy, nil
	default:
		return "", &conditionError{
			Reason: reasonInvalidAdoptionPolicy,
			Message: fmt.Sprintf("Annotation %s is %q: use %q or %q, or remove it to only create",
				reconciler.AdoptionPolicyAnnotation, policy,
				reconciler.AdoptionPolicyAdopt, reconciler.AdoptionPolicyAdoptOrCreate),
		}
	}
}

// mayAdopt returns nil when obj's adoption policy lets it adopt the existing
// object that what describes (for example `KV namespace "cache"`), and the
// condition to report otherwise.
func mayAdopt(obj client.Object, what string) *conditionError {
	policy, condErr := adoptionPolicy(obj)
	if condErr != nil {
		return condErr
	}
	if policy == "" {
		return &conditionError{
			Reason: reasonAlreadyExists,
			Message: fmt.Sprintf("%s already exists and kflare did not create it. To manage it, set the annotation "+
				"%s: %s", what, reconciler.AdoptionPolicyAnnotation, reconciler.AdoptionPolicyAdopt),
		}
	}
	return nil
}

// mayCreate returns nil when obj's adoption policy lets it create the object
// that what describes, and the condition to report otherwise.
func mayCreate(obj client.Object, what string) *conditionError {
	policy, condErr := adoptionPolicy(obj)
	if condErr != nil {
		return condErr
	}
	if policy == reconciler.AdoptionPolicyAdopt {
		return &conditionError{
			Reason: reasonNotFound,
			Message: fmt.Sprintf("%s does not exist, and the adoption policy %s never creates one",
				what, reconciler.AdoptionPolicyAdopt),
		}
	}
	return nil
}

// creating reports whether obj was about to create the object with key:
// an earlier reconcile created it, or tried to, but did not record it.
func creating(obj client.Object, key string) bool {
	return obj.GetAnnotations()[reconciler.CreatingAnnotation] == key
}

// markCreating records on obj, before it creates the object with key, that it
// is about to (see creating). It also drops the adopted annotation: the
// object obj manages from now on is one kflare creates.
func markCreating(ctx context.Context, c client.Writer, obj client.Object, key string) error {
	return patchMetadata(ctx, c, obj, func(_, annotations map[string]string) {
		annotations[reconciler.CreatingAnnotation] = key
		delete(annotations, reconciler.AdoptedAnnotation)
	})
}

// markAdopted records on obj, which has no claim label, that it adopted its
// object. Callers patch it before writing status, so a resource that manages
// an adopted object always says so.
func markAdopted(ctx context.Context, c client.Writer, obj client.Object) error {
	return patchMetadata(ctx, c, obj, func(_, annotations map[string]string) {
		annotations[reconciler.AdoptedAnnotation] = "true"
		delete(annotations, reconciler.CreatingAnnotation)
	})
}

// clearCreating drops the creating annotation from obj, which has no claim
// label, once its status records the object it created.
func clearCreating(ctx context.Context, c client.Writer, obj client.Object) error {
	return patchMetadata(ctx, c, obj, func(_, annotations map[string]string) {
		delete(annotations, reconciler.CreatingAnnotation)
	})
}

// patchMetadata applies mutate to copies of obj's labels and annotations and
// patches obj with them, only when that changes something.
func patchMetadata(
	ctx context.Context,
	c client.Writer,
	obj client.Object,
	mutate func(labels, annotations map[string]string),
) error {
	labels := maps.Clone(obj.GetLabels())
	if labels == nil {
		labels = map[string]string{}
	}
	annotations := maps.Clone(obj.GetAnnotations())
	if annotations == nil {
		annotations = map[string]string{}
	}
	mutate(labels, annotations)
	if maps.Equal(labels, obj.GetLabels()) && maps.Equal(annotations, obj.GetAnnotations()) {
		return nil
	}
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	obj.SetLabels(labels)
	obj.SetAnnotations(annotations)
	return c.Patch(ctx, obj, patch)
}
