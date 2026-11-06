// Package tracing bootstraps OpenTelemetry tracing. It is active only when
// OTEL_EXPORTER_OTLP_ENDPOINT is set; otherwise every function is a no-op and
// the service behaves exactly as before.
package tracing

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Enabled reports whether tracing should be wired. Callers use it to gate
// instrumentation so disabled builds add zero overhead.
func Enabled() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
}

// Init installs the global tracer provider and W3C propagator when enabled.
// The returned shutdown must be called at process exit to flush spans; when
// disabled it is a no-op. serviceName/serviceVersion are defaults that
// OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES override.
func Init(ctx context.Context, serviceName, serviceVersion string) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if !Enabled() {
		return noop, nil
	}

	// otlptracehttp.New reads OTEL_EXPORTER_OTLP_ENDPOINT/PROTOCOL from env.
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}

	// Schemaless defaults first so resource.Default() (env detector) wins on
	// conflicts, letting OTEL_SERVICE_NAME override the code default.
	res, err := resource.Merge(
		resource.NewSchemaless(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(serviceVersion),
		),
		resource.Default(),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tp.Shutdown, nil
}
