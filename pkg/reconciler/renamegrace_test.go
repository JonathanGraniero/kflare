/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package reconciler_test

import (
	"testing"
	"time"

	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

func TestRenameGracePeriod(t *testing.T) {
	if got := reconciler.RenameGracePeriod(); got != 0 {
		t.Errorf("RenameGracePeriod() = %s without one set, want 0", got)
	}
	t.Cleanup(func() { _ = reconciler.SetRenameGracePeriod(0) })
	if err := reconciler.SetRenameGracePeriod(30 * time.Second); err != nil {
		t.Fatalf("SetRenameGracePeriod(30s): %v", err)
	}
	if got := reconciler.RenameGracePeriod(); got != 30*time.Second {
		t.Errorf("RenameGracePeriod() = %s, want 30s", got)
	}
	if err := reconciler.SetRenameGracePeriod(-time.Second); err == nil {
		t.Error("SetRenameGracePeriod(-1s) returned no error")
	}
	if got := reconciler.RenameGracePeriod(); got != 30*time.Second {
		t.Errorf("RenameGracePeriod() = %s after a refused period, want 30s", got)
	}
}
