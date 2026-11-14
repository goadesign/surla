// These tests follow held settings through the real executor, SDK, private
// gateway wrapper and generated client, using only a local HTTP stand-in.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"goa.design/goa-ai/features/model/gateway"
	"goa.design/goa-ai/runtime/agent/model"
	"goa.design/goa-ai/runtime/agent/rawjson"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/goadesign/surla/internal/modelsettings"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
)

type (
	// pipelineResult is one generated client's observed result, never an SDK
	// result substituted for the consumer proof.
	pipelineResult struct {
		Case        string
		Text        string
		Usage       model.TokenUsage
		EOF         bool
		Chunks      int
		PartialText string
		Failure     pipelineFailure
	}

	// pipelineFailure retains every returned cause string for the sentinel
	// assertion and the structured facts the wrapper is required to preserve.
	pipelineFailure struct {
		Causes              []string
		Provider            *pipelineProviderFailure
		Rejected            *pipelineOutputFailure
		Usage               *model.TokenUsage
		UsageIncomplete     bool
		RateLimited         bool
		CountingUnsupported bool
		UnadvertisedName    string
	}

	// pipelineProviderFailure is the provider's structured error contract.
	pipelineProviderFailure struct {
		Provider, Operation, Kind, Code, RequestID string
		Status                                     int
		Retryable                                  bool
	}

	// pipelineOutputFailure contains bounded rejection facts without payloads.
	pipelineOutputFailure struct {
		Kind       model.OutputValidationKind
		Evidence   model.ResponseEvidence
		Usage      *model.TokenUsage
		Correction string
	}

	// pipelineObservation records the real SDK outcome before the private
	// production wrapper sees it. It returns no replacement errors or output.
	pipelineObservation struct {
		Operation string
		Failure   pipelineFailure
	}

	// pipelineSpan contains every recorded name, attribute and event value.
	pipelineSpan struct {
		Name       string
		Attributes map[string]string
		Events     []pipelineSpan
	}

	// pipelineCapture is an atomic snapshot. The parent requires observations
	// and a successful final write; a missing capture cannot pass as zero spans.
	pipelineCapture struct {
		Observations []pipelineObservation
		Spans        []pipelineSpan
		CaptureError string
	}

	// pipelineRecorder serializes file replacement across SDK stream callbacks.
	pipelineRecorder struct {
		mu       sync.Mutex
		recorder *tracetest.SpanRecorder
		capture  pipelineCapture
	}

	// pipelineHTTPCall captures routing and framing facts without credentials.
	pipelineHTTPCall struct {
		Case, Method, Route, RequestContentType, ResponseContentType string
		Status                                                       int
	}

	// pipelineHTTPRecorder retains the bounded fixture requests across handlers.
	pipelineHTTPRecorder struct {
		mu    sync.Mutex
		calls []pipelineHTTPCall
	}

	// pipelineProvider transparently observes the actual command constructor's
	// provider. The production liveProvider wrapper is outside this observer.
	pipelineProvider struct {
		inner  model.Provider
		record *pipelineRecorder
	}

	// pipelineStream observes the SDK's raw stream without changing its chunks,
	// complete response, terminal error, or cleanup outcome.
	pipelineStream struct {
		inner  model.Streamer
		record *pipelineRecorder
		name   string
	}
)

const (
	pipelineResultsFile = "model-pipeline-results.json"
	pipelineCaptureFile = "model-pipeline-capture.json"
)

// TestLiveSDKProvidersCrossTheProductionGateway uses the existing TestMain
// helper dispatcher and LaunchLive/Live, injecting only trusted preparation.
// Provider construction, settings handoff, gateway wrapping and gRPC consumers
// are the production path. No candidate binary or real provider is executed.
func TestLiveSDKProvidersCrossTheProductionGateway(t *testing.T) {
	for _, family := range modelsettings.Providers() {
		t.Run(family.Name, func(t *testing.T) {
			for _, entry := range os.Environ() {
				if name, _, _ := strings.Cut(entry, "="); modelsettings.InFamily(name) {
					t.Setenv(name, "")
				}
			}
			server, httpCalls := newPipelineHTTPProvider(t, family.Name)
			defer server.Close()
			endpoint := strings.Replace(server.URL, "://", "://u:pw-sentinel@", 1) + "/path-sentinel/"
			values := map[string]string{family.Key: "key-sentinel", family.Prefix + "BASE_URL": endpoint,
				family.Prefix + "CUSTOM_HEADERS": "X-Surla-Test: header-sentinel"}
			if family.Name == "anthropic" {
				values["ANTHROPIC_AUTH_TOKEN"] = "token-sentinel"
			} else {
				values["OPENAI_ORG_ID"] = "organization"
				values["OPENAI_PROJECT_ID"] = "project"
			}
			held, err := modelsettings.New(family.Name, values)
			require.NoError(t, err)
			self, err := os.Executable()
			require.NoError(t, err)
			env := append(os.Environ(), verify.LiveModelProviderEnv+"="+family.Name, verify.LiveModelNameEnv+"=fixture-model")
			process, err := verifyrun.LaunchLive(t.TempDir(), verify.Plan{Tests: verify.TestsWritten}, "pipeline-key",
				[]string{self, "test-live-model-executor"}, env, []modelsettings.Settings{held})
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, process.Stop()) })
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			require.NoError(t, process.AwaitReady(ctx))
			resultsPath := filepath.Join(process.Dir(), "tmp", pipelineResultsFile)
			require.Eventually(t, func() bool {
				_, err := os.Stat(resultsPath)
				return err == nil
			}, 35*time.Second, 10*time.Millisecond)
			resultsRaw, err := os.ReadFile(resultsPath)
			require.NoError(t, err)
			captureRaw, captureErr := os.ReadFile(filepath.Join(process.Dir(), "tmp", pipelineCaptureFile))
			logRaw, logErr := os.ReadFile(filepath.Join(process.Dir(), "executor.log"))
			require.NoError(t, process.Stop())
			require.NoError(t, errors.Join(captureErr, logErr))
			assert.NotContains(t, string(logRaw), "PIPELINE_CAPTURE_FAILED")
			var results []pipelineResult
			require.NoError(t, json.Unmarshal(resultsRaw, &results))
			var capture pipelineCapture
			require.NoError(t, json.Unmarshal(captureRaw, &capture))
			require.Empty(t, capture.CaptureError)
			require.NotEmpty(t, capture.Observations, "a missing observer must not look like zero SDK spans")
			assertPipelineResults(t, family.Name, results, capture)
			assertPipelineHTTPCalls(t, family.Name, httpCalls)
		})
	}
}

// runLiveModelPipelineExecutor is selected only by the test dispatcher. A
// global tracer is installed inside the actual executor before the production
// constructor runs, so SDK telemetry is observed in the process that emits it.
func runLiveModelPipelineExecutor(args []string) error {
	if len(args) != 3 || args[0] != "verify-run" || args[1] != "live" {
		return errors.New("invalid model pipeline executor arguments")
	}
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	observations := &pipelineRecorder{recorder: recorder}
	constructor := func(provider, name string, settings []modelsettings.Settings) (model.Provider, error) {
		inner, err := newLiveProvider(provider, name, settings)
		observations.observe("constructor", err)
		if err != nil {
			return inner, err
		}
		return &pipelineProvider{inner: inner, record: observations}, nil
	}
	prepare := func(context.Context, verify.Plan) (verify.Prepared, error) {
		self, err := os.Executable()
		if err != nil {
			return verify.Prepared{}, err
		}
		return verify.Prepared{SuiteBin: self, ArtifactFlags: []string{"--model-pipeline-suite", os.Getenv(verify.LiveModelProviderEnv)}}, nil
	}
	return verifyrun.Live(args[2], prepare, constructor, os.Stdout, os.Stderr)
}

// runLiveModelPipelineSuite is a trusted test program, selected by an explicit
// marker after --live. It uses only the gateway and its private TMPDIR.
func runLiveModelPipelineSuite(args []string) error {
	if len(args) != 2 || args[0] != "--model-pipeline-suite" {
		return errors.New("invalid model pipeline suite arguments")
	}
	conn, err := grpc.NewClient(os.Getenv(verify.LiveModelGatewayEnv), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client, err := gateway.NewGRPCClient(conn)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"url": "http://127.0.0.1:4100"}); err != nil {
		return err
	}
	var results []pipelineResult
	for _, scenario := range []string{"success", "stream", "case400", "casecount", "casebroken", "caseinvalid", "case429", "caseclosed"} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		request := &model.Request{MaxTokens: 16, ModelClass: model.ModelClassDefault, Messages: []*model.Message{{
			Role: model.ConversationRoleUser, Parts: []model.Part{model.TextPart{Text: scenario}},
		}}}
		if scenario == "caseinvalid" {
			input, err := model.AdvertisedToolInputFromSchema(rawjson.Message(`{"type":"object","properties":{},"additionalProperties":false}`))
			if err != nil {
				cancel()
				return err
			}
			request.Tools = []*model.ToolDefinition{{Name: "catalog.list_items", Description: "List catalog items", Input: input}}
		}
		result := pipelineResult{Case: scenario}
		var response *model.Response
		var failure error
		switch scenario {
		case "stream", "casebroken":
			stream, err := client.Stream(ctx, request)
			failure = err
			if err == nil {
				for {
					var chunk model.Chunk
					chunk, failure = stream.Recv()
					if failure != nil {
						break
					}
					result.Chunks++
					if text, ok := chunk.(model.TextChunk); ok {
						result.PartialText += text.Message.Text()
					}
				}
				if failure == io.EOF {
					result.EOF = true
					failure = nil
					response = stream.Response()
				}
				if err := stream.Close(); err != nil {
					failure = errors.Join(failure, err)
				}
			}
		case "casecount":
			_, failure = client.CountTokens(ctx, request)
		default:
			response, failure = client.Complete(ctx, request)
		}
		if response != nil {
			for _, message := range response.Content {
				result.Text += message.Text()
			}
			result.Usage = response.Usage
		}
		result.Failure = inspectPipelineFailure(failure)
		results = append(results, result)
		cancel()
	}
	if err := writePipelineJSON(filepath.Join(os.TempDir(), pipelineResultsFile), results); err != nil {
		return err
	}
	// The parent owns this target and ends it through LiveProcess.Stop.
	select {}
}

// Complete returns the SDK's exact response and error after recording facts.
func (p *pipelineProvider) Complete(ctx context.Context, request *model.Request) (*model.Response, error) {
	response, err := p.inner.Complete(ctx, request)
	p.record.observe("complete/"+pipelineCase(request), err)
	return response, err
}

// Stream preserves SDK creation failures and wraps only a successful stream.
func (p *pipelineProvider) Stream(ctx context.Context, request *model.Request) (model.Streamer, error) {
	stream, err := p.inner.Stream(ctx, request)
	name := "stream/" + pipelineCase(request)
	p.record.observe(name+"/open", err)
	if err != nil || pipelineNilStream(stream) {
		return stream, err
	}
	return &pipelineStream{inner: stream, record: p.record, name: name}, nil
}

// CountTokens preserves the raw provider's native or unsupported outcome.
func (p *pipelineProvider) CountTokens(ctx context.Context, request *model.Request) (model.TokenCount, error) {
	counter, ok := p.inner.(model.TokenCounter)
	if !ok {
		p.record.observe("count/"+pipelineCase(request), model.ErrTokenCountingUnsupported)
		return model.TokenCount{}, model.ErrTokenCountingUnsupported
	}
	count, err := counter.CountTokens(ctx, request)
	p.record.observe("count/"+pipelineCase(request), err)
	return count, err
}

// Recv observes only terminal outcomes without altering chunks or literal EOF.
func (s *pipelineStream) Recv() (model.Chunk, error) {
	chunk, err := s.inner.Recv()
	if err != nil {
		s.record.observe(s.name+"/terminal", err)
	}
	return chunk, err
}

// Response returns the actual SDK's assembled response unchanged.
func (s *pipelineStream) Response() *model.Response { return s.inner.Response() }

// Close preserves cleanup errors and captures any SDK span that ended at close.
func (s *pipelineStream) Close() error {
	err := s.inner.Close()
	s.record.observe(s.name+"/close", err)
	return err
}

// observe atomically snapshots every started span and the raw SDK's bounded
// facts. Failed writes are both retained and marked in private executor stderr.
func (r *pipelineRecorder) observe(operation string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.capture.Observations = append(r.capture.Observations, pipelineObservation{Operation: operation, Failure: inspectPipelineFailure(err)})
	r.capture.Spans = nil
	for _, span := range r.recorder.Started() {
		item := pipelineSpan{Name: span.Name(), Attributes: map[string]string{}}
		for _, value := range span.Attributes() {
			item.Attributes[string(value.Key)] = fmt.Sprint(value.Value.AsInterface())
		}
		for _, event := range span.Events() {
			e := pipelineSpan{Name: event.Name, Attributes: map[string]string{}}
			for _, value := range event.Attributes {
				e.Attributes[string(value.Key)] = fmt.Sprint(value.Value.AsInterface())
			}
			item.Events = append(item.Events, e)
		}
		r.capture.Spans = append(r.capture.Spans, item)
	}
	if err := writePipelineJSON(filepath.Join(os.TempDir(), pipelineCaptureFile), r.capture); err != nil {
		r.capture.CaptureError = err.Error()
		fmt.Fprintln(os.Stderr, "PIPELINE_CAPTURE_FAILED:", err)
	}
}

// inspectPipelineFailure preserves every cause for a consumer leak assertion.
// SDK observations remain private fake-data captures, never product output.
func inspectPipelineFailure(err error) pipelineFailure {
	failure := pipelineFailure{Usage: model.UsageFromError(err), UsageIncomplete: model.UsageIncompleteFromError(err),
		RateLimited: errors.Is(err, model.ErrRateLimited), CountingUnsupported: errors.Is(err, model.ErrTokenCountingUnsupported)}
	var visit func(error)
	visit = func(cause error) {
		if cause == nil {
			return
		}
		failure.Causes = append(failure.Causes, cause.Error())
		if joined, ok := cause.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				visit(child)
			}
		} else if wrapped, ok := cause.(interface{ Unwrap() error }); ok {
			visit(wrapped.Unwrap())
		}
	}
	visit(err)
	failure.UnadvertisedName, _ = model.UnadvertisedToolName(err)
	if provider, ok := model.AsProviderError(err); ok {
		failure.Provider = &pipelineProviderFailure{Provider: provider.Provider(), Operation: provider.Operation(), Kind: string(provider.Kind()),
			Code: provider.Code(), RequestID: provider.RequestID(), Status: provider.HTTPStatus(), Retryable: provider.Retryable()}
	}
	var rejected *model.OutputValidationError
	if errors.As(err, &rejected) {
		failure.Rejected = &pipelineOutputFailure{Kind: rejected.Kind(), Evidence: rejected.Evidence(), Usage: rejected.Usage(), Correction: rejected.RecoveryCorrection()}
	}
	return failure
}

// pipelineCase reads the explicit scenario marker of this trusted fixture.
func pipelineCase(request *model.Request) string { return request.Messages[0].Text() }

// writePipelineJSON replaces one private fixture artifact atomically so the
// parent cannot mistake a partial capture for a complete result.
func writePipelineJSON(name string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary := name + ".writing"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, name)
}

// newPipelineHTTPProvider is the only network peer the real SDK can reach.
// The final case closes its listener and current socket, producing an actual
// connection failure on retries rather than a fabricated provider error.
func newPipelineHTTPProvider(t *testing.T, family string) (*httptest.Server, *pipelineHTTPRecorder) {
	t.Helper()
	var listener net.Listener
	calls := &pipelineHTTPRecorder{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if !assert.NoError(t, err) {
			return
		}
		var requestBody struct {
			Stream bool `json:"stream"`
		}
		if !assert.NoError(t, json.Unmarshal(body, &requestBody)) {
			return
		}
		scenario := "success"
		if requestBody.Stream {
			scenario = "stream"
		}
		for _, name := range []string{"case400", "casecount", "casebroken", "caseinvalid", "case429", "caseclosed"} {
			if strings.Contains(string(body), name) {
				scenario = name
				break
			}
		}
		call := pipelineHTTPCall{Case: scenario, Method: request.Method, RequestContentType: request.Header.Get("Content-Type"),
			Route: strings.TrimPrefix(request.URL.Path, "/path-sentinel/")}
		defer func() { calls.record(call) }()
		assert.Contains(t, request.URL.Path, "/path-sentinel/")
		if family == "anthropic" {
			assert.Equal(t, "key-sentinel", request.Header.Get("X-Api-Key"))
			assert.NotContains(t, request.Header.Get("Authorization"), "token-sentinel")
			assert.Empty(t, request.Header.Get("X-Surla-Test"))
		} else {
			assert.Equal(t, "Bearer key-sentinel", request.Header.Get("Authorization"))
			assert.Equal(t, "header-sentinel", request.Header.Get("X-Surla-Test"))
			assert.Equal(t, "organization", request.Header.Get("OpenAI-Organization"))
			assert.Equal(t, "project", request.Header.Get("OpenAI-Project"))
		}
		if strings.Contains(string(body), "caseclosed") {
			hijacker, ok := w.(http.Hijacker)
			if !assert.True(t, ok) {
				return
			}
			connection, _, err := hijacker.Hijack()
			if !assert.NoError(t, err) {
				return
			}
			assert.NoError(t, listener.Close())
			assert.NoError(t, connection.Close())
			return
		}
		w.Header().Set("request-id", "request-123")
		w.Header().Set("x-request-id", "request-123")
		if strings.Contains(string(body), "case400") || strings.Contains(string(body), "casecount") || strings.Contains(string(body), "case429") {
			status, code := http.StatusBadRequest, "invalid_request_error"
			if strings.Contains(string(body), "case429") {
				status, code = http.StatusTooManyRequests, "rate_limit_error"
			}
			call.Status, call.ResponseContentType = status, "application/json"
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"code":%q,"message":"key-sentinel token-sentinel header-sentinel pw-sentinel path-sentinel"}}`, code, code)
			return
		}
		if strings.Contains(string(body), "caseinvalid") {
			call.Status, call.ResponseContentType = http.StatusOK, "application/json"
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, pipelineInvalidToolResponse(family))
			return
		}
		if requestBody.Stream {
			call.Status, call.ResponseContentType = http.StatusOK, "text/event-stream"
			w.Header().Set("Content-Type", "text/event-stream")
			if strings.Contains(string(body), "casebroken") {
				io.WriteString(w, liveSDKBrokenStream(family))
			} else if family == "openai" {
				// The canonical gRPC consumer requires the text delta to match
				// the complete response, not only an SDK terminal event.
				io.WriteString(w, `data: {"type":"response.output_text.delta","sequence_number":1,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"answer","logprobs":[]}`+"\n\n")
				io.WriteString(w, strings.Replace(liveSDKStream(family), `"sequence_number":1`, `"sequence_number":2`, 1))
			} else {
				io.WriteString(w, liveSDKStream(family))
			}
			return
		}
		call.Status, call.ResponseContentType = http.StatusOK, "application/json"
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, liveSDKResponse(family))
	}))
	listener = server.Listener
	server.Start()
	return server, calls
}

// pipelineInvalidToolResponse makes each actual adapter reject an unadvertised
// tool. The held key is deliberately in the invalid name/private cause; the
// generated client must receive only the preserved bounded rejection facts.
func pipelineInvalidToolResponse(family string) string {
	if family == "anthropic" {
		return `{"id":"msg_1","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"tool_use","id":"call_1","name":"key-sentinel","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":1}}`
	}
	return `{"id":"resp_1","object":"response","status":"completed","model":"fixture-model","output":[{"type":"function_call","id":"tool_1","call_id":"call_1","name":"key-sentinel","arguments":"{}"}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`
}

// assertPipelineResults compares actual SDK facts with the final consumer and
// checks every returned cause and every recorded span field for held values.
func assertPipelineResults(t *testing.T, family string, results []pipelineResult, capture pipelineCapture) {
	t.Helper()
	require.Len(t, results, 8)
	byCase := map[string]pipelineResult{}
	for _, result := range results {
		byCase[result.Case] = result
		encoded, err := json.Marshal(result.Failure)
		require.NoError(t, err)
		assertNoPipelineSentinels(t, string(encoded))
		t.Logf("%s generated %s: eof=%t chunks=%d partial=%q provider=%+v rejected=%+v usage=%+v rateLimited=%t unsupportedCount=%t causes=%d",
			family, result.Case, result.EOF, result.Chunks, result.PartialText, result.Failure.Provider, result.Failure.Rejected,
			result.Failure.Usage, result.Failure.RateLimited, result.Failure.CountingUnsupported, len(result.Failure.Causes))
	}
	observed := map[string]pipelineFailure{}
	for _, observation := range capture.Observations {
		_, duplicate := observed[observation.Operation]
		assert.False(t, duplicate, "unexpected repeated observer operation %s", observation.Operation)
		observed[observation.Operation] = observation.Failure
	}
	for _, operation := range []string{"constructor", "complete/success", "stream/stream/open", "stream/stream/terminal", "stream/stream/close",
		"complete/case400", "count/casecount", "stream/casebroken/open", "stream/casebroken/terminal", "stream/casebroken/close",
		"complete/caseinvalid", "complete/case429", "complete/caseclosed"} {
		require.Contains(t, observed, operation, "incomplete final observer capture")
	}
	for _, name := range []string{"success", "stream"} {
		result := byCase[name]
		assert.Equal(t, "answer", result.Text)
		assert.Equal(t, 2, result.Usage.InputTokens)
		assert.Equal(t, 1, result.Usage.OutputTokens)
		assert.Empty(t, result.Failure.Causes)
	}
	assert.True(t, byCase["stream"].EOF, "the generated stream finishes with literal EOF")
	assert.Greater(t, byCase["stream"].Chunks, 0)
	assert.Equal(t, "answer", byCase["stream"].PartialText)
	assert.Greater(t, byCase["casebroken"].Chunks, 0, "the generated consumer sees a chunk before the SDK fails")
	assert.Equal(t, "answer", byCase["casebroken"].PartialText)
	for scenario, operation := range map[string]string{"case400": "complete/case400", "casebroken": "stream/casebroken/terminal", "caseclosed": "complete/caseclosed"} {
		raw := observed[operation]
		consumer := byCase[scenario].Failure
		require.NotEmpty(t, raw.Causes)
		require.NotEmpty(t, consumer.Causes)
		assert.Equal(t, raw.Provider, consumer.Provider)
		assert.Equal(t, raw.Usage, consumer.Usage)
		assert.Equal(t, raw.UsageIncomplete, consumer.UsageIncomplete)
		t.Logf("%s generated client %s: provider=%+v usage=%+v causes=%d", family, scenario, consumer.Provider, consumer.Usage, len(consumer.Causes))
	}
	require.NotNil(t, byCase["case400"].Failure.Provider)
	assert.Equal(t, 400, byCase["case400"].Failure.Provider.Status)
	assert.Equal(t, "invalid_request", byCase["case400"].Failure.Provider.Kind)
	if family == "anthropic" {
		assert.Empty(t, byCase["case400"].Failure.Provider.Code, "the real Anthropic adapter exercises preservation of an empty code")
		require.NotNil(t, byCase["casecount"].Failure.Provider)
		assert.Equal(t, 400, byCase["casecount"].Failure.Provider.Status)
		assert.Equal(t, observed["count/casecount"].Provider, byCase["casecount"].Failure.Provider)
	} else {
		assert.True(t, byCase["casecount"].Failure.CountingUnsupported)
	}
	require.NotNil(t, observed["complete/caseclosed"].Provider)
	assert.Empty(t, observed["complete/caseclosed"].Provider.Code, "the transport failure exercises an empty crossing code too")
	assert.True(t, byCase["case429"].Failure.RateLimited)
	assert.Equal(t, observed["complete/case429"].Usage, byCase["case429"].Failure.Usage)
	rawRejection := observed["complete/caseinvalid"].Rejected
	require.NotNil(t, rawRejection, "the actual SDK adapter must produce this rejection")
	assert.Equal(t, model.OutputValidationToolIdentity, rawRejection.Kind)
	assert.True(t, rawRejection.Evidence.Present)
	// RejectProviderOutput cannot fingerprint a canonical response that its
	// translation could not construct; the presence-only evidence is exact.
	assert.Equal(t, model.ResponseEvidence{Present: true}, rawRejection.Evidence)
	require.NotNil(t, rawRejection.Usage)
	assert.Equal(t, 2, rawRejection.Usage.InputTokens)
	assert.Equal(t, 1, rawRejection.Usage.OutputTokens)
	assert.Equal(t, rawRejection, byCase["caseinvalid"].Failure.Rejected)
	assert.Empty(t, rawRejection.Correction, "these natural SDK rejections are terminal; the R2 test proves nonempty correction preservation")
	assert.Equal(t, "key-sentinel", observed["complete/caseinvalid"].UnadvertisedName,
		"the actual SDK retains the returned name behind its typed accessor")
	assert.Empty(t, byCase["caseinvalid"].Failure.UnadvertisedName,
		"the wrapper must remove the private cause, not merely its printable message")
	spans, err := json.Marshal(capture.Spans)
	require.NoError(t, err)
	assertNoPipelineSentinels(t, string(spans))
	t.Logf("%s pipeline complete: generated results=%d SDK observations=%d spans=%d literalEOF=%t partialText=%q rejected=%+v",
		family, len(results), len(capture.Observations), len(capture.Spans), byCase["stream"].EOF, byCase["casebroken"].PartialText, rawRejection)
}

// assertNoPipelineSentinels checks credential fragments even when a URL's
// password is rewritten by net/http or a provider error wraps several causes.
func assertNoPipelineSentinels(t *testing.T, text string) {
	t.Helper()
	for _, sentinel := range []string{"key-sentinel", "token-sentinel", "header-sentinel", "pw-sentinel", "path-sentinel"} {
		assert.NotContains(t, text, sentinel)
	}
}

// pipelineNilStream leaves typed nils unchanged so the observer cannot mask
// a provider contract failure before the production wrapper checks it.
func pipelineNilStream(stream model.Streamer) bool {
	if stream == nil {
		return true
	}
	value := reflect.ValueOf(stream)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// record retains one completed handler observation; it records no headers,
// request body, endpoint credentials or dynamic machine-local address.
func (r *pipelineHTTPRecorder) record(call pipelineHTTPCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
}

// assertPipelineHTTPCalls verifies and logs the SDK's actual request framing
// and the local fixture's JSON/SSE responses for the bounded focused capture.
func assertPipelineHTTPCalls(t *testing.T, family string, recorder *pipelineHTTPRecorder) {
	t.Helper()
	recorder.mu.Lock()
	calls := append([]pipelineHTTPCall(nil), recorder.calls...)
	recorder.mu.Unlock()
	require.NotEmpty(t, calls)
	seen := map[string]int{}
	for _, call := range calls {
		seen[call.Case]++
		assert.Equal(t, http.MethodPost, call.Method)
		assert.True(t, strings.HasPrefix(call.RequestContentType, "application/json"))
		switch call.Case {
		case "stream", "casebroken":
			assert.Equal(t, "text/event-stream", call.ResponseContentType)
		case "caseclosed":
			assert.Empty(t, call.ResponseContentType, "the socket is closed before any response")
		default:
			assert.Equal(t, "application/json", call.ResponseContentType)
		}
		t.Logf("%s HTTP %s %s case=%s request=%s response=%s status=%d", family, call.Method, call.Route,
			call.Case, call.RequestContentType, call.ResponseContentType, call.Status)
	}
	for _, scenario := range []string{"success", "stream", "case400", "casebroken", "caseinvalid", "case429", "caseclosed"} {
		assert.Positive(t, seen[scenario], "missing HTTP scenario %s", scenario)
	}
	if family == "anthropic" {
		assert.Positive(t, seen["casecount"], "the native counter reached the provider")
	} else {
		assert.Zero(t, seen["casecount"], "unsupported counting does not invent an HTTP call")
	}
}
