/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

var _ = Describe("defaultTunnelConfigurationAPI", func() {
	It("returns an error for an empty token", func() {
		_, err := defaultTunnelConfigurationAPI("")
		Expect(err).To(HaveOccurred())
	})

	It("returns a client satisfying TunnelConfigurationAPI for a non-empty token", func() {
		api, err := defaultTunnelConfigurationAPI("any-token-value")
		Expect(err).NotTo(HaveOccurred())
		Expect(api).NotTo(BeNil())
	})

	It("is used when NewTunnelConfigurationAPI is nil", func() {
		r := &TunnelConfigurationReconciler{}
		_, err := r.newTunnelConfigurationAPI("")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("olderThan", func() {
	at := func(name string, t time.Time) *cloudflarev1alpha1.TunnelConfiguration {
		return &cloudflarev1alpha1.TunnelConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(t)},
		}
	}
	now := time.Now()

	It("orders by creation time first", func() {
		Expect(olderThan(at("z", now.Add(-time.Minute)), at("a", now))).To(BeTrue())
		Expect(olderThan(at("a", now), at("z", now.Add(-time.Minute)))).To(BeFalse())
	})

	It("breaks ties by name", func() {
		Expect(olderThan(at("a", now), at("b", now))).To(BeTrue())
		Expect(olderThan(at("b", now), at("a", now))).To(BeFalse())
	})
})

var _ = Describe("desiredTunnelConfiguration", func() {
	It("falls back to http_status:404 when defaultService is empty", func() {
		tc := &cloudflarev1alpha1.TunnelConfiguration{
			Spec: cloudflarev1alpha1.TunnelConfigurationSpec{
				Ingress: []cloudflarev1alpha1.TunnelIngressRule{{Hostname: "a.example.com", Service: "http://a:80"}},
			},
		}
		rules := desiredTunnelConfiguration(tc).Ingress
		Expect(rules).To(HaveLen(2))
		Expect(rules[1]).To(Equal(cf.UnvalidatedIngressRule{Service: "http_status:404"}))
	})
})

var _ = Describe("isTunnelReady", func() {
	tunnel := func(id string, status metav1.ConditionStatus) *cloudflarev1alpha1.Tunnel {
		t := &cloudflarev1alpha1.Tunnel{}
		t.Status.CloudflareMetadata.TunnelID = id
		if status != "" {
			t.Status.Conditions = []metav1.Condition{{Type: cloudflarev1alpha1.ConditionReady, Status: status}}
		}
		return t
	}

	It("requires both a tunnel ID and Ready=True", func() {
		Expect(isTunnelReady(tunnel("id", metav1.ConditionTrue))).To(BeTrue())
		Expect(isTunnelReady(tunnel("", metav1.ConditionTrue))).To(BeFalse())
		Expect(isTunnelReady(tunnel("id", metav1.ConditionFalse))).To(BeFalse())
		Expect(isTunnelReady(tunnel("id", ""))).To(BeFalse())
	})
})

var _ = Describe("TunnelConfigurationReconciler reconcileDelete (no finalizer)", func() {
	It("returns no error when the configuration has no finalizer", func() {
		tc := &cloudflarev1alpha1.TunnelConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "no-finalizer-config", Namespace: "default"},
		}
		r := &TunnelConfigurationReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		result, err := r.reconcileDelete(context.Background(), tc)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
	})
})

var _ = Describe("TunnelConfigurationReconciler SetupWithManager", func() {
	It("registers without error", func() {
		mgr, err := manager.New(cfg, manager.Options{})
		Expect(err).NotTo(HaveOccurred())

		r := &TunnelConfigurationReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
	})
})

var _ = Describe("TunnelConfigurationReconciler configurationsForTunnel", func() {
	r := func() *TunnelConfigurationReconciler {
		return &TunnelConfigurationReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}

	It("returns nil when the list fails", func() {
		cancelCtx, cancel := context.WithCancel(context.Background())
		cancel()
		Expect(r().configurationsForTunnel(cancelCtx, &cloudflarev1alpha1.Tunnel{})).To(BeNil())
	})

	Context("when configurations exist", func() {
		const (
			cftTunnel = "cft-tunnel"
			cftNS     = "default"
		)
		ctx := context.Background()
		names := []string{"cft-config-match", "cft-config-other"}

		BeforeEach(func() {
			for i, tunnelName := range []string{cftTunnel, "cft-unrelated-tunnel"} {
				Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.TunnelConfiguration{
					ObjectMeta: metav1.ObjectMeta{Name: names[i], Namespace: cftNS},
					Spec: cloudflarev1alpha1.TunnelConfigurationSpec{
						TunnelRef: corev1.LocalObjectReference{Name: tunnelName},
						Ingress: []cloudflarev1alpha1.TunnelIngressRule{
							{Hostname: "cft.example.com", Service: "http://cft:80"},
						},
					},
				})).To(Succeed())
			}
		})

		AfterEach(func() {
			for _, name := range names {
				tc := &cloudflarev1alpha1.TunnelConfiguration{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: cftNS}, tc); err == nil {
					_ = k8sClient.Delete(ctx, tc)
				}
			}
		})

		It("returns a request only for configurations referencing the Tunnel in its namespace", func() {
			tunnel := &cloudflarev1alpha1.Tunnel{ObjectMeta: metav1.ObjectMeta{Name: cftTunnel, Namespace: cftNS}}
			reqs := r().configurationsForTunnel(ctx, tunnel)
			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{Name: "cft-config-match", Namespace: cftNS}))

			elsewhere := &cloudflarev1alpha1.Tunnel{ObjectMeta: metav1.ObjectMeta{Name: cftTunnel, Namespace: "kube-system"}}
			Expect(r().configurationsForTunnel(ctx, elsewhere)).To(BeEmpty())
		})
	})
})
