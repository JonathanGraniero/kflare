/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

// published is a published release, as GitHub's API lists it.
func published(tag, at string) githubRelease {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		panic(err)
	}
	return githubRelease{TagName: tag, PublishedAt: &t}
}

// releasesToday are kflare's releases on 2026-11-08, as GitHub lists them.
var releasesToday = []githubRelease{
	published("v0.4.2", "2026-11-04T16:56:22Z"),
	published("v0.4.1", "2026-11-01T00:19:09Z"),
	published("v0.4.0", "2026-10-31T21:43:33Z"),
	published("v0.3.0", "2026-10-29T22:52:59Z"),
	published("v0.2.1", "2026-10-26T21:11:56Z"),
	published("v0.2.0", "2026-10-26T02:01:23Z"),
	published("v0.1.0", "2026-10-21T17:36:07Z"),
}

// with returns releasesToday and more.
func with(more ...githubRelease) []githubRelease {
	return append(slices.Clone(releasesToday), more...)
}

func latestScan(tag string) scan   { return scan{Tag: tag, Category: latestCategory} }
func previousScan(tag string) scan { return scan{Tag: tag, Category: previousCategory} }

// v050 is v0.5.0, as it might come out.
var v050 = published("v0.5.0", "2027-01-10T12:00:00Z")

func TestSupported(t *testing.T) {
	prerelease := published("v0.6.0", "2027-02-01T00:00:00Z")
	prerelease.Prerelease = true
	tests := []struct {
		name     string
		releases []githubRelease
		now      string
		// The zero policy is supportPolicy.
		policy policy
		want   []scan
	}{
		{
			name:     "today: the minor release before the latest gets security fixes from v0.4 on",
			releases: releasesToday,
			now:      "2026-11-08T00:00:00Z",
			want:     []scan{latestScan("v0.4.2")},
		},
		{
			name:     "once v0.5.0 is out, v0.4's newest patch gets security fixes",
			releases: with(v050),
			now:      "2027-01-10T12:00:00Z",
			want:     []scan{latestScan("v0.5.0"), previousScan("v0.4.2")},
		},
		{
			name:     "for three months",
			releases: with(v050),
			now:      "2027-04-10T11:59:59Z",
			want:     []scan{latestScan("v0.5.0"), previousScan("v0.4.2")},
		},
		{
			name:     "and no longer",
			releases: with(v050),
			now:      "2027-04-10T12:00:00Z",
			want:     []scan{latestScan("v0.5.0")},
		},
		{
			name:     "three months after November 30 end on March 2",
			releases: with(published("v0.5.0", "2026-11-30T00:00:00Z")),
			now:      "2027-03-01T23:59:59Z",
			want:     []scan{latestScan("v0.5.0"), previousScan("v0.4.2")},
		},
		{
			name:     "when February has 28 days",
			releases: with(published("v0.5.0", "2026-11-30T00:00:00Z")),
			now:      "2027-03-02T00:00:00Z",
			want:     []scan{latestScan("v0.5.0")},
		},
		{
			name: "each minor release's newest patch, by version rather than by publication",
			releases: with(
				v050,
				published("v0.5.1", "2027-02-01T12:00:00Z"),
				published("v0.4.3", "2027-02-02T12:00:00Z"),
			),
			now:  "2027-02-03T00:00:00Z",
			want: []scan{latestScan("v0.5.1"), previousScan("v0.4.3")},
		},
		{
			name:     "the three months start with the latest minor release's first release, not its newest",
			releases: with(v050, published("v0.5.1", "2027-03-01T12:00:00Z")),
			now:      "2027-04-11T00:00:00Z",
			want:     []scan{latestScan("v0.5.1")},
		},
		{
			name: "its first published release",
			releases: with(
				githubRelease{TagName: "v0.5.0", Draft: true},
				published("v0.5.1", "2027-01-20T12:00:00Z"),
			),
			now:  "2027-04-11T00:00:00Z",
			want: []scan{latestScan("v0.5.1"), previousScan("v0.4.2")},
		},
		{
			name:     "two minor releases at a time: a newer one ends the oldest's security fixes",
			releases: with(v050, published("v0.6.0", "2027-02-01T12:00:00Z")),
			now:      "2027-02-02T00:00:00Z",
			want:     []scan{latestScan("v0.6.0"), previousScan("v0.5.0")},
		},
		{
			name:     "the minor release before the latest is the newest older one",
			releases: with(published("v0.6.0", "2027-02-01T12:00:00Z")),
			now:      "2027-02-02T00:00:00Z",
			want:     []scan{latestScan("v0.6.0"), previousScan("v0.4.2")},
		},
		{
			name: "versions compare as numbers",
			releases: []githubRelease{
				published("v0.9.3", "2027-01-01T00:00:00Z"),
				published("v0.10.0", "2027-02-01T00:00:00Z"),
				published("v0.9.10", "2027-01-15T00:00:00Z"),
			},
			now:  "2027-02-02T00:00:00Z",
			want: []scan{latestScan("v0.10.0"), previousScan("v0.9.10")},
		},
		{
			name: "the minor release before a major release",
			releases: with(
				published("v0.9.0", "2027-06-01T00:00:00Z"),
				published("v1.0.0", "2027-08-01T00:00:00Z"),
			),
			now:  "2027-08-02T00:00:00Z",
			want: []scan{latestScan("v1.0.0"), previousScan("v0.9.0")},
		},
		{
			name: "a later major release's minor releases",
			releases: with(
				published("v1.0.0", "2027-08-01T00:00:00Z"),
				published("v1.2.0", "2028-01-01T00:00:00Z"),
				published("v1.3.0", "2028-02-01T00:00:00Z"),
			),
			now:  "2028-02-02T00:00:00Z",
			want: []scan{latestScan("v1.3.0"), previousScan("v1.2.0")},
		},
		{
			name: "drafts, prereleases and other tags are not releases",
			releases: with(
				v050,
				githubRelease{TagName: "v0.6.0", Draft: true},
				prerelease,
				published("v0.6.0-rc.1", "2027-02-01T00:00:00Z"),
				published("v0.6", "2027-02-01T00:00:00Z"),
				published("v0.06.0", "2027-02-01T00:00:00Z"),
				published("kflare-v0.6.0", "2027-02-01T00:00:00Z"),
				published("v0.6.0 ", "2027-02-01T00:00:00Z"),
				published("v99999999999999999999.0.0", "2027-02-01T00:00:00Z"),
			),
			now:  "2027-02-02T00:00:00Z",
			want: []scan{latestScan("v0.5.0"), previousScan("v0.4.2")},
		},
		{
			name:     "a release without a publication time is not out",
			releases: with(githubRelease{TagName: "v0.5.0"}),
			now:      "2027-02-02T00:00:00Z",
			want:     []scan{latestScan("v0.4.2")},
		},
		{
			name:     "nor is a draft with one, as a release made a draft again could be",
			releases: with(func() githubRelease { r := v050; r.Draft = true; return r }()),
			now:      "2027-02-02T00:00:00Z",
			want:     []scan{latestScan("v0.4.2")},
		},
		{
			name:     "the first minor release has none before it",
			releases: []githubRelease{published("v0.1.0", "2026-10-21T17:36:07Z")},
			now:      "2026-10-22T00:00:00Z",
			want:     []scan{latestScan("v0.1.0")},
		},
		{
			// v0.4.0 came out on October 31, and the month after it ends on
			// December 1: there is no November 31.
			name:     "a policy's months",
			releases: releasesToday,
			now:      "2026-12-01T21:43:32Z",
			policy:   policy{months: 1, since: minorVersion{0, 3}},
			want:     []scan{latestScan("v0.4.2"), previousScan("v0.3.0")},
		},
		{
			name:     "after them",
			releases: releasesToday,
			now:      "2026-12-01T21:43:33Z",
			policy:   policy{months: 1, since: minorVersion{0, 3}},
			want:     []scan{latestScan("v0.4.2")},
		},
		{
			name: "a policy's first minor release, of a later major",
			releases: []githubRelease{
				published("v0.9.0", "2027-06-01T00:00:00Z"),
				published("v1.0.0", "2027-08-01T00:00:00Z"),
			},
			now:    "2027-08-02T00:00:00Z",
			policy: policy{months: 3, since: minorVersion{1, 0}},
			want:   []scan{latestScan("v1.0.0")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.policy
			if p == (policy{}) {
				p = supportPolicy
			}
			now, err := time.Parse(time.RFC3339, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			got, _, err := supported(tt.releases, now, p)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("supported() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSupportedReasons(t *testing.T) {
	tests := []struct {
		name     string
		releases []githubRelease
		now      string
		want     []string
	}{
		{
			name:     "before v0.4",
			releases: releasesToday,
			now:      "2026-11-08T00:00:00Z",
			want: []string{
				"v0.4.2 is the latest release: it gets every fix",
				"v0.3.0, the newest v0.3 release, gets no fixes: the minor releases before v0.4 get none",
			},
		},
		{
			name:     "before v0.4, after the three months too",
			releases: releasesToday,
			now:      "2027-02-01T00:00:00Z",
			want: []string{
				"v0.4.2 is the latest release: it gets every fix",
				"v0.3.0, the newest v0.3 release, gets no fixes: the minor releases before v0.4 get none",
			},
		},
		{
			name:     "within the three months",
			releases: with(v050),
			now:      "2027-01-11T00:00:00Z",
			want: []string{
				"v0.5.0 is the latest release: it gets every fix",
				"v0.4.2 is the newest v0.4 release: it gets security fixes until 2027-04-10T12:00:00Z, " +
					"3 months after v0.5.0 came out",
			},
		},
		{
			name:     "after them",
			releases: with(v050),
			now:      "2027-05-01T00:00:00Z",
			want: []string{
				"v0.5.0 is the latest release: it gets every fix",
				"v0.4.2, the newest v0.4 release, got security fixes until 2027-04-10T12:00:00Z, " +
					"3 months after v0.5.0 came out",
			},
		},
		{
			name:     "the first minor release",
			releases: []githubRelease{published("v0.1.0", "2026-10-21T17:36:07Z")},
			now:      "2026-10-22T00:00:00Z",
			want:     []string{"v0.1.0 is the latest release: it gets every fix"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			_, got, err := supported(tt.releases, now, supportPolicy)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("reasons:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
		})
	}
}

func TestSupportedNoRelease(t *testing.T) {
	for name, releases := range map[string][]githubRelease{
		"none":         nil,
		"only a draft": {{TagName: "v0.1.0", Draft: true}},
		"no vX.Y.Z":    {published("latest", "2026-10-21T17:36:07Z")},
	} {
		t.Run(name, func(t *testing.T) {
			if got, _, err := supported(releases, time.Now(), supportPolicy); err == nil {
				t.Errorf("supported() = %v, want an error", got)
			}
		})
	}
}

func TestParseTag(t *testing.T) {
	tests := []struct {
		tag  string
		want version
		ok   bool
	}{
		{"v0.4.2", version{0, 4, 2}, true},
		{"v10.20.30", version{10, 20, 30}, true},
		{"v1.0.0-rc.1", version{}, false},
		{"v1.0", version{}, false},
		{"1.0.0", version{}, false},
		{"V1.0.0", version{}, false},
		{"v01.0.0", version{}, false},
		{" v1.0.0", version{}, false},
		{"v1.0.0\n", version{}, false},
		// Past the range of an int.
		{"v18446744073709551616.0.0", version{}, false},
	}
	for _, tt := range tests {
		if got, ok := parseTag(tt.tag); got != tt.want || ok != tt.ok {
			t.Errorf("parseTag(%q) = %v, %v, want %v, %v", tt.tag, got, ok, tt.want, tt.ok)
		}
	}
}

// pages is two pages of the releases API, in the form gh api --paginate prints
// them, with fields this command does not read.
const pages = `[{"id":1,"tag_name":"v0.5.0","target_commitish":"main","name":"kflare v0.5.0","draft":false,
"prerelease":false,"created_at":"2027-01-10T11:50:00Z","published_at":"2027-01-10T12:00:00Z"},
{"id":2,"tag_name":"v0.5.1","draft":true,"prerelease":false,"published_at":null}]
[{"id":3,"tag_name":"v0.4.2","target_commitish":"main","draft":false,"prerelease":false,
"published_at":"2026-11-04T16:56:22Z"}]
`

func TestReadReleases(t *testing.T) {
	got, err := readReleases(strings.NewReader(pages))
	if err != nil {
		t.Fatal(err)
	}
	want := []githubRelease{v050, {TagName: "v0.5.1", Draft: true}, releasesToday[0]}
	if len(got) != len(want) {
		t.Fatalf("readReleases() read %d releases, want %d", len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.TagName != w.TagName || g.Draft != w.Draft || g.Prerelease != w.Prerelease ||
			(g.PublishedAt == nil) != (w.PublishedAt == nil) ||
			g.PublishedAt != nil && !g.PublishedAt.Equal(*w.PublishedAt) {
			t.Errorf("release %d = %+v, want %+v", i, g, w)
		}
	}

	for _, empty := range []string{"", "[]", "[]\n[]\n"} {
		if got, err := readReleases(strings.NewReader(empty)); err != nil || len(got) != 0 {
			t.Errorf("readReleases(%q) = %v, %v, want none", empty, got, err)
		}
	}
	for _, bad := range []string{`[{"tag_name":`, `{"message":"Not Found"}`, `[{"published_at":"yesterday"}]`} {
		if got, err := readReleases(strings.NewReader(bad)); err == nil {
			t.Errorf("readReleases(%q) = %v, want an error", bad, got)
		}
	}
}

// failingWriter fails every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestRun(t *testing.T) {
	now := time.Date(2027, 1, 11, 0, 0, 0, 0, time.UTC)
	var out, log bytes.Buffer
	if err := run(nil, strings.NewReader(pages), &out, &log, now); err != nil {
		t.Fatal(err)
	}
	const matrix = `[{"tag":"v0.5.0","category":"release-image"},` +
		`{"tag":"v0.4.2","category":"release-image-previous-minor"}]` + "\n"
	if out.String() != matrix {
		t.Errorf("out = %q, want %q", out.String(), matrix)
	}
	const reasons = "v0.5.0 is the latest release: it gets every fix\n" +
		"v0.4.2 is the newest v0.4 release: it gets security fixes until 2027-04-10T12:00:00Z, " +
		"3 months after v0.5.0 came out\n"
	if log.String() != reasons {
		t.Errorf("log = %q, want %q", log.String(), reasons)
	}

	// -now replaces the current time.
	out.Reset()
	log.Reset()
	if err := run([]string{"-now", "2027-04-10T12:00:00Z"}, strings.NewReader(pages), &out, &log, now); err != nil {
		t.Fatal(err)
	}
	if want := `[{"tag":"v0.5.0","category":"release-image"}]` + "\n"; out.String() != want {
		t.Errorf("with -now, out = %q, want %q", out.String(), want)
	}

	for name, tt := range map[string]struct {
		args     []string
		in       string
		out, log io.Writer
	}{
		"an unknown flag":             {args: []string{"-then"}, in: pages},
		"a time that is not RFC 3339": {args: []string{"-now", "2027-01-11"}, in: pages},
		"input that is not releases":  {in: `{"message":"Not Found"}`},
		"no release":                  {in: "[]"},
		"output that fails":           {in: pages, out: failingWriter{}},
		"a log that fails":            {in: pages, log: failingWriter{}},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			o, l := tt.out, tt.log
			if o == nil {
				o = &buf
			}
			if l == nil {
				l = &buf
			}
			if err := run(tt.args, strings.NewReader(tt.in), o, l, now); err == nil {
				t.Error("run() succeeded, want an error")
			}
		})
	}
}
