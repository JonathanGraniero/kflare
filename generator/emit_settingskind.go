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
	"slices"
	"strconv"
	"strings"
)

// emitSettingsTypes returns the Go source of a settings kind's API types: its
// spec, the structs of its settings, its root and list types, and the
// accessors of the settings kinds.
func emitSettingsTypes(k *settingsKind, header string) ([]byte, error) {
	var b strings.Builder
	write := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	write("%s\n\n%s\n\npackage v1alpha1\n\n", strings.TrimRight(header, "\n"), generatedMarker)
	write("import (\n\tmetav1 %q\n)\n\n", "k8s.io/apimachinery/pkg/apis/meta/v1")

	comment(&b, "", wrap(fmt.Sprintf("%sSpec defines the desired state of %s: its zone, and the %s it configures "+
		"there.", k.name, k.name, k.noun)))
	comment(&b, "", ruleMarkers(k.specRules))
	write("type %sSpec struct {\n\tSettingsSpec `json:\",inline\"`\n", k.name)
	if len(k.params.fields) > 0 {
		write("\n")
	}
	writeParamFields(&b, k.params.fields)
	write("}\n\n")

	for _, st := range k.structs {
		comment(&b, "", st.doc)
		comment(&b, "", ruleMarkers(st.rules))
		write("type %s struct {\n", st.goName)
		writeParamFields(&b, st.fields)
		write("}\n\n")
	}

	write("// +kubebuilder:object:root=true\n// +kubebuilder:subresource:status\n")
	write("// +kubebuilder:resource:path=%s,scope=Namespaced,shortName=%s,categories=kflare\n", k.plural, k.shortName)
	for _, c := range k.printColumns {
		write("// +kubebuilder:printcolumn:name=%q,type=%s,JSONPath=`%s`\n", c.name, c.typ, c.jsonPath)
	}
	write("\n")
	comment(&b, "", k.doc)
	write("type %s struct {\n", k.name)
	write("\tmetav1.TypeMeta   `json:\",inline\"`\n\tmetav1.ObjectMeta `json:\"metadata,omitempty\"`\n\n")
	write("\tSpec   %sSpec   `json:\"spec,omitempty\"`\n", k.name)
	write("\tStatus SettingsStatus `json:\"status,omitempty\"`\n}\n\n")

	write("// +kubebuilder:object:root=true\n\n// %sList contains a list of %s.\n", k.name, k.name)
	write("type %sList struct {\n", k.name)
	write("\tmetav1.TypeMeta `json:\",inline\"`\n\tmetav1.ListMeta `json:\"metadata,omitempty\"`\n")
	write("\tItems []%s `json:\"items\"`\n}\n\n", k.name)

	comment(&b, "", wrap(fmt.Sprintf("GetConditions returns the status conditions of %s.", article(k.name))))
	write("func (in *%s) GetConditions() []metav1.Condition { return in.Status.Conditions }\n\n", k.name)
	comment(&b, "", wrap(fmt.Sprintf("SetConditions replaces the status conditions of %s.", article(k.name))))
	write("func (in *%s) SetConditions(c []metav1.Condition) { in.Status.Conditions = c }\n\n", k.name)
	comment(&b, "", wrap(fmt.Sprintf("GetSettingsSpec returns the spec fields %s shares with every settings kind.",
		article(k.name))))
	write("func (in *%s) GetSettingsSpec() *SettingsSpec { return &in.Spec.SettingsSpec }\n\n", k.name)
	comment(&b, "", wrap(fmt.Sprintf("GetSettingsStatus returns the status of %s.", article(k.name))))
	write("func (in *%s) GetSettingsStatus() *SettingsStatus { return &in.Status }\n\n", k.name)
	write("func init() {\n\tregister(&%s{}, &%sList{})\n}\n", k.name, k.name)

	src, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("formatting %s's types: %w\n%s", k.name, err, b.String())
	}
	return src, nil
}

// conversions returns a settings kind's conversions, of the settings its
// service's write sends.
func (k *settingsKind) conversions() conversions {
	return conversions{kind: k.name, params: k.params, root: k.varName + "Params",
		rootWhat: fmt.Sprintf("the settings %s asks for, as cloudflare-go v7's %s sends them, but for the zone",
			article(k.name), k.svc.write.name)}
}

// emitSettingsController returns the Go source of a settings kind's
// controller: the narrow interface to its cloudflare-go v7 service, its
// reconciler, RBAC markers, the descriptor the shared settings reconciler runs
// it with, and the conversion of its spec into the settings it asks for.
func emitSettingsController(k *settingsKind, header string) ([]byte, error) {
	f := newGoFile()
	name, v, svc := k.name, k.varName, k.svc
	api := name + "API"
	svcPkg := f.use(svc.pkg.Path(), svc.pkg.Name())
	service := svcPkg + "." + svc.typeName
	ctx := f.use("context", "context")
	client := f.use(clientPackage, "client")
	ctrl := f.use("sigs.k8s.io/controller-runtime", "ctrl")
	obj := "*" + f.api() + "." + name

	comment(&f.Builder, "", wrap(fmt.Sprintf("%s is the part of cloudflare-go v7's %s that the %s reconciler calls. "+
		"Tests inject a fake; in production it is a *%s.", api, service, name, service)))
	f.printf("type %s interface {\n", api)
	for _, call := range []*sdkCall{svc.read, svc.write, svc.reset} {
		f.printf("\t%s\n", interfaceMethod(f, call))
	}
	f.printf("}\n\nvar _ %s = (*%s)(nil)\n\n", api, service)

	comment(&f.Builder, "", wrap(fmt.Sprintf("%sReconciler reconciles %s object, with the shared settings "+
		"controller (settings_controller.go).", name, article(name))))
	f.printf("type %sReconciler struct {\n\t%s.Client\n\tScheme *%s.Scheme\n\n", name, client,
		f.use("k8s.io/apimachinery/pkg/runtime", "runtime"))
	comment(&f.Builder, "\t", wrapIndented(fmt.Sprintf("New%s constructs %s from a raw API token. Defaults to "+
		"default%s; tests inject a fake.", api, article(api), api)))
	f.printf("\tNew%s func(token string) (%s, error)\n}\n\n", api, api)
	for _, line := range []string{
		fmt.Sprintf("groups=kflare.dev,resources=%s,verbs=get;list;watch;create;update;patch;delete", k.plural),
		fmt.Sprintf("groups=kflare.dev,resources=%s/status,verbs=get;update;patch", k.plural),
		fmt.Sprintf("groups=kflare.dev,resources=%s/finalizers,verbs=update", k.plural),
		"groups=kflare.dev,resources=zones,verbs=get;list;watch",
		"groups=kflare.dev,resources=cloudflareaccounts,verbs=get;list;watch",
		`groups="",resources=secrets,verbs=get;list;watch`,
	} {
		f.printf("// +kubebuilder:rbac:%s\n", line)
	}
	f.printf("\n")

	comment(&f.Builder, "", wrap(fmt.Sprintf("%sKind describes %s to the shared settings reconciler. %s "+
		"configures the %s of its zone: it writes the settings its spec sets, and resets them when it is deleted. "+
		"A zone nobody has configured holds %s.", v, pluralOf(name), upperCamel(article(name)), k.noun,
		prose(unconfiguredWords(k.unconfigured)))))
	f.printf("var %sKind = settingsKind[%s]{\n", v, api)
	f.printf("name: %q,\nnoun: %q,\n", name, k.noun)
	f.printf("newObject: func() settingsObject { return &%s.%s{} },\n", f.api(), name)
	f.printf("newList: func() %s.ObjectList { return &%s.%sList{} },\n\n", client, f.api(), name)
	f.printf("read: func(ctx %s.Context, api %s, zoneID string) (string, error) {\n", ctx, api)
	if err := zoneParams(f, svc.read); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	f.printf("settings, err := api.%s(ctx, params)\nif err != nil {\nreturn \"\", err\n}\n", svc.read.name)
	f.printf("return settings.JSON.RawJSON(), nil\n},\n")
	f.printf("write: func(ctx %s.Context, api %s, zoneID string, obj settingsObject) error {\n", ctx, api)
	f.printf("params := %s(obj.(%s).Spec)\n", k.conversions().name(k.params), obj)
	if err := setZoneID(f, svc.write); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	f.printf("_, err := api.%s(ctx, params)\nreturn err\n},\n", svc.write.name)
	f.printf("reset: func(ctx %s.Context, api %s, zoneID string) error {\n", ctx, api)
	if err := zoneParams(f, svc.reset); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if svc.reset.sig.Results().Len() == 1 {
		f.printf("return api.%s(ctx, params)\n},\n", svc.reset.name)
	} else {
		f.printf("_, err := api.%s(ctx, params)\nreturn err\n},\n", svc.reset.name)
	}
	f.printf("desired: func(obj settingsObject) ([]byte, error) {\n")
	f.printf("return %s(obj.(%s).Spec).MarshalJSON()\n},\n\n", k.conversions().name(k.params), obj)
	f.printf("shape: %s,\n", settingsShapeLiteral(k))
	f.printf("unconfigured: []settingsValue{\n")
	for _, u := range k.unconfigured {
		f.printf("{path: %q, value: %s},\n", u.path, jsonValueLiteral(u.value))
	}
	f.printf("},\n}\n\n")

	f.printf("func (r *%sReconciler) Reconcile(ctx %s.Context, req %s.Request) (%s.Result, error) {\n", name, ctx, ctrl,
		ctrl)
	f.printf("return r.settings().reconcile(ctx, req)\n}\n\n")
	comment(&f.Builder, "", wrap(fmt.Sprintf("SetupWithManager registers %sReconciler with the manager. It watches "+
		"Zones, so %s waiting for its Zone is reconciled once the Zone is ready, and the deletion of %s, so "+
		"another one takes over the zone's %s at once.", name, article(name), pluralOf(name), k.noun)))
	f.printf("func (r *%sReconciler) SetupWithManager(mgr %s.Manager) error {\nreturn r.settings().setup(mgr, r)\n}\n\n",
		name, ctrl)
	settings := fmt.Sprintf("settingsReconciler[%s]", api)
	comment(&f.Builder, "", wrap(fmt.Sprintf("settings returns the shared settings reconciler for %s, with the "+
		"injected factory or the production one.", pluralOf(name))))
	f.printf("func (r *%sReconciler) settings() *%s {\n", name, settings)
	f.printf("newAPI := r.New%s\nif newAPI == nil {\nnewAPI = default%s\n}\n", api, api)
	f.printf("return &%s{Client: r.Client, kind: %sKind, newAPI: newAPI}\n}\n\n", settings, v)

	comment(&f.Builder, "", wrap(fmt.Sprintf("default%s is the production factory: cloudflare-go v7's %s, built "+
		"from cfpkg.New's options for the token.", api, service)))
	f.printf("func default%s(token string) (%s, error) {\n", api, api)
	f.printf("client, err := %s.New(token)\nif err != nil {\nreturn nil, err\n}\n", f.use(cfPackage, "cfpkg"))
	f.printf("return %s.New%s(client.V7Options()...), nil\n}\n\n", svcPkg, svc.typeName)

	for _, st := range append([]*paramStruct{k.params}, k.structs...) {
		if err := emitConversion(f, k.conversions(), st); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	return f.source(header)
}

// zoneParams writes the params of a call that takes only the zone's ID, set
// to zoneID.
func zoneParams(f *goFile, call *sdkCall) error {
	f.printf("var params %s\n", f.typ(call.params))
	return setZoneID(f, call)
}

// setZoneID writes the statement that sets the zone's ID in a call's params
// to zoneID.
func setZoneID(f *goFile, call *sdkCall) error {
	zone := call.paramFields[slices.IndexFunc(call.paramFields, isZoneID)]
	value, err := f.convert("zoneID", types.Typ[types.String], zone.typ)
	if err != nil {
		return fmt.Errorf("%s's zone_id: %w", call.name, err)
	}
	f.printf("params.%s.Value, params.%s.Present = %s, true\n", zone.goName, zone.goName, value)
	return nil
}

// settingsShapeLiteral writes a settings kind's settingsShape as Go source.
func settingsShapeLiteral(k *settingsKind) string {
	var fields []string
	if len(k.listKeys) > 0 {
		keys := make([]string, 0, len(k.listKeys))
		for _, path := range slices.Sorted(maps.Keys(k.listKeys)) {
			keys = append(keys, fmt.Sprintf("%q: %q,", path, k.listKeys[path]))
		}
		fields = append(fields, "listKeys: map[string]string{\n"+strings.Join(keys, "\n")+"\n}")
	}
	if len(k.sets) > 0 {
		sets := make([]string, 0, len(k.sets))
		for _, path := range k.sets {
			sets = append(sets, fmt.Sprintf("%q: true,", path))
		}
		fields = append(fields, "sets: map[string]bool{\n"+strings.Join(sets, "\n")+"\n}")
	}
	if len(fields) == 0 {
		return "settingsShape{}"
	}
	return "settingsShape{\n" + strings.Join(fields, ",\n") + ",\n}"
}

// jsonValueLiteral writes a value as Go source, as encoding/json decodes it
// into an any: a number as a float64.
func jsonValueLiteral(value any) string {
	switch v := value.(type) {
	case string:
		return strconv.Quote(v)
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return "float64(" + strconv.FormatFloat(v, 'f', -1, 64) + ")"
	case int64:
		return "float64(" + strconv.FormatInt(v, 10) + ")"
	}
	return fmt.Sprintf("%#v", value)
}

// unconfiguredWords says what the settings of a zone nobody has configured
// hold, a path and its value for each, for the descriptor's documentation.
func unconfiguredWords(values []settingsValue) []string {
	words := make([]string, len(values))
	for i, v := range values {
		words[i] = v.path + " " + jsonValueLiteral(v.value)
	}
	return words
}

// emitSettingsKindTest returns the Go source of a settings kind's envtest
// table: a fake of its cloudflare-go v7 service on fakeSettingsStore, the
// settings of an unconfigured zone, and the specs the shared settings kind
// specs write (settings_specs_test.go), with the settings each asks for.
func emitSettingsKindTest(k *settingsKind, header string) ([]byte, error) {
	f := newGoFile()
	name, svc := k.name, k.svc
	api, fake := name+"API", "fake"+name+"API"
	ctx := f.use("context", "context")
	opts := f.typ(svc.read.sig.Params().At(svc.read.sig.Params().Len() - 1).Type().(*types.Slice).Elem())
	zoneOf := func(call *sdkCall) string {
		return call.sig.Params().At(1).Name() + "." +
			call.paramFields[slices.IndexFunc(call.paramFields, isZoneID)].goName + ".Value"
	}
	param := func(call *sdkCall) string {
		return call.sig.Params().At(1).Name() + " " + f.typ(call.params)
	}

	comment(&f.Builder, "", wrap(fmt.Sprintf("%s is an in-memory %s for %s's generated specs: it keeps each zone's "+
		"%s in a fakeSettingsStore.", fake, svc.name, name, k.noun)))
	f.printf("type %s struct {\n*fakeSettingsStore\n}\n\nvar _ %s = %s{}\n\n", fake, api, fake)
	read := svc.read.sig.Results().At(0).Type().(*types.Pointer).Elem()
	f.printf("func (f %s) %s(_ %s.Context, %s, _ ...%s) (*%s, error) {\n", fake, svc.read.name, ctx, param(svc.read),
		opts, f.typ(read))
	f.printf("return fakeSettingsRead[%s](f.fakeSettingsStore, %s)\n}\n\n", f.typ(read), zoneOf(svc.read))
	written := svc.write.sig.Results().At(0).Type().(*types.Pointer).Elem()
	f.printf("func (f %s) %s(_ %s.Context, %s, _ ...%s) (*%s, error) {\n", fake, svc.write.name, ctx,
		param(svc.write), opts, f.typ(written))
	f.printf("return fakeSettingsWrite[%s](f.fakeSettingsStore, %s, %s)\n}\n\n", f.typ(written), zoneOf(svc.write),
		svc.write.sig.Params().At(1).Name())
	if svc.reset.sig.Results().Len() == 1 {
		f.printf("func (f %s) %s(_ %s.Context, %s, _ ...%s) error {\n", fake, svc.reset.name, ctx, param(svc.reset),
			opts)
		f.printf("return f.reset(%s)\n}\n\n", zoneOf(svc.reset))
	} else {
		reset := f.typ(svc.reset.sig.Results().At(0).Type().(*types.Pointer).Elem())
		f.printf("func (f %s) %s(_ %s.Context, %s, _ ...%s) (*%s, error) {\n", fake, svc.reset.name, ctx,
			param(svc.reset), opts, reset)
		f.printf("if err := f.reset(%s); err != nil {\nreturn nil, err\n}\nreturn new(%s), nil\n}\n\n",
			zoneOf(svc.reset), reset)
	}

	client := f.use(clientPackage, "client")
	obj := "*" + f.api() + "." + name
	comment(&f.Builder, "", wrap(fmt.Sprintf("The shared settings kind specs, on %s's sample and tests, the edits "+
		"of them, and specs that set every field.", name)))
	f.printf("var _ = describeSettingsKind(settingsKindSpecs[%s, %s]{\n", obj, api)
	f.printf("kind: %sKind,\n", k.varName)
	f.printf("newObject: func() %s { return &%s.%s{} },\n", obj, f.api(), name)
	f.printf("newReconciler: func(c %s.Client, newAPI func(string) (%s, error)) settingsKindReconciler[%s] {\n",
		client, api, api)
	f.printf("return &%sReconciler{Client: c, Scheme: c.Scheme(), New%s: newAPI}\n},\n", name, api)
	f.printf("newFake: func(store *fakeSettingsStore) %s { return %s{store} },\n", api, fake)
	f.printf("defaultAPI: default%s,\n", api)
	// The fake writes the keyed lists as Cloudflare does, from the table's
	// own copy of them, so that the specs check the kind's.
	if len(k.listKeys) > 0 {
		f.printf("keyedLists: map[string]string{\n")
		for _, path := range slices.Sorted(maps.Keys(k.listKeys)) {
			f.printf("%q: %q,\n", path, k.listKeys[path])
		}
		f.printf("},\n")
	}
	unconfigured, err := specJSON(k.unconfiguredState)
	if err != nil {
		return nil, err
	}
	f.printf("unconfigured: %s,\n", unconfigured)
	f.printf("configured: []settingsSpecCase{\n")
	for i, v := range k.unconfigured {
		other, err := json.Marshal(v.other)
		if err != nil {
			return nil, err
		}
		settings, err := specJSON(k.configuredStates[i])
		if err != nil {
			return nil, err
		}
		f.printf("{\nwhat: %q,\nsettings: %s,\n},\n", v.path+" "+string(other), settings)
	}
	f.printf("},\n")
	cases := func(label string, edits []ruleEdit) error {
		if len(edits) == 0 {
			return nil
		}
		f.printf("%s: []settingsSpecCase{\n", label)
		for _, e := range edits {
			spec, err := specJSON(e.spec)
			if err != nil {
				return err
			}
			f.printf("{\nwhat: %q,\nspec: %s,\n", e.what, spec)
			if e.sample > 0 {
				f.printf("sample: %d,\n", e.sample)
			}
			if e.rule != nil {
				settings, err := specJSON(e.rule)
				if err != nil {
					return err
				}
				f.printf("settings: %s,\n", settings)
			}
			if e.message != "" {
				f.printf("message: %q,\n", e.message)
			}
			f.printf("},\n")
		}
		f.printf("},\n")
		return nil
	}
	specs := k.specs()
	samples := make([]ruleEdit, len(specs))
	for i, spec := range specs {
		what := "the sample"
		if i > 0 {
			what = fmt.Sprintf("test %d", i)
		}
		samples[i] = ruleEdit{what: what, spec: spec, rule: k.bodies[i]}
	}
	for _, c := range []struct {
		label string
		edits []ruleEdit
	}{{"samples", samples}, {"changes", k.changes}, {"invalid", k.invalid}, {"full", k.full}} {
		if err := cases(c.label, c.edits); err != nil {
			return nil, err
		}
	}
	f.printf("})\n")
	return f.source(header)
}

// emitSettingsSample returns a settings kind's sample, for config/samples.
func emitSettingsSample(k *settingsKind) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s from generator/generator.yaml. DO NOT EDIT.\n#\n", generatedSample)
	for _, line := range k.doc {
		yamlComment(&b, "", line)
	}
	fmt.Fprintf(&b, "apiVersion: kflare.dev/v1alpha1\nkind: %s\nmetadata:\n  name: example-%s\n  namespace: default\n",
		k.name, strings.ToLower(k.name))
	fmt.Fprintf(&b, "spec:\n  zoneRef:\n    name: example-zone\n")
	for _, f := range k.params.fields {
		if value, ok := k.samples[0][f.jsonName]; ok {
			if err := writeYAMLField(&b, "  ", f.jsonName, f.node, value); err != nil {
				return nil, err
			}
		}
	}
	return []byte(b.String()), nil
}
