package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunAcceptsOnlyFullCommitIDs(t *testing.T) {
	full := "4d77355e0c1b2a3f4d5e6f708192a3b4c5d6e7f8"
	cases := []struct {
		name      string
		candidate string
		suite     string
	}{
		{"candidate branch name", "main", full},
		{"suite branch name", full, "main"},
		{"symbolic ref", "HEAD", full},
		{"abbreviated ID", "4d77355", full},
		{"revision expression", full + "~1", full},
		{"uppercase ID", "4D77355E0C1B2A3F4D5E6F708192A3B4C5D6E7F8", full},
		{"length of neither object format", full + "0", full},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Run(t.Context(), Plan{CandidateCommit: c.candidate, SuiteCommit: c.suite, WorkDir: t.TempDir()})
			assert.ErrorContains(t, err, "is not a full commit ID")
		})
	}
}

func TestRunRejectsInvalidPlansBeforeWriting(t *testing.T) {
	full := "4d77355e0c1b2a3f4d5e6f708192a3b4c5d6e7f8"
	valid := func() Plan {
		return Plan{
			CandidateCommit: full,
			CandidateModule: "examples/orders",
			Binaries:        map[string]string{"orders": "./cmd/orders"},
			SuiteCommit:     full,
			SuiteModule:     "examples/orders-eval",
			SuiteCommand:    "./cmd/orders_acceptance-evals",
			Tests:           TestsEvaluator,
		}
	}
	cases := []struct {
		name    string
		edit    func(*Plan)
		wantErr string
	}{
		{"parent binary name", func(p *Plan) { p.Binaries = map[string]string{"..": "./cmd/orders"} }, `binary name ".." is not a valid artifact name`},
		{"current binary name", func(p *Plan) { p.Binaries = map[string]string{".": "./cmd/orders"} }, `binary name "." is not a valid artifact name`},
		{"empty binary name", func(p *Plan) { p.Binaries = map[string]string{"": "./cmd/orders"} }, `binary name "" is not a valid artifact name`},
		{"binary name with separator", func(p *Plan) { p.Binaries = map[string]string{"../x": "./cmd/orders"} }, `binary name "../x" must not contain`},
		{"binary name with backslash", func(p *Plan) { p.Binaries = map[string]string{`a\b`: "./cmd/orders"} }, `must not contain`},
		{"binary name with equals", func(p *Plan) { p.Binaries = map[string]string{"a=b": "./cmd/orders"} }, `binary name "a=b" must not contain`},
		{"binary name with NUL", func(p *Plan) { p.Binaries = map[string]string{"a\x00b": "./cmd/orders"} }, `must not contain`},
		{"binary without package", func(p *Plan) { p.Binaries = map[string]string{"orders": ""} }, `binary "orders": package path "" must be "." or start with "./"`},
		{"standard library package", func(p *Plan) { p.Binaries = map[string]string{"orders": "cmd/gofmt"} }, `binary "orders": package path "cmd/gofmt" must be`},
		{"go build flag as package", func(p *Plan) { p.Binaries = map[string]string{"orders": "-toolexec=/bin/sh"} }, `package path "-toolexec=/bin/sh" must be`},
		{"suite command outside module", func(p *Plan) { p.SuiteCommand = "cmd/gofmt" }, `suite command: package path "cmd/gofmt" must be`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := valid()
			p.WorkDir = t.TempDir()
			c.edit(&p)
			_, err := Run(t.Context(), p)
			var stage *StageError
			require.ErrorAs(t, err, &stage)
			assert.Equal(t, StageCheckout, stage.Stage)
			assert.Equal(t, summaryPlan, stage.Summary, "the builder-safe summary never quotes the plan")
			assert.Contains(t, stage.Err.Error(), c.wantErr)
			entries, err := os.ReadDir(p.WorkDir)
			require.NoError(t, err)
			assert.Empty(t, entries, "a rejected plan must not write into WorkDir")
		})
	}

	_, err := Run(t.Context(), valid())
	assert.EqualError(t, errors.Unwrap(err), "work directory is required", "an empty WorkDir must not mean the current directory")

	used := valid()
	used.WorkDir = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(used.WorkDir, "build-home-suite"), 0o700))
	_, err = Run(t.Context(), used)
	assert.EqualError(t, errors.Unwrap(err), "work directory "+used.WorkDir+" must be missing or empty",
		"files left in WorkDir could be read by a build through a committed link")
}

func TestResolveInsideFollowsLinksAndStaysInside(t *testing.T) {
	outside := t.TempDir()
	checkout := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(checkout, "examples", "orders", "cmd"), 0o700))
	require.NoError(t, os.Symlink(outside, filepath.Join(checkout, "examples", "orders", "cmd", "linked-out")))
	require.NoError(t, os.Symlink(outside, filepath.Join(checkout, "linked-out")))
	require.NoError(t, os.Symlink("examples", filepath.Join(checkout, "linked-in")))
	root, err := filepath.EvalSymlinks(checkout)
	require.NoError(t, err)

	cases := []struct {
		rel     string
		want    string
		wantErr string
	}{
		{rel: ".", want: root},
		{rel: "examples/orders", want: filepath.Join(root, "examples", "orders")},
		{rel: "./examples/orders/cmd", want: filepath.Join(root, "examples", "orders", "cmd")},
		{rel: "linked-in/orders", want: filepath.Join(root, "examples", "orders")},
		{rel: "linked-out", wantErr: "outside"},
		{rel: "./examples/orders/cmd/linked-out", wantErr: "outside"},
		{rel: "../" + filepath.Base(outside), wantErr: "outside"},
		{rel: "missing", wantErr: "no such file or directory"},
	}
	for _, c := range cases {
		t.Run(c.rel, func(t *testing.T) {
			got, err := resolveInside(checkout, c.rel)
			if c.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), c.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestRunRefusesACommittedModuleLinkOutsideTheCheckout(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "go.mod"), []byte("module example.com/outside\n"), 0o600))
	repo := t.TempDir()
	git(t, repo, "init", "--quiet")
	require.NoError(t, os.Symlink(outside, filepath.Join(repo, "module")))
	git(t, repo, "add", "module")
	git(t, repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "c")
	head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))

	_, err := Run(t.Context(), Plan{
		CandidateRepo:   repo,
		CandidateCommit: head,
		CandidateModule: "module",
		Binaries:        map[string]string{"orders": "./cmd/orders"},
		SuiteCommit:     head,
		SuiteCommand:    "./cmd/suite",
		WorkDir:         t.TempDir(),
		Tests:           TestsEvaluator,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `candidate module "module": resolves to`)
	assert.Contains(t, err.Error(), "outside")
}

func TestCheckoutRecordsTheExactCommitInEitherObjectFormat(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			repo := t.TempDir()
			git(t, repo, "init", "--quiet", "--object-format="+format)
			require.NoError(t, os.WriteFile(filepath.Join(repo, "f"), []byte("x"), 0o600))
			git(t, repo, "add", "f")
			git(t, repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "c")
			head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
			require.Regexp(t, commitID, head)

			r := &runner{evidenceDir: t.TempDir()}
			require.NoError(t, r.checkout(t.Context(), "candidate", repo, head, filepath.Join(t.TempDir(), "clone")))
			other := strings.Repeat("0", len(head))
			assert.Error(t, r.checkout(t.Context(), "candidate", repo, other, filepath.Join(t.TempDir(), "clone")),
				"a well-formed ID that is not in the repository is refused")
		})
	}
}

// TestRunChecksOutOnlyThePlansRepositoryWhateverGitEnvironmentItInherits runs
// a verification in a process whose environment points git at another
// repository, work tree, index and reference store (pointGitAt). The checkout
// must clone the plan's repository at its commit, with its file in the work
// tree (Run then stops at the plan's absent module, after the checkout), and
// leave every file of the other repository as it was.
func TestRunChecksOutOnlyThePlansRepositoryWhateverGitEnvironmentItInherits(t *testing.T) {
	repo, decoy, workDir := t.TempDir(), t.TempDir(), t.TempDir()
	head := commitOneFile(t, repo)
	commitOneFile(t, decoy)
	before := filesUnder(t, decoy)
	pointGitAt(t, decoy)

	_, err := Run(t.Context(), Plan{
		CandidateRepo:   repo,
		CandidateCommit: head,
		CandidateModule: "absent",
		Binaries:        map[string]string{"orders": "./cmd/orders"},
		SuiteCommit:     head,
		SuiteCommand:    "./cmd/suite",
		WorkDir:         workDir,
		Tests:           TestsEvaluator,
	})
	var stage *StageError
	require.ErrorAs(t, err, &stage)
	assert.Equal(t, "The candidate commit does not contain the configured module.", stage.Summary,
		"the checkout itself succeeded: %v", err)
	checkedOut, err := os.ReadFile(filepath.Join(workDir, "candidate", "f"))
	require.NoError(t, err, "the checkout holds the plan's file")
	assert.Equal(t, repo, string(checkedOut), "the checkout holds the plan repository's content")
	assert.Equal(t, before, filesUnder(t, decoy), "the inherited repository is untouched")
}

// TestPrepareBuildFetchesModulesWhateverGitEnvironmentItInherits downloads, as
// a verification does before its sandboxed build, the modules of a module
// whose dependency is fetched straight from version control (GOPROXY=direct;
// the user's own url.<base>.insteadOf maps it onto a local repository), in a
// process whose git environment points at another repository (pointGitAt).
// Go runs git for that fetch, so the download must succeed and leave every
// file of the other repository as it was.
func TestPrepareBuildFetchesModulesWhateverGitEnvironmentItInherits(t *testing.T) {
	dep, decoy, moduleDir := t.TempDir(), t.TempDir(), t.TempDir()
	git(t, dep, "init", "--quiet")
	require.NoError(t, os.WriteFile(filepath.Join(dep, "go.mod"), []byte("module example.invalid/dep.git\n\ngo 1.21\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dep, "dep.go"), []byte("package dep\n"), 0o600))
	git(t, dep, "add", ".")
	git(t, dep, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "dep")
	git(t, dep, "tag", "v1.0.0")
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "go.mod"),
		[]byte("module example.invalid/main\n\ngo 1.21\n\nrequire example.invalid/dep.git v1.0.0\n"), 0o600))
	commitOneFile(t, decoy)
	before := filesUnder(t, decoy)
	modCache := filepath.Join(t.TempDir(), "modcache")
	t.Setenv("GOPROXY", "direct")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	// -modcacherw lets the test's folder cleanup remove the module cache.
	t.Setenv("GOFLAGS", "-mod=mod -modcacherw")
	t.Setenv("GOMODCACHE", modCache)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url.file://"+dep+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://example.invalid/dep")
	// The fetch must not depend on the developer's own protocol settings: Go
	// checks the module's https scheme against GIT_ALLOW_PROTOCOL, and git
	// the file:// stand-in it rewrites to.
	t.Setenv("GIT_ALLOW_PROTOCOL", "https:file")
	pointGitAt(t, decoy)

	r := &runner{evidenceDir: t.TempDir()}
	toolchain, err := r.prepareBuild(t.Context(), "candidate", moduleDir, t.TempDir())
	require.NoError(t, err, "the dependency is fetched with the user's Git settings")
	assert.Equal(t, modCache, toolchain.modCache)
	assert.Equal(t, before, filesUnder(t, decoy), "the inherited repository is untouched")
}

// TestRunClonesARemoteRepositoryThroughTheUsersGitSettings runs a
// verification whose plan names a remote repository URL that only the user's
// own Git settings in the environment can reach: here they map it onto a
// local repository through file://, which stands in for the remote. The
// checkout must keep those settings, so it clones the plan's commit (Run
// then stops at the plan's absent module, after the checkout).
func TestRunClonesARemoteRepositoryThroughTheUsersGitSettings(t *testing.T) {
	repo := t.TempDir()
	head := commitOneFile(t, repo)
	remote := "https://example.invalid/shop.git"
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url.file://"+repo+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", remote)
	// The file:// stand-in must not depend on the developer's own protocol
	// settings (protocol.file.allow, GIT_PROTOCOL_FROM_USER).
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")

	_, err := Run(t.Context(), Plan{
		CandidateRepo:   remote,
		CandidateCommit: head,
		CandidateModule: "absent",
		Binaries:        map[string]string{"orders": "./cmd/orders"},
		SuiteCommit:     head,
		SuiteCommand:    "./cmd/suite",
		WorkDir:         t.TempDir(),
		Tests:           TestsEvaluator,
	})
	var stage *StageError
	require.ErrorAs(t, err, &stage)
	assert.Equal(t, "The candidate commit does not contain the configured module.", stage.Summary,
		"the remote repository was cloned: %v", err)
}

// TestBuildSummarySurfacesExit71GuidanceWithoutLeakingPrivateDiagnostics
// verifies that buildSummary replaces the generic build failure summary with
// conditional exit-71 recovery guidance only when a non-canceled build command
// exits with status 71, while keeping compiler output and log paths inside
// StageError.Err and out of StageError.Summary.
func TestBuildSummarySurfacesExit71GuidanceWithoutLeakingPrivateDiagnostics(t *testing.T) {
	exit71Err := exec.Command("sh", "-c", "exit 71").Run()
	require.Error(t, exit71Err)
	exit2Err := exec.Command("sh", "-c", "exit 2").Run()
	require.Error(t, exit2Err)

	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()

	cases := []struct {
		name           string
		ctx            context.Context
		err            error
		defaultSummary string
		wantSummary    string
	}{
		{
			name:           "candidate build exit 71",
			ctx:            t.Context(),
			err:            fmt.Errorf("sandboxed build: %w", exit71Err),
			defaultSummary: summaryCandidateBuild,
			wantSummary:    summarySandboxExit71,
		},
		{
			name:           "suite build exit 71",
			ctx:            t.Context(),
			err:            fmt.Errorf("sandboxed build: %w", exit71Err),
			defaultSummary: summarySuiteBuild,
			wantSummary:    summarySandboxExit71,
		},
		{
			name:           "ordinary compile failure keeps candidate summary",
			ctx:            t.Context(),
			err:            fmt.Errorf("sandboxed build: %w", exit2Err),
			defaultSummary: summaryCandidateBuild,
			wantSummary:    summaryCandidateBuild,
		},
		{
			name:           "ordinary compile failure keeps suite summary",
			ctx:            t.Context(),
			err:            fmt.Errorf("sandboxed build: %w", exit2Err),
			defaultSummary: summarySuiteBuild,
			wantSummary:    summarySuiteBuild,
		},
		{
			name:           "canceled context keeps default summary even on exit 71",
			ctx:            canceledCtx,
			err:            fmt.Errorf("sandboxed build: %w", exit71Err),
			defaultSummary: summaryCandidateBuild,
			wantSummary:    summaryCandidateBuild,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildSummary(tc.ctx, tc.err, tc.defaultSummary)
			assert.Equal(t, tc.wantSummary, got)
			stage := stageError(StageBuild, got, fmt.Errorf("build candidate orders: %w\nbuild output, full output in /private/secret/build.log:\nsandbox_apply: Operation not permitted", tc.err))
			var se *StageError
			require.ErrorAs(t, stage, &se)
			assert.Equal(t, tc.wantSummary, se.Summary)
			assert.NotContains(t, se.Summary, "/private/secret/build.log")
			assert.Contains(t, se.Error(), "/private/secret/build.log")
		})
	}
}

// git runs one git command in dir and returns its output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	return string(out)
}

// pointGitAt points the test process's git environment at the repository in
// decoy, as git does for the commands its hooks run and as a user can by hand:
// its repository, work tree, index and reference store.
func pointGitAt(t *testing.T, decoy string) {
	t.Helper()
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(decoy, ".git", "index"))
	t.Setenv("GIT_REFERENCE_BACKEND", "files://"+filepath.Join(decoy, ".git"))
}

// filesUnder returns the content of every file under dir, by path.
func filesUnder(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		files[path] = string(raw)
		return err
	}))
	return files
}

// commitOneFile makes dir a git repository with one commit of one file, whose
// content is dir itself, and returns the commit's ID. It runs git with the
// test process's environment, so callers make their repositories before
// pointing that environment elsewhere.
func commitOneFile(t *testing.T, dir string) string {
	t.Helper()
	git(t, dir, "init", "--quiet")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte(dir), 0o600))
	git(t, dir, "add", "f")
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "c")
	return strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
}
