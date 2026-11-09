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

	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/verifyrun"
)

var (
	// cardsHashDirPattern matches a direct `cards-<16-lowercase-hex>` store
	// directory inside `operatorProjectDir`.
	cardsHashDirPattern = regexp.MustCompile(`^cards-[0-9a-f]{16}$`)
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
		_ = unix.Close(projectDirFD)
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

	var startupExternalVer *controlplane.VerificationConfig
	var verPath string
	if cfg.verificationPath != "" {
		verPath = resolveAgainstBase(startupBaseDir, cfg.verificationPath)
		startupExternalVer, err = loadWorkspaceVerificationConfig(verPath, startupBaseDir, initialWorkspaceRoot, initialWorkspaceRoot)
		if err != nil {
			return err
		}
	}

	effectivePromotionPath := filepath.Join(operatorProjectDir, "promotion.json")
	hasPromotionConfig := false
	if cfg.promotionPath != "" {
		effectivePromotionPath = resolveAgainstBase(startupBaseDir, cfg.promotionPath)
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
		if err := validatePromotionPathSeparation(effectivePromotionPath, initialWorkspaceRoot, initialWorkspaceRoot); err != nil {
			return err
		}
		promoFile, err := decodePromotionFileAtBase(effectivePromotionPath, startupBaseDir)
		if err != nil {
			return err
		}
		configuredReportDirs = append(configuredReportDirs, promoFile.ReportDir)
		promoReportInsideWS, err = verifyrun.IsWorkspaceReportDir(initialWorkspaceRoot, promoFile.ReportDir)
		if err != nil {
			return fmt.Errorf("-promotion-config %s: %w", effectivePromotionPath, err)
		}
	}

	startupCardsDir := resolveAgainstBase(startupBaseDir, ".surla/cards")
	caseC3 := !cfg.cardsDirExplicit && isWithinOrEqualDir(startupCardsDir, initialWorkspaceRoot)
	starterMode := cfg.verificationPath == ""
	if !caseC3 && !starterMode && !promoReportInsideWS {
		return errors.New("cannot import legacy workspace: no workspace-interior .surla/ store is applicable when both -cards-dir and -verification-config point outside the workspace")
	}

	cardsHash := sha256HexString(startupBaseDir + "\x00" + initialWorkspaceRoot)[:16]
	wsCardsHash := sha256HexString(initialWorkspaceRoot + "\x00" + initialWorkspaceRoot)[:16]
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
			_ = f.Close()
		}
	}()

	for _, storeName := range cardStoreNames {
		storeDir := filepath.Join(operatorProjectDir, storeName)
		info, statErr := os.Lstat(storeDir)
		if errors.Is(statErr, fs.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return fmt.Errorf("stat operator store %s: %w", storeDir, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("operator store %s is not a directory", storeDir)
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

	if starterMode || promoReportInsideWS {
		for _, dirSpec := range []struct {
			path      string
			allowLive bool
		}{
			{filepath.Join(operatorProjectDir, "runs"), true},
			{filepath.Join(operatorProjectDir, "reports"), false},
		} {
			info, statErr := os.Lstat(dirSpec.path)
			if errors.Is(statErr, fs.ErrNotExist) {
				continue
			}
			if statErr != nil {
				return fmt.Errorf("stat operator store %s: %w", dirSpec.path, statErr)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("operator store %s is not a directory", dirSpec.path)
			}
			if err := requireEmptyOperatorStore(dirSpec.path, dirSpec.allowLive); err != nil {
				return err
			}
		}
	}

	wsLegacyCardsDir := filepath.Join(initialWorkspaceRoot, ".surla", "cards")
	stageDir, err := os.MkdirTemp(operatorProjectDir, ".surla-import-stage-*")
	if err != nil {
		return fmt.Errorf("create import staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(stageDir)
		}
	}()
	if err := markOperatorPrivateDir(stageDir); err != nil {
		return err
	}

	for _, storeName := range cardStoreNames {
		var legacySrc string
		if caseC3 {
			switch storeName {
			case "cards-" + cardsHash:
				legacySrc = startupCardsDir
			case "cards-" + wsCardsHash:
				legacySrc = wsLegacyCardsDir
			}
		}
		unlockCards, err := controlplane.PrepareCardSnapshot(legacySrc, filepath.Join(stageDir, storeName))
		if unlockCards != nil {
			defer func() {
				_ = unlockCards()
			}()
		}
		if err != nil {
			return err
		}
	}

	var stagedSelectedCardsDir string
	switch {
	case cfg.cardsDirExplicit:
		stagedSelectedCardsDir = resolveAgainstBase(startupBaseDir, cfg.cardsDir)
	case !isWithinOrEqualDir(startupCardsDir, initialWorkspaceRoot):
		stagedSelectedCardsDir = startupCardsDir
	default:
		stagedSelectedCardsDir = filepath.Join(stageDir, "cards-"+cardsHash)
	}
	probeWorkspaceDir := peekRestoredWorkspaceDir(stagedSelectedCardsDir, initialWorkspaceRoot)
	if starterMode && probeWorkspaceDir != initialWorkspaceRoot {
		configuredReportDirs = append(configuredReportDirs, filepath.Join(probeWorkspaceDir, "reports"))
	}

	if starterMode || promoReportInsideWS {
		var (
			legacyRunsDir           string
			legacyDefaultReportsDir string
		)
		if starterMode {
			legacyRunsDir = filepath.Join(initialWorkspaceRoot, ".surla", "runs")
			legacyDefaultReportsDir = filepath.Join(initialWorkspaceRoot, "reports")
		}
		unlockRuns, err := verifyrun.PrepareSnapshot(
			legacyRunsDir,
			legacyDefaultReportsDir,
			filepath.Join(stageDir, "runs"),
			filepath.Join(stageDir, "reports"),
			filepath.Join(operatorProjectDir, "imported", "reports"),
			initialWorkspaceRoot,
			configuredReportDirs...,
		)
		if unlockRuns != nil {
			defer func() {
				_ = unlockRuns()
			}()
		}
		if err != nil {
			return err
		}
	}

	// Prevalidate the complete post-import configuration before irreversible
	// RenameatxNp publication so a rejected configuration never leaves imported/
	// published on disk.
	if cfg.cardsDirExplicit {
		if err := validateWorkspaceDirSeparation("cards directory", stagedSelectedCardsDir, initialWorkspaceRoot, probeWorkspaceDir); err != nil {
			return err
		}
	}
	if cfg.verificationPath != "" {
		if err := validateVerificationConfigSeparation(verPath, startupExternalVer, initialWorkspaceRoot, probeWorkspaceDir); err != nil {
			return err
		}
	}
	if hasPromotionConfig {
		if _, _, err := loadWorkspacePromotionTarget(
			effectivePromotionPath,
			startupBaseDir,
			initialWorkspaceRoot,
			probeWorkspaceDir,
			filepath.Join(stageDir, "reports"),
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

// hasUnimportedLegacyWorkspaceNotice reports whether `opts` is serving a
// non-imported workspace that still has legacy `.surla/cards`, `.surla/runs`, or
// `reports/` artifacts on disk.
func hasUnimportedLegacyWorkspaceNotice(opts controlplane.Options) bool {
	if opts.WorkspaceDir == "" {
		return false
	}
	if strings.Contains(filepath.ToSlash(opts.CardsDir), "/imported/") ||
		strings.Contains(filepath.ToSlash(opts.RunsDir), "/imported/") {
		return false
	}
	if hasMatchingEntries(filepath.Join(opts.WorkspaceDir, ".surla", "cards"), hex64JSONPattern) {
		return true
	}
	if hasMatchingEntries(filepath.Join(opts.WorkspaceDir, ".surla", "runs"), hex32DirPattern) {
		return true
	}
	return hasMatchingEntries(filepath.Join(opts.WorkspaceDir, "reports"), hex64JSONPattern)
}

// hasMatchingEntries reports whether dir contains any entry whose name matches
// pattern.
func hasMatchingEntries(dir string, pattern *regexp.Regexp) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if pattern.MatchString(entry.Name()) {
			return true
		}
	}
	return false
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
