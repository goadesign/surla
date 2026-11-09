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

	gensurla "github.com/goadesign/surla/gen/surla"
	"golang.org/x/sys/unix"
)

// errManifestConflict reports that a live manifest cannot be updated safely:
// it is a symlink, not a regular file, or changed during the call.
var errManifestConflict = errors.New("manifest conflict")

// UpdateDependencies runs go mod tidy for every Go module of the active
// workspace in a protected copy, populates go.work.sum when the copy has a
// go.work, and publishes only the changed manifests. Modules are published
// one at a time with Go's normal partial semantics.
func (c *ControlPlane) UpdateDependencies(ctx context.Context) (*gensurla.UpdateDependenciesResult, error) {
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
	written, notes := op.publishManifests(failures)
	if err := op.finish(nil); err != nil {
		return nil, err
	}
	return &gensurla.UpdateDependenciesResult{
		OpID:         op.id,
		FilesWritten: written,
		Message:      op.updateMessage(written, notes, failures),
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
// the workspace-relative paths written and one note per refused file.
func (op *checkOperation) publishManifests(failures []moduleOutcome) ([]string, []string) {
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
	var written, notes []string
	for _, rel := range candidates {
		changed, err := op.publishManifest(rel)
		wsRel, _ := filepath.Rel(op.workspaceDir, filepath.Join(op.grantRoot, filepath.FromSlash(rel)))
		switch {
		case err != nil:
			notes = append(notes, fmt.Sprintf("%s was not written: %v", filepath.ToSlash(wsRel), err))
		case changed:
			written = append(written, filepath.ToSlash(wsRel))
		}
	}
	return written, notes
}

// publishManifest compares the copy's bytes of the grant-relative manifest
// rel with the bytes read from the live file during the copy, and writes the
// new bytes to the live file when they differ. Both sides are opened
// through held directory descriptors without following a link.
func (op *checkOperation) publishManifest(rel string) (bool, error) {
	after, afterOK, err := readNoFollow(op.path("src"), rel)
	if err != nil {
		return false, err
	}
	before, beforeOK := op.copy.manifests[rel]
	if !afterOK || (beforeOK && bytes.Equal(before, after)) {
		return false, nil
	}
	dirFD, err := openDirNoFollow(op.grantRoot, path.Dir(rel))
	if err != nil {
		return false, err
	}
	defer unix.Close(dirFD)
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
	return true, writeAtomic(dirFD, name, after, !beforeOK)
}

// updateMessage is the bounded update_dependencies message.
func (op *checkOperation) updateMessage(written, notes []string, failures []moduleOutcome) string {
	var b strings.Builder
	if len(written) == 0 {
		b.WriteString("No go.mod, go.sum or go.work.sum needed changes.")
	} else {
		fmt.Fprintf(&b, "Wrote %s.", strings.Join(written, ", "))
	}
	for _, n := range notes {
		b.WriteString(" " + n + ".")
	}
	if len(failures) == 0 {
		return op.withGap(b.String())
	}
	f := failures[0]
	fmt.Fprintf(&b, " go %s failed for %s (exit %d); its files were not written. Read the full output with read_check_output(op_id). Last output:\n", f.step, f.dir, f.exitCode)
	return op.withGap(b.String() + op.out.tail(f.start, f.end, maxCheckMessageBytes-b.Len()-256))
}

// openDirNoFollow opens the slash-relative directory rel under root,
// one component at a time, refusing any symlink component.
func openDirNoFollow(root, rel string) (int, error) {
	fd, err := unix.Open(root, openDirFlags, 0)
	if err != nil {
		return -1, fmt.Errorf("open %s: %w", root, err)
	}
	if rel == "." {
		return fd, nil
	}
	for _, part := range strings.Split(rel, "/") {
		next, err := unix.Openat(fd, part, openDirFlags, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, fmt.Errorf("open directory %s: %w", rel, err)
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
// true, so a concurrently created file is never overwritten.
func writeAtomic(dirFD int, name string, data []byte, create bool) error {
	tmp := "." + name + ".surla-" + newOperationID()
	fd, err := unix.Openat(dirFD, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), tmp)
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = unix.Unlinkat(dirFD, tmp, 0)
		return fmt.Errorf("write %s: %w", name, err)
	}
	if create {
		err = unix.RenameatxNp(dirFD, tmp, dirFD, name, unix.RENAME_EXCL)
		if errors.Is(err, unix.EEXIST) {
			_ = unix.Unlinkat(dirFD, tmp, 0)
			return fmt.Errorf("%w: %s was created concurrently; call again", errManifestConflict, name)
		}
	} else {
		err = unix.Renameat(dirFD, tmp, dirFD, name)
	}
	if err != nil {
		_ = unix.Unlinkat(dirFD, tmp, 0)
		return fmt.Errorf("replace %s: %w", name, err)
	}
	return unix.Fsync(dirFD)
}
