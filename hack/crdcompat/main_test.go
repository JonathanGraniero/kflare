/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// repoRoot is the repository's root, from this package's directory.
const repoRoot = "../.."

// specSchema returns a version's schema whose spec field has the schema s,
// written in YAML.
func specSchema(t *testing.T, s string) *apiextensionsv1.JSONSchemaProps {
	t.Helper()
	var spec apiextensionsv1.JSONSchemaProps
	if err := yaml.Unmarshal([]byte(s), &spec); err != nil {
		t.Fatalf("schema %s: %v", s, err)
	}
	return &apiextensionsv1.JSONSchemaProps{
		Type:       "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{"spec": spec},
	}
}

// compare returns what compareSchema reports from before to after, the spec
// field's schemas in YAML.
func compare(t *testing.T, before, after string) []string {
	t.Helper()
	var got []string
	compareSchema(specSchema(t, before), specSchema(t, after), "", func(field, what string) {
		got = append(got, field+": "+what)
	})
	return got
}

func TestCompareSchema(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after string
		want          []string
	}{
		{
			name: "an unchanged schema",
			before: `{type: object, required: [a], properties: {a: {type: string, maxLength: 3, enum: [x, q], ` +
				`pattern: "^[a-z]$", format: hostname}, b: {type: integer, minimum: 1, exclusiveMinimum: true, ` +
				`maximum: 9, exclusiveMaximum: true, multipleOf: 2}, c: {type: array, uniqueItems: true, ` +
				`items: {type: string}}, d: {type: string, nullable: true}, ` +
				`e: {type: object, x-kubernetes-preserve-unknown-fields: true}}}`,
			after: `{type: object, required: [a], properties: {a: {type: string, maxLength: 3, enum: [x, q], ` +
				`pattern: "^[a-z]$", format: hostname}, b: {type: integer, minimum: 1, exclusiveMinimum: true, ` +
				`maximum: 9, exclusiveMaximum: true, multipleOf: 2}, c: {type: array, uniqueItems: true, ` +
				`items: {type: string}}, d: {type: string, nullable: true}, ` +
				`e: {type: object, x-kubernetes-preserve-unknown-fields: true}}}`,
		},
		{
			name:   "a new optional field",
			before: `{type: object, properties: {a: {type: string}}}`,
			after:  `{type: object, properties: {a: {type: string}, b: {type: integer, minimum: 1}}}`,
		},
		{
			name:   "a removed field",
			before: `{type: object, properties: {a: {type: string}, b: {type: string}}}`,
			after:  `{type: object, properties: {a: {type: string}}}`,
			want:   []string{".spec.b: it is removed"},
		},
		{
			name:   "a field removed from an object that keeps unknown fields",
			before: `{type: object, properties: {a: {type: string}, b: {type: string}}}`,
			after:  `{type: object, x-kubernetes-preserve-unknown-fields: true, properties: {a: {type: string}}}`,
		},
		{
			name: "a removed field under a field and a list",
			before: `{type: object, properties: {a: {type: array, items: {type: object, properties: {b: {type: object, ` +
				`properties: {c: {type: string}}}}}}}}`,
			after: `{type: object, properties: {a: {type: array, items: {type: object, properties: {b: {type: object}}}}}}`,
			want:  []string{".spec.a[*].b.c: it is removed"},
		},
		{
			name:   "a new type",
			before: `{type: object, properties: {a: {type: string, maxLength: 3}}}`,
			after:  `{type: object, properties: {a: {type: integer, maximum: 3}}}`,
			want:   []string{".spec.a: its type changes from string to integer"},
		},
		{
			name:   "a type for a field of any type",
			before: `{type: object, properties: {a: {x-kubernetes-preserve-unknown-fields: true}}}`,
			after:  `{type: object, properties: {a: {type: object}}}`,
			want:   []string{".spec.a: its type changes from any type to object"},
		},
		{
			name:   "an int-or-string field that takes only strings",
			before: `{type: object, properties: {a: {x-kubernetes-int-or-string: true}}}`,
			after:  `{type: object, properties: {a: {type: string}}}`,
			want:   []string{".spec.a: its type changes from int-or-string to string"},
		},
		{
			name:   "a string field that takes integers too",
			before: `{type: object, properties: {a: {type: string}}}`,
			after:  `{type: object, properties: {a: {x-kubernetes-int-or-string: true}}}`,
		},
		{
			name:   "an enum on a field that had none",
			before: `{type: object, properties: {a: {type: string}}}`,
			after:  `{type: object, properties: {a: {type: string, enum: [x, q]}}}`,
			want:   []string{`.spec.a: its values are limited to "x", "q"`},
		},
		{
			name:   "an enum that loses values",
			before: `{type: object, properties: {a: {type: string, enum: [x, q, z]}}}`,
			after:  `{type: object, properties: {a: {type: string, enum: [q]}}}`,
			want:   []string{`.spec.a: its enum loses "x", "z"`},
		},
		{
			name:   "an enum that gains a value, and one that goes",
			before: `{type: object, properties: {a: {type: string, enum: [x]}, b: {type: integer, enum: [1, 2]}}}`,
			after:  `{type: object, properties: {a: {type: string, enum: [x, q]}, b: {type: integer}}}`,
		},
		{
			name:   "a minimum added, and one raised",
			before: `{type: object, properties: {a: {type: integer}, b: {type: number, minimum: 1}}}`,
			after:  `{type: object, properties: {a: {type: integer, minimum: 0}, b: {type: number, minimum: 1.5}}}`,
			want:   []string{".spec.a: minimum 0 is added", ".spec.b: minimum changes from 1 to 1.5"},
		},
		{
			name:   "a minimum made exclusive",
			before: `{type: object, properties: {a: {type: integer, minimum: 1}}}`,
			after:  `{type: object, properties: {a: {type: integer, minimum: 1, exclusiveMinimum: true}}}`,
			want:   []string{".spec.a: minimum changes from 1 to 1, exclusive"},
		},
		{
			name:   "an exclusive minimum added",
			before: `{type: object, properties: {a: {type: integer}}}`,
			after:  `{type: object, properties: {a: {type: integer, minimum: 1, exclusiveMinimum: true}}}`,
			want:   []string{".spec.a: minimum 1, exclusive is added"},
		},
		{
			name: "minimums lowered, made inclusive and removed",
			before: `{type: object, properties: {a: {type: integer, minimum: 2}, ` +
				`b: {type: integer, minimum: 1, exclusiveMinimum: true}, c: {type: integer, minimum: 1}}}`,
			after: `{type: object, properties: {a: {type: integer, minimum: 1}, b: {type: integer, minimum: 1}, ` +
				`c: {type: integer}}}`,
		},
		{
			name: "a maximum added, one lowered and one made exclusive",
			before: `{type: object, properties: {a: {type: integer}, b: {type: integer, maximum: 10}, c: {type: integer, ` +
				`maximum: 5}}}`,
			after: `{type: object, properties: {a: {type: integer, maximum: 10}, b: {type: integer, maximum: 9}, ` +
				`c: {type: integer, maximum: 5, exclusiveMaximum: true}}}`,
			want: []string{".spec.a: maximum 10 is added", ".spec.b: maximum changes from 10 to 9",
				".spec.c: maximum changes from 5 to 5, exclusive"},
		},
		{
			name: "maximums raised, made inclusive and removed",
			before: `{type: object, properties: {a: {type: integer, maximum: 9}, ` +
				`b: {type: integer, maximum: 5, exclusiveMaximum: true}, c: {type: integer, maximum: 1}}}`,
			after: `{type: object, properties: {a: {type: integer, maximum: 10}, b: {type: integer, maximum: 5}, ` +
				`c: {type: integer}}}`,
		},
		{
			name: "lengths and counts added",
			before: `{type: object, properties: {s: {type: string}, l: {type: array, items: {type: string}}, ` +
				`m: {type: object, additionalProperties: {type: string}}}}`,
			after: `{type: object, properties: {s: {type: string, minLength: 1, maxLength: 63}, ` +
				`l: {type: array, minItems: 1, maxItems: 10, items: {type: string}}, ` +
				`m: {type: object, minProperties: 1, maxProperties: 5, additionalProperties: {type: string}}}}`,
			want: []string{".spec.l: minItems 1 is added", ".spec.l: maxItems 10 is added",
				".spec.m: minProperties 1 is added", ".spec.m: maxProperties 5 is added",
				".spec.s: minLength 1 is added", ".spec.s: maxLength 63 is added"},
		},
		{
			name: "lengths and counts tightened",
			before: `{type: object, properties: {s: {type: string, minLength: 1, maxLength: 63}, ` +
				`l: {type: array, minItems: 1, maxItems: 10, items: {type: string}}, ` +
				`m: {type: object, minProperties: 1, maxProperties: 5, additionalProperties: {type: string}}}}`,
			after: `{type: object, properties: {s: {type: string, minLength: 2, maxLength: 62}, ` +
				`l: {type: array, minItems: 2, maxItems: 9, items: {type: string}}, ` +
				`m: {type: object, minProperties: 2, maxProperties: 4, additionalProperties: {type: string}}}}`,
			want: []string{".spec.l: minItems changes from 1 to 2", ".spec.l: maxItems changes from 10 to 9",
				".spec.m: minProperties changes from 1 to 2", ".spec.m: maxProperties changes from 5 to 4",
				".spec.s: minLength changes from 1 to 2", ".spec.s: maxLength changes from 63 to 62"},
		},
		{
			name: "lengths and counts loosened and removed",
			before: `{type: object, properties: {s: {type: string, minLength: 2, maxLength: 62}, ` +
				`l: {type: array, minItems: 2, maxItems: 9, items: {type: string}}, ` +
				`m: {type: object, minProperties: 2, maxProperties: 4, additionalProperties: {type: string}}}}`,
			after: `{type: object, properties: {s: {type: string, minLength: 1}, ` +
				`l: {type: array, maxItems: 10, items: {type: string}}, ` +
				`m: {type: object, minProperties: 1, additionalProperties: {type: string}}}}`,
		},
		{
			name: "a multipleOf added, one changed and one removed",
			before: `{type: object, properties: {a: {type: integer}, b: {type: integer, multipleOf: 2}, c: {type: integer, ` +
				`multipleOf: 3}}}`,
			after: `{type: object, properties: {a: {type: integer, multipleOf: 5}, b: {type: integer, multipleOf: 4}, ` +
				`c: {type: integer}}}`,
			want: []string{".spec.a: multipleOf 5 is added", ".spec.b: multipleOf changes from 2 to 4"},
		},
		{
			name: "unique items",
			before: `{type: object, properties: {a: {type: array, items: {type: string}}, b: {type: array, uniqueItems: true, ` +
				`items: {type: string}}}}`,
			after: `{type: object, properties: {a: {type: array, uniqueItems: true, items: {type: string}}, b: {type: array, ` +
				`items: {type: string}}}}`,
			want: []string{".spec.a: its items must be unique"},
		},
		{
			name:   "null no longer accepted, and newly accepted",
			before: `{type: object, properties: {a: {type: string, nullable: true}, b: {type: string}}}`,
			after:  `{type: object, properties: {a: {type: string}, b: {type: string, nullable: true}}}`,
			want:   []string{".spec.a: it no longer accepts null"},
		},
		{
			name: "unknown fields no longer kept",
			before: `{type: object, x-kubernetes-preserve-unknown-fields: true, properties: {a: {type: object, ` +
				`x-kubernetes-preserve-unknown-fields: true}}}`,
			after: `{type: object, x-kubernetes-preserve-unknown-fields: false, properties: {a: {type: object}}}`,
			want:  []string{".spec: it no longer keeps unknown fields", ".spec.a: it no longer keeps unknown fields"},
		},
		{
			name: "embedded resources",
			before: `{type: object, properties: {a: {type: object}, ` +
				`b: {type: object, x-kubernetes-embedded-resource: true, x-kubernetes-preserve-unknown-fields: true}}}`,
			after: `{type: object, properties: {a: {type: object, x-kubernetes-embedded-resource: true, ` +
				`x-kubernetes-preserve-unknown-fields: true}, b: {type: object, x-kubernetes-preserve-unknown-fields: true}}}`,
			want: []string{".spec.a: x-kubernetes-embedded-resource changes from false to true",
				".spec.b: x-kubernetes-embedded-resource changes from true to false"},
		},
		{
			name: "a list's type and its map keys",
			before: `{type: object, properties: {a: {type: array, items: {type: string}}, b: {type: array, ` +
				`x-kubernetes-list-type: map, x-kubernetes-list-map-keys: [id], items: {type: object, required: [id, name], ` +
				`properties: {id: {type: string}, name: {type: string}}}}}}`,
			after: `{type: object, properties: {a: {type: array, x-kubernetes-list-type: set, items: {type: string}}, ` +
				`b: {type: array, x-kubernetes-list-type: map, x-kubernetes-list-map-keys: [id, name], items: {type: object, ` +
				`required: [id, name], properties: {id: {type: string}, name: {type: string}}}}}}`,
			want: []string{".spec.a: its list type changes from atomic to set",
				".spec.b: its list map keys change from [id] to [id name]"},
		},
		{
			name:   "a list type made explicit",
			before: `{type: object, properties: {a: {type: array, items: {type: string}}}}`,
			after:  `{type: object, properties: {a: {type: array, x-kubernetes-list-type: atomic, items: {type: string}}}}`,
		},
		{
			name: "an object's map type",
			before: `{type: object, properties: {a: {type: object, additionalProperties: {type: string}}, b: {type: object, ` +
				`x-kubernetes-map-type: granular}}}`,
			after: `{type: object, properties: {a: {type: object, x-kubernetes-map-type: atomic, ` +
				`additionalProperties: {type: string}}, b: {type: object}}}`,
			want: []string{".spec.a: its map type changes from granular to atomic"},
		},
		{
			name:   "a pattern and a format added",
			before: `{type: object, properties: {a: {type: string}, b: {type: string}}}`,
			after:  `{type: object, properties: {a: {type: string, pattern: "^[a-z]+$"}, b: {type: string, format: uri}}}`,
			want:   []string{`.spec.a: pattern "^[a-z]+$" is added`, `.spec.b: format "uri" is added`},
		},
		{
			name:   "a pattern and a format changed",
			before: `{type: object, properties: {a: {type: string, pattern: "^[a-z]+$"}, b: {type: string, format: uri}}}`,
			after: `{type: object, properties: {a: {type: string, pattern: "^[a-z0-9]+$"}, b: {type: string, ` +
				`format: hostname}}}`,
			want: []string{`.spec.a: pattern changes from "^[a-z]+$" to "^[a-z0-9]+$"`,
				`.spec.b: format changes from "uri" to "hostname"`},
		},
		{
			name:   "a pattern and a format removed",
			before: `{type: object, properties: {a: {type: string, pattern: "^[a-z]+$"}, b: {type: string, format: uri}}}`,
			after:  `{type: object, properties: {a: {type: string}, b: {type: string}}}`,
		},
		{
			name:   "a CEL rule added",
			before: `{type: object, x-kubernetes-validations: [{rule: "has(self.a)"}], properties: {a: {type: string}}}`,
			after: `{type: object, x-kubernetes-validations: [{rule: "has(self.a)"}, {rule: "self.a != 'b'", ` +
				`message: "not b"}], properties: {a: {type: string, x-kubernetes-validations: [{rule: "self == oldSelf"}]}}}`,
			want: []string{`.spec: CEL rule "self.a != 'b'" is added`, `.spec.a: CEL rule "self == oldSelf" is added`},
		},
		{
			name:   "a CEL rule whose old self becomes optional",
			before: `{type: object, properties: {a: {type: string, x-kubernetes-validations: [{rule: "self == oldSelf"}]}}}`,
			after: `{type: object, properties: {a: {type: string, x-kubernetes-validations: [{rule: "self == oldSelf", ` +
				`optionalOldSelf: true}]}}}`,
			want: []string{`.spec.a: CEL rule "self == oldSelf" is added`},
		},
		{
			name: "a CEL rule removed, and one with a new message",
			before: `{type: object, x-kubernetes-validations: [{rule: "has(self.a)", message: "a"}, {rule: "has(self.b)"}], ` +
				`properties: {a: {type: string}, b: {type: string}}}`,
			after: `{type: object, x-kubernetes-validations: [{rule: "has(self.a)", message: "set a", ` +
				`optionalOldSelf: false}], properties: {a: {type: string}, b: {type: string}}}`,
		},
		{
			name:   "allOf, anyOf, oneOf and not added",
			before: `{type: object, properties: {a: {type: string}}}`,
			after: `{type: object, allOf: [{required: [a]}], anyOf: [{required: [a]}], oneOf: [{required: [a]}], ` +
				`not: {required: [b]}, properties: {a: {type: string}}}`,
			want: []string{".spec: allOf is added or changed", ".spec: anyOf is added or changed",
				".spec: oneOf is added or changed", ".spec: not is added or changed"},
		},
		{
			name: "allOf, anyOf, oneOf and not kept, and removed",
			before: `{type: object, allOf: [{required: [a]}], anyOf: [{required: [a]}], oneOf: [{required: [a]}], ` +
				`not: {required: [b]}, properties: {a: {type: string}, b: {type: object, allOf: [{required: [c]}], ` +
				`anyOf: [{required: [c]}], oneOf: [{required: [c]}], not: {required: [d]}, ` +
				`properties: {c: {type: string}}}}}`,
			after: `{type: object, allOf: [{required: [a]}], anyOf: [{required: [a]}], oneOf: [{required: [a]}], ` +
				`not: {required: [b]}, properties: {a: {type: string}, b: {type: object, properties: {c: {type: string}}}}}`,
		},
		{
			name: "defaults added, changed and removed",
			before: `{type: object, properties: {a: {type: string}, b: {type: string, default: x}, c: {type: integer, ` +
				`default: 1}}}`,
			after: `{type: object, properties: {a: {type: string, default: w}, b: {type: string, default: z}, ` +
				`c: {type: integer}}}`,
			want: []string{`.spec.a: its default "w" is added`, `.spec.b: its default changes from "x" to "z"`,
				".spec.c: its default 1 is removed"},
		},
		{
			name:   "a new field with a default",
			before: `{type: object, properties: {a: {type: string}}}`,
			after:  `{type: object, required: [b], properties: {a: {type: string}, b: {type: string, default: x}}}`,
		},
		{
			name:   "fields newly required",
			before: `{type: object, required: [a], properties: {a: {type: string}, b: {type: string}}}`,
			after:  `{type: object, required: [a, b, c], properties: {a: {type: string}, b: {type: string}, c: {type: string}}}`,
			want:   []string{".spec.b: it becomes required", ".spec.c: it is new and required"},
		},
		{
			name:   "a field newly required, with a default",
			before: `{type: object, properties: {a: {type: string}}}`,
			after:  `{type: object, required: [a], properties: {a: {type: string, default: x}}}`,
			want:   []string{`.spec.a: its default "x" is added`},
		},
		{
			name:   "a required field that loses its default",
			before: `{type: object, required: [a], properties: {a: {type: string, default: x}}}`,
			after:  `{type: object, required: [a], properties: {a: {type: string}}}`,
			want:   []string{`.spec.a: its default "x" is removed`},
		},
		{
			name:   "a required field made optional",
			before: `{type: object, required: [a], properties: {a: {type: string}}}`,
			after:  `{type: object, properties: {a: {type: string}}}`,
		},
		{
			name: "a map's values",
			before: `{type: object, properties: {a: {type: object, additionalProperties: {type: string}}, b: {type: object, ` +
				`additionalProperties: true}}}`,
			after: `{type: object, properties: {a: {type: object, additionalProperties: {type: string, maxLength: 3}}, ` +
				`b: {type: object, additionalProperties: {type: integer}}}}`,
			want: []string{".spec.a[*]: maxLength 3 is added",
				".spec.b[*]: its type changes from any type to integer"},
		},
		{
			name:   "a map that becomes an object of fixed fields",
			before: `{type: object, properties: {a: {type: object, additionalProperties: {type: string}}}}`,
			after:  `{type: object, properties: {a: {type: object, properties: {x: {type: string}}}}}`,
			want:   []string{".spec.a: it no longer accepts keys of its own"},
		},
		{
			name:   "an object of fixed fields that becomes a map",
			before: `{type: object, properties: {a: {type: object, properties: {x: {type: string}}}}}`,
			after:  `{type: object, properties: {a: {type: object, x-kubernetes-preserve-unknown-fields: true}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := compare(t, tc.before, tc.after); !slices.Equal(got, tc.want) {
				t.Errorf("got\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(tc.want, "\n  "))
			}
		})
	}
}

// widgetYAML is a CRD as controller-gen writes one, with two versions: an
// alpha that is served and a beta that is not.
const widgetYAML = `---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.com
spec:
  group: example.com
  names:
    kind: Widget
    listKind: WidgetList
    plural: widgets
    singular: widget
  scope: Namespaced
  versions:
  - name: v1alpha1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              size:
                type: integer
              color:
                type: string
  - name: v1beta1
    served: false
    storage: false
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              size:
                type: integer
`

// widget returns a copy of widgetYAML's CRD.
func widget(t *testing.T) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	found := crds{}
	if err := found.add([]byte(widgetYAML)); err != nil {
		t.Fatal(err)
	}
	return found["widgets.example.com"]
}

// spec returns the spec field's schema of a CRD's version.
func spec(crd *apiextensionsv1.CustomResourceDefinition, version int) apiextensionsv1.JSONSchemaProps {
	return crd.Spec.Versions[version].Schema.OpenAPIV3Schema.Properties["spec"]
}

// setSpec replaces the spec field's schema of a CRD's version.
func setSpec(crd *apiextensionsv1.CustomResourceDefinition, version int, s apiextensionsv1.JSONSchemaProps) {
	crd.Spec.Versions[version].Schema.OpenAPIV3Schema.Properties["spec"] = s
}

func TestCompareCRDs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.CustomResourceDefinition
		// released changes the released CRD, if set.
		released func(*apiextensionsv1.CustomResourceDefinition)
		want     []change
	}{
		{
			name:   "an unchanged CRD",
			change: func(c *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.CustomResourceDefinition { return c },
		},
		{
			name: "a removed CRD",
			change: func(*apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.CustomResourceDefinition {
				return nil
			},
			want: []change{{kind: "Widget", what: "the CRD widgets.example.com is removed", alpha: true}},
		},
		{
			name: "a new scope, kind and list kind",
			change: func(c *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.CustomResourceDefinition {
				c.Spec.Scope = apiextensionsv1.ClusterScoped
				c.Spec.Names.Kind, c.Spec.Names.ListKind = "Gadget", "GadgetList"
				return c
			},
			want: []change{
				{kind: "Widget", what: "its scope changes from Namespaced to Cluster", alpha: true},
				{kind: "Widget", what: "its kind becomes Gadget", alpha: true},
				{kind: "Widget", what: "its list kind becomes GadgetList", alpha: true},
			},
		},
		{
			name: "a field removed from a served version",
			change: func(c *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.CustomResourceDefinition {
				s := spec(c, 0)
				delete(s.Properties, "color")
				return c
			},
			want: []change{{kind: "Widget", version: "v1alpha1", field: ".spec.color", what: "it is removed", alpha: true}},
		},
		{
			name: "a change to a version that was not served",
			change: func(c *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.CustomResourceDefinition {
				setSpec(c, 1, apiextensionsv1.JSONSchemaProps{Type: "string"})
				return c
			},
		},
		{
			name: "a served version removed",
			change: func(c *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.CustomResourceDefinition {
				c.Spec.Versions = c.Spec.Versions[1:]
				return c
			},
			want: []change{{kind: "Widget", version: "v1alpha1", what: "the version is removed", alpha: true}},
		},
		{
			name: "a version no longer served, and another served",
			change: func(c *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.CustomResourceDefinition {
				c.Spec.Versions[0].Served, c.Spec.Versions[1].Served = false, true
				return c
			},
			want: []change{{kind: "Widget", version: "v1alpha1", what: "the version is no longer served", alpha: true}},
		},
		{
			name: "a version that had no schema",
			change: func(c *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.CustomResourceDefinition {
				return c
			},
			released: func(c *apiextensionsv1.CustomResourceDefinition) { c.Spec.Versions[0].Schema = nil },
			want: []change{{kind: "Widget", version: "v1alpha1",
				what: "its type changes from any type to object", alpha: true}},
		},
		{
			name: "a new version",
			change: func(c *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.CustomResourceDefinition {
				v := *c.Spec.Versions[0].DeepCopy()
				v.Name, v.Storage = "v1", false
				c.Spec.Versions = append(c.Spec.Versions, v)
				return c
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := crds{}
			if crd := tc.change(widget(t)); crd != nil {
				current[crd.Name] = crd
			}
			released := widget(t)
			if tc.released != nil {
				tc.released(released)
			}
			got := compareCRDs(crds{released.Name: released}, current)
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestCompareCRDsVersions(t *testing.T) {
	// A beta that the release served cannot break, and neither can the whole CRD
	// while it serves one, but its alpha can.
	released := widget(t)
	released.Spec.Versions[1].Served = true
	current := released.DeepCopy()
	current.Spec.Names.ListKind = "Widgets"
	for i := range current.Spec.Versions {
		s := spec(current, i)
		delete(s.Properties, "size")
	}
	got := compareCRDs(crds{released.Name: released}, crds{current.Name: current})
	want := []change{
		{kind: "Widget", what: "its list kind becomes Widgets", alpha: false},
		{kind: "Widget", version: "v1alpha1", field: ".spec.size", what: "it is removed", alpha: true},
		{kind: "Widget", version: "v1beta1", field: ".spec.size", what: "it is removed", alpha: false},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestChangeString(t *testing.T) {
	for _, tc := range []struct {
		change change
		want   string
	}{
		{change{kind: "Widget", what: "its kind becomes Gadget"}, "Widget: its kind becomes Gadget"},
		{change{kind: "Widget", version: "v1alpha1", what: "the version is removed"},
			"Widget v1alpha1: the version is removed"},
		{change{kind: "Widget", version: "v1alpha1", field: ".spec.color", what: "it is removed"},
			"Widget v1alpha1 .spec.color: it is removed"},
	} {
		if got := tc.change.String(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

// eachRemoval calls f once for each property in root, after removing it from
// root, with its path as compareSchema writes it, and whether the object it is
// in keeps unknown fields. It puts the property back afterwards.
func eachRemoval(root *apiextensionsv1.JSONSchemaProps, f func(path string, kept bool)) {
	var walk func(s *apiextensionsv1.JSONSchemaProps, path string)
	walk = func(s *apiextensionsv1.JSONSchemaProps, path string) {
		for _, name := range slices.Sorted(maps.Keys(s.Properties)) {
			property := s.Properties[name]
			delete(s.Properties, name)
			f(path+"."+name, keepsUnknownFields(s))
			s.Properties[name] = property
			// The copy shares the property's own maps and pointers with root.
			walk(&property, path+"."+name)
		}
		if s.Items != nil && s.Items.Schema != nil {
			walk(s.Items.Schema, path+"[*]")
		}
		if s.AdditionalProperties != nil && s.AdditionalProperties.Schema != nil {
			walk(s.AdditionalProperties.Schema, path+"[*]")
		}
	}
	walk(root, "")
}

func TestRepositoryCRDs(t *testing.T) {
	current, err := currentCRDs(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) == 0 {
		t.Fatalf("no CRDs in %s", crdDir)
	}
	if changes := compareCRDs(current, current); len(changes) > 0 {
		t.Fatalf("the CRDs differ from themselves: %v", changes)
	}

	// Each field of each CRD, removed, is the one change found.
	for _, name := range slices.Sorted(maps.Keys(current)) {
		released := current[name]
		fields := 0
		for i, version := range released.Spec.Versions {
			changed := released.DeepCopy()
			eachRemoval(changed.Spec.Versions[i].Schema.OpenAPIV3Schema, func(path string, kept bool) {
				fields++
				var want []change
				if !kept {
					want = []change{{kind: released.Spec.Names.Kind, version: version.Name, field: path,
						what: "it is removed", alpha: alphaVersion.MatchString(version.Name)}}
				}
				got := compareCRDs(crds{name: released}, crds{name: changed})
				if !slices.Equal(got, want) {
					t.Errorf("%s %s without %s: got %v, want %v", name, version.Name, path, got, want)
				}
			})
		}
		// apiVersion, kind, metadata, spec and status at least.
		if fields < 5 {
			t.Errorf("%s: only %d fields removed", name, fields)
		}
		t.Logf("%s: %d fields removed, one at a time", name, fields)
	}

	// A breaking change of each other kind to a real CRD is found too.
	dns := current["dnsrecords.kflare.dev"].DeepCopy()
	s := spec(dns, 0)
	content := s.Properties["content"]
	content.MaxLength = new(int64(8))
	s.Properties["content"] = content
	s.XValidations = append(s.XValidations, apiextensionsv1.ValidationRule{Rule: "self.name != 'www'"})
	s.Required = append(s.Required, "comment")
	setSpec(dns, 0, s)
	got := compareCRDs(crds{dns.Name: current[dns.Name]}, crds{dns.Name: dns})
	want := []string{
		`DNSRecord v1alpha1 .spec: CEL rule "self.name != 'www'" is added`,
		"DNSRecord v1alpha1 .spec.content: maxLength 8 is added",
		"DNSRecord v1alpha1 .spec.comment: it becomes required",
	}
	if strs := changeStrings(got); !slices.Equal(strs, want) {
		t.Errorf("got\n  %s\nwant\n  %s", strings.Join(strs, "\n  "), strings.Join(want, "\n  "))
	}
}

// changeStrings returns changes as their reports' lines.
func changeStrings(changes []change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.String())
	}
	return out
}

// changelog has the unreleased changes, a release being prepared and the
// release the branch builds on, v1.2.0, between two other releases.
var changelog = fenced(`# Changelog

Intro.

## Unreleased

### Breaking changes

- ` + "`DNSRecord`" + `'s ` + "`ttl`" + ` is gone

#### How to upgrade

Rewrite the DNSRecords.

### The controller

- Not breaking: Zone

### Breaking changes

- ` + "`Tunnel`" + `, in a second breaking section

## v1.3.0 (alpha)

The release, which an IPList does not break.

### Breaking Changes
'''yaml
### Not a heading
kind: TunnelConfiguration
'''

### Upgrading from v1.2.0

- WorkerScript is named here, but not as breaking

## v1.2.01 (alpha)

### Breaking changes

- R2Bucket, in a release above the base whose version starts with the base's

## v1.2.0 (alpha)

### Breaking changes

- KVNamespace, in the base release

## v1.1.0 (alpha)
`)

// fenced turns three single quotes in s into a Markdown code fence's three
// backticks, which a raw string cannot hold.
func fenced(s string) string { return strings.ReplaceAll(s, "'''", "```") }

func TestBreakingNotes(t *testing.T) {
	notes, err := breakingNotes(changelog, "v1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string]bool{
		"DNSRecord": true, "Tunnel": true, "TunnelConfiguration": true,
		"R2Bucket": true, "Zone": false, "IPList": false, "WorkerScript": false, "KVNamespace": false,
	} {
		if got := names(notes, kind); got != want {
			t.Errorf("names(%s) = %t, want %t, in notes:\n%s", kind, got, want, notes)
		}
	}
	if !strings.Contains(notes, "Rewrite the DNSRecords.") {
		t.Errorf("the notes lose a subsection of a breaking section:\n%s", notes)
	}
	if strings.Contains(notes, "### Breaking") {
		t.Errorf("the notes hold a section's heading:\n%s", notes)
	}

	_, err = breakingNotes(changelog, "v1.2.1")
	if err == nil || !strings.Contains(err.Error(), "no section for v1.2.1") {
		t.Errorf("a release without a section: got %v", err)
	}
	if _, err := breakingNotes(fenced("## Unreleased\n'''\n## v1.2.0\n'''\n"), "v1.2.0"); err == nil {
		t.Error("a heading in a code fence counts as the release's section")
	}
}

func TestRepositoryChangelog(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot, changelogPath))
	if err != nil {
		t.Fatal(err)
	}
	latest := regexp.MustCompile(`(?m)^## (v\d+\.\d+\.\d+)\b`).FindStringSubmatch(string(data))
	if latest == nil {
		t.Fatalf("%s has no release section", changelogPath)
	}
	if _, err := breakingNotes(string(data), latest[1]); err != nil {
		t.Errorf("%s: %v", changelogPath, err)
	}
}

func TestNames(t *testing.T) {
	for _, tc := range []struct {
		notes, kind string
		want        bool
	}{
		{"`Tunnel`'s name", "Tunnel", true},
		{"Tunnels now", "Tunnel", true},
		{"DNSRecords", "DNSRecord", true},
		{"TunnelConfiguration", "Tunnel", false},
		{"ZoneSettings", "Zone", false},
		{"zones", "Zone", false},
		{"a tunnel", "Tunnel", false},
		{"MyTunnel", "Tunnel", false},
	} {
		if got := names(tc.notes, tc.kind); got != tc.want {
			t.Errorf("names(%q, %s) = %t, want %t", tc.notes, tc.kind, got, tc.want)
		}
	}
}

func TestVerdict(t *testing.T) {
	alpha := change{kind: "DNSRecord", version: "v1alpha1", field: ".spec.ttl", what: "it is removed", alpha: true}
	other := change{kind: "Zone", version: "v1alpha1", field: ".spec.plan", what: "it is removed", alpha: true}
	beta := change{kind: "Tunnel", version: "v1beta1", field: ".spec.name", what: "it is removed"}
	const unnamed = "CHANGELOG.md's breaking changes since v1.2.0 do not name "
	for _, tc := range []struct {
		name    string
		changes []change
		notes   string
		wantErr []string
	}{
		{name: "every kind named", changes: []change{alpha, other}, notes: "DNSRecord and Zone"},
		{name: "a kind not named", changes: []change{alpha, other, other}, notes: "DNSRecord",
			wantErr: []string{unnamed + "Zone: add"}},
		{name: "no notes", changes: []change{alpha, other}, wantErr: []string{unnamed + "DNSRecord, Zone: add"}},
		{name: "a change to a beta", changes: []change{beta, alpha, beta}, notes: "Tunnel, DNSRecord",
			wantErr: []string{"Tunnel: a version that is not an alpha must not break"}},
		{name: "both", changes: []change{alpha, beta}, wantErr: []string{"Tunnel: a version that is not an alpha",
			unnamed + "DNSRecord: add"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := verdict(tc.changes, tc.notes, "v1.2.0")
			if !strings.HasPrefix(report, "Breaking changes to the CRDs since v1.2.0:\n\n") {
				t.Errorf("report:\n%s", report)
			}
			for _, c := range tc.changes {
				if !strings.Contains(report, "\n  "+c.String()+"\n") {
					t.Errorf("the report lacks %q:\n%s", c, report)
				}
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !strings.HasSuffix(report, "\nCHANGELOG.md lists them under \"Breaking changes\".\n") {
					t.Errorf("report:\n%s", report)
				}
				return
			}
			if err == nil {
				t.Fatal("no error")
			}
			lines := strings.Split(err.Error(), "\n")
			if len(lines) != len(tc.wantErr) {
				t.Fatalf("error %q: want %d lines", err, len(tc.wantErr))
			}
			for i, want := range tc.wantErr {
				if !strings.HasPrefix(lines[i], want) {
					t.Errorf("error line %q does not start with %q", lines[i], want)
				}
			}
			if strings.Contains(report, "lists them") {
				t.Errorf("a failed check says the changelog lists the changes:\n%s", report)
			}
		})
	}
}

// gitRepo is a git repository in a temporary directory, isolated from the
// user's and the system's git configuration.
type gitRepo struct {
	t   *testing.T
	dir string
}

func newGitRepo(t *testing.T) *gitRepo {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("LC_ALL", "C")
	for _, who := range []string{"AUTHOR", "COMMITTER"} {
		t.Setenv("GIT_"+who+"_NAME", "kflare test")
		t.Setenv("GIT_"+who+"_EMAIL", "test@example.com")
	}
	r := &gitRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *gitRepo) git(args ...string) {
	r.t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", r.dir}, args...)...).CombinedOutput(); err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func (r *gitRepo) write(file, content string) {
	r.t.Helper()
	path := filepath.Join(r.dir, file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// release commits the working tree and tags it with an annotated tag, as Tag
// release does.
func (r *gitRepo) release(tag string) {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", "release "+tag)
	r.git("tag", "-a", tag, "-m", "kflare "+tag)
}

const (
	widgetFile = crdDir + "/example.com_widgets.yaml"
	// releasedChangelog is the changelog of release v0.1.0.
	releasedChangelog = "# Changelog\n\n## Unreleased\n\n## v0.1.0 (alpha)\n\nThe first release.\n"
)

func TestRun(t *testing.T) {
	r := newGitRepo(t)
	r.write(widgetFile, widgetYAML)
	r.write(changelogPath, releasedChangelog)
	r.release("v0.1.0")

	report, err := run(r.dir, "")
	if err != nil || report != "No breaking change to the CRDs since v0.1.0.\n" {
		t.Fatalf("an unchanged CRD: %q, %v", report, err)
	}

	r.write(widgetFile, strings.Replace(widgetYAML, "              color:\n                type: string\n", "", 1))
	report, err = run(r.dir, "")
	if err == nil || !strings.Contains(err.Error(), "do not name Widget") {
		t.Errorf("a breaking change the changelog does not list: %v", err)
	}
	if !strings.Contains(report, "\n  Widget v1alpha1 .spec.color: it is removed\n") {
		t.Errorf("report:\n%s", report)
	}

	r.write(changelogPath, strings.Replace(releasedChangelog, "## Unreleased\n",
		"## Unreleased\n\n### Breaking changes\n\n- A Widget has no color: remove it from your manifests\n", 1))
	if report, err = run(r.dir, ""); err != nil || !strings.HasSuffix(report, "lists them under \"Breaking changes\".\n") {
		t.Errorf("a breaking change the changelog lists: %q, %v", report, err)
	}

	r.write(changelogPath, "# Changelog\n")
	if _, err = run(r.dir, ""); err == nil || !strings.Contains(err.Error(), "no section for v0.1.0") {
		t.Errorf("a changelog without the release's section: %v", err)
	}
	if _, err = run(r.dir, "v0.0.9"); err == nil || !strings.Contains(err.Error(), "v0.0.9") ||
		!strings.Contains(err.Error(), "Not a valid object name") {
		t.Errorf("a release that does not exist: %v", err)
	}
}

func TestLastRelease(t *testing.T) {
	r := newGitRepo(t)
	if _, err := lastRelease(r.dir); err == nil {
		t.Error("a repository without commits has a release")
	}
	r.write(widgetFile, widgetYAML)
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "first")
	if _, err := lastRelease(r.dir); err == nil || !strings.Contains(err.Error(), "no release tag") {
		t.Errorf("a repository without tags: %v", err)
	}

	r.release("v0.9.0")
	r.release("v0.10.0")
	// Not releases: Tag release makes vX.Y.Z tags only.
	r.release("v0.11.0-rc.1")
	r.release("vnext")
	r.git("tag", "v1.0")
	// A newer release on another branch is not in HEAD's history.
	r.git("switch", "-q", "-c", "other")
	r.release("v0.12.0")
	r.git("switch", "-q", "main")

	got, err := lastRelease(r.dir)
	if err != nil || got != "v0.10.0" {
		t.Errorf("lastRelease = %q, %v; want v0.10.0", got, err)
	}
	// run compares with it, or with the release it is given.
	r.write(changelogPath, "## Unreleased\n\n## v0.12.0\n\n## v0.10.0\n\n## v0.9.0\n")
	for base, want := range map[string]string{"": "v0.10.0", "v0.12.0": "v0.12.0", "v0.9.0": "v0.9.0"} {
		if report, err := run(r.dir, base); err != nil || !strings.Contains(report, "since "+want+".") {
			t.Errorf("run(%q) = %q, %v; want a comparison with %s", base, report, err, want)
		}
	}
}

func TestReadCRDs(t *testing.T) {
	r := newGitRepo(t)
	r.write(changelogPath, releasedChangelog)
	// The directory, without a CRD in it.
	r.write(crdDir+"/README.md", "CRDs go here.\n")
	r.release("v0.1.0")
	if _, err := releasedCRDs(r.dir, "v0.1.0"); err == nil || !strings.Contains(err.Error(), "holds no CRDs") {
		t.Errorf("a release without CRDs: %v", err)
	}
	if _, err := releasedCRDs(r.dir, "v0.0.1"); err == nil {
		t.Error("a release that does not exist has CRDs")
	}

	r.write(crdDir+"/broken.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: broken\n")
	r.release("v0.2.0")
	for _, read := range []func() (crds, error){
		func() (crds, error) { return releasedCRDs(r.dir, "v0.2.0") },
		func() (crds, error) { return currentCRDs(r.dir) },
	} {
		if _, err := read(); err == nil || !strings.Contains(err.Error(), "broken.yaml") ||
			!strings.Contains(err.Error(), "not a CustomResourceDefinition") {
			t.Errorf("a file that is not a CRD: %v", err)
		}
	}
	r.write(crdDir+"/broken.yaml", "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n")
	if _, err := currentCRDs(r.dir); err == nil || !strings.Contains(err.Error(), "not a CustomResourceDefinition") {
		t.Errorf("a CRD without a name: %v", err)
	}
	r.write(crdDir+"/broken.yaml", "{not yaml")
	if _, err := currentCRDs(r.dir); err == nil || !strings.Contains(err.Error(), "broken.yaml") {
		t.Errorf("a file that is not YAML: %v", err)
	}
	if _, err := currentCRDs(t.TempDir()); err == nil {
		t.Error("a repository without CRDs has CRDs")
	}
}
