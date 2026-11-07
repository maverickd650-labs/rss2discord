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
	expectf(t, len(fs.LastError) <= maxStatusTextBytes, "LastError is %d bytes after load, want <= %d", len(fs.LastError), maxStatusTextBytes)
	expectf(t, len(fs.LastSeenEntry) <= maxStatusTextBytes, "LastSeenEntry is %d bytes after load, want <= %d", len(fs.LastSeenEntry), maxStatusTextBytes)
	expectf(t, ledgerResumes([]rss.Entry{{ID: longID}}, fs.LastSeenEntry), "normalized watermark no longer matches the entry it was derived from")
	// A watermark freshly stored for the same entry is the same value.
	fresh := clampWatermark(entryIdentity(rss.Entry{ID: longID}))
	expectf(t, fresh == fs.LastSeenEntry, "fresh watermark %q != normalized legacy watermark %q", fresh, fs.LastSeenEntry)
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
		got := feedStatusFor(fg, testFeedName(staleURL))
		expectf(t, got == nil, "expected stale URL's FeedStatus to be removed, got %+v", got)
	})
	t.Run("keeps an existing feed's state", func(t *testing.T) {
		kept := feedStatusFor(fg, testFeedName(keptURL))
		requiref(t, kept != nil, "expected kept URL's FeedStatus to be retained")
		expectf(t, kept.LastError == "x", "expected kept URL's existing state to be preserved, got %+v", kept)
		expectf(t, kept.RetryCount == 1, "expected kept URL's existing state to be preserved, got %+v", kept)
	})
	t.Run("adds a zero-valued entry for a new feed", func(t *testing.T) {
		added := feedStatusFor(fg, testFeedName(newURL))
		requiref(t, added != nil, "expected new URL to get a fresh FeedStatus entry")
		expectf(t, added.LastError == "", "expected new URL's FeedStatus to be zero-valued, got %+v", added)
		expectf(t, added.RetryCount == 0, "expected new URL's FeedStatus to be zero-valued, got %+v", added)
	})
	t.Run("one status per spec feed", func(t *testing.T) {
		expectf(t, len(fg.Status.Feeds) == 2, "expected exactly one FeedStatus per spec feed, got %d", len(fg.Status.Feeds))
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

	pruned := ensureFeedStatuses(fg)
	expectf(t, len(pruned) == 0, "pruned = %v, want none: the feed's name is unchanged", pruned)

	fs := feedStatusFor(fg, feedName)
	requiref(t, fs != nil, "no status for %q after the URL change", feedName)
	requiref(t, fs.URL == newURL, "fs = %+v, want the status re-pointed at %s", fs, newURL)
	expectf(t, keepsDeliveryHistory(fs), "delivery history not preserved: %+v", fs)
	expectf(t, carriesNoValidators(fs), "validators carried to the new URL: etag=%q lastModified=%q", fs.ETag, fs.LastModified)
	expectf(t, hasNoCheckOrRetryState(fs), "check/retry state carried to the new URL: %+v", fs)
}

// keepsDeliveryHistory reports whether fs still holds the delivery history the
// URL-change test seeds: initialized, its watermark and its one sent entry.
func keepsDeliveryHistory(fs *v1beta1.FeedStatus) bool {
	sent := fs.LastSent["hash1"]
	return fs.Initialized && fs.LastSeenEntry == "entry-123" && sent.Time.Equal(ts("2026-01-01T00:00:00Z").Time)
}

// carriesNoValidators reports whether fs holds neither an ETag nor a
// Last-Modified.
func carriesNoValidators(fs *v1beta1.FeedStatus) bool {
	return fs.ETag == "" && fs.LastModified == ""
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
	fs := feedStatusFor(fg, "a")
	expectf(t, fs.ETag == `"e"`, "fs = %+v, want validators and retry state kept", fs)
	expectf(t, fs.LastModified == "lm", "fs = %+v, want validators and retry state kept", fs)
	expectf(t, fs.RetryCount == 1, "fs = %+v, want validators and retry state kept", fs)
}

// Renaming a feed is a different feed: the old name's status is pruned and
// the new name starts fresh, even at the same URL.
func TestEnsureFeedStatuses_RenameStartsFresh(t *testing.T) {
	const url = "https://example.com/feed.xml"
	fg := &v1beta1.FeedGroup{
		Spec:   v1beta1.FeedGroupSpec{Feeds: []v1beta1.FeedSpec{{Name: "new-name", URL: url}}},
		Status: v1beta1.FeedGroupStatus{Feeds: []v1beta1.FeedStatus{{Name: "old-name", URL: url, LastSeenEntry: "entry"}}},
	}
	pruned := ensureFeedStatuses(fg)
	expectf(t, slices.Equal(pruned, []string{"old-name"}), "pruned = %v, want [old-name]", pruned)

	fs := feedStatusFor(fg, "new-name")
	requiref(t, fs != nil, "no status for new-name")
	expectf(t, fs.LastSeenEntry == "", "fs = %+v, want a fresh status for new-name", fs)
	expectf(t, fs.URL == url, "fs = %+v, want a fresh status for new-name", fs)
	expectf(t, feedStatusFor(fg, "old-name") == nil, "old-name's status survived")
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
	a := feedStatusFor(fg, "a")
	expectf(t, a.URL == urlB, "a = %+v", a)
	expectf(t, a.LastSeenEntry == "a-entry", "a = %+v", a)
	b := feedStatusFor(fg, "b")
	expectf(t, b.URL == urlA, "b = %+v", b)
	expectf(t, b.LastSeenEntry == "b-entry", "b = %+v", b)
}
