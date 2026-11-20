/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"maps"

	cf "github.com/cloudflare/cloudflare-go"
	"github.com/cloudflare/cloudflare-go/v7/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// observeAnnotation is the annotation of a resource kflare only observes.
var observeAnnotation = map[string]string{reconciler.ManagementPolicyAnnotation: reconciler.ManagementPolicyObserve}

// setManagementPolicy sets obj's kflare.dev/management-policy annotation, or
// removes it for "", and updates obj, which it reads again first.
func setManagementPolicy(ctx context.Context, obj client.Object, policy string) {
	GinkgoHelper()
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[reconciler.ManagementPolicyAnnotation] = policy
	if policy == "" {
		delete(annotations, reconciler.ManagementPolicyAnnotation)
	}
	obj.SetAnnotations(annotations)
	Expect(k8sClient.Update(ctx, obj)).To(Succeed())
}

// observeEverything makes the installation observe every resource, as
// --observe-only does, until the spec ends.
func observeEverything() {
	reconciler.SetObserveOnly(true)
	DeferCleanup(reconciler.SetObserveOnly, false)
}

// describeCRUDObserve covers observe mode (reconciler.Observed) for a
// generated CRUD kind: what observeOnly reports for a kind that can be
// observed, and ObserveNotSupported for another, neither writing to
// Cloudflare.
func describeCRUDObserve[O conditionedObject, A any, R any, L any](e *crudSpecEnv[O, A, R, L]) {
	t, k, ctx := e.t, e.t.kind, e.ctx

	expectGone := func() {
		GinkgoHelper()
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, e.key, k.newObject()))).To(BeTrue())
	}

	Describe("observing", func() {
		It("reports InvalidManagementPolicy for an unknown policy, and calls Cloudflare for nothing, even when deleted",
			func() {
				e.createParent(true)
				e.create(e.key, map[string]string{reconciler.ManagementPolicyAnnotation: "observ"})
				for range 2 {
					result, err := e.reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(BeZero())
					Expect(e.store.parentIDs).To(BeEmpty())
					Expect(e.ready().Reason).To(Equal(reasonInvalidManagementPolicy))
					Expect(e.ready().Message).To(ContainSubstring(`"observ"`))
					Expect(k8sClient.Delete(ctx, e.get())).To(Succeed())
				}
				Expect(e.get().GetFinalizers()).To(ContainElement(reconciler.Finalizer))
			})

		It("can be observed: its comparison names what differs", func() {
			Expect(k.observable()).To(BeTrue())
		})

		// observe creates an observed resource, changed by edit if set, and
		// the sample's object, and reconciles the resource.
		observe := func(edit func(O)) string {
			GinkgoHelper()
			e.createParent(true)
			obj := t.newResource(e.key, e.parentName)
			obj.SetAnnotations(observeAnnotation)
			obj.SetFinalizers([]string{reconciler.Finalizer})
			if edit != nil {
				edit(obj)
			}
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			id := e.seed("existing", k.key(obj))
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			return id
		}
		// observeDiffering observes an object that differs from the spec in
		// change's field. An observed resource finds its object by its key, so
		// a changed key differs only from the object its status names already.
		observeDiffering := func(change crudSpecEdit[O]) string {
			GinkgoHelper()
			if change.field != t.keyField {
				return observe(change.edit)
			}
			id := observe(nil)
			obj := e.get()
			change.edit(obj)
			Expect(k8sClient.Update(ctx, obj)).To(Succeed())
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			return id
		}

		It("reports InSync for a "+k.noun+" that matches the spec, records it and claims nothing", func() {
			id := observe(nil)
			Expect(e.ready().Status).To(Equal(metav1.ConditionTrue))
			Expect(e.ready().Reason).To(Equal(reasonInSync))
			obj, object := e.get(), e.store.objects[id]
			Expect(k.statusID(obj)).To(Equal(id))
			Expect(t.observed(obj, &object)).To(BeTrue(), "status records the object")
			if e.zoned {
				Expect(*k.statusZoneID(obj)).To(Equal(e.zoneID))
			}
			Expect(obj.GetLabels()).NotTo(HaveKey(k.claimLabel))
			Expect(obj.GetAnnotations()).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))
			Expect(e.store.writes).To(BeZero())

			By("reading the object by the ID in status from then on, not by its key")
			calls := len(e.store.parentIDs)
			e.store.listErr = cloudflareError(500, 0)
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.store.parentIDs).To(HaveLen(calls+1), "one read")
			Expect(e.ready().Reason).To(Equal(reasonInSync), "the list, which fails, was not asked")
			e.store.listErr = nil
			expectDriftCheck(e.reconcileIt)
			Expect(e.store.writes).To(BeZero())
		})

		for _, change := range t.changes {
			It("reports DriftDetected, naming "+change.field+", after "+change.what+", and leaves the "+k.noun+" as it is",
				func() {
					id := observeDiffering(change)
					before := e.store.objects[id]
					result, err := e.reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
					Expect(e.ready().Status).To(Equal(metav1.ConditionFalse))
					Expect(e.ready().Reason).To(Equal(reasonDriftDetected))
					Expect(e.ready().Message).To(ContainSubstring("differs from the spec in: " + change.field + "."))
					Expect(k.statusID(e.get())).To(Equal(id))
					Expect(e.store.writes).To(BeZero())
					Expect(e.store.objects).To(Equal(map[string]R{id: before}))
				})
		}

		// The specs below need an object that differs in a field the kind
		// changes in place: a kind without one has nothing to drift.
		hasChanges := len(t.changes) > 0

		if hasChanges {
			It("reads a "+k.noun+" that is missing or differs again at the drift check, not every minute", func() {
				turnOnDriftChecks()
				id := observeDiffering(t.changes[0])
				result, err := e.reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.ready().Reason).To(Equal(reasonDriftDetected))
				Expect(result.RequeueAfter).To(BeNumerically("~", driftCheckTestInterval, driftCheckTestSpread))

				delete(e.store.objects, id)
				result, err = e.reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.ready().Reason).To(Equal(reasonNotFound))
				Expect(result.RequeueAfter).To(BeNumerically("~", driftCheckTestInterval, driftCheckTestSpread))
			})
		}

		It("keeps a "+k.noun+" it was creating its own: not marked adopted", func() {
			e.createParent(true)
			obj := t.newResource(e.key, e.parentName)
			obj.SetFinalizers([]string{reconciler.Finalizer})
			annotations := maps.Clone(observeAnnotation)
			annotations[reconciler.CreatingAnnotation] = e.r.crud().objectKey(obj, e.refs)
			obj.SetAnnotations(annotations)
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			id := e.seed("created", k.key(obj))
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ready().Reason).To(Equal(reasonInSync))
			Expect(k.statusID(e.get())).To(Equal(id))
			Expect(e.get().GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation))
			Expect(e.get().GetAnnotations()).To(HaveKey(reconciler.CreatingAnnotation))
		})

		if !k.sharedObjects && hasChanges {
			It("reports "+k.conflictReason+" once switched to manage, for a "+k.noun+" another "+k.name+
				" has claimed, and leaves it to that one", func() {
				id := observeDiffering(t.changes[len(t.changes)-1])
				Expect(e.ready().Reason).To(Equal(reasonDriftDetected))
				other := t.newResource(e.otherKey, e.parentName)
				other.SetFinalizers([]string{reconciler.Finalizer})
				other.SetLabels(map[string]string{k.claimLabel: id})
				Expect(k8sClient.Create(ctx, other)).To(Succeed())

				setManagementPolicy(ctx, e.get(), reconciler.ManagementPolicyManage)
				result, err := e.reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
				Expect(e.ready().Reason).To(Equal(k.conflictReason))
				Expect(e.ready().Message).To(ContainSubstring(e.otherKey.String()))
				Expect(e.get().GetLabels()).NotTo(HaveKey(k.claimLabel))
				Expect(e.store.writes).To(BeZero())
			})
		}

		if e.zoned {
			It("is observed in a Zone kflare only observes and that differs, where a managed one waits", func() {
				e.createParent(true)
				zone := &cloudflarev1alpha1.Zone{}
				zoneKey := types.NamespacedName{Name: e.zoneName, Namespace: crudSpecNamespace}
				Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
				zone.Status.Conditions = []metav1.Condition{{Type: cloudflarev1alpha1.ConditionReady,
					Status: metav1.ConditionFalse, Reason: reasonDriftDetected, LastTransitionTime: metav1.Now()}}
				Expect(k8sClient.Status().Update(ctx, zone)).To(Succeed())

				e.create(e.key, nil)
				_, err := e.reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.ready().Reason).To(Equal("ZoneNotReady"))
				Expect(e.store.parentIDs).To(BeEmpty(), "a managed resource called Cloudflare for a zone that is not Ready")

				setManagementPolicy(ctx, e.get(), reconciler.ManagementPolicyObserve)
				_, err = e.reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.ready().Reason).To(Equal(reasonNotFound))
				Expect(e.store.writes).To(BeZero())
			})
		}

		It("reports NotFound without a "+k.noun+", and creates none", func() {
			e.createParent(true)
			e.create(e.key, observeAnnotation)
			result, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(e.ready().Reason).To(Equal(reasonNotFound))
			Expect(e.store.objects).To(BeEmpty())
			Expect(e.store.writes).To(BeZero())
		})

		It("finds the "+k.noun+" by its "+t.keyField+" again once the one status names is gone", func() {
			id := observe(nil)
			object := e.store.objects[id]
			delete(e.store.objects, id)
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ready().Reason).To(Equal(reasonNotFound))

			id = e.seed("replacement", k.key(e.get()))
			if id == "replacement" {
				Expect(k.id(&object)).NotTo(Equal(id))
			}
			_, err = e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ready().Reason).To(Equal(reasonInSync))
			Expect(k.statusID(e.get())).To(Equal(id))
		})

		It("reports a Cloudflare error, and writes nothing", func() {
			observe(nil)
			e.store.getErr = cloudflareError(403, 10000)
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ready().Reason).To(Equal("TerminalError"))
			Expect(e.store.writes).To(BeZero())
		})

		It("leaves the "+k.noun+" when the resource is deleted, whatever its deletion policy", func() {
			id := observe(nil)
			obj := e.get()
			obj.GetAnnotations()[reconciler.DeletionPolicyAnnotation] = reconciler.DeletionPolicyDelete
			Expect(k8sClient.Update(ctx, obj)).To(Succeed())
			Expect(k8sClient.Delete(ctx, e.get())).To(Succeed())
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			expectGone()
			Expect(e.store.objects).To(HaveKey(id))
			Expect(e.store.writes).To(BeZero())
		})

		It("stops managing a "+k.noun+" it created: no correction, and the object stays when the resource is deleted",
			func() {
				id := e.synced(nil)
				writes := e.store.writes
				obj := e.get()
				for _, change := range t.changes {
					change.edit(obj)
				}
				Expect(k8sClient.Update(ctx, obj)).To(Succeed())
				setManagementPolicy(ctx, e.get(), reconciler.ManagementPolicyObserve)
				_, err := e.reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				reason := reasonDriftDetected
				if len(t.changes) == 0 {
					reason = reasonInSync
				}
				Expect(e.ready().Reason).To(Equal(reason))
				Expect(e.get().GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation), "kflare created the object")

				Expect(k8sClient.Delete(ctx, e.get())).To(Succeed())
				_, err = e.reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				expectGone()
				Expect(e.store.objects).To(HaveKey(id))
				Expect(e.store.writes).To(Equal(writes))
			})

		if hasChanges {
			It("corrects the "+k.noun+" once switched to manage, as one it adopted: kept when the resource is deleted",
				func() {
					id := observeDiffering(t.changes[len(t.changes)-1])
					Expect(e.ready().Reason).To(Equal(reasonDriftDetected))

					setManagementPolicy(ctx, e.get(), reconciler.ManagementPolicyManage)
					_, err := e.reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					Expect(e.store.writes).To(Equal(1))
					object := e.store.objects[id]
					Expect(e.matches(e.get(), &object)).To(BeTrue(), "the object is what the spec asks for")
					Expect(e.ready().Reason).To(Equal("Synced"))
					Expect(e.get().GetLabels()).To(HaveKeyWithValue(k.claimLabel, id))
					Expect(e.get().GetAnnotations()).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))

					Expect(k8sClient.Delete(ctx, e.get())).To(Succeed())
					_, err = e.reconcileIt()
					Expect(err).NotTo(HaveOccurred())
					expectGone()
					Expect(e.store.objects).To(HaveKey(id))
				})
		}

		It("observes every resource when the installation does, one whose policy is manage included", func() {
			observeEverything()
			e.createParent(true)
			e.create(e.key, map[string]string{reconciler.ManagementPolicyAnnotation: reconciler.ManagementPolicyManage})
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ready().Reason).To(Equal(reasonNotFound))
			Expect(e.store.objects).To(BeEmpty())
			Expect(e.store.writes).To(BeZero())
		})
	})
}

var _ = Describe("The fields an observed object differs in", func() {
	It("names a DNS record's, by the spec field that asks for each", func() {
		record := &cloudflarev1alpha1.DNSRecord{Spec: cloudflarev1alpha1.DNSRecordSpec{
			Type: "MX", Content: "mail.example.com", Comment: "mail",
			DNSRecordSpecExtras: cloudflarev1alpha1.DNSRecordSpecExtras{TTL: 300, Proxied: new(true),
				Priority: new(uint16(10)), Tags: []string{"a"}},
		}}
		current := &dns.RecordResponse{Content: "mail.example.com", Comment: "mail", Priority: 10, TTL: 300, Proxied: true}
		Expect(dnsRecordDifferences(record, current, nil)).To(Equal([]string{"tags"}))

		current = &dns.RecordResponse{TTL: automaticTTL}
		Expect(dnsRecordDifferences(record, current, nil)).To(Equal(
			[]string{"content", "ttl", "proxied", "priority", "comment", "tags"}))

		By("leaving what the spec leaves to Cloudflare, and comparing the defaults of what it leaves unset")
		unset := &cloudflarev1alpha1.DNSRecord{Spec: cloudflarev1alpha1.DNSRecordSpec{Type: "MX"}}
		Expect(dnsRecordDifferences(unset, &dns.RecordResponse{TTL: automaticTTL, Priority: 20}, nil)).To(BeEmpty())
		Expect(dnsRecordDifferences(unset, &dns.RecordResponse{TTL: 300, Proxied: true}, nil)).To(Equal(
			[]string{"ttl", "proxied"}))

		By("naming data rather than the content Cloudflare derives from it")
		withData := &cloudflarev1alpha1.DNSRecord{Spec: cloudflarev1alpha1.DNSRecordSpec{Type: "SRV",
			DNSRecordSpecExtras: cloudflarev1alpha1.DNSRecordSpecExtras{Data: &apiextensionsv1.JSON{Raw: []byte(`{"port":1}`)}}}}
		Expect(dnsRecordDifferences(withData, &dns.RecordResponse{TTL: automaticTTL, Content: "derived"}, nil)).To(
			Equal([]string{"data"}))

		By("naming tunnelRef for a record that points at a Tunnel")
		tunnelled := &cloudflarev1alpha1.DNSRecord{Spec: cloudflarev1alpha1.DNSRecordSpec{Type: "CNAME",
			DNSRecordSpecExtras: cloudflarev1alpha1.DNSRecordSpecExtras{
				TunnelRef: &corev1.LocalObjectReference{Name: "tunnel"}}}}
		refs := map[string]string{tunnelRefField: "tunnel-id"}
		Expect(dnsRecordDifferences(tunnelled, &dns.RecordResponse{TTL: automaticTTL, Proxied: true,
			Content: "elsewhere.example.com"}, refs)).To(Equal([]string{tunnelRefField}))
		Expect(dnsRecordDifferences(tunnelled, &dns.RecordResponse{TTL: automaticTTL, Proxied: true,
			Content: "tunnel-id" + cloudflarev1alpha1.TunnelHostnameSuffix}, refs)).To(BeEmpty())
	})

	It("names a zone's type, and the plan kflare would subscribe it to", func() {
		zone := &cloudflarev1alpha1.Zone{}
		zone.Spec.Plan = "pro"
		Expect(zoneDifferences(zone, &cf.Zone{Type: "full", Plan: cf.ZonePlan{LegacyID: "pro"}}, nil)).To(BeEmpty())
		Expect(zoneDifferences(zone, &cf.Zone{Type: "full", PlanPending: cf.ZonePlan{LegacyID: "pro"}}, nil)).To(
			BeEmpty(), "a change Cloudflare has scheduled")
		Expect(zoneDifferences(zone, &cf.Zone{Type: "partial", Plan: cf.ZonePlan{LegacyID: "free"}}, nil)).To(
			Equal([]string{"type", "plan"}))
		zone.Spec.Plan = ""
		Expect(zoneDifferences(zone, &cf.Zone{Type: "full", Plan: cf.ZonePlan{LegacyID: "business"}}, nil)).To(
			BeEmpty(), "an unset plan is left to Cloudflare")
	})
})
