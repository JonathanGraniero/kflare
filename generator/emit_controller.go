/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"encoding/json"
	"fmt"
	"go/format"
	"go/types"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// The packages generated controllers name, besides cloudflare-go v7's.
const (
	kflareModule  = "github.com/JonathanGraniero/kflare"
	apiPackage    = kflareModule + "/api/v1alpha1"
	cfPackage     = kflareModule + "/pkg/cloudflare"
	clientPackage = "sigs.k8s.io/controller-runtime/pkg/client"
)

// goFile is a generated Go file: its body, and the packages the body names.
type goFile struct {
	strings.Builder
	// imports holds the name the file gives each package, by path.
	imports map[string]string
}

func newGoFile() *goFile {
	return &goFile{imports: map[string]string{}}
}

func (f *goFile) printf(format string, args ...any) {
	fmt.Fprintf(&f.Builder, format, args...)
}

// use records that the file names the package at path as name, and returns
// the name.
func (f *goFile) use(path, name string) string {
	f.imports[path] = name
	return name
}

// api returns the API types' package's name for the file.
func (f *goFile) api() string {
	return f.use(apiPackage, "cloudflarev1alpha1")
}

// typ writes t as the file names it.
func (f *goFile) typ(t types.Type) string {
	return types.TypeString(t, func(p *types.Package) string { return f.use(p.Path(), p.Name()) })
}

// convert returns value, of type from, as type to. Both are basic types or
// named types of one: a string converts to a string, a bool to a bool and a
// number to a number.
func (f *goFile) convert(value string, from, to types.Type) (string, error) {
	if types.Identical(from, to) {
		return value, nil
	}
	if class := basicClass(from); class == 0 || class != basicClass(to) {
		return "", fmt.Errorf("kflare-gen cannot convert a %s to a %s", typeName(from), typeName(to))
	}
	return f.typ(to) + "(" + value + ")", nil
}

// basicClass returns whether t is a string, a bool or a number type; 0 when it
// is none of them.
func basicClass(t types.Type) types.BasicInfo {
	basic, ok := t.Underlying().(*types.Basic)
	if !ok {
		return 0
	}
	for _, class := range []types.BasicInfo{types.IsString, types.IsBoolean, types.IsNumeric} {
		if basic.Info()&class != 0 {
			return class
		}
	}
	return 0
}

// source returns the file's formatted source, in package controller: header,
// the generated marker, the package clause, the imports the body uses and the
// body.
func (f *goFile) source(header string) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s\n\npackage controller\n\n", strings.TrimRight(header, "\n"), generatedMarker)
	byName := map[string]string{}
	var groups [3][]string
	for _, p := range slices.Sorted(maps.Keys(f.imports)) {
		name := f.imports[p]
		if other, ok := byName[name]; ok {
			return nil, fmt.Errorf("%s and %s are both named %s", other, p, name)
		}
		byName[name] = p
		spec := strconv.Quote(p)
		if name != path.Base(p) {
			spec = name + " " + spec
		}
		group := 1
		switch {
		case !strings.Contains(strings.Split(p, "/")[0], "."):
			group = 0
		case strings.HasPrefix(p, kflareModule+"/"):
			group = 2
		}
		groups[group] = append(groups[group], spec)
	}
	b.WriteString("import (\n")
	written := false
	for _, group := range groups {
		if len(group) == 0 {
			continue
		}
		if written {
			b.WriteString("\n")
		}
		for _, spec := range group {
			b.WriteString("\t" + spec + "\n")
		}
		written = true
	}
	b.WriteString(")\n\n")
	b.WriteString(f.String())
	src, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("formatting: %w\n%s", err, b.String())
	}
	return src, nil
}

// specType is the Go type of a spec field, without its pointer.
func specType(f field) types.Type {
	switch strings.TrimPrefix(f.goType, "*") {
	case typeBool:
		return types.Typ[types.Bool]
	case typeInt64:
		return types.Typ[types.Int64]
	}
	return types.Typ[types.String]
}

// pluralOf returns a kind's name or a noun made plural, as plural makes a
// resource name, but keeping its capitals: KVNamespaces, AccessPolicies, KV
// namespaces.
func pluralOf(name string) string {
	p, lower := plural(name), strings.ToLower(name)
	common := 0
	for common < len(lower) && lower[common] == p[common] {
		common++
	}
	return name[:common] + p[common:]
}

// prose joins names as a sentence lists them: a, b and c.
func prose(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// emitController returns the Go source of a kind's controller: the narrow
// interface to its cloudflare-go v7 service, its reconciler, RBAC markers and
// the descriptor the shared CRUD reconciler runs it with.
func emitController(c *controller, header string) ([]byte, error) {
	f := newGoFile()
	name, v := c.name, c.varName
	api := name + "API"
	svcPkg := f.use(c.svc.pkg.Path(), c.svc.pkg.Name())
	service := svcPkg + "." + c.svc.typeName
	obj := "*" + f.api() + "." + name
	object, listed := f.typ(c.object), f.typ(c.listed)
	ctx := f.use("context", "context")
	client := f.use(clientPackage, "client")

	if c.list != nil && !c.singlePage {
		comment(&f.Builder, "", wrap(fmt.Sprintf("%sListPageSize is how many %s a page of the list asks for: the "+
			"most the OpenAPI spec allows.", v, pluralOf(c.noun))))
		f.printf("const %sListPageSize = %d\n\n", v, c.pageSize)
	}

	comment(&f.Builder, "", wrap(fmt.Sprintf("%s is the part of cloudflare-go v7's %s that the %s reconciler calls. "+
		"Tests inject a fake; in production it is a *%s.", api, service, name, service)))
	f.printf("type %s interface {\n", api)
	for _, call := range c.calls() {
		f.printf("\t%s\n", interfaceMethod(f, call))
	}
	production := service
	if c.hooksAPI() {
		comment(&f.Builder, "\t", []string{"The calls the kind's hooks make."})
		f.printf("\t%sHooksAPI\n", name)
		production = v + "Service"
	}
	f.printf("}\n\nvar _ %s = (*%s)(nil)\n\n", api, production)

	emitCRUDReconcilerType(f, name, api)

	for _, marker := range rbacMarkers(c) {
		f.printf("// %s\n", marker)
	}
	f.printf("\n")

	comment(&f.Builder, "", wrap(kindDoc(c)))
	f.printf("var %sKind = crudResourceKind[%s, %s, %s, %s]{\n", v, obj, api, object, listed)
	f.printf("name: %q,\nnoun: %q,\n", name, c.noun)
	f.printf("newObject: func() %s { return &%s.%s{} },\n", obj, f.api(), name)
	f.printf("newList: func() %s.ObjectList { return &%s.%sList{} },\n\n", client, f.api(), name)
	if c.zoned {
		f.printf("zoneRef: func(obj %s) string { return obj.Spec.ZoneRef.Name },\n", obj)
		f.printf("statusZoneID: func(obj %s) *string { return &obj.Status.CloudflareMetadata.%s },\n", obj,
			c.statusZoneID)
	} else {
		f.printf("accountRef: func(obj %s) string { return obj.Spec.AccountRef.Name },\n", obj)
	}
	f.printf("key: func(obj %s) string { return obj.Spec.%s },\n", obj, c.key.goName)
	f.printf("claimLabel: %s.%sIDLabel,\nconflictReason: %q,\n", f.api(), name, c.conflictReason)
	if len(c.address) > 0 {
		values := make([]string, 0, len(c.address))
		for _, af := range c.address {
			values = append(values, "obj.Spec."+af.goName)
		}
		f.printf("address: func(obj %s) []string { return []string{%s} },\n", obj, strings.Join(values, ", "))
	}
	f.printf("statusID: func(obj %s) string {\nreturn obj.Status.CloudflareMetadata.%s\n},\n", obj,
		c.status[0].goName)
	f.printf("observe: func(obj %s, current *%s) {\n", obj, object)
	for _, s := range c.status {
		value, err := f.convert("current."+s.object.goName, s.object.typ, specType(s.field))
		if err != nil {
			return nil, fmt.Errorf("%s: status.cloudflareMetadata.%s: %w", name, s.jsonName, err)
		}
		f.printf("obj.Status.CloudflareMetadata.%s = %s\n", s.goName, value)
	}
	f.printf("},\n\n")

	f.printf("id: func(current *%s) string { return current.%s },\n", object, c.id.goName)
	matches, usesRefs, err := matchesExpr(f, c, false)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if matches == "" {
		f.printf("matches: func(%s, *%s, map[string]string) bool { return true },\n", obj, object)
	} else {
		f.printf("matches: func(obj %s, current *%s, %s map[string]string) bool {\nreturn %s\n},\n", obj, object,
			refsParam(usesRefs), matches)
	}
	if c.list != nil {
		listedID, err := f.convert("listed."+c.listedID.goName, c.listedID.typ, types.Typ[types.String])
		if err != nil {
			return nil, fmt.Errorf("%s: the listed ID: %w", name, err)
		}
		listedKey, err := f.convert("listed."+c.listedKey.goName, c.listedKey.typ, types.Typ[types.String])
		if err != nil {
			return nil, fmt.Errorf("%s: adopt.by: %w", name, err)
		}
		f.printf("listedID: func(listed *%s) string { return %s },\n", listed, listedID)
		f.printf("listedKey: func(listed *%s) string { return %s },\n", listed, listedKey)
	}
	f.printf("\n")

	if err := emitCalls(f, c, api, obj, object); err != nil {
		return nil, err
	}
	if len(c.references) > 0 {
		f.printf("\nreferences: []crudReference[%s]{\n", obj)
		for _, af := range c.references {
			ref := "obj.Spec." + af.goName
			refName := fmt.Sprintf("func(obj %s) string { return %s.Name }", obj, ref)
			if af.optional {
				refName = fmt.Sprintf("func(obj %s) string {\nif %s == nil {\nreturn \"\"\n}\nreturn %s.Name\n}", obj, ref, ref)
			}
			target := "*" + f.api() + "." + af.ref.Kind
			f.printf("reference(%q, %q,\n%s,\nfunc(ref %s) string { return ref.Spec.AccountRef.Name },\n"+
				"func(ref %s) string { return ref.Spec.%s }),\n", af.jsonName, af.ref.Kind, refName, target, target,
				upperCamel(af.ref.Field))
		}
		f.printf("},\n")
	}
	if len(c.deletionBlockers) > 0 {
		f.printf("\ndeletionBlockers: []deletionBlocker{\n")
		for _, b := range c.deletionBlockers {
			comment(&f.Builder, "\t", wrapIndented(b.Why))
			f.printf("{code: %d, reason: %q,\nmessage: %q},\n", b.Code, b.Reason, b.Message)
		}
		f.printf("},\n")
	}
	if len(c.dependents) > 0 {
		f.printf("\ndependents: %sDependents,\n", v)
	}
	if c.hooks != nil {
		f.printf("\nhooks: %sHooks,\n", v)
	}
	f.printf("}\n\n")

	if len(c.dependents) > 0 {
		comment(&f.Builder, "", wrap(fmt.Sprintf("%sDependents use %s, which waits for them before it is deleted.",
			v, article(name))))
		f.printf("var %sDependents = []dependentKind{\n", v)
		for _, d := range c.dependents {
			comment(&f.Builder, "\t", wrapIndented(d.Reason))
			f.printf("dependents(%q, func() %s.ObjectList { return &%s.%sList{} },\n%s),\n", d.Kind, client, f.api(),
				d.Kind, d.Refs)
		}
		f.printf("}\n\n")
	}

	ctrl := f.use("sigs.k8s.io/controller-runtime", "ctrl")
	f.printf("func (r *%sReconciler) Reconcile(ctx %s.Context, req %s.Request) (%s.Result, error) {\n", name, ctx, ctrl,
		ctrl)
	f.printf("return r.crud().reconcile(ctx, req)\n}\n\n")
	crud := fmt.Sprintf("crudResourceReconciler[%s, %s, %s, %s]", obj, api, object, listed)
	emitCRUDReconcilerSetup(f, name, api, v, crud, setupDoc(c))
	f.printf("\n")

	comment(&f.Builder, "", wrap(fmt.Sprintf("default%s is the production factory: cloudflare-go v7's %s, built "+
		"from cfpkg.New's options for the token.", api, service)))
	f.printf("func default%s(token string) (%s, error) {\n", api, api)
	f.printf("client, err := %s.New(token)\nif err != nil {\nreturn nil, err\n}\n", f.use(cfPackage, "cfpkg"))
	build := fmt.Sprintf("%s.New%s(client.V7Options()...)", svcPkg, c.svc.typeName)
	if c.hooksAPI() {
		build = fmt.Sprintf("&%sService{%s: %s}", v, c.svc.typeName, build)
	}
	f.printf("return %s, nil\n}\n", build)
	return f.source(header)
}

// emitCRUDReconcilerType writes the exported reconciler of a kind the shared
// CRUD reconciler runs, generated or hooked, whose narrow interface is api:
// its client and scheme, the interface's factory, and the reader that reads
// past the manager's cache.
func emitCRUDReconcilerType(f *goFile, name, api string) {
	comment(&f.Builder, "", wrap(fmt.Sprintf("%sReconciler reconciles %s object, with the shared CRUD reconciler "+
		"(crudresource_controller.go).", name, article(name))))
	client := f.use(clientPackage, "client")
	f.printf("type %sReconciler struct {\n\t%s.Client\n\tScheme *%s.Scheme\n\n", name, client,
		f.use("k8s.io/apimachinery/pkg/runtime", "runtime"))
	comment(&f.Builder, "\t", wrapIndented(fmt.Sprintf("New%s constructs %s from a raw API token. Defaults to "+
		"default%s; tests inject a fake.", api, article(api), api)))
	f.printf("\tNew%s func(token string) (%s, error)\n\n", api, api)
	comment(&f.Builder, "\t", wrapIndented("apiReader reads from the API server, past the manager's cache: "+
		"SetupWithManager sets the manager's. Without one, Client reads, as in tests that reconcile without a "+
		"manager."))
	f.printf("\tapiReader %s.Reader\n}\n\n", client)
}

// emitCRUDReconcilerSetup writes the SetupWithManager (documented by doc) and
// crud methods of the reconciler emitCRUDReconcilerType writes, for the kind
// whose descriptor is <v>Kind and whose shared reconciler's type is crud.
func emitCRUDReconcilerSetup(f *goFile, name, api, v, crud, doc string) {
	comment(&f.Builder, "", wrap(doc))
	f.printf("func (r *%sReconciler) SetupWithManager(mgr %s.Manager) error {\nr.apiReader = mgr.GetAPIReader()\n"+
		"return r.crud().setup(mgr, r)\n}\n\n", name, f.use("sigs.k8s.io/controller-runtime", "ctrl"))
	comment(&f.Builder, "", wrap(fmt.Sprintf("crud returns the shared CRUD reconciler for %s, with the injected "+
		"factory or the production one, and the manager's API reader or the client.", pluralOf(name))))
	f.printf("func (r *%sReconciler) crud() *%s {\n", name, crud)
	f.printf("newAPI := r.New%s\nif newAPI == nil {\nnewAPI = default%s\n}\n", api, api)
	f.printf("apiReader := r.apiReader\nif apiReader == nil {\napiReader = r.Client\n}\n")
	f.printf("return &%s{\nClient: r.Client, apiReader: apiReader, kind: %sKind, newAPI: newAPI,\n}\n}\n", crud, v)
}

// setupDoc documents a kind's SetupWithManager: what it watches, and why.
func setupDoc(c *controller) string {
	name := c.name
	clauses := []string{fmt.Sprintf("CloudflareAccounts, so %s waiting for its account is reconciled once the "+
		"account is ready", article(name))}
	if c.zoned {
		clauses[0] = fmt.Sprintf("Zones, so %s waiting for its Zone is reconciled once the Zone is ready", article(name))
	}
	for _, af := range c.references {
		clauses = append(clauses, fmt.Sprintf("%s, so %s waiting for the %s its %s names is reconciled once it is "+
			"ready", pluralOf(af.ref.Kind), article(name), af.ref.Kind, af.jsonName))
	}
	if len(c.dependents) > 0 {
		kinds := make([]string, 0, len(c.dependents))
		for _, d := range c.dependents {
			kinds = append(kinds, pluralOf(d.Kind))
		}
		clauses = append(clauses, fmt.Sprintf("deleted %s, so %s waiting for them is released", prose(kinds),
			article(name)))
	}
	doc := fmt.Sprintf("SetupWithManager registers %sReconciler with the manager. It watches %s", name,
		strings.Join(clauses[:len(clauses)-1], ", "))
	if len(clauses) > 1 {
		doc += ", and " + clauses[len(clauses)-1]
	} else {
		doc += clauses[0]
	}
	return doc + "."
}

// hooksAPI reports whether the kind's hooks make calls of their own.
func (c *controller) hooksAPI() bool { return c.hooks != nil && c.hooks.API }

// emitCalls writes the descriptor's calls to Cloudflare: each builds its
// params from the parent's ID, the resource, the address and the values its
// references give, and calls the narrow interface api.
func emitCalls(f *goFile, c *controller, api, obj, object string) error {
	name, v := c.name, c.varName
	ctx := f.use("context", "context")
	getParams, err := addressStatements(f, c, c.get)
	if err != nil {
		return err
	}
	f.printf("get: func(ctx %s.Context, api %s, %s %s,\nparentID, id string) (*%s, error) {\n", ctx, api,
		objParam(getParams), obj, object)
	if err := callBody(f, c, c.get, getParams, "return api.Get(ctx, id, params)"); err != nil {
		return err
	}
	f.printf("},\n")
	if c.list != nil {
		listParams, err := addressStatements(f, c, c.list)
		if err != nil {
			return err
		}
		if c.singlePage {
			f.printf("listAll: func(ctx %s.Context, api %s, %s %s,\nparentID string) (%s, error) {\n", ctx, api,
				objParam(listParams), obj, f.typ(c.list.sig.Results().At(0).Type()))
		} else {
			page, err := f.convert("page", types.Typ[types.Int64], c.page.typ)
			if err != nil {
				return fmt.Errorf("%s: List's page: %w", name, err)
			}
			f.printf("list: func(ctx %s.Context, api %s, %s %s,\nparentID string, page int64) (%s, error) {\n", ctx,
				api, objParam(listParams), obj, f.typ(c.list.sig.Results().At(0).Type()))
			listParams = append(listParams,
				fmt.Sprintf("params.%s.Value, params.%s.Present = %s, true", c.page.goName, c.page.goName, page),
				fmt.Sprintf("params.%s.Value, params.%s.Present = %sListPageSize, true", c.perPage.goName,
					c.perPage.goName, v))
		}
		if err := callBody(f, c, c.list, listParams, "return api.List(ctx, params)"); err != nil {
			return err
		}
		f.printf("},\n")
	}
	createParams, err := paramStatements(f, c, func(af apiField) *sdkField { return af.create }, false)
	if err != nil {
		return fmt.Errorf("%s: New: %w", name, err)
	}
	f.printf("create: func(ctx %s.Context, api %s, parentID string,\nobj %s, %s map[string]string) (*%s, error) {\n",
		ctx, api, obj, refsParam(readsRefs(createParams) || c.hooks.params()), object)
	ret, err := resultReturn(f, c, "api.New(ctx, params)", c.created)
	if err != nil {
		return fmt.Errorf("%s: New: %w", name, err)
	}
	if err := callBody(f, c, c.create, createParams, ret); err != nil {
		return err
	}
	f.printf("},\n")
	if c.update != nil {
		updateParams, err := paramStatements(f, c, func(af apiField) *sdkField { return af.update }, true)
		if err != nil {
			return fmt.Errorf("%s: %s: %w", name, c.update.name, err)
		}
		address, err := addressStatements(f, c, c.update)
		if err != nil {
			return err
		}
		f.printf("update: func(ctx %s.Context, api %s, obj %s,\nparentID, id string, %s map[string]string) (*%s, "+
			"error) {\n", ctx, api, obj, refsParam(readsRefs(updateParams) || c.hooks.params()), object)
		ret, err := resultReturn(f, c, fmt.Sprintf("api.%s(ctx, id, params)", c.update.name), c.updated)
		if err != nil {
			return fmt.Errorf("%s: %s: %w", name, c.update.name, err)
		}
		if err := callBody(f, c, c.update, append(address, updateParams...), ret); err != nil {
			return err
		}
		f.printf("},\n")
	}
	deleteParams, err := addressStatements(f, c, c.remove)
	if err != nil {
		return err
	}
	f.printf("delete: func(ctx %s.Context, api %s, %s %s,\nparentID, id string) error {\n", ctx, api,
		objParam(deleteParams), obj)
	if err := callBody(f, c, c.remove, deleteParams, "_, err := api.Delete(ctx, id, params)\nreturn err"); err != nil {
		return err
	}
	f.printf("},\n")
	return nil
}

// resultReturn returns the statements that return what call, a call to New
// or Update, returns as the object Get returns: as it is, or, when the call
// returns a type of its own (result), converted field by field.
func resultReturn(f *goFile, c *controller, call string, result types.Type) (string, error) {
	if result == nil {
		return "return " + call, nil
	}
	converted, err := convertObject(f, "result", result, c.object, c.svc.object)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("result, err := %s\nif err != nil {\nreturn nil, err\n}\nreturn &%s, nil", call, converted), nil
}

// convertObject returns a composite literal of the type to that holds the
// fields of value, of the type from: each of fields, the fields of the object
// Get returns, by its Go name, converted. sameFields has checked that both
// types hold them.
func convertObject(f *goFile, value string, from, to types.Type, fields []sdkField) (string, error) {
	var out []string
	for _, field := range fields {
		converted, err := f.convert(value+"."+field.goName, structFieldType(from, field.goName),
			structFieldType(to, field.goName))
		if err != nil {
			return "", fmt.Errorf("converting %s: %w", field.goName, err)
		}
		out = append(out, fmt.Sprintf("%s: %s", field.goName, converted))
	}
	return compositeLiteral(f.typ(to), out), nil
}

// structFieldType returns the type of the field name of the struct type t.
func structFieldType(t types.Type, name string) types.Type {
	st := t.Underlying().(*types.Struct)
	return st.Field(slices.IndexFunc(structFields(st), func(v *types.Var) bool { return v.Name() == name })).Type()
}

// refsParam names a descriptor function's references parameter: refs when its
// body reads the values the references give, _ when it does not.
func refsParam(used bool) string {
	if used {
		return "refs"
	}
	return "_"
}

// readsRefs reports whether statements read the values references give.
func readsRefs(statements []string) bool {
	return slices.ContainsFunc(statements, func(s string) bool { return strings.Contains(s, "refs[") })
}

// interfaceMethod declares call as the narrow interface does, with the SDK's
// parameter names: returning a result and an error, or an error alone.
func interfaceMethod(f *goFile, call *sdkCall) string {
	params := call.sig.Params()
	args := make([]string, 0, params.Len())
	for i := range params.Len() {
		p := params.At(i)
		t := f.typ(p.Type())
		if i == params.Len()-1 && call.sig.Variadic() {
			t = "..." + f.typ(p.Type().(*types.Slice).Elem())
		}
		args = append(args, p.Name()+" "+t)
	}
	results := "error"
	if call.sig.Results().Len() == 2 {
		results = "(" + f.typ(call.sig.Results().At(0).Type()) + ", error)"
	}
	joined := strings.Join(args, ", ")
	// The last parameter goes on a line of its own when the declaration
	// would be long.
	if len(call.name)+len(joined)+len(results) > 109 {
		joined = strings.Join(args[:len(args)-1], ", ") + ",\n" + args[len(args)-1]
	}
	return fmt.Sprintf("%s(%s) %s", call.name, joined, results)
}

// bodyHook names the hand-written function that finishes call's union body:
// dnsRecordNewBody for a DNS record's New.
func bodyHook(c *controller, call *sdkCall) string {
	return c.varName + call.name + "Body"
}

// callBody writes the body of a descriptor's call: its params, with the
// parent's ID and the statements given, then ret, which returns.
func callBody(f *goFile, c *controller, call *sdkCall, statements []string, ret string) error {
	i := slices.IndexFunc(call.paramFields, c.isParent)
	parent, err := f.convert("parentID", types.Typ[types.String], call.paramFields[i].typ)
	if err != nil {
		return fmt.Errorf("%s: %s's %s: %w", c.name, call.name, call.paramFields[i].apiName, err)
	}
	f.printf("var params %s\n", f.typ(call.params))
	f.printf("params.%s.Value, params.%s.Present = %s, true\n", call.paramFields[i].goName,
		call.paramFields[i].goName, parent)
	if call.body != nil {
		f.printf("var body %s\n", f.typ(call.body.typ))
	}
	for _, s := range statements {
		f.printf("%s\n", s)
	}
	if call.body != nil {
		if c.hooks.params() {
			f.printf("%s(obj, refs, &body)\n", bodyHook(c, call))
		}
		f.printf("params.%s = body\n", call.body.goName)
	}
	f.printf("%s\n", ret)
	return nil
}

// paramStatements returns the statements that set a call's params from the
// spec and the fixed values; param picks each field's param in the call.
func paramStatements(f *goFile, c *controller, param func(apiField) *sdkField, update bool) ([]string, error) {
	var out []string
	for _, af := range append(slices.Clone(c.fields), c.fixedFields...) {
		p := param(af)
		if p == nil {
			continue
		}
		s, err := assignParam(f, af, p, update && af.clearable)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// addressStatements returns the statements that set call's params that
// carry the object's address, from the spec.
func addressStatements(f *goFile, c *controller, call *sdkCall) ([]string, error) {
	var out []string
	for _, af := range c.address {
		p := addressParam(call, af)
		if p == nil {
			continue
		}
		s, err := assignParam(f, af, p, false)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", c.name, call.name, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// assignParam returns the statement that sets the param p from the spec
// field or fixed value af: an optional field's only when the spec sets it,
// unless always is set. A reference's value is the one it gives, in refs.
func assignParam(f *goFile, af apiField, p *sdkField, always bool) (string, error) {
	set := func(value string, from types.Type) (string, error) {
		converted, err := f.convert(value, from, p.typ)
		if err != nil {
			return "", fmt.Errorf("%s: %w", af.apiName, err)
		}
		return fmt.Sprintf("%s.Value, %s.Present = %s, true", paramExpr(p), paramExpr(p), converted), nil
	}
	if af.goName == "" {
		return set(strconv.Quote(af.fixed), types.Typ[types.String])
	}
	spec := "obj.Spec." + af.goName
	if af.ref != nil {
		spec = refValue(af)
	}
	switch {
	case !af.optional || always:
		return set(spec, specType(af.field))
	case strings.HasPrefix(af.goType, "*") && af.ref == nil:
		s, err := set("*"+spec, specType(af.field))
		return fmt.Sprintf("if %s != nil {\n%s\n}", spec, s), err
	default:
		s, err := set(spec, specType(af.field))
		return fmt.Sprintf("if %s != \"\" {\n%s\n}", spec, s), err
	}
}

// paramExpr is the expression of the param p in a call's statements: a field of
// the params, or of the union body's variant, which they fill in first.
func paramExpr(p *sdkField) string {
	if p.inUnion {
		return "body." + p.goName
	}
	return "params." + p.goName
}

// refValue is the expression of the value the reference af gives, in refs.
func refValue(af apiField) string {
	return fmt.Sprintf("refs[%q]", af.jsonName)
}

// objParam names a call's resource parameter: obj when its statements read
// the spec, _ when they do not.
func objParam(statements []string) string {
	if len(statements) == 0 {
		return "_"
	}
	return "obj"
}

// matchesExpr returns the expression that reports whether the object current
// is what obj's spec asks for: every field Update can change matches, or with
// all set, every field the object reports. A field the spec leaves unset is
// not compared, Cloudflare keeping its value, unless it is clearable. A reference is always compared
// with the value it gives, in refs: an unset one gives none. "" when no field
// is compared. It also reports whether the expression reads refs.
func matchesExpr(f *goFile, c *controller, all bool) (string, bool, error) {
	var terms []string
	usesRefs := false
	for _, af := range c.fields {
		if af.object == nil || (af.update == nil && !all) {
			continue
		}
		current, err := f.convert("current."+af.object.goName, af.object.typ, specType(af.field))
		if err != nil {
			return "", false, fmt.Errorf("comparing %s: %w", af.apiName, err)
		}
		spec := "obj.Spec." + af.goName
		switch {
		case af.ref != nil:
			terms = append(terms, fmt.Sprintf("%s == %s", current, refValue(af)))
			usesRefs = true
		case !af.optional || af.clearable:
			terms = append(terms, fmt.Sprintf("%s == %s", current, spec))
		case strings.HasPrefix(af.goType, "*"):
			terms = append(terms, fmt.Sprintf("(%s == nil || %s == *%s)", spec, current, spec))
		default:
			terms = append(terms, fmt.Sprintf("(%s == \"\" || %s == %s)", spec, current, spec))
		}
	}
	return strings.Join(terms, " &&\n"), usesRefs, nil
}

// rbacMarkers returns the RBAC markers of a kind's controller: its own
// resources, its Zone, its account and the account's token Secret, the kinds
// it references and those it waits for.
func rbacMarkers(c *controller) []string {
	group := "+kubebuilder:rbac:groups=kflare.dev,resources="
	out := []string{
		group + c.plural + ",verbs=get;list;watch;create;update;patch;delete",
		group + c.plural + "/status,verbs=get;update;patch",
		group + c.plural + "/finalizers,verbs=update",
	}
	if c.zoned {
		out = append(out, group+"zones,verbs=get;list;watch")
	}
	out = append(out, group+"cloudflareaccounts,verbs=get;list;watch")
	for _, af := range c.references {
		out = append(out, group+plural(af.ref.Kind)+",verbs=get;list;watch")
	}
	for _, d := range c.dependents {
		out = append(out, group+plural(d.Kind)+",verbs=get;list;watch")
	}
	return append(out, `+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch`)
}

// kindDoc documents a kind's descriptor: how its resources adopt, update and
// recreate their objects, and what they wait for.
func kindDoc(c *controller) string {
	var updatable, immutable []string
	for _, af := range c.fields {
		if af.immutable {
			immutable = append(immutable, af.jsonName)
		} else {
			updatable = append(updatable, af.jsonName)
		}
	}
	same := make([]string, 0, 1+len(c.address))
	same = append(same, c.key.jsonName)
	for _, af := range c.address {
		same = append(same, af.jsonName)
	}
	parent := "account"
	if c.zoned {
		parent = "zone"
	}
	var doc strings.Builder
	fmt.Fprintf(&doc, "%sKind describes %s to the shared CRUD reconciler. %s creates its %s or, as its "+
		"adoption policy allows, adopts the %s's %s with the same %s, unless another %s manages it.",
		c.varName, pluralOf(c.name), upperCamel(article(c.name)), c.noun, parent, c.noun, prose(same), c.name)
	if c.byKey {
		fmt.Fprintf(&doc, " The %s has no ID but its %s, by which it is read.", c.noun, c.key.jsonName)
	}
	if len(updatable) > 0 {
		fmt.Fprintf(&doc, " It changes %s in place.", prose(updatable))
	}
	if len(immutable) > 0 {
		verb := "is"
		if len(immutable) > 1 {
			verb = "are"
		}
		fmt.Fprintf(&doc, " %s %s set when the %s is created and cannot change.", upperCamel(prose(immutable)),
			verb, c.noun)
	}
	for _, af := range c.references {
		fmt.Fprintf(&doc, " It sends the %s of the %s its %s names as %s, once that %s is ready.",
			af.ref.Field, af.ref.Kind, af.jsonName, af.apiName, af.ref.Kind)
	}
	fmt.Fprintf(&doc, " %s deleted outside kflare is created again.", upperCamel(article(c.noun)))
	for _, b := range c.deletionBlockers {
		fmt.Fprintf(&doc, " While Cloudflare refuses to delete the %s with %d, the %s keeps its finalizer and "+
			"reports %s.", c.noun, b.Code, c.name, b.Reason)
	}
	for _, d := range c.dependents {
		fmt.Fprintf(&doc, " %s waits for the %s that use it before it is deleted: %s", upperCamel(article(c.name)),
			pluralOf(d.Kind), strings.TrimSpace(d.Reason))
	}
	return doc.String()
}

// emitControllerTest returns the Go source of a kind's envtest table: a fake
// of its cloudflare-go v7 service on fakeCRUDStore, and the sample and edits
// that describeCRUDKind runs the shared specs with
// (crudresource_specs_test.go).
func emitControllerTest(c *controller, header string) ([]byte, error) {
	f := newGoFile()
	name, v := c.name, c.varName
	api, fake := name+"API", "fake"+name+"API"
	object, listed := f.typ(c.object), f.typ(c.listed)
	obj := "*" + f.api() + "." + name
	if err := emitFake(f, c); err != nil {
		return nil, err
	}

	client := f.use(clientPackage, "client")
	k8stypes := f.use("k8s.io/apimachinery/pkg/types", "types")
	f.printf("var _ = describeCRUDKind(crudKindSpecs[%s, %s, %s, %s]{\n", obj, api, object, listed)
	f.printf("kind: %sKind,\nkeyField: %q,\n", v, c.key.jsonName)
	f.printf("claimLabel: %q,\nconflictReason: %q,\n", c.claimLabel, c.conflictReason)
	if c.list != nil && !c.singlePage {
		f.printf("pageSize: %sListPageSize,\n", v)
	}
	if c.scope != "" {
		f.printf("scope: %q,\n", c.scope)
	}
	f.printf("newReconciler: func(c %s.Client,\nnewAPI func(string) (%s, error)) crudKindReconciler[%s, %s, %s, %s] {\n",
		client, api, obj, api, object, listed)
	f.printf("return &%sReconciler{Client: c, Scheme: c.Scheme(), New%s: newAPI}\n},\n", name, api)
	if c.hooksAPI() {
		f.printf("newFake: func(store *fakeCRUDStore[%s]) %s {\nreturn %s{store, newFake%sHooksAPI(store)}\n},\n", object,
			api, fake, name)
		f.printf("defaultAPI: default%s,\nservice: &%sService{},\n", api, v)
	} else {
		f.printf("newFake: func(store *fakeCRUDStore[%s]) %s { return %s{store} },\n", object, api, fake)
		f.printf("defaultAPI: default%s,\nservice: &%s{},\n", api,
			f.typ(c.svc.pkg.Scope().Lookup(c.svc.typeName).Type()))
	}
	parentRef := accountRefName
	if c.zoned {
		parentRef = "ZoneRef"
	}
	f.printf("newResource: func(key %s.NamespacedName, parent string) %s {\n", k8stypes, obj)
	extras, err := tableExtras(c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	build := "return"
	if extras != "" {
		build = "resource :="
	}
	f.printf("%s &%s.%s{\nObjectMeta: %s.ObjectMeta{Name: key.Name, Namespace: key.Namespace},\n", build, f.api(), name,
		f.use("k8s.io/apimachinery/pkg/apis/meta/v1", "metav1"))
	f.printf("Spec: %s.%sSpec{\n%s: %s.LocalObjectReference{Name: parent},\n", f.api(), name, parentRef,
		f.use("k8s.io/api/core/v1", "corev1"))
	for _, af := range c.fields {
		if value, ok := c.sample[af.apiName]; ok {
			f.printf("%s: %s,\n", af.goName, goValue(f, af.field, value))
		}
	}
	f.printf("},\n}\n")
	if extras != "" {
		// The extras are hand-written types: their values go in as the sample's
		// JSON, which a resource decodes as the API server would.
		f.printf("if err := %s.Unmarshal([]byte(%q), &resource.Spec); err != nil {\npanic(err)\n}\nreturn resource\n",
			f.use("encoding/json", "json"), extras)
	}
	f.printf("},\n")
	if err := emitSeed(f, c, object); err != nil {
		return nil, err
	}
	created, usesRefs, err := matchesExpr(f, c, true)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if created == "" {
		created = "true"
	}
	f.printf("created: func(obj %s, %s map[string]string, current *%s) bool {\nreturn %s\n},\n", obj,
		refsParam(usesRefs), object, created)
	if err := emitTableChecks(f, c, obj, object); err != nil {
		return nil, err
	}

	edits := func(label string, edits []specEdit) {
		if len(edits) == 0 {
			return
		}
		f.printf("%s: []crudSpecEdit[%s]{\n", label, obj)
		for _, e := range edits {
			f.printf("{field: %q, what: %q,\nedit: func(obj %s) { obj.Spec.%s = %s }},\n", e.f.jsonName, e.what, obj,
				e.f.goName, goValue(f, e.f, e.value))
		}
		f.printf("},\n")
	}
	edits("changes", c.changes)
	f.printf("immutable: []crudSpecEdit[%s]{\n", obj)
	parentField, parentName := accountRefField, "other-account"
	if c.zoned {
		parentField, parentName = zoneRefField, "other-zone"
	}
	f.printf("{field: %q, what: %q,\nedit: func(obj %s) { obj.Spec.%s.Name = %q }},\n", parentField,
		"changing "+parentField, obj, parentRef, parentName)
	for _, e := range c.immutable {
		f.printf("{field: %q, what: %q,\n", e.f.jsonName, e.what)
		if e.setup != nil {
			f.printf("setup: func(obj %s) { obj.Spec.%s = %s },\n", obj, e.f.goName, goValue(f, e.f, e.setup))
		}
		f.printf("edit: func(obj %s) { obj.Spec.%s = %s }},\n", obj, e.f.goName, goValue(f, e.f, e.value))
	}
	f.printf("},\n")
	edits("invalid", c.invalid)
	if e := c.elsewhere; e != nil {
		f.printf("elsewhere: &crudScopeEdit[%s]{\ncrudSpecEdit: crudSpecEdit[%s]{field: %q, what: %q,\n"+
			"edit: func(obj %s) { obj.Spec.%s = %s }},\nscope: %q,\n},\n", obj, obj, e.f.jsonName, e.what, obj,
			e.f.goName, goValue(f, e.f, e.value), c.scopeElsewhere)
	}
	emitReferenceSpecs(f, c, obj)
	if len(c.deletionBlockers) > 0 {
		// The table's own copy: the specs check the descriptor's against it.
		f.printf("deletionBlockers: []deletionBlocker{\n")
		for _, b := range c.deletionBlockers {
			f.printf("{code: %d, reason: %q,\nmessage: %q},\n", b.Code, b.Reason, b.Message)
		}
		f.printf("},\n")
	}
	f.printf("})\n")
	return f.source(header)
}

// tableExtras returns the spec extras of the resources in the kind's table as
// JSON: "" when they have none.
func tableExtras(c *controller) (string, error) {
	if c.hooks == nil {
		return "", nil
	}
	extras := c.hooks.TableExtras
	if extras == nil {
		extras = c.hooks.SampleExtras
	}
	if len(extras) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(extras)
	if err != nil {
		return "", fmt.Errorf("the table's extras: %w", err)
	}
	return string(encoded), nil
}

// emitSeed writes the table's seed: an object with an ID and a key, as
// Cloudflare holds it for the sample, with the values Cloudflare defaults and
// what the sample's references give.
func emitSeed(f *goFile, c *controller, object string) error {
	name := c.name
	keyField, err := f.convert("key", types.Typ[types.String], c.key.object.typ)
	if err != nil {
		return fmt.Errorf("%s: the fake's objects: %w", name, err)
	}
	// A seeded object holds what Cloudflare holds for the sample: the sample's
	// values, the defaults of the fields it leaves unset, the fixed values and
	// what its references give.
	seedFields := []string{fmt.Sprintf("%s: %s", c.key.object.goName, keyField)}
	idParam := "_"
	if !c.byKey {
		seedFields = append([]string{fmt.Sprintf("%s: id", c.id.goName)}, seedFields...)
		idParam = "id"
	}
	refs := "_"
	for _, af := range c.fields {
		if af.object == nil || af.object.goName == c.key.object.goName {
			continue
		}
		var value string
		var from types.Type
		sample, set := c.sample[af.apiName]
		switch {
		case af.ref != nil:
			value, from, refs = refValue(af), types.Typ[types.String], "refs"
		case set:
			value, from = goValue(f, field{goType: strings.TrimPrefix(af.goType, "*")}, sample), specType(af.field)
		case af.def != "":
			def, err := defaultValue(af.field)
			if err != nil {
				return fmt.Errorf("%s: %s's default: %w", name, af.apiName, err)
			}
			value, from = goValue(f, field{goType: strings.TrimPrefix(af.goType, "*")}, def), specType(af.field)
		default:
			continue
		}
		converted, err := f.convert(value, from, af.object.typ)
		if err != nil {
			return fmt.Errorf("%s: the fake's objects: %s: %w", name, af.apiName, err)
		}
		seedFields = append(seedFields, fmt.Sprintf("%s: %s", af.object.goName, converted))
	}
	for _, af := range c.fixedFields {
		if af.object == nil {
			continue
		}
		converted, err := f.convert(strconv.Quote(af.fixed), types.Typ[types.String], af.object.typ)
		if err != nil {
			return fmt.Errorf("%s: the fake's objects: %s: %w", name, af.apiName, err)
		}
		seedFields = append(seedFields, fmt.Sprintf("%s: %s", af.object.goName, converted))
	}
	switch {
	case c.hooks.params():
		// What Cloudflare holds beyond the entry's fields, which the hooks send.
		f.printf("seed: func(%s, key string, %s map[string]string) %s {\nobject := %s\nfake%sSeed(&object)\n"+
			"return object\n},\n", idParam, refs, object, compositeLiteral(object, seedFields), name)
	case len(seedFields) <= 2:
		f.printf("seed: func(%s, key string, %s map[string]string) %s { return %s },\n", idParam, refs, object,
			compositeLiteral(object, seedFields))
	default:
		f.printf("seed: func(%s, key string, %s map[string]string) %s {\nreturn %s\n},\n", idParam, refs, object,
			compositeLiteral(object, seedFields))
	}
	return nil
}

// emitReferenceSpecs writes the table's references: each with the resource
// the sample names, and how to point a resource at another one.
func emitReferenceSpecs(f *goFile, c *controller, obj string) {
	if len(c.references) > 0 {
		f.printf("references: []crudRefSpec[%s]{\n", obj)
		for _, af := range c.references {
			name, _ := c.sample[af.apiName].(string)
			f.printf("{field: %q, kind: %q, name: %q, optional: %t,\n", af.jsonName, af.ref.Kind, name, af.optional)
			corev1 := f.use("k8s.io/api/core/v1", "corev1")
			if af.optional {
				f.printf("set: func(obj %s, name string) {\nobj.Spec.%s = nil\nif name != \"\" {\n"+
					"obj.Spec.%s = &%s.LocalObjectReference{Name: name}\n}\n}},\n", obj, af.goName, af.goName, corev1)
			} else {
				f.printf("set: func(obj %s, name string) { obj.Spec.%s = %s.LocalObjectReference{Name: name} }},\n",
					obj, af.goName, corev1)
			}
		}
		f.printf("},\n")
	}
}

// emitTableChecks writes the table's checks of what the reconciler sends and
// records: sent, that New's params hold every field of a resource's spec (a
// field the object does not report included), the values its references give
// and the fixed values; observed,
// that status records the object; and defaulted, when the spec has defaults,
// that the API server applied them.
func emitTableChecks(f *goFile, c *controller, obj, object string) error {
	var sent []string
	usesRefs := false
	for _, af := range append(slices.Clone(c.fields), c.fixedFields...) {
		if af.create == nil {
			continue
		}
		p := paramExpr(af.create)
		if af.goName == "" {
			value, err := f.convert(p+".Value", af.create.typ, types.Typ[types.String])
			if err != nil {
				return fmt.Errorf("%s: sent: %s: %w", c.name, af.apiName, err)
			}
			sent = append(sent, fmt.Sprintf("%s == %q", value, af.fixed))
			continue
		}
		value, err := f.convert(p+".Value", af.create.typ, specType(af.field))
		if err != nil {
			return fmt.Errorf("%s: sent: %s: %w", c.name, af.apiName, err)
		}
		spec := "obj.Spec." + af.goName
		switch {
		case af.ref != nil:
			// An unset reference sends nothing: Cloudflare refuses an empty
			// value where it takes a name.
			sent = append(sent, fmt.Sprintf("%s.Present == (%s != \"\") && %s == %s", p, refValue(af), value,
				refValue(af)))
			usesRefs = true
		case !af.optional:
			sent = append(sent, fmt.Sprintf("%s == %s", value, spec))
		case strings.HasPrefix(af.goType, "*"):
			sent = append(sent, fmt.Sprintf("(%s == nil || %s.Present && %s == *%s)", spec, p, value, spec))
		default:
			sent = append(sent, fmt.Sprintf("(%s == \"\" || %s.Present && %s == %s)", spec, p, value, spec))
		}
	}
	body := ""
	if b := c.create.body; b != nil {
		body = fmt.Sprintf("body := params.%s.(%s)\n", b.goName, f.typ(b.typ))
	}
	f.printf("sent: func(obj %s, %s map[string]string, sent any) bool {\nparams := sent.(%s)\n%sreturn %s\n},\n", obj,
		refsParam(usesRefs), f.typ(c.create.params), body, strings.Join(sent, " &&\n"))

	observed := make([]string, 0, len(c.status))
	for _, st := range c.status {
		value, err := f.convert("current."+st.object.goName, st.object.typ, specType(st.field))
		if err != nil {
			return fmt.Errorf("%s: observed: %s: %w", c.name, st.jsonName, err)
		}
		observed = append(observed, fmt.Sprintf("obj.Status.CloudflareMetadata.%s == %s", st.goName, value))
	}
	f.printf("observed: func(obj %s, current *%s) bool {\nreturn %s\n},\n", obj, object,
		strings.Join(observed, " &&\n"))

	var defaulted []string
	for _, af := range c.fields {
		if af.def == "" {
			continue
		}
		def, err := defaultValue(af.field)
		if err != nil {
			return fmt.Errorf("%s: %s's default: %w", c.name, af.apiName, err)
		}
		literal := goValue(f, field{goType: strings.TrimPrefix(af.goType, "*")}, def)
		if strings.HasPrefix(af.goType, "*") {
			defaulted = append(defaulted, fmt.Sprintf("(obj.Spec.%s != nil && *obj.Spec.%s == %s)", af.goName,
				af.goName, literal))
		} else {
			defaulted = append(defaulted, fmt.Sprintf("obj.Spec.%s == %s", af.goName, literal))
		}
	}
	if len(defaulted) > 0 {
		f.printf("defaulted: func(obj %s) bool {\nreturn %s\n},\n", obj, strings.Join(defaulted, " &&\n"))
	}
	return nil
}

// emitFake writes the fake of a kind's cloudflare-go v7 service that its
// envtest table runs on: each call converts its params into a call on
// fakeCRUDStore, at the address the params give.
func emitFake(f *goFile, c *controller) error {
	name := c.name
	api, fake := name+"API", "fake"+name+"API"
	object, listed := f.typ(c.object), f.typ(c.listed)
	ctx := f.use("context", "context")
	opts := f.typ(c.create.sig.Params().At(c.create.sig.Params().Len() - 1).Type().(*types.Slice).Elem())
	params := func(call *sdkCall) string { return f.typ(call.params) }
	parentID := func(call *sdkCall) string {
		return "params." + call.paramFields[slices.IndexFunc(call.paramFields, c.isParent)].goName + ".Value"
	}

	parent := "account"
	if c.zoned {
		parent = "zone"
	}
	doc := fmt.Sprintf("%s is an in-memory %s for %s's generated specs: it keeps one %s's %s in a "+
		"fakeCRUDStore", fake, c.svc.name, name, parent, pluralOf(c.noun))
	if len(c.address) > 0 {
		names := make([]string, 0, len(c.address))
		for _, af := range c.address {
			names = append(names, af.jsonName)
		}
		doc += fmt.Sprintf(", apart by their %s", prose(names))
	}
	comment(&f.Builder, "", wrap(doc+"."))
	hooksFake := ""
	if c.hooksAPI() {
		hooksFake = fmt.Sprintf("fake%sHooksAPI\n", name)
	}
	f.printf("type %s struct {\n*fakeCRUDStore[%s]\n%s}\n\nvar _ %s = %s{}\n\n", fake, object, hooksFake, api, fake)
	// scope writes the address a call's params give, as the fake keeps
	// objects apart by it.
	scope := func(call *sdkCall) (string, error) {
		if len(c.address) == 0 {
			return `""`, nil
		}
		p := addressParam(call, c.address[0])
		if p == nil {
			return "", fmt.Errorf("%s: %s takes no %s", name, call.name, c.address[0].apiName)
		}
		value, err := f.convert("params."+p.goName+".Value", p.typ, types.Typ[types.String])
		if err != nil {
			return "", fmt.Errorf("%s: the fake's %s: %w", name, call.name, err)
		}
		return value, nil
	}
	scopes := map[*sdkCall]string{}
	for _, call := range c.calls() {
		sc, err := scope(call)
		if err != nil {
			return err
		}
		scopes[call] = sc
	}

	fields, err := fakeNewFields(f, c)
	if err != nil {
		return err
	}
	// An object identified by its key is created under it; others get a new
	// ID.
	newID := `""`
	if c.byKey {
		var err error
		if newID, err = f.convert("params."+c.key.create.goName+".Value", c.key.create.typ,
			types.Typ[types.String]); err != nil {
			return fmt.Errorf("%s: the fake's New: %w", name, err)
		}
	}
	// A call that returns a type of its own returns the object converted.
	returned := func(call string, result types.Type) (string, string, error) {
		if result == nil {
			return "return " + call, object, nil
		}
		converted, err := convertObject(f, "stored", c.object, result, c.svc.object)
		if err != nil {
			return "", "", err
		}
		return fmt.Sprintf("stored, err := %s\nif err != nil {\nreturn nil, err\n}\nreturn &%s, nil", call, converted),
			f.typ(result), nil
	}
	build := "return " + compositeLiteral(object, fields)
	if c.hooks.params() {
		build = fmt.Sprintf("object := %s\nfake%sNewBody(&object, body)\nreturn object", compositeLiteral(object, fields),
			name)
	}
	ret, result, err := returned(fmt.Sprintf("f.create(%s, %s,\n%s, func(id string) %s {\n%s\n})",
		parentID(c.create), scopes[c.create], newID, object, build), c.created)
	if err != nil {
		return fmt.Errorf("%s: the fake's New: %w", name, err)
	}
	f.printf("func (f %s) New(_ %s.Context, params %s,\n_ ...%s) (*%s, error) {\n", fake, ctx, params(c.create), opts,
		result)
	f.printf("f.sent = append(f.sent, params)\n%s%s\n}\n\n", fakeBody(f, c.create), ret)

	f.printf("func (f %s) Get(_ %s.Context, id string, params %s,\n_ ...%s) (*%s, error) {\n", fake, ctx,
		params(c.get), opts, object)
	f.printf("return f.get(%s, %s, id)\n}\n\n", parentID(c.get), scopes[c.get])

	if c.list != nil && c.singlePage {
		asListed, err := listedObject(f, c)
		if err != nil {
			return fmt.Errorf("%s: the fake's List: %w", name, err)
		}
		f.printf("func (f %s) List(_ %s.Context, params %s,\n_ ...%s) (%s, error) {\n", fake, ctx, params(c.list),
			opts, f.typ(c.list.sig.Results().At(0).Type()))
		f.printf("return fakeListAll(f.fakeCRUDStore, %s, %s,\nfunc(current *%s) %s {\nreturn %s\n})\n}\n\n",
			parentID(c.list), scopes[c.list], object, listed, asListed)
	} else if c.list != nil {
		page, err := f.convert("params."+c.page.goName+".Value", c.page.typ, types.Typ[types.Int64])
		if err != nil {
			return fmt.Errorf("%s: the fake's List: %w", name, err)
		}
		perPage, err := f.convert("params."+c.perPage.goName+".Value", c.perPage.typ, types.Typ[types.Int64])
		if err != nil {
			return fmt.Errorf("%s: the fake's List: %w", name, err)
		}
		asListed, err := listedObject(f, c)
		if err != nil {
			return fmt.Errorf("%s: the fake's List: %w", name, err)
		}
		f.printf("func (f %s) List(_ %s.Context, params %s,\n_ ...%s) (%s, error) {\n", fake, ctx, params(c.list),
			opts, f.typ(c.list.sig.Results().At(0).Type()))
		f.printf("return fakeList(f.fakeCRUDStore, %s, %s,\n%s, %s,\nfunc(current *%s) %s {\nreturn %s\n})\n}\n\n",
			parentID(c.list), scopes[c.list], page, perPage, object, listed, asListed)
	}

	if c.update != nil {
		if err := emitFakeUpdate(f, c, parentID(c.update), scopes[c.update], returned); err != nil {
			return err
		}
	}

	f.printf("func (f %s) Delete(_ %s.Context, id string, params %s,\n_ ...%s) (*%s, error) {\n", fake, ctx,
		params(c.remove), opts, f.typ(c.deleteResult))
	f.printf("if err := f.delete(%s, %s, id); err != nil {\nreturn nil, err\n}\nreturn new(%s), nil\n}\n\n",
		parentID(c.remove), scopes[c.remove], f.typ(c.deleteResult))
	return nil
}

// fakeBody returns the statement that reads call's union body in the fake: ""
// for a call without one.
func fakeBody(f *goFile, call *sdkCall) string {
	if call.body == nil {
		return ""
	}
	return fmt.Sprintf("body := params.%s.(%s)\n", call.body.goName, f.typ(call.body.typ))
}

// fakeNewFields returns the fields of the object a kind's fake creates for
// New's params: the ID, what the params set, and a value of the new object's
// own for each field status records that Cloudflare fills in.
func fakeNewFields(f *goFile, c *controller) ([]string, error) {
	name := c.name
	var fields []string
	fields = append(fields, fmt.Sprintf("%s: id", c.id.goName))
	sent := map[string]bool{}
	for _, af := range append(slices.Clone(c.fields), c.fixedFields...) {
		if af.create == nil || af.object == nil || af.object.goName == c.id.goName {
			continue
		}
		value, err := f.convert(paramExpr(af.create)+".Value", af.create.typ, af.object.typ)
		if err != nil {
			return nil, fmt.Errorf("%s: the fake's New: %s: %w", name, af.apiName, err)
		}
		fields = append(fields, fmt.Sprintf("%s: %s", af.object.goName, value))
		sent[af.object.goName] = true
	}
	// What Cloudflare fills in, which status records: a value of the new
	// object's own. Status also records what a reference sends, which New's
	// params hold.
	for _, st := range c.status[1:] {
		if sent[st.object.goName] {
			continue
		}
		var made string
		var from types.Type
		switch basicClass(st.object.typ) {
		case types.IsString:
			made, from = fmt.Sprintf("%q + id", st.jsonName+"-"), types.Typ[types.String]
		case types.IsBoolean:
			made, from = strconv.FormatBool(true), types.Typ[types.Bool]
		default:
			made, from = "1", types.Typ[types.Int64]
		}
		value, err := f.convert(made, from, st.object.typ)
		if err != nil {
			return nil, fmt.Errorf("%s: the fake's New: status.cloudflareMetadata.%s: %w", name, st.jsonName, err)
		}
		fields = append(fields, fmt.Sprintf("%s: %s", st.object.goName, value))
	}
	return fields, nil
}

// emitFakeUpdate writes the fake's Update (or Edit): it applies the params to
// the object id, as parentID and scope name it, and returns what returned
// makes of the stored object.
func emitFakeUpdate(f *goFile, c *controller, parentID, scope string,
	returned func(call string, result types.Type) (string, string, error)) error {
	name, fake := c.name, "fake"+c.name+"API"
	object, ctx := f.typ(c.object), f.use("context", "context")
	opts := f.typ(c.create.sig.Params().At(c.create.sig.Params().Len() - 1).Type().(*types.Slice).Elem())
	// A PUT replaces the object: a field it is not sent is cleared, as
	// Cloudflare clears a Worker route's script. A PATCH changes the
	// fields it is sent.
	replaces := strings.EqualFold(c.svc.updateRoute.verb, "put")
	var apply strings.Builder
	for _, af := range append(slices.Clone(c.fields), c.fixedFields...) {
		if af.update == nil || af.object == nil {
			continue
		}
		value, err := f.convert(paramExpr(af.update)+".Value", af.update.typ, af.object.typ)
		if err != nil {
			return fmt.Errorf("%s: the fake's %s: %s: %w", name, c.update.name, af.apiName, err)
		}
		if replaces {
			fmt.Fprintf(&apply, "current.%s = %s\n", af.object.goName, value)
		} else {
			fmt.Fprintf(&apply, "if %s.Present {\ncurrent.%s = %s\n}\n", paramExpr(af.update), af.object.goName,
				value)
		}
	}
	if c.hooks.params() {
		fmt.Fprintf(&apply, "fake%s%sBody(current, body)\n", name, c.update.name)
	}
	ret, result, err := returned(fmt.Sprintf("f.update(%s, %s, id, func(current *%s) {\n%s})", parentID,
		scope, object, apply.String()), c.updated)
	if err != nil {
		return fmt.Errorf("%s: the fake's %s: %w", name, c.update.name, err)
	}
	f.printf("func (f %s) %s(_ %s.Context, id string, params %s,\n_ ...%s) (*%s, error) {\n", fake,
		c.update.name, ctx, f.typ(c.update.params), opts, result)
	f.printf("%s%s\n}\n\n", fakeBody(f, c.update), ret)
	return nil
}

// listedObject returns the expression that shows the object current as a
// page of the list shows it: the object itself when the list holds Get's
// type, else the fields the two share, by API name. The ID and the adoption
// key are always among them.
func listedObject(f *goFile, c *controller) (string, error) {
	if types.Identical(c.object, c.listed) {
		return "*current", nil
	}
	var fields []string
	for _, l := range c.svc.listed {
		i := slices.IndexFunc(c.svc.object, func(o sdkField) bool { return o.apiName == l.apiName })
		if i < 0 {
			continue
		}
		value, err := f.convert("current."+c.svc.object[i].goName, c.svc.object[i].typ, l.typ)
		if err != nil {
			if l.apiName == c.listedID.apiName || l.apiName == c.listedKey.apiName {
				return "", fmt.Errorf("%s: %w", l.apiName, err)
			}
			// A field the fake cannot copy, such as a nested object, stays
			// empty: the reconciler reads only the ID and the key of a
			// listed object.
			continue
		}
		fields = append(fields, fmt.Sprintf("%s: %s", l.goName, value))
	}
	return compositeLiteral(f.typ(c.listed), fields), nil
}

// compositeLiteral writes a struct literal of typ, one field to a line when
// it has more than two.
func compositeLiteral(typ string, fields []string) string {
	if len(fields) <= 2 {
		return typ + "{" + strings.Join(fields, ", ") + "}"
	}
	return typ + "{\n" + strings.Join(fields, ",\n") + ",\n}"
}

// goValue writes a value of the spec field f as Go source: a pointer for a
// pointer field, a long run of one character with strings.Repeat, and a
// reference to the resource the value names.
func goValue(f *goFile, sf field, value any) string {
	if sf.ref != nil {
		// A reference's value is the referenced resource's name.
		ref := fmt.Sprintf("%s.LocalObjectReference{Name: %q}", f.use("k8s.io/api/core/v1", "corev1"), value)
		if sf.optional {
			return "&" + ref
		}
		return ref
	}
	var literal string
	switch value := value.(type) {
	case string:
		literal = strconv.Quote(value)
		if runes := []rune(value); len(runes) > 20 && strings.Count(value, string(runes[0])) == len(runes) {
			literal = fmt.Sprintf("%s.Repeat(%q, %d)", f.use("strings", "strings"), string(runes[0]), len(runes))
		}
	case bool:
		literal = strconv.FormatBool(value)
	case int64:
		literal = strconv.FormatInt(value, 10)
	}
	if strings.HasPrefix(sf.goType, "*") {
		ptr := f.use("k8s.io/utils/ptr", "ptr")
		if sf.goType == "*"+typeInt64 {
			return fmt.Sprintf("%s.To[int64](%s)", ptr, literal)
		}
		return fmt.Sprintf("%s.To(%s)", ptr, literal)
	}
	return literal
}

// emitKinds returns the Go source of what registers the generated kinds: the
// function cmd/main.go sets their reconcilers up with, the CRUD kinds'
// entries as dependents of their account, the rule kinds, which the shared
// rule controller lists, and the rule and settings kinds as dependents of
// their Zone.
func emitKinds(kinds []*controller, ruleKinds []*ruleKind, settingsKinds []*settingsKind, hookedKinds []*hookedKind,
	header string) ([]byte, error) {
	f := newGoFile()
	ctrl := f.use("sigs.k8s.io/controller-runtime", "ctrl")
	client := f.use(clientPackage, "client")
	comment(&f.Builder, "", wrap("SetupGeneratedReconcilers registers the reconciler of every kind kflare-gen "+
		"generates with mgr."))
	f.printf("func SetupGeneratedReconcilers(mgr %s.Manager) error {\n", ctrl)
	names := make([]string, 0, len(kinds)+len(ruleKinds)+len(settingsKinds))
	for _, c := range kinds {
		names = append(names, c.name)
	}
	for _, k := range ruleKinds {
		names = append(names, k.name)
	}
	for _, k := range settingsKinds {
		names = append(names, k.name)
	}
	for _, k := range hookedKinds {
		names = append(names, k.name)
	}
	for _, name := range slices.Sorted(slices.Values(names)) {
		f.printf("if err := (&%sReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr); "+
			"err != nil {\nreturn %s.Errorf(\"setting up the %s controller: %%w\", err)\n}\n", name,
			f.use("fmt", "fmt"), name)
	}
	f.printf("return nil\n}\n\n")

	comment(&f.Builder, "", wrap("generatedAccountDependents are the generated kinds of an account that the shared "+
		"CRUD reconciler drives, whose resources use a CloudflareAccount's credentials directly."))
	f.printf("var generatedAccountDependents = []dependentKind{\n")
	var accountKinds []string
	for _, c := range kinds {
		if !c.zoned {
			accountKinds = append(accountKinds, c.name)
		}
	}
	for _, k := range hookedKinds {
		if k.usesAccount {
			accountKinds = append(accountKinds, k.name)
		}
	}
	for _, name := range slices.Sorted(slices.Values(accountKinds)) {
		f.printf("dependents(%q, func() %s.ObjectList { return &%s.%sList{} },\n", name, client, f.api(), name)
		f.printf("func(obj *%s.%s) []string { return []string{obj.Spec.AccountRef.Name} }),\n", f.api(), name)
	}
	f.printf("}\n\n")

	comment(&f.Builder, "", wrap("generatedRulesetRuleKinds are the generated rule kinds, which the shared rule "+
		"controller drives."))
	f.printf("var generatedRulesetRuleKinds = []rulesetRuleKind{")
	for i, k := range ruleKinds {
		if i > 0 {
			f.printf(", ")
		}
		f.printf("%sKind", k.varName)
	}
	f.printf("}\n\n")
	comment(&f.Builder, "", wrap("generatedZoneDependents are the generated rule and settings kinds, and the CRUD "+
		"kinds of a zone, whose resources reference a Zone in their namespace."))
	f.printf("var generatedZoneDependents = []dependentKind{\n")
	zoneKinds := make([]string, 0, len(ruleKinds)+len(settingsKinds))
	for _, c := range kinds {
		if c.zoned {
			zoneKinds = append(zoneKinds, c.name)
		}
	}
	for _, k := range ruleKinds {
		zoneKinds = append(zoneKinds, k.name)
	}
	for _, k := range settingsKinds {
		zoneKinds = append(zoneKinds, k.name)
	}
	for _, name := range slices.Sorted(slices.Values(zoneKinds)) {
		f.printf("dependents(%q, func() %s.ObjectList { return &%s.%sList{} },\n", name, client, f.api(), name)
		f.printf("func(obj *%s.%s) []string { return []string{obj.Spec.ZoneRef.Name} }),\n", f.api(), name)
	}
	f.printf("}\n")
	return f.source(header)
}

// emitSample returns a kind's sample resource, for config/samples.
func emitSample(c *controller) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by kflare-gen from generator/generator.yaml. DO NOT EDIT.\n#\n")
	for _, line := range c.doc {
		yamlComment(&b, "", line)
	}
	fmt.Fprintf(&b, "apiVersion: kflare.dev/v1alpha1\nkind: %s\nmetadata:\n  name: example-%s\n  namespace: default\n",
		c.name, strings.ToLower(c.name))
	fmt.Fprintf(&b, "  annotations:\n")
	for _, line := range wrapAt(fmt.Sprintf("The retain policy keeps the %s in Cloudflare when this resource is "+
		"deleted. Without it, deleting the resource deletes the %s.", c.noun, c.noun), commentWidth-len("    # ")) {
		yamlComment(&b, "    ", line)
	}
	fmt.Fprintf(&b, "    kflare.dev/deletion-policy: retain\nspec:\n")
	if c.zoned {
		fmt.Fprintf(&b, "  zoneRef:\n    name: example-zone\n")
	} else {
		fmt.Fprintf(&b, "  accountRef:\n    name: my-account\n")
	}
	for _, af := range c.fields {
		value, ok := c.sample[af.apiName]
		if !ok {
			continue
		}
		for _, line := range af.doc {
			yamlComment(&b, "  ", line)
		}
		if af.ref != nil {
			fmt.Fprintf(&b, "  %s:\n    name: %s\n", af.jsonName, value)
			continue
		}
		encoded, err := yaml.Marshal(value)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "  %s: %s", af.jsonName, encoded)
	}
	if c.hooks != nil && len(c.hooks.SampleExtras) > 0 {
		encoded, err := yaml.Marshal(c.hooks.SampleExtras)
		if err != nil {
			return nil, err
		}
		for line := range strings.Lines(string(encoded)) {
			b.WriteString("  " + line)
		}
	}
	return []byte(b.String()), nil
}

// yamlComment writes line as a YAML comment after indent.
func yamlComment(b *strings.Builder, indent, line string) {
	if line == "" {
		b.WriteString(indent + "#\n")
		return
	}
	b.WriteString(indent + "# " + line + "\n")
}
