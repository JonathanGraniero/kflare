/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ManagedRulesetSpec defines the desired state of ManagedRuleset: the
// deployment of one Cloudflare managed ruleset to the zone, as an execute rule
// in its http_request_firewall_managed phase. The rule's expression selects
// the requests the managed ruleset inspects; "true" selects every request.
type ManagedRulesetSpec struct {
	RulesetRuleSpec `json:",inline"`

	// RulesetID is the managed ruleset to deploy. Free plans can deploy the
	// Cloudflare Free Managed Ruleset, 77454fe2d30c4220b5701f6fdfb893ba; higher
	// plans also the Cloudflare Managed Ruleset,
	// efb7b8c949ac4650a09736fc376e9aee, and the OWASP Core Ruleset,
	// 4814384a9e5d4991b9815dcfc25d2f1f. Cloudflare always runs the ruleset's
	// latest version. A zone can deploy a ruleset only once. Immutable after
	// creation.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{32}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="rulesetID is immutable"
	RulesetID string `json:"rulesetID"`

	// Overrides change what the ruleset's rules do. An override for a rule
	// takes precedence over one for a category the rule is tagged with, which
	// takes precedence over the overrides for the whole ruleset.
	// +optional
	Overrides *ManagedRulesetOverrides `json:"overrides,omitempty"`
}

// ManagedRulesetOverrides change the behavior of a deployed managed ruleset.
// +kubebuilder:validation:XValidation:rule="has(self.enabled) || has(self.action) || (has(self.categories) && size(self.categories) > 0) || (has(self.rules) && size(self.rules) > 0)",message="overrides must override something"
type ManagedRulesetOverrides struct {
	// Enabled turns every rule of the ruleset on or off.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Action replaces the action of every rule of the ruleset.
	// +optional
	Action ManagedRulesetAction `json:"action,omitempty"`

	// Categories override the rules tagged with a category, such as
	// wordpress or log4j. Cloudflare refuses a category none of the ruleset's
	// rules has.
	// +optional
	// +listType=map
	// +listMapKey=category
	Categories []ManagedRulesetCategoryOverride `json:"categories,omitempty"`

	// Rules override single rules of the ruleset, by ID.
	// +optional
	// +listType=map
	// +listMapKey=id
	Rules []ManagedRulesetRuleOverride `json:"rules,omitempty"`
}

// ManagedRulesetCategoryOverride overrides the rules tagged with a category.
// +kubebuilder:validation:XValidation:rule="has(self.enabled) || has(self.action)",message="a category override needs enabled or action"
type ManagedRulesetCategoryOverride struct {
	// Category is the tag the overridden rules have.
	// +kubebuilder:validation:MinLength=1
	Category string `json:"category"`

	// Enabled turns the category's rules on or off.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Action replaces the action of the category's rules.
	// +optional
	Action ManagedRulesetAction `json:"action,omitempty"`
}

// ManagedRulesetRuleOverride overrides one rule of a managed ruleset.
// +kubebuilder:validation:XValidation:rule="has(self.enabled) || has(self.action)",message="a rule override needs enabled or action"
type ManagedRulesetRuleOverride struct {
	// ID is the rule's ID in the managed ruleset.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{32}$`
	ID string `json:"id"`

	// Enabled turns the rule on or off.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Action replaces the rule's action.
	// +optional
	Action ManagedRulesetAction `json:"action,omitempty"`
}

// ManagedRulesetAction is what an overridden managed rule does with a
// matching request: block it, challenge the visitor (challenge, js_challenge
// or managed_challenge), or only log it.
// +kubebuilder:validation:Enum=block;challenge;js_challenge;managed_challenge;log
type ManagedRulesetAction string

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cfmanaged,categories=kflare
// +kubebuilder:printcolumn:name="Ruleset",type=string,JSONPath=`.spec.rulesetID`
// +kubebuilder:printcolumn:name="Priority",type=integer,JSONPath=`.spec.priority`
// +kubebuilder:printcolumn:name="Rule ID",type=string,JSONPath=`.status.cloudflareMetadata.ruleID`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ManagedRuleset is the Schema for the managedrulesets API.
// It deploys a Cloudflare managed ruleset to a zone. Cloudflare deploys its
// Free Managed Ruleset to Free zones by default, but only while the zone has
// no rules of its own in the http_request_firewall_managed phase: as soon as
// any rule is there, only what the rules deploy runs. To turn a managed
// ruleset off, keep its ManagedRuleset and set enabled to false.
type ManagedRuleset struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ManagedRulesetSpec `json:"spec,omitempty"`
	Status RulesetRuleStatus  `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ManagedRulesetList contains a list of ManagedRuleset.
type ManagedRulesetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ManagedRuleset `json:"items"`
}

func init() {
	register(&ManagedRuleset{}, &ManagedRulesetList{})
}
