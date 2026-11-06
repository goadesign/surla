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
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	gensurlahttp "github.com/goadesign/surla/gen/http/surla/server"
	genmcpsurla "github.com/goadesign/surla/gen/jsonrpc/mcp_surla/server"
	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/diff"
	"github.com/goadesign/surla/internal/goldenpath"
	"github.com/goadesign/surla/internal/swarm"
	"github.com/goadesign/surla/internal/verify"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"goa.design/goa-ai/runtime/agent/model"
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

	// CP1Analyzer performs semantic Checkpoint 1 analysis of an operator feature
	// request against the current Goa design source.
	CP1Analyzer interface {
		// Analyze inspects the prompt and design source and returns structured
		// summary, open domain questions, concrete examples, and preserved
		// behaviors.
		Analyze(ctx context.Context, promptText, designSource string) (CP1SemanticAnalysis, error)
		// Revise re-analyzes a revised prompt and design source against the
		// prior Change Card's confirmed decisions, open questions, examples,
		// and preserved behaviors.
		Revise(ctx context.Context, prior CP1PriorCard, promptText, designSource string) (CP1SemanticAnalysis, error)
	}

	// CP1ModelAmbiguity is an open domain question returned by a CP1Analyzer
	// model call before Surla assigns its mechanical question ID.
	CP1ModelAmbiguity struct {
		// Question is the plain-English ambiguity question.
		Question string `json:"question"`
		// Options lists the candidate answers (at least 2 distinct options).
		Options []string `json:"options"`
	}

	// CP1ModelExample is a concrete input/outcome example returned by a
	// CP1Analyzer model call before Surla assigns its mechanical check ID.
	CP1ModelExample struct {
		// Scenario is the plain-English scenario description.
		Scenario string `json:"scenario"`
		// InputSummary describes the representative user request and fixture state.
		InputSummary string `json:"input_summary"`
		// ExpectedOutcome describes the expected side effect and response behavior.
		ExpectedOutcome string `json:"expected_outcome"`
		// VerificationKind classifies the check ("deterministic" or "model_judged").
		VerificationKind string `json:"verification_kind"`
	}

	// CP1SemanticAnalysis is the structured semantic output returned by a
	// CP1Analyzer model call.
	CP1SemanticAnalysis struct {
		// Summary is the plain-English description of the requested behavior.
		Summary string `json:"summary"`
		// Ambiguities lists genuine domain questions that require human confirmation.
		Ambiguities []CP1ModelAmbiguity `json:"ambiguities"`
		// Examples lists concrete input/outcome examples for the Change Card.
		Examples []CP1ModelExample `json:"examples"`
		// PreservedBehaviors lists existing contract invariants preserved by the change.
		PreservedBehaviors []string `json:"preserved_behaviors"`
		// UIComposition is an optional OpenUI Lang decision view composed from
		// the Surla decision catalog. Nil means the model composed none. Any
		// string the model returns, including an empty or invalid program, is
		// carried to the card unchanged: it is presentation only, so a bad
		// program never costs the operator the authoritative analysis.
		UIComposition *string `json:"ui_composition,omitempty"`
	}

	// ModelCP1Analyzer implements CP1Analyzer using a Goa-AI model.Client with
	// schema-validated structured JSON output.
	ModelCP1Analyzer struct {
		// client is the underlying Goa-AI model.Client.
		client model.Client
	}

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
		// Analyzer performs CP1 semantic analysis via a model call.
		Analyzer CP1Analyzer
		// TargetUpdater applies CP3 Fast Draft Mode prompt overrides to the bound target platform.
		TargetUpdater TargetPromptUpdater
		// SwarmProfiles lists server-configured coding-agent provider profiles for
		// proposing Chorus multi-agent role assignments at CP2.
		SwarmProfiles []swarm.ProviderProfile
		// PromotionTarget supplies CP5 verification reports and current
		// promotion inputs. Without it approve_merge fails.
		PromotionTarget PromotionTarget
		// Verification is the operator's CP4 configuration. Without it
		// verify_change, get_verification and list_verifications fail.
		Verification *VerificationConfig
		// Live lets start_live_target build and run CP3 live targets with
		// Verification's plan, in <CardsDir>/live. Without it, or without
		// Verification, start_live_target fails.
		Live *LiveConfig
	}

	// storedChangeCard holds a versioned Change Card alongside its authenticated
	// owner principal and original design source snapshot.
	storedChangeCard struct {
		// card is the latest versioned ChangeCardResult.
		card *gensurla.ChangeCardResult
		// owner is the authenticated Principal.ID that created the card.
		owner string
		// designSource is the Goa design source snapshot inspected at CP1.
		designSource string
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
		// mu serializes every read-check-write of a saved card in this
		// process; the cards directory lock keeps other processes out.
		mu sync.RWMutex
		// cards saves pending and confirmed CP1 Change Cards by change_id.
		cards *cardStore
		// analyzer performs semantic CP1 analysis.
		analyzer CP1Analyzer
		// targetUpdater applies CP3 Fast Draft Mode prompt overrides to the target platform.
		targetUpdater TargetPromptUpdater
		// swarmProfiles stores server-configured coding-agent provider profiles.
		swarmProfiles []swarm.ProviderProfile
		// promotionTarget supplies CP5 reports and current promotion inputs;
		// nil when the host bound none.
		promotionTarget PromotionTarget
		// verification is the operator's CP4 configuration, or nil.
		verification *VerificationConfig
		// evaluator evaluates designs for preview_architecture and diff. It
		// creates its build workspace on the first evaluation, and Close
		// removes it.
		evaluator *designeval.Evaluator
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
	// cp1AnalysisMaxOutputTokens is the output allowance for the one CP1
	// inference that writes the structured Change Card analysis. It caps that
	// response only, not the cost of a whole change. Providers such as
	// Anthropic require a positive allowance on every request. A response the
	// provider marks OutputLimited fails Analyze even when its JSON parses, so
	// no card is stored from an analysis that may be incomplete.
	cp1AnalysisMaxOutputTokens = 6144
)

var (
	// checkpointUIHTML is the stateless MCP Apps (`ui://surla/checkpoint`,
	// `text/html;profile=mcp-app`) HTML document host embedded from
	// checkpoint.html.
	//
	//go:embed checkpoint.html
	checkpointUIHTML string

	// cp1AnalysisJSONSchema is the canonical JSON Schema (Draft 2020-12) enforced
	// at the goa-ai model.Client boundary for CP1 semantic analysis.
	cp1AnalysisJSONSchema = []byte(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["summary", "ambiguities", "examples", "preserved_behaviors"],
  "properties": {
    "summary": {
      "type": "string",
      "minLength": 1
    },
    "ambiguities": {
      "type": "array",
      "description": "Open product, domain, or operational choices that only the requester can make. Leave out details the request, design, or playbook already settle.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["question", "options"],
        "properties": {
          "question": {
            "type": "string",
            "minLength": 1,
            "description": "One decision the requester must make, in plain language, saying what the choice changes."
          },
          "options": {
            "type": "array",
            "minItems": 2,
            "description": "Distinct, self-contained choices; each states the behavior it selects and what follows from it.",
            "items": {
              "type": "string",
              "minLength": 1
            }
          }
        }
      }
    },
    "examples": {
      "type": "array",
      "minItems": 1,
      "description": "Concrete acceptance examples drawn from the request and the design, never placeholder text; each becomes a check the change must pass.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["scenario", "input_summary", "expected_outcome", "verification_kind"],
        "properties": {
          "scenario": {
            "type": "string",
            "minLength": 1,
            "description": "Short name of the situation, in the requester's domain terms."
          },
          "input_summary": {
            "type": "string",
            "minLength": 1,
            "description": "The actual request, inputs, and starting state in this scenario."
          },
          "expected_outcome": {
            "type": "string",
            "minLength": 1,
            "description": "The observable result or refusal that must hold."
          },
          "verification_kind": {
            "type": "string",
            "enum": ["deterministic", "model_judged"],
            "description": "deterministic when code can check the outcome directly; model_judged when checking it needs judgment."
          }
        }
      }
    },
    "preserved_behaviors": {
      "type": "array",
      "items": {
        "type": "string",
        "minLength": 1
      }
    },
    "ui_composition": {
      "type": "string"
    }
  }
}`)
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
	if opts.TargetUpdater != nil && opts.Live != nil {
		return nil, errors.New("set either TargetUpdater or Live, not both: draft_override sends prompt text to exactly one target")
	}
	cards, err := openCardStore(opts.CardsDir)
	if err != nil {
		return nil, err
	}
	return &ControlPlane{
		cards:           cards,
		analyzer:        opts.Analyzer,
		targetUpdater:   opts.TargetUpdater,
		swarmProfiles:   opts.SwarmProfiles,
		promotionTarget: opts.PromotionTarget,
		verification:    opts.Verification,
		evaluator:       designeval.NewEvaluator(),
		liveState:       newLiveState(opts.Live, opts.CardsDir),
	}, nil
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

// NewModelCP1Analyzer constructs a CP1Analyzer backed by a Goa-AI model.Client.
func NewModelCP1Analyzer(client model.Client) *ModelCP1Analyzer {
	return &ModelCP1Analyzer{client: client}
}

// Analyze invokes the underlying Goa-AI model.Client with a JSON Schema
// StructuredOutput contract for CP1 semantic analysis and strictly decodes the
// validated CP1SemanticAnalysis response.
func (a *ModelCP1Analyzer) Analyze(ctx context.Context, promptText, designSource string) (CP1SemanticAnalysis, error) {
	return a.completeAnalysis(
		ctx,
		goldenpath.ArchitectSystemPrompt(),
		fmt.Sprintf("Request:\n%s\n\nDesign:\n%s", promptText, formatCP1DesignSection(designSource)),
	)
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
	if c.analyzer == nil {
		return nil, gensurla.MakeNotImplemented(errors.New("cp1 semantic analyzer is required"))
	}
	var designSrc string
	if p.DesignSource != nil {
		designSrc = *p.DesignSource
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

	ctx, span := otel.Tracer("surla/controlplane").Start(
		ctx,
		"surla.controlplane.propose_change",
		trace.WithAttributes(attribute.String("surla.change_id", p.ChangeID)),
	)
	defer span.End()

	if err := reportProgress(ctx, 0, 1, "Analyzing the feature request with the CP1 model"); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "report cp1 analysis progress")
		return nil, err
	}
	analysis, err := c.analyzer.Analyze(ctx, p.Prompt, designSrc)
	if err != nil {
		if se, ok := asAnalysisRejected(err); ok {
			err = newAnalysisRejected(
				fmt.Sprintf(
					"cp1 analysis rejected (%s): this propose_change request did not save Change Card %q; retry propose_change (including design_source when evaluating an existing Goa design), or call get_change if another request may have created the card",
					se.Message,
					p.ChangeID,
				),
				err,
			)
		} else {
			err = unclassifiedCP1Error{cause: err}
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "analyze cp1 change card")
		return nil, err
	}
	if err := reportProgress(ctx, 1, 1, "Validating the CP1 analysis"); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "report cp1 validation progress")
		return nil, err
	}

	summary, ambiguities, examples, preserved, err := validateCP1Analysis(p.ChangeID, analysis)
	if err != nil {
		se := newAnalysisRejected(
			fmt.Sprintf(
				"cp1 analysis rejected (%s): this propose_change request did not save Change Card %q; retry propose_change (including design_source when evaluating an existing Goa design), or call get_change if another request may have created the card",
				err.Error(),
				p.ChangeID,
			),
			err,
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
		span.RecordError(err)
		span.SetStatus(codes.Error, "load cp1 change card")
		return nil, err
	}
	if exists {
		return nil, gensurla.MakeConflict(fmt.Errorf("change_id %q already exists", p.ChangeID))
	}
	if err := c.cards.save(storedChangeCard{card: card, owner: principal.ID, designSource: designSrc}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "save cp1 change card")
		return nil, err
	}
	return card, nil
}

// Close shuts the control plane down. It first refuses new live targets,
// then stops the live target, then cancels the design evaluations in
// progress, waits for them, and removes their build workspace, and finally
// releases the cards directory so another process can use it. It always
// attempts the evaluator cleanup, and joins its error with any other. If the
// live target cannot be confirmed stopped, the cards directory stays locked
// on this and every later call, because the target's processes may still use
// it. Card cleanup waits for a card read or save in progress; requests that
// reach the cards afterwards, such as one still waiting on a model or
// promotion target, fail without writing. A later call returns the same
// evaluator result and, if the live target could not be stopped, the same
// failure; the cards directory is released only once.
func (c *ControlPlane) Close() error {
	c.cancelServer()
	liveErr := c.closeLive()
	evalErr := c.evaluator.Close()
	if liveErr != nil {
		return errors.Join(evalErr, fmt.Errorf("stop live target; cards directory %s stays locked: %w", c.cards.dir, liveErr))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return errors.Join(evalErr, c.cards.close())
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

	if err := reportProgress(ctx, 0, 2, "Evaluating the candidate design in the sandbox"); err != nil {
		return nil, err
	}
	evaluated, err := c.evaluator.Evaluate(ctx, p.Design.Files)
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

	designDigest := designeval.SnapshotDigest(p.Design.Files)
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

// extractAssistantText concatenates TextPart payloads from a model.Response.
func extractAssistantText(resp *model.Response) string {
	if resp == nil {
		return ""
	}
	var b strings.Builder
	for _, msg := range resp.Content {
		for _, part := range msg.Parts {
			if tp, ok := part.(model.TextPart); ok {
				b.WriteString(tp.Text)
			}
		}
	}
	return b.String()
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
