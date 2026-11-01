/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"fmt"
	"go/types"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// controller is the model of a generated kind's controller, of its envtest
// table and of its sample: what zz_generated.<kind>.go and
// zz_generated.<kind>_test.go in internal/controller declare, and what
// config/samples holds for it.
type controller struct {
	*kind
	// varName starts the kind's unexported names, as in kvNamespaceKind.
	varName string
	svc     *service
	// The calls the shared CRUD reconciler makes. update is nil for a
	// service that cannot change an object, and list for a kind whose
	// objects their key identifies (byKey).
	create, get, list, update, remove *sdkCall
	// byKey is set for a kind whose objects have no ID but their key, as an
	// R2 bucket's name is: a resource reads the object by its key rather than
	// finding it in the list.
	byKey bool
	// zoned is set for a kind whose objects are in a zone, which a resource
	// names with its zoneRef; others are in an account (accountRef).
	// isParent reports whether a call's param is the zone's or the account's
	// ID.
	zoned    bool
	isParent func(sdkField) bool
	// object is the type of the Cloudflare object that Get returns a pointer
	// to, listed the type of the objects a page of List holds, and
	// deleteResult the type Delete returns a pointer to. created and updated
	// are the types New and Update return a pointer to, when they are not
	// object: a Worker route's calls each return a type of their own, with
	// the object's fields.
	object, listed, deleteResult types.Type
	created, updated             types.Type
	// singlePage is set for a List that returns all of a parent's objects in
	// one page, which takes no page params: a zone's Worker routes. Otherwise
	// pageSize is the most objects a page of the list holds, and page and
	// perPage are List's params that ask for a page.
	singlePage    bool
	pageSize      int64
	page, perPage sdkField
	// fields are the spec's fields that the API takes, accountRef or zoneRef
	// left out; key is the one a resource adopts an object by, address those
	// that name the object in every call, besides the parent and the ID, and
	// references those that name a resource of another kind, whose value
	// kflare sends.
	fields     []apiField
	key        apiField
	address    []apiField
	references []apiField
	// fixedFields holds the values kflare always sends, as fields without a
	// spec field.
	fixedFields []apiField
	// id is the object's ID and status the fields that status records from
	// the object, ID first; statusZoneID, for a zone's kind, is the status
	// field that records the zone's ID. listedID and listedKey are the ID and
	// the adoption key of the objects List returns.
	id                  sdkField
	status              []statusField
	statusZoneID        string
	listedID, listedKey sdkField
	// conflictReason is the reason a resource reports when another one
	// manages the object it would adopt.
	conflictReason string
	// hooks is what the kind's hand-written hooks add; nil without hooks.
	hooks            *hooksConfig
	dependents       []dependentConfig
	deletionBlockers []deletionBlockerConfig
	// sample holds the sample's value of each spec field it sets, by API
	// name.
	sample map[string]any
	// scope is the sample's address, as the fake keeps objects apart by it:
	// its address field's value.
	scope string
	// changes, immutable and invalid are the generated specs' edits of the
	// sample: a change of each updatable field, of each immutable one, and
	// values the CRD refuses. elsewhere moves the sample to another address,
	// a change of the address field, and scopeElsewhere is the scope it
	// leads to.
	changes, immutable, invalid []specEdit
	elsewhere                   *specEdit
	scopeElsewhere              string
}

// apiField is a spec field, or a fixed value, with its counterparts in the
// SDK: the params of New and Update that set it, and the object's field that
// reports it. update is nil for a field Update does not take, and object nil
// for one the object does not report.
type apiField struct {
	field
	// fixed is the value kflare always sends, for a field without a spec
	// field.
	fixed                  string
	create, update, object *sdkField
}

// statusField is a field of status.cloudflareMetadata and the object's field
// it records.
type statusField struct {
	field
	object sdkField
}

// specEdit is an edit of the sample's spec in a generated spec: value set on
// the spec field f, described by what. setup, when set, is f's value in the
// resource the edit starts from, which the sample leaves unset.
type specEdit struct {
	f            field
	value, setup any
	what         string
}

// buildController builds the model of k's controller from the kind's entry,
// its service and the OpenAPI spec. A service that the shared CRUD
// reconciler cannot drive yet is refused, with what it lacks.
func buildController(k *kind, entry kindConfig, svc *service, spec *apiSpec) (*controller, error) {
	b := &controllerBuilder{k: k, entry: entry, svc: svc, spec: spec}
	c, err := b.build()
	if err != nil {
		return nil, fmt.Errorf("%s (%s): %w", entry.Kind, entry.Service, err)
	}
	return c, nil
}

type controllerBuilder struct {
	k     *kind
	entry kindConfig
	svc   *service
	spec  *apiSpec
}

func (b *controllerBuilder) build() (*controller, error) {
	k, svc := b.k, b.svc
	c := &controller{kind: k, varName: lowerCamel(k.name), svc: svc, dependents: b.entry.Dependents,
		deletionBlockers: b.entry.DeletionBlockers, byKey: b.entry.ID.Field == b.entry.Adopt.By,
		zoned: b.entry.Scope == scopeZone, hooks: b.entry.Hooks}
	c.isParent, _ = parentParam(b.entry.Scope)
	if err := b.calls(c); err != nil {
		return nil, err
	}
	if err := b.fields(c); err != nil {
		return nil, err
	}
	if err := b.status(c); err != nil {
		return nil, err
	}
	if err := b.adoption(c); err != nil {
		return nil, err
	}
	if err := b.sample(c); err != nil {
		return nil, err
	}
	return c, nil
}

// calls checks the shape of the calls the shared CRUD reconciler makes. A
// kind whose objects their key identifies does not list them.
func (b *controllerBuilder) calls(c *controller) error {
	svc := b.svc
	needed := []string{listMethod, "Delete"}
	if c.byKey {
		needed = needed[1:]
	}
	for _, name := range needed {
		if svc.calls[name] == nil {
			return fmt.Errorf("the service has no %s, which the shared CRUD reconciler calls", name)
		}
	}
	if _, ok := svc.pkg.Scope().Lookup("New" + svc.typeName).(*types.Func); !ok {
		return fmt.Errorf("%s has no New%s to build the service with", svc.pkg.Name(), svc.typeName)
	}
	c.create, c.get, c.remove = svc.calls["New"], svc.calls["Get"], svc.calls["Delete"]
	if !c.byKey {
		c.list = svc.calls["List"]
	}
	if svc.updateMethod != "" {
		c.update = svc.calls[svc.updateMethod]
	}
	for _, call := range c.calls() {
		// Get, Update and Delete name the object by its ID.
		withID := call != c.create && call != c.list
		if err := checkShape(call, withID); err != nil {
			return err
		}
	}

	ptr, ok := c.get.sig.Results().At(0).Type().(*types.Pointer)
	if !ok {
		return fmt.Errorf("the service's Get does not return a pointer")
	}
	c.object = ptr.Elem()
	for _, call := range []*sdkCall{c.create, c.update} {
		if call == nil {
			continue
		}
		got := call.sig.Results().At(0).Type()
		if types.Identical(got, ptr) {
			continue
		}
		result, ok := got.(*types.Pointer)
		if !ok {
			return fmt.Errorf("the service's %s does not return a pointer", call.name)
		}
		if err := sameFields(c.object, result.Elem(), svc.object); err != nil {
			return fmt.Errorf("%s returns %s and Get %s: %w", call.name, typeName(got), typeName(ptr), err)
		}
		if call == c.create {
			c.created = result.Elem()
		} else {
			c.updated = result.Elem()
		}
	}
	deleted, ok := c.remove.sig.Results().At(0).Type().(*types.Pointer)
	if !ok {
		return fmt.Errorf("the service's Delete does not return a pointer")
	}
	c.deleteResult = deleted.Elem()
	// Every call names the parent.
	_, parentName := parentParam(b.entry.Scope)
	for _, call := range c.calls() {
		if !slices.ContainsFunc(call.paramFields, c.isParent) {
			return fmt.Errorf("%s takes no %s", call.name, parentName)
		}
	}
	if c.byKey {
		c.listed = c.object
		return nil
	}

	got := c.list.sig.Results().At(0).Type()
	listed, page, ok := pageElement(got)
	if !ok {
		return fmt.Errorf("the service's List returns %s: the shared CRUD reconciler can only read a "+
			"pagination.V4PagePaginationArray or SinglePage yet, or read an object by its key (adopt.by the ID)",
			typeName(got))
	}
	if svc.listed == nil {
		return fmt.Errorf("the service's List returns pages of %s, which kflare-gen cannot read", typeName(listed))
	}
	c.listed = listed
	if c.listedID, ok = listedField(svc, b.entry.ID.Field); !ok {
		return fmt.Errorf("the objects List returns have no %s, the ID", b.entry.ID.Field)
	}
	if page == singlePage {
		c.singlePage = true
		return nil
	}
	for _, name := range []string{pageParam, perPageParam} {
		i := slices.IndexFunc(c.list.paramFields, func(f sdkField) bool { return f.in == inQuery && f.apiName == name })
		if i < 0 || !isNumber(c.list.paramFields[i].typ) {
			return fmt.Errorf("the service's List takes no %s number, which the shared CRUD reconciler pages with",
				name)
		}
		if name == pageParam {
			c.page = c.list.paramFields[i]
		} else {
			c.perPage = c.list.paramFields[i]
		}
	}
	var err error
	c.pageSize, err = b.spec.pageSize(b.svc.listRoute)
	return err
}

// pageParam and perPageParam are the query parameters of a List that ask for
// a page.
const (
	pageParam    = "page"
	perPageParam = "per_page"
)

// sameFields reports why result, the type a call returns, does not hold every
// field of object, the type Get returns, whose fields are fields: each by its
// Go name, of a type that converts to the object's. nil when it does, so that
// the call's result converts to the object.
func sameFields(object, result types.Type, fields []sdkField) error {
	st, ok := result.Underlying().(*types.Struct)
	if !ok {
		return fmt.Errorf("%s is not a struct", typeName(result))
	}
	for _, f := range fields {
		i := slices.IndexFunc(structFields(st), func(v *types.Var) bool { return v.Name() == f.goName })
		if i < 0 {
			return fmt.Errorf("it has no %s, which %s reports", f.goName, typeName(object))
		}
		from := st.Field(i).Type()
		if !types.Identical(from, f.typ) && (basicClass(from) == 0 || basicClass(from) != basicClass(f.typ)) {
			return fmt.Errorf("its %s is a %s, which kflare-gen cannot convert to the %s %s reports",
				f.goName, typeName(from), typeName(f.typ), typeName(object))
		}
	}
	return nil
}

// structFields returns the fields of st.
func structFields(st *types.Struct) []*types.Var {
	out := make([]*types.Var, 0, st.NumFields())
	for i := range st.NumFields() {
		out = append(out, st.Field(i))
	}
	return out
}

// calls returns the calls the shared CRUD reconciler makes, in the order the
// narrow interface declares them.
func (c *controller) calls() []*sdkCall {
	calls := []*sdkCall{c.create, c.get}
	if c.list != nil {
		calls = append(calls, c.list)
	}
	if c.update != nil {
		calls = append(calls, c.update)
	}
	return append(calls, c.remove)
}

// addressParam returns call's param that carries the address field af.
func addressParam(call *sdkCall, af apiField) *sdkField {
	return counterpart(call.paramFields, *af.create)
}

// sets reports whether the reconciler sets call's param p: the parent's ID,
// a field's param, the address, or List's page.
func (c *controller) sets(call *sdkCall, p sdkField) bool {
	if c.isParent(p) {
		return true
	}
	if call == c.list && !c.singlePage && p.in == inQuery && (p.apiName == pageParam || p.apiName == perPageParam) {
		return true
	}
	for _, af := range slices.Concat(c.fields, c.fixedFields) {
		var param *sdkField
		switch call {
		case c.create:
			param = af.create
		case c.update:
			param = af.update
		}
		if param != nil && param.goName == p.goName {
			return true
		}
	}
	return slices.ContainsFunc(c.address, func(af apiField) bool {
		q := addressParam(call, af)
		return q != nil && q.goName == p.goName
	})
}

// checkShape checks that a call takes (ctx, params, opts...), or (ctx, id
// string, params, opts...) when withID is set.
func checkShape(call *sdkCall, withID bool) error {
	params := call.sig.Params()
	want, shape := 3, "(ctx, params, opts...)"
	if withID {
		want, shape = 4, "(ctx, id string, params, opts...)"
	}
	if params.Len() != want || (withID && !types.Identical(params.At(1).Type(), types.Typ[types.String])) {
		return fmt.Errorf("%s is not shaped %s", call.name, shape)
	}
	return nil
}

// listedField returns the field apiName of the objects List returns.
func listedField(svc *service, apiName string) (sdkField, bool) {
	i := slices.IndexFunc(svc.listed, func(f sdkField) bool { return f.apiName == apiName })
	if i < 0 {
		return sdkField{}, false
	}
	return svc.listed[i], true
}

// fields pairs each spec field and fixed value with the params that set it
// and the object's field that reports it, and checks that the reconciler
// sets every param a call requires.
func (b *controllerBuilder) fields(c *controller) error {
	var update []sdkField
	if c.update != nil {
		update = c.update.paramFields
	}
	pair := func(f apiField) (apiField, error) {
		i := slices.IndexFunc(c.create.paramFields, func(p sdkField) bool { return p.apiName == f.apiName })
		if i < 0 {
			return f, nil
		}
		f.create = &c.create.paramFields[i]
		if !isAddress(b.svc, *f.create) && !b.entry.Fields[f.apiName].Immutable {
			f.update = counterpart(update, *f.create)
		}
		var err error
		f.object, err = objectCounterpart(b.svc, b.entry, *f.create)
		return f, err
	}
	for _, f := range c.spec[1:] {
		af, err := pair(apiField{field: f})
		if err != nil {
			return err
		}
		if af.update != nil && af.object == nil {
			return fmt.Errorf("%s can be updated, but Get's object does not report it, so drift in it would go "+
				"unseen; ignore it with a reason", f.apiName)
		}
		c.fields = append(c.fields, af)
		if isAddress(b.svc, *af.create) {
			c.address = append(c.address, af)
		}
		if af.ref != nil {
			c.references = append(c.references, af)
		}
	}
	if len(c.address) > 1 {
		return fmt.Errorf("%s and %s both name the object in every call; kflare-gen takes one such param yet",
			c.address[0].apiName, c.address[1].apiName)
	}
	for _, name := range slices.Sorted(maps.Keys(c.fixed)) {
		af, err := pair(apiField{field: field{apiName: name}, fixed: c.fixed[name]})
		if err != nil {
			return err
		}
		c.fixedFields = append(c.fixedFields, af)
	}
	// An Update that takes nothing kflare sends is never called: a D1
	// database's takes only its read replication.
	if !slices.ContainsFunc(slices.Concat(c.fields, c.fixedFields), func(af apiField) bool { return af.update != nil }) {
		c.update = nil
	}
	// A call may take a param the reconciler does not set only when it is
	// optional.
	for _, call := range c.calls() {
		for _, f := range call.paramFields {
			if !f.required || c.sets(call, f) || f.inUnion && b.entry.Hooks.params() {
				continue
			}
			// Only Update can require a body field the spec does not hold: New
			// requires none the entry may ignore.
			if f.in == inBody {
				return fmt.Errorf("%s requires %s, which the entry ignores", call.name, f.apiName)
			}
			return fmt.Errorf("%s requires the %s parameter %s, which the shared CRUD reconciler does not set",
				call.name, f.in, f.apiName)
		}
	}
	return nil
}

// status pairs the fields of status.cloudflareMetadata with the object's,
// but for the zone's ID, which the Zone gives.
func (b *controllerBuilder) status(c *controller) error {
	for _, f := range c.metadata {
		if f.zoneID {
			c.statusZoneID = f.goName
			continue
		}
		i := slices.IndexFunc(b.svc.object, func(o sdkField) bool { return o.apiName == f.apiName })
		if i < 0 {
			return fmt.Errorf("the object Get returns has no %s", f.apiName)
		}
		c.status = append(c.status, statusField{field: f, object: b.svc.object[i]})
	}
	c.id = c.status[0].object
	return nil
}

// adoption reads which field a resource adopts an object by.
func (b *controllerBuilder) adoption(c *controller) error {
	by := b.entry.Adopt.By
	i := slices.IndexFunc(c.fields, func(f apiField) bool { return f.apiName == by })
	switch {
	case i < 0:
		return fmt.Errorf("adopt.by: the spec has no field %s", by)
	case c.fields[i].optional || c.fields[i].goType != typeString:
		return fmt.Errorf("adopt.by: %s is not a required string", by)
	case c.fields[i].object == nil:
		return fmt.Errorf("adopt.by: Get's object does not report %s", by)
	}
	c.key = c.fields[i]
	c.conflictReason = c.key.goName + "Conflict"
	if c.byKey {
		// The key is the ID: Get, Update and Delete name the object by it.
		return nil
	}
	var ok bool
	if c.listedKey, ok = listedField(b.svc, by); !ok {
		return fmt.Errorf("adopt.by: the objects List returns do not report %s", by)
	}
	return nil
}

// sample checks the sample's values against the spec's fields, and derives
// the generated specs' edits from them.
func (b *controllerBuilder) sample(c *controller) error {
	c.sample = map[string]any{}
	specFields := map[string]field{}
	for _, f := range c.fields {
		specFields[f.apiName] = f.field
	}
	for _, name := range slices.Sorted(maps.Keys(b.entry.Sample)) {
		f, ok := specFields[name]
		if !ok {
			return fmt.Errorf("sample.%s: the spec has no such field", name)
		}
		value, err := sampleValue(f, b.entry.Sample[name])
		if err != nil {
			return fmt.Errorf("sample.%s: %w", name, err)
		}
		c.sample[name] = value
	}
	for _, f := range c.fields {
		value, set := c.sample[f.apiName]
		if !set && !f.optional {
			return fmt.Errorf("sample.%s: the field is required, so the sample sets it", f.apiName)
		}
		if f.ref != nil {
			// The references' specs point a resource at other resources, which
			// must exist; the CRD only checks an immutable one.
			if f.immutable {
				edit := specEdit{f: f.field, value: "other-" + strings.ToLower(f.ref.Kind), what: "changing " + f.jsonName}
				if !set {
					edit.what = "setting " + f.jsonName
				}
				c.immutable = append(c.immutable, edit)
			}
			continue
		}
		// A field the sample leaves unset holds its default, once the API
		// server has applied it.
		if !set && f.def != "" {
			var err error
			if value, err = defaultValue(f.field); err != nil {
				return fmt.Errorf("%s's default: %w", f.apiName, err)
			}
		}
		// A field that cannot be updated is immutable: the generated specs
		// check that the CRD refuses a change, or else that the reconciler
		// makes it in place.
		other, err := otherValue(f.field, value)
		if err != nil {
			return fmt.Errorf("sample.%s: %w; the generated specs change it, so the sample sets it", f.apiName, err)
		}
		edit := specEdit{f: f.field, value: other, what: "changing " + f.jsonName}
		if value == nil {
			edit.what = "setting " + f.jsonName
		}
		if f.clearable && set {
			c.changes = append(c.changes, specEdit{f: f.field, value: "", what: "clearing " + f.jsonName})
		}
		if f.immutable {
			c.immutable = append(c.immutable, edit)
			// Setting the field is refused because it was unset; changing it
			// once set is refused too, when it has another valid value (an
			// enum of one value has none).
			if changed, err := otherValue(f.field, other); value == nil && err == nil {
				c.immutable = append(c.immutable, specEdit{f: f.field, setup: other, value: changed,
					what: "changing " + f.jsonName + " once set"})
			}
		} else {
			c.changes = append(c.changes, edit)
		}
		invalid, err := invalidValues(f.field)
		if err != nil {
			return fmt.Errorf("%s: %w", f.apiName, err)
		}
		c.invalid = append(c.invalid, invalid...)
		if slices.ContainsFunc(c.address, func(af apiField) bool { return af.apiName == f.apiName }) {
			if f.goType != typeString {
				return fmt.Errorf("%s names the object in every call, so it is a string", f.apiName)
			}
			c.scope, _ = value.(string)
			c.elsewhere, c.scopeElsewhere = &edit, other.(string)
		}
	}
	return nil
}

// defaultValue returns the default of f, as a sample gives a value.
func defaultValue(f field) (any, error) {
	switch strings.TrimPrefix(f.goType, "*") {
	case typeString:
		return strconv.Unquote(f.def)
	case typeBool:
		return strconv.ParseBool(f.def)
	case typeInt64:
		return strconv.ParseInt(f.def, 10, 64)
	}
	return nil, fmt.Errorf("a %s has no default kflare-gen reads", f.goType)
}

// sampleValue checks a sample's value for f, as YAML gives it, against f's
// type and validation, and returns it as a string, bool or int64: for a
// reference, the referenced resource's name.
func sampleValue(f field, raw any) (any, error) {
	if f.ref != nil {
		name, ok := raw.(string)
		if !ok || !resourceName.MatchString(name) {
			return nil, fmt.Errorf("%v is not the name of a %s", raw, f.ref.Kind)
		}
		return name, nil
	}
	var value any
	switch base := strings.TrimPrefix(f.goType, "*"); base {
	case typeString:
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("%v is not a string", raw)
		}
		value = s
	case typeBool:
		v, ok := raw.(bool)
		if !ok {
			return nil, fmt.Errorf("%v is not a bool", raw)
		}
		value = v
	case typeInt64:
		n, ok := raw.(float64)
		if !ok || n != float64(int64(n)) {
			return nil, fmt.Errorf("%v is not an integer", raw)
		}
		value = int64(n)
	default:
		return nil, fmt.Errorf("a %s cannot be sampled yet", f.goType)
	}
	if err := checkValue(f, value); err != nil {
		return nil, err
	}
	return value, nil
}

// checkValue reports why value is not valid for f, as the CRD validates it;
// nil when it is.
func checkValue(f field, value any) error {
	v := f.validation
	switch value := value.(type) {
	case string:
		n := int64(len([]rune(value)))
		switch {
		case len(f.enum) > 0 && !slices.Contains(f.enum, value):
			return fmt.Errorf("%q is not one of %s", value, strings.Join(f.enum, ", "))
		case v.minLength != nil && n < *v.minLength:
			return fmt.Errorf("%q is shorter than %d characters", value, *v.minLength)
		case v.maxLength != nil && n > *v.maxLength:
			return fmt.Errorf("%q is longer than %d characters", value, *v.maxLength)
		case v.pattern != "" && !patternMatches(v.pattern, value):
			return fmt.Errorf("%q does not match %s", value, v.pattern)
		}
	case int64:
		n := float64(value)
		if v.minimum != nil && (n < *v.minimum || v.exclusiveMinimum && n == *v.minimum) {
			return fmt.Errorf("%d is below the minimum", value)
		}
		if v.maximum != nil && (n > *v.maximum || v.exclusiveMaximum && n == *v.maximum) {
			return fmt.Errorf("%d is above the maximum", value)
		}
	}
	return nil
}

// otherValue returns a valid value of f that differs from value, which is
// nil when the sample leaves f unset: another member of an enum, the other
// bool, the next integer in range, or a string with its last character
// changed.
func otherValue(f field, value any) (any, error) {
	base := strings.TrimPrefix(f.goType, "*")
	var candidates []any
	switch {
	case len(f.enum) > 0:
		for _, member := range f.enum {
			candidates = append(candidates, member)
		}
	case base == typeBool:
		candidates = []any{true, false}
	case base == typeInt64:
		n := int64(0)
		if value != nil {
			n = value.(int64)
		} else if f.validation.minimum != nil {
			n = int64(*f.validation.minimum)
		}
		candidates = []any{n + 1, n - 1, n}
	case value != nil:
		candidates = changedStrings(value.(string))
	default:
		return nil, fmt.Errorf("no value to change %s to", f.apiName)
	}
	for _, candidate := range candidates {
		if candidate != value && checkValue(f, candidate) == nil {
			return candidate, nil
		}
	}
	return nil, fmt.Errorf("no valid value of %s differs from %v", f.apiName, value)
}

// changedStrings returns s with its last character replaced, by the next and
// the previous one of its kind, and a different digit or letter.
func changedStrings(s string) []any {
	runes := []rune(s)
	if len(runes) == 0 {
		return []any{"a"}
	}
	last := len(runes) - 1
	var out []any
	for _, r := range []rune{runes[last] + 1, runes[last] - 1, '0', 'a'} {
		changed := slices.Clone(runes)
		changed[last] = r
		out = append(out, string(changed))
	}
	return out
}

// invalidValues returns values of f that the CRD refuses: too short, too
// long, not matching its pattern, outside the enum or out of range.
func invalidValues(f field) ([]specEdit, error) {
	v := f.validation
	var out []specEdit
	a := article(f.jsonName)
	if len(f.enum) > 0 {
		out = append(out, specEdit{f: f, value: "kflare-not-in-enum", what: a + " outside its enum"})
	}
	if v.pattern != "" && len(f.enum) == 0 {
		s, ok := patternMismatch(v)
		if !ok {
			return nil, fmt.Errorf("kflare-gen finds no value its pattern %s refuses", v.pattern)
		}
		out = append(out, specEdit{f: f, value: s, what: a + " not matching its pattern"})
	}
	for _, value := range f.excluded {
		out = append(out, specEdit{f: f, value: value, what: f.jsonName + " " + value + ", which the entry's enum " +
			"leaves out"})
	}
	switch {
	case v.minLength == nil || *v.minLength == 0:
	case *v.minLength == 1 && f.optional:
		// An optional string left empty is unset, which the CRD does not
		// check.
	case *v.minLength == 1:
		out = append(out, specEdit{f: f, value: "", what: "an empty " + f.jsonName})
	default:
		out = append(out, specEdit{f: f, value: strings.Repeat("a", int(*v.minLength-1)),
			what: fmt.Sprintf("%s shorter than %d characters", a, *v.minLength)})
	}
	if v.maxLength != nil {
		out = append(out, specEdit{f: f, value: strings.Repeat("a", int(*v.maxLength+1)),
			what: fmt.Sprintf("%s longer than %d characters", a, *v.maxLength)})
	}
	if v.minimum != nil {
		below := int64(*v.minimum)
		if !v.exclusiveMinimum {
			below--
		}
		out = append(out, specEdit{f: f, value: below, what: a + " below its minimum"})
	}
	if v.maximum != nil {
		above := int64(*v.maximum)
		if !v.exclusiveMaximum {
			above++
		}
		out = append(out, specEdit{f: f, value: above, what: a + " above its maximum"})
	}
	return out, nil
}

// article returns name after the article a or an: an before a vowel, and
// before an initialism read letter by letter whose first letter sounds like
// one (an R2 bucket, an HTTP header).
func article(name string) string {
	switch {
	case name == "":
	case strings.ContainsRune("aeiouAEIOU", rune(name[0])):
		return "an " + name
	case len(name) > 1 && strings.ContainsRune("FHLMNRSX", rune(name[0])) &&
		(unicode.IsUpper(rune(name[1])) || unicode.IsDigit(rune(name[1]))):
		return "an " + name
	}
	return "a " + name
}

// resourceName matches the name of a Kubernetes resource (a DNS label, short
// of the length limit).
var resourceName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// patternMatches reports whether s matches pattern, as the API server
// checks it, with Go's regular expressions. A pattern that does not compile
// matches nothing.
func patternMatches(pattern, s string) bool {
	re, err := regexp.Compile(pattern)
	return err == nil && re.MatchString(s)
}

// isNumber reports whether t is a number type.
func isNumber(t types.Type) bool {
	basic, ok := t.Underlying().(*types.Basic)
	return ok && basic.Info()&types.IsNumeric != 0
}

// typeName writes t for an error message.
func typeName(t types.Type) string {
	return types.TypeString(t, (*types.Package).Name)
}
