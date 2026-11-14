// This file exercises build reads that overlap a replacement through the served
// page. Working-copy and window effects use the existing simulated boundaries.
package controlplane

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/assert"
)

// pagePendingBuildRead observes the real owner's next build answer before the
// HTTP harness delays its delivery. All other service methods remain real.
type pagePendingBuildRead struct {
	*ControlPlane
	// armed selects the first owner answer after its delivery gate is installed.
	armed atomic.Bool
	// captured proves the old answer existed while replacement was pending,
	// before the owner handles its held POST.
	captured chan *gensurla.ChangeBuild
}

// TestViewsPageBuildIgnoresAReadCapturedDuringPendingReplacement preserves a
// successful replacement's saved start and working-copy receipt when a delayed
// list update reads the previous build while the replacement POST is held. A
// later Team update must coalesce with that read without blocking its own facts.
// This proves page/HTTP behavior, not candidate execution or real window effects.
func TestViewsPageBuildIgnoresAReadCapturedDuringPendingReplacement(t *testing.T) {
	team := &teamServiceFixture{snapshot: pageTeamSnapshot("working")}
	cp := newBuildControlPlane(t, Options{Team: &TeamBinding{Owner: pageOwner, Service: team}})
	cp.changesWait = 400 * time.Millisecond
	cp.changesPoll = 20 * time.Millisecond
	confirmedChange(t, cp, "chg")
	service := &pagePendingBuildRead{ControlPlane: cp, captured: make(chan *gensurla.ChangeBuild, 1)}
	ps := startPageServer(t, service)
	ps.act("hold-old-build-answer", func() {
		// Test controls hold ps.mu, so interception cannot see the gate until
		// the observer is also armed. The owner's next answer will be held.
		ps.gates["GET /changes/chg/build"] = &pageGate{open: make(chan struct{}), answered: true}
		service.armed.Store(true)
	})
	ps.act("new-attention", func() {
		team.mu.Lock()
		team.snapshot = pageTeamSnapshot("needs_you")
		team.mu.Unlock()
	})
	ps.act("await-old-build-answer", func() {
		select {
		case captured := <-service.captured:
			_, started := captured.Build.AsStarted()
			assert.True(t, started, "the delayed owner answer must contain the previous saved build")
		case <-time.After(10 * time.Second):
			t.Error("the delayed list update did not capture a build answer before replacement")
		}
	})

	runPageScript(t, ps, nil, `
const path = "/changes/chg/build";
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-start") && !page.$("#surla-build-start").disabled, "first Build did not become available");
// Keep the first build's version update behind its direct successful answer.
// Releasing it during replacement forces the background-read interleaving.
await control(origin, "answer-later", { key: "GET /changes" });
await until(() => page.$("#surla-build-start") && !page.$("#surla-build-start").disabled, "first Build did not remain ready after delaying its list update");
page.$("#surla-build-start").click();
await until(() => page.$("#surla-build-again") && !page.$("#surla-build-again").disabled && page.$("#surla-build-start-copy-receipt") && page.sent("GET", path).every((r) => r.status === 200), "the first saved build and its read did not finish");
const oldStarted = JSON.parse(page.sent("POST", path)[0].answer).build.started_at;
expect(page.text("#surla-build-details").includes(oldStarted), "the first saved start is missing");

// Hold before the owner handles replacement; its old build remains readable.
await control(origin, "hold", { key: "POST " + path });
await until(() => page.$("#surla-build-again") && !page.$("#surla-build-again").disabled, "the first build's background read did not settle before replacement");
page.$("#surla-build-again").click();
await until(() => page.$("#surla-build-confirm") && !page.$("#surla-build-confirm").disabled, "the replacement confirmation did not become ready");
page.$("#surla-build-confirm").click();
await until(() => page.sent("POST", path).length === 2 && page.$("#surla-build-starting"), "the replacement POST was not held pending");
const replacement = page.sent("POST", path)[1];
expect(replacement.status === undefined, "replacement completed before the race was armed");

// Run the delayed list's normal get_build handler during replacement, holding
// only its answer. Capture is armed before that list can dispatch the read.
await control(origin, "act", { key: "hold-old-build-answer" });
const readsBeforeList = page.sent("GET", path).length;
await until(() => page.sent("GET", "/changes").some((r) => r.status === undefined), "the first build's list update was not held");
await control(origin, "release", { key: "GET /changes" });
await until(() => page.sent("GET", path).length > readsBeforeList && page.sent("GET", path).at(-1).status === undefined, "the first build's delayed list update did not start a held read during replacement");
const reads = page.sent("GET", path).length - 1;
const oldRead = page.sent("GET", path)[reads];
expect(page.requests.indexOf(oldRead) > page.requests.indexOf(replacement), "the held build read began before replacement was pending");
await control(origin, "act", { key: "await-old-build-answer" });
// This real Team mutation follows wait/list updates. Its build refresh may
// coalesce with oldRead; another GET is neither required nor safe to await.
await control(origin, "act", { key: "new-attention" });
await until(() => page.$("#surla-team-attention-builder"), "the real Team update did not reach the page while its build read was held");
expect(oldRead.status === undefined && replacement.status === undefined, "the captured old read or replacement escaped its gate");

await control(origin, "release", { key: "POST " + path });
await until(() => replacement.status === 200 && !page.$("#surla-build-starting") && page.$("#surla-build-start-copy-receipt"), "the replacement did not display its successful receipt");
const newStarted = JSON.parse(replacement.answer).build.started_at;
expect(newStarted !== oldStarted && page.text("#surla-build-details").includes(newStarted), "the replacement did not display its new saved start");
const copyReceipt = page.text("#surla-build-start-copy-receipt");
expect(copyReceipt.includes("the working copy was at the project commit"), "the replacement working-copy receipt is missing");
const regressions = [];
const observe = () => {
  if (page.text("#surla-build-details").includes(oldStarted)) regressions.push("previous started_at flashed");
  if (page.text("#surla-build-start-copy-receipt") !== copyReceipt) regressions.push("replacement working-copy receipt cleared");
};
const observer = new page.win.MutationObserver(observe);
observer.observe(page.$("#surla-build-section"), { childList: true, attributes: true, subtree: true });
await control(origin, "release", { key: "GET " + path });
await until(() => oldRead.status === 200, "the delayed old answer did not arrive");
expect(JSON.parse(oldRead.answer).build.started_at === oldStarted, "the delayed read did not actually contain the previous build");
await until(() => page.sent("GET", path).slice(reads + 1).some((r) => r.status === 200 && JSON.parse(r.answer).build.started_at === newStarted), "the coalesced current read did not finish");
// Flush the delivered answer's promise, render and observer before asserting.
await sleep(0);
observe();
observer.disconnect();
expect(regressions.length === 0, "a read captured during replacement overwrote successful Build facts: " + JSON.stringify(regressions));
expect(page.text("#surla-build-details").includes(newStarted), "the replacement saved start was lost");
expect(page.text("#surla-build-start-copy-receipt") === copyReceipt, "the replacement working-copy receipt was lost");
expect(page.sent("POST", path).length === 2, "the delayed read repeated the replacement effect");
page.close();
`)
}

// GetBuild preserves the owner's result and signals only the armed read. The
// surrounding answer-later gate then holds delivery while StartBuild completes.
func (s *pagePendingBuildRead) GetBuild(
	ctx context.Context,
	p *gensurla.GetBuildPayload,
) (*gensurla.ChangeBuild, error) {
	result, err := s.ControlPlane.GetBuild(ctx, p)
	if err == nil && s.armed.CompareAndSwap(true, false) {
		s.captured <- result
	}
	return result, err
}
