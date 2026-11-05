/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

// Command crdcompat reports the breaking changes to the CRDs since the last
// release: the changes to a version the release served that an object it
// stored, or a manifest written for it, may not fit. Kubernetes goes on
// serving the stored objects after an upgrade, but a write to one can fail,
// and so can a GitOps sync of a manifest nobody changed, or the same
// manifests applied to a new cluster.
//
// It compares config/crd/bases with the CRDs at the newest release tag in
// HEAD's history, or the release -base names, and reports, for each version
// that release served:
//
//   - the CRD, the version or a field removed, the version no longer served,
//     and a new kind, list kind or scope
//   - a field's new type, a field newly required without a default, and a
//     default added, changed or removed, which changes what a manifest that
//     leaves the field out means
//   - a tighter enum, bound, length or count, a pattern, format or CEL rule
//     that is new or changed, and null or unknown fields no longer accepted
//   - a list or map whose type or keys change, which changes how a server-side
//     apply merges it
//
// A change to an alpha version is allowed once CHANGELOG.md lists it, as
// GOVERNANCE.md asks of a breaking change: a section headed "Breaking
// changes", above the release's own section (in Unreleased, or in the section
// of the release being prepared), must name each kind the command reports. A
// change to a beta or GA version always fails: it belongs in a new version.
//
// Run it from the repository's root, through make crd-compat.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// The repository's paths, relative to its root.
const (
	crdDir        = "config/crd/bases"
	changelogPath = "CHANGELOG.md"
)

func main() {
	flags := flag.NewFlagSet("crdcompat", flag.ContinueOnError)
	root := flags.String("root", ".", "the repository whose CRDs to check")
	base := flags.String("base", "", "the release tag to compare with (default: the newest vX.Y.Z tag in HEAD's history)")
	err := flags.Parse(os.Args[1:])
	var report string
	if err == nil {
		report, err = run(*root, *base)
	}
	fmt.Print(report)
	if err != nil {
		fmt.Fprintln(os.Stderr, "crdcompat:", err)
		os.Exit(1)
	}
}

// run compares the CRDs of the repository at root with those of the release
// base, or of the last release when base is empty. It returns its report,
// and an error when a breaking change is not allowed.
func run(root, base string) (string, error) {
	if base == "" {
		var err error
		if base, err = lastRelease(root); err != nil {
			return "", err
		}
	}
	released, err := releasedCRDs(root, base)
	if err != nil {
		return "", err
	}
	current, err := currentCRDs(root)
	if err != nil {
		return "", err
	}
	changes := compareCRDs(released, current)
	if len(changes) == 0 {
		return fmt.Sprintf("No breaking change to the CRDs since %s.\n", base), nil
	}
	changelog, err := os.ReadFile(filepath.Join(root, changelogPath))
	if err != nil {
		return "", err
	}
	notes, err := breakingNotes(string(changelog), base)
	if err != nil {
		return "", fmt.Errorf("%s: %w", changelogPath, err)
	}
	return verdict(changes, notes, base)
}

// releaseTag matches a release's tag, the only tags Tag release makes.
var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// lastRelease returns the newest release tag in the history of the
// repository's HEAD: the release a branch builds on.
func lastRelease(root string) (string, error) {
	out, err := git(root, "tag", "--merged", "HEAD", "--list", "--sort=-v:refname", "v*")
	if err != nil {
		return "", err
	}
	for tag := range strings.FieldsSeq(string(out)) {
		if releaseTag.MatchString(tag) {
			return tag, nil
		}
	}
	return "", errors.New("no release tag (vX.Y.Z) in HEAD's history: fetch the tags (git fetch --tags), " +
		"or name a release with -base")
}

// git runs git in the repository at root, and returns its output.
func git(root string, args ...string) ([]byte, error) {
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			err = fmt.Errorf("%w: %s", err, bytes.TrimSpace(exit.Stderr))
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// crds holds CRDs by name.
type crds map[string]*apiextensionsv1.CustomResourceDefinition

// releasedCRDs returns the CRDs at the release tag base.
func releasedCRDs(root, base string) (crds, error) {
	out, err := git(root, "ls-tree", "-z", "--name-only", base+":"+crdDir)
	if err != nil {
		return nil, err
	}
	found := crds{}
	for name := range strings.SplitSeq(string(out), "\x00") {
		if filepath.Ext(name) != ".yaml" {
			continue
		}
		file := crdDir + "/" + name
		data, err := git(root, "show", base+":"+file)
		if err != nil {
			return nil, err
		}
		if err := found.add(data); err != nil {
			return nil, fmt.Errorf("%s at %s: %w", file, base, err)
		}
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("%s holds no CRDs at %s", crdDir, base)
	}
	return found, nil
}

// currentCRDs returns the CRDs in the repository at root.
func currentCRDs(root string) (crds, error) {
	entries, err := os.ReadDir(filepath.Join(root, crdDir))
	if err != nil {
		return nil, err
	}
	found := crds{}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		file := filepath.Join(crdDir, entry.Name())
		data, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			return nil, err
		}
		if err := found.add(data); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
	}
	return found, nil
}

// add reads data, a CRD as controller-gen writes one, into c.
func (c crds) add(data []byte) error {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(data, crd); err != nil {
		return err
	}
	if crd.Kind != "CustomResourceDefinition" || crd.Name == "" {
		return errors.New("not a CustomResourceDefinition")
	}
	c[crd.Name] = crd
	return nil
}

// change is a breaking change to a CRD, to one of its versions, or to a field
// of a version's schema.
type change struct {
	kind string
	// version is the version changed, or empty for a change to the whole CRD.
	version string
	// field is the field's path in the version's schema, or empty for a change
	// to the version itself.
	field string
	what  string
	// alpha: every version the change breaks is an alpha, which may change
	// once CHANGELOG.md says how to upgrade.
	alpha bool
}

func (c change) String() string {
	s := c.kind
	if c.version != "" {
		s += " " + c.version
	}
	if c.field != "" {
		s += " " + c.field
	}
	return s + ": " + c.what
}

// alphaVersion matches the name of an alpha API version.
var alphaVersion = regexp.MustCompile(`^v\d+alpha\d+$`)

// compareCRDs returns the breaking changes from the released CRDs to the
// current ones, by CRD and version.
func compareCRDs(released, current crds) []change {
	var changes []change
	for _, name := range slices.Sorted(maps.Keys(released)) {
		old := released[name]
		kind := old.Spec.Names.Kind
		alpha := true
		for _, v := range old.Spec.Versions {
			alpha = alpha && (!v.Served || alphaVersion.MatchString(v.Name))
		}
		whole := func(what string) {
			changes = append(changes, change{kind: kind, what: what, alpha: alpha})
		}
		crd, ok := current[name]
		if !ok {
			whole("the CRD " + name + " is removed")
			continue
		}
		if crd.Spec.Scope != old.Spec.Scope {
			whole(fmt.Sprintf("its scope changes from %s to %s", old.Spec.Scope, crd.Spec.Scope))
		}
		if crd.Spec.Names.Kind != kind {
			whole("its kind becomes " + crd.Spec.Names.Kind)
		}
		if crd.Spec.Names.ListKind != old.Spec.Names.ListKind {
			whole("its list kind becomes " + crd.Spec.Names.ListKind)
		}
		for _, version := range old.Spec.Versions {
			if !version.Served {
				continue
			}
			report := func(field, what string) {
				changes = append(changes, change{kind: kind, version: version.Name, field: field, what: what,
					alpha: alphaVersion.MatchString(version.Name)})
			}
			i := slices.IndexFunc(crd.Spec.Versions, func(v apiextensionsv1.CustomResourceDefinitionVersion) bool {
				return v.Name == version.Name
			})
			switch {
			case i < 0:
				report("", "the version is removed")
			case !crd.Spec.Versions[i].Served:
				report("", "the version is no longer served")
			default:
				compareSchema(schemaOf(version), schemaOf(crd.Spec.Versions[i]), "", report)
			}
		}
	}
	return changes
}

// schemaOf returns a version's schema, or an empty one, which allows anything.
func schemaOf(version apiextensionsv1.CustomResourceDefinitionVersion) *apiextensionsv1.JSONSchemaProps {
	if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
		return &apiextensionsv1.JSONSchemaProps{}
	}
	return version.Schema.OpenAPIV3Schema
}

// compareSchema reports, through report, each way in which n, the schema of
// the field at path, accepts less than o, the field's schema in the release,
// and does the same for the fields under it.
func compareSchema(o, n *apiextensionsv1.JSONSchemaProps, path string, report func(field, what string)) {
	at := func(what string) { report(path, what) }
	if n.Type != "" && n.Type != o.Type {
		at(fmt.Sprintf("its type changes from %s to %s", typeName(o), n.Type))
		return
	}
	compareValues(o, n, at)
	compareBounds(o, n, at)
	compareRules(o, n, at)
	compareDefault(o, n, at)

	if n.Type == "array" {
		if before, after := listType(o), listType(n); before != after {
			at(fmt.Sprintf("its list type changes from %s to %s", before, after))
		} else if !slices.Equal(o.XListMapKeys, n.XListMapKeys) {
			at(fmt.Sprintf("its list map keys change from %v to %v", o.XListMapKeys, n.XListMapKeys))
		}
	}
	if n.Type == "object" {
		if before, after := mapType(o), mapType(n); before != after {
			at(fmt.Sprintf("its map type changes from %s to %s", before, after))
		}
	}
	compareFields(o, n, path, report)
}

// compareValues reports the values n refuses that o accepted: other types,
// enum values, null, unknown fields and other embedded objects.
func compareValues(o, n *apiextensionsv1.JSONSchemaProps, at func(string)) {
	oldValues, newValues := enumValues(o), enumValues(n)
	switch {
	case len(newValues) == 0:
	case len(oldValues) == 0:
		at("its values are limited to " + strings.Join(newValues, ", "))
	default:
		var lost []string
		for _, v := range oldValues {
			if !slices.Contains(newValues, v) {
				lost = append(lost, v)
			}
		}
		if len(lost) > 0 {
			at("its enum loses " + strings.Join(lost, ", "))
		}
	}
	if o.Nullable && !n.Nullable {
		at("it no longer accepts null")
	}
	if keepsUnknownFields(o) && !keepsUnknownFields(n) {
		at("it no longer keeps unknown fields")
	}
	if o.XEmbeddedResource != n.XEmbeddedResource {
		at(fmt.Sprintf("x-kubernetes-embedded-resource changes from %t to %t", o.XEmbeddedResource,
			n.XEmbeddedResource))
	}
}

// compareBounds reports the bounds n holds a value to that are tighter than
// o's: of a number, a string's length, and the counts of items and of
// properties.
func compareBounds(o, n *apiextensionsv1.JSONSchemaProps, at func(string)) {
	if tighterBound(o.Minimum, o.ExclusiveMinimum, n.Minimum, n.ExclusiveMinimum, 1) {
		at(describeBound("minimum", o.Minimum, o.ExclusiveMinimum, n.Minimum, n.ExclusiveMinimum))
	}
	if tighterBound(o.Maximum, o.ExclusiveMaximum, n.Maximum, n.ExclusiveMaximum, -1) {
		at(describeBound("maximum", o.Maximum, o.ExclusiveMaximum, n.Maximum, n.ExclusiveMaximum))
	}
	for _, limit := range []struct {
		name     string
		old, new *int64
		sign     int64
	}{
		{"minLength", o.MinLength, n.MinLength, 1},
		{"maxLength", o.MaxLength, n.MaxLength, -1},
		{"minItems", o.MinItems, n.MinItems, 1},
		{"maxItems", o.MaxItems, n.MaxItems, -1},
		{"minProperties", o.MinProperties, n.MinProperties, 1},
		{"maxProperties", o.MaxProperties, n.MaxProperties, -1},
	} {
		switch {
		case limit.new == nil:
		case limit.old == nil:
			at(fmt.Sprintf("%s %d is added", limit.name, *limit.new))
		case (*limit.new-*limit.old)*limit.sign > 0:
			at(fmt.Sprintf("%s changes from %d to %d", limit.name, *limit.old, *limit.new))
		}
	}
	switch {
	case n.MultipleOf == nil:
	case o.MultipleOf == nil:
		at("multipleOf " + formatNumber(*n.MultipleOf) + " is added")
	case *n.MultipleOf != *o.MultipleOf:
		at("multipleOf changes from " + formatNumber(*o.MultipleOf) + " to " + formatNumber(*n.MultipleOf))
	}
	if n.UniqueItems && !o.UniqueItems {
		at("its items must be unique")
	}
}

// tighterBound reports whether n, a number's bound, exclusive or not, accepts
// less than o: sign is 1 for a minimum and -1 for a maximum.
func tighterBound(o *float64, oExclusive bool, n *float64, nExclusive bool, sign float64) bool {
	switch {
	case n == nil:
		return false
	case o == nil:
		return true
	default:
		return (*n-*o)*sign > 0 || *n == *o && nExclusive && !oExclusive
	}
}

// describeBound says how a minimum or maximum changes.
func describeBound(name string, o *float64, oExclusive bool, n *float64, nExclusive bool) string {
	bound := func(v float64, exclusive bool) string {
		if exclusive {
			return formatNumber(v) + ", exclusive"
		}
		return formatNumber(v)
	}
	if o == nil {
		return fmt.Sprintf("%s %s is added", name, bound(*n, nExclusive))
	}
	return fmt.Sprintf("%s changes from %s to %s", name, bound(*o, oExclusive), bound(*n, nExclusive))
}

// compareRules reports the rules n holds a value to that o did not: a
// pattern, a format, a CEL rule, and allOf, anyOf, oneOf and not.
func compareRules(o, n *apiextensionsv1.JSONSchemaProps, at func(string)) {
	for _, rule := range []struct{ name, old, new string }{
		{"pattern", o.Pattern, n.Pattern},
		{"format", o.Format, n.Format},
	} {
		switch {
		case rule.new == "" || rule.new == rule.old:
		case rule.old == "":
			at(fmt.Sprintf("%s %q is added", rule.name, rule.new))
		default:
			at(fmt.Sprintf("%s changes from %q to %q", rule.name, rule.old, rule.new))
		}
	}
	for _, rule := range n.XValidations {
		known := slices.ContainsFunc(o.XValidations, func(r apiextensionsv1.ValidationRule) bool {
			return r.Rule == rule.Rule && optional(r.OptionalOldSelf) == optional(rule.OptionalOldSelf)
		})
		if !known {
			at(fmt.Sprintf("CEL rule %q is added", rule.Rule))
		}
	}
	for _, composite := range []struct {
		name     string
		old, new any
		set      bool
	}{
		{"allOf", o.AllOf, n.AllOf, len(n.AllOf) > 0},
		{"anyOf", o.AnyOf, n.AnyOf, len(n.AnyOf) > 0},
		{"oneOf", o.OneOf, n.OneOf, len(n.OneOf) > 0},
		{"not", o.Not, n.Not, n.Not != nil},
	} {
		if composite.set && marshal(composite.old) != marshal(composite.new) {
			at(composite.name + " is added or changed")
		}
	}
}

// compareDefault reports a default that changes, which changes what a
// manifest that leaves the field out means.
func compareDefault(o, n *apiextensionsv1.JSONSchemaProps, at func(string)) {
	switch before, after := rawJSON(o.Default), rawJSON(n.Default); {
	case before == after:
	case before == "":
		at("its default " + after + " is added")
	case after == "":
		at("its default " + before + " is removed")
	default:
		at("its default changes from " + before + " to " + after)
	}
}

// compareFields compares the fields under o and n: the properties of an
// object, the items of a list and the values of a map, and the properties n
// newly requires.
func compareFields(o, n *apiextensionsv1.JSONSchemaProps, path string, report func(field, what string)) {
	for _, name := range slices.Sorted(maps.Keys(o.Properties)) {
		field := path + "." + name
		before := o.Properties[name]
		after, ok := n.Properties[name]
		if !ok {
			// A field that the object keeps as an unknown field is not lost.
			if !keepsUnknownFields(n) {
				report(field, "it is removed")
			}
			continue
		}
		compareSchema(&before, &after, field, report)
	}
	for _, name := range n.Required {
		if slices.Contains(o.Required, name) {
			// A default it loses is reported as the field's own change.
			continue
		}
		property, defined := n.Properties[name]
		if defined && property.Default != nil {
			continue
		}
		if _, existed := o.Properties[name]; existed {
			report(path+"."+name, "it becomes required")
		} else {
			report(path+"."+name, "it is new and required")
		}
	}
	if o.Items != nil && o.Items.Schema != nil && n.Items != nil && n.Items.Schema != nil {
		compareSchema(o.Items.Schema, n.Items.Schema, path+"[*]", report)
	}
	if allowsKeys(o) {
		if !allowsKeys(n) {
			report(path, "it no longer accepts keys of its own")
		} else {
			compareSchema(valuesOf(o), valuesOf(n), path+"[*]", report)
		}
	}
}

// typeName names a schema's type, as the type or what accepts any type.
func typeName(s *apiextensionsv1.JSONSchemaProps) string {
	switch {
	case s.Type != "":
		return s.Type
	case s.XIntOrString:
		return "int-or-string"
	default:
		return "any type"
	}
}

// enumValues returns a schema's enum values as compact JSON.
func enumValues(s *apiextensionsv1.JSONSchemaProps) []string {
	values := make([]string, 0, len(s.Enum))
	for _, v := range s.Enum {
		values = append(values, compactJSON(v.Raw))
	}
	return values
}

// keepsUnknownFields reports whether a schema keeps the fields it does not
// define, rather than drop them.
func keepsUnknownFields(s *apiextensionsv1.JSONSchemaProps) bool {
	return s.XPreserveUnknownFields != nil && *s.XPreserveUnknownFields
}

// allowsKeys reports whether a schema is a map's, accepting keys it does not
// define.
func allowsKeys(s *apiextensionsv1.JSONSchemaProps) bool {
	return s.AdditionalProperties != nil && (s.AdditionalProperties.Allows || s.AdditionalProperties.Schema != nil)
}

// valuesOf returns the schema of a map's values, or an empty one, which
// allows anything.
func valuesOf(s *apiextensionsv1.JSONSchemaProps) *apiextensionsv1.JSONSchemaProps {
	if s.AdditionalProperties.Schema == nil {
		return &apiextensionsv1.JSONSchemaProps{}
	}
	return s.AdditionalProperties.Schema
}

// listType returns a list's x-kubernetes-list-type; a list without one is
// atomic.
func listType(s *apiextensionsv1.JSONSchemaProps) string {
	if s.XListType == nil {
		return "atomic"
	}
	return *s.XListType
}

// mapType returns an object's x-kubernetes-map-type; an object without one is
// granular.
func mapType(s *apiextensionsv1.JSONSchemaProps) string {
	if s.XMapType == nil {
		return "granular"
	}
	return *s.XMapType
}

// optional reads a CEL rule's optionalOldSelf, false when unset.
func optional(b *bool) bool { return b != nil && *b }

// rawJSON returns a JSON value as compact JSON, or "" for none.
func rawJSON(v *apiextensionsv1.JSON) string {
	if v == nil {
		return ""
	}
	return compactJSON(v.Raw)
}

// compactJSON returns raw without insignificant space, or as it is if it is
// not JSON.
func compactJSON(raw []byte) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}

// marshal returns v as JSON, to compare schemas whose types have no equality.
func marshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// formatNumber formats a bound as the schema writes it.
func formatNumber(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// breakingHeading matches the heading of a changelog's breaking changes.
var breakingHeading = regexp.MustCompile(`(?i)^###\s+breaking\b`)

// breakingNotes returns the text of each "Breaking changes" section of the
// changelog above the section of the release base: the unreleased changes,
// and those of a release being prepared.
func breakingNotes(changelog, base string) (string, error) {
	baseHeading := regexp.MustCompile(`^## ` + regexp.QuoteMeta(base) + `(\s|$)`)
	var notes []string
	var inSection, inFence bool
	for line := range strings.Lines(changelog) {
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
		} else if !inFence {
			if baseHeading.MatchString(line) {
				return strings.Join(notes, "\n"), nil
			}
			if strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ") {
				inSection = breakingHeading.MatchString(line)
				continue
			}
		}
		if inSection {
			notes = append(notes, line)
		}
	}
	return "", fmt.Errorf("no section for %s", base)
}

// names reports whether notes name kind, as a word or as its plural.
func names(notes, kind string) bool {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(kind) + `s?\b`).MatchString(notes)
}

// verdict reports the changes, and returns an error if one is not allowed:
// one to a version that is not an alpha, or to a kind that notes, the
// changelog's breaking changes since the release base, do not name.
func verdict(changes []change, notes, base string) (string, error) {
	var report strings.Builder
	fmt.Fprintf(&report, "Breaking changes to the CRDs since %s:\n\n", base)
	var stable, unnamed []string
	for _, c := range changes {
		fmt.Fprintf(&report, "  %s\n", c)
		switch {
		case !c.alpha:
			stable = append(stable, c.kind)
		case !names(notes, c.kind) && !slices.Contains(unnamed, c.kind):
			unnamed = append(unnamed, c.kind)
		}
	}
	report.WriteString("\n")
	var errs []error
	if len(stable) > 0 {
		errs = append(errs, fmt.Errorf("%s: a version that is not an alpha must not break; serve the change in a new "+
			"version instead", strings.Join(slices.Compact(stable), ", ")))
	}
	if len(unnamed) > 0 {
		errs = append(errs, fmt.Errorf("%s's breaking changes since %s do not name %s: add a \"### Breaking "+
			"changes\" section to its Unreleased section that names each kind above and says how to upgrade",
			changelogPath, base, strings.Join(unnamed, ", ")))
	}
	if len(errs) > 0 {
		return report.String(), errors.Join(errs...)
	}
	report.WriteString(changelogPath + " lists them under \"Breaking changes\".\n")
	return report.String(), nil
}
