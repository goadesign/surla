// These tests use local HTTP stand-ins for the SDKs; no real provider is called.
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"goa.design/goa-ai/runtime/agent/model"

	"github.com/goadesign/surla/internal/modelsettings"
)

// TestLiveProviderConstructorsUseHeldSettingsWithoutSDKSpans exercises real SDK
// requests with sentinel credentials, endpoint paths, headers and HTTP failures.
// A global recorder catches SDK instrumentation even if the executor later gains
// a tracer provider. Anthropic's pinned key precedence is asserted explicitly.
func TestLiveProviderConstructorsUseHeldSettingsWithoutSDKSpans(t *testing.T) {
	for _, family := range modelsettings.Providers() {
		t.Run(family.Name, func(t *testing.T) {
			for _, provider := range modelsettings.Providers() {
				for _, name := range provider.Settings() {
					t.Setenv(name, "")
				}
			}
			recorder := tracetest.NewSpanRecorder()
			tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			previous := otel.GetTracerProvider()
			otel.SetTracerProvider(tracer)
			t.Cleanup(func() { otel.SetTracerProvider(previous); assert.NoError(t, tracer.Shutdown(context.Background())) })
			var refuse atomic.Bool
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Contains(t, r.URL.Path, "/path-sentinel/")
				if family.Name == "anthropic" {
					assert.Equal(t, "key-sentinel", r.Header.Get("X-Api-Key"))
					assert.NotContains(t, r.Header.Get("Authorization"), "token-sentinel")
					assert.Empty(t, r.Header.Get("X-Surla-Test"), "the pinned Anthropic SDK ignores custom headers when a key is held")
				} else {
					assert.Equal(t, "Bearer key-sentinel", r.Header.Get("Authorization"))
					assert.Equal(t, "header-sentinel", r.Header.Get("X-Surla-Test"))
					assert.Equal(t, "organization", r.Header.Get("OpenAI-Organization"))
					assert.Equal(t, "project", r.Header.Get("OpenAI-Project"))
				}
				w.Header().Set("request-id", "request-123")
				w.Header().Set("x-request-id", "request-123")
				if refuse.Load() {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","code":"invalid","message":"key-sentinel token-sentinel header-sentinel pw-sentinel path-sentinel"}}`)
					return
				}
				var body struct {
					Stream bool `json:"stream"`
				}
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				if body.Stream {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, liveSDKStream(family.Name))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, liveSDKResponse(family.Name))
			}))
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
			for variable, value := range values {
				t.Setenv(variable, value)
			}
			provider, err := newLiveProvider(family.Name, "fixture-model", []modelsettings.Settings{held})
			require.NoError(t, err)
			request := &model.Request{MaxTokens: 16, Messages: []*model.Message{{Role: model.ConversationRoleUser, Parts: []model.Part{model.TextPart{Text: "Hello"}}}}}
			response, err := provider.Complete(t.Context(), request)
			require.NoError(t, err)
			require.NotEmpty(t, response.Content)
			assert.Equal(t, "answer", response.Content[0].Text())
			stream, err := provider.Stream(t.Context(), request)
			require.NoError(t, err)
			for {
				_, err = stream.Recv()
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
			}
			require.NotNil(t, stream.Response())
			assert.NoError(t, stream.Close())
			refuse.Store(true)
			_, err = provider.Complete(t.Context(), request)
			var providerErr *model.ProviderError
			require.ErrorAs(t, err, &providerErr)
			assert.Equal(t, 400, providerErr.HTTPStatus())
			// The pinned adapters do not copy SDK request-ID headers into ProviderError.
			assert.Empty(t, providerErr.RequestID())
			if counter, ok := provider.(model.TokenCounter); ok {
				_, err = counter.CountTokens(t.Context(), request)
				assert.Error(t, err)
			}
			assert.GreaterOrEqual(t, calls.Load(), int32(3))
			assert.Empty(t, recorder.Ended(), "provider SDKs must create no spans carrying request settings")
		})
	}
}

// liveSDKResponse is a successful response from each provider's public API.
func liveSDKResponse(family string) string {
	if family == "anthropic" {
		return `{"id":"msg_1","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`
	}
	return `{"id":"resp_1","object":"response","status":"completed","model":"fixture-model","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer","annotations":[]}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`
}

// liveSDKStream completes a real SDK stream with the same successful answer.
func liveSDKStream(family string) string {
	if family == "openai" {
		return "data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":" + liveSDKResponse(family) + "}\n\ndata: [DONE]\n\n"
	}
	return strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"fixture-model","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":0}}}`,
		``, `event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``, `event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}`,
		``, `event: content_block_stop`, `data: {"type":"content_block_stop","index":0}`,
		``, `event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		``, `event: message_stop`, `data: {"type":"message_stop"}`, ``, ``,
	}, "\n")
}
