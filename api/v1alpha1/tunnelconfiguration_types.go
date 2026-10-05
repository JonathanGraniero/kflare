/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TunnelConfigurationSpec defines the desired state of TunnelConfiguration.
type TunnelConfigurationSpec struct {
	// TunnelRef references the Tunnel, in the same namespace, whose ingress
	// rules this resource manages. Immutable after creation.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="tunnelRef is immutable"
	TunnelRef corev1.LocalObjectReference `json:"tunnelRef"`

	// Ingress maps public hostnames to the services cloudflared forwards them
	// to. Rules are matched in order; the first match wins.
	// +kubebuilder:validation:MinItems=1
	Ingress []TunnelIngressRule `json:"ingress"`

	// DefaultService handles requests that match no ingress rule. kflare
	// appends it as the final catch-all rule that Cloudflare requires.
	// +kubebuilder:default="http_status:404"
	// +kubebuilder:validation:MinLength=1
	// +optional
	DefaultService string `json:"defaultService,omitempty"`
}

// TunnelIngressRule routes requests for a hostname (and optionally a path) to
// a service reachable from cloudflared.
type TunnelIngressRule struct {
	// Hostname to match, such as "app.example.com" or "*.example.com".
	// Requests only reach the tunnel once the hostname has a proxied CNAME
	// record pointing at <tunnelID>.cfargotunnel.com.
	// +kubebuilder:validation:MinLength=1
	Hostname string `json:"hostname"`

	// Path is a regular expression matched against the request path.
	// +optional
	Path string `json:"path,omitempty"`

	// Service is where cloudflared sends matching requests, for example
	// "http://my-service.my-namespace:80", "tcp://db:5432" or "http_status:404".
	// +kubebuilder:validation:MinLength=1
	Service string `json:"service"`

	// OriginRequest tunes how cloudflared connects to the service.
	// +optional
	OriginRequest *TunnelOriginRequest `json:"originRequest,omitempty"`
}

// TunnelOriginRequest holds per-rule settings for the connection from
// cloudflared to the origin service.
type TunnelOriginRequest struct {
	// HTTPHostHeader overrides the Host header sent to the origin.
	// +optional
	HTTPHostHeader *string `json:"httpHostHeader,omitempty"`

	// OriginServerName is the hostname expected on the origin's TLS certificate.
	// +optional
	OriginServerName *string `json:"originServerName,omitempty"`

	// NoTLSVerify disables verification of the origin's TLS certificate.
	// +optional
	NoTLSVerify *bool `json:"noTLSVerify,omitempty"`

	// HTTP2Origin makes cloudflared connect to the origin over HTTP/2.
	// +optional
	HTTP2Origin *bool `json:"http2Origin,omitempty"`

	// DisableChunkedEncoding turns off chunked transfer encoding, which some
	// WSGI servers need.
	// +optional
	DisableChunkedEncoding *bool `json:"disableChunkedEncoding,omitempty"`
}

// TunnelConfigurationCloudflareMetadata holds identifiers and metadata
// returned by the Cloudflare API.
type TunnelConfigurationCloudflareMetadata struct {
	// TunnelID is the Cloudflare tunnel this configuration was last written to.
	TunnelID string `json:"tunnelID,omitempty"`

	// Version is the configuration version Cloudflare reported after the last
	// sync. cloudflared picks up each new version automatically.
	Version int `json:"version,omitempty"`
}

// TunnelConfigurationStatus defines the observed state of TunnelConfiguration.
type TunnelConfigurationStatus struct {
	// Conditions represent the latest available observations of the configuration state.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// CloudflareMetadata holds identifiers and metadata returned by the Cloudflare API.
	CloudflareMetadata TunnelConfigurationCloudflareMetadata `json:"cloudflareMetadata,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cftunnelconfig
// +kubebuilder:printcolumn:name="Tunnel",type=string,JSONPath=`.spec.tunnelRef.name`
// +kubebuilder:printcolumn:name="Version",type=integer,JSONPath=`.status.cloudflareMetadata.version`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TunnelConfiguration is the Schema for the tunnelconfigurations API.
// It owns the complete ingress configuration of one remotely-managed Tunnel.
type TunnelConfiguration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TunnelConfigurationSpec   `json:"spec,omitempty"`
	Status TunnelConfigurationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TunnelConfigurationList contains a list of TunnelConfiguration.
type TunnelConfigurationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TunnelConfiguration `json:"items"`
}

func init() {
	register(&TunnelConfiguration{}, &TunnelConfigurationList{})
}
