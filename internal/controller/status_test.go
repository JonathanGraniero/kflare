/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"strings"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"

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
