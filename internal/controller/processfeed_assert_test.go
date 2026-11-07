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
	"strings"
	"testing"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/tools/events"
)

// Assertions shared by the processFeed tests. Each takes the one fact it
// checks, so a test reads as a sequence of expectations rather than nested
// conditionals.

// nextEvent returns the next Event the recorder holds without blocking.
func nextEvent(recorder *events.FakeRecorder) (string, bool) {
	select {
	case ev := <-recorder.Events:
		return ev, true
	default:
		return "", false
	}
}

// discardEvents drops every Event the recorder holds.
func discardEvents(recorder *events.FakeRecorder) {
	for range len(recorder.Events) {
		<-recorder.Events
	}
}

// assertEventReason fails unless the recorder holds an Event naming reason.
func assertEventReason(t *testing.T, recorder *events.FakeRecorder, reason string) {
	t.Helper()
	ev, ok := nextEvent(recorder)
	if !ok {
		t.Fatalf("expected an Event with reason %s, got none", reason)
	}
	if !strings.Contains(ev, reason) {
		t.Fatalf("event = %q, want reason %s", ev, reason)
	}
}

// assertNoEvent fails if the recorder holds any Event.
func assertNoEvent(t *testing.T, recorder *events.FakeRecorder) {
	t.Helper()
	if ev, ok := nextEvent(recorder); ok {
		t.Fatalf("unexpected event: %s", ev)
	}
}

// assertNoSkipEvent fails if the recorder holds a SkippedEntry Event.
func assertNoSkipEvent(t *testing.T, recorder *events.FakeRecorder) {
	t.Helper()
	for range len(recorder.Events) {
		if ev := <-recorder.Events; strings.Contains(ev, reasonEntrySkipped) {
			t.Errorf("unexpected skip event: %s", ev)
		}
	}
}

// assertRetry fails unless the verdict asks for a retry exactly when want.
func assertRetry(t *testing.T, verdict attemptVerdict, want bool) {
	t.Helper()
	if verdict.retry != want {
		t.Fatalf("verdict.retry = %t, want %t", verdict.retry, want)
	}
}

// assertQuiet fails if the verdict asks for a retry or a rate-limit wait.
func assertQuiet(t *testing.T, verdict attemptVerdict) {
	t.Helper()
	if verdict.retry {
		t.Fatalf("unexpected retry (retryAfter=%v)", verdict.retryAfter)
	}
	if verdict.retryAfter != 0 {
		t.Fatalf("unexpected rate-limit wait %v", verdict.retryAfter)
	}
}

// assertRetryCount fails unless fs counts want consecutive failures.
func assertRetryCount(t *testing.T, fs *v1beta1.FeedStatus, want int32) {
	t.Helper()
	if fs.RetryCount != want {
		t.Fatalf("RetryCount = %d, want %d", fs.RetryCount, want)
	}
}

// feedState is the part of a FeedStatus a delivery outcome settles.
type feedState struct {
	lastSeen  string
	retries   int32
	lastError string
}

// assertFeedState fails unless fs has exactly the want watermark, retry count
// and last error.
func assertFeedState(t *testing.T, fs *v1beta1.FeedStatus, want feedState) {
	t.Helper()
	if got := (feedState{fs.LastSeenEntry, fs.RetryCount, fs.LastError}); got != want {
		t.Errorf("status = %+v, want %+v (full status %+v)", got, want, fs)
	}
}

// assertHeld fails if fs marked any entry handled or advanced its watermark,
// or if sent messages went out: the failing entry must stay pending.
func assertHeld(t *testing.T, fs *v1beta1.FeedStatus, sent int) {
	t.Helper()
	if len(fs.LastSent) != 0 || fs.LastSeenEntry != "" {
		t.Errorf("an entry was marked handled: LastSent=%v seen=%q", fs.LastSent, fs.LastSeenEntry)
	}
	if sent != 0 {
		t.Errorf("sent %d messages, want none while the entry is held", sent)
	}
}

// assertDeliveredReason fails unless fs's Delivered condition carries reason.
func assertDeliveredReason(t *testing.T, fs *v1beta1.FeedStatus, reason string) {
	t.Helper()
	delivered := apimeta.FindStatusCondition(fs.Conditions, v1beta1.FeedConditionTypeDelivered)
	if delivered == nil {
		t.Fatalf("no Delivered condition, want Reason %s", reason)
	}
	if delivered.Reason != reason {
		t.Fatalf("Delivered condition = %v, want Reason %s", delivered, reason)
	}
}
