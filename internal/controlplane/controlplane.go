// Package controlplane implements the Surla MCP software factory control
// plane: authenticated intent & ambiguity resolution (CP1), evaluated C4
// architecture & cost/safety guardrail preview (CP2), target-bound Fast Draft
// Mode prompt overrides (CP3), and recorded promotion approval of a passed
// verification report (CP5).
package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	gensurlahttp "github.com/goadesign/surla/gen/http/surla/server"
	genmcpsurla "github.com/goadesign/surla/gen/jsonrpc/mcp_surla/server"
	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/diff"
	"github.com/goadesign/surla/internal/goldenpath"
	"github.com/goadesign/surla/internal/scaffold"
	"github.com/goadesign/surla/internal/traceview"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/verify/candidate"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"goa.design/goa-ai/runtime/mcp"
	goahttp "goa.design/goa/v3/http"
)

type (
	// Principal identifies the authenticated operator invoking the Surla control plane.
	Principal struct {
		// ID is the stable authenticated operator identifier (for example, "alice@example.com").
		ID string
		// CallerID identifies the browser session or agent responsible for an action.
		CallerID string
		// Kind is established by the credential owner, never by request payloads.
		Kind CallerKind
	}

	// RequestAuthenticator verifies an incoming HTTP request and returns the
	// authenticated Principal.
	RequestAuthenticator func(r *http.Request) (Principal, error)

	// principalContextKey is the context key storing the authenticated Principal.
	principalContextKey struct{}

	// PromotionTarget is the promotion owner's view of a candidate being
	// promoted at Checkpoint 5. The project server binds it from the stored
	// promotion settings; approve_merge never takes these facts from its
	// caller, because a caller-supplied report or binary digest would let the
	// caller approve its own claims.
	PromotionTarget interface {
		// LoadReport returns the verification report that verify.Run wrote
		// under digest, from storage only the evaluator writes. It fails when
		// no such report exists.
		LoadReport(ctx context.Context, digest string) (verify.Report, error)
		// CurrentInputs returns the evaluator suite and environment the
		// promotion requires and the digests of the candidate binaries
		// measured independently for candidateCommit, as one coherent
		// snapshot the implementation does not change after returning it.
		// It fails when those facts do not fix the target's behavior (see
		// the verify package). approve_merge checks a report against it,
		// and get_change and confirm_intent return a saved approval only
		// while it still matches.
		CurrentInputs(ctx context.Context, candidateCommit string) (PromotionInputs, error)
	}

	// PromotionInputs are the facts a verification report must match at
	// Checkpoint 5, besides the candidate commit and request revision.
	PromotionInputs struct {
		// SuiteID names the evaluator suite the promotion requires.
		SuiteID string
		// SuiteCommit is the evaluator suite commit the promotion requires.
		SuiteCommit string
		// Environment is the dependency profile the promotion requires, such
		// as "sim".
		Environment string
		// Artifacts maps each candidate binary name to its measured SHA-256
		// digest.
		Artifacts map[string]string
	}

	// Options configures a ControlPlane instance.
	Options struct {
		// CardsDir is the directory where the control plane saves its CP1
		// Change Cards, one file per card. It is required, and one process at
		// a time may use it.
		CardsDir string
		// TracerProvider optionally supplies the host-owned SDK provider used
		// to record authenticated operation diagnostics from construction.
		// When nil, operations keep global telemetry but ListTraces returns
		// not_implemented. The host shuts down this provider after Close, or
		// if New fails; the control plane never shuts it down.
		TracerProvider *sdktrace.TracerProvider
		// WorkspaceDir is the initial filesystem workspace bound to this
		// control plane. When set, cards, runs, and reports must live outside
		// it, and CP1/CP2/CP3/CP4/CP5 can read or derive project inputs from
		// the bound workspace.
		WorkspaceDir string
		// InitStageDir is an optional operator-owned staging parent directory
		// outside WorkspaceDir on the same filesystem device used by
		// init_project before atomic publication.
		InitStageDir string
		// Checker runs check_project and update_dependencies containers on
		// the local Docker engine. When nil, both tools return
		// not_implemented.
		Checker *candidate.ContainerRunner
		// CheckHelper starts the check helper process that runs each
		// check_project and update_dependencies operation, so its containers
		// are removed even if this server dies. It is required when Checker
		// is set; the composition root binds it (see CheckHelper).
		CheckHelper CheckHelper
		// CheckTimeout bounds one whole check or dependency operation. It
		// must be positive when Checker is set; New rejects zero or a
		// negative value. Per-operation directories live under CardsDir; each
		// is owned by the check helper that holds its lock, and this server's
		// recovery acts on one only once that lock is free.
		CheckTimeout time.Duration
		// GoProxyEnv holds the operator's GOPROXY, GOSUMDB, GOPRIVATE,
		// GONOPROXY, GONOSUMDB and GOINSECURE settings as KEY=value, passed
		// to dependency-download containers and nothing else.
		GoProxyEnv []string
		// Evaluator evaluates Goa design snapshots for preview_architecture and
		// diff. When nil, New constructs a default designeval.Evaluator (using
		// <CardsDir>/designeval as its protected temporary parent when
		// WorkspaceDir is set). When a host supplies Evaluator for a bound
		// workspace, it must construct it with designeval.NewEvaluatorInDir
		// using a protected directory outside WorkspaceDir.
		Evaluator *designeval.Evaluator
		// NewGenerator constructs a fresh scaffold.Generator for each
		// init_project call. When nil, InitProject constructs a fresh
		// designeval.NewEvaluatorInDir("<CardsDir>/designeval") per call. When
		// a host supplies NewGenerator, each returned generator must use a
		// protected directory outside WorkspaceDir.
		NewGenerator func() scaffold.Generator
		// InitRunner runs external commands (`git`, `go`) during init_project.
		// When nil, scaffold.RunCommand is used.
		InitRunner scaffold.Runner
		// SurlaVersion is the exact canonical Surla module version pinned in
		// `eval/go.mod` by init_project. When empty, init_project returns
		// not_implemented.
		SurlaVersion string
		// LiveModelEnv returns the CP3 live-target environment for a model
		// choice made with configure_model. Without it, configure_model returns
		// not_implemented.
		LiveModelEnv LiveModelEnv
		// PromotionTarget supplies CP5 verification reports and current
		// promotion inputs. Without it or ResolvePromotionTarget, approve_merge
		// fails.
		PromotionTarget PromotionTarget
		// ResolvePromotionTarget dynamically resolves a PromotionTarget from
		// the currently bound workspace directory when PromotionTarget is nil.
		ResolvePromotionTarget func(ctx context.Context, workspaceDir string) (PromotionTarget, error)
		// Verification is the operator's static CP4 configuration. When nil,
		// RunsDir and ResolveVerification supply dynamic workspace-derived CP4
		// configuration.
		Verification *VerificationConfig
		// RunsDir is the protected verification runs directory used by
		// get_verification, list_verifications, and init_project when
		// Verification is nil.
		RunsDir string
		// ResolveVerification dynamically resolves a VerificationConfig from
		// the currently bound workspace directory and its trusted starter suite
		// commit (set by init_project or restored from workspace.json, or empty
		// when the workspace was not initialized via init_project) for
		// verify_change and start_live_target.
		ResolveVerification func(ctx context.Context, workspaceDir, suiteCommit string) (*VerificationConfig, error)
		// Live lets start_live_target build and run CP3 live targets with
		// Verification's plan, in <CardsDir>/live. Without it, or without
		// Verification/ResolveVerification, start_live_target fails.
		Live *LiveConfig
		// BuildTeam readies the builder and the test writer when the user
		// starts a build. Without it, start_build saves the build and the
		// test writer's assignment names no working folder.
		BuildTeam BuildTeam
		// Team is the project owner composed before this control plane; nil leaves team operations unavailable.
		Team *TeamBinding
	}

	// storedChangeCard holds a versioned Change Card alongside its authenticated
	// owner principal, server-observed design source snapshot, optional
	// adopted domain Stories, and the revision proposed beside it, if any.
	storedChangeCard struct {
		// card is the revision in force.
		card *gensurla.ChangeCardResult
		// owner is the authenticated Principal.ID that created the card.
		owner string
		// designSource is the Goa design source snapshot inspected at CP1.
		designSource string
		// stories holds the evaluated domain Stories adopted for this revision,
		// or nil when none were adopted.
		stories []designeval.StorySummary
		// savedAt is when the card store last wrote this card; zero for a
		// record not yet saved. It is not part of request_revision.
		savedAt time.Time
		// proposed is the revision the guide proposed beside a confirmed card,
		// waiting for the user to adopt or decline it; nil otherwise. card
		// stays the revision in force until then.
		proposed *storedRevision
	}

	// storedRevision is a proposed revision saved beside a confirmed card, with
	// the design source and Stories it was made from, so its request_revision
	// is computed and checked on its own.
	storedRevision struct {
		// card is the proposed revision, always in needs_confirmation status.
		card *gensurla.ChangeCardResult
		// designSource is the Goa design source the proposal was made from.
		designSource string
		// stories holds the proposal's evaluated domain Stories, or nil.
		stories []designeval.StorySummary
	}

	// canonicalAnswerPair is a deterministic key/value entry for JSON hashing.
	canonicalAnswerPair struct {
		// ID is the ambiguity question ID.
		ID string `json:"id"`
		// Answer is the selected option value.
		Answer string `json:"answer"`
	}

	// canonicalCardRevision is the canonical JSON structure hashed to produce
	// RequestRevision without delimiter ambiguity.
	canonicalCardRevision struct {
		// ChangeID is the Change Card identifier.
		ChangeID string `json:"change_id"`
		// Owner is the authenticated creator principal ID.
		Owner string `json:"owner"`
		// Status is the Change Card status.
		Status string `json:"status"`
		// Prompt is the original operator feature request.
		Prompt string `json:"prompt"`
		// DesignSource is the Goa design source inspected at CP1.
		DesignSource string `json:"design_source"`
		// Stories is the ordered slice of evaluated domain Stories adopted for
		// this revision.
		Stories []designeval.StorySummary `json:"stories"`
		// Summary is the plain-English summary.
		Summary string `json:"summary"`
		// Ambiguities is the ordered slice of surfaced domain questions.
		Ambiguities []*gensurla.AmbiguityQuestion `json:"ambiguities"`
		// Answers is the key-sorted slice of confirmed ambiguity answers.
		Answers []canonicalAnswerPair `json:"answers"`
		// Examples is the ordered slice of confirmed example checks.
		Examples []*gensurla.ExampleItem `json:"examples"`
		// PreservedBehaviors is the ordered slice of preserved contract invariants.
		PreservedBehaviors []string `json:"preserved_behaviors"`
		// ConfirmedBy is the authenticated principal ID that confirmed the card.
		ConfirmedBy string `json:"confirmed_by"`
	}

	// ControlPlane implements gensurla.Service.
	ControlPlane struct {
		// traces owns the bounded diagnostic records under the card-store lock.
		traces *traceview.Store
		// traceProvider is fixed at construction; only this control plane's
		// operation tracers record into its private store.
		traceProvider *sdktrace.TracerProvider
		// mu serializes every read-check-write of a saved card in this
		// process; the cards directory lock keeps other processes out.
		mu sync.RWMutex
		// cards saves pending and confirmed CP1 Change Cards by change_id.
		cards *cardStore
		// modelMu guards liveModelEnv.
		modelMu sync.Mutex
		// liveModelEnv is the environment the next CP3 live target starts
		// with: Options.Live.Env until configure_model replaces it.
		liveModelEnv []string
		// liveModelEnvFor is Options.LiveModelEnv, or nil when runtime model
		// reconfiguration is not configured.
		liveModelEnvFor LiveModelEnv
		// promotionTarget supplies CP5 reports and current promotion inputs;
		// nil when the project server bound none.
		promotionTarget PromotionTarget
		// resolvePromotionTarget dynamically resolves a PromotionTarget from
		// the currently bound workspace when promotionTarget is nil.
		resolvePromotionTarget func(ctx context.Context, workspaceDir string) (PromotionTarget, error)
		// verification is the operator's static CP4 configuration, or nil.
		verification *VerificationConfig
		// runsDir is the protected verification runs directory when
		// verification is nil.
		runsDir string
		// resolveVerification dynamically resolves a VerificationConfig from
		// the currently bound workspace and its trusted starter suite commit.
		resolveVerification func(ctx context.Context, workspaceDir, suiteCommit string) (*VerificationConfig, error)
		// evaluator evaluates designs for preview_architecture and diff. It
		// creates its build workspace on the first evaluation, and Close
		// removes it.
		evaluator *designeval.Evaluator
		// workspaceMu serializes InitProject and guards activeWorkspaceDir,
		// boundWorkspaceDev, boundWorkspaceIno, and boundSuiteCommit.
		workspaceMu sync.RWMutex
		// initialWorkspaceRoot is the cleaned physical path of
		// Options.WorkspaceDir at startup, or empty when unbound.
		initialWorkspaceRoot string
		// protectedDirs holds the deduplicated physical paths of
		// initialWorkspaceRoot and its enclosing Git working tree root,
		// computed once at startup.
		protectedDirs []string
		// initialWorkspaceDev and initialWorkspaceIno record the device and
		// inode of initialWorkspaceRoot at startup.
		initialWorkspaceDev uint64
		initialWorkspaceIno uint64
		// activeWorkspaceDir is the currently bound workspace directory.
		activeWorkspaceDir string
		// boundWorkspaceDev and boundWorkspaceIno record the device and inode
		// of activeWorkspaceDir.
		boundWorkspaceDev uint64
		boundWorkspaceIno uint64
		// boundSuiteCommit is the trusted initial starter commit recorded by
		// init_project or restored from workspace.json.
		boundSuiteCommit string
		// initStageDir is Options.InitStageDir.
		initStageDir string
		// checker is Options.Checker.
		checker *candidate.ContainerRunner
		// checkHelper is Options.CheckHelper.
		checkHelper CheckHelper
		// checkWorkDir is <cards dir>/check-work when Checker is set, the
		// parent of per-operation directories; empty otherwise. The card
		// store's exclusive lock keeps other servers out of it; each
		// operation directory is owned by the check helper that holds its
		// lock, possibly started by an earlier server, and recovery acts on
		// one only once that lock is free.
		checkWorkDir string
		// checkTimeout is Options.CheckTimeout.
		checkTimeout time.Duration
		// goProxyEnv is Options.GoProxyEnv.
		goProxyEnv []string
		// checkOps counts running check operations so Close can wait for
		// their check helpers to finish.
		checkOps sync.WaitGroup
		// storyRefreshMu guards storyRefreshes and each storyRefreshOp's state.
		storyRefreshMu sync.Mutex
		// storyRefreshes holds the current in-memory Story refresh operation per
		// authenticated principal and bound workspace identity.
		storyRefreshes map[storyRefreshKey]*storyRefreshOp
		// storyRefreshOps counts running Story refresh evaluations so Close can
		// wait for them to finish before closing the evaluator.
		storyRefreshOps sync.WaitGroup
		// storyRefreshTimeout overrides defaultStoryRefreshTimeout in tests when positive.
		storyRefreshTimeout time.Duration
		// changesWait and changesPoll override defaultChangesWait and
		// defaultChangesPoll in tests when positive.
		changesWait, changesPoll time.Duration
		// newGenerator constructs a fresh scaffold.Generator for each
		// InitProject call when non-nil.
		newGenerator func() scaffold.Generator
		// initRunner runs external commands during InitProject.
		initRunner scaffold.Runner
		// surlaVersion pins the Surla module version in eval/go.mod during
		// InitProject.
		surlaVersion string
		// initStepHook is an optional test hook invoked in InitProject at
		// "after_wal" (after Step 1 writeWorkspaceRecord before renameat.NoReplace)
		// and "after_rename" (after renameat.NoReplace and Fsync before verifyPublishedTarget).
		initStepHook func(step string)
		// liveState holds the live CP3 target state.
		liveState
		// buildTeam is Options.BuildTeam, or nil when none is bound.
		buildTeam BuildTeam
		// workingCopyTurn is the one lock on the project's test working copy,
		// a channel with room for one value: a sender takes the turn, a
		// receive releases it, and a waiter can give up when its context
		// ends. Whoever holds the turn is the only one opening the copy,
		// writing the required tests into it, checking it or recording its
		// accepted tests, for the whole of that step. Take it before mu and
		// never while holding mu: those steps run for minutes, and mu guards
		// every card and build read. Nothing takes it yet.
		workingCopyTurn chan struct{}
		// buildStartOps counts accepted starts, admitted through workspaceMu's
		// existing shutdown barrier and drained before stores and evaluators close.
		buildStartOps sync.WaitGroup
		// buildStartTimeout replaces defaultBuildStartTimeout in focused tests.
		buildStartTimeout time.Duration
		// team is the single injected owner; teamWorker drains before stores close.
		team       *TeamBinding
		teamWorker sync.WaitGroup
	}
)

const (
	// StatusNeedsConfirmation indicates CP1 halted on genuine domain ambiguity.
	StatusNeedsConfirmation = "needs_confirmation"
	// StatusConfirmed indicates CP1 is locked and ready for codegen/verification.
	StatusConfirmed = "confirmed"
	// TargetKindPromptTemplate identifies a 0-second target prompt override in CP3.
	TargetKindPromptTemplate = "prompt_template"
	// TargetKindCompiledRule identifies a compiled Go business rule that cannot be overridden via prompt store.
	TargetKindCompiledRule = "compiled_rule"
)

// WithPrincipal returns a child context carrying the authenticated Principal.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

// PrincipalFromContext extracts the authenticated Principal from ctx. It
// reports none when the ID is empty or not valid UTF-8: the ID is saved as a
// card's owner, and invalid bytes would be saved as U+FFFD, so the same
// principal would no longer own its own card.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	if ctx == nil {
		return Principal{}, false
	}
	p, ok := ctx.Value(principalContextKey{}).(Principal)
	if !ok || strings.TrimSpace(p.ID) == "" || !utf8.ValidString(p.ID) {
		return Principal{}, false
	}
	p.ID = strings.TrimSpace(p.ID)
	return p, true
}

// New constructs a ControlPlane that saves its Change Cards under
// opts.CardsDir, so cards written by an earlier process are served again. It
// creates the directory when missing and holds its lock until Close; it fails
// when CardsDir is empty or another process holds the lock.
func New(opts Options) (*ControlPlane, error) {
	cleanupFailedNew := func() error {
		var evalErr error
		if opts.Evaluator != nil {
			evalErr = opts.Evaluator.Close()
		}
		return evalErr
	}
	surlaVersion := strings.TrimSpace(opts.SurlaVersion)
	if surlaVersion != "" && !scaffold.ValidVersion(surlaVersion) {
		err := fmt.Errorf("surla version %q must be a canonical semantic version such as v0.1.0", opts.SurlaVersion)
		return nil, errors.Join(err, cleanupFailedNew())
	}
	wsRoot, wsDev, wsIno, err := openWorkspaceRootAndStat(opts.WorkspaceDir)
	if err != nil {
		return nil, errors.Join(err, cleanupFailedNew())
	}
	runsDir, initStageDir, verCfg, protectedDirs, err := normalizeAndValidateStartupPaths(opts, wsRoot)
	if err != nil {
		return nil, errors.Join(err, cleanupFailedNew())
	}
	cards, err := openCardStore(opts.CardsDir)
	if err != nil {
		return nil, errors.Join(err, cleanupFailedNew())
	}
	traces, err := traceview.Open(cards.dir)
	if err != nil {
		return nil, errors.Join(err, cards.close(), cleanupFailedNew())
	}
	eval := opts.Evaluator
	if eval == nil {
		if wsRoot != "" {
			eval = designeval.NewEvaluatorInDir(filepath.Join(cards.dir, "designeval"))
		} else {
			eval = designeval.NewEvaluator()
		}
	}
	var initialLiveEnv []string
	if opts.Live != nil {
		initialLiveEnv = opts.Live.Env
	}
	cp := &ControlPlane{
		cards:                  cards,
		traces:                 traces,
		traceProvider:          opts.TracerProvider,
		liveModelEnv:           initialLiveEnv,
		liveModelEnvFor:        opts.LiveModelEnv,
		promotionTarget:        opts.PromotionTarget,
		resolvePromotionTarget: opts.ResolvePromotionTarget,
		verification:           verCfg,
		runsDir:                runsDir,
		resolveVerification:    opts.ResolveVerification,
		evaluator:              eval,
		initialWorkspaceRoot:   wsRoot,
		protectedDirs:          protectedDirs,
		initialWorkspaceDev:    wsDev,
		initialWorkspaceIno:    wsIno,
		activeWorkspaceDir:     wsRoot,
		boundWorkspaceDev:      wsDev,
		boundWorkspaceIno:      wsIno,
		initStageDir:           initStageDir,
		checker:                opts.Checker,
		checkHelper:            opts.CheckHelper,
		checkTimeout:           opts.CheckTimeout,
		goProxyEnv:             opts.GoProxyEnv,
		newGenerator:           opts.NewGenerator,
		initRunner:             opts.InitRunner,
		surlaVersion:           surlaVersion,
		liveState:              newLiveState(opts.Live, cards.dir),
		buildTeam:              opts.BuildTeam,
		workingCopyTurn:        make(chan struct{}, 1),
		team:                   opts.Team,
	}
	if err := cp.restoreWorkspace(); err != nil {
		return nil, errors.Join(err, cp.Close())
	}
	if err := validateStartupSeparation(cards.dir, runsDir, initStageDir, verCfg, protectedDirs...); err != nil {
		return nil, errors.Join(err, cp.Close())
	}
	if opts.Checker != nil {
		if opts.CheckTimeout <= 0 {
			return nil, errors.Join(fmt.Errorf("check timeout must be positive, got %s", opts.CheckTimeout), cp.Close())
		}
		if opts.CheckHelper == nil {
			return nil, errors.Join(errors.New("check operations need Options.CheckHelper when Options.Checker is set: they run in a helper process the composition root binds"), cp.Close())
		}
		cp.checkWorkDir = filepath.Join(cards.dir, "check-work")
		if err := os.MkdirAll(cp.checkWorkDir, 0o700); err != nil {
			return nil, errors.Join(fmt.Errorf("create check work directory: %w", err), cp.Close())
		}
		// Retry removal of containers that a previous server with this card
		// store could not confirm. A failed retry keeps its directory for
		// Close and the next start, and its span records the failure, so it
		// does not stop this server from starting.
		cp.recoverCheckOperations()
	}
	if cp.team != nil && (cp.team.Owner == "" || cp.team.Service == nil) {
		return nil, errors.Join(errors.New("team binding requires its owner and service"), cp.Close())
	}
	cp.startTeamSupervisor()
	return cp, nil
}

// NewHTTPHandler mounts both the Goa HTTP endpoints and the MCP JSON-RPC
// server (`/mcp`) for svc onto a single HTTP handler protected by auth. The
// HTTP endpoints decode JSON bodies with StrictJSONRequestDecoder; the MCP
// server keeps Goa's decoder for its JSON-RPC envelope, because its generated
// tool codecs already refuse undeclared argument fields.
// auth is required and is supplied by the project credential owner.
func NewHTTPHandler(svc gensurla.Service, auth RequestAuthenticator) http.Handler {
	mux := goahttp.NewMuxer()
	endpoints := gensurla.NewEndpoints(svc)
	recorder := otel.Tracer("surla/requests")
	if owner, ok := svc.(*ControlPlane); ok {
		recorder = owner.tracer("surla/requests")
	}
	applyOperationPolicy(endpoints, recorder)
	httpServer := gensurlahttp.New(
		endpoints,
		mux,
		StrictJSONRequestDecoder,
		goahttp.ResponseEncoder,
		nil,
		nil,
	)
	gensurlahttp.Mount(mux, httpServer)

	mcpAdapter := genmcpadapter.NewMCPAdapter(endpoints, nil)
	mcpEndpoints := genmcpadapter.NewEndpoints(mcpAdapter)
	filterMCPDiscovery(mcpEndpoints)
	mcpServer := genmcpsurla.New(
		mcpEndpoints,
		mux,
		goahttp.RequestDecoder,
		goahttp.ResponseEncoder,
		nil,
	)
	genmcpsurla.Mount(mux, mcpServer)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := auth(r)
		if err != nil {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			if encodeErr := goahttp.ResponseEncoder(r.Context(), w).Encode(goahttp.NewErrorResponse(r.Context(), gensurla.MakeForbidden(errors.New("open the project views with surla; session or credential is no longer valid")))); encodeErr != nil {
				otel.Handle(encodeErr)
			}
			return
		}
		r = r.WithContext(WithPrincipal(r.Context(), principal))
		mux.ServeHTTP(w, r)
	})
}

// GoldenPath returns the Markdown `surla://golden-path` serves: the Golden
// Path architecture playbook followed by the decision component catalog for
// an analysis's optional ui_composition.
func (c *ControlPlane) GoldenPath(_ context.Context) (string, error) {
	return goldenpath.ResourceMarkdown(), nil
}

// ProposeChange saves a new Checkpoint 1 (CP1) Change Card from the caller's
// feature request and analysis. It first checks p against the design's rules
// with the generated gensurla.ValidateProposeChangePayloadValue, so a Go
// caller gets the same structural checks the HTTP and MCP decoders apply
// (required, non-blank and valid UTF-8 text, minimum counts, enums, no null
// list items); invalid UTF-8 matters because a change_id saved as U+FFFD
// could not be found again. It then checks the rule that spans fields (each
// ambiguity's options are distinct), numbers ambiguities
// <change_id>-ambiguity-NN and examples check_NN in list order, records the
// design source and optional Stories (when story_refresh_op_id is given, the
// design/*.go snapshot and evaluated Stories from that succeeded
// refresh_story operation; otherwise design/*.go
// from the bound workspace; else none), and binds the card to the
// authenticated principal. Every proposed card waits in needs_confirmation for the user, including a card without ambiguities. A change_id that
// already has a card is a conflict; a payload that breaks a rule, a blank
// change_id or prompt, an invalid or stale story_refresh_op_id, or an
// unreadable workspace design is unprocessable, and none saves anything.
func (c *ControlPlane) ProposeChange(ctx context.Context, p *gensurla.ProposeChangePayload) (*gensurla.ChangeCardResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	if err := gensurla.ValidateProposeChangePayloadValue(p); err != nil {
		return nil, gensurla.MakeUnprocessable(err)
	}
	if strings.TrimSpace(p.ChangeID) == "" || strings.TrimSpace(p.Prompt) == "" {
		return nil, gensurla.MakeUnprocessable(errors.New("change_id and prompt are required"))
	}
	var designSrc string
	var stories []designeval.StorySummary
	switch {
	case p.StoryRefreshOpID != nil:
		var err error
		designSrc, stories, err = c.adoptStoryRefresh(principal, *p.StoryRefreshOpID)
		if err != nil {
			return nil, err
		}
	default:
		if wsDir, wsDev, wsIno := c.currentWorkspaceTarget(); wsDir != "" {
			src, err := readWorkspaceDesignSource(wsDir, wsDev, wsIno)
			if err != nil {
				return nil, err
			}
			designSrc = src
		}
	}

	_, span := c.tracer("surla/controlplane").Start(
		ctx,
		"surla.controlplane.propose_change",
		trace.WithAttributes(attribute.String("surla.change_id", p.ChangeID)),
	)
	defer span.End()

	ambiguities, examples, preserved, err := proposedCardContent(p.ChangeID, p.Analysis)
	if err != nil {
		span.SetAttributes(attribute.String("surla.public_summary", cp1RejectedAnalysisSummary(false)))
		return nil, gensurla.MakeUnprocessable(fmt.Errorf("invalid analysis: %w", err))
	}
	status := StatusNeedsConfirmation

	card := &gensurla.ChangeCardResult{
		ChangeID:           p.ChangeID,
		Status:             status,
		Prompt:             p.Prompt,
		Summary:            p.Analysis.Summary,
		Ambiguities:        ambiguities,
		Examples:           examples,
		PreservedBehaviors: preserved,
		Stories:            storySummaryItemsOrNil(stories),
	}
	rev, err := computeCanonicalRevision(card, principal.ID, designSrc)
	if err != nil {
		span.SetAttributes(attribute.String("surla.public_summary", cp1SafeFaultSummary(false, cp1FaultRevision)))
		span.RecordError(err)
		span.SetStatus(codes.Error, "compute cp1 card revision")
		return nil, err
	}
	card.RequestRevision = rev
	// The composition is bound to the revision it was composed with but is
	// not part of it: request_revision covers only the authoritative card.
	if p.Analysis.UIComposition != nil {
		card.UIComposition = &gensurla.UICompositionItem{Program: *p.Analysis.UIComposition, ComposedForRevision: rev}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	_, exists, err := c.cards.load(p.ChangeID)
	if err != nil {
		span.SetAttributes(attribute.String("surla.public_summary", cp1SafeFaultSummary(false, cp1FaultStorageRead)))
		span.RecordError(err)
		span.SetStatus(codes.Error, "load cp1 change card")
		return nil, err
	}
	if exists {
		span.SetAttributes(attribute.String("surla.public_summary", "Change Card ID already exists (this request did not overwrite the existing Change Card); call get_change or choose a new change_id."))
		return nil, gensurla.MakeConflict(fmt.Errorf("change_id %q already exists", p.ChangeID))
	}
	if err := c.cards.save(storedChangeCard{
		card:         card,
		owner:        principal.ID,
		designSource: designSrc,
		stories:      stories,
	}); err != nil {
		span.SetAttributes(attribute.String("surla.public_summary", cp1SafeFaultSummary(false, cp1FaultStorageSave)))
		span.RecordError(err)
		span.SetStatus(codes.Error, "save cp1 change card")
		return nil, err
	}
	span.SetAttributes(attribute.String("surla.public_summary", fmt.Sprintf("Proposed Change Card (%s).", status)))
	return card, nil
}

// Close shuts the control plane down. It first refuses new live targets,
// then stops the live target, then cancels the design evaluations in
// progress, waits for them, and removes their build workspace, and finally
// releases the card store's lock so another process can use the directory.
// Local tracing stays active through owned work and cleanup; its
// file then closes, and later spans are discarded. The host flushes its other
// exporters afterward.
// It always attempts the evaluator cleanup, and joins its error with
// any other. If the live target cannot be confirmed stopped, the cards
// directory stays locked on this and every later call,
// because the target's processes may still use its files. Card cleanup waits for a
// card read or save in progress; requests that reach the cards afterwards,
// such as one still waiting on a promotion target, fail without
// writing. A later call returns the same evaluator result and, if the live
// target could not be stopped, the same failure; the card store's lock
// is released only once.
func (c *ControlPlane) Close() error {
	c.cancelServer()
	c.teamWorker.Wait()
	// Wait out any check admission in progress: an operation admitted under
	// workspaceMu before this point is already registered, and every later
	// one sees the server context ended and is refused (see
	// admitCheckOperation).
	c.workspaceMu.Lock()
	c.workspaceMu.Unlock()
	c.buildStartOps.Wait()
	// Running checks see the server context end, remove their containers
	// and return; then any container still unconfirmed gets one more try.
	c.checkOps.Wait()
	checkErr := c.recoverCheckOperations()
	c.closeStoryRefreshes()
	liveErr := c.closeLive()
	evalErr := c.evaluator.Close()
	if liveErr != nil {
		return errors.Join(evalErr, fmt.Errorf("stop live target; cards directory %s stays locked: %w", c.cards.dir, liveErr))
	}
	c.workspaceMu.Lock()
	defer c.workspaceMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	return errors.Join(evalErr, checkErr, c.traces.Close(), c.cards.close())
}

// GetChange returns the current Change Card for change_id to its owner. It
// fails for an unknown change_id or a caller who does not own the card, and
// never changes the card. A saved approval is returned in Approval only while
// it still matches the promotion target's current inputs, or in
// UnverifiedApproval when no promotion target is configured or the target
// cannot report its current inputs (see currentCard). A revision proposed
// beside a confirmed card is returned in ProposedRevision while it waits.
func (c *ControlPlane) GetChange(ctx context.Context, p *gensurla.GetChangePayload) (*gensurla.ChangeCardResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	c.mu.RLock()
	record, ok, err := c.cards.load(p.ChangeID)
	c.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, gensurla.MakeNotFound(fmt.Errorf("unknown change_id %q", p.ChangeID))
	}
	if record.owner != principal.ID {
		return nil, gensurla.MakeForbidden(fmt.Errorf("principal %q is not the owner of change_id %q", principal.ID, p.ChangeID))
	}
	return c.changeResult(ctx, record)
}

// PreviewArchitecture evaluates the candidate design module snapshot via the
// real Goa evaluation engine (the control plane's designeval.Evaluator) bound to a confirmed CP1
// Change Card and returns the validated C4 topology, directed edges, per-agent
// runtime policies, and tool contracts.
func (c *ControlPlane) PreviewArchitecture(ctx context.Context, p *gensurla.PreviewArchitecturePayload) (*gensurla.ArchitecturePreviewResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	if p == nil || strings.TrimSpace(p.ChangeID) == "" || strings.TrimSpace(p.RequestRevision) == "" {
		return nil, gensurla.MakeUnprocessable(errors.New("change_id and request_revision are required"))
	}

	c.mu.RLock()
	record, exists, err := c.cards.load(p.ChangeID)
	c.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, gensurla.MakeNotFound(fmt.Errorf("unknown change_id %q", p.ChangeID))
	}
	if record.owner != principal.ID {
		return nil, gensurla.MakeForbidden(fmt.Errorf("principal %q is not the owner of change_id %q", principal.ID, p.ChangeID))
	}
	if record.card.Status != StatusConfirmed {
		return nil, gensurla.MakeConflict(fmt.Errorf("change_id %q must be confirmed before architecture preview (status %q)", p.ChangeID, record.card.Status))
	}
	if record.card.RequestRevision != p.RequestRevision {
		return nil, gensurla.MakeConflict(fmt.Errorf("request_revision mismatch for %q: got %q, want %q", p.ChangeID, p.RequestRevision, record.card.RequestRevision))
	}

	wsDir, wsDev, wsIno := c.currentWorkspaceTarget()
	if wsDir == "" {
		return nil, gensurla.MakeUnprocessable(errors.New(
			"workspace directory is not bound to this control plane",
		))
	}
	designFiles, err := readWorkspaceModuleSnapshot(wsDir, wsDev, wsIno)
	if err != nil {
		return nil, err
	}

	if err := reportProgress(ctx, 0, 2, "Evaluating the candidate design in the sandbox"); err != nil {
		return nil, err
	}
	evaluated, err := c.evaluator.Evaluate(ctx, designFiles)
	if err != nil {
		return nil, err
	}
	if evaluated.SystemName == "" || len(evaluated.Containers) == 0 {
		return nil, gensurla.MakeUnprocessable(errors.New("design must declare at least one Goa API or Service container"))
	}
	for _, pol := range evaluated.AgentPolicies {
		if pol.MaxExecutionSeconds <= 0 || pol.MaxToolCalls <= 0 {
			return nil, gensurla.MakeUnprocessable(fmt.Errorf("agent %q must declare positive TimeBudget and DefaultCaps(MaxToolCalls) in RunPolicy", pol.AgentID))
		}
	}

	designDigest := designeval.SnapshotDigest(designFiles)
	if err := reportProgress(ctx, 2, 2, "Architecture preview ready"); err != nil {
		return nil, err
	}
	banner := evaluated.Summary()

	return &gensurla.ArchitecturePreviewResult{
		ChangeID:        p.ChangeID,
		RequestRevision: p.RequestRevision,
		DesignDigest:    designDigest,
		SystemName:      evaluated.SystemName,
		Containers:      evaluated.Containers,
		Edges:           evaluated.Edges,
		AgentPolicies:   evaluated.AgentPolicies,
		Tools:           evaluated.Tools,
		Banner:          banner,
	}, nil
}

// DraftOverride sends a prompt template to the live target start_live_target
// runs when target_kind is "prompt_template", or refuses to mask a compiled
// Go business rule when target_kind is "compiled_rule". Surla changes prompts
// only in a live target it built and started itself, so a prompt template
// with no live target running is refused with no_live_target, which names
// start_live_target as the step that enables it.
func (c *ControlPlane) DraftOverride(ctx context.Context, p *gensurla.DraftOverridePayload) (*gensurla.DraftOverrideResult, error) {
	if _, ok := PrincipalFromContext(ctx); !ok {
		return nil, errors.New("authenticated principal is required")
	}
	if p == nil {
		return nil, errors.New("draft override payload is required")
	}
	switch p.TargetKind {
	case TargetKindCompiledRule:
		return &gensurla.DraftOverrideResult{
			AppliedInStore:  false,
			RequiresRebuild: true,
			Message:         "Compiled Go business rule cannot be overridden in prompt.Store; requires Go rebuild and CP4 verification.",
		}, nil
	case TargetKindPromptTemplate:
		if p.PromptID == nil || strings.TrimSpace(*p.PromptID) == "" {
			return nil, errors.New("prompt_id is required when target_kind is prompt_template")
		}
		if strings.TrimSpace(p.NewContent) == "" {
			return nil, errors.New("new_content is required when target_kind is prompt_template")
		}
		lt := c.boundLive()
		if lt == nil {
			return nil, gensurla.MakeNoLiveTarget(errors.New(msgNoLiveTarget))
		}
		return c.draftLive(ctx, lt, strings.TrimSpace(*p.PromptID), p.NewContent)
	default:
		return nil, fmt.Errorf("unsupported target_kind %q", p.TargetKind)
	}
}

// Diff compares two Goa design sources and go.mod snapshots and returns the
// structural diff without emitting a blanket compatibility verdict.
func (c *ControlPlane) Diff(ctx context.Context, p *gensurla.DiffPayload) (*gensurla.DiffResult, error) {
	if p == nil {
		return nil, errors.New("diff payload is required")
	}
	rep, err := diff.Compare(ctx, c.evaluator, p.Old.Files, p.New.Files)
	if err != nil {
		return nil, err
	}
	return &gensurla.DiffResult{
		HasChanges:           rep.HasChanges,
		AddedDeclarations:    rep.AddedDeclarations,
		RemovedDeclarations:  rep.RemovedDeclarations,
		ModifiedDeclarations: rep.ModifiedDeclarations,
		VersionDeltas:        rep.VersionDeltas,
		Summary:              rep.Summary,
	}, nil
}

// StrictJSONRequestDecoder is the request body decoder of every HTTP route
// NewHTTPHandler mounts. It returns goahttp.RequestDecoder's decoder for r's
// Content-Type and, when that is a JSON decoder (application/json, or no
// Content-Type), makes it refuse object fields the route's design does not
// declare, at any depth. The generated route decoder reports such a body as a
// decode_payload error, HTTP 400, naming the field; for example a
// propose_change analysis carrying a caller-chosen ambiguity "id". Bodies in
// other formats Goa accepts (XML, gob, text) decode exactly as before.
func StrictJSONRequestDecoder(r *http.Request) goahttp.Decoder {
	dec := goahttp.RequestDecoder(r)
	if jsonDec, ok := dec.(*json.Decoder); ok {
		jsonDec.DisallowUnknownFields()
	}
	return dec
}

// computeCanonicalRevision computes a collision-free SHA-256 hex digest over
// the canonical JSON encoding of the ChangeCardResult, its owner principal, and
// its inspected design source.
func computeCanonicalRevision(card *gensurla.ChangeCardResult, owner, designSource string) (string, error) {
	keys := make([]string, 0, len(card.Answers))
	for k := range card.Answers {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	answers := make([]canonicalAnswerPair, 0, len(keys))
	for _, k := range keys {
		answers = append(answers, canonicalAnswerPair{
			ID:     k,
			Answer: card.Answers[k],
		})
	}
	var confirmedBy string
	if card.ConfirmedBy != nil {
		confirmedBy = *card.ConfirmedBy
	}
	payload := canonicalCardRevision{
		ChangeID:           card.ChangeID,
		Owner:              owner,
		Status:             card.Status,
		Prompt:             card.Prompt,
		DesignSource:       designSource,
		Stories:            canonicalStorySummariesFromItems(card.Stories),
		Summary:            card.Summary,
		Ambiguities:        card.Ambiguities,
		Answers:            answers,
		Examples:           card.Examples,
		PreservedBehaviors: card.PreservedBehaviors,
		ConfirmedBy:        confirmedBy,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal canonical change card revision: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// reportProgress sends one MCP progress update for the current request:
// value of total units done, with message. It does nothing when the caller did
// not ask for progress, and fails when an update cannot be delivered.
func reportProgress(ctx context.Context, value, total float64, message string) error {
	if err := mcp.ReportProgress(ctx, value, &total, &message); err != nil {
		return fmt.Errorf("report progress: %w", err)
	}
	return nil
}
