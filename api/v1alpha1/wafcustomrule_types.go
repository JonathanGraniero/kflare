/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WAFCustomRuleSpec defines the desired state of WAFCustomRule: one rule in
// the zone's WAF custom rules, Cloudflare's http_request_firewall_custom
// phase.
// +kubebuilder:validation:XValidation:rule="(self.action == 'skip') == has(self.skip)",message="skip is required with action skip and allowed only with it"
// +kubebuilder:validation:XValidation:rule="!has(self.blockResponse) || self.action == 'block'",message="blockResponse is allowed only with action block"
type WAFCustomRuleSpec struct {
	RulesetRuleSpec `json:",inline"`

	// Action is what Cloudflare does with a matching request: block it,
	// challenge the visitor (challenge, js_challenge or managed_challenge),
	// let it skip other security features (skip), or only log it (log, on
	// Enterprise plans).
	// +kubebuilder:validation:Enum=block;challenge;js_challenge;managed_challenge;skip;log
	Action string `json:"action"`

	// Skip says what a skip rule skips. Required with action skip.
	// +optional
	Skip *WAFCustomRuleSkip `json:"skip,omitempty"`

	// BlockResponse replaces Cloudflare's block page for a block rule. It
	// needs a Pro plan or higher.
	// +optional
	BlockResponse *WAFCustomRuleBlockResponse `json:"blockResponse,omitempty"`
}

// WAFCustomRuleSkip says what a skip rule skips for the requests it matches.
// +kubebuilder:validation:XValidation:rule="(has(self.remainingRules) && self.remainingRules) || (has(self.phases) && size(self.phases) > 0) || (has(self.products) && size(self.products) > 0)",message="skip needs remainingRules, phases or products"
type WAFCustomRuleSkip struct {
	// RemainingRules skips the zone's custom rules that run after this one.
	// +optional
	RemainingRules bool `json:"remainingRules,omitempty"`

	// Phases skips later security phases: rate limiting rules
	// (http_ratelimit), managed rules (http_request_firewall_managed) and
	// Super Bot Fight Mode (http_request_sbfm).
	// +optional
	// +listType=set
	Phases []WAFCustomRuleSkipPhase `json:"phases,omitempty"`

	// Products skips security features of the previous generation: zone
	// lockdown (zoneLockdown), user agent blocking (uaBlock), browser
	// integrity check (bic), hotlink protection (hot), security level
	// (securityLevel), the previous rate limiting (rateLimit) and the
	// previous managed rules (waf).
	// +optional
	// +listType=set
	Products []WAFCustomRuleSkipProduct `json:"products,omitempty"`

	// Log records the requests the rule skips for in Security Events. As in
	// Cloudflare, it defaults to true.
	// +optional
	// +kubebuilder:default=true
	Log *bool `json:"log,omitempty"`
}

// WAFCustomRuleSkipPhase is a phase a skip rule can skip.
// +kubebuilder:validation:Enum=http_ratelimit;http_request_firewall_managed;http_request_sbfm
type WAFCustomRuleSkipPhase string

// WAFCustomRuleSkipProduct is a previous-generation security feature a skip
// rule can skip.
// +kubebuilder:validation:Enum=zoneLockdown;uaBlock;bic;hot;securityLevel;rateLimit;waf
type WAFCustomRuleSkipProduct string

// WAFCustomRuleBlockResponse is the response a block rule sends instead of
// Cloudflare's block page.
type WAFCustomRuleBlockResponse struct {
	// StatusCode is the response's HTTP status code.
	// +optional
	// +kubebuilder:default=403
	// +kubebuilder:validation:Minimum=400
	// +kubebuilder:validation:Maximum=499
	StatusCode int32 `json:"statusCode,omitempty"`

	// ContentType is the response's content type.
	// +kubebuilder:validation:Enum=text/html;text/plain;application/json;text/xml
	ContentType string `json:"contentType"`

	// Content is the response body. Cloudflare allows at most 2 KB.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Content string `json:"content"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cfwaf,categories=kflare
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action`
// +kubebuilder:printcolumn:name="Priority",type=integer,JSONPath=`.spec.priority`
// +kubebuilder:printcolumn:name="Rule ID",type=string,JSONPath=`.status.cloudflareMetadata.ruleID`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// WAFCustomRule is the Schema for the wafcustomrules API.
// It manages one WAF custom rule in a zone.
type WAFCustomRule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WAFCustomRuleSpec `json:"spec,omitempty"`
	Status RulesetRuleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WAFCustomRuleList contains a list of WAFCustomRule.
type WAFCustomRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WAFCustomRule `json:"items"`
}

func init() {
	register(&WAFCustomRule{}, &WAFCustomRuleList{})
}
