// This file owns the control plane's bound filesystem workspace, inode-verified
// startup recovery (`<cards_dir>/workspace.json`), `os.Root` design/module
// snapshot readers, and workspace-separation checks.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"golang.org/x/sys/unix"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/verify"
)

type (
	// workspaceFile is the persisted JSON schema of `<cards_dir>/workspace.json`.
	// During `init_project`, the uncommitted write-ahead record sets `StageDir`,
	// `Staging`, `Dev`, `Ino`, and `SuiteCommit` before `RenameatxNp`; the
	// finalized record clears `StageDir` and `Staging` once the renamed target
	// directory's `(Dev, Ino)` is verified and synced.
	workspaceFile struct {
		// Dir is the direct child directory name inside initialWorkspaceRoot.
		Dir string `json:"dir"`
		// SuiteCommit is the trusted initial starter commit recorded for Dir.
		SuiteCommit string `json:"suite_commit"`
		// StageDir is the protected operator staging parent directory holding
		// Staging while initialization is in flight.
		StageDir string `json:"stage_dir,omitempty"`
		// Staging is the `.surla-init-*` directory name inside StageDir while
		// initialization is in flight.
		Staging string `json:"staging,omitempty"`
		// Dev is the filesystem device ID of the staged/published project root.
		Dev uint64 `json:"dev,omitempty"`
		// Ino is the inode number of the staged/published project root.
		Ino uint64 `json:"ino,omitempty"`
	}
)

const (
	// workspaceRecordFile names the write-ahead and bound-workspace record inside
	// the control plane's cards directory.
	workspaceRecordFile = "workspace.json"
	// workspaceStagingWriteFile names the temporary file written before atomic
	// rename over workspaceRecordFile.
	workspaceStagingWriteFile = "workspace.json.tmp"
)

// commitIDPattern matches a full 40- or 64-character lowercase hex Git commit ID.
var commitIDPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// openWorkspaceRootAndStat resolves dir to a physical non-symlink directory,
// opens an os.Root handle to verify its open-descriptor identity, and returns
// its cleaned physical path, device ID, and inode number.
func openWorkspaceRootAndStat(dir string) (string, uint64, uint64, error) {
	if dir == "" {
		return "", 0, 0, nil
	}
	lstatInfo, err := os.Lstat(dir)
	if err != nil {
		return "", 0, 0, fmt.Errorf("stat workspace %s: %w", dir, err)
	}
	if lstatInfo.Mode()&os.ModeSymlink != 0 {
		return "", 0, 0, fmt.Errorf("workspace %s must not be a symbolic link", dir)
	}
	if !lstatInfo.IsDir() {
		return "", 0, 0, fmt.Errorf("workspace %s is not a directory", dir)
	}
	// The stored spelling is the one the kernel reports for the open
	// workspace later, so expected-path checks compare like with like.
	cleanDir, err := abspath.Stored(dir)
	if err != nil {
		return "", 0, 0, fmt.Errorf("resolve workspace %s: %w", dir, err)
	}
	root, err := os.OpenRoot(cleanDir)
	if err != nil {
		return "", 0, 0, fmt.Errorf("open workspace %s: %w", cleanDir, err)
	}
	rootInfo, statErr := root.Stat(".")
	closeErr := root.Close()
	if err := errors.Join(statErr, closeErr); err != nil {
		return "", 0, 0, fmt.Errorf("stat opened workspace %s: %w", cleanDir, err)
	}
	st, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return "", 0, 0, fmt.Errorf("workspace %s did not report unix stat identity", cleanDir)
	}
	if uint64(st.Dev) == 0 || st.Ino == 0 {
		return "", 0, 0, fmt.Errorf("workspace %s reported zero device/inode identity", cleanDir)
	}
	return cleanDir, uint64(st.Dev), st.Ino, nil
}

// currentWorkspaceDir returns the currently bound workspace directory path.
func (c *ControlPlane) currentWorkspaceDir() string {
	c.workspaceMu.RLock()
	defer c.workspaceMu.RUnlock()
	return c.activeWorkspaceDir
}

// currentWorkspaceTarget returns the currently bound workspace directory path
// alongside its expected `(dev, ino)` directory identity.
func (c *ControlPlane) currentWorkspaceTarget() (string, uint64, uint64) {
	c.workspaceMu.RLock()
	defer c.workspaceMu.RUnlock()
	return c.activeWorkspaceDir, c.boundWorkspaceDev, c.boundWorkspaceIno
}

// currentWorkspaceVerificationTarget returns the currently bound workspace
// directory path, its expected `(dev, ino)` identity, and the trusted starter
// suite commit recorded by init_project or restored from workspace.json.
func (c *ControlPlane) currentWorkspaceVerificationTarget() (string, uint64, uint64, string) {
	c.workspaceMu.RLock()
	defer c.workspaceMu.RUnlock()
	return c.activeWorkspaceDir, c.boundWorkspaceDev, c.boundWorkspaceIno, c.boundSuiteCommit
}

// restoreWorkspace recovers or binds `<cards_dir>/workspace.json` at control
// plane startup using inode-verified target and staging directory checks.
func (c *ControlPlane) restoreWorkspace() (err error) {
	wf, ok, err := c.readWorkspaceRecord()
	if err != nil || !ok {
		return err
	}
	if c.initialWorkspaceRoot == "" {
		return errors.New("cards directory contains workspace.json, but no workspace directory is configured")
	}
	if err := validateInitDirName(wf.Dir); err != nil {
		return fmt.Errorf("invalid dir %q in workspace.json: %w", wf.Dir, err)
	}
	if !commitIDPattern.MatchString(wf.SuiteCommit) {
		return fmt.Errorf("invalid suite_commit %q in workspace.json", wf.SuiteCommit)
	}
	if wf.Dev == 0 || wf.Ino == 0 {
		return errors.New("workspace.json is missing non-zero dev/ino identity")
	}
	if (wf.Staging != "") != (wf.StageDir != "") {
		return errors.New("workspace.json must set stage_dir and staging together")
	}
	if wf.Staging != "" {
		if strings.ContainsAny(wf.Staging, `/\`) || wf.Staging == "." || wf.Staging == ".." {
			return fmt.Errorf("invalid staging name %q in workspace.json", wf.Staging)
		}
		if !filepath.IsAbs(wf.StageDir) || wf.StageDir != filepath.Clean(wf.StageDir) {
			return fmt.Errorf("invalid stage_dir %q in workspace.json", wf.StageDir)
		}
		for _, root := range c.protectedDirs {
			inside, err := abspath.Within(root, wf.StageDir)
			if err != nil {
				return err
			}
			if inside {
				return fmt.Errorf("stage_dir %s in workspace.json must be outside workspace %s", wf.StageDir, root)
			}
		}
	}

	workspaceFD, err := openVerifiedDirFD(c.initialWorkspaceRoot, c.initialWorkspaceDev, c.initialWorkspaceIno)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := unix.Close(workspaceFD); closeErr != nil {
			if err != nil {
				err = errors.Join(err, closeErr)
			} else {
				recordWorkspaceCleanupFailure("surla.controlplane.cleanup_restored_workspace_descriptors", closeErr)
			}
		}
	}()

	stageParentFD := -1
	if wf.Staging != "" {
		fd, openErr := unix.Open(wf.StageDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil && !errors.Is(openErr, unix.ENOENT) {
			return fmt.Errorf("open stage_dir %s from workspace.json: %w", wf.StageDir, openErr)
		}
		if openErr == nil {
			stageParentFD = fd
			defer func() {
				if closeErr := unix.Close(stageParentFD); closeErr != nil {
					if err != nil {
						err = errors.Join(err, closeErr)
					} else {
						recordWorkspaceCleanupFailure("surla.controlplane.cleanup_restored_workspace_descriptors", closeErr)
					}
				}
			}()
		}
	}

	targetPath := filepath.Join(c.initialWorkspaceRoot, wf.Dir)
	var targetStat unix.Stat_t
	targetErr := unix.Fstatat(workspaceFD, wf.Dir, &targetStat, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case targetErr == nil:
		isDir := targetStat.Mode&unix.S_IFMT == unix.S_IFDIR
		if isDir && uint64(targetStat.Dev) == wf.Dev && targetStat.Ino == wf.Ino {
			// Target directory exists with the exact recorded (Dev, Ino).
			if wf.Staging != "" {
				if err := unix.Fsync(workspaceFD); err != nil {
					return fmt.Errorf("sync restored workspace %s: %w", c.initialWorkspaceRoot, err)
				}
			}
			c.activeWorkspaceDir = targetPath
			c.boundWorkspaceDev = wf.Dev
			c.boundWorkspaceIno = wf.Ino
			c.boundSuiteCommit = wf.SuiteCommit
			if wf.Staging != "" {
				var cleanErr error
				if stageParentFD >= 0 {
					if err := removeOwnedStagingDir(stageParentFD, -1, wf.StageDir, wf.Staging, wf.Dev, wf.Ino); err != nil {
						var foreignErr foreignStagingError
						if !errors.As(err, &foreignErr) {
							cleanErr = errors.Join(cleanErr, err)
						}
					}
				}
				cleanErr = errors.Join(
					cleanErr,
					c.writeWorkspaceRecord(workspaceFile{
						Dir:         wf.Dir,
						SuiteCommit: wf.SuiteCommit,
						Dev:         wf.Dev,
						Ino:         wf.Ino,
					}),
				)
				if cleanErr != nil {
					recordWorkspaceCleanupFailure("surla.controlplane.finalize_restored_workspace", cleanErr)
				}
			}
			return nil
		}
		// Target directory exists, but its (Dev, Ino) does not match wf.
		if wf.Staging != "" {
			// Uncommitted initialization collided with a separately created target directory.
			if stageParentFD >= 0 {
				if err := removeOwnedStagingDir(stageParentFD, -1, wf.StageDir, wf.Staging, wf.Dev, wf.Ino); err != nil {
					var foreignErr foreignStagingError
					if !errors.As(err, &foreignErr) {
						return err
					}
				}
			}
			return c.removeWorkspaceRecord()
		}
		// Previously published target directory was replaced with a different inode.
		return fmt.Errorf("bound workspace directory %s no longer matches its initialized (dev, ino) identity", targetPath)

	case errors.Is(targetErr, unix.ENOENT):
		if wf.Staging == "" {
			// Previously published workspace directory was deleted.
			return fmt.Errorf("bound workspace directory %s does not exist", targetPath)
		}
		if stageParentFD < 0 {
			return c.removeWorkspaceRecord()
		}
		var stageStat unix.Stat_t
		stageErr := unix.Fstatat(stageParentFD, wf.Staging, &stageStat, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(stageErr, unix.ENOENT) {
			// Uncommitted initialization whose staging directory was already removed.
			return c.removeWorkspaceRecord()
		}
		if stageErr != nil {
			return fmt.Errorf("stat staged workspace %s: %w", filepath.Join(wf.StageDir, wf.Staging), stageErr)
		}
		if stageStat.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(stageStat.Dev) != wf.Dev || stageStat.Ino != wf.Ino {
			// Staging path holds a different inode; leave it untouched and remove the uncommitted record.
			return c.removeWorkspaceRecord()
		}
		// Target directory is missing and the staged directory still matches (wf.Dev, wf.Ino): roll forward publication.
		stageFD, err := unix.Openat(stageParentFD, wf.Staging, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open staged workspace %s: %w", filepath.Join(wf.StageDir, wf.Staging), err)
		}
		var openStat unix.Stat_t
		statErr := unix.Fstat(stageFD, &openStat)
		gotStagePath, pathErr := fdPath(stageFD)
		closeStageErr := unix.Close(stageFD)
		if err := errors.Join(statErr, closeStageErr); err != nil {
			return err
		}
		if pathErr != nil || gotStagePath != filepath.Join(wf.StageDir, wf.Staging) ||
			uint64(openStat.Dev) != wf.Dev || openStat.Ino != wf.Ino {
			return c.removeWorkspaceRecord()
		}
		if err := unix.RenameatxNp(stageParentFD, wf.Staging, workspaceFD, wf.Dir, unix.RENAME_EXCL); err != nil {
			return fmt.Errorf("recover staged workspace into %s: %w", targetPath, err)
		}
		syncErr := errors.Join(unix.Fsync(workspaceFD), unix.Fsync(stageParentFD))
		if err := verifyPublishedTarget(workspaceFD, wf.Dir, wf.Dev, wf.Ino); err != nil {
			return errors.Join(err, c.removeWorkspaceRecord(), syncErr)
		}
		c.activeWorkspaceDir = targetPath
		c.boundWorkspaceDev = wf.Dev
		c.boundWorkspaceIno = wf.Ino
		c.boundSuiteCommit = wf.SuiteCommit
		if syncErr != nil {
			return syncErr
		}
		if err := c.writeWorkspaceRecord(workspaceFile{
			Dir:         wf.Dir,
			SuiteCommit: wf.SuiteCommit,
			Dev:         wf.Dev,
			Ino:         wf.Ino,
		}); err != nil {
			recordWorkspaceCleanupFailure("surla.controlplane.finalize_restored_workspace", err)
		}
		return nil

	default:
		return fmt.Errorf("stat workspace target %s: %w", targetPath, targetErr)
	}
}

// readWorkspaceRecord reads and strictly decodes `<cards_dir>/workspace.json`.
func (c *ControlPlane) readWorkspaceRecord() (workspaceFile, bool, error) {
	if c.cards == nil || c.cards.lock == nil {
		return workspaceFile{}, false, errCardsClosed
	}
	recPath := filepath.Join(c.cards.dir, workspaceRecordFile)
	raw, err := readCardRegularFileNoFollow(recPath)
	if errors.Is(err, fs.ErrNotExist) {
		return workspaceFile{}, false, nil
	}
	if err != nil {
		return workspaceFile{}, false, fmt.Errorf("read workspace.json: %w", err)
	}
	var wf workspaceFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wf); err != nil {
		return workspaceFile{}, false, fmt.Errorf("decode workspace.json: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return workspaceFile{}, false, errors.New("decode workspace.json: trailing data")
	}
	return wf, true, nil
}

// writeWorkspaceRecord atomically writes and syncs wf to `<cards_dir>/workspace.json`.
func (c *ControlPlane) writeWorkspaceRecord(wf workspaceFile) error {
	if c.cards == nil || c.cards.lock == nil {
		return errCardsClosed
	}
	raw, err := json.Marshal(wf)
	if err != nil {
		return fmt.Errorf("encode workspace.json: %w", err)
	}
	tmpPath := filepath.Join(c.cards.dir, workspaceStagingWriteFile)
	recPath := filepath.Join(c.cards.dir, workspaceRecordFile)
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err == nil {
		_, err = f.Write(raw)
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
	}
	if err == nil {
		err = os.Rename(tmpPath, recPath)
	}
	if err != nil {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
		return fmt.Errorf("write workspace.json: %w", err)
	}
	return syncDir(c.cards.dir)
}

// removeWorkspaceRecord removes `<cards_dir>/workspace.json` and any temporary
// staging record file if present, then syncs the cards directory.
func (c *ControlPlane) removeWorkspaceRecord() error {
	if c.cards == nil || c.cards.lock == nil {
		return errCardsClosed
	}
	var err error
	for _, name := range []string{workspaceRecordFile, workspaceStagingWriteFile} {
		recPath := filepath.Join(c.cards.dir, name)
		if rmErr := os.Remove(recPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove %s: %w", name, rmErr))
		}
	}
	if err != nil {
		return err
	}
	return syncDir(c.cards.dir)
}

// openModuleRootWithIdentity opens dir as an `*os.Root` handle via
// `designeval.OpenModuleRoot` and verifies on the opened handle itself
// (`root.Stat(".")`) that its `(dev, ino)` matches `(expectedDev, expectedIno)`.
func openModuleRootWithIdentity(dir string, expectedDev, expectedIno uint64) (string, *os.Root, error) {
	if dir == "" {
		return "", nil, gensurla.MakeUnprocessable(errors.New("no workspace directory is bound to this control plane"))
	}
	if expectedDev == 0 || expectedIno == 0 {
		return "", nil, gensurla.MakeUnprocessable(fmt.Errorf("bound workspace directory %s has no recorded device/inode identity", dir))
	}
	cleanDir, root, err := designeval.OpenModuleRoot(dir)
	if err != nil {
		return "", nil, gensurla.MakeUnprocessable(fmt.Errorf("bound workspace directory %s was replaced or removed after startup: %w", dir, err))
	}
	rootInfo, err := root.Stat(".")
	if err != nil {
		return "", nil, errors.Join(
			gensurla.MakeUnprocessable(fmt.Errorf("bound workspace directory %s was replaced or removed after startup: %w", dir, err)),
			root.Close(),
		)
	}
	st, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok || st == nil || uint64(st.Dev) != expectedDev || st.Ino != expectedIno {
		return "", nil, errors.Join(
			gensurla.MakeUnprocessable(fmt.Errorf("bound workspace directory %s was replaced or removed after startup", dir)),
			root.Close(),
		)
	}
	return cleanDir, root, nil
}

// readWorkspaceModuleSnapshot reads the bound workspace's Go module and design
// snapshot (`go.mod`, `go.sum`, and non-test `.go` files) through an
// identity-verified `*os.Root` handle for `PreviewArchitecture`.
func readWorkspaceModuleSnapshot(wsDir string, expectedDev, expectedIno uint64) (map[string]string, error) {
	cleanDir, root, err := openModuleRootWithIdentity(wsDir, expectedDev, expectedIno)
	if err != nil {
		return nil, err
	}
	files, readErr := designeval.ReadModuleSnapshotFromRoot(cleanDir, root)
	if err := errors.Join(readErr, root.Close()); err != nil {
		return nil, gensurla.MakeUnprocessable(err)
	}
	if err := designeval.ValidateUTF8Snapshot(files); err != nil {
		return nil, gensurla.MakeUnprocessable(err)
	}
	if _, hasGoMod := files["go.mod"]; !hasGoMod {
		return nil, gensurla.MakeUnprocessable(fmt.Errorf(
			"workspace %s does not contain a go.mod file; run init_project first or pass design in the request",
			cleanDir,
		))
	}
	return files, nil
}

// readWorkspaceDesignSource reads `design/*.go` files from the bound workspace
// through an identity-verified `*os.Root` handle, closes the handle before
// returning, validates that every file path and body is valid UTF-8, and
// returns the design source a Change Card records for them: the files as a
// JSON object keyed by module-relative path, with HTML characters unescaped
// and no trailing newline. That exact text is saved as the card's
// design_source and hashed into its request_revision, so it must not change
// for the same files. When `design/` does not exist or contains no non-test
// `.go` files, it returns "" so a card proposed before `init_project` has an
// empty design source and does not block initialization. A workspace that was
// replaced or cannot be read is unprocessable.
func readWorkspaceDesignSource(wsDir string, expectedDev, expectedIno uint64) (string, error) {
	cleanDir, root, err := openModuleRootWithIdentity(wsDir, expectedDev, expectedIno)
	if err != nil {
		return "", err
	}
	files, readErr := designeval.ReadDesignFilesFromRoot(cleanDir, root)
	if err := errors.Join(readErr, root.Close()); err != nil {
		return "", gensurla.MakeUnprocessable(err)
	}
	if len(files) == 0 {
		return "", nil
	}
	if err := designeval.ValidateUTF8Snapshot(files); err != nil {
		return "", gensurla.MakeUnprocessable(err)
	}
	return encodeDesignSource(files)
}

// designSourceFromModuleSnapshot extracts the non-test design/*.go entries from
// a validated workspace module snapshot and encodes them with encodeDesignSource.
func designSourceFromModuleSnapshot(files map[string]string) (string, error) {
	var designFiles map[string]string
	for rel, content := range files {
		if strings.HasPrefix(rel, "design/") && strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go") {
			if designFiles == nil {
				designFiles = make(map[string]string)
			}
			designFiles[rel] = content
		}
	}
	return encodeDesignSource(designFiles)
}

// encodeDesignSource encodes a map of design/*.go files as the canonical JSON
// object saved in a Change Card's design_source (HTML characters unescaped, no
// trailing newline), or "" when designFiles is empty.
func encodeDesignSource(designFiles map[string]string) (string, error) {
	if len(designFiles) == 0 {
		return "", nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(designFiles); err != nil {
		return "", fmt.Errorf("encode workspace design source: %w", err)
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// normalizeAndValidateStartupPaths resolves configured operator storage paths
// to physical paths via abspath.EvalMissing and validates separation from
// wsRoot and its enclosing Git working tree before any directory is created.
func normalizeAndValidateStartupPaths(opts Options, wsRoot string) (string, string, *VerificationConfig, []string, error) {
	var runsDir string
	if opts.RunsDir != "" {
		physRuns, err := abspath.EvalMissing(opts.RunsDir)
		if err != nil {
			return "", "", nil, nil, fmt.Errorf("resolve runs directory %s: %w", opts.RunsDir, err)
		}
		runsDir = physRuns
	}
	var initStageDir string
	if opts.InitStageDir != "" {
		physStage, err := abspath.EvalMissing(opts.InitStageDir)
		if err != nil {
			return "", "", nil, nil, fmt.Errorf("resolve init stage directory %s: %w", opts.InitStageDir, err)
		}
		initStageDir = physStage
	}
	verCfg, err := normalizeVerificationConfig(opts.Verification)
	if err != nil {
		return "", "", nil, nil, err
	}
	gitCtx, cancel := context.WithTimeout(contextBackgroundForCleanup(), workspaceGitTimeout)
	protectedDirs, err := verify.WorkspaceBoundaryRoots(gitCtx, wsRoot)
	cancel()
	if err != nil {
		return "", "", nil, nil, err
	}
	if err := validateStartupSeparation(opts.CardsDir, runsDir, initStageDir, verCfg, protectedDirs...); err != nil {
		return "", "", nil, nil, err
	}
	return runsDir, initStageDir, verCfg, protectedDirs, nil
}

// normalizeVerificationConfig returns a shallow copy of cfg with RunsDir and
// Plan.ReportDir resolved to physical paths via abspath.EvalMissing.
func normalizeVerificationConfig(cfg *VerificationConfig) (*VerificationConfig, error) {
	if cfg == nil {
		return nil, nil
	}
	out := *cfg
	if out.RunsDir != "" {
		physRuns, err := abspath.EvalMissing(out.RunsDir)
		if err != nil {
			return nil, fmt.Errorf("resolve verification runs_dir %s: %w", out.RunsDir, err)
		}
		out.RunsDir = physRuns
	}
	if out.Plan.ReportDir != "" {
		physReports, err := abspath.EvalMissing(out.Plan.ReportDir)
		if err != nil {
			return nil, fmt.Errorf("resolve verification report_dir %s: %w", out.Plan.ReportDir, err)
		}
		out.Plan.ReportDir = physReports
	}
	return &out, nil
}

// validateStartupSeparation verifies that cardsDir, runsDir, initStageDir, and
// verCfg lie strictly outside protectedDirs (the bound workspace and its
// enclosing Git working tree root).
func validateStartupSeparation(
	cardsDir, runsDir, initStageDir string,
	verCfg *VerificationConfig,
	protectedDirs ...string,
) error {
	if err := validateCardsWorkspaceSeparation(cardsDir, protectedDirs...); err != nil {
		return err
	}
	if err := validateRunsDirWorkspaceSeparation(runsDir, protectedDirs...); err != nil {
		return err
	}
	if err := validateInitStageWorkspaceSeparation(initStageDir, protectedDirs...); err != nil {
		return err
	}
	return validateVerificationWorkspaceSeparation(verCfg, protectedDirs...)
}

// resolveActiveVerification resolves the active VerificationConfig for
// VerifyChange and StartLiveTarget, verifying the bound workspace's (dev, ino)
// identity and physical path separation.
func (c *ControlPlane) resolveActiveVerification(ctx context.Context) (*VerificationConfig, error) {
	if c.resolveVerification != nil {
		wsDir, wsDev, wsIno, suiteCommit := c.currentWorkspaceVerificationTarget()
		if wsDir != "" {
			_, root, err := openModuleRootWithIdentity(wsDir, wsDev, wsIno)
			if err != nil {
				return nil, err
			}
			if err := root.Close(); err != nil {
				return nil, err
			}
		}
		cfg, err := c.resolveVerification(ctx, wsDir, suiteCommit)
		if err != nil {
			return nil, err
		}
		if cfg == nil {
			return nil, nil
		}
		normCfg, err := normalizeVerificationConfig(cfg)
		if err != nil {
			return nil, err
		}
		if err := validateVerificationWorkspaceSeparation(normCfg, c.protectedDirs...); err != nil {
			return nil, err
		}
		return normCfg, nil
	}
	return c.verification, nil
}

// validateCardsWorkspaceSeparation verifies that cardsDir does not lie inside
// any protected workspace or enclosing Git working tree root.
func validateCardsWorkspaceSeparation(cardsDir string, protectedDirs ...string) error {
	if cardsDir == "" {
		return nil
	}
	physCards, err := abspath.EvalMissing(cardsDir)
	if err != nil {
		return fmt.Errorf("resolve cards directory %s: %w", cardsDir, err)
	}
	for _, ws := range protectedDirs {
		if ws == "" {
			continue
		}
		inside, err := abspath.Within(ws, physCards)
		if err != nil {
			return err
		}
		if inside {
			return fmt.Errorf(
				"cards directory %s must be outside the workspace (%s) so builder agents cannot modify authoritative Change Cards or workspace state",
				physCards,
				ws,
			)
		}
	}
	return nil
}

// validateRunsDirWorkspaceSeparation verifies that runsDir does not lie inside
// any protected workspace or enclosing Git working tree root.
func validateRunsDirWorkspaceSeparation(runsDir string, protectedDirs ...string) error {
	if runsDir == "" {
		return nil
	}
	physRuns, err := abspath.EvalMissing(runsDir)
	if err != nil {
		return fmt.Errorf("resolve runs directory %s: %w", runsDir, err)
	}
	for _, ws := range protectedDirs {
		if ws == "" {
			continue
		}
		inside, err := abspath.Within(ws, physRuns)
		if err != nil {
			return err
		}
		if inside {
			return fmt.Errorf(
				"verification runs directory %s must be outside the workspace (%s) so builder agents cannot modify verification state",
				physRuns,
				ws,
			)
		}
	}
	return nil
}

// validateInitStageWorkspaceSeparation verifies that initStageDir does not lie
// inside any protected workspace or enclosing Git working tree root.
func validateInitStageWorkspaceSeparation(initStageDir string, protectedDirs ...string) error {
	if initStageDir == "" {
		return nil
	}
	physStage, err := abspath.EvalMissing(initStageDir)
	if err != nil {
		return fmt.Errorf("resolve init stage directory %s: %w", initStageDir, err)
	}
	for _, ws := range protectedDirs {
		if ws == "" {
			continue
		}
		inside, err := abspath.Within(ws, physStage)
		if err != nil {
			return err
		}
		if inside {
			return fmt.Errorf(
				"init stage directory %s must be outside workspace %s",
				physStage,
				ws,
			)
		}
	}
	return nil
}

// validateVerificationWorkspaceSeparation verifies that a static or resolved
// VerificationConfig's RunsDir and Plan.ReportDir do not lie inside any
// protected workspace or enclosing Git working tree root.
func validateVerificationWorkspaceSeparation(cfg *VerificationConfig, protectedDirs ...string) error {
	if cfg == nil {
		return nil
	}
	if err := validateRunsDirWorkspaceSeparation(cfg.RunsDir, protectedDirs...); err != nil {
		return err
	}
	if cfg.Plan.ReportDir == "" {
		return nil
	}
	physReports, err := abspath.EvalMissing(cfg.Plan.ReportDir)
	if err != nil {
		return fmt.Errorf("resolve verification report_dir %s: %w", cfg.Plan.ReportDir, err)
	}
	for _, ws := range protectedDirs {
		if ws == "" {
			continue
		}
		inside, err := abspath.Within(ws, physReports)
		if err != nil {
			return err
		}
		if inside {
			return fmt.Errorf(
				"verification report_dir %s must be outside the workspace (%s) so builder agents cannot read or modify evaluator reports",
				physReports,
				ws,
			)
		}
	}
	return nil
}

// recordWorkspaceCleanupFailure records a post-durability cleanup failure on a
// dedicated OpenTelemetry span without failing the already-completed operation.
func recordWorkspaceCleanupFailure(spanName string, err error) {
	if err == nil {
		return
	}
	_, span := otel.Tracer("surla/controlplane").Start(
		contextBackgroundForCleanup(),
		spanName,
	)
	span.RecordError(err)
	span.SetStatus(codes.Error, "workspace post-publication cleanup failed")
	span.End()
}
