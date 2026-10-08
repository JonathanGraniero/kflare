/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// credentialsRetryInterval is how long to wait before retrying when a
// resource cannot proceed because of something it does not watch, such as
// its account's API token Secret or a Secret kflare does not own.
const credentialsRetryInterval = time.Minute

// conditionedObject is a kflare resource whose status carries the Ready
// condition. Every type in api/v1alpha1 implements it.
type conditionedObject interface {
	client.Object
	GetConditions() []metav1.Condition
	SetConditions([]metav1.Condition)
}

// conditionError explains why a reconcile cannot proceed. Reason is a
// CamelCase condition reason suitable for a Ready=False condition.
type conditionError struct {
	Reason  string
	Message string
}

func (e *conditionError) Error() string { return e.Message }

// reasonAccountMismatch reports a reference to a resource that uses a
// different CloudflareAccount than the resource referring to it.
const reasonAccountMismatch = "AccountMismatch"

// setReady sets obj's Ready condition for its current generation, in memory.
func setReady(obj conditionedObject, status metav1.ConditionStatus, reason, message string) {
	conditions := obj.GetConditions()
	reconciler.SetCondition(&conditions, cloudflarev1alpha1.ConditionReady,
		status, reason, message, obj.GetGeneration())
	obj.SetConditions(conditions)
}

// updateReady sets Ready=True and writes obj's status. Callers set any other
// status fields first, so they are written in the same update.
func updateReady(ctx context.Context, c client.StatusClient, obj conditionedObject, reason, message string) error {
	setReady(obj, metav1.ConditionTrue, reason, message)
	return c.Status().Update(ctx, obj)
}

// updateNotReady sets Ready=False and writes obj's status.
func updateNotReady(ctx context.Context, c client.StatusClient, obj conditionedObject, reason, message string) error {
	setReady(obj, metav1.ConditionFalse, reason, message)
	return c.Status().Update(ctx, obj)
}

// notReadyRetryAfter reports condErr on obj and retries after
// credentialsRetryInterval. It is used for failures caused by something this
// resource does not watch, such as its account's credentials.
func notReadyRetryAfter(
	ctx context.Context,
	c client.StatusClient,
	obj conditionedObject,
	condErr *conditionError,
) (ctrl.Result, error) {
	return ctrl.Result{RequeueAfter: credentialsRetryInterval},
		updateNotReady(ctx, c, obj, condErr.Reason, condErr.Message)
}

// invalidToken reports that no Cloudflare client could be built from the
// account's token. Fixing the token changes the account, which re-triggers
// this resource.
func invalidToken(ctx context.Context, c client.StatusClient, obj conditionedObject, err error) (ctrl.Result, error) {
	return ctrl.Result{}, updateNotReady(ctx, c, obj, "InvalidToken",
		fmt.Sprintf("Failed to create Cloudflare client: %v", err))
}

// handleCloudflareError reports a failed Cloudflare API call on obj. Terminal
// errors (4xx) are not requeued: they need a change to the spec or the
// credentials, which re-triggers the reconcile. Anything else is returned so
// controller-runtime retries with back-off.
func handleCloudflareError(ctx context.Context, c client.StatusClient, obj conditionedObject, err error) (ctrl.Result, error) {
	if cfpkg.IsTerminalError(err) {
		return ctrl.Result{}, updateNotReady(ctx, c, obj, "TerminalError",
			fmt.Sprintf("Terminal Cloudflare API error: %v", err))
	}
	if statusErr := updateNotReady(ctx, c, obj, "APIError", fmt.Sprintf("Cloudflare API error: %v", err)); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	return ctrl.Result{}, err
}
