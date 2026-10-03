/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"encoding/base64"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

var _ = Describe("defaultTunnelAPI", func() {
	It("returns an error for an empty token", func() {
		_, err := defaultTunnelAPI("")
		Expect(err).To(HaveOccurred())
	})

	It("returns a client satisfying TunnelAPI for a non-empty token", func() {
		api, err := defaultTunnelAPI("any-token-value")
		Expect(err).NotTo(HaveOccurred())
		Expect(api).NotTo(BeNil())
	})

	It("is used when NewTunnelAPI is nil", func() {
		r := &TunnelReconciler{}
		_, err := r.newTunnelAPI("")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("generateTunnelSecret", func() {
	It("returns 32 random bytes, base64-encoded", func() {
		a, err := generateTunnelSecret()
		Expect(err).NotTo(HaveOccurred())
		decoded, err := base64.StdEncoding.DecodeString(a)
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded).To(HaveLen(tunnelSecretBytes))

		b, err := generateTunnelSecret()
		Expect(err).NotTo(HaveOccurred())
		Expect(b).NotTo(Equal(a))
	})
})

var _ = Describe("TunnelReconciler reconcileDelete (no finalizer)", func() {
	It("returns no error when the tunnel has no finalizer", func() {
		tunnel := &cloudflarev1alpha1.Tunnel{
			ObjectMeta: metav1.ObjectMeta{Name: "no-finalizer-tunnel", Namespace: "default"},
		}
		r := &TunnelReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		result, err := r.reconcileDelete(context.Background(), tunnel)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
	})
})

var _ = Describe("TunnelReconciler SetupWithManager", func() {
	It("registers without error", func() {
		mgr, err := manager.New(cfg, manager.Options{})
		Expect(err).NotTo(HaveOccurred())

		r := &TunnelReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
	})
})

var _ = Describe("TunnelReconciler tunnelsForAccount", func() {
	r := func() *TunnelReconciler {
		return &TunnelReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}

	It("returns nil when the tunnel list fails", func() {
		// A pre-cancelled context causes r.List to fail.
		cancelCtx, cancel := context.WithCancel(context.Background())
		cancel()
		Expect(r().tunnelsForAccount(cancelCtx, &cloudflarev1alpha1.CloudflareAccount{})).To(BeNil())
	})

	Context("when tunnels exist", func() {
		const (
			tfaAccountName = "tfa-test-account"
			tfaNS          = "default"
		)
		ctx := context.Background()
		names := []string{"tfa-tunnel-match", "tfa-tunnel-other"}

		BeforeEach(func() {
			for i, account := range []string{tfaAccountName, "tfa-unrelated-account"} {
				Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.Tunnel{
					ObjectMeta: metav1.ObjectMeta{Name: names[i], Namespace: tfaNS},
					Spec: cloudflarev1alpha1.TunnelSpec{
						Name:                 names[i],
						AccountRef:           corev1.LocalObjectReference{Name: account},
						CredentialsSecretRef: cloudflarev1alpha1.TunnelCredentialsSecretReference{Name: names[i] + "-token"},
					},
				})).To(Succeed())
			}
		})

		AfterEach(func() {
			for _, name := range names {
				tunnel := &cloudflarev1alpha1.Tunnel{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: tfaNS}, tunnel); err == nil {
					_ = k8sClient.Delete(ctx, tunnel)
				}
			}
		})

		It("returns a request only for tunnels referencing the account", func() {
			account := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Name: tfaAccountName}}
			reqs := r().tunnelsForAccount(ctx, account)
			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{Name: "tfa-tunnel-match", Namespace: tfaNS}))
		})
	})
})
