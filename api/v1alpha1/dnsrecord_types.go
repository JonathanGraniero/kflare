/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DNSRecordSpec defines the desired state of DNSRecord.
type DNSRecordSpec struct {
	// ZoneRef references the Zone CR (in the same namespace) that owns this record.
	ZoneRef corev1.LocalObjectReference `json:"zoneRef"`

	// Name is the fully-qualified DNS record name (e.g. "www.example.com"). Immutable after creation.
	Name string `json:"name"`

	// Type is the DNS record type.
	// +kubebuilder:validation:Enum=A;AAAA;CNAME;MX;TXT;SRV;CAA;NS;PTR;CERT;DNSKEY;DS;NAPTR;SMIMEA;SSHFP;TLSA;URI
	Type string `json:"type"`

	// Content is the DNS record content (e.g. an IP address for A records, a hostname for CNAME/MX).
	// Omit for record types that use the Data field instead (SRV, LOC).
	// +optional
	Content string `json:"content,omitempty"`

	// TTL is the time-to-live in seconds. Use 1 for automatic TTL.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	// +optional
	TTL int `json:"ttl,omitempty"`

	// Proxied controls whether Cloudflare proxies traffic through its network for this record.
	// Only applicable to A, AAAA, and CNAME records.
	// +optional
	Proxied *bool `json:"proxied,omitempty"`

	// Priority is the record priority, used for MX and SRV records.
	// +optional
	Priority *uint16 `json:"priority,omitempty"`

	// Comment is an optional free-text annotation for the record.
	// +optional
	Comment string `json:"comment,omitempty"`

	// Tags are optional labels attached to the record in Cloudflare.
	// +optional
	Tags []string `json:"tags,omitempty"`

	// Data holds structured record data for types that require it (SRV, LOC, CAA).
	// The JSON structure must match the Cloudflare API schema for the given record type.
	// +optional
	Data *apiextensionsv1.JSON `json:"data,omitempty"`
}

// DNSRecordCloudflareMetadata holds identifiers and read-only metadata returned by the Cloudflare API.
type DNSRecordCloudflareMetadata struct {
	// RecordID is the Cloudflare DNS record identifier.
	RecordID string `json:"recordID,omitempty"`

	// ZoneID is the Cloudflare zone identifier (denormalized from the parent Zone status).
	ZoneID string `json:"zoneID,omitempty"`

	// Proxiable indicates whether this record type supports Cloudflare proxying.
	Proxiable bool `json:"proxiable,omitempty"`
}

// DNSRecordStatus defines the observed state of DNSRecord.
type DNSRecordStatus struct {
	// Conditions represent the latest available observations of the DNS record state.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// CloudflareMetadata holds identifiers and metadata returned by the Cloudflare API.
	CloudflareMetadata DNSRecordCloudflareMetadata `json:"cloudflareMetadata,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cfrecord
// +kubebuilder:printcolumn:name="Record Name",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Content",type=string,JSONPath=`.spec.content`
// +kubebuilder:printcolumn:name="Record ID",type=string,JSONPath=`.status.cloudflareMetadata.recordID`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DNSRecord is the Schema for the dnsrecords API.
// It represents a DNS record within a Cloudflare-managed zone.
type DNSRecord struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DNSRecordSpec   `json:"spec,omitempty"`
	Status DNSRecordStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DNSRecordList contains a list of DNSRecord.
type DNSRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSRecord `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DNSRecord{}, &DNSRecordList{})
}
