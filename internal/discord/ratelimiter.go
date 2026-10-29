package discord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"sync"
	"time"
)

// RateLimiter tracks per-webhook cooldowns so that a 429 seen by one
// FeedGroup's Client also holds off every other Client sharing the same
// webhook -- and, once --max-concurrent-feedgroup-reconciles > 1, concurrent
// sends from different FeedGroups against the same webhook. Without this,
// rate-limit backoff is purely per-reconcile: one FeedGroup backs off while
// another sharing the webhook (or racing it) fires anyway and gets 429'd in
// turn.
//
// A single RateLimiter must be constructed once and shared across every
// Client the process builds (see NewClientWithLimiter and cmd/main.go, which
// already shares one *http.Client the same way for connection pooling).
type RateLimiter struct {
	mu        sync.Mutex
	cooldowns map[string]time.Time
	// pacing holds short, advisory waits learned from rate-limit headers on
	// successful responses (the webhook's bucket is empty until the reset
	// time). Unlike cooldowns, callers sleep through these instead of
	// failing, so a burst of sends is paced rather than 429'd.
	pacing map[string]time.Time
}

// NewRateLimiter returns an empty RateLimiter, ready to share across Clients.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{cooldowns: make(map[string]time.Time), pacing: make(map[string]time.Time)}
}

// webhookKey returns the map key for webhookURL. It's a SHA-256 hash rather
// than the raw URL because the URL contains the webhook token, and this key
// can end up in the same places RateLimitError already avoids leaking the
// token into (nothing here logs or returns the key today, but hashing keeps
// that guarantee cheap to preserve).
func webhookKey(webhookURL string) string {
	sum := sha256.Sum256([]byte(webhookURL))
	return hex.EncodeToString(sum[:])
}

// reserve reports whether webhookURL is currently in a rate-limit cooldown
// and, if so, how much longer it will last. Callers in cooldown must not
// send; callers not in cooldown may proceed immediately.
//
// Every call also prunes any cooldowns that have already expired, across the
// whole map, not just webhookURL's entry -- so a webhook that gets one 429
// and is then never sent to again (e.g. its FeedGroup is deleted) doesn't
// leave its entry in the map forever.
func (l *RateLimiter) reserve(webhookURL string) (remaining time.Duration, cooling bool) {
	key := webhookKey(webhookURL)
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	pruneExpired(l.cooldowns, now)

	if until, ok := l.cooldowns[key]; ok {
		if remaining := until.Sub(now); remaining > 0 {
			return remaining, true
		}
	}
	return 0, false
}

// pruneExpired drops every entry in m whose deadline has passed.
func pruneExpired(m map[string]time.Time, now time.Time) {
	maps.DeleteFunc(m, func(_ string, until time.Time) bool { return !until.After(now) })
}

// cooldown records that webhookURL must not be sent to again until
// retryAfter has elapsed.
func (l *RateLimiter) cooldown(webhookURL string, retryAfter time.Duration) {
	key := webhookKey(webhookURL)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.cooldowns[key] = time.Now().Add(retryAfter)
}

// pace records that webhookURL's rate-limit bucket is empty until d from now,
// so the next send should wait that long rather than be rejected with a 429.
func (l *RateLimiter) pace(webhookURL string, d time.Duration) {
	key := webhookKey(webhookURL)

	l.mu.Lock()
	defer l.mu.Unlock()
	if until := time.Now().Add(d); until.After(l.pacing[key]) {
		l.pacing[key] = until
	}
}

// paceDelay reports how long a sender must wait before the webhook's bucket
// refills, pruning expired entries across the map as reserve does.
func (l *RateLimiter) paceDelay(webhookURL string) time.Duration {
	key := webhookKey(webhookURL)
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	pruneExpired(l.pacing, now)
	return l.pacing[key].Sub(now)
}

// wait sleeps out any advisory pacing delay for webhookURL (see pace), or
// returns ctx's error if the context ends first. Delays longer than
// maxPaceWait are never recorded as pacing (they become hard cooldowns), so
// this never blocks a reconcile for long.
func (l *RateLimiter) wait(ctx context.Context, webhookURL string) error {
	d := l.paceDelay(webhookURL)
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
