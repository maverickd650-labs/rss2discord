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

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
)

func TestSetReadyCondition_ClampsWebhookErrorMessage(t *testing.T) {
	webhookErr := errors.New(strings.Repeat("k", 10*maxConditionMessageBytes))
	fg := &v1beta1.FeedGroup{}
	setReadyCondition(fg, webhookErr, nil)

	cond := apimeta.FindStatusCondition(fg.Status.Conditions, v1beta1.ConditionTypeReady)
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
		if !isCleanClamp(got, budget) {
			t.Errorf("clampText(budget=%d) split a rune or overshot: len=%d valid=%v %q", budget, len(got), utf8.ValidString(got), got)
		}
	}
}

// isCleanClamp reports whether got fits the byte budget, is valid UTF-8 and
// ends in the truncation ellipsis.
func isCleanClamp(got string, budget int) bool {
	return len(got) <= budget && utf8.ValidString(got) && strings.HasSuffix(got, "…")
}

// TestFeedGroupStatusWorstCaseStaysUnderBudget fills every bounded status
// field to its cap for a maximal (50-feed) group and checks the serialized
// status leaves room in etcd's ~1.5MB object limit for the spec.
func TestFeedGroupStatusWorstCaseStaysUnderBudget(t *testing.T) {
	var status v1beta1.FeedGroupStatus
	for i := range 50 {
		fs := v1beta1.FeedStatus{
			Name: testFeedName("https://example.com/" + strings.Repeat("u", 2048-21) + strconv.Itoa(i%10)), URL: "https://example.com/" + strings.Repeat("u", 2048-21) + strconv.Itoa(i%10),
			LastChecked:   metav1TimeRFC3339("2026-01-01T00:00:00Z"),
			LastSeenEntry: clampWatermark(strings.Repeat("s", 5000)),
			LastError:     clampText(strings.Repeat("e", 40000), maxStatusTextBytes),
			ETag:          strings.Repeat("g", 256),
			LastModified:  strings.Repeat("m", 64),
			BackoffUntil:  metav1TimeRFC3339("2026-01-01T00:00:00Z"),
			LastSent:      map[string]metav1.Time{},
		}
		for j := range maxLastSentPerFeed {
			fs.LastSent[hashIdentity(strconv.Itoa(j))] = ts(testReconcileTime)
		}
		for _, typ := range []string{v1beta1.FeedConditionTypeReachable, v1beta1.FeedConditionTypeDelivered} {
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

// TestSetFeedCondition_IgnoresObservedGenerationOnlyChange walks one
// condition through a sequence of writes: only a change to the status,
// reason or message counts as semantic, never ObservedGeneration alone.
func TestSetFeedCondition_IgnoresObservedGenerationOnlyChange(t *testing.T) {
	const differentInvalidFilter = "different invalid filter"
	oversized := strings.Repeat("x", maxConditionMessageBytes+64)
	fs := &v1beta1.FeedStatus{}

	steps := []struct {
		name        string
		status      metav1.ConditionStatus
		reason      string
		message     string
		generation  int64
		wantChange  bool
		assertClamp bool
	}{
		{"new condition", metav1.ConditionFalse, reasonConfigError, oversized, 1, true, false},
		{"observed generation only", metav1.ConditionFalse, reasonConfigError, oversized, 2, false, true},
		{"message change", metav1.ConditionFalse, reasonConfigError, differentInvalidFilter, 2, true, false},
		{"status change", metav1.ConditionTrue, reasonConfigError, differentInvalidFilter, 2, true, false},
		{"reason change", metav1.ConditionTrue, reasonMessageSent, differentInvalidFilter, 2, true, false},
	}
	for _, step := range steps {
		got := setFeedCondition(fs, v1beta1.FeedConditionTypeDelivered, step.status, step.reason, step.message, step.generation)
		if got != step.wantChange {
			t.Fatalf("%s: setFeedCondition changed = %v, want %v", step.name, got, step.wantChange)
		}
		if step.assertClamp {
			assertClampedAtGeneration(t, fs, step.generation)
		}
	}
}

// assertClampedAtGeneration asserts the Delivered condition carries the given
// ObservedGeneration and a message within the condition budget.
func assertClampedAtGeneration(t *testing.T, fs *v1beta1.FeedStatus, generation int64) {
	t.Helper()
	condition := apimeta.FindStatusCondition(fs.Conditions, v1beta1.FeedConditionTypeDelivered)
	if condition == nil {
		t.Fatal("expected a Delivered condition")
	}
	if condition.ObservedGeneration != generation || len(condition.Message) > maxConditionMessageBytes {
		t.Fatalf("condition = %+v, want ObservedGeneration %d and clamped Message", condition, generation)
	}
}

func TestSetReadyCondition_IgnoresObservedGenerationOnlyChange(t *testing.T) {
	fg := &v1beta1.FeedGroup{}
	fg.Generation = 1
	intervalErr := errors.New("invalid interval")
	if !setReadyCondition(fg, nil, intervalErr) {
		t.Fatal("expected a newly added Ready condition to count as changed")
	}
	fg.Generation = 2
	if setReadyCondition(fg, nil, intervalErr) {
		t.Fatal("ObservedGeneration-only Ready update must not count as semantic change")
	}
	condition := apimeta.FindStatusCondition(fg.Status.Conditions, v1beta1.ConditionTypeReady)
	if condition == nil || condition.ObservedGeneration != 2 {
		t.Fatalf("Ready condition = %+v, want ObservedGeneration 2", condition)
	}
}
