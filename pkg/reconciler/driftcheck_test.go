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

// setDriftCheckInterval sets the drift check interval for one test.
func setDriftCheckInterval(t *testing.T, interval time.Duration) {
	t.Helper()
	if err := reconciler.SetDriftCheckInterval(interval); err != nil {
		t.Fatalf("SetDriftCheckInterval(%s): %v", interval, err)
	}
	t.Cleanup(func() { _ = reconciler.SetDriftCheckInterval(0) })
}

func TestNextDriftCheckIsOffByDefault(t *testing.T) {
	if got := reconciler.NextDriftCheck(); got != 0 {
		t.Errorf("NextDriftCheck() = %s without an interval, want 0", got)
	}
}

func TestNextDriftCheckSpreadsChecksAroundTheInterval(t *testing.T) {
	setDriftCheckInterval(t, time.Hour)
	lowest, highest := time.Duration(1<<63-1), time.Duration(0)
	for range 1000 {
		got := reconciler.NextDriftCheck()
		if got < 54*time.Minute || got > 66*time.Minute {
			t.Fatalf("NextDriftCheck() = %s, want within a tenth of an hour of it", got)
		}
		lowest, highest = min(lowest, got), max(highest, got)
	}
	// 1,000 draws all missing the first or the last twelfth of the range
	// happen less than once in 10^37.
	if lowest > 55*time.Minute || highest < 65*time.Minute {
		t.Errorf("1,000 checks fell between %s and %s, want them spread over 54m to 66m", lowest, highest)
	}
}

func TestNextDriftCheckOfAnIntervalTooShortToSpread(t *testing.T) {
	setDriftCheckInterval(t, 9*time.Nanosecond)
	for range 100 {
		if got := reconciler.NextDriftCheck(); got != 9*time.Nanosecond {
			t.Fatalf("NextDriftCheck() = %s, want 9ns", got)
		}
	}
}

func TestSetDriftCheckIntervalTurnsTheChecksOff(t *testing.T) {
	setDriftCheckInterval(t, time.Hour)
	setDriftCheckInterval(t, 0)
	if got := reconciler.NextDriftCheck(); got != 0 {
		t.Errorf("NextDriftCheck() = %s after the interval was set to 0, want 0", got)
	}
}

func TestSetDriftCheckIntervalRefusesANegativeInterval(t *testing.T) {
	setDriftCheckInterval(t, time.Hour)
	if err := reconciler.SetDriftCheckInterval(-time.Second); err == nil {
		t.Fatal("SetDriftCheckInterval(-1s) succeeded, want an error")
	}
	if got := reconciler.NextDriftCheck(); got < 54*time.Minute || got > 66*time.Minute {
		t.Errorf("NextDriftCheck() = %s after a refused interval, want the hour set before", got)
	}
}
