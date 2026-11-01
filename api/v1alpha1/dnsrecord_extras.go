/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// TunnelHostnameSuffix follows a tunnel's ID in the hostname that routes to
// it: a DNSRecord with a tunnelRef is a CNAME to <tunnel ID> + this suffix.
const TunnelHostnameSuffix = ".cfargotunnel.com"

// DNSRecordSpecExtras holds what a DNSRecord's spec asks for that cloudflare-go
// v7's union of record types does not type: the kind's hooks send them.
// +kubebuilder:validation:XValidation:rule="(has(self.content) ? 1 : 0) + (has(self.data) ? 1 : 0) + (has(self.tunnelRef) ? 1 : 0) == 1",message="set exactly one of content, data and tunnelRef"
// +kubebuilder:validation:XValidation:rule="!has(self.tunnelRef) || self.type == 'CNAME'",message="tunnelRef needs type CNAME"
// +kubebuilder:validation:XValidation:rule="!has(self.tunnelRef) || !has(self.proxied) || self.proxied",message="a record with a tunnelRef must be proxied: an unproxied one resolves to an address the internet cannot reach"
// +kubebuilder:validation:XValidation:rule="!(has(self.tunnelRef) || (has(self.proxied) && self.proxied)) || !has(self.ttl) || self.ttl == 1",message="a proxied record takes ttl 1 (automatic): Cloudflare ignores any other"
type DNSRecordSpecExtras struct {
	// TunnelRef points the record at a Tunnel in the same namespace: kflare
	// sets its content to the tunnel's hostname, <tunnel ID>.cfargotunnel.com,
	// and corrects it when the tunnel is recreated with a new ID. Only for
	// CNAME records, which are then proxied: a tunnel carries traffic only
	// through Cloudflare's proxy, and only for records in the tunnel's
	// account, so the Tunnel must use the Zone's CloudflareAccount. The Tunnel
	// waits for the records that point at it before it is deleted.
	// +optional
	TunnelRef *corev1.LocalObjectReference `json:"tunnelRef,omitempty"`

	// TTL is the time-to-live in seconds: 1 for automatic (the default), or
	// 30-86400. Cloudflare only accepts values below 60 on Enterprise zones,
	// and a proxied record always has an automatic TTL.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:validation:XValidation:rule="self == 1 || self >= 30",message="ttl must be 1 (automatic) or between 30 and 86400"
	// +optional
	TTL int `json:"ttl,omitempty"`

	// Proxied controls whether Cloudflare proxies traffic through its network for this record.
	// Only applicable to A, AAAA, and CNAME records. Defaults to false (DNS only),
	// or to true with a tunnelRef, which cannot be unproxied.
	// +optional
	Proxied *bool `json:"proxied,omitempty"`

	// Priority is the record priority, used for MX, SRV and URI records.
	// When unset, kflare leaves the priority Cloudflare chose unchanged.
	// +optional
	Priority *uint16 `json:"priority,omitempty"`

	// Tags are optional labels attached to the record in Cloudflare.
	// +optional
	Tags []string `json:"tags,omitempty"`

	// Data holds structured record data for types that require it (SRV, LOC, CAA).
	// The JSON structure must match the Cloudflare API schema for the given record type.
	// Set exactly one of content, data and tunnelRef.
	// Cloudflare derives the record content from it, so content drift is not
	// corrected while data is set.
	// +kubebuilder:validation:Type=object
	// +optional
	Data *apiextensionsv1.JSON `json:"data,omitempty"`
}
