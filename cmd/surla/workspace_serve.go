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
		// active workspace directory and pinned suite commit.
		resolveVerification func(ctx context.Context, workspaceDir, suiteCommit string) (*controlplane.VerificationConfig, error)
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

	protectedDirs, err := workspaceProtectedDirs(initialWorkspaceRoot)
	if err != nil {
		return workspaceServeConfig{}, err
	}
	var startupExternalVer *controlplane.VerificationConfig
	var verPath string
	if cfg.verificationPath != "" {
		verPath, err = abspath.EvalFrom(startupBaseDir, cfg.verificationPath)
		if err != nil {
			return workspaceServeConfig{}, fmt.Errorf("resolve -verification-config %s: %w", cfg.verificationPath, err)
		}
		startupExternalVer, err = loadWorkspaceVerificationConfig(verPath, startupBaseDir, protectedDirs...)
		if err != nil {
			return workspaceServeConfig{}, err
		}
	}
	if cfg.cardsDirExplicit {
		explicitCardsDir, err := abspath.EvalFrom(startupBaseDir, cfg.cardsDir)
		if err != nil {
			return workspaceServeConfig{}, fmt.Errorf("resolve -cards-dir %s: %w", cfg.cardsDir, err)
		}
		if err := validateWorkspaceDirSeparation("cards directory", explicitCardsDir, protectedDirs...); err != nil {
			return workspaceServeConfig{}, err
		}
	}
	if cfg.promotionPath != "" {
		promoPath, err := abspath.EvalFrom(startupBaseDir, cfg.promotionPath)
		if err != nil {
			return workspaceServeConfig{}, fmt.Errorf("resolve -promotion-config %s: %w", cfg.promotionPath, err)
		}
		if err := validatePromotionPathSeparation(promoPath, protectedDirs...); err != nil {
			return workspaceServeConfig{}, err
		}
	}

	opRoot, err := defaultOperatorRoot(initialWorkspaceRoot, protectedDirs...)
	if err != nil {
		return workspaceServeConfig{}, err
	}
	wsHash := sha256HexString(initialWorkspaceRoot)
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
	if _, statErr := os.Lstat(importedRoot); statErr == nil {
		if err := validateOperatorPrivateDir(importedRoot); err != nil {
			return workspaceServeConfig{}, fmt.Errorf("validate imported workspace state %s: %w", importedRoot, err)
		}
		hasImported = true
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return workspaceServeConfig{}, fmt.Errorf("stat imported workspace state %s: %w", importedRoot, statErr)
	}
	if !hasImported {
		if err := requireNoUnimportedImplicitLegacyStore(startupBaseDir, initialWorkspaceRoot, cfg, protectedDirs); err != nil {
			return workspaceServeConfig{}, err
		}
	}

	cardsHash := sha256HexString(startupBaseDir + "\x00" + initialWorkspaceRoot)
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
		if _, statErr := os.Lstat(operatorReportsRoot); statErr == nil {
			if err := validateOperatorPrivateDir(operatorReportsRoot); err != nil {
				return workspaceServeConfig{}, fmt.Errorf("validate imported reports root %s: %w", operatorReportsRoot, err)
			}
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return workspaceServeConfig{}, fmt.Errorf("stat imported reports root %s: %w", operatorReportsRoot, statErr)
		}
		defaultReportDir = filepath.Join(operatorReportsRoot, "default")
	} else {
		operatorCardsDir = filepath.Join(operatorProjectDir, "cards-"+cardsHash)
		operatorRunsDir = filepath.Join(operatorProjectDir, "runs")
		defaultReportDir = filepath.Join(operatorProjectDir, "reports")
	}

	startupCardsDir, err := abspath.EvalFrom(startupBaseDir, ".surla/cards")
	if err != nil {
		return workspaceServeConfig{}, fmt.Errorf("resolve default cards directory: %w", err)
	}
	effectivePromotionPath := filepath.Join(operatorProjectDir, "promotion.json")
	hasStartupPromotion := false
	if cfg.promotionPath != "" {
		effectivePromotionPath, err = abspath.EvalFrom(startupBaseDir, cfg.promotionPath)
		if err != nil {
			return workspaceServeConfig{}, fmt.Errorf("resolve -promotion-config %s: %w", cfg.promotionPath, err)
		}
		hasStartupPromotion = true
	} else if _, statErr := os.Lstat(effectivePromotionPath); statErr == nil {
		hasStartupPromotion = true
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return workspaceServeConfig{}, fmt.Errorf("stat promotion config %s: %w", effectivePromotionPath, statErr)
	}

	var startupPromotion controlplane.PromotionTarget
	if hasStartupPromotion {
		pt, _, err := loadWorkspacePromotionTarget(
			effectivePromotionPath,
			startupBaseDir,
			initialWorkspaceRoot,
			initialWorkspaceRoot,
			operatorReportsRoot,
			protectedDirs...,
		)
		if err != nil {
			return workspaceServeConfig{}, err
		}
		startupPromotion = pt
	}
	resolvePromotionTarget := func(_ context.Context, activeWorkspaceDir string) (controlplane.PromotionTarget, error) {
		if cfg.promotionPath == "" {
			if _, statErr := os.Lstat(effectivePromotionPath); errors.Is(statErr, fs.ErrNotExist) {
				return nil, nil
			} else if statErr != nil {
				return nil, gensurla.MakeUnprocessable(fmt.Errorf("stat promotion config %s: %w", effectivePromotionPath, statErr))
			}
		}
		pt, _, err := loadWorkspacePromotionTarget(
			effectivePromotionPath,
			startupBaseDir,
			initialWorkspaceRoot,
			activeWorkspaceDir,
			operatorReportsRoot,
			protectedDirs...,
		)
		if err != nil {
			return nil, gensurla.MakeUnprocessable(err)
		}
		return pt, nil
	}

	startupCardsInside, err := withinAnyDir(startupCardsDir, protectedDirs)
	if err != nil {
		return workspaceServeConfig{}, err
	}
	var cardsDir string
	switch {
	case cfg.cardsDirExplicit:
		cardsDir, err = abspath.EvalFrom(startupBaseDir, cfg.cardsDir)
		if err != nil {
			return workspaceServeConfig{}, fmt.Errorf("resolve -cards-dir %s: %w", cfg.cardsDir, err)
		}
	case !startupCardsInside:
		cardsDir = startupCardsDir
	default:
		cardsDir = operatorCardsDir
		if err := markOperatorPrivateDir(cardsDir); err != nil {
			return workspaceServeConfig{}, err
		}
	}

	initStageDir := filepath.Join(operatorProjectDir, "init-stage")
	evalWorkDir := filepath.Join(operatorProjectDir, "designeval-work")
	if err := markOperatorPrivateDir(evalWorkDir); err != nil {
		return workspaceServeConfig{}, err
	}

	var (
		runsDir             string
		startupVerification *controlplane.VerificationConfig
		resolveVerification func(ctx context.Context, activeWorkspaceDir, suiteCommit string) (*controlplane.VerificationConfig, error)
	)
	if cfg.verificationPath != "" {
		runsDir = startupExternalVer.RunsDir
		startupVerification = startupExternalVer
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
					protectedDirs...,
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
		startupRepDir, err := resolveStarterReportDir(initialWorkspaceRoot)
		if err != nil {
			return workspaceServeConfig{}, err
		}
		var startupVerErr error
		startupCtx, cancel := context.WithTimeout(context.Background(), gitReadTimeout)
		startupVerification, startupVerErr = starterVerificationAtHEAD(
			startupCtx,
			initialWorkspaceRoot,
			operatorRunsDir,
			startupRepDir,
		)
		cancel()
		resolveVerification = func(ctx context.Context, activeWorkspaceDir, suiteCommit string) (*controlplane.VerificationConfig, error) {
			repDir, err := resolveStarterReportDir(activeWorkspaceDir)
			if err != nil {
				return nil, err
			}
			if suiteCommit != "" {
				vCtx, cancel := context.WithTimeout(ctx, gitReadTimeout)
				defer cancel()
				return starterVerificationAtCommit(vCtx, activeWorkspaceDir, suiteCommit, operatorRunsDir, repDir)
			}
			if startupVerification != nil && activeWorkspaceDir == initialWorkspaceRoot {
				cpVer := *startupVerification
				cpVer.Plan.ReportDir = repDir
				return &cpVer, nil
			}
			if startupVerErr != nil {
				return nil, fmt.Errorf(
					"automatic starter verification was unavailable at server startup (%w); if you initialized or fixed and committed the starter repository after starting surla serve, restart surla serve so it pins the committed starter HEAD, or call init_project when starting in an empty workspace",
					startupVerErr,
				)
			}
			return nil, fmt.Errorf("workspace %s has no pinned starter evaluator suite commit", activeWorkspaceDir)
		}
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
// config file, `runs_dir`, and `report_dir` lie strictly outside protectedDirs
// (the bound workspace and its enclosing Git working tree root).
func loadWorkspaceVerificationConfig(
	path, baseDir string,
	protectedDirs ...string,
) (*controlplane.VerificationConfig, error) {
	cfg, err := loadVerificationConfig(path)
	if err != nil || cfg == nil {
		return cfg, err
	}
	runsDir, err := abspath.EvalFrom(baseDir, cfg.RunsDir)
	if err != nil {
		return nil, fmt.Errorf("-verification-config %s: resolve runs_dir: %w", path, err)
	}
	reportDir, err := abspath.EvalFrom(baseDir, cfg.Plan.ReportDir)
	if err != nil {
		return nil, fmt.Errorf("-verification-config %s: resolve report_dir: %w", path, err)
	}
	cfg.RunsDir = runsDir
	cfg.Plan.ReportDir = reportDir
	if err := validateVerificationConfigSeparation(path, cfg, protectedDirs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validateVerificationConfigSeparation verifies that `-verification-config` and
// its `runs_dir` and `report_dir` do not lie inside any protected workspace or
// enclosing Git working tree root.
func validateVerificationConfigSeparation(
	configPath string,
	cfg *controlplane.VerificationConfig,
	protectedDirs ...string,
) error {
	if cfg == nil {
		return nil
	}
	physConfig, err := abspath.EvalMissing(configPath)
	if err != nil {
		return err
	}
	physRuns, err := abspath.EvalMissing(cfg.RunsDir)
	if err != nil {
		return err
	}
	physReports, err := abspath.EvalMissing(cfg.Plan.ReportDir)
	if err != nil {
		return err
	}
	for _, ws := range protectedDirs {
		if ws == "" {
			continue
		}
		for _, item := range []struct{ label, target string }{
			{"verification config", physConfig},
			{"runs_dir", physRuns},
			{"report_dir", physReports},
		} {
			inside, err := abspath.Within(ws, item.target)
			if err != nil {
				return err
			}
			if inside {
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
// `report_dir`, and `release_dir` do not lie inside initialWorkspaceRoot,
// activeWorkspaceDir, or any protectedDirs.
func loadWorkspacePromotionTarget(
	promoPath, baseDir, initialWorkspaceRoot, activeWorkspaceDir, operatorReportsRoot string,
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
	resolvedReportDir, err := verifyrun.ResolveImportedReportDir(
		operatorReportsRoot,
		initialWorkspaceRoot,
		f.ReportDir,
		allProtected...,
	)
	if err != nil {
		return nil, "", fmt.Errorf("-promotion-config %s: %w", promoPath, err)
	}
	physReport, err := abspath.EvalMissing(resolvedReportDir)
	if err != nil {
		return nil, "", fmt.Errorf("-promotion-config %s: %w", promoPath, err)
	}
	physRelease, err := abspath.EvalMissing(f.ReleaseDir)
	if err != nil {
		return nil, "", fmt.Errorf("-promotion-config %s: %w", promoPath, err)
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
	f.ReportDir = joinPreservingRawSegments(baseDir, f.ReportDir)
	f.ReleaseDir = joinPreservingRawSegments(baseDir, f.ReleaseDir)
	return f, nil
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
