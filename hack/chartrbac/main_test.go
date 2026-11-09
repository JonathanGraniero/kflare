/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// repoRoot is the repository's root, from this package's directory.
const repoRoot = "../.."

// TestRulesAreCurrent runs the command on a copy of the repository's role and
// CRDs and expects the committed rules back: run make helm after changing a
// kubebuilder RBAC marker or a CRD's scope.
func TestRulesAreCurrent(t *testing.T) {
	root := t.TempDir()
	copyFile(t, filepath.Join(repoRoot, rolePath), filepath.Join(root, rolePath))
	if err := os.CopyFS(filepath.Join(root, crdDir), os.DirFS(filepath.Join(repoRoot, crdDir))); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(clusterRulesPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run(root); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, file := range []string{clusterRulesPath, namespacedRulesPath} {
		got, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(repoRoot, file))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s is not what make helm writes: run make helm", file)
		}
	}
}

// grant is one verb on one resource of an API group.
type grant struct {
	group, resource, verb string
}

// grants returns every verb on every resource rules give.
func grants(rules []rbacv1.PolicyRule) map[grant]bool {
	all := map[grant]bool{}
	for _, rule := range rules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, verb := range rule.Verbs {
					all[grant{group, resource, verb}] = true
				}
			}
		}
	}
	return all
}

// TestCommittedRulesSplitTheRole checks the committed rules against the role
// they come from: together they grant exactly what it grants, no grant is in
// both, Secrets and ConfigMaps are granted only per namespace, and
// CloudflareAccounts, Namespaces and Events only across the cluster.
func TestCommittedRulesSplitTheRole(t *testing.T) {
	var role rbacv1.ClusterRole
	readYAML(t, filepath.Join(repoRoot, rolePath), &role)
	var cluster, namespaced []rbacv1.PolicyRule
	readYAML(t, filepath.Join(repoRoot, clusterRulesPath), &cluster)
	readYAML(t, filepath.Join(repoRoot, namespacedRulesPath), &namespaced)

	clusterGrants, namespacedGrants := grants(cluster), grants(namespaced)
	union := map[grant]bool{}
	for g := range clusterGrants {
		if namespacedGrants[g] {
			t.Errorf("%+v is granted both across the cluster and per namespace", g)
		}
		union[g] = true
	}
	for g := range namespacedGrants {
		union[g] = true
	}
	if want := grants(role.Rules); !reflect.DeepEqual(union, want) {
		t.Errorf("the split rules grant %v, the role %v", union, want)
	}

	for g := range clusterGrants {
		if g.resource == "secrets" || g.resource == "configmaps" {
			t.Errorf("%+v is granted across the cluster", g)
		}
	}
	for g := range namespacedGrants {
		base, _, _ := strings.Cut(g.resource, "/")
		if base == "cloudflareaccounts" || base == "namespaces" || base == "events" {
			t.Errorf("%+v is granted only in watched namespaces", g)
		}
	}
	for _, want := range []grant{
		{"", "secrets", "get"}, {"", "secrets", "patch"}, {"", "configmaps", "watch"},
		{"kflare.dev", "zones", "update"}, {"kflare.dev", "dnsrecords/status", "update"},
	} {
		if !namespacedGrants[want] {
			t.Errorf("%+v is not granted per namespace", want)
		}
	}
	for _, want := range []grant{
		{"kflare.dev", "cloudflareaccounts", "watch"}, {"kflare.dev", "cloudflareaccounts/status", "update"},
		{"kflare.dev", "cloudflareaccounts/finalizers", "update"}, {"", "namespaces", "list"},
		{"events.k8s.io", "events", "create"},
	} {
		if !clusterGrants[want] {
			t.Errorf("%+v is not granted across the cluster", want)
		}
	}
}

func TestSplit(t *testing.T) {
	kinds := map[groupResource]bool{{"kflare.dev", "zones"}: false, {"kflare.dev", "cloudflareaccounts"}: true}
	verbs := []string{"get", "list"}
	tests := []struct {
		name                string
		rules               []rbacv1.PolicyRule
		cluster, namespaced []rbacv1.PolicyRule
		err                 string
	}{{
		name: "a rule whose resources have both scopes, subresources by their resource's",
		rules: []rbacv1.PolicyRule{{APIGroups: []string{"kflare.dev"},
			Resources: []string{"cloudflareaccounts/status", "zones", "zones/status"}, Verbs: verbs}},
		cluster: []rbacv1.PolicyRule{{APIGroups: []string{"kflare.dev"},
			Resources: []string{"cloudflareaccounts/status"}, Verbs: verbs}},
		namespaced: []rbacv1.PolicyRule{{APIGroups: []string{"kflare.dev"},
			Resources: []string{"zones", "zones/status"}, Verbs: verbs}},
	}, {
		name:  "a rule of the core group, by coreResources",
		rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps", "namespaces"}, Verbs: verbs}},
		cluster: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"namespaces"},
			Verbs: verbs}},
		namespaced: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"},
			Verbs: verbs}},
	}, {
		name:  "Events, granted across the cluster",
		rules: []rbacv1.PolicyRule{{APIGroups: []string{"events.k8s.io"}, Resources: []string{"events"}, Verbs: verbs}},
		cluster: []rbacv1.PolicyRule{{APIGroups: []string{"events.k8s.io"}, Resources: []string{"events"},
			Verbs: verbs}},
	}, {
		name:  "a rule of several API groups, whose resources are classified in each",
		rules: []rbacv1.PolicyRule{{APIGroups: []string{"", "kflare.dev"}, Resources: []string{"secrets"}, Verbs: verbs}},
		err:   `resource "secrets" of API group "kflare.dev"`,
	}, {
		name: "a resource of another group",
		rules: []rbacv1.PolicyRule{{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"},
			Verbs: verbs}},
		err: `resource "leases" of API group "coordination.k8s.io" is neither a kflare kind nor in`,
	}, {
		name:  "resource names",
		rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"x"}}},
		err:   "names non-resource URLs or resource names",
	}, {
		name:  "non-resource URLs",
		rules: []rbacv1.PolicyRule{{NonResourceURLs: []string{"/metrics"}, Verbs: []string{"get"}}},
		err:   "names non-resource URLs or resource names",
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cluster, namespaced, err := split(tc.rules, kinds)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("got error %v, want one containing %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cluster, tc.cluster) || !reflect.DeepEqual(namespaced, tc.namespaced) {
				t.Errorf("got cluster %+v and namespaced %+v, want %+v and %+v", cluster, namespaced, tc.cluster,
					tc.namespaced)
			}
		})
	}
}

// TestSplitByGroup checks a rule of two groups whose resources both know.
func TestSplitByGroup(t *testing.T) {
	kinds := map[groupResource]bool{{"a.example", "things"}: true, {"b.example", "things"}: false}
	rules := []rbacv1.PolicyRule{{APIGroups: []string{"a.example", "b.example"}, Resources: []string{"things"},
		Verbs: []string{"get"}}}
	cluster, namespaced, err := split(rules, kinds)
	if err != nil {
		t.Fatal(err)
	}
	wantCluster := []rbacv1.PolicyRule{{APIGroups: []string{"a.example"}, Resources: []string{"things"},
		Verbs: []string{"get"}}}
	wantNamespaced := []rbacv1.PolicyRule{{APIGroups: []string{"b.example"}, Resources: []string{"things"},
		Verbs: []string{"get"}}}
	if !reflect.DeepEqual(cluster, wantCluster) || !reflect.DeepEqual(namespaced, wantNamespaced) {
		t.Errorf("got cluster %+v and namespaced %+v", cluster, namespaced)
	}
}

func TestKindScopes(t *testing.T) {
	dir := t.TempDir()
	write := func(name, plural, scope string) {
		crd := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nspec:\n  group: kflare.dev\n" +
			"  names:\n    plural: " + plural + "\n  scope: " + scope + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(crd), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("accounts.yaml", "cloudflareaccounts", "Cluster")
	write("zones.yaml", "zones", "Namespaced")
	scopes, err := kindScopes(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[groupResource]bool{{"kflare.dev", "cloudflareaccounts"}: true, {"kflare.dev", "zones"}: false}
	if !reflect.DeepEqual(scopes, want) {
		t.Errorf("got %v, want %v", scopes, want)
	}

	if _, err := kindScopes(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no CRDs") {
		t.Errorf("an empty directory gave %v", err)
	}
}

// TestRunRefuses checks that the command writes nothing it cannot stand behind:
// it fails on a role or a CRD it cannot read, a resource it cannot place, and
// a chart it cannot write to, and skips a document that is not a CRD.
func TestRunRefuses(t *testing.T) {
	role := "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: manager-role\nrules:\n" +
		"- apiGroups:\n  - kflare.dev\n  resources:\n  - zones\n  verbs:\n  - get\n"
	crd := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nspec:\n  group: kflare.dev\n" +
		"  names:\n    plural: zones\n  scope: Namespaced\n"
	// setup writes a repository holding role and, under crdDir, crds, by
	// file name; chart makes the chart's files directory.
	setup := func(t *testing.T, role string, crds map[string]string, chart bool) string {
		t.Helper()
		root := t.TempDir()
		write := func(path, content string) {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if role != "" {
			write(rolePath, role)
		}
		for name, content := range crds {
			write(filepath.Join(crdDir, name), content)
		}
		if chart {
			if err := os.MkdirAll(filepath.Join(root, filepath.Dir(clusterRulesPath)), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}

	for _, tc := range []struct {
		name, role string
		crds       map[string]string
		chart      bool
		err        string
	}{
		{"no role", "", map[string]string{"zones.yaml": crd}, true, "role.yaml"},
		{"a role with a field ClusterRole does not have", role + "extra: true\n",
			map[string]string{"zones.yaml": crd}, true, rolePath},
		{"no CRDs", role, nil, true, "no CRDs"},
		{"a CRD that does not decode", role, map[string]string{"zones.yaml": crd, "bad.yaml": "spec: [\n"}, true,
			"bad.yaml"},
		{"a resource it cannot place", role + "- apiGroups:\n  - apps\n  resources:\n  - deployments\n  verbs:\n" +
			"  - get\n", map[string]string{"zones.yaml": crd}, true, rolePath},
		{"no rules on cluster-scoped resources", role, map[string]string{"zones.yaml": crd}, true,
			"no rules for a scoped installation grants across the cluster"},
		{"a chart it cannot write to", role + "- apiGroups:\n  - \"\"\n  resources:\n  - namespaces\n  verbs:\n" +
			"  - get\n", map[string]string{"zones.yaml": crd}, false, "manager-cluster-rules.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := run(setup(t, tc.role, tc.crds, tc.chart))
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("got error %v, want one containing %q", err, tc.err)
			}
		})
	}

	t.Run("a document that is not a CRD", func(t *testing.T) {
		withNamespaces := role + "- apiGroups:\n  - \"\"\n  resources:\n  - namespaces\n  verbs:\n  - get\n"
		root := setup(t, withNamespaces, map[string]string{"zones.yaml": "---\n" + crd + "---\nkind: ConfigMap\n"}, true)
		if err := run(root); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRulesFile(t *testing.T) {
	if _, err := rulesFile("nothing", nil); err == nil {
		t.Error("no rules gave no error")
	}
	file, err := rulesFile("of a test", []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"},
		Verbs: []string{"get"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := "# Generated by hack/chartrbac from config/rbac/role.yaml. DO NOT EDIT.\n# The rules of a test.\n" +
		"- apiGroups:\n  - \"\"\n  resources:\n  - secrets\n  verbs:\n  - get\n"
	if string(file) != want {
		t.Errorf("got\n%s\nwant\n%s", file, want)
	}
}

func readYAML(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.UnmarshalStrict(data, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
