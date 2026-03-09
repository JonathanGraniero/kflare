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

// ---------------------------------------------------------------------------
// Zone deep-copy tests
// ---------------------------------------------------------------------------

func TestZoneCloudflareMetadata_DeepCopy(t *testing.T) {
	original := &ZoneCloudflareMetadata{
		ZoneID:      "z1",
		NameServers: []string{"ns1.example.com", "ns2.example.com"},
		Status:      "active",
	}
	copy := original.DeepCopy()

	if copy == original {
		t.Fatal("DeepCopy returned same pointer")
	}
	if copy.ZoneID != original.ZoneID || copy.Status != original.Status {
		t.Fatalf("scalar fields not copied: %+v vs %+v", copy, original)
	}
	if len(copy.NameServers) != len(original.NameServers) {
		t.Fatalf("NameServers length mismatch")
	}

	// Slice must be independent.
	copy.NameServers[0] = "changed"
	if original.NameServers[0] != "ns1.example.com" {
		t.Fatal("mutating copy NameServers affected original")
	}
}

func TestZoneCloudflareMetadata_DeepCopy_NilNameServers(t *testing.T) {
	original := &ZoneCloudflareMetadata{ZoneID: "z1"}
	copy := original.DeepCopy()
	if copy.NameServers != nil {
		t.Fatal("expected nil NameServers on copy")
	}
}

func TestZoneCloudflareMetadata_DeepCopy_Nil(t *testing.T) {
	var m *ZoneCloudflareMetadata
	if m.DeepCopy() != nil {
		t.Fatal("expected nil")
	}
}

func TestZoneSpec_DeepCopy(t *testing.T) {
	original := &ZoneSpec{
		Name: "example.com",
		Plan: "free",
		Type: "full",
	}
	copy := original.DeepCopy()

	if copy == original {
		t.Fatal("DeepCopy returned same pointer")
	}
	if copy.Name != original.Name || copy.Plan != original.Plan || copy.Type != original.Type {
		t.Fatalf("fields not copied: %+v vs %+v", copy, original)
	}

	copy.Name = "changed.com"
	if original.Name != "example.com" {
		t.Fatal("mutating copy Name affected original")
	}
}

func TestZoneSpec_DeepCopy_Nil(t *testing.T) {
	var s *ZoneSpec
	if s.DeepCopy() != nil {
		t.Fatal("expected nil")
	}
}

func TestZoneStatus_DeepCopy(t *testing.T) {
	original := &ZoneStatus{
		Conditions: []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Synced"},
		},
		CloudflareMetadata: ZoneCloudflareMetadata{
			ZoneID:      "z1",
			NameServers: []string{"ns1.cloudflare.com"},
			Status:      "active",
		},
	}
	copy := original.DeepCopy()

	if copy == original {
		t.Fatal("DeepCopy returned same pointer")
	}
	if len(copy.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(copy.Conditions))
	}
	if copy.CloudflareMetadata.ZoneID != "z1" {
		t.Fatalf("ZoneID not copied")
	}

	// Conditions slice must be independent.
	copy.Conditions[0].Reason = "Changed"
	if original.Conditions[0].Reason != "Synced" {
		t.Fatal("mutating copy conditions affected original")
	}

	// NameServers slice must be independent.
	copy.CloudflareMetadata.NameServers[0] = "changed"
	if original.CloudflareMetadata.NameServers[0] != "ns1.cloudflare.com" {
		t.Fatal("mutating copy NameServers affected original")
	}
}

func TestZoneStatus_DeepCopy_NilConditions(t *testing.T) {
	original := &ZoneStatus{CloudflareMetadata: ZoneCloudflareMetadata{ZoneID: "z1"}}
	copy := original.DeepCopy()
	if copy.Conditions != nil {
		t.Fatal("expected nil conditions on copy")
	}
}

func TestZoneStatus_DeepCopy_Nil(t *testing.T) {
	var s *ZoneStatus
	if s.DeepCopy() != nil {
		t.Fatal("expected nil")
	}
}

func TestZone_DeepCopy(t *testing.T) {
	original := &Zone{
		ObjectMeta: metav1.ObjectMeta{Name: "my-zone", Namespace: "default"},
		Spec: ZoneSpec{
			Name: "example.com",
			Type: "full",
		},
		Status: ZoneStatus{
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Synced"},
			},
			CloudflareMetadata: ZoneCloudflareMetadata{ZoneID: "z1"},
		},
	}
	copy := original.DeepCopy()

	if copy == original {
		t.Fatal("DeepCopy returned same pointer")
	}
	if copy.Name != "my-zone" || copy.Spec.Name != "example.com" {
		t.Fatalf("fields not copied")
	}
	if copy.Status.CloudflareMetadata.ZoneID != "z1" {
		t.Fatalf("status not copied")
	}
}

func TestZone_DeepCopy_Nil(t *testing.T) {
	var z *Zone
	if z.DeepCopy() != nil {
		t.Fatal("expected nil")
	}
}

func TestZone_DeepCopyObject(t *testing.T) {
	original := &Zone{ObjectMeta: metav1.ObjectMeta{Name: "my-zone"}}
	obj := original.DeepCopyObject()
	if obj == nil {
		t.Fatal("expected non-nil")
	}
	copied, ok := obj.(*Zone)
	if !ok {
		t.Fatalf("expected *Zone, got %T", obj)
	}
	if copied.Name != "my-zone" {
		t.Fatalf("Name not copied via DeepCopyObject")
	}
}

func TestZone_DeepCopyObject_Nil(t *testing.T) {
	var z *Zone
	if z.DeepCopyObject() != nil {
		t.Fatal("expected nil DeepCopyObject of nil receiver")
	}
}

func TestZoneList_DeepCopy(t *testing.T) {
	original := &ZoneList{
		Items: []Zone{
			{ObjectMeta: metav1.ObjectMeta{Name: "zone-a"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "zone-b"}},
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
	if original.Items[0].Name != "zone-a" {
		t.Fatal("mutating copy items affected original")
	}
}

func TestZoneList_DeepCopy_NilItems(t *testing.T) {
	original := &ZoneList{}
	copy := original.DeepCopy()
	if copy.Items != nil {
		t.Fatal("expected nil items on copy")
	}
}

func TestZoneList_DeepCopy_Nil(t *testing.T) {
	var l *ZoneList
	if l.DeepCopy() != nil {
		t.Fatal("expected nil")
	}
}

func TestZoneList_DeepCopyObject(t *testing.T) {
	original := &ZoneList{
		Items: []Zone{{ObjectMeta: metav1.ObjectMeta{Name: "z"}}},
	}
	obj := original.DeepCopyObject()
	if obj == nil {
		t.Fatal("expected non-nil")
	}
	if _, ok := obj.(*ZoneList); !ok {
		t.Fatalf("expected *ZoneList, got %T", obj)
	}
}

func TestZoneList_DeepCopyObject_Nil(t *testing.T) {
	var l *ZoneList
	if l.DeepCopyObject() != nil {
		t.Fatal("expected nil DeepCopyObject of nil receiver")
	}
}
