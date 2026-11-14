// This file tests the views page's approval check: reading the card again
// shows whether a recorded approval still matches the promotion target,
// without discarding the user's edits, and a failed read stays visible.
package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
)

// TestCheckpointUIRefreshesApprovalWithoutDiscardingEdits drives the page's
// "Check current approval" control through real get_change requests:
// matching inputs keep the approval and the draft, changed inputs remove it
// while keeping the draft, and a failed read keeps both and can be retried.
func TestCheckpointUIRefreshesApprovalWithoutDiscardingEdits(t *testing.T) {
	spec := reportSpec{
		SuiteID: "orders_acceptance", SuiteCommit: strings.Repeat("2", 40),
		Environment: "sim", Artifacts: map[string]string{"orders": strings.Repeat("a", 64)},
	}
	target := &pageTarget{artifacts: spec.Artifacts}
	cp := pageControlPlane(t, Options{ResolvePromotionTarget: target.resolve})
	guide := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: "guide", Kind: CallerGuide})
	user := WithPrincipal(context.Background(), Principal{ID: pageOwner, CallerID: pageUserID, Kind: CallerUser})
	proposed := mustPropose(t, cp, guide, "approval-refresh", "Refunds above $50 require a manager.", refundAnalysis("Refunds above $50 require a manager."))
	confirmed, err := cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
		ChangeID: proposed.ChangeID, RequestRevision: proposed.RequestRevision,
		Answers: map[string]string{}, Examples: proposed.Examples,
	})
	require.NoError(t, err)
	commit := strings.Repeat("1", 40)
	report := passingReport(t, confirmed.RequestRevision, commit, spec)
	target.update(func(p *pageTarget) { p.report = report })
	_, err = cp.ApproveMerge(user, &gensurla.ApproveMergePayload{
		ChangeID: confirmed.ChangeID, RequestRevision: confirmed.RequestRevision,
		CandidateCommit: commit, ReportDigest: report.Digest,
	})
	require.NoError(t, err)
	ps := startPageServer(t, cp)
	ps.act("inputs-changed", func() {
		target.update(func(p *pageTarget) {
			p.artifacts = map[string]string{"orders": strings.Repeat("c", 64)}
		})
	})
	runPageScript(t, ps, nil, `
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-cp5-approval"), "the recorded approval is not shown");
const field = () => page.$('textarea[data-field="expected_outcome"]');
const reads = () => page.sent("GET", "/changes/approval-refresh");
page.type(field(), "Keep this draft");
const before = reads().length;
page.$("#surla-cp5-refresh-approval").click();
await until(() => reads().length === before + 1 && !page.$("#surla-cp5-refresh-approval").disabled, "checking the approval did not read the card once");
expect(page.$("#surla-cp5-approval"), "matching inputs removed the approval");
expect(field().value === "Keep this draft", "matching approval reset the draft");
expect(!page.doc.body.textContent.includes("Approved for promotion"), "recorded snapshot claims continuing authority");

// A failed read is visible, keeps the record and the draft, and can be retried.
await control(origin, "reply", { key: "GET /changes/approval-refresh", status: 500, body: { name: "fault", id: "x", message: "Target inputs are unavailable", temporary: false, timeout: false, fault: true } });
page.$("#surla-cp5-refresh-approval").click();
await until(() => page.$("#surla-confirm-feedback [role=alert]"), "the failed read is not visible");
expect(page.text("#surla-confirm-feedback [role=alert]").includes("Target inputs are unavailable"), "the server's error is not shown");
expect(page.$("#surla-cp5-approval"), "a failed read discarded the recorded check");
expect(field().value === "Keep this draft", "a failed read erased the draft");

// After the target's inputs change, retrying removes the approval and keeps the draft.
await control(origin, "act", { key: "inputs-changed" });
const beforeRetry = reads().length;
page.$("#surla-load-latest-card").click();
await until(() => reads().length === beforeRetry + 1 && !page.$("#surla-cp5-approval"), "retrying did not read the card and drop the stale approval");
expect(field().value === "Keep this draft", "the retried read erased the draft");
await sleep(200);
expect(reads().length === beforeRetry + 1, "the approval check looped");
page.close();
`)
}
