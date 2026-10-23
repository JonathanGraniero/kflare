/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ruleKind is the model of a generated rule kind: what its types file, its
// controller, its envtest table and its sample hold. Its paramKind's params
// are the action parameters, and the bodies, the changes and the full specs
// ask for a rule, as Cloudflare stores it, its ref aside.
type ruleKind struct {
	paramKind
	shortName, noun, plural string
	doc                     []string
	// varName starts the kind's unexported names, as in transformRuleKind.
	varName string
	rule    *sdkRule
	// actionConst names the RulesetNewParamsRulesAction constant of the
	// kind's action.
	actionConst string
	// phases are the kind's phases: one per type, in the order of the types,
	// or one.
	phases []rulePhase
	// typeDoc documents the type field; nil for a kind with one phase.
	typeDoc []string
	// retypes are specs a resource of the first sample may not change to.
	retypes      []ruleEdit
	printColumns []printColumn
}

// rulePhase is a phase of a rule kind, and the type that picks it.
type rulePhase struct {
	// typeName is the value of the type field; "" for a kind with one phase.
	typeName string
	phase    string
	// constant names the rulesets.Phase constant of the phase.
	constant string
	doc      string
}

// ruleEdit is a spec a generated spec writes, and what it checks.
type ruleEdit struct {
	what string
	spec map[string]any
	// sample is the sample a change edits.
	sample int
	// rule is the rule a change asks for; message what the CRD refuses an
	// invalid spec with.
	rule    map[string]any
	message string
}

// The names of the spec fields every rule kind's spec holds (RulesetRuleSpec's,
// zoneRef aside) and of its type field, and the API names of the rule fields
// kflare-gen reads.
const (
	fieldType            = "type"
	fieldExpression      = "expression"
	fieldDescription     = "description"
	fieldEnabled         = "enabled"
	fieldPriority        = "priority"
	ruleAction           = "action"
	ruleActionParameters = "action_parameters"
)

// actionParametersRoot is what messages call a rule kind's params.
const actionParametersRoot = "the action parameters"

// sharedSpecFields are the spec fields a sample sets besides the action
// parameters, in the order a sample lists them. zoneRef a generated spec
// fills in.
var sharedSpecFields = []string{fieldType, fieldExpression, fieldDescription, fieldEnabled, fieldPriority}

// sharedRuleFields are the fields of the rule type that every rule kind maps,
// through RulesetRuleSpec and the shared rule controller.
var sharedRuleFields = []string{"id", "ref", ruleAction, fieldExpression, fieldDescription, fieldEnabled}

// buildRuleKind builds the model of entry's kind from its rule type and the
// OpenAPI spec.
func buildRuleKind(entry ruleKindConfig, rule *sdkRule, spec *apiSpec) (*ruleKind, error) {
	k, err := (&ruleKindBuilder{entry: entry, rule: rule, spec: spec}).build()
	if err != nil {
		return nil, fmt.Errorf("%s (%s): %w", entry.Kind, entry.Rule, err)
	}
	return k, nil
}

type ruleKindBuilder struct {
	entry ruleKindConfig
	rule  *sdkRule
	spec  *apiSpec
}

func (b *ruleKindBuilder) build() (*ruleKind, error) {
	e, rule := b.entry, b.rule
	k := &ruleKind{
		paramKind: paramKind{name: e.Kind, root: actionParametersRoot},
		shortName: e.ShortName, noun: e.Noun, plural: plural(e.Kind), doc: lines(e.Doc),
		varName: lowerCamel(e.Kind), rule: rule,
	}
	k.check, k.body = k.checkSpec, k.apiRule
	var err error
	if k.actionConst, err = rule.r.constantNamed("RulesetNewParamsRulesAction", rule.action); err != nil {
		return nil, err
	}
	if err := b.phases(k); err != nil {
		return nil, err
	}
	paramsSchema, err := b.paramsSchema()
	if err != nil {
		return nil, err
	}

	used := map[string]bool{}
	for _, f := range rule.fields {
		if slices.Contains(sharedRuleFields, f.apiName) || f.apiName == ruleActionParameters {
			continue
		}
		if _, ok := e.Ignore[f.apiName]; !ok {
			return nil, fmt.Errorf("the rule type has a field %s, which rule kinds do not map; ignore it with a reason",
				f.apiName)
		}
		used[f.apiName] = true
	}
	pb := &paramBuilder{kind: e.Kind, r: rule.r, spec: b.spec, adjust: e.Fields, ignore: e.Ignore, used: used,
		root: actionParametersRoot}
	if k.params, err = pb.object("", e.Kind+"Spec", e.Kind, rule.params, paramsSchema); err != nil {
		return nil, err
	}
	k.structs = pb.structs
	for _, section := range []map[string]bool{keySet(e.Fields), keySet(e.Ignore)} {
		for _, path := range slices.Sorted(maps.Keys(section)) {
			if !used[path] {
				return nil, fmt.Errorf("%s: the rule type has no such field", path)
			}
		}
	}
	for _, f := range k.params.fields {
		if f.jsonName == "zoneRef" || slices.Contains(sharedSpecFields, f.jsonName) {
			return nil, fmt.Errorf("the action parameters have a field %s, which the spec already holds", f.jsonName)
		}
	}

	k.specRules = append(k.specRules, k.params.rules...)
	for _, r := range e.Rules {
		k.specRules = append(k.specRules, celRule{rule: r.Rule, message: r.Message})
	}
	if k.typeDoc != nil {
		k.printColumns = append(k.printColumns, printColumn{name: "Type", typ: typeString, jsonPath: ".spec.type"})
	}
	k.printColumns = append(k.printColumns,
		printColumn{name: "Priority", typ: typeInteger, jsonPath: ".spec.priority"},
		printColumn{name: "Rule ID", typ: typeString, jsonPath: ".status.cloudflareMetadata.ruleID"},
		readyColumn, ageColumn)

	if k.cel, err = newCELChecker(); err != nil {
		return nil, err
	}
	// A sample's type is the one at its place in the types; a test's is the
	// one it sets, which must be one of them.
	err = k.addSpecs(e.Samples, e.Tests, func(sample bool, i int, spec map[string]any) (string, error) {
		if sample {
			return k.phases[i].typeName, nil
		}
		typeName, _ := spec[fieldType].(string)
		if k.typeDoc != nil && !slices.ContainsFunc(k.phases, func(p rulePhase) bool { return p.typeName == typeName }) {
			return "", fmt.Errorf("type %v is none of the kind's types", spec[fieldType])
		}
		return typeName, nil
	})
	if err != nil {
		return nil, err
	}
	if err := k.edits(entryRules(e.Rules, e.Fields)); err != nil {
		return nil, err
	}
	return k, nil
}

// edits builds the generated specs' edits of the samples: a rule kind's own,
// its description and enabled changed and the specs of its other types,
// around the edits of its action parameters.
func (k *ruleKind) edits(rules []entryRule) error {
	e := k.editor()
	e.change(0, "a new description", func(spec map[string]any) { spec[fieldDescription] = "changed by kflare-gen" })
	if enabled, ok := k.samples[0][fieldEnabled].(bool); ok && !enabled {
		e.change(0, "the rule enabled", func(spec map[string]any) { spec[fieldEnabled] = true })
	} else {
		e.change(0, "the rule disabled", func(spec map[string]any) { spec[fieldEnabled] = false })
	}
	k.scalarChanges(e, false)
	if e.failed != nil {
		return e.failed
	}
	values := k.sampleValues()
	k.swaps(values, e.change)
	if e.failed != nil {
		return e.failed
	}

	for i := 1; i < len(k.samples); i++ {
		k.retypes = append(k.retypes, ruleEdit{what: "type " + k.phases[i].typeName, spec: copySpec(k.samples[i]),
			message: "type is immutable"})
	}

	if k.typeDoc != nil {
		spec := copySpec(k.samples[0])
		spec[fieldType] = "kflare-not-a-type"
		k.invalid = append(k.invalid, ruleEdit{what: "a type outside its values", spec: spec,
			message: "spec.type: Unsupported value"})
	}
	if err := k.invalidValues(); err != nil {
		return err
	}
	if err := k.ruleBreaches(values); err != nil {
		return err
	}
	if err := k.entryInvalid(rules); err != nil {
		return err
	}
	shared := map[string]any{}
	for _, name := range sharedSpecFields {
		if value, ok := k.samples[0][name]; ok {
			shared[name] = value
		}
	}
	return k.fullSpecs(values, shared, k.phases[0].typeName)
}

// phaseOf returns the phase of a checked spec's rule: its type's, or the
// kind's only one.
func (k *ruleKind) phaseOf(spec map[string]any) rulePhase {
	for _, p := range k.phases {
		if p.typeName != "" && p.typeName == spec[fieldType] {
			return p
		}
	}
	return k.phases[0]
}

// entryRule is a CEL rule of an entry, with where the entry gives it.
type entryRule struct {
	where string
	ruleConfig
}

// entryRules returns an entry's CEL rules: those on the spec, then those on
// objects of its fields, by path.
func entryRules(rules []ruleConfig, fields map[string]ruleFieldConfig) []entryRule {
	out := make([]entryRule, 0, len(rules))
	for i, r := range rules {
		out = append(out, entryRule{where: fmt.Sprintf("rules[%d]", i), ruleConfig: r})
	}
	for _, path := range slices.Sorted(maps.Keys(fields)) {
		for i, r := range fields[path].Rules {
			out = append(out, entryRule{where: fmt.Sprintf("fields.%s.rules[%d]", path, i), ruleConfig: r})
		}
	}
	return out
}

// keySet returns a map's keys as a set.
func keySet[V any](m map[string]V) map[string]bool {
	out := map[string]bool{}
	for key := range m {
		out[key] = true
	}
	return out
}

// phases reads the kind's phases: its one phase, or one per type.
func (b *ruleKindBuilder) phases(k *ruleKind) error {
	e := b.entry
	if e.Phase != "" {
		constant, err := b.rule.r.constantNamed("Phase", e.Phase)
		if err != nil {
			return err
		}
		k.phases = []rulePhase{{phase: e.Phase, constant: constant}}
		return nil
	}
	for _, t := range e.Types {
		constant, err := b.rule.r.constantNamed("Phase", t.Phase)
		if err != nil {
			return fmt.Errorf("types: %w", err)
		}
		k.phases = append(k.phases, rulePhase{typeName: t.Name, phase: t.Phase, constant: constant, doc: t.Doc})
	}
	k.typeDoc = lines(e.TypeDoc)
	return nil
}

// ruleSchemaName returns the name of the OpenAPI schema of a rule type: the
// spec names it as cloudflare-go v7 names the type, rulesets_RewriteRule for
// rulesets.RewriteRuleParam.
func ruleSchemaName(rule string) string {
	return "rulesets_" + strings.TrimSuffix(rule[strings.LastIndex(rule, ".")+1:], "Param")
}

// paramsSchema returns the OpenAPI schema of the rule's action parameters. The
// schema's action must be the rule type's.
func (b *ruleKindBuilder) paramsSchema() (*schema, error) {
	name := ruleSchemaName(b.rule.name)
	component, ok := b.spec.Components.Schemas[name]
	if !ok {
		return nil, fmt.Errorf("the OpenAPI subset has no schema %s: run make sync-spec", name)
	}
	actions, err := b.actionsOf(component)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(actions, []string{b.rule.action}) {
		return nil, fmt.Errorf("%s's actions are %v, not the rule type's %s", name, actions, b.rule.action)
	}
	props, err := b.spec.properties(component)
	if err != nil {
		return nil, err
	}
	params, ok := props[ruleActionParameters]
	if !ok {
		return nil, fmt.Errorf("%s has no action_parameters", name)
	}
	return params, nil
}

// actionsOf returns the actions a rule schema allows: the enum of the action
// property of one of the schemas it combines with allOf.
func (b *ruleKindBuilder) actionsOf(sch *schema) ([]string, error) {
	sch, err := b.spec.resolve(sch)
	if err != nil {
		return nil, err
	}
	if action, ok := sch.Properties[ruleAction]; ok {
		fs, err := b.spec.fieldSpec(action)
		if err != nil {
			return nil, err
		}
		if len(fs.enum) > 0 {
			return fs.enum, nil
		}
	}
	for _, part := range sch.AllOf {
		actions, err := b.actionsOf(part)
		if err != nil || actions != nil {
			return actions, err
		}
	}
	return nil, nil
}

// checkSpec checks a sample, or another spec of the kind, against the model,
// and returns it with its integers as int64. typeName is the type it must
// have; "" for a kind with one phase.
func (k *ruleKind) checkSpec(spec map[string]any, typeName string) (map[string]any, error) {
	out := map[string]any{}
	rest := map[string]any{}
	for key, value := range spec {
		switch {
		case key == fieldType && k.typeDoc != nil:
			if value != typeName {
				return nil, fmt.Errorf("type is %v, not %s", value, typeName)
			}
			out[key] = value
		case key == fieldExpression || key == fieldDescription:
			s, ok := value.(string)
			if !ok || (key == fieldExpression && s == "") {
				return nil, fmt.Errorf("%s is not a non-empty string", key)
			}
			out[key] = s
		case key == fieldEnabled:
			if _, ok := value.(bool); !ok {
				return nil, fmt.Errorf("enabled is not a bool")
			}
			out[key] = value
		case key == fieldPriority:
			n, ok := wholeNumber(value)
			if !ok {
				return nil, fmt.Errorf("priority is not a whole number")
			}
			out[key] = n
		default:
			rest[key] = value
		}
	}
	if _, ok := out[fieldExpression]; !ok {
		return nil, fmt.Errorf("sets no expression")
	}
	if _, ok := out[fieldType]; !ok && k.typeDoc != nil {
		return nil, fmt.Errorf("sets no type")
	}
	params, err := checkStruct(k.params, rest, "spec")
	if err != nil {
		return nil, err
	}
	maps.Copy(out, params)
	return out, nil
}

// wholeNumber returns a number a sample holds as an int64, when it is one.
func wholeNumber(value any) (int64, bool) {
	switch n := value.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		if n == float64(int64(n)) {
			return int64(n), true
		}
	}
	return 0, false
}

// checkStruct checks an object's value against its struct.
func checkStruct(st *paramStruct, value map[string]any, path string) (map[string]any, error) {
	out := map[string]any{}
	for key := range value {
		if !slices.ContainsFunc(st.fields, func(f *paramField) bool { return f.jsonName == key }) {
			return nil, fmt.Errorf("%s has no field %s", path, key)
		}
	}
	for _, f := range st.fields {
		v, ok := value[f.jsonName]
		if !ok {
			if f.required {
				return nil, fmt.Errorf("%s.%s is required", path, f.jsonName)
			}
			continue
		}
		checked, err := checkNode(f.node, v, path+"."+f.jsonName)
		if err != nil {
			return nil, err
		}
		out[f.jsonName] = checked
	}
	return out, nil
}

// checkNode checks a field's value against its type.
func checkNode(node paramNode, value any, path string) (any, error) {
	switch n := node.(type) {
	case *paramScalar:
		return checkScalar(n, value, path)
	case *paramStruct:
		m, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s is not an object", path)
		}
		return checkStruct(n, m, path)
	case *paramList:
		items, ok := value.([]any)
		if !ok || len(items) == 0 {
			return nil, fmt.Errorf("%s is not a list with items", path)
		}
		out := make([]any, len(items))
		keys := map[any]bool{}
		for i, item := range items {
			checked, err := checkNode(n.items, item, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			if n.key != nil {
				key := checked.(map[string]any)[n.key.jsonName]
				if keys[key] {
					return nil, fmt.Errorf("%s lists %v twice", path, key)
				}
				keys[key] = true
			}
			out[i] = checked
		}
		return out, nil
	case *paramMap:
		items, ok := value.([]any)
		if !ok || len(items) == 0 {
			return nil, fmt.Errorf("%s is not a list with items", path)
		}
		out := make([]any, len(items))
		keys := map[any]bool{}
		for i, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s[%d] is not an object", path, i)
			}
			checked, err := checkStruct(n.value, m, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			key := checked[n.value.fields[0].jsonName]
			if keys[key] {
				return nil, fmt.Errorf("%s lists %v twice", path, key)
			}
			keys[key] = true
			out[i] = checked
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s: unknown type", path)
}

// checkScalar checks a scalar's value against its type and validation.
func checkScalar(n *paramScalar, value any, path string) (any, error) {
	switch n.goType {
	case typeBool:
		b, ok := value.(bool)
		switch {
		case !ok:
			return nil, fmt.Errorf("%s is not a bool", path)
		case len(n.enum) > 0 && !slices.Contains(n.enum, strconv.FormatBool(b)):
			return nil, fmt.Errorf("%s is %t, not one of %s", path, b, strings.Join(n.enum, ", "))
		}
		return value, nil
	case typeInt64:
		i, ok := wholeNumber(value)
		v := n.validation
		switch {
		case !ok:
			return nil, fmt.Errorf("%s is not a whole number", path)
		case len(n.enum) > 0 && !slices.Contains(n.enum, strconv.FormatInt(i, 10)):
			return nil, fmt.Errorf("%s is %d, not one of %s", path, i, strings.Join(n.enum, ", "))
		case v.minimum != nil && (float64(i) < *v.minimum || v.exclusiveMinimum && float64(i) == *v.minimum):
			return nil, fmt.Errorf("%s is below its minimum", path)
		case v.maximum != nil && (float64(i) > *v.maximum || v.exclusiveMaximum && float64(i) == *v.maximum):
			return nil, fmt.Errorf("%s is above its maximum", path)
		}
		return i, nil
	}
	s, ok := value.(string)
	v := n.validation
	switch {
	case !ok:
		return nil, fmt.Errorf("%s is not a string", path)
	case len(n.enum) > 0 && !slices.Contains(n.enum, s):
		return nil, fmt.Errorf("%s is %q, not one of %s", path, s, strings.Join(n.enum, ", "))
	case v.minLength != nil && int64(len(s)) < *v.minLength:
		return nil, fmt.Errorf("%s is shorter than %d", path, *v.minLength)
	case v.maxLength != nil && int64(len(s)) > *v.maxLength:
		return nil, fmt.Errorf("%s is longer than %d", path, *v.maxLength)
	case v.pattern != "" && !regexp.MustCompile(v.pattern).MatchString(s):
		return nil, fmt.Errorf("%s does not match %s", path, v.pattern)
	}
	return s, nil
}

// apiRule returns the rule a checked spec asks for, as Cloudflare stores it,
// its ref aside: what the generated conversion sends.
func (k *ruleKind) apiRule(spec map[string]any) map[string]any {
	enabled, ok := spec[fieldEnabled]
	if !ok {
		enabled = true
	}
	rule := map[string]any{ruleAction: k.rule.action, fieldExpression: spec[fieldExpression], fieldEnabled: enabled}
	if description, _ := spec[fieldDescription].(string); description != "" {
		rule[fieldDescription] = description
	}
	if params := apiStruct(k.params, spec); len(params) > 0 {
		rule[ruleActionParameters] = params
	}
	return rule
}

// apiStruct returns an object's value as the API takes it.
func apiStruct(st *paramStruct, value map[string]any) map[string]any {
	out := map[string]any{}
	for _, f := range st.fields {
		v, ok := value[f.jsonName]
		if ok && !f.key {
			out[f.apiName] = apiNode(f.node, v)
		}
	}
	return out
}

// apiNode returns a field's value as the API takes it.
func apiNode(node paramNode, value any) any {
	switch n := node.(type) {
	case *paramStruct:
		return apiStruct(n, value.(map[string]any))
	case *paramList:
		items := value.([]any)
		out := make([]any, len(items))
		for i, item := range items {
			out[i] = apiNode(n.items, item)
		}
		if n.set {
			slices.SortFunc(out, func(a, b any) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
		}
		return out
	case *paramMap:
		out := map[string]any{}
		for _, item := range value.([]any) {
			m := item.(map[string]any)
			out[m[n.value.fields[0].jsonName].(string)] = apiStruct(n.value, m)
		}
		return out
	}
	return value
}

// copySpec returns a deep copy of a spec.
func copySpec(spec map[string]any) map[string]any {
	data, err := json.Marshal(spec)
	if err != nil {
		panic(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	// JSON reads numbers as float64; the checked spec held int64.
	return normalizeNumbers(out).(map[string]any)
}

// normalizeNumbers turns the whole float64s of a decoded JSON value into
// int64s.
func normalizeNumbers(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			v[key] = normalizeNumbers(item)
		}
	case []any:
		for i, item := range v {
			v[i] = normalizeNumbers(item)
		}
	case float64:
		if v == float64(int64(v)) {
			return int64(v)
		}
	}
	return value
}
