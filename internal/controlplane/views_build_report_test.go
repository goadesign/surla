// This file follows real test-writer reports from the served page through the
// production sandboxed checker and the tests branch, without running scenarios.
package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/testmodule"
	"github.com/goadesign/surla/internal/testwriter"
	"github.com/goadesign/surla/verify/candidate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pageReportDesign keeps one ordinary scenario alongside the Story-derived
// RefundOverLimit scenario that production Prepare adds at Build and report.
const pageReportDesign = `package design

import (
	. "goa.design/goa-ai/dsl"
	. "goa.design/goa-ai/eval/dsl"
	. "goa.design/goa/v3/dsl"
)

var tag = Tags

var _ = Service("orders", func() {
	Method("get_order", func() {
		Payload(String)
		Result(String)
		HTTP(func() { GET("/orders/{id}") })
	})
})

var _ = Suite("shop_acceptance", func() {
	Description("Acceptance suite.")
	Timeout("30s")
	Scenario("prices_tea", func() { Description("Tea costs 350 cents.") })
})
`

// pageReportAcceptance supplies the starter's required entry points and the
// existing scenario hook. The Story hook is deliberately absent initially.
const pageReportAcceptance = `// Package acceptance holds trusted scenario hooks.
package acceptance

import (
	"context"
	"errors"

	"goa.design/goa-ai/eval"
	"goa.design/goa-ai/runtime/agent/model"

	"github.com/goadesign/surla/verify/candidate"
	"github.com/goadesign/surla/verify/suite"
)

const Name = "shop"
type Hooks struct{}

func New(candidate.Artifact) *Hooks { return &Hooks{} }

func Live(context.Context, candidate.Artifact, string, model.Provider) (suite.LiveTarget, error) {
	return suite.LiveTarget{}, errors.New("the fixture has no live target")
}

func LiveThroughGateway(context.Context, candidate.Artifact, string, string) (suite.LiveTarget, error) {
	return suite.LiveTarget{}, errors.New("the fixture has no live target")
}

func (h *Hooks) PricesTea(context.Context) (eval.Result, error) { return eval.Result{}, nil }
`

// pageReportRefundHook fixes only the compiler finding. The worker checks its
// presence and declaration; it never runs or judges the hook's assertions.
const pageReportRefundHook = `
func (h *Hooks) RefundOverLimit(context.Context) (eval.Result, error) { return eval.Result{}, nil }
`

// pageRealReportScript drives the served UI and agent HTTP entry points. Each
// verdict arrives through the page's existing version wait, without reloads.
const pageRealReportScript = `
const page = await openPage(origin, { link: input.link });
const buildPath = "/changes/" + input.change + "/build";
const findingsPath = buildPath + "/findings";
const writerPath = "/assignments/test-writer/" + input.change;
function answers(path) {
  return page.sent("GET", path).filter((r) => r.status === 200 && r.answer).map((r) => JSON.parse(r.answer));
}
function shownBuild(state, report) {
  return answers(buildPath).findLast((r) => r.build.state === "started" && r.build.test_writer.tests.state === state && r.build.test_writer.report === report)?.build;
}
async function agent(method, path, body, token) {
  const out = await api(origin, method, path, body, token);
  expect(out.status === 200, method + " " + path + " refused: " + JSON.stringify(out));
  return out.body;
}
async function version(stage) { await control(origin, "act", { key: "version-" + stage }); }
async function report(number) {
  await control(origin, "act", { key: "hold-check" });
  const out = await agent("POST", writerPath + "/finished", { request_revision: input.revision }, input.testWriter);
  expect(out.progress.tests.state === "reported" && out.progress.report === number, "the real report did not return reported: " + JSON.stringify(out));
  await until(() => shownBuild("reported", number), "the page did not follow report " + number);
  await version("reported-" + number);
  await control(origin, "act", { key: "release-check" });
}
await until(() => page.$("#surla-cp1-editor").dataset.changeId === input.change, "the confirmed Story did not open");
await until(() => page.$("#surla-build-start") && !page.$("#surla-build-start").disabled, "the agreed change offered no enabled Build control");
checkControl(page, page.$("#surla-build-start"), "Build control");
page.$("#surla-build-start").click();
await until(() => page.sent("POST", buildPath).some((r) => r.status === 200), "the real Build did not finish preparing the Story tests", 45000);
await until(() => shownBuild("assigned", 0), "the prepared Build did not appear on the page");
const started = shownBuild("assigned", 0);
expect(started.required_test_count === 1 && started.request_revision === input.revision && started.in_force, "Build did not use exactly the complete Story flow: " + JSON.stringify(started));
expect(!started.builder.read && !started.test_writer.read, "new assignments were already read");
await until(() => page.text("#surla-build-builder").includes("Assignment unread") && page.text("#surla-build-test-writer").includes("Assignment unread"), "the page did not show both new assignments as unread");
expect(page.sent("POST", buildPath).length === 1, "the user click did not send exactly one Build");
await version("assigned");

const writerList = await agent("GET", "/assignments/test-writer", undefined, input.testWriter);
const builderList = await agent("GET", "/assignments/builder", undefined, input.builder);
expect(!writerList.assignments[0].progress.read && !builderList.assignments[0].read, "listing assignments marked them read");
const writer = await agent("GET", writerPath, undefined, input.testWriter);
const builder = await agent("GET", "/assignments/builder/" + input.change, undefined, input.builder);
expect(writer.progress.read && builder.read, "full assignment reads did not mark work read");
expect(writer.required_tests.length === 1 && writer.required_tests[0].hook === "RefundOverLimit", "the draft flow changed the required tests: " + JSON.stringify(writer.required_tests));
expect(writer.stories.length === 1 && writer.stories[0].flows.length === 2 && writer.working_folder.length > 0, "the full assignment lacks its saved Story or working folder");
await version("read-1");
await until(() => page.text("#surla-build-builder").includes("Assignment read") && page.text("#surla-build-test-writer").includes("Assignment read"), "full agent reads did not update the page's assignment facts");
const inputSelector = 'textarea[data-field="expected_outcome"]';
const typed = page.$(inputSelector);
page.type(typed, "Keep this unsaved expected outcome");

await report(1);
await until(() => shownBuild("needs_changes", 1) && page.$("#surla-build-findings"), "the real compilation finding did not reach the page", 45000);
const rejected = shownBuild("needs_changes", 1);
expect(rejected.test_writer.tests.finding_count === 1 && !rejected.test_writer.read, "the compilation verdict lacks its count or unread work");
await until(() => answers(findingsPath).some((f) => f.report === 1), "the page did not read the user's finding text");
const findings = answers(findingsPath).findLast((f) => f.report === 1);
expect(findings.change_id === input.change && findings.request_revision === input.revision && findings.started_at === started.started_at && findings.report === 1 && findings.checked_at === rejected.test_writer.tests.checked_at, "finding text was not correlated to all five build/verdict values: " + JSON.stringify(findings));
expect(findings.findings.length === 1 && findings.findings[0].includes("RefundOverLimit") && findings.findings[0].includes("does not compile"), "the finding did not come from the missing-hook compilation: " + JSON.stringify(findings));
await until(() => page.text("#surla-build-findings").includes("RefundOverLimit"), "the user's page showed only a finding count");
expect(page.text("#surla-build-test-writer").includes("Assignment unread"), "the compiler verdict did not show new unread writer work");
expect(page.$(inputSelector) === typed && typed.value === "Keep this unsaved expected outcome", "a worker refresh lost typed input");
await version("needs_changes");

await control(origin, "act", { key: "repair-hook" });
await version("read-2");
await report(2);
await until(() => shownBuild("accepted", 2) && page.$("#surla-build-test-writer").dataset.state === "accepted", "the repaired hook was not accepted on the page", 45000);
const accepted = shownBuild("accepted", 2);
expect(/^[a-f0-9]{40,64}$/.test(accepted.test_writer.tests.commit) && !accepted.builder.read && !accepted.test_writer.read, "acceptance did not record a commit and new unread assignments: " + JSON.stringify(accepted));
expect(page.text("#surla-build-builder").includes("Assignment unread") && page.text("#surla-build-test-writer").includes("Assignment unread"), "acceptance did not show both new unread assignments");
expect(page.text("#surla-build-section").includes(accepted.test_writer.tests.not_judged) && accepted.test_writer.tests.not_judged.includes("did not judge what they assert"), "the page overstated what acceptance proves");
expect(!page.$("#surla-build-findings"), "the superseded finding stayed beside acceptance");
expect(page.$(inputSelector) === typed && typed.value === "Keep this unsaved expected outcome", "acceptance lost typed input");
await version("accepted");
const acceptedWriter = await agent("GET", writerPath, undefined, input.testWriter);
const acceptedBuilder = await agent("GET", "/assignments/builder/" + input.change, undefined, input.builder);
expect(acceptedWriter.progress.read && acceptedWriter.findings.length === 0 && acceptedBuilder.read && acceptedBuilder.written_tests.commit === accepted.test_writer.tests.commit, "full assignment reads did not consume the accepted generation");
page.close();
`

// TestViewsPageFollowsRealTestReports builds a confirmed Story, observes the
// compiler finding for its missing hook, repairs that hook in the assigned
// working folder, and observes the real worker recording the second report.
// Only the external team/window owner is a fixture; generation, checking and
// recording use the production owners and their candidate sandbox unchanged.
func TestViewsPageFollowsRealTestReports(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real sandboxed test generation, compilation and repair; covered by the full suite")
	}
	if err := candidate.CheckSandbox(); err != nil {
		t.Skipf("real report-worker page proof requires the candidate sandbox: %v", err)
	}
	requirePageTools(t)
	repo, project := productionBuildProject(t, ".", pageReportModule(t))
	team := &teamServiceFixture{snapshot: gensurla.TeamSnapshot{
		Team: "shop", Roles: []*gensurla.TeamRoleState{},
	}}
	// Written-test runs make the project verify with the tests a build
	// accepts, so the build uses a test writer; without them the page harness
	// stores verification settings and the build would use none.
	cp := pageControlPlane(t, Options{
		WorkspaceDir: project, Team: &TeamBinding{Owner: pageOwner, Service: team},
		RunsDir:         t.TempDir(),
		WrittenTestRuns: &WrittenTestRuns{ReportDir: t.TempDir(), Command: []string{"/bin/sh", "-c", "exit 0", "sh"}, Timeout: time.Hour},
	})
	card := saveStoryCard(t, cp, "chg-real-report", StatusConfirmed, refundStory("Refund over the limit"))
	user := asCaller(pageOwner, CallerUser)
	before, err := cp.ListChanges(user)
	require.NoError(t, err)
	ps := startPageServer(t, cp)

	// The existing copy turn holds each real check until the script has seen
	// reported. Cleanup releases only a turn this test itself acquired.
	var gateMu sync.Mutex
	held := false
	release := func() {
		gateMu.Lock()
		defer gateMu.Unlock()
		if held {
			<-cp.workingCopyTurn
			held = false
		}
	}
	t.Cleanup(release)
	ps.act("hold-check", func() {
		gateMu.Lock()
		defer gateMu.Unlock()
		if held {
			t.Error("the test already holds the copy turn")
			return
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		select {
		case cp.workingCopyTurn <- struct{}{}:
			held = true
		case <-ctx.Done():
			t.Error("the production checker did not release the copy turn")
		}
	})
	ps.act("release-check", release)
	ps.act("repair-hook", func() {
		assignment, err := cp.GetTestWriterAssignment(asCaller(pageOwner, CallerTestWriter),
			&gensurla.GetTestWriterAssignmentPayload{ChangeID: card.ChangeID})
		require.NoError(t, err)
		require.Equal(t, filepath.Join(project, testwriter.Folder), assignment.WorkingFolder)
		hook := filepath.Join(assignment.WorkingFolder, "eval", "acceptance", "acceptance.go")
		require.NoError(t, os.WriteFile(hook, []byte(pageReportAcceptance+pageReportRefundHook), 0o600))
	})

	// Read the owner's real versions at stable journey points. Reported is
	// held by the copy turn, while verdicts are reached by the page's one wait
	// loop. No card metadata changes during any of these transitions.
	previous := before.Version
	for _, stage := range []string{
		"assigned", "read-1", "reported-1", "needs_changes", "read-2", "reported-2", "accepted",
	} {
		ps.act("version-"+stage, func() {
			list, err := cp.ListChanges(user)
			require.NoError(t, err)
			assert.Equal(t, before.Changes, list.Changes, "report progress must not rewrite the change")
			assert.NotEqual(t, previous, list.Version, "the %s transition must wake the page", stage)
			waited, err := cp.WaitForChanges(user, &gensurla.WaitForChangesPayload{Since: previous})
			require.NoError(t, err)
			assert.Equal(t, list.Version, waited.Version)
			again, err := cp.ListChanges(user)
			require.NoError(t, err)
			assert.Equal(t, list.Version, again.Version, "unchanged reads have a stable version")
			previous = list.Version
		})
	}

	runPageScript(t, ps, map[string]any{
		"change": card.ChangeID, "revision": card.RequestRevision,
	}, viewsPageScriptHelpers+pageRealReportScript)
	build := mustStartedBuild(t, cp, user, card.ChangeID)
	accepted, ok := build.TestWriter.Tests.AsAccepted()
	require.True(t, ok, "the production worker accepted the repaired hook")
	assert.Equal(t, 2, build.TestWriter.Report)
	assert.Equal(t, accepted.Commit, productionBuildGit(t, repo, "rev-parse", "surla/tests"))
	assert.NotEqual(t, productionBuildGit(t, repo, "rev-parse", "HEAD"), accepted.Commit)
	assert.Equal(t, "Add the tests Surla accepted for change "+card.ChangeID+", report 2",
		productionBuildGit(t, repo, "show", "-s", "--format=%s", accepted.Commit))
	assert.Contains(t, productionBuildGit(t, repo, "show", accepted.Commit+":eval/acceptance/acceptance.go"),
		"func (h *Hooks) RefundOverLimit")
	assert.Empty(t, productionBuildGit(t, repo, "status", "--porcelain", "--untracked-files=all"))
}

// pageReportModule prepares the trusted starter-shaped fixture as the checker
// tests do: the acceptance package exists before generation, and the generated
// program's public Surla dependency is pinned before the final tidy. No authored
// binary or scenario is executed here; Generate uses the candidate sandbox.
func pageReportModule(t *testing.T) map[string]string {
	t.Helper()
	module := testmodule.Module(t, "example.com/shop/eval", "", map[string]string{
		"design/design.go": pageReportDesign,
	})
	module["acceptance/acceptance.go"] = pageReportAcceptance
	evaluator := designeval.NewEvaluatorInDir(t.TempDir())
	t.Cleanup(func() { assert.NoError(t, evaluator.Close()) })
	generated, err := evaluator.Generate(t.Context(), module)
	require.NoError(t, err)
	require.Contains(t, generated, "gen/cmd/shop-evals/main.go")
	for name, data := range generated {
		module[name] = string(data)
	}
	module["go.mod"] = strings.Replace(module["go.mod"], "require (",
		"require (\n\tgithub.com/goadesign/surla v0.0.0-20261110163737-d9beac313142", 1)
	return testmodule.Tidy(t, module)
}
