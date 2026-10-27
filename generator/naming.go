/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"strings"
	"unicode"
)

// lowerCamel returns the JSON name for a Go name: its first word in lower
// case, an initialism counting as one word. Title is title, ID id, NamespaceID
// namespaceID, URLPattern urlPattern, SupportsURLEncoding supportsURLEncoding
// and TLS13 tls13. cloudflare-go v7 already spells its Go names with Go's
// initialisms, so the JSON names follow them.
func lowerCamel(goName string) string {
	runes := []rune(goName)
	upper := 0
	for upper < len(runes) && unicode.IsUpper(runes[upper]) {
		upper++
	}
	switch {
	case upper == len(runes):
		return strings.ToLower(goName)
	case upper > 1 && unicode.IsLower(runes[upper]):
		// An initialism followed by a word: its last capital starts the word.
		upper--
	}
	return strings.ToLower(string(runes[:upper])) + string(runes[upper:])
}

// upperCamel returns the Go name for a JSON name in lowerCamel case.
func upperCamel(jsonName string) string {
	runes := []rune(jsonName)
	if len(runes) == 0 {
		return ""
	}
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// humanize returns a column header for a JSON name in lowerCamel case:
// namespaceID is "Namespace ID" and supportsURLEncoding "Supports URL
// Encoding".
func humanize(jsonName string) string {
	runes := []rune(jsonName)
	var words []string
	start := 0
	for i := 1; i < len(runes); i++ {
		lowerBefore := unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])
		initialismEnds := unicode.IsUpper(runes[i-1]) && i+1 < len(runes) && unicode.IsLower(runes[i+1])
		if unicode.IsUpper(runes[i]) && (lowerBefore || initialismEnds) {
			words = append(words, string(runes[start:i]))
			start = i
		}
	}
	words = append(words, string(runes[start:]))
	for i, word := range words {
		words[i] = upperCamel(word)
	}
	return strings.Join(words, " ")
}

// plural returns a kind's resource name: the kind in lower case, made plural
// as English makes a regular noun plural. KVNamespace is kvnamespaces,
// AccessPolicy accesspolicies and AddressMap addressmaps. A kind named for
// settings is plural already: ZoneSettings is zonesettings. The types emitter
// gives controller-gen the name, so that the CRD, the RBAC rules and the
// files kflare-gen names agree.
func plural(kind string) string {
	lower := strings.ToLower(kind)
	switch {
	case strings.HasSuffix(kind, "Settings"):
		return lower
	case strings.HasSuffix(lower, "y") && len(lower) > 1 && !strings.ContainsRune("aeiou", rune(lower[len(lower)-2])):
		return strings.TrimSuffix(lower, "y") + "ies"
	case strings.HasSuffix(lower, "s"), strings.HasSuffix(lower, "x"), strings.HasSuffix(lower, "z"),
		strings.HasSuffix(lower, "ch"), strings.HasSuffix(lower, "sh"):
		return lower + "es"
	}
	return lower + "s"
}
