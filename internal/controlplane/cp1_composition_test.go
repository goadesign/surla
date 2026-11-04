package controlplane

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa-ai/runtime/agent/model"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/goldenpath"
)

// compositionProgram is an OpenUI Lang program that refers to the card's
// ambiguity and examples only by 1-based position, as the prompt requires.
const compositionProgram = `root = DecisionWorkspace("Refund limit", [choice, table])
choice = AmbiguityChoiceCard(1, "Tax and shipping change who waits for a manager")
table = PolicySimulatorTable("Who waits?", 1, [row])
row = PolicySimulatorRow("$48 item + $6 shipping", "$54.00 total", ["Instant credit", "Waits for a manager"], 1)`

// mcpResultFrame is the final JSON-RPC tools/call response frame of an MCP
// event stream.
type mcpResultFrame struct {
	// ID is the JSON-RPC request ID the frame answers.
	ID json.RawMessage `json:"id"`
	// Result is the tool call outcome.
	Result struct {
		// IsError reports a tool error.
		IsError bool `json:"isError"`
		// StructuredContent is the encoded tool result.
		StructuredContent json.RawMessage `json:"structuredContent"`
	} `json:"result"`
}

// recordingCP1Provider is a model.Provider that records the CP1 request it
// receives and answers with a fixed analysis.
type recordingCP1Provider struct {
	// analysis is the JSON returned as assistant text.
	analysis string
	// mu guards request.
	mu sync.Mutex
	// request is the last request the analyzer sent.
	request *model.Request
}

func TestProposeChangeBindsTheModelCompositionToTheRevisionItAnalyzed(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	provider := &recordingCP1Provider{analysis: compositionAnalysis(t, &[]string{compositionProgram}[0])}
	cp := newCompositionControlPlane(t, provider)

	card, err := cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant."})
	require.NoError(t, err)

	req := provider.lastRequest(t)
	require.NotEmpty(t, req.Messages)
	system := req.Messages[0].Parts[0].(model.TextPart).Text
	assert.Equal(t, goldenpath.ArchitectSystemPrompt(), system, "the model must see the composition instruction and catalog")
	assert.Contains(t, system, "1-based position")
	assert.Contains(t, string(req.StructuredOutput.Schema), `"ui_composition"`)

	require.NotNil(t, card.UIComposition)
	assert.Equal(t, compositionProgram, card.UIComposition.Program, "the program must round-trip byte for byte")
	assert.Equal(t, card.RequestRevision, card.UIComposition.ComposedForRevision)
	assert.Equal(t, "chg-ambiguity-01", card.Ambiguities[0].ID, "ordinal 1 in the program names this ambiguity")

	plain := newCompositionControlPlane(t, &recordingCP1Provider{analysis: compositionAnalysis(t, nil)})
	plainCard, err := plain.ProposeChange(ctx, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant."})
	require.NoError(t, err)
	assert.Nil(t, plainCard.UIComposition, "no composition from the model means none on the card")
	assert.Equal(t, plainCard.RequestRevision, card.RequestRevision, "the composition must not be part of request_revision")
}

func TestProposeChangeCarriesAnyModelCompositionVerbatim(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	cases := []struct {
		name    string
		program *string
	}{
		{name: "absent", program: nil},
		{name: "empty", program: &[]string{""}[0]},
		{name: "whitespace only", program: &[]string{" \n\t"}[0]},
		{name: "not a valid program", program: &[]string{"root = Nonsense("}[0]},
		{name: "catalog program", program: &[]string{compositionProgram}[0]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := newCompositionControlPlane(t, &recordingCP1Provider{analysis: compositionAnalysis(t, tc.program)})
			card, err := cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant."})
			require.NoError(t, err, "a composition the viewer cannot use must not cost the operator the analysis")
			require.Len(t, card.Ambiguities, 1, "the authoritative questions survive")
			require.Len(t, card.Examples, 1, "the authoritative examples survive")
			if tc.program == nil {
				assert.Nil(t, card.UIComposition)
				return
			}
			require.NotNil(t, card.UIComposition)
			assert.Equal(t, *tc.program, card.UIComposition.Program, "the server carries the program byte for byte and never judges it")
			assert.Equal(t, card.RequestRevision, card.UIComposition.ComposedForRevision)
		})
	}
}

func TestCompositionDescribesOnlyTheRevisionItWasComposedFor(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	cp := newCompositionControlPlane(t, &recordingCP1Provider{analysis: compositionAnalysis(t, &[]string{compositionProgram}[0])})
	proposed, err := cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant."})
	require.NoError(t, err)

	first, err := cp.ConfirmIntent(ctx, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: proposed.RequestRevision,
		Answers: map[string]string{"chg-ambiguity-01": "order_total"}, Examples: proposed.Examples,
	})
	require.NoError(t, err)
	assert.Nil(t, first.UIComposition, "confirming answers creates a new revision the composition did not analyze")

	read, err := cp.GetChange(ctx, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Nil(t, read.UIComposition, "get_change must not restore the earlier revision's composition")

	noop, err := cp.ConfirmIntent(ctx, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: first.RequestRevision,
		Answers: map[string]string{"chg-ambiguity-01": "order_total"}, Examples: first.Examples,
	})
	require.NoError(t, err)
	assert.Equal(t, first.RequestRevision, noop.RequestRevision)
	assert.Nil(t, noop.UIComposition)
}

func TestCompositionSurvivesAnUnchangedResubmission(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	unambiguous := compositionAnalysis(t, &[]string{compositionProgram}[0])
	var analysis CP1SemanticAnalysis
	require.NoError(t, json.Unmarshal([]byte(unambiguous), &analysis))
	analysis.Ambiguities = []CP1ModelAmbiguity{}
	raw, err := json.Marshal(analysis)
	require.NoError(t, err)
	cp := newCompositionControlPlane(t, &recordingCP1Provider{analysis: string(raw)})
	proposed, err := cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant."})
	require.NoError(t, err)
	require.Equal(t, StatusConfirmed, proposed.Status)
	require.NotNil(t, proposed.UIComposition)

	same, err := cp.ConfirmIntent(ctx, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: proposed.RequestRevision, Answers: map[string]string{}, Examples: proposed.Examples,
	})
	require.NoError(t, err)
	assert.Equal(t, proposed.RequestRevision, same.RequestRevision)
	require.NotNil(t, same.UIComposition, "an unchanged card keeps the composition composed for its revision")
	assert.Equal(t, proposed.RequestRevision, same.UIComposition.ComposedForRevision)

	ex := *proposed.Examples[0]
	ex.ExpectedOutcome = "Waits for a manager"
	changed, err := cp.ConfirmIntent(ctx, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: proposed.RequestRevision, Answers: map[string]string{}, Examples: []*gensurla.ExampleItem{&ex},
	})
	require.NoError(t, err)
	assert.Nil(t, changed.UIComposition, "an edited example creates a revision the composition did not analyze")
}

func TestProposeChangeReportsProgressOverMCP(t *testing.T) {
	cp := newCompositionControlPlane(t, &recordingCP1Provider{analysis: compositionAnalysis(t, &[]string{compositionProgram}[0])})
	srv := httptest.NewServer(NewHTTPHandler(cp, NewTokenAuthenticator(map[string]Principal{"alice-token": {ID: "alice@example.com"}})))
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{` +
		`"name":"propose_change","arguments":{"change_id":"chg","prompt":"Refunds up to $50 are instant."},` +
		`"_meta":{"progressToken":"p1","io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer alice-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "propose_change")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.True(t, strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream"), "Content-Type %q", resp.Header.Get("Content-Type"))

	var messages []string
	var progress []map[string]any
	var final *mcpResultFrame
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var msg struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		require.NoError(t, json.Unmarshal([]byte(data), &msg))
		if msg.Method == "notifications/progress" {
			require.Nil(t, final, "progress must arrive before the final result")
			progress = append(progress, msg.Params)
			messages = append(messages, msg.Params["message"].(string))
			continue
		}
		require.Nil(t, final, "the stream must carry exactly one final result")
		final = new(mcpResultFrame)
		require.NoError(t, json.Unmarshal([]byte(data), final))
	}
	require.NoError(t, scanner.Err())
	assert.Equal(t, []string{"Analyzing the feature request with the CP1 model", "Validating the CP1 analysis"}, messages)
	for _, p := range progress {
		assert.Equal(t, "p1", p["progressToken"])
		assert.EqualValues(t, 1, p["total"])
	}

	require.NotNil(t, final, "the stream must end with the tools/call result")
	assert.JSONEq(t, `1`, string(final.ID), "the final result must answer request 1")
	require.False(t, final.Result.IsError, "propose_change failed: %s", final.Result.StructuredContent)
	var card struct {
		RequestRevision string `json:"request_revision"`
		Ambiguities     []struct {
			ID string `json:"id"`
		} `json:"ambiguities"`
		UIComposition struct {
			Program             string `json:"program"`
			ComposedForRevision string `json:"composed_for_revision"`
		} `json:"ui_composition"`
	}
	require.NoError(t, json.Unmarshal(final.Result.StructuredContent, &card))
	assert.Equal(t, compositionProgram, card.UIComposition.Program, "the program must cross the MCP wire byte for byte")
	assert.Equal(t, card.RequestRevision, card.UIComposition.ComposedForRevision)
	assert.NotEmpty(t, card.RequestRevision)
	require.Len(t, card.Ambiguities, 1)
	assert.Equal(t, "chg-ambiguity-01", card.Ambiguities[0].ID)
}

func TestReportProgressWithoutARequestedTokenIsANoOp(t *testing.T) {
	assert.NoError(t, reportProgress(context.Background(), 0, 1, "unused"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.True(t, errors.Is(reportProgress(ctx, 0, 1, "unused"), context.Canceled))
}

// TestCP1ModelContractDescribesQuestionsAndExamples verifies the CP1 model
// contract for the card's open questions and examples: every ambiguity and
// example field the model fills has a description in the structured-output
// schema, and both the proposal and the revision prompt carry the rules that
// span fields. It checks the contract the model receives, not how a model
// behaves with it.
func TestCP1ModelContractDescribesQuestionsAndExamples(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
			Items       struct {
				Properties map[string]struct {
					Description string `json:"description"`
				} `json:"properties"`
			} `json:"items"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(cp1AnalysisJSONSchema, &schema))
	for field, itemFields := range map[string][]string{
		"ambiguities": {"question", "options"},
		"examples":    {"scenario", "input_summary", "expected_outcome", "verification_kind"},
	} {
		prop, ok := schema.Properties[field]
		require.True(t, ok, "schema declares %s", field)
		assert.NotEmpty(t, strings.TrimSpace(prop.Description), "%s has a description", field)
		for _, name := range itemFields {
			assert.NotEmpty(t, strings.TrimSpace(prop.Items.Properties[name].Description), "%s.%s has a description", field, name)
		}
	}
	for name, prompt := range map[string]string{
		"proposal": goldenpath.ArchitectSystemPrompt(),
		"revision": goldenpath.ArchitectReviseSystemPrompt(),
	} {
		assert.Contains(t, prompt, "Write the card for the requester", "%s prompt", name)
	}
}

// Complete records req and returns the fixed analysis as assistant text.
func (p *recordingCP1Provider) Complete(_ context.Context, req *model.Request) (*model.Response, error) {
	p.mu.Lock()
	p.request = req
	p.mu.Unlock()
	return &model.Response{
		Content:    []model.Message{{Role: model.ConversationRoleAssistant, Parts: []model.Part{model.TextPart{Text: p.analysis}}}},
		StopReason: "stop",
	}, nil
}

// Stream is unused because the CP1 analyzer only calls Complete.
func (p *recordingCP1Provider) Stream(context.Context, *model.Request) (model.Streamer, error) {
	return nil, errors.New("streaming not used by CP1 analyzer")
}

// lastRequest returns the request the analyzer last sent.
func (p *recordingCP1Provider) lastRequest(t *testing.T) *model.Request {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	require.NotNil(t, p.request, "the analyzer sent no request")
	return p.request
}

// compositionAnalysis returns a CP1 analysis with one ambiguity and one
// example, carrying program as ui_composition when it is not nil.
func compositionAnalysis(t *testing.T, program *string) string {
	t.Helper()
	raw, err := json.Marshal(CP1SemanticAnalysis{
		Summary:            "Refunds up to $50 are instant.",
		Ambiguities:        []CP1ModelAmbiguity{{Question: "Is the limit the item subtotal or the order total?", Options: []string{"item_subtotal", "order_total"}}},
		Examples:           []CP1ModelExample{{Scenario: "Refund with shipping", InputSummary: "$48 item + $6 shipping", ExpectedOutcome: "Depends on the limit", VerificationKind: "deterministic"}},
		PreservedBehaviors: []string{},
		UIComposition:      program,
	})
	require.NoError(t, err)
	return string(raw)
}

// newCompositionControlPlane returns a ControlPlane whose CP1 analyzer calls
// provider through a goa-ai model client.
func newCompositionControlPlane(t *testing.T, provider model.Provider) *ControlPlane {
	t.Helper()
	client, err := model.NewClient(provider)
	require.NoError(t, err)
	return newControlPlane(t, Options{Analyzer: NewModelCP1Analyzer(client)})
}
