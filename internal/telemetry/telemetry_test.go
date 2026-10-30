package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSetup_BuildsWorkingProvider(t *testing.T) {
	// otlptracegrpc.New dials lazily (non-blocking), so this succeeds
	// without a live collector; the exercise here is that Setup wires up a
	// usable TracerProvider and a shutdown func that doesn't hang or panic.
	tp, shutdown, err := Setup(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Setup returned error: %v", err)
	}
	if tp == nil {
		t.Fatal("expected a non-nil TracerProvider")
	}

	_, span := tp.Tracer("test").Start(t.Context(), "span")
	span.End()

	// With no collector listening, flushing the span on shutdown is
	// expected to fail once the context deadline is hit rather than hang
	// forever; a short deadline here just proves Setup wired up a real,
	// well-behaved shutdown func instead of asserting network success.
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	_ = shutdown(ctx)
}

func TestHTTPTransportWrap_InstrumentsWithoutBreakingRequests(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	// Client must be called before srv.URL is read: it sets URL.
	base := srv.Client().Transport

	tp, shutdown, err := Setup(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Setup returned error: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()

	wrap := HTTPTransportWrap(tp)
	client := &http.Client{Transport: wrap(base)}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("request through wrapped transport failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}
