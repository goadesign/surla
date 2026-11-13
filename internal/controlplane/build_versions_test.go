// This file checks change detection against the build owner's public rendering.
package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	gensurla "github.com/goadesign/surla/gen/surla"
)

// TestChangeVersionFollowsBuildReadsAndReports exercises real assignment APIs
// and build starts while the card itself stays unchanged. The waiting read must
// name exactly the version a subsequent list returns.
func TestChangeVersionFollowsBuildReadsAndReports(t *testing.T) {
	cp := newBuildControlPlane(t, Options{})
	user := asCaller("alice@example.com", CallerUser)
	builder := asCaller("alice@example.com", CallerBuilder)
	writer := asCaller("alice@example.com", CallerTestWriter)
	card := confirmedChange(t, cp, "chg")
	before, err := cp.ListChanges(user)
	require.NoError(t, err)
	cases := []struct {
		name    string
		changed bool
		run     func() error
	}{
		{name: "start", changed: true, run: func() error {
			_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
			return err
		}},
		{name: "build read", run: func() error {
			build, err := cp.GetBuild(user, &gensurla.GetBuildPayload{ChangeID: "chg"})
			if assert.NoError(t, err) {
				started, ok := build.Build.AsStarted()
				if assert.True(t, ok) {
					assert.False(t, started.Builder.Read)
					assert.False(t, started.TestWriter.Read)
				}
			}
			return err
		}},
		{name: "builder list", run: func() error {
			_, err := cp.ListBuilderAssignments(builder)
			return err
		}},
		{name: "writer list", run: func() error {
			_, err := cp.ListTestWriterAssignments(writer)
			return err
		}},
		{name: "builder full read", changed: true, run: func() error {
			_, err := cp.GetBuilderAssignment(builder, &gensurla.GetBuilderAssignmentPayload{ChangeID: "chg"})
			return err
		}},
		{name: "builder reread", run: func() error {
			_, err := cp.GetBuilderAssignment(builder, &gensurla.GetBuilderAssignmentPayload{ChangeID: "chg"})
			return err
		}},
		{name: "writer full read", changed: true, run: func() error {
			_, err := cp.GetTestWriterAssignment(writer, &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
			return err
		}},
		{name: "writer reread", run: func() error {
			_, err := cp.GetTestWriterAssignment(writer, &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
			return err
		}},
		{name: "report", changed: true, run: func() error {
			_, err := cp.ReportTestsFinished(writer, &gensurla.ReportTestsFinishedPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
			return err
		}},
		{name: "same revision starts again", changed: true, run: func() error {
			_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, tc.run())
			after, err := cp.ListChanges(user)
			require.NoError(t, err)
			assert.Equal(t, before.Changes, after.Changes, "build effects do not save the card")
			if tc.changed {
				assert.NotEqual(t, before.Version, after.Version)
				waited, err := cp.WaitForChanges(user, &gensurla.WaitForChangesPayload{Since: before.Version})
				require.NoError(t, err)
				assert.Equal(t, after.Version, waited.Version)
			} else {
				assert.Equal(t, before.Version, after.Version)
			}
			again, err := cp.ListChanges(user)
			require.NoError(t, err)
			assert.Equal(t, after, again, "reading the list does not mark assignments read")
			before = after
		})
	}
}

// TestChangeVersionReadsOnlyOwnersBuilds checks that another owner's saves and
// unreadable build do not affect this owner, while this owner's unreadable build
// fails both list and wait and keeps the cause on the operation's span.
func TestChangeVersionReadsOnlyOwnersBuilds(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	cp := newBuildControlPlane(t, Options{TracerProvider: provider})
	user := asCaller("alice@example.com", CallerUser)
	card := confirmedChange(t, cp, "chg")
	_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)
	before, err := cp.ListChanges(user)
	require.NoError(t, err)
	bob := asCaller("bob@example.com", CallerGuide)
	mustPropose(t, cp, bob, "bob", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
	bobBuild := filepath.Join(cp.cards.dir, buildFileName("bob"))
	require.NoError(t, os.WriteFile(bobBuild, []byte(`{"invalid_build":true}`), 0o600))
	after, err := cp.ListChanges(user)
	require.NoError(t, err)
	assert.Equal(t, before, after)

	require.NoError(t, os.WriteFile(filepath.Join(cp.cards.dir, buildFileName("chg")), []byte(`{"invalid_build":true}`), 0o600))
	_, err = cp.ListChanges(user)
	assert.EqualError(t, err, "list changes: saved builds could not be read")
	_, err = cp.WaitForChanges(user, &gensurla.WaitForChangesPayload{Since: before.Version})
	assert.EqualError(t, err, "list changes: saved builds could not be read")
	spans := exporter.GetSpans().Snapshots()
	failed := spans[len(spans)-1]
	assert.Equal(t, codes.Error, failed.Status().Code)
	require.Len(t, failed.Events(), 1)
	var cause string
	for _, attr := range failed.Events()[0].Attributes {
		if attr.Key == "exception.message" {
			cause = attr.Value.AsString()
		}
	}
	assert.Contains(t, cause, "invalid_build")
}

// TestChangeVersionUsesRenderedBuild checks missing and unusable builds have
// the same not_started rendering and that unexposed saved fields do not affect
// the version. No test verdict is manufactured here.
func TestChangeVersionUsesRenderedBuild(t *testing.T) {
	cp := newControlPlane(t, Options{})
	user := asCaller("alice@example.com", CallerUser)
	card := confirmedChange(t, cp, "chg")
	missing, err := cp.ListChanges(user)
	require.NoError(t, err)
	saved := buildFile{
		ChangeID: "chg", RequestRevision: card.RequestRevision, StartedAt: time.Now().UTC(),
		Roles: buildRolesBuilderOnly, TestWriter: testWriterAssignment{State: testsAssigned},
	}
	require.NoError(t, cp.cards.saveBuild(saved))
	unusable, err := cp.ListChanges(user)
	require.NoError(t, err)
	assert.Equal(t, missing, unusable)
	read, err := cp.GetBuild(user, &gensurla.GetBuildPayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, gensurla.BuildKindNotStarted, read.Build.Kind())

	saved.Roles = buildRolesBuilderAndTestWriter
	folder := t.TempDir()
	saved.TestWriter.WorkingFolder = &folder
	require.NoError(t, cp.cards.saveBuild(saved))
	usable, err := cp.ListChanges(user)
	require.NoError(t, err)
	assert.NotEqual(t, unusable.Version, usable.Version)
	before, err := cp.GetBuild(user, &gensurla.GetBuildPayload{ChangeID: "chg"})
	require.NoError(t, err)
	saved.DesignSource = "unexposed saved design"
	folder = t.TempDir()
	require.NoError(t, cp.cards.saveBuild(saved))
	after, err := cp.GetBuild(user, &gensurla.GetBuildPayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, before, after)
	unchanged, err := cp.ListChanges(user)
	require.NoError(t, err)
	assert.Equal(t, usable, unchanged, "only the canonical build result contributes")
}
