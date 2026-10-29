/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

const (
	// sdkModule is cloudflare-go v7's module. kflare-gen reads the version
	// the module in its working directory requires.
	sdkModule = "github.com/cloudflare/cloudflare-go/v7"
	// paramPackage holds param.Field, the type of every field of a params
	// struct.
	paramPackage = sdkModule + "/internal/param"
)

// Where a param goes in a request.
const (
	inBody   = "body"
	inPath   = "path"
	inQuery  = "query"
	inHeader = "header"
)

// service is what kflare-gen reads off a cloudflare-go v7 service type.
type service struct {
	// name is the service as an entry names it, such as kv.NamespaceService.
	name string
	// pkg is the service's package, and typeName the service type's name.
	pkg      *types.Package
	typeName string
	// create holds the fields of New's params, update those of Update's (or
	// Edit's), and object those of what Get returns. update is nil for a
	// service that cannot change an object. listed holds the fields of the
	// objects a page of List holds, when the service lists a struct of its
	// package by page.
	create, update, object, listed []sdkField
	// newRoute, getRoute, updateRoute and listRoute are the routes of New,
	// Get, Update (or Edit) and List, which find their operations in the
	// OpenAPI spec. updateRoute is empty when update is nil, and listRoute
	// when the service cannot list.
	newRoute, getRoute, updateRoute, listRoute route
	// calls holds the service's methods that a controller calls, by name:
	// New, Get, List, Delete, and Update or Edit, which updateMethod names. A
	// method the service does not have is missing.
	calls        map[string]*sdkCall
	updateMethod string
}

// sdkCall is one method of a service, shaped (ctx, [id,] params, opts...)
// (*result, error).
type sdkCall struct {
	name string
	sig  *types.Signature
	// params is the type of its params, and paramFields their fields.
	params      types.Type
	paramFields []sdkField
}

// route is the HTTP verb and path of an SDK call, as its package's api.md
// documents them: post /accounts/{account_id}/storage/kv/namespaces.
type route struct {
	verb, path string
}

func (r route) String() string { return r.verb + " " + r.path }

// sdkField is one field of a params struct or of an object.
type sdkField struct {
	// goName is the SDK's Go name, such as Title.
	goName string
	// apiName is the field's name in the API: its json, path, query or
	// header name, such as title or account_id.
	apiName string
	// in is where a param goes: body, path, query or header. An object's
	// fields are all body.
	in string
	// typ is the field's type; for a param, the type inside param.Field.
	typ types.Type
	// required is set for a field tagged api:"required".
	required bool
	// doc is the SDK's documentation of the field.
	doc string
	// enum holds, for a named string type, its constants' values in the order
	// the SDK declares them.
	enum []string
}

// sdk loads cloudflare-go v7's packages.
type sdk struct {
	// dir is the directory of the module whose go.mod pins v7.
	dir string
}

// load loads the type an entry names as <package>.<type>, as what (a
// service, a rule), with its package.
func (s sdk) load(name, what string) (*packages.Package, *types.TypeName, error) {
	dot := strings.LastIndex(name, ".")
	if dot <= 0 || dot == len(name)-1 {
		return nil, nil, fmt.Errorf("%s %q is not <package>.<type>", what, name)
	}
	path, typeName := sdkModule+"/"+name[:dot], name[dot+1:]
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo,
		Dir:  s.dir,
	}, path)
	if err != nil {
		return nil, nil, fmt.Errorf("loading %s: %w", path, err)
	}
	if len(pkgs) != 1 {
		return nil, nil, fmt.Errorf("loading %s: got %d packages", path, len(pkgs))
	}
	pkg := pkgs[0]
	if len(pkg.Errors) > 0 {
		return nil, nil, fmt.Errorf("loading %s: %v", path, pkg.Errors[0])
	}
	typ, ok := pkg.Types.Scope().Lookup(typeName).(*types.TypeName)
	if !ok {
		return nil, nil, fmt.Errorf("%s has no type %s", path, typeName)
	}
	return pkg, typ, nil
}

// service loads the service an entry names.
func (s sdk) service(name string) (*service, error) {
	pkg, typ, err := s.load(name, "service")
	if err != nil {
		return nil, err
	}
	path, typeName := pkg.PkgPath, typ.Name()

	routes, err := readRoutes(pkg)
	if err != nil {
		return nil, err
	}
	routeOf := func(method string) (route, error) {
		r, ok := routes[typeName+"."+method]
		if !ok {
			return route{}, fmt.Errorf("%s's api.md has no route for %s.%s", path, typeName, method)
		}
		return r, nil
	}

	r := newSDKReader(pkg)
	methods := types.NewMethodSet(types.NewPointer(typ.Type()))
	svc := &service{name: name, pkg: pkg.Types, typeName: typeName, calls: map[string]*sdkCall{}}
	newCall, _, err := r.call(methods, "New")
	if err != nil {
		return nil, err
	}
	svc.calls["New"], svc.create = newCall, newCall.paramFields
	if svc.newRoute, err = routeOf("New"); err != nil {
		return nil, err
	}
	getCall, object, err := r.call(methods, "Get")
	if err != nil {
		return nil, err
	}
	svc.calls["Get"], svc.object = getCall, object
	if svc.getRoute, err = routeOf("Get"); err != nil {
		return nil, err
	}
	for _, method := range writeMethods {
		c, _, err := r.call(methods, method)
		if errors.Is(err, errNoMethod) {
			continue
		}
		if err != nil {
			return nil, err
		}
		svc.calls[method], svc.update, svc.updateMethod = c, c.paramFields, method
		if svc.updateRoute, err = routeOf(method); err != nil {
			return nil, err
		}
		break
	}
	// What List returns is a page of objects, and what Delete returns goes
	// unread: only their params are read.
	for _, method := range []string{listMethod, "Delete"} {
		c, err := r.method(methods, method)
		if errors.Is(err, errNoMethod) {
			continue
		}
		if err != nil {
			return nil, err
		}
		svc.calls[method] = c
	}
	if list := svc.calls["List"]; list != nil {
		if svc.listRoute, err = routeOf("List"); err != nil {
			return nil, err
		}
		if elem, _, ok := pageElement(list.sig.Results().At(0).Type()); ok {
			// Not a struct of the package: listed stays nil, and kflare-gen
			// says why when it builds the kind's controller.
			svc.listed, _ = r.fields(elem, false)
		}
	}
	return svc, nil
}

// paginationPackage holds cloudflare-go v7's pages of a list.
const paginationPackage = sdkModule + "/packages/pagination"

// The pages of a list that kflare-gen reads: one of a list by page number,
// and the one page of a list that is never paged, as a zone's Worker routes
// are.
const (
	pagedList  = "V4PagePaginationArray"
	singlePage = "SinglePage"
)

// pageElement returns T for *pagination.V4PagePaginationArray[T] or
// *pagination.SinglePage[T], and the page's type name.
func pageElement(t types.Type) (types.Type, string, bool) {
	ptr, ok := t.(*types.Pointer)
	if !ok {
		return nil, "", false
	}
	named, ok := ptr.Elem().(*types.Named)
	if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != paginationPackage ||
		named.Obj().Name() != pagedList && named.Obj().Name() != singlePage || named.TypeArgs().Len() != 1 {
		return nil, "", false
	}
	return named.TypeArgs().At(0), named.Obj().Name(), true
}

// apiMDCall matches a call in a cloudflare-go v7 api.md: its route, then the
// link to its method, as in
//
//	<code title="post /accounts/{account_id}/storage/kv/namespaces">client.KV.Namespaces.<a
//	href="https://pkg.go.dev/github.com/cloudflare/cloudflare-go/v7/kv#NamespaceService.New">New</a>
var apiMDCall = regexp.MustCompile(`<code title="([a-z]+) ([^"]+)">[^<]*<a href="https://pkg\.go\.dev/` +
	regexp.QuoteMeta(sdkModule) + `/([^"#]+)#(\w+)\.(\w+)">`)

// readRoutes returns the routes of the calls in a package's api.md, by
// <type>.<method>.
func readRoutes(pkg *packages.Package) (map[string]route, error) {
	if len(pkg.GoFiles) == 0 {
		return nil, fmt.Errorf("%s has no Go files", pkg.PkgPath)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(pkg.GoFiles[0]), "api.md"))
	if err != nil {
		return nil, err
	}
	return parseRoutes(string(data), pkg.PkgPath), nil
}

// parseRoutes reads the routes of an api.md's calls that are methods of the
// package pkgPath.
func parseRoutes(apiMD, pkgPath string) map[string]route {
	routes := map[string]route{}
	for _, m := range apiMDCall.FindAllStringSubmatch(apiMD, -1) {
		if sdkModule+"/"+m[3] == pkgPath {
			routes[m[4]+"."+m[5]] = route{verb: m[1], path: m[2]}
		}
	}
	return routes
}

// listMethod names a service's method that lists its objects, or, in a
// service of settings, may read them.
const listMethod = "List"

// writeMethods are the names a service's method that changes an object, or
// writes settings, may have, in the order kflare-gen looks for them.
var writeMethods = []string{"Update", "Edit"}

// errNoMethod is the error for a service without the method asked for.
var errNoMethod = errors.New("no such method")

// sdkReader reads types out of one loaded package.
type sdkReader struct {
	pkg *packages.Package
	// specs indexes the package's type declarations by name, and typeDocs
	// their documentation.
	specs    map[string]*ast.TypeSpec
	typeDocs map[string]string
}

func newSDKReader(pkg *packages.Package) *sdkReader {
	r := &sdkReader{pkg: pkg, specs: map[string]*ast.TypeSpec{}, typeDocs: map[string]string{}}
	for _, file := range pkg.Syntax {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok {
					r.specs[ts.Name.Name] = ts
					doc := ts.Doc
					if doc == nil && len(gen.Specs) == 1 {
						doc = gen.Doc
					}
					r.typeDocs[ts.Name.Name] = strings.TrimSpace(doc.Text())
				}
			}
		}
	}
	return r
}

// method reads a service method shaped (ctx, [id,] params, opts...)
// (*result, error): its signature and its params.
func (r *sdkReader) method(methods *types.MethodSet, name string) (*sdkCall, error) {
	sel := methods.Lookup(r.pkg.Types, name)
	if sel == nil {
		return nil, fmt.Errorf("%s: %w", name, errNoMethod)
	}
	sig := sel.Type().(*types.Signature)
	if !sig.Variadic() || sig.Params().Len() < 3 || sig.Results().Len() != 2 {
		return nil, fmt.Errorf("%s is not shaped (ctx, [id,] params, opts...) (*result, error)", name)
	}
	c := &sdkCall{name: name, sig: sig, params: sig.Params().At(sig.Params().Len() - 2).Type()}
	var err error
	if c.paramFields, err = r.fields(c.params, true); err != nil {
		return nil, fmt.Errorf("%s's params: %w", name, err)
	}
	return c, nil
}

// call reads a method as method does, and the fields of its result.
func (r *sdkReader) call(methods *types.MethodSet, name string) (*sdkCall, []sdkField, error) {
	c, err := r.method(methods, name)
	if err != nil {
		return nil, nil, err
	}
	result, err := r.fields(c.sig.Results().At(0).Type(), false)
	if err != nil {
		return nil, nil, fmt.Errorf("%s's result: %w", name, err)
	}
	return c, result, nil
}

// fields reads the fields of a struct type declared in the package, or of a
// pointer to one: for params, the fields inside their param.Field.
func (r *sdkReader) fields(t types.Type, params bool) ([]sdkField, error) {
	named, st, err := r.structType(t)
	if err != nil {
		return nil, err
	}
	docs := r.fieldDocs(named.Obj().Name())
	var out []sdkField
	for i := range st.NumFields() {
		f, ok, err := r.field(named, st.Field(i), reflect.StructTag(st.Tag(i)), docs, params)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, f)
		}
	}
	return out, nil
}

// structType returns the struct type t, declared in the package, or the
// struct a pointer t points to.
func (r *sdkReader) structType(t types.Type) (*types.Named, *types.Struct, error) {
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() != r.pkg.Types {
		return nil, nil, fmt.Errorf("%s is not a type of %s", t, r.pkg.PkgPath)
	}
	st, ok := named.Underlying().(*types.Struct)
	if !ok {
		return nil, nil, fmt.Errorf("%s is not a struct", named.Obj().Name())
	}
	return named, st, nil
}

// field reads the field v of the struct named, tagged tag, as fields does:
// false for a field the API does not name, such as the JSON metadata of an
// object.
func (r *sdkReader) field(named *types.Named, v *types.Var, tag reflect.StructTag, docs map[string]string,
	params bool) (sdkField, bool, error) {
	f := sdkField{goName: v.Name(), typ: v.Type(), required: tag.Get("api") == "required", doc: docs[v.Name()]}
	for _, in := range []string{inPath, inQuery, inHeader} {
		if name := tag.Get(in); name != "" {
			f.in, f.apiName = in, name
		}
	}
	if f.in == "" {
		name := jsonName(tag)
		if name == "" || name == "-" {
			return sdkField{}, false, nil
		}
		f.in, f.apiName = inBody, name
	}
	if params {
		inner, ok := paramFieldType(f.typ)
		if !ok {
			return sdkField{}, false, fmt.Errorf("%s.%s is a %s, not a param.Field: kflare-gen cannot map unions yet",
				named.Obj().Name(), f.goName, types.TypeString(f.typ, types.RelativeTo(r.pkg.Types)))
		}
		f.typ = inner
	}
	f.enum = r.enumValues(f.typ)
	return f, true, nil
}

// jsonName returns the name a struct tag gives a field in JSON.
func jsonName(tag reflect.StructTag) string {
	name, _, _ := strings.Cut(tag.Get("json"), ",")
	return name
}

// paramFieldType returns T for param.Field[T].
func paramFieldType(t types.Type) (types.Type, bool) {
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != paramPackage ||
		named.Obj().Name() != "Field" || named.TypeArgs().Len() != 1 {
		return nil, false
	}
	return named.TypeArgs().At(0), true
}

// fieldDocs returns the documentation of a struct type's fields, by Go name.
func (r *sdkReader) fieldDocs(typeName string) map[string]string {
	docs := map[string]string{}
	spec, ok := r.specs[typeName]
	if !ok {
		return docs
	}
	st, ok := spec.Type.(*ast.StructType)
	if !ok {
		return docs
	}
	for _, field := range st.Fields.List {
		for _, name := range field.Names {
			docs[name.Name] = strings.TrimSpace(field.Doc.Text())
		}
	}
	return docs
}

// enumValues returns the values of the constants of t, in the order the
// package declares them, when t is a string type the package declares and
// gives constants.
func (r *sdkReader) enumValues(t types.Type) []string {
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() != r.pkg.Types {
		return nil
	}
	if basic, ok := named.Underlying().(*types.Basic); !ok || basic.Kind() != types.String {
		return nil
	}
	var values []string
	for _, file := range r.pkg.Syntax {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				for _, ident := range spec.(*ast.ValueSpec).Names {
					c, ok := r.pkg.TypesInfo.Defs[ident].(*types.Const)
					if ok && types.Identical(c.Type(), named) {
						values = append(values, constant.StringVal(c.Val()))
					}
				}
			}
		}
	}
	return values
}

// sdkRule is what kflare-gen reads off cloudflare-go v7's type for a rule
// with one action, such as rulesets.RewriteRuleParam.
type sdkRule struct {
	// name is the type as an entry names it.
	name string
	r    *sdkReader
	typ  *types.Named
	// fields are the rule type's fields, inside their param.Field.
	fields []sdkField
	// action is the one action the rule type allows, such as rewrite.
	action string
	// params is the struct of the rule's action parameters; nil for an
	// action that takes none, such as log.
	params *types.Named
}

// rule loads the rule type an entry names.
func (s sdk) rule(name string) (*sdkRule, error) {
	pkg, obj, err := s.load(name, "rule")
	if err != nil {
		return nil, err
	}
	return ruleOf(newSDKReader(pkg), name, obj)
}

// sibling returns the rule type name, of r's package, from the package r was
// loaded with, so that the two share their types: a kind's rule types must.
func (r *sdkRule) sibling(name string) (*sdkRule, error) {
	dot := strings.LastIndex(name, ".")
	if dot <= 0 || dot == len(name)-1 {
		return nil, fmt.Errorf("rule %q is not <package>.<type>", name)
	}
	pkg := r.r.pkg
	if path := sdkModule + "/" + name[:dot]; path != pkg.PkgPath {
		return nil, fmt.Errorf("%s is not in %s, the package of the kind's other rule types", name, pkg.PkgPath)
	}
	obj, ok := pkg.Types.Scope().Lookup(name[dot+1:]).(*types.TypeName)
	if !ok {
		return nil, fmt.Errorf("%s has no type %s", pkg.PkgPath, name[dot+1:])
	}
	return ruleOf(r.r, name, obj)
}

// ruleOf reads the rule type obj, which an entry names as name, with the
// reader of its package.
func ruleOf(r *sdkReader, name string, obj *types.TypeName) (*sdkRule, error) {
	pkg, path := r.pkg, r.pkg.PkgPath
	named, ok := obj.Type().(*types.Named)
	if !ok {
		return nil, fmt.Errorf("%s is not a named type", name)
	}
	fields, err := r.fields(named, true)
	if err != nil {
		return nil, err
	}
	rule := &sdkRule{name: name, r: r, typ: named, fields: fields}
	for _, f := range fields {
		switch f.apiName {
		case ruleAction:
			if len(f.enum) != 1 {
				return nil, fmt.Errorf("%s's action allows %d actions, not one: it is not a rule type for one action",
					name, len(f.enum))
			}
			rule.action = f.enum[0]
		case ruleActionParameters:
			if isEmptyInterface(f.typ) {
				// Untyped, as for log: the action takes none, which the
				// rule's OpenAPI schema must confirm.
				continue
			}
			params, ok := f.typ.(*types.Named)
			if _, isStruct := f.typ.Underlying().(*types.Struct); !ok || !isStruct || params.Obj().Pkg() != pkg.Types {
				return nil, fmt.Errorf("%s's action parameters are a %s, not a struct of %s", name, f.typ, path)
			}
			rule.params = params
		}
	}
	if rule.action == "" {
		return nil, fmt.Errorf("%s has no action", name)
	}
	return rule, nil
}

// constantNamed returns the name of the package's constant of type t whose
// value is value, such as RulesetNewParamsRulesActionRewrite for t
// RulesetNewParamsRulesAction and value rewrite.
func (r *sdkReader) constantNamed(t, value string) (string, error) {
	obj, ok := r.pkg.Types.Scope().Lookup(t).(*types.TypeName)
	if !ok {
		return "", fmt.Errorf("%s has no type %s", r.pkg.PkgPath, t)
	}
	scope := r.pkg.Types.Scope()
	for _, name := range scope.Names() {
		c, ok := scope.Lookup(name).(*types.Const)
		if ok && types.Identical(c.Type(), obj.Type()) && constant.StringVal(c.Val()) == value {
			return name, nil
		}
	}
	return "", fmt.Errorf("%s has no %s constant %q", r.pkg.PkgPath, t, value)
}

// variants returns the structs of the package that satisfy the union
// interface union, sorted by name.
func (r *sdkReader) variants(union *types.Named) []*types.Named {
	iface, ok := union.Underlying().(*types.Interface)
	if !ok {
		return nil
	}
	var out []*types.Named
	scope := r.pkg.Types.Scope()
	for _, name := range scope.Names() {
		obj, ok := scope.Lookup(name).(*types.TypeName)
		if !ok {
			continue
		}
		named, ok := obj.Type().(*types.Named)
		if !ok {
			continue
		}
		if _, isStruct := named.Underlying().(*types.Struct); isStruct && types.Implements(named, iface) {
			out = append(out, named)
		}
	}
	return out
}

// isUnion reports whether t is a union of the package: an interface its
// variants satisfy through an unexported method.
func (r *sdkReader) isUnion(t types.Type) (*types.Named, bool) {
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() != r.pkg.Types {
		return nil, false
	}
	iface, ok := named.Underlying().(*types.Interface)
	return named, ok && iface.NumMethods() > 0
}

// settingsService is what kflare-gen reads off a cloudflare-go v7 service
// that keeps one product's settings in a zone: a read, a write and a reset,
// and no New, since the settings always exist.
type settingsService struct {
	// name is the service as an entry names it, such as
	// managed_transforms.ManagedTransformService.
	name string
	// pkg is the service's package, typeName the service type's name, and r
	// reads the package's types.
	pkg      *types.Package
	typeName string
	r        *sdkReader
	// read is the service's Get, or its List where that returns the settings
	// rather than a page of them; write is its Update or Edit; reset is its
	// Delete, which restores Cloudflare's defaults.
	read, write, reset *sdkCall
	// settings is the struct read returns.
	settings *types.Named
	// readRoute, writeRoute and resetRoute are their routes, which find their
	// operations in the OpenAPI spec.
	readRoute, writeRoute, resetRoute route

	// byID is set for a service that reads and writes each of a zone's
	// settings by its ID, as zones.SettingService does: read is its Get and
	// write its Update or Edit, each taking the setting's ID before its
	// params, and it has no reset. Its settings are those it knows, in the
	// order of their IDs: the variants of what read returns.
	byID          bool
	settingsByID  []*sdkSetting
	writeByIDBody byIDBody
}

// sdkSetting is one setting a service whose settings go by their IDs knows:
// the variant of what its read returns for that setting.
type sdkSetting struct {
	// id is the setting's ID, such as always_use_https, and goName the name
	// cloudflare-go v7's constant for the ID gives it, such as AlwaysUseHTTPS.
	id, goName string
	// doc is the variant's documentation.
	doc string
	// value is the type of the variant's value; nil for a variant that holds
	// none.
	value types.Type
}

// byIDBody is how the write of a service whose settings go by their IDs
// takes a setting's value, as zones.SettingService's Edit does: in a body, a
// union its params hold (field), whose variant body carries the value (value),
// a union of every type a value may have (variants).
type byIDBody struct {
	field    sdkField
	body     *types.Named
	value    sdkField
	variants []*types.Named
}

// settings loads the service of settings an entry names.
func (s sdk) settings(name string) (*settingsService, error) {
	pkg, typ, err := s.load(name, "service")
	if err != nil {
		return nil, err
	}
	routes, err := readRoutes(pkg)
	if err != nil {
		return nil, err
	}
	r := newSDKReader(pkg)
	methods := types.NewMethodSet(types.NewPointer(typ.Type()))
	if methods.Lookup(pkg.Types, "New") != nil {
		return nil, fmt.Errorf("%s has a New: it creates objects, which a settings kind does not", name)
	}
	svc := &settingsService{name: name, pkg: pkg.Types, typeName: typ.Name(), r: r}
	// A Get that takes a setting's ID reads one of the zone's settings.
	if get := methods.Lookup(pkg.Types, "Get"); get != nil && takesSettingID(get.Type().(*types.Signature)) {
		if err := svc.loadByID(methods, routes); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return svc, nil
	}

	for _, method := range []string{"Get", listMethod} {
		c, err := r.method(methods, method)
		if errors.Is(err, errNoMethod) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result := c.sig.Results().At(0).Type()
		settings, ok := structOf(result, pkg.Types)
		if !ok {
			return nil, fmt.Errorf("%s returns %s, not settings: a pointer to a struct of %s", method,
				typeName(result), pkg.PkgPath)
		}
		svc.read, svc.settings = c, settings
		break
	}
	if svc.read == nil {
		return nil, fmt.Errorf("%s has no Get or List to read the settings with", name)
	}
	for _, method := range writeMethods {
		c, err := r.method(methods, method)
		if errors.Is(err, errNoMethod) {
			continue
		}
		if err != nil {
			return nil, err
		}
		svc.write = c
		break
	}
	if svc.write == nil {
		return nil, fmt.Errorf("%s has no Update or Edit to write the settings with", name)
	}
	svc.reset, err = r.resetMethod(methods, "Delete")
	if errors.Is(err, errNoMethod) {
		return nil, fmt.Errorf("%s has no Delete, which the shared settings reconciler resets the settings with",
			name)
	}
	if err != nil {
		return nil, err
	}

	for _, c := range []struct {
		call *sdkCall
		into *route
	}{{svc.read, &svc.readRoute}, {svc.write, &svc.writeRoute}, {svc.reset, &svc.resetRoute}} {
		route, ok := routes[typ.Name()+"."+c.call.name]
		if !ok {
			return nil, fmt.Errorf("%s's api.md has no route for %s.%s", pkg.PkgPath, typ.Name(), c.call.name)
		}
		*c.into = route
	}
	return svc, nil
}

// structOf returns T for *T, a struct the package pkg declares.
func structOf(t types.Type, pkg *types.Package) (*types.Named, bool) {
	ptr, ok := t.(*types.Pointer)
	if !ok {
		return nil, false
	}
	named, ok := ptr.Elem().(*types.Named)
	if !ok || named.Obj().Pkg() != pkg {
		return nil, false
	}
	_, isStruct := named.Underlying().(*types.Struct)
	return named, isStruct
}

// resetMethod reads a method shaped (ctx, params, opts...) error, as
// cloudflare-go v7 declares a call whose response has no body, or shaped as
// method reads one: a Delete, whose result goes unread.
func (r *sdkReader) resetMethod(methods *types.MethodSet, name string) (*sdkCall, error) {
	sel := methods.Lookup(r.pkg.Types, name)
	if sel == nil {
		return nil, fmt.Errorf("%s: %w", name, errNoMethod)
	}
	sig := sel.Type().(*types.Signature)
	if sig.Results().Len() == 2 {
		return r.method(methods, name)
	}
	errorType := types.Universe.Lookup("error").Type()
	if !sig.Variadic() || sig.Params().Len() < 3 || sig.Results().Len() != 1 ||
		!types.Identical(sig.Results().At(0).Type(), errorType) {
		return nil, fmt.Errorf("%s is not shaped (ctx, params, opts...) error, or (*result, error)", name)
	}
	c := &sdkCall{name: name, sig: sig, params: sig.Params().At(sig.Params().Len() - 2).Type()}
	var err error
	if c.paramFields, err = r.fields(c.params, true); err != nil {
		return nil, fmt.Errorf("%s's params: %w", name, err)
	}
	return c, nil
}

// takesSettingID reports whether a service method is shaped (ctx, id string,
// params, opts...): it names one setting by its ID.
func takesSettingID(sig *types.Signature) bool {
	params := sig.Params()
	return params.Len() == 4 && types.Identical(params.At(1).Type(), types.Typ[types.String])
}

// loadByID reads a service whose Get and Update (or Edit) take a setting's
// ID: the settings it knows, from the variants of what Get returns, and how
// its write takes a setting's value.
func (svc *settingsService) loadByID(methods *types.MethodSet, routes map[string]route) error {
	r := svc.r
	read, err := r.method(methods, "Get")
	if err != nil {
		return err
	}
	svc.read = read
	if methods.Lookup(r.pkg.Types, "Delete") != nil {
		return fmt.Errorf("its settings go by their IDs, and it has a Delete, which kflare-gen cannot call yet")
	}
	for _, method := range writeMethods {
		sel := methods.Lookup(r.pkg.Types, method)
		if sel == nil {
			continue
		}
		if !takesSettingID(sel.Type().(*types.Signature)) {
			return fmt.Errorf("its Get takes a setting's ID, and its %s does not", method)
		}
		c, body, err := r.bodyMethod(sel, method)
		if err != nil {
			return err
		}
		svc.write, svc.writeByIDBody.field = c, body
		break
	}
	if svc.write == nil {
		return fmt.Errorf("it has no Update or Edit to write the settings with")
	}
	svc.byID = true
	if err := svc.readSettingsByID(); err != nil {
		return err
	}
	if err := svc.readValueBody(); err != nil {
		return err
	}
	for _, c := range []struct {
		call *sdkCall
		into *route
	}{{svc.read, &svc.readRoute}, {svc.write, &svc.writeRoute}} {
		route, ok := routes[svc.typeName+"."+c.call.name]
		if !ok {
			return fmt.Errorf("%s's api.md has no route for %s.%s", svc.pkg.Path(), svc.typeName, c.call.name)
		}
		*c.into = route
	}
	return nil
}

// bodyMethod reads the method sel, named name, as method does, but for params
// that hold, besides their param.Fields, a body that is a union, as
// zones.SettingEditParams does: a field named body in JSON, whose type is a
// union of the package. It returns the body's field too, its type the union.
func (r *sdkReader) bodyMethod(sel *types.Selection, name string) (*sdkCall, sdkField, error) {
	sig := sel.Type().(*types.Signature)
	if !sig.Variadic() || sig.Params().Len() < 3 || sig.Results().Len() != 2 {
		return nil, sdkField{}, fmt.Errorf("%s is not shaped (ctx, [id,] params, opts...) (*result, error)", name)
	}
	c := &sdkCall{name: name, sig: sig, params: sig.Params().At(sig.Params().Len() - 2).Type()}
	named, st, err := r.structType(c.params)
	if err != nil {
		return nil, sdkField{}, fmt.Errorf("%s's params: %w", name, err)
	}
	docs := r.fieldDocs(named.Obj().Name())
	var body sdkField
	for i := range st.NumFields() {
		v, tag := st.Field(i), reflect.StructTag(st.Tag(i))
		if _, union := r.isUnion(v.Type()); union && jsonName(tag) == "body" {
			body = sdkField{goName: v.Name(), apiName: "body", in: inBody, typ: v.Type(), required: true}
			continue
		}
		f, ok, err := r.field(named, v, tag, docs, true)
		if err != nil {
			return nil, sdkField{}, fmt.Errorf("%s's params: %w", name, err)
		}
		if ok {
			c.paramFields = append(c.paramFields, f)
		}
	}
	if body.typ == nil {
		return nil, sdkField{}, fmt.Errorf("%s's params hold no body that is a union of settings' values", name)
	}
	return c, body, nil
}

// readSettingsByID reads the settings a service whose settings go by their
// IDs knows: the variants of the union what its Get returns holds
// (AsUnion), each with an ID field whose type has one constant, the
// setting's ID.
func (svc *settingsService) readSettingsByID() error {
	r := svc.r
	result := svc.read.sig.Results().At(0).Type()
	asUnion := types.NewMethodSet(result).Lookup(r.pkg.Types, "AsUnion")
	if asUnion == nil {
		return fmt.Errorf("%s returns %s, which holds no union of settings (AsUnion)", svc.read.name,
			typeName(result))
	}
	union, ok := r.isUnion(asUnion.Type().(*types.Signature).Results().At(0).Type())
	if !ok {
		return fmt.Errorf("the AsUnion of what Get returns returns no union of the package")
	}
	byID := map[string]*sdkSetting{}
	for _, variant := range r.variants(union) {
		fields, err := r.fields(variant, false)
		if err != nil {
			return err
		}
		name := variant.Obj().Name()
		s := &sdkSetting{doc: r.typeDocs[name]}
		for _, f := range fields {
			switch f.apiName {
			case "id":
				if len(f.enum) != 1 {
					return fmt.Errorf("%s's id is a %s, not a type of one constant: the setting's ID", name,
						typeName(f.typ))
				}
				s.id = f.enum[0]
				idType := f.typ.(*types.Named).Obj().Name()
				constant, err := r.constantNamed(idType, s.id)
				if err != nil {
					return err
				}
				s.goName = strings.ReplaceAll(strings.TrimPrefix(constant, idType), "_", "")
			case "value":
				s.value = f.typ
			}
		}
		if s.id == "" {
			return fmt.Errorf("%s has no id, which names its setting", name)
		}
		if other, ok := byID[s.id]; ok {
			return fmt.Errorf("two variants of what Get returns are the setting %s: %s's ID is %s's", s.id, name,
				other.goName)
		}
		byID[s.id] = s
	}
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		svc.settingsByID = append(svc.settingsByID, byID[id])
	}
	return nil
}

// readValueBody reads how the write of a service whose settings go by their
// IDs takes a setting's value: the variant of its body's union that holds
// one field, value, a union, and the types of that union, in the package or
// in those it imports, such as cloudflare-go v7's shared.UnionString.
func (svc *settingsService) readValueBody() error {
	r, b := svc.r, &svc.writeByIDBody
	b.body, b.value, b.variants = nil, sdkField{}, nil
	union := b.field.typ.(*types.Named)
	for _, variant := range r.variants(union) {
		fields, err := r.fields(variant, true)
		if err != nil || len(fields) != 1 || fields[0].apiName != "value" {
			continue
		}
		if _, ok := r.isUnion(fields[0].typ); ok {
			b.body, b.value = variant, fields[0]
			break
		}
	}
	if b.body == nil {
		return fmt.Errorf("%s's body has no variant that holds only a value: the setting's", svc.write.name)
	}
	values := b.value.typ.(*types.Named)
	iface := values.Underlying().(*types.Interface)
	for _, pkg := range append([]*types.Package{r.pkg.Types}, r.pkg.Types.Imports()...) {
		scope := pkg.Scope()
		for _, name := range scope.Names() {
			obj, ok := scope.Lookup(name).(*types.TypeName)
			if !ok || !obj.Exported() {
				continue
			}
			named, ok := obj.Type().(*types.Named)
			if ok && !types.IsInterface(named) && types.Implements(named, iface) {
				b.variants = append(b.variants, named)
			}
		}
	}
	return nil
}

// valueVariant returns the type of the union of values that carries a
// setting's value of type value: a string type of the union for a string, a
// number type for a number, and, for an object, the struct of the union whose
// fields have the object's names in JSON.
func (svc *settingsService) valueVariant(value types.Type) (*types.Named, error) {
	r := svc.r
	var matches []*types.Named
	switch class := basicClass(value); {
	case class == types.IsString || class == types.IsNumeric:
		for _, v := range svc.writeByIDBody.variants {
			if _, isStruct := v.Underlying().(*types.Struct); !isStruct && basicClass(v) == class {
				matches = append(matches, v)
			}
		}
	case class == 0 && isStructType(value):
		names, err := jsonNames(r, value, false)
		if err != nil {
			return nil, err
		}
		for _, v := range svc.writeByIDBody.variants {
			if !isStructType(v) || v.Obj().Pkg() != r.pkg.Types {
				continue
			}
			if variant, err := jsonNames(r, v, true); err == nil && slices.Equal(variant, names) {
				matches = append(matches, v)
			}
		}
	default:
		return nil, fmt.Errorf("its value is a %s, which kflare-gen cannot write by the setting's ID yet",
			typeName(value))
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("its value is a %s, and no type of %s takes one", typeName(value),
			typeName(svc.writeByIDBody.value.typ))
	case 1:
		return matches[0], nil
	}
	return nil, fmt.Errorf("its value is a %s, which %d types of %s take", typeName(value), len(matches),
		typeName(svc.writeByIDBody.value.typ))
}

// isStructType reports whether t is a struct type.
func isStructType(t types.Type) bool {
	_, ok := t.Underlying().(*types.Struct)
	return ok
}

// jsonNames returns the JSON names of a struct's fields, sorted.
func jsonNames(r *sdkReader, t types.Type, params bool) ([]string, error) {
	fields, err := r.fields(t, params)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(fields))
	for i, f := range fields {
		names[i] = f.apiName
	}
	slices.Sort(names)
	return names, nil
}
