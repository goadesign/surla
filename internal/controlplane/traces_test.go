// These tests exercise authenticated trace history through the generated HTTP
// and MCP transports, alongside a full OpenTelemetry exporter.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	genhttpclient "github.com/goadesign/surla/gen/http/surla/client"
	gensurlahttp "github.com/goadesign/surla/gen/http/surla/server"
	genmcpclient "github.com/goadesign/surla/gen/jsonrpc/mcp_surla/client"
	genmcpsurla "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	gensurlatools "github.com/goadesign/surla/gen/surla/toolsets/surla"
	"github.com/goadesign/surla/internal/traceview"
	"github.com/goadesign/surla/verify/candidate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"goa.design/goa/v3/codegen"
	goahttp "goa.design/goa/v3/http"
)

// blockedCheckExport holds a real request at its final export so shutdown's
// ordering can be checked without timers or a running container engine.
type blockedCheckExport struct {
	*tracetest.InMemoryExporter
	// started reports that the operation has reached its final span export.
	started chan struct{}
	// release lets that export finish after Close has begun waiting.
	release <-chan struct{}
	// deadline prevents a failed assertion from stranding the request.
	deadline context.Context
}

// TestCloseKeepsTracingUntilCheckRequestEnds proves that Close waits for a
// check's final error span, even when setup fails before opening an engine.
func TestCloseKeepsTracingUntilCheckRequestEnds(t *testing.T) {
	deadline, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cardsDir := filepath.Join(t.TempDir(), "cards")
	release := make(chan struct{})
	exported := &blockedCheckExport{
		InMemoryExporter: tracetest.NewInMemoryExporter(),
		started:          make(chan struct{}),
		release:          release,
		deadline:         deadline,
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exported))
	cp, err := New(Options{
		CardsDir: cardsDir, Checker: candidate.NewContainerRunner(), CheckHelper: testCheckHelper(t), CheckTimeout: time.Minute,
		TracerProvider: tp,
	})
	require.NoError(t, err)
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		assert.NoError(t, cp.Close())
		assert.NoError(t, tp.Shutdown(context.Background()))
	})
	requestDone := make(chan error, 1)
	go func() {
		_, err := cp.CheckProject(WithPrincipal(deadline, Principal{ID: "alice@example.com", CallerID: "test-user", Kind: CallerUser}))
		requestDone <- err
	}()
	select {
	case <-exported.started:
	case <-deadline.Done():
		t.Fatal("check did not reach its final span export")
	}
	closed := make(chan error, 1)
	go func() { closed <- cp.Close() }()
	select {
	case <-cp.serverCtx.Done():
	case <-deadline.Done():
		t.Fatal("Close did not cancel owned work")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close released diagnostics while the request was exporting: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-requestDone:
		assertServiceError(t, err, "not_implemented")
	case <-deadline.Done():
		t.Fatal("check did not finish")
	}
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-deadline.Done():
		t.Fatal("Close did not finish")
	}
	ended := exported.GetSpans()
	require.Len(t, ended, 3)
	assert.Equal(t, "surla.controlplane.recover_check_operations", ended[0].Name)
	assert.Equal(t, "surla.controlplane.check_project", ended[1].Name)
	assert.Equal(t, "surla.controlplane.recover_check_operations", ended[2].Name)
	saved, err := traceview.Open(cardsDir)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, saved.Close()) })
	page, err := saved.List("alice@example.com", "error", 20, nil)
	require.NoError(t, err)
	require.Len(t, page.Spans, 1)
	assert.Equal(t, "surla.controlplane.check_project", page.Spans[0].Name)
}

// ExportSpans pauses only the check under test, then preserves its original
// evidence in the standard SDK test exporter.
func (e *blockedCheckExport) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, span := range spans {
		if span.Name() == "surla.controlplane.check_project" {
			close(e.started)
			select {
			case <-e.release:
			case <-e.deadline.Done():
				return e.deadline.Err()
			}
		}
	}
	return e.InMemoryExporter.ExportSpans(ctx, spans)
}

// TestTraceHistoryHTTPAndMCP verifies real generated codecs, defaults and auth.
// It also proves a safe local view leaves the original exported evidence intact.
func TestTraceHistoryHTTPAndMCP(t *testing.T) {
	t.Parallel()
	full := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSyncer(full),
	)
	t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.Background())) })
	cp := newControlPlane(t, Options{TracerProvider: tp})
	alice := WithPrincipal(t.Context(), Principal{ID: "alice@example.com", CallerID: "test-user", Kind: CallerUser})
	parentCtx, parent := cp.tracer("surla/controlplane").Start(alice, "operation-parent")
	_, failed := cp.tracer("surla/controlplane").Start(parentCtx, "operation-failed")
	failed.SetAttributes(
		attribute.String(traceview.SummaryAttribute, cp1SafeFaultSummary(false, cp1FaultStorageSave)),
		attribute.String("surla.change_id", "change-1"),
		attribute.String("raw.secret", "/private/operator/key"),
	)
	failed.RecordError(errors.New("private storage content"))
	failed.SetStatus(codes.Error, "private storage content")
	failed.End()
	parent.End()
	bob := WithPrincipal(t.Context(), Principal{ID: "bob@example.com", CallerID: "bob@example.com", Kind: CallerUser})
	_, other := cp.tracer("surla/controlplane").Start(bob, "bob-operation")
	other.SetStatus(codes.Error, "bob-private-error")
	other.End()
	_, unauthenticated := cp.tracer("surla/controlplane").Start(t.Context(), "no-authenticated-owner")
	unauthenticated.End()

	srv := httptest.NewServer(NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{
		"generation-token": {ID: "alice@example.com"},
		"bob-token":        {ID: "bob@example.com"},
	})))
	t.Cleanup(srv.Close)
	origin, err := url.Parse(srv.URL)
	require.NoError(t, err)
	doer := generationDoer{client: srv.Client()}
	httpTransport := genhttpclient.NewClient(origin.Scheme, origin.Host, doer,
		goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	client := &gensurla.Client{ListTracesEndpoint: httpTransport.ListTraces()}
	page, err := client.ListTraces(t.Context(), &gensurla.ListTracesPayload{Status: "error", Limit: 20})
	require.NoError(t, err)
	require.Len(t, page.Spans, 1)
	view := page.Spans[0]
	assert.Equal(t, "operation-failed", view.Name)
	assert.Equal(t, parent.SpanContext().TraceID().String(), view.TraceID)
	assert.Equal(t, parent.SpanContext().SpanID().String(), view.ParentSpanID)
	assert.Equal(t, "error", view.Status)
	assert.Equal(t, map[string]string{"surla.change_id": "change-1", "surla.caller_id": "test-user", "surla.caller_kind": "user"}, view.Attributes)
	assert.Equal(t, cp1SafeFaultSummary(false, cp1FaultStorageSave), view.Summary)
	raw, err := json.Marshal(page)
	require.NoError(t, err)
	for _, private := range []string{"alice@example.com", "bob@example.com", "private storage content", "raw.secret", "/private/operator/key", "bob-private-error"} {
		assert.NotContains(t, string(raw), private)
	}

	mcpTransport := genmcpclient.NewClient(origin.Scheme, origin.Host, doer,
		goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	mcp := &genmcpsurla.Client{ToolsCallEndpoint: mcpTransport.ToolsCall()}
	callMCPTool(t, mcp, "list_traces", `{"status":"error"}`, gensurlatools.UnmarshalListTracesResult)
	callMCPTool(t, mcp, "list_traces", `{"status":"ok","limit":1}`, gensurlatools.UnmarshalListTracesResult)

	// Both auth identities cross the same real HTTP edge; generated code owns
	// filter and limit validation before the service reads that caller's history.
	for _, test := range []struct {
		name, query, token string
		wantStatus         int
		wantName           string
	}{
		{"missing authentication", "", "", http.StatusUnauthorized, ""},
		{"bob", "?status=error", "bob-token", http.StatusOK, "bob-operation"},
		{"limit below", "?limit=0", "generation-token", http.StatusBadRequest, ""},
		{"limit above", "?limit=101", "generation-token", http.StatusBadRequest, ""},
		{"limit at maximum", "?limit=100", "generation-token", http.StatusOK, "operation-failed"},
		{"invalid filter", "?status=missing", "generation-token", http.StatusBadRequest, ""},
		{"invalid cursor", "?cursor=bad", "generation-token", http.StatusUnprocessableEntity, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/traces"+test.query, nil)
			require.NoError(t, err)
			if test.token != "" {
				req.Header.Set("Authorization", "Bearer "+test.token)
			}
			res, err := srv.Client().Do(req)
			require.NoError(t, err)
			defer func() { assert.NoError(t, res.Body.Close()) }()
			assert.Equal(t, test.wantStatus, res.StatusCode)
			if test.wantName != "" {
				var body genhttpclient.ListTracesResponseBody
				require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
				names := make([]string, 0, len(body.Spans))
				for _, span := range body.Spans {
					require.NotNil(t, span.Name)
					names = append(names, *span.Name)
				}
				assert.Contains(t, names, test.wantName)
			}
		})
	}
	ended := full.GetSpans()
	require.GreaterOrEqual(t, len(ended), 4)
	assert.Equal(t, "private storage content", ended[0].Status.Description)
	assert.Equal(t, "exception", ended[0].Events[0].Name)
	assert.Contains(t, ended[0].Attributes, attribute.String("raw.secret", "/private/operator/key"))
	for _, span := range ended {
		for _, value := range span.Attributes {
			assert.NotEqual(t, "surla.principal_hash", string(value.Key))
		}
	}
}

// TestTraceToolEncodesEmptyData verifies the generated list_traces tool
// contract without relying on HTTP's normalization of nil collections.
func TestTraceToolEncodesEmptyData(t *testing.T) {
	t.Parallel()
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.Background())) })
	cp := newControlPlane(t, Options{TracerProvider: tp})
	adapter := genmcpsurla.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)
	tools, err := adapter.ToolsList(t.Context(), &genmcpsurla.ToolsListPayload{})
	require.NoError(t, err)
	var found bool
	for _, tool := range tools.Tools {
		if tool.Name == "list_traces" {
			found = true
		}
	}
	require.True(t, found, "MCP ToolsList must include list_traces")
	ctx := WithPrincipal(t.Context(), Principal{ID: "alice@example.com", CallerID: "test-user", Kind: CallerUser})
	result, err := adapter.ToolsCall(ctx, &genmcpsurla.ToolsCallPayload{Name: "list_traces", Arguments: json.RawMessage(`{}`)})
	require.NoError(t, err)
	complete, ok := result.Outcome.AsComplete()
	require.True(t, ok)
	assert.NotContains(t, string(complete.StructuredContent), `"spans":null`)
	page, err := gensurlatools.UnmarshalListTracesResult(complete.StructuredContent)
	require.NoError(t, err)
	assert.Empty(t, page.Spans)
	assert.False(t, page.HasMore)
}

// TestTraceToolExplainsMissingRecording separates an unconfigured host from
// configured empty history through both generated HTTP and MCP error paths.
func TestTraceToolExplainsMissingRecording(t *testing.T) {
	t.Parallel()
	cp := newControlPlane(t, Options{})
	ctx := WithPrincipal(t.Context(), Principal{ID: "alice@example.com", CallerID: "test-user", Kind: CallerUser})
	_, err := cp.ListTraces(ctx, &gensurla.ListTracesPayload{Status: "all", Limit: 20})
	assertServiceError(t, err, "not_implemented")
	adapter := genmcpsurla.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)
	result, err := adapter.ToolsCall(ctx, &genmcpsurla.ToolsCallPayload{Name: "list_traces", Arguments: json.RawMessage(`{}`)})
	require.NoError(t, err)
	complete, ok := result.Outcome.AsComplete()
	require.True(t, ok)
	require.NotNil(t, complete.IsError)
	assert.True(t, *complete.IsError)
	require.NotEmpty(t, complete.Content)
	require.NotNil(t, complete.Content[0].Text)
	assert.Contains(t, *complete.Content[0].Text, "Options.TracerProvider")
	assert.Contains(t, *complete.Content[0].Text, "start the project with surla")
	srv := httptest.NewServer(NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{
		"generation-token": {ID: "alice@example.com"},
	})))
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/traces", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer generation-token-builder")
	response, err := srv.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, response.Body.Close()) })
	assert.Equal(t, http.StatusNotImplemented, response.StatusCode)
}

// TestTraceToolReadsSavedHistoryAfterRestart exercises persisted ownership
// through the generated MCP codec after the original control plane releases its lock.
func TestTraceToolReadsSavedHistoryAfterRestart(t *testing.T) {
	t.Parallel()
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.Background())) })
	opts := Options{CardsDir: t.TempDir(), TracerProvider: tp}
	first, err := New(opts)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, first.Close()) })
	ctx := WithPrincipal(t.Context(), Principal{ID: "alice@example.com", CallerID: "test-user", Kind: CallerUser})
	_, span := first.tracer("surla/controlplane").Start(ctx, "saved-before-restart")
	span.End()
	require.NoError(t, first.Close())
	second, err := New(opts)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, second.Close()) })
	adapter := genmcpsurla.NewMCPAdapter(gensurla.NewEndpoints(second), nil)
	result, err := adapter.ToolsCall(ctx, &genmcpsurla.ToolsCallPayload{Name: "list_traces", Arguments: json.RawMessage(`{}`)})
	require.NoError(t, err)
	complete, ok := result.Outcome.AsComplete()
	require.True(t, ok)
	page, err := gensurlatools.UnmarshalListTracesResult(complete.StructuredContent)
	require.NoError(t, err)
	require.Len(t, page.Spans, 1)
	assert.Equal(t, "saved-before-restart", page.Spans[0].Name)
}

// TestCP1FailuresProduceSafeSavedRecovery exercises the production annotation
// owners through real proposals and revisions and generated MCP result
// decoding. No fixture sets diagnostic attributes on its spans. A revision
// that breaks the example accounting is the caller's to fix, so its span
// carries a code-authored summary without error status; a save that fails is
// a server fault the operator must look at.
func TestCP1FailuresProduceSafeSavedRecovery(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	cp := newControlPlane(t, Options{TracerProvider: tp})
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		assert.NoError(t, cp.Close())
		assert.NoError(t, tp.Shutdown(context.Background()))
	})
	ctx := WithPrincipal(t.Context(), Principal{ID: "alice@example.com", CallerID: "test-user", Kind: CallerUser})
	seed, err := cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{
		ChangeID: "existing", Prompt: "private-feature-request", Analysis: refundAnalysis("private-analysis-summary"),
	})
	require.NoError(t, err)

	reference := "private-analysis-reference"
	_, err = cp.ReviseChange(ctx, &gensurla.ReviseChangePayload{
		ChangeID: "existing", RequestRevision: seed.RequestRevision, Prompt: "private-feature-request",
		Analysis: &gensurla.CP1RevisionAnalysis{
			Summary: "private-analysis-summary", Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
			Examples: []*gensurla.CP1RevisionAnalysisExample{{
				PriorID: &reference, Scenario: "Refund at the limit", InputSummary: "Order total $50.00",
				ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
			}},
			RemovedExampleIds: []string{}, PreservedBehaviors: []string{},
		},
	})
	assertServiceError(t, err, "unprocessable")
	unchanged, err := cp.GetChange(ctx, &gensurla.GetChangePayload{ChangeID: "existing"})
	require.NoError(t, err)
	assert.Equal(t, seed.RequestRevision, unchanged.RequestRevision)

	// The existing card-store fault fixture makes only the final save fail.
	require.NoError(t, os.Mkdir(filepath.Join(cp.cards.dir, stagingFile), 0o700))
	_, err = cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{
		ChangeID: "save-fault", Prompt: "private-feature-request", Analysis: refundAnalysis("private-analysis-summary"),
	})
	require.Error(t, err)
	adapter := genmcpsurla.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)
	result, err := adapter.ToolsCall(ctx, &genmcpsurla.ToolsCallPayload{
		Name: "list_traces", Arguments: json.RawMessage(`{"status":"all","limit":100}`),
	})
	require.NoError(t, err)
	complete, ok := result.Outcome.AsComplete()
	require.True(t, ok)
	page, err := gensurlatools.UnmarshalListTracesResult(complete.StructuredContent)
	require.NoError(t, err)
	seen := map[string]string{}
	for _, span := range page.Spans {
		if span.Name != "surla.controlplane.propose_change" && span.Name != "surla.controlplane.revise_change" {
			continue
		}
		key := span.Name + " " + span.Attributes["surla.change_id"]
		seen[key] = span.Status
		switch key {
		case "surla.controlplane.propose_change existing":
			assert.Equal(t, "Proposed Change Card (needs_confirmation).", span.Summary)
		case "surla.controlplane.revise_change existing":
			assert.NotEqual(t, "error", span.Status, "a broken revision rule is the caller's input, not a server failure")
			assert.Contains(t, span.Summary, "retry revise_change")
		case "surla.controlplane.propose_change save-fault":
			assert.Equal(t, "error", span.Status)
			assert.Contains(t, strings.ToLower(span.Summary), "operator")
		default:
			t.Errorf("unexpected CP1 span: %+v", span)
		}
	}
	assert.Len(t, seen, 3)
	// A caller-authored card summary belongs to the card, not to the safe
	// diagnostic file or any caller-visible diagnostic page.
	raw, err := os.ReadFile(filepath.Join(cp.cards.dir, "traces.jsonl"))
	require.NoError(t, err)
	for _, secret := range []string{
		"private-feature-request", "private-analysis-summary", "private-analysis-reference", "alice@example.com",
	} {
		assert.NotContains(t, string(raw), secret)
		assert.NotContains(t, string(complete.StructuredContent), secret)
	}
}

// TestSharedProviderKeepsControlPlaneHistoriesSeparate exercises two embedded
// instances and unrelated host spans on one provider, without identity tags.
func TestSharedProviderKeepsControlPlaneHistoriesSeparate(t *testing.T) {
	full := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(full))
	t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.Background())) })
	a := newControlPlane(t, Options{TracerProvider: tp})
	b := newControlPlane(t, Options{TracerProvider: tp})
	ctx := WithPrincipal(t.Context(), Principal{ID: "alice@example.com", CallerID: "test-user", Kind: CallerUser})
	_, host := tp.Tracer("surla/controlplane").Start(ctx, "private-host-operation")
	host.SetAttributes(attribute.String("host.evidence", "original-host-evidence"))
	host.End()
	_, foreign := tp.Tracer("third-party").Start(ctx, "private-third-party-operation")
	foreign.End()
	_, first := a.tracer("surla/controlplane").Start(ctx, "only-a")
	first.End()
	first.End()
	_, second := b.tracer("surla/controlplane").Start(ctx, "only-b")
	second.End()
	pageA, err := a.ListTraces(ctx, &gensurla.ListTracesPayload{Status: "all", Limit: 100})
	require.NoError(t, err)
	require.Len(t, pageA.Spans, 1)
	assert.Equal(t, "only-a", pageA.Spans[0].Name)
	pageB, err := b.ListTraces(ctx, &gensurla.ListTracesPayload{Status: "all", Limit: 100})
	require.NoError(t, err)
	require.Len(t, pageB.Spans, 1)
	assert.Equal(t, "only-b", pageB.Spans[0].Name)
	ended := full.GetSpans()
	require.Len(t, ended, 6)
	assert.Equal(t, "private-host-operation", ended[0].Name)
	assert.Equal(t, []attribute.KeyValue{attribute.String("host.evidence", "original-host-evidence")}, ended[0].Attributes)
	for _, span := range ended[1:] {
		assert.Empty(t, span.Attributes, "capture must add no metadata to any exported span")
	}
	require.NoError(t, a.Close())
	_, after := b.tracer("surla/controlplane").Start(ctx, "b-after-a-closes")
	after.End()
	pageB, err = b.ListTraces(ctx, &gensurla.ListTracesPayload{Status: "all", Limit: 100})
	require.NoError(t, err)
	require.Len(t, pageB.Spans, 3)
	assert.Equal(t, "b-after-a-closes", pageB.Spans[0].Name)
}

// TestDiagnosticTracingPreservesSDKParentAccounting keeps the SDK context's
// concrete parent span, its child count and normal parent-child identifiers.
func TestDiagnosticTracingPreservesSDKParentAccounting(t *testing.T) {
	full := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(full))
	t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.Background())) })
	cp := newControlPlane(t, Options{TracerProvider: tp})
	ctx := WithPrincipal(t.Context(), Principal{ID: "alice@example.com", CallerID: "test-user", Kind: CallerUser})
	ctx, parent := cp.tracer("surla/controlplane").Start(ctx, "parent")
	_, child := tp.Tracer("host-child").Start(ctx, "child")
	child.End()
	parent.End()
	ended := full.GetSpans()
	require.Len(t, ended, 2)
	assert.Equal(t, 1, ended[1].ChildSpanCount)
	assert.Equal(t, parent.SpanContext().TraceID(), ended[0].SpanContext.TraceID())
	assert.Equal(t, parent.SpanContext().SpanID(), ended[0].Parent.SpanID())
}

// TestDiagnosticTracingPreservesSDKPanicEvidence compares direct and indirect
// deferred End calls with the unwrapped SDK, including continued panics.
func TestDiagnosticTracingPreservesSDKPanicEvidence(t *testing.T) {
	for _, direct := range []bool{true, false} {
		name := "indirect End"
		if direct {
			name = "directly deferred End"
		}
		t.Run(name, func(t *testing.T) {
			full := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(full))
			t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.Background())) })
			cp := newControlPlane(t, Options{TracerProvider: tp})
			ctx := WithPrincipal(t.Context(), Principal{ID: "alice@example.com", CallerID: "test-user", Kind: CallerUser})
			plain := runDiagnosticPanic(ctx, tp.Tracer("surla/controlplane"), direct)
			owned := runDiagnosticPanic(ctx, cp.tracer("surla/controlplane"), direct)
			assert.Equal(t, "private-panic-evidence", plain)
			assert.Equal(t, plain, owned)
			ended := full.GetSpans()
			require.Len(t, ended, 2)
			assert.Equal(t, ended[0].Status, ended[1].Status)
			require.Len(t, ended[1].Events, len(ended[0].Events))
			for i, event := range ended[0].Events {
				assert.Equal(t, event.Name, ended[1].Events[i].Name)
				assert.Equal(t, event.Attributes, ended[1].Events[i].Attributes)
			}
			raw, err := os.ReadFile(filepath.Join(cp.cards.dir, "traces.jsonl"))
			require.NoError(t, err)
			assert.NotContains(t, string(raw), "private-panic-evidence")
		})
	}
}

// runDiagnosticPanic uses the same call shape with a raw or owned SDK tracer.
// The recovery belongs outside End so both paths must continue the same panic.
func runDiagnosticPanic(ctx context.Context, tracer trace.Tracer, direct bool) (recovered any) {
	defer func() { recovered = recover() }()
	_, span := tracer.Start(ctx, "panic-operation")
	if direct {
		defer span.End()
	} else {
		defer func() { span.End() }()
	}
	panic("private-panic-evidence")
}

// TestWaitingReadBoundaryExportsWithoutRecording excludes periodic browser waits from private operation history.
func TestWaitingReadBoundaryExportsWithoutRecording(t *testing.T) {
	exported := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exported))
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	cp := newControlPlane(t, Options{TracerProvider: provider})
	caller := WithPrincipal(t.Context(), Principal{ID: "owner", CallerID: "session", Kind: CallerUser})
	_, span := cp.tracer("surla/requests").Start(caller, "surla.request.wait_for_changes")
	span.End()
	assert.Len(t, exported.GetSpans(), 1)
	history, err := cp.traces.List("owner", "all", 20, nil)
	require.NoError(t, err)
	assert.Empty(t, history.Spans)
}

// TestHistoryLeavesOutOnlyTheUsersSuccessfulReads sends one call through the
// real HTTP handler or MCP server and compares the records the history gains.
// A read the user's session sends with GET adds none when it succeeds,
// including reading the history itself, or is refused (not found, forbidden),
// an expected answer without error status. A read by the user that faults or
// panics, a change by the user, and a read by an agent over HTTP or MCP are
// recorded.
// The waiting read stays out even when it ends with an error. Every call
// keeps its exported spans and their status.
func TestHistoryLeavesOutOnlyTheUsersSuccessfulReads(t *testing.T) {
	const (
		owner = "alice@example.com"
		// user, guide and builder are workflowAuthenticator's credentials;
		// generationDoer sends builder.
		user  = "generation-token"
		guide = "generation-token-guide"
		// historyPanic is the value a panicking read panics with.
		historyPanic = "history-read-panic"
	)
	// get returns a call sending a GET for path with token.
	get := func(path, token string) func(*testing.T, *ControlPlane, *httptest.Server, *gensurla.ChangeCardResult) int {
		return func(t *testing.T, _ *ControlPlane, srv *httptest.Server, _ *gensurla.ChangeCardResult) int {
			return sendHistoryRequest(t, t.Context(), srv, http.MethodGet, path, token, nil)
		}
	}
	for _, test := range []struct {
		name string
		// panics makes the promotion target lookup every list_changes runs
		// panic while the call is sent.
		panics bool
		// call sends the request and returns its HTTP status, or 0 when no
		// answer arrived.
		call       func(*testing.T, *ControlPlane, *httptest.Server, *gensurla.ChangeCardResult) int
		wantStatus int
		// saved lists the records the call adds as "name caller_kind status".
		saved []string
		// exported gives the status of each span the exporter must hold.
		exported map[string]codes.Code
	}{
		{
			name: "user reads a change", call: get("/changes/chg-history", user), wantStatus: http.StatusOK,
			saved:    []string{},
			exported: map[string]codes.Code{"surla.request.get_change": codes.Unset},
		},
		{
			name: "user lists changes", call: get("/changes", user), wantStatus: http.StatusOK,
			saved:    []string{},
			exported: map[string]codes.Code{"surla.request.list_changes": codes.Unset, "surla.controlplane.list_changes": codes.Unset},
		},
		{
			name: "user reads the history", call: get("/traces?status=all&limit=100", user), wantStatus: http.StatusOK,
			saved:    []string{},
			exported: map[string]codes.Code{"surla.request.list_traces": codes.Unset, "surla.controlplane.list_traces": codes.Unset},
		},
		{
			name: "user read is refused as not found", call: get("/changes/chg-missing", user), wantStatus: http.StatusNotFound,
			saved:    []string{},
			exported: map[string]codes.Code{"surla.request.get_change": codes.Unset},
		},
		{
			name: "user read is refused as forbidden", call: get("/assignments/builder", user), wantStatus: http.StatusForbidden,
			saved:    []string{},
			exported: map[string]codes.Code{"surla.request.list_builder_assignments": codes.Unset},
		},
		{
			name: "user read faults",
			call: func(t *testing.T, cp *ControlPlane, srv *httptest.Server, _ *gensurla.ChangeCardResult) int {
				require.NoError(t, os.WriteFile(cp.cards.path("chg-broken"), []byte("{"), 0o600))
				return sendHistoryRequest(t, t.Context(), srv, http.MethodGet, "/changes/chg-broken", user, nil)
			},
			wantStatus: http.StatusInternalServerError,
			saved:      []string{"surla.request.get_change user error"},
			exported:   map[string]codes.Code{"surla.request.get_change": codes.Error},
		},
		{
			// The panic reaches net/http, which drops the connection, so no
			// answer arrives; the SDK records it as an exception event only.
			name: "user read panics", panics: true, call: get("/changes", user), wantStatus: 0,
			saved:    []string{"surla.controlplane.list_changes user unset", "surla.request.list_changes user unset"},
			exported: map[string]codes.Code{"surla.request.list_changes": codes.Unset, "surla.controlplane.list_changes": codes.Unset},
		},
		{
			name: "user confirms a change",
			call: func(t *testing.T, _ *ControlPlane, srv *httptest.Server, card *gensurla.ChangeCardResult) int {
				raw, err := gensurla.EncodeConfirmIntentPayload(&gensurla.ConfirmIntentPayload{
					ChangeID: card.ChangeID, RequestRevision: card.RequestRevision, Answers: map[string]string{}, Examples: card.Examples,
				})
				require.NoError(t, err)
				return sendHistoryRequest(t, t.Context(), srv, http.MethodPost, "/changes/confirm", user, raw)
			},
			wantStatus: http.StatusOK,
			saved:      []string{"surla.request.confirm_intent user unset"},
			exported:   map[string]codes.Code{"surla.request.confirm_intent": codes.Unset},
		},
		{
			name: "guide reads a change over HTTP", call: get("/changes/chg-history", guide), wantStatus: http.StatusOK,
			saved:    []string{"surla.request.get_change guide unset"},
			exported: map[string]codes.Code{"surla.request.get_change": codes.Unset},
		},
		{
			name: "builder reads a change over MCP",
			call: func(t *testing.T, _ *ControlPlane, srv *httptest.Server, card *gensurla.ChangeCardResult) int {
				origin, err := url.Parse(srv.URL)
				require.NoError(t, err)
				transport := genmcpclient.NewClient(origin.Scheme, origin.Host, generationDoer{client: srv.Client()},
					goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
				mcp := &genmcpsurla.Client{ToolsCallEndpoint: transport.ToolsCall()}
				read := callMCPTool(t, mcp, "get_change", `{"change_id":"`+card.ChangeID+`"}`, gensurlatools.UnmarshalGetChangeResult)
				assert.Equal(t, card.ChangeID, read.ChangeID)
				return http.StatusOK
			},
			wantStatus: http.StatusOK,
			saved:      []string{"surla.request.get_change builder unset"},
			exported:   map[string]codes.Code{"surla.request.get_change": codes.Unset},
		},
		{
			name: "user's waiting read ends with an error",
			call: func(t *testing.T, cp *ControlPlane, srv *httptest.Server, _ *gensurla.ChangeCardResult) int {
				// The current version keeps the read waiting; reading it starts no span.
				list, err := cp.changeList(t.Context(), trace.SpanFromContext(t.Context()), owner)
				require.NoError(t, err)
				// The caller leaves while the server waits, as a closing page does.
				ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
				defer cancel()
				return sendHistoryRequest(t, ctx, srv, http.MethodGet, "/change-updates?since="+list.Version, user, nil)
			},
			wantStatus: 0,
			saved:      []string{},
			exported:   map[string]codes.Code{"surla.request.wait_for_changes": codes.Error, waitForChangesSpan: codes.Unset},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
			// Unarmed, the lookup reports no promotion target, as when none is set.
			var panicking atomic.Bool
			cp := newControlPlane(t, Options{
				TracerProvider: provider,
				ResolvePromotionTarget: func(context.Context, string) (PromotionTarget, error) {
					if panicking.Load() {
						panic(historyPanic)
					}
					return nil, nil
				},
			})
			card := mustPropose(t, cp, WithPrincipal(t.Context(), Principal{ID: owner, CallerID: "guide", Kind: CallerGuide}),
				"chg-history", "Route refunds to a manager.", refundAnalysis("Route refunds to a manager."))
			srv := httptest.NewServer(NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{user: {ID: owner}})))
			t.Cleanup(srv.Close)
			saved := func() map[string]string {
				page, err := cp.traces.List(owner, "all", 100, nil)
				require.NoError(t, err)
				records := map[string]string{}
				for _, span := range page.Spans {
					records[span.SpanID] = span.Name + " " + span.Attributes["surla.caller_kind"] + " " + span.Status
				}
				return records
			}
			before := saved()

			panicking.Store(test.panics)
			assert.Equal(t, test.wantStatus, test.call(t, cp, srv, card))
			// Close returns once every request's handler, and so every span, has ended.
			srv.Close()

			added := []string{}
			for id, record := range saved() {
				if _, ok := before[id]; !ok {
					added = append(added, record)
				}
			}
			assert.ElementsMatch(t, test.saved, added)
			exported := map[string]codes.Code{}
			for _, span := range exporter.GetSpans() {
				if _, ok := test.exported[span.Name]; ok {
					exported[span.Name] = span.Status.Code
					if test.panics {
						require.NotEmpty(t, span.Events, "%s does not carry the panic", span.Name)
						assert.Equal(t, "exception", span.Events[0].Name)
						assert.Contains(t, span.Events[0].Attributes, attribute.String("exception.message", historyPanic))
					}
				}
			}
			assert.Equal(t, test.exported, exported)
		})
	}
}

// TestUserGETRoutesOnlyRead checks the fact the history's rule for the user's
// reads rests on, instead of trusting it: every GET route of the generated
// HTTP server that the operation table allows to the user's session leaves the
// project's saved files unchanged. The fixture holds a confirmed change whose
// build has unread assignments, the state a read that records it was read
// would save. Every request carries since, the only required query parameter
// of a GET route the user may call; a route that cannot be decoded fails as
// not run.
func TestUserGETRoutesOnlyRead(t *testing.T) {
	workspace := t.TempDir()
	cp := newBuildControlPlane(t, Options{WorkspaceDir: workspace})
	card := confirmedChange(t, cp, "chg")
	srv := httptest.NewServer(NewHTTPHandler(cp, fixtureCredentials(map[string]Principal{
		"user": {ID: "alice@example.com", CallerID: "user", Kind: CallerUser},
	})))
	t.Cleanup(srv.Close)
	status, body := httpCall(t, srv.URL, "user", http.MethodPost, "/changes/chg/build", `{"request_revision":"`+card.RequestRevision+`"}`)
	require.Equal(t, http.StatusOK, status, body)
	build := mustStartedBuild(t, cp, asCaller("alice@example.com", CallerUser), "chg")
	require.False(t, build.Builder.Read || build.TestWriter.Read, "the fixture's assignments start unread")

	// Mount points name the Go method; the operation table uses the design's name.
	operations := map[string]string{}
	for _, name := range gensurla.MethodNames {
		operations[codegen.Goify(name, true)] = name
	}
	routes := gensurlahttp.New(gensurla.NewEndpoints(cp), goahttp.NewMuxer(), StrictJSONRequestDecoder,
		goahttp.ResponseEncoder, nil, nil).Mounts
	param := regexp.MustCompile(`\{[^}]+\}`)
	var called []string
	for _, route := range routes {
		operation, ok := operations[route.Method]
		require.True(t, ok, "mount point %s names no design method", route.Method)
		if route.Verb != http.MethodGet || !OperationAllowed(CallerUser, operation) {
			continue
		}
		called = append(called, operation)
		before := savedProjectFiles(t, cp.cards.dir, workspace)
		status, body := httpCall(t, srv.URL, "user", http.MethodGet, param.ReplaceAllString(route.Pattern, "chg")+"?since=unread", "")
		assert.NotEqual(t, http.StatusBadRequest, status, "%s did not run: %s", operation, body)
		assert.NotEqual(t, http.StatusForbidden, status, "%s refused the user: %s", operation, body)
		assert.Equal(t, before, savedProjectFiles(t, cp.cards.dir, workspace), "the user's %s changed a saved file", operation)
	}
	assert.NotEmpty(t, called)
	t.Logf("GET routes the user's session may call: %v", called)
}

// sendHistoryRequest sends one request with token to srv and returns its
// status, or 0 when no answer arrived: ctx ended first, or the server dropped
// the connection after a panic.
func sendHistoryRequest(
	t *testing.T,
	ctx context.Context,
	srv *httptest.Server,
	method, path, token string,
	body []byte,
) int {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, method, srv.URL+path, bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := srv.Client().Do(request)
	if err != nil {
		t.Logf("%s %s got no answer: %v", method, path, err)
		return 0
	}
	assert.NoError(t, response.Body.Close())
	return response.StatusCode
}

// savedProjectFiles returns the content of every regular file under dirs by
// path, except the diagnostics history, whose records the history tests own.
func savedProjectFiles(t *testing.T, dirs ...string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || !entry.Type().IsRegular() || entry.Name() == "traces.jsonl" {
				return err
			}
			raw, err := os.ReadFile(path)
			files[path] = string(raw)
			return err
		})
		require.NoError(t, err)
	}
	return files
}
