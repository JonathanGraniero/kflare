/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"github.com/cloudflare/cloudflare-go/v7/shared"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// These specs cover parents that wait for their dependents before they are
// deleted: a Zone for its DNSRecords, WorkerRoutes and rules, a Tunnel for its
// TunnelConfigurations and the DNSRecords that point at it. The
// CloudflareAccount case is in cloudflareaccount_protection_test.go.
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
			create(&cloudflarev1alpha1.WAFCustomRule{
				ObjectMeta: metav1.ObjectMeta{Name: "prot-waf", Namespace: ns},
				Spec: cloudflarev1alpha1.WAFCustomRuleSpec{
					RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
						ZoneRef: corev1.LocalObjectReference{Name: zoneName}, Expression: "true",
					},
					Action: "block",
				},
			})
			create(&cloudflarev1alpha1.RateLimitRule{
				ObjectMeta: metav1.ObjectMeta{Name: "prot-ratelimit", Namespace: ns},
				Spec: cloudflarev1alpha1.RateLimitRuleSpec{
					RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
						ZoneRef: corev1.LocalObjectReference{Name: zoneName}, Expression: "true",
					},
					Action: "block",
					Ratelimit: &cloudflarev1alpha1.RateLimitRuleRatelimit{
						Characteristics: []string{"cf.colo.id", "ip.src"}, Period: 10,
						RequestsPerPeriod: new(int64(5)), MitigationTimeout: new(int64(10)),
					},
				},
			})
			create(&cloudflarev1alpha1.ManagedRuleset{
				ObjectMeta: metav1.ObjectMeta{Name: "prot-managed", Namespace: ns},
				Spec: cloudflarev1alpha1.ManagedRulesetSpec{
					RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
						ZoneRef: corev1.LocalObjectReference{Name: zoneName}, Expression: "true",
					},
					ID: freeManagedRulesetID,
				},
			})
			Expect(k8sClient.Delete(ctx, &cloudflarev1alpha1.Zone{ObjectMeta: metav1.ObjectMeta{Name: zoneName, Namespace: ns}})).To(Succeed())
		})

		It("waits for its DNSRecords, WorkerRoutes and rules, then deletes", func() {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())
			zone := &cloudflarev1alpha1.Zone{}
			cond := readyReason(zoneKey, zone, func() []metav1.Condition { return zone.Status.Conditions })
			Expect(cond.Reason).To(Equal("InUse"))
			Expect(cond.Message).To(ContainSubstring("DNSRecord default/prot-record"))
			Expect(cond.Message).To(ContainSubstring("WorkerRoute default/prot-route"))
			Expect(cond.Message).To(ContainSubstring("WAFCustomRule default/prot-waf"))
			Expect(cond.Message).To(ContainSubstring("RateLimitRule default/prot-ratelimit"))
			Expect(cond.Message).To(ContainSubstring("ManagedRuleset default/prot-managed"))
			Expect(fake.deleteCalled).To(BeFalse())

			forceDelete(ctx, &cloudflarev1alpha1.DNSRecord{}, types.NamespacedName{Name: "prot-record", Namespace: ns})
			forceDelete(ctx, &cloudflarev1alpha1.WorkerRoute{}, types.NamespacedName{Name: "prot-route", Namespace: ns})
			forceDelete(ctx, &cloudflarev1alpha1.WAFCustomRule{}, types.NamespacedName{Name: "prot-waf", Namespace: ns})
			forceDelete(ctx, &cloudflarev1alpha1.RateLimitRule{}, types.NamespacedName{Name: "prot-ratelimit", Namespace: ns})
			forceDelete(ctx, &cloudflarev1alpha1.ManagedRuleset{}, types.NamespacedName{Name: "prot-managed", Namespace: ns})
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
		var store *fakeCRUDStore[shared.CloudflareTunnel]
		var hooks fakeTunnelHooksAPI
		var r *TunnelReconciler

		BeforeEach(func() {
			store = newFakeCRUDStore[shared.CloudflareTunnel](1000, "")
			hooks = newFakeTunnelHooksAPI(store)
			r = &TunnelReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				NewTunnelAPI: func(string) (TunnelAPI, error) { return fakeTunnelAPI{store, hooks}, nil },
			}
			create(&cloudflarev1alpha1.Tunnel{
				ObjectMeta: metav1.ObjectMeta{Name: tunnelName, Namespace: ns},
				Spec: cloudflarev1alpha1.TunnelSpec{
					Name:             "prot-cf-tunnel",
					AccountRef:       corev1.LocalObjectReference{Name: "prot-account"},
					TunnelSpecExtras: cloudflarev1alpha1.TunnelSpecExtras{CredentialsSecretRef: cloudflarev1alpha1.TunnelCredentialsSecretReference{Name: "prot-tunnel-token"}},
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
			Expect(store.parentIDs).To(BeEmpty())
			Expect(hooks.cleanups).To(BeEmpty())

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
			Expect(r.crud().deletingResourcesOf(ctx, tc)).To(ConsistOf(reconcile.Request{NamespacedName: tunnelKey}))
		})

		It("is re-triggered by the deletion of a DNSRecord that points at it while it is being deleted", func() {
			pointing := &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					DNSRecordSpecExtras: cloudflarev1alpha1.DNSRecordSpecExtras{TunnelRef: &corev1.LocalObjectReference{Name: tunnelName}},
				},
			}
			Expect(r.crud().deletingResourcesOf(ctx, pointing)).To(ConsistOf(reconcile.Request{NamespacedName: tunnelKey}))
			Expect(r.crud().deletingResourcesOf(ctx, &cloudflarev1alpha1.DNSRecord{ObjectMeta: metav1.ObjectMeta{Namespace: ns}})).
				To(BeEmpty(), "a record without a tunnelRef")
		})
	})

	Describe("Tunnel pointed at by DNSRecords", func() {
		const tunnelName = "prot-dns-tunnel"
		tunnelKey := types.NamespacedName{Name: tunnelName, Namespace: ns}
		recordKey := types.NamespacedName{Name: "prot-tunnel-record", Namespace: ns}

		It("waits for the DNSRecords that point at it, and is released once they are gone", func() {
			create(&cloudflarev1alpha1.Tunnel{
				ObjectMeta: metav1.ObjectMeta{Name: tunnelName, Namespace: ns},
				Spec: cloudflarev1alpha1.TunnelSpec{
					Name:             "prot-dns-cf-tunnel",
					AccountRef:       corev1.LocalObjectReference{Name: "prot-account"},
					TunnelSpecExtras: cloudflarev1alpha1.TunnelSpecExtras{CredentialsSecretRef: cloudflarev1alpha1.TunnelCredentialsSecretReference{Name: "prot-dns-tunnel-token"}},
				},
			})
			create(&cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: recordKey.Name, Namespace: ns},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					ZoneRef: corev1.LocalObjectReference{Name: "prot-zone"}, Name: "app.prot-example.com",
					Type:                "CNAME",
					DNSRecordSpecExtras: cloudflarev1alpha1.DNSRecordSpecExtras{TunnelRef: &corev1.LocalObjectReference{Name: tunnelName}},
				},
			})
			create(&cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: "prot-unrelated-record", Namespace: ns},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					ZoneRef: corev1.LocalObjectReference{Name: "prot-zone"}, Name: "prot-example.com",
					Type: "A", Content: "192.0.2.1",
				},
			})
			Expect(dependentsOf(ctx, k8sClient, ns, tunnelName, tunnelDependents)).
				To(ConsistOf("DNSRecord " + ns + "/" + recordKey.Name))
			Expect(k8sClient.Delete(ctx, &cloudflarev1alpha1.Tunnel{
				ObjectMeta: metav1.ObjectMeta{Name: tunnelName, Namespace: ns}})).To(Succeed())

			By("running the Tunnel controller, which must hear of the DNSRecord's deletion")
			mgr, err := manager.New(cfg, manager.Options{
				Controller: config.Controller{SkipNameValidation: new(true)},
				Metrics:    metricsserver.Options{BindAddress: "0"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect((&TunnelReconciler{
				Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
				NewTunnelAPI: func(string) (TunnelAPI, error) { return nil, errors.New("this spec calls no Cloudflare API") },
			}).SetupWithManager(mgr)).To(Succeed())
			mgrCtx, stop := context.WithCancel(ctx)
			stopped := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				defer close(stopped)
				Expect(mgr.Start(mgrCtx)).To(Succeed())
			}()
			DeferCleanup(func() {
				stop()
				Eventually(stopped).Should(BeClosed())
			})

			Eventually(func(g Gomega) {
				tunnel := &cloudflarev1alpha1.Tunnel{}
				g.Expect(k8sClient.Get(ctx, tunnelKey, tunnel)).To(Succeed())
				ready := meta.FindStatusCondition(tunnel.Status.Conditions, cloudflarev1alpha1.ConditionReady)
				g.Expect(ready).NotTo(BeNil())
				g.Expect(ready.Reason).To(Equal("InUse"))
				g.Expect(ready.Message).To(ContainSubstring("DNSRecord " + ns + "/" + recordKey.Name))
			}).WithTimeout(10 * time.Second).Should(Succeed())

			By("deleting the DNSRecord, which releases the Tunnel")
			forceDelete(ctx, &cloudflarev1alpha1.DNSRecord{}, recordKey)
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, tunnelKey, &cloudflarev1alpha1.Tunnel{}))
			}).WithTimeout(10*time.Second).Should(BeTrue(), "the Tunnel was not re-triggered by the DNSRecord's deletion")
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
