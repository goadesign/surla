// This file owns the control plane's bound filesystem workspace, inode-verified
// startup recovery (`<cards_dir>/workspace.json`), `os.Root` design/module
// snapshot readers, and workspace-separation checks.
package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"golang.org/x/sys/unix"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/designeval"
)

type (
	// workspaceFile is the persisted JSON schema of `<cards_dir>/workspace.json`.
	// During `init_project`, Step 1 writes the record with `StageDir`, `Staging`,
	// `Dev`, and `Ino` before `RenameatxNp`; Step 3 clears `StageDir` and
	// `Staging` once the renamed target directory's `(Dev, Ino)` is verified and
	// synced.
	workspaceFile struct {
		// Dir is the direct child directory name inside initialWorkspaceRoot.
		Dir string `json:"dir"`
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

// openWorkspaceRootAndStat resolves dir to a physical non-symlink directory,
// opens an os.Root handle to verify its open-descriptor identity, and returns
// its cleaned physical path, device ID, and inode number.
func openWorkspaceRootAndStat(dir string) (string, uint64, uint64, error) {
	if strings.TrimSpace(dir) == "" {
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
	cleanDir, err := abspath.Eval(dir)
	if err != nil {
		return "", 0, 0, fmt.Errorf("resolve workspace %s: %w", dir, err)
	}
	root, err := os.OpenRoot(cleanDir)
	if err != nil {
		return "", 0, 0, fmt.Errorf("open workspace %s: %w", cleanDir, err)
	}
	defer func() {
		_ = root.Close()
	}()
	rootInfo, err := root.Stat(".")
	if err != nil {
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

// restoreWorkspace recovers or binds `<cards_dir>/workspace.json` at control
// plane startup using the 4-case inode-verified state machine.
func (c *ControlPlane) restoreWorkspace() error {
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
		if isWithinOrEqualDir(wf.StageDir, c.initialWorkspaceRoot) {
			return fmt.Errorf("stage_dir %s in workspace.json must be outside workspace %s", wf.StageDir, c.initialWorkspaceRoot)
		}
	}

	workspaceFD, err := openVerifiedDirFD(c.initialWorkspaceRoot, c.initialWorkspaceDev, c.initialWorkspaceIno)
	if err != nil {
		return err
	}
	defer func() {
		_ = unix.Close(workspaceFD)
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
				_ = unix.Close(stageParentFD)
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
			// Case 1: targetBase exists with the exact staged (Dev, Ino).
			if wf.Staging != "" {
				if err := unix.Fsync(workspaceFD); err != nil {
					return fmt.Errorf("sync restored workspace %s: %w", c.initialWorkspaceRoot, err)
				}
			}
			c.activeWorkspaceDir = targetPath
			c.boundWorkspaceDev = wf.Dev
			c.boundWorkspaceIno = wf.Ino
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
						Dir: wf.Dir,
						Dev: wf.Dev,
						Ino: wf.Ino,
					}),
				)
				if cleanErr != nil {
					recordWorkspaceCleanupFailure("surla.controlplane.finalize_restored_workspace", cleanErr)
				}
			}
			return nil
		}
		// Case 2: targetBase exists, but its (Dev, Ino) does not match wf.
		if wf.Staging != "" {
			// Subcase 2a: uncommitted init collided with an external targetBase.
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
		// Subcase 2b: already-published targetBase was replaced.
		return fmt.Errorf("bound workspace directory %s no longer matches its initialized (dev, ino) identity", targetPath)

	case errors.Is(targetErr, unix.ENOENT):
		if wf.Staging == "" {
			// Case 4 (committed): already-published workspace directory was deleted.
			return fmt.Errorf("bound workspace directory %s does not exist", targetPath)
		}
		if stageParentFD < 0 {
			return c.removeWorkspaceRecord()
		}
		var stageStat unix.Stat_t
		stageErr := unix.Fstatat(stageParentFD, wf.Staging, &stageStat, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(stageErr, unix.ENOENT) {
			// Case 4 (uncommitted, staging missing): remove uncommitted record.
			return c.removeWorkspaceRecord()
		}
		if stageErr != nil {
			return fmt.Errorf("stat staged workspace %s: %w", filepath.Join(wf.StageDir, wf.Staging), stageErr)
		}
		if stageStat.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(stageStat.Dev) != wf.Dev || stageStat.Ino != wf.Ino {
			// Case 4 (foreign staging entry): preserve foreign entry untouched and
			// remove uncommitted workspace.json.
			return c.removeWorkspaceRecord()
		}
		// Case 3: targetBase is ENOENT and (stageParentFD, wf.Staging) matches (wf.Dev, wf.Ino).
		stageFD, err := unix.Openat(stageParentFD, wf.Staging, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open staged workspace %s: %w", filepath.Join(wf.StageDir, wf.Staging), err)
		}
		var openStat unix.Stat_t
		statErr := unix.Fstat(stageFD, &openStat)
		gotStagePath, pathErr := fdPath(stageFD)
		_ = unix.Close(stageFD)
		if statErr != nil {
			return statErr
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
		if syncErr != nil {
			return syncErr
		}
		if err := c.writeWorkspaceRecord(workspaceFile{
			Dir: wf.Dir,
			Dev: wf.Dev,
			Ino: wf.Ino,
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
		return fmt.Errorf("write workspace.json: %w", err)
	}
	return syncDir(c.cards.dir)
}

// removeWorkspaceRecord removes `<cards_dir>/workspace.json` if present and
// syncs the cards directory.
func (c *ControlPlane) removeWorkspaceRecord() error {
	if c.cards == nil || c.cards.lock == nil {
		return errCardsClosed
	}
	recPath := filepath.Join(c.cards.dir, workspaceRecordFile)
	if err := os.Remove(recPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove workspace.json: %w", err)
	}
	return syncDir(c.cards.dir)
}

// openModuleRootWithIdentity opens dir as an `*os.Root` handle via
// `designeval.OpenModuleRoot` and verifies on the opened handle itself
// (`root.Stat(".")`) that its `(dev, ino)` matches `(expectedDev, expectedIno)`.
func openModuleRootWithIdentity(dir string, expectedDev, expectedIno uint64) (string, *os.Root, error) {
	if strings.TrimSpace(dir) == "" {
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
// returning, and validates that every file path and body is valid UTF-8. When
// `design/` does not exist or contains no non-test `.go` files, it returns
// `("", nil)` so a card proposed before `init_project` has an empty
// `designSource` and does not block initialization.
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
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(files); err != nil {
		return "", fmt.Errorf("encode workspace design source: %w", err)
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// validateCardsWorkspaceSeparation verifies that cardsDir lies strictly outside
// initialWorkspaceRoot and activeWorkspaceDir.
func validateCardsWorkspaceSeparation(cardsDir, initialWorkspaceRoot, activeWorkspaceDir string) error {
	if strings.TrimSpace(cardsDir) == "" {
		return nil
	}
	physCards, err := resolveExistingOrMissingPath(cardsDir)
	if err != nil {
		return fmt.Errorf("resolve cards directory %s: %w", cardsDir, err)
	}
	for _, ws := range []string{initialWorkspaceRoot, activeWorkspaceDir} {
		if strings.TrimSpace(ws) == "" {
			continue
		}
		physWS, err := resolveExistingOrMissingPath(ws)
		if err != nil {
			return fmt.Errorf("resolve workspace %s: %w", ws, err)
		}
		if isWithinOrEqualDir(physCards, physWS) {
			return fmt.Errorf(
				"cards directory %s must be outside the workspace (%s) so builder agents cannot modify authoritative Change Cards or workspace state",
				physCards,
				physWS,
			)
		}
	}
	return nil
}

// validateRunsDirWorkspaceSeparation verifies that runsDir lies strictly
// outside initialWorkspaceRoot and activeWorkspaceDir.
func validateRunsDirWorkspaceSeparation(runsDir, initialWorkspaceRoot, activeWorkspaceDir string) error {
	if strings.TrimSpace(runsDir) == "" {
		return nil
	}
	physRuns, err := resolveExistingOrMissingPath(runsDir)
	if err != nil {
		return fmt.Errorf("resolve runs directory %s: %w", runsDir, err)
	}
	for _, ws := range []string{initialWorkspaceRoot, activeWorkspaceDir} {
		if strings.TrimSpace(ws) == "" {
			continue
		}
		physWS, err := resolveExistingOrMissingPath(ws)
		if err != nil {
			return fmt.Errorf("resolve workspace %s: %w", ws, err)
		}
		if isWithinOrEqualDir(physRuns, physWS) {
			return fmt.Errorf(
				"verification runs directory %s must be outside the workspace (%s) so builder agents cannot modify verification state",
				physRuns,
				physWS,
			)
		}
	}
	return nil
}

// validateVerificationWorkspaceSeparation verifies that a static or resolved
// VerificationConfig's RunsDir and Plan.ReportDir lie strictly outside
// initialWorkspaceRoot and activeWorkspaceDir.
func validateVerificationWorkspaceSeparation(cfg *VerificationConfig, initialWorkspaceRoot, activeWorkspaceDir string) error {
	if cfg == nil {
		return nil
	}
	if err := validateRunsDirWorkspaceSeparation(cfg.RunsDir, initialWorkspaceRoot, activeWorkspaceDir); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Plan.ReportDir) == "" {
		return nil
	}
	physReports, err := resolveExistingOrMissingPath(cfg.Plan.ReportDir)
	if err != nil {
		return fmt.Errorf("resolve verification report_dir %s: %w", cfg.Plan.ReportDir, err)
	}
	for _, ws := range []string{initialWorkspaceRoot, activeWorkspaceDir} {
		if strings.TrimSpace(ws) == "" {
			continue
		}
		physWS, err := resolveExistingOrMissingPath(ws)
		if err != nil {
			return fmt.Errorf("resolve workspace %s: %w", ws, err)
		}
		if isWithinOrEqualDir(physReports, physWS) {
			return fmt.Errorf(
				"verification report_dir %s must be outside the workspace (%s) so builder agents cannot read or modify evaluator reports",
				physReports,
				physWS,
			)
		}
	}
	return nil
}

// resolveExistingOrMissingPath resolves existing ancestors via abspath.Eval and
// appends any non-existent trailing path segments.
func resolveExistingOrMissingPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	var missing []string
	cur := abs
	for {
		phys, err := abspath.Eval(cur)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				phys = filepath.Join(phys, missing[i])
			}
			return phys, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}

// isWithinOrEqualDir reports whether child equals or is a subdirectory of root.
func isWithinOrEqualDir(child, root string) bool {
	if child == "" || root == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(child))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
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
