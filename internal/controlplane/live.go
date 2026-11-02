package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	goahttp "goa.design/goa/v3/http"
	goa "goa.design/goa/v3/pkg"

	"go.opentelemetry.io/otel"
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
// --live mode (see verifyrun.StartLive). draft_override sends prompt text to
// it through the one operation Surla expects a target to serve
// (design/targetprompt), authenticated with a key minted for that target.
// start_live_target replaces the running target only after it has fully
// stopped, so draft_override never reaches two targets.

type (
	// LiveConfig is what a control plane needs to run live CP3 targets. The
	// build and launch plan comes from Options.Verification.
	LiveConfig struct {
		// Root is the directory under which each live target gets its own
		// directory. Only this server may use it.
		Root string
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

	// liveTarget is the running live target and the client bound to it.
	liveTarget struct {
		// commit is the candidate commit it was built from.
		commit string
		// proc owns its processes and directory.
		proc *verifyrun.LiveProcess
		// client calls its prompt operation; key authenticates the calls.
		client *genprompts.Client
		key    string
		// ended is closed once the target ended on its own.
		ended chan struct{}
	}

	// liveLocks are the control plane's live-target fields, embedded in
	// ControlPlane.
	liveLocks struct {
		// liveMu serializes start, stop and the handling of an ended target, so
		// a target is fully stopped before another starts.
		liveMu sync.Mutex
		// bindMu guards liveTarget for draft_override, which never waits for a
		// start or stop to finish.
		bindMu sync.RWMutex
		// liveTarget is the bound live target, or nil.
		liveTarget *liveTarget
		// live is the operator's live configuration, or nil.
		live *LiveConfig
	}
)

// startTimeoutDefault bounds a start when LiveConfig sets no StartTimeout.
const startTimeoutDefault = 30 * time.Minute

// StartLiveTarget builds the candidate commit with the operator's
// verification plan and runs it as this server's live target, stopping any
// running one first. The build continues if the caller disconnects; it is
// bounded by LiveConfig.StartTimeout. Starts are serialized.
func (c *ControlPlane) StartLiveTarget(ctx context.Context, p *gensurla.StartLiveTargetPayload) (*gensurla.LiveTargetResult, error) {
	if _, ok := PrincipalFromContext(ctx); !ok {
		return nil, errors.New("authenticated principal is required")
	}
	if c.verification == nil || c.live == nil {
		return nil, gensurla.MakeLiveTargetUnavailable(errors.New("this server has no verification configuration, so it cannot build or run a live target"))
	}
	ctx, span := otel.Tracer("surla/controlplane").Start(ctx, "controlplane.start_live_target",
		trace.WithAttributes(attribute.String("candidate_commit", p.CandidateCommit)))
	defer span.End()

	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	if err := c.stopLiveLocked(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "stop previous live target failed")
		return nil, gensurla.MakeLiveTargetFailed(errors.New("the previous live target could not be stopped; see the server log"))
	}
	key, err := newLiveKey()
	if err != nil {
		return nil, err
	}
	plan := c.verification.Plan
	plan.CandidateCommit = p.CandidateCommit
	timeout := c.live.StartTimeout
	if timeout <= 0 {
		timeout = startTimeoutDefault
	}
	startCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	proc, err := verifyrun.StartLive(startCtx, c.live.Root, plan, key, c.live.Command, c.live.Env)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "live target did not start")
		summary := "The live target could not be started; see the server log."
		if start := (*verifyrun.LiveStartError)(nil); errors.As(err, &start) {
			summary = start.Summary
		}
		return nil, gensurla.MakeLiveTargetFailed(errors.New(summary))
	}
	client, err := newPromptsClient(proc.URL)
	if err != nil {
		return nil, errors.Join(err, proc.Stop())
	}
	lt := &liveTarget{commit: p.CandidateCommit, proc: proc, client: client, key: key, ended: make(chan struct{})}
	c.bindMu.Lock()
	c.liveTarget = lt
	c.bindMu.Unlock()
	go c.watchLive(lt)
	span.AddEvent("live target ready")
	return &gensurla.LiveTargetResult{
		CandidateCommit: p.CandidateCommit,
		URL:             proc.URL,
		Message:         fmt.Sprintf("Live target for commit %s is running at %s; draft_override now sends prompt text to it.", p.CandidateCommit, proc.URL),
	}, nil
}

// StopLiveTarget stops the running live target, if any.
func (c *ControlPlane) StopLiveTarget(ctx context.Context) (*gensurla.StopLiveTargetResult, error) {
	if _, ok := PrincipalFromContext(ctx); !ok {
		return nil, errors.New("authenticated principal is required")
	}
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	c.bindMu.RLock()
	running := c.liveTarget != nil
	c.bindMu.RUnlock()
	if err := c.stopLiveLocked(); err != nil {
		return nil, err
	}
	if !running {
		return &gensurla.StopLiveTargetResult{Stopped: false, Message: "No live target was running."}, nil
	}
	return &gensurla.StopLiveTargetResult{Stopped: true, Message: "The live target is stopped; draft_override is no longer bound to it."}, nil
}

// draftLive sends a prompt template to the live target and maps its outcome
// to draft_override's errors. Only a rejection by the target guarantees that
// its text is unchanged; any other failure is reported as not confirmed.
func (c *ControlPlane) draftLive(ctx context.Context, lt *liveTarget, promptID, template string) (*gensurla.DraftOverrideResult, error) {
	ctx, span := otel.Tracer("surla/controlplane").Start(ctx, "controlplane.draft_live",
		trace.WithAttributes(attribute.String("prompt_id", promptID)))
	defer span.End()
	select {
	case <-lt.ended:
		return nil, gensurla.MakeLiveTargetEnded(errors.New("the live target is no longer running; start it again with start_live_target"))
	default:
	}
	res, err := lt.client.OverridePrompt(ctx, &genprompts.OverridePromptPayload{Key: lt.key, PromptID: promptID, Template: template})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "live target prompt update failed")
		return nil, liveDraftError(err, lt)
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

// watchLive stops a live target that ended on its own and keeps it bound,
// marked ended, so draft_override reports live_target_ended until the next
// start or stop.
func (c *ControlPlane) watchLive(lt *liveTarget) {
	<-lt.proc.Done()
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	c.bindMu.RLock()
	current := c.liveTarget == lt
	c.bindMu.RUnlock()
	if !current {
		return
	}
	close(lt.ended)
	if err := lt.proc.Stop(); err != nil {
		_, span := otel.Tracer("surla/controlplane").Start(context.Background(), "controlplane.live_target_ended")
		span.RecordError(err)
		span.SetStatus(codes.Error, "stop ended live target failed")
		span.End()
	}
}

// stopLiveLocked unbinds and stops the running live target. c.liveMu must be
// held.
func (c *ControlPlane) stopLiveLocked() error {
	c.bindMu.Lock()
	lt := c.liveTarget
	c.liveTarget = nil
	c.bindMu.Unlock()
	if lt == nil {
		return nil
	}
	return lt.proc.Stop()
}

// currentLive returns the bound live target, or nil.
func (c *ControlPlane) currentLive() *liveTarget {
	c.bindMu.RLock()
	defer c.bindMu.RUnlock()
	return c.liveTarget
}

// liveDraftError maps an override error to draft_override's errors. A
// target that ended during the call is reported as ended.
func liveDraftError(err error, lt *liveTarget) error {
	var unauthorized genprompts.Unauthorized
	var serviceErr *goa.ServiceError
	switch {
	case errors.As(err, &unauthorized):
		return gensurla.MakeTargetRejectedCredential(errors.New("the live target rejected this server's key; nothing changed. Start it again with start_live_target"))
	case errors.As(err, &serviceErr) && serviceErr.Name == "unknown_prompt":
		return gensurla.MakePromptNotFound(errors.New("the live target has no prompt with this ID; nothing changed"))
	case errors.As(err, &serviceErr) && serviceErr.Name == "invalid_prompt":
		return gensurla.MakeInvalidPrompt(fmt.Errorf("the live target rejected the template; nothing changed: %s", serviceErr.Message))
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
