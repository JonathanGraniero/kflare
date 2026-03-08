/*
Copyright 2026 kflare contributors.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSecretReference_DeepCopy(t *testing.T) {
	original := &SecretReference{Name: "sec", Namespace: "ns", Key: "tok"}
	copy := original.DeepCopy()

	if copy == original {
		t.Fatal("DeepCopy returned same pointer")
	}
	if *copy != *original {
		t.Fatalf("expected %+v, got %+v", *original, *copy)
	}

	// Mutating the copy must not affect the original.
	copy.Name = "changed"
	if original.Name != "sec" {
		t.Fatal("mutating copy affected original")
	}
}

func TestSecretReference_DeepCopy_Nil(t *testing.T) {
	var s *SecretReference
	if s.DeepCopy() != nil {
		t.Fatal("expected nil DeepCopy of nil receiver")
	}
}

func TestCloudflareAccountSpec_DeepCopy(t *testing.T) {
	original := &CloudflareAccountSpec{
		AccountID: "acct-1",
		TokenSecretRef: SecretReference{
			Name: "sec", Namespace: "ns", Key: "CF_API_TOKEN",
		},
	}
	copy := original.DeepCopy()

	if copy == original {
		t.Fatal("DeepCopy returned same pointer")
	}
	if copy.AccountID != original.AccountID {
		t.Fatalf("AccountID mismatch: %q vs %q", copy.AccountID, original.AccountID)
	}
	if copy.TokenSecretRef != original.TokenSecretRef {
		t.Fatalf("TokenSecretRef mismatch: %+v vs %+v", copy.TokenSecretRef, original.TokenSecretRef)
	}

	copy.AccountID = "changed"
	if original.AccountID != "acct-1" {
		t.Fatal("mutating copy affected original")
	}
}

func TestCloudflareAccountSpec_DeepCopy_Nil(t *testing.T) {
	var s *CloudflareAccountSpec
	if s.DeepCopy() != nil {
		t.Fatal("expected nil")
	}
}

func TestCloudflareAccountStatus_DeepCopy(t *testing.T) {
	original := &CloudflareAccountStatus{
		AccountName: "My Account",
		Conditions: []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Validated"},
		},
	}
	copy := original.DeepCopy()

	if copy == original {
		t.Fatal("DeepCopy returned same pointer")
	}
	if copy.AccountName != original.AccountName {
		t.Fatalf("AccountName mismatch")
	}
	if len(copy.Conditions) != len(original.Conditions) {
		t.Fatalf("Conditions length mismatch")
	}

	// Slice must be independent.
	copy.Conditions[0].Reason = "Changed"
	if original.Conditions[0].Reason != "Validated" {
		t.Fatal("mutating copy conditions affected original")
	}
}

func TestCloudflareAccountStatus_DeepCopy_NilConditions(t *testing.T) {
	original := &CloudflareAccountStatus{AccountName: "x"}
	copy := original.DeepCopy()
	if copy.Conditions != nil {
		t.Fatal("expected nil conditions slice on copy")
	}
}

func TestCloudflareAccountStatus_DeepCopy_Nil(t *testing.T) {
	var s *CloudflareAccountStatus
	if s.DeepCopy() != nil {
		t.Fatal("expected nil")
	}
}

func TestCloudflareAccount_DeepCopy(t *testing.T) {
	original := &CloudflareAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "acct", Namespace: ""},
		Spec: CloudflareAccountSpec{
			AccountID:      "id-1",
			TokenSecretRef: SecretReference{Name: "s", Namespace: "ns", Key: "k"},
		},
		Status: CloudflareAccountStatus{
			AccountName: "My Acct",
			Conditions:  []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
		},
	}
	copy := original.DeepCopy()

	if copy == original {
		t.Fatal("DeepCopy returned same pointer")
	}
	if copy.Name != "acct" {
		t.Fatalf("Name not copied")
	}
	if copy.Spec.AccountID != "id-1" {
		t.Fatalf("Spec not copied")
	}
	if copy.Status.AccountName != "My Acct" {
		t.Fatalf("Status not copied")
	}
}

func TestCloudflareAccount_DeepCopy_Nil(t *testing.T) {
	var a *CloudflareAccount
	if a.DeepCopy() != nil {
		t.Fatal("expected nil")
	}
}

func TestCloudflareAccount_DeepCopyObject(t *testing.T) {
	original := &CloudflareAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "acct"},
	}
	obj := original.DeepCopyObject()
	if obj == nil {
		t.Fatal("expected non-nil")
	}
	copied, ok := obj.(*CloudflareAccount)
	if !ok {
		t.Fatalf("expected *CloudflareAccount, got %T", obj)
	}
	if copied.Name != "acct" {
		t.Fatalf("Name not copied via DeepCopyObject")
	}
}

func TestCloudflareAccountList_DeepCopy(t *testing.T) {
	original := &CloudflareAccountList{
		Items: []CloudflareAccount{
			{ObjectMeta: metav1.ObjectMeta{Name: "a"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "b"}},
		},
	}
	copy := original.DeepCopy()

	if copy == original {
		t.Fatal("DeepCopy returned same pointer")
	}
	if len(copy.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(copy.Items))
	}

	// Items slice must be independent.
	copy.Items[0].Name = "changed"
	if original.Items[0].Name != "a" {
		t.Fatal("mutating copy items affected original")
	}
}

func TestCloudflareAccountList_DeepCopy_NilItems(t *testing.T) {
	original := &CloudflareAccountList{}
	copy := original.DeepCopy()
	if copy.Items != nil {
		t.Fatal("expected nil items on copy")
	}
}

func TestCloudflareAccountList_DeepCopy_Nil(t *testing.T) {
	var l *CloudflareAccountList
	if l.DeepCopy() != nil {
		t.Fatal("expected nil")
	}
}

func TestCloudflareAccountList_DeepCopyObject(t *testing.T) {
	original := &CloudflareAccountList{
		Items: []CloudflareAccount{{ObjectMeta: metav1.ObjectMeta{Name: "x"}}},
	}
	obj := original.DeepCopyObject()
	if obj == nil {
		t.Fatal("expected non-nil")
	}
	if _, ok := obj.(*CloudflareAccountList); !ok {
		t.Fatalf("expected *CloudflareAccountList, got %T", obj)
	}
}

func TestCloudflareAccount_DeepCopyObject_Nil(t *testing.T) {
	var a *CloudflareAccount
	if a.DeepCopyObject() != nil {
		t.Fatal("expected nil DeepCopyObject of nil receiver")
	}
}

func TestCloudflareAccountList_DeepCopyObject_Nil(t *testing.T) {
	var l *CloudflareAccountList
	if l.DeepCopyObject() != nil {
		t.Fatal("expected nil DeepCopyObject of nil receiver")
	}
}
