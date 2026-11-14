// These tests start disposable executor groups to exercise gateway lifetime.
package verifyrun

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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
	// heldLiveStream blocks the service's stream until its executor is killed.
	heldLiveStream struct{ ctx context.Context }
	// stoppedLiveListener makes Serve return, exercising the exact production
	// goroutine in a disposable executor rather than killing the test process.
	stoppedLiveListener struct{}
)

// Recv blocks until the incoming service request is canceled.
func (s heldLiveStream) Recv() (model.Chunk, error) {
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

// Response has no completed response for a held stream.
func (heldLiveStream) Response() *model.Response { return nil }

// Close ends no process; executor ownership must perform that cleanup.
func (heldLiveStream) Close() error { return nil }

// Accept fails as if the gateway listener stopped serving.
func (stoppedLiveListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }

// Close owns no OS resource.
func (stoppedLiveListener) Close() error { return nil }

// Addr keeps the listener's address within loopback for grpc's server setup.
func (stoppedLiveListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// callFakeLiveModel is a suite-side consumer of the real generated gateway.
// Its mode arrives in artifact flags, since written tests receive no test-only
// executor environment. Errors are not printed: the parent checks process
// outcome and the executor's protected private diagnostic tail.
func callFakeLiveModel(mode string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(os.Getenv(verify.LiveModelGatewayEnv),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return 2
	}
	defer conn.Close()
	client, err := gateway.NewGRPCClient(conn)
	if err != nil {
		return 2
	}
	request := &model.Request{MaxTokens: 16, Messages: []*model.Message{{
		Role: model.ConversationRoleUser, Parts: []model.Part{model.TextPart{Text: "Hello"}},
	}}}
	if mode == "gateway-stream-panic" || mode == "gateway-hold" {
		stream, err := client.Stream(ctx, request)
		if err == nil {
			stream.Recv()
			stream.Close()
		}
	} else {
		client.Complete(ctx, request)
	}
	return 0
}

// TestLiveModelGatewayEndsWithItsExecutor proves panic containment, protected
// pre-ready diagnostics and port cleanup, including a stream held by a service.
func TestLiveModelGatewayEndsWithItsExecutor(t *testing.T) {
	isolateProviderFamilies(t)
	for _, tc := range []struct {
		mode                       string
		ready, explicitStop, panic bool
	}{
		{mode: "gateway-ready", ready: true, explicitStop: true},
		{mode: "gateway-hold", ready: true, explicitStop: true},
		{mode: "gateway-ends", ready: true},
		{mode: "gateway-panic", ready: true, panic: true},
		{mode: "gateway-stream-panic", ready: true, panic: true},
		{mode: "gateway-before-panic", panic: true},
		{mode: "gateway-serve-ends", ready: true},
		{mode: "gateway-constructor-fails"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv(liveFakeEnv, tc.mode)
			self, err := os.Executable()
			require.NoError(t, err)
			held, err := modelsettings.New("anthropic", map[string]string{"ANTHROPIC_API_KEY": "sentinel-gateway-key"})
			require.NoError(t, err)
			env := append(os.Environ(), verify.LiveModelProviderEnv+"=anthropic", verify.LiveModelNameEnv+"=fixture")
			p, err := LaunchLive(t.TempDir(), verify.Plan{Tests: verify.TestsWritten}, "secret-key", []string{self}, env,
				[]modelsettings.Settings{held})
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, p.Stop()) })
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			err = p.AwaitReady(ctx)
			if tc.ready {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				var failed *LiveStartError
				require.ErrorAs(t, err, &failed)
				assert.NotContains(t, failed.Summary, "sentinel-gateway-key")
				assert.NotContains(t, failed.Detail, "sentinel-gateway-key")
			}
			address := ""
			if tc.mode != "gateway-constructor-fails" {
				address = gatewayAddress(t, p.Dir())
			}
			if tc.mode == "gateway-hold" {
				require.Eventually(t, func() bool {
					_, err := os.Stat(filepath.Join(p.Dir(), liveTmpDir, "gateway-call"))
					return err == nil
				}, 5*time.Second, 10*time.Millisecond)
			}
			if !tc.explicitStop {
				select {
				case <-p.Done():
				case <-ctx.Done():
					t.Fatal("executor did not end")
				}
			}
			if tc.panic {
				raw, err := os.ReadFile(filepath.Join(p.Dir(), "executor.log"))
				require.NoError(t, err)
				assert.Contains(t, string(raw), "panic: sentinel-gateway-key", "the injected panic actually happened")
			}
			require.NoError(t, p.Stop())
			requireGroupGone(t, p.pgid)
			assert.NoDirExists(t, p.Dir())
			if address != "" {
				assertGatewayPortClosed(t, address)
			}
		})
	}
}

// gatewayAddress reads only the address the executor gave its test program.
func gatewayAddress(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, liveTmpDir, "suite-env"))
	require.NoError(t, err)
	address, ok := lookupEnv(strings.Split(string(raw), "\n"), verify.LiveModelGatewayEnv)
	require.True(t, ok)
	host, _, err := net.SplitHostPort(address)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", host)
	return address
}

// TestLiveGatewayErrorsDoNotInventAnOutputRejection rejects a mixed provider
// failure and keeps literal EOF's role separate from wrapped transport errors.
func TestLiveGatewayErrorsDoNotInventAnOutputRejection(t *testing.T) {
	rejected, err := model.RestoreOutputValidationError(model.OutputValidationResponseShape,
		errors.New("rejected"), model.ResponseEvidence{}, nil)
	require.NoError(t, err)
	assert.Nil(t, liveOutputRejection(errors.Join(rejected, errors.New("cleanup failed"))))
	assert.Same(t, rejected, liveOutputRejection(errors.Join(rejected, rejected)))
}

// assertGatewayPortClosed checks the model listener after its owner confirmed
// process-group cleanup; a successful connection cannot count as shutdown.
func assertGatewayPortClosed(t *testing.T, address string) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if connection != nil {
		assert.NoError(t, connection.Close())
	}
	assert.Error(t, err, "gateway port outlived the executor")
}

// TestLiveExecutorDiesWithItsHostWhileReadingSettings leaves standard input
// incomplete and open. Closing the control pipe must still end the group, so
// waiting for the NUL payload's EOF never weakens parent-process ownership.
func TestLiveExecutorDiesWithItsHostWhileReadingSettings(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	control, keep, err := os.Pipe()
	require.NoError(t, err)
	defer keep.Close()
	input, inputWriter, err := os.Pipe()
	require.NoError(t, err)
	defer inputWriter.Close()
	cmd := exec.Command(self, "verify-run", "live", t.TempDir())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{control}
	cmd.Stdin = input
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	require.NoError(t, cmd.Start())
	require.NoError(t, control.Close())
	require.NoError(t, input.Close())
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
	})
	_, err = inputWriter.Write([]byte("ANTHROPIC_API_KEY=incomplete"))
	require.NoError(t, err)
	require.NoError(t, keep.Close())
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit)
		wait, ok := exit.Sys().(syscall.WaitStatus)
		require.True(t, ok)
		assert.Equal(t, syscall.SIGKILL, wait.Signal())
	case <-time.After(5 * time.Second):
		t.Fatal("executor remained blocked reading settings after its host closed")
	}
	requireGroupGone(t, cmd.Process.Pid)
}
