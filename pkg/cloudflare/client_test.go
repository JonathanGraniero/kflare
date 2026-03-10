/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package cloudflare_test

import (
	"testing"

	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

func TestNew_EmptyToken(t *testing.T) {
	_, err := cfpkg.New("")
	if err == nil {
		t.Fatal("expected error for empty token, got nil")
	}
}

func TestNew_ValidToken(t *testing.T) {
	// cloudflare-go only validates token format at construction time; a
	// syntactically valid token string (non-empty) is accepted without a
	// live API call.  Credential correctness is verified on the first request.
	client, err := cfpkg.New("test-token-value")
	if err != nil {
		t.Fatalf("unexpected error for non-empty token: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}
