/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"fmt"
	"go/types"
	"maps"
	"slices"
	"strings"
	"testing"
)

// kvEntry is an entry for the KV namespace service that ignores nothing.
func kvEntry() kindConfig {
	return kindConfig{
		Kind: "TestKV", Service: "kv.NamespaceService", Scope: "account", ShortName: "testkv", Noun: "KV namespace",
		Doc: "TestKV is a KV namespace.", ID: idConfig{Field: "id", Status: "namespaceID"},
		ClaimLabel: "kflare.dev/test-kv-namespace-id",
	}
}

// listEntry is an entry for the custom list service that ignores its numbers.
func listEntry() kindConfig {
	entry := kvEntry()
	entry.Service, entry.ID = "rules.ListService", idConfig{Field: "id", Status: "listID"}
	entry.Ignore = map[string]string{"num_items": "a number", "num_referencing_filters": "a number"}
	return entry
}

// The synthetic things service: kflare-gen's behavior where no real service
// shows it.
var (
	thingNew = route{verb: "post", path: "/accounts/{account_id}/things"}
	thingGet = route{verb: "get", path: "/accounts/{account_id}/things/{thing_id}"}
)

// thingService returns a things service with an account ID and an ID, plus
// the fields given.
func thingService(create, update, object []sdkField) *service {
	str := types.Typ[types.String]
	return &service{
		create: append([]sdkField{
			{goName: "AccountID", apiName: "account_id", in: inPath, typ: str, required: true},
		}, create...),
		update:   update,
		object:   append([]sdkField{{goName: "ID", apiName: "id", in: inBody, typ: str, required: true}}, object...),
		newRoute: thingNew,
		getRoute: thingGet,
	}
}

// thingSpec returns a spec for the things service: request and result are
// the JSON properties of the create request and of the object Get returns.
func thingSpec(t *testing.T, request, result string) *apiSpec {
	t.Helper()
	response := fmt.Sprintf(`{"200": {"content": {"application/json": {"schema":
		{"properties": {"result": {"properties": %s}}}}}}}`, result)
	spec, err := parseSpec(fmt.Appendf(nil, `{"paths": {
		"/accounts/{account_id}/things": {"post": {
			"requestBody": {"content": {"application/json": {"schema": {"properties": %s}}}},
			"responses": %s}},
		"/accounts/{account_id}/things/{thing_id}": {"get": {"responses": %s}}}}`, request, response, response))
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// fieldNamed returns the field goName of fields.
func fieldNamed(t *testing.T, fields []field, goName string) field {
	t.Helper()
	i := slices.IndexFunc(fields, func(f field) bool { return f.goName == goName })
	if i < 0 {
		t.Fatalf("no field %s", goName)
	}
	return fields[i]
}

func TestBuildKindMapsTheKVNamespaceService(t *testing.T) {
	k, err := buildKind(kvEntry(), loadTestService(t, "kv.NamespaceService"), loadTestSpec(t))
	if err != nil {
		t.Fatalf("buildKind: %v", err)
	}
	names := func(fields []field) (out []string) {
		for _, f := range fields {
			out = append(out, f.goName)
		}
		return out
	}
	if got := names(k.spec); !slices.Equal(got, []string{"AccountRef", "Title", "Jurisdiction", "Mode"}) {
		t.Errorf("spec = %q", got)
	}
	if got := names(k.metadata); !slices.Equal(got, []string{"NamespaceID", "SupportsURLEncoding"}) {
		t.Errorf("metadata = %q", got)
	}

	if title := fieldNamed(t, k.spec, "Title"); title.optional ||
		!slices.Equal(title.markers(), []string{"+kubebuilder:validation:MaxLength=512"}) {
		t.Errorf("title, required, updatable and at most 512 characters in the spec: %+v %q", title, title.markers())
	}
	jurisdiction := fieldNamed(t, k.spec, "Jurisdiction")
	if !jurisdiction.optional || jurisdiction.goType != "string" || !slices.Equal(jurisdiction.markers(), []string{
		"+kubebuilder:validation:Enum=eu;fedramp;us", immutable("jurisdiction"),
	}) {
		t.Errorf("jurisdiction, an optional create-only enum: %+v %q", jurisdiction, jurisdiction.markers())
	}
	mode := fieldNamed(t, k.spec, "Mode")
	if !mode.optional || mode.goType != "string" || !slices.Equal(mode.markers(), []string{
		"+kubebuilder:validation:Enum=instant", immutable("mode"),
	}) {
		t.Errorf("mode, an optional create-only enum: %+v %q", mode, mode.markers())
	}
	if len(k.specRules) != 2 || !strings.Contains(k.specRules[0], "has(self.jurisdiction) == has(oldSelf.jurisdiction)") ||
		!strings.Contains(k.specRules[1], "has(self.mode) == has(oldSelf.mode)") {
		t.Errorf("specRules = %q, want the rules that keep jurisdiction and mode from being added or removed",
			k.specRules)
	}
	if url := fieldNamed(t, k.metadata, "SupportsURLEncoding"); url.goType != "bool" || !url.optional ||
		len(url.markers()) != 0 {
		t.Errorf("a status bool is optional, no pointer, and unvalidated: %+v", url)
	}

	columns := make([]string, 0, len(k.printColumns))
	for _, c := range k.printColumns {
		columns = append(columns, c.name+" "+c.typ+" "+c.jsonPath)
	}
	if !slices.Equal(columns, []string{
		"Namespace ID string .status.cloudflareMetadata.namespaceID",
		`Ready string .status.conditions[?(@.type=="Ready")].status`,
		"Age date .metadata.creationTimestamp",
	}) {
		t.Errorf("printColumns = %q", columns)
	}
}

func TestBuildKindReplacesValidation(t *testing.T) {
	entry := kvEntry()
	minLength, maxLength := int64(1), int64(512)
	entry.Fields = map[string]fieldConfig{"title": {Validation: &validationConfig{
		MinLength: &minLength, MaxLength: &maxLength, Reason: "an empty title is refused",
	}}}
	k, err := buildKind(entry, loadTestService(t, "kv.NamespaceService"), loadTestSpec(t))
	if err != nil {
		t.Fatalf("buildKind: %v", err)
	}
	if got := fieldNamed(t, k.spec, "Title").markers(); !slices.Equal(got, []string{
		"+kubebuilder:validation:MinLength=1", "+kubebuilder:validation:MaxLength=512",
	}) {
		t.Errorf("title's markers = %q, want the entry's validation in place of the spec's", got)
	}
}

func TestBuildKindFixesValues(t *testing.T) {
	entry := listEntry()
	entry.Fixed = map[string]string{"kind": "ip"}
	k, err := buildKind(entry, loadTestService(t, "rules.ListService"), loadTestSpec(t))
	if err != nil {
		t.Fatalf("buildKind: %v", err)
	}
	if slices.ContainsFunc(k.spec, func(f field) bool { return f.jsonName == "kind" }) {
		t.Error("a fixed field is not in the spec")
	}
	if !maps.Equal(k.fixed, map[string]string{"kind": "ip"}) {
		t.Errorf("fixed = %v", k.fixed)
	}
	if name := fieldNamed(t, k.spec, "Name"); !slices.Equal(name.markers(), []string{
		"+kubebuilder:validation:MaxLength=50", "+kubebuilder:validation:Pattern=`^[a-zA-Z0-9_]+$`", immutable("name"),
	}) {
		t.Errorf("name's markers = %q, want the spec's", name.markers())
	}
}

func TestBuildKindChecksFixedValuesAgainstTheSDK(t *testing.T) {
	svc := thingService([]sdkField{
		{goName: "Kind", apiName: "kind", in: inBody, typ: types.Typ[types.String], required: true, enum: []string{"a", "b"}},
	}, nil, nil)
	spec := thingSpec(t, `{"kind": {"type": "string"}}`, `{}`)
	entry := kvEntry()
	entry.Fixed = map[string]string{"kind": "b"}
	if _, err := buildKind(entry, svc, spec); err != nil {
		t.Errorf("a value of the SDK's enum: %v", err)
	}
	entry.Fixed = map[string]string{"kind": "c"}
	if _, err := buildKind(entry, svc, spec); err == nil || !strings.Contains(err.Error(), `"c" is not one of a, b`) {
		t.Errorf("a value outside the SDK's enum: err = %v", err)
	}
}

func TestBuildKindReadsTheSpec(t *testing.T) {
	float, str := types.Typ[types.Float64], types.Typ[types.String]
	svc := thingService(
		[]sdkField{
			{goName: "Count", apiName: "count", in: inBody, typ: float},
			{goName: "Limit", apiName: "limit", in: inBody, typ: float, required: true},
			{goName: "Mode", apiName: "mode", in: inBody, typ: str},
			{goName: "Label", apiName: "label", in: inBody, typ: str},
			{goName: "Weight", apiName: "weight", in: inBody, typ: float},
		},
		[]sdkField{
			{goName: "Weight", apiName: "weight", in: inBody, typ: float},
			{goName: "Count", apiName: "count", in: inBody, typ: float},
			{goName: "Limit", apiName: "limit", in: inBody, typ: float},
			{goName: "Mode", apiName: "mode", in: inBody, typ: str},
			{goName: "Label", apiName: "label", in: inBody, typ: str},
		},
		[]sdkField{{goName: "Size", apiName: "size", in: inBody, typ: float}},
	)
	spec := thingSpec(t, `{
		"count": {"type": "integer", "minimum": 1, "maximum": 10, "exclusiveMaximum": true},
		"weight": {"type": "integer", "minimum": 0},
		"limit": {"allOf": [{"$ref": "#/components/schemas/limit"}]},
		"mode": {"type": "string", "enum": ["fast", "slow"], "default": "fast"},
		"label": {"$ref": "#/components/schemas/label"}}`, `{"size": {"type": "integer", "maximum": 5}}`)
	spec.Components.Schemas = map[string]*schema{
		"limit": {Type: "integer", Minimum: new(0.5), ExclusiveMinimum: true},
		"label": {Type: "string", MinLength: new(int64(2)), MaxLength: new(int64(8)), Pattern: "^[a-z]+$"},
	}
	entry := kvEntry()
	two, three := int64(2), int64(3)
	entry.Fields = map[string]fieldConfig{"weight": {Validation: &validationConfig{Minimum: &two, Maximum: &three,
		Reason: "a narrower range"}}}
	k, err := buildKind(entry, svc, spec)
	if err != nil {
		t.Fatalf("buildKind: %v", err)
	}
	for goName, want := range map[string][]string{
		"Count": {"+kubebuilder:validation:Minimum=1", "+kubebuilder:validation:Maximum=10",
			"+kubebuilder:validation:ExclusiveMaximum=true"},
		"Weight": {"+kubebuilder:validation:Minimum=2", "+kubebuilder:validation:Maximum=3"},
		"Limit":  {"+kubebuilder:validation:Minimum=0.5", "+kubebuilder:validation:ExclusiveMinimum=true"},
		"Mode":   {"+kubebuilder:validation:Enum=fast;slow", `+kubebuilder:default="fast"`},
		"Label": {"+kubebuilder:validation:MinLength=2", "+kubebuilder:validation:MaxLength=8",
			"+kubebuilder:validation:Pattern=`^[a-z]+$`"},
	} {
		if got := fieldNamed(t, k.spec, goName).markers(); !slices.Equal(got, want) {
			t.Errorf("%s's markers = %q, want %q", goName, got, want)
		}
	}
	if count := fieldNamed(t, k.spec, "Count"); count.goType != "*int64" {
		t.Errorf("an optional integer is an *int64: %+v", count)
	}
	if limit := fieldNamed(t, k.spec, "Limit"); limit.goType != "int64" {
		t.Errorf("a required integer is an int64: %+v", limit)
	}
	if size := fieldNamed(t, k.metadata, "Size"); size.goType != "int64" || len(size.markers()) != 0 {
		t.Errorf("a status integer is an unvalidated int64: %+v", size)
	}
}

func TestBuildKindMapsBooleans(t *testing.T) {
	boolean := types.Typ[types.Bool]
	svc := thingService(
		[]sdkField{
			{goName: "Paused", apiName: "paused", in: inBody, typ: boolean},
			{goName: "Proxied", apiName: "proxied", in: inBody, typ: boolean, required: true},
		},
		[]sdkField{{goName: "Paused", apiName: "paused", in: inBody, typ: boolean}},
		nil,
	)
	entry := kvEntry()
	entry.Fields = map[string]fieldConfig{"paused": {PrintColumn: "Paused"}}
	k, err := buildKind(entry, svc, thingSpec(t, `{"paused": {"type": "boolean", "default": false}}`, `{}`))
	if err != nil {
		t.Fatalf("buildKind: %v", err)
	}
	if paused := fieldNamed(t, k.spec, "Paused"); paused.goType != "*bool" || !paused.optional || paused.def != "false" {
		t.Errorf("an optional bool is a pointer, with the spec's default: %+v", paused)
	}
	if proxied := fieldNamed(t, k.spec, "Proxied"); proxied.goType != "bool" || proxied.optional {
		t.Errorf("a required bool is no pointer: %+v", proxied)
	}
	if k.printColumns[0] != (printColumn{name: "Paused", typ: "boolean", jsonPath: ".spec.paused"}) {
		t.Errorf("printColumns[0] = %+v", k.printColumns[0])
	}
}

func TestBuildKindLeavesEnumsOffStatus(t *testing.T) {
	svc := thingService(nil, nil, []sdkField{
		{goName: "Status", apiName: "status", in: inBody, typ: types.Typ[types.String], enum: []string{"active", "pending"}},
	})
	spec := thingSpec(t, `{}`, `{"status": {"type": "string", "enum": ["active", "pending"]}}`)
	k, err := buildKind(kvEntry(), svc, spec)
	if err != nil {
		t.Fatalf("buildKind: %v", err)
	}
	if status := fieldNamed(t, k.metadata, "Status"); len(status.markers()) != 0 {
		t.Errorf("a status field carries no enum, so a value Cloudflare adds cannot fail a status update: %+v", status)
	}
}

func TestBuildKindRefuses(t *testing.T) {
	str, boolean, float := types.Typ[types.String], types.Typ[types.Bool], types.Typ[types.Float64]
	one := int64(1)
	with := func(entry kindConfig, change func(*kindConfig)) kindConfig {
		change(&entry)
		return entry
	}
	thing := func(fields ...sdkField) *service { return thingService(fields, nil, nil) }
	tests := []struct {
		name    string
		service string
		svc     *service
		spec    string // a things spec's request properties, for svc
		entry   kindConfig
		want    string
	}{
		{name: "a number that need not be whole", service: "rules.ListService",
			entry: with(listEntry(), func(e *kindConfig) { e.Ignore = nil }),
			want:  "num_items is a number that need not be whole"},
		{name: "a float64 the spec does not describe", svc: thing(sdkField{apiName: "n", in: inBody, typ: float}),
			spec: `{}`, entry: kvEntry(), want: "does not say it is an integer"},
		{name: "a sensitive field", svc: thing(sdkField{apiName: "password", in: inBody, typ: str}),
			spec: `{"password": {"type": "string", "x-sensitive": true}}`, entry: kvEntry(), want: "is sensitive"},
		{name: "a default that does not fit", svc: thing(sdkField{apiName: "mode", in: inBody, typ: str}),
			spec: `{"mode": {"type": "string", "default": 5}}`, entry: kvEntry(), want: "5 does not fit a string"},
		{name: "a fixed value outside the enum", service: "rules.ListService",
			entry: with(listEntry(), func(e *kindConfig) { e.Fixed = map[string]string{"kind": "ipv7"} }),
			want:  `"ipv7" is not one of ip, redirect, hostname, asn`},
		{name: "a fixed field that is not a string", svc: thing(sdkField{apiName: "paused", in: inBody, typ: boolean}),
			spec: `{}`, entry: with(kvEntry(), func(e *kindConfig) { e.Fixed = map[string]string{"paused": "true"} }),
			want: "only a string the request body holds can be fixed"},
		{name: "a fixed field the service lacks", service: "kv.NamespaceService",
			entry: with(kvEntry(), func(e *kindConfig) { e.Fixed = map[string]string{"nope": "x"} }),
			want:  "fixed.nope: the service has no such field"},
		{name: "lengths on a bool", svc: thing(sdkField{apiName: "paused", in: inBody, typ: boolean}), spec: `{}`,
			entry: with(kvEntry(), func(e *kindConfig) {
				e.Fields = map[string]fieldConfig{"paused": {Validation: &validationConfig{MaxLength: &one, Reason: "x"}}}
			}),
			want: "lengths and patterns need a string field"},
		{name: "a range on a string", service: "kv.NamespaceService",
			entry: with(kvEntry(), func(e *kindConfig) {
				e.Fields = map[string]fieldConfig{"title": {Validation: &validationConfig{Minimum: &one, Reason: "x"}}}
			}),
			want: "a range needs an integer field"},
		{name: "validation on a status field", service: "kv.NamespaceService",
			entry: with(kvEntry(), func(e *kindConfig) {
				e.Fields = map[string]fieldConfig{"supports_url_encoding": {Validation: &validationConfig{Reason: "x"}}}
			}),
			want: "only spec fields are validated"},
		{name: "an operation the subset lacks", svc: &service{
			create:   []sdkField{{apiName: "account_id", in: inPath, typ: str, required: true}},
			newRoute: route{verb: "post", path: "/accounts/{account_id}/elsewhere"},
		}, spec: `{}`, entry: kvEntry(), want: "the OpenAPI subset has no post /accounts/{account_id}/elsewhere"},
		{name: "a Get the subset lacks", svc: &service{
			create:   []sdkField{{apiName: "account_id", in: inPath, typ: str, required: true}},
			newRoute: thingNew, getRoute: route{verb: "get", path: "/accounts/{account_id}/elsewhere/{id}"},
		}, spec: `{}`, entry: kvEntry(), want: "the OpenAPI subset has no get /accounts/{account_id}/elsewhere/{id}"},
		{name: "a property the subset cannot read", svc: thing(sdkField{apiName: "x", in: inBody, typ: str}),
			spec: `{"x": {"allOf": [{"$ref": "#/components/schemas/missing"}]}}`, entry: kvEntry(),
			want: "x in the OpenAPI spec"},
		{name: "an ignore naming no field", service: "kv.NamespaceService",
			entry: with(kvEntry(), func(e *kindConfig) { e.Ignore = map[string]string{"nope": "why"} }),
			want:  "ignore.nope: the service has no such field"},
		{name: "an adjustment naming no field", service: "kv.NamespaceService",
			entry: with(kvEntry(), func(e *kindConfig) { e.Fields = map[string]fieldConfig{"nope": {}} }),
			want:  "fields.nope: the service has no such field"},
		{name: "an ignored required field", service: "kv.NamespaceService",
			entry: with(kvEntry(), func(e *kindConfig) { e.Ignore = map[string]string{"title": "why"} }),
			want:  "title is required to create the object"},
		{name: "an unknown ID", service: "kv.NamespaceService",
			entry: with(kvEntry(), func(e *kindConfig) { e.ID.Field = "uuid" }), want: "the object has no field uuid"},
		{name: "an ID that is not a string", service: "kv.NamespaceService",
			entry: with(kvEntry(), func(e *kindConfig) { e.ID.Field = "supports_url_encoding" }), want: "not a string"},
		{name: "a header parameter", service: "r2.BucketService",
			entry: with(kvEntry(), func(e *kindConfig) { e.ID.Field = "name" }),
			want:  "cf-r2-jurisdiction is a header parameter"},
		{name: "a zone-level service", service: "workers.RouteService", entry: kvEntry(),
			want: "scope account, but New takes no account_id"},
		{name: "a field only an update sets",
			svc:  thingService(nil, []sdkField{{goName: "Color", apiName: "color", in: inBody, typ: str}}, nil),
			spec: `{}`, entry: kvEntry(), want: "color can be updated but not set at creation"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, spec := tc.svc, loadTestSpec(t)
			if svc == nil {
				svc = loadTestService(t, tc.service)
			} else {
				spec = thingSpec(t, tc.spec, `{}`)
			}
			_, err := buildKind(tc.entry, svc, spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("buildKind: err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestCheckDefault(t *testing.T) {
	for _, tc := range []struct {
		goType, def string
		ok          bool
	}{
		{"string", `"a"`, true}, {"*string", "5", false},
		{"*bool", "true", true}, {"bool", "yes", false},
		{"*int64", "42", true}, {"int64", "4.2", false},
		{"[]string", `"a"`, false},
	} {
		if err := checkDefault(tc.goType, tc.def); (err == nil) != tc.ok {
			t.Errorf("checkDefault(%s, %s) = %v, want ok %t", tc.goType, tc.def, err, tc.ok)
		}
	}
}

func TestBuildKindRefusesWhatTheSubsetCannotRead(t *testing.T) {
	get := `"/accounts/{account_id}/things/{thing_id}": {"get": {"responses": {"200": {"content": {"application/json":
		{"schema": {"properties": {"result": {"properties": {}}}}}}}}}}`
	for name, tc := range map[string]struct{ paths, want string }{
		"a dangling create request": {`"/accounts/{account_id}/things": {"post": {"requestBody":
			{"$ref": "#/components/requestBodies/missing"}}}, ` + get,
			"post /accounts/{account_id}/things: $ref #/components/requestBodies/missing is not in the OpenAPI subset"},
		"a Get without a result": {`"/accounts/{account_id}/things": {"post": {}},
			"/accounts/{account_id}/things/{thing_id}": {"get": {"responses": {"200": {"content": {"application/json":
			{"schema": {"properties": {}}}}}}}}`,
			"get /accounts/{account_id}/things/{thing_id}: the response has no result"},
	} {
		spec := parseTestSpec(t, `{"paths": {`+tc.paths+`}}`)
		if _, err := buildKind(kvEntry(), thingService(nil, nil, nil), spec); err == nil ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, tc.want)
		}
	}
}
