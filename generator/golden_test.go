/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// boilerplate is the license header the repository's generated files carry.
var boilerplate = filepath.Join("..", "hack", "boilerplate.go.txt")

// committedSpec is the OpenAPI subset make codegen reads.
var committedSpec = filepath.Join("openapi", "openapi.json")

// testHeader is the license header of the golden files.
const testHeader = "// Test header."

// loadTestSpec loads the committed OpenAPI subset.
func loadTestSpec(t *testing.T) *apiSpec {
	t.Helper()
	spec, err := loadSpec(committedSpec)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// refusedControllers are the test kinds whose controllers kflare-gen refuses,
// with what it says: the shared CRUD reconciler cannot drive their services
// yet.
var refusedControllers = map[string]string{
	"TestList": "New returns *rules.ListNewResponse and Get *rules.ListGetResponse: " +
		"the shared CRUD reconciler needs one object type yet",
	"TestIPList": "New returns *rules.ListNewResponse and Get *rules.ListGetResponse: " +
		"the shared CRUD reconciler needs one object type yet",
}

// goldenFiles generates the kinds of testdata/generator.yaml, each from a
// real cloudflare-go v7 service, by golden file name: a kind's types, and the
// controller, envtest table and sample of a kind whose controller kflare-gen
// does not refuse. It checks that it refuses the others, and why. Its rule
// kinds, each from a real rule type, and its settings kinds get all four.
func goldenFiles(t *testing.T) map[string][]byte {
	t.Helper()
	cfg, err := loadConfig(filepath.Join("testdata", "generator.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	spec := loadTestSpec(t)
	files := map[string][]byte{}
	for _, entry := range cfg.Kinds {
		svc := loadTestService(t, entry.Service)
		k, err := buildKind(entry, svc, spec)
		if err != nil {
			t.Fatal(err)
		}
		c, err := buildController(k, entry, svc, spec)
		if want, refused := refusedControllers[entry.Kind]; refused {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s's controller: err = %v, want one containing %q", entry.Kind, err, want)
			}
			src, err := emitTypes(k, testHeader)
			if err != nil {
				t.Fatal(err)
			}
			files[filepath.Base(typesFile(strings.ToLower(entry.Kind)))] = src
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out := &outputs{files: map[string][]byte{}}
		if err := out.addKind(c, testHeader); err != nil {
			t.Fatal(err)
		}
		for path, src := range out.files {
			files[filepath.Base(path)] = src
		}
	}
	for _, entry := range cfg.RuleKinds {
		k, err := buildRuleKind(entry, testRuleLoader(t), spec)
		if err != nil {
			t.Fatal(err)
		}
		out := &outputs{files: map[string][]byte{}}
		if err := out.addRuleKind(k, testHeader); err != nil {
			t.Fatal(err)
		}
		for path, src := range out.files {
			files[filepath.Base(path)] = src
		}
	}
	for _, entry := range cfg.SettingsKinds {
		k, err := buildSettingsKind(entry, loadTestSettings(t, entry.Service), spec)
		if err != nil {
			t.Fatal(err)
		}
		out := &outputs{files: map[string][]byte{}}
		if err := out.addSettingsKind(k, testHeader); err != nil {
			t.Fatal(err)
		}
		for path, src := range out.files {
			files[filepath.Base(path)] = src
		}
	}
	return files
}

// TestKindsPutAnBeforeAVowel generates the test rule and settings kinds and the
// committed ones, between them a phase map, unions, maps and lists of objects,
// under names that start with a vowel, and checks that no file they get says
// "a" before one.
func TestKindsPutAnBeforeAVowel(t *testing.T) {
	var ruleKinds []ruleKindConfig
	var settingsKinds []settingsKindConfig
	for _, path := range []string{filepath.Join("testdata", "generator.yaml"), "generator.yaml"} {
		cfg, err := loadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		ruleKinds = append(ruleKinds, cfg.RuleKinds...)
		settingsKinds = append(settingsKinds, cfg.SettingsKinds...)
	}
	spec := loadTestSpec(t)
	check := func(kind string, files map[string][]byte) {
		for path, src := range files {
			// The docs are wrapped, so read them as one line of words.
			text := " " + strings.Join(strings.Fields(strings.ReplaceAll(string(src), "//", "")), " ")
			if strings.Contains(text, " a "+kind) {
				t.Errorf("%s says \"a %s\"", filepath.Base(path), kind)
			}
		}
	}
	for _, entry := range ruleKinds {
		entry.Kind = "Every" + strings.TrimPrefix(entry.Kind, "Test")
		k, err := buildRuleKind(entry, testRuleLoader(t), spec)
		if err != nil {
			t.Fatal(err)
		}
		out := &outputs{files: map[string][]byte{}}
		if err := out.addRuleKind(k, testHeader); err != nil {
			t.Fatal(err)
		}
		check(entry.Kind, out.files)
	}
	for _, entry := range settingsKinds {
		entry.Kind = "Every" + strings.TrimPrefix(entry.Kind, "Test")
		entry.Doc = entry.Kind + " " + strings.SplitN(entry.Doc, " ", 2)[1]
		k, err := buildSettingsKind(entry, loadTestSettings(t, entry.Service), spec)
		if err != nil {
			t.Fatal(err)
		}
		out := &outputs{files: map[string][]byte{}}
		if err := out.addSettingsKind(k, testHeader); err != nil {
			t.Fatal(err)
		}
		check(entry.Kind, out.files)
	}
}

// TestGolden compares what kflare-gen generates for the test kinds with their
// golden files.
func TestGolden(t *testing.T) {
	files := goldenFiles(t)
	if want := 4 + 2*3 + 5*4 + 4*4; len(files) != want {
		t.Errorf("generated %d files, want %d: four types files, the controller, specs and sample of two kinds, and "+
			"the four files of each of five rule kinds and of four settings kinds", len(files), want)
	}
	for name, got := range files {
		golden := filepath.Join("testdata", name+".golden")
		if *update {
			if err := os.WriteFile(golden, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%v (go test ./generator -run TestGolden -update writes it)", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from %s: review the change, then go test ./generator -run TestGolden -update",
				name, golden)
		}
	}
}

// TestGoldenControllerCompiles type-checks the golden controllers and envtest
// tables of TestKVNamespace, the test rule kinds and the test settings kinds
// in internal/controller, as if they were generated there, with their golden
// types in api/v1alpha1.
// controller-gen writes a kind's deepcopy methods, so the test writes the two
// each kind's types need.
func TestGoldenControllerCompiles(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) []byte {
		src, err := os.ReadFile(filepath.Join("testdata", name+".golden"))
		if err != nil {
			t.Fatal(err)
		}
		return src
	}
	overlay := map[string][]byte{}
	for _, kind := range []string{"TestKVNamespace", "TestExecuteRule", "TestSkipRule", "TestCustomRule",
		"TestManagedRule", "TestRateLimitRule", "TestURLNormalization", "TestManagedTransform", "TestDNSSEC",
		"TestZoneSettings"} {
		lower := strings.ToLower(kind)
		overlay[filepath.Join(root, typesFile(lower))] = read(filepath.Base(typesFile(lower)))
		overlay[filepath.Join(root, apiDir, lower+"_deepcopy.go")] = fmt.Appendf(nil, `package v1alpha1

import "k8s.io/apimachinery/pkg/runtime"

func (in *%[1]s) DeepCopyObject() runtime.Object     { out := *in; return &out }
func (in *%[1]sList) DeepCopyObject() runtime.Object { out := *in; return &out }
`, kind)
		overlay[filepath.Join(root, controllerFile(lower))] = read(filepath.Base(controllerFile(lower)))
		overlay[filepath.Join(root, controllerTestFile(lower))] = read(filepath.Base(controllerTestFile(lower)))
	}
	pkgs, err := packages.Load(&packages.Config{
		Mode:    packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo,
		Dir:     root,
		Tests:   true,
		Overlay: overlay,
	}, "./"+controllerDir)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			t.Errorf("%s: %v", pkg.ID, e)
		}
		if pkg.Types == nil {
			continue
		}
		scope := pkg.Types.Scope()
		if scope.Lookup("fakeTestKVNamespaceAPI") != nil && scope.Lookup("testExecuteRuleKind") != nil &&
			scope.Lookup("testSkipRuleKind") != nil && scope.Lookup("testCustomRuleKind") != nil &&
			scope.Lookup("testManagedRuleKind") != nil &&
			scope.Lookup("testRateLimitRuleKind") != nil && scope.Lookup("fakeTestURLNormalizationAPI") != nil &&
			scope.Lookup("fakeTestManagedTransformAPI") != nil && scope.Lookup("fakeTestDNSSECAPI") != nil &&
			scope.Lookup("fakeTestZoneSettingsAPI") != nil {
			checked++
		}
	}
	if checked == 0 {
		t.Error("no package type-checked holds the golden envtest tables")
	}
}

// TestCommittedOutputIsUpToDate fails when what kflare-gen generates from
// generator.yaml is not what the repository holds: make codegen regenerates
// it.
func TestCommittedOutputIsUpToDate(t *testing.T) {
	cfg, err := loadConfig("generator.yaml")
	if err != nil {
		t.Fatal(err)
	}
	header, err := os.ReadFile(boilerplate)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generate(cfg, sdk{dir: "."}, loadTestSpec(t), string(header))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range out.files {
		got, err := os.ReadFile(filepath.Join("..", path))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s is not what kflare-gen generates (%v): run make codegen", path, err)
		}
	}
	for path, lines := range out.blocks {
		src, err := os.ReadFile(filepath.Join("..", path))
		if err != nil {
			t.Fatal(err)
		}
		if updated, err := replaceBlock(src, lines); err != nil || !bytes.Equal(updated, src) {
			t.Errorf("%s's generated block is not what kflare-gen generates (%v): run make codegen", path, err)
		}
	}
}

// runConfig is a configuration whose kind kflare-gen generates in full.
const runConfig = `kinds:
  - kind: TestKVNamespace
    service: kv.NamespaceService
    scope: account
    shortName: testkv
    noun: KV namespace
    doc: TestKVNamespace is a KV namespace.
    id: {field: id, status: namespaceID}
    claimLabel: kflare.dev/test-kv-namespace-id
    adopt: {by: title}
    sample: {title: example}
    ignore: {jurisdiction: not tested}
`

// testRepository returns a repository for run to write to, holding what it
// edits: the files with blocks.
func testRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{apiDir, controllerDir, samplesDir, filepath.Dir(crdKustomization),
		filepath.Dir(helmRBAC)} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	block := "before\n  # " + blockBegin + "\n  - stale\n  # " + blockEnd + "\nafter\n"
	for _, path := range []string{crdKustomization, helmRBAC} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestRun(t *testing.T) {
	root := testRepository(t)
	write := func(path, content string) {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// What kflare-gen generated for a kind no longer listed goes; the rest
	// stays.
	write(typesFile("gone"), "package v1alpha1\n\n"+generatedMarker+"\n")
	write(controllerFile("gone"), "package controller\n\n"+generatedMarker+"\n")
	write(controllerTestFile("gone"), "package controller\n\n"+generatedMarker+"\n")
	write(sampleFile("gone"), generatedSample+" from generator/generator.yaml. DO NOT EDIT.\n")
	write(sampleFile("handwritten"), "kind: HandWritten\n")
	write(apiDir+"/zz_generated.deepcopy.go", "package v1alpha1\n")
	write(controllerDir+"/zz_generated.handwritten.go", "package controller\n")

	if err := run(writeConfig(t, runConfig), committedSpec, root, boilerplate); err != nil {
		t.Fatalf("run: %v", err)
	}
	var paths []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		paths = append(paths, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		apiDir + "/zz_generated.deepcopy.go", typesFile("testkvnamespace"),
		sampleFile("handwritten"), sampleFile("testkvnamespace"), crdKustomization,
		controllerDir + "/zz_generated.handwritten.go", controllerFile("testkvnamespace"),
		controllerTestFile("testkvnamespace"), kindsFile, helmRBAC,
	}
	slices.Sort(want)
	if !slices.Equal(paths, want) {
		t.Errorf("after run: %q, want %q: the stale generated files removed, the others kept", paths, want)
	}
	for _, path := range []string{typesFile("testkvnamespace"), controllerFile("testkvnamespace"), kindsFile} {
		src, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || !strings.HasPrefix(string(src), "/*\nCopyright") || !strings.Contains(string(src), generatedMarker) {
			t.Errorf("%s starts with the license header and carries the generated marker: %v", path, err)
		}
	}
	for path, item := range map[string]string{
		crdKustomization: "  - bases/kflare.dev_testkvnamespaces.yaml\n", helmRBAC: "  - testkvnamespaces\n",
	} {
		src, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if want := "before\n  # " + blockBegin + "\n" + item + "  # " + blockEnd + "\nafter\n"; string(src) != want {
			t.Errorf("%s = %q, want %q", path, src, want)
		}
	}
}

func TestRunRefuses(t *testing.T) {
	config := writeConfig(t, runConfig)
	unknown := writeConfig(t, strings.Replace(runConfig, "kv.NamespaceService", "kv.NoSuchService", 1))
	refused := writeConfig(t, strings.Replace(runConfig, "{by: title}", "{by: jurisdiction}", 1))
	noBlock, noFile := testRepository(t), testRepository(t)
	if err := os.WriteFile(filepath.Join(noBlock, helmRBAC), []byte("no block\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(noFile, crdKustomization)); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][4]string{
		"a missing configuration":    {"missing.yaml", committedSpec, testRepository(t), boilerplate},
		"a missing spec":             {config, "missing.json", testRepository(t), boilerplate},
		"a missing header":           {config, committedSpec, testRepository(t), "missing.txt"},
		"an unknown service":         {unknown, committedSpec, testRepository(t), boilerplate},
		"a controller it refuses":    {refused, committedSpec, testRepository(t), boilerplate},
		"a missing repository":       {config, committedSpec, filepath.Join(t.TempDir(), "missing"), boilerplate},
		"a file without its block":   {config, committedSpec, noBlock, boilerplate},
		"a missing file with blocks": {config, committedSpec, noFile, boilerplate},
	} {
		if err := run(args[0], args[1], args[2], args[3]); err == nil {
			t.Errorf("run with %s: err = nil", name)
		}
	}
}
