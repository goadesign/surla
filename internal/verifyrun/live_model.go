// This file owns a written-test live target's private model gateway. It runs
// only in the disposable executor process, never in the project server.
package verifyrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"

	"goa.design/goa-ai/features/model/gateway"
	"goa.design/goa-ai/runtime/agent/model"
	"google.golang.org/grpc"

	"github.com/goadesign/surla/internal/modelsettings"
	"github.com/goadesign/surla/internal/verify"
)

type (
	// liveProvider removes provider text from every failure crossing the gateway.
	liveProvider struct {
		// inner is the provider constructed after preparation in this executor.
		inner model.Provider
		// name identifies the selected family in fixed, server-authored messages.
		name string
	}

	// liveStream preserves raw output while protecting terminal provider errors.
	liveStream struct {
		// inner owns the provider's response and stream resources.
		inner model.Streamer
		// name identifies the selected provider in fixed failure messages.
		name string
	}
)

// readLiveSettings reads the starting block's NAME=value NUL format. A missing
// final NUL is a truncated handover, never a partial setting to apply. The only
// accepted names and values are those the handover's settings owner validates.
func readLiveSettings(input io.Reader) ([]modelsettings.Settings, error) {
	raw, err := io.ReadAll(input)
	if err != nil {
		return nil, errors.New("read live model settings")
	}
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[len(raw)-1] != 0 {
		return nil, errors.New("truncated live model settings")
	}
	values := map[string]map[string]string{}
	for _, entry := range strings.Split(string(raw[:len(raw)-1]), "\x00") {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, errors.New("invalid live model setting")
		}
		found := false
		for _, provider := range modelsettings.Providers() {
			if !provider.Crosses(name) {
				continue
			}
			if values[provider.Name] == nil {
				values[provider.Name] = map[string]string{}
			}
			if _, duplicate := values[provider.Name][name]; duplicate {
				return nil, errors.New("duplicate live model setting")
			}
			values[provider.Name][name] = value
			found = true
			break
		}
		if !found {
			return nil, errors.New("unrecognized live model setting")
		}
	}
	var held []modelsettings.Settings
	for _, provider := range modelsettings.Providers() {
		if entries := values[provider.Name]; entries != nil {
			settings, err := modelsettings.New(provider.Name, entries)
			if err != nil {
				return nil, err
			}
			held = append(held, settings)
		}
	}
	return held, nil
}

// serveLiveModel opens a loopback-only listener owned by this executor. Any
// Serve return ends its whole process group, just as a suite's exit does. Stop
// uses SIGKILL, so an open stream cannot delay cleanup; there is no graceful
// shutdown that could retain the test program or its provider requests.
func serveLiveModel(provider model.Provider, name string) (string, error) {
	server, err := gateway.NewServer(gateway.WithProvider(&liveProvider{inner: provider, name: name}))
	if err != nil {
		return "", err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	rpc := grpc.NewServer()
	gateway.RegisterGRPCServer(rpc, server)
	go func() {
		// The group ends even if Serve returns nil: without its gateway the
		// accepted target cannot continue using the model it was started with.
		if err := rpc.Serve(listener); err != nil {
			endGroup()
			return
		}
		endGroup()
	}()
	return listener.Addr().String(), nil
}

// Complete preserves successful output and strips provider text from failures.
func (p *liveProvider) Complete(ctx context.Context, req *model.Request) (*model.Response, error) {
	response, err := p.inner.Complete(ctx, req)
	if err != nil {
		return nil, liveModelError(p.name, err)
	}
	return response, nil
}

// Stream never returns a typed-nil stream or an inner stream with an error.
func (p *liveProvider) Stream(ctx context.Context, req *model.Request) (model.Streamer, error) {
	stream, err := p.inner.Stream(ctx, req)
	if err != nil {
		if !nilLiveStream(stream) {
			err = errors.Join(err, stream.Close())
		}
		return nil, liveModelError(p.name, err)
	}
	if nilLiveStream(stream) {
		return nil, liveModelError(p.name, errors.New("nil model stream"))
	}
	return &liveStream{inner: stream, name: p.name}, nil
}

// CountTokens preserves the optional native counter's contract. Advertising
// this wrapper as a counter never invents support for a provider without one.
func (p *liveProvider) CountTokens(ctx context.Context, req *model.Request) (model.TokenCount, error) {
	counter, ok := p.inner.(model.TokenCounter)
	if !ok {
		return model.TokenCount{}, model.ErrTokenCountingUnsupported
	}
	count, err := counter.CountTokens(ctx, req)
	if err != nil {
		return model.TokenCount{}, liveModelError(p.name, err)
	}
	return count, nil
}

// Recv preserves literal EOF; wrapping it would turn completion into failure.
func (s *liveStream) Recv() (model.Chunk, error) {
	chunk, err := s.inner.Recv()
	if err != nil {
		return nil, liveModelError(s.name, err)
	}
	return chunk, nil
}

// Response returns the provider-owned response only after clean EOF.
func (s *liveStream) Response() *model.Response { return s.inner.Response() }

// Close releases the stream while protecting an error from the SDK.
func (s *liveStream) Close() error { return liveModelError(s.name, s.inner.Close()) }

// liveModelError retains model error classifications and numeric usage, never
// the provider's message or cause, which can quote credentials or endpoint URLs.
func liveModelError(name string, err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	safe := liveModelFailure(name, err)
	if usage := model.UsageFromError(err); usage != nil {
		if model.UsageIncompleteFromError(err) {
			return model.RetainIncompleteUsage(safe, *usage)
		}
		return model.RetainUsage(safe, *usage)
	}
	return safe
}

// liveModelFailure constructs a failure without retaining any unsafe cause.
func liveModelFailure(name string, err error) error {
	for _, sentinel := range []error{
		context.Canceled, context.DeadlineExceeded, model.ErrRateLimited,
		model.ErrStreamingUnsupported, model.ErrStructuredOutputUnsupported,
		model.ErrToolSearchUnsupported, model.ErrToolContentUnsupported,
		model.ErrTokenCountingUnsupported, model.ErrEmptyStream,
		model.ErrRequestByteCapacity, model.ErrImageSourceCapacity,
	} {
		if errors.Is(err, sentinel) {
			return sentinel
		}
	}
	if rejected := liveOutputRejection(err); rejected != nil {
		fixed, restoreErr := model.RestoreOutputValidationError(rejected.Kind(),
			fmt.Errorf("model provider %s returned invalid output: %s", name, rejected.Kind()),
			rejected.Evidence(), rejected.Usage())
		if restoreErr == nil {
			if correction := rejected.RecoveryCorrection(); correction != "" {
				if corrected, err := model.RestoreCorrectableOutputValidationError(fixed, correction); err == nil {
					return corrected
				}
			}
			return fixed
		}
	}
	if provider, ok := model.AsProviderError(err); ok {
		return model.NewProviderError(provider.Provider(), provider.Operation(), provider.HTTPStatus(),
			provider.Kind(), provider.Code(),
			fmt.Sprintf("model provider %s refused the request: %s, HTTP %d", name, provider.Kind(), provider.HTTPStatus()),
			provider.RequestID(), provider.Retryable(), nil)
	}
	return fmt.Errorf("model provider %s could not be reached", name)
}

// liveSettingsSummary names the user action without quoting a setting value.
func liveSettingsSummary(provider string) string {
	return fmt.Sprintf("The project server's %s settings could not be used; the user restarts Surla from a shell with corrected settings.", provider)
}

// liveSuiteSummary names the owner who can repair this profile's live program.
func liveSuiteSummary(plan verify.Plan) string {
	if plan.Tests == verify.TestsWritten {
		return "The accepted tests did not start a live target. If the service at this commit fails at startup, fix it and commit; otherwise tell the user: the test writer corrects its acceptance package and reports again."
	}
	return summaryLiveSuite
}

// writtenLivePreparationSummary uses the structured stage, never error prose,
// to name a builder action without claiming which module failed to build.
func writtenLivePreparationSummary(stage *verify.StageError) string {
	if stage != nil && stage.Stage == verify.StageBuild {
		return "The service or accepted test module did not build. Run check_project, fix the service at this commit and commit; if the accepted tests need repair, tell the user so the test writer can correct its acceptance package and report again."
	}
	return "The live target's service or accepted test module could not be prepared. Check the candidate commit with check_project; tell the user if the test writer must correct its acceptance package and report again."
}

// nilLiveStream recognizes nil interfaces and typed nils without assuming a
// stream must be implemented by a pointer rather than a value.
func nilLiveStream(stream model.Streamer) bool {
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

// liveOutputRejection follows one cause chain, never promoting one branch of
// a joined provider/cleanup failure into an exact rejected-output result.
func liveOutputRejection(err error) *model.OutputValidationError {
	for err != nil {
		if rejected, ok := err.(*model.OutputValidationError); ok {
			return rejected
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return nil
		}
		err = wrapped.Unwrap()
	}
	return nil
}
