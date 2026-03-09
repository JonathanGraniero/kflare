/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	cfgo "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

var _ = Describe("defaultZoneAPI", func() {
	It("returns an error for an empty token", func() {
		_, err := defaultZoneAPI("")
		Expect(err).To(HaveOccurred())
	})

	It("returns a non-nil client for a non-empty token", func() {
		api, err := defaultZoneAPI("any-token-value")
		Expect(err).NotTo(HaveOccurred())
		Expect(api).NotTo(BeNil())
	})
})

var _ = Describe("ZoneReconciler syncZone (empty Type defaults)", func() {
	const (
		stZoneName    = "st-test-zone"
		stAccountName = "st-test-account"
		stSecretName  = "st-test-secret"
		stSecretNS    = "default"
		stZoneNS      = "default"
	)
	ctx := context.Background()
	stZoneKey := types.NamespacedName{Name: stZoneName, Namespace: stZoneNS}
	stAccountKey := types.NamespacedName{Name: stAccountName}
	stSecretKey := types.NamespacedName{Name: stSecretName, Namespace: stSecretNS}

	BeforeEach(func() {
		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: stAccountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID: "st-acct-id",
				TokenSecretRef: cloudflarev1alpha1.SecretReference{
					Name: stSecretName, Namespace: stSecretNS,
				},
			},
		}
		Expect(k8sClient.Create(ctx, acct)).To(Succeed())

		zone := &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: stZoneName, Namespace: stZoneNS},
			Spec: cloudflarev1alpha1.ZoneSpec{
				Name:       "st-example.com",
				AccountRef: corev1.LocalObjectReference{Name: stAccountName},
				Type:       "full",
			},
		}
		Expect(k8sClient.Create(ctx, zone)).To(Succeed())
	})

	AfterEach(func() {
		zone := &cloudflarev1alpha1.Zone{}
		if err := k8sClient.Get(ctx, stZoneKey, zone); err == nil {
			zone.Finalizers = nil
			_ = k8sClient.Update(ctx, zone)
			_ = k8sClient.Delete(ctx, zone)
		}
		acct := &cloudflarev1alpha1.CloudflareAccount{}
		if err := k8sClient.Get(ctx, stAccountKey, acct); err == nil {
			_ = k8sClient.Delete(ctx, acct)
		}
		secret := &corev1.Secret{}
		if err := k8sClient.Get(ctx, stSecretKey, secret); err == nil {
			_ = k8sClient.Delete(ctx, secret)
		}
	})

	It("defaults zoneType to 'full' when Spec.Type is empty (in-memory override)", func() {
		zone := &cloudflarev1alpha1.Zone{}
		Expect(k8sClient.Get(ctx, stZoneKey, zone)).To(Succeed())

		account := &cloudflarev1alpha1.CloudflareAccount{}
		Expect(k8sClient.Get(ctx, stAccountKey, account)).To(Succeed())

		// Override Type in-memory to "" to exercise the defensive default branch.
		zone.Spec.Type = ""

		createdZone := cfgo.Zone{ID: "st-zone-id", Name: "st-example.com", Type: "full", Status: "active"}
		fake := &fakeZoneAPI{listZones: []cfgo.Zone{}, createZone: createdZone}

		r := &ZoneReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		// logr.Discard() satisfies the logger interface.
		nopLogger := nopLog{}
		_, err := r.syncZone(ctx, nopLogger, zone, account, fake)
		Expect(err).NotTo(HaveOccurred())
	})
})

// nopLog is a no-op logger satisfying the narrow interface used by syncZone.
type nopLog struct{}

func (nopLog) Info(_ string, _ ...interface{}) {}

var _ = Describe("ZoneReconciler reconcileDelete (no finalizer)", func() {
	It("returns no error when the zone has no finalizer (safety path)", func() {
		// Call reconcileDelete directly with a zone that has no finalizer.
		// This covers the early-return safety check without needing a full lifecycle.
		zone := &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: "no-finalizer-zone", Namespace: "default"},
			Spec: cloudflarev1alpha1.ZoneSpec{
				Name:       "no-finalizer.com",
				AccountRef: corev1.LocalObjectReference{Name: "some-account"},
				Type:       "full",
			},
		}
		r := &ZoneReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		result, err := r.reconcileDelete(context.Background(), zone)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
	})
})

var _ = Describe("ZoneReconciler SetupWithManager", func() {
	It("registers without error", func() {
		mgr, err := manager.New(cfg, manager.Options{})
		Expect(err).NotTo(HaveOccurred())

		r := &ZoneReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
	})
})

var _ = Describe("ZoneReconciler zonesForAccount", func() {
	It("satisfies the ZoneAPI interface from *cfpkg.Client via defaultZoneAPI", func() {
		api, err := defaultZoneAPI("token")
		Expect(err).NotTo(HaveOccurred())

		var _ ZoneAPI = api
		Expect(api).NotTo(BeNil())
	})

	It("returns empty when no zones reference the account", func() {
		r := &ZoneReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		reqs := r.zonesForAccount(context.Background(), &cloudflarev1alpha1.CloudflareAccount{})
		Expect(reqs).To(Or(BeNil(), BeEmpty()))
	})

	It("returns nil when the zone list fails", func() {
		r := &ZoneReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		// A pre-cancelled context causes r.List to fail.
		cancelCtx, cancel := context.WithCancel(context.Background())
		cancel()
		reqs := r.zonesForAccount(cancelCtx, &cloudflarev1alpha1.CloudflareAccount{})
		Expect(reqs).To(BeNil())
	})

	Context("when zones reference the account", func() {
		const (
			zfaZoneName    = "zfa-test-zone"
			zfaAccountName = "zfa-test-account"
			zfaZoneNS      = "default"
		)
		ctx := context.Background()

		BeforeEach(func() {
			zone := &cloudflarev1alpha1.Zone{
				ObjectMeta: metav1.ObjectMeta{Name: zfaZoneName, Namespace: zfaZoneNS},
				Spec: cloudflarev1alpha1.ZoneSpec{
					Name:       "zfa-example.com",
					AccountRef: corev1.LocalObjectReference{Name: zfaAccountName},
					Type:       "full",
				},
			}
			Expect(k8sClient.Create(ctx, zone)).To(Succeed())
		})

		AfterEach(func() {
			zone := &cloudflarev1alpha1.Zone{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: zfaZoneName, Namespace: zfaZoneNS}, zone); err == nil {
				zone.Finalizers = nil
				_ = k8sClient.Update(ctx, zone)
				_ = k8sClient.Delete(ctx, zone)
			}
		})

		It("returns a reconcile.Request for each zone referencing the account", func() {
			r := &ZoneReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			account := &cloudflarev1alpha1.CloudflareAccount{
				ObjectMeta: metav1.ObjectMeta{Name: zfaAccountName},
			}
			reqs := r.zonesForAccount(ctx, account)
			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].NamespacedName.Name).To(Equal(zfaZoneName))
			Expect(reqs[0].NamespacedName.Namespace).To(Equal(zfaZoneNS))
		})
	})
})
