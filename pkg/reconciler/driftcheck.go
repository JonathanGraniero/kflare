/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package reconciler

import (
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"
)

// driftCheckInterval is the drift check interval (SetDriftCheckInterval), in
// nanoseconds.
var driftCheckInterval atomic.Int64

// SetDriftCheckInterval sets how long after a successful reconcile a resource
// is reconciled again. A change made outside kflare, in the Cloudflare
// dashboard, through the API or by Terraform, triggers no reconcile of its
// own, so this bounds how long it stays before kflare corrects it. 0, the
// default, schedules nothing: a resource is reconciled again only when it,
// or something it watches, changes, or at the informers' resync. The manager
// calls it once, before its controllers start (--drift-check-interval).
func SetDriftCheckInterval(interval time.Duration) error {
	if interval < 0 {
		return fmt.Errorf("drift check interval %s is negative: want 0 (off) or a positive duration", interval)
	}
	driftCheckInterval.Store(int64(interval))
	return nil
}

// NextDriftCheck returns how long after a successful reconcile to check the
// resource again: the drift check interval, moved by up to a tenth of it
// either way at random, so that resources synced together, as after a
// restart, spread their checks instead of calling Cloudflare at once. It is 0
// while no interval is set.
func NextDriftCheck() time.Duration {
	interval := time.Duration(driftCheckInterval.Load())
	spread := interval / 10
	return interval - spread + rand.N(2*spread+1)
}
