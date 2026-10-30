/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseStats(t *testing.T) {
	// docker stats --no-stream --format '{{json .}}', as Docker 29 prints it.
	out := `{"BlockIO":"0B / 0B","CPUPerc":"12.50%","Container":"0123","ID":"0123","MemPerc":"0.10%",` +
		`"MemUsage":"15.93MiB / 15.71GiB","Name":"kflare-sizing-0","NetIO":"0B / 0B","PIDs":"11"}
{"BlockIO":"0B / 0B","CPUPerc":"0.00%","Container":"4567","ID":"4567","MemPerc":"0.06%",` +
		`"MemUsage":"9.426MiB / 256MiB","Name":"kflare-sizing-1","NetIO":"0B / 0B","PIDs":"9"}
`
	stats, err := parseStats([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := []stat{
		{name: "kflare-sizing-0", millicpu: 125, workingMi: 15.93},
		{name: "kflare-sizing-1", millicpu: 0, workingMi: 9.426},
	}
	if !slices.Equal(stats, want) {
		t.Errorf("parseStats() = %+v, want %+v", stats, want)
	}
}

func TestParseStatsRefusesWhatItCannotRead(t *testing.T) {
	for _, line := range []string{
		`not json`,
		`{"Name":"a","CPUPerc":"--","MemUsage":"1MiB / 2MiB"}`,
		`{"Name":"a","CPUPerc":"1%","MemUsage":"1 / 2MiB"}`,
	} {
		if _, err := parseStats([]byte(line)); err == nil {
			t.Errorf("parseStats(%s) succeeded, want an error", line)
		}
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]float64{
		"512B":    512,
		"1.5KiB":  1536,
		"15.9MiB": 15.9 * (1 << 20),
		"2GiB":    2 << 30,
		"1TiB":    1 << 40,
		" 3MiB ":  3 << 20,
	} {
		got, err := parseBytes(in)
		if err != nil || got != want {
			t.Errorf("parseBytes(%q) = %v, %v, want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"12", "MiB", "x MiB"} {
		if _, err := parseBytes(in); err == nil {
			t.Errorf("parseBytes(%q) succeeded, want an error", in)
		}
	}
}

func TestAcquiredAt(t *testing.T) {
	logs := `2026-10-30T12:51:06.100000000Z {"level":"info","msg":"Attempting to acquire leader lease..."}
2026-10-30T12:51:06.200000000Z {"level":"info","msg":"Successfully acquired lease"}
2026-10-30T12:52:00.000000000Z {"level":"info","msg":"Starting workers"}
2026-10-30T12:53:10.500000000Z {"level":"info","msg":"Successfully acquired lease"}
`
	at, ok := acquiredAt([]byte(logs))
	want := time.Date(2026, 10, 30, 12, 53, 10, 500000000, time.UTC)
	if !ok || !at.Equal(want) {
		t.Errorf("acquiredAt() = %v, %v, want the last acquisition, %v", at, ok, want)
	}

	if _, ok := acquiredAt([]byte(`2026-10-30T12:51:06Z {"msg":"Attempting to acquire leader lease..."}`)); ok {
		t.Error("acquiredAt() found an acquisition in a standby's log")
	}
	if _, ok := acquiredAt([]byte(`no-timestamp Successfully acquired lease`)); ok {
		t.Error("acquiredAt() accepted a line without a timestamp")
	}
}

func TestSplitYAML(t *testing.T) {
	content := `# A comment before the first document.
apiVersion: v1
kind: A
---
# Only a comment.
---
apiVersion: v1
kind: B
`
	docs := splitYAML([]byte(content))
	if len(docs) != 2 || !strings.Contains(string(docs[0]), "kind: A") ||
		!strings.Contains(string(docs[1]), "kind: B") {
		t.Errorf("splitYAML() = %q, want the documents of kinds A and B", docs)
	}
}

func TestSampleTemplatesReadsTheRepositorysSamples(t *testing.T) {
	templates, err := sampleTemplates(filepath.Join("..", "..", "config", "samples"))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, obj := range templates {
		kinds[obj.GetKind()] = true
		if obj.GetName() == "" {
			t.Errorf("a %s sample has no name", obj.GetKind())
		}
	}
	if kinds["CloudflareAccount"] {
		t.Error("the templates include a CloudflareAccount, which would call Cloudflare")
	}
	for _, kind := range []string{"Zone", "DNSRecord", "KVNamespace", "CacheRule", "ManagedTransform"} {
		if !kinds[kind] {
			t.Errorf("the templates have no %s", kind)
		}
	}

	if _, err := sampleTemplates(t.TempDir()); err == nil {
		t.Error("sampleTemplates() of an empty directory succeeded, want an error")
	}
}

func TestParallelCreatesEveryIndexOnce(t *testing.T) {
	var seen [100]atomic.Int32
	if err := parallel(len(seen), func(i int) error { seen[i].Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	for i := range seen {
		if n := seen[i].Load(); n != 1 {
			t.Errorf("index %d created %d times", i, n)
		}
	}
}

func TestParallelReturnsAnError(t *testing.T) {
	boom := errors.New("boom")
	err := parallel(1000, func(i int) error {
		if i == 10 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Errorf("parallel() = %v, want %v", err, boom)
	}
}

func TestSummarize(t *testing.T) {
	stats := []stat{{millicpu: 2, workingMi: 10}, {millicpu: 6, workingMi: 14}}
	r := summarize("idle", "c", "leader", time.Minute, stats)
	want := result{phase: "idle", container: "c", role: "leader", duration: time.Minute, samples: 2,
		cpuMean: 4, cpuMax: 6, memMean: 12, memMax: 14, stats: stats}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("summarize() = %+v, want %+v", r, want)
	}
	if empty := summarize("idle", "c", "standby", 0, nil); empty.cpuMean != 0 || empty.samples != 0 {
		t.Errorf("summarize() of no samples = %+v", empty)
	}
}

func TestWriteTable(t *testing.T) {
	var out strings.Builder
	err := writeTable(&out, []result{{phase: "failover", container: "kflare-sizing-1", role: "leader",
		duration: 90 * time.Second, samples: 45, cpuMean: 15.4, cpuMax: 114, memMean: 10.12, memMax: 16.2}})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || strings.Join(strings.Fields(lines[1]), " ") !=
		"failover kflare-sizing-1 leader 1m30s 45 15 114 10.1 16.2" {
		t.Errorf("writeTable() wrote:\n%s", out.String())
	}
}

func TestValidate(t *testing.T) {
	valid := options{image: "controller:latest", assets: "bin/k8s", replicas: 2, window: time.Minute}
	if err := valid.validate(); err != nil {
		t.Fatalf("validate() = %v for valid options", err)
	}
	for name, change := range map[string]func(*options){
		"no image":           func(o *options) { o.image = "" },
		"no assets":          func(o *options) { o.assets = "" },
		"no replicas":        func(o *options) { o.replicas = 0 },
		"negative secrets":   func(o *options) { o.secrets = -1 },
		"negative bytes":     func(o *options) { o.secretBytes = -1 },
		"negative resources": func(o *options) { o.resources = -1 },
		"no window":          func(o *options) { o.window = 0 },
	} {
		o := valid
		change(&o)
		if err := o.validate(); err == nil {
			t.Errorf("validate() accepted options with %s", name)
		}
	}
}

func TestWriteSamples(t *testing.T) {
	var out strings.Builder
	err := writeSamples(&out, []result{{phase: "failover", container: "kflare-sizing-1", role: "leader",
		stats: []stat{{at: 1500 * time.Millisecond, millicpu: 114, workingMi: 16.2}}}})
	if err != nil {
		t.Fatal(err)
	}
	want := "phase,container,role,seconds,cpu_millicores,working_set_mib\n" +
		"failover,kflare-sizing-1,leader,1.5,114.0,16.20\n"
	if out.String() != want {
		t.Errorf("writeSamples() wrote %q, want %q", out.String(), want)
	}
}
