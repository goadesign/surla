// This file wires `surla serve` to operator-owned storage outside the bound
// workspace and configures dynamic starter verification and promotion
// resolution.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// workspace-bound `surla serve` instance.
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
		// verification is the startup VerificationConfig when already available.
		verification *controlplane.VerificationConfig
		// resolveVerification dynamically resolves VerificationConfig for the
		// active workspace directory and pinned suite commit.
		resolveVerification func(ctx context.Context, workspaceDir, suiteCommit string) (*controlplane.VerificationConfig, error)
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
)

// configureWorkspace resolves the bound workspace, operator-owned storage
// directories and dynamic
// verification/promotion resolvers for `surla serve`.
func (runtime projectRuntime) configureWorkspace(cfg serveConfig) (workspaceServeConfig, error) {
	var state *project.State
	var err error
	if cfg.projectDir == "" {
		state, err = runtime.discover()
	} else {
		var root string
		root, err = runtime.stateRoot(cfg.projectDir)
		if err == nil {
			state, err = project.Discover(root, cfg.projectDir)
		}
	}
	if err != nil {
		return workspaceServeConfig{}, err
	}
	if state.Record.ID == "" {
		lock, err := state.StartLock(context.Background())
		if err != nil {
			return workspaceServeConfig{}, err
		}
		_, initializeErr := state.Initialize(fmt.Sprintf("uid:%d", os.Geteuid()))
		if err := errors.Join(initializeErr, lock.Close()); err != nil {
			return workspaceServeConfig{}, err
		}
	}
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
			return workspaceServeConfig{}, err
		}
		if err := validatePromotionFile(file); err != nil {
			return workspaceServeConfig{}, err
		}
		file.ReportDir = reports
		if err := validateWorkspaceDirSeparation("release_dir", file.ReleaseDir, protected...); err != nil {
			return workspaceServeConfig{}, err
		}
		ws.promotionTarget = &releaseTarget{cfg: file}
		ws.resolvePromotionTarget = func(context.Context, string) (controlplane.PromotionTarget, error) { return ws.promotionTarget, nil }
	} else if !errors.Is(err, os.ErrNotExist) {
		return workspaceServeConfig{}, err
	}
	if raw, err := state.ReadSettings("verification"); err == nil {
		var file verificationFile
		if err := decodeSettings(raw, &file); err != nil {
			return workspaceServeConfig{}, err
		}
		config, err := verificationConfig(file, ws.runsDir, reports)
		if err != nil {
			return workspaceServeConfig{}, err
		}
		ws.verification = config
		return ws, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return workspaceServeConfig{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitReadTimeout)
	startup, startupErr := starterVerificationAtHEAD(ctx, initial, ws.runsDir, reports)
	cancel()
	ws.verification = startup
	ws.resolveVerification = func(ctx context.Context, active, suiteCommit string) (*controlplane.VerificationConfig, error) {
		if suiteCommit != "" {
			bounded, cancel := context.WithTimeout(ctx, gitReadTimeout)
			defer cancel()
			return starterVerificationAtCommit(bounded, active, suiteCommit, ws.runsDir, reports)
		}
		if startup != nil && active == initial {
			copy := *startup
			return &copy, nil
		}
		if startupErr != nil {
			return nil, fmt.Errorf("automatic starter verification was unavailable at server startup (%w); restart surla after committing the starter, or call init_project in an empty workspace", startupErr)
		}
		return nil, fmt.Errorf("workspace %s has no pinned starter evaluator suite commit", active)
	}
	return ws, nil
}

// loadWorkspacePromotionTarget reads `promoPath`, resolves relative paths
// against baseDir and verifies that `promoPath`, the resolved
// `report_dir`, and `release_dir` do not lie inside initialWorkspaceRoot,
// activeWorkspaceDir, or any protectedDirs.
func loadWorkspacePromotionTarget(
	promoPath, baseDir, initialWorkspaceRoot, activeWorkspaceDir string,
	protectedDirs ...string,
) (controlplane.PromotionTarget, string, error) {
	allProtected := append([]string{initialWorkspaceRoot, activeWorkspaceDir}, protectedDirs...)
	if err := validatePromotionPathSeparation(promoPath, allProtected...); err != nil {
		return nil, "", err
	}
	f, err := decodePromotionFileAtBase(promoPath, baseDir)
	if err != nil {
		return nil, "", err
	}
	physReport, err := abspath.EvalMissing(filepath.Join(filepath.Dir(promoPath), "reports"))
	if err != nil {
		return nil, "", fmt.Errorf("promotion settings %s: %w", promoPath, err)
	}
	physRelease, err := abspath.EvalMissing(f.ReleaseDir)
	if err != nil {
		return nil, "", fmt.Errorf("promotion settings %s: %w", promoPath, err)
	}
	for _, ws := range allProtected {
		if ws == "" {
			continue
		}
		for _, item := range []struct{ field, target string }{
			{"report_dir", physReport},
			{"release_dir", physRelease},
		} {
			inside, err := abspath.Within(ws, item.target)
			if err != nil {
				return nil, "", err
			}
			if inside {
				return nil, "", fmt.Errorf(
					"promotion config %s: %s (%s) must be outside the workspace (%s) so builder agents cannot modify promotion inputs, reports, or release binaries",
					promoPath,
					item.field,
					item.target,
					ws,
				)
			}
		}
	}
	f.ReportDir = physReport
	f.ReleaseDir = physRelease
	return &releaseTarget{cfg: f}, physReport, nil
}

// validatePromotionPathSeparation checks that promoPath itself does not lie
// inside any protected workspace or enclosing Git working tree root.
func validatePromotionPathSeparation(promoPath string, protectedDirs ...string) error {
	physPromo, err := abspath.EvalMissing(promoPath)
	if err != nil {
		return err
	}
	for _, ws := range protectedDirs {
		if ws == "" {
			continue
		}
		inside, err := abspath.Within(ws, physPromo)
		if err != nil {
			return err
		}
		if inside {
			return fmt.Errorf(
				"promotion config %s: config file (%s) must be outside the workspace (%s) so builder agents cannot modify promotion inputs, reports, or release binaries",
				promoPath,
				physPromo,
				ws,
			)
		}
	}
	return nil
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
	for _, field := range []struct{ name, value string }{
		{"release_dir", f.ReleaseDir}, {"suite_id", f.SuiteID},
		{"suite_commit", f.SuiteCommit}, {"environment", f.Environment},
	} {
		if field.value == "" {
			missing = append(missing, field.name)
		}
	}
	if len(f.Binaries) == 0 {
		missing = append(missing, "binaries")
	}
	if len(missing) > 0 {
		return fmt.Errorf("promotion configuration is missing %s", strings.Join(missing, ", "))
	}
	if !verify.IsCommitID(f.SuiteCommit) {
		return fmt.Errorf("suite_commit %q must be a full commit ID (40 or 64 lowercase hex characters)", f.SuiteCommit)
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

// defaultOperatorRoot returns the single deterministic operator-only storage
// root (`<UserConfigDir>/surla/operator`) strictly outside initialWorkspaceRoot
// and any extraDirs, failing if unavailable, inside the workspace grant, or not
// owned by the current effective user with 0o700 permissions.
func defaultOperatorRoot(initialWorkspaceRoot string, extraDirs ...string) (string, error) {
	cfgRoot, err := userConfigDir()
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
		return "", fmt.Errorf("prepare operator storage root %s: %w", phys, err)
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
		return "", err
	}
	return evalPhys, nil
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

// sha256HexString returns the 64-character lowercase hex SHA-256 digest of s.
func sha256HexString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
