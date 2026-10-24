/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// TestEveryCRDIsInTheKflareCategory checks the category that lets
// `kubectl get kflare` list the resources of every kind. A CRD gets it from
// its kind's +kubebuilder:resource marker, which kflare-gen writes for the
// generated kinds.
func TestEveryCRDIsInTheKflareCategory(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "config", "crd", "bases", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no CRDs in config/crd/bases")
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(data, &crd); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if !slices.Contains(crd.Spec.Names.Categories, "kflare") {
			t.Errorf("%s is not in the kflare category: add categories=kflare to its +kubebuilder:resource marker",
				crd.Spec.Names.Kind)
		}
	}
}
