/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

var _ = Describe("defaultKVNamespaceAPI", func() {
	It("returns an error for an empty token", func() {
		_, err := defaultKVNamespaceAPI("")
		Expect(err).To(HaveOccurred())
	})

	It("returns the production client for a token", func() {
		api, err := defaultKVNamespaceAPI("token")
		Expect(err).NotTo(HaveOccurred())
		Expect(api).To(BeAssignableToTypeOf(&cfpkg.Client{}))
	})

	It("is used when no factory is injected", func() {
		_, err := (&KVNamespaceReconciler{}).newKVNamespaceAPI("")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("KVNamespaceReconciler SetupWithManager", func() {
	It("registers without error", func() {
		mgr, err := manager.New(cfg, manager.Options{})
		Expect(err).NotTo(HaveOccurred())
		r := &KVNamespaceReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
	})
})

var _ = Describe("KV namespace lookups", func() {
	namespaces := []cf.WorkersKVNamespace{{ID: "a", Title: "alpha"}, {ID: "b", Title: "beta"}}

	It("finds a namespace by ID, and never by an empty ID", func() {
		ns, ok := kvNamespaceWithID(namespaces, "b")
		Expect(ok).To(BeTrue())
		Expect(ns.Title).To(Equal("beta"))
		_, ok = kvNamespaceWithID(namespaces, "")
		Expect(ok).To(BeFalse())
		_, ok = kvNamespaceWithID(namespaces, "c")
		Expect(ok).To(BeFalse())
	})

	It("finds a namespace by title", func() {
		ns, ok := kvNamespaceWithTitle(namespaces, "alpha")
		Expect(ok).To(BeTrue())
		Expect(ns.ID).To(Equal("a"))
		_, ok = kvNamespaceWithTitle(namespaces, "gamma")
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("KVNamespaceReconciler mappers", func() {
	const ns = "default"
	ctx := context.Background()
	r := &KVNamespaceReconciler{}

	BeforeEach(func() {
		r.Client = k8sClient
		kv := &cloudflarev1alpha1.KVNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "kvm-namespace", Namespace: ns},
			Spec: cloudflarev1alpha1.KVNamespaceSpec{
				AccountRef: corev1.LocalObjectReference{Name: "kvm-account"}, Title: "kvm-title",
			},
		}
		Expect(k8sClient.Create(ctx, kv)).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.KVNamespace{}, types.NamespacedName{Name: kv.Name, Namespace: ns})
	})

	It("maps a CloudflareAccount to the namespaces that use it", func() {
		account := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Name: "kvm-account"}}
		Expect(r.namespacesForAccount(ctx, account)).To(ConsistOf(
			reconcile.Request{NamespacedName: types.NamespacedName{Name: "kvm-namespace", Namespace: ns}}))
		other := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Name: "kvm-other"}}
		Expect(r.namespacesForAccount(ctx, other)).To(BeEmpty())
	})

	It("returns nil when the list fails", func() {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		account := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Name: "kvm-account"}}
		Expect(r.namespacesForAccount(cancelled, account)).To(BeNil())
	})

	It("re-triggers each bound namespace that is being deleted when a WorkerScript goes", func() {
		ws := &cloudflarev1alpha1.WorkerScript{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns},
			Spec: cloudflarev1alpha1.WorkerScriptSpec{Bindings: []cloudflarev1alpha1.WorkerBinding{
				{Name: "A", KVNamespaceRef: &corev1.LocalObjectReference{Name: "kvm-namespace"}},
				{Name: "B", KVNamespaceRef: &corev1.LocalObjectReference{Name: "kvm-missing"}},
			}},
		}
		Expect(r.deletingKVNamespacesOf(ctx, ws)).To(BeEmpty(), "the namespace is not being deleted yet")

		kv := &cloudflarev1alpha1.KVNamespace{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "kvm-namespace", Namespace: ns}, kv)).To(Succeed())
		kv.Finalizers = []string{"test/hold"}
		Expect(k8sClient.Update(ctx, kv)).To(Succeed())
		Expect(k8sClient.Delete(ctx, kv)).To(Succeed())
		Expect(r.deletingKVNamespacesOf(ctx, ws)).To(ConsistOf(
			reconcile.Request{NamespacedName: types.NamespacedName{Name: "kvm-namespace", Namespace: ns}}))
	})
})

var _ = Describe("KVNamespace as a dependent", func() {
	It("counts as a user of its CloudflareAccount", func() {
		ctx := context.Background()
		kv := &cloudflarev1alpha1.KVNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "kvd-namespace", Namespace: "default"},
			Spec: cloudflarev1alpha1.KVNamespaceSpec{
				AccountRef: corev1.LocalObjectReference{Name: "kvd-account"}, Title: "kvd-title",
			},
		}
		Expect(k8sClient.Create(ctx, kv)).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.KVNamespace{}, types.NamespacedName{Name: kv.Name, Namespace: kv.Namespace})

		deps, err := dependentsOf(ctx, k8sClient, "", "kvd-account", accountDependents)
		Expect(err).NotTo(HaveOccurred())
		Expect(deps).To(ConsistOf("KVNamespace default/kvd-namespace"))
	})
})
