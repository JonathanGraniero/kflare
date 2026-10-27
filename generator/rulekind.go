/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"go/types"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ruleKind is the model of a generated rule kind: what its types file, its
// controller, its envtest table and its sample hold. Its paramKind's params
// are the spec's fields but those every rule kind shares: the action, for a
// kind of several actions, the parameters of its actions, and the rule's own
// fields it maps. The bodies, the changes and the full specs ask for a rule,
// as Cloudflare stores it, its ref aside.
type ruleKind struct {
	paramKind
	shortName, noun, plural string
	doc                     []string
	// varName starts the kind's unexported names, as in transformRuleKind.
	varName string
	// actions are the kind's actions: its one action, or the values of its
	// spec's action field, in the entry's order.
	actions []ruleKindAction
	// actionDoc documents the action field; nil for a kind of one action.
	actionDoc []string
	// ruleFields are the spec fields that are the rule's own fields, such as
	// logging, rather than parameters of its action.
	ruleFields []*paramField
	conflicts  []ruleConflictConfig
	// phases are the kind's phases: one per type, in the order of the types,
	// or one.
	phases []rulePhase
	// typeDoc documents the type field; nil for a kind with one phase.
	typeDoc []string
	// retypes are specs a resource of the first sample may not change to.
	retypes      []ruleEdit
	printColumns []printColumn
}

// ruleKindAction is one of a rule kind's actions.
type ruleKindAction struct {
	// name is the action, such as block, and doc what a rule with it does,
	// from the entry: none for a kind of one action.
	name, doc string
	// constant names the RulesetNewParamsRulesAction constant of the action,
	// and goName is the constant's last part, such as ManagedChallenge.
	constant, goName string
	// params is the struct of the action's parameters, whose fields the spec
	// holds, built as cloudflare-go v7's struct for them; nil for an action
	// that takes none. For a kind of one action that maps none of the rule's
	// own fields, it is the paramKind's params.
	params *paramStruct
	// required are the JSON names of the parameters a kind of several actions
	// requires with the action, which its spec cannot require.
	required []string
}

// severalActions reports whether the kind's spec picks one of several
// actions.
func (k *ruleKind) severalActions() bool {
	return k.actionDoc != nil
}

// actionOf returns the action of a checked spec: the one its action field
// names, or the kind's only one.
func (k *ruleKind) actionOf(spec map[string]any) ruleKindAction {
	for _, a := range k.actions {
		if k.severalActions() && a.name == spec[ruleAction] {
			return a
		}
	}
	return k.actions[0]
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

// buildRuleKind builds the model of entry's kind from its rule types, which
// load loads by name, and the OpenAPI spec.
func buildRuleKind(entry ruleKindConfig, load func(name string) (*sdkRule, error), spec *apiSpec) (*ruleKind, error) {
	k, err := (&ruleKindBuilder{entry: entry, load: load, spec: spec}).build()
	if err != nil {
		if entry.Rule != "" {
			return nil, fmt.Errorf("%s (%s): %w", entry.Kind, entry.Rule, err)
		}
		return nil, fmt.Errorf("%s: %w", entry.Kind, err)
	}
	return k, nil
}

type ruleKindBuilder struct {
	entry ruleKindConfig
	load  func(name string) (*sdkRule, error)
	spec  *apiSpec
	// r reads the package of the entry's rule types.
	r *sdkReader
}

func (b *ruleKindBuilder) build() (*ruleKind, error) {
	e := b.entry
	k := &ruleKind{
		paramKind: paramKind{name: e.Kind, root: actionParametersRoot},
		shortName: e.ShortName, noun: e.Noun, plural: plural(e.Kind), doc: lines(e.Doc),
		varName: lowerCamel(e.Kind), conflicts: e.Conflicts,
	}
	k.check, k.body = k.checkSpec, k.apiRule
	configs := e.Actions
	if e.Rule != "" {
		configs = []ruleActionConfig{{Rule: e.Rule}}
	} else {
		k.actionDoc = lines(e.ActionDoc)
	}
	rules, err := b.rules(configs)
	if err != nil {
		return nil, err
	}
	if err := b.phases(k); err != nil {
		return nil, err
	}

	pb := &paramBuilder{kind: e.Kind, r: b.r, spec: b.spec, adjust: e.Fields, ignore: e.Ignore, used: map[string]bool{},
		root: actionParametersRoot}
	for i, c := range configs {
		where := fmt.Sprintf("actions[%d] (%s): ", i, c.Action)
		if !k.severalActions() {
			where = ""
		}
		a, err := b.action(pb, c, rules[i])
		if err != nil {
			return nil, fmt.Errorf("%s%w", where, err)
		}
		k.actions = append(k.actions, a)
	}
	generic, err := b.genericRuleFields()
	if err != nil {
		return nil, err
	}
	if k.ruleFields, err = b.ruleFields(pb, rules, generic); err != nil {
		return nil, err
	}
	k.structs = pb.structs
	for _, section := range []map[string]bool{keySet(e.Fields), keySet(e.Ignore)} {
		for _, path := range slices.Sorted(maps.Keys(section)) {
			if !pb.used[path] {
				return nil, fmt.Errorf("%s: the rule type has no such field", path)
			}
		}
	}
	if err := k.root(); err != nil {
		return nil, err
	}

	k.specRules = append(k.specRules, k.params.rules...)
	for _, r := range e.Rules {
		k.specRules = append(k.specRules, celRule{rule: r.Rule, message: r.Message})
	}
	if k.printColumns, err = k.fieldColumns(e.Fields); err != nil {
		return nil, err
	}
	if k.typeDoc != nil {
		k.printColumns = append(k.printColumns, printColumn{name: "Type", typ: typeString, jsonPath: ".spec.type"})
	}
	if k.severalActions() {
		k.printColumns = append(k.printColumns, printColumn{name: "Action", typ: typeString, jsonPath: ".spec.action"})
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
	// The generated specs write a rule of each action, and break the rules
	// of its parameters from a spec of it.
	for i, a := range k.actions {
		if k.severalActions() && !slices.ContainsFunc(k.specs(), func(s map[string]any) bool {
			return s[ruleAction] == a.name
		}) {
			return nil, fmt.Errorf("actions[%d]: no sample or test has action %s; add a test of it", i, a.name)
		}
	}
	if err := k.edits(entryRules(e.Rules, e.Fields)); err != nil {
		return nil, err
	}
	return k, nil
}

// rules loads the rule type each action names, nil for an action without
// one: the first with the entry's loader, the others from its package, which
// b.r then reads.
func (b *ruleKindBuilder) rules(configs []ruleActionConfig) ([]*sdkRule, error) {
	rules := make([]*sdkRule, len(configs))
	var first *sdkRule
	for i, c := range configs {
		if c.Rule == "" {
			continue
		}
		var rule *sdkRule
		var err error
		if first == nil {
			rule, err = b.load(c.Rule)
			first = rule
		} else {
			rule, err = first.sibling(c.Rule)
		}
		switch {
		case err != nil && c.Action == "":
			return nil, err
		case err != nil:
			return nil, fmt.Errorf("actions[%d] (%s): %w", i, c.Action, err)
		case c.Action != "" && rule.action != c.Action:
			return nil, fmt.Errorf("actions[%d] (%s): %s is the rule type for %s", i, c.Action, c.Rule, rule.action)
		}
		rules[i] = rule
	}
	b.r = first.r
	return rules, nil
}

// action builds one of the kind's actions from its entry and its rule type,
// nil when cloudflare-go v7 has none for it, and its OpenAPI schema: the
// parameters of the action are the struct the spec holds them in.
func (b *ruleKindBuilder) action(pb *paramBuilder, c ruleActionConfig, rule *sdkRule) (ruleKindAction, error) {
	a := ruleKindAction{name: c.Action, doc: c.Doc}
	schemaName := actionSchemaName(c.Action)
	if rule != nil {
		a.name, schemaName = rule.action, ruleSchemaName(rule.name)
	}
	var err error
	if a.constant, err = b.r.constantNamed("RulesetNewParamsRulesAction", a.name); err != nil {
		return a, err
	}
	a.goName = strings.TrimPrefix(a.constant, "RulesetNewParamsRulesAction")
	paramsSchema, err := b.paramsSchema(schemaName, a.name)
	if err != nil {
		return a, err
	}
	switch {
	case (rule == nil || rule.params == nil) && paramsSchema != nil:
		return a, fmt.Errorf("%s takes action parameters, which cloudflare-go v7 has no type for", schemaName)
	case rule != nil && rule.params != nil && paramsSchema == nil:
		return a, fmt.Errorf("%s has no action_parameters", schemaName)
	case paramsSchema == nil && b.entry.Rule != "":
		return a, fmt.Errorf("the rule type takes no action parameters, which a kind of one action maps")
	case paramsSchema == nil:
		return a, nil
	}
	if a.params, err = pb.object("", b.entry.Kind+"Spec", b.entry.Kind, rule.params, paramsSchema); err != nil {
		return a, err
	}
	if b.entry.Rule == "" {
		// With several actions, each requires its own parameters, which the
		// spec cannot require of every resource.
		for _, f := range a.params.fields {
			if f.required {
				a.required = append(a.required, f.jsonName)
				f.required = false
			}
		}
	}
	return a, nil
}

// ruleFields builds the spec fields of the rule's own fields that the entry
// does not ignore, such as logging, from the first rule type that has each.
// The conversion sets them on cloudflare-go v7's rule for every action, so
// each must fit the field of that rule: generic holds their types.
func (b *ruleKindBuilder) ruleFields(pb *paramBuilder, rules []*sdkRule,
	generic map[string]types.Type) ([]*paramField, error) {
	var out []*paramField
	seen := map[string]bool{}
	for _, rule := range rules {
		if rule == nil {
			continue
		}
		component, err := b.spec.resolve(b.spec.Components.Schemas[ruleSchemaName(rule.name)])
		if err != nil {
			return nil, err
		}
		props, err := b.spec.properties(component)
		if err != nil {
			return nil, err
		}
		for _, f := range rule.fields {
			if slices.Contains(sharedRuleFields, f.apiName) || f.apiName == ruleActionParameters || seen[f.apiName] {
				continue
			}
			seen[f.apiName] = true
			if pb.ignored(f.apiName) {
				continue
			}
			if err := fitsGenericRule(generic, f); err != nil {
				return nil, err
			}
			pf, err := pb.field(f.apiName, b.entry.Kind, f, f.typ, props[f.apiName], false)
			if err != nil {
				return nil, err
			}
			out = append(out, pf)
		}
	}
	return out, nil
}

// fitsGenericRule checks that the rule's own field f fits the field of
// cloudflare-go v7's rule for every action that the conversion sets it on:
// generic holds their types by API name.
func fitsGenericRule(generic map[string]types.Type, f sdkField) error {
	if target, ok := generic[f.apiName]; !ok || !types.AssignableTo(f.typ, target) {
		return fmt.Errorf("the rule's field %s is a %s, which cloudflare-go v7's rule does not take as its %s; "+
			"ignore it with a reason", f.apiName, f.typ, f.apiName)
	}
	return nil
}

// genericRuleFields returns the types of the fields of cloudflare-go v7's
// rule for every action, RulesetNewParamsRule, which the conversion fills
// in, by API name.
func (b *ruleKindBuilder) genericRuleFields() (map[string]types.Type, error) {
	obj, ok := b.r.pkg.Types.Scope().Lookup("RulesetNewParamsRule").(*types.TypeName)
	if !ok {
		return nil, fmt.Errorf("%s has no type RulesetNewParamsRule", b.r.pkg.PkgPath)
	}
	fields, err := b.r.fields(obj.Type().(*types.Named), true)
	if err != nil {
		return nil, err
	}
	out := map[string]types.Type{}
	for _, f := range fields {
		out[f.apiName] = f.typ
	}
	return out, nil
}

// root builds the struct whose fields the spec holds besides the shared ones:
// the action, for a kind of several actions, the parameters of the actions
// and the rule's own fields. A kind of one action without the rule's own
// fields holds its parameters' struct itself, its rules on the spec.
func (k *ruleKind) root() error {
	if !k.severalActions() && len(k.ruleFields) == 0 {
		k.params = k.actions[0].params
		return k.checkRootNames()
	}
	k.params = &paramStruct{goName: k.name + "Spec"}
	if k.severalActions() {
		names := make([]string, len(k.actions))
		for i, a := range k.actions {
			names[i] = a.name
		}
		k.params.fields = append(k.params.fields, &paramField{
			apiName: ruleAction, goName: "Action", jsonName: ruleAction, required: true, doc: k.actionFieldDoc(),
			node: &paramScalar{goType: typeString, enum: names},
		})
	}
	holders := map[*paramField]string{}
	for _, a := range k.actions {
		if a.params == nil {
			continue
		}
		for _, f := range a.params.fields {
			if slices.ContainsFunc(k.params.fields, func(g *paramField) bool { return g.jsonName == f.jsonName }) {
				return fmt.Errorf("action %s takes a parameter %s, which the spec already holds: another action "+
					"takes one by that name, or it is the action field", a.name, f.jsonName)
			}
			k.params.fields = append(k.params.fields, f)
			holders[f] = a.name
		}
	}
	for _, f := range k.ruleFields {
		if slices.ContainsFunc(k.params.fields, func(g *paramField) bool { return g.jsonName == f.jsonName }) {
			return fmt.Errorf("the rule's field %s is also a parameter of an action", f.jsonName)
		}
		k.params.fields = append(k.params.fields, f)
	}
	if err := k.checkRootNames(); err != nil {
		return err
	}
	if !k.severalActions() {
		k.params.rules = k.actions[0].params.rules
		return nil
	}
	k.params.rules = k.actionRules(holders)
	return nil
}

// fieldColumns returns the columns the entry's fields ask for (printColumn), in
// the order the spec holds the fields: a column shows a string, an integer or
// a boolean of the spec, or of an object in it, but not of a list's items or a
// map's values.
func (k *ruleKind) fieldColumns(fields map[string]ruleFieldConfig) ([]printColumn, error) {
	columnTypes := map[string]string{typeString: typeString, typeInt64: typeInteger, typeBool: "boolean"}
	var columns []printColumn
	shown := map[string]bool{}
	var walk func(st *paramStruct, path, jsonPath string)
	walk = func(st *paramStruct, path, jsonPath string) {
		for _, f := range st.fields {
			fieldPath, fieldJSONPath := joinPath(path, f.apiName), jsonPath+"."+f.jsonName
			switch n := f.node.(type) {
			case *paramScalar:
				if name := fields[fieldPath].PrintColumn; name != "" {
					columns = append(columns, printColumn{name: name, typ: columnTypes[n.goType], jsonPath: fieldJSONPath})
					shown[fieldPath] = true
				}
			case *paramStruct:
				walk(n, fieldPath, fieldJSONPath)
			}
		}
	}
	walk(k.params, "", ".spec")
	for _, path := range slices.Sorted(maps.Keys(fields)) {
		if fields[path].PrintColumn != "" && !shown[path] {
			return nil, fmt.Errorf("fields.%s.printColumn: %s is not a string, an integer or a boolean outside a "+
				"list or a map, which a column can show", path, path)
		}
	}
	return columns, nil
}

// checkRootNames checks that no field of the params takes the name of a field
// the spec already holds.
func (k *ruleKind) checkRootNames() error {
	for _, f := range k.params.fields {
		if f.jsonName == "zoneRef" || slices.Contains(sharedSpecFields, f.jsonName) {
			return fmt.Errorf("the action parameters have a field %s, which the spec already holds", f.jsonName)
		}
	}
	return nil
}

// actionRules returns the rules on the spec of a kind of several actions: a
// parameter is set only with the action that takes it (holders; no two take
// one by the same name), an action sets the parameters it requires, and its
// parameters' own rules hold with it. They are the rules of the actions'
// structs, now on the spec.
func (k *ruleKind) actionRules(holders map[*paramField]string) []celRule {
	var out []celRule
	for _, f := range k.params.fields {
		if action, ok := holders[f]; ok {
			out = append(out, celRule{rule: "!has(self." + f.jsonName + ") || self.action == '" + action + "'",
				message: f.jsonName + " is allowed only with action " + action,
				action:  &actionCheck{field: f, holder: action}})
		}
	}
	for _, a := range k.actions {
		for _, name := range a.required {
			f := k.params.fields[slices.IndexFunc(k.params.fields, func(g *paramField) bool { return g.jsonName == name })]
			out = append(out, celRule{rule: "self.action != '" + a.name + "' || has(self." + name + ")",
				message: "action " + a.name + " requires " + name, action: &actionCheck{field: f, requiredBy: a.name}})
		}
	}
	for _, a := range k.actions {
		if a.params == nil {
			continue
		}
		for _, r := range a.params.rules {
			out = append(out, celRule{rule: "self.action != '" + a.name + "' || (" + r.rule + ")",
				message: "action " + a.name + " " + r.message, action: &actionCheck{within: a.name, inner: r,
					params: a.params}})
		}
	}
	return out
}

// actionFieldDoc documents the action field: the entry's actionDoc, then what
// each action does.
func (k *ruleKind) actionFieldDoc() []string {
	out := append(slices.Clone(k.actionDoc), "")
	for _, a := range k.actions {
		for j, line := range wrapIndented(fmt.Sprintf("%s: %s", a.name, a.doc)) {
			if j == 0 {
				out = append(out, "  - "+line)
			} else {
				out = append(out, "    "+line)
			}
		}
	}
	return out
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
	// Each spec with each other action its parameters allow: a rule changes
	// its action in place.
	for i, spec := range e.specs {
		for _, a := range k.actions {
			if k.severalActions() && a.name != spec[ruleAction] {
				e.change(i, "action "+a.name, func(spec map[string]any) { spec[ruleAction] = a.name })
			}
		}
	}
	k.parametersAlone(e)
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
	if !k.severalActions() {
		return k.fullSpecs(values, shared, k.phases[0].typeName)
	}
	// One for each action, which the conversion sends only the parameters of.
	for _, a := range k.actions {
		st := &paramStruct{goName: k.params.goName}
		if a.params != nil {
			st.fields = slices.Clone(a.params.fields)
		}
		st.fields = append(st.fields, k.ruleFields...)
		base := maps.Clone(shared)
		base[ruleAction] = a.name
		if err := k.fullSpecsOf(st, "action "+a.name+", ", values, base, k.phases[0].typeName); err != nil {
			return fmt.Errorf("action %s: %w", a.name, err)
		}
	}
	return nil
}

// parametersAlone adds, for a kind of several actions, a change that sets
// each parameter of each action alone, from the first spec of the action, to
// the value a spec gives it or one that sets everything: the conversion sends
// an action's parameters whenever the spec sets any of them. A change the CEL
// rules refuse (an action requires another parameter) or that asks for the
// rule the spec already does is left out.
func (k *ruleKind) parametersAlone(e *editor) {
	if !k.severalActions() {
		return
	}
	values := k.sampleValues()
	for _, a := range k.actions {
		i := slices.IndexFunc(e.specs, func(s map[string]any) bool { return s[ruleAction] == a.name })
		if a.params == nil || i < 0 {
			continue
		}
		for _, f := range a.params.fields {
			value, set := e.specs[i][f.jsonName]
			if !set {
				var err error
				if value, err = fullNode(f, f.node, values, false); err != nil {
					e.failed = cmp.Or(e.failed, fmt.Errorf("action %s, %s alone: %w", a.name, f.jsonName, err))
					return
				}
			}
			alone := copySpec(e.specs[i])
			for _, g := range a.params.fields {
				delete(alone, g.jsonName)
			}
			alone[f.jsonName] = copySpec(map[string]any{f.jsonName: value})[f.jsonName]
			if reflect.DeepEqual(k.body(alone), k.body(e.specs[i])) {
				continue
			}
			e.change(i, fmt.Sprintf("action %s with %s alone", a.name, f.jsonName), func(spec map[string]any) {
				clear(spec)
				maps.Copy(spec, alone)
			})
		}
	}
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
		constant, err := b.r.constantNamed("Phase", e.Phase)
		if err != nil {
			return err
		}
		k.phases = []rulePhase{{phase: e.Phase, constant: constant}}
		return nil
	}
	for _, t := range e.Types {
		constant, err := b.r.constantNamed("Phase", t.Phase)
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

// actionSchemaName returns the name of the OpenAPI schema of a rule with an
// action cloudflare-go v7 has no rule type for, such as rulesets_JsChallengeRule
// for js_challenge: each word of the action capitalized.
func actionSchemaName(action string) string {
	var b strings.Builder
	b.WriteString("rulesets_")
	for word := range strings.SplitSeq(action, "_") {
		b.WriteString(upperCamel(word))
	}
	b.WriteString("Rule")
	return b.String()
}

// paramsSchema returns the OpenAPI schema of the action parameters of the rule
// schema name, nil when it takes none: every rule schema combines
// rulesets_Rule's, an object without properties, so an action that takes
// parameters is one whose schema gives them properties. The schema's action
// must be action.
func (b *ruleKindBuilder) paramsSchema(name, action string) (*schema, error) {
	component, ok := b.spec.Components.Schemas[name]
	if !ok {
		return nil, fmt.Errorf("the OpenAPI subset has no schema %s: run make sync-spec", name)
	}
	actions, err := b.actionsOf(component)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(actions, []string{action}) {
		return nil, fmt.Errorf("%s's actions are %v, not %s", name, actions, action)
	}
	props, err := b.spec.properties(component)
	if err != nil {
		return nil, err
	}
	params, ok := props[ruleActionParameters]
	if !ok {
		return nil, nil
	}
	resolved, err := b.spec.resolve(params)
	if err != nil {
		return nil, err
	}
	paramProps, err := b.spec.properties(params)
	if err != nil {
		return nil, err
	}
	if len(paramProps) == 0 && len(resolved.OneOf) == 0 && len(resolved.AnyOf) == 0 {
		return nil, nil
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
	action := k.actionOf(spec)
	rule := map[string]any{ruleAction: action.name, fieldExpression: spec[fieldExpression], fieldEnabled: enabled}
	if description, _ := spec[fieldDescription].(string); description != "" {
		rule[fieldDescription] = description
	}
	if action.params != nil {
		if params := apiStruct(action.params, spec); len(params) > 0 {
			rule[ruleActionParameters] = params
		}
	}
	for _, f := range k.ruleFields {
		if value, ok := spec[f.jsonName]; ok {
			rule[f.apiName] = apiNode(f.node, value)
		}
	}
	return rule
}

// apiStruct returns an object's value as the API takes it, without the zero
// values of the fields Cloudflare stores as absent.
func apiStruct(st *paramStruct, value map[string]any) map[string]any {
	out := map[string]any{}
	for _, f := range st.fields {
		v, ok := value[f.jsonName]
		if f.omitZero && v == zeroValue(f.node.(*paramScalar)) {
			continue
		}
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
