//go:build e2e

// This file holds how the whole-journey test's callers act: the credential
// each one presents on its requests, the launcher roster a scripted agent
// connects from, the in-process project server's lifetime, and the calls and
// reads the journey's legs share.
package main

import (
	"context"
	"encoding/json"
	"errors"
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

	genmcpclient "github.com/goadesign/surla/gen/jsonrpc/mcp_surla/client"
	genmcpsurla "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/testwriter"
)

type (
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

// Do sends r with the caller's headers.
func (d credentialDoer) Do(r *http.Request) (*http.Response, error) {
	for name := range d.header {
		r.Header.Set(name, d.header.Get(name))
	}
	return d.client.Do(r)
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
	require.True(t, complete.IsError != nil && *complete.IsError,
		"%s must be refused; it returned %s", name, complete.StructuredContent)
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
