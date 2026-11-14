// This file wires the project server to operator-owned storage outside the bound
// workspace and configures dynamic starter verification and promotion
// resolution.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/project"
	"github.com/goadesign/surla/internal/verify"
)

type (
	// workspaceServeConfig holds the resolved control-plane options for a
	// workspace-bound project server instance.
	workspaceServeConfig struct {
		// workspaceDir is the physical initial workspace directory.
		workspaceDir string
		// cardsDir is the selected protected cards directory.
		cardsDir string
		// initStageDir is the operator-owned staging parent directory for
		// init_project.
		initStageDir string
		// evalWorkDir is the operator-owned temporary parent directory for
		// designeval.Evaluator.
		evalWorkDir string
		// runsDir is the protected verification runs directory.
		runsDir string
		// verification is the stored evaluator, from the project's stored
		// verification settings, or nil when none are stored.
		verification *controlplane.VerificationConfig
		// writtenTestRuns runs the tests accepted for each change when no
		// verification settings are stored; nil when they are.
		writtenTestRuns *controlplane.WrittenTestRuns
		// promotionTarget is the startup PromotionTarget when already available.
		promotionTarget controlplane.PromotionTarget
		// resolvePromotionTarget dynamically resolves PromotionTarget for the
		// active workspace directory.
		resolvePromotionTarget func(ctx context.Context, workspaceDir string) (controlplane.PromotionTarget, error)
	}
)

const (
	// operatorPrivateMarker is the marker file written inside operator-owned
	// private directories.
	operatorPrivateMarker = ".surla-private"
	// operatorPrivateXattr names the extended attribute attached to private
	// Surla directories.
	operatorPrivateXattr = "com.goadesign.surla.private"
	// operatorPrivateDirValue is the extended attribute value for private Surla
	// directories.
	operatorPrivateDirValue = "dir-v1"
	// storedPromotionSettingsStep tells the operator how to inspect, replace or
	// remove stored promotion settings when the project server cannot load them.
	storedPromotionSettingsStep = "see them with surla promotion show, store them again with surla promotion set <file>, or remove them with surla promotion remove if this project needs no approvals; then run surla"
	// storedVerificationSettingsStep tells the operator how to inspect, replace or
	// remove stored verification settings when the project server cannot load them.
	storedVerificationSettingsStep = "see them with surla verification show, store them again with surla verification set <file>, or remove them with surla verification remove if this project uses the tests written for each change; then run surla"
)

// configureProjectWorkspace resolves the bound workspace, operator-owned storage
// directories and startup promotion inputs for the project server, and how it
// verifies: with the evaluator in the project's stored verification settings,
// or, when none are stored, with the tests accepted for each change's build.
func configureProjectWorkspace(state *project.State) (workspaceServeConfig, error) {
	initial := state.Record.Directory
	protected, err := workspaceProtectedDirs(initial)
	if err != nil {
		return workspaceServeConfig{}, err
	}
	for _, name := range []string{"cards", "runs", "reports", "designeval-work"} {
		if err := markOperatorPrivateDir(filepath.Join(state.Dir, name)); err != nil {
			return workspaceServeConfig{}, err
		}
	}
	ws := workspaceServeConfig{
		workspaceDir: initial,
		cardsDir:     state.CardsDir(),
		runsDir:      filepath.Join(state.Dir, "runs"),
		initStageDir: filepath.Join(state.Dir, "init-stage"),
		evalWorkDir:  filepath.Join(state.Dir, "designeval-work"),
	}
	reports := filepath.Join(state.Dir, "reports")
	// Read snapshots only at startup: later management writes apply after restart.
	if raw, err := state.ReadSettings("promotion"); err == nil {
		var file promotionFile
		if err := decodeSettings(raw, &file); err != nil {
			return workspaceServeConfig{}, fmt.Errorf("%w: %s", err, storedPromotionSettingsStep)
		}
		if err := validatePromotionFile(file); err != nil {
			return workspaceServeConfig{}, fmt.Errorf("%w: %s", err, storedPromotionSettingsStep)
		}
		file.ReportDir = reports
		if err := validateWorkspaceDirSeparation("release_dir", file.ReleaseDir, protected...); err != nil {
			return workspaceServeConfig{}, fmt.Errorf("%w: %s", err, storedPromotionSettingsStep)
		}
		ws.promotionTarget = &releaseTarget{cfg: file}
		ws.resolvePromotionTarget = func(context.Context, string) (controlplane.PromotionTarget, error) { return ws.promotionTarget, nil }
	} else if !errors.Is(err, os.ErrNotExist) {
		return workspaceServeConfig{}, err
	}
	if raw, err := state.ReadSettings("verification"); err == nil {
		var file verificationFile
		if err := decodeSettings(raw, &file); err != nil {
			return workspaceServeConfig{}, fmt.Errorf("%w: %s", err, storedVerificationSettingsStep)
		}
		config, err := verificationConfig(file, ws.runsDir, reports)
		if err != nil {
			return workspaceServeConfig{}, fmt.Errorf("%w: %s", err, storedVerificationSettingsStep)
		}
		ws.verification = config
		return ws, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return workspaceServeConfig{}, err
	}
	self, err := selfCommand()
	if err != nil {
		return workspaceServeConfig{}, err
	}
	ws.writtenTestRuns = &controlplane.WrittenTestRuns{
		ReportDir: reports,
		Command:   self,
		Timeout:   defaultProjectVerifyTimeout,
	}
	return ws, nil
}

// decodePromotionFileAtBase decodes a promotion configuration file and resolves
// its relative directories against baseDir.
func decodePromotionFileAtBase(path, baseDir string) (promotionFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return promotionFile{}, fmt.Errorf("read promotion settings: %w", err)
	}
	var f promotionFile
	if err := decodeSettings(raw, &f); err != nil {
		return promotionFile{}, fmt.Errorf("decode promotion settings %s: %w", path, err)
	}
	if err := validatePromotionFile(f); err != nil {
		return promotionFile{}, err
	}

	f.ReleaseDir = joinPreservingRawSegments(baseDir, f.ReleaseDir)
	return f, nil
}

// validatePromotionFile enforces the settings contract at both operator-file and stored-snapshot ingress.
func validatePromotionFile(f promotionFile) error {
	var missing []string
	if f.ReleaseDir == "" {
		missing = append(missing, "release_dir")
	}
	if len(f.Binaries) == 0 {
		missing = append(missing, "binaries")
	}
	if len(missing) > 0 {
		return fmt.Errorf("promotion configuration is missing %s", strings.Join(missing, ", "))
	}
	seen := make(map[string]bool, len(f.Binaries))
	for _, name := range f.Binaries {
		if err := verify.ValidateArtifactName(name); err != nil {
			return err
		}
		if seen[name] {
			return fmt.Errorf("binary %q is listed twice", name)
		}
		seen[name] = true
	}
	return nil
}

// joinPreservingRawSegments resolves p against baseDir when p is relative
// without collapsing ".." segments before symlink-aware callers inspect them.
func joinPreservingRawSegments(baseDir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return strings.TrimRight(baseDir, string(filepath.Separator)) + string(filepath.Separator) + p
}

// operatorRoot returns the single deterministic operator-only storage
// root (`<UserConfigDir>/surla/operator`) strictly outside initialWorkspaceRoot
// and any extraDirs, failing if unavailable, inside the workspace grant, or not
// owned by the current effective user with 0o700 permissions.
func (runtime projectRuntime) operatorRoot(initialWorkspaceRoot string, extraDirs ...string) (string, error) {
	cfgRoot, err := runtime.configRoot()
	if err != nil {
		return "", fmt.Errorf("determine user configuration directory for operator storage: %w", err)
	}
	if cfgRoot == "" {
		return "", errors.New("user configuration directory is empty; set HOME or XDG_CONFIG_HOME for operator storage")
	}
	cand := filepath.Join(cfgRoot, "surla", "operator")
	excluded := append([]string{initialWorkspaceRoot}, extraDirs...)
	phys, err := abspath.EvalMissing(cand)
	if err != nil {
		return "", fmt.Errorf("resolve operator storage root %s: %w", cand, err)
	}
	inside, err := withinAnyDir(phys, excluded)
	if err != nil {
		return "", err
	}
	if inside {
		return "", fmt.Errorf("operator storage root %s must be outside the workspace (%s)", phys, initialWorkspaceRoot)
	}
	if err := markOperatorPrivateDir(phys); err != nil {
		return "", operatorRootError(phys, err)
	}
	evalPhys, err := abspath.Eval(phys)
	if err != nil {
		return "", fmt.Errorf("resolve operator storage root %s: %w", phys, err)
	}
	inside, err = withinAnyDir(evalPhys, excluded)
	if err != nil {
		return "", err
	}
	if inside {
		return "", fmt.Errorf("operator storage root %s must be outside the workspace (%s)", evalPhys, initialWorkspaceRoot)
	}
	if err := validateOperatorPrivateDir(evalPhys); err != nil {
		return "", operatorRootError(evalPhys, err)
	}
	return evalPhys, nil
}

// operatorRootError names the operator storage root dir that operatorRoot could not
// prepare. When dir exists but is not a folder of this account that only this account
// can open, it also names the step that repairs it; `surla` reads every project's state
// below dir, so no project can start until then. Other failures (dir could not be
// created, or its marker written) keep only the cause, since that step would not
// repair them.
func operatorRootError(dir string, err error) error {
	if validErr := validateOperatorPrivateDir(dir); validErr == nil || errors.Is(validErr, os.ErrNotExist) {
		return fmt.Errorf("prepare operator storage root %s: %w", dir, err)
	}
	return fmt.Errorf("prepare operator storage root %s: %w; make it a folder of this account that only this account can open (chmod 700 %s, and chown -R it to this account if another account owns it), then run surla again", dir, err, dir)
}

// withinAnyDir checks physical containment in the supplied existing roots.
// Filesystem failures stop admission instead of treating a path as outside.
func withinAnyDir(dir string, dirs []string) (bool, error) {
	for _, d := range dirs {
		if d == "" {
			continue
		}
		inside, err := abspath.Within(d, dir)
		if err != nil {
			return false, err
		}
		if inside {
			return true, nil
		}
	}
	return false, nil
}

// workspaceProtectedDirs returns the deduplicated physical paths of
// initialWorkspaceRoot and its enclosing Git working tree root.
func workspaceProtectedDirs(initialWorkspaceRoot string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitReadTimeout)
	defer cancel()
	return verify.WorkspaceBoundaryRoots(ctx, initialWorkspaceRoot)
}

// markOperatorPrivateDir creates dir with mode 0o700, verifies its non-symlink
// directory ownership and 0o700 permissions, and writes the private Surla
// directory marker and extended attribute.
func markOperatorPrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := validateOperatorPrivateDir(dir); err != nil {
		return err
	}
	if err := unix.Setxattr(dir, operatorPrivateXattr, []byte(operatorPrivateDirValue), 0); err != nil &&
		!errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EOPNOTSUPP) {
		return fmt.Errorf("set private xattr on %s: %w", dir, err)
	}
	markerPath := filepath.Join(dir, operatorPrivateMarker)
	f, err := os.OpenFile(markerPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// validateOperatorPrivateDir verifies that dir is a non-symlink directory owned
// by the current effective user with exact 0o700 permissions and R_OK|W_OK|X_OK
// effective access.
func validateOperatorPrivateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("operator private directory %s must be a non-symlink directory", dir)
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open operator private directory %s: %w", dir, err)
	}
	var st unix.Stat_t
	statErr := unix.Fstat(fd, &st)
	closeErr := unix.Close(fd)
	if err := errors.Join(statErr, closeErr); err != nil {
		return fmt.Errorf("stat operator private directory %s: %w", dir, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("operator private directory %s is not a directory", dir)
	}
	euid := uint32(os.Geteuid())
	if st.Uid != euid {
		return fmt.Errorf("operator private directory %s is owned by uid %d, want effective uid %d", dir, st.Uid, euid)
	}
	perm := st.Mode & 0o777
	if perm != 0o700 {
		return fmt.Errorf("operator private directory %s has permissions %#o, want 0o700", dir, perm)
	}
	if err := unix.Faccessat(unix.AT_FDCWD, dir, unix.R_OK|unix.W_OK|unix.X_OK, unix.AT_EACCESS); err != nil {
		return fmt.Errorf("operator private directory %s is not accessible: %w", dir, err)
	}
	return nil
}

// validateWorkspaceDirSeparation checks that targetDir does not lie inside any
// protected workspace or enclosing Git working tree root.
func validateWorkspaceDirSeparation(label, targetDir string, protectedDirs ...string) error {
	physTarget, err := abspath.EvalMissing(targetDir)
	if err != nil {
		return err
	}
	for _, ws := range protectedDirs {
		if ws == "" {
			continue
		}
		inside, err := abspath.Within(ws, physTarget)
		if err != nil {
			return err
		}
		if inside {
			return fmt.Errorf(
				"%s %s must be outside the workspace (%s) so builder agents cannot modify authoritative Change Cards or workspace state",
				label,
				physTarget,
				ws,
			)
		}
	}
	return nil
}
