/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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
	// retry asks for a check after a minute, for what nothing watches.
	retry bool
}

func (e *conditionError) Error() string { return e.Message }

// reasonAccountMismatch reports a reference to a resource that uses a
// different CloudflareAccount than the resource referring to it.
const reasonAccountMismatch = "AccountMismatch"

// reasonNameConflict reports that the Cloudflare object a resource names is
// already taken: managed by another resource, or of another kind.
const reasonNameConflict = "NameConflict"

// reasonPaused reports a resource that kflare leaves alone because of its
// kflare.dev/paused annotation (reconciler.Paused).
const reasonPaused = "Paused"

// The messages of a paused resource's Ready condition, while it is in use and
// while it is being deleted.
const (
	pausedMessage = "Reconciling is paused by the " + reconciler.PausedAnnotation +
		" annotation: kflare changes nothing in Cloudflare until the annotation is removed"
	pausedDeletionMessage = "Deletion is paused by the " + reconciler.PausedAnnotation +
		" annotation: kflare goes on with it once the annotation is removed"
)

// isReady reports whether obj's Ready condition is True.
func isReady(obj conditionedObject) bool {
	return meta.IsStatusConditionTrue(obj.GetConditions(), cloudflarev1alpha1.ConditionReady)
}

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

// readyWithDriftCheck ends a successful reconcile: it sets Ready=True and
// writes obj's status, as updateReady does, and schedules obj's next drift
// check (reconciler.NextDriftCheck), since a change made in Cloudflare outside
// kflare triggers no reconcile of its own. Without a drift check interval it
// schedules nothing.
func readyWithDriftCheck(
	ctx context.Context,
	c client.StatusClient,
	obj conditionedObject,
	reason, message string,
) (ctrl.Result, error) {
	if err := updateReady(ctx, c, obj, reason, message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: reconciler.NextDriftCheck()}, nil
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

// updatePaused reports that obj is paused (reconciler.Paused): no Cloudflare
// call, and a deletion keeps its finalizer until the annotation goes. Ready
// takes the reason Paused but keeps its status and observed generation, so a
// Ready resource still serves its dependents and a spec changed while paused
// shows as unobserved; one never reconciled is Unknown. A deleted resource
// without kflare's finalizer is not written.
func updatePaused(ctx context.Context, c client.StatusClient, obj conditionedObject) error {
	deleting := !obj.GetDeletionTimestamp().IsZero()
	if deleting && !controllerutil.ContainsFinalizer(obj, reconciler.Finalizer) {
		return nil
	}
	paused := metav1.Condition{
		Type:               cloudflarev1alpha1.ConditionReady,
		Status:             metav1.ConditionUnknown,
		Reason:             reasonPaused,
		Message:            pausedMessage,
		ObservedGeneration: obj.GetGeneration(),
	}
	if deleting {
		paused.Message = pausedDeletionMessage
	}
	conditions := obj.GetConditions()
	old := meta.FindStatusCondition(conditions, cloudflarev1alpha1.ConditionReady)
	if old != nil {
		paused.Status, paused.ObservedGeneration = old.Status, old.ObservedGeneration
	}
	changed := old == nil || old.Reason != reasonPaused
	meta.SetStatusCondition(&conditions, paused)
	obj.SetConditions(conditions)
	if err := c.Status().Update(ctx, obj); err != nil {
		return err
	}
	if changed {
		recordReadyChange(obj, paused.Status, paused.Reason, paused.Message)
	}
	return nil
}

// statusWriteTimeout bounds the status write of a reconcile that ran out of
// time (statusContext).
const statusWriteTimeout = 10 * time.Second

// statusContext returns the context handleCloudflareError writes status with:
// ctx, or, once ctx's deadline (--reconcile-timeout) has passed, a copy with
// statusWriteTimeout, so a timed-out reconcile still reports why. A reconcile
// cancelled otherwise, as on shutdown, writes nothing.
func statusContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), statusWriteTimeout)
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
// when Ready is True or the resource is paused, which someone asked for, and
// Warning otherwise, with the condition's reason, and its message as the note.
func recordReadyChange(obj conditionedObject, status metav1.ConditionStatus, reason, message string) {
	eventType := corev1.EventTypeWarning
	if status == metav1.ConditionTrue || reason == reasonPaused {
		eventType = corev1.EventTypeNormal
	}
	recordEvent(obj, eventType, reason, "Reconcile", message)
}

// recordWarning records a Warning Event about what action on obj left undone,
// which obj's Ready condition does not show.
func recordWarning(obj conditionedObject, reason, action, message string) {
	recordEvent(obj, corev1.EventTypeWarning, reason, action, message)
}

// recordNormal records a Normal Event about what action did with obj, which
// obj's Ready condition does not show.
func recordNormal(obj conditionedObject, reason, action, message string) {
	recordEvent(obj, corev1.EventTypeNormal, reason, action, message)
}

// recordEvent records an Event on obj, once SetEventRecorder has been called.
func recordEvent(obj conditionedObject, eventType, reason, action, message string) {
	if recorder := readyEvents.Load(); recorder != nil && *recorder != nil {
		(*recorder).Eventf(obj, nil, eventType, reason, action, "%s", eventNote(message))
	}
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

// handleCloudflareError reports a failed Cloudflare API call on obj. A rate
// limit is waited out (rateLimited); a terminal error (4xx) is not requeued,
// as only a change to the spec or credentials can fix it; anything else is
// returned for controller-runtime's back-off. The condition holds
// Cloudflare's messages and codes and the cf-ray ID (withRayID), plus the
// context's cause when the reconcile timed out.
func handleCloudflareError(ctx context.Context, c client.StatusClient, obj conditionedObject, err error) (ctrl.Result, error) {
	message := cfpkg.ErrorMessage(err)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		message = fmt.Sprintf("%s (%v)", message, context.Cause(ctx))
	}
	statusCtx, cancel := statusContext(ctx)
	defer cancel()
	if cfpkg.IsRateLimit(err) {
		return rateLimited(statusCtx, c, obj, rateLimitRetryInterval)
	}
	ray := cfpkg.RayID(err)
	if cfpkg.IsTerminalError(err) {
		return ctrl.Result{}, updateNotReady(statusCtx, c, obj, "TerminalError",
			withRayID(obj, fmt.Sprintf("Terminal Cloudflare API error: %s", message), ray))
	}
	if statusErr := updateNotReady(statusCtx, c, obj, "APIError",
		withRayID(obj, fmt.Sprintf("Cloudflare API error: %s", message), ray)); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	return ctrl.Result{}, err
}

// rayIDPrefix starts what withRayID adds to a message.
const rayIDPrefix = " (cf-ray "

// withRayID ends message with ray, the failed response's ID (Cloudflare
// support asks for it), unless obj already reports message with an ID, which
// it keeps: a status change reconciles at once, past the back-off, so a new
// ID per attempt would call Cloudflare in a loop. The log has every ID.
func withRayID(obj conditionedObject, message, ray string) string {
	if ray == "" {
		return message
	}
	ready := meta.FindStatusCondition(obj.GetConditions(), cloudflarev1alpha1.ConditionReady)
	if ready != nil && strings.HasPrefix(ready.Message, message+rayIDPrefix) {
		return ready.Message
	}
	return message + rayIDPrefix + ray + ")"
}
