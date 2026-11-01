/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"strings"
	"testing"
)

func hookedEntry() hookedKindConfig {
	return hookedKindConfig{Kind: "Zone", ShortName: "cfzone", Noun: "zone", Doc: "Zone is a zone.", Scope: scopeAccount,
		ID: idConfig{Field: "id", Status: "zoneID", Doc: "ZoneID is the zone's ID."}, ClaimLabel: "kflare.dev/zone-id",
		SpecExtras: "ZoneSpecExtras", MetadataExtras: "ZoneMetadataExtras", StatusExtras: "ZoneStatusExtras",
		Columns: []columnConfig{{Name: "Domain", Type: "string", Path: ".spec.name"}}}
}

func TestHookedKindTypes(t *testing.T) {
	source, err := emitTypes(buildHookedKind(hookedEntry()).kind, testHeader)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"AccountRef corev1.LocalObjectReference `json:\"accountRef\"`",
		"ZoneSpecExtras `json:\",inline\"`", "ZoneMetadataExtras `json:\",inline\"`", "ZoneStatusExtras `json:\",inline\"`",
		"ZoneID string `json:\"zoneID,omitempty\"`", `name="Domain",type=string,JSONPath=` + "`.spec.name`",
		`name="Ready"`, `const ZoneIDLabel = "kflare.dev/zone-id"`} {
		if !strings.Contains(string(source), want) {
			t.Errorf("the types lack %q:\n%s", want, source)
		}
	}
}

// A kind without a scope holds no accountRef: its hooks find its account.
func TestHookedKindWithoutScope(t *testing.T) {
	entry := hookedEntry()
	entry.Scope = ""
	k := buildHookedKind(entry)
	if k.usesAccount || len(k.spec) != 0 {
		t.Errorf("usesAccount = %t, spec = %+v", k.usesAccount, k.spec)
	}
}

func TestHookedKindController(t *testing.T) {
	source, err := emitHookedController(buildHookedKind(hookedEntry()), testHeader)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"type ZoneReconciler struct", "NewZoneAPI func(token string) (ZoneAPI, error)",
		"resources=zones,verbs=get;list;watch;create;update;patch;delete", "resources=zones/status,verbs=get;update;patch",
		"resources=zones/finalizers,verbs=update", "resources=cloudflareaccounts,verbs=get;list;watch",
		"return r.crud().reconcile(ctx, req)", "return r.crud().setup(mgr, r)", "newAPI = defaultZoneAPI", "kind: zoneKind",
		"crudResourceReconciler[*cloudflarev1alpha1.Zone, ZoneAPI, zoneObject, zoneListed]"} {
		if !strings.Contains(string(source), want) {
			t.Errorf("the controller lacks %q:\n%s", want, source)
		}
	}
}

// A hooked kind registers, and counts among its account's dependents only when
// it names the account.
func TestHookedKindsRegister(t *testing.T) {
	scoped := buildHookedKind(hookedEntry())
	entry := hookedEntry()
	entry.Kind, entry.Scope = "TunnelConfiguration", ""
	source, err := emitKinds(nil, nil, nil, []*hookedKind{scoped, buildHookedKind(entry)}, testHeader)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"(&ZoneReconciler{", "(&TunnelConfigurationReconciler{",
		`dependents("Zone", func() client.ObjectList { return &cloudflarev1alpha1.ZoneList{} }`} {
		if !strings.Contains(string(source), want) {
			t.Errorf("the registration lacks %q:\n%s", want, source)
		}
	}
	if strings.Contains(string(source), `dependents("TunnelConfiguration"`) {
		t.Errorf("a kind without an accountRef is its account's dependent:\n%s", source)
	}
}
