/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeAPI stands in for Cloudflare: it answers with what answer returns for a
// request, and records when each request reached it, by token. Nothing leaves
// the process, so the tests can run in a synctest bubble, whose clock moves
// only when every goroutine waits.
type fakeAPI struct {
	answer func(token string, sent int) *http.Response

	mu   sync.Mutex
	sent map[string][]time.Time
}

func (f *fakeAPI) RoundTrip(req *http.Request) (*http.Response, error) {
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	if f.sent == nil {
		f.sent = map[string][]time.Time{}
	}
	f.sent[token] = append(f.sent[token], time.Now())
	sent := len(f.sent[token])
	f.mu.Unlock()

	res := okResponse()
	if f.answer != nil {
		res = f.answer(token, sent)
	}
	res.Request = req
	return res, nil
}

// requests returns how many requests with token reached Cloudflare.
func (f *fakeAPI) requests(token string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent[token])
}

func okResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"result":{"id":"id","name":"name","title":"title"},"success":true,"errors":[],"messages":[]}`)),
	}
}

// tooManyRequests is Cloudflare's 429, with these extra headers.
func tooManyRequests(header ...string) *http.Response {
	res := okResponse()
	res.StatusCode = http.StatusTooManyRequests
	res.Body = io.NopCloser(strings.NewReader(
		`{"result":null,"success":false,"errors":[{"code":10000,"message":"rate limited"}],"messages":[]}`))
	for i := 0; i < len(header); i += 2 {
		res.Header.Set(header[i], header[i+1])
	}
	return res
}

// testClient returns a Client with token that spends limits and reaches api.
func testClient(t *testing.T, limits *rateLimiter, api http.RoundTripper, token string) *Client {
	t.Helper()
	client, err := newClient(token, withRateLimit(&http.Client{Transport: api}, limits))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	client.BaseURL = invalidBaseURL
	return client
}

// callV7 calls v7 the way the controllers do, at invalidBaseURL.
func callV7(client *Client) error {
	return getNamespace(context.Background(), client, option.WithBaseURL(invalidBaseURL+"/"))
}

// spend uses up b's tokens at now.
func spend(b *tokenBudget, now time.Time) {
	b.pace.ReserveN(now, int(b.pace.TokensAt(now)))
}

func TestRateLimit_ConcurrentCallersStayWithinIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const token = "paced-token"
		const seconds = 30
		api := &fakeAPI{}
		// One request a second, with a burst of 10.
		client := testClient(t, newRateLimiter(300), api, token)

		start := time.Now()
		var wg sync.WaitGroup
		for _, call := range []func(*Client) error{
			callV7,
			func(c *Client) error { return getAccount(context.Background(), c) },
			callV7,
			func(c *Client) error { return getAccount(context.Background(), c) },
		} {
			wg.Go(func() {
				for time.Since(start) < seconds*time.Second {
					if err := call(client); err != nil {
						t.Errorf("unexpected error: %v", err)
						return
					}
				}
			})
		}
		wg.Wait()

		sent := api.sent[token]
		if len(sent) < 10+seconds {
			t.Errorf("%d requests went out in %d seconds, want at least the burst and one a second", len(sent), seconds)
		}
		for i, at := range sent {
			if allowed := 10 + int(at.Sub(start)/time.Second); i+1 > allowed+1 {
				t.Fatalf("request %d went out after %s, when %d were allowed", i+1, at.Sub(start), allowed)
			}
		}
	})
}

func TestRateLimit_ClientsOfOneTokenShareABudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const token = "shared-token"
		limits, api := newRateLimiter(300), &fakeAPI{}
		for range 2 * burst(300) {
			if err := getAccount(context.Background(), testClient(t, limits, api, token)); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if got, want := time.Since(api.sent[token][0]), time.Duration(burst(300))*time.Second; got != want {
			t.Errorf("twice the burst took %s, want %s: the second burst paced at a request a second", got, want)
		}
	})
}

func TestRateLimit_A429HoldsEveryCallOfItsTokenAndNoOther(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const limited, other = "limited-token", "other-token"
		api := &fakeAPI{answer: func(token string, sent int) *http.Response {
			if token == limited && sent == 1 {
				return tooManyRequests("Retry-After", "300")
			}
			return okResponse()
		}}
		limits := newRateLimiter(DefaultRateLimit)
		client := testClient(t, limits, api, limited)

		start := time.Now()
		err := callV7(client)
		if !IsRateLimit(err) {
			t.Fatalf("err = %v, want a rate limit", err)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Errorf("v7 took %s over a 429, want it not to sleep for Retry-After", elapsed)
		}

		for name, call := range map[string]func() error{
			"v7": func() error { return callV7(client) },
			"v0": func() error { return getAccount(context.Background(), client) },
		} {
			if err := call(); !IsRateLimit(err) {
				t.Errorf("%s during the hold: err = %v, want a rate limit", name, err)
			}
			if got := api.requests(limited); got != 1 {
				t.Errorf("%s: %d requests reached Cloudflare during the hold, want only the 429", name, got-1)
			}
		}

		if err := getAccount(context.Background(), testClient(t, limits, api, other)); err != nil {
			t.Errorf("another token's call: %v", err)
		}
		if got := api.requests(other); got != 1 {
			t.Errorf("another token sent %d requests, want 1", got)
		}

		time.Sleep(time.Until(start.Add(300 * time.Second)))
		if err := callV7(client); err != nil {
			t.Errorf("after the hold: %v", err)
		}
		if got := api.requests(limited); got != 2 {
			t.Errorf("%d requests with the token, want the 429 and one after the hold", got)
		}
	})
}

// v0 retries a 429 whatever it is told: the hold keeps those retries from
// reaching Cloudflare, and what the caller sees is still a rate limit.
func TestRateLimit_V0RetriesOfA429AreNotSent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const token = "v0-retry-token"
		api := &fakeAPI{answer: func(string, int) *http.Response { return tooManyRequests("Retry-After", "60") }}
		err := getAccount(context.Background(), testClient(t, newRateLimiter(DefaultRateLimit), api, token))
		if !IsRateLimit(err) {
			t.Fatalf("err = %v, want a rate limit", err)
		}
		if IsTerminalError(err) {
			t.Errorf("err = %v is terminal", err)
		}
		if got := api.requests(token); got != 1 {
			t.Errorf("%d requests reached Cloudflare, want 1", got)
		}
	})
}

func TestRateLimit_ANearlySpentQuotaHoldsCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const token = "low-quota-token"
		api := &fakeAPI{answer: func(_ string, sent int) *http.Response {
			res := okResponse()
			if sent == 1 {
				res.Header.Set("Ratelimit", `"default";r=100;t=30`)
				res.Header.Set("Ratelimit-Policy", `"default";q=1200;w=300`)
			}
			return res
		}}
		limits := newRateLimiter(DefaultRateLimit)
		client := testClient(t, limits, api, token)
		if err := getAccount(context.Background(), client); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		start := time.Now()
		if err := getAccount(context.Background(), client); !IsRateLimit(err) {
			t.Fatalf("err = %v, want the call held", err)
		}
		time.Sleep(time.Until(start.Add(30 * time.Second)))
		if err := getAccount(context.Background(), client); err != nil {
			t.Errorf("after the hold: %v", err)
		}
		if got := api.requests(token); got != 2 {
			t.Errorf("%d requests reached Cloudflare, want 2", got)
		}
	})
}

// A short wait stays in the transport: only a long one gives the worker back.
func TestRateLimit_ACallWaitsOutAShortHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const token = "short-hold-token"
		api := &fakeAPI{answer: func(_ string, sent int) *http.Response {
			res := okResponse()
			if sent == 1 {
				res.Header.Set("Ratelimit", `"default";r=0;t=2`)
			}
			return res
		}}
		client := testClient(t, newRateLimiter(DefaultRateLimit), api, token)
		if err := getAccount(context.Background(), client); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Two seconds, because v0's retries of a refused call land on 1 and 3.
		start := time.Now()
		if err := getAccount(context.Background(), client); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if elapsed := time.Since(start); elapsed != 2*time.Second {
			t.Errorf("the call waited %s, want the hold's 2s", elapsed)
		}
	})
}

func TestRateLimit_ACallFailsRatherThanWaitLong(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const token = "long-wait-token"
		// Ten requests a minute, with a burst of 1: the second waits 30 seconds.
		api := &fakeAPI{}
		client := testClient(t, newRateLimiter(10), api, token)
		if err := callV7(client); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		start := time.Now()
		err := getAccount(context.Background(), client)
		if !IsRateLimit(err) {
			t.Fatalf("err = %v, want a rate limit", err)
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("err = %v holds the token", err)
		}
		if elapsed := time.Since(start); elapsed > maxCallWait {
			t.Errorf("the call took %s, want at most %s: v0's retries only", elapsed, maxCallWait)
		}
		if got := api.requests(token); got != 1 {
			t.Errorf("%d requests reached Cloudflare, want 1", got)
		}
	})
}

func TestTokenBudget_Acquire(t *testing.T) {
	t.Run("a hold that starts during the wait stops the call", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := newTokenBudget(300)
			now := time.Now()
			spend(b, now)
			errs := make(chan error, 1)
			go func() { errs <- b.acquire(context.Background()) }()
			synctest.Wait()

			time.Sleep(500 * time.Millisecond)
			b.observe(context.Background(), tooManyRequests("Retry-After", "60"), time.Now())
			var limited *rateLimitedError
			if err := <-errs; !errors.As(err, &limited) {
				t.Fatalf("err = %v, want it held", err)
			}
			if limited.wait != 59500*time.Millisecond {
				t.Errorf("wait = %s, want what is left of the hold", limited.wait)
			}
		})
	})

	t.Run("a cancelled call gives its turn back", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := newTokenBudget(300)
			now := time.Now()
			spend(b, now)
			ctx, cancel := context.WithCancel(context.Background())
			errs := make(chan error, 1)
			go func() { errs <- b.acquire(ctx) }()
			synctest.Wait()
			if tokens := b.pace.TokensAt(now); tokens != -1 {
				t.Fatalf("tokens = %v while the call waits, want -1", tokens)
			}

			cancel()
			if err := <-errs; !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want the context's", err)
			}
			if tokens := b.pace.TokensAt(now); tokens != 0 {
				t.Errorf("tokens = %v after the cancel, want the call's turn back", tokens)
			}
		})
	})

	t.Run("a call turned away leaves its turn", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := newTokenBudget(10)
			now := time.Now()
			spend(b, now)
			if err := b.acquire(context.Background()); err == nil {
				t.Fatal("a call 30 seconds off went ahead")
			}
			if tokens := b.pace.TokensAt(now); tokens != 0 {
				t.Errorf("tokens = %v, want the call's turn back", tokens)
			}
		})
	})

	t.Run("a call turned away by a hold leaves the bucket as it is", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := newTokenBudget(300)
			now := time.Now()
			spend(b, now)
			b.observe(context.Background(), tooManyRequests("Retry-After", "60"), now)
			if err := b.acquire(context.Background()); !IsRateLimit(err) {
				t.Fatalf("err = %v, want it held", err)
			}
			if tokens := b.pace.TokensAt(now); tokens != 0 {
				t.Errorf("tokens = %v: the hold's refill was credited early", tokens)
			}
		})
	})
}

func TestRateLimit_Off(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const token = "unpaced-token"
		api := &fakeAPI{answer: func(_ string, sent int) *http.Response {
			if sent == 1000 {
				return tooManyRequests("Retry-After", "60")
			}
			return okResponse()
		}}
		limits := newRateLimiter(0)
		client := testClient(t, limits, api, token)
		start := time.Now()
		for range 999 {
			if err := getAccount(context.Background(), client); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Errorf("unpaced calls took %s", elapsed)
		}
		budget := limits.budget(token, time.Now())
		if wait, _ := budget.admit(Admission{Account: "a"}, time.Now()); wait != 0 {
			t.Errorf("admit = %s, want no wait", wait)
		}

		if err := callV7(client); !IsRateLimit(err) {
			t.Fatalf("err = %v, want a rate limit", err)
		}
		held := time.Now()
		if err := getAccount(context.Background(), client); !IsRateLimit(err) {
			t.Errorf("err = %v, want Cloudflare's hold kept with the pacing off", err)
		}
		wait, reason := budget.admit(Admission{Account: "a"}, time.Now())
		if want := time.Minute - time.Since(held); wait != want || reason != deferredByHold {
			t.Errorf("admit = %s (%s), want what is left of the hold: %s", wait, reason, want)
		}
	})
}

func TestRoundTrip(t *testing.T) {
	t.Run("a request without a token passes through, unpaced", func(t *testing.T) {
		api := &fakeAPI{}
		transport := &rateLimitedTransport{next: api, limits: newRateLimiter(1)}
		for range 3 {
			req, err := http.NewRequest(http.MethodGet, invalidBaseURL, nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			_ = res.Body.Close()
		}
		if len(transport.limits.budgets) != 0 {
			t.Errorf("%d budgets, want none", len(transport.limits.budgets))
		}
	})

	t.Run("a failed request passes its error on", func(t *testing.T) {
		failure := errors.New("connection reset")
		transport := &rateLimitedTransport{
			next:   transportFunc(func(*http.Request) (*http.Response, error) { return nil, failure }),
			limits: newRateLimiter(DefaultRateLimit),
		}
		req, err := http.NewRequest(http.MethodGet, invalidBaseURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer token")
		if _, err := transport.RoundTrip(req); !errors.Is(err, failure) {
			t.Errorf("err = %v, want the transport's", err)
		}
	})

	t.Run("a refused request closes its body", func(t *testing.T) {
		limits := newRateLimiter(DefaultRateLimit)
		limits.budget("token", time.Now()).observe(context.Background(), tooManyRequests(), time.Now())
		body := &closeRecorder{Reader: strings.NewReader("{}")}
		req, err := http.NewRequest(http.MethodPut, invalidBaseURL, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer token")
		api := &fakeAPI{}
		if _, err := (&rateLimitedTransport{next: api, limits: limits}).RoundTrip(req); !IsRateLimit(err) {
			t.Fatalf("err = %v, want a rate limit", err)
		}
		if !body.closed {
			t.Error("the request's body was left open")
		}
		if api.requests("token") != 0 {
			t.Error("the request was sent")
		}
	})

	t.Run("only a 429 is marked not to retry", func(t *testing.T) {
		for status, want := range map[int]string{http.StatusTooManyRequests: "false", http.StatusOK: ""} {
			transport := &rateLimitedTransport{next: &fakeAPI{answer: func(string, int) *http.Response {
				res := okResponse()
				res.StatusCode = status
				return res
			}}, limits: newRateLimiter(DefaultRateLimit)}
			req, err := http.NewRequest(http.MethodGet, invalidBaseURL, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer token")
			res, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			_ = res.Body.Close()
			if got := res.Header.Get("x-should-retry"); got != want {
				t.Errorf("status %d: x-should-retry = %q, want %q", status, got, want)
			}
		}
	})
}

type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error {
	c.closed = true
	return nil
}

func TestTokenBudget_AdmitGoesAheadWithBudgetLeft(t *testing.T) {
	b, now := newTokenBudget(DefaultRateLimit), time.Now()
	for _, a := range []Admission{{Account: "a"}, {Account: "a", Recheck: true}} {
		if wait, _ := b.admit(a, now); wait != 0 {
			t.Errorf("admit(%+v) = %s, want no wait", a, wait)
		}
	}
}

func TestTokenBudget_AdmitGivesEachWaitingReconcileItsTurn(t *testing.T) {
	b, now := newTokenBudget(300), time.Now()
	spend(b, now)
	for i := 1; i <= 3; i++ {
		wait, reason := b.admit(Admission{Account: "a"}, now)
		if want := time.Duration(i) * time.Second; wait != want || reason != deferredByBudget {
			t.Errorf("reconcile %d: admit = %s (%s), want %s (%s)", i, wait, reason, want, deferredByBudget)
		}
	}
}

// A restart queues every resource: a change made then must not wait for them.
func TestTokenBudget_AChangeDoesNotWaitBehindRechecks(t *testing.T) {
	b, now := newTokenBudget(300), time.Now()
	spend(b, now)
	var last time.Duration
	for range 5000 {
		last, _ = b.admit(Admission{Account: "a", Recheck: true}, now)
	}
	if last != 5000*time.Second {
		t.Fatalf("the last recheck waits %s, want one turn a second", last)
	}
	if wait, _ := b.admit(Admission{Account: "a"}, now); wait != time.Second {
		t.Errorf("a change waits %s behind the rechecks, want a single turn", wait)
	}
}

func TestTokenBudget_ARecheckWaitsForChanges(t *testing.T) {
	b, now := newTokenBudget(300), time.Now()
	spend(b, now)
	for range 3 {
		b.admit(Admission{Account: "a"}, now)
	}

	// The budget has refilled, but the last change's turn is yet to come.
	now = now.Add(2 * time.Second)
	if wait, _ := b.admit(Admission{Account: "a", Recheck: true}, now); wait == 0 {
		t.Error("a recheck went ahead of a change waiting for its turn")
	}
	if wait, _ := b.admit(Admission{Account: "a"}, now); wait != 0 {
		t.Errorf("a change waits %s with budget left, want none", wait)
	}

	now = now.Add(time.Second)
	if wait, _ := b.admit(Admission{Account: "a", Recheck: true}, now); wait != 0 {
		t.Errorf("a recheck waits %s with no change waiting and budget left, want none", wait)
	}
}

func TestTokenBudget_AdmitAfterAHold(t *testing.T) {
	b, now := newTokenBudget(300), time.Now()
	b.observe(context.Background(), tooManyRequests("Retry-After", "60"), now)
	for i, a := range []Admission{{Account: "a"}, {Account: "a"}, {Account: "a", Recheck: true}} {
		want := time.Minute + time.Second
		if i == 1 {
			want += time.Second
		}
		wait, reason := b.admit(a, now)
		if wait != want || reason != deferredByHold {
			t.Errorf("admit(%+v) = %s (%s), want %s (%s)", a, wait, reason, want, deferredByHold)
		}
	}
	if wait, _ := b.admit(Admission{Account: "a"}, now.Add(time.Minute)); wait != 0 {
		// The turns taken are the ones of reconciles still away, but the budget
		// has refilled over the hold, so this one need not wait.
		t.Errorf("a reconcile after the hold waits %s with budget left", wait)
	}
}

func TestObserve(t *testing.T) {
	t.Run("a hold is only ever extended", func(t *testing.T) {
		b, now := newTokenBudget(DefaultRateLimit), time.Now()
		b.observe(context.Background(), tooManyRequests("Retry-After", "60"), now)
		b.observe(context.Background(), tooManyRequests("Retry-After", "5"), now)
		if want := now.Add(time.Minute); !b.heldUntil.Equal(want) {
			t.Errorf("heldUntil = %s, want %s", b.heldUntil, want)
		}
		b.observe(context.Background(), tooManyRequests("Retry-After", "120"), now)
		if want := now.Add(2 * time.Minute); !b.heldUntil.Equal(want) {
			t.Errorf("heldUntil = %s, want %s", b.heldUntil, want)
		}
	})

	t.Run("a response with quota left holds nothing", func(t *testing.T) {
		b, now := newTokenBudget(DefaultRateLimit), time.Now()
		res := okResponse()
		res.Header.Set("Ratelimit", `"default";r=1199;t=1`)
		res.Header.Set("Ratelimit-Policy", `"default";q=1200;w=300`)
		b.observe(context.Background(), res, now)
		if !b.heldUntil.IsZero() {
			t.Errorf("heldUntil = %s, want no hold", b.heldUntil)
		}
	})

	t.Run("a 429's Retry-After wins over its Ratelimit", func(t *testing.T) {
		b, now := newTokenBudget(DefaultRateLimit), time.Now()
		b.observe(context.Background(), tooManyRequests("Retry-After", "60", "Ratelimit", `"default";r=0;t=5`), now)
		if want := now.Add(time.Minute); !b.heldUntil.Equal(want) {
			t.Errorf("heldUntil = %s, want %s", b.heldUntil, want)
		}
	})

	t.Run("429s count for every account using the token, and holds do not", func(t *testing.T) {
		b, now := newTokenBudget(DefaultRateLimit), time.Now()
		first, second := t.Name()+"-first", t.Name()+"-second"
		b.admit(Admission{Account: first}, now)
		b.admit(Admission{Account: second}, now)
		before := testutil.ToFloat64(rateLimitedResponses.WithLabelValues(first))
		b.observe(context.Background(), tooManyRequests(), now)
		low := okResponse()
		low.Header.Set("Ratelimit", `"default";r=0;t=5`)
		b.observe(context.Background(), low, now)
		for _, account := range []string{first, second} {
			if got := testutil.ToFloat64(rateLimitedResponses.WithLabelValues(account)); got != before+1 {
				t.Errorf("%s: %v 429s counted, want %v", account, got, before+1)
			}
		}
	})
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name, value string
		want        time.Duration
	}{
		{"seconds", "30", 30 * time.Second},
		{"a date", now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{"missing", "", defaultHold},
		{"unreadable", "soon", defaultHold},
		{"zero", "0", defaultHold},
		{"negative", "-5", defaultHold},
		{"a date gone by", now.Add(-time.Minute).Format(http.TimeFormat), defaultHold},
		{"too long", "86400", maxHold},
		{"a date too far off", now.Add(24 * time.Hour).Format(http.TimeFormat), maxHold},
		{"too long for a duration", "9223372036854775807", maxHold},
	} {
		t.Run(c.name, func(t *testing.T) {
			header := http.Header{}
			if c.value != "" {
				header.Set("Retry-After", c.value)
			}
			if got := retryAfter(header, now); got != c.want {
				t.Errorf("retryAfter(%q) = %s, want %s", c.value, got, c.want)
			}
		})
	}
}

func TestQuotaWait(t *testing.T) {
	for _, c := range []struct {
		name     string
		limit    []string
		policy   []string
		wantWait time.Duration
	}{
		{name: "no headers"},
		{
			name:   "default, as Cloudflare sent it live",
			limit:  []string{`"default";r=1199;t=1`},
			policy: []string{`"default";q=1200;w=300`},
		},
		{
			name:   "a named limit, as Cloudflare sent it live",
			limit:  []string{`"list_zones";r=1200;t=1`},
			policy: []string{`"list_zones";q=1201;w=300`},
		},
		{
			name:     "at the reserve",
			limit:    []string{`"default";r=120;t=30`},
			policy:   []string{`"default";q=1200;w=300`},
			wantWait: 30 * time.Second,
		},
		{
			name:   "just above the reserve",
			limit:  []string{`"default";r=121;t=30`},
			policy: []string{`"default";q=1200;w=300`},
		},
		{
			name:  "a low figure without a policy says nothing",
			limit: []string{`"default";r=5;t=30`},
		},
		{
			name:     "spent, without a policy",
			limit:    []string{`"default";r=0;t=30`},
			wantWait: 30 * time.Second,
		},
		{
			name:     "another limit's policy does not count",
			limit:    []string{`"default";r=0;t=30`},
			policy:   []string{`"burst";q=100;w=60`},
			wantWait: 30 * time.Second,
		},
		{
			name:     "the furthest reset among the low limits",
			limit:    []string{`"default";r=1000;t=250, "burst";r=5;t=20, "other";r=0;t=10`},
			policy:   []string{`"default";q=1200;w=300, "burst";q=100;w=60`},
			wantWait: 20 * time.Second,
		},
		{
			name:     "limits on separate header lines",
			limit:    []string{`"default";r=1000;t=250`, `"burst";r=5;t=20`},
			policy:   []string{`"default";q=1200;w=300`, `"burst";q=100;w=60`},
			wantWait: 20 * time.Second,
		},
		{
			name:     "no reset says a second",
			limit:    []string{`"default";r=0`},
			wantWait: time.Second,
		},
		{
			name:     "a reset of 0 says a second",
			limit:    []string{`"default";r=0;t=0`},
			wantWait: time.Second,
		},
		{
			name:     "a reset too far off",
			limit:    []string{`"default";r=0;t=9223372036854775807`},
			wantWait: maxHold,
		},
		{
			name:  "no remaining figure",
			limit: []string{`"default";t=30`},
		},
		{
			name:  "unreadable",
			limit: []string{`nonsense`, `"default";r=lots;t=soon`, `;;;`, `,,,`, `"";r=0;t=30`, `"default";r=-1;t=30`},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			header := http.Header{}
			for _, value := range c.limit {
				header.Add("Ratelimit", value)
			}
			for _, value := range c.policy {
				header.Add("Ratelimit-Policy", value)
			}
			if got := quotaWait(header); got != c.wantWait {
				t.Errorf("quotaWait = %s, want %s", got, c.wantWait)
			}
		})
	}
}

// FuzzRateLimitHeaders feeds both waits what a network could put in the
// headers: they must never panic, or ask for a wait out of bounds.
func FuzzRateLimitHeaders(f *testing.F) {
	f.Add(`"default";r=1199;t=1`, `"default";q=1200;w=300`, "60")
	f.Add(`"list_zones";r=1200;t=1, "burst";r=0;t=9`, `"burst";q=100;w=60`, "Sun, 01 Nov 2026 00:00:50 GMT")
	f.Add(`;;=;,"`, `q=;"`, "-1")
	f.Fuzz(func(t *testing.T, limit, policy, after string) {
		header := http.Header{"Ratelimit": {limit}, "Ratelimit-Policy": {policy}, "Retry-After": {after}}
		if wait := quotaWait(header); wait < 0 || wait > maxHold {
			t.Errorf("quotaWait = %s", wait)
		}
		if wait := retryAfter(header, time.Now()); wait <= 0 || wait > maxHold {
			t.Errorf("retryAfter = %s", wait)
		}
	})
}

func TestTokenBudget_Limits(t *testing.T) {
	limitOf := func(b *tokenBudget) (float64, int) { return float64(b.pace.Limit()), b.pace.Burst() }

	t.Run("an account's limit replaces the default", func(t *testing.T) {
		b, now := newTokenBudget(300), time.Now()
		b.admit(Admission{Account: "a", RateLimit: 6000}, now)
		if limit, burst := limitOf(b); limit != 20 || burst != 200 {
			t.Errorf("rate %v, burst %d, want 20, 200", limit, burst)
		}
	})

	t.Run("accounts sharing a token get the lowest of their limits", func(t *testing.T) {
		b, now := newTokenBudget(300), time.Now()
		b.admit(Admission{Account: "a", RateLimit: 6000}, now)
		b.admit(Admission{Account: "b", RateLimit: 600}, now)
		b.admit(Admission{Account: "c"}, now)
		if limit, _ := limitOf(b); limit != 2 {
			t.Errorf("rate %v, want 2", limit)
		}
		b.admit(Admission{Account: "a", RateLimit: 6000}, now)
		if limit, _ := limitOf(b); limit != 2 {
			t.Errorf("rate %v, want 2 whichever account is seen last", limit)
		}
	})

	t.Run("unsetting a limit brings the default back", func(t *testing.T) {
		b, now := newTokenBudget(300), time.Now()
		b.admit(Admission{Account: "a", RateLimit: 6000}, now)
		b.admit(Admission{Account: "a"}, now)
		if limit, burst := limitOf(b); limit != 1 || burst != 10 {
			t.Errorf("rate %v, burst %d, want 1, 10", limit, burst)
		}
	})

	t.Run("a limit no account has mentioned for a while stops counting", func(t *testing.T) {
		b, now := newTokenBudget(300), time.Now()
		b.admit(Admission{Account: "deleted", RateLimit: 30}, now)
		b.admit(Admission{Account: "a", RateLimit: 600}, now)
		if limit, _ := limitOf(b); limit != 0.1 {
			t.Fatalf("rate %v, want 0.1", limit)
		}
		b.admit(Admission{Account: "a", RateLimit: 600}, now.Add(budgetIdle+time.Second))
		if limit, _ := limitOf(b); limit != 2 {
			t.Errorf("rate %v, want 2", limit)
		}
		if _, ok := b.accounts["deleted"]; ok {
			t.Error("the account is still recorded")
		}
	})

	t.Run("an account can pace a token kflare otherwise leaves unpaced", func(t *testing.T) {
		b, now := newTokenBudget(0), time.Now()
		b.admit(Admission{Account: "a", RateLimit: 300}, now)
		if limit, _ := limitOf(b); limit != 1 {
			t.Errorf("rate %v, want 1", limit)
		}
	})
}

func TestBurst(t *testing.T) {
	for limit, want := range map[int]int{0: 1, 1: 1, 29: 1, 30: 1, 60: 2, DefaultRateLimit: 30, 6000: 200} {
		if got := burst(limit); got != want {
			t.Errorf("burst(%d) = %d, want %d", limit, got, want)
		}
	}
}

func TestRateLimiter_DropsIdleBudgets(t *testing.T) {
	limits, now := newRateLimiter(DefaultRateLimit), time.Now()
	rotated := limits.budget("old-token", now)
	limits.budget("new-token", now)

	limits.budget("new-token", now.Add(budgetIdle))
	if len(limits.budgets) != 2 {
		t.Fatalf("%d budgets, want the idle token's kept until budgetIdle has passed", len(limits.budgets))
	}

	later := now.Add(budgetIdle + budgetSweepInterval + time.Second)
	limits.budget("new-token", later)
	if len(limits.budgets) != 1 {
		t.Fatalf("%d budgets, want the rotated token's dropped", len(limits.budgets))
	}
	if limits.budget("old-token", later) == rotated {
		t.Error("the dropped budget came back")
	}
}

func TestRateLimiter_SweepsAtMostEverySweepInterval(t *testing.T) {
	limits, now := newRateLimiter(DefaultRateLimit), time.Now()
	limits.budget("first", now)
	limits.budget("second", now)
	limits.budgets[sumOf("first")].lastUsed = now.Add(-2 * budgetIdle)
	limits.budget("second", now.Add(budgetSweepInterval-time.Second))
	if len(limits.budgets) != 2 {
		t.Errorf("%d budgets: a sweep ran within budgetSweepInterval", len(limits.budgets))
	}
	limits.budget("second", now.Add(budgetSweepInterval+time.Second))
	if len(limits.budgets) != 1 {
		t.Errorf("%d budgets: no sweep after budgetSweepInterval", len(limits.budgets))
	}
}

func TestBudgetIdle_OutlastsEveryHold(t *testing.T) {
	if budgetIdle < maxHold {
		t.Errorf("budgetIdle = %s: a budget could be dropped during a hold of %s", budgetIdle, maxHold)
	}
}

func TestSetRateLimit(t *testing.T) {
	original := limiter.limit
	t.Cleanup(func() { limiter.limit = original })

	if err := SetRateLimit(-1); err == nil {
		t.Error("a negative limit was accepted")
	}
	if err := SetRateLimit(600); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := limiter.budget("set-rate-limit-token", time.Now()).pace.Limit(); got != 2 {
		t.Errorf("rate %v, want 2", got)
	}
	if err := SetRateLimit(0); err != nil {
		t.Errorf("0 was refused: %v", err)
	}
}

// The default budget has to be what every Client spends.
func TestAdmit_SeesWhatEveryClientSends(t *testing.T) {
	transport := httpClient.Transport.(*rateLimitedTransport)
	original := transport.next
	t.Cleanup(func() { transport.next = original })
	transport.next = &fakeAPI{answer: func(string, int) *http.Response { return tooManyRequests("Retry-After", "60") }}

	token, other := uniqueToken("admit"), uniqueToken("admit-other")
	account := t.Name()
	client, err := New(token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wait := Admit(token, Admission{Account: account}); wait != 0 {
		t.Fatalf("Admit = %s before any call, want no wait", wait)
	}
	if err := callV7(client); !IsRateLimit(err) {
		t.Fatalf("err = %v, want a rate limit", err)
	}

	before := testutil.ToFloat64(deferredReconciles.WithLabelValues(account, deferredByHold))
	if wait := Admit(token, Admission{Account: account}); wait <= 0 || wait > time.Minute+time.Second {
		t.Errorf("Admit = %s, want about the 429's minute", wait)
	}
	if got := testutil.ToFloat64(deferredReconciles.WithLabelValues(account, deferredByHold)); got != before+1 {
		t.Errorf("%v reconciles counted as deferred by a hold, want %v", got, before+1)
	}
	if wait := Admit(other, Admission{Account: "other-" + account}); wait != 0 {
		t.Errorf("Admit = %s for another token, want no wait", wait)
	}
}

func TestAdmit_CountsReconcilesDeferredByTheBudget(t *testing.T) {
	token, account := uniqueToken("budget"), t.Name()
	now := time.Now()
	spend(limiter.budget(token, now), now)
	before := testutil.ToFloat64(deferredReconciles.WithLabelValues(account, deferredByBudget))
	if wait := Admit(token, Admission{Account: account}); wait <= 0 {
		t.Fatalf("Admit = %s with the budget spent", wait)
	}
	if got := testutil.ToFloat64(deferredReconciles.WithLabelValues(account, deferredByBudget)); got != before+1 {
		t.Errorf("%v reconciles counted as deferred by the budget, want %v", got, before+1)
	}
}

// uniqueToken returns a token of its own, for a test of the shared limiter
// that may run more than once in a process.
func uniqueToken(name string) string {
	return fmt.Sprintf("%s-%d", name, time.Now().UnixNano())
}

func sumOf(token string) [32]byte {
	limits := newRateLimiter(0)
	limits.budget(token, time.Now())
	for key := range limits.budgets {
		return key
	}
	panic(fmt.Sprintf("no budget for %q", token))
}
