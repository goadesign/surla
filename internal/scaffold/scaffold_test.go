package scaffold

import (
	"context"
	"errors"
	"go/format"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// published is a Surla version that exists on the module proxy.
const published = "v0.0.0-20261102083950-a4246a33f714"

func TestValidVersionAcceptsOnlyExactVersions(t *testing.T) {
	for v, want := range map[string]bool{
		"v1.2.3":     true,
		"v1.2.3-pre": true,
		published:    true,
		"v0.0.0-20261102083950-a4246a33f714+dirty": false,
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
		_, err := Init(t.Context(), Options{Dir: dir, Module: "example.com/shop", SurlaVersion: published}, failRunner(t))
		require.ErrorContains(t, err, "already exists", dir)
	}
	assertOnly(t, parent, "empty", "full", "link")
	assertOnly(t, empty)
	assertOnly(t, full, "keep.txt")
}

func TestInitLeavesNothingWhenAStepFails(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "shop")
	var ran [][]string
	run := func(_ context.Context, _, logPath string, args ...string) error {
		ran = append(ran, args)
		out := "ok"
		if args[1] == "run" {
			out = strings.Repeat("x", 5000) + "design/design.go:3: boom"
		}
		if err := os.WriteFile(logPath, []byte(out), 0o600); err != nil {
			return err
		}
		if args[1] == "run" {
			return errors.New("exit status 1")
		}
		return nil
	}
	_, err := Init(t.Context(), Options{Dir: dir, Module: "example.com/shop", SurlaVersion: published}, run)
	var step *StepError
	require.ErrorAs(t, err, &step)
	assert.Equal(t, []string{"go", "run", "goa.design/goa/v3/cmd/goa", "gen", "example.com/shop/design"}, step.Args)
	assert.Contains(t, err.Error(), "design/design.go:3: boom")
	assert.True(t, strings.HasPrefix(step.Tail, "...\n"), "only the end of a long log is quoted")
	assert.Len(t, step.Tail, len("...\n")+maxTail)
	full, err := os.ReadFile(step.Log)
	require.NoError(t, err, "the failed step's log is kept")
	assert.Len(t, full, 5000+len("design/design.go:3: boom"))
	assert.Equal(t, [][]string{{"go", "mod", "tidy"}, step.Args}, ran)
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
		return fakeGoaGen(moduleDir, "shop", args)
	}
	_, err := Init(t.Context(), Options{Dir: dir, Module: "example.com/shop", SurlaVersion: published}, run)
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
	_, err := Init(t.Context(), Options{Dir: dir, Module: "example.com/shop", SurlaVersion: published}, run)
	var step *StepError
	require.ErrorAs(t, err, &step)
	assert.Equal(t, []string{"go", "mod", "tidy"}, step.Args)
	assert.Equal(t, ".", step.Module)
	assert.Empty(t, step.Tail, "a log that may still be written is not read")
	assert.Contains(t, err.Error(), "is kept because processes may still use it")
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.True(t, strings.HasPrefix(entries[0].Name(), "shop.surla-init-"), entries[0].Name())
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
	_, err := Init(ctx, Options{Dir: dir, Module: "example.com/shop", SurlaVersion: published}, run)
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
		return fakeGoaGen(dir, "bakery", args)
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	name, err := Init(t.Context(), Options{Dir: dir, Module: "example.com/bakery", SurlaVersion: published}, run)
	require.NoError(t, err)
	assertOnly(t, tmp)
	assert.Equal(t, "bakery", name)
	assert.Equal(t, []string{
		"go mod tidy", "go run goa.design/goa/v3/cmd/goa gen example.com/bakery/design", "go mod tidy", "go vet ./...",
		"go mod tidy", "go get github.com/goadesign/surla@" + published,
		"go run goa.design/goa/v3/cmd/goa gen example.com/bakery/eval/design", "go mod tidy", "go vet ./...",
	}, ran)
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
		return fakeGoaGen(moduleDir, "errors_", args)
	}
	name, err := Init(t.Context(), Options{Dir: dir, Module: "example.com/errors", SurlaVersion: published}, run)
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
				for _, pkg := range tc.pkgs {
					if err := fakeGoaGen(moduleDir, pkg, args); err != nil {
						return err
					}
				}
				return nil
			}
			_, err := Init(t.Context(), Options{Dir: filepath.Join(parent, "shop"), Module: "example.com/shop", SurlaVersion: published}, run)
			require.ErrorContains(t, err, tc.wantErr)
			assertOnly(t, parent)
		})
	}
}

// fakeGoaGen stands in for goa gen: for a gen command it writes the service
// package Goa would generate, named pkg, into dir.
func fakeGoaGen(dir, pkg string, args []string) error {
	if !slices.Contains(args, "gen") {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(dir, "gen", pkg), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "gen", pkg, "service.go"), []byte("package "+pkg+"\n"), 0o644)
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
