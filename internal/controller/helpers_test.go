package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/maverickd650-labs/rss2discord/internal/discord"
)

// metav1TimeRFC3339 parses an RFC3339 timestamp string into a *metav1.Time.
// It panics on malformed fixture input rather than silently returning a zero time.
func metav1TimeRFC3339(s string) *metav1.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic("invalid RFC3339 timestamp " + s + ": " + err.Error())
	}
	mt := metav1.NewTime(t)
	return &mt
}

// timePtrEqual reports whether two *metav1.Time pointers represent the same time.
func timePtrEqual(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(b)
}

// fakeApplyClient returns a controller client that intercepts SubResourceApply calls
// without performing real cluster API writes, allowing settlePass to execute cleanly in unit tests.
func fakeApplyClient() client.Client {
	return interceptor.NewClient(fake.NewClientBuilder().Build(), interceptor.Funcs{
		SubResourceApply: func(ctx context.Context, client client.Client, subResourceName string, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
			return nil
		},
	})
}

// http500Status is the HTTP status string for a 500 Internal Server Error response,
// shared across test cases to satisfy goconst.
const (
	http500Status = "500 Internal Server Error"
	testTitle     = "Title"
	nonHTTPURL    = "ftp://example.com"
)

// errInternalServer is a common 500 error used in fakeSender setups.
var errInternalServer = &discord.HTTPStatusError{
	StatusCode: http.StatusInternalServerError,
	Status:     http500Status,
}

// overflowingInterval passes the CRD's Pattern and MaxLength yet is rejected
// by time.ParseDuration (overflow); notParsable is rejected outright.
const (
	overflowingInterval = "9999999999999h"
	notParsable         = "nope"
)

// shortContent is reused across test cases below.
const shortContent = "short"

// Shared fixtures that several tests below (and fuzz_test.go) repeat.
const (
	jsAlertURL        = "javascript:alert(1)"
	discordRelativeTS = "<t:1714979289:R>"
	imageFixtureURL   = "https://x/a.png"
)

// excerptText is reused across the "continue reading" stripHTML test cases
// below.
const excerptText = "Some excerpt text"

// Template sources shared across tests.
const (
	tmplTitle        = "{{.Title}}"
	tmplUnclosed     = "{{.Unclosed"
	tmplPublished    = "{{.Published}}"
	testDesc         = "Desc"
	testArticleURL   = "https://example.com/a"
	tmplMissingField = "{{.DoesNotExist}}"
	tmplSafeText     = "safe"
)

// testFeedName is the FeedSpec/FeedStatus name tests give the feed at url: a
// DNS label derived from the URL alone, so a spec and a status built from the
// same URL match by name the way production feeds do.
func testFeedName(url string) string {
	sum := sha256.Sum256([]byte(url))
	return "feed-" + hex.EncodeToString(sum[:6])
}

// ts parses an RFC 3339 timestamp for a LastSent value, panicking on a
// malformed literal.
func ts(rfc3339 string) metav1.Time {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		panic(err)
	}
	return metav1.NewTime(t)
}

// expectf reports a non-fatal failure when ok is false. It keeps the
// condition of a check out of an if statement, so a test that states many
// expectations stays flat.
func expectf(t testing.TB, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Errorf(format, args...)
	}
}

// requiref is expectf that stops the test on failure, for a precondition the
// rest of the test dereferences. Its arguments are evaluated before the call,
// so they must be safe to evaluate when ok is false.
func requiref(t testing.TB, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}
