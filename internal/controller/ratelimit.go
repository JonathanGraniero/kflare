/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

// reasonRateLimited reports a resource waiting for its token's rate limit.
const reasonRateLimited = "RateLimited"

// rateLimitedMessage is the message of a resource waiting for its token's rate
// limit. It holds no time, so that a resource that waits and returns many
// times writes its status once.
const rateLimitedMessage = "Waiting for the Cloudflare API rate limit of this resource's API token; " +
	"kflare carries on by itself"

// rateLimitRetryInterval is how soon a reconcile that a rate limit stopped
// partway comes back. The next one then waits as long as it has to (admit).
const rateLimitRetryInterval = 5 * time.Second

// admit asks account's token's budget whether a reconcile of obj can call
// Cloudflare, and returns how long it should wait if not. A resource already
// in sync at its current spec is only being checked, so it waits behind those
// with a change to make (cfpkg.Admission's Recheck).
func admit(account *cloudflarev1alpha1.CloudflareAccount, token string, obj conditionedObject) time.Duration {
	admission := cfpkg.Admission{Account: account.Name, Recheck: inSync(obj)}
	if limit := account.Spec.RateLimit; limit != nil {
		admission.RateLimit = int(limit.RequestsPerFiveMinutes)
	}
	return cfpkg.Admit(token, admission)
}

// inSync reports whether obj's last reconcile succeeded on its current spec.
func inSync(obj conditionedObject) bool {
	ready := meta.FindStatusCondition(obj.GetConditions(), cloudflarev1alpha1.ConditionReady)
	return obj.GetDeletionTimestamp().IsZero() && ready != nil &&
		ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == obj.GetGeneration()
}

// rateLimited ends a reconcile that a rate limit stopped: it comes back after
// wait, and its controller goes on to the next resource. A rate limit says
// nothing about the resource, and may touch thousands of them at once, so a
// resource that was Ready stays Ready, with no Event, and the dependents that
// wait for it carry on. Any other reports reason RateLimited, once.
func rateLimited(ctx context.Context, c client.StatusClient, obj conditionedObject, wait time.Duration) (ctrl.Result,
	error) {
	log.FromContext(ctx).V(1).Info("Waiting for the Cloudflare API rate limit", "wait", wait)
	result := ctrl.Result{RequeueAfter: wait}
	ready := meta.FindStatusCondition(obj.GetConditions(), cloudflarev1alpha1.ConditionReady)
	if ready != nil && (ready.Status == metav1.ConditionTrue || ready.Reason == reasonRateLimited) {
		return result, nil
	}
	return result, updateNotReady(ctx, c, obj, reasonRateLimited, rateLimitedMessage)
}

// cleanupDeferred is the error of a deletion's Cloudflare cleanup that must
// wait for a rate limit (retryCleanup).
type cleanupDeferred struct {
	wait time.Duration
}

func (e *cleanupDeferred) Error() string {
	return fmt.Sprintf("waiting %s for the Cloudflare API rate limit", e.wait)
}

// retryCleanup returns the result of a reconcile whose deletion's cleanup
// failed with err. A cleanup that waits for a rate limit comes back after its
// wait, and one a rate limit stopped partway after rateLimitRetryInterval, as
// a sync does (handleCloudflareError). Any other error is returned for
// controller-runtime to retry.
func retryCleanup(err error) (ctrl.Result, error) {
	var deferred *cleanupDeferred
	if errors.As(err, &deferred) {
		return ctrl.Result{RequeueAfter: deferred.wait}, nil
	}
	if cfpkg.IsRateLimit(err) {
		return ctrl.Result{RequeueAfter: rateLimitRetryInterval}, nil
	}
	return ctrl.Result{}, err
}
