/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"fmt"
	"slices"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
)

// kflare-gen evaluates the CEL rules of a rule kind, those it writes and those
// its entry gives, on the specs its generated specs write: the API server
// refuses a spec that breaks one. So a change that would is left out, a
// sample or test that does is an error in the entry, and each invalid spec
// must break the rule it is for. The environment is CEL's with the strings
// extension, which the API server's also holds.

// celChecker evaluates CEL rules on objects as JSON decodes them.
type celChecker struct {
	env      *cel.Env
	programs map[string]cel.Program
}

func newCELChecker() (*celChecker, error) {
	env, err := cel.NewEnv(cel.Variable("self", cel.DynType), ext.Strings())
	if err != nil {
		return nil, err
	}
	return &celChecker{env: env, programs: map[string]cel.Program{}}, nil
}

// holds reports whether rule holds on self.
func (c *celChecker) holds(rule string, self any) (bool, error) {
	prg, ok := c.programs[rule]
	if !ok {
		ast, iss := c.env.Compile(rule)
		if iss.Err() != nil {
			return false, fmt.Errorf("the rule %q: %w", rule, iss.Err())
		}
		var err error
		if prg, err = c.env.Program(ast); err != nil {
			return false, fmt.Errorf("the rule %q: %w", rule, err)
		}
		c.programs[rule] = prg
	}
	out, _, err := prg.Eval(map[string]any{"self": self})
	if err != nil {
		return false, fmt.Errorf("the rule %q: %w", rule, err)
	}
	held, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("the rule %q gives %v, not a bool", rule, out.Value())
	}
	return held, nil
}

// breaks checks that a spec breaks a rule with message, as an invalid spec
// must.
func (k *ruleKind) breaks(spec map[string]any, message string) error {
	broken, err := k.broken(spec)
	if err != nil {
		return err
	}
	if !slices.Contains(broken, message) {
		return fmt.Errorf("breaks no rule with the message %q", message)
	}
	return nil
}

// valid checks that a spec breaks no rule, as a sample or test must.
func (k *ruleKind) valid(spec map[string]any) error {
	broken, err := k.broken(spec)
	if err != nil {
		return err
	}
	if len(broken) > 0 {
		return fmt.Errorf("breaks the rule %q", broken[0])
	}
	return nil
}

// broken returns the messages of the rules a checked spec breaks: the spec's
// own, and those of every object in it.
func (k *ruleKind) broken(spec map[string]any) ([]string, error) {
	var messages []string
	var failed error
	check := func(rules []celRule, self map[string]any) {
		for _, r := range rules {
			if failed != nil {
				return
			}
			held, err := k.cel.holds(r.rule, self)
			if err != nil {
				failed = err
				return
			}
			if !held {
				messages = append(messages, r.message)
			}
		}
	}
	check(k.specRules, spec)
	visitor{object: func(_ []step, st *paramStruct, value map[string]any) {
		if st != k.params {
			check(st.rules, value)
		}
	}}.visitStruct(nil, k.params, spec)
	return messages, failed
}
