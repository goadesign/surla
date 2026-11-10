// This file installs standalone-server telemetry. Embedded control planes keep
// ownership of their host's OpenTelemetry provider and exporters.
package main

import (
	"context"
	"fmt"
	"io"

	"github.com/goadesign/surla/internal/traceview"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// cp1FailurePrinter writes one operator line for a failed CP1 operation. SDK simple
// processing serializes writes. This operator-owned output includes recorded
// fault evidence; it is separate from the caller-safe diagnostic history.
type cp1FailurePrinter struct {
	// output is the operator's terminal, supplied by the composition root.
	output io.Writer
}

// ExportSpans prints the safe summary and original fault evidence for failed
// CP1 operation spans to the operator, who otherwise receives a masked MCP fault.
// Other operations and expected negative results remain quiet in the terminal.
func (p *cp1FailurePrinter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, span := range spans {
		if err := ctx.Err(); err != nil {
			return err
		}
		if span.InstrumentationScope().Name != "surla/controlplane" || span.Status().Code != codes.Error ||
			(span.Name() != "surla.controlplane.propose_change" && span.Name() != "surla.controlplane.revise_change") {
			continue
		}
		view := traceview.SafeSpan(span)
		if _, err := fmt.Fprintf(p.output, "surla: %s: %q; status=%q", span.Name(), view.Summary, span.Status().Description); err != nil {
			return err
		}
		for _, event := range span.Events() {
			for _, attr := range event.Attributes {
				if attr.Key == "exception.message" {
					if _, err := fmt.Fprintf(p.output, "; error=%q", attr.Value.AsString()); err != nil {
						return err
					}
				}
			}
		}
		if _, err := fmt.Fprintln(p.output); err != nil {
			return err
		}
	}
	return nil
}

// Shutdown has no resources to release; the operator owns output.
func (p *cp1FailurePrinter) Shutdown(context.Context) error {
	return nil
}

// newTraceProvider captures authenticated diagnostics locally without setup.
// An explicitly configured OTLP endpoint also receives all original spans via
// the standard exporter. Local recording is bound to this control plane's
// operations and never adds identity metadata or changes original evidence.
func newTraceProvider(
	ctx context.Context,
	output io.Writer,
	getenv func(string) string,
) (*sdktrace.TracerProvider, error) {
	options := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "surla"))),
		sdktrace.WithSyncer(&cp1FailurePrinter{output: output}),
	}
	if getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("configure OpenTelemetry export: %w", err)
		}
		options = append(options, sdktrace.WithBatcher(exporter))
	}
	return sdktrace.NewTracerProvider(options...), nil
}
