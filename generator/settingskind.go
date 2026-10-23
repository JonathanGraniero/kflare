/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"fmt"
	"go/types"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// settingsKind is the model of a generated settings kind: what its types
// file, its controller, its envtest table and its sample hold. Its
// paramKind's params are the settings its service's write sends, and the
// bodies, the changes and the full specs ask for those settings, as
// cloudflare-go v7 sends them.
type settingsKind struct {
	paramKind
	shortName, noun, plural string
	doc                     []string
	// varName starts the kind's unexported names, as in managedTransformKind.
	varName string
	svc     *settingsService
	// unconfigured are the values the settings hold in a zone nobody has
	// configured, sorted by path, and unconfiguredState the settings of such a
	// zone that the generated specs start from: the sample's, with an item no
	// spec lists in each keyed list (unlisted), and with those values.
	// configuredStates are those settings with one value each changed to its
	// other: settings configured outside kflare, in that one respect.
	unconfigured      []settingsValue
	unconfiguredState map[string]any
	configuredStates  []map[string]any
	// listKeys holds the field that tells apart the items of each keyed list,
	// and sets the paths of the sets, by path of API names (settingsPath).
	listKeys     map[string]string
	sets         []string
	printColumns []printColumn
}

// settingsRoot is what messages call a settings kind's params.
const settingsRoot = "the settings"

// settingsValue is the value the settings of a zone nobody has configured
// hold at a path of API names, such as managed_request_headers[].enabled, and
// other another value of the field, which configured settings may hold.
type settingsValue struct {
	path         string
	value, other any
}

// buildSettingsKind builds the model of entry's kind from its service and the
// OpenAPI spec.
func buildSettingsKind(entry settingsKindConfig, svc *settingsService, spec *apiSpec) (*settingsKind, error) {
	k, err := (&settingsKindBuilder{entry: entry, svc: svc, spec: spec}).build()
	if err != nil {
		return nil, fmt.Errorf("%s (%s): %w", entry.Kind, entry.Service, err)
	}
	return k, nil
}

type settingsKindBuilder struct {
	entry settingsKindConfig
	svc   *settingsService
	spec  *apiSpec
}

func (b *settingsKindBuilder) build() (*settingsKind, error) {
	e, svc := b.entry, b.svc
	k := &settingsKind{
		paramKind: paramKind{name: e.Kind, root: settingsRoot, oneSample: true},
		shortName: e.ShortName, noun: e.Noun, plural: plural(e.Kind), doc: lines(e.Doc),
		varName: lowerCamel(e.Kind), svc: svc,
	}
	k.check, k.body = k.checkSpec, k.apiSettings
	if err := b.calls(); err != nil {
		return nil, err
	}
	writeSchema, err := b.writeSchema()
	if err != nil {
		return nil, err
	}

	used := map[string]bool{}
	pb := &paramBuilder{kind: e.Kind, r: svc.r, spec: b.spec, adjust: e.Fields, ignore: e.Ignore, used: used,
		zoneParams: true, root: settingsRoot}
	params, ok := svc.write.params.(*types.Named)
	if !ok {
		return nil, fmt.Errorf("%s's params are a %s, not a named struct", svc.write.name, svc.write.params)
	}
	if k.params, err = pb.object("", e.Kind+"Spec", e.Kind, params, writeSchema); err != nil {
		return nil, err
	}
	k.structs = pb.structs
	for _, section := range []map[string]bool{keySet(e.Fields), keySet(e.Ignore)} {
		for _, path := range slices.Sorted(maps.Keys(section)) {
			if !used[path] {
				return nil, fmt.Errorf("%s: the settings have no such field", path)
			}
		}
	}
	for _, f := range k.params.fields {
		if f.jsonName == "zoneRef" {
			return nil, fmt.Errorf("the settings have a field zoneRef, which the spec already holds")
		}
	}
	k.specRules = append(k.specRules, k.params.rules...)
	for _, r := range e.Rules {
		k.specRules = append(k.specRules, celRule{rule: r.Rule, message: r.Message})
	}
	k.listKeys = map[string]string{}
	k.shapeOf(k.params, "")
	if err := b.unconfigured(k); err != nil {
		return nil, err
	}
	k.printColumns = []printColumn{
		{name: "Zone ID", typ: typeString, jsonPath: ".status.cloudflareMetadata.zoneID"},
		readyColumn, ageColumn,
	}

	if k.cel, err = newCELChecker(); err != nil {
		return nil, err
	}
	err = k.addSpecs([]map[string]any{e.Sample}, e.Tests, func(bool, int, map[string]any) (string, error) {
		return "", nil
	})
	if err != nil {
		return nil, err
	}
	// The generated specs write the sample to an unconfigured zone, and take
	// over a zone it configured as one configured outside kflare.
	if !settingsConfigured(k.bodies[0], k.unconfigured) {
		return nil, fmt.Errorf("sample: it configures nothing: each value it sets is the one unconfigured gives")
	}
	if err := k.edits(entryRules(e.Rules, e.Fields)); err != nil {
		return nil, err
	}
	k.unconfiguredState = copySpec(k.bodies[0])
	k.unlisted()
	for _, v := range k.unconfigured {
		setSettingsValue(k.unconfiguredState, strings.Split(v.path, "."), v.value)
	}
	for _, v := range k.unconfigured {
		settings := copySpec(k.unconfiguredState)
		setLastSettingsValue(settings, strings.Split(v.path, "."), v.other)
		if !settingsConfigured(settings, k.unconfigured) {
			return nil, fmt.Errorf("unconfigured.values.%s: the sample's settings hold no such value, so the "+
				"generated specs cannot configure it", v.path)
		}
		k.configuredStates = append(k.configuredStates, settings)
	}
	return k, nil
}

// unlisted adds to each keyed list of the unconfigured settings an item no
// spec lists, as Cloudflare holds the items a write leaves out, such as the
// managed transforms a ManagedTransform does not list: a copy of the first
// item a spec lists, under a key no spec uses. A list the sample leaves out
// holds that item alone.
func (k *settingsKind) unlisted() {
	bodies := slices.Clone(k.bodies)
	for _, e := range slices.Concat(k.changes, k.full) {
		bodies = append(bodies, e.rule)
	}
	for _, path := range slices.Sorted(maps.Keys(k.listKeys)) {
		key := k.listKeys[path]
		var first map[string]any
		used := map[any]bool{}
		for _, body := range bodies {
			for _, item := range settingsValuesAt(body, strings.Split(path+"[]", ".")) {
				object, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if first == nil {
					first = object
				}
				used[normalizeNumber(object[key])] = true
			}
		}
		if first == nil {
			continue
		}
		item := copySpec(first)
		item[key] = unusedKey(first[key], used)
		appendSettingsItem(k.unconfiguredState, strings.Split(path, "."), item)
	}
}

// unusedKey returns a key that used, as normalizeNumber holds them, does not
// hold: a number if example is one, else "unlisted", or "unlisted-2" and on.
func unusedKey(example any, used map[any]bool) any {
	if _, number := normalizeNumber(example).(float64); number {
		n := int64(1)
		for used[float64(n)] {
			n++
		}
		return n
	}
	key := "unlisted"
	for i := 2; used[key]; i++ {
		key = fmt.Sprintf("unlisted-%d", i)
	}
	return key
}

// calls checks that each of the service's calls takes the zone's ID, which
// the shared settings reconciler sets, and requires nothing else it does not
// set: the read and the reset nothing, the write only the settings.
func (b *settingsKindBuilder) calls() error {
	for _, call := range []*sdkCall{b.svc.read, b.svc.write, b.svc.reset} {
		if err := checkShape(call, false); err != nil {
			return err
		}
		i := slices.IndexFunc(call.paramFields, isZoneID)
		if i < 0 {
			return fmt.Errorf("%s takes no zone_id: only zone settings are generated yet", call.name)
		}
		if !isString(call.paramFields[i].typ) {
			return fmt.Errorf("%s's zone_id is a %s, not a string", call.name, call.paramFields[i].typ)
		}
		for _, f := range call.paramFields {
			if !f.required || isZoneID(f) || (call == b.svc.write && f.in == inBody) {
				continue
			}
			return fmt.Errorf("%s requires the %s parameter %s, which the shared settings reconciler does not set",
				call.name, f.in, f.apiName)
		}
	}
	return nil
}

// writeSchema returns the OpenAPI schema of the settings the write sends.
func (b *settingsKindBuilder) writeSchema() (*schema, error) {
	op, err := b.spec.operation(b.svc.writeRoute)
	if err != nil {
		return nil, err
	}
	sch, err := b.spec.requestSchema(op)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", b.svc.writeRoute, err)
	}
	if sch == nil {
		return nil, fmt.Errorf("%s takes no JSON body", b.svc.writeRoute)
	}
	return sch, nil
}

// unconfigured reads the entry's unconfigured values, each at a path the
// settings the read returns hold, of the type of the value there.
func (b *settingsKindBuilder) unconfigured(k *settingsKind) error {
	values := b.entry.Unconfigured.Values
	for _, path := range slices.Sorted(maps.Keys(values)) {
		leaf, err := b.settingsField(path)
		if err != nil {
			return fmt.Errorf("unconfigured.values.%s: %w", path, err)
		}
		value := values[path]
		other, err := b.checkUnconfigured(leaf, value)
		if err != nil {
			return fmt.Errorf("unconfigured.values.%s: %w", path, err)
		}
		k.unconfigured = append(k.unconfigured, settingsValue{path: path, value: value, other: other})
	}
	return nil
}

// settingsField returns the field of the settings the read returns at path.
func (b *settingsKindBuilder) settingsField(path string) (sdkField, error) {
	var t types.Type = b.svc.settings
	var leaf sdkField
	steps := strings.Split(path, ".")
	for i, step := range steps {
		name, list := strings.CutSuffix(step, "[]")
		fields, err := b.svc.r.fields(t, false)
		if err != nil {
			return sdkField{}, fmt.Errorf("the settings at %s are not an object: %w", strings.Join(steps[:i], "."), err)
		}
		j := slices.IndexFunc(fields, func(f sdkField) bool { return f.apiName == name })
		if j < 0 {
			return sdkField{}, fmt.Errorf("the settings %s returns have no field %s", b.svc.read.name, name)
		}
		leaf, t = fields[j], fields[j].typ
		if list {
			slice, ok := t.Underlying().(*types.Slice)
			if !ok {
				return sdkField{}, fmt.Errorf("%s is not a list", name)
			}
			leaf.typ, t = slice.Elem(), slice.Elem()
		}
	}
	return leaf, nil
}

// checkUnconfigured checks a value an unconfigured zone's settings hold
// against the type of the field there, a string, a bool or a number, and
// returns another value of the field: the other bool, the next number,
// another of an enum's values, or another string.
func (b *settingsKindBuilder) checkUnconfigured(f sdkField, value any) (any, error) {
	switch basicClass(f.typ) {
	case types.IsString:
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("%v is not a string", value)
		}
		if f.enum == nil {
			return s + "-other", nil
		}
		if !slices.Contains(f.enum, s) {
			return nil, fmt.Errorf("%q is not one of %s", s, strings.Join(f.enum, ", "))
		}
		for _, other := range f.enum {
			if other != s {
				return other, nil
			}
		}
		return nil, fmt.Errorf("%q is the field's only value, so the settings cannot be configured", s)
	case types.IsBoolean:
		on, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("%v is not a bool", value)
		}
		return !on, nil
	case types.IsNumeric:
		n, ok := value.(float64)
		if !ok {
			return nil, fmt.Errorf("%v is not a number", value)
		}
		return n + 1, nil
	}
	return nil, fmt.Errorf("the field is a %s; only a string, a bool or a number can be compared", typeName(f.typ))
}

// shapeOf records the keyed lists and the sets of the struct st, at path.
// Maps are not entered: their entries' paths are their keys.
func (k *settingsKind) shapeOf(st *paramStruct, path string) {
	for _, f := range st.fields {
		fieldPath := settingsPath(path, f.apiName)
		switch n := f.node.(type) {
		case *paramStruct:
			k.shapeOf(n, fieldPath)
		case *paramList:
			if n.key != nil {
				k.listKeys[fieldPath] = n.key.apiName
			}
			if n.set {
				k.sets = append(k.sets, fieldPath)
			}
			if items, ok := n.items.(*paramStruct); ok {
				k.shapeOf(items, fieldPath+"[]")
			}
		}
	}
}

// settingsPath returns the path of the field apiName of the object at path.
func settingsPath(path, apiName string) string {
	if path == "" {
		return apiName
	}
	return path + "." + apiName
}

// checkSpec checks a sample, or another spec of the kind, against the model,
// and returns it with its integers as int64.
func (k *settingsKind) checkSpec(spec map[string]any, _ string) (map[string]any, error) {
	return checkStruct(k.params, spec, "spec")
}

// apiSettings returns the settings a checked spec asks for, as cloudflare-go
// v7 sends them: what the generated conversion builds.
func (k *settingsKind) apiSettings(spec map[string]any) map[string]any {
	return apiStruct(k.params, spec)
}

// edits builds the generated specs' edits of the sample and the tests: those
// of the settings' params, with each enum changed too.
func (k *settingsKind) edits(rules []entryRule) error {
	e := k.editor()
	k.scalarChanges(e, true)
	if e.failed != nil {
		return e.failed
	}
	values := k.sampleValues()
	k.swaps(values, e.change)
	if e.failed != nil {
		return e.failed
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
	return k.fullSpecs(values, nil, "")
}

// settingsConfigured reports whether settings, as the API takes them, hold
// another value at one of the paths of unconfigured than a zone nobody has
// configured, a null saying nothing: the controller's test of the same name,
// which kflare-gen runs on the sample.
func settingsConfigured(settings map[string]any, unconfigured []settingsValue) bool {
	for _, v := range unconfigured {
		for _, found := range settingsValuesAt(settings, strings.Split(v.path, ".")) {
			if found != nil && !reflect.DeepEqual(normalizeNumber(found), normalizeNumber(v.value)) {
				return true
			}
		}
	}
	return false
}

// settingsValuesAt returns the values settings hold at the path made of
// steps: one for a path through objects, one for each item of a list it goes
// through, none when it ends early.
func settingsValuesAt(value any, steps []string) []any {
	if len(steps) == 0 {
		return []any{value}
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	name, list := strings.CutSuffix(steps[0], "[]")
	next, ok := object[name]
	if !ok {
		return nil
	}
	if !list {
		return settingsValuesAt(next, steps[1:])
	}
	items, _ := next.([]any)
	var out []any
	for _, item := range items {
		out = append(out, settingsValuesAt(item, steps[1:])...)
	}
	return out
}

// setSettingsValue sets the value at the path made of steps in settings: in
// each item of a list the path goes through, and in objects it makes where
// the path goes through none.
func setSettingsValue(settings map[string]any, steps []string, value any) {
	name, list := strings.CutSuffix(steps[0], "[]")
	if len(steps) == 1 && !list {
		settings[name] = value
		return
	}
	if !list {
		next, ok := settings[name].(map[string]any)
		if !ok {
			next = map[string]any{}
			settings[name] = next
		}
		setSettingsValue(next, steps[1:], value)
		return
	}
	items, _ := settings[name].([]any)
	for _, item := range items {
		if object, ok := item.(map[string]any); ok && len(steps) > 1 {
			setSettingsValue(object, steps[1:], value)
		}
	}
}

// setLastSettingsValue sets the value at the path made of steps in settings,
// as setSettingsValue does, but in the last item of each list the path goes
// through only, and in no object it does not find.
func setLastSettingsValue(settings map[string]any, steps []string, value any) {
	name, list := strings.CutSuffix(steps[0], "[]")
	if len(steps) == 1 && !list {
		settings[name] = value
		return
	}
	next := settings[name]
	if list {
		items, _ := next.([]any)
		if len(items) == 0 {
			return
		}
		next = items[len(items)-1]
	}
	if object, ok := next.(map[string]any); ok && len(steps) > 1 {
		setLastSettingsValue(object, steps[1:], value)
	}
}

// appendSettingsItem appends a copy of item to the list at the path made of
// steps in settings, the list's name last: in each item of a list the path
// goes through, and in a new list, and objects, where it finds none.
func appendSettingsItem(settings map[string]any, steps []string, item map[string]any) {
	name, list := strings.CutSuffix(steps[0], "[]")
	if len(steps) == 1 {
		items, _ := settings[name].([]any)
		settings[name] = append(items, copySpec(item))
		return
	}
	if !list {
		next, ok := settings[name].(map[string]any)
		if !ok {
			next = map[string]any{}
			settings[name] = next
		}
		appendSettingsItem(next, steps[1:], item)
		return
	}
	items, _ := settings[name].([]any)
	for _, i := range items {
		if object, ok := i.(map[string]any); ok {
			appendSettingsItem(object, steps[1:], item)
		}
	}
}

// normalizeNumber returns a whole number as a float64, as JSON decodes it,
// whether a sample holds it as an int64 or YAML as a float64.
func normalizeNumber(value any) any {
	if n, ok := value.(int64); ok {
		return float64(n)
	}
	return value
}
