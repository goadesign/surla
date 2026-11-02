//go:build scaffold

package scaffold

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa-ai/features/model/gateway"
	"goa.design/goa-ai/runtime/agent/model"
	"google.golang.org/grpc"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/verify"
)

// echoSystem is a model provider that answers every request with the system
// text it received, so a test sees which prompt text a service sent.
type echoSystem struct{}

// echoStream replays one answer.
type echoStream struct {
	// chunks are returned in order; response is the final response.
	chunks   []model.Chunk
	response *model.Response
}

// TestStarterLiveTargetAppliesADraftToTheNextModelRequest creates a real
// starter project, commits it as C1, and runs the full CP3 journey through the
// control plane, with real sandboxed builds and processes: start_live_target
// builds C1 and starts it with the generated suite's --live mode through a
// loopback model gateway, draft_override replaces the helper's system prompt
// on it, and the next question's model request carries the drafted text. The
// builder then writes that text into source and commits C2: a live target
// for C2 sends it with no draft, and verification of C2 passes. It needs the
// network and, for the sandbox, macOS.
func TestStarterLiveTargetAppliesADraftToTheNextModelRequest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shop")
	name, err := Init(t.Context(), Options{Dir: dir, Module: "example.com/shop", SurlaVersion: published}, RunCommand)
	require.NoError(t, err)
	commit := commitAll(t, dir)

	surlaBin := filepath.Join(t.TempDir(), "surla")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", surlaBin, "./cmd/surla")
	build.Dir = filepath.Join("..", "..")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	gatewayAddr := serveEchoGateway(t)
	cardsDir := t.TempDir()
	cp, err := controlplane.New(controlplane.Options{
		CardsDir: cardsDir,
		Verification: &controlplane.VerificationConfig{
			RunsDir: t.TempDir(),
			Plan: verify.Plan{
				CandidateRepo: dir, CandidateModule: ".", Binaries: map[string]string{name: "./cmd/" + name},
				SuiteRepo: dir, SuiteCommit: commit, SuiteModule: "eval", SuiteCommand: "./cmd/" + name + "-evals",
			},
		},
		Live: &controlplane.LiveConfig{
			Command:      []string{surlaBin},
			Env:          append(os.Environ(), "SURLA_LIVE_MODEL_GATEWAY="+gatewayAddr),
			StartTimeout: 10 * time.Minute,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })
	alice := controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: "alice@example.com"})

	started, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: commit})
	require.NoError(t, err)
	before := askLive(t, started.URL)
	assert.True(t, strings.HasPrefix(before, "SYSTEM="), before)

	promptID := name + ".helper.system"
	const text = "You answer questions about item prices. [drafted]"
	drafted, err := cp.DraftOverride(alice, &gensurla.DraftOverridePayload{
		TargetKind: controlplane.TargetKindPromptTemplate, PromptID: &promptID, NewContent: text,
	})
	require.NoError(t, err)
	assert.True(t, drafted.AppliedInStore)
	assert.Equal(t, "SYSTEM="+text, askLive(t, started.URL), "the next model request carries the drafted text")

	// The builder keeps the accepted text by writing it into source and
	// committing C2; the draft itself is never what CP4 verifies.
	planner := filepath.Join(dir, "internal", "helper", "planner.go")
	src, err := os.ReadFile(planner)
	require.NoError(t, err)
	start := strings.Index(string(src), "systemPrompt = `")
	end := strings.Index(string(src)[start+len("systemPrompt = `"):], "`")
	require.True(t, start >= 0 && end >= 0, "the starter declares its system prompt as a raw string")
	end += start + len("systemPrompt = `")
	edited := string(src[:start]) + "systemPrompt = `" + text + "`" + string(src[end+1:])
	require.NoError(t, os.WriteFile(planner, []byte(edited), 0o600))
	c2 := commitAll(t, dir)

	restarted, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: c2})
	require.NoError(t, err, "starting C2 replaces the C1 target")
	assert.Equal(t, "SYSTEM="+text, askLive(t, restarted.URL), "C2 carries the committed text without any draft")
	stopped, err := cp.StopLiveTarget(alice)
	require.NoError(t, err)
	assert.True(t, stopped.Stopped)

	report, err := verify.Run(t.Context(), verify.Plan{
		CandidateRepo: dir, CandidateCommit: c2, CandidateModule: ".", Binaries: map[string]string{name: "./cmd/" + name},
		SuiteRepo: dir, SuiteCommit: c2, SuiteModule: "eval", SuiteCommand: "./cmd/" + name + "-evals",
		RequestRevision: strings.Repeat("r", 64), WorkDir: filepath.Join(t.TempDir(), "work"), ReportDir: t.TempDir(),
	})
	require.NoError(t, err)
	assert.True(t, report.Passed, "verification of the committed text passes: %+v", report)
}

// Complete is not used; the starter's planner streams.
func (echoSystem) Complete(context.Context, *model.Request) (*model.Response, error) {
	return nil, errors.New("echo model supports streaming only")
}

// Stream answers with "SYSTEM=" and the request's system text.
func (echoSystem) Stream(_ context.Context, req *model.Request) (model.Streamer, error) {
	var system strings.Builder
	for _, msg := range req.Messages {
		if msg.Role != model.ConversationRoleSystem {
			continue
		}
		for _, part := range msg.Parts {
			if text, ok := part.(model.TextPart); ok {
				system.WriteString(text.Text)
			}
		}
	}
	answer := "SYSTEM=" + system.String()
	msg := model.Message{Role: model.ConversationRoleAssistant, Parts: []model.Part{model.TextPart{Text: answer}}}
	return &echoStream{
		chunks:   []model.Chunk{model.TextChunk{Message: msg}, model.StopChunk{Reason: "end_turn"}},
		response: &model.Response{Content: []model.Message{msg}, StopReason: "end_turn"},
	}, nil
}

// Recv returns the next chunk or io.EOF.
func (s *echoStream) Recv() (model.Chunk, error) {
	if len(s.chunks) == 0 {
		return nil, io.EOF
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, nil
}

// Response returns the final response.
func (s *echoStream) Response() *model.Response {
	return s.response
}

// Close releases nothing.
func (s *echoStream) Close() error {
	return nil
}

// commitAll commits every file in dir, creating its git repository on the
// first call, and returns the commit ID.
func commitAll(t *testing.T, dir string) string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		cmd := exec.CommandContext(t.Context(), "git", "init", "-q")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "starter"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

// serveEchoGateway serves echoSystem through a goa-ai gateway on a loopback
// port for the test's lifetime and returns its address.
func serveEchoGateway(t *testing.T) string {
	t.Helper()
	gw, err := gateway.NewServer(gateway.WithProvider(echoSystem{}))
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	gateway.RegisterGRPCServer(server, gw)
	served := make(chan error, 1)
	go func() { served <- server.Serve(ln) }()
	t.Cleanup(func() {
		server.Stop()
		if err := <-served; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("echo gateway: %v", err)
		}
	})
	return ln.Addr().String()
}

// askLive asks the live starter a question over its public API and returns
// the answer.
func askLive(t *testing.T, origin string) string {
	t.Helper()
	resp, err := http.Post(origin+"/ask", "application/json", strings.NewReader(`{"question":"How much is tea?"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out struct {
		Answer string `json:"answer"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	return out.Answer
}
