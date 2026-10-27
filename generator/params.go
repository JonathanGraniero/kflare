/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"fmt"
	"go/types"
	"slices"
	"strconv"
	"strings"
)

// A rule kind's spec holds the parameters of its action, a tree of objects,
// lists, maps and unions. paramBuilder maps the tree to the API types'
// structs, walking cloudflare-go v7's param types, which decide the fields
// and their Go types, together with the OpenAPI schema, which adds
// constraints and says how a union's variants combine:
//
//   - An object is a struct. A field that the SDK or the schema requires is
//     required; minProperties and maxProperties become a CEL rule on how many
//     of its fields are set.
//   - An array is a list: a set (listType=set) when its items are unique
//     scalars.
//   - A map is a list of its values, each holding its key in the field the
//     entry names, by which the list is keyed (listType=map), as Kubernetes's
//     API conventions prefer to a map.
//   - A union is one struct with the fields of all its variants, and a CEL
//     rule that a value matches a variant. The variants of a oneOf are closed,
//     as the live API treats them: a field outside a variant must be unset.
//     Those of an anyOf combine: at least one of them must match.

// paramStruct is an object of the action parameters: a struct of the API
// types, which the conversion turns into the SDK param struct sdk.
type paramStruct struct {
	// goName is the struct's name. The action parameters themselves are the
	// spec, whose struct the kind declares.
	goName string
	// path is the object's place in the action parameters, in API names
	// joined by dots; "" for the action parameters themselves.
	path string
	doc  []string
	// sdk is the SDK struct the conversion builds: for a union, the variant
	// that holds every field.
	sdk    *types.Named
	fields []*paramField
	// rules are CEL rules on the object.
	rules []celRule
}

// paramField is one field of a paramStruct.
type paramField struct {
	apiName, goName, jsonName string
	doc                       []string
	required                  bool
	node                      paramNode
	// key marks the field a map's entry keeps its key in, which has no SDK
	// field.
	key bool
	// sdkGoName is the SDK struct's field the conversion sets, and sdkType the
	// type inside its param.Field: an empty interface when a union's variant
	// that holds every field leaves the type to the other variants.
	sdkGoName string
	sdkType   types.Type
}

// celRule is a CEL rule, and what a value that breaks it is refused with.
type celRule struct {
	rule, message string
	// variants are a oneOf's variants, for the rule that a value matches one
	// of them; the generated specs break each.
	variants []variant
	// fromEntry marks an entry's rule, whose invalid spec the entry gives.
	fromEntry bool
}

// variant is a oneOf's variant as its rule tells it apart, by the JSON names
// of its fields: those it fixes to one value, those it requires that the
// struct does not, and every field it holds.
type variant struct {
	describe string
	fixed    map[string]string
	required []string
	holds    map[string]bool
}

// matches reports whether an object of st is one of the variant's.
func (v variant) matches(st *paramStruct, object map[string]any) bool {
	for name, value := range v.fixed {
		if object[name] != value {
			return false
		}
	}
	for _, name := range v.required {
		if _, ok := object[name]; !ok {
			return false
		}
	}
	for _, f := range st.fields {
		if _, set := object[f.jsonName]; set && !f.key && !v.holds[f.jsonName] {
			return false
		}
	}
	return true
}

// paramNode is the type of a field: a *paramScalar, *paramStruct, *paramList
// or *paramMap.
type paramNode interface{ isParamNode() }

func (*paramScalar) isParamNode() {}
func (*paramStruct) isParamNode() {}
func (*paramList) isParamNode()   {}
func (*paramMap) isParamNode()    {}

// paramScalar is a string, a bool or an integer.
type paramScalar struct {
	// goType is typeString, typeBool or typeInt64.
	goType string
	// sdk is the SDK's type, such as a named string of enum constants, which
	// the conversion converts to.
	sdk        types.Type
	enum       []string
	validation validation
	// example is the OpenAPI schema's example of a value, which specs that
	// need a value no sample gives may take.
	example any
}

// paramList is an array.
type paramList struct {
	items paramNode
	// set is true for unique scalars, which the conversion sorts.
	set bool
	// key is the field of a list of objects that tells its items apart, when
	// the entry names one (listMapKey): the list is keyed by it.
	key                *paramField
	minItems, maxItems *int64
	// sdkItems is the type of the SDK slice's items.
	sdkItems types.Type
}

// paramMap is an object keyed by names, which the spec lists.
type paramMap struct {
	// value is the struct of each entry, its key field first.
	value              *paramStruct
	minItems, maxItems *int64
	// sdkValue is the type of the SDK map's values: a struct, or a union.
	sdkValue types.Type
}

// paramBuilder builds a rule kind's action parameters.
type paramBuilder struct {
	// kind is the kind's name, which the names of its nested structs start
	// with.
	kind   string
	r      *sdkReader
	spec   *apiSpec
	adjust map[string]ruleFieldConfig
	ignore map[string]string
	// used records the entry's paths that matched a field.
	used map[string]bool
	// structs are the nested structs, in the order they appear: a parent
	// before its children.
	structs []*paramStruct
	// zoneParams is set when the root struct is a call's params, whose zone_id
	// the shared settings controller sets, rather than the spec.
	zoneParams bool
	// root is what messages call the root struct, such as "the action
	// parameters".
	root string
}

// joinPath appends a field's API name to a path.
func joinPath(path, apiName string) string {
	if path == "" {
		return apiName
	}
	return path + "." + apiName
}

// singular returns the name of one item of a list or map field: Headers is a
// list of Header.
func singular(goName string) string {
	switch {
	case strings.HasSuffix(goName, "ies"):
		return strings.TrimSuffix(goName, "ies") + "y"
	case strings.HasSuffix(goName, "s") && !strings.HasSuffix(goName, "ss"):
		return strings.TrimSuffix(goName, "s")
	}
	return goName
}

// object builds the struct goName for the SDK param struct sdkType, at path,
// with the schema sch. Its nested structs' names start with prefix.
func (b *paramBuilder) object(path, goName, prefix string, sdkType *types.Named, sch *schema) (*paramStruct, error) {
	fields, err := b.r.fields(sdkType, true)
	if err != nil {
		return nil, err
	}
	resolved, err := b.spec.resolve(sch)
	if err != nil {
		return nil, err
	}
	props, err := b.spec.properties(sch)
	if err != nil {
		return nil, err
	}
	required, err := b.spec.required(sch)
	if err != nil {
		return nil, err
	}
	st := &paramStruct{goName: goName, path: path, sdk: sdkType}
	if path != "" {
		b.structs = append(b.structs, st)
		st.doc = wrap(fmt.Sprintf("%s is %s in %s.", goName, path, article(b.kind)))
	}
	for _, f := range fields {
		fieldPath := joinPath(path, f.apiName)
		if path == "" && b.zoneParams && isZoneID(f) {
			continue
		}
		if b.ignored(fieldPath) {
			continue
		}
		if f.in != inBody {
			return nil, fmt.Errorf("%s is a %s parameter, which kflare-gen cannot map yet; ignore it with a reason",
				fieldPath, f.in)
		}
		pf, err := b.field(fieldPath, prefix, f, f.typ, props[f.apiName], f.required || required[f.apiName])
		if err != nil {
			return nil, err
		}
		st.fields = append(st.fields, pf)
	}
	if len(st.fields) == 0 {
		return nil, fmt.Errorf("%s: every field is ignored", b.describePath(path))
	}
	st.rules = append(st.rules, countRules(resolved, st.fields)...)
	st.rules = append(st.rules, b.entryRules(path)...)
	return st, nil
}

// entryRules returns the CEL rules the entry puts on the object at path.
func (b *paramBuilder) entryRules(path string) []celRule {
	adjust := b.adjust[path]
	if len(adjust.Rules) == 0 {
		return nil
	}
	b.used[path] = true
	rules := make([]celRule, 0, len(adjust.Rules))
	for _, r := range adjust.Rules {
		rules = append(rules, celRule{rule: r.Rule, message: r.Message, fromEntry: true})
	}
	return rules
}

// describePath names a path in messages.
func (b *paramBuilder) describePath(path string) string {
	if path == "" {
		return b.root
	}
	return path
}

// field builds the field for f, of type t, at path, with the schema sch.
func (b *paramBuilder) field(path, prefix string, f sdkField, t types.Type, sch *schema,
	required bool) (*paramField, error) {
	adjust, adjusted := b.adjust[path]
	if adjusted {
		b.used[path] = true
	}
	pf := &paramField{
		apiName: f.apiName, goName: f.goName, jsonName: lowerCamel(f.goName), required: required,
		sdkGoName: f.goName, sdkType: f.typ,
	}
	doc := adjust.Doc
	if doc == "" {
		doc = f.doc
	}
	pf.doc = lines(doc)
	node, err := b.node(path, prefix, f.goName, t, sch, adjust)
	if err != nil {
		return nil, err
	}
	pf.node = node
	if _, isStruct := node.(*paramStruct); !isStruct && len(adjust.Rules) > 0 {
		return nil, fmt.Errorf("fields.%s.rules: %s is not an object; put a list's or a map's rules on %s[]", path,
			path, path)
	}
	if _, isMap := node.(*paramMap); !isMap && adjust.Key != "" {
		return nil, fmt.Errorf("fields.%s.key: %s is not a map", path, path)
	}
	if list, isList := node.(*paramList); (!isList || list.key == nil) && adjust.ListMapKey != "" {
		return nil, fmt.Errorf("fields.%s.listMapKey: %s is not a list of objects", path, path)
	}
	if _, isScalar := node.(*paramScalar); !isScalar && adjust.Validation != nil {
		return nil, fmt.Errorf("fields.%s.validation: only a string or an integer is validated", path)
	}
	return pf, nil
}

// node builds the type of the field goName at path, of SDK type t.
func (b *paramBuilder) node(path, prefix, goName string, t types.Type, sch *schema,
	adjust ruleFieldConfig) (paramNode, error) {
	if union, ok := b.r.isUnion(t); ok {
		return b.union(path, prefix+goName, union, sch)
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return b.scalar(path, t, sch, adjust)
	case *types.Struct:
		named, ok := t.(*types.Named)
		if !ok || named.Obj().Pkg() != b.r.pkg.Types {
			return nil, fmt.Errorf("%s is a %s, which kflare-gen cannot map", path, t)
		}
		return b.object(path, prefix+goName, prefix+goName, named, sch)
	case *types.Slice:
		return b.list(path, prefix, goName, u.Elem(), sch, adjust)
	case *types.Map:
		return b.mapOf(path, prefix, goName, u, sch, adjust)
	case *types.Interface:
		return nil, fmt.Errorf("%s is untyped in the SDK, which kflare-gen cannot map; ignore it with a reason", path)
	}
	return nil, fmt.Errorf("%s is a %s, which kflare-gen cannot map; ignore it with a reason", path, t)
}

// scalar builds a string, bool or integer field. cloudflare-go v7 types every
// number as a float64, so only the spec can tell an integer.
func (b *paramBuilder) scalar(path string, t types.Type, sch *schema, adjust ruleFieldConfig) (*paramScalar, error) {
	fs, err := b.spec.fieldSpec(sch)
	if err != nil {
		return nil, fmt.Errorf("%s in the OpenAPI spec: %w", path, err)
	}
	if fs.sensitive {
		return nil, fmt.Errorf("%s is sensitive (x-sensitive), and kflare-gen cannot keep it in a Secret yet; "+
			"ignore it with a reason", path)
	}
	out := &paramScalar{sdk: t, enum: fs.enum, example: fs.example}
	if out.enum == nil {
		out.enum = b.r.enumValues(t)
	}
	integer := adjust.Validation != nil && adjust.Validation.Integer
	if integer && (basicClass(t) != types.IsNumeric || fs.typ == typeInteger) {
		return nil, fmt.Errorf("fields.%s.validation.integer: only a number the OpenAPI spec does not say is an "+
			"integer is made one", path)
	}
	switch basicClass(t) {
	case types.IsString:
		out.goType = typeString
		out.validation = validation{minLength: fs.validation.minLength, maxLength: fs.validation.maxLength,
			pattern: fs.validation.pattern}
	case types.IsBoolean:
		for _, value := range out.enum {
			if _, err := strconv.ParseBool(value); err != nil {
				return nil, fmt.Errorf("%s is a bool whose enum holds %q", path, value)
			}
		}
		out.goType = typeBool
	case types.IsNumeric:
		if fs.typ != typeInteger && !integer {
			return nil, fmt.Errorf("%s is a number the OpenAPI spec does not say is an integer, which a CRD cannot "+
				"hold well; ignore it with a reason, or make it an integer (validation.integer) with one", path)
		}
		for _, value := range out.enum {
			if _, err := strconv.ParseInt(value, 10, 64); err != nil {
				return nil, fmt.Errorf("%s is an integer whose enum holds %q", path, value)
			}
		}
		out.goType = typeInt64
		v := fs.validation
		out.validation = validation{minimum: v.minimum, maximum: v.maximum,
			exclusiveMinimum: v.exclusiveMinimum, exclusiveMaximum: v.exclusiveMaximum}
	default:
		return nil, fmt.Errorf("%s is a %s, which kflare-gen cannot map; ignore it with a reason", path, t)
	}
	if override := adjust.Validation; override != nil {
		lengths := override.MinLength != nil || override.MaxLength != nil || override.Pattern != ""
		if lengths && out.goType != typeString {
			return nil, fmt.Errorf("fields.%s.validation: lengths and patterns need a string field", path)
		}
		if (override.Minimum != nil || override.Maximum != nil) && out.goType != typeInt64 {
			return nil, fmt.Errorf("fields.%s.validation: a range needs an integer field", path)
		}
		out.validation = override.validation()
	}
	return out, nil
}

// list builds an array field: a list of scalars or of structs, keyed by the
// field the entry names.
func (b *paramBuilder) list(path, prefix, goName string, elem types.Type, sch *schema,
	adjust ruleFieldConfig) (*paramList, error) {
	resolved, err := b.spec.resolve(sch)
	if err != nil {
		return nil, err
	}
	var items *schema
	out := &paramList{sdkItems: elem}
	if resolved != nil {
		items = resolved.Items
		out.minItems, out.maxItems = resolved.MinItems, resolved.MaxItems
	}
	node, err := b.node(path+"[]", prefix, singular(goName), elem, items, ruleFieldConfig{})
	if err != nil {
		return nil, err
	}
	switch n := node.(type) {
	case *paramScalar:
		out.set = resolved != nil && resolved.UniqueItems
	case *paramStruct:
		if adjust.ListMapKey == "" {
			break
		}
		i := slices.IndexFunc(n.fields, func(f *paramField) bool { return f.apiName == adjust.ListMapKey })
		if i < 0 {
			return nil, fmt.Errorf("fields.%s.listMapKey: the items of %s have no field %s", path, path,
				adjust.ListMapKey)
		}
		key := n.fields[i]
		scalar, ok := key.node.(*paramScalar)
		if !ok || !key.required || (scalar.goType != typeString && scalar.goType != typeInt64) {
			return nil, fmt.Errorf("fields.%s.listMapKey: %s is not a required string or integer, which a list "+
				"is keyed by", path, adjust.ListMapKey)
		}
		out.key = key
	default:
		return nil, fmt.Errorf("%s is a list of lists or maps, which kflare-gen cannot map yet; ignore it with a reason",
			path)
	}
	out.items = node
	return out, nil
}

// mapOf builds a map field: a list of its values, keyed by the field the entry
// names.
func (b *paramBuilder) mapOf(path, prefix, goName string, m *types.Map, sch *schema,
	adjust ruleFieldConfig) (*paramMap, error) {
	if basicClass(m.Key()) != types.IsString {
		return nil, fmt.Errorf("%s is a map whose keys are not strings, which kflare-gen cannot map", path)
	}
	if adjust.Key == "" {
		return nil, fmt.Errorf("%s is a map: name the field its keys go in with fields.%s.key and keyDoc", path, path)
	}
	resolved, err := b.spec.resolve(sch)
	if err != nil {
		return nil, err
	}
	var values *schema
	out := &paramMap{sdkValue: m.Elem()}
	if resolved != nil {
		if values, err = resolved.valueSchema(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out.minItems, out.maxItems = resolved.MinProperties, resolved.MaxProperties
	}
	node, err := b.node(path+"[]", prefix, singular(goName), m.Elem(), values, ruleFieldConfig{})
	if err != nil {
		return nil, err
	}
	value, ok := node.(*paramStruct)
	if !ok {
		return nil, fmt.Errorf("%s is a map of scalars or lists, which kflare-gen cannot map yet; ignore it with a "+
			"reason", path)
	}
	keyLength := int64(1)
	keyValidation := validation{minLength: &keyLength}
	if adjust.KeyValidation != nil {
		keyValidation = adjust.KeyValidation.validation()
	}
	key := &paramField{
		goName: upperCamel(adjust.Key), jsonName: adjust.Key, required: true, key: true,
		doc: lines(adjust.KeyDoc), node: &paramScalar{goType: typeString, validation: keyValidation},
	}
	for _, f := range value.fields {
		if f.jsonName == key.jsonName {
			return nil, fmt.Errorf("fields.%s.key: the values already have a field %s", path, key.jsonName)
		}
	}
	value.fields = append([]*paramField{key}, value.fields...)
	value.doc = wrap(fmt.Sprintf("%s is an item of %s in %s.", value.goName, path, article(b.kind)))
	out.value = value
	return out, nil
}

// union builds a union field: one struct with the fields of every variant,
// built as the variant that holds them all, and a rule that a value matches a
// variant.
func (b *paramBuilder) union(path, goName string, union *types.Named, sch *schema) (*paramStruct, error) {
	variants := b.r.variants(union)
	fieldsOf := map[*types.Named][]sdkField{}
	for _, v := range variants {
		fields, err := b.r.fields(v, true)
		if err != nil {
			return nil, err
		}
		fieldsOf[v] = fields
	}
	var full *types.Named
	for _, v := range variants {
		if holdsAll(fieldsOf[v], variants, fieldsOf) && (full == nil || len(fieldsOf[v]) > len(fieldsOf[full])) {
			full = v
		}
	}
	if full == nil {
		return nil, fmt.Errorf("%s is a union of %s without a variant that holds every field, which kflare-gen "+
			"cannot build yet", path, union.Obj().Name())
	}

	resolved, err := b.spec.resolve(sch)
	if err != nil {
		return nil, err
	}
	combinator, alternatives, err := b.spec.alternatives(resolved)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if combinator == "" {
		return nil, fmt.Errorf("%s is a union in the SDK, but its OpenAPI schema has no oneOf or anyOf", path)
	}
	variantProps := make([]map[string]*schema, len(alternatives))
	variantRequired := make([]map[string]bool, len(alternatives))
	merged := map[string]*schema{}
	for i, alt := range alternatives {
		if variantProps[i], err = b.spec.properties(alt); err != nil {
			return nil, err
		}
		if variantRequired[i], err = b.spec.required(alt); err != nil {
			return nil, err
		}
		for name, prop := range variantProps[i] {
			if merged[name], err = b.mergeEnums(merged[name], prop); err != nil {
				return nil, fmt.Errorf("%s.%s: %w", path, name, err)
			}
		}
	}

	st := &paramStruct{goName: goName, path: path, sdk: full}
	b.structs = append(b.structs, st)
	st.doc = wrap(fmt.Sprintf("%s is %s in %s.", goName, path, article(b.kind)))
	for _, f := range fieldsOf[full] {
		fieldPath := joinPath(path, f.apiName)
		if b.ignored(fieldPath) {
			continue
		}
		typ := f.typ
		if isEmptyInterface(typ) {
			if typ, err = variantType(f.apiName, variants, fieldsOf); err != nil {
				return nil, fmt.Errorf("%s: %w", fieldPath, err)
			}
		}
		if f.doc == "" {
			f.doc = variantDoc(f.apiName, variants, fieldsOf)
		}
		required := len(alternatives) > 0
		for i := range alternatives {
			required = required && variantRequired[i][f.apiName]
		}
		pf, err := b.field(fieldPath, goName, f, typ, merged[f.apiName], required)
		if err != nil {
			return nil, err
		}
		pf.sdkType = f.typ
		st.fields = append(st.fields, pf)
	}
	rule, err := b.variantRule(combinator, st, variantProps, variantRequired)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	st.rules = append(st.rules, rule)
	st.rules = append(st.rules, countRules(resolved, st.fields)...)
	st.rules = append(st.rules, b.entryRules(path)...)
	return st, nil
}

// holdsAll reports whether a variant's fields include every other variant's.
func holdsAll(fields []sdkField, variants []*types.Named, fieldsOf map[*types.Named][]sdkField) bool {
	names := map[string]bool{}
	for _, f := range fields {
		names[f.apiName] = true
	}
	for _, v := range variants {
		for _, f := range fieldsOf[v] {
			if !names[f.apiName] {
				return false
			}
		}
	}
	return true
}

// isEmptyInterface reports whether t is interface{}.
func isEmptyInterface(t types.Type) bool {
	iface, ok := t.Underlying().(*types.Interface)
	return ok && iface.NumMethods() == 0
}

// variantType returns the type the variants that type the field apiName give
// it, which must agree.
func variantType(apiName string, variants []*types.Named, fieldsOf map[*types.Named][]sdkField) (types.Type, error) {
	var typ types.Type
	for _, v := range variants {
		for _, f := range fieldsOf[v] {
			if f.apiName != apiName || isEmptyInterface(f.typ) {
				continue
			}
			if typ != nil && !types.Identical(typ, f.typ) {
				return nil, fmt.Errorf("the variants type it differently: %s and %s", typ, f.typ)
			}
			typ = f.typ
		}
	}
	if typ == nil {
		return nil, fmt.Errorf("no variant gives it a type")
	}
	return typ, nil
}

// variantDoc returns the documentation the first variant that documents the
// field apiName gives it.
func variantDoc(apiName string, variants []*types.Named, fieldsOf map[*types.Named][]sdkField) string {
	for _, v := range variants {
		for _, f := range fieldsOf[v] {
			if f.apiName == apiName && f.doc != "" {
				return f.doc
			}
		}
	}
	return ""
}

// mergeEnums returns the schema of a union's field that variants declare
// alike, but for the values of an enum: each variant may allow its own, so the
// struct allows them all. existing is what the variants before declare, nil
// for none.
func (b *paramBuilder) mergeEnums(existing, prop *schema) (*schema, error) {
	if existing == nil {
		return prop, nil
	}
	had, err := b.spec.fieldSpec(existing)
	if err != nil {
		return nil, err
	}
	adds, err := b.spec.fieldSpec(prop)
	if err != nil {
		return nil, err
	}
	if len(had.enum) == 0 || len(adds.enum) == 0 {
		return existing, nil
	}
	values := slices.Clone(had.enum)
	for _, v := range adds.enum {
		if !slices.Contains(values, v) {
			values = append(values, v)
		}
	}
	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = v
	}
	// A keyword a schema sets itself wins over one it combines in.
	return &schema{AllOf: []*schema{existing}, Enum: enum}, nil
}

// variantRule returns the rule that a union's value matches one of its
// variants: one oneOf variant exactly, its fields and no other, or any number
// of anyOf variants.
func (b *paramBuilder) variantRule(combinator string, st *paramStruct, props []map[string]*schema,
	required []map[string]bool) (celRule, error) {
	var alternatives, descriptions []string
	var variants []variant
	singleFields := true
	for i := range props {
		var conditions, describe []string
		v := variant{fixed: map[string]string{}, holds: map[string]bool{}}
		for _, f := range st.fields {
			prop, inVariant := props[i][f.apiName]
			if !inVariant {
				if combinator == combinatorOneOf {
					conditions = append(conditions, "!has(self."+f.jsonName+")")
				}
				continue
			}
			v.holds[f.jsonName] = true
			fs, err := b.spec.fieldSpec(prop)
			if err != nil {
				return celRule{}, err
			}
			if len(fs.enum) == 1 {
				condition := fmt.Sprintf("self.%s == '%s'", f.jsonName, fs.enum[0])
				if !f.required {
					condition = "has(self." + f.jsonName + ") && " + condition
				}
				conditions = append(conditions, condition)
				describe = append(describe, f.jsonName+" "+fs.enum[0])
				v.fixed[f.jsonName] = fs.enum[0]
				continue
			}
			if required[i][f.apiName] && !f.required {
				conditions = append(conditions, "has(self."+f.jsonName+")")
				describe = append(describe, "with "+f.jsonName)
				v.required = append(v.required, f.jsonName)
			}
		}
		if len(conditions) == 0 {
			return celRule{}, fmt.Errorf("variant %d requires nothing kflare-gen can tell apart", i+1)
		}
		singleFields = singleFields && len(describe) == 1 && strings.HasPrefix(describe[0], "with ")
		alternative := strings.Join(conditions, " && ")
		if len(conditions) > 1 {
			alternative = "(" + alternative + ")"
		}
		alternatives = append(alternatives, alternative)
		descriptions = append(descriptions, strings.Join(describe, " "))
		v.describe = descriptions[i]
		variants = append(variants, v)
	}
	rule := celRule{rule: strings.Join(alternatives, " || ")}
	if combinator == combinatorOneOf {
		rule.variants = variants
	}
	switch {
	case combinator == combinatorAnyOf && singleFields:
		var names []string
		for _, d := range descriptions {
			names = append(names, strings.TrimPrefix(d, "with "))
		}
		rule.message = "must set at least one of " + prose(names)
	case combinator == combinatorAnyOf:
		rule.message = "must match at least one of: " + strings.Join(descriptions, "; ")
	default:
		rule.message = "must be one of: " + strings.Join(descriptions, "; ")
	}
	return rule, nil
}

// countRules returns the rules that bound how many of an object's fields are
// set. An object left with one field, as when the entry ignores the others,
// needs none: a minimum makes the field required instead, and a maximum holds
// anyway.
func countRules(sch *schema, fields []*paramField) []celRule {
	var settable []*paramField
	for _, f := range fields {
		if !f.key {
			settable = append(settable, f)
		}
	}
	if len(settable) != 1 {
		return propertyCountRules(sch, fields)
	}
	if sch != nil && sch.MinProperties != nil && *sch.MinProperties > 0 {
		settable[0].required = true
	}
	return nil
}

// propertyCountRules returns the rules that bound how many of an object's
// fields are set, from its minProperties and maxProperties.
func propertyCountRules(sch *schema, fields []*paramField) []celRule {
	if sch == nil || (sch.MinProperties == nil && sch.MaxProperties == nil) {
		return nil
	}
	var set, counts, names []string
	for _, f := range fields {
		if f.key {
			continue
		}
		set = append(set, "has(self."+f.jsonName+")")
		counts = append(counts, "(has(self."+f.jsonName+") ? 1 : 0)")
		names = append(names, f.jsonName)
	}
	// The count is a sum rather than a filtered list: CEL's cost estimate,
	// which the API server checks against a budget, is far lower for it, and
	// an object in a list without maxItems may be checked many times over.
	minimum, maximum, count := sch.MinProperties, sch.MaxProperties, strings.Join(counts, " + ")
	switch {
	case minimum != nil && maximum != nil && *minimum == 1 && *maximum == 1:
		return []celRule{{rule: count + " == 1", message: "must set exactly one of " + prose(names)}}
	case minimum != nil && *minimum == 1 && maximum == nil:
		return []celRule{{rule: strings.Join(set, " || "), message: "must set at least one of " + prose(names)}}
	}
	var rules []celRule
	if minimum != nil && *minimum > 0 {
		rules = append(rules, celRule{rule: fmt.Sprintf("%s >= %d", count, *minimum),
			message: fmt.Sprintf("must set at least %d of %s", *minimum, prose(names))})
	}
	if maximum != nil {
		rules = append(rules, celRule{rule: fmt.Sprintf("%s <= %d", count, *maximum),
			message: fmt.Sprintf("must set at most %d of %s", *maximum, prose(names))})
	}
	return rules
}

// ignored reports whether the entry ignores the field at path.
func (b *paramBuilder) ignored(path string) bool {
	if _, ok := b.ignore[path]; ok {
		b.used[path] = true
		return true
	}
	return false
}
