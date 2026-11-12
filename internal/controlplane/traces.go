// This file connects authenticated operation spans to the control plane's
// private diagnostic history and serves the caller's bounded view of that file.
package controlplane

import (
	"context"
	"errors"
	"sync/atomic"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/traceview"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type (
	// diagnosticTracer records only operations created by its owning control
	// plane. The host's provider and unrelated spans remain unchanged.
	diagnosticTracer struct {
		// Tracer creates original SDK spans with the host's existing settings.
		trace.Tracer
		// store belongs exclusively to the control plane requesting this tracer.
		store *traceview.Store
	}

	// diagnosticSpan records one completed authenticated operation without
	// putting private identity metadata on the SDK span or its context.
	diagnosticSpan struct {
		// ReadWriteSpan preserves the original SDK span and its readable result.
		sdktrace.ReadWriteSpan
		// store is the operation's control plane, never a provider-wide choice.
		store *traceview.Store
		// principal is taken from authenticated context when the operation starts.
		principal string
		// ended lets only the first End export and append this operation.
		ended atomic.Bool
	}
)

// ListTraces reads one bounded disk window belonging to the authenticated
// caller. A missing Options.TracerProvider returns not_implemented with setup
// instructions. An invalid cursor is a recoverable input error; storage
// failures retain full OpenTelemetry evidence and return a path-free message.
func (c *ControlPlane) ListTraces(
	ctx context.Context,
	p *gensurla.ListTracesPayload,
) (*gensurla.TraceList, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	if c.traceProvider == nil {
		return nil, gensurla.MakeNotImplemented(errors.New("operation recording is not configured; the server operator must set controlplane.Options.TracerProvider when constructing controlplane.New, or use surla serve for automatic recording"))
	}
	_, span := c.tracer("surla/controlplane").Start(ctx, "surla.controlplane.list_traces")
	defer span.End()
	result, err := c.traces.List(principal.ID, p.Status, p.Limit, p.Cursor)
	if errors.Is(err, traceview.ErrCursor) {
		return nil, gensurla.MakeUnprocessable(traceview.ErrCursor)
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "read saved operation diagnostics")
		return nil, errors.New("cannot read operation diagnostics; ask the server operator to check its private storage, then retry")
	}
	return result, nil
}

// Start returns the SDK's unchanged context so parent-child accounting keeps
// seeing its concrete SDK parent span. Only the separately returned local span
// owns recording the authenticated operation when its caller ends it.
func (t diagnosticTracer) Start(
	ctx context.Context,
	name string,
	opts ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	principal, ok := PrincipalFromContext(ctx)
	if ok {
		opts = append(opts, trace.WithAttributes(attribute.String("surla.caller_id", principal.CallerID), attribute.String("surla.caller_kind", string(principal.Kind))))
	}
	next, span := t.Tracer.Start(ctx, name, opts...)
	if !span.IsRecording() || !ok {
		return next, span
	}
	return next, &diagnosticSpan{
		ReadWriteSpan: span.(sdktrace.ReadWriteSpan),
		store:         t.store,
		principal:     principal.ID,
	}
}

// End completes the original span through the host's exporters, then appends
// its safe view once. Storage failures use the host's OpenTelemetry error
// handler; spans finishing after store closure are intentionally discarded.
func (s *diagnosticSpan) End(opts ...trace.SpanEndOption) {
	if !s.ended.CompareAndSwap(false, true) {
		return
	}
	// The SDK's End must be directly deferred to record an active panic as it
	// would without this wrapper. Re-panic only when this End was itself directly
	// deferred; an indirect helper must retain the SDK's ordinary behavior.
	// Reverse defer order records the completed span after SDK export, including
	// while its End continues the original panic.
	defer s.recordCompleted()
	defer s.ReadWriteSpan.End(opts...)
	if recovered := recover(); recovered != nil {
		panic(recovered)
	}
}

// tracer keeps global instrumentation when the host omitted TracerProvider.
// A configured provider carries the operation's store lexically rather than
// adding provider-wide attribution or exported identity metadata.
func (c *ControlPlane) tracer(scope string) trace.Tracer {
	if c.traceProvider == nil {
		return otel.Tracer(scope)
	}
	return diagnosticTracer{Tracer: c.traceProvider.Tracer(scope), store: c.traces}
}

// recordCompleted saves the ended SDK span even while its End resumes a panic.
// It holds no span history and reports only actual storage failures to the host.
func (s *diagnosticSpan) recordCompleted() {
	if err := s.store.Record(s.principal, s.ReadWriteSpan); err != nil {
		otel.Handle(err)
	}
}
