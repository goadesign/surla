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
	// armed selects just the first read after the Team mutation.
	armed atomic.Bool
	// captured proves the owner's answer existed before replacement began.
	captured chan *gensurla.ChangeBuild
}

// TestViewsPageBuildIgnoresAReadCapturedDuringPendingReplacement preserves a
// successful replacement's saved start and working-copy receipt when a Team
// update read the previous build while the replacement POST was still held.
// This proves page/HTTP behavior, not candidate execution or real window effects.
func TestViewsPageBuildIgnoresAReadCapturedDuringPendingReplacement(t *testing.T) {
	team := &teamServiceFixture{snapshot: pageTeamSnapshot("working")}
	cp := newBuildControlPlane(t, Options{Team: &TeamBinding{Owner: pageOwner, Service: team}})
	cp.changesWait = 400 * time.Millisecond
	cp.changesPoll = 20 * time.Millisecond
	confirmedChange(t, cp, "chg")
	service := &pagePendingBuildRead{ControlPlane: cp, captured: make(chan *gensurla.ChangeBuild, 1)}
	ps := startPageServer(t, service)
	ps.act("new-attention", func() {
		service.armed.Store(true)
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
			t.Error("the Team update did not capture a build answer before replacement")
		}
	})

	runPageScript(t, ps, nil, `
const path = "/changes/chg/build";
const page = await openPage(origin, { link: input.link });
await until(() => page.$("#surla-build-start") && !page.$("#surla-build-start").disabled, "first Build did not become available");
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

// Run the handler now and delay only its old answer, using the normal Team
// mutation -> wait/list update -> get_build path to initiate that read.
await control(origin, "answer-later", { key: "GET " + path });
const reads = page.sent("GET", path).length;
await control(origin, "act", { key: "new-attention" });
await until(() => page.sent("GET", path).length > reads && page.$("#surla-team-attention-builder"), "the real Team update did not send a build read");
const oldRead = page.sent("GET", path)[reads];
await control(origin, "act", { key: "await-old-build-answer" });
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
