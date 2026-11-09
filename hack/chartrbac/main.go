/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

// Command chartrbac splits the controller's RBAC rules, which controller-gen
// writes into config/rbac/role.yaml from the kubebuilder markers, for the Helm
// chart's installations scoped to some namespaces (watchNamespaces, with
// rbac.namespaced). Such an installation's ClusterRole grants the rules of
// files/manager-cluster-rules.yaml, and a Role in each namespace it watches
// those of files/manager-namespaced-rules.yaml:
//
//   - The cluster rules are those on cluster-scoped resources, such as
//     CloudflareAccounts and Namespaces, and on Events: an Event about a
//     cluster-scoped object goes to the default namespace.
//   - The namespaced rules are all the others: Secrets, ConfigMaps and
//     kflare's namespaced kinds.
//
// Every other installation grants all of them with its ClusterRole, from
// files/manager-rules.yaml, which hack/sync-helm-chart.sh copies.
//
// A kflare kind's scope comes from its CRD. Any other resource must be listed
// in coreResources, so that a marker for a new one fails the command until
// someone decides where the chart grants it. Run it from the repository's
// root, through make helm, after controller-gen has written the role and the
// CRDs.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// The repository's paths, relative to its root.
const (
	rolePath            = "config/rbac/role.yaml"
	crdDir              = "config/crd/bases"
	clusterRulesPath    = "helm/kflare/files/manager-cluster-rules.yaml"
	namespacedRulesPath = "helm/kflare/files/manager-namespaced-rules.yaml"
)

// groupResource is a resource of an API group, without a subresource.
type groupResource struct {
	group, resource string
}

// coreResources says, for each resource of another API group than kflare's
// that the controller's markers name, whether the chart grants it across the
// cluster (true) or in each namespace an installation watches (false).
var coreResources = map[groupResource]bool{
	{"", "namespaces"}: true,
	{"", "configmaps"}: false,
	{"", "secrets"}:    false,
	// The Events of a cluster-scoped object, a CloudflareAccount's, go to the
	// default namespace, which a scoped installation need not watch.
	{"events.k8s.io", "events"}: true,
}

func main() {
	flags := flag.NewFlagSet("chartrbac", flag.ContinueOnError)
	root := flags.String("root", ".", "the repository whose chart to write the rules of")
	err := flags.Parse(os.Args[1:])
	if err == nil {
		err = run(*root)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "chartrbac:", err)
		os.Exit(1)
	}
}

// run writes the chart's cluster and namespaced rules in the repository at
// root.
func run(root string) error {
	roleYAML, err := os.ReadFile(filepath.Join(root, rolePath))
	if err != nil {
		return err
	}
	var role rbacv1.ClusterRole
	if err := yaml.UnmarshalStrict(roleYAML, &role); err != nil {
		return fmt.Errorf("%s: %w", rolePath, err)
	}
	scopes, err := kindScopes(filepath.Join(root, crdDir))
	if err != nil {
		return err
	}
	cluster, namespaced, err := split(role.Rules, scopes)
	if err != nil {
		return fmt.Errorf("%s: %w", rolePath, err)
	}
	for _, out := range []struct {
		path, what string
		rules      []rbacv1.PolicyRule
	}{
		{clusterRulesPath, "a scoped installation grants across the cluster", cluster},
		{namespacedRulesPath, "a scoped installation grants in each namespace it watches", namespaced},
	} {
		file, err := rulesFile(out.what, out.rules)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(root, out.path), file, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// kindScopes reads the CRDs in dir, and returns whether each of their kinds'
// resources is cluster-scoped.
func kindScopes(dir string) (map[groupResource]bool, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no CRDs in %s", dir)
	}
	scopes := map[groupResource]bool{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
		for {
			var crd apiextensionsv1.CustomResourceDefinition
			if err := decoder.Decode(&crd); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return nil, fmt.Errorf("%s: %w", file, err)
			}
			if crd.Spec.Names.Plural == "" {
				continue
			}
			scopes[groupResource{crd.Spec.Group, crd.Spec.Names.Plural}] = crd.Spec.Scope == apiextensionsv1.ClusterScoped
		}
	}
	return scopes, nil
}

// split returns rules divided into those on resources the chart grants
// across the cluster and those it grants in each watched namespace: a rule
// is split by its resources' scope, and by API group when it has several.
func split(rules []rbacv1.PolicyRule, kinds map[groupResource]bool) (cluster, namespaced []rbacv1.PolicyRule,
	err error) {
	for _, rule := range rules {
		if len(rule.NonResourceURLs) > 0 || len(rule.ResourceNames) > 0 {
			return nil, nil, fmt.Errorf("a rule names non-resource URLs or resource names, which no marker should: %+v",
				rule)
		}
		for _, group := range rule.APIGroups {
			var clusterResources, namespacedResources []string
			for _, resource := range rule.Resources {
				base, _, _ := strings.Cut(resource, "/")
				key := groupResource{group, base}
				wide, known := kinds[key]
				if !known {
					wide, known = coreResources[key]
				}
				if !known {
					return nil, nil, fmt.Errorf("resource %q of API group %q is neither a kflare kind nor in "+
						"hack/chartrbac's coreResources: decide whether the chart grants it across the cluster",
						resource, group)
				}
				if wide {
					clusterResources = append(clusterResources, resource)
				} else {
					namespacedResources = append(namespacedResources, resource)
				}
			}
			if len(clusterResources) > 0 {
				cluster = append(cluster, rbacv1.PolicyRule{APIGroups: []string{group}, Resources: clusterResources,
					Verbs: slices.Clone(rule.Verbs)})
			}
			if len(namespacedResources) > 0 {
				namespaced = append(namespaced, rbacv1.PolicyRule{APIGroups: []string{group},
					Resources: namespacedResources, Verbs: slices.Clone(rule.Verbs)})
			}
		}
	}
	return cluster, namespaced, nil
}

// rulesFile returns rules as a YAML list for the chart's rbac.yaml to include,
// under a header saying what they are.
func rulesFile(what string, rules []rbacv1.PolicyRule) ([]byte, error) {
	if len(rules) == 0 {
		return nil, errors.New("no rules for " + what)
	}
	body, err := yaml.Marshal(rules)
	if err != nil {
		return nil, err
	}
	header := fmt.Sprintf("# Generated by hack/chartrbac from %s. DO NOT EDIT.\n# The rules %s.\n", rolePath, what)
	return append([]byte(header), body...), nil
}
