/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"
	"strings"
	"time"

	"github.com/maverickd650-labs/rss2discord/api/v1alpha1"
)

// minParsedInterval floors any interval/retryInterval a cadence parses. The
// CRD's Pattern rejects a negative literal, but a tiny positive one ("1ns")
// still validates and, fed straight into ctrl.Result{RequeueAfter: ...}, would
// requeue in a tight loop that hammers every feed host on each pass; this floor
// is the backstop for that, and also for objects admitted under an older, less
// strict schema.
const minParsedInterval = 10 * time.Second

// fallbackInterval and fallbackRetryInterval are the waits used when the spec's
// Interval/RetryInterval is empty or unparsable. A cadence is the single source
// of these fallbacks for polling, requeueing and permanent-failure backoff.
const (
	fallbackInterval      = 30 * time.Minute
	fallbackRetryInterval = 5 * time.Minute
)

// cadence is a FeedGroup's timing, resolved once from its spec: how often to
// poll, how long to wait before retrying a failure, and how many retries a
// transient failure gets. Everything that schedules, requeues or backs off
// reads it, so "an unparsable duration falls back to the default" holds in one
// place instead of at each caller, and the parse error is kept (err) for the
// Ready condition rather than dropped.
type cadence struct {
	// poll is the wait between successful checks: spec.interval floored at
	// minParsedInterval, or fallbackInterval when empty or unparsable.
	poll time.Duration
	// retry is the wait after a failure, and the base of a permanent failure's
	// backoff: spec.retryInterval floored at minParsedInterval, or
	// fallbackRetryInterval when empty or unparsable.
	retry time.Duration
	// maxRetries is spec.retries with a minimum of one.
	maxRetries int32
	// err is the first of spec.interval and spec.retryInterval that could not
	// be parsed (interval first), or nil. Both fields are always checked, so
	// the Ready condition doesn't flap between the normal and retry paths (and
	// re-fire its Event each time) while only one of them is bad. The CRD
	// Pattern admits values like "9999999999999h" that overflow
	// time.ParseDuration, and objects admitted under an older schema may hold
	// others.
	err error
}

// cadenceOf resolves spec's timing. A blank (whitespace-only) duration takes
// its fallback without error; an unparsable one takes its fallback and sets err;
// a parsed one is floored at minParsedInterval (the fallbacks are not floored).
func cadenceOf(spec *v1alpha1.FeedGroupSpec) cadence {
	c := cadence{maxRetries: max(spec.Retries, 1)}
	var intervalErr, retryErr error
	c.poll, intervalErr = parseDurationWithDefault(spec.Interval, fallbackInterval)
	c.retry, retryErr = parseDurationWithDefault(spec.RetryInterval, fallbackRetryInterval)
	switch {
	case intervalErr != nil:
		c.err = fmt.Errorf("spec.interval: %w; using the default instead", intervalErr)
	case retryErr != nil:
		c.err = fmt.Errorf("spec.retryInterval: %w; using the default instead", retryErr)
	}
	return c
}

// parseDurationWithDefault parses value as a Go duration and floors parsed
// values at minParsedInterval. Whitespace-only input returns fallback without
// error; invalid input returns fallback and a wrapped parse error. The fallback
// is returned unchanged, without applying the floor.
func parseDurationWithDefault(value string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}

	duration, err := time.ParseDuration(value)
	if err != nil {
		return fallback, fmt.Errorf("invalid duration %q: %w", value, err)
	}

	return max(duration, minParsedInterval), nil
}
