/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// fakeZoneAPI is a test double for ZoneAPI.
type fakeZoneAPI struct {
	listZones    []cf.Zone
	listErr      error
	createZone   cf.Zone
	createErr    error
	getZone      cf.Zone
	getErr       error
	deleteErr    error
	editZone     cf.Zone
	editErr      error
	editCalled   bool
	deleteCalled bool
}

func (f *fakeZoneAPI) CreateZone(_ context.Context, _ string, _ bool, _ cf.Account, _ string) (cf.Zone, error) {
	return f.createZone, f.createErr
}
func (f *fakeZoneAPI) ZoneDetails(_ context.Context, _ string) (cf.Zone, error) {
	return f.getZone, f.getErr
}
func (f *fakeZoneAPI) ListZones(_ context.Context, _ ...string) ([]cf.Zone, error) {
	return f.listZones, f.listErr
}
func (f *fakeZoneAPI) DeleteZone(_ context.Context, _ string) (cf.ZoneID, error) {
	f.deleteCalled = true
	return cf.ZoneID{}, f.deleteErr
}
func (f *fakeZoneAPI) EditZone(_ context.Context, _ string, _ cf.ZoneOptions) (cf.Zone, error) {
	f.editCalled = true
	return f.editZone, f.editErr
}

// reconcilerWithFakeZoneAPI returns a ZoneReconciler wired to a pre-built fake.
func reconcilerWithFakeZoneAPI(fake ZoneAPI) *ZoneReconciler {
	return &ZoneReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		NewZoneAPI: func(_ string) (ZoneAPI, error) {
			return fake, nil
		},
	}
}

// reconcilerWithZoneClientErr returns a ZoneReconciler whose client constructor always errors.
func reconcilerWithZoneClientErr(err error) *ZoneReconciler {
	return &ZoneReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		NewZoneAPI: func(_ string) (ZoneAPI, error) {
			return nil, err
		},
	}
}

var _ = Describe("Zone Controller", func() {
	const (
		zoneCRName  = "test-zone"
		domainName  = "example.com"
		accountName = "zone-test-account"
		secretName  = "zone-test-secret"
		secretNS    = "default"
		tokenKey    = "CF_API_TOKEN"
		fakeToken   = "fake-zone-token"
		fakeAcctID  = "acct-123"
		fakeZoneID  = "zone-456"
	)

	ctx := context.Background()
	zoneKey := types.NamespacedName{Name: zoneCRName, Namespace: "default"}
	accountKey := types.NamespacedName{Name: accountName}
	secretKey := types.NamespacedName{Name: secretName, Namespace: secretNS}

	// createAccount creates a CloudflareAccount CR and optionally marks it Ready=True.
	createAccount := func(ready bool) {
		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID: fakeAcctID,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{
					Name:      secretName,
					Namespace: secretNS,
					Key:       tokenKey,
				},
			},
		}
		Expect(k8sClient.Create(ctx, acct)).To(Succeed())

		if ready {
			// Fetch fresh copy to get resource version, then set status.
			Expect(k8sClient.Get(ctx, accountKey, acct)).To(Succeed())
			acct.Status.Conditions = []metav1.Condition{
				{
					Type:               cloudflarev1alpha1.ConditionReady,
					Status:             metav1.ConditionTrue,
					Reason:             "Validated",
					Message:            "Credentials valid",
					ObservedGeneration: acct.Generation,
					LastTransitionTime: metav1.Now(),
				},
			}
			Expect(k8sClient.Status().Update(ctx, acct)).To(Succeed())
		}
	}

	// createSecret creates the credentials secret.
	createSecret := func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretNS},
			Data:       map[string][]byte{tokenKey: []byte(fakeToken)},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	}

	// createZone creates a Zone CR.  annotations may be nil.
	createZone := func(annotations map[string]string) *cloudflarev1alpha1.Zone {
		zone := &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{
				Name:        zoneCRName,
				Namespace:   "default",
				Annotations: annotations,
			},
			Spec: cloudflarev1alpha1.ZoneSpec{
				Name:       domainName,
				AccountRef: corev1.LocalObjectReference{Name: accountName},
				Type:       "full",
			},
		}
		Expect(k8sClient.Create(ctx, zone)).To(Succeed())
		return zone
	}

	// setZoneIDInStatus directly writes the zone ID into status (simulates a prior successful reconcile).
	setZoneIDInStatus := func(zoneID string) {
		zone := &cloudflarev1alpha1.Zone{}
		Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
		zone.Status.CloudflareMetadata.ZoneID = zoneID
		Expect(k8sClient.Status().Update(ctx, zone)).To(Succeed())
	}

	// getZoneCondition fetches the zone and returns the named condition (or nil).
	getZoneCondition := func(condType string) *metav1.Condition {
		zone := &cloudflarev1alpha1.Zone{}
		Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
		for i := range zone.Status.Conditions {
			if zone.Status.Conditions[i].Type == condType {
				return &zone.Status.Conditions[i]
			}
		}
		return nil
	}

	AfterEach(func() {
		zone := &cloudflarev1alpha1.Zone{}
		if err := k8sClient.Get(ctx, zoneKey, zone); err == nil {
			// Remove finalizer so deletion proceeds without a real CF client.
			zone.Finalizers = nil
			_ = k8sClient.Update(ctx, zone)
			_ = k8sClient.Delete(ctx, zone)
		}
		acct := &cloudflarev1alpha1.CloudflareAccount{}
		if err := k8sClient.Get(ctx, accountKey, acct); err == nil {
			Expect(k8sClient.Delete(ctx, acct)).To(Succeed())
		}
		secret := &corev1.Secret{}
		if err := k8sClient.Get(ctx, secretKey, secret); err == nil {
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		}
	})

	Describe("Reconcile", func() {
		It("returns no error when the Zone CR does not exist", func() {
			r := reconcilerWithFakeZoneAPI(&fakeZoneAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())
		})

		It("sets Ready=False AccountNotFound when the CloudflareAccount is missing", func() {
			createZone(nil)
			// Add finalizer so we get past the EnsureFinalizer early return.
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())

			r := reconcilerWithFakeZoneAPI(&fakeZoneAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getZoneCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("AccountNotFound"))
		})

		It("sets Ready=False AccountNotReady when the CloudflareAccount is not ready", func() {
			createAccount(false) // ready=false
			createZone(nil)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())

			r := reconcilerWithFakeZoneAPI(&fakeZoneAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getZoneCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("AccountNotReady"))
		})

		It("sets Ready=False SecretNotFound when the credentials secret is missing", func() {
			createAccount(true)
			createZone(nil)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())

			r := reconcilerWithFakeZoneAPI(&fakeZoneAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getZoneCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("SecretNotFound"))
		})

		It("sets Ready=False TokenKeyMissing when the token key is absent from the secret", func() {
			createAccount(true)
			// Create secret with a wrong key.
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretNS},
				Data:       map[string][]byte{"WRONG_KEY": []byte(fakeToken)},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			createZone(nil)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())

			r := reconcilerWithFakeZoneAPI(&fakeZoneAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getZoneCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("TokenKeyMissing"))
		})

		It("sets Ready=False InvalidToken when the CF client cannot be constructed", func() {
			createAccount(true)
			createSecret()
			createZone(nil)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())

			r := reconcilerWithZoneClientErr(errors.New("bad token"))
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getZoneCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("InvalidToken"))
		})

		It("sets Ready=False TerminalError and returns nil for terminal CF API errors", func() {
			createAccount(true)
			createSecret()
			createZone(nil)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())

			terminalErrVal := cf.NewAuthorizationError(&cf.Error{StatusCode: 403, Type: cf.ErrorTypeAuthorization})
			terminalErr := &terminalErrVal
			r := reconcilerWithFakeZoneAPI(&fakeZoneAPI{listErr: terminalErr})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred()) // nil return — stop requeuing

			cond := getZoneCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("TerminalError"))
		})

		It("sets Ready=False APIError and returns an error for retryable CF API errors", func() {
			createAccount(true)
			createSecret()
			createZone(nil)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())

			retryableErr := errors.New("connection reset by peer")
			r := reconcilerWithFakeZoneAPI(&fakeZoneAPI{listErr: retryableErr})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).To(HaveOccurred()) // error returned — triggers backoff

			cond := getZoneCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("APIError"))
		})

		It("creates a new zone when none exists and sets Ready=True with zoneID", func() {
			createAccount(true)
			createSecret()
			createZone(nil)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())

			fake := &fakeZoneAPI{
				listZones:  []cf.Zone{},
				createZone: cf.Zone{ID: fakeZoneID, Name: domainName, Type: "full", Status: "active"},
			}
			r := reconcilerWithFakeZoneAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getZoneCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("Synced"))

			updated := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, updated)).To(Succeed())
			Expect(updated.Status.CloudflareMetadata.ZoneID).To(Equal(fakeZoneID))
		})

		It("reconciles without drift when zone already exists in status", func() {
			createAccount(true)
			createSecret()
			createZone(nil)
			setZoneIDInStatus(fakeZoneID)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())

			cfZone := cf.Zone{ID: fakeZoneID, Name: domainName, Type: "full", Status: "active",
				NameServers: []string{"ns1.cloudflare.com", "ns2.cloudflare.com"}}
			fake := &fakeZoneAPI{getZone: cfZone}
			r := reconcilerWithFakeZoneAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.editCalled).To(BeFalse())

			cond := getZoneCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))

			updated := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, updated)).To(Succeed())
			Expect(updated.Status.CloudflareMetadata.NameServers).To(ConsistOf("ns1.cloudflare.com", "ns2.cloudflare.com"))
		})

		It("recreates a zone that was deleted externally (Get returns NotFound)", func() {
			createAccount(true)
			createSecret()
			createZone(nil)
			setZoneIDInStatus(fakeZoneID)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())

			notFoundErrVal := cf.NewNotFoundError(&cf.Error{StatusCode: 404, Type: cf.ErrorTypeNotFound})
			notFoundErr := &notFoundErrVal
			recreated := cf.Zone{ID: "zone-new-789", Name: domainName, Type: "full", Status: "pending"}
			fake := &fakeZoneAPI{
				getErr:     notFoundErr,
				listZones:  []cf.Zone{},
				createZone: recreated,
			}
			r := reconcilerWithFakeZoneAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getZoneCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))

			updated := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, updated)).To(Succeed())
			Expect(updated.Status.CloudflareMetadata.ZoneID).To(Equal("zone-new-789"))
		})

		It("adopts a pre-existing zone found via List", func() {
			createAccount(true)
			createSecret()
			createZone(nil)
			// No zoneID in status — zone hasn't been reconciled yet.
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())

			existing := cf.Zone{ID: "zone-existing-111", Name: domainName, Type: "full", Status: "active"}
			fake := &fakeZoneAPI{listZones: []cf.Zone{existing}}
			r := reconcilerWithFakeZoneAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())

			updated := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, updated)).To(Succeed())
			Expect(updated.Status.CloudflareMetadata.ZoneID).To(Equal("zone-existing-111"))

			cond := getZoneCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("calls EditZone when the zone type has drifted", func() {
			createAccount(true)
			createSecret()
			// Zone spec requests "full" but CF has "partial".
			createZone(nil)
			setZoneIDInStatus(fakeZoneID)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())

			driftedZone := cf.Zone{ID: fakeZoneID, Name: domainName, Type: "partial", Status: "active"}
			correctedZone := cf.Zone{ID: fakeZoneID, Name: domainName, Type: "full", Status: "active"}
			fake := &fakeZoneAPI{
				getZone:  driftedZone,
				editZone: correctedZone,
			}
			r := reconcilerWithFakeZoneAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.editCalled).To(BeTrue())

			cond := getZoneCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("removes the finalizer without calling CF delete when deletion-policy=retain", func() {
			createAccount(true)
			createSecret()
			createZone(map[string]string{"cloudflare.k8s.io/deletion-policy": "retain"})
			setZoneIDInStatus(fakeZoneID)

			// Add finalizer, then trigger deletion.
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())
			Expect(k8sClient.Delete(ctx, zone)).To(Succeed())

			// Fetch with DeletionTimestamp set.
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			Expect(zone.DeletionTimestamp).NotTo(BeNil())

			fake := &fakeZoneAPI{}
			r := reconcilerWithFakeZoneAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.deleteCalled).To(BeFalse()) // CF delete must NOT be called

			// Finalizer should be gone now.
			updated := &cloudflarev1alpha1.Zone{}
			err = k8sClient.Get(ctx, zoneKey, updated)
			// Either the zone was fully deleted or the finalizer is gone.
			if err == nil {
				Expect(updated.Finalizers).NotTo(ContainElement(reconciler.Finalizer))
			}
		})

		It("calls CF delete and removes the finalizer when deletion-policy=delete", func() {
			createAccount(true)
			createSecret()
			createZone(map[string]string{"cloudflare.k8s.io/deletion-policy": "delete"})
			setZoneIDInStatus(fakeZoneID)

			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())
			Expect(k8sClient.Delete(ctx, zone)).To(Succeed())

			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			Expect(zone.DeletionTimestamp).NotTo(BeNil())

			fake := &fakeZoneAPI{}
			r := reconcilerWithFakeZoneAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.deleteCalled).To(BeTrue()) // CF delete MUST be called

			// Zone should be fully gone or have no finalizer.
			updated := &cloudflarev1alpha1.Zone{}
			err = k8sClient.Get(ctx, zoneKey, updated)
			if err == nil {
				Expect(updated.Finalizers).NotTo(ContainElement(reconciler.Finalizer))
			}
		})

		It("adds the finalizer on first reconcile and returns without calling CF", func() {
			createAccount(true)
			createSecret()
			createZone(nil)

			fake := &fakeZoneAPI{}
			r := reconcilerWithFakeZoneAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())

			// Finalizer must be set.
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			Expect(zone.Finalizers).To(ContainElement(reconciler.Finalizer))

			// No CF API calls should have been made yet.
			Expect(fake.editCalled).To(BeFalse())
			Expect(fake.deleteCalled).To(BeFalse())
		})

		It("sets Ready=False TerminalError when ZoneDetails returns a non-NotFound terminal error", func() {
			createAccount(true)
			createSecret()
			createZone(nil)
			setZoneIDInStatus(fakeZoneID)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())

			authErrVal := cf.NewAuthorizationError(&cf.Error{StatusCode: 403, Type: cf.ErrorTypeAuthorization})
			authErr := &authErrVal
			fake := &fakeZoneAPI{getErr: authErr}
			r := reconcilerWithFakeZoneAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred()) // terminal — nil return

			cond := getZoneCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("TerminalError"))
		})

		It("sets Ready=False APIError when CreateZone returns a retryable error", func() {
			createAccount(true)
			createSecret()
			createZone(nil)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())

			fake := &fakeZoneAPI{
				listZones: []cf.Zone{},
				createErr: errors.New("upstream connect error"),
			}
			r := reconcilerWithFakeZoneAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).To(HaveOccurred())

			cond := getZoneCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("APIError"))
		})

		It("sets Ready=False APIError when EditZone returns a retryable error", func() {
			createAccount(true)
			createSecret()
			createZone(nil)
			setZoneIDInStatus(fakeZoneID)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())

			driftedZone := cf.Zone{ID: fakeZoneID, Name: domainName, Type: "partial", Status: "active"}
			fake := &fakeZoneAPI{
				getZone: driftedZone,
				editErr: errors.New("upstream timeout"),
			}
			r := reconcilerWithFakeZoneAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).To(HaveOccurred())

			cond := getZoneCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("APIError"))
		})

		It("returns an error from reconcileDelete when account is not found during deletion", func() {
			// No account created — deletion should fail to resolve credentials.
			createZone(map[string]string{"cloudflare.k8s.io/deletion-policy": "delete"})
			setZoneIDInStatus(fakeZoneID)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())
			Expect(k8sClient.Delete(ctx, zone)).To(Succeed())

			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			Expect(zone.DeletionTimestamp).NotTo(BeNil())

			r := reconcilerWithFakeZoneAPI(&fakeZoneAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).To(HaveOccurred())
		})

		It("returns an error from reconcileDelete when secret is not found during deletion", func() {
			createAccount(true) // account exists but no secret
			createZone(map[string]string{"cloudflare.k8s.io/deletion-policy": "delete"})
			setZoneIDInStatus(fakeZoneID)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())
			Expect(k8sClient.Delete(ctx, zone)).To(Succeed())

			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			Expect(zone.DeletionTimestamp).NotTo(BeNil())

			r := reconcilerWithFakeZoneAPI(&fakeZoneAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).To(HaveOccurred())
		})

		It("returns an error from reconcileDelete when the token key is missing during deletion", func() {
			createAccount(true)
			// Secret exists but with wrong key.
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretNS},
				Data:       map[string][]byte{"WRONG_KEY": []byte(fakeToken)},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			createZone(map[string]string{"cloudflare.k8s.io/deletion-policy": "delete"})
			setZoneIDInStatus(fakeZoneID)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())
			Expect(k8sClient.Delete(ctx, zone)).To(Succeed())

			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			Expect(zone.DeletionTimestamp).NotTo(BeNil())

			r := reconcilerWithFakeZoneAPI(&fakeZoneAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).To(HaveOccurred())
		})

		It("falls back to defaultZoneAPI when NewZoneAPI is nil during deletion and returns error on empty token", func() {
			createAccount(true)
			// Secret with empty token so defaultZoneAPI("") fails.
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretNS},
				Data:       map[string][]byte{tokenKey: []byte("")},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			createZone(map[string]string{"cloudflare.k8s.io/deletion-policy": "delete"})
			setZoneIDInStatus(fakeZoneID)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())
			Expect(k8sClient.Delete(ctx, zone)).To(Succeed())

			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			Expect(zone.DeletionTimestamp).NotTo(BeNil())

			// No NewZoneAPI — falls back to defaultZoneAPI, which errors on empty token.
			r := &ZoneReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).To(HaveOccurred())
		})

		It("returns an error from reconcileDelete when DeleteZone returns a non-NotFound error", func() {
			createAccount(true)
			createSecret()
			createZone(map[string]string{"cloudflare.k8s.io/deletion-policy": "delete"})
			setZoneIDInStatus(fakeZoneID)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())
			Expect(k8sClient.Delete(ctx, zone)).To(Succeed())

			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			Expect(zone.DeletionTimestamp).NotTo(BeNil())

			fake := &fakeZoneAPI{deleteErr: errors.New("cloudflare API unavailable")}
			r := reconcilerWithFakeZoneAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).To(HaveOccurred())
			Expect(fake.deleteCalled).To(BeTrue())
		})

		It("uses CF_API_TOKEN as the default key when TokenSecretRef.Key is empty", func() {
			// Create account with no Key set in TokenSecretRef.
			acct := &cloudflarev1alpha1.CloudflareAccount{
				ObjectMeta: metav1.ObjectMeta{Name: accountName},
				Spec: cloudflarev1alpha1.CloudflareAccountSpec{
					AccountID: fakeAcctID,
					TokenSecretRef: cloudflarev1alpha1.SecretReference{
						Name:      secretName,
						Namespace: secretNS,
						// Key intentionally omitted — should default to CF_API_TOKEN.
					},
				},
			}
			Expect(k8sClient.Create(ctx, acct)).To(Succeed())
			Expect(k8sClient.Get(ctx, accountKey, acct)).To(Succeed())
			acct.Status.Conditions = []metav1.Condition{{
				Type:               cloudflarev1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             "Validated",
				LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, acct)).To(Succeed())

			// Secret uses the default CF_API_TOKEN key.
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretNS},
				Data:       map[string][]byte{"CF_API_TOKEN": []byte(fakeToken)},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			createZone(nil)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, zone)).To(Succeed())

			fake := &fakeZoneAPI{
				listZones:  []cf.Zone{},
				createZone: cf.Zone{ID: fakeZoneID, Name: domainName, Type: "full", Status: "active"},
			}
			r := reconcilerWithFakeZoneAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: zoneKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getZoneCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})
	})
})
