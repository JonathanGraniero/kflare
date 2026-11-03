/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package gitopshealth

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// root is the repository's root, from this package's directory.
const root = "../.."

// loadChecks reads kflare's health checks, failing the test if it cannot.
func loadChecks(t *testing.T) *Checks {
	t.Helper()
	checks, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return checks
}

// readObject reads a resource's manifest as the API server returns it: unstructured content with int64 integers.
func readObject(t *testing.T, path string) *unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(data, obj); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return obj
}

// argoCDHealthTests is Argo CD's health_test.yaml.
type argoCDHealthTests struct {
	Tests []struct {
		HealthStatus ArgoCDHealth `json:"healthStatus"`
		InputPath    string       `json:"inputPath"`
	} `json:"tests"`
}

// fluxHealthTests is config/flux/health_test.yaml, the same shape for Flux's statuses.
type fluxHealthTests struct {
	Tests []struct {
		Status    string `json:"status"`
		InputPath string `json:"inputPath"`
	} `json:"tests"`
}

// readStrict decodes the YAML file at path into out, refusing unknown fields.
func readStrict(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.UnmarshalStrict(data, out); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// TestArgoCDHealthTests runs config/argocd/health_test.yaml as Argo CD runs the tests of its built-in checks: the
// script's health of each input must be the one the case expects, message included.
func TestArgoCDHealthTests(t *testing.T) {
	checks := loadChecks(t)
	dir := filepath.Join(root, "config", "argocd")
	var tests argoCDHealthTests
	readStrict(t, filepath.Join(dir, "health_test.yaml"), &tests)
	if len(tests.Tests) == 0 {
		t.Fatal("config/argocd/health_test.yaml holds no tests")
	}
	for _, tc := range tests.Tests {
		t.Run(tc.InputPath, func(t *testing.T) {
			got, err := checks.ArgoCD(readObject(t, filepath.Join(dir, tc.InputPath)))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.HealthStatus {
				t.Errorf("health is %+v, want %+v", got, tc.HealthStatus)
			}
		})
	}
}

// TestFluxHealthTests runs config/flux/health_test.yaml: Flux's status of each input must be the one the case
// expects. An Unknown status must come with the error Flux reports it for.
func TestFluxHealthTests(t *testing.T) {
	checks := loadChecks(t)
	dir := filepath.Join(root, "config", "flux")
	var tests fluxHealthTests
	readStrict(t, filepath.Join(dir, "health_test.yaml"), &tests)
	if len(tests.Tests) == 0 {
		t.Fatal("config/flux/health_test.yaml holds no tests")
	}
	for _, tc := range tests.Tests {
		t.Run(tc.InputPath, func(t *testing.T) {
			got, err := checks.Flux(readObject(t, filepath.Join(dir, tc.InputPath)))
			if got != tc.Status {
				t.Errorf("status is %s (error: %v), want %s", got, err, tc.Status)
			}
			if (err != nil) != (got == FluxUnknown) {
				t.Errorf("status %s with error %v: only Unknown comes with an error", got, err)
			}
		})
	}
}

// TestHealthTestsShareTheirInputs checks that the Argo CD and Flux tests run the same inputs, and that testdata holds
// no input neither runs.
func TestHealthTestsShareTheirInputs(t *testing.T) {
	var argoCD argoCDHealthTests
	readStrict(t, filepath.Join(root, "config", "argocd", "health_test.yaml"), &argoCD)
	var flux fluxHealthTests
	readStrict(t, filepath.Join(root, "config", "flux", "health_test.yaml"), &flux)

	argoCDInputs := make([]string, 0, len(argoCD.Tests))
	fluxInputs := make([]string, 0, len(flux.Tests))
	for _, tc := range argoCD.Tests {
		argoCDInputs = append(argoCDInputs, filepath.Join("config", "argocd", tc.InputPath))
	}
	for _, tc := range flux.Tests {
		fluxInputs = append(fluxInputs, filepath.Join("config", "flux", tc.InputPath))
	}
	if !reflect.DeepEqual(argoCDInputs, fluxInputs) {
		t.Errorf("the Argo CD tests run %v, and the Flux tests %v", argoCDInputs, fluxInputs)
	}

	files, err := filepath.Glob(filepath.Join(root, "config", "argocd", "testdata", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		rel, err := filepath.Rel(root, file)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, input := range argoCDInputs {
			found = found || filepath.Clean(input) == rel
		}
		if !found {
			t.Errorf("no test runs %s", rel)
		}
	}
}

// crdsByKind reads the CRDs in config/crd/bases, by kind.
func crdsByKind(t *testing.T) map[string]*apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "config", "crd", "bases", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	crds := map[string]*apiextensionsv1.CustomResourceDefinition{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(data, crd); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		crds[crd.Spec.Names.Kind] = crd
	}
	if len(crds) == 0 {
		t.Fatal("no CRDs in config/crd/bases")
	}
	return crds
}

// TestHealthTestInputsAreValidResources creates each input on an API server with kflare's CRDs (envtest, which
// make test provides), refusing unknown fields, and writes its status the same way, so the inputs are objects kflare
// could hold, as Argo CD's guidelines ask: every field is one its kind's schema has, of the type, form and bounds the
// schema and its CEL rules ask for.
func TestHealthTestInputsAreValidResources(t *testing.T) {
	crds := crdsByKind(t)
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest (make test sets KUBEBUILDER_ASSETS): %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	strict := client.FieldValidation(metav1.FieldValidationStrict)

	files, err := filepath.Glob(filepath.Join(root, "config", "argocd", "testdata", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			obj := readObject(t, file)
			crd, ok := crds[obj.GetKind()]
			if !ok {
				t.Fatalf("no CRD for kind %q", obj.GetKind())
			}
			if obj.GetAPIVersion() != Group+"/v1alpha1" || crd.Spec.Group != Group {
				t.Fatalf("apiVersion is %s, and the CRD's group %s", obj.GetAPIVersion(), crd.Spec.Group)
			}
			if obj.GetGeneration() < 1 {
				t.Error("metadata.generation is not set: the API server sets it from 1")
			}
			if ns := obj.GetNamespace(); ns != "" {
				namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
				if err := c.Create(ctx, namespace); err != nil && !apierrors.IsAlreadyExists(err) {
					t.Fatal(err)
				}
			}

			// The API server sets what metadata it owns itself, and takes status through its subresource alone.
			created := obj.DeepCopy()
			created.SetResourceVersion("")
			created.SetUID("")
			created.SetCreationTimestamp(metav1.Time{})
			if err := c.Create(ctx, created, strict); err != nil {
				t.Fatalf("the API server refuses it: %v", err)
			}
			t.Cleanup(func() {
				created.SetFinalizers(nil)
				if err := c.Update(context.Background(), created); err != nil {
					t.Error(err)
				}
				if err := c.Delete(context.Background(), created); err != nil && !apierrors.IsNotFound(err) {
					t.Error(err)
				}
			})
			if status, found := obj.Object["status"]; found {
				created.Object["status"] = status
				if err := c.Status().Update(ctx, created, strict); err != nil {
					t.Errorf("the API server refuses its status: %v", err)
				}
			}
		})
	}
}

// TestHealthChecksCoverEveryKind checks that both checks judge every kind of kflare's: Argo CD's through the wildcard
// key the deploy guide sets, Flux's through an entry without a kind.
func TestHealthChecksCoverEveryKind(t *testing.T) {
	checks := loadChecks(t)
	for kind, crd := range crdsByKind(t) {
		if crd.Spec.Group != Group {
			t.Errorf("%s's group is %s, which the health checks do not cover", kind, crd.Spec.Group)
		}
		for _, version := range crd.Spec.Versions {
			gvk := apiextensionsv1.SchemeGroupVersion.WithKind(kind)
			gvk.Group, gvk.Version = crd.Spec.Group, version.Name
			if !checks.FluxCheck.Applies(gvk) {
				t.Errorf("the Flux health check does not apply to %s", gvk)
			}
		}
	}
}

// deployGuideYAML returns the objects of docs/deploy.md's YAML code blocks, decoded.
func deployGuideYAML(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "docs", "deploy.md"))
	if err != nil {
		t.Fatal(err)
	}
	var objects []*unstructured.Unstructured
	var block strings.Builder
	inBlock := false
	for line := range strings.Lines(string(data)) {
		trimmed := strings.TrimSpace(line)
		switch {
		case !inBlock && trimmed == "```yaml":
			inBlock = true
			block.Reset()
		case inBlock && trimmed == "```":
			inBlock = false
			decoder := k8syaml.NewYAMLOrJSONDecoder(strings.NewReader(block.String()), 4096)
			for {
				var doc any
				if err := decoder.Decode(&doc); err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					t.Fatalf("a YAML block of docs/deploy.md does not decode: %v", err)
				}
				// Blocks of Helm values or other fragments hold no object.
				if content, ok := doc.(map[string]any); ok && content["kind"] != nil {
					objects = append(objects, &unstructured.Unstructured{Object: content})
				}
			}
		case inBlock:
			block.WriteString(line)
		}
	}
	return objects
}

// TestDeployGuideHoldsTheHealthChecks checks that docs/deploy.md sets the health checks the tests run: argocd-cm's
// resource.customizations holds config/argocd/health.lua for kflare.dev/*, and every Kustomization and HelmRelease that
// sets healthCheckExprs sets config/flux/health-check-exprs.yaml's entry.
func TestDeployGuideHoldsTheHealthChecks(t *testing.T) {
	checks := loadChecks(t)
	var argoCD, flux int
	for _, obj := range deployGuideYAML(t) {
		switch {
		case obj.GetKind() == "ConfigMap" && obj.GetName() == "argocd-cm":
			argoCD++
			customizations, _, _ := unstructured.NestedString(obj.Object, "data", "resource.customizations")
			var byKey map[string]struct {
				HealthLua string `json:"health.lua"`
			}
			if err := yaml.UnmarshalStrict([]byte(customizations), &byKey); err != nil {
				t.Fatalf("argocd-cm's resource.customizations: %v", err)
			}
			if len(byKey) != 1 {
				t.Errorf("argocd-cm's resource.customizations holds %d keys, not kflare.dev/* alone", len(byKey))
			}
			if got := byKey[Group+"/*"].HealthLua; got != checks.ArgoCDScript {
				t.Errorf("argocd-cm's health check for %s/* is not %s:\n%s", Group, ArgoCDScriptPath,
					lineDiff(checks.ArgoCDScript, got))
			}
		case obj.GetKind() == "Kustomization" || obj.GetKind() == "HelmRelease":
			exprs, found, _ := unstructured.NestedSlice(obj.Object, "spec", "healthCheckExprs")
			if !found {
				continue
			}
			flux++
			data, err := yaml.Marshal(exprs)
			if err != nil {
				t.Fatal(err)
			}
			var entries []FluxHealthCheck
			if err := yaml.UnmarshalStrict(data, &entries); err != nil {
				t.Fatalf("%s %s's healthCheckExprs: %v", obj.GetKind(), obj.GetName(), err)
			}
			if !reflect.DeepEqual(entries, []FluxHealthCheck{checks.FluxCheck}) {
				t.Errorf("%s %s's healthCheckExprs are %+v, not %s's %+v", obj.GetKind(), obj.GetName(), entries,
					FluxHealthChecksPath, checks.FluxCheck)
			}
		}
	}
	if argoCD != 1 {
		t.Errorf("docs/deploy.md sets argocd-cm %d times, not once", argoCD)
	}
	if flux != 2 {
		t.Errorf("docs/deploy.md sets healthCheckExprs %d times, not on a Kustomization and a HelmRelease", flux)
	}
}

// lineDiff shows the first line where got differs from want.
func lineDiff(want, got string) string {
	wantLines, gotLines := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := range max(len(wantLines), len(gotLines)) {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			return fmt.Sprintf("line %d is %q, want %q", i+1, g, w)
		}
	}
	return "they differ in their line endings"
}

// readyAt returns a resource of generation 2 whose Ready condition has status and reason, observed at observed.
func readyAt(status, reason string, observed int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": Group + "/v1alpha1",
		"kind":       "DNSRecord",
		"metadata":   map[string]any{"name": "www", "namespace": "web", "generation": int64(2)},
		"status": map[string]any{"conditions": []any{map[string]any{
			"type": "Ready", "status": status, "reason": reason, "message": "the message",
			"observedGeneration": observed, "lastTransitionTime": "2026-10-31T09:14:37Z",
		}}},
	}}
}

// readyReasons holds every reason of kflare's Ready condition, with its status and the health Argo CD gives a
// resource reporting it for its current generation. A new reason goes here, and, if a person must act on it, into
// health.lua's needsPerson and health-check-exprs.yaml's failed list. TestEveryReadyReasonIsClassified finds those
// the controllers set but for references' <Kind>NotFound and <Kind>NotReady, which wait for the resource, as the
// ones listed here do.
var readyReasons = []struct {
	status, reason, health string
}{
	{"True", "Synced", ArgoCDHealthy},
	{"True", "Validated", ArgoCDHealthy},
	{"Unknown", "Paused", ArgoCDSuspended},
	{"True", "Paused", ArgoCDSuspended},
	{"False", "Paused", ArgoCDSuspended},

	// Cloudflare refused the request, or kflare cannot build a client from the token.
	{"False", "TerminalError", ArgoCDDegraded},
	{"False", "InvalidToken", ArgoCDDegraded},
	// Retried with back-off, or after a rate limit's wait.
	{"False", "APIError", ArgoCDProgressing},
	{"False", "RateLimited", ArgoCDProgressing},

	// Credentials (credentials.go) and the CloudflareAccount's own checks.
	{"False", "AccountNotFound", ArgoCDProgressing},
	{"False", "AccountNotReady", ArgoCDProgressing},
	{"False", "SecretNotFound", ArgoCDProgressing},
	{"False", "TokenKeyMissing", ArgoCDDegraded},
	{"False", "AccountNotAllowed", ArgoCDDegraded},
	{"False", "InvalidAllowedNamespaces", ArgoCDDegraded},

	// Parents and references.
	{"False", "ZoneNotFound", ArgoCDProgressing},
	{"False", "ZoneNotReady", ArgoCDProgressing},
	{"False", "TunnelNotFound", ArgoCDProgressing},
	{"False", "TunnelNotReady", ArgoCDProgressing},
	{"False", "WorkerScriptNotFound", ArgoCDProgressing},
	{"False", "WorkerScriptNotReady", ArgoCDProgressing},
	{"False", "KVNamespaceNotFound", ArgoCDProgressing},
	{"False", "KVNamespaceNotReady", ArgoCDProgressing},
	{"False", "R2BucketNotFound", ArgoCDProgressing},
	{"False", "R2BucketNotReady", ArgoCDProgressing},
	{"False", "AccountMismatch", ArgoCDDegraded},
	{"False", "R2BucketJurisdictionUnsupported", ArgoCDDegraded},

	// A WorkerScript's script and bindings.
	{"False", "ScriptConfigMapNotFound", ArgoCDProgressing},
	{"False", "ScriptKeyMissing", ArgoCDDegraded},
	{"False", "BindingSecretNotFound", ArgoCDProgressing},
	{"False", "BindingSecretKeyMissing", ArgoCDDegraded},

	// Adoption (adoption.go).
	{"False", "AlreadyExists", ArgoCDDegraded},
	{"False", "NotFound", ArgoCDDegraded},
	{"False", "InvalidAdoptionPolicy", ArgoCDDegraded},

	// Another resource manages the object, or configures the same settings.
	{"False", "NameConflict", ArgoCDDegraded},
	{"False", "TitleConflict", ArgoCDDegraded},
	{"False", "PatternConflict", ArgoCDDegraded},
	{"False", "RulesetConflict", ArgoCDDegraded},
	{"False", "CredentialsSecretConflict", ArgoCDDegraded},
	{"False", "AlreadyConfigured", ArgoCDDegraded},
	{"False", "TunnelAlreadyConfigured", ArgoCDDegraded},

	// Steps that finish by themselves.
	{"False", "ReplacingItems", ArgoCDProgressing},

	// Deletion, which Argo CD reports as Progressing whatever the check says: dependents being deleted, and
	// Cloudflare refusing to delete an object in use or a bucket that holds objects, which kflare retries.
	{"False", "InUse", ArgoCDProgressing},
	{"False", "ListInUse", ArgoCDProgressing},
	{"False", "BucketNotEmpty", ArgoCDProgressing},
}

// fluxStatusFor is the status Flux's check gives where Argo CD's gives health, for a resource whose Ready condition
// has status: Flux has no Suspended, and goes by Ready alone for a paused resource.
func fluxStatusFor(health, status string) string {
	switch health {
	case ArgoCDHealthy:
		return FluxCurrent
	case ArgoCDDegraded:
		return FluxFailed
	case ArgoCDSuspended:
		if status == "True" {
			return FluxCurrent
		}
	}
	return FluxInProgress
}

// TestReadyReasons checks the health both checks give every reason kflare reports, for the current generation and
// for an earlier one, which is Progressing until kflare reconciles the current spec, but for a paused resource.
func TestReadyReasons(t *testing.T) {
	checks := loadChecks(t)
	for _, r := range readyReasons {
		t.Run(r.status+"/"+r.reason, func(t *testing.T) {
			current := readyAt(r.status, r.reason, 2)
			health, err := checks.ArgoCD(current)
			if err != nil {
				t.Fatal(err)
			}
			if health.Status != r.health || health.Message != "the message" {
				t.Errorf("Argo CD's health is %+v, want %s with the condition's message", health, r.health)
			}
			if got, err := checks.Flux(current); err != nil || got != fluxStatusFor(r.health, r.status) {
				t.Errorf("Flux's status is %s (error: %v), want %s", got, err, fluxStatusFor(r.health, r.status))
			}

			earlier := readyAt(r.status, r.reason, 1)
			wantHealth, wantFlux := ArgoCDProgressing, FluxInProgress
			if r.reason == "Paused" {
				wantHealth = ArgoCDSuspended
			}
			if health, err := checks.ArgoCD(earlier); err != nil || health.Status != wantHealth {
				t.Errorf("Argo CD's health for an earlier generation is %+v (error: %v), want %s", health, err,
					wantHealth)
			}
			if got, err := checks.Flux(earlier); err != nil || got != wantFlux {
				t.Errorf("Flux's status for an earlier generation is %s (error: %v), want %s", got, err, wantFlux)
			}
		})
	}
}

// TestHealthWithoutReady checks a resource without a Ready condition: kflare has not reconciled it, and Argo CD
// waits for it, as Flux does once the resource has a status.
func TestHealthWithoutReady(t *testing.T) {
	checks := loadChecks(t)
	obj := readyAt("True", "Synced", 2)
	if err := unstructured.SetNestedSlice(obj.Object, []any{map[string]any{
		"type": "Other", "status": "True", "reason": "Synced", "message": "", "observedGeneration": int64(2),
		"lastTransitionTime": "2026-10-31T09:14:37Z",
	}}, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	if health, err := checks.ArgoCD(obj); err != nil || health.Status != ArgoCDProgressing {
		t.Errorf("Argo CD's health is %+v (error: %v), want Progressing", health, err)
	}
	if got, err := checks.Flux(obj); err != nil || got != FluxInProgress {
		t.Errorf("Flux's status is %s (error: %v), want InProgress", got, err)
	}
	unstructured.RemoveNestedField(obj.Object, "status", "conditions")
	if health, err := checks.ArgoCD(obj); err != nil || health.Status != ArgoCDProgressing {
		t.Errorf("Argo CD's health without conditions is %+v (error: %v), want Progressing", health, err)
	}
	if got, err := checks.Flux(obj); got != FluxUnknown || err == nil {
		t.Errorf("Flux's status without conditions is %s (error: %v), want Unknown with an error", got, err)
	}
}

// readyReasonCalls are the status helpers of internal/controller that take a Ready reason, with the index of its
// argument.
var readyReasonCalls = map[string]int{
	"updateNotReady":       3,
	"updateReady":          3,
	"readyWithDriftCheck":  3,
	"updateReadyCondition": 4,
	"setReady":             2,
	"recordReadyChange":    2,
}

// readyReasonFields are the fields of internal/controller's types that hold a Ready reason: a conditionError's or a
// condition's, a kind's conflictReason, and a deletion blocker's. A kind's conflicts map holds them as its values.
var readyReasonFields = map[string]bool{"Reason": true, "reason": true, "conflictReason": true}

// controllerReadyReasons returns the Ready reasons internal/controller's code sets, by reading it: the string
// constants and literals passed to the status helpers, or set in the fields, above. A reason built at run time, as a
// reference's ref.kind + "NotFound" is, is not found.
func controllerReadyReasons(t *testing.T) map[string][]string {
	t.Helper()
	dir := filepath.Join(root, "internal", "controller")
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed []*ast.File
	constants := map[string]string{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, f)
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				for i, name := range value.Names {
					if i < len(value.Values) {
						if s, ok := stringLiteral(value.Values[i]); ok {
							constants[name.Name] = s
						}
					}
				}
			}
		}
	}

	reasons := map[string][]string{}
	add := func(expr ast.Expr) {
		value, ok := stringLiteral(expr)
		if ident, isIdent := expr.(*ast.Ident); !ok && isIdent {
			value, ok = constants[ident.Name]
		}
		if ok {
			reasons[value] = append(reasons[value], fset.Position(expr.Pos()).String())
		}
	}
	for _, f := range parsed {
		ast.Inspect(f, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.CallExpr:
				if fun, ok := n.Fun.(*ast.Ident); ok {
					if i, ok := readyReasonCalls[fun.Name]; ok && i < len(n.Args) {
						add(n.Args[i])
					}
				}
			case *ast.KeyValueExpr:
				key, ok := n.Key.(*ast.Ident)
				switch {
				case !ok:
				case readyReasonFields[key.Name]:
					add(n.Value)
				case key.Name == "conflicts":
					if values, ok := n.Value.(*ast.CompositeLit); ok {
						for _, elt := range values.Elts {
							if kv, ok := elt.(*ast.KeyValueExpr); ok {
								add(kv.Value)
							}
						}
					}
				}
			}
			return true
		})
	}
	return reasons
}

// stringLiteral returns the value of a string literal.
func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	return value, err == nil
}

// TestEveryReadyReasonIsClassified checks that readyReasons holds every Ready reason the controllers set, so that a
// new one is not left to the scripts' default, Progressing, without a decision: one a person must act on goes into
// both checks' lists. It also checks that the reader still finds the reasons it is known to, so that a change to the
// status helpers cannot leave it finding none.
func TestEveryReadyReasonIsClassified(t *testing.T) {
	found := controllerReadyReasons(t)
	classified := map[string]bool{}
	for _, r := range readyReasons {
		classified[r.reason] = true
	}
	for reason, at := range found {
		if !classified[reason] {
			t.Errorf("the Ready reason %s (%s) is not in readyReasons: decide whether a person must act on it",
				reason, strings.Join(at, ", "))
		}
	}
	for reason := range classified {
		_, set := found[reason]
		reference := strings.HasSuffix(reason, "NotFound") || strings.HasSuffix(reason, "NotReady")
		if !set && !reference {
			t.Errorf("readyReasons holds %s, which no controller sets", reason)
		}
	}
	for _, known := range []string{"Synced", "Validated", "Paused", "TerminalError", "APIError", "RateLimited",
		"AccountNotFound", "AlreadyExists", "NameConflict", "TitleConflict", "PatternConflict", "RulesetConflict",
		"AlreadyConfigured", "ListInUse", "BucketNotEmpty", "InUse"} {
		if _, ok := found[known]; !ok {
			t.Errorf("reading internal/controller found no %s: the reader misses how the controllers set it", known)
		}
	}
}

// TestDegradedNeedsReadyFalse checks that a reason a person must act on makes a resource Degraded and Failed only
// with Ready False, as the deploy guide says: kflare reports those reasons with False only, and a check that went by
// the reason alone would fail an Unknown or True condition that carried one.
func TestDegradedNeedsReadyFalse(t *testing.T) {
	checks := loadChecks(t)
	for _, tc := range []struct{ status, health, flux string }{
		{"Unknown", ArgoCDProgressing, FluxInProgress},
		{"True", ArgoCDHealthy, FluxCurrent},
	} {
		obj := readyAt(tc.status, "TerminalError", 2)
		if health, err := checks.ArgoCD(obj); err != nil || health.Status != tc.health {
			t.Errorf("Ready %s with TerminalError: Argo CD's health is %+v (error: %v), want %s", tc.status, health,
				err, tc.health)
		}
		if got, err := checks.Flux(obj); err != nil || got != tc.flux {
			t.Errorf("Ready %s with TerminalError: Flux's status is %s (error: %v), want %s", tc.status, got, err,
				tc.flux)
		}
	}
}
