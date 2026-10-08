/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// GetConditions and SetConditions give controllers uniform access to every
// kflare resource's status conditions.

// GetConditions returns the account's status conditions.
func (in *CloudflareAccount) GetConditions() []metav1.Condition { return in.Status.Conditions }

// SetConditions replaces the account's status conditions.
func (in *CloudflareAccount) SetConditions(c []metav1.Condition) { in.Status.Conditions = c }

// GetConditions returns the zone's status conditions.
func (in *Zone) GetConditions() []metav1.Condition { return in.Status.Conditions }

// SetConditions replaces the zone's status conditions.
func (in *Zone) SetConditions(c []metav1.Condition) { in.Status.Conditions = c }

// GetConditions returns the DNS record's status conditions.
func (in *DNSRecord) GetConditions() []metav1.Condition { return in.Status.Conditions }

// SetConditions replaces the DNS record's status conditions.
func (in *DNSRecord) SetConditions(c []metav1.Condition) { in.Status.Conditions = c }

// GetConditions returns the tunnel's status conditions.
func (in *Tunnel) GetConditions() []metav1.Condition { return in.Status.Conditions }

// SetConditions replaces the tunnel's status conditions.
func (in *Tunnel) SetConditions(c []metav1.Condition) { in.Status.Conditions = c }

// GetConditions returns the tunnel configuration's status conditions.
func (in *TunnelConfiguration) GetConditions() []metav1.Condition { return in.Status.Conditions }

// SetConditions replaces the tunnel configuration's status conditions.
func (in *TunnelConfiguration) SetConditions(c []metav1.Condition) { in.Status.Conditions = c }

// GetConditions returns the Worker script's status conditions.
func (in *WorkerScript) GetConditions() []metav1.Condition { return in.Status.Conditions }

// SetConditions replaces the Worker script's status conditions.
func (in *WorkerScript) SetConditions(c []metav1.Condition) { in.Status.Conditions = c }

// GetConditions returns the Worker route's status conditions.
func (in *WorkerRoute) GetConditions() []metav1.Condition { return in.Status.Conditions }

// SetConditions replaces the Worker route's status conditions.
func (in *WorkerRoute) SetConditions(c []metav1.Condition) { in.Status.Conditions = c }

// GetConditions returns the KV namespace's status conditions.
func (in *KVNamespace) GetConditions() []metav1.Condition { return in.Status.Conditions }

// SetConditions replaces the KV namespace's status conditions.
func (in *KVNamespace) SetConditions(c []metav1.Condition) { in.Status.Conditions = c }

// GetConditions returns the R2 bucket's status conditions.
func (in *R2Bucket) GetConditions() []metav1.Condition { return in.Status.Conditions }

// SetConditions replaces the R2 bucket's status conditions.
func (in *R2Bucket) SetConditions(c []metav1.Condition) { in.Status.Conditions = c }
