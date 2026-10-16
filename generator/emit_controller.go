/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
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

	comment(&f.Builder, "", wrap(fmt.Sprintf("%sListPageSize is how many %s a page of the list asks for: the most "+
		"the OpenAPI spec allows.", v, pluralOf(c.noun))))
	f.printf("const %sListPageSize = %d\n\n", v, c.pageSize)

	comment(&f.Builder, "", wrap(fmt.Sprintf("%s is the part of cloudflare-go v7's %s that the %s reconciler calls. "+
		"Tests inject a fake; in production it is a *%s.", api, service, name, service)))
	f.printf("type %s interface {\n", api)
	for _, call := range c.calls() {
		f.printf("\t%s\n", interfaceMethod(f, call))
	}
	f.printf("}\n\nvar _ %s = (*%s)(nil)\n\n", api, service)

	comment(&f.Builder, "", wrap(fmt.Sprintf("%sReconciler reconciles a %s object, with the shared CRUD reconciler "+
		"(crudresource_controller.go).", name, name)))
	f.printf("type %sReconciler struct {\n\t%s.Client\n\tScheme *%s.Scheme\n\n", name, client,
		f.use("k8s.io/apimachinery/pkg/runtime", "runtime"))
	comment(&f.Builder, "\t", wrapIndented(fmt.Sprintf("New%s constructs a %s from a raw API token. Defaults to "+
		"default%s; tests inject a fake.", api, api, api)))
	f.printf("\tNew%s func(token string) (%s, error)\n}\n\n", api, api)

	for _, marker := range rbacMarkers(c) {
		f.printf("// %s\n", marker)
	}
	f.printf("\n")

	comment(&f.Builder, "", wrap(kindDoc(c)))
	f.printf("var %sKind = crudResourceKind[%s, %s, %s, %s]{\n", v, obj, api, object, listed)
	f.printf("name: %q,\nnoun: %q,\n", name, c.noun)
	f.printf("newObject: func() %s { return &%s.%s{} },\n", obj, f.api(), name)
	f.printf("newList: func() %s.ObjectList { return &%s.%sList{} },\n\n", client, f.api(), name)
	f.printf("accountRef: func(obj %s) string { return obj.Spec.AccountRef.Name },\n", obj)
	f.printf("key: func(obj %s) string { return obj.Spec.%s },\n", obj, c.key.goName)
	f.printf("claimLabel: %s.%sIDLabel,\nconflictReason: %q,\n", f.api(), name, c.conflictReason)
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
	matches, err := matchesExpr(f, c, false)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if matches == "" {
		f.printf("matches: func(%s, *%s) bool { return true },\n", obj, object)
	} else {
		f.printf("matches: func(obj %s, current *%s) bool {\nreturn %s\n},\n", obj, object, matches)
	}
	listedID, err := f.convert("listed."+c.listedID.goName, c.listedID.typ, types.Typ[types.String])
	if err != nil {
		return nil, fmt.Errorf("%s: the listed ID: %w", name, err)
	}
	listedKey, err := f.convert("listed."+c.listedKey.goName, c.listedKey.typ, types.Typ[types.String])
	if err != nil {
		return nil, fmt.Errorf("%s: adopt.by: %w", name, err)
	}
	f.printf("listedID: func(listed *%s) string { return %s },\n", listed, listedID)
	f.printf("listedKey: func(listed *%s) string { return %s },\n\n", listed, listedKey)

	f.printf("get: func(ctx %s.Context, api %s, accountID, id string) (*%s, error) {\n", ctx, api, object)
	if err := callBody(f, c, c.get, nil, "return api.Get(ctx, id, params)"); err != nil {
		return nil, err
	}
	f.printf("},\nlist: func(ctx %s.Context, api %s, accountID string,\npage int64) (%s, error) {\n", ctx, api,
		f.typ(c.list.sig.Results().At(0).Type()))
	page, err := f.convert("page", types.Typ[types.Int64], c.page.typ)
	if err != nil {
		return nil, fmt.Errorf("%s: List's page: %w", name, err)
	}
	listParams := []string{
		fmt.Sprintf("params.%s.Value, params.%s.Present = %s, true", c.page.goName, c.page.goName, page),
		fmt.Sprintf("params.%s.Value, params.%s.Present = %sListPageSize, true", c.perPage.goName, c.perPage.goName,
			v),
	}
	if err := callBody(f, c, c.list, listParams, "return api.List(ctx, params)"); err != nil {
		return nil, err
	}
	f.printf("},\ncreate: func(ctx %s.Context, api %s, accountID string,\nobj %s) (*%s, error) {\n", ctx, api, obj,
		object)
	createParams, err := paramStatements(f, c, func(af apiField) *sdkField { return af.create })
	if err != nil {
		return nil, fmt.Errorf("%s: New: %w", name, err)
	}
	if err := callBody(f, c, c.create, createParams, "return api.New(ctx, params)"); err != nil {
		return nil, err
	}
	f.printf("},\n")
	if c.update != nil {
		f.printf("update: func(ctx %s.Context, api %s, accountID, id string,\nobj %s) (*%s, error) {\n", ctx, api,
			obj, object)
		updateParams, err := paramStatements(f, c, func(af apiField) *sdkField { return af.update })
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", name, c.update.name, err)
		}
		if err := callBody(f, c, c.update, updateParams, fmt.Sprintf("return api.%s(ctx, id, params)",
			c.update.name)); err != nil {
			return nil, err
		}
		f.printf("},\n")
	}
	f.printf("delete: func(ctx %s.Context, api %s, accountID, id string) error {\n", ctx, api)
	if err := callBody(f, c, c.remove, nil, "_, err := api.Delete(ctx, id, params)\nreturn err"); err != nil {
		return nil, err
	}
	f.printf("},\n")
	if len(c.dependents) > 0 {
		f.printf("\ndependents: %sDependents,\n", v)
	}
	f.printf("}\n\n")

	if len(c.dependents) > 0 {
		comment(&f.Builder, "", wrap(fmt.Sprintf("%sDependents use a %s, which waits for them before it is deleted.",
			v, name)))
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
	setupDoc := fmt.Sprintf("SetupWithManager registers %sReconciler with the manager. It watches CloudflareAccounts, "+
		"so a %s waiting for its account is reconciled once the account is ready", name, name)
	if len(c.dependents) > 0 {
		var kinds []string
		for _, d := range c.dependents {
			kinds = append(kinds, pluralOf(d.Kind))
		}
		setupDoc += fmt.Sprintf(", and deleted %s, so a %s waiting for them is released", prose(kinds), name)
	}
	comment(&f.Builder, "", wrap(setupDoc+"."))
	f.printf("func (r *%sReconciler) SetupWithManager(mgr %s.Manager) error {\nreturn r.crud().setup(mgr, r)\n}\n\n",
		name, ctrl)

	crud := fmt.Sprintf("crudResourceReconciler[%s, %s, %s, %s]", obj, api, object, listed)
	comment(&f.Builder, "", wrap(fmt.Sprintf("crud returns the shared CRUD reconciler for %s, with the injected "+
		"factory or the production one.", pluralOf(name))))
	f.printf("func (r *%sReconciler) crud() *%s {\n", name, crud)
	f.printf("newAPI := r.New%s\nif newAPI == nil {\nnewAPI = default%s\n}\n", api, api)
	f.printf("return &%s{\nClient: r.Client, kind: %sKind, newAPI: newAPI,\n}\n}\n\n", crud, v)

	comment(&f.Builder, "", wrap(fmt.Sprintf("default%s is the production factory: cloudflare-go v7's %s, built "+
		"from cfpkg.New's options for the token.", api, service)))
	f.printf("func default%s(token string) (%s, error) {\n", api, api)
	f.printf("client, err := %s.New(token)\nif err != nil {\nreturn nil, err\n}\n", f.use(cfPackage, "cfpkg"))
	f.printf("return %s.New%s(client.V7Options()...), nil\n}\n", svcPkg, c.svc.typeName)
	return f.source(header)
}

// interfaceMethod declares call as the narrow interface does, with the SDK's
// parameter names.
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
	result := f.typ(call.sig.Results().At(0).Type())
	joined := strings.Join(args, ", ")
	// The last parameter goes on a line of its own when the declaration
	// would be long.
	if len(call.name)+len(joined)+len(result) > 100 {
		joined = strings.Join(args[:len(args)-1], ", ") + ",\n" + args[len(args)-1]
	}
	return fmt.Sprintf("%s(%s) (%s, error)", call.name, joined, result)
}

// callBody writes the body of a descriptor's call: its params, with the
// account and the statements given, then the call.
func callBody(f *goFile, c *controller, call *sdkCall, statements []string, ret string) error {
	i := slices.IndexFunc(call.paramFields, isAccountID)
	account, err := f.convert("accountID", types.Typ[types.String], call.paramFields[i].typ)
	if err != nil {
		return fmt.Errorf("%s: %s's account_id: %w", c.name, call.name, err)
	}
	f.printf("var params %s\n", f.typ(call.params))
	f.printf("params.%s.Value, params.%s.Present = %s, true\n", call.paramFields[i].goName,
		call.paramFields[i].goName, account)
	for _, s := range statements {
		f.printf("%s\n", s)
	}
	f.printf("%s\n", ret)
	return nil
}

// paramStatements returns the statements that set a call's params from the
// spec and the fixed values; param picks each field's param in the call.
func paramStatements(f *goFile, c *controller, param func(apiField) *sdkField) ([]string, error) {
	var out []string
	for _, af := range append(slices.Clone(c.fields), c.fixedFields...) {
		p := param(af)
		if p == nil {
			continue
		}
		set := func(value string, from types.Type) (string, error) {
			converted, err := f.convert(value, from, p.typ)
			if err != nil {
				return "", fmt.Errorf("%s: %w", af.apiName, err)
			}
			return fmt.Sprintf("params.%s.Value, params.%s.Present = %s, true", p.goName, p.goName, converted), nil
		}
		if af.goName == "" {
			s, err := set(strconv.Quote(af.fixed), types.Typ[types.String])
			if err != nil {
				return nil, err
			}
			out = append(out, s)
			continue
		}
		spec := "obj.Spec." + af.goName
		switch {
		case !af.optional:
			s, err := set(spec, specType(af.field))
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		case strings.HasPrefix(af.goType, "*"):
			s, err := set("*"+spec, specType(af.field))
			if err != nil {
				return nil, err
			}
			out = append(out, fmt.Sprintf("if %s != nil {\n%s\n}", spec, s))
		default:
			s, err := set(spec, specType(af.field))
			if err != nil {
				return nil, err
			}
			out = append(out, fmt.Sprintf("if %s != \"\" {\n%s\n}", spec, s))
		}
	}
	return out, nil
}

// matchesExpr returns the expression that reports whether the object current
// is what obj's spec asks for: every field Update can change matches, or with
// all set, every field the object reports. A field the spec leaves unset is
// not compared: Cloudflare keeps its value. "" when no field is compared.
func matchesExpr(f *goFile, c *controller, all bool) (string, error) {
	var terms []string
	for _, af := range c.fields {
		if af.object == nil || (af.update == nil && !all) {
			continue
		}
		current, err := f.convert("current."+af.object.goName, af.object.typ, specType(af.field))
		if err != nil {
			return "", fmt.Errorf("comparing %s: %w", af.apiName, err)
		}
		spec := "obj.Spec." + af.goName
		switch {
		case !af.optional:
			terms = append(terms, fmt.Sprintf("%s == %s", current, spec))
		case strings.HasPrefix(af.goType, "*"):
			terms = append(terms, fmt.Sprintf("(%s == nil || %s == *%s)", spec, current, spec))
		default:
			terms = append(terms, fmt.Sprintf("(%s == \"\" || %s == %s)", spec, current, spec))
		}
	}
	return strings.Join(terms, " &&\n"), nil
}

// rbacMarkers returns the RBAC markers of a kind's controller: its own
// resources, its account and the account's token Secret, and the kinds it
// waits for.
func rbacMarkers(c *controller) []string {
	group := "+kubebuilder:rbac:groups=kflare.dev,resources="
	out := []string{
		group + c.plural + ",verbs=get;list;watch;create;update;patch;delete",
		group + c.plural + "/status,verbs=get;update;patch",
		group + c.plural + "/finalizers,verbs=update",
		group + "cloudflareaccounts,verbs=get;list;watch",
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
	var doc strings.Builder
	fmt.Fprintf(&doc, "%sKind describes %s to the shared CRUD reconciler. A %s adopts the account's %s with "+
		"the same %s, unless another %s manages it, or else creates one.", c.varName, pluralOf(c.name), c.name,
		c.noun, c.key.jsonName, c.name)
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
	fmt.Fprintf(&doc, " A %s deleted outside kflare is created again.", c.noun)
	for _, d := range c.dependents {
		fmt.Fprintf(&doc, " A %s waits for the %s that use it before it is deleted: %s", c.name, pluralOf(d.Kind),
			strings.TrimSpace(d.Reason))
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
	ctx := f.use("context", "context")
	opts := f.typ(c.create.sig.Params().At(c.create.sig.Params().Len() - 1).Type().(*types.Slice).Elem())
	obj := "*" + f.api() + "." + name
	params := func(call *sdkCall) string { return f.typ(call.params) }
	account := func(call *sdkCall) string {
		return "params." + call.paramFields[slices.IndexFunc(call.paramFields, isAccountID)].goName + ".Value"
	}

	comment(&f.Builder, "", wrap(fmt.Sprintf("%s is an in-memory %s for %s's generated specs: it keeps one "+
		"account's %s in a fakeCRUDStore.", fake, c.svc.name, name, pluralOf(c.noun))))
	f.printf("type %s struct {\n*fakeCRUDStore[%s]\n}\n\nvar _ %s = %s{}\n\n", fake, object, api, fake)

	var fields []string
	fields = append(fields, fmt.Sprintf("%s: id", c.id.goName))
	for _, af := range append(slices.Clone(c.fields), c.fixedFields...) {
		if af.create == nil || af.object == nil || af.object.goName == c.id.goName {
			continue
		}
		value, err := f.convert("params."+af.create.goName+".Value", af.create.typ, af.object.typ)
		if err != nil {
			return nil, fmt.Errorf("%s: the fake's New: %s: %w", name, af.apiName, err)
		}
		fields = append(fields, fmt.Sprintf("%s: %s", af.object.goName, value))
	}
	f.printf("func (f %s) New(_ %s.Context, params %s,\n_ ...%s) (*%s, error) {\n", fake, ctx, params(c.create), opts,
		object)
	f.printf("return f.create(%s, func(id string) %s {\nreturn %s\n})\n}\n\n", account(c.create), object,
		compositeLiteral(object, fields))

	f.printf("func (f %s) Get(_ %s.Context, id string, params %s,\n_ ...%s) (*%s, error) {\n", fake, ctx,
		params(c.get), opts, object)
	f.printf("return f.get(%s, id)\n}\n\n", account(c.get))

	page, err := f.convert("params."+c.page.goName+".Value", c.page.typ, types.Typ[types.Int64])
	if err != nil {
		return nil, fmt.Errorf("%s: the fake's List: %w", name, err)
	}
	perPage, err := f.convert("params."+c.perPage.goName+".Value", c.perPage.typ, types.Typ[types.Int64])
	if err != nil {
		return nil, fmt.Errorf("%s: the fake's List: %w", name, err)
	}
	asListed, err := listedObject(f, c)
	if err != nil {
		return nil, fmt.Errorf("%s: the fake's List: %w", name, err)
	}
	f.printf("func (f %s) List(_ %s.Context, params %s,\n_ ...%s) (%s, error) {\n", fake, ctx, params(c.list), opts,
		f.typ(c.list.sig.Results().At(0).Type()))
	f.printf("return fakeList(f.fakeCRUDStore, %s, %s, %s,\nfunc(current *%s) %s {\nreturn %s\n})\n}\n\n",
		account(c.list), page, perPage, object, listed, asListed)

	if c.update != nil {
		f.printf("func (f %s) %s(_ %s.Context, id string, params %s,\n_ ...%s) (*%s, error) {\n", fake,
			c.update.name, ctx, params(c.update), opts, object)
		f.printf("return f.update(%s, id, func(current *%s) {\n", account(c.update), object)
		for _, af := range append(slices.Clone(c.fields), c.fixedFields...) {
			if af.update == nil || af.object == nil {
				continue
			}
			value, err := f.convert("params."+af.update.goName+".Value", af.update.typ, af.object.typ)
			if err != nil {
				return nil, fmt.Errorf("%s: the fake's %s: %s: %w", name, c.update.name, af.apiName, err)
			}
			f.printf("if params.%s.Present {\ncurrent.%s = %s\n}\n", af.update.goName, af.object.goName, value)
		}
		f.printf("})\n}\n\n")
	}

	f.printf("func (f %s) Delete(_ %s.Context, id string, params %s,\n_ ...%s) (*%s, error) {\n", fake, ctx,
		params(c.remove), opts, f.typ(c.deleteResult))
	f.printf("if err := f.delete(%s, id); err != nil {\nreturn nil, err\n}\nreturn new(%s), nil\n}\n\n",
		account(c.remove), f.typ(c.deleteResult))

	client := f.use(clientPackage, "client")
	k8stypes := f.use("k8s.io/apimachinery/pkg/types", "types")
	f.printf("var _ = describeCRUDKind(crudKindSpecs[%s, %s, %s, %s]{\n", obj, api, object, listed)
	f.printf("kind: %sKind,\nkeyField: %q,\npageSize: %sListPageSize,\n", v, c.key.jsonName, v)
	f.printf("newReconciler: func(c %s.Client,\nnewAPI func(string) (%s, error)) crudKindReconciler[%s, %s, %s, %s] {\n",
		client, api, obj, api, object, listed)
	f.printf("return &%sReconciler{Client: c, Scheme: c.Scheme(), New%s: newAPI}\n},\n", name, api)
	f.printf("newFake: func(store *fakeCRUDStore[%s]) %s { return %s{store} },\n", object, api, fake)
	f.printf("defaultAPI: default%s,\nservice: &%s{},\n", api, f.typ(c.svc.pkg.Scope().Lookup(c.svc.typeName).Type()))
	f.printf("newResource: func(key %s.NamespacedName, account string) %s {\n", k8stypes, obj)
	f.printf("return &%s.%s{\nObjectMeta: %s.ObjectMeta{Name: key.Name, Namespace: key.Namespace},\n", f.api(), name,
		f.use("k8s.io/apimachinery/pkg/apis/meta/v1", "metav1"))
	f.printf("Spec: %s.%sSpec{\nAccountRef: %s.LocalObjectReference{Name: account},\n", f.api(), name,
		f.use("k8s.io/api/core/v1", "corev1"))
	for _, af := range c.fields {
		if value, ok := c.sample[af.apiName]; ok {
			f.printf("%s: %s,\n", af.goName, goValue(f, af.field, value))
		}
	}
	f.printf("},\n}\n},\n")
	keyField, err := f.convert("key", types.Typ[types.String], c.key.object.typ)
	if err != nil {
		return nil, fmt.Errorf("%s: the fake's objects: %w", name, err)
	}
	f.printf("seed: func(id, key string) %s { return %s{%s: id, %s: %s} },\n", object, object, c.id.goName,
		c.key.object.goName, keyField)
	created, err := matchesExpr(f, c, true)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if created == "" {
		created = "true"
	}
	f.printf("created: func(obj %s, current *%s) bool {\nreturn %s\n},\n", obj, object, created)

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
	f.printf("{field: \"accountRef\", what: \"changing accountRef\",\nedit: func(obj %s) { "+
		"obj.Spec.AccountRef.Name = \"other-account\" }},\n", obj)
	for _, e := range c.immutable {
		f.printf("{field: %q, what: %q,\nedit: func(obj %s) { obj.Spec.%s = %s }},\n", e.f.jsonName, e.what, obj,
			e.f.goName, goValue(f, e.f, e.value))
	}
	f.printf("},\n")
	edits("invalid", c.invalid)
	f.printf("})\n")
	return f.source(header)
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
// pointer field, and a long run of one character with strings.Repeat.
func goValue(f *goFile, sf field, value any) string {
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
// entries as dependents of their account, and the rule kinds, which the
// shared rule controller lists, as dependents of their Zone.
func emitKinds(kinds []*controller, ruleKinds []*ruleKind, header string) ([]byte, error) {
	f := newGoFile()
	ctrl := f.use("sigs.k8s.io/controller-runtime", "ctrl")
	client := f.use(clientPackage, "client")
	comment(&f.Builder, "", wrap("SetupGeneratedReconcilers registers the reconciler of every kind kflare-gen "+
		"generates with mgr."))
	f.printf("func SetupGeneratedReconcilers(mgr %s.Manager) error {\n", ctrl)
	names := make([]string, 0, len(kinds)+len(ruleKinds))
	for _, c := range kinds {
		names = append(names, c.name)
	}
	for _, k := range ruleKinds {
		names = append(names, k.name)
	}
	for _, name := range slices.Sorted(slices.Values(names)) {
		f.printf("if err := (&%sReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr); "+
			"err != nil {\nreturn %s.Errorf(\"setting up the %s controller: %%w\", err)\n}\n", name,
			f.use("fmt", "fmt"), name)
	}
	f.printf("return nil\n}\n\n")

	comment(&f.Builder, "", wrap("generatedAccountDependents are the generated kinds that the shared CRUD "+
		"reconciler drives, whose resources use a CloudflareAccount's credentials directly."))
	f.printf("var generatedAccountDependents = []dependentKind{\n")
	for _, c := range kinds {
		f.printf("dependents(%q, func() %s.ObjectList { return &%s.%sList{} },\n", c.name, client, f.api(), c.name)
		f.printf("func(obj *%s.%s) []string { return []string{obj.Spec.AccountRef.Name} }),\n", f.api(), c.name)
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
	comment(&f.Builder, "", wrap("generatedZoneDependents are the generated rule kinds, whose resources reference "+
		"a Zone in their namespace."))
	f.printf("var generatedZoneDependents = []dependentKind{\n")
	for _, k := range ruleKinds {
		f.printf("dependents(%q, func() %s.ObjectList { return &%s.%sList{} },\n", k.name, client, f.api(), k.name)
		f.printf("func(obj *%s.%s) []string { return []string{obj.Spec.ZoneRef.Name} }),\n", f.api(), k.name)
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
	fmt.Fprintf(&b, "    kflare.dev/deletion-policy: retain\nspec:\n  accountRef:\n    name: my-account\n")
	for _, af := range c.fields {
		value, ok := c.sample[af.apiName]
		if !ok {
			continue
		}
		for _, line := range af.doc {
			yamlComment(&b, "  ", line)
		}
		encoded, err := yaml.Marshal(value)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "  %s: %s", af.jsonName, encoded)
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
