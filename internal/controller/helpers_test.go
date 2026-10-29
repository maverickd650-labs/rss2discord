package controller

import (
	"errors"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// overflowingInterval passes the CRD's Pattern and MaxLength yet is rejected
// by time.ParseDuration (overflow); notParsable is rejected outright.
const (
	overflowingInterval = "9999999999999h"
	notParsable         = "nope"
)

// shortContent is reused across truncation test cases below.
const shortContent = "short"

// Shared fixtures that several tests below (and fuzz_test.go) repeat.
const (
	jsAlertURL        = "javascript:alert(1)"
	discordRelativeTS = "<t:1714979289:R>"
	imageFixtureURL   = "https://x/a.png"
)
const truncateMe = "truncate me"

// excerptText is reused across the "continue reading" stripHTML test cases
// below.
const excerptText = "Some excerpt text"

func TestParseHexColor(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  int
	}{
		{name: "hash prefix", input: "#00FF00", want: 0x00FF00},
		{name: "no prefix", input: "FF0000", want: 0xFF0000},
		{name: "lowercase", input: "#ffffff", want: 0xFFFFFF},
		{name: "blank", input: "  ", want: 0},
		{name: "malformed", input: "#nothex", want: 0},
		{name: "with surrounding space", input: "  #123456  ", want: 0x123456},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseHexColor(tc.input); got != tc.want {
				t.Fatalf("parseHexColor(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestHTTPURLOrEmpty(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "https kept", input: "https://example.com/a", want: "https://example.com/a"},
		{name: "http kept", input: "http://example.com/a", want: "http://example.com/a"},
		{name: "javascript dropped", input: jsAlertURL, want: ""},
		{name: "data dropped", input: "data:text/html,<script>", want: ""},
		{name: "empty dropped", input: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := httpURLOrEmpty(tc.input); got != tc.want {
				t.Fatalf("httpURLOrEmpty(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestStripHTML(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "plain text unchanged", input: "plain text body", want: "plain text body"},
		{name: "paragraphs become blank lines", input: "<p>one</p><p>two</p>", want: "one\n\ntwo"},
		{name: "br becomes newline", input: "a<br>b", want: "a\nb"},
		{name: "inline tags stripped", input: "<b>bold</b> text", want: "bold text"},
		{name: "entities unescaped", input: "a &amp; b &lt;c&gt;", want: "a & b <c>"},
		{name: "collapses excess blank lines", input: "<p>a</p><br><br><br><p>b</p>", want: "a\n\nb"},
		{
			name:  "trailing continue reading link stripped",
			input: `<p>` + excerptText + `</p><a href="https://example.com/full">Continue reading...</a>`,
			want:  excerptText,
		},
		{
			name:  "trailing continue reading link with ellipsis char stripped",
			input: `<p>` + excerptText + `</p><p><a href="https://example.com/full">Continue reading…</a></p>`,
			want:  excerptText,
		},
		{
			name:  "trailing continue reading plain text stripped",
			input: excerptText + "\nContinue reading...",
			want:  excerptText,
		},
		{
			name:  "continue reading mid-text kept",
			input: "<p>Click <a href=\"https://example.com\">Continue reading</a> below for more, then come back.</p>",
			want:  "Click Continue reading below for more, then come back.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripHTML(tc.input); got != tc.want {
				t.Fatalf("stripHTML(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestTruncateMessage(t *testing.T) {
	cases := []struct {
		name         string
		content      string
		max          int
		want         string
		wantOverflow int
	}{
		{name: "under limit unchanged", content: shortContent, max: 10, want: shortContent, wantOverflow: 0},
		{name: "exactly at limit unchanged", content: shortContent, max: 5, want: shortContent, wantOverflow: 0},
		{name: "truncated with ellipsis", content: truncateMe, max: 5, want: "trun…", wantOverflow: len([]rune(truncateMe)) - 5},
		{name: "multibyte trimmed by rune", content: "日本語テスト", max: 3, want: "日本…", wantOverflow: 3},
		{name: "max one keeps single rune without ellipsis", content: truncateMe, max: 1, want: "t", wantOverflow: len([]rune(truncateMe)) - 1},
		{name: "max zero yields empty", content: truncateMe, max: 0, want: "", wantOverflow: len([]rune(truncateMe))},
		{name: "negative max yields empty", content: truncateMe, max: -5, want: "", wantOverflow: len([]rune(truncateMe)) + 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, overflow := truncateMessage(tc.content, tc.max)
			if got != tc.want {
				t.Fatalf("truncateMessage(%q, %d) = %q, want %q", tc.content, tc.max, got, tc.want)
			}
			if overflow != tc.wantOverflow {
				t.Fatalf("truncateMessage(%q, %d) overflow = %d, want %d", tc.content, tc.max, overflow, tc.wantOverflow)
			}
		})
	}
}

func TestParseDurationWithDefault(t *testing.T) {
	fallback := 5 * time.Minute

	cases := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{name: "blank uses fallback", in: "  ", want: fallback},
		{name: "valid duration parsed", in: "90s", want: 90 * time.Second},
		{name: "invalid duration returns fallback and error", in: notParsable, want: fallback, wantErr: true},
		// The CRD's Pattern validation on Interval/RetryInterval
		// (feedgroup_types.go) accepts unbounded digit runs, so a value like
		// this passes admission but still overflows time.ParseDuration.
		{name: "overflowing duration returns fallback and error", in: "99999999999999999999h", want: fallback, wantErr: true},
		// "1ns" (or "0s") passes the CRD's Pattern validation, but fed
		// straight into ctrl.Result{RequeueAfter: ...} would requeue in a
		// tight loop; parseDurationWithDefault floors it instead.
		{name: "tiny duration is floored", in: "1ns", want: minParsedInterval},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDurationWithDefault(tc.in, fallback)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestInvalidIntervalError asserts both interval fields are checked (so a bad
// retryInterval is reported even on reconciles that requeue on interval), and
// that blank or valid values are not flagged. The overflow case passes the
// CRD's Pattern and MaxLength but is rejected by time.ParseDuration.
func TestInvalidIntervalError(t *testing.T) {
	cases := []struct {
		name         string
		spec         v1alpha1.FeedGroupSpec
		wantContains string // empty means no error expected
	}{
		{name: "both blank", spec: v1alpha1.FeedGroupSpec{}},
		{name: "both valid", spec: v1alpha1.FeedGroupSpec{Interval: "30m", RetryInterval: "1m"}},
		{name: "overflowing interval", spec: v1alpha1.FeedGroupSpec{Interval: overflowingInterval}, wantContains: `spec.interval: invalid duration "` + overflowingInterval + `"`},
		{name: "overflowing retryInterval", spec: v1alpha1.FeedGroupSpec{Interval: "30m", RetryInterval: overflowingInterval}, wantContains: `spec.retryInterval: invalid duration "` + overflowingInterval + `"`},
		{name: "interval reported first", spec: v1alpha1.FeedGroupSpec{Interval: notParsable, RetryInterval: notParsable}, wantContains: "spec.interval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := invalidIntervalError(&tc.spec)
			if tc.wantContains == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantContains) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantContains)
			}
		})
	}
}

// TestSetReadyCondition_InvalidIntervalPrecedence asserts InvalidInterval
// outranks the per-feed summary (the spec is wrong however the feeds fared)
// but yields to WebhookUnresolved (no feed was processed, so the webhook
// failure is the more fundamental problem).
func TestSetReadyCondition_InvalidIntervalPrecedence(t *testing.T) {
	intervalErr := invalidIntervalError(&v1alpha1.FeedGroupSpec{Interval: overflowingInterval})
	webhookErr := errors.New("secret missing")

	readyReason := func(fg *v1alpha1.FeedGroup) string {
		c := apimeta.FindStatusCondition(fg.Status.Conditions, v1alpha1.ConditionTypeReady)
		if c == nil {
			t.Fatal("Ready condition not set")
		}
		return c.Reason
	}

	failing := &v1alpha1.FeedGroup{Status: v1alpha1.FeedGroupStatus{Feeds: []v1alpha1.FeedStatus{{RSSUrl: "u", LastError: "boom"}}}}
	setReadyCondition(failing, nil, intervalErr)
	if got := readyReason(failing); got != reasonInvalidInterval {
		t.Fatalf("with a failing feed and a bad interval: reason = %q, want %q", got, reasonInvalidInterval)
	}

	both := &v1alpha1.FeedGroup{}
	setReadyCondition(both, webhookErr, intervalErr)
	if got := readyReason(both); got != "WebhookUnresolved" {
		t.Fatalf("with a webhook failure and a bad interval: reason = %q, want WebhookUnresolved", got)
	}

	// Correcting the interval clears the condition on the next reconcile.
	setReadyCondition(failing, nil, nil)
	if got := readyReason(failing); got != "FeedErrors" {
		t.Fatalf("after the interval is fixed: reason = %q, want FeedErrors", got)
	}
}

// TestJitterDuration asserts enabled=true adds up to requeueJitterMaxFactor
// extra to the duration (so FeedGroups created together with the same
// interval don't requeue in lockstep forever), while enabled=false -- used
// for Discord's rate-limit Retry-After, an externally-dictated minimum wait
// -- leaves it exact.
func TestJitterDuration(t *testing.T) {
	const base = 10 * time.Minute

	if got := jitterDuration(base, false); got != base {
		t.Fatalf("expected exact %v with jitter disabled, got %v", base, got)
	}

	for range 20 {
		got := jitterDuration(base, true)
		if got < base {
			t.Fatalf("jittered duration %v must never be shorter than base %v", got, base)
		}
		if max := time.Duration(1.1 * float64(base)); got > max {
			t.Fatalf("jittered duration %v exceeds %v (base + %.0f%%)", got, max, requeueJitterMaxFactor*100)
		}
	}
}

func TestMaxRetryCount(t *testing.T) {
	cases := []struct {
		in, want int32
	}{
		{in: -1, want: 1},
		{in: 0, want: 1},
		{in: 1, want: 1},
		{in: 5, want: 5},
	}
	for _, tc := range cases {
		if got := maxRetryCount(tc.in); got != tc.want {
			t.Fatalf("maxRetryCount(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestLimitCatchUp(t *testing.T) {
	mk := func(n int) []rss.Entry {
		entries := make([]rss.Entry, n)
		for i := range entries {
			entries[i] = rss.Entry{ID: string(rune('a' + i))}
		}
		return entries
	}

	t.Run("non-positive limit falls back to default", func(t *testing.T) {
		got := limitCatchUp(mk(10), 0)
		if len(got) != defaultCatchUpLimit {
			t.Fatalf("expected %d entries, got %d", defaultCatchUpLimit, len(got))
		}
		// Keeps the most recent (tail) entries.
		if got[len(got)-1].ID != mk(10)[9].ID {
			t.Fatalf("expected most recent entry retained, got %q", got[len(got)-1].ID)
		}
	})

	t.Run("under limit returns all", func(t *testing.T) {
		got := limitCatchUp(mk(3), 5)
		if len(got) != 3 {
			t.Fatalf("expected 3 entries, got %d", len(got))
		}
	})

	t.Run("trims to limit keeping newest", func(t *testing.T) {
		got := limitCatchUp(mk(10), 2)
		if len(got) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(got))
		}
		if got[0].ID != "i" || got[1].ID != "j" {
			t.Fatalf("expected last two entries, got %q,%q", got[0].ID, got[1].ID)
		}
	})
}

func TestPruneLastSent(t *testing.T) {
	t.Run("ties on timestamp are broken deterministically by key", func(t *testing.T) {
		const ts = "2024-01-01T00:00:00Z"
		for range 50 {
			sent := map[string]string{}
			for _, k := range []string{"e", "b", "g", "a", "f", "c", "d"} {
				sent[k] = ts
			}
			pruneLastSent(sent, 3)
			if len(sent) != 3 {
				t.Fatalf("expected 3 entries, got %d", len(sent))
			}
			for _, k := range []string{"e", "f", "g"} {
				if _, ok := sent[k]; !ok {
					t.Fatalf("expected %q to be kept, got %v", k, sent)
				}
			}
		}
	})

	t.Run("under cap unchanged", func(t *testing.T) {
		sent := map[string]string{"a": "2024-01-01T00:00:00Z", "b": "2024-01-02T00:00:00Z"}
		pruneLastSent(sent, 5)
		if len(sent) != 2 {
			t.Fatalf("expected map unchanged, got %d entries", len(sent))
		}
	})

	t.Run("drops oldest by timestamp", func(t *testing.T) {
		sent := map[string]string{
			"oldest": "2024-01-01T00:00:00Z",
			"middle": "2024-01-02T00:00:00Z",
			"newest": "2024-01-03T00:00:00Z",
		}
		pruneLastSent(sent, 2)
		if len(sent) != 2 {
			t.Fatalf("expected 2 entries after prune, got %d", len(sent))
		}
		if _, ok := sent["oldest"]; ok {
			t.Fatal("expected the oldest entry to be pruned")
		}
		if _, ok := sent["newest"]; !ok {
			t.Fatal("expected the newest entry to be retained")
		}
	})
}

func TestComputeEntryKey(t *testing.T) {
	const entryLink = "http://x"
	a := computeEntryKey(rss.Entry{ID: "1", Link: entryLink, Title: "T"})
	b := computeEntryKey(rss.Entry{ID: "1", Link: entryLink, Title: "T"})
	c := computeEntryKey(rss.Entry{ID: "2", Link: entryLink, Title: "T"})

	if a != b {
		t.Fatal("expected identical entries to produce the same key")
	}
	if a == c {
		t.Fatal("expected differing IDs to produce different keys")
	}
	if len(a) != 64 {
		t.Fatalf("expected 64-char hex sha256, got %d chars", len(a))
	}
}

func TestComputeEntryKey_IgnoresLinkAndTitleChurn(t *testing.T) {
	// A feed editing a headline or rotating a tracking parameter on an
	// otherwise-unchanged article must not look like a new entry, as long
	// as its GUID-derived ID is unchanged (the failure mode this guards
	// against: a feed like the Guardian's that live-edits articles in
	// place would otherwise get re-sent as a "new" duplicate every time the
	// headline changed).
	original := rss.Entry{ID: "churn-test-entry", Link: "https://example.com/churn", Title: "Original Title"}
	edited := rss.Entry{ID: "churn-test-entry", Link: "https://example.com/churn", Title: "Updated Title"}

	if computeEntryKey(original) != computeEntryKey(edited) {
		t.Fatal("expected entries sharing an ID to produce the same key despite a title edit")
	}
}

func TestEntryIdentity_NormalizesTrackingParamsOnURLIdentity(t *testing.T) {
	withTracking := rss.Entry{ID: "https://example.com/article?utm_source=feed&fbclid=abc&id=42"}
	clean := rss.Entry{ID: "https://example.com/article?id=42"}

	if entryIdentity(withTracking) != entryIdentity(clean) {
		t.Fatalf("expected tracking params to be stripped: got %q vs %q",
			entryIdentity(withTracking), entryIdentity(clean))
	}
}

func TestEntryIdentity_PreservesNonURLGUIDUnchanged(t *testing.T) {
	entry := rss.Entry{ID: "tag:example.com,2026:article-123"}
	if got := entryIdentity(entry); got != entry.ID {
		t.Fatalf("expected opaque GUID to pass through unchanged, got %q", got)
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

func TestCompileFilterRegex(t *testing.T) {
	cases := []struct {
		name    string
		filter  *v1alpha1.Filter
		wantRe  bool
		wantErr bool
	}{
		{name: "nil filter yields nil regex"},
		{name: "blank regex yields nil regex", filter: &v1alpha1.Filter{Regex: "  "}},
		{name: "valid regex compiled", filter: &v1alpha1.Filter{Regex: "foo.*"}, wantRe: true},
		{name: "invalid regex returns error", filter: &v1alpha1.Filter{Regex: "("}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			re, err := compileFilterRegex(tc.filter)
			if (err != nil) != tc.wantErr || (re != nil) != tc.wantRe {
				t.Fatalf("got regex %v, err %v; want regex=%v err=%v", re, err, tc.wantRe, tc.wantErr)
			}
		})
	}
}

func TestMatchesFilter(t *testing.T) {
	entry := rss.Entry{Title: "Breaking News", Description: "something happened"}

	t.Run("nil filter matches", func(t *testing.T) {
		if !matchesFilter(nil, entry, nil) {
			t.Fatal("expected nil filter to match everything")
		}
	})

	t.Run("regex non-match rejects", func(t *testing.T) {
		re, _ := compileFilterRegex(&v1alpha1.Filter{Regex: "nomatch"})
		if matchesFilter(&v1alpha1.Filter{Regex: "nomatch"}, entry, re) {
			t.Fatal("expected non-matching regex to reject entry")
		}
	})

	t.Run("keyword case-insensitive match", func(t *testing.T) {
		f := &v1alpha1.Filter{Keywords: []string{"BREAKING"}}
		if !matchesFilter(f, entry, nil) {
			t.Fatal("expected case-insensitive keyword match")
		}
	})

	t.Run("no keyword match rejects", func(t *testing.T) {
		f := &v1alpha1.Filter{Keywords: []string{"weather"}}
		if matchesFilter(f, entry, nil) {
			t.Fatal("expected no keyword match to reject")
		}
	})

	t.Run("blank keywords skipped, empty keyword list matches", func(t *testing.T) {
		f := &v1alpha1.Filter{Keywords: []string{"  "}}
		// All keywords are blank/skipped, so none match.
		if matchesFilter(f, entry, nil) {
			t.Fatal("expected all-blank keywords to match nothing")
		}
	})
}

func TestResolveEmbedSpec(t *testing.T) {
	feedEmbed := &v1alpha1.EmbedSpec{Color: "#feed00"}
	groupEmbed := &v1alpha1.EmbedSpec{Color: "#9009ff"}

	t.Run("feed embed takes precedence", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{Spec: v1alpha1.FeedGroupSpec{Embed: groupEmbed}}
		got := resolveEmbedSpec(fg, &v1alpha1.FeedSpec{Embed: feedEmbed})
		if got != feedEmbed {
			t.Fatal("expected feed embed to take precedence")
		}
	})

	t.Run("falls back to group embed", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{Spec: v1alpha1.FeedGroupSpec{Embed: groupEmbed}}
		got := resolveEmbedSpec(fg, &v1alpha1.FeedSpec{})
		if got != groupEmbed {
			t.Fatal("expected fallback to group embed")
		}
	})

	t.Run("nil when neither set", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{}
		if got := resolveEmbedSpec(fg, &v1alpha1.FeedSpec{}); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})
}

func TestCompileMessageTemplate_Precedence(t *testing.T) {
	t.Run("feed format wins", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{Spec: v1alpha1.FeedGroupSpec{Format: "group {{.Title}}"}}
		tmpl, err := compileMessageTemplate(fg, &v1alpha1.FeedSpec{Format: "feed {{.Title}}"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var sb strings.Builder
		if err := tmpl.Execute(&sb, map[string]string{"Title": "X"}); err != nil {
			t.Fatalf("execute error: %v", err)
		}
		if sb.String() != "feed X" {
			t.Fatalf("expected feed format used, got %q", sb.String())
		}
	})

	t.Run("falls back to default when both blank", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{}
		tmpl, err := compileMessageTemplate(fg, &v1alpha1.FeedSpec{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tmpl == nil {
			t.Fatal("expected a compiled default template")
		}
	})

	t.Run("invalid template returns error", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{}
		if _, err := compileMessageTemplate(fg, &v1alpha1.FeedSpec{Format: "{{.Unclosed"}); err == nil {
			t.Fatal("expected parse error for invalid template, got nil")
		}
	})
}

func TestRenderTemplate_AuthorAndCategories(t *testing.T) {
	tmpl, err := compileTemplate("test", "{{.Author}} | {{.Categories}}", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entry := rss.Entry{Author: "Jane Doe", Categories: []string{"Go", "Kubernetes"}}
	got, _, err := renderTemplate(tmpl, entry, maxDiscordMessageLength)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "Jane Doe | Go, Kubernetes"; got != want {
		t.Fatalf("renderTemplate() = %q, want %q", got, want)
	}

	t.Run("empty author and categories render blank", func(t *testing.T) {
		got, _, err := renderTemplate(tmpl, rss.Entry{}, maxDiscordMessageLength)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := " | "; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})
}

// TestRenderTemplate_Published asserts a date-less entry (zero Published)
// renders an empty {{.Published}} rather than "0001-01-01T00:00:00Z", while a
// real date is still formatted as RFC 3339 UTC.
func TestRenderTemplate_Published(t *testing.T) {
	tmpl, err := compileTemplate("test", "[{{.Published}}]", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Run("zero published renders empty", func(t *testing.T) {
		got, _, err := renderTemplate(tmpl, rss.Entry{}, maxDiscordMessageLength)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "[]"; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})

	t.Run("non-zero published unchanged", func(t *testing.T) {
		entry := rss.Entry{Published: time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)}
		got, _, err := renderTemplate(tmpl, entry, maxDiscordMessageLength)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "[2024-05-06T07:08:09Z]"; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})
}

// TestRenderTemplate_NewFields covers the placeholders added after the
// original six: Content, Image, FeedTitle and PublishedUnix.
func TestRenderTemplate_NewFields(t *testing.T) {
	render := func(t *testing.T, format string, entry rss.Entry) string {
		t.Helper()
		tmpl, err := compileTemplate("test", format, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, _, err := renderTemplate(tmpl, entry, maxDiscordMessageLength)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return got
	}

	t.Run("Content is HTML-stripped", func(t *testing.T) {
		entry := rss.Entry{Content: "<p>Full <b>body</b> &amp; more</p><script>alert(1)</script>"}
		if got, want := render(t, "{{.Content}}", entry), "Full body & more"; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})

	t.Run("Content is distinct from Description", func(t *testing.T) {
		entry := rss.Entry{Description: shortContent, Content: "long"}
		if got, want := render(t, "{{.Description}}/{{.Content}}", entry), shortContent+"/long"; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})

	t.Run("Image passes http(s) URLs through", func(t *testing.T) {
		entry := rss.Entry{Image: "https://cdn.example.com/a.jpg"}
		if got, want := render(t, "{{.Image}}", entry), "https://cdn.example.com/a.jpg"; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})

	t.Run("Image with unsafe scheme is blanked", func(t *testing.T) {
		for _, image := range []string{"javascript:void(0)", "data:text/html,<script>x</script>", "ftp://example.com/a.jpg"} {
			if got := render(t, "[{{.Image}}]", rss.Entry{Image: image}); got != "[]" {
				t.Fatalf("Image %q rendered %q, want it blanked", image, got)
			}
		}
	})

	t.Run("FeedTitle is HTML-stripped", func(t *testing.T) {
		entry := rss.Entry{FeedTitle: "<b>Example</b> &amp; Co"}
		if got, want := render(t, "{{.FeedTitle}}", entry), "Example & Co"; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})

	t.Run("PublishedUnix is Unix seconds", func(t *testing.T) {
		// A non-UTC zone: the Unix value is zone-independent.
		published := time.Date(2024, 5, 6, 9, 8, 9, 0, time.FixedZone("x", 2*3600))
		got := render(t, "<t:{{.PublishedUnix}}:R>", rss.Entry{Published: published})
		if want := discordRelativeTS; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})

	t.Run("zero PublishedUnix is 0 and guardable", func(t *testing.T) {
		const format = "{{if .PublishedUnix}}<t:{{.PublishedUnix}}:R>{{end}}"
		if got := render(t, "<t:{{.PublishedUnix}}:R>", rss.Entry{}); got != "<t:0:R>" {
			t.Fatalf("renderTemplate() = %q, want %q", got, "<t:0:R>")
		}
		if got := render(t, format, rss.Entry{}); got != "" {
			t.Fatalf("guarded template rendered %q for an undated entry, want empty", got)
		}
		dated := rss.Entry{Published: time.Unix(1714979289, 0)}
		if got, want := render(t, format, dated), discordRelativeTS; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})

	t.Run("all new fields empty on a zero entry", func(t *testing.T) {
		if got, want := render(t, "[{{.Content}}|{{.Image}}|{{.FeedTitle}}]", rss.Entry{}), "[||]"; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})
}

// TestBuildDiscordMessage_EmbedTimestamp asserts an entry without a publish
// date leaves the embed timestamp empty (discord.Embed omits it from the
// payload) instead of sending year 0001, and that real dates are unchanged.
func TestBuildDiscordMessage_EmbedTimestamp(t *testing.T) {
	feedGroup := &v1alpha1.FeedGroup{}
	feed := &v1alpha1.FeedSpec{}
	embedSpec := &v1alpha1.EmbedSpec{Enabled: true}
	descriptionTmpl, err := compileTemplate("description", "", "{{.Description}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cases := []struct {
		name      string
		published time.Time
		want      string
	}{
		{name: "zero published", want: ""},
		{name: "non-zero published", published: time.Date(2024, 5, 6, 9, 8, 9, 0, time.FixedZone("x", 2*3600)), want: "2024-05-06T07:08:09Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := buildDiscordMessage(feedGroup, embedSpec, nil, descriptionTmpl, nil, feed, rss.Entry{Title: "T", Published: tc.published})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if msg.Embed == nil {
				t.Fatal("expected an embed to be set")
			}
			if msg.Embed.Timestamp != tc.want {
				t.Fatalf("msg.Embed.Timestamp = %q, want %q", msg.Embed.Timestamp, tc.want)
			}
		})
	}
}

// TestBuildDiscordMessage_EmbedImageStyle asserts the entry image goes to the
// small thumbnail by default and to the large image slot for imageStyle
// "image", and that non-http(s) image URLs are dropped either way.
func TestBuildDiscordMessage_EmbedImageStyle(t *testing.T) {
	descriptionTmpl, err := compileTemplate("description", "", "{{.Description}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cases := []struct {
		name, style, image   string
		wantThumb, wantLarge string
	}{
		{name: "default is thumbnail", image: imageFixtureURL, wantThumb: imageFixtureURL},
		{name: "explicit thumbnail", style: "thumbnail", image: imageFixtureURL, wantThumb: imageFixtureURL},
		{name: "image style", style: "image", image: imageFixtureURL, wantLarge: imageFixtureURL},
		{name: "unsafe scheme dropped", style: "image", image: jsAlertURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &v1alpha1.EmbedSpec{Enabled: true, ImageStyle: tc.style}
			msg, err := buildDiscordMessage(&v1alpha1.FeedGroup{}, spec, nil, descriptionTmpl, nil, &v1alpha1.FeedSpec{}, rss.Entry{Title: "T", Image: tc.image})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if msg.Embed.ThumbnailURL != tc.wantThumb || msg.Embed.ImageURL != tc.wantLarge {
				t.Fatalf("thumbnail=%q image=%q, want %q / %q", msg.Embed.ThumbnailURL, msg.Embed.ImageURL, tc.wantThumb, tc.wantLarge)
			}
		})
	}
}

func TestRenderTemplate_StripsHTMLFromTitle(t *testing.T) {
	tmpl, err := compileTemplate("test", "{{.Title}}", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entry := rss.Entry{Title: "<b>Breaking</b> News &amp; Views"}
	got, _, err := renderTemplate(tmpl, entry, maxDiscordMessageLength)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "Breaking News & Views"; got != want {
		t.Fatalf("renderTemplate() = %q, want %q", got, want)
	}
}

func TestBuildDiscordMessage_EmbedTitleStripsHTML(t *testing.T) {
	feedGroup := &v1alpha1.FeedGroup{}
	feed := &v1alpha1.FeedSpec{}
	embedSpec := &v1alpha1.EmbedSpec{Enabled: true}

	descriptionTmpl, err := compileTemplate("description", "", "{{.Description}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entry := rss.Entry{Title: "<b>Breaking</b> News"}
	msg, err := buildDiscordMessage(feedGroup, embedSpec, nil, descriptionTmpl, nil, feed, entry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Embed == nil {
		t.Fatal("expected an embed to be set")
	}
	if want := "Breaking News"; msg.Embed.Title != want {
		t.Fatalf("msg.Embed.Title = %q, want %q", msg.Embed.Title, want)
	}
}

// TestBuildDiscordMessage_EmbedDescriptionRenderError asserts a description
// template that parses but fails at execution (referencing a field absent
// from renderTemplate's render data) surfaces as an error instead of a
// blank/partial embed.
func TestBuildDiscordMessage_EmbedDescriptionRenderError(t *testing.T) {
	feedGroup := &v1alpha1.FeedGroup{}
	feed := &v1alpha1.FeedSpec{}
	embedSpec := &v1alpha1.EmbedSpec{Enabled: true}

	descriptionTmpl, err := compileTemplate("description", "", "{{.DoesNotExist}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := buildDiscordMessage(feedGroup, embedSpec, nil, descriptionTmpl, nil, feed, rss.Entry{}); err == nil {
		t.Fatal("expected an error from a description template that fails at execution")
	}
}

// TestBuildDiscordMessage_ForumThreadNameRenderError asserts a forum thread
// name template that parses but fails at execution surfaces as an error,
// distinct from the content/description render-error paths.
func TestBuildDiscordMessage_ForumThreadNameRenderError(t *testing.T) {
	feedGroup := &v1alpha1.FeedGroup{}
	feed := &v1alpha1.FeedSpec{ForumThreadName: "{{.DoesNotExist}}"}

	contentTmpl, err := compileTemplate("content", "{{.Title}}", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	threadNameTmpl, err := compileTemplate("threadName", "{{.DoesNotExist}}", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := buildDiscordMessage(feedGroup, nil, contentTmpl, nil, threadNameTmpl, feed, rss.Entry{}); err == nil {
		t.Fatal("expected an error from a forum thread name template that fails at execution")
	}
}

// TestBuildDiscordMessage_EmptyRenderIsError asserts a template that renders
// to blank content or a blank forum thread name (here: only {{.Published}},
// for an entry with no date) is reported as a render error, so the
// render-error path can skip the entry, rather than producing a message
// Discord would reject on every retry.
func TestBuildDiscordMessage_EmptyRenderIsError(t *testing.T) {
	feedGroup := &v1alpha1.FeedGroup{}
	publishedOnly, err := compileTemplate("content", " {{.Published}} ", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	title, err := compileTemplate("content", "{{.Title}}", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entry := rss.Entry{Title: "T"}

	t.Run("blank content", func(t *testing.T) {
		if _, err := buildDiscordMessage(feedGroup, nil, publishedOnly, nil, nil, &v1alpha1.FeedSpec{}, entry); err == nil {
			t.Fatal("expected an error for content that renders blank")
		}
	})

	t.Run("blank forum thread name", func(t *testing.T) {
		feed := &v1alpha1.FeedSpec{ForumThreadName: "{{.Published}}"}
		if _, err := buildDiscordMessage(feedGroup, nil, title, nil, publishedOnly, feed, entry); err == nil {
			t.Fatal("expected an error for a forum thread name that renders blank")
		}
	})

	t.Run("dated entry still renders", func(t *testing.T) {
		dated := rss.Entry{Title: "T", Published: time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)}
		msg, err := buildDiscordMessage(feedGroup, nil, publishedOnly, nil, nil, &v1alpha1.FeedSpec{}, dated)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "2024-05-06T07:08:09Z"; msg.Content != want {
			t.Fatalf("msg.Content = %q, want %q", msg.Content, want)
		}
	})
}

// TestBuildDiscordMessage_ForumThreadIDTakesPrecedenceOverThreadName asserts
// an explicit ForumThreadID (posting into an existing thread) wins over a
// configured ForumThreadName template (which creates a new thread), even
// when both are set.
func TestBuildDiscordMessage_ForumThreadIDTakesPrecedenceOverThreadName(t *testing.T) {
	feedGroup := &v1alpha1.FeedGroup{}
	feed := &v1alpha1.FeedSpec{ForumThreadID: "123456", ForumThreadName: "{{.Title}}"}

	contentTmpl, err := compileTemplate("content", "{{.Title}}", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	threadNameTmpl, err := compileTemplate("threadName", "{{.Title}}", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msg, err := buildDiscordMessage(feedGroup, nil, contentTmpl, nil, threadNameTmpl, feed, rss.Entry{Title: "Hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.ThreadID != "123456" {
		t.Fatalf("msg.ThreadID = %q, want %q", msg.ThreadID, "123456")
	}
	if msg.ThreadName != "" {
		t.Fatalf("expected ThreadName to stay empty when ForumThreadID is set, got %q", msg.ThreadName)
	}
}

// URL-looking IDs that don't parse, or parse to a non-http(s) scheme, are
// returned unchanged rather than normalized.
func TestNormalizeIdentity_UnparsableOrNonHTTPURLUnchanged(t *testing.T) {
	for _, id := range []string{"http://[::1", "httpx://example.com/a#frag", "http"} {
		if got := normalizeIdentity(id); got != id {
			t.Errorf("normalizeIdentity(%q) = %q, want unchanged", id, got)
		}
	}
}

// A forum thread-name template that fails at execution is a render error,
// like a failing content template, so the entry goes through retry/skip.
func TestBuildDiscordMessage_ThreadNameRenderError(t *testing.T) {
	threadNameTmpl, err := compileTemplate("thread", "{{.DoesNotExist}}", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	contentTmpl, err := compileTemplate("content", "{{.Title}}", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = buildDiscordMessage(&v1alpha1.FeedGroup{}, nil, contentTmpl, nil, threadNameTmpl, &v1alpha1.FeedSpec{}, rss.Entry{Title: "T"})
	if err == nil {
		t.Fatal("expected a render error from the thread name template")
	}
}
