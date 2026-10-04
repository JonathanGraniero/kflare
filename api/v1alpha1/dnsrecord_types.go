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

// DNSRecordIDLabel holds the ID of the Cloudflare DNS record a DNSRecord
// manages. kflare sets it, and never adopts a record that another DNSRecord
// already carries in this label, so each Cloudflare record has at most one
// owner. Find the owner of a record with
// `kubectl get dnsrecords -A -l cloudflare.k8s.io/record-id=<id>`.
const DNSRecordIDLabel = "cloudflare.k8s.io/record-id"

// DNSRecordSpec defines the desired state of DNSRecord.
type DNSRecordSpec struct {
	// ZoneRef references the Zone CR (in the same namespace) that owns this record.
	// Immutable after creation.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="zoneRef is immutable"
	ZoneRef corev1.LocalObjectReference `json:"zoneRef"`

	// Name is the fully-qualified DNS record name (e.g. "www.example.com"). Immutable after creation.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	Name string `json:"name"`

	// Type is the DNS record type. Immutable after creation.
	// +kubebuilder:validation:Enum=A;AAAA;CAA;CERT;CNAME;DNSKEY;DS;HTTPS;LOC;MX;NAPTR;NS;OPENPGPKEY;PTR;SMIMEA;SRV;SSHFP;SVCB;TLSA;TXT;URI
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="type is immutable"
	Type string `json:"type"`

	// Content is the DNS record content (e.g. an IP address for A records, a hostname for CNAME/MX).
	// Omit for record types that use the Data field instead (SRV, LOC, CAA, ...).
	// +optional
	Content string `json:"content,omitempty"`

	// TTL is the time-to-live in seconds: 1 for automatic (the default), or
	// 30-86400. Cloudflare only accepts values below 60 on Enterprise zones.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:validation:XValidation:rule="self == 1 || self >= 30",message="ttl must be 1 (automatic) or between 30 and 86400"
	// +optional
	TTL int `json:"ttl,omitempty"`

	// Proxied controls whether Cloudflare proxies traffic through its network for this record.
	// Only applicable to A, AAAA, and CNAME records. Defaults to false (DNS only).
	// +optional
	Proxied *bool `json:"proxied,omitempty"`

	// Priority is the record priority, used for MX, SRV and URI records.
	// When unset, kflare leaves the priority Cloudflare chose unchanged.
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
	// Cloudflare derives the record content from it, so content drift is not
	// corrected while data is set.
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
