/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package gitopshealth

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// Group is the API group of kflare's kinds, which both health checks cover whole.
const Group = "kflare.dev"

// The paths of kflare's health checks, from the repository's root.
const (
	ArgoCDScriptPath     = "config/argocd/health.lua"
	FluxHealthChecksPath = "config/flux/health-check-exprs.yaml"
)

// Checks holds kflare's health checks for Argo CD and Flux, as the repository has them.
type Checks struct {
	ArgoCDScript string
	FluxCheck    FluxHealthCheck
}

// Load reads kflare's health checks from the repository at root. The Flux file must hold one entry, for every kind
// of kflare's group.
func Load(root string) (*Checks, error) {
	script, err := os.ReadFile(filepath.Join(root, ArgoCDScriptPath))
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(root, FluxHealthChecksPath))
	if err != nil {
		return nil, err
	}
	var flux []FluxHealthCheck
	if err := yaml.UnmarshalStrict(data, &flux); err != nil {
		return nil, fmt.Errorf("reading %s: %w", FluxHealthChecksPath, err)
	}
	if len(flux) != 1 {
		return nil, fmt.Errorf("%s holds %d entries, not one", FluxHealthChecksPath, len(flux))
	}
	if gv, err := schema.ParseGroupVersion(flux[0].APIVersion); err != nil || gv.Group != Group || flux[0].Kind != "" {
		return nil, fmt.Errorf("%s's entry is for %s %s, not every kind of %s", FluxHealthChecksPath,
			flux[0].APIVersion, flux[0].Kind, Group)
	}
	return &Checks{ArgoCDScript: string(script), FluxCheck: flux[0]}, nil
}

// ArgoCD returns the health Argo CD gives obj with kflare's health check (RunArgoCDHealthScript).
func (c *Checks) ArgoCD(obj runtime.Object) (ArgoCDHealth, error) {
	content, err := toUnstructured(obj)
	if err != nil {
		return ArgoCDHealth{}, err
	}
	return RunArgoCDHealthScript(c.ArgoCDScript, content)
}

// Flux returns the status Flux gives obj with kflare's health check expressions (EvaluateFluxHealthCheck).
func (c *Checks) Flux(obj runtime.Object) (string, error) {
	content, err := toUnstructured(obj)
	if err != nil {
		return "", err
	}
	return EvaluateFluxHealthCheck(c.FluxCheck, content)
}

// toUnstructured converts obj to unstructured content, with int64 integers, as the API server's JSON decodes to.
func toUnstructured(obj runtime.Object) (map[string]any, error) {
	if u, ok := obj.(runtime.Unstructured); ok {
		return u.UnstructuredContent(), nil
	}
	return runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
}
