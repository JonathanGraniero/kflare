/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	"github.com/cloudflare/cloudflare-go/v7/r2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// R2Bucket is generated, and so are most of its specs
// (zz_generated.r2bucket_test.go). These are the ones the generated specs
// cannot write: a bucket waits for the WorkerScripts bound to it.
var _ = Describe("R2Bucket and the WorkerScripts bound to it", func() {
	const (
		ns          = "default"
		r2Name      = "r2-bound-bucket"
		workerName  = "r2-bound-worker"
		accountName = "r2-bound-account"
		secretName  = "r2-bound-token"
		accountID   = "acct-r2-bound"
	)
	ctx := context.Background()
	r2Key := types.NamespacedName{Name: r2Name, Namespace: ns}
	workerKey := types.NamespacedName{Name: workerName, Namespace: ns}

	var store *fakeCRUDStore[r2.Bucket]
	var r *R2BucketReconciler

	// bind creates a WorkerScript that binds the R2Bucket r2Name, and the one
	// named other.
	bind := func(other string) {
		script := "export default {}"
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.WorkerScript{
			ObjectMeta: metav1.ObjectMeta{Name: workerName, Namespace: ns, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.WorkerScriptSpec{
				Name: "r2-bound-script", AccountRef: corev1.LocalObjectReference{Name: accountName}, Script: &script,
				Bindings: []cloudflarev1alpha1.WorkerBinding{
					{Name: "ASSETS", R2BucketRef: &corev1.LocalObjectReference{Name: r2Name}},
					{Name: "OTHER", R2BucketRef: &corev1.LocalObjectReference{Name: other}},
				},
			},
		})).To(Succeed())
	}

	BeforeEach(func() {
		store = newFakeCRUDStore[r2.Bucket](0, "default")
		r = &R2BucketReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			NewR2BucketAPI: func(string) (R2BucketAPI, error) { return fakeR2BucketAPI{store}, nil },
		}
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      accountID,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
			},
		})).To(Succeed())
		account := &cloudflarev1alpha1.CloudflareAccount{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: accountName}, account)).To(Succeed())
		account.Status.Conditions = []metav1.Condition{{
			Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Validated", LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, account)).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.R2Bucket{
			ObjectMeta: metav1.ObjectMeta{Name: r2Name, Namespace: ns, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.R2BucketSpec{
				AccountRef: corev1.LocalObjectReference{Name: accountName}, Name: "r2-bound-bucket",
			},
		})).To(Succeed())

		DeferCleanup(func() {
			forceDelete(ctx, &cloudflarev1alpha1.R2Bucket{}, r2Key)
			forceDelete(ctx, &cloudflarev1alpha1.WorkerScript{}, workerKey)
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
			forceDelete(ctx, &corev1.Secret{}, types.NamespacedName{Name: secretName, Namespace: ns})
		})
	})

	It("waits for the WorkerScripts bound to it before it is deleted", func() {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: r2Key})
		Expect(err).NotTo(HaveOccurred())
		Expect(store.objects).To(HaveLen(1))
		bind("r2-bound-missing")
		obj := &cloudflarev1alpha1.R2Bucket{}
		Expect(k8sClient.Get(ctx, r2Key, obj)).To(Succeed())
		Expect(k8sClient.Delete(ctx, obj)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: r2Key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, r2Key, obj)).To(Succeed())
		ready := meta.FindStatusCondition(obj.Status.Conditions, cloudflarev1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal("InUse"))
		Expect(ready.Message).To(ContainSubstring("WorkerScript " + workerKey.String()))
		Expect(store.objects).To(HaveLen(1))

		forceDelete(ctx, &cloudflarev1alpha1.WorkerScript{}, workerKey)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: r2Key})
		Expect(err).NotTo(HaveOccurred())
		Expect(store.objects).To(BeEmpty())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, r2Key, &cloudflarev1alpha1.R2Bucket{}))).To(BeTrue())
	})

	It("is re-triggered by the deletion of a WorkerScript bound to it, once it is being deleted", func() {
		bind("r2-bound-missing")
		worker := &cloudflarev1alpha1.WorkerScript{}
		Expect(k8sClient.Get(ctx, workerKey, worker)).To(Succeed())
		Expect(r.crud().deletingResourcesOf(ctx, worker)).To(BeEmpty(), "the bucket is not being deleted yet")

		obj := &cloudflarev1alpha1.R2Bucket{}
		Expect(k8sClient.Get(ctx, r2Key, obj)).To(Succeed())
		Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
		Expect(r.crud().deletingResourcesOf(ctx, worker)).To(ConsistOf(reconcile.Request{NamespacedName: r2Key}),
			"only the bucket that exists and is being deleted")
	})

	It("watches WorkerScripts, the kind it waits for", func() {
		Expect(r2BucketKind.dependents).To(HaveLen(1))
		Expect(r2BucketKind.dependents[0].newObject()).To(BeAssignableToTypeOf(&cloudflarev1alpha1.WorkerScript{}))
	})
})
