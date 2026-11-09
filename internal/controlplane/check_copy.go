// This file copies the live grant (the workspace's enclosing Git working tree,
// or the workspace itself) into one check operation's private directory, the
// only source view any check or dependency-repair container ever sees.
package controlplane

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

type (
	// grantCopy is the result of copying one grant: the modules found under
	// the workspace and the manifest bytes as they were read from the live
	// tree.
	grantCopy struct {
		// modules are workspace-relative module directories, in lexical
		// order, such as "." and "eval".
		modules []string
		// manifests maps a grant-relative slash path of each copied go.mod,
		// go.sum, go.work and go.work.sum to the bytes read from the live
		// file during the copy.
		manifests map[string][]byte
		// skipped lists grant-relative paths of entries that are neither
		// regular files, directories nor symlinks (sockets, devices), which a
		// copy cannot reproduce.
		skipped []string
	}

	// grantCopier walks one grant through descriptors it holds and writes the
	// copy. It never follows a symlink and decides imported-report exclusion
	// per entry with the kernel's own name resolution.
	grantCopier struct {
		// workspaceRel is the workspace's slash path relative to the grant
		// root, "." when they are the same directory.
		workspaceRel string
		// recorded maps each recorded original report's parent directory to
		// the report basenames recorded in it.
		recorded map[string][]string
		// out is the result being built.
		out grantCopy
	}

	// errSourceChanged reports that an entry vanished or was replaced while
	// the copy held its directory, so its identity could not be
	// established. The caller asks the agent to call again.
	errSourceChanged struct {
		// rel is the grant-relative path of the entry.
		rel string
		// err is the failure that revealed the change.
		err error
	}
)

// openDirFlags open a directory the walk holds without following a link.
const openDirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// openEntryFlags open any entry itself: a symlink is opened, not followed,
// and a FIFO never blocks.
const openEntryFlags = unix.O_RDONLY | unix.O_SYMLINK | unix.O_NONBLOCK | unix.O_CLOEXEC

// copyGrant copies the grant rooted at grantRoot into dst, which must exist
// and be empty. workspaceDir is the active workspace inside the grant, and
// importedReports are the validated absolute original report paths the
// import recorded. It skips .git and .surla* entries and every entry the
// kernel resolves as a recorded original report at the moment its entry is
// decided; it copies every other regular file (keeping its mode), directory
// and symlink (with its raw target).
func copyGrant(grantRoot, workspaceDir, dst string, importedReports []string) (grantCopy, error) {
	rel, err := filepath.Rel(grantRoot, workspaceDir)
	if err != nil || !filepath.IsLocal(rel) && rel != "." {
		return grantCopy{}, fmt.Errorf("workspace %s is not inside grant %s", workspaceDir, grantRoot)
	}
	c := &grantCopier{
		workspaceRel: filepath.ToSlash(rel),
		recorded:     map[string][]string{},
		out:          grantCopy{manifests: map[string][]byte{}},
	}
	for _, p := range importedReports {
		c.recorded[filepath.Dir(p)] = append(c.recorded[filepath.Dir(p)], filepath.Base(p))
	}
	rootFD, err := unix.Open(grantRoot, openDirFlags, 0)
	if err != nil {
		return grantCopy{}, fmt.Errorf("open grant %s: %w", grantRoot, err)
	}
	defer unix.Close(rootFD)
	if c.workspaceRel == "." && hasRegularGoMod(rootFD, ".") {
		c.out.modules = append(c.out.modules, ".")
	}
	if err := c.copyDir(rootFD, ".", dst); err != nil {
		return grantCopy{}, err
	}
	slices.Sort(c.out.modules)
	return c.out, nil
}

// Error implements error.
func (e *errSourceChanged) Error() string {
	return fmt.Sprintf("the workspace changed while check_project was copying %s; call check_project again: %v", e.rel, e.err)
}

// Unwrap returns the failure that revealed the change.
func (e *errSourceChanged) Unwrap() error {
	return e.err
}

// copyDir copies every entry of the held directory dirFD (grant-relative
// slash path rel) into the existing directory dst.
func (c *grantCopier) copyDir(dirFD int, rel, dst string) error {
	names, err := readDirNames(dirFD)
	if err != nil {
		return fmt.Errorf("read directory %s: %w", rel, err)
	}
	recordedNames, err := c.recordedNamesFor(dirFD)
	if err != nil {
		return fmt.Errorf("resolve recorded reports for %s: %w", rel, err)
	}
	for _, name := range names {
		if name == ".git" || strings.HasPrefix(name, ".surla") {
			continue
		}
		if err := c.copyEntry(dirFD, path.Join(rel, name), name, dst, recordedNames); err != nil {
			return err
		}
	}
	return nil
}

// copyEntry copies one entry of dirFD unless the kernel resolves one of
// recordedNames in dirFD to it.
func (c *grantCopier) copyEntry(dirFD int, rel, name, dst string, recordedNames []string) error {
	fd, err := unix.Openat(dirFD, name, openEntryFlags, 0)
	if err != nil {
		return &errSourceChanged{rel: rel, err: err}
	}
	f := os.NewFile(uintptr(fd), rel)
	defer f.Close()
	excluded, err := isRecordedReport(dirFD, fd, recordedNames)
	if err != nil {
		return &errSourceChanged{rel: rel, err: err}
	}
	if excluded {
		return nil
	}
	info, err := f.Stat()
	if err != nil {
		return &errSourceChanged{rel: rel, err: err}
	}
	target := filepath.Join(dst, name)
	switch {
	case info.Mode().IsDir():
		if err := os.Mkdir(target, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", target, err)
		}
		if c.isModuleRoot(dirFD, name, rel) {
			c.addModule(rel)
		}
		if err := c.copyDir(fd, rel, target); err != nil {
			return err
		}
		return os.Chmod(target, info.Mode().Perm()|0o700)
	case info.Mode()&fs.ModeSymlink != 0:
		link, err := readlinkAt(dirFD, name)
		if err != nil {
			return &errSourceChanged{rel: rel, err: err}
		}
		return os.Symlink(link, target)
	case info.Mode().IsRegular():
		return c.copyFile(f, rel, target, info.Mode().Perm())
	default:
		c.out.skipped = append(c.out.skipped, rel)
		return nil
	}
}

// copyFile copies the open regular file f to target with perm, keeping the
// bytes of a Go manifest for the caller.
func (c *grantCopier) copyFile(f *os.File, rel, target string, perm fs.FileMode) error {
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm|0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	var w io.Writer = out
	var manifest *strings.Builder
	switch path.Base(rel) {
	case "go.mod", "go.sum", "go.work", "go.work.sum":
		manifest = &strings.Builder{}
		w = io.MultiWriter(out, manifest)
	}
	if _, err := io.Copy(w, f); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s: %w", rel, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", target, err)
	}
	if manifest != nil {
		c.out.manifests[rel] = []byte(manifest.String())
	}
	return nil
}

// recordedNamesFor returns the recorded report basenames whose recorded
// parent is the directory dirFD, comparing the kernel's stored path of each
// (F_GETPATH), so case and Unicode aliases of a directory resolve to one
// spelling. A recorded parent that does not exist contributes nothing.
func (c *grantCopier) recordedNamesFor(dirFD int) ([]string, error) {
	if len(c.recorded) == 0 {
		return nil, nil
	}
	held, err := fdPath(dirFD)
	if err != nil {
		return nil, err
	}
	var names []string
	for parent, bases := range c.recorded {
		pfd, err := unix.Open(parent, openDirFlags, 0)
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("open recorded report directory %s: %w", parent, err)
		}
		stored, err := fdPath(pfd)
		_ = unix.Close(pfd)
		if err != nil {
			return nil, fmt.Errorf("resolve recorded report directory %s: %w", parent, err)
		}
		if stored == held {
			names = append(names, bases...)
		}
	}
	return names, nil
}

// isRecordedReport reports whether the kernel resolves one of recordedNames
// in dirFD to the open entry entryFD: the same file, or the same stored
// name. A missing recorded name excludes nothing. An entry whose stored name
// cannot be read was unlinked while open, which is an error unless the same
// file is already proven.
func isRecordedReport(dirFD, entryFD int, recordedNames []string) (bool, error) {
	if len(recordedNames) == 0 {
		return false, nil
	}
	var entryStat unix.Stat_t
	if err := unix.Fstat(entryFD, &entryStat); err != nil {
		return false, err
	}
	for _, name := range recordedNames {
		rfd, err := unix.Openat(dirFD, name, openEntryFlags, 0)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return false, err
		}
		match, err := sameEntry(entryFD, rfd, entryStat)
		_ = unix.Close(rfd)
		if err != nil || match {
			return match, err
		}
	}
	return false, nil
}

// sameEntry reports whether open descriptors a and b are the same file or
// carry the same kernel-stored name.
func sameEntry(a, b int, aStat unix.Stat_t) (bool, error) {
	var bStat unix.Stat_t
	if err := unix.Fstat(b, &bStat); err != nil {
		return false, err
	}
	if aStat.Dev == bStat.Dev && aStat.Ino == bStat.Ino {
		return true, nil
	}
	aPath, err := fdPath(a)
	if err != nil {
		return false, err
	}
	bPath, err := fdPath(b)
	if err != nil {
		return false, err
	}
	return filepath.Base(aPath) == filepath.Base(bPath), nil
}

// isModuleRoot reports whether the directory name in dirFD, at
// grant-relative rel, is a module root that go test ./... would visit from
// the workspace: inside the workspace, not under testdata, vendor or a
// directory whose name starts with "." or "_", and holding a regular go.mod.
func (c *grantCopier) isModuleRoot(dirFD int, name, rel string) bool {
	wsRel, ok := c.workspaceRelative(rel)
	if !ok {
		return false
	}
	if wsRel != "." {
		for _, part := range strings.Split(wsRel, "/") {
			if part == "testdata" || part == "vendor" || strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") {
				return false
			}
		}
	}
	return hasRegularGoMod(dirFD, name)
}

// hasRegularGoMod reports whether directory name in dirFD holds go.mod as a
// regular file, without following a link.
func hasRegularGoMod(dirFD int, name string) bool {
	var st unix.Stat_t
	if err := unix.Fstatat(dirFD, name+"/go.mod", &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false
	}
	return st.Mode&unix.S_IFMT == unix.S_IFREG
}

// addModule records the workspace-relative module directory for the
// grant-relative directory rel.
func (c *grantCopier) addModule(rel string) {
	if wsRel, ok := c.workspaceRelative(rel); ok {
		c.out.modules = append(c.out.modules, wsRel)
	}
}

// workspaceRelative returns rel relative to the workspace, and whether rel
// is the workspace or inside it.
func (c *grantCopier) workspaceRelative(rel string) (string, bool) {
	if c.workspaceRel == "." {
		return rel, true
	}
	if rel == c.workspaceRel {
		return ".", true
	}
	if strings.HasPrefix(rel, c.workspaceRel+"/") {
		return strings.TrimPrefix(rel, c.workspaceRel+"/"), true
	}
	return "", false
}

// readDirNames returns the stored names in the held directory dirFD, sorted.
func readDirNames(dirFD int) ([]string, error) {
	dup, err := unix.Dup(dirFD)
	if err != nil {
		return nil, err
	}
	d := os.NewFile(uintptr(dup), "dir")
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	return names, nil
}

// readlinkAt returns the raw target of the symlink name in dirFD.
func readlinkAt(dirFD int, name string) (string, error) {
	buf := make([]byte, unix.PathMax)
	n, err := unix.Readlinkat(dirFD, name, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}
