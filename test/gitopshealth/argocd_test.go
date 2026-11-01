/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package gitopshealth

import (
	"strings"
	"testing"
	"time"
)

// TestRunArgoCDHealthScript checks that scripts run as Argo CD runs one from argocd-cm (util/lua in v3.5.6).
func TestRunArgoCDHealthScript(t *testing.T) {
	obj := map[string]any{
		"metadata": map[string]any{"name": "www", "generation": int64(3), "labels": map[string]any{"a": "b"}},
		"spec":     map[string]any{"ratio": 0.5, "on": true, "items": []any{"x", "y"}, "none": nil},
	}
	for _, tc := range []struct {
		name, script string
		want         ArgoCDHealth
		wantErr      string
	}{{
		name: "obj holds the object, with integers as numbers",
		script: `return {status = "Healthy", message = obj.metadata.name .. " " .. tostring(obj.metadata.generation + 1) ..
			" " .. tostring(obj.spec.ratio) .. " " .. tostring(obj.spec.on) .. " " .. obj.spec.items[2] .. " " ..
			obj.metadata.labels.a .. " " .. tostring(obj.spec.none)}`,
		want: ArgoCDHealth{Status: ArgoCDHealthy, Message: "www 4 0.5 true y b nil"},
	}, {
		name:   "the table library is open",
		script: `local t = {"a", "b"}; return {status = "Progressing", message = table.concat(t, ",")}`,
		want:   ArgoCDHealth{Status: ArgoCDProgressing, Message: "a,b"},
	}, {
		name:    "the string library is closed",
		script:  `return {status = string.lower("Healthy")}`,
		wantErr: "attempt to index a non-table object(nil) with key 'lower'",
	}, {
		name:    "the os library is closed, though Argo CD opens a safe part of it, which kflare's script does not use",
		script:  `return {status = "Healthy", message = tostring(os.time())}`,
		wantErr: "attempt to index a non-table object(nil) with key 'time'",
	}, {
		name:   "a status Argo CD does not know is Unknown",
		script: `return {status = "Fine"}`,
		want:   ArgoCDHealth{Status: ArgoCDUnknown, Message: argoCDInvalidStatus},
	}, {
		name:   "nil is no health",
		script: `return nil`,
		want:   ArgoCDHealth{},
	}, {
		name:   "an empty table, an array once encoded, is no health",
		script: `return {}`,
		want:   ArgoCDHealth{},
	}, {
		name:   "fields a health status does not have are ignored",
		script: `return {status = "Suspended", other = {1, 2}, more = {a = 1, b = true, c = "d"}}`,
		want:   ArgoCDHealth{Status: ArgoCDSuspended},
	}, {
		name:    "a status that is not a string fails",
		script:  `return {status = 1}`,
		wantErr: "reading the health check's result",
	}, {
		name:    "a table with both kinds of keys fails",
		script:  `return {status = "Healthy", other = {1, a = 2}}`,
		wantErr: "both string and number keys",
	}, {
		name:    "a function fails",
		script:  `return {status = "Healthy", other = print}`,
		wantErr: "cannot encode function",
	}, {
		name:    "anything but a table or nil fails",
		script:  `return "Healthy"`,
		wantErr: "expect table output from Lua script, not string",
	}, {
		name:    "a script that does not parse fails",
		script:  `return {`,
		wantErr: "syntax error",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RunArgoCDHealthScript(tc.script, obj)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error is %v, want one about %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("health is %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestRunArgoCDHealthScriptTimesOut checks that a script gets a second to run, as in Argo CD.
func TestRunArgoCDHealthScriptTimesOut(t *testing.T) {
	start := time.Now()
	_, err := RunArgoCDHealthScript(`while true do end`, nil)
	if err == nil {
		t.Fatal("a script that never ends returned")
	}
	if elapsed := time.Since(start); elapsed < argoCDScriptTimeout || elapsed > 5*argoCDScriptTimeout {
		t.Errorf("the script ran %s, want about %s", elapsed, argoCDScriptTimeout)
	}
}
