package designeval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
)

type (
	// goCommand runs the go command of one toolchain: the toolchain the
	// sandboxed build compiles with, so resolving a design's modules and
	// compiling it agree on the Go version. A design whose go.mod needs a
	// newer Go than that toolchain fails explicitly instead of switching.
	goCommand struct {
		// bin is the toolchain's go binary.
		bin string
	}

	// listedModule is one module of `go list -m -json all`.
	listedModule struct {
		// Path is the module path.
		Path string
		// Version is the selected version; empty for the main module.
		Version string
		// Main marks the main module.
		Main bool
		// Replace is the module's replacement, if any.
		Replace *listedModule
	}
)

const (
	// harnessModulePath is the evaluator program's module path.
	harnessModulePath = "surla.local/designeval"
	// targetPseudoVersion is the version the harness requires the staged
	// target at; a replace directive points it at the staged copy.
	targetPseudoVersion = "v0.0.0-00010101000000-000000000000"
	// harnessCoreModules are the generator modules the harness imports. Their
	// versions come from the target when it requires them; otherwise from the
	// evaluator's own go.mod.
	harnessGoaModule, harnessGoaAIModule, harnessModelModule = "goa.design/goa/v3", "goa.design/goa-ai", "goa.design/model"
	// harnessStoryModule is the portable Story DSL module. The harness imports
	// and requires it only when the target's design packages load it, using
	// the exact version the target selects.
	harnessStoryModule = "github.com/goadesign/surla/story"
)

// prepareHarnessModule makes buildDir a module that builds the evaluator
// program against the target module staged in buildDir/target, using exactly
// the dependency versions the target selects.
//
// Outside any sandbox, and without running target code, it:
//  1. checks the target is a complete module: its design packages load with
//     -mod=readonly, so a go.mod or go.sum that is missing a dependency or a
//     checksum fails here instead of being filled in, and records which
//     modules provide the code the design loads;
//  2. stages the harness program files for that loaded module set and writes
//     the harness go.mod requiring the target (replaced by the staged copy),
//     the generator modules, each at the target's required version or, when
//     the target does not require it, the evaluator's, and the portable Story
//     module at the target's selected version when the design loads it, then
//     resolves it, which may write only the harness's own go.mod and go.sum;
//  3. requires each of those modules, and each generator module (goa,
//     goa-ai, model) the target requires, to keep exactly the target's version
//     in the harness build: an evaluator that would upgrade one, such as an
//     older target goa, is an error naming the module, never a silent
//     evaluation of a different contract.
//
// Modules are downloaded through the operator's Go proxy settings. evalMod is
// the evaluator's own go.mod, which supplies generator versions the target
// does not require. It returns the Goa module the harness build selects, with
// its replacement if any, so a failed build can name it.
func prepareHarnessModule(
	ctx context.Context, gocmd goCommand, buildDir, logDir string,
	target, evalMod *modfile.File, program harnessProgram,
) (listedModule, error) {
	targetDir := filepath.Join(buildDir, "target")
	loaded, err := gocmd.designModules(ctx, targetDir, filepath.Join(logDir, "target-load.log"))
	if err != nil {
		return listedModule{}, fmt.Errorf("target module is incomplete or unresolvable: %w", err)
	}
	for name, src := range program(loaded) {
		src = strings.ReplaceAll(src, "{{DESIGN_IMPORT}}", strconv.Quote(target.Module.Mod.Path+"/design"))
		src = strings.ReplaceAll(src, "{{GEN_IMPORT}}", strconv.Quote(target.Module.Mod.Path+"/gen"))
		if err := os.WriteFile(filepath.Join(buildDir, name), []byte(src), 0o600); err != nil {
			return listedModule{}, fmt.Errorf("write evaluator %s: %w", name, err)
		}
	}
	for _, core := range []string{harnessGoaModule, harnessGoaAIModule, harnessModelModule} {
		if _, ok := requiredVersion(target, core); ok {
			loaded[core] = true
		}
	}
	targetModules, err := gocmd.listModules(ctx, targetDir, filepath.Join(logDir, "target-modules.log"), "-mod=readonly")
	if err != nil {
		return listedModule{}, fmt.Errorf("list target modules: %w", err)
	}

	harness := &modfile.File{}
	targetPath := target.Module.Mod.Path
	for _, step := range []func() error{
		func() error { return harness.AddModuleStmt(harnessModulePath) },
		func() error { return harness.AddGoStmt(evalMod.Go.Version) },
		func() error { return harness.AddRequire(targetPath, targetPseudoVersion) },
		func() error { return harness.AddReplace(targetPath, "", "./target", "") },
	} {
		if err := step(); err != nil {
			return listedModule{}, fmt.Errorf("write harness go.mod: %w", err)
		}
	}
	for _, core := range []string{harnessGoaModule, harnessGoaAIModule, harnessModelModule} {
		// The target's own requirement wins, so evaluating a module never
		// changes the versions it builds with; the evaluator's version only
		// fills a core module the target does not require. sameVersions still
		// rejects a graph in which the evaluator's code raises any of them.
		version, ok := requiredVersion(target, core)
		if !ok {
			version, ok = requiredVersion(evalMod, core)
		}
		if !ok {
			return listedModule{}, fmt.Errorf("evaluator go.mod does not require %s", core)
		}
		if err := harness.AddRequire(core, version); err != nil {
			return listedModule{}, fmt.Errorf("write harness go.mod: %w", err)
		}
	}
	if loaded[harnessStoryModule] {
		storyMod, ok := targetModules[harnessStoryModule]
		if !ok || (!storyMod.Main && storyMod.Version == "") {
			return listedModule{}, fmt.Errorf("target module loads %s without a selected version", harnessStoryModule)
		}
		if !storyMod.Main {
			if err := harness.AddRequire(harnessStoryModule, storyMod.Version); err != nil {
				return listedModule{}, fmt.Errorf("write harness go.mod: %w", err)
			}
		}
	}
	for _, r := range target.Replace {
		// The target's versioned replacements apply to the whole build, as
		// they would in the target's own builds.
		if err := harness.AddReplace(r.Old.Path, r.Old.Version, r.New.Path, r.New.Version); err != nil {
			return listedModule{}, fmt.Errorf("write harness go.mod: %w", err)
		}
	}
	formatted, err := harness.Format()
	if err != nil {
		return listedModule{}, fmt.Errorf("format harness go.mod: %w", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "go.mod"), formatted, 0o600); err != nil {
		return listedModule{}, fmt.Errorf("write harness go.mod: %w", err)
	}
	targetSum, err := os.ReadFile(filepath.Join(targetDir, "go.sum"))
	if err != nil {
		return listedModule{}, fmt.Errorf("read staged go.sum: %w", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "go.sum"), targetSum, 0o600); err != nil {
		return listedModule{}, fmt.Errorf("write harness go.sum: %w", err)
	}
	if err := gocmd.run(ctx, buildDir, filepath.Join(logDir, "harness-load.log"), "list", "-mod=mod", "-deps", "."); err != nil {
		return listedModule{}, fmt.Errorf("resolve evaluator dependencies: %w", err)
	}
	harnessModules, err := gocmd.listModules(ctx, buildDir, filepath.Join(logDir, "harness-modules.log"), "-mod=readonly")
	if err != nil {
		return listedModule{}, fmt.Errorf("list evaluator modules: %w", err)
	}
	if err := sameVersions(loaded, targetModules, harnessModules); err != nil {
		return listedModule{}, err
	}
	return harnessModules[harnessGoaModule], nil
}

// designModules returns the modules that provide a package the target's
// design packages load, directly or transitively. Loading uses -mod=readonly,
// so a go.mod or go.sum missing a dependency or a checksum is an error. It
// parses source and resolves imports but compiles and runs nothing.
func (g goCommand) designModules(ctx context.Context, targetDir, logPath string) (map[string]bool, error) {
	out := logPath + ".txt"
	if err := g.runTo(ctx, targetDir, logPath, out, "list", "-mod=readonly", "-deps",
		"-f", "{{with .Module}}{{.Path}}{{end}}", "./design/..."); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	mods := map[string]bool{}
	for _, line := range strings.Fields(string(raw)) {
		mods[line] = true
	}
	return mods, nil
}

// sameVersions requires every module in loaded, other than the target
// itself, to have the version and replacement the target selects in the
// harness build too. Modules only the evaluator program uses may differ: they
// provide none of the code the design runs.
func sameVersions(loaded map[string]bool, target, harness map[string]listedModule) error {
	var changed []string
	for p := range loaded {
		want, ok := target[p]
		if !ok || want.Main {
			continue
		}
		got, ok := harness[p]
		if !ok {
			changed = append(changed, fmt.Sprintf("%s %s is not in the evaluator build", p, want.Version))
			continue
		}
		if got.Version != want.Version || replacement(got) != replacement(want) {
			changed = append(changed, fmt.Sprintf("%s: target selects %s, evaluator build selects %s",
				p, describe(want), describe(got)))
		}
	}
	if len(changed) > 0 {
		return fmt.Errorf("evaluating this design would change its dependency versions, so the result would not "+
			"describe the target's contract: %s", strings.Join(slices.Sorted(slices.Values(changed)), "; "))
	}
	return nil
}

// listModules returns `go list -m -json all` for the module in dir.
func (g goCommand) listModules(ctx context.Context, dir, logPath, modFlag string) (map[string]listedModule, error) {
	out := logPath + ".json"
	if err := g.runTo(ctx, dir, logPath, out, "list", modFlag, "-m", "-json", "all"); err != nil {
		return nil, err
	}
	f, err := os.Open(out)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	mods := map[string]listedModule{}
	dec := json.NewDecoder(f)
	for {
		var m listedModule
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			return mods, nil
		} else if err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		mods[m.Path] = m
	}
}

// run runs one go command in dir with stdout and stderr in logPath.
func (g goCommand) run(ctx context.Context, dir, logPath string, args ...string) error {
	return g.runTo(ctx, dir, logPath, logPath, args...)
}

// runTo runs one go command in dir with stdout in outPath and stderr in
// logPath. The environment is replaced: cgo is off, workspaces and toolchain
// switching are off, GOFLAGS cannot inject flags such as -toolexec, and only
// the operator's module download settings are passed through.
func (g goCommand) runTo(ctx context.Context, dir, logPath, outPath string, args ...string) error {
	stdout, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer stdout.Close()
	stderr := stdout
	if logPath != outPath {
		if stderr, err = os.Create(logPath); err != nil {
			return err
		}
		defer stderr.Close()
	}
	cmd := exec.CommandContext(ctx, g.bin, args...)
	cmd.Dir = dir
	cmd.Env = goEnv()
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go %s: %w%s", strings.Join(args, " "), err, tail(logPath))
	}
	return nil
}

// goEnv is the environment for the evaluator's go commands outside the
// sandbox.
func goEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"),
		"CGO_ENABLED=0", "GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=",
	}
	for _, name := range []string{"GOMODCACHE", "GOCACHE", "GOPROXY", "GOSUMDB", "GOPRIVATE", "GONOPROXY", "GONOSUMDB"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// tail returns the end of a log file for an error message.
func tail(logPath string) string {
	b, err := os.ReadFile(logPath)
	if err != nil || len(b) == 0 {
		return ""
	}
	const limit = 4 << 10
	if len(b) > limit {
		b = b[len(b)-limit:]
	}
	return "\n" + strings.TrimSpace(string(b))
}

// withStoryRequirementStep returns err, a failure to load the dependencies of
// the target's design, as a *storyRequirementError when the snapshot shows
// why: a design file imports the Story language while the target's go.mod
// does not require its module, so Go cannot resolve that import. The step it
// carries names the Story version the evaluator's own go.mod (evalMod)
// requires, the language this Surla is built and tested with. It decides from
// files and target alone, because the module command's output, which holds
// Go's own hint, is withheld from callers. Any other failure is returned
// unchanged, including one whose target already requires the Story module at
// another version: that requirement resolves, and the evaluation uses it.
func withStoryRequirementStep(err error, files map[string]string, target, evalMod *modfile.File) error {
	if _, ok := requiredVersion(target, harnessStoryModule); ok || !importsStoryLanguage(files) {
		return err
	}
	version, ok := requiredVersion(evalMod, harnessStoryModule)
	if !ok {
		return errors.Join(err, fmt.Errorf("evaluator go.mod does not require %s", harnessStoryModule))
	}
	return &storyRequirementError{err: err, version: version}
}

// importsStoryLanguage reports whether a design file of the snapshot, a .go
// file under design/ (the packages ./design/... names when the evaluator
// loads them), imports a package of the Story module. A file Go cannot parse
// is skipped: it fails to load for its own reason.
func importsStoryLanguage(files map[string]string) bool {
	fset := token.NewFileSet()
	for name, src := range files {
		if !strings.HasPrefix(name, "design/") || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			continue
		}
		for _, imp := range f.Imports {
			if p, err := strconv.Unquote(imp.Path.Value); err == nil &&
				(p == harnessStoryModule || strings.HasPrefix(p, harnessStoryModule+"/")) {
				return true
			}
		}
	}
	return false
}

// requiredVersion returns the version mod requires of path.
func requiredVersion(mod *modfile.File, path string) (string, bool) {
	for _, r := range mod.Require {
		if r.Mod.Path == path {
			return r.Mod.Version, true
		}
	}
	return "", false
}

// replacement describes a module's replacement, or "" when it has none.
func replacement(m listedModule) string {
	if m.Replace == nil {
		return ""
	}
	return m.Replace.Path + "@" + m.Replace.Version
}

// describe names a module's selected version and replacement.
func describe(m listedModule) string {
	if r := replacement(m); r != "" {
		return m.Version + " => " + r
	}
	return m.Version
}
