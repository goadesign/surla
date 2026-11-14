// This file checks which tests verification runs and live copies use in a
// project without stored verification settings: a run uses the tests Surla
// accepted for the change's build, at their commit, and a live copy uses the
// candidate commit's own test module. Both read the layout `surla init` makes
// from a real git repository, and both refuse a commit that lacks it.
package controlplane

import (
	"encoding/json"
	"net/http"
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

	// Its Close at the test's end is idempotent, so the explicit Close below
	// can reopen the card store.
	first := newBuildControlPlane(t, options())

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
	assertRefusalSays(t, err, "no_build", "change \"chg\" has no build with a test writer")
	_, err = first.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)
	_, err = first.VerifyChange(builder, verifyPayload)
	assertRefusalSays(t, err, "tests_not_accepted", "the tests of this build are not accepted; their state is assigned")
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

// TestVerifyChangeRefusesWhatItCannotRunWithItsStep verifies each refusal of
// verify_change in a project without stored verification settings, before
// any run starts: called directly, over HTTP (409, the refusal's name, a
// message that ends with its step) and through the MCP tool as the builder,
// whose result is an error holding the reason and the step. Where a live copy
// would read the same commit, start_live_target refuses it too.
func TestVerifyChangeRefusesWhatItCannotRunWithItsStep(t *testing.T) {
	root := realTempDir(t)
	repo := newLayoutRepo(t, filepath.Join(root, "repo"))
	plain := filepath.Join(root, "plain", "shop")
	writeStarterAt(t, plain, "example.com/shop")
	unknown := strings.Repeat("e", 40)
	const (
		// noBuild starts no build.
		noBuild = iota
		// withoutTestWriter starts a build while the project has stored
		// settings, which saves it without a test writer, and then serves
		// the project without them.
		withoutTestWriter
		// assigned starts a build whose tests are not reported yet.
		assigned
		// accepted starts a build and accepts its tests at the case's commit.
		accepted
	)
	cases := []struct {
		name      string
		workspace string
		build     int
		tests     string
		otherRev  bool
		candidate string
		refusal   string
		reason    string
		step      string
		live      bool
	}{
		{
			name: "no build", build: noBuild, candidate: repo.candidate,
			refusal: "no_build", reason: `change "chg" has no build with a test writer, so no tests are accepted for revision`,
			step: "the user starts a build on the views page",
		},
		{
			name: "build started without a test writer", build: withoutTestWriter, candidate: repo.candidate,
			refusal: "no_build", reason: `change "chg" has no build with a test writer, so no tests are accepted for revision`,
			step: "the user starts a build on the views page",
		},
		{
			name: "build of another revision", build: accepted, tests: repo.tests, otherRev: true, candidate: repo.candidate,
			refusal: "no_build", reason: `change "chg" is built at revision`, step: "on the views page",
		},
		{
			name: "tests not accepted", build: assigned, candidate: repo.candidate,
			refusal: "tests_not_accepted", reason: "the tests of this build are not accepted; their state is assigned",
			step: "the test writer writes the required tests and reports them finished; your step: keep your Chorus watcher running; Surla messages you there when the tests are accepted; then read get_builder_assignment and verify again",
		},
		{
			name: "unknown candidate commit", build: accepted, tests: repo.tests, candidate: unknown,
			refusal: "conflict", reason: "the candidate commit " + unknown + " is not in the project's repository",
			step: "commit it and try again", live: true,
		},
		{
			name: "candidate without its command package", build: accepted, tests: repo.tests, candidate: repo.noProgram,
			refusal: "no_test_layout",
			reason:  "commit " + repo.noProgram + " does not have the test layout surla init makes (it has no folder services/shop/cmd/shop)",
			step:    "the builder commits the layout surla init makes and verifies again", live: true,
		},
		{
			name: "accepted tests without their entry point", build: accepted, tests: repo.noEntry, candidate: repo.candidate,
			refusal: "no_test_layout",
			reason:  "commit " + repo.noEntry + " does not have the test layout surla init makes (it has no folder services/shop/eval/gen/cmd/shop-evals)",
			step:    "the test writer reports again, so Surla checks and records its tests again",
		},
		{
			name: "accepted tests' commit no longer in the repository", build: accepted, tests: strings.Repeat("f", 40), candidate: repo.candidate,
			refusal: "no_test_layout", reason: "the accepted tests' commit " + strings.Repeat("f", 40) + " is not in the project's repository",
			step: "the test writer reports again, so Surla records the tests anew, or the user builds again",
		},
		{
			name: "project folder outside any git repository", workspace: plain, build: accepted, tests: repo.tests, candidate: repo.candidate,
			refusal: "no_test_layout", reason: "the project folder " + plain + " is not inside a git repository, so it has no commits to verify",
			step: "make the project folder a git repository and commit it", live: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace := repo.workspace
			if tc.workspace != "" {
				workspace = tc.workspace
			}
			runsDir := filepath.Join(realTempDir(t), "runs")
			self, err := os.Executable()
			require.NoError(t, err)
			opts := Options{
				WorkspaceDir:    workspace,
				RunsDir:         runsDir,
				SurlaVersion:    testSurlaVersion,
				WrittenTestRuns: &WrittenTestRuns{ReportDir: realTempDir(t), Command: []string{"/bin/sh", "-c", "exit 0", "sh"}, Timeout: time.Hour},
				Live:            &LiveConfig{Command: []string{self}, Env: append(os.Environ(), liveTargetModeEnv+"=serve"), StartTimeout: time.Minute},
			}
			var cp *ControlPlane
			var card *gensurla.ChangeCardResult
			if tc.build == withoutTestWriter {
				cp, card = builtWithoutTestWriter(t, opts)
			} else {
				cp = newBuildControlPlane(t, opts)
				card = saveStoryCard(t, cp, "chg", StatusConfirmed, refundStory("Refund over the limit waits"))
			}
			builder := asCaller("alice@example.com", CallerBuilder)
			if tc.build == assigned || tc.build == accepted {
				_, err = cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
				require.NoError(t, err)
			}
			if tc.build == accepted {
				acceptTests(t, cp, "chg", tc.tests)
			}
			if tc.otherRev {
				card = saveStoryCard(t, cp, "chg", StatusConfirmed, refundStory("Refund over the limit waits for a manager"))
			}
			srv := roleServer(t, cp)
			body := `{"request_revision":"` + card.RequestRevision + `","candidate_commit":"` + tc.candidate + `"}`

			_, err = cp.VerifyChange(builder, &gensurla.VerifyChangePayload{ChangeID: "chg", RequestRevision: card.RequestRevision, CandidateCommit: tc.candidate})
			assertRefusalSays(t, err, tc.refusal, tc.reason)
			status, raw := httpCall(t, srv.URL, "builder", http.MethodPost, "/changes/chg/verifications", body)
			assert.Equal(t, http.StatusConflict, status, raw)
			var refused struct {
				Name    string `json:"name"`
				Message string `json:"message"`
			}
			require.NoError(t, json.Unmarshal([]byte(raw), &refused), raw)
			assert.Equal(t, tc.refusal, refused.Name, "over HTTP")
			assert.Contains(t, refused.Message, tc.reason, "over HTTP")
			assert.True(t, strings.HasSuffix(refused.Message, tc.step), "over HTTP the message ends with its step: %s", refused.Message)
			failure, _ := mcpToolCall(t, srv.URL, "builder", "verify_change",
				`{"change_id":"chg","request_revision":"`+card.RequestRevision+`","candidate_commit":"`+tc.candidate+`"}`)
			assert.Contains(t, failure, tc.reason, "through the MCP tool")
			assert.Contains(t, failure, tc.step, "through the MCP tool")
			runs, err := verifyrun.List(runsDir, "alice@example.com", "chg")
			require.NoError(t, err)
			assert.Empty(t, runs, "no run is accepted")

			if !tc.live {
				return
			}
			_, err = cp.StartLiveTarget(builder, &gensurla.StartLiveTargetPayload{CandidateCommit: tc.candidate})
			assertRefusalSays(t, err, "live_target_failed", tc.reason)
			assert.Nil(t, cp.owned, "no live copy is started")
		})
	}
}

// TestANewAcceptanceChangesOnlyTheTestsCommit verifies that verifying the
// same candidate after Surla accepted the tests again, at another commit,
// saves a plan equal to the one before except for the tests' commit (and the
// run's own work folder).
func TestANewAcceptanceChangesOnlyTheTestsCommit(t *testing.T) {
	root := realTempDir(t)
	repo := newLayoutRepo(t, filepath.Join(root, "repo"))
	runsDir := filepath.Join(root, "runs")
	cp := newBuildControlPlane(t, Options{
		WorkspaceDir:    repo.workspace,
		RunsDir:         runsDir,
		SurlaVersion:    testSurlaVersion,
		WrittenTestRuns: &WrittenTestRuns{ReportDir: realTempDir(t), Command: []string{"/bin/sh", "-c", "exit 0", "sh"}, Timeout: time.Hour},
	})
	card := saveStoryCard(t, cp, "chg", StatusConfirmed, refundStory("Refund over the limit waits"))
	_, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)
	// verifyAfterAccepting accepts the tests at commit, verifies the
	// candidate and returns the saved plan.
	verifyAfterAccepting := func(commit string) verify.Plan {
		acceptTests(t, cp, "chg", commit)
		run, err := cp.VerifyChange(asCaller("alice@example.com", CallerBuilder), &gensurla.VerifyChangePayload{
			ChangeID: "chg", RequestRevision: card.RequestRevision, CandidateCommit: repo.candidate,
		})
		require.NoError(t, err)
		var req verifyrun.Request
		readJSONFile(t, filepath.Join(runsDir, run.RunID, "request.json"), &req)
		return req.Plan
	}

	first := verifyAfterAccepting(repo.tests)
	second := verifyAfterAccepting(repo.candidate)
	assert.Equal(t, repo.tests, first.SuiteCommit)
	assert.Equal(t, repo.candidate, second.SuiteCommit)
	assert.NotEqual(t, first.WorkDir, second.WorkDir, "each run has its own work folder")
	second.SuiteCommit, second.WorkDir = first.SuiteCommit, first.WorkDir
	assert.Equal(t, first, second, "nothing else changes")
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
			want: "reported: Surla is checking the test writer's report; your step: keep your Chorus watcher running; Surla messages you there when the tests are accepted; then read get_builder_assignment and verify again",
		},
		{
			name: "needs changes", tw: testWriterAssignment{State: testsNeedChanges, Findings: []string{"refund_over_limit is missing"}, CheckedAt: time.Now()},
			want: "needs changes: the test writer fixes Surla's findings and reports again; your step: keep your Chorus watcher running; Surla messages you there when the tests are accepted; then read get_builder_assignment and verify again",
		},
		{
			name: "could not run", tw: testWriterAssignment{
				State: testsCouldNotRun, Findings: []string{}, Reason: "git could not write the tests branch",
				Step: "Free disk space, then report again.", CheckedAt: time.Now(),
			},
			want: "could not run: Surla's check could not run (git could not write the tests branch); the user's step: Free disk space, then report again; your step: keep your Chorus watcher running; Surla messages you there when the tests are accepted; then read get_builder_assignment and verify again",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runsDir := filepath.Join(realTempDir(t), "runs")
			cp := newBuildControlPlane(t, Options{
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
			assertRefusalSays(t, err, "tests_not_accepted", "the tests of this build are not accepted; their state is "+tc.want)
			_, err = os.Stat(runsDir)
			assert.ErrorIs(t, err, os.ErrNotExist, "no run is accepted")
		})
	}
}

// TestTestsNotAcceptedIsWordedForEachReader verifies that verify_change and
// approve_merge, refused for the same build in the same state, each give the
// text written for its reader: the builder's ends with the builder's own step,
// and the user's names the builder and instructs no agent.
func TestTestsNotAcceptedIsWordedForEachReader(t *testing.T) {
	repo := newLayoutRepo(t, filepath.Join(realTempDir(t), "repo"))
	cp := newBuildControlPlane(t, Options{
		WorkspaceDir:    repo.workspace,
		RunsDir:         filepath.Join(realTempDir(t), "runs"),
		SurlaVersion:    testSurlaVersion,
		WrittenTestRuns: &WrittenTestRuns{ReportDir: realTempDir(t), Command: []string{"/bin/sh", "-c", "exit 0", "sh"}, Timeout: time.Hour},
	})
	card := saveStoryCard(t, cp, "chg", StatusConfirmed, refundStory("Refund over the limit waits"))
	_, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)
	saveTestWriter(t, cp, "chg", testWriterAssignment{State: testsReported, Findings: []string{}})

	_, verifyErr := cp.VerifyChange(asCaller("alice@example.com", CallerBuilder), &gensurla.VerifyChangePayload{
		ChangeID: "chg", RequestRevision: card.RequestRevision, CandidateCommit: repo.candidate,
	})
	_, approveErr := cp.ApproveMerge(asCaller("alice@example.com", CallerUser), &gensurla.ApproveMergePayload{
		ChangeID: "chg", RequestRevision: card.RequestRevision, CandidateCommit: repo.candidate, ReportDigest: strings.Repeat("a", 64),
	})
	for _, tc := range []struct {
		reader string
		err    error
		want   string
	}{
		{"builder", verifyErr, "the tests of this build are not accepted; their state is reported: Surla is checking the test writer's report; your step: keep your Chorus watcher running; Surla messages you there when the tests are accepted; then read get_builder_assignment and verify again"},
		{"user", approveErr, "the tests of this build are not accepted; their state is reported: Surla is checking the test writer's report; the builder verifies again after its verdict"},
	} {
		t.Run(tc.reader, func(t *testing.T) {
			var refusal *goa.ServiceError
			require.ErrorAs(t, tc.err, &refusal)
			assert.Equal(t, "tests_not_accepted", refusal.Name)
			assert.Equal(t, tc.want, refusal.Message)
		})
	}
}

// TestTestWriterReadsOnlyTheRunsOfAnAssignedChange verifies that the test
// writer reads and lists the runs of a change it has an assignment for, and
// is refused the runs of a change without a build, of a build started without
// a test writer, and every run of a project that verifies with the stored
// evaluator, which has no test writer.
func TestTestWriterReadsOnlyTheRunsOfAnAssignedChange(t *testing.T) {
	root := realTempDir(t)
	repo := newLayoutRepo(t, filepath.Join(root, "repo"))
	cases := []struct {
		name    string
		stored  bool
		build   bool
		builder bool
		allowed bool
	}{
		{name: "assigned change", build: true, allowed: true},
		{name: "change without a build"},
		{name: "build started without a test writer", builder: true},
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
			var cp *ControlPlane
			var card *gensurla.ChangeCardResult
			if tc.builder {
				cp, card = builtWithoutTestWriter(t, opts)
			} else {
				cp = newBuildControlPlane(t, opts)
				card = saveStoryCard(t, cp, "chg", StatusConfirmed, refundStory("Refund over the limit waits"))
			}
			testWriter := asCaller("alice@example.com", CallerTestWriter)
			if tc.build {
				_, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
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

// builtWithoutTestWriter starts alice's build of the confirmed change "chg"
// while the project has stored verification settings, which saves the build
// without a test writer, and then returns a control plane on the same card
// store with opts, which has none, together with the change.
func builtWithoutTestWriter(t *testing.T, opts Options) (*ControlPlane, *gensurla.ChangeCardResult) {
	t.Helper()
	opts.CardsDir = filepath.Join(realTempDir(t), "cards")
	stored := opts
	stored.WrittenTestRuns, stored.Live = nil, nil
	stored.Verification = &VerificationConfig{
		RunsDir: opts.RunsDir, Plan: verify.Plan{ReportDir: realTempDir(t)},
		Command: []string{"/bin/sh", "-c", "exit 0", "sh"}, Timeout: time.Hour,
	}
	first := newBuildControlPlane(t, stored)
	card := saveStoryCard(t, first, "chg", StatusConfirmed, refundStory("Refund over the limit waits"))
	_, err := first.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)
	assert.Equal(t, `"`+buildRolesBuilderOnly+`"`, jsonAt(t, readBuildFile(t, first, "chg"), "roles"), "a stored evaluator's build has no test writer")
	require.NoError(t, first.Close())
	return newControlPlane(t, opts), card
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
// build, through the build store's save path, keeping the working folder the
// build was started with, which no verdict changes.
func saveTestWriter(t *testing.T, cp *ControlPlane, changeID string, tw testWriterAssignment) {
	t.Helper()
	cp.mu.Lock()
	defer cp.mu.Unlock()
	build, found, err := cp.cards.loadBuild(changeID)
	require.NoError(t, err)
	require.True(t, found, "change %q has a build", changeID)
	tw.WorkingFolder = build.TestWriter.WorkingFolder
	build.TestWriter = tw
	// A reported build has had at least one report; a verdict keeps its report's number.
	if tw.State != testsAssigned {
		build.ReportNumber = max(build.ReportNumber, 1)
	}
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
	}, []string{"/bin/sh", "-c", "exit 0", "sh"}, nil)
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
