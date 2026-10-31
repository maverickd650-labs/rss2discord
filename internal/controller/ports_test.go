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
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/discord"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// fakeSender is a MessageSender that replays scripted results, one per call,
// repeating the last once the script runs out.
type fakeSender struct {
	results []error
	sent    []discord.Message
}

func (f *fakeSender) SendMessage(_ context.Context, msg discord.Message) error {
	f.sent = append(f.sent, msg)
	i := len(f.sent) - 1
	if len(f.results) == 0 {
		return nil
	}
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	return f.results[i]
}

// fakeFetcher is a FeedFetcher that returns a fixed result and records what it
// was asked for.
type fakeFetcher struct {
	result     rss.FetchResult
	err        error
	gotURL     string
	gotCache   rss.CacheValidators
	fetchCalls int
}

func (f *fakeFetcher) FetchEntries(_ context.Context, url string, validators rss.CacheValidators) (rss.FetchResult, error) {
	f.fetchCalls++
	f.gotURL, f.gotCache = url, validators
	return f.result, f.err
}

func TestTimedSender_ObservesOncePerCallAndPassesErrorsThrough(t *testing.T) {
	boom := errors.New(testBoom)
	rateLimited := &discord.RateLimitError{RetryAfter: time.Second}

	cases := []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "error", err: boom},
		{name: "throttled", err: rateLimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, name := "ports-timed-sender-"+tc.name, "fg-timed-sender-"+tc.name
			forgetMetricsAfter(t, ns, name)

			inner := &fakeSender{results: []error{tc.err}}
			sender := newTimedSender(inner, ns, name)

			err := sender.SendMessage(t.Context(), discord.Message{Content: "hello"})
			if !errors.Is(err, tc.err) {
				t.Fatalf("SendMessage error = %v, want %v", err, tc.err)
			}
			if len(inner.sent) != 1 || inner.sent[0].Content != "hello" {
				t.Fatalf("inner received %+v, want the one message passed in", inner.sent)
			}
			if got := histogramSampleCount(t, ns, name, operationSend); got != 1 {
				t.Fatalf("send sample count = %d, want 1", got)
			}
			if got := histogramSampleCount(t, ns, name, operationFetch); got != 0 {
				t.Fatalf("fetch sample count = %d, want 0", got)
			}

			_ = sender.SendMessage(t.Context(), discord.Message{})
			if got := histogramSampleCount(t, ns, name, operationSend); got != 2 {
				t.Fatalf("send sample count after a second call = %d, want 2", got)
			}
		})
	}
}

func TestTimedFetcher_ObservesOncePerCallAndPassesResultsThrough(t *testing.T) {
	boom := errors.New(testBoom)
	cases := []struct {
		name   string
		result rss.FetchResult
		err    error
	}{
		{name: "success", result: oneEntryFetch()},
		{name: "not_modified", result: rss.FetchResult{NotModified: true}},
		{name: "error", err: boom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, name := "ports-timed-fetcher-"+tc.name, "fg-timed-fetcher-"+tc.name
			forgetMetricsAfter(t, ns, name)

			inner := &fakeFetcher{result: tc.result, err: tc.err}
			fetcher := newTimedFetcher(inner, ns, name)
			validators := rss.CacheValidators{ETag: "etag", LastModified: "lm"}

			got, err := fetcher.FetchEntries(t.Context(), exampleFeedURL, validators)
			if !errors.Is(err, tc.err) {
				t.Fatalf("FetchEntries error = %v, want %v", err, tc.err)
			}
			if got.NotModified != tc.result.NotModified || len(got.Entries) != len(tc.result.Entries) {
				t.Fatalf("FetchEntries result = %+v, want %+v", got, tc.result)
			}
			if inner.gotURL != exampleFeedURL || inner.gotCache != validators {
				t.Fatalf("inner called with (%q, %+v), want the arguments passed in", inner.gotURL, inner.gotCache)
			}
			if got := histogramSampleCount(t, ns, name, operationFetch); got != 1 {
				t.Fatalf("fetch sample count = %d, want 1", got)
			}
			if got := histogramSampleCount(t, ns, name, operationSend); got != 0 {
				t.Fatalf("send sample count = %d, want 0", got)
			}
		})
	}
}

// TestProcessFeed_FakeSenderRateLimited asserts a 429 from the sender yields a
// verdict carrying the Retry-After and leaves the entry unsent.
func TestProcessFeed_FakeSenderRateLimited(t *testing.T) {
	ns, name := "ports-fake-429", "fg-fake-429"

	fg, feed := newMetricsFeedGroup(ns, name, "")
	sender := &fakeSender{results: []error{&discord.RateLimitError{RetryAfter: 7 * time.Second}}}

	verdict := processFeed(t.Context(), testRun(t, fg, feed, oneEntryFetch(), nil, sender))

	if verdict.retryAfter != 7*time.Second {
		t.Fatalf("verdict.retryAfter = %v, want 7s", verdict.retryAfter)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sender called %d times, want 1", len(sender.sent))
	}
	if got := feedStatusFor(fg, feed.RSSUrl).LastSent; len(got) != 0 {
		t.Fatalf("LastSent = %v, want the entry left unsent", got)
	}
	if got := testutil.ToFloat64(feedOperationsTotal.WithLabelValues(ns, name, exampleFeedURL, outcomeRateLimited)); got != 1 {
		t.Fatalf("feedOperationsTotal{outcome=%q} = %v, want 1", outcomeRateLimited, got)
	}
}

// TestProcessFeed_FakeSenderServerError asserts a 5xx counts a retry, asks for
// one, and records the classified send_error outcome.
func TestProcessFeed_FakeSenderServerError(t *testing.T) {
	ns, name := "ports-fake-5xx", "fg-fake-5xx"

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Spec.Retries = 3
	sender := &fakeSender{results: []error{&discord.HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}}}

	verdict := processFeed(t.Context(), testRun(t, fg, feed, oneEntryFetch(), nil, sender))

	if !verdict.retry {
		t.Fatal("verdict.retry = false, want a transient send failure to ask for a retry")
	}
	fs := feedStatusFor(fg, feed.RSSUrl)
	if fs.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1", fs.RetryCount)
	}
	if fs.BackoffUntil != "" {
		t.Fatalf("BackoffUntil = %q, want none for a transient failure", fs.BackoffUntil)
	}
	if len(fs.LastSent) != 0 {
		t.Fatalf("LastSent = %v, want the entry left unsent", fs.LastSent)
	}
	if got := testutil.ToFloat64(feedOperationsTotal.WithLabelValues(ns, name, exampleFeedURL, sendErrorOutcome(classServerError))); got != 1 {
		t.Fatalf("feedOperationsTotal{outcome=send_error_server_error} = %v, want 1", got)
	}
}

// TestProcessFeed_FakeSenderPermanentClientError asserts a permanent 4xx backs
// the feed off at once and does not ask for a prompt retry.
func TestProcessFeed_FakeSenderPermanentClientError(t *testing.T) {
	ns, name := "ports-fake-4xx", "fg-fake-4xx"

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Spec.RetryInterval = "5m"
	sender := &fakeSender{results: []error{&discord.HTTPStatusError{StatusCode: 400, Status: "400 Bad Request"}}}

	verdict := processFeed(t.Context(), testRun(t, fg, feed, oneEntryFetch(), nil, sender))

	if verdict.retry {
		t.Fatal("verdict.retry = true, want a permanent failure to back off instead")
	}
	fs := feedStatusFor(fg, feed.RSSUrl)
	if fs.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1", fs.RetryCount)
	}
	// RetryCount 1 backs off 5m * 2^1 = 10m from the run's clock.
	if want := nextBackoffUntil(testReconcileInstant, 10*time.Minute); fs.BackoffUntil != want {
		t.Fatalf("BackoffUntil = %q, want %q", fs.BackoffUntil, want)
	}
	if got := testutil.ToFloat64(feedOperationsTotal.WithLabelValues(ns, name, exampleFeedURL, sendErrorOutcome(classClientError))); got != 1 {
		t.Fatalf("feedOperationsTotal{outcome=send_error_client_error} = %v, want 1", got)
	}
}

// TestProcessFeed_FakeSenderSuccess asserts a nil send marks the entry sent.
func TestProcessFeed_FakeSenderSuccess(t *testing.T) {
	ns, name := "ports-fake-ok", "fg-fake-ok"

	fg, feed := newMetricsFeedGroup(ns, name, "")
	sender := &fakeSender{}

	verdict := processFeed(t.Context(), testRun(t, fg, feed, oneEntryFetch(), nil, sender))

	if verdict.retry || verdict.retryAfter != 0 {
		t.Fatalf("verdict = %+v, want none", verdict)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sender called %d times, want 1", len(sender.sent))
	}
	if got := testutil.ToFloat64(feedOperationsTotal.WithLabelValues(ns, name, exampleFeedURL, outcomeSent)); got != 1 {
		t.Fatalf("feedOperationsTotal{outcome=sent} = %v, want 1", got)
	}
}

// concurrencyFetcher is a FeedFetcher that returns a result whose title
// identifies the requested URL. Its calls block until the test closes release,
// and the call for last also waits until every other call has returned, so
// overlap and completion order are forced rather than left to the scheduler.
// filled is closed once full calls are in flight together. A wait that is not
// satisfied within barrierTimeout records a failure instead of hanging.
type concurrencyFetcher struct {
	full int
	last string
	rest int // calls other than last

	mu        sync.Mutex
	inFlight  int
	peak      int
	calls     int
	returned  int
	timedOut  bool
	wasFilled bool
	errFor    map[string]error

	filled   chan struct{}
	release  chan struct{}
	restDone chan struct{}
}

const barrierTimeout = 10 * time.Second

func newConcurrencyFetcher(full int, last string, rest int, errFor map[string]error) *concurrencyFetcher {
	return &concurrencyFetcher{
		full: full, last: last, rest: rest, errFor: errFor,
		filled: make(chan struct{}), release: make(chan struct{}), restDone: make(chan struct{}),
	}
}

// wait blocks on ch and reports whether it was closed before barrierTimeout.
func (f *concurrencyFetcher) wait(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(barrierTimeout):
		f.mu.Lock()
		f.timedOut = true
		f.mu.Unlock()
		return false
	}
}

func (f *concurrencyFetcher) FetchEntries(_ context.Context, url string, _ rss.CacheValidators) (rss.FetchResult, error) {
	f.mu.Lock()
	f.inFlight++
	f.calls++
	f.peak = max(f.peak, f.inFlight)
	if f.inFlight == f.full && !f.wasFilled {
		f.wasFilled = true
		close(f.filled)
	}
	f.mu.Unlock()

	f.wait(f.release)
	if url == f.last {
		f.wait(f.restDone)
	}

	f.mu.Lock()
	f.inFlight--
	if url != f.last {
		if f.returned++; f.returned == f.rest {
			close(f.restDone)
		}
	}
	f.mu.Unlock()
	return rss.FetchResult{FeedTitle: url}, f.errFor[url]
}

func TestFetchAll_InputOrderAndBoundedConcurrency(t *testing.T) {
	const n = maxConcurrentFetches*3 + 1
	fg := &v1alpha1.FeedGroup{}
	feeds := make([]v1alpha1.FeedSpec, n)
	for i := range feeds {
		feeds[i].RSSUrl = fmt.Sprintf("https://example.com/feed-%d.xml", i)
	}
	fg.Spec.Feeds = feeds
	ensureFeedStatuses(fg)
	errFor := map[string]error{feeds[3].RSSUrl: errors.New(testBoom)}
	// Feed 0 completes last, so outcomes in completion order would put it at
	// the end; fetchAll must still return it first.
	f := newConcurrencyFetcher(maxConcurrentFetches, feeds[0].RSSUrl, n-1, errFor)

	done := make(chan []fetchOutcome, 1)
	go func() { done <- fetchAll(t.Context(), f, fg, feeds) }()

	// With maxConcurrentFetches calls held, a bounded fan-out cannot start
	// another; an unbounded one would within the grace period. A correct
	// bound never fails this, so the grace only affects how reliably a
	// regression is caught, not whether the test flakes.
	filled := f.wait(f.filled)
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	started := f.calls
	f.mu.Unlock()
	close(f.release)
	got := <-done

	if !filled || f.timedOut {
		t.Fatalf("a barrier timed out: fewer than %d fetches ran at once (peak %d)", maxConcurrentFetches, f.peak)
	}
	if started != maxConcurrentFetches {
		t.Fatalf("%d fetches started while %d were in flight, want the bound to hold at %d", started, maxConcurrentFetches, maxConcurrentFetches)
	}
	if len(got) != n || f.calls != n {
		t.Fatalf("got %d outcomes from %d calls, want %d of each", len(got), f.calls, n)
	}
	for i, o := range got {
		if o.result.FeedTitle != feeds[i].RSSUrl {
			t.Fatalf("outcome %d is for %q, want %q (input order)", i, o.result.FeedTitle, feeds[i].RSSUrl)
		}
		if (o.err != nil) != (i == 3) {
			t.Fatalf("outcome %d err = %v, want error only at index 3", i, o.err)
		}
	}
	if f.peak != maxConcurrentFetches {
		t.Fatalf("peak concurrency = %d, want exactly %d", f.peak, maxConcurrentFetches)
	}
}

func TestFetchAll_NoFeeds(t *testing.T) {
	if got := fetchAll(t.Context(), &fakeFetcher{}, &v1alpha1.FeedGroup{}, nil); len(got) != 0 {
		t.Fatalf("got %d outcomes, want none", len(got))
	}
}
