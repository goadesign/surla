// This file implements one-time snapshot preparation of legacy workspace
// verification runs and reports for `surla serve -import-legacy-workspace`,
// and resolves configured report directories to their published imported
// subdirectories.
package verifyrun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/goadesign/surla/internal/abspath"
)

type (
	// importedWorkspacePaths records the original workspace report file paths
	// imported into <imported>/reports/ under <imported>/runs/imported_paths.json.
	importedWorkspacePaths struct {
		// Version is the schema version of imported_paths.json (always 1).
		Version int `json:"version"`
		// ReportFiles lists the sorted unique physical workspace file paths of
		// reports copied during legacy workspace import.
		ReportFiles []string `json:"report_files,omitempty"`
	}
)

const (
	// importedPathsFile names the record written under <imported>/runs/ by
	// PrepareSnapshot.
	importedPathsFile = "imported_paths.json"
	// defaultImportedReportSubdir is the subdirectory under <imported>/reports/
	// that holds reports imported from <initialWorkspaceRoot>/reports and any
	// subsequent starter verification reports.
	defaultImportedReportSubdir = "default"
)

// PrepareSnapshot stages a verified copy of legacyRunsDir,
// legacyDefaultReportsDir, and any workspace-interior configuredReportDirs into
// stageRunsDir and stageReportsRoot before atomic publication of
// <operatorProjectDir>/imported. It verifies that no legacy run directory holds
// an active lock (acquiring LOCK_SH|LOCK_NB on every existing legacy run lock
// and returning releaseLocks so the caller keeps them held until RenameatxNp
// completes), omits unaccepted run folders where request.json is absent,
// preserves lock absence (and original lock bytes when present) for accepted
// run records, copies default, configured, and run-referenced workspace report
// files verbatim into distinct subdirectories under stageReportsRoot, copies
// completed, interrupted (unlocked without result.json), and corrupt published
// run records without modifying legacy originals, rewrites workspace-interior
// Plan.ReportDir paths to their published imported locations, and writes
// <stageRunsDir>/imported_paths.json.
func PrepareSnapshot(
	legacyRunsDir, legacyDefaultReportsDir, stageRunsDir, stageReportsRoot, publishedReportsRoot, initialWorkspaceRoot string,
	configuredReportDirs ...string,
) (releaseLocks func() error, err error) {
	wsRoot, err := abspath.EvalMissing(initialWorkspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root %s: %w", initialWorkspaceRoot, err)
	}
	if err := os.MkdirAll(stageRunsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create staged runs directory: %w", err)
	}
	if err := os.MkdirAll(stageReportsRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create staged reports root: %w", err)
	}

	var heldLocks []*os.File
	closeHeldLocks := func() error {
		var closeErr error
		for _, f := range heldLocks {
			closeErr = errors.Join(closeErr, f.Close())
		}
		heldLocks = nil
		return closeErr
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, closeHeldLocks())
		}
	}()

	var recordedReportFiles []string
	copiedReportDirs := make(map[string]bool)
	if legacyDefaultReportsDir != "" {
		copiedReportDirs[defaultImportedReportSubdir] = true
		defaultStageSubdir := filepath.Join(stageReportsRoot, defaultImportedReportSubdir)
		if err := copyLegacyReportDir(wsRoot, legacyDefaultReportsDir, defaultStageSubdir, &recordedReportFiles); err != nil {
			return nil, err
		}
	}
	for _, cfgReportDir := range configuredReportDirs {
		if cfgReportDir == "" {
			continue
		}
		cleanReportDir, insideWS, err := resolveWorkspaceReportPath(wsRoot, cfgReportDir)
		if err != nil {
			return nil, fmt.Errorf("resolve configured report_dir %s: %w", cfgReportDir, err)
		}
		if !insideWS {
			continue
		}
		dirKey := importedReportDirKey(wsRoot, cleanReportDir)
		if !copiedReportDirs[dirKey] {
			copiedReportDirs[dirKey] = true
			if err := copyLegacyReportDir(
				wsRoot,
				cleanReportDir,
				filepath.Join(stageReportsRoot, dirKey),
				&recordedReportFiles,
			); err != nil {
				return nil, err
			}
		}
	}

	runEntries, err := readNonSymlinkDir(legacyRunsDir)
	if err != nil {
		return nil, fmt.Errorf("read legacy runs directory %s: %w", legacyRunsDir, err)
	}
	for _, entry := range runEntries {
		name := entry.Name()
		if !runIDPattern.MatchString(name) {
			continue
		}
		srcRunDir := filepath.Join(legacyRunsDir, name)
		info, err := os.Lstat(srcRunDir)
		if err != nil {
			return nil, fmt.Errorf("stat legacy run %s: %w", srcRunDir, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("legacy run entry %s is not a regular directory", srcRunDir)
		}
		lockPath := filepath.Join(srcRunDir, lockFile)
		var (
			hasLock   bool
			lockBytes []byte
		)
		if lockF, rawLock, lockErr := openSharedNonblockingLock(lockPath); lockErr == nil {
			hasLock = true
			lockBytes = rawLock
			heldLocks = append(heldLocks, lockF)
		} else if !errors.Is(lockErr, fs.ErrNotExist) {
			if errors.Is(lockErr, syscall.EWOULDBLOCK) {
				return nil, fmt.Errorf(
					"legacy verification run %s has an active writer; wait for or stop all legacy verification runs before running -import-legacy-workspace",
					name,
				)
			}
			return nil, fmt.Errorf("lock legacy verification run %s: %w", name, lockErr)
		}

		reqBytes, hasRequest, err := readOptionalRegularFileNoFollow(filepath.Join(srcRunDir, requestFile))
		if err != nil {
			return nil, fmt.Errorf("read legacy run request %s: %w", name, err)
		}
		if !hasRequest {
			// A run folder without request.json was never accepted/published;
			// skip it to match verifyrun.List while leaving the original folder untouched.
			continue
		}
		resBytes, hasResult, err := readOptionalRegularFileNoFollow(filepath.Join(srcRunDir, resultFile))
		if err != nil {
			return nil, fmt.Errorf("read legacy run result %s: %w", name, err)
		}

		req, reqErr := decodeRequest(name, reqBytes)
		if reqErr == nil && req.Plan.ReportDir != "" {
			cleanReportDir, insideWS, err := resolveWorkspaceReportPath(wsRoot, req.Plan.ReportDir)
			if err != nil {
				return nil, fmt.Errorf("resolve legacy run %s report_dir: %w", name, err)
			}
			if insideWS {
				dirKey := importedReportDirKey(wsRoot, cleanReportDir)
				if !copiedReportDirs[dirKey] {
					copiedReportDirs[dirKey] = true
					if err := copyLegacyReportDir(
						wsRoot,
						cleanReportDir,
						filepath.Join(stageReportsRoot, dirKey),
						&recordedReportFiles,
					); err != nil {
						return nil, err
					}
				}
				req.Plan.ReportDir = filepath.Join(publishedReportsRoot, dirKey)
				rewritten, err := json.Marshal(req)
				if err != nil {
					return nil, fmt.Errorf("encode rewritten run request %s: %w", name, err)
				}
				reqBytes = rewritten
			}
		}

		dstRunDir := filepath.Join(stageRunsDir, name)
		if err := os.Mkdir(dstRunDir, 0o700); err != nil {
			return nil, fmt.Errorf("create staged run directory %s: %w", name, err)
		}
		if hasLock {
			if err := writeRegularFileNoFollow(filepath.Join(dstRunDir, lockFile), lockBytes, 0o600); err != nil {
				return nil, fmt.Errorf("write staged run lock %s: %w", name, err)
			}
		}
		if err := writeRegularFileNoFollow(filepath.Join(dstRunDir, requestFile), reqBytes, 0o600); err != nil {
			return nil, fmt.Errorf("write staged run request %s: %w", name, err)
		}
		if hasResult {
			if err := writeRegularFileNoFollow(filepath.Join(dstRunDir, resultFile), resBytes, 0o600); err != nil {
				return nil, fmt.Errorf("write staged run result %s: %w", name, err)
			}
		}
		if err := syncDir(dstRunDir); err != nil {
			return nil, fmt.Errorf("sync staged run directory %s: %w", name, err)
		}
	}

	slices.Sort(recordedReportFiles)
	recordedReportFiles = slices.Compact(recordedReportFiles)
	pathsRaw, err := json.Marshal(importedWorkspacePaths{
		Version:     1,
		ReportFiles: recordedReportFiles,
	})
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", importedPathsFile, err)
	}
	if err := writeRegularFileNoFollow(filepath.Join(stageRunsDir, importedPathsFile), pathsRaw, 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", importedPathsFile, err)
	}
	if err := syncDir(stageRunsDir); err != nil {
		return nil, fmt.Errorf("sync staged runs directory %s: %w", stageRunsDir, err)
	}
	if err := syncDir(stageReportsRoot); err != nil {
		return nil, fmt.Errorf("sync staged reports directory %s: %w", stageReportsRoot, err)
	}
	return closeHeldLocks, nil
}

// IsWorkspaceReportDir reports whether configuredReportDir resolves inside
// initialWorkspaceRoot without following symlinks inside initialWorkspaceRoot.
func IsWorkspaceReportDir(initialWorkspaceRoot, configuredReportDir string) (bool, error) {
	if initialWorkspaceRoot == "" || configuredReportDir == "" {
		return false, nil
	}
	wsRoot, err := abspath.EvalMissing(initialWorkspaceRoot)
	if err != nil {
		return false, err
	}
	_, insideWS, err := resolveWorkspaceReportPath(wsRoot, configuredReportDir)
	return insideWS, err
}

// ResolveImportedReportDir maps a configured workspace-interior report
// directory to its published counterpart under publishedReportsRoot when that
// imported subdirectory exists as a non-symlink directory. Ancestors outside
// initialWorkspaceRoot resolve symlinks left-to-right before applying "..",
// while path components inside initialWorkspaceRoot are resolved without
// following symlinks so post-import workspace edits or symlinks cannot redirect
// an imported report directory. Non-workspace paths and workspace paths without
// a published imported subdirectory (fs.ErrNotExist) are returned unchanged so
// normal validation can accept external paths and reject unimported workspace
// paths; filesystem errors or non-directory entries under publishedReportsRoot
// return an error.
func ResolveImportedReportDir(publishedReportsRoot, initialWorkspaceRoot, configuredReportDir string) (string, error) {
	if publishedReportsRoot == "" || initialWorkspaceRoot == "" || configuredReportDir == "" {
		return configuredReportDir, nil
	}
	wsRoot, err := abspath.EvalMissing(initialWorkspaceRoot)
	if err != nil {
		return "", err
	}
	cleanConfigured, insideWS, err := resolveWorkspaceReportPath(wsRoot, configuredReportDir)
	if err != nil {
		return "", err
	}
	if !insideWS {
		return configuredReportDir, nil
	}
	dirKey := importedReportDirKey(wsRoot, cleanConfigured)
	candidate := filepath.Join(publishedReportsRoot, dirKey)
	info, err := os.Lstat(candidate)
	if errors.Is(err, fs.ErrNotExist) {
		return configuredReportDir, nil
	}
	if err != nil {
		return "", fmt.Errorf("stat imported report directory %s: %w", candidate, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("imported report directory %s is not a regular directory", candidate)
	}
	return candidate, nil
}

// resolveWorkspaceReportPath resolves configuredReportDir left-to-right,
// expanding symlinks outside wsRoot before applying ".." while refusing to
// follow symlinks once inside wsRoot so builder changes inside the workspace
// after import cannot redirect imported report lookup.
func resolveWorkspaceReportPath(wsRoot, configuredReportDir string) (string, bool, error) {
	var (
		cur  string
		rest string
	)
	if filepath.IsAbs(configuredReportDir) {
		cur = string(filepath.Separator)
		rest = strings.TrimPrefix(configuredReportDir, string(filepath.Separator))
	} else {
		wd, err := os.Getwd()
		if err != nil {
			return "", false, err
		}
		cur, err = abspath.Eval(wd)
		if err != nil {
			return "", false, err
		}
		rest = configuredReportDir
	}

	const maxSymlinkExpansions = 40
	links := 0
	for rest != "" {
		part, after, _ := strings.Cut(rest, string(filepath.Separator))
		rest = after
		switch part {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, part)
		if wsRoot != "" && isWithinOrEqualPath(cur, wsRoot) {
			cur = next
			continue
		}
		info, err := os.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) {
			cur = next
			continue
		}
		if err != nil {
			return "", false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > maxSymlinkExpansions {
				return "", false, fmt.Errorf("resolve %s: too many symlinks", configuredReportDir)
			}
			target, err := os.Readlink(next)
			if err != nil {
				return "", false, err
			}
			if rest != "" {
				target = target + string(filepath.Separator) + rest
			}
			if filepath.IsAbs(target) {
				cur = string(filepath.Separator)
				rest = strings.TrimPrefix(target, string(filepath.Separator))
			} else {
				rest = target
			}
			continue
		}
		cur = next
	}
	cleanConfigured := filepath.Clean(cur)
	return cleanConfigured, isWithinOrEqualPath(cleanConfigured, wsRoot), nil
}

// importedReportDirKey returns "default" for <initialWorkspaceRoot>/reports and
// "dir-<sha256>" for any other workspace-interior report directory.
func importedReportDirKey(wsRoot, reportDir string) string {
	if filepath.Clean(reportDir) == filepath.Join(filepath.Clean(wsRoot), "reports") {
		return defaultImportedReportSubdir
	}
	sum := sha256.Sum256([]byte(filepath.Clean(reportDir)))
	return "dir-" + hex.EncodeToString(sum[:])
}

// copyLegacyReportDir copies every <64-hex>.json report file in origReportDir
// verbatim without JSON decoding into stageSubdir and appends each copied
// source path to *recordedReportFiles.
func copyLegacyReportDir(
	wsRoot, origReportDir, stageSubdir string,
	recordedReportFiles *[]string,
) error {
	if err := os.MkdirAll(stageSubdir, 0o700); err != nil {
		return fmt.Errorf("create staged report directory %s: %w", stageSubdir, err)
	}
	if origReportDir == "" {
		return syncDir(stageSubdir)
	}
	cleanOrig, insideWS, err := resolveWorkspaceReportPath(wsRoot, origReportDir)
	if err != nil {
		return fmt.Errorf("resolve legacy report directory %s: %w", origReportDir, err)
	}
	if insideWS && wsRoot != "" {
		if err := ensureNoSymlinksUnderRoot(wsRoot, cleanOrig); err != nil {
			return err
		}
	}
	entries, err := readNonSymlinkDir(cleanOrig)
	if err != nil {
		return fmt.Errorf("read legacy report directory %s: %w", cleanOrig, err)
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if isHexReportFilename(name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		srcPath := filepath.Join(cleanOrig, name)
		raw, err := readRegularFileNoFollow(srcPath)
		if err != nil {
			return fmt.Errorf("read legacy report %s: %w", srcPath, err)
		}
		dstPath := filepath.Join(stageSubdir, name)
		if err := writeRegularFileNoFollow(dstPath, raw, 0o400); err != nil {
			return fmt.Errorf("write staged report %s: %w", dstPath, err)
		}
		*recordedReportFiles = append(*recordedReportFiles, srcPath)
	}
	return syncDir(stageSubdir)
}

// ensureNoSymlinksUnderRoot verifies that every path component from wsRoot down
// to targetDir is either missing or a non-symlink directory.
func ensureNoSymlinksUnderRoot(wsRoot, targetDir string) error {
	rel, err := filepath.Rel(wsRoot, targetDir)
	if err != nil || rel == "." {
		return err
	}
	cur := wsRoot
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("legacy report directory %s is not a regular directory", cur)
		}
	}
	return nil
}

// isHexReportFilename reports whether name is <64 lowercase hex>.json.
func isHexReportFilename(name string) bool {
	return strings.HasSuffix(name, ".json") && digestPattern.MatchString(strings.TrimSuffix(name, ".json"))
}

// readNonSymlinkDir reads dir entries after verifying dir is not a symlink. A
// missing dir returns nil, nil.
func readNonSymlinkDir(dir string) ([]os.DirEntry, error) {
	if dir == "" {
		return nil, nil
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("%s is not a regular directory", dir)
	}
	return os.ReadDir(dir)
}

// openSharedNonblockingLock opens path without following symlinks, acquires a
// non-blocking shared flock lock, and reads its contents while keeping the
// descriptor open.
func openSharedNonblockingLock(path string) (*os.File, []byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", path)
	}
	if err == nil {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	}
	var raw []byte
	if err == nil {
		raw, err = io.ReadAll(f)
	}
	if err != nil {
		return nil, nil, errors.Join(err, f.Close())
	}
	return f, raw, nil
}

// readRegularFileNoFollow reads path without following symlinks and requires a
// regular file.
func readRegularFileNoFollow(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", path)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	raw, readErr := io.ReadAll(f)
	return raw, errors.Join(readErr, f.Close())
}

// readOptionalRegularFileNoFollow reads path if it exists, returning
// (nil, false, nil) when path does not exist.
func readOptionalRegularFileNoFollow(path string) ([]byte, bool, error) {
	raw, err := readRegularFileNoFollow(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

// writeRegularFileNoFollow creates path exclusively without following symlinks,
// writes raw, syncs the file, sets perm, and closes it.
func writeRegularFileNoFollow(path string, raw []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		return errors.Join(err, f.Close())
	}
	if perm != 0o600 {
		if err := f.Chmod(perm); err != nil {
			return errors.Join(err, f.Close())
		}
	}
	return errors.Join(f.Sync(), f.Close())
}

// isWithinOrEqualPath reports whether target equals or lies inside root.
func isWithinOrEqualPath(target, root string) bool {
	if target == "" || root == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// syncDir flushes dir's directory entries so staged files and directories
// survive a crash after atomic rename.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
