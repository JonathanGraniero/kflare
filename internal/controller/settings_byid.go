/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// Settings by ID. Some cloudflare-go v7 services read and write each of a
// zone's settings by its ID, as zones.SettingService does: its Get and Edit
// take the setting's ID, and the zone has no reset. A settings kind of such a
// service (settingsKind.byID) keeps the settings its resource configures as
// one JSON object whose fields are the settings' IDs, each holding the
// setting's value, so that the shared settings reconciler compares them as it
// compares any kind's (settings.go). Its generated calls go through these
// helpers, one call to Cloudflare per setting:
//
//   - The read gets the settings the resource sets, and those that tell
//     whether one of them was configured: Cloudflare picks a zone's SSL/TLS
//     encryption mode itself while Automatic SSL/TLS is on, so the value of
//     ssl_automatic_mode tells whether someone configured ssl.
//   - A sync writes only the settings that differ (settingsShape.differing).
//   - The reset writes back, for each setting the resource sets, the value a
//     zone nobody has configured holds, or, for one Cloudflare picks, the
//     value of the setting that hands it back to Cloudflare.

// settingValue is a setting a kind whose settings go by their IDs writes: its
// ID, and its value as cloudflare-go v7's write takes it, of type V.
type settingValue[V any] struct {
	id    string
	value V
}

// settingIDs returns the IDs of the settings of lists, each once, in order.
func settingIDs[V any](lists ...[]settingValue[V]) []string {
	ids := map[string]bool{}
	for _, list := range lists {
		for _, setting := range list {
			ids[setting.id] = true
		}
	}
	return slices.Sorted(maps.Keys(ids))
}

// settingsNamed returns the settings of settings whose IDs ids names, in the
// order of settings.
func settingsNamed[V any](settings []settingValue[V], ids []string) []settingValue[V] {
	var out []settingValue[V]
	for _, setting := range settings {
		if slices.Contains(ids, setting.id) {
			out = append(out, setting)
		}
	}
	return out
}

// readSettingsByID returns the settings ids names, as Cloudflare returned
// them: a JSON object holding each setting's value under its ID, null for one
// without a value. get reads one setting, and returns it as Cloudflare
// returned it: a JSON object holding the setting's ID and value.
func readSettingsByID(ctx context.Context, ids []string,
	get func(ctx context.Context, id string) (string, error)) (string, error) {
	settings := map[string]json.RawMessage{}
	for _, id := range ids {
		raw, err := get(ctx, id)
		if err != nil {
			return "", err
		}
		var setting struct {
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal([]byte(raw), &setting); err != nil {
			return "", fmt.Errorf("reading the setting %s: %w", id, err)
		}
		// A missing value stays nil, which marshals as null.
		settings[id] = setting.Value
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// writeSettingsByID writes each setting of settings, in order, with edit,
// which writes one setting's value; it stops at the first error.
func writeSettingsByID[V any](ctx context.Context, settings []settingValue[V],
	edit func(ctx context.Context, id string, value V) error) error {
	for _, setting := range settings {
		if err := edit(ctx, setting.id, setting.value); err != nil {
			return err
		}
	}
	return nil
}

// settingsByIDJSON returns settings as the JSON object a kind whose settings
// go by their IDs compares: each setting's value under its ID, as
// cloudflare-go v7 sends the body body returns for it.
func settingsByIDJSON[V any, B json.Marshaler](settings []settingValue[V], body func(V) B) ([]byte, error) {
	out := map[string]json.RawMessage{}
	for _, setting := range settings {
		data, err := body(setting.value).MarshalJSON()
		if err != nil {
			return nil, err
		}
		var sent struct {
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(data, &sent); err != nil {
			return nil, err
		}
		if sent.Value == nil {
			return nil, fmt.Errorf("the body of the setting %s holds no value", setting.id)
		}
		out[setting.id] = sent.Value
	}
	return json.Marshal(out)
}
