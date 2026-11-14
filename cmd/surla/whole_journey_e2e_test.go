//go:build e2e

// This file drives the whole team journey through the real project server:
// the server `surla serve` runs, with the in-memory Herdr stand-in and the
// launcher roster Chorus reads in temporary state, and scripted programs in
// place of the guide, the builder and the test writer, each connected with its
// own role's credential exactly as the roster hands it to an agent. The user
// acts through a views-page session obtained the way the page obtains one.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goahttp "goa.design/goa/v3/http"
	goa "goa.design/goa/v3/pkg"

	genprojectclient "github.com/goadesign/surla/gen/http/project/client"
	genhttpclient "github.com/goadesign/surla/gen/http/surla/client"
	genmcpclient "github.com/goadesign/surla/gen/jsonrpc/mcp_surla/client"
	genmcpsurla "github.com/goadesign/surla/gen/mcp_surla"
	genproject "github.com/goadesign/surla/gen/project"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/project"
	"github.com/goadesign/surla/internal/testwriter"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/verify/candidate"
)

type (
	// wholeJourney carries what each step of the journey hands the next. Its
	// steps run as subtests of one test; everything that outlives a step (the
	// folders, the working directory, the environment and the server) belongs
	// to that test, t, so a step's own test only asserts.
	wholeJourney struct {
		// t is the journey's test, which owns every resource the steps share.
		// Steps call only its Cleanup and Chdir, whose effects must outlive
		// the step; they assert with their own test.
		t *testing.T
		// workspace is the empty folder the user creates the project in.
		workspace string
		// dir is the project folder, as the server stores it: symlink-free.
		dir string
		// projectCommit is the starter's commit, C1, where the build's working
		// copy for the test writer starts.
		projectCommit string
		// state is the project's private state, which the server owns.
		state *project.State
		// rosters is the fixture team effects' launcher roster folder: the
		// server writes there the roster Chorus's launcher starts agents from.
		rosters string
		// space is the Herdr space the server prepared for the team.
		space string
		// user is the user's views-page session.
		user *gensurla.Client
		// guide, builder and testWriter are the scripted agents' connections.
		guide, builder, testWriter *genmcpsurla.Client
		// testsFolder is the test writer's working copy, as its roster row
		// names it.
		testsFolder string
		// proposed and confirmed are the change's revisions before and after the
		// user confirmed it, R0 and R1.
		proposed, confirmed string
		// testsCommit is the commit Surla recorded the test writer's first
		// accepted tests at, T1.
		testsCommit string
		// passed is the verification run of the builder's commit against T1,
		// as it finished with its passed report.
		passed *gensurla.VerificationStatusResult
	}

	// journeyRoster is the part of the launcher roster that Chorus turns into
	// an agent: each role's working folder and its connection to the project
	// server, which names the role's credential file.
	journeyRoster struct {
		// Roles lists one row per registered role whose folder exists.
		Roles []journeyRosterRole `json:"roles"`
	}

	// journeyRosterRole is one role's row of the launcher roster.
	journeyRosterRole struct {
		// Role is the trusted role: guide, builder or test_writer.
		Role string `json:"role"`
		// Handle is the role's stable Chorus handle.
		Handle string `json:"handle"`
		// Folder is the folder the agent works in.
		Folder string `json:"folder"`
		// Connections are the MCP servers the agent connects to.
		Connections []journeyRosterConnection `json:"connections"`
	}

	// journeyRosterConnection is the MCP server an agent connects to and the
	// file holding the credential it presents.
	journeyRosterConnection struct {
		// Name is the connection's name in the agent's configuration.
		Name string `json:"name"`
		// URL is the project server's MCP endpoint.
		URL string `json:"url"`
		// CredentialFile is the private file holding the role's credential.
		CredentialFile string `json:"credential_file"`
	}

	// credentialDoer sends every request with exactly one caller's headers: a
	// scripted agent's bearer credential, the user's page session with the
	// same-origin headers a browser adds to the page's own requests, or the
	// management credential the CLI presents.
	credentialDoer struct {
		// client sends the requests; contexts bound each call.
		client *http.Client
		// header sets one value for each of its names on every request.
		header http.Header
	}
)

const (
	// journeyChangeID is the change the guide proposes.
	journeyChangeID = "add-scone-price"
	// journeyCookieFlow ends the finished flow the starter's design/story.go
	// ships, prices-cookie, after which the guide adds its own flow.
	journeyCookieFlow = `		Outcome("cookie-priced", "customer", "The helper answers that a cookie costs $1.25")
	})
`
	// journeySconeFlow is the finished flow the guide adds to the shipped
	// Story, so a build requires two tests: prices_cookie and prices_scone.
	journeySconeFlow = `	Flow("prices-scone", "Price a scone", func() {
		StartsWith("ask-scone-price", "customer", "The customer asks how much a scone is")
		Outcome("scone-priced", "customer", "The helper answers that a scone costs $2.75")
	})
`
	// journeyCookieHook ends the starter's PricesCookie hook in
	// eval/acceptance/acceptance.go, after which the test writer adds its own.
	journeyCookieHook = `	return h.ask(ctx, "How much is cookie?", "[scripted] Cookie costs $1.25.", true)
}
`
	// journeySconeHook is the hook the test writer writes for prices_scone.
	journeySconeHook = `
// PricesScone is the test the Story's finished flow prices-scone requires.
func (h *Hooks) PricesScone(ctx context.Context) (eval.Result, error) {
	return h.ask(ctx, "How much is scone?", "[scripted] Scone costs $2.75.", true)
}
`
	// journeyCookiePrice is the starter's catalog line after which the builder
	// prices scones.
	journeyCookiePrice = `	"cookie": 125,
`
	// journeyStepWait bounds one step's server-owned work: a Story evaluation
	// or a build start (5 minutes each in the control plane), the team's
	// 45-second window phase, or one check of the test writer's report, whose
	// sandboxed build and listing have their own 10-minute and 60-second
	// limits but take seconds here.
	journeyStepWait = 6 * time.Minute
	// journeyWrittenTestsNotice is the fixed notice every run of written tests,
	// and every approval of one, carries.
	journeyWrittenTestsNotice = "These tests were written by a separate agent on this machine " +
		"and are not isolated from the builder."
)

var (
	// journeyChangeArgs are the tool arguments naming the journey's change,
	// which get_build, list_verifications and each worker's get_*_assignment
	// take.
	journeyChangeArgs = []byte(`{"change_id":"` + journeyChangeID + `"}`)
	// journeyScenarios are the scenarios a run of the accepted tests reports:
	// the starter suite's own and one per finished flow of the agreed Story.
	journeyScenarios = []string{
		"prices_tea", "prompt_override_reaches_model", "unknown_item", "prices_cookie", "prices_scone",
	}
)

// TestWholeJourneyThroughTheProjectServer walks one change through the team
// in journey order: the starter project, which ships a Story, is committed;
// the real project server starts with its team and gives the user a page
// session; the guide adds a finished flow to the Story and proposes the
// change with it, and the user confirms it; the user starts the build, and
// the builder and the test writer, each with the credential that start
// created, read their assignments. Then: a report for the change's earlier
// revision is refused; the builder's verification before any tests are
// accepted is refused with the test writer's step; a report that changed a
// file outside the test module is sent back with findings only the test
// writer and the user can read; a report without the hook the new flow
// requires is sent back with the compiler's finding naming it; the report
// with the hook is accepted and recorded on the tests branch, and the
// builder's assignment names that commit; the builder's commit passes those
// tests in a sandboxed run that says where the tests came from and that the
// test writer may read, and the change list shows it as current; the user
// approves that run on the page without promotion settings, and the approval
// names the builder's commit and the tests' commit; and a later acceptance of
// changed tests leaves that run the change's latest, passed, but no longer
// current, hides its approval, and refuses approving it again. Each step
// stops the journey when it fails, because every later step builds on it.
func TestWholeJourneyThroughTheProjectServer(t *testing.T) {
	if err := candidate.CheckSandbox(); err != nil {
		t.Skipf("the journey builds the starter and its tests in the candidate sandbox: %v", err)
	}
	j := newWholeJourney(t)
	for _, step := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"the starter project is committed", j.commitStarter},
		{"the project server runs its team and opens the user's session", j.serveProject},
		{"the guide adds a flow, proposes the change and the user confirms it", j.proposeAndConfirm},
		{"the user starts the build and each worker reads its assignment", j.startBuild},
		{"a report for an earlier revision is refused", j.refuseEarlierReport},
		{"verification before accepted tests is refused with the test writer's step",
			j.refuseUnacceptedVerification},
		{"a change outside the test module is sent back to the test writer", j.sendBackOutsideChange},
		{"tests without the required hook are sent back with the compiler's finding", j.sendBackMissingHook},
		{"the fixed tests are accepted and recorded on the tests branch", j.acceptTests},
		{"the builder's commit passes the accepted tests", j.verifyAcceptedTests},
		{"the user approves the passing run on the page", j.approvePassedRun},
		{"a later acceptance leaves the run passed but not current and its approval not shown",
			j.acceptLaterTests},
	} {
		if !t.Run(step.name, step.run) {
			return
		}
	}
}

// Do sends r with the caller's headers.
func (d credentialDoer) Do(r *http.Request) (*http.Response, error) {
	for name := range d.header {
		r.Header.Set(name, d.header.Get(name))
	}
	return d.client.Do(r)
}

// newWholeJourney isolates the journey from the user's configuration, Git
// identity, model keys and trace export, and moves into an empty workspace.
func newWholeJourney(t *testing.T) *wholeJourney {
	t.Helper()
	workspace := isolateServeWorkspace(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Surla Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Surla Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	for _, name := range []string{
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY",
		verify.LiveModelGatewayEnv, verify.LiveModelProviderEnv, verify.LiveModelNameEnv,
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	} {
		t.Setenv(name, "")
	}
	return &wholeJourney{t: t, workspace: workspace}
}

// journeyReportArgs returns report_tests_finished's arguments for revision
// of the journey's change.
func journeyReportArgs(t *testing.T, revision string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"change_id": journeyChangeID, "request_revision": revision,
	})
	require.NoError(t, err)
	return raw
}

// journeyRefusal calls the tool name and returns the text of the refusal it
// must answer with: a refused operation reaches an agent as a failed tool
// result, not as a transport error.
func journeyRefusal(t *testing.T, ctx context.Context, client *genmcpsurla.Client,
	name string, args []byte,
) string {
	t.Helper()
	result, err := client.ToolsCall(ctx, &genmcpsurla.ToolsCallPayload{Name: name, Arguments: args})
	require.NoError(t, err)
	complete, ok := result.Outcome.AsComplete()
	require.True(t, ok, "%s completes in the same call", name)
	require.True(t, complete.IsError != nil && *complete.IsError, "%s must be refused", name)
	return toolErrorText(complete.Content)
}

// readBuild has client read the journey's change's build with get_build,
// which every role may call, and returns the started build.
func readBuild(
	t *testing.T, ctx context.Context, client *genmcpsurla.Client,
) *gensurla.StartedBuild {
	t.Helper()
	read, err := gensurla.DecodeChangeBuild(
		trustedSuiteCall(t, ctx, client, "get_build", journeyChangeArgs))
	require.NoError(t, err)
	started, ok := read.Build.AsStarted()
	require.True(t, ok, "the change has a build: %s", read.Build.Kind())
	return started
}

// journeyVerifyArgs returns verify_change's arguments for candidateCommit at
// revision of the journey's change.
func journeyVerifyArgs(t *testing.T, revision, candidateCommit string) []byte {
	t.Helper()
	raw, err := gensurla.EncodeVerifyChangePayload(&gensurla.VerifyChangePayload{
		ChangeID: journeyChangeID, RequestRevision: revision, CandidateCommit: candidateCommit,
	})
	require.NoError(t, err)
	return raw
}

// listRuns has client list the verification runs of the journey's change.
func listRuns(
	t *testing.T, ctx context.Context, client *genmcpsurla.Client,
) []*gensurla.VerificationStatusResult {
	t.Helper()
	listed, err := gensurla.DecodeVerificationListResult(
		trustedSuiteCall(t, ctx, client, "list_verifications", journeyChangeArgs))
	require.NoError(t, err)
	return listed.Runs
}

// replaceOnce edits the file at path the way an agent edits a file in its
// own folder: old must occur in it exactly once and becomes replacement.
func replaceOnce(t *testing.T, path, old, replacement string) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(raw), old),
		"%s holds the text to replace once", filepath.Base(path))
	edited := strings.Replace(string(raw), old, replacement, 1)
	require.NoError(t, os.WriteFile(path, []byte(edited), info.Mode().Perm()))
}

// commitStarter creates the starter with surla init, which ships its Story,
// and makes the project's first commit of every file, as the commands surla
// init prints do.
func (j *wholeJourney) commitStarter(t *testing.T) {
	dir := filepath.Join(j.workspace, "shop")
	require.NoError(t, runInit([]string{
		"-dir", dir, "-module", "example.com/shop", "-surla-version", starterSurlaVersion,
	}, io.Discard))
	runTestGit(t, dir, "init", "-q")
	commit := gitCommitAll(t, dir, "Start shop")
	assert.Regexp(t, "^[0-9a-f]{40}$", commit)
	assert.Empty(t, runTestGit(t, dir, "status", "--porcelain", "--untracked-files=all"),
		"the commit holds every file")
	physical, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	j.dir, j.projectCommit = physical, commit
}

// serveProject creates the project the way surla does and runs the real
// surla serve composition in the project folder, with the fixture team
// effects; ensures the team through the CLI's management client; and turns a
// browser link into the user's page session.
func (j *wholeJourney) serveProject(t *testing.T) {
	state, err := fixtureProjectState(j.dir)
	require.NoError(t, err)
	j.state = state
	j.t.Chdir(j.dir)
	runtime := testProjectRuntime()
	bindFixtureTeamEffects(&runtime)
	root, err := runtime.configRoot()
	require.NoError(t, err)
	// bindFixtureTeamEffects writes the launcher's rosters here.
	j.rosters = filepath.Join(root, "fixture-rosters")
	j.startServer(t, runtime)

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	management := projectClient(state)
	team, err := management.EnsureTeam(ctx)
	require.NoError(t, err)
	j.space = team.Space
	require.Len(t, team.Roles, 1, "only the guide works before a build")
	assert.Equal(t, "guide", team.Roles[0].Role)
	assert.False(t, team.Roles[0].Running)
	assert.Equal(t, "opened", team.Dialog, "the launcher opens for the guide")
	roster := j.roster(t)
	require.Len(t, roster.Roles, 1, "the roster names no worker credential before a build")
	assert.Equal(t, team.Roles[0].Handle, roster.Roles[0].Handle)

	link, err := management.BrowserLink(ctx)
	require.NoError(t, err)
	require.NoError(t, validateProjectViewsLink(state, link.URL))
	page := genprojectclient.NewClient("http", state.Address(),
		credentialDoer{client: &http.Client{}, header: j.pageHeader()},
		goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	exchanged, err := page.ExchangeBrowserLink()(ctx, &genproject.ExchangeBrowserLinkPayload{
		Link: strings.TrimPrefix(link.URL, state.Origin()+"/views#link="),
	})
	require.NoError(t, err)
	header := j.pageHeader()
	header.Set("Authorization", "Bearer "+exchanged.(*genproject.ExchangeBrowserLinkResult).Session)
	transport := genhttpclient.NewClient("http", state.Address(),
		credentialDoer{client: &http.Client{}, header: header},
		goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	j.user = &gensurla.Client{
		GetTeamEndpoint:         transport.GetTeam(),
		ConfirmIntentEndpoint:   transport.ConfirmIntent(),
		StartBuildEndpoint:      transport.StartBuild(),
		GetBuildEndpoint:        transport.GetBuild(),
		GetTestFindingsEndpoint: transport.GetTestFindings(),
		ListChangesEndpoint:     transport.ListChanges(),
		GetChangeEndpoint:       transport.GetChange(),
		ApproveMergeEndpoint:    transport.ApproveMerge(),
	}
	seen, err := j.user.GetTeam(ctx)
	require.NoError(t, err, "the user's session reads the team")
	require.Len(t, seen.Roles, 1)
	assert.Equal(t, team.Roles[0].Handle, seen.Roles[0].Handle)

	var folder string
	j.guide, folder = j.scriptedAgent(t, "guide")
	assert.Equal(t, j.dir, folder, "the guide works in the project")
}

// proposeAndConfirm has the guide add the finished flow prices-scone to the
// Story the starter shipped, evaluate it and propose the change with it, then
// the user confirm the proposal on the page, which makes the confirmed
// revision the change's agreed one.
func (j *wholeJourney) proposeAndConfirm(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), journeyStepWait)
	defer cancel()
	replaceOnce(t, filepath.Join(j.dir, "design", "story.go"),
		journeyCookieFlow, journeyCookieFlow+journeySconeFlow)
	refresh, err := gensurla.DecodeStoryRefreshResult(
		trustedSuiteCall(t, ctx, j.guide, "refresh_story", []byte(`{}`)))
	require.NoError(t, err)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for refresh.State == "running" {
		select {
		case <-ctx.Done():
			t.Fatalf("the Story evaluation did not finish: %v", ctx.Err())
		case <-ticker.C:
		}
		refresh, err = gensurla.DecodeStoryRefreshResult(
			trustedSuiteCall(t, ctx, j.guide, "get_story_refresh", []byte(`{}`)))
		require.NoError(t, err)
	}
	require.Equal(t, "succeeded", refresh.State, "%+v", refresh.Diagnostic)
	require.NotNil(t, refresh.OpID)
	require.Len(t, refresh.Stories, 1)
	assert.Equal(t, "Prices", refresh.Stories[0].Name, "the guide extends the Story the starter ships")
	require.Len(t, refresh.Stories[0].Flows, 2)
	for i, key := range []string{"prices-cookie", "prices-scone"} {
		assert.Equal(t, key, refresh.Stories[0].Flows[i].Key)
		assert.True(t, refresh.Stories[0].Flows[i].Complete, "a flow with a start and an outcome is finished")
	}

	args, err := gensurla.EncodeProposeChangePayload(&gensurla.ProposeChangePayload{
		ChangeID:         journeyChangeID,
		Prompt:           "Sell scones for $2.75 and let the helper answer their price.",
		StoryRefreshOpID: refresh.OpID,
		Analysis: &gensurla.CP1Analysis{
			Summary: "The shop sells scones at $2.75, and the helper answers a scone's price " +
				"as it answers a cookie's.",
			Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
			Examples: []*gensurla.CP1AnalysisExample{{
				Scenario: "Scone price", InputSummary: "Ask how much a scone is.",
				ExpectedOutcome:  "The helper answers that a scone costs $2.75.",
				VerificationKind: "deterministic",
			}},
			PreservedBehaviors: []string{"Tea costs $3.50 and a cookie $1.25."},
		},
	})
	require.NoError(t, err)
	proposed, err := gensurla.DecodeChangeCardResult(
		trustedSuiteCall(t, ctx, j.guide, "propose_change", args))
	require.NoError(t, err)
	assert.Equal(t, controlplane.StatusNeedsConfirmation, proposed.Status)
	assert.Equal(t, refresh.Stories, proposed.Stories, "the card adopts the evaluated Story")

	confirmed, err := j.user.ConfirmIntent(ctx, &gensurla.ConfirmIntentPayload{
		ChangeID: journeyChangeID, RequestRevision: proposed.RequestRevision,
		Answers: map[string]string{}, Examples: proposed.Examples,
	})
	require.NoError(t, err)
	assert.Equal(t, controlplane.StatusConfirmed, confirmed.Status)
	assert.NotEqual(t, proposed.RequestRevision, confirmed.RequestRevision,
		"confirming makes a new revision")
	assert.Equal(t, proposed.Stories, confirmed.Stories)
	if assert.NotNil(t, confirmed.ConfirmedBy) {
		assert.Equal(t, j.state.Record.Owner, *confirmed.ConfirmedBy,
			"the page session acts for the project's owner")
	}
	j.proposed, j.confirmed = proposed.RequestRevision, confirmed.RequestRevision
}

// startBuild has the user start the build of the confirmed revision, then
// connects the builder and the test writer with the roles and credentials
// that start registered, and has each read its assignment.
func (j *wholeJourney) startBuild(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), journeyStepWait)
	defer cancel()
	started, err := j.user.StartBuild(ctx, &gensurla.StartBuildPayload{
		ChangeID: journeyChangeID, RequestRevision: j.confirmed,
	})
	require.NoError(t, err)
	assert.Equal(t, j.confirmed, started.Build.RequestRevision)
	assert.True(t, started.Build.InForce)
	assert.Equal(t, 2, started.Build.RequiredTestCount, "one test per finished flow")
	assert.Equal(t, gensurla.TestsKindAssigned, started.Build.TestWriter.Tests.Kind())
	copied, ok := started.WorkingCopy.AsAtProjectCommit()
	require.True(t, ok, "the test writer's copy starts at the project's commit: %s",
		started.WorkingCopy.Kind())
	assert.Equal(t, j.projectCommit, copied.Commit)
	team, ok := started.Team.AsReady()
	require.True(t, ok, "the team is ready: %s", started.Team.Kind())
	assert.Equal(t, "opened", team.Dialog, "the launcher opens for the new workers")
	handles := map[string]string{}
	for _, role := range team.Roles {
		handles[role.Role] = role.Handle
	}

	copyDir := filepath.Join(j.dir, testwriter.Folder)
	assert.Equal(t, j.projectCommit, strings.TrimSpace(runTestGit(t, copyDir, "rev-parse", "HEAD")))
	assert.Equal(t, testwriter.Branch,
		strings.TrimSpace(runTestGit(t, copyDir, "symbolic-ref", "--short", "HEAD")))

	roster := j.roster(t)
	credentials := map[string]string{}
	for _, role := range roster.Roles {
		assert.Equal(t, handles[role.Role], role.Handle, "the roster lists the team's %s", role.Role)
		require.Len(t, role.Connections, 1)
		raw, err := os.ReadFile(role.Connections[0].CredentialFile)
		require.NoError(t, err)
		credentials[role.Role] = string(raw)
	}
	require.Len(t, credentials, 3, "the start adds the builder and the test writer to the guide")
	assert.NotEqual(t, credentials["guide"], credentials["builder"])
	assert.NotEqual(t, credentials["guide"], credentials["test_writer"])
	assert.NotEqual(t, credentials["builder"], credentials["test_writer"])

	var folder string
	j.builder, folder = j.scriptedAgent(t, "builder")
	assert.Equal(t, j.dir, folder, "the builder works in the project")
	j.testWriter, j.testsFolder = j.scriptedAgent(t, "test_writer")
	require.Equal(t, copyDir, j.testsFolder, "the test writer works in its own copy")

	listed, err := gensurla.DecodeBuilderAssignmentList(
		trustedSuiteCall(t, ctx, j.builder, "list_builder_assignments", []byte(`{}`)))
	require.NoError(t, err)
	require.Len(t, listed.Assignments, 1)
	assert.Equal(t, journeyChangeID, listed.Assignments[0].ChangeID)
	built := j.readBuilderAssignment(t, ctx)
	assert.Equal(t, j.confirmed, built.RequestRevision)
	assert.True(t, built.InForce)
	assert.True(t, built.Read)
	assert.Equal(t, gensurla.WrittenTestsKindNotYetAccepted, built.WrittenTests.Kind(),
		"the builder sees that no tests are accepted yet")

	tests, err := gensurla.DecodeTestWriterAssignmentList(
		trustedSuiteCall(t, ctx, j.testWriter, "list_test_writer_assignments", []byte(`{}`)))
	require.NoError(t, err)
	require.Len(t, tests.Assignments, 1)
	assert.Equal(t, journeyChangeID, tests.Assignments[0].ChangeID)
	assignment := j.readTestWriterAssignment(t, ctx)
	assert.Equal(t, j.confirmed, assignment.RequestRevision)
	assert.Equal(t, copyDir, assignment.WorkingFolder)
	assert.Equal(t, []*gensurla.RequiredTest{
		{Story: "Prices", FlowKey: "prices-cookie", Scenario: "prices_cookie", Hook: "PricesCookie"},
		{Story: "Prices", FlowKey: "prices-scone", Scenario: "prices_scone", Hook: "PricesScone"},
	}, assignment.RequiredTests, "each finished flow requires one test, in declaration order")
	assert.Equal(t, gensurla.TestsKindAssigned, assignment.Progress.Tests.Kind())
	assert.True(t, assignment.Progress.Read)
}

// refuseEarlierReport has the test writer report its tests for the proposal's
// revision, which the build is not for; the refusal leaves its assignment as
// it was.
func (j *wholeJourney) refuseEarlierReport(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	refused := journeyRefusal(t, ctx, j.testWriter, "report_tests_finished",
		journeyReportArgs(t, j.proposed))
	assert.Contains(t, refused, fmt.Sprintf("is for request_revision %q, not %q", j.confirmed, j.proposed))
	assignment := j.readTestWriterAssignment(t, ctx)
	assert.Equal(t, gensurla.TestsKindAssigned, assignment.Progress.Tests.Kind(),
		"a refused report changes nothing")
}

// refuseUnacceptedVerification has the builder verify the starter's commit
// for the agreed revision before the test writer reported: the refusal names
// the tests' state and the test writer's step, and starts no run.
func (j *wholeJourney) refuseUnacceptedVerification(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	refused := journeyRefusal(t, ctx, j.builder, "verify_change",
		journeyVerifyArgs(t, j.confirmed, j.projectCommit))
	assert.Contains(t, refused, "the tests of this build are not accepted; their state is assigned")
	assert.Contains(t, refused, "the test writer writes the required tests and reports them finished")
	assert.Empty(t, listRuns(t, ctx, j.builder), "a refused verification starts no run")
}

// sendBackOutsideChange has the test writer change the README at the top of
// its working copy, outside the test module, and report. Surla's check sends
// the report back: every role that reads the build sees how many findings it
// has, only the test writer and the user read their text, and the tests
// branch stays where the build started it.
func (j *wholeJourney) sendBackOutsideChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), journeyStepWait)
	defer cancel()
	readme := filepath.Join(j.testsFolder, "README.md")
	raw, err := os.ReadFile(readme)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(readme, append(raw, "\nThe test writer's notes.\n"...), 0o644))

	build := j.reportTests(t, ctx, 1)
	needs, ok := build.TestWriter.Tests.AsNeedsChanges()
	require.True(t, ok, "the report is sent back: %s", build.TestWriter.Tests.Kind())
	assert.Equal(t, 1, needs.FindingCount, "one file changed outside the test module")
	for role, client := range map[string]*genmcpsurla.Client{
		"guide": j.guide, "builder": j.builder, "test writer": j.testWriter,
	} {
		seen := readBuild(t, ctx, client)
		assert.Equal(t, build.TestWriter.Tests, seen.TestWriter.Tests, "the %s sees the count", role)
	}
	page, err := j.user.GetBuild(ctx, &gensurla.GetBuildPayload{ChangeID: journeyChangeID})
	require.NoError(t, err)
	shown, ok := page.Build.AsStarted()
	require.True(t, ok)
	assert.Equal(t, build.TestWriter.Tests, shown.TestWriter.Tests, "the user's page sees the count")

	findings, err := j.user.GetTestFindings(ctx, &gensurla.GetTestFindingsPayload{ChangeID: journeyChangeID})
	require.NoError(t, err)
	assert.Equal(t, j.confirmed, findings.RequestRevision)
	assert.Equal(t, 1, findings.Report)
	require.Len(t, findings.Findings, 1)
	assert.Contains(t, findings.Findings[0], "README.md is changed outside the test module eval")
	assignment := j.readTestWriterAssignment(t, ctx)
	assert.Equal(t, findings.Findings, assignment.Findings, "the test writer reads the user's findings")
	for role, client := range map[string]*genmcpsurla.Client{"guide": j.guide, "builder": j.builder} {
		raw := trustedSuiteCall(t, ctx, client, "get_build", journeyChangeArgs)
		assert.NotContains(t, string(raw), "README.md", "the %s's build holds no finding text", role)
		refused := journeyRefusal(t, ctx, client, "get_test_writer_assignment", journeyChangeArgs)
		assert.Contains(t, refused, "caller may not call get_test_writer_assignment", "the %s", role)
		_, err := client.ToolsCall(ctx, &genmcpsurla.ToolsCallPayload{
			Name: "get_test_findings", Arguments: journeyChangeArgs,
		})
		assert.ErrorContains(t, err, "unknown tool", "no tool gives the %s the findings", role)
	}
	assert.Equal(t, j.projectCommit, j.testsBranch(t), "a report sent back records nothing")
}

// sendBackMissingHook has the test writer restore the README and report
// again before it wrote the hook the new flow requires. Surla's check builds
// the test program, which does not compile without that hook, and sends the
// report back with the compiler's finding naming it, for the test writer and
// the user; the tests branch stays where the build started it.
func (j *wholeJourney) sendBackMissingHook(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), journeyStepWait)
	defer cancel()
	runTestGit(t, j.testsFolder, "checkout", "--", "README.md")

	build := j.reportTests(t, ctx, 2)
	needs, ok := build.TestWriter.Tests.AsNeedsChanges()
	require.True(t, ok, "a test program without the hook is sent back: %s", build.TestWriter.Tests.Kind())
	assert.Equal(t, 1, needs.FindingCount, "the build is the one step that failed")
	findings, err := j.user.GetTestFindings(ctx, &gensurla.GetTestFindingsPayload{ChangeID: journeyChangeID})
	require.NoError(t, err)
	assert.Equal(t, 2, findings.Report)
	require.Len(t, findings.Findings, 1)
	assert.True(t, strings.HasPrefix(findings.Findings[0], "The suite command does not compile:"),
		"the finding is the compiler's: %s", findings.Findings[0])
	assert.Contains(t, findings.Findings[0], "PricesScone", "the compiler names the missing hook")
	assignment := j.readTestWriterAssignment(t, ctx)
	assert.Equal(t, findings.Findings, assignment.Findings, "the test writer reads the compiler's finding")
	assert.Equal(t, j.projectCommit, j.testsBranch(t), "a report sent back records nothing")
}

// acceptTests has the test writer write the hook the new flow requires and
// report again. Surla accepts the tests and records them on the tests branch,
// and the builder's assignment, unread again, names that commit.
func (j *wholeJourney) acceptTests(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), journeyStepWait)
	defer cancel()
	replaceOnce(t, filepath.Join(j.testsFolder, "eval", "acceptance", "acceptance.go"),
		journeyCookieHook, journeyCookieHook+journeySconeHook)

	build := j.reportTests(t, ctx, 3)
	accepted, ok := build.TestWriter.Tests.AsAccepted()
	require.True(t, ok, "the tests are accepted: %s", build.TestWriter.Tests.Kind())
	require.Regexp(t, "^[0-9a-f]{40}$", accepted.Commit)
	assert.Equal(t, accepted.Commit, j.testsBranch(t), "the accepted commit is the tests branch's tip")
	assignment := j.readTestWriterAssignment(t, ctx)
	assert.Equal(t, build.TestWriter.Tests, assignment.Progress.Tests)
	assert.Empty(t, assignment.Findings)

	listed, err := gensurla.DecodeBuilderAssignmentList(
		trustedSuiteCall(t, ctx, j.builder, "list_builder_assignments", []byte(`{}`)))
	require.NoError(t, err)
	require.Len(t, listed.Assignments, 1)
	assert.False(t, listed.Assignments[0].Read, "an acceptance makes the builder's assignment unread")
	built := j.readBuilderAssignment(t, ctx)
	shown, ok := built.WrittenTests.AsAccepted()
	require.True(t, ok, "the builder sees the accepted tests: %s", built.WrittenTests.Kind())
	assert.Equal(t, accepted, shown)
	j.testsCommit = accepted.Commit
}

// verifyAcceptedTests has the builder price scones, commit the change with
// the guide's Story edit, which the project folder still holds uncommitted,
// and verify that commit for the agreed revision, then follows the run until it
// finishes. The run passes every scenario of the accepted tests and says
// where they came from; the test writer reads it, a caller the policy does
// not allow is refused, and the builder and the user's page see it as the
// change's latest verification, passed, with current inputs.
func (j *wholeJourney) verifyAcceptedTests(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), journeyStepWait)
	defer cancel()
	replaceOnce(t, filepath.Join(j.dir, "internal", "service", "service.go"),
		journeyCookiePrice, journeyCookiePrice+"\t\"scone\":  275,\n")
	runTestGit(t, j.dir, "add", "design/story.go", "internal/service/service.go")
	runTestGit(t, j.dir, "commit", "-q", "-m", "Price scones at $2.75")
	commit := strings.TrimSpace(runTestGit(t, j.dir, "rev-parse", "HEAD"))

	run, err := gensurla.DecodeVerificationStatusResult(trustedSuiteCall(t, ctx, j.builder, "verify_change",
		journeyVerifyArgs(t, j.confirmed, commit)))
	require.NoError(t, err)
	require.NotEmpty(t, run.RunID)
	// configureProjectWorkspace keeps the project's runs in its state folder.
	runsDir := filepath.Join(j.state.Dir, "runs")
	j.t.Cleanup(func() { drainTrustedSuiteRun(runsDir, run.RunID, j.state.Dir, j.dir) })
	args, err := gensurla.EncodeGetVerificationPayload(&gensurla.GetVerificationPayload{RunID: run.RunID})
	require.NoError(t, err)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for run.State.Kind() == gensurla.StateKindRunning {
		select {
		case <-ctx.Done():
			t.Fatalf("the verification run did not finish: %v", ctx.Err())
		case <-ticker.C:
		}
		run, err = gensurla.DecodeVerificationStatusResult(
			trustedSuiteCall(t, ctx, j.builder, "get_verification", args))
		require.NoError(t, err)
	}
	completed, ok := run.State.AsCompleted()
	require.True(t, ok, "the run ends with a report: %+v", run.State)
	require.True(t, completed.Report.Passed,
		"the builder's commit passes the accepted tests: %+v", completed.Report)
	scenarios := make([]string, 0, len(completed.Report.Scenarios))
	for _, s := range completed.Report.Scenarios {
		scenarios = append(scenarios, s.ID)
		assert.Equal(t, "passed", s.Outcome, "scenario %s", s.ID)
	}
	assert.ElementsMatch(t, journeyScenarios, scenarios, "the starter's scenarios and one per finished flow")
	assert.Equal(t, commit, run.CandidateCommit)
	assert.Equal(t, j.confirmed, run.RequestRevision)
	assert.Equal(t, j.testsCommit, run.SuiteCommit, "the run uses the accepted tests")
	written, ok := run.TestSource.AsWrittenTests()
	require.True(t, ok, "the run's tests are the written ones: %s", run.TestSource.Kind())
	assert.Equal(t, j.testsCommit, written.Commit)
	assert.Equal(t, journeyWrittenTestsNotice, written.Notice)

	read, err := gensurla.DecodeVerificationStatusResult(
		trustedSuiteCall(t, ctx, j.testWriter, "get_verification", args))
	require.NoError(t, err)
	assert.Equal(t, run, read, "the test writer reads the run of its assignment's change")
	runs := listRuns(t, ctx, j.testWriter)
	require.Len(t, runs, 1)
	assert.Equal(t, run.RunID, runs[0].RunID)
	running, err := j.state.ReadRunning()
	require.NoError(t, err)
	management := genhttpclient.NewClient("http", j.state.Address(), credentialDoer{
		client: &http.Client{},
		header: http.Header{"Authorization": {"Bearer " + running.ManagementCredential}},
	}, goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	_, err = management.GetVerification()(ctx, &gensurla.GetVerificationPayload{RunID: run.RunID})
	var refused *goa.ServiceError
	require.ErrorAs(t, err, &refused, "the management credential may not read a run")
	assert.Equal(t, "forbidden", refused.Name)
	for who, latest := range j.latestVerifications(t, ctx) {
		assert.Equal(t, &gensurla.LatestVerificationItem{RunID: run.RunID, State: "passed", InputsCurrent: true},
			latest, "the %s sees the run passed, for the revision and tests selected now", who)
	}
	j.passed = run
}

// approvePassedRun has the user approve the passing run on the page, with the
// call the page's Approve control makes. The project has no promotion
// settings, so the page offers approval and the approval records no binary
// digests; it names the builder's commit, the accepted tests' commit, the
// suite and environment the run's report declared, and where the tests came
// from. The user's next read of the change returns the same approval.
func (j *wholeJourney) approvePassedRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	list, err := j.user.ListChanges(ctx)
	require.NoError(t, err)
	assert.True(t, list.ApprovalAvailable, "the page offers approval without promotion settings")

	approved, err := j.user.ApproveMerge(ctx, j.approvePayload(t))
	require.NoError(t, err)
	completed, ok := j.passed.State.AsCompleted()
	require.True(t, ok)
	want := &gensurla.ApprovalItem{
		ApprovedBy:      j.state.Record.Owner,
		ReportDigest:    completed.Report.ReportDigest,
		CandidateCommit: j.passed.CandidateCommit,
		SuiteID:         completed.SuiteID,
		SuiteCommit:     j.testsCommit,
		Environment:     completed.Environment,
		Artifacts:       map[string]string{},
		ScenarioCount:   len(journeyScenarios),
		TestSource: gensurla.NewApprovalTestSourceWrittenTests(&gensurla.AgentWrittenTests{
			Commit: j.testsCommit, Notice: journeyWrittenTestsNotice,
		}),
	}
	assert.Equal(t, j.confirmed, approved.RequestRevision, "approval does not change the revision")
	assert.Equal(t, want, approved.Approval)
	read, err := j.user.GetChange(ctx, &gensurla.GetChangePayload{ChangeID: journeyChangeID})
	require.NoError(t, err)
	assert.Equal(t, want, read.Approval, "the user's next read of the change returns the approval")
	assert.Nil(t, read.UnverifiedApproval)
}

// acceptLaterTests has the test writer change a comment in its test module
// after the passing run was approved and report again. Surla accepts and
// records the changed tests at a new commit, and the builder's assignment
// names it; the earlier run, which ran the earlier tests, stays the change's
// latest verification, still passed, and the builder and the user's page see
// that its inputs are no longer current. The user's read of the change no
// longer shows the approval of the earlier tests, and approving that run
// again is refused with the builder's step.
func (j *wholeJourney) acceptLaterTests(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), journeyStepWait)
	defer cancel()
	replaceOnce(t, filepath.Join(j.testsFolder, "eval", "acceptance", "acceptance.go"),
		"// PricesScone is the test the Story's finished flow prices-scone requires.\n",
		"// PricesScone is the test the Story's finished flow prices-scone requires:\n"+
			"// the helper answers a scone's price from the catalog.\n")

	build := j.reportTests(t, ctx, 4)
	accepted, ok := build.TestWriter.Tests.AsAccepted()
	require.True(t, ok, "the changed tests are accepted: %s", build.TestWriter.Tests.Kind())
	assert.NotEqual(t, j.testsCommit, accepted.Commit, "Surla records the changed tests anew")
	assert.Equal(t, accepted.Commit, j.testsBranch(t))
	assert.Equal(t, j.testsCommit, strings.TrimSpace(runTestGit(t, j.dir, "rev-parse", accepted.Commit+"^")),
		"the new tests follow the earlier ones on the tests branch")

	listed, err := gensurla.DecodeBuilderAssignmentList(
		trustedSuiteCall(t, ctx, j.builder, "list_builder_assignments", []byte(`{}`)))
	require.NoError(t, err)
	require.Len(t, listed.Assignments, 1)
	assert.False(t, listed.Assignments[0].Read, "the new acceptance makes the builder's assignment unread")
	shown, ok := j.readBuilderAssignment(t, ctx).WrittenTests.AsAccepted()
	require.True(t, ok)
	assert.Equal(t, accepted.Commit, shown.Commit, "the builder sees the new tests")

	args, err := gensurla.EncodeGetVerificationPayload(&gensurla.GetVerificationPayload{RunID: j.passed.RunID})
	require.NoError(t, err)
	earlier, err := gensurla.DecodeVerificationStatusResult(
		trustedSuiteCall(t, ctx, j.builder, "get_verification", args))
	require.NoError(t, err)
	completed, ok := earlier.State.AsCompleted()
	require.True(t, ok)
	assert.True(t, completed.Report.Passed, "the earlier run still reads passed")
	assert.Equal(t, j.testsCommit, earlier.SuiteCommit, "it ran the earlier tests")
	notCurrent := &gensurla.LatestVerificationItem{RunID: j.passed.RunID, State: "passed", InputsCurrent: false}
	for who, latest := range j.latestVerifications(t, ctx) {
		assert.Equal(t, notCurrent, latest, "the %s sees the earlier run passed, for tests no longer selected", who)
	}

	read, err := j.user.GetChange(ctx, &gensurla.GetChangePayload{ChangeID: journeyChangeID})
	require.NoError(t, err)
	assert.Nil(t, read.Approval, "the approval of the earlier tests is no longer shown")
	assert.Nil(t, read.UnverifiedApproval)
	_, err = j.user.ApproveMerge(ctx, j.approvePayload(t))
	var refused *goa.ServiceError
	require.ErrorAs(t, err, &refused, "approving the run of the earlier tests is refused")
	assert.Equal(t, "unprocessable", refused.Name)
	assert.Contains(t, refused.Message, "the report ran the tests at "+j.testsCommit)
	assert.Contains(t, refused.Message, accepted.Commit, "the refusal names the accepted tests")
	assert.Contains(t, refused.Message, "the builder verifies the candidate again")
}

// approvePayload returns what the page's Approve control sends for the
// passing run: the change and its agreed revision, and the run's candidate
// commit and report digest.
func (j *wholeJourney) approvePayload(t *testing.T) *gensurla.ApproveMergePayload {
	t.Helper()
	completed, ok := j.passed.State.AsCompleted()
	require.True(t, ok, "only a finished run is approved")
	return &gensurla.ApproveMergePayload{
		ChangeID:        journeyChangeID,
		RequestRevision: j.confirmed,
		CandidateCommit: j.passed.CandidateCommit,
		ReportDigest:    completed.Report.ReportDigest,
	}
}

// startServer runs the real serve composition until the journey's test ends.
// Cleanup stops it through the CLI's management client and waits until it
// has drained its work; a server that does not stop keeps the folders it may
// still use by ending the test binary.
func (j *wholeJourney) startServer(t *testing.T, runtime projectRuntime) {
	served := make(chan error, 1)
	starting, cancelStart := context.WithTimeout(t.Context(), time.Minute)
	defer cancelStart()
	state := j.state
	go func() {
		err := runtime.serve(state.Dir)
		cancelStart()
		served <- err
	}()
	j.t.Cleanup(func() {
		select {
		case err := <-served:
			assert.NoError(j.t, err, "the project server ended early")
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		stopErr := projectClient(state).Stop(ctx)
		select {
		case err := <-served:
			assert.NoError(j.t, err, "the project server stopped with an error")
		case <-ctx.Done():
			retainAndExit(errors.Join(errors.New("the project server did not stop"), stopErr),
				state.Dir, j.dir)
		}
	})
	_, err := waitForRunning(starting, state)
	require.NoError(t, err, "the project server did not publish its management access")
}

// scriptedAgent connects a scripted program in place of role's agent the way
// the launcher starts an agent: from the role's roster row, with its surla
// connection and the credential in the file the row names. It returns the
// program's MCP client and the row's working folder.
func (j *wholeJourney) scriptedAgent(t *testing.T, role string) (*genmcpsurla.Client, string) {
	t.Helper()
	for _, row := range j.roster(t).Roles {
		if row.Role != role {
			continue
		}
		require.Len(t, row.Connections, 1)
		connection := row.Connections[0]
		assert.Equal(t, "surla", connection.Name)
		address, err := url.Parse(connection.URL)
		require.NoError(t, err)
		assert.Equal(t, j.state.Origin()+"/mcp", connection.URL)
		info, err := os.Stat(connection.CredentialFile)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "only the owner reads a credential")
		credential, err := os.ReadFile(connection.CredentialFile)
		require.NoError(t, err)
		doer := credentialDoer{
			client: &http.Client{},
			header: http.Header{"Authorization": {"Bearer " + string(credential)}},
		}
		transport := genmcpclient.NewClient(address.Scheme, address.Host, doer,
			goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
		return &genmcpsurla.Client{ToolsCallEndpoint: transport.ToolsCall()}, row.Folder
	}
	t.Fatalf("the roster has no %s", role)
	return nil, ""
}

// reportTests has the test writer report its tests finished for the agreed
// revision, checks that the report returns at once as reported with the
// number report, and waits, reading get_build as the test writer, until
// Surla's check replaces reported with its verdict on that report. It
// returns the build with the verdict.
func (j *wholeJourney) reportTests(
	t *testing.T, ctx context.Context, report int,
) *gensurla.StartedBuild {
	t.Helper()
	item, err := gensurla.DecodeTestWriterAssignmentItem(trustedSuiteCall(t, ctx, j.testWriter,
		"report_tests_finished", journeyReportArgs(t, j.confirmed)))
	require.NoError(t, err)
	require.Equal(t, gensurla.TestsKindReported, item.Progress.Tests.Kind(), "a report returns at once")
	require.Equal(t, report, item.Progress.Report)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		build := readBuild(t, ctx, j.testWriter)
		if build.TestWriter.Tests.Kind() != gensurla.TestsKindReported {
			require.Equal(t, report, build.TestWriter.Report, "the verdict answers report %d", report)
			return build
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Surla's check of report %d reached no verdict: %v", report, ctx.Err())
		case <-ticker.C:
		}
	}
}

// latestVerifications reads the change list as the builder, through
// list_changes, and as the user's page does, and returns the latest
// verification each sees for the journey's change, the only change listed.
func (j *wholeJourney) latestVerifications(
	t *testing.T, ctx context.Context,
) map[string]*gensurla.LatestVerificationItem {
	t.Helper()
	builder, err := gensurla.DecodeChangeList(
		trustedSuiteCall(t, ctx, j.builder, "list_changes", []byte(`{}`)))
	require.NoError(t, err)
	page, err := j.user.ListChanges(ctx)
	require.NoError(t, err)
	latest := map[string]*gensurla.LatestVerificationItem{}
	for who, list := range map[string]*gensurla.ChangeList{"builder": builder, "user": page} {
		require.Len(t, list.Changes, 1)
		require.NotNil(t, list.Changes[0].LatestVerification, "the %s sees a latest verification", who)
		latest[who] = list.Changes[0].LatestVerification
	}
	return latest
}

// testsBranch returns the commit the tests branch points to in the project's
// repository.
func (j *wholeJourney) testsBranch(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(runTestGit(t, j.dir, "rev-parse", "refs/heads/"+testwriter.Branch))
}

// readBuilderAssignment has the builder read its assignment for the journey's
// change in full.
func (j *wholeJourney) readBuilderAssignment(
	t *testing.T, ctx context.Context,
) *gensurla.BuilderAssignment {
	t.Helper()
	assignment, err := gensurla.DecodeBuilderAssignment(
		trustedSuiteCall(t, ctx, j.builder, "get_builder_assignment", journeyChangeArgs))
	require.NoError(t, err)
	return assignment
}

// readTestWriterAssignment has the test writer read its assignment for the
// journey's change in full.
func (j *wholeJourney) readTestWriterAssignment(
	t *testing.T, ctx context.Context,
) *gensurla.TestWriterAssignment {
	t.Helper()
	assignment, err := gensurla.DecodeTestWriterAssignment(
		trustedSuiteCall(t, ctx, j.testWriter, "get_test_writer_assignment", journeyChangeArgs))
	require.NoError(t, err)
	return assignment
}

// roster reads the launcher roster the server last wrote for the team's space.
func (j *wholeJourney) roster(t *testing.T) journeyRoster {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(j.rosters, j.space+".json"))
	require.NoError(t, err)
	var roster journeyRoster
	require.NoError(t, json.Unmarshal(raw, &roster))
	return roster
}

// pageHeader returns the headers a browser adds to the views page's own
// requests to the project server.
func (j *wholeJourney) pageHeader() http.Header {
	return http.Header{"Sec-Fetch-Site": {"same-origin"}, "Origin": {j.state.Origin()}}
}
