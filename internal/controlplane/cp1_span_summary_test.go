// This file tests code-authored CP1 OpenTelemetry span attributes
// (`surla.cp1.rejection_kind` and `surla.public_summary`) across `propose_change`
// and `revise_change` successes, rejections, conflicts, declared unprocessable
// errors, cancellations, and server faults, verifying request-scoped
// persistence claims and secret/path exclusion.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"goa.design/goa-ai/runtime/agent/model"
)

// TestCP1SpanAttributes_CodeAuthoredAndSecretExclusion verifies that
// surla.public_summary is strictly code-authored and request-scoped across
// success, model rejection, semantic validation rejection (including
// model-returned prior_id), concurrent request_revision conflicts, declared
// unprocessable errors, pure context cancellation/timeout, mixed fault joins,
// and unconfirmed storage_save faults.
func TestCP1SpanAttributes_CodeAuthoredAndSecretExclusion(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		require.NoError(t, tp.Shutdown(context.Background()))
	})

	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	wsDir := setupWorkspaceWithFiles(t, map[string]string{"design/design.go": "package design\n"})

	secretSummaryJSON := `{"summary":"private-prompt-paraphrase-secret","ambiguities":[],"examples":[{"scenario":"S","input_summary":"I","expected_outcome":"O","verification_kind":"deterministic"}],"preserved_behaviors":[]}`
	secretPriorIDReviseJSON := `{"summary":"Revised","ambiguities":[],"examples":[{"prior_id":"private-model-prior-id-secret","scenario":"S","input_summary":"I","expected_outcome":"O","verification_kind":"deterministic"}],"removed_example_ids":["check_01"],"preserved_behaviors":[]}`
	validSecretReviseJSON := `{"summary":"private-revised-summary-secret","ambiguities":[],"examples":[{"prior_id":"check_01","scenario":"S","input_summary":"I","expected_outcome":"O","verification_kind":"deterministic"}],"removed_example_ids":[],"preserved_behaviors":[]}`

	q := &queuedModelClient{
		responses: []queuedModelTurn{
			// 1. ProposeChange success (confirmed)
			toolUseTurn("c1", "read_design", `{"path":"design/design.go"}`),
			jsonAnswerTurn(secretSummaryJSON),
			// 2. ReviseChange semantic_validation rejection with secret prior_id
			toolUseTurn("c2", "read_design", `{"path":"design/design.go"}`),
			jsonAnswerTurn(secretPriorIDReviseJSON),
			// 3. ProposeChange final Phase 2 output_limited rejection
			toolUseTurn("c3", "read_design", `{"path":"design/design.go"}`),
			{
				resp: &model.Response{
					OutputLimited: true,
					StopReason:    "max_tokens",
					Content: []model.Message{{
						Role:  model.ConversationRoleAssistant,
						Parts: []model.Part{model.TextPart{Text: "private-truncated-secret"}},
					}},
				},
			},
			// 4. ProposeChange non-rejection provider/analyzer fault
			{
				err: errors.New("private-provider-credential-secret"),
			},
			// 5. ProposeChange valid response followed by card save storage fault
			toolUseTurn("call-save", "read_design", `{"path":"design/design.go"}`),
			jsonAnswerTurn(secretSummaryJSON),
			// 6. ReviseChange valid response followed by card save storage fault
			toolUseTurn("call-rev-save", "read_design", `{"path":"design/design.go"}`),
			jsonAnswerTurn(validSecretReviseJSON),
		},
	}
	cardsDir := t.TempDir()
	cp := newControlPlane(t, Options{
		CardsDir:     cardsDir,
		WorkspaceDir: wsDir,
		Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
	})

	card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-span", Prompt: "private-user-prompt-secret",
	})
	require.NoError(t, err)
	proposeSpan := lastSpanByName(t, exp.GetSpans(), "surla.controlplane.propose_change")
	pubSummary, ok := findSpanAttr(proposeSpan.Attributes, "surla.public_summary")
	require.True(t, ok)
	assert.Equal(t, "Proposed Change Card (confirmed).", pubSummary.AsString())
	assert.NotContains(t, pubSummary.AsString(), "private-")
	_, hasRej := findSpanAttr(proposeSpan.Attributes, "surla.cp1.rejection_kind")
	assert.False(t, hasRej)

	// ReviseChange fails semantic validation with model-invented prior_id; caller
	// error retains diagnostic detail, while surla.public_summary excludes it and
	// uses request-scoped persistence wording.
	_, err = cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID: "chg-span", RequestRevision: card.RequestRevision, Prompt: "private-revise-prompt-secret",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private-model-prior-id-secret")
	reviseSpan := lastSpanByName(t, exp.GetSpans(), "surla.controlplane.revise_change")
	rejKind, ok := findSpanAttr(reviseSpan.Attributes, "surla.cp1.rejection_kind")
	require.True(t, ok)
	assert.Equal(t, "semantic_validation", rejKind.AsString())
	revSummary, ok := findSpanAttr(reviseSpan.Attributes, "surla.public_summary")
	require.True(t, ok)
	assert.Equal(t, "CP1 revision rejected: model output failed Change Card semantic validation (this request did not modify the Change Card); retry revise_change.", revSummary.AsString())
	assert.NotContains(t, revSummary.AsString(), "private-")

	// OutputLimited rejection records output_limited and excludes model output.
	_, err = cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-span-lim", Prompt: "private-user-prompt-secret",
	})
	require.Error(t, err)
	limSpan := lastSpanByName(t, exp.GetSpans(), "surla.controlplane.propose_change")
	rejKind, ok = findSpanAttr(limSpan.Attributes, "surla.cp1.rejection_kind")
	require.True(t, ok)
	assert.Equal(t, "output_limited", rejKind.AsString())
	limSummary, ok := findSpanAttr(limSpan.Attributes, "surla.public_summary")
	require.True(t, ok)
	assert.Equal(t, "CP1 analysis rejected: provider stopped at output limit (this request did not save a Change Card); shorten the request and retry propose_change.", limSummary.AsString())
	assert.NotContains(t, limSummary.AsString(), "private-")

	// Non-rejection analyzer fault records operator recovery with request-scoped
	// persistence claim and without claiming unproven sub-component causes.
	_, err = cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-span-fault", Prompt: "private-user-prompt-secret",
	})
	require.Error(t, err)
	faultSpan := lastSpanByName(t, exp.GetSpans(), "surla.controlplane.propose_change")
	_, hasRej = findSpanAttr(faultSpan.Attributes, "surla.cp1.rejection_kind")
	assert.False(t, hasRej)
	faultSummary, ok := findSpanAttr(faultSpan.Attributes, "surla.public_summary")
	require.True(t, ok)
	assert.Equal(t, "CP1 analysis failed before completion (this request did not save a Change Card); ask the server operator to check server diagnostics and model configuration before retrying.", faultSummary.AsString())
	assert.NotContains(t, faultSummary.AsString(), "private-")

	// Card save storage faults on both ProposeChange and ReviseChange state that
	// saving could not be confirmed (since directory sync can fail after rename),
	// tell the caller to preserve their request and check get_change, and exclude
	// host filesystem paths.
	require.NoError(t, os.Mkdir(filepath.Join(cardsDir, stagingFile), 0o700))
	_, err = cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-span-save-fault", Prompt: "private-user-prompt-secret",
	})
	require.Error(t, err)
	saveSpan := lastSpanByName(t, exp.GetSpans(), "surla.controlplane.propose_change")
	_, hasRej = findSpanAttr(saveSpan.Attributes, "surla.cp1.rejection_kind")
	assert.False(t, hasRej)
	saveSummary, ok := findSpanAttr(saveSpan.Attributes, "surla.public_summary")
	require.True(t, ok)
	assert.Contains(t, saveSummary.AsString(), "could not confirm Change Card save")
	assert.Contains(t, saveSummary.AsString(), "get_change")
	assert.Contains(t, saveSummary.AsString(), "preserve your request")
	assert.Contains(t, saveSummary.AsString(), "operator")
	assert.NotContains(t, saveSummary.AsString(), "private-")
	assert.NotContains(t, saveSummary.AsString(), cardsDir)

	_, err = cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID: "chg-span", RequestRevision: card.RequestRevision, Prompt: "private-revise-prompt-secret",
	})
	require.Error(t, err)
	revSaveSpan := lastSpanByName(t, exp.GetSpans(), "surla.controlplane.revise_change")
	revSaveSummary, ok := findSpanAttr(revSaveSpan.Attributes, "surla.public_summary")
	require.True(t, ok)
	assert.Contains(t, revSaveSummary.AsString(), "could not confirm Change Card save")
	assert.Contains(t, revSaveSummary.AsString(), "get_change")
	assert.Contains(t, revSaveSummary.AsString(), "preserve your request")
	assert.Contains(t, revSaveSummary.AsString(), "operator")
	assert.NotContains(t, revSaveSummary.AsString(), "private-")
	assert.NotContains(t, revSaveSummary.AsString(), cardsDir)
}

// TestCP1SpanAttributes_AnalyzerErrorKindsAndConcurrentConflicts verifies
// request-scoped span summaries for bare custom rejections, declared
// unprocessable errors, pure context cancellation and deadline expiry, mixed
// fault joins, and concurrent ReviseChange request_revision conflicts.
func TestCP1SpanAttributes_AnalyzerErrorKindsAndConcurrentConflicts(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		require.NoError(t, tp.Shutdown(context.Background()))
	})

	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	sharedRejected := newAnalysisRejected("private-custom-rejection", errors.New("cause"))

	startedA, releaseA := make(chan struct{}), make(chan struct{})
	cp := newControlPlane(t, Options{
		CardsDir: t.TempDir(),
		Analyzer: &stubCP1Analyzer{
			analyzeFn: func(_ context.Context, promptText string, _ CP1Design) (CP1SemanticAnalysis, error) {
				switch promptText {
				case "seed":
					return CP1SemanticAnalysis{
						Summary: "Seed",
						Examples: []CP1ModelExample{{
							Scenario: "S", InputSummary: "I", ExpectedOutcome: "O", VerificationKind: "deterministic",
						}},
					}, nil
				case "custom_rejection":
					return CP1SemanticAnalysis{}, sharedRejected
				case "unprocessable":
					return CP1SemanticAnalysis{}, fmt.Errorf("wrap: %w", gensurla.MakeUnprocessable(errors.New("private-unprocessable-detail")))
				case "canceled":
					return CP1SemanticAnalysis{}, fmt.Errorf("cp1 turn: %w", context.Canceled)
				case "deadline":
					return CP1SemanticAnalysis{}, fmt.Errorf("cp1 turn: %w", context.DeadlineExceeded)
				case "mixed_join":
					return CP1SemanticAnalysis{}, errors.Join(sharedRejected, context.Canceled)
				default:
					return CP1SemanticAnalysis{}, errors.New("private-unexpected-fault")
				}
			},
			reviseFn: func(_ context.Context, prior CP1PriorCard, promptText string, _ CP1Design) (CP1RevisionSemanticAnalysis, error) {
				if promptText == "hold" {
					close(startedA)
					<-releaseA
				}
				return CP1RevisionSemanticAnalysis{
					Summary: "Updated by " + promptText,
					Examples: []CP1RevisionModelExample{{
						PriorID: prior.Examples[0].ID, Scenario: "S", InputSummary: "I", ExpectedOutcome: "O", VerificationKind: "deterministic",
					}},
					RemovedExampleIDs: []string{},
				}, nil
			},
		},
	})

	seed, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg-seed", Prompt: "seed"})
	require.NoError(t, err)

	for _, tc := range []struct {
		prompt      string
		wantSummary string
	}{
		{
			prompt:      "custom_rejection",
			wantSummary: "CP1 analysis rejected (this request did not save a Change Card); retry propose_change.",
		},
		{
			prompt:      "unprocessable",
			wantSummary: "CP1 analysis rejected the request as unprocessable (this request did not save a Change Card); revise the request inputs and retry propose_change.",
		},
		{
			prompt:      "canceled",
			wantSummary: "CP1 analysis was canceled before completion (this request did not save a Change Card); retry propose_change if still needed.",
		},
		{
			prompt:      "deadline",
			wantSummary: "CP1 analysis timed out before completion (this request did not save a Change Card); retry propose_change or ask the server operator to check model responsiveness.",
		},
		{
			prompt:      "mixed_join",
			wantSummary: "CP1 analysis failed before completion (this request did not save a Change Card); ask the server operator to check server diagnostics and model configuration before retrying.",
		},
	} {
		t.Run(tc.prompt, func(t *testing.T) {
			_, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
				ChangeID: "chg-" + tc.prompt,
				Prompt:   tc.prompt,
			})
			require.Error(t, err)
			sp := lastSpanByName(t, exp.GetSpans(), "surla.controlplane.propose_change")
			_, hasRej := findSpanAttr(sp.Attributes, "surla.cp1.rejection_kind")
			assert.False(t, hasRej)
			got, ok := findSpanAttr(sp.Attributes, "surla.public_summary")
			require.True(t, ok)
			assert.Equal(t, tc.wantSummary, got.AsString())
			assert.NotContains(t, got.AsString(), "private-")
		})
	}

	// Concurrent ReviseChange conflict: Request B updates chg-seed while Request A
	// is in flight, so Request A hits request_revision conflict and its span
	// summary claims only that Request A did not modify the Change Card.
	revErrCh := make(chan error, 1)
	go func() {
		_, rErr := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
			ChangeID: "chg-seed", RequestRevision: seed.RequestRevision, Prompt: "hold",
		})
		revErrCh <- rErr
	}()
	<-startedA

	revB, err := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID: "chg-seed", RequestRevision: seed.RequestRevision, Prompt: "fast",
	})
	require.NoError(t, err)
	close(releaseA)

	errA := <-revErrCh
	require.Error(t, errA)
	conflictSpan := lastSpanByName(t, exp.GetSpans(), "surla.controlplane.revise_change")
	conflictSummary, ok := findSpanAttr(conflictSpan.Attributes, "surla.public_summary")
	require.True(t, ok)
	assert.Equal(
		t,
		"Change Card request_revision changed concurrently (this request did not modify the Change Card); call get_change and retry revise_change.",
		conflictSummary.AsString(),
	)

	saved, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg-seed"})
	require.NoError(t, err)
	assert.Equal(t, revB.RequestRevision, saved.RequestRevision)

	// Closed OutputValidationKind categories produce truthful category-specific
	// summaries rather than blaming structured output validation for tool,
	// shape/bounds, or protocol/usage errors.
	for _, rc := range []struct {
		kind        string
		wantSummary string
	}{
		{
			kind:        string(model.OutputValidationStructuredOutput),
			wantSummary: "CP1 analysis rejected: model output failed structured validation (this request did not save a Change Card); retry propose_change.",
		},
		{
			kind:        string(model.OutputValidationToolIdentity),
			wantSummary: "CP1 analysis rejected: model design-inspection tool call was invalid (this request did not save a Change Card); retry propose_change.",
		},
		{
			kind:        string(model.OutputValidationToolArguments),
			wantSummary: "CP1 analysis rejected: model design-inspection tool call was invalid (this request did not save a Change Card); retry propose_change.",
		},
		{
			kind:        string(model.OutputValidationToolChoice),
			wantSummary: "CP1 analysis rejected: model design-inspection tool call was invalid (this request did not save a Change Card); retry propose_change.",
		},
		{
			kind:        string(model.OutputValidationResponseShape),
			wantSummary: "CP1 analysis rejected: model response failed response-shape validation (this request did not save a Change Card); retry propose_change.",
		},
		{
			kind:        string(model.OutputValidationOutputBounds),
			wantSummary: "CP1 analysis rejected: model response exceeded output size or structure bounds (this request did not save a Change Card); shorten the request and retry propose_change.",
		},
		{
			kind:        string(model.OutputValidationStreamProtocol),
			wantSummary: "CP1 analysis rejected: model response failed stream-protocol validation (this request did not save a Change Card); retry propose_change or ask the server operator to check server diagnostics.",
		},
		{
			kind:        string(model.OutputValidationUsage),
			wantSummary: "CP1 analysis rejected: model response failed token-usage validation (this request did not save a Change Card); retry propose_change or ask the server operator to check server diagnostics.",
		},
		{
			kind:        "unknown_future_kind",
			wantSummary: "CP1 analysis rejected (this request did not save a Change Card); retry propose_change.",
		},
	} {
		t.Run("rejection_summary/"+rc.kind, func(t *testing.T) {
			assert.Equal(t, rc.wantSummary, cp1SafeRejectionSummary(false, rc.kind, true))
		})
	}
}

// lastSpanByName returns the most recent span with name from spans.
func lastSpanByName(t *testing.T, spans []tracetest.SpanStub, name string) tracetest.SpanStub {
	t.Helper()
	for i := len(spans) - 1; i >= 0; i-- {
		if spans[i].Name == name {
			return spans[i]
		}
	}
	t.Fatalf("span %q not found", name)
	return tracetest.SpanStub{}
}

// findSpanAttr finds the attribute value for key in attrs.
func findSpanAttr(attrs []attribute.KeyValue, key attribute.Key) (attribute.Value, bool) {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value, true
		}
	}
	return attribute.Value{}, false
}
