package controller

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	"github.com/maverickd650-labs/rss2discord/api/v1alpha1"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

func TestSetReadyCondition_ClampsWebhookErrorMessage(t *testing.T) {
	webhookErr := errors.New(strings.Repeat("k", 10*maxConditionMessageBytes))
	fg := &v1alpha1.FeedGroup{}
	setReadyCondition(fg, webhookErr, nil)

	cond := apimeta.FindStatusCondition(fg.Status.Conditions, v1alpha1.ConditionTypeReady)
	if cond == nil {
		t.Fatal("expected a Ready condition")
	}
	if want := clampText(webhookErr.Error(), maxConditionMessageBytes); cond.Message != want {
		t.Fatalf("Ready message is %d bytes, want the %d-byte clamped webhook error", len(cond.Message), len(want))
	}
	if len(cond.Message) > maxConditionMessageBytes {
		t.Fatalf("Ready message is %d bytes, want at most %d", len(cond.Message), maxConditionMessageBytes)
	}
}

func TestClampText(t *testing.T) {
	if got := clampText(shortContent, 10); got != shortContent {
		t.Errorf("clampText(short) = %q", got)
	}
	long := strings.Repeat("é", 100) // 2 bytes per rune
	// Budgets 50-51 land the cut mid-rune for one parity and on a boundary
	// for the other, so both branches of the UTF-8 backoff run.
	for _, budget := range []int{50, 51} {
		got := clampText(long, budget)
		if len(got) > budget || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
			t.Errorf("clampText(budget=%d) split a rune or overshot: len=%d valid=%v %q", budget, len(got), utf8.ValidString(got), got)
		}
	}
}

// TestEnsureFeedStatuses_NormalizesOversizedStoredValues asserts values
// persisted before the status fields were bounded are brought within the
// CRD limits on load (an oversized one would be rejected on every write),
// while the long watermark still matches the entry it was derived from.
func TestEnsureFeedStatuses_NormalizesOversizedStoredValues(t *testing.T) {
	longID := "https://example.com/" + strings.Repeat("p", 4000)
	fg, feed := newMetricsFeedGroup("normalize-ns", "normalize", "")
	fs := feedStatusFor(fg, feed.RSSUrl)
	fs.LastError = strings.Repeat("e", 30000)
	fs.LastSeenEntry = longID

	ensureFeedStatuses(fg)

	fs = feedStatusFor(fg, feed.RSSUrl)
	if len(fs.LastError) > maxStatusTextBytes {
		t.Errorf("LastError is %d bytes after load, want <= %d", len(fs.LastError), maxStatusTextBytes)
	}
	if len(fs.LastSeenEntry) > maxStatusTextBytes {
		t.Errorf("LastSeenEntry is %d bytes after load, want <= %d", len(fs.LastSeenEntry), maxStatusTextBytes)
	}
	if !ledgerResumes([]rss.Entry{{ID: longID}}, fs.LastSeenEntry) {
		t.Error("normalized watermark no longer matches the entry it was derived from")
	}
	// A watermark freshly stored for the same entry is the same value.
	if fresh := clampWatermark(entryIdentity(rss.Entry{ID: longID})); fresh != fs.LastSeenEntry {
		t.Errorf("fresh watermark %q != normalized legacy watermark %q", fresh, fs.LastSeenEntry)
	}
}

// TestFeedGroupStatusWorstCaseStaysUnderBudget fills every bounded status
// field to its cap for a maximal (50-feed) group and checks the serialized
// status leaves room in etcd's ~1.5MB object limit for the spec.
func TestFeedGroupStatusWorstCaseStaysUnderBudget(t *testing.T) {
	var status v1alpha1.FeedGroupStatus
	for i := range 50 {
		fs := v1alpha1.FeedStatus{
			RSSUrl:        "https://example.com/" + strings.Repeat("u", 2048-21) + strconv.Itoa(i%10),
			LastChecked:   strings.Repeat("t", 64),
			LastSeenEntry: clampWatermark(strings.Repeat("s", 5000)),
			LastError:     clampText(strings.Repeat("e", 40000), maxStatusTextBytes),
			ETag:          strings.Repeat("g", 256),
			LastModified:  strings.Repeat("m", 64),
			BackoffUntil:  strings.Repeat("b", 64),
			LastSent:      map[string]string{},
		}
		for j := range maxLastSentPerFeed {
			fs.LastSent[hashIdentity(strconv.Itoa(j))] = testReconcileTime
		}
		for _, typ := range []string{v1alpha1.FeedConditionTypeReachable, v1alpha1.FeedConditionTypeDelivered} {
			setFeedCondition(&fs, typ, metav1.ConditionFalse, strings.Repeat("R", 64), strings.Repeat("m", 40000), 1)
		}
		status.Feeds = append(status.Feeds, fs)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	const budget = 800 * 1024
	if len(raw) > budget {
		t.Fatalf("worst-case status is %d bytes, over the %d budget", len(raw), budget)
	}
	t.Logf("worst-case status: %d bytes", len(raw))
}

// TestEnsureFeedStatuses asserts ensureFeedStatuses drops the FeedStatus
// entry for a URL no longer in spec, carries over an existing entry's state
// for a URL still in spec untouched, and adds a fresh zero-valued entry for
// a URL newly added to spec.
func TestEnsureFeedStatuses(t *testing.T) {
	const keptURL = "https://example.com/kept.xml"
	const staleURL = "https://example.com/stale.xml"
	const newURL = "https://example.com/new.xml"

	fg := &v1alpha1.FeedGroup{
		Spec: v1alpha1.FeedGroupSpec{
			Feeds: []v1alpha1.FeedSpec{{RSSUrl: keptURL}, {RSSUrl: newURL}},
		},
		Status: v1alpha1.FeedGroupStatus{
			Feeds: []v1alpha1.FeedStatus{
				{RSSUrl: keptURL, LastError: "x", RetryCount: 1},
				{RSSUrl: staleURL, LastError: "x", RetryCount: 1},
			},
		},
	}

	ensureFeedStatuses(fg)

	if got := feedStatusFor(fg, staleURL); got != nil {
		t.Fatalf("expected stale URL's FeedStatus to be removed, got %+v", got)
	}

	kept := feedStatusFor(fg, keptURL)
	if kept == nil {
		t.Fatal("expected kept URL's FeedStatus to be retained")
	}
	if kept.LastError != "x" || kept.RetryCount != 1 {
		t.Fatalf("expected kept URL's existing state to be preserved, got %+v", kept)
	}

	newEntry := feedStatusFor(fg, newURL)
	if newEntry == nil {
		t.Fatal("expected new URL to get a fresh FeedStatus entry")
	}
	if newEntry.LastError != "" || newEntry.RetryCount != 0 {
		t.Fatalf("expected new URL's FeedStatus to be zero-valued, got %+v", newEntry)
	}

	if len(fg.Status.Feeds) != 2 {
		t.Fatalf("expected exactly one FeedStatus per spec feed, got %d", len(fg.Status.Feeds))
	}
}

func TestSetFeedCondition_IgnoresObservedGenerationOnlyChange(t *testing.T) {
	const differentInvalidFilter = "different invalid filter"
	oversized := strings.Repeat("x", maxConditionMessageBytes+64)
	fs := &v1alpha1.FeedStatus{}
	if !setFeedCondition(fs, v1alpha1.FeedConditionTypeDelivered, metav1.ConditionFalse, reasonConfigError, oversized, 1) {
		t.Fatal("expected a newly added condition to count as changed")
	}
	if setFeedCondition(fs, v1alpha1.FeedConditionTypeDelivered, metav1.ConditionFalse, reasonConfigError, oversized, 2) {
		t.Fatal("ObservedGeneration-only change with clamped message must not count as semantic change")
	}
	condition := apimeta.FindStatusCondition(fs.Conditions, v1alpha1.FeedConditionTypeDelivered)
	if condition == nil || condition.ObservedGeneration != 2 || len(condition.Message) > maxConditionMessageBytes {
		t.Fatalf("condition = %+v, want ObservedGeneration 2 and clamped Message", condition)
	}
	if !setFeedCondition(fs, v1alpha1.FeedConditionTypeDelivered, metav1.ConditionFalse, reasonConfigError, differentInvalidFilter, 2) {
		t.Fatal("expected a message change to count as semantic change")
	}
	if !setFeedCondition(fs, v1alpha1.FeedConditionTypeDelivered, metav1.ConditionTrue, reasonConfigError, differentInvalidFilter, 2) {
		t.Fatal("expected a status change to count as semantic change")
	}
	if !setFeedCondition(fs, v1alpha1.FeedConditionTypeDelivered, metav1.ConditionTrue, reasonMessageSent, differentInvalidFilter, 2) {
		t.Fatal("expected a reason change to count as semantic change")
	}
}

func TestSetReadyCondition_IgnoresObservedGenerationOnlyChange(t *testing.T) {
	fg := &v1alpha1.FeedGroup{}
	fg.Generation = 1
	intervalErr := errors.New("invalid interval")
	if !setReadyCondition(fg, nil, intervalErr) {
		t.Fatal("expected a newly added Ready condition to count as changed")
	}
	fg.Generation = 2
	if setReadyCondition(fg, nil, intervalErr) {
		t.Fatal("ObservedGeneration-only Ready update must not count as semantic change")
	}
	condition := apimeta.FindStatusCondition(fg.Status.Conditions, v1alpha1.ConditionTypeReady)
	if condition == nil || condition.ObservedGeneration != 2 {
		t.Fatalf("Ready condition = %+v, want ObservedGeneration 2", condition)
	}
}

func TestOpenGroupSync(t *testing.T) {
	const ns = "open-sync-ns"
	const name = "open-sync"
	const retainedURL = "https://example.com/retained.xml"
	const prunedURL = "https://example.com/pruned.xml"
	const newURL = "https://example.com/new.xml"
	const testBackoffUntil = "2026-11-04T15:00:00Z"

	forgetMetricsAfter(t, ns, name)

	fg := &v1alpha1.FeedGroup{
		Spec: v1alpha1.FeedGroupSpec{
			Interval: "15m",
			Feeds: []v1alpha1.FeedSpec{
				{RSSUrl: retainedURL},
				{RSSUrl: newURL},
			},
		},
		Status: v1alpha1.FeedGroupStatus{
			Feeds: []v1alpha1.FeedStatus{
				{RSSUrl: retainedURL, LastError: "404", RetryCount: 3, BackoffUntil: testBackoffUntil},
				{RSSUrl: prunedURL, LastError: "500", RetryCount: 1},
			},
		},
	}
	fg.Namespace = ns
	fg.Name = name

	// Populate metrics for the pruned feed so we can verify forgetFeed was called.
	metricsFor(fg).syncFeedState(fg)
	if _, ok := feedGaugeValue(feedHealthy, ns, name, prunedURL); !ok {
		t.Fatalf("expected feedHealthy metric to exist for %q before pruning", prunedURL)
	}

	r := &FeedGroupReconciler{}

	// Case 1: Spec changed (Generation > ObservedGeneration)
	fg.Generation = 2
	fg.Status.ObservedGeneration = 1

	sync := r.openGroupSync(fg)

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

	// Verify dropped feed was pruned from Status.Feeds and its metrics forgotten
	if got := feedStatusFor(fg, prunedURL); got != nil {
		t.Errorf("expected pruned feed %q to be removed from Status.Feeds", prunedURL)
	}
	if _, ok := feedGaugeValue(feedHealthy, ns, name, prunedURL); ok {
		t.Errorf("expected feedHealthy metric for %q to be forgotten after openGroupSync", prunedURL)
	}

	// Verify new feed was added
	if got := feedStatusFor(fg, newURL); got == nil {
		t.Errorf("expected new feed %q to be added to Status.Feeds", newURL)
	}

	// Verify permanent backoff was cleared on spec change
	retained := feedStatusFor(fg, retainedURL)
	if retained == nil {
		t.Fatalf("expected retained feed %q to exist", retainedURL)
	}
	if retained.BackoffUntil != "" || retained.RetryCount != 0 {
		t.Errorf("expected backoff cleared on spec change, got BackoffUntil=%q RetryCount=%d", retained.BackoffUntil, retained.RetryCount)
	}

	// Verify snapshot deep-copies status
	if sync.original == &fg.Status {
		t.Error("sync.original aliases fg.Status")
	}
	retained.RetryCount = 42
	if sync.original.Feeds[0].RetryCount == 42 {
		t.Error("mutating fg.Status mutated sync.original")
	}

	// Case 2: Spec unchanged (Generation == ObservedGeneration)
	fg.Generation = 2
	fg.Status.ObservedGeneration = 2
	retained.BackoffUntil = testBackoffUntil
	retained.RetryCount = 5

	syncUnchanged := r.openGroupSync(fg)
	if syncUnchanged.specChanged {
		t.Error("expected specChanged to be false when Generation == ObservedGeneration")
	}
	gotRetained := feedStatusFor(fg, retainedURL)
	if gotRetained == nil {
		t.Fatalf("expected retained feed %q to exist after unchanged-generation openGroupSync", retainedURL)
	}
	if gotRetained.BackoffUntil != testBackoffUntil || gotRetained.RetryCount != 5 {
		t.Errorf("expected backoff preserved when spec unchanged, got BackoffUntil=%q RetryCount=%d", gotRetained.BackoffUntil, gotRetained.RetryCount)
	}
}

func outOfRangeFeedStatus(url string) v1alpha1.FeedStatus {
	return v1alpha1.FeedStatus{
		RSSUrl: url,
		Conditions: []metav1.Condition{{
			Type:               v1alpha1.FeedConditionTypeReachable,
			Status:             metav1.ConditionTrue,
			Reason:             "TimeOutOfRange",
			LastTransitionTime: metav1.NewTime(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)),
		}},
	}
}

func TestGroupSync_SettlePass(t *testing.T) {
	const ns = "settle-pass-ns"
	const name = "settle-pass"
	forgetMetricsAfter(t, ns, name)

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Generation = 3
	fg.Status.ObservedGeneration = 2
	fg.Status.Feeds[0].LastError = "feed fetch error"
	fg.Status.Feeds[0].LastChecked = "2026-11-04T14:00:00Z"

	// Set conditions ahead of time so after settlePass, semantic equality matches original.
	fg.Status.ObservedGeneration = fg.Generation
	setReadyCondition(fg, nil, nil)
	setFeedReachableCondition(fg)

	recorder := events.NewFakeRecorder(10)
	r := &FeedGroupReconciler{Recorder: recorder}

	sync := r.openGroupSync(fg)

	// Since status is already equivalent, settlePass skips applyStatus (no client needed).
	res, err := sync.settlePass(t.Context(), feedPlan{nextDue: 20 * time.Minute}, attemptVerdict{})
	if err != nil {
		t.Fatalf("settlePass error: %v", err)
	}
	if res.RequeueAfter < 18*time.Minute || res.RequeueAfter > 22*time.Minute {
		t.Errorf("unexpected RequeueAfter: %v", res.RequeueAfter)
	}
	if fg.Status.ObservedGeneration != 3 {
		t.Errorf("ObservedGeneration = %d, want 3", fg.Status.ObservedGeneration)
	}

	// Verify per-feed gauges were synced
	if got, ok := feedGaugeValue(feedHealthy, ns, name, feed.RSSUrl); !ok || got != 0 {
		t.Errorf("feedHealthy = %v (present=%v), want 0", got, ok)
	}

	// Warning Event for InvalidInterval
	fg.Spec.Interval = "invalid"
	fg.Status.ObservedGeneration = fg.Generation - 1
	syncInvalid := r.openGroupSync(fg)
	// Mutate status to force semantic difference, but with out-of-range status to verify applyStatus error
	fg.Status.Feeds = []v1alpha1.FeedStatus{outOfRangeFeedStatus(feed.RSSUrl)}
	_, err = syncInvalid.settlePass(t.Context(), feedPlan{}, attemptVerdict{})
	if err == nil {
		t.Fatal("expected status conversion error from applyStatus")
	}
	if fg.Status.ObservedGeneration != fg.Generation {
		t.Errorf("ObservedGeneration = %d, want %d", fg.Status.ObservedGeneration, fg.Generation)
	}
	eventList := drainEvents(recorder)
	if len(eventList) == 0 || !strings.Contains(eventList[0], reasonInvalidInterval) {
		t.Errorf("expected InvalidInterval warning event, got %v", eventList)
	}
}

func TestGroupSync_SettleWebhookError(t *testing.T) {
	const ns = "settle-webhook-ns"
	const name = "settle-webhook"
	forgetMetricsAfter(t, ns, name)

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Generation = 4
	fg.Status.ObservedGeneration = 2
	webhookErr := errors.New("secret missing")

	// Pre-populate Ready condition so no-op write can be exercised
	fg.Status.ObservedGeneration = fg.Generation
	setReadyCondition(fg, webhookErr, nil)
	setFeedReachableCondition(fg)

	r := &FeedGroupReconciler{}
	sync := r.openGroupSync(fg)

	res, err := sync.settleWebhookError(t.Context(), webhookErr)
	if err != nil {
		t.Fatalf("settleWebhookError error: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("expected positive RequeueAfter, got %v", res.RequeueAfter)
	}
	if fg.Status.ObservedGeneration != 4 {
		t.Errorf("ObservedGeneration = %d, want 4", fg.Status.ObservedGeneration)
	}

	ready := apimeta.FindStatusCondition(fg.Status.Conditions, v1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != reasonWebhookUnresolved {
		t.Fatalf("Ready condition = %+v, want False with reason %q", ready, reasonWebhookUnresolved)
	}

	// Status conversion failure propagates error
	fg.Status.Feeds = []v1alpha1.FeedStatus{outOfRangeFeedStatus(feed.RSSUrl)}
	fg.Status.ObservedGeneration = fg.Generation - 1
	syncErr := r.openGroupSync(fg)
	fg.Status.Feeds[0].LastError = "changed" // make it not deep equal
	_, err = syncErr.settleWebhookError(t.Context(), webhookErr)
	if err == nil {
		t.Fatal("expected status conversion error")
	}
	if fg.Status.ObservedGeneration != fg.Generation {
		t.Errorf("ObservedGeneration = %d, want %d", fg.Status.ObservedGeneration, fg.Generation)
	}
}

func TestGroupSync_SettlePanic(t *testing.T) {
	const ns = "settle-panic-ns"
	const name = "settle-panic"
	forgetMetricsAfter(t, ns, name)

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Generation = 5
	fg.Status.ObservedGeneration = 3
	panicked := &feedPanic{url: feed.RSSUrl, value: "unexpected nil"}

	// Pre-set Ready condition to the panic condition so status DeepEquals original and skips write
	setReadyCondition(fg, panicked, nil)
	setFeedReachableCondition(fg)

	r := &FeedGroupReconciler{}
	sync := r.openGroupSync(fg)

	err := sync.settlePanic(t.Context(), panicked)
	if err != nil {
		t.Fatalf("settlePanic returned unexpected error: %v", err)
	}

	// ObservedGeneration must NOT advance on panic
	if fg.Status.ObservedGeneration != 3 {
		t.Errorf("ObservedGeneration = %d, want unchanged 3", fg.Status.ObservedGeneration)
	}

	ready := apimeta.FindStatusCondition(fg.Status.Conditions, v1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != reasonReconcilePanicked {
		t.Fatalf("Ready condition = %+v, want False with reason %q", ready, reasonReconcilePanicked)
	}

	// Verify errors.Join behavior: when settlePanic returns nil, errors.Join(panicked, nil) wraps panicked
	if joined := errors.Join(panicked, sync.settlePanic(t.Context(), panicked)); !errors.Is(joined, panicked) {
		t.Errorf("joined error = %v, want error matching panicked", joined)
	}

	// When write fails, error is returned
	fg.Status.Feeds = []v1alpha1.FeedStatus{outOfRangeFeedStatus(feed.RSSUrl)}
	syncErr := r.openGroupSync(fg)
	fg.Status.Feeds[0].LastError = "changed" // make it not deep equal
	writeErr := syncErr.settlePanic(t.Context(), panicked)
	if writeErr == nil {
		t.Fatal("expected status conversion error from settlePanic")
	}
	if joined := errors.Join(panicked, writeErr); joined == nil || !errors.Is(joined, panicked) || !errors.Is(joined, writeErr) {
		t.Errorf("joined error = %v, want error containing panicked and writeErr", joined)
	}
}
