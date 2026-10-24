/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
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

// reasonNameConflict reports that the Cloudflare object a resource names is
// already taken: managed by another resource, or of another kind.
const reasonNameConflict = "NameConflict"

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
	return updateReadyCondition(ctx, c, obj, metav1.ConditionTrue, reason, message)
}

// updateNotReady sets Ready=False and writes obj's status.
func updateNotReady(ctx context.Context, c client.StatusClient, obj conditionedObject, reason, message string) error {
	return updateReadyCondition(ctx, c, obj, metav1.ConditionFalse, reason, message)
}

// updateReadyCondition sets obj's Ready condition and writes obj's status.
// Once the write succeeds, a new status or reason is recorded as an Event
// (recordReadyChange); a new message alone is not.
func updateReadyCondition(
	ctx context.Context,
	c client.StatusClient,
	obj conditionedObject,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	old := meta.FindStatusCondition(obj.GetConditions(), cloudflarev1alpha1.ConditionReady)
	changed := old == nil || old.Status != status || old.Reason != reason
	setReady(obj, status, reason, message)
	if err := c.Status().Update(ctx, obj); err != nil {
		return err
	}
	if changed {
		recordReadyChange(obj, status, reason, message)
	}
	return nil
}

// readyEvents records the Events of Ready changes. Until SetEventRecorder
// is called, none is recorded.
var readyEvents atomic.Pointer[events.EventRecorder]

// SetEventRecorder makes the status helpers record every change of a
// resource's Ready condition as an Event, so that kubectl describe and
// kubectl get events show its history. cmd/main.go calls it once, with the
// manager's recorder, before the manager starts.
func SetEventRecorder(recorder events.EventRecorder) {
	readyEvents.Store(&recorder)
}

// eventNoteLimit is the longest note, in bytes, the API server accepts in an
// Event.
const eventNoteLimit = 1024

//+kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// recordReadyChange records obj's new Ready condition as an Event: Normal
// when Ready is True and Warning when it is False, with the condition's
// reason, and its message as the note.
func recordReadyChange(obj conditionedObject, status metav1.ConditionStatus, reason, message string) {
	recorder := readyEvents.Load()
	if recorder == nil || *recorder == nil {
		return
	}
	eventType := corev1.EventTypeWarning
	if status == metav1.ConditionTrue {
		eventType = corev1.EventTypeNormal
	}
	(*recorder).Eventf(obj, nil, eventType, reason, "Reconcile", "%s", eventNote(message))
}

// eventNote is message, shortened at a character boundary to fit an Event's
// note, and then ending in "...".
func eventNote(message string) string {
	if len(message) <= eventNoteLimit {
		return message
	}
	end := eventNoteLimit - len("...")
	for !utf8.RuneStart(message[end]) {
		end--
	}
	return message[:end] + "..."
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
// controller-runtime retries with back-off. The condition shows Cloudflare's
// messages and codes (cfpkg.ErrorMessage); the returned error, which
// controller-runtime logs, keeps everything.
func handleCloudflareError(ctx context.Context, c client.StatusClient, obj conditionedObject, err error) (ctrl.Result, error) {
	message := cfpkg.ErrorMessage(err)
	if cfpkg.IsTerminalError(err) {
		return ctrl.Result{}, updateNotReady(ctx, c, obj, "TerminalError",
			fmt.Sprintf("Terminal Cloudflare API error: %s", message))
	}
	if statusErr := updateNotReady(ctx, c, obj, "APIError", fmt.Sprintf("Cloudflare API error: %s", message)); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	return ctrl.Result{}, err
}
