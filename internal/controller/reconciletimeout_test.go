/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/cloudflare/cloudflare-go/v7/kv"
	"github.com/cloudflare/cloudflare-go/v7/option"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// hungKVNamespaceAPI is fakeKVNamespaceAPI, but creating a namespace with the
// title hung gets no answer, as from a Cloudflare that went silent: the call
// returns only once its context is done, as both SDKs' calls do.
type hungKVNamespaceAPI struct {
	fakeKVNamespaceAPI
	hung  string
	calls atomic.Int32
}

func (f *hungKVNamespaceAPI) New(ctx context.Context, params kv.NamespaceNewParams,
	opts ...option.RequestOption) (*kv.Namespace, error) {
	if params.Title.Value != f.hung {
		return f.fakeKVNamespaceAPI.New(ctx, params, opts...)
	}
	f.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

// These specs run a controller with the manager's reconcile timeout, which
// cmd/main.go sets from --reconcile-timeout. Every kind reports through the
// same status helpers, so KVNamespace stands in for all of them.
var _ = Describe("The reconcile timeout", func() {
	const (
		ns          = "default"
		accountName = "reconcile-timeout-account"
		secretName  = "reconcile-timeout-token"
	)
	ctx := context.Background()
	hungKey := types.NamespacedName{Name: "reconcile-timeout-hung", Namespace: ns}
	nextKey := types.NamespacedName{Name: "reconcile-timeout-next", Namespace: ns}

	ready := func(key types.NamespacedName) *metav1.Condition {
		obj := &cloudflarev1alpha1.KVNamespace{}
		Expect(k8sClient.Get(ctx, key, obj)).To(Succeed())
		return meta.FindStatusCondition(obj.Status.Conditions, cloudflarev1alpha1.ConditionReady)
	}

	createNamespace := func(key types.NamespacedName, title string) {
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.KVNamespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace, Finalizers: []string{reconciler.Finalizer},
			},
			Spec: cloudflarev1alpha1.KVNamespaceSpec{
				AccountRef: corev1.LocalObjectReference{Name: accountName},
				Title:      title,
			},
		})).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.KVNamespace{}, key)
	}

	It("ends a reconcile whose Cloudflare call never returns with APIError, retries it, and goes on to the next "+
		"resource", func() {
		account := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "acct-reconcile-timeout",
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
			},
		}
		Expect(k8sClient.Create(ctx, account)).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
		account.Status.Conditions = []metav1.Condition{{
			Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Validated", LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, account)).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
		})).To(Succeed())
		DeferCleanup(forceDelete, ctx, &corev1.Secret{}, types.NamespacedName{Name: secretName, Namespace: ns})

		By("creating a KVNamespace whose namespace Cloudflare never creates, then another")
		createNamespace(hungKey, "hung-title")
		createNamespace(nextKey, "next-title")

		By("running the KVNamespace controller, one reconcile at a time, with a timeout of a second")
		api := &hungKVNamespaceAPI{fakeKVNamespaceAPI: fakeKVNamespaceAPI{newFakeCRUDStore[kv.Namespace](
			kvNamespaceListPageSize)}, hung: "hung-title"}
		mgr, err := manager.New(cfg, manager.Options{
			Controller: config.Controller{SkipNameValidation: new(true), ReconciliationTimeout: time.Second},
			Metrics:    metricsserver.Options{BindAddress: "0"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect((&KVNamespaceReconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
			NewKVNamespaceAPI: func(string) (KVNamespaceAPI, error) { return api, nil },
		}).SetupWithManager(mgr)).To(Succeed())
		mgrCtx, stop := context.WithCancel(ctx)
		stopped := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(stopped)
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		// Stop it before the cleanup deletes the resources, which it would
		// otherwise reconcile.
		DeferCleanup(func() {
			stop()
			Eventually(stopped).Should(BeClosed())
		})

		Eventually(func(g Gomega) {
			cond := ready(nextKey)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal("Synced"))
		}).WithTimeout(10*time.Second).Should(Succeed(), "the controller did not go on to the next resource")
		Eventually(func(g Gomega) {
			cond := ready(hungKey)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("APIError"))
			g.Expect(cond.Message).To(Equal("Cloudflare API error: context deadline exceeded (reconciliation timeout)"))
		}).WithTimeout(10 * time.Second).Should(Succeed())
		Eventually(api.calls.Load).WithTimeout(10*time.Second).Should(BeNumerically(">=", 3),
			"the timed out reconcile was not retried")
	})
})
