/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"encoding/json"
	"fmt"
	"go/types"
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
	k, err := buildRuleKind(parseEntry(t, rewriteEntry), testRuleLoader(t), loadTestSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	if a := k.actions[0]; len(k.actions) != 1 || a.name != "rewrite" ||
		a.constant != "RulesetNewParamsRulesActionRewrite" || a.params != k.params || k.severalActions() {
		t.Errorf("actions = %+v", k.actions)
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
	if k.bodies[1]["action_parameters"].(map[string]any)["headers"].(map[string]any)["x-a"] == nil {
		t.Errorf("the second sample's rule keys its headers by name: %v", k.bodies[1])
	}

	if names := []string{k.specName(0), k.specName(2)}; !slices.Equal(names, []string{"samples[0]", "tests[0]"}) {
		t.Errorf("spec names = %v", names)
	}
	swap := slices.IndexFunc(k.changes, func(e ruleEdit) bool { return e.what == "uri.path holding expression instead" })
	if swap < 0 {
		t.Fatalf("no change sets the path's expression in place of its value: %v", k.changes)
	}
	path := valueAt(k.changes[swap].spec, []step{{field: "uri"}, {field: "path"}}).(map[string]any)
	if _, hasValue := path["value"]; hasValue || path["expression"] == nil {
		t.Errorf("the swapped path = %v, want an expression and no value", path)
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
		{"rules on a field that is not an object", func(e *ruleKindConfig, _ *apiSpec) {
			e.Fields["uri.path.value"] = ruleFieldConfig{Rules: []ruleConfig{{Rule: "true", Message: "m"}}}
		}, "fields.uri.path.value.rules: uri.path.value is not an object"},
		{"a field's rule whose invalid spec breaks no rule", func(e *ruleKindConfig, _ *apiSpec) {
			e.Fields["uri.path"] = ruleFieldConfig{Rules: []ruleConfig{{Rule: "true", Message: "never", Reason: "r",
				Invalid: map[string]any{"type": "url", "expression": "true", "uri": map[string]any{
					"path": map[string]any{"value": "/x"}}}}}}
		}, `fields.uri.path.rules[0].invalid: breaks no rule with the message "never"`},
		{"a sample that breaks a field's rule", func(e *ruleKindConfig, _ *apiSpec) {
			e.Fields["uri.path"] = ruleFieldConfig{Rules: []ruleConfig{{Rule: "!has(self.value)", Message: "no value"}}}
		}, `samples[0]: breaks the rule "no value"`},
		{"a test of none of the types", func(e *ruleKindConfig, _ *apiSpec) {
			e.Tests = []map[string]any{{"type": "nope", "expression": "true"}}
		}, "tests[0]: type nope is none of the kind's types"},
		{"a test that breaks a rule", func(e *ruleKindConfig, _ *apiSpec) {
			e.Tests = []map[string]any{{"type": "url", "expression": "true", "uri": map[string]any{
				"path": map[string]any{"value": "/x", "expression": "y"}}}}
		}, `tests[0]: breaks the rule "must set exactly one of expression and value"`},
		{"a test that is not a spec", func(e *ruleKindConfig, _ *apiSpec) {
			e.Tests = []map[string]any{{"type": "url"}}
		}, "tests[0]: sets no expression"},
		{"a rule that does not compile", func(e *ruleKindConfig, _ *apiSpec) {
			e.Rules = []ruleConfig{{Rule: "self.", Message: "m"}}
		}, `samples[0]: the rule "self."`},
		{"a rule that gives no bool", func(e *ruleKindConfig, _ *apiSpec) {
			e.Rules = []ruleConfig{{Rule: "1", Message: "m"}}
		}, `samples[0]: the rule "1" gives 1, not a bool`},
		{"a rule that fails on a sample", func(e *ruleKindConfig, _ *apiSpec) {
			e.Rules = []ruleConfig{{Rule: "self.nope == 1", Message: "m"}}
		}, `samples[0]: the rule "self.nope == 1"`},
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
		}, "actions are [redirect], not rewrite"},
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
			_, err := buildRuleKind(entry, testRuleLoader(t), spec)
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
			_, err := buildRuleKind(entry, testRuleLoader(t), spec)
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
		{"a type that is not a rule", "rulesets.LoggingParam", "has no action"},
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
			[]celRule{{rule: "(has(self.a) ? 1 : 0) + (has(self.b) ? 1 : 0) == 1", message: "must set exactly one of a and b"}}},
		{"at least one", new(int64(1)), nil,
			[]celRule{{rule: "has(self.a) || has(self.b)", message: "must set at least one of a and b"}}},
		{"at least two, at most two", new(int64(2)), new(int64(2)), []celRule{
			{rule: "(has(self.a) ? 1 : 0) + (has(self.b) ? 1 : 0) >= 2", message: "must set at least 2 of a and b"},
			{rule: "(has(self.a) ? 1 : 0) + (has(self.b) ? 1 : 0) <= 2", message: "must set at most 2 of a and b"},
		}},
		{"at most one", nil, new(int64(1)),
			[]celRule{{rule: "(has(self.a) ? 1 : 0) + (has(self.b) ? 1 : 0) <= 1", message: "must set at most 1 of a and b"}}},
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
			map[string]any{"kind": "a", "x": "x", "y": "x", "z": map[string]any{}}},
	}
	for _, tc := range tests {
		if err := breakRule(st, celRule{message: tc.message}, tc.object, nil); err != nil {
			t.Errorf("%s: %v", tc.message, err)
		} else if !reflect.DeepEqual(tc.object, tc.want) {
			t.Errorf("%s: object = %v, want %v", tc.message, tc.object, tc.want)
		}
	}
	if err := breakRule(st, celRule{message: "must set exactly one of x and y"},
		map[string]any{"kind": "a", "x": "1", "y": "2", "z": map[string]any{}}, nil); err == nil {
		t.Error("an object with every field set: err = nil, want one")
	}
	for _, message := range []string{"something else", "must be one of: kind a with x"} {
		if err := breakRule(st, celRule{message: message}, map[string]any{}, nil); err == nil {
			t.Errorf("%s: err = nil, want one", message)
		}
	}
}

func TestRuleBreachesMustBreakTheRule(t *testing.T) {
	text := &paramScalar{goType: typeString}
	params := &paramStruct{fields: []*paramField{
		{jsonName: "a", required: true, node: text}, {jsonName: "b", required: true, node: text},
	}, rules: []celRule{{rule: "has(self.a) || has(self.b)", message: "must set at least one of a and b"}}}
	checker, err := newCELChecker()
	if err != nil {
		t.Fatal(err)
	}
	k := &paramKind{params: params, cel: checker,
		samples: []map[string]any{{"expression": "true", "a": "x", "b": "y"}}}
	// Both fields are required, so the breach leaves them set, and the rule
	// still holds.
	if err := k.ruleBreaches(nil); err == nil || !strings.Contains(err.Error(),
		`breaks no rule with the message "must set at least one of a and b"`) {
		t.Errorf("a breach that breaks nothing: err = %v", err)
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
	k := &paramKind{samples: []map[string]any{{"expression": "true",
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
		{pattern("^[a-z-]+$", nil), "KFLARE-NOT-MATCHING", true},
		{pattern("^[A-Za-z-]+$", nil), "kflare not matching!", true},
		{pattern("^[ -~]+$", nil), "kflare-nöt-matching", true},
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
		{"an integer enum", &paramScalar{goType: typeInt64, enum: []string{"308", "301"}}, int64(308), true},
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
	status := &paramScalar{goType: typeInt64, enum: []string{"301", "308"}}
	_, err := checkScalar(status, 302, "x")
	if err == nil || !strings.Contains(err.Error(), "x is 302, not one of 301, 308") {
		t.Errorf("an integer outside its enum: err = %v", err)
	}
	if got, err := checkScalar(status, 308, "x"); err != nil || got != int64(308) {
		t.Errorf("an integer in its enum: %v, %v", got, err)
	}
}

func TestFullScalarTakesAnEnumValueNoSampleUses(t *testing.T) {
	f := &paramField{jsonName: "statusCode"}
	status := &paramScalar{goType: typeInt64, enum: []string{"301", "302"}}
	if got, ok := fullScalar(f, status, map[*paramField]any{f: int64(301)}); !ok || got != int64(302) {
		t.Errorf("an integer enum the samples set to 301: %v, %t, want 302", got, ok)
	}
	one := &paramScalar{goType: typeString, enum: []string{"set"}}
	if got, ok := fullScalar(f, one, map[*paramField]any{f: "set"}); !ok || got != "set" {
		t.Errorf("an enum of one value: %v, %t", got, ok)
	}
	if got, ok := fullScalar(f, status, nil); !ok || got != int64(301) {
		t.Errorf("an enum no sample sets: %v, %t, want its first value", got, ok)
	}
}

func TestTooMany(t *testing.T) {
	two := new(int64(2))
	pack := &paramScalar{goType: typeString, validation: validation{pattern: "^[a-z0-9]+$"}}
	long, ok := tooMany(&paramList{items: pack, maxItems: two, set: true}, []any{"a"})
	if !ok || !reflect.DeepEqual(long, []any{"a0", "a1", "a2"}) {
		t.Errorf("a set of strings: %v, %t", long, ok)
	}
	if _, ok := tooMany(&paramList{items: pack, maxItems: two}, []any{"A"}); ok {
		t.Error("copies the pattern refuses: ok, want none")
	}
	if _, ok := tooMany(&paramList{items: pack}, []any{"a"}); ok {
		t.Error("a list without a maximum: ok, want none")
	}
	if _, ok := tooMany(&paramList{items: &paramStruct{}, maxItems: two}, []any{map[string]any{}}); ok {
		t.Error("a list of objects: ok, want none")
	}
	key := &paramField{jsonName: "name", key: true, node: &paramScalar{goType: typeString}}
	entries := &paramMap{value: &paramStruct{fields: []*paramField{key}}, maxItems: new(int64(1))}
	long, ok = tooMany(entries, []any{map[string]any{"name": "accept", "action": "normalize"}})
	want := []any{map[string]any{"name": "accept0", "action": "normalize"}, map[string]any{"name": "accept1",
		"action": "normalize"}}
	if !ok || !reflect.DeepEqual(long, want) {
		t.Errorf("a map: %v, %t", long, ok)
	}
	if _, ok := tooMany(&paramScalar{}, []any{"a"}); ok {
		t.Error("a scalar: ok, want none")
	}
}

func TestOutsideEnum(t *testing.T) {
	tests := []struct {
		name string
		n    *paramScalar
		want any
		ok   bool
	}{
		{"a string enum", &paramScalar{goType: typeString, enum: []string{"a"}}, "kflare-not-in-enum", true},
		{"an integer enum", &paramScalar{goType: typeInt64, enum: []string{"308", "301"}}, int64(309), true},
		{"an enum of true", &paramScalar{goType: typeBool, enum: []string{"true"}}, false, true},
		{"an enum of false", &paramScalar{goType: typeBool, enum: []string{"false"}}, true, true},
		{"an enum of both bools", &paramScalar{goType: typeBool, enum: []string{"true", "false"}}, nil, false},
	}
	for _, tc := range tests {
		if got, ok := outsideEnum(tc.n); got != tc.want || ok != tc.ok {
			t.Errorf("%s: %v, %t, want %v, %t", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCountRules(t *testing.T) {
	one := new(int64(1))
	only := &paramField{jsonName: "a"}
	if rules := countRules(&schema{MinProperties: one, MaxProperties: one},
		[]*paramField{{jsonName: "name", key: true}, only}); rules != nil || !only.required {
		t.Errorf("one field left of at least one: rules = %v, required = %t, want none and required", rules,
			only.required)
	}
	optional := &paramField{jsonName: "a"}
	if rules := countRules(&schema{MaxProperties: one}, []*paramField{optional}); rules != nil || optional.required {
		t.Errorf("one field left of at most one: rules = %v, required = %t, want none", rules, optional.required)
	}
	if rules := countRules(nil, []*paramField{{jsonName: "a"}}); rules != nil {
		t.Errorf("one field without a schema: rules = %v", rules)
	}
	two := []*paramField{{jsonName: "a"}, {jsonName: "b"}}
	if rules := countRules(&schema{MinProperties: one}, two); len(rules) != 1 || two[0].required || two[1].required {
		t.Errorf("two fields: rules = %v, want the count rule", rules)
	}
}

func TestBuildRuleKindTestsRulesAndKeys(t *testing.T) {
	e := parseEntry(t, rewriteEntry)
	e.Tests = []map[string]any{{"type": "headers", "expression": "true",
		"headers": []any{map[string]any{"name": "x-b", "operation": "set", "value": "2"}}}}
	e.Fields["headers"] = ruleFieldConfig{Key: "name", KeyDoc: "Name.",
		KeyValidation: &validationConfig{MinLength: new(int64(1)), Pattern: "^[a-z-]+$", Reason: "r"}}
	e.Fields["uri.path"] = ruleFieldConfig{Rules: []ruleConfig{{Rule: "!has(self.value) || self.value != '/'",
		Message: "a path other than /", Reason: "r", Invalid: map[string]any{"type": "url", "expression": "true",
			"uri": map[string]any{"path": map[string]any{"value": "/"}}}}}}
	e.Rules = []ruleConfig{{Rule: "!has(self.description) || self.description != 'changed by kflare-gen'",
		Message: "kept description", Reason: "r", Invalid: map[string]any{"type": "url", "expression": "true",
			"description": "changed by kflare-gen", "uri": map[string]any{"path": map[string]any{"value": "/x"}}}}}
	k, err := buildRuleKind(e, testRuleLoader(t), loadTestSpec(t))
	if err != nil {
		t.Fatal(err)
	}

	if len(k.samples) != 2 || len(k.tests) != 1 || len(k.specs()) != 3 || len(k.bodies) != 3 {
		t.Errorf("samples, tests, specs and rules = %d, %d, %d, %d", len(k.samples), len(k.tests), len(k.specs()),
			len(k.bodies))
	}
	if p := k.phaseOf(k.tests[0]); p.constant != "PhaseHTTPResponseHeadersTransform" {
		t.Errorf("the test's phase = %s", p.constant)
	}
	if slices.ContainsFunc(k.changes, func(c ruleEdit) bool { return c.what == "a new description" }) {
		t.Error("a change that breaks a rule is kept")
	}
	if !slices.ContainsFunc(k.changes, func(c ruleEdit) bool { return c.sample == 2 }) {
		t.Error("the test gets no change")
	}
	pathStruct := k.structs[2]
	entry := slices.IndexFunc(pathStruct.rules, func(r celRule) bool { return r.fromEntry })
	if pathStruct.path != "uri.path" || entry < 0 || pathStruct.rules[entry].message != "a path other than /" {
		t.Errorf("uri.path's rules = %+v", pathStruct.rules)
	}
	for _, message := range []string{"a path other than /", "kept description"} {
		if !slices.ContainsFunc(k.invalid, func(c ruleEdit) bool { return c.what == "a breach of: "+message }) {
			t.Errorf("no invalid spec breaks %q", message)
		}
	}
	key := k.structs[0].fields[0].node.(*paramScalar)
	if key.validation.pattern != "^[a-z-]+$" {
		t.Errorf("the key's validation = %+v", key.validation)
	}
	pattern := func(c ruleEdit) bool { return c.what == "headers[0].name not matching its pattern" }
	if !slices.ContainsFunc(k.invalid, pattern) {
		t.Error("no invalid spec breaks the key's pattern")
	}

	source, err := emitRuleKindTest(k, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), `"test 1, of type headers"`) {
		t.Errorf("the table names no test of its type:\n%s", source)
	}
}

func TestScalarIntegerEnums(t *testing.T) {
	b := &paramBuilder{spec: &apiSpec{}}
	n, err := b.scalar("status_code", types.Typ[types.Int64], &schema{Type: typeInteger, Enum: []any{301.0, 308.0}},
		ruleFieldConfig{})
	if err != nil || n.goType != typeInt64 || !slices.Equal(n.enum, []string{"301", "308"}) {
		t.Errorf("an integer enum: %+v, %v", n, err)
	}
	_, err = b.scalar("status_code", types.Typ[types.Int64], &schema{Type: typeInteger, Enum: []any{"permanent"}},
		ruleFieldConfig{})
	if err == nil || !strings.Contains(err.Error(), `status_code is an integer whose enum holds "permanent"`) {
		t.Errorf("an integer whose enum holds a string: err = %v", err)
	}

	all, err := b.scalar("all", types.Typ[types.Bool], &schema{Type: "boolean", Enum: []any{true}}, ruleFieldConfig{})
	if err != nil || all.goType != typeBool || !slices.Equal(all.enum, []string{"true"}) {
		t.Fatalf("a bool enum: %+v, %v", all, err)
	}
	if value, ok := someValue(all); !ok || value != true {
		t.Errorf("a value of the bool enum: %v, %t", value, ok)
	}
	if _, err := checkScalar(all, false, "all"); err == nil ||
		!strings.Contains(err.Error(), "all is false, not one of true") {
		t.Errorf("false for a bool enum of true: err = %v", err)
	}
	if enumValue(all, "maybe") != "maybe" {
		t.Error("a bool enum value that is not a bool does not stay a string")
	}
	_, err = b.scalar("all", types.Typ[types.Bool], &schema{Type: "boolean", Enum: []any{"maybe"}}, ruleFieldConfig{})
	if err == nil || !strings.Contains(err.Error(), `all is a bool whose enum holds "maybe"`) {
		t.Errorf("a bool whose enum holds a string: err = %v", err)
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
			k, err := buildRuleKind(entry, testRuleLoader(t), spec)
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

func TestBuildRuleKindRefusesActions(t *testing.T) {
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
		edit       func(e *ruleKindConfig)
		want       string
	}{
		{"an action's rule type for another action", "TestCustomRule", func(e *ruleKindConfig) {
			e.Actions[0].Rule = "rulesets.SkipRuleParam"
		}, "actions[0] (block): rulesets.SkipRuleParam is the rule type for skip"},
		{"a rule type of another package", "TestCustomRule", func(e *ruleKindConfig) {
			e.Actions[2].Rule = "kv.NamespaceNewParams"
		}, "actions[2] (skip): kv.NamespaceNewParams is not in github.com/cloudflare/cloudflare-go/v7/rulesets"},
		{"a rule type the package lacks", "TestCustomRule", func(e *ruleKindConfig) {
			e.Actions[2].Rule = "rulesets.NopeRuleParam"
		}, "actions[2] (skip): github.com/cloudflare/cloudflare-go/v7/rulesets has no type NopeRuleParam"},
		{"an action v7 has no constant for", "TestCustomRule", func(e *ruleKindConfig) {
			e.Actions[1].Action = "nope"
		}, `actions[1] (nope): github.com/cloudflare/cloudflare-go/v7/rulesets has no RulesetNewParamsRulesAction ` +
			`constant "nope"`},
		{"an action whose schema the subset lacks", "TestCustomRule", func(e *ruleKindConfig) {
			e.Actions[1].Action = "compress_response"
		}, "the OpenAPI subset has no schema rulesets_CompressResponseRule"},
		{"an action without a rule type that takes parameters", "TestCustomRule", func(e *ruleKindConfig) {
			e.Actions[2].Rule = ""
		}, "actions[2] (skip): rulesets_SkipRule takes action parameters, which cloudflare-go v7 has no type for"},
		{"an action no sample or test has", "TestCustomRule", func(e *ruleKindConfig) {
			e.Tests = slices.DeleteFunc(slices.Clone(e.Tests), func(s map[string]any) bool { return s["action"] == "log" })
		}, "actions[3]: no sample or test has action log; add a test of it"},
		{"a kind of one action whose rule type takes no parameters", "TestSkipRule", func(e *ruleKindConfig) {
			e.Rule = "rulesets.LogRuleParam"
			e.Ignore = map[string]string{"logging": "x", "ratelimit": "x", "exposed_credential_check": "x"}
		}, "the rule type takes no action parameters, which a kind of one action maps"},
		{"the rule's own field set with an action that does not allow it", "TestCustomRule", func(e *ruleKindConfig) {
			e.Tests = slices.Clone(e.Tests)
			e.Tests[2] = map[string]any{"expression": "true", "action": "log", "logging": map[string]any{"enabled": true}}
		}, "tests[2]: breaks the rule \"logging is allowed only with action skip\""},
		{"an enum value the API description does not allow", "TestCustomRule", func(e *ruleKindConfig) {
			e.Fields = map[string]ruleFieldConfig{"phases[]": {Validation: &validationConfig{
				Enum: []string{"http_ratelimit", "nope"}, Reason: "r"}}}
		}, `fields.phases[].validation.enum: "nope" is none of the values the API description allows, ddos_l4`},
		{"an enum on an integer", "TestCustomRule", func(e *ruleKindConfig) {
			e.Fields = map[string]ruleFieldConfig{"response.status_code": {Validation: &validationConfig{
				Enum: []string{"403"}, Reason: "r"}}}
		}, "fields.response.status_code.validation.enum: only a string field takes an enum"},
		{"a validation of a list of objects' items", "TestManagedRule", func(e *ruleKindConfig) {
			e.Fields = map[string]ruleFieldConfig{"overrides.rules[]": {Validation: &validationConfig{
				MinLength: new(int64(1)), Reason: "r"}}}
		}, "fields.overrides.rules[].validation: only the items of a list of strings or integers are validated"},
		{"a sample outside an entry's enum", "TestCustomRule", func(e *ruleKindConfig) {
			e.Samples = slices.Clone(e.Samples)
			e.Samples[0] = map[string]any{"expression": "true", "action": "block", "response": map[string]any{
				"statusCode": 403, "contentType": "text/csv", "content": "x"}}
		}, `samples[0]: spec.response.contentType is "text/csv", not one of text/html`},
		{"a parameter set with an action that does not take it", "TestCustomRule", func(e *ruleKindConfig) {
			e.Tests = slices.Clone(e.Tests)
			e.Tests[0] = map[string]any{"expression": "true", "action": "challenge", "products": []any{"waf"}}
		}, "tests[0]: breaks the rule \"products is allowed only with action skip\""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry := entries[tc.kind]
			entry.Actions = slices.Clone(entry.Actions)
			entry.Ignore = cloneMap(entry.Ignore)
			tc.edit(&entry)
			_, err := buildRuleKind(entry, testRuleLoader(t), loadTestSpec(t))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestRuleKindRootRefusesFieldsTakenTwice(t *testing.T) {
	field := func(name string) *paramField {
		return &paramField{jsonName: name, node: &paramScalar{goType: typeString}}
	}
	tests := []struct {
		name string
		k    *ruleKind
		want string
	}{
		{"two actions taking a parameter by one name", &ruleKind{actionDoc: []string{"x"}, actions: []ruleKindAction{
			{name: "a", params: &paramStruct{fields: []*paramField{field("p")}}},
			{name: "b", params: &paramStruct{fields: []*paramField{field("p")}}},
		}}, "action b takes a parameter p, which the spec already holds"},
		{"a parameter named action", &ruleKind{actionDoc: []string{"x"}, actions: []ruleKindAction{
			{name: "a", params: &paramStruct{fields: []*paramField{field("action")}}}, {name: "b"},
		}}, "action a takes a parameter action, which the spec already holds"},
		{"the rule's field named as a parameter", &ruleKind{actions: []ruleKindAction{
			{name: "a", params: &paramStruct{fields: []*paramField{field("logging")}}},
		}, ruleFields: []*paramField{field("logging")}}, "the rule's field logging is also a parameter of an action"},
		{"a parameter named as a shared field", &ruleKind{actions: []ruleKindAction{
			{name: "a", params: &paramStruct{fields: []*paramField{field("priority")}}},
		}, ruleFields: []*paramField{field("logging")}}, "a field priority, which the spec already holds"},
		{"a parameter named zoneRef, without rule fields", &ruleKind{actions: []ruleKindAction{
			{name: "a", params: &paramStruct{fields: []*paramField{field("zoneRef")}}},
		}}, "a field zoneRef, which the spec already holds"},
	}
	for _, tc := range tests {
		if err := tc.k.root(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", tc.name, err, tc.want)
		}
	}
}

func TestFitsGenericRule(t *testing.T) {
	rule := loadTestRule(t, "rulesets.BlockRuleParam")
	generic, err := (&ruleKindBuilder{r: rule.r}).genericRuleFields()
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]sdkField{}
	for _, f := range rule.fields {
		fields[f.apiName] = f
	}
	for _, name := range []string{"logging", "ratelimit", "exposed_credential_check"} {
		if err := fitsGenericRule(generic, fields[name]); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	tests := []struct {
		name string
		f    sdkField
		want string
	}{
		{"a field the generic rule lacks", sdkField{apiName: "nope", typ: types.Typ[types.String]},
			"the rule's field nope is a string, which cloudflare-go v7's rule does not take as its nope"},
		{"a field of another type", sdkField{apiName: "logging", typ: types.Typ[types.String]},
			"the rule's field logging is a string"},
	}
	for _, tc := range tests {
		if err := fitsGenericRule(generic, tc.f); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", tc.name, err, tc.want)
		}
	}
}

func TestActionSchemaName(t *testing.T) {
	for action, want := range map[string]string{
		"challenge":    "rulesets_ChallengeRule",
		"js_challenge": "rulesets_JsChallengeRule",
		"log":          "rulesets_LogRule",
	} {
		if got := actionSchemaName(action); got != want {
			t.Errorf("actionSchemaName(%q) = %q, want %q", action, got, want)
		}
	}
}

func TestRuleFieldsRefusesAFieldTheGenericRuleDoesNotTake(t *testing.T) {
	rule := loadTestRule(t, "rulesets.SkipRuleParam")
	b := &ruleKindBuilder{r: rule.r, spec: loadTestSpec(t)}
	generic, err := b.genericRuleFields()
	if err != nil {
		t.Fatal(err)
	}
	ignore := map[string]string{"ratelimit": "x", "exposed_credential_check": "x"}
	newBuilder := func() *paramBuilder {
		return &paramBuilder{kind: "K", r: rule.r, spec: b.spec, ignore: ignore, used: map[string]bool{}}
	}
	fields, err := b.ruleFields(newBuilder(), []*sdkRule{nil, rule}, generic)
	if err != nil || len(fields) != 1 || fields[0].jsonName != "logging" {
		t.Fatalf("fields = %v, err = %v; want logging alone", fields, err)
	}
	generic["logging"] = types.Typ[types.String]
	_, err = b.ruleFields(newBuilder(), []*sdkRule{rule}, generic)
	if want := "the rule's field logging is a"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want one containing %q", err, want)
	}
}

func TestSetCondition(t *testing.T) {
	fields := []*paramField{
		{goName: "Phases", node: &paramList{items: &paramScalar{goType: typeString}}},
		{goName: "Ruleset", node: &paramScalar{goType: typeString, enum: []string{"current"}}},
		{goName: "Count", node: &paramScalar{goType: typeInt64}},
		{goName: "Response", node: &paramStruct{goName: "R"}},
	}
	want := `len(in.Phases) > 0 || in.Ruleset != "" || in.Count != nil || in.Response != nil`
	if got := setCondition("in.", fields); got != want {
		t.Errorf("setCondition = %s, want %s", got, want)
	}
}

func TestSpecValidationTakesAnEntryEnum(t *testing.T) {
	b := &kindBuilder{}
	out := &field{goType: typeString}
	f := sdkField{apiName: "location_hint", enum: []string{"wnam", "enam", "weur"}}
	err := b.specValidation(out, f, fieldSpec{}, &validationConfig{Enum: []string{"wnam", "weur"}, Reason: "r"})
	if err != nil || !slices.Equal(out.enum, []string{"wnam", "weur"}) || !slices.Equal(out.excluded, []string{"enam"}) {
		t.Errorf("enum = %v, excluded = %v, err = %v; want wnam and weur, enam excluded", out.enum, out.excluded, err)
	}
	out.jsonName = "locationHint"
	if !slices.ContainsFunc(invalidValues(*out), func(e specEdit) bool {
		return e.value == "enam" && e.what == "locationHint enam, which the entry's enum leaves out"
	}) {
		t.Errorf("invalidValues = %+v, want enam refused", invalidValues(*out))
	}
	err = b.specValidation(out, f, fieldSpec{}, &validationConfig{Enum: []string{"oc"}, Reason: "r"})
	if want := `fields.location_hint.validation.enum: "oc" is none of the values`; err == nil ||
		!strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want one containing %q", err, want)
	}
	err = b.specValidation(&field{goType: "*" + typeInt64}, sdkField{apiName: "size"}, fieldSpec{},
		&validationConfig{Enum: []string{"1"}, Reason: "r"})
	if want := "fields.size.validation.enum: only a string field takes an enum"; err == nil ||
		!strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want one containing %q", err, want)
	}
}
