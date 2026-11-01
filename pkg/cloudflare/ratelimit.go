/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package cloudflare

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	// DefaultRateLimit is how many requests per five minutes kflare sends
	// with one API token, unless SetRateLimit or the token's CloudflareAccount
	// says otherwise: three quarters of Cloudflare's global limit of 1,200, so
	// that the dashboard and other tools using the same token keep working.
	DefaultRateLimit = 900

	// rateLimitWindow is the period Cloudflare's limits are stated over.
	rateLimitWindow = 5 * time.Minute

	// burstDivisor gives a budget's burst: a thirtieth of its limit, the
	// requests of ten seconds at the limit's rate, so that a handful of
	// changes never waits.
	burstDivisor = 30

	// maxCallWait is the longest one call waits in the transport for its
	// turn. A longer wait fails the call instead, unsent: a waiting call holds
	// its controller's only worker, which every other account's resources of
	// the kind are waiting for.
	maxCallWait = 10 * time.Second

	// defaultHold is how long a token's calls stop after a 429 that does not
	// say for how long: Cloudflare blocks all calls for five minutes past its
	// global limit.
	defaultHold = rateLimitWindow

	// maxHold bounds a hold, whatever a response asks for.
	maxHold = time.Hour

	// quotaReserve is the share of a quota kflare leaves to other users of the
	// token: it stops calling when a limit has a tenth of its quota left.
	quotaReserve = 10

	// budgetIdle is how long a budget stays without calls before it is
	// dropped, and with it a rotated token's. At least maxHold, so that no
	// budget is dropped while it holds calls.
	budgetIdle = maxHold

	// budgetSweepInterval is how often idle budgets are looked for.
	budgetSweepInterval = 10 * time.Minute
)

// The reasons kflare_cloudflare_reconciles_deferred_total counts.
const (
	deferredByHold   = "hold"
	deferredByBudget = "budget"
)

var (
	rateLimitedResponses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kflare_cloudflare_api_rate_limited_total",
		Help: "Responses with status 429 from Cloudflare, by CloudflareAccount using the token.",
	}, []string{"account"})

	deferredReconciles = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kflare_cloudflare_reconciles_deferred_total",
		Help: "Reconciles put off because of Cloudflare's rate limit, by CloudflareAccount and reason: hold " +
			"(Cloudflare asked kflare to stop) or budget (the token's budget is used up).",
	}, []string{"account", "reason"})
)

func init() {
	metrics.Registry.MustRegister(rateLimitedResponses, deferredReconciles)
}

// rateLimiter keeps one budget per API token, for every call kflare makes
// with either SDK. Cloudflare counts calls per user or account token, not per
// Cloudflare account, so a token used by several CloudflareAccounts has one
// budget. A token's budget is found by a hash of it, so the token itself stays
// out of the limiter.
//
// Two things use a budget. A reconcile asks Admit before it starts: when the
// budget is used up, or Cloudflare asked kflare to stop, it comes back later
// and its controller's worker goes on to the next resource, which may be of
// another account. The transport then paces each call, so that no reconcile
// that went ahead sends more than the budget allows, and stops what is in
// flight when a response says to.
//
// Budgets live in the manager's memory. That is enough because only the leader
// calls Cloudflare, but a new leader starts with full budgets, and two
// installations sharing a token each spend theirs. Both learn otherwise from
// Cloudflare's responses.
type rateLimiter struct {
	mu        sync.Mutex
	budgets   map[[sha256.Size]byte]*tokenBudget
	limit     int
	nextSweep time.Time
}

// limiter is the rateLimiter of every Client.
var limiter = newRateLimiter(DefaultRateLimit)

func newRateLimiter(limit int) *rateLimiter {
	return &rateLimiter{budgets: map[[sha256.Size]byte]*tokenBudget{}, limit: limit}
}

// SetRateLimit sets how many requests per five minutes kflare sends with one
// API token. 0 lets calls through unpaced, though still not during a hold
// Cloudflare asks for. cmd/main.go calls it once, before its controllers start
// (--cloudflare-rate-limit).
func SetRateLimit(limit int) error {
	if limit < 0 {
		return fmt.Errorf("rate limit %d: must be 0 (off) or positive", limit)
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	limiter.limit = limit
	return nil
}

// budget returns token's budget, which it makes on the token's first use, and
// drops the budgets of tokens no longer in use.
func (l *rateLimiter) budget(token string, now time.Time) *tokenBudget {
	key := sha256.Sum256([]byte(token))
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.After(l.nextSweep) {
		l.nextSweep = now.Add(budgetSweepInterval)
		maps.DeleteFunc(l.budgets, func(_ [sha256.Size]byte, b *tokenBudget) bool {
			return now.Sub(b.lastUsed) > budgetIdle
		})
	}
	b, ok := l.budgets[key]
	if !ok {
		b = newTokenBudget(l.limit)
		l.budgets[key] = b
	}
	b.lastUsed = now
	return b
}

// tokenBudget is what one API token may spend.
type tokenBudget struct {
	// lastUsed is guarded by rateLimiter.mu.
	lastUsed time.Time

	// pace is the token bucket every call draws from.
	pace *rate.Limiter

	mu sync.Mutex
	// defaultLimit and limit are in requests per five minutes: what the
	// budget uses unless an account says otherwise, and what it uses now.
	defaultLimit, limit int
	// accounts are the CloudflareAccounts using the token, by name.
	accounts map[string]accountUse
	// heldUntil is when calls may start again.
	heldUntil time.Time
	// nextChange and nextCheck are the next free turns of the reconciles Admit
	// puts off (see admit).
	nextChange, nextCheck time.Time
}

// accountUse is what a CloudflareAccount last said about a token it uses.
type accountUse struct {
	limit int
	seen  time.Time
}

func newTokenBudget(limit int) *tokenBudget {
	return &tokenBudget{
		pace:         rate.NewLimiter(perSecond(limit), burst(limit)),
		defaultLimit: limit,
		limit:        limit,
		accounts:     map[string]accountUse{},
	}
}

// perSecond returns a limit in requests per five minutes as a rate.
func perSecond(limit int) rate.Limit {
	if limit == 0 {
		return rate.Inf
	}
	return rate.Limit(float64(limit) / rateLimitWindow.Seconds())
}

func burst(limit int) int {
	return max(1, limit/burstDivisor)
}

// Admission describes a reconcile about to call Cloudflare, for Admit.
type Admission struct {
	// Account names the CloudflareAccount whose token it calls with.
	Account string
	// RateLimit is that account's limit in requests per five minutes, 0 if it
	// sets none. CloudflareAccounts sharing a token share its budget, so the
	// lowest they ask for applies.
	RateLimit int
	// Recheck says that the reconcile checks a resource already in sync, such
	// as after a restart or at a drift check, rather than acting on a change.
	Recheck bool
}

// Admit returns how long a reconcile about to call Cloudflare with token
// should stay away: 0 if it can go ahead. It returns as soon as it knows, so
// that the reconcile's controller can go on to a resource of another account.
func Admit(token string, a Admission) time.Duration {
	now := time.Now()
	wait, reason := limiter.budget(token, now).admit(a, now)
	if wait > 0 {
		deferredReconciles.WithLabelValues(a.Account, reason).Inc()
	}
	return wait
}

// admit is Admit for one budget at now, and also returns why it put the
// reconcile off.
//
// A reconcile put off comes back at a turn of its own, one per request the
// budget can pay for, so that a backlog costs little CPU and does not all
// return at once when a hold ends. Whether it goes ahead is decided again when
// it returns: a reconcile may make several calls, so not every turn has a
// token left. Changes and rechecks take turns separately, and a recheck goes
// ahead only when no change is waiting: a restart queues every resource, and a
// change made meanwhile must not wait behind them. The priority a workqueue
// gives a change does not do that here, since its items leave the queue at
// once and spend their wait out of it.
func (b *tokenBudget) admit(a Admission, now time.Time) (time.Duration, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.use(a, now)

	held := b.heldUntil.After(now)
	if !held && b.pace.TokensAt(now) >= 1 && (!a.Recheck || !b.nextChange.After(now)) {
		return 0, ""
	}

	next := &b.nextChange
	if a.Recheck {
		next = &b.nextCheck
	}
	turn := later(later(*next, now), b.heldUntil)
	*next = turn.Add(time.Duration(float64(time.Second) / float64(b.pace.Limit())))
	if held {
		return next.Sub(now), deferredByHold
	}
	return next.Sub(now), deferredByBudget
}

// use records that a's account uses the token, and applies the lowest limit
// among the accounts doing so. An account not seen for budgetIdle no longer
// counts, so a deleted one's limit does not outlive it.
func (b *tokenBudget) use(a Admission, now time.Time) {
	b.accounts[a.Account] = accountUse{limit: a.RateLimit, seen: now}
	lowest := 0
	for name, use := range b.accounts {
		if now.Sub(use.seen) > budgetIdle {
			delete(b.accounts, name)
			continue
		}
		if use.limit > 0 && (lowest == 0 || use.limit < lowest) {
			lowest = use.limit
		}
	}
	if lowest == 0 {
		lowest = b.defaultLimit
	}
	if lowest != b.limit {
		b.limit = lowest
		b.pace.SetLimitAt(now, perSecond(lowest))
		b.pace.SetBurstAt(now, burst(lowest))
	}
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// rateLimitedError is what a call fails with, unsent, while its token's budget
// makes it wait longer than maxCallWait.
type rateLimitedError struct {
	wait time.Duration
}

func (e *rateLimitedError) Error() string {
	return fmt.Sprintf("Cloudflare API calls with this token have to wait %s for its rate limit",
		e.wait.Round(time.Second))
}

// acquire waits for a request's turn, or fails it with a *rateLimitedError
// when that is further off than maxCallWait. A long hold turns a request away
// before it touches the bucket, since a turn reserved past the hold's end
// would credit the bucket with what it has yet to refill.
func (b *tokenBudget) acquire(ctx context.Context) error {
	now := time.Now()
	b.mu.Lock()
	start := later(now, b.heldUntil)
	if hold := start.Sub(now); hold > maxCallWait {
		b.mu.Unlock()
		return &rateLimitedError{wait: hold}
	}
	reservation := b.pace.ReserveN(start, 1)
	b.mu.Unlock()

	wait := reservation.DelayFrom(now)
	if wait > maxCallWait {
		reservation.CancelAt(now)
		return &rateLimitedError{wait: wait}
	}
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			reservation.Cancel()
			return ctx.Err()
		}
	}

	// A response may have asked to stop while the request waited.
	now = time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.heldUntil.After(now) {
		return &rateLimitedError{wait: b.heldUntil.Sub(now)}
	}
	return nil
}

// observe holds the budget's calls for as long as res asks: a 429's, or a
// response showing that a quota is nearly gone. Only a 429 is logged by
// default: while other tools spend a quota, holds a second long may follow
// each other for hours.
func (b *tokenBudget) observe(ctx context.Context, res *http.Response, now time.Time) {
	limited := res.StatusCode == http.StatusTooManyRequests
	wait := quotaWait(res.Header)
	if limited {
		wait = retryAfter(res.Header, now)
	}
	if wait <= 0 {
		return
	}

	b.mu.Lock()
	wasHeld := b.heldUntil.After(now)
	b.heldUntil = later(b.heldUntil, now.Add(wait))
	accounts := slices.Sorted(maps.Keys(b.accounts))
	b.mu.Unlock()

	logger := log.FromContext(ctx).V(1)
	if limited {
		logger = log.FromContext(ctx)
		for _, account := range accounts {
			rateLimitedResponses.WithLabelValues(account).Inc()
		}
	}
	if !wasHeld {
		logger.Info("Holding Cloudflare API calls for the token's rate limit",
			"accounts", accounts, "status", res.StatusCode, "wait", wait)
	}
}

// retryAfter returns how long a 429's headers ask to wait: Retry-After's
// seconds or date, else defaultHold, and at most maxHold.
func retryAfter(header http.Header, now time.Time) time.Duration {
	value := header.Get("Retry-After")
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return secondsToWait(seconds)
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return min(date.Sub(now), maxHold)
	}
	return defaultHold
}

// quotaWait returns how long to stop calling because of a response's
// Ratelimit and Ratelimit-Policy headers: the time to the reset of the limit
// furthest off among those with quotaReserve's share or less of their quota
// left, 0 if none is. A limit's quota is its policy's, and a limit without
// one counts as nearly gone only at 0.
//
// Cloudflare sends these on most, not all, responses, with a limit named for
// the operation or "default", for example:
//
//	Ratelimit: "default";r=1199;t=1
//	Ratelimit-Policy: "default";q=1200;w=300
//
// r keeps close to its quota over calls in a row, so a quota refills as it is
// spent, and how close to the end of a window t counts down is not known: a
// hold lasts t seconds, and the next response decides if another follows.
func quotaWait(header http.Header) time.Duration {
	limits := quotaItems(header.Values("Ratelimit"))
	if len(limits) == 0 {
		return 0
	}
	policies := quotaItems(header.Values("Ratelimit-Policy"))
	var wait time.Duration
	for name, limit := range limits {
		remaining, ok := limit["r"]
		if !ok {
			continue
		}
		quota := policies[name]["q"]
		if remaining == 0 || remaining <= quota/quotaReserve {
			wait = max(wait, secondsToWait(limit["t"]))
		}
	}
	return wait
}

// secondsToWait returns seconds as a wait of at least a second and at most
// maxHold.
func secondsToWait(seconds int64) time.Duration {
	return max(time.Second, time.Duration(min(seconds, int64(maxHold/time.Second)))*time.Second)
}

// quotaItems reads the values of a Ratelimit or Ratelimit-Policy header, each
// a comma-separated list of items such as `"default";r=50;t=30`, into each
// item's numbers by its name. What it cannot read is left out: these headers
// only ever slow kflare down, and Cloudflare's 429 stops it anyway.
func quotaItems(values []string) map[string]map[string]int64 {
	items := map[string]map[string]int64{}
	for _, value := range values {
		for item := range strings.SplitSeq(value, ",") {
			name, params, _ := strings.Cut(item, ";")
			name = strings.Trim(strings.TrimSpace(name), `"`)
			numbers := map[string]int64{}
			for param := range strings.SplitSeq(params, ";") {
				key, number, _ := strings.Cut(param, "=")
				if parsed, err := strconv.ParseInt(strings.TrimSpace(number), 10, 64); err == nil && parsed >= 0 {
					numbers[strings.TrimSpace(key)] = parsed
				}
			}
			if name != "" && len(numbers) > 0 {
				items[name] = numbers
			}
		}
	}
	return items
}

// rateLimitedTransport makes each request wait for its token's budget, and
// tells the budget what Cloudflare answered. Both SDKs send through it, so it
// finds the token in the request itself.
type rateLimitedTransport struct {
	next   http.RoundTripper
	limits *rateLimiter
}

// RoundTrip passes a 429 on marked x-should-retry: false, which keeps v7 from
// retrying it: v7 would sleep for all of Retry-After, up to the five minutes
// of a block, inside a reconcile. v0 ignores the mark and retries after a few
// seconds, and the budget's hold fails those retries unsent.
func (t *rateLimitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return t.next.RoundTrip(req)
	}
	budget := t.limits.budget(token, time.Now())
	if err := budget.acquire(req.Context()); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	res, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	budget.observe(req.Context(), res, time.Now())
	if res.StatusCode == http.StatusTooManyRequests {
		res.Header.Set("X-Should-Retry", "false")
	}
	return res, nil
}
