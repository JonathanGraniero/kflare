/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

// ZoneReference names the Zone a resource belongs to: one in the resource's
// namespace, or, with namespace, one in another namespace, whose
// spec.allowedReferences must then allow the resource's namespace and
// hostname.
// +structType=atomic
type ZoneReference struct {
	// Name of the Zone.
	// +kubebuilder:default=""
	// +optional
	Name string `json:"name,omitempty"`

	// Namespace of the Zone, when it is not the resource's own. The Zone's
	// spec.allowedReferences must allow this resource's namespace and
	// hostname; otherwise the resource reports reason ZoneReferenceNotAllowed,
	// and kflare makes no Cloudflare call for it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ZoneAllowedReference lets the resources of some namespaces use a Zone from
// outside its namespace, for some of the zone's hostnames.
type ZoneAllowedReference struct {
	// Namespaces says which namespaces' resources this entry allows.
	// +kubebuilder:validation:Required
	Namespaces AllowedNamespaces `json:"namespaces"`

	// Hostnames are the hostnames those resources may use: a DNSRecord's
	// name, and the hostname of a WorkerRoute's pattern. Each is a hostname,
	// which allows itself alone, or starts with "*.", which allows every name
	// below the rest, at any depth, but not the rest itself:
	// "*.team-a.example.com" allows "api.team-a.example.com" and
	// "v2.api.team-a.example.com", not "team-a.example.com". A name with a
	// wildcard of its own, such as the record "*.team-a.example.com" or the
	// route "*.team-a.example.com/*", needs an entry that covers every name it
	// stands for. Names ignore case.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^(\*\.)?([a-zA-Z0-9_]([a-zA-Z0-9_-]{0,61}[a-zA-Z0-9_])?\.)*[a-zA-Z0-9_]([a-zA-Z0-9_-]{0,61}[a-zA-Z0-9_])?$`
	// +listType=set
	Hostnames []string `json:"hostnames"`
}
