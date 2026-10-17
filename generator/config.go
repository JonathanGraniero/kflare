/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// config is generator/generator.yaml: one entry per kind kflare-gen generates.
type config struct {
	// Kinds are the kinds the shared CRUD reconciler drives.
	Kinds []kindConfig `json:"kinds"`
	// RuleKinds are the kinds whose resources each manage one rule in a zone's
	// Rulesets phase, which the shared rule controller drives.
	RuleKinds []ruleKindConfig `json:"ruleKinds,omitempty"`
}

// kindConfig is one kind's entry. Every field of the kind's cloudflare-go v7
// service becomes a field of the kind (fields are opt-out), except those the
// entry ignores.
type kindConfig struct {
	// Kind is the API type's name, such as KVNamespace.
	Kind string `json:"kind"`
	// Service is the cloudflare-go v7 service the kind calls, as
	// <package>.<type>, the package's path being relative to the module: the
	// KV namespaces service is kv.NamespaceService.
	Service string `json:"service"`
	// Scope is what a resource of the kind names its parent with. The only
	// scope so far is account: an accountRef, for the account_id the
	// service's calls take.
	Scope string `json:"scope"`
	// ShortName is the kind's short name for kubectl, such as cfkv.
	ShortName string `json:"shortName"`
	// Noun is what docs and messages call the Cloudflare object, such as "KV
	// namespace".
	Noun string `json:"noun"`
	// Doc is the kind's documentation, from the type's name on.
	Doc string `json:"doc"`
	// ID names the object's ID and where status records it.
	ID idConfig `json:"id"`
	// ClaimLabel is the label holding the ID of the object a resource
	// manages.
	ClaimLabel string `json:"claimLabel"`
	// Adopt says how a resource finds the existing object it adopts.
	Adopt adoptConfig `json:"adopt"`
	// Dependents are the kinds whose resources use a resource of this kind,
	// which waits for them before it is deleted.
	Dependents []dependentConfig `json:"dependents,omitempty"`
	// Sample holds the values of the sample's spec fields, by API name. It
	// sets every required field, and every immutable string field, which the
	// generated specs change.
	Sample map[string]any `json:"sample"`
	// Fields adjusts fields, by their API name.
	Fields map[string]fieldConfig `json:"fields,omitempty"`
	// Fixed leaves out of the spec fields that kflare always sends with the
	// same value, by their API name: an IPList's list kind is always ip.
	Fixed map[string]string `json:"fixed,omitempty"`
	// Ignore leaves out fields, by their API name, each with the reason.
	Ignore map[string]string `json:"ignore,omitempty"`
}

// idConfig names an object's ID.
type idConfig struct {
	// Field is the ID's API name in the object, such as id.
	Field string `json:"field"`
	// Status is the ID's name in status.cloudflareMetadata, such as
	// namespaceID.
	Status string `json:"status"`
	// Doc replaces the SDK's documentation of the ID.
	Doc string `json:"doc,omitempty"`
	// PrintColumn names the ID's column; by default, the status name in
	// words ("Namespace ID").
	PrintColumn string `json:"printColumn,omitempty"`
}

// adoptConfig says how a resource finds the existing object it adopts.
type adoptConfig struct {
	// By is the API name of the field a resource adopts an existing object
	// by, such as title: one that no two objects in an account share. A
	// resource that would adopt an object another one manages reports the
	// field's Go name followed by Conflict, as in TitleConflict.
	By string `json:"by"`
}

// dependentConfig is a kind whose resources use resources of another.
type dependentConfig struct {
	// Kind is the dependent kind, such as WorkerScript.
	Kind string `json:"kind"`
	// Refs names a hand-written function in internal/controller that returns
	// the names of the resources a dependent uses, in its namespace, such as
	// boundKVNamespaces.
	Refs string `json:"refs"`
	// Reason says why a resource waits for these dependents.
	Reason string `json:"reason"`
}

// fieldConfig adjusts one field.
type fieldConfig struct {
	// Doc replaces the SDK's documentation of the field.
	Doc string `json:"doc,omitempty"`
	// PrintColumn adds a column for the field, with this name.
	PrintColumn string `json:"printColumn,omitempty"`
	// Validation replaces what the OpenAPI spec says the field's value may
	// be. The spec is not always the live API.
	Validation *validationConfig `json:"validation,omitempty"`
}

// validationConfig replaces a field's validation, with the reason.
type validationConfig struct {
	// MinLength, MaxLength and Pattern bound a string field.
	MinLength *int64 `json:"minLength,omitempty"`
	MaxLength *int64 `json:"maxLength,omitempty"`
	Pattern   string `json:"pattern,omitempty"`
	// Minimum and Maximum bound an integer field.
	Minimum *int64 `json:"minimum,omitempty"`
	Maximum *int64 `json:"maximum,omitempty"`
	// Reason says why the spec's validation does not do, such as what a live
	// check found.
	Reason string `json:"reason"`
}

// validation returns the validation v replaces a field's with.
func (v *validationConfig) validation() validation {
	out := validation{minLength: v.MinLength, maxLength: v.MaxLength, pattern: v.Pattern}
	if v.Minimum != nil {
		minimum := float64(*v.Minimum)
		out.minimum = &minimum
	}
	if v.Maximum != nil {
		maximum := float64(*v.Maximum)
		out.maximum = &maximum
	}
	return out
}

var (
	kindName   = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	shortName  = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	statusName = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)
	// funcName is an unexported Go name.
	funcName = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)
)

// loadConfig reads and checks the configuration at path. Unknown keys are
// refused, so that a misspelled one does not go unnoticed.
func loadConfig(path string) (*config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &config{}
	if err := yaml.UnmarshalStrict(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// validate checks what can be checked without the SDK. Field names are
// checked against the SDK when the kind is built.
func (c *config) validate() error {
	if len(c.Kinds) == 0 && len(c.RuleKinds) == 0 {
		return fmt.Errorf("no kinds")
	}
	seen := map[string]bool{}
	for i, k := range c.Kinds {
		if err := k.validate(); err != nil {
			return fmt.Errorf("kinds[%d] (%s): %w", i, k.Kind, err)
		}
		if seen[k.Kind] {
			return fmt.Errorf("kinds[%d]: %s is listed twice", i, k.Kind)
		}
		seen[k.Kind] = true
	}
	for i, k := range c.RuleKinds {
		if err := k.validate(); err != nil {
			return fmt.Errorf("ruleKinds[%d] (%s): %w", i, k.Kind, err)
		}
		if seen[k.Kind] {
			return fmt.Errorf("ruleKinds[%d]: %s is listed twice", i, k.Kind)
		}
		seen[k.Kind] = true
	}
	return nil
}

func (k *kindConfig) validate() error {
	switch {
	case !kindName.MatchString(k.Kind):
		return fmt.Errorf("kind %q is not a Go type name", k.Kind)
	case strings.Count(k.Service, ".") == 0 || strings.HasPrefix(k.Service, ".") || strings.HasSuffix(k.Service, "."):
		return fmt.Errorf("service %q is not <package>.<type>", k.Service)
	case k.Scope != "account":
		return fmt.Errorf("scope %q is not supported: the only scope is account", k.Scope)
	case !shortName.MatchString(k.ShortName):
		return fmt.Errorf("shortName %q is not lower-case letters and digits", k.ShortName)
	case strings.TrimSpace(k.Noun) == "":
		return fmt.Errorf("noun is empty")
	case !strings.HasPrefix(k.Doc, k.Kind+" "):
		return fmt.Errorf("doc does not start with %q", k.Kind+" ")
	case k.ID.Field == "":
		return fmt.Errorf("id.field is empty")
	case !statusName.MatchString(k.ID.Status):
		return fmt.Errorf("id.status %q is not a lowerCamel name", k.ID.Status)
	case !strings.HasPrefix(k.ClaimLabel, "kflare.dev/"):
		return fmt.Errorf("claimLabel %q does not start with kflare.dev/", k.ClaimLabel)
	case k.Adopt.By == "":
		return fmt.Errorf("adopt.by is empty")
	case len(k.Sample) == 0:
		return fmt.Errorf("sample is empty")
	}
	seenDependents := map[string]bool{}
	for i, d := range k.Dependents {
		switch {
		case !kindName.MatchString(d.Kind):
			return fmt.Errorf("dependents[%d]: kind %q is not a Go type name", i, d.Kind)
		case seenDependents[d.Kind]:
			return fmt.Errorf("dependents[%d]: %s is listed twice", i, d.Kind)
		case !funcName.MatchString(d.Refs):
			return fmt.Errorf("dependents[%d]: refs %q is not an unexported Go name", i, d.Refs)
		case strings.TrimSpace(d.Reason) == "":
			return fmt.Errorf("dependents[%d]: gives no reason", i)
		}
		seenDependents[d.Kind] = true
	}
	for name, reason := range k.Ignore {
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("ignore.%s gives no reason", name)
		}
		if _, ok := k.Fields[name]; ok {
			return fmt.Errorf("%s is both ignored and adjusted in fields", name)
		}
		if _, ok := k.Fixed[name]; ok {
			return fmt.Errorf("%s is both ignored and fixed", name)
		}
	}
	for name := range k.Fixed {
		if _, ok := k.Fields[name]; ok {
			return fmt.Errorf("%s is both fixed and adjusted in fields", name)
		}
	}
	for name, f := range k.Fields {
		if f.Validation != nil {
			if err := f.Validation.validate(); err != nil {
				return fmt.Errorf("fields.%s.validation: %w", name, err)
			}
		}
	}
	return nil
}

func (v *validationConfig) validate() error {
	switch {
	case strings.TrimSpace(v.Reason) == "":
		return fmt.Errorf("gives no reason")
	case v.MinLength != nil && v.MaxLength != nil && *v.MinLength > *v.MaxLength:
		return fmt.Errorf("minLength is above maxLength")
	case v.Minimum != nil && v.Maximum != nil && *v.Minimum > *v.Maximum:
		return fmt.Errorf("minimum is above maximum")
	case strings.Contains(v.Pattern, "`"):
		return fmt.Errorf("the pattern holds a backquote, which a kubebuilder marker cannot")
	}
	if _, err := regexp.Compile(v.Pattern); err != nil {
		return fmt.Errorf("the pattern does not compile: %w", err)
	}
	return nil
}

// ruleKindConfig is the entry of a rule kind: a kind whose resources each
// manage one rule in a zone's entry point ruleset for a Rulesets phase,
// through the shared rule controller. Its spec holds the fields every rule
// kind shares (RulesetRuleSpec) and the parameters of its action, which
// kflare-gen builds from cloudflare-go v7's rule type for the action. Every
// field of the action parameters becomes a field of the spec (fields are
// opt-out), except those the entry ignores.
type ruleKindConfig struct {
	// Kind is the API type's name, such as TransformRule.
	Kind string `json:"kind"`
	// Rule is cloudflare-go v7's type for a rule with the kind's action, as
	// <package>.<type>: rulesets.RewriteRuleParam. The action it allows is
	// the kind's, and its action parameters give the kind's fields.
	Rule string `json:"rule"`
	// ShortName is the kind's short name for kubectl, such as cftransform.
	ShortName string `json:"shortName"`
	// Noun is what docs and messages call the rule, such as "transform rule".
	Noun string `json:"noun"`
	// Doc is the kind's documentation, from the type's name on.
	Doc string `json:"doc"`
	// Phase is the Rulesets phase of the kind's rules, when they all go in
	// one, such as http_request_firewall_custom.
	Phase string `json:"phase,omitempty"`
	// Types, when the kind's rules go in several phases, are the values of
	// the immutable spec field type, which picks each resource's phase.
	Types []ruleTypeConfig `json:"types,omitempty"`
	// TypeDoc documents the type field, before the list of its values.
	TypeDoc string `json:"typeDoc,omitempty"`
	// Samples are valid specs, as a resource sets them, zoneRef aside: one
	// for each type, in the order of Types, or one. config/samples holds them
	// and the generated specs start from them.
	Samples []map[string]any `json:"samples"`
	// Tests are more valid specs, of any type, that the generated specs write
	// and edit as they do the samples, for fields no sample sets. They are
	// not published as samples.
	Tests []map[string]any `json:"tests,omitempty"`
	// Fields adjusts fields of the action parameters, by their path of API
	// names, such as uri.path.
	Fields map[string]ruleFieldConfig `json:"fields,omitempty"`
	// Rules are CEL rules on the spec, for what Cloudflare refuses that its
	// API description does not say.
	Rules []ruleConfig `json:"rules,omitempty"`
	// Ignore leaves out fields of the rule type or of its action parameters,
	// by path, each with the reason.
	Ignore map[string]string `json:"ignore,omitempty"`
}

// ruleTypeConfig is one value of a rule kind's type field.
type ruleTypeConfig struct {
	// Name is the value, such as urlRewrite.
	Name string `json:"name"`
	// Phase is the Rulesets phase it picks, such as http_request_transform.
	Phase string `json:"phase"`
	// Doc says what a rule of the type does, for the type field's doc.
	Doc string `json:"doc"`
}

// ruleFieldConfig adjusts one field of a rule kind's action parameters.
type ruleFieldConfig struct {
	// Doc replaces the SDK's documentation of the field.
	Doc string `json:"doc,omitempty"`
	// Key, for a map, names the field that holds an entry's key: the spec
	// lists the map's values, each with its key in this field, keyed by it.
	Key string `json:"key,omitempty"`
	// KeyDoc documents the key field.
	KeyDoc string `json:"keyDoc,omitempty"`
	// KeyValidation replaces the key field's validation, a minimum length of
	// one: what Cloudflare takes as a key.
	KeyValidation *validationConfig `json:"keyValidation,omitempty"`
	// Validation replaces what the OpenAPI spec says the field's value may
	// be.
	Validation *validationConfig `json:"validation,omitempty"`
	// Rules are CEL rules on the object at the path, such as browser_ttl, or
	// on an item of a list or map, such as vary.headers[]: what Cloudflare
	// refuses that its API description does not say.
	Rules []ruleConfig `json:"rules,omitempty"`
}

// ruleConfig is a CEL rule on a rule kind's spec, or on an object in it.
type ruleConfig struct {
	// Rule is the CEL expression, on the spec or the object. It quotes
	// strings with single quotes: a kubebuilder marker holds it in double
	// quotes.
	Rule string `json:"rule"`
	// Message is what a resource that breaks the rule is refused with.
	Message string `json:"message"`
	// Reason says why the rule is there: what Cloudflare refuses, and when
	// that was verified.
	Reason string `json:"reason"`
	// Invalid is a spec the rule refuses, zoneRef aside: a generated spec
	// checks that the CRD refuses it with Message.
	Invalid map[string]any `json:"invalid"`
}

// typeValue is a type field's value: a lowerCamel name.
var typeValue = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)

func (k *ruleKindConfig) validate() error {
	switch {
	case !kindName.MatchString(k.Kind):
		return fmt.Errorf("kind %q is not a Go type name", k.Kind)
	case strings.Count(k.Rule, ".") == 0 || strings.HasPrefix(k.Rule, ".") || strings.HasSuffix(k.Rule, "."):
		return fmt.Errorf("rule %q is not <package>.<type>", k.Rule)
	case !shortName.MatchString(k.ShortName):
		return fmt.Errorf("shortName %q is not lower-case letters and digits", k.ShortName)
	case strings.TrimSpace(k.Noun) == "":
		return fmt.Errorf("noun is empty")
	case !strings.HasPrefix(k.Doc, k.Kind+" "):
		return fmt.Errorf("doc does not start with %q", k.Kind+" ")
	case (k.Phase == "") == (len(k.Types) == 0):
		return fmt.Errorf("set exactly one of phase and types")
	case len(k.Types) == 1:
		return fmt.Errorf("types lists one type: set phase instead")
	case len(k.Types) > 0 && strings.TrimSpace(k.TypeDoc) == "":
		return fmt.Errorf("typeDoc is empty")
	case len(k.Types) == 0 && k.TypeDoc != "":
		return fmt.Errorf("typeDoc is set without types")
	case len(k.Samples) != max(len(k.Types), 1):
		return fmt.Errorf("give %d samples, one for each type, in their order", max(len(k.Types), 1))
	}
	if err := k.validateTypes(); err != nil {
		return err
	}
	if err := k.validateRules(); err != nil {
		return err
	}
	return k.validateFields()
}

// validateTypes checks a rule kind's types.
func (k *ruleKindConfig) validateTypes() error {
	seen := map[string]bool{}
	for i, t := range k.Types {
		switch {
		case !typeValue.MatchString(t.Name):
			return fmt.Errorf("types[%d]: name %q is not a lowerCamel name", i, t.Name)
		case seen[t.Name]:
			return fmt.Errorf("types[%d]: %s is listed twice", i, t.Name)
		case t.Phase == "":
			return fmt.Errorf("types[%d]: phase is empty", i)
		case strings.TrimSpace(t.Doc) == "":
			return fmt.Errorf("types[%d]: doc is empty", i)
		}
		seen[t.Name] = true
	}
	return nil
}

// validateRules checks a rule kind's CEL rules on the spec.
func (k *ruleKindConfig) validateRules() error {
	for i, r := range k.Rules {
		if err := r.validate(); err != nil {
			return fmt.Errorf("rules[%d]: %w", i, err)
		}
	}
	return nil
}

// validate checks a CEL rule.
func (r ruleConfig) validate() error {
	switch {
	case strings.TrimSpace(r.Rule) == "" || strings.TrimSpace(r.Message) == "":
		return fmt.Errorf("needs a rule and a message")
	case strings.ContainsAny(r.Rule+r.Message, "\"`"):
		return fmt.Errorf("holds a double quote or a backquote; quote strings with single quotes")
	case strings.TrimSpace(r.Reason) == "":
		return fmt.Errorf("gives no reason")
	case len(r.Invalid) == 0:
		return fmt.Errorf("gives no invalid spec")
	}
	return nil
}

// validateFields checks a rule kind's ignored and adjusted fields.
func (k *ruleKindConfig) validateFields() error {
	for name, reason := range k.Ignore {
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("ignore.%s gives no reason", name)
		}
		if _, ok := k.Fields[name]; ok {
			return fmt.Errorf("%s is both ignored and adjusted in fields", name)
		}
	}
	for name, f := range k.Fields {
		if (f.Key == "") != (f.KeyDoc == "") {
			return fmt.Errorf("fields.%s: set key and keyDoc together", name)
		}
		if f.Key != "" && !statusName.MatchString(f.Key) {
			return fmt.Errorf("fields.%s: key %q is not a lowerCamel name", name, f.Key)
		}
		if f.Validation != nil {
			if err := f.Validation.validate(); err != nil {
				return fmt.Errorf("fields.%s.validation: %w", name, err)
			}
		}
		if f.KeyValidation != nil {
			if f.Key == "" {
				return fmt.Errorf("fields.%s: keyValidation without a key", name)
			}
			if err := f.KeyValidation.validate(); err != nil {
				return fmt.Errorf("fields.%s.keyValidation: %w", name, err)
			}
		}
		for i, r := range f.Rules {
			if err := r.validate(); err != nil {
				return fmt.Errorf("fields.%s.rules[%d]: %w", name, i, err)
			}
		}
	}
	return nil
}
