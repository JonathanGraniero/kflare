/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package reconciler

import (
	"fmt"
	"sync/atomic"
	"time"
)

// renameGracePeriod is the rename grace period (SetRenameGracePeriod), in
// nanoseconds.
var renameGracePeriod atomic.Int64

// SetRenameGracePeriod sets how long a resource that is deleted keeps its
// Cloudflare object for a resource that replaces it. A rename is a new
// resource and the old one's deletion, and a tool that deletes first, as
// Argo CD prunes before it applies, leaves a moment with no new resource to
// hand the object to: deleting it then takes a DNS record out of its zone
// until the new resource creates it again. 0 deletes the object at once. The
// manager calls it once, before its controllers start
// (--rename-grace-period).
func SetRenameGracePeriod(period time.Duration) error {
	if period < 0 {
		return fmt.Errorf("rename grace period %s is negative: want 0 (off) or a positive duration", period)
	}
	renameGracePeriod.Store(int64(period))
	return nil
}

// RenameGracePeriod returns the rename grace period: 0 while none is set.
func RenameGracePeriod() time.Duration {
	return time.Duration(renameGracePeriod.Load())
}
