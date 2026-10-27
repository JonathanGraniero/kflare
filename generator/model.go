/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"fmt"
	"go/types"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// kind is the model of a generated kind: what its types file declares.
type kind struct {
	name, shortName, noun string
	// plural is the kind's resource name, such as kvnamespaces.
	plural string
	// doc is the kind's documentation, one entry per line.
	doc []string
	// claimLabel is the value of the kind's claim label constant.
	claimLabel string
	// specRules are CEL rules on the spec as a whole.
	specRules []string
	// spec holds the spec's fields, accountRef first; metadata those of
	// status.cloudflareMetadata, the ID first.
	spec, metadata []field
	printColumns   []printColumn
	// fixed holds the values kflare sends for fields the spec leaves out, by
	// API name.
	fixed map[string]string
}

// field is one field of a generated struct.
type field struct {
	// apiName is the field's name in the API, such as title; "" for
	// accountRef.
	apiName                  string
	goName, jsonName, goType string
	doc                      []string
	optional, immutable      bool
	validation               validation
	enum                     []string
	// excluded are the values of the API description's enum that the entry's
	// enum leaves out, which the CRD must refuse too.
	excluded []string
	// def is the field's default, as a marker value; "" when there is none.
	def string
}

// validation bounds a field's value: a string's length and pattern, or an
// integer's range.
type validation struct {
	minLength, maxLength               *int64
	pattern                            string
	minimum, maximum                   *float64
	exclusiveMinimum, exclusiveMaximum bool
}

// printColumn is one of the kind's additional printer columns.
type printColumn struct {
	name, typ, jsonPath string
}

// readyColumn and ageColumn are the print columns every kind's list ends
// with.
var (
	readyColumn = printColumn{name: "Ready", typ: typeString, jsonPath: `.status.conditions[?(@.type=="Ready")].status`}
	ageColumn   = printColumn{name: "Age", typ: "date", jsonPath: ".metadata.creationTimestamp"}
)

// typeString is a string, both as a Go type and as a print column's type;
// typeInt64 and typeBool are the Go types of an integer and a bool, and
// typeInteger an integer's type in the OpenAPI spec and in a print column.
const (
	typeString  = "string"
	typeInt64   = "int64"
	typeBool    = "bool"
	typeInteger = "integer"
)

// markers returns a field's kubebuilder markers.
func (f field) markers() []string {
	var out []string
	if len(f.enum) > 0 {
		out = append(out, "+kubebuilder:validation:Enum="+strings.Join(f.enum, ";"))
	}
	v := f.validation
	if v.minLength != nil {
		out = append(out, fmt.Sprintf("+kubebuilder:validation:MinLength=%d", *v.minLength))
	}
	if v.maxLength != nil {
		out = append(out, fmt.Sprintf("+kubebuilder:validation:MaxLength=%d", *v.maxLength))
	}
	if v.pattern != "" {
		out = append(out, fmt.Sprintf("+kubebuilder:validation:Pattern=`%s`", v.pattern))
	}
	if v.minimum != nil {
		out = append(out, "+kubebuilder:validation:Minimum="+formatNumber(*v.minimum))
		if v.exclusiveMinimum {
			out = append(out, "+kubebuilder:validation:ExclusiveMinimum=true")
		}
	}
	if v.maximum != nil {
		out = append(out, "+kubebuilder:validation:Maximum="+formatNumber(*v.maximum))
		if v.exclusiveMaximum {
			out = append(out, "+kubebuilder:validation:ExclusiveMaximum=true")
		}
	}
	if f.def != "" {
		out = append(out, "+kubebuilder:default="+f.def)
	}
	if f.immutable {
		out = append(out, immutable(f.jsonName))
	}
	return out
}

// formatNumber writes a bound without a needless fraction.
func formatNumber(n float64) string {
	return strconv.FormatFloat(n, 'f', -1, 64)
}

// immutable returns the CEL rule that keeps a field as it was created.
func immutable(jsonName string) string {
	return fmt.Sprintf(`+kubebuilder:validation:XValidation:rule="self == oldSelf",message="%s is immutable"`, jsonName)
}

// buildKind builds the model of entry's kind from its service and the
// OpenAPI spec. Every field of the service's create params and of the object
// it returns is placed, fixed or ignored; an entry that names a field the
// service does not have is refused, so that a misspelled name or a field an
// SDK bump removed shows up.
func buildKind(entry kindConfig, svc *service, spec *apiSpec) (*kind, error) {
	b := &kindBuilder{entry: entry, svc: svc, spec: spec, used: map[string]bool{}}
	k, err := b.build()
	if err != nil {
		return nil, fmt.Errorf("%s (%s): %w", entry.Kind, entry.Service, err)
	}
	return k, nil
}

type kindBuilder struct {
	entry kindConfig
	svc   *service
	spec  *apiSpec
	// createProps and objectProps are the spec's properties of the create
	// request and of the object Get returns.
	createProps, objectProps map[string]*schema
	// used records the entry's field, fixed and ignore names that matched a
	// field.
	used map[string]bool
}

func (b *kindBuilder) build() (*kind, error) {
	e := b.entry
	if !slices.ContainsFunc(b.svc.create, isAccountID) {
		return nil, fmt.Errorf("scope account, but New takes no account_id")
	}
	if err := b.loadSpec(); err != nil {
		return nil, err
	}
	k := &kind{name: e.Kind, shortName: e.ShortName, noun: e.Noun, plural: plural(e.Kind), doc: lines(e.Doc),
		claimLabel: e.ClaimLabel, fixed: map[string]string{}}
	k.spec = append(k.spec, field{
		goName: "AccountRef", jsonName: "accountRef", goType: "corev1.LocalObjectReference", immutable: true,
		doc: []string{
			fmt.Sprintf("AccountRef references the CloudflareAccount that owns the %s.", e.Noun),
			"Immutable after creation.",
		},
	})

	inCreate := map[string]bool{}
	var specColumns []printColumn
	for _, f := range b.svc.create {
		inCreate[f.apiName] = true
		if isAccountID(f) {
			continue
		}
		if value, ok := e.Fixed[f.apiName]; ok {
			b.used[f.apiName] = true
			if err := b.checkFixed(f, value); err != nil {
				return nil, err
			}
			k.fixed[f.apiName] = value
			continue
		}
		if b.ignored(f.apiName) {
			if f.required {
				return nil, fmt.Errorf("%s is required to create the object, so it cannot be ignored", f.apiName)
			}
			continue
		}
		if f.in != inBody {
			return nil, fmt.Errorf("%s is a %s parameter, which kflare-gen cannot map yet; ignore it with a reason",
				f.apiName, f.in)
		}
		out, column, err := b.field(f, true)
		if err != nil {
			return nil, err
		}
		if !b.updatable(f.apiName) {
			out.immutable = true
			if out.optional {
				// self == oldSelf holds only while the field is set: it must
				// not be added or removed either.
				k.specRules = append(k.specRules, fmt.Sprintf(
					`+kubebuilder:validation:XValidation:rule="has(self.%[1]s) == has(oldSelf.%[1]s)",message="%[1]s is immutable"`,
					out.jsonName))
			}
		}
		k.spec = append(k.spec, out)
		specColumns = append(specColumns, column...)
	}
	for _, f := range b.svc.update {
		if f.in == inBody && !inCreate[f.apiName] && !b.ignored(f.apiName) {
			return nil, fmt.Errorf("%s can be updated but not set at creation, which kflare-gen cannot map yet; "+
				"ignore it with a reason", f.apiName)
		}
	}

	statusColumns, err := b.metadata(k, inCreate)
	if err != nil {
		return nil, err
	}
	k.printColumns = append(append(specColumns, statusColumns...), readyColumn, ageColumn)

	sections := []struct {
		name  string
		names []string
	}{
		{"ignore", slices.Sorted(maps.Keys(e.Ignore))},
		{"fixed", slices.Sorted(maps.Keys(e.Fixed))},
		{"fields", slices.Sorted(maps.Keys(e.Fields))},
	}
	for _, section := range sections {
		for _, name := range section.names {
			if !b.used[name] {
				return nil, fmt.Errorf("%s.%s: the service has no such field", section.name, name)
			}
		}
	}
	return k, nil
}

// loadSpec reads the spec's properties of the create request and of the
// object Get returns.
func (b *kindBuilder) loadSpec() error {
	create, err := b.spec.operation(b.svc.newRoute)
	if err != nil {
		return err
	}
	if b.createProps, err = b.spec.requestProperties(create); err != nil {
		return fmt.Errorf("%s: %w", b.svc.newRoute, err)
	}
	get, err := b.spec.operation(b.svc.getRoute)
	if err != nil {
		return err
	}
	if b.objectProps, err = b.spec.resultProperties(get); err != nil {
		return fmt.Errorf("%s: %w", b.svc.getRoute, err)
	}
	return nil
}

// metadata adds the status fields to k: the ID, then the fields of the object
// that the create params do not hold. It returns their print columns.
func (b *kindBuilder) metadata(k *kind, inCreate map[string]bool) ([]printColumn, error) {
	e := b.entry
	id, ok := b.objectField(e.ID.Field)
	if !ok {
		return nil, fmt.Errorf("id.field: the object has no field %s", e.ID.Field)
	}
	if !isString(id.typ) {
		return nil, fmt.Errorf("id.field: %s is a %s, not a string", e.ID.Field, id.typ)
	}
	idDoc := e.ID.Doc
	if idDoc == "" {
		idDoc = id.doc
	}
	k.metadata = append(k.metadata, field{
		apiName: e.ID.Field, goName: upperCamel(e.ID.Status), jsonName: e.ID.Status, goType: typeString,
		doc: lines(idDoc), optional: true,
	})
	idColumn := e.ID.PrintColumn
	if idColumn == "" {
		idColumn = humanize(e.ID.Status)
	}
	columns := []printColumn{{name: idColumn, typ: typeString, jsonPath: ".status.cloudflareMetadata." + e.ID.Status}}
	for _, f := range b.svc.object {
		// The object's other fields that the spec sets are the spec's.
		if f.apiName == e.ID.Field || inCreate[f.apiName] || b.ignored(f.apiName) {
			continue
		}
		out, column, err := b.field(f, false)
		if err != nil {
			return nil, err
		}
		k.metadata = append(k.metadata, out)
		columns = append(columns, column...)
	}
	return columns, nil
}

// checkFixed checks the value an entry fixes a field to.
func (b *kindBuilder) checkFixed(f sdkField, value string) error {
	if f.in != inBody || !isString(f.typ) {
		return fmt.Errorf("fixed.%s: only a string the request body holds can be fixed", f.apiName)
	}
	fs, err := b.fieldSpec(b.createProps, f.apiName)
	if err != nil {
		return err
	}
	enum := fs.enum
	if enum == nil {
		enum = f.enum
	}
	if enum != nil && !slices.Contains(enum, value) {
		return fmt.Errorf("fixed.%s: %q is not one of %s", f.apiName, value, strings.Join(enum, ", "))
	}
	return nil
}

// ignored reports whether the entry ignores the field apiName.
func (b *kindBuilder) ignored(apiName string) bool {
	if _, ok := b.entry.Ignore[apiName]; ok {
		b.used[apiName] = true
		return true
	}
	return false
}

// updatable reports whether the service's update takes the field apiName.
func (b *kindBuilder) updatable(apiName string) bool {
	return slices.ContainsFunc(b.svc.update, func(f sdkField) bool { return f.in == inBody && f.apiName == apiName })
}

// objectField returns the object's field apiName.
func (b *kindBuilder) objectField(apiName string) (sdkField, bool) {
	i := slices.IndexFunc(b.svc.object, func(f sdkField) bool { return f.apiName == apiName })
	if i < 0 {
		return sdkField{}, false
	}
	return b.svc.object[i], true
}

// fieldSpec returns what the spec says about the property apiName of props;
// nothing when the spec does not describe it.
func (b *kindBuilder) fieldSpec(props map[string]*schema, apiName string) (fieldSpec, error) {
	prop, ok := props[apiName]
	if !ok {
		return fieldSpec{}, nil
	}
	fs, err := b.spec.fieldSpec(prop)
	if err != nil {
		return fieldSpec{}, fmt.Errorf("%s in the OpenAPI spec: %w", apiName, err)
	}
	return fs, nil
}

// field maps an SDK field to a field of the spec, or of
// status.cloudflareMetadata, with what the OpenAPI spec says about it and the
// entry's adjustments, and returns the print column it asks for, if any.
//
// An optional spec field that is not a string is a pointer, so that unset
// differs from a zero value. Status fields are all optional, never pointers,
// and carry no validation: a value Cloudflare starts returning must not make
// kflare's status updates fail.
func (b *kindBuilder) field(f sdkField, spec bool) (field, []printColumn, error) {
	adjust, adjusted := b.entry.Fields[f.apiName]
	if adjusted {
		b.used[f.apiName] = true
	}
	if !spec && adjust.Validation != nil {
		return field{}, nil, fmt.Errorf("fields.%s.validation: only spec fields are validated", f.apiName)
	}
	props := b.objectProps
	if spec {
		props = b.createProps
	}
	fs, err := b.fieldSpec(props, f.apiName)
	if err != nil {
		return field{}, nil, err
	}
	if fs.sensitive {
		return field{}, nil, fmt.Errorf("%s is sensitive (x-sensitive), and kflare-gen cannot keep it in a Secret "+
			"yet; ignore it with a reason", f.apiName)
	}
	integer := adjust.Validation != nil && adjust.Validation.Integer
	if integer && (basicClass(f.typ) != types.IsNumeric || fs.typ == typeInteger) {
		return field{}, nil, fmt.Errorf("fields.%s.validation.integer: only a number the OpenAPI spec does not say "+
			"is an integer is made one", f.apiName)
	}
	goType, column, err := goTypeOf(f, fs.typ, integer)
	if err != nil {
		return field{}, nil, err
	}
	out := field{apiName: f.apiName, goName: f.goName, jsonName: lowerCamel(f.goName), goType: goType,
		optional: !spec || !f.required}
	parent := ".status.cloudflareMetadata."
	if spec {
		parent = ".spec."
		if out.optional && goType != typeString {
			out.goType = "*" + goType
		}
		if err := b.specValidation(&out, f, fs, adjust.Validation); err != nil {
			return field{}, nil, err
		}
	}
	doc := adjust.Doc
	if doc == "" {
		doc = f.doc
	}
	out.doc = lines(doc)
	var columns []printColumn
	if adjust.PrintColumn != "" {
		columns = append(columns, printColumn{name: adjust.PrintColumn, typ: column, jsonPath: parent + out.jsonName})
	}
	return out, columns, nil
}

// specValidation sets a spec field's enum, default and validation: the
// spec's, or the validation the entry replaces it with.
func (b *kindBuilder) specValidation(out *field, f sdkField, fs fieldSpec, override *validationConfig) error {
	out.enum = fs.enum
	if out.enum == nil {
		out.enum = f.enum
	}
	if fs.def != "" {
		if err := checkDefault(out.goType, fs.def); err != nil {
			return fmt.Errorf("%s's default in the OpenAPI spec: %w", f.apiName, err)
		}
		out.def = fs.def
	}
	base := strings.TrimPrefix(out.goType, "*")
	if override != nil {
		lengths := override.MinLength != nil || override.MaxLength != nil || override.Pattern != ""
		if lengths && base != typeString {
			return fmt.Errorf("fields.%s.validation: lengths and patterns need a string field", f.apiName)
		}
		if (override.Minimum != nil || override.Maximum != nil) && base != typeInt64 {
			return fmt.Errorf("fields.%s.validation: a range needs an integer field", f.apiName)
		}
		enum, excluded, err := override.enum(f.apiName, base == typeString, out.enum)
		if err != nil {
			return err
		}
		out.enum, out.excluded, out.validation = enum, excluded, override.validation()
		return nil
	}
	// The spec's keywords that fit the field's type.
	v := fs.validation
	switch base {
	case typeString:
		out.validation = validation{minLength: v.minLength, maxLength: v.maxLength, pattern: v.pattern}
	case typeInt64:
		out.validation = validation{minimum: v.minimum, maximum: v.maximum,
			exclusiveMinimum: v.exclusiveMinimum, exclusiveMaximum: v.exclusiveMaximum}
	}
	return nil
}

// checkDefault checks that a default fits a field's Go type.
func checkDefault(goType, def string) error {
	switch strings.TrimPrefix(goType, "*") {
	case typeString:
		if _, err := strconv.Unquote(def); err == nil {
			return nil
		}
	case typeBool:
		if def == "true" || def == "false" {
			return nil
		}
	case typeInt64:
		if _, err := strconv.ParseInt(def, 10, 64); err == nil {
			return nil
		}
	}
	return fmt.Errorf("%s does not fit a %s", def, goType)
}

// goTypeOf maps an SDK field's type to a Go type for the CRD and the type of
// its print column. specType is the field's type in the OpenAPI spec:
// cloudflare-go v7 types every number as a float64, so only the spec, or the
// entry (integer), can tell an integer, which a CRD needs.
func goTypeOf(f sdkField, specType string, integer bool) (goType, column string, err error) {
	if basic, ok := f.typ.Underlying().(*types.Basic); ok {
		switch basic.Kind() {
		case types.String:
			return typeString, typeString, nil
		case types.Bool:
			return typeBool, "boolean", nil
		case types.Float64, types.Int64, types.Int:
			switch {
			case specType == typeInteger || integer:
				return typeInt64, typeInteger, nil
			case specType == "number":
				return "", "", fmt.Errorf("%s is a number that need not be whole, which a CRD cannot hold well; "+
					"ignore it with a reason, or make it an integer (validation.integer) with one", f.apiName)
			}
			return "", "", fmt.Errorf("%s is a %s, and the OpenAPI spec does not say it is an integer; ignore it "+
				"with a reason", f.apiName, f.typ)
		}
	}
	return "", "", fmt.Errorf("%s is a %s, which kflare-gen cannot map yet; ignore it with a reason", f.apiName, f.typ)
}

// isAccountID reports whether f is the account_id a call's path takes.
func isAccountID(f sdkField) bool {
	return f.in == inPath && f.apiName == "account_id"
}

// isZoneID reports whether f is the zone_id a call's path takes.
func isZoneID(f sdkField) bool {
	return f.in == inPath && f.apiName == "zone_id"
}

// isString reports whether t is a string type.
func isString(t types.Type) bool {
	basic, ok := t.Underlying().(*types.Basic)
	return ok && basic.Kind() == types.String
}

// lines splits documentation into lines, without trailing blank ones.
func lines(doc string) []string {
	doc = strings.TrimRight(doc, " \n")
	if doc == "" {
		return nil
	}
	out := strings.Split(doc, "\n")
	for i, line := range out {
		out[i] = strings.TrimRight(line, " ")
	}
	return out
}
