/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ZoneSpec defines the desired state of Zone.
type ZoneSpec struct {
	// Name is the domain (e.g. "example.com"). Immutable after creation.
	// +kubebuilder:validation:Pattern=`^([a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	Name string `json:"name"`

	// AccountRef references the CloudflareAccount that owns this zone.
	// Immutable after creation.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="accountRef is immutable"
	AccountRef corev1.LocalObjectReference `json:"accountRef"`

	// Plan is the billing plan for the zone: free, pro, business, or enterprise.
	// Not applied yet: kflare does not change a zone's plan, which goes
	// through Cloudflare billing.
	// +kubebuilder:validation:Enum=free;pro;business;enterprise
	// +optional
	Plan string `json:"plan,omitempty"`

	// Type is the zone type: "full" (Cloudflare manages DNS) or "partial" (proxy only).
	// +kubebuilder:validation:Enum=full;partial
	// +kubebuilder:default=full
	// +optional
	Type string `json:"type,omitempty"`
}

// ZoneCloudflareMetadata holds identifiers and metadata returned by the Cloudflare API.
type ZoneCloudflareMetadata struct {
	// ZoneID is the Cloudflare zone identifier.
	ZoneID string `json:"zoneID,omitempty"`

	// NameServers are the authoritative name servers assigned by Cloudflare.
	NameServers []string `json:"nameServers,omitempty"`

	// Status is the zone activation status as reported by Cloudflare
	// (active, pending, initializing, moved, deactivated).
	Status string `json:"status,omitempty"`
}

// ZoneStatus defines the observed state of Zone.
type ZoneStatus struct {
	// Conditions represent the latest available observations of the zone state.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// CloudflareMetadata holds identifiers and metadata returned by the Cloudflare API.
	CloudflareMetadata ZoneCloudflareMetadata `json:"cloudflareMetadata,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cfzone
// +kubebuilder:printcolumn:name="Domain",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Zone ID",type=string,JSONPath=`.status.cloudflareMetadata.zoneID`
// +kubebuilder:printcolumn:name="CF Status",type=string,JSONPath=`.status.cloudflareMetadata.status`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Zone is the Schema for the zones API.
// It represents a DNS zone managed in Cloudflare.
type Zone struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZoneSpec   `json:"spec,omitempty"`
	Status ZoneStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ZoneList contains a list of Zone.
type ZoneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Zone `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Zone{}, &ZoneList{})
}
