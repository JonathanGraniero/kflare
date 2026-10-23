/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The types here are shared by every settings kind: a kind whose resources
// each configure one product's settings in a zone, such as its managed
// transforms. kflare-gen generates the settings kinds; the shared settings
// controller runs them.

// SettingsSpec holds the spec fields every settings kind shares. Each kind
// embeds it inline, next to its settings.
type SettingsSpec struct {
	// ZoneRef references the Zone, in the same namespace, whose settings the
	// resource configures. Immutable after creation.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="zoneRef is immutable"
	ZoneRef corev1.LocalObjectReference `json:"zoneRef"`
}

// SettingsCloudflareMetadata identifies the settings a resource configures.
type SettingsCloudflareMetadata struct {
	// ZoneID is the Cloudflare zone whose settings the resource configures.
	ZoneID string `json:"zoneID,omitempty"`
}

// SettingsStatus is the observed state of a resource that configures a
// zone's settings.
type SettingsStatus struct {
	// Conditions represent the latest available observations of the settings' state.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// CloudflareMetadata identifies the settings in Cloudflare.
	CloudflareMetadata SettingsCloudflareMetadata `json:"cloudflareMetadata,omitempty"`
}
