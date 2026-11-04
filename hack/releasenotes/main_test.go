/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot is the repository's root, from this package's directory.
const repoRoot = "../.."

// fenced turns three single quotes in s into a Markdown code fence's three
// backticks, which a raw string cannot hold.
func fenced(s string) string { return strings.ReplaceAll(s, "'''", "```") }

// changelog has the release, v1.2.1, between a release whose version starts
// with its own and an older one.
var changelog = fenced(`# Changelog

## Unreleased

- [Not released](docs/next.md)

## v1.2.10 (alpha)

- Not this release

## v1.2.1 (alpha)

The release, with [a link](docs/deploy.md#upgrading), one [to a
heading](#upgrading-from-v120) and one [elsewhere](https://example.com/a). Read
[the README](README.md) first.

'''sh
helm upgrade kflare oci://example.com/kflare \
  --version 1.2.1
## Not a heading, and [not a link](docs/a.md)
'''

### Upgrading from v1.2.0
Straight after its heading.

1. **Rewrite the manifests**, which wraps
   onto a second line
2. Upgrade the chart

| Field | Was |
|---|---|
| ` + "`name`" + ` | [docs](docs/fields.md) |

### The controller

- **A change** that wraps onto the next
  line, with [an issue](https://github.com/o/r/issues/7)
  - A nested item, which wraps
    too
- A last change

  Its second paragraph,
  wrapped, and a manifest:

  '''yaml
  kind: Zone
  spec:
    name: example.com
  '''

## v1.2.0

- Older
`)

// notes is v1.2.1's section of changelog as its release shows it, each
// paragraph and list item on one line.
var notes = fenced("The release, with [a link](https://github.com/o/r/blob/v1.2.1/docs/deploy.md#upgrading), one " +
	"[to a heading](https://github.com/o/r/blob/v1.2.1/CHANGELOG.md#upgrading-from-v120) and one " +
	"[elsewhere](https://example.com/a). Read [the README](https://github.com/o/r/blob/v1.2.1/README.md) first." + `

'''sh
helm upgrade kflare oci://example.com/kflare \
  --version 1.2.1
## Not a heading, and [not a link](docs/a.md)
'''

### Upgrading from v1.2.0
Straight after its heading.

1. **Rewrite the manifests**, which wraps onto a second line
2. Upgrade the chart

| Field | Was |
|---|---|
| ` + "`name`" + ` | [docs](https://github.com/o/r/blob/v1.2.1/docs/fields.md) |

### The controller

- **A change** that wraps onto the next line, with [an issue](https://github.com/o/r/issues/7)
  - A nested item, which wraps too
- A last change

  Its second paragraph, wrapped, and a manifest:

  '''yaml
  kind: Zone
  spec:
    name: example.com
  '''
`)

func TestReleaseNotes(t *testing.T) {
	for _, home := range []string{"https://github.com/o/r", "https://github.com/o/r/"} {
		got, err := releaseNotes(changelog, "1.2.1", home)
		if err != nil {
			t.Fatalf("releaseNotes(%q): %v", home, err)
		}
		if got != notes {
			t.Errorf("releaseNotes(%q) =\n%s\nwant\n%s", home, got, notes)
		}
	}

	// The last section of the file, which ends without a newline.
	got, err := releaseNotes("## v1.2.0\n\n- Last\n  of all", "1.2.0", "https://github.com/o/r")
	if err != nil || got != "- Last of all\n" {
		t.Errorf("releaseNotes of the file's last section = %q, %v, want %q", got, err, "- Last of all\n")
	}

	for _, tc := range []struct{ changelog, version, want string }{
		{changelog, "1.3.0", "no section for v1.3.0"},
		{"## v1.2.0-rc.1\n\n- A candidate\n", "1.2.0", "no section for v1.2.0"},
		{"## v1.2.0\n\n## v1.1.0\n\n- Older\n", "1.2.0", "v1.2.0's section is empty"},
		{"## v1.2.0\n\n\n", "1.2.0", "v1.2.0's section is empty"},
		{"## v1.2.0\n\n- [a](http://[::1]:namedport)\n", "1.2.0", "invalid port"},
	} {
		got, err := releaseNotes(tc.changelog, tc.version, "https://github.com/o/r")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("releaseNotes(%q, %q) = %q, %v, want an error with %q", tc.changelog, tc.version, got, err, tc.want)
		}
	}
}

func TestRun(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(changelogPath, changelog)
	// The annotations' examples hold keys of the same names, which are not
	// the chart's.
	write(chartPath, "apiVersion: v2\nname: kflare\nannotations:\n  artifacthub.io/crdsExamples: |\n"+
		"    - version: v1alpha1\n      home: https://example.com/other\n"+
		"version: 1.2.1\nappVersion: \"1.2.1\"\nhome: https://github.com/o/r\n")
	if got, err := run(root); err != nil || got != notes {
		t.Errorf("run = %q, %v, want v1.2.1's notes", got, err)
	}

	write(chartPath, "apiVersion: v2\nname: kflare\nversion: 1.2.1\n")
	if _, err := run(root); err == nil || !strings.Contains(err.Error(), "has no version or no home") {
		t.Errorf("run with a chart without a home = %v, want an error", err)
	}
	write(chartPath, "apiVersion: v2\nname: kflare\nversion: 1.3.0\nhome: https://github.com/o/r\n")
	if _, err := run(root); err == nil || !strings.Contains(err.Error(), "CHANGELOG.md: no section for v1.3.0") {
		t.Errorf("run with a version CHANGELOG.md lacks = %v, want an error", err)
	}
	if err := os.Remove(filepath.Join(root, changelogPath)); err != nil {
		t.Fatal(err)
	}
	if _, err := run(root); err == nil {
		t.Error("run without CHANGELOG.md succeeded, want an error")
	}
}

// TestTheChartsNotes writes the notes of the chart's version, which Release
// publishes once the version is tagged: every link in them leaves the
// repository's tree, to the release's tag or elsewhere.
func TestTheChartsNotes(t *testing.T) {
	got, err := run(repoRoot)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	chart, err := os.ReadFile(filepath.Join(repoRoot, chartPath))
	if err != nil {
		t.Fatal(err)
	}
	tag := "/blob/v" + string(versionRE.FindSubmatch(chart)[1]) + "/"
	if strings.HasPrefix(got, "## ") {
		t.Errorf("the notes start with a section's heading:\n%s", got)
	}
	inFence := false
	for line := range strings.Lines(got) {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
		}
		if inFence {
			continue
		}
		for _, m := range linkTarget.FindAllStringSubmatch(line, -1) {
			if !regexp.MustCompile(`^https?://`).MatchString(m[1]) {
				t.Errorf("link %q is relative", m[1])
			} else if strings.HasPrefix(m[1], string(homeRE.FindSubmatch(chart)[1])+"/blob/") &&
				!strings.Contains(m[1], tag) {
				t.Errorf("link %q is not to the release's tag", m[1])
			}
		}
	}
}
