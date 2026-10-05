/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Worker script formats.
const (
	// WorkerFormatModule is an ES module exporting handlers (export default { fetch() {} }).
	WorkerFormatModule = "module"
	// WorkerFormatServiceWorker is the older addEventListener("fetch", ...) syntax.
	WorkerFormatServiceWorker = "serviceWorker"
)

// WorkerScriptSpec defines the desired state of WorkerScript.
// +kubebuilder:validation:XValidation:rule="has(self.script) != has(self.scriptConfigMapRef)",message="exactly one of script or scriptConfigMapRef must be set"
type WorkerScriptSpec struct {
	// Name is the Worker's script name in Cloudflare. Immutable after creation.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9_-]{0,61}[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	Name string `json:"name"`

	// AccountRef references the CloudflareAccount that owns this Worker.
	// Immutable after creation.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="accountRef is immutable"
	AccountRef corev1.LocalObjectReference `json:"accountRef"`

	// Script is the Worker source code. Set either this or ScriptConfigMapRef.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Script *string `json:"script,omitempty"`

	// ScriptConfigMapRef reads the Worker source code from a key of a
	// ConfigMap in the same namespace. Set either this or Script.
	// +optional
	ScriptConfigMapRef *WorkerKeyReference `json:"scriptConfigMapRef,omitempty"`

	// Format is the script syntax: "module" (ES modules) or "serviceWorker".
	// +kubebuilder:validation:Enum=module;serviceWorker
	// +kubebuilder:default=module
	// +optional
	Format string `json:"format,omitempty"`

	// CompatibilityDate pins the Workers runtime behaviour (YYYY-MM-DD).
	// +kubebuilder:validation:Pattern=`^\d{4}-\d{2}-\d{2}$`
	// +optional
	CompatibilityDate string `json:"compatibilityDate,omitempty"`

	// CompatibilityFlags enable or disable individual runtime features.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	CompatibilityFlags []string `json:"compatibilityFlags,omitempty"`

	// Bindings expose values and resources to the Worker as environment
	// bindings. Names must be unique.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Bindings []WorkerBinding `json:"bindings,omitempty"`
}

// WorkerKeyReference selects a key of a ConfigMap or Secret in the same
// namespace as the WorkerScript. Both must exist; there is no optional form.
type WorkerKeyReference struct {
	// Name of the ConfigMap or Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key within the ConfigMap or Secret.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// WorkerBinding exposes one value or resource to the Worker under Name.
// Exactly one source must be set.
// +kubebuilder:validation:XValidation:rule="[has(self.plainText), has(self.secretKeyRef), has(self.kvNamespaceID), has(self.r2BucketName)].filter(x, x).size() == 1",message="exactly one of plainText, secretKeyRef, kvNamespaceID or r2BucketName must be set"
type WorkerBinding struct {
	// Name of the binding, as seen by the Worker (env.NAME).
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	Name string `json:"name"`

	// PlainText binds a plain text value.
	// +kubebuilder:validation:MinLength=1
	// +optional
	PlainText *string `json:"plainText,omitempty"`

	// SecretKeyRef binds a secret text value read from a key of a Secret in
	// the same namespace. The value is sent to Cloudflare and never written
	// to this resource's status.
	// +optional
	SecretKeyRef *WorkerKeyReference `json:"secretKeyRef,omitempty"`

	// KVNamespaceID binds a Workers KV namespace by ID.
	// +kubebuilder:validation:MinLength=1
	// +optional
	KVNamespaceID *string `json:"kvNamespaceID,omitempty"`

	// R2BucketName binds an R2 bucket by name.
	// +kubebuilder:validation:MinLength=1
	// +optional
	R2BucketName *string `json:"r2BucketName,omitempty"`
}

// WorkerScriptCloudflareMetadata holds metadata returned by the Cloudflare API.
type WorkerScriptCloudflareMetadata struct {
	// Etag is Cloudflare's hash of the uploaded code. It does not change when
	// only bindings or compatibility settings change.
	Etag string `json:"etag,omitempty"`

	// ModifiedOn is the modification time Cloudflare reported for kflare's
	// last upload, kept at full precision. A different value on Cloudflare
	// means the Worker was changed outside kflare.
	ModifiedOn string `json:"modifiedOn,omitempty"`
}

// WorkerScriptStatus defines the observed state of WorkerScript.
type WorkerScriptStatus struct {
	// Conditions represent the latest available observations of the Worker state.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// CloudflareMetadata holds metadata returned by the Cloudflare API.
	CloudflareMetadata WorkerScriptCloudflareMetadata `json:"cloudflareMetadata,omitempty"`

	// AppliedHash identifies the desired state last uploaded: the script,
	// format, compatibility settings and bindings. Secret bindings contribute
	// the Secret's UID and resourceVersion, never its value.
	AppliedHash string `json:"appliedHash,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cfworker
// +kubebuilder:printcolumn:name="Script",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Modified",type=string,JSONPath=`.status.cloudflareMetadata.modifiedOn`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// WorkerScript is the Schema for the workerscripts API.
// It represents a Cloudflare Worker script and its bindings.
type WorkerScript struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkerScriptSpec   `json:"spec,omitempty"`
	Status WorkerScriptStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkerScriptList contains a list of WorkerScript.
type WorkerScriptList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkerScript `json:"items"`
}

func init() {
	register(&WorkerScript{}, &WorkerScriptList{})
}
