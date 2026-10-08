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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

var _ = Describe("WorkerScript r2BucketRef bindings", func() {
	const (
		ns          = "default"
		wsName      = "wsr2-worker"
		r2Name      = "wsr2-bucket"
		bucketName  = "wsr2-assets"
		accountName = "wsr2-account"
		secretName  = "wsr2-api-token"
	)

	ctx := context.Background()
	wsKey := types.NamespacedName{Name: wsName, Namespace: ns}
	r2Key := types.NamespacedName{Name: r2Name, Namespace: ns}
	uploadedAt := time.Date(2026, 10, 7, 12, 0, 0, 123456000, time.UTC)

	var fake *fakeWorkerScriptAPI
	var r *WorkerScriptReconciler

	// createR2 creates the R2Bucket in account; when ready it is marked
	// ready, as the R2Bucket controller would after a sync.
	createR2 := func(account string, ready bool) {
		b := &cloudflarev1alpha1.R2Bucket{
			ObjectMeta: metav1.ObjectMeta{Name: r2Name, Namespace: ns},
			Spec: cloudflarev1alpha1.R2BucketSpec{
				AccountRef: corev1.LocalObjectReference{Name: account}, Name: bucketName,
			},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		if ready {
			setR2BucketCreated(ctx, r2Key, time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC))
		}
	}

	getWorker := func() *cloudflarev1alpha1.WorkerScript {
		ws := &cloudflarev1alpha1.WorkerScript{}
		Expect(k8sClient.Get(ctx, wsKey, ws)).To(Succeed())
		return ws
	}
	readyReason := func() string {
		cond := meta.FindStatusCondition(getWorker().Status.Conditions, cloudflarev1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		return cond.Reason
	}
	reconcileWorker := func() (reconcile.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
	}

	BeforeEach(func() {
		fake = &fakeWorkerScriptAPI{uploadResp: cf.WorkerScriptResponse{WorkerScript: cf.WorkerScript{
			WorkerMetaData: cf.WorkerMetaData{ID: "wsr2-script", ETAG: "etag", ModifiedOn: uploadedAt},
		}}}
		r = &WorkerScriptReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			NewWorkerScriptAPI: func(string) (WorkerScriptAPI, error) { return fake, nil },
		}

		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "acct-wsr2",
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
			},
		}
		Expect(k8sClient.Create(ctx, acct)).To(Succeed())
		acct.Status.Conditions = []metav1.Condition{{
			Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Validated", LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, acct)).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
		})).To(Succeed())

		script := "export default {}"
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.WorkerScript{
			ObjectMeta: metav1.ObjectMeta{Name: wsName, Namespace: ns, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.WorkerScriptSpec{
				Name: "wsr2-script", AccountRef: corev1.LocalObjectReference{Name: accountName}, Script: &script,
				Bindings: []cloudflarev1alpha1.WorkerBinding{
					{Name: "ASSETS", R2BucketRef: &corev1.LocalObjectReference{Name: r2Name}},
				},
			},
		})).To(Succeed())

		DeferCleanup(func() {
			forceDelete(ctx, &cloudflarev1alpha1.WorkerScript{}, wsKey)
			forceDelete(ctx, &cloudflarev1alpha1.R2Bucket{}, r2Key)
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
			forceDelete(ctx, &corev1.Secret{}, types.NamespacedName{Name: secretName, Namespace: ns})
		})
	})

	DescribeTable("waits without uploading",
		func(setup func(), reason string) {
			setup()
			result, err := reconcileWorker()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue(), "the R2Bucket watch re-triggers this reconcile")
			Expect(readyReason()).To(Equal(reason))
			Expect(fake.uploads).To(BeEmpty())
		},
		Entry("when the R2Bucket is missing", func() {}, "R2BucketNotFound"),
		Entry("when the R2Bucket is not ready", func() { createR2(accountName, false) }, "R2BucketNotReady"),
		Entry("when the R2Bucket is in another account", func() { createR2("wsr2-other-account", true) }, "AccountMismatch"),
	)

	It("binds the bucket by its Cloudflare name", func() {
		createR2(accountName, true)
		_, err := reconcileWorker()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.uploads).To(HaveLen(1))
		Expect(fake.uploads[0].Bindings).To(HaveKeyWithValue("ASSETS", cf.WorkerR2BucketBinding{BucketName: bucketName}))
		Expect(readyReason()).To(Equal("Synced"))
	})

	It("does not upload again when the bucket is recreated under the same name", func() {
		createR2(accountName, true)
		_, err := reconcileWorker()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.uploads).To(HaveLen(1))
		fake.workers = []cf.WorkerMetaData{{ID: "wsr2-script", ModifiedOn: uploadedAt}}

		setR2BucketCreated(ctx, r2Key, time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC))
		_, err = reconcileWorker()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.uploads).To(HaveLen(1))
		Expect(readyReason()).To(Equal("Synced"))
	})

	It("maps an R2Bucket to the WorkerScripts that bind it", func() {
		b := &cloudflarev1alpha1.R2Bucket{ObjectMeta: metav1.ObjectMeta{Name: r2Name, Namespace: ns}}
		Expect(r.workersForR2Bucket(ctx, b)).To(ConsistOf(reconcile.Request{NamespacedName: wsKey}))
		other := &cloudflarev1alpha1.R2Bucket{ObjectMeta: metav1.ObjectMeta{Name: "unbound", Namespace: ns}}
		Expect(r.workersForR2Bucket(ctx, other)).To(BeEmpty())
	})
})

// setR2BucketCreated marks the R2Bucket at key ready with a bucket created at
// created, as the R2Bucket controller reports it.
func setR2BucketCreated(ctx context.Context, key types.NamespacedName, created time.Time) {
	b := &cloudflarev1alpha1.R2Bucket{}
	Expect(k8sClient.Get(ctx, key, b)).To(Succeed())
	createdOn := metav1.NewTime(created)
	b.Status.CloudflareMetadata = cloudflarev1alpha1.R2BucketCloudflareMetadata{Location: "WEUR", CreationDate: &createdOn}
	b.Status.Conditions = []metav1.Condition{{
		Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
		Reason: "Synced", LastTransitionTime: metav1.Now(),
	}}
	Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
}
