/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudflare/cloudflare-go/v7/kv"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

// These specs cover the Events the status helpers record when they change a
// resource's Ready condition. Every kind reports through the same helpers,
// so a CloudflareAccount stands in for all of them.
var _ = Describe("Ready Events", func() {
	const name = "ready-events"
	ctx := context.Background()
	var recorder *events.FakeRecorder
	var account *cloudflarev1alpha1.CloudflareAccount

	BeforeEach(func() {
		recorder = events.NewFakeRecorder(10)
		SetEventRecorder(recorder)
		DeferCleanup(func() { SetEventRecorder(nil) })
		account = &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "acct-" + name,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: name, Namespace: "default", Key: "CF_API_TOKEN"},
			},
		}
		Expect(k8sClient.Create(ctx, account)).To(Succeed())
		DeferCleanup(func() {
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: name})
		})
	})

	It("records a Warning when Ready turns False and a Normal when it turns True", func() {
		Expect(updateNotReady(ctx, k8sClient, account, "SecretNotFound", "no secret")).To(Succeed())
		Expect(recorder.Events).To(Receive(Equal("Warning SecretNotFound no secret")))
		Expect(updateReady(ctx, k8sClient, account, "Validated", "token valid")).To(Succeed())
		Expect(recorder.Events).To(Receive(Equal("Normal Validated token valid")))
		Expect(recorder.Events).To(BeEmpty())
	})

	It("records a new status or reason, and nothing for a new message alone", func() {
		Expect(updateNotReady(ctx, k8sClient, account, "APIError", "first")).To(Succeed())
		Expect(recorder.Events).To(Receive(Equal("Warning APIError first")))
		Expect(updateNotReady(ctx, k8sClient, account, "APIError", "second")).To(Succeed())
		Expect(recorder.Events).To(BeEmpty())
		Expect(updateNotReady(ctx, k8sClient, account, "TerminalError", "third")).To(Succeed())
		Expect(recorder.Events).To(Receive(Equal("Warning TerminalError third")))
		Expect(updateReady(ctx, k8sClient, account, "Validated", "valid")).To(Succeed())
		Expect(recorder.Events).To(Receive(Equal("Normal Validated valid")))
		Expect(updateReady(ctx, k8sClient, account, "Validated", "still valid")).To(Succeed())
		Expect(recorder.Events).To(BeEmpty())
		Expect(updateNotReady(ctx, k8sClient, account, "Validated", "same reason")).To(Succeed())
		Expect(recorder.Events).To(Receive(Equal("Warning Validated same reason")), "a new status alone is recorded")
	})

	It("records nothing when the status write fails", func() {
		stale := account.DeepCopy()
		Expect(updateNotReady(ctx, k8sClient, account, "APIError", "first")).To(Succeed())
		Expect(recorder.Events).To(Receive())
		err := updateReady(ctx, k8sClient, stale, "Validated", "from a stale copy")
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "got %v", err)
		Expect(recorder.Events).To(BeEmpty())
	})

	It("shortens a note longer than an Event holds, at a character boundary", func() {
		message := strings.Repeat("é", 600) // 1,200 bytes
		Expect(updateNotReady(ctx, k8sClient, account, "APIError", message)).To(Succeed())
		var event string
		Expect(recorder.Events).To(Receive(&event))
		note := strings.TrimPrefix(event, "Warning APIError ")
		Expect(len(note)).To(BeNumerically("<=", eventNoteLimit))
		Expect(utf8.ValidString(note)).To(BeTrue())
		Expect(note).To(Equal(message[:len(note)-len("...")] + "..."))
	})

	It("records nothing without a recorder", func() {
		SetEventRecorder(nil)
		Expect(updateNotReady(ctx, k8sClient, account, "APIError", "first")).To(Succeed())
		Expect(recorder.Events).To(BeEmpty())
	})
})

// These specs cover a reconcile that runs out of time: controller-runtime
// cancels its context once the manager's reconcile timeout passes, and every
// Cloudflare call then fails with the context's error.
var _ = Describe("A reconcile that runs out of time", func() {
	const name = "reconcile-out-of-time"
	ctx := context.Background()
	var account *cloudflarev1alpha1.CloudflareAccount

	BeforeEach(func() {
		account = &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "acct-" + name,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: name, Namespace: "default", Key: "CF_API_TOKEN"},
			},
		}
		Expect(k8sClient.Create(ctx, account)).To(Succeed())
		DeferCleanup(func() {
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: name})
		})
	})

	ready := func() *metav1.Condition {
		stored := &cloudflarev1alpha1.CloudflareAccount{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, stored)).To(Succeed())
		return meta.FindStatusCondition(stored.Status.Conditions, cloudflarev1alpha1.ConditionReady)
	}

	It("still reports APIError, naming the timeout, and returns the error for a retry", func() {
		cause := errors.New("reconciliation timeout")
		expired, cancel := context.WithDeadlineCause(ctx, time.Now().Add(-time.Second), cause)
		defer cancel()
		result, err := handleCloudflareError(expired, k8sClient, account, expired.Err())
		Expect(err).To(MatchError(context.DeadlineExceeded))
		Expect(result).To(Equal(ctrl.Result{}))
		cond := ready()
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("APIError"))
		Expect(cond.Message).To(Equal("Cloudflare API error: context deadline exceeded (reconciliation timeout)"))
	})

	It("still reports a terminal error that came back as the reconcile ran out of time", func() {
		expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
		defer cancel()
		result, err := handleCloudflareError(expired, k8sClient, account, cloudflareError(400, 10000))
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
		Expect(ready().Reason).To(Equal("TerminalError"))
	})

	It("writes nothing once the reconcile is cancelled for another reason", func() {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := handleCloudflareError(cancelled, k8sClient, account, cancelled.Err())
		Expect(err).To(MatchError(context.Canceled))
		Expect(ready()).To(BeNil())
	})

	It("names no timeout for a call that failed within the reconcile's time", func() {
		inTime, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		_, err := handleCloudflareError(inTime, k8sClient, account, errors.New("connection reset"))
		Expect(err).To(MatchError("connection reset"))
		Expect(ready().Message).To(Equal("Cloudflare API error: connection reset"))
	})
})

// These specs cover the cf-ray ID in a failure's condition, with a
// CloudflareAccount standing in for the kinds. metrics_test.go checks in a
// running manager what they guard against: a new message per attempt, which
// reconciles a failing resource in a loop.
var _ = Describe("The cf-ray ID of a failed call", func() {
	const name = "ray-id"
	ctx := context.Background()
	var recorder *events.FakeRecorder

	BeforeEach(func() {
		recorder = events.NewFakeRecorder(10)
		SetEventRecorder(recorder)
		DeferCleanup(func() { SetEventRecorder(nil) })
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "acct-" + name,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: name, Namespace: "default", Key: "CF_API_TOKEN"},
			},
		})).To(Succeed())
		DeferCleanup(func() {
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: name})
		})
	})

	stored := func() *cloudflarev1alpha1.CloudflareAccount {
		GinkgoHelper()
		obj := &cloudflarev1alpha1.CloudflareAccount{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, obj)).To(Succeed())
		return obj
	}
	ready := func() *metav1.Condition {
		GinkgoHelper()
		return meta.FindStatusCondition(stored().Status.Conditions, cloudflarev1alpha1.ConditionReady)
	}
	// failure is Cloudflare's answer with status, code and the ID ray, as v7
	// reports it.
	failure := func(status, code int, ray string) error {
		err := v7APIError(http.MethodGet, "accounts/acct", status, code, "refused")
		var v7Err *kv.Error
		Expect(errors.As(err, &v7Err)).To(BeTrue())
		v7Err.Response.Header = http.Header{"Cf-Ray": {ray}}
		return err
	}

	DescribeTable("is reported once, however many attempts follow",
		func(status int, reason, message string) {
			_, _ = handleCloudflareError(ctx, k8sClient, stored(), failure(status, 10000, "ray-1"))
			Expect(ready().Reason).To(Equal(reason))
			Expect(ready().Message).To(Equal(message + " (cf-ray ray-1)"))
			Expect(recorder.Events).To(Receive(ContainSubstring("(cf-ray ray-1)")), "the Event has the ID too")

			version := stored().ResourceVersion
			_, _ = handleCloudflareError(ctx, k8sClient, stored(), failure(status, 10000, "ray-2"))
			Expect(ready().Message).To(Equal(message + " (cf-ray ray-1)"))
			Expect(stored().ResourceVersion).To(Equal(version),
				"a status write is a watch event, which reconciles the resource at once, past its back-off")
			Expect(recorder.Events).NotTo(Receive())
		},
		Entry("a terminal error", http.StatusForbidden, "TerminalError",
			"Terminal Cloudflare API error: refused (10000)"),
		Entry("a retryable error", http.StatusBadGateway, "APIError", "Cloudflare API error: refused (10000)"),
	)

	It("is the new failure's when the failure changes", func() {
		_, _ = handleCloudflareError(ctx, k8sClient, stored(), failure(http.StatusForbidden, 10000, "ray-1"))
		_, _ = handleCloudflareError(ctx, k8sClient, stored(), failure(http.StatusForbidden, 10001, "ray-2"))
		Expect(ready().Message).To(Equal("Terminal Cloudflare API error: refused (10001) (cf-ray ray-2)"))
	})

	It("leaves a condition as it was for an error without one", func() {
		_, err := handleCloudflareError(ctx, k8sClient, stored(), errors.New("connection reset"))
		Expect(err).To(MatchError("connection reset"))
		Expect(ready().Message).To(Equal("Cloudflare API error: connection reset"))
	})

	It("stays out of the error controller-runtime logs, which holds what v7 said", func() {
		failed := failure(http.StatusBadGateway, 10000, "ray-1")
		_, err := handleCloudflareError(ctx, k8sClient, stored(), failed)
		Expect(err).To(BeIdenticalTo(failed))
	})

	DescribeTable("withRayID",
		func(current, message, ray, want string) {
			obj := &cloudflarev1alpha1.CloudflareAccount{}
			if current != "" {
				obj.Status.Conditions = []metav1.Condition{{Type: cloudflarev1alpha1.ConditionReady, Message: current}}
			}
			Expect(withRayID(obj, message, ray)).To(Equal(want))
		},
		Entry("a first failure", "", "failed", "ray-1", "failed (cf-ray ray-1)"),
		Entry("the same failure", "failed (cf-ray ray-1)", "failed", "ray-2", "failed (cf-ray ray-1)"),
		Entry("another failure", "other (cf-ray ray-1)", "failed", "ray-2", "failed (cf-ray ray-2)"),
		Entry("a failure that starts the same", "failed again (cf-ray ray-1)", "failed", "ray-2",
			"failed (cf-ray ray-2)"),
		Entry("the same failure, reported without an ID before", "failed", "failed", "ray-2",
			"failed (cf-ray ray-2)"),
		Entry("a failure without an ID", "failed (cf-ray ray-1)", "failed", "", "failed"),
	)
})
