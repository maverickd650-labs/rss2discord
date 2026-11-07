package controller

import (
	"errors"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
)

// Tests of groupSync: opening one (openGroupSync) and settling a reconcile
// into the group's status (settle.go).

func outOfRangeFeedStatus(url string) v1beta1.FeedStatus {
	return v1beta1.FeedStatus{
		Name: testFeedName(url), URL: url,
		Conditions: []metav1.Condition{{
			Type:               v1beta1.FeedConditionTypeReachable,
			Status:             metav1.ConditionTrue,
			Reason:             "TimeOutOfRange",
			LastTransitionTime: metav1.NewTime(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)),
		}},
	}
}

// requireReady fails the test unless the group's Ready condition has the
// given status and reason.
func requireReady(t *testing.T, fg *v1beta1.FeedGroup, status metav1.ConditionStatus, reason string) {
	t.Helper()
	ready := apimeta.FindStatusCondition(fg.Status.Conditions, v1beta1.ConditionTypeReady)
	if ready == nil || ready.Status != status || ready.Reason != reason {
		t.Fatalf("Ready condition = %+v, want %s with reason %q", ready, status, reason)
	}
}

func requireObservedGeneration(t *testing.T, fg *v1beta1.FeedGroup, want int64) {
	t.Helper()
	if fg.Status.ObservedGeneration != want {
		t.Errorf("ObservedGeneration = %d, want %d", fg.Status.ObservedGeneration, want)
	}
}

// ---- openGroupSync ----

const (
	openSyncNS   = "open-sync-ns"
	openSyncName = "open-sync"

	openSyncRetainedURL = "https://example.com/retained.xml"
	openSyncPrunedURL   = "https://example.com/pruned.xml"
	openSyncNewURL      = "https://example.com/new.xml"
)

var openSyncBackoffUntil = metav1TimeRFC3339("2026-11-04T15:00:00Z")

// newOpenSyncGroup builds a group whose status retains one feed (with retry
// state), carries one feed no longer in spec, and lacks one newly in spec.
func newOpenSyncGroup() *v1beta1.FeedGroup {
	fg := &v1beta1.FeedGroup{
		Spec: v1beta1.FeedGroupSpec{
			Interval: "15m",
			Feeds: []v1beta1.FeedSpec{
				{Name: testFeedName(openSyncRetainedURL), URL: openSyncRetainedURL},
				{Name: testFeedName(openSyncNewURL), URL: openSyncNewURL},
			},
		},
		Status: v1beta1.FeedGroupStatus{
			Feeds: []v1beta1.FeedStatus{
				{Name: testFeedName(openSyncRetainedURL), URL: openSyncRetainedURL, LastError: "404", RetryCount: 3, BackoffUntil: openSyncBackoffUntil},
				{Name: testFeedName(openSyncPrunedURL), URL: openSyncPrunedURL, LastError: "500", RetryCount: 1},
			},
		},
	}
	fg.Namespace = openSyncNS
	fg.Name = openSyncName
	return fg
}

func TestOpenGroupSync_SpecChanged(t *testing.T) {
	forgetMetricsAfter(t, openSyncNS, openSyncName)
	fg := newOpenSyncGroup()

	// Populate metrics for the pruned feed so we can verify forgetFeed was called.
	metricsFor(fg).syncFeedState(fg)
	if _, ok := feedGaugeValue(feedHealthy, openSyncNS, openSyncName, testFeedName(openSyncPrunedURL)); !ok {
		t.Fatalf("expected feedHealthy metric to exist for %q before pruning", openSyncPrunedURL)
	}

	r := &FeedGroupReconciler{}
	fg.Generation = 2
	fg.Status.ObservedGeneration = 1

	sync := r.openGroupSync(fg)

	t.Run("records the group and cadence", func(t *testing.T) {
		if !sync.specChanged {
			t.Error("expected specChanged to be true when Generation > ObservedGeneration")
		}
		if sync.reconciler != r {
			t.Errorf("sync.reconciler = %v, want %v", sync.reconciler, r)
		}
		if sync.group != fg {
			t.Errorf("sync.group = %v, want %v", sync.group, fg)
		}
		if sync.cadence.poll != 15*time.Minute {
			t.Errorf("sync.cadence.poll = %v, want 15m", sync.cadence.poll)
		}
	})
	t.Run("prunes a dropped feed and forgets its metrics", func(t *testing.T) {
		if got := feedStatusFor(fg, testFeedName(openSyncPrunedURL)); got != nil {
			t.Errorf("expected pruned feed %q to be removed from Status.Feeds", openSyncPrunedURL)
		}
		if _, ok := feedGaugeValue(feedHealthy, openSyncNS, openSyncName, testFeedName(openSyncPrunedURL)); ok {
			t.Errorf("expected feedHealthy metric for %q to be forgotten after openGroupSync", openSyncPrunedURL)
		}
	})
	t.Run("adds a new feed", func(t *testing.T) {
		if got := feedStatusFor(fg, testFeedName(openSyncNewURL)); got == nil {
			t.Errorf("expected new feed %q to be added to Status.Feeds", openSyncNewURL)
		}
	})
	t.Run("clears backoff and snapshots status", func(t *testing.T) {
		retained := feedStatusFor(fg, testFeedName(openSyncRetainedURL))
		if retained == nil {
			t.Fatalf("expected retained feed %q to exist", openSyncRetainedURL)
		}
		if retained.BackoffUntil != nil || retained.RetryCount != 0 {
			t.Errorf("expected backoff cleared on spec change, got BackoffUntil=%v RetryCount=%d", retained.BackoffUntil, retained.RetryCount)
		}
		if sync.original == &fg.Status {
			t.Error("sync.original aliases fg.Status")
		}
		retained.RetryCount = 42
		if sync.original.Feeds[0].RetryCount == 42 {
			t.Error("mutating fg.Status mutated sync.original")
		}
	})
}

func TestOpenGroupSync_SpecUnchangedKeepsBackoff(t *testing.T) {
	forgetMetricsAfter(t, openSyncNS, openSyncName)
	fg := newOpenSyncGroup()
	fg.Generation = 2
	fg.Status.ObservedGeneration = 2
	fg.Status.Feeds[0].RetryCount = 5

	sync := (&FeedGroupReconciler{}).openGroupSync(fg)

	if sync.specChanged {
		t.Error("expected specChanged to be false when Generation == ObservedGeneration")
	}
	retained := feedStatusFor(fg, testFeedName(openSyncRetainedURL))
	if retained == nil {
		t.Fatalf("expected retained feed %q to exist after unchanged-generation openGroupSync", openSyncRetainedURL)
	}
	if !retained.BackoffUntil.Equal(openSyncBackoffUntil) || retained.RetryCount != 5 {
		t.Errorf("expected backoff preserved when spec unchanged, got BackoffUntil=%v RetryCount=%d", retained.BackoffUntil, retained.RetryCount)
	}
}

// ---- settlePass ----

func TestGroupSync_SettlePass(t *testing.T) {
	const ns = "settle-pass-ns"
	const name = "settle-pass"
	forgetMetricsAfter(t, ns, name)

	t.Run("equivalent status skips the write", func(t *testing.T) {
		fg, feed := newMetricsFeedGroup(ns, name, "")
		fg.Generation = 3
		fg.Status.Feeds[0].LastError = "feed fetch error"
		fg.Status.Feeds[0].LastChecked = metav1TimeRFC3339("2026-11-04T14:00:00Z")

		// Set conditions ahead of time so after settlePass, semantic equality matches original.
		fg.Status.ObservedGeneration = fg.Generation
		setReadyCondition(fg, nil, nil)
		setFeedReachableCondition(fg)

		r := &FeedGroupReconciler{Recorder: events.NewFakeRecorder(10)}
		sync := r.openGroupSync(fg)

		// Since status is already equivalent, settlePass skips applyStatus (no client needed).
		res, err := sync.settlePass(t.Context(), feedPlan{nextDue: 20 * time.Minute}, attemptVerdict{})
		if err != nil {
			t.Fatalf("settlePass error: %v", err)
		}
		if res.RequeueAfter < 18*time.Minute || res.RequeueAfter > 22*time.Minute {
			t.Errorf("unexpected RequeueAfter: %v", res.RequeueAfter)
		}
		requireObservedGeneration(t, fg, 3)

		// Verify per-feed gauges were synced
		if got, ok := feedGaugeValue(feedHealthy, ns, name, feed.Name); !ok || got != 0 {
			t.Errorf("feedHealthy = %v (present=%v), want 0", got, ok)
		}
	})

	t.Run("invalid interval warns and a write failure propagates", func(t *testing.T) {
		fg, feed := newMetricsFeedGroup(ns, name, "")
		fg.Generation = 3
		fg.Spec.Interval = "invalid"
		fg.Status.ObservedGeneration = fg.Generation - 1

		recorder := events.NewFakeRecorder(10)
		r := &FeedGroupReconciler{Recorder: recorder}
		sync := r.openGroupSync(fg)

		// Mutate status to force semantic difference, but with out-of-range status to verify applyStatus error
		fg.Status.Feeds = []v1beta1.FeedStatus{outOfRangeFeedStatus(feed.URL)}
		if _, err := sync.settlePass(t.Context(), feedPlan{}, attemptVerdict{}); err == nil {
			t.Fatal("expected status conversion error from applyStatus")
		}
		requireObservedGeneration(t, fg, fg.Generation)

		eventList := drainEvents(recorder)
		if len(eventList) == 0 || !strings.Contains(eventList[0], reasonInvalidInterval) {
			t.Errorf("expected InvalidInterval warning event, got %v", eventList)
		}
	})
}

// ---- settleWebhookError ----

func TestGroupSync_SettleWebhookError(t *testing.T) {
	const ns = "settle-webhook-ns"
	const name = "settle-webhook"
	forgetMetricsAfter(t, ns, name)
	webhookErr := errors.New("secret missing")

	t.Run("equivalent status skips the write", func(t *testing.T) {
		fg, _ := newMetricsFeedGroup(ns, name, "")
		fg.Generation = 4

		// Pre-populate Ready condition so no-op write can be exercised
		fg.Status.ObservedGeneration = fg.Generation
		setReadyCondition(fg, webhookErr, nil)
		setFeedReachableCondition(fg)

		sync := (&FeedGroupReconciler{}).openGroupSync(fg)

		res, err := sync.settleWebhookError(t.Context(), webhookErr)
		if err != nil {
			t.Fatalf("settleWebhookError error: %v", err)
		}
		if res.RequeueAfter <= 0 {
			t.Errorf("expected positive RequeueAfter, got %v", res.RequeueAfter)
		}
		requireObservedGeneration(t, fg, 4)
		requireReady(t, fg, metav1.ConditionFalse, reasonWebhookUnresolved)
	})

	t.Run("status conversion failure propagates", func(t *testing.T) {
		fg, feed := newMetricsFeedGroup(ns, name, "")
		fg.Generation = 4
		fg.Status.Feeds = []v1beta1.FeedStatus{outOfRangeFeedStatus(feed.URL)}
		fg.Status.ObservedGeneration = fg.Generation - 1
		sync := (&FeedGroupReconciler{}).openGroupSync(fg)
		fg.Status.Feeds[0].LastError = "changed" // make it not deep equal

		if _, err := sync.settleWebhookError(t.Context(), webhookErr); err == nil {
			t.Fatal("expected status conversion error")
		}
		requireObservedGeneration(t, fg, fg.Generation)
	})
}

// ---- settlePanic ----

func TestGroupSync_SettlePanic(t *testing.T) {
	const ns = "settle-panic-ns"
	const name = "settle-panic"
	forgetMetricsAfter(t, ns, name)

	t.Run("does not advance ObservedGeneration and marks Ready false", func(t *testing.T) {
		fg, feed := newMetricsFeedGroup(ns, name, "")
		fg.Generation = 5
		fg.Status.ObservedGeneration = 3
		panicked := &feedPanic{url: feed.URL, value: "unexpected nil"}

		// Pre-set Ready condition to the panic condition so status DeepEquals original and skips write
		setReadyCondition(fg, panicked, nil)
		setFeedReachableCondition(fg)

		sync := (&FeedGroupReconciler{}).openGroupSync(fg)

		if err := sync.settlePanic(t.Context(), panicked); err != nil {
			t.Fatalf("settlePanic returned unexpected error: %v", err)
		}
		requireObservedGeneration(t, fg, 3)
		requireReady(t, fg, metav1.ConditionFalse, reasonReconcilePanicked)

		// Verify errors.Join behavior: when settlePanic returns nil, errors.Join(panicked, nil) wraps panicked
		if joined := errors.Join(panicked, sync.settlePanic(t.Context(), panicked)); !errors.Is(joined, panicked) {
			t.Errorf("joined error = %v, want error matching panicked", joined)
		}
	})

	t.Run("a failed write is returned alongside the panic", func(t *testing.T) {
		fg, feed := newMetricsFeedGroup(ns, name, "")
		fg.Generation = 5
		fg.Status.ObservedGeneration = 3
		panicked := &feedPanic{url: feed.URL, value: "unexpected nil"}

		fg.Status.Feeds = []v1beta1.FeedStatus{outOfRangeFeedStatus(feed.URL)}
		sync := (&FeedGroupReconciler{}).openGroupSync(fg)
		fg.Status.Feeds[0].LastError = "changed" // make it not deep equal

		writeErr := sync.settlePanic(t.Context(), panicked)
		if writeErr == nil {
			t.Fatal("expected status conversion error from settlePanic")
		}
		joined := errors.Join(panicked, writeErr)
		if !errors.Is(joined, panicked) || !errors.Is(joined, writeErr) {
			t.Errorf("joined error = %v, want error containing panicked and writeErr", joined)
		}
	})
}

// ---- settleSuspend ----

// suspendedGroup builds a suspended group at generation 2 (observed 1) whose
// one feed has a watermark and retry state.
func suspendedGroup(ns, name string) *v1beta1.FeedGroup {
	return &v1beta1.FeedGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 2},
		Spec: v1beta1.FeedGroupSpec{
			Suspend: true,
			Feeds:   []v1beta1.FeedSpec{{Name: testFeedName(exampleFeedURL), URL: exampleFeedURL}},
		},
		Status: v1beta1.FeedGroupStatus{
			ObservedGeneration: 1,
			Feeds: []v1beta1.FeedStatus{{
				Name: testFeedName(exampleFeedURL), URL: exampleFeedURL,
				LastSeenEntry: "wm-1",
				RetryCount:    4,
				BackoffUntil:  metav1TimeRFC3339("2026-11-04T15:00:00Z"),
			}},
		},
	}
}

func TestGroupSync_SettleSuspend(t *testing.T) {
	const ns = "settle-suspend-ns"
	const name = "settle-suspend"
	forgetMetricsAfter(t, ns, name)

	r := &FeedGroupReconciler{Client: fakeApplyClient()}

	// settled is the group after its first suspended settlement, as the next
	// reconcile would load it.
	settled := suspendedGroup(ns, name)
	res, err := r.openGroupSync(settled).settleSuspend(t.Context())
	if err != nil {
		t.Fatalf("settleSuspend error: %v", err)
	}

	t.Run("settles Unknown and preserves watermark and backoff", func(t *testing.T) {
		if res.RequeueAfter != 0 {
			t.Errorf("RequeueAfter = %v, want 0", res.RequeueAfter)
		}
		requireObservedGeneration(t, settled, 2)
		requireReady(t, settled, metav1.ConditionUnknown, "Suspended")

		// Watermark and retry backoff must be preserved during suspension
		fs := feedStatusFor(settled, testFeedName(exampleFeedURL))
		if fs == nil || fs.LastSeenEntry != "wm-1" {
			t.Fatalf("feed status watermark not preserved: %+v", fs)
		}
		if fs.RetryCount != 4 || fs.BackoffUntil == nil {
			t.Fatalf("expected RetryCount and BackoffUntil preserved during suspension: %+v", fs)
		}
	})

	t.Run("resuming clears backoff when generation changes", func(t *testing.T) {
		resumed := settled.DeepCopy()
		resumed.Spec.Suspend = false
		resumed.Generation = 3
		r.openGroupSync(resumed)
		fs := feedStatusFor(resumed, testFeedName(exampleFeedURL))
		if fs.RetryCount != 0 || fs.BackoffUntil != nil {
			t.Fatalf("expected RetryCount and BackoffUntil cleared on resume: %+v", fs)
		}
	})

	t.Run("unchanged suspend skips the write", func(t *testing.T) {
		res, err := r.openGroupSync(settled).settleSuspend(t.Context())
		if err != nil {
			t.Fatalf("second settleSuspend error: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Errorf("second RequeueAfter = %v, want 0", res.RequeueAfter)
		}
	})

	t.Run("status apply error is propagated", func(t *testing.T) {
		boom := errors.New("apply error")
		failing := &FeedGroupReconciler{Client: &failingApplyClient{Client: fakeApplyClient(), err: boom}}
		fg := settled.DeepCopy()
		fg.Generation = 3
		if _, err := failing.openGroupSync(fg).settleSuspend(t.Context()); !errors.Is(err, boom) {
			t.Fatalf("settleSuspend error = %v, want %v", err, boom)
		}
	})
}
