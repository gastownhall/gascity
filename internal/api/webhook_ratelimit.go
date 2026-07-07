package api

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// webhookRateLimiterMaxBuckets caps distinct per-webhook buckets so the limiter
// cannot grow unbounded. In practice a city has a handful of webhooks; the cap is
// a safety net that evicts the least-recently-used bucket if ever exceeded.
const webhookRateLimiterMaxBuckets = 4096

// webhookRateLimiter is the E8 per-webhook token-bucket limiter: bounded,
// in-memory, keyed by webhook name. The sustained rate (perMinute) and burst are
// supplied per call from config.WebhookPolicyConfig.EffectiveRateLimit, so a
// config reload takes effect on the next delivery and — crucially — a pack can
// never widen a limit it is subject to (the operator owns the ceiling; a pack's
// MaxPerMinute is clamped before it reaches here).
type webhookRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	max     int
	// now is an injectable clock for tests; nil uses time.Now.
	now func() time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newWebhookRateLimiter() *webhookRateLimiter {
	return &webhookRateLimiter{
		buckets: make(map[string]*tokenBucket),
		max:     webhookRateLimiterMaxBuckets,
	}
}

func (l *webhookRateLimiter) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// allow consumes one token for name at the given sustained rate and burst. It
// returns whether the delivery is permitted and, when not, the recommended
// Retry-After wait. A non-positive perMinute fails closed (throughput disabled).
func (l *webhookRateLimiter) allow(name string, perMinute, burst int) (bool, time.Duration) {
	if perMinute <= 0 {
		// The operator (or the clamp) set this webhook to zero sustained
		// throughput; refuse every delivery rather than divide by zero.
		return false, time.Minute
	}
	if burst < 1 {
		burst = 1
	}
	ratePerSec := float64(perMinute) / 60.0
	capacity := float64(burst)

	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	b, ok := l.buckets[name]
	if !ok {
		l.evictIfFullLocked()
		b = &tokenBucket{tokens: capacity, last: now}
		l.buckets[name] = b
	}
	// Refill by the elapsed time, then clamp to the (possibly reloaded) capacity.
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * ratePerSec
		b.last = now
	}
	if b.tokens > capacity {
		b.tokens = capacity
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// Under one token: time until the next token accrues.
	wait := time.Duration((1 - b.tokens) / ratePerSec * float64(time.Second))
	return false, wait
}

// evictIfFullLocked drops the least-recently-used bucket when at capacity. Must
// hold l.mu.
func (l *webhookRateLimiter) evictIfFullLocked() {
	if len(l.buckets) < l.max {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, b := range l.buckets {
		if oldestKey == "" || b.last.Before(oldest) {
			oldestKey = k
			oldest = b.last
		}
	}
	if oldestKey != "" {
		delete(l.buckets, oldestKey)
	}
}

// setRetryAfter writes an integer-second Retry-After header (RFC 9110), rounding
// up and flooring at 1s so the sender always backs off at least a second. It must
// be called before the status line is written.
func setRetryAfter(w http.ResponseWriter, d time.Duration) {
	secs := int(math.Ceil(d.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
}
