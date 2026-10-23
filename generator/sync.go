/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// syncSpec writes to out the subset of the full OpenAPI spec at from that the
// kinds of the configurations use: the operations of their services' New, Get,
// Update (or Edit) and List calls, those of the settings kinds' writes, and the
// schemas of the rule kinds' rule types, with every component those reach. source says where the full spec
// comes from; the subset records it.
func syncSpec(configPaths []string, s sdk, from, source, out string) error {
	var routes []route
	var schemas []string
	for _, path := range configPaths {
		cfg, err := loadConfig(path)
		if err != nil {
			return err
		}
		for _, entry := range cfg.Kinds {
			svc, err := s.service(entry.Service)
			if err != nil {
				return fmt.Errorf("%s: %w", entry.Kind, err)
			}
			routes = append(routes, svc.newRoute, svc.getRoute)
			if svc.update != nil {
				routes = append(routes, svc.updateRoute)
			}
			if svc.calls["List"] != nil {
				routes = append(routes, svc.listRoute)
			}
		}
		for _, entry := range cfg.RuleKinds {
			schemas = append(schemas, ruleSchemaName(entry.Rule))
		}
		for _, entry := range cfg.SettingsKinds {
			svc, err := s.settings(entry.Service)
			if err != nil {
				return fmt.Errorf("%s: %w", entry.Kind, err)
			}
			routes = append(routes, svc.writeRoute)
		}
	}

	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	var full map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	// Numbers stay as written, so that the subset copies them exactly.
	decoder.UseNumber()
	if err := decoder.Decode(&full); err != nil {
		return fmt.Errorf("%s: %w", from, err)
	}
	subset, err := specSubset(full, routes, schemas, source)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(subset, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(out, append(encoded, '\n'), 0o644)
}

// specSubset returns the operations of routes and the component schemas named
// schemas out of spec, with the components they reach. Its maps marshal with
// sorted keys, so the subset only changes where the spec does.
func specSubset(spec map[string]any, routes []route, schemas []string, source string) (map[string]any, error) {
	paths, _ := spec["paths"].(map[string]any)
	components, _ := spec["components"].(map[string]any)
	byRoute := map[string]string{}
	for path := range paths {
		byRoute[normalizeRoute(path)] = path
	}

	outPaths := map[string]any{}
	var pending []any
	for _, r := range routes {
		path, ok := byRoute[normalizeRoute(r.path)]
		if !ok {
			return nil, fmt.Errorf("the spec has no path for %s", r)
		}
		item, _ := paths[path].(map[string]any)
		op, ok := item[r.verb]
		if !ok {
			return nil, fmt.Errorf("the spec has no %s", r)
		}
		outItem, ok := outPaths[path].(map[string]any)
		if !ok {
			outItem = map[string]any{}
			if params, ok := item["parameters"]; ok {
				outItem["parameters"] = params
				pending = append(pending, params)
			}
			outPaths[path] = outItem
		}
		outItem[r.verb] = op
		pending = append(pending, op)
	}

	for _, name := range schemas {
		pending = append(pending, map[string]any{"$ref": "#/components/schemas/" + name})
	}
	outComponents := map[string]any{}
	copied := map[string]bool{}
	for len(pending) > 0 {
		node := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		refs, err := localRefs(node)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			if copied[ref] {
				continue
			}
			copied[ref] = true
			section, name := splitRef(ref)
			sectionMap, _ := components[section].(map[string]any)
			c, ok := sectionMap[name]
			if !ok {
				return nil, fmt.Errorf("$ref %s is not in the spec", ref)
			}
			outSection, ok := outComponents[section].(map[string]any)
			if !ok {
				outSection = map[string]any{}
				outComponents[section] = outSection
			}
			outSection[name] = c
			pending = append(pending, c)
		}
	}

	info, _ := spec["info"].(map[string]any)
	return map[string]any{
		"openapi": spec["openapi"],
		"info": map[string]any{
			"title":           info["title"],
			"version":         info["version"],
			"x-kflare-source": source,
		},
		"paths":      outPaths,
		"components": outComponents,
	}, nil
}

// localRefs returns the $refs inside node. Every one must point into the
// spec's components: the subset copies nothing else.
func localRefs(node any) ([]string, error) {
	var refs []string
	var walk func(any) error
	walk = func(n any) error {
		switch v := n.(type) {
		case map[string]any:
			for key, value := range v {
				if key == "$ref" {
					ref, ok := value.(string)
					if !ok || !strings.HasPrefix(ref, "#/components/") {
						return fmt.Errorf("$ref %v does not point into the spec's components", value)
					}
					refs = append(refs, ref)
					continue
				}
				if err := walk(value); err != nil {
					return err
				}
			}
		case []any:
			for _, item := range v {
				if err := walk(item); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return refs, walk(node)
}

// splitRef splits a $ref into the components section and name it points at,
// unescaping the JSON pointer.
func splitRef(ref string) (section, name string) {
	section, name, _ = strings.Cut(strings.TrimPrefix(ref, "#/components/"), "/")
	return section, strings.ReplaceAll(strings.ReplaceAll(name, "~1", "/"), "~0", "~")
}
