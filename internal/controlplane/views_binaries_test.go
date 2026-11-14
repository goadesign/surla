// These served-page tests use retained run and binary fixtures to check
// promotion guidance. They do not execute candidate code or a model.
package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
	"github.com/stretchr/testify/require"
)

// binariesPageFixture retains two executions with the same report digest and
// distinct output folders outside the default run layout. Its configured
// promotion target measures different release files, so approval is refused.
type binariesPageFixture struct {
	cp       *ControlPlane
	older    string
	newer    string
	olderDir string
	newerDir string
	digest   string
	// disablePromotion changes the embedding host's current target through
	// its normal resolver, without mutating a target that a read is using.
	disablePromotion func()
}

// TestViewsPageBinariesGuidanceUsesTheSelectedRun follows saved source paths
// through Proof, a release-file refusal, and selection between equivalent
// reports. Without promotion settings, neither view exposes a private path.
func TestViewsPageBinariesGuidanceUsesTheSelectedRun(t *testing.T) {
	for _, promotion := range []bool{true, false} {
		name := "without promotion settings"
		if promotion {
			name = "with promotion settings"
		}
		t.Run(name, func(t *testing.T) {
			f := newBinariesPageFixture(t, promotion)
			ps := startPageServer(t, f.cp)
			if promotion {
				ps.act("remove-promotion", f.disablePromotion)
			}
			runPageScript(t, ps, map[string]any{
				"promotion": promotion, "older": f.older, "newer": f.newer,
				"olderDir": f.olderDir, "newerDir": f.newerDir, "digest": f.digest,
			}, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-review-check") && !page.$("#surla-build-review-check").disabled, "the saved current check did not settle");
page.$("#surla-build-review-check").click();
await until(() => page.$("#surla-result-view").dataset.runId === input.newer && page.$("#surla-cp5-approve") && !page.$("#surla-cp5-approve").disabled && page.$("#surla-cp4-run-" + input.older), "the latest proof and selection did not settle");
const folder = () => page.$("#surla-cp4-binaries-dir code")?.textContent;
if (input.promotion) expect(folder() === input.newerDir, "Proof did not show the latest run's recorded output folder");
else expect(!page.$("#surla-cp4-binaries-dir"), "a project without promotion settings exposed a private output folder");
page.$("#surla-cp4-run-" + input.older).click();
await until(() => page.$("#surla-result-view").dataset.runId === input.older && !page.$("#surla-cp5-approve").disabled, "the older selected proof did not settle");
if (!input.promotion) {
  expect(!page.$("#surla-cp4-binaries-dir") && !page.$("#surla-cp5-binaries-dir"), "selection exposed an output folder without promotion settings");
  page.close();
  return;
}
expect(folder() === input.olderDir && !page.text("#surla-cp4-binaries-dir").includes(input.newerDir), "an equivalent report selected another execution's folder");
page.$("#surla-cp5-approve").click();
await until(() => page.$("#surla-cp5-approve-error") && !page.$("#surla-cp5-approve").disabled, "the changed release binaries did not show an approval refusal");
expect(page.text("#surla-cp5-approve-error").includes("whose folder the run's Proof view shows") && page.$("#surla-cp5-binaries-dir code").textContent === input.olderDir, "the refusal did not identify this selected run's output folder");
expect(!page.text("#surla-cp5-approve-error").includes(input.newerDir), "the refusal named the newest execution instead of the selected one");
const posted = page.sent("POST", "/changes/approve");
expect(posted.length === 1 && posted[0].status === 422 && posted[0].body.report_digest === input.digest && Object.keys(posted[0].body).length === 4, "publication guidance changed the approval identity contract");
page.$("#surla-cp4-run-" + input.newer).click();
await until(() => page.$("#surla-result-view").dataset.runId === input.newer && !page.$("#surla-cp5-approve").disabled, "the newer run did not settle after the refusal");
expect(folder() === input.newerDir && !page.$("#surla-cp5-approve-error"), "selection carried another run's refusal or output folder");
page.$("#surla-cp4-run-" + input.older).click();
await until(() => page.$("#surla-result-view").dataset.runId === input.older && page.$("#surla-cp5-binaries-dir"), "returning to the selected refusal lost its guidance");
expect(folder() === input.olderDir && page.$("#surla-cp5-binaries-dir code").textContent === input.olderDir, "returning to saved proof mixed execution paths");
expect(page.sent("POST", "/changes/approve").length === 1, "reading source guidance retried approval");
const verdict = page.text("#surla-cp4-title");
const listPath = "/changes/chg-selected-inputs/verifications";
const lists = page.sent("GET", listPath).length;
await control(origin, "act", { key: "remove-promotion" });
page.$("#surla-load-verifications").click();
await until(() => page.sent("GET", listPath).slice(lists).some((r) => r.status === 200) && !page.$("#surla-load-verifications").disabled && !page.$("#surla-cp4-binaries-dir"), "a fresh owner read did not remove promotion-only metadata from the retained report");
expect(!page.$("#surla-cp5-binaries-dir") && page.text("#surla-cp4-title") === verdict && page.$("#surla-result-view").dataset.runId === input.older, "metadata refresh replaced the selected report or retained a private output path");
page.close();
`)
		})
	}
}

// newBinariesPageFixture uses the ordinary persisted-run fixture and the real
// control-plane projections. Only the promotion target's measurements are
// simulated; no binary is executed or published.
func newBinariesPageFixture(t *testing.T, promotion bool) binariesPageFixture {
	t.Helper()
	cp, card, runsDir := selectedInputsFixture(t)
	f := binariesPageFixture{cp: cp, older: strings.Repeat("1", 32), newer: strings.Repeat("2", 32)}
	for i, runID := range []string{f.older, f.newer} {
		writeListedRun(t, runsDir, pageOwner, card, runID,
			time.Date(2026, 11, 13, 9+i, 0, 0, 0, time.UTC), true)
		setListedRunSource(t, runsDir, runID, verify.TestsWritten)
		workDir := filepath.Join(t.TempDir(), "Retained outputs with spaces", runID)
		editVerificationRequest(t, runsDir, runID, func(req *verifyrun.Request) {
			req.Plan.WorkDir = workDir
		})
		dir := verify.CandidateBinariesDir(workDir)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "orders"), []byte("fixture binary; never executed"), 0o600))
		_, state, err := verifyrun.Read(runsDir, runID)
		require.NoError(t, err)
		if i == 0 {
			f.olderDir, f.digest = dir, state.Report.Digest
		} else {
			f.newerDir = dir
			require.Equal(t, f.digest, state.Report.Digest, "execution identity is not the report digest")
			if promotion {
				target := &promotionTargetStub{
					report: state.Report, artifacts: map[string]string{"orders": strings.Repeat("b", 64)},
				}
				var active atomic.Bool
				active.Store(true)
				cp.resolvePromotionTarget = func(context.Context, string) (PromotionTarget, error) {
					if active.Load() {
						return target, nil
					}
					return nil, nil
				}
				f.disablePromotion = func() { active.Store(false) }
			}
		}
	}
	return f
}
