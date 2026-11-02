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
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goahttp "goa.design/goa/v3/http"
	goa "goa.design/goa/v3/pkg"
	"goa.design/goa/v3/security"

	gensurla "github.com/goadesign/surla/gen/surla"
	genpromptsserver "github.com/goadesign/surla/internal/targetprompt/gen/http/prompts/server"
	genprompts "github.com/goadesign/surla/internal/targetprompt/gen/prompts"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
	"github.com/goadesign/surla/verify/suite"
)

const (
	// liveTargetModeEnv selects what the fake live target does: "serve"
	// serves the prompt operation, "drop" closes every update's connection
	// without answering, and "fail" makes the suite exit before it is ready.
	liveTargetModeEnv = "CONTROLPLANE_TEST_LIVE_TARGET"
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
// processes.
func TestMain(m *testing.M) {
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
func fakeLivePrepare(context.Context, verify.Plan) (verify.Prepared, error) {
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
	if mode == "fail" {
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
	root := t.TempDir()
	cp, err := New(Options{
		CardsDir:     t.TempDir(),
		Verification: &VerificationConfig{RunsDir: t.TempDir()},
		Live: &LiveConfig{
			Root: root, Command: []string{self},
			Env: append(os.Environ(), liveTargetModeEnv+"="+mode), StartTimeout: time.Minute,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })
	return cp, root
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
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
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
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	_, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	require.NoError(t, err)
	lt := cp.currentLive()
	require.NotNil(t, lt)
	lt.key = "not-the-key"
	_, err = draft(alice, cp, fakePromptID, "x")
	assert.Equal(t, "target_rejected_credential", goaErrorName(err))
}

func TestLiveTargetReportsAnUnconfirmedUpdate(t *testing.T) {
	cp, _ := newLiveControlPlane(t, "drop")
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	_, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	require.NoError(t, err)
	_, err = draft(alice, cp, fakePromptID, "x")
	assert.Equal(t, "update_not_confirmed", goaErrorName(err))
}

func TestLiveTargetThatEndsIsReportedEndedUntilRestarted(t *testing.T) {
	cp, _ := newLiveControlPlane(t, "serve")
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	res, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	require.NoError(t, err)
	lt := cp.currentLive()
	require.NotNil(t, lt)
	resp, err := http.Post(res.URL+"/exit", "text/plain", nil)
	if err == nil {
		resp.Body.Close()
	}
	require.Eventually(t, func() bool {
		select {
		case <-lt.ended:
			return true
		default:
			return false
		}
	}, 30*time.Second, 20*time.Millisecond)
	_, err = draft(alice, cp, fakePromptID, "x")
	assert.Equal(t, "live_target_ended", goaErrorName(err))

	_, err = cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("b", 40)})
	require.NoError(t, err)
	_, err = draft(alice, cp, fakePromptID, "y")
	require.NoError(t, err, "a new start binds a running target again")
}

func TestLiveTargetStartFailuresAreTyped(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	cp, root := newLiveControlPlane(t, "fail")
	_, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	assert.Equal(t, "live_target_failed", goaErrorName(err))
	assert.Contains(t, err.Error(), "The evaluator suite did not start a live target.")
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
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	first, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	require.NoError(t, err)
	oldProc := cp.currentLive().proc

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
	root := t.TempDir()
	cp, err := New(Options{
		CardsDir:     t.TempDir(),
		Verification: &VerificationConfig{RunsDir: t.TempDir()},
		Live:         &LiveConfig{Root: root, Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=serve")},
	})
	require.NoError(t, err)
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	_, err = cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: strings.Repeat("a", 40)})
	require.NoError(t, err)
	proc := cp.currentLive().proc
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
