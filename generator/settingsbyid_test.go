/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"go/types"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// zoneSettings is the test settings kind whose settings go by their IDs.
const zoneSettings = "TestZoneSettings"

func TestSettingsReadsAServiceByID(t *testing.T) {
	svc := loadTestSettings(t, "zones.SettingService")
	if !svc.byID || svc.read.name != "Get" || svc.write.name != "Edit" || svc.reset != nil {
		t.Fatalf("byID %t, read %v, write %v, reset %v", svc.byID, svc.read, svc.write, svc.reset)
	}
	if svc.readRoute.String() != "get /zones/{zone_id}/settings/{setting_id}" ||
		svc.writeRoute.String() != "patch /zones/{zone_id}/settings/{setting_id}" {
		t.Errorf("routes = %s, %s", svc.readRoute, svc.writeRoute)
	}
	if got := summary(svc.write.paramFields); !slices.Equal(got, []string{"path zone_id string required"}) {
		t.Errorf("Edit's params but the body = %v", got)
	}
	b := svc.writeByIDBody
	if b.field.goName != "Body" || b.body.Obj().Name() != "SettingEditParamsBodyValue" || b.value.goName != "Value" ||
		typeName(b.value.typ) != "zones.SettingEditParamsBodyValueValueUnion" {
		t.Errorf("the write's body = %s %s, %s, %s %s", b.field.goName, typeName(b.field.typ), b.body.Obj().Name(),
			b.value.goName, typeName(b.value.typ))
	}
	variants := make([]string, 0, len(b.variants))
	for _, v := range b.variants {
		variants = append(variants, types.TypeString(v, (*types.Package).Name))
	}
	for _, want := range []string{"shared.UnionString", "shared.UnionFloat",
		"zones.SettingEditParamsBodyValueValueZonesSecurityHeaderValue"} {
		if !slices.Contains(variants, want) {
			t.Errorf("the union of values lacks %s: %v", want, variants)
		}
	}
}

func TestSettingsReadsTheSettingsAServiceByIDKnows(t *testing.T) {
	svc := loadTestSettings(t, "zones.SettingService")
	byID := map[string]*sdkSetting{}
	for i, s := range svc.settingsByID {
		if i > 0 && svc.settingsByID[i-1].id >= s.id {
			t.Errorf("the settings are not in the order of their IDs: %s before %s", svc.settingsByID[i-1].id, s.id)
		}
		byID[s.id] = s
	}
	for _, tc := range []struct{ id, goName, value, variant string }{
		{"ssl", "SSL", "zones.SettingGetResponseZonesSSL2Value", "shared.UnionString"},
		{"always_use_https", "AlwaysUseHTTPS", "zones.SettingGetResponseZonesAlwaysUseHTTPS2Value",
			"shared.UnionString"},
		{"tls_1_3", "TLS13", "zones.TLS1_3Value", "shared.UnionString"},
		{"browser_cache_ttl", "BrowserCacheTTL", "int64", "shared.UnionFloat"},
		{"security_header", "SecurityHeader", "zones.SecurityHeadersValue",
			"zones.SettingEditParamsBodyValueValueZonesSecurityHeaderValue"},
		{"0rtt", "0rtt", "zones.ZeroRTTValue", "shared.UnionString"},
	} {
		s, ok := byID[tc.id]
		if !ok {
			t.Errorf("no setting %s", tc.id)
			continue
		}
		got := types.TypeString(s.value, (*types.Package).Name)
		if s.goName != tc.goName || got != tc.value {
			t.Errorf("%s: Go name %s, value %s; want %s, %s", tc.id, s.goName, got, tc.goName, tc.value)
		}
		variant, err := svc.valueVariant(s.value)
		if err != nil || types.TypeString(variant, (*types.Package).Name) != tc.variant {
			t.Errorf("%s: the value's variant = %v, %v; want %s", tc.id, variant, err, tc.variant)
		}
	}
	if s := byID["ssl_recommender"]; s == nil || s.value != nil {
		t.Errorf("ssl_recommender holds no value, yet reads as %v", s)
	}
	if !strings.Contains(byID["always_use_https"].doc, "301 redirect") {
		t.Errorf("always_use_https's doc = %q", byID["always_use_https"].doc)
	}
}

func TestValueVariantRefuses(t *testing.T) {
	svc := loadTestSettings(t, "zones.SettingService")
	ciphers := svc.settingsByID[slices.IndexFunc(svc.settingsByID, func(s *sdkSetting) bool { return s.id == "ciphers" })]
	if _, err := svc.valueVariant(ciphers.value); err == nil ||
		!strings.Contains(err.Error(), "its value is a []string, which kflare-gen cannot write by the setting's ID") {
		t.Errorf("ciphers' variant: err = %v", err)
	}
	if _, err := svc.valueVariant(types.Typ[types.Bool]); err == nil ||
		!strings.Contains(err.Error(), "its value is a bool, which kflare-gen cannot write by the setting's ID yet") {
		t.Errorf("a bool's variant: err = %v", err)
	}
	zone := svc.r.pkg.Types.Scope().Lookup("Zone").Type()
	if _, err := svc.valueVariant(zone); err == nil ||
		!strings.Contains(err.Error(), "no type of zones.SettingEditParamsBodyValueValueUnion takes one") {
		t.Errorf("an object no variant holds: err = %v", err)
	}
	ambiguous := *svc
	ambiguous.writeByIDBody.variants = append(slices.Clone(svc.writeByIDBody.variants),
		svc.writeByIDBody.variants...)
	if _, err := ambiguous.valueVariant(types.Typ[types.String]); err == nil ||
		!strings.Contains(err.Error(), "which 2 types of") {
		t.Errorf("a string two variants take: err = %v", err)
	}
}

func TestSettingsRefusesAServiceByIDItCannotDrive(t *testing.T) {
	for name, want := range map[string]string{
		"api_gateway.LabelManagedService":             "it has no Update or Edit to write the settings with",
		"origin_tls_client_auth.HostnameService":      "its Get takes a setting's ID, and its Update does not",
		"r2.BucketCORSService":                        "it has a Delete, which kflare-gen cannot call yet",
		"addressing.PrefixAdvertisementStatusService": "Edit's params hold no body that is a union of settings' values",
		// Its Get takes a phase, not a string: its settings go all at once.
		"rulesets.PhaseService": "has no Delete, which the shared settings reconciler resets the settings with",
		// Its Get takes a package's ID and a group's, not one setting's: it reads settings that go all at once.
		"firewall.WAFPackageGroupService": "Get returns *interface{}, not settings",
	} {
		if _, err := (sdk{dir: "."}).settings(name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("settings(%q): err = %v, want one containing %q", name, err, want)
		}
	}
	// Its Get takes a hostname after a setting's ID: one for each hostname, not a zone's settings by their IDs.
	if svc, err := (sdk{dir: "."}).settings("hostnames.SettingTLSService"); err != nil || svc.byID {
		t.Errorf("hostnames.SettingTLSService: %v, by ID %t", err, svc != nil && svc.byID)
	}

	svc := *loadTestSettings(t, "zones.SettingService")
	readsNoUnion := svc
	readsNoUnion.read = loadTestService(t, "kv.NamespaceService").calls["Get"]
	if err := readsNoUnion.readSettingsByID(); err == nil ||
		!strings.Contains(err.Error(), "Get returns *kv.Namespace, which holds no union of settings (AsUnion)") {
		t.Errorf("a Get without a union of settings: err = %v", err)
	}
	bodyOfNoValue := svc
	bodyOfNoValue.writeByIDBody.field.typ = svc.r.pkg.Types.Scope().Lookup("SettingGetResponseUnion").Type()
	if err := bodyOfNoValue.readValueBody(); err == nil ||
		!strings.Contains(err.Error(), "Edit's body has no variant that holds only a value") {
		t.Errorf("a body without a value: err = %v", err)
	}
}

func TestBuildSettingsKindMapsSettingsByID(t *testing.T) {
	entry := testSettingsEntry(t, zoneSettings)
	k, err := buildSettingsKind(entry, loadTestSettings(t, entry.Service), loadTestSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	if !k.byID || k.plural != "testzonesettings" {
		t.Errorf("byID %t, plural %s", k.byID, k.plural)
	}
	fields := make([]string, 0, len(k.params.fields))
	for _, f := range k.params.fields {
		fields = append(fields, f.apiName+" "+f.jsonName+" "+f.goType()+" "+typeName(f.sdkType))
	}
	if want := []string{
		"always_use_https alwaysUseHTTPS string shared.UnionString",
		"browser_cache_ttl browserCacheTTL *int64 shared.UnionFloat",
		"security_header securityHeader *TestZoneSettingsSecurityHeader " +
			"zones.SettingEditParamsBodyValueValueZonesSecurityHeaderValue",
		"ssl ssl string shared.UnionString",
	}; !slices.Equal(fields, want) {
		t.Errorf("fields = %q, want %q", fields, want)
	}
	maxAge := k.structs[1].fields[2]
	if maxAge.apiName != "max_age" || maxAge.goType() != "*int64" {
		t.Errorf("max_age = %s %s, want an integer", maxAge.apiName, maxAge.goType())
	}
	if pick := k.picks["ssl"]; len(k.picks) != 1 || pick == nil || pick.Setting != "ssl_automatic_mode" {
		t.Errorf("picks = %v", k.picks)
	}
	unconfigured := make([]string, 0, len(k.unconfigured))
	for _, v := range k.unconfigured {
		unconfigured = append(unconfigured, v.path)
	}
	if !slices.Contains(unconfigured, "ssl_automatic_mode") || slices.Contains(unconfigured, "ssl") ||
		len(unconfigured) != 8 {
		t.Errorf("unconfigured = %v", unconfigured)
	}
	if got := k.unconfiguredState["ssl"]; got == "strict" || got == nil {
		t.Errorf("the unconfigured zone's ssl = %v, the sample's", got)
	}
	if len(k.configuredSpecs) != len(k.configuredStates) || slices.ContainsFunc(k.configuredSpecs,
		func(i int) bool { return i != 0 }) {
		t.Errorf("configured specs = %v: the sample sets every setting", k.configuredSpecs)
	}
	want := map[string]any{"strict_transport_security": map[string]any{"enabled": false, "include_subdomains": false,
		"max_age": float64(0), "nosniff": false, "preload": false}}
	if got := k.resetValue(k.field("security_header")); !reflect.DeepEqual(got, want) {
		t.Errorf("security_header's reset = %v, want %v", got, want)
	}
	if got := k.resetValue(k.field("always_use_https")); got != "off" {
		t.Errorf("always_use_https's reset = %v", got)
	}
}

func TestBuildSettingsKindReadsAConfiguredValueWithTheSpecThatSetsIt(t *testing.T) {
	entry := testSettingsEntry(t, zoneSettings)
	delete(entry.Sample, "browserCacheTTL")
	entry.Tests = append(entry.Tests, map[string]any{"browserCacheTTL": float64(60)})
	k, err := buildSettingsKind(entry, loadTestSettings(t, entry.Service), loadTestSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(k.unconfigured, func(v settingsValue) bool { return v.path == "browser_cache_ttl" })
	if k.configuredSpecs[i] != 2 {
		t.Errorf("browser_cache_ttl's configured case reads it with spec %d, want 2, the test that sets it",
			k.configuredSpecs[i])
	}
}

func TestBuildSettingsKindRefusesSettingsByID(t *testing.T) {
	type edit func(e *settingsKindConfig, svc *settingsService, spec *apiSpec)
	tests := []struct {
		name string
		edit edit
		want string
	}{
		{"a setting with no Go name", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			delete(e.Ignore, "0rtt")
		}, `0rtt's name in cloudflare-go v7, "0rtt", is no exported Go name`},
		{"a setting that holds no value", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			delete(e.Ignore, "ssl_recommender")
		}, "ssl_recommender holds no value"},
		{"a setting whose value is a list", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			delete(e.Ignore, "ciphers")
		}, "ciphers: its value is a []string, which kflare-gen cannot write by the setting's ID yet"},
		{"a setting whose value the write takes no schema of", func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			delete(e.Ignore, "automatic_platform_optimization")
		}, "takes no value of the setting (zones_automatic_platform_optimization_value"},
		{"a write without a value", func(_ *settingsKindConfig, _ *settingsService, spec *apiSpec) {
			body := spec.Components.Schemas["zones_zone_settings_single_request"]
			body.OneOf = body.OneOf[:1]
		}, "takes no value"},
		{"every setting ignored", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			for _, id := range []string{"always_use_https", "browser_cache_ttl", "security_header", "ssl"} {
				e.Ignore[id] = "x"
				delete(e.Fields, id)
			}
		}, "every setting is ignored"},
		{"a field of a setting picked", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			f := e.Fields["security_header.strict_transport_security.max_age"]
			f.PickedBy = e.Fields["ssl"].PickedBy
			e.Fields["security_header.strict_transport_security.max_age"] = f
			delete(e.Fields, "ssl")
		}, "a setting is picked, not a field of one"},
		{"an object picked", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			e.Fields["security_header"] = ruleFieldConfig{PickedBy: e.Fields["ssl"].PickedBy}
		}, "security_header is an object; only a string, a bool or an integer is picked"},
		{"a setting picked by one of the kind's", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			pick := *e.Fields["ssl"].PickedBy
			pick.Setting = "always_use_https"
			e.Fields["ssl"] = ruleFieldConfig{PickedBy: &pick}
		}, "always_use_https is a setting of the kind, which the reset of ssl would write too"},
		{"two settings picked by one", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			e.Fields["always_use_https"] = ruleFieldConfig{PickedBy: e.Fields["ssl"].PickedBy}
		}, "fields.ssl.pickedBy: always_use_https is picked by ssl_automatic_mode too"},
		{"a field without an unconfigured value", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			delete(e.Unconfigured.Values, "security_header.strict_transport_security.preload")
		}, "unconfigured.values: give security_header.strict_transport_security.preload, which a reset writes"},
		{"an unconfigured value of a setting Cloudflare picks", func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			e.Unconfigured.Values["ssl"] = "full"
		}, `Cloudflare picks ssl while ssl_automatic_mode is "auto"`},
		{"an unconfigured value of the setting one is picked by", func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			e.Unconfigured.Values["ssl_automatic_mode"] = "auto"
		}, "unconfigured.values.ssl_automatic_mode: fields.ssl.pickedBy gives its value"},
		{"an unconfigured value the settings lack", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			e.Unconfigured.Values["nope"] = "x"
		}, "unconfigured.values.nope: the settings have no such field"},
		{"an unconfigured value of an object", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			e.Unconfigured.Values["security_header"] = "x"
		}, "unconfigured.values.security_header: the settings have no such field, or it holds more than a value"},
		{"an unconfigured value outside the enum", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			e.Unconfigured.Values["always_use_https"] = "maybe"
		}, `unconfigured.values.always_use_https: the value is "maybe", not one of on, off`},
		{"an unconfigured value out of range", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			e.Unconfigured.Values["browser_cache_ttl"] = float64(-1)
		}, "unconfigured.values.browser_cache_ttl: the value is below its minimum"},
		{"a setting no spec sets", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			delete(e.Sample, "browserCacheTTL")
		}, "unconfigured.values.browser_cache_ttl: no sample or test sets browser_cache_ttl"},
		{"a picked setting no spec sets", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			delete(e.Sample, "ssl")
			e.Tests = nil
		}, "unconfigured.values.ssl_automatic_mode: no sample or test sets ssl"},
		{"a number not made an integer", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			delete(e.Fields, "security_header.strict_transport_security.max_age")
		}, "make it an integer (validation.integer) with one"},
		{"an integer made one", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			e.Fields["browser_cache_ttl"] = ruleFieldConfig{Validation: &validationConfig{Integer: true, Reason: "r"}}
		}, "fields.browser_cache_ttl.validation.integer: only a number the OpenAPI spec does not say is an integer"},
		{"a string made an integer", func(e *settingsKindConfig, _ *settingsService, _ *apiSpec) {
			e.Fields["always_use_https"] = ruleFieldConfig{Validation: &validationConfig{Integer: true, Reason: "r"}}
		}, "fields.always_use_https.validation.integer: only a number"},
		{"a setting Cloudflare picks that breaks a rule alone", func(e *settingsKindConfig, _ *settingsService,
			_ *apiSpec) {
			e.Rules = []ruleConfig{{Rule: "!has(self.ssl) || has(self.alwaysUseHTTPS)", Message: "ssl alone",
				Reason: "r", Invalid: map[string]any{"ssl": "strict"}}}
			e.Tests = nil
		}, `fields.ssl.pickedBy: a spec of ssl alone: breaks the rule "ssl alone"`},
		{"a write that names no setting by its ID", func(_ *settingsKindConfig, svc *settingsService, _ *apiSpec) {
			svc.write = loadTestSettings(t, "url_normalization.URLNormalizationService").write
		}, "Update is not shaped (ctx, id string, params, opts...)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry, spec := testSettingsEntry(t, zoneSettings), loadTestSpec(t)
			svc := *loadTestSettings(t, entry.Service)
			tc.edit(&entry, &svc, spec)
			_, err := buildSettingsKind(entry, &svc, spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// zoneSettingsBuilder returns the builders of the test kind whose settings go
// by their IDs, as buildSettingsKind starts them.
func zoneSettingsBuilder(t *testing.T) (*settingsKindBuilder, *paramBuilder, *settingsService) {
	t.Helper()
	entry, spec := testSettingsEntry(t, zoneSettings), loadTestSpec(t)
	svc := loadTestSettings(t, entry.Service)
	return &settingsKindBuilder{entry: entry, svc: svc, spec: spec}, &paramBuilder{kind: entry.Kind, r: svc.r,
		spec: spec, adjust: entry.Fields, ignore: entry.Ignore, used: map[string]bool{}, zoneParams: true,
		root: settingsRoot}, svc
}

func TestSettingFieldRefuses(t *testing.T) {
	b, pb, svc := zoneSettingsBuilder(t)
	apo := svc.settingsByID[slices.IndexFunc(svc.settingsByID, func(s *sdkSetting) bool {
		return s.id == "automatic_platform_optimization"
	})]
	sch := &schema{Ref: "#/components/schemas/zones_automatic_platform_optimization"}
	if _, err := b.settingField(pb, apo, sch); err == nil || !strings.Contains(err.Error(),
		"automatic_platform_optimization.hostnames is a list, which kflare-gen cannot reset by the setting's ID") {
		t.Errorf("an object holding a list: err = %v", err)
	}
	lower := &sdkSetting{id: "lower", goName: "lower", value: types.Typ[types.String]}
	if _, err := b.settingField(pb, lower, sch); err == nil ||
		!strings.Contains(err.Error(), `lower's name in cloudflare-go v7, "lower", is no exported Go name`) {
		t.Errorf("a name in lower case: err = %v", err)
	}
}

func TestBuildSettingsKindPicksOnlySettingsByID(t *testing.T) {
	entry := testSettingsEntry(t, "TestURLNormalization")
	entry.Fields = map[string]ruleFieldConfig{"scope": {PickedBy: &pickedByConfig{Setting: "x", While: "a",
		Written: "b", Reason: "r"}}}
	_, err := buildSettingsKind(entry, loadTestSettings(t, entry.Service), loadTestSpec(t))
	if err == nil || !strings.Contains(err.Error(),
		"fields.scope.pickedBy: only a setting of a service whose settings go by their IDs is picked") {
		t.Errorf("err = %v", err)
	}
}

func TestLoadConfigRefusesPickedByAndInteger(t *testing.T) {
	replace := func(old, new string) string {
		if !strings.Contains(validSettingsKind, old) {
			t.Fatalf("validSettingsKind has no %q", old)
		}
		return strings.Replace(validSettingsKind, old, new, 1)
	}
	pickedBy := func(fields string) string {
		return replace("scope: {doc: The scope.}", "scope: {doc: The scope., pickedBy: {"+fields+"}}")
	}
	if _, err := loadConfig(writeConfig(t, pickedBy("setting: ssl_automatic_mode, while: auto, written: custom, "+
		"reason: r"))); err != nil {
		t.Fatalf("a pickedBy: %v", err)
	}
	for _, tc := range []struct{ name, config, want string }{
		{"a setting that is not an ID", pickedBy("setting: Mode, while: auto, written: custom, reason: r"),
			`fields.scope.pickedBy: setting "Mode" is not a setting's ID`},
		{"no while", pickedBy("setting: mode, written: custom, reason: r"), "give while and written"},
		{"no written", pickedBy("setting: mode, while: auto, reason: r"), "give while and written"},
		{"while and written alike", pickedBy("setting: mode, while: auto, written: auto, reason: r"),
			`while and written are both "auto"`},
		{"no reason", pickedBy("setting: mode, while: auto, written: custom"), "pickedBy: gives no reason"},
		{"a column", replace("scope: {doc: The scope.}", "scope: {doc: The scope., printColumn: Scope}"),
			"fields.scope.printColumn: only a rule kind's fields have columns"},
		{"an integer with a pattern", replace("scope: {doc: The scope.}",
			"scope: {validation: {integer: true, pattern: '^a$', reason: r}}"),
			"fields.scope.validation: integer takes no lengths or pattern"},
	} {
		if _, err := loadConfig(writeConfig(t, tc.config)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", tc.name, err, tc.want)
		}
	}
}

func TestLoadConfigRefusesARuleKindPicked(t *testing.T) {
	config := strings.Replace(validRuleKind, "fields:\n", "fields:\n      picked: {pickedBy: {setting: s, while: a, "+
		"written: b, reason: r}}\n", 1)
	if config == validRuleKind {
		t.Fatal("validRuleKind has no fields")
	}
	if _, err := loadConfig(writeConfig(t, config)); err == nil ||
		!strings.Contains(err.Error(), "fields.picked.pickedBy: only a setting a settings kind writes by its ID") {
		t.Errorf("err = %v", err)
	}
}

func TestOtherScalarValue(t *testing.T) {
	minimum, maximum := float64(0), float64(10)
	for _, tc := range []struct {
		name        string
		n           *paramScalar
		value, want any
		err         string
	}{
		{"another enum value", &paramScalar{goType: typeString, enum: []string{"on", "off"}}, "on", "off", ""},
		{"the first other enum value", &paramScalar{goType: typeString, enum: []string{"a", "b", "c"}}, "b", "a", ""},
		{"another string", &paramScalar{goType: typeString}, "x", "x-other", ""},
		{"the other bool", &paramScalar{goType: typeBool}, false, true, ""},
		{"the next number", &paramScalar{goType: typeInt64}, float64(4), float64(5), ""},
		{"the number before, at the maximum", &paramScalar{goType: typeInt64,
			validation: validation{minimum: &minimum, maximum: &maximum}}, float64(10), float64(9), ""},
		{"another number of an enum", &paramScalar{goType: typeInt64, enum: []string{"30", "60"}}, float64(60),
			float64(30), ""},
		{"a value outside the enum", &paramScalar{goType: typeString, enum: []string{"on"}}, "off", nil,
			`the value is "off", not one of on`},
		{"an enum of one value", &paramScalar{goType: typeString, enum: []string{"on"}}, "on", nil,
			"on is the only value kflare-gen finds the field takes"},
		{"a number for a string", &paramScalar{goType: typeString}, float64(1), nil, "the value is not a string"},
	} {
		got, err := otherScalarValue(tc.n, tc.value)
		switch {
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s: err = %v, want one containing %q", tc.name, err, tc.err)
		case tc.err == "" && (err != nil || got != tc.want):
			t.Errorf("%s: otherScalarValue = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}

func TestPickedValue(t *testing.T) {
	minimum := float64(3)
	for _, tc := range []struct {
		name           string
		n              *paramScalar
		sample, want   any
		wantsAnErrorOf string
	}{
		{"another value than the sample's", &paramScalar{goType: typeString, enum: []string{"off", "strict"}},
			"off", "strict", ""},
		{"the first of an enum", &paramScalar{goType: typeString, enum: []string{"off", "strict"}}, nil, "off", ""},
		{"the first of an enum of integers", &paramScalar{goType: typeInt64, enum: []string{"30", "60"}}, nil,
			float64(30), ""},
		{"false", &paramScalar{goType: typeBool}, nil, false, ""},
		{"the minimum", &paramScalar{goType: typeInt64, validation: validation{minimum: &minimum}}, nil,
			float64(3), ""},
		{"zero", &paramScalar{goType: typeInt64}, nil, float64(0), ""},
		{"a string", &paramScalar{goType: typeString}, nil, "unconfigured", ""},
		{"a string kflare-gen finds no value of", &paramScalar{goType: typeString,
			validation: validation{pattern: "^[0-9]+$"}}, nil, nil, "kflare-gen finds no value it takes"},
	} {
		got, err := pickedValue(tc.n, tc.sample)
		switch {
		case tc.wantsAnErrorOf != "" && (err == nil || !strings.Contains(err.Error(), tc.wantsAnErrorOf)):
			t.Errorf("%s: err = %v", tc.name, err)
		case tc.wantsAnErrorOf == "" && (err != nil || got != tc.want):
			t.Errorf("%s: pickedValue = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}

func TestGoLiteral(t *testing.T) {
	inner := &paramStruct{goName: "Inner", fields: []*paramField{
		{apiName: "on", goName: "On", node: &paramScalar{goType: typeBool}},
		{apiName: "age", goName: "Age", node: &paramScalar{goType: typeInt64}},
		{apiName: "name", goName: "Name", required: true, node: &paramScalar{goType: typeString}},
		{apiName: "count", goName: "Count", required: true, node: &paramScalar{goType: typeInt64}},
		{apiName: "unset", goName: "Unset", node: &paramScalar{goType: typeString}},
	}}
	outer := &paramStruct{goName: "Outer", fields: []*paramField{{apiName: "inner", goName: "Inner", node: inner}}}
	f := newGoFile()
	for _, tc := range []struct {
		node    paramNode
		value   any
		pointer bool
		want    string
	}{
		{&paramScalar{goType: typeString}, `a "b"`, false, `"a \"b\""`},
		{&paramScalar{goType: typeString}, "x", true, `new("x")`},
		{&paramScalar{goType: typeBool}, false, true, "new(false)"},
		{&paramScalar{goType: typeInt64}, float64(14400), false, "14400"},
		{&paramScalar{goType: typeInt64}, float64(0), true, "new(int64(0))"},
		{outer, map[string]any{"inner": map[string]any{"on": false, "age": float64(0), "name": "n",
			"count": float64(2)}}, false, `cloudflarev1alpha1.Outer{Inner: &cloudflarev1alpha1.Inner{On: new(false), ` +
			`Age: new(int64(0)), Name: "n", Count: 2}}`},
	} {
		got, err := goLiteral(f, tc.node, tc.value, tc.pointer)
		if err != nil || got != tc.want {
			t.Errorf("goLiteral(%v) = %s, %v; want %s", tc.value, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		node  paramNode
		value any
		want  string
	}{
		{&paramScalar{goType: typeString}, true, "true is not a string"},
		{&paramScalar{goType: typeBool}, "x", "x is not a bool"},
		{&paramScalar{goType: typeInt64}, 1.5, "1.5 is not a whole number"},
		{outer, "x", "x is not an object"},
		{outer, map[string]any{"inner": map[string]any{"on": "x"}}, "inner: on: x is not a bool"},
		{&paramList{items: &paramScalar{goType: typeString}}, []any{"x"}, "cannot write a list's value as Go yet"},
	} {
		if _, err := goLiteral(f, tc.node, tc.value, false); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("goLiteral(%v): err = %v, want one containing %q", tc.value, err, tc.want)
		}
	}
}

func TestWritableByIDRefusesAList(t *testing.T) {
	st := &paramStruct{fields: []*paramField{{apiName: "hosts", node: &paramList{}}}}
	if err := writableByID(st, "setting"); err == nil ||
		!strings.Contains(err.Error(), "setting.hosts is a list, which kflare-gen cannot reset by the setting's ID") {
		t.Errorf("err = %v", err)
	}
	if err := writableByID(&paramMap{}, "setting"); err == nil {
		t.Error("a map was taken for writable by ID")
	}
}
