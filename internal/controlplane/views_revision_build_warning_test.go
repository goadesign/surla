// This file proves revision warnings before the served page changes an agreed
// card. Team and working-copy effects use the existing explicit stand-ins.
package controlplane

import (
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/require"
)

// TestViewsPageWarnsBeforeChangingABuiltRevision distinguishes a writer build,
// stored settings, unread/failed build reads, and no build. A no-op refinement
// keeps its revision and needs no invalidation warning; a changed submission
// or adoption shows the consequence beside its action before sending anything.
func TestViewsPageWarnsBeforeChangingABuiltRevision(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stored bool
		build  bool
		read   string
		adopt  bool
	}{
		{name: "writer refine", build: true},
		{name: "writer adopt", build: true, adopt: true},
		{name: "stored refine", stored: true, build: true},
		{name: "stored adopt", stored: true, build: true, adopt: true},
		{name: "unread refine", build: true, read: "held"},
		{name: "unread adopt", build: true, read: "held", adopt: true},
		{name: "failed read", build: true, read: "failed"},
		{name: "no build"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{Team: &TeamBinding{Owner: pageOwner, Service: &teamServiceFixture{}}}
			if tc.stored {
				opts.Verification = &VerificationConfig{RunsDir: t.TempDir()}
			}
			cp := newBuildControlPlane(t, opts)
			cp.changesWait, cp.changesPoll = 400*time.Millisecond, 20*time.Millisecond
			card := confirmedChange(t, cp, "chg-warning")
			if tc.build {
				_, err := cp.StartBuild(asCaller(pageOwner, CallerUser), &gensurla.StartBuildPayload{
					ChangeID: card.ChangeID, RequestRevision: card.RequestRevision,
				})
				require.NoError(t, err)
			}
			if tc.adopt {
				example := card.Examples[0]
				_, err := cp.ReviseChange(asCaller(pageOwner, CallerGuide), &gensurla.ReviseChangePayload{
					ChangeID: card.ChangeID, RequestRevision: card.RequestRevision,
					Prompt: "Review the revised refund decision.",
					Analysis: &gensurla.CP1RevisionAnalysis{
						Summary: "The revised refund decision.",
						Examples: []*gensurla.CP1RevisionAnalysisExample{{
							PriorID: &example.ID, Scenario: example.Scenario, InputSummary: example.InputSummary,
							ExpectedOutcome: "A manager reviews the revised decision", VerificationKind: "deterministic",
						}},
						PreservedBehaviors: card.PreservedBehaviors,
					},
				})
				require.NoError(t, err)
			}
			ps := startPageServer(t, cp)
			runPageScript(t, ps, map[string]any{
				"revision": card.RequestRevision, "stored": tc.stored,
				"build": tc.build, "read": tc.read, "adopt": tc.adopt,
			}, `
const buildPath = "/changes/chg-warning/build";
if (input.read === "held") await control(origin, "hold", { key: "GET " + buildPath });
if (input.read === "failed") await control(origin, "reply", { key: "GET " + buildPath, status: 503,
  body: { name: "unavailable", id: "warning-read", message: "Build read unavailable.", fault: false, temporary: true, timeout: false } });
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp1-editor")?.dataset.changeId === "chg-warning", "the agreed card did not open");
if (input.read === "held") await until(() => page.$("#surla-build-loading"), "the build read was not held");
else if (input.read === "failed") await until(() => page.$("#surla-build-error"), "the build read did not fail");
else await until(() => page.$("#surla-build-section")?.dataset.state === (input.build ? "started" : "not_started") && !page.$("#surla-build-loading"), "the saved build read did not settle");

let previousSaves = 0;
let warningId = "surla-proposal-build-consequence";
let actionId = "surla-proposal-adopt";
if (!input.adopt) {
  expect(!page.$("#surla-confirm-build-consequence"), "an unchanged confirmation falsely warns of invalidation");
  if (!input.read) {
    await until(() => page.$("#surla-confirm-submit") && !page.$("#surla-confirm-submit").disabled, "no-op confirmation was not available");
    page.$("#surla-confirm-submit").click();
    await until(() => page.sent("POST", "/changes/confirm").some((r) => r.status === 200) && !page.$("#surla-confirm-submit").disabled, "no-op confirmation did not return");
    expect(page.$("#surla-cp1-editor").dataset.requestRevision === input.revision, "no-op confirmation changed the revision");
    previousSaves = page.sent("POST", "/changes/confirm").length;
  }
  const field = page.$('textarea[data-example-id="check_01"][data-field="expected_outcome"]');
  field.focus();
  page.type(field, "A manager reviews the revised decision");
  expect(page.doc.activeElement === field && field.value.includes("revised decision"), "the warning replaced or unfocused the draft");
  warningId = "surla-confirm-build-consequence";
  actionId = "surla-confirm-submit";
}
const warning = page.$("#" + warningId);
const action = page.$("#" + actionId);
if (input.build) {
  expect(warning && action && action.getAttribute("aria-describedby")?.split(" ").includes(warningId), "the action has no linked build warning");
  expect(action.closest(".surla-field-row").previousElementSibling === warning, "the build consequence is not immediately beside the action");
  expect(warning.textContent.includes("earlier revision"), "the warning does not explain the build becoming earlier");
  if (input.stored) {
    expect(warning.textContent.includes("builder’s assignment") && warning.textContent.includes("stored verification settings") && !warning.textContent.includes("acceptance again"), "the warning invented a test-writer gate for stored settings");
  } else {
    expect(warning.textContent.includes(input.read ? "any existing build" : "this build") && warning.textContent.includes("before verification"), "the warning did not distinguish known and unread builds");
    expect(warning.textContent.includes("report") && warning.textContent.includes("verdict"), "the rebuild reset is not explained");
  }
} else expect(!warning, "a known absent build gets an invalidation warning");
expect(page.sent("POST", "/changes/confirm").length === previousSaves && page.sent("POST", buildPath).length === 0, "showing a consequence caused an effect");
await until(() => page.$("#" + actionId) && !page.$("#" + actionId).disabled, "the warned action was not available");
page.$("#" + actionId).click();
await until(() => page.$("#surla-cp1-editor")?.dataset.requestRevision !== input.revision, "the explicit revision action did not save");
expect(page.sent("POST", "/changes/confirm").length === previousSaves + 1 && page.sent("POST", buildPath).length === 0, "the action was repeated or rebuilt automatically");
if (input.read === "held") await control(origin, "release", { key: "GET " + buildPath });
if (input.build) await until(() => page.text("#surla-build-section").includes("Built for an earlier revision."), "the saved owner result did not mark the previous build earlier");
page.close();
`)
		})
	}
}
