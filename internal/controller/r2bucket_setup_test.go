/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

var _ = Describe("defaultR2BucketAPI", func() {
	It("returns an error for an empty token", func() {
		_, err := defaultR2BucketAPI("")
		Expect(err).To(HaveOccurred())
	})

	It("returns the production client for a token", func() {
		api, err := defaultR2BucketAPI("token")
		Expect(err).NotTo(HaveOccurred())
		Expect(api).To(BeAssignableToTypeOf(&cfpkg.Client{}))
	})

	It("is used when no factory is injected", func() {
		_, err := (&R2BucketReconciler{}).newR2BucketAPI("")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("R2BucketReconciler SetupWithManager", func() {
	It("registers without error", func() {
		mgr, err := manager.New(cfg, manager.Options{})
		Expect(err).NotTo(HaveOccurred())
		r := &R2BucketReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
	})
})

var _ = Describe("metaTime", func() {
	It("converts a timestamp, and keeps a missing one missing", func() {
		t := time.Date(2026, 10, 8, 0, 4, 14, 0, time.UTC)
		Expect(metaTime(&t).Time).To(BeTemporally("==", t))
		Expect(metaTime(nil)).To(BeNil())
	})
})

var _ = Describe("R2BucketReconciler mappers", func() {
	const ns = "default"
	ctx := context.Background()
	r := &R2BucketReconciler{}

	BeforeEach(func() {
		r.Client = k8sClient
		b := &cloudflarev1alpha1.R2Bucket{
			ObjectMeta: metav1.ObjectMeta{Name: "r2m-bucket", Namespace: ns},
			Spec: cloudflarev1alpha1.R2BucketSpec{
				AccountRef: corev1.LocalObjectReference{Name: "r2m-account"}, Name: "r2m-assets",
			},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.R2Bucket{}, types.NamespacedName{Name: b.Name, Namespace: ns})
	})

	It("maps a CloudflareAccount to the buckets that use it", func() {
		account := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Name: "r2m-account"}}
		Expect(r.bucketsForAccount(ctx, account)).To(ConsistOf(
			reconcile.Request{NamespacedName: types.NamespacedName{Name: "r2m-bucket", Namespace: ns}}))
		other := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Name: "r2m-other"}}
		Expect(r.bucketsForAccount(ctx, other)).To(BeEmpty())
	})

	It("returns nil when the list fails", func() {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		account := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Name: "r2m-account"}}
		Expect(r.bucketsForAccount(cancelled, account)).To(BeNil())
	})

	It("re-triggers each bound bucket that is being deleted when a WorkerScript goes", func() {
		ws := &cloudflarev1alpha1.WorkerScript{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns},
			Spec: cloudflarev1alpha1.WorkerScriptSpec{Bindings: []cloudflarev1alpha1.WorkerBinding{
				{Name: "A", R2BucketRef: &corev1.LocalObjectReference{Name: "r2m-bucket"}},
				{Name: "B", R2BucketRef: &corev1.LocalObjectReference{Name: "r2m-missing"}},
			}},
		}
		Expect(r.deletingR2BucketsOf(ctx, ws)).To(BeEmpty(), "the bucket is not being deleted yet")

		b := &cloudflarev1alpha1.R2Bucket{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "r2m-bucket", Namespace: ns}, b)).To(Succeed())
		b.Finalizers = []string{"test/hold"}
		Expect(k8sClient.Update(ctx, b)).To(Succeed())
		Expect(k8sClient.Delete(ctx, b)).To(Succeed())
		Expect(r.deletingR2BucketsOf(ctx, ws)).To(ConsistOf(
			reconcile.Request{NamespacedName: types.NamespacedName{Name: "r2m-bucket", Namespace: ns}}))
	})
})

var _ = Describe("R2Bucket as a dependent", func() {
	It("counts as a user of its CloudflareAccount", func() {
		ctx := context.Background()
		b := &cloudflarev1alpha1.R2Bucket{
			ObjectMeta: metav1.ObjectMeta{Name: "r2d-bucket", Namespace: "default"},
			Spec: cloudflarev1alpha1.R2BucketSpec{
				AccountRef: corev1.LocalObjectReference{Name: "r2d-account"}, Name: "r2d-assets",
			},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.R2Bucket{}, types.NamespacedName{Name: b.Name, Namespace: b.Namespace})

		deps, err := dependentsOf(ctx, k8sClient, "", "r2d-account", accountDependents)
		Expect(err).NotTo(HaveOccurred())
		Expect(deps).To(ConsistOf("R2Bucket default/r2d-bucket"))
	})
})
