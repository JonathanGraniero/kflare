/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"slices"
	"strings"
	"testing"
)

func TestWrap(t *testing.T) {
	long := "KVNamespaceIDLabel holds the ID of the Cloudflare KV namespace a KVNamespace manages. kflare sets it, " +
		"and never adopts a KV namespace that another KVNamespace already carries in this label."
	got := wrap(long)
	want := []string{
		"KVNamespaceIDLabel holds the ID of the Cloudflare KV namespace a KVNamespace",
		"manages. kflare sets it, and never adopts a KV namespace that another",
		"KVNamespace already carries in this label.",
	}
	if !slices.Equal(got, want) {
		t.Errorf("wrap(long) = %q, want %q", got, want)
	}
	for _, line := range got {
		if len("// "+line) > commentWidth {
			t.Errorf("line %q is longer than %d characters once commented", line, commentWidth)
		}
	}

	if got := wrap("  a   short  text "); !slices.Equal(got, []string{"a short text"}) {
		t.Errorf("wrap(short) = %q", got)
	}
	if got := wrap(""); got != nil {
		t.Errorf("wrap(\"\") = %q, want nil", got)
	}
	word := strings.Repeat("x", commentWidth+5)
	if got := wrap("a " + word + " b"); !slices.Equal(got, []string{"a", word, "b"}) {
		t.Errorf("a word longer than a line gets a line of its own, got %q", got)
	}
}

func TestComment(t *testing.T) {
	var b strings.Builder
	comment(&b, "\t", []string{"one", "", "two"})
	if got, want := b.String(), "\t// one\n\t//\n\t// two\n"; got != want {
		t.Errorf("comment = %q, want %q", got, want)
	}
}

func TestLines(t *testing.T) {
	if got := lines("first  \nsecond\n\n"); !slices.Equal(got, []string{"first", "second"}) {
		t.Errorf("lines = %q", got)
	}
	if got := lines(" \n"); got != nil {
		t.Errorf("lines of blank text = %q, want nil", got)
	}
}

// A line the block will hold cannot stand above it too: kustomize refuses a
// resource listed twice, as a kind moved onto kflare-gen could leave one.
func TestReplaceBlockRefusesALineListedTwice(t *testing.T) {
	src := []byte("resources:\n- a.yaml\n# +kflare-gen:begin\n# +kflare-gen:end\n")
	if _, err := replaceBlock(src, []string{"- b.yaml"}); err != nil {
		t.Fatal(err)
	}
	_, err := replaceBlock(src, []string{"- a.yaml"})
	if err == nil || !strings.Contains(err.Error(), "stands outside the block") {
		t.Errorf("err = %v, want one naming the line", err)
	}
}
