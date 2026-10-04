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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// fakeDNSRecordAPI is a test double for DNSRecordAPI.
type fakeDNSRecordAPI struct {
	createRecord cf.DNSRecord
	createErr    error
	getRecord    cf.DNSRecord
	getErr       error
	listRecords  []cf.DNSRecord
	listErr      error
	updateRecord cf.DNSRecord
	updateErr    error
	deleteErr    error

	updateCalled bool
	deleteCalled bool
	updateParams cf.UpdateDNSRecordParams
}

func (f *fakeDNSRecordAPI) CreateDNSRecord(_ context.Context, _ *cf.ResourceContainer, _ cf.CreateDNSRecordParams) (cf.DNSRecord, error) {
	return f.createRecord, f.createErr
}
func (f *fakeDNSRecordAPI) GetDNSRecord(_ context.Context, _ *cf.ResourceContainer, _ string) (cf.DNSRecord, error) {
	return f.getRecord, f.getErr
}
func (f *fakeDNSRecordAPI) ListDNSRecords(_ context.Context, _ *cf.ResourceContainer, _ cf.ListDNSRecordsParams) ([]cf.DNSRecord, *cf.ResultInfo, error) {
	return f.listRecords, nil, f.listErr
}
func (f *fakeDNSRecordAPI) UpdateDNSRecord(_ context.Context, _ *cf.ResourceContainer, params cf.UpdateDNSRecordParams) (cf.DNSRecord, error) {
	f.updateCalled = true
	f.updateParams = params
	return f.updateRecord, f.updateErr
}
func (f *fakeDNSRecordAPI) DeleteDNSRecord(_ context.Context, _ *cf.ResourceContainer, _ string) error {
	f.deleteCalled = true
	return f.deleteErr
}

// reconcilerWithFakeDNSAPI returns a DNSRecordReconciler wired to a pre-built fake.
func reconcilerWithFakeDNSAPI(fake DNSRecordAPI) *DNSRecordReconciler {
	return &DNSRecordReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		NewDNSRecordAPI: func(_ string) (DNSRecordAPI, error) {
			return fake, nil
		},
	}
}

// reconcilerWithDNSClientErr returns a DNSRecordReconciler whose client constructor always errors.
func reconcilerWithDNSClientErr(err error) *DNSRecordReconciler {
	return &DNSRecordReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		NewDNSRecordAPI: func(_ string) (DNSRecordAPI, error) {
			return nil, err
		},
	}
}

var _ = Describe("DNSRecord Controller", func() {
	const (
		dnsRecordCRName = "test-dns-record"
		dnsRecordNS     = "default"
		dnsZoneCRName   = "dns-test-zone"
		dnsAccountName  = "dns-test-account"
		dnsSecretName   = "dns-test-secret"
		dnsSecretNS     = "default"
		dnsTokenKey     = "CF_API_TOKEN"
		dnsFakeToken    = "fake-dns-token"
		dnsFakeAcctID   = "dns-acct-123"
		dnsFakeZoneID   = "dns-zone-456"
		dnsFakeRecordID = "dns-record-789"
		dnsRecordName   = "www.example.com"
		dnsRecordType   = "A"
		dnsRecordIP     = "1.2.3.4"
	)

	ctx := context.Background()
	recordKey := types.NamespacedName{Name: dnsRecordCRName, Namespace: dnsRecordNS}
	zoneKey := types.NamespacedName{Name: dnsZoneCRName, Namespace: dnsRecordNS}
	accountKey := types.NamespacedName{Name: dnsAccountName}
	secretKey := types.NamespacedName{Name: dnsSecretName, Namespace: dnsSecretNS}

	// createDNSAccount creates a CloudflareAccount CR and optionally marks it Ready=True.
	createDNSAccount := func(ready bool) {
		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: dnsAccountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID: dnsFakeAcctID,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{
					Name:      dnsSecretName,
					Namespace: dnsSecretNS,
					Key:       dnsTokenKey,
				},
			},
		}
		Expect(k8sClient.Create(ctx, acct)).To(Succeed())

		if ready {
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

	// createDNSSecret creates the credentials secret.
	createDNSSecret := func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: dnsSecretName, Namespace: dnsSecretNS},
			Data:       map[string][]byte{dnsTokenKey: []byte(dnsFakeToken)},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	}

	// createReadyZone creates a Zone CR with Ready=True and a zone ID in status.
	createReadyZone := func() {
		zone := &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: dnsZoneCRName, Namespace: dnsRecordNS},
			Spec: cloudflarev1alpha1.ZoneSpec{
				Name:       "example.com",
				AccountRef: corev1.LocalObjectReference{Name: dnsAccountName},
				Type:       "full",
			},
		}
		Expect(k8sClient.Create(ctx, zone)).To(Succeed())
		Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
		zone.Status.CloudflareMetadata.ZoneID = dnsFakeZoneID
		zone.Status.Conditions = []metav1.Condition{
			{
				Type:               cloudflarev1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             "Synced",
				Message:            "Zone is synced",
				ObservedGeneration: zone.Generation,
				LastTransitionTime: metav1.Now(),
			},
		}
		Expect(k8sClient.Status().Update(ctx, zone)).To(Succeed())
	}

	// createNotReadyZone creates a Zone CR with no Ready condition.
	createNotReadyZone := func() {
		zone := &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: dnsZoneCRName, Namespace: dnsRecordNS},
			Spec: cloudflarev1alpha1.ZoneSpec{
				Name:       "example.com",
				AccountRef: corev1.LocalObjectReference{Name: dnsAccountName},
				Type:       "full",
			},
		}
		Expect(k8sClient.Create(ctx, zone)).To(Succeed())
	}

	// createDNSRecord creates a DNSRecord CR. annotations may be nil.
	createDNSRecord := func(annotations map[string]string) *cloudflarev1alpha1.DNSRecord {
		record := &cloudflarev1alpha1.DNSRecord{
			ObjectMeta: metav1.ObjectMeta{
				Name:        dnsRecordCRName,
				Namespace:   dnsRecordNS,
				Annotations: annotations,
			},
			Spec: cloudflarev1alpha1.DNSRecordSpec{
				ZoneRef: corev1.LocalObjectReference{Name: dnsZoneCRName},
				Name:    dnsRecordName,
				Type:    dnsRecordType,
				Content: dnsRecordIP,
				TTL:     300,
			},
		}
		Expect(k8sClient.Create(ctx, record)).To(Succeed())
		return record
	}

	// setRecordIDInStatus directly writes the record ID into status.
	setRecordIDInStatus := func(recordID string) {
		record := &cloudflarev1alpha1.DNSRecord{}
		Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
		record.Status.CloudflareMetadata.RecordID = recordID
		Expect(k8sClient.Status().Update(ctx, record)).To(Succeed())
	}

	// getRecordCondition fetches the record and returns the named condition (or nil).
	getRecordCondition := func(condType string) *metav1.Condition {
		record := &cloudflarev1alpha1.DNSRecord{}
		Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
		for i := range record.Status.Conditions {
			if record.Status.Conditions[i].Type == condType {
				return &record.Status.Conditions[i]
			}
		}
		return nil
	}

	AfterEach(func() {
		record := &cloudflarev1alpha1.DNSRecord{}
		if err := k8sClient.Get(ctx, recordKey, record); err == nil {
			record.Finalizers = nil
			_ = k8sClient.Update(ctx, record)
			_ = k8sClient.Delete(ctx, record)
		}
		zone := &cloudflarev1alpha1.Zone{}
		if err := k8sClient.Get(ctx, zoneKey, zone); err == nil {
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
		It("returns no error when the DNSRecord CR does not exist", func() {
			r := reconcilerWithFakeDNSAPI(&fakeDNSRecordAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
		})

		It("adds the finalizer on first reconcile and returns without calling CF", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(nil)

			fake := &fakeDNSRecordAPI{}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())

			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			Expect(record.Finalizers).To(ContainElement(reconciler.Finalizer))

			// No CF API calls should have been made.
			Expect(fake.updateCalled).To(BeFalse())
			Expect(fake.deleteCalled).To(BeFalse())
		})

		It("sets Ready=False ZoneNotFound when the Zone is missing", func() {
			// No zone created.
			createDNSRecord(nil)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			r := reconcilerWithFakeDNSAPI(&fakeDNSRecordAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("ZoneNotFound"))
		})

		It("sets Ready=False ZoneNotReady and returns an error when the Zone is not ready", func() {
			createNotReadyZone()
			createDNSRecord(nil)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			r := reconcilerWithFakeDNSAPI(&fakeDNSRecordAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).To(HaveOccurred()) // returns error to trigger requeue

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("ZoneNotReady"))
		})

		It("sets Ready=False AccountNotFound when the CloudflareAccount is missing", func() {
			// Zone exists but account does not.
			createReadyZone()
			createDNSRecord(nil)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			r := reconcilerWithFakeDNSAPI(&fakeDNSRecordAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("AccountNotFound"))
		})

		It("sets Ready=False AccountNotReady when the CloudflareAccount is not ready", func() {
			createDNSAccount(false) // not ready
			createReadyZone()
			createDNSRecord(nil)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			r := reconcilerWithFakeDNSAPI(&fakeDNSRecordAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("AccountNotReady"))
		})

		It("sets Ready=False SecretNotFound when the credentials secret is missing", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSRecord(nil)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			r := reconcilerWithFakeDNSAPI(&fakeDNSRecordAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("SecretNotFound"))
		})

		It("sets Ready=False TokenKeyMissing when the token key is absent from the secret", func() {
			createDNSAccount(true)
			createReadyZone()
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: dnsSecretName, Namespace: dnsSecretNS},
				Data:       map[string][]byte{"WRONG_KEY": []byte(dnsFakeToken)},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			createDNSRecord(nil)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			r := reconcilerWithFakeDNSAPI(&fakeDNSRecordAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("TokenKeyMissing"))
		})

		It("sets Ready=False InvalidToken when the CF client cannot be constructed", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(nil)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			r := reconcilerWithDNSClientErr(errors.New("bad token"))
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("InvalidToken"))
		})

		It("creates a new record when none exists and sets Ready=True with recordID", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(nil)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			created := cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300, Proxiable: true}
			fake := &fakeDNSRecordAPI{listRecords: []cf.DNSRecord{}, createRecord: created}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("Synced"))

			updated := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, updated)).To(Succeed())
			Expect(updated.Status.CloudflareMetadata.RecordID).To(Equal(dnsFakeRecordID))
			Expect(updated.Status.CloudflareMetadata.ZoneID).To(Equal(dnsFakeZoneID))
			Expect(updated.Status.CloudflareMetadata.Proxiable).To(BeTrue())
		})

		It("adopts a pre-existing record found via List", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(nil)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			existing := cf.DNSRecord{ID: "existing-111", ZoneID: dnsFakeZoneID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300}
			fake := &fakeDNSRecordAPI{listRecords: []cf.DNSRecord{existing}}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())

			updated := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, updated)).To(Succeed())
			Expect(updated.Status.CloudflareMetadata.RecordID).To(Equal("existing-111"))

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("reconciles without drift when record already exists in status (no update)", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(nil)
			setRecordIDInStatus(dnsFakeRecordID)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			// Cloudflare always reports proxied, even for records that never set it.
			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID, Name: dnsRecordName, Type: dnsRecordType,
				Content: dnsRecordIP, TTL: 300, Proxied: cf.BoolPtr(false), Tags: []string{}}
			fake := &fakeDNSRecordAPI{getRecord: cfRecord}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.updateCalled).To(BeFalse())

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("does not update a record that leaves ttl and proxied unset", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			rec := &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: dnsRecordCRName, Namespace: dnsRecordNS},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					ZoneRef: corev1.LocalObjectReference{Name: dnsZoneCRName},
					Name:    dnsRecordName,
					Type:    dnsRecordType,
					Content: dnsRecordIP,
				},
			}
			Expect(k8sClient.Create(ctx, rec)).To(Succeed())
			setRecordIDInStatus(dnsFakeRecordID)
			Expect(k8sClient.Get(ctx, recordKey, rec)).To(Succeed())
			rec.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, rec)).To(Succeed())

			// What Cloudflare returns for a record created without ttl or proxied.
			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID, Name: dnsRecordName, Type: dnsRecordType,
				Content: dnsRecordIP, TTL: 1, Proxied: cf.BoolPtr(false), Tags: []string{}}
			fake := &fakeDNSRecordAPI{getRecord: cfRecord}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.updateCalled).To(BeFalse())
		})

		It("calls UpdateDNSRecord when content has drifted", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(nil) // spec content = "1.2.3.4"
			setRecordIDInStatus(dnsFakeRecordID)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			// CF has a different IP.
			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID, Name: dnsRecordName, Type: dnsRecordType, Content: "9.9.9.9", TTL: 300}
			updated := cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300}
			fake := &fakeDNSRecordAPI{getRecord: cfRecord, updateRecord: updated}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.updateCalled).To(BeTrue())

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("calls UpdateDNSRecord when TTL has drifted", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(nil) // spec TTL = 300
			setRecordIDInStatus(dnsFakeRecordID)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 1}
			updated := cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300}
			fake := &fakeDNSRecordAPI{getRecord: cfRecord, updateRecord: updated}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.updateCalled).To(BeTrue())
		})

		It("calls UpdateDNSRecord when Proxied has drifted", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()

			// Create record with proxied=true.
			proxied := true
			rec := &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: dnsRecordCRName, Namespace: dnsRecordNS},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					ZoneRef: corev1.LocalObjectReference{Name: dnsZoneCRName},
					Name:    dnsRecordName,
					Type:    dnsRecordType,
					Content: dnsRecordIP,
					TTL:     300,
					Proxied: &proxied,
				},
			}
			Expect(k8sClient.Create(ctx, rec)).To(Succeed())
			setRecordIDInStatus(dnsFakeRecordID)
			Expect(k8sClient.Get(ctx, recordKey, rec)).To(Succeed())
			rec.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, rec)).To(Succeed())

			cfProxied := false
			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300, Proxied: &cfProxied}
			fake := &fakeDNSRecordAPI{getRecord: cfRecord, updateRecord: cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID}}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.updateCalled).To(BeTrue())
		})

		It("calls UpdateDNSRecord when Priority has drifted", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()

			priority := uint16(10)
			rec := &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: dnsRecordCRName, Namespace: dnsRecordNS},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					ZoneRef:  corev1.LocalObjectReference{Name: dnsZoneCRName},
					Name:     "mail.example.com",
					Type:     "MX",
					Content:  "mail.example.com",
					TTL:      300,
					Priority: &priority,
				},
			}
			Expect(k8sClient.Create(ctx, rec)).To(Succeed())
			setRecordIDInStatus(dnsFakeRecordID)
			Expect(k8sClient.Get(ctx, recordKey, rec)).To(Succeed())
			rec.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, rec)).To(Succeed())

			cfPriority := uint16(20)
			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID, Name: "mail.example.com", Type: "MX", Content: "mail.example.com", TTL: 300, Priority: &cfPriority}
			fake := &fakeDNSRecordAPI{getRecord: cfRecord, updateRecord: cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID}}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.updateCalled).To(BeTrue())
		})

		It("calls UpdateDNSRecord when Comment has drifted", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()

			rec := &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: dnsRecordCRName, Namespace: dnsRecordNS},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					ZoneRef: corev1.LocalObjectReference{Name: dnsZoneCRName},
					Name:    dnsRecordName,
					Type:    dnsRecordType,
					Content: dnsRecordIP,
					TTL:     300,
					Comment: "new comment",
				},
			}
			Expect(k8sClient.Create(ctx, rec)).To(Succeed())
			setRecordIDInStatus(dnsFakeRecordID)
			Expect(k8sClient.Get(ctx, recordKey, rec)).To(Succeed())
			rec.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, rec)).To(Succeed())

			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300, Comment: "old comment"}
			fake := &fakeDNSRecordAPI{getRecord: cfRecord, updateRecord: cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID}}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.updateCalled).To(BeTrue())
		})

		It("calls UpdateDNSRecord when Tags have drifted", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()

			rec := &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: dnsRecordCRName, Namespace: dnsRecordNS},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					ZoneRef: corev1.LocalObjectReference{Name: dnsZoneCRName},
					Name:    dnsRecordName,
					Type:    dnsRecordType,
					Content: dnsRecordIP,
					TTL:     300,
					Tags:    []string{"env:prod", "owner:platform"},
				},
			}
			Expect(k8sClient.Create(ctx, rec)).To(Succeed())
			setRecordIDInStatus(dnsFakeRecordID)
			Expect(k8sClient.Get(ctx, recordKey, rec)).To(Succeed())
			rec.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, rec)).To(Succeed())

			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300, Tags: []string{"env:staging"}}
			fake := &fakeDNSRecordAPI{getRecord: cfRecord, updateRecord: cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID}}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.updateCalled).To(BeTrue())
		})

		It("calls UpdateDNSRecord when Data has drifted", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()

			rec := &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: dnsRecordCRName, Namespace: dnsRecordNS},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					ZoneRef: corev1.LocalObjectReference{Name: dnsZoneCRName},
					Name:    "_http._tcp.example.com",
					Type:    "SRV",
					TTL:     300,
					Data:    &apiextensionsv1.JSON{Raw: []byte(`{"port":8080,"weight":1}`)},
				},
			}
			Expect(k8sClient.Create(ctx, rec)).To(Succeed())
			setRecordIDInStatus(dnsFakeRecordID)
			Expect(k8sClient.Get(ctx, recordKey, rec)).To(Succeed())
			rec.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, rec)).To(Succeed())

			cfRecord := cf.DNSRecord{
				ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID,
				Name: "_http._tcp.example.com", Type: "SRV", TTL: 300,
				Data: map[string]interface{}{"port": float64(9000), "weight": float64(1)},
			}
			fake := &fakeDNSRecordAPI{getRecord: cfRecord, updateRecord: cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID}}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.updateCalled).To(BeTrue())
		})

		It("does NOT call UpdateDNSRecord for content drift on SRV records", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()

			rec := &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: dnsRecordCRName, Namespace: dnsRecordNS},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					ZoneRef: corev1.LocalObjectReference{Name: dnsZoneCRName},
					Name:    "_http._tcp.example.com",
					Type:    "SRV",
					Content: "spec-content",
					TTL:     300,
				},
			}
			Expect(k8sClient.Create(ctx, rec)).To(Succeed())
			setRecordIDInStatus(dnsFakeRecordID)
			Expect(k8sClient.Get(ctx, recordKey, rec)).To(Succeed())
			rec.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, rec)).To(Succeed())

			// CF has different content (auto-formatted by CF for SRV), but TTL matches.
			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID, Name: "_http._tcp.example.com", Type: "SRV", Content: "cf-auto-formatted-content", TTL: 300}
			fake := &fakeDNSRecordAPI{getRecord: cfRecord}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.updateCalled).To(BeFalse()) // content drift skipped for SRV
		})

		It("recreates a record deleted externally (GetDNSRecord returns NotFound)", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(nil)
			setRecordIDInStatus(dnsFakeRecordID)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			notFoundErrVal := cf.NewNotFoundError(&cf.Error{StatusCode: 404, Type: cf.ErrorTypeNotFound})
			notFoundErr := &notFoundErrVal
			recreated := cf.DNSRecord{ID: "new-record-abc", ZoneID: dnsFakeZoneID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300}
			fake := &fakeDNSRecordAPI{
				getErr:       notFoundErr,
				listRecords:  []cf.DNSRecord{},
				createRecord: recreated,
			}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())

			updated := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, updated)).To(Succeed())
			Expect(updated.Status.CloudflareMetadata.RecordID).To(Equal("new-record-abc"))
		})

		It("sets Ready=False TerminalError and returns nil for terminal CF API errors", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(nil)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			authErrVal := cf.NewAuthorizationError(&cf.Error{StatusCode: 403, Type: cf.ErrorTypeAuthorization})
			authErr := &authErrVal
			fake := &fakeDNSRecordAPI{listErr: authErr}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred()) // terminal — nil return, stop requeuing

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("TerminalError"))
		})

		It("sets Ready=False APIError and returns an error for retryable CF API errors", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(nil)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			retryableErr := errors.New("connection reset by peer")
			fake := &fakeDNSRecordAPI{listErr: retryableErr}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).To(HaveOccurred()) // triggers backoff requeue

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("APIError"))
		})

		It("sets Ready=False TerminalError when GetDNSRecord returns a non-NotFound terminal error", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(nil)
			setRecordIDInStatus(dnsFakeRecordID)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			authErrVal := cf.NewAuthorizationError(&cf.Error{StatusCode: 403, Type: cf.ErrorTypeAuthorization})
			authErr := &authErrVal
			fake := &fakeDNSRecordAPI{getErr: authErr}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("TerminalError"))
		})

		It("sets Ready=False APIError when UpdateDNSRecord returns a retryable error", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(nil) // spec content = "1.2.3.4", TTL = 300
			setRecordIDInStatus(dnsFakeRecordID)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			// CF has different content → drift → update → returns error.
			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID, Name: dnsRecordName, Type: dnsRecordType, Content: "9.9.9.9", TTL: 300}
			fake := &fakeDNSRecordAPI{
				getRecord: cfRecord,
				updateErr: errors.New("upstream timeout"),
			}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).To(HaveOccurred())

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("APIError"))
		})

		It("removes the finalizer without calling CF delete when deletion-policy=retain", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(map[string]string{"cloudflare.k8s.io/deletion-policy": "retain"})
			setRecordIDInStatus(dnsFakeRecordID)

			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())
			Expect(k8sClient.Delete(ctx, record)).To(Succeed())

			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			Expect(record.DeletionTimestamp).NotTo(BeNil())

			fake := &fakeDNSRecordAPI{}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.deleteCalled).To(BeFalse()) // CF delete must NOT be called

			updated := &cloudflarev1alpha1.DNSRecord{}
			err = k8sClient.Get(ctx, recordKey, updated)
			if err == nil {
				Expect(updated.Finalizers).NotTo(ContainElement(reconciler.Finalizer))
			}
		})

		It("calls CF delete and removes the finalizer when deletion-policy=delete", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(map[string]string{"cloudflare.k8s.io/deletion-policy": "delete"})
			setRecordIDInStatus(dnsFakeRecordID)

			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())
			Expect(k8sClient.Delete(ctx, record)).To(Succeed())

			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			Expect(record.DeletionTimestamp).NotTo(BeNil())

			fake := &fakeDNSRecordAPI{}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.deleteCalled).To(BeTrue()) // CF delete MUST be called

			updated := &cloudflarev1alpha1.DNSRecord{}
			err = k8sClient.Get(ctx, recordKey, updated)
			if err == nil {
				Expect(updated.Finalizers).NotTo(ContainElement(reconciler.Finalizer))
			}
		})

		It("returns no error when reconcileDelete is called with no finalizer", func() {
			// Call reconcileDelete directly with a record that has no finalizer.
			rec := &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: "no-finalizer-record", Namespace: dnsRecordNS},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					ZoneRef: corev1.LocalObjectReference{Name: dnsZoneCRName},
					Name:    dnsRecordName,
					Type:    dnsRecordType,
					Content: dnsRecordIP,
				},
			}
			r := &DNSRecordReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			result, err := r.reconcileDelete(context.Background(), rec)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
		})

		It("removes the finalizer without calling CF delete when the Zone is already gone", func() {
			// No zone: it was deleted first, e.g. together with its namespace.
			createDNSRecord(map[string]string{"cloudflare.k8s.io/deletion-policy": "delete"})
			setRecordIDInStatus(dnsFakeRecordID)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())
			Expect(k8sClient.Delete(ctx, record)).To(Succeed())

			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			Expect(record.DeletionTimestamp).NotTo(BeNil())

			fake := &fakeDNSRecordAPI{}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.deleteCalled).To(BeFalse())

			err = k8sClient.Get(ctx, recordKey, record)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("returns an error from reconcileDelete when DeleteDNSRecord returns a non-NotFound error", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(map[string]string{"cloudflare.k8s.io/deletion-policy": "delete"})
			setRecordIDInStatus(dnsFakeRecordID)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())
			Expect(k8sClient.Delete(ctx, record)).To(Succeed())

			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			Expect(record.DeletionTimestamp).NotTo(BeNil())

			fake := &fakeDNSRecordAPI{deleteErr: errors.New("cloudflare API unavailable")}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).To(HaveOccurred())
			Expect(fake.deleteCalled).To(BeTrue())
		})

		It("falls back to defaultDNSRecordAPI when NewDNSRecordAPI is nil and errors on empty token", func() {
			createDNSAccount(true)
			createReadyZone()
			// Secret with empty token so defaultDNSRecordAPI("") fails.
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: dnsSecretName, Namespace: dnsSecretNS},
				Data:       map[string][]byte{dnsTokenKey: []byte("")},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			createDNSRecord(nil)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			// No NewDNSRecordAPI — falls back to defaultDNSRecordAPI.
			r := &DNSRecordReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("InvalidToken"))
		})

		It("creates a new record with a data field (SRV) and sets Ready=True", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()

			rec := &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: dnsRecordCRName, Namespace: dnsRecordNS},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					ZoneRef: corev1.LocalObjectReference{Name: dnsZoneCRName},
					Name:    "_http._tcp.example.com",
					Type:    "SRV",
					TTL:     300,
					Data:    &apiextensionsv1.JSON{Raw: []byte(`{"port":8080,"weight":1}`)},
				},
			}
			Expect(k8sClient.Create(ctx, rec)).To(Succeed())
			Expect(k8sClient.Get(ctx, recordKey, rec)).To(Succeed())
			rec.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, rec)).To(Succeed())

			// CF returns the data field matching what was sent.
			srvData := map[string]interface{}{"port": float64(8080), "weight": float64(1)}
			created := cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID, Name: "_http._tcp.example.com", Type: "SRV", TTL: 300, Data: srvData}
			fake := &fakeDNSRecordAPI{listRecords: []cf.DNSRecord{}, createRecord: created}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))

			updated := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, updated)).To(Succeed())
			Expect(updated.Status.CloudflareMetadata.RecordID).To(Equal(dnsFakeRecordID))
		})

		It("returns an error from reconcileDelete when account is not found during deletion", func() {
			// No account created — zone exists but account was deleted.
			createReadyZone()
			createDNSRecord(map[string]string{"cloudflare.k8s.io/deletion-policy": "delete"})
			setRecordIDInStatus(dnsFakeRecordID)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())
			Expect(k8sClient.Delete(ctx, record)).To(Succeed())

			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			Expect(record.DeletionTimestamp).NotTo(BeNil())

			r := reconcilerWithFakeDNSAPI(&fakeDNSRecordAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).To(HaveOccurred())
		})

		It("returns an error from reconcileDelete when secret is not found during deletion", func() {
			createDNSAccount(true) // account exists but no secret
			createReadyZone()
			createDNSRecord(map[string]string{"cloudflare.k8s.io/deletion-policy": "delete"})
			setRecordIDInStatus(dnsFakeRecordID)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())
			Expect(k8sClient.Delete(ctx, record)).To(Succeed())

			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			Expect(record.DeletionTimestamp).NotTo(BeNil())

			r := reconcilerWithFakeDNSAPI(&fakeDNSRecordAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).To(HaveOccurred())
		})

		It("returns an error from reconcileDelete when the token key is missing during deletion", func() {
			createDNSAccount(true)
			createReadyZone()
			// Secret with wrong key.
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: dnsSecretName, Namespace: dnsSecretNS},
				Data:       map[string][]byte{"WRONG_KEY": []byte(dnsFakeToken)},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			createDNSRecord(map[string]string{"cloudflare.k8s.io/deletion-policy": "delete"})
			setRecordIDInStatus(dnsFakeRecordID)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())
			Expect(k8sClient.Delete(ctx, record)).To(Succeed())

			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			Expect(record.DeletionTimestamp).NotTo(BeNil())

			r := reconcilerWithFakeDNSAPI(&fakeDNSRecordAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).To(HaveOccurred())
		})

		It("returns an error from reconcileDelete when the CF client fails to build during deletion", func() {
			createDNSAccount(true)
			createReadyZone()
			// Secret with empty token so defaultDNSRecordAPI("") fails.
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: dnsSecretName, Namespace: dnsSecretNS},
				Data:       map[string][]byte{dnsTokenKey: []byte("")},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			createDNSRecord(map[string]string{"cloudflare.k8s.io/deletion-policy": "delete"})
			setRecordIDInStatus(dnsFakeRecordID)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())
			Expect(k8sClient.Delete(ctx, record)).To(Succeed())

			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			Expect(record.DeletionTimestamp).NotTo(BeNil())

			r := reconcilerWithDNSClientErr(errors.New("bad token"))
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).To(HaveOccurred())
		})

		It("uses CF_API_TOKEN as the default key when TokenSecretRef.Key is empty", func() {
			// Account with no Key set — should default to CF_API_TOKEN.
			acct := &cloudflarev1alpha1.CloudflareAccount{
				ObjectMeta: metav1.ObjectMeta{Name: dnsAccountName},
				Spec: cloudflarev1alpha1.CloudflareAccountSpec{
					AccountID: dnsFakeAcctID,
					TokenSecretRef: cloudflarev1alpha1.SecretReference{
						Name:      dnsSecretName,
						Namespace: dnsSecretNS,
						// Key intentionally omitted.
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

			createReadyZone()
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: dnsSecretName, Namespace: dnsSecretNS},
				Data:       map[string][]byte{"CF_API_TOKEN": []byte(dnsFakeToken)},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			createDNSRecord(nil)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			created := cf.DNSRecord{ID: dnsFakeRecordID, ZoneID: dnsFakeZoneID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300}
			fake := &fakeDNSRecordAPI{listRecords: []cf.DNSRecord{}, createRecord: created}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Describe("Validation", func() {
		getRecord := func() *cloudflarev1alpha1.DNSRecord {
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			return record
		}

		BeforeEach(func() { createDNSRecord(nil) })

		It("rejects changing spec.name", func() {
			record := getRecord()
			record.Spec.Name = "api.example.com"
			Expect(k8sClient.Update(ctx, record)).To(MatchError(ContainSubstring("name is immutable")))
		})

		It("rejects changing spec.type", func() {
			record := getRecord()
			record.Spec.Type = "AAAA"
			Expect(k8sClient.Update(ctx, record)).To(MatchError(ContainSubstring("type is immutable")))
		})

		It("rejects changing spec.zoneRef", func() {
			record := getRecord()
			record.Spec.ZoneRef.Name = "other-zone"
			Expect(k8sClient.Update(ctx, record)).To(MatchError(ContainSubstring("zoneRef is immutable")))
		})

		It("allows changing spec.content", func() {
			record := getRecord()
			record.Spec.Content = "5.6.7.8"
			Expect(k8sClient.Update(ctx, record)).To(Succeed())
		})

		It("rejects a ttl Cloudflare cannot accept", func() {
			record := getRecord()
			record.Spec.TTL = 10
			Expect(k8sClient.Update(ctx, record)).To(MatchError(ContainSubstring("ttl must be 1 (automatic)")))
		})
	})
})
