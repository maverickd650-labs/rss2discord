package rss

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// parseErrorPrefix starts every ParseError message; the wrapped cause's own
// wording is not pinned.
const parseErrorPrefix = "parse feed: "

func TestParseError_ErrorAndUnwrap(t *testing.T) {
	cause := errors.New("boom")
	err := error(&ParseError{Err: cause})

	if got, want := err.Error(), "parse feed: boom"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, cause) {
		t.Fatal("errors.Is must find the wrapped cause")
	}
	if got := errors.Unwrap(err); got != cause {
		t.Fatalf("Unwrap() = %v, want %v", got, cause)
	}
}

func TestParseError_ChainsThroughParseFeed(t *testing.T) {
	_, _, err := parseFeed(nil)
	pe, ok := errors.AsType[*ParseError](err)
	if !ok {
		t.Fatalf("expected *ParseError, got %T: %v", err, err)
	}
	if got := pe.Error(); !strings.HasPrefix(got, parseErrorPrefix) {
		t.Fatalf("Error() = %q, want prefix %q", got, parseErrorPrefix)
	}
	if !errors.Is(err, io.EOF) {
		t.Fatal("an empty body must unwrap to io.EOF")
	}

	_, _, err = parseFeed([]byte(`<html></html>`))
	unrecognized, ok := errors.AsType[*UnrecognizedFormatError](err)
	if !ok {
		t.Fatalf("expected *UnrecognizedFormatError in chain, got %T: %v", err, err)
	}
	if unrecognized.Root != "html" {
		t.Fatalf("Root = %q, want html", unrecognized.Root)
	}
	if got := err.Error(); !strings.HasPrefix(got, parseErrorPrefix) {
		t.Fatalf("Error() = %q, want prefix %q", got, parseErrorPrefix)
	}
}

func TestInvalidURLError_ErrorAndUnwrap(t *testing.T) {
	t.Run("synthetic", func(t *testing.T) {
		cause := errors.New("bad")
		err := error(&InvalidURLError{URL: "x", Msg: "the message", Err: cause})
		if got, want := err.Error(), "the message"; got != want {
			t.Fatalf("Error() = %q, want %q", got, want)
		}
		if !errors.Is(err, cause) {
			t.Fatal("errors.Is must find the wrapped cause")
		}
		if got := errors.Unwrap(err); got != cause {
			t.Fatalf("Unwrap() = %v, want %v", got, cause)
		}
	})

	t.Run("without cause", func(t *testing.T) {
		err := error(&InvalidURLError{Msg: "m"})
		if got := errors.Unwrap(err); got != nil {
			t.Fatalf("Unwrap() = %v, want nil", got)
		}
	})

	cases := []struct {
		name string
		url  string
		// wantPrefix is the client's own message; an unparsable URL's message
		// goes on to quote net/url's diagnostic, which is not pinned.
		wantPrefix string
		wantCause  bool
	}{
		{"blank url", "  ", "feed URL is empty", false},
		{"unparsable", "://bad-url", `invalid feed URL "://bad-url": `, true},
		{"scheme", "ftp://example.com/f", `unsupported feed URL scheme "ftp": only http/https are allowed`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClient(nil).FetchEntries(t.Context(), tc.url, CacheValidators{})
			invalid, ok := errors.AsType[*InvalidURLError](err)
			if !ok {
				t.Fatalf("expected *InvalidURLError, got %T: %v", err, err)
			}
			if got := err.Error(); !strings.HasPrefix(got, tc.wantPrefix) {
				t.Fatalf("Error() = %q, want prefix %q", got, tc.wantPrefix)
			}
			if tc.wantCause {
				if _, ok := errors.AsType[*url.Error](err); !ok {
					t.Fatalf("expected *url.Error in the chain of %v", err)
				}
			} else if invalid.Unwrap() != nil {
				t.Fatalf("Unwrap() = %v, want nil", invalid.Unwrap())
			}
		})
	}
}

func TestFetchEntries_RejectsNilContext(t *testing.T) {
	// http.NewRequestWithContext refuses a nil context; the error must be
	// returned as is, before any request is attempted.
	var ctx context.Context
	_, err := NewClient(nil).FetchEntries(ctx, "http://example.com/feed.xml", CacheValidators{})
	if err == nil {
		t.Fatal("expected an error for a nil context")
	}
	if _, ok := errors.AsType[*InvalidURLError](err); ok {
		t.Fatalf("a request-construction failure is not an InvalidURLError: %v", err)
	}
	// Pin the error to the one http.NewRequestWithContext produces, so a
	// different failure or an added wrapper fails the test.
	_, want := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/feed.xml", nil)
	if want == nil || err.Error() != want.Error() || errors.Unwrap(err) != nil {
		t.Fatalf("err = %q (unwraps to %v), want the unwrapped request-construction error %q", err, errors.Unwrap(err), want)
	}
}
