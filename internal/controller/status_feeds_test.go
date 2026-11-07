package controller

import (
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// TestEnsureFeedStatuses_NormalizesOversizedStoredValues asserts values
// persisted before the status fields were bounded are brought within the
// CRD limits on load (an oversized one would be rejected on every write),
// while the long watermark still matches the entry it was derived from.
func TestEnsureFeedStatuses_NormalizesOversizedStoredValues(t *testing.T) {
	longID := "https://example.com/" + strings.Repeat("p", 4000)
	fg, feed := newMetricsFeedGroup("normalize-ns", "normalize", "")
	fs := feedStatusFor(fg, feed.Name)
	fs.LastError = strings.Repeat("e", 30000)
	fs.LastSeenEntry = longID

	ensureFeedStatuses(fg)

	fs = feedStatusFor(fg, feed.Name)
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

// TestEnsureFeedStatuses asserts ensureFeedStatuses drops the FeedStatus
// entry for a URL no longer in spec, carries over an existing entry's state
// for a URL still in spec untouched, and adds a fresh zero-valued entry for
// a URL newly added to spec.
func TestEnsureFeedStatuses(t *testing.T) {
	const keptURL = "https://example.com/kept.xml"
	const staleURL = "https://example.com/stale.xml"
	const newURL = "https://example.com/new.xml"

	fg := &v1beta1.FeedGroup{
		Spec: v1beta1.FeedGroupSpec{
			Feeds: []v1beta1.FeedSpec{{Name: testFeedName(keptURL), URL: keptURL}, {Name: testFeedName(newURL), URL: newURL}},
		},
		Status: v1beta1.FeedGroupStatus{
			Feeds: []v1beta1.FeedStatus{
				{Name: testFeedName(keptURL), URL: keptURL, LastError: "x", RetryCount: 1},
				{Name: testFeedName(staleURL), URL: staleURL, LastError: "x", RetryCount: 1},
			},
		},
	}

	ensureFeedStatuses(fg)

	t.Run("drops a feed no longer in spec", func(t *testing.T) {
		if got := feedStatusFor(fg, testFeedName(staleURL)); got != nil {
			t.Fatalf("expected stale URL's FeedStatus to be removed, got %+v", got)
		}
	})
	t.Run("keeps an existing feed's state", func(t *testing.T) {
		kept := feedStatusFor(fg, testFeedName(keptURL))
		if kept == nil {
			t.Fatal("expected kept URL's FeedStatus to be retained")
		}
		if kept.LastError != "x" || kept.RetryCount != 1 {
			t.Fatalf("expected kept URL's existing state to be preserved, got %+v", kept)
		}
	})
	t.Run("adds a zero-valued entry for a new feed", func(t *testing.T) {
		newEntry := feedStatusFor(fg, testFeedName(newURL))
		if newEntry == nil {
			t.Fatal("expected new URL to get a fresh FeedStatus entry")
		}
		if newEntry.LastError != "" || newEntry.RetryCount != 0 {
			t.Fatalf("expected new URL's FeedStatus to be zero-valued, got %+v", newEntry)
		}
	})
	t.Run("one status per spec feed", func(t *testing.T) {
		if len(fg.Status.Feeds) != 2 {
			t.Fatalf("expected exactly one FeedStatus per spec feed, got %d", len(fg.Status.Feeds))
		}
	})
}

// A feed's name is its identity: when its URL changes it keeps its delivery
// history, but nothing the old URL's server said -- validators, check time,
// retry state -- carries over to the new one.
func TestEnsureFeedStatuses_URLChangeKeepsHistoryNotServerState(t *testing.T) {
	const (
		oldURL   = "https://example.com/old.xml"
		newURL   = "https://example.com/new.xml"
		feedName = "my-feed"
	)
	checked := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	fg := &v1beta1.FeedGroup{
		Spec: v1beta1.FeedGroupSpec{Feeds: []v1beta1.FeedSpec{{Name: feedName, URL: newURL}}},
		Status: v1beta1.FeedGroupStatus{Feeds: []v1beta1.FeedStatus{{
			Name:          feedName,
			URL:           oldURL,
			Initialized:   true,
			LastSeenEntry: "entry-123",
			LastSent:      map[string]metav1.Time{"hash1": ts("2026-01-01T00:00:00Z")},
			ETag:          `"etag-val"`,
			LastModified:  "Wed, 01 Jan 2026 00:00:00 GMT",
			LastChecked:   &checked,
			RetryCount:    2,
			BackoffUntil:  &checked,
		}}},
	}

	if pruned := ensureFeedStatuses(fg); len(pruned) != 0 {
		t.Fatalf("pruned = %v, want none: the feed's name is unchanged", pruned)
	}

	fs := feedStatusFor(fg, feedName)
	if fs == nil || fs.URL != newURL {
		t.Fatalf("fs = %+v, want the status re-pointed at %s", fs, newURL)
	}
	if !keepsDeliveryHistory(fs) {
		t.Errorf("delivery history not preserved: %+v", fs)
	}
	if fs.ETag != "" || fs.LastModified != "" {
		t.Errorf("validators carried to the new URL: etag=%q lastModified=%q", fs.ETag, fs.LastModified)
	}
	if !hasNoCheckOrRetryState(fs) {
		t.Errorf("check/retry state carried to the new URL: %+v", fs)
	}
}

// keepsDeliveryHistory reports whether fs still holds the delivery history the
// URL-change test seeds: initialized, its watermark and its one sent entry.
func keepsDeliveryHistory(fs *v1beta1.FeedStatus) bool {
	return fs.Initialized &&
		fs.LastSeenEntry == "entry-123" &&
		fs.LastSent["hash1"].Time.Equal(ts("2026-01-01T00:00:00Z").Time)
}

// hasNoCheckOrRetryState reports whether fs carries no check time and no
// retry state.
func hasNoCheckOrRetryState(fs *v1beta1.FeedStatus) bool {
	return fs.LastChecked == nil && fs.RetryCount == 0 && fs.BackoffUntil == nil
}

// An unchanged URL keeps its validators.
func TestEnsureFeedStatuses_SameURLKeepsValidators(t *testing.T) {
	const url = "https://example.com/feed.xml"
	fg := &v1beta1.FeedGroup{
		Spec:   v1beta1.FeedGroupSpec{Feeds: []v1beta1.FeedSpec{{Name: "a", URL: url}}},
		Status: v1beta1.FeedGroupStatus{Feeds: []v1beta1.FeedStatus{{Name: "a", URL: url, ETag: `"e"`, LastModified: "lm", RetryCount: 1}}},
	}
	ensureFeedStatuses(fg)
	if fs := feedStatusFor(fg, "a"); fs.ETag != `"e"` || fs.LastModified != "lm" || fs.RetryCount != 1 {
		t.Fatalf("fs = %+v, want validators and retry state kept", fs)
	}
}

// Renaming a feed is a different feed: the old name's status is pruned and
// the new name starts fresh, even at the same URL.
func TestEnsureFeedStatuses_RenameStartsFresh(t *testing.T) {
	const url = "https://example.com/feed.xml"
	fg := &v1beta1.FeedGroup{
		Spec:   v1beta1.FeedGroupSpec{Feeds: []v1beta1.FeedSpec{{Name: "new-name", URL: url}}},
		Status: v1beta1.FeedGroupStatus{Feeds: []v1beta1.FeedStatus{{Name: "old-name", URL: url, LastSeenEntry: "entry"}}},
	}
	if pruned := ensureFeedStatuses(fg); !slices.Equal(pruned, []string{"old-name"}) {
		t.Fatalf("pruned = %v, want [old-name]", pruned)
	}
	if fs := feedStatusFor(fg, "new-name"); fs == nil || fs.LastSeenEntry != "" || fs.URL != url {
		t.Fatalf("fs = %+v, want a fresh status for new-name", fs)
	}
	if feedStatusFor(fg, "old-name") != nil {
		t.Fatal("old-name's status survived")
	}
}

// Two feeds swapping URLs keep their own histories.
func TestEnsureFeedStatuses_SwappedURLsFollowNames(t *testing.T) {
	const urlA, urlB = "https://a.example.com/feed.xml", "https://b.example.com/feed.xml"
	fg := &v1beta1.FeedGroup{
		Spec: v1beta1.FeedGroupSpec{Feeds: []v1beta1.FeedSpec{{Name: "a", URL: urlB}, {Name: "b", URL: urlA}}},
		Status: v1beta1.FeedGroupStatus{Feeds: []v1beta1.FeedStatus{
			{Name: "a", URL: urlA, LastSeenEntry: "a-entry"},
			{Name: "b", URL: urlB, LastSeenEntry: "b-entry"},
		}},
	}
	ensureFeedStatuses(fg)
	if fs := feedStatusFor(fg, "a"); fs.URL != urlB || fs.LastSeenEntry != "a-entry" {
		t.Fatalf("a = %+v", fs)
	}
	if fs := feedStatusFor(fg, "b"); fs.URL != urlA || fs.LastSeenEntry != "b-entry" {
		t.Fatalf("b = %+v", fs)
	}
}
