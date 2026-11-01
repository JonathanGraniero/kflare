/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// GetConditions and SetConditions give controllers uniform access to every
// kflare resource's status conditions. The kinds kflare-gen generates declare
// theirs in their generated types files.

// GetConditions returns the account's status conditions.
func (in *CloudflareAccount) GetConditions() []metav1.Condition { return in.Status.Conditions }

// SetConditions replaces the account's status conditions.
func (in *CloudflareAccount) SetConditions(c []metav1.Condition) { in.Status.Conditions = c }
