package controller

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
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
