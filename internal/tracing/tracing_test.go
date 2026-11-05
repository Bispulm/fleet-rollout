package tracing_test

import (
	"context"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/Bispulm/fleet-rollout/internal/tracing"
)

func TestTracingIsOffWithoutAnEndpoint(t *testing.T) {
	provider, stop, err := tracing.Setup(t.Context(), "rolloutd", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(noop.TracerProvider); !ok {
		t.Fatalf("provider = %T, want one that records nothing", provider)
	}
	if err := stop(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestTracingExportsWithAnEndpoint(t *testing.T) {
	env := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:1"}
	provider, stop, err := tracing.Setup(t.Context(), "rolloutd", func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(*sdktrace.TracerProvider); !ok {
		t.Fatalf("provider = %T, want an exporting one", provider)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_ = stop(ctx) // Nothing listens there, so a flush can fail. Stopping must still return.
}
