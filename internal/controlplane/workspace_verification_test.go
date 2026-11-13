// This file checks which tests verification runs and live copies use in a
// project without stored verification settings: a run uses the tests Surla
// accepted for the change's build, at their commit, and a live copy uses the
// candidate commit's own test module. Both read the layout `surla init` makes
// from a real git repository, and both refuse a commit that lacks it.
package controlplane

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goa "goa.design/goa/v3/pkg"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
)

type (
	// layoutRepo is a git repository holding the shop project in
	// services/shop, with commits in the states the tests below need.
	layoutRepo struct {
		// workspace is the project folder the control plane is bound to.
		workspace string
		// tests is a commit with the whole layout, whose tests are accepted.
		tests string
		// candidate is a later builder commit with the whole layout.
		candidate string
		// noEntry lacks the tests' generated entry point.
		noEntry string
		// noProgram lacks the service's command package.
		noProgram string
	}
)

// shopLayout is the layout `surla init` makes for example.com/shop, with a
// hand-made generated entry point at the path the generator writes it to.
var shopLayout = map[string]string{
	"go.mod":                          "module example.com/shop\n\ngo 1.26\n",
	"cmd/shop/main.go":                "package main\n\nfunc main() {}\n",
	"eval/go.mod":                     "module example.com/shop/eval\n\ngo 1.26\n",
	"eval/gen/cmd/shop-evals/main.go": "package main\n\nfunc main() {}\n",
}

// TestVerificationUsesTheAcceptedTestsAndLiveCopiesTheCandidatesOwn verifies
// what a project without stored verification settings saves for each consumer
// of a commit's layout, on the server that accepted the tests and on one
// reopened from the same card store: a live copy of a candidate needs no
// build and runs that candidate's own generated entry point; verify_change is
// refused until the user built the revision and Surla accepted its tests,
// then runs the accepted commit's entry point against the later candidate;
// and every result says where the tests came from. The live target is this
// test binary and the run's supervisor a shell that exits at once, so no Go
// build runs.
func TestVerificationUsesTheAcceptedTestsAndLiveCopiesTheCandidatesOwn(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	root := realTempDir(t)
	repo := newLayoutRepo(t, filepath.Join(root, "repo"))
	runsDir := filepath.Join(root, "ops", "runs")
	options := func() Options {
		return Options{
			CardsDir:     filepath.Join(root, "ops", "cards"),
			WorkspaceDir: repo.workspace,
			RunsDir:      runsDir,
			SurlaVersion: testSurlaVersion,
			WrittenTestRuns: &WrittenTestRuns{
				ReportDir: filepath.Join(root, "ops", "reports"),
				// The supervisor exits at once; only the saved request is
				// under test.
				Command: []string{"/bin/sh", "-c", "exit 0", "sh"},
				Timeout: time.Hour,
			},
			Live: &LiveConfig{
				Command:      []string{self},
				Env:          append(os.Environ(), liveTargetModeEnv+"=serve"),
				StartTimeout: time.Minute,
			},
		}
	}
	user := asCaller("alice@example.com", CallerUser)
	builder := asCaller("alice@example.com", CallerBuilder)
	writtenTests := gensurla.NewTestSourceWrittenTests(&gensurla.AgentWrittenTests{Commit: repo.tests, Notice: writtenTestsNotice})

	first, err := New(options())
	require.NoError(t, err)
	// Close is idempotent: this also covers a failure before the explicit
	// Close below.
	t.Cleanup(func() { assert.NoError(t, first.Close()) })

	started, err := first.StartLiveTarget(builder, &gensurla.StartLiveTargetPayload{CandidateCommit: repo.candidate})
	require.NoError(t, err, "a live copy needs no build")
	assert.Equal(t, repo.candidate, started.CandidateCommit)
	liveDirs, err := verifyrun.LeftoverLiveDirs(LiveRoot(filepath.Join(root, "ops", "cards")))
	require.NoError(t, err)
	require.Len(t, liveDirs, 1)
	var live verifyrun.LiveRequest
	readJSONFile(t, filepath.Join(liveDirs[0], "live.json"), &live)
	wantLive := layoutPlan(repo, repo.candidate, repo.candidate)
	wantLive.WorkDir = filepath.Join(liveDirs[0], "work")
	assert.Equal(t, wantLive, withoutReport(live.Plan), "the live copy runs the candidate's own tests")
	_, err = first.StopLiveTarget(builder)
	require.NoError(t, err)

	card := confirmedChange(t, first, "chg")
	verifyPayload := &gensurla.VerifyChangePayload{ChangeID: "chg", RequestRevision: card.RequestRevision, CandidateCommit: repo.candidate}
	_, err = first.VerifyChange(builder, verifyPayload)
	assertRefusalSays(t, err, "conflict", "the user has not started a build of change \"chg\"")
	_, err = first.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)
	_, err = first.VerifyChange(builder, verifyPayload)
	assertRefusalSays(t, err, "conflict", "the tests for this build are not accepted (state assigned)")
	acceptTests(t, first, "chg", repo.tests)

	for _, server := range []struct {
		name string
		open func(t *testing.T) *ControlPlane
	}{
		{name: "server that accepted the tests", open: func(*testing.T) *ControlPlane { return first }},
		{name: "server reopened from the same card store", open: func(t *testing.T) *ControlPlane {
			require.NoError(t, first.Close())
			reopened, err := New(options())
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
			return reopened
		}},
	} {
		t.Run(server.name, func(t *testing.T) {
			cp := server.open(t)
			run, err := cp.VerifyChange(builder, verifyPayload)
			require.NoError(t, err)
			assert.Equal(t, repo.candidate, run.CandidateCommit)
			assert.Equal(t, repo.tests, run.SuiteCommit)
			assert.Equal(t, writtenTests, run.TestSource)
			var req verifyrun.Request
			readJSONFile(t, filepath.Join(runsDir, run.RunID, "request.json"), &req)
			want := layoutPlan(repo, repo.candidate, repo.tests)
			want.RequestRevision = card.RequestRevision
			want.WorkDir = filepath.Join(runsDir, run.RunID, "work")
			assert.Equal(t, want, withoutReport(req.Plan), "the run builds the candidate and runs the accepted tests")

			read, err := cp.GetVerification(builder, &gensurla.GetVerificationPayload{RunID: run.RunID})
			require.NoError(t, err)
			assert.Equal(t, writtenTests, read.TestSource, "get_verification")
			listed, err := cp.ListVerifications(builder, &gensurla.ListVerificationsPayload{ChangeID: "chg"})
			require.NoError(t, err)
			require.NotEmpty(t, listed.Runs)
			assert.Equal(t, writtenTests, listed.Runs[0].TestSource, "list_verifications")
		})
	}
}

// TestVerificationAndLiveCopiesRefuseCommitsWithoutTheTestLayout verifies that
// verify_change and start_live_target refuse, before any run or live copy
// starts, a commit the repository does not hold, a candidate commit without
// the service's command package, and accepted tests without their generated
// entry point, each naming the commit and who acts; and that verify_change
// refuses a build of another revision, naming both.
func TestVerificationAndLiveCopiesRefuseCommitsWithoutTheTestLayout(t *testing.T) {
	root := realTempDir(t)
	repo := newLayoutRepo(t, filepath.Join(root, "repo"))
	unknown := strings.Repeat("e", 40)
	cases := []struct {
		name      string
		candidate string
		tests     string
		otherRev  bool
		verify    string
		live      string
	}{
		{
			name: "unknown candidate commit", candidate: unknown, tests: repo.tests,
			verify: "the candidate commit " + unknown + " is not in the project's repository; commit it and try again",
			live:   "the candidate commit " + unknown + " is not in the project's repository",
		},
		{
			name: "candidate without its command package", candidate: repo.noProgram, tests: repo.tests,
			verify: "commit " + repo.noProgram + " does not have the test layout surla init makes (it has no folder services/shop/cmd/shop):" +
				" the builder commits the layout surla init makes and verifies again",
			live: "commit " + repo.noProgram + " does not have the test layout surla init makes (it has no folder services/shop/cmd/shop)",
		},
		{
			name: "accepted tests without their entry point", candidate: repo.candidate, tests: repo.noEntry,
			verify: "commit " + repo.noEntry + " does not have the test layout surla init makes (it has no folder services/shop/eval/gen/cmd/shop-evals):" +
				" the test writer reports again, so Surla checks and records its tests again",
		},
		{
			name: "build of another revision", candidate: repo.candidate, tests: repo.tests, otherRev: true,
			verify: "but the revision in force is",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runsDir := filepath.Join(realTempDir(t), "runs")
			self, err := os.Executable()
			require.NoError(t, err)
			cp := newControlPlane(t, Options{
				WorkspaceDir:    repo.workspace,
				RunsDir:         runsDir,
				SurlaVersion:    testSurlaVersion,
				WrittenTestRuns: &WrittenTestRuns{ReportDir: realTempDir(t), Command: []string{"/bin/sh", "-c", "exit 0", "sh"}, Timeout: time.Hour},
				Live:            &LiveConfig{Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=serve"), StartTimeout: time.Minute},
			})
			user := asCaller("alice@example.com", CallerUser)
			builder := asCaller("alice@example.com", CallerBuilder)
			card := saveStoryCard(t, cp, "chg", StatusConfirmed, refundStory("Refund over the limit waits"))
			_, err = cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
			require.NoError(t, err)
			acceptTests(t, cp, "chg", tc.tests)
			if tc.otherRev {
				card = saveStoryCard(t, cp, "chg", StatusConfirmed, refundStory("Refund over the limit waits for a manager"))
			}

			_, err = cp.VerifyChange(builder, &gensurla.VerifyChangePayload{ChangeID: "chg", RequestRevision: card.RequestRevision, CandidateCommit: tc.candidate})
			assertRefusalSays(t, err, "conflict", tc.verify)
			entries, err := os.ReadDir(runsDir)
			if !os.IsNotExist(err) {
				require.NoError(t, err)
			}
			assert.Empty(t, entries, "no run is accepted")

			if tc.live == "" {
				return
			}
			_, err = cp.StartLiveTarget(builder, &gensurla.StartLiveTargetPayload{CandidateCommit: tc.candidate})
			assertRefusalSays(t, err, "live_target_failed", tc.live)
			assert.Nil(t, cp.owned, "no live copy is started")
		})
	}
}

// TestVerifyChangeSaysWhoActsUntilTheTestsAreAccepted verifies that
// verify_change refuses a build whose tests are not accepted, in each state
// the test writer's assignment can be in, naming the state and who acts next,
// and starts no run.
func TestVerifyChangeSaysWhoActsUntilTheTestsAreAccepted(t *testing.T) {
	root := realTempDir(t)
	repo := newLayoutRepo(t, filepath.Join(root, "repo"))
	cases := []struct {
		name string
		tw   testWriterAssignment
		want string
	}{
		{
			name: "reported", tw: testWriterAssignment{State: testsReported, Findings: []string{}},
			want: "(state reported): Surla is checking the test writer's report; verify again after its verdict",
		},
		{
			name: "needs changes", tw: testWriterAssignment{State: testsNeedChanges, Findings: []string{"refund_over_limit is missing"}, CheckedAt: time.Now()},
			want: "(state needs_changes): the test writer fixes Surla's findings and reports again",
		},
		{
			name: "could not run", tw: testWriterAssignment{
				State: testsCouldNotRun, Findings: []string{}, Reason: "git could not write the tests branch",
				Step: "Free disk space, then report again.", CheckedAt: time.Now(),
			},
			want: "(state could_not_run): Surla's check could not run (git could not write the tests branch); the user's step: Free disk space, then report again.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runsDir := filepath.Join(realTempDir(t), "runs")
			cp := newControlPlane(t, Options{
				WorkspaceDir:    repo.workspace,
				RunsDir:         runsDir,
				SurlaVersion:    testSurlaVersion,
				WrittenTestRuns: &WrittenTestRuns{ReportDir: realTempDir(t), Command: []string{"/bin/sh", "-c", "exit 0", "sh"}, Timeout: time.Hour},
			})
			card := saveStoryCard(t, cp, "chg", StatusConfirmed, refundStory("Refund over the limit waits"))
			_, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
			require.NoError(t, err)
			saveTestWriter(t, cp, "chg", tc.tw)

			_, err = cp.VerifyChange(asCaller("alice@example.com", CallerBuilder), &gensurla.VerifyChangePayload{
				ChangeID: "chg", RequestRevision: card.RequestRevision, CandidateCommit: repo.candidate,
			})
			assertRefusalSays(t, err, "conflict", "the tests for this build are not accepted "+tc.want)
			_, err = os.Stat(runsDir)
			assert.ErrorIs(t, err, os.ErrNotExist, "no run is accepted")
		})
	}
}

// TestTestWriterReadsOnlyTheRunsOfAnAssignedChange verifies that the test
// writer reads and lists the runs of a change it has an assignment for, and
// is refused the runs of a change without a build and every run of a project
// that verifies with the stored evaluator, which has no test writer.
func TestTestWriterReadsOnlyTheRunsOfAnAssignedChange(t *testing.T) {
	root := realTempDir(t)
	repo := newLayoutRepo(t, filepath.Join(root, "repo"))
	cases := []struct {
		name    string
		stored  bool
		build   bool
		allowed bool
	}{
		{name: "assigned change", build: true, allowed: true},
		{name: "change without a build"},
		{name: "stored evaluator", stored: true, build: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runsDir := filepath.Join(realTempDir(t), "runs")
			opts := Options{WorkspaceDir: repo.workspace, RunsDir: runsDir, SurlaVersion: testSurlaVersion}
			exit := []string{"/bin/sh", "-c", "exit 0", "sh"}
			if tc.stored {
				plan := layoutPlan(repo, "", repo.tests)
				plan.ReportDir = realTempDir(t)
				opts.Verification = &VerificationConfig{RunsDir: runsDir, Plan: plan, Command: exit, Timeout: time.Hour}
			} else {
				opts.WrittenTestRuns = &WrittenTestRuns{ReportDir: realTempDir(t), Command: exit, Timeout: time.Hour}
			}
			cp := newControlPlane(t, opts)
			user := asCaller("alice@example.com", CallerUser)
			testWriter := asCaller("alice@example.com", CallerTestWriter)
			card := saveStoryCard(t, cp, "chg", StatusConfirmed, refundStory("Refund over the limit waits"))
			if tc.build {
				_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
				require.NoError(t, err)
			}
			if !tc.stored && tc.build {
				acceptTests(t, cp, "chg", repo.tests)
			}
			runID := startRunFor(t, cp, tc.stored || tc.build, card, repo, runsDir)

			read, getErr := cp.GetVerification(testWriter, &gensurla.GetVerificationPayload{RunID: runID})
			listed, listErr := cp.ListVerifications(testWriter, &gensurla.ListVerificationsPayload{ChangeID: "chg"})
			if !tc.allowed {
				assertRefusalSays(t, getErr, "forbidden", "the test writer reads only the runs of a change it has an assignment for")
				assertRefusalSays(t, listErr, "forbidden", "the test writer reads only the runs of a change it has an assignment for")
				return
			}
			require.NoError(t, getErr)
			require.NoError(t, listErr)
			assert.Equal(t, runID, read.RunID)
			require.Len(t, listed.Runs, 1)
			assert.Equal(t, runID, listed.Runs[0].RunID)
		})
	}
}

// newLayoutRepo creates a git repository at dir holding the shop project in
// services/shop and commits it in each state layoutRepo names.
func newLayoutRepo(t *testing.T, dir string) layoutRepo {
	t.Helper()
	workspace := filepath.Join(dir, "services", "shop")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	gitForLayoutTest(t, dir, "init", "-q")
	commit := func(edit func()) string {
		edit()
		gitForLayoutTest(t, dir, "add", "-A")
		gitForLayoutTest(t, dir, "commit", "-q", "--allow-empty", "-m", "fixture")
		return strings.TrimSpace(gitForLayoutTest(t, dir, "rev-parse", "HEAD"))
	}
	remove := func(rel string) func() {
		return func() { require.NoError(t, os.RemoveAll(filepath.Join(workspace, filepath.FromSlash(rel)))) }
	}
	var repo layoutRepo
	repo.workspace = workspace
	repo.noEntry = commit(func() {
		writeTree(t, workspace, shopLayout)
		remove("eval/gen")()
	})
	repo.tests = commit(func() { writeTree(t, workspace, shopLayout) })
	repo.candidate = commit(func() {
		writeTree(t, workspace, map[string]string{"cmd/shop/refund.go": "package main\n"})
	})
	repo.noProgram = commit(remove("cmd"))
	return repo
}

// gitForLayoutTest runs a trusted git command in dir for the layout fixture,
// with a fixed identity and no system or global configuration, and returns
// its output.
func gitForLayoutTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.com"}, args...)...)
	cmd.Dir = dir
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

// layoutPlan returns the plan that builds candidate's shop program and runs
// the generated entry point of the tests at tests, as starterPlanAt makes it
// for repo, without the fields its callers add.
func layoutPlan(repo layoutRepo, candidate, tests string) verify.Plan {
	repoRoot := filepath.Dir(filepath.Dir(repo.workspace))
	return verify.Plan{
		CandidateRepo:   repoRoot,
		CandidateCommit: candidate,
		CandidateModule: "services/shop",
		Binaries:        map[string]string{"shop": "./cmd/shop"},
		SuiteRepo:       repoRoot,
		SuiteCommit:     tests,
		SuiteModule:     "services/shop/eval",
		SuiteCommand:    "./gen/cmd/shop-evals",
		Tests:           verify.TestsWritten,
	}
}

// withoutReport returns plan without its report folder, which each server
// resolves from its own settings.
func withoutReport(plan verify.Plan) verify.Plan {
	plan.ReportDir = ""
	return plan
}

// acceptTests records, through the build store's save path, that Surla
// accepted the test writer's tests of changeID's build at commit.
func acceptTests(t *testing.T, cp *ControlPlane, changeID, commit string) {
	t.Helper()
	saveTestWriter(t, cp, changeID, testWriterAssignment{State: testsAccepted, Read: true, Findings: []string{}, Commit: commit, CheckedAt: time.Now()})
}

// saveTestWriter saves tw as the test writer's assignment of changeID's
// build, through the build store's save path.
func saveTestWriter(t *testing.T, cp *ControlPlane, changeID string, tw testWriterAssignment) {
	t.Helper()
	cp.mu.Lock()
	defer cp.mu.Unlock()
	build, found, err := cp.cards.loadBuild(changeID)
	require.NoError(t, err)
	require.True(t, found, "change %q has a build", changeID)
	build.TestWriter = tw
	require.NoError(t, cp.cards.saveBuild(build))
}

// startRunFor starts a verification run of card for its owner and returns its
// run ID: through verify_change when verifiable, or, for a change without a
// build, by saving the run directly, since verify_change refuses it.
func startRunFor(t *testing.T, cp *ControlPlane, verifiable bool, card *gensurla.ChangeCardResult, repo layoutRepo, runsDir string) string {
	t.Helper()
	if verifiable {
		run, err := cp.VerifyChange(asCaller("alice@example.com", CallerBuilder), &gensurla.VerifyChangePayload{
			ChangeID: card.ChangeID, RequestRevision: card.RequestRevision, CandidateCommit: repo.candidate,
		})
		require.NoError(t, err)
		return run.RunID
	}
	runID := verifyrun.NewRunID()
	plan := layoutPlan(repo, repo.candidate, repo.tests)
	plan.RequestRevision = card.RequestRevision
	plan.ReportDir = realTempDir(t)
	plan.WorkDir = filepath.Join(runsDir, runID, "work")
	_, err := verifyrun.Start(runsDir, verifyrun.Request{
		RunID: runID, Owner: "alice@example.com", ChangeID: card.ChangeID, AcceptedAt: time.Now().UTC(), Timeout: time.Hour, Plan: plan,
	}, []string{"/bin/sh", "-c", "exit 0", "sh"})
	require.NoError(t, err)
	return runID
}

// assertRefusalSays checks that err is the design's refusal named name and
// that its message contains want.
func assertRefusalSays(t *testing.T, err error, name, want string) {
	t.Helper()
	var refusal *goa.ServiceError
	if assert.ErrorAs(t, err, &refusal) {
		assert.Equal(t, name, refusal.Name)
		assert.Contains(t, refusal.Message, want)
	}
}

// readJSONFile decodes the JSON file at p into v.
func readJSONFile(t *testing.T, p string, v any) {
	t.Helper()
	raw, err := os.ReadFile(p)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, v))
}
