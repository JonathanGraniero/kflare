/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go/v7/rulesets"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// The specs every generated rule kind runs. kflare-gen writes a table per rule
// kind (zz_generated.<kind>_test.go): its samples, each with the phase and the
// rule it asks for, edits of them, and a spec that sets every field.
// describeRuleKind runs them against fakeRulesetAPI: each sample's rule is
// written to its phase, as the generated conversion builds it; an idle
// reconcile writes nothing; each change updates the rule in place; deletion
// and the retain policy, and in a phase whose emptied entry point kflare
// deletes, the entry point and the peers; the Zone waiting for the kind's
// resources; every
// field converted; and the CRD: the immutable zoneRef and type, and the specs
// it refuses. The shared rule controller's other paths (waiting for the Zone,
// credentials, errors, ordering) are the WAFCustomRule specs'.

// ruleKindReconciler is a generated rule kind's reconciler.
type ruleKindReconciler interface {
	reconcile.Reconciler
	SetupWithManager(mgr ctrl.Manager) error
	rules() *rulesetRuleReconciler
}

// ruleSpecCase is one spec of a generated table.
type ruleSpecCase struct {
	what string
	// spec is the spec as JSON, zoneRef aside, which the specs set.
	spec string
	// sample is the sample whose resource a change edits.
	sample int
	// phase is the phase a sample's rule goes in. rule is the rule a sample
	// or a change asks for, as Cloudflare stores it, its ref aside.
	phase rulesets.Phase
	rule  string
	// message is what the CRD refuses an invalid spec, or a change of type,
	// with.
	message string
}

// ruleKindSpecs is a generated rule kind's table.
type ruleKindSpecs[O rulesetRuleObject] struct {
	kind          rulesetRuleKind
	newObject     func() O
	newReconciler func(c client.Client, newAPI func(string) (RulesetRuleAPI, error)) ruleKindReconciler
	// samples are valid specs: the entry's samples, one per phase, then its
	// tests; changes edit them; retypes are specs of another type, which the
	// first sample's resource may not take; invalid are specs the CRD refuses.
	samples, changes, retypes, invalid []ruleSpecCase
	// full are specs that set every field of the action parameters, which
	// the specs convert without the API server, which may refuse them: they
	// can hold several variants of a union at once.
	full []ruleSpecCase
	// conflicts are the entry's conflicts, the condition reason of each
	// error code: the table's own copy, which the kind's must match.
	conflicts map[int]string
}

// comparableStoredRule is a rule as fakeRulesetAPI stores it, without what
// Cloudflare sets or kflare derives (its ID, version, update time and ref) and
// without empty values, which Cloudflare treats as unset; what Cloudflare
// fills in for a rule that leaves it out counts as set.
func comparableStoredRule(rule fakeRule) any {
	out := rule.clone()
	out.fillDefaults()
	for _, key := range []string{"id", "version", "last_updated", "ref"} {
		delete(out, key)
	}
	return withoutEmpty(map[string]any(out))
}

// comparableJSONRule is a rule a table holds as JSON, as comparableStoredRule
// shows a stored one.
func comparableJSONRule(rule string) any {
	var out fakeRule
	Expect(json.Unmarshal([]byte(rule), &out)).To(Succeed())
	return comparableStoredRule(out)
}

func describeRuleKind[O rulesetRuleObject](specs ruleKindSpecs[O]) bool {
	// The kind as its API type names it, which the specs expect refs and
	// objects to carry; one spec checks that the rule kind goes by it too.
	kind := reflect.TypeOf(specs.newObject()).Elem().Name()
	return Describe(kind+" (generated rule kind)", func() {
		const ns = "default"
		ctx := context.Background()
		prefix := strings.ToLower(kind)
		zoneName, accountName, secretName := prefix+"-zone", prefix+"-account", prefix+"-api-token"
		var (
			fake *fakeRulesetAPI
			r    ruleKindReconciler
		)

		// create creates obj and deletes it again after the spec.
		create := func(obj client.Object) {
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(forceDelete, ctx, obj.DeepCopyObject().(client.Object), client.ObjectKeyFromObject(obj))
		}

		BeforeEach(func() {
			fake = newFakeRulesetAPI()
			r = specs.newReconciler(k8sClient, func(string) (RulesetRuleAPI, error) { return fake.api(), nil })

			create(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
				Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
			})
			account := &cloudflarev1alpha1.CloudflareAccount{
				ObjectMeta: metav1.ObjectMeta{Name: accountName},
				Spec: cloudflarev1alpha1.CloudflareAccountSpec{
					AccountID:      "acct-" + prefix,
					TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
				},
			}
			create(account)
			account.Status.Conditions = []metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Validated", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, account)).To(Succeed())
			zone := &cloudflarev1alpha1.Zone{
				ObjectMeta: metav1.ObjectMeta{Name: zoneName, Namespace: ns},
				Spec: cloudflarev1alpha1.ZoneSpec{
					Name: prefix + ".example.com", AccountRef: corev1.LocalObjectReference{Name: accountName},
				},
			}
			create(zone)
			zone.Status.CloudflareMetadata.ZoneID = rulesTestZone
			zone.Status.Conditions = []metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Synced", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, zone)).To(Succeed())
		})

		key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: ns} }

		// object returns a resource named name, in the Zone's namespace and
		// for it, with the spec a table holds as JSON. An unknown field in the
		// JSON fails the spec: the table must name the API types' fields.
		object := func(name, spec string) O {
			obj := specs.newObject()
			decoder := json.NewDecoder(strings.NewReader(`{"spec":` + spec + `}`))
			decoder.DisallowUnknownFields()
			Expect(decoder.Decode(obj)).To(Succeed(), "the table's spec does not fit the API types")
			obj.SetName(name)
			obj.SetNamespace(ns)
			obj.GetRulesetRuleSpec().ZoneRef.Name = zoneName
			return obj
		}
		// rawObject returns the resource that object returns as JSON, which the
		// API server takes unchanged: an empty value or list that the API types
		// leave out stays in.
		rawObject := func(name, spec string) *unstructured.Unstructured {
			object(name, spec)
			raw := &unstructured.Unstructured{}
			Expect(raw.UnmarshalJSON(fmt.Appendf(nil,
				`{"apiVersion":%q,"kind":%q,"metadata":{"name":%q,"namespace":%q},"spec":%s}`,
				cloudflarev1alpha1.GroupVersion.String(), kind, name, ns, spec))).To(Succeed())
			Expect(unstructured.SetNestedField(raw.Object, zoneName, "spec", "zoneRef", "name")).To(Succeed())
			return raw
		}
		// get reads the resource name.
		get := func(name string) O {
			obj := specs.newObject()
			Expect(k8sClient.Get(ctx, key(name), obj)).To(Succeed())
			return obj
		}
		// createSynced creates a resource named name with spec, carrying the
		// finalizer, and reconciles it once.
		createSynced := func(name, spec string) O {
			obj := object(name, spec)
			obj.SetFinalizers([]string{reconciler.Finalizer})
			create(obj)
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			return get(name)
		}
		// replaceSpec replaces the spec of the resource name with spec.
		replaceSpec := func(name, spec string) error {
			current := get(name)
			updated := object(name, spec)
			updated.SetResourceVersion(current.GetResourceVersion())
			updated.SetFinalizers(current.GetFinalizers())
			return k8sClient.Update(ctx, updated)
		}
		// storedRule returns the rule fake Cloudflare holds for the resource
		// name in phase.
		storedRule := func(phase rulesets.Phase, name string) fakeRule {
			rule, ok := fake.entrypointIn(phase).rule(rulesetRuleRef(kind, ns, name))
			Expect(ok).To(BeTrue(), "Cloudflare has no rule for %s in %s", name, phase)
			return rule
		}
		// holdsRule reports whether fake Cloudflare holds a rule for the
		// resource name in phase, whose entry point may be gone.
		holdsRule := func(phase rulesets.Phase, name string) bool {
			entrypoint, ok := fake.entrypoints[phase]
			if !ok {
				return false
			}
			_, ok = entrypoint.rule(rulesetRuleRef(kind, ns, name))
			return ok
		}

		It("writes each sample's rule to the phase its spec picks, and reports it in status", func() {
			for i, sample := range specs.samples {
				By(sample.what)
				name := fmt.Sprintf("sample-%d", i)
				obj := createSynced(name, sample.spec)
				Expect(specs.kind.phase(obj)).To(Equal(sample.phase))
				rule := storedRule(sample.phase, name)
				Expect(comparableStoredRule(rule)).To(Equal(comparableJSONRule(sample.rule)))

				ready := meta.FindStatusCondition(obj.GetConditions(), cloudflarev1alpha1.ConditionReady)
				Expect(ready).NotTo(BeNil())
				Expect(ready.Status).To(Equal(metav1.ConditionTrue))
				Expect(obj.GetRulesetRuleStatus().CloudflareMetadata).To(Equal(cloudflarev1alpha1.RulesetRuleCloudflareMetadata{
					ZoneID:    rulesTestZone,
					RulesetID: fake.entrypointIn(sample.phase).id,
					RuleID:    rule.id(),
					Ref:       rulesetRuleRef(kind, ns, name),
					Version:   "1",
				}))
			}

			By("keeping each phase's rules apart")
			for i, sample := range specs.samples {
				name := fmt.Sprintf("sample-%d", i)
				Expect(refs(fake.entrypointIn(sample.phase).rules)).To(ContainElement(rulesetRuleRef(kind, ns, name)))
				peers, err := r.rules().peers(ctx, rulesTestZone, sample.phase)
				Expect(err).NotTo(HaveOccurred())
				var names []string
				for _, peer := range peers {
					if peer.kind == kind {
						Expect(specs.kind.phase(get(peer.name))).To(Equal(sample.phase))
						names = append(names, peer.name)
					}
				}
				Expect(names).To(ContainElement(name), "the rule is not a peer in its own phase")
			}
		})

		It("writes nothing to Cloudflare once a sample's rule is in sync", func() {
			for i, sample := range specs.samples {
				name := fmt.Sprintf("idle-%d", i)
				createSynced(name, sample.spec)
				writes := fake.writes
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
				Expect(err).NotTo(HaveOccurred())
				Expect(fake.writes).To(Equal(writes), "%s: an idle reconcile wrote to Cloudflare", sample.what)
			}
		})

		It("schedules a drift check after a sync, once the manager sets an interval", func() {
			createSynced("drift-check", specs.samples[0].spec)
			writes := fake.writes
			expectDriftCheck(func() (reconcile.Result, error) {
				return r.Reconcile(ctx, reconcile.Request{NamespacedName: key("drift-check")})
			})
			Expect(fake.writes).To(Equal(writes))
		})

		It("updates the rule in place when the spec changes", func() {
			for i, change := range specs.changes {
				By(change.what)
				name := fmt.Sprintf("change-%d", i)
				sample := specs.samples[change.sample]
				createSynced(name, sample.spec)
				ruleID := storedRule(sample.phase, name).id()

				Expect(replaceSpec(name, change.spec)).To(Succeed())
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
				Expect(err).NotTo(HaveOccurred())
				rule := storedRule(sample.phase, name)
				Expect(rule.id()).To(Equal(ruleID))
				Expect(comparableStoredRule(rule)).To(Equal(comparableJSONRule(change.rule)))
				Expect(get(name).GetRulesetRuleStatus().CloudflareMetadata.Version).To(Equal("2"))

				// The resource goes again, so that a kind with many changes in a
				// phase stays within its quota.
				Expect(k8sClient.Delete(ctx, get(name))).To(Succeed())
				_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
				Expect(err).NotTo(HaveOccurred())
				Expect(holdsRule(sample.phase, name)).To(BeFalse())
			}
		})

		It("deletes the rule with the resource, and keeps it with the retain policy", func() {
			for i, sample := range specs.samples {
				By(sample.what)
				for _, retain := range []bool{false, true} {
					name := fmt.Sprintf("deleted-%d-%t", i, retain)
					obj := createSynced(name, sample.spec)
					if retain {
						obj.SetAnnotations(map[string]string{
							reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain,
						})
						Expect(k8sClient.Update(ctx, obj)).To(Succeed())
					}
					Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
					_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
					Expect(err).NotTo(HaveOccurred())
					Expect(holdsRule(sample.phase, name)).To(Equal(retain))
					Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name), specs.newObject()))).To(BeTrue())
				}
			}
		})

		// In a phase whose emptied entry point changes what the zone runs
		// (emptiedEntrypointPhases), a deletion that empties the entry point
		// deletes it, unless another resource is about to write to it, and
		// re-triggers the resources that may have lost their rule with it.
		var emptiedSamples []ruleSpecCase
		for _, sample := range specs.samples {
			if emptiedEntrypointPhases[sample.phase] {
				emptiedSamples = append(emptiedSamples, sample)
			}
		}
		if len(emptiedSamples) > 0 {
			reconcileDeleted := func(obj O) {
				GinkgoHelper()
				Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)})
				Expect(err).NotTo(HaveOccurred())
			}
			hasEntrypoint := func(phase rulesets.Phase) bool {
				_, ok := fake.entrypoints[phase]
				return ok
			}

			It("deletes the entry point its deletion empties, but not while another resource is to write to it", func() {
				for i, sample := range emptiedSamples {
					By(sample.what)
					reconcileDeleted(createSynced(fmt.Sprintf("emptying-%d", i), sample.spec))
					Expect(hasEntrypoint(sample.phase)).To(BeFalse())

					By("keeping the entry point for a resource not synced yet")
					first, pendingName := createSynced(fmt.Sprintf("first-%d", i), sample.spec), fmt.Sprintf("pending-%d", i)
					pending := object(pendingName, sample.spec)
					pending.SetFinalizers([]string{reconciler.Finalizer})
					create(pending)
					reconcileDeleted(first)
					Expect(hasEntrypoint(sample.phase)).To(BeTrue())
					Expect(fake.entrypointIn(sample.phase).rules).To(BeEmpty())
					_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key(pendingName)})
					Expect(err).NotTo(HaveOccurred())
					Expect(holdsRule(sample.phase, pendingName)).To(BeTrue())
					reconcileDeleted(get(pendingName))
				}
			})

			It("re-triggers the resources that may have lost their rule when one is deleted", func() {
				sample := emptiedSamples[0]
				createSynced("synced", sample.spec)
				for _, name := range []string{"pending", "elsewhere"} {
					create(object(name, sample.spec))
				}
				elsewhere := get("elsewhere")
				elsewhere.GetRulesetRuleStatus().CloudflareMetadata.ZoneID = "zone-elsewhere"
				Expect(k8sClient.Status().Update(ctx, elsewhere)).To(Succeed())

				deleted := specs.newObject()
				deleted.SetName("gone")
				deleted.SetNamespace(ns)
				deleted.GetRulesetRuleStatus().CloudflareMetadata.ZoneID = rulesTestZone
				Expect(r.rules().rulesInZoneOfDeleted(ctx, deleted)).To(ConsistOf(
					reconcile.Request{NamespacedName: key("synced")},
					reconcile.Request{NamespacedName: key("pending")},
				))

				By("re-triggering every resource when the deleted one never synced")
				deleted.GetRulesetRuleStatus().CloudflareMetadata.ZoneID = ""
				Expect(r.rules().rulesInZoneOfDeleted(ctx, deleted)).To(HaveLen(3))
				Expect(r.rules().rulesInZoneOfDeleted(ctx, &cloudflarev1alpha1.Zone{})).To(BeEmpty())

				By("watching the deletions of every kind in the phase, each once")
				kinds := specs.kind.deletionsWatched()
				watched := make([]string, 0, len(kinds))
				for _, k := range kinds {
					watched = append(watched, k.name)
				}
				var inPhase []string
				for _, k := range rulesetRuleKinds {
					if slices.Contains(k.phases, sample.phase) {
						inPhase = append(inPhase, k.name)
					}
				}
				Expect(watched).To(ConsistOf(inPhase))
				Expect(watched).To(ContainElement(kind))
			})
		} else {
			It("watches the deletions of no kind, outside a phase whose emptied entry point kflare deletes", func() {
				Expect(specs.kind.deletionsWatched()).To(BeEmpty())
			})
		}

		// reconcilePaused reconciles the resource name, and checks that it made
		// no Cloudflare call and scheduled nothing.
		reconcilePaused := func(name string) {
			GinkgoHelper()
			calls := 0
			fake.before = func(string) { calls++ }
			defer func() { fake.before = nil }()
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(calls).To(BeZero(), "a paused resource called Cloudflare")
		}
		// readyOf returns the Ready condition of the resource name.
		readyOf := func(name string) metav1.Condition {
			GinkgoHelper()
			condition := meta.FindStatusCondition(get(name).GetConditions(), cloudflarev1alpha1.ConditionReady)
			Expect(condition).NotTo(BeNil())
			return *condition
		}

		It("calls Cloudflare for nothing while paused, and writes the changed rule once unpaused", func() {
			sample, spec, rule := specs.samples[0], specs.samples[0].spec, specs.samples[0].rule
			if len(specs.changes) > 0 {
				change := specs.changes[0]
				sample, spec, rule = specs.samples[change.sample], change.spec, change.rule
			}
			createSynced("paused", sample.spec)
			observed := readyOf("paused").ObservedGeneration
			Expect(replaceSpec("paused", spec)).To(Succeed())
			setPaused(ctx, get("paused"), true)
			reconcilePaused("paused")
			Expect(readyOf("paused").Status).To(Equal(metav1.ConditionTrue))
			Expect(readyOf("paused").Reason).To(Equal(reasonPaused))
			Expect(readyOf("paused").ObservedGeneration).To(Equal(observed), "the spec changed while paused is not observed")
			Expect(comparableStoredRule(storedRule(sample.phase, "paused"))).To(Equal(comparableJSONRule(sample.rule)),
				"the rule is as the last sync left it")

			By("writing the changed rule once unpaused")
			setPaused(ctx, get("paused"), false)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key("paused")})
			Expect(err).NotTo(HaveOccurred())
			Expect(comparableStoredRule(storedRule(sample.phase, "paused"))).To(Equal(comparableJSONRule(rule)))
			Expect(readyOf("paused").Reason).To(Equal("Synced"))
		})

		It("keeps a paused resource's rule and finalizer when it is deleted, and deletes both once unpaused", func() {
			sample := specs.samples[0]
			createSynced("paused-deleted", sample.spec)
			setPaused(ctx, get("paused-deleted"), true)
			Expect(k8sClient.Delete(ctx, get("paused-deleted"))).To(Succeed())
			reconcilePaused("paused-deleted")
			Expect(holdsRule(sample.phase, "paused-deleted")).To(BeTrue())
			Expect(get("paused-deleted").GetFinalizers()).To(ContainElement(reconciler.Finalizer))
			Expect(readyOf("paused-deleted").Reason).To(Equal(reasonPaused))
			Expect(readyOf("paused-deleted").Message).To(Equal(pausedDeletionMessage))

			By("deleting both once unpaused")
			setPaused(ctx, get("paused-deleted"), false)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key("paused-deleted")})
			Expect(err).NotTo(HaveOccurred())
			Expect(holdsRule(sample.phase, "paused-deleted")).To(BeFalse())
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key("paused-deleted"), specs.newObject()))).To(BeTrue())
		})

		It("adds no finalizer to a resource created paused", func() {
			obj := object("created-paused", specs.samples[0].spec)
			obj.SetAnnotations(map[string]string{reconciler.PausedAnnotation: "true"})
			create(obj)
			reconcilePaused("created-paused")
			Expect(get("created-paused").GetFinalizers()).To(BeEmpty())
			Expect(readyOf("created-paused").Status).To(Equal(metav1.ConditionUnknown))
			Expect(readyOf("created-paused").Reason).To(Equal(reasonPaused))
		})

		It("keeps zoneRef and the type immutable", func() {
			obj := object("immutable", specs.samples[0].spec)
			create(obj)
			obj.GetRulesetRuleSpec().ZoneRef.Name = "another-zone"
			Expect(k8sClient.Update(ctx, obj)).To(MatchError(ContainSubstring("zoneRef is immutable")))
			for _, retype := range specs.retypes {
				By(retype.what)
				Expect(replaceSpec("immutable", retype.spec)).To(MatchError(ContainSubstring(retype.message)))
			}
		})

		It("makes its Zone wait for it before the Zone is deleted, and releases the Zone once it is gone", func() {
			create(object("dependent", specs.samples[0].spec))
			dependents, err := dependentsOf(ctx, k8sClient, ns, zoneName, zoneDependents)
			Expect(err).NotTo(HaveOccurred())
			Expect(dependents).To(ContainElement(kind + " " + ns + "/dependent"))

			By("running the Zone controller, which must hear of the " + kind + "'s deletion")
			mgr, err := manager.New(cfg, manager.Options{
				Controller: config.Controller{SkipNameValidation: new(true)},
				Metrics:    metricsserver.Options{BindAddress: "0"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect((&ZoneReconciler{
				Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
				NewZoneAPI: func(string) (ZoneAPI, error) { return nil, errors.New("this spec calls no Cloudflare API") },
			}).SetupWithManager(mgr)).To(Succeed())
			mgrCtx, stop := context.WithCancel(ctx)
			stopped := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				defer close(stopped)
				Expect(mgr.Start(mgrCtx)).To(Succeed())
			}()
			// Stop it before the next spec, which may reconcile Zones itself.
			DeferCleanup(func() {
				stop()
				Eventually(stopped).Should(BeClosed())
			})

			By("deleting the Zone, retained, which waits for the " + kind)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, key(zoneName), zone)).To(Succeed())
			patch := client.MergeFrom(zone.DeepCopy())
			zone.Finalizers = append(zone.Finalizers, reconciler.Finalizer)
			zone.Annotations = map[string]string{reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain}
			Expect(k8sClient.Patch(ctx, zone, patch)).To(Succeed())
			Expect(k8sClient.Delete(ctx, zone)).To(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, key(zoneName), zone)).To(Succeed())
				ready := meta.FindStatusCondition(zone.Status.Conditions, cloudflarev1alpha1.ConditionReady)
				g.Expect(ready).NotTo(BeNil())
				g.Expect(ready.Reason).To(Equal("InUse"))
			}).WithTimeout(10 * time.Second).Should(Succeed())

			By("deleting the " + kind + ", which releases the Zone")
			Expect(k8sClient.Delete(ctx, get("dependent"))).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, key(zoneName), &cloudflarev1alpha1.Zone{}))
			}).WithTimeout(10*time.Second).Should(BeTrue(), "the Zone was not re-triggered by the %s's deletion", kind)
		})

		It("converts specs that set every field into the rules they ask for", func() {
			Expect(specs.full).NotTo(BeEmpty())
			for i, full := range specs.full {
				By(full.what)
				rule := specs.kind.definition(object(fmt.Sprintf("full-%d", i), full.spec))
				Expect(comparableStoredRule(sent(rule))).To(Equal(comparableJSONRule(full.rule)))
			}
		})

		It("reports each conflict Cloudflare refuses its rule with, and writes the rule once it is gone", func() {
			Expect(specs.kind.conflicts).To(Equal(specs.conflicts), "the kind's conflicts are not its entry's")
			sample := specs.samples[0]
			for _, code := range slices.Sorted(maps.Keys(specs.conflicts)) {
				reason := specs.conflicts[code]
				By(reason)
				name := fmt.Sprintf("conflict-%d", code)
				refusal := v7APIError(http.MethodPost, "zones/"+rulesTestZone+"/rulesets", http.StatusBadRequest, code,
					"refused by the fake")
				fake.errs["Rulesets.New"], fake.errs["Rules.New"] = refusal, refusal
				obj := object(name, sample.spec)
				obj.SetFinalizers([]string{reconciler.Finalizer})
				create(obj)
				result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
				ready := meta.FindStatusCondition(get(name).GetConditions(), cloudflarev1alpha1.ConditionReady)
				Expect(ready).NotTo(BeNil())
				Expect(ready.Status).To(Equal(metav1.ConditionFalse))
				Expect(ready.Reason).To(Equal(reason))
				Expect(ready.Message).To(Equal(fmt.Sprintf("Cloudflare refused the rule: refused by the fake (%d)", code)))
				Expect(holdsRule(sample.phase, name)).To(BeFalse())

				By("writing the rule once the conflict is gone")
				delete(fake.errs, "Rulesets.New")
				delete(fake.errs, "Rules.New")
				_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
				Expect(err).NotTo(HaveOccurred())
				Expect(holdsRule(sample.phase, name)).To(BeTrue())
			}
		})

		It("refuses the specs its validation does not allow", func() {
			for i, invalid := range specs.invalid {
				By(invalid.what)
				err := k8sClient.Create(ctx, rawObject(fmt.Sprintf("invalid-%d", i), invalid.spec))
				Expect(err).To(MatchError(ContainSubstring(invalid.message)))
			}
		})

		It("registers with a manager, and falls back to the production client", func() {
			mgr, err := manager.New(cfg, manager.Options{
				Controller: config.Controller{SkipNameValidation: new(true)},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(specs.newReconciler(mgr.GetClient(), nil).SetupWithManager(mgr)).To(Succeed())
			_, err = specs.newReconciler(k8sClient, nil).rules().newAPI("")
			Expect(err).To(HaveOccurred(), "the production client rejects an empty token")
		})

		It("is named after its API type, which its rules' refs are derived from", func() {
			gvk, err := apiutil.GVKForObject(specs.newObject(), k8sClient.Scheme())
			Expect(err).NotTo(HaveOccurred())
			Expect(gvk.Kind).To(Equal(kind))
			Expect(specs.kind.name).To(Equal(kind), "another name would change the ref of every rule the kind manages")
		})
	})
}
