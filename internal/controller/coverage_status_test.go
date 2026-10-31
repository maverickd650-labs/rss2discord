package controller

import (
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
)

// Diagnostics shared by the conversion-failure tests below.
const (
	msgWantConversionError = "expected a conversion error"
	msgWantTimeParseError  = "expected a *time.ParseError in the chain, got %T: %v"
)

// outOfRangeStatus carries a condition time that metav1.Time marshals without
// complaint but cannot read back (RFC 3339 has four-digit years), so the
// JSON round trip fails while decoding into the apply configuration. That is
// the only way the conversion fails: the marshal step has no failing input,
// as every field of FeedGroupStatus is a plain string, number, bool or time.
func outOfRangeStatus() v1alpha1.FeedGroupStatus {
	return v1alpha1.FeedGroupStatus{
		Conditions: []metav1.Condition{{
			Type:               v1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionTrue,
			Reason:             "TimeOutOfRange",
			LastTransitionTime: metav1.NewTime(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)),
		}},
	}
}

func TestStatusApplyConfiguration_DecodeFailureIsWrapped(t *testing.T) {
	status := outOfRangeStatus()

	ac, err := statusApplyConfiguration(&status)
	if err == nil {
		t.Fatal(msgWantConversionError)
	}
	if ac != nil {
		t.Fatalf("got apply configuration %v alongside the error", ac)
	}
	if _, ok := errors.AsType[*time.ParseError](err); !ok {
		t.Fatalf(msgWantTimeParseError, err, err)
	}
	const wantPrefix = "converting FeedGroup status to an apply configuration: "
	if got := err.Error(); !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("Error() = %q, want prefix %q", got, wantPrefix)
	}
}

// applyStatus must return a conversion error unchanged and before touching
// the API, so a reconciler with no client is enough to exercise it.
func TestApplyStatus_ReturnsConversionErrorWithoutWriting(t *testing.T) {
	fg := &v1alpha1.FeedGroup{Status: outOfRangeStatus()}
	fg.Name, fg.Namespace = "fg", "ns"

	err := (&FeedGroupReconciler{}).applyStatus(t.Context(), fg)
	if err == nil {
		t.Fatal(msgWantConversionError)
	}
	if _, ok := errors.AsType[*time.ParseError](err); !ok {
		t.Fatalf(msgWantTimeParseError, err, err)
	}
}
