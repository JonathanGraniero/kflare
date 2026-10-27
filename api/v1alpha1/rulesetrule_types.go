/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The types here are shared by every kind that manages one rule in a zone's
// Rulesets API phase: WAFCustomRule, and the rule kinds kflare-gen generates,
// such as RateLimitRule and CacheRule.

// RulesetRuleSpec holds the spec fields every rule kind shares. Each kind
// embeds it inline, next to the fields of its action.
type RulesetRuleSpec struct {
	// ZoneRef references the Zone, in the same namespace, the rule belongs to.
	// Immutable after creation.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="zoneRef is immutable"
	ZoneRef corev1.LocalObjectReference `json:"zoneRef"`

	// Expression selects the requests the rule applies to, in Cloudflare's
	// Rules language, for example `http.request.uri.path eq "/admin"`.
	// Cloudflare validates it; the fields and operators it may use depend on
	// the zone's plan.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	Expression string `json:"expression"`

	// Enabled turns the rule on or off without deleting it.
	// +optional
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`

	// Description is shown with the rule in the Cloudflare dashboard.
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	Description string `json:"description,omitempty"`

	// Priority orders the rules kflare manages in a zone and phase: lower runs
	// first, and rules with the same priority run in order of namespace and
	// name. Rules created outside kflare keep their place.
	// +optional
	Priority int32 `json:"priority,omitempty"`
}

// RulesetRuleCloudflareMetadata identifies the rule a resource manages.
type RulesetRuleCloudflareMetadata struct {
	// ZoneID is the Cloudflare zone the rule is in.
	ZoneID string `json:"zoneID,omitempty"`

	// RulesetID is the zone's entry point ruleset for the rule's phase.
	RulesetID string `json:"rulesetID,omitempty"`

	// RuleID is the rule's Cloudflare ID.
	RuleID string `json:"ruleID,omitempty"`

	// Ref is the rule's ref, which kflare derives from the resource's kind,
	// namespace and name. kflare finds the rule it manages by its ref.
	Ref string `json:"ref,omitempty"`

	// Version is the rule's version, which Cloudflare increments whenever the
	// rule changes.
	Version string `json:"version,omitempty"`
}

// RulesetRuleStatus is the observed state of a rule.
type RulesetRuleStatus struct {
	// Conditions represent the latest available observations of the rule's state.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// CloudflareMetadata identifies the rule in Cloudflare.
	CloudflareMetadata RulesetRuleCloudflareMetadata `json:"cloudflareMetadata,omitempty"`
}

// GetRulesetRuleSpec returns the spec fields the WAF custom rule shares with
// every rule kind.
func (in *WAFCustomRule) GetRulesetRuleSpec() *RulesetRuleSpec { return &in.Spec.RulesetRuleSpec }

// GetRulesetRuleStatus returns the WAF custom rule's status.
func (in *WAFCustomRule) GetRulesetRuleStatus() *RulesetRuleStatus { return &in.Status }
