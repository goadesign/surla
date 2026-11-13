// This file tests the optional ui_composition a caller sends in its analysis,
// and the tool descriptions that tell the caller what a good analysis holds.
package controlplane

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/goldenpath"
)

// compositionProgram is an OpenUI Lang program that refers to the card's
// ambiguity and examples only by 1-based position, as the catalog requires.
const compositionProgram = `root = DecisionWorkspace("Refund limit", [choice, table])
choice = AmbiguityChoiceCard(1, "Tax and shipping change who waits for a manager")
table = PolicySimulatorTable("Who waits?", 1, [row])
row = PolicySimulatorRow("$48 item + $6 shipping", "$54.00 total", ["Instant credit", "Waits for a manager"], 1)`

// mcpResultFrame is the JSON-RPC tools/call response of an MCP request.
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

// TestProposeChangeBindsTheCallerCompositionToTheRevision verifies that a
// composition in the analysis is saved byte for byte, bound to the revision it
// was sent with, and kept out of request_revision.
func TestProposeChangeBindsTheCallerCompositionToTheRevision(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	cp := newControlPlane(t, Options{})
	card, err := cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{
		ChangeID: "chg", Prompt: "Refunds up to $50 are instant.", Analysis: compositionAnalysis(&[]string{compositionProgram}[0]),
	})
	require.NoError(t, err)
	require.NotNil(t, card.UIComposition)
	assert.Equal(t, compositionProgram, card.UIComposition.Program, "the program must round-trip byte for byte")
	assert.Equal(t, card.RequestRevision, card.UIComposition.ComposedForRevision)
	assert.Equal(t, "chg-ambiguity-01", card.Ambiguities[0].ID, "ordinal 1 in the program names this ambiguity")

	plain := newControlPlane(t, Options{})
	plainCard, err := plain.ProposeChange(ctx, &gensurla.ProposeChangePayload{
		ChangeID: "chg", Prompt: "Refunds up to $50 are instant.", Analysis: compositionAnalysis(nil),
	})
	require.NoError(t, err)
	assert.Nil(t, plainCard.UIComposition, "no composition in the analysis means none on the card")
	assert.Equal(t, plainCard.RequestRevision, card.RequestRevision, "the composition must not be part of request_revision")
}

// TestProposeChangeCarriesAnyCallerCompositionVerbatim verifies that the
// server never judges a composition: an empty, blank or invalid program is
// saved as sent, and the card's questions and examples are saved with it.
func TestProposeChangeCarriesAnyCallerCompositionVerbatim(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
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
			cp := newControlPlane(t, Options{})
			card, err := cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{
				ChangeID: "chg", Prompt: "Refunds up to $50 are instant.", Analysis: compositionAnalysis(tc.program),
			})
			require.NoError(t, err, "a composition the views page cannot use must not cost the operator the analysis")
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

// TestCompositionDescribesOnlyTheRevisionItWasComposedFor verifies that a
// confirmation that changes answers drops the composition, and that reads
// never restore it.
func TestCompositionDescribesOnlyTheRevisionItWasComposedFor(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	cp := newControlPlane(t, Options{})
	proposed, err := cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{
		ChangeID: "chg", Prompt: "Refunds up to $50 are instant.", Analysis: compositionAnalysis(&[]string{compositionProgram}[0]),
	})
	require.NoError(t, err)

	first, err := cp.ConfirmIntent(ctx, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: proposed.RequestRevision,
		Answers: map[string]string{"chg-ambiguity-01": "order_total"}, Examples: proposed.Examples,
	})
	require.NoError(t, err)
	assert.Nil(t, first.UIComposition, "confirming answers creates a new revision the composition does not describe")

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

// TestConfirmationDropsDraftCompositionAtTheNewRevision verifies that an unchanged
// confirmation creates a new revision and drops the draft composition.
func TestConfirmationDropsDraftCompositionAtTheNewRevision(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	analysis := compositionAnalysis(&[]string{compositionProgram}[0])
	analysis.Ambiguities = []*gensurla.CP1AnalysisAmbiguity{}
	cp := newControlPlane(t, Options{})
	proposed, err := cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant.", Analysis: analysis})
	require.NoError(t, err)
	require.Equal(t, StatusNeedsConfirmation, proposed.Status)
	require.NotNil(t, proposed.UIComposition)

	same, err := cp.ConfirmIntent(ctx, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: proposed.RequestRevision, Answers: map[string]string{}, Examples: proposed.Examples,
	})
	require.NoError(t, err)
	assert.NotEqual(t, proposed.RequestRevision, same.RequestRevision)
	assert.Nil(t, same.UIComposition, "confirming creates a revision the draft composition does not describe")

	ex := *proposed.Examples[0]
	ex.ExpectedOutcome = "Waits for a manager"
	changed, err := cp.ConfirmIntent(ctx, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: same.RequestRevision, Answers: map[string]string{}, Examples: []*gensurla.ExampleItem{&ex},
	})
	require.NoError(t, err)
	assert.Nil(t, changed.UIComposition, "an edited example creates a revision the composition does not describe")
}

// TestProposeChangeOverMCPCarriesTheCompositionVerbatim sends propose_change
// through the real authenticated /mcp endpoint, as a coding agent does, and
// checks that the composition crosses the wire byte for byte with the
// code-assigned IDs. propose_change sends no progress notifications: there is
// no slow step to report.
func TestProposeChangeOverMCPCarriesTheCompositionVerbatim(t *testing.T) {
	cp := newControlPlane(t, Options{})
	srv := httptest.NewServer(NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{"alice-token": {ID: "alice@example.com"}})))
	defer srv.Close()

	args := proposeArgs(t, &gensurla.ProposeChangePayload{
		ChangeID: "chg", Prompt: "Refunds up to $50 are instant.", Analysis: compositionAnalysis(&[]string{compositionProgram}[0]),
	})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{` +
		`"name":"propose_change","arguments":` + string(args) + `,` +
		`"_meta":{"progressToken":"p1","io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer alice-token-guide")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "propose_change")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	final := readMCPResult(t, resp)
	assert.JSONEq(t, `1`, string(final.ID), "the final result must answer request 1")
	require.False(t, final.Result.IsError, "propose_change failed: %s", final.Result.StructuredContent)
	var card struct {
		RequestRevision string `json:"request_revision"`
		Ambiguities     []struct {
			ID string `json:"id"`
		} `json:"ambiguities"`
		Examples []struct {
			ID string `json:"id"`
		} `json:"examples"`
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
	require.Len(t, card.Examples, 1)
	assert.Equal(t, "check_01", card.Examples[0].ID)
}

// TestReportProgressWithoutARequestedTokenIsANoOp verifies reportProgress, which
// preview_architecture uses, outside an MCP request with a progress token.
func TestReportProgressWithoutARequestedTokenIsANoOp(t *testing.T) {
	assert.NoError(t, reportProgress(context.Background(), 0, 1, "unused"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.True(t, errors.Is(reportProgress(ctx, 0, 1, "unused"), context.Canceled))
}

// TestAnalysisToolsDescribeWhatAGoodAnalysisContains verifies that the
// propose_change and revise_change contracts a coding agent reads (tool
// descriptions and input schemas) carry the content rules for an analysis,
// point at the playbook and decision catalog, and that the guidance resource
// serves the decision catalog the ui_composition field names.
func TestAnalysisToolsDescribeWhatAGoodAnalysisContains(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	cp := newControlPlane(t, Options{})
	adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)
	tools, err := adapter.ToolsList(ctx, &genmcpadapter.ToolsListPayload{})
	require.NoError(t, err)
	contracts := map[string]string{}
	for _, tool := range tools.Tools {
		require.NotNil(t, tool.Description)
		contracts[tool.Name] = *tool.Description + "\n" + string(tool.InputSchema)
	}
	for _, name := range []string{"propose_change", "revise_change"} {
		contract := contracts[name]
		require.NotEmpty(t, contract, name)
		for _, rule := range []string{
			"surla://golden-path",
			"Open questions only the requester can answer",
			"never placeholder",
			"decision components",
			"1-based position",
			"cannot judge whether the analysis is complete or correct",
		} {
			assert.Contains(t, contract, rule, "%s contract", name)
		}
		assert.Contains(t, contract, `"analysis"`, "%s takes the analysis as input", name)
	}
	revise := contracts["revise_change"]
	for _, rule := range []string{"prior_id", "removed_example_ids", "exactly once", "confirmed answer stays settled", "get_change"} {
		assert.Contains(t, revise, rule)
	}

	guide, err := cp.GoldenPath(ctx)
	require.NoError(t, err)
	assert.Equal(t, goldenpath.ResourceMarkdown(), guide)
	assert.Contains(t, guide, "PolicySimulatorTable(", "the decision catalog is served with the playbook")
}

// compositionAnalysis returns a proposal analysis with one ambiguity and one
// example, carrying program as ui_composition when it is not nil.
func compositionAnalysis(program *string) *gensurla.CP1Analysis {
	return &gensurla.CP1Analysis{
		Summary:            "Refunds up to $50 are instant.",
		Ambiguities:        []*gensurla.CP1AnalysisAmbiguity{{Question: "Is the limit the item subtotal or the order total?", Options: []string{"item_subtotal", "order_total"}}},
		Examples:           []*gensurla.CP1AnalysisExample{{Scenario: "Refund with shipping", InputSummary: "$48 item + $6 shipping", ExpectedOutcome: "Depends on the limit", VerificationKind: "deterministic"}},
		PreservedBehaviors: []string{},
		UIComposition:      program,
	}
}

// readMCPResult reads the single tools/call result from resp, whether the
// server answered with a JSON body or an event stream. An event stream must
// carry nothing but that result.
func readMCPResult(t *testing.T, resp *http.Response) *mcpResultFrame {
	t.Helper()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		final := new(mcpResultFrame)
		require.NoError(t, json.Unmarshal(raw, final), string(raw))
		return final
	}
	var final *mcpResultFrame
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		require.Nil(t, final, "the stream must carry exactly one message, the result")
		final = new(mcpResultFrame)
		require.NoError(t, json.Unmarshal([]byte(data), final))
	}
	require.NoError(t, scanner.Err())
	require.NotNil(t, final, "the response must carry the tools/call result")
	return final
}
