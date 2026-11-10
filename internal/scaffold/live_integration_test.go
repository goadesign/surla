//go:build scaffold

// This file is an external test package: it drives a generated starter
// through the control plane, and the control plane imports the starter
// package (scaffold) to initialize projects, so an internal test here would
// form an import cycle.
package scaffold_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/scaffold"
	"github.com/goadesign/surla/internal/verify"
)

// scriptedAnalyzer is a scripted CP1 analyzer: it returns a fixed analysis
// with no open questions, so it proves the Change Card plumbing, not live
// CP1 model judgment.
type scriptedAnalyzer struct{}

// echoSystem is a model provider that answers every request with the system
// text it received, so a test sees which prompt text a service sent.
type echoSystem struct{}

// echoStream replays one answer.
type echoStream struct {
	// chunks are returned in order; response is the final response.
	chunks   []model.Chunk
	response *model.Response
}

// published is the Surla version the starter's go.mod pins; it exists on the
// module proxy. It matches the internal tests' fixture of the same name.
const published = "v0.0.0-20261110163737-d9beac313142"

// TestStarterLiveTargetAppliesADraftToTheNextModelRequest creates a real
// starter project, commits it as C1, and runs the full CP3 journey through the
// control plane for one confirmed Change Card, with real sandboxed builds and
// processes and a fake model that echoes the system prompt:
// start_live_target builds C1 and starts it with the generated suite's --live
// mode through a loopback model gateway, draft_override replaces the helper's
// system prompt on it, the next question's model request carries the drafted
// text, and the card keeps its revision. The builder then writes that text
// into source and commits C2: a live target for C2 sends it with no draft,
// and verify_change of C2 for the same card passes. A second card skips CP3
// and verifies C1 directly. The CP1 analyzer is scripted. It needs the
// network and, for the sandbox, macOS.
func TestStarterLiveTargetAppliesADraftToTheNextModelRequest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shop")
	name, err := scaffold.Init(t.Context(), scaffold.Options{Dir: dir, WorkDir: t.TempDir(), Module: "example.com/shop", SurlaVersion: published}, scaffold.RunCommand, designeval.NewEvaluator())
	require.NoError(t, err)
	commit := commitAll(t, dir)

	surlaBin := filepath.Join(t.TempDir(), "surla")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", surlaBin, "./cmd/surla")
	build.Dir = filepath.Join("..", "..")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	gatewayAddr := serveEchoGateway(t)
	cardsDir, runsDir := t.TempDir(), t.TempDir()
	cp, err := controlplane.New(controlplane.Options{
		CardsDir: cardsDir,
		Analyzer: scriptedAnalyzer{},
		Verification: &controlplane.VerificationConfig{
			RunsDir: runsDir,
			Plan: verify.Plan{
				CandidateRepo: dir, CandidateModule: ".", Binaries: map[string]string{name: "./cmd/" + name},
				SuiteRepo: dir, SuiteCommit: commit, SuiteModule: "eval", SuiteCommand: "./cmd/" + name + "-evals",
				ReportDir: t.TempDir(),
			},
			Command: []string{surlaBin},
			Timeout: 15 * time.Minute,
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
	card := confirmCard(t, cp, alice, "starter-live-prompt")

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
	afterDraft, err := cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: card.ChangeID})
	require.NoError(t, err)
	assert.Equal(t, card.RequestRevision, afterDraft.RequestRevision, "a runtime draft never changes the card")
	assert.Equal(t, controlplane.StatusConfirmed, afterDraft.Status)

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

	assertVerified(t, cp, alice, runsDir, card, c2, "the committed text verifies for the same card")

	skipped := confirmCard(t, cp, alice, "starter-skip-cp3")
	assertVerified(t, cp, alice, runsDir, skipped, commit, "a card that skips CP3 verifies C1 directly")
}

// keepRun copies a failed run's directory to $SURLA_TEST_KEEP_RUNS, when it
// is set, so its evidence outlives the test's temporary directories.
func keepRun(t *testing.T, runsDir, runID string) {
	t.Helper()
	dest := os.Getenv("SURLA_TEST_KEEP_RUNS")
	if dest == "" {
		return
	}
	out, err := exec.Command("cp", "-R", filepath.Join(runsDir, runID), dest).CombinedOutput()
	if err != nil {
		t.Logf("keep run %s: %v: %s", runID, err, out)
		return
	}
	t.Logf("kept run %s in %s", runID, dest)
}

// buildLogs returns every log and evidence file a verification run left,
// each with its name and byte count, including empty files and read errors,
// so a failed run shows exactly what was and was not written.
func buildLogs(t *testing.T, runsDir, runID string) string {
	t.Helper()
	evidence, err := filepath.Glob(filepath.Join(runsDir, runID, "work", "evidence", "*"))
	if err != nil {
		return err.Error()
	}
	runLogs, err := filepath.Glob(filepath.Join(runsDir, runID, "*.log"))
	if err != nil {
		return err.Error()
	}
	logs := append(runLogs, evidence...)
	var b strings.Builder
	for _, l := range logs {
		data, err := os.ReadFile(l)
		if err != nil {
			fmt.Fprintf(&b, "== %s: read error: %v\n", filepath.Base(l), err)
			continue
		}
		fmt.Fprintf(&b, "== %s (%d bytes)\n%s\n", filepath.Base(l), len(data), data)
	}
	return b.String()
}

// Analyze returns a fixed analysis with no open questions.
func (scriptedAnalyzer) Analyze(
	context.Context, string, controlplane.CP1Design,
) (controlplane.CP1SemanticAnalysis, error) {
	return controlplane.CP1SemanticAnalysis{
		Summary: "Answer price questions in a friendlier tone.",
		Examples: []controlplane.CP1ModelExample{{
			Scenario: "Tea price", InputSummary: "How much is tea?",
			ExpectedOutcome: "Answers $3.50", VerificationKind: "deterministic",
		}},
		PreservedBehaviors: []string{"Prices come only from lookup_price."},
	}, nil
}

// Revise is never called by this journey; it returns a fixed revision analysis,
// so the scripted analyzer satisfies controlplane.CP1Analyzer.
func (a scriptedAnalyzer) Revise(
	ctx context.Context,
	prior controlplane.CP1PriorCard,
	prompt string,
	design controlplane.CP1Design,
) (controlplane.CP1RevisionSemanticAnalysis, error) {
	analysis, err := a.Analyze(ctx, prompt, design)
	if err != nil {
		return controlplane.CP1RevisionSemanticAnalysis{}, err
	}
	var priorID string
	if len(prior.Examples) > 0 {
		priorID = prior.Examples[0].ID
	}
	return controlplane.CP1RevisionSemanticAnalysis{
		Summary: analysis.Summary,
		Examples: []controlplane.CP1RevisionModelExample{{
			PriorID:          priorID,
			Scenario:         analysis.Examples[0].Scenario,
			InputSummary:     analysis.Examples[0].InputSummary,
			ExpectedOutcome:  analysis.Examples[0].ExpectedOutcome,
			VerificationKind: analysis.Examples[0].VerificationKind,
		}},
		RemovedExampleIDs:  []string{},
		PreservedBehaviors: analysis.PreservedBehaviors,
	}, nil
}

// confirmCard proposes changeID with the scripted analyzer and confirms it,
// returning the confirmed card.
func confirmCard(t *testing.T, cp *controlplane.ControlPlane, ctx context.Context, changeID string) *gensurla.ChangeCardResult {
	t.Helper()
	card, err := cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{ChangeID: changeID, Prompt: "Answer price questions in a friendlier tone."})
	require.NoError(t, err)
	if card.Status != controlplane.StatusConfirmed {
		card, err = cp.ConfirmIntent(ctx, &gensurla.ConfirmIntentPayload{
			ChangeID: changeID, RequestRevision: card.RequestRevision, Answers: map[string]string{}, Examples: card.Examples,
		})
		require.NoError(t, err)
	}
	require.Equal(t, controlplane.StatusConfirmed, card.Status)
	return card
}

// assertVerified runs verify_change for commit on card and waits, within a
// phase deadline, until the run completes. It fails at once on a run that
// ends without a report or a report that does not pass.
func assertVerified(t *testing.T, cp *controlplane.ControlPlane, ctx context.Context, runsDir string, card *gensurla.ChangeCardResult, commit, why string) {
	t.Helper()
	run, err := cp.VerifyChange(ctx, &gensurla.VerifyChangePayload{ChangeID: card.ChangeID, RequestRevision: card.RequestRevision, CandidateCommit: commit})
	require.NoError(t, err)
	phase, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	for {
		status, err := cp.GetVerification(ctx, &gensurla.GetVerificationPayload{RunID: run.RunID})
		require.NoError(t, err)
		require.Equal(t, card.RequestRevision, status.RequestRevision)
		require.Equal(t, commit, status.CandidateCommit)
		if done, ok := status.State.AsCompleted(); ok {
			assert.True(t, done.Report.Passed, "%s: %+v", why, done.Report)
			return
		}
		if failed, ok := status.State.AsNotCompleted(); ok {
			keepRun(t, runsDir, run.RunID)
			t.Fatalf("%s: the run ended without a report: %+v\n%s", why, failed, buildLogs(t, runsDir, run.RunID))
		}
		if _, ok := status.State.AsRunning(); !ok {
			t.Fatalf("%s: unexpected verification state %v", why, status.State.Kind())
		}
		select {
		case <-phase.Done():
			t.Fatalf("%s: verification did not finish: %v", why, phase.Err())
		case <-time.After(2 * time.Second):
		}
	}
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
