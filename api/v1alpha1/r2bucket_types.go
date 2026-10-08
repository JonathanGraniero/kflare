/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// R2BucketNameLabel holds the name of the Cloudflare R2 bucket an R2Bucket
// manages. kflare sets it once the bucket is created or adopted, and never
// adopts a bucket that another R2Bucket in the same Cloudflare account
// already carries in this label.
const R2BucketNameLabel = "kflare.dev/r2-bucket-name"

// R2BucketSpec defines the desired state of R2Bucket.
// +kubebuilder:validation:XValidation:rule="has(self.locationHint) == has(oldSelf.locationHint)",message="locationHint is immutable"
type R2BucketSpec struct {
	// AccountRef references the CloudflareAccount that owns the bucket.
	// Immutable after creation.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="accountRef is immutable"
	AccountRef corev1.LocalObjectReference `json:"accountRef"`

	// Name is the bucket's name in Cloudflare, unique within the account:
	// 3 to 63 lowercase letters, digits and hyphens, starting and ending with
	// a letter or digit. Buckets cannot be renamed, so it is immutable.
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	Name string `json:"name"`

	// LocationHint asks Cloudflare to place a new bucket near a region:
	// wnam, enam, weur, eeur, apac or oc. It is only a hint, and only used
	// when kflare creates the bucket; an adopted bucket stays where it is.
	// status.cloudflareMetadata.location shows where the bucket is.
	// Immutable after creation.
	// +kubebuilder:validation:Enum=wnam;enam;weur;eeur;apac;oc
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="locationHint is immutable"
	// +optional
	LocationHint string `json:"locationHint,omitempty"`
}

// R2BucketCloudflareMetadata holds metadata returned by the Cloudflare API.
type R2BucketCloudflareMetadata struct {
	// Location is the region Cloudflare placed the bucket in, for example WEUR.
	Location string `json:"location,omitempty"`

	// CreationDate is when Cloudflare created the bucket. It changes if the
	// bucket is deleted outside kflare and created again.
	CreationDate *metav1.Time `json:"creationDate,omitempty"`
}

// R2BucketStatus defines the observed state of R2Bucket.
type R2BucketStatus struct {
	// Conditions represent the latest available observations of the bucket state.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// CloudflareMetadata holds metadata returned by the Cloudflare API.
	CloudflareMetadata R2BucketCloudflareMetadata `json:"cloudflareMetadata,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cfr2
// +kubebuilder:printcolumn:name="Bucket",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Location",type=string,JSONPath=`.status.cloudflareMetadata.location`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// R2Bucket is the Schema for the r2buckets API.
// It represents a Cloudflare R2 bucket in the account's default
// jurisdiction. Deleting it deletes the bucket, unless the retain deletion
// policy is set; kflare never deletes objects, so a bucket that still holds
// any is kept and reported as BucketNotEmpty.
type R2Bucket struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   R2BucketSpec   `json:"spec,omitempty"`
	Status R2BucketStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// R2BucketList contains a list of R2Bucket.
type R2BucketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []R2Bucket `json:"items"`
}

func init() {
	register(&R2Bucket{}, &R2BucketList{})
}
