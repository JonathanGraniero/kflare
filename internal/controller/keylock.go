/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// keyLock lets one reconcile at a time hold each key, so that reconciles that
// run at once (--max-concurrent-reconciles) never act together on the same
// thing in Cloudflare: a zone's ruleset for one phase, or the object two
// resources of a kind would both create. Its keys live in the manager's
// memory, which is enough because only the leader reconciles. The zero value
// is ready to use.
type keyLock struct {
	mu sync.Mutex
	// keys holds the keys held or waited for. A key is dropped once nobody
	// holds or waits for it, so the map stays as small as the work in hand.
	keys map[string]*keyTurn
}

// keyTurn is one key of a keyLock: turn holds a value while a reconcile holds
// the key, and users counts those holding or waiting for it.
type keyTurn struct {
	turn  chan struct{}
	users int
}

// lock waits until the reconcile holds key, or ctx is done, and returns the
// function that releases it, which may be called more than once. A reconcile
// that waits logs so, at debug level: a wait means another reconcile is
// writing the same thing, and lasts as long as that reconcile's calls.
func (l *keyLock) lock(ctx context.Context, key string) (release func(), err error) {
	l.mu.Lock()
	if l.keys == nil {
		l.keys = map[string]*keyTurn{}
	}
	k := l.keys[key]
	if k == nil {
		k = &keyTurn{turn: make(chan struct{}, 1)}
		l.keys[key] = k
	}
	k.users++
	l.mu.Unlock()

	release = sync.OnceFunc(func() {
		<-k.turn
		l.leave(key, k)
	})
	select {
	case k.turn <- struct{}{}:
		return release, nil
	default:
	}
	log.FromContext(ctx).V(1).Info("Waiting for another reconcile writing the same thing to Cloudflare", "key", key)
	select {
	case k.turn <- struct{}{}:
		return release, nil
	case <-ctx.Done():
		l.leave(key, k)
		return nil, ctx.Err()
	}
}

// leave counts out one user of key, and drops the key once it has none.
func (l *keyLock) leave(key string, k *keyTurn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k.users--
	if k.users == 0 {
		delete(l.keys, key)
	}
}

// held returns how many keys are held or waited for, for tests.
func (l *keyLock) held() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.keys)
}
