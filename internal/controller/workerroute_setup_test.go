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

var _ = Describe("defaultWorkerRouteAPI", func() {
	It("returns an error for an empty token", func() {
		_, err := defaultWorkerRouteAPI("")
		Expect(err).To(HaveOccurred())
	})

	It("returns the production client for a token", func() {
		api, err := defaultWorkerRouteAPI("token")
		Expect(err).NotTo(HaveOccurred())
		Expect(api).To(BeAssignableToTypeOf(&cfpkg.Client{}))
	})
})

var _ = Describe("WorkerRouteReconciler SetupWithManager", func() {
	It("registers without error", func() {
		mgr, err := manager.New(cfg, manager.Options{})
		Expect(err).NotTo(HaveOccurred())
		r := &WorkerRouteReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
	})
})

var _ = DescribeTable("patternInZone",
	func(pattern, zone string, want bool) {
		Expect(patternInZone(pattern, zone)).To(Equal(want))
	},
	Entry("apex with path", "example.com/*", "example.com", true),
	Entry("apex without path", "example.com", "example.com", true),
	Entry("subdomain", "api.example.com/v1/*", "example.com", true),
	Entry("subdomain wildcard", "*.example.com/*", "example.com", true),
	Entry("wildcard including the apex", "*example.com/*", "example.com", true),
	Entry("case-insensitive", "API.Example.COM/*", "example.com", true),
	Entry("another zone", "example.org/*", "example.com", false),
	Entry("a name that only ends with the zone", "notexample.com/*", "example.com", false),
	Entry("the zone as a path", "evil.org/example.com", "example.com", false),
)

var _ = Describe("routeWithPattern", func() {
	routes := []cf.WorkerRoute{{ID: "a", Pattern: "example.com/a/*"}, {ID: "b", Pattern: "example.com/b/*"}}

	It("finds the route with an exact pattern", func() {
		rt, ok := routeWithPattern(routes, "example.com/b/*")
		Expect(ok).To(BeTrue())
		Expect(rt.ID).To(Equal("b"))
	})

	It("reports a missing pattern", func() {
		_, ok := routeWithPattern(routes, "example.com/*")
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("isWorkerScriptReady", func() {
	ready := []metav1.Condition{{Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue}}

	It("needs both Ready=True and an upload", func() {
		ws := &cloudflarev1alpha1.WorkerScript{}
		Expect(isWorkerScriptReady(ws)).To(BeFalse())
		ws.Status.Conditions = ready
		Expect(isWorkerScriptReady(ws)).To(BeFalse())
		ws.Status.CloudflareMetadata.ModifiedOn = "2026-10-05T00:00:00Z"
		Expect(isWorkerScriptReady(ws)).To(BeTrue())
	})
})

var _ = Describe("WorkerRouteReconciler mappers", func() {
	const ns = "default"
	ctx := context.Background()
	r := &WorkerRouteReconciler{}

	BeforeEach(func() {
		r.Client = k8sClient
		for name, worker := range map[string]string{"wrm-to-worker": "wrm-worker", "wrm-exclusion": ""} {
			route := &cloudflarev1alpha1.WorkerRoute{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: cloudflarev1alpha1.WorkerRouteSpec{
					ZoneRef: corev1.LocalObjectReference{Name: "wrm-zone"}, Pattern: "wrm.example.com/" + name,
				},
			}
			if worker != "" {
				route.Spec.WorkerScriptRef = &corev1.LocalObjectReference{Name: worker}
			}
			Expect(k8sClient.Create(ctx, route)).To(Succeed())
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.WorkerRoute{}, types.NamespacedName{Name: name, Namespace: ns})
		}
	})

	request := func(name string) reconcile.Request {
		return reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
	}

	It("maps a Zone to every route in it", func() {
		zone := &cloudflarev1alpha1.Zone{ObjectMeta: metav1.ObjectMeta{Name: "wrm-zone", Namespace: ns}}
		Expect(r.routesForZone(ctx, zone)).To(ConsistOf(request("wrm-to-worker"), request("wrm-exclusion")))
	})

	It("maps a WorkerScript to the routes that run it", func() {
		ws := &cloudflarev1alpha1.WorkerScript{ObjectMeta: metav1.ObjectMeta{Name: "wrm-worker", Namespace: ns}}
		Expect(r.routesForWorkerScript(ctx, ws)).To(ConsistOf(request("wrm-to-worker")))
	})

	It("ignores objects in another namespace", func() {
		zone := &cloudflarev1alpha1.Zone{ObjectMeta: metav1.ObjectMeta{Name: "wrm-zone", Namespace: "kube-public"}}
		Expect(r.routesForZone(ctx, zone)).To(BeEmpty())
	})

	It("returns nil when the list fails", func() {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		zone := &cloudflarev1alpha1.Zone{ObjectMeta: metav1.ObjectMeta{Name: "wrm-zone", Namespace: ns}}
		Expect(r.routesForZone(cancelled, zone)).To(BeNil())
	})
})

var _ = Describe("WorkerRouteReconciler newWorkerRouteAPI", func() {
	It("falls back to the production client when no factory is injected", func() {
		_, err := (&WorkerRouteReconciler{}).newWorkerRouteAPI("")
		Expect(err).To(HaveOccurred(), "the production client rejects an empty token")
	})
})

var _ = Describe("claimedIDs", func() {
	It("returns the list error", func() {
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := claimedIDs(cancelled, k8sClient, &cloudflarev1alpha1.WorkerRouteList{},
			cloudflarev1alpha1.WorkerRouteIDLabel, &cloudflarev1alpha1.WorkerRoute{})
		Expect(err).To(HaveOccurred())
	})
})
