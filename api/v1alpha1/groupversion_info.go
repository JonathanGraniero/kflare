/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

// Package v1alpha1 contains API Schema definitions for the cloudflare v1alpha1 API group
// +kubebuilder:object:generate=true
// +groupName=kflare.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// This package depends only on k8s.io/apimachinery, so other projects can
// import the kflare API types without pulling in controller-runtime.

var (
	// GroupVersion is group version used to register these objects
	GroupVersion = schema.GroupVersion{Group: "kflare.dev", Version: "v1alpha1"}

	// SchemeBuilder collects the functions that add this group-version's types
	// to a scheme. Each type registers itself with register in an init function.
	SchemeBuilder = runtime.NewSchemeBuilder(addGroupVersion)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// addGroupVersion adds the meta types every group-version needs (ListOptions,
// WatchEvent, ...).
func addGroupVersion(s *runtime.Scheme) error {
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// register adds objects to the group-version when AddToScheme runs.
func register(objects ...runtime.Object) {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, objects...)
		return nil
	})
}
