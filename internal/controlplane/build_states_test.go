// This file tests how a build's saved test writer state reads: every case of
// the test writer's progress and of the builder's view of the tests, over HTTP
// and over MCP, and the stored evaluator's "not used" whatever was saved.
package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
)

// TestBuildStatesReadTheSameOnBothInterfaces saves each test writer state, or
// binds a stored evaluator, and reads the build over HTTP and over MCP: the
// build read shows the test writer's progress with its report number and only
// the number of any findings, and the builder's assignment shows where the
// tests stand, both with each verdict's time.
func TestBuildStatesReadTheSameOnBothInterfaces(t *testing.T) {
	commit := strings.Repeat("c", 40)
	at := "2026-11-13T09:30:00Z"
	cases := []struct {
		name         string
		saved        map[string]any
		stored       bool
		tests        string
		writtenTests string
	}{
		{
			name:         "not used",
			saved:        map[string]any{"state": "assigned"},
			stored:       true,
			tests:        `{"state":"not_used"}`,
			writtenTests: `{"state":"not_used"}`,
		},
		{
			name:         "assigned",
			saved:        map[string]any{"state": "assigned"},
			tests:        `{"state":"assigned"}`,
			writtenTests: `{"state":"not_yet_accepted"}`,
		},
		{
			name:         "reported",
			saved:        map[string]any{"state": "reported"},
			tests:        `{"state":"reported"}`,
			writtenTests: `{"state":"not_yet_accepted"}`,
		},
		{
			name:         "needs changes",
			saved:        map[string]any{"state": "needs_changes", "findings": []string{"RefundOverLimit is not implemented"}, "checked_at": at},
			tests:        `{"state":"needs_changes","finding_count":1,"checked_at":"` + at + `"}`,
			writtenTests: `{"state":"not_yet_accepted"}`,
		},
		{
			name:         "accepted",
			saved:        map[string]any{"state": "accepted", "commit": commit, "checked_at": at},
			tests:        `{"state":"accepted","commit":"` + commit + `","checked_at":"` + at + `","not_judged":"` + testsNotJudged + `"}`,
			writtenTests: `{"state":"accepted","commit":"` + commit + `","checked_at":"` + at + `","not_judged":"` + testsNotJudged + `"}`,
		},
		{
			name:         "could not run",
			saved:        map[string]any{"state": "could_not_run", "reason": "git could not write the tests branch", "step": "Free disk space, then report again.", "checked_at": at},
			tests:        `{"state":"could_not_run","reason":"git could not write the tests branch","step":"Free disk space, then report again.","checked_at":"` + at + `"}`,
			writtenTests: `{"state":"not_yet_accepted"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{}
			if tc.stored {
				opts.Verification = &VerificationConfig{RunsDir: t.TempDir()}
			}
			cp := newBuildControlPlane(t, opts)
			srv := roleServer(t, cp)
			card := confirmedChange(t, cp, "chg")
			_, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
			require.NoError(t, err)
			editBuildFile(t, cp, "chg", savedTestWriter(tc.saved))

			status, body := httpCall(t, srv.URL, "user", http.MethodGet, "/changes/chg/build", "")
			require.Equal(t, http.StatusOK, status, body)
			report := "1"
			if tc.saved["state"] == testsAssigned {
				report = "0"
			}
			assert.JSONEq(t, tc.tests, jsonAt(t, []byte(body), "build", "test_writer", "tests"), "get_build over HTTP")
			assert.JSONEq(t, report, jsonAt(t, []byte(body), "build", "test_writer", "report"))
			failure, result := mcpToolCall(t, srv.URL, "guide", "get_build", `{"change_id":"chg"}`)
			require.Empty(t, failure)
			assert.JSONEq(t, tc.tests, jsonAt(t, result, "build", "test_writer", "tests"), "get_build over MCP")
			assert.JSONEq(t, report, jsonAt(t, result, "build", "test_writer", "report"))

			status, body = httpCall(t, srv.URL, "builder", http.MethodGet, "/assignments/builder/chg", "")
			require.Equal(t, http.StatusOK, status, body)
			assert.JSONEq(t, tc.writtenTests, jsonAt(t, []byte(body), "written_tests"), "get_builder_assignment over HTTP")
			failure, result = mcpToolCall(t, srv.URL, "builder", "list_builder_assignments", `{}`)
			require.Empty(t, failure)
			assert.JSONEq(t, tc.writtenTests, jsonAt(t, result, "assignments", "0", "written_tests"), "list_builder_assignments over MCP")
		})
	}
}

// TestBuildReadsShowTheSavedReportNumber saves a report number and reads it
// from every result that carries the test writer's progress, over HTTP and
// over MCP.
func TestBuildReadsShowTheSavedReportNumber(t *testing.T) {
	cp := newBuildControlPlane(t, Options{})
	srv := roleServer(t, cp)
	card := confirmedChange(t, cp, "chg")
	_, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)
	editBuildFile(t, cp, "chg", func(f map[string]any) map[string]any {
		f["test_writer"].(map[string]any)["state"] = testsReported
		f["report_number"] = 3
		return f
	})
	reads := []struct {
		name, token, method, path string
		keys                      []string
	}{
		{name: "get_build", token: "user", method: http.MethodGet, path: "/changes/chg/build", keys: []string{"build", "test_writer", "report"}},
		{name: "get_test_writer_assignment", token: "tests", method: http.MethodGet, path: "/assignments/test-writer/chg", keys: []string{"progress", "report"}},
		{name: "list_test_writer_assignments", token: "tests", method: http.MethodGet, path: "/assignments/test-writer", keys: []string{"assignments", "0", "progress", "report"}},
	}
	for _, r := range reads {
		status, body := httpCall(t, srv.URL, r.token, r.method, r.path, "")
		require.Equal(t, http.StatusOK, status, body)
		assert.JSONEq(t, "3", jsonAt(t, []byte(body), r.keys...), r.name+" over HTTP")
		arguments := `{"change_id":"chg"}`
		if r.name == "list_test_writer_assignments" {
			arguments = `{}`
		}
		failure, result := mcpToolCall(t, srv.URL, r.token, r.name, arguments)
		require.Empty(t, failure)
		assert.JSONEq(t, "3", jsonAt(t, result, r.keys...), r.name+" over MCP")
	}
}

// TestVerdictTimesAreSavedAndReturnedInUTC saves a verdict time with another
// offset, as a file could hold it, and checks that reads return it in UTC
// and that the next save writes it in UTC.
func TestVerdictTimesAreSavedAndReturnedInUTC(t *testing.T) {
	cp := newBuildControlPlane(t, Options{})
	card := confirmedChange(t, cp, "chg")
	_, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)
	editBuildFile(t, cp, "chg", savedTestWriter(map[string]any{
		"state": "accepted", "commit": strings.Repeat("c", 40), "checked_at": "2026-11-13T10:30:00+01:00",
	}))
	full, err := cp.GetTestWriterAssignment(asCaller("alice@example.com", CallerTestWriter), &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
	require.NoError(t, err)
	accepted, ok := full.Progress.Tests.AsAccepted()
	require.True(t, ok)
	assert.Equal(t, "2026-11-13T09:30:00Z", accepted.CheckedAt, "returned in UTC")
	var file map[string]any
	require.NoError(t, json.Unmarshal(readBuildFile(t, cp, "chg"), &file))
	assert.Equal(t, "2026-11-13T09:30:00Z", file["test_writer"].(map[string]any)["checked_at"], "saved in UTC by the read that recorded the read")
}

// TestStoredEvaluatorBuildsReadNotUsed reads builds in a project that verifies
// with its stored evaluator: whatever the test writer's state was saved as,
// every read shows that no test writer is used, while the same saved state in
// a project without one reads as saved.
func TestStoredEvaluatorBuildsReadNotUsed(t *testing.T) {
	commit := strings.Repeat("c", 40)
	at := "2026-11-13T09:30:00Z"
	saved := []map[string]any{
		{"state": "assigned"},
		{"state": "reported"},
		{"state": "needs_changes", "findings": []string{"RefundOverLimit is not implemented"}, "checked_at": at},
		{"state": "accepted", "commit": commit, "checked_at": at},
		{"state": "could_not_run", "reason": "git failed", "step": "Report again.", "checked_at": at},
	}
	for _, fields := range saved {
		t.Run(fields["state"].(string), func(t *testing.T) {
			stored := newBuildControlPlane(t, Options{Verification: &VerificationConfig{RunsDir: t.TempDir()}})
			require.True(t, stored.storedEvaluator())
			without := newBuildControlPlane(t, Options{})
			for _, cp := range []*ControlPlane{stored, without} {
				card := confirmedChange(t, cp, "chg")
				_, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
				require.NoError(t, err)
				editBuildFile(t, cp, "chg", savedTestWriter(fields))
			}

			build := mustStartedBuild(t, stored, asCaller("alice@example.com", CallerUser), "chg")
			assert.Equal(t, gensurla.TestsKindNotUsed, build.TestWriter.Tests.Kind(), "get_build")
			builder, err := stored.GetBuilderAssignment(asCaller("alice@example.com", CallerBuilder), &gensurla.GetBuilderAssignmentPayload{ChangeID: "chg"})
			require.NoError(t, err)
			assert.Equal(t, gensurla.WrittenTestsKindNotUsed, builder.WrittenTests.Kind(), "get_builder_assignment")
			listed, err := stored.ListBuilderAssignments(asCaller("alice@example.com", CallerBuilder))
			require.NoError(t, err)
			require.Len(t, listed.Assignments, 1)
			assert.Equal(t, gensurla.WrittenTestsKindNotUsed, listed.Assignments[0].WrittenTests.Kind(), "list_builder_assignments")
			full, err := stored.GetTestWriterAssignment(asCaller("alice@example.com", CallerTestWriter), &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
			assert.Nil(t, full)
			assertRefused(t, err, "not_found")
			items, err := stored.ListTestWriterAssignments(asCaller("alice@example.com", CallerTestWriter))
			require.NoError(t, err)
			assert.Empty(t, items.Assignments, "a builder-only build assigns no test writer")

			kept := mustStartedBuild(t, without, asCaller("alice@example.com", CallerUser), "chg")
			assert.Equal(t, fields["state"], string(kept.TestWriter.Tests.Kind()), "the saved state reads as saved without a stored evaluator")
		})
	}
}

// TestReportAfterAVerdictClearsIt reports finished tests after each verdict:
// the state becomes reported, as from any state, the report number rises,
// and the verdict's time, findings, commit, reason and step are gone, so the
// build still reads.
func TestReportAfterAVerdictClearsIt(t *testing.T) {
	at := "2026-11-13T09:30:00Z"
	verdicts := []map[string]any{
		{"state": "needs_changes", "findings": []string{"RefundOverLimit is not implemented"}, "checked_at": at},
		{"state": "accepted", "commit": strings.Repeat("c", 40), "checked_at": at},
		{"state": "could_not_run", "reason": "git failed", "step": "Report again.", "checked_at": at},
	}
	for _, fields := range verdicts {
		t.Run(fields["state"].(string), func(t *testing.T) {
			cp := newBuildControlPlane(t, Options{})
			card := confirmedChange(t, cp, "chg")
			_, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
			require.NoError(t, err)
			editBuildFile(t, cp, "chg", savedTestWriter(fields))
			reported, err := cp.ReportTestsFinished(asCaller("alice@example.com", CallerTestWriter), &gensurla.ReportTestsFinishedPayload{
				ChangeID: "chg", RequestRevision: card.RequestRevision,
			})
			require.NoError(t, err)
			assert.Equal(t, gensurla.TestsKindReported, reported.Progress.Tests.Kind())
			assert.Equal(t, 2, reported.Progress.Report, "the report after the verdict's is counted")
			assert.Equal(t, gensurla.TestsKindReported, mustStartedBuild(t, cp, asCaller("alice@example.com", CallerUser), "chg").TestWriter.Tests.Kind())
			var file map[string]any
			require.NoError(t, json.Unmarshal(readBuildFile(t, cp, "chg"), &file))
			saved := file["test_writer"].(map[string]any)
			for _, gone := range []string{"checked_at", "reason", "step"} {
				assert.NotContains(t, saved, gone)
			}
			assert.Empty(t, saved["findings"])
			assert.Empty(t, saved["commit"])
		})
	}
}

// TestStartBuildWithAStoredEvaluatorReturnsNotUsed starts builds in a project
// with a stored evaluator and in one without: the started build itself says
// that no test writer is used only in the first.
func TestStartBuildWithAStoredEvaluatorReturnsNotUsed(t *testing.T) {
	cases := []struct {
		name  string
		opts  func(t *testing.T) Options
		tests gensurla.TestsKind
	}{
		{name: "stored evaluator", opts: func(t *testing.T) Options {
			return Options{Verification: &VerificationConfig{RunsDir: t.TempDir()}}
		}, tests: gensurla.TestsKindNotUsed},
		{name: "no stored evaluator", opts: func(*testing.T) Options { return Options{} }, tests: gensurla.TestsKindAssigned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := newBuildControlPlane(t, tc.opts(t))
			card := confirmedChange(t, cp, "chg")
			started, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
			require.NoError(t, err)
			assert.Equal(t, tc.tests, started.Build.TestWriter.Tests.Kind())
		})
	}
}

// roleServer serves cp over HTTP and MCP to alice's user, guide, builder and
// test writer ("user", "guide", "builder" and "tests").
func roleServer(t *testing.T, cp *ControlPlane) *httptest.Server {
	t.Helper()
	credentials := map[string]Principal{}
	for token, kind := range map[string]CallerKind{"user": CallerUser, "guide": CallerGuide, "builder": CallerBuilder, "tests": CallerTestWriter} {
		credentials[token] = Principal{ID: "alice@example.com", CallerID: token, Kind: kind}
	}
	srv := httptest.NewServer(NewHTTPHandler(cp, fixtureCredentials(credentials)))
	t.Cleanup(srv.Close)
	return srv
}

// editBuildFile rewrites changeID's saved build through edit.
func editBuildFile(t *testing.T, cp *ControlPlane, changeID string, edit func(map[string]any) map[string]any) {
	t.Helper()
	var file map[string]any
	require.NoError(t, json.Unmarshal(readBuildFile(t, cp, changeID), &file))
	raw, err := json.Marshal(edit(file))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cp.cards.dir, buildFileName(changeID)), raw, 0o600))
}

// jsonAt returns the JSON value at keys in raw; a key that is a number
// indexes an array.
func jsonAt(t *testing.T, raw []byte, keys ...string) string {
	t.Helper()
	var value any
	require.NoError(t, json.Unmarshal(raw, &value))
	for _, key := range keys {
		switch node := value.(type) {
		case map[string]any:
			require.Contains(t, node, key)
			value = node[key]
		case []any:
			var index int
			require.NoError(t, json.Unmarshal([]byte(key), &index))
			require.Less(t, index, len(node))
			value = node[index]
		default:
			require.Failf(t, "not an object or array", "at %q in %s", key, raw)
		}
	}
	out, err := json.Marshal(value)
	require.NoError(t, err)
	return string(out)
}
