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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.EqualError(t, err, "no target prompt updater is bound to this control plane", "stopping restores today's unbound error")
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
	require.EqualError(t, err, "no target prompt updater is bound to this control plane", "later drafts get the unbound error")
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
		allowed := err.Error() == "no target prompt updater is bound to this control plane" ||
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
		Live:         &LiveConfig{Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=serve")},
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
		Live: &LiveConfig{Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=prepare-hangs"),
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
		Live:         &LiveConfig{Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=serve")},
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
	require.EqualError(t, err, "no target prompt updater is bound to this control plane", "drafts are unbound while stopping")

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

func TestStaticUpdaterAndLiveCannotBeCombined(t *testing.T) {
	_, err := New(Options{CardsDir: t.TempDir(), TargetUpdater: staticUpdater{}, Live: &LiveConfig{}})
	require.EqualError(t, err, "set either TargetUpdater or Live, not both: draft_override sends prompt text to exactly one target")

	cp, err := New(Options{CardsDir: t.TempDir(), TargetUpdater: staticUpdater{}, Verification: &VerificationConfig{RunsDir: t.TempDir()}})
	require.NoError(t, err, "a static updater with verification and no Live keeps working")
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })
	out, err := draft(WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser}), cp, "orders.refund.system", "x")
	require.NoError(t, err)
	assert.Equal(t, "static-v1", *out.PromptVersion)
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

// staticUpdater is a TargetPromptUpdater that accepts every prompt.
type staticUpdater struct{}

// ApplyPromptOverride returns a fixed version.
func (staticUpdater) ApplyPromptOverride(context.Context, string, string) (string, error) {
	return "static-v1", nil
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
		Live:         &LiveConfig{Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=fail-pinned")},
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
		Live: &LiveConfig{Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=prepare-hangs"),
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
