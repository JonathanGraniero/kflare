/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package gitopshealth

import (
	"context"
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/ext"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The statuses of kstatus that a Flux health check gives, which Flux waits on: a Kustomization or HelmRelease that
// waits is ready once all of its objects are Current, and fails as soon as one is Failed.
const (
	FluxCurrent    = "Current"
	FluxInProgress = "InProgress"
	FluxFailed     = "Failed"
	FluxUnknown    = "Unknown"
)

// FluxHealthCheck is an entry of a Kustomization's or HelmRelease's spec.healthCheckExprs: CEL expressions that judge
// the objects of a group, or of one of its kinds.
type FluxHealthCheck struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind,omitempty"`
	InProgress string `json:"inProgress,omitempty"`
	Failed     string `json:"failed,omitempty"`
	Current    string `json:"current"`
}

// Applies reports whether Flux judges objects of gvk with check: those of its apiVersion's group, all of them when
// it names no kind. The version does not matter.
func (check FluxHealthCheck) Applies(gvk schema.GroupVersionKind) bool {
	gv, err := schema.ParseGroupVersion(check.APIVersion)
	if err != nil {
		return false
	}
	return gv.Group == gvk.Group && (check.Kind == "" || check.Kind == gvk.Kind)
}

// EvaluateFluxHealthCheck judges obj with check, as Flux v2.10 does in fluxcd/pkg/runtime/cel (StatusEvaluator and
// NewExpression), and returns the status it gives:
//   - A status.observedGeneration that differs from metadata.generation gives InProgress. kflare sets none.
//   - Then inProgress, failed and current are evaluated in turn, and the first that is true gives its status;
//     none gives InProgress. Each expression is parsed, not compiled, in Flux's CEL environment, with the object's
//     top-level fields as its variables.
//   - An expression that fails to evaluate, such as one that reads status before the object has one, fails the
//     check: the error is returned with Unknown, which is what kstatus' generic status reader makes of it, so that
//     Flux goes on waiting.
//
// obj is an object as the API server returns it, decoded into unstructured content.
func EvaluateFluxHealthCheck(check FluxHealthCheck, obj map[string]any) (string, error) {
	observed, found, err := unstructured.NestedInt64(obj, "status", "observedGeneration")
	if err == nil && found {
		generation, found, err := unstructured.NestedInt64(obj, "metadata", "generation")
		if err == nil && found && observed != generation {
			return FluxInProgress, nil
		}
	}
	for _, step := range []struct{ expr, status string }{
		{check.InProgress, FluxInProgress},
		{check.Failed, FluxFailed},
		{check.Current, FluxCurrent},
	} {
		if step.expr == "" {
			continue
		}
		result, err := evaluateFluxExpression(step.expr, obj)
		if err != nil {
			return FluxUnknown, err
		}
		if result {
			return step.status, nil
		}
	}
	return FluxInProgress, nil
}

// evaluateFluxExpression evaluates a boolean CEL expression on obj in Flux's environment (fluxcd/pkg/runtime/cel's
// NewExpression without WithCompile, and Expression.EvaluateBoolean).
func evaluateFluxExpression(expr string, obj map[string]any) (bool, error) {
	env, err := cel.NewEnv(
		cel.HomogeneousAggregateLiterals(),
		cel.EagerlyValidateDeclarations(true),
		cel.DefaultUTCTimeZone(true),
		cel.CrossTypeNumericComparisons(true),
		cel.OptionalTypes(),
		ext.Strings(),
		ext.Sets(),
		ext.Encoders(),
	)
	if err != nil {
		return false, fmt.Errorf("creating the CEL environment: %w", err)
	}
	ast, issues := env.Parse(expr)
	if issues != nil && issues.Err() != nil {
		return false, fmt.Errorf("parsing the CEL expression %q: %w", expr, issues.Err())
	}
	program, err := env.Program(ast, cel.EvalOptions(cel.OptOptimize), cel.InterruptCheckFrequency(100))
	if err != nil {
		return false, fmt.Errorf("creating the CEL program of %q: %w", expr, err)
	}
	value, _, err := program.ContextEval(context.Background(), obj)
	if err != nil {
		return false, fmt.Errorf("evaluating the CEL expression %q: %w", expr, err)
	}
	result, ok := value.(types.Bool)
	if !ok {
		return false, fmt.Errorf("the CEL expression %q returns %s, not a bool", expr, value.Type())
	}
	return bool(result), nil
}
