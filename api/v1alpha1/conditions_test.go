/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConditionAccessors(t *testing.T) {
	objects := map[string]interface {
		GetConditions() []metav1.Condition
		SetConditions([]metav1.Condition)
	}{
		"CloudflareAccount":   &CloudflareAccount{},
		"Zone":                &Zone{},
		"DNSRecord":           &DNSRecord{},
		"Tunnel":              &Tunnel{},
		"TunnelConfiguration": &TunnelConfiguration{},
		"WorkerScript":        &WorkerScript{},
	}
	want := []metav1.Condition{{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: "Synced"}}
	for kind, obj := range objects {
		obj.SetConditions(want)
		got := obj.GetConditions()
		if len(got) != 1 || got[0] != want[0] {
			t.Errorf("%s: GetConditions() = %v, want %v", kind, got, want)
		}
	}
}
