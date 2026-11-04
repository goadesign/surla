package candidate

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// probeResult mirrors the probe's output line.
type probeResult struct {
	Action  string `json:"action"`
	Allowed bool   `json:"allowed"`
	Detail  string `json:"detail"`
}

// libGoMod is the go.mod of the single-package test modules.
const libGoMod = "module example.com/lib\n\ngo 1.26\n"

// TestExecEnforcesTheSandbox runs a hostile probe binary through Exec and
// checks, from what the operating system actually allowed, that every
// forbidden access fails and every assigned access works.
func TestExecEnforcesTheSandbox(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the sandbox is implemented with sandbox-exec on macOS")
	}
	probe := buildProbe(t)
	state := t.TempDir()
	protected := t.TempDir()
	secret := filepath.Join(protected, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("evaluator secret"), 0o600))
	allowed := listen(t)
	unassigned := listen(t)
	listenPort := freePort(t)
	t.Setenv("EVAL_SECRET", "must not leak")

	p, err := Exec(t.Context(), probe, Spec{
		Args: []string{
			"read:" + secret,
			"write:" + filepath.Join(protected, "out.txt"),
			"write:" + filepath.Join(state, "ok.txt"),
			"env:EVAL_SECRET",
			"env:GIVEN",
			fmt.Sprintf("connect:127.0.0.1:%d", allowed),
			fmt.Sprintf("connect:127.0.0.1:%d", unassigned),
			"connect:1.1.1.1:53",
			fmt.Sprintf("listen:127.0.0.1:%d", listenPort),
			fmt.Sprintf("listen:127.0.0.1:%d", freePort(t)),
			"spawn",
			"read:/etc/hosts",
			"stat:" + secret,
			"stat:" + filepath.Join(state, "ok.txt"),
		},
		Env:          []string{"GIVEN=explicit"},
		StateDir:     state,
		ConnectPorts: []int{allowed},
		ListenPort:   listenPort,
		LogFile:      filepath.Join(t.TempDir(), "probe.log"),
	})
	require.NoError(t, err)
	out, truncated, err := excerptAfterExit(p)
	require.NoError(t, err)
	require.False(t, truncated)

	got := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var r probeResult
		require.NoError(t, json.Unmarshal(sc.Bytes(), &r), sc.Text())
		got[strings.SplitN(r.Action, ":", 2)[0]+"@"+r.Action] = r.Allowed
	}
	cases := []struct {
		name   string
		action string
		allow  bool
	}{
		{"read a protected file", "read:" + secret, false},
		{"write outside its state", "write:" + filepath.Join(protected, "out.txt"), false},
		{"write its own state", "write:" + filepath.Join(state, "ok.txt"), true},
		{"see the evaluator's environment", "env:EVAL_SECRET", false},
		{"see its explicit environment", "env:GIVEN", true},
		{"connect to an assigned port", fmt.Sprintf("connect:127.0.0.1:%d", allowed), true},
		{"connect to an unassigned loopback port", fmt.Sprintf("connect:127.0.0.1:%d", unassigned), false},
		{"connect off the machine", "connect:1.1.1.1:53", false},
		{"listen on its assigned port", fmt.Sprintf("listen:127.0.0.1:%d", listenPort), true},
		{"spawn a child process", "spawn", false},
		{"read other system files", "read:/etc/hosts", false},
		{"learn whether a protected file exists", "stat:" + secret, false},
		{"stat its own state", "stat:" + filepath.Join(state, "ok.txt"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, ok := got[strings.SplitN(c.action, ":", 2)[0]+"@"+c.action]
			require.True(t, ok, "probe did not report %s", c.action)
			assert.Equal(t, c.allow, v)
		})
	}
	listenOthers := 0
	for k, v := range got {
		if strings.HasPrefix(k, "listen@") && !strings.HasSuffix(k, fmt.Sprintf(":%d", listenPort)) {
			listenOthers++
			assert.False(t, v, "listened on an unassigned port: %s", k)
		}
	}
	assert.Equal(t, 1, listenOthers)
}

func TestExecRefusesAModifiedBinary(t *testing.T) {
	if runtime.GOOS != "darwin" {
		_, err := Exec(t.Context(), Artifact{}, Spec{})
		require.ErrorIs(t, err, ErrIsolationUnavailable)
		return
	}
	probe := buildProbe(t)
	require.NoError(t, os.Chmod(filepath.Dir(probe.path), 0o700))
	require.NoError(t, os.Chmod(probe.path, 0o700))
	require.NoError(t, os.WriteFile(probe.path, []byte("tampered"), 0o700))
	_, err := Exec(t.Context(), probe, Spec{StateDir: t.TempDir(), LogFile: filepath.Join(t.TempDir(), "x.log")})
	require.ErrorIs(t, err, ErrDigestMismatch)
}

func TestPinIsolatesTheArtifactFromItsSource(t *testing.T) {
	src := filepath.Join(t.TempDir(), "bin")
	require.NoError(t, os.WriteFile(src, []byte("original"), 0o700))
	store := filepath.Join(t.TempDir(), "artifacts")
	t.Cleanup(func() { require.NoError(t, RemoveStore(store)) })
	a, err := Pin(store, src)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(src, []byte("replaced after pinning"), 0o700))
	got, err := fileDigest(a.path)
	require.NoError(t, err)
	assert.Equal(t, a.Digest(), got, "the pinned copy keeps the original bytes")
	info, err := os.Stat(a.path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o500), info.Mode().Perm())
}

// TestPinKeepsItsBinaryAfterTheCallerChangesDirectories pins through a relative
// symlink and then reads the same copy from another working directory.
func TestPinKeepsItsBinaryAfterTheCallerChangesDirectories(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	require.NoError(t, os.Mkdir("storage", 0o700))
	require.NoError(t, os.Symlink("storage", "alias"))
	require.NoError(t, os.WriteFile("source", []byte("pinned bytes"), 0o700))
	store := filepath.Join(base, "storage", "artifacts")
	t.Cleanup(func() { require.NoError(t, RemoveStore(store)) })

	art, err := Pin(filepath.Join("alias", "artifacts"), "source")
	require.NoError(t, err)
	t.Chdir(t.TempDir())
	got, err := fileDigest(art.path)
	require.NoError(t, err)
	assert.Equal(t, art.Digest(), got)
	assert.True(t, filepath.IsAbs(art.path))
	resolved, err := filepath.EvalSymlinks(art.path)
	require.NoError(t, err)
	assert.Equal(t, resolved, art.path)
}

// TestExecUsesRelativeSymlinkedState preserves the physical state directory
// selected by relative paths and by symlinks followed by parent components.
func TestExecUsesRelativeSymlinkedState(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the sandbox is implemented with sandbox-exec on macOS")
	}
	probe := buildProbe(t)
	base := t.TempDir()
	t.Chdir(base)
	require.NoError(t, os.Mkdir("state", 0o700))
	require.NoError(t, os.Symlink("state", "state-link"))
	require.NoError(t, os.MkdirAll("target/nested", 0o700))
	require.NoError(t, os.Mkdir("target/state", 0o700))
	require.NoError(t, os.Symlink("target/nested", "alias"))
	physicalBase, err := filepath.EvalSymlinks(base)
	require.NoError(t, err)
	cases := []struct {
		name string
		// stateArg keeps the caller's spelling, including symlinks before "..".
		stateArg string
		// state is the physical directory that spelling must select.
		state string
	}{
		{"relative_symlink", "state-link", filepath.Join(physicalBase, "state")},
		{"relative_parent", "alias/../state", filepath.Join(physicalBase, "target", "state")},
		{"absolute_parent", base + "/alias/../state", filepath.Join(physicalBase, "target", "state")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			writeAction := "write:" + tc.name + ".txt"
			proc, err := Exec(ctx, probe, Spec{
				Args:     []string{writeAction, "env:HOME", "env:TMPDIR"},
				StateDir: tc.stateArg,
				LogFile:  tc.name + ".log",
			})
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, proc.Kill()) })
			out, truncated, err := excerptAfterExit(proc)
			require.NoError(t, err)
			require.False(t, truncated)
			results := make(map[string]probeResult)
			scanner := bufio.NewScanner(strings.NewReader(out))
			for scanner.Scan() {
				var result probeResult
				require.NoError(t, json.Unmarshal(scanner.Bytes(), &result))
				results[result.Action] = result
			}
			require.NoError(t, scanner.Err())
			require.Len(t, results, 3)
			for _, action := range []string{writeAction, "env:HOME", "env:TMPDIR"} {
				assert.True(t, results[action].Allowed, action)
			}
			assert.Equal(t, tc.state, results["env:HOME"].Detail)
			assert.Equal(t, tc.state, results["env:TMPDIR"].Detail)
			assert.FileExists(t, filepath.Join(tc.state, tc.name+".txt"))

			inside, err := Exec(ctx, probe, Spec{
				StateDir: tc.stateArg,
				LogFile:  filepath.Join(tc.state, "inside.log"),
			})
			assert.Nil(t, inside)
			assert.ErrorContains(t, err, "outside the candidate state directory")
		})
	}
}

func TestExcerptIsBoundedAndReportsTruncation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the sandbox is implemented with sandbox-exec on macOS")
	}
	probe := buildProbe(t)
	state := t.TempDir()
	p, err := Exec(t.Context(), probe, Spec{
		Args:     []string{"env:A", "env:B", "env:C"},
		StateDir: state,
		LogFile:  filepath.Join(t.TempDir(), "probe.log"),
	})
	require.NoError(t, err)
	require.NoError(t, p.Wait(), "the probe reports failed lookups in its output and exits normally")
	full, truncated, err := p.Excerpt(1 << 20)
	require.NoError(t, err)
	assert.False(t, truncated)
	tail, truncated, err := p.Excerpt(20)
	require.NoError(t, err)
	assert.True(t, truncated)
	assert.Len(t, tail, 20)
	assert.True(t, strings.HasSuffix(full, tail))
}

func TestExecRejectsALogFileInsideTheState(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the sandbox is implemented with sandbox-exec on macOS")
	}
	state := t.TempDir()
	_, err := Exec(t.Context(), buildProbe(t), Spec{StateDir: state, LogFile: filepath.Join(state, "probe.log")})
	require.ErrorContains(t, err, "outside the candidate state directory")
}

// excerptAfterExit waits for the process and returns its whole output.
func excerptAfterExit(p *Process) (string, bool, error) {
	if err := p.Wait(); err != nil {
		return "", false, err
	}
	return p.Excerpt(1 << 20)
}

// buildProbe builds the hostile probe and pins it.
func buildProbe(t *testing.T) Artifact {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "probe")
	build := exec.Command("go", "build", "-o", bin, "./testdata/probe")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))
	store := filepath.Join(t.TempDir(), "artifacts")
	t.Cleanup(func() { require.NoError(t, RemoveStore(store)) })
	a, err := Pin(store, bin)
	require.NoError(t, err)
	return a
}

// listen opens a loopback listener for the test's lifetime and returns its port.
func listen(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// freePort returns a currently unused loopback port.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

func TestBuildDeniesReadsOutsideTheModule(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the sandbox is implemented with sandbox-exec on macOS")
	}
	goroot, modCache := goEnv(t, "GOROOT"), goEnv(t, "GOMODCACHE")
	protected := t.TempDir()
	secret := filepath.Join(protected, "secret.h")
	require.NoError(t, os.WriteFile(secret, []byte("#define SECRET 42\n"), 0o600))

	mod := t.TempDir()
	includePath, err := filepath.Rel(mod, secret)
	require.NoError(t, err)
	cases := []struct {
		name    string
		asm     string
		wantErr bool
	}{
		{"plain module builds", "TEXT ·answer(SB),$0-8\n\tMOVD $42, R0\n\tMOVD R0, ret+0(FP)\n\tRET\n", false},
		{"assembly include of a protected file is denied", "#include \"" + includePath + "\"\nTEXT ·answer(SB),$0-8\n\tMOVD $SECRET, R0\n\tMOVD R0, ret+0(FP)\n\tRET\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module probebuild\n\ngo 1.26\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(mod, "main.go"), []byte("package main\n\nfunc answer() int\n\nfunc main() { println(answer()) }\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(mod, "answer_arm64.s"), []byte("#include \"textflag.h\"\n"+c.asm), 0o600))
			logs := t.TempDir()
			err := Build(t.Context(), BuildSpec{
				SourceDir: mod, ModuleDir: mod, Package: ".", Out: filepath.Join(t.TempDir(), "probe"),
				GOROOT: goroot, ModCache: modCache, WorkDir: t.TempDir(), LogFile: filepath.Join(logs, "build.log"),
			}, (*exec.Cmd).Run)
			log, _ := os.ReadFile(filepath.Join(logs, "build.log"))
			if !c.wantErr {
				require.NoError(t, err, string(log))
				return
			}
			// Control: the same build outside the sandbox reads the protected
			// file, so the sandboxed failure is the sandbox refusing it.
			control := exec.Command("go", "build", "-o", filepath.Join(t.TempDir(), "control"), ".")
			control.Dir = mod
			controlOut, controlErr := control.CombinedOutput()
			require.NoError(t, controlErr, "the unsandboxed control must succeed to prove the include works: %s", controlOut)
			require.Error(t, err, "the sandboxed build must not read the protected file")
			assert.Contains(t, string(log), "#include", "the build failed at the include:\n%s", log)
		})
	}
}

// goEnv returns one go env value.
func goEnv(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("go", "env", name).Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

func TestBuildReadsOnlyItsSourceDir(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the sandbox is implemented with sandbox-exec on macOS")
	}
	goroot, modCache := goEnv(t, "GOROOT"), goEnv(t, "GOMODCACHE")
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "extra.go"), []byte("package main\n\nfunc extra() int { return 1 }\n"), 0o600))

	// newTree writes a checkout holding an app module that replaces a sibling
	// lib module, plus unused links in docs/ that point outside or nowhere.
	newTree := func(t *testing.T) string {
		root := t.TempDir()
		files := map[string]string{
			"lib/go.mod":  "module example.com/lib\n\ngo 1.26\n",
			"lib/lib.go":  "package lib\n\nfunc V() int { return 7 }\n",
			"app/go.mod":  "module example.com/app\n\ngo 1.26\n\nrequire example.com/lib v0.0.0\n\nreplace example.com/lib => ../lib\n",
			"app/main.go": "package main\n\nimport \"example.com/lib\"\n\nfunc main() { println(lib.V()) }\n",
		}
		for name, content := range files {
			path := filepath.Join(root, filepath.FromSlash(name))
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		}
		require.NoError(t, os.MkdirAll(filepath.Join(root, "docs"), 0o700))
		require.NoError(t, os.Symlink(outside, filepath.Join(root, "docs", "outside")))
		require.NoError(t, os.Symlink("missing.md", filepath.Join(root, "docs", "dangling.md")))
		return root
	}
	build := func(t *testing.T, sourceDir, moduleDir string) (string, error) {
		logs := t.TempDir()
		err := Build(t.Context(), BuildSpec{
			SourceDir: sourceDir, ModuleDir: moduleDir, Package: ".", Out: filepath.Join(t.TempDir(), "app"),
			GOROOT: goroot, ModCache: modCache, WorkDir: t.TempDir(), LogFile: filepath.Join(logs, "build.log"),
		}, (*exec.Cmd).Run)
		log, _ := os.ReadFile(filepath.Join(logs, "build.log"))
		return string(log), err
	}
	// unsandboxed builds moduleDir with the host toolchain, proving that a
	// sandboxed failure comes from the sandbox and not from broken source.
	unsandboxed := func(t *testing.T, moduleDir string) {
		cmd := exec.Command("go", "build", "-o", filepath.Join(t.TempDir(), "control"), ".")
		cmd.Dir = moduleDir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "the unsandboxed control must succeed: %s", out)
	}

	t.Run("sibling replace inside the source tree builds despite unused outside links", func(t *testing.T) {
		root := newTree(t)
		log, err := build(t, root, filepath.Join(root, "app"))
		require.NoError(t, err, log)
	})
	t.Run("source file linked outside the source tree is denied", func(t *testing.T) {
		root := newTree(t)
		require.NoError(t, os.Symlink(filepath.Join(outside, "extra.go"), filepath.Join(root, "app", "extra.go")))
		unsandboxed(t, filepath.Join(root, "app"))
		log, err := build(t, root, filepath.Join(root, "app"))
		require.Error(t, err, "the sandboxed build must not read the linked outside file")
		assert.Contains(t, log, "extra.go", "the build failed reading the linked file:\n%s", log)
	})
	t.Run("replace leaving the source tree is denied", func(t *testing.T) {
		root := newTree(t)
		unsandboxed(t, filepath.Join(root, "app"))
		log, err := build(t, filepath.Join(root, "app"), filepath.Join(root, "app"))
		require.Error(t, err, "lib is outside SourceDir, so the sandbox must deny reading it")
		assert.Contains(t, log, "lib", "the build failed reading the replaced module:\n%s", log)
	})
	t.Run("module directory outside the source tree is rejected", func(t *testing.T) {
		root := newTree(t)
		_, err := build(t, filepath.Join(root, "lib"), filepath.Join(root, "app"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is outside source directory")
	})
}

// TestBuildTestReportsWhetherThePackageHasTests compiles one package's tests in
// the build sandbox and checks the three outcomes a caller acts on: a test
// binary, no tests, and a compile error.
func TestBuildTestReportsWhetherThePackageHasTests(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the sandbox is implemented with sandbox-exec on macOS")
	}
	cases := []struct {
		name      string
		pkg       string
		files     map[string]string
		wantTests bool
		wantErr   string
	}{
		{
			name: "package with tests yields a test binary",
			files: map[string]string{
				"go.mod":      libGoMod,
				"lib.go":      "package lib\n\nfunc V() int { return 7 }\n",
				"lib_test.go": "package lib\n\nimport \"testing\"\n\nfunc TestV(t *testing.T) {\n\tif V() != 7 {\n\t\tt.Fatal(V())\n\t}\n}\n",
			},
			wantTests: true,
		},
		{
			name: "package directory ending in .go yields a test binary",
			pkg:  "./dir.go",
			files: map[string]string{
				"go.mod":             libGoMod,
				"dir.go/lib.go":      "package lib\n\nfunc V() int { return 7 }\n",
				"dir.go/lib_test.go": "package lib\n\nimport \"testing\"\n\nfunc TestV(t *testing.T) {\n\tif V() != 7 {\n\t\tt.Fatal(V())\n\t}\n}\n",
			},
			wantTests: true,
		},
		{
			name:  "package without tests is reported, not an error",
			files: map[string]string{"go.mod": libGoMod, "lib.go": "package lib\n\nfunc V() int { return 7 }\n"},
		},
		{
			name: "compile error in a test file fails the build",
			files: map[string]string{
				"go.mod":      libGoMod,
				"lib.go":      "package lib\n\nfunc V() int { return 7 }\n",
				"lib_test.go": "package lib\n\nimport \"testing\"\n\nfunc TestV(t *testing.T) { undefinedInTest() }\n",
			},
			wantErr: "sandboxed test build of .",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mod := writeModule(t, c.files)
			out := filepath.Join(t.TempDir(), "lib.test")
			log := filepath.Join(t.TempDir(), "build.log")
			spec := testBuildSpec(t, mod, mod, out, log)
			if c.pkg != "" {
				spec.Package = c.pkg
			}
			hasTests, err := BuildTest(t.Context(), spec, (*exec.Cmd).Run)
			logText, _ := os.ReadFile(log)
			if c.wantErr != "" {
				require.ErrorContains(t, err, c.wantErr)
				assert.Contains(t, string(logText), "undefinedInTest", "the compiler output is in the log")
				return
			}
			require.NoError(t, err, string(logText))
			assert.Equal(t, c.wantTests, hasTests)
			info, statErr := os.Lstat(out)
			if !c.wantTests {
				assert.ErrorIs(t, statErr, os.ErrNotExist, "no test binary is written")
				return
			}
			require.NoError(t, statErr)
			assert.True(t, info.Mode().IsRegular())
		})
	}

	t.Run("compiler metadata written at the output path is not mistaken for a test binary or overwritten", func(t *testing.T) {
		noTestsMod := writeModule(t, map[string]string{
			"go.mod": libGoMod,
			"lib.go": "package lib\n\nfunc V() int { return 7 }\n",
		})
		withTestsMod := writeModule(t, map[string]string{
			"go.mod":      libGoMod,
			"lib.go":      "package lib\n\nfunc V() int { return 7 }\n",
			"lib_test.go": "package lib\n\nimport \"testing\"\n\nfunc TestV(t *testing.T) {}\n",
		})

		t.Run("GOCACHE README at output path in package without tests", func(t *testing.T) {
			workDir := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(workDir, "gocache"), 0o700))
			out := filepath.Join(workDir, "gocache", "README")
			spec := testBuildSpec(t, noTestsMod, noTestsMod, out, filepath.Join(t.TempDir(), "build.log"))
			spec.WorkDir = workDir
			hasTests, err := BuildTest(t.Context(), spec, (*exec.Cmd).Run)
			require.NoError(t, err)
			assert.False(t, hasTests)
		})

		t.Run("symlinked GOCACHE README at output path in package without tests", func(t *testing.T) {
			workDir := t.TempDir()
			outDir := t.TempDir()
			require.NoError(t, os.Symlink(outDir, filepath.Join(workDir, "gocache")))
			out := filepath.Join(outDir, "README")
			spec := testBuildSpec(t, noTestsMod, noTestsMod, out, filepath.Join(t.TempDir(), "build.log"))
			spec.WorkDir = workDir
			hasTests, err := BuildTest(t.Context(), spec, (*exec.Cmd).Run)
			require.NoError(t, err)
			assert.False(t, hasTests)
		})

		t.Run("relative output path in GOCACHE directory in package without tests", func(t *testing.T) {
			workDir := t.TempDir()
			gocacheDir := filepath.Join(workDir, "gocache")
			require.NoError(t, os.MkdirAll(gocacheDir, 0o700))
			t.Chdir(gocacheDir)
			spec := testBuildSpec(t, noTestsMod, noTestsMod, "README", filepath.Join(t.TempDir(), "build.log"))
			spec.WorkDir = workDir
			hasTests, err := BuildTest(t.Context(), spec, (*exec.Cmd).Run)
			require.NoError(t, err)
			assert.False(t, hasTests)
		})

		t.Run("workspace go.work.sum at output path in package without tests", func(t *testing.T) {
			ws := writeModule(t, map[string]string{
				"go.work":    "go 1.26\n\nuse (\n\t./app\n\t./dep\n)\n",
				"app/go.mod": "module example.com/app\n\ngo 1.26\n",
				"app/app.go": "package app\n\nfunc V() int { return 1 }\n",
				"dep/go.mod": "module example.com/dep\n\ngo 1.26\n",
				"dep/go.sum": "example.com/other v1.0.0 h1:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=\n",
				"dep/dep.go": "package dep\n",
			})
			out := filepath.Join(ws, "go.work.sum")
			spec := testBuildSpec(t, ws, filepath.Join(ws, "app"), out, filepath.Join(t.TempDir(), "build.log"))
			hasTests, err := BuildTest(t.Context(), spec, (*exec.Cmd).Run)
			require.NoError(t, err)
			assert.False(t, hasTests)
		})

		t.Run("compiler metadata created at output path in package with tests fails without overwriting", func(t *testing.T) {
			workDir := t.TempDir()
			outDir := t.TempDir()
			require.NoError(t, os.Symlink(outDir, filepath.Join(workDir, "gocache")))
			out := filepath.Join(outDir, "README")
			spec := testBuildSpec(t, withTestsMod, withTestsMod, out, filepath.Join(t.TempDir(), "build.log"))
			spec.WorkDir = workDir
			hasTests, err := BuildTest(t.Context(), spec, (*exec.Cmd).Run)
			require.ErrorContains(t, err, "publish test binary")
			require.ErrorIs(t, err, os.ErrExist)
			assert.False(t, hasTests)
			readme, readErr := os.ReadFile(out)
			require.NoError(t, readErr)
			assert.Contains(t, string(readme), "cached build artifacts", "GOCACHE README must not be overwritten with the compiled test binary")
		})
	})
}

// TestBuildTestRejectsBeforeRunningTheToolchain checks the preconditions that
// let a missing output mean "no tests": a pattern, a flag or import path in
// place of the package directory, and an existing output path are refused
// before any command runs.
func TestBuildTestRejectsBeforeRunningTheToolchain(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the sandbox is implemented with sandbox-exec on macOS")
	}
	mod := writeModule(t, map[string]string{
		"go.mod":      libGoMod,
		"lib.go":      "package lib\n",
		"lib_test.go": "package lib\n\nimport \"testing\"\n\nfunc TestV(t *testing.T) {}\n",
	})
	outDir := t.TempDir()
	existing := filepath.Join(outDir, "existing.test")
	require.NoError(t, os.WriteFile(existing, []byte("stale"), 0o600))
	link := filepath.Join(outDir, "link.test")
	require.NoError(t, os.Symlink(filepath.Join(outDir, "missing"), link))
	symOutDir := filepath.Join(t.TempDir(), "sym-out")
	require.NoError(t, os.Symlink(outDir, symOutDir))
	aliasLinkTarget := filepath.Join(outDir, "alias-link.test")
	logLinkToOut := filepath.Join(t.TempDir(), "build.log")
	require.NoError(t, os.Symlink(aliasLinkTarget, logLinkToOut))
	type rejectCase struct {
		name    string
		pkg     string
		out     string
		log     string
		wantErr string
	}
	cases := []rejectCase{
		{name: "package pattern", pkg: "./...", out: filepath.Join(outDir, "new.test"), wantErr: "is not one package path"},
		{name: "parent traversal package", pkg: "./../outside", out: filepath.Join(outDir, "new.test"), wantErr: "is not one package path"},
		{name: "output flag as package", pkg: "-o=" + filepath.Join(outDir, "other.test"), out: filepath.Join(outDir, "new.test"), wantErr: "is not one package path"},
		{name: "compile flag as package", pkg: "-c=false", out: filepath.Join(outDir, "new.test"), wantErr: "is not one package path"},
		{name: "import path as package", pkg: "fmt", out: filepath.Join(outDir, "new.test"), wantErr: "is not one package path"},
		{name: "source file as package", pkg: "./lib.go", out: filepath.Join(outDir, "new.test"), wantErr: "is not a directory"},
		{name: "missing package directory", pkg: "./missing", out: filepath.Join(outDir, "new.test"), wantErr: "inspect test package directory"},
		{name: "existing output file", pkg: ".", out: existing, wantErr: "must not exist"},
		{name: "existing output symlink", pkg: ".", out: link, wantErr: "must not exist"},
		{name: "log file equals output path", pkg: ".", out: filepath.Join(outDir, "same.test"), log: filepath.Join(outDir, "same.test"), wantErr: "must not be the output binary"},
		{name: "log file aliases output path through symlinked directory", pkg: ".", out: filepath.Join(outDir, "alias-dir.test"), log: filepath.Join(symOutDir, "alias-dir.test"), wantErr: "must not be the output binary"},
		{name: "log file symlink points to output path", pkg: ".", out: aliasLinkTarget, log: logLinkToOut, wantErr: "must not be the output binary"},
	}
	if _, err := os.Stat(filepath.Join(outDir, "EXISTING.TEST")); err == nil {
		cases = append(cases, rejectCase{
			name:    "log file aliases output path by case on case-insensitive filesystem",
			pkg:     ".",
			out:     filepath.Join(outDir, "case-alias.test"),
			log:     filepath.Join(outDir, "CASE-ALIAS.TEST"),
			wantErr: "must not be the output binary",
		})
	} else {
		t.Run("distinct case paths are allowed on case-sensitive filesystem", func(t *testing.T) {
			spec := testBuildSpec(t, mod, mod, filepath.Join(outDir, "case-distinct.test"), filepath.Join(outDir, "CASE-DISTINCT.TEST"))
			var ran bool
			_, err := BuildTest(t.Context(), spec, func(*exec.Cmd) error {
				ran = true
				return nil
			})
			require.NoError(t, err)
			assert.True(t, ran)
		})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logPath := c.log
			if logPath == "" {
				logPath = filepath.Join(t.TempDir(), "build.log")
			}
			spec := testBuildSpec(t, mod, mod, c.out, logPath)
			spec.Package = c.pkg
			_, err := BuildTest(t.Context(), spec, func(*exec.Cmd) error {
				t.Fatal("the toolchain must not run")
				return nil
			})
			require.ErrorContains(t, err, c.wantErr)
			if c.out != existing && c.out != link {
				_, statErr := os.Lstat(c.out)
				assert.ErrorIs(t, statErr, os.ErrNotExist, "rejected preflight must not create output file")
			}
		})
	}
	t.Run("relative existing output path in working directory is rejected", func(t *testing.T) {
		t.Chdir(outDir)
		spec := testBuildSpec(t, mod, mod, "existing.test", filepath.Join(t.TempDir(), "build.log"))
		_, err := BuildTest(t.Context(), spec, func(*exec.Cmd) error {
			t.Fatal("the toolchain must not run")
			return nil
		})
		require.ErrorContains(t, err, "must not exist")
	})
	t.Run("relative log file aliasing relative output path in working directory is rejected", func(t *testing.T) {
		t.Chdir(outDir)
		spec := testBuildSpec(t, mod, mod, "rel-same.test", "rel-same.test")
		_, err := BuildTest(t.Context(), spec, func(*exec.Cmd) error {
			t.Fatal("the toolchain must not run")
			return nil
		})
		require.ErrorContains(t, err, "must not be the output binary")
		_, statErr := os.Lstat(filepath.Join(outDir, "rel-same.test"))
		assert.ErrorIs(t, statErr, os.ErrNotExist)
	})
	t.Run("relative output path resolves from working directory and publishes staged output", func(t *testing.T) {
		relOutDir := t.TempDir()
		t.Chdir(relOutDir)
		spec := testBuildSpec(t, mod, mod, "relative.test", filepath.Join(t.TempDir(), "build.log"))
		var stageArg string
		hasTests, err := BuildTest(t.Context(), spec, func(cmd *exec.Cmd) error {
			for i, arg := range cmd.Args {
				if arg == "-o" && i+1 < len(cmd.Args) {
					stageArg = cmd.Args[i+1]
				}
			}
			require.NotEmpty(t, stageArg)
			return os.WriteFile(stageArg, []byte("bin"), 0o700)
		})
		require.NoError(t, err)
		assert.True(t, hasTests)
		resolvedRelOutDir, err := filepath.EvalSymlinks(relOutDir)
		require.NoError(t, err)
		assert.NotEqual(t, filepath.Join(resolvedRelOutDir, "relative.test"), stageArg, "toolchain writes to staging path, not directly to Out")
		got, err := os.ReadFile(filepath.Join(resolvedRelOutDir, "relative.test"))
		require.NoError(t, err)
		assert.Equal(t, "bin", string(got))
	})
}

// TestBuildTestReadsOnlyItsSourceDir mirrors TestBuildReadsOnlyItsSourceDir for
// test builds: a sibling replace inside SourceDir builds, while a test file
// linked outside SourceDir, and a replace directive leaving it, are denied by
// the sandbox although the same module builds outside it.
func TestBuildTestReadsOnlyItsSourceDir(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the sandbox is implemented with sandbox-exec on macOS")
	}
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "extra_test.go"),
		[]byte("package lib\n\nimport \"testing\"\n\nfunc TestExtra(t *testing.T) {}\n"), 0o600))
	cases := []struct {
		name    string
		prepare func(t *testing.T, root string) (sourceDir string)
		wantLog string
	}{
		{
			name:    "sibling replace inside the source tree",
			prepare: func(t *testing.T, root string) string { return root },
		},
		{
			name: "test file linked outside the source tree",
			prepare: func(t *testing.T, root string) string {
				require.NoError(t, os.Symlink(filepath.Join(outside, "extra_test.go"), filepath.Join(root, "app", "extra_test.go")))
				return root
			},
			wantLog: "extra_test.go",
		},
		{
			name:    "replace leaving the source tree",
			prepare: func(t *testing.T, root string) string { return filepath.Join(root, "app") },
			wantLog: "example.com/lib",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := writeModule(t, map[string]string{
				"lib/go.mod":      libGoMod,
				"lib/lib.go":      "package lib\n\nfunc V() int { return 7 }\n",
				"app/go.mod":      "module example.com/app\n\ngo 1.26\n\nrequire example.com/lib v0.0.0\n\nreplace example.com/lib => ../lib\n",
				"app/app.go":      "package lib\n\nimport dep \"example.com/lib\"\n\nfunc V() int { return dep.V() }\n",
				"app/app_test.go": "package lib\n\nimport \"testing\"\n\nfunc TestV(t *testing.T) {}\n",
			})
			sourceDir := c.prepare(t, root)
			app := filepath.Join(root, "app")
			control := exec.Command("go", "test", "-c", "-o", filepath.Join(t.TempDir(), "control.test"), ".")
			control.Dir = app
			control.Env = append(os.Environ(), "GOCACHE="+t.TempDir())
			controlOut, controlErr := control.CombinedOutput()
			require.NoError(t, controlErr, "the unsandboxed control must succeed: %s", controlOut)

			log := filepath.Join(t.TempDir(), "build.log")
			hasTests, err := BuildTest(t.Context(), testBuildSpec(t, sourceDir, app, filepath.Join(t.TempDir(), "app.test"), log), (*exec.Cmd).Run)
			logText, _ := os.ReadFile(log)
			if c.wantLog == "" {
				require.NoError(t, err, string(logText))
				assert.True(t, hasTests)
				return
			}
			require.Error(t, err, "the sandboxed test build must not read outside SourceDir")
			assert.Contains(t, string(logText), c.wantLog, "the build failed at the outside read:\n%s", logText)
			assert.Contains(t, string(logText), "operation not permitted", "the sandbox denied the read:\n%s", logText)
		})
	}
}

// TestBuildTestBinaryRunsUnderExec builds a test binary in the build sandbox and
// runs it through Pin and Exec: a test using t.TempDir passes inside its state
// directory, and a test that starts a child process fails because the sandbox
// denies starting it.
func TestBuildTestBinaryRunsUnderExec(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the sandbox is implemented with sandbox-exec on macOS")
	}
	mod := writeModule(t, map[string]string{
		"go.mod": libGoMod,
		"lib.go": "package lib\n",
		"lib_test.go": `package lib

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestTempDir(t *testing.T) {
	if err := os.WriteFile(filepath.Join(t.TempDir(), "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSpawn(t *testing.T) {
	if err := exec.Command("/bin/echo", "spawned").Run(); err != nil {
		t.Fatalf("spawn: %v", err)
	}
}
`,
	})
	out := filepath.Join(t.TempDir(), "lib.test")
	buildLog := filepath.Join(t.TempDir(), "build.log")
	hasTests, err := BuildTest(t.Context(), testBuildSpec(t, mod, mod, out, buildLog), (*exec.Cmd).Run)
	logText, _ := os.ReadFile(buildLog)
	require.NoError(t, err, string(logText))
	require.True(t, hasTests)

	store := filepath.Join(t.TempDir(), "artifacts")
	t.Cleanup(func() { require.NoError(t, RemoveStore(store)) })
	art, err := Pin(store, out)
	require.NoError(t, err)
	p, err := Exec(t.Context(), art, Spec{
		Args:     []string{"-test.v", "-test.timeout=60s"},
		StateDir: t.TempDir(),
		LogFile:  filepath.Join(t.TempDir(), "test.log"),
	})
	require.NoError(t, err)
	require.Error(t, p.Wait(), "TestSpawn must fail, so the test binary exits non-zero")
	got, truncated, err := p.Excerpt(1 << 20)
	require.NoError(t, err)
	require.False(t, truncated)
	assert.Contains(t, got, "--- PASS: TestTempDir")
	assert.Contains(t, got, "--- FAIL: TestSpawn")
	assert.Contains(t, got, "operation not permitted")
}

// writeModule writes files, keyed by slash-separated relative path, under a new
// temporary directory and returns that directory.
func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	return root
}

// testBuildSpec returns a BuildSpec for package "." of moduleDir with the
// host's toolchain and module cache and a fresh work directory.
func testBuildSpec(t *testing.T, sourceDir, moduleDir, out, log string) BuildSpec {
	t.Helper()
	return BuildSpec{
		SourceDir: sourceDir, ModuleDir: moduleDir, Package: ".", Out: out,
		GOROOT: goEnv(t, "GOROOT"), ModCache: goEnv(t, "GOMODCACHE"), WorkDir: t.TempDir(), LogFile: log,
	}
}
