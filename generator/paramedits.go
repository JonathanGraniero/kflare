/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The edits of a kind's samples that its generated specs write, from its
// spec's tree of params: changes of each sample, which update what it asks
// for in place; specs the CRD refuses: an enum value or key it does not
// allow, a value out of bounds or not matching its pattern, an empty or
// repeating list, a breach of each rule of the params (of each variant of a
// oneOf), and each entry rule's invalid spec; and specs that set every field,
// which the generated specs convert without the API server. A rule kind adds
// its own (rulekind.go): its description and enabled changed, the other
// samples, to which the first sample's resource may not change type, and a
// type outside its values.

// step is one step into a spec: into a field, or into an item of a list.
type step struct {
	field string
	index int
}

// pathString writes a path as the API server reports it, such as
// spec.headers[0].value.
func pathString(path []step) string {
	var b strings.Builder
	b.WriteString("spec")
	for _, s := range path {
		if s.field != "" {
			b.WriteString("." + s.field)
		} else {
			fmt.Fprintf(&b, "[%d]", s.index)
		}
	}
	return b.String()
}

// relativePath writes a path without its spec prefix, for what a spec does.
func relativePath(path []step) string {
	return strings.TrimPrefix(pathString(path), "spec.")
}

// appendStep returns path with one more step, sharing nothing with path.
func appendStep(path []step, s step) []step {
	return append(append([]step(nil), path...), s)
}

// visitor sees each value a checked spec holds: the struct of each object,
// with the object, and the field of each scalar, with its value.
type visitor struct {
	object func(path []step, st *paramStruct, value map[string]any)
	scalar func(path []step, f *paramField, n *paramScalar, value any)
}

// visitStruct walks an object's value, its own fields before those of its
// nested objects.
func (v visitor) visitStruct(path []step, st *paramStruct, value map[string]any) {
	if v.object != nil {
		v.object(path, st, value)
	}
	for _, f := range st.fields {
		item, ok := value[f.jsonName]
		if !ok {
			continue
		}
		v.visitNode(appendStep(path, step{field: f.jsonName}), f, f.node, item)
	}
}

func (v visitor) visitNode(path []step, f *paramField, node paramNode, value any) {
	switch n := node.(type) {
	case *paramScalar:
		if v.scalar != nil {
			v.scalar(path, f, n, value)
		}
	case *paramStruct:
		v.visitStruct(path, n, value.(map[string]any))
	case *paramList:
		for i, item := range value.([]any) {
			v.visitNode(appendStep(path, step{index: i}), f, n.items, item)
		}
	case *paramMap:
		for i, item := range value.([]any) {
			v.visitStruct(appendStep(path, step{index: i}), n.value, item.(map[string]any))
		}
	}
}

// valueAt returns the value at path in spec.
func valueAt(spec map[string]any, path []step) any {
	var current any = spec
	for _, s := range path {
		if s.field != "" {
			current = current.(map[string]any)[s.field]
		} else {
			current = current.([]any)[s.index]
		}
	}
	return current
}

// setAt sets the field at the end of path in spec to value, or deletes it
// when value is nil.
func setAt(spec map[string]any, path []step, value any) {
	parent := valueAt(spec, path[:len(path)-1])
	last := path[len(path)-1]
	if last.field == "" {
		parent.([]any)[last.index] = value
		return
	}
	if value == nil {
		delete(parent.(map[string]any), last.field)
		return
	}
	parent.(map[string]any)[last.field] = value
}

// scalarChanges adds a change of each scalar of each spec but keys, and but
// enums unless enums is set: another value of an enum, the other bool, the
// next integer in range, or a string with its last character changed.
func (k *paramKind) scalarChanges(e *editor, enums bool) {
	for i, sample := range e.specs {
		visitor{scalar: func(path []step, f *paramField, n *paramScalar, value any) {
			if f.key || (len(n.enum) > 0 && !enums) || e.failed != nil {
				return
			}
			other, err := otherScalar(n, value)
			if err != nil {
				e.failed = fmt.Errorf("%s: %s: %w", k.specName(i), pathString(path), err)
				return
			}
			e.change(i, "a new "+relativePath(path), func(spec map[string]any) { setAt(spec, path, other) })
		}}.visitStruct(nil, k.params, sample)
	}
}

// entryInvalid adds the invalid spec of each of the entry's CEL rules, which
// must break its rule.
func (k *paramKind) entryInvalid(rules []entryRule) error {
	for _, r := range rules {
		typeName, _ := r.Invalid[fieldType].(string)
		spec, err := k.check(r.Invalid, typeName)
		if err != nil {
			return fmt.Errorf("%s.invalid: %w", r.where, err)
		}
		if err := k.breaks(spec, r.Message); err != nil {
			return fmt.Errorf("%s.invalid: %w", r.where, err)
		}
		k.invalid = append(k.invalid, ruleEdit{what: "a breach of: " + r.Message, spec: spec, message: r.Message})
	}
	return nil
}

// swaps adds, for each object that takes exactly one of its fields, the
// changes that set each other scalar field in place of the one the first
// sample holding the object sets, so that every alternative is written and
// refused when invalid.
func (k *paramKind) swaps(values map[*paramField]any, change func(int, string, func(map[string]any))) {
	swapped := map[*paramStruct]bool{}
	for i, sample := range k.specs() {
		visitor{object: func(path []step, st *paramStruct, value map[string]any) {
			if swapped[st] || !takesOne(st) {
				return
			}
			swapped[st] = true
			for _, f := range st.fields {
				n, scalar := f.node.(*paramScalar)
				if _, set := value[f.jsonName]; set || f.key || f.required || !scalar {
					continue
				}
				other, ok := scalarValue(f, n, values)
				if !ok {
					continue
				}
				change(i, k.describeObject(path)+" holding "+f.jsonName+" instead", func(spec map[string]any) {
					object := objectAt(spec, path)
					for _, g := range st.fields {
						if !g.key && !g.required {
							delete(object, g.jsonName)
						}
					}
					object[f.jsonName] = other
				})
			}
		}}.visitStruct(nil, k.params, sample)
	}
}

// takesOne reports whether an object takes exactly one of its fields.
func takesOne(st *paramStruct) bool {
	return slices.ContainsFunc(st.rules, func(r celRule) bool {
		return strings.HasPrefix(r.message, "must set exactly one of")
	})
}

// invalidValues adds the values the CRD refuses, each in the first spec that
// holds the field, a sample or a change of one: an enum value or key it does
// not allow, a value out of bounds or not matching its pattern, an empty or
// repeating list, and a required field left out.
func (k *paramKind) invalidValues() error {
	var sample map[string]any
	invalid := func(what, message string, edit func(spec map[string]any)) {
		spec := copySpec(sample)
		edit(spec)
		k.invalid = append(k.invalid, ruleEdit{what: what, spec: spec, message: message})
	}
	var failed error
	seen, seenField := map[*paramField]bool{}, map[*paramField]bool{}
	fields := func(path []step, st *paramStruct, value map[string]any) {
		for _, f := range st.fields {
			if _, ok := value[f.jsonName]; !ok || seenField[f] {
				continue
			}
			seenField[f] = true
			path := appendStep(path, step{field: f.jsonName})
			if f.required && !f.key {
				invalid(relativePath(path)+" left out", pathString(path)+": Required value",
					func(spec map[string]any) { setAt(spec, path, nil) })
			}
			items, ok := value[f.jsonName].([]any)
			if !ok {
				continue
			}
			minItems, unique := listBounds(f.node)
			if minItems != nil && *minItems > 0 {
				invalid("an empty "+relativePath(path), pathString(path), func(spec map[string]any) {
					setAt(spec, path, []any{})
				})
			}
			if unique {
				message := fmt.Sprintf("%s[%d]: Duplicate value", pathString(path), len(items))
				invalid(relativePath(path)+" listing an item twice", message, func(spec map[string]any) {
					list := valueAt(spec, path).([]any)
					setAt(spec, path, append(list, list[0]))
				})
			}
			if long, ok := tooMany(f.node, items); ok {
				invalid(relativePath(path)+" with too many items", pathString(path)+": Too many", func(spec map[string]any) {
					setAt(spec, path, long)
				})
			}
		}
	}
	scalars := func(path []step, f *paramField, n *paramScalar, value any) {
		if seen[f] || failed != nil {
			return
		}
		seen[f] = true
		at := pathString(path)
		v := n.validation
		switch {
		case f.key:
			invalid("an empty "+relativePath(path), at, func(spec map[string]any) { setAt(spec, path, "") })
		case len(n.enum) > 0:
			if outside, ok := outsideEnum(n); ok {
				invalid(relativePath(path)+" outside its enum", at+": Unsupported value", func(spec map[string]any) {
					setAt(spec, path, outside)
				})
			}
		case v.minLength != nil && *v.minLength > 0:
			short, what := strings.Repeat("a", int(*v.minLength-1)), relativePath(path)+" too short"
			if short == "" {
				what = "an empty " + relativePath(path)
			}
			invalid(what, at, func(spec map[string]any) { setAt(spec, path, short) })
		}
		if v.maxLength != nil {
			invalid(relativePath(path)+" too long", at, func(spec map[string]any) {
				setAt(spec, path, strings.Repeat("a", int(*v.maxLength+1)))
			})
		}
		if v.pattern != "" && len(n.enum) == 0 {
			refused, ok := mismatch(n)
			if !ok {
				failed = fmt.Errorf("%s: kflare-gen finds no value its pattern %s refuses", at, v.pattern)
				return
			}
			invalid(relativePath(path)+" not matching its pattern", at, func(spec map[string]any) {
				setAt(spec, path, refused)
			})
		}
		if v.minimum != nil {
			invalid(relativePath(path)+" below its minimum", at, func(spec map[string]any) {
				setAt(spec, path, int64(*v.minimum)-1)
			})
		}
		if v.maximum != nil {
			invalid(relativePath(path)+" above its maximum", at, func(spec map[string]any) {
				setAt(spec, path, int64(*v.maximum)+1)
			})
		}
	}
	specs := k.specs()
	for _, c := range k.changes {
		specs = append(specs, c.spec)
	}
	for _, sample = range specs {
		visitor{object: fields, scalar: scalars}.visitStruct(nil, k.params, sample)
	}
	return failed
}

// outsideEnum returns a value of a scalar's type that its enum does not hold:
// a string, the integer after the enum's highest, or the bool it leaves out;
// none for an enum of both bools.
func outsideEnum(n *paramScalar) (any, bool) {
	switch n.goType {
	case typeBool:
		for _, b := range []bool{false, true} {
			if !slices.Contains(n.enum, strconv.FormatBool(b)) {
				return b, true
			}
		}
		return nil, false
	case typeInt64:
		var highest int64
		for _, value := range n.enum {
			if i, err := strconv.ParseInt(value, 10, 64); err == nil && i > highest {
				highest = i
			}
		}
		return highest + 1, true
	}
	return "kflare-not-in-enum", true
}

// listBounds returns the least number of items a list or map field takes, and
// whether its items must differ: a set's, or a map's keys.
func listBounds(node paramNode) (minItems *int64, unique bool) {
	switch n := node.(type) {
	case *paramList:
		return n.minItems, n.set || n.key != nil
	case *paramMap:
		return n.minItems, true
	}
	return nil, false
}

// tooMany returns a list one item longer than a list or map field allows,
// made of distinct copies of its first item: a string with a number after it,
// or a map's entry with a number after its key. None for a field without a
// maximum, a list of objects, or copies the field's validation refuses.
func tooMany(node paramNode, items []any) ([]any, bool) {
	var maximum *int64
	var copyOf func(i int) (any, bool)
	switch n := node.(type) {
	case *paramList:
		maximum = n.maxItems
		if n.key != nil {
			copyOf = keyedCopies(n.key, items[0].(map[string]any))
			break
		}
		first, isString := items[0].(string)
		scalar, isScalar := n.items.(*paramScalar)
		if !isString || !isScalar {
			return nil, false
		}
		copyOf = func(i int) (any, bool) {
			value := fmt.Sprintf("%s%d", first, i)
			_, err := checkScalar(scalar, value, "")
			return value, err == nil
		}
	case *paramMap:
		maximum = n.maxItems
		copyOf = keyedCopies(n.value.fields[0], items[0].(map[string]any))
	default:
		return nil, false
	}
	if maximum == nil {
		return nil, false
	}
	long := make([]any, 0, *maximum+1)
	for i := range int(*maximum) + 1 {
		item, ok := copyOf(i)
		if !ok {
			return nil, false
		}
		long = append(long, item)
	}
	return long, true
}

// keyedCopies returns copies of first, an item of a list or map keyed by key,
// each with its own key: a string key with a number after it, an integer one
// plus the number.
func keyedCopies(key *paramField, first map[string]any) func(i int) (any, bool) {
	return func(i int) (any, bool) {
		entry := maps.Clone(first)
		var name any = fmt.Sprintf("%v%d", first[key.jsonName], i)
		if n, ok := first[key.jsonName].(int64); ok {
			name = n + int64(i)
		}
		entry[key.jsonName] = name
		_, err := checkScalar(key.node.(*paramScalar), name, "")
		return entry, err == nil
	}
}

// mismatch returns a string of a length a scalar allows that its pattern
// refuses.
func mismatch(n *paramScalar) (string, bool) {
	v := n.validation
	pattern := regexp.MustCompile(v.pattern)
	for _, s := range []string{"kflare-not-matching", "KFLARE-NOT-MATCHING", "kflare not matching!", "kflare-nöt-matching",
		"", "-", "0", "/"} {
		length := int64(len(s))
		if (v.minLength == nil || length >= *v.minLength) && (v.maxLength == nil || length <= *v.maxLength) &&
			!pattern.MatchString(s) {
			return s, true
		}
	}
	return "", false
}

// sampleValues returns the first value the samples give each scalar field, or
// each item of a list of scalars.
func (k *paramKind) sampleValues() map[*paramField]any {
	values := map[*paramField]any{}
	for _, sample := range k.specs() {
		visitor{scalar: func(_ []step, f *paramField, _ *paramScalar, value any) {
			if _, ok := values[f]; !ok {
				values[f] = value
			}
		}}.visitStruct(nil, k.params, sample)
	}
	return values
}

// scalarValue returns a value of a scalar field, or of an item of a list of
// scalars, that its own validation allows: a sample's, the OpenAPI schema's
// example, or one kflare-gen makes up.
func scalarValue(f *paramField, n *paramScalar, values map[*paramField]any) (any, bool) {
	if value, ok := values[f]; ok {
		return value, true
	}
	if n.example != nil {
		if value, err := checkScalar(n, n.example, ""); err == nil {
			return value, true
		}
	}
	return someValue(n)
}

// fullSpecs adds specs that set every field of the params, to values the
// samples hold or kflare-gen makes up, and, when an optional scalar allows its
// zero value, one with every such scalar zero, which the conversion must still
// send. Each also holds base, the spec fields outside the params, and is of
// the type typeName. The generated specs convert them without the API server,
// which may refuse them: they can hold several variants of a union at once.
func (k *paramKind) fullSpecs(values map[*paramField]any, base map[string]any, typeName string) error {
	for _, zero := range []bool{false, true} {
		params, err := fullStruct(k.params, values, zero)
		if err != nil {
			return fmt.Errorf("a spec that sets every field: %w", err)
		}
		spec := map[string]any{}
		maps.Copy(spec, base)
		maps.Copy(spec, params)
		checked, err := k.check(spec, typeName)
		if err != nil {
			return fmt.Errorf("a spec that sets every field: %w", err)
		}
		what := "every field set"
		if zero {
			if reflect.DeepEqual(checked, k.full[0].spec) {
				break
			}
			what = "every field set, optional scalars to their zero values where allowed"
		}
		k.full = append(k.full, ruleEdit{what: what, spec: checked, rule: k.body(checked)})
	}
	return nil
}

// fullStruct returns an object of st that sets every field: an optional
// scalar to its zero value, with zero, when its validation allows it.
func fullStruct(st *paramStruct, values map[*paramField]any, zero bool) (map[string]any, error) {
	out := map[string]any{}
	for _, f := range st.fields {
		if n, ok := f.node.(*paramScalar); ok && zero && f.pointer() {
			if value, err := checkScalar(n, zeroValue(n), ""); err == nil {
				out[f.jsonName] = value
				continue
			}
		}
		value, err := fullNode(f, f.node, values, zero)
		if err != nil {
			return nil, err
		}
		out[f.jsonName] = value
	}
	return out, nil
}

// fullNode returns a value of a field that sets everything it holds: a list
// or a map holds one item.
func fullNode(f *paramField, node paramNode, values map[*paramField]any, zero bool) (any, error) {
	switch n := node.(type) {
	case *paramStruct:
		return fullStruct(n, values, zero)
	case *paramList:
		item, err := fullNode(f, n.items, values, zero)
		if err != nil {
			return nil, err
		}
		return []any{item}, nil
	case *paramMap:
		item, err := fullStruct(n.value, values, zero)
		if err != nil {
			return nil, err
		}
		return []any{item}, nil
	}
	if value, ok := fullScalar(f, node.(*paramScalar), values); ok {
		return value, nil
	}
	return nil, fmt.Errorf("kflare-gen cannot make up a value of %s, which the OpenAPI spec gives no valid "+
		"example of; set it in a sample", f.jsonName)
}

// fullScalar returns the value a spec that sets every field gives a scalar:
// for an enum, a value the samples do not use, when it has one, so that a
// conversion sending the samples' value whatever the spec says fails;
// otherwise the value scalarValue returns.
func fullScalar(f *paramField, n *paramScalar, values map[*paramField]any) (any, bool) {
	if sampled, ok := values[f]; ok {
		for _, value := range n.enum {
			other, err := checkScalar(n, enumValue(n, value), "")
			if err == nil && other != sampled {
				return other, true
			}
		}
	}
	return scalarValue(f, n, values)
}

// enumValue returns a value of a scalar's enum as its type holds it: an
// integer's as an int64, a bool's as a bool. A value the type cannot hold
// stays a string, which checkScalar refuses.
func enumValue(n *paramScalar, value string) any {
	switch n.goType {
	case typeInt64:
		if i, err := strconv.ParseInt(value, 10, 64); err == nil {
			return i
		}
	case typeBool:
		if b, err := strconv.ParseBool(value); err == nil {
			return b
		}
	}
	return value
}

// zeroValue returns the zero value of a scalar's Go type.
func zeroValue(n *paramScalar) any {
	switch n.goType {
	case typeBool:
		return false
	case typeInt64:
		return int64(0)
	}
	return ""
}

// ruleBreaches adds an invalid spec for each rule of the action parameters:
// the first sample that holds an object of the rule's struct, with that
// object changed to break it; for a oneOf's rule, one for each way to break
// each variant (see variantBreaches).
func (k *paramKind) ruleBreaches(values map[*paramField]any) error {
	type site struct {
		sample int
		path   []step
	}
	first, specs := map[*paramStruct]site{}, k.specs()
	var order []*paramStruct
	for i, sample := range specs {
		visitor{object: func(path []step, st *paramStruct, _ map[string]any) {
			if _, seen := first[st]; !seen && len(st.rules) > 0 {
				first[st] = site{sample: i, path: path}
				order = append(order, st)
			}
		}}.visitStruct(nil, k.params, sample)
	}
	for _, st := range order {
		s := first[st]
		for _, rule := range st.rules {
			switch {
			case rule.fromEntry:
				continue
			case len(rule.variants) > 0:
				k.variantBreaches(st, rule, s.sample, s.path, values)
				continue
			}
			spec := copySpec(specs[s.sample])
			if err := breakRule(st, rule, objectAt(spec, s.path), values); err != nil {
				return fmt.Errorf("%s: %w", pathString(s.path), err)
			}
			if err := k.breaks(spec, rule.message); err != nil {
				return fmt.Errorf("%s: %w", pathString(s.path), err)
			}
			k.invalid = append(k.invalid, ruleEdit{what: k.describeObject(s.path) + " breaking: " + rule.message,
				spec: spec, message: rule.message})
		}
	}
	return nil
}

// variantBreaches adds the invalid specs that break a oneOf's rule: for each
// variant, the object at path in a sample changed to hold the variant alone,
// then given each field outside it (the value the object had, or another
// sample's), or left without each field it requires, when no variant holds
// what results. A variant whose object kflare-gen cannot fill in is left out.
func (k *paramKind) variantBreaches(st *paramStruct, rule celRule, sample int, path []step,
	values map[*paramField]any) {
	where := k.describeObject(path)
	seen := map[string]bool{}
	for _, v := range rule.variants {
		base := objectAt(copySpec(k.specs()[sample]), path)
		object, ok := variantObject(st, v, base, values)
		if !ok {
			continue
		}
		breach := func(what string, edit func(object map[string]any)) {
			spec := copySpec(k.specs()[sample])
			target := objectAt(spec, path)
			clear(target)
			maps.Copy(target, object)
			edit(target)
			for _, other := range rule.variants {
				if other.matches(st, target) {
					return
				}
			}
			if data, err := json.Marshal(spec); err == nil && !seen[string(data)] {
				seen[string(data)] = true
				k.invalid = append(k.invalid, ruleEdit{what: where + " as " + v.describe + ", " + what, spec: spec,
					message: rule.message})
			}
		}
		for _, f := range st.fields {
			n, scalar := f.node.(*paramScalar)
			if f.key || v.holds[f.jsonName] || !scalar {
				continue
			}
			value, ok := base[f.jsonName]
			if !ok {
				value, ok = scalarValue(f, n, values)
			}
			if ok {
				breach("with "+f.jsonName+" too", func(object map[string]any) { object[f.jsonName] = value })
			}
		}
		for _, name := range v.required {
			breach("without "+name, func(object map[string]any) { delete(object, name) })
		}
	}
}

// describeObject names the object at path in messages.
func (k *paramKind) describeObject(path []step) string {
	if len(path) == 0 {
		return k.root
	}
	return relativePath(path)
}

// objectAt returns the object at path in spec: the spec itself for no path.
func objectAt(spec map[string]any, path []step) map[string]any {
	if len(path) == 0 {
		return spec
	}
	return valueAt(spec, path).(map[string]any)
}

// variantObject returns an object of st that holds a variant alone: base's
// key and the fields the variant requires, from base or values, and the
// values it fixes.
func variantObject(st *paramStruct, v variant, base map[string]any,
	values map[*paramField]any) (map[string]any, bool) {
	object := map[string]any{}
	for _, f := range st.fields {
		if value, ok := v.fixed[f.jsonName]; ok {
			object[f.jsonName] = value
			continue
		}
		if !f.key && !f.required && !slices.Contains(v.required, f.jsonName) {
			continue
		}
		value, ok := base[f.jsonName]
		if !ok {
			n, scalar := f.node.(*paramScalar)
			if !scalar {
				return nil, false
			}
			if value, ok = scalarValue(f, n, values); !ok {
				return nil, false
			}
		}
		object[f.jsonName] = value
	}
	return object, v.matches(st, object)
}

// breakRule changes an object so that it breaks rule, one of its struct's: a
// rule that a value matches an anyOf's variant, or one that bounds how many
// fields are set. Fields that must be set at least once are all unset; a rule
// that allows one at most gets the object's unset fields set, an object field
// to an object that sets everything.
func breakRule(st *paramStruct, rule celRule, object map[string]any, values map[*paramField]any) error {
	optional := func(f *paramField) bool { return !f.key && !f.required }
	switch {
	case strings.HasPrefix(rule.message, "must set at least") || strings.HasPrefix(rule.message, "must match at least"):
		for _, f := range st.fields {
			if optional(f) {
				delete(object, f.jsonName)
			}
		}
		return nil
	case strings.HasPrefix(rule.message, "must set exactly one of"), strings.HasPrefix(rule.message, "must set at most"):
		added := false
		for _, f := range st.fields {
			if _, set := object[f.jsonName]; set || !optional(f) {
				continue
			}
			value, err := fullNode(f, f.node, values, false)
			if err != nil {
				continue
			}
			object[f.jsonName] = value
			added = true
			if !strings.HasPrefix(rule.message, "must set at most") {
				break
			}
		}
		if !added {
			return fmt.Errorf("found no field to set to break %q", rule.message)
		}
		return nil
	}
	return fmt.Errorf("kflare-gen cannot break the rule %q", rule.message)
}

// someValue returns a value of a scalar field that its own validation allows.
func someValue(node paramNode) (any, bool) {
	n, ok := node.(*paramScalar)
	if !ok {
		return nil, false
	}
	switch {
	case len(n.enum) > 0:
		value := enumValue(n, n.enum[0])
		_, err := checkScalar(n, value, "")
		return value, err == nil
	case n.goType == typeBool:
		return true, true
	case n.goType == typeInt64:
		value := int64(1)
		if n.validation.minimum != nil {
			value = int64(*n.validation.minimum) + 1
		}
		return value, true
	}
	length := int64(1)
	if n.validation.minLength != nil {
		length = max(length, *n.validation.minLength)
	}
	value := strings.Repeat("x", int(length))
	if _, err := checkScalar(n, value, ""); err != nil {
		return nil, false
	}
	return value, true
}

// otherScalar returns a valid value of a scalar that differs from value:
// another value of its enum, the other bool, the next integer in range, or a
// string with its last character changed.
func otherScalar(n *paramScalar, value any) (any, error) {
	var candidates []any
	switch {
	case len(n.enum) > 0:
		for _, member := range n.enum {
			candidates = append(candidates, enumValue(n, member))
		}
	case n.goType == typeBool:
		candidates = []any{!value.(bool)}
	case n.goType == typeInt64:
		candidates = []any{value.(int64) + 1, value.(int64) - 1}
	default:
		candidates = changedStrings(value.(string))
	}
	for _, candidate := range candidates {
		if _, err := checkScalar(n, candidate, ""); err == nil && candidate != value {
			return candidate, nil
		}
	}
	return nil, fmt.Errorf("no valid value differs from %v", value)
}
