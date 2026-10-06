/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// These specs cover parents that wait for their dependents before they are
// deleted: a Zone for its DNSRecords and WorkerRoutes, a Tunnel for its
// TunnelConfigurations. The CloudflareAccount case is in
// cloudflareaccount_protection_test.go.
var _ = Describe("Deletion protection for parents", func() {
	const ns = "default"
	ctx := context.Background()

	// create makes obj with the kflare finalizer and registers its cleanup.
	create := func(obj client.Object) {
		obj.SetFinalizers([]string{reconciler.Finalizer})
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(forceDelete, ctx, obj.DeepCopyObject().(client.Object), client.ObjectKeyFromObject(obj))
	}
	readyReason := func(key types.NamespacedName, obj client.Object, conditions func() []metav1.Condition) metav1.Condition {
		Expect(k8sClient.Get(ctx, key, obj)).To(Succeed())
		cond := meta.FindStatusCondition(conditions(), cloudflarev1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		return *cond
	}
	expectGone := func(key types.NamespacedName, obj client.Object) {
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, obj))).To(BeTrue())
	}

	Describe("Zone", func() {
		const zoneName = "prot-zone"
		zoneKey := types.NamespacedName{Name: zoneName, Namespace: ns}
		var fake *fakeZoneAPI
		var r *ZoneReconciler

		BeforeEach(func() {
			fake = &fakeZoneAPI{}
			r = reconcilerWithFakeZoneAPI(fake)
			create(&cloudflarev1alpha1.Zone{
				ObjectMeta: metav1.ObjectMeta{Name: zoneName, Namespace: ns},
				Spec: cloudflarev1alpha1.ZoneSpec{
					Name: "prot-example.com", AccountRef: corev1.LocalObjectReference{Name: "prot-account"},
				},
			})
			create(&cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: "prot-record", Namespace: ns},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					ZoneRef: corev1.LocalObjectReference{Name: zoneName},
					Name:    "www.prot-example.com", Type: "A", Content: "192.0.2.1",
				},
			})
			create(&cloudflarev1alpha1.WorkerRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "prot-route", Namespace: ns},
				Spec: cloudflarev1alpha1.WorkerRouteSpec{
					ZoneRef: corev1.LocalObjectReference{Name: zoneName}, Pattern: "prot-example.com/*",
				},
			})
			Expect(k8sClient.Delete(ctx, &cloudflarev1alpha1.Zone{ObjectMeta: metav1.ObjectMeta{Name: zoneName, Namespace: ns}})).To(Succeed())
		})

		It("waits for its DNSRecords and WorkerRoutes, then deletes", func() {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())
			zone := &cloudflarev1alpha1.Zone{}
			cond := readyReason(zoneKey, zone, func() []metav1.Condition { return zone.Status.Conditions })
			Expect(cond.Reason).To(Equal("InUse"))
			Expect(cond.Message).To(ContainSubstring("DNSRecord default/prot-record"))
			Expect(cond.Message).To(ContainSubstring("WorkerRoute default/prot-route"))
			Expect(fake.deleteCalled).To(BeFalse())

			forceDelete(ctx, &cloudflarev1alpha1.DNSRecord{}, types.NamespacedName{Name: "prot-record", Namespace: ns})
			forceDelete(ctx, &cloudflarev1alpha1.WorkerRoute{}, types.NamespacedName{Name: "prot-route", Namespace: ns})
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())
			expectGone(zoneKey, &cloudflarev1alpha1.Zone{})
		})

		It("is re-triggered by a dependent's deletion only while it is being deleted", func() {
			record := &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns},
				Spec:       cloudflarev1alpha1.DNSRecordSpec{ZoneRef: corev1.LocalObjectReference{Name: zoneName}},
			}
			Expect(r.deletingZoneOf(ctx, record)).To(ConsistOf(reconcile.Request{NamespacedName: zoneKey}))

			other := record.DeepCopy()
			other.Namespace = "kube-public"
			Expect(r.deletingZoneOf(ctx, other)).To(BeEmpty(), "the Zone is looked up in the dependent's namespace")
			Expect(r.deletingZoneOf(ctx, &corev1.Secret{})).To(BeEmpty())
		})
	})

	Describe("Tunnel", func() {
		const tunnelName = "prot-tunnel"
		tunnelKey := types.NamespacedName{Name: tunnelName, Namespace: ns}
		var fake *fakeTunnelAPI
		var r *TunnelReconciler

		BeforeEach(func() {
			fake = &fakeTunnelAPI{}
			r = &TunnelReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				NewTunnelAPI: func(string) (TunnelAPI, error) { return fake, nil },
			}
			create(&cloudflarev1alpha1.Tunnel{
				ObjectMeta: metav1.ObjectMeta{Name: tunnelName, Namespace: ns},
				Spec: cloudflarev1alpha1.TunnelSpec{
					Name:                 "prot-cf-tunnel",
					AccountRef:           corev1.LocalObjectReference{Name: "prot-account"},
					CredentialsSecretRef: cloudflarev1alpha1.TunnelCredentialsSecretReference{Name: "prot-tunnel-token"},
				},
			})
			create(&cloudflarev1alpha1.TunnelConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: "prot-config", Namespace: ns},
				Spec: cloudflarev1alpha1.TunnelConfigurationSpec{
					TunnelRef: corev1.LocalObjectReference{Name: tunnelName},
					Ingress:   []cloudflarev1alpha1.TunnelIngressRule{{Hostname: "a.prot-example.com", Service: "http://a:80"}},
				},
			})
			Expect(k8sClient.Delete(ctx, &cloudflarev1alpha1.Tunnel{ObjectMeta: metav1.ObjectMeta{Name: tunnelName, Namespace: ns}})).To(Succeed())
		})

		It("waits for its TunnelConfigurations, then deletes", func() {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: tunnelKey})
			Expect(err).NotTo(HaveOccurred())
			tunnel := &cloudflarev1alpha1.Tunnel{}
			cond := readyReason(tunnelKey, tunnel, func() []metav1.Condition { return tunnel.Status.Conditions })
			Expect(cond.Reason).To(Equal("InUse"))
			Expect(cond.Message).To(ContainSubstring("TunnelConfiguration default/prot-config"))
			Expect(fake.calls).To(BeEmpty())

			forceDelete(ctx, &cloudflarev1alpha1.TunnelConfiguration{}, types.NamespacedName{Name: "prot-config", Namespace: ns})
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: tunnelKey})
			Expect(err).NotTo(HaveOccurred())
			expectGone(tunnelKey, &cloudflarev1alpha1.Tunnel{})
		})

		It("is re-triggered by a configuration's deletion while it is being deleted", func() {
			tc := &cloudflarev1alpha1.TunnelConfiguration{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns},
				Spec:       cloudflarev1alpha1.TunnelConfigurationSpec{TunnelRef: corev1.LocalObjectReference{Name: tunnelName}},
			}
			Expect(r.deletingTunnelOf(ctx, tc)).To(ConsistOf(reconcile.Request{NamespacedName: tunnelKey}))
		})
	})

	It("does not re-trigger a parent that is not being deleted", func() {
		create(&cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: "prot-live-zone", Namespace: ns},
			Spec: cloudflarev1alpha1.ZoneSpec{
				Name: "prot-live.com", AccountRef: corev1.LocalObjectReference{Name: "prot-account"},
			},
		})
		route := &cloudflarev1alpha1.WorkerRoute{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns},
			Spec:       cloudflarev1alpha1.WorkerRouteSpec{ZoneRef: corev1.LocalObjectReference{Name: "prot-live-zone"}},
		}
		r := &ZoneReconciler{Client: k8sClient}
		Expect(r.deletingZoneOf(ctx, route)).To(BeEmpty())
	})

	It("returns the list error from dependentsOf", func() {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := dependentsOf(cancelled, k8sClient, ns, "anything", zoneDependents)
		Expect(err).To(HaveOccurred())
	})
})
