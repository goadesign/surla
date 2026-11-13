package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	goahttp "goa.design/goa/v3/http"
	goa "goa.design/goa/v3/pkg"
	"goa.design/goa/v3/security"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	genpromptsserver "github.com/goadesign/surla/internal/targetprompt/gen/http/prompts/server"
	genprompts "github.com/goadesign/surla/internal/targetprompt/gen/prompts"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
	"github.com/goadesign/surla/verify/suite"
)

const (
	// liveTargetModeEnv selects what the fake live target does: "serve"
	// serves the prompt operation; "drop" closes every update's connection
	// without answering; "leaky" rejects templates with a message quoting a
	// path, a key and a URL; "version" accepts with the version in
	// liveTargetVersionEnv; "other-prompt" accepts but answers for another
	// prompt ID; "fail" makes the suite exit before it
	// is ready, and "fail-pinned" does so after leaving a file its cleanup
	// cannot remove; "prepare-hangs" blocks preparation until it is canceled.
	liveTargetModeEnv = "CONTROLPLANE_TEST_LIVE_TARGET"
	// liveTargetVersionEnv is the version the "version" mode returns.
	liveTargetVersionEnv = "CONTROLPLANE_TEST_LIVE_VERSION"
	// fakePromptID is the one prompt the fake target registers.
	fakePromptID = "homes.helper.system"
)

// fakeTarget implements the target side of the prompt operation from the
// same generated contract a real target declares.
type fakeTarget struct {
	// key is the key the target was started with.
	key string
	// mu guards last, the text of the last accepted update.
	mu   sync.Mutex
	last string
}

// TestMain lets the test binary act as the live executor (`verify-run live`)
// and as the evaluator suite's --live mode, so StartLiveTarget runs real
// processes, and as the check helper (`check-run`) and a server that tests
// kill mid-operation (`check-host <cards> <workspace> <tool>`).
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "check-run" {
		os.Exit(runTestCheckHelper())
	}
	if len(os.Args) == 5 && os.Args[1] == "check-host" {
		os.Exit(checkHost(os.Args[2], os.Args[3], os.Args[4]))
	}
	if len(os.Args) == 4 && os.Args[1] == "verify-run" && os.Args[2] == "live" {
		if err := verifyrun.Live(os.Args[3], fakeLivePrepare, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) >= 2 && os.Args[1] == "--live" {
		os.Exit(serveFakeTarget())
	}
	os.Exit(m.Run())
}

// OverridePrompt replaces the fake prompt's text.
func (f *fakeTarget) OverridePrompt(_ context.Context, p *genprompts.OverridePromptPayload) (*genprompts.OverridePromptResult, error) {
	switch os.Getenv(liveTargetModeEnv) {
	case "leaky":
		return nil, genprompts.MakeInvalidPrompt(errors.New("parse /private/secret/path with key=abc123 from http://10.0.0.1/admin"))
	case "version":
		return &genprompts.OverridePromptResult{PromptID: p.PromptID, Version: os.Getenv(liveTargetVersionEnv)}, nil
	case "other-prompt":
		return &genprompts.OverridePromptResult{PromptID: "/private/secret/path?key=abc123", Version: "v1"}, nil
	}
	if p.PromptID != fakePromptID {
		return nil, genprompts.MakeUnknownPrompt(fmt.Errorf("no prompt %s", p.PromptID))
	}
	if strings.Contains(p.Template, "{{") && !strings.Contains(p.Template, "}}") {
		return nil, genprompts.MakeInvalidPrompt(errors.New("template: unclosed action"))
	}
	f.mu.Lock()
	f.last = p.Template
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(p.Template))
	return &genprompts.OverridePromptResult{PromptID: p.PromptID, Version: "sha256:" + hex.EncodeToString(sum[:])}, nil
}

// APIKeyAuth accepts only the key the target was started with.
func (f *fakeTarget) APIKeyAuth(ctx context.Context, key string, _ *security.APIKeyScheme) (context.Context, error) {
	if key != f.key {
		return ctx, genprompts.Unauthorized("wrong key")
	}
	return ctx, nil
}

// fakeLivePrepare returns this test binary as the suite.
func fakeLivePrepare(ctx context.Context, _ verify.Plan) (verify.Prepared, error) {
	if os.Getenv(liveTargetModeEnv) == "prepare-hangs" {
		fmt.Fprintln(os.Stderr, "preparing: downloading modules")
		<-ctx.Done()
		return verify.Prepared{}, ctx.Err()
	}
	self, err := os.Executable()
	if err != nil {
		return verify.Prepared{}, err
	}
	return verify.Prepared{SuiteBin: self}, nil
}

// serveFakeTarget acts as `<suite> --live`: it serves the prompt operation on
// a loopback port with the key from the environment, plus GET /last, which
// returns the last accepted text, and prints the ready line.
func serveFakeTarget() int {
	mode := os.Getenv(liveTargetModeEnv)
	switch mode {
	case "fail":
		fmt.Fprintln(os.Stderr, "no provider key")
		return 2
	case "fail-pinned":
		// A file the suite cannot remove makes the owner's cleanup fail.
		pinned := filepath.Join(os.TempDir(), "immutable")
		if err := os.WriteFile(pinned, []byte("x"), 0o600); err != nil {
			return 2
		}
		if err := exec.Command("chflags", "uchg", pinned).Run(); err != nil {
			return 2
		}
		fmt.Fprintln(os.Stderr, "no provider key")
		return 2
	}
	target := &fakeTarget{key: os.Getenv(suite.LiveKeyEnv)}
	mux := goahttp.NewMuxer()
	srv := genpromptsserver.New(genprompts.NewEndpoints(target), mux, goahttp.RequestDecoder, goahttp.ResponseEncoder, nil, nil)
	genpromptsserver.Mount(mux, srv)
	var handler http.Handler = mux
	if mode == "drop" {
		handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		})
	}
	root := http.NewServeMux()
	root.Handle("/prompts/", handler)
	root.HandleFunc("POST /exit", func(http.ResponseWriter, *http.Request) {
		// The suite exiting stands in for a target that ends on its own.
		os.Exit(3)
	})
	root.HandleFunc("GET /last", func(w http.ResponseWriter, _ *http.Request) {
		target.mu.Lock()
		defer target.mu.Unlock()
		fmt.Fprint(w, target.last)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 2
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"url": "http://" + ln.Addr().String()}); err != nil {
		return 2
	}
	if err := http.Serve(ln, root); err != nil {
		return 2
	}
	return 0
}

// newLiveControlPlane returns a control plane whose live targets are this
// test binary in the given mode.
func newLiveControlPlane(t *testing.T, mode string) (*ControlPlane, string) {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	cardsDir := t.TempDir()
	cp, err := New(Options{
		CardsDir:     cardsDir,
		Verification: &VerificationConfig{RunsDir: t.TempDir()},
		Live: &LiveConfig{
			Command: []string{self},
			Env:     append(os.Environ(), liveTargetModeEnv+"="+mode), StartTimeout: time.Minute,
			Model: noModel(),
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })
	return cp, LiveRoot(cardsDir)
}

// draft sends text for promptID through draft_override.
func draft(ctx context.Context, cp *ControlPlane, promptID, text string) (*gensurla.DraftOverrideResult, error) {
	return cp.DraftOverride(ctx, &gensurla.DraftOverridePayload{TargetKind: TargetKindPromptTemplate, PromptID: &promptID, NewContent: text})
}

// lastText returns the text the live target last accepted.
func lastText(t *testing.T, origin string) string {
	t.Helper()
	resp, err := http.Get(origin + "/last")
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

// goaErrorName returns the Goa error name of err, or "".
func goaErrorName(err error) string {
	var se *goa.ServiceError
	if errors.As(err, &se) {
		return se.Name
	}
	return ""
}

func TestLiveTargetAppliesDraftsThroughTheGeneratedContract(t *testing.T) {
	cp, root := newLiveControlPlane(t, "serve")
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	commit := strings.Repeat("a", 40)

	res, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: commit})
	require.NoError(t, err)
	assert.Equal(t, commit, res.CandidateCommit)
	assert.True(t, strings.HasPrefix(res.URL, "http://127.0.0.1:"), res.URL)

	out, err := draft(alice, cp, fakePromptID, "Answer in French.")
	require.NoError(t, err)
	assert.True(t, out.AppliedInStore)
	sum := sha256.Sum256([]byte("Answer in French."))
	assert.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), *out.PromptVersion)
	assert.Contains(t, out.Message, "It is not verified")
	assert.Equal(t, "Answer in French.", lastText(t, res.URL), "the text reached the target")

	cases := []struct {
		name     string
		promptID string
		text     string
		wantErr  string
	}{
		{name: "unknown prompt", promptID: "nope", text: "x", wantErr: "prompt_not_found"},
		{name: "invalid template", promptID: fakePromptID, text: "{{ .Missing", wantErr: "invalid_prompt"},
	}
	for _, tc := range cases {
		_, err := draft(alice, cp, tc.promptID, tc.text)
		require.Error(t, err, tc.name)
		assert.Equal(t, tc.wantErr, goaErrorName(err), tc.name)
	}
	assert.Equal(t, "Answer in French.", lastText(t, res.URL), "rejected drafts change nothing")

	stopped, err := cp.StopLiveTarget(alice)
	require.NoError(t, err)
	assert.True(t, stopped.Stopped)
	dirs, err := verifyrun.LeftoverLiveDirs(root)
	require.NoError(t, err)
	assert.Empty(t, dirs)
	_, err = draft(alice, cp, fakePromptID, "x")
	assert.Equal(t, "no_live_target", goaErrorName(err), "after a stop, drafts are refused until a live target starts again")
	again, err := cp.StopLiveTarget(alice)
	require.NoError(t, err)
	assert.False(t, again.Stopped, "stopping when none runs is not an error")
}

func TestLiveTargetRejectsAWrongKey(t *testing.T) {
	cp, _ := newLiveControlPlane(t, "serve")
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	_, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	require.NoError(t, err)
	lt := cp.boundLive()
	require.NotNil(t, lt)
	lt.key = "not-the-key"
	_, err = draft(alice, cp, fakePromptID, "x")
	assert.Equal(t, "target_rejected_credential", goaErrorName(err))
}

func TestLiveTargetReportsAnUnconfirmedUpdate(t *testing.T) {
	cp, _ := newLiveControlPlane(t, "drop")
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	_, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	require.NoError(t, err)
	_, err = draft(alice, cp, fakePromptID, "x")
	assert.Equal(t, "update_not_confirmed", goaErrorName(err))
}

func TestLiveTargetThatEndsIsUnboundAndCleanedUp(t *testing.T) {
	cp, _ := newLiveControlPlane(t, "serve")
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	res, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	require.NoError(t, err)
	lt := cp.boundLive()
	require.NotNil(t, lt)
	resp, err := http.Post(res.URL+"/exit", "text/plain", nil)
	if err == nil {
		resp.Body.Close()
	}
	require.Eventually(t, func() bool { return cp.boundLive() == nil }, 30*time.Second, 20*time.Millisecond,
		"an ended target is unbound")
	<-lt.proc.Done()
	_, err = draft(alice, cp, fakePromptID, "x")
	assert.Equal(t, "no_live_target", goaErrorName(err), "later drafts find no live target")
	stopped, err := cp.StopLiveTarget(alice)
	require.NoError(t, err)
	assert.False(t, stopped.Stopped, "nothing runs after the target ended and was cleaned up")
	assert.Equal(t, "live_target_ended", goaErrorName(liveDraftError(errors.New("connection reset"), lt)),
		"a draft in flight when the target ended is reported as ended")

	_, err = cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("b", 40)})
	require.NoError(t, err)
	_, err = draft(alice, cp, fakePromptID, "y")
	require.NoError(t, err, "a new start binds a running target again")
}

func TestLiveTargetStartFailuresAreTyped(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	cp, root := newLiveControlPlane(t, "fail")
	_, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	assert.Equal(t, "live_target_failed", goaErrorName(err))
	assert.Contains(t, err.Error(), "The evaluator suite did not start a live target. Check that its suite.Command sets Live")
	dirs, err := verifyrun.LeftoverLiveDirs(root)
	require.NoError(t, err)
	assert.Empty(t, dirs)

	unconfigured, err := New(Options{CardsDir: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, unconfigured.Close()) })
	_, err = unconfigured.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	assert.Equal(t, "live_target_unavailable", goaErrorName(err))

	_, err = cp.StartLiveTarget(context.Background(), &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	assert.EqualError(t, err, "authenticated principal is required")
}

func TestLiveTargetReplacementNeverReachesTwoTargets(t *testing.T) {
	cp, root := newLiveControlPlane(t, "serve")
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	first, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	require.NoError(t, err)
	oldProc := cp.boundLive().proc

	var wg sync.WaitGroup
	results := make(chan error, 40)
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, err := draft(alice, cp, fakePromptID, "during replacement")
				select {
				case results <- err:
				default:
				}
			}
		}()
	}
	second, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("b", 40)})
	close(stop)
	wg.Wait()
	close(results)
	require.NoError(t, err)
	for err := range results {
		if err == nil {
			continue
		}
		// A draft during the replacement either reached the old target before
		// it was unbound, or found no target bound; nothing else.
		allowed := goaErrorName(err) == "no_live_target" ||
			goaErrorName(err) == "update_not_confirmed" || goaErrorName(err) == "live_target_ended"
		assert.True(t, allowed, "unexpected draft error during replacement: %v", err)
	}
	select {
	case <-oldProc.Done():
	default:
		t.Fatal("the first target is still running after the replacement")
	}
	assert.NotEqual(t, first.URL, second.URL)
	dirs, err := verifyrun.LeftoverLiveDirs(root)
	require.NoError(t, err)
	assert.Len(t, dirs, 1, "only the second target's directory remains")
	_, err = draft(alice, cp, fakePromptID, "after replacement")
	require.NoError(t, err)
	assert.Equal(t, "after replacement", lastText(t, second.URL))
}

func TestCloseStopsTheLiveTarget(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	cardsDir := t.TempDir()
	root := LiveRoot(cardsDir)
	cp, err := New(Options{
		CardsDir:     cardsDir,
		Verification: &VerificationConfig{RunsDir: t.TempDir()},
		Live:         &LiveConfig{Model: noModel(), Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=serve")},
	})
	require.NoError(t, err)
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	_, err = cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	require.NoError(t, err)
	proc := cp.boundLive().proc
	require.NoError(t, cp.Close())
	select {
	case <-proc.Done():
	default:
		t.Fatal("Close left the live target running")
	}
	dirs, err := verifyrun.LeftoverLiveDirs(root)
	require.NoError(t, err)
	assert.Empty(t, dirs)
}

func TestCloseEndsAStartInProgressAndRefusesLaterStarts(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	cardsDir := t.TempDir()
	cp, err := New(Options{
		CardsDir:     cardsDir,
		Verification: &VerificationConfig{RunsDir: t.TempDir()},
		Live: &LiveConfig{Model: noModel(), Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=prepare-hangs"),
			StartTimeout: time.Hour},
	})
	require.NoError(t, err)
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	first := make(chan error, 1)
	go func() {
		_, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
		first <- err
	}()
	require.Eventually(t, func() bool {
		dirs, err := verifyrun.LeftoverLiveDirs(LiveRoot(cardsDir))
		return err == nil && len(dirs) == 1
	}, 30*time.Second, 20*time.Millisecond, "the first start is preparing")
	queued := make(chan error, 1)
	go func() {
		_, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("b", 40)})
		queued <- err
	}()

	closed := time.Now()
	require.NoError(t, cp.Close())
	assert.Less(t, time.Since(closed), 30*time.Second, "Close does not wait for the build to finish")
	assert.Equal(t, "live_target_failed", goaErrorName(<-first), "the start in progress ended")
	assert.Equal(t, "live_target_unavailable", goaErrorName(<-queued), "a queued start is refused before launching")
	_, err = cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("c", 40)})
	assert.Equal(t, "live_target_unavailable", goaErrorName(err), "a start after Close is refused")
	dirs, err := verifyrun.LeftoverLiveDirs(LiveRoot(cardsDir))
	require.NoError(t, err)
	assert.Empty(t, dirs, "nothing was launched after Close and the canceled start was cleaned up")
}

func TestAFailedStopKeepsTheOwnerAndStartsNoOtherTarget(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	self, err := os.Executable()
	require.NoError(t, err)
	cardsDir := t.TempDir()
	cp, err := New(Options{
		CardsDir:     cardsDir,
		Verification: &VerificationConfig{RunsDir: t.TempDir()},
		Live:         &LiveConfig{Model: noModel(), Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=serve")},
	})
	require.NoError(t, err)
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	_, err = cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	require.NoError(t, err)
	lt := cp.boundLive()
	require.NotNil(t, lt)
	// An immutable file makes removing the live directory fail, so Stop
	// cannot confirm the cleanup.
	pinned := filepath.Join(lt.proc.Dir(), "tmp", "immutable")
	require.NoError(t, os.WriteFile(pinned, []byte("x"), 0o600))
	require.NoError(t, exec.Command("chflags", "uchg", pinned).Run())
	t.Cleanup(func() {
		require.NoError(t, exec.Command("chflags", "nouchg", pinned).Run())
		require.NoError(t, os.RemoveAll(cardsDir))
	})

	_, err = cp.StopLiveTarget(alice)
	assert.Equal(t, "live_target_stop_failed", goaErrorName(err))
	assert.NotContains(t, err.Error(), cardsDir, "the public error names no private directory")
	_, err = cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("b", 40)})
	assert.Equal(t, "live_target_failed", goaErrorName(err))
	assert.Contains(t, err.Error(), "could not be confirmed stopped")
	dirs, err := verifyrun.LeftoverLiveDirs(LiveRoot(cardsDir))
	require.NoError(t, err)
	require.Len(t, dirs, 1, "no second target was launched")
	resolvedDir, err := filepath.EvalSymlinks(dirs[0])
	require.NoError(t, err)
	assert.Equal(t, lt.proc.Dir(), resolvedDir, "the original target's directory remains")
	_, err = draft(alice, cp, fakePromptID, "x")
	assert.Equal(t, "no_live_target", goaErrorName(err), "drafts are unbound while stopping")

	// A diff admits a design evaluation, which creates the evaluator's
	// workspace, and fails on the incomplete snapshot.
	_, err = cp.Diff(alice, &gensurla.DiffPayload{
		Old: &gensurla.DesignSnapshot{Files: map[string]string{"design/design.go": "package design\n"}},
		New: &gensurla.DesignSnapshot{Files: map[string]string{"design/design.go": "package design\n"}},
	})
	require.Error(t, err)
	require.Len(t, workspaces(t, tmp), 1)

	err = cp.Close()
	require.Error(t, err, "Close reports the target it could not confirm stopped")
	assert.Empty(t, workspaces(t, tmp), "Close still removes the evaluator's workspace")
	again := cp.Close()
	require.Error(t, again, "a later Close returns the same stop failure")
	assert.Equal(t, err.Error(), again.Error())
	_, err = New(Options{CardsDir: cardsDir})
	require.Error(t, err, "the cards directory, which holds the live root, stays locked")
	_, err = cp.Diff(alice, &gensurla.DiffPayload{
		Old: &gensurla.DesignSnapshot{Files: map[string]string{}}, New: &gensurla.DesignSnapshot{Files: map[string]string{}},
	})
	require.ErrorContains(t, err, "design evaluator is closed")
}

// workspaces lists the design evaluator workspaces in dir.
func workspaces(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "surla-designeval-work-*"))
	require.NoError(t, err)
	return matches
}

func TestTargetMessagesNeverReachCallers(t *testing.T) {
	cp, _ := newLiveControlPlane(t, "leaky")
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	_, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	require.NoError(t, err)
	args, err := json.Marshal(map[string]string{"target_kind": TargetKindPromptTemplate, "prompt_id": fakePromptID, "new_content": "x"})
	require.NoError(t, err)

	srv := httptest.NewServer(NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{"tok": {ID: "alice@example.com"}})))
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/sandbox/draft", strings.NewReader(string(args)))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok-builder")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)

	adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)
	call, err := adapter.ToolsCall(alice, &genmcpadapter.ToolsCallPayload{Name: "draft_override", Arguments: args})
	var mcpText string
	if err != nil {
		mcpText = err.Error()
	} else {
		b, merr := json.Marshal(call)
		require.NoError(t, merr)
		mcpText = string(b)
	}
	for _, secret := range []string{"/private/secret/path", "abc123", "10.0.0.1"} {
		assert.NotContains(t, string(body), secret, "HTTP body")
		assert.NotContains(t, mcpText, secret, "MCP result")
	}
	assert.Contains(t, string(body), "does not render")
}

// TestDraftWithNoLiveTargetNamesTheStepThatEnablesIt verifies that a
// prompt_template draft is refused with the declared no_live_target error,
// which names start_live_target, whether or not the server can start live
// targets, over the Go service, HTTP (409 with the goa-error name) and MCP.
// A compiled_rule draft needs no target and still answers.
func TestDraftWithNoLiveTargetNamesTheStepThatEnablesIt(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerBuilder})
	cases := []struct {
		name string
		cp   func(t *testing.T) *ControlPlane
	}{
		{name: "no live settings", cp: func(t *testing.T) *ControlPlane { return newControlPlane(t, Options{}) }},
		{name: "live settings but no target started", cp: func(t *testing.T) *ControlPlane {
			cp, _ := newLiveControlPlane(t, "serve")
			return cp
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := tc.cp(t)
			_, err := draft(alice, cp, fakePromptID, "x")
			assert.Equal(t, "no_live_target", goaErrorName(err))
			assert.ErrorContains(t, err, "start_live_target", "the error names the step that enables drafts")

			args, err := json.Marshal(map[string]string{"target_kind": TargetKindPromptTemplate, "prompt_id": fakePromptID, "new_content": "x"})
			require.NoError(t, err)
			srv := httptest.NewServer(NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{"tok": {ID: "alice@example.com"}})))
			defer srv.Close()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/sandbox/draft", strings.NewReader(string(args)))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer tok-builder")
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, http.StatusConflict, resp.StatusCode)
			assert.Equal(t, "no_live_target", resp.Header.Get("goa-error"))
			assert.Contains(t, string(body), "start_live_target")

			adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)
			call, err := adapter.ToolsCall(alice, &genmcpadapter.ToolsCallPayload{Name: "draft_override", Arguments: args})
			require.NoError(t, err, "a declared error is a tool result, not a transport or protocol error")
			complete, ok := call.Outcome.AsComplete()
			require.True(t, ok)
			require.NotNil(t, complete.IsError)
			assert.True(t, *complete.IsError, "the tool result is marked as an error")
			require.NotEmpty(t, complete.Content)
			require.NotNil(t, complete.Content[0].Text)
			assert.Contains(t, *complete.Content[0].Text, msgNoLiveTarget, "the MCP caller sees the declared message, which names the enabling step")

			rule, err := cp.DraftOverride(alice, &gensurla.DraftOverridePayload{TargetKind: TargetKindCompiledRule, NewContent: "refund limit 60"})
			require.NoError(t, err)
			assert.True(t, rule.RequiresRebuild, "a compiled rule needs no live target")
		})
	}
}

func TestLiveTargetVersionsMustNotBeBlank(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	cases := []struct {
		name    string
		version string
		valid   bool
	}{
		{name: "empty", version: ""},
		{name: "ordinary whitespace", version: " \t\n"},
		{name: "vertical tab", version: "\v"},
		{name: "no-break space", version: "\u00a0"},
		{name: "ideographic space", version: "\u3000"},
		{name: "opaque ASCII", version: "v1", valid: true},
		{name: "opaque Unicode", version: "版本-α\u00a01", valid: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(liveTargetVersionEnv, tc.version)
			cp, _ := newLiveControlPlane(t, "version")
			_, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
			require.NoError(t, err)
			out, err := draft(alice, cp, fakePromptID, "x")
			if !tc.valid {
				assert.Equal(t, "update_not_confirmed", goaErrorName(err), "the generated client rejects a blank version")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.version, *out.PromptVersion, "an opaque version is passed through unchanged")
		})
	}
}

func TestServersWithDifferentCardsDirectoriesNeverSeeEachOthersTargets(t *testing.T) {
	a, rootA := newLiveControlPlane(t, "serve")
	_, rootB := newLiveControlPlane(t, "serve")
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	_, err := a.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	require.NoError(t, err)
	dirsA, err := verifyrun.LeftoverLiveDirs(rootA)
	require.NoError(t, err)
	require.Len(t, dirsA, 1)
	dirsB, err := verifyrun.LeftoverLiveDirs(rootB)
	require.NoError(t, err)
	assert.Empty(t, dirsB, "B lists nothing of A's active target")
}

func TestToolsListWarnsThatStartingDiscardsTheRunningTargetsDrafts(t *testing.T) {
	cp, _ := newLiveControlPlane(t, "serve")
	adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)
	res, err := adapter.ToolsList(context.Background(), &genmcpadapter.ToolsListPayload{})
	require.NoError(t, err)
	descriptions := map[string]string{}
	for _, tool := range res.Tools {
		if tool.Description != nil {
			descriptions[tool.Name] = *tool.Description
		}
	}
	assert.Contains(t, descriptions["start_live_target"], "first stops the target already running, which discards every prompt draft applied to it, even if the new build then fails")
	assert.Contains(t, descriptions["stop_live_target"], "discards every prompt draft applied to it")
}

func TestAStartThatFailsBeforeReadyAndCannotBeCleanedUpKeepsItsOwner(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	cardsDir := t.TempDir()
	cp, err := New(Options{
		CardsDir:     cardsDir,
		Verification: &VerificationConfig{RunsDir: t.TempDir()},
		Live:         &LiveConfig{Model: noModel(), Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=fail-pinned")},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		matches, err := filepath.Glob(filepath.Join(LiveRoot(cardsDir), "live-*", "tmp", "immutable"))
		require.NoError(t, err)
		for _, m := range matches {
			require.NoError(t, exec.Command("chflags", "nouchg", m).Run())
		}
	})
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})

	_, err = cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	assert.Equal(t, "live_target_failed", goaErrorName(err))
	assert.Contains(t, err.Error(), "could not be confirmed stopped")
	dirs, err := verifyrun.LeftoverLiveDirs(LiveRoot(cardsDir))
	require.NoError(t, err)
	require.Len(t, dirs, 1)

	_, err = cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("b", 40)})
	assert.Equal(t, "live_target_failed", goaErrorName(err))
	again, err := verifyrun.LeftoverLiveDirs(LiveRoot(cardsDir))
	require.NoError(t, err)
	assert.Equal(t, dirs, again, "no second target was launched")
	_, err = cp.StopLiveTarget(alice)
	assert.Equal(t, "live_target_stop_failed", goaErrorName(err), "the owner is still known")
	require.Error(t, cp.Close())
	_, err = New(Options{CardsDir: cardsDir})
	require.Error(t, err, "the cards directory stays locked")
}

func TestAStartThatTimesOutSaysSo(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	cardsDir := t.TempDir()
	cp, err := New(Options{
		CardsDir:     cardsDir,
		Verification: &VerificationConfig{RunsDir: t.TempDir()},
		Live: &LiveConfig{Model: noModel(), Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=prepare-hangs"),
			StartTimeout: 300 * time.Millisecond},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	_, err = cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	assert.Equal(t, "live_target_failed", goaErrorName(err))
	assert.Contains(t, err.Error(), "did not become ready within the configured start timeout (300ms)")
	assert.NotContains(t, err.Error(), "commit exists")
	dirs, err := verifyrun.LeftoverLiveDirs(LiveRoot(cardsDir))
	require.NoError(t, err)
	assert.Empty(t, dirs)
}

func TestLiveTargetAnsweringForAnotherPromptIsNotConfirmed(t *testing.T) {
	cp, _ := newLiveControlPlane(t, "other-prompt")
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	_, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	require.NoError(t, err)
	out, err := draft(alice, cp, fakePromptID, "x")
	assert.Nil(t, out, "no applied-success result")
	assert.Equal(t, "update_not_confirmed", goaErrorName(err))
	for _, secret := range []string{"/private/secret/path", "abc123"} {
		assert.NotContains(t, err.Error(), secret, "the untrusted response ID is not echoed")
	}
}

// TestGetLiveTargetFollowsEachTransition verifies the live target read
// through a real fake target's life: none before a start; running with its
// commit, address and start time and no prompts; one entry per prompt whose
// draft was accepted, a second draft replacing it and refused drafts adding
// nothing; none once the target ends by itself; a later target with a later
// start time and no entries; and none after a stop. Over HTTP the read
// carries the version in force but never the drafted text, and no span the
// server recorded holds that text.
func TestGetLiveTargetFollowsEachTransition(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	self, err := os.Executable()
	require.NoError(t, err)
	cp := newControlPlane(t, Options{
		TracerProvider: provider,
		Verification:   &VerificationConfig{RunsDir: t.TempDir()},
		Live: &LiveConfig{
			Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=serve"),
			StartTimeout: time.Minute, Model: noModel(),
		},
	})
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	assert.Equal(t, gensurla.LiveTargetKindNone, cp.liveTargetState().Kind(), "no live target before a start")

	commit := strings.Repeat("a", 40)
	res, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: commit})
	require.NoError(t, err)
	running := readRunning(t, cp)
	assert.Equal(t, commit, running.CandidateCommit)
	assert.Equal(t, res.URL, running.URL)
	firstStart := parseLiveTime(t, running.StartedAt)
	assert.Empty(t, running.Prompts, "no draft was accepted yet")

	const first, second = "Answer in French.", "Answer in Spanish."
	_, err = draft(alice, cp, fakePromptID, first)
	require.NoError(t, err)
	running = readRunning(t, cp)
	require.Len(t, running.Prompts, 1)
	assert.Equal(t, fakePromptID, running.Prompts[0].PromptID)
	assert.Equal(t, textVersion(first), running.Prompts[0].Version)
	firstAccepted := parseLiveTime(t, running.Prompts[0].AcceptedAt)
	_, err = draft(alice, cp, fakePromptID, second)
	require.NoError(t, err)
	_, err = draft(alice, cp, "homes.helper.unknown", "Hello.")
	assert.Equal(t, "prompt_not_found", goaErrorName(err))
	_, err = draft(alice, cp, fakePromptID, "Hello {{ .Name")
	assert.Equal(t, "invalid_prompt", goaErrorName(err))
	running = readRunning(t, cp)
	require.Len(t, running.Prompts, 1, "a second draft of a prompt replaces its entry, and refused drafts add none")
	assert.Equal(t, textVersion(second), running.Prompts[0].Version)
	assert.False(t, parseLiveTime(t, running.Prompts[0].AcceptedAt).Before(firstAccepted))

	srv := httptest.NewServer(NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{"tok": {ID: "alice@example.com"}})))
	defer srv.Close()
	status, body := getWithToken(t, srv.URL+"/live-target", "tok-builder")
	require.Equal(t, http.StatusOK, status, body)
	var read struct {
		LiveTarget struct {
			State   string `json:"state"`
			Prompts []struct {
				Version string `json:"version"`
			} `json:"prompts"`
		} `json:"live_target"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &read))
	assert.Equal(t, "running", read.LiveTarget.State)
	require.Len(t, read.LiveTarget.Prompts, 1)
	assert.Equal(t, textVersion(second), read.LiveTarget.Prompts[0].Version)
	for _, text := range []string{first, second} {
		assert.NotContains(t, body, text, "the read never returns prompt text")
		for _, span := range exporter.GetSpans().Snapshots() {
			assertSpanNeverContainsValue(t, span, text)
		}
	}

	lt := cp.boundLive()
	require.NotNil(t, lt)
	if resp, err := http.Post(res.URL+"/exit", "text/plain", nil); err == nil {
		require.NoError(t, resp.Body.Close())
	}
	require.Eventually(t, func() bool { return cp.liveTargetState().Kind() == gensurla.LiveTargetKindNone },
		30*time.Second, 20*time.Millisecond, "a target that ended by itself is shown as none")
	<-lt.proc.Done()

	_, err = cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("b", 40)})
	require.NoError(t, err)
	running = readRunning(t, cp)
	assert.Empty(t, running.Prompts, "drafts end with the target that accepted them")
	assert.True(t, parseLiveTime(t, running.StartedAt).After(firstStart), "a later target has a later start time")
	stopped, err := cp.StopLiveTarget(alice)
	require.NoError(t, err)
	assert.True(t, stopped.Stopped)
	assert.Equal(t, gensurla.LiveTargetKindNone, cp.liveTargetState().Kind(), "a stopped target is shown as none")
}

// TestGetLiveTargetShowsAStartInProgressWithoutWaiting verifies that a
// start still building is read at once as starting, with its commit and when
// it began, although the start holds the live lock, and that it is shown as
// none once Close ends it.
func TestGetLiveTargetShowsAStartInProgressWithoutWaiting(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	cp, err := New(Options{
		CardsDir:     t.TempDir(),
		Verification: &VerificationConfig{RunsDir: t.TempDir()},
		Live: &LiveConfig{Model: noModel(), Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=prepare-hangs"),
			StartTimeout: time.Hour},
	})
	require.NoError(t, err)
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	commit := strings.Repeat("a", 40)
	started := make(chan error, 1)
	before := time.Now()
	go func() {
		_, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: commit})
		started <- err
	}()
	require.Eventually(t, func() bool { return cp.liveTargetState().Kind() == gensurla.LiveTargetKindStarting },
		30*time.Second, 20*time.Millisecond, "a start in progress is shown as starting")
	readAt := time.Now()
	res, err := cp.GetLiveTarget(alice)
	require.NoError(t, err)
	assert.Less(t, time.Since(readAt), time.Second, "the read does not wait for the start to finish")
	starting, ok := res.LiveTarget.AsStarting()
	require.True(t, ok)
	assert.Equal(t, commit, starting.CandidateCommit)
	assert.False(t, parseLiveTime(t, starting.Since).Before(before.Truncate(time.Second)))

	require.NoError(t, cp.Close())
	assert.Equal(t, "live_target_failed", goaErrorName(<-started))
	assert.Equal(t, gensurla.LiveTargetKindNone, cp.liveTargetState().Kind(), "the ended start is shown as none")
}

// TestGetLiveTargetShowsATargetNotConfirmedStopped verifies that a stop
// that cannot be confirmed shows the target as not confirmed stopped, with
// its commit and the step the server names, and that it stays so: a target's
// stop runs once, so a later stop and Close report the same failure and the
// read never claims none. Making the stop fail needs macOS's chflags, as
// TestAFailedStopKeepsTheOwnerAndStartsNoOtherTarget does.
func TestGetLiveTargetShowsATargetNotConfirmedStopped(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("making a stop fail needs macOS's chflags")
	}
	t.Setenv("TMPDIR", t.TempDir())
	self, err := os.Executable()
	require.NoError(t, err)
	cardsDir := t.TempDir()
	cp, err := New(Options{
		CardsDir:     cardsDir,
		Verification: &VerificationConfig{RunsDir: t.TempDir()},
		Live:         &LiveConfig{Model: noModel(), Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=serve")},
	})
	require.NoError(t, err)
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	commit := strings.Repeat("a", 40)
	_, err = cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: commit})
	require.NoError(t, err)
	lt := cp.boundLive()
	require.NotNil(t, lt)
	// An immutable file makes removing the live directory fail, so Stop
	// cannot confirm the cleanup.
	pinned := filepath.Join(lt.proc.Dir(), "tmp", "immutable")
	require.NoError(t, os.WriteFile(pinned, []byte("x"), 0o600))
	require.NoError(t, exec.Command("chflags", "uchg", pinned).Run())
	t.Cleanup(func() {
		require.NoError(t, exec.Command("chflags", "nouchg", pinned).Run())
		require.NoError(t, os.RemoveAll(cardsDir))
	})

	for _, attempt := range []string{"the first stop", "a later stop"} {
		_, err = cp.StopLiveTarget(alice)
		assert.Equal(t, "live_target_stop_failed", goaErrorName(err), attempt)
		res, err := cp.GetLiveTarget(alice)
		require.NoError(t, err)
		stuck, ok := res.LiveTarget.AsNotConfirmedStopped()
		require.True(t, ok, "after %s the target is shown as not confirmed stopped", attempt)
		assert.Equal(t, commit, stuck.CandidateCommit)
		assert.Equal(t, msgStopFailed, stuck.Message)
		assert.Contains(t, stuck.Message, "stop and start Surla", "the read names the step the server already names")
	}
	require.Error(t, cp.Close(), "Close reports the target it could not confirm stopped")
	assert.Equal(t, gensurla.LiveTargetKindNotConfirmedStopped, cp.liveTargetState().Kind(), "Close does not claim the target stopped")
}

// readRunning reads the live target and requires it to be running.
func readRunning(t *testing.T, cp *ControlPlane) *gensurla.RunningLiveTarget {
	t.Helper()
	res, err := cp.GetLiveTarget(WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "builder", Kind: CallerBuilder}))
	require.NoError(t, err)
	running, ok := res.LiveTarget.AsRunning()
	require.True(t, ok, "the live target is %s, want running", res.LiveTarget.Kind())
	return running
}

// parseLiveTime parses a time the live target read reports.
func parseLiveTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	require.NoError(t, err)
	return parsed
}

// textVersion is the version the fake target returns for text.
func textVersion(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// getWithToken sends GET url with a bearer token and returns the status and
// body.
func getWithToken(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode, string(body)
}

// TestChangeListVersionFollowsTheLiveTarget verifies that the version the
// views page's waiting read compares changes when a live target starts,
// accepts a draft of new text and ends by itself, stays the same when nothing
// about the target changes, and that a waiting read answers soon after a
// draft without the live target's part recording any span of its own.
func TestChangeListVersionFollowsTheLiveTarget(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	self, err := os.Executable()
	require.NoError(t, err)
	cp := newControlPlane(t, Options{
		TracerProvider: provider,
		Verification:   &VerificationConfig{RunsDir: t.TempDir()},
		Live: &LiveConfig{
			Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=serve"),
			StartTimeout: time.Minute, Model: noModel(),
		},
	})
	cp.changesWait = 20 * time.Second
	cp.changesPoll = 20 * time.Millisecond
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	version := func() string {
		list, err := cp.ListChanges(alice)
		require.NoError(t, err)
		return list.Version
	}
	idle := version()
	assert.Equal(t, idle, version(), "nothing changed, so the version stays")

	res, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	require.NoError(t, err)
	started := version()
	assert.NotEqual(t, idle, started, "a started target changes the version")
	_, err = draft(alice, cp, fakePromptID, "Answer in French.")
	require.NoError(t, err)
	drafted := version()
	assert.NotEqual(t, started, drafted, "an accepted draft changes the version")
	_, err = draft(alice, cp, fakePromptID, "Hello {{ .Name")
	assert.Equal(t, "invalid_prompt", goaErrorName(err))
	assert.Equal(t, drafted, version(), "a refused draft leaves the version")

	exporter.Reset()
	waited := make(chan *gensurla.ChangeListVersion, 1)
	go func() {
		out, err := cp.WaitForChanges(alice, &gensurla.WaitForChangesPayload{Since: drafted})
		assert.NoError(t, err)
		waited <- out
	}()
	time.Sleep(100 * time.Millisecond)
	_, err = draft(alice, cp, fakePromptID, "Answer in Spanish.")
	require.NoError(t, err)
	select {
	case out := <-waited:
		assert.NotEqual(t, drafted, out.Version, "the waiting read answers with the new version")
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting read did not answer a draft within 5 seconds")
	}
	for _, span := range exporter.GetSpans().Snapshots() {
		assert.Contains(t, []string{waitForChangesSpan, "controlplane.draft_live"}, span.Name(),
			"the live target's part of the version records no span")
	}

	redrafted := version()
	if resp, err := http.Post(res.URL+"/exit", "text/plain", nil); err == nil {
		require.NoError(t, resp.Body.Close())
	}
	require.Eventually(t, func() bool { return cp.liveTargetState().Kind() == gensurla.LiveTargetKindNone },
		30*time.Second, 20*time.Millisecond)
	assert.NotEqual(t, redrafted, version(), "a target that ended changes the version")
}
