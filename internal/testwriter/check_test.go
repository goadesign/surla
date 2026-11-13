package testwriter

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/shquote"
	"github.com/goadesign/surla/internal/testmodule"
	"github.com/goadesign/surla/verify/candidate"
	"github.com/goadesign/surla/verify/suite"
)

// fixtureSuite locates the fixture's suite module and the program the
// generator writes in it.
var fixtureSuite = Suite{Module: "eval", Command: "./gen/cmd/shop-evals"}

// refundStories has two finished flows and one draft.
var refundStories = []designeval.StorySummary{{
	Name: "Refunds",
	Flows: []designeval.StoryFlow{
		{Key: "refund-over-limit", Title: "A refund above the limit waits for a manager.", Complete: true},
		{Key: "store-credit-within-limit", Title: "A small refund becomes store credit.", Complete: true},
		{Key: "disputed-chargeback", Title: "A customer opens a chargeback.", Complete: false},
	},
}}

// fixture caches the fixture project, which takes a sandboxed generation and
// a go mod tidy to build. Tests clone it.
var fixture struct {
	mu sync.Mutex
	// files is the project's tree keyed by slash path, nil until built.
	files map[string]string
}

// fixtureDesign is the fixture's evaluation design: one service and one
// suite with one scenario written by hand. It imports Goa-AI's DSL, as the
// starter's does, so a step can tag a scenario.
const fixtureDesign = `package design

import (
	. "goa.design/goa-ai/dsl"
	. "goa.design/goa-ai/eval/dsl"
	. "goa.design/goa/v3/dsl"
)

// tag is Goa-AI's Tags, which a scenario may use.
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
	Scenario("prices_tea", func() {
		Description("Tea costs 350 cents.")
	})
})
`

// TestPrepareWritesOneRequiredMethodPerFinishedFlow prepares a working copy
// from a Story with two finished flows and a draft: the copy's generated
// suite requires one hook method per finished flow, the result names them by
// the generator's rule, and the project folder is byte-identical.
func TestPrepareWritesOneRequiredMethodPerFinishedFlow(t *testing.T) {
	skipWithoutSandbox(t)
	repo, markers := newProject(t, fixtureProject(t))
	before := projectFiles(t, repo)
	wc, err := OpenWorkingCopy(t.Context(), repo)
	require.NoError(t, err)

	required, err := testChecker.Prepare(t.Context(), wc, fixtureSuite, refundStories)
	require.NoError(t, err)
	assert.Equal(t, []designeval.StoryScenario{
		{Story: "Refunds", Key: "refund-over-limit", Scenario: "refund_over_limit", Hook: "RefundOverLimit"},
		{Story: "Refunds", Key: "store-credit-within-limit", Scenario: "store_credit_within_limit", Hook: "StoreCreditWithinLimit"},
	}, required)
	suiteGo, err := os.ReadFile(filepath.Join(wc.Project(), "eval", generatedSuite))
	require.NoError(t, err)
	assert.Contains(t, string(suiteGo), "RefundOverLimit(context.Context) (eval.Result, error)")
	assert.Contains(t, string(suiteGo), "StoreCreditWithinLimit(context.Context) (eval.Result, error)")
	assert.NotContains(t, string(suiteGo), "DisputedChargeback", "a draft flow requires nothing")
	assert.Equal(t, before, projectFiles(t, repo), "the project folder is not touched")
	assert.Empty(t, runGit(t, repo, "status", "--porcelain", "--untracked-files=all"))
	assert.Empty(t, ranHooks(t, markers))
}

// TestPrepareGivesTheAuthorItsGenerationFailure breaks the copy's design, as
// its author could: Prepare returns an *AuthorError whose finding quotes the
// compiler, with no absolute path, and writes no generated folder.
func TestPrepareGivesTheAuthorItsGenerationFailure(t *testing.T) {
	skipWithoutSandbox(t)
	repo, _ := newProject(t, fixtureProject(t))
	wc, err := OpenWorkingCopy(t.Context(), repo)
	require.NoError(t, err)
	design := filepath.Join(wc.Project(), "eval", "design", "design.go")
	raw, err := os.ReadFile(design)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(design, append(raw, "\nvar _ = undefinedInTheDesign\n"...), 0o600))

	_, err = testChecker.Prepare(t.Context(), wc, fixtureSuite, refundStories)
	var author *AuthorError
	require.ErrorAs(t, err, &author)
	assert.Equal(t, FindingGeneration, author.Finding.Kind)
	assert.Contains(t, author.Finding.Message, "undefinedInTheDesign")
	assert.NotContains(t, author.Finding.Message, wc.Dir())
}

// TestCheckJudgesTheWorkingCopy plays the test writer through a prepared copy:
// each step changes the copy's files and checks it, and Check builds only the
// program it generates. Each of these returns one finding naming what is
// wrong:
//   - a missing hook method;
//   - an edit of the generated folder that drops a requirement;
//   - a hand-written program that only prints the required names;
//   - a build that reads a file outside the snapshot, which only the build
//     sandbox refuses;
//   - a listing that writes 256 MiB to its error stream, quoted from a
//     bounded excerpt of its end while allocating far less than that;
//   - a scenario tagged holdout.
//
// A complete copy whose package prints noise, including a JSON object, on
// standard error and tries to write outside its sandbox while starting is
// accepted, and the write never happens. Recording makes one commit holding
// the files as checked, with the generated folder written from the Story,
// unsigned though the repository asks for signed commits, and recording the
// unchanged copy again returns that commit.
func TestCheckJudgesTheWorkingCopy(t *testing.T) {
	skipWithoutSandbox(t)
	repo, markers := newProject(t, fixtureProject(t))
	start := runGit(t, repo, "rev-parse", "HEAD")
	wc, err := OpenWorkingCopy(t.Context(), repo)
	require.NoError(t, err)
	_, err = testChecker.Prepare(t.Context(), wc, fixtureSuite, refundStories)
	require.NoError(t, err)
	prepared, err := os.ReadFile(filepath.Join(wc.Project(), "eval", generatedSuite))
	require.NoError(t, err)
	outside := filepath.Join(t.TempDir(), "written-outside-the-sandbox")
	quiet := quietInit(outside)
	// header is readable by any build but the sandboxed one, which may read
	// only the snapshot, the toolchain and the module cache.
	header := filepath.Join(t.TempDir(), "outside.h")
	require.NoError(t, os.WriteFile(header, []byte("#define OUTSIDE 1\n"), 0o600))
	design := fixtureProject(t)["eval/design/design.go"]

	steps := []struct {
		name string
		// files are written into the copy's suite module before the check.
		files map[string]string
		// remove lists files of the suite module removed before the check.
		remove []string
		// wantKind is the one expected finding's kind; empty means accepted.
		wantKind FindingKind
		// wantMessage lists fragments of that finding's message.
		wantMessage []string
		// maxAlloc, when not zero, bounds the bytes the check allocates.
		maxAlloc uint64
	}{
		{
			name:        "an unwritten hook method",
			files:       map[string]string{"acceptance/acceptance.go": fixtureAcceptance(quiet, "StoreCreditWithinLimit")},
			wantKind:    FindingBuild,
			wantMessage: []string{"does not compile", "RefundOverLimit"},
		},
		{
			name:        "the generated folder edited to drop the requirement",
			files:       map[string]string{generatedSuite: fixtureProject(t)["eval/"+generatedSuite]},
			wantKind:    FindingBuild,
			wantMessage: []string{"RefundOverLimit"},
		},
		{
			name:        "a hand-written program that only prints the required names",
			files:       map[string]string{"cmd/shop-evals/main.go": hollowMain},
			wantKind:    FindingBuild,
			wantMessage: []string{"RefundOverLimit"},
		},
		{
			name: "a build that reads outside the snapshot",
			files: map[string]string{
				"acceptance/acceptance.go": fixtureAcceptance(quiet, "RefundOverLimit", "StoreCreditWithinLimit"),
				"acceptance/outside.s":     "#include \"" + header + "\"\n",
			},
			wantKind:    FindingBuild,
			wantMessage: []string{"does not compile", "#include: open <absolute path>"},
		},
		{
			name:        "a listing that fails after flooding its error stream",
			files:       map[string]string{"acceptance/acceptance.go": fixtureAcceptance(floodInit, "RefundOverLimit", "StoreCreditWithinLimit")},
			remove:      []string{"acceptance/outside.s"},
			wantKind:    FindingListing,
			wantMessage: []string{"exited with status 3", "earlier output left out", "end of the error stream"},
			// The error stream is read only as a bounded excerpt. The whole
			// check allocated 2.5 MB under the race detector (measured with
			// this 256 MiB stream); 32 MiB leaves room for that to vary and
			// is an eighth of what reading the stream whole would allocate.
			maxAlloc: 32 << 20,
		},
		{
			name: "a scenario tagged holdout",
			files: map[string]string{
				"acceptance/acceptance.go": fixtureAcceptance(quiet, "RefundOverLimit", "StoreCreditWithinLimit"),
				"design/design.go": strings.Replace(design,
					`Description("Tea costs 350 cents.")`, `Description("Tea costs 350 cents.")`+"\n\t\tTags(\"holdout\")", 1),
			},
			wantKind:    FindingHiddenTag,
			wantMessage: []string{`Scenario "prices_tea" is tagged holdout`},
		},
		{
			name:  "a complete copy that prints noise on standard error",
			files: map[string]string{"design/design.go": design},
		},
	}
	var accepted Verdict
	for _, step := range steps {
		writeFiles(t, filepath.Join(wc.Project(), "eval"), step.files)
		for _, name := range step.remove {
			require.NoError(t, os.Remove(filepath.Join(wc.Project(), "eval", filepath.FromSlash(name))))
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		v, err := testChecker.Check(t.Context(), wc, fixtureSuite, refundStories)
		runtime.ReadMemStats(&after)
		require.NoError(t, err, step.name)
		if step.maxAlloc > 0 {
			t.Logf("%s: the check allocated %d bytes", step.name, after.TotalAlloc-before.TotalAlloc)
			assert.Less(t, after.TotalAlloc-before.TotalAlloc, step.maxAlloc, "%s: bytes allocated", step.name)
		}
		t.Logf("%s: %+v", step.name, v.Findings)
		assertNoAbsolutePath(t, v.Findings)
		if step.wantKind == "" {
			require.True(t, v.Accepted(), "%s: %+v", step.name, v.Findings)
			accepted = v
			continue
		}
		require.Len(t, v.Findings, 1, "%s: %+v", step.name, v.Findings)
		assert.Equal(t, step.wantKind, v.Findings[0].Kind, step.name)
		for _, w := range step.wantMessage {
			assert.Contains(t, v.Findings[0].Message, w, step.name)
		}
		assert.LessOrEqual(t, len(v.Findings[0].Message), maxFindingBytes, step.name)
	}
	assert.NoFileExists(t, outside, "--describe runs only in the sandbox")

	// The repository asks for signed commits with a signing program that
	// leaves a marker and fails: a recorded commit is never signed.
	signer, signed := filepath.Join(t.TempDir(), "signer"), filepath.Join(t.TempDir(), "signed")
	require.NoError(t, os.WriteFile(signer, []byte("#!/bin/sh\n: > "+shquote.Quote(signed)+"\nexit 1\n"), 0o700))
	runGit(t, repo, "config", "commit.gpgsign", "true")
	runGit(t, repo, "config", "gpg.program", signer)
	commit, err := Record(t.Context(), accepted, "Add the accepted tests")
	require.NoError(t, err)
	assert.NoFileExists(t, signed, "the signing program never ran")
	assert.NotContains(t, runGit(t, repo, "cat-file", "commit", commit), "gpgsig")
	assert.Equal(t, start, runGit(t, repo, "rev-parse", commit+"^"), "one commit on top of the branch")
	assert.Equal(t, strings.TrimSpace(string(prepared)), runGit(t, repo, "show", commit+":eval/"+generatedSuite),
		"the commit holds the generated suite written from the Story, not the author's edit")
	assert.Equal(t, "M eval/"+generatedSuite, runGit(t, wc.Dir(), "status", "--porcelain"),
		"the author's edit of the generated folder stays an uncommitted change")
	changed := runGit(t, repo, "diff", "--name-only", start, commit)
	for _, p := range strings.Split(changed, "\n") {
		assert.True(t, strings.HasPrefix(p, "eval/"), "the commit changes only the test module, not %s", p)
	}
	assert.Empty(t, runGit(t, repo, "status", "--porcelain", "--untracked-files=all"))

	again, err := testChecker.Check(t.Context(), wc, fixtureSuite, refundStories)
	require.NoError(t, err)
	require.True(t, again.Accepted(), "%+v", again.Findings)
	same, err := Record(t.Context(), again, "Add the accepted tests")
	require.NoError(t, err)
	assert.Equal(t, commit, same, "an unchanged copy returns the existing commit")
	assert.Empty(t, ranHooks(t, markers))
}

// TestCheckIgnoresTheCopysOwnRepository rewrites the complete copy's .git in
// the two ways its author could, to a repository whose configuration defines
// the filter the copy's attributes name. Check still accepts the copy, the
// filter never runs, and Record commits the generated suite written from the
// Story.
func TestCheckIgnoresTheCopysOwnRepository(t *testing.T) {
	skipWithoutSandbox(t)
	repo, markers := newProject(t, fixtureProject(t))
	wc, err := OpenWorkingCopy(t.Context(), repo)
	require.NoError(t, err)
	_, err = testChecker.Prepare(t.Context(), wc, fixtureSuite, refundStories)
	require.NoError(t, err)
	prepared, err := os.ReadFile(filepath.Join(wc.Project(), "eval", generatedSuite))
	require.NoError(t, err)
	outside := filepath.Join(t.TempDir(), "written-outside-the-sandbox")
	writeFiles(t, wc.Dir(), map[string]string{
		"eval/.gitattributes":           authorsAttributes,
		"eval/acceptance/acceptance.go": fixtureAcceptance(quietInit(outside), "RefundOverLimit", "StoreCreditWithinLimit"),
	})
	marker := filepath.Join(t.TempDir(), "filter-ran")
	for _, rewrite := range []func(t *testing.T, dir, marker string){replaceCopysGit, pointCopysGitInside} {
		rewrite(t, wc.Dir(), marker)
		v, err := testChecker.Check(t.Context(), wc, fixtureSuite, refundStories)
		require.NoError(t, err)
		require.True(t, v.Accepted(), "%+v", v.Findings)
		assert.NoFileExists(t, marker, "no program named inside the copy runs")
		commit, err := Record(t.Context(), v, "Add the accepted tests")
		require.NoError(t, err)
		assert.Equal(t, strings.TrimSpace(string(prepared)), runGit(t, repo, "show", commit+":eval/"+generatedSuite))
	}
	assert.NoFileExists(t, outside)
	assert.Empty(t, ranHooks(t, markers))
}

// TestCheckWantsExactlyOneEvaluationSuite gives the copy's design two
// suites, then none, each with a Story that requires tests and with one that
// requires none: Check says the module must declare exactly one evaluation
// suite and how many it declares, never a build error about a missing
// package. A module whose path is outside Surla's test layout gets no
// generated program, and Check says so.
func TestCheckWantsExactlyOneEvaluationSuite(t *testing.T) {
	skipWithoutSandbox(t)
	repo, _ := newProject(t, fixtureProject(t))
	wc, err := OpenWorkingCopy(t.Context(), repo)
	require.NoError(t, err)
	module := filepath.Join(wc.Project(), "eval")
	design := fixtureProject(t)["eval/design/design.go"]
	cases := []struct {
		name, design, count string
	}{
		{name: "two suites", design: design + `
var _ = Suite("second", func() {
	Description("A second suite.")
	Timeout("30s")
	Scenario("other", func() { Description("Other.") })
})
`, count: "declares 2"},
		{name: "no suite", design: design[:strings.Index(design, "var _ = Suite(")] + "\n// The evaluation DSL stays imported.\nvar _ = Suite\n",
			count: "declares 0"},
	}
	for _, tc := range cases {
		for storyName, stories := range map[string][]designeval.StorySummary{
			"a Story that requires tests": refundStories,
			"a Story that requires none":  nil,
		} {
			t.Run(tc.name+", "+storyName, func(t *testing.T) {
				writeFiles(t, module, map[string]string{"design/design.go": tc.design})
				v, err := testChecker.Check(t.Context(), wc, fixtureSuite, stories)
				require.NoError(t, err)
				require.Len(t, v.Findings, 1, "%+v", v.Findings)
				assert.Equal(t, FindingGeneration, v.Findings[0].Kind)
				assert.Contains(t, v.Findings[0].Message, "exactly one evaluation suite")
				assert.Contains(t, v.Findings[0].Message, tc.count)
				assertNoAbsolutePath(t, v.Findings)
			})
		}
	}
	t.Run("a module outside the test layout", func(t *testing.T) {
		goMod := fixtureProject(t)["eval/go.mod"]
		writeFiles(t, module, map[string]string{
			"design/design.go": design,
			"go.mod":           strings.Replace(goMod, "module example.com/shop/eval\n", "module example.com/shop/evaluation\n", 1),
		})
		v, err := testChecker.Check(t.Context(), wc, fixtureSuite, refundStories)
		require.NoError(t, err)
		require.Len(t, v.Findings, 1, "%+v", v.Findings)
		assert.Equal(t, FindingGeneration, v.Findings[0].Kind)
		assert.Contains(t, v.Findings[0].Message, "Surla generated no test program at ./gen/cmd/shop-evals")
		assertNoAbsolutePath(t, v.Findings)
	})
}

// TestCheckLimitsTheBuildAndTheListing runs a complete copy with stand-ins
// for the sandboxed build and listing that sleep just below, at and just above
// a shortened limit, and give a program and a declaration otherwise. Below the
// limit the copy is accepted; at and above it the step's limit is the finding.
// A stand-in listing that leaves out a required scenario gives that scenario's
// finding.
func TestCheckLimitsTheBuildAndTheListing(t *testing.T) {
	skipWithoutSandbox(t)
	repo, _ := newProject(t, fixtureProject(t))
	wc, err := OpenWorkingCopy(t.Context(), repo)
	require.NoError(t, err)
	writeFiles(t, filepath.Join(wc.Project(), "eval"), map[string]string{
		"acceptance/acceptance.go": fixtureAcceptance(quietInit(filepath.Join(t.TempDir(), "unused")), "RefundOverLimit", "StoreCreditWithinLimit"),
	})
	const limit = 500 * time.Millisecond
	declare := func(ids ...string) []byte {
		d := suite.Declaration{ID: "shop_acceptance", Environment: "scripted-model"}
		for _, id := range ids {
			d.Scenarios = append(d.Scenarios, suite.DeclaredScenario{ID: id, Tags: []string{}})
		}
		b, err := json.Marshal(d)
		require.NoError(t, err)
		return b
	}
	complete := declare("prices_tea", "refund_over_limit", "store_credit_within_limit")
	sleepThenBuild := func(d time.Duration) func(context.Context, candidate.BuildSpec, func(*exec.Cmd) error) error {
		return func(_ context.Context, spec candidate.BuildSpec, _ func(*exec.Cmd) error) error {
			time.Sleep(d)
			return os.WriteFile(spec.Out, []byte("stand-in program"), 0o700)
		}
	}
	sleepThenList := func(d time.Duration, out []byte) func(context.Context, candidate.Artifact, candidate.Spec, int) ([]byte, error) {
		return func(context.Context, candidate.Artifact, candidate.Spec, int) ([]byte, error) {
			time.Sleep(d)
			return out, nil
		}
	}
	cases := []struct {
		name      string
		build     time.Duration
		list      time.Duration
		listed    []byte
		wantKind  FindingKind
		wantWords string
	}{
		{name: "build just below", build: limit - 200*time.Millisecond, listed: complete},
		{name: "build at", build: limit, listed: complete, wantKind: FindingBuild, wantWords: "did not finish building within 500ms"},
		{name: "build just above", build: limit + 200*time.Millisecond, listed: complete, wantKind: FindingBuild, wantWords: "did not finish building within 500ms"},
		{name: "listing just below", list: limit - 200*time.Millisecond, listed: complete},
		{name: "listing at", list: limit, listed: complete, wantKind: FindingListing, wantWords: "did not finish within 500ms"},
		{name: "listing just above", list: limit + 200*time.Millisecond, listed: complete, wantKind: FindingListing, wantWords: "did not finish within 500ms"},
		{name: "a listing without a required scenario", listed: declare("prices_tea", "store_credit_within_limit"),
			wantKind: FindingMissingScenario, wantWords: `scenario "refund_over_limit"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := *testChecker
			c.buildLimit, c.listLimit = limit, limit
			c.build, c.list = sleepThenBuild(tc.build), sleepThenList(tc.list, tc.listed)
			v, err := c.Check(t.Context(), wc, fixtureSuite, refundStories)
			require.NoError(t, err)
			if tc.wantKind == "" {
				assert.True(t, v.Accepted(), "%+v", v.Findings)
				return
			}
			require.Len(t, v.Findings, 1, "%+v", v.Findings)
			assert.Equal(t, tc.wantKind, v.Findings[0].Kind)
			assert.Contains(t, v.Findings[0].Message, tc.wantWords)
		})
	}
}

// generatedSuite is the fixture's generated suite file, relative to its
// module.
const generatedSuite = "gen/evals/shop_acceptance/suite.go"

// skipWithoutSandbox skips a test that compiles and runs code in the sandbox
// in short mode and where the sandbox does not exist.
func skipWithoutSandbox(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("the sandbox is implemented with sandbox-exec on macOS")
	}
}

// publishedSurla is a Surla version on the module proxy whose suite contract
// has everything the generated program calls; the starter's tests use the
// same one. The fixture requires it as a starter does, because the snapshot
// a check builds may read no Surla source outside it, and generation refuses
// a replacement by a local folder.
const publishedSurla = "v0.0.0-20261110163737-d9beac313142"

// fixtureProject returns a project's files holding the fixture's test module
// under eval/, as a test writer would start from: generated without a Story,
// so with the generated program, and with an acceptance package providing the
// five names the program calls. The module requires publishedSurla for the
// suite contract the program uses.
func fixtureProject(t *testing.T) map[string]string {
	t.Helper()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.files == nil {
		design := testmodule.Module(t, "example.com/shop/eval", "", map[string]string{"design/design.go": fixtureDesign})
		generated, err := testEvaluator.Generate(t.Context(), design)
		require.NoError(t, err)
		require.Contains(t, generated, "gen/cmd/shop-evals/main.go", "the generator writes the program")
		module := maps.Clone(design)
		for p, b := range generated {
			module[p] = string(b)
		}
		module["acceptance/acceptance.go"] = fixtureAcceptance(quietInit(filepath.Join(os.TempDir(), "unused")))
		module["go.mod"] = strings.Replace(module["go.mod"], "require (",
			"require (\n\tgithub.com/goadesign/surla "+publishedSurla, 1)
		files := map[string]string{"README.md": "shop\n"}
		for p, content := range testmodule.Tidy(t, module) {
			files["eval/"+p] = content
		}
		fixture.files = files
	}
	return maps.Clone(fixture.files)
}

// fixtureAcceptance returns the fixture's acceptance package: the five names
// the generated program calls, the hand-written scenario's hook, the named
// flow methods, and an init function with initBody, which runs whenever the
// program starts, also to list its tests.
func fixtureAcceptance(initBody string, methods ...string) string {
	var b strings.Builder
	b.WriteString(`// Package acceptance holds the fixture's scenario hooks.
package acceptance

import (
	"context"
	"errors"
	"fmt"
	"os"

	"goa.design/goa-ai/eval"
	"goa.design/goa-ai/runtime/agent/model"

	"github.com/goadesign/surla/verify/candidate"
	"github.com/goadesign/surla/verify/suite"
)

// Name is the name the program under test is bound as.
const Name = "shop"

// Hooks implements the generated shop_acceptance hooks.
type Hooks struct{}

// New returns the hooks for the pinned program under test.
func New(candidate.Artifact) *Hooks { return &Hooks{} }

// Live starts no live target: the fixture has none.
func Live(context.Context, candidate.Artifact, string, model.Provider) (suite.LiveTarget, error) {
	return suite.LiveTarget{}, errors.New("the fixture has no live target")
}

// LiveThroughGateway starts no live target: the fixture has none.
func LiveThroughGateway(context.Context, candidate.Artifact, string, string) (suite.LiveTarget, error) {
	return suite.LiveTarget{}, errors.New("the fixture has no live target")
}

// init runs whenever the program starts.
func init() {
`)
	b.WriteString(initBody)
	b.WriteString("}\n")
	for _, m := range append([]string{"PricesTea"}, methods...) {
		fmt.Fprintf(&b, "\n// %s passes.\nfunc (h *Hooks) %s(context.Context) (eval.Result, error) {\n\treturn eval.Result{}, nil\n}\n", m, m)
	}
	return b.String()
}

// quietInit is an init body that prints noise on standard error, including a
// declaration-shaped JSON object, and tries to write marker, which the
// sandbox denies.
func quietInit(marker string) string {
	return fmt.Sprintf(`	fmt.Fprintln(os.Stderr, %q)
	if err := os.WriteFile(%q, []byte("outside"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "write outside the sandbox:", err)
	}
`, `{"id":"noise","environment":"stderr","scenarios":[]}`, marker)
}

// floodInit is an init body that writes 256 MiB to standard error, then a last
// line, and exits with status 3.
const floodInit = `	fmt.Fprintln(os.Stderr, "no declaration today")
	line := make([]byte, 1<<20)
	for i := range line {
		line[i] = 'x'
	}
	for range 256 {
		if _, err := os.Stderr.Write(line); err != nil {
			os.Exit(4)
		}
	}
	fmt.Fprintln(os.Stderr, "\nend of the error stream")
	os.Exit(3)
`

// hollowMain is a hand-written program that prints the required scenario
// names as a declaration without building the generated tests.
const hollowMain = `// Command shop-evals only prints the required names.
package main

import "fmt"

func main() {
	fmt.Println(` + "`" + `{"id":"shop_acceptance","environment":"scripted-model","scenarios":[{"id":"prices_tea","tags":[]},{"id":"refund_over_limit","tags":[]},{"id":"store_credit_within_limit","tags":[]}]}` + "`" + `)
}
`

// assertNoAbsolutePath fails the test when a finding quotes an absolute path.
func assertNoAbsolutePath(t *testing.T, findings []Finding) {
	t.Helper()
	for _, f := range findings {
		assert.NotRegexp(t, absolutePath, f.Message, "a finding quotes an absolute path")
	}
}

// projectFiles returns every file of the project folder except git's and the
// working copy's (whether a folder or a link in its place), with its content.
func projectFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" || d.Name() == Folder {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		files[rel] = string(b)
		return err
	}))
	return files
}
