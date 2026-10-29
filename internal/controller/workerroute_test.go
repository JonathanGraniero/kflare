/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

// WorkerRoute is generated, and so are most of its specs
// (zz_generated.workerroute_test.go). These are the ones the generated specs
// cannot write: the patterns its entry's validation allows, which the
// generated specs check with one value it refuses.
var _ = Describe("WorkerRoute patterns", func() {
	ctx := context.Background()
	key := types.NamespacedName{Name: "route-pattern", Namespace: "default"}

	AfterEach(func() {
		forceDelete(ctx, &cloudflarev1alpha1.WorkerRoute{}, key)
	})

	DescribeTable("takes a host, an optional leading wildcard and a path, and nothing else",
		func(pattern string, valid bool) {
			err := k8sClient.Create(ctx, &cloudflarev1alpha1.WorkerRoute{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
				Spec: cloudflarev1alpha1.WorkerRouteSpec{
					ZoneRef: corev1.LocalObjectReference{Name: "route-pattern-zone"},
					Pattern: pattern,
				},
			})
			if valid {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(MatchError(ContainSubstring("spec.pattern")))
			}
		},
		Entry("host and path wildcard", "wr-example.com/*", true),
		Entry("subdomain wildcard", "*.wr-example.com/*", true),
		Entry("wildcard including the apex", "*wr-example.com/*", true),
		Entry("host only", "api.wr-example.com", true),
		Entry("nothing", "", false),
		Entry("a path without a host", "/api/*", false),
		Entry("a scheme, which Cloudflare stores as it is given", "https://wr-example.com/*", false),
		Entry("a port", "wr-example.com:8080/*", false),
		Entry("whitespace", "wr-example.com/a b", false),
	)
})
