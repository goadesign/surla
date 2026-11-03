package main

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/sys/unix"
)

// genStage is the .surla-gen-<random> directory runGen creates in the module
// to build the new gen/ in. dir stays open from acquisition to cleanup, so the
// exchange and the removal of the old tree act on that directory even if its
// name is later renamed or replaced.
type genStage struct {
	// name is the directory's name in the module root.
	name string
	// dir is the directory, held open. acquireStage created gen inside it
	// through this handle before the stage existed, so cleanup only ever
	// removes a gen this run created, or, after the exchange, the module's
	// old gen.
	dir *os.Root
}

// newStage creates a new staging directory in root under a random name and
// acquires it with acquireStage.
func newStage(root *os.Root) (*genStage, error) {
	name := ".surla-gen-" + strings.ToLower(rand.Text())
	if err := root.Mkdir(name, 0o755); err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	return acquireStage(root, name)
}

// acquireStage opens the directory name that newStage just created and
// establishes that this run owns what it will later write and remove: the
// opened directory must be empty, and gen is then created inside it through
// the held handle, failing if it exists. A directory that was replaced
// between creation and opening by one holding anything is refused, and
// nothing in it is removed. This guards against a replacement holding files,
// such as a foreign gen/; it does not claim to stop every concurrent change
// by another process of the same user.
func acquireStage(root *os.Root, name string) (*genStage, error) {
	dir, err := root.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("open staging directory: %w", err)
	}
	refuse := func(reason error) (*genStage, error) {
		return nil, errors.Join(fmt.Errorf("staging directory %s is not the empty directory surla gen created; it was left alone: %w", name, reason), dir.Close())
	}
	listing, err := dir.Open(".")
	if err != nil {
		return refuse(err)
	}
	entries, err := listing.Readdirnames(1)
	if err := errors.Join(listingErr(err), listing.Close()); err != nil {
		return refuse(err)
	}
	if len(entries) > 0 {
		return refuse(fmt.Errorf("it holds %s", entries[0]))
	}
	if err := dir.Mkdir("gen", 0o755); err != nil {
		return refuse(err)
	}
	return &genStage{name: name, dir: dir}, nil
}

// listingErr treats the end of a directory listing as no error.
func listingErr(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// write builds gen/ in the staging directory: every generated file, and a
// copy of each top-level regular file of the module's current gen/ that is
// not generated, which `goa gen` also keeps. It syncs every file and both
// directories. The module's gen/ must be a directory or missing.
func (s *genStage) write(root *os.Root, generated map[string][]byte) error {
	current, err := currentGenDir(root)
	if err != nil {
		return err
	}
	for _, p := range slices.Sorted(maps.Keys(generated)) {
		if err := s.dir.MkdirAll(path.Dir(p), 0o755); err != nil {
			return fmt.Errorf("stage %s: %w", p, err)
		}
		if err := writeSynced(s.dir, p, generated[p], 0o644); err != nil {
			return fmt.Errorf("stage %s: %w", p, err)
		}
	}
	if current {
		if err := s.keepUserFiles(root, generated); err != nil {
			return err
		}
	}
	return errors.Join(syncDir(s.dir, "gen"), syncDir(s.dir, "."))
}

// exchange publishes the staged gen/ in one call on the two held directory
// handles, with bare names only: it swaps it with the module's gen/
// (RENAME_SWAP), or moves it in when there is none (RENAME_EXCL, which fails
// if gen/ appeared meanwhile). Afterwards the staging directory holds the
// old tree, if there was one.
func (s *genStage) exchange(root *os.Root) error {
	current, err := currentGenDir(root)
	if err != nil {
		return err
	}
	flag := uint32(unix.RENAME_EXCL)
	if current {
		flag = unix.RENAME_SWAP
	}
	module, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open module directory: %w", err)
	}
	staged, err := s.dir.Open(".")
	if err != nil {
		return errors.Join(fmt.Errorf("open staging directory: %w", err), module.Close())
	}
	exchangeErr := renameatx(int(staged.Fd()), "gen", int(module.Fd()), "gen", flag)
	closeErr := errors.Join(staged.Close(), module.Close())
	if exchangeErr != nil {
		return errors.Join(fmt.Errorf("replace gen/: %w", exchangeErr), closeErr)
	}
	// The exchange happened; a failure to close the handles does not undo it.
	return nil
}

// report lists the files under gen/ that the exchange added, changed and
// removed, comparing the old tree, now in the staging directory, with the
// new one.
func (s *genStage) report(root *os.Root) (string, error) {
	before, beforeErr := treeDigests(s.dir)
	after, afterErr := treeDigests(root)
	var added, changed, removed []string
	for p, d := range after {
		old, ok := before[p]
		switch {
		case !ok:
			added = append(added, p)
		case old != d:
			changed = append(changed, p)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			removed = append(removed, p)
		}
	}
	var b strings.Builder
	for _, group := range []struct {
		label string
		paths []string
	}{{"added", added}, {"changed", changed}, {"removed", removed}} {
		slices.Sort(group.paths)
		fmt.Fprintf(&b, "%s: %d\n", group.label, len(group.paths))
		for _, p := range group.paths {
			fmt.Fprintf(&b, "  %s\n", p)
		}
	}
	return b.String(), errors.Join(beforeErr, afterErr)
}

// cleanup removes gen/ inside the held staging directory (the old tree after
// an exchange, or the unpublished new one), then
// removes the staging directory's name only if it still names that same
// directory, with a non-recursive remove. It never deletes recursively by
// name; anything it leaves is named in the error.
func (s *genStage) cleanup(root *os.Root) error {
	removeErr := s.dir.RemoveAll("gen")
	held, statErr := s.dir.Stat(".")
	closeErr := s.dir.Close()
	if removeErr != nil || statErr != nil {
		return errors.Join(fmt.Errorf("left %s with the old gen/ in it", s.name), removeErr, statErr, closeErr)
	}
	named, err := root.Lstat(s.name)
	if err != nil || !os.SameFile(held, named) {
		return errors.Join(fmt.Errorf("left %s: it no longer names the staging directory", s.name), err, closeErr)
	}
	if err := root.Remove(s.name); err != nil {
		return errors.Join(fmt.Errorf("left %s", s.name), err, closeErr)
	}
	return closeErr
}

// keepUserFiles copies into the staged gen/ each top-level regular file of
// the module's gen/ that is not generated.
func (s *genStage) keepUserFiles(root *os.Root, generated map[string][]byte) error {
	dir, err := root.Open("gen")
	if err != nil {
		return fmt.Errorf("read gen/: %w", err)
	}
	entries, readErr := dir.ReadDir(-1)
	if err := errors.Join(readErr, dir.Close()); err != nil {
		return fmt.Errorf("read gen/: %w", err)
	}
	for _, e := range entries {
		p := "gen/" + e.Name()
		if !e.Type().IsRegular() {
			continue
		}
		if _, ok := generated[p]; ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return fmt.Errorf("keep %s: %w", p, err)
		}
		content, err := root.ReadFile(p)
		if err != nil {
			return fmt.Errorf("keep %s: %w", p, err)
		}
		// The copy keeps the file's permission bits, as goa gen, which leaves
		// the file in place, would.
		if err := writeSynced(s.dir, p, content, info.Mode().Perm()); err != nil {
			return fmt.Errorf("keep %s: %w", p, err)
		}
	}
	return nil
}

// currentGenDir reports whether root holds a gen directory. A gen that is not
// a directory, such as a symbolic link, is an error.
func currentGenDir(root *os.Root) (bool, error) {
	info, err := root.Lstat("gen")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("inspect gen/: %w", err)
	case !info.IsDir():
		return false, fmt.Errorf("gen is not a directory (%s); move it away and run surla gen again", info.Mode().Type())
	}
	return true, nil
}

// syncDir syncs the directory name inside root.
func syncDir(root *os.Root, name string) error {
	d, err := root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// treeDigests returns the SHA-256 of every regular file under gen/ in root,
// keyed by its slash-separated path. A missing gen/ is an empty tree.
func treeDigests(root *os.Root) (map[string][sha256.Size]byte, error) {
	out := map[string][sha256.Size]byte{}
	if _, err := root.Lstat("gen"); errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	err := fs.WalkDir(root.FS(), "gen", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.Type().IsRegular() {
			return nil
		}
		content, err := root.ReadFile(p)
		if err != nil {
			return err
		}
		out[p] = sha256.Sum256(content)
		return nil
	})
	return out, err
}

// moduleRequirements returns the module path and required module paths of a
// go.mod; an unparsable go.mod yields none.
func moduleRequirements(gomod string) (string, []string) {
	f, err := modfile.ParseLax("go.mod", []byte(gomod), nil)
	if err != nil || f.Module == nil {
		return "", nil
	}
	requires := make([]string, 0, len(f.Require))
	for _, r := range f.Require {
		requires = append(requires, r.Mod.Path)
	}
	return f.Module.Mod.Path, requires
}

// goImports returns the import paths of one Go source file; a file that does
// not parse yields none.
func goImports(src []byte) []string {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.ImportsOnly)
	if err != nil {
		return nil
	}
	imports := make([]string, 0, len(f.Imports))
	for _, spec := range f.Imports {
		if p, err := strconv.Unquote(spec.Path.Value); err == nil {
			imports = append(imports, p)
		}
	}
	return imports
}
