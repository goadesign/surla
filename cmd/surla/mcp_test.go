package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/swarm"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
)

// mcpStdioSession drives an in-process `runMCP` call over OS pipes so tests can
// send newline-delimited JSON-RPC requests and read responses deterministically.
type mcpStdioSession struct {
	t *testing.T
	// stdin is the write end of the pipe connected to `runMCP`'s stdin.
	stdin *os.File
	// reader reads newline-delimited JSON-RPC responses from stdout.
	reader *bufio.Reader
	// stdout is the read end of the pipe connected to `runMCP`'s stdout.
	stdout *os.File
	// stderr captures startup status and diagnostic output from `runMCP`.
	stderr *bytes.Buffer
	// done receives the error returned by `runMCP` when it exits.
	done chan error
	// legacy is true after a `2025-11-25` `initialize` call on this session.
	legacy bool
}

// TestMCPSubprocessHelper runs `surla mcp` inside a child process when
// SURLA_TEST_MCP_SUBPROCESS=1 is set, exiting with the CLI status before
// `testing` prints PASS so stdout contains only protocol output.
func TestMCPSubprocessHelper(t *testing.T) {
	if os.Getenv("SURLA_TEST_MCP_SUBPROCESS") != "1" {
		return
	}
	if cfgDir := os.Getenv("SURLA_TEST_USER_CONFIG_DIR"); cfgDir != "" {
		userConfigDir = func() (string, error) { return cfgDir, nil }
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if err := run(args, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "surla: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// TestWriteMCPStatusNamesThePrincipalAndCheckpointState verifies that
// writeMCPStatus prints the stdio banner with the active principal and the
// shared CP1–CP5 checkpoint status lines without an HTTP listener or token.
func TestWriteMCPStatusNamesThePrincipalAndCheckpointState(t *testing.T) {
	var out bytes.Buffer
	writeMCPStatus(&out, "uid:501", controlplane.Options{
		CardsDir:      "/var/lib/surla/cards",
		SwarmProfiles: []swarm.ProviderProfile{{ID: "claude-a"}},
	}, "")
	want := `surla: serving MCP over stdio as principal uid:501 (architecture guide: surla://golden-path)
surla: saving Change Cards in /var/lib/surla/cards
surla: CP1 (propose_change, revise_change, confirm_intent, get_change): on
surla: CP2 (preview_architecture): on (1 swarm profile loaded; pass builders in the request to propose a roster)
surla: CP3 (draft_override, start_live_target): compiled_rule is on; prompt_template is off (set -project or -verification-config, then call start_live_target with a commit; or edit the prompt's source text, commit, and verify the commit)
surla: CP4 (verify_change, get_verification, list_verifications): off (set -project or -verification-config, or run surla verify on the CLI)
surla: CP5 (approve_merge): off (set -promotion-config, or run surla approve on the CLI)
`
	assert.Equal(t, want, out.String())
}

// TestRunMCPRejectsInvalidFlagsAndPrincipal verifies that `surla mcp` rejects
// blank or non-UTF-8 `-principal` values, stray positional arguments, and
// HTTP-only flags (`-addr`, `-auth-token`) while writing nothing to stdout.
func TestRunMCPRejectsInvalidFlagsAndPrincipal(t *testing.T) {
	isolateServeWorkspace(t)
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "empty principal flag", args: []string{"-principal", ""}, wantErr: "-principal must not be blank"},
		{name: "whitespace principal flag", args: []string{"-principal", " \t\n "}, wantErr: "-principal must not be blank"},
		{name: "invalid UTF-8 principal flag", args: []string{"-principal", string([]byte{0xff, 0xfe})}, wantErr: "-principal must be valid UTF-8"},
		{name: "unexpected positional argument", args: []string{"extra"}, wantErr: `unexpected arguments ["extra"]`},
		{name: "addr flag is not defined on mcp", args: []string{"-addr", "127.0.0.1:0"}, wantErr: "flag provided but not defined: -addr"},
		{name: "auth-token flag is not defined on mcp", args: []string{"-auth-token", "tok"}, wantErr: "flag provided but not defined: -auth-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdinR, stdinW, err := os.Pipe()
			require.NoError(t, err)
			defer stdinR.Close()
			defer stdinW.Close()
			stdoutR, stdoutW, err := os.Pipe()
			require.NoError(t, err)
			defer stdoutR.Close()
			var stderr bytes.Buffer
			err = runMCP(tc.args, stdinR, stdoutW, &stderr)
			_ = stdoutW.Close()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			outBytes, readErr := io.ReadAll(stdoutR)
			require.NoError(t, readErr)
			assert.Empty(t, outBytes, "failed startup must not write to stdout")
		})
	}
}

// TestRunMCPOwnershipRefinementResourcesAndLockReleaseAcrossRestarts verifies
// the full local stdio lifecycle across restarts:
//   - default `uid:<euid>` principal ownership on zero-flag `surla mcp`;
//   - current (`server/discover`) and legacy (`initialize` `2025-11-25`) MCP
//     handshakes, all 20 tools with `_meta.ui.resourceUri`, and `resources/list`
//   - `resources/read` for `surla://golden-path` and `ui://surla/checkpoint`;
//   - reopen and refinement (`propose_change`, `get_change`, `revise_change`,
//     `confirm_intent`, `list_traces`) across `surla mcp` restarts with
//     `<cardsDir>/lock` and `import.lock` released cleanly on stdin EOF;
//   - strict principal isolation (`forbidden` on other-owner cards and separate
//     trace histories, with no silent ownership rewrite when switching between
//     default `uid:<euid>` and explicit `-principal`).
func TestRunMCPOwnershipRefinementResourcesAndLockReleaseAcrossRestarts(t *testing.T) {
	isolateServeWorkspace(t)
	defaultPrincipal := fmt.Sprintf("uid:%d", os.Geteuid())

	// Session 1: default principal (uid:<euid>), current MCP discovery, tools/list,
	// resources/list, resources/read, propose_change, and confirm_intent.
	s1 := startMCPStdioSession(t, nil)
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
	assert.Len(t, rawTools, 20)
	uiTools := map[string]bool{}
	for _, item := range rawTools {
		toolMap, isMap := item.(map[string]any)
		require.True(t, isMap)
		name, _ := toolMap["name"].(string)
		if toolMeta, hasMeta := toolMap["_meta"].(map[string]any); hasMeta {
			if uiMeta, hasUI := toolMeta["ui"].(map[string]any); hasUI {
				if uri, _ := uiMeta["resourceUri"].(string); uri == "ui://surla/checkpoint" {
					uiTools[name] = true
				}
			}
		}
	}
	for _, wantUITool := range []string{
		"propose_change", "revise_change", "confirm_intent", "get_change",
		"preview_architecture", "draft_override", "verify_change",
		"get_verification", "list_verifications", "approve_merge",
		"diff",
	} {
		assert.True(t, uiTools[wantUITool], "tool %s must advertise ui://surla/checkpoint", wantUITool)
	}

	resourcesRes := s1.call("3", "resources/list", map[string]any{})
	rawResources, ok := resourcesRes["resources"].([]any)
	require.True(t, ok)
	assert.Len(t, rawResources, 2)

	gpRead := s1.call("4", "resources/read", map[string]any{"uri": "surla://golden-path"})
	gpContents := requireResourceContents(t, gpRead)
	assert.Equal(t, "text/markdown", gpContents["mimeType"])
	assert.Contains(t, gpContents["text"], "# Golden Path\n")

	uiRead := s1.call("5", "resources/read", map[string]any{"uri": "ui://surla/checkpoint"})
	uiContents := requireResourceContents(t, uiRead)
	assert.Equal(t, "text/html;profile=mcp-app", uiContents["mimeType"])
	assert.Contains(t, strings.ToLower(fmt.Sprint(uiContents["text"])), "<html")

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
	assert.Contains(t, s1.stderr.String(), "surla: serving MCP over stdio as principal "+defaultPrincipal)

	// Session 2: immediate restart with default principal (proving locks were
	// released), using the legacy 2025-11-25 initialize handshake, reopening
	// and refining the saved card via get_change -> revise_change -> confirm_intent,
	// and reading saved traces across both sessions via list_traces.
	s2 := startMCPStdioSession(t, nil)
	initRes := s2.call("10", "initialize", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "legacy-host", "version": "1.0.0"},
	})
	assert.Equal(t, "2025-11-25", initRes["protocolVersion"])
	s2.notify("notifications/initialized", map[string]any{})
	pingRes := s2.call("11", "ping", map[string]any{})
	assert.Empty(t, pingRes)

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
	assert.Equal(t, controlplane.StatusNeedsConfirmation, revised.Status)
	assert.NotEqual(t, reopened.RequestRevision, revised.RequestRevision)

	reconfirmedRaw := s2.callTool("14", "confirm_intent", map[string]any{
		"change_id":        "vip-refunds",
		"request_revision": revised.RequestRevision,
		"answers":          map[string]string{},
		"examples":         marshalConfirmationExamples(revised.Examples),
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

	// Session 3: explicit `-principal alice@example.com` in the same workspace
	// is forbidden from reading uid:<euid>'s card, sees only its own traces,
	// and can create its own card.
	s3 := startMCPStdioSession(t, []string{"-principal", "alice@example.com"})
	errText := s3.callToolError("20", "get_change", map[string]any{"change_id": "vip-refunds"})
	assert.Contains(t, errText, `principal "alice@example.com" is not the owner of change_id "vip-refunds"`)

	aliceTracesRaw := s3.callTool("21", "list_traces", map[string]any{})
	aliceTraces, err := gensurla.DecodeTraceList(aliceTracesRaw)
	require.NoError(t, err)
	for _, sp := range aliceTraces.Spans {
		assert.NotEqual(t, "surla.controlplane.propose_change", sp.Name, "alice must not see uid:<euid>'s propose_change trace")
		assert.NotEqual(t, "surla.controlplane.revise_change", sp.Name, "alice must not see uid:<euid>'s revise_change trace")
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
	require.NotNil(t, aliceCard.ConfirmedBy)
	assert.Equal(t, "alice@example.com", *aliceCard.ConfirmedBy)
	s3.closeAndWait()
	assert.Contains(t, s3.stderr.String(), "surla: serving MCP over stdio as principal alice@example.com")

	// Session 4: restarting without `-principal` uses default `uid:<euid>` again,
	// refuses `alice-only` as forbidden (no silent ownership rewrite), and still
	// reads `vip-refunds`.
	s4 := startMCPStdioSession(t, nil)
	errText = s4.callToolError("30", "get_change", map[string]any{"change_id": "alice-only"})
	assert.Contains(t, errText, `is not the owner of change_id "alice-only"`)

	stillOwnedRaw := s4.callTool("31", "get_change", map[string]any{"change_id": "vip-refunds"})
	stillOwned, err := gensurla.DecodeChangeCardResult(stillOwnedRaw)
	require.NoError(t, err)
	assert.Equal(t, reconfirmed.RequestRevision, stillOwned.RequestRevision)
	s4.closeAndWait()
}

// TestMCPSubprocessSignalExitAndDetachedVerificationSurvival verifies that:
//   - a child `surla mcp` process keeps stdout strictly limited to JSON-RPC
//     protocol frames while writing startup status to stderr;
//   - SIGTERM while stdin remains open stops `surla mcp` with exit code 0 and
//     releases its locks; and
//   - a detached CP4 verification supervisor continues running while `surla mcp`
//     is stopped and its completed result is readable when `surla mcp` restarts.
func TestMCPSubprocessSignalExitAndDetachedVerificationSurvival(t *testing.T) {
	wsDir := isolateServeWorkspace(t)
	cfgDir, err := userConfigDir()
	require.NoError(t, err)
	defaultPrincipal := fmt.Sprintf("uid:%d", os.Geteuid())

	opsDir := filepath.Join(t.TempDir(), "ops")
	runsDir := filepath.Join(opsDir, "runs")
	reportsDir := filepath.Join(opsDir, "reports")
	require.NoError(t, os.MkdirAll(runsDir, 0o700))
	require.NoError(t, os.MkdirAll(reportsDir, 0o700))
	suiteCommit := strings.Repeat("b", 40)
	candCommit := strings.Repeat("c", 40)
	verPath := filepath.Join(opsDir, "verification.json")
	verJSON := `{"runs_dir":"` + runsDir + `","report_dir":"` + reportsDir + `",` +
		`"candidate_repo":"` + wsDir + `","candidate_module":".","binaries":{"shop":"./cmd/shop"},` +
		`"suite_repo":"` + wsDir + `","suite_commit":"` + suiteCommit + `",` +
		`"suite_module":"eval","suite_command":"./cmd/shop-evals","timeout":"5m"}`
	require.NoError(t, os.WriteFile(verPath, []byte(verJSON), 0o600))

	// Start a detached verification supervisor in runsDir that waits for a release
	// file before writing its terminal state, proving it survives `surla mcp` exit.
	releaseFile := filepath.Join(opsDir, "finish-run")
	supervisorScript := filepath.Join(opsDir, "fake-supervisor.sh")
	scriptBody := "#!/bin/sh\n" +
		"RUN_DIR=\"$3\"\n" +
		"while [ ! -f \"" + releaseFile + "\" ]; do /bin/sleep 0.05; done\n" +
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
		},
	}, []string{supervisorScript})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = os.WriteFile(releaseFile, []byte("done"), 0o600)
	})

	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(exe, "-test.run=^TestMCPSubprocessHelper$", "--", "mcp", "-verification-config", verPath)
	cmd.Dir = wsDir
	cmd.Env = append(os.Environ(),
		"SURLA_TEST_MCP_SUBPROCESS=1",
		"SURLA_TEST_USER_CONFIG_DIR="+cfgDir,
	)
	stdinPipe, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdoutPipe, err := cmd.StdoutPipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())

	stdoutReader := bufio.NewReader(stdoutPipe)
	sendSubprocessReq := func(id, method string, params any) map[string]any {
		raw, marshalErr := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  method,
			"params":  params,
		})
		require.NoError(t, marshalErr)
		_, writeErr := stdinPipe.Write(append(raw, '\n'))
		require.NoError(t, writeErr)
		line, readErr := stdoutReader.ReadBytes('\n')
		require.NoError(t, readErr)
		var resp struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      any             `json:"id"`
			Result  json.RawMessage `json:"result"`
			Error   any             `json:"error"`
		}
		require.NoError(t, json.Unmarshal(line, &resp), "stdout line must be pure JSON-RPC: %s", string(line))
		assert.Equal(t, "2.0", resp.JSONRPC)
		require.Nil(t, resp.Error)
		var out map[string]any
		require.NoError(t, json.Unmarshal(resp.Result, &out))
		return out
	}

	disc := sendSubprocessReq("1", "server/discover", map[string]any{
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "claude-code", "version": "1.0.0"},
		},
	})
	assert.NotNil(t, disc["capabilities"])

	// Read the running verification via get_verification before stopping surla mcp.
	verCall := sendSubprocessReq("2", "tools/call", map[string]any{
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		},
		"name":      "get_verification",
		"arguments": map[string]any{"run_id": runID},
	})
	structured, err := json.Marshal(verCall["structuredContent"])
	require.NoError(t, err)
	verStatus, err := gensurla.DecodeVerificationStatusResult(structured)
	require.NoError(t, err)
	_, isRunning := verStatus.State.AsRunning()
	assert.True(t, isRunning, "verification run must be running while supervisor waits")

	// Send SIGTERM while stdin remains open; surla mcp must exit 0 and write
	// no non-JSON bytes to stdout. Read stdout to EOF before cmd.Wait closes
	// theStdoutPipe descriptor.
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	type drainResult struct {
		rest []byte
		err  error
	}
	drainDone := make(chan drainResult, 1)
	go func() {
		b, readErr := io.ReadAll(stdoutReader)
		drainDone <- drainResult{rest: b, err: readErr}
	}()
	var restStdout []byte
	select {
	case dr := <-drainDone:
		require.NoError(t, dr.err)
		restStdout = dr.rest
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("surla mcp did not close stdout within 10s of SIGTERM")
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case waitErr := <-waitDone:
		require.NoError(t, waitErr, "SIGTERM must stop surla mcp cleanly with exit status 0 (stderr: %s)", stderr.String())
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("surla mcp did not exit within 10s of SIGTERM")
	}
	_ = stdinPipe.Close()
	assert.Empty(t, restStdout, "shutdown must not write trailing text to stdout")
	assert.Contains(t, stderr.String(), "surla: serving MCP over stdio as principal "+defaultPrincipal)

	// Verify the detached supervisor process is still running after surla mcp exited.
	require.NoError(t, syscall.Kill(supPID, 0), "detached verification supervisor must survive surla mcp exit")

	// Let the detached supervisor finish, then reconnect a new surla mcp session
	// and read the finished verification state.
	require.NoError(t, os.WriteFile(releaseFile, []byte("done"), 0o600))
	require.Eventually(t, func() bool {
		_, st, readErr := verifyrun.Read(runsDir, runID)
		return readErr == nil && st.Kind == verifyrun.StateNotCompleted
	}, 5*time.Second, 50*time.Millisecond)

	s2 := startMCPStdioSession(t, []string{"-verification-config", verPath})
	finishedRaw := s2.callTool("3", "get_verification", map[string]any{"run_id": runID})
	finishedStatus, err := gensurla.DecodeVerificationStatusResult(finishedRaw)
	require.NoError(t, err)
	notCompleted, ok := finishedStatus.State.AsNotCompleted()
	require.True(t, ok)
	assert.Contains(t, notCompleted.Summary, "synthetic check finished")
	s2.closeAndWait()
}

// startMCPStdioSession starts `runMCP(args, ...)` in a background goroutine
// wired to OS pipes and returns the session handle.
func startMCPStdioSession(t *testing.T, args []string) *mcpStdioSession {
	t.Helper()
	stdinR, stdinW, err := os.Pipe()
	require.NoError(t, err)
	stdoutR, stdoutW, err := os.Pipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runMCP(args, stdinR, stdoutW, &stderr)
	}()
	s := &mcpStdioSession{
		t:      t,
		stdin:  stdinW,
		reader: bufio.NewReader(stdoutR),
		stdout: stdoutR,
		stderr: &stderr,
		done:   done,
	}
	t.Cleanup(func() {
		_ = stdinW.Close()
		_ = stdoutR.Close()
	})
	return s
}

// requireResourceContents extracts the first entry of `contents` from a
// `resources/read` result.
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

// call sends one JSON-RPC request and returns the decoded result map.
func (s *mcpStdioSession) call(id, method string, params map[string]any) map[string]any {
	s.t.Helper()
	if method == "initialize" {
		s.legacy = true
	} else if !s.legacy {
		if params == nil {
			params = map[string]any{}
		}
		meta, _ := params["_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		if _, ok := meta["io.modelcontextprotocol/protocolVersion"]; !ok {
			meta["io.modelcontextprotocol/protocolVersion"] = "2026-07-28"
		}
		if _, ok := meta["io.modelcontextprotocol/clientCapabilities"]; !ok {
			meta["io.modelcontextprotocol/clientCapabilities"] = map[string]any{}
		}
		params["_meta"] = meta
	}
	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	require.NoError(s.t, err)
	_, err = s.stdin.Write(append(raw, '\n'))
	require.NoError(s.t, err)
	line, err := s.reader.ReadBytes('\n')
	require.NoError(s.t, err)
	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   any             `json:"error"`
	}
	require.NoError(s.t, json.Unmarshal(line, &resp), "stdout line must be valid JSON-RPC: %s", string(line))
	assert.Equal(s.t, "2.0", resp.JSONRPC)
	require.Nil(s.t, resp.Error, "unexpected JSON-RPC error: %+v", resp.Error)
	var out map[string]any
	require.NoError(s.t, json.Unmarshal(resp.Result, &out))
	return out
}

// notify sends a JSON-RPC notification (no id) and expects no response frame.
func (s *mcpStdioSession) notify(method string, params any) {
	s.t.Helper()
	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	require.NoError(s.t, err)
	_, err = s.stdin.Write(append(raw, '\n'))
	require.NoError(s.t, err)
}

// callTool invokes `tools/call` for name and returns the raw structuredContent
// JSON, failing the test if the tool returned `isError: true`.
func (s *mcpStdioSession) callTool(id, name string, arguments any) json.RawMessage {
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
func (s *mcpStdioSession) callToolError(id, name string, arguments any) string {
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

// closeAndWait closes stdin, verifies that stdout reaches EOF with no extra
// non-JSON bytes, and waits for `runMCP` to return nil.
func (s *mcpStdioSession) closeAndWait() {
	s.t.Helper()
	require.NoError(s.t, s.stdin.Close())
	select {
	case err := <-s.done:
		require.NoError(s.t, err)
	case <-time.After(10 * time.Second):
		s.t.Fatal("runMCP did not exit within 10s after stdin closed")
	}
	rest, err := io.ReadAll(s.reader)
	require.NoError(s.t, err)
	assert.Empty(s.t, rest, "stdout must not contain trailing non-protocol bytes")
	require.NoError(s.t, s.stdout.Close())
}
