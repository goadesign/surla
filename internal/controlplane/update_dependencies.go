// This file implements update_dependencies: run go mod tidy for every
// builder-owned workspace module in a protected copy inside Linux containers
// (leaving out the starter test module eval when the project's tests belong to
// the test writer), then write only the changed go.mod, go.sum and go.work.sum
// files back to the live workspace through descriptors that never follow a link.
package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sys/unix"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/renameat"
	"github.com/goadesign/surla/internal/scaffold"
)

const (
	// maxCleanupWarningBytes bounds the post-write failure text in an
	// update_dependencies message, so it never displaces the written paths.
	maxCleanupWarningBytes = 2 << 10
	// skippedTestModuleNote explains in update_dependencies' result and
	// output why the starter test module eval was left untouched.
	skippedTestModuleNote = "Left test module eval out (tests belong to the test writer)"
)

var (
	// errManifestConflict reports that a live manifest cannot be updated
	// safely: it is a symlink, not a regular file, or changed during the
	// call.
	errManifestConflict = errors.New("manifest conflict")
	// errPublicationCanceled reports a changed manifest that was not written
	// because the operation was canceled or ran out of time first.
	errPublicationCanceled = errors.New("the operation was canceled or exceeded check timeout before writing it")
	// fsyncDir makes a rename durable; tests replace it to inject a failure
	// after the rename succeeded.
	fsyncDir = unix.Fsync
)

// UpdateDependencies runs go mod tidy for every Go module of the active
// workspace in a protected copy (except the starter test module eval when the
// project has no stored verification settings, because those tests belong to
// the test writer), populates go.work.sum when the copy has a go.work, and
// publishes only the changed manifests. The work runs in a check helper
// process that finishes and cleans up the operation even if this server dies.
// Modules are published one at a time with Go's normal partial semantics. Once
// a manifest is replaced, later failures are reported next to the written
// paths, never instead of them. It requires an authenticated principal.
func (c *ControlPlane) UpdateDependencies(ctx context.Context) (res *gensurla.UpdateDependenciesResult, retErr error) {
	if _, ok := PrincipalFromContext(ctx); !ok {
		return nil, errors.New("authenticated principal is required")
	}
	ctx, span := c.tracer("surla/controlplane").Start(ctx, "surla.controlplane.update_dependencies")
	if err := c.admitCheckOperation(); err != nil {
		endCheckSpan(span, err)
		return nil, err
	}
	defer func() {
		endCheckSpan(span, retErr)
		c.checkOps.Done()
	}()
	ctx, cancel := c.checkDeadline(ctx)
	defer cancel()
	result, err := c.runCheckHelper(ctx, checkToolDependencies)
	if err != nil {
		return nil, err
	}
	return result.Update, nil
}

// updateDependencies runs update_dependencies' operation inside the check
// helper: it prepares the operation, runs go mod tidy (and go list -deps
// -test ./... when the copy has a go.work) for every module, publishes the
// changed manifests of the modules that succeeded unless the operation was
// canceled first, and finishes the operation directory. A cancellation that
// stops publication before any file was written is an error; one that
// stops it after some files were written leaves them reported with the
// files that were not.
func (op *checkOperation) updateDependencies(ctx context.Context) (*gensurla.UpdateDependenciesResult, error) {
	if err := op.start(ctx); err != nil {
		return nil, err
	}
	leftOutTestModule := op.leaveOutStarterTestModule()
	if leftOutTestModule {
		fmt.Fprintf(op.out, "note: %s\n", skippedTestModuleNote)
	}
	var failures []moduleOutcome
	for _, mod := range op.copy.modules {
		if f, ok, err := op.runGo(ctx, mod, "tidy", "bridge", false, op.goProxyEnv, "mod", "tidy"); err != nil {
			return nil, op.finish(err)
		} else if !ok {
			failures = append(failures, f)
		}
	}
	if op.hasGoWork() {
		for _, mod := range op.copy.modules {
			if f, ok, err := op.runGo(ctx, mod, "download", "bridge", false, op.goProxyEnv, "list", "-deps", "-test", "./..."); err != nil {
				return nil, op.finish(err)
			} else if !ok {
				failures = append(failures, f)
			}
		}
	}
	written, notes, canceled := op.publishManifests(ctx, failures)
	if canceled && len(written) == 0 {
		return nil, op.finish(fmt.Errorf("check exceeded check timeout (%s) or was canceled before writing the dependency files; nothing was written: %w", op.checkTimeout, ctx.Err()))
	}
	if leftOutTestModule {
		notes = append([]string{skippedTestModuleNote}, notes...)
	}
	warning := ""
	if err := op.finish(nil); err != nil {
		if len(written) == 0 {
			return nil, err
		}
		trace.SpanFromContext(ctx).RecordError(err)
		warning = clipText(fmt.Sprintf(" The files were written, but finishing the operation failed: %v.", err), maxCleanupWarningBytes)
	}
	return &gensurla.UpdateDependenciesResult{
		OpID:         op.id,
		FilesWritten: written,
		Message:      op.updateMessage(written, notes, warning, failures),
	}, nil
}

// leaveOutStarterTestModule removes the starter test module (eval) from
// op.copy.modules when the project has no stored verification settings and
// the workspace holds a starter test layout (go.mod and eval/go.mod accepted
// by scaffold.StarterLayout). Tests in eval then belong to the test writer,
// so update_dependencies neither tidies nor writes eval/go.mod or eval/go.sum.
func (op *checkOperation) leaveOutStarterTestModule() bool {
	if op.storedEvaluator || !slices.Contains(op.copy.modules, ".") || !slices.Contains(op.copy.modules, scaffold.SuiteDir) {
		return false
	}
	rootMod, rootOK := op.copy.manifests[path.Join(op.moduleGrantRel("."), "go.mod")]
	evalMod, evalOK := op.copy.manifests[path.Join(op.moduleGrantRel(scaffold.SuiteDir), "go.mod")]
	if !rootOK || !evalOK || len(rootMod) > maxLayoutFileBytes || len(evalMod) > maxLayoutFileBytes {
		return false
	}
	if _, err := scaffold.StarterLayout(rootMod, evalMod); err != nil {
		return false
	}
	op.copy.modules = slices.DeleteFunc(op.copy.modules, func(mod string) bool {
		return mod == scaffold.SuiteDir
	})
	return true
}

// hasGoWork reports whether the copy holds a go.work file.
func (op *checkOperation) hasGoWork() bool {
	for rel := range op.copy.manifests {
		if path.Base(rel) == "go.work" {
			return true
		}
	}
	return false
}

// publishManifests writes each changed manifest of every module whose tidy
// succeeded, plus changed go.work.sum files, to the live grant, one file at
// a time. Before each file it checks ctx: once the operation is canceled no
// new file is written, while a write already begun finishes. Each file
// written or refused is logged to the output as it happens, so the output
// stays truthful if the helper is killed between files. It returns an
// initialized list of workspace-relative paths written, one note per file
// refused, not written because of the cancellation, or written without
// being made durable, and whether the cancellation stopped any file. A
// cancellation can leave a module with its new go.mod and its old go.sum;
// the result reports both.
func (op *checkOperation) publishManifests(ctx context.Context, failures []moduleOutcome) ([]string, []string, bool) {
	failed := map[string]bool{}
	for _, f := range failures {
		failed[f.dir] = true
	}
	var candidates []string
	for _, mod := range op.copy.modules {
		if failed[mod] {
			continue
		}
		base := op.moduleGrantRel(mod)
		candidates = append(candidates, path.Join(base, "go.mod"), path.Join(base, "go.sum"))
	}
	for rel := range op.copy.manifests {
		if path.Base(rel) == "go.work" {
			candidates = append(candidates, path.Join(path.Dir(rel), "go.work.sum"))
		}
	}
	slices.Sort(candidates)
	candidates = slices.Compact(candidates)
	span := trace.SpanFromContext(ctx)
	written := []string{}
	var notes []string
	canceled := false
	for _, rel := range candidates {
		replaced, err := op.publishManifest(ctx, rel)
		wsRel, relErr := filepath.Rel(op.workspaceRel, rel)
		if relErr != nil {
			wsRel = rel
		}
		note := ""
		switch {
		case replaced && err != nil:
			written = append(written, wsRel)
			note = fmt.Sprintf("%s was written, but making the write durable failed: %v", wsRel, err)
			span.AddEvent("manifest_written", trace.WithAttributes(attribute.String("surla.check.path", wsRel), attribute.String("surla.check.warning", err.Error())))
		case replaced:
			written = append(written, wsRel)
			span.AddEvent("manifest_written", trace.WithAttributes(attribute.String("surla.check.path", wsRel)))
		case errors.Is(err, errPublicationCanceled):
			canceled = true
			note = fmt.Sprintf("%s was not written: %v", wsRel, err)
			span.AddEvent("manifest_not_written", trace.WithAttributes(attribute.String("surla.check.path", wsRel), attribute.String("surla.check.reason", err.Error())))
		case err != nil:
			note = fmt.Sprintf("%s was not written: %v", wsRel, err)
			span.AddEvent("manifest_refused", trace.WithAttributes(attribute.String("surla.check.path", wsRel), attribute.String("surla.check.reason", err.Error())))
		}
		if replaced {
			fmt.Fprintf(op.out, "==> wrote: %s\n", wsRel)
		}
		if note != "" {
			notes = append(notes, note)
			fmt.Fprintf(op.out, "note: %s\n", note)
		}
	}
	return written, notes, canceled
}

// publishManifest compares the copy's bytes of the grant-relative manifest
// rel with the bytes read from the live file during the copy, and writes the
// new bytes to the live file when they differ, unless ctx has ended: a
// changed file is then not written, and the error wraps
// errPublicationCanceled. Both sides are opened through held directory
// descriptors without following a link; the live side starts from the held
// workspace or grant. It reports whether the live file was replaced: true
// with an error means the new bytes are in place but the write could not be
// made durable.
//
// The live comparison and the replacement run under an exclusive,
// non-blocking flock of the live directory, so two update_dependencies
// calls, in this server or another one on the same workspace, never both
// replace a file read with the same old bytes: the second gets a conflict
// and calls again. An editor that does not take that lock can still write
// the file between the comparison and the rename; edits made before the
// comparison are always detected and kept.
func (op *checkOperation) publishManifest(ctx context.Context, rel string) (bool, error) {
	after, afterOK, err := readNoFollow(op.path("src"), rel)
	if err != nil {
		return false, err
	}
	before, beforeOK := op.copy.manifests[rel]
	if !afterOK || (beforeOK && bytes.Equal(before, after)) {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("%w: %w", errPublicationCanceled, err)
	}
	_, span := checkTracer(ctx).Start(ctx, "surla.check.publish_manifest", trace.WithAttributes(attribute.String("surla.check.path", rel)))
	replaced, err := op.replaceManifest(rel, before, beforeOK, after)
	span.SetAttributes(attribute.Bool("surla.check.replaced", replaced))
	if err != nil && !replaced {
		span.AddEvent("manifest_refused", trace.WithAttributes(attribute.String("surla.check.reason", err.Error())))
		span.End()
		return false, err
	}
	endCheckSpan(span, err)
	return replaced, err
}

// replaceManifest replaces the live manifest rel with after under the
// directory's flock when the live bytes still equal before (or the file is
// still absent when beforeOK is false). It reports whether the rename
// happened; see publishManifest.
func (op *checkOperation) replaceManifest(rel string, before []byte, beforeOK bool, after []byte) (bool, error) {
	dirFD, err := op.liveDirAt(path.Dir(rel))
	if err != nil {
		return false, err
	}
	defer unix.Close(dirFD)
	if err := unix.Flock(dirFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return false, fmt.Errorf("%w: another update_dependencies is writing the directory of %s; call again", errManifestConflict, rel)
		}
		return false, fmt.Errorf("lock the directory of %s: %w", rel, err)
	}
	name := path.Base(rel)
	live, liveOK, err := readAtNoFollow(dirFD, name)
	if err != nil {
		return false, err
	}
	switch {
	case liveOK && !beforeOK:
		return false, fmt.Errorf("%w: %s was created during update_dependencies; call again", errManifestConflict, name)
	case !liveOK && beforeOK:
		return false, fmt.Errorf("%w: %s was removed during update_dependencies; call again", errManifestConflict, name)
	case liveOK && !bytes.Equal(live, before):
		return false, fmt.Errorf("%w: %s changed during update_dependencies; call again", errManifestConflict, name)
	}
	return writeAtomic(dirFD, name, after, !beforeOK)
}

// updateMessage is the update_dependencies message, bounded as a whole: the
// written files and refusal notes, any failure after the writes, then the
// first failed module and the end of its output.
func (op *checkOperation) updateMessage(written, notes []string, warning string, failures []moduleOutcome) string {
	head := "No go.mod, go.sum or go.work.sum needed changes."
	list := ""
	if len(written) > 0 {
		head = fmt.Sprintf("Wrote %d file(s): ", len(written))
		list = strings.Join(written, ", ") + "."
	}
	for _, n := range notes {
		list += " " + n + "."
	}
	if len(failures) == 0 {
		return op.boundedMessage(head, list, warning, nil)
	}
	f := failures[0]
	body := warning + fmt.Sprintf(" go %s failed for %s (exit %d); its files were not written. Read the full output with read_check_output(op_id). Last output:\n", f.step, f.dir, f.exitCode)
	return op.boundedMessage(head, list, body, &f)
}

// openDirNoFollow opens the slash-relative directory rel under the
// directory path root without following a link.
func openDirNoFollow(root, rel string) (int, error) {
	fd, err := unix.Open(root, openDirFlags, 0)
	if err != nil {
		return -1, fmt.Errorf("open %s: %w", root, err)
	}
	return walkDirNoFollow(fd, rel)
}

// openDirAt opens the slash-relative directory rel under the held
// directory base without following a link, leaving base open.
func openDirAt(base int, rel string) (int, error) {
	fd, err := unix.Openat(base, ".", openDirFlags, 0)
	if err != nil {
		return -1, fmt.Errorf("reopen the held directory: %w", err)
	}
	return walkDirNoFollow(fd, rel)
}

// walkDirNoFollow walks from the open directory fd to the slash-relative
// directory rel, one component at a time, refusing any symlink component.
// It takes ownership of fd and returns the descriptor of rel.
func walkDirNoFollow(fd int, rel string) (int, error) {
	if rel == "." {
		return fd, nil
	}
	for _, part := range strings.Split(rel, "/") {
		next, err := unix.Openat(fd, part, openDirFlags, 0)
		if err := errors.Join(err, unix.Close(fd)); err != nil {
			return -1, errors.Join(fmt.Errorf("open directory %s: %w", rel, err), closeFD(next))
		}
		fd = next
	}
	return fd, nil
}

// readNoFollow reads the slash-relative file rel under root through
// no-follow descriptors. A missing file is (nil, false, nil); a symlink or
// non-regular file is an error.
func readNoFollow(root, rel string) ([]byte, bool, error) {
	dirFD, err := openDirNoFollow(root, path.Dir(rel))
	if err != nil {
		return nil, false, err
	}
	defer unix.Close(dirFD)
	return readAtNoFollow(dirFD, path.Base(rel))
}

// readAtNoFollow reads name in dirFD without following a link and requires
// a regular file. A missing file is (nil, false, nil).
func readAtNoFollow(dirFD int, name string) ([]byte, bool, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, false, nil
	case errors.Is(err, unix.ELOOP):
		return nil, false, fmt.Errorf("%w: %s is a symbolic link; replace it with a regular file", errManifestConflict, name)
	case err != nil:
		return nil, false, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%w: %s is not a regular file", errManifestConflict, name)
	}
	b, err := io.ReadAll(f)
	return b, true, err
}

// writeAtomic writes data to name in dirFD through a temporary file in the
// same directory, then renames it into place: exclusively when create is
// true, so a concurrently created file is never overwritten. It reports
// whether the rename happened; an error after it means name holds data but
// the directory entry may not yet be durable.
func writeAtomic(dirFD int, name string, data []byte, create bool) (bool, error) {
	tmp := "." + name + ".surla-" + newOperationID()
	fd, err := unix.Openat(dirFD, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return false, fmt.Errorf("create temporary %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), tmp)
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		return false, errors.Join(fmt.Errorf("write %s: %w", name, err), unix.Unlinkat(dirFD, tmp, 0))
	}
	if create {
		err = renameat.NoReplace(dirFD, tmp, dirFD, name)
		if errors.Is(err, unix.EEXIST) {
			return false, errors.Join(fmt.Errorf("%w: %s was created concurrently; call again", errManifestConflict, name), unix.Unlinkat(dirFD, tmp, 0))
		}
	} else {
		err = unix.Renameat(dirFD, tmp, dirFD, name)
	}
	if err != nil {
		return false, errors.Join(fmt.Errorf("replace %s: %w", name, err), unix.Unlinkat(dirFD, tmp, 0))
	}
	if err := fsyncDir(dirFD); err != nil {
		return true, fmt.Errorf("sync the directory of %s: %w", name, err)
	}
	return true, nil
}
