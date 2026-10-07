/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KVNamespaceIDLabel holds the ID of the Cloudflare Workers KV namespace a
// KVNamespace manages. kflare sets it, and never adopts a namespace that
// another KVNamespace already carries in this label.
const KVNamespaceIDLabel = "kflare.dev/kv-namespace-id"

// KVNamespaceSpec defines the desired state of KVNamespace.
type KVNamespaceSpec struct {
	// AccountRef references the CloudflareAccount that owns the namespace.
	// Immutable after creation.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="accountRef is immutable"
	AccountRef corev1.LocalObjectReference `json:"accountRef"`

	// Title is the namespace's title in Cloudflare, unique within the
	// account. Changing it renames the namespace in place; its ID and data
	// are kept.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Title string `json:"title"`
}

// KVNamespaceCloudflareMetadata holds identifiers returned by the Cloudflare API.
type KVNamespaceCloudflareMetadata struct {
	// NamespaceID is the Cloudflare KV namespace identifier. WorkerScripts
	// bind the namespace by this ID.
	NamespaceID string `json:"namespaceID,omitempty"`
}

// KVNamespaceStatus defines the observed state of KVNamespace.
type KVNamespaceStatus struct {
	// Conditions represent the latest available observations of the namespace state.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// CloudflareMetadata holds identifiers returned by the Cloudflare API.
	CloudflareMetadata KVNamespaceCloudflareMetadata `json:"cloudflareMetadata,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cfkv
// +kubebuilder:printcolumn:name="Title",type=string,JSONPath=`.spec.title`
// +kubebuilder:printcolumn:name="Namespace ID",type=string,JSONPath=`.status.cloudflareMetadata.namespaceID`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KVNamespace is the Schema for the kvnamespaces API.
// It represents a Cloudflare Workers KV namespace. Deleting it deletes the
// namespace and all of its data, unless the retain deletion policy is set.
type KVNamespace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KVNamespaceSpec   `json:"spec,omitempty"`
	Status KVNamespaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KVNamespaceList contains a list of KVNamespace.
type KVNamespaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KVNamespace `json:"items"`
}

func init() {
	register(&KVNamespace{}, &KVNamespaceList{})
}
