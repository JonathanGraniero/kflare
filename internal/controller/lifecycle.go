/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

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
func startReconcile(ctx context.Context, c client.Client, req ctrl.Request, obj conditionedObject,
	reconcileDelete func() (ctrl.Result, error)) (done bool, result ctrl.Result, err error) {
	if err := c.Get(ctx, req.NamespacedName, obj); err != nil {
		return true, ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if reconciler.Paused(obj) {
		return true, ctrl.Result{}, updatePaused(ctx, c, obj)
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
