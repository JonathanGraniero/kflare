/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

// TunnelTokenKey is the key under which the tunnel token is stored in the
// credentials Secret. cloudflared reads the same name from the environment,
// so the Secret can be mounted with envFrom and run with `cloudflared tunnel run`.
const TunnelTokenKey = "TUNNEL_TOKEN"

// TunnelSpecExtras holds what a Tunnel's spec asks for beyond the tunnel
// service: where the kind's hooks keep the tunnel's token.
type TunnelSpecExtras struct {
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

// TunnelStatusExtras holds what the hooks record about the token's Secret.
type TunnelStatusExtras struct {
	// CredentialsSecretName is the Secret the token was last written to. When
	// spec.credentialsSecretRef changes, kflare deletes this Secret after
	// writing the new one.
	CredentialsSecretName string `json:"credentialsSecretName,omitempty"`
}
