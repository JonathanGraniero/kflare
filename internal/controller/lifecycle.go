/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// startReconcile reads the resource req names into obj and runs the steps
// every shared reconciler starts with: a paused resource only reports so
// (updatePaused), even when deleted; a deleted one goes to reconcileDelete;
// one without the finalizer gets it, and that update re-triggers the
// reconcile. done ends the reconcile with result and err.
//
// A resource kflare only observes (reconciler.Observed) goes on only when its
// kind can be observed (observable): its reconciler then asks Observed again,
// and writes nothing to Cloudflare. Another kind's reports
// ObserveNotSupported, with no call to Cloudflare, and a deleted one leaves
// its Cloudflare object. An unknown management policy only reports so, even
// when deleted: nobody said what to do with the object.
func startReconcile(ctx context.Context, c client.Client, req ctrl.Request, obj conditionedObject, observable bool,
	reconcileDelete func() (ctrl.Result, error)) (done bool, result ctrl.Result, err error) {
	if err := c.Get(ctx, req.NamespacedName, obj); err != nil {
		return true, ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if reconciler.Paused(obj) {
		return true, ctrl.Result{}, updatePaused(ctx, c, obj)
	}
	observed, err := reconciler.Observed(obj)
	if err != nil {
		return true, ctrl.Result{}, updateNotReady(ctx, c, obj, reasonInvalidManagementPolicy, upperFirst(err.Error()))
	}
	if observed && !observable {
		if !obj.GetDeletionTimestamp().IsZero() {
			if controllerutil.ContainsFinalizer(obj, reconciler.Finalizer) {
				recordWarning(obj, reasonObserveNotSupported, "Delete", observeNotSupportedMessage+
					"; its Cloudflare object stays in Cloudflare")
			}
			_, err := reconciler.RemoveFinalizer(ctx, c, obj)
			return true, ctrl.Result{}, err
		}
		return true, ctrl.Result{}, updateNotReady(ctx, c, obj, reasonObserveNotSupported, observeNotSupportedMessage)
	}
	if !obj.GetDeletionTimestamp().IsZero() {
		result, err := reconcileDelete()
		return true, result, err
	}
	added, err := reconciler.EnsureFinalizer(ctx, c, obj)
	if added && err == nil {
		log.FromContext(ctx).Info("Added finalizer", "resource", req.NamespacedName)
	}
	return added || err != nil, ctrl.Result{}, err
}

// The reasons and messages of observe mode (reconciler.Observed).
const (
	// reasonInSync reports, with Ready=True, an observed object that matches
	// the spec, and reasonDriftDetected one that does not.
	reasonInSync        = "InSync"
	reasonDriftDetected = "DriftDetected"
	// reasonObserveNotSupported reports an observed resource of a kind kflare
	// cannot observe yet, and reasonInvalidManagementPolicy an annotation
	// kflare does not know.
	reasonObserveNotSupported     = "ObserveNotSupported"
	reasonInvalidManagementPolicy = "InvalidManagementPolicy"

	observeNotSupportedMessage = "kflare only observes this resource (the " + reconciler.ManagementPolicyAnnotation +
		" annotation, or --observe-only), which it cannot do for this kind yet: it makes no call to Cloudflare for it"
)

// upperFirst returns s with its first letter in upper case, as a condition's
// message starts.
func upperFirst(s string) string {
	return strings.ToUpper(s[:1]) + s[1:]
}

// deleteUnlessRetained finishes deleting obj: cleanup removes its Cloudflare
// object unless its deletion policy keeps it, then the finalizer goes.
func deleteUnlessRetained(ctx context.Context, c client.Client, obj conditionedObject,
	cleanup func() error) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(obj, reconciler.Finalizer) {
		return ctrl.Result{}, nil
	}
	if !reconciler.RetainOnDelete(obj) {
		if err := cleanup(); err != nil {
			return retryCleanup(err)
		}
	}
	_, err := reconciler.RemoveFinalizer(ctx, c, obj)
	return ctrl.Result{}, err
}
