/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// repoRoot is the repository's root, from this package's directory.
const repoRoot = "../.."

// TestChartIsCurrent runs the command on a copy of the repository's inputs and
// expects the committed Chart.yaml back: run make helm after changing the
// CRDs, the samples, the chart's version or its CHANGELOG.md section.
func TestChartIsCurrent(t *testing.T) {
	root := t.TempDir()
	for _, file := range []string{chartPath, valuesPath, releasePath, changelogPath} {
		copyFile(t, filepath.Join(repoRoot, file), filepath.Join(root, file))
	}
	for _, dir := range []string{crdDir, samplesDir} {
		if err := os.CopyFS(filepath.Join(root, dir), os.DirFS(filepath.Join(repoRoot, dir))); err != nil {
			t.Fatal(err)
		}
	}
	if err := run(root); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, chartPath))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(repoRoot, chartPath))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("%s is not what make helm writes: run make helm", chartPath)
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

// The values Artifact Hub accepts (its tracker's internal/pkg and
// internal/hub): any other makes it refuse the chart version.
var (
	validCategories = []string{"ai-machine-learning", "database", "integration-delivery", "monitoring-logging",
		"networking", "security", "storage", "streaming-messaging", "skip-prediction"}
	validCapabilities = []string{"basic install", "seamless upgrades", "full lifecycle", "deep insights", "auto pilot"}
	validChangeKinds  = []string{"added", "changed", "deprecated", "removed", "fixed", "security"}
	imageReferenceRE  = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// example is an entry of artifacthub.io/crdsExamples.
type example struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   map[string]any `json:"metadata"`
	Spec       map[string]any `json:"spec"`
}

// committedAnnotations returns the committed Chart.yaml's annotations.
func committedAnnotations(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, chartPath))
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := yaml.Unmarshal(data, &chart); err != nil {
		t.Fatal(err)
	}
	return chart.Annotations
}

// unmarshalAnnotation reads the YAML of one of the committed annotations.
func unmarshalAnnotation(t *testing.T, key string, into any) {
	t.Helper()
	if err := yaml.UnmarshalStrict([]byte(committedAnnotations(t)[key]), into); err != nil {
		t.Errorf("%s: %v", key, err)
	}
}

// checkLinks expects each link to have a name and an absolute URL.
func checkLinks(t *testing.T, links []link) {
	t.Helper()
	for _, l := range links {
		if u, err := url.Parse(l.URL); l.Name == "" || err != nil || u.Scheme != "https" || u.Host == "" {
			t.Errorf("link %+v: want a name and an absolute https URL", l)
		}
	}
}

// The tests below check the committed Chart.yaml's annotations as Artifact
// Hub's tracker does when it indexes a chart version
// (EnrichPackageFromAnnotations), and that the page gets what it shows.

func TestHandWrittenAnnotations(t *testing.T) {
	a := committedAnnotations(t)
	license, err := os.ReadFile(filepath.Join(repoRoot, "LICENSE"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(license), a["artifacthub.io/license"]+" License\n") {
		t.Errorf("artifacthub.io/license %q is not LICENSE's", a["artifacthub.io/license"])
	}
	if !slices.Contains(validCategories, a["artifacthub.io/category"]) {
		t.Errorf("artifacthub.io/category %q is not one of %v", a["artifacthub.io/category"], validCategories)
	}
	if operator, err := strconv.ParseBool(a["artifacthub.io/operator"]); err != nil || !operator {
		t.Errorf("artifacthub.io/operator = %q, want true", a["artifacthub.io/operator"])
	}
	if c := strings.ToLower(a["artifacthub.io/operatorCapabilities"]); !slices.Contains(validCapabilities, c) {
		t.Errorf("artifacthub.io/operatorCapabilities %q is not one of %v", c, validCapabilities)
	}
	var links []link
	unmarshalAnnotation(t, "artifacthub.io/links", &links)
	if !slices.ContainsFunc(links, func(l link) bool { return l.Name == "support" }) {
		t.Errorf("artifacthub.io/links = %v, want a support link, which Artifact Hub highlights", links)
	}
	checkLinks(t, links)
}

func TestChangesAnnotation(t *testing.T) {
	var changes []struct {
		Kind string `json:"kind"`
		change
	}
	unmarshalAnnotation(t, "artifacthub.io/changes", &changes)
	if len(changes) == 0 {
		t.Error("artifacthub.io/changes lists no changes")
	}
	for _, c := range changes {
		if c.Kind != "" && !slices.Contains(validChangeKinds, c.Kind) {
			t.Errorf("change %q: kind %q is not one of %v", c.Description, c.Kind, validChangeKinds)
		}
		if strings.TrimSpace(c.Description) == "" || strings.Contains(c.Description, "**") ||
			markdownLink.MatchString(c.Description) {
			t.Errorf("change %q: want a plain-text description", c.Description)
		}
		checkLinks(t, c.Links)
	}
}

func TestImagesAnnotation(t *testing.T) {
	var images []image
	unmarshalAnnotation(t, "artifacthub.io/images", &images)
	if len(images) == 0 {
		t.Error("artifacthub.io/images lists no images: Artifact Hub's security report scans them")
	}
	for _, i := range images {
		if !imageReferenceRE.MatchString(i.Image) || i.Name == "" || len(i.Platforms) == 0 {
			t.Errorf("image %+v: want a name, a reference with a tag and platforms", i)
		}
	}
}

// TestCRDsAnnotations expects a card for each CRD, with the fields Artifact
// Hub's page shows, and an example of its kind.
func TestCRDsAnnotations(t *testing.T) {
	var crds []crd
	unmarshalAnnotation(t, "artifacthub.io/crds", &crds)
	var examples []example
	unmarshalAnnotation(t, "artifacthub.io/crdsExamples", &examples)
	files, err := filepath.Glob(filepath.Join(repoRoot, crdDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(crds) != len(files) || len(examples) != len(crds) {
		t.Errorf("%d CRDs and %d examples, want %d of each, one per CRD in %s",
			len(crds), len(examples), len(files), crdDir)
	}
	for _, c := range crds {
		if c.Kind == "" || c.Version == "" || c.Name == "" || c.DisplayName == "" || c.Description == "" {
			t.Errorf("crd %+v: want a kind, version, name, display name and description", c)
		}
		i := slices.IndexFunc(examples, func(e example) bool { return e.Kind == c.Kind })
		if i < 0 {
			t.Errorf("crd %s has no example", c.Kind)
			continue
		}
		if e := examples[i]; !strings.HasSuffix(e.APIVersion, "/"+c.Version) || e.Metadata["name"] == nil ||
			e.Spec == nil {
			t.Errorf("crd %s: example %+v, want the CRD's version, a name and a spec", c.Kind, e)
		}
	}
}

func TestSummary(t *testing.T) {
	for _, tc := range []struct{ description, want string }{
		{"Zone is the Schema for the zones API.\nIt represents a DNS zone managed in Cloudflare.",
			"Zone represents a DNS zone managed in Cloudflare."},
		{"Zone is the Schema for the zones API.\nIt manages a zone: its\nname servers. Deleting it deletes the zone.",
			"Zone manages a zone: its name servers."},
		{"A DNS zone. Deleting it deletes the zone.", "A DNS zone."},
		{"Its name servers, as Cloudflare assigns them", "Its name servers, as Cloudflare assigns them"},
	} {
		got, err := summary("Zone", tc.description)
		if err != nil || got != tc.want {
			t.Errorf("summary(%q) = %q, %v, want %q", tc.description, got, err, tc.want)
		}
	}
	for _, description := range []string{
		"",
		"Zone is the Schema for the zones API.",
		"Zone is the Schema for the zones API.\n \n",
	} {
		if got, err := summary("Zone", description); err == nil {
			t.Errorf("summary(%q) = %q, want an error", description, got)
		}
	}
}

// changelog is a CHANGELOG.md with what releaseChanges reads and skips.
const changelog = "# Changelog\n\n" +
	"## Unreleased\n\n- Not released yet\n\n" +
	"## v1.2.0 (alpha)\n\n" +
	"The release's summary, which is not a change.\n\n" +
	"- A change before any subsection, [with a link](https://example.com/a)\n\n" +
	"### Upgrading from v1.1.0\n\n- A step, not a change\n\n1. **A numbered step**\n   that wraps\n\n" +
	"### The controller\n\n" +
	"- **Bold** words, a `code span`, and a change that\n  wraps onto [the next line](docs/deploy.md#upgrading)\n" +
	"- Two links to [one](#upgrading-from-v110) [page](#upgrading-from-v110) ([#7](https://github.com/o/r/issues/7))\n\n" +
	"### `Zone`\n\n- `name` is now `zoneName`\n- A Zone stored with `name` loses it:\n  - One item\n    that wraps\n" +
	"  - Another\n- After the nested list, with its own:\n  - Item\n\n" +
	"Before:\n\n```yaml\n- not: a change\n```\n\n- A kind's second change\n\n" +
	"### Known limitations\n\n- A limitation, not a change\n\n" +
	"## v1.1.0\n\n- An older change\n"

func TestReleaseChanges(t *testing.T) {
	got, err := releaseChanges([]byte(changelog), "1.2.0", "https://github.com/o/r/")
	if err != nil {
		t.Fatalf("releaseChanges: %v", err)
	}
	const tag = "https://github.com/o/r/blob/v1.2.0/"
	want := []change{
		{Description: "A change before any subsection, with a link",
			Links: []link{{"with a link", "https://example.com/a"}}},
		{Description: "Bold words, a `code span`, and a change that wraps onto the next line",
			Links: []link{{"the next line", tag + "docs/deploy.md#upgrading"}}},
		{Description: "Two links to one page (#7)", Links: []link{
			{"one", tag + "CHANGELOG.md#upgrading-from-v110"}, {"#7", "https://github.com/o/r/issues/7"}}},
		{Description: "Zone: `name` is now `zoneName`"},
		{Description: "Zone: A Zone stored with `name` loses it: One item that wraps; Another"},
		{Description: "Zone: After the nested list, with its own: Item"},
		{Description: "Zone: A kind's second change"},
	}
	if !slices.EqualFunc(got, want, func(a, b change) bool {
		return a.Description == b.Description && slices.Equal(a.Links, b.Links)
	}) {
		t.Errorf("releaseChanges =\n%+v\nwant\n%+v", got, want)
	}

	// A section whose last line, the file's, is a bullet without a newline.
	got, err = releaseChanges([]byte("## v1.2.0\n\n- First\n- Last\n  of all"), "1.2.0", "https://github.com/o/r")
	if err != nil || len(got) != 2 || got[1].Description != "Last of all" {
		t.Errorf("releaseChanges of a section ending the file = %+v, %v, want its last change", got, err)
	}

	const home = "https://github.com/o/r"
	for _, tc := range []struct{ changelog, version, home, want string }{
		{changelog, "1.3.0", home, "no section for v1.3.0"},
		{changelog, "1.2.0", "", "need the chart's version and home"},
		{"## v1.2.0\n\nOnly a summary.\n", "1.2.0", home, "lists no changes"},
		{"## v1.2.0\n\n### Known limitations\n\n- One\n", "1.2.0", home, "lists no changes"},
		{"## v1.2.0\n\n- [a](http://[::1]:namedport)\n", "1.2.0", home, "invalid port"},
		{"## v1.2.0\n\n- ****\n", "1.2.0", home, "without a description"},
	} {
		got, err := releaseChanges([]byte(tc.changelog), tc.version, tc.home)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("releaseChanges(%q, %q) = %+v, %v, want an error with %q", tc.changelog, tc.version, got, err, tc.want)
		}
	}
}

func TestReleasePlatforms(t *testing.T) {
	got, err := releasePlatforms([]byte("steps:\n  - with:\n      platforms: linux/amd64,linux/arm64\n"))
	if err != nil || !slices.Equal(got, []string{"linux/amd64", "linux/arm64"}) {
		t.Errorf("releasePlatforms = %v, %v", got, err)
	}
	for _, workflow := range []string{"steps: []\n", "    platforms: linux/amd64\n    platforms: linux/arm64\n"} {
		if got, err := releasePlatforms([]byte(workflow)); err == nil {
			t.Errorf("releasePlatforms(%q) = %v, want an error", workflow, got)
		}
	}
}

func TestReadExamples(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const zone = "apiVersion: kflare.dev/v1alpha1\nkind: Zone\nmetadata:\n  name: "
	const record = "apiVersion: kflare.dev/v1alpha1\nkind: DNSRecord\nmetadata:\n  name: "
	// A Zone in another kind's file comes before the Zone's own file, which
	// holds an older version first: the example is the own file's v1alpha1.
	write("cloudflare_v1alpha1_aaa.yaml", zone+"elsewhere\n")
	write("cloudflare_v1alpha1_zone.yaml",
		"# a comment-only document\n---\napiVersion: kflare.dev/v1alpha0\nkind: Zone\nmetadata:\n  name: old\n---\n"+
			zone+"own\n")
	// A kind with several samples: the first file's.
	write("cloudflare_v1alpha1_dnsrecord_mx.yaml", record+"mx\n")
	write("cloudflare_v1alpha1_dnsrecord_a.yaml", record+"a\n")

	kinds := []crd{{Kind: "DNSRecord", Version: "v1alpha1", group: "kflare.dev"},
		{Kind: "Zone", Version: "v1alpha1", group: "kflare.dev"}}
	got, err := readExamples(dir, kinds)
	if err != nil {
		t.Fatalf("readExamples: %v", err)
	}
	names := make([]any, 0, len(got))
	for _, e := range got {
		names = append(names, e["metadata"].(map[string]any)["name"])
	}
	if !slices.Equal(names, []any{"a", "own"}) {
		t.Errorf("readExamples names = %v, want [a own]", names)
	}

	for _, kinds := range [][]crd{
		{{Kind: "Tunnel", Version: "v1alpha1", group: "kflare.dev"}},       // no sample file
		{{Kind: "Zone", Version: "v1beta1", group: "kflare.dev"}},          // none of the version
		{{Kind: "DNSRecord", Version: "v1alpha1", group: "other.example"}}, // none of the group
	} {
		if got, err := readExamples(dir, kinds); err == nil {
			t.Errorf("readExamples(%+v) = %v, want an error", kinds, got)
		}
	}
	write("cloudflare_v1alpha1_tunnel.yaml", "kind: [unclosed\n")
	if got, err := readExamples(dir, []crd{{Kind: "Tunnel", Version: "v1alpha1", group: "kflare.dev"}}); err == nil {
		t.Errorf("readExamples of a sample that does not parse = %v, want an error", got)
	}
}

func TestAnnotate(t *testing.T) {
	chart := "name: kflare\nannotations:\n  artifacthub.io/license: MIT\n" + marker + "\n  artifacthub.io/old: x\n"
	annotations := []annotation{
		{"artifacthub.io/images", []image{
			{Name: "kflare", Image: "example.com/kflare:1.0.0", Platforms: []string{"linux/amd64"}}}},
		{"artifacthub.io/crdsExamples", []map[string]any{{"kind": "Zone", "spec": map[string]any{
			"script": "line one\n\nline three, after a blank line\n"}}}},
	}
	got, err := annotate([]byte(chart), annotations)
	if err != nil {
		t.Fatalf("annotate: %v", err)
	}
	want := "name: kflare\nannotations:\n  artifacthub.io/license: MIT\n" + marker + "\n" +
		"  artifacthub.io/images: |\n" +
		"    - image: example.com/kflare:1.0.0\n      name: kflare\n      platforms:\n      - linux/amd64\n" +
		"  artifacthub.io/crdsExamples: |\n" +
		"    - kind: Zone\n      spec:\n        script: |\n          line one\n\n          line three, after a blank line\n"
	if string(got) != want {
		t.Errorf("annotate =\n%s\nwant\n%s", got, want)
	}
	if again, err := annotate(got, annotations); err != nil || string(again) != want {
		t.Errorf("annotate of its own output =\n%s, %v\nwant it unchanged", again, err)
	}

	if got, err := annotate([]byte("name: kflare\nannotations: {}\n"), annotations); err == nil {
		t.Errorf("annotate without the marker = %s, want an error", got)
	}
	for _, chart := range []string{
		// Annotations that are not the last key: annotate would drop the key after them.
		"annotations:\n" + marker + "\n  artifacthub.io/old: x\nname: kflare\n",
		// No annotations key above the marker.
		"name: kflare\n" + marker + "\n",
		// A hand-written annotation that is not a string: the chart does not read back.
		"annotations:\n  artifacthub.io/license: [MIT]\n" + marker + "\n",
	} {
		if got, err := annotate([]byte(chart), annotations); err == nil {
			t.Errorf("annotate(%q) = %s, want an error", chart, got)
		}
	}
	// A value ending in blank lines, which the annotation's block would drop.
	trailing := []annotation{{"artifacthub.io/crdsExamples", []map[string]any{{"spec": "script\n\n"}}}}
	if got, err := annotate([]byte(chart), trailing); err == nil {
		t.Errorf("annotate of a value ending in blank lines = %s, want an error", got)
	}
}
