/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"sync/atomic"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// countingWorkers counts a fake's uploads where a spec can read them while a
// manager runs.
type countingWorkers struct {
	*fakeWorkerScriptAPI
	uploads *atomic.Int64
}

func (w countingWorkers) UploadWorker(ctx context.Context, rc *cf.ResourceContainer,
	params cf.CreateWorkerParams) (cf.WorkerScriptResponse, error) {
	w.uploads.Add(1)
	return w.fakeWorkerScriptAPI.UploadWorker(ctx, rc, params)
}

var _ = Describe("WorkerScript hooks", func() {
	const (
		ns          = "default"
		accountName = "ws-hooks-account"
		tokenName   = "ws-hooks-token"
		configMap   = "ws-hooks-code"
		secretName  = "ws-hooks-secret"
		kvName      = "ws-hooks-kv"
		bucketName  = "ws-hooks-bucket"
	)
	ctx := context.Background()
	key := types.NamespacedName{Name: "ws-hooks", Namespace: ns}

	create := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(forceDelete, ctx, obj.DeepCopyObject().(client.Object), client.ObjectKeyFromObject(obj))
	}
	setReady := func(obj conditionedObject) {
		obj.SetConditions([]metav1.Condition{{Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Synced", LastTransitionTime: metav1.Now()}})
		Expect(k8sClient.Status().Update(ctx, obj)).To(Succeed())
	}
	createSecret := func() *corev1.Secret {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data: map[string][]byte{"api-key": []byte("value")}}
		create(secret)
		return secret
	}

	It("hashes a Secret's version, never its value", func() {
		secret := createSecret()
		ws := &cloudflarev1alpha1.WorkerScript{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns},
			Spec: cloudflarev1alpha1.WorkerScriptSpec{WorkerScriptSpecExtras: cloudflarev1alpha1.WorkerScriptSpecExtras{
				Name: "hashed", Script: new("export default {}"),
				Bindings: []cloudflarev1alpha1.WorkerBinding{{Name: "API_KEY",
					SecretKeyRef: &cloudflarev1alpha1.WorkerKeyReference{Name: secretName, Key: "api-key"}}},
			}},
		}
		first := map[string]string{}
		Expect(resolveWorkerUpload(ctx, k8sClient, ws, first)).To(BeNil())
		Expect(workerUpload(ws, first).Bindings["API_KEY"]).To(Equal(cf.WorkerSecretTextBinding{Text: "value"}))

		secret.Labels = map[string]string{"touched": "true"}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		second := map[string]string{}
		Expect(resolveWorkerUpload(ctx, k8sClient, ws, second)).To(BeNil())
		Expect(second[workerHashValue]).NotTo(Equal(first[workerHashValue]),
			"the same value, in another version of the Secret")
	})

	It("follows what a Worker is made of, in a running manager", func() {
		create(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: tokenName, Namespace: ns},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
		})
		account := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "ws-hooks-account-id",
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: tokenName, Namespace: ns, Key: "CF_API_TOKEN"},
			},
		}
		create(account)
		setReady(account)
		secret := createSecret()
		code := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configMap, Namespace: ns},
			Data: map[string]string{"worker.js": "export default {}"}}
		create(code)
		accountRef := corev1.LocalObjectReference{Name: accountName}
		kv := &cloudflarev1alpha1.KVNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: kvName, Namespace: ns},
			Spec:       cloudflarev1alpha1.KVNamespaceSpec{AccountRef: accountRef, Title: "ws-hooks"},
		}
		create(kv)
		bucket := &cloudflarev1alpha1.R2Bucket{
			ObjectMeta: metav1.ObjectMeta{Name: bucketName, Namespace: ns},
			Spec:       cloudflarev1alpha1.R2BucketSpec{AccountRef: accountRef, Name: "ws-hooks"},
		}
		create(bucket)
		create(&cloudflarev1alpha1.WorkerScript{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.WorkerScriptSpec{
				AccountRef: accountRef,
				WorkerScriptSpecExtras: cloudflarev1alpha1.WorkerScriptSpecExtras{
					Name:               "ws-hooks",
					ScriptConfigMapRef: &cloudflarev1alpha1.WorkerKeyReference{Name: configMap, Key: "worker.js"},
					Bindings: []cloudflarev1alpha1.WorkerBinding{
						{Name: "API_KEY", SecretKeyRef: &cloudflarev1alpha1.WorkerKeyReference{Name: secretName, Key: "api-key"}},
						{Name: "CACHE", KVNamespaceRef: &corev1.LocalObjectReference{Name: kvName}},
						{Name: "ASSETS", R2BucketRef: &corev1.LocalObjectReference{Name: bucketName}},
					},
				},
			},
		})

		uploads := &atomic.Int64{}
		api := countingWorkers{&fakeWorkerScriptAPI{uploadResp: cf.WorkerScriptResponse{WorkerScript: cf.WorkerScript{
			WorkerMetaData: cf.WorkerMetaData{ID: "ws-hooks", ETAG: "etag", ModifiedOn: time.Now()},
		}}}, uploads}
		startManager(ctx, func(mgr manager.Manager) error {
			return (&WorkerScriptReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
				NewWorkerScriptAPI: func(string) (WorkerScriptAPI, error) { return api, nil }}).SetupWithManager(mgr)
		})
		reason := func() string {
			ws := &cloudflarev1alpha1.WorkerScript{}
			if err := k8sClient.Get(ctx, key, ws); err != nil {
				return err.Error()
			}
			if cond := meta.FindStatusCondition(ws.Status.Conditions, cloudflarev1alpha1.ConditionReady); cond != nil {
				return cond.Reason
			}
			return ""
		}
		const wait = 10 * time.Second

		By("waiting for its KVNamespace, then its R2Bucket, then uploading")
		Eventually(reason, wait).Should(Equal("KVNamespaceNotReady"))
		kv.Status.CloudflareMetadata.NamespaceID = "namespace"
		setReady(kv)
		Eventually(reason, wait).Should(Equal("R2BucketNotReady"))
		setReady(bucket)
		Eventually(uploads.Load, wait).Should(Equal(int64(1)))
		Eventually(reason, wait).Should(Equal("Synced"))

		By("uploading again when its ConfigMap changes")
		code.Data["worker.js"] = "export default { fetch() {} }"
		Expect(k8sClient.Update(ctx, code)).To(Succeed())
		Eventually(uploads.Load, wait).Should(Equal(int64(2)))

		By("uploading again when its Secret changes")
		secret.Data["api-key"] = []byte("rotated")
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		Eventually(uploads.Load, wait).Should(Equal(int64(3)))
	})
})
