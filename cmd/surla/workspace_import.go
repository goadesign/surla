// This file implements the one-time `-import-legacy-workspace` snapshot import
// and the shared operator-project import lock for `surla serve`.
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/verifyrun"
)

var (
	// cardsHashDirPattern matches a direct `cards-<64-lowercase-hex>` store
	// directory inside `operatorProjectDir`.
	cardsHashDirPattern = regexp.MustCompile(`^cards-[0-9a-f]{64}$`)
	// hex64JSONPattern matches a `<64-lowercase-hex>.json` card or report file.
	hex64JSONPattern = regexp.MustCompile(`^[0-9a-f]{64}\.json$`)
	// hex32DirPattern matches a `<32-lowercase-hex>` verification run directory.
	hex32DirPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// acquireProjectImportLock acquires <operatorProjectDir>/import.lock, running
// the one-time legacy workspace import under LOCK_EX before downgrading to
// LOCK_SH when cfg.importLegacyWorkspace is set, and returns a release closure
// held for the lifetime of the control plane.
func acquireProjectImportLock(
	operatorProjectDir, startupBaseDir, initialWorkspaceRoot string,
	cfg serveConfig,
) (func() error, error) {
	lockPath := filepath.Join(operatorProjectDir, "import.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open operator project lock %s: %w", lockPath, err)
	}
	info, err := lockFile.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("operator project lock %s is not a regular file", lockPath)
	}
	if err != nil {
		return nil, errors.Join(err, lockFile.Close())
	}

	if cfg.importLegacyWorkspace {
		if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			closeErr := lockFile.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, errors.Join(
					fmt.Errorf(
						"operator project %s has an active server or import in progress; stop running surla serve processes before running -import-legacy-workspace",
						operatorProjectDir,
					),
					closeErr,
				)
			}
			return nil, errors.Join(fmt.Errorf("lock operator project %s: %w", operatorProjectDir, err), closeErr)
		}
		if err := importLegacyWorkspace(operatorProjectDir, startupBaseDir, initialWorkspaceRoot, cfg); err != nil {
			return nil, errors.Join(err, lockFile.Close())
		}
	}

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		closeErr := lockFile.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(
				fmt.Errorf(
					"operator project %s has a legacy workspace import in progress; wait for -import-legacy-workspace to finish and retry",
					operatorProjectDir,
				),
				closeErr,
			)
		}
		return nil, errors.Join(fmt.Errorf("lock operator project %s: %w", operatorProjectDir, err), closeErr)
	}
	return lockFile.Close, nil
}

// importLegacyWorkspace stages and atomically publishes `<operatorProjectDir>/imported`
// from legacy workspace `.surla/cards`, `.surla/runs`, and `reports/` stores
// after verifying that all destination stores are record-empty and all legacy
// writers are stopped.
func importLegacyWorkspace(
	operatorProjectDir, startupBaseDir, initialWorkspaceRoot string,
	cfg serveConfig,
) (err error) {
	projectDirFD, err := unix.Open(operatorProjectDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open operator project directory %s: %w", operatorProjectDir, err)
	}
	defer func() {
		err = errors.Join(err, unix.Close(projectDirFD))
	}()

	var st unix.Stat_t
	if err := unix.Fstatat(projectDirFD, "imported", &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return fmt.Errorf(
			"legacy workspace import has already been completed (%s); remove -import-legacy-workspace for normal startup",
			filepath.Join(operatorProjectDir, "imported"),
		)
	} else if !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("check imported workspace state in %s: %w", operatorProjectDir, err)
	}

	protectedDirs, err := workspaceProtectedDirs(initialWorkspaceRoot)
	if err != nil {
		return err
	}
	var startupExternalVer *controlplane.VerificationConfig
	var verPath string
	if cfg.verificationPath != "" {
		verPath, err = abspath.EvalFrom(startupBaseDir, cfg.verificationPath)
		if err != nil {
			return fmt.Errorf("resolve -verification-config %s: %w", cfg.verificationPath, err)
		}
		startupExternalVer, err = loadWorkspaceVerificationConfig(verPath, startupBaseDir, protectedDirs...)
		if err != nil {
			return err
		}
	}

	effectivePromotionPath := filepath.Join(operatorProjectDir, "promotion.json")
	hasPromotionConfig := false
	if cfg.promotionPath != "" {
		effectivePromotionPath, err = abspath.EvalFrom(startupBaseDir, cfg.promotionPath)
		if err != nil {
			return fmt.Errorf("resolve -promotion-config %s: %w", cfg.promotionPath, err)
		}
		hasPromotionConfig = true
	} else if _, statErr := os.Lstat(effectivePromotionPath); statErr == nil {
		hasPromotionConfig = true
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("stat promotion config %s: %w", effectivePromotionPath, statErr)
	}
	var (
		configuredReportDirs []string
		promoReportInsideWS  bool
	)
	if hasPromotionConfig {
		if err := validatePromotionPathSeparation(effectivePromotionPath, protectedDirs...); err != nil {
			return err
		}
		promoFile, err := decodePromotionFileAtBase(effectivePromotionPath, startupBaseDir)
		if err != nil {
			return err
		}
		configuredReportDirs = append(configuredReportDirs, promoFile.ReportDir)
		promoReportInsideWS, err = verifyrun.IsWorkspaceReportDir(initialWorkspaceRoot, promoFile.ReportDir, protectedDirs...)
		if err != nil {
			return fmt.Errorf("-promotion-config %s: %w", effectivePromotionPath, err)
		}
	}

	startupCardsDir, err := abspath.EvalFrom(startupBaseDir, ".surla/cards")
	if err != nil {
		return fmt.Errorf("resolve default cards directory: %w", err)
	}
	wsLegacyCardsDir := filepath.Join(initialWorkspaceRoot, ".surla", "cards")
	startupCardsInside, err := withinAnyDir(startupCardsDir, protectedDirs)
	if err != nil {
		return err
	}
	implicitCardsInsideProtected := !cfg.cardsDirExplicit && startupCardsInside
	var hasLegacyWSCards bool
	if !cfg.cardsDirExplicit {
		hasLegacyWSCards, err = hasLegacyHexJSONRecords(wsLegacyCardsDir)
		if err != nil {
			return err
		}
	}
	hasLegacyRuns, err := hasAcceptedLegacyRuns(filepath.Join(initialWorkspaceRoot, ".surla", "runs"))
	if err != nil {
		return err
	}
	hasLegacyReports, err := hasLegacyHexJSONRecords(filepath.Join(initialWorkspaceRoot, "reports"))
	if err != nil {
		return err
	}
	hasLegacyWSRunsOrReports := hasLegacyRuns || hasLegacyReports
	starterMode := cfg.verificationPath == ""
	importRunsAndReports := starterMode || promoReportInsideWS || hasLegacyWSRunsOrReports
	if !implicitCardsInsideProtected && !hasLegacyWSCards && !importRunsAndReports {
		return errors.New("cannot import legacy workspace: no workspace-interior .surla/ store is applicable when both -cards-dir and -verification-config point outside the workspace")
	}

	cardsHash := sha256HexString(startupBaseDir + "\x00" + initialWorkspaceRoot)
	wsCardsHash := sha256HexString(initialWorkspaceRoot + "\x00" + initialWorkspaceRoot)
	cardStoreSet := map[string]bool{
		"cards-" + cardsHash:   true,
		"cards-" + wsCardsHash: true,
	}
	entries, err := os.ReadDir(operatorProjectDir)
	if err != nil {
		return fmt.Errorf("read operator project directory %s: %w", operatorProjectDir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".surla-import-stage-") {
			if rmErr := os.RemoveAll(filepath.Join(operatorProjectDir, name)); rmErr != nil {
				return fmt.Errorf("remove leftover import stage %s: %w", name, rmErr)
			}
			continue
		}
		if !strings.HasPrefix(name, "cards-") {
			continue
		}
		if entry.Type()&fs.ModeSymlink != 0 || !entry.IsDir() || !cardsHashDirPattern.MatchString(name) {
			return fmt.Errorf("invalid operator cards store entry %s in %s", name, operatorProjectDir)
		}
		cardStoreSet[name] = true
	}
	cardStoreNames := make([]string, 0, len(cardStoreSet))
	for name := range cardStoreSet {
		cardStoreNames = append(cardStoreNames, name)
	}
	slices.Sort(cardStoreNames)

	var heldLocks []*os.File
	defer func() {
		for _, f := range heldLocks {
			err = errors.Join(err, f.Close())
		}
	}()

	for _, storeName := range cardStoreNames {
		storeDir := filepath.Join(operatorProjectDir, storeName)
		if _, statErr := os.Lstat(storeDir); errors.Is(statErr, fs.ErrNotExist) {
			continue
		} else if statErr != nil {
			return fmt.Errorf("stat operator store %s: %w", storeDir, statErr)
		}
		if err := validateOperatorPrivateDir(storeDir); err != nil {
			return fmt.Errorf("validate operator store %s: %w", storeDir, err)
		}
		lockFile, lockErr := openAndLockStoreFile(
			filepath.Join(storeDir, "lock"),
			os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK,
			syscall.LOCK_EX|syscall.LOCK_NB,
			fmt.Sprintf("operator cards directory %s has an active server process; stop running surla serve processes before running -import-legacy-workspace", storeDir),
		)
		if lockErr != nil {
			return lockErr
		}
		heldLocks = append(heldLocks, lockFile)
		if err := requireEmptyOperatorStore(storeDir, false); err != nil {
			return err
		}
	}

	if importRunsAndReports {
		for _, dirSpec := range []struct {
			path      string
			allowLive bool
		}{
			{filepath.Join(operatorProjectDir, "runs"), true},
			{filepath.Join(operatorProjectDir, "reports"), false},
		} {
			if _, statErr := os.Lstat(dirSpec.path); errors.Is(statErr, fs.ErrNotExist) {
				continue
			} else if statErr != nil {
				return fmt.Errorf("stat operator store %s: %w", dirSpec.path, statErr)
			}
			if err := validateOperatorPrivateDir(dirSpec.path); err != nil {
				return fmt.Errorf("validate operator store %s: %w", dirSpec.path, err)
			}
			if err := requireEmptyOperatorStore(dirSpec.path, dirSpec.allowLive); err != nil {
				return err
			}
		}
	}

	stageDir, err := os.MkdirTemp(operatorProjectDir, ".surla-import-stage-*")
	if err != nil {
		return fmt.Errorf("create import staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			err = errors.Join(err, os.RemoveAll(stageDir))
		}
	}()
	if err := markOperatorPrivateDir(stageDir); err != nil {
		return err
	}

	for _, storeName := range cardStoreNames {
		var legacySrc string
		switch storeName {
		case "cards-" + cardsHash:
			if implicitCardsInsideProtected {
				legacySrc = startupCardsDir
			} else if !cfg.cardsDirExplicit && cardsHash == wsCardsHash {
				legacySrc = wsLegacyCardsDir
			}
		case "cards-" + wsCardsHash:
			if !cfg.cardsDirExplicit {
				legacySrc = wsLegacyCardsDir
			}
		}
		stagedStoreDir := filepath.Join(stageDir, storeName)
		unlockCards, snapErr := controlplane.PrepareCardSnapshot(legacySrc, stagedStoreDir)
		if unlockCards != nil {
			defer func() {
				err = errors.Join(err, unlockCards())
			}()
		}
		if snapErr != nil {
			return snapErr
		}
		if err := markOperatorPrivateDir(stagedStoreDir); err != nil {
			return err
		}
	}

	var stagedSelectedCardsDir string
	switch {
	case cfg.cardsDirExplicit:
		stagedSelectedCardsDir, err = abspath.EvalFrom(startupBaseDir, cfg.cardsDir)
		if err != nil {
			return fmt.Errorf("resolve -cards-dir %s: %w", cfg.cardsDir, err)
		}
	case !startupCardsInside:
		stagedSelectedCardsDir = startupCardsDir
	default:
		stagedSelectedCardsDir = filepath.Join(stageDir, "cards-"+cardsHash)
	}

	if importRunsAndReports {
		var (
			legacyRunsDir           string
			legacyDefaultReportsDir string
		)
		if starterMode || hasLegacyWSRunsOrReports {
			legacyRunsDir = filepath.Join(initialWorkspaceRoot, ".surla", "runs")
			legacyDefaultReportsDir = filepath.Join(initialWorkspaceRoot, "reports")
		}
		stagedRunsDir := filepath.Join(stageDir, "runs")
		stagedReportsDir := filepath.Join(stageDir, "reports")
		unlockRuns, snapErr := verifyrun.PrepareSnapshot(
			legacyRunsDir,
			legacyDefaultReportsDir,
			stagedRunsDir,
			stagedReportsDir,
			filepath.Join(operatorProjectDir, "imported", "reports"),
			initialWorkspaceRoot,
			protectedDirs,
			configuredReportDirs...,
		)
		if unlockRuns != nil {
			defer func() {
				err = errors.Join(err, unlockRuns())
			}()
		}
		if snapErr != nil {
			return snapErr
		}
		if err := markOperatorPrivateDir(stagedRunsDir); err != nil {
			return err
		}
		if err := markOperatorPrivateDir(stagedReportsDir); err != nil {
			return err
		}
	}

	// Prevalidate the complete post-import configuration before irreversible
	// RenameatxNp publication so a rejected configuration never leaves imported/
	// published on disk.
	if cfg.cardsDirExplicit {
		if err := validateWorkspaceDirSeparation("cards directory", stagedSelectedCardsDir, protectedDirs...); err != nil {
			return err
		}
	}
	if cfg.verificationPath != "" {
		if err := validateVerificationConfigSeparation(verPath, startupExternalVer, protectedDirs...); err != nil {
			return err
		}
	}
	if hasPromotionConfig {
		if _, _, err := loadWorkspacePromotionTarget(
			effectivePromotionPath,
			startupBaseDir,
			initialWorkspaceRoot,
			initialWorkspaceRoot,
			filepath.Join(stageDir, "reports"),
			protectedDirs...,
		); err != nil {
			return err
		}
	}

	if err := syncDirectory(stageDir); err != nil {
		return fmt.Errorf("sync staged workspace import: %w", err)
	}
	if err := unix.RenameatxNp(projectDirFD, filepath.Base(stageDir), projectDirFD, "imported", unix.RENAME_EXCL); err != nil {
		return fmt.Errorf("publish imported workspace state: %w", err)
	}
	published = true
	if err := unix.Fsync(projectDirFD); err != nil {
		return fmt.Errorf(
			"legacy workspace import was published to %s, but directory sync failed; restart surla serve without -import-legacy-workspace to verify durability: %w",
			filepath.Join(operatorProjectDir, "imported"),
			err,
		)
	}
	return nil
}

// openAndLockStoreFile opens a regular lock file without following symlinks and
// acquires the requested non-blocking flock.
func openAndLockStoreFile(path string, openFlags, flockFlags int, busyMessage string) (*os.File, error) {
	f, err := os.OpenFile(path, openFlags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open store lock %s: %w", path, err)
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("store lock %s is not a regular file", path)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if err := syscall.Flock(int(f.Fd()), flockFlags); err != nil {
		closeErr := f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(errors.New(busyMessage), closeErr)
		}
		return nil, errors.Join(fmt.Errorf("lock %s: %w", path, err), closeErr)
	}
	return f, nil
}

// requireEmptyOperatorStore verifies that dir contains no persisted records
// before `-import-legacy-workspace` switches global project routing.
func requireEmptyOperatorStore(dir string, allowEmptyLive bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read operator store %s: %w", dir, err)
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "lock", operatorPrivateMarker:
			continue
		case "live":
			if allowEmptyLive && entry.Type()&fs.ModeSymlink == 0 && entry.IsDir() {
				liveEntries, liveErr := os.ReadDir(filepath.Join(dir, "live"))
				if liveErr != nil {
					return fmt.Errorf("read operator live directory %s: %w", filepath.Join(dir, "live"), liveErr)
				}
				emptyLive := true
				for _, le := range liveEntries {
					if le.Name() != operatorPrivateMarker {
						emptyLive = false
						break
					}
				}
				if emptyLive {
					continue
				}
			}
		}
		return fmt.Errorf(
			"cannot import legacy workspace: operator store %s already contains existing records; -import-legacy-workspace requires an empty destination store",
			dir,
		)
	}
	return nil
}

// requireNoUnimportedImplicitLegacyStore verifies before normal (non-imported)
// startup that no selected implicit legacy store (.surla/cards, .surla/runs, or
// reports/) contains durable records. When an unimported implicit legacy store
// holds records, startup fails before creating divergent state in the new
// operator store and instructs the operator to run -import-legacy-workspace.
func requireNoUnimportedImplicitLegacyStore(
	startupBaseDir, initialWorkspaceRoot string,
	cfg serveConfig,
	protectedDirs []string,
) error {
	if !cfg.cardsDirExplicit {
		startupCardsDir, err := abspath.EvalFrom(startupBaseDir, ".surla/cards")
		if err != nil {
			return fmt.Errorf("resolve default cards directory: %w", err)
		}
		inside, err := withinAnyDir(startupCardsDir, protectedDirs)
		if err != nil {
			return err
		}
		if inside {
			hasCards, err := hasLegacyHexJSONRecords(startupCardsDir)
			if err != nil {
				return err
			}
			if hasCards {
				return fmt.Errorf(
					"legacy workspace state found in %s; run surla serve -import-legacy-workspace with writers stopped to import existing cards, verification runs, and reports into protected operator storage before starting surla serve",
					startupCardsDir,
				)
			}
			wsLegacyCardsDir := filepath.Join(initialWorkspaceRoot, ".surla", "cards")
			if wsLegacyCardsDir != startupCardsDir {
				hasWSCards, err := hasLegacyHexJSONRecords(wsLegacyCardsDir)
				if err != nil {
					return err
				}
				if hasWSCards {
					return fmt.Errorf(
						"legacy workspace state found in %s; run surla serve -import-legacy-workspace with writers stopped to import existing cards, verification runs, and reports into protected operator storage before starting surla serve",
						wsLegacyCardsDir,
					)
				}
			}
		}
	}
	if cfg.verificationPath == "" {
		wsLegacyRunsDir := filepath.Join(initialWorkspaceRoot, ".surla", "runs")
		hasRuns, err := hasAcceptedLegacyRuns(wsLegacyRunsDir)
		if err != nil {
			return err
		}
		if hasRuns {
			return fmt.Errorf(
				"legacy workspace state found in %s; run surla serve -import-legacy-workspace with writers stopped to import existing cards, verification runs, and reports into protected operator storage before starting surla serve",
				wsLegacyRunsDir,
			)
		}
		wsLegacyReportsDir := filepath.Join(initialWorkspaceRoot, "reports")
		hasReports, err := hasLegacyHexJSONRecords(wsLegacyReportsDir)
		if err != nil {
			return err
		}
		if hasReports {
			return fmt.Errorf(
				"legacy workspace state found in %s; run surla serve -import-legacy-workspace with writers stopped to import existing cards, verification runs, and reports into protected operator storage before starting surla serve",
				wsLegacyReportsDir,
			)
		}
	}
	return nil
}

// hasLegacyHexJSONRecords reports whether dir contains any `<64-hex>.json`
// record, returning (false, nil) only when dir does not exist or contains no
// matching entries and returning any non-ErrNotExist filesystem error.
func hasLegacyHexJSONRecords(dir string) (bool, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat legacy store %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("legacy store %s is not a regular directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("read legacy store %s: %w", dir, err)
	}
	for _, entry := range entries {
		if hex64JSONPattern.MatchString(entry.Name()) {
			return true, nil
		}
	}
	return false, nil
}

// hasAcceptedLegacyRuns reports whether dir contains any `<32-hex>` run
// directory with a `request.json` entry, ignoring unaccepted `<32-hex>` run
// directories where `request.json` is absent and returning any non-ErrNotExist
// filesystem error.
func hasAcceptedLegacyRuns(dir string) (bool, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat legacy runs directory %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("legacy runs directory %s is not a regular directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("read legacy runs directory %s: %w", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !hex32DirPattern.MatchString(name) {
			continue
		}
		runDir := filepath.Join(dir, name)
		runInfo, err := os.Lstat(runDir)
		if err != nil {
			return false, fmt.Errorf("stat legacy run %s: %w", runDir, err)
		}
		if runInfo.Mode()&os.ModeSymlink != 0 || !runInfo.IsDir() {
			return false, fmt.Errorf("legacy run entry %s is not a regular directory", runDir)
		}
		reqPath := filepath.Join(runDir, "request.json")
		_, reqErr := os.Lstat(reqPath)
		if errors.Is(reqErr, fs.ErrNotExist) {
			continue
		}
		if reqErr != nil {
			return false, fmt.Errorf("stat legacy run request %s: %w", reqPath, reqErr)
		}
		return true, nil
	}
	return false, nil
}

// syncDirectory opens dir without following symlinks and flushes its directory
// entries to disk.
func syncDirectory(dir string) error {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	return errors.Join(unix.Fsync(fd), unix.Close(fd))
}
