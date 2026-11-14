package genpublish

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
)

// genStage is the .surla-gen-<random> directory Publish creates in the module
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

// stagePrefix starts the name of every staging directory.
const stagePrefix = ".surla-gen-"

// newStage creates a new staging directory in root under a random name and
// acquires it with acquireStage.
func newStage(root *os.Root) (*genStage, error) {
	name := stagePrefix + strings.ToLower(rand.Text())
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

// writeGuide writes the agent guide at the module root through a temporary
// file renamed over the old one, both inside root.
func writeGuide(root *os.Root, content []byte) error {
	tmp := "." + quickstartGuide + ".surla-" + strings.ToLower(rand.Text())
	if err := writeSynced(root, tmp, content, 0o644); err != nil {
		return errors.Join(err, removeIfPresent(root, tmp))
	}
	if err := root.Rename(tmp, quickstartGuide); err != nil {
		return errors.Join(err, removeIfPresent(root, tmp))
	}
	return nil
}

// writeSynced creates name inside root, which must not exist, with exactly
// the permission bits perm (whatever the umask), writes content, syncs it and
// closes it.
func writeSynced(root *os.Root, name string, content []byte, perm os.FileMode) error {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if err := f.Chmod(perm); err != nil {
		return errors.Join(err, f.Close())
	}
	if _, err := f.Write(content); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

// removeIfPresent removes name inside root, treating a missing name as done.
func removeIfPresent(root *os.Root, name string) error {
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// leftoverStages returns the names of .surla-gen-* entries in the module
// root, sorted. A directory that cannot be read yields none.
func leftoverStages(root *os.Root) []string {
	dir, err := root.Open(".")
	if err != nil {
		return nil
	}
	names, err := dir.Readdirnames(-1)
	if err := errors.Join(err, dir.Close()); err != nil {
		return nil
	}
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, stagePrefix) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// openLeftover opens the entry name of the module root as a staging directory
// an earlier publication left, when it is one. inspected is what Lstat gave
// for name just before. The name proves nothing, since anyone who writes the
// module can create it, so the entry counts as a staging directory only when
// it has the shape Publish gives one:
//
//   - it is a real directory, not a symbolic link or a file;
//   - the directory opened is the one inspected, so a link swapped in between
//     the two leads nowhere;
//   - it holds nothing, or only a real directory gen.
//
// Anything else is the module author's and yields no stage and no error; it
// is neither opened further nor changed. An entry that changed while it was
// opened, or that cannot be read, is an error and is left alone too. The
// returned stage holds the directory open, so cleanup acts on that directory
// whatever its name is made to point at afterwards.
func openLeftover(root *os.Root, name string, inspected fs.FileInfo) (*genStage, error) {
	if !inspected.IsDir() {
		return nil, nil
	}
	dir, err := root.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("left %s: %w", name, err)
	}
	leave := func(reason error) (*genStage, error) {
		if reason != nil {
			reason = fmt.Errorf("left %s: %w", name, reason)
		}
		return nil, errors.Join(reason, dir.Close())
	}
	held, err := dir.Stat(".")
	if err != nil {
		return leave(err)
	}
	if !os.SameFile(inspected, held) {
		return leave(errors.New("it changed while it was opened"))
	}
	listing, err := dir.Open(".")
	if err != nil {
		return leave(err)
	}
	entries, err := listing.Readdirnames(2)
	if err := errors.Join(listingErr(err), listing.Close()); err != nil {
		return leave(err)
	}
	for _, entry := range entries {
		if entry != "gen" {
			return leave(nil)
		}
	}
	if len(entries) > 0 {
		staged, err := dir.Lstat("gen")
		if err != nil {
			return leave(err)
		}
		if !staged.IsDir() {
			return leave(nil)
		}
	}
	return &genStage{name: name, dir: dir}, nil
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
		if p == quickstartGuide {
			continue
		}
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
// (renameat.Exchange), or moves it in when there is none (renameat.NoReplace,
// which fails if gen/ appeared meanwhile). Afterwards the staging directory
// holds the old tree, if there was one. A true result means publication
// succeeded; any accompanying error only reports handles that could not be
// closed. A false result means the exchange did not happen and its error is a
// failure.
func (s *genStage) exchange(root *os.Root) (bool, error) {
	current, err := currentGenDir(root)
	if err != nil {
		return false, err
	}
	module, err := root.Open(".")
	if err != nil {
		return false, fmt.Errorf("open module directory: %w", err)
	}
	staged, err := s.dir.Open(".")
	if err != nil {
		return false, errors.Join(fmt.Errorf("open staging directory: %w", err), module.Close())
	}
	var exchangeErr error
	if current {
		exchangeErr = exchangeNames(int(staged.Fd()), "gen", int(module.Fd()), "gen")
	} else {
		exchangeErr = noReplace(int(staged.Fd()), "gen", int(module.Fd()), "gen")
	}
	closeErr := errors.Join(staged.Close(), module.Close())
	if exchangeErr != nil {
		return false, errors.Join(fmt.Errorf("replace gen/: %w", exchangeErr), closeErr)
	}
	// The exchange happened; a failure to close the handles does not undo it.
	return true, closeErr
}

// report records the files added, changed and removed by comparing the old
// tree, now in the staging directory, with the new one. A partial listing
// returns the changes observed so far and the read error.
func (s *genStage) report(root *os.Root, result *Result) error {
	before, beforeErr := treeDigests(s.dir)
	after, afterErr := treeDigests(root)
	for p, d := range after {
		old, ok := before[p]
		switch {
		case !ok:
			result.Added = append(result.Added, p)
		case old != d:
			result.Changed = append(result.Changed, p)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			result.Removed = append(result.Removed, p)
		}
	}
	slices.Sort(result.Added)
	slices.Sort(result.Changed)
	slices.Sort(result.Removed)
	return errors.Join(beforeErr, afterErr)
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
