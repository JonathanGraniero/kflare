/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	"github.com/cloudflare/cloudflare-go/v7/kv"
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

// KVNamespace is generated, and so are most of its specs
// (zz_generated.kvnamespace_test.go). These are the ones the generated specs
// cannot write: a namespace waits for the WorkerScripts bound to it.
var _ = Describe("KVNamespace and the WorkerScripts bound to it", func() {
	const (
		ns          = "default"
		kvName      = "kv-bound-namespace"
		workerName  = "kv-bound-worker"
		accountName = "kv-bound-account"
		secretName  = "kv-bound-token"
		accountID   = "acct-kv-bound"
	)
	ctx := context.Background()
	kvKey := types.NamespacedName{Name: kvName, Namespace: ns}
	workerKey := types.NamespacedName{Name: workerName, Namespace: ns}

	var store *fakeCRUDStore[kv.Namespace]
	var r *KVNamespaceReconciler

	// bind creates a WorkerScript that binds the namespace kvName, and the
	// namespace named other.
	bind := func(other string) {
		script := "export default {}"
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.WorkerScript{
			ObjectMeta: metav1.ObjectMeta{Name: workerName, Namespace: ns, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.WorkerScriptSpec{
				Name: "kv-bound-script", AccountRef: corev1.LocalObjectReference{Name: accountName}, Script: &script,
				Bindings: []cloudflarev1alpha1.WorkerBinding{
					{Name: "KV", KVNamespaceRef: &corev1.LocalObjectReference{Name: kvName}},
					{Name: "OTHER", KVNamespaceRef: &corev1.LocalObjectReference{Name: other}},
				},
			},
		})).To(Succeed())
	}

	BeforeEach(func() {
		store = newFakeCRUDStore[kv.Namespace](kvNamespaceListPageSize, "")
		r = &KVNamespaceReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			NewKVNamespaceAPI: func(string) (KVNamespaceAPI, error) { return fakeKVNamespaceAPI{store}, nil },
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
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.KVNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: kvName, Namespace: ns, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.KVNamespaceSpec{
				AccountRef: corev1.LocalObjectReference{Name: accountName}, Title: "kv-bound-title",
			},
		})).To(Succeed())

		DeferCleanup(func() {
			forceDelete(ctx, &cloudflarev1alpha1.KVNamespace{}, kvKey)
			forceDelete(ctx, &cloudflarev1alpha1.WorkerScript{}, workerKey)
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
			forceDelete(ctx, &corev1.Secret{}, types.NamespacedName{Name: secretName, Namespace: ns})
		})
	})

	It("waits for the WorkerScripts bound to it before it is deleted", func() {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: kvKey})
		Expect(err).NotTo(HaveOccurred())
		Expect(store.objects).To(HaveLen(1))
		bind("kv-bound-missing")
		obj := &cloudflarev1alpha1.KVNamespace{}
		Expect(k8sClient.Get(ctx, kvKey, obj)).To(Succeed())
		Expect(k8sClient.Delete(ctx, obj)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: kvKey})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, kvKey, obj)).To(Succeed())
		ready := meta.FindStatusCondition(obj.Status.Conditions, cloudflarev1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal("InUse"))
		Expect(ready.Message).To(ContainSubstring("WorkerScript " + workerKey.String()))
		Expect(store.objects).To(HaveLen(1))

		forceDelete(ctx, &cloudflarev1alpha1.WorkerScript{}, workerKey)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: kvKey})
		Expect(err).NotTo(HaveOccurred())
		Expect(store.objects).To(BeEmpty())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, kvKey, &cloudflarev1alpha1.KVNamespace{}))).To(BeTrue())
	})

	It("is re-triggered by the deletion of a WorkerScript bound to it, once it is being deleted", func() {
		bind("kv-bound-missing")
		worker := &cloudflarev1alpha1.WorkerScript{}
		Expect(k8sClient.Get(ctx, workerKey, worker)).To(Succeed())
		Expect(r.crud().deletingResourcesOf(ctx, worker)).To(BeEmpty(), "the namespace is not being deleted yet")

		obj := &cloudflarev1alpha1.KVNamespace{}
		Expect(k8sClient.Get(ctx, kvKey, obj)).To(Succeed())
		Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
		Expect(r.crud().deletingResourcesOf(ctx, worker)).To(ConsistOf(reconcile.Request{NamespacedName: kvKey}),
			"only the namespace that exists and is being deleted")
	})

	It("watches WorkerScripts, the kind it waits for", func() {
		Expect(kvNamespaceKind.dependents).To(HaveLen(1))
		Expect(kvNamespaceKind.dependents[0].newObject()).To(BeAssignableToTypeOf(&cloudflarev1alpha1.WorkerScript{}))
	})
})
