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
	// spec holds the spec's fields, accountRef or zoneRef first; metadata
	// those of status.cloudflareMetadata, the ID first.
	spec, metadata []field
	printColumns   []printColumn
	// fixed holds the values kflare sends for fields the spec leaves out, by
	// API name.
	fixed map[string]string
	// specExtras, metadataExtras and statusExtras name the hand-written
	// structs the spec, status.cloudflareMetadata and status embed inline.
	specExtras, metadataExtras, statusExtras string
}

// field is one field of a generated struct.
type field struct {
	// apiName is the field's name in the API, such as title; "" for
	// accountRef, zoneRef and the zone's ID in status.
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
	// ref, for a spec field that references a resource of another kind,
	// names what it refers to.
	ref *refConfig
	// clearable marks an optional string that unset clears in Cloudflare.
	clearable bool
	// zoneID marks the field of a zone's kind's status that records its
	// zone's ID, which the Zone gives rather than the object.
	zoneID bool
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
	// request and of the object Get returns, and createHeaders the schemas
	// of the create operation's header parameters, by name.
	createProps, objectProps, createHeaders map[string]*schema
	// used records the entry's field, fixed and ignore names that matched a
	// field.
	used map[string]bool
}

func (b *kindBuilder) build() (*kind, error) {
	e := b.entry
	isParent, parentName := parentParam(e.Scope)
	if !slices.ContainsFunc(b.svc.create, isParent) {
		return nil, fmt.Errorf("scope %s, but New takes no %s", e.Scope, parentName)
	}
	if err := b.loadSpec(); err != nil {
		return nil, err
	}
	k := &kind{name: e.Kind, shortName: e.ShortName, noun: e.Noun, plural: plural(e.Kind), doc: lines(e.Doc),
		claimLabel: e.ClaimLabel, fixed: map[string]string{}}
	parent := field{
		goName: "AccountRef", jsonName: "accountRef", goType: "corev1.LocalObjectReference", immutable: true,
		doc: []string{
			fmt.Sprintf("AccountRef references the CloudflareAccount that owns the %s.", e.Noun),
			"Immutable after creation.",
		},
	}
	if e.Scope == scopeZone {
		parent.goName, parent.jsonName = "ZoneRef", zoneRefField
		parent.doc = append(wrapIndented(fmt.Sprintf("ZoneRef references the Zone, in the same namespace, the %s "+
			"belongs to.", e.Noun)), "Immutable after creation.")
	}
	k.spec = append(k.spec, parent)

	// reported holds the fields of the object that New's params set, by API
	// name: status holds the object's other fields.
	reported := map[string]bool{}
	var specColumns []printColumn
	for _, f := range b.svc.create {
		if isParent(f) {
			continue
		}
		object, err := b.objectCounterpart(f)
		if err != nil {
			return nil, err
		}
		// Status records the value a reference gives too: the spec holds only
		// the referenced resource's name.
		if object != nil && e.Fields[f.apiName].Ref == nil {
			reported[object.apiName] = true
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
			if f.required && !e.Hooks.params() {
				return nil, fmt.Errorf("%s is required to create the object, so it cannot be ignored", f.apiName)
			}
			continue
		}
		if f.in != inBody && f.in != inHeader {
			return nil, fmt.Errorf("%s is a %s parameter, which kflare-gen cannot map yet; ignore it with a reason",
				f.apiName, f.in)
		}
		out, column, err := b.field(f, true)
		if err != nil {
			return nil, err
		}
		if !b.updatable(f) {
			out.immutable = true
			if out.optional && out.def == "" {
				// self == oldSelf holds only while the field is set: it must
				// not be added or removed either. A field with a default is
				// always set.
				k.specRules = append(k.specRules, fmt.Sprintf(
					`+kubebuilder:validation:XValidation:rule="has(self.%[1]s) == has(oldSelf.%[1]s)",message="%[1]s is immutable"`,
					out.jsonName))
			}
		}
		k.spec = append(k.spec, out)
		specColumns = append(specColumns, column...)
	}
	for _, f := range b.svc.update {
		if f.in != inBody && f.in != inHeader || isParent(f) || counterpart(b.svc.create, f) != nil ||
			b.ignored(f.apiName) {
			continue
		}
		return nil, fmt.Errorf("%s can be updated but not set at creation, which kflare-gen cannot map yet; "+
			"ignore it with a reason", f.apiName)
	}
	if err := b.checkReportedAs(k); err != nil {
		return nil, err
	}

	statusColumns, err := b.metadata(k, reported)
	if err != nil {
		return nil, err
	}
	k.printColumns = append(specColumns, statusColumns...)
	if h := e.Hooks; h != nil {
		k.specExtras, k.metadataExtras, k.statusExtras = h.SpecExtras, h.MetadataExtras, h.StatusExtras
		for _, c := range h.Columns {
			k.printColumns = append(k.printColumns, printColumn{name: c.Name, typ: c.Type, jsonPath: c.Path})
		}
	}
	k.printColumns = append(k.printColumns, readyColumn, ageColumn)

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
	if b.createHeaders, err = b.spec.parameterSchemas(create, inHeader); err != nil {
		return fmt.Errorf("%s: %w", b.svc.newRoute, err)
	}
	return nil
}

// counterpart returns the field of fields that carries what the param f
// does: the one with f's API name, where f goes, or else the one with f's Go
// name, which Stainless gives a param in every call wherever the call sends
// it. R2's storage class is New's body field storageClass, Edit's header
// cf-r2-storage-class and the object's storage_class, each StorageClass.
// Callers never ask for the account_id's: every call takes its own.
func counterpart(fields []sdkField, f sdkField) *sdkField {
	matches := []func(sdkField) bool{
		func(o sdkField) bool { return o.in == f.in && o.apiName == f.apiName },
		func(o sdkField) bool { return o.goName == f.goName },
	}
	for _, match := range matches {
		i := slices.IndexFunc(fields, match)
		if i >= 0 {
			return &fields[i]
		}
	}
	return nil
}

// objectCounterpart returns the field of the object Get returns that reports
// what the param f of New sets: the one the entry names (reportedAs), or its
// counterpart. nil when the object reports no such field.
func (b *kindBuilder) objectCounterpart(f sdkField) (*sdkField, error) {
	return objectCounterpart(b.svc, b.entry, f)
}

// objectCounterpart returns the field of the object svc's Get returns that
// reports what the param f of New sets, as kindBuilder.objectCounterpart does.
func objectCounterpart(svc *service, entry kindConfig, f sdkField) (*sdkField, error) {
	as := entry.Fields[f.apiName].ReportedAs
	if as == "" {
		return counterpart(svc.object, f), nil
	}
	i := slices.IndexFunc(svc.object, func(o sdkField) bool { return o.apiName == as })
	if i < 0 {
		return nil, fmt.Errorf("fields.%s.reportedAs: the object Get returns has no field %s", f.apiName, as)
	}
	return &svc.object[i], nil
}

// checkReportedAs refuses a reportedAs on anything but a header param of New
// that k's spec holds.
func (b *kindBuilder) checkReportedAs(k *kind) error {
	for _, name := range slices.Sorted(maps.Keys(b.entry.Fields)) {
		if as := b.entry.Fields[name].ReportedAs; as != "" && !slices.ContainsFunc(k.spec, func(f field) bool {
			return f.apiName == name && b.isHeader(name)
		}) {
			return fmt.Errorf("fields.%s.reportedAs: only a header parameter of New that the spec holds takes one",
				name)
		}
	}
	return nil
}

// isHeader reports whether New takes the param apiName as a header.
func (b *kindBuilder) isHeader(apiName string) bool {
	return slices.ContainsFunc(b.svc.create, func(f sdkField) bool { return f.in == inHeader && f.apiName == apiName })
}

// metadata adds the status fields to k: the ID, then the fields of the object
// that the create params do not set (reported). It returns their print
// columns.
func (b *kindBuilder) metadata(k *kind, reported map[string]bool) ([]printColumn, error) {
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
	if e.Scope == scopeZone {
		k.metadata = append(k.metadata, field{goName: "ZoneID", jsonName: "zoneID", goType: typeString, optional: true,
			zoneID: true, doc: wrapIndented(fmt.Sprintf("ZoneID is the Cloudflare zone the %s belongs to (denormalized "+
				"from the Zone status).", e.Noun))})
	}
	for _, f := range b.svc.object {
		// The object's other fields that the spec sets are the spec's. A
		// zone's kind records its zone's ID from the Zone.
		if f.apiName == e.ID.Field || reported[f.apiName] || b.ignored(f.apiName) ||
			e.Scope == scopeZone && f.apiName == zoneIDParam {
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

// updatable reports whether the service's update can change what the param
// f of New sets: it takes f's counterpart, and f does not name the object.
func (b *kindBuilder) updatable(f sdkField) bool {
	if b.entry.Fields[f.apiName].Immutable {
		return false
	}
	return !isAddress(b.svc, f) && counterpart(b.svc.update, f) != nil
}

// isAddress reports whether the param f of New is part of what names the
// object in every call, which Get also takes: an R2 bucket's jurisdiction,
// whose header goes with every call on the bucket. Changing it would name
// another object, so it is immutable.
func isAddress(svc *service, f sdkField) bool {
	get := svc.calls["Get"]
	return get != nil && counterpart(get.paramFields, f) != nil
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
	if adjust.Ref != nil {
		if spec {
			return b.refField(f, adjust)
		}
		// The adjustments are the reference's: status records the value it
		// gives, and says where it comes from.
		ref := adjust.Ref
		adjust = fieldConfig{Doc: fmt.Sprintf("%s is the %s's %s at the last sync: the %s of the %s its %sRef names.",
			f.goName, b.entry.Noun, f.apiName, ref.Field, ref.Kind, lowerCamel(ref.Kind))}
		if param := counterpart(b.svc.create, f); param != nil && !param.required {
			adjust.Doc += fmt.Sprintf(" Empty when the %s has none.", b.entry.Noun)
		}
		adjust.Doc = strings.Join(wrapIndented(adjust.Doc), "\n")
	}
	if !spec && adjust.Validation != nil {
		return field{}, nil, fmt.Errorf("fields.%s.validation: only spec fields are validated", f.apiName)
	}
	props := b.objectProps
	switch {
	case spec && f.in == inHeader:
		props = b.createHeaders
	case spec:
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
	if adjust.Clearable {
		if !spec || !out.optional || goType != typeString || !b.updatable(f) {
			return field{}, nil, fmt.Errorf("fields.%s.clearable: only an optional string Update takes is cleared",
				f.apiName)
		}
		out.clearable = true
	}
	doc := adjust.Doc
	if doc == "" {
		doc = f.doc
	}
	if spec && f.in == inHeader {
		// A header's name is no field name: the object's field that reports
		// the value names it.
		object, err := b.objectCounterpart(f)
		if err != nil {
			return field{}, nil, err
		}
		if object != nil {
			out.goName, out.jsonName = object.goName, lowerCamel(object.goName)
			if doc == "" {
				doc = object.doc
			}
		}
	}
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
	out.doc = lines(doc)
	var columns []printColumn
	if adjust.PrintColumn != "" {
		columns = append(columns, printColumn{name: adjust.PrintColumn, typ: column, jsonPath: parent + out.jsonName})
	}
	return out, columns, nil
}

// refField maps the param f of New, which the entry makes a reference, to a
// spec field that names a resource of the kind it refers to, in the
// resource's namespace: <kind>Ref, optional unless New requires f. It takes no
// validation, which the referenced resource's own value is subject to.
func (b *kindBuilder) refField(f sdkField, adjust fieldConfig) (field, []printColumn, error) {
	if f.in != inBody || !isString(f.typ) {
		return field{}, nil, fmt.Errorf("fields.%s.ref: only a string the request body holds can reference a "+
			"resource", f.apiName)
	}
	ref := adjust.Ref
	goName := ref.Kind + "Ref"
	out := field{apiName: f.apiName, goName: goName, jsonName: lowerCamel(goName),
		goType: "corev1.LocalObjectReference", optional: !f.required, ref: ref}
	if out.optional {
		out.goType = "*" + out.goType
	}
	doc := adjust.Doc
	if doc == "" {
		doc = strings.Join(wrapIndented(fmt.Sprintf("%s references the %s, in the same namespace, whose %s the %s "+
			"takes as its %s.", goName, ref.Kind, ref.Field, b.entry.Noun, f.apiName)), "\n")
	}
	out.doc = lines(doc)
	var columns []printColumn
	if adjust.PrintColumn != "" {
		columns = append(columns, printColumn{name: adjust.PrintColumn, typ: typeString,
			jsonPath: ".spec." + out.jsonName + ".name"})
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
	return f.in == inPath && f.apiName == zoneIDParam
}

// zoneIDParam is the name of a zone's ID, in a call's path and in an
// object, and zoneRefField the spec field of a zone's kind that names its
// Zone.
const (
	zoneIDParam  = "zone_id"
	zoneRefField = "zoneRef"
)

// parentParam returns what reports whether a call's param is the ID of a
// scope's parent, and the param's name: the account_id, or a zone's
// zone_id.
func parentParam(scope string) (func(sdkField) bool, string) {
	if scope == scopeZone {
		return isZoneID, zoneIDParam
	}
	return isAccountID, "account_id"
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
