/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("keyLock", func() {
	ctx := context.Background()

	// lockIn starts taking key in a goroutine and returns a channel that gets
	// its release once it holds the key, or nil if lock failed.
	lockIn := func(ctx context.Context, l *keyLock, key string) <-chan func() {
		got := make(chan func(), 1)
		go func() {
			defer GinkgoRecover()
			release, err := l.lock(ctx, key)
			if err != nil {
				got <- nil
				return
			}
			got <- release
		}()
		return got
	}

	It("lets one reconcile at a time hold a key", func() {
		var l keyLock
		release, err := l.lock(ctx, "zone/phase")
		Expect(err).NotTo(HaveOccurred())

		second := lockIn(ctx, &l, "zone/phase")
		Consistently(second, 200*time.Millisecond).ShouldNot(Receive(), "the key is held")

		release()
		var releaseSecond func()
		Eventually(second).Should(Receive(&releaseSecond), "the key is free")
		Expect(releaseSecond).NotTo(BeNil())
		releaseSecond()
		Expect(l.held()).To(BeZero())
	})

	It("does not hold one key up for another", func() {
		var l keyLock
		release, err := l.lock(ctx, "zone/phase")
		Expect(err).NotTo(HaveOccurred())
		defer release()

		other := lockIn(ctx, &l, "zone/other-phase")
		var releaseOther func()
		Eventually(other).Should(Receive(&releaseOther))
		Expect(releaseOther).NotTo(BeNil())
		releaseOther()
		Expect(l.held()).To(Equal(1), "only the first key is still held")
	})

	It("stops waiting when the reconcile's context is done, and drops the key once nobody holds it", func() {
		var l keyLock
		release, err := l.lock(ctx, "key")
		Expect(err).NotTo(HaveOccurred())

		waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		_, err = l.lock(waitCtx, "key")
		Expect(err).To(MatchError(context.DeadlineExceeded))
		Expect(l.held()).To(Equal(1))

		release()
		Expect(l.held()).To(BeZero())

		By("handing the key to the next reconcile, though one gave up waiting for it")
		again, err := l.lock(ctx, "key")
		Expect(err).NotTo(HaveOccurred())
		again()
	})

	It("releases a key once, however often its release is called", func() {
		var l keyLock
		release, err := l.lock(ctx, "key")
		Expect(err).NotTo(HaveOccurred())
		second := lockIn(ctx, &l, "key")

		release()
		var releaseSecond func()
		Eventually(second).Should(Receive(&releaseSecond))
		release()
		third := lockIn(ctx, &l, "key")
		Consistently(third, 100*time.Millisecond).ShouldNot(Receive(),
			"a second call of the first release frees nothing")

		releaseSecond()
		var releaseThird func()
		Eventually(third).Should(Receive(&releaseThird))
		releaseThird()
		Expect(l.held()).To(BeZero())
	})

	It("never lets two of many reconciles hold a key at once", func() {
		var l keyLock
		var holding, most, done atomic.Int32
		var wg sync.WaitGroup
		for range 50 {
			wg.Go(func() {
				defer GinkgoRecover()
				release, err := l.lock(ctx, "key")
				Expect(err).NotTo(HaveOccurred())
				now := holding.Add(1)
				for {
					seen := most.Load()
					if now <= seen || most.CompareAndSwap(seen, now) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				holding.Add(-1)
				done.Add(1)
				release()
			})
		}
		wg.Wait()
		Expect(done.Load()).To(BeEquivalentTo(50))
		Expect(most.Load()).To(BeEquivalentTo(1))
		Expect(l.held()).To(BeZero())
	})
})
