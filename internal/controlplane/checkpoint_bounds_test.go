// This file checks numeric bounds at the page generator and response boundary.
package controlplane

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCheckpointGeneratedNumericBounds generates validators in t.TempDir,
// retaining real declared bounds and adding synthetic number/maximum cases.
// It never rewrites the checkout or adds a production schema for a test.
func TestCheckpointGeneratedNumericBounds(t *testing.T) {
	node := requirePageTools(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "../../ui/checkpoint/testing/build_bounds.cjs", t.TempDir())
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "numeric validator generation failed:\n%s", out)
	require.True(t, strings.HasSuffix(string(out), "NUMERIC-BOUNDS-PASSED\n"), "unexpected generator proof output:\n%s", out)
}

// TestViewsPageRejectsOutOfBoundsBuild answers a read with a negative declared
// count or report. Neither may enable a start as though no build existed;
// the normal declared-result error and explicit retry recover the page.
func TestViewsPageRejectsOutOfBoundsBuild(t *testing.T) {
	for _, field := range []string{"required_test_count", "report"} {
		t.Run(field, func(t *testing.T) {
			team := &teamServiceFixture{snapshot: pageTeamSnapshot("working")}
			cp := newBuildControlPlane(t, Options{Team: &TeamBinding{Owner: pageOwner, Service: team}})
			card := confirmedChange(t, cp, "chg")
			ps := startPageServer(t, cp)
			runPageScript(t, ps, map[string]any{"field": field, "revision": card.RequestRevision}, `
const path = "/changes/chg/build";
const build = { state: "started", request_revision: input.revision, started_at: "2026-11-13T00:00:00Z", in_force: true,
  required_test_count: 0, builder: { read: false }, test_writer: { read: false, report: 0, tests: { state: "assigned" } } };
if (input.field === "report") build.test_writer.report = -1;
else build.required_test_count = -1;
await control(origin, "reply", { key: "GET " + path, status: 200, body: { change_id: "chg", build } });
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-error"), "a negative declared bound was accepted");
expect(page.text("#surla-build-error").includes("did not match its declared result"), "numeric violation bypassed the normal result decoder");
expect(!page.$("#surla-build-start") && !page.$("#surla-build-again"), "an invalid result enabled a build effect");
page.$("#surla-build-refresh").click();
await until(() => page.$("#surla-build-start") && !page.$("#surla-build-start").disabled, "a valid result did not recover the page");
expect(page.sent("POST", path).length === 0, "validation or recovery sent a start");
page.close();
`)
		})
	}
}
