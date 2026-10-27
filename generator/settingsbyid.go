/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"fmt"
	"go/token"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Settings by ID. A service such as zones.SettingService reads and writes each
// of a zone's settings by its ID, and the zone has no reset. A settings kind
// of it has a field for each setting the service knows, but those its entry
// ignores, named for the setting's ID (always_use_https is alwaysUseHTTPS, as
// cloudflare-go v7's constant for the ID names it) and holding the setting's
// value:
//
//   - The value's type is that of the read's variant for the setting, with
//     the constraints of the OpenAPI schema of the value the write takes
//     (zones_<id>_value), and the conversion turns it into the type of the
//     write's union of values that carries it: shared.UnionString for a
//     string, shared.UnionFloat for a number, a struct of the union for an
//     object.
//   - A reset writes back, for each setting the resource sets, the value a
//     zone nobody has configured holds, so the entry's unconfigured values
//     give every field of every setting, by its path from the setting's ID.
//   - A setting Cloudflare picks the value of itself while another setting
//     holds a value (fields.<id>.pickedBy) has no unconfigured value of its
//     own: the other setting's value tells whether someone configured it, and
//     a reset writes that value back, handing the setting back to Cloudflare.

// paramsByID builds the spec of a kind whose settings go by their IDs: a field
// for each setting its service knows, in the order of their IDs, but those the
// entry ignores.
func (b *settingsKindBuilder) paramsByID(pb *paramBuilder, k *settingsKind) (*paramStruct, error) {
	e := b.entry
	schemas, err := b.valueSchemas()
	if err != nil {
		return nil, err
	}
	root := &paramStruct{goName: e.Kind + "Spec"}
	k.picks = map[string]*pickedByConfig{}
	for _, s := range b.svc.settingsByID {
		if pb.ignored(s.id) {
			continue
		}
		f, err := b.settingField(pb, s, schemas[s.id])
		if err != nil {
			return nil, err
		}
		if pick := e.Fields[s.id].PickedBy; pick != nil {
			if _, scalar := f.node.(*paramScalar); !scalar {
				return nil, fmt.Errorf("fields.%s.pickedBy: %s is an object; only a string, a bool or an integer is "+
					"picked", s.id, s.id)
			}
			k.picks[s.id] = pick
		}
		root.fields = append(root.fields, f)
	}
	if len(root.fields) == 0 {
		return nil, fmt.Errorf("%s: every setting is ignored", settingsRoot)
	}
	byOther := map[string]string{}
	for _, path := range slices.Sorted(maps.Keys(e.Fields)) {
		pick := e.Fields[path].PickedBy
		switch {
		case pick == nil:
			continue
		case strings.Contains(path, "."):
			return nil, fmt.Errorf("fields.%s.pickedBy: a setting is picked, not a field of one", path)
		case slices.ContainsFunc(root.fields, func(f *paramField) bool { return f.apiName == pick.Setting }):
			return nil, fmt.Errorf("fields.%s.pickedBy: %s is a setting of the kind, which the reset of %s would "+
				"write too", path, pick.Setting, path)
		case byOther[pick.Setting] != "":
			return nil, fmt.Errorf("fields.%s.pickedBy: %s is picked by %s too", path, byOther[pick.Setting],
				pick.Setting)
		}
		byOther[pick.Setting] = path
	}
	return root, nil
}

// settingField builds the field of the setting s, whose value the OpenAPI
// schema sch describes.
func (b *settingsKindBuilder) settingField(pb *paramBuilder, s *sdkSetting, sch *schema) (*paramField, error) {
	first, _ := utf8First(s.goName)
	switch {
	case s.value == nil:
		return nil, fmt.Errorf("%s holds no value, which kflare-gen cannot write by the setting's ID yet; ignore it "+
			"with a reason", s.id)
	case !token.IsIdentifier(s.goName) || !unicode.IsUpper(first):
		return nil, fmt.Errorf("%s's name in cloudflare-go v7, %q, is no exported Go name, which its field needs; "+
			"ignore it with a reason", s.id, s.goName)
	case sch == nil:
		return nil, fmt.Errorf("%s: %s takes no value of the setting (zones_%s_value in the OpenAPI subset); ignore "+
			"it with a reason", s.id, b.svc.writeRoute, s.id)
	}
	write, err := b.svc.valueVariant(s.value)
	if err != nil {
		return nil, fmt.Errorf("%s: %w; ignore it with a reason", s.id, err)
	}
	// A scalar takes its type from the read's variant, which gives its
	// enum's constants; an object its fields from the write's.
	t := s.value
	if isStructType(write) {
		t = write
	}
	f, err := pb.field(s.id, b.entry.Kind, sdkField{goName: s.goName, apiName: s.id, in: inBody, typ: write,
		doc: s.doc}, t, sch, false)
	if err != nil {
		return nil, err
	}
	return f, writableByID(f.node, s.id)
}

// utf8First returns the first rune of s.
func utf8First(s string) (rune, bool) {
	for _, r := range s {
		return r, true
	}
	return 0, false
}

// writableByID refuses a setting's value that holds a list or a map, whose
// reset kflare-gen cannot write yet.
func writableByID(node paramNode, path string) error {
	switch n := node.(type) {
	case *paramStruct:
		for _, f := range n.fields {
			if err := writableByID(f.node, joinPath(path, f.apiName)); err != nil {
				return err
			}
		}
	case *paramList, *paramMap:
		return fmt.Errorf("%s is a list, which kflare-gen cannot reset by the setting's ID yet; ignore it with a "+
			"reason", path)
	}
	return nil
}

// valueSchemas returns, by setting ID, the OpenAPI schemas of the values the
// write of a kind whose settings go by their IDs takes: the variants of the
// value its body carries, which the spec names zones_<id>_value.
func (b *settingsKindBuilder) valueSchemas() (map[string]*schema, error) {
	body, err := b.writeSchema()
	if err != nil {
		return nil, err
	}
	resolved, err := b.spec.resolve(body)
	if err != nil {
		return nil, err
	}
	_, variants, err := b.spec.alternatives(resolved)
	if err != nil {
		return nil, err
	}
	var value *schema
	for _, variant := range append([]*schema{resolved}, variants...) {
		props, err := b.spec.properties(variant)
		if err != nil {
			return nil, err
		}
		if v, ok := props["value"]; ok {
			value = v
		}
	}
	if value == nil {
		return nil, fmt.Errorf("%s's body takes no value", b.svc.writeRoute)
	}
	if value, err = b.spec.resolve(value); err != nil {
		return nil, err
	}
	out := map[string]*schema{}
	for _, part := range slices.Concat(value.OneOf, value.AnyOf) {
		name, ok := strings.CutPrefix(part.Ref, "#/components/schemas/zones_")
		if id, isValue := strings.CutSuffix(name, "_value"); ok && isValue {
			out[id] = part
		}
	}
	return out, nil
}

// unconfiguredByID reads the unconfigured values of a kind whose settings go
// by their IDs: one for every field of every setting, but for a setting
// Cloudflare picks, whose pickedBy gives the value of the setting it is picked
// by instead.
func (b *settingsKindBuilder) unconfiguredByID(k *settingsKind) error {
	values := b.entry.Unconfigured.Values
	leaves := map[string]*paramScalar{}
	for _, f := range k.params.fields {
		if _, picked := k.picks[f.apiName]; !picked {
			scalarLeaves(f.node, f.apiName, leaves)
		}
	}
	for _, path := range slices.Sorted(maps.Keys(leaves)) {
		if _, ok := values[path]; !ok {
			return fmt.Errorf("unconfigured.values: give %s, which a reset writes", path)
		}
	}
	pickers := map[string]string{}
	for id, pick := range k.picks {
		pickers[pick.Setting] = id
	}
	for _, path := range slices.Sorted(maps.Keys(values)) {
		n, ok := leaves[path]
		if !ok {
			id, _, _ := strings.Cut(path, ".")
			if pick, picked := k.picks[id]; picked {
				return fmt.Errorf("unconfigured.values.%s: Cloudflare picks %s while %s is %q (fields.%s.pickedBy), "+
					"so no value of %s tells whether someone configured it", path, id, pick.Setting, pick.While, id, id)
			}
			if picked, ok := pickers[id]; ok {
				return fmt.Errorf("unconfigured.values.%s: fields.%s.pickedBy gives its value, while", path, picked)
			}
			return fmt.Errorf("unconfigured.values.%s: the settings have no such field, or it holds more than a value",
				path)
		}
		other, err := otherScalarValue(n, values[path])
		if err != nil {
			return fmt.Errorf("unconfigured.values.%s: %w", path, err)
		}
		k.unconfigured = append(k.unconfigured, settingsValue{path: path, value: values[path], other: other})
	}
	for _, id := range slices.Sorted(maps.Keys(k.picks)) {
		pick := k.picks[id]
		k.unconfigured = append(k.unconfigured, settingsValue{path: pick.Setting, value: pick.While,
			other: pick.Written})
	}
	slices.SortFunc(k.unconfigured, func(a, b settingsValue) int { return strings.Compare(a.path, b.path) })
	return nil
}

// scalarLeaves records the scalars a setting's value holds, by path.
func scalarLeaves(node paramNode, path string, leaves map[string]*paramScalar) {
	switch n := node.(type) {
	case *paramScalar:
		leaves[path] = n
	case *paramStruct:
		for _, f := range n.fields {
			scalarLeaves(f.node, joinPath(path, f.apiName), leaves)
		}
	}
}

// otherScalarValue checks a value of a scalar field, as JSON decodes it,
// against the field, and returns another value the field takes, as JSON
// decodes it: another of an enum's values, another string, the other bool, or
// the next whole number in range, else the one before.
func otherScalarValue(n *paramScalar, value any) (any, error) {
	if _, err := checkScalar(n, value, "the value"); err != nil {
		return nil, err
	}
	var candidates []any
	switch n.goType {
	case typeBool:
		candidates = []any{!value.(bool)}
	case typeInt64:
		number, _ := wholeNumber(value)
		for _, e := range n.enum {
			if i, err := strconv.ParseInt(e, 10, 64); err == nil {
				candidates = append(candidates, float64(i))
			}
		}
		if len(n.enum) == 0 {
			candidates = []any{float64(number + 1), float64(number - 1)}
		}
	default:
		for _, e := range n.enum {
			candidates = append(candidates, e)
		}
		if len(n.enum) == 0 {
			candidates = []any{value.(string) + "-other"}
		}
	}
	for _, c := range candidates {
		if _, err := checkScalar(n, c, "another value"); err == nil && normalizeNumber(c) != normalizeNumber(value) {
			return c, nil
		}
	}
	return nil, fmt.Errorf("%v is the only value kflare-gen finds the field takes, so the settings cannot be "+
		"configured", value)
}

// statesByID builds the settings the generated specs of a kind whose settings
// go by their IDs start from: a zone nobody has configured, holding every
// setting at the value unconfigured gives, a setting Cloudflare picks at
// another value than the sample's, and the setting it is picked by at its
// value while Cloudflare picks; and that zone configured outside kflare in
// each one respect, with the spec whose resource reads it.
func (k *settingsKind) statesByID() error {
	state := map[string]any{}
	for _, v := range k.unconfigured {
		setSettingsValue(state, strings.Split(v.path, "."), v.value)
	}
	for _, id := range slices.Sorted(maps.Keys(k.picks)) {
		n := k.field(id).node.(*paramScalar)
		value, err := pickedValue(n, k.bodies[0][id])
		if err != nil {
			return fmt.Errorf("fields.%s.pickedBy: %w", id, err)
		}
		state[id] = value
	}
	k.unconfiguredState = state
	for _, v := range k.unconfigured {
		settings := copySpec(state)
		setLastSettingsValue(settings, strings.Split(v.path, "."), v.other)
		reader := k.readerOf(v.path)
		if reader < 0 {
			id, _, _ := strings.Cut(v.path, ".")
			return fmt.Errorf("unconfigured.values.%s: no sample or test sets %s, so the generated specs cannot "+
				"check that the value tells a zone configured outside kflare", v.path, id)
		}
		k.configuredStates = append(k.configuredStates, settings)
		k.configuredSpecs = append(k.configuredSpecs, reader)
	}
	for _, id := range slices.Sorted(maps.Keys(k.picks)) {
		f := k.field(id)
		value, err := otherScalarValue(f.node.(*paramScalar), state[id])
		if err != nil {
			return fmt.Errorf("fields.%s.pickedBy: %w", id, err)
		}
		spec, err := k.check(map[string]any{f.jsonName: value}, "")
		if err == nil {
			err = k.valid(spec)
		}
		if err != nil {
			return fmt.Errorf("fields.%s.pickedBy: a spec of %s alone: %w", id, id, err)
		}
		k.pickedSpecs = append(k.pickedSpecs, ruleEdit{what: id + " alone", spec: spec, rule: k.body(spec)})
	}
	return nil
}

// field returns the setting id of a kind whose settings go by their IDs.
func (k *settingsKind) field(id string) *paramField {
	i := slices.IndexFunc(k.params.fields, func(f *paramField) bool { return f.apiName == id })
	return k.params.fields[i]
}

// pickedValue returns the value a zone nobody has configured holds, for the
// generated specs, of a setting Cloudflare picks: another value than the
// sample's, so that the sample's resource writes it, or, when the sample does
// not set it, the first value the field takes.
func pickedValue(n *paramScalar, sample any) (any, error) {
	if sample != nil {
		return otherScalarValue(n, sample)
	}
	switch {
	case len(n.enum) > 0 && n.goType == typeInt64:
		i, err := strconv.ParseInt(n.enum[0], 10, 64)
		return float64(i), err
	case len(n.enum) > 0:
		return n.enum[0], nil
	case n.goType == typeBool:
		return false, nil
	case n.goType == typeInt64 && n.validation.minimum != nil:
		return *n.validation.minimum, nil
	case n.goType == typeInt64:
		return float64(0), nil
	}
	if _, err := checkScalar(n, "unconfigured", "the value"); err != nil {
		return nil, fmt.Errorf("the sample does not set it, and kflare-gen finds no value it takes")
	}
	return "unconfigured", nil
}

// readerOf returns the index, in specs, of the first spec whose resource
// reads the setting at path, or that a setting at path is picked by: the
// first that sets it. -1 when none does.
func (k *settingsKind) readerOf(path string) int {
	id, _, _ := strings.Cut(path, ".")
	for picked, pick := range k.picks {
		if pick.Setting == id {
			id = picked
		}
	}
	for i, body := range k.bodies {
		if _, ok := body[id]; ok {
			return i
		}
	}
	return -1
}

// resetValue returns the value a reset writes to the setting f of a kind
// whose settings go by their IDs, as JSON decodes it: its unconfigured value.
func (k *settingsKind) resetValue(f *paramField) any {
	settings := map[string]any{}
	for _, v := range k.unconfigured {
		if v.path == f.apiName || strings.HasPrefix(v.path, f.apiName+".") {
			setSettingsValue(settings, strings.Split(v.path, "."), v.value)
		}
	}
	return settings[f.apiName]
}
