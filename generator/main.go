/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

// Command kflare-gen generates the kinds in generator/generator.yaml from
// cloudflare-go v7, the SDK Cloudflare generates from its OpenAPI spec, and
// from a committed subset of that spec, which adds what the SDK's types leave
// out: lengths, patterns, ranges, defaults, integers and page sizes. For each
// kind it writes the API types, the controller, its envtest table and a
// sample, and it registers the kinds. Run it from the repository's root:
//
//	kflare-gen            generates the kinds (make codegen, which runs before
//	                      controller-gen)
//	kflare-gen sync-spec  rewrites the spec subset from a full spec (make
//	                      sync-spec)
package main

import (
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
)

// The repository's paths, relative to its root.
const (
	defaultConfig = "generator/generator.yaml"
	defaultSpec   = "generator/openapi/openapi.json"
)

func main() {
	var err error
	if len(os.Args) > 1 && os.Args[1] == "sync-spec" {
		err = syncSpecCommand(os.Args[2:])
	} else {
		err = generateCommand(os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kflare-gen:", err)
		os.Exit(1)
	}
}

func generateCommand(args []string) error {
	flags := flag.NewFlagSet("kflare-gen", flag.ContinueOnError)
	configPath := flags.String("config", defaultConfig, "the kinds to generate")
	specPath := flags.String("spec", defaultSpec, "the OpenAPI spec subset")
	root := flags.String("root", ".", "the repository the generated files go to")
	headerPath := flags.String("header", "hack/boilerplate.go.txt", "the license header of every generated Go file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return run(*configPath, *specPath, *root, *headerPath)
}

func syncSpecCommand(args []string) error {
	flags := flag.NewFlagSet("kflare-gen sync-spec", flag.ContinueOnError)
	var configs stringList
	flags.Var(&configs, "config", "a configuration whose kinds' operations the subset holds (repeatable)")
	from := flags.String("from", "", "the full OpenAPI spec, Cloudflare's api-schemas openapi.json")
	source := flags.String("source", "", "where the full spec comes from, recorded in the subset")
	out := flags.String("out", defaultSpec, "the subset to write")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *from == "" || *source == "" {
		return fmt.Errorf("sync-spec needs -from and -source")
	}
	if len(configs) == 0 {
		configs = stringList{defaultConfig}
	}
	return syncSpec(configs, sdk{dir: "."}, *from, *source, *out)
}

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// run generates the kinds in the configuration at configPath into the
// repository at root, removes what kflare-gen generated for kinds the
// configuration no longer lists, and rewrites the generated blocks of the
// files it edits.
func run(configPath, specPath, root, headerPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	spec, err := loadSpec(specPath)
	if err != nil {
		return err
	}
	header, err := os.ReadFile(headerPath)
	if err != nil {
		return err
	}
	out, err := generate(cfg, sdk{dir: "."}, spec, string(header))
	if err != nil {
		return err
	}
	return out.write(root)
}

// generate returns what kflare-gen writes for the kinds in cfg.
func generate(cfg *config, s sdk, spec *apiSpec, header string) (*outputs, error) {
	out := &outputs{files: map[string][]byte{}, blocks: map[string][]string{}}
	var controllers []*controller
	for _, entry := range cfg.Kinds {
		svc, err := s.service(entry.Service)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Kind, err)
		}
		k, err := buildKind(entry, svc, spec)
		if err != nil {
			return nil, err
		}
		c, err := buildController(k, entry, svc, spec)
		if err != nil {
			return nil, err
		}
		if err := out.addKind(c, header); err != nil {
			return nil, err
		}
		controllers = append(controllers, c)
	}
	var ruleKinds []*ruleKind
	for _, entry := range cfg.RuleKinds {
		k, err := buildRuleKind(entry, s.rule, spec)
		if err != nil {
			return nil, err
		}
		if err := out.addRuleKind(k, header); err != nil {
			return nil, err
		}
		ruleKinds = append(ruleKinds, k)
	}
	var settingsKinds []*settingsKind
	for _, entry := range cfg.SettingsKinds {
		svc, err := s.settings(entry.Service)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Kind, err)
		}
		k, err := buildSettingsKind(entry, svc, spec)
		if err != nil {
			return nil, err
		}
		if err := out.addSettingsKind(k, header); err != nil {
			return nil, err
		}
		settingsKinds = append(settingsKinds, k)
	}
	slices.SortFunc(controllers, func(a, b *controller) int { return strings.Compare(a.name, b.name) })
	slices.SortFunc(ruleKinds, func(a, b *ruleKind) int { return strings.Compare(a.name, b.name) })
	slices.SortFunc(settingsKinds, func(a, b *settingsKind) int { return strings.Compare(a.name, b.name) })
	src, err := emitKinds(controllers, ruleKinds, settingsKinds, header)
	if err != nil {
		return nil, err
	}
	out.files[kindsFile] = src
	plurals := map[string]string{}
	for _, c := range controllers {
		plurals[c.name] = c.plural
	}
	for _, k := range ruleKinds {
		plurals[k.name] = k.plural
	}
	for _, k := range settingsKinds {
		plurals[k.name] = k.plural
	}
	for _, name := range slices.Sorted(maps.Keys(plurals)) {
		out.blocks[crdKustomization] = append(out.blocks[crdKustomization], "- bases/kflare.dev_"+plurals[name]+".yaml")
		out.blocks[helmRBAC] = append(out.blocks[helmRBAC], "- "+plurals[name])
	}
	return out, nil
}
