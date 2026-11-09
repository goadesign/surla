// This file wires `surla serve` to operator-owned storage outside the bound
// workspace and configures dynamic starter verification and promotion
// resolution.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
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
		// active workspace directory.
		resolveVerification func(ctx context.Context, workspaceDir string) (*controlplane.VerificationConfig, error)
		// promotionTarget is the startup PromotionTarget when already available.
		promotionTarget controlplane.PromotionTarget
		// resolvePromotionTarget dynamically resolves PromotionTarget for the
		// active workspace directory.
		resolvePromotionTarget func(ctx context.Context, workspaceDir string) (controlplane.PromotionTarget, error)
		// closeProjectLock releases the shared lock on <operatorProjectDir>/import.lock.
		closeProjectLock func() error
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

// configureServeWorkspace resolves the bound workspace, operator-owned storage
// directories, optional `-import-legacy-workspace` cutover, and dynamic
// verification/promotion resolvers for `surla serve`.
func configureServeWorkspace(cfg serveConfig) (ws workspaceServeConfig, err error) {
	if cfg.projectSet && cfg.verificationPath != "" {
		return workspaceServeConfig{}, errors.New("-project and -verification-config cannot be used together")
	}
	wd, err := os.Getwd()
	if err != nil {
		return workspaceServeConfig{}, err
	}
	startupBaseDir, err := abspath.Eval(wd)
	if err != nil {
		return workspaceServeConfig{}, err
	}
	initialWorkspaceRoot := startupBaseDir
	if cfg.projectSet {
		if cfg.projectDir == "" {
			return workspaceServeConfig{}, errors.New("-project requires a directory")
		}
		info, err := os.Stat(cfg.projectDir)
		if err != nil {
			return workspaceServeConfig{}, fmt.Errorf("read -project %s: %w", cfg.projectDir, err)
		}
		if !info.IsDir() {
			return workspaceServeConfig{}, fmt.Errorf("-project %s is not a directory", cfg.projectDir)
		}
		initialWorkspaceRoot, err = abspath.Eval(cfg.projectDir)
		if err != nil {
			return workspaceServeConfig{}, fmt.Errorf("resolve -project %s: %w", cfg.projectDir, err)
		}
	}

	if cfg.cardsDirExplicit {
		explicitCardsDir := resolveAgainstBase(startupBaseDir, cfg.cardsDir)
		if err := validateWorkspaceDirSeparation("cards directory", explicitCardsDir, initialWorkspaceRoot, initialWorkspaceRoot); err != nil {
			return workspaceServeConfig{}, err
		}
	}
	var startupExternalVer *controlplane.VerificationConfig
	var verPath string
	if cfg.verificationPath != "" {
		verPath = resolveAgainstBase(startupBaseDir, cfg.verificationPath)
		startupExternalVer, err = loadWorkspaceVerificationConfig(verPath, startupBaseDir, initialWorkspaceRoot, initialWorkspaceRoot)
		if err != nil {
			return workspaceServeConfig{}, err
		}
	}
	if cfg.promotionPath != "" {
		promoPath := resolveAgainstBase(startupBaseDir, cfg.promotionPath)
		if err := validatePromotionPathSeparation(promoPath, initialWorkspaceRoot, initialWorkspaceRoot); err != nil {
			return workspaceServeConfig{}, err
		}
	}

	opRoot, err := defaultOperatorRoot(startupBaseDir, initialWorkspaceRoot)
	if err != nil {
		return workspaceServeConfig{}, err
	}
	wsHash := sha256HexString(initialWorkspaceRoot)[:16]
	operatorProjectDir := filepath.Join(opRoot, "project-"+wsHash)
	if err := markOperatorPrivateDir(operatorProjectDir); err != nil {
		return workspaceServeConfig{}, fmt.Errorf("create operator project directory: %w", err)
	}

	closeLock, err := acquireProjectImportLock(operatorProjectDir, startupBaseDir, initialWorkspaceRoot, cfg)
	if err != nil {
		return workspaceServeConfig{}, err
	}
	defer func() {
		if err != nil && closeLock != nil {
			err = errors.Join(err, closeLock())
		}
	}()

	importedRoot := filepath.Join(operatorProjectDir, "imported")
	hasImported := false
	if info, statErr := os.Lstat(importedRoot); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return workspaceServeConfig{}, fmt.Errorf("imported workspace state %s is not a directory", importedRoot)
		}
		hasImported = true
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return workspaceServeConfig{}, fmt.Errorf("stat imported workspace state %s: %w", importedRoot, statErr)
	}

	cardsHash := sha256HexString(startupBaseDir + "\x00" + initialWorkspaceRoot)[:16]
	var (
		operatorCardsDir    string
		operatorRunsDir     string
		operatorReportsRoot string
		defaultReportDir    string
	)
	if hasImported {
		operatorCardsDir = filepath.Join(importedRoot, "cards-"+cardsHash)
		operatorRunsDir = filepath.Join(importedRoot, "runs")
		operatorReportsRoot = filepath.Join(importedRoot, "reports")
		defaultReportDir = filepath.Join(operatorReportsRoot, "default")
	} else {
		operatorCardsDir = filepath.Join(operatorProjectDir, "cards-"+cardsHash)
		operatorRunsDir = filepath.Join(operatorProjectDir, "runs")
		defaultReportDir = filepath.Join(operatorProjectDir, "reports")
	}

	startupCardsDir := resolveAgainstBase(startupBaseDir, ".surla/cards")
	var cardsDir string
	switch {
	case cfg.cardsDirExplicit:
		cardsDir = resolveAgainstBase(startupBaseDir, cfg.cardsDir)
	case !isWithinOrEqualDir(startupCardsDir, initialWorkspaceRoot):
		cardsDir = startupCardsDir
	default:
		cardsDir = operatorCardsDir
		if err := markOperatorPrivateDir(cardsDir); err != nil {
			return workspaceServeConfig{}, err
		}
	}

	initStageDir := filepath.Join(operatorProjectDir, "init-stage")
	if err := markOperatorPrivateDir(initStageDir); err != nil {
		return workspaceServeConfig{}, err
	}
	evalWorkDir := filepath.Join(operatorProjectDir, "designeval-work")
	if err := markOperatorPrivateDir(evalWorkDir); err != nil {
		return workspaceServeConfig{}, err
	}

	probeWorkspaceDir := peekRestoredWorkspaceDir(cardsDir, initialWorkspaceRoot)
	if cfg.cardsDirExplicit {
		if err := validateWorkspaceDirSeparation("cards directory", cardsDir, initialWorkspaceRoot, probeWorkspaceDir); err != nil {
			return workspaceServeConfig{}, err
		}
	}
	effectivePromotionPath := filepath.Join(operatorProjectDir, "promotion.json")
	if cfg.promotionPath != "" {
		effectivePromotionPath = resolveAgainstBase(startupBaseDir, cfg.promotionPath)
	}

	var startupPromotion controlplane.PromotionTarget
	if cfg.promotionPath != "" || regularFileOrSymlinkExists(effectivePromotionPath) {
		pt, _, err := loadWorkspacePromotionTarget(
			effectivePromotionPath,
			startupBaseDir,
			initialWorkspaceRoot,
			probeWorkspaceDir,
			operatorReportsRoot,
		)
		if err != nil {
			return workspaceServeConfig{}, err
		}
		startupPromotion = pt
	}
	resolvePromotionTarget := func(_ context.Context, activeWorkspaceDir string) (controlplane.PromotionTarget, error) {
		if cfg.promotionPath == "" {
			if _, err := os.Lstat(effectivePromotionPath); errors.Is(err, fs.ErrNotExist) {
				return nil, nil
			}
		}
		pt, _, err := loadWorkspacePromotionTarget(
			effectivePromotionPath,
			startupBaseDir,
			initialWorkspaceRoot,
			activeWorkspaceDir,
			operatorReportsRoot,
		)
		if err != nil {
			return nil, gensurla.MakeUnprocessable(err)
		}
		return pt, nil
	}

	var (
		runsDir             string
		startupVerification *controlplane.VerificationConfig
		resolveVerification func(ctx context.Context, activeWorkspaceDir string) (*controlplane.VerificationConfig, error)
	)
	if cfg.verificationPath != "" {
		if err := validateVerificationConfigSeparation(verPath, startupExternalVer, initialWorkspaceRoot, probeWorkspaceDir); err != nil {
			return workspaceServeConfig{}, err
		}
		runsDir = startupExternalVer.RunsDir
		startupVerification = startupExternalVer
		resolveVerification = func(_ context.Context, activeWorkspaceDir string) (*controlplane.VerificationConfig, error) {
			v, err := loadWorkspaceVerificationConfig(verPath, startupBaseDir, initialWorkspaceRoot, activeWorkspaceDir)
			if err != nil {
				return nil, err
			}
			if v.RunsDir != startupExternalVer.RunsDir {
				return nil, fmt.Errorf(
					"-verification-config %s changed runs_dir from %s to %s; restart surla serve to change runs_dir",
					verPath,
					startupExternalVer.RunsDir,
					v.RunsDir,
				)
			}
			return v, nil
		}
	} else {
		if err := markOperatorPrivateDir(operatorRunsDir); err != nil {
			return workspaceServeConfig{}, err
		}
		if err := markOperatorPrivateDir(defaultReportDir); err != nil {
			return workspaceServeConfig{}, err
		}
		runsDir = operatorRunsDir
		resolveStarterReportDir := func(activeWorkspaceDir string) (string, error) {
			if _, err := os.Lstat(effectivePromotionPath); err == nil {
				_, repDir, loadErr := loadWorkspacePromotionTarget(
					effectivePromotionPath,
					startupBaseDir,
					initialWorkspaceRoot,
					activeWorkspaceDir,
					operatorReportsRoot,
				)
				if loadErr != nil {
					return "", loadErr
				}
				return repDir, nil
			} else if !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
			return defaultReportDir, nil
		}
		resolveVerification = func(ctx context.Context, activeWorkspaceDir string) (*controlplane.VerificationConfig, error) {
			repDir, err := resolveStarterReportDir(activeWorkspaceDir)
			if err != nil {
				return nil, err
			}
			vCtx, cancel := context.WithTimeout(ctx, gitReadTimeout)
			defer cancel()
			return starterVerificationConfig(vCtx, activeWorkspaceDir, operatorRunsDir, repDir)
		}
		startupCtx, cancel := context.WithTimeout(context.Background(), gitReadTimeout)
		if v, probeErr := resolveVerification(startupCtx, probeWorkspaceDir); probeErr == nil {
			startupVerification = v
		}
		cancel()
	}

	return workspaceServeConfig{
		workspaceDir:           initialWorkspaceRoot,
		cardsDir:               cardsDir,
		initStageDir:           initStageDir,
		evalWorkDir:            evalWorkDir,
		runsDir:                runsDir,
		verification:           startupVerification,
		resolveVerification:    resolveVerification,
		promotionTarget:        startupPromotion,
		resolvePromotionTarget: resolvePromotionTarget,
		closeProjectLock:       closeLock,
	}, nil
}

// loadWorkspaceVerificationConfig reads `-verification-config`, resolves its
// relative `runs_dir` and `report_dir` against baseDir, and verifies that the
// config file, `runs_dir`, and `report_dir` lie strictly outside both
// initialWorkspaceRoot and activeWorkspaceDir.
func loadWorkspaceVerificationConfig(
	path, baseDir, initialWorkspaceRoot, activeWorkspaceDir string,
) (*controlplane.VerificationConfig, error) {
	cfg, err := loadVerificationConfig(path)
	if err != nil || cfg == nil {
		return cfg, err
	}
	cfg.RunsDir = resolveAgainstBase(baseDir, cfg.RunsDir)
	cfg.Plan.ReportDir = resolveAgainstBase(baseDir, cfg.Plan.ReportDir)
	if err := validateVerificationConfigSeparation(path, cfg, initialWorkspaceRoot, activeWorkspaceDir); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validateVerificationConfigSeparation verifies that `-verification-config` and
// its `runs_dir` and `report_dir` lie strictly outside initialWorkspaceRoot and
// activeWorkspaceDir.
func validateVerificationConfigSeparation(
	configPath string,
	cfg *controlplane.VerificationConfig,
	initialWorkspaceRoot, activeWorkspaceDir string,
) error {
	if cfg == nil {
		return nil
	}
	physConfig, err := evalExistingOrMissingPath(configPath)
	if err != nil {
		return err
	}
	physRuns, err := evalExistingOrMissingPath(cfg.RunsDir)
	if err != nil {
		return err
	}
	physReports, err := evalExistingOrMissingPath(cfg.Plan.ReportDir)
	if err != nil {
		return err
	}
	for _, ws := range []string{initialWorkspaceRoot, activeWorkspaceDir} {
		if ws == "" {
			continue
		}
		for _, item := range []struct{ label, target string }{
			{"verification config", physConfig},
			{"runs_dir", physRuns},
			{"report_dir", physReports},
		} {
			if isWithinOrEqualDir(item.target, ws) {
				return fmt.Errorf(
					"-verification-config %s: %s (%s) must be outside the workspace (%s) so builder agents cannot modify evaluator runs or reports",
					configPath,
					item.label,
					item.target,
					ws,
				)
			}
		}
	}
	return nil
}

// loadWorkspacePromotionTarget reads `promoPath`, resolves relative paths
// against baseDir, maps `report_dir` through `verifyrun.ResolveImportedReportDir`
// when imported reports exist, and verifies that `promoPath`, the resolved
// `report_dir`, and `release_dir` lie strictly outside initialWorkspaceRoot and
// activeWorkspaceDir.
func loadWorkspacePromotionTarget(
	promoPath, baseDir, initialWorkspaceRoot, activeWorkspaceDir, operatorReportsRoot string,
) (controlplane.PromotionTarget, string, error) {
	if err := validatePromotionPathSeparation(promoPath, initialWorkspaceRoot, activeWorkspaceDir); err != nil {
		return nil, "", err
	}
	f, err := decodePromotionFileAtBase(promoPath, baseDir)
	if err != nil {
		return nil, "", err
	}
	resolvedReportDir, err := verifyrun.ResolveImportedReportDir(operatorReportsRoot, initialWorkspaceRoot, f.ReportDir)
	if err != nil {
		return nil, "", fmt.Errorf("-promotion-config %s: %w", promoPath, err)
	}
	physReport, err := evalExistingOrMissingPath(resolvedReportDir)
	if err != nil {
		return nil, "", fmt.Errorf("-promotion-config %s: %w", promoPath, err)
	}
	physRelease, err := evalExistingOrMissingPath(f.ReleaseDir)
	if err != nil {
		return nil, "", fmt.Errorf("-promotion-config %s: %w", promoPath, err)
	}
	for _, ws := range []string{initialWorkspaceRoot, activeWorkspaceDir} {
		if ws == "" {
			continue
		}
		for _, item := range []struct{ field, target string }{
			{"report_dir", physReport},
			{"release_dir", physRelease},
		} {
			if isWithinOrEqualDir(item.target, ws) {
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

// validatePromotionPathSeparation checks that promoPath itself lies outside
// initialWorkspaceRoot and activeWorkspaceDir.
func validatePromotionPathSeparation(promoPath, initialWorkspaceRoot, activeWorkspaceDir string) error {
	physPromo, err := evalExistingOrMissingPath(promoPath)
	if err != nil {
		return err
	}
	for _, ws := range []string{initialWorkspaceRoot, activeWorkspaceDir} {
		if ws != "" && isWithinOrEqualDir(physPromo, ws) {
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
		return promotionFile{}, fmt.Errorf("read -promotion-config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f promotionFile
	if err := dec.Decode(&f); err != nil {
		return promotionFile{}, fmt.Errorf("decode -promotion-config %s: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return promotionFile{}, fmt.Errorf("decode -promotion-config %s: data after the configuration", path)
	}
	var missing []string
	for _, field := range []struct{ name, value string }{
		{"report_dir", f.ReportDir}, {"release_dir", f.ReleaseDir}, {"suite_id", f.SuiteID},
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
		return promotionFile{}, fmt.Errorf("-promotion-config %s is missing %s", path, strings.Join(missing, ", "))
	}
	if !verify.IsCommitID(f.SuiteCommit) {
		return promotionFile{}, fmt.Errorf("-promotion-config %s: suite_commit %q must be a full commit ID (40 or 64 lowercase hex characters)", path, f.SuiteCommit)
	}
	seen := make(map[string]bool, len(f.Binaries))
	for _, name := range f.Binaries {
		if err := verify.ValidateArtifactName(name); err != nil {
			return promotionFile{}, fmt.Errorf("-promotion-config %s: %w", path, err)
		}
		if seen[name] {
			return promotionFile{}, fmt.Errorf("-promotion-config %s: binary %q is listed twice", path, name)
		}
		seen[name] = true
	}
	f.ReportDir = resolveAgainstBase(baseDir, f.ReportDir)
	f.ReleaseDir = resolveAgainstBase(baseDir, f.ReleaseDir)
	return f, nil
}

// defaultOperatorRoot returns the first writable operator-only storage root
// strictly outside startupBaseDir, initialWorkspaceRoot, and any extraDirs.
func defaultOperatorRoot(startupBaseDir, initialWorkspaceRoot string, extraDirs ...string) (string, error) {
	var candidates []string
	if cfgRoot, err := userConfigDir(); err == nil && strings.TrimSpace(cfgRoot) != "" {
		candidates = append(candidates, filepath.Join(cfgRoot, "surla", "operator"))
	}
	candidates = append(
		candidates,
		filepath.Join(os.TempDir(), fmt.Sprintf("surla-operator-%d", os.Getuid())),
		filepath.Join(filepath.Dir(initialWorkspaceRoot), fmt.Sprintf(".surla-operator-%d", os.Getuid())),
	)
	excluded := append([]string{startupBaseDir, initialWorkspaceRoot}, extraDirs...)
	var failures []error
	for _, cand := range candidates {
		phys, err := evalExistingOrMissingPath(cand)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if overlapsAnyDir(phys, excluded) {
			failures = append(failures, fmt.Errorf("operator root %s overlaps workspace", phys))
			continue
		}
		if err := markOperatorPrivateDir(phys); err != nil {
			failures = append(failures, err)
			continue
		}
		evalPhys, err := abspath.Eval(phys)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if overlapsAnyDir(evalPhys, excluded) {
			failures = append(failures, fmt.Errorf("operator root %s overlaps workspace", evalPhys))
			continue
		}
		if err := unix.Faccessat(unix.AT_FDCWD, evalPhys, unix.R_OK|unix.W_OK|unix.X_OK, unix.AT_EACCESS); err != nil {
			failures = append(failures, err)
			continue
		}
		return evalPhys, nil
	}
	return "", fmt.Errorf("cannot find operator storage root outside workspace: %w", errors.Join(failures...))
}

// overlapsAnyDir reports whether dir lies within or equals any directory in
// dirs.
func overlapsAnyDir(dir string, dirs []string) bool {
	for _, d := range dirs {
		if d != "" && isWithinOrEqualDir(dir, d) {
			return true
		}
	}
	return false
}

// peekRestoredWorkspaceDir returns `<initialWorkspaceRoot>/<dir>` when
// `<cardsDir>/workspace.json` already records an existing child directory, so
// startup validation and status probing inspect the restored project directory.
func peekRestoredWorkspaceDir(cardsDir, initialWorkspaceRoot string) string {
	raw, err := os.ReadFile(filepath.Join(cardsDir, "workspace.json"))
	if err != nil {
		return initialWorkspaceRoot
	}
	var wf struct {
		Dir     string `json:"dir"`
		Staging string `json:"staging"`
	}
	if err := json.Unmarshal(raw, &wf); err != nil || wf.Dir == "" || strings.ContainsAny(wf.Dir, `/\`) {
		return initialWorkspaceRoot
	}
	target := filepath.Join(initialWorkspaceRoot, wf.Dir)
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir() {
		return target
	}
	return initialWorkspaceRoot
}

// markOperatorPrivateDir creates dir with mode 0o700 and writes the private
// Surla directory marker and extended attribute.
func markOperatorPrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = unix.Setxattr(dir, operatorPrivateXattr, []byte(operatorPrivateDirValue), 0)
	markerPath := filepath.Join(dir, operatorPrivateMarker)
	f, err := os.OpenFile(markerPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// validateWorkspaceDirSeparation checks that targetDir lies strictly outside
// initialWorkspaceRoot and activeWorkspaceDir.
func validateWorkspaceDirSeparation(label, targetDir, initialWorkspaceRoot, activeWorkspaceDir string) error {
	physTarget, err := evalExistingOrMissingPath(targetDir)
	if err != nil {
		return err
	}
	for _, ws := range []string{initialWorkspaceRoot, activeWorkspaceDir} {
		if ws != "" && isWithinOrEqualDir(physTarget, ws) {
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

// resolveAgainstBase resolves p against baseDir when p is relative.
func resolveAgainstBase(baseDir, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(baseDir, p)
}

// evalExistingOrMissingPath resolves the deepest existing ancestor of p via
// `abspath.Eval` and joins any remaining non-existent suffix components cleanly.
func evalExistingOrMissingPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	cur := filepath.Clean(abs)
	var suffix []string
	for {
		resolved, evalErr := abspath.Eval(cur)
		if evalErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(evalErr, fs.ErrNotExist) {
			return "", evalErr
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", evalErr
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
}

// isWithinOrEqualDir reports whether child equals parent or is nested inside
// parent.
func isWithinOrEqualDir(child, parent string) bool {
	if child == "" || parent == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// regularFileOrSymlinkExists reports whether path exists on disk via Lstat.
func regularFileOrSymlinkExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// sha256HexString returns the 64-character lowercase hex SHA-256 digest of s.
func sha256HexString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
