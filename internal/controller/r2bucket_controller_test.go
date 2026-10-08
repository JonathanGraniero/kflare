/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// fakeR2BucketAPI is an in-memory R2 buckets API for one account. It follows
// the behavior verified against the live API: names are unique in an account
// (a duplicate create is a 409, code 10004), a missing bucket is a 404 (code
// 10006) for reads and deletes alike, and deleting a bucket that still holds
// objects is a 409 (code 10008). Without a location hint Cloudflare picks a
// location itself. Like cloudflare-go, it rejects a resource container
// without an account ID.
type fakeR2BucketAPI struct {
	buckets map[string]cf.R2Bucket
	// objects counts the objects each bucket holds.
	objects map[string]int
	// created records every successful create, in order.
	created []cf.CreateR2BucketParameters
	clock   time.Time

	getErr, createErr, deleteErr error

	accountIDs []string
	writes     int
}

func newFakeR2BucketAPI(existing ...cf.R2Bucket) *fakeR2BucketAPI {
	f := &fakeR2BucketAPI{
		buckets: map[string]cf.R2Bucket{},
		objects: map[string]int{},
		clock:   time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC),
	}
	for _, b := range existing {
		f.buckets[b.Name] = b
	}
	return f
}

// r2Error builds the error cloudflare-go returns for an R2 API failure with
// HTTP status and Cloudflare error code.
func r2Error(status, code int) error {
	e := &cf.Error{StatusCode: status, ErrorCodes: []int{code}}
	if status == 404 {
		e.Type = cf.ErrorTypeNotFound
		v := cf.NewNotFoundError(e)
		return &v
	}
	e.Type = cf.ErrorTypeRequest
	v := cf.NewRequestError(e)
	return &v
}

func (f *fakeR2BucketAPI) record(rc *cf.ResourceContainer) error {
	f.accountIDs = append(f.accountIDs, rc.Identifier)
	if rc.Identifier == "" {
		return cf.ErrMissingAccountID
	}
	return nil
}

func (f *fakeR2BucketAPI) GetR2Bucket(_ context.Context, rc *cf.ResourceContainer, bucketName string) (cf.R2Bucket, error) {
	if err := f.record(rc); err != nil {
		return cf.R2Bucket{}, err
	}
	if f.getErr != nil {
		return cf.R2Bucket{}, f.getErr
	}
	b, ok := f.buckets[bucketName]
	if !ok {
		return cf.R2Bucket{}, r2Error(404, 10006)
	}
	return b, nil
}

func (f *fakeR2BucketAPI) CreateR2Bucket(_ context.Context, rc *cf.ResourceContainer, params cf.CreateR2BucketParameters) (cf.R2Bucket, error) {
	if err := f.record(rc); err != nil {
		return cf.R2Bucket{}, err
	}
	f.writes++
	if f.createErr != nil {
		return cf.R2Bucket{}, f.createErr
	}
	if _, ok := f.buckets[params.Name]; ok {
		return cf.R2Bucket{}, r2Error(409, 10004)
	}
	location := "ENAM"
	if params.LocationHint != "" {
		location = strings.ToUpper(params.LocationHint)
	}
	f.clock = f.clock.Add(time.Hour)
	created := f.clock
	b := cf.R2Bucket{Name: params.Name, Location: location, CreationDate: &created}
	f.buckets[b.Name] = b
	f.created = append(f.created, params)
	return b, nil
}

func (f *fakeR2BucketAPI) DeleteR2Bucket(_ context.Context, rc *cf.ResourceContainer, bucketName string) error {
	if err := f.record(rc); err != nil {
		return err
	}
	f.writes++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.buckets[bucketName]; !ok {
		return r2Error(404, 10006)
	}
	if f.objects[bucketName] > 0 {
		return r2Error(409, r2BucketNotEmptyCode)
	}
	delete(f.buckets, bucketName)
	return nil
}

var _ = Describe("R2Bucket Controller", func() {
	const (
		ns           = "default"
		r2Name       = "r2-test-bucket"
		otherR2      = "r2-test-bucket-other"
		accountName  = "r2-test-account"
		otherAccount = "r2-test-account-other"
		secretName   = "r2-test-api-token"
		fakeAcctID   = "acct-r2-123"
		bucketName   = "r2-test-assets"
	)

	ctx := context.Background()
	r2Key := types.NamespacedName{Name: r2Name, Namespace: ns}

	var fake *fakeR2BucketAPI
	var r *R2BucketReconciler

	// createAccountFor creates a CloudflareAccount named name for the
	// Cloudflare account accountID.
	createAccountFor := func(name, accountID string, ready bool) {
		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      accountID,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
			},
		}
		Expect(k8sClient.Create(ctx, acct)).To(Succeed())
		if ready {
			acct.Status.Conditions = []metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Validated", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, acct)).To(Succeed())
		}
	}

	createAccount := func(ready bool) {
		createAccountFor(accountName, fakeAcctID, ready)
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
		})).To(Succeed())
	}

	// createR2 creates an R2Bucket that already carries the finalizer.
	createR2 := func(name, account string, annotations map[string]string) {
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.R2Bucket{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns, Annotations: annotations,
				Finalizers: []string{reconciler.Finalizer},
			},
			Spec: cloudflarev1alpha1.R2BucketSpec{
				AccountRef: corev1.LocalObjectReference{Name: account}, Name: bucketName, LocationHint: "weur",
			},
		})).To(Succeed())
	}

	// claimAs makes the R2Bucket name claim bucketName, as its controller
	// would after creating or adopting it.
	claimAs := func(name string) {
		other := &cloudflarev1alpha1.R2Bucket{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, other)).To(Succeed())
		other.Labels = map[string]string{cloudflarev1alpha1.R2BucketNameLabel: bucketName}
		Expect(k8sClient.Update(ctx, other)).To(Succeed())
	}

	getR2 := func() *cloudflarev1alpha1.R2Bucket {
		b := &cloudflarev1alpha1.R2Bucket{}
		Expect(k8sClient.Get(ctx, r2Key, b)).To(Succeed())
		return b
	}

	readyCondition := func() metav1.Condition {
		cond := meta.FindStatusCondition(getR2().Status.Conditions, cloudflarev1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		return *cond
	}

	reconcileR2 := func() (ctrl.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: r2Key})
	}

	BeforeEach(func() {
		fake = newFakeR2BucketAPI()
		r = &R2BucketReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			NewR2BucketAPI: func(string) (R2BucketAPI, error) {
				return fake, nil
			},
		}
	})

	AfterEach(func() {
		forceDelete(ctx, &cloudflarev1alpha1.R2Bucket{}, r2Key)
		forceDelete(ctx, &cloudflarev1alpha1.R2Bucket{}, types.NamespacedName{Name: otherR2, Namespace: ns})
		forceDelete(ctx, &cloudflarev1alpha1.WorkerScript{}, types.NamespacedName{Name: "r2-test-worker", Namespace: ns})
		forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
		forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: otherAccount})
		forceDelete(ctx, &corev1.Secret{}, types.NamespacedName{Name: secretName, Namespace: ns})
	})

	Describe("Reconcile", func() {
		It("returns no error when the R2Bucket does not exist", func() {
			_, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
		})

		It("adds the finalizer on the first reconcile without calling Cloudflare", func() {
			Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.R2Bucket{
				ObjectMeta: metav1.ObjectMeta{Name: r2Name, Namespace: ns},
				Spec: cloudflarev1alpha1.R2BucketSpec{
					AccountRef: corev1.LocalObjectReference{Name: accountName}, Name: bucketName,
				},
			})).To(Succeed())
			_, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(getR2().Finalizers).To(ContainElement(reconciler.Finalizer))
			Expect(fake.accountIDs).To(BeEmpty())
		})

		It("retries after a minute when the account is not ready", func() {
			createAccount(false)
			createR2(r2Name, accountName, nil)
			result, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(readyCondition().Reason).To(Equal("AccountNotReady"))
		})

		It("reports InvalidToken when no Cloudflare client can be built", func() {
			createAccount(true)
			createR2(r2Name, accountName, nil)
			r.NewR2BucketAPI = func(string) (R2BucketAPI, error) { return nil, errors.New("bad token") }
			_, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(readyCondition().Reason).To(Equal("InvalidToken"))
		})

		It("creates the bucket with the location hint, claims it and reports where it is", func() {
			createAccount(true)
			createR2(r2Name, accountName, nil)
			_, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())

			Expect(fake.created).To(Equal([]cf.CreateR2BucketParameters{{Name: bucketName, LocationHint: "weur"}}))
			Expect(fake.accountIDs).To(HaveEach(fakeAcctID))
			b := getR2()
			Expect(b.Labels).To(HaveKeyWithValue(cloudflarev1alpha1.R2BucketNameLabel, bucketName))
			Expect(b.Status.CloudflareMetadata.Location).To(Equal("WEUR"))
			Expect(b.Status.CloudflareMetadata.CreationDate).NotTo(BeNil())
			Expect(b.Status.CloudflareMetadata.CreationDate.Time).To(BeTemporally("==", *fake.buckets[bucketName].CreationDate))
			Expect(readyCondition().Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCondition().Reason).To(Equal("Synced"))
		})

		It("writes nothing to Cloudflare once the bucket exists", func() {
			createAccount(true)
			createR2(r2Name, accountName, nil)
			_, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			writes := fake.writes

			_, err = reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.writes).To(Equal(writes))
		})

		It("adopts the existing bucket with its name and keeps its location", func() {
			createAccount(true)
			fake = newFakeR2BucketAPI(cf.R2Bucket{Name: bucketName, Location: "APAC"})
			createR2(r2Name, accountName, nil)
			_, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.writes).To(BeZero())
			b := getR2()
			Expect(b.Labels).To(HaveKeyWithValue(cloudflarev1alpha1.R2BucketNameLabel, bucketName))
			Expect(b.Status.CloudflareMetadata.Location).To(Equal("APAC"))
			Expect(b.Status.CloudflareMetadata.CreationDate).To(BeNil())
			Expect(readyCondition().Status).To(Equal(metav1.ConditionTrue))
		})

		It("reports NameConflict instead of taking over a bucket another R2Bucket manages", func() {
			createAccount(true)
			fake = newFakeR2BucketAPI(cf.R2Bucket{Name: bucketName})
			createR2(otherR2, accountName, nil)
			claimAs(otherR2)

			createR2(r2Name, accountName, nil)
			result, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(readyCondition().Reason).To(Equal("NameConflict"))
			Expect(readyCondition().Message).To(ContainSubstring(ns + "/" + otherR2))
			Expect(getR2().Labels).NotTo(HaveKey(cloudflarev1alpha1.R2BucketNameLabel))
			Expect(fake.writes).To(BeZero())
		})

		It("does not create a missing bucket that another R2Bucket manages", func() {
			createAccount(true)
			createR2(otherR2, accountName, nil)
			claimAs(otherR2)

			createR2(r2Name, accountName, nil)
			_, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(readyCondition().Reason).To(Equal("NameConflict"))
			Expect(fake.created).To(BeEmpty())
		})

		It("reports NameConflict when the claim comes through another CloudflareAccount for the same account", func() {
			createAccount(true)
			createAccountFor(otherAccount, fakeAcctID, true)
			fake = newFakeR2BucketAPI(cf.R2Bucket{Name: bucketName})
			createR2(otherR2, otherAccount, nil)
			claimAs(otherR2)

			createR2(r2Name, accountName, nil)
			_, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(readyCondition().Reason).To(Equal("NameConflict"))
		})

		DescribeTable("ignores claims on the same name that cannot be in this account",
			func(setup func()) {
				createAccount(true)
				setup()
				createR2(otherR2, otherAccount, nil)
				claimAs(otherR2)

				createR2(r2Name, accountName, nil)
				_, err := reconcileR2()
				Expect(err).NotTo(HaveOccurred())
				Expect(readyCondition().Status).To(Equal(metav1.ConditionTrue))
				Expect(fake.created).To(HaveLen(1))
			},
			Entry("a claim in another Cloudflare account", func() { createAccountFor(otherAccount, "acct-r2-other", true) }),
			Entry("a claim whose CloudflareAccount does not exist", func() {}),
		)

		It("recreates the bucket when it was deleted outside kflare", func() {
			createAccount(true)
			createR2(r2Name, accountName, nil)
			_, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			firstCreated := getR2().Status.CloudflareMetadata.CreationDate.DeepCopy()
			delete(fake.buckets, bucketName)

			_, err = reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.created).To(HaveLen(2))
			Expect(getR2().Status.CloudflareMetadata.CreationDate.After(firstCreated.Time)).To(BeTrue())
			Expect(readyCondition().Status).To(Equal(metav1.ConditionTrue))
		})

		It("sets TerminalError when Cloudflare rejects the bucket", func() {
			createAccount(true)
			createR2(r2Name, accountName, nil)
			fake.createErr = r2Error(400, 10005)
			result, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(readyCondition().Reason).To(Equal("TerminalError"))
			Expect(getR2().Labels).NotTo(HaveKey(cloudflarev1alpha1.R2BucketNameLabel))
		})

		It("returns retryable errors for back-off", func() {
			createAccount(true)
			createR2(r2Name, accountName, nil)
			fake.getErr = errors.New("connection reset")
			_, err := reconcileR2()
			Expect(err).To(MatchError("connection reset"))
			Expect(readyCondition().Reason).To(Equal("APIError"))

			fake.getErr = nil
			fake.createErr = errors.New("unavailable")
			_, err = reconcileR2()
			Expect(err).To(MatchError("unavailable"))
		})

		It("returns the error when the claims cannot be listed", func() {
			createAccount(true)
			createR2(r2Name, accountName, nil)
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			_, err := r.bucketClaimant(cancelled, getR2(), fakeAcctID)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("deletion", func() {
		syncedR2 := func(annotations map[string]string) {
			createAccount(true)
			createR2(r2Name, accountName, annotations)
			_, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.buckets).To(HaveKey(bucketName))
		}
		startDeletion := func() {
			Expect(k8sClient.Delete(ctx, getR2())).To(Succeed())
		}
		expectGone := func() {
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, r2Key, &cloudflarev1alpha1.R2Bucket{}))).To(BeTrue())
		}

		It("deletes the bucket from Cloudflare and removes the finalizer", func() {
			syncedR2(nil)
			startDeletion()
			_, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.buckets).To(BeEmpty())
			expectGone()
		})

		It("keeps the bucket with the retain policy", func() {
			syncedR2(map[string]string{reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain})
			startDeletion()
			_, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.buckets).To(HaveKey(bucketName))
			expectGone()
		})

		It("finishes when the bucket is already gone from Cloudflare", func() {
			syncedR2(nil)
			delete(fake.buckets, bucketName)
			startDeletion()
			_, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			expectGone()
		})

		It("leaves alone a bucket it never claimed", func() {
			createAccount(true)
			fake = newFakeR2BucketAPI(cf.R2Bucket{Name: bucketName})
			createR2(otherR2, accountName, nil)
			claimAs(otherR2)
			createR2(r2Name, accountName, nil)
			_, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(readyCondition().Reason).To(Equal("NameConflict"))
			fake.accountIDs = nil

			startDeletion()
			_, err = reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.accountIDs).To(BeEmpty())
			Expect(fake.buckets).To(HaveKey(bucketName))
			expectGone()
		})

		It("reports BucketNotEmpty and keeps checking until the bucket is emptied", func() {
			syncedR2(nil)
			fake.objects[bucketName] = 2
			startDeletion()

			result, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(readyCondition().Reason).To(Equal("BucketNotEmpty"))
			Expect(readyCondition().Message).To(ContainSubstring(reconciler.DeletionPolicyAnnotation))
			Expect(getR2().Finalizers).To(ContainElement(reconciler.Finalizer))
			Expect(fake.buckets).To(HaveKey(bucketName))

			fake.objects[bucketName] = 0
			_, err = reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.buckets).To(BeEmpty())
			expectGone()
		})

		It("lets a bucket that is not empty go once the retain policy is set", func() {
			syncedR2(nil)
			fake.objects[bucketName] = 1
			startDeletion()
			_, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(readyCondition().Reason).To(Equal("BucketNotEmpty"))

			b := getR2()
			b.Annotations = map[string]string{reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain}
			Expect(k8sClient.Update(ctx, b)).To(Succeed())
			_, err = reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.buckets).To(HaveKey(bucketName))
			expectGone()
		})

		It("waits for the WorkerScripts bound to it, then deletes", func() {
			syncedR2(nil)
			script := "export default {}"
			Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.WorkerScript{
				ObjectMeta: metav1.ObjectMeta{Name: "r2-test-worker", Namespace: ns, Finalizers: []string{reconciler.Finalizer}},
				Spec: cloudflarev1alpha1.WorkerScriptSpec{
					Name: "r2-test-script", AccountRef: corev1.LocalObjectReference{Name: accountName}, Script: &script,
					Bindings: []cloudflarev1alpha1.WorkerBinding{
						{Name: "BUCKET", R2BucketRef: &corev1.LocalObjectReference{Name: r2Name}},
					},
				},
			})).To(Succeed())
			startDeletion()

			_, err := reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(readyCondition().Reason).To(Equal("InUse"))
			Expect(readyCondition().Message).To(ContainSubstring("WorkerScript default/r2-test-worker"))
			Expect(fake.buckets).To(HaveKey(bucketName))

			forceDelete(ctx, &cloudflarev1alpha1.WorkerScript{}, types.NamespacedName{Name: "r2-test-worker", Namespace: ns})
			_, err = reconcileR2()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.buckets).To(BeEmpty())
			expectGone()
		})

		It("keeps the finalizer when Cloudflare fails, so deletion is retried", func() {
			syncedR2(nil)
			fake.deleteErr = errors.New("unavailable")
			startDeletion()
			_, err := reconcileR2()
			Expect(err).To(MatchError("unavailable"))
			Expect(getR2().Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("keeps the finalizer when the credentials cannot be resolved", func() {
			syncedR2(nil)
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
			startDeletion()
			_, err := reconcileR2()
			Expect(err).To(HaveOccurred())
			Expect(getR2().Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("returns the error when no Cloudflare client can be built", func() {
			syncedR2(nil)
			r.NewR2BucketAPI = func(string) (R2BucketAPI, error) { return nil, errors.New("bad token") }
			startDeletion()
			_, err := reconcileR2()
			Expect(err).To(MatchError("bad token"))
		})

		It("does nothing for a bucket without the finalizer", func() {
			result, err := r.reconcileDelete(ctx, &cloudflarev1alpha1.R2Bucket{})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
		})
	})

	Describe("Validation", func() {
		BeforeEach(func() { createR2(r2Name, accountName, nil) })

		update := func(mutate func(*cloudflarev1alpha1.R2Bucket)) error {
			b := getR2()
			mutate(b)
			return k8sClient.Update(ctx, b)
		}

		DescribeTable("rejects changes to immutable fields",
			func(mutate func(*cloudflarev1alpha1.R2Bucket), message string) {
				Expect(update(mutate)).To(MatchError(ContainSubstring(message)))
			},
			Entry("spec.accountRef", func(b *cloudflarev1alpha1.R2Bucket) { b.Spec.AccountRef.Name = "other" }, "accountRef is immutable"),
			Entry("spec.name", func(b *cloudflarev1alpha1.R2Bucket) { b.Spec.Name = "r2-test-renamed" }, "name is immutable"),
			Entry("spec.locationHint", func(b *cloudflarev1alpha1.R2Bucket) { b.Spec.LocationHint = "apac" }, "locationHint is immutable"),
			Entry("removing spec.locationHint", func(b *cloudflarev1alpha1.R2Bucket) { b.Spec.LocationHint = "" }, "locationHint is immutable"),
		)

		It("rejects adding a location hint to a bucket created without one", func() {
			Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.R2Bucket{
				ObjectMeta: metav1.ObjectMeta{Name: otherR2, Namespace: ns},
				Spec: cloudflarev1alpha1.R2BucketSpec{
					AccountRef: corev1.LocalObjectReference{Name: accountName}, Name: bucketName,
				},
			})).To(Succeed())
			other := &cloudflarev1alpha1.R2Bucket{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: otherR2, Namespace: ns}, other)).To(Succeed())
			other.Spec.LocationHint = "weur"
			Expect(k8sClient.Update(ctx, other)).To(MatchError(ContainSubstring("locationHint is immutable")))
		})

		DescribeTable("rejects invalid values on create",
			func(spec cloudflarev1alpha1.R2BucketSpec, field string) {
				spec.AccountRef = corev1.LocalObjectReference{Name: accountName}
				err := k8sClient.Create(ctx, &cloudflarev1alpha1.R2Bucket{
					ObjectMeta: metav1.ObjectMeta{Name: otherR2, Namespace: ns}, Spec: spec,
				})
				Expect(err).To(MatchError(ContainSubstring(field)))
			},
			Entry("an uppercase name", cloudflarev1alpha1.R2BucketSpec{Name: "Assets"}, "spec.name"),
			Entry("a name shorter than 3 characters", cloudflarev1alpha1.R2BucketSpec{Name: "ab"}, "spec.name"),
			Entry("a name ending in a hyphen", cloudflarev1alpha1.R2BucketSpec{Name: "assets-"}, "spec.name"),
			Entry("a name with an underscore", cloudflarev1alpha1.R2BucketSpec{Name: "my_assets"}, "spec.name"),
			Entry("an unknown location hint", cloudflarev1alpha1.R2BucketSpec{Name: "assets", LocationHint: "mars"}, "spec.locationHint"),
		)
	})
})
