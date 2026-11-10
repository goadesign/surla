// Package controlplane implements the Surla MCP software factory control
// plane: authenticated intent & ambiguity resolution (CP1), evaluated C4
// architecture & cost/safety guardrail preview (CP2), target-bound Fast Draft
// Mode prompt overrides (CP3), and recorded promotion approval of a passed
// verification report (CP5).
package controlplane

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	"github.com/goadesign/surla/internal/swarm"
	"github.com/goadesign/surla/internal/traceview"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/verify/candidate"
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
	}

	// RequestAuthenticator verifies an incoming HTTP request and returns the
	// authenticated Principal.
	RequestAuthenticator func(r *http.Request) (Principal, error)

	// principalContextKey is the context key storing the authenticated Principal.
	principalContextKey struct{}

	// TargetPromptUpdater applies a Checkpoint 3 Fast Draft Mode prompt template
	// override to the bound target platform's runtime prompt store.
	TargetPromptUpdater interface {
		// ApplyPromptOverride updates promptID with newContent on the bound target
		// platform and returns the resulting prompt version identifier.
		ApplyPromptOverride(ctx context.Context, promptID, newContent string) (version string, err error)
	}

	// PromotionTarget is the promotion owner's view of a candidate being
	// promoted at Checkpoint 5. The embedding host binds it; approve_merge never
	// takes these facts from its caller, because a caller-supplied report or
	// binary digest would let the caller approve its own claims.
	PromotionTarget interface {
		// LoadReport returns the verification report that verify.Run wrote
		// under digest, from storage only the evaluator writes. It fails when
		// no such report exists.
		LoadReport(ctx context.Context, digest string) (verify.Report, error)
		// CurrentInputs returns the evaluator suite and environment the
		// promotion requires and the digests of the candidate binaries
		// measured independently for candidateCommit, as one coherent
		// snapshot the implementation does not change after returning it.
		// It fails when those facts do not fix the target's behavior, for
		// example when the host also binds Options.TargetUpdater to the same
		// running target, whose runtime prompt overrides no verification
		// report covers (see the verify package). approve_merge checks a
		// report against it, and get_change and confirm_intent return a
		// saved approval only while it still matches.
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
		// CheckTimeout bounds one whole check or dependency operation. It
		// must be positive when Checker is set; New rejects zero or a
		// negative value. Per-operation directories live under CardsDir,
		// whose exclusive lock makes this server their only owner.
		CheckTimeout time.Duration
		// ImportedReports are the validated absolute original report paths
		// recorded by -import-legacy-workspace; check copies never include
		// the entries the kernel resolves them to.
		ImportedReports []string
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
		// Analyzer performs CP1 semantic analysis via a model call.
		Analyzer CP1Analyzer
		// CloseAnalyzer releases the transport owned by Analyzer (such as a
		// gRPC model gateway connection) when Analyzer is superseded by
		// ConfigureModel, when ControlPlane.Close runs, or if New fails
		// validation.
		CloseAnalyzer func() error
		// ModelBuilder constructs replacement CP1 analyzers and CP3 live-target
		// environments when configure_model is called. Without it,
		// configure_model returns not_implemented.
		ModelBuilder ModelBuilder
		// TargetUpdater applies CP3 Fast Draft Mode prompt overrides to the bound target platform.
		TargetUpdater TargetPromptUpdater
		// SwarmProfiles lists server-configured coding-agent provider profiles for
		// proposing Chorus multi-agent role assignments at CP2.
		SwarmProfiles []swarm.ProviderProfile
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
		// CloseProjectLock releases the operator project directory's shared
		// import lock when ControlPlane.Close releases CardsDir (or if New
		// fails).
		CloseProjectLock func() error
	}

	// storedChangeCard holds a versioned Change Card alongside its authenticated
	// owner principal, original design source snapshot, and design source origin.
	storedChangeCard struct {
		// card is the latest versioned ChangeCardResult.
		card *gensurla.ChangeCardResult
		// owner is the authenticated Principal.ID that created the card.
		owner string
		// designSource is the Goa design source snapshot inspected at CP1.
		designSource string
		// sourceOrigin records whether designSource was supplied by the caller
		// ("supplied") or read from the bound workspace ("workspace").
		sourceOrigin string
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
		// modelMu serializes access to activeModel, modelClosed, and every
		// modelHandle's reference count and closeFn.
		modelMu sync.Mutex
		// activeModel holds the currently active CP1 analyzer and CP3
		// live-target environment snapshot. Guarded by modelMu.
		activeModel *modelHandle
		// modelClosed reports whether closeModel has run during Close. Guarded
		// by modelMu.
		modelClosed bool
		// modelBuilder constructs replacement CP1 analyzers and CP3 live-target
		// environments when configure_model is called, or nil when runtime
		// model reconfiguration is not configured.
		modelBuilder ModelBuilder
		// targetUpdater applies CP3 Fast Draft Mode prompt overrides to the target platform.
		targetUpdater TargetPromptUpdater
		// swarmProfiles stores server-configured coding-agent provider profiles.
		swarmProfiles []swarm.ProviderProfile
		// promotionTarget supplies CP5 reports and current promotion inputs;
		// nil when the host bound none.
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
		// checkWorkDir is <cards dir>/check-work when Checker is set, the
		// parent of per-operation directories; empty otherwise. The card
		// store's exclusive lock makes this server its only owner.
		checkWorkDir string
		// checkTimeout is Options.CheckTimeout.
		checkTimeout time.Duration
		// importedReports is Options.ImportedReports.
		importedReports []string
		// goProxyEnv is Options.GoProxyEnv.
		goProxyEnv []string
		// checkOps counts running check operations so Close can wait for
		// their containers to be removed.
		checkOps sync.WaitGroup
		// newGenerator constructs a fresh scaffold.Generator for each
		// InitProject call when non-nil.
		newGenerator func() scaffold.Generator
		// initRunner runs external commands during InitProject.
		initRunner scaffold.Runner
		// surlaVersion pins the Surla module version in eval/go.mod during
		// InitProject.
		surlaVersion string
		// closeProjectLock releases the operator project directory's shared
		// import lock when Close releases cards.
		closeProjectLock func() error
		// initStepHook is an optional test hook invoked in InitProject at
		// "after_wal" (after Step 1 writeWorkspaceRecord before RenameatxNp)
		// and "after_rename" (after RenameatxNp and Fsync before verifyPublishedTarget).
		initStepHook func(step string)
		// liveState holds the live CP3 target state.
		liveState
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
	// cp1AnalysisMaxOutputTokens is the output allowance for each CP1 model
	// turn (both Phase 1 read_design inspection turns and the final structured
	// Change Card completion), including reasoning tokens counted against that
	// allowance. It caps one model response only, not the cost of a whole
	// request or change. Providers such as Anthropic require a positive
	// allowance on every request. During Phase 1 inspection, an OutputLimited
	// tool batch is never executed; on the final structured completion, a
	// response marked OutputLimited fails Analyze or Revise even when its JSON
	// parses, so no card is stored from an incomplete final analysis.
	cp1AnalysisMaxOutputTokens = 16384
)

var (
	// checkpointUIHTML is the stateless MCP Apps (`ui://surla/checkpoint`,
	// `text/html;profile=mcp-app`) HTML document host embedded from
	// checkpoint.html.
	//
	//go:embed checkpoint.html
	checkpointUIHTML string
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
	return Principal{ID: strings.TrimSpace(p.ID)}, true
}

// NewTokenAuthenticator returns a RequestAuthenticator that validates
// `Authorization: Bearer <token>` headers in constant time against the
// configured token-to-principal map.
func NewTokenAuthenticator(tokens map[string]Principal) RequestAuthenticator {
	cloned := maps.Clone(tokens)
	return func(r *http.Request) (Principal, error) {
		if r == nil {
			return Principal{}, errors.New("missing http request")
		}
		raw := strings.TrimSpace(r.Header.Get("Authorization"))
		token, ok := strings.CutPrefix(raw, "Bearer ")
		if !ok || strings.TrimSpace(token) == "" {
			return Principal{}, errors.New("missing bearer token")
		}
		token = strings.TrimSpace(token)
		for expected, principal := range cloned {
			if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1 {
				if strings.TrimSpace(principal.ID) == "" {
					return Principal{}, errors.New("empty principal configured for token")
				}
				return Principal{ID: strings.TrimSpace(principal.ID)}, nil
			}
		}
		return Principal{}, errors.New("invalid bearer token")
	}
}

// New constructs a ControlPlane that saves its Change Cards under
// opts.CardsDir, so cards written by an earlier process are served again. It
// creates the directory when missing and holds its lock until Close; it fails
// when CardsDir is empty or another process holds the lock.
func New(opts Options) (*ControlPlane, error) {
	cleanupFailedNew := func() error {
		var evalErr, lockErr error
		if opts.Evaluator != nil {
			evalErr = opts.Evaluator.Close()
		}
		if opts.CloseProjectLock != nil {
			lockErr = opts.CloseProjectLock()
		}
		return errors.Join(closeInitialAnalyzer(opts.CloseAnalyzer), evalErr, lockErr)
	}
	if opts.TargetUpdater != nil && opts.Live != nil {
		err := errors.New("set either TargetUpdater or Live, not both: draft_override sends prompt text to exactly one target")
		return nil, errors.Join(err, cleanupFailedNew())
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
		cards:         cards,
		traces:        traces,
		traceProvider: opts.TracerProvider,
		activeModel: &modelHandle{
			analyzer: opts.Analyzer,
			liveEnv:  initialLiveEnv,
			closeFn:  opts.CloseAnalyzer,
			refs:     1,
		},
		modelBuilder:           opts.ModelBuilder,
		targetUpdater:          opts.TargetUpdater,
		swarmProfiles:          opts.SwarmProfiles,
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
		checkTimeout:           opts.CheckTimeout,
		importedReports:        opts.ImportedReports,
		goProxyEnv:             opts.GoProxyEnv,
		newGenerator:           opts.NewGenerator,
		initRunner:             opts.InitRunner,
		surlaVersion:           surlaVersion,
		closeProjectLock:       opts.CloseProjectLock,
		liveState:              newLiveState(opts.Live, cards.dir),
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
	return cp, nil
}

// NewHTTPHandler mounts both the Goa HTTP endpoints and the MCP JSON-RPC
// server (`/mcp`) for svc onto a single HTTP handler protected by auth.
func NewHTTPHandler(svc gensurla.Service, auth RequestAuthenticator) http.Handler {
	mux := goahttp.NewMuxer()
	endpoints := gensurla.NewEndpoints(svc)
	httpServer := gensurlahttp.New(
		endpoints,
		mux,
		goahttp.RequestDecoder,
		goahttp.ResponseEncoder,
		nil,
		httpErrorFormatter,
	)
	gensurlahttp.Mount(mux, httpServer)

	mcpAdapter := genmcpadapter.NewMCPAdapter(endpoints, nil)
	mcpEndpoints := genmcpadapter.NewEndpoints(mcpAdapter)
	mcpServer := genmcpsurla.New(
		mcpEndpoints,
		mux,
		goahttp.RequestDecoder,
		goahttp.ResponseEncoder,
		nil,
	)
	genmcpsurla.Mount(mux, mcpServer)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth != nil {
			principal, err := auth(r)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			r = r.WithContext(WithPrincipal(r.Context(), principal))
		}
		mux.ServeHTTP(w, r)
	})
}

// CheckpointUI returns the interactive MCP Apps HTML host for `ui://surla/checkpoint`.
func (c *ControlPlane) CheckpointUI(_ context.Context) (string, error) {
	return checkpointUIHTML, nil
}

// GoldenPath returns the Surla Golden Path architecture playbook Markdown
// for `surla://golden-path`.
func (c *ControlPlane) GoldenPath(_ context.Context) (string, error) {
	return goldenpath.PlaybookMarkdown(), nil
}

// ProposeChange evaluates a feature request at Checkpoint 1 (CP1) via the
// configured CP1Analyzer model call, validates all model-emitted fields
// strictly, assigns code-owned ambiguity and example IDs, binds ownership to
// the authenticated principal, and stops for human confirmation only when
// genuine domain ambiguities are present.
func (c *ControlPlane) ProposeChange(ctx context.Context, p *gensurla.ProposeChangePayload) (*gensurla.ChangeCardResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	if p == nil || strings.TrimSpace(p.ChangeID) == "" || strings.TrimSpace(p.Prompt) == "" {
		return nil, gensurla.MakeUnprocessable(errors.New("change_id and prompt are required"))
	}
	// A saved card is found by its change_id; invalid UTF-8 would be saved as
	// U+FFFD and the card could not be found again. HTTP and MCP requests
	// are already valid, so only Go callers can send such an ID.
	if !utf8.ValidString(p.ChangeID) {
		return nil, gensurla.MakeUnprocessable(errors.New("change_id must be valid UTF-8"))
	}
	model, err := c.acquireModel()
	if err != nil {
		return nil, err
	}
	if model == nil {
		return nil, c.notConfiguredAnalyzerError()
	}
	defer func() {
		c.releaseModel(ctx, model)
	}()
	var design CP1Design
	var designSrc string
	sourceOrigin := sourceOriginSupplied
	switch {
	case p.DesignSource != nil:
		designSrc = *p.DesignSource
		design = SuppliedCP1Design(designSrc)
	default:
		wsDir, wsDev, wsIno := c.currentWorkspaceTarget()
		if wsDir != "" {
			design, designSrc, err = readWorkspaceCP1Design(wsDir, wsDev, wsIno)
			if err != nil {
				return nil, err
			}
			sourceOrigin = sourceOriginWorkspace
		} else {
			design = SuppliedCP1Design("")
		}
	}

	c.mu.RLock()
	_, alreadyExists, err := c.cards.load(p.ChangeID)
	c.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if alreadyExists {
		return nil, gensurla.MakeConflict(fmt.Errorf("change_id %q already exists", p.ChangeID))
	}

	ctx, span := c.tracer("surla/controlplane").Start(
		ctx,
		"surla.controlplane.propose_change",
		trace.WithAttributes(attribute.String("surla.change_id", p.ChangeID)),
	)
	defer span.End()

	if err := reportProgress(ctx, 0, 1, "Analyzing the feature request with the CP1 model"); err != nil {
		span.SetAttributes(attribute.String("surla.public_summary", cp1SafeFaultSummary(false, cp1FaultProgress)))
		span.RecordError(err)
		span.SetStatus(codes.Error, "report cp1 analysis progress")
		return nil, err
	}
	analysis, err := model.analyzer.Analyze(ctx, p.Prompt, design)
	if err != nil {
		if se, ok := asAnalysisRejected(err); ok {
			recordCP1RejectionSpan(span, false, err)
			err = newAnalysisRejected(
				fmt.Sprintf(
					"cp1 analysis rejected (%s): this propose_change request did not save Change Card %q; retry propose_change (including design_source when evaluating an existing Goa design), or call get_change if another request may have created the card",
					se.Message,
					p.ChangeID,
				),
				err,
			)
		} else {
			span.SetAttributes(attribute.String("surla.public_summary", cp1SafeAnalyzerErrorSummary(false, err)))
			if containsAnalysisRejected(err) {
				err = unclassifiedCP1Error{cause: err}
			}
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "analyze cp1 change card")
		return nil, err
	}
	if err := reportProgress(ctx, 1, 1, "Validating the CP1 analysis"); err != nil {
		span.SetAttributes(attribute.String("surla.public_summary", cp1SafeFaultSummary(false, cp1FaultProgress)))
		span.RecordError(err)
		span.SetStatus(codes.Error, "report cp1 validation progress")
		return nil, err
	}

	summary, ambiguities, examples, preserved, err := validateCP1Analysis(p.ChangeID, analysis)
	if err != nil {
		cause := &cp1Rejection{kind: "semantic_validation", err: err}
		recordCP1RejectionSpan(span, false, cause)
		se := newAnalysisRejected(
			fmt.Sprintf(
				"cp1 analysis rejected (%s): this propose_change request did not save Change Card %q; retry propose_change (including design_source when evaluating an existing Goa design), or call get_change if another request may have created the card",
				err.Error(),
				p.ChangeID,
			),
			cause,
		)
		span.RecordError(se)
		span.SetStatus(codes.Error, "validate cp1 analysis")
		return nil, se
	}

	status := StatusConfirmed
	var confirmedBy *string
	if len(ambiguities) > 0 {
		status = StatusNeedsConfirmation
	} else {
		ownerCopy := principal.ID
		confirmedBy = &ownerCopy
	}

	card := &gensurla.ChangeCardResult{
		ChangeID:           p.ChangeID,
		Status:             status,
		Prompt:             p.Prompt,
		Summary:            summary,
		Ambiguities:        ambiguities,
		Examples:           examples,
		PreservedBehaviors: preserved,
		ConfirmedBy:        confirmedBy,
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
	if analysis.UIComposition != nil {
		card.UIComposition = &gensurla.UICompositionItem{Program: *analysis.UIComposition, ComposedForRevision: rev}
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
		sourceOrigin: sourceOrigin,
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
// releases the cards directory and operator project lock so another process can
// use them. Local tracing stays active through owned work and cleanup; its
// file then closes, and later spans are discarded. The host flushes its other
// exporters afterward.
// It always attempts the evaluator cleanup, and joins its error with
// any other. If the live target cannot be confirmed stopped, the cards
// directory and operator project lock stay locked on this and every later call,
// because the target's processes may still use them. Card cleanup waits for a
// card read or save in progress; requests that reach the cards afterwards,
// such as one still waiting on a model or promotion target, fail without
// writing. A later call returns the same evaluator result and, if the live
// target could not be stopped, the same failure; the cards directory and
// project lock are released only once.
func (c *ControlPlane) Close() error {
	c.cancelServer()
	// Wait out any check admission in progress: an operation admitted under
	// workspaceMu before this point is already registered, and every later
	// one sees the server context ended and is refused (see
	// admitCheckOperation).
	c.workspaceMu.Lock()
	c.workspaceMu.Unlock()
	// Running checks see the server context end, remove their containers
	// and return; then any container still unconfirmed gets one more try.
	c.checkOps.Wait()
	checkErr := c.recoverCheckOperations()
	liveErr := c.closeLive()
	evalErr := c.evaluator.Close()
	modelErr := c.closeModel()
	if liveErr != nil {
		return errors.Join(evalErr, modelErr, fmt.Errorf("stop live target; cards directory %s stays locked: %w", c.cards.dir, liveErr))
	}
	c.workspaceMu.Lock()
	defer c.workspaceMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	var lockErr error
	if c.closeProjectLock != nil {
		lockErr = c.closeProjectLock()
		c.closeProjectLock = nil
	}
	return errors.Join(evalErr, modelErr, checkErr, c.traces.Close(), c.cards.close(), lockErr)
}

// GetChange returns the current Change Card for change_id to its owner. It
// fails for an unknown change_id or a caller who does not own the card, and
// never changes the card. A saved approval is returned in Approval only while
// it still matches the promotion target's current inputs, or in
// UnverifiedApproval when no promotion target is configured or the target
// cannot report its current inputs (see currentCard).
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
	return c.currentCard(ctx, record.card)
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

	var designFiles map[string]string
	switch {
	case p.Design != nil:
		designFiles = p.Design.Files
	default:
		wsDir, wsDev, wsIno := c.currentWorkspaceTarget()
		if wsDir == "" {
			return nil, gensurla.MakeUnprocessable(errors.New(
				"design is required when no workspace directory is bound to this control plane",
			))
		}
		designFiles, err = readWorkspaceModuleSnapshot(wsDir, wsDev, wsIno)
		if err != nil {
			return nil, err
		}
	}

	if err := reportProgress(ctx, 0, 2, "Evaluating the candidate design in the sandbox"); err != nil {
		return nil, err
	}
	evaluated, err := c.evaluator.Evaluate(ctx, designFiles)
	if err != nil {
		return nil, err
	}
	if err := reportProgress(ctx, 1, 2, "Planning the proposed swarm roster"); err != nil {
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
	swarmItem, err := planSwarmRoster(
		p.ChangeID,
		designDigest,
		c.swarmProfiles,
		p.Builders,
		p.ArchitectPaths,
		p.EvalPaths,
	)
	if err != nil {
		return nil, err
	}
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
		Swarm:           swarmItem,
		Banner:          banner,
	}, nil
}

// DraftOverride applies a 0-second prompt override on the bound target platform
// via TargetPromptUpdater when target_kind is "prompt_template", or refuses to
// mask a compiled Go business rule when target_kind is "compiled_rule".
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
		id := strings.TrimSpace(*p.PromptID)
		if c.targetUpdater == nil {
			if lt := c.boundLive(); lt != nil {
				return c.draftLive(ctx, lt, id, p.NewContent)
			}
			return nil, errors.New("no target prompt updater is bound to this control plane")
		}
		version, err := c.targetUpdater.ApplyPromptOverride(ctx, id, p.NewContent)
		if err != nil {
			return nil, fmt.Errorf("apply target prompt override: %w", err)
		}
		if strings.TrimSpace(version) == "" {
			return nil, errors.New("target prompt updater returned an empty version")
		}
		return &gensurla.DraftOverrideResult{
			AppliedInStore:  true,
			RequiresRebuild: false,
			PromptVersion:   &version,
			Message:         fmt.Sprintf("Updated prompt %q on target platform (version %s) for 0s hot reload.", id, version),
		}, nil
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

// planSwarmRoster invokes swarm.Plan over server-owned provider profiles and
// explicit workspace path scopes and projects the result into a
// gensurla.SwarmPlanItem.
func planSwarmRoster(
	changeID, designDigest string,
	profiles []swarm.ProviderProfile,
	in []*gensurla.BuilderScopeItem,
	architectPaths, evalPaths []string,
) (*gensurla.SwarmPlanItem, error) {
	builders := make([]swarm.BuilderScope, 0, len(in))
	for i, item := range in {
		if item == nil {
			return nil, gensurla.MakeUnprocessable(fmt.Errorf("builders[%d] is nil", i))
		}
		var preferred string
		if item.PreferredProfileID != nil {
			preferred = *item.PreferredProfileID
		}
		builders = append(builders, swarm.BuilderScope{
			Name:               item.Name,
			Paths:              item.Paths,
			PreferredProfileID: preferred,
		})
	}
	plan, err := swarm.Plan(swarm.Request{
		ChangeID:       changeID,
		DesignDigest:   designDigest,
		Profiles:       profiles,
		ArchitectPaths: architectPaths,
		Builders:       builders,
		EvalPaths:      evalPaths,
	})
	if err != nil {
		return nil, fmt.Errorf("plan swarm roster: %w", err)
	}

	assignments := make([]*gensurla.SwarmRoleItem, 0, len(plan.Assignments))
	for _, a := range plan.Assignments {
		assignments = append(assignments, &gensurla.SwarmRoleItem{
			Handle:     a.Handle,
			Role:       string(a.Role),
			ScopeName:  a.ScopeName,
			ProfileID:  a.ProfileID,
			Provider:   a.Provider,
			Model:      a.Model,
			Family:     a.Family,
			OwnedPaths: a.OwnedPaths,
			AccessMode: a.AccessMode,
		})
	}

	var missingRoles []string
	if len(plan.MissingRoles) > 0 {
		missingRoles = make([]string, 0, len(plan.MissingRoles))
		for _, r := range plan.MissingRoles {
			missingRoles = append(missingRoles, string(r))
		}
	}

	var manifestPtr *string
	if plan.Status == swarm.StatusConfigured {
		manifest, err := swarm.FormatManifest(changeID, designDigest, plan)
		if err != nil {
			return nil, fmt.Errorf("format swarm manifest: %w", err)
		}
		manifestPtr = &manifest
	}

	return &gensurla.SwarmPlanItem{
		Status:            string(plan.Status),
		Assignments:       assignments,
		CrossFamilyReview: plan.CrossFamilyReview,
		CrossFamilyEval:   plan.CrossFamilyEval,
		SingleFamily:      plan.SingleFamily,
		MissingRoles:      missingRoles,
		Summary:           plan.Summary,
		Manifest:          manifestPtr,
	}, nil
}
