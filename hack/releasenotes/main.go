/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

// Command releasenotes prints the notes of the GitHub release of the Helm
// chart's version: its section of CHANGELOG.md, without the heading, which
// the release's name repeats. A release page is not a file of the
// repository, and GitHub renders it differently, so the notes differ from
// the section in two ways:
//
//   - A relative link becomes absolute, to the file at the release's tag. A
//     link to a heading ("#upgrading-from-v041") goes to CHANGELOG.md at the
//     tag too: a release page gives its headings no anchors.
//   - A wrapped line joins the line it continues, since a release page shows
//     each line break. Code blocks stay as they are.
//
// Run it from the repository's root. The Release workflow publishes its
// output.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The repository's paths, relative to its root.
const (
	chartPath     = "helm/kflare/Chart.yaml"
	changelogPath = "CHANGELOG.md"
)

func main() {
	flags := flag.NewFlagSet("releasenotes", flag.ContinueOnError)
	root := flags.String("root", ".", "the repository whose release notes to print")
	err := flags.Parse(os.Args[1:])
	var notes string
	if err == nil {
		notes, err = run(*root)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "releasenotes:", err)
		os.Exit(1)
	}
	fmt.Print(notes)
}

// Chart.yaml's version and home. The release workflow reads the version the
// same way.
var (
	versionRE = regexp.MustCompile(`(?m)^version:[ \t]*(\S+)[ \t]*$`)
	homeRE    = regexp.MustCompile(`(?m)^home:[ \t]*(\S+)[ \t]*$`)
)

// run returns the notes of the release of the chart in the repository at
// root.
func run(root string) (string, error) {
	chart, err := os.ReadFile(filepath.Join(root, chartPath))
	if err != nil {
		return "", err
	}
	version, home := versionRE.FindSubmatch(chart), homeRE.FindSubmatch(chart)
	if version == nil || home == nil {
		return "", fmt.Errorf("%s has no version or no home", chartPath)
	}
	changelog, err := os.ReadFile(filepath.Join(root, changelogPath))
	if err != nil {
		return "", err
	}
	notes, err := releaseNotes(string(changelog), string(version[1]), string(home[1]))
	if err != nil {
		return "", fmt.Errorf("%s: %w", changelogPath, err)
	}
	return notes, nil
}

var (
	// linkTarget matches the target of an inline Markdown link. A link's text
	// can wrap onto the line before; its target cannot.
	linkTarget = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	// The lines that start a block of their own, which no wrapped line joins:
	// a heading, a list item and a table row.
	headingRE  = regexp.MustCompile(`^#{1,6}(\s|$)`)
	listItemRE = regexp.MustCompile(`^\s*([-*+]|\d+[.)])\s`)
	tableRowRE = regexp.MustCompile(`^\s*\|`)
)

// releaseNotes returns version's section of changelog as the notes of its
// release on GitHub: without its heading and the blank lines around it, each
// wrapped line joined to the one it continues, and each relative link
// resolved against CHANGELOG.md at the release's tag in the repository at
// home.
func releaseNotes(changelog, version, home string) (string, error) {
	base, err := url.Parse(strings.TrimSuffix(home, "/") + "/blob/v" + version + "/" + changelogPath)
	if err != nil {
		return "", err
	}
	heading := regexp.MustCompile(`^## v` + regexp.QuoteMeta(version) + `(\s.*)?$`)

	var (
		lines          []string
		found, inFence bool
		// joinable: the last line is a paragraph's or a list item's, which
		// a wrapped line continues.
		joinable bool
		linkErr  error
	)
	for line := range strings.Lines(changelog) {
		line = strings.TrimRight(line, "\r\n")
		if !found {
			found = heading.MatchString(line)
			continue
		}
		if fence := strings.HasPrefix(strings.TrimSpace(line), "```"); fence || inFence {
			inFence = inFence != fence
			lines, joinable = append(lines, line), false
			continue
		}
		if strings.HasPrefix(line, "## ") {
			break
		}
		line = linkTarget.ReplaceAllStringFunc(line, func(m string) string {
			target := linkTarget.FindStringSubmatch(m)[1]
			ref, err := url.Parse(target)
			if err != nil {
				linkErr = errors.Join(linkErr, fmt.Errorf("link %q: %w", target, err))
				return m
			}
			return "](" + base.ResolveReference(ref).String() + ")"
		})
		text := strings.TrimSpace(line)
		switch {
		case text == "" || headingRE.MatchString(line) || tableRowRE.MatchString(line):
			lines, joinable = append(lines, line), false
		case listItemRE.MatchString(line):
			lines, joinable = append(lines, line), true
		case joinable:
			lines[len(lines)-1] += " " + text
		default:
			lines, joinable = append(lines, line), true
		}
	}
	if !found {
		return "", fmt.Errorf("no section for v%s", version)
	}
	if linkErr != nil {
		return "", linkErr
	}
	notes := strings.Trim(strings.Join(lines, "\n"), "\n")
	if strings.TrimSpace(notes) == "" {
		return "", fmt.Errorf("v%s's section is empty", version)
	}
	return notes + "\n", nil
}
