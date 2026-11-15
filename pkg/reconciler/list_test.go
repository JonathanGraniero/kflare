/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package reconciler_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

func listFixture() client.Client {
	a, b := newConfigMap("a"), newConfigMap("b")
	other := newConfigMap("c")
	other.Namespace = "other"
	return newFakeClient(a, b, other)
}

func TestList(t *testing.T) {
	got, err := reconciler.List[*corev1.ConfigMap](context.Background(), listFixture(), &corev1.ConfigMapList{},
		client.InNamespace("default"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(got))
	for _, cm := range got {
		names = append(names, cm.Name)
	}
	if !slices.Equal(names, []string{"a", "b"}) {
		t.Errorf("List = %v, want [a b]", names)
	}
}

func TestListRefusesAnotherType(t *testing.T) {
	if _, err := reconciler.List[*corev1.Secret](context.Background(), listFixture(),
		&corev1.ConfigMapList{}); err == nil {
		t.Error("List took ConfigMaps for Secrets")
	}
}

func TestListReturnsTheListError(t *testing.T) {
	failed := errors.New("list failed")
	c := interceptor.NewClient(listFixture().(client.WithWatch), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return failed },
	})
	if _, err := reconciler.List[*corev1.ConfigMap](context.Background(), c, &corev1.ConfigMapList{}); err != failed {
		t.Errorf("List error = %v, want %v", err, failed)
	}
	if reqs := reconciler.RequestsWhere(context.Background(), c, &corev1.ConfigMapList{}, nil,
		func(*corev1.ConfigMap) bool { return true }); reqs != nil {
		t.Errorf("RequestsWhere = %v after a failed list, want none", reqs)
	}
}

func TestRequestsWhere(t *testing.T) {
	got := reconciler.RequestsWhere(context.Background(), listFixture(), &corev1.ConfigMapList{}, nil,
		func(cm *corev1.ConfigMap) bool { return cm.Name != "a" })
	want := []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: "default", Name: "b"}},
		{NamespacedName: types.NamespacedName{Namespace: "other", Name: "c"}},
	}
	if !slices.Equal(got, want) {
		t.Errorf("RequestsWhere = %v, want %v", got, want)
	}
	opts := []client.ListOption{client.InNamespace(metav1.NamespaceDefault)}
	if got := reconciler.RequestsWhere(context.Background(), listFixture(), &corev1.ConfigMapList{}, opts,
		func(*corev1.ConfigMap) bool { return true }); len(got) != 2 {
		t.Errorf("RequestsWhere in default = %v, want a and b", got)
	}
}
