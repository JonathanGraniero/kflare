/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	cf "github.com/cloudflare/cloudflare-go"
	"github.com/cloudflare/cloudflare-go/v7/packages/pagination"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

//+kubebuilder:rbac:groups=kflare.dev,resources=dnsrecords;workerroutes;wafcustomrules;ratelimitrules;managedrulesets,verbs=get;list;watch

// zoneObject and zoneListed are what a Zone's calls return, and its list holds.
type (
	zoneObject = cf.Zone
	zoneListed = cf.Zone
)

// ZoneAPI is the subset of the Cloudflare API used by a Zone. In production,
// *cfpkg.Client satisfies this interface because it embeds *cf.API.
type ZoneAPI interface {
	CreateZone(ctx context.Context, name string, jumpstart bool, account cf.Account, zoneType string) (cf.Zone, error)
	ZoneDetails(ctx context.Context, zoneID string) (cf.Zone, error)
	ListZones(ctx context.Context, z ...string) ([]cf.Zone, error)
	DeleteZone(ctx context.Context, zoneID string) (cf.ZoneID, error)
	EditZone(ctx context.Context, zoneID string, zoneOpts cf.ZoneOptions) (cf.Zone, error)
	ZoneSetPlan(ctx context.Context, zoneID string, planType string) error
	ZoneUpdatePlan(ctx context.Context, zoneID string, planType string) error
}

// defaultZoneAPI is the production factory: it delegates to cfpkg.New so that
// the returned *cfpkg.Client (which embeds *cf.API) satisfies ZoneAPI.
func defaultZoneAPI(token string) (ZoneAPI, error) {
	return cfpkg.New(token)
}

// zoneRatePlans maps spec.plan to the rate plan IDs of the zone subscription
// API. Cloudflare reports the current plan by the spec.plan names
// (plan.legacy_id).
var zoneRatePlans = map[string]string{
	"free":       "CF_FREE",
	"pro":        "CF_PRO",
	"business":   "CF_BIZ",
	"enterprise": "CF_ENT",
}

// zoneKind describes Zones to the shared CRUD reconciler. A Zone creates its
// zone or, as its adoption policy allows, adopts the account's zone with the
// same name, which another Zone may name too: two namespaces can each hold a
// Zone for one Cloudflare zone. It changes the zone's type in place, and its
// plan through its hooks. A zone deleted outside kflare is
// created again. A Zone waits for the resources that use it before it is
// deleted.
var zoneKind = crudResourceKind[*cloudflarev1alpha1.Zone, ZoneAPI, zoneObject, zoneListed]{
	name:      "Zone",
	noun:      "zone",
	newObject: func() *cloudflarev1alpha1.Zone { return &cloudflarev1alpha1.Zone{} },
	newList:   func() client.ObjectList { return &cloudflarev1alpha1.ZoneList{} },

	accountRef:     func(obj *cloudflarev1alpha1.Zone) string { return obj.Spec.AccountRef.Name },
	key:            func(obj *cloudflarev1alpha1.Zone) string { return obj.Spec.Name },
	claimLabel:     cloudflarev1alpha1.ZoneIDLabel,
	conflictReason: reasonNameConflict,
	sharedObjects:  true,
	statusID:       func(obj *cloudflarev1alpha1.Zone) string { return obj.Status.CloudflareMetadata.ZoneID },
	observe:        observeZone,

	id: func(current *cf.Zone) string { return current.ID },
	matches: func(obj *cloudflarev1alpha1.Zone, current *cf.Zone, _ map[string]string) bool {
		return current.Type == desiredZoneType(obj)
	},
	listedID:  func(listed *cf.Zone) string { return listed.ID },
	listedKey: func(listed *cf.Zone) string { return listed.Name },

	get: func(ctx context.Context, api ZoneAPI, _ *cloudflarev1alpha1.Zone, _, id string) (*cf.Zone, error) {
		zone, err := api.ZoneDetails(ctx, id)
		if err != nil {
			return nil, err
		}
		return &zone, nil
	},
	listAll: listAccountZones,
	create: func(ctx context.Context, api ZoneAPI, accountID string, obj *cloudflarev1alpha1.Zone,
		_ map[string]string) (*cf.Zone, error) {
		zone, err := api.CreateZone(ctx, obj.Spec.Name, true, cf.Account{ID: accountID}, desiredZoneType(obj))
		if err != nil {
			return nil, err
		}
		return &zone, nil
	},
	update: func(ctx context.Context, api ZoneAPI, obj *cloudflarev1alpha1.Zone, _, id string,
		_ map[string]string) (*cf.Zone, error) {
		zone, err := api.EditZone(ctx, id, cf.ZoneOptions{Type: desiredZoneType(obj)})
		if err != nil {
			return nil, err
		}
		return &zone, nil
	},
	delete: func(ctx context.Context, api ZoneAPI, _ *cloudflarev1alpha1.Zone, _, id string) error {
		_, err := api.DeleteZone(ctx, id)
		return err
	},

	dependents: zoneDependents,
	hooks: crudHooks[*cloudflarev1alpha1.Zone, ZoneAPI, zoneObject]{
		synced: syncZonePlan,
	},
}

// observeZone records a zone in status.
func observeZone(zone *cloudflarev1alpha1.Zone, current *cf.Zone) {
	metadata := &zone.Status.CloudflareMetadata
	metadata.ZoneID = current.ID
	metadata.NameServers = current.NameServers
	metadata.Status = current.Status
	metadata.Plan = current.Plan.LegacyID
	metadata.PendingPlan = current.PlanPending.LegacyID
}

// listAccountZones returns the account's zones named as the resource's spec
// says. The token may reach several accounts, and the same domain can exist in
// more than one of them (for example while it moves between accounts), so a
// zone from another account must never be adopted: deleting this resource would
// delete it.
func listAccountZones(ctx context.Context, api ZoneAPI, obj *cloudflarev1alpha1.Zone,
	accountID string) (*pagination.SinglePage[cf.Zone], error) {
	zones, err := api.ListZones(ctx, obj.Spec.Name)
	if err != nil {
		return nil, err
	}
	page := &pagination.SinglePage[cf.Zone]{}
	for _, zone := range zones {
		if zone.Account.ID == accountID {
			page.Result = append(page.Result, zone)
		}
	}
	return page, nil
}

// syncZonePlan subscribes the zone to spec.plan, when it is set and differs.
func syncZonePlan(ctx context.Context, c client.Client, api ZoneAPI, zone *cloudflarev1alpha1.Zone, _ string,
	current *cf.Zone) (ctrl.Result, bool, error) {
	ratePlan, drifted := zonePlanDrift(zone, *current)
	if !drifted {
		return ctrl.Result{}, false, nil
	}
	// A zone on the free plan has no subscription yet, so it needs one
	// created; a paid zone's subscription is updated in place.
	setPlan := api.ZoneUpdatePlan
	if !current.Plan.IsSubscribed {
		setPlan = api.ZoneSetPlan
	}
	if err := setPlan(ctx, current.ID, ratePlan); err != nil {
		result, err := handleCloudflareError(ctx, c, zone, err)
		return result, true, err
	}
	log.FromContext(ctx).Info("Changed zone plan", "zoneID", current.ID, "from", current.Plan.LegacyID,
		"to", zone.Spec.Plan)

	// Read the zone again so status shows the new (or pending) plan.
	updated, err := api.ZoneDetails(ctx, current.ID)
	if err != nil {
		result, err := handleCloudflareError(ctx, c, zone, err)
		return result, true, err
	}
	observeZone(zone, &updated)
	return ctrl.Result{}, false, nil
}

// zonePlanDrift returns the rate plan to subscribe cfZone to, or false when
// spec.plan is unset or Cloudflare already has it, either as the current
// plan or as a change scheduled for the end of the billing period.
func zonePlanDrift(zone *cloudflarev1alpha1.Zone, cfZone cf.Zone) (string, bool) {
	desired := zone.Spec.Plan
	if desired == "" || desired == cfZone.Plan.LegacyID || desired == cfZone.PlanPending.LegacyID {
		return "", false
	}
	return zoneRatePlans[desired], true
}

// desiredZoneType returns spec.type, or "full" when it is unset.
func desiredZoneType(zone *cloudflarev1alpha1.Zone) string {
	if zone.Spec.Type == "" {
		return "full"
	}
	return zone.Spec.Type
}
