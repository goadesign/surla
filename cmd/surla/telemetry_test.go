// These tests use a local collector and scripted spans shaped like the ones
// propose_change and revise_change record today; no model or provider
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
// The scripted propose_change and revise_change spans carry the summaries and
// status descriptions those operations record: a save fault and a storage
// read fault, which the operator terminal prints with their private error
// text, and a refused revision, which it does not print because a refused
// analysis is not a server fault.
func TestStandaloneTelemetryKeepsFullOTLPAndSafeDiagnostics(t *testing.T) {
	const (
		proposalSaveFault = "CP1 proposal could not confirm Change Card save; preserve your request, " +
			"call get_change to check whether the card was saved, and ask the server operator to check private card storage before retrying."
		revisionReadFault = "CP1 revision could not read Change Card storage (this request did not modify the Change Card); " +
			"preserve your request and ask the server operator to check private card storage."
		revisionRefused = "Change Card analysis broke a revision rule (this request did not modify the Change Card); " +
			"correct the analysis and retry revise_change."
	)
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
	_, failed := tp.Tracer("surla/controlplane").Start(auth, "surla.controlplane.propose_change")
	failed.SetAttributes(
		attribute.String("surla.change_id", "chg-save-fault"),
		attribute.String(traceview.SummaryAttribute, proposalSaveFault),
	)
	failed.RecordError(errors.New("save change card: private-storage-content"))
	failed.SetStatus(codes.Error, "save cp1 change card")
	failed.End()
	_, refused := tp.Tracer("surla/controlplane").Start(auth, "surla.controlplane.revise_change")
	refused.SetAttributes(
		attribute.String("surla.change_id", "chg-refused"),
		attribute.String(traceview.SummaryAttribute, revisionRefused),
	)
	refused.End()
	_, fault := tp.Tracer("surla/controlplane").Start(auth, "surla.controlplane.revise_change")
	fault.SetAttributes(
		attribute.String("surla.change_id", "chg-read-fault"),
		attribute.String(traceview.SummaryAttribute, revisionReadFault),
	)
	fault.RecordError(errors.New("decode change card: private-storage-content"))
	fault.SetStatus(codes.Error, "load cp1 change card")
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
	assert.Equal(t, 5, count, "full export preserves even spans outside the caller view")
	assert.Contains(t, string(raw), "private-storage-content")
	assert.Contains(t, string(raw), "unowned-private-content")
	assert.NotContains(t, string(raw), "alice@example.com")
	assert.NotContains(t, string(raw), "surla.principal_hash")
	assert.Equal(t, 2, strings.Count(terminal.String(), "\n"), "only the two faults are printed")
	for _, want := range []string{
		proposalSaveFault, "save cp1 change card", "save change card: private-storage-content",
		revisionReadFault, "load cp1 change card", "decode change card: private-storage-content",
	} {
		assert.Contains(t, terminal.String(), want)
	}
	assert.NotContains(t, terminal.String(), revisionRefused, "a refused analysis is not printed as a fault")
	page, err := cp.ListTraces(auth, &gensurla.ListTracesPayload{Status: "all", Limit: 100})
	require.NoError(t, err)
	require.Len(t, page.Spans, 1)
	assert.Equal(t, "surla.controlplane.propose_change", page.Spans[0].Name)
	safe, err := json.Marshal(page)
	require.NoError(t, err)
	assert.NotContains(t, string(safe), "private-storage-content")
	assert.NotContains(t, string(safe), "unowned-private-content")
	assert.NotContains(t, string(safe), "alice@example.com")
}
