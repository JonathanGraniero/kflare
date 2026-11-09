/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

// Command supportedreleases prints the releases that get fixes, as
// docs/versioning.md's "Supported releases" says: the latest release, and the
// newest patch of the minor release before it, for three months after the
// latest minor release came out. The minor releases before v0.4 get none. The
// Release scan workflow scans the image of each.
//
// It reads the repository's releases on standard input, as GitHub's REST API
// lists them, one page after another, as `gh api --paginate
// repos/{owner}/{repo}/releases` prints them. A release is one that is
// published, neither a draft nor a prerelease, with a vX.Y.Z tag. It prints a
// JSON list for a workflow's matrix, the latest release first, each with its
// tag and the code scanning category of its scan's report, and says why on
// standard error.
package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"time"
)

// The code scanning categories of the scans' reports: the latest release's,
// and that of the minor release before it, whichever release that is. Each
// scan replaces the last report of its category.
const (
	latestCategory   = "release-image"
	previousCategory = "release-image-previous-minor"
)

// policy says how long the minor release before the latest gets security
// fixes: for months after the latest minor release comes out, if it is since
// or newer.
type policy struct {
	months int
	since  minorVersion
}

// supportPolicy is docs/versioning.md's: three months, starting with v0.4.
var supportPolicy = policy{months: 3, since: minorVersion{0, 4}}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "supportedreleases:", err)
		os.Exit(1)
	}
}

// run prints the releases among those read from in that get fixes at now, or
// at the time of the -now flag, to out, and why to log.
func run(args []string, in io.Reader, out, log io.Writer, now time.Time) error {
	flags := flag.NewFlagSet("supportedreleases", flag.ContinueOnError)
	flags.SetOutput(log)
	at := flags.String("now", "", "the time to work it out for, in RFC 3339 (default: the current time)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *at != "" {
		t, err := time.Parse(time.RFC3339, *at)
		if err != nil {
			return fmt.Errorf("-now: %w", err)
		}
		now = t
	}
	releases, err := readReleases(in)
	if err != nil {
		return err
	}
	scans, reasons, err := supported(releases, now, supportPolicy)
	if err != nil {
		return err
	}
	for _, reason := range reasons {
		if _, err := fmt.Fprintln(log, reason); err != nil {
			return err
		}
	}
	matrix, err := json.Marshal(scans)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\n", matrix)
	return err
}

// githubRelease is what this command reads of a release in GitHub's REST API.
// A draft has no publication time.
type githubRelease struct {
	TagName     string     `json:"tag_name"`
	Draft       bool       `json:"draft"`
	Prerelease  bool       `json:"prerelease"`
	PublishedAt *time.Time `json:"published_at"`
}

// readReleases reads the releases in r, a JSON list per page.
func readReleases(r io.Reader) ([]githubRelease, error) {
	var releases []githubRelease
	pages := json.NewDecoder(r)
	for {
		var page []githubRelease
		if err := pages.Decode(&page); errors.Is(err, io.EOF) {
			return releases, nil
		} else if err != nil {
			return nil, fmt.Errorf("reading the releases: %w", err)
		}
		releases = append(releases, page...)
	}
}

// version is a release's version, from its vX.Y.Z tag.
type version struct{ major, minor, patch int }

// minorVersion is a minor release's X.Y, which its patch releases share.
type minorVersion struct{ major, minor int }

// tagRE matches a release's tag, without leading zeros, as semantic
// versioning writes a version.
var tagRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// parseTag returns the version of a vX.Y.Z tag, and false for any other tag.
func parseTag(tag string) (version, bool) {
	m := tagRE.FindStringSubmatch(tag)
	if m == nil {
		return version{}, false
	}
	var parts [3]int
	for i, s := range m[1:] {
		n, err := strconv.Atoi(s)
		if err != nil {
			return version{}, false
		}
		parts[i] = n
	}
	return version{parts[0], parts[1], parts[2]}, true
}

// compare orders versions as numbers: v0.10.0 is newer than v0.9.3.
func (v version) compare(o version) int {
	return cmp.Or(cmp.Compare(v.major, o.major), cmp.Compare(v.minor, o.minor), cmp.Compare(v.patch, o.patch))
}

func (v version) minorVersion() minorVersion { return minorVersion{v.major, v.minor} }

// before reports whether m is an older minor release than o.
func (m minorVersion) before(o minorVersion) bool {
	return m.major < o.major || m.major == o.major && m.minor < o.minor
}

func (m minorVersion) String() string { return fmt.Sprintf("v%d.%d", m.major, m.minor) }

// release is a published release.
type release struct {
	tag       string
	version   version
	published time.Time
}

// scan is a release whose image to scan, and the category of its report.
type scan struct {
	Tag      string `json:"tag"`
	Category string `json:"category"`
}

// supported returns the releases that get fixes at now under p, the latest
// first, and why, a line each.
func supported(all []githubRelease, now time.Time, p policy) ([]scan, []string, error) {
	var releases []release
	for _, r := range all {
		v, ok := parseTag(r.TagName)
		if !ok || r.Draft || r.Prerelease || r.PublishedAt == nil {
			continue
		}
		releases = append(releases, release{r.TagName, v, *r.PublishedAt})
	}
	if len(releases) == 0 {
		return nil, nil, errors.New("no published vX.Y.Z release")
	}
	// Newest first, so that a minor release's patches come together, its
	// newest first.
	slices.SortFunc(releases, func(a, b release) int { return b.version.compare(a.version) })

	latest := releases[0]
	scans := []scan{{latest.tag, latestCategory}}
	reasons := []string{latest.tag + " is the latest release: it gets every fix"}

	// The latest minor release came out with its first published release.
	// The first release of an older minor is the newest of the one before.
	cameOut := latest
	var previous *release
	for i, r := range releases {
		if r.version.minorVersion() != latest.version.minorVersion() {
			previous = &releases[i]
			break
		}
		if r.published.Before(cameOut.published) {
			cameOut = r
		}
	}
	if previous == nil {
		return scans, reasons, nil
	}

	// A day that the last month lacks rolls over into the next: three months
	// after November 30 end on March 2, or 1 in a leap year.
	line := previous.version.minorVersion()
	end := cameOut.published.AddDate(0, p.months, 0)
	until := end.UTC().Format(time.RFC3339)
	after := fmt.Sprintf("%d months after %s came out", p.months, cameOut.tag)
	switch {
	case line.before(p.since):
		reasons = append(reasons, fmt.Sprintf(
			"%s, the newest %s release, gets no fixes: the minor releases before %s get none",
			previous.tag, line, p.since))
	case !now.Before(end):
		reasons = append(reasons, fmt.Sprintf("%s, the newest %s release, got security fixes until %s, %s",
			previous.tag, line, until, after))
	default:
		scans = append(scans, scan{previous.tag, previousCategory})
		reasons = append(reasons, fmt.Sprintf("%s is the newest %s release: it gets security fixes until %s, %s",
			previous.tag, line, until, after))
	}
	return scans, reasons, nil
}
