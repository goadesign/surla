// This file tests code-authored CP1 OpenTelemetry span attributes
// (`surla.cp1.rejection_kind` and `surla.public_summary`) across `propose_change`
// and `revise_change` successes, rejections, and faults, verifying that prompts,
// model text, credentials, and host filesystem paths never enter trace summaries.
package controlplane

import (
	"context"
	"errors"
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
// surla.public_summary is strictly code-authored across success, model
// rejection, semantic validation rejection (including model-returned prior_id),
// unconfirmed storage_save faults, and unclassified external rejections.
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
			toolUseTurn("c1", "read_design", `{}`),
			jsonAnswerTurn(secretSummaryJSON),
			// 2. ReviseChange semantic_validation rejection with secret prior_id
			toolUseTurn("c2", "read_design", `{}`),
			jsonAnswerTurn(secretPriorIDReviseJSON),
			// 3. ProposeChange output_limited rejection
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
			// 4. ProposeChange non-rejection provider fault
			{
				err: errors.New("private-provider-credential-secret"),
			},
			// 5. ProposeChange valid response followed by card save storage fault
			toolUseTurn("call-save", "read_design", `{}`),
			jsonAnswerTurn(secretSummaryJSON),
			// 6. ReviseChange valid response followed by card save storage fault
			toolUseTurn("call-rev-save", "read_design", `{}`),
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
	// error retains diagnostic detail, while surla.public_summary excludes it.
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
	assert.Equal(t, "CP1 revision rejected: model output failed Change Card semantic validation (the saved Change Card was unchanged); retry revise_change.", revSummary.AsString())
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
	assert.Equal(t, "CP1 analysis rejected: provider stopped at output limit (no Change Card was saved); shorten the request and retry propose_change.", limSummary.AsString())
	assert.NotContains(t, limSummary.AsString(), "private-")

	// Non-rejection provider fault records operator recovery without leaking
	// provider error strings or setting surla.cp1.rejection_kind.
	_, err = cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-span-fault", Prompt: "private-user-prompt-secret",
	})
	require.Error(t, err)
	faultSpan := lastSpanByName(t, exp.GetSpans(), "surla.controlplane.propose_change")
	_, hasRej = findSpanAttr(faultSpan.Attributes, "surla.cp1.rejection_kind")
	assert.False(t, hasRej)
	faultSummary, ok := findSpanAttr(faultSpan.Attributes, "surla.public_summary")
	require.True(t, ok)
	assert.Contains(t, faultSummary.AsString(), "operator")
	assert.Contains(t, faultSummary.AsString(), "no Change Card was saved")
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
	assert.NotContains(t, saveSummary.AsString(), "no Change Card was saved")
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
	assert.NotContains(t, revSaveSummary.AsString(), "the saved Change Card was unchanged")
	assert.NotContains(t, revSaveSummary.AsString(), "private-")
	assert.NotContains(t, revSaveSummary.AsString(), cardsDir)

	// Bare external ServiceError rejection neither leaks its message into
	// surla.public_summary nor guesses surla.cp1.rejection_kind.
	cpCustom := newControlPlane(t, Options{
		CardsDir: t.TempDir(),
		Analyzer: &stubCP1Analyzer{
			analyzeFn: func(context.Context, string, CP1Design) (CP1SemanticAnalysis, error) {
				return CP1SemanticAnalysis{}, newAnalysisRejected("private-custom-analyzer-detail", errors.New("cause"))
			},
		},
	})
	_, err = cpCustom.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-custom", Prompt: "P",
	})
	require.Error(t, err)
	customSpan := lastSpanByName(t, exp.GetSpans(), "surla.controlplane.propose_change")
	_, hasRej = findSpanAttr(customSpan.Attributes, "surla.cp1.rejection_kind")
	assert.False(t, hasRej, "bare ServiceError without typed kind must not guess semantic_validation")
	customSummary, ok := findSpanAttr(customSpan.Attributes, "surla.public_summary")
	require.True(t, ok)
	assert.Equal(t, "CP1 analysis rejected (no Change Card was saved); retry propose_change.", customSummary.AsString())
	assert.NotContains(t, customSummary.AsString(), "private-")
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
