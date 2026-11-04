// Package scaffold creates a new Surla starter project for `surla init`: a
// Goa / Goa-AI service whose helper agent prices items through a tool, plus a
// starter evaluator suite that `surla verify` runs against it.
//
// Init builds the whole project in a staging directory next to the target,
// running `go mod tidy` in each module and generating its code with a
// Generator (Surla's sandboxed Goa generator, the one `surla gen` uses), and
// renames the staging directory to the target only when every step
// succeeded. A failed or canceled step removes the staging directory, so the
// target is never left half-written. Each command's output goes to a log file; a failed step's log
// is kept and named in the error.
package scaffold

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"go/format"
	"go/token"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"text/template"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/goadesign/surla"
	"github.com/goadesign/surla/internal/procgroup"
)

type (
	// Options describes the project Init creates.
	Options struct {
		// Dir is the directory to create. It must not exist.
		Dir string
		// Module is the candidate's Go module path, such as
		// example.com/shop. Its last element names the service.
		Module string
		// SurlaVersion is the exact github.com/goadesign/surla version the
		// evaluator module requires, such as
		// v0.0.0-20261102083950-a4246a33f714.
		SurlaVersion string
	}

	// Runner runs one command in dir, writing its combined output to the
	// file at logPath, and returns once every process it started has
	// exited. Init uses RunCommand; tests substitute their own.
	Runner func(ctx context.Context, dir, logPath string, args ...string) error

	// Generator runs Goa's generator for one module snapshot without running
	// the module's code on the host. *designeval.Evaluator implements it.
	// Generate receives the module's go.mod, go.sum and design files keyed by
	// module-relative path and returns the generated files by the same kind
	// of path: files under gen/ and AGENTS_QUICKSTART.md. Its processes work
	// on their own copies of those files, never on Init's staging directory.
	// Init owns the Generator it is given and closes it before returning.
	Generator interface {
		Generate(ctx context.Context, files map[string]string) (map[string][]byte, error)
		Close() error
	}

	// data is what the templates are rendered with.
	data struct {
		// Name is the service name, the last element of Module.
		Name string
		// Pkg is the Go package Goa generated for the service: Name, or
		// Name with "_" appended when Name is reserved in generated Go code
		// (errors becomes errors_). Only templates rendered after generation
		// use it.
		Pkg string
		// Module is the candidate's module path.
		Module string
		// SurlaVersion is the evaluator's required Surla version.
		SurlaVersion string
		// GoVersion is the go directive of Surla's own go.mod.
		GoVersion string
		// GoaVersion, GoaAIVersion and ModelVersion are the versions
		// Surla's own go.mod requires.
		GoaVersion, GoaAIVersion, ModelVersion string
	}

	// moduleStep is one module of the project and the files it is built
	// from.
	moduleStep struct {
		// dir is the module's directory relative to the project root.
		dir string
		// templates is the template directory holding its files.
		templates string
		// path is the module path.
		path string
		// requireSurla is set for the evaluator module, which pins Surla.
		requireSurla bool
	}

	// StepError reports the command that failed while creating a project.
	StepError struct {
		// Args is the command line.
		Args []string
		// Module is the module directory it ran in: "." or "eval".
		Module string
		// Log is the file holding its complete output; Init keeps it.
		Log string
		// Tail is the end of that output.
		Tail string
		// Err is the command's error.
		Err error
	}

	// stuckError reports a command whose processes did not all exit.
	stuckError struct {
		// err is procgroup.Run's stop error.
		err error
	}
)

const (
	// quickstartGuide is the one generated file outside gen/: goa-ai's guide
	// for coding agents, written at the module root.
	quickstartGuide = "AGENTS_QUICKSTART.md"
	// surlaModule is Surla's module path.
	surlaModule = "github.com/goadesign/surla"
	// stopGrace is how long a canceled command's process group gets to exit
	// after SIGTERM.
	stopGrace = 5 * time.Second
	// stopTimeout bounds the wait for a command's process group to empty.
	stopTimeout = 10 * time.Second
	// maxTail bounds how much of a failed command's log a StepError quotes.
	maxTail = 4 << 10
)

var (
	// templatesFS holds the project files. Files end in .tmpl so the Go
	// tooling ignores them here; "_name_" in a path is the service name, and
	// "gitignore.tmpl" becomes ".gitignore".
	//
	//go:embed all:templates
	templatesFS embed.FS

	// lowerAlnum is the form a service name must have: lowercase letters and
	// digits starting with a letter, so Goa generates a Go package of the
	// same name.
	lowerAlnum = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
)

// Init creates the project described by o, running every command with run
// and generating each module's code with gen, which it closes before
// returning. It returns the service name. On error the target directory is
// unchanged.
func Init(ctx context.Context, o Options, run Runner, gen Generator) (name string, err error) {
	genClosed := false
	defer func() {
		if !genClosed {
			err = errors.Join(err, gen.Close())
		}
	}()
	d, err := newData(o)
	if err != nil {
		return "", err
	}
	target, err := filepath.Abs(o.Dir)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("%s already exists; surla init only creates a new directory", target)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("check %s: %w", target, err)
	}
	logs, err := os.MkdirTemp("", "surla-init-logs-*")
	if err != nil {
		return "", fmt.Errorf("create log directory: %w", err)
	}
	staging, err := os.MkdirTemp(filepath.Dir(target), filepath.Base(target)+".surla-init-*")
	if err != nil {
		return "", errors.Join(fmt.Errorf("create staging directory: %w", err), os.RemoveAll(logs))
	}
	if err := build(ctx, staging, logs, d, run, gen); err != nil {
		var stuck *stuckError
		if errors.As(err, &stuck) {
			return "", fmt.Errorf("%w; the staging directory %s is kept because processes may still use it", err, staging)
		}
		return "", errors.Join(err, os.RemoveAll(staging))
	}
	// The generator's processes worked on their own copies of the files, so
	// whatever its Close reports, nothing of it uses the staging directory.
	// It is closed and the context checked before publishing, so a project
	// appears only when generation is fully over and Init was not canceled.
	genClosed = true
	if err := errors.Join(gen.Close(), ctx.Err()); err != nil {
		return "", errors.Join(err, os.RemoveAll(logs), os.RemoveAll(staging))
	}
	// Every step succeeded, so the logs are no longer needed. They are removed
	// before publishing so that the rename is the last thing that can fail.
	if err := os.RemoveAll(logs); err != nil {
		return "", errors.Join(fmt.Errorf("remove command logs: %w", err), os.RemoveAll(staging))
	}
	// MkdirTemp creates the staging directory private (0700); the project
	// gets the usual directory permissions before it is published.
	if err := os.Chmod(staging, 0o755); err != nil {
		return "", errors.Join(fmt.Errorf("set project permissions: %w", err), os.RemoveAll(staging))
	}
	// os.Rename refuses a target that exists when it checks. A directory
	// created between that check and the rename is replaced only if it is
	// empty (rename(2) fails on a non-empty one), so no data is lost.
	if err := os.Rename(staging, target); err != nil {
		return "", errors.Join(fmt.Errorf("move project into %s: %w", target, err), os.RemoveAll(staging))
	}
	return d.Name, nil
}

// RunCommand runs args in dir as its own process group with GOWORK=off, so a
// go.work file around dir cannot change the new modules, and writes the
// combined output to a new file at logPath. When ctx ends the group gets
// SIGTERM, then SIGKILL; a group that does not exit is reported as a stuck
// error so Init keeps the staging directory its processes may still use.
func RunCommand(ctx context.Context, dir, logPath string, args ...string) error {
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create command log: %w", err)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	cmd.Stdout, cmd.Stderr = log, log
	runErr, stopErr := procgroup.Run(cmd, syscall.SIGTERM, stopGrace, stopTimeout)
	if stopErr != nil {
		return errors.Join(&stuckError{err: stopErr}, runErr, log.Close())
	}
	return errors.Join(runErr, log.Close())
}

// ValidVersion reports whether v is an exact module version: a canonical
// semantic version such as v1.2.3, v1.2.3-pre or a pseudo-version, not a
// query such as latest or a branch name, and without build metadata such as
// +dirty.
func ValidVersion(v string) bool {
	return semver.IsValid(v) && semver.Canonical(v) == v
}

// Error describes the failed command and the end of its output.
func (e *StepError) Error() string {
	return fmt.Sprintf("%s (in module %s) failed: %v; full output in %s\n%s", strings.Join(e.Args, " "), e.Module, e.Err, e.Log, e.Tail)
}

// Unwrap returns the command's error.
func (e *StepError) Unwrap() error {
	return e.Err
}

// Error describes the processes left running.
func (e *stuckError) Error() string {
	return "command processes did not exit: " + e.err.Error()
}

// Unwrap returns procgroup.Run's stop error.
func (e *stuckError) Unwrap() error {
	return e.err
}

// newData validates o and returns the template data, with the Go, Goa,
// Goa-AI and Model versions read from Surla's own go.mod.
func newData(o Options) (data, error) {
	if err := module.CheckPath(o.Module); err != nil {
		return data{}, fmt.Errorf("-module %q: %w", o.Module, err)
	}
	name := path.Base(o.Module)
	if !lowerAlnum.MatchString(name) || !token.IsIdentifier(name) || name == "main" {
		return data{}, fmt.Errorf("-module %q must end in a service name of lowercase letters and digits starting with a letter, other than a Go keyword or main, such as example.com/shop", o.Module)
	}
	if !ValidVersion(o.SurlaVersion) {
		return data{}, fmt.Errorf("Surla version %q is not an exact module version such as v0.0.0-20261102083950-a4246a33f714", o.SurlaVersion)
	}
	mod, err := modfile.Parse("go.mod", []byte(surla.GoMod()), nil)
	if err != nil {
		return data{}, fmt.Errorf("parse Surla's go.mod: %w", err)
	}
	d := data{Name: name, Module: o.Module, SurlaVersion: o.SurlaVersion, GoVersion: mod.Go.Version}
	for _, req := range []struct {
		path string
		dst  *string
	}{
		{"goa.design/goa/v3", &d.GoaVersion},
		{"goa.design/goa-ai", &d.GoaAIVersion},
		{"goa.design/model", &d.ModelVersion},
	} {
		for _, r := range mod.Require {
			if r.Mod.Path == req.path {
				*req.dst = r.Mod.Version
			}
		}
		if *req.dst == "" {
			return data{}, fmt.Errorf("Surla's go.mod does not require %s", req.path)
		}
	}
	return d, nil
}

// build creates both modules under root. For each module it writes go.mod and
// design/, resolves the design's dependencies, generates code with gen,
// writes the remaining files, tidies again and vets. The evaluator module
// pins the exact Surla version with go get before generation: the final tidy
// then keeps that version instead of picking another, and the code is
// generated with whatever Goa versions it requires.
func build(ctx context.Context, root, logs string, d data, run Runner, gen Generator) error {
	for _, m := range []moduleStep{
		{dir: ".", templates: "templates/candidate", path: d.Module},
		{dir: "eval", templates: "templates/eval", path: d.Module + "/eval", requireSurla: true},
	} {
		dir := filepath.Join(root, m.dir)
		designFiles, err := render(dir, m.templates, d, isDesignFile)
		if err != nil {
			return err
		}
		steps := [][]string{{"go", "mod", "tidy"}}
		if m.requireSurla {
			// Pinning Surla first may raise Goa or Goa-AI to what this Surla
			// version requires, so generation runs once with the final
			// versions.
			steps = append(steps, []string{"go", "get", surlaModule + "@" + d.SurlaVersion})
		}
		if err := runAll(ctx, dir, m.dir, logs, steps, run); err != nil {
			return err
		}
		if err := generate(ctx, dir, m.dir, designFiles, gen); err != nil {
			return err
		}
		pkg, err := generatedServicePackage(dir)
		if err != nil {
			return err
		}
		d.Pkg = pkg
		if _, err := render(dir, m.templates, d, func(p string) bool { return !isDesignFile(p) }); err != nil {
			return err
		}
		steps = [][]string{{"go", "mod", "tidy"}, {"go", "vet", "./..."}}
		if err := runAll(ctx, dir, m.dir, logs, steps, run); err != nil {
			return err
		}
	}
	return nil
}

// runAll runs each command in dir in order, each logging to its own new file
// in logs, and stops at the first failure.
func runAll(ctx context.Context, dir, moduleDir, logs string, steps [][]string, run Runner) error {
	for _, args := range steps {
		log, err := newLogPath(logs, moduleDir, args)
		if err != nil {
			return err
		}
		if err := run(ctx, dir, log, args...); err != nil {
			step := &StepError{Args: args, Module: moduleDir, Log: log, Err: err}
			var stuck *stuckError
			if errors.As(err, &stuck) {
				// Processes may still be writing the log, so it is not read.
				return step
			}
			tail, tailErr := readTail(log)
			step.Tail = tail
			return errors.Join(step, tailErr)
		}
	}
	return nil
}

// generate runs gen on the module in dir, whose files are exactly
// designFiles (go.mod and design/, as rendered and then resolved) plus the
// go.sum the resolution wrote, and writes the generated files into dir.
// Every generated path is checked before anything is written: it must be
// AGENTS_QUICKSTART.md or a clean relative path under gen/. dir is a new,
// unpublished module, so there is nothing in it to keep or replace.
func generate(ctx context.Context, dir, moduleDir string, designFiles []string, gen Generator) error {
	files := map[string]string{}
	for _, name := range append(slices.Clone(designFiles), "go.sum") {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return fmt.Errorf("generate code for module %s: %w", moduleDir, err)
		}
		files[name] = string(b)
	}
	generated, err := gen.Generate(ctx, files)
	if err != nil {
		return fmt.Errorf("generate code for module %s: %w", moduleDir, err)
	}
	for p := range generated {
		if p != quickstartGuide && (p != path.Clean(p) || !strings.HasPrefix(p, "gen/")) {
			return fmt.Errorf("generate code for module %s: refusing to write %q: generated files must be inside gen/", moduleDir, p)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("generate code for module %s: %w", moduleDir, err)
	}
	for _, p := range slices.Sorted(maps.Keys(generated)) {
		err := root.MkdirAll(path.Dir(p), 0o755)
		if err == nil {
			err = root.WriteFile(p, generated[p], 0o644)
		}
		if err != nil {
			return errors.Join(fmt.Errorf("write %s in module %s: %w", p, moduleDir, err), root.Close())
		}
	}
	return root.Close()
}

// newLogPath returns a new, unused file path in logs for one command, named
// after its module and the command's verb, such as eval-get-123.log.
func newLogPath(logs, moduleDir string, args []string) (string, error) {
	module := moduleDir
	if module == "." {
		module = "root"
	}
	// Every step is "go <verb> ...", so the verb names the step.
	f, err := os.CreateTemp(logs, module+"-"+args[1]+"-*.log")
	if err != nil {
		return "", fmt.Errorf("create command log: %w", err)
	}
	return f.Name(), errors.Join(f.Close(), os.Remove(f.Name()))
}

// generatedServicePackage returns the package Goa generated for the
// project's only service: the one directory under gen/ holding service.go.
// Goa owns that name, so the templates import it instead of predicting it.
func generatedServicePackage(dir string) (string, error) {
	// dir is opened as a file system so characters such as [ in the user's
	// path are not read as pattern syntax; the only wildcard is Goa's.
	matches, err := fs.Glob(os.DirFS(dir), "gen/*/service.go")
	if err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("the generator produced %d service packages under %s, want exactly 1", len(matches), filepath.Join(dir, "gen"))
	}
	return path.Base(path.Dir(matches[0])), nil
}

// isDesignFile reports whether a template path is written before generation:
// go.mod and the design package.
func isDesignFile(p string) bool {
	return p == "go.mod.tmpl" || strings.HasPrefix(p, "design/")
}

// render writes every template under root in templatesFS for which keep
// reports true into dir, with "_name_" in paths replaced by the service name,
// and returns the slash-separated paths it wrote, relative to dir.
func render(dir, root string, d data, keep func(string) bool) ([]string, error) {
	var written []string
	err := fs.WalkDir(templatesFS, root, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, root+"/")
		if !keep(rel) {
			return nil
		}
		raw, err := templatesFS.ReadFile(p)
		if err != nil {
			return err
		}
		t, err := template.New(rel).Delims("[[", "]]").Option("missingkey=error").Parse(string(raw))
		if err != nil {
			return fmt.Errorf("parse template %s: %w", rel, err)
		}
		var b bytes.Buffer
		if err := t.Execute(&b, d); err != nil {
			return fmt.Errorf("render template %s: %w", rel, err)
		}
		out := strings.ReplaceAll(strings.TrimSuffix(rel, ".tmpl"), "_name_", d.Name)
		content := b.Bytes()
		if strings.HasSuffix(out, ".go") {
			// Import order depends on the service name, so the rendered
			// source is formatted rather than the template.
			if content, err = format.Source(content); err != nil {
				return fmt.Errorf("format %s: %w", out, err)
			}
		}
		if out == "gitignore" {
			out = ".gitignore"
		}
		dst := filepath.Join(dir, filepath.FromSlash(out))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		written = append(written, out)
		return os.WriteFile(dst, content, 0o644)
	})
	return written, err
}

// readTail returns at most the last maxTail bytes of the file at path, which
// its command has finished writing.
func readTail(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read command log: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("read command log: %w", err)
	}
	prefix := ""
	if info.Size() > maxTail {
		if _, err := f.Seek(-maxTail, io.SeekEnd); err != nil {
			return "", fmt.Errorf("read command log: %w", err)
		}
		prefix = "...\n"
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("read command log: %w", err)
	}
	return prefix + string(b), nil
}
