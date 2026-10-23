/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"cmp"
	"fmt"
	"slices"
)

// A rule kind's spec holds the parameters of its action, and a settings
// kind's the settings it writes: both are a tree of objects, lists, maps and
// unions that kflare-gen maps from cloudflare-go v7's param types
// (params.go). paramKind is what the two share: the tree, its CEL rules, and
// what the generated specs write from it, the specs and the edits of them
// (paramedits.go). Each kind says how a spec of it is checked, and what it
// asks Cloudflare for.

// paramKind is the part of a kind's model that its spec's tree of params
// gives.
type paramKind struct {
	// name is the kind's name, and root what messages call its params, such
	// as "the action parameters".
	name, root string
	// params is the struct whose fields the spec holds, and structs the
	// nested structs, in the order they appear.
	params  *paramStruct
	structs []*paramStruct
	// specRules are CEL rules on the spec: the params' own, then the entry's.
	specRules []celRule
	// samples are the entry's samples, checked, and tests its tests; bodies
	// are what each of them asks Cloudflare for, the samples' first.
	samples, tests, bodies []map[string]any
	// oneSample is set for an entry that gives one sample, as sample rather
	// than as samples, which messages name it by.
	oneSample bool
	// changes are edits of the samples that change what they ask for, with
	// what each asks for; invalid are specs the CRD refuses; full are specs
	// that set every field, with what each asks for.
	changes, invalid, full []ruleEdit
	// cel evaluates the kind's CEL rules on the specs the generated specs
	// write.
	cel *celChecker
	// check checks a spec of the kind, from an entry, and returns it with its
	// integers as int64. typeName is the type a rule kind's spec must have;
	// "" for a kind without types.
	check func(spec map[string]any, typeName string) (map[string]any, error)
	// body returns what a checked spec asks Cloudflare for: a rule kind's
	// rule, as Cloudflare stores it, or a settings kind's settings, as
	// cloudflare-go v7 sends them.
	body func(spec map[string]any) map[string]any
}

// specs returns the specs the generated specs write: the samples, then the
// tests.
func (k *paramKind) specs() []map[string]any {
	return append(slices.Clone(k.samples), k.tests...)
}

// specName names the spec specs returns at i in messages: a sample or a test
// of the entry.
func (k *paramKind) specName(i int) string {
	switch {
	case i >= len(k.samples):
		return fmt.Sprintf("tests[%d]", i-len(k.samples))
	case k.oneSample:
		return "sample"
	}
	return fmt.Sprintf("samples[%d]", i)
}

// addSpecs checks the entry's samples, then its tests, each against the
// kind's CEL rules too, and records them with what each asks for. typeOf
// returns the type the sample or test at i must have.
func (k *paramKind) addSpecs(samples, tests []map[string]any,
	typeOf func(sample bool, i int, spec map[string]any) (string, error)) error {
	add := func(sample bool, i int, spec map[string]any) error {
		typeName, err := typeOf(sample, i, spec)
		if err != nil {
			return err
		}
		checked, err := k.check(spec, typeName)
		if err == nil {
			err = k.valid(checked)
		}
		if err != nil {
			return err
		}
		if sample {
			k.samples = append(k.samples, checked)
		} else {
			k.tests = append(k.tests, checked)
		}
		k.bodies = append(k.bodies, k.body(checked))
		return nil
	}
	for i, spec := range samples {
		if err := add(true, i, spec); err != nil {
			if k.oneSample {
				return fmt.Errorf("sample: %w", err)
			}
			return fmt.Errorf("samples[%d]: %w", i, err)
		}
	}
	for i, spec := range tests {
		if err := add(false, i, spec); err != nil {
			return fmt.Errorf("tests[%d]: %w", i, err)
		}
	}
	return nil
}

// editor adds a kind's changes, keeping the first error.
type editor struct {
	k      *paramKind
	specs  []map[string]any
	failed error
}

func (k *paramKind) editor() *editor {
	return &editor{k: k, specs: k.specs()}
}

// change adds an edit of the spec at sample, unless it breaks a CEL rule, as
// flipping a bool that another field needs would.
func (e *editor) change(sample int, what string, edit func(spec map[string]any)) {
	spec := copySpec(e.specs[sample])
	edit(spec)
	broken, err := e.k.broken(spec)
	switch {
	case err != nil:
		e.failed = cmp.Or(e.failed, fmt.Errorf("%s, %s: %w", e.k.specName(sample), what, err))
	case len(broken) == 0:
		e.k.changes = append(e.k.changes, ruleEdit{what: what, sample: sample, spec: spec, rule: e.k.body(spec)})
	}
}
