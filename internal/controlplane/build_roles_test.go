// This file checks the durable role contract and the one usable-build decision.
package controlplane

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuilderOnlyBuildRequiresStoredSettings reopens the saved records after the
// operator removes verification settings and checks every owning read consumer.
// The writer build keeps its assignment and unread work on the same restart.
func TestBuilderOnlyBuildRequiresStoredSettings(t *testing.T) {
	dir := t.TempDir()
	opts := Options{
		CardsDir:     dir,
		Verification: &VerificationConfig{RunsDir: t.TempDir()},
		Team:         &TeamBinding{Owner: "alice@example.com", Service: &teamServiceFixture{}},
	}
	cp := newControlPlane(t, opts)
	folder := t.TempDir()
	for _, roles := range []string{buildRolesBuilderOnly, buildRolesBuilderAndTestWriter} {
		card := confirmedChange(t, cp, roles)
		saved := buildFile{
			ChangeID: roles, RequestRevision: card.RequestRevision, StartedAt: time.Now().UTC(),
			Roles: roles, TestWriter: testWriterAssignment{State: testsAssigned},
		}
		if roles == buildRolesBuilderAndTestWriter {
			saved.TestWriter.WorkingFolder = &folder
		}
		require.NoError(t, cp.cards.saveBuild(saved))
		read := mustStartedBuild(t, cp, asCaller("alice@example.com", CallerUser), roles)
		assert.Equal(t, gensurla.TestsKindNotUsed, read.TestWriter.Tests.Kind())
	}
	assert.NotContains(t, string(readBuildFile(t, cp, buildRolesBuilderOnly)), "working_folder")
	_, err := cp.GetTestWriterAssignment(asCaller("alice@example.com", CallerTestWriter), &gensurla.GetTestWriterAssignmentPayload{ChangeID: buildRolesBuilderOnly})
	assertRefused(t, err, "not_found")
	assert.NoError(t, cp.Close())

	opts.Verification = nil
	opts.Team = &TeamBinding{Owner: "alice@example.com", Service: &teamServiceFixture{}}
	reopened := newControlPlane(t, opts)
	user := asCaller("alice@example.com", CallerUser)
	read, err := reopened.GetBuild(user, &gensurla.GetBuildPayload{ChangeID: buildRolesBuilderOnly})
	require.NoError(t, err)
	assert.Equal(t, gensurla.BuildKindNotStarted, read.Build.Kind())
	builder := asCaller("alice@example.com", CallerBuilder)
	builders, err := reopened.ListBuilderAssignments(builder)
	require.NoError(t, err)
	require.Len(t, builders.Assignments, 1)
	assert.Equal(t, buildRolesBuilderAndTestWriter, builders.Assignments[0].ChangeID)
	writer := asCaller("alice@example.com", CallerTestWriter)
	writers, err := reopened.ListTestWriterAssignments(writer)
	require.NoError(t, err)
	require.Len(t, writers.Assignments, 1)
	assert.Equal(t, buildRolesBuilderAndTestWriter, writers.Assignments[0].ChangeID)
	_, err = reopened.GetBuilderAssignment(builder, &gensurla.GetBuilderAssignmentPayload{ChangeID: buildRolesBuilderOnly})
	assertRefused(t, err, "not_found")
	assert.True(t, strings.HasSuffix(err.Error(), "start a new build on the views page"))
	_, err = reopened.GetTestWriterAssignment(writer, &gensurla.GetTestWriterAssignmentPayload{ChangeID: buildRolesBuilderOnly})
	assertRefused(t, err, "not_found")
	_, err = reopened.ReportTestsFinished(writer, &gensurla.ReportTestsFinishedPayload{ChangeID: buildRolesBuilderOnly})
	assertRefused(t, err, "not_found")
	owed, err := reopened.assignmentGenerations(t.Context())
	require.NoError(t, err)
	require.Len(t, owed, 2, "only the writer build owes its builder and test writer reads")
	assert.Equal(t, "builder", owed[0].Role)
	assert.Equal(t, "test_writer", owed[1].Role)
	assignment, err := reopened.GetTestWriterAssignment(writer, &gensurla.GetTestWriterAssignmentPayload{ChangeID: buildRolesBuilderAndTestWriter})
	require.NoError(t, err)
	assert.Equal(t, folder, assignment.WorkingFolder)
	assert.Equal(t, gensurla.TestsKindAssigned, assignment.Progress.Tests.Kind())
}

// TestBuildRoleRecordIsStrict validates the declared cases independently of
// current settings and never treats an absent or empty folder as the role mode.
func TestBuildRoleRecordIsStrict(t *testing.T) {
	abs := t.TempDir()
	empty, relative := "", "relative"
	cases := []struct {
		name, roles string
		folder      *string
		valid       bool
	}{
		{name: "builder only", roles: buildRolesBuilderOnly, valid: true},
		{name: "writer", roles: buildRolesBuilderAndTestWriter, folder: &abs, valid: true},
		{name: "missing mode"},
		{name: "unknown mode", roles: "automatic"},
		{name: "builder only with a folder", roles: buildRolesBuilderOnly, folder: &abs},
		{name: "builder only with an empty folder", roles: buildRolesBuilderOnly, folder: &empty},
		{name: "writer without a folder", roles: buildRolesBuilderAndTestWriter},
		{name: "writer with an empty folder", roles: buildRolesBuilderAndTestWriter, folder: &empty},
		{name: "writer with a relative folder", roles: buildRolesBuilderAndTestWriter, folder: &relative},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			saved := buildFile{
				ChangeID: "chg", RequestRevision: "revision", StartedAt: time.Now().UTC(), Roles: tc.roles,
				TestWriter: testWriterAssignment{State: testsAssigned, WorkingFolder: tc.folder},
			}
			raw, err := json.Marshal(saved)
			require.NoError(t, err)
			_, err = decodeBuild(buildFileName("chg"), raw)
			if tc.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}
