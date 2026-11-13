// This file prepares the required tests in a working copy, judges the copy
// against a Story, and records an accepted copy as a commit.
//
// Only files inside the test module may change. A check measures the copy's
// changes from its base, the merge base of the surla/tests branch and the
// project's HEAD (copyBase), so the test writer's own commits on the branch
// count as changes too. Both refs are read through the project's repository,
// though they are not a trust boundary: any program of the same user can
// move a branch, so this is a separation of duties, not isolation. When the
// project's history is rewritten (rebased, squashed or replaced), the merge
// base moves back to an older commit, or disappears, and the check names
// files outside the test module the test writer never touched, or stops with
// ErrNoCommonBase. The user's step is then to delete the project's
// .surla-tests folder and its surla/tests branch and build again, which
// starts a new working copy at the project's commit.
package testwriter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/genpublish"
	"github.com/goadesign/surla/internal/procgroup"
	"github.com/goadesign/surla/internal/shquote"
	"github.com/goadesign/surla/verify/candidate"
	"github.com/goadesign/surla/verify/suite"
)

type (
	// Suite locates the project's evaluator suite: the Go module holding the
	// evaluation design, its generated gen/ folder and the test writer's
	// scenario hooks, and the suite command in it that follows Surla's suite
	// command contract (verify/suite). A surla init starter has Module "eval"
	// and Command "./gen/cmd/<name>-evals", the program the generator writes
	// in every Surla test module (designeval.SuiteCommandPackage).
	Suite struct {
		// Module is the module's slash-separated path relative to the
		// project folder.
		Module string
		// Command is the suite command's package relative to Module: "." or
		// a path starting with "./".
		Command string
	}

	// Checker prepares and checks test writers' working copies. Its methods
	// may be called concurrently, but not for the same working copy.
	Checker struct {
		// evaluator generates the required tests; its owner closes it.
		evaluator *designeval.Evaluator
		// workDir is the owner's private folder, outside every project, that
		// holds each check's temporary folder and the build cache checks
		// share. The owner removes it.
		workDir string
		// build and list run a check's two sandboxed steps: candidate.Build
		// and candidate.Output. Tests wrap them with stand-ins that sleep.
		build func(context.Context, candidate.BuildSpec, func(*exec.Cmd) error) error
		list  func(context.Context, candidate.Artifact, candidate.Spec, int) ([]byte, error)
		// buildLimit and listLimit bound those two steps: buildProgramLimit
		// and listTestsLimit, which tests shorten.
		buildLimit, listLimit time.Duration
	}

	// Verdict is Check's judgment of one working copy against a Story.
	Verdict struct {
		// Findings lists what the test writer must change, in the order the
		// check found them; it is empty exactly when the copy is accepted.
		Findings []Finding
		// copy is the working copy that was judged.
		copy WorkingCopy
		// tree is the git tree that was built and described: the copy's
		// files, as a commit would hold them, with the generated folder
		// written again from the Story. It is set exactly when Findings is
		// empty; Record commits it.
		tree string
	}

	// Finding is one reason a working copy is not accepted, worded for the
	// test writer.
	Finding struct {
		// Kind is what failed.
		Kind FindingKind
		// Message says what failed and how, quoting the tool's output, at
		// most maxFindingBytes long.
		Message string
	}

	// FindingKind names the step of the check that failed.
	FindingKind string

	// AuthorError is a failure of Prepare that the working copy's author
	// caused and can fix: the suite module cannot be opened (for example
	// because a folder on its path is a symbolic link) or read, or its
	// generation failed while working on the module's own files
	// (designeval.FailedOnModule), such as a design that does not compile.
	// Finding says what to change, worded as Check words the same failure.
	// Callers find it with errors.As; any other error from Prepare is a
	// failure the author cannot fix.
	AuthorError struct {
		// Finding is what the author must change, with paths relative to the
		// working copy.
		Finding Finding
		// cause is the underlying failure, kept for errors.Is and traces.
		cause error
	}

	// toolchain is the Go toolchain and module cache a check builds with.
	toolchain struct {
		// goroot is the resolved toolchain root.
		goroot string
		// modCache is the module cache the toolchain downloads into.
		modCache string
	}
)

const (
	// FindingGeneration means the required tests could not be generated from
	// the Story with the copy's suite module, for example because its design
	// does not compile or does not declare exactly one evaluation suite.
	FindingGeneration FindingKind = "generation"
	// FindingDependencies means the suite module's dependencies could not be
	// downloaded from its go.mod and go.sum.
	FindingDependencies FindingKind = "dependencies"
	// FindingBuild means the suite command does not compile, for example
	// because a required hook method is missing. The message quotes the
	// compiler, which names the method.
	FindingBuild FindingKind = "build"
	// FindingListing means the suite command's --describe failed or did not
	// print exactly one suite declaration on standard output.
	FindingListing FindingKind = "listing"
	// FindingMissingScenario means the suite command does not declare a
	// scenario the Story requires. The message names the scenario, its flow
	// and its hook method.
	FindingMissingScenario FindingKind = "missing_scenario"
	// FindingOutsideModule means a file outside the test module differs
	// from the copy's base. The message names the file, the base commit and
	// the step that undoes the change.
	FindingOutsideModule FindingKind = "outside_module"
	// FindingHiddenTag means a declared scenario is tagged holdout. Tests in
	// files the builder can read are not hidden.
	FindingHiddenTag FindingKind = "hidden_tag"

	// buildProgramLimit bounds one sandboxed build of the test program. It
	// compiles the test module and its dependencies with an empty or warm
	// build cache, which takes a few minutes at most for a starter's
	// dependencies; ten leaves room for a slower machine while still
	// leaving the whole check room to finish.
	buildProgramLimit = 10 * time.Minute
	// listTestsLimit bounds one run of the test program's --describe, which
	// declares the scenarios without running any: it should take well under
	// a second, so sixty seconds is reached only by a program that waits on
	// something while starting.
	listTestsLimit = 60 * time.Second
	// maxOutsideFindings bounds how many changed files outside the test
	// module a verdict names one by one; one more finding counts the rest.
	maxOutsideFindings = 20
	// holdoutTag is the scenario tag that marks a hidden test.
	holdoutTag = "holdout"

	// maxFindingBytes bounds one finding's message.
	maxFindingBytes = 8 << 10
	// maxDeclarationBytes bounds what --describe may print.
	maxDeclarationBytes = 1 << 20
	// buildStopGrace bounds how long a canceled build gets to exit, and how
	// long its process group may take to empty.
	buildStopGrace = 10 * time.Second
)

// ErrNoCommonBase reports a check that cannot measure the copy's changes: the
// tests branch and the project's commit share no history, for example because
// the project's history was rewritten from its first commit. The user's step
// is to delete the .surla-tests folder and the surla/tests branch, then build
// again.
var ErrNoCommonBase = errors.New("the tests branch and the project share no history")

// ErrGeneration and ErrWriteGenerated mark the two steps of Prepare and Check
// that write the required tests, for a failure the copy's author cannot fix.
// Callers tell them apart with errors.Is to say what failed.
var (
	// ErrGeneration marks a failure of the generator itself: while it
	// prepared or removed its own files, started its sandbox or read its
	// output, or because ctx ended or the platform has no sandbox. It wraps
	// the evaluator's error, from which designeval.GenerationDiagnostic
	// derives the caller-safe text.
	ErrGeneration = errors.New("generate the required tests")
	// ErrWriteGenerated marks a failure to replace the test module's
	// generated folder after generation succeeded, for example because the
	// disk is full or the module cannot be written. Its text names files
	// only inside the module, apart from absolute paths that
	// WithoutAbsolutePaths removes.
	ErrWriteGenerated = errors.New("write the generated folder")
)

// absolutePath matches an absolute file path where a word starts: a slash at
// the start of a line or after a space, a quote, a parenthesis, a bracket, an
// equals sign or a comma, followed by a name character, up to the next
// space, quote, parenthesis, bracket, comma or colon (so a compiler's
// file.go:12:3 keeps its line and column). A URL's slashes follow a colon or
// another slash, an import path starts with a name, and a relative path with
// a name or a dot, so none of them matches.
var absolutePath = regexp.MustCompile(`(?m)(^|[\s"'(\[=,])/[A-Za-z0-9_.~+@-][^\s"'()\[\],:]*`)

// ErrLinkedFolder refuses a suite module reached through a symbolic link.
// Prepare and Check read and write the module only through real folders from
// the working copy's root, so a link the copy's author places on the way
// cannot lead them outside the copy, for example into the project.
var ErrLinkedFolder = errors.New("the suite module must be reached through real folders, not a symbolic link")

// NewChecker returns a Checker that generates with evaluator and works in
// workDir, a private folder its owner keeps outside every project and
// removes after the Checker's last call. Nothing is created until a call
// needs it.
func NewChecker(evaluator *designeval.Evaluator, workDir string) *Checker {
	return &Checker{
		evaluator:  evaluator,
		workDir:    workDir,
		build:      candidate.Build,
		list:       candidate.Output,
		buildLimit: buildProgramLimit,
		listLimit:  listTestsLimit,
	}
}

// Record commits an accepted verdict's tree, exactly the files Check built
// and described, on the working copy's branch with message and the user's own
// git identity, never signed and with hooks disabled, and returns the commit. The message is
// the commit message; it carries no attribution. When the branch already
// holds that tree, as when the copy did not change since the last record, it
// returns the branch's commit and makes none. Edits made in the copy after
// the check are not recorded; they show as changes in the copy. A verdict
// with findings or not made by Check, or an empty message, is refused.
func Record(ctx context.Context, v Verdict, message string) (string, error) {
	if !v.Accepted() {
		return "", errors.New("record: the verdict has findings; only an accepted working copy is recorded")
	}
	if v.tree == "" {
		return "", errors.New("record: the verdict did not come from Check")
	}
	if strings.TrimSpace(message) == "" {
		return "", errors.New("record: the commit message is empty")
	}
	return commitTree(ctx, v.copy, v.tree, message)
}

// WithoutAbsolutePaths replaces every absolute file path in text with
// <absolute path>, leaving URLs, import paths and relative paths such as
// ./gen/cmd/shop-evals as they are. Every finding and every reason a check
// gives passes through it, so no path of Surla's folders or of the user's
// machine reaches the test writer, the builder or the page.
func WithoutAbsolutePaths(text string) string {
	return absolutePath.ReplaceAllString(text, "${1}<absolute path>")
}

// Prepare writes the required tests for stories into the working copy: it
// runs the design evaluator's story generator on the copy's suite module and
// replaces the module's gen/ folder (and its AGENTS_QUICKSTART.md) with the
// result. It returns the required scenarios, one per finished flow, named by
// the generator. The project folder is not touched: the module is opened one
// real folder at a time from the project folder through the copy, and a
// symbolic link on the way is refused, naming it, before anything is read or
// written.
//
// A failure the copy's author can fix is an *AuthorError carrying the finding
// Check would give, worded relative to the copy; a symbolic link on the way
// also wraps ErrLinkedFolder. Any other error is a failure the author cannot
// fix. Two are marked so a caller can say what failed: ErrGeneration, when
// the generator failed (the context ended, the platform has no sandbox, which
// also wraps candidate.ErrIsolationUnavailable, or it failed while preparing
// its own files), and ErrWriteGenerated, when the generated folder could not
// be written. A staging folder that an interrupted publication left in the
// module is removed and never a failure (publish).
func (c *Checker) Prepare(
	ctx context.Context, wc WorkingCopy, s Suite, stories []designeval.StorySummary,
) ([]designeval.StoryScenario, error) {
	if err := errors.Join(wc.opened(), s.validate()); err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(wc.dir, wc.project)
	if err != nil {
		return nil, err
	}
	return c.generate(ctx, filepath.Dir(wc.dir), filepath.Base(wc.dir), path.Join(filepath.ToSlash(rel), s.Module), s, stories)
}

// Check judges the working copy against stories without trusting its author.
// It snapshots the copy's files as a commit would hold them into a private
// folder, compares them with the copy's base (copyBase), writes the suite
// module's generated folder there again from the stories (so the author
// cannot change what is required), downloads the module's dependencies,
// compiles the generated suite command in the verify/candidate build sandbox,
// lists the scenarios it declares by running its --describe in the candidate
// sandbox, reading standard output alone, and compares them with the required
// ones by the generator's own names.
//
// The verdict is accepted, or carries findings for the first step the
// author's files failed: each file changed outside the test module, the
// generation (including a module that does not declare exactly one
// evaluation suite), dependencies, the build (quoting the compiler, which
// names a missing hook method), the listing, or each declared scenario
// tagged holdout and each required scenario not declared. A build that does
// not finish within buildProgramLimit, or a listing within listTestsLimit,
// is a finding too. No finding holds an absolute path
// (WithoutAbsolutePaths). The working copy itself is not written. Errors are
// failures that say nothing about the author's work: the platform has no
// sandbox (candidate.ErrIsolationUnavailable), ctx ended, the copy and the
// project share no history (ErrNoCommonBase), or git, the file system or the
// build's process cleanup failed. ctx bounds the whole check.
//
// Only time bounds the bytes the author's code writes to disk: what the
// sandboxed build writes to its log and to the build cache in the Checker's
// folder within its limit, and what --describe writes to its standard error
// and its state folder within its own. Its standard output is kept in
// memory, at most 1 MiB. The check's own folder, holding that log, standard
// error and state, is removed when Check returns; the build cache stays until
// the owner removes the Checker's folder.
func (c *Checker) Check(
	ctx context.Context, wc WorkingCopy, s Suite, stories []designeval.StorySummary,
) (_ Verdict, err error) {
	if err := errors.Join(wc.opened(), s.validate()); err != nil {
		return Verdict{}, err
	}
	if err := os.MkdirAll(c.workDir, 0o700); err != nil {
		return Verdict{}, fmt.Errorf("create the check folder: %w", err)
	}
	tmp, err := os.MkdirTemp(c.workDir, "check-*")
	if err != nil {
		return Verdict{}, fmt.Errorf("create the check folder: %w", err)
	}
	keep := false
	defer func() {
		if keep {
			err = fmt.Errorf("%w; %s is kept because build processes may still use it", err, tmp)
			return
		}
		err = errors.Join(err, candidate.RemoveStore(filepath.Join(tmp, "store")), os.RemoveAll(tmp))
	}()
	index, src := filepath.Join(tmp, "index"), filepath.Join(tmp, "src")
	if err := stageCopy(ctx, wc, index, src); err != nil {
		return Verdict{}, err
	}
	rel, err := filepath.Rel(wc.dir, wc.project)
	if err != nil {
		return Verdict{}, err
	}
	modulePath := path.Join(filepath.ToSlash(rel), s.Module)
	moduleDir := filepath.Join(src, filepath.FromSlash(modulePath))
	base, err := copyBase(ctx, wc)
	if err != nil {
		return Verdict{}, err
	}
	changed, err := changedSince(ctx, wc, index, base)
	if err != nil {
		return Verdict{}, err
	}
	if findings := outsideModule(changed, modulePath, base); len(findings) > 0 {
		return Verdict{Findings: findings, copy: wc}, nil
	}
	required, err := c.generate(ctx, src, ".", modulePath, s, stories)
	if author := (*AuthorError)(nil); errors.As(err, &author) {
		return Verdict{Findings: []Finding{author.Finding}, copy: wc}, nil
	}
	if err != nil {
		return Verdict{}, err
	}
	tree, err := writeTree(ctx, wc, index, src)
	if err != nil {
		return Verdict{}, err
	}
	declared, findings, err := c.listScenarios(ctx, tmp, src, moduleDir, s.Command, &keep)
	if err != nil {
		return Verdict{}, err
	}
	if len(findings) > 0 {
		return Verdict{Findings: findings, copy: wc}, nil
	}
	ids := make([]string, 0, len(declared))
	for _, sc := range declared {
		ids = append(ids, sc.ID)
		if slices.Contains(sc.Tags, holdoutTag) {
			findings = append(findings, Finding{Kind: FindingHiddenTag, Message: fmt.Sprintf(
				"Scenario %q is tagged %s. Tests in files the builder can read are not hidden: "+
					"remove the tag from the test module's design, then report again.", sc.ID, holdoutTag)})
		}
	}
	for _, r := range required {
		if !slices.Contains(ids, r.Scenario) {
			findings = append(findings, Finding{Kind: FindingMissingScenario, Message: fmt.Sprintf(
				"The suite command does not declare scenario %q, which the Story requires for flow %q of story %q: "+
					"its --describe must list every scenario of the generated suite, whose hook %s runs it.",
				r.Scenario, r.Key, r.Story, r.Hook)})
		}
	}
	if len(findings) > 0 {
		return Verdict{Findings: findings, copy: wc}, nil
	}
	return Verdict{copy: wc, tree: tree}, nil
}

// Error returns the finding's message.
func (e *AuthorError) Error() string {
	return e.Finding.Message
}

// Unwrap returns the underlying failure.
func (e *AuthorError) Unwrap() error {
	return e.cause
}

// Accepted reports whether the working copy passed the check.
func (v Verdict) Accepted() bool {
	return len(v.Findings) == 0
}

// publish replaces the gen/ folder of the module open at root with files. An
// error means gen/ was not replaced and wraps ErrWriteGenerated.
//
// It first removes the staging folders that publications cut short by a
// server's death left in the module (genpublish.RemoveLeftoverStages). Surla
// writes the generated folder of a working copy and of its snapshots one
// publication at a time, in the project's working-copy turn, so none of its
// own publications is using one. A surla gen the author runs in the copy at
// that moment loses its staging folder and fails; the copy's generated folder
// is Surla's to write, and this publication writes it. An entry with a
// staging folder's name that is not one Surla made, such as a link or a file,
// is the author's: it is left as it is, like any other file the author keeps
// in the module.
//
// Nothing else fails the publication. A staging folder that could not be
// removed and the warnings of genpublish.Publish (the agent guide, the change
// listing, this publication's own staging folder) all describe work beside a
// gen/ that already holds the generated files, so they are recorded as events
// on ctx's span and the caller goes on.
func publish(ctx context.Context, root *os.Root, files map[string][]byte) error {
	span := trace.SpanFromContext(ctx)
	if err := genpublish.RemoveLeftoverStages(root); err != nil {
		span.AddEvent("staging folders of earlier publications were left in the test module",
			trace.WithAttributes(attribute.String("surla.error", err.Error())))
	}
	res, err := genpublish.Publish(ctx, root, files)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrWriteGenerated, err)
	}
	if len(res.Warnings) > 0 {
		span.AddEvent("the generated folder was written with warnings",
			trace.WithAttributes(attribute.StringSlice("surla.warnings", res.Warnings)))
	}
	return nil
}

// runBuild runs a check's build in its own process group, the build's owning
// boundary: whatever ends it, the compiler, assembler and linker it started
// are killed and waited for. A group that does not empty is reported with the
// build's own error and marked by stuck.
func runBuild(stuck *bool) func(*exec.Cmd) error {
	return func(cmd *exec.Cmd) error {
		runErr, stopErr := procgroup.Run(cmd, syscall.SIGKILL, buildStopGrace, buildStopGrace)
		if stopErr != nil {
			*stuck = true
			return errors.Join(fmt.Errorf("build processes did not exit: %w", stopErr), runErr)
		}
		return runErr
	}
}

// goEnv is the environment of the go commands a check runs outside the
// sandbox: the user's module download settings, with cgo, workspaces,
// toolchain switching and injected go flags turned off.
func goEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"),
		"CGO_ENABLED=0", "GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=",
	}
	for _, name := range []string{"GOMODCACHE", "GOPROXY", "GOSUMDB", "GOPRIVATE", "GONOPROXY", "GONOSUMDB"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// forAuthor words message for the test writer: the check's private snapshot
// folder src is removed from the paths it quotes, so they read relative to
// the working copy, and it is cut to at most maxFindingBytes on a character
// boundary, saying how much was left out.
func forAuthor(message, src string) string {
	s := WithoutAbsolutePaths(strings.TrimSpace(strings.ReplaceAll(message, src+string(filepath.Separator), "")))
	if len(s) <= maxFindingBytes {
		return s
	}
	const more = "\n… (%d more bytes)"
	k := maxFindingBytes - len(more) - 8
	for k > 0 && !utf8.RuneStart(s[k]) {
		k--
	}
	return s[:k] + fmt.Sprintf(more, len(s)-k)
}

// logFinding returns one finding of kind whose message is head followed by
// the end of the log file at path: as much as fits in maxFindingBytes after
// head, read with candidate.ExcerptFile so a log of any size is never loaded
// whole, starting on a character boundary and saying when earlier output was
// left out. src is removed from quoted paths, as forAuthor does.
func logFinding(kind FindingKind, head, path, src string) ([]Finding, error) {
	const leftOut = "… (earlier output left out)\n"
	out, cut, err := candidate.ExcerptFile(path, int64(maxFindingBytes-len(head)-len(leftOut)))
	if err != nil {
		return nil, err
	}
	if cut {
		for len(out) > 0 && !utf8.RuneStart(out[0]) {
			out = out[1:]
		}
		out = leftOut + out
	}
	return []Finding{{Kind: kind, Message: forAuthor(head+out, src)}}, nil
}

// withinLimit runs step under ctx with its own deadline limit from now and
// reports whether the limit was reached: the deadline had passed when step
// returned, whatever step returned, while ctx itself had not ended. A step
// that returns exactly at the deadline has reached it, so a test can place a
// stand-in just below, at and just above the limit and get the same answer
// each time. The caller's own deadline ending is never the limit.
func withinLimit(ctx context.Context, limit time.Duration, step func(context.Context) error) (bool, error) {
	deadline := time.Now().Add(limit)
	stepCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	err := step(stepCtx)
	return ctx.Err() == nil && !time.Now().Before(deadline), err
}

// limitText words a limit for a finding: two or more whole minutes as
// minutes, other whole seconds as seconds, anything else as Go prints a
// duration. For example, 10 minutes, 60 seconds, 500ms.
func limitText(d time.Duration) string {
	switch {
	case d >= 2*time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%d minutes", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%d seconds", d/time.Second)
	default:
		return d.String()
	}
}

// outsideModule returns one finding per changed path that lies outside the
// test module at modulePath, naming at most maxOutsideFindings and counting
// the rest in one more finding. The module's own folder counts as inside: a
// link put in its place is refused when the module is opened, with its own
// finding. Each finding names base, the commit the copy's changes are measured
// from, and the step that undoes the change whether or not the author
// committed it in the copy: the git command that restores a file base has,
// which names the file from the copy's root and literally, so it works from
// any folder of the copy and for any file name, or deleting a file base does
// not have. Restoring from the copy's own HEAD would bring a committed change
// back, so the finding never leaves the commit unnamed.
func outsideModule(changed []changedPath, modulePath, base string) []Finding {
	var outside []changedPath
	for _, c := range changed {
		if c.path != modulePath && !strings.HasPrefix(c.path, modulePath+"/") {
			outside = append(outside, c)
		}
	}
	var findings []Finding
	for i, c := range outside {
		if i == maxOutsideFindings {
			findings = append(findings, Finding{Kind: FindingOutsideModule, Message: fmt.Sprintf(
				"%d more files outside the test module %s differ from the working copy's base, commit %s.",
				len(outside)-i, modulePath, base)})
			break
		}
		undo := fmt.Sprintf("restore it to commit %s, the base your working copy is measured from, by running "+
			"git restore --source=%s --staged --worktree -- %s in your working copy, then report again.",
			base, base, shquote.Quote(":(top,literal)"+c.path))
		if c.added {
			undo = fmt.Sprintf("commit %s, the base your working copy is measured from, does not have this file: "+
				"delete it, then report again.", base)
		}
		findings = append(findings, Finding{Kind: FindingOutsideModule, Message: WithoutAbsolutePaths(fmt.Sprintf(
			"%s is changed outside the test module %s. Only files inside the test module may change: %s "+
				"If you never changed it, the project's history was rewritten since your working copy was made: "+
				"ask the user to delete the project's .surla-tests folder and its surla/tests branch, then build again.",
			c.path, modulePath, undo))})
	}
	return findings
}

// generatedSuites counts the evaluation suites whose packages a generation
// wrote: the files gen/evals/<suite>/suite.go.
func generatedSuites(files map[string][]byte) int {
	n := 0
	for p := range files {
		if path.Base(p) == "suite.go" && path.Dir(path.Dir(p)) == "gen/evals" {
			n++
		}
	}
	return n
}

// isFailure reports whether err from generating, building or running the
// author's code says nothing about the author's work: ctx ended or the
// platform has no sandbox.
func isFailure(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, candidate.ErrIsolationUnavailable)
}

// decodeDeclaration decodes exactly one suite.Declaration from out: unknown
// fields and anything after it are errors.
func decodeDeclaration(out []byte) (suite.Declaration, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	var d suite.Declaration
	if err := dec.Decode(&d); err != nil {
		return suite.Declaration{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return suite.Declaration{}, errors.New("unexpected data after the declaration")
	}
	return d, nil
}

// resolveToolchain resolves the local Go toolchain's root and module cache,
// from dir, outside any module, so the author's go.mod cannot select another
// toolchain.
func resolveToolchain(ctx context.Context, dir string) (toolchain, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "GOROOT", "GOMODCACHE")
	cmd.Dir, cmd.Env = dir, goEnv()
	out, err := cmd.Output()
	if err != nil {
		return toolchain{}, fmt.Errorf("resolve go env GOROOT GOMODCACHE: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 || lines[0] == "" || lines[1] == "" {
		return toolchain{}, fmt.Errorf("unexpected go env output %q", out)
	}
	goroot, err := abspath.Eval(lines[0])
	if err != nil {
		return toolchain{}, fmt.Errorf("resolve the Go toolchain: %w", err)
	}
	return toolchain{goroot: goroot, modCache: lines[1]}, nil
}

// openModule opens the folder at the slash path rel below the copy's root,
// which is the slash path copyDir below the real folder top, one folder at a
// time, each through the folder above it. Each step is checked with Lstat and
// then opened, and the opened folder must be the one checked, so a symbolic
// link anywhere on the way, even one swapped in meanwhile, is refused with
// ErrLinkedFolder naming it, and nothing outside top is opened. A message
// names a folder on rel relative to the copy's root, which is how the copy's
// author sees it, and a folder on copyDir, the copy's own folder, relative to
// top. The caller closes the result.
func openModule(top, copyDir, rel string) (_ *os.Root, err error) {
	cur, err := os.OpenRoot(top)
	if err != nil {
		return nil, err
	}
	for _, part := range []string{copyDir, rel} {
		walked := ""
		for _, name := range strings.Split(path.Clean(part), "/") {
			if name == "." {
				continue
			}
			walked = path.Join(walked, name)
			next, err := openFolder(cur, name, walked)
			if err := errors.Join(err, cur.Close()); err != nil {
				if next != nil {
					err = errors.Join(err, next.Close())
				}
				return nil, err
			}
			cur = next
		}
	}
	return cur, nil
}

// openFolder opens the folder name inside dir, which walked names for
// messages, refusing a symbolic link or anything but a folder.
func openFolder(dir *os.Root, name, walked string) (*os.Root, error) {
	info, err := dir.Lstat(name)
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s is a symbolic link", ErrLinkedFolder, walked)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a folder", walked)
	}
	next, err := dir.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	held, err := next.Stat(".")
	if err != nil || !os.SameFile(info, held) {
		return nil, errors.Join(fmt.Errorf("%w: %s changed while it was opened", ErrLinkedFolder, walked), err, next.Close())
	}
	return next, nil
}

// generate writes the generated folder of the suite module again from
// stories and returns the required scenarios. The module is at the slash path
// modulePath below the copy's root, and the copy's root is the slash path
// copyDir below the real folder top. Prepare runs it on the working copy (top
// is the project folder and copyDir the copy's folder in it) and Check on its
// snapshot (top is the snapshot and copyDir is "."). A cause the module's
// author can fix is an *AuthorError whose finding gives paths relative to the
// copy's root, in Prepare as in Check: the module cannot be opened, including
// through a symbolic link, or read; its generation failed while working on
// the module's own files; its design does not declare exactly one evaluation
// suite; or no program was generated at s.Command. A failure of the generator
// that is not the author's wraps ErrGeneration, and a failure to write the
// generated folder ErrWriteGenerated.
func (c *Checker) generate(
	ctx context.Context, top, copyDir, modulePath string, s Suite, stories []designeval.StorySummary,
) (_ []designeval.StoryScenario, err error) {
	copyRoot := filepath.Join(top, filepath.FromSlash(copyDir))
	author := func(cause error, message string) error {
		return &AuthorError{Finding: Finding{Kind: FindingGeneration, Message: forAuthor(message, copyRoot)}, cause: cause}
	}
	root, err := openModule(top, copyDir, modulePath)
	if err != nil {
		return nil, author(err, fmt.Sprintf("The suite module %s could not be opened: %v", modulePath, err))
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	files, err := designeval.ReadModuleSnapshotFromRoot(filepath.Join(copyRoot, filepath.FromSlash(modulePath)), root)
	if _, ok := files["go.mod"]; err == nil && !ok {
		err = errors.New("it has no go.mod")
	}
	if err != nil {
		return nil, author(err, fmt.Sprintf("The suite module could not be read: %v", err))
	}
	gen, err := c.evaluator.GenerateWithStories(ctx, files, stories)
	if err != nil {
		if isFailure(ctx, err) || !designeval.FailedOnModule(err) {
			return nil, fmt.Errorf("%w: %w", ErrGeneration, err)
		}
		return nil, author(err, designeval.GenerationDiagnostic(err))
	}
	if suites := generatedSuites(gen.Files); suites != 1 {
		return nil, author(nil, fmt.Sprintf("The test module must declare exactly one evaluation suite, but its design "+
			"declares %d: keep exactly one Suite(...) in %s/design, then report again.", suites, modulePath))
	}
	program := path.Join(strings.TrimPrefix(s.Command, "./"), "main.go")
	if _, ok := gen.Files[program]; !ok {
		return nil, author(nil, fmt.Sprintf("Surla generated no test program at %s, because the test module is not "+
			"a Surla test module: its go.mod must declare the module path <service module>/%s, as surla init writes it, "+
			"and it must keep its %s package, which provides Name, Hooks, New, Live and LiveThroughGateway.",
			s.Command, designeval.TestModuleDir, designeval.AcceptancePackage))
	}
	if err := publish(ctx, root, gen.Files); err != nil {
		return nil, err
	}
	return gen.Included, nil
}

// listScenarios builds the suite command of the snapshot module at moduleDir in
// the build sandbox, with src as the only source it may read, within
// c.buildLimit, runs its --describe in the candidate sandbox within
// c.listLimit, and returns the scenarios it declares, or findings when the
// module's dependencies, its build or its listing fail or reach their limit.
// tmp is the check's private folder; keep is set when build processes may
// still use it.
func (c *Checker) listScenarios(
	ctx context.Context, tmp, src, moduleDir, command string, keep *bool,
) ([]suite.DeclaredScenario, []Finding, error) {
	finding := func(kind FindingKind, message string) []Finding {
		return []Finding{{Kind: kind, Message: forAuthor(message, src)}}
	}
	tc, err := resolveToolchain(ctx, tmp)
	if err != nil {
		return nil, nil, err
	}
	download := exec.CommandContext(ctx, filepath.Join(tc.goroot, "bin", "go"), "mod", "download")
	download.Dir, download.Env = moduleDir, goEnv()
	// The download's output is withheld from the finding: it runs outside
	// the sandbox with the user's proxy settings, which it may print.
	if err := download.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("download the suite module's dependencies: %w", err)
		}
		return nil, finding(FindingDependencies, "The suite module's dependencies could not be downloaded "+
			"(go mod download failed). Check that its go.mod and go.sum list every module it imports, "+
			"for example by running go mod tidy in it."), nil
	}
	modCache, err := abspath.Eval(tc.modCache)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve the module cache: %w", err)
	}
	buildHome := filepath.Join(c.workDir, "build-home")
	binDir := filepath.Join(tmp, "bin")
	for _, d := range []string{buildHome, binDir, filepath.Join(tmp, "state")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	bin, buildLog := filepath.Join(binDir, "suite"), filepath.Join(tmp, "build.log")
	reached, err := withinLimit(ctx, c.buildLimit, func(ctx context.Context) error {
		return c.build(ctx, candidate.BuildSpec{
			SourceDir: src, ModuleDir: moduleDir, Package: command, Out: bin,
			GOROOT: tc.goroot, ModCache: modCache, WorkDir: buildHome, LogFile: buildLog,
		}, runBuild(keep))
	})
	if reached && !*keep {
		return nil, finding(FindingBuild, fmt.Sprintf("The test program did not finish building within %s. "+
			"Make the test module build faster, for example with fewer or smaller dependencies, then report again.",
			limitText(c.buildLimit))), nil
	}
	if err != nil {
		if *keep || isFailure(ctx, err) {
			return nil, nil, fmt.Errorf("build the suite command: %w", err)
		}
		found, readErr := logFinding(FindingBuild, "The suite command does not compile:\n", buildLog, src)
		if readErr != nil {
			return nil, nil, errors.Join(fmt.Errorf("build the suite command: %w", err), readErr)
		}
		return nil, found, nil
	}
	art, err := candidate.Pin(filepath.Join(tmp, "store"), bin)
	if err != nil {
		return nil, nil, fmt.Errorf("pin the suite command: %w", err)
	}
	stderrLog := filepath.Join(tmp, "describe.log")
	var out []byte
	reached, err = withinLimit(ctx, c.listLimit, func(ctx context.Context) error {
		var err error
		out, err = c.list(ctx, art, candidate.Spec{
			Args: []string{"--describe"}, StateDir: filepath.Join(tmp, "state"), LogFile: stderrLog,
		}, maxDeclarationBytes)
		return err
	})
	if reached {
		return nil, finding(FindingListing, fmt.Sprintf("Listing the tests (--describe) did not finish within %s. "+
			"Listing must not wait: the test module's package initialization (init functions and package-level "+
			"variables) and a zero Hooks must not need the program under test.", limitText(c.listLimit))), nil
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case isFailure(ctx, err):
		return nil, nil, fmt.Errorf("list the declared scenarios: %w", err)
	case errors.Is(err, candidate.ErrOutputTooLarge):
		return nil, finding(FindingListing, fmt.Sprintf(
			"The suite command's --describe printed more than %d bytes on standard output.", maxDeclarationBytes)), nil
	case errors.As(err, &exitErr):
		head := fmt.Sprintf("The suite command's --describe exited with status %d. Its standard error:\n", exitErr.ExitCode())
		found, readErr := logFinding(FindingListing, head, stderrLog, src)
		if readErr != nil {
			return nil, nil, errors.Join(fmt.Errorf("list the declared scenarios: %w", err), readErr)
		}
		return nil, found, nil
	default:
		return nil, nil, fmt.Errorf("list the declared scenarios: %w", err)
	}
	d, err := decodeDeclaration(out)
	if err != nil {
		return nil, finding(FindingListing, "The suite command's --describe did not print one suite declaration "+
			"on standard output: "+err.Error()), nil
	}
	return d.Scenarios, nil, nil
}

// validate requires a local module path and a single-package command path.
func (s Suite) validate() error {
	if s.Module == "" || !filepath.IsLocal(filepath.FromSlash(s.Module)) || path.Clean(s.Module) != s.Module {
		return fmt.Errorf("suite module %q is not a clean path inside the project", s.Module)
	}
	if c := s.Command; (c != "." && !strings.HasPrefix(c, "./")) || !filepath.IsLocal(filepath.FromSlash(c)) ||
		strings.Contains(c, "...") {
		return fmt.Errorf("suite command %q is not one package path inside the suite module", s.Command)
	}
	return nil
}
