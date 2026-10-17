/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

// parseTestSpec parses a spec, failing the test if it does not parse.
func parseTestSpec(t *testing.T, data string) *apiSpec {
	t.Helper()
	spec, err := parseSpec([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// propertyNames returns the sorted names of props.
func propertyNames(props map[string]*schema) []string {
	return slices.Sorted(maps.Keys(props))
}

func TestParseSpecRefusesWhatIsNotJSON(t *testing.T) {
	if _, err := parseSpec([]byte("{")); err == nil {
		t.Error("parseSpec: err = nil")
	}
	if _, err := loadSpec("missing.json"); err == nil {
		t.Error("loadSpec of a missing file: err = nil")
	}
	if _, err := loadSpec("generator.yaml"); err == nil || !strings.Contains(err.Error(), "generator.yaml") {
		t.Errorf("loadSpec of YAML: err = %v, want one naming the file", err)
	}
}

func TestOperationMatchesRoutesWhateverTheParameterNames(t *testing.T) {
	spec := parseTestSpec(t, `{"paths": {"/accounts/{account_identifier}/things/{id}": {"get": {}}}}`)
	if _, err := spec.operation(route{verb: "get", path: "/accounts/{account_id}/things/{thing_id}"}); err != nil {
		t.Errorf("operation: %v", err)
	}
	for _, r := range []route{
		{verb: "get", path: "/accounts/{account_id}/others/{id}"},
		{verb: "post", path: "/accounts/{account_id}/things/{id}"},
	} {
		if _, err := spec.operation(r); err == nil || !strings.Contains(err.Error(), "run make sync-spec") {
			t.Errorf("operation(%s): err = %v", r, err)
		}
	}
	bad := parseTestSpec(t, `{"paths": {"/things": {"get": {"responses": []}}}}`)
	if _, err := bad.operation(route{verb: "get", path: "/things"}); err == nil {
		t.Error("operation of a malformed operation: err = nil")
	}
}

func TestRequestProperties(t *testing.T) {
	spec := parseTestSpec(t, `{
		"paths": {
			"/a": {"post": {"requestBody": {"$ref": "#/components/requestBodies/thing"}}},
			"/b": {"post": {}},
			"/c": {"post": {"requestBody": {"content": {"text/plain": {}}}}},
			"/d": {"post": {"requestBody": {"$ref": "#/components/requestBodies/missing"}}}
		},
		"components": {
			"requestBodies": {"thing": {"content": {"application/json": {"schema": {
				"allOf": [{"$ref": "#/components/schemas/named"}, {"properties": {"size": {"type": "integer"}}}]}}}}},
			"schemas": {"named": {"properties": {"name": {"type": "string"}}}}
		}}`)
	props := func(path string) (map[string]*schema, error) {
		op, err := spec.operation(route{verb: "post", path: path})
		if err != nil {
			t.Fatal(err)
		}
		return spec.requestProperties(op)
	}
	if got, err := props("/a"); err != nil || !slices.Equal(propertyNames(got), []string{"name", "size"}) {
		t.Errorf("a request body's properties, through its $ref and allOf = %v, %v", propertyNames(got), err)
	}
	for _, path := range []string{"/b", "/c"} {
		if got, err := props(path); err != nil || got != nil {
			t.Errorf("%s, without a JSON request body = %v, %v", path, got, err)
		}
	}
	if _, err := props("/d"); err == nil || !strings.Contains(err.Error(), "not in the OpenAPI subset") {
		t.Errorf("a dangling request body: err = %v", err)
	}
}

func TestResultProperties(t *testing.T) {
	spec := parseTestSpec(t, `{
		"paths": {
			"/generic-first": {"get": {"responses": {"200": {"content": {"application/json": {"schema": {
				"allOf": [{"$ref": "#/components/schemas/envelope"}, {"properties": {"result": {"$ref": "#/components/schemas/thing"}}}]}}}}}}},
			"/specific-first": {"get": {"responses": {"201": {"$ref": "#/components/responses/thing"}}}},
			"/no-success": {"get": {"responses": {"404": {}}}},
			"/no-json": {"get": {"responses": {"200": {"content": {"text/plain": {}}}}}},
			"/no-result": {"get": {"responses": {"200": {"content": {"application/json": {"schema": {"properties": {}}}}}}}},
			"/bad-ref": {"get": {"responses": {"200": {"$ref": "#/components/schemas/thing"}}}}
		},
		"components": {
			"responses": {"thing": {"content": {"application/json": {"schema": {
				"allOf": [{"properties": {"result": {"$ref": "#/components/schemas/thing"}}}, {"$ref": "#/components/schemas/envelope"}]}}}}},
			"schemas": {
				"envelope": {"properties": {"success": {"type": "boolean"}, "result": {"type": "object"}}},
				"thing": {"properties": {"id": {"type": "string"}, "name": {"type": "string"}}}
			}
		}}`)
	result := func(path string) (map[string]*schema, error) {
		op, err := spec.operation(route{verb: "get", path: path})
		if err != nil {
			t.Fatal(err)
		}
		return spec.resultProperties(op)
	}
	for _, path := range []string{"/generic-first", "/specific-first"} {
		if got, err := result(path); err != nil || !slices.Equal(propertyNames(got), []string{"id", "name"}) {
			t.Errorf("%s: the specific result wins over the envelope's generic one: %v, %v", path, propertyNames(got), err)
		}
	}
	for path, want := range map[string]string{
		"/no-success": "no 200 or 201 response",
		"/no-json":    "no JSON response",
		"/no-result":  "the response has no result",
		"/bad-ref":    "is not a responses component",
	} {
		if _, err := result(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want one containing %q", path, err, want)
		}
	}
}

func TestFieldSpec(t *testing.T) {
	spec := parseTestSpec(t, `{"components": {"schemas": {
		"base": {"type": "string", "maxLength": 50, "pattern": "^[a-z]+$", "x-sensitive": true},
		"limits": {"type": "integer", "minimum": 1, "maximum": 9, "exclusiveMaximum": true, "default": 5}
	}}}`)
	fs, err := spec.fieldSpec(&schema{MaxLength: new(int64(10)), AllOf: []*schema{{Ref: "#/components/schemas/base"}}})
	if err != nil {
		t.Fatal(err)
	}
	if fs.typ != "string" || *fs.validation.maxLength != 10 || fs.validation.pattern != "^[a-z]+$" || !fs.sensitive {
		t.Errorf("a property's own keyword wins over what it combines in: %+v", fs)
	}
	fs, err = spec.fieldSpec(&schema{Ref: "#/components/schemas/limits"})
	if err != nil {
		t.Fatal(err)
	}
	v := fs.validation
	if *v.minimum != 1 || *v.maximum != 9 || !v.exclusiveMaximum || v.exclusiveMinimum || fs.def != "5" {
		t.Errorf("the range and default through a $ref: %+v", fs)
	}
	if fs, err := spec.fieldSpec(&schema{Enum: []any{"a", "b"}}); err != nil || !slices.Equal(fs.enum, []string{"a", "b"}) {
		t.Errorf("an enum: %+v, %v", fs, err)
	}
	if fs, err := spec.fieldSpec(&schema{Enum: []any{301.0, 308.0}}); err != nil ||
		!slices.Equal(fs.enum, []string{"301", "308"}) {
		t.Errorf("an enum of whole numbers: %+v, %v", fs, err)
	}
	if fs, err := spec.fieldSpec(&schema{Enum: []any{true}}); err != nil || !slices.Equal(fs.enum, []string{"true"}) {
		t.Errorf("an enum of bools: %+v, %v", fs, err)
	}

	for name, sch := range map[string]*schema{
		"an enum of fractions":      {Enum: []any{1.5}},
		"an enum of objects":        {Enum: []any{map[string]any{}}},
		"a default of another type": {Default: []any{}},
		"a dangling $ref":           {Ref: "#/components/schemas/missing"},
		"a $ref elsewhere":          {Ref: "#/paths/x"},
	} {
		if _, err := spec.fieldSpec(sch); err == nil {
			t.Errorf("fieldSpec of %s: err = nil", name)
		}
	}
}

func TestDefaultLiteral(t *testing.T) {
	for value, want := range map[any]string{"Standard": `"Standard"`, "true": `"true"`, true: "true", 42.0: "42"} {
		if got, err := defaultLiteral(value); err != nil || got != want {
			t.Errorf("defaultLiteral(%v) = %s, %v, want %s", value, got, err, want)
		}
	}
	for _, value := range []any{4.2, map[string]any{}} {
		if _, err := defaultLiteral(value); err == nil {
			t.Errorf("defaultLiteral(%v): err = nil", value)
		}
	}
}

func TestPropertiesRefusesWhatItCannotResolve(t *testing.T) {
	spec := parseTestSpec(t, `{"components": {"schemas": {"thing": {"properties": {"id": {"type": "string"}}}}}}`)
	for name, sch := range map[string]*schema{
		"a dangling $ref":       {Ref: "#/components/schemas/missing"},
		"a dangling allOf part": {AllOf: []*schema{{Ref: "#/components/schemas/missing"}}},
		"a property declared twice, once dangling": {AllOf: []*schema{
			{Properties: map[string]*schema{"result": {Ref: "#/components/schemas/thing"}}},
			{Properties: map[string]*schema{"result": {Ref: "#/components/schemas/missing"}}},
		}},
		"a property declared twice, first dangling": {AllOf: []*schema{
			{Properties: map[string]*schema{"result": {Ref: "#/components/schemas/missing"}}},
			{Properties: map[string]*schema{"result": {Ref: "#/components/schemas/thing"}}},
		}},
		"a declared property, after an allOf, dangling": {
			AllOf:      []*schema{{Properties: map[string]*schema{"result": {Ref: "#/components/schemas/thing"}}}},
			Properties: map[string]*schema{"result": {Ref: "#/components/schemas/missing"}},
		},
	} {
		if _, err := spec.properties(sch); err == nil {
			t.Errorf("properties of %s: err = nil", name)
		}
	}
	op := &operation{Responses: map[string]*response{"200": {Content: map[string]mediaType{
		"application/json": {Schema: &schema{Ref: "#/components/schemas/missing"}}}}}}
	if _, err := spec.resultProperties(op); err == nil {
		t.Error("resultProperties of a dangling envelope: err = nil")
	}
}

func TestPageSize(t *testing.T) {
	list := route{verb: "get", path: "/accounts/{account_id}/things"}
	if got, err := loadTestSpec(t).pageSize(route{verb: "get", path: "/accounts/{id}/storage/kv/namespaces"}); err != nil ||
		got != 1000 {
		t.Errorf("the KV namespaces' page size = %d, %v, want 1000", got, err)
	}
	spec := parseTestSpec(t, `{"paths": {"/accounts/{account_id}/things": {"get": {"parameters": [
		{"name": "page", "in": "query", "schema": {"type": "number", "maximum": 5}},
		{"$ref": "#/components/parameters/per_page"}]}}},
		"components": {"parameters": {"per_page": {"name": "per_page", "in": "query",
			"schema": {"type": "number", "maximum": 50}}}}}`)
	if got, err := spec.pageSize(list); err != nil || got != 50 {
		t.Errorf("a per_page through a $ref: page size = %d, %v, want 50", got, err)
	}
	for name, tc := range map[string]struct{ params, want string }{
		"no per_page":           {`[{"name": "per_page", "in": "header"}]`, "takes no per_page query parameter"},
		"no maximum":            {`[{"name": "per_page", "in": "query", "schema": {"type": "number"}}]`, "no maximum"},
		"a fraction":            {`[{"name": "per_page", "in": "query", "schema": {"maximum": 2.5}}]`, "no maximum"},
		"an exclusive maximum":  {`[{"name": "per_page", "in": "query", "schema": {"maximum": 5, "exclusiveMaximum": true}}]`, "no maximum"},
		"a dangling $ref":       {`[{"$ref": "#/components/parameters/missing"}]`, "not in the OpenAPI subset"},
		"a schema that dangles": {`[{"name": "per_page", "in": "query", "schema": {"$ref": "#/components/schemas/x"}}]`, "per_page"},
	} {
		spec := parseTestSpec(t, `{"paths": {"/accounts/{account_id}/things": {"get": {"parameters": `+tc.params+`}}}}`)
		if _, err := spec.pageSize(list); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, tc.want)
		}
	}
	if _, err := spec.pageSize(route{verb: "get", path: "/elsewhere"}); err == nil {
		t.Error("the page size of an operation the spec lacks: err = nil")
	}
}

func TestValueSchema(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      bool
	}{{"absent", "", false}, {"a bool", "true", false}, {"a schema", `{"type":"string"}`, true}} {
		got, err := (&schema{AdditionalProperties: json.RawMessage(tc.raw)}).valueSchema()
		if err != nil || (got != nil) != tc.want {
			t.Errorf("%s: valueSchema = %v, %v", tc.name, got, err)
		}
	}
	if _, err := (&schema{AdditionalProperties: json.RawMessage(`{"type": 1}`)}).valueSchema(); err == nil {
		t.Error("a schema that does not decode: err = nil")
	}
}

func TestRequiredAndAlternatives(t *testing.T) {
	spec, err := parseSpec([]byte(`{"components": {"schemas": {
		"a": {"required": ["x"], "allOf": [{"$ref": "#/components/schemas/b"}]},
		"b": {"required": ["y"]},
		"union": {"allOf": [{"anyOf": [{"required": ["x"]}, {"$ref": "#/components/schemas/b"}]}, {"type": "object"}]},
		"both": {"oneOf": [{}], "anyOf": [{}]},
		"two": {"allOf": [{"oneOf": [{}]}, {"anyOf": [{}]}]},
		"broken": {"allOf": [{"$ref": "#/components/schemas/missing"}]}
	}}}`))
	if err != nil {
		t.Fatal(err)
	}
	ref := func(name string) *schema { return &schema{Ref: "#/components/schemas/" + name} }
	required, err := spec.required(ref("a"))
	if err != nil || !required["x"] || !required["y"] || len(required) != 2 {
		t.Errorf("required = %v, %v", required, err)
	}
	if _, err := spec.required(ref("broken")); err == nil {
		t.Error("required through a missing $ref: err = nil")
	}
	union, _ := spec.resolve(ref("union"))
	combinator, alternatives, err := spec.alternatives(union)
	if err != nil || combinator != "anyOf" || len(alternatives) != 2 || alternatives[1].Required[0] != "y" {
		t.Errorf("alternatives = %s, %v, %v", combinator, alternatives, err)
	}
	if combinator, _, err := spec.alternatives(nil); err != nil || combinator != "" {
		t.Errorf("alternatives of nothing = %s, %v", combinator, err)
	}
	for _, name := range []string{"both", "two", "broken"} {
		s, _ := spec.resolve(ref(name))
		if _, _, err := spec.alternatives(s); err == nil {
			t.Errorf("%s: err = nil", name)
		}
	}
}
