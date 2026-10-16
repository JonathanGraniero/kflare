/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"sigs.k8s.io/yaml"
)

// rewriteEntry is a rule kind on the rewrite rule, with two types: one that
// rewrites the URL and one that changes response headers.
const rewriteEntry = `
kind: TestRewriteRule
rule: rulesets.RewriteRuleParam
shortName: testrewrite
noun: rewrite rule
doc: TestRewriteRule rewrites.
typeDoc: Type picks the phase.
types:
  - {name: url, phase: http_request_transform, doc: rewrites the URL}
  - {name: headers, phase: http_response_headers_transform, doc: changes response headers}
samples:
  - {type: url, expression: 'true', uri: {path: {value: /new}}}
  - {type: headers, expression: 'true', headers: [{name: x-a, operation: set, value: '1'}]}
fields:
  headers: {key: name, keyDoc: Name is the header's name.}
ignore:
  logging: Not mapped.
  ratelimit: Not mapped.
  exposed_credential_check: Not mapped.
`

// parseEntry reads a rule kind's entry.
func parseEntry(t *testing.T, entry string) ruleKindConfig {
	t.Helper()
	var out ruleKindConfig
	if err := yaml.UnmarshalStrict([]byte(entry), &out); err != nil {
		t.Fatal(err)
	}
	if err := out.validate(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBuildRuleKindMapsTheRewriteRule(t *testing.T) {
	k, err := buildRuleKind(parseEntry(t, rewriteEntry), loadTestRule(t, "rulesets.RewriteRuleParam"), loadTestSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	if k.rule.action != "rewrite" || k.actionConst != "RulesetNewParamsRulesActionRewrite" {
		t.Errorf("action = %s (%s)", k.rule.action, k.actionConst)
	}
	if got := []string{k.phases[0].constant, k.phases[1].constant}; !slices.Equal(got,
		[]string{"PhaseHTTPRequestTransform", "PhaseHTTPResponseHeadersTransform"}) {
		t.Errorf("phases = %v", got)
	}
	structs := make([]string, 0, len(k.structs))
	for _, st := range k.structs {
		structs = append(structs, st.goName+" "+st.path)
	}
	want := []string{"TestRewriteRuleHeader headers[]", "TestRewriteRuleURI uri", "TestRewriteRuleURIPath uri.path",
		"TestRewriteRuleURIQuery uri.query"}
	if !slices.Equal(structs, want) {
		t.Errorf("structs = %v, want %v", structs, want)
	}
	header := k.structs[0]
	if header.fields[0].jsonName != "name" || !header.fields[0].key || !header.fields[1].required {
		t.Errorf("the header's key and operation are not the first, required fields: %+v", header.fields[:2])
	}
	if k.rules[1]["action_parameters"].(map[string]any)["headers"].(map[string]any)["x-a"] == nil {
		t.Errorf("the second sample's rule keys its headers by name: %v", k.rules[1])
	}

	if len(k.full) != 2 {
		t.Fatalf("full specs = %d, want one with values and one with the query's value empty", len(k.full))
	}
	paths := []string{"headers[0].expression", "headers[0].value", "uri.path.expression", "uri.path.value",
		"uri.query.expression", "uri.query.value"}
	for _, p := range paths {
		if value, err := jsonPath(k.full[0].spec, p); err != nil || value == "" {
			t.Errorf("every field set: %s = %q, %v", p, value, err)
		}
	}
	if value, err := jsonPath(k.full[1].spec, "uri.query.value"); err != nil || value != "" {
		t.Errorf("zero values: the query's value = %q, %v, want it empty", value, err)
	}
	query := k.full[1].rule["action_parameters"].(map[string]any)["uri"].(map[string]any)["query"]
	if value, ok := query.(map[string]any)["value"]; !ok || value != "" {
		t.Errorf("zero values: the rule's query = %v, want an empty value", query)
	}
}

// jsonPath returns the string at a path such as headers[0].value in a spec.
func jsonPath(spec map[string]any, p string) (string, error) {
	var path []step
	for part := range strings.SplitSeq(p, ".") {
		name, index, list := strings.Cut(part, "[")
		path = append(path, step{field: name})
		if list {
			i, err := strconv.Atoi(strings.TrimSuffix(index, "]"))
			if err != nil {
				return "", err
			}
			path = append(path, step{index: i})
		}
	}
	value, ok := valueAt(spec, path).(string)
	if !ok {
		return "", fmt.Errorf("%s is not a string", p)
	}
	return value, nil
}

func TestBuildRuleKindRefuses(t *testing.T) {
	rewriteSchema := func(spec *apiSpec) *schema { return spec.Components.Schemas["rulesets_RewriteRule"].AllOf[1] }
	tests := []struct {
		name string
		edit func(e *ruleKindConfig, spec *apiSpec)
		want string
	}{
		{"a field of the rule type left unmapped", func(e *ruleKindConfig, _ *apiSpec) { delete(e.Ignore, "logging") },
			"has a field logging, which rule kinds do not map"},
		{"an ignored field the rule type lacks", func(e *ruleKindConfig, _ *apiSpec) { e.Ignore["nope"] = "x" },
			"nope: the rule type has no such field"},
		{"an adjusted field the rule type lacks", func(e *ruleKindConfig, _ *apiSpec) {
			e.Fields["uri.nope"] = ruleFieldConfig{Doc: "x"}
		}, "uri.nope: the rule type has no such field"},
		{"a map without a key", func(e *ruleKindConfig, _ *apiSpec) { e.Fields["headers"] = ruleFieldConfig{} },
			"headers is a map: name the field its keys go in"},
		{"a key on an object", func(e *ruleKindConfig, _ *apiSpec) {
			e.Fields["uri"] = ruleFieldConfig{Key: "name", KeyDoc: "x"}
		}, "fields.uri.key: uri is not a map"},
		{"a key a value already has", func(e *ruleKindConfig, _ *apiSpec) {
			e.Fields["headers"] = ruleFieldConfig{Key: "value", KeyDoc: "x"}
		}, "the values already have a field value"},
		{"a pattern every made-up value matches", func(e *ruleKindConfig, _ *apiSpec) {
			e.Fields["uri.path.value"] = ruleFieldConfig{Validation: &validationConfig{Pattern: ".*", Reason: "x"}}
		}, "spec.uri.path.value: kflare-gen finds no value its pattern .* refuses"},
		{"a value no sample, example or made-up value fits", func(e *ruleKindConfig, _ *apiSpec) {
			e.Fields["uri.query.expression"] = ruleFieldConfig{Validation: &validationConfig{Pattern: "^/",
				Reason: "x"}}
		}, "kflare-gen cannot make up a value of expression, which the OpenAPI spec gives no valid example of"},
		{"validation of an object", func(e *ruleKindConfig, _ *apiSpec) {
			e.Fields["uri"] = ruleFieldConfig{Validation: &validationConfig{MinLength: new(int64(1)), Reason: "x"}}
		}, "only a string or an integer is validated"},
		{"a range on a string", func(e *ruleKindConfig, _ *apiSpec) {
			e.Fields["uri.path.value"] = ruleFieldConfig{Validation: &validationConfig{Minimum: new(int64(1)), Reason: "x"}}
		}, "a range needs an integer field"},
		{"an unknown phase", func(e *ruleKindConfig, _ *apiSpec) { e.Types[0].Phase = "http_nope" },
			`no Phase constant "http_nope"`},
		{"every field of an object ignored", func(e *ruleKindConfig, _ *apiSpec) {
			e.Ignore["uri.path.value"], e.Ignore["uri.path.expression"] = "x", "x"
		}, "uri.path: every field is ignored"},
		{"no schema for the rule type", func(_ *ruleKindConfig, spec *apiSpec) {
			delete(spec.Components.Schemas, "rulesets_RewriteRule")
		}, "has no schema rulesets_RewriteRule"},
		{"a schema for another action", func(_ *ruleKindConfig, spec *apiSpec) {
			rewriteSchema(spec).Properties["action"] = &schema{Type: "string", Enum: []any{"redirect"}}
		}, "actions are [redirect], not the rule type's rewrite"},
		{"a schema without action parameters", func(_ *ruleKindConfig, spec *apiSpec) {
			delete(rewriteSchema(spec).Properties, "action_parameters")
			delete(spec.Components.Schemas["rulesets_Rule"].Properties, "action_parameters")
		}, "has no action_parameters"},
		{"a union whose schema has no oneOf or anyOf", func(_ *ruleKindConfig, spec *apiSpec) {
			spec.Components.Schemas["rulesets_RewriteHeaders"].AdditionalProperties = json.RawMessage(`{"type":"object"}`)
		}, "its OpenAPI schema has no oneOf or anyOf"},
		{"a sensitive field", func(_ *ruleKindConfig, spec *apiSpec) {
			spec.Components.Schemas["rulesets_RewriteHeaderValue"].Sensitive = true
		}, "headers[].value is sensitive"},
		{"a sample without an expression", func(e *ruleKindConfig, _ *apiSpec) { delete(e.Samples[0], "expression") },
			"samples[0]: sets no expression"},
		{"a sample with an empty expression", func(e *ruleKindConfig, _ *apiSpec) { e.Samples[0]["expression"] = "" },
			"expression is not a non-empty string"},
		{"a sample of another type", func(e *ruleKindConfig, _ *apiSpec) { e.Samples[0]["type"] = "headers" },
			"type is headers, not url"},
		{"a sample without a type", func(e *ruleKindConfig, _ *apiSpec) { delete(e.Samples[0], "type") },
			"sets no type"},
		{"a sample with an enabled that is not a bool", func(e *ruleKindConfig, _ *apiSpec) {
			e.Samples[0]["enabled"] = "yes"
		}, "enabled is not a bool"},
		{"a sample with a priority that is not whole", func(e *ruleKindConfig, _ *apiSpec) {
			e.Samples[0]["priority"] = 1.5
		}, "priority is not a whole number"},
		{"a sample with an unknown field", func(e *ruleKindConfig, _ *apiSpec) { e.Samples[0]["colour"] = "blue" },
			"spec has no field colour"},
		{"a sample with a number for a string", func(e *ruleKindConfig, _ *apiSpec) {
			e.Samples[0]["uri"] = map[string]any{"path": map[string]any{"value": float64(1)}}
		}, "spec.uri.path.value is not a string"},
		{"a sample with a string for an object", func(e *ruleKindConfig, _ *apiSpec) { e.Samples[0]["uri"] = "x" },
			"spec.uri is not an object"},
		{"a sample outside an enum", func(e *ruleKindConfig, _ *apiSpec) {
			header(e)["operation"] = "append"
		}, `spec.headers[0].operation is "append", not one of add, set, remove`},
		{"a sample shorter than its minimum", func(e *ruleKindConfig, _ *apiSpec) { header(e)["value"] = "" },
			"spec.headers[0].value is shorter than 1"},
		{"a sample without a required field", func(e *ruleKindConfig, _ *apiSpec) { delete(header(e), "operation") },
			"spec.headers[0].operation is required"},
		{"a sample that lists a key twice", func(e *ruleKindConfig, _ *apiSpec) {
			e.Samples[1]["headers"] = []any{header(e), header(e)}
		}, "spec.headers lists x-a twice"},
		{"a sample with an empty list", func(e *ruleKindConfig, _ *apiSpec) { e.Samples[1]["headers"] = []any{} },
			"spec.headers is not a list with items"},
		{"a sample whose item is not an object", func(e *ruleKindConfig, _ *apiSpec) {
			e.Samples[1]["headers"] = []any{"x-a"}
		}, "spec.headers[0] is not an object"},
		{"a pattern a sample does not match", func(e *ruleKindConfig, _ *apiSpec) {
			e.Fields["uri.path.value"] = ruleFieldConfig{Validation: &validationConfig{Pattern: "^/", Reason: "x"}}
			e.Samples[0]["uri"] = map[string]any{"path": map[string]any{"value": "new"}}
		}, "spec.uri.path.value does not match ^/"},
		{"an entry rule whose invalid spec does not fit", func(e *ruleKindConfig, _ *apiSpec) {
			e.Rules = []ruleConfig{{Rule: "true", Message: "m", Reason: "r",
				Invalid: map[string]any{"type": "url", "expression": "true", "colour": "blue"}}}
		}, "rules[0].invalid: spec has no field colour"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry, spec := parseEntry(t, rewriteEntry), loadTestSpec(t)
			tc.edit(&entry, spec)
			_, err := buildRuleKind(entry, loadTestRule(t, "rulesets.RewriteRuleParam"), spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// header returns the first header of the second sample of a rewrite entry.
func header(e *ruleKindConfig) map[string]any {
	return e.Samples[1]["headers"].([]any)[0].(map[string]any)
}

func TestBuildRuleKindRefusesTheTestKindsUnmapped(t *testing.T) {
	cfg, err := loadConfig("testdata/generator.yaml")
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]ruleKindConfig{}
	for _, e := range cfg.RuleKinds {
		entries[e.Kind] = e
	}
	tests := []struct {
		name, kind string
		edit       func(e *ruleKindConfig, spec *apiSpec)
		want       string
	}{
		{"a map of lists", "TestSkipRule", func(e *ruleKindConfig, _ *apiSpec) {
			delete(e.Ignore, "rules")
			e.Fields = map[string]ruleFieldConfig{"rules": {Key: "ruleset", KeyDoc: "x"}}
		}, "rules is a map of scalars or lists"},
		{"lengths of an integer", "TestExecuteRule", func(e *ruleKindConfig, _ *apiSpec) {
			e.Fields = map[string]ruleFieldConfig{"overrides.rules[].score_threshold": {
				Validation: &validationConfig{MinLength: new(int64(1)), Reason: "x"}}}
		}, "lengths and patterns need a string field"},
		{"a number that is not an integer", "TestExecuteRule", func(_ *ruleKindConfig, spec *apiSpec) {
			rules := spec.Components.Schemas["rulesets_ExecuteOverrides"].Properties["rules"]
			resolved, err := spec.resolve(rules)
			if err != nil {
				t.Fatal(err)
			}
			resolved.Items.Properties["score_threshold"] = &schema{Type: "number"}
		}, "score_threshold is a number the OpenAPI spec does not say is an integer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry, spec := entries[tc.kind], loadTestSpec(t)
			entry.Ignore = cloneMap(entry.Ignore)
			tc.edit(&entry, spec)
			_, err := buildRuleKind(entry, loadTestRule(t, entry.Rule), spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// cloneMap returns a copy of m.
func cloneMap[V any](m map[string]V) map[string]V {
	out := make(map[string]V, len(m))
	maps.Copy(out, m)
	return out
}

func TestRuleRefuses(t *testing.T) {
	tests := []struct{ name, rule, want string }{
		{"a name without a type", "rulesets", "not <package>.<type>"},
		{"a type the package lacks", "rulesets.NopeRuleParam", "has no type NopeRuleParam"},
		{"a rule type for every action", "rulesets.RulesetNewParamsRule", "allows 21 actions, not one"},
		{"a type that is not a rule", "rulesets.LoggingParam", "has no action, or no action parameters"},
	}
	for _, tc := range tests {
		if _, err := (sdk{dir: "."}).rule(tc.rule); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", tc.name, err, tc.want)
		}
	}
}

func TestPropertyCountRules(t *testing.T) {
	fields := []*paramField{{jsonName: "name", key: true}, {jsonName: "a"}, {jsonName: "b"}}
	tests := []struct {
		name     string
		min, max *int64
		want     []celRule
	}{
		{"none", nil, nil, nil},
		{"exactly one", new(int64(1)), new(int64(1)),
			[]celRule{{rule: "[has(self.a), has(self.b)].exists_one(x, x)", message: "must set exactly one of a and b"}}},
		{"at least one", new(int64(1)), nil,
			[]celRule{{rule: "has(self.a) || has(self.b)", message: "must set at least one of a and b"}}},
		{"at least two, at most two", new(int64(2)), new(int64(2)), []celRule{
			{rule: "[has(self.a), has(self.b)].filter(x, x).size() >= 2", message: "must set at least 2 of a and b"},
			{rule: "[has(self.a), has(self.b)].filter(x, x).size() <= 2", message: "must set at most 2 of a and b"},
		}},
		{"at most one", nil, new(int64(1)),
			[]celRule{{rule: "[has(self.a), has(self.b)].filter(x, x).size() <= 1", message: "must set at most 1 of a and b"}}},
	}
	for _, tc := range tests {
		got := propertyCountRules(&schema{MinProperties: tc.min, MaxProperties: tc.max}, fields)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: rules = %v, want %v", tc.name, got, tc.want)
		}
	}
	if got := propertyCountRules(nil, fields); got != nil {
		t.Errorf("no schema: rules = %v", got)
	}
}

func TestBreakRule(t *testing.T) {
	text := &paramScalar{goType: typeString, validation: validation{minLength: new(int64(1))}}
	st := &paramStruct{fields: []*paramField{
		{jsonName: "kind", required: true, node: &paramScalar{goType: typeString, enum: []string{"a", "b"}}},
		{jsonName: "x", node: text}, {jsonName: "y", node: text},
		{jsonName: "z", node: &paramStruct{}},
	}}
	tests := []struct {
		message string
		object  map[string]any
		want    map[string]any
	}{
		{"must set at least one of x and y", map[string]any{"kind": "a", "x": "1", "y": "2"}, map[string]any{"kind": "a"}},
		{"must set exactly one of x and y", map[string]any{"kind": "a", "x": "1"},
			map[string]any{"kind": "a", "x": "1", "y": "x"}},
		{"must set at most 1 of x and y", map[string]any{"kind": "a"},
			map[string]any{"kind": "a", "x": "x", "y": "x"}},
	}
	for _, tc := range tests {
		if err := breakRule(st, celRule{message: tc.message}, tc.object); err != nil {
			t.Errorf("%s: %v", tc.message, err)
		} else if !reflect.DeepEqual(tc.object, tc.want) {
			t.Errorf("%s: object = %v, want %v", tc.message, tc.object, tc.want)
		}
	}
	if err := breakRule(st, celRule{message: "must set exactly one of x and y"},
		map[string]any{"kind": "a", "x": "1", "y": "2", "z": map[string]any{}}); err == nil {
		t.Error("an object with every field set: err = nil, want one")
	}
	for _, message := range []string{"something else", "must be one of: kind a with x"} {
		if err := breakRule(st, celRule{message: message}, map[string]any{}); err == nil {
			t.Errorf("%s: err = nil, want one", message)
		}
	}
}

// itemsVariants is a oneOf rule on testItem: an item that sets its value, or
// one that removes it, which holds no value.
var itemsVariants = celRule{message: "must be one of: op set with value; op remove", variants: []variant{
	{describe: "op set with value", fixed: map[string]string{"op": "set"}, required: []string{"value"},
		holds: map[string]bool{"op": true, "value": true}},
	{describe: "op remove", fixed: map[string]string{"op": "remove"}, holds: map[string]bool{"op": true}},
}}

// testItem is the struct of an item in a map: its key, the operation, and a
// value.
func testItem() *paramStruct {
	text := &paramScalar{goType: typeString, validation: validation{minLength: new(int64(1))}}
	return &paramStruct{fields: []*paramField{
		{jsonName: "name", key: true, required: true, node: text},
		{jsonName: "op", required: true, node: &paramScalar{goType: typeString, enum: []string{"set", "remove"}}},
		{jsonName: "value", node: text},
		{jsonName: "nested", node: &paramStruct{}},
	}}
}

func TestVariants(t *testing.T) {
	st := testItem()
	set, remove := itemsVariants.variants[0], itemsVariants.variants[1]
	tests := []struct {
		object       map[string]any
		set, removes bool
	}{
		{map[string]any{"name": "a", "op": "set", "value": "1"}, true, false},
		{map[string]any{"name": "a", "op": "set"}, false, false},
		{map[string]any{"name": "a", "op": "remove"}, false, true},
		{map[string]any{"name": "a", "op": "remove", "value": "1"}, false, false},
	}
	for _, tc := range tests {
		if set.matches(st, tc.object) != tc.set || remove.matches(st, tc.object) != tc.removes {
			t.Errorf("%v: matches set %t, remove %t", tc.object, set.matches(st, tc.object),
				remove.matches(st, tc.object))
		}
	}

	base := map[string]any{"name": "a", "op": "remove"}
	object, ok := variantObject(st, set, base, map[*paramField]any{st.fields[2]: "from a sample"})
	if want := map[string]any{"name": "a", "op": "set", "value": "from a sample"}; !ok ||
		!reflect.DeepEqual(object, want) {
		t.Errorf("the set variant's object = %v, %t, want %v", object, ok, want)
	}
	if object, ok := variantObject(st, set, base, nil); !ok || object["value"] != "x" {
		t.Errorf("the set variant's object without a sample's value = %v, %t", object, ok)
	}
	nested := variant{fixed: map[string]string{"op": "set"}, required: []string{"nested"},
		holds: map[string]bool{"op": true, "nested": true}}
	if object, ok := variantObject(st, nested, base, nil); ok {
		t.Errorf("a variant that requires an object no sample sets: object = %v, want none", object)
	}
	if _, ok := variantObject(st, nested, map[string]any{"name": "a", "nested": map[string]any{}}, nil); !ok {
		t.Error("a variant that requires an object the sample sets: no object")
	}
}

func TestVariantBreaches(t *testing.T) {
	k := &ruleKind{samples: []map[string]any{{"expression": "true",
		"items": []any{map[string]any{"name": "a", "op": "set", "value": "1"}}}}}
	k.variantBreaches(testItem(), itemsVariants, 0, []step{{field: "items"}, {index: 0}}, nil)
	got := map[string]any{}
	for _, e := range k.invalid {
		if e.message != itemsVariants.message {
			t.Errorf("%s: message = %q", e.what, e.message)
		}
		got[e.what] = valueAt(e.spec, []step{{field: "items"}, {index: 0}})
	}
	want := map[string]any{
		"items[0] as op set with value, without value": map[string]any{"name": "a", "op": "set"},
		"items[0] as op remove, with value too":        map[string]any{"name": "a", "op": "remove", "value": "1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("breaches = %v, want %v", got, want)
	}
	if k.samples[0]["items"].([]any)[0].(map[string]any)["op"] != "set" {
		t.Errorf("the breaches changed the sample: %v", k.samples[0])
	}

	// A variant that sets without a value holds what breaks the one with a
	// value, and the other way round: only the removal breaks a variant.
	overlapping := itemsVariants
	overlapping.variants = append(slices.Clone(itemsVariants.variants), variant{describe: "op set",
		fixed: map[string]string{"op": "set"}, holds: map[string]bool{"op": true}})
	k.invalid = nil
	k.variantBreaches(testItem(), overlapping, 0, []step{{field: "items"}, {index: 0}}, nil)
	if len(k.invalid) != 1 || k.invalid[0].what != "items[0] as op remove, with value too" {
		t.Errorf("breaches of overlapping variants = %v, want only the removal with a value", k.invalid)
	}
}

func TestFullStructZeroesOptionalScalarsOnly(t *testing.T) {
	flag := &paramScalar{goType: typeBool}
	st := &paramStruct{fields: []*paramField{
		{jsonName: "required", required: true, node: flag}, {jsonName: "optional", node: flag},
	}}
	for zero, want := range map[bool]map[string]any{
		false: {"required": true, "optional": true},
		true:  {"required": true, "optional": false},
	} {
		if got, err := fullStruct(st, nil, zero); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("zero %t: %v, %v, want %v", zero, got, err, want)
		}
	}
}

func TestMismatchAndListBounds(t *testing.T) {
	pattern := func(p string, maxLength *int64) *paramScalar {
		return &paramScalar{goType: typeString, validation: validation{pattern: p, maxLength: maxLength}}
	}
	tests := []struct {
		n    *paramScalar
		want string
		ok   bool
	}{
		{pattern("^/", nil), "kflare-not-matching", true},
		{pattern("^[a-z-]+$", nil), "kflare not matching!", true},
		{pattern("^/", new(int64(3))), "", true},
		{pattern(".*", nil), "", false},
	}
	for _, tc := range tests {
		if got, ok := mismatch(tc.n); got != tc.want || ok != tc.ok {
			t.Errorf("mismatch(%s) = %q, %t, want %q, %t", tc.n.validation.pattern, got, ok, tc.want, tc.ok)
		}
	}

	one := new(int64(1))
	bounds := []struct {
		node   paramNode
		min    *int64
		unique bool
	}{
		{&paramList{set: true, minItems: one}, one, true},
		{&paramList{}, nil, false},
		{&paramMap{minItems: one}, one, true},
		{&paramScalar{}, nil, false},
	}
	for _, tc := range bounds {
		if minItems, unique := listBounds(tc.node); minItems != tc.min || unique != tc.unique {
			t.Errorf("listBounds(%T) = %v, %t", tc.node, minItems, unique)
		}
	}
}

func TestScalarValue(t *testing.T) {
	f := &paramField{jsonName: "id"}
	n := &paramScalar{goType: typeString, validation: validation{pattern: "^[0-9]+$"}, example: "42"}
	if value, ok := scalarValue(f, n, map[*paramField]any{f: "7"}); !ok || value != "7" {
		t.Errorf("with a sample's value: %v, %t", value, ok)
	}
	if value, ok := scalarValue(f, n, nil); !ok || value != "42" {
		t.Errorf("with the schema's example: %v, %t", value, ok)
	}
	n.example = "not a number"
	if value, ok := scalarValue(f, n, nil); ok {
		t.Errorf("with an example its pattern refuses: %v, want none", value)
	}
	n.validation.pattern = ""
	if value, ok := scalarValue(f, n, nil); !ok || value != "not a number" {
		t.Errorf("with no pattern: %v, %t", value, ok)
	}
	if value, ok := scalarValue(f, &paramScalar{goType: typeInt64, example: 5.0}, nil); !ok || value != int64(5) {
		t.Errorf("an integer's example: %v (%T), %t", value, value, ok)
	}
}

func TestSomeValue(t *testing.T) {
	tests := []struct {
		name string
		node paramNode
		want any
		ok   bool
	}{
		{"an enum", &paramScalar{goType: typeString, enum: []string{"b", "a"}}, "b", true},
		{"a bool", &paramScalar{goType: typeBool}, true, true},
		{"an integer", &paramScalar{goType: typeInt64}, int64(1), true},
		{"an integer with a minimum", &paramScalar{goType: typeInt64, validation: validation{minimum: new(9.0)}},
			int64(10), true},
		{"a string with a minimum length", &paramScalar{goType: typeString,
			validation: validation{minLength: new(int64(3))}}, "xxx", true},
		{"a string with a pattern x does not match", &paramScalar{goType: typeString,
			validation: validation{pattern: "^/"}}, nil, false},
		{"an object", &paramStruct{}, nil, false},
	}
	for _, tc := range tests {
		got, ok := someValue(tc.node)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: someValue = %v, %t, want %v, %t", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestOtherScalar(t *testing.T) {
	tests := []struct {
		name  string
		node  *paramScalar
		value any
		want  any
	}{
		{"a bool", &paramScalar{goType: typeBool}, true, false},
		{"an integer", &paramScalar{goType: typeInt64}, int64(4), int64(5)},
		{"an integer at its maximum", &paramScalar{goType: typeInt64, validation: validation{maximum: new(4.0)}},
			int64(4), int64(3)},
		{"a string", &paramScalar{goType: typeString}, "/new", "/nex"},
	}
	for _, tc := range tests {
		got, err := otherScalar(tc.node, tc.value)
		if err != nil || got != tc.want {
			t.Errorf("%s: otherScalar = %v, %v, want %v", tc.name, got, err, tc.want)
		}
	}
	pinned := &paramScalar{goType: typeInt64, validation: validation{minimum: new(4.0), maximum: new(4.0)}}
	if _, err := otherScalar(pinned, int64(4)); err == nil {
		t.Error("an integer that can only be 4: err = nil, want one")
	}
}

func TestCheckScalarBounds(t *testing.T) {
	ranged := &paramScalar{goType: typeInt64, validation: validation{minimum: new(1.0), maximum: new(9.0),
		exclusiveMaximum: true}}
	tests := []struct {
		value any
		want  string
	}{
		{float64(1.5), "is not a whole number"},
		{int64(0), "is below its minimum"},
		{int64(9), "is above its maximum"},
	}
	for _, tc := range tests {
		if _, err := checkScalar(ranged, tc.value, "x"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want one containing %q", tc.value, err, tc.want)
		}
	}
	if got, err := checkScalar(ranged, 5, "x"); err != nil || got != int64(5) {
		t.Errorf("an int: %v, %v", got, err)
	}
	long := &paramScalar{goType: typeString, validation: validation{maxLength: new(int64(2))}}
	if _, err := checkScalar(long, "abc", "x"); err == nil || !strings.Contains(err.Error(), "longer than 2") {
		t.Errorf("a long string: err = %v", err)
	}
	if _, err := checkScalar(&paramScalar{goType: typeBool}, "true", "x"); err == nil {
		t.Error("a string for a bool: err = nil, want one")
	}
}

func TestAPINodeSortsSets(t *testing.T) {
	set := &paramList{items: &paramScalar{goType: typeString}, set: true}
	if got := apiNode(set, []any{"b", "a"}); !reflect.DeepEqual(got, []any{"a", "b"}) {
		t.Errorf("a set = %v, want it sorted", got)
	}
	list := &paramList{items: &paramScalar{goType: typeString}}
	if got := apiNode(list, []any{"b", "a"}); !reflect.DeepEqual(got, []any{"b", "a"}) {
		t.Errorf("a list = %v, want it in its order", got)
	}
}

func TestSpecEdits(t *testing.T) {
	spec := map[string]any{"a": []any{map[string]any{"b": "1"}}, "c": int64(2)}
	path := []step{{field: "a"}, {index: 0}, {field: "b"}}
	if pathString(path) != "spec.a[0].b" || relativePath(path) != "a[0].b" {
		t.Errorf("paths = %s, %s", pathString(path), relativePath(path))
	}
	copied := copySpec(spec)
	setAt(copied, path, "2")
	setAt(copied, []step{{field: "c"}}, nil)
	setAt(copied, []step{{field: "a"}, {index: 0}}, map[string]any{"b": "3"})
	if !reflect.DeepEqual(copied, map[string]any{"a": []any{map[string]any{"b": "3"}}}) {
		t.Errorf("edited = %v", copied)
	}
	if valueAt(spec, path) != "1" || spec["c"] != int64(2) {
		t.Errorf("the copy shares values with the spec: %v", spec)
	}
	if n, ok := wholeNumber(3); !ok || n != 3 {
		t.Errorf("wholeNumber(3) = %d, %t", n, ok)
	}
}

func TestSingularAndKebab(t *testing.T) {
	singulars := map[string]string{"Headers": "Header", "Policies": "Policy", "Address": "Address", "URI": "URI"}
	for in, want := range singulars {
		if got := singular(in); got != want {
			t.Errorf("singular(%s) = %s, want %s", in, got, want)
		}
	}
	for in, want := range map[string]string{"urlRewrite": "url-rewrite", "a": "a", "requestHeaders": "request-headers"} {
		if got := kebab(in); got != want {
			t.Errorf("kebab(%s) = %s, want %s", in, got, want)
		}
	}
}

// TestRuleKindRulesParse parses every CEL rule of the rule kinds kflare-gen
// generates, here and in testdata: an API server checks those of committed
// kinds when envtest installs their CRDs, but nothing else checks the forms
// only the test kinds use.
func TestRuleKindRulesParse(t *testing.T) {
	env, err := cel.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	spec := loadTestSpec(t)
	parsed := 0
	for _, path := range []string{"generator.yaml", "testdata/generator.yaml"} {
		cfg, err := loadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range cfg.RuleKinds {
			k, err := buildRuleKind(entry, loadTestRule(t, entry.Rule), spec)
			if err != nil {
				t.Fatal(err)
			}
			rules := slices.Clone(k.specRules)
			for _, st := range k.structs {
				rules = append(rules, st.rules...)
			}
			for _, r := range rules {
				if _, issues := env.Parse(r.rule); issues.Err() != nil {
					t.Errorf("%s: %s does not parse: %v", k.name, r.rule, issues.Err())
				}
				parsed++
			}
		}
	}
	if parsed == 0 {
		t.Error("parsed no rule")
	}
}
