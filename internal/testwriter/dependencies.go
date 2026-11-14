// This file decides whose failure it is when the test module's dependencies
// cannot be downloaded: the author's, who gets a finding, or one outside the
// author's changes, which is ErrDependencies and the user's to clear.
//
// The download runs outside the sandbox with the user's proxy settings, so
// its output is withheld and cannot say why it failed. What Surla can know
// without it is whether the failure follows the author's changes. A download
// reads only the module's go.mod and go.sum, so:
//
//   - when both are the ones of the copy's base, the project's own lists fail,
//     and the author changed nothing that could explain it;
//   - when the author changed them, the same download is run on the base's
//     lists. If those fail too, downloading does not work where Surla runs,
//     whatever the author changed. If they download, the author's change is
//     what cannot be downloaded, and it stays the author's finding.
//
// The base's lists come from the project's commit, not from the copy, so the
// author cannot make that comparison fail and hide a defect of its own lists
// behind it: while downloading works, a defect in the author's lists is
// always a finding.
//
// What this cannot tell apart: a module the author added that exists but
// cannot be reached from where Surla runs (no network while everything the
// base lists is already in the module cache, or a private module the server
// has no access to) reads the same as one that does not exist. Both are the
// author's finding, whose text says to tell the user when the download works
// in the copy.
package testwriter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/goadesign/surla/internal/designeval"
)

// dependencyLists is the content of a module's go.mod and go.sum, the two
// files a download of its dependencies reads, each without its final newline.
type dependencyLists struct {
	// mod is go.mod's content.
	mod string
	// sum is go.sum's content, empty when hasSum is false.
	sum string
	// hasSum is whether the module has a go.sum.
	hasSum bool
}

// maxListBytes bounds a go.mod or go.sum read to decide a download failure,
// as maxGitStdout bounds the same file read from the base commit.
const maxListBytes = maxGitStdout

// dependencyFailure decides whose failure a failed download of the
// dependencies listed by current is. current is the go.mod and go.sum of the
// copy's test module at modulePath, a slash path below the copy's root, and
// goBin the go command that failed to download them.
//
// It returns an error wrapping ErrDependencies when the cause is outside the
// author's changes: current is the base's own lists, or the base's lists
// cannot be downloaded either. Otherwise the failure is the author's and the
// error is nil: comparedWith is the base commit when that commit's lists
// download and only the author's do not, and empty when no comparison could
// be made, because the base has no such module, or its go.mod cannot be
// parsed or replaces a module with a local folder, which a download outside
// the copy cannot follow. Any other error is a failure to decide: git, the
// file system, or ctx ended.
func (c *Checker) dependencyFailure(
	ctx context.Context, wc WorkingCopy, goBin, modulePath string, current dependencyLists,
) (comparedWith string, err error) {
	base, err := copyBase(ctx, wc)
	if err != nil {
		return "", err
	}
	based, found, err := baseDependencyLists(ctx, wc, base, modulePath)
	if err != nil {
		return "", err
	}
	if !found {
		return "", nil
	}
	if current == based {
		return "", fmt.Errorf("%w, and its go.mod and go.sum are those of the project's commit %s, "+
			"so the cause is not in the test writer's changes", ErrDependencies, base)
	}
	parsed, err := modfile.Parse("go.mod", []byte(based.mod), nil)
	if err != nil {
		return "", nil
	}
	for _, r := range parsed.Replace {
		if r.New.Version == "" {
			return "", nil
		}
	}
	downloadErr, err := c.downloadLists(ctx, goBin, based)
	switch {
	case err != nil:
		return "", err
	case ctx.Err() != nil:
		return "", fmt.Errorf("download the dependencies the copy's base lists: %w", ctx.Err())
	case downloadErr != nil:
		return "", fmt.Errorf("%w, and neither could those the project's commit %s lists, "+
			"so downloading does not work there whatever the test writer changed", ErrDependencies, base)
	}
	return base, nil
}

// loadFailure decides whether a generation that failed while loading the
// dependencies of the module at modulePath, whose snapshot is files, failed
// for a cause outside the author's changes. It downloads the dependencies the
// snapshot's go.mod and go.sum list: when they download, the load failed on
// the module's own files and the result is nil. When they do not, the result
// is dependencyFailure's: an error wrapping ErrDependencies for a cause
// outside the author's changes, nil for the author's, or a failure to decide.
func (c *Checker) loadFailure(
	ctx context.Context, wc WorkingCopy, modulePath string, files map[string]string,
) error {
	if err := os.MkdirAll(c.workDir, 0o700); err != nil {
		return fmt.Errorf("create the check folder: %w", err)
	}
	tc, err := resolveToolchain(ctx, c.workDir)
	if err != nil {
		return err
	}
	sum, hasSum := files["go.sum"]
	current := dependencyLists{
		mod: strings.TrimSuffix(files["go.mod"], "\n"), sum: strings.TrimSuffix(sum, "\n"), hasSum: hasSum,
	}
	downloadErr, err := c.downloadLists(ctx, tc.goBin(), current)
	switch {
	case err != nil:
		return err
	case ctx.Err() != nil:
		return fmt.Errorf("download the suite module's dependencies: %w", ctx.Err())
	case downloadErr == nil:
		return nil
	}
	_, err = c.dependencyFailure(ctx, wc, tc.goBin(), modulePath, current)
	return err
}

// downloadLists downloads the dependencies lists names, with goBin, in a new
// module folder of the Checker's own that holds only those two files and is
// removed afterwards, so the download reads nothing else of a copy. downloaded
// is the download's own result, and err a failure to set the folder up or to
// remove it.
func (c *Checker) downloadLists(
	ctx context.Context, goBin string, lists dependencyLists,
) (downloaded, err error) {
	if err := os.MkdirAll(c.workDir, 0o700); err != nil {
		return nil, fmt.Errorf("create the check folder: %w", err)
	}
	dir, err := os.MkdirTemp(c.workDir, "lists-*")
	if err != nil {
		return nil, fmt.Errorf("create the check folder: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(lists.mod+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write the lists to download: %w", err)
	}
	if lists.hasSum {
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte(lists.sum+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("write the lists to download: %w", err)
		}
	}
	return c.download(ctx, goBin, dir), nil
}

// baseDependencyLists returns the go.mod and go.sum that the module at the
// slash path modulePath has in the commit base, read through the project's
// repository. found is false when base has no go.mod there.
func baseDependencyLists(
	ctx context.Context, wc WorkingCopy, base, modulePath string,
) (lists dependencyLists, found bool, err error) {
	goMod, goSum := modulePath+"/go.mod", modulePath+"/go.sum"
	listed, err := wc.git(ctx, wc.dir, nil, "ls-tree", "-z", "--name-only", base, "--", goMod, goSum)
	if err != nil {
		return dependencyLists{}, false, fmt.Errorf("read the base's dependency lists: %w", err)
	}
	names := strings.Split(strings.TrimSuffix(listed, "\x00"), "\x00")
	// read returns the content of the file name in base, when base has it.
	read := func(name string) (string, bool, error) {
		if !slices.Contains(names, name) {
			return "", false, nil
		}
		content, err := wc.git(ctx, wc.dir, nil, "cat-file", "blob", base+":"+name)
		if err != nil {
			return "", false, fmt.Errorf("read the base's dependency lists: %w", err)
		}
		return content, true, nil
	}
	if lists.mod, found, err = read(goMod); err != nil || !found {
		return dependencyLists{}, false, err
	}
	if lists.sum, lists.hasSum, err = read(goSum); err != nil {
		return dependencyLists{}, false, err
	}
	return lists, true, nil
}

// snapshotDependencyLists reads the go.mod and go.sum of the module at
// moduleDir in a check's snapshot. The snapshot holds the author's files, so
// each is read as a regular file through the module's folder and never
// through a link.
func snapshotDependencyLists(moduleDir string) (lists dependencyLists, err error) {
	root, err := os.OpenRoot(moduleDir)
	if err != nil {
		return dependencyLists{}, fmt.Errorf("read the suite module's dependency lists: %w", err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	mod, err := designeval.ReadRegularFile(root, "go.mod", maxListBytes)
	if err != nil {
		return dependencyLists{}, fmt.Errorf("read the suite module's dependency lists: %w", err)
	}
	lists.mod = strings.TrimSuffix(string(mod), "\n")
	sum, err := designeval.ReadRegularFile(root, "go.sum", maxListBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return dependencyLists{}, fmt.Errorf("read the suite module's dependency lists: %w", err)
	default:
		lists.sum, lists.hasSum = strings.TrimSuffix(string(sum), "\n"), true
	}
	return lists, nil
}
