/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"encoding/json"
	"errors"
	"go/types"
	"reflect"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// validSettingsKind is a settings kind loadConfig accepts.
const validSettingsKind = `settingsKinds:
  - kind: URLNormalization
    service: url_normalization.URLNormalizationService
    shortName: cfurlnormalization
    noun: URL normalization
    doc: URLNormalization normalizes URLs.
    sample: {scope: both, type: rfc3986}
    rules: [{rule: 'true', message: m, reason: r, invalid: {scope: none}}]
    fields:
      scope: {doc: The scope.}
    unconfigured:
      values: {scope: incoming, 'list[].type': cloudflare}
      reason: Read live.
    ignore:
      type: Not mapped.
`

func TestLoadConfigRefusesSettingsKinds(t *testing.T) {
	if _, err := loadConfig(writeConfig(t, validSettingsKind)); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	replace := func(old, new string) string {
		if !strings.Contains(validSettingsKind, old) {
			t.Fatalf("validSettingsKind has no %q", old)
		}
		return strings.Replace(validSettingsKind, old, new, 1)
	}
	tests := []struct{ name, config, want string }{
		{"a settings kind listed twice", validSettingsKind + strings.TrimPrefix(validSettingsKind, "settingsKinds:\n"),
			"listed twice"},
		{"a settings kind named as a kind", validConfig + strings.NewReplacer("kind: URLNormalization",
			"kind: KVNamespace", "doc: URLNormalization", "doc: KVNamespace").Replace(validSettingsKind),
			"settingsKinds[0]: KVNamespace is listed twice"},
		{"a kind that is not a Go name", replace("kind: URLNormalization", "kind: url-normalization"),
			"not a Go type name"},
		{"a service without a type", replace("service: url_normalization.URLNormalizationService",
			"service: url_normalization"), "not <package>.<type>"},
		{"a short name in capitals", replace("shortName: cfurlnormalization", "shortName: CF"), "lower-case"},
		{"no noun", replace("noun: URL normalization", `noun: ""`), "noun is empty"},
		{"a doc not starting with the kind", replace("doc: URLNormalization normalizes", "doc: It normalizes"),
			"does not start with"},
		{"no sample", replace("sample: {scope: both, type: rfc3986}", "sample: {}"), "sample is empty"},
		{"no unconfigured values", replace("values: {scope: incoming, 'list[].type': cloudflare}", "values: {}"),
			"unconfigured.values is empty"},
		{"unconfigured values without a reason", replace("reason: Read live.", `reason: " "`),
			"unconfigured gives no reason"},
		{"an unconfigured path in capitals", replace("{scope: incoming,", "{Scope: incoming,"),
			`"Scope" is not a path of API names`},
		{"an unconfigured path with an empty step", replace("{scope: incoming,", "{'a..b': incoming,"),
			"is not a path of API names"},
		{"an unconfigured path with a list in its middle", replace("{scope: incoming,", "{'a[]b': incoming,"),
			"is not a path of API names"},
		{"a rule without a message", replace("message: m,", `message: "",`), "needs a rule and a message"},
		{"an ignore without a reason", replace("type: Not mapped.", `type: " "`), "gives no reason"},
		{"a field both ignored and adjusted", replace("scope: {doc: The scope.}", "type: {doc: The type.}"),
			"both ignored and adjusted"},
		{"a key and a list key", replace("scope: {doc: The scope.}", "scope: {key: name, keyDoc: x, listMapKey: id}"),
			"fields.scope: set key for a map or listMapKey for a list, not both"},
		{"an unknown key", validSettingsKind + "    colour: blue\n", "unknown field"},
	}
	for _, tc := range tests {
		if _, err := loadConfig(writeConfig(t, tc.config)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", tc.name, err, tc.want)
		}
	}
}

func TestSettingsReadsTheServices(t *testing.T) {
	for _, tc := range []struct {
		service, read, write, reset, settings string
		resetResults                          int
		readRoute                             string
	}{
		{"managed_transforms.ManagedTransformService", "List", "Edit", "Delete", "ManagedTransformListResponse", 1,
			"get /zones/{zone_id}/managed_headers"},
		{"url_normalization.URLNormalizationService", "Get", "Update", "Delete", "URLNormalizationGetResponse", 1,
			"get /zones/{zone_id}/url_normalization"},
		{"dns.DNSSECService", "Get", "Edit", "Delete", "DNSSEC", 2, "get /zones/{zone_id}/dnssec"},
	} {
		svc := loadTestSettings(t, tc.service)
		got := []string{svc.read.name, svc.write.name, svc.reset.name, svc.settings.Obj().Name()}
		if want := []string{tc.read, tc.write, tc.reset, tc.settings}; !slices.Equal(got, want) {
			t.Errorf("%s: read, write, reset and settings = %v, want %v", tc.service, got, want)
		}
		if n := svc.reset.sig.Results().Len(); n != tc.resetResults {
			t.Errorf("%s: the reset returns %d values, want %d", tc.service, n, tc.resetResults)
		}
		if svc.readRoute.String() != tc.readRoute || svc.writeRoute.path != svc.readRoute.path ||
			svc.resetRoute.verb != "delete" {
			t.Errorf("%s: routes = %s, %s, %s", tc.service, svc.readRoute, svc.writeRoute, svc.resetRoute)
		}
	}
}

func TestSettingsRefuses(t *testing.T) {
	for name, want := range map[string]string{
		"url_normalization":                   "not <package>.<type>",
		"url_normalization.NoSuchService":     "has no type NoSuchService",
		"kv.NamespaceService":                 "has a New",
		"addressing.AddressMapAccountService": "has no Get or List to read the settings with",
		"botnet_feed.ConfigASNService":        "has no Update or Edit to write the settings with",
		"acm.TotalTLSService":                 "has no Delete",
		"workers_for_platforms.DispatchNamespaceScriptTagService": "List returns *pagination.SinglePage[string], " +
			"not settings",
		// It has both a Get and a List, and Get is the one tried first.
		"snippets.RuleService":                 "Get returns *pagination.SinglePage[snippets.RuleGetResponse], not settings",
		"nosuchpackage.URLNormalizationServic": "loading " + sdkModule + "/nosuchpackage",
	} {
		_, err := sdk{dir: "."}.settings(name)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("settings(%q): err = %v, want one containing %q", name, err, want)
		}
	}
}

func TestResetMethodRefusesWhatIsNotAReset(t *testing.T) {
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo,
	}, sdkModule+"/kv")
	if err != nil || len(pkgs) != 1 || len(pkgs[0].Errors) > 0 {
		t.Fatalf("loading kv: %v %v", err, pkgs)
	}
	r := newSDKReader(pkgs[0])
	typ := pkgs[0].Types.Scope().Lookup("NamespaceService").(*types.TypeName)
	methods := types.NewMethodSet(types.NewPointer(typ.Type()))
	if _, err := r.resetMethod(methods, "Rename"); !errors.Is(err, errNoMethod) {
		t.Errorf("resetMethod(Rename): err = %v, want errNoMethod", err)
	}
	if _, err := r.resetMethod(methods, "ListAutoPaging"); err == nil ||
		!strings.Contains(err.Error(), "is not shaped (ctx, params, opts...) error") {
		t.Errorf("resetMethod(ListAutoPaging), which returns one value but no error: err = %v", err)
	}
	if c, err := r.resetMethod(methods, "Delete"); err != nil || c.sig.Results().Len() != 2 {
		t.Errorf("resetMethod(Delete), which returns a result: %v, %v", c, err)
	}
}

// testSettingsEntry returns the settings kind kind of testdata/generator.yaml.
func testSettingsEntry(t *testing.T, kind string) settingsKindConfig {
	t.Helper()
	cfg, err := loadConfig("testdata/generator.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range cfg.SettingsKinds {
		if entry.Kind == kind {
			return entry
		}
	}
	t.Fatalf("testdata/generator.yaml has no settings kind %s", kind)
	return settingsKindConfig{}
}

func TestBuildSettingsKindMapsTheManagedTransforms(t *testing.T) {
	entry := testSettingsEntry(t, "TestManagedTransform")
	k, err := buildSettingsKind(entry, loadTestSettings(t, entry.Service), loadTestSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"managed_request_headers": "id", "managed_response_headers": "id"}; !reflect.DeepEqual(
		k.listKeys, want) || len(k.sets) != 0 {
		t.Errorf("listKeys = %v and sets = %v, want %v and none", k.listKeys, k.sets, want)
	}
	if got := k.unconfigured; len(got) != 2 || got[0].path != "managed_request_headers[].enabled" ||
		got[1].path != "managed_response_headers[].enabled" || got[0].value != false {
		t.Errorf("unconfigured = %v, sorted by path", got)
	}
	state, err := json.Marshal(k.unconfiguredState)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"managed_request_headers":[{"enabled":false,"id":"add_visitor_location_headers"}],` +
		`"managed_response_headers":[{"enabled":false,"id":"add_security_headers"}]}`; string(state) != want {
		t.Errorf("unconfiguredState = %s, want the sample's settings with every transform disabled", state)
	}
	if body := k.bodies[0]; settingsConfigured(k.unconfiguredState, k.unconfigured) || !settingsConfigured(body,
		k.unconfigured) {
		t.Errorf("the unconfigured state counts as configured, or the sample's settings %v do not", body)
	}
	if k.svc.read.name != "List" || k.svc.write.name != "Edit" || k.printColumns[0].name != "Zone ID" {
		t.Errorf("read %s, write %s, columns %v", k.svc.read.name, k.svc.write.name, k.printColumns)
	}
}

func TestBuildSettingsKindChangesEnums(t *testing.T) {
	entry := testSettingsEntry(t, "TestURLNormalization")
	k, err := buildSettingsKind(entry, loadTestSettings(t, entry.Service), loadTestSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	changed := make([]string, 0, len(k.changes))
	for _, c := range k.changes {
		changed = append(changed, c.what+": "+c.spec["scope"].(string)+" "+c.spec["type"].(string))
	}
	if want := []string{"a new scope: incoming rfc3986", "a new type: both cloudflare"}; !slices.Equal(changed, want) {
		t.Errorf("changes = %q, want each enum changed to another of its values", changed)
	}
	if len(k.full) != 1 || k.full[0].spec["scope"] == "both" || k.full[0].spec["type"] == "rfc3986" {
		t.Errorf("full = %v, want the values the sample does not use", k.full)
	}
}

func TestBuildSettingsKindRefuses(t *testing.T) {
	type edit func(e *settingsKindConfig, svc *settingsService, spec *apiSpec)
	urlNormalization, managedTransforms := "TestURLNormalization", "TestManagedTransform"
	tests := []struct {
		name, kind string
		edit       edit
		want       string
	}{
		{"an ignored field the settings lack", urlNormalization, func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			e.Ignore = map[string]string{"nope": "x"}
		}, "nope: the settings have no such field"},
		{"an adjusted field the settings lack", urlNormalization, func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			e.Fields = map[string]ruleFieldConfig{"nope": {Doc: "x"}}
		}, "nope: the settings have no such field"},
		{"every field ignored", urlNormalization, func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			e.Ignore = map[string]string{"scope": "x", "type": "x"}
		}, "the settings: every field is ignored"},
		{"a list key on a scalar", urlNormalization, func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			e.Fields = map[string]ruleFieldConfig{"scope": {ListMapKey: "id"}}
		}, "fields.scope.listMapKey: scope is not a list of objects"},
		{"a list key the items lack", managedTransforms, func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			e.Fields["managed_request_headers"] = ruleFieldConfig{ListMapKey: "nope"}
		}, "the items of managed_request_headers have no field nope"},
		{"a list key that is a bool", managedTransforms, func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			e.Fields["managed_request_headers"] = ruleFieldConfig{ListMapKey: "enabled"}
		}, "enabled is not a required string or integer"},
		{"an unconfigured path the settings lack", urlNormalization, func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			e.Unconfigured.Values["nope"] = "x"
		}, "unconfigured.values.nope: the settings Get returns have no field nope"},
		{"an unconfigured path through a string", urlNormalization, func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			e.Unconfigured.Values["scope.inner"] = "x"
		}, "the settings at scope are not an object"},
		{"an unconfigured list that is a string", urlNormalization, func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			e.Unconfigured.Values["scope[]"] = "x"
		}, "scope is not a list"},
		{"an unconfigured path to an object", managedTransforms, func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			e.Unconfigured.Values["managed_request_headers[]"] = "x"
		}, "only a string, a bool or a number can be compared"},
		{"an unconfigured value of another type", urlNormalization, func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			e.Unconfigured.Values["scope"] = true
		}, "unconfigured.values.scope: true is not a string"},
		{"an unconfigured value outside its enum", urlNormalization, func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			e.Unconfigured.Values["scope"] = "nope"
		}, `"nope" is not one of incoming, both, none`},
		{"an unconfigured bool that is not one", managedTransforms, func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			e.Unconfigured.Values["managed_request_headers[].enabled"] = "no"
		}, "no is not a bool"},
		{"a sample that configures nothing", urlNormalization, func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			e.Sample = map[string]any{"scope": "incoming", "type": "cloudflare"}
		}, "sample: it configures nothing"},
		{"a sample with an unknown field", urlNormalization, func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			e.Sample["colour"] = "blue"
		}, "sample: spec has no field colour"},
		{"a sample that breaks a rule", urlNormalization, func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			e.Rules = []ruleConfig{{Rule: "self.scope != 'both'", Message: "not both", Reason: "r",
				Invalid: map[string]any{"scope": "both", "type": "rfc3986"}}}
		}, `sample: breaks the rule "not both"`},
		{"a sample listing a key twice", managedTransforms, func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			item := map[string]any{"id": "add_visitor_location_headers", "enabled": true}
			e.Sample["managedRequestHeaders"] = []any{item, item}
		}, "spec.managedRequestHeaders lists add_visitor_location_headers twice"},
		{"a test that is not a spec", urlNormalization, func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			e.Tests = []map[string]any{{"scope": float64(1), "type": "rfc3986"}}
		}, "tests[0]: spec.scope is not a string"},
		{"an entry rule whose invalid spec breaks no rule", urlNormalization, func(e *settingsKindConfig,
			_ *settingsService, _ *apiSpec) {
			e.Rules = []ruleConfig{{Rule: "true", Message: "never", Reason: "r",
				Invalid: map[string]any{"scope": "both", "type": "rfc3986"}}}
		}, `rules[0].invalid: breaks no rule with the message "never"`},
		{"a write the subset does not hold", urlNormalization, func(_ *settingsKindConfig, _ *settingsService,
			spec *apiSpec) {
			delete(spec.Paths["/zones/{zone_id}/url_normalization"], "put")
		}, "the OpenAPI subset has no put /zones/{zone_id}/url_normalization"},
		{"a write without a JSON body", urlNormalization, func(_ *settingsKindConfig, _ *settingsService,
			spec *apiSpec) {
			spec.Paths["/zones/{zone_id}/url_normalization"]["put"] = json.RawMessage(`{}`)
		}, "put /zones/{zone_id}/url_normalization takes no JSON body"},
		{"a call that names an object by its ID", urlNormalization, func(_ *settingsKindConfig, svc *settingsService,
			_ *apiSpec) {
			svc.read = loadTestService(t, "kv.NamespaceService").calls["Get"]
		}, "Get is not shaped (ctx, params, opts...)"},
		{"a call without a zone", urlNormalization, func(_ *settingsKindConfig, svc *settingsService, _ *apiSpec) {
			read := *svc.read
			read.paramFields = slices.DeleteFunc(slices.Clone(read.paramFields), isZoneID)
			svc.read = &read
		}, "Get takes no zone_id"},
		{"a zone ID that is not a string", urlNormalization, func(_ *settingsKindConfig, svc *settingsService,
			_ *apiSpec) {
			reset := *svc.reset
			reset.paramFields = slices.Clone(reset.paramFields)
			reset.paramFields[0].typ = types.Typ[types.Int64]
			svc.reset = &reset
		}, "Delete's zone_id is a int64, not a string"},
		{"a read that requires what kflare does not set", urlNormalization, func(_ *settingsKindConfig,
			svc *settingsService, _ *apiSpec) {
			read := *svc.write
			svc.read = &read
		}, "Update requires the body parameter scope, which the shared settings reconciler does not set"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry, spec := testSettingsEntry(t, tc.kind), loadTestSpec(t)
			svc := *loadTestSettings(t, entry.Service)
			tc.edit(&entry, &svc, spec)
			_, err := buildSettingsKind(entry, &svc, spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestGenerateRegistersSettingsKinds(t *testing.T) {
	cfg, err := loadConfig("testdata/generator.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Kinds, cfg.RuleKinds = nil, nil
	out, err := generate(cfg, sdk{dir: "."}, loadTestSpec(t), testHeader)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.blocks[helmRBAC]; !slices.Equal(got, []string{"- testdnssecs", "- testmanagedtransforms",
		"- testurlnormalizations"}) {
		t.Errorf("the Helm role's block = %q", got)
	}
	kinds := string(out.files[kindsFile])
	for _, want := range []string{
		"(&TestManagedTransformReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr)",
		`dependents("TestManagedTransform", func() client.ObjectList {`,
		`dependents("TestURLNormalization",`,
	} {
		if !strings.Contains(kinds, want) {
			t.Errorf("%s does not hold %q:\n%s", kindsFile, want, kinds)
		}
	}
	if first, last := strings.Index(kinds, `dependents("TestManagedTransform"`),
		strings.Index(kinds, `dependents("TestURLNormalization"`); last < first {
		t.Errorf("the Zone's dependents are not in order of their names:\n%s", kinds)
	}
}

func TestSettingsValues(t *testing.T) {
	settings := map[string]any{
		"scope":   "both",
		"items":   []any{map[string]any{"on": true}, map[string]any{"on": false}},
		"count":   int64(2),
		"nothing": nil,
	}
	unconfigured := func(path string, value any) []settingsValue { return []settingsValue{{path: path, value: value}} }
	for _, tc := range []struct {
		path       string
		value      any
		configured bool
	}{
		{"nothing", "x", false},
		{"scope", "both", false},
		{"scope", "incoming", true},
		{"items[].on", false, true},
		{"items[].on", true, true},
		{"count", float64(2), false},
		{"missing", "x", false},
		{"scope.inner", "x", false},
	} {
		if got := settingsConfigured(settings, unconfigured(tc.path, tc.value)); got != tc.configured {
			t.Errorf("settingsConfigured at %s, unconfigured %v = %t, want %t", tc.path, tc.value, got, tc.configured)
		}
	}

	setSettingsValue(settings, []string{"items[]", "on"}, false)
	setSettingsValue(settings, []string{"nested", "deep"}, "x")
	setSettingsValue(settings, []string{"absent[]", "on"}, true)
	want := map[string]any{
		"scope":   "both",
		"items":   []any{map[string]any{"on": false}, map[string]any{"on": false}},
		"count":   int64(2),
		"nothing": nil,
		"nested":  map[string]any{"deep": "x"},
	}
	if !reflect.DeepEqual(settings, want) {
		t.Errorf("after setSettingsValue: %v, want %v", settings, want)
	}
}

func TestJSONValueLiteral(t *testing.T) {
	for value, want := range map[any]string{
		"incoming": `"incoming"`, true: "true", float64(1.5): "float64(1.5)", int64(3): "float64(3)", nil: "<nil>",
	} {
		if got := jsonValueLiteral(value); got != want {
			t.Errorf("jsonValueLiteral(%v) = %s, want %s", value, got, want)
		}
	}
}

func TestSettingsShapeOf(t *testing.T) {
	text := &paramScalar{goType: typeString}
	key := &paramField{apiName: "id", jsonName: "id", required: true, node: text}
	item := &paramStruct{fields: []*paramField{key, {apiName: "tags", jsonName: "tags",
		node: &paramList{items: text, set: true}}}}
	params := &paramStruct{fields: []*paramField{
		{apiName: "tags", jsonName: "tags", node: &paramList{items: text, set: true}},
		{apiName: "names", jsonName: "names", node: &paramList{items: text}},
		{apiName: "items", jsonName: "items", node: &paramList{items: item, key: key}},
		{apiName: "nested", jsonName: "nested", node: &paramStruct{fields: []*paramField{
			{apiName: "codes", jsonName: "codes", node: &paramList{items: text, set: true}}}}},
		{apiName: "by_name", jsonName: "byName", node: &paramMap{value: item}},
	}}
	k := &settingsKind{listKeys: map[string]string{}}
	k.shapeOf(params, "")
	if want := map[string]string{"items": "id"}; !reflect.DeepEqual(k.listKeys, want) {
		t.Errorf("listKeys = %v, want %v", k.listKeys, want)
	}
	if want := []string{"tags", "items[].tags", "nested.codes"}; !slices.Equal(k.sets, want) {
		t.Errorf("sets = %v, want %v: a map's entries are not entered", k.sets, want)
	}
	want := "settingsShape{\nlistKeys: map[string]string{\n\"items\": \"id\",\n},\nsets: map[string]bool{\n" +
		"\"tags\": true,\n\"items[].tags\": true,\n\"nested.codes\": true,\n},\n}"
	if got := settingsShapeLiteral(k); got != want {
		t.Errorf("settingsShapeLiteral = %q, want %q", got, want)
	}
	if got := settingsShapeLiteral(&settingsKind{}); got != "settingsShape{}" {
		t.Errorf("settingsShapeLiteral of a kind without keyed lists or sets = %q", got)
	}
}

func TestParamBuilderRefusesAParamOutsideTheBody(t *testing.T) {
	svc := loadTestSettings(t, "url_normalization.URLNormalizationService")
	pb := &paramBuilder{kind: "TestURLNormalization", r: svc.r, spec: loadTestSpec(t), used: map[string]bool{},
		root: settingsRoot}
	_, err := pb.object("", "TestURLNormalizationSpec", "TestURLNormalization", svc.write.params.(*types.Named), nil)
	if err == nil || !strings.Contains(err.Error(), "zone_id is a path parameter, which kflare-gen cannot map yet") {
		t.Errorf("a zone_id that only a settings kind's root skips: err = %v", err)
	}
}

func TestListMapKeyRefusesAnOptionalField(t *testing.T) {
	cfg, err := loadConfig("testdata/generator.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var entry ruleKindConfig
	for _, e := range cfg.RuleKinds {
		if e.Kind == "TestExecuteRule" {
			entry = e
		}
	}
	for _, field := range []string{"action", "score_threshold"} {
		entry.Fields = map[string]ruleFieldConfig{"overrides.rules": {ListMapKey: field}}
		_, err := buildRuleKind(entry, loadTestRule(t, entry.Rule), loadTestSpec(t))
		if want := field + " is not a required string or integer"; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("listMapKey %s, an optional field: err = %v, want one containing %q", field, err, want)
		}
	}
	entry.Fields = map[string]ruleFieldConfig{"overrides.rules": {ListMapKey: "id"}}
	k, err := buildRuleKind(entry, loadTestRule(t, entry.Rule), loadTestSpec(t))
	if err != nil {
		t.Fatalf("listMapKey id: %v", err)
	}
	var markers []string
	for _, st := range k.structs {
		for _, f := range st.fields {
			if f.apiName == "rules" {
				markers = f.markers()
			}
		}
	}
	if !slices.Contains(markers, "+listType=map") || !slices.Contains(markers, "+listMapKey=id") {
		t.Errorf("the markers of a list keyed by id = %v", markers)
	}
}
