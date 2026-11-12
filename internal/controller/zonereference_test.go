/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

var _ = Describe("A Zone's allowed hostnames", func() {
	DescribeTable("hostnameAllowed",
		func(pattern, hostname string, allowed bool) {
			Expect(hostnameAllowed(pattern, hostname)).To(Equal(allowed))
		},
		Entry("a hostname allows itself", "api.example.com", "api.example.com", true),
		Entry("a hostname allows no other", "api.example.com", "www.example.com", false),
		Entry("a hostname allows nothing below it", "example.com", "api.example.com", false),
		Entry("a hostname allows nothing above it", "api.example.com", "example.com", false),
		Entry("a hostname does not allow a name that only ends like it", "example.com", "notexample.com", false),
		Entry("names ignore case", "API.Example.com", "api.EXAMPLE.com", true),
		Entry("names ignore the dot of a fully-qualified name", "api.example.com.", "api.example.com", true),
		Entry("names ignore that dot in the hostname too", "api.example.com", "api.example.com.", true),
		Entry("a wildcard allows a name below it", "*.team-a.example.com", "api.team-a.example.com", true),
		Entry("a wildcard allows names at any depth below it", "*.team-a.example.com", "v2.api.team-a.example.com", true),
		Entry("a wildcard does not allow its own parent", "*.team-a.example.com", "team-a.example.com", false),
		Entry("a wildcard does not allow a sibling", "*.team-a.example.com", "api.team-b.example.com", false),
		Entry("a wildcard does not allow a name that only ends like its parent", "*.team-a.example.com",
			"api.not-team-a.example.com", false),
		Entry("a wildcard does not allow a name without a label below it", "*.example.com", ".example.com", false),
		Entry("a wildcard allows a wildcard record below it", "*.team-a.example.com", "*.team-a.example.com", true),
		Entry("a wildcard allows a wildcard record further below it", "*.example.com", "*.team-a.example.com", true),
		Entry("a wildcard does not allow a wildcard record above it", "*.team-a.example.com", "*.example.com", false),
		Entry("a hostname does not allow a wildcard record below it", "team-a.example.com", "*.team-a.example.com", false),
		Entry("a wildcard allows a route for a suffix below it", "*.example.com", "*team-a.example.com", true),
		Entry("a wildcard does not allow a route for a suffix that reaches beside it", "*.team-a.example.com",
			"*team-a.example.com", false),
		Entry("an empty hostname is never allowed", "*.example.com", "", false),
		Entry("an empty pattern allows nothing", "", "api.example.com", false),
		Entry("a wildcard alone allows nothing", "*.", "api.example.com", false),
	)

	DescribeTable("routePatternHostname",
		func(pattern, hostname string) {
			Expect(routePatternHostname(pattern)).To(Equal(hostname))
		},
		Entry("a hostname and a path", "example.com/api/*", "example.com"),
		Entry("a hostname alone", "api.example.com", "api.example.com"),
		Entry("a wildcard subdomain", "*.example.com/*", "*.example.com"),
		Entry("a wildcard suffix", "*example.com/*", "*example.com"),
		Entry("a scheme, as Cloudflare stores one it is given", "https://api.example.com/v1/*", "api.example.com"),
		Entry("a path that holds a scheme of its own, which names no hostname", "example.com/go/http://elsewhere.example",
			"example.com"),
		Entry("a scheme before a path that holds another", "https://example.com/go/http://elsewhere.example",
			"example.com"),
	)

	DescribeTable("allowsNamespace",
		func(allowed cloudflarev1alpha1.AllowedNamespaces, expected bool) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Labels: map[string]string{"team": "a"}}}
			Expect(allowsNamespace(allowed, ns)).To(Equal(expected))
		},
		Entry("from All allows it", cloudflarev1alpha1.AllowedNamespaces{From: cloudflarev1alpha1.NamespacesFromAll}, true),
		Entry("a selector that matches allows it", *selectorAllowing(map[string]string{"team": "a"}), true),
		Entry("a selector that does not match does not", *selectorAllowing(map[string]string{"team": "b"}), false),
		Entry("an empty selector allows it, as it matches every namespace", cloudflarev1alpha1.AllowedNamespaces{
			From: cloudflarev1alpha1.NamespacesFromSelector, Selector: &metav1.LabelSelector{}}, true),
		Entry("from Selector without a selector allows none",
			cloudflarev1alpha1.AllowedNamespaces{From: cloudflarev1alpha1.NamespacesFromSelector}, false),
		Entry("no from allows none, even with a selector that matches", cloudflarev1alpha1.AllowedNamespaces{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}}, false),
		Entry("an invalid selector allows none", cloudflarev1alpha1.AllowedNamespaces{
			From: cloudflarev1alpha1.NamespacesFromSelector,
			Selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "team", Operator: metav1.LabelSelectorOpIn,
			}}}}, false),
	)

	Describe("checkZoneReference", func() {
		ctx := context.Background()
		zone := func(entries ...cloudflarev1alpha1.ZoneAllowedReference) *cloudflarev1alpha1.Zone {
			return &cloudflarev1alpha1.Zone{
				ObjectMeta: metav1.ObjectMeta{Name: "example-com", Namespace: "platform-dns"},
				Spec: cloudflarev1alpha1.ZoneSpec{
					ZoneSpecExtras: cloudflarev1alpha1.ZoneSpecExtras{AllowedReferences: entries},
				},
			}
		}
		entry := func(namespace string, hostnames ...string) cloudflarev1alpha1.ZoneAllowedReference {
			return cloudflarev1alpha1.ZoneAllowedReference{
				Namespaces: *selectorAllowing(map[string]string{corev1.LabelMetadataName: namespace}),
				Hostnames:  hostnames,
			}
		}

		It("allows a namespace and hostname that one entry lists together", func() {
			Expect(checkZoneReference(ctx, k8sClient, zone(entry("kube-system", "a.example.com"),
				entry("default", "b.example.com", "*.c.example.com")), "default", "x.c.example.com")).To(BeNil())
		})

		It("does not combine one entry's namespace with another's hostname", func() {
			condErr := checkZoneReference(ctx, k8sClient, zone(entry("kube-system", "a.example.com"),
				entry("default", "b.example.com")), "default", "a.example.com")
			Expect(condErr).NotTo(BeNil())
			Expect(condErr.Reason).To(Equal(reasonZoneReferenceNotAllowed))
			Expect(condErr.Message).To(Equal(`Namespace "default" may not use Zone platform-dns/example-com for ` +
				`"a.example.com": no entry of its spec.allowedReferences that allows the namespace lists a hostname ` +
				`that covers it`))
		})

		It("says so when no entry allows the namespace", func() {
			condErr := checkZoneReference(ctx, k8sClient, zone(entry("kube-system", "a.example.com")), "default",
				"a.example.com")
			Expect(condErr).NotTo(BeNil())
			Expect(condErr.Message).To(Equal(`Namespace "default" may not use Zone platform-dns/example-com for ` +
				`"a.example.com": no entry of its spec.allowedReferences allows the namespace`))
		})

		It("allows nothing without entries", func() {
			condErr := checkZoneReference(ctx, k8sClient, zone(), "default", "a.example.com")
			Expect(condErr).NotTo(BeNil())
			Expect(condErr.Message).To(Equal(`Namespace "default" may not use Zone platform-dns/example-com for ` +
				`"a.example.com": its spec.allowedReferences is unset, so only resources in its own namespace use it`))
		})

		It("allows nothing to a namespace it cannot read, even from an entry for every namespace", func() {
			open := cloudflarev1alpha1.ZoneAllowedReference{
				Namespaces: cloudflarev1alpha1.AllowedNamespaces{From: cloudflarev1alpha1.NamespacesFromAll},
				Hostnames:  []string{"a.example.com"},
			}
			Expect(checkZoneReference(ctx, k8sClient, zone(open), "default", "a.example.com")).To(BeNil())
			condErr := checkZoneReference(ctx, k8sClient, zone(open), "no-such-namespace", "a.example.com")
			Expect(condErr).NotTo(BeNil())
			Expect(condErr.Reason).To(Equal(reasonZoneReferenceNotAllowed))
			Expect(condErr.Message).To(ContainSubstring("the namespace could not be read"))
		})
	})

	Describe("the Zone's CRD", func() {
		ctx := context.Background()
		// createZone creates a Zone whose spec.allowedReferences is allowed, as
		// a manifest would give it.
		createZone := func(name string, allowed []any) error {
			zone := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": cloudflarev1alpha1.GroupVersion.String(),
				"kind":       "Zone",
				"metadata":   map[string]any{"name": name, "namespace": "default"},
				"spec": map[string]any{
					"accountRef":        map[string]any{"name": "main"},
					"name":              "example.com",
					"allowedReferences": allowed,
				},
			}}
			err := k8sClient.Create(ctx, zone)
			if err == nil {
				DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.Zone{}, types.NamespacedName{Name: name, Namespace: "default"})
			}
			return err
		}
		selected := map[string]any{"from": "Selector", "selector": map[string]any{
			"matchLabels": map[string]any{"team": "a"}}}

		It("takes an entry for selected namespaces and one for all, with hostnames and wildcards", func() {
			Expect(createZone("zone-crd-valid", []any{
				map[string]any{"namespaces": selected, "hostnames": []any{"*.team-a.example.com", "team-a.example.com"}},
				map[string]any{"namespaces": map[string]any{"from": "All"}, "hostnames": []any{"_acme-challenge.example.com"}},
			})).To(Succeed())
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "zone-crd-valid", Namespace: "default"}, zone)).To(Succeed())
			Expect(zone.Spec.AllowedReferences).To(HaveLen(2))
			Expect(zone.Spec.AllowedReferences[0].Hostnames).To(Equal([]string{"*.team-a.example.com", "team-a.example.com"}))
		})

		DescribeTable("refuses",
			func(name string, entry map[string]any, message string) {
				err := createZone(name, []any{entry})
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
				Expect(err.Error()).To(ContainSubstring(message))
			},
			Entry("an entry without namespaces", "zone-crd-no-namespaces",
				map[string]any{"hostnames": []any{"a.example.com"}}, "namespaces"),
			Entry("an entry without hostnames", "zone-crd-no-hostnames",
				map[string]any{"namespaces": selected}, "hostnames"),
			Entry("an entry with no hostname", "zone-crd-empty-hostnames",
				map[string]any{"namespaces": selected, "hostnames": []any{}}, "hostnames"),
			Entry("a selector without from: Selector, which would read as every namespace", "zone-crd-selector-alone",
				map[string]any{"namespaces": map[string]any{"selector": map[string]any{}},
					"hostnames": []any{"a.example.com"}}, "selector goes with from: Selector"),
			Entry("a wildcard that is not a whole first label", "zone-crd-infix",
				map[string]any{"namespaces": selected, "hostnames": []any{"a*.example.com"}}, "hostnames"),
			Entry("a wildcard in the middle", "zone-crd-middle",
				map[string]any{"namespaces": selected, "hostnames": []any{"a.*.example.com"}}, "hostnames"),
			Entry("a wildcard alone", "zone-crd-star",
				map[string]any{"namespaces": selected, "hostnames": []any{"*"}}, "hostnames"),
			Entry("a hostname with a path", "zone-crd-path",
				map[string]any{"namespaces": selected, "hostnames": []any{"example.com/api"}}, "hostnames"),
			Entry("a hostname listed twice", "zone-crd-twice",
				map[string]any{"namespaces": selected, "hostnames": []any{"a.example.com", "a.example.com"}}, "Duplicate"),
		)
	})
})

// describeCRUDSharedZone adds the specs of a zone's kind whose resources may
// name a Zone in another namespace: the Zone's spec.allowedReferences decides
// whether a resource's namespace may use it, and for which hostnames. A kind
// whose resources name a Zone in their own namespace only has none.
func describeCRUDSharedZone[O conditionedObject, A any, R any, L any](e *crudSpecEnv[O, A, R, L]) {
	if e.t.inZoneNamespace == nil {
		return
	}
	ctx, k, t, key := e.ctx, e.t.kind, e.t, e.key
	// platform is the namespace of the Zone, and sharedID its zone's ID. The
	// resources' own namespace holds a Zone of the same name for another
	// zone (createParent), which a resource that names platform's must not
	// use.
	platform, sharedID := e.base+"-platform", "zone-shared-"+e.base
	zoneKey := types.NamespacedName{Name: e.zoneName, Namespace: platform}

	// hostname is what the sample asks its Zone for, and victimKey the key of
	// another object of the zone, of another hostname.
	hostname := k.hostname(t.newResource(key, e.zoneName))
	victimKey := "victim-" + k.key(t.newResource(key, e.zoneName))

	inNamespace := func(name string) cloudflarev1alpha1.AllowedNamespaces {
		return *selectorAllowing(map[string]string{corev1.LabelMetadataName: name})
	}
	allowing := func(namespaces cloudflarev1alpha1.AllowedNamespaces, hostnames ...string) []cloudflarev1alpha1.ZoneAllowedReference {
		return []cloudflarev1alpha1.ZoneAllowedReference{{Namespaces: namespaces, Hostnames: hostnames}}
	}
	// allow sets the shared Zone's spec.allowedReferences.
	allow := func(entries []cloudflarev1alpha1.ZoneAllowedReference) {
		GinkgoHelper()
		zone := &cloudflarev1alpha1.Zone{}
		Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
		zone.Spec.AllowedReferences = entries
		Expect(k8sClient.Update(ctx, zone)).To(Succeed())
	}
	// createShared creates the account, the resources the sample references,
	// and the shared Zone, ready, with entries.
	createShared := func(entries []cloudflarev1alpha1.ZoneAllowedReference) {
		GinkgoHelper()
		e.createParent(true)
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: platform}})
		Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue(), "%v", err)
		e.createZone(zoneKey, sharedID, true)
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.Zone{}, zoneKey)
		allow(entries)
	}
	// create creates a resource at at, with the finalizer, that names the
	// shared Zone.
	create := func(at types.NamespacedName, labels map[string]string) O {
		GinkgoHelper()
		obj := t.newResource(at, e.zoneName)
		t.inZoneNamespace(obj, platform)
		obj.SetLabels(labels)
		obj.SetFinalizers([]string{reconciler.Finalizer})
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		return obj
	}
	// synced creates the shared Zone, allowing the sample, and a resource with
	// the object it manages there.
	synced := func() string {
		GinkgoHelper()
		createShared(allowing(inNamespace(key.Namespace), hostname))
		create(key, nil)
		_, err := e.reconcileIt()
		Expect(err).NotTo(HaveOccurred())
		Expect(e.ready().Reason).To(Equal("Synced"), e.ready().Message)
		Expect(e.store.objects).To(HaveLen(1))
		return k.statusID(e.get())
	}
	// reconcileRefused reconciles the resource, and checks that it made no
	// Cloudflare call, which the fake records the parent's ID of.
	reconcileRefused := func() {
		GinkgoHelper()
		calls := len(e.store.parentIDs)
		_, err := e.reconcileIt()
		Expect(err).NotTo(HaveOccurred())
		Expect(e.store.parentIDs).To(HaveLen(calls), "a resource its Zone does not allow reached Cloudflare")
	}
	notAllowed := func(why string) string {
		return fmt.Sprintf("Namespace %q may not use Zone %s/%s for %q: %s", key.Namespace, platform, e.zoneName,
			hostname, why)
	}
	expectGone := func() {
		GinkgoHelper()
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, k.newObject()))).To(BeTrue(), "the resource kept its finalizer")
	}
	drained := func() *events.FakeRecorder {
		recorder := events.NewFakeRecorder(10)
		SetEventRecorder(recorder)
		DeferCleanup(func() { SetEventRecorder(nil) })
		return recorder
	}
	drain := func(recorder *events.FakeRecorder) {
		for len(recorder.Events) > 0 {
			<-recorder.Events
		}
	}

	Describe("a Zone in another namespace", func() {
		It("manages its "+k.noun+" in the zone of a Zone that allows its namespace and hostname", func() {
			id := synced()
			Expect(e.store.objects).To(HaveKey(id))
			Expect(e.store.parentIDs).To(HaveEach(sharedID), "it used the Zone of the same name in its own namespace")
			Expect(*k.statusZoneID(e.get())).To(Equal(sharedID))

			By("reconciling again without writing")
			writes := e.store.writes
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.store.writes).To(Equal(writes))
			Expect(e.ready().Reason).To(Equal("Synced"))
		})

		It("calls Cloudflare for nothing until the Zone allows its namespace and hostname", func() {
			createShared(nil)
			create(key, nil)
			reconcileRefused()
			Expect(e.ready().Status).To(Equal(metav1.ConditionFalse))
			Expect(e.ready().Reason).To(Equal(reasonZoneReferenceNotAllowed))
			Expect(e.ready().Message).To(Equal(notAllowed("its spec.allowedReferences is unset, so only resources in " +
				"its own namespace use it")))

			By("allowing another namespace the hostname")
			allow(allowing(inNamespace("some-other-namespace"), hostname))
			reconcileRefused()
			Expect(e.ready().Reason).To(Equal(reasonZoneReferenceNotAllowed))
			Expect(e.ready().Message).To(Equal(notAllowed("no entry of its spec.allowedReferences allows the namespace")))

			By("allowing its namespace another hostname")
			allow(allowing(inNamespace(key.Namespace), "elsewhere.example.org", "*."+hostname))
			reconcileRefused()
			Expect(e.ready().Reason).To(Equal(reasonZoneReferenceNotAllowed))
			Expect(e.ready().Message).To(Equal(notAllowed("no entry of its spec.allowedReferences that allows the " +
				"namespace lists a hostname that covers it")))
			Expect(e.store.objects).To(BeEmpty())

			By("allowing its namespace its hostname")
			allow(allowing(inNamespace(key.Namespace), "elsewhere.example.org", hostname))
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ready().Reason).To(Equal("Synced"), e.ready().Message)
			Expect(e.store.objects).To(HaveLen(1))
		})

		It("is refused before it learns whether the Zone is ready", func() {
			createShared(nil)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
			zone.Status.Conditions[0].Status = metav1.ConditionFalse
			Expect(k8sClient.Status().Update(ctx, zone)).To(Succeed())
			create(key, nil)
			reconcileRefused()
			Expect(e.ready().Reason).To(Equal(reasonZoneReferenceNotAllowed))

			By("waiting for the Zone once it allows the resource")
			allow(allowing(inNamespace(key.Namespace), hostname))
			reconcileRefused()
			Expect(e.ready().Reason).To(Equal("ZoneNotReady"))
			Expect(e.ready().Message).To(Equal(fmt.Sprintf("Zone %q is not ready", platform+"/"+e.zoneName)))
		})

		It("reports a Zone that its namespace does not hold, by namespace and name", func() {
			e.createParent(true)
			err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: platform}})
			Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue(), "%v", err)
			create(key, nil)
			reconcileRefused()
			Expect(e.ready().Reason).To(Equal("ZoneNotFound"))
			Expect(e.ready().Message).To(ContainSubstring(fmt.Sprintf("Zone %q", platform+"/"+e.zoneName)))
		})

		It("stops at a Zone that no longer allows it", func() {
			id := synced()
			before := e.store.objects[id]
			allow(allowing(inNamespace("some-other-namespace"), hostname))
			reconcileRefused()
			Expect(e.ready().Status).To(Equal(metav1.ConditionFalse))
			Expect(e.ready().Reason).To(Equal(reasonZoneReferenceNotAllowed))
			Expect(e.store.objects[id]).To(Equal(before))
		})

		It("asks the Zone's account about the Zone's namespace, not its own", func() {
			createShared(allowing(inNamespace(key.Namespace), hostname))
			create(key, nil)
			allowOnlyNamespace(ctx, e.accountName, platform)
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ready().Reason).To(Equal("Synced"), e.ready().Message)

			By("stopping once the account leaves the Zone's namespace out")
			allowOnlyNamespace(ctx, e.accountName, key.Namespace)
			reconcileRefused()
			Expect(e.ready().Reason).To(Equal(reasonAccountNotAllowed))
			Expect(e.ready().Message).To(ContainSubstring(fmt.Sprintf("Namespace %q may not use", platform)))
		})

		It("leaves a "+k.noun+" of another hostname that its status names, and reports it", func() {
			synced()
			victim := e.seed("victim", victimKey)
			before := e.store.objects[victim]
			obj := e.get()
			k.observe(obj, &before)
			Expect(k8sClient.Status().Update(ctx, obj)).To(Succeed())
			writes := e.store.writes

			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ready().Reason).To(Equal(reasonZoneReferenceNotAllowed))
			Expect(e.ready().Message).To(Equal(fmt.Sprintf("%s %s, which this %s names by its ID, has the hostname %q. "+
				"Namespace %q may not use Zone %s/%s for %q: no entry of its spec.allowedReferences that allows the "+
				"namespace lists a hostname that covers it", k.noun, victim, k.name, k.objectHostname(&before),
				key.Namespace, platform, e.zoneName, k.objectHostname(&before))))
			Expect(e.store.writes).To(Equal(writes), "it wrote to an object its Zone does not allow it")
			Expect(e.store.objects[victim]).To(Equal(before))
		})

		It("does not adopt a "+k.noun+" that another resource manages through the same Zone", func() {
			createShared(allowing(inNamespace(key.Namespace), hostname))
			claimant := create(e.otherKey, nil)
			claimant.SetLabels(map[string]string{k.claimLabel: e.seed("taken", k.key(claimant))})
			Expect(k8sClient.Update(ctx, claimant)).To(Succeed())
			obj := create(key, nil)
			obj.SetAnnotations(map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt})
			Expect(k8sClient.Update(ctx, obj)).To(Succeed())
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ready().Reason).To(Equal(k.conflictReason), e.ready().Message)
			Expect(e.store.writes).To(BeZero())
		})

		It("adopts a "+k.noun+" that a resource in its namespace claims through the Zone of the same name there, "+
			"for another zone", func() {
			createShared(allowing(inNamespace(key.Namespace), hostname))
			// It names the Zone createParent made in the resources' namespace.
			claimant := t.newResource(e.otherKey, e.zoneName)
			claimant.SetFinalizers([]string{reconciler.Finalizer})
			claimant.SetLabels(map[string]string{k.claimLabel: e.seed("taken", k.key(claimant))})
			Expect(k8sClient.Create(ctx, claimant)).To(Succeed())
			obj := create(key, nil)
			obj.SetAnnotations(map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt})
			Expect(k8sClient.Update(ctx, obj)).To(Succeed())
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ready().Status).To(Equal(metav1.ConditionTrue), e.ready().Message)
			Expect(e.get().GetAnnotations()).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))
		})

		It("does not adopt a "+k.noun+" that a resource manages through a Zone in another namespace, for the same zone",
			func() {
				createShared(allowing(inNamespace(key.Namespace), hostname))
				claimant := create(e.otherKey, nil)
				claimant.SetLabels(map[string]string{k.claimLabel: e.seed("taken", k.key(claimant))})
				Expect(k8sClient.Update(ctx, claimant)).To(Succeed())
				// A second Zone for the shared Zone's zone, in the resources'
				// namespace, which the resource names. The Zone of the claimant's
				// name there is for another zone.
				e.createZone(types.NamespacedName{Name: e.otherZoneName, Namespace: key.Namespace}, sharedID, true)
				obj := t.newResource(key, e.otherZoneName)
				obj.SetFinalizers([]string{reconciler.Finalizer})
				obj.SetAnnotations(map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt})
				Expect(k8sClient.Create(ctx, obj)).To(Succeed())
				_, err := e.reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.ready().Reason).To(Equal(k.conflictReason), e.ready().Message)
				Expect(e.store.writes).To(BeZero())
			})

		It("hands its "+k.noun+" to the resource that replaces it, when only its claim names the "+k.noun, func() {
			createShared(allowing(inNamespace(key.Namespace), hostname))
			// No Zone of the shared Zone's name in the resources' namespace.
			forceDelete(ctx, &cloudflarev1alpha1.Zone{}, types.NamespacedName{Name: e.zoneName, Namespace: key.Namespace})
			obj := t.newResource(key, e.zoneName)
			id := e.seed("mine", k.key(obj))
			create(key, map[string]string{k.claimLabel: id})
			create(e.otherKey, nil)
			Expect(k8sClient.Delete(ctx, e.get())).To(Succeed())
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			expectGone()
			Expect(e.store.objects).To(HaveKey(id), "the object went with the renamed resource")
			successor := k.newObject()
			Expect(k8sClient.Get(ctx, e.otherKey, successor)).To(Succeed())
			Expect(successor.GetLabels()).To(HaveKeyWithValue(k.claimLabel, id))
		})

		Describe("deletion", func() {
			It("deletes the "+k.noun+" through a Zone that allows it", func() {
				id := synced()
				Expect(k8sClient.Delete(ctx, e.get())).To(Succeed())
				_, err := e.reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.store.objects).NotTo(HaveKey(id))
				Expect(e.store.parentIDs[len(e.store.parentIDs)-1]).To(Equal(sharedID))
				expectGone()
			})

			It("deletes the "+k.noun+" with an account that allows the Zone's namespace alone", func() {
				id := synced()
				allowOnlyNamespace(ctx, e.accountName, platform)
				Expect(k8sClient.Delete(ctx, e.get())).To(Succeed())
				_, err := e.reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.store.objects).NotTo(HaveKey(id), "the account was asked about the resource's namespace")
				expectGone()
			})

			It("leaves the "+k.noun+" when the Zone no longer allows the resource, with a Warning Event", func() {
				recorder := drained()
				id := synced()
				drain(recorder)
				allow(allowing(inNamespace("some-other-namespace"), hostname))
				Expect(k8sClient.Delete(ctx, e.get())).To(Succeed())
				reconcileRefused()

				Expect(e.store.objects).To(HaveKey(id), "a namespace the Zone leaves out deleted an object")
				expectGone()
				Expect(recorder.Events).To(Receive(And(ContainSubstring("Warning "+reasonZoneReferenceNotAllowed),
					ContainSubstring(fmt.Sprintf("may not use Zone %s/%s", platform, e.zoneName)),
					ContainSubstring("stays in Cloudflare"))))
			})

			It("leaves a "+k.noun+" of another hostname that only its claim label names, with a Warning Event", func() {
				recorder := drained()
				createShared(allowing(inNamespace(key.Namespace), hostname))
				victim := e.seed("victim", victimKey)
				before := e.store.objects[victim]
				create(key, map[string]string{k.claimLabel: victim})
				drain(recorder)
				Expect(k8sClient.Delete(ctx, e.get())).To(Succeed())
				_, err := e.reconcileIt()
				Expect(err).NotTo(HaveOccurred())

				Expect(e.store.writes).To(BeZero(), "a claim label deleted an object its Zone does not allow")
				Expect(e.store.objects[victim]).To(Equal(before))
				expectGone()
				Expect(recorder.Events).To(Receive(And(ContainSubstring("Warning "+reasonZoneReferenceNotAllowed),
					ContainSubstring(fmt.Sprintf("%s %s, which this %s names by its ID", k.noun, victim, k.name)),
					ContainSubstring("stays in Cloudflare"))))
			})

			It("goes when its "+k.noun+" is already gone", func() {
				id := synced()
				delete(e.store.objects, id)
				Expect(k8sClient.Delete(ctx, e.get())).To(Succeed())
				writes := e.store.writes
				_, err := e.reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.store.writes).To(Equal(writes))
				expectGone()
			})

			It("keeps the finalizer while its "+k.noun+" cannot be read", func() {
				id := synced()
				Expect(k8sClient.Delete(ctx, e.get())).To(Succeed())
				e.store.getErr = errors.New("cloudflare unavailable")
				_, err := e.reconcileIt()
				Expect(err).To(HaveOccurred())
				Expect(e.get().GetFinalizers()).To(ContainElement(reconciler.Finalizer))
				Expect(e.store.objects).To(HaveKey(id))
			})

			It("keeps the finalizer when the Zone cannot be read", func() {
				synced()
				e.failGets(e.zoneName, errors.New("zone unavailable"))
				Expect(k8sClient.Delete(ctx, e.get())).To(Succeed())
				_, err := e.reconcileIt()
				Expect(err).To(MatchError("zone unavailable"))
				Expect(e.get().GetFinalizers()).To(ContainElement(reconciler.Finalizer))
				Expect(e.store.objects).To(HaveLen(1))
			})
		})

		Describe("the Zone's own deletion", func() {
			It("waits for a resource in another namespace that it allows, and not for one it does not", func() {
				createShared(allowing(inNamespace(key.Namespace), hostname))
				create(key, nil)
				deps, err := dependentsOf(ctx, k8sClient, platform, e.zoneName, zoneDependents)
				Expect(err).NotTo(HaveOccurred())
				Expect(deps).To(ConsistOf(k.name + " " + key.String()))

				By("not counting it for the Zone of the same name in its own namespace")
				deps, err = dependentsOf(ctx, k8sClient, key.Namespace, e.zoneName, zoneDependents)
				Expect(err).NotTo(HaveOccurred())
				Expect(deps).To(BeEmpty())

				By("letting the Zone go once it no longer allows the resource")
				allow(allowing(inNamespace(key.Namespace), "elsewhere.example.org"))
				deps, err = dependentsOf(ctx, k8sClient, platform, e.zoneName, zoneDependents)
				Expect(err).NotTo(HaveOccurred())
				Expect(deps).To(BeEmpty(), "any namespace could hold back a Zone that does not allow it")
			})

			It("returns the error of reading the Zone a resource in another namespace names", func() {
				createShared(allowing(inNamespace(key.Namespace), hostname))
				create(key, nil)
				forceDelete(ctx, &cloudflarev1alpha1.Zone{}, zoneKey)
				_, err := dependentsOf(ctx, k8sClient, platform, e.zoneName, zoneDependents)
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%v", err)
			})

			It("maps the deletion of a resource to the Zone it names, when that one is being deleted", func() {
				createShared(allowing(inNamespace(key.Namespace), hostname))
				obj := create(key, nil)
				newZone := func() client.Object { return &cloudflarev1alpha1.Zone{} }
				Expect(deletingParentOf(ctx, k8sClient, obj, newZone, true, zoneDependents)).To(BeEmpty(),
					"a Zone in normal use has nothing to do")

				zone := &cloudflarev1alpha1.Zone{}
				Expect(k8sClient.Get(ctx, zoneKey, zone)).To(Succeed())
				zone.Finalizers = []string{reconciler.Finalizer}
				Expect(k8sClient.Update(ctx, zone)).To(Succeed())
				Expect(k8sClient.Delete(ctx, zone)).To(Succeed())
				Expect(deletingParentOf(ctx, k8sClient, obj, newZone, true, zoneDependents)).To(
					ConsistOf(reconcile.Request{NamespacedName: zoneKey}))
			})
		})

		Describe("its watches", func() {
			It("maps a Zone to the resources that name it from another namespace", func() {
				create(key, nil)
				zone := func(name, namespace string) client.Object {
					return &cloudflarev1alpha1.Zone{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
				}
				Expect(e.r.crud().resourcesForZone(ctx, zone(e.zoneName, platform))).To(
					ConsistOf(reconcile.Request{NamespacedName: key}))
				Expect(e.r.crud().resourcesForZone(ctx, zone(e.zoneName, key.Namespace))).To(BeEmpty(),
					"the Zone of the same name in its own namespace is not the one it names")
				Expect(e.r.crud().resourcesForZone(ctx, zone(e.zoneName+"-other", platform))).To(BeEmpty())
			})

			It("maps a namespace to its resources that name a Zone elsewhere, and to no other", func() {
				create(key, nil)
				// A resource of its own namespace's Zone.
				local := t.newResource(e.otherKey, e.zoneName)
				Expect(k8sClient.Create(ctx, local)).To(Succeed())
				namespace := func(name string) client.Object {
					return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
				}
				Expect(e.r.crud().resourcesOfSharedZones(ctx, namespace(key.Namespace))).To(
					ConsistOf(reconcile.Request{NamespacedName: key}))
				Expect(e.r.crud().resourcesOfSharedZones(ctx, namespace(platform))).To(BeEmpty())
			})

			It("is reconciled when the Zone allows it, and when its namespace's labels change", func() {
				label := "kflare.dev/test-" + e.base
				createShared(nil)
				create(key, nil)
				e.runManager()
				e.eventuallyReady(reasonZoneReferenceNotAllowed)

				By("allowing the namespaces with a label, which its namespace lacks")
				allow(allowing(*selectorAllowing(map[string]string{label: "yes"}), hostname))
				Eventually(func(g Gomega) {
					g.Expect(e.ready().Message).To(ContainSubstring("no entry of its spec.allowedReferences allows the " +
						"namespace"))
				}).Should(Succeed())

				By("labelling its namespace")
				setLabel := func(value string) {
					ns := &corev1.Namespace{}
					Expect(k8sClient.Get(ctx, types.NamespacedName{Name: key.Namespace}, ns)).To(Succeed())
					if value == "" {
						delete(ns.Labels, label)
					} else {
						ns.Labels[label] = value
					}
					Expect(k8sClient.Update(ctx, ns)).To(Succeed())
				}
				setLabel("yes")
				DeferCleanup(setLabel, "")
				e.eventuallyReady("Synced")
				Expect(e.store.parentIDs).To(HaveEach(sharedID))
			})
		})

		It("returns nothing from its mappers when a list fails", func() {
			base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			failing := interceptor.NewClient(base, interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errors.New("list unavailable")
				},
			})
			r := t.newReconciler(failing, nil).crud()
			Expect(r.resourcesOfSharedZones(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: key.Namespace}})).
				To(BeNil())
		})

		It("refuses a zoneRef whose namespace is not a namespace's name", func() {
			obj := t.newResource(key, e.zoneName)
			t.inZoneNamespace(obj, "Not_A_Namespace")
			err := k8sClient.Create(ctx, obj)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
			Expect(err.Error()).To(ContainSubstring("spec.zoneRef.namespace"))
		})
	})
}
