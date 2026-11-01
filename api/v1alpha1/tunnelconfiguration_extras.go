/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
)

// TunnelConfigurationSpecExtras holds what a TunnelConfiguration's spec asks
// for: a tunnel's configuration is not an object of its own, so its fields and
// calls stay hand-written.
type TunnelConfigurationSpecExtras struct {
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

// TunnelConfigurationMetadataExtras holds what Cloudflare reports about a
// configuration.
type TunnelConfigurationMetadataExtras struct {
	// Version is the configuration version Cloudflare reported after the last
	// sync. cloudflared picks up each new version automatically.
	Version int `json:"version,omitempty"`
}
