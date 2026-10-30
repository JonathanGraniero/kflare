/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

// ZoneSpecExtras holds what a Zone's spec asks for: its fields and calls stay
// hand-written until the test account can verify a zone's creation.
type ZoneSpecExtras struct {
	// Name is the domain (e.g. "example.com"). Immutable after creation.
	// +kubebuilder:validation:Pattern=`^([a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	Name string `json:"name"`

	// Plan is the zone's billing plan: free, pro, business, or enterprise.
	// When unset, kflare leaves the plan alone. When set, kflare subscribes
	// the zone to it, which bills the account's default payment method.
	// Enterprise cannot be bought through the API; set it only for a zone
	// that is already on Enterprise. Downgrades take effect at the end of
	// the billing period and show up in status.cloudflareMetadata.pendingPlan.
	// +kubebuilder:validation:Enum=free;pro;business;enterprise
	// +optional
	Plan string `json:"plan,omitempty"`

	// Type is the zone type: "full" (Cloudflare manages DNS) or "partial" (proxy only).
	// +kubebuilder:validation:Enum=full;partial
	// +kubebuilder:default=full
	// +optional
	Type string `json:"type,omitempty"`
}

// ZoneMetadataExtras holds what Cloudflare reports about a zone.
type ZoneMetadataExtras struct {
	// NameServers are the authoritative name servers assigned by Cloudflare.
	NameServers []string `json:"nameServers,omitempty"`

	// Plan is the zone's current billing plan (free, pro, business or enterprise).
	Plan string `json:"plan,omitempty"`

	// PendingPlan is a plan change Cloudflare has scheduled for the end of
	// the billing period, such as a downgrade.
	PendingPlan string `json:"pendingPlan,omitempty"`

	// Status is the zone activation status as reported by Cloudflare
	// (active, pending, initializing, moved, deactivated).
	Status string `json:"status,omitempty"`
}
