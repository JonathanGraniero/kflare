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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

var _ = Describe("defaultWorkerScriptAPI", func() {
	It("returns an error for an empty token", func() {
		_, err := defaultWorkerScriptAPI("")
		Expect(err).To(HaveOccurred())
	})

	It("returns a client satisfying WorkerScriptAPI for a non-empty token", func() {
		api, err := defaultWorkerScriptAPI("any-token-value")
		Expect(err).NotTo(HaveOccurred())
		Expect(api).NotTo(BeNil())
	})

	It("is used when NewWorkerScriptAPI is nil", func() {
		r := &WorkerScriptReconciler{}
		_, err := r.newWorkerScriptAPI("")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("desiredUpload", func() {
	It("uploads as an ES module when format is empty", func() {
		ws := &cloudflarev1alpha1.WorkerScript{Spec: cloudflarev1alpha1.WorkerScriptSpec{
			Name:   "no-format",
			Script: cf.StringPtr("export default {};"),
		}}
		upload, hash, srcErr := (&WorkerScriptReconciler{}).desiredUpload(context.Background(), ws)
		Expect(srcErr).To(BeNil())
		Expect(upload.Module).To(BeTrue())
		Expect(hash).To(HaveLen(64))
	})
})

var _ = Describe("formatModifiedOn", func() {
	It("keeps sub-second precision and normalises to UTC", func() {
		t := time.Date(2026, 10, 2, 22, 13, 10, 984591000, time.FixedZone("EDT", -4*3600))
		Expect(formatModifiedOn(t)).To(Equal("2026-10-03T02:13:10.984591Z"))
	})
})

var _ = Describe("workerDrift", func() {
	at := time.Date(2026, 10, 3, 2, 13, 10, 984591000, time.UTC)
	applied := formatModifiedOn(at)

	It("reports nothing when the Worker still has kflare's upload", func() {
		reason, observed := workerDrift([]cf.WorkerMetaData{{ID: "other"}, {ID: "mine", ModifiedOn: at}}, "mine", applied)
		Expect(reason).To(BeEmpty())
		Expect(observed).To(Equal(applied))
	})

	It("reports a modification time other than kflare's upload", func() {
		reason, observed := workerDrift([]cf.WorkerMetaData{{ID: "mine", ModifiedOn: at.Add(time.Microsecond)}}, "mine", applied)
		Expect(reason).To(Equal("modified outside kflare"))
		Expect(observed).To(Equal("2026-10-03T02:13:10.984592Z"))
	})

	It("reports a missing Worker", func() {
		reason, observed := workerDrift(nil, "mine", applied)
		Expect(reason).To(Equal("missing from Cloudflare"))
		Expect(observed).To(BeEmpty())
	})
})

var _ = Describe("WorkerScriptReconciler reconcileDelete (no finalizer)", func() {
	It("returns no error when the Worker has no finalizer", func() {
		ws := &cloudflarev1alpha1.WorkerScript{
			ObjectMeta: metav1.ObjectMeta{Name: "no-finalizer-worker", Namespace: "default"},
		}
		r := &WorkerScriptReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		result, err := r.reconcileDelete(context.Background(), ws)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
	})
})

var _ = Describe("WorkerScriptReconciler SetupWithManager", func() {
	It("registers without error", func() {
		mgr, err := manager.New(cfg, manager.Options{})
		Expect(err).NotTo(HaveOccurred())

		r := &WorkerScriptReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
	})
})

var _ = Describe("WorkerScriptReconciler watch mappings", func() {
	const ns = "default"
	ctx := context.Background()
	r := func() *WorkerScriptReconciler {
		return &WorkerScriptReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}

	It("returns nil when the list fails", func() {
		cancelCtx, cancel := context.WithCancel(context.Background())
		cancel()
		Expect(r().workersForAccount(cancelCtx, &cloudflarev1alpha1.CloudflareAccount{})).To(BeNil())
	})

	Context("with one Worker reading code from a ConfigMap and one with a secret binding", func() {
		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.WorkerScript{
				ObjectMeta: metav1.ObjectMeta{Name: "wm-from-configmap", Namespace: ns},
				Spec: cloudflarev1alpha1.WorkerScriptSpec{
					Name:               "wm-from-configmap",
					AccountRef:         corev1.LocalObjectReference{Name: "wm-account-a"},
					ScriptConfigMapRef: &cloudflarev1alpha1.WorkerKeyReference{Name: "wm-code", Key: "worker.js"},
				},
			})).To(Succeed())
			Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.WorkerScript{
				ObjectMeta: metav1.ObjectMeta{Name: "wm-with-secret", Namespace: ns},
				Spec: cloudflarev1alpha1.WorkerScriptSpec{
					Name:       "wm-with-secret",
					AccountRef: corev1.LocalObjectReference{Name: "wm-account-b"},
					Script:     cf.StringPtr("export default {};"),
					Bindings: []cloudflarev1alpha1.WorkerBinding{
						{Name: "PLAIN", PlainText: cf.StringPtr("x")},
						{Name: "KEY", SecretKeyRef: &cloudflarev1alpha1.WorkerKeyReference{Name: "wm-secret", Key: "k"}},
					},
				},
			})).To(Succeed())
		})

		AfterEach(func() {
			for _, name := range []string{"wm-from-configmap", "wm-with-secret"} {
				ws := &cloudflarev1alpha1.WorkerScript{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, ws); err == nil {
					_ = k8sClient.Delete(ctx, ws)
				}
			}
		})

		request := func(name string) types.NamespacedName {
			return types.NamespacedName{Name: name, Namespace: ns}
		}

		It("maps a ConfigMap to the Workers reading code from it, in its namespace only", func() {
			reqs := r().workersForConfigMap(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "wm-code", Namespace: ns}})
			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].NamespacedName).To(Equal(request("wm-from-configmap")))

			elsewhere := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "wm-code", Namespace: "kube-system"}}
			Expect(r().workersForConfigMap(ctx, elsewhere)).To(BeEmpty())
		})

		It("maps a Secret to the Workers with a binding read from it", func() {
			reqs := r().workersForSecret(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "wm-secret", Namespace: ns}})
			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].NamespacedName).To(Equal(request("wm-with-secret")))

			Expect(r().workersForSecret(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unused", Namespace: ns}})).To(BeEmpty())
		})

		It("maps a CloudflareAccount to the Workers that reference it", func() {
			reqs := r().workersForAccount(ctx, &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Name: "wm-account-b"}})
			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].NamespacedName).To(Equal(request("wm-with-secret")))
		})
	})
})
