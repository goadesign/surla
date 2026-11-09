package scaffold

import (
	"context"
	"encoding/json"
	"errors"
	"go/format"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// published is a Surla version that exists on the module proxy.
const published = "v0.0.0-20261104215521-580509970fb6"

func TestValidVersionAcceptsOnlyExactVersions(t *testing.T) {
	for v, want := range map[string]bool{
		"v1.2.3":     true,
		"v1.2.3-pre": true,
		published:    true,
		"v0.0.0-20261104215521-580509970fb6+dirty": false,
		"latest":  false,
		"main":    false,
		"8ec6e09": false,
		"(devel)": false,
		"":        false,
		"v1.2":    false,
		"1.2.3":   false,
	} {
		assert.Equal(t, want, ValidVersion(v), v)
	}
}

func TestSurlaVersionReadsSurlaModuleFromStandaloneAndEmbeddedBuildInfo(t *testing.T) {
	assert.Equal(t, "", SurlaVersion(nil))
	assert.Equal(t, published, SurlaVersion(&debug.BuildInfo{
		Main: debug.Module{
			Path:    surlaModule,
			Version: published,
		},
	}))
	assert.Equal(t, "", SurlaVersion(&debug.BuildInfo{
		Main: debug.Module{
			Path:    surlaModule,
			Version: "(devel)",
		},
	}))
	// Embedded host application counterexample: Main.Version is the host app's
	// version (v2.4.1), while Surla's version lives in Deps.
	assert.Equal(t, published, SurlaVersion(&debug.BuildInfo{
		Main: debug.Module{
			Path:    "example.com/host-app",
			Version: "v2.4.1",
		},
		Deps: []*debug.Module{
			{
				Path:    "example.com/other",
				Version: "v1.0.0",
			},
			{
				Path:    surlaModule,
				Version: published,
			},
		},
	}))
	assert.Equal(t, "", SurlaVersion(&debug.BuildInfo{
		Main: debug.Module{
			Path:    "example.com/host-app",
			Version: "v2.4.1",
		},
		Deps: []*debug.Module{
			{
				Path:    surlaModule,
				Version: published,
				Replace: &debug.Module{
					Path: "../surla",
				},
			},
		},
	}))
	assert.Equal(t, "v0.2.0", SurlaVersion(&debug.BuildInfo{
		Main: debug.Module{
			Path:    "example.com/host-app",
			Version: "v2.4.1",
		},
		Deps: []*debug.Module{
			{
				Path:    surlaModule,
				Version: "v0.1.0",
				Replace: &debug.Module{
					Path:    surlaModule,
					Version: "v0.2.0",
				},
			},
		},
	}))
}

func TestNewDataValidatesTheModuleAndReadsSurlasVersions(t *testing.T) {
	d, err := newData(Options{Module: "example.com/shop", SurlaVersion: published})
	require.NoError(t, err)
	assert.Equal(t, "shop", d.Name)
	assert.True(t, strings.HasPrefix(d.GoaVersion, "v3."), d.GoaVersion)
	assert.NotEmpty(t, d.GoaAIVersion)
	assert.NotEmpty(t, d.ModelVersion)
	assert.NotEmpty(t, d.GoVersion)

	for _, tc := range []struct {
		module, version, wantErr string
	}{
		{"example.com/my-shop", published, "must end in a service name"},
		{"example.com/Shop", published, "must end in a service name"},
		{"example.com/func", published, "must end in a service name"},
		{"example.com/main", published, "must end in a service name"},
		{"not a path", published, "-module"},
		{"example.com/shop", "latest", "not an exact module version"},
	} {
		_, err := newData(Options{Module: tc.module, SurlaVersion: tc.version})
		assert.ErrorContains(t, err, tc.wantErr, tc.module+" "+tc.version)
	}
}

func TestInitRefusesAnExistingDirectory(t *testing.T) {
	parent := t.TempDir()
	empty := filepath.Join(parent, "empty")
	require.NoError(t, os.Mkdir(empty, 0o755))
	full := filepath.Join(parent, "full")
	require.NoError(t, os.Mkdir(full, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(full, "keep.txt"), []byte("mine"), 0o600))
	link := filepath.Join(parent, "link")
	require.NoError(t, os.Symlink(empty, link))
	for _, dir := range []string{empty, full, link} {
		gen := &fakeGenerator{}
		_, err := Init(t.Context(), Options{Dir: dir, WorkDir: t.TempDir(), Module: "example.com/shop", SurlaVersion: published}, failRunner(t), gen)
		require.ErrorContains(t, err, "already exists", dir)
		assert.ErrorContains(t, err, "-dir "+filepath.Join(dir, "shop"), dir)
		assert.ErrorContains(t, err, "change to the parent directory and remove an empty directory first", dir)
		assert.Equal(t, 1, gen.closes, "Init closes the generator it is given on every return")
	}
	assertOnly(t, parent, "empty", "full", "link")
	assertOnly(t, empty)
	assertOnly(t, full, "keep.txt")
}

func TestInitLeavesNothingWhenAStepFails(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "shop")
	var ran [][]string
	run := func(_ context.Context, moduleDir, logPath string, args ...string) error {
		ran = append(ran, args)
		out := "ok"
		if args[1] == "vet" {
			out = strings.Repeat("x", 5000) + "internal/service/service.go:3: boom"
		}
		if err := os.WriteFile(logPath, []byte(out), 0o600); err != nil {
			return err
		}
		if args[1] == "vet" {
			return errors.New("exit status 1")
		}
		return fakeGo(moduleDir, args)
	}
	_, err := Init(t.Context(), Options{Dir: dir, WorkDir: t.TempDir(), Module: "example.com/shop", SurlaVersion: published}, run, &fakeGenerator{pkgs: []string{"shop"}})
	var step *StepError
	require.ErrorAs(t, err, &step)
	assert.Equal(t, []string{"go", "vet", "./..."}, step.Args)
	assert.Contains(t, err.Error(), "internal/service/service.go:3: boom")
	assert.True(t, strings.HasPrefix(step.Tail, "...\n"), "only the end of a long log is quoted")
	assert.Len(t, step.Tail, len("...\n")+maxTail)
	full, err := os.ReadFile(step.Log)
	require.NoError(t, err, "the failed step's log is kept")
	assert.Len(t, full, 5000+len("internal/service/service.go:3: boom"))
	assert.Equal(t, [][]string{{"go", "mod", "tidy"}, {"go", "mod", "tidy"}, step.Args}, ran)
	assertOnly(t, parent)
}

func TestInitNeverReplacesADirectoryCreatedMeanwhile(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "shop")
	run := func(_ context.Context, moduleDir, _ string, args ...string) error {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "late.txt"), []byte("written meanwhile"), 0o600); err != nil {
			return err
		}
		return fakeGo(moduleDir, args)
	}
	_, err := Init(t.Context(), Options{Dir: dir, WorkDir: t.TempDir(), Module: "example.com/shop", SurlaVersion: published}, run, &fakeGenerator{pkgs: []string{"shop"}})
	require.ErrorContains(t, err, "move project into")
	assertOnly(t, parent, "shop")
	assertOnly(t, dir, "late.txt")
}

func TestInitKeepsStagingWhenProcessesMayStillRun(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "shop")
	run := func(_ context.Context, _, _ string, _ ...string) error {
		return &stuckError{err: errors.New("process group did not exit")}
	}
	work := t.TempDir()
	gen := &fakeGenerator{}
	_, err := Init(t.Context(), Options{Dir: dir, WorkDir: work, Module: "example.com/shop", SurlaVersion: published}, run, gen)
	assert.Equal(t, 1, gen.closes)
	var step *StepError
	require.ErrorAs(t, err, &step)
	assert.Equal(t, []string{"go", "mod", "tidy"}, step.Args)
	assert.Equal(t, ".", step.Module)
	assert.Empty(t, step.Tail, "a log that may still be written is not read")
	assert.Contains(t, err.Error(), "is kept because processes may still use it")
	entries, err := os.ReadDir(work)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.True(t, strings.HasPrefix(entries[0].Name(), ".surla-init-"), entries[0].Name())
	assert.DirExists(t, filepath.Dir(step.Log), "logs remain while processes may still write them")
	assertOnly(t, parent)
}

func TestInitStopsAndCleansUpWhenCanceled(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "shop")
	ctx, cancel := context.WithCancel(t.Context())
	run := func(ctx context.Context, dir, logPath string, _ ...string) error {
		time.AfterFunc(100*time.Millisecond, cancel)
		return RunCommand(ctx, dir, logPath, "sleep", "30")
	}
	start := time.Now()
	_, err := Init(ctx, Options{Dir: dir, WorkDir: t.TempDir(), Module: "example.com/shop", SurlaVersion: published}, run, &fakeGenerator{})
	require.Error(t, err)
	assert.Less(t, time.Since(start), 10*time.Second, "the canceled command's process group is stopped")
	assertOnly(t, parent)
}

func TestInitWritesTheProjectWithTheServiceName(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "bakery")
	var ran []string
	run := func(_ context.Context, dir, _ string, args ...string) error {
		ran = append(ran, strings.Join(args, " "))
		return fakeGo(dir, args)
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	gen := &fakeGenerator{pkgs: []string{"bakery"}, extra: map[string]string{quickstartGuide: "guide\n"}}
	name, err := Init(t.Context(), Options{Dir: dir, WorkDir: t.TempDir(), Module: "example.com/bakery", SurlaVersion: published}, run, gen)
	require.NoError(t, err)
	assertOnly(t, tmp)
	assert.Equal(t, "bakery", name)
	assert.Equal(t, []string{
		"go mod tidy", "go mod tidy", "go vet ./...", "go test ./...",
		"go mod tidy", "go get github.com/goadesign/surla@" + published, "go mod tidy", "go vet ./...", "go test ./...",
	}, ran, "no command generates code on the host")
	require.Len(t, gen.inputs, 2, "one generation per module")
	assert.Equal(t, []string{"design/design.go", "design/model.go", "go.mod", "go.sum"}, slices.Sorted(maps.Keys(gen.inputs[0])))
	assert.Contains(t, gen.inputs[0]["go.mod"], "module example.com/bakery\n")
	assert.Equal(t, []string{"design/design.go", "go.mod", "go.sum"}, slices.Sorted(maps.Keys(gen.inputs[1])))
	assert.Contains(t, gen.inputs[1]["go.mod"], "module example.com/bakery/eval\n")
	assert.Equal(t, 1, gen.closes)
	rootGoMod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	require.NoError(t, err)
	evalGoMod, err := os.ReadFile(filepath.Join(dir, "eval", "go.mod"))
	require.NoError(t, err)
	layout, err := StarterLayout(rootGoMod, evalGoMod)
	require.NoError(t, err, "surla serve -project accepts the modules Init writes")
	for _, pkgDir := range []string{
		filepath.Join(dir, filepath.FromSlash(layout.CandidatePackage)),
		filepath.Join(dir, SuiteDir, filepath.FromSlash(layout.SuiteCommand)),
	} {
		info, err := os.Stat(pkgDir)
		require.NoError(t, err, "Init writes the command directory the layout names")
		assert.True(t, info.IsDir())
	}
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	err = filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.HasSuffix(p, ".go") {
			return err
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		formatted, err := format.Source(src)
		if err != nil {
			return err
		}
		assert.Equal(t, string(formatted), string(src), "%s is gofmt-formatted", p)
		return nil
	})
	require.NoError(t, err)
	for _, f := range []string{
		"go.mod", ".gitignore", "README.md", "design/design.go", "design/model.go",
		"internal/helper/planner.go", "internal/service/service.go", "cmd/bakery/main.go",
		"eval/go.mod", "eval/design/design.go", "eval/scripted/scripted.go",
		"eval/acceptance/acceptance.go", "eval/cmd/bakery-evals/main.go",
		"gen/bakery/service.go", quickstartGuide, "eval/gen/bakery/service.go", "eval/" + quickstartGuide,
	} {
		assert.FileExists(t, filepath.Join(dir, f))
	}
	mod, err := os.ReadFile(filepath.Join(dir, "eval", "go.mod"))
	require.NoError(t, err)
	assert.Contains(t, string(mod), "module example.com/bakery/eval")
	assert.NotContains(t, string(mod), "replace")
	main, err := os.ReadFile(filepath.Join(dir, "cmd", "bakery", "main.go"))
	require.NoError(t, err)
	assert.Contains(t, string(main), `genbakery "example.com/bakery/gen/bakery"`)
	assert.NotContains(t, string(main), "[[")
	assertOnly(t, parent, "bakery")
}

func TestInitImportsThePackageGoaGenerated(t *testing.T) {
	// Brackets in the destination path must not be read as glob syntax.
	parent := filepath.Join(t.TempDir(), "demo[1]")
	require.NoError(t, os.Mkdir(parent, 0o755))
	dir := filepath.Join(parent, "errors")
	run := func(_ context.Context, moduleDir, _ string, args ...string) error {
		return fakeGo(moduleDir, args)
	}
	name, err := Init(t.Context(), Options{Dir: dir, WorkDir: t.TempDir(), Module: "example.com/errors", SurlaVersion: published}, run, &fakeGenerator{pkgs: []string{"errors_"}})
	require.NoError(t, err)
	assert.Equal(t, "errors", name)
	main, err := os.ReadFile(filepath.Join(dir, "cmd", "errors", "main.go"))
	require.NoError(t, err)
	assert.Contains(t, string(main), `generrors_ "example.com/errors/gen/errors_"`)
	assert.Contains(t, string(main), `genhttperrors_ "example.com/errors/gen/http/errors_/server"`)
	svc, err := os.ReadFile(filepath.Join(dir, "internal", "service", "service.go"))
	require.NoError(t, err)
	assert.Contains(t, string(svc), `"example.com/errors/gen/errors_/agents/helper"`)
	acc, err := os.ReadFile(filepath.Join(dir, "eval", "acceptance", "acceptance.go"))
	require.NoError(t, err)
	assert.Contains(t, string(acc), `"example.com/errors/eval/gen/http/errors_/client"`)
	assert.Contains(t, string(acc), `generrors_ "example.com/errors/eval/gen/errors_"`)
	design, err := os.ReadFile(filepath.Join(dir, "design", "design.go"))
	require.NoError(t, err)
	assert.Contains(t, string(design), `Service("errors"`, "the DSL keeps the service name")
}

func TestInitRequiresExactlyOneGeneratedService(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pkgs    []string
		wantErr string
	}{
		{name: "none", wantErr: "produced 0 service packages"},
		{name: "two", pkgs: []string{"shop", "other"}, wantErr: "produced 2 service packages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			run := func(_ context.Context, moduleDir, _ string, args ...string) error {
				return fakeGo(moduleDir, args)
			}
			_, err := Init(t.Context(), Options{Dir: filepath.Join(parent, "shop"), WorkDir: t.TempDir(), Module: "example.com/shop", SurlaVersion: published}, run, &fakeGenerator{pkgs: tc.pkgs})
			require.ErrorContains(t, err, tc.wantErr)
			assertOnly(t, parent)
		})
	}
}

func TestInitLeavesNoProjectWhenGenerationFails(t *testing.T) {
	cases := []struct {
		name string
		// gen is the generator.
		gen *fakeGenerator
		// cancelInGenerate cancels Init's context during the last
		// generation, after which every remaining step succeeds.
		cancelInGenerate bool
		// wantErr is part of the expected error.
		wantErr string
	}{
		{name: "generation fails", gen: &fakeGenerator{err: errors.New("design/design.go:3: boom")}, wantErr: "generate code for module .: design/design.go:3: boom"},
		{name: "closing the generator fails", gen: &fakeGenerator{pkgs: []string{"shop"}, closeErr: errors.New("workspace not removed")}, wantErr: "workspace not removed"},
		{name: "path outside the module", gen: &fakeGenerator{pkgs: []string{"shop"}, extra: map[string]string{"../escape.go": "x"}}, wantErr: `refusing to write "../escape.go"`},
		{name: "path outside gen/", gen: &fakeGenerator{pkgs: []string{"shop"}, extra: map[string]string{"design/design.go": "x"}}, wantErr: `refusing to write "design/design.go"`},
		{name: "unclean path", gen: &fakeGenerator{pkgs: []string{"shop"}, extra: map[string]string{"gen/../go.mod": "x"}}, wantErr: `refusing to write "gen/../go.mod"`},
		{name: "canceled after generation", gen: &fakeGenerator{pkgs: []string{"shop"}}, cancelInGenerate: true, wantErr: context.Canceled.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancelInGenerate {
				tc.gen.onGenerate = func(n int) {
					if n == 2 {
						cancel()
					}
				}
			}
			run := func(_ context.Context, moduleDir, _ string, args ...string) error {
				return fakeGo(moduleDir, args)
			}
			_, err := Init(ctx, Options{Dir: filepath.Join(parent, "shop"), WorkDir: t.TempDir(), Module: "example.com/shop", SurlaVersion: published}, run, tc.gen)
			require.ErrorContains(t, err, tc.wantErr)
			assertOnly(t, parent)
			assert.Equal(t, 1, tc.gen.closes)
		})
	}
}

// TestInitReadmeFirstRunSnippetsMatchTheProject verifies the starter README's
// copy-and-run material against the project it describes: every scenario the
// evaluator design declares is named, the verification configuration written
// after the first commit is a JSON object with exactly the fields surla serve's
// -verification-config requires, the Claude Code .mcp.json example is valid
// JSON that names the default MCP endpoint and takes the bearer token from
// SURLA_AUTH_TOKEN, the change-request step names propose_change's
// design_source field with the project's design files, and .gitignore keeps
// .mcp.json and .surla/ out of git.
func TestInitReadmeFirstRunSnippetsMatchTheProject(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bakery")
	run := func(_ context.Context, moduleDir, _ string, args ...string) error {
		return fakeGo(moduleDir, args)
	}
	t.Setenv("TMPDIR", t.TempDir())
	gen := &fakeGenerator{pkgs: []string{"bakery"}, extra: map[string]string{quickstartGuide: "guide\n"}}
	_, err := Init(t.Context(), Options{Dir: dir, WorkDir: t.TempDir(), Module: "example.com/bakery", SurlaVersion: published}, run, gen)
	require.NoError(t, err)
	readmeRaw, err := os.ReadFile(filepath.Join(dir, "README.md"))
	require.NoError(t, err)
	readme := string(readmeRaw)
	evalDesign, err := os.ReadFile(filepath.Join(dir, "eval", "design", "design.go"))
	require.NoError(t, err)

	scenarios := regexp.MustCompile(`Scenario\("([a-z_]+)"`).FindAllStringSubmatch(string(evalDesign), -1)
	require.NotEmpty(t, scenarios)
	for _, m := range scenarios {
		assert.Contains(t, readme, "`"+m[1]+"`", "README names scenario %s", m[1])
	}

	config := between(t, readme, "cat > .surla/verification.json <<EOF\n", "\nEOF\n")
	assert.NotContains(t, config, "$PWD", "paths stay relative so any valid project path writes valid JSON")
	config = strings.ReplaceAll(config, "$(git rev-parse HEAD)", strings.Repeat("a", 40))
	var fields map[string]any
	require.NoError(t, json.Unmarshal([]byte(config), &fields), config)
	// These are the fields cmd/surla's verificationFile defines and
	// loadVerificationConfig requires; keep them in step with that type.
	assert.Equal(t, []string{
		"binaries", "candidate_module", "candidate_repo", "report_dir", "runs_dir",
		"suite_command", "suite_commit", "suite_module", "suite_repo", "timeout",
	}, slices.Sorted(maps.Keys(fields)))
	assert.Equal(t, map[string]any{"bakery": "./cmd/bakery"}, fields["binaries"])
	assert.Equal(t, "./cmd/bakery-evals", fields["suite_command"])
	assert.Equal(t, ".", fields["candidate_repo"])
	assert.Equal(t, ".", fields["suite_repo"])
	assert.Contains(t, readme, "-project .")
	assert.Contains(t, readme, "-verification-config .surla/verification.json")
	assert.Contains(t, readme, "-report-dir reports -builder-view")
	assert.Contains(t, readme, "while `-builder-view` hides holdout scenario names and checks\nin the completed report JSON and summary line (if a run fails before writing a\nreport, `stderr` prints operator setup/execution failure diagnostics).")
	assert.Contains(t, readme, "-model-provider openai -model-name \"$MODEL_NAME\"")
	assert.Contains(t, readme, "surla://golden-path")
	assert.Contains(t, readme, "http://127.0.0.1:8090/golden-path")
	assert.Contains(t, readme, "planner.ToolFailureProvider")
	assert.Contains(t, readme, "use\n  `surla gen` and `surla verify` in place of the guide's `goa gen` and\n  `go run` commands")

	candDesign, err := os.ReadFile(filepath.Join(dir, "design", "design.go"))
	require.NoError(t, err)
	assert.Contains(t, string(candDesign), `Required("prompt_id", "template")`, "override_prompt leaves key optional so a missing X-Live-Target-Key header reaches APIKeyAuth and answers 401")
	assert.NotContains(t, string(candDesign), `Required("key", "prompt_id", "template")`)

	evalAcceptance, err := os.ReadFile(filepath.Join(dir, "eval", "acceptance", "acceptance.go"))
	require.NoError(t, err)
	assert.Contains(t, string(evalAcceptance), `"missing_key_rejected"`)

	mcpSection := between(t, readme, "Register the server with your coding agent.", "Other coding agents")
	mcp := between(t, mcpSection, "```json\n", "\n```")
	var mcpConfig struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcp), &mcpConfig), mcp)
	server := mcpConfig.MCPServers["surla"]
	assert.Equal(t, "http", server.Type)
	assert.Equal(t, "http://127.0.0.1:8090/mcp", server.URL)
	assert.Equal(t, "Bearer ${SURLA_AUTH_TOKEN}", server.Headers["Authorization"])
	assert.Contains(t, readme, "export SURLA_AUTH_TOKEN=")

	proposal := between(t, readme, "To start a change,", "To try prompt wording")
	assert.Contains(t, proposal, "`propose_change`")
	assert.Contains(t, proposal, "`design/*.go` files as `design_source`")
	assert.Contains(t, proposal, "`confirm_intent`")
	assert.Contains(t, proposal, "`request_revision`")

	ignore, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	require.NoError(t, err)
	assert.Contains(t, strings.Split(string(ignore), "\n"), "/.surla/")
	assert.Contains(t, strings.Split(string(ignore), "\n"), "/.mcp.json")
}

// TestPrepareCommitsInitialScaffoldAndStripsRedirectingGitEnv verifies that
// Prepare runs `go test ./...` in both modules and creates the initial Git
// commit in the protected staging directory using the operator's Git identity
// (including via GIT_CONFIG_COUNT) while stripping inherited
// repository-redirecting GIT_* variables and neutralizing hooks, fsmonitor,
// templates, attributes, and clean/smudge filters.
func TestPrepareCommitsInitialScaffoldAndStripsRedirectingGitEnv(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "shop")
	work := t.TempDir()
	poisonRepo := t.TempDir()
	hookDir := t.TempDir()
	tmplDir := t.TempDir()
	attrDir := t.TempDir()

	hookMarker := filepath.Join(t.TempDir(), "hook-ran")
	hookScript := "#!/bin/sh\ntouch " + strconv.Quote(hookMarker) + "\nexit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, "pre-commit"), []byte(hookScript), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, "commit-msg"), []byte(hookScript), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, "post-commit"), []byte(hookScript), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(tmplDir, "hooks"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmplDir, "hooks", "pre-commit"), []byte(hookScript), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(tmplDir, "poison-template-file"), []byte("poison"), 0o600))
	attrFile := filepath.Join(attrDir, "gitattributes")
	require.NoError(t, os.WriteFile(attrFile, []byte("* filter=rot\n"), 0o600))

	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "")
	t.Setenv("GIT_AUTHOR_EMAIL", "")
	t.Setenv("GIT_COMMITTER_NAME", "")
	t.Setenv("GIT_COMMITTER_EMAIL", "")
	t.Setenv("GIT_CONFIG_COUNT", "7")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "Config Count Operator")
	t.Setenv("GIT_CONFIG_KEY_1", "user.email")
	t.Setenv("GIT_CONFIG_VALUE_1", "count-operator@example.com")
	t.Setenv("GIT_CONFIG_KEY_2", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_2", hookDir)
	t.Setenv("GIT_CONFIG_KEY_3", "core.fsmonitor")
	t.Setenv("GIT_CONFIG_VALUE_3", filepath.Join(hookDir, "pre-commit"))
	t.Setenv("GIT_CONFIG_KEY_4", "init.templateDir")
	t.Setenv("GIT_CONFIG_VALUE_4", tmplDir)
	t.Setenv("GIT_CONFIG_KEY_5", "core.attributesFile")
	t.Setenv("GIT_CONFIG_VALUE_5", attrFile)
	t.Setenv("GIT_CONFIG_KEY_6", "filter.rot.clean")
	t.Setenv("GIT_CONFIG_VALUE_6", "sh -c 'touch "+hookMarker+"; exit 1'")
	t.Setenv("GIT_DIR", filepath.Join(poisonRepo, ".git"))
	t.Setenv("GIT_WORK_TREE", poisonRepo)
	t.Setenv("GIT_TEMPLATE_DIR", tmplDir)

	var ran []string
	run := func(ctx context.Context, moduleDir, logPath string, args ...string) error {
		ran = append(ran, strings.Join(args, " "))
		if len(args) > 0 && args[0] == "git" {
			return RunCommand(ctx, moduleDir, logPath, args...)
		}
		if err := os.WriteFile(logPath, []byte("ok\n"), 0o600); err != nil {
			return err
		}
		return fakeGo(moduleDir, args)
	}
	gen := &fakeGenerator{pkgs: []string{"shop"}, extra: map[string]string{quickstartGuide: "guide\n"}}
	name, stagePath, commit, err := Prepare(
		t.Context(),
		Options{Dir: dir, WorkDir: work, Module: "example.com/shop", SurlaVersion: published},
		run,
		gen,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, os.RemoveAll(stagePath))
	})
	assert.Equal(t, "shop", name)
	assert.Regexp(t, `^([0-9a-f]{40}|[0-9a-f]{64})$`, commit)
	gitPrefix := "git -c core.hooksPath=/dev/null -c core.fsmonitor=false -c core.worktree= -c init.templateDir= -c core.attributesFile=/dev/null -c core.autocrlf=false -c core.safecrlf=false"
	repoPrefix := gitPrefix + " --git-dir=.git --work-tree=."
	assert.Equal(t, []string{
		"go mod tidy", "go mod tidy", "go vet ./...", "go test ./...",
		"go mod tidy", "go get github.com/goadesign/surla@" + published, "go mod tidy", "go vet ./...", "go test ./...",
		gitPrefix + " init --template=",
		repoPrefix + " add -A",
		repoPrefix + " commit --no-verify -m Initial scaffold",
		repoPrefix + " rev-parse --verify HEAD^{commit}",
	}, ran)
	_, err = os.Stat(hookMarker)
	assert.ErrorIs(t, err, os.ErrNotExist, "hooks, fsmonitor, templates, and clean filters must not execute")
	_, err = os.Stat(filepath.Join(stagePath, ".git", "poison-template-file"))
	assert.ErrorIs(t, err, os.ErrNotExist, "init.templateDir and GIT_TEMPLATE_DIR must not copy files into .git")
	_, err = os.Stat(filepath.Join(poisonRepo, ".git"))
	assert.ErrorIs(t, err, os.ErrNotExist, "RunCommand must strip inherited GIT_DIR and GIT_WORK_TREE")
	_, err = os.Stat(filepath.Join(stagePath, ".git"))
	require.NoError(t, err, "staged project has its own .git directory")
}

// TestPrepareFailsClosedWhenGitIdentityIsMissing verifies that Prepare fails
// without fabricating a fallback author when the operator has no configured
// Git identity, retains the failed command log, and removes the staged tree.
func TestPrepareFailsClosedWhenGitIdentityIsMissing(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "shop")
	work := t.TempDir()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "")
	t.Setenv("GIT_AUTHOR_EMAIL", "")
	t.Setenv("GIT_COMMITTER_NAME", "")
	t.Setenv("GIT_COMMITTER_EMAIL", "")
	t.Setenv("EMAIL", "")

	run := func(ctx context.Context, moduleDir, logPath string, args ...string) error {
		if len(args) > 0 && args[0] == "git" {
			return RunCommand(ctx, moduleDir, logPath, args...)
		}
		if err := os.WriteFile(logPath, []byte("ok\n"), 0o600); err != nil {
			return err
		}
		return fakeGo(moduleDir, args)
	}
	gen := &fakeGenerator{pkgs: []string{"shop"}}
	_, _, _, err := Prepare(
		t.Context(),
		Options{Dir: dir, WorkDir: work, Module: "example.com/shop", SurlaVersion: published},
		run,
		gen,
	)
	var step *StepError
	require.ErrorAs(t, err, &step)
	assert.Equal(t, []string{
		"git",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "core.worktree=",
		"-c", "init.templateDir=",
		"-c", "core.attributesFile=/dev/null",
		"-c", "core.autocrlf=false",
		"-c", "core.safecrlf=false",
		"--git-dir=.git",
		"--work-tree=.",
		"commit",
		"--no-verify",
		"-m",
		"Initial scaffold",
	}, step.Args)
	assert.FileExists(t, step.Log)
	assertOnly(t, parent)
}

// TestStarterLayoutValidatesModulesAndDerivesPaths verifies that StarterLayout
// derives the same service name and command paths as Init, including when the
// service name is reserved in generated Go code (such as "errors"), and
// rejects malformed, missing, invalid or mismatched module declarations.
func TestStarterLayoutValidatesModulesAndDerivesPaths(t *testing.T) {
	cases := []struct {
		name      string
		rootGoMod string
		evalGoMod string
		want      Layout
		wantErr   string
	}{
		{
			name:      "standard starter module pair",
			rootGoMod: "module example.com/shop\n\ngo 1.26\n",
			evalGoMod: "module example.com/shop/eval\n\ngo 1.26\n",
			want: Layout{
				Service:          "shop",
				CandidatePackage: "./cmd/shop",
				SuiteCommand:     "./cmd/shop-evals",
			},
		},
		{
			name:      "service name reserved in generated Go code",
			rootGoMod: "module example.com/errors\n\ngo 1.26\n",
			evalGoMod: "module example.com/errors/eval\n\ngo 1.26\n",
			want: Layout{
				Service:          "errors",
				CandidatePackage: "./cmd/errors",
				SuiteCommand:     "./cmd/errors-evals",
			},
		},
		{
			name:      "missing root module directive",
			rootGoMod: "go 1.26\n",
			evalGoMod: "module example.com/shop/eval\n\ngo 1.26\n",
			wantErr:   "go.mod does not declare a module path",
		},
		{
			name:      "invalid service name in root module",
			rootGoMod: "module example.com/my-shop\n\ngo 1.26\n",
			evalGoMod: "module example.com/my-shop/eval\n\ngo 1.26\n",
			wantErr:   "must end in a service name of lowercase letters and digits",
		},
		{
			name:      "missing eval module directive",
			rootGoMod: "module example.com/shop\n\ngo 1.26\n",
			evalGoMod: "go 1.26\n",
			wantErr:   "eval/go.mod does not declare a module path",
		},
		{
			name:      "mismatched eval module path",
			rootGoMod: "module example.com/shop\n\ngo 1.26\n",
			evalGoMod: "module example.com/other/eval\n\ngo 1.26\n",
			wantErr:   `eval/go.mod declares module "example.com/other/eval", want "example.com/shop/eval"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := StarterLayout([]byte(tc.rootGoMod), []byte(tc.evalGoMod))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// fakeGo stands in for the go command: `go mod tidy` writes the module's
// go.sum, and every other command does nothing.
func fakeGo(dir string, args []string) error {
	if slices.Equal(args, []string{"go", "mod", "tidy"}) {
		return os.WriteFile(filepath.Join(dir, "go.sum"), []byte("sum\n"), 0o644)
	}
	return nil
}

// fakeGenerator stands in for Surla's sandboxed generator.
type fakeGenerator struct {
	// pkgs are the service packages each Generate returns a service.go for.
	pkgs []string
	// extra is added to every Generate result.
	extra map[string]string
	// err, when set, is what Generate returns.
	err error
	// closeErr is what Close returns.
	closeErr error
	// onGenerate, when set, runs on each Generate call with its 1-based
	// number.
	onGenerate func(n int)
	// inputs records each Generate call's files.
	inputs []map[string]string
	// closes counts Close calls.
	closes int
}

// Generate records files and returns the configured result.
func (g *fakeGenerator) Generate(_ context.Context, files map[string]string) (map[string][]byte, error) {
	g.inputs = append(g.inputs, files)
	if g.onGenerate != nil {
		g.onGenerate(len(g.inputs))
	}
	if g.err != nil {
		return nil, g.err
	}
	out := map[string][]byte{}
	for _, pkg := range g.pkgs {
		out["gen/"+pkg+"/service.go"] = []byte("package " + pkg + "\n")
	}
	for p, content := range g.extra {
		out[p] = []byte(content)
	}
	return out, nil
}

// Close counts the call and returns closeErr.
func (g *fakeGenerator) Close() error {
	g.closes++
	return g.closeErr
}

// failRunner fails the test if Init runs any command.
func failRunner(t *testing.T) Runner {
	return func(_ context.Context, _, _ string, args ...string) error {
		t.Fatalf("unexpected command %v", args)
		return nil
	}
}

// assertOnly checks that dir holds exactly the named entries.
func assertOnly(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	assert.ElementsMatch(t, names, got)
}

// between returns the text of s after the first start and before the next
// end, failing the test when either marker is missing.
func between(t *testing.T, s, start, end string) string {
	t.Helper()
	_, after, ok := strings.Cut(s, start)
	require.True(t, ok, "missing %q", start)
	body, _, ok := strings.Cut(after, end)
	require.True(t, ok, "missing %q after %q", end, start)
	return body
}
