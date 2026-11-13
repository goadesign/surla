package designeval

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/mod/modfile"
)

// reservedDir is the snapshot directory the evaluator owns; snapshots must not
// use it.
const reservedDir = "surla_designeval"

var (
	// ErrNotRegularFile marks a read refused because the path is not a
	// regular file inside the root: a symbolic link at the path or at a
	// directory on the way, a directory, a FIFO, a device or a socket.
	ErrNotRegularFile = errors.New("not a regular file")
	// ErrFileTooLarge marks a read refused because the regular file holds
	// more bytes than the caller's limit.
	ErrFileTooLarge = errors.New("larger than the read limit")
)

// OpenModuleRoot opens dir as an os.Root after checking that dir is not a
// symbolic link and that the opened directory is the one checked.
func OpenModuleRoot(dir string) (string, *os.Root, error) {
	cleanDir := filepath.Clean(dir)
	lstatInfo, err := os.Lstat(cleanDir)
	if err != nil {
		return "", nil, err
	}
	if lstatInfo.Mode()&fs.ModeSymlink != 0 {
		return "", nil, fmt.Errorf("%s is a symbolic link; a design snapshot holds only regular files", dir)
	}
	root, err := os.OpenRoot(cleanDir)
	if err != nil {
		return "", nil, err
	}
	rootInfo, err := root.Stat(".")
	if err != nil {
		return "", nil, errors.Join(err, root.Close())
	}
	if !os.SameFile(lstatInfo, rootInfo) {
		return "", nil, errors.Join(fmt.Errorf("%s changed while opening design snapshot root", dir), root.Close())
	}
	return cleanDir, root, nil
}

// ReadModuleSnapshot reads the module rooted at dir into a design snapshot:
// its go.mod, go.sum and every non-test .go file, keyed by slash-separated
// path relative to dir. Hidden, vendor and testdata directories and nested
// modules are skipped. A symbolic link at dir or anywhere in the module is an
// error, and both directory inspection and regular-file reads go through one
// os.Root handle opened on dir, so a replaced parent directory cannot escape
// dir. Raw file bytes are preserved as read for local preview and diff.
func ReadModuleSnapshot(dir string) (files map[string]string, err error) {
	cleanDir, root, err := OpenModuleRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	return ReadModuleSnapshotFromRoot(cleanDir, root)
}

// ReadModuleSnapshotFromRoot walks root and reads its module snapshot files
// relative to root. cleanDir is used only to format diagnostic paths.
func ReadModuleSnapshotFromRoot(cleanDir string, root *os.Root) (map[string]string, error) {
	files := map[string]string{}
	err := fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link; a design snapshot holds only regular files",
				filepath.Join(cleanDir, filepath.FromSlash(rel)))
		}
		if d.IsDir() {
			if shouldSkipSnapshotDir(root, ".", rel, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		keep := rel == "go.mod" || rel == "go.sum" ||
			(strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go"))
		if !keep || !d.Type().IsRegular() {
			return nil
		}
		b, err := readSnapshotFileBytes(cleanDir, root, rel)
		if err != nil {
			return err
		}
		files[rel] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// ReadDesignFilesFromRoot walks only the `design` subdirectory inside root and
// reads its non-test `.go` regular files keyed by slash-separated path relative
// to root (`design/...`). When `design` does not exist inside root, it returns
// `(nil, nil)`. Hidden, vendor, testdata, and nested-module directories are
// skipped, and symbolic links or non-regular files under `design` fail the read.
func ReadDesignFilesFromRoot(cleanDir string, root *os.Root) (map[string]string, error) {
	info, err := root.Lstat("design")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Join(cleanDir, "design"), err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symbolic link; a design snapshot holds only regular files",
			filepath.Join(cleanDir, "design"))
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", filepath.Join(cleanDir, "design"))
	}
	files := map[string]string{}
	err = fs.WalkDir(root.FS(), "design", func(rel string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link; a design snapshot holds only regular files",
				filepath.Join(cleanDir, filepath.FromSlash(rel)))
		}
		if d.IsDir() {
			if shouldSkipSnapshotDir(root, "design", rel, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") || !d.Type().IsRegular() {
			return nil
		}
		b, err := readSnapshotFileBytes(cleanDir, root, rel)
		if err != nil {
			return err
		}
		files[rel] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// ReadRegularFile reads the regular file at slash-separated path rel inside
// root without following a symbolic link or blocking on a FIFO or device,
// and reads at most limit bytes. Callers read files an agent can write with
// it, so the file can be anything. It checks rel and every directory on the
// way to it with Lstat, as the snapshot walkers check each entry before
// opening it, because a root follows a link inside it even when asked not
// to; it then opens rel with the snapshot readers' opener and refuses a file
// that is not the one it checked. A missing file or directory wraps
// fs.ErrNotExist, a path that is not a regular file there (a link at it or
// on the way, a directory, a FIFO, a device) wraps ErrNotRegularFile, and a
// file of more than limit bytes wraps ErrFileTooLarge; any other error is a
// failure to read.
func ReadRegularFile(root *os.Root, rel string, limit int64) ([]byte, error) {
	for dir := path.Dir(rel); dir != "."; dir = path.Dir(dir) {
		info, err := root.Lstat(filepath.FromSlash(dir))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", rel, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("read %s: %s is %w", rel, dir, ErrNotRegularFile)
		}
	}
	checked, err := root.Lstat(filepath.FromSlash(rel))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	// A link is refused here, before the root would fail to open one that
	// leaves it or would open the file one inside it names.
	if !checked.Mode().IsRegular() {
		return nil, fmt.Errorf("read %s: %w", rel, ErrNotRegularFile)
	}
	return readCheckedFile(root, rel, checked, limit)
}

// ValidateUTF8Snapshot checks that every file path and content in files is
// valid UTF-8 before encoding the snapshot as JSON or sending it to a model,
// because encoding/json silently replaces invalid UTF-8 bytes with U+FFFD.
func ValidateUTF8Snapshot(files map[string]string) error {
	for _, p := range slices.Sorted(maps.Keys(files)) {
		if !utf8.ValidString(p) {
			return fmt.Errorf("design snapshot path %q is not valid UTF-8", p)
		}
		if !utf8.ValidString(files[p]) {
			return fmt.Errorf("design snapshot file %s is not valid UTF-8", p)
		}
	}
	return nil
}

// SnapshotDigest returns the SHA-256 of a snapshot's canonical encoding: each
// path and its content in path order, each length-prefixed, so two snapshots
// have the same digest only when they hold exactly the same files.
func SnapshotDigest(files map[string]string) string {
	h := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(files)) {
		for _, part := range []string{name, files[name]} {
			_, _ = fmt.Fprintf(h, "%d:", len(part))
			_, _ = io.WriteString(h, part)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// shouldSkipSnapshotDir reports whether WalkDir should skip directory rel
// when walking from walkRoot ("." or "design").
func shouldSkipSnapshotDir(root *os.Root, walkRoot, rel, name string) bool {
	if rel == walkRoot {
		return false
	}
	if strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" {
		return true
	}
	_, err := root.Lstat(filepath.FromSlash(rel + "/go.mod"))
	return err == nil
}

// readSnapshotFileBytes opens and reads the regular file at slash-separated
// path rel inside root without blocking on special files. Like
// openSnapshotFile, it relies on its caller having checked that rel is not a
// symbolic link: the snapshot walkers check each entry's type first.
func readSnapshotFileBytes(cleanDir string, root *os.Root, rel string) ([]byte, error) {
	f, err := openSnapshotFile(root, rel)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Join(cleanDir, filepath.FromSlash(rel)), err)
	}
	b, readErr := io.ReadAll(f)
	if err := errors.Join(readErr, f.Close()); err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Join(cleanDir, filepath.FromSlash(rel)), err)
	}
	return b, nil
}

// readCheckedFile opens rel inside root with the snapshot readers' opener and
// reads at most limit bytes of it, refusing it with ErrNotRegularFile when the
// opened file is not checked, the file ReadRegularFile examined: rel was
// replaced between that check and the open.
func readCheckedFile(root *os.Root, rel string, checked fs.FileInfo, limit int64) ([]byte, error) {
	f, err := openSnapshotFile(root, rel)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	opened, err := f.Stat()
	if err == nil && !os.SameFile(checked, opened) {
		err = fmt.Errorf("%s changed while it was opened: %w", rel, ErrNotRegularFile)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("read %s: %w", rel, err), f.Close())
	}
	b, readErr := io.ReadAll(io.LimitReader(f, limit+1))
	if err := errors.Join(readErr, f.Close()); err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("read %s: %w of %d bytes", rel, ErrFileTooLarge, limit)
	}
	return b, nil
}

// openSnapshotFile opens the file at slash-separated path rel inside root
// without blocking on special files, and checks that the opened descriptor is
// a regular file. A root opens the file a symbolic link inside it names even
// with O_NOFOLLOW, so callers check that rel is not a link before calling it.
func openSnapshotFile(root *os.Root, rel string) (*os.File, error) {
	f, err := root.OpenFile(filepath.FromSlash(rel), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is %w", rel, ErrNotRegularFile)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// validateSnapshot checks a design snapshot and returns its parsed go.mod.
//
// A snapshot is the exact files of one Go module: every path is clean,
// slash-separated and relative to the module root, names a .go file, the root
// go.mod or the root go.sum, and stays out of the reserved evaluator
// directory. go.mod, go.sum and at least one design/*.go file are required.
// go.mod must declare a module path and may not replace a module with a local
// directory, because the snapshot is the only source the evaluator reads.
func validateSnapshot(files map[string]string) (*modfile.File, error) {
	designFiles := 0
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := validatePath(name); err != nil {
			return nil, err
		}
		if path.Dir(name) == "design" && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			designFiles++
		}
	}
	if _, ok := files["go.mod"]; !ok {
		return nil, errors.New("design snapshot has no go.mod: give the root directory of a Go module (the directory holding go.mod and design/)")
	}
	if _, ok := files["go.sum"]; !ok {
		return nil, errors.New("design snapshot has no go.sum: the evaluator loads dependencies read-only, so go.sum must list every dependency's checksum; run `go mod tidy` in the module and try again")
	}
	if designFiles == 0 {
		return nil, errors.New("design snapshot has no design/*.go file")
	}
	mod, err := modfile.Parse("go.mod", []byte(files["go.mod"]), nil)
	if err != nil {
		return nil, fmt.Errorf("parse snapshot go.mod: %w", err)
	}
	if mod.Module == nil || mod.Module.Mod.Path == "" {
		return nil, errors.New("snapshot go.mod declares no module path")
	}
	for _, r := range mod.Replace {
		if r.New.Version == "" {
			return nil, fmt.Errorf("snapshot go.mod replaces %s with local directory %q; only versioned replacements are supported",
				r.Old.Path, r.New.Path)
		}
	}
	return mod, nil
}

// validatePath enforces the snapshot path rules for one file.
func validatePath(name string) error {
	switch {
	case name == "" || path.IsAbs(name) || strings.Contains(name, `\`) || path.Clean(name) != name:
		return fmt.Errorf("snapshot path %q is not a clean relative path", name)
	case name == ".." || strings.HasPrefix(name, "../"):
		return fmt.Errorf("snapshot path %q leaves the module", name)
	case name == reservedDir || strings.HasPrefix(name, reservedDir+"/"):
		return fmt.Errorf("snapshot path %q uses the evaluator's reserved directory", name)
	case name == "go.mod" || name == "go.sum":
		return nil
	case path.Base(name) == "go.mod" || path.Base(name) == "go.sum":
		return fmt.Errorf("snapshot path %q is a nested module file; a snapshot holds one module", name)
	case !strings.HasSuffix(name, ".go"):
		return fmt.Errorf("snapshot path %q is not a .go, go.mod or go.sum file", name)
	}
	return nil
}

// stageSnapshot writes exactly the snapshot's bytes under dir, which must be
// a new empty directory. Each file is created exclusively, so nothing that
// already exists, such as a symbolic link, is followed or overwritten; no
// host path named by the snapshot is ever read.
func stageSnapshot(dir string, files map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(files)) {
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return fmt.Errorf("stage %s: %w", name, err)
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("stage %s: %w", name, err)
		}
		_, writeErr := f.WriteString(files[name])
		if closeErr := f.Close(); writeErr != nil || closeErr != nil {
			return fmt.Errorf("stage %s: %w", name, errors.Join(writeErr, closeErr))
		}
	}
	return nil
}
