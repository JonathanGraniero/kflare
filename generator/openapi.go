/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// apiSpec is the part of Cloudflare's OpenAPI spec kflare-gen reads: the
// committed subset that sync-spec writes. The SDK decides which fields a kind
// has; the spec only adds what the SDK's types leave out: lengths, patterns,
// ranges, defaults, integers, and Stainless's hints.
type apiSpec struct {
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas       map[string]*schema      `json:"schemas"`
		RequestBodies map[string]*requestBody `json:"requestBodies"`
		Responses     map[string]*response    `json:"responses"`
		Parameters    map[string]*parameter   `json:"parameters"`
	} `json:"components"`

	// byRoute indexes the paths by their normalized route.
	byRoute map[string]string
}

// operation is one method of a path.
type operation struct {
	Parameters  []*parameter         `json:"parameters"`
	RequestBody *requestBody         `json:"requestBody"`
	Responses   map[string]*response `json:"responses"`
}

// parameter is a path, query or header parameter of an operation.
type parameter struct {
	Ref    string  `json:"$ref"`
	Name   string  `json:"name"`
	In     string  `json:"in"`
	Schema *schema `json:"schema"`
}

type requestBody struct {
	Ref     string               `json:"$ref"`
	Content map[string]mediaType `json:"content"`
}

type response struct {
	Ref     string               `json:"$ref"`
	Content map[string]mediaType `json:"content"`
}

type mediaType struct {
	Schema *schema `json:"schema"`
}

// schema is a JSON schema, as far as kflare-gen reads one.
type schema struct {
	Ref              string             `json:"$ref"`
	Type             string             `json:"type"`
	Properties       map[string]*schema `json:"properties"`
	AllOf            []*schema          `json:"allOf"`
	Enum             []any              `json:"enum"`
	Default          any                `json:"default"`
	Example          any                `json:"example"`
	MinLength        *int64             `json:"minLength"`
	MaxLength        *int64             `json:"maxLength"`
	Pattern          string             `json:"pattern"`
	Minimum          *float64           `json:"minimum"`
	Maximum          *float64           `json:"maximum"`
	ExclusiveMinimum bool               `json:"exclusiveMinimum"`
	ExclusiveMaximum bool               `json:"exclusiveMaximum"`
	// Sensitive is Stainless's mark for a secret, such as a password.
	Sensitive bool `json:"x-sensitive"`

	// The keywords of nested objects, lists and unions, which a rule kind's
	// action parameters use.
	OneOf    []*schema `json:"oneOf"`
	AnyOf    []*schema `json:"anyOf"`
	Required []string  `json:"required"`
	Items    *schema   `json:"items"`
	// AdditionalProperties is a schema, or a bool: see valueSchema.
	AdditionalProperties json.RawMessage `json:"additionalProperties"`
	MinProperties        *int64          `json:"minProperties"`
	MaxProperties        *int64          `json:"maxProperties"`
	MinItems             *int64          `json:"minItems"`
	MaxItems             *int64          `json:"maxItems"`
	UniqueItems          bool            `json:"uniqueItems"`
	ReadOnly             bool            `json:"readOnly"`
}

// valueSchema returns the schema of the values of a map: the schema
// additionalProperties holds, or nil when it is absent or a bool.
func (sch *schema) valueSchema() (*schema, error) {
	if len(sch.AdditionalProperties) == 0 || sch.AdditionalProperties[0] != '{' {
		return nil, nil
	}
	value := &schema{}
	if err := json.Unmarshal(sch.AdditionalProperties, value); err != nil {
		return nil, fmt.Errorf("additionalProperties: %w", err)
	}
	return value, nil
}

// loadSpec reads the spec at path.
func loadSpec(path string) (*apiSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	spec, err := parseSpec(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return spec, nil
}

// parseSpec reads a spec.
func parseSpec(data []byte) (*apiSpec, error) {
	spec := &apiSpec{}
	if err := json.Unmarshal(data, spec); err != nil {
		return nil, err
	}
	spec.byRoute = map[string]string{}
	for path := range spec.Paths {
		spec.byRoute[normalizeRoute(path)] = path
	}
	return spec, nil
}

// pathParameter matches a path parameter, such as {account_id}.
var pathParameter = regexp.MustCompile(`\{[^}]*\}`)

// normalizeRoute drops the names of a route's path parameters, which the SDK's
// api.md and the spec do not always spell alike.
func normalizeRoute(path string) string {
	return pathParameter.ReplaceAllString(path, "{}")
}

// operation returns the operation for an SDK call's route.
func (s *apiSpec) operation(r route) (*operation, error) {
	path, ok := s.byRoute[normalizeRoute(r.path)]
	if !ok {
		return nil, fmt.Errorf("the OpenAPI subset has no %s %s: run make sync-spec", r.verb, r.path)
	}
	raw, ok := s.Paths[path][r.verb]
	if !ok {
		return nil, fmt.Errorf("the OpenAPI subset has no %s %s: run make sync-spec", r.verb, r.path)
	}
	op := &operation{}
	if err := json.Unmarshal(raw, op); err != nil {
		return nil, fmt.Errorf("%s %s: %w", r.verb, r.path, err)
	}
	return op, nil
}

// pageSize returns the most objects a page of a list may hold: the maximum of
// the per_page query parameter of the list's operation.
func (s *apiSpec) pageSize(list route) (int64, error) {
	op, err := s.operation(list)
	if err != nil {
		return 0, err
	}
	for _, p := range op.Parameters {
		if p.Ref != "" {
			if p, err = component(s.Components.Parameters, p.Ref, "parameters"); err != nil {
				return 0, err
			}
		}
		if p.Name != "per_page" || p.In != inQuery {
			continue
		}
		fs, err := s.fieldSpec(p.Schema)
		if err != nil {
			return 0, fmt.Errorf("%s's per_page: %w", list, err)
		}
		maximum := fs.validation.maximum
		if maximum == nil || *maximum < 1 || *maximum != float64(int64(*maximum)) || fs.validation.exclusiveMaximum {
			return 0, fmt.Errorf("%s's per_page has no maximum that is a whole number of objects", list)
		}
		return int64(*maximum), nil
	}
	return 0, fmt.Errorf("%s takes no per_page query parameter", list)
}

// requestProperties returns the properties of an operation's JSON request
// body.
func (s *apiSpec) requestProperties(op *operation) (map[string]*schema, error) {
	body := op.RequestBody
	if body != nil && body.Ref != "" {
		var err error
		if body, err = component(s.Components.RequestBodies, body.Ref, "requestBodies"); err != nil {
			return nil, err
		}
	}
	if body == nil {
		return nil, nil
	}
	media, ok := body.Content["application/json"]
	if !ok {
		return nil, nil
	}
	return s.properties(media.Schema)
}

// resultProperties returns the properties of the result an operation's
// successful JSON response wraps.
func (s *apiSpec) resultProperties(op *operation) (map[string]*schema, error) {
	resp, ok := op.Responses["200"]
	if !ok {
		resp, ok = op.Responses["201"]
	}
	if !ok {
		return nil, fmt.Errorf("no 200 or 201 response")
	}
	if resp.Ref != "" {
		var err error
		if resp, err = component(s.Components.Responses, resp.Ref, "responses"); err != nil {
			return nil, err
		}
	}
	media, ok := resp.Content["application/json"]
	if !ok {
		return nil, fmt.Errorf("no JSON response")
	}
	envelope, err := s.properties(media.Schema)
	if err != nil {
		return nil, err
	}
	result, ok := envelope["result"]
	if !ok {
		return nil, fmt.Errorf("the response has no result")
	}
	return s.properties(result)
}

// properties returns a schema's properties, including those of the schemas it
// combines with allOf. When two of them declare the same property, the one
// with properties of its own wins: Cloudflare's response envelopes declare a
// generic result next to the specific one.
func (s *apiSpec) properties(sch *schema) (map[string]*schema, error) {
	sch, err := s.resolve(sch)
	if err != nil || sch == nil {
		return nil, err
	}
	props := map[string]*schema{}
	merge := func(name string, p *schema) error {
		existing, ok := props[name]
		if !ok {
			props[name] = p
			return nil
		}
		existingProps, err := s.properties(existing)
		if err != nil {
			return err
		}
		newProps, err := s.properties(p)
		if err != nil {
			return err
		}
		if len(newProps) > len(existingProps) {
			props[name] = p
		}
		return nil
	}
	for _, part := range sch.AllOf {
		partProps, err := s.properties(part)
		if err != nil {
			return nil, err
		}
		for name, p := range partProps {
			if err := merge(name, p); err != nil {
				return nil, err
			}
		}
	}
	for name, p := range sch.Properties {
		if err := merge(name, p); err != nil {
			return nil, err
		}
	}
	return props, nil
}

// resolve follows a schema's $ref.
func (s *apiSpec) resolve(sch *schema) (*schema, error) {
	for sch != nil && sch.Ref != "" {
		next, err := component(s.Components.Schemas, sch.Ref, "schemas")
		if err != nil {
			return nil, err
		}
		sch = next
	}
	return sch, nil
}

// component returns the component a $ref names in one of the spec's
// component maps.
func component[T any](components map[string]*T, ref, kind string) (*T, error) {
	name, ok := strings.CutPrefix(ref, "#/components/"+kind+"/")
	if !ok {
		return nil, fmt.Errorf("$ref %s is not a %s component", ref, kind)
	}
	c, ok := components[name]
	if !ok {
		return nil, fmt.Errorf("$ref %s is not in the OpenAPI subset: run make sync-spec", ref)
	}
	return c, nil
}

// fieldSpec is what the spec says about one field: its JSON type, its
// validation and its default, Stainless's hints included.
type fieldSpec struct {
	typ        string
	validation validation
	enum       []string
	// def is the default as a marker value, "" when there is none.
	def       string
	sensitive bool
	// example is the schema's example of a value, nil when it gives none.
	example any
}

// fieldSpec reads a property's schema, following $refs and allOf. A keyword
// the property sets itself wins over one it combines in.
func (s *apiSpec) fieldSpec(prop *schema) (fieldSpec, error) {
	var out fieldSpec
	var visit func(*schema) error
	visit = func(sch *schema) error {
		sch, err := s.resolve(sch)
		if err != nil || sch == nil {
			return err
		}
		if out.typ == "" {
			out.typ = sch.Type
		}
		v := &out.validation
		if v.minLength == nil {
			v.minLength = sch.MinLength
		}
		if v.maxLength == nil {
			v.maxLength = sch.MaxLength
		}
		if v.pattern == "" {
			v.pattern = sch.Pattern
		}
		if v.minimum == nil && sch.Minimum != nil {
			v.minimum, v.exclusiveMinimum = sch.Minimum, sch.ExclusiveMinimum
		}
		if v.maximum == nil && sch.Maximum != nil {
			v.maximum, v.exclusiveMaximum = sch.Maximum, sch.ExclusiveMaximum
		}
		if out.enum == nil && len(sch.Enum) > 0 {
			for _, value := range sch.Enum {
				switch v := value.(type) {
				case string:
					out.enum = append(out.enum, v)
				case float64:
					if v != math.Trunc(v) {
						return fmt.Errorf("enum value %v is not a string or a whole number", value)
					}
					out.enum = append(out.enum, strconv.FormatInt(int64(v), 10))
				default:
					return fmt.Errorf("enum value %v is not a string or a whole number", value)
				}
			}
		}
		if out.def == "" && sch.Default != nil {
			literal, err := defaultLiteral(sch.Default)
			if err != nil {
				return err
			}
			out.def = literal
		}
		out.sensitive = out.sensitive || sch.Sensitive
		if out.example == nil {
			out.example = sch.Example
		}
		for _, part := range sch.AllOf {
			if err := visit(part); err != nil {
				return err
			}
		}
		return nil
	}
	return out, visit(prop)
}

// defaultLiteral writes a default as the value of a +kubebuilder:default
// marker. Strings are always quoted, so that one such as "true" stays a
// string.
func defaultLiteral(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return fmt.Sprintf("%q", v), nil
	case bool:
		return fmt.Sprintf("%t", v), nil
	case float64:
		if v != float64(int64(v)) {
			return "", fmt.Errorf("default %v is not an integer", v)
		}
		return fmt.Sprintf("%d", int64(v)), nil
	}
	return "", fmt.Errorf("default %v is a %T, which kflare-gen cannot map yet", value, value)
}

// required returns the properties a schema requires, including those of the
// schemas it combines with allOf.
func (s *apiSpec) required(sch *schema) (map[string]bool, error) {
	out := map[string]bool{}
	var visit func(*schema) error
	visit = func(sch *schema) error {
		sch, err := s.resolve(sch)
		if err != nil || sch == nil {
			return err
		}
		for _, name := range sch.Required {
			out[name] = true
		}
		for _, part := range sch.AllOf {
			if err := visit(part); err != nil {
				return err
			}
		}
		return nil
	}
	return out, visit(sch)
}

// How a union's schema combines its variants: one of them exactly, or any
// number of them.
const (
	combinatorOneOf = "oneOf"
	combinatorAnyOf = "anyOf"
)

// alternatives returns how a union's schema combines its variants, oneOf or
// anyOf, and the variants, resolved. The combinator may sit in one of the
// schemas the union combines with allOf; "" when there is none.
func (s *apiSpec) alternatives(sch *schema) (string, []*schema, error) {
	if sch == nil {
		return "", nil, nil
	}
	combinator, parts := "", []*schema(nil)
	switch {
	case len(sch.OneOf) > 0 && len(sch.AnyOf) > 0:
		return "", nil, fmt.Errorf("the schema has both oneOf and anyOf")
	case len(sch.OneOf) > 0:
		combinator, parts = combinatorOneOf, sch.OneOf
	case len(sch.AnyOf) > 0:
		combinator, parts = combinatorAnyOf, sch.AnyOf
	}
	if combinator == "" {
		for _, part := range sch.AllOf {
			resolved, err := s.resolve(part)
			if err != nil {
				return "", nil, err
			}
			c, alts, err := s.alternatives(resolved)
			if err != nil {
				return "", nil, err
			}
			if c != "" {
				if combinator != "" {
					return "", nil, fmt.Errorf("the schema combines two unions")
				}
				combinator, parts = c, alts
			}
		}
		return combinator, parts, nil
	}
	alternatives := make([]*schema, len(parts))
	for i, part := range parts {
		resolved, err := s.resolve(part)
		if err != nil {
			return "", nil, err
		}
		alternatives[i] = resolved
	}
	return combinator, alternatives, nil
}
