/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"encoding/json"
	"fmt"
	"go/token"
	"go/types"
	"slices"
	"strings"
	"testing"
)

// kvControllerEntry is kvEntry, adopting by title, with a sample.
func kvControllerEntry() kindConfig {
	entry := kvEntry()
	entry.Adopt = adoptConfig{By: "title"}
	entry.Sample = map[string]any{"title": "example"}
	return entry
}

// buildTestController builds the controller of entry from svc and spec.
func buildTestController(t *testing.T, entry kindConfig, svc *service, spec *apiSpec) (*controller, error) {
	t.Helper()
	k, err := buildKind(entry, svc, spec)
	if err != nil {
		t.Fatalf("buildKind: %v", err)
	}
	return buildController(k, entry, svc, spec)
}

// kvController builds the controller of kvControllerEntry.
func kvController(t *testing.T) *controller {
	t.Helper()
	c, err := buildTestController(t, kvControllerEntry(), loadTestService(t, "kv.NamespaceService"), loadTestSpec(t))
	if err != nil {
		t.Fatalf("buildController: %v", err)
	}
	return c
}

// cloneService returns a copy of svc whose calls and fields can be changed.
func cloneService(svc *service) *service {
	out := *svc
	out.calls = map[string]*sdkCall{}
	for name, call := range svc.calls {
		c := *call
		c.paramFields = slices.Clone(call.paramFields)
		out.calls[name] = &c
	}
	out.create, out.update, out.object = slices.Clone(svc.create), slices.Clone(svc.update), slices.Clone(svc.object)
	out.listed = slices.Clone(svc.listed)
	return &out
}

// describeEdits describes edits compactly.
func describeEdits(edits []specEdit) []string {
	out := make([]string, 0, len(edits))
	for _, e := range edits {
		value, _ := json.Marshal(e.value)
		from := ""
		if e.setup != nil {
			setup, _ := json.Marshal(e.setup)
			from = " from " + string(setup)
		}
		out = append(out, e.f.apiName+"="+string(value)+from+" ("+e.what+")")
	}
	return out
}

func TestBuildControllerForTheKVNamespaceService(t *testing.T) {
	c := kvController(t)
	if c.varName != "testKV" || c.pageSize != 1000 || c.page.goName != "Page" || c.perPage.goName != "PerPage" {
		t.Errorf("varName %s, page size %d, page params %s and %s", c.varName, c.pageSize, c.page.goName,
			c.perPage.goName)
	}
	if c.update.name != "Update" || typeName(c.object) != "kv.Namespace" ||
		typeName(c.deleteResult) != "kv.NamespaceDeleteResponse" {
		t.Errorf("update %s, object %s, Delete's result %s", c.update.name, typeName(c.object), typeName(c.deleteResult))
	}
	if c.key.goName != "Title" || c.key.object.goName != "Title" || c.conflictReason != "TitleConflict" ||
		c.id.goName != "ID" {
		t.Errorf("key %s (object %s), conflict reason %s, ID %s", c.key.goName, c.key.object.goName, c.conflictReason,
			c.id.goName)
	}
	mark := func(f *sdkField, call string) string {
		if f == nil {
			return "-"
		}
		return call
	}
	pairs := make([]string, 0, len(c.fields))
	for _, f := range c.fields {
		pairs = append(pairs, fmt.Sprintf("%s %s %s %s", f.apiName, mark(f.create, "New"), mark(f.update, "Update"),
			mark(f.object, "Get")))
	}
	if !slices.Equal(pairs, []string{"title New Update Get", "jurisdiction New - Get", "mode New - Get"}) {
		t.Errorf("fields and their counterparts = %q", pairs)
	}
	status := make([]string, 0, len(c.status))
	for _, s := range c.status {
		status = append(status, s.goName+"="+s.object.goName)
	}
	if !slices.Equal(status, []string{"NamespaceID=ID", "SupportsURLEncoding=SupportsURLEncoding"}) {
		t.Errorf("status = %q", status)
	}
	if got := describeEdits(c.changes); !slices.Equal(got, []string{`title="examplf" (changing title)`}) {
		t.Errorf("changes = %q", got)
	}
	if got := describeEdits(c.immutable); !slices.Equal(got, []string{`jurisdiction="eu" (setting jurisdiction)`,
		`jurisdiction="fedramp" from "eu" (changing jurisdiction once set)`, `mode="instant" (setting mode)`}) {
		t.Errorf("immutable = %q", got)
	}
	if got := describeEdits(c.invalid); len(got) != 3 ||
		got[0] != fmt.Sprintf("title=%q (a title longer than 512 characters)", strings.Repeat("a", 513)) ||
		got[1] != `jurisdiction="kflare-not-in-enum" (a jurisdiction outside its enum)` ||
		got[2] != `mode="kflare-not-in-enum" (a mode outside its enum)` {
		t.Errorf("invalid = %q", got)
	}
}

func TestBuildControllerRefuses(t *testing.T) {
	kv := loadTestService(t, "kv.NamespaceService")
	str := types.Typ[types.String]
	// withResult returns call returning result instead.
	withResult := func(call *sdkCall, result types.Type) *sdkCall {
		out := *call
		results := types.NewTuple(types.NewVar(token.NoPos, nil, "res", result), call.sig.Results().At(1))
		out.sig = types.NewSignatureType(nil, nil, nil, call.sig.Params(), results, true)
		return &out
	}
	without := func(fields []sdkField, apiName string) []sdkField {
		return slices.DeleteFunc(fields, func(f sdkField) bool { return f.apiName == apiName })
	}
	tests := []struct {
		name   string
		change func(svc *service, entry *kindConfig, spec *apiSpec)
		want   string
	}{
		{"a service without List", func(svc *service, _ *kindConfig, _ *apiSpec) { delete(svc.calls, "List") },
			"the service has no List"},
		{"a service without Delete", func(svc *service, _ *kindConfig, _ *apiSpec) { delete(svc.calls, "Delete") },
			"the service has no Delete"},
		{"a kind named by its key without Delete", func(svc *service, entry *kindConfig, _ *apiSpec) {
			entry.ID.Field, entry.Adopt.By = "title", "title"
			delete(svc.calls, "List")
			delete(svc.calls, "Delete")
		}, "the service has no Delete"},
		{"a service without a constructor", func(svc *service, _ *kindConfig, _ *apiSpec) { svc.typeName = "Nothing" },
			"kv has no NewNothing to build the service with"},
		{"a Get without an ID", func(svc *service, _ *kindConfig, _ *apiSpec) {
			get := *svc.calls["List"]
			get.name = "Get"
			svc.calls["Get"] = &get
		}, "Get is not shaped (ctx, id string, params, opts...)"},
		{"a Get that returns no pointer", func(svc *service, _ *kindConfig, _ *apiSpec) {
			svc.calls["Get"] = withResult(svc.calls["Get"], str)
		}, "the service's Get does not return a pointer"},
		{"an Update that returns another type", func(svc *service, _ *kindConfig, _ *apiSpec) {
			svc.calls["Update"] = withResult(svc.calls["Update"], types.NewPointer(str))
		}, "Update returns *string and Get *kv.Namespace"},
		{"a Delete that returns no pointer", func(svc *service, _ *kindConfig, _ *apiSpec) {
			svc.calls["Delete"] = withResult(svc.calls["Delete"], str)
		}, "the service's Delete does not return a pointer"},
		{"a call without an account", func(svc *service, _ *kindConfig, _ *apiSpec) {
			svc.calls["Delete"].paramFields = without(svc.calls["Delete"].paramFields, "account_id")
		}, "Delete takes no account_id"},
		{"a Get that requires more", func(svc *service, _ *kindConfig, _ *apiSpec) {
			svc.calls["Get"].paramFields = append(svc.calls["Get"].paramFields,
				sdkField{apiName: "version", in: inQuery, typ: str, required: true})
		}, "Get requires the query parameter version, which the shared CRUD reconciler does not set"},
		{"a List without per_page", func(svc *service, _ *kindConfig, _ *apiSpec) {
			svc.calls["List"].paramFields = without(svc.calls["List"].paramFields, "per_page")
		}, "List takes no per_page number"},
		{"a List whose page is no number", func(svc *service, _ *kindConfig, _ *apiSpec) {
			fields := svc.calls["List"].paramFields
			fields[slices.IndexFunc(fields, func(f sdkField) bool { return f.apiName == "page" })].typ = str
		}, "List takes no page number"},
		{"a List the spec gives no page size", func(_ *service, _ *kindConfig, spec *apiSpec) {
			spec.Paths["/accounts/{account_id}/storage/kv/namespaces"]["get"] = json.RawMessage(`{}`)
		}, "takes no per_page query parameter"},
		{"an updatable field Get does not report", func(svc *service, _ *kindConfig, _ *apiSpec) {
			svc.object = without(svc.object, "title")
		}, "title can be updated, but Get's object does not report it"},
		{"an Update that requires an ignored field", func(svc *service, entry *kindConfig, _ *apiSpec) {
			svc.update = append(svc.update, sdkField{goName: "Color", apiName: "color", in: inBody, typ: str,
				required: true})
			svc.calls["Update"].paramFields = svc.update
			entry.Ignore = map[string]string{"color": "not tested"}
		}, "Update requires color, which the entry ignores"},
		{"an Update that requires an ignored header", func(svc *service, entry *kindConfig, _ *apiSpec) {
			svc.update = append(svc.update, sdkField{goName: "Version", apiName: "version", in: inHeader, typ: str,
				required: true})
			svc.calls["Update"].paramFields = svc.update
			entry.Ignore = map[string]string{"version": "not tested"}
		}, "Update requires the header parameter version, which the shared CRUD reconciler does not set"},
		{"adoption by an unknown field", func(_ *service, entry *kindConfig, _ *apiSpec) { entry.Adopt.By = "name" },
			"adopt.by: the spec has no field name"},
		{"adoption by an optional field", func(_ *service, entry *kindConfig, _ *apiSpec) {
			entry.Adopt.By = "jurisdiction"
		}, "adopt.by: jurisdiction is not a required string"},
		{"a List of objects kflare-gen cannot read", func(svc *service, _ *kindConfig, _ *apiSpec) { svc.listed = nil },
			"the service's List returns pages of kv.Namespace, which kflare-gen cannot read"},
		{"a List without the ID", func(svc *service, _ *kindConfig, _ *apiSpec) {
			svc.listed = without(slices.Clone(svc.listed), "id")
		}, "the objects List returns have no id, the ID"},
		{"adoption by a field the List does not report", func(svc *service, _ *kindConfig, _ *apiSpec) {
			svc.listed = without(slices.Clone(svc.listed), "title")
		}, "adopt.by: the objects List returns do not report title"},
		{"adoption by a field Get does not report", func(svc *service, _ *kindConfig, _ *apiSpec) {
			svc.object = without(svc.object, "title")
			svc.update, svc.updateMethod = nil, ""
			delete(svc.calls, "Update")
		}, "adopt.by: Get's object does not report title"},
		{"a sample of an unknown field", func(_ *service, entry *kindConfig, _ *apiSpec) {
			entry.Sample = map[string]any{"title": "a", "colour": "blue"}
		}, "sample.colour: the spec has no such field"},
		{"a sample of the wrong type", func(_ *service, entry *kindConfig, _ *apiSpec) {
			entry.Sample = map[string]any{"title": 5.0}
		}, "sample.title: 5 is not a string"},
		{"a sample the CRD refuses", func(_ *service, entry *kindConfig, _ *apiSpec) {
			entry.Sample = map[string]any{"title": "a", "jurisdiction": "mars"}
		}, `sample.jurisdiction: "mars" is not one of eu, fedramp, us`},
		{"a sample without a required field", func(_ *service, entry *kindConfig, _ *apiSpec) {
			entry.Sample = map[string]any{"jurisdiction": "eu"}
		}, "sample.title: the field is required, so the sample sets it"},
		{"a sample whose value cannot change", func(_ *service, entry *kindConfig, spec *apiSpec) {
			entry.Sample = map[string]any{"title": "only"}
			entry.Fields = map[string]fieldConfig{"title": {Validation: &validationConfig{Pattern: "^only$",
				Reason: "a test"}}}
		}, `sample.title: no valid value of title differs from only; the generated specs change it`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, entry, spec := cloneService(kv), kvControllerEntry(), loadTestSpec(t)
			tc.change(svc, &entry, spec)
			_, err := buildTestController(t, entry, svc, spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("buildController: err = %v, want one containing %q", err, tc.want)
			}
		})
	}

	t.Run("a status field Get does not report", func(t *testing.T) {
		entry, spec := kvControllerEntry(), loadTestSpec(t)
		k, err := buildKind(entry, kv, spec)
		if err != nil {
			t.Fatal(err)
		}
		k.metadata = append(k.metadata, field{apiName: "nothing", goName: "Nothing", goType: typeString})
		if _, err := buildController(k, entry, kv, spec); err == nil ||
			!strings.Contains(err.Error(), "the object Get returns has no nothing") {
			t.Errorf("buildController: err = %v", err)
		}
	})
}

func TestBuildControllerRefusesTwoAddresses(t *testing.T) {
	svc := cloneService(loadTestService(t, "r2.BucketService"))
	region := sdkField{goName: "CfR2Region", apiName: "cf-r2-region", in: inHeader, typ: types.Typ[types.String]}
	svc.create = append(svc.create, region)
	svc.calls["New"].paramFields = svc.create
	svc.calls["Get"].paramFields = append(svc.calls["Get"].paramFields, region)
	entry := r2Entry()
	entry.Sample = map[string]any{"name": "a-bucket"}
	_, err := buildTestController(t, entry, svc, loadTestSpec(t))
	want := "cf-r2-jurisdiction and cf-r2-region both name the object in every call; kflare-gen takes one such param yet"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("buildController: err = %v, want one containing %q", err, want)
	}
}

func TestBuildControllerPairsParamsWhereTheyGo(t *testing.T) {
	// A Get that takes a query param of the title's name does not make the
	// title the object's address: the title stays updatable.
	svc := cloneService(loadTestService(t, "kv.NamespaceService"))
	svc.calls["Get"].paramFields = append(svc.calls["Get"].paramFields,
		sdkField{goName: "Version", apiName: "title", in: inQuery, typ: types.Typ[types.String]})
	c, err := buildTestController(t, kvControllerEntry(), svc, loadTestSpec(t))
	if err != nil {
		t.Fatalf("buildController: %v", err)
	}
	if len(c.address) != 0 || len(c.changes) != 1 || c.changes[0].f.apiName != "title" {
		t.Errorf("address = %v, changes = %q; want no address and the title updatable", c.address,
			describeEdits(c.changes))
	}
}

func TestBuildControllerSetsARequiredPage(t *testing.T) {
	svc := cloneService(loadTestService(t, "kv.NamespaceService"))
	for i, f := range svc.calls["List"].paramFields {
		if f.apiName == "page" || f.apiName == "per_page" {
			svc.calls["List"].paramFields[i].required = true
		}
	}
	if _, err := buildTestController(t, kvControllerEntry(), svc, loadTestSpec(t)); err != nil {
		t.Errorf("buildController: %v; the reconciler sets List's page and per_page", err)
	}
}

func TestDefaultValue(t *testing.T) {
	for _, tc := range []struct {
		f    field
		want any
	}{
		{field{goType: typeString, def: `"Standard"`}, "Standard"},
		{field{goType: "*" + typeBool, def: "true"}, true},
		{field{goType: typeBool, def: "false"}, false},
		{field{goType: typeInt64, def: "42"}, int64(42)},
	} {
		got, err := defaultValue(tc.f)
		if err != nil || got != tc.want {
			t.Errorf("defaultValue(%s %s) = %v, %v; want %v", tc.f.goType, tc.f.def, got, err, tc.want)
		}
	}
	if _, err := defaultValue(field{goType: "[]string", def: "[]"}); err == nil {
		t.Error("defaultValue took a list's default")
	}
}

func TestBuildControllerSetsTheAddressInEveryCall(t *testing.T) {
	svc := cloneService(loadTestService(t, "r2.BucketService"))
	for _, name := range []string{"Get", "Edit", "Delete"} {
		fields := svc.calls[name].paramFields
		fields[slices.IndexFunc(fields, func(f sdkField) bool { return f.apiName == "cf-r2-jurisdiction" })].required =
			true
	}
	entry := r2Entry()
	entry.Sample = map[string]any{"name": "a-bucket"}
	c, err := buildTestController(t, entry, svc, loadTestSpec(t))
	if err != nil {
		t.Fatalf("buildController: %v; the reconciler sets the jurisdiction in every call", err)
	}
	if len(c.address) != 1 || c.address[0].jsonName != "jurisdiction" || c.scope != "default" ||
		c.scopeElsewhere != "eu" {
		t.Errorf("address = %v, scope %q, elsewhere %q; want the jurisdiction, default and eu", c.address, c.scope,
			c.scopeElsewhere)
	}
}

func TestCheckShape(t *testing.T) {
	ctx := types.NewVar(token.NoPos, nil, "ctx", types.Typ[types.String])
	opts := types.NewVar(token.NoPos, nil, "opts", types.NewSlice(types.Typ[types.Int]))
	call := func(params ...*types.Var) *sdkCall {
		all := append(append([]*types.Var{ctx}, params...), opts)
		return &sdkCall{name: "Get", sig: types.NewSignatureType(nil, nil, nil, types.NewTuple(all...), nil, true)}
	}
	id := types.NewVar(token.NoPos, nil, "id", types.Typ[types.String])
	number := types.NewVar(token.NoPos, nil, "id", types.Typ[types.Int])
	params := types.NewVar(token.NoPos, nil, "params", types.Typ[types.Bool])
	for _, tc := range []struct {
		call   *sdkCall
		withID bool
		ok     bool
	}{
		{call(params), false, true},
		{call(id, params), true, true},
		{call(params), true, false},
		{call(number, params), true, false},
		{call(id, params), false, false},
	} {
		if err := checkShape(tc.call, tc.withID); (err == nil) != tc.ok {
			t.Errorf("checkShape(%s, %t) = %v, want ok %t", tc.call.sig, tc.withID, err, tc.ok)
		}
	}
}

func TestPageElement(t *testing.T) {
	c := kvController(t)
	page := c.list.sig.Results().At(0).Type()
	if elem, ok := pageElement(page); !ok || !types.Identical(elem, c.object) {
		t.Errorf("pageElement(%s) = %v, %t", typeName(page), elem, ok)
	}
	for _, typ := range []types.Type{c.object, types.NewPointer(c.object), types.NewPointer(types.Typ[types.String])} {
		if _, ok := pageElement(typ); ok {
			t.Errorf("pageElement(%s) = true", typeName(typ))
		}
	}
}

func TestBuildControllerForAListOfAnotherType(t *testing.T) {
	entry := kvControllerEntry()
	entry.Service, entry.ID = "d1.DatabaseService", idConfig{Field: "uuid", Status: "databaseID"}
	entry.Adopt.By, entry.Sample = "name", map[string]any{"name": "example"}
	entry.Ignore = map[string]string{"read_replication": "nested", "created_at": "a time", "file_size": "a number",
		"num_tables": "a number"}
	c, err := buildTestController(t, entry, loadTestService(t, "d1.DatabaseService"), loadTestSpec(t))
	if err != nil {
		t.Fatalf("buildController: %v", err)
	}
	if typeName(c.object) != "d1.D1" || typeName(c.listed) != "d1.DatabaseListResponse" ||
		c.listedID.goName != "UUID" || c.listedKey.goName != "Name" {
		t.Errorf("object %s, listed %s (ID %s, key %s)", typeName(c.object), typeName(c.listed), c.listedID.goName,
			c.listedKey.goName)
	}
	if c.update != nil || slices.Contains(c.calls(), c.svc.calls["Update"]) {
		t.Errorf("an Update that takes only an ignored field is dropped: %v", c.update)
	}
	if c.pageSize != 10000 {
		t.Errorf("page size = %d, want the spec's maximum per_page, 10000", c.pageSize)
	}
	got, err := listedObject(newGoFile(), c)
	if want := "d1.DatabaseListResponse{\nCreatedAt: current.CreatedAt,\n" +
		"Jurisdiction: d1.DatabaseListResponseJurisdiction(current.Jurisdiction),\nName: current.Name,\n" +
		"UUID: current.UUID,\nVersion: current.Version,\n}"; err != nil || got != want {
		t.Errorf("listedObject = %q, %v, want %q", got, err, want)
	}
}

func TestListedObject(t *testing.T) {
	c := kvController(t)
	if got, err := listedObject(newGoFile(), c); err != nil || got != "*current" {
		t.Errorf("a list of Get's type: listedObject = %q, %v", got, err)
	}

	pkg := types.NewPackage(sdkModule+"/kv", "kv")
	c.listed = types.NewNamed(types.NewTypeName(token.NoPos, pkg, "NamespaceListResponse", nil),
		types.NewStruct(nil, nil), nil)
	svc := cloneService(c.svc)
	svc.listed = []sdkField{
		{goName: "ID", apiName: "id", typ: types.Typ[types.String]},
		{goName: "Encoding", apiName: "supports_url_encoding", typ: types.Typ[types.String]},
		{goName: "Elsewhere", apiName: "elsewhere", typ: types.Typ[types.String]},
		{goName: "Title", apiName: "title", typ: types.Typ[types.String]},
	}
	c.svc = svc
	if got, err := listedObject(newGoFile(), c); err != nil ||
		got != "kv.NamespaceListResponse{ID: current.ID, Title: current.Title}" {
		t.Errorf("listedObject = %q, %v: a field the object lacks or the fake cannot copy stays empty", got, err)
	}
	svc.listed[0].typ = types.Typ[types.Bool]
	if _, err := listedObject(newGoFile(), c); err == nil || !strings.Contains(err.Error(), "id: ") {
		t.Errorf("an ID the fake cannot copy: err = %v", err)
	}
}

// testField returns a spec field of goType with validation.
func testField(goType string, v validation, enum ...string) field {
	return field{apiName: "f", goName: "F", jsonName: "f", goType: goType, validation: v, enum: enum,
		optional: strings.HasPrefix(goType, "*")}
}

func TestSampleValue(t *testing.T) {
	two, five := int64(2), int64(5)
	low, high := 1.0, 10.0
	for _, tc := range []struct {
		f    field
		raw  any
		want any
		err  string
	}{
		{f: testField(typeString, validation{}), raw: "a", want: "a"},
		{f: testField(typeString, validation{}), raw: true, err: "true is not a string"},
		{f: testField("*bool", validation{}), raw: true, want: true},
		{f: testField("bool", validation{}), raw: "yes", err: "yes is not a bool"},
		{f: testField("*int64", validation{}), raw: 3.0, want: int64(3)},
		{f: testField(typeInt64, validation{}), raw: 3.5, err: "3.5 is not an integer"},
		{f: testField(typeInt64, validation{}), raw: "3", err: "3 is not an integer"},
		{f: testField("[]string", validation{}), raw: "a", err: "a []string cannot be sampled yet"},
		{f: testField(typeString, validation{}, "x", "y"), raw: "z", err: `"z" is not one of x, y`},
		{f: testField(typeString, validation{minLength: &two}), raw: "a", err: "shorter than 2 characters"},
		{f: testField(typeString, validation{maxLength: &five}), raw: "abcdef", err: "longer than 5 characters"},
		{f: testField(typeString, validation{pattern: "^[a-z]+$"}), raw: "A", err: "does not match ^[a-z]+$"},
		{f: testField(typeString, validation{pattern: "(?!x)"}), raw: "a", err: "does not match"},
		{f: testField(typeInt64, validation{minimum: &low}), raw: 0.0, err: "0 is below the minimum"},
		{f: testField(typeInt64, validation{minimum: &low, exclusiveMinimum: true}), raw: 1.0, err: "below"},
		{f: testField(typeInt64, validation{maximum: &high}), raw: 11.0, err: "11 is above the maximum"},
		{f: testField(typeInt64, validation{maximum: &high, exclusiveMaximum: true}), raw: 10.0, err: "above"},
		{f: testField(typeInt64, validation{minimum: &low, maximum: &high}), raw: 10.0, want: int64(10)},
	} {
		got, err := sampleValue(tc.f, tc.raw)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("sampleValue(%s, %v): err = %v, want one containing %q", tc.f.goType, tc.raw, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("sampleValue(%s, %v) = %v, %v, want %v", tc.f.goType, tc.raw, got, err, tc.want)
		}
	}
}

func TestOtherValue(t *testing.T) {
	one, ten := 1.0, 10.0
	for _, tc := range []struct {
		f     field
		value any
		want  any
		err   string
	}{
		{f: testField(typeString, validation{}, "a", "b"), value: "a", want: "b"},
		{f: testField(typeString, validation{}, "a", "b"), value: nil, want: "a"},
		{f: testField(typeString, validation{}, "a"), value: "a", err: "no valid value of f differs from a"},
		{f: testField("*bool", validation{}), value: true, want: false},
		{f: testField("*bool", validation{}), value: nil, want: true},
		{f: testField(typeInt64, validation{}), value: int64(5), want: int64(6)},
		{f: testField(typeInt64, validation{maximum: &ten}), value: int64(10), want: int64(9)},
		{f: testField("*int64", validation{minimum: &one}), value: nil, want: int64(2)},
		{f: testField(typeString, validation{}), value: "example", want: "examplf"},
		{f: testField(typeString, validation{pattern: "^[a-z]+$"}), value: "abz", want: "aby"},
		{f: testField(typeString, validation{pattern: "^[a-z0-9]+$"}), value: "x9", want: "x8"},
		{f: testField(typeString, validation{pattern: "^[a-z]+$"}), value: "-", want: "a"},
		{f: testField(typeString, validation{pattern: "^[a-z]+-$"}), value: "a-", err: "no valid value"},
		{f: testField(typeString, validation{}), value: nil, err: "no value to change f to"},
	} {
		got, err := otherValue(tc.f, tc.value)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("otherValue(%s, %v): err = %v, want one containing %q", tc.f.goType, tc.value, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("otherValue(%s, %v) = %v, %v, want %v", tc.f.goType, tc.value, got, err, tc.want)
		}
	}
	if got := changedStrings(""); !slices.Equal(got, []any{"a"}) {
		t.Errorf(`changedStrings("") = %v`, got)
	}
}

func TestInvalidValues(t *testing.T) {
	zero, one, three, eight := int64(0), int64(1), int64(3), int64(8)
	low, high := 1.0, 10.0
	for _, tc := range []struct {
		f    field
		want []string
	}{
		{testField(typeString, validation{minLength: &zero}), nil},
		{testField(typeString, validation{minLength: &one}), []string{`f="" (an empty f)`}},
		{testField("*string", validation{minLength: &one}), nil},
		{testField(typeString, validation{minLength: &three, maxLength: &eight}, "aa", "bbbb"), []string{
			`f="kflare-not-in-enum" (a f outside its enum)`, `f="aa" (a f shorter than 3 characters)`,
			`f="aaaaaaaaa" (a f longer than 8 characters)`,
		}},
		{testField(typeInt64, validation{minimum: &low, maximum: &high}), []string{
			"f=0 (a f below its minimum)", "f=11 (a f above its maximum)",
		}},
		{testField(typeInt64, validation{minimum: &low, maximum: &high, exclusiveMinimum: true,
			exclusiveMaximum: true}), []string{"f=1 (a f below its minimum)", "f=10 (a f above its maximum)"}},
		{testField(typeString, validation{pattern: "^[a-z-]+$"}), []string{
			`f="KFLARE-NOT-MATCHING" (a f not matching its pattern)`,
		}},
		{testField(typeString, validation{pattern: "^[a-z-]+$"}, "a", "b"), []string{
			`f="kflare-not-in-enum" (a f outside its enum)`,
		}},
	} {
		got, err := invalidValues(tc.f)
		if err != nil || !slices.Equal(describeEdits(got), tc.want) {
			t.Errorf("invalidValues(%s, %+v) = %q, %v; want %q", tc.f.goType, tc.f.validation, describeEdits(got), err,
				tc.want)
		}
	}
	if _, err := invalidValues(testField(typeString, validation{pattern: ".*"})); err == nil ||
		!strings.Contains(err.Error(), "kflare-gen finds no value its pattern .* refuses") {
		t.Errorf("invalidValues of a pattern that takes everything: err = %v", err)
	}
	for name, want := range map[string]string{"title": "a title", "ID": "an ID", "enabled": "an enabled"} {
		if got := article(name); got != want {
			t.Errorf("article(%s) = %q, want %q", name, got, want)
		}
	}
}

func TestConvert(t *testing.T) {
	pkg := types.NewPackage(sdkModule+"/things", "things")
	named := func(name string, underlying types.Type) types.Type {
		return types.NewNamed(types.NewTypeName(token.NoPos, pkg, name, nil), underlying, nil)
	}
	str, f64, i64, boolean := types.Typ[types.String], types.Typ[types.Float64], types.Typ[types.Int64],
		types.Typ[types.Bool]
	mode := named("Mode", str)
	for _, tc := range []struct {
		from, to types.Type
		want     string
	}{
		{str, str, "v"},
		{str, mode, "things.Mode(v)"},
		{mode, str, "string(v)"},
		{i64, f64, "float64(v)"},
		{f64, i64, "int64(v)"},
		{boolean, named("Flag", boolean), "things.Flag(v)"},
	} {
		f := newGoFile()
		got, err := f.convert("v", tc.from, tc.to)
		if err != nil || got != tc.want {
			t.Errorf("convert(%s → %s) = %q, %v, want %q", typeName(tc.from), typeName(tc.to), got, err, tc.want)
		}
	}
	for _, tc := range []struct{ from, to types.Type }{
		{str, boolean},
		{f64, str},
		{named("Thing", types.NewStruct(nil, nil)), str},
		{types.NewPointer(str), str},
	} {
		if got, err := newGoFile().convert("v", tc.from, tc.to); err == nil {
			t.Errorf("convert(%s → %s) = %q, want an error", typeName(tc.from), typeName(tc.to), got)
		}
	}
	f := newGoFile()
	if _, err := f.convert("v", str, mode); err != nil || f.imports[pkg.Path()] != "things" {
		t.Errorf("a conversion to a named type imports its package: %v %v", err, f.imports)
	}
}

func TestGoValue(t *testing.T) {
	for _, tc := range []struct {
		goType string
		value  any
		want   string
	}{
		{typeString, "a", `"a"`},
		{typeString, strings.Repeat("a", 21), `strings.Repeat("a", 21)`},
		{typeString, strings.Repeat("a", 20), `"` + strings.Repeat("a", 20) + `"`},
		{typeString, "ab" + strings.Repeat("a", 30), `"ab` + strings.Repeat("a", 30) + `"`},
		{"*string", "a", `ptr.To("a")`},
		{"bool", true, "true"},
		{"*bool", false, "ptr.To(false)"},
		{typeInt64, int64(5), "5"},
		{"*int64", int64(-1), "ptr.To[int64](-1)"},
	} {
		f := newGoFile()
		if got := goValue(f, field{goType: tc.goType}, tc.value); got != tc.want {
			t.Errorf("goValue(%s, %v) = %s, want %s", tc.goType, tc.value, got, tc.want)
		}
	}
}

// syntheticController is a controller whose fields show what KV's do not: a
// pointer and an optional string updated in place, numbers the SDK types as
// float64, an enum of the SDK's own type, and a fixed value.
func syntheticController() *controller {
	pkg := types.NewPackage(sdkModule+"/things", "things")
	named := func(name string, underlying types.Type) types.Type {
		return types.NewNamed(types.NewTypeName(token.NoPos, pkg, name, nil), underlying, nil)
	}
	str, f64, boolean := types.Typ[types.String], types.Typ[types.Float64], types.Typ[types.Bool]
	sdk := func(goName string, t types.Type) *sdkField { return &sdkField{goName: goName, typ: t} }
	return &controller{
		kind: &kind{name: "Thing"},
		fields: []apiField{
			{field: field{apiName: "name", goName: "Name", goType: typeString}, create: sdk("Name", str),
				update: sdk("Name", str), object: sdk("Name", str)},
			{field: field{apiName: "note", goName: "Note", goType: typeString, optional: true},
				create: sdk("Note", str), update: sdk("Note", str), object: sdk("Note", str)},
			{field: field{apiName: "size", goName: "Size", goType: "*int64", optional: true},
				create: sdk("Size", f64), update: sdk("Size", f64), object: sdk("Size", f64)},
			{field: field{apiName: "mode", goName: "Mode", goType: typeString, immutable: true},
				create: sdk("Mode", named("ThingNewParamsMode", str)), object: sdk("Mode", named("ThingMode", str))},
			{field: field{apiName: "paused", goName: "Paused", goType: "*bool", optional: true},
				create: sdk("Paused", boolean), update: sdk("Paused", boolean), object: sdk("Paused", boolean)},
		},
		fixedFields: []apiField{{field: field{apiName: "kind"}, fixed: "ip",
			create: sdk("Kind", named("ThingNewParamsKind", str))}},
	}
}

func TestParamStatements(t *testing.T) {
	c := syntheticController()
	f := newGoFile()
	create, err := paramStatements(f, c, func(af apiField) *sdkField { return af.create })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"params.Name.Value, params.Name.Present = obj.Spec.Name, true",
		"if obj.Spec.Note != \"\" {\nparams.Note.Value, params.Note.Present = obj.Spec.Note, true\n}",
		"if obj.Spec.Size != nil {\nparams.Size.Value, params.Size.Present = float64(*obj.Spec.Size), true\n}",
		"params.Mode.Value, params.Mode.Present = things.ThingNewParamsMode(obj.Spec.Mode), true",
		"if obj.Spec.Paused != nil {\nparams.Paused.Value, params.Paused.Present = *obj.Spec.Paused, true\n}",
		`params.Kind.Value, params.Kind.Present = things.ThingNewParamsKind("ip"), true`,
	}
	if !slices.Equal(create, want) {
		t.Errorf("New's params:\n%s\nwant\n%s", strings.Join(create, "\n"), strings.Join(want, "\n"))
	}

	matches, err := matchesExpr(newGoFile(), c, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := "current.Name == obj.Spec.Name &&\n" +
		`(obj.Spec.Note == "" || current.Note == obj.Spec.Note) &&` + "\n" +
		"(obj.Spec.Size == nil || int64(current.Size) == *obj.Spec.Size) &&\n" +
		"(obj.Spec.Paused == nil || current.Paused == *obj.Spec.Paused)"; matches != want {
		t.Errorf("matches:\n%s\nwant\n%s", matches, want)
	}
	created, err := matchesExpr(newGoFile(), c, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(created, "string(current.Mode) == obj.Spec.Mode") {
		t.Errorf("what a new object holds compares create-only fields too:\n%s", created)
	}

	c.fields[0].create.typ = types.Typ[types.Bool]
	if _, err := paramStatements(newGoFile(), c, func(af apiField) *sdkField { return af.create }); err == nil ||
		!strings.Contains(err.Error(), "name: kflare-gen cannot convert a string to a bool") {
		t.Errorf("a param of another kind: err = %v", err)
	}
	c.fields[0].object.typ = types.Typ[types.Bool]
	if _, err := matchesExpr(newGoFile(), c, false); err == nil || !strings.Contains(err.Error(), "comparing name") {
		t.Errorf("an object's field of another kind: err = %v", err)
	}
	c.fixedFields[0].create.typ = types.Typ[types.Int]
	c.fields = nil
	if _, err := paramStatements(newGoFile(), c, func(af apiField) *sdkField { return af.create }); err == nil {
		t.Error("a fixed value of another kind: err = nil")
	}
	if matches, err := matchesExpr(newGoFile(), c, true); err != nil || matches != "" {
		t.Errorf("no field Update takes: matches = %q, %v", matches, err)
	}
}

func TestEmitControllerRefusesWhatItCannotConvert(t *testing.T) {
	kv := loadTestService(t, "kv.NamespaceService")
	tests := []struct {
		name   string
		change func(c *controller)
		want   string
	}{
		{"a status field of another kind", func(c *controller) { c.status[1].object.typ = types.Typ[types.String] },
			"status.cloudflareMetadata.supportsURLEncoding"},
		{"an adoption key of another kind", func(c *controller) { c.listedKey.typ = types.Typ[types.Bool] }, "adopt.by"},
		{"a listed ID of another kind", func(c *controller) { c.listedID.typ = types.Typ[types.Bool] },
			"the listed ID"},
		{"a page that is no number", func(c *controller) { c.page.typ = types.Typ[types.String] }, "List's page"},
		{"an account that is no string", func(c *controller) {
			c.get = &sdkCall{name: "Get", sig: c.get.sig, params: c.get.params,
				paramFields: []sdkField{{goName: "AccountID", apiName: "account_id", in: inPath, typ: types.Typ[types.Bool]}}}
		}, "Get's account_id"},
		{"a New param of another kind", func(c *controller) {
			p := *c.fields[0].create
			p.typ = types.Typ[types.Bool]
			c.fields[0].create = &p
		}, "New: title"},
		{"an Update param of another kind", func(c *controller) {
			p := *c.fields[0].update
			p.typ = types.Typ[types.Bool]
			c.fields[0].update = &p
		}, "Update: title"},
		{"an updatable field of another kind", func(c *controller) {
			o := *c.fields[0].object
			o.typ = types.Typ[types.Bool]
			c.fields[0].object, c.key.object = &o, &sdkField{goName: "Title", typ: types.Typ[types.String]}
		}, "comparing title"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := buildTestController(t, kvControllerEntry(), cloneService(kv), loadTestSpec(t))
			if err != nil {
				t.Fatal(err)
			}
			c.fields, c.status = slices.Clone(c.fields), slices.Clone(c.status)
			tc.change(c)
			if _, err := emitController(c, testHeader); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("emitController: err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestEmitControllerTestRefusesWhatItCannotConvert(t *testing.T) {
	kv := loadTestService(t, "kv.NamespaceService")
	tests := []struct {
		name   string
		change func(c *controller)
		want   string
	}{
		{"a New param the object cannot hold", func(c *controller) {
			o := *c.fields[0].object
			o.typ = types.Typ[types.Bool]
			c.fields[0].object = &o
		}, "the fake's New: title"},
		{"an Update param the object cannot hold", func(c *controller) {
			p := *c.fields[0].update
			p.typ = types.Typ[types.Bool]
			c.fields[0].update = &p
			c.fields[0].create = nil
		}, "the fake's Update: title"},
		{"a page that is no number", func(c *controller) { c.page.typ = types.Typ[types.String] }, "the fake's List"},
		{"a page size that is no number", func(c *controller) { c.perPage.typ = types.Typ[types.String] },
			"the fake's List"},
		{"an adoption key the object cannot hold", func(c *controller) {
			c.key.object = &sdkField{goName: "Title", typ: types.Typ[types.Bool]}
		}, "the fake's objects"},
		{"a create-only field the object cannot hold", func(c *controller) {
			c.fields[1].create, c.fields[1].object = nil, &sdkField{goName: "Jurisdiction", typ: types.Typ[types.Bool]}
		}, "comparing jurisdiction"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := buildTestController(t, kvControllerEntry(), cloneService(kv), loadTestSpec(t))
			if err != nil {
				t.Fatal(err)
			}
			c.fields = slices.Clone(c.fields)
			tc.change(c)
			if _, err := emitControllerTest(c, testHeader); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("emitControllerTest: err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestEmitKindPutsAnBeforeAVowel(t *testing.T) {
	entry := kvControllerEntry()
	entry.Kind, entry.Noun = "IPNamespace", "IP namespace"
	entry.Dependents = []dependentConfig{{Kind: "WorkerScript", Refs: "boundIPNamespaces", Reason: "A reason."}}
	svc, spec := loadTestService(t, "kv.NamespaceService"), loadTestSpec(t)
	k, err := buildKind(entry, svc, spec)
	if err != nil {
		t.Fatalf("buildKind: %v", err)
	}
	c, err := buildController(k, entry, svc, spec)
	if err != nil {
		t.Fatalf("buildController: %v", err)
	}
	types, err := emitTypes(k, "")
	if err != nil {
		t.Fatalf("emitTypes: %v", err)
	}
	controller, err := emitController(c, "")
	if err != nil {
		t.Fatalf("emitController: %v", err)
	}
	// The docs are wrapped, so compare them as one line of words.
	text := strings.Join(strings.Fields(strings.ReplaceAll(string(types)+string(controller), "//", "")), " ")
	for _, want := range []string{
		"the Cloudflare IP namespace an IPNamespace manages",
		"never adopts an IP namespace that another IPNamespace already carries",
		"IPNamespaceReconciler reconciles an IPNamespace object",
		"NewIPNamespaceAPI constructs an IPNamespaceAPI from a raw API token",
		"Dependents use an IPNamespace, which waits",
		"so an IPNamespace waiting for its account",
		"so an IPNamespace waiting for them is released",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the controller does not say %q", want)
		}
	}
}

func TestGoFileSource(t *testing.T) {
	f := newGoFile()
	f.use("example.com/a/things", "things")
	f.use("example.com/b/things", "things")
	if _, err := f.source(testHeader); err == nil || !strings.Contains(err.Error(), "both named things") {
		t.Errorf("two packages of one name: err = %v", err)
	}
	f = newGoFile()
	f.printf("func {")
	if _, err := f.source(testHeader); err == nil || !strings.Contains(err.Error(), "formatting") {
		t.Errorf("source that does not parse: err = %v", err)
	}
}

func TestGenerateRegistersEveryKindInOrder(t *testing.T) {
	second := strings.ReplaceAll(strings.TrimPrefix(runConfig, "kinds:\n"), "TestKVNamespace", "TestAKVNamespace")
	cfg, err := loadConfig(writeConfig(t, runConfig+second))
	if err != nil {
		t.Fatal(err)
	}
	out, err := generate(cfg, sdk{dir: "."}, loadTestSpec(t), testHeader)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.blocks[helmRBAC]; !slices.Equal(got, []string{"- testakvnamespaces", "- testkvnamespaces"}) {
		t.Errorf("the Helm role's block = %q", got)
	}
	if got := out.blocks[crdKustomization]; !slices.Equal(got, []string{
		"- bases/kflare.dev_testakvnamespaces.yaml", "- bases/kflare.dev_testkvnamespaces.yaml",
	}) {
		t.Errorf("the CRD kustomization's block = %q", got)
	}
	kinds := string(out.files[kindsFile])
	first, last := strings.Index(kinds, "TestAKVNamespaceReconciler{"), strings.Index(kinds, "TestKVNamespaceReconciler{")
	if first < 0 || last < first {
		t.Errorf("SetupGeneratedReconcilers sets the kinds up in order of their names:\n%s", kinds)
	}
}

func TestReplaceBlock(t *testing.T) {
	got, err := replaceBlock([]byte("a\n\t# "+blockBegin+" why\n\told\n\t# "+blockEnd+"\nb"), []string{"x", "y"})
	if err != nil || string(got) != "a\n\t# "+blockBegin+" why\n\tx\n\ty\n\t# "+blockEnd+"\nb" {
		t.Errorf("replaceBlock = %q, %v", got, err)
	}
	for name, src := range map[string]string{
		"no block":         "a\n",
		"no end":           blockBegin + "\n",
		"an end first":     blockEnd + "\n" + blockBegin + "\n",
		"two begin lines":  blockBegin + "\n" + blockBegin + "\n" + blockEnd + "\n",
		"two end lines":    blockBegin + "\n" + blockEnd + "\n" + blockEnd + "\n",
		"an end and begin": blockEnd + "\n",
	} {
		if _, err := replaceBlock([]byte(src), nil); err == nil {
			t.Errorf("replaceBlock of %s: err = nil", name)
		}
	}
}

func TestPluralOfAndProse(t *testing.T) {
	for in, want := range map[string]string{
		"KVNamespace": "KVNamespaces", "AccessPolicy": "AccessPolicies", "KV namespace": "KV namespaces",
		"Gateway": "Gateways", "Address": "Addresses",
	} {
		if got := pluralOf(in); got != want {
			t.Errorf("pluralOf(%q) = %q, want %q", in, got, want)
		}
	}
	for want, names := range map[string][]string{
		"": nil, "a": {"a"}, "a and b": {"a", "b"}, "a, b and c": {"a", "b", "c"},
	} {
		if got := prose(names); got != want {
			t.Errorf("prose(%q) = %q, want %q", names, got, want)
		}
	}
}
