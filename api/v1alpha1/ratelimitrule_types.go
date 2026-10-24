/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RateLimitRuleSpec defines the desired state of RateLimitRule: one rule in
// the zone's rate limiting rules, Cloudflare's http_ratelimit phase. The rule
// counts the requests its expression matches; once a client sends more than
// requestsPerPeriod of them in a period, the action applies to the client's
// matching requests for the mitigation timeout.
// +kubebuilder:validation:XValidation:rule="!has(self.mitigationTimeout) || self.mitigationTimeout == 0 || self.mitigationTimeout >= self.period",message="mitigationTimeout must be 0 or at least the period"
// +kubebuilder:validation:XValidation:rule="!has(self.blockResponse) || self.action == 'block'",message="blockResponse is allowed only with action block"
type RateLimitRuleSpec struct {
	RulesetRuleSpec `json:",inline"`

	// Action is what Cloudflare does with a client's requests once it is over
	// the rate: block them, challenge the visitor (challenge, js_challenge or
	// managed_challenge), or only log them (log, on Enterprise plans). Free
	// plans can only block.
	// +optional
	// +kubebuilder:default=block
	// +kubebuilder:validation:Enum=block;challenge;js_challenge;managed_challenge;log
	Action string `json:"action,omitempty"`

	// Characteristics are what requests are counted by: requests that share
	// all of them share a counter. The default, ip.src, counts per client IP
	// address, the only choice on Free plans; higher plans can count by other
	// fields, such as http.request.headers["x-api-key"]. Cloudflare also
	// counts per data center, cf.colo.id, which kflare adds.
	// +optional
	// +kubebuilder:default={"ip.src"}
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	Characteristics []RateLimitRuleCharacteristic `json:"characteristics,omitempty"`

	// Period is how long, in seconds, requests are counted for. Cloudflare
	// accepts set periods between 10 seconds and an hour, and the zone's plan
	// narrows them: Free plans can only count for 10 seconds.
	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	Period int32 `json:"period,omitempty"`

	// RequestsPerPeriod is how many matching requests a client can send in a
	// period before the action applies.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	RequestsPerPeriod int32 `json:"requestsPerPeriod"`

	// MitigationTimeout is how long, in seconds, the action applies once a
	// client is over the rate. It defaults to the period, and must be at least
	// the period, or 0 to throttle instead: apply the action only to the
	// requests over the rate (Enterprise plans). Free plans can only use 10.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MitigationTimeout *int32 `json:"mitigationTimeout,omitempty"`

	// CountingExpression counts other requests than those the rule matches,
	// for example only the failed logins among the login requests. It
	// defaults to the rule's expression. Free plans cannot set it.
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	CountingExpression string `json:"countingExpression,omitempty"`

	// RequestsToOrigin counts only the requests that reach the origin,
	// leaving out those served from Cloudflare's cache.
	// +optional
	RequestsToOrigin bool `json:"requestsToOrigin,omitempty"`

	// BlockResponse replaces Cloudflare's response to the requests a block
	// rule blocks.
	// +optional
	BlockResponse *RateLimitRuleBlockResponse `json:"blockResponse,omitempty"`
}

// RateLimitRuleCharacteristic is a field requests are counted by, such as
// ip.src.
// +kubebuilder:validation:MinLength=1
type RateLimitRuleCharacteristic string

// RateLimitRuleBlockResponse is the response a rate limiting rule sends to
// the requests it blocks, instead of Cloudflare's.
type RateLimitRuleBlockResponse struct {
	// StatusCode is the response's HTTP status code.
	// +optional
	// +kubebuilder:default=429
	// +kubebuilder:validation:Minimum=400
	// +kubebuilder:validation:Maximum=499
	StatusCode int32 `json:"statusCode,omitempty"`

	// ContentType is the response's content type.
	// +kubebuilder:validation:Enum=text/html;text/plain;application/json;text/xml
	ContentType string `json:"contentType"`

	// Content is the response body. Cloudflare allows at most 30,720 bytes.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=30720
	Content string `json:"content"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cfratelimit,categories=kflare
// +kubebuilder:printcolumn:name="Requests",type=integer,JSONPath=`.spec.requestsPerPeriod`
// +kubebuilder:printcolumn:name="Period",type=integer,JSONPath=`.spec.period`
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action`
// +kubebuilder:printcolumn:name="Priority",type=integer,JSONPath=`.spec.priority`
// +kubebuilder:printcolumn:name="Rule ID",type=string,JSONPath=`.status.cloudflareMetadata.ruleID`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RateLimitRule is the Schema for the ratelimitrules API.
// It manages one rate limiting rule in a zone.
type RateLimitRule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RateLimitRuleSpec `json:"spec,omitempty"`
	Status RulesetRuleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RateLimitRuleList contains a list of RateLimitRule.
type RateLimitRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RateLimitRule `json:"items"`
}

func init() {
	register(&RateLimitRule{}, &RateLimitRuleList{})
}
