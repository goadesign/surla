// This file exercises persistence, access and detached verification through the shared HTTP server.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/project"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sharedServerSession drives authenticated MCP over the same HTTP handler as serve.
type sharedServerSession struct {
	// t receives assertions from the protocol driver.
	t *testing.T
	// server serves the shared authenticated HTTP handler.
	server *httptest.Server
	// cp owns the persistent stores until closeAndWait.
	cp *controlplane.ControlPlane
}

// startSharedServerSession reopens the workspace's persistent stores under one principal.
func startSharedServerSession(t *testing.T, principal string) *sharedServerSession {
	t.Helper()
	if principal == "" {
		principal = "operator@example.com"
	}
	opts, err := serveOptions(func(string) string { return "" }, nil)
	require.NoError(t, err)
	provider, err := newTraceProvider(context.Background(), io.Discard, func(string) string { return "" })
	require.NoError(t, err)
	opts.TracerProvider = provider
	cp, err := controlplane.New(opts)
	require.NoError(t, err)
	server := httptest.NewServer(controlplane.NewHTTPHandler(cp, workflowAuthenticator(map[string]controlplane.Principal{"test-token": {ID: principal}})))
	session := &sharedServerSession{t: t, server: server, cp: cp}
	t.Cleanup(func() {
		if session.server != nil {
			session.closeAndWait()
		}
		assert.NoError(t, provider.Shutdown(context.Background()))
	})
	return session
}

// call executes one MCP JSON-RPC request over authenticated HTTP.
func (s *sharedServerSession) call(id, method string, params map[string]any) map[string]any {
	return s.callWithCredential("test-token", id, method, params)
}

// callWithCredential selects one fixture actor without expanding its permissions.
func (s *sharedServerSession) callWithCredential(token, id, method string, params map[string]any) map[string]any {
	s.t.Helper()
	meta, _ := params["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["io.modelcontextprotocol/protocolVersion"] = "2026-07-28"
	meta["io.modelcontextprotocol/clientCapabilities"] = map[string]any{}
	params["_meta"] = meta
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	require.NoError(s.t, err)
	req, err := http.NewRequest(http.MethodPost, s.server.URL+"/mcp", bytes.NewReader(raw))
	require.NoError(s.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	workflowAuthorization(req, token)
	req.Header.Set("Mcp-Method", method)
	if name, ok := params["name"].(string); ok {
		req.Header.Set("Mcp-Name", name)
	}
	if uri, ok := params["uri"].(string); ok {
		req.Header.Set("Mcp-Name", uri)
	}
	resp, err := s.server.Client().Do(req)
	require.NoError(s.t, err)
	defer resp.Body.Close()
	rawResponse, err := io.ReadAll(resp.Body)
	require.NoError(s.t, err)
	require.Equal(s.t, http.StatusOK, resp.StatusCode, string(rawResponse))
	var envelope struct {
		Result map[string]any
		Error  any
	}
	require.NoError(s.t, json.Unmarshal(rawResponse, &envelope))
	require.Nil(s.t, envelope.Error)
	return envelope.Result
}

// closeAndWait closes the server and drains its owned work before releasing storage.
func (s *sharedServerSession) closeAndWait() {
	s.t.Helper()
	s.server.Close()
	require.NoError(s.t, s.cp.Close())
	s.server = nil
}

// TestSharedServerOwnershipRefinementResourcesAndLockReleaseAcrossRestarts checks
// current MCP resources, persisted refinements, private history and owner access.
func TestSharedServerOwnershipRefinementResourcesAndLockReleaseAcrossRestarts(t *testing.T) {
	isolateServeWorkspace(t)
	defaultPrincipal := "operator@example.com"

	s1 := startSharedServerSession(t, "")
	discoverRes := s1.call("1", "server/discover", map[string]any{
		"_meta": map[string]any{
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "claude-code", "version": "1.0.0"},
		},
	})
	assert.NotNil(t, discoverRes["capabilities"])
	meta, ok := discoverRes["_meta"].(map[string]any)
	require.True(t, ok)
	assert.NotNil(t, meta["io.modelcontextprotocol/serverInfo"])

	toolsRes := s1.call("2", "tools/list", map[string]any{})
	rawTools, ok := toolsRes["tools"].([]any)
	require.True(t, ok)
	for _, actor := range []struct {
		token string
		kind  controlplane.CallerKind
	}{{"test-token-guide", controlplane.CallerGuide}, {"test-token-builder", controlplane.CallerBuilder}, {"test-token-test-writer", controlplane.CallerTestWriter}} {
		catalog := s1.callWithCredential(actor.token, "2-"+string(actor.kind), "tools/list", map[string]any{})
		tools, ok := catalog["tools"].([]any)
		require.True(t, ok)
		for _, item := range tools {
			tool, ok := item.(map[string]any)
			require.True(t, ok)
			name, ok := tool["name"].(string)
			require.True(t, ok)
			assert.True(t, controlplane.OperationAllowed(actor.kind, name), name)
		}
		rawTools = append(rawTools, tools...)
	}
	toolNames := map[string]bool{}
	for _, item := range rawTools {
		toolMap, isMap := item.(map[string]any)
		require.True(t, isMap)
		name, _ := toolMap["name"].(string)
		toolNames[name] = true
		toolMeta, _ := toolMap["_meta"].(map[string]any)
		assert.NotContains(t, toolMeta, "ui", "tool %s must not ask a client to embed a page", name)
	}
	for _, wantTool := range []string{"refresh_story", "get_story_refresh", "list_changes"} {
		assert.True(t, toolNames[wantTool], "tool %s must be listed in tools/list", wantTool)
	}

	resourcesRes := s1.call("3", "resources/list", map[string]any{})
	rawResources, ok := resourcesRes["resources"].([]any)
	require.True(t, ok)
	assert.Len(t, rawResources, 1)
	// Waiting, starting builds, reading the model choice, reading a build's
	// findings, and the three team
	// operations are HTTP-only.
	assert.Len(t, toolNames, len(gensurla.MethodNames)-len(rawResources)-7)

	gpRead := s1.call("4", "resources/read", map[string]any{"uri": "surla://golden-path"})
	gpContents := requireResourceContents(t, gpRead)
	assert.Equal(t, "text/markdown", gpContents["mimeType"])
	assert.Contains(t, gpContents["text"], "# Golden Path\n")

	proposedRaw := s1.callTool("6", "propose_change", map[string]any{
		"change_id": "vip-refunds",
		"prompt":    "VIP customers get instant store credit up to $150.",
		"analysis": map[string]any{
			"summary": "Give VIP customers instant store credit up to $150.",
			"ambiguities": []map[string]any{{
				"question": "Does $150 include tax and shipping?",
				"options":  []string{"Include tax and shipping", "Subtotal only"},
			}},
			"examples": []map[string]any{{
				"scenario":          "VIP refund under limit",
				"input_summary":     "VIP customer, $120.00 order",
				"expected_outcome":  "Instant store credit",
				"verification_kind": "deterministic",
			}},
			"preserved_behaviors": []string{"Non-VIP orders over $50 still require manager approval."},
		},
	})
	proposed, err := gensurla.DecodeChangeCardResult(proposedRaw)
	require.NoError(t, err)
	assert.Equal(t, controlplane.StatusNeedsConfirmation, proposed.Status)
	require.Len(t, proposed.Ambiguities, 1)

	confirmedRaw := s1.callTool("7", "confirm_intent", map[string]any{
		"change_id":        "vip-refunds",
		"request_revision": proposed.RequestRevision,
		"answers": map[string]string{
			proposed.Ambiguities[0].ID: "Include tax and shipping",
		},
		"examples": marshalConfirmationExamples(proposed.Examples),
	})
	confirmed, err := gensurla.DecodeChangeCardResult(confirmedRaw)
	require.NoError(t, err)
	assert.Equal(t, controlplane.StatusConfirmed, confirmed.Status)
	require.NotNil(t, confirmed.ConfirmedBy)
	assert.Equal(t, defaultPrincipal, *confirmed.ConfirmedBy)

	s1.closeAndWait()

	s2 := startSharedServerSession(t, "")
	discovery := s2.call("10", "server/discover", map[string]any{
		"_meta": map[string]any{"io.modelcontextprotocol/clientInfo": map[string]any{"name": "test-host", "version": "1.0.0"}},
	})
	assert.NotNil(t, discovery["capabilities"])

	reopenedRaw := s2.callTool("12", "get_change", map[string]any{"change_id": "vip-refunds"})
	reopened, err := gensurla.DecodeChangeCardResult(reopenedRaw)
	require.NoError(t, err)
	assert.Equal(t, confirmed.RequestRevision, reopened.RequestRevision)
	require.NotNil(t, reopened.ConfirmedBy)
	assert.Equal(t, defaultPrincipal, *reopened.ConfirmedBy)

	priorID := reopened.Examples[0].ID
	revisedRaw := s2.callTool("13", "revise_change", map[string]any{
		"change_id":        "vip-refunds",
		"request_revision": reopened.RequestRevision,
		"prompt":           "VIP customers get instant store credit up to $200 including tax and shipping.",
		"analysis": map[string]any{
			"summary":     "Give VIP customers instant store credit up to $200 including tax and shipping.",
			"ambiguities": []map[string]any{},
			"examples": []map[string]any{{
				"prior_id":          priorID,
				"scenario":          "VIP refund under $200 limit",
				"input_summary":     "VIP customer, $180.00 order with tax and shipping",
				"expected_outcome":  "Instant store credit",
				"verification_kind": "deterministic",
			}},
			"removed_example_ids": []string{},
			"preserved_behaviors": []string{"Non-VIP orders over $50 still require manager approval."},
		},
	})
	revised, err := gensurla.DecodeChangeCardResult(revisedRaw)
	require.NoError(t, err)
	assert.Equal(t, controlplane.StatusConfirmed, revised.Status, "the agreed revision stays in force")
	assert.Equal(t, reopened.RequestRevision, revised.RequestRevision)
	require.NotNil(t, revised.ProposedRevision, "the revision waits beside the agreed one")

	reconfirmedRaw := s2.callTool("14", "confirm_intent", map[string]any{
		"change_id":        "vip-refunds",
		"request_revision": revised.ProposedRevision.RequestRevision,
		"answers":          map[string]string{},
		"examples":         marshalConfirmationExamples(revised.ProposedRevision.Examples),
	})
	reconfirmed, err := gensurla.DecodeChangeCardResult(reconfirmedRaw)
	require.NoError(t, err)
	assert.Equal(t, controlplane.StatusConfirmed, reconfirmed.Status)
	require.NotNil(t, reconfirmed.ConfirmedBy)
	assert.Equal(t, defaultPrincipal, *reconfirmed.ConfirmedBy)

	tracesRaw := s2.callTool("15", "list_traces", map[string]any{})
	traces, err := gensurla.DecodeTraceList(tracesRaw)
	require.NoError(t, err)
	var spanNames []string
	for _, sp := range traces.Spans {
		spanNames = append(spanNames, sp.Name)
	}
	assert.Contains(t, spanNames, "surla.controlplane.propose_change")
	assert.Contains(t, spanNames, "surla.controlplane.revise_change")
	s2.closeAndWait()

	s3 := startSharedServerSession(t, "alice@example.com")
	errText := s3.callToolError("20", "get_change", map[string]any{"change_id": "vip-refunds"})
	assert.Contains(t, errText, `principal "alice@example.com" is not the owner of change_id "vip-refunds"`)

	aliceTracesRaw := s3.callTool("21", "list_traces", map[string]any{})
	aliceTraces, err := gensurla.DecodeTraceList(aliceTracesRaw)
	require.NoError(t, err)
	for _, sp := range aliceTraces.Spans {
		assert.NotEqual(t, "surla.controlplane.propose_change", sp.Name, "alice must not see the operator's propose_change trace")
		assert.NotEqual(t, "surla.controlplane.revise_change", sp.Name, "alice must not see the operator's revise_change trace")
	}

	aliceCardRaw := s3.callTool("22", "propose_change", map[string]any{
		"change_id": "alice-only",
		"prompt":    "Add receipt export.",
		"analysis": map[string]any{
			"summary":     "Export receipts as JSON.",
			"ambiguities": []map[string]any{},
			"examples": []map[string]any{{
				"scenario":          "Export receipt",
				"input_summary":     "Completed order",
				"expected_outcome":  "Receipt JSON returned",
				"verification_kind": "deterministic",
			}},
			"preserved_behaviors": []string{},
		},
	})
	aliceCard, err := gensurla.DecodeChangeCardResult(aliceCardRaw)
	require.NoError(t, err)
	assert.Nil(t, aliceCard.ConfirmedBy)
	var wire map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(aliceCardRaw, &wire))
	aliceConfirmedRaw := s3.callTool("22-confirm", "confirm_intent", map[string]any{"change_id": aliceCard.ChangeID, "request_revision": aliceCard.RequestRevision, "answers": map[string]string{}, "examples": wire["examples"]})
	aliceCard, err = gensurla.DecodeChangeCardResult(aliceConfirmedRaw)
	require.NoError(t, err)
	require.NotNil(t, aliceCard.ConfirmedBy)
	assert.Equal(t, "alice@example.com", *aliceCard.ConfirmedBy)
	s3.closeAndWait()

	s4 := startSharedServerSession(t, "")
	errText = s4.callToolError("30", "get_change", map[string]any{"change_id": "alice-only"})
	assert.Contains(t, errText, `is not the owner of change_id "alice-only"`)

	stillOwnedRaw := s4.callTool("31", "get_change", map[string]any{"change_id": "vip-refunds"})
	stillOwned, err := gensurla.DecodeChangeCardResult(stillOwnedRaw)
	require.NoError(t, err)
	assert.Equal(t, reconfirmed.RequestRevision, stillOwned.RequestRevision)
	s4.closeAndWait()
}

// requireResourceContents extracts the one resource record from a resources/read result.
func requireResourceContents(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	contents, ok := res["contents"].([]any)
	require.True(t, ok)
	require.Len(t, contents, 1)
	item, ok := contents[0].(map[string]any)
	require.True(t, ok)
	return item
}

// marshalConfirmationExamples converts decoded ExampleItem structs into the
// snake_case JSON map shape expected by `confirm_intent`.
func marshalConfirmationExamples(examples []*gensurla.ExampleItem) []map[string]any {
	out := make([]map[string]any, 0, len(examples))
	for _, ex := range examples {
		out = append(out, map[string]any{
			"id":                ex.ID,
			"scenario":          ex.Scenario,
			"input_summary":     ex.InputSummary,
			"expected_outcome":  ex.ExpectedOutcome,
			"verification_kind": ex.VerificationKind,
		})
	}
	return out
}

// callTool invokes a tool and returns its successful structured content.
func (s *sharedServerSession) callTool(id, name string, arguments any) json.RawMessage {
	s.t.Helper()
	res := s.call(id, "tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	isErr, _ := res["isError"].(bool)
	require.False(s.t, isErr, "tool %s failed: %+v", name, res["content"])
	raw, err := json.Marshal(res["structuredContent"])
	require.NoError(s.t, err)
	return raw
}

// callToolError invokes `tools/call` for name, requires `isError: true`, and
// returns the concatenated text content.
func (s *sharedServerSession) callToolError(id, name string, arguments any) string {
	s.t.Helper()
	res := s.call(id, "tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	isErr, _ := res["isError"].(bool)
	require.True(s.t, isErr, "expected tool %s to return isError=true, got: %+v", name, res)
	items, ok := res["content"].([]any)
	require.True(s.t, ok)
	var parts []string
	for _, item := range items {
		if m, isMap := item.(map[string]any); isMap {
			if txt, isStr := m["text"].(string); isStr {
				parts = append(parts, txt)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// TestSharedServerSignalExitAndDetachedVerificationSurvival stops an actual server
// while detached verification continues, then reads its result after restart.
func TestSharedServerSignalExitAndDetachedVerificationSurvival(t *testing.T) {
	wsDir := isolateServeWorkspace(t)
	cfgDir := os.Getenv("SURLA_TEST_USER_CONFIG_DIR")
	require.NotEmpty(t, cfgDir)
	defaultPrincipal := fmt.Sprintf("uid:%d", os.Geteuid())

	opsDir := filepath.Join(t.TempDir(), "ops")
	runsDir := filepath.Join(opsDir, "runs")
	reportsDir := filepath.Join(opsDir, "reports")
	require.NoError(t, os.MkdirAll(runsDir, 0o700))
	require.NoError(t, os.MkdirAll(reportsDir, 0o700))
	suiteCommit := strings.Repeat("b", 40)
	candCommit := strings.Repeat("c", 40)
	verPath := filepath.Join(opsDir, "verification.json")
	verJSON := `{"candidate_repo":"` + wsDir + `","candidate_module":".","binaries":{"shop":"./cmd/shop"},` +
		`"suite_repo":"` + wsDir + `","suite_commit":"` + suiteCommit + `",` +
		`"suite_module":"eval","suite_command":"./cmd/shop-evals","timeout":"5m"}`
	require.NoError(t, os.WriteFile(verPath, []byte(verJSON), 0o600))
	require.NoError(t, runProjectSettings("verification", []string{"set", verPath}, io.Discard))
	workspace, err := configureServeWorkspace()
	require.NoError(t, err)
	runsDir = workspace.runsDir
	reportsDir = workspace.verification.Plan.ReportDir

	// Start a detached verification supervisor in runsDir that waits for a release
	// file before writing its terminal state, proving it survives the shared server exiting.
	releaseFile := filepath.Join(opsDir, "finish-run")
	supervisorScript := filepath.Join(opsDir, "fake-supervisor.sh")
	scriptBody := "#!/bin/sh\n" +
		"RUN_DIR=\"$3\"\n" +
		"attempts=0\nwhile [ ! -f \"" + releaseFile + "\" ]; do attempts=$((attempts + 1)); if [ \"$attempts\" -ge 1200 ]; then exit 1; fi; /bin/sleep 0.05; done\n" +
		"printf '{\"kind\":\"no_report\",\"failure\":\"execution_failed\",\"stage\":\"build\",\"summary\":\"synthetic check finished\"}' > \"$RUN_DIR/result.json\"\n"
	require.NoError(t, os.WriteFile(supervisorScript, []byte(scriptBody), 0o700))

	runID := strings.Repeat("1", 32)
	reqRevision := strings.Repeat("a", 64)
	supPID, err := verifyrun.Start(runsDir, verifyrun.Request{
		RunID:      runID,
		ChangeID:   "vip-refunds",
		Owner:      defaultPrincipal,
		AcceptedAt: time.Now().UTC(),
		Timeout:    2 * time.Minute,
		Plan: verify.Plan{
			WorkDir:         filepath.Join(opsDir, "work-1"),
			CandidateRepo:   wsDir,
			CandidateCommit: candCommit,
			CandidateModule: ".",
			Binaries:        map[string]string{"shop": "./cmd/shop"},
			SuiteRepo:       wsDir,
			SuiteCommit:     suiteCommit,
			SuiteModule:     "eval",
			SuiteCommand:    "./cmd/shop-evals",
			RequestRevision: reqRevision,
			ReportDir:       reportsDir,
			Tests:           verify.TestsEvaluator,
		},
	}, []string{supervisorScript})
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, os.WriteFile(releaseFile, []byte("done"), 0o600))
	})

	first := startSharedServerProcess(t, wsDir, cfgDir)
	running := readSharedVerification(t, first.url, runID, first.token)
	_, ok := running.State.AsRunning()
	assert.True(t, ok)
	first.stop()
	require.NoError(t, syscall.Kill(supPID, 0), "detached supervisor must survive shared server exit")
	require.NoError(t, os.WriteFile(releaseFile, []byte("done"), 0o600))
	require.Eventually(t, func() bool {
		_, state, err := verifyrun.Read(runsDir, runID)
		return err == nil && state.Kind == verifyrun.StateNotCompleted
	}, 5*time.Second, 50*time.Millisecond)
	second := startSharedServerProcess(t, wsDir, cfgDir)
	finished := readSharedVerification(t, second.url, runID, second.token)
	result, ok := finished.State.AsNotCompleted()
	require.True(t, ok)
	assert.Contains(t, result.Summary, "synthetic check finished")
	second.stop()
}

// sharedServerProcess holds one actual serve subprocess until its signal-driven exit.
type sharedServerProcess struct {
	// t receives lifecycle assertions.
	t *testing.T
	// cmd runs the actual shared server entry point.
	cmd *exec.Cmd
	// url is the bound HTTP origin reported by the child.
	url string
	// done reports process exit exactly once.
	done chan error
	// stopped prevents duplicate cleanup after the process was reaped.
	stopped bool
	token   string
}

// startSharedServerProcess waits for the shared server's bound address or a bounded setup failure.
func startSharedServerProcess(t *testing.T, workspace, config string) *sharedServerProcess {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	root, err := testProjectRuntimeAt(config).operatorRoot(workspace)
	require.NoError(t, err)
	state, err := project.Discover(root, workspace)
	require.NoError(t, err)
	lock, err := state.StartLock(t.Context())
	require.NoError(t, err)
	_, err = state.Initialize(fmt.Sprintf("uid:%d", os.Geteuid()))
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	server, err := project.NewServer(state, func() {}, fixtureTeamDependencies(state))
	require.NoError(t, err)
	team, err := server.StartAgents(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, team.Roles)
	raw, err := os.ReadFile(state.AgentFile(team.Roles[0].Handle))
	require.NoError(t, err)
	cmd := exec.Command(exe, "serve", "-state", state.Dir)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "SURLA_TEST_USER_CONFIG_DIR="+config)
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	p := &sharedServerProcess{t: t, cmd: cmd, done: make(chan error, 1), token: string(raw)}
	t.Cleanup(func() {
		if !p.stopped {
			assert.NoError(t, cmd.Process.Kill())
			<-p.done
		}
	})
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "surla: serving on ") {
				ready <- strings.TrimPrefix(scanner.Text(), "surla: serving on ")
			}
		}
	}()
	go func() { p.done <- cmd.Wait() }()
	select {
	case p.url = <-ready:
	case err := <-p.done:
		p.stopped = true
		t.Fatalf("server exited before ready: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("server did not become ready")
	}
	return p
}

// stop requires a clean SIGTERM exit, including all server-owned cleanup.
func (p *sharedServerProcess) stop() {
	p.t.Helper()
	require.NoError(p.t, p.cmd.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-p.done:
		p.stopped = true
		require.NoError(p.t, err)
	case <-time.After(10 * time.Second):
		p.t.Fatal("shared server did not stop")
	}
}

// readSharedVerification reads a persisted run through the public authenticated HTTP endpoint.
func readSharedVerification(t *testing.T, origin, id, token string) *gensurla.VerificationStatusResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, origin+"/verifications/"+id, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	result, err := gensurla.DecodeVerificationStatusResult(raw)
	require.NoError(t, err)
	return result
}
