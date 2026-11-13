// This file tests the change list, its waiting read, and the saved time the
// card store keeps outside request_revision.
package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
)

// TestListChangesOrdersOwnersCardsBySavedTime lists only the owner's cards,
// most recently saved first, with each card's state and latest run.
func TestListChangesOrdersOwnersCardsBySavedTime(t *testing.T) {
	runsDir := t.TempDir()
	cp := newControlPlane(t, Options{RunsDir: runsDir})
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "guide", Kind: CallerGuide})
	bob := WithPrincipal(context.Background(), Principal{ID: "bob@example.com", CallerID: "guide", Kind: CallerGuide})

	empty, err := cp.ListChanges(alice)
	require.NoError(t, err)
	assert.Empty(t, empty.Changes, "a project without cards lists none")
	assert.NotEmpty(t, empty.Version)

	first := mustPropose(t, cp, alice, "chg-a", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
	mustPropose(t, cp, bob, "chg-bob", "Bob's change.", refundAnalysis("Bob's change."))
	second := mustPropose(t, cp, alice, "chg-b", "Refunds over $50 wait for a manager.", refundAnalysis("Refunds over $50 wait for a manager."))
	writeListedRun(t, runsDir, "alice@example.com", second, "11111111111111111111111111111111", time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), true)
	writeListedRun(t, runsDir, "alice@example.com", second, "22222222222222222222222222222222", time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC), false)

	list, err := cp.ListChanges(alice)
	require.NoError(t, err)
	require.Len(t, list.Changes, 2, "another owner's cards are not listed")
	assert.Equal(t, "chg-b", list.Changes[0].ChangeID, "most recently saved first")
	assert.Equal(t, "chg-a", list.Changes[1].ChangeID)
	assert.Equal(t, second.Summary, list.Changes[0].Summary)
	assert.Equal(t, StatusNeedsConfirmation, list.Changes[0].Status)
	assert.Equal(t, second.RequestRevision, list.Changes[0].RequestRevision)
	require.NotNil(t, list.Changes[0].LatestVerification, "the newest run is reported")
	assert.Equal(t, "22222222222222222222222222222222", list.Changes[0].LatestVerification.RunID)
	assert.Equal(t, "failed", list.Changes[0].LatestVerification.State)
	assert.Nil(t, list.Changes[1].LatestVerification, "a card without runs reports none")
	assert.NotEqual(t, empty.Version, list.Version)

	user := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "user-1", Kind: CallerUser})
	_, err = cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
		ChangeID: first.ChangeID, RequestRevision: first.RequestRevision,
		Answers: map[string]string{}, Examples: first.Examples,
	})
	require.NoError(t, err)
	relisted, err := cp.ListChanges(alice)
	require.NoError(t, err)
	assert.Equal(t, "chg-a", relisted.Changes[0].ChangeID, "saving a card moves it first")
	assert.Equal(t, StatusConfirmed, relisted.Changes[0].Status)
	assert.NotEqual(t, list.Version, relisted.Version, "a save changes the version")
	again, err := cp.ListChanges(alice)
	require.NoError(t, err)
	assert.Equal(t, relisted.Version, again.Version, "an unchanged list keeps its version")
}

// TestListChangesRecordsWhyARunCouldNotBeRead damages one saved run and
// checks that the list fails with a message naming only the kind of record
// that could not be read, while the span keeps the cause.
func TestListChangesRecordsWhyARunCouldNotBeRead(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	runsDir := t.TempDir()
	cp := newControlPlane(t, Options{TracerProvider: provider, RunsDir: runsDir})
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "guide", Kind: CallerGuide})
	mustPropose(t, cp, alice, "chg-a", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
	damaged := filepath.Join(runsDir, "33333333333333333333333333333333")
	require.NoError(t, os.MkdirAll(damaged, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(damaged, "request.json"), []byte(`{"secret_host_path":"/var/lib/secret`), 0o600))

	_, err := cp.ListChanges(alice)
	require.Error(t, err)
	assert.Equal(t, "list changes: saved verification records could not be read", err.Error())
	var listed []sdktrace.ReadOnlySpan
	for _, span := range exporter.GetSpans().Snapshots() {
		if span.Name() == "surla.controlplane.list_changes" {
			listed = append(listed, span)
		}
	}
	require.Len(t, listed, 1)
	assert.Equal(t, codes.Error, listed[0].Status().Code)
	require.Len(t, listed[0].Events(), 1)
	var cause string
	for _, attr := range listed[0].Events()[0].Attributes {
		if attr.Key == "exception.message" {
			cause = attr.Value.AsString()
		}
	}
	assert.Contains(t, cause, "decode run request", "the span keeps why the record could not be read")
}

// TestSavedTimeIsOutsideTheRevision saves identical content twice and checks
// that the revision stays while the saved time moves, and that a card file
// without a saved time is refused.
func TestSavedTimeIsOutsideTheRevision(t *testing.T) {
	cp := newControlPlane(t, Options{})
	card := saveCard(t, cp, nil)
	first, ok, err := cp.cards.load("chg")
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, first.savedAt.IsZero())
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, cp.cards.save(first))
	second, _, err := cp.cards.load("chg")
	require.NoError(t, err)
	assert.Equal(t, card.RequestRevision, second.card.RequestRevision, "saving again keeps the revision")
	assert.True(t, second.savedAt.After(first.savedAt), "saving again moves the saved time")

	raw, err := os.ReadFile(cp.cards.path("chg"))
	require.NoError(t, err)
	var file map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &file))
	require.Contains(t, file, "saved_at", "the saved time is a field of the card file, beside the card")
	delete(file, "saved_at")
	stripped, err := json.Marshal(file)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cp.cards.path("chg"), stripped, 0o600))
	_, err = cp.GetChange(WithPrincipal(context.Background(), Principal{ID: "alice@example.com"}), &gensurla.GetChangePayload{ChangeID: "chg"})
	assert.ErrorContains(t, err, "has no saved time")
}

// TestWaitForChangesReturnsOnChangeOrAfterItsBound checks the waiting read's
// three endings: at once when the caller's version is old, when a card is
// saved during the wait, and with the same version after the bound.
func TestWaitForChangesReturnsOnChangeOrAfterItsBound(t *testing.T) {
	cp := newControlPlane(t, Options{})
	cp.changesWait = 300 * time.Millisecond
	cp.changesPoll = 10 * time.Millisecond
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "guide", Kind: CallerGuide})
	list, err := cp.ListChanges(alice)
	require.NoError(t, err)

	stale, err := cp.WaitForChanges(alice, &gensurla.WaitForChangesPayload{Since: "older"})
	require.NoError(t, err)
	assert.Equal(t, list.Version, stale.Version, "an old version answers at once with the current one")

	start := time.Now()
	unchanged, err := cp.WaitForChanges(alice, &gensurla.WaitForChangesPayload{Since: list.Version})
	require.NoError(t, err)
	assert.Equal(t, list.Version, unchanged.Version)
	assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond, "nothing changed, so the wait ran to its bound")

	cp.changesWait = 10 * time.Second
	saved := make(chan struct{})
	go func() {
		defer close(saved)
		time.Sleep(50 * time.Millisecond)
		mustPropose(t, cp, alice, "chg-new", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
	}()
	start = time.Now()
	changed, err := cp.WaitForChanges(alice, &gensurla.WaitForChangesPayload{Since: list.Version})
	require.NoError(t, err)
	<-saved
	assert.NotEqual(t, list.Version, changed.Version)
	assert.Less(t, time.Since(start), 5*time.Second, "a save ends the wait")
	current, err := cp.ListChanges(alice)
	require.NoError(t, err)
	assert.Equal(t, current.Version, changed.Version, "the wait names the list list_changes returns")

	ctx, cancel := context.WithTimeout(alice, 50*time.Millisecond)
	defer cancel()
	_, err = cp.WaitForChanges(ctx, &gensurla.WaitForChangesPayload{Since: current.Version})
	assert.ErrorIs(t, err, context.DeadlineExceeded, "a caller that goes away ends the wait")
}

// TestWaitForChangesIsTracedButNotSaved checks that the waiting read's span
// is exported while the diagnostics history keeps only other operations.
func TestWaitForChangesIsTracedButNotSaved(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	cp := newControlPlane(t, Options{TracerProvider: provider})
	cp.changesWait = 20 * time.Millisecond
	user := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "user-1", Kind: CallerUser})
	list, err := cp.ListChanges(user)
	require.NoError(t, err)
	for range 3 {
		_, err := cp.WaitForChanges(user, &gensurla.WaitForChangesPayload{Since: list.Version})
		require.NoError(t, err)
	}
	var exported []string
	for _, span := range exporter.GetSpans() {
		exported = append(exported, span.Name)
	}
	assert.Equal(t, []string{"surla.controlplane.list_changes", waitForChangesSpan, waitForChangesSpan, waitForChangesSpan}, exported)
	history, err := cp.traces.List("alice@example.com", "all", 100, nil)
	require.NoError(t, err)
	require.Len(t, history.Spans, 1, "only list_changes is saved")
	assert.Equal(t, "surla.controlplane.list_changes", history.Spans[0].Name)
}

// TestChangeListOperationsFollowTheRoleTable checks the two operations over
// HTTP: every agent role the table names may list changes, and only the
// user's browser session may wait for them.
func TestChangeListOperationsFollowTheRoleTable(t *testing.T) {
	cp := newControlPlane(t, Options{})
	cp.changesWait = 20 * time.Millisecond
	auth := fixtureCredentials(map[string]Principal{
		"user":    {ID: "alice@example.com", CallerID: "user-1", Kind: CallerUser},
		"guide":   {ID: "alice@example.com", CallerID: "guide", Kind: CallerGuide},
		"builder": {ID: "alice@example.com", CallerID: "builder", Kind: CallerBuilder},
		"tests":   {ID: "alice@example.com", CallerID: "tests", Kind: CallerTestWriter},
	})
	srv := httptest.NewServer(NewHTTPHandler(cp, auth))
	t.Cleanup(srv.Close)
	call := func(token, path string) int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return resp.StatusCode
	}
	cases := []struct {
		token      string
		list, wait int
	}{
		{"user", http.StatusOK, http.StatusOK},
		{"guide", http.StatusOK, http.StatusForbidden},
		{"builder", http.StatusOK, http.StatusForbidden},
		{"tests", http.StatusForbidden, http.StatusForbidden},
	}
	listTools := func(token string) map[string]bool {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{`+
				`"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", "tools/list")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		var body struct {
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		names := map[string]bool{}
		for _, tool := range body.Result.Tools {
			names[tool.Name] = true
		}
		return names
	}
	for _, c := range cases {
		t.Run(c.token, func(t *testing.T) {
			assert.Equal(t, c.list, call(c.token, "/changes"))
			assert.Equal(t, c.wait, call(c.token, "/change-updates?since=x"))
			tools := listTools(c.token)
			assert.Equal(t, c.list == http.StatusOK, tools["list_changes"], "list_changes is a tool exactly for the callers allowed to list")
			assert.False(t, tools["wait_for_changes"], "the waiting read is never offered as a tool")
		})
	}
	all, err := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil).ToolsList(t.Context(), &genmcpadapter.ToolsListPayload{})
	require.NoError(t, err)
	declared := map[string]bool{}
	for _, tool := range all.Tools {
		declared[tool.Name] = true
	}
	assert.True(t, declared["list_changes"])
	assert.False(t, declared["wait_for_changes"], "the waiting read is HTTP-only: no MCP tool exists for it")
	for _, kind := range []CallerKind{CallerGuide, CallerBuilder, CallerTestWriter, CallerManagement} {
		assert.False(t, OperationAllowed(kind, "wait_for_changes"), "only the user's browser session may wait: %s", kind)
	}
}

// writeListedRun publishes a finished verification run of card for owner
// under runsDir, with its report, as a verify-run supervisor leaves it.
func writeListedRun(t *testing.T, runsDir, owner string, card *gensurla.ChangeCardResult, runID string, accepted time.Time, passed bool) {
	t.Helper()
	inputs := PromotionInputs{SuiteID: "orders_acceptance", SuiteCommit: strings.Repeat("2", 40), Environment: "sim", Artifacts: map[string]string{"orders": strings.Repeat("a", 64)}}
	commit := strings.Repeat("1", 40)
	report := failingReport(t, card.RequestRevision, commit, inputs)
	if passed {
		report = passingReport(t, card.RequestRevision, commit, inputs)
	}
	reportDir := filepath.Join(runsDir, "reports")
	require.NoError(t, os.MkdirAll(reportDir, 0o700))
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(reportDir, report.Digest+".json"), raw, 0o600))
	dir := filepath.Join(runsDir, runID)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	req, err := json.Marshal(verifyrun.Request{
		RunID: runID, Owner: owner, ChangeID: card.ChangeID, AcceptedAt: accepted,
		Plan: verify.Plan{CandidateCommit: commit, RequestRevision: card.RequestRevision, SuiteCommit: inputs.SuiteCommit, ReportDir: reportDir},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "request.json"), req, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lock"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "result.json"), []byte(`{"kind":"report","report_digest":"`+report.Digest+`"}`), 0o600))
}
