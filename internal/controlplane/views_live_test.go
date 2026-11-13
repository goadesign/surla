// This file drives the views page's Live target view through the real page
// harness: the live target is the fake target started through the real
// `verify-run live` path, the builder agent starts it and sends drafts over
// HTTP with its own credential, and the page follows through its one waiting
// read.
package controlplane

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	gensurla "github.com/goadesign/surla/gen/surla"
)

// liveScriptHelpers are the page-script helpers the Live target view's tests
// share: opening the view, and the builder agent's start and draft calls.
const liveScriptHelpers = `
const commitA = "a".repeat(40);
async function openLive(page) {
  await until(() => page.$("#surla-nav-live"), "the Live target tab is missing");
  page.$("#surla-nav-live").click();
  await until(() => page.state() === "live", "the Live target view did not open");
  await until(() => page.$("#surla-live-target") && page.$("#surla-live-target").dataset.state !== "unread", "the live target was never read");
}
async function startTarget(commit) {
  const out = await api(origin, "POST", "/live-target", { candidate_commit: commit }, input.builder);
  expect(out.status === 200, "the builder could not start a live target: " + JSON.stringify(out));
  return out.body;
}
async function draftPrompt(text) {
  const out = await api(origin, "POST", "/sandbox/draft", { target_kind: "prompt_template", prompt_id: "homes.helper.system", new_content: text }, input.builder);
  expect(out.status === 200, "the builder's draft was refused: " + JSON.stringify(out));
  return out.body;
}
function liveState(page) {
  return page.$("#surla-live-target").dataset.state;
}
`

// TestViewsPageLiveTargetFollowsAStartDraftsAndAnEnd opens the Live target
// view with nothing running, then follows, without a reload and through the
// one waiting read, the builder agent starting a target, two drafts of the
// same prompt, and the target ending by itself. It checks the sentences,
// the link, the announcements, that the second draft replaces the first
// entry, that no prompt text appears, that text typed in the model form
// survives every refresh, that the live target is read only after the
// version moved, and that the diagnostics history holds no waiting read.
func TestViewsPageLiveTargetFollowsAStartDraftsAndAnEnd(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	cp := pageLiveControlPlane(t, "serve", Options{TracerProvider: provider}, noModel())
	ps := startPageServer(t, cp)
	runPageScript(t, ps, nil, liveScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await openLive(page);
expect(liveState(page) === "none", "a fresh server shows " + liveState(page));
expect(page.text("#surla-live-none").includes("The builder agent starts one from a commit to try prompt wording on it."), "the empty state does not say what starts a live target: " + page.text("#surla-live-none"));
expect(!page.text("#surla-live-stage").includes("start_live_target"), "a sentence names an operation");
expect(!page.$("#surla-live-stop"), "Stop is offered with nothing running");
const typed = page.$("#surla-model-name");
page.type(typed, "typed-but-not-applied");

const started = await startTarget(commitA);
await until(() => liveState(page) === "running", "a started target did not appear without a reload");
expect(page.text("#surla-live-running").includes(commitA), "the running target does not name its commit");
const link = page.$("#surla-live-address");
expect(link.tagName === "A" && link.getAttribute("href") === new URL(started.url).origin, "the local address is not a link to its origin: " + link.outerHTML);
expect(link.getAttribute("target") === "_blank" && link.getAttribute("rel") === "noopener noreferrer", "the link does not open a new tab safely: " + link.outerHTML);
expect(page.$("#surla-live-running time").getAttribute("datetime").length > 0, "the start time is not machine-readable");
expect(page.$("#surla-live-no-prompts"), "a target with no drafts does not say so");
expect(page.text("#surla-live-drafts-note").includes("discarded when this live target stops") && page.text("#surla-live-drafts-note").includes("committed"), "the drafts note is missing");
await until(() => page.text("#surla-live-announcement").includes("is running"), "the start was not announced: " + page.text("#surla-live-announcement"));
expect(page.$("#surla-live-announcement").getAttribute("role") === "status", "the announcements are not in a polite live region");

const first = await draftPrompt("Answer in French.");
await until(() => page.text("#surla-live-prompts").includes(first.prompt_version), "an accepted draft did not appear without a reload");
await until(() => page.text("#surla-live-announcement").includes("homes.helper.system now uses version " + first.prompt_version), "the draft was not announced: " + page.text("#surla-live-announcement"));
const second = await draftPrompt("Answer in Spanish.");
await until(() => page.text("#surla-live-prompts").includes(second.prompt_version), "a second draft did not appear without a reload");
expect(page.$$("#surla-live-prompts li").length === 1, "a second draft of the same prompt added an entry");
expect(!page.text("#surla-live-prompts").includes(first.prompt_version), "the first draft's version is still shown");
expect(!page.text("#surla-live-stage").includes("Answer in"), "prompt text appeared on the page");
expect(page.$("#surla-model-name") === typed && typed.value === "typed-but-not-applied", "a refresh lost the model form's typed text");

await fetch(started.url + "/exit", { method: "POST" }).catch(() => {});
await until(() => liveState(page) === "none", "a target that ended by itself is still shown", 30000);
await until(() => page.text("#surla-live-announcement") === "No live target is running.", "the end was not announced: " + page.text("#surla-live-announcement"));
expect(page.$("#surla-model-name") === typed && typed.value === "typed-but-not-applied", "a refresh lost the model form's typed text");

const reads = page.sent("GET", "/live-target").length;
const lists = page.sent("GET", "/changes").length;
expect(reads <= lists + 1, "the live target was read " + reads + " times for " + lists + " list reads: it is read only when the version moves");
const others = page.requests.filter((r) => !["/changes", "/change-updates", "/live-target", "/cp1/model", "/project/browser-session"].includes(r.path));
expect(others.length === 0, "the page sent other requests: " + JSON.stringify(others.map((r) => r.method + " " + r.path)));
const history = await api(origin, "GET", "/traces?status=all&limit=100", undefined, input.guide);
expect(history.status === 200, "diagnostics could not be read: " + JSON.stringify(history));
expect(!history.body.spans.some((span) => span.name.includes("wait_for_changes")), "a waiting read was saved in the diagnostics history");
page.close();
`)
}

// TestViewsPageLiveTargetShowsAStartInProgress draws a start that is still
// building: the commit, since when, that it cannot be stopped yet, no Stop
// control, and an announcement.
func TestViewsPageLiveTargetShowsAStartInProgress(t *testing.T) {
	cp := pageLiveControlPlane(t, "prepare-hangs", Options{}, noModel())
	ps := startPageServer(t, cp)
	runPageScript(t, ps, nil, liveScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await openLive(page);
void api(origin, "POST", "/live-target", { candidate_commit: commitA }, input.builder);
await until(() => liveState(page) === "starting", "a start in progress was not drawn");
expect(page.text("#surla-live-starting").includes(commitA) && page.text("#surla-live-starting").includes("cannot be stopped until the start finishes"), "the starting state is not described: " + page.text("#surla-live-starting"));
expect(page.$("#surla-live-starting time"), "the start time is missing");
expect(!page.$("#surla-live-stop"), "Stop is offered while the start builds");
await until(() => page.text("#surla-live-announcement").includes("is starting from commit"), "the start was not announced");
page.close();
`)
	// Close ends the start the script left building, so the test server's
	// own close does not wait for that request.
	require.NoError(t, cp.Close())
}

// TestViewsPageStopsTheLiveTargetAfterConfirming checks the stop control
// from the keyboard's side: opening the confirmation moves focus to it,
// Escape and "Keep it running" return focus to Stop and send nothing, and
// "Stop it" sends exactly one stop request, shows the server's message, and
// leaves the panel saying none is running.
func TestViewsPageStopsTheLiveTargetAfterConfirming(t *testing.T) {
	cp := pageLiveControlPlane(t, "serve", Options{}, noModel())
	ps := startPageServer(t, cp)
	runPageScript(t, ps, nil, liveScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await openLive(page);
await startTarget(commitA);
await until(() => liveState(page) === "running", "the started target did not appear");
const stop = () => page.$("#surla-live-stop");
expect(stop() && stop().tagName === "BUTTON" && stop().textContent === "Stop live target", "the Stop control is missing");

stop().click();
await until(() => page.$("#surla-live-stop-confirm"), "Stop did not ask first");
const confirm = page.$("#surla-live-stop-confirm");
await until(() => page.doc.activeElement === confirm, "opening the confirmation did not move focus to it");
expect(page.text("#surla-live-stop-question").includes("It ends now, and every prompt draft applied to it is discarded"), "the confirmation does not say what stopping does: " + page.text("#surla-live-stop-question"));
expect(!stop(), "Stop is still offered beside its confirmation");
confirm.dispatchEvent(new page.win.KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
await until(() => !page.$("#surla-live-stop-confirm") && stop() && page.doc.activeElement === stop(), "Escape did not close the confirmation and return focus to Stop");

stop().click();
await until(() => page.$("#surla-live-stop-keep"), "Stop did not ask again");
page.$("#surla-live-stop-keep").click();
await until(() => !page.$("#surla-live-stop-confirm") && stop() && page.doc.activeElement === stop(), "Keep it running did not return focus to Stop");
expect(page.sent("DELETE", "/live-target").length === 0, "a stop was sent before the user confirmed");

stop().click();
await until(() => page.$("#surla-live-stop-yes"), "Stop did not ask a third time");
page.$("#surla-live-stop-yes").click();
await until(() => page.$("#surla-live-stop-outcome"), "the stop's answer was not shown");
expect(page.text("#surla-live-stop-outcome") === "The live target is stopped; its prompt drafts are discarded and draft_override is no longer bound to it.", "the operation's message is not shown as it is: " + page.text("#surla-live-stop-outcome"));
await until(() => liveState(page) === "none", "the stopped target is still shown");
expect(page.sent("DELETE", "/live-target").length === 1, "the stop was sent " + page.sent("DELETE", "/live-target").length + " times");
expect(page.$("#surla-live-stop-outcome"), "the stop's message went before the state changed");
await startTarget(commitA);
await until(() => liveState(page) === "running" && !page.$("#surla-live-stop-outcome"), "the stop's message stayed beside a target started afterwards");
page.close();
`)
}

// TestViewsPageLiveTargetEndingClosesTheConfirmation lets a target end by
// itself twice: once with the stop confirmation open, which closes, and once
// with focus on the Stop control and no confirmation. Each time nothing is
// sent and focus goes to the view's heading, not the page body.
func TestViewsPageLiveTargetEndingClosesTheConfirmation(t *testing.T) {
	cp := pageLiveControlPlane(t, "serve", Options{}, noModel())
	ps := startPageServer(t, cp)
	runPageScript(t, ps, nil, liveScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await openLive(page);
const started = await startTarget(commitA);
await until(() => liveState(page) === "running", "the started target did not appear");
page.$("#surla-live-stop").click();
await until(() => page.doc.activeElement === page.$("#surla-live-stop-confirm"), "the confirmation did not take focus");
await fetch(started.url + "/exit", { method: "POST" }).catch(() => {});
await until(() => liveState(page) === "none", "a target that ended by itself is still shown", 30000);
await until(() => !page.$("#surla-live-stop-confirm"), "the confirmation stayed open for a target that ended");
await until(() => page.doc.activeElement === page.$("#surla-live-target-title"), "focus did not go to the view's heading: " + page.doc.activeElement.outerHTML.slice(0, 80));

const again = await startTarget(commitA);
await until(() => liveState(page) === "running" && page.$("#surla-live-stop"), "the second target did not appear");
page.$("#surla-live-stop").focus();
expect(page.doc.activeElement === page.$("#surla-live-stop"), "the Stop control did not take focus");
await fetch(again.url + "/exit", { method: "POST" }).catch(() => {});
await until(() => liveState(page) === "none", "the second target that ended is still shown", 30000);
await until(() => page.doc.activeElement === page.$("#surla-live-target-title"), "focus left on Stop did not go to the view's heading: " + page.doc.activeElement.outerHTML.slice(0, 80));
expect(page.sent("DELETE", "/live-target").length === 0, "a stop was sent although the user never confirmed");
page.close();
`)
}

// TestViewsPageClearsARefusedStopWhenTheStateChanges has the server refuse
// a stop, shows the refusal, keeps it while the target's state is the one
// the refusal left, and clears it once the target ends.
func TestViewsPageClearsARefusedStopWhenTheStateChanges(t *testing.T) {
	cp := pageLiveControlPlane(t, "serve", Options{}, noModel())
	ps := startPageServer(t, cp)
	runPageScript(t, ps, nil, liveScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await openLive(page);
const started = await startTarget(commitA);
await until(() => liveState(page) === "running", "the started target did not appear");
await control(origin, "reply", { key: "DELETE /live-target", status: 500, body: { name: "live_target_stop_failed", id: "x", message: "The stop could not be completed for this test.", temporary: false, timeout: false, fault: false } });
page.$("#surla-live-stop").click();
await until(() => page.$("#surla-live-stop-yes"), "Stop did not ask first");
page.$("#surla-live-stop-yes").click();
await until(() => page.text("#surla-live-stop-outcome") === "The stop could not be completed for this test.", "the refusal was not shown");
await until(() => page.sent("GET", "/live-target").length >= 2, "the page did not read the live target after the refusal");
await sleep(600);
expect(page.$("#surla-live-stop-outcome"), "the refusal went while the state stayed the same");
await fetch(started.url + "/exit", { method: "POST" }).catch(() => {});
await until(() => liveState(page) === "none", "a target that ended by itself is still shown", 30000);
await until(() => !page.$("#surla-live-stop-outcome"), "the refusal stayed after the state changed");
page.close();
`)
}

// TestViewsPageLiveTargetNotConfirmedStopped makes a stop fail and checks
// that the page shows the server's message, draws the target as not
// confirmed stopped with the server's step, and offers no Stop: a target's
// stop runs once, so a retry could never succeed. Making the stop fail needs
// macOS's chflags, as TestAFailedStopKeepsTheOwnerAndStartsNoOtherTarget
// does.
func TestViewsPageLiveTargetNotConfirmedStopped(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("making a stop fail needs macOS's chflags")
	}
	self, err := os.Executable()
	require.NoError(t, err)
	cardsDir := t.TempDir()
	var pinned string
	// Registered first so it runs last: after Close, unpin the file and
	// remove the cards directory the failed stop left behind.
	t.Cleanup(func() {
		if pinned != "" {
			require.NoError(t, exec.Command("chflags", "nouchg", pinned).Run())
		}
		require.NoError(t, os.RemoveAll(cardsDir))
	})
	cp, err := New(Options{
		CardsDir:     cardsDir,
		Verification: &VerificationConfig{RunsDir: t.TempDir()},
		Live: &LiveConfig{
			Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=serve"),
			StartTimeout: time.Minute, Model: noModel(),
		},
	})
	require.NoError(t, err)
	cp.changesWait = 400 * time.Millisecond
	cp.changesPoll = 20 * time.Millisecond
	t.Cleanup(func() { assert.Error(t, cp.Close(), "Close reports the target it could not confirm stopped") })
	ps := startPageServer(t, cp)
	ps.act("pin", func() {
		lt := cp.boundLive()
		require.NotNil(t, lt)
		pinned = filepath.Join(lt.proc.Dir(), "tmp", "immutable")
		require.NoError(t, os.WriteFile(pinned, []byte("x"), 0o600))
		require.NoError(t, exec.Command("chflags", "uchg", pinned).Run())
	})
	runPageScript(t, ps, map[string]any{"step": msgStopFailed}, liveScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await openLive(page);
await startTarget(commitA);
await until(() => liveState(page) === "running", "the started target did not appear");
await control(origin, "act", { key: "pin" });
page.$("#surla-live-stop").click();
await until(() => page.$("#surla-live-stop-yes"), "Stop did not ask first");
page.$("#surla-live-stop-yes").click();
await until(() => page.$("#surla-live-stop-outcome"), "the stop's refusal was not shown");
expect(page.text("#surla-live-stop-outcome") === input.step, "the server's message is not shown as it is: " + page.text("#surla-live-stop-outcome"));
expect(page.$("#surla-live-stop-outcome").getAttribute("role") === "alert", "the refusal is not an alert");
await until(() => liveState(page) === "not_confirmed_stopped", "the target is not drawn as not confirmed stopped: " + liveState(page));
expect(page.text("#surla-live-message") === input.step, "the read's step is not shown: " + page.text("#surla-live-message"));
expect(page.text("#surla-live-target").includes(commitA), "the stuck target's commit is not shown");
expect(!page.$("#surla-live-stop") && !page.$("#surla-live-stop-confirm"), "Stop is offered for a target whose stop already ran");
await until(() => page.text("#surla-live-announcement") === "The live target could not be confirmed stopped.", "the failed stop was not announced");
page.close();
`)
}

// TestViewsPageLiveTargetLinksOnlyALocalAddressAndWrapsLongText answers the
// live target read with raw server answers: an address on another host, a
// loopback address with a path, and a plain loopback origin; only the last
// is a link. Long prompt identifiers, versions and a long server message are
// set to wrap inside the card, so they cannot widen the page.
func TestViewsPageLiveTargetLinksOnlyALocalAddressAndWrapsLongText(t *testing.T) {
	cp := pageLiveControlPlane(t, "serve", Options{}, noModel())
	ps := startPageServer(t, cp)
	longID := "orders.refund." + strings.Repeat("system", 40)
	longVersion := "sha256:" + strings.Repeat("0123456789abcdef", 16)
	running := func(url string) string {
		return `{"live_target":{"state":"running","candidate_commit":"` + strings.Repeat("a", 40) + `","url":"` + url +
			`","started_at":"2026-11-13T14:00:00Z","prompts":[{"prompt_id":"` + longID + `","version":"` + longVersion +
			`","accepted_at":"2026-11-13T14:05:00Z"}]}}`
	}
	longMessage := strings.Repeat("The live target's processes could not be confirmed stopped. ", 12)
	runPageScript(t, ps, map[string]any{
		"answers": []string{
			running("http://example.com:8080"),
			running("http://127.0.0.1:53114/admin"),
			running("http://127.0.0.1:53114"),
			`{"live_target":{"state":"not_confirmed_stopped","candidate_commit":"` + strings.Repeat("b", 40) + `","message":"` + longMessage + `"}}`,
		},
		"longID": longID, "longVersion": longVersion,
	}, liveScriptHelpers+`
for (const body of input.answers) {
  await control(origin, "reply", { key: "GET /live-target", status: 200, body: JSON.parse(body) });
}
const links = ["", "-2", "-3", "-4"];
const pages = [];
for (const suffix of links) {
  const page = await openPage(origin, { link: input.link + suffix });
  await openLive(page);
  pages.push(page);
}
const address = (page) => page.$("#surla-live-address");
expect(address(pages[0]).tagName === "CODE", "an address on another host is a link: " + address(pages[0]).outerHTML);
expect(address(pages[1]).tagName === "CODE", "a loopback address with a path is a link: " + address(pages[1]).outerHTML);
expect(address(pages[2]).tagName === "A" && address(pages[2]).getAttribute("href") === "http://127.0.0.1:53114", "a plain loopback origin is not a link: " + address(pages[2]).outerHTML);
const wraps = (page, element, label) => {
  expect(element, label + " is missing");
  expect(page.win.getComputedStyle(element).overflowWrap === "anywhere", label + " does not wrap inside the card");
};
wraps(pages[0], pages[0].$(".surla-live-prompt-id"), "a long prompt identifier");
expect(pages[0].text(".surla-live-prompt-id") === input.longID, "the prompt identifier is not shown");
wraps(pages[0], pages[0].$(".surla-live-version"), "a long version");
wraps(pages[0], address(pages[0]), "an address shown as text");
wraps(pages[2], address(pages[2]), "an address shown as a link");
expect(pages[3].$("#surla-live-target").dataset.state === "not_confirmed_stopped", "the answer was not drawn as not confirmed stopped");
wraps(pages[3], pages[3].$("#surla-live-message"), "a long server message");
for (const page of pages) {
  page.close();
}
`)
}

// TestViewsPageModelConnectionForm shows the start-up choice with the form
// open on its kind, only that kind's fields displayed and the shared field
// styles, keeps typed text when the user leaves the view and comes back, has
// a provider whose key is missing refused with the variable named and the
// choice unchanged, applies a gateway and shows the new choice, opens a new
// page on the gateway kind, and checks that no answer the page received,
// nothing it shows and nothing in the diagnostics history holds the
// provider key.
func TestViewsPageModelConnectionForm(t *testing.T) {
	const secret = "sk-page-test-secret-value"
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	cp := pageLiveControlPlane(t, "serve", Options{
		TracerProvider: provider,
		LiveModelEnv: func(choice gensurla.Connection) ([]string, error) {
			if direct, ok := choice.AsDirect(); ok && direct.Provider == "openai" {
				return nil, gensurla.MakeUnprocessable(errors.New(
					"provider openai requires OPENAI_API_KEY in the project server's environment; set OPENAI_API_KEY and stop and start Surla"))
			}
			return modelEnv([]string{"ANTHROPIC_API_KEY=" + secret}, choice), nil
		},
	}, directModel("anthropic", "claude-sonnet-4-6"), "ANTHROPIC_API_KEY="+secret)
	ps := startPageServer(t, cp)
	runPageScript(t, ps, map[string]any{"secret": secret}, liveScriptHelpers+`
const page = await openPage(origin, { link: input.link });
await openLive(page);
await until(() => page.text("#surla-model-current").includes("anthropic / claude-sonnet-4-6"), "the start-up choice is not shown: " + page.text("#surla-model-current"));
expect(page.text("#surla-model-current").startsWith("Live targets started from now on use"), "the choice does not say what it applies to");
expect(page.text("#surla-model-note").includes("live targets started afterwards") && page.text("#surla-model-note").includes("verification is not affected"), "the note does not say what the choice changes: " + page.text("#surla-model-note"));
const shown = (p, id) => p.win.getComputedStyle(p.$(id)).display !== "none";
expect(page.$("input[name='surla-model-kind'][value='direct']").checked, "the form does not open on the current choice's kind");
expect(shown(page, "#surla-model-direct-fields") && !shown(page, "#surla-model-gateway-fields"), "the unused kind's fields are displayed");
for (const id of ["#surla-model-provider", "#surla-model-name", "#surla-model-gateway"]) {
  expect(page.$(id).classList.contains("surla-input") && page.$(id).closest("label").classList.contains("surla-field-label"), id + " does not use the shared field styles");
}
const apply = page.$("#surla-model-apply");
expect(apply.tagName === "BUTTON" && apply.type === "submit" && apply.classList.contains("shadcn-btn-primary"), "Apply is not the view's primary submit button");
const select = page.$("#surla-model-provider");
select.value = "openai";
select.dispatchEvent(new page.win.Event("change", { bubbles: true }));
page.type(page.$("#surla-model-name"), "gpt-5.4-mini");
page.$("#surla-nav-story").click();
await until(() => page.state() === "story" && !page.$("#surla-model-form"), "the Story view did not open");
page.$("#surla-nav-live").click();
await until(() => page.$("#surla-model-name"), "the Live target view did not open again");
expect(page.$("#surla-model-name").value === "gpt-5.4-mini" && page.$("#surla-model-provider").value === "openai", "leaving the view lost the typed text: " + page.$("#surla-model-name").value + " / " + page.$("#surla-model-provider").value);
page.$("#surla-model-apply").click();
await until(() => page.$("#surla-model-outcome"), "the refusal was not shown");
expect(page.text("#surla-model-outcome").includes("OPENAI_API_KEY"), "the refusal does not name the missing variable: " + page.text("#surla-model-outcome"));
expect(page.$("#surla-model-outcome").getAttribute("role") === "alert", "the refusal is not an alert");
expect(page.text("#surla-model-current").includes("anthropic / claude-sonnet-4-6"), "a refused change replaced the shown choice");

page.$("input[name='surla-model-kind'][value='gateway']").click();
await until(() => shown(page, "#surla-model-gateway-fields"), "the gateway fields were not displayed");
expect(!shown(page, "#surla-model-direct-fields"), "the provider fields are still displayed beside the gateway's");
page.type(page.$("#surla-model-gateway"), "127.0.0.1:9090");
page.$("#surla-model-apply").click();
await until(() => page.text("#surla-model-outcome") === "Live targets started from now on use the model gateway at 127.0.0.1:9090.", "the accepted change was not shown: " + page.text("#surla-model-outcome"));
await until(() => page.text("#surla-model-current").includes("the model gateway at 127.0.0.1:9090"), "the new choice was not read again");
expect(page.sent("POST", "/cp1/model").length === 2, "each Apply should send one request");

const second = await openPage(origin, { link: input.link + "-2" });
await openLive(second);
await until(() => second.text("#surla-model-current").includes("127.0.0.1:9090"), "a new page did not read the gateway choice");
expect(second.$("input[name='surla-model-kind'][value='gateway']").checked && shown(second, "#surla-model-gateway-fields") && !shown(second, "#surla-model-direct-fields"), "a new page does not open on the current choice's kind");

const answered = [...page.requests, ...second.requests].filter((r) => r.answer !== undefined);
expect(answered.some((r) => r.path === "/cp1/model" && r.method === "GET") && answered.some((r) => r.path === "/cp1/model" && r.method === "POST" && r.status === 422), "the scan did not see the model reads and the refusal");
for (const r of answered) {
  expect(!r.answer.includes(input.secret), "the answer to " + r.method + " " + r.path + " holds the provider key");
}
expect(!page.doc.body.textContent.includes(input.secret) && !second.doc.body.textContent.includes(input.secret), "a page shows the provider key");
second.close();
const history = await api(origin, "GET", "/traces?status=all&limit=100", undefined, input.guide);
expect(history.status === 200 && !JSON.stringify(history.body).includes(input.secret), "the diagnostics history holds the provider key");
page.close();
`)
	for _, span := range exporter.GetSpans().Snapshots() {
		assertSpanNeverContainsValue(t, span, secret)
	}
}

// TestViewsPageLiveAndModelReadsRefuseOtherRoles checks over the page's
// server that the guide and the test writer are refused both reads, and
// that the builder may read the live target but not the model connection.
func TestViewsPageLiveAndModelReadsRefuseOtherRoles(t *testing.T) {
	cp := pageLiveControlPlane(t, "serve", Options{}, noModel())
	ps := startPageServer(t, cp)
	runPageScript(t, ps, nil, liveScriptHelpers+`
for (const [token, who] of [[input.guide, "the guide"], [input.testWriter, "the test writer"]]) {
  for (const path of ["/live-target", "/cp1/model"]) {
    const out = await api(origin, "GET", path, undefined, token);
    expect(out.status === 403, who + " was not refused GET " + path + ": " + JSON.stringify(out));
  }
}
const live = await api(origin, "GET", "/live-target", undefined, input.builder);
expect(live.status === 200 && live.body.live_target.state === "none", "the builder could not read the live target: " + JSON.stringify(live));
const model = await api(origin, "GET", "/cp1/model", undefined, input.builder);
expect(model.status === 403, "the builder was not refused the model connection: " + JSON.stringify(model));
`)
}

// pageLiveControlPlane returns a page control plane whose live targets are
// the fake target started through the real `verify-run live` path in mode,
// starting from the model choice and the extra environment env it became.
// Its waiting read answers quickly.
func pageLiveControlPlane(t *testing.T, mode string, opts Options, choice gensurla.Connection, env ...string) *ControlPlane {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	opts.Verification = &VerificationConfig{RunsDir: t.TempDir()}
	opts.Live = &LiveConfig{
		Command:      []string{self},
		Env:          modelEnv(append(os.Environ(), append(env, liveTargetModeEnv+"="+mode)...), choice),
		StartTimeout: time.Minute,
		Model:        choice,
	}
	return pageControlPlane(t, opts)
}
