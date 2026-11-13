package testwriter

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/testmodule"
)

// fixtureSuite locates the fixture's suite module and command in a project.
var fixtureSuite = Suite{Module: "eval", Command: "./cmd/shop-evals"}

// refundStories has two finished flows and one draft.
var refundStories = []designeval.StorySummary{{
	Name: "Refunds",
	Flows: []designeval.StoryFlow{
		{Key: "refund-over-limit", Title: "A refund above the limit waits for a manager.", Complete: true},
		{Key: "store-credit-within-limit", Title: "A small refund becomes store credit.", Complete: true},
		{Key: "disputed-chargeback", Title: "A customer opens a chargeback.", Complete: false},
	},
}}

// fixture caches the fixture suite module, which takes a sandboxed generation
// to build. Tests clone it.
var fixture struct {
	mu sync.Mutex
	// files is the module's tree keyed by slash path, nil until built.
	files map[string]string
}

// fixtureDesign is the fixture's evaluation design: one service and one
// suite with one scenario written by hand.
const fixtureDesign = `package design

import (
	. "goa.design/goa-ai/eval/dsl"
	. "goa.design/goa/v3/dsl"
)

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

// TestCheckJudgesTheWorkingCopy plays the test writer through a prepared copy:
// each step changes the copy's files and checks it. A missing hook method, an
// edit of the generated folder that drops a requirement, a renamed or removed
// scenario, a build that reads a file outside the snapshot (which only the
// build sandbox refuses) and a failing listing each return one finding naming
// what is wrong, and a listing that writes 256 MiB to its error stream is
// quoted from a bounded excerpt of its end, allocating far less than that; a
// complete copy that prints noise, including a JSON object, on standard error
// and tries to write outside its sandbox is accepted, and the
// write never happens. Recording makes one commit holding the files as
// checked, with the generated folder written from the Story, and recording
// the unchanged copy again returns that commit.
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
	// header is readable by any build but the sandboxed one, which may read
	// only the snapshot, the toolchain and the module cache.
	header := filepath.Join(t.TempDir(), "outside.h")
	require.NoError(t, os.WriteFile(header, []byte("#define OUTSIDE 1\n"), 0o600))

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
			name: "an unwritten hook method",
			files: map[string]string{
				"acceptance/acceptance.go": fixtureHooks("StoreCreditWithinLimit"),
				"cmd/shop-evals/main.go":   fixtureMain(outside, ""),
			},
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
			name: "a renamed scenario",
			files: map[string]string{
				"acceptance/acceptance.go": fixtureHooks("RefundOverLimit", "StoreCreditWithinLimit"),
				"cmd/shop-evals/main.go": fixtureMain(outside,
					`if id == "refund_over_limit" { id = "refund_over_the_limit" }`),
			},
			wantKind:    FindingMissingScenario,
			wantMessage: []string{`scenario "refund_over_limit"`, `flow "refund-over-limit"`, "RefundOverLimit"},
		},
		{
			name: "a removed scenario",
			files: map[string]string{
				"cmd/shop-evals/main.go": fixtureMain(outside, `if id == "store_credit_within_limit" { continue }`),
			},
			wantKind:    FindingMissingScenario,
			wantMessage: []string{`scenario "store_credit_within_limit"`},
		},
		{
			name: "a build that reads outside the snapshot",
			files: map[string]string{
				"cmd/shop-evals/main.go":   fixtureMain(outside, ""),
				"cmd/shop-evals/outside.s": "#include \"" + header + "\"\n",
			},
			wantKind:    FindingBuild,
			wantMessage: []string{"does not compile", "#include", "outside.h"},
		},
		{
			name:        "a listing that fails after flooding its error stream",
			files:       map[string]string{"cmd/shop-evals/main.go": floodingMain},
			remove:      []string{"cmd/shop-evals/outside.s"},
			wantKind:    FindingListing,
			wantMessage: []string{"exited with status 3", "earlier output left out", "end of the error stream"},
			// The error stream is read only as a bounded excerpt. The whole
			// check allocated 2.5 MB under the race detector (measured with
			// this 256 MiB stream); 32 MiB leaves room for that to vary and
			// is an eighth of what reading the stream whole would allocate.
			maxAlloc: 32 << 20,
		},
		{
			name:  "a complete copy that prints noise on standard error",
			files: map[string]string{"cmd/shop-evals/main.go": fixtureMain(outside, "")},
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

	commit, err := Record(t.Context(), accepted, "Add the accepted tests")
	require.NoError(t, err)
	assert.Equal(t, start, runGit(t, repo, "rev-parse", commit+"^"), "one commit on top of the branch")
	assert.Equal(t, strings.TrimSpace(string(prepared)), runGit(t, repo, "show", commit+":eval/"+generatedSuite),
		"the commit holds the generated suite written from the Story, not the author's edit")
	assert.Equal(t, "M eval/"+generatedSuite, runGit(t, wc.Dir(), "status", "--porcelain"),
		"the author's edit of the generated folder stays an uncommitted change")
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
		".gitattributes":                authorsAttributes,
		"eval/acceptance/acceptance.go": fixtureHooks("RefundOverLimit", "StoreCreditWithinLimit"),
		"eval/cmd/shop-evals/main.go":   fixtureMain(outside, ""),
	})
	marker := filepath.Join(t.TempDir(), "filter-ran")
	for _, rewrite := range []func(t *testing.T, dir, marker string){replaceCopysGit, pointCopysGitInside} {
		rewrite(t, wc.Dir(), marker)
		v, err := testChecker.Check(t.Context(), wc, fixtureSuite, refundStories)
		require.NoError(t, err)
		assert.True(t, v.Accepted(), "%+v", v.Findings)
		assert.NoFileExists(t, marker, "no program named inside the copy runs")
		commit, err := Record(t.Context(), v, "Add the accepted tests")
		require.NoError(t, err)
		assert.Equal(t, strings.TrimSpace(string(prepared)), runGit(t, repo, "show", commit+":eval/"+generatedSuite))
	}
	assert.NoFileExists(t, outside)
	assert.Empty(t, ranHooks(t, markers))
}

// floodingMain is a suite command whose --describe writes 256 MiB to its
// standard error, then a last line, and exits with status 3.
const floodingMain = `// Command shop-evals floods its error stream.
package main

import (
	"bytes"
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "no declaration today")
	line := bytes.Repeat([]byte("x"), 1<<20)
	for range 256 {
		if _, err := os.Stderr.Write(line); err != nil {
			os.Exit(4)
		}
	}
	fmt.Fprintln(os.Stderr, "\nend of the error stream")
	os.Exit(3)
}
`

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

// fixtureProject returns a project's files holding the fixture suite module
// under eval/, as a test writer would start from: generated without a Story,
// with the hand-written scenario's hook and a suite command whose
// --describe prints its declaration.
func fixtureProject(t *testing.T) map[string]string {
	t.Helper()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.files == nil {
		design := testmodule.Module(t, "example.com/shop/eval", "", map[string]string{"design/design.go": fixtureDesign})
		generated, err := testEvaluator.Generate(t.Context(), design)
		require.NoError(t, err)
		tree := maps.Clone(design)
		for p, b := range generated {
			tree[p] = string(b)
		}
		tree["acceptance/acceptance.go"] = fixtureHooks()
		tree["cmd/shop-evals/main.go"] = fixtureMain(filepath.Join(os.TempDir(), "unused"), "")
		fixture.files = testmodule.Tidy(t, tree)
	}
	project := map[string]string{"README.md": "shop\n"}
	for p, content := range fixture.files {
		project["eval/"+p] = content
	}
	return project
}

// fixtureHooks returns the fixture's hand-written hooks: the hand-written
// scenario's and the named flow methods.
func fixtureHooks(methods ...string) string {
	var b strings.Builder
	b.WriteString("// Package acceptance holds the fixture's scenario hooks.\npackage acceptance\n\n" +
		"import (\n\t\"context\"\n\n\t\"goa.design/goa-ai/eval\"\n)\n\n" +
		"// Hooks implements the generated shop_acceptance hooks.\ntype Hooks struct{}\n")
	for _, m := range append([]string{"PricesTea"}, methods...) {
		fmt.Fprintf(&b, "\n// %s passes.\nfunc (h *Hooks) %s(context.Context) (eval.Result, error) {\n\treturn eval.Result{}, nil\n}\n", m, m)
	}
	return b.String()
}

// fixtureMain returns a suite command that follows the --describe contract:
// it prints its generated suite's declaration on standard output, applying
// change to each scenario ID (id) in the loop, with noise around it on
// standard error, including a declaration-shaped JSON object. It also tries
// to write marker, which the sandbox denies.
func fixtureMain(marker, change string) string {
	return fmt.Sprintf(`// Command shop-evals is the fixture's suite command.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"example.com/shop/eval/acceptance"
	genevals "example.com/shop/eval/gen/evals/shop_acceptance"
)

type (
	declaration struct {
		ID          string     `+"`json:\"id\"`"+`
		Environment string     `+"`json:\"environment\"`"+`
		Scenarios   []scenario `+"`json:\"scenarios\"`"+`
	}
	scenario struct {
		ID   string   `+"`json:\"id\"`"+`
		Tags []string `+"`json:\"tags\"`"+`
	}
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "--describe" {
		fmt.Fprintln(os.Stderr, "usage: shop-evals --describe")
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, `+"`"+`{"id":"noise","environment":"stderr","scenarios":[]}`+"`"+`)
	if err := os.WriteFile(%q, []byte("outside"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "write outside the sandbox:", err)
	}
	s, err := genevals.New(&acceptance.Hooks{}, genevals.Inputs{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	d := declaration{ID: s.ID, Environment: "fixture", Scenarios: []scenario{}}
	for _, sc := range s.Scenarios {
		id := sc.ID
		%s
		d.Scenarios = append(d.Scenarios, scenario{ID: id, Tags: sc.Tags})
	}
	if err := json.NewEncoder(os.Stdout).Encode(d); err != nil {
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, "noise after the declaration")
}
`, marker, change)
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
