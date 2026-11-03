/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package gitopshealth

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestEvaluateFluxHealthCheck checks that expressions are evaluated as Flux evaluates them (fluxcd/pkg/runtime/cel).
func TestEvaluateFluxHealthCheck(t *testing.T) {
	obj := map[string]any{
		"metadata": map[string]any{"name": "www", "generation": int64(2)},
		"status":   map[string]any{"phase": "Ready", "count": int64(3)},
	}
	for _, tc := range []struct {
		name    string
		check   FluxHealthCheck
		obj     map[string]any
		want    string
		wantErr string
	}{{
		name:  "inProgress is evaluated first",
		check: FluxHealthCheck{InProgress: "true", Failed: "true", Current: "true"},
		want:  FluxInProgress,
	}, {
		name:  "failed before current",
		check: FluxHealthCheck{InProgress: "false", Failed: "true", Current: "true"},
		want:  FluxFailed,
	}, {
		name:  "current last",
		check: FluxHealthCheck{Failed: "status.phase == 'Failed'", Current: "status.phase == 'Ready'"},
		want:  FluxCurrent,
	}, {
		name:  "none true is InProgress",
		check: FluxHealthCheck{Failed: "false", Current: "false"},
		want:  FluxInProgress,
	}, {
		name:  "integers compare across types",
		check: FluxHealthCheck{Current: "status.count == 3.0 && metadata.generation < status.count"},
		want:  FluxCurrent,
	}, {
		name:  "Flux's string extensions are there",
		check: FluxHealthCheck{Current: "status.phase.lowerAscii() == 'ready'"},
		want:  FluxCurrent,
	}, {
		name:  "a status.observedGeneration behind the generation is InProgress before any expression",
		check: FluxHealthCheck{Current: "true"},
		obj: map[string]any{
			"metadata": map[string]any{"generation": int64(2)},
			"status":   map[string]any{"observedGeneration": int64(1)},
		},
		want: FluxInProgress,
	}, {
		name:  "a status.observedGeneration at the generation leaves it to the expressions",
		check: FluxHealthCheck{Current: "true"},
		obj: map[string]any{
			"metadata": map[string]any{"generation": int64(2)},
			"status":   map[string]any{"observedGeneration": int64(2)},
		},
		want: FluxCurrent,
	}, {
		name:    "a field the object lacks is an error, and Unknown",
		check:   FluxHealthCheck{Current: "status.conditions.exists(c, c.type == 'Ready')"},
		obj:     map[string]any{"metadata": map[string]any{"generation": int64(1)}},
		want:    FluxUnknown,
		wantErr: "evaluating",
	}, {
		name:    "an expression that does not parse is an error",
		check:   FluxHealthCheck{Current: "status.phase =="},
		want:    FluxUnknown,
		wantErr: "parsing",
	}, {
		name:    "an expression that is not a bool is an error",
		check:   FluxHealthCheck{Current: "status.phase"},
		want:    FluxUnknown,
		wantErr: "not a bool",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.obj == nil {
				tc.obj = obj
			}
			got, err := EvaluateFluxHealthCheck(tc.check, tc.obj)
			if got != tc.want {
				t.Errorf("status is %s (error: %v), want %s", got, err, tc.want)
			}
			if tc.wantErr == "" && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Errorf("error is %v, want one about %q", err, tc.wantErr)
			}
		})
	}
}

// TestFluxHealthCheckApplies checks which objects an entry judges: those of its group, of its kind if it names one,
// whatever their version.
func TestFluxHealthCheckApplies(t *testing.T) {
	dnsRecord := schema.GroupVersionKind{Group: Group, Version: "v1beta1", Kind: "DNSRecord"}
	for _, tc := range []struct {
		check FluxHealthCheck
		want  bool
	}{
		{FluxHealthCheck{APIVersion: "kflare.dev/v1alpha1"}, true},
		{FluxHealthCheck{APIVersion: "kflare.dev/v1alpha1", Kind: "DNSRecord"}, true},
		{FluxHealthCheck{APIVersion: "kflare.dev/v1alpha1", Kind: "Zone"}, false},
		{FluxHealthCheck{APIVersion: "example.com/v1"}, false},
		{FluxHealthCheck{APIVersion: "v1"}, false},
		{FluxHealthCheck{APIVersion: "a/b/c"}, false},
	} {
		if got := tc.check.Applies(dnsRecord); got != tc.want {
			t.Errorf("%+v applies to %s: %t, want %t", tc.check, dnsRecord, got, tc.want)
		}
	}
}
