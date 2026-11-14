// These tests exercise the executor's gateway without real provider settings.
package verifyrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa-ai/features/model/gateway"
	"goa.design/goa-ai/runtime/agent/model"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/goadesign/surla/internal/modelsettings"
	"github.com/goadesign/surla/internal/verify"
)

type (
	// testLiveCounter supplies a native counter independently of completions.
	testLiveCounter struct {
		*testLiveProvider
		err error
	}
	// testLiveProvider supplies deterministic provider outcomes to the wrapper.
	testLiveProvider struct {
		completeErr error
		mode        string
		streamErr   error
		stream      model.Streamer
	}
	// testLiveStream exercises each terminal stream boundary.
	testLiveStream struct {
		err      error
		closeErr error
	}
)

// Complete returns successful canonical output or the requested failure.
func (p *testLiveProvider) Complete(context.Context, *model.Request) (*model.Response, error) {
	if strings.Contains(p.mode, "panic") {
		panic("sentinel-gateway-key")
	}
	if p.mode == "gateway-serve-ends" {
		go runLiveModel(grpc.NewServer(), stoppedLiveListener{})
	}
	return &model.Response{}, p.completeErr
}

// Stream returns the requested raw stream, including typed-nil error cases.
func (p *testLiveProvider) Stream(ctx context.Context, _ *model.Request) (model.Streamer, error) {
	if p.mode == "gateway-stream-panic" {
		go func() { panic("sentinel-gateway-key") }()
	}
	if p.mode == "gateway-hold" {
		if err := os.WriteFile(filepath.Join(os.TempDir(), "gateway-call"), []byte("stream started"), 0o600); err != nil {
			return nil, err
		}
		return heldLiveStream{ctx: ctx}, nil
	}
	return p.stream, p.streamErr
}

// Recv supplies the configured terminal condition.
func (s testLiveStream) Recv() (model.Chunk, error) { return nil, s.err }

// Response returns a complete successful response.
func (s testLiveStream) Response() *model.Response { return &model.Response{} }

// Close supplies an independent SDK cleanup failure.
func (s testLiveStream) Close() error { return s.closeErr }

// fakeLiveProvider lets real executor-process tests open a gateway without SDKs.
// The constructor verifies that settings arrive after preparation, not in a
// starting block, and that they agree with the explicit held snapshot.
func fakeLiveProvider(name, _ string, settings []modelsettings.Settings) (model.Provider, error) {
	for _, held := range settings {
		for _, entry := range held.Environ() {
			variable, value, _ := strings.Cut(entry, "=")
			if os.Getenv(variable) != value {
				return nil, errors.New("held setting was not applied after preparation")
			}
		}
	}
	if name == "" {
		return nil, errors.New("provider missing")
	}
	if os.Getenv(liveFakeEnv) == "gateway-constructor-fails" {
		return nil, errors.New("sentinel-gateway-key")
	}
	return &testLiveProvider{stream: testLiveStream{err: io.EOF}, mode: os.Getenv(liveFakeEnv)}, nil
}

// TestLiveSettingsInput rejects incomplete, unknown, duplicate and invalid
// settings without returning submitted values; valid newline/equal signs stay.
func TestLiveSettingsInput(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        bool
	}{
		{"empty", "", true},
		{"key", "ANTHROPIC_API_KEY=sentinel\x00", true},
		{"equals and newline", "ANTHROPIC_API_KEY=x=y\nz\x00", true},
		{"truncated", "ANTHROPIC_API_KEY=sentinel", false},
		{"unknown", "OTHER_SECRET=sentinel\x00", false},
		{"missing equals", "ANTHROPIC_API_KEY\x00", false},
		{"empty entry", "ANTHROPIC_API_KEY=sentinel\x00\x00", false},
		{"duplicate", "ANTHROPIC_API_KEY=a\x00ANTHROPIC_API_KEY=b\x00", false},
		{"missing key", "ANTHROPIC_BASE_URL=https://sentinel.invalid\x00", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held, err := readLiveSettings(strings.NewReader(tc.input))
			if !tc.want {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "sentinel")
				return
			}
			require.NoError(t, err)
			if tc.input != "" {
				require.Len(t, held, 1)
				assert.Equal(t, []string{strings.TrimSuffix(tc.input, "\x00")}, held[0].Environ())
			}
		})
	}
}

// TestLiveModelErrorsPreserveClassificationAndUsage demonstrates each unsafe
// cause is dropped without losing the structured facts consumers use.
func TestLiveModelErrorsPreserveClassificationAndUsage(t *testing.T) {
	secret := "credential-sentinel"
	provider := model.NewProviderError("anthropic", "complete", 400,
		model.ProviderErrorKindInvalidRequest, "invalid", secret, "request-123", false, errors.New(secret))
	got := liveModelError("anthropic", fmt.Errorf("%s: %w", secret, provider))
	var safe *model.ProviderError
	require.ErrorAs(t, got, &safe)
	assert.Equal(t, provider.Provider(), safe.Provider())
	assert.Equal(t, provider.Operation(), safe.Operation())
	assert.Equal(t, provider.HTTPStatus(), safe.HTTPStatus())
	assert.Equal(t, provider.Kind(), safe.Kind())
	assert.Equal(t, provider.Code(), safe.Code())
	assert.Equal(t, provider.RequestID(), safe.RequestID())
	assert.Equal(t, provider.Retryable(), safe.Retryable())
	assert.Nil(t, errors.Unwrap(safe))
	assert.NotContains(t, got.Error(), secret)
	for _, original := range []error{context.Canceled, context.DeadlineExceeded, model.ErrRateLimited,
		model.ErrTokenCountingUnsupported, model.ErrStreamingUnsupported, model.ErrEmptyStream} {
		assert.Equal(t, original, liveModelError("anthropic", fmt.Errorf("%s: %w", secret, original)))
	}
	assert.Same(t, io.EOF, liveModelError("anthropic", io.EOF))
	assert.NotErrorIs(t, liveModelError("anthropic", fmt.Errorf("%s: %w", secret, io.EOF)), io.EOF)
	usage := model.TokenUsage{InputTokens: 4, OutputTokens: 3}
	for _, incomplete := range []bool{false, true} {
		original := model.RetainUsage(fmt.Errorf("%s: %w", secret, model.ErrRateLimited), usage)
		if incomplete {
			original = model.RetainIncompleteUsage(original, usage)
		}
		safe := liveModelError("anthropic", original)
		assert.ErrorIs(t, safe, model.ErrRateLimited)
		assert.Equal(t, &usage, model.UsageFromError(safe))
		assert.Equal(t, incomplete, model.UsageIncompleteFromError(safe))
		assert.NotContains(t, safe.Error(), secret)
	}
	rejected, err := model.RestoreOutputValidationError(model.OutputValidationResponseShape,
		errors.New(secret), model.ResponseEvidence{}, &usage)
	require.NoError(t, err)
	got = liveModelError("anthropic", rejected)
	var invalid *model.OutputValidationError
	require.ErrorAs(t, got, &invalid)
	assert.Equal(t, rejected.Kind(), invalid.Kind())
	assert.Equal(t, rejected.Evidence(), invalid.Evidence())
	assert.Equal(t, &usage, invalid.Usage())
	assert.NotContains(t, got.Error(), secret)
	assert.NotContains(t, errors.Unwrap(invalid).Error(), secret)
}

// TestLiveProviderProtectsEveryOperation checks raw provider and stream paths,
// nil-stream boundaries, unsupported counting and successful response forwarding.
func TestLiveProviderProtectsEveryOperation(t *testing.T) {
	secret := errors.New("key-sentinel")
	inner := &testLiveProvider{completeErr: secret, streamErr: secret}
	p := &liveProvider{inner: inner, name: "openai"}
	_, err := p.Complete(t.Context(), &model.Request{})
	assert.NotContains(t, err.Error(), secret.Error())
	stream, err := p.Stream(t.Context(), &model.Request{})
	assert.Nil(t, stream)
	assert.NotContains(t, err.Error(), secret.Error())
	_, err = p.CountTokens(t.Context(), &model.Request{})
	assert.ErrorIs(t, err, model.ErrTokenCountingUnsupported)
	inner.streamErr = nil
	var typedNil *testLiveStream
	inner.stream = typedNil
	stream, err = p.Stream(t.Context(), &model.Request{})
	assert.Nil(t, stream)
	assert.Error(t, err)
	inner.stream = testLiveStream{err: io.EOF, closeErr: secret}
	stream, err = p.Stream(t.Context(), &model.Request{})
	require.NoError(t, err)
	_, err = stream.Recv()
	assert.Same(t, io.EOF, err)
	assert.NotNil(t, stream.Response())
	assert.NotContains(t, stream.Close().Error(), secret.Error())
	inner.stream = testLiveStream{err: secret}
	stream, err = p.Stream(t.Context(), &model.Request{})
	require.NoError(t, err)
	_, err = stream.Recv()
	assert.NotContains(t, err.Error(), secret.Error())
}

// TestLiveModelFailureCrossesTheRealGateway verifies structured provider facts
// survive the generated gRPC codec without carrying the original error chain.
func TestLiveModelFailureCrossesTheRealGateway(t *testing.T) {
	secret := "token-sentinel"
	provider := &liveProvider{name: "anthropic", inner: &testLiveProvider{
		completeErr: model.NewProviderError("anthropic", "complete", 400,
			model.ProviderErrorKindInvalidRequest, "invalid", secret, "rid-1", false, errors.New(secret)),
	}}
	client := startLiveTestGateway(t, provider)
	_, err := client.Complete(t.Context(), &model.Request{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	var got *model.ProviderError
	require.ErrorAs(t, err, &got)
	assert.Equal(t, 400, got.HTTPStatus())
	assert.Equal(t, "rid-1", got.RequestID())
}

// TestLiveDetailIsBoundedAfterRedaction covers expansion by a short held value
// and UTF-8 content at the new cut, preserving the owner's overlap protection.
func TestLiveDetailIsBoundedAfterRedaction(t *testing.T) {
	held, err := modelsettings.New("anthropic", map[string]string{"ANTHROPIC_API_KEY": "x"})
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "executor.log"), []byte(strings.Repeat("x界", maxLiveDetail)), 0o600))
	process := &LiveProcess{dir: dir, settings: []modelsettings.Settings{held}}
	failure := process.startError("failed", nil)
	assert.LessOrEqual(t, len(failure.Detail), maxLiveDetail)
	assert.True(t, utf8.ValidString(failure.Detail))
	assert.NotContains(t, failure.Detail, "x")
}

// TestWrittenLivePreparationMessagesUseStructuredStages keeps operator settings
// and generated suite internals out of accepted-tests repair instructions.
func TestWrittenLivePreparationMessagesUseStructuredStages(t *testing.T) {
	for _, stage := range []verify.Stage{verify.StageCheckout, verify.StageBuild} {
		message := writtenLivePreparationSummary(&verify.StageError{Stage: stage})
		assert.Contains(t, message, "check_project")
		assert.Contains(t, message, "test writer")
		assert.NotContains(t, message, "suite.Command")
		assert.NotContains(t, message, "verification configuration")
	}
}

// TestLiveStreamCreationPreservesRejectedOutput covers a provider that returns
// both its partial stream and a rejection. Closing the partial stream must not
// turn that rejection into a generic provider failure, even when cleanup fails.
func TestLiveStreamCreationPreservesRejectedOutput(t *testing.T) {
	usage := model.TokenUsage{InputTokens: 7, OutputTokens: 2}
	rejected, err := model.RestoreOutputValidationError(model.OutputValidationToolArguments,
		errors.New("unsafe-key-sentinel"), model.ResponseEvidence{}, &usage)
	require.NoError(t, err)
	rejected, err = model.RestoreCorrectableOutputValidationError(rejected, "Return tool arguments matching the schema.")
	require.NoError(t, err)
	for _, closeErr := range []error{nil, errors.New("unsafe-close-key-sentinel")} {
		name := "close succeeds"
		if closeErr != nil {
			name = "close fails"
		}
		t.Run(name, func(t *testing.T) {
			provider := &liveProvider{name: "anthropic", inner: &testLiveProvider{
				stream: testLiveStream{closeErr: closeErr}, streamErr: rejected,
			}}
			stream, err := provider.Stream(t.Context(), &model.Request{})
			assert.Nil(t, stream)
			assertLiveOutputRejection(t, err, rejected)

			client := startLiveTestGateway(t, provider)
			remote, err := client.Stream(t.Context(), &model.Request{})
			if err == nil {
				_, err = remote.Recv()
				assert.NoError(t, remote.Close())
			}
			assertLiveOutputRejection(t, err, rejected)
		})
	}
	t.Run("canceled cleanup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		provider := &liveProvider{name: "anthropic", inner: &testLiveProvider{
			stream: testLiveStream{}, streamErr: rejected,
		}}
		stream, err := provider.Stream(ctx, &model.Request{})
		assert.Nil(t, stream)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, &usage, model.UsageFromError(err))
	})
}

// assertLiveOutputRejection verifies classification and bounded correction
// survive without retaining the original unsafe error or cleanup cause.
func assertLiveOutputRejection(t *testing.T, err error, original *model.OutputValidationError) {
	t.Helper()
	var rejected *model.OutputValidationError
	require.ErrorAs(t, err, &rejected)
	assert.Equal(t, original.Kind(), rejected.Kind())
	assert.Equal(t, original.Evidence(), rejected.Evidence())
	assert.Equal(t, original.Usage(), rejected.Usage())
	assert.Equal(t, original.RecoveryCorrection(), rejected.RecoveryCorrection())
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		assert.NotContains(t, cause.Error(), "sentinel")
	}
}

// startLiveTestGateway reaches the production error wrapper through the real
// generated gRPC consumer and stops its server without a graceful stream wait.
func startLiveTestGateway(t *testing.T, provider model.Provider) model.Client {
	t.Helper()
	server, err := gateway.NewServer(gateway.WithProvider(provider))
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	rpc := grpc.NewServer()
	gateway.RegisterGRPCServer(rpc, server)
	go func() { rpc.Serve(listener) }()
	t.Cleanup(rpc.Stop)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })
	client, err := gateway.NewGRPCClient(conn)
	require.NoError(t, err)
	return client
}

// CountTokens supplies a successful count or the configured provider failure.
func (c *testLiveCounter) CountTokens(context.Context, *model.Request) (model.TokenCount, error) {
	return model.TokenCount{Model: "fixture", InputTokens: 3, Exact: true}, c.err
}

// TestLiveNativeCounterUsesTheSameProtectedBoundary guards the counting path
// that bypasses gateway completion middleware, for both success and failure.
func TestLiveNativeCounterUsesTheSameProtectedBoundary(t *testing.T) {
	counter := &testLiveCounter{testLiveProvider: &testLiveProvider{}}
	client := startLiveTestGateway(t, &liveProvider{inner: counter, name: "anthropic"})
	count, err := client.CountTokens(t.Context(), &model.Request{})
	require.NoError(t, err)
	assert.Equal(t, 3, count.InputTokens)
	assert.True(t, count.Exact)
	counter.err = model.NewProviderError("anthropic", "count", 400,
		model.ProviderErrorKindInvalidRequest, "invalid", "key-sentinel", "count-request", false, errors.New("key-sentinel"))
	_, err = client.CountTokens(t.Context(), &model.Request{})
	require.Error(t, err)
	var provider *model.ProviderError
	require.ErrorAs(t, err, &provider)
	assert.Equal(t, 400, provider.HTTPStatus())
	assert.Equal(t, "count-request", provider.RequestID())
	assert.NotContains(t, err.Error(), "key-sentinel")
}
