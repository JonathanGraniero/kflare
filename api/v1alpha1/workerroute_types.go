/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkerRouteIDLabel holds the ID of the Cloudflare Worker route a
// WorkerRoute manages. kflare sets it, and never adopts a route that another
// WorkerRoute already carries in this label.
const WorkerRouteIDLabel = "kflare.dev/route-id"

// WorkerRouteSpec defines the desired state of WorkerRoute.
type WorkerRouteSpec struct {
	// ZoneRef references the Zone, in the same namespace, the route belongs
	// to. Immutable after creation.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="zoneRef is immutable"
	ZoneRef corev1.LocalObjectReference `json:"zoneRef"`

	// Pattern selects the requests the route applies to, such as
	// "example.com/api/*" or "*.example.com/*". It has no scheme or port,
	// may start with "*" (any subdomain) and end with "*" (any path), and its
	// hostname must be in the zone. Patterns are unique within a zone.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^\*?[A-Za-z0-9.-]+(/\S*)?$`
	Pattern string `json:"pattern"`

	// WorkerScriptRef references the WorkerScript, in the same namespace,
	// that handles matching requests. Omit it to exclude the pattern from a
	// broader route, so that no Worker runs for it.
	// +optional
	WorkerScriptRef *corev1.LocalObjectReference `json:"workerScriptRef,omitempty"`
}

// WorkerRouteCloudflareMetadata holds identifiers and metadata returned by the Cloudflare API.
type WorkerRouteCloudflareMetadata struct {
	// RouteID is the Cloudflare route identifier.
	RouteID string `json:"routeID,omitempty"`

	// ZoneID is the Cloudflare zone the route belongs to (denormalized from
	// the Zone status).
	ZoneID string `json:"zoneID,omitempty"`

	// Script is the Worker script the route sent requests to at the last
	// sync. It is empty for a route that excludes its pattern.
	Script string `json:"script,omitempty"`
}

// WorkerRouteStatus defines the observed state of WorkerRoute.
type WorkerRouteStatus struct {
	// Conditions represent the latest available observations of the route state.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// CloudflareMetadata holds identifiers and metadata returned by the Cloudflare API.
	CloudflareMetadata WorkerRouteCloudflareMetadata `json:"cloudflareMetadata,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cfroute
// +kubebuilder:printcolumn:name="Pattern",type=string,JSONPath=`.spec.pattern`
// +kubebuilder:printcolumn:name="Worker",type=string,JSONPath=`.spec.workerScriptRef.name`
// +kubebuilder:printcolumn:name="Route ID",type=string,JSONPath=`.status.cloudflareMetadata.routeID`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// WorkerRoute is the Schema for the workerroutes API.
// It routes requests matching a URL pattern in a zone to a WorkerScript.
type WorkerRoute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkerRouteSpec   `json:"spec,omitempty"`
	Status WorkerRouteStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkerRouteList contains a list of WorkerRoute.
type WorkerRouteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkerRoute `json:"items"`
}

func init() {
	register(&WorkerRoute{}, &WorkerRouteList{})
}
