/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"net/http"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// fakeZoneAccount is the account the shared specs' Zones use, which
// crudSpecEnv names after the kind: a zone's calls name no account.
const fakeZoneAccount = "acct-gen-zone"

// fakeZoneNew is what CreateZone was sent.
type fakeZoneNew struct {
	name, accountID, zoneType string
	jumpstart                 bool
}

// fakeZonePlans holds what a store's fake keeps across reconciles: the rate plan
// sent through each endpoint, the error to answer with, and the plan a zone
// reports once it has changed.
type fakeZonePlans struct {
	// calls counts the changes of plan, whatever they were sent.
	calls            int
	created, updated string
	err, rereadErr   error
	after            cf.ZonePlan
	pending          cf.ZonePlan
}

// fakeZoneAPI is an in-memory cloudflare-go v0.119 zones API on a fakeCRUDStore.
type fakeZoneAPI struct {
	*fakeCRUDStore[cf.Zone]
	account string
}

var _ ZoneAPI = fakeZoneAPI{}

func newFakeZoneAPI(store *fakeCRUDStore[cf.Zone], account string) fakeZoneAPI {
	if store.extra == nil {
		store.extra = &fakeZonePlans{}
	}
	return fakeZoneAPI{store, account}
}

func (f fakeZoneAPI) plans() *fakeZonePlans { return f.extra.(*fakeZonePlans) }

func (f fakeZoneAPI) CreateZone(_ context.Context, name string, jumpstart bool, account cf.Account,
	zoneType string) (cf.Zone, error) {
	f.sent = append(f.sent, fakeZoneNew{name: name, accountID: account.ID, zoneType: zoneType, jumpstart: jumpstart})
	created, err := f.create(account.ID, "", "", func(id string) cf.Zone {
		return cf.Zone{ID: id, Name: name, Type: zoneType, Account: cf.Account{ID: account.ID}, Status: "pending",
			NameServers: []string{"a.ns.cloudflare.com"}, Plan: cf.ZonePlan{LegacyID: "free"}}
	})
	if err != nil {
		return cf.Zone{}, err
	}
	return *created, nil
}

func (f fakeZoneAPI) ZoneDetails(_ context.Context, id string) (cf.Zone, error) {
	if plans := f.plans(); plans.rereadErr != nil && (plans.created != "" || plans.updated != "") {
		return cf.Zone{}, plans.rereadErr
	}
	zone, err := f.get(f.account, "", id)
	if err != nil {
		return cf.Zone{}, err
	}
	return *zone, nil
}

// ListZones returns the zones of every account the token reaches, as Cloudflare
// does.
func (f fakeZoneAPI) ListZones(_ context.Context, names ...string) ([]cf.Zone, error) {
	page, err := fakeListAll(f.fakeCRUDStore, f.account, "", func(current *cf.Zone) cf.Zone { return *current })
	if err != nil {
		return nil, err
	}
	var zones []cf.Zone
	for _, zone := range page.Result {
		if len(names) == 0 || zone.Name == names[0] {
			zones = append(zones, zone)
		}
	}
	return zones, nil
}

func (f fakeZoneAPI) EditZone(_ context.Context, id string, options cf.ZoneOptions) (cf.Zone, error) {
	zone, err := f.update(f.account, "", id, func(current *cf.Zone) { current.Type = options.Type })
	if err != nil {
		return cf.Zone{}, err
	}
	return *zone, nil
}

func (f fakeZoneAPI) DeleteZone(_ context.Context, id string) (cf.ZoneID, error) {
	return cf.ZoneID{ID: id}, f.delete(f.account, "", id)
}

func (f fakeZoneAPI) setPlan(id, ratePlan string, sent *string) error {
	plans := f.plans()
	plans.calls++
	if plans.err != nil {
		return plans.err
	}
	*sent = ratePlan
	zone := f.objects[id]
	zone.Plan, zone.PlanPending = plans.after, plans.pending
	f.objects[id] = zone
	return nil
}

func (f fakeZoneAPI) ZoneSetPlan(_ context.Context, id, ratePlan string) error {
	return f.setPlan(id, ratePlan, &f.plans().created)
}

func (f fakeZoneAPI) ZoneUpdatePlan(_ context.Context, id, ratePlan string) error {
	return f.setPlan(id, ratePlan, &f.plans().updated)
}

var _ = describeCRUDKind(crudKindSpecs[*cloudflarev1alpha1.Zone, ZoneAPI, zoneObject, zoneListed]{
	kind:           zoneKind,
	keyField:       "name",
	claimLabel:     "kflare.dev/zone-id",
	conflictReason: reasonNameConflict,
	newReconciler: func(c client.Client,
		newAPI func(string) (ZoneAPI, error)) crudKindReconciler[*cloudflarev1alpha1.Zone, ZoneAPI, zoneObject, zoneListed] {
		return &ZoneReconciler{Client: c, Scheme: c.Scheme(), NewZoneAPI: newAPI}
	},
	newFake:    func(store *fakeCRUDStore[cf.Zone]) ZoneAPI { return newFakeZoneAPI(store, fakeZoneAccount) },
	defaultAPI: defaultZoneAPI,
	service:    &cfpkg.Client{},
	newResource: func(key types.NamespacedName, parent string) *cloudflarev1alpha1.Zone {
		return &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec: cloudflarev1alpha1.ZoneSpec{
				AccountRef:     corev1.LocalObjectReference{Name: parent},
				ZoneSpecExtras: cloudflarev1alpha1.ZoneSpecExtras{Name: "example.com"},
			},
		}
	},
	seed: func(id, key string, _ map[string]string) cf.Zone {
		return cf.Zone{ID: id, Name: key, Type: "full", Account: cf.Account{ID: fakeZoneAccount}}
	},
	created: func(obj *cloudflarev1alpha1.Zone, _ map[string]string, current *cf.Zone) bool {
		return current.Name == obj.Spec.Name && current.Type == obj.Spec.Type
	},
	sent: func(obj *cloudflarev1alpha1.Zone, _ map[string]string, sent any) bool {
		params := sent.(fakeZoneNew)
		return params.name == obj.Spec.Name && params.zoneType == obj.Spec.Type && params.accountID == fakeZoneAccount &&
			params.jumpstart
	},
	observed: func(obj *cloudflarev1alpha1.Zone, current *cf.Zone) bool {
		metadata := obj.Status.CloudflareMetadata
		return metadata.ZoneID == current.ID && metadata.Status == current.Status &&
			len(metadata.NameServers) == len(current.NameServers) && metadata.Plan == current.Plan.LegacyID
	},
	defaulted: func(obj *cloudflarev1alpha1.Zone) bool { return obj.Spec.Type == "full" },
	changes: []crudSpecEdit[*cloudflarev1alpha1.Zone]{
		{field: "type", what: "changing type", edit: func(obj *cloudflarev1alpha1.Zone) { obj.Spec.Type = "partial" }},
	},
	immutable: []crudSpecEdit[*cloudflarev1alpha1.Zone]{
		{field: "accountRef", what: "changing accountRef",
			edit: func(obj *cloudflarev1alpha1.Zone) { obj.Spec.AccountRef.Name = "other-account" }},
		{field: "name", what: "changing name",
			edit: func(obj *cloudflarev1alpha1.Zone) { obj.Spec.Name = "example.org" }},
	},
	invalid: []crudSpecEdit[*cloudflarev1alpha1.Zone]{
		{field: "name", what: "a name without a dot",
			edit: func(obj *cloudflarev1alpha1.Zone) { obj.Spec.Name = "example" }},
		{field: "plan", what: "a plan outside its enum",
			edit: func(obj *cloudflarev1alpha1.Zone) { obj.Spec.Plan = "platinum" }},
		{field: "type", what: "a type outside its enum",
			edit: func(obj *cloudflarev1alpha1.Zone) { obj.Spec.Type = "secondary" }},
	},
})

var _ = Describe("Zone hooks", func() {
	const (
		ns          = "default"
		accountName = "zone-hooks-account"
		secretName  = "zone-hooks-token"
		accountID   = "zone-hooks-account-id"
	)
	ctx := context.Background()
	key := types.NamespacedName{Name: "site", Namespace: ns}
	var store *fakeCRUDStore[cf.Zone]
	var plans *fakeZonePlans
	var r *ZoneReconciler

	create := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(forceDelete, ctx, obj.DeepCopyObject().(client.Object), client.ObjectKeyFromObject(obj))
	}
	createZone := func(plan string, annotations map[string]string) {
		create(&cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns, Annotations: annotations,
				Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.ZoneSpec{
				AccountRef:     corev1.LocalObjectReference{Name: accountName},
				ZoneSpecExtras: cloudflarev1alpha1.ZoneSpecExtras{Name: "example.com", Plan: plan},
			},
		})
	}
	getZone := func() *cloudflarev1alpha1.Zone {
		zone := &cloudflarev1alpha1.Zone{}
		Expect(k8sClient.Get(ctx, key, zone)).To(Succeed())
		return zone
	}
	condition := func() metav1.Condition {
		cond := meta.FindStatusCondition(getZone().Status.Conditions, cloudflarev1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil(), "no Ready condition")
		return *cond
	}
	reconcileZone := func() (ctrl.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	}
	// onPlan stores the account's zone on plan, subscribed or not.
	onPlan := func(plan string, subscribed bool) {
		store.objects["zone-1"] = cf.Zone{ID: "zone-1", Name: "example.com", Type: "full",
			Account: cf.Account{ID: accountID},
			Plan:    cf.ZonePlan{LegacyID: plan, IsSubscribed: subscribed}}
	}
	adopted := map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt}

	BeforeEach(func() {
		store = newFakeCRUDStore[cf.Zone](0, "")
		api := newFakeZoneAPI(store, accountID)
		plans = api.plans()
		r = &ZoneReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			NewZoneAPI: func(string) (ZoneAPI, error) { return api, nil }}
		create(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
		})
		account := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      accountID,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
			},
		}
		create(account)
		account.Status.Conditions = []metav1.Condition{{
			Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Validated", LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, account)).To(Succeed())
	})

	It("creates a zone rather than adopt one of another account the token reaches", func() {
		store.objects["elsewhere"] = cf.Zone{ID: "elsewhere", Name: "example.com", Type: "full",
			Account: cf.Account{ID: "another-account"}}
		createZone("", map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdoptOrCreate})
		_, err := reconcileZone()
		Expect(err).NotTo(HaveOccurred())
		Expect(getZone().Status.CloudflareMetadata.ZoneID).NotTo(Equal("elsewhere"))
		Expect(getZone().Annotations).NotTo(HaveKey(reconciler.AdoptedAnnotation))
		Expect(store.objects).To(HaveLen(2))
	})

	It("lets a second Zone adopt the zone another Zone manages, and keeps it for the first", func() {
		onPlan("free", false)
		createZone("", adopted)
		_, err := reconcileZone()
		Expect(err).NotTo(HaveOccurred())
		Expect(condition().Reason).To(Equal("Synced"))
		Expect(getZone().Labels).To(HaveKeyWithValue(cloudflarev1alpha1.ZoneIDLabel, "zone-1"))

		other := types.NamespacedName{Name: "site", Namespace: "kube-public"}
		create(&cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: other.Name, Namespace: other.Namespace, Annotations: adopted,
				Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.ZoneSpec{
				AccountRef:     corev1.LocalObjectReference{Name: accountName},
				ZoneSpecExtras: cloudflarev1alpha1.ZoneSpecExtras{Name: "example.com"},
			},
		})
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: other})
		Expect(err).NotTo(HaveOccurred())
		second := &cloudflarev1alpha1.Zone{}
		Expect(k8sClient.Get(ctx, other, second)).To(Succeed())
		Expect(second.Status.CloudflareMetadata.ZoneID).To(Equal("zone-1"))
		cond := meta.FindStatusCondition(second.Status.Conditions, cloudflarev1alpha1.ConditionReady)
		Expect(cond.Reason).To(Equal("Synced"))
	})

	Describe("the plan", func() {
		It("leaves the plan alone when spec.plan is unset", func() {
			onPlan("pro", true)
			createZone("", adopted)
			_, err := reconcileZone()
			Expect(err).NotTo(HaveOccurred())
			Expect(plans.calls).To(BeZero())
			Expect(getZone().Status.CloudflareMetadata.Plan).To(Equal("pro"))
		})

		It("leaves a change Cloudflare has scheduled alone when spec.plan is unset", func() {
			onPlan("business", true)
			zone := store.objects["zone-1"]
			zone.PlanPending = cf.ZonePlan{LegacyID: "pro"}
			store.objects["zone-1"] = zone
			createZone("", adopted)
			_, err := reconcileZone()
			Expect(err).NotTo(HaveOccurred())
			Expect(plans.calls).To(BeZero())
			Expect(getZone().Status.CloudflareMetadata.PendingPlan).To(Equal("pro"))
		})

		It("does nothing when the zone is already on the plan", func() {
			onPlan("pro", true)
			createZone("pro", adopted)
			_, err := reconcileZone()
			Expect(err).NotTo(HaveOccurred())
			Expect(plans.calls).To(BeZero())
		})

		It("creates a subscription to upgrade a free zone", func() {
			onPlan("free", false)
			plans.after = cf.ZonePlan{LegacyID: "pro", IsSubscribed: true}
			createZone("pro", adopted)
			_, err := reconcileZone()
			Expect(err).NotTo(HaveOccurred())
			Expect(plans.created).To(Equal("CF_PRO"))
			Expect(plans.updated).To(BeEmpty())
			Expect(getZone().Status.CloudflareMetadata.Plan).To(Equal("pro"), "status shows the new plan")
			Expect(condition().Reason).To(Equal("Synced"))
		})

		It("updates the subscription of a paid zone and reports a scheduled downgrade", func() {
			onPlan("business", true)
			plans.after = cf.ZonePlan{LegacyID: "business", IsSubscribed: true}
			plans.pending = cf.ZonePlan{LegacyID: "pro"}
			createZone("pro", adopted)
			_, err := reconcileZone()
			Expect(err).NotTo(HaveOccurred())
			Expect(plans.updated).To(Equal("CF_PRO"))
			Expect(plans.created).To(BeEmpty())
			metadata := getZone().Status.CloudflareMetadata
			Expect(metadata.Plan).To(Equal("business"))
			Expect(metadata.PendingPlan).To(Equal("pro"))
		})

		It("does not change the plan again while the change is pending", func() {
			onPlan("business", true)
			zone := store.objects["zone-1"]
			zone.PlanPending = cf.ZonePlan{LegacyID: "pro"}
			store.objects["zone-1"] = zone
			createZone("pro", adopted)
			_, err := reconcileZone()
			Expect(err).NotTo(HaveOccurred())
			Expect(plans.calls).To(BeZero())
		})

		It("sets TerminalError when Cloudflare refuses the plan change", func() {
			onPlan("free", false)
			plans.err = v7APIError(http.MethodPost, "subscription", http.StatusForbidden, 1000, "no payment method")
			createZone("pro", adopted)
			result, err := reconcileZone()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(condition().Reason).To(Equal("TerminalError"))
			Expect(getZone().Status.CloudflareMetadata.ZoneID).To(Equal("zone-1"), "status keeps the zone")
		})

		It("returns a failed read of the zone after a plan change for back-off", func() {
			onPlan("free", false)
			plans.rereadErr = errors.New("unavailable")
			createZone("pro", adopted)
			_, err := reconcileZone()
			Expect(err).To(MatchError("unavailable"))
		})
	})

	It("sends full for an unset type, in memory too", func() {
		Expect(desiredZoneType(&cloudflarev1alpha1.Zone{})).To(Equal("full"))
	})
})
