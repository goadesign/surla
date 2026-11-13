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

// TestBuildStatesReadTheSameOnBothInterfaces saves each test writer state and
// reads it over HTTP and over MCP: the build read shows the test writer's
// progress and the builder's assignment shows where the tests stand, both
// with each verdict's time.
func TestBuildStatesReadTheSameOnBothInterfaces(t *testing.T) {
	commit := strings.Repeat("c", 40)
	at := "2026-11-13T09:30:00Z"
	cases := []struct {
		name         string
		saved        map[string]any
		tests        string
		writtenTests string
	}{
		{
			name:         "not used",
			saved:        map[string]any{"state": "not_used"},
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
			tests:        `{"state":"needs_changes","findings":["RefundOverLimit is not implemented"],"checked_at":"` + at + `"}`,
			writtenTests: `{"state":"not_yet_accepted"}`,
		},
		{
			name:         "accepted",
			saved:        map[string]any{"state": "accepted", "commit": commit, "checked_at": at},
			tests:        `{"state":"accepted","commit":"` + commit + `","checked_at":"` + at + `"}`,
			writtenTests: `{"state":"accepted","commit":"` + commit + `","checked_at":"` + at + `"}`,
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
			cp := newControlPlane(t, Options{})
			srv := roleServer(t, cp)
			card := confirmedChange(t, cp, "chg")
			_, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
			require.NoError(t, err)
			editBuildFile(t, cp, "chg", savedTestWriter(tc.saved))

			status, body := httpCall(t, srv.URL, "user", http.MethodGet, "/changes/chg/build", "")
			require.Equal(t, http.StatusOK, status, body)
			assert.JSONEq(t, tc.tests, jsonAt(t, []byte(body), "build", "test_writer", "tests"), "get_build over HTTP")
			failure, result := mcpToolCall(t, srv.URL, "guide", "get_build", `{"change_id":"chg"}`)
			require.Empty(t, failure)
			assert.JSONEq(t, tc.tests, jsonAt(t, result, "build", "test_writer", "tests"), "get_build over MCP")

			status, body = httpCall(t, srv.URL, "builder", http.MethodGet, "/assignments/builder/chg", "")
			require.Equal(t, http.StatusOK, status, body)
			assert.JSONEq(t, tc.writtenTests, jsonAt(t, []byte(body), "written_tests"), "get_builder_assignment over HTTP")
			failure, result = mcpToolCall(t, srv.URL, "builder", "list_builder_assignments", `{}`)
			require.Empty(t, failure)
			assert.JSONEq(t, tc.writtenTests, jsonAt(t, result, "assignments", "0", "written_tests"), "list_builder_assignments over MCP")
		})
	}
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
			stored := newControlPlane(t, Options{Verification: &VerificationConfig{RunsDir: t.TempDir()}})
			require.True(t, stored.storedEvaluator())
			without := newControlPlane(t, Options{})
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
			require.NoError(t, err)
			assert.Equal(t, gensurla.TestsKindNotUsed, full.Progress.Tests.Kind(), "get_test_writer_assignment")
			items, err := stored.ListTestWriterAssignments(asCaller("alice@example.com", CallerTestWriter))
			require.NoError(t, err)
			require.Len(t, items.Assignments, 1)
			assert.Equal(t, gensurla.TestsKindNotUsed, items.Assignments[0].Progress.Tests.Kind(), "list_test_writer_assignments")

			kept := mustStartedBuild(t, without, asCaller("alice@example.com", CallerUser), "chg")
			assert.Equal(t, fields["state"], string(kept.TestWriter.Tests.Kind()), "the saved state reads as saved without a stored evaluator")
		})
	}
}

// TestReportAfterAVerdictClearsIt reports finished tests after each verdict:
// the state becomes reported, as from any state, and the verdict's time,
// findings, commit, reason and step are gone, so the build still reads.
func TestReportAfterAVerdictClearsIt(t *testing.T) {
	at := "2026-11-13T09:30:00Z"
	verdicts := []map[string]any{
		{"state": "needs_changes", "findings": []string{"RefundOverLimit is not implemented"}, "checked_at": at},
		{"state": "accepted", "commit": strings.Repeat("c", 40), "checked_at": at},
		{"state": "could_not_run", "reason": "git failed", "step": "Report again.", "checked_at": at},
	}
	for _, fields := range verdicts {
		t.Run(fields["state"].(string), func(t *testing.T) {
			cp := newControlPlane(t, Options{})
			card := confirmedChange(t, cp, "chg")
			_, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
			require.NoError(t, err)
			editBuildFile(t, cp, "chg", savedTestWriter(fields))
			reported, err := cp.ReportTestsFinished(asCaller("alice@example.com", CallerTestWriter), &gensurla.ReportTestsFinishedPayload{
				ChangeID: "chg", RequestRevision: card.RequestRevision,
			})
			require.NoError(t, err)
			assert.Equal(t, gensurla.TestsKindReported, reported.Progress.Tests.Kind())
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
