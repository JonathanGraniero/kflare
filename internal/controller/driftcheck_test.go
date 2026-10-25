/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// driftCheckTestInterval is the drift check interval the specs set, and
// driftCheckTestSpread how far a check may fall from it.
const (
	driftCheckTestInterval = time.Hour
	driftCheckTestSpread   = driftCheckTestInterval / 10
)

// turnOnDriftChecks sets the drift check interval to driftCheckTestInterval
// until the end of the spec.
func turnOnDriftChecks() {
	GinkgoHelper()
	Expect(reconciler.SetDriftCheckInterval(driftCheckTestInterval)).To(Succeed())
	DeferCleanup(reconciler.SetDriftCheckInterval, time.Duration(0))
}

// expectDriftCheck runs sync, a reconcile that leaves its resource in sync,
// once without a drift check interval and once with one, and checks what
// each schedules: nothing, then the next drift check, within the interval's
// spread.
func expectDriftCheck(sync func() (reconcile.Result, error)) {
	GinkgoHelper()
	result, err := sync()
	Expect(err).NotTo(HaveOccurred())
	Expect(result).To(BeZero(), "a sync without a drift check interval scheduled a reconcile")

	turnOnDriftChecks()
	result, err = sync()
	Expect(err).NotTo(HaveOccurred())
	Expect(result.RequeueAfter).To(BeNumerically("~", driftCheckTestInterval, driftCheckTestSpread),
		"the next drift check")
}

// These specs cover readyWithDriftCheck, which every controller's successful
// reconcile ends with. Each controller's own specs check that its successful
// reconciles schedule a drift check (expectDriftCheck); a CloudflareAccount
// stands in for all of them here.
var _ = Describe("The drift check after a successful reconcile", func() {
	const name = "drift-check"
	ctx := context.Background()
	var account *cloudflarev1alpha1.CloudflareAccount

	BeforeEach(func() {
		account = &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "acct-" + name,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: name, Namespace: "default", Key: "CF_API_TOKEN"},
			},
		}
		Expect(k8sClient.Create(ctx, account)).To(Succeed())
		DeferCleanup(func() {
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: name})
		})
	})

	It("writes Ready, and schedules nothing without an interval", func() {
		result, err := readyWithDriftCheck(ctx, k8sClient, account, "Validated", "valid")
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(BeZero())
		stored := &cloudflarev1alpha1.CloudflareAccount{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, stored)).To(Succeed())
		Expect(stored.Status.Conditions).To(ContainElement(And(
			HaveField("Type", cloudflarev1alpha1.ConditionReady),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", "Validated"),
		)))
	})

	It("spreads the checks of resources synced together across the interval's spread", func() {
		turnOnDriftChecks()
		lowest, highest := time.Duration(1<<63-1), time.Duration(0)
		for range 200 {
			result, err := readyWithDriftCheck(ctx, k8sClient, account, "Validated", "valid")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically("~", driftCheckTestInterval, driftCheckTestSpread))
			lowest, highest = min(lowest, result.RequeueAfter), max(highest, result.RequeueAfter)
		}
		// The chance that 200 draws all fall within half of the range is
		// below 10^-57.
		Expect(highest-lowest).To(BeNumerically(">", driftCheckTestSpread),
			"the checks fell between %s and %s", lowest, highest)
	})

	It("returns the status write's error, and schedules no drift check, when the write fails", func() {
		turnOnDriftChecks()
		stale := account.DeepCopy()
		Expect(updateNotReady(ctx, k8sClient, account, "APIError", "first")).To(Succeed())
		result, err := readyWithDriftCheck(ctx, k8sClient, stale, "Validated", "from a stale copy")
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "got %v", err)
		Expect(result).To(BeZero())
	})
})
