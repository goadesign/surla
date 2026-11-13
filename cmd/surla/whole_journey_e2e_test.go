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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goahttp "goa.design/goa/v3/http"

	genprojectclient "github.com/goadesign/surla/gen/http/project/client"
	genhttpclient "github.com/goadesign/surla/gen/http/surla/client"
	genmcpclient "github.com/goadesign/surla/gen/jsonrpc/mcp_surla/client"
	genmcpsurla "github.com/goadesign/surla/gen/mcp_surla"
	genproject "github.com/goadesign/surla/gen/project"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/project"
	"github.com/goadesign/surla/internal/testwriter"
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
		// projectCommit is the commit holding the starter and its Story, C1.
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
		// proposed and confirmed are the change's revisions before and after the
		// user confirmed it, R0 and R1.
		proposed, confirmed string
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
	// scripted agent's bearer credential, or the user's page session with the
	// same-origin headers a browser adds to the page's own requests.
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
	// journeyStoryVersion is the published github.com/goadesign/surla/story
	// version the project's Story requires, as the Story evaluation tests use.
	journeyStoryVersion = "v0.0.0-20261111183703-4568fcf5bb85"
	// journeyStory is the Story the user adds to the starter's design: one
	// finished flow, so a build requires one test, prices_scone.
	journeyStory = `package design

import . "github.com/goadesign/surla/story/dsl"

var _ = Story("Shop", func() {
	Actor("customer", "Customer")
	Flow("prices-scone", "Price a scone", func() {
		StartsWith("ask-scone-price", "customer", "The customer asks how much a scone is")
		Outcome("scone-priced", "customer", "The helper answers that a scone costs $2.75")
	})
})
`
	// journeyStepWait bounds one step's server-owned work: a Story evaluation
	// or a build start (5 minutes each in the control plane) and the team's
	// 45-second window phase.
	journeyStepWait = 6 * time.Minute
)

// journeyChangeArgs are the tool arguments naming the journey's change, which
// each worker's get_*_assignment takes.
var journeyChangeArgs = []byte(`{"change_id":"` + journeyChangeID + `"}`)

// TestWholeJourneyThroughTheProjectServer walks one change through the team
// in journey order: the starter project gets a Story and a commit; the real
// project server starts with its team and gives the user a page session; the
// guide proposes the change with the Story and the user confirms it; the user
// starts the build, and the builder and the test writer, each with the
// credential that start created, read their assignments; and a report for the
// change's earlier revision is refused. Each step stops the journey when it
// fails, because every later step builds on it.
func TestWholeJourneyThroughTheProjectServer(t *testing.T) {
	if err := candidate.CheckSandbox(); err != nil {
		t.Skipf("the journey builds the starter and its tests in the candidate sandbox: %v", err)
	}
	j := newWholeJourney(t)
	for _, step := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"the starter project with a Story is committed", j.commitStarterWithStory},
		{"the project server runs its team and opens the user's session", j.serveProject},
		{"the guide proposes the change and the user confirms it", j.proposeAndConfirm},
		{"the user starts the build and each worker reads its assignment", j.startBuild},
		{"a report for an earlier revision is refused", j.refuseEarlierReport},
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
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", liveModelGatewayEnv, liveModelProviderEnv, liveModelNameEnv,
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

// commitStarterWithStory creates the starter with surla init, adds the Story
// and the Story module it imports, and makes the project's first commit, as
// the commands surla init prints do.
func (j *wholeJourney) commitStarterWithStory(t *testing.T) {
	dir := filepath.Join(j.workspace, "shop")
	require.NoError(t, runInit([]string{
		"-dir", dir, "-module", "example.com/shop", "-surla-version", starterSurlaVersion,
	}, io.Discard))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "design", "story.go"), []byte(journeyStory), 0o644))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	get := exec.CommandContext(ctx, "go", "get",
		"github.com/goadesign/surla/story/dsl@"+journeyStoryVersion)
	get.Dir = dir
	get.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local")
	out, err := get.CombinedOutput()
	require.NoError(t, err, string(out))
	runTestGit(t, dir, "init", "-q")
	commit := gitCommitAll(t, dir, "Start shop with its Story")
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
		GetTeamEndpoint:       transport.GetTeam(),
		ConfirmIntentEndpoint: transport.ConfirmIntent(),
		StartBuildEndpoint:    transport.StartBuild(),
	}
	seen, err := j.user.GetTeam(ctx)
	require.NoError(t, err, "the user's session reads the team")
	require.Len(t, seen.Roles, 1)
	assert.Equal(t, team.Roles[0].Handle, seen.Roles[0].Handle)

	var folder string
	j.guide, folder = j.scriptedAgent(t, "guide")
	assert.Equal(t, j.dir, folder, "the guide works in the project")
}

// proposeAndConfirm has the guide evaluate the committed Story and propose
// the change with it, then the user confirm the proposal on the page, which
// makes the confirmed revision the change's agreed one.
func (j *wholeJourney) proposeAndConfirm(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), journeyStepWait)
	defer cancel()
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
	assert.Equal(t, "Shop", refresh.Stories[0].Name)
	require.Len(t, refresh.Stories[0].Flows, 1)
	assert.Equal(t, "prices-scone", refresh.Stories[0].Flows[0].Key)
	assert.True(t, refresh.Stories[0].Flows[0].Complete, "a flow with a start and an outcome is finished")

	args, err := gensurla.EncodeProposeChangePayload(&gensurla.ProposeChangePayload{
		ChangeID:         journeyChangeID,
		Prompt:           "Sell scones for $2.75 and let the helper answer their price.",
		StoryRefreshOpID: refresh.OpID,
		Analysis: &gensurla.CP1Analysis{
			Summary: "The shop sells scones at $2.75, and the helper answers a scone's price " +
				"as it answers tea's.",
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
	assert.Equal(t, 1, started.Build.RequiredTestCount)
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
	j.testWriter, folder = j.scriptedAgent(t, "test_writer")
	assert.Equal(t, copyDir, folder, "the test writer works in its own copy")

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
	assert.Equal(t, []*gensurla.RequiredTest{{
		Story: "Shop", FlowKey: "prices-scone", Scenario: "prices_scone", Hook: "PricesScone",
	}}, assignment.RequiredTests, "the Story's finished flow requires one test")
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

// startServer runs the real serve composition until the journey's test ends.
// Cleanup stops it through the CLI's management client and waits until it
// has drained its work; a server that does not stop keeps the folders it may
// still use by ending the test binary.
func (j *wholeJourney) startServer(t *testing.T, runtime projectRuntime) {
	served := make(chan error, 1)
	starting, cancelStart := context.WithTimeout(t.Context(), time.Minute)
	defer cancelStart()
	go func() {
		err := runtime.serve()
		cancelStart()
		served <- err
	}()
	state := j.state
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
