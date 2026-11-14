package testwriter

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/goadesign/surla/internal/testmodule"
	"github.com/goadesign/surla/verify/candidate"
	"github.com/goadesign/surla/verify/suite"
)

// leftoverStage is the name of a staging folder a publication that died left
// in a test module.
const leftoverStage = ".surla-gen-earlier"

// TestPublishClearsWhatAnInterruptedPublishLeft publishes into a module that
// holds an entry with a staging folder's name. A staging folder left by a
// publication that died is removed and the publication succeeds. A regular
// file with that name is the author's: it stays as it is and fails nothing.
func TestPublishClearsWhatAnInterruptedPublishLeft(t *testing.T) {
	cases := []struct {
		name string
		// before lays out the module's files before publishing.
		before map[string]string
		// emptyFolders are created empty beside them.
		emptyFolders []string
		// want is every file of the module afterwards.
		want map[string]string
	}{
		{
			name: "a staging folder left before the exchange",
			before: map[string]string{
				"gen/evals/suite.go": "old\n", leftoverStage + "/gen/evals/suite.go": "half written\n",
			},
			want: map[string]string{"gen/evals/suite.go": "new\n"},
		},
		{
			name:         "a staging folder left before its gen was created, and no gen yet",
			emptyFolders: []string{leftoverStage},
			want:         map[string]string{"gen/evals/suite.go": "new\n"},
		},
		{
			name:   "a regular file with a staging folder's name",
			before: map[string]string{"gen/evals/suite.go": "old\n", leftoverStage: "the author's\n"},
			want:   map[string]string{"gen/evals/suite.go": "new\n", leftoverStage: "the author's\n"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, tc.before)
			for _, folder := range tc.emptyFolders {
				require.NoError(t, os.Mkdir(filepath.Join(dir, folder), 0o750))
			}
			root, err := os.OpenRoot(dir)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, root.Close()) })

			require.NoError(t, publish(t.Context(), root, map[string][]byte{"gen/evals/suite.go": []byte("new\n")}))
			assert.Equal(t, tc.want, projectFiles(t, dir))
			if _, ok := tc.want[leftoverStage]; !ok {
				assert.NoDirExists(t, filepath.Join(dir, leftoverStage))
			}
		})
	}
}

// TestPublishRecordsWarningsWithoutFailing publishes where the generated
// folder is replaced but work beside it is not done: a folder stands where
// the agent guide belongs, and a folder with a staging folder's name holds a
// file no publication stages. The publication succeeds, the generated folder
// holds the new files, and the span says what was left.
func TestPublishRecordsWarningsWithoutFailing(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"gen/evals/suite.go": "old\n", "AGENTS_QUICKSTART.md/keep.txt": "a folder\n",
		leftoverStage + "/notes.txt": "the author's\n",
	})
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, root.Close()) })

	ctx, span := provider.Tracer("test").Start(t.Context(), "publish")
	err = publish(ctx, root, map[string][]byte{
		"gen/evals/suite.go": []byte("new\n"), "AGENTS_QUICKSTART.md": []byte("guide\n"),
	})
	span.End()
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"gen/evals/suite.go": "new\n", "AGENTS_QUICKSTART.md/keep.txt": "a folder\n",
		leftoverStage + "/notes.txt": "the author's\n",
	}, projectFiles(t, dir))
	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	require.Len(t, spans[0].Events, 1)
	assert.Equal(t, "the generated folder was written with warnings", spans[0].Events[0].Name)
	require.Len(t, spans[0].Events[0].Attributes, 1)
	warnings := spans[0].Events[0].Attributes[0].Value.AsStringSlice()
	require.Len(t, warnings, 2)
	assert.Contains(t, warnings[0], leftoverStage)
	assert.Contains(t, warnings[1], "AGENTS_QUICKSTART.md was not")
}

// TestALeftoverStagingFolderFailsNeitherPrepareNorCheck leaves a staging
// folder in the copy's test module, as a server that died while writing the
// required tests does. Prepare removes it and succeeds. Left there again, as
// by a death during a report's preparation, it does not stop Check, which
// accepts the complete copy, and the tree it accepts does not hold it.
func TestALeftoverStagingFolderFailsNeitherPrepareNorCheck(t *testing.T) {
	skipWithoutSandbox(t)
	repo, _ := newProject(t, fixtureProject(t))
	wc, err := OpenWorkingCopy(t.Context(), repo)
	require.NoError(t, err)
	module := filepath.Join(wc.Project(), "eval")
	stage := map[string]string{leftoverStage + "/gen/evals/shop_acceptance/suite.go": "package half\n"}
	writeFiles(t, module, stage)

	_, err = testChecker.Prepare(t.Context(), wc, fixtureSuite, refundStories)
	require.NoError(t, err, "a staging folder left by an interrupted publication does not fail the preparation")
	assert.NoDirExists(t, filepath.Join(module, leftoverStage), "the preparation removed it")
	suiteGo, err := os.ReadFile(filepath.Join(module, generatedSuite))
	require.NoError(t, err)
	assert.Contains(t, string(suiteGo), "RefundOverLimit(context.Context) (eval.Result, error)")

	writeFiles(t, module, stage)
	writeFiles(t, module, map[string]string{
		"acceptance/acceptance.go": fixtureAcceptance(quietInit(filepath.Join(t.TempDir(), "unused")), "RefundOverLimit", "StoreCreditWithinLimit"),
	})
	c := *testChecker
	c.build = func(_ context.Context, spec candidate.BuildSpec, _ func(*exec.Cmd) error) error {
		return os.WriteFile(spec.Out, []byte("stand-in program"), 0o700)
	}
	c.list = func(context.Context, candidate.Artifact, candidate.Spec, int) ([]byte, error) {
		d := suite.Declaration{ID: "shop_acceptance", Environment: "scripted-model"}
		for _, id := range []string{"prices_tea", "refund_over_limit", "store_credit_within_limit"} {
			d.Scenarios = append(d.Scenarios, suite.DeclaredScenario{ID: id, Tags: []string{}})
		}
		return json.Marshal(d)
	}
	v, err := c.Check(t.Context(), wc, fixtureSuite, refundStories)
	require.NoError(t, err, "a staging folder in the copy does not stop the check")
	require.True(t, v.Accepted(), "%+v", v.Findings)
	assert.NotContains(t, runGit(t, repo, "ls-tree", "-r", "--name-only", v.tree), leftoverStage,
		"the accepted tree holds the generated folder, not the staging folder")
}

// TestALinkWithAStagingFoldersNameIsTheAuthorsFinding plants a symbolic link
// with a staging folder's name in the copy's test module, pointing at a
// folder outside the copy that holds a gen. Prepare never follows it: the
// folder is byte-identical afterwards, and the link is reported as any other
// link in the module is, as the author's finding naming it.
func TestALinkWithAStagingFoldersNameIsTheAuthorsFinding(t *testing.T) {
	t.Parallel()
	files := map[string]string{}
	for p, content := range testmodule.Module(t, "example.com/shop/eval", "", map[string]string{"design/design.go": fixtureDesign}) {
		files["eval/"+p] = content
	}
	repo, _ := newProject(t, files)
	wc, err := OpenWorkingCopy(t.Context(), repo)
	require.NoError(t, err)
	outside := t.TempDir()
	writeFiles(t, outside, map[string]string{"gen/theirs.go": "theirs\n", "keep.txt": "keep\n"})
	link := filepath.Join(wc.Project(), "eval", leftoverStage)
	require.NoError(t, os.Symlink(outside, link))

	_, err = testChecker.Prepare(t.Context(), wc, Suite{Module: "eval", Command: "./cmd/shop-evals"}, nil)
	var author *AuthorError
	require.ErrorAs(t, err, &author, "a link the author placed is the author's to fix")
	assert.Equal(t, FindingGeneration, author.Finding.Kind)
	assert.Contains(t, author.Finding.Message, "eval/"+leftoverStage+" is a symbolic link")
	assert.Equal(t, map[string]string{"gen/theirs.go": "theirs\n", "keep.txt": "keep\n"}, projectFiles(t, outside))
	target, err := os.Readlink(link)
	require.NoError(t, err, "the link stays")
	assert.Equal(t, outside, target)
}
