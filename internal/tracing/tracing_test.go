package tracing_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"

	"github.com/noyitz/ai-gateway-metering-service/internal/tracing"
)

func TestInit_NoopWhenDisabled(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if tracing.Enabled() {
		t.Fatal("expected Enabled() == false when endpoint is empty")
	}
	shutdown, err := tracing.Init(context.Background(), "test", "dev")
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown returned error: %v", err)
	}
}

func TestInit_EnabledInstallsProvider(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	if !tracing.Enabled() {
		t.Fatal("expected Enabled() == true when endpoint is set")
	}
	shutdown, err := tracing.Init(context.Background(), "test", "dev")
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	defer shutdown(context.Background())

	_, span := otel.Tracer("t").Start(context.Background(), "x")
	if !span.SpanContext().IsValid() {
		t.Fatal("expected a valid span context from the installed provider")
	}
	span.End()
}
