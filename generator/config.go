/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"fmt"
	"go/token"
	"os"
	"regexp"
	"slices"
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
	// SettingsKinds are the kinds whose resources each configure one
	// product's settings in a zone, which the shared settings controller
	// drives.
	SettingsKinds []settingsKindConfig `json:"settingsKinds,omitempty"`
	// HookedKinds are the kinds the shared CRUD reconciler drives whose
	// fields and calls are hand-written. Each is a step on the way to a
	// kindConfig: see hookedKindConfig.
	HookedKinds []hookedKindConfig `json:"hookedKinds,omitempty"`
}

// hookedKindConfig is a kind whose spec and calls cloudflare-go v7 cannot give
// yet: a Worker's upload, a tunnel's configuration, and a zone, whose creation
// the test account cannot verify. kflare-gen writes its API types around
// hand-written extras, its reconciler on the shared CRUD one, its registration
// and its place among the account's dependents. <kind>_hooks.go holds the rest:
// <Kind>API, default<Kind>API, the crudResourceKind <kind>Kind, and <kind>Object
// and <kind>Listed, the types its calls return and list.
type hookedKindConfig struct {
	Kind      string `json:"kind"`
	ShortName string `json:"shortName"`
	Noun      string `json:"noun"`
	Doc       string `json:"doc"`
	// Scope is account, for an accountRef the spec holds, or empty for a kind
	// whose hooks find its account through a resource it references.
	Scope string `json:"scope,omitempty"`
	// ID names where status records the object's ID.
	ID idConfig `json:"id"`
	// ClaimLabel is the label holding the ID of the object a resource manages.
	ClaimLabel string `json:"claimLabel"`
	// SpecExtras, MetadataExtras and StatusExtras, and Columns, as in
	// hooksConfig. The spec holds nothing else, but for its accountRef.
	SpecExtras     string         `json:"specExtras"`
	MetadataExtras string         `json:"metadataExtras,omitempty"`
	StatusExtras   string         `json:"statusExtras,omitempty"`
	Columns        []columnConfig `json:"columns,omitempty"`
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
	// Scope is what a resource of the kind names its parent with: account,
	// an accountRef, for the account_id the service's calls take, or zone, a
	// zoneRef to a Zone in the resource's namespace, for their zone_id.
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
	// Update names the call that changes an object, for a service with both
	// Update and Edit: by default the first, Update. A DNS record's Edit leaves
	// what it is not sent, as its Update does not.
	Update string `json:"update,omitempty"`
	// Dependents are the kinds whose resources use a resource of this kind,
	// which waits for them before it is deleted.
	Dependents []dependentConfig `json:"dependents,omitempty"`
	// DeletionBlockers are Cloudflare's refusals to delete an object that
	// last until someone acts, such as emptying an R2 bucket.
	DeletionBlockers []deletionBlockerConfig `json:"deletionBlockers,omitempty"`
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
	// Hooks says what the kind's hand-written hooks add, for what an entry
	// cannot say.
	Hooks *hooksConfig `json:"hooks,omitempty"`
}

// hooksConfig is what a kind's hooks, a crudHooks in a hand-written
// <kind>_hooks.go in internal/controller, bring besides their functions: the
// descriptor names that crudHooks, <kind>Hooks.
type hooksConfig struct {
	// API adds the hand-written interface <Kind>HooksAPI to the kind's narrow
	// interface: the calls its hooks make besides the service's own. In
	// production the hand-written struct <kind>Service, which embeds the
	// service, implements them; the generated fake embeds the hand-written
	// fake<Kind>HooksAPI that newFake<Kind>HooksAPI returns for its store.
	API bool `json:"api,omitempty"`
	// SpecExtras, MetadataExtras and StatusExtras name hand-written structs
	// in api/v1alpha1 that the spec, status.cloudflareMetadata and status
	// embed inline: fields Cloudflare's API does not describe, which the
	// hooks act on, such as an IPList's items.
	SpecExtras     string `json:"specExtras,omitempty"`
	MetadataExtras string `json:"metadataExtras,omitempty"`
	StatusExtras   string `json:"statusExtras,omitempty"`
	// SampleExtras holds the sample's values of the spec extras, by their
	// JSON name.
	SampleExtras map[string]any `json:"sampleExtras,omitempty"`
	// TableExtras replaces SampleExtras in the envtest table, when set, even
	// to nothing: the shared specs expect a resource to be ready after one
	// reconcile, which an IPList with items is not.
	TableExtras map[string]any `json:"tableExtras,omitempty"`
	// Columns are print columns of fields the extras hold.
	Columns []columnConfig `json:"columns,omitempty"`
	// Params is set when the hooks finish a call's union body: the generated
	// calls pass it to the hand-written <kind>New<Body> and <kind><Update>Body
	// with the resource and what its references give, and the entry may ignore
	// a field the body requires, which those functions then set. The generated
	// fake passes the body the same way to fake<Kind>NewBody and
	// fake<Kind><Update>Body, in the test files.
	Params bool `json:"params,omitempty"`
}

// columnConfig is one of a kind's additional print columns.
type columnConfig struct {
	Name string `json:"name"`
	// Type is the column's type, such as string or integer.
	Type string `json:"type"`
	// Path is the column's JSONPath.
	Path string `json:"path"`
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

// deletionBlockerConfig is a refusal to delete an object that lasts until
// someone acts. A resource whose delete Cloudflare refuses with the code
// keeps its finalizer, reports the reason with the message, and is checked
// again after a minute.
type deletionBlockerConfig struct {
	// Code is the error code Cloudflare refuses the delete with, such as
	// 10008 for an R2 bucket that holds objects.
	Code int `json:"code"`
	// Reason is the Ready condition's reason, such as BucketNotEmpty.
	Reason string `json:"reason"`
	// Message follows the object's noun and key in the condition's message,
	// and says what to do: "still holds objects, which kflare never deletes:
	// empty it". The message ends with how to keep the object instead.
	Message string `json:"message"`
	// Why says how the refusal was verified.
	Why string `json:"why"`
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
	// ReportedAs names, by API name, the field of the object Get returns that
	// reports what a header parameter of New sets, where neither their API
	// names nor their Go names match: the header cf-r2-jurisdiction sets the
	// R2 bucket's jurisdiction. The spec field takes the object field's name.
	ReportedAs string `json:"reportedAs,omitempty"`
	// Ref makes a string field a reference to a resource of another kind, in
	// the resource's namespace: the spec holds <kind>Ref, and kflare sends the
	// referenced resource's field once it is ready. A Worker route's script
	// is a WorkerScript's name.
	Ref *refConfig `json:"ref,omitempty"`
	// Immutable keeps a field Update takes from changing: an object's identity
	// may be fixed even though the API could change it. A DNS record's name and
	// type tell it from the other records of its set.
	Immutable bool `json:"immutable,omitempty"`
	// Clearable is set for an optional string that Cloudflare does not fill
	// in itself, so that unset means empty: it is compared, and Update sends
	// it, even when the spec leaves it unset, which clears Cloudflare's.
	Clearable bool `json:"clearable,omitempty"`
}

// refConfig names what a reference field refers to.
type refConfig struct {
	// Kind is the referenced kind, such as WorkerScript. Its resources use a
	// CloudflareAccount (spec.accountRef) and report a Ready condition.
	Kind string `json:"kind"`
	// Field is the referenced resource's spec field whose value kflare sends,
	// by its JSON name, such as name.
	Field string `json:"field"`
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
	// Integer makes a number field that the spec does not say is an integer
	// one: Cloudflare takes whole numbers only, which a CRD holds as an int64.
	Integer bool `json:"integer,omitempty"`
	// Enum is the values a string field may take, where Cloudflare takes
	// fewer than its API description lists, or the description lists none.
	// It narrows an enum the description has: each value must be one of it.
	Enum []string `json:"enum,omitempty"`
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

// enum returns the values a field may take once the entry's enum replaces
// enum, the values it allows: the entry's, which must be among them when it
// has any, or enum itself when the entry gives none. It also returns the
// values of enum the entry leaves out, which the CRD must refuse too. Only a
// string field (isString) takes an enum from an entry.
func (v *validationConfig) enum(path string, isString bool, enum []string) (values, excluded []string, err error) {
	if v.Enum == nil {
		return enum, nil, nil
	}
	if !isString {
		return nil, nil, fmt.Errorf("fields.%s.validation.enum: only a string field takes an enum", path)
	}
	for _, value := range v.Enum {
		if len(enum) > 0 && !slices.Contains(enum, value) {
			return nil, nil, fmt.Errorf("fields.%s.validation.enum: %q is none of the values the API description "+
				"allows, %s", path, value, strings.Join(enum, ", "))
		}
	}
	for _, value := range enum {
		if !slices.Contains(v.Enum, value) {
			excluded = append(excluded, value)
		}
	}
	return slices.Clone(v.Enum), excluded, nil
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
	if len(c.Kinds) == 0 && len(c.RuleKinds) == 0 && len(c.SettingsKinds) == 0 && len(c.HookedKinds) == 0 {
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
	for i := range c.HookedKinds {
		k := &c.HookedKinds[i]
		if err := k.validate(); err != nil {
			return err
		}
		if seen[k.Kind] {
			return fmt.Errorf("kind %s is listed twice", k.Kind)
		}
		seen[k.Kind] = true
	}
	for i, k := range c.SettingsKinds {
		if err := k.validate(); err != nil {
			return fmt.Errorf("settingsKinds[%d] (%s): %w", i, k.Kind, err)
		}
		if seen[k.Kind] {
			return fmt.Errorf("settingsKinds[%d]: %s is listed twice", i, k.Kind)
		}
		seen[k.Kind] = true
	}
	return nil
}

func (k *kindConfig) validate() error {
	if err := k.Hooks.validate(); err != nil {
		return err
	}
	switch {
	case !kindName.MatchString(k.Kind):
		return fmt.Errorf("kind %q is not a Go type name", k.Kind)
	case strings.Count(k.Service, ".") == 0 || strings.HasPrefix(k.Service, ".") || strings.HasSuffix(k.Service, "."):
		return fmt.Errorf("service %q is not <package>.<type>", k.Service)
	case k.Scope != scopeAccount && k.Scope != scopeZone:
		return fmt.Errorf("scope %q is not supported: it is account or zone", k.Scope)
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
	if err := validateDeletionBlockers(k.DeletionBlockers); err != nil {
		return err
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
	return validateFields(k.Fields)
}

// validateFields checks a kind's field adjustments: their validations and
// references.
func validateFields(fields map[string]fieldConfig) error {
	for name, f := range fields {
		if f.Validation != nil {
			if err := f.Validation.validate(); err != nil {
				return fmt.Errorf("fields.%s.validation: %w", name, err)
			}
		}
		if err := f.validateRef(name); err != nil {
			return err
		}
	}
	return nil
}

// validateRef checks a field's reference: a kind and a field it names, and
// nothing that applies to the value itself, which the referenced resource
// gives.
func (f fieldConfig) validateRef(name string) error {
	switch {
	case f.Ref == nil:
		return nil
	case !kindName.MatchString(f.Ref.Kind):
		return fmt.Errorf("fields.%s.ref: kind %q is not a Go type name", name, f.Ref.Kind)
	case !statusName.MatchString(f.Ref.Field):
		return fmt.Errorf("fields.%s.ref: field %q is not a lowerCamel name", name, f.Ref.Field)
	case f.Validation != nil:
		return fmt.Errorf("fields.%s: a reference takes no validation: the referenced resource gives its value", name)
	case f.ReportedAs != "":
		return fmt.Errorf("fields.%s: a reference takes no reportedAs", name)
	}
	return nil
}

// The scopes of a CRUD kind: what its resources name their parent with.
const (
	scopeAccount = "account"
	scopeZone    = "zone"

	// accountRefName and accountRefField are the Go and JSON names of the spec
	// field an account's kind names its account with.
	accountRefName  = "AccountRef"
	accountRefField = "accountRef"
)

// validateDeletionBlockers checks a kind's deletion blockers.
func validateDeletionBlockers(blockers []deletionBlockerConfig) error {
	seen := map[int]bool{}
	for i, b := range blockers {
		switch {
		case b.Code <= 0:
			return fmt.Errorf("deletionBlockers[%d]: code %d is not a Cloudflare error code", i, b.Code)
		case seen[b.Code]:
			return fmt.Errorf("deletionBlockers[%d]: code %d is listed twice", i, b.Code)
		case !kindName.MatchString(b.Reason):
			return fmt.Errorf("deletionBlockers[%d]: reason %q is not a CamelCase condition reason", i, b.Reason)
		case strings.TrimSpace(b.Message) == "":
			return fmt.Errorf("deletionBlockers[%d]: gives no message", i)
		case strings.TrimSpace(b.Why) == "":
			return fmt.Errorf("deletionBlockers[%d]: does not say why", i)
		}
		seen[b.Code] = true
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
	case v.Integer && (v.MinLength != nil || v.MaxLength != nil || v.Pattern != ""):
		return fmt.Errorf("integer takes no lengths or pattern, which bound a string")
	case v.Enum != nil && (v.MinLength != nil || v.MaxLength != nil || v.Pattern != "" || v.Minimum != nil ||
		v.Maximum != nil || v.Integer):
		return fmt.Errorf("an enum takes no lengths, pattern, range or integer: its values say it all")
	}
	seen := map[string]bool{}
	for _, value := range v.Enum {
		switch {
		case value == "":
			return fmt.Errorf("the enum holds an empty value")
		case seen[value]:
			return fmt.Errorf("the enum lists %q twice", value)
		case strings.ContainsAny(value, ";\"`"):
			return fmt.Errorf("the enum value %q holds a semicolon or a quote, which a kubebuilder marker cannot", value)
		}
		seen[value] = true
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
	Rule string `json:"rule,omitempty"`
	// Actions, for a kind whose rules take one of several actions in place of
	// Rule, are the values of the spec field action, each with its rule type.
	Actions []ruleActionConfig `json:"actions,omitempty"`
	// ActionDoc documents the action field, before the list of its values.
	ActionDoc string `json:"actionDoc,omitempty"`
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
	// Conflicts are the Cloudflare error codes that mean another rule holds
	// what the kind's rule needs, each with the condition reason to report.
	Conflicts []ruleConflictConfig `json:"conflicts,omitempty"`
}

// ruleActionConfig is one value of a rule kind's action field.
type ruleActionConfig struct {
	// Action is the action, as the Rulesets API names it, such as block.
	Action string `json:"action"`
	// Rule is cloudflare-go v7's type for a rule with the action, as
	// <package>.<type>: rulesets.BlockRuleParam. An action v7 has no type for,
	// such as challenge, leaves it out, and must take no action parameters.
	Rule string `json:"rule,omitempty"`
	// Doc says what a rule with the action does, for the action field's doc.
	Doc string `json:"doc"`
}

// ruleConflictConfig is a Cloudflare error code that means another rule holds
// what a rule kind's rule needs. A resource that gets it reports Reason and
// checks again every minute, since the other rule may be about to go.
type ruleConflictConfig struct {
	// Code is Cloudflare's error code, such as 20014.
	Code int `json:"code"`
	// Reason is the Ready condition's reason, such as RulesetConflict.
	Reason string `json:"reason"`
	// Why says what Cloudflare refuses with the code, and when that was
	// verified.
	Why string `json:"why"`
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
	// PrintColumn adds a column for the field, with this name: a rule kind's
	// string, integer or boolean field outside a list or a map.
	PrintColumn string `json:"printColumn,omitempty"`
	// OmitZero, for a rule kind's optional boolean or integer, says how it was
	// verified that Cloudflare stores the field's zero value (false, 0) as
	// absent and reads an absent field as that value: the conversion sends
	// the field only when it is not zero, which is the rule Cloudflare
	// returns.
	OmitZero string `json:"omitZero,omitempty"`
	// Key, for a map, names the field that holds an entry's key: the spec
	// lists the map's values, each with its key in this field, keyed by it.
	Key string `json:"key,omitempty"`
	// KeyDoc documents the key field.
	KeyDoc string `json:"keyDoc,omitempty"`
	// KeyValidation replaces the key field's validation, a minimum length of
	// one: what Cloudflare takes as a key.
	KeyValidation *validationConfig `json:"keyValidation,omitempty"`
	// ListMapKey, for a list of objects, names the field of its items, by API
	// name, that no two items share: the list is keyed by it (listType=map),
	// and the API server refuses two items with the same key.
	ListMapKey string `json:"listMapKey,omitempty"`
	// Validation replaces what the OpenAPI spec says the field's value may
	// be.
	Validation *validationConfig `json:"validation,omitempty"`
	// Rules are CEL rules on the object at the path, such as browser_ttl, or
	// on an item of a list or map, such as vary.headers[]: what Cloudflare
	// refuses that its API description does not say.
	Rules []ruleConfig `json:"rules,omitempty"`
	// PickedBy, for a setting of a settings kind whose settings go by their
	// IDs, says that Cloudflare picks its value itself while another setting
	// holds a value.
	PickedBy *pickedByConfig `json:"pickedBy,omitempty"`
}

// pickedByConfig says that Cloudflare picks a setting's value itself while
// another setting holds a value, as it picks a zone's SSL/TLS encryption mode
// (ssl) while Automatic SSL/TLS is on (ssl_automatic_mode auto). A zone whose
// other setting holds While has not configured the setting, whatever its
// value, and a reset writes While to the other setting, handing the value back
// to Cloudflare, rather than writing the setting.
type pickedByConfig struct {
	// Setting is the other setting's ID.
	Setting string `json:"setting"`
	// While is the other setting's value while Cloudflare picks the setting's,
	// and Written its value once the setting is written.
	While   string `json:"while"`
	Written string `json:"written"`
	// Reason says how that was verified.
	Reason string `json:"reason"`
}

// settingID matches a zone setting's ID, such as ssl_automatic_mode.
var settingID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// validate checks a pickedBy.
func (p *pickedByConfig) validate() error {
	switch {
	case !settingID.MatchString(p.Setting):
		return fmt.Errorf("setting %q is not a setting's ID", p.Setting)
	case p.While == "" || p.Written == "":
		return fmt.Errorf("give while and written, the other setting's values")
	case p.While == p.Written:
		return fmt.Errorf("while and written are both %q: writing the setting would not show", p.While)
	case strings.TrimSpace(p.Reason) == "":
		return fmt.Errorf("gives no reason")
	}
	return nil
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

// actionName is a Rulesets action's name, such as managed_challenge.
var actionName = regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)

// isTypeName reports whether s names a type as <package>.<type>.
func isTypeName(s string) bool {
	return strings.Count(s, ".") > 0 && !strings.HasPrefix(s, ".") && !strings.HasSuffix(s, ".")
}

func (k *ruleKindConfig) validate() error {
	switch {
	case !kindName.MatchString(k.Kind):
		return fmt.Errorf("kind %q is not a Go type name", k.Kind)
	case (k.Rule == "") == (len(k.Actions) == 0):
		return fmt.Errorf("set exactly one of rule and actions")
	case k.Rule != "" && !isTypeName(k.Rule):
		return fmt.Errorf("rule %q is not <package>.<type>", k.Rule)
	case len(k.Actions) == 1:
		return fmt.Errorf("actions lists one action: set rule instead")
	case len(k.Actions) > 0 && strings.TrimSpace(k.ActionDoc) == "":
		return fmt.Errorf("actionDoc is empty")
	case len(k.Actions) == 0 && k.ActionDoc != "":
		return fmt.Errorf("actionDoc is set without actions")
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
	if err := k.validateActions(); err != nil {
		return err
	}
	if err := k.validateConflicts(); err != nil {
		return err
	}
	if err := k.validateRules(); err != nil {
		return err
	}
	return k.validateFields()
}

// validateActions checks a rule kind's actions. At least one names its rule
// type, which gives the rule's own fields.
func (k *ruleKindConfig) validateActions() error {
	seen := map[string]bool{}
	typed := false
	for i, a := range k.Actions {
		switch {
		case !actionName.MatchString(a.Action):
			return fmt.Errorf("actions[%d]: action %q is not an action's name", i, a.Action)
		case seen[a.Action]:
			return fmt.Errorf("actions[%d]: %s is listed twice", i, a.Action)
		case a.Rule != "" && !isTypeName(a.Rule):
			return fmt.Errorf("actions[%d]: rule %q is not <package>.<type>", i, a.Rule)
		case strings.TrimSpace(a.Doc) == "":
			return fmt.Errorf("actions[%d]: doc is empty", i)
		}
		seen[a.Action] = true
		typed = typed || a.Rule != ""
	}
	if len(k.Actions) > 0 && !typed {
		return fmt.Errorf("actions: none names its rule type")
	}
	return nil
}

// validateConflicts checks a rule kind's conflicts.
func (k *ruleKindConfig) validateConflicts() error {
	seen := map[int]bool{}
	for i, c := range k.Conflicts {
		switch {
		case c.Code <= 0:
			return fmt.Errorf("conflicts[%d]: code %d is not a Cloudflare error code", i, c.Code)
		case seen[c.Code]:
			return fmt.Errorf("conflicts[%d]: code %d is listed twice", i, c.Code)
		case !kindName.MatchString(c.Reason):
			return fmt.Errorf("conflicts[%d]: reason %q is not an UpperCamel condition reason", i, c.Reason)
		case strings.TrimSpace(c.Why) == "":
			return fmt.Errorf("conflicts[%d]: why is empty", i)
		}
		seen[c.Code] = true
	}
	return nil
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
	return validateRules(k.Rules)
}

// validateRules checks an entry's CEL rules on the spec.
func validateRules(rules []ruleConfig) error {
	for i, r := range rules {
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
	for name, f := range k.Fields {
		if f.PickedBy != nil {
			return fmt.Errorf("fields.%s.pickedBy: only a setting a settings kind writes by its ID is picked", name)
		}
	}
	return validateParamFields(k.Fields, k.Ignore)
}

// validateParamFields checks the ignored and adjusted fields of an entry
// whose spec holds a tree of params: a rule kind's or a settings kind's.
func validateParamFields(fields map[string]ruleFieldConfig, ignore map[string]string) error {
	for name, reason := range ignore {
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("ignore.%s gives no reason", name)
		}
		if _, ok := fields[name]; ok {
			return fmt.Errorf("%s is both ignored and adjusted in fields", name)
		}
	}
	for name, f := range fields {
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
		if f.ListMapKey != "" && f.Key != "" {
			return fmt.Errorf("fields.%s: set key for a map or listMapKey for a list, not both", name)
		}
		if f.OmitZero != "" && strings.TrimSpace(f.OmitZero) == "" {
			return fmt.Errorf("fields.%s.omitZero gives no reason", name)
		}
		if f.PickedBy != nil {
			if err := f.PickedBy.validate(); err != nil {
				return fmt.Errorf("fields.%s.pickedBy: %w", name, err)
			}
		}
	}
	return nil
}

// settingsKindConfig is the entry of a settings kind: a kind whose resources
// each configure one product's settings in a zone, such as its managed
// transforms, through the shared settings controller. The service's write
// (Update or Edit) gives the settings, which kflare-gen builds from
// cloudflare-go v7's param types: every field of them becomes a field of the
// spec (fields are opt-out), except those the entry ignores.
type settingsKindConfig struct {
	// Kind is the API type's name, such as ManagedTransform.
	Kind string `json:"kind"`
	// Service is the cloudflare-go v7 service the kind calls, as
	// <package>.<type>: managed_transforms.ManagedTransformService. Its read
	// (Get, or List where it returns the settings), its write (Update or
	// Edit) and its reset (Delete) each take the zone's ID, and nothing else
	// kflare does not send.
	Service string `json:"service"`
	// ShortName is the kind's short name for kubectl, such as
	// cfmanagedtransform.
	ShortName string `json:"shortName"`
	// Noun is what docs and messages call the settings, such as "managed
	// transforms".
	Noun string `json:"noun"`
	// Doc is the kind's documentation, from the type's name on.
	Doc string `json:"doc"`
	// Sample is a valid spec, zoneRef aside, that configures the settings:
	// config/samples holds it, and the generated specs start from it.
	Sample map[string]any `json:"sample"`
	// Tests are more valid specs, which the generated specs write and edit as
	// they do the sample, for fields it does not set.
	Tests []map[string]any `json:"tests,omitempty"`
	// Fields adjusts fields of the settings, by their path of API names.
	Fields map[string]ruleFieldConfig `json:"fields,omitempty"`
	// Rules are CEL rules on the spec, for what Cloudflare refuses that its
	// API description does not say.
	Rules []ruleConfig `json:"rules,omitempty"`
	// Unconfigured says what the settings hold in a zone nobody has
	// configured.
	Unconfigured unconfiguredConfig `json:"unconfigured"`
	// Ignore leaves out fields of the settings, by path, each with the
	// reason.
	Ignore map[string]string `json:"ignore,omitempty"`
}

// unconfiguredConfig says what a kind's settings hold in a zone nobody has
// configured: a new zone, or one whose settings were reset.
type unconfiguredConfig struct {
	// Values are those settings' values, by path: the API names of the
	// fields to each, joined by dots, a list's items marked by [] after its
	// name, as in managed_request_headers[].enabled for every item's. Settings
	// that hold another value at one of these paths were configured outside
	// kflare, and a resource takes them over only as its adoption policy
	// allows.
	Values map[string]any `json:"values"`
	// Reason says how that was verified.
	Reason string `json:"reason"`
}

func (k *settingsKindConfig) validate() error {
	switch {
	case !kindName.MatchString(k.Kind):
		return fmt.Errorf("kind %q is not a Go type name", k.Kind)
	case strings.Count(k.Service, ".") == 0 || strings.HasPrefix(k.Service, ".") || strings.HasSuffix(k.Service, "."):
		return fmt.Errorf("service %q is not <package>.<type>", k.Service)
	case !shortName.MatchString(k.ShortName):
		return fmt.Errorf("shortName %q is not lower-case letters and digits", k.ShortName)
	case strings.TrimSpace(k.Noun) == "":
		return fmt.Errorf("noun is empty")
	case !strings.HasPrefix(k.Doc, k.Kind+" "):
		return fmt.Errorf("doc does not start with %q", k.Kind+" ")
	case len(k.Sample) == 0:
		return fmt.Errorf("sample is empty")
	case len(k.Unconfigured.Values) == 0:
		return fmt.Errorf("unconfigured.values is empty: say what a zone nobody has configured holds")
	case strings.TrimSpace(k.Unconfigured.Reason) == "":
		return fmt.Errorf("unconfigured gives no reason")
	}
	for path := range k.Unconfigured.Values {
		if !settingsValuePath.MatchString(path) {
			return fmt.Errorf("unconfigured.values: %q is not a path of API names, such as a.b[].c", path)
		}
	}
	if err := validateRules(k.Rules); err != nil {
		return err
	}
	for name, f := range k.Fields {
		if f.PrintColumn != "" {
			return fmt.Errorf("fields.%s.printColumn: only a rule kind's fields have columns", name)
		}
		if f.OmitZero != "" {
			return fmt.Errorf("fields.%s.omitZero: only a rule kind's fields leave out a zero value", name)
		}
	}
	return validateParamFields(k.Fields, k.Ignore)
}

// settingsValuePath matches a path of API names, each list's marked by [].
var settingsValuePath = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\[\])?(\.[a-z][a-z0-9_-]*(\[\])?)*$`)

// validate checks a hooked kind's entry.
func (k *hookedKindConfig) validate() error {
	switch {
	case !kindName.MatchString(k.Kind):
		return fmt.Errorf("hookedKinds: %q is not a Go type name", k.Kind)
	case k.ShortName == "" || k.Noun == "" || k.ClaimLabel == "" || k.ID.Status == "":
		return fmt.Errorf("hookedKinds (%s): shortName, noun, claimLabel and id.status are required", k.Kind)
	case !strings.HasPrefix(k.Doc, k.Kind+" "):
		return fmt.Errorf("hookedKinds (%s): doc does not start with %q", k.Kind, k.Kind+" ")
	case k.Scope != "" && k.Scope != scopeAccount:
		return fmt.Errorf("hookedKinds (%s): scope %q: it is account, or empty", k.Kind, k.Scope)
	}
	hooks := hooksConfig{SpecExtras: k.SpecExtras, MetadataExtras: k.MetadataExtras, StatusExtras: k.StatusExtras,
		Columns: k.Columns}
	if k.SpecExtras == "" {
		return fmt.Errorf("hookedKinds (%s): specExtras is required", k.Kind)
	}
	return hooks.validate()
}

// chooseUpdate makes the call the entry names the one that changes an object.
func (k *kindConfig) chooseUpdate(svc *service) error {
	if k.Update == "" {
		return nil
	}
	if svc.calls[k.Update] == nil || !slices.Contains(writeMethods, k.Update) {
		return fmt.Errorf("%s: update: %s is not a call that changes an object of %s", k.Kind, k.Update, k.Service)
	}
	svc.chooseUpdate(k.Update)
	return nil
}

// params reports whether a kind's hooks finish its calls' union bodies.
func (h *hooksConfig) params() bool { return h != nil && h.Params }

// validate checks the names and columns of a kind's hooks; a kind may have
// none.
func (h *hooksConfig) validate() error {
	if h == nil {
		return nil
	}
	for what, name := range map[string]string{"specExtras": h.SpecExtras, "metadataExtras": h.MetadataExtras,
		"statusExtras": h.StatusExtras} {
		if name != "" && !token.IsExported(name) {
			return fmt.Errorf("hooks.%s: %q is no exported type's name", what, name)
		}
	}
	if len(h.SampleExtras) > 0 && h.SpecExtras == "" {
		return fmt.Errorf("hooks.sampleExtras: the spec has no extras")
	}
	for _, c := range h.Columns {
		if c.Name == "" || c.Type == "" || c.Path == "" {
			return fmt.Errorf("hooks.columns: a column takes a name, a type and a path")
		}
	}
	return nil
}
