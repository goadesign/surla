// These tests use a local collector and scripted spans; no model or provider
// requests are made. They verify the operator export and safe local view apart.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/traceview"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	genotlp "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

// TestStandaloneTelemetryKeepsFullOTLPAndSafeDiagnostics uses the actual OTLP
// HTTP exporter, including the standard configured endpoint and flush path.
func TestStandaloneTelemetryKeepsFullOTLPAndSafeDiagnostics(t *testing.T) {
	requests := make(chan []byte, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/traces", r.URL.Path)
		assert.Equal(t, "application/x-protobuf", r.Header.Get("Content-Type"))
		raw, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case requests <- raw:
		default:
			t.Error("unexpected second telemetry request")
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)
	// The test uses only its local collector, never ambient exporter headers.
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", collector.URL+"/v1/traces")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_COMPRESSION", "none")
	var terminal bytes.Buffer
	cardsDir := filepath.Join(t.TempDir(), "cards")
	tp, err := newTraceProvider(t.Context(), &terminal, os.Getenv)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, tp.Shutdown(ctx))
	})
	cp, err := controlplane.New(controlplane.Options{
		CardsDir: cardsDir, TracerProvider: tp,
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })
	auth := controlplane.WithPrincipal(t.Context(), controlplane.Principal{ID: "alice@example.com"})
	ctx, failed := tp.Tracer("surla/controlplane").Start(auth, "surla.controlplane.propose_change")
	failed.SetAttributes(
		attribute.String(traceview.SummaryAttribute, "The response could not be used. Retry this request."),
		attribute.String("surla.cp1.rejection_kind", "structured_output"),
		attribute.String("raw.secret", "private-provider-content"),
	)
	failed.RecordError(errors.New("private-provider-content"))
	failed.SetStatus(codes.Error, "private-provider-content")
	_, child := tp.Tracer("test").Start(ctx, "model.complete")
	child.RecordError(errors.New("private-provider-content"))
	child.SetStatus(codes.Error, "private-provider-content")
	child.End()
	failed.End()
	_, expected := tp.Tracer("surla/controlplane").Start(auth, "surla.controlplane.revise_change")
	expected.End()
	_, fault := tp.Tracer("surla/controlplane").Start(auth, "surla.controlplane.revise_change")
	fault.RecordError(errors.New("provider rejected credentials: 401"))
	fault.SetStatus(codes.Error, "revise cp1 change card")
	fault.End()
	_, unowned := tp.Tracer("test").Start(t.Context(), "surla.controlplane.propose_change")
	unowned.SetStatus(codes.Error, "unowned-private-content")
	unowned.End()
	_, err = cp.ProposeChange(auth, &gensurla.ProposeChangePayload{ChangeID: "local-card", Prompt: "private-feature-request", Analysis: confirmedAnalysis()})
	require.NoError(t, err)
	flushCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, tp.ForceFlush(flushCtx))
	var raw []byte
	select {
	case raw = <-requests:
	default:
		t.Fatal("ForceFlush completed without the configured collector receiving spans")
	}
	var exported genotlp.ExportTraceServiceRequest
	require.NoError(t, proto.Unmarshal(raw, &exported))
	count := 0
	for _, resource := range exported.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			count += len(scope.Spans)
		}
	}
	assert.Equal(t, 6, count, "full export preserves even spans outside the caller view")
	assert.Contains(t, string(raw), "private-provider-content")
	assert.Contains(t, string(raw), "unowned-private-content")
	assert.NotContains(t, string(raw), "alice@example.com")
	assert.NotContains(t, string(raw), "surla.principal_hash")
	assert.Equal(t, 2, strings.Count(terminal.String(), "\n"))
	assert.Contains(t, terminal.String(), "provider rejected credentials: 401")
	assert.Contains(t, terminal.String(), "revise cp1 change card")
	assert.Contains(t, terminal.String(), "The response could not be used. Retry this request.")
	assert.Contains(t, terminal.String(), "private-provider-content")
	page, err := cp.ListTraces(auth, &gensurla.ListTracesPayload{Status: "all", Limit: 100})
	require.NoError(t, err)
	require.Len(t, page.Spans, 1)
	assert.Equal(t, "surla.controlplane.propose_change", page.Spans[0].Name)
	safe, err := json.Marshal(page)
	require.NoError(t, err)
	assert.NotContains(t, string(safe), "private-provider-content")
	assert.NotContains(t, string(safe), "unowned-private-content")
	assert.NotContains(t, string(safe), "alice@example.com")
}
