/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package gitopshealth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

// TestLoadRefusesWhatTheDeployGuideCouldNotUse checks that Load fails on missing files, and on a Flux file that is not
// one entry for every kind of kflare's group.
func TestLoadRefusesWhatTheDeployGuideCouldNotUse(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(root, ArgoCDScriptPath))
	if err != nil {
		t.Fatal(err)
	}
	const valid = "- apiVersion: kflare.dev/v1alpha1\n  current: 'true'\n"
	for _, tc := range []struct {
		name, flux, wantErr string
		noScript, noFlux    bool
	}{
		{name: "no script", noScript: true, wantErr: "health.lua"},
		{name: "no Flux file", noFlux: true, wantErr: "health-check-exprs.yaml"},
		{name: "an unknown field", flux: "- apiVersion: kflare.dev/v1alpha1\n  current: 'true'\n  extra: x\n",
			wantErr: "unknown field"},
		{name: "two entries", flux: valid + valid, wantErr: "holds 2 entries"},
		{name: "another group", flux: "- apiVersion: example.com/v1\n  current: 'true'\n", wantErr: "not every kind"},
		{name: "one kind", flux: "- apiVersion: kflare.dev/v1alpha1\n  kind: Zone\n  current: 'true'\n",
			wantErr: "not every kind"},
		{name: "valid", flux: valid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, file := range []struct {
				path, content string
				skip          bool
			}{
				{ArgoCDScriptPath, string(script), tc.noScript},
				{FluxHealthChecksPath, tc.flux, tc.noFlux},
			} {
				if file.skip {
					continue
				}
				path := filepath.Join(dir, file.path)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(file.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			checks, err := Load(dir)
			if tc.wantErr == "" {
				if err != nil || checks.ArgoCDScript != string(script) || checks.FluxCheck.Current != "true" {
					t.Errorf("Load returned %+v, %v", checks, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error is %v, want one about %q", err, tc.wantErr)
			}
		})
	}
}

// TestChecksJudgeTypedObjects checks that the checks judge the API types the controllers write as they judge the
// same object read from the API server.
func TestChecksJudgeTypedObjects(t *testing.T) {
	checks := loadChecks(t)
	record := &cloudflarev1alpha1.DNSRecord{
		ObjectMeta: metav1.ObjectMeta{Name: "www", Namespace: "web", Generation: 2},
		Status: cloudflarev1alpha1.DNSRecordStatus{Conditions: []metav1.Condition{{
			Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "TerminalError",
			Message: "refused", ObservedGeneration: 2,
		}}},
	}
	if health, err := checks.ArgoCD(record); err != nil || health != (ArgoCDHealth{Status: ArgoCDDegraded,
		Message: "refused"}) {
		t.Errorf("Argo CD's health is %+v (error: %v), want Degraded", health, err)
	}
	if status, err := checks.Flux(record); err != nil || status != FluxFailed {
		t.Errorf("Flux's status is %s (error: %v), want Failed", status, err)
	}
}

// TestLuaValue checks the conversion of the values unstructured content can hold, as Argo CD's decodeValue does.
func TestLuaValue(t *testing.T) {
	health, err := RunArgoCDHealthScript(`return {status = "Healthy", message = tostring(obj.a) .. " " ..
		tostring(obj.b) .. " " .. tostring(obj.c) .. " " .. type(obj.c) .. " " .. tostring(obj.d)}`,
		map[string]any{"a": 1, "b": int32(2), "c": json.Number("3"), "d": struct{}{}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "1 2 3 string nil"; health.Message != want {
		t.Errorf("message is %q, want %q", health.Message, want)
	}
}

// TestJSONValueRefusesWhatGopherJSONCannotEncode checks the results Argo CD cannot read.
func TestJSONValueRefusesWhatGopherJSONCannotEncode(t *testing.T) {
	for _, tc := range []struct{ script, wantErr string }{
		{`local t = {}; t[1] = "a"; t[3] = "c"; return {status = "Healthy", other = t}`, "sparse array"},
		{`return {status = "Healthy", other = {[true] = 1}}`, "boolean key"},
	} {
		_, err := RunArgoCDHealthScript(tc.script, nil)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error is %v, want one about %q", tc.script, err, tc.wantErr)
		}
	}
}
