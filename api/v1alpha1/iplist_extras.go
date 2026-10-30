/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

// IPListSpecExtras holds what an IPList's spec asks for beyond the list
// service: Cloudflare keeps a list's items in a service of their own, which the
// kind's hooks call.
type IPListSpecExtras struct {
	// Items are the addresses and ranges in the list. kflare replaces the
	// list's items whenever they differ from these.
	// +optional
	// +listType=map
	// +listMapKey=ip
	Items []IPListItem `json:"items,omitempty"`
}

// IPListItem is one address or range in an IP list.
type IPListItem struct {
	// IP is an IPv4 or IPv6 address, or a CIDR range without host bits set:
	// /8 to /32 for IPv4, /12 to /128 for IPv6. Cloudflare refuses
	// IPv4-mapped IPv6 addresses.
	// +kubebuilder:validation:MaxLength=49
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F:.]+(/[0-9]{1,3})?$`
	IP string `json:"ip"`

	// Comment describes the item in the Cloudflare dashboard.
	// +optional
	// +kubebuilder:validation:MaxLength=500
	Comment string `json:"comment,omitempty"`
}

// IPListMetadataExtras holds what the hooks record about a list's items.
type IPListMetadataExtras struct {
	// NumItems is how many items the list held when it was last synced.
	// +optional
	NumItems int32 `json:"numItems"`

	// OperationID is Cloudflare's bulk operation replacing the list's items,
	// while kflare waits for it to finish.
	OperationID string `json:"operationID,omitempty"`
}
