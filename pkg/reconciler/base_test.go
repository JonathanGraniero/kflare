/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package reconciler_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// newFakeClient returns a controller-runtime fake client pre-populated with
// the provided objects.
func newFakeClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// newConfigMap returns a minimal ConfigMap suitable for finalizer tests.
func newConfigMap(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
	}
}

// ---------------------------------------------------------------------------
// SetCondition
// ---------------------------------------------------------------------------

func TestSetCondition_Append(t *testing.T) {
	conditions := []metav1.Condition{}

	reconciler.SetCondition(&conditions, "Ready", metav1.ConditionTrue, "AllGood", "everything is fine", 1)

	if len(conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(conditions))
	}
	c := conditions[0]
	if c.Type != "Ready" {
		t.Errorf("Type = %q, want %q", c.Type, "Ready")
	}
	if c.Status != metav1.ConditionTrue {
		t.Errorf("Status = %v, want %v", c.Status, metav1.ConditionTrue)
	}
	if c.Reason != "AllGood" {
		t.Errorf("Reason = %q, want %q", c.Reason, "AllGood")
	}
	if c.Message != "everything is fine" {
		t.Errorf("Message = %q, want %q", c.Message, "everything is fine")
	}
	if c.ObservedGeneration != 1 {
		t.Errorf("ObservedGeneration = %d, want 1", c.ObservedGeneration)
	}
}

func TestSetCondition_Update(t *testing.T) {
	conditions := []metav1.Condition{}

	reconciler.SetCondition(&conditions, "Ready", metav1.ConditionFalse, "NotReady", "still working", 1)
	reconciler.SetCondition(&conditions, "Ready", metav1.ConditionTrue, "AllGood", "done", 2)

	if len(conditions) != 1 {
		t.Fatalf("expected 1 condition after update, got %d", len(conditions))
	}
	c := conditions[0]
	if c.Status != metav1.ConditionTrue {
		t.Errorf("Status = %v, want True after update", c.Status)
	}
	if c.ObservedGeneration != 2 {
		t.Errorf("ObservedGeneration = %d, want 2 after update", c.ObservedGeneration)
	}
}

func TestSetCondition_MultipleTypes(t *testing.T) {
	conditions := []metav1.Condition{}

	reconciler.SetCondition(&conditions, "Ready", metav1.ConditionTrue, "AllGood", "ok", 1)
	reconciler.SetCondition(&conditions, "Terminal", metav1.ConditionFalse, "NoTerminalError", "clean", 1)

	if len(conditions) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(conditions))
	}
}

func TestSetCondition_GenerationPopulated(t *testing.T) {
	conditions := []metav1.Condition{}
	reconciler.SetCondition(&conditions, "Ready", metav1.ConditionUnknown, "Initialising", "starting up", 42)

	if conditions[0].ObservedGeneration != 42 {
		t.Errorf("ObservedGeneration = %d, want 42", conditions[0].ObservedGeneration)
	}
}

// ---------------------------------------------------------------------------
// EnsureFinalizer
// ---------------------------------------------------------------------------

func TestEnsureFinalizer_AddsWhenAbsent(t *testing.T) {
	cm := newConfigMap("test-cm")
	c := newFakeClient(cm)

	added, err := reconciler.EnsureFinalizer(context.Background(), c, cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !added {
		t.Fatal("expected added=true when finalizer was absent")
	}

	// Verify the finalizer is present on the object in the fake store.
	updated := &corev1.ConfigMap{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(cm), updated)
	found := false
	for _, f := range updated.Finalizers {
		if f == reconciler.Finalizer {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("finalizer %q not found in object after EnsureFinalizer", reconciler.Finalizer)
	}
}

func TestEnsureFinalizer_NoopWhenPresent(t *testing.T) {
	cm := newConfigMap("test-cm")
	cm.Finalizers = []string{reconciler.Finalizer}
	c := newFakeClient(cm)

	added, err := reconciler.EnsureFinalizer(context.Background(), c, cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if added {
		t.Fatal("expected added=false when finalizer was already present")
	}
}

// ---------------------------------------------------------------------------
// RemoveFinalizer
// ---------------------------------------------------------------------------

func TestRemoveFinalizer_RemovesWhenPresent(t *testing.T) {
	cm := newConfigMap("test-cm")
	cm.Finalizers = []string{reconciler.Finalizer}
	c := newFakeClient(cm)

	removed, err := reconciler.RemoveFinalizer(context.Background(), c, cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !removed {
		t.Fatal("expected removed=true when finalizer was present")
	}

	// Verify the finalizer is gone.
	updated := &corev1.ConfigMap{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(cm), updated)
	for _, f := range updated.Finalizers {
		if f == reconciler.Finalizer {
			t.Errorf("finalizer %q still present after RemoveFinalizer", reconciler.Finalizer)
		}
	}
}

func TestRemoveFinalizer_NoopWhenAbsent(t *testing.T) {
	cm := newConfigMap("test-cm")
	c := newFakeClient(cm)

	removed, err := reconciler.RemoveFinalizer(context.Background(), c, cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if removed {
		t.Fatal("expected removed=false when finalizer was absent")
	}
}

func TestRemoveFinalizer_PreservesOtherFinalizers(t *testing.T) {
	cm := newConfigMap("test-cm")
	other := "other.io/finalizer"
	cm.Finalizers = []string{other, reconciler.Finalizer}
	c := newFakeClient(cm)

	_, err := reconciler.RemoveFinalizer(context.Background(), c, cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := &corev1.ConfigMap{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(cm), updated)

	hasOther := false
	hasKflare := false
	for _, f := range updated.Finalizers {
		switch f {
		case other:
			hasOther = true
		case reconciler.Finalizer:
			hasKflare = true
		}
	}
	if !hasOther {
		t.Errorf("other finalizer %q was removed unexpectedly", other)
	}
	if hasKflare {
		t.Errorf("kflare finalizer %q still present after RemoveFinalizer", reconciler.Finalizer)
	}
}

// ---------------------------------------------------------------------------
// RetainOnDelete
// ---------------------------------------------------------------------------

func TestRetainOnDelete(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{name: "no annotations", annotations: nil, want: false},
		{name: "retain", annotations: map[string]string{reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain}, want: true},
		{name: "delete", annotations: map[string]string{reconciler.DeletionPolicyAnnotation: "delete"}, want: false},
		{name: "unrelated annotation", annotations: map[string]string{"example.com/other": reconciler.DeletionPolicyRetain}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cm := newConfigMap("retain-cm")
			cm.Annotations = tc.annotations
			if got := reconciler.RetainOnDelete(cm); got != tc.want {
				t.Errorf("RetainOnDelete() = %v, want %v", got, tc.want)
			}
		})
	}
}
