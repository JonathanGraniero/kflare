/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// Settings. A settings kind's resources each configure one product's
// settings in a zone (settings_controller.go), and kflare compares them as
// JSON: the settings a resource asks for, as cloudflare-go v7 sends them,
// against Cloudflare's copy, as v7 received it. This file holds that
// comparison, and what tells settings configured outside kflare from those of
// a zone nobody has configured.
//
// A path names a place in the settings: the API names of the fields leading
// to it, joined by dots, a list's items marked by [] after its name, as in
// managed_request_headers[].enabled.

// settingsValue is the value the settings of a zone nobody has configured
// hold at path.
type settingsValue struct {
	path  string
	value any
}

// settingsShape is what comparing a kind's settings needs to know of them,
// by path: the lists whose items are keyed, by the field that tells them
// apart, and the sets, whose order does not matter.
type settingsShape struct {
	listKeys map[string]string
	sets     map[string]bool
}

// matches reports whether actual, the settings as Cloudflare returned them,
// already hold desired, the settings a resource asks for as cloudflare-go v7
// sends them. Every value desired holds must be actual's: an object's fields
// one by one, a keyed list's items by their key, a set's items in any order,
// and another list's items in order. What desired leaves out is not compared,
// so kflare never fights Cloudflare over a value it does not manage; its nulls
// and empty objects and lists count as left out.
func (s settingsShape) matches(desired, actual []byte) (bool, error) {
	var want, got any
	if err := json.Unmarshal(desired, &want); err != nil {
		return false, err
	}
	if err := json.Unmarshal(actual, &got); err != nil {
		return false, err
	}
	return s.holds(withoutEmpty(want), got, ""), nil
}

// holds reports whether got, the value at path in the settings Cloudflare
// returned, holds want, the one a resource asks for there.
func (s settingsShape) holds(want, got any, path string) bool {
	switch w := want.(type) {
	case map[string]any:
		// A value that is not an object holds none of the fields.
		g, _ := got.(map[string]any)
		for name, value := range w {
			if !s.holds(value, g[name], settingsPath(path, name)) {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok {
			return false
		}
		items := path + "[]"
		if key, keyed := s.listKeys[path]; keyed {
			for _, item := range w {
				i := slices.IndexFunc(g, func(other any) bool { return sameKey(item, other, key) })
				if i < 0 || !s.holds(item, g[i], items) {
					return false
				}
			}
			return true
		}
		if len(w) != len(g) {
			return false
		}
		if s.sets[path] {
			return sameItems(w, g)
		}
		for i := range w {
			if !s.holds(w[i], g[i], items) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(want, got)
}

// differing returns, for a kind whose settings go by their IDs, the IDs of the
// settings that actual, as Cloudflare returned them, does not already hold as
// desired asks for them, compared one by one as matches compares them, in
// order: the settings a sync writes.
func (s settingsShape) differing(desired, actual []byte) ([]string, error) {
	var want any
	if err := json.Unmarshal(desired, &want); err != nil {
		return nil, err
	}
	var got map[string]any
	if err := json.Unmarshal(actual, &got); err != nil {
		return nil, err
	}
	settings, _ := withoutEmpty(want).(map[string]any)
	var ids []string
	for _, id := range slices.Sorted(maps.Keys(settings)) {
		if !s.holds(settings[id], got[id], id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// settingsPath returns the path of the field name of the object at path.
func settingsPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// sameKey reports whether two items of a keyed list, as JSON decodes them,
// hold the same key.
func sameKey(a, b any, key string) bool {
	am, ok := a.(map[string]any)
	if !ok {
		return false
	}
	bm, ok := b.(map[string]any)
	if !ok {
		return false
	}
	value, ok := am[key]
	return ok && reflect.DeepEqual(value, bm[key])
}

// sameItems reports whether two lists of the same length hold the same items,
// each as many times, in any order.
func sameItems(a, b []any) bool {
	counts := map[string]int{}
	for _, item := range a {
		counts[canonicalJSON(item)]++
	}
	for _, item := range b {
		key := canonicalJSON(item)
		if counts[key] == 0 {
			return false
		}
		counts[key]--
	}
	return true
}

// canonicalJSON writes a decoded JSON value with its objects' keys sorted, so
// that equal values write alike.
func canonicalJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		// A value json.Unmarshal decoded always marshals.
		panic(err)
	}
	return string(data)
}

// settingsConfigured reports whether actual, the settings as Cloudflare
// returned them, were configured: they hold another value, at one of the
// paths of unconfigured, than a zone nobody has configured holds there. A
// path the settings do not reach, or reach with a null, says nothing either
// way.
func settingsConfigured(actual []byte, unconfigured []settingsValue) (bool, error) {
	var settings any
	if err := json.Unmarshal(actual, &settings); err != nil {
		return false, err
	}
	for _, v := range unconfigured {
		for _, found := range valuesAt(settings, strings.Split(v.path, ".")) {
			if found != nil && !reflect.DeepEqual(found, v.value) {
				return true, nil
			}
		}
	}
	return false, nil
}

// valuesAt returns the values a decoded JSON value holds at the path made of
// steps: one for a path through objects, one for each item of a list it goes
// through, none when it ends early.
func valuesAt(value any, steps []string) []any {
	if len(steps) == 0 {
		return []any{value}
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	name, list := strings.CutSuffix(steps[0], "[]")
	next, ok := object[name]
	if !ok {
		return nil
	}
	if !list {
		return valuesAt(next, steps[1:])
	}
	items, _ := next.([]any)
	var out []any
	for _, item := range items {
		out = append(out, valuesAt(item, steps[1:])...)
	}
	return out
}
