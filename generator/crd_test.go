/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// constraints is what a CRD says a spec field's value may be, in a form both
// a CRD's schema and kflare-gen's model give.
type constraints struct {
	required, immutable  bool
	minLength, maxLength string
	pattern              string
	minimum, maximum     string
	enum                 []string
	def                  string
}

func (c constraints) String() string {
	// A type without the method, or formatting would call String again.
	type fields constraints
	return fmt.Sprintf("%+v", fields(c))
}

// crdConstraints reads a CRD property's constraints.
func crdConstraints(t *testing.T, prop apiextensionsv1.JSONSchemaProps, required bool) constraints {
	t.Helper()
	c := constraints{required: required, pattern: prop.Pattern}
	if prop.MinLength != nil {
		c.minLength = strconv.FormatInt(*prop.MinLength, 10)
	}
	if prop.MaxLength != nil {
		c.maxLength = strconv.FormatInt(*prop.MaxLength, 10)
	}
	if prop.Minimum != nil {
		c.minimum = formatNumber(*prop.Minimum)
	}
	if prop.Maximum != nil {
		c.maximum = formatNumber(*prop.Maximum)
	}
	for _, value := range prop.Enum {
		var s string
		if err := json.Unmarshal(value.Raw, &s); err != nil {
			t.Fatalf("enum value %s: %v", value.Raw, err)
		}
		c.enum = append(c.enum, s)
	}
	slices.Sort(c.enum)
	if prop.Default != nil {
		c.def = string(prop.Default.Raw)
	}
	for _, rule := range prop.XValidations {
		if rule.Rule == "self == oldSelf" {
			c.immutable = true
		}
	}
	return c
}

// modelConstraints reads a generated field's constraints.
func modelConstraints(f field) constraints {
	v := f.validation
	c := constraints{required: !f.optional, immutable: f.immutable, pattern: v.pattern, def: f.def,
		enum: slices.Sorted(slices.Values(f.enum))}
	if v.minLength != nil {
		c.minLength = strconv.FormatInt(*v.minLength, 10)
	}
	if v.maxLength != nil {
		c.maxLength = strconv.FormatInt(*v.maxLength, 10)
	}
	if v.minimum != nil {
		c.minimum = formatNumber(*v.minimum)
	}
	if v.maximum != nil {
		c.maximum = formatNumber(*v.maximum)
	}
	return c
}

// ruleMarker reads the rule of a +kubebuilder:validation:XValidation marker.
var ruleMarker = regexp.MustCompile(`XValidation:rule="(.*)",message=`)

// TestConstraintsMatchTheHandWrittenCRDs is feat/generator-openapi's done
// criterion. It builds KVNamespace and IPList from cloudflare-go v7 and the
// OpenAPI subset, and compares the validation of every spec field with their
// CRDs in config/crd/bases. IPList's types are hand-written; KVNamespace's were
// generated before kflare-gen read the spec, from validation the hand-written
// ones had. Where the spec is wrong, the entries replace its validation and say
// why. R2Bucket was compared too until kflare-gen generated it
// (feat/generate-r2-bucket).
func TestConstraintsMatchTheHandWrittenCRDs(t *testing.T) {
	tests := []struct {
		config, kind, crd string
		// handWrittenOnly lists the CRD's spec fields that the service does
		// not have, with the reason.
		handWrittenOnly map[string]string
	}{
		{config: "generator.yaml", kind: "KVNamespace", crd: "kflare.dev_kvnamespaces.yaml"},
		{config: "testdata/generator.yaml", kind: "TestIPList", crd: "kflare.dev_iplists.yaml",
			handWrittenOnly: map[string]string{"items": "written through the list items API, not the list service"}},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			cfg, err := loadConfig(tc.config)
			if err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(cfg.Kinds, func(k kindConfig) bool { return k.Kind == tc.kind })
			if i < 0 {
				t.Fatalf("%s has no kind %s", tc.config, tc.kind)
			}
			entry := cfg.Kinds[i]
			k, err := buildKind(entry, loadTestService(t, entry.Service), loadTestSpec(t))
			if err != nil {
				t.Fatal(err)
			}

			data, err := os.ReadFile(filepath.Join("..", "config", "crd", "bases", tc.crd))
			if err != nil {
				t.Fatal(err)
			}
			crd := &apiextensionsv1.CustomResourceDefinition{}
			if err := yaml.Unmarshal(data, crd); err != nil {
				t.Fatal(err)
			}
			spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]

			generated := map[string]field{}
			for _, f := range k.spec {
				generated[f.jsonName] = f
			}
			for name, prop := range spec.Properties {
				if _, ok := tc.handWrittenOnly[name]; ok {
					continue
				}
				f, ok := generated[name]
				if !ok {
					t.Errorf("the CRD has spec.%s, which kflare-gen does not generate", name)
					continue
				}
				want := crdConstraints(t, prop, slices.Contains(spec.Required, name))
				if got := modelConstraints(f); !equalConstraints(got, want) {
					t.Errorf("spec.%s:\n generated %s\n the CRD   %s", name, got, want)
				}
			}
			for name := range generated {
				if _, ok := spec.Properties[name]; !ok {
					t.Errorf("kflare-gen generates spec.%s, which the CRD does not have", name)
				}
			}

			crdRules := make([]string, 0, len(spec.XValidations))
			generatedRules := make([]string, 0, len(k.specRules))
			for _, rule := range spec.XValidations {
				crdRules = append(crdRules, rule.Rule)
			}
			for _, marker := range k.specRules {
				generatedRules = append(generatedRules, ruleMarker.FindStringSubmatch(marker)[1])
			}
			slices.Sort(crdRules)
			slices.Sort(generatedRules)
			if !slices.Equal(crdRules, generatedRules) {
				t.Errorf("the spec's rules: generated %q, the CRD %q", generatedRules, crdRules)
			}
		})
	}
}

func equalConstraints(a, b constraints) bool {
	return a.required == b.required && a.immutable == b.immutable && a.minLength == b.minLength &&
		a.maxLength == b.maxLength && a.pattern == b.pattern && a.minimum == b.minimum && a.maximum == b.maximum &&
		slices.Equal(a.enum, b.enum) && a.def == b.def
}
