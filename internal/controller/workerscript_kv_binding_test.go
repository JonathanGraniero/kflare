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

var _ = Describe("WorkerScript kvNamespaceRef bindings", func() {
	const (
		ns          = "default"
		wsName      = "wskv-worker"
		kvName      = "wskv-namespace"
		accountName = "wskv-account"
		secretName  = "wskv-api-token"
	)

	ctx := context.Background()
	wsKey := types.NamespacedName{Name: wsName, Namespace: ns}
	kvKey := types.NamespacedName{Name: kvName, Namespace: ns}
	uploadedAt := time.Date(2026, 10, 7, 12, 0, 0, 123456000, time.UTC)

	var fake *fakeWorkerScriptAPI
	var r *WorkerScriptReconciler

	// createKV creates the KVNamespace in account; with a namespace ID it is
	// marked ready, as the KVNamespace controller would after a sync.
	createKV := func(account, namespaceID string) {
		kv := &cloudflarev1alpha1.KVNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: kvName, Namespace: ns},
			Spec: cloudflarev1alpha1.KVNamespaceSpec{
				AccountRef: corev1.LocalObjectReference{Name: account}, Title: "wskv-title",
			},
		}
		Expect(k8sClient.Create(ctx, kv)).To(Succeed())
		if namespaceID != "" {
			setKVNamespaceID(ctx, kvKey, namespaceID)
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
			WorkerMetaData: cf.WorkerMetaData{ID: "wskv-script", ETAG: "etag", ModifiedOn: uploadedAt},
		}}}
		r = &WorkerScriptReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			NewWorkerScriptAPI: func(string) (WorkerScriptAPI, error) { return fake, nil },
		}

		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "acct-wskv",
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
				Name: "wskv-script", AccountRef: corev1.LocalObjectReference{Name: accountName}, Script: &script,
				Bindings: []cloudflarev1alpha1.WorkerBinding{
					{Name: "CACHE", KVNamespaceRef: &corev1.LocalObjectReference{Name: kvName}},
				},
			},
		})).To(Succeed())

		DeferCleanup(func() {
			forceDelete(ctx, &cloudflarev1alpha1.WorkerScript{}, wsKey)
			forceDelete(ctx, &cloudflarev1alpha1.KVNamespace{}, kvKey)
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
			forceDelete(ctx, &corev1.Secret{}, types.NamespacedName{Name: secretName, Namespace: ns})
		})
	})

	DescribeTable("waits without uploading",
		func(setup func(), reason string) {
			setup()
			result, err := reconcileWorker()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue(), "the KVNamespace watch re-triggers this reconcile")
			Expect(readyReason()).To(Equal(reason))
			Expect(fake.uploads).To(BeEmpty())
		},
		Entry("when the KVNamespace is missing", func() {}, "KVNamespaceNotFound"),
		Entry("when the KVNamespace is not ready", func() { createKV(accountName, "") }, "KVNamespaceNotReady"),
		Entry("when the KVNamespace is in another account", func() { createKV("wskv-other-account", "kv-1") }, "AccountMismatch"),
	)

	It("binds the namespace by its Cloudflare ID", func() {
		createKV(accountName, "kv-1")
		_, err := reconcileWorker()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.uploads).To(HaveLen(1))
		Expect(fake.uploads[0].Bindings).To(HaveKeyWithValue("CACHE", cf.WorkerKvNamespaceBinding{NamespaceID: "kv-1"}))
		Expect(readyReason()).To(Equal("Synced"))
	})

	It("uploads again with the new ID when the namespace is recreated", func() {
		createKV(accountName, "kv-1")
		_, err := reconcileWorker()
		Expect(err).NotTo(HaveOccurred())

		setKVNamespaceID(ctx, kvKey, "kv-2")
		_, err = reconcileWorker()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.uploads).To(HaveLen(2))
		Expect(fake.uploads[1].Bindings).To(HaveKeyWithValue("CACHE", cf.WorkerKvNamespaceBinding{NamespaceID: "kv-2"}))
	})

	It("maps a KVNamespace to the WorkerScripts that bind it", func() {
		kv := &cloudflarev1alpha1.KVNamespace{ObjectMeta: metav1.ObjectMeta{Name: kvName, Namespace: ns}}
		Expect(r.workersForKVNamespace(ctx, kv)).To(ConsistOf(reconcile.Request{NamespacedName: wsKey}))
		other := &cloudflarev1alpha1.KVNamespace{ObjectMeta: metav1.ObjectMeta{Name: "unbound", Namespace: ns}}
		Expect(r.workersForKVNamespace(ctx, other)).To(BeEmpty())
	})
})

// setKVNamespaceID marks the KVNamespace at key ready with namespaceID.
func setKVNamespaceID(ctx context.Context, key types.NamespacedName, namespaceID string) {
	kv := &cloudflarev1alpha1.KVNamespace{}
	Expect(k8sClient.Get(ctx, key, kv)).To(Succeed())
	kv.Status.CloudflareMetadata.NamespaceID = namespaceID
	kv.Status.Conditions = []metav1.Condition{{
		Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
		Reason: "Synced", LastTransitionTime: metav1.Now(),
	}}
	Expect(k8sClient.Status().Update(ctx, kv)).To(Succeed())
}
