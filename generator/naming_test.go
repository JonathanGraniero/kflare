/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import "testing"

func TestLowerCamel(t *testing.T) {
	for goName, want := range map[string]string{
		"Title":               "title",
		"ID":                  "id",
		"NamespaceID":         "namespaceID",
		"URLPattern":          "urlPattern",
		"SupportsURLEncoding": "supportsURLEncoding",
		"CreatedOn":           "createdOn",
		"TLS13":               "tls13",
		"IPV6":                "ipv6",
		"title":               "title",
		"":                    "",
	} {
		if got := lowerCamel(goName); got != want {
			t.Errorf("lowerCamel(%q) = %q, want %q", goName, got, want)
		}
	}
}

func TestUpperCamel(t *testing.T) {
	for jsonName, want := range map[string]string{"namespaceID": "NamespaceID", "listID": "ListID", "": ""} {
		if got := upperCamel(jsonName); got != want {
			t.Errorf("upperCamel(%q) = %q, want %q", jsonName, got, want)
		}
	}
}

func TestHumanize(t *testing.T) {
	for jsonName, want := range map[string]string{
		"namespaceID":         "Namespace ID",
		"supportsURLEncoding": "Supports URL Encoding",
		"createdOn":           "Created On",
		"listID":              "List ID",
		"title":               "Title",
		"ipv4":                "Ipv4",
	} {
		if got := humanize(jsonName); got != want {
			t.Errorf("humanize(%q) = %q, want %q", jsonName, got, want)
		}
	}
}

func TestPlural(t *testing.T) {
	for kind, want := range map[string]string{
		"KVNamespace":  "kvnamespaces",
		"AccessPolicy": "accesspolicies",
		"Gateway":      "gateways",
		"Address":      "addresses",
		"Box":          "boxes",
		"Buzz":         "buzzes",
		"Branch":       "branches",
		"Mesh":         "meshes",
		"Y":            "ys",
		"ZoneSettings": "zonesettings",
	} {
		if got := plural(kind); got != want {
			t.Errorf("plural(%q) = %q, want %q", kind, got, want)
		}
	}
}
