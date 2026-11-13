// This file copies the live grant (the workspace's enclosing Git working tree,
// or the workspace itself) into one check operation's private directory, the
// only source view any check or dependency-repair container ever sees.
package controlplane

import (
	"context"
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

	"github.com/goadesign/surla/internal/abspath"
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
	// copy. It never follows a symlink and excludes protected entries
	// with the kernel's own name resolution.
	grantCopier struct {
		// ctx bounds the copy by the operation's deadline; the walk checks it
		// before each entry and while reading each file.
		ctx context.Context
		// workspaceRel is the workspace's slash path relative to the grant
		// root, "." when they are the same directory.
		workspaceRel string
		// workspaceID is the verified workspace's identity; a directory at
		// workspaceRel with any other identity was replaced.
		workspaceID fileID
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

	// fileID is a file's device and inode.
	fileID struct {
		// dev is the device number.
		dev uint64
		// ino is the inode number.
		ino uint64
	}

	// ctxReader reads from r until ctx ends, so copying a file that keeps
	// growing still stops at the operation's deadline.
	ctxReader struct {
		// ctx bounds the reads.
		ctx context.Context
		// r is the file being copied.
		r io.Reader
	}
)

// openDirFlags open a directory the walk holds without following a link.
const openDirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// copyGrant copies the grant held open as rootFD into dst, which must exist
// and be empty. workspaceRel is the active workspace's slash path inside the
// grant ("." when they are the same directory), workspaceID is the verified
// workspace's identity. It skips .git and .surla*
// entries, any entry the kernel resolves as the directory's .git or .surla
// (a case or Unicode alias on a case-insensitive volume), each decided when
// its entry is opened; it copies every other regular file (keeping its mode),
// directory and symlink (with its raw target). It refuses a workspace path
// that no longer resolves to the verified workspace, and stops with ctx's
// error when ctx ends.
func copyGrant(ctx context.Context, rootFD int, workspaceRel string, workspaceID fileID, dst string) (grantCopy, error) {
	c := &grantCopier{
		ctx:          ctx,
		workspaceRel: workspaceRel,
		workspaceID:  workspaceID,
		out:          grantCopy{manifests: map[string][]byte{}},
	}
	if c.workspaceRel == "." {
		if err := c.requireWorkspace(rootFD, "."); err != nil {
			return grantCopy{}, err
		}
	}
	if err := c.copyDir(rootFD, ".", dst); err != nil {
		return grantCopy{}, err
	}
	if err := c.recordIfModule(".", dst); err != nil {
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
	for _, name := range names {
		if name == ".git" || strings.HasPrefix(name, ".surla") {
			continue
		}
		if err := c.copyEntry(dirFD, path.Join(rel, name), name, dst); err != nil {
			return err
		}
	}
	return nil
}

// protectedNames are the directories a copy never includes under any name
// the kernel resolves to them.
var protectedNames = []string{".git", ".surla"}

// copyEntry copies one entry of dirFD unless the kernel currently resolves
// .git or .surla in dirFD to it.
func (c *grantCopier) copyEntry(dirFD int, rel, name, dst string) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	fd, err := openEntry(dirFD, name)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) {
			return err
		}
		return &errSourceChanged{rel: rel, err: err}
	}
	f := os.NewFile(uintptr(fd), rel)
	defer f.Close()
	excluded, err := isProtectedEntry(dirFD, fd)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) {
			return err
		}
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
		if rel == c.workspaceRel {
			if err := c.requireWorkspace(fd, rel); err != nil {
				return err
			}
		}
		if err := os.Mkdir(target, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", target, err)
		}
		if err := c.copyDir(fd, rel, target); err != nil {
			return err
		}
		if err := c.recordIfModule(rel, target); err != nil {
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
	if _, err := io.Copy(w, ctxReader{ctx: c.ctx, r: f}); err != nil {
		return errors.Join(fmt.Errorf("copy %s: %w", rel, err), out.Close())
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", target, err)
	}
	if manifest != nil {
		c.out.manifests[rel] = []byte(manifest.String())
	}
	return nil
}

// requireWorkspace fails with errSourceChanged unless the open directory fd
// at grant-relative rel is the verified workspace.
func (c *grantCopier) requireWorkspace(fd int, rel string) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return &errSourceChanged{rel: rel, err: err}
	}
	if (fileID{dev: uint64(st.Dev), ino: st.Ino}) != c.workspaceID {
		return &errSourceChanged{rel: rel, err: errors.New("the workspace directory was replaced")}
	}
	return nil
}

// Read implements io.Reader, failing with ctx's error once ctx ends.
func (r ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// isProtectedEntry reports whether the kernel resolves a protected name
// in dirFD to the open entry entryFD: the same file, or the same stored
// name. A missing protected name excludes nothing. An entry whose stored name
// cannot be read was unlinked while open, which is an error unless the same
// file is already proven.
func isProtectedEntry(dirFD, entryFD int) (bool, error) {
	var entryStat unix.Stat_t
	if err := unix.Fstat(entryFD, &entryStat); err != nil {
		return false, err
	}
	for _, name := range protectedNames {
		rfd, err := openEntry(dirFD, name)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return false, err
		}
		match, err := sameEntry(entryFD, rfd, entryStat)
		err = errors.Join(err, unix.Close(rfd))
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
	aPath, err := abspath.FDPath(a)
	if err != nil {
		return false, err
	}
	bPath, err := abspath.FDPath(b)
	if err != nil {
		return false, err
	}
	return filepath.Base(aPath) == filepath.Base(bPath), nil
}

// recordIfModule schedules the copied directory at grant-relative rel,
// copied to target, for its own go commands when its copy holds a go.mod
// entry, a regular file or a symlink, and it is the workspace or inside it,
// not under testdata, vendor or a directory whose name starts with "." or
// "_". It reads the finished copy, exactly what Go in the container
// receives, and never follows a link on the host: resolving a linked go.mod,
// deciding which module owns the directory's packages, and reporting any
// error are left to Go in the container. It fails only when the copy itself
// cannot be read.
func (c *grantCopier) recordIfModule(rel, target string) error {
	wsRel, ok := c.workspaceRelative(rel)
	if !ok {
		return nil
	}
	if wsRel != "." {
		for _, part := range strings.Split(wsRel, "/") {
			if part == "testdata" || part == "vendor" || strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") {
				return nil
			}
		}
	}
	info, err := os.Lstat(filepath.Join(target, "go.mod"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("read the copied go.mod of %s: %w", rel, err)
	case info.Mode().IsRegular() || info.Mode()&fs.ModeSymlink != 0:
		c.out.modules = append(c.out.modules, wsRel)
	}
	return nil
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
