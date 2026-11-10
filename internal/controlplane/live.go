package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	goahttp "goa.design/goa/v3/http"
	goa "goa.design/goa/v3/pkg"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	gensurla "github.com/goadesign/surla/gen/surla"
	genpromptsclient "github.com/goadesign/surla/internal/targetprompt/gen/http/prompts/client"
	genprompts "github.com/goadesign/surla/internal/targetprompt/gen/prompts"
	"github.com/goadesign/surla/internal/verifyrun"
)

// A control plane runs at most one live CP3 target: a candidate commit built
// the way CP4 builds it and started by the operator's evaluator suite in its
// --live mode (see verifyrun.LaunchLive). draft_override sends prompt text to
// it through the one operation Surla expects a target to serve
// (design/targetprompt), authenticated with a key minted for that target.
//
// Two fields track it. owned is the target whose processes this control
// plane must stop; it is cleared only once they are confirmed stopped, so a
// target that could not be stopped is never forgotten and no second target
// starts beside it. bound is the target draft_override sends to; it is
// cleared first whenever a target is stopped or ends. Live targets live in
// <CardsDir>/live, which the card store's exclusive lock reserves for this
// control plane. They run no longer than it: Close cancels the server
// context, which ends a start in progress and refuses every later start, and
// then stops the owned target.

type (
	// LiveConfig is what a control plane needs to run live CP3 targets. The
	// build and launch plan comes from Options.Verification, and targets
	// live in <CardsDir>/live.
	LiveConfig struct {
		// Command is the argument list that runs this surla executable;
		// targets start `<Command> verify-run live <dir>`.
		Command []string
		// Env is the environment live targets start with. It should carry
		// the model provider settings the evaluator suite needs.
		Env []string
		// StartTimeout bounds how long building and starting a target may
		// take.
		StartTimeout time.Duration
	}

	// liveTarget is a started live target and the client bound to it.
	liveTarget struct {
		// commit is the candidate commit it was built from.
		commit string
		// proc owns its processes and directory.
		proc *verifyrun.LiveProcess
		// client calls its prompt operation; key authenticates the calls.
		client *genprompts.Client
		key    string
	}

	// liveState is the control plane's live-target state, embedded in
	// ControlPlane.
	liveState struct {
		// liveConfigured records whether live launch settings were supplied.
		// A verification plan is also required before a target can start.
		liveConfigured bool
		// liveCommand is the argument list that runs this surla executable;
		// targets start `<liveCommand> verify-run live <dir>`.
		liveCommand []string
		// liveTimeout bounds how long building and starting a target may
		// take.
		liveTimeout time.Duration
		// liveRoot is <CardsDir>/live.
		liveRoot string
		// serverCtx lives as long as the control plane. Close cancels it with
		// cancelServer; a canceled serverCtx is the fact that no target may
		// start any more, and it also ends a start in progress.
		serverCtx    context.Context
		cancelServer context.CancelFunc
		// liveMu serializes start, stop, the handling of an ended target and
		// Close, so a target is confirmed stopped before another starts.
		// It guards owned.
		liveMu sync.Mutex
		// owned is the target whose processes are not yet confirmed stopped.
		owned *liveTarget
		// bindMu guards bound, so draft_override never waits for a start or
		// stop to finish.
		bindMu sync.RWMutex
		// bound is the target draft_override sends to, or nil.
		bound *liveTarget
	}
)

const (
	// startTimeoutDefault bounds a start when LiveConfig sets no
	// StartTimeout.
	startTimeoutDefault = 30 * time.Minute
	// liveDirName is the live root's name inside CardsDir.
	liveDirName = "live"
	// msgStopFailed is the public explanation of a target whose processes
	// could not be confirmed stopped.
	msgStopFailed = "The live target's processes could not be confirmed stopped, so no other live target can start; restart surla serve."
)

// StartLiveTarget builds the candidate commit with the operator's
// verification plan and runs it as this server's live target, stopping the
// running one first. The build belongs to the server: it continues if the
// caller disconnects and ends when the server closes or StartTimeout passes.
// Starts are serialized, and none starts once Close has begun.
func (c *ControlPlane) StartLiveTarget(ctx context.Context, p *gensurla.StartLiveTargetPayload) (*gensurla.LiveTargetResult, error) {
	if _, ok := PrincipalFromContext(ctx); !ok {
		return nil, errors.New("authenticated principal is required")
	}
	if !c.liveConfigured {
		return nil, gensurla.MakeLiveTargetUnavailable(errors.New("this server has no verification configuration, so it cannot build or run a live target"))
	}
	verCfg, err := c.resolveActiveVerification(ctx)
	if err != nil {
		return nil, gensurla.MakeLiveTargetUnavailable(err)
	}
	if verCfg == nil {
		return nil, gensurla.MakeLiveTargetUnavailable(errors.New("this server has no verification configuration, so it cannot build or run a live target"))
	}
	ctx, span := c.tracer("surla/controlplane").Start(ctx, "controlplane.start_live_target",
		trace.WithAttributes(attribute.String("candidate_commit", p.CandidateCommit)))
	defer span.End()

	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	if c.serverCtx.Err() != nil {
		return nil, gensurla.MakeLiveTargetUnavailable(errors.New("the server is shutting down, so it starts no live target"))
	}
	if err := c.stopOwnedLocked(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "stop previous live target failed")
		return nil, gensurla.MakeLiveTargetFailed(errors.New(msgStopFailed))
	}
	key, err := newLiveKey()
	if err != nil {
		return nil, err
	}
	plan := verCfg.Plan
	plan.CandidateCommit = p.CandidateCommit
	timeout := c.liveTimeout
	if timeout <= 0 {
		timeout = startTimeoutDefault
	}
	cmd := c.liveCommand
	if len(cmd) == 0 {
		cmd = verCfg.Command
	}
	proc, err := verifyrun.LaunchLive(c.liveRoot, plan, key, cmd, c.liveEnv())
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "live target did not launch")
		return nil, gensurla.MakeLiveTargetFailed(errors.New("The live target could not be launched; the server could not prepare its directory or start its process."))
	}
	// The control plane owns the target from launch, so a target whose
	// cleanup fails is never forgotten.
	lt := &liveTarget{commit: p.CandidateCommit, proc: proc, key: key}
	c.owned = lt
	startCtx, cancel := context.WithTimeout(c.serverCtx, timeout)
	defer cancel()
	if err := proc.AwaitReady(startCtx); err != nil {
		summary := err.Error()
		switch {
		case c.serverCtx.Err() != nil:
			summary = "The server is shutting down, so the live target was not started."
		case errors.Is(err, context.DeadlineExceeded):
			summary = fmt.Sprintf("The live target did not become ready within the configured start timeout (%s). "+
				"Check the evaluator suite's startup, or raise the verification configuration's timeout.", timeout)
		}
		if start := (*verifyrun.LiveStartError)(nil); errors.As(err, &start) {
			span.AddEvent("live target start failed", trace.WithAttributes(attribute.String("executor_log_tail", start.Detail)))
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "live target did not start")
		if stopErr := c.stopOwnedLocked(); stopErr != nil {
			span.RecordError(stopErr)
			return nil, gensurla.MakeLiveTargetFailed(errors.New(summary + " " + msgStopFailed))
		}
		return nil, gensurla.MakeLiveTargetFailed(errors.New(summary))
	}
	client, err := newPromptsClient(proc.URL)
	if err != nil {
		return nil, errors.Join(err, c.stopOwnedLocked())
	}
	lt.client = client
	c.bindMu.Lock()
	c.bound = lt
	c.bindMu.Unlock()
	go c.watchLive(lt)
	span.AddEvent("live target ready")
	return &gensurla.LiveTargetResult{
		CandidateCommit: p.CandidateCommit,
		URL:             proc.URL,
		Message: fmt.Sprintf("Live target for commit %s is running at %s, a loopback address on the Surla server's host; draft_override now sends prompt text to it.",
			p.CandidateCommit, proc.URL),
	}, nil
}

// StopLiveTarget stops the running live target, if any.
func (c *ControlPlane) StopLiveTarget(ctx context.Context) (*gensurla.StopLiveTargetResult, error) {
	if _, ok := PrincipalFromContext(ctx); !ok {
		return nil, errors.New("authenticated principal is required")
	}
	_, span := c.tracer("surla/controlplane").Start(ctx, "controlplane.stop_live_target")
	defer span.End()
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	running := c.owned != nil
	if err := c.stopOwnedLocked(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "stop live target failed")
		return nil, gensurla.MakeLiveTargetStopFailed(errors.New(msgStopFailed))
	}
	if !running {
		return &gensurla.StopLiveTargetResult{Stopped: false, Message: "No live target was running."}, nil
	}
	return &gensurla.StopLiveTargetResult{Stopped: true, Message: "The live target is stopped; its prompt drafts are discarded and draft_override is no longer bound to it."}, nil
}

// draftLive sends a prompt template to the live target and maps its outcome
// to draft_override's errors. Only a rejection by the target guarantees that
// its text is unchanged; any other failure is reported as not confirmed, or
// as ended when the target stopped meanwhile. Target messages are untrusted,
// so they go only to the span.
func (c *ControlPlane) draftLive(ctx context.Context, lt *liveTarget, promptID, template string) (*gensurla.DraftOverrideResult, error) {
	ctx, span := c.tracer("surla/controlplane").Start(ctx, "controlplane.draft_live",
		trace.WithAttributes(attribute.String("prompt_id", promptID), attribute.String("candidate_commit", lt.commit)))
	defer span.End()
	res, err := lt.client.OverridePrompt(ctx, &genprompts.OverridePromptPayload{Key: lt.key, PromptID: promptID, Template: template})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "live target prompt update failed")
		return nil, liveDraftError(err, lt)
	}
	if res.PromptID != promptID {
		err := fmt.Errorf("live target answered for prompt %q instead of %q", res.PromptID, promptID)
		span.RecordError(err)
		span.SetStatus(codes.Error, "live target answered for another prompt")
		return nil, gensurla.MakeUpdateNotConfirmed(errors.New("the live target answered for a different prompt, so the update is not confirmed; keep the draft and send it again"))
	}
	span.AddEvent("prompt text accepted", trace.WithAttributes(attribute.String("version", res.Version)))
	return &gensurla.DraftOverrideResult{
		AppliedInStore:  true,
		RequiresRebuild: false,
		PromptVersion:   &res.Version,
		Message: fmt.Sprintf("Updated prompt %q on the live target for commit %s (version %s). It is not verified: commit the text and run verify_change.",
			promptID, lt.commit, res.Version),
	}, nil
}

// closeLive is Close's part for live targets. Canceling the server context
// first ends a start in progress and makes every queued or later start
// refuse; the owned target is then stopped. An error means the target's
// processes could not be confirmed stopped.
func (c *ControlPlane) closeLive() error {
	c.cancelServer()
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	return c.stopOwnedLocked()
}

// watchLive handles a live target that ended on its own: it unbinds it and
// stops it, so later drafts get the unbound error and stop reports that
// nothing runs. A target that cannot be confirmed stopped stays owned.
func (c *ControlPlane) watchLive(lt *liveTarget) {
	<-lt.proc.Done()
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	if c.owned != lt {
		return
	}
	if err := c.stopOwnedLocked(); err != nil {
		_, span := c.tracer("surla/controlplane").Start(context.Background(), "controlplane.live_target_ended")
		span.RecordError(err)
		span.SetStatus(codes.Error, "stop ended live target failed")
		span.End()
	}
}

// stopOwnedLocked unbinds the owned target and stops it, clearing it only
// once its processes are confirmed stopped. c.liveMu must be held.
func (c *ControlPlane) stopOwnedLocked() error {
	lt := c.owned
	if lt == nil {
		return nil
	}
	c.bindMu.Lock()
	if c.bound == lt {
		c.bound = nil
	}
	c.bindMu.Unlock()
	if err := lt.proc.Stop(); err != nil {
		return err
	}
	c.owned = nil
	return nil
}

// boundLive returns the target draft_override sends to, or nil.
func (c *ControlPlane) boundLive() *liveTarget {
	c.bindMu.RLock()
	defer c.bindMu.RUnlock()
	return c.bound
}

// LiveRoot returns the directory under which a control plane whose cards are
// in cardsDir keeps its live targets.
func LiveRoot(cardsDir string) string {
	return filepath.Join(cardsDir, liveDirName)
}

// newLiveState returns the live-target state for a control plane whose cards
// are in cardsDir, retaining only the launch command and start timeout from
// cfg while ControlPlane.activeModel owns the live target environment.
func newLiveState(cfg *LiveConfig, cardsDir string) liveState {
	ctx, cancel := context.WithCancel(context.Background())
	if cfg == nil {
		return liveState{liveRoot: LiveRoot(cardsDir), serverCtx: ctx, cancelServer: cancel}
	}
	return liveState{
		liveConfigured: true,
		liveCommand:    cfg.Command,
		liveTimeout:    cfg.StartTimeout,
		liveRoot:       LiveRoot(cardsDir),
		serverCtx:      ctx,
		cancelServer:   cancel,
	}
}

// liveDraftError maps an override error to draft_override's errors with
// fixed explanations, because the target's own messages are untrusted.
func liveDraftError(err error, lt *liveTarget) error {
	var unauthorized genprompts.Unauthorized
	var serviceErr *goa.ServiceError
	switch {
	case errors.As(err, &unauthorized):
		return gensurla.MakeTargetRejectedCredential(errors.New("the live target rejected this server's key; nothing changed. Start it again with start_live_target"))
	case errors.As(err, &serviceErr) && serviceErr.Name == "unknown_prompt":
		return gensurla.MakePromptNotFound(errors.New("the live target has no prompt with this ID; nothing changed"))
	case errors.As(err, &serviceErr) && serviceErr.Name == "invalid_prompt":
		return gensurla.MakeInvalidPrompt(errors.New("the live target rejected the template because it does not render with the prompt's registered spec; nothing changed"))
	}
	select {
	case <-lt.proc.Done():
		return gensurla.MakeLiveTargetEnded(errors.New("the live target stopped during the update, so it may or may not have applied it; start it again with start_live_target"))
	default:
	}
	return gensurla.MakeUpdateNotConfirmed(errors.New("the live target did not confirm the update, so it may or may not have applied it; keep the draft and send it again"))
}

// newPromptsClient returns the generated prompt-operation client for a live
// target's loopback origin. Redirects are not followed, so the key reaches
// only that origin.
func newPromptsClient(origin string) (*genprompts.Client, error) {
	u, err := url.Parse(origin)
	if err != nil {
		return nil, fmt.Errorf("live target URL: %w", err)
	}
	doer := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	transport := genpromptsclient.NewClient(u.Scheme, u.Host, doer, goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	return genprompts.NewClient(transport.OverridePrompt()), nil
}

// newLiveKey returns a new random key for one live target.
func newLiveKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("create live target key: %w", err)
	}
	return hex.EncodeToString(b), nil
}
