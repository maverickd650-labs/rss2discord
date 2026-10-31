package telemetry

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
)

// grpc.NewClient parses the endpoint as a dns:/// target, so an endpoint that
// is not a valid URL path fails exporter creation; Setup must report it
// without installing a global provider.
func TestSetup_RejectsUnusableEndpoint(t *testing.T) {
	for name, endpoint := range map[string]string{
		"bad escape":      "%zz",
		"control byte":    "collector\x00:4317",
		"newline in host": "collector\n:4317",
	} {
		t.Run(name, func(t *testing.T) {
			before := otel.GetTracerProvider()

			tp, shutdown, err := Setup(t.Context(), endpoint)
			if err == nil {
				t.Fatal("expected an error")
			}
			if tp != nil || shutdown != nil {
				t.Fatalf("got tp=%v shutdown=%v alongside the error, want nil", tp, shutdown != nil)
			}
			const wantPrefix = "creating OTLP trace exporter: "
			if got := err.Error(); !strings.HasPrefix(got, wantPrefix) {
				t.Fatalf("Error() = %q, want prefix %q", got, wantPrefix)
			}
			if _, ok := errors.AsType[*url.Error](err); !ok {
				t.Fatalf("expected a *url.Error in the chain of %v", err)
			}
			if otel.GetTracerProvider() != before {
				t.Fatal("a failed Setup replaced the global TracerProvider")
			}
		})
	}
}
