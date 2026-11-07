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
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/discord"
)

// Tests of the attemptOutcome and attemptVerdict values themselves, apart from
// what record does with them.

func TestAttemptVerdict_FoldAndSettlement(t *testing.T) {
	retry := attemptVerdict{retry: true}
	wait := attemptVerdict{retry: true, retryAfter: 9 * time.Second, pending: true}
	shorter := attemptVerdict{retryAfter: 3 * time.Second}
	pending := attemptVerdict{pending: true}

	for _, tc := range []struct {
		name string
		a, b attemptVerdict
		want attemptVerdict
	}{
		{name: "zero folds to zero", want: attemptVerdict{}},
		{name: "a retry survives", a: retry, want: retry},
		{name: "the longest wait wins", a: wait, b: shorter, want: wait},
		{name: "the longest wait wins either way round", a: shorter, b: wait, want: wait},
		{name: "pending and retry combine", a: pending, b: retry, want: attemptVerdict{retry: true, pending: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.fold(tc.b); got != tc.want {
				t.Fatalf("fold = %+v, want %+v", got, tc.want)
			}
		})
	}

	if got := (attemptVerdict{retry: true, retryAfter: time.Second}).settlement(); got != syncAccountedFor {
		t.Errorf("settlement of a verdict with nothing pending = %v, want accountedFor", got)
	}
	if got := pending.settlement(); got != syncPending {
		t.Errorf("settlement of a pending verdict = %v, want pending", got)
	}
}

// Every kind either counts under exactly one outcome label the dashboard and
// alerts know about (see TestObservabilityContract) or under none; a kind
// added without being handled by record panics instead of going unrecorded.
func TestAttemptOutcome_MetricLabels(t *testing.T) {
	uncounted := map[attemptKind]bool{attemptFetchSucceeded: true, attemptBacklogSkipped: true, attemptConfigError: true}
	known := expectedOutcomes()
	for kind := attemptFetchSucceeded; kind < attemptKindEnd; kind++ {
		for _, class := range allFailureClasses {
			label := attemptOutcome{kind: kind}.metricLabel(class)
			switch {
			case uncounted[kind] && label != "":
				t.Fatalf("kind %d counts under %q, want none", kind, label)
			case !uncounted[kind] && !slices.Contains(known, label):
				t.Fatalf("kind %d counts under %q, which expectedOutcomes does not know", kind, label)
			}
		}
	}

	defer func() {
		if recover() == nil {
			t.Fatal("recording an unknown kind did not panic")
		}
	}()
	fg := &v1beta1.FeedGroup{}
	testAttempt(t, fg, &v1beta1.FeedStatus{}, attemptURL, attemptInstant).
		record(attemptOutcome{kind: attemptKindEnd}, nil)
}

func TestAttemptOutcomeResolve(t *testing.T) {
	rateLimited := &discord.RateLimitError{RetryAfter: time.Second}
	for _, tc := range []struct {
		name      string
		err       error
		wantKind  attemptKind
		wantClass failureClass
	}{
		{name: "rate limit", err: rateLimited, wantKind: attemptRateLimited, wantClass: classRateLimited},
		{name: "wrapped rate limit", err: fmt.Errorf("sending: %w", rateLimited), wantKind: attemptRateLimited, wantClass: classRateLimited},
		{name: "a 429 status is a counted failure", err: &discord.HTTPStatusError{StatusCode: 429}, wantKind: attemptSendFailed, wantClass: classRateLimited},
		{name: "deleted webhook", err: &discord.HTTPStatusError{StatusCode: 404}, wantKind: attemptSendFailed, wantClass: classWebhookInvalid},
		{name: "server error", err: &discord.HTTPStatusError{StatusCode: 502}, wantKind: attemptSendFailed, wantClass: classServerError},
		{name: "anything else", err: errors.New(testBoom), wantKind: attemptSendFailed, wantClass: classOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome, class := attemptOutcome{kind: attemptSendFailed}.resolve(tc.err)
			if outcome.kind != tc.wantKind || class != tc.wantClass {
				t.Fatalf("resolve = (%d, %+v), want (%d, %+v)", outcome.kind, class, tc.wantKind, tc.wantClass)
			}
		})
	}
}
