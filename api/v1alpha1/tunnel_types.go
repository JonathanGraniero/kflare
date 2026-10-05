/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TunnelTokenKey is the key under which the tunnel token is stored in the
// credentials Secret. cloudflared reads the same name from the environment,
// so the Secret can be mounted with envFrom and run with `cloudflared tunnel run`.
const TunnelTokenKey = "TUNNEL_TOKEN"

// TunnelSpec defines the desired state of Tunnel.
type TunnelSpec struct {
	// Name is the tunnel name in Cloudflare. Immutable after creation.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	Name string `json:"name"`

	// AccountRef references the CloudflareAccount that owns this tunnel.
	// Immutable after creation.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="accountRef is immutable"
	AccountRef corev1.LocalObjectReference `json:"accountRef"`

	// CredentialsSecretRef names the Secret, in the Tunnel's namespace, that
	// receives the tunnel token under the TUNNEL_TOKEN key. kflare creates the
	// Secret and owns it, so it is deleted together with the Tunnel. An existing
	// Secret that kflare does not own is never overwritten.
	CredentialsSecretRef TunnelCredentialsSecretReference `json:"credentialsSecretRef"`
}

// TunnelCredentialsSecretReference names the Secret that receives the tunnel token.
type TunnelCredentialsSecretReference struct {
	// Name of the Secret, in the same namespace as the Tunnel.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// TunnelCloudflareMetadata holds identifiers and metadata returned by the Cloudflare API.
type TunnelCloudflareMetadata struct {
	// TunnelID is the Cloudflare tunnel identifier (a UUID).
	TunnelID string `json:"tunnelID,omitempty"`

	// Status is the tunnel health reported by Cloudflare at the last reconcile
	// (inactive, degraded, healthy, or down).
	Status string `json:"status,omitempty"`
}

// TunnelStatus defines the observed state of Tunnel.
type TunnelStatus struct {
	// Conditions represent the latest available observations of the tunnel state.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// CloudflareMetadata holds identifiers and metadata returned by the Cloudflare API.
	CloudflareMetadata TunnelCloudflareMetadata `json:"cloudflareMetadata,omitempty"`

	// CredentialsSecretName is the Secret the token was last written to. When
	// spec.credentialsSecretRef changes, kflare deletes this Secret after
	// writing the new one.
	CredentialsSecretName string `json:"credentialsSecretName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cftunnel
// +kubebuilder:printcolumn:name="Tunnel",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Tunnel ID",type=string,JSONPath=`.status.cloudflareMetadata.tunnelID`
// +kubebuilder:printcolumn:name="CF Status",type=string,JSONPath=`.status.cloudflareMetadata.status`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Tunnel is the Schema for the tunnels API.
// It represents a remotely-managed Cloudflare Tunnel and the Secret holding
// the token a cloudflared connector needs to run it.
type Tunnel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TunnelSpec   `json:"spec,omitempty"`
	Status TunnelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TunnelList contains a list of Tunnel.
type TunnelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tunnel `json:"items"`
}

func init() {
	register(&Tunnel{}, &TunnelList{})
}
