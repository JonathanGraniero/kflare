/*
Copyright 2026 Jonathan Graniero.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CloudflareAccountSpec defines the desired state of CloudflareAccount
type CloudflareAccountSpec struct {
	// AccountID is the Cloudflare account ID.
	// +kubebuilder:validation:Required
	AccountID string `json:"accountID"`

	// TokenSecretRef references a Secret containing the CF_API_TOKEN key.
	// +kubebuilder:validation:Required
	TokenSecretRef SecretReference `json:"tokenSecretRef"`
}

// SecretReference points to a key within a Kubernetes Secret.
type SecretReference struct {
	// Name of the Secret.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace of the Secret.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`

	// Key within the Secret. Defaults to CF_API_TOKEN.
	// +kubebuilder:default=CF_API_TOKEN
	Key string `json:"key,omitempty"`
}

// CloudflareAccountStatus defines the observed state of CloudflareAccount
type CloudflareAccountStatus struct {
	// Conditions represent the latest available observations of the account state.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// AccountName is the name of the Cloudflare account as returned by the API.
	AccountName string `json:"accountName,omitempty"`
}

// Condition type constants
const (
	// ConditionReady indicates the account credentials are valid and the account is reachable.
	ConditionReady = "Ready"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster,shortName=cfaccount
//+kubebuilder:printcolumn:name="Account ID",type=string,JSONPath=`.spec.accountID`
//+kubebuilder:printcolumn:name="Account Name",type=string,JSONPath=`.status.accountName`
//+kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CloudflareAccount is the Schema for the cloudflareaccounts API.
// It is cluster-scoped and holds credentials for a single Cloudflare account.
type CloudflareAccount struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CloudflareAccountSpec   `json:"spec,omitempty"`
	Status CloudflareAccountStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// CloudflareAccountList contains a list of CloudflareAccount
type CloudflareAccountList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudflareAccount `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CloudflareAccount{}, &CloudflareAccountList{})
}
