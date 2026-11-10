// This file implements update_dependencies: run go mod tidy for every
// workspace module in a protected copy inside Linux containers, then write
// only the changed go.mod, go.sum and go.work.sum files back to the live
// workspace through descriptors that never follow a link.
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
)

// maxCleanupWarningBytes bounds the post-write failure text in an
// update_dependencies message, so it never displaces the written paths.
const maxCleanupWarningBytes = 2 << 10

var (
	// errManifestConflict reports that a live manifest cannot be updated
	// safely: it is a symlink, not a regular file, or changed during the
	// call.
	errManifestConflict = errors.New("manifest conflict")
	// fsyncDir makes a rename durable; tests replace it to inject a failure
	// after the rename succeeded.
	fsyncDir = unix.Fsync
)

// UpdateDependencies runs go mod tidy for every Go module of the active
// workspace in a protected copy, populates go.work.sum when the copy has a
// go.work, and publishes only the changed manifests. Modules are published
// one at a time with Go's normal partial semantics. Once a manifest is
// replaced, later failures are reported next to the written paths, never
// instead of them. It requires an authenticated principal.
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
	op, err := c.startCheckOperation(ctx)
	if err != nil {
		return nil, err
	}
	var failures []moduleOutcome
	for _, mod := range op.copy.modules {
		if f, ok, err := op.runGo(ctx, mod, "tidy", "bridge", false, op.c.goProxyEnv, "mod", "tidy"); err != nil {
			return nil, op.finish(err)
		} else if !ok {
			failures = append(failures, f)
		}
	}
	if op.hasGoWork() {
		for _, mod := range op.copy.modules {
			if f, ok, err := op.runGo(ctx, mod, "download", "bridge", false, op.c.goProxyEnv, "list", "-deps", "-test", "./..."); err != nil {
				return nil, op.finish(err)
			} else if !ok {
				failures = append(failures, f)
			}
		}
	}
	written, notes := op.publishManifests(ctx, failures)
	if len(written) > 0 {
		fmt.Fprintf(op.out, "==> wrote: %s\n", strings.Join(written, ", "))
	}
	for _, n := range notes {
		fmt.Fprintf(op.out, "note: %s\n", n)
	}
	warning := ""
	if err := op.finish(nil); err != nil {
		if len(written) == 0 {
			return nil, err
		}
		span.RecordError(err)
		warning = clipText(fmt.Sprintf(" The files were written, but finishing the operation failed: %v.", err), maxCleanupWarningBytes)
	}
	return &gensurla.UpdateDependenciesResult{
		OpID:         op.id,
		FilesWritten: written,
		Message:      op.updateMessage(written, notes, warning, failures),
	}, nil
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
// succeeded, plus changed go.work.sum files, to the live grant. It returns
// an initialized list of workspace-relative paths written and one note per
// refused file or per written file whose write could not be made durable.
func (op *checkOperation) publishManifests(ctx context.Context, failures []moduleOutcome) ([]string, []string) {
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
	for _, rel := range candidates {
		replaced, err := op.publishManifest(rel)
		wsRel, relErr := filepath.Rel(op.workspaceRel, rel)
		if relErr != nil {
			wsRel = rel
		}
		switch {
		case replaced && err != nil:
			written = append(written, wsRel)
			notes = append(notes, fmt.Sprintf("%s was written, but making the write durable failed: %v", wsRel, err))
			span.AddEvent("manifest_written", trace.WithAttributes(attribute.String("surla.check.path", wsRel), attribute.String("surla.check.warning", err.Error())))
		case replaced:
			written = append(written, wsRel)
			span.AddEvent("manifest_written", trace.WithAttributes(attribute.String("surla.check.path", wsRel)))
		case err != nil:
			notes = append(notes, fmt.Sprintf("%s was not written: %v", wsRel, err))
			span.AddEvent("manifest_refused", trace.WithAttributes(attribute.String("surla.check.path", wsRel), attribute.String("surla.check.reason", err.Error())))
		}
	}
	return written, notes
}

// publishManifest compares the copy's bytes of the grant-relative manifest
// rel with the bytes read from the live file during the copy, and writes the
// new bytes to the live file when they differ. Both sides are opened
// through held directory descriptors without following a link; the live
// side starts from the held workspace or grant. It reports whether the live
// file was replaced: true with an error means the new bytes are in place but
// the write could not be made durable.
//
// The live comparison and the replacement run under an exclusive,
// non-blocking flock of the live directory, so two update_dependencies
// calls, in this server or another one on the same workspace, never both
// replace a file read with the same old bytes: the second gets a conflict
// and calls again. An editor that does not take that lock can still write
// the file between the comparison and the rename; edits made before the
// comparison are always detected and kept.
func (op *checkOperation) publishManifest(rel string) (bool, error) {
	after, afterOK, err := readNoFollow(op.path("src"), rel)
	if err != nil {
		return false, err
	}
	before, beforeOK := op.copy.manifests[rel]
	if !afterOK || (beforeOK && bytes.Equal(before, after)) {
		return false, nil
	}
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
		err = unix.RenameatxNp(dirFD, tmp, dirFD, name, unix.RENAME_EXCL)
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
