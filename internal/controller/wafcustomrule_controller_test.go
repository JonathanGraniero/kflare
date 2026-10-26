/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"strings"

	"github.com/cloudflare/cloudflare-go/v7/rulesets"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// The WAFCustomRule specs run the reconciler in envtest against
// fakeRulesetAPI (rulesetrule_test.go), the in-memory Rulesets API that
// follows the live API's behavior. Its zone, rulesTestZone, is the zone ID of
// the Zones created here.
var _ = Describe("WAFCustomRule Controller", func() {
	const (
		ns          = "default"
		otherNS     = "kube-public"
		zoneCRName  = "waf-test-zone"
		accountName = "waf-test-account"
		secretName  = "waf-test-api-token"
	)

	ctx := context.Background()
	var fake *fakeRulesetAPI
	var r *WAFCustomRuleReconciler

	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: ns} }

	createAccount := func(ready bool) {
		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "acct-waf",
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
			},
		}
		Expect(k8sClient.Create(ctx, acct)).To(Succeed())
		if ready {
			acct.Status.Conditions = []metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Validated", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, acct)).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
		})).To(Succeed())
	}

	// createZone creates a Zone in namespace; with a zone ID it is ready and
	// synced with that Cloudflare zone.
	createZone := func(namespace, zoneID string) {
		zone := &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: zoneCRName, Namespace: namespace},
			Spec: cloudflarev1alpha1.ZoneSpec{
				Name: "waf-example.com", AccountRef: corev1.LocalObjectReference{Name: accountName},
			},
		}
		Expect(k8sClient.Create(ctx, zone)).To(Succeed())
		if zoneID != "" {
			zone.Status.CloudflareMetadata.ZoneID = zoneID
			zone.Status.Conditions = []metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Synced", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, zone)).To(Succeed())
		}
	}

	// createRule creates a block rule for /<name> that already carries the
	// finalizer; change adjusts its spec first.
	createRule := func(namespace, name string, change func(*cloudflarev1alpha1.WAFCustomRuleSpec)) {
		rule := &cloudflarev1alpha1.WAFCustomRule{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.WAFCustomRuleSpec{
				RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
					ZoneRef:     corev1.LocalObjectReference{Name: zoneCRName},
					Expression:  `http.request.uri.path eq "/` + name + `"`,
					Description: "block /" + name,
				},
				Action: "block",
			},
		}
		if change != nil {
			change(&rule.Spec)
		}
		Expect(k8sClient.Create(ctx, rule)).To(Succeed())
	}

	getRule := func(k types.NamespacedName) *cloudflarev1alpha1.WAFCustomRule {
		rule := &cloudflarev1alpha1.WAFCustomRule{}
		Expect(k8sClient.Get(ctx, k, rule)).To(Succeed())
		return rule
	}
	readyCondition := func(k types.NamespacedName) metav1.Condition {
		cond := meta.FindStatusCondition(getRule(k).Status.Conditions, cloudflarev1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil(), "no Ready condition")
		return *cond
	}
	reconcileRule := func(k types.NamespacedName) (ctrl.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: k})
	}
	// cloudflareRule is the rule fake Cloudflare holds for the resource at k.
	cloudflareRule := func(k types.NamespacedName) fakeRule {
		rule, ok := fake.entrypoint().rule(rulesetRuleRef("WAFCustomRule", k.Namespace, k.Name))
		Expect(ok).To(BeTrue(), "Cloudflare has no rule for %s", k)
		return rule
	}
	// order lists the resources whose rules fake Cloudflare holds, in order.
	order := func() []string {
		var names []string
		for _, rule := range fake.entrypoint().rules {
			for _, k := range []types.NamespacedName{key("a"), key("b"), {Name: "c", Namespace: otherNS}} {
				if rule.ref() == rulesetRuleRef("WAFCustomRule", k.Namespace, k.Name) {
					names = append(names, k.Name)
				}
			}
		}
		return names
	}

	BeforeEach(func() {
		fake = newFakeRulesetAPI()
		r = &WAFCustomRuleReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			NewRulesetRuleAPI: func(string) (RulesetRuleAPI, error) {
				return fake.api(), nil
			},
		}
	})

	AfterEach(func() {
		for _, k := range []types.NamespacedName{key("a"), key("b"), {Name: "c", Namespace: otherNS}} {
			forceDelete(ctx, &cloudflarev1alpha1.WAFCustomRule{}, k)
		}
		forceDelete(ctx, &cloudflarev1alpha1.Zone{}, key(zoneCRName))
		forceDelete(ctx, &cloudflarev1alpha1.Zone{}, types.NamespacedName{Name: zoneCRName, Namespace: otherNS})
		forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
		forceDelete(ctx, &corev1.Secret{}, key(secretName))
	})

	readyEnv := func() {
		createAccount(true)
		createZone(ns, rulesTestZone)
	}

	Describe("Reconcile", func() {
		It("returns no error when the WAFCustomRule does not exist", func() {
			_, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
		})

		It("adds the finalizer on the first reconcile without calling Cloudflare", func() {
			fake.before = func(method string) { Fail("Cloudflare was called: " + method) }
			rule := &cloudflarev1alpha1.WAFCustomRule{
				ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: ns},
				Spec: cloudflarev1alpha1.WAFCustomRuleSpec{
					RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
						ZoneRef: corev1.LocalObjectReference{Name: zoneCRName}, Expression: "true",
					},
					Action: "block",
				},
			}
			Expect(k8sClient.Create(ctx, rule)).To(Succeed())
			_, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(getRule(key("a")).Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		DescribeTable("waits for its Zone without calling Cloudflare",
			func(setup func(), reason string) {
				fake.before = func(method string) { Fail("Cloudflare was called: " + method) }
				setup()
				createRule(ns, "a", nil)
				result, err := reconcileRule(key("a"))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.IsZero()).To(BeTrue(), "the Zone watch re-triggers this reconcile")
				Expect(readyCondition(key("a")).Status).To(Equal(metav1.ConditionFalse))
				Expect(readyCondition(key("a")).Reason).To(Equal(reason))
			},
			Entry("when the Zone is missing", func() {}, "ZoneNotFound"),
			Entry("when the Zone is not ready", func() {
				createAccount(true)
				createZone(ns, "")
			}, "ZoneNotReady"),
		)

		It("retries after a minute when the account is not ready", func() {
			createAccount(false)
			createZone(ns, rulesTestZone)
			createRule(ns, "a", nil)
			result, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(readyCondition(key("a")).Reason).To(Equal("AccountNotReady"))
		})

		It("reports InvalidToken when no Cloudflare client can be built", func() {
			readyEnv()
			createRule(ns, "a", nil)
			r.NewRulesetRuleAPI = func(string) (RulesetRuleAPI, error) { return RulesetRuleAPI{}, errors.New("bad token") }
			_, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(readyCondition(key("a")).Reason).To(Equal("InvalidToken"))
		})

		It("creates the zone's entry point holding the rule and reports the rule in status", func() {
			readyEnv()
			createRule(ns, "a", nil)
			result, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())

			rule := cloudflareRule(key("a"))
			Expect(rule).To(HaveKeyWithValue("action", "block"))
			Expect(rule).To(HaveKeyWithValue("expression", `http.request.uri.path eq "/a"`))
			Expect(rule).To(HaveKeyWithValue("description", "block /a"))
			Expect(rule).To(HaveKeyWithValue("enabled", true))

			Expect(getRule(key("a")).Status.CloudflareMetadata).To(Equal(cloudflarev1alpha1.RulesetRuleCloudflareMetadata{
				ZoneID:    rulesTestZone,
				RulesetID: fake.entrypoint().id,
				RuleID:    rule.id(),
				Ref:       rulesetRuleRef("WAFCustomRule", ns, "a"),
				Version:   "1",
			}))
			Expect(readyCondition(key("a")).Status).To(Equal(metav1.ConditionTrue))
		})

		It("writes nothing to Cloudflare once the rule is in sync", func() {
			readyEnv()
			createRule(ns, "a", nil)
			_, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			writes := fake.writes

			_, err = reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.writes).To(Equal(writes))
		})

		It("writes a skip rule with its sorted phases and products, then leaves it alone", func() {
			readyEnv()
			createRule(ns, "a", func(spec *cloudflarev1alpha1.WAFCustomRuleSpec) {
				spec.Action = "skip"
				spec.Skip = &cloudflarev1alpha1.WAFCustomRuleSkip{
					RemainingRules: true,
					Phases:         []cloudflarev1alpha1.WAFCustomRuleSkipPhase{"http_request_sbfm", "http_ratelimit"},
					Products:       []cloudflarev1alpha1.WAFCustomRuleSkipProduct{"waf", "bic"},
					Log:            new(false),
				}
			})
			_, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())

			rule := cloudflareRule(key("a"))
			Expect(rule).To(HaveKeyWithValue("action", "skip"))
			Expect(rule.jsonOf("action_parameters")).To(MatchJSON(
				`{"ruleset":"current","phases":["http_ratelimit","http_request_sbfm"],"products":["bic","waf"]}`))
			Expect(rule.jsonOf("logging")).To(MatchJSON(`{"enabled":false}`))

			writes := fake.writes
			_, err = reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.writes).To(Equal(writes))
		})

		It("rewrites a rule changed outside kflare, keeping its ID", func() {
			readyEnv()
			createRule(ns, "a", nil)
			_, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			ruleID := cloudflareRule(key("a")).id()
			for _, rule := range fake.entrypoint().rules {
				rule["action"] = "managed_challenge"
			}

			_, err = reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			rule := cloudflareRule(key("a"))
			Expect(rule.id()).To(Equal(ruleID))
			Expect(rule).To(HaveKeyWithValue("action", "block"))
			Expect(getRule(key("a")).Status.CloudflareMetadata.Version).To(Equal("2"))
		})

		It("disables the rule when the spec does", func() {
			readyEnv()
			createRule(ns, "a", nil)
			_, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())

			rule := getRule(key("a"))
			rule.Spec.Enabled = new(false)
			Expect(k8sClient.Update(ctx, rule)).To(Succeed())
			_, err = reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(cloudflareRule(key("a"))).To(HaveKeyWithValue("enabled", false))
		})

		It("keeps the rules of every namespace sharing the zone in priority order", func() {
			readyEnv()
			createZone(otherNS, rulesTestZone)
			createRule(ns, "a", func(spec *cloudflarev1alpha1.WAFCustomRuleSpec) { spec.Priority = 10 })
			createRule(ns, "b", func(spec *cloudflarev1alpha1.WAFCustomRuleSpec) { spec.Priority = 1 })
			createRule(otherNS, "c", func(spec *cloudflarev1alpha1.WAFCustomRuleSpec) { spec.Priority = 5 })

			for _, k := range []types.NamespacedName{key("a"), key("b"), {Name: "c", Namespace: otherNS}} {
				result, err := reconcileRule(k)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.IsZero()).To(BeTrue(), "a new rule is placed, not moved")
			}
			Expect(order()).To(Equal([]string{"b", "c", "a"}))

			By("moving the rules when a priority changes, and checking again shortly")
			rule := getRule(key("a"))
			rule.Spec.Priority = 0
			Expect(k8sClient.Update(ctx, rule)).To(Succeed())
			result, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(rulesetRuleRecheckInterval))
			Expect(order()).To(Equal([]string{"a", "b", "c"}))
			Expect(fake.writes).To(Equal(3), "moving rules does not rewrite them")
		})

		It("checks the order again shortly after moving rules, whatever the drift check interval", func() {
			readyEnv()
			createRule(ns, "a", func(spec *cloudflarev1alpha1.WAFCustomRuleSpec) { spec.Priority = 10 })
			createRule(ns, "b", func(spec *cloudflarev1alpha1.WAFCustomRuleSpec) { spec.Priority = 1 })
			for _, k := range []types.NamespacedName{key("a"), key("b")} {
				_, err := reconcileRule(k)
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(order()).To(Equal([]string{"b", "a"}))

			turnOnDriftChecks()
			rule := getRule(key("a"))
			rule.Spec.Priority = 0
			Expect(k8sClient.Update(ctx, rule)).To(Succeed())
			result, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(rulesetRuleRecheckInterval))
			Expect(order()).To(Equal([]string{"a", "b"}))

			By("going back to drift checks once the order holds")
			result, err = reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically("~", driftCheckTestInterval, driftCheckTestSpread))
		})

		It("leaves a paused rule where it is while it orders the others, and orders it again once unpaused", func() {
			readyEnv()
			createRule(ns, "a", func(spec *cloudflarev1alpha1.WAFCustomRuleSpec) { spec.Priority = 10 })
			createRule(ns, "b", func(spec *cloudflarev1alpha1.WAFCustomRuleSpec) { spec.Priority = 1 })
			for _, k := range []types.NamespacedName{key("a"), key("b")} {
				_, err := reconcileRule(k)
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(order()).To(Equal([]string{"b", "a"}))

			By("keeping b after a, where it was moved outside kflare while paused")
			setPaused(ctx, getRule(key("b")), true)
			rules := fake.entrypoint().rules
			rules[0], rules[1] = rules[1], rules[0]
			result, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue(), "nothing was moved")
			Expect(order()).To(Equal([]string{"a", "b"}))
			Expect(fake.moves).To(BeZero())

			By("moving the rules back into order once b is unpaused")
			setPaused(ctx, getRule(key("b")), false)
			result, err = reconcileRule(key("b"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(rulesetRuleRecheckInterval))
			Expect(order()).To(Equal([]string{"b", "a"}))
		})

		It("sets TerminalError without requeueing when Cloudflare rejects the rule", func() {
			readyEnv()
			createRule(ns, "a", nil)
			fake.maxRules = 0
			result, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(readyCondition(key("a")).Reason).To(Equal("TerminalError"))
		})

		It("returns retryable errors for back-off", func() {
			readyEnv()
			createRule(ns, "a", nil)
			fake.errs["Phases.Get"] = errors.New("connection reset")
			_, err := reconcileRule(key("a"))
			Expect(err).To(MatchError("connection reset"))
			Expect(readyCondition(key("a")).Reason).To(Equal("APIError"))
		})
	})

	Describe("deletion", func() {
		// syncedRule creates rule a and lets the controller write it.
		syncedRule := func(annotations map[string]string) {
			readyEnv()
			createRule(ns, "a", nil)
			_, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			rule := getRule(key("a"))
			if annotations != nil {
				rule.Annotations = annotations
				Expect(k8sClient.Update(ctx, rule)).To(Succeed())
			}
			Expect(k8sClient.Delete(ctx, rule)).To(Succeed())
		}
		expectGone := func() {
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key("a"), &cloudflarev1alpha1.WAFCustomRule{}))).To(BeTrue())
		}

		It("deletes the rule from Cloudflare and removes the finalizer", func() {
			syncedRule(nil)
			_, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.entrypoint().rules).To(BeEmpty())
			expectGone()
		})

		It("keeps the rule in Cloudflare with the retain policy", func() {
			syncedRule(map[string]string{reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain})
			_, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.entrypoint().rules).To(HaveLen(1))
			expectGone()
		})

		It("finds the rule by its ref when its status was never written", func() {
			syncedRule(nil)
			rule := getRule(key("a"))
			rule.Status.CloudflareMetadata = cloudflarev1alpha1.RulesetRuleCloudflareMetadata{}
			Expect(k8sClient.Status().Update(ctx, rule)).To(Succeed())
			_, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.entrypoint().rules).To(BeEmpty())
			expectGone()
		})

		It("skips Cloudflare when the Zone resource is already gone", func() {
			syncedRule(nil)
			forceDelete(ctx, &cloudflarev1alpha1.Zone{}, key(zoneCRName))
			_, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.entrypoint().rules).To(HaveLen(1))
			expectGone()
		})

		It("skips Cloudflare for a rule that never reached it", func() {
			createAccount(true)
			createZone(ns, "")
			createRule(ns, "a", nil)
			fake.before = func(method string) { Fail("Cloudflare was called: " + method) }
			Expect(k8sClient.Delete(ctx, getRule(key("a")))).To(Succeed())
			_, err := reconcileRule(key("a"))
			Expect(err).NotTo(HaveOccurred())
			expectGone()
		})

		It("keeps the finalizer when Cloudflare fails, so deletion is retried", func() {
			syncedRule(nil)
			fake.errs["Rules.Delete"] = errors.New("unavailable")
			_, err := reconcileRule(key("a"))
			Expect(err).To(MatchError("unavailable"))
			Expect(getRule(key("a")).Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("keeps the finalizer when the credentials cannot be resolved or used", func() {
			syncedRule(nil)
			r.NewRulesetRuleAPI = func(string) (RulesetRuleAPI, error) { return RulesetRuleAPI{}, errors.New("bad token") }
			_, err := reconcileRule(key("a"))
			Expect(err).To(MatchError("bad token"))

			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
			_, err = reconcileRule(key("a"))
			Expect(err).To(HaveOccurred())
			Expect(getRule(key("a")).Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("does nothing for a rule without the finalizer", func() {
			result, err := r.rules().reconcileDelete(ctx, &cloudflarev1alpha1.WAFCustomRule{})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
		})
	})

	Describe("Validation", func() {
		create := func(change func(*cloudflarev1alpha1.WAFCustomRule)) error {
			rule := &cloudflarev1alpha1.WAFCustomRule{
				ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: ns},
				Spec: cloudflarev1alpha1.WAFCustomRuleSpec{
					RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
						ZoneRef: corev1.LocalObjectReference{Name: zoneCRName}, Expression: "true",
					},
					Action: "block",
				},
			}
			change(rule)
			return k8sClient.Create(ctx, rule)
		}
		skipping := func(skip cloudflarev1alpha1.WAFCustomRuleSkip) func(*cloudflarev1alpha1.WAFCustomRule) {
			return func(rule *cloudflarev1alpha1.WAFCustomRule) {
				rule.Spec.Action = "skip"
				rule.Spec.Skip = &skip
			}
		}

		It("applies the defaults", func() {
			Expect(create(skipping(cloudflarev1alpha1.WAFCustomRuleSkip{RemainingRules: true}))).To(Succeed())
			rule := getRule(key("a"))
			Expect(*rule.Spec.Enabled).To(BeTrue())
			Expect(*rule.Spec.Skip.Log).To(BeTrue())
			forceDelete(ctx, &cloudflarev1alpha1.WAFCustomRule{}, key("a"))

			Expect(create(func(rule *cloudflarev1alpha1.WAFCustomRule) {
				rule.Spec.BlockResponse = &cloudflarev1alpha1.WAFCustomRuleBlockResponse{
					ContentType: "text/plain", Content: "blocked",
				}
			})).To(Succeed())
			Expect(getRule(key("a")).Spec.BlockResponse.StatusCode).To(BeEquivalentTo(403))
		})

		It("rejects changing spec.zoneRef", func() {
			Expect(create(func(*cloudflarev1alpha1.WAFCustomRule) {})).To(Succeed())
			rule := getRule(key("a"))
			rule.Spec.ZoneRef.Name = "other-zone"
			Expect(k8sClient.Update(ctx, rule)).To(MatchError(ContainSubstring("zoneRef is immutable")))
		})

		DescribeTable("rejects an invalid spec",
			func(change func(*cloudflarev1alpha1.WAFCustomRule), message string) {
				Expect(create(change)).To(MatchError(ContainSubstring(message)))
			},
			Entry("an unknown action", func(rule *cloudflarev1alpha1.WAFCustomRule) {
				rule.Spec.Action = "allow"
			}, "spec.action"),
			Entry("an empty expression", func(rule *cloudflarev1alpha1.WAFCustomRule) {
				rule.Spec.Expression = ""
			}, "spec.expression"),
			Entry("an expression over 4096 characters", func(rule *cloudflarev1alpha1.WAFCustomRule) {
				rule.Spec.Expression = strings.Repeat("a", 4097)
			}, "spec.expression"),
			Entry("a skip rule without skip", func(rule *cloudflarev1alpha1.WAFCustomRule) {
				rule.Spec.Action = "skip"
			}, "skip is required with action skip"),
			Entry("skip on a block rule", func(rule *cloudflarev1alpha1.WAFCustomRule) {
				rule.Spec.Skip = &cloudflarev1alpha1.WAFCustomRuleSkip{RemainingRules: true}
			}, "skip is required with action skip and allowed only with it"),
			Entry("a skip that skips nothing", skipping(cloudflarev1alpha1.WAFCustomRuleSkip{}),
				"skip needs remainingRules, phases or products"),
			Entry("an unknown phase", skipping(cloudflarev1alpha1.WAFCustomRuleSkip{
				Phases: []cloudflarev1alpha1.WAFCustomRuleSkipPhase{"http_request_firewall_custom"},
			}), "spec.skip.phases"),
			Entry("an unknown product", skipping(cloudflarev1alpha1.WAFCustomRuleSkip{
				Products: []cloudflarev1alpha1.WAFCustomRuleSkipProduct{"nope"},
			}), "spec.skip.products"),
			Entry("a block response on a challenge rule", func(rule *cloudflarev1alpha1.WAFCustomRule) {
				rule.Spec.Action = "managed_challenge"
				rule.Spec.BlockResponse = &cloudflarev1alpha1.WAFCustomRuleBlockResponse{
					ContentType: "text/plain", Content: "blocked",
				}
			}, "blockResponse is allowed only with action block"),
			Entry("a block response status outside 400-499", func(rule *cloudflarev1alpha1.WAFCustomRule) {
				rule.Spec.BlockResponse = &cloudflarev1alpha1.WAFCustomRuleBlockResponse{
					StatusCode: 302, ContentType: "text/plain", Content: "blocked",
				}
			}, "spec.blockResponse.statusCode"),
			Entry("a block response with an unknown content type", func(rule *cloudflarev1alpha1.WAFCustomRule) {
				rule.Spec.BlockResponse = &cloudflarev1alpha1.WAFCustomRuleBlockResponse{
					ContentType: "image/png", Content: "blocked",
				}
			}, "spec.blockResponse.contentType"),
			Entry("a block response body over 2 KB", func(rule *cloudflarev1alpha1.WAFCustomRule) {
				rule.Spec.BlockResponse = &cloudflarev1alpha1.WAFCustomRuleBlockResponse{
					ContentType: "text/plain", Content: strings.Repeat("a", 2049),
				}
			}, "spec.blockResponse.content"),
		)
	})
})

var _ = Describe("WAFCustomRule peers", func() {
	ctx := context.Background()

	// createRule creates a WAFCustomRule in namespace for the Zone zoneName.
	createRule := func(namespace, name, zoneName string, priority int32) {
		rule := &cloudflarev1alpha1.WAFCustomRule{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.WAFCustomRuleSpec{
				RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
					ZoneRef: corev1.LocalObjectReference{Name: zoneName}, Expression: "true", Priority: priority,
				},
				Action: "block",
			},
		}
		Expect(k8sClient.Create(ctx, rule)).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.WAFCustomRule{}, client.ObjectKeyFromObject(rule))
	}
	// createZone creates a Zone in namespace whose status names zoneID.
	createZone := func(namespace, name, zoneID string) {
		zone := &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: cloudflarev1alpha1.ZoneSpec{
				Name: "peers-example.com", AccountRef: corev1.LocalObjectReference{Name: "peers-account"},
			},
		}
		Expect(k8sClient.Create(ctx, zone)).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.Zone{}, client.ObjectKeyFromObject(zone))
		zone.Status.CloudflareMetadata.ZoneID = zoneID
		Expect(k8sClient.Status().Update(ctx, zone)).To(Succeed())
	}

	It("are the kind's rules for one Cloudflare zone in every namespace, but not those being deleted or paused", func() {
		createZone("default", "peers-zone", "zone-peers")
		createZone("kube-public", "peers-zone", "zone-peers")
		createZone("default", "peers-other-zone", "zone-elsewhere")
		createRule("default", "peers-a", "peers-zone", 3)
		createRule("kube-public", "peers-b", "peers-zone", 1)
		createRule("default", "peers-elsewhere", "peers-other-zone", 1)
		createRule("default", "peers-no-zone", "peers-missing-zone", 1)
		createRule("default", "peers-deleting", "peers-zone", 1)
		Expect(k8sClient.Delete(ctx, &cloudflarev1alpha1.WAFCustomRule{
			ObjectMeta: metav1.ObjectMeta{Name: "peers-deleting", Namespace: "default"},
		})).To(Succeed())
		createRule("default", "peers-paused", "peers-zone", 1)
		setPaused(ctx, &cloudflarev1alpha1.WAFCustomRule{
			ObjectMeta: metav1.ObjectMeta{Name: "peers-paused", Namespace: "default"},
		}, true)

		rules := (&WAFCustomRuleReconciler{Client: k8sClient}).rules()
		peers, err := rules.peers(ctx, "zone-peers", rulesets.PhaseHTTPRequestFirewallCustom)
		Expect(err).NotTo(HaveOccurred())
		Expect(peers).To(ConsistOf(
			rulesetRulePeer{
				ref: rulesetRuleRef("WAFCustomRule", "default", "peers-a"), priority: 3,
				namespace: "default", name: "peers-a", kind: "WAFCustomRule",
			},
			rulesetRulePeer{
				ref: rulesetRuleRef("WAFCustomRule", "kube-public", "peers-b"), priority: 1,
				namespace: "kube-public", name: "peers-b", kind: "WAFCustomRule",
			},
		))
	})

	It("returns the list error", func() {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		rules := (&WAFCustomRuleReconciler{Client: k8sClient}).rules()
		_, err := rules.peers(cancelled, "zone-peers", rulesets.PhaseHTTPRequestFirewallCustom)
		Expect(err).To(HaveOccurred())
	})
})
