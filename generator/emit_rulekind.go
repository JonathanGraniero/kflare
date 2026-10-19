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
	"regexp"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// rulesetsPackage is cloudflare-go v7's rulesets package, which the rule
// kinds' conversions build their rules with.
const rulesetsPackage = sdkModule + "/rulesets"

// goType returns a param field's Go type in the API types. An optional field
// is a pointer, so that unset differs from a zero value, but for a string
// that cannot be empty: an empty one is unset.
func (f *paramField) goType() string {
	switch n := f.node.(type) {
	case *paramScalar:
		if f.required || (n.goType == typeString && n.nonEmpty()) {
			return n.goType
		}
		return "*" + n.goType
	case *paramStruct:
		if f.required {
			return n.goName
		}
		return "*" + n.goName
	case *paramList:
		if items, ok := n.items.(*paramStruct); ok {
			return "[]" + items.goName
		}
		return "[]" + n.items.(*paramScalar).goType
	case *paramMap:
		return "[]" + n.value.goName
	}
	return ""
}

// nonEmpty reports whether a string's validation refuses an empty one: by an
// enum, a minimum length or a pattern that an empty string does not match.
func (n *paramScalar) nonEmpty() bool {
	v := n.validation
	return len(n.enum) > 0 || (v.minLength != nil && *v.minLength > 0) ||
		(v.pattern != "" && !regexp.MustCompile(v.pattern).MatchString(""))
}

// pointer reports whether a param field's Go type is a pointer.
func (f *paramField) pointer() bool {
	return strings.HasPrefix(f.goType(), "*")
}

// markers returns a param field's kubebuilder markers.
func (f *paramField) markers() []string {
	switch n := f.node.(type) {
	case *paramScalar:
		return field{enum: n.enum, validation: n.validation}.markers()
	case *paramList:
		out := []string{"+listType=atomic"}
		if n.set {
			out = []string{"+listType=set"}
		}
		out = append(out, itemBounds(n.minItems, n.maxItems)...)
		if items, ok := n.items.(*paramScalar); ok {
			// The validation of each item of a list of scalars.
			for _, m := range (field{enum: items.enum, validation: items.validation}).markers() {
				out = append(out, strings.Replace(m, "+kubebuilder:validation:", "+kubebuilder:validation:items:", 1))
			}
		}
		return out
	case *paramMap:
		return append([]string{"+listType=map", "+listMapKey=" + n.value.fields[0].jsonName},
			itemBounds(n.minItems, n.maxItems)...)
	}
	return nil
}

// itemBounds returns the markers that bound how many items a list holds.
func itemBounds(minItems, maxItems *int64) []string {
	var out []string
	if minItems != nil {
		out = append(out, fmt.Sprintf("+kubebuilder:validation:MinItems=%d", *minItems))
	}
	if maxItems != nil {
		out = append(out, fmt.Sprintf("+kubebuilder:validation:MaxItems=%d", *maxItems))
	}
	return out
}

// ruleMarkers returns the markers of CEL rules.
func ruleMarkers(rules []celRule) []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = fmt.Sprintf(`+kubebuilder:validation:XValidation:rule="%s",message="%s"`, r.rule, r.message)
	}
	return out
}

// writeParamFields writes a struct's fields, a blank line between two.
func writeParamFields(b *strings.Builder, fields []*paramField) {
	for i, f := range fields {
		if i > 0 {
			b.WriteString("\n")
		}
		comment(b, "\t", f.doc)
		if !f.required {
			b.WriteString("\t// +optional\n")
		}
		comment(b, "\t", f.markers())
		tag := f.jsonName
		if !f.required {
			tag += ",omitempty"
		}
		fmt.Fprintf(b, "\t%s %s `json:\"%s\"`\n", f.goName, f.goType(), tag)
	}
}

// emitRuleTypes returns the Go source of a rule kind's API types: its spec,
// the structs of its action parameters, its root and list types, and the
// accessors of the rule kinds.
func emitRuleTypes(k *ruleKind, header string) ([]byte, error) {
	var b strings.Builder
	write := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	write("%s\n\n%s\n\npackage v1alpha1\n\n", strings.TrimRight(header, "\n"), generatedMarker)
	write("import (\n\tmetav1 %q\n)\n\n", "k8s.io/apimachinery/pkg/apis/meta/v1")

	comment(&b, "", wrap(fmt.Sprintf("%sSpec defines the desired state of %s: the fields every rule kind shares, "+
		"and the parameters of its %s action.", k.name, k.name, k.rule.action)))
	comment(&b, "", ruleMarkers(k.specRules))
	write("type %sSpec struct {\n\tRulesetRuleSpec `json:\",inline\"`\n\n", k.name)
	if k.typeDoc != nil {
		comment(&b, "\t", k.typeDoc)
		b.WriteString("\t//\n")
		names := make([]string, len(k.phases))
		for i, p := range k.phases {
			names[i] = p.typeName
			for j, line := range wrapIndented(fmt.Sprintf("%s: %s (phase %s)", p.typeName, p.doc, p.phase)) {
				if j == 0 {
					comment(&b, "\t", []string{"  - " + line})
				} else {
					comment(&b, "\t", []string{"    " + line})
				}
			}
		}
		b.WriteString("\t//\n\t// Immutable.\n")
		write("\t// +kubebuilder:validation:Enum=%s\n", strings.Join(names, ";"))
		write("\t// %s\n", immutable(fieldType))
		write("\tType string `json:\"type\"`\n\n")
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
	write("// +kubebuilder:resource:path=%s,scope=Namespaced,shortName=%s\n", k.plural, k.shortName)
	for _, c := range k.printColumns {
		write("// +kubebuilder:printcolumn:name=%q,type=%s,JSONPath=`%s`\n", c.name, c.typ, c.jsonPath)
	}
	write("\n")
	comment(&b, "", k.doc)
	write("type %s struct {\n", k.name)
	write("\tmetav1.TypeMeta   `json:\",inline\"`\n\tmetav1.ObjectMeta `json:\"metadata,omitempty\"`\n\n")
	write("\tSpec   %sSpec   `json:\"spec,omitempty\"`\n", k.name)
	write("\tStatus RulesetRuleStatus `json:\"status,omitempty\"`\n}\n\n")

	write("// +kubebuilder:object:root=true\n\n// %sList contains a list of %s.\n", k.name, k.name)
	write("type %sList struct {\n", k.name)
	write("\tmetav1.TypeMeta `json:\",inline\"`\n\tmetav1.ListMeta `json:\"metadata,omitempty\"`\n")
	write("\tItems []%s `json:\"items\"`\n}\n\n", k.name)

	write("// GetConditions returns the %s's status conditions.\n", k.noun)
	write("func (in *%s) GetConditions() []metav1.Condition { return in.Status.Conditions }\n\n", k.name)
	write("// SetConditions replaces the %s's status conditions.\n", k.noun)
	write("func (in *%s) SetConditions(c []metav1.Condition) { in.Status.Conditions = c }\n\n", k.name)
	comment(&b, "", wrap(fmt.Sprintf("GetRulesetRuleSpec returns the spec fields the %s shares with every rule kind.",
		k.noun)))
	write("func (in *%s) GetRulesetRuleSpec() *RulesetRuleSpec { return &in.Spec.RulesetRuleSpec }\n\n", k.name)
	write("// GetRulesetRuleStatus returns the %s's status.\n", k.noun)
	write("func (in *%s) GetRulesetRuleStatus() *RulesetRuleStatus { return &in.Status }\n\n", k.name)
	write("func init() {\n\tregister(&%s{}, &%sList{})\n}\n", k.name, k.name)

	src, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("formatting %s's types: %w\n%s", k.name, err, b.String())
	}
	return src, nil
}

// conversionName returns the name of the function that converts a struct of
// the action parameters into its SDK struct.
func (k *ruleKind) conversionName(st *paramStruct) string {
	if st == k.params {
		return k.varName + "ActionParameters"
	}
	return lowerCamel(st.goName) + "Param"
}

// emitRuleController returns the Go source of a rule kind's controller: its
// reconciler and RBAC markers, its phases, its descriptor for the shared rule
// controller, and the conversion of its spec into the rule it asks for.
func emitRuleController(k *ruleKind, header string) ([]byte, error) {
	f := newGoFile()
	api, client := f.api(), f.use(clientPackage, "client")
	ctrl := f.use("sigs.k8s.io/controller-runtime", "ctrl")
	rs := f.use(rulesetsPackage, "rulesets")
	ctx := f.use("context", "context")

	comment(&f.Builder, "", wrap(fmt.Sprintf("%sReconciler reconciles %s object, with the shared rule controller "+
		"(rulesetrule_controller.go): one rule in its zone's entry point ruleset for the %s.", k.name, article(k.name),
		phrasePhases(k))))
	f.printf("type %sReconciler struct {\n%s.Client\nScheme *%s.Scheme\n\n", k.name, client,
		f.use("k8s.io/apimachinery/pkg/runtime", "runtime"))
	f.printf("// NewRulesetRuleAPI constructs a RulesetRuleAPI from a raw API token.\n")
	f.printf("// Defaults to defaultRulesetRuleAPI; tests inject a fake.\n")
	f.printf("NewRulesetRuleAPI func(token string) (RulesetRuleAPI, error)\n}\n\n")
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

	phases := make([]string, len(k.phases))
	for i, p := range k.phases {
		phases[i] = rs + "." + p.constant
	}
	if k.typeDoc != nil {
		comment(&f.Builder, "", wrap(fmt.Sprintf("%sPhases maps %s's type to the phase of its rule.", k.varName,
			article(k.name))))
		f.printf("var %sPhases = map[string]%s.Phase{\n", k.varName, rs)
		for _, p := range k.phases {
			f.printf("%q: %s.%s,\n", p.typeName, rs, p.constant)
		}
		f.printf("}\n\n")
	}
	comment(&f.Builder, "", []string{fmt.Sprintf("%sKind describes %s to the shared rule reconciler.", k.varName,
		k.name)})
	f.printf("var %sKind = rulesetRuleKind{\nname: %q,\nphases: []%s.Phase{%s},\n", k.varName, k.name, rs,
		strings.Join(phases, ", "))
	if k.typeDoc != nil {
		f.printf("phaseFor: func(obj rulesetRuleObject) %s.Phase {\nreturn %sPhases[obj.(*%s.%s).Spec.Type]\n},\n",
			rs, k.varName, api, k.name)
	}
	f.printf("newObject: func() rulesetRuleObject { return &%s.%s{} },\n", api, k.name)
	f.printf("newList: func() %s.ObjectList { return &%s.%sList{} },\n", client, api, k.name)
	f.printf("definition: func(obj rulesetRuleObject) %s.RulesetNewParamsRule {\n", rs)
	f.printf("return %sDefinition(obj.(*%s.%s))\n},\n}\n\n", k.varName, api, k.name)

	f.printf("func (r *%sReconciler) Reconcile(ctx %s.Context, req %s.Request) (%s.Result, error) {\n", k.name, ctx,
		ctrl, ctrl)
	f.printf("return r.rules().reconcile(ctx, req)\n}\n\n")
	comment(&f.Builder, "", wrap(fmt.Sprintf("SetupWithManager registers %sReconciler with the manager. It watches "+
		"Zones, so a rule waiting for its Zone is written as soon as the Zone is ready.", k.name)))
	f.printf("func (r *%sReconciler) SetupWithManager(mgr %s.Manager) error {\nreturn r.rules().setup(mgr, r)\n}\n\n",
		k.name, ctrl)
	f.printf("// rules returns the shared rule reconciler for %s.\n", pluralOf(k.name))
	f.printf("func (r *%sReconciler) rules() *rulesetRuleReconciler {\nnewAPI := r.NewRulesetRuleAPI\n", k.name)
	f.printf("if newAPI == nil {\nnewAPI = defaultRulesetRuleAPI\n}\n")
	f.printf("return &rulesetRuleReconciler{Client: r.Client, kind: %sKind, newAPI: newAPI}\n}\n\n", k.varName)

	comment(&f.Builder, "", wrap(fmt.Sprintf("%sDefinition is the Cloudflare rule %s asks for: a rule with the "+
		"%s action and the action parameters its spec holds.", k.varName, article(k.name), k.rule.action)))
	f.printf("func %sDefinition(rule *%s.%s) %s.RulesetNewParamsRule {\n", k.varName, api, k.name, rs)
	f.printf("definition := rulesetRuleDefinition(%s.%s, rule.Spec.RulesetRuleSpec)\n", rs, k.actionConst)
	f.printf("params := %s(rule.Spec)\n", k.conversionName(k.params))
	f.printf("definition.ActionParameters.Value, definition.ActionParameters.Present = params, true\n")
	f.printf("return definition\n}\n\n")

	for _, st := range append([]*paramStruct{k.params}, k.structs...) {
		if err := emitConversion(f, k, st); err != nil {
			return nil, fmt.Errorf("%s: %w", k.name, err)
		}
	}
	return f.source(header)
}

// phrasePhases says which phase a kind's rules are in, for its reconciler's
// documentation.
func phrasePhases(k *ruleKind) string {
	if k.typeDoc == nil {
		return k.phases[0].phase + " phase"
	}
	return "phase its type picks"
}

// emitConversion writes the function that converts a struct of the action
// parameters into its SDK struct: it sets each field the spec sets.
func emitConversion(f *goFile, k *ruleKind, st *paramStruct) error {
	in := f.api() + "." + st.goName
	what := fmt.Sprintf("%s of %s as cloudflare-go v7 sends it", st.path, article(k.name))
	switch {
	case st == k.params:
		in = f.api() + "." + k.name + "Spec"
		what = fmt.Sprintf("the action parameters of %s as cloudflare-go v7 sends them", article(k.name))
	case strings.HasSuffix(st.path, "[]"):
		what = fmt.Sprintf("an item of %s in %s as cloudflare-go v7 sends it", strings.TrimSuffix(st.path, "[]"),
			article(k.name))
	}
	sdk := f.typ(st.sdk)
	name := k.conversionName(st)
	comment(&f.Builder, "", wrap(fmt.Sprintf("%s returns %s.", name, what)))
	f.printf("func %s(in %s) %s {\nvar out %s\n", name, in, sdk, sdk)
	for _, pf := range st.fields {
		if pf.key {
			continue
		}
		if err := emitAssignment(f, k, pf); err != nil {
			return fmt.Errorf("%s.%s: %w", st.goName, pf.goName, err)
		}
	}
	f.printf("return out\n}\n\n")
	return nil
}

// emitAssignment writes the statements that set the SDK struct's field for a
// param field, when the spec sets it.
func emitAssignment(f *goFile, k *ruleKind, pf *paramField) error {
	value := "in." + pf.goName
	set := func(expr string) {
		f.printf("out.%s.Value, out.%s.Present = %s, true\n", pf.sdkGoName, pf.sdkGoName, expr)
	}
	open := func(condition string) { f.printf("if %s {\n", condition) }
	switch n := pf.node.(type) {
	case *paramScalar:
		target := pf.sdkType
		if pf.pointer() {
			value = "*" + value
		}
		expr, err := scalarExpr(f, n, value, target)
		if err != nil {
			return err
		}
		switch {
		case pf.pointer():
			open("in." + pf.goName + " != nil")
		case !pf.required:
			open("in." + pf.goName + " != " + zeroOf(n.goType))
		default:
			set(expr)
			return nil
		}
		set(expr)
		f.printf("}\n")
	case *paramStruct:
		if pf.pointer() {
			open("in." + pf.goName + " != nil")
			set(k.conversionName(n) + "(*" + value + ")")
			f.printf("}\n")
			return nil
		}
		set(k.conversionName(n) + "(" + value + ")")
	case *paramList:
		items := f.typ(n.sdkItems)
		open("len(" + value + ") > 0")
		switch item := n.items.(type) {
		case *paramStruct:
			f.printf("items := make([]%s, 0, len(%s))\nfor _, item := range %s {\n", items, value, value)
			f.printf("items = append(items, %s(item))\n}\n", k.conversionName(item))
		case *paramScalar:
			expr, err := scalarExpr(f, item, "item", n.sdkItems)
			if err != nil {
				return err
			}
			if expr == "item" {
				f.printf("items := %s.Clone(%s)\n", f.use("slices", "slices"), value)
			} else {
				f.printf("items := make([]%s, 0, len(%s))\nfor _, item := range %s {\n", items, value, value)
				f.printf("items = append(items, %s)\n}\n", expr)
			}
			if n.set {
				f.printf("%s.Sort(items)\n", f.use("slices", "slices"))
			}
		}
		set("items")
		f.printf("}\n")
	case *paramMap:
		open("len(" + value + ") > 0")
		f.printf("entries := make(map[string]%s, len(%s))\nfor _, item := range %s {\n", f.typ(n.sdkValue), value, value)
		f.printf("entries[item.%s] = %s(item)\n}\n", n.value.fields[0].goName, k.conversionName(n.value))
		set("entries")
		f.printf("}\n")
	}
	return nil
}

// scalarExpr converts a scalar's value, of its Go type in the API types, to
// the type the SDK takes: none for an untyped field.
func scalarExpr(f *goFile, n *paramScalar, value string, target types.Type) (string, error) {
	if isEmptyInterface(target) {
		return value, nil
	}
	from := types.Typ[types.String]
	switch n.goType {
	case typeBool:
		from = types.Typ[types.Bool]
	case typeInt64:
		from = types.Typ[types.Int64]
	}
	return f.convert(value, from, target)
}

// zeroOf returns the zero value of a Go type of the API types, as source.
func zeroOf(goType string) string {
	switch goType {
	case typeBool:
		return "false"
	case typeInt64:
		return "0"
	}
	return `""`
}

// specJSON writes a spec, or a rule, as the JSON the generated specs hold:
// a raw string literal when it can be one.
func specJSON(value map[string]any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if strings.Contains(string(data), "`") {
		return strconv.Quote(string(data)), nil
	}
	return "`" + string(data) + "`", nil
}

// emitRuleKindTest returns the Go source of a rule kind's envtest table: its
// samples with the phase and rule each asks for, and the edits of them, which
// the shared rule kind specs run (rulesetrule_specs_test.go).
func emitRuleKindTest(k *ruleKind, header string) ([]byte, error) {
	f := newGoFile()
	api, client := f.api(), f.use(clientPackage, "client")
	rs := f.use(rulesetsPackage, "rulesets")
	cases := func(name string, edits []ruleEdit, phase func(int) string) error {
		f.printf("%s: []ruleSpecCase{\n", name)
		for i, e := range edits {
			spec, err := specJSON(e.spec)
			if err != nil {
				return err
			}
			f.printf("{\nwhat: %q,\nspec: %s,\n", e.what, spec)
			if e.sample > 0 {
				f.printf("sample: %d,\n", e.sample)
			}
			if phase != nil {
				f.printf("phase: %s,\n", phase(i))
			}
			if e.rule != nil {
				rule, err := specJSON(e.rule)
				if err != nil {
					return err
				}
				f.printf("rule: %s,\n", rule)
			}
			if e.message != "" {
				f.printf("message: %q,\n", e.message)
			}
			f.printf("},\n")
		}
		f.printf("},\n")
		return nil
	}

	comment(&f.Builder, "", wrap(fmt.Sprintf("The shared rule kind specs, on %s's samples, the edits of them, "+
		"and specs that set every field.", k.name)))
	f.printf("var _ = describeRuleKind(ruleKindSpecs[*%s.%s]{\n", api, k.name)
	f.printf("kind: %sKind,\n", k.varName)
	f.printf("newObject: func() *%s.%s { return &%s.%s{} },\n", api, k.name, api, k.name)
	f.printf("newReconciler: func(c %s.Client, newAPI func(string) (RulesetRuleAPI, error)) ruleKindReconciler {\n",
		client)
	f.printf("return &%sReconciler{Client: c, Scheme: c.Scheme(), NewRulesetRuleAPI: newAPI}\n},\n", k.name)
	specs := k.specs()
	samples := make([]ruleEdit, len(specs))
	for i, spec := range specs {
		what := "the sample"
		if k.typeDoc != nil {
			what = "type " + k.phaseOf(spec).typeName
		}
		if i >= len(k.samples) {
			what = fmt.Sprintf("test %d", i-len(k.samples)+1)
			if k.typeDoc != nil {
				what += ", of type " + k.phaseOf(spec).typeName
			}
		}
		samples[i] = ruleEdit{what: what, spec: spec, rule: k.rules[i]}
	}
	phase := func(i int) string { return rs + "." + k.phaseOf(specs[i]).constant }
	if err := cases("samples", samples, phase); err != nil {
		return nil, err
	}
	for _, c := range []struct {
		name  string
		edits []ruleEdit
	}{{"changes", k.changes}, {"retypes", k.retypes}, {"invalid", k.invalid}, {"full", k.full}} {
		if len(c.edits) == 0 {
			continue
		}
		if err := cases(c.name, c.edits, nil); err != nil {
			return nil, err
		}
	}
	f.printf("})\n")
	return f.source(header)
}

// emitRuleSample returns a rule kind's samples, one YAML document each, for
// config/samples.
func emitRuleSample(k *ruleKind) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s from generator/generator.yaml. DO NOT EDIT.\n", generatedSample)
	for i, sample := range k.samples {
		if i > 0 {
			b.WriteString("---\n")
		}
		b.WriteString("#\n")
		name := "example-" + strings.ToLower(k.name)
		lines := k.doc
		if k.typeDoc != nil {
			p := k.phases[i]
			name += "-" + kebab(p.typeName)
			lines = wrapAt(fmt.Sprintf("A %s of type %s: %s.", k.noun, p.typeName, strings.TrimSuffix(p.doc, ".")),
				commentWidth-len("# "))
		}
		for _, line := range lines {
			yamlComment(&b, "", line)
		}
		fmt.Fprintf(&b, "apiVersion: kflare.dev/v1alpha1\nkind: %s\nmetadata:\n  name: %s\n  namespace: default\n",
			k.name, name)
		fmt.Fprintf(&b, "spec:\n  zoneRef:\n    name: example-zone\n")
		for _, key := range sharedSpecFields {
			if value, ok := sample[key]; ok {
				if err := writeYAMLField(&b, "  ", key, nil, value); err != nil {
					return nil, err
				}
			}
		}
		for _, f := range k.params.fields {
			if value, ok := sample[f.jsonName]; ok {
				if err := writeYAMLField(&b, "  ", f.jsonName, f.node, value); err != nil {
					return nil, err
				}
			}
		}
	}
	return []byte(b.String()), nil
}

// writeYAMLField writes a field of a sample as YAML after indent, the fields
// of its objects in their struct's order rather than sorted, as a person
// reads them: a header's name before its operation.
func writeYAMLField(b *strings.Builder, indent, key string, node paramNode, value any) error {
	switch n := node.(type) {
	case *paramStruct:
		b.WriteString(indent + key + ":\n")
		return writeYAMLObject(b, indent+"  ", n, value.(map[string]any))
	case *paramList, *paramMap:
		b.WriteString(indent + key + ":\n")
		for _, item := range value.([]any) {
			var err error
			switch n := node.(type) {
			case *paramMap:
				err = writeYAMLItem(b, indent, n.value, item)
			case *paramList:
				st, _ := n.items.(*paramStruct)
				err = writeYAMLItem(b, indent, st, item)
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	encoded, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	b.WriteString(indent + key + ": " + string(encoded))
	return nil
}

// writeYAMLObject writes an object's fields after indent, in its struct's
// order.
func writeYAMLObject(b *strings.Builder, indent string, st *paramStruct, value map[string]any) error {
	for _, f := range st.fields {
		if v, ok := value[f.jsonName]; ok {
			if err := writeYAMLField(b, indent, f.jsonName, f.node, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeYAMLItem writes an item of a list after indent: an object, its first
// field on the item's line, or a scalar when st is nil.
func writeYAMLItem(b *strings.Builder, indent string, st *paramStruct, item any) error {
	if st == nil {
		encoded, err := yaml.Marshal(item)
		if err != nil {
			return err
		}
		b.WriteString(indent + "- " + string(encoded))
		return nil
	}
	var object strings.Builder
	if err := writeYAMLObject(&object, indent+"  ", st, item.(map[string]any)); err != nil {
		return err
	}
	b.WriteString(indent + "- " + strings.TrimPrefix(object.String(), indent+"  "))
	return nil
}

// kebab writes a lowerCamel name in kebab case: urlRewrite is url-rewrite.
func kebab(name string) string {
	var b strings.Builder
	for i, r := range name {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('-')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
