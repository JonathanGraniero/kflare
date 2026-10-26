/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"sync"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// fakeDNSRecordAPI is a test double for DNSRecordAPI. Its calls hold a lock,
// so a controller running in a manager can use it; read what it recorded
// from such a spec through lastUpdate.
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

	mu           sync.Mutex
	updateCalled bool
	deleteCalled bool
	createCalled bool
	createParams cf.CreateDNSRecordParams
	updateParams cf.UpdateDNSRecordParams
}

func (f *fakeDNSRecordAPI) CreateDNSRecord(_ context.Context, _ *cf.ResourceContainer, params cf.CreateDNSRecordParams) (cf.DNSRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalled = true
	f.createParams = params
	return f.createRecord, f.createErr
}
func (f *fakeDNSRecordAPI) GetDNSRecord(_ context.Context, _ *cf.ResourceContainer, _ string) (cf.DNSRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getRecord, f.getErr
}
func (f *fakeDNSRecordAPI) ListDNSRecords(_ context.Context, _ *cf.ResourceContainer, _ cf.ListDNSRecordsParams) ([]cf.DNSRecord, *cf.ResultInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listRecords, nil, f.listErr
}
func (f *fakeDNSRecordAPI) UpdateDNSRecord(_ context.Context, _ *cf.ResourceContainer, params cf.UpdateDNSRecordParams) (cf.DNSRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalled = true
	f.updateParams = params
	return f.updateRecord, f.updateErr
}
func (f *fakeDNSRecordAPI) DeleteDNSRecord(_ context.Context, _ *cf.ResourceContainer, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalled = true
	return f.deleteErr
}

// lastUpdate returns the params of the last UpdateDNSRecord call, and
// whether there was one.
func (f *fakeDNSRecordAPI) lastUpdate() (cf.UpdateDNSRecordParams, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.updateParams, f.updateCalled
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

		It("sets Ready=False ZoneNotReady when the Zone is not ready", func() {
			createNotReadyZone()
			createDNSRecord(nil)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			r := reconcilerWithFakeDNSAPI(&fakeDNSRecordAPI{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred()) // the Zone watch re-triggers it

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

			created := cf.DNSRecord{ID: dnsFakeRecordID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300, Proxiable: true}
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
			createDNSRecord(map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt})
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			existing := cf.DNSRecord{ID: "existing-111", Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300}
			fake := &fakeDNSRecordAPI{listRecords: []cf.DNSRecord{existing}}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())

			updated := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, updated)).To(Succeed())
			Expect(updated.Status.CloudflareMetadata.RecordID).To(Equal("existing-111"))
			Expect(updated.Labels).To(HaveKeyWithValue(cloudflarev1alpha1.DNSRecordIDLabel, "existing-111"))
			Expect(updated.Annotations).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))
			Expect(fake.createCalled).To(BeFalse())

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		// unsyncedRecord creates a DNSRecord with annotations and the
		// finalizer, which no reconcile has recorded a record for yet.
		unsyncedRecord := func(annotations map[string]string) {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(annotations)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())
		}
		getRecord := func() *cloudflarev1alpha1.DNSRecord {
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			return record
		}
		identical := cf.DNSRecord{ID: "existing-111", Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP,
			TTL: 300}
		other := cf.DNSRecord{ID: "other-222", Name: dnsRecordName, Type: dnsRecordType, Content: "203.0.113.9", TTL: 300}
		created := cf.DNSRecord{ID: "created-333", Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP,
			TTL: 300}

		It("reports AlreadyExists for an identical record without an adoption policy, and leaves it alone", func() {
			unsyncedRecord(nil)
			fake := &fakeDNSRecordAPI{listRecords: []cf.DNSRecord{identical}}
			result, err := reconcilerWithFakeDNSAPI(fake).Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond.Reason).To(Equal("AlreadyExists"))
			Expect(cond.Message).To(ContainSubstring(dnsRecordType + " " + dnsRecordName))
			Expect(fake.createCalled).To(BeFalse())
			Expect(fake.updateCalled).To(BeFalse())
			Expect(getRecord().Labels).NotTo(HaveKey(cloudflarev1alpha1.DNSRecordIDLabel))
		})

		It("creates a separate member of the set without an adoption policy when the record there differs", func() {
			unsyncedRecord(nil)
			fake := &fakeDNSRecordAPI{listRecords: []cf.DNSRecord{other}, createRecord: created}
			_, err := reconcilerWithFakeDNSAPI(fake).Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.createCalled).To(BeTrue())
			Expect(fake.updateCalled).To(BeFalse(), "the other record is left as it is")
			Expect(getRecord().Status.CloudflareMetadata.RecordID).To(Equal("created-333"))
			Expect(getRecord().Annotations).NotTo(HaveKey(reconciler.AdoptedAnnotation))
		})

		It("adopts and corrects the only record with the name and type with adopt-or-create", func() {
			unsyncedRecord(map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdoptOrCreate})
			corrected := other
			corrected.Content = dnsRecordIP
			fake := &fakeDNSRecordAPI{listRecords: []cf.DNSRecord{other}, updateRecord: corrected}
			_, err := reconcilerWithFakeDNSAPI(fake).Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.createCalled).To(BeFalse())
			Expect(fake.updateCalled).To(BeTrue())
			Expect(fake.updateParams.ID).To(Equal("other-222"))
			Expect(getRecord().Annotations).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))
		})

		It("reports NotFound with the adopt policy when no record matches", func() {
			unsyncedRecord(map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt})
			fake := &fakeDNSRecordAPI{listRecords: []cf.DNSRecord{other, {ID: "other-444", Name: dnsRecordName,
				Type: dnsRecordType, Content: "203.0.113.10"}}}
			_, err := reconcilerWithFakeDNSAPI(fake).Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(getRecordCondition(cloudflarev1alpha1.ConditionReady).Reason).To(Equal("NotFound"))
			Expect(fake.createCalled).To(BeFalse())
			Expect(fake.updateCalled).To(BeFalse())
		})

		It("reports an unknown adoption policy instead of creating or adopting", func() {
			unsyncedRecord(map[string]string{reconciler.AdoptionPolicyAnnotation: "always"})
			fake := &fakeDNSRecordAPI{}
			_, err := reconcilerWithFakeDNSAPI(fake).Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(getRecordCondition(cloudflarev1alpha1.ConditionReady).Reason).To(Equal("InvalidAdoptionPolicy"))
			Expect(fake.createCalled).To(BeFalse())
		})

		It("marks the record it is about to create, and finds it on the next reconcile as its own", func() {
			unsyncedRecord(nil)
			fake := &fakeDNSRecordAPI{createErr: errors.New("lost response")}
			_, err := reconcilerWithFakeDNSAPI(fake).Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).To(HaveOccurred())
			Expect(getRecord().Annotations).To(HaveKeyWithValue(reconciler.CreatingAnnotation,
				dnsRecordType+" "+dnsRecordName))

			By("finding the record the lost create made among the set, without an adoption policy")
			fake = &fakeDNSRecordAPI{listRecords: []cf.DNSRecord{other, created}}
			_, err = reconcilerWithFakeDNSAPI(fake).Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.createCalled).To(BeFalse())
			Expect(getRecord().Status.CloudflareMetadata.RecordID).To(Equal("created-333"))
			Expect(getRecord().Annotations).NotTo(HaveKey(reconciler.CreatingAnnotation), "the claim records it")
			Expect(getRecord().Annotations).NotTo(HaveKey(reconciler.AdoptedAnnotation))
		})

		It("finds the record it claimed when its status was not recorded", func() {
			unsyncedRecord(nil)
			record := getRecord()
			record.Labels = map[string]string{cloudflarev1alpha1.DNSRecordIDLabel: "existing-111"}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())
			fake := &fakeDNSRecordAPI{listRecords: []cf.DNSRecord{identical}}
			_, err := reconcilerWithFakeDNSAPI(fake).Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(getRecordCondition(cloudflarev1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue))
			Expect(getRecord().Status.CloudflareMetadata.RecordID).To(Equal("existing-111"))
			Expect(fake.createCalled).To(BeFalse())
		})

		It("creates a record instead of adopting one another DNSRecord already manages", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(nil)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			// Another DNSRecord, in another namespace, already owns the only
			// record with this name and type.
			owner := &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: "owner-record", Namespace: "kube-public",
					Labels: map[string]string{cloudflarev1alpha1.DNSRecordIDLabel: "taken-111"}},
				Spec: cloudflarev1alpha1.DNSRecordSpec{ZoneRef: corev1.LocalObjectReference{Name: "z"},
					Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP},
			}
			Expect(k8sClient.Create(ctx, owner)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, owner)).To(Succeed()) })

			taken := cf.DNSRecord{ID: "taken-111", Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300}
			created := cf.DNSRecord{ID: "new-222", Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300}
			fake := &fakeDNSRecordAPI{listRecords: []cf.DNSRecord{taken}, createRecord: created}
			_, err := reconcilerWithFakeDNSAPI(fake).Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())

			updated := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, updated)).To(Succeed())
			Expect(updated.Status.CloudflareMetadata.RecordID).To(Equal("new-222"))
			Expect(updated.Labels).To(HaveKeyWithValue(cloudflarev1alpha1.DNSRecordIDLabel, "new-222"))
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
			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, Name: dnsRecordName, Type: dnsRecordType,
				Content: dnsRecordIP, TTL: 300, Proxied: new(false), Tags: []string{}}
			fake := &fakeDNSRecordAPI{getRecord: cfRecord}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.updateCalled).To(BeFalse())

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("schedules a drift check after a sync, once the manager sets an interval", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(nil)
			setRecordIDInStatus(dnsFakeRecordID)
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())

			fake := &fakeDNSRecordAPI{getRecord: cf.DNSRecord{ID: dnsFakeRecordID, Name: dnsRecordName,
				Type: dnsRecordType, Content: dnsRecordIP, TTL: 300, Proxied: new(false), Tags: []string{}}}
			r := reconcilerWithFakeDNSAPI(fake)
			expectDriftCheck(func() (reconcile.Result, error) {
				return r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			})
			Expect(fake.updateCalled).To(BeFalse())
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
			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, Name: dnsRecordName, Type: dnsRecordType,
				Content: dnsRecordIP, TTL: 1, Proxied: new(false), Tags: []string{}}
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
			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, Name: dnsRecordName, Type: dnsRecordType, Content: "9.9.9.9", TTL: 300}
			updated := cf.DNSRecord{ID: dnsFakeRecordID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300}
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

			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 1}
			updated := cf.DNSRecord{ID: dnsFakeRecordID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300}
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

			// Create record with proxied=true, and so an automatic TTL: the
			// only one Cloudflare keeps on a proxied record.
			proxied := true
			rec := &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: dnsRecordCRName, Namespace: dnsRecordNS},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					ZoneRef: corev1.LocalObjectReference{Name: dnsZoneCRName},
					Name:    dnsRecordName,
					Type:    dnsRecordType,
					Content: dnsRecordIP,
					TTL:     1,
					Proxied: &proxied,
				},
			}
			Expect(k8sClient.Create(ctx, rec)).To(Succeed())
			setRecordIDInStatus(dnsFakeRecordID)
			Expect(k8sClient.Get(ctx, recordKey, rec)).To(Succeed())
			rec.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, rec)).To(Succeed())

			cfProxied := false
			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 1, Proxied: &cfProxied}
			fake := &fakeDNSRecordAPI{getRecord: cfRecord, updateRecord: cf.DNSRecord{ID: dnsFakeRecordID}}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.updateCalled).To(BeTrue())
			Expect(fake.updateParams.Proxied).To(Equal(new(true)))
			Expect(fake.updateParams.TTL).To(Equal(1))
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
			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, Name: "mail.example.com", Type: "MX", Content: "mail.example.com", TTL: 300, Priority: &cfPriority}
			fake := &fakeDNSRecordAPI{getRecord: cfRecord, updateRecord: cf.DNSRecord{ID: dnsFakeRecordID}}
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

			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300, Comment: "old comment"}
			fake := &fakeDNSRecordAPI{getRecord: cfRecord, updateRecord: cf.DNSRecord{ID: dnsFakeRecordID}}
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

			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300, Tags: []string{"env:staging"}}
			fake := &fakeDNSRecordAPI{getRecord: cfRecord, updateRecord: cf.DNSRecord{ID: dnsFakeRecordID}}
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
				ID:   dnsFakeRecordID,
				Name: "_http._tcp.example.com", Type: "SRV", TTL: 300,
				Data: map[string]any{"port": float64(9000), "weight": float64(1)},
			}
			fake := &fakeDNSRecordAPI{getRecord: cfRecord, updateRecord: cf.DNSRecord{ID: dnsFakeRecordID}}
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
			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, Name: "_http._tcp.example.com", Type: "SRV", Content: "cf-auto-formatted-content", TTL: 300}
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
			recreated := cf.DNSRecord{ID: "new-record-abc", Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300}
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
			cfRecord := cf.DNSRecord{ID: dnsFakeRecordID, Name: dnsRecordName, Type: dnsRecordType, Content: "9.9.9.9", TTL: 300}
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

		DescribeTable("deleting a record it adopted",
			func(annotations map[string]string, deleted bool) {
				createDNSAccount(true)
				createReadyZone()
				createDNSSecret()
				createDNSRecord(annotations)
				setRecordIDInStatus(dnsFakeRecordID)
				record := &cloudflarev1alpha1.DNSRecord{}
				Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
				record.Finalizers = []string{reconciler.Finalizer}
				Expect(k8sClient.Update(ctx, record)).To(Succeed())
				Expect(k8sClient.Delete(ctx, record)).To(Succeed())

				fake := &fakeDNSRecordAPI{}
				_, err := reconcilerWithFakeDNSAPI(fake).Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
				Expect(err).NotTo(HaveOccurred())
				Expect(fake.deleteCalled).To(Equal(deleted))
				Expect(apierrors.IsNotFound(k8sClient.Get(ctx, recordKey, &cloudflarev1alpha1.DNSRecord{}))).To(BeTrue())
			},
			Entry("keeps it", map[string]string{reconciler.AdoptedAnnotation: "true"}, false),
			Entry("deletes it with deletion-policy=delete", map[string]string{
				reconciler.AdoptedAnnotation: "true", reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyDelete,
			}, true),
		)

		It("removes the finalizer without calling CF delete when deletion-policy=retain", func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(map[string]string{"kflare.dev/deletion-policy": "retain"})
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
			createDNSRecord(map[string]string{"kflare.dev/deletion-policy": "delete"})
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
			createDNSRecord(map[string]string{"kflare.dev/deletion-policy": "delete"})
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
			createDNSRecord(map[string]string{"kflare.dev/deletion-policy": "delete"})
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
			srvData := map[string]any{"port": float64(8080), "weight": float64(1)}
			created := cf.DNSRecord{ID: dnsFakeRecordID, Name: "_http._tcp.example.com", Type: "SRV", TTL: 300, Data: srvData}
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
			createDNSRecord(map[string]string{"kflare.dev/deletion-policy": "delete"})
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
			createDNSRecord(map[string]string{"kflare.dev/deletion-policy": "delete"})
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
			createDNSRecord(map[string]string{"kflare.dev/deletion-policy": "delete"})
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
			createDNSRecord(map[string]string{"kflare.dev/deletion-policy": "delete"})
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

			created := cf.DNSRecord{ID: dnsFakeRecordID, Name: dnsRecordName, Type: dnsRecordType, Content: dnsRecordIP, TTL: 300}
			fake := &fakeDNSRecordAPI{listRecords: []cf.DNSRecord{}, createRecord: created}
			r := reconcilerWithFakeDNSAPI(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Describe("pausing", func() {
		// inSync is the record as Cloudflare holds it after a sync of the spec
		// createDNSRecord writes.
		inSync := cf.DNSRecord{ID: dnsFakeRecordID, Name: dnsRecordName, Type: dnsRecordType,
			Content: dnsRecordIP, TTL: 300, Proxied: new(false), Tags: []string{}}
		var (
			fake   *fakeDNSRecordAPI
			r      *DNSRecordReconciler
			builds int
		)
		get := func() *cloudflarev1alpha1.DNSRecord {
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			return record
		}
		reconcileRecord := func() {
			GinkgoHelper()
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeZero())
		}

		// Each spec starts from a record in sync with Cloudflare, then paused.
		BeforeEach(func() {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			createDNSRecord(nil)
			setRecordIDInStatus(dnsFakeRecordID)
			record := get()
			record.Finalizers = []string{reconciler.Finalizer}
			Expect(k8sClient.Update(ctx, record)).To(Succeed())
			fake = &fakeDNSRecordAPI{getRecord: inSync}
			r = &DNSRecordReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
				NewDNSRecordAPI: func(string) (DNSRecordAPI, error) {
					builds++
					return fake, nil
				}}
			reconcileRecord()
			Expect(getRecordCondition(cloudflarev1alpha1.ConditionReady).Reason).To(Equal("Synced"))
			setPaused(ctx, get(), true)
			builds = 0
		})

		It("makes no Cloudflare call while paused, its spec changed included, and applies the spec once unpaused",
			func() {
				observed := getRecordCondition(cloudflarev1alpha1.ConditionReady).ObservedGeneration
				record := get()
				record.Spec.Content = "198.51.100.7"
				Expect(k8sClient.Update(ctx, record)).To(Succeed())
				reconcileRecord()
				Expect(builds).To(BeZero(), "a paused record built a Cloudflare client")
				cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
				Expect(cond.Status).To(Equal(metav1.ConditionTrue))
				Expect(cond.Reason).To(Equal(reasonPaused))
				Expect(cond.ObservedGeneration).To(Equal(observed))

				By("writing the new content once unpaused")
				written := inSync
				written.Content = "198.51.100.7"
				fake.updateRecord = written
				setPaused(ctx, get(), false)
				reconcileRecord()
				params, updated := fake.lastUpdate()
				Expect(updated).To(BeTrue())
				Expect(params.Content).To(Equal("198.51.100.7"))
				cond = getRecordCondition(cloudflarev1alpha1.ConditionReady)
				Expect(cond.Reason).To(Equal("Synced"))
				Expect(cond.ObservedGeneration).To(Equal(get().Generation))
			})

		It("keeps a paused record, and its finalizer, when it is deleted, and deletes it once unpaused", func() {
			Expect(k8sClient.Delete(ctx, get())).To(Succeed())
			reconcileRecord()
			Expect(builds).To(BeZero())
			Expect(fake.deleteCalled).To(BeFalse())
			Expect(get().Finalizers).To(ContainElement(reconciler.Finalizer))
			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond.Reason).To(Equal(reasonPaused))
			Expect(cond.Message).To(Equal(pausedDeletionMessage))

			By("deleting the record once unpaused")
			setPaused(ctx, get(), false)
			reconcileRecord()
			Expect(fake.deleteCalled).To(BeTrue())
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, recordKey, &cloudflarev1alpha1.DNSRecord{}))).To(BeTrue())
		})
	})

	Describe("with a tunnelRef", func() {
		const (
			tunnelCRName = "dns-test-tunnel"
			appHostname  = "app.example.com"
			oldTunnelID  = "6f1d2a7e-0000-4000-8000-000000000001"
			newTunnelID  = "6f1d2a7e-0000-4000-8000-000000000002"
		)
		tunnelKey := types.NamespacedName{Name: tunnelCRName, Namespace: dnsRecordNS}
		// target is the hostname a record pointing at tunnelID has as content.
		target := func(tunnelID string) string { return tunnelID + ".cfargotunnel.com" }

		// setTunnelID reports the Tunnel ready, with tunnelID.
		setTunnelID := func(tunnelID string) {
			tunnel := &cloudflarev1alpha1.Tunnel{}
			Expect(k8sClient.Get(ctx, tunnelKey, tunnel)).To(Succeed())
			tunnel.Status.CloudflareMetadata.TunnelID = tunnelID
			tunnel.Status.Conditions = []metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Synced",
				ObservedGeneration: tunnel.Generation, LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, tunnel)).To(Succeed())
		}
		// createTunnel creates the Tunnel the record points at, using the
		// CloudflareAccount account; with a tunnelID, it is ready with it.
		createTunnel := func(account, tunnelID string) {
			Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.Tunnel{
				ObjectMeta: metav1.ObjectMeta{Name: tunnelCRName, Namespace: dnsRecordNS},
				Spec: cloudflarev1alpha1.TunnelSpec{
					Name:                 "app",
					AccountRef:           corev1.LocalObjectReference{Name: account},
					CredentialsSecretRef: cloudflarev1alpha1.TunnelCredentialsSecretReference{Name: "app-tunnel-token"},
				},
			})).To(Succeed())
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.Tunnel{}, tunnelKey)
			if tunnelID != "" {
				setTunnelID(tunnelID)
			}
		}
		// createTunnelRecord creates the account, the Zone, the token Secret,
		// and a DNSRecord with the finalizer that points at the Tunnel.
		createTunnelRecord := func(annotations map[string]string) {
			createDNSAccount(true)
			createReadyZone()
			createDNSSecret()
			Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: dnsRecordCRName, Namespace: dnsRecordNS,
					Annotations: annotations, Finalizers: []string{reconciler.Finalizer}},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					ZoneRef:   corev1.LocalObjectReference{Name: dnsZoneCRName},
					Name:      appHostname,
					Type:      "CNAME",
					TunnelRef: &corev1.LocalObjectReference{Name: tunnelCRName},
				},
			})).To(Succeed())
		}
		// inCloudflare is the record id as Cloudflare reports it once it
		// points at tunnelID.
		inCloudflare := func(id, tunnelID string) cf.DNSRecord {
			return cf.DNSRecord{ID: id, Type: "CNAME", Name: appHostname, Content: target(tunnelID), TTL: 1,
				Proxied: new(true), Proxiable: true, Tags: []string{}}
		}
		reconcileWith := func(fake *fakeDNSRecordAPI) error {
			_, err := reconcilerWithFakeDNSAPI(fake).Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			return err
		}
		getRecord := func() *cloudflarev1alpha1.DNSRecord {
			record := &cloudflarev1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
			return record
		}
		// expectWaiting reconciles the record and checks that it reports
		// reason, without a Cloudflare client or a requeue: the Tunnel watch
		// re-triggers it.
		expectWaiting := func(reason string) *metav1.Condition {
			built := false
			r := &DNSRecordReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
				NewDNSRecordAPI: func(string) (DNSRecordAPI, error) { built = true; return &fakeDNSRecordAPI{}, nil }}
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: recordKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(built).To(BeFalse(), "no Cloudflare client while the record waits")
			cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(reason), cond.Message)
			return cond
		}

		It("waits for the Tunnel while it does not exist", func() {
			createTunnelRecord(nil)
			Expect(expectWaiting("TunnelNotFound").Message).To(ContainSubstring(tunnelCRName))
		})

		It("waits for the Tunnel until it is ready with an ID", func() {
			createTunnelRecord(nil)
			createTunnel(dnsAccountName, "")
			expectWaiting("TunnelNotReady")
		})

		It("refuses a Tunnel that uses another CloudflareAccount than the Zone", func() {
			createTunnelRecord(nil)
			createTunnel("other-account", oldTunnelID)
			message := expectWaiting("AccountMismatch").Message
			Expect(message).To(ContainSubstring(`"other-account"`))
			Expect(message).To(ContainSubstring(`"` + dnsAccountName + `"`))
		})

		It("creates a proxied CNAME to the tunnel's hostname", func() {
			createTunnelRecord(nil)
			createTunnel(dnsAccountName, oldTunnelID)
			fake := &fakeDNSRecordAPI{createRecord: inCloudflare(dnsFakeRecordID, oldTunnelID)}
			Expect(reconcileWith(fake)).To(Succeed())
			Expect(fake.createCalled).To(BeTrue())
			Expect(fake.createParams.Type).To(Equal("CNAME"))
			Expect(fake.createParams.Name).To(Equal(appHostname))
			Expect(fake.createParams.Content).To(Equal(target(oldTunnelID)))
			Expect(fake.createParams.Proxied).To(Equal(new(true)))
			Expect(fake.createParams.TTL).To(Equal(1))
			Expect(getRecordCondition(cloudflarev1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue))
			Expect(getRecord().Status.CloudflareMetadata.RecordID).To(Equal(dnsFakeRecordID))
		})

		It("leaves the record alone once it points at the tunnel", func() {
			createTunnelRecord(nil)
			createTunnel(dnsAccountName, oldTunnelID)
			setRecordIDInStatus(dnsFakeRecordID)
			fake := &fakeDNSRecordAPI{getRecord: inCloudflare(dnsFakeRecordID, oldTunnelID)}
			Expect(reconcileWith(fake)).To(Succeed())
			Expect(fake.updateCalled).To(BeFalse())
			Expect(getRecordCondition(cloudflarev1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		})

		It("follows the tunnel to its new ID once the tunnel is recreated", func() {
			createTunnelRecord(nil)
			createTunnel(dnsAccountName, newTunnelID)
			setRecordIDInStatus(dnsFakeRecordID)
			fake := &fakeDNSRecordAPI{getRecord: inCloudflare(dnsFakeRecordID, oldTunnelID),
				updateRecord: inCloudflare(dnsFakeRecordID, newTunnelID)}
			Expect(reconcileWith(fake)).To(Succeed())
			Expect(fake.updateCalled).To(BeTrue())
			Expect(fake.updateParams.ID).To(Equal(dnsFakeRecordID))
			Expect(fake.updateParams.Content).To(Equal(target(newTunnelID)))
			Expect(fake.updateParams.Proxied).To(Equal(new(true)))
		})

		It("turns proxying back on when it was turned off outside kflare", func() {
			createTunnelRecord(nil)
			createTunnel(dnsAccountName, oldTunnelID)
			setRecordIDInStatus(dnsFakeRecordID)
			unproxied := inCloudflare(dnsFakeRecordID, oldTunnelID)
			unproxied.Proxied = new(false)
			fake := &fakeDNSRecordAPI{getRecord: unproxied, updateRecord: inCloudflare(dnsFakeRecordID, oldTunnelID)}
			Expect(reconcileWith(fake)).To(Succeed())
			Expect(fake.updateCalled).To(BeTrue())
			Expect(fake.updateParams.Proxied).To(Equal(new(true)))
			Expect(fake.updateParams.Content).To(Equal(target(oldTunnelID)))
		})

		It("reports AlreadyExists for a CNAME to the tunnel it did not create, without an adoption policy", func() {
			createTunnelRecord(nil)
			createTunnel(dnsAccountName, oldTunnelID)
			fake := &fakeDNSRecordAPI{listRecords: []cf.DNSRecord{inCloudflare("existing-111", oldTunnelID)}}
			Expect(reconcileWith(fake)).To(Succeed())
			Expect(getRecordCondition(cloudflarev1alpha1.ConditionReady).Reason).To(Equal("AlreadyExists"))
			Expect(fake.createCalled).To(BeFalse())
			Expect(fake.updateCalled).To(BeFalse())
		})

		It("finds the record it was creating by the tunnel's hostname", func() {
			createTunnelRecord(map[string]string{reconciler.CreatingAnnotation: "CNAME " + appHostname})
			createTunnel(dnsAccountName, oldTunnelID)
			fake := &fakeDNSRecordAPI{listRecords: []cf.DNSRecord{inCloudflare("created-333", oldTunnelID)}}
			Expect(reconcileWith(fake)).To(Succeed())
			Expect(fake.createCalled).To(BeFalse())
			record := getRecord()
			Expect(record.Status.CloudflareMetadata.RecordID).To(Equal("created-333"))
			Expect(record.Annotations).NotTo(HaveKey(reconciler.AdoptedAnnotation))
		})

		It("follows its Tunnel to a new ID through the Tunnel watch", func() {
			createTunnelRecord(nil)
			createTunnel(dnsAccountName, oldTunnelID)
			setRecordIDInStatus(dnsFakeRecordID)
			fake := &fakeDNSRecordAPI{getRecord: inCloudflare(dnsFakeRecordID, oldTunnelID),
				updateRecord: inCloudflare(dnsFakeRecordID, newTunnelID)}

			By("running the DNSRecord controller, which finds the record in sync")
			mgr, err := manager.New(cfg, manager.Options{
				Controller: config.Controller{SkipNameValidation: new(true)},
				Metrics:    metricsserver.Options{BindAddress: "0"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect((&DNSRecordReconciler{
				Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
				NewDNSRecordAPI: func(string) (DNSRecordAPI, error) { return fake, nil },
			}).SetupWithManager(mgr)).To(Succeed())
			mgrCtx, stop := context.WithCancel(ctx)
			stopped := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				defer close(stopped)
				Expect(mgr.Start(mgrCtx)).To(Succeed())
			}()
			// Stop it before the cleanup deletes the record, which it would
			// otherwise reconcile.
			stopManager := func() {
				stop()
				Eventually(stopped).Should(BeClosed())
			}
			DeferCleanup(stopManager)
			Eventually(func(g Gomega) {
				cond := getRecordCondition(cloudflarev1alpha1.ConditionReady)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			}).WithTimeout(10 * time.Second).Should(Succeed())
			_, updated := fake.lastUpdate()
			Expect(updated).To(BeFalse())

			By("recreating the tunnel, which gives it a new ID")
			setTunnelID(newTunnelID)
			Eventually(func(g Gomega) {
				params, updated := fake.lastUpdate()
				g.Expect(updated).To(BeTrue(), "the record was not re-triggered by its Tunnel")
				g.Expect(params.Content).To(Equal(target(newTunnelID)))
			}).WithTimeout(10 * time.Second).Should(Succeed())
			stopManager()
		})
	})

	Describe("Validation of what the record points at", func() {
		// create creates a DNSRecord named name with spec in the test Zone,
		// deleted after the spec.
		create := func(name string, spec cloudflarev1alpha1.DNSRecordSpec) error {
			spec.ZoneRef = corev1.LocalObjectReference{Name: dnsZoneCRName}
			spec.Name = "app.example.com"
			record := &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: dnsRecordNS},
				Spec:       spec,
			}
			err := k8sClient.Create(ctx, record)
			if err == nil {
				DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.DNSRecord{},
					types.NamespacedName{Name: name, Namespace: dnsRecordNS})
			}
			return err
		}
		tunnelRef := &corev1.LocalObjectReference{Name: "app-tunnel"}
		srvData := &apiextensionsv1.JSON{Raw: []byte(`{"port":443,"target":"app.example.com"}`)}

		DescribeTable("refuses",
			func(spec cloudflarev1alpha1.DNSRecordSpec, message string) {
				Expect(create("refused", spec)).To(MatchError(ContainSubstring(message)))
			},
			Entry("content and a tunnelRef", cloudflarev1alpha1.DNSRecordSpec{
				Type: "CNAME", Content: "app.example.net", TunnelRef: tunnelRef},
				"set exactly one of content, data and tunnelRef"),
			Entry("data and a tunnelRef", cloudflarev1alpha1.DNSRecordSpec{
				Type: "CNAME", Data: srvData, TunnelRef: tunnelRef},
				"set exactly one of content, data and tunnelRef"),
			Entry("content and data", cloudflarev1alpha1.DNSRecordSpec{
				Type: "SRV", Content: "0 443 app.example.com", Data: srvData},
				"set exactly one of content, data and tunnelRef"),
			Entry("none of them", cloudflarev1alpha1.DNSRecordSpec{Type: "A"},
				"set exactly one of content, data and tunnelRef"),
			Entry("a tunnelRef on another type than CNAME", cloudflarev1alpha1.DNSRecordSpec{
				Type: "A", TunnelRef: tunnelRef}, "tunnelRef needs type CNAME"),
			Entry("an unproxied record with a tunnelRef", cloudflarev1alpha1.DNSRecordSpec{
				Type: "CNAME", TunnelRef: tunnelRef, Proxied: new(false)},
				"a record with a tunnelRef must be proxied"),
			Entry("a ttl on a record with a tunnelRef", cloudflarev1alpha1.DNSRecordSpec{
				Type: "CNAME", TunnelRef: tunnelRef, TTL: 300}, "a proxied record takes ttl 1"),
			Entry("a ttl on a proxied record", cloudflarev1alpha1.DNSRecordSpec{
				Type: "A", Content: "192.0.2.1", Proxied: new(true), TTL: 300}, "a proxied record takes ttl 1"),
			Entry("data that is not an object", cloudflarev1alpha1.DNSRecordSpec{
				Type: "SRV", Data: &apiextensionsv1.JSON{Raw: []byte(`"0 443 app.example.com"`)}},
				"must be of type object"),
		)

		DescribeTable("accepts",
			func(spec cloudflarev1alpha1.DNSRecordSpec) {
				Expect(create("accepted", spec)).To(Succeed())
			},
			Entry("a tunnelRef on a CNAME", cloudflarev1alpha1.DNSRecordSpec{Type: "CNAME", TunnelRef: tunnelRef}),
			Entry("a tunnelRef, proxied, with an automatic ttl", cloudflarev1alpha1.DNSRecordSpec{
				Type: "CNAME", TunnelRef: tunnelRef, Proxied: new(true), TTL: 1}),
			Entry("a ttl on an unproxied record", cloudflarev1alpha1.DNSRecordSpec{
				Type: "A", Content: "192.0.2.1", Proxied: new(false), TTL: 300}),
			Entry("data alone", cloudflarev1alpha1.DNSRecordSpec{Type: "SRV", Data: srvData}),
		)

		It("refuses empty content", func() {
			record := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": cloudflarev1alpha1.GroupVersion.String(),
				"kind":       "DNSRecord",
				"metadata":   map[string]any{"name": "empty-content", "namespace": dnsRecordNS},
				"spec": map[string]any{
					"zoneRef": map[string]any{"name": dnsZoneCRName},
					"name":    "app.example.com",
					"type":    "A",
					"content": "",
				},
			}}
			Expect(k8sClient.Create(ctx, record)).To(MatchError(ContainSubstring("spec.content")))
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
