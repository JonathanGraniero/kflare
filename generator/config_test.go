/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validConfig is a configuration loadConfig accepts.
const validConfig = `kinds:
  - kind: KVNamespace
    service: kv.NamespaceService
    scope: account
    shortName: cfkv
    noun: KV namespace
    doc: KVNamespace is a Workers KV namespace.
    id: {field: id, status: namespaceID}
    claimLabel: kflare.dev/kv-namespace-id
    adopt: {by: title}
    sample: {title: example}
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "generator.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.Kinds) != 1 || cfg.Kinds[0].Kind != "KVNamespace" || cfg.Kinds[0].ID.Status != "namespaceID" {
		t.Errorf("loadConfig = %+v", cfg)
	}
	zoned := strings.Replace(validConfig, "scope: account", "scope: zone", 1) +
		"    fields:\n      title: {ref: {kind: WorkerScript, field: name}}\n"
	cfg, err = loadConfig(writeConfig(t, zoned))
	if err != nil || cfg.Kinds[0].Fields["title"].Ref.Kind != "WorkerScript" {
		t.Errorf("loadConfig of a zone's kind with a reference: %+v, %v", cfg, err)
	}

	if _, err := loadConfig(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("loadConfig of a missing file: err = nil")
	}
}

// dependent returns a dependents key with one dependent.
func dependent(kind, refs, reason string) string {
	return "    dependents:\n      - kind: " + kind + "\n        refs: " + refs + "\n        reason: " + reason + "\n"
}

func TestLoadConfigRefuses(t *testing.T) {
	replace := func(old, new string) string {
		if !strings.Contains(validConfig, old) {
			t.Fatalf("validConfig has no %q", old)
		}
		return strings.Replace(validConfig, old, new, 1)
	}
	tests := []struct{ name, config, want string }{
		{"YAML that does not parse", "kinds: [", "error converting YAML"},
		{"an unknown key", validConfig + "    colour: blue\n", "unknown field"},
		{"no kinds", "kinds: []\n", "no kinds"},
		{"a kind listed twice", validConfig + strings.TrimPrefix(validConfig, "kinds:\n"), "listed twice"},
		{"a kind that is not a Go name", replace("kind: KVNamespace", "kind: kv-namespace"), "not a Go type name"},
		{"a service without a type", replace("service: kv.NamespaceService", "service: kv"), "not <package>.<type>"},
		{"a service ending in a dot", replace("service: kv.NamespaceService", "service: kv."), "not <package>.<type>"},
		{"another scope", replace("scope: account", "scope: plan"), "it is account or zone"},
		{"a short name in capitals", replace("shortName: cfkv", "shortName: CFKV"), "lower-case"},
		{"extras of a type outside the package", validConfig + "    hooks: {specExtras: extras}\n",
			"no exported type's name"},
		{"a sample's extras on a spec without any", validConfig + "    hooks: {sampleExtras: {items: []}}\n",
			"the spec has no extras"},
		{"a column without a path", validConfig + "    hooks: {columns: [{name: Items, type: integer}]}\n",
			"a name, a type and a path"},
		{"no noun", replace("noun: KV namespace", `noun: ""`), "noun is empty"},
		{"a doc not starting with the kind", replace("doc: KVNamespace is", "doc: It is"), "does not start with"},
		{"no ID field", replace("{field: id,", `{field: "",`), "id.field is empty"},
		{"an ID status name in capitals", replace("status: namespaceID", "status: NamespaceID"), "lowerCamel"},
		{"a claim label outside kflare.dev", replace("kflare.dev/kv-namespace-id", "example.com/id"), "kflare.dev/"},
		{"an ignore without a reason", validConfig + "    ignore:\n      jurisdiction: \" \"\n", "gives no reason"},
		{"a deletion blocker without a code", validConfig + "    deletionBlockers:\n" +
			"      - {reason: BucketNotEmpty, message: holds objects, why: checked}\n",
			"deletionBlockers[0]: code 0 is not a Cloudflare error code"},
		{"a deletion blocker listed twice", validConfig + "    deletionBlockers:\n" +
			"      - {code: 10008, reason: BucketNotEmpty, message: holds objects, why: checked}\n" +
			"      - {code: 10008, reason: Again, message: holds objects, why: checked}\n",
			"deletionBlockers[1]: code 10008 is listed twice"},
		{"a deletion blocker's reason in lower case", validConfig + "    deletionBlockers:\n" +
			"      - {code: 10008, reason: bucketNotEmpty, message: holds objects, why: checked}\n",
			"deletionBlockers[0]: reason \"bucketNotEmpty\" is not a CamelCase condition reason"},
		{"a deletion blocker without a message", validConfig + "    deletionBlockers:\n" +
			"      - {code: 10008, reason: BucketNotEmpty, message: \" \", why: checked}\n",
			"deletionBlockers[0]: gives no message"},
		{"a deletion blocker without why", validConfig + "    deletionBlockers:\n" +
			"      - {code: 10008, reason: BucketNotEmpty, message: holds objects}\n",
			"deletionBlockers[0]: does not say why"},
		{"a field both ignored and adjusted", validConfig + "    ignore:\n      title: why\n    fields:\n      title: {}\n",
			"both ignored and adjusted"},
		{"a validation without a reason", validConfig + "    fields:\n      title: {validation: {maxLength: 2}}\n",
			"fields.title.validation: gives no reason"},
		{"a minimum length above the maximum",
			validConfig + "    fields:\n      title: {validation: {minLength: 5, maxLength: 2, reason: x}}\n",
			"minLength is above maxLength"},
		{"a minimum above the maximum",
			validConfig + "    fields:\n      size: {validation: {minimum: 5, maximum: 2, reason: x}}\n",
			"minimum is above maximum"},
		{"a pattern that does not compile",
			validConfig + "    fields:\n      title: {validation: {pattern: \"[a-\", reason: x}}\n",
			"the pattern does not compile"},
		{"a pattern with a backquote",
			validConfig + "    fields:\n      title: {validation: {pattern: \"^`$\", reason: x}}\n",
			"backquote"},
		{"a field both ignored and fixed", validConfig + "    ignore:\n      kind: why\n    fixed:\n      kind: ip\n",
			"both ignored and fixed"},
		{"a field both fixed and adjusted", validConfig + "    fixed:\n      kind: ip\n    fields:\n      kind: {}\n",
			"both fixed and adjusted"},
		{"no adoption key", replace("adopt: {by: title}", "adopt: {}"), "adopt.by is empty"},
		{"no sample", replace("sample: {title: example}", "sample: {}"), "sample is empty"},
		{"a dependent that is not a Go name", validConfig + dependent("worker-script", "bound", "why"),
			"not a Go type name"},
		{"a dependent listed twice", validConfig + dependent("WorkerScript", "bound", "why") +
			strings.TrimPrefix(dependent("WorkerScript", "bound", "why"), "    dependents:\n"), "listed twice"},
		{"a dependent's refs that is exported", validConfig + dependent("WorkerScript", "Bound", "why"),
			"not an unexported Go name"},
		{"a dependent without a reason", validConfig + dependent("WorkerScript", "bound", `" "`), "gives no reason"},
		{"a reference to a kind that is not a Go name",
			validConfig + "    fields:\n      title: {ref: {kind: worker-script, field: name}}\n",
			"fields.title.ref: kind \"worker-script\" is not a Go type name"},
		{"a reference to a field in capitals",
			validConfig + "    fields:\n      title: {ref: {kind: WorkerScript, field: Name}}\n",
			"fields.title.ref: field \"Name\" is not a lowerCamel name"},
		{"a reference with a validation",
			validConfig + "    fields:\n      title: {ref: {kind: WorkerScript, field: name}, validation: {maxLength: 2, " +
				"reason: x}}\n", "fields.title: a reference takes no validation"},
		{"a reference reported as another field",
			validConfig + "    fields:\n      title: {ref: {kind: WorkerScript, field: name}, reportedAs: name}\n",
			"fields.title: a reference takes no reportedAs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(writeConfig(t, tc.config))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("loadConfig: err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// validRuleKind is a configuration with one rule kind that loadConfig accepts.
const validRuleKind = `ruleKinds:
  - kind: TransformRule
    rule: rulesets.RewriteRuleParam
    shortName: cftransform
    noun: transform rule
    doc: TransformRule transforms.
    typeDoc: Type picks the phase.
    types:
      - {name: urlRewrite, phase: http_request_transform, doc: rewrites the URL}
      - {name: responseHeaders, phase: http_response_headers_transform, doc: changes response headers}
    samples:
      - {type: urlRewrite, expression: 'true'}
      - {type: responseHeaders, expression: 'true'}
    fields:
      headers: {key: name, keyDoc: Name is the header's name.}
    rules:
      - {rule: 'true', message: m, reason: r, invalid: {expression: 'true'}}
    ignore:
      logging: Not mapped.
`

// validActionsKind is a rule kind of several actions, with a conflict.
const validActionsKind = `ruleKinds:
  - kind: CustomRule
    actions:
      - {action: block, rule: rulesets.BlockRuleParam, doc: blocks}
      - {action: challenge, doc: challenges}
    actionDoc: Action is what Cloudflare does.
    shortName: cfcustom
    noun: custom rule
    doc: CustomRule acts.
    phase: http_request_firewall_custom
    samples:
      - {expression: 'true', action: block}
    conflicts:
      - {code: 20014, reason: RulesetConflict, why: Another rule holds it.}
`

func TestLoadConfigRefusesRuleKindActions(t *testing.T) {
	if _, err := loadConfig(writeConfig(t, validActionsKind)); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	replace := func(old, new string) string {
		if !strings.Contains(validActionsKind, old) {
			t.Fatalf("validActionsKind has no %q", old)
		}
		return strings.Replace(validActionsKind, old, new, 1)
	}
	tests := []struct{ name, config, want string }{
		{"both a rule and actions", replace("    actions:\n", "    rule: rulesets.BlockRuleParam\n    actions:\n"),
			"set exactly one of rule and actions"},
		{"neither a rule nor actions", replace("      - {action: block, rule: rulesets.BlockRuleParam, doc: blocks}\n"+
			"      - {action: challenge, doc: challenges}\n", ""), "set exactly one of rule and actions"},
		{"one action", replace("      - {action: challenge, doc: challenges}\n", ""), "actions lists one action"},
		{"actions without a doc", replace("    actionDoc: Action is what Cloudflare does.\n", ""),
			"actionDoc is empty"},
		{"an action doc without actions", strings.Replace(validRuleKind, "    typeDoc:", "    actionDoc: x\n    typeDoc:",
			1), "actionDoc is set without actions"},
		{"an action that is not an action's name", replace("{action: challenge,", "{action: Challenge,"),
			"actions[1]: action \"Challenge\" is not an action's name"},
		{"an action listed twice", replace("{action: challenge,", "{action: block,"), "actions[1]: block is listed twice"},
		{"an action's rule without a type", replace("rule: rulesets.BlockRuleParam", "rule: rulesets"),
			"actions[0]: rule \"rulesets\" is not <package>.<type>"},
		{"an action without a doc", replace("doc: challenges}", `doc: ""}`), "actions[1]: doc is empty"},
		{"no action naming its rule type", replace("{action: block, rule: rulesets.BlockRuleParam,",
			"{action: block,"), "actions: none names its rule type"},
		{"a conflict without a code", replace("{code: 20014,", "{code: 0,"), "conflicts[0]: code 0 is not"},
		{"a conflict listed twice", replace("      - {code: 20014, reason: RulesetConflict, why: Another rule holds it.}\n",
			"      - {code: 20014, reason: RulesetConflict, why: Another rule holds it.}\n"+
				"      - {code: 20014, reason: Other, why: x}\n"), "conflicts[1]: code 20014 is listed twice"},
		{"a conflict reason in lower case", replace("reason: RulesetConflict", "reason: rulesetConflict"),
			"not an UpperCamel condition reason"},
		{"a conflict without why", replace("why: Another rule holds it.", `why: " "`), "conflicts[0]: why is empty"},
	}
	for _, tc := range tests {
		if _, err := loadConfig(writeConfig(t, tc.config)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", tc.name, err, tc.want)
		}
	}
}

func TestLoadConfigRefusesRuleKinds(t *testing.T) {
	if _, err := loadConfig(writeConfig(t, validRuleKind)); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	replace := func(old, new string) string {
		if !strings.Contains(validRuleKind, old) {
			t.Fatalf("validRuleKind has no %q", old)
		}
		return strings.Replace(validRuleKind, old, new, 1)
	}
	single := replace("    typeDoc: Type picks the phase.\n", "    phase: http_request_firewall_custom\n")
	single = strings.Replace(single, "    types:\n      - {name: urlRewrite, phase: http_request_transform, doc: "+
		"rewrites the URL}\n      - {name: responseHeaders, phase: http_response_headers_transform, doc: changes "+
		"response headers}\n", "", 1)
	single = strings.Replace(single, "      - {type: responseHeaders, expression: 'true'}\n", "", 1)
	if _, err := loadConfig(writeConfig(t, single)); err != nil {
		t.Fatalf("a rule kind with one phase: %v", err)
	}
	tests := []struct{ name, config, want string }{
		{"a rule kind listed twice", validRuleKind + strings.TrimPrefix(validRuleKind, "ruleKinds:\n"), "listed twice"},
		{"a rule kind named as a kind", validConfig + strings.NewReplacer("kind: TransformRule", "kind: KVNamespace",
			"doc: TransformRule", "doc: KVNamespace").Replace(validRuleKind), "ruleKinds[0]: KVNamespace is listed twice"},
		{"a kind that is not a Go name", replace("kind: TransformRule", "kind: transform-rule"), "not a Go type name"},
		{"a rule without a type", replace("rule: rulesets.RewriteRuleParam", "rule: rulesets"), "not <package>.<type>"},
		{"a short name in capitals", replace("shortName: cftransform", "shortName: CF"), "lower-case"},
		{"no noun", replace("noun: transform rule", `noun: ""`), "noun is empty"},
		{"a doc not starting with the kind", replace("doc: TransformRule transforms.", "doc: It transforms."),
			"does not start with"},
		{"both a phase and types", replace("    typeDoc:", "    phase: http_request_transform\n    typeDoc:"),
			"set exactly one of phase and types"},
		{"one type", replace("      - {name: responseHeaders, phase: http_response_headers_transform, doc: changes "+
			"response headers}\n", ""), "types lists one type"},
		{"types without a doc", replace("    typeDoc: Type picks the phase.\n", ""), "typeDoc is empty"},
		{"a type doc without types", strings.Replace(single, "    phase:", "    typeDoc: x\n    phase:", 1),
			"typeDoc is set without types"},
		{"a sample missing", replace("      - {type: responseHeaders, expression: 'true'}\n", ""), "give 2 samples"},
		{"a type that is not lowerCamel", replace("{name: urlRewrite,", "{name: URLRewrite,"), "not a lowerCamel name"},
		{"a type listed twice", replace("{name: responseHeaders,", "{name: urlRewrite,"), "listed twice"},
		{"a type without a phase", replace("phase: http_request_transform,", `phase: "",`), "phase is empty"},
		{"a type without a doc", replace("doc: rewrites the URL}", `doc: ""}`), "doc is empty"},
		{"a rule without a message", replace("message: m,", `message: "",`), "needs a rule and a message"},
		{"a rule in double quotes", replace("rule: 'true',", `rule: 'self.a == "b"',`), "double quote"},
		{"a rule without a reason", replace("reason: r,", `reason: "",`), "gives no reason"},
		{"a rule without an invalid spec", replace("invalid: {expression: 'true'}", "invalid: {}"),
			"gives no invalid spec"},
		{"an ignore without a reason", replace("logging: Not mapped.", `logging: " "`), "gives no reason"},
		{"a field both ignored and adjusted", replace("logging: Not mapped.", "headers: Not mapped."),
			"both ignored and adjusted"},
		{"a key without a doc", replace("{key: name, keyDoc: Name is the header's name.}", "{key: name}"),
			"set key and keyDoc together"},
		{"a key in capitals", replace("{key: name,", "{key: Name,"), "not a lowerCamel name"},
		{"an enum with a length", replace("{key: name, keyDoc: Name is the header's name.}",
			"{validation: {enum: [a], maxLength: 2, reason: r}}"), "an enum takes no lengths, pattern, range or integer"},
		{"an enum with an empty value", replace("{key: name, keyDoc: Name is the header's name.}",
			"{validation: {enum: [a, ''], reason: r}}"), "the enum holds an empty value"},
		{"an enum listing a value twice", replace("{key: name, keyDoc: Name is the header's name.}",
			"{validation: {enum: [a, a], reason: r}}"), `the enum lists "a" twice`},
		{"an enum value with a semicolon", replace("{key: name, keyDoc: Name is the header's name.}",
			"{validation: {enum: ['a;b'], reason: r}}"), "holds a semicolon or a quote"},
		{"a validation without a reason", replace("{key: name, keyDoc: Name is the header's name.}",
			"{validation: {minLength: 1}}"), "gives no reason"},
		{"a field's rule without a message", replace("{key: name, keyDoc: Name is the header's name.}",
			"{key: name, keyDoc: Name is the header's name., rules: [{rule: 'true', message: '', reason: r, "+
				"invalid: {expression: 'true'}}]}"), "fields.headers.rules[0]: needs a rule and a message"},
		{"a zero left out without a reason", replace("{key: name, keyDoc: Name is the header's name.}",
			"{key: name, keyDoc: Name is the header's name., omitZero: ' '}"), "fields.headers.omitZero gives no reason"},
		{"a key validation without a key", replace("{key: name, keyDoc: Name is the header's name.}",
			"{keyValidation: {minLength: 1, reason: r}}"), "fields.headers: keyValidation without a key"},
		{"a key validation without a reason", replace("{key: name, keyDoc: Name is the header's name.}",
			"{key: name, keyDoc: Name is the header's name., keyValidation: {minLength: 1}}"),
			"fields.headers.keyValidation: gives no reason"},
	}
	for _, tc := range tests {
		if _, err := loadConfig(writeConfig(t, tc.config)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", tc.name, err, tc.want)
		}
	}
}
