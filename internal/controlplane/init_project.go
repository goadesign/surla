// This file implements atomic starter-project initialization (`init_project`)
// inside an operator-owned staging directory outside the workspace and its
// descriptor-relative publication and cleanup helpers.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unsafe"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	goa "goa.design/goa/v3/pkg"
	"golang.org/x/mod/module"
	"golang.org/x/sys/unix"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/scaffold"
)

type (
	// foreignStagingError reports that an unpublished staging entry no longer
	// matches the `(dev, ino)` recorded in `workspace.json`.
	foreignStagingError struct {
		msg string
	}
)

const (
	// workspaceGitTimeout bounds git read probes when checking whether a
	// workspace directory already has a committed HEAD.
	workspaceGitTimeout = 10 * time.Second
)

var (
	// initDirNamePattern matches a valid single child directory name for
	// `init_project`.
	initDirNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	// serviceNamePattern matches a valid Goa starter service name.
	serviceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	// hexRunDirPattern matches a 32-character lowercase hex verification run ID.
	hexRunDirPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// projectMarkerNames lists top-level files and directories whose presence in
	// the active workspace refuses `init_project`.
	projectMarkerNames = []string{"go.mod", "design", "gen", "internal", "cmd", "eval"}
)

// InitProject initializes a new Surla starter project inside a direct child
// directory of the server's bound initial workspace root, building the starter
// inside an operator-owned staging directory outside the workspace and
// publishing it atomically via `RenameatxNp(..., RENAME_EXCL)`.
func (c *ControlPlane) InitProject(
	ctx context.Context,
	p *gensurla.InitProjectPayload,
) (res *gensurla.InitProjectResult, retErr error) {
	if _, ok := PrincipalFromContext(ctx); !ok {
		return nil, errors.New("authenticated principal is required")
	}

	ctx, span := otel.Tracer("surla/controlplane").Start(ctx, "surla.controlplane.init_project")
	defer span.End()

	if p == nil || strings.TrimSpace(p.ModulePath) == "" {
		recordInitRefused(span, "invalid_payload")
		return nil, gensurla.MakeUnprocessable(errors.New("module_path is required"))
	}
	modulePath := strings.TrimSpace(p.ModulePath)
	serviceName, err := deriveStarterServiceName(modulePath)
	if err != nil {
		recordInitRefused(span, "invalid_module_path")
		return nil, gensurla.MakeUnprocessable(err)
	}
	targetBase := serviceName
	if p.Dir != nil && *p.Dir != "" {
		targetBase = *p.Dir
	}
	if err := validateInitDirName(targetBase); err != nil {
		recordInitRefused(span, "invalid_dir_name")
		return nil, gensurla.MakeUnprocessable(fmt.Errorf(
			"init_project creates a direct child directory inside the workspace; omit dir to create %q or pass a single child directory name: %w",
			serviceName,
			err,
		))
	}
	if c.initialWorkspaceRoot == "" {
		recordInitRefused(span, "workspace_unavailable")
		return nil, gensurla.MakeNotImplemented(errors.New("no workspace directory is bound to this control plane"))
	}
	if !scaffold.ValidVersion(c.surlaVersion) {
		recordInitRefused(span, "surla_version_unavailable")
		return nil, gensurla.MakeNotImplemented(errors.New(
			"this server has no exact github.com/goadesign/surla module version configured for starter generation; build or install surla from a tagged or pseudo-versioned module release, pass -surla-version to surla init on the CLI, or set controlplane.Options.SurlaVersion in an embedding host",
		))
	}

	c.workspaceMu.Lock()
	defer c.workspaceMu.Unlock()

	initCtx, cancelInit := context.WithCancel(ctx)
	stopOnServerClose := context.AfterFunc(c.serverCtx, cancelInit)
	defer func() {
		stopOnServerClose()
		cancelInit()
	}()
	if err := initCtx.Err(); err != nil {
		return nil, failInitFault(span, "init_project canceled", err)
	}

	if c.activeWorkspaceDir != c.initialWorkspaceRoot {
		recordInitRefused(span, "already_initialized")
		return nil, gensurla.MakeConflict(fmt.Errorf(
			"workspace %s is already initialized at %s",
			c.initialWorkspaceRoot,
			c.activeWorkspaceDir,
		))
	}
	_, hasRecord, err := c.readWorkspaceRecord()
	if err != nil {
		return nil, failInitFault(span, "read workspace.json failed", err)
	}
	if hasRecord {
		recordInitRefused(span, "already_initialized")
		return nil, gensurla.MakeConflict(fmt.Errorf(
			"workspace %s is already initialized at %s",
			c.initialWorkspaceRoot,
			c.activeWorkspaceDir,
		))
	}

	workspaceFD, err := openVerifiedDirFD(c.initialWorkspaceRoot, c.initialWorkspaceDev, c.initialWorkspaceIno)
	if err != nil {
		return nil, failInitFault(span, "open bound workspace failed", err)
	}
	defer func() {
		if closeErr := unix.Close(workspaceFD); closeErr != nil {
			if retErr != nil {
				retErr = errors.Join(retErr, closeErr)
			} else {
				recordWorkspaceCleanupFailure("surla.controlplane.cleanup_init_workspace_fd", closeErr)
			}
		}
	}()

	if err := checkWorkspaceUninitialized(initCtx, workspaceFD, c.initialWorkspaceRoot, targetBase); err != nil {
		if isConflictError(err) {
			recordInitRefused(span, "already_initialized")
			return nil, err
		}
		return nil, failInitFault(span, "check workspace uninitialized failed", err)
	}
	if hasRuns, err := hasVerificationRuns(c.effectiveRunsDir()); err != nil {
		return nil, failInitFault(span, "check verification runs failed", err)
	} else if hasRuns {
		recordInitRefused(span, "already_initialized")
		return nil, gensurla.MakeConflict(errors.New("workspace already has verification runs; cannot re-initialize project"))
	}

	c.mu.RLock()
	hasWorkspaceCards, err := c.cards.hasWorkspaceCardsWithFiles()
	c.mu.RUnlock()
	if err != nil {
		return nil, failInitFault(span, "check workspace cards failed", err)
	}
	if hasWorkspaceCards {
		recordInitRefused(span, "already_initialized")
		return nil, gensurla.MakeConflict(errors.New(
			"workspace already has Change Cards that captured existing design files; cannot re-initialize project",
		))
	}

	targetPath := filepath.Join(c.initialWorkspaceRoot, targetBase)
	if err := validateCardsWorkspaceSeparation(c.cards.dir, c.initialWorkspaceRoot, targetPath); err != nil {
		return nil, failInitFault(span, "validate cards workspace separation failed", err)
	}
	if err := validateRunsDirWorkspaceSeparation(c.effectiveRunsDir(), c.initialWorkspaceRoot, targetPath); err != nil {
		return nil, failInitFault(span, "validate runs workspace separation failed", err)
	}

	stageParentDir, err := resolveInitStageParentDir(c.initialWorkspaceRoot, c.initStageDir, c.initialWorkspaceDev)
	if err != nil {
		return nil, failInitFault(span, "resolve init stage directory failed", err)
	}

	if err := reportProgress(initCtx, 0, 3, "Generating, building, and testing the starter project"); err != nil {
		return nil, failInitFault(span, "init_project canceled", err)
	}
	physCards, err := abspath.Eval(c.cards.dir)
	if err != nil {
		return nil, failInitFault(span, "resolve cards directory failed", err)
	}
	gen := c.makeInitGenerator(filepath.Join(physCards, "designeval"))
	baseRun := c.initRunner
	if baseRun == nil {
		baseRun = scaffold.RunCommand
	}
	run := func(ctx context.Context, dir, logPath string, args ...string) error {
		if gitSubcommand(args) == "init" {
			if err := reportProgress(ctx, 1, 3, "Recording the initial starter Git commit"); err != nil {
				return err
			}
		}
		return baseRun(ctx, dir, logPath, args...)
	}
	builtService, stagePath, suiteCommit, err := scaffold.Prepare(
		initCtx,
		scaffold.Options{
			Dir:          targetPath,
			WorkDir:      stageParentDir,
			Module:       modulePath,
			SurlaVersion: c.surlaVersion,
		},
		run,
		gen,
	)
	if err != nil {
		return nil, failInitFault(span, "prepare starter project failed", err)
	}
	if !commitIDPattern.MatchString(suiteCommit) {
		return nil, failInitFault(
			span,
			"verify starter commit failed",
			errors.Join(
				fmt.Errorf("scaffold.Prepare returned invalid commit ID %q", suiteCommit),
				os.RemoveAll(stagePath),
			),
		)
	}
	if err := reportProgress(initCtx, 2, 3, "Publishing the initialized starter workspace"); err != nil {
		return nil, failInitFault(
			span,
			"init_project canceled",
			errors.Join(err, os.RemoveAll(stagePath)),
		)
	}

	stageParentFD, err := unix.Open(stageParentDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, failInitFault(
			span,
			"open init stage parent failed",
			errors.Join(
				fmt.Errorf("open init stage parent %s: %w", stageParentDir, err),
				os.RemoveAll(stagePath),
			),
		)
	}
	defer func() {
		if closeErr := unix.Close(stageParentFD); closeErr != nil {
			if retErr != nil {
				retErr = errors.Join(retErr, closeErr)
			} else {
				recordWorkspaceCleanupFailure("surla.controlplane.cleanup_init_stage_parent_fd", closeErr)
			}
		}
	}()
	if gotPath, err := fdPath(stageParentFD); err != nil || gotPath != stageParentDir {
		return nil, failInitFault(span, "verify init stage parent path failed", fmt.Errorf("init stage parent %s changed path during initialization", stageParentDir))
	}

	stageName := filepath.Base(stagePath)
	stageFD, err := unix.Openat(stageParentFD, stageName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, failInitFault(span, "open staged project failed", fmt.Errorf("open staged project %s: %w", stagePath, err))
	}
	stageClosed := false
	defer func() {
		if !stageClosed {
			if closeErr := unix.Close(stageFD); closeErr != nil {
				if retErr != nil {
					retErr = errors.Join(retErr, closeErr)
				} else {
					recordWorkspaceCleanupFailure("surla.controlplane.cleanup_init_stage_fd", closeErr)
				}
			}
		}
	}()

	var stageStat, entryStat unix.Stat_t
	if err := unix.Fstat(stageFD, &stageStat); err != nil {
		return nil, failInitFault(span, "stat staged project failed", fmt.Errorf("stat staged project %s: %w", stagePath, err))
	}
	if err := unix.Fstatat(stageParentFD, stageName, &entryStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, failInitFault(span, "stat staged project entry failed", fmt.Errorf("stat staged project entry %s: %w", stagePath, err))
	}
	stageDev := uint64(stageStat.Dev)
	stageIno := stageStat.Ino
	if stageDev != uint64(entryStat.Dev) || stageIno != entryStat.Ino || stageDev != c.initialWorkspaceDev {
		return nil, failInitFault(span, "verify staged project identity failed", fmt.Errorf("staged project %s does not match expected device/inode identity", stagePath))
	}

	if err := syncDirTreeFD(stageFD, stageDev); err != nil {
		stageClosed = true
		return nil, failInitFault(
			span,
			"sync staged project failed",
			errors.Join(
				fmt.Errorf("sync staged project %s: %w", stagePath, err),
				removeOwnedStagingDir(stageParentFD, stageFD, stageParentDir, stageName, stageDev, stageIno),
			),
		)
	}
	if err := unix.Fsync(stageParentFD); err != nil {
		stageClosed = true
		return nil, failInitFault(
			span,
			"sync init stage parent failed",
			errors.Join(
				fmt.Errorf("sync init stage parent %s: %w", stageParentDir, err),
				removeOwnedStagingDir(stageParentFD, stageFD, stageParentDir, stageName, stageDev, stageIno),
			),
		)
	}

	wal := workspaceFile{
		Dir:         targetBase,
		SuiteCommit: suiteCommit,
		StageDir:    stageParentDir,
		Staging:     stageName,
		Dev:         stageDev,
		Ino:         stageIno,
	}
	if err := c.writeWorkspaceRecord(wal); err != nil {
		stageClosed = true
		return nil, failInitFault(
			span,
			"write workspace.json failed",
			errors.Join(
				err,
				c.removeWorkspaceRecord(),
				removeOwnedStagingDir(stageParentFD, stageFD, stageParentDir, stageName, stageDev, stageIno),
			),
		)
	}
	if c.initStepHook != nil {
		c.initStepHook("after_wal")
	}

	if gotWSPath, err := fdPath(workspaceFD); err != nil || gotWSPath != c.initialWorkspaceRoot {
		stageClosed = true
		return nil, failInitFault(
			span,
			"verify bound workspace path failed",
			errors.Join(
				fmt.Errorf("bound workspace directory %s was replaced or removed after startup", c.initialWorkspaceRoot),
				c.removeWorkspaceRecord(),
				removeOwnedStagingDir(stageParentFD, stageFD, stageParentDir, stageName, stageDev, stageIno),
			),
		)
	}
	if err := unix.Fstatat(stageParentFD, stageName, &entryStat, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		uint64(entryStat.Dev) != stageDev || entryStat.Ino != stageIno {
		stageClosed = true
		return nil, failInitFault(
			span,
			"verify staged project before publication failed",
			errors.Join(
				fmt.Errorf("staged project %s changed identity before publication", stagePath),
				c.removeWorkspaceRecord(),
				removeOwnedStagingDir(stageParentFD, stageFD, stageParentDir, stageName, stageDev, stageIno),
			),
		)
	}

	stageClosed = true
	if err := unix.Close(stageFD); err != nil {
		return nil, failInitFault(
			span,
			"close staged project descriptor failed",
			errors.Join(
				err,
				c.removeWorkspaceRecord(),
				removeOwnedStagingDir(stageParentFD, -1, stageParentDir, stageName, stageDev, stageIno),
			),
		)
	}

	if err := unix.RenameatxNp(stageParentFD, stageName, workspaceFD, targetBase, unix.RENAME_EXCL); err != nil {
		cleanErr := errors.Join(
			c.removeWorkspaceRecord(),
			removeOwnedStagingDir(stageParentFD, -1, stageParentDir, stageName, stageDev, stageIno),
		)
		if errors.Is(err, unix.EEXIST) {
			conflictErr := gensurla.MakeConflict(fmt.Errorf("target directory %q already exists in workspace %s", targetBase, c.initialWorkspaceRoot))
			if cleanErr == nil {
				recordInitRefused(span, "already_initialized")
				return nil, conflictErr
			}
			return nil, failInitFault(span, "cleanup after target conflict failed", errors.Join(conflictErr, cleanErr))
		}
		return nil, failInitFault(
			span,
			"publish project failed",
			errors.Join(fmt.Errorf("publish project into %s: %w", targetPath, err), cleanErr),
		)
	}

	syncErr := errors.Join(unix.Fsync(workspaceFD), unix.Fsync(stageParentFD))
	if c.initStepHook != nil {
		c.initStepHook("after_rename")
	}
	if err := verifyPublishedTarget(workspaceFD, targetBase, stageDev, stageIno); err != nil {
		return nil, failInitFault(
			span,
			"verify published target failed",
			errors.Join(err, c.removeWorkspaceRecord(), syncErr),
		)
	}

	c.activeWorkspaceDir = targetPath
	c.boundWorkspaceDev = stageDev
	c.boundWorkspaceIno = stageIno
	c.boundSuiteCommit = suiteCommit
	if syncErr != nil {
		return nil, failInitFault(
			span,
			"sync published project directory failed",
			fmt.Errorf(
				"project %s was published and bound in this server, and workspace.json retains its recovery record, but directory sync failed so restart surla serve to verify crash durability: %w",
				targetPath,
				syncErr,
			),
		)
	}
	if err := c.writeWorkspaceRecord(workspaceFile{
		Dir:         targetBase,
		SuiteCommit: suiteCommit,
		Dev:         stageDev,
		Ino:         stageIno,
	}); err != nil {
		recordWorkspaceCleanupFailure("surla.controlplane.finalize_init_project", err)
	}

	span.AddEvent(
		"project_initialized",
		trace.WithAttributes(
			attribute.String("surla.init.service_name", builtService),
			attribute.String("surla.init.suite_commit", suiteCommit),
		),
	)
	return &gensurla.InitProjectResult{
		Dir:         targetPath,
		ModulePath:  modulePath,
		ServiceName: builtService,
		Commit:      suiteCommit,
	}, nil
}

// Error returns the foreign staging replacement message.
func (e foreignStagingError) Error() string {
	return e.msg
}

// makeInitGenerator returns a fresh scaffold.Generator for one `InitProject`
// call without closing or sharing `c.evaluator`.
func (c *ControlPlane) makeInitGenerator(defaultTempParent string) scaffold.Generator {
	if c.newGenerator != nil {
		return c.newGenerator()
	}
	return designeval.NewEvaluatorInDir(defaultTempParent)
}

// deriveStarterServiceName validates modulePath and returns its derived Goa
// starter service name.
func deriveStarterServiceName(modulePath string) (string, error) {
	if err := module.CheckPath(modulePath); err != nil {
		return "", fmt.Errorf("module_path %q: %w", modulePath, err)
	}
	name := path.Base(modulePath)
	if !serviceNamePattern.MatchString(name) || !token.IsIdentifier(name) || name == "main" {
		return "", fmt.Errorf(
			"module_path %q must end in a service name of lowercase letters and digits starting with a letter, other than a Go keyword or main, such as example.com/shop",
			modulePath,
		)
	}
	return name, nil
}

// validateInitDirName checks that name is a single direct child directory name
// inside the workspace and does not use reserved `.surla` prefixes.
func validateInitDirName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid directory name %q", name)
	}
	if strings.HasPrefix(name, ".surla") {
		return fmt.Errorf("directory name %q is reserved by Surla", name)
	}
	if !initDirNamePattern.MatchString(name) {
		return fmt.Errorf("directory name %q must start with an ASCII letter or digit and contain only letters, digits, '.', '_', or '-'", name)
	}
	return nil
}

// openVerifiedDirFD opens dir with O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC
// and verifies that both its descriptor Stat and kernel path match dir and
// (expectedDev, expectedIno).
func openVerifiedDirFD(dir string, expectedDev, expectedIno uint64) (int, error) {
	if expectedDev == 0 || expectedIno == 0 {
		return -1, fmt.Errorf("bound workspace directory %s has no recorded device/inode identity", dir)
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("bound workspace directory %s was replaced or removed after startup: %w", dir, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return -1, errors.Join(
			fmt.Errorf("bound workspace directory %s was replaced or removed after startup: %w", dir, err),
			unix.Close(fd),
		)
	}
	gotPath, pathErr := fdPath(fd)
	if pathErr != nil || gotPath != dir || uint64(st.Dev) != expectedDev || st.Ino != expectedIno {
		return -1, errors.Join(
			fmt.Errorf("bound workspace directory %s was replaced or removed after startup", dir),
			unix.Close(fd),
		)
	}
	return fd, nil
}

// checkWorkspaceUninitialized verifies that workspaceFD has no project markers,
// no existing targetBase entry, and no committed Git HEAD.
func checkWorkspaceUninitialized(ctx context.Context, workspaceFD int, workspaceDir, targetBase string) error {
	for _, marker := range projectMarkerNames {
		var st unix.Stat_t
		if err := unix.Fstatat(workspaceFD, marker, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
			return gensurla.MakeConflict(fmt.Errorf(
				"workspace %s already contains %q; init_project requires an uninitialized workspace",
				workspaceDir,
				marker,
			))
		} else if !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("check workspace %s for %q: %w", workspaceDir, marker, err)
		}
	}
	var st unix.Stat_t
	if err := unix.Fstatat(workspaceFD, targetBase, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return gensurla.MakeConflict(fmt.Errorf(
			"target directory %q already exists in workspace %s",
			targetBase,
			workspaceDir,
		))
	} else if !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("check target directory %q in %s: %w", targetBase, workspaceDir, err)
	}
	if hasCommittedGitHEAD(ctx, workspaceDir) {
		return gensurla.MakeConflict(fmt.Errorf(
			"workspace %s is already inside a Git repository with a committed HEAD",
			workspaceDir,
		))
	}
	return nil
}

// hasCommittedGitHEAD reports whether dir is inside a Git repository that
// already has a verifiable `HEAD^{commit}`.
func hasCommittedGitHEAD(ctx context.Context, dir string) bool {
	gitCtx, cancel := context.WithTimeout(ctx, workspaceGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(
		gitCtx,
		"git",
		"-C",
		dir,
		"--no-optional-locks",
		"--no-replace-objects",
		"--no-lazy-fetch",
		"rev-parse",
		"--verify",
		"HEAD^{commit}",
	)
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C")
	return cmd.Run() == nil
}

// hasVerificationRuns reports whether runsDir already contains any `<32-hex>`
// verification run directory.
func hasVerificationRuns(runsDir string) (bool, error) {
	if runsDir == "" {
		return false, nil
	}
	entries, err := os.ReadDir(runsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read runs directory %s: %w", runsDir, err)
	}
	for _, entry := range entries {
		if hexRunDirPattern.MatchString(entry.Name()) {
			return true, nil
		}
	}
	return false, nil
}

// resolveInitStageParentDir selects an operator-owned staging parent directory
// outside initialWorkspaceRoot on the same filesystem device (`wsDev`),
// validating that the directory is owned by the current effective UID with mode
// 0700.
func resolveInitStageParentDir(initialWorkspaceRoot, configuredStageDir string, wsDev uint64) (string, error) {
	var candidates []string
	if configuredStageDir != "" {
		candidates = append(candidates, configuredStageDir)
	}
	candidates = append(
		candidates,
		filepath.Join(os.TempDir(), fmt.Sprintf("surla-operator-%d", os.Getuid()), "init-stage"),
		filepath.Join(filepath.Dir(initialWorkspaceRoot), fmt.Sprintf(".surla-operator-%d", os.Getuid()), "init-stage"),
	)
	var failures []error
	for _, cand := range candidates {
		phys, err := abspath.EvalMissing(cand)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if isWithinOrEqualDir(phys, initialWorkspaceRoot) {
			failures = append(failures, fmt.Errorf("init stage directory %s must be outside workspace %s", phys, initialWorkspaceRoot))
			continue
		}
		if err := os.MkdirAll(phys, 0o700); err != nil {
			failures = append(failures, err)
			continue
		}
		evalPhys, err := abspath.Eval(phys)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if isWithinOrEqualDir(evalPhys, initialWorkspaceRoot) {
			failures = append(failures, fmt.Errorf("init stage directory %s must be outside workspace %s", evalPhys, initialWorkspaceRoot))
			continue
		}
		if err := validateInitStageParentDir(evalPhys, wsDev, initialWorkspaceRoot); err != nil {
			failures = append(failures, err)
			continue
		}
		return evalPhys, nil
	}
	return "", fmt.Errorf("cannot find protected init staging storage on the workspace filesystem: %w", errors.Join(failures...))
}

// validateInitStageParentDir verifies that dir is a non-symlink regular
// directory owned by the current effective UID with mode 0700, accessible for
// read/write/search, and residing on wsDev.
func validateInitStageParentDir(dir string, wsDev uint64, workspaceRoot string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat init stage directory %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("init stage directory %s must be a non-symlink directory", dir)
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open init stage directory %s: %w", dir, err)
	}
	var st unix.Stat_t
	statErr := unix.Fstat(fd, &st)
	closeErr := unix.Close(fd)
	if err := errors.Join(statErr, closeErr); err != nil {
		return fmt.Errorf("stat init stage directory %s: %w", dir, err)
	}
	if st.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("init stage directory %s is owned by uid %d, want effective uid %d", dir, st.Uid, os.Geteuid())
	}
	if perm := st.Mode & 0o777; perm != 0o700 {
		return fmt.Errorf("init stage directory %s has mode %04o, want 0700", dir, perm)
	}
	if err := unix.Faccessat(unix.AT_FDCWD, dir, unix.R_OK|unix.W_OK|unix.X_OK, unix.AT_EACCESS); err != nil {
		return fmt.Errorf("init stage directory %s is not accessible: %w", dir, err)
	}
	if uint64(st.Dev) != wsDev {
		return fmt.Errorf("init stage directory %s is on a different filesystem from workspace %s", dir, workspaceRoot)
	}
	return nil
}

// verifyPublishedTarget performs a non-destructive open-descriptor check that
// `(workspaceFD, targetBase)` still has `(expectedDev, expectedIno)` immediately
// after `RenameatxNp` succeeds. It never moves or deletes any directory inside
// the workspace.
func verifyPublishedTarget(workspaceFD int, targetBase string, expectedDev, expectedIno uint64) error {
	pubFD, err := unix.Openat(workspaceFD, targetBase, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("target directory %q was moved or replaced concurrently after publication: %w", targetBase, err)
	}
	var pubStat unix.Stat_t
	statErr := unix.Fstat(pubFD, &pubStat)
	closeErr := unix.Close(pubFD)
	if err := errors.Join(statErr, closeErr); err != nil {
		return fmt.Errorf("target directory %q was moved or replaced concurrently after publication: %w", targetBase, err)
	}
	if uint64(pubStat.Dev) != expectedDev || pubStat.Ino != expectedIno {
		return fmt.Errorf("target directory %q was moved or replaced concurrently after publication", targetBase)
	}
	return nil
}

// removeOwnedStagingDir removes an unpublished staging directory `(stageParentFD, stageName)`
// exclusively through its open directory descriptor after verifying that its
// `(dev, ino)` matches `(expectedDev, expectedIno)` and its kernel path matches
// `filepath.Join(stageParentDir, stageName)`. It is used only before
// `RenameatxNp` succeeds or during startup recovery cleanup.
func removeOwnedStagingDir(
	stageParentFD, stageFD int,
	stageParentDir, stageName string,
	expectedDev, expectedIno uint64,
) (err error) {
	ownFD := stageFD
	if ownFD < 0 {
		var entryStat unix.Stat_t
		if err := unix.Fstatat(stageParentFD, stageName, &entryStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				return nil
			}
			return err
		}
		if entryStat.Mode&unix.S_IFMT != unix.S_IFDIR ||
			uint64(entryStat.Dev) != expectedDev ||
			entryStat.Ino != expectedIno {
			return foreignStagingError{
				msg: fmt.Sprintf(
					"staging entry %q was replaced externally (dev=%d ino=%d, expected dev=%d ino=%d)",
					stageName,
					entryStat.Dev,
					entryStat.Ino,
					expectedDev,
					expectedIno,
				),
			}
		}
		fd, err := unix.Openat(stageParentFD, stageName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				return nil
			}
			if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
				return foreignStagingError{
					msg: fmt.Sprintf("staging entry %q was replaced with a non-directory", stageName),
				}
			}
			return err
		}
		ownFD = fd
	}
	defer func() {
		err = errors.Join(err, unix.Close(ownFD))
	}()

	var st unix.Stat_t
	if err := unix.Fstat(ownFD, &st); err != nil {
		return err
	}
	if uint64(st.Dev) != expectedDev || st.Ino != expectedIno {
		return foreignStagingError{
			msg: fmt.Sprintf(
				"staging entry %q was replaced externally (dev=%d ino=%d, expected dev=%d ino=%d)",
				stageName,
				st.Dev,
				st.Ino,
				expectedDev,
				expectedIno,
			),
		}
	}
	expectedPath := filepath.Join(stageParentDir, stageName)
	if gotPath, err := fdPath(ownFD); err != nil || gotPath != expectedPath {
		return foreignStagingError{
			msg: fmt.Sprintf("staging entry %q is no longer at %s", stageName, expectedPath),
		}
	}

	if err := emptyOwnedDirFD(ownFD, expectedDev); err != nil {
		return err
	}
	if err := unix.Unlinkat(stageParentFD, stageName, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return unix.Fsync(stageParentFD)
}

// readDirEntriesFD opens an independent directory stream on dirFD via
// `Openat(dirFD, ".", O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC)` so reading or
// closing the stream neither shares nor advances dirFD's open file description.
func readDirEntriesFD(dirFD int, name string) ([]os.DirEntry, error) {
	iterFD, err := unix.Openat(dirFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	dirFile := os.NewFile(uintptr(iterFD), name)
	entries, readErr := dirFile.ReadDir(-1)
	closeErr := dirFile.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	return entries, nil
}

// emptyOwnedDirFD recursively unlinks all entries inside dirFD using only
// descriptor-relative operations (`Openat` with `O_NOFOLLOW` and `Unlinkat`),
// verifying that every child resides on `expectedDev`.
func emptyOwnedDirFD(dirFD int, expectedDev uint64) error {
	entries, err := readDirEntriesFD(dirFD, "surla-stage-dir")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		var childStat unix.Stat_t
		if err := unix.Fstatat(dirFD, name, &childStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return err
		}
		if uint64(childStat.Dev) != expectedDev {
			return fmt.Errorf("staging child %q crossed filesystem device boundary", name)
		}
		if childStat.Mode&unix.S_IFMT == unix.S_IFDIR {
			childFD, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			emptyErr := emptyOwnedDirFD(childFD, expectedDev)
			closeChildErr := unix.Close(childFD)
			if err := errors.Join(emptyErr, closeChildErr); err != nil {
				return err
			}
			if err := unix.Unlinkat(dirFD, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
				return err
			}
			continue
		}
		if err := unix.Unlinkat(dirFD, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
	}
	return nil
}

// syncDirTreeFD recursively fsyncs all regular files and child directories in
// post-order under dirFD on expectedDev, then fsyncs dirFD itself.
func syncDirTreeFD(dirFD int, expectedDev uint64) error {
	entries, err := readDirEntriesFD(dirFD, "surla-sync-dir")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		var childStat unix.Stat_t
		if err := unix.Fstatat(dirFD, name, &childStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if uint64(childStat.Dev) != expectedDev {
			return fmt.Errorf("staged entry %q crossed filesystem device boundary", name)
		}
		switch childStat.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			childFD, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			syncErr := syncDirTreeFD(childFD, expectedDev)
			closeChildErr := unix.Close(childFD)
			if err := errors.Join(syncErr, closeChildErr); err != nil {
				return err
			}
		case unix.S_IFREG:
			fileFD, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			syncErr := unix.Fsync(fileFD)
			closeFileErr := unix.Close(fileFD)
			if err := errors.Join(syncErr, closeFileErr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("staged entry %q is not a regular file or directory", name)
		}
	}
	return unix.Fsync(dirFD)
}

// fdPath returns the kernel path of open descriptor fd via F_GETPATH on Darwin.
func fdPath(fd int) (string, error) {
	buf := make([]byte, unix.PathMax)
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETPATH, int(uintptr(unsafe.Pointer(&buf[0])))); err != nil {
		return "", err
	}
	return unix.ByteSliceToString(buf), nil
}

// recordInitRefused records a bounded refusal event on span without marking the
// span failed.
func recordInitRefused(span trace.Span, reason string) {
	span.AddEvent(
		"project_initialization_refused",
		trace.WithAttributes(attribute.String("surla.init.refusal_reason", reason)),
	)
}

// failInitFault records the full raw error on span and returns a caller-safe
// Goa Fault ServiceError (HTTP 500 / MCP internal error) that omits private
// operator paths, command log filenames, and raw command output tails.
func failInitFault(span trace.Span, msg string, err error) error {
	span.RecordError(err)
	span.SetStatus(codes.Error, msg)
	var stepErr *scaffold.StepError
	if errors.As(err, &stepErr) && stepErr != nil {
		cmdSummary := initStepSummary(stepErr.Args)
		if gitSubcommand(stepErr.Args) == "commit" {
			return goa.Fault(
				"%s: command %q in %q failed during starter initialization (verify that git user.name, user.email, and any commit signing settings are configured for this user); inspect the server trace span for diagnostic output",
				msg,
				cmdSummary,
				stepErr.Module,
			)
		}
		return goa.Fault(
			"%s: command %q in %q failed during starter initialization; inspect the server trace span for diagnostic output",
			msg,
			cmdSummary,
			stepErr.Module,
		)
	}
	return goa.Fault("%s; inspect the server trace span for diagnostic details", msg)
}

// gitSubcommand returns the git subcommand verb (such as "init", "add",
// "commit", or "rev-parse") when args invokes git, or an empty string.
func gitSubcommand(args []string) string {
	if len(args) == 0 || args[0] != "git" {
		return ""
	}
	for i := 1; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		if strings.HasPrefix(args[i], "-") {
			continue
		}
		return args[i]
	}
	return ""
}

// initStepSummary returns a caller-safe command summary for a scaffold step
// without internal git override flags.
func initStepSummary(args []string) string {
	if sub := gitSubcommand(args); sub != "" {
		return "git " + sub
	}
	return strings.Join(args, " ")
}

// isConflictError reports whether err is a Goa conflict ServiceError.
func isConflictError(err error) bool {
	if se, ok := err.(*goa.ServiceError); ok {
		return se != nil && se.GoaErrorName() == "conflict"
	}
	return false
}

// contextBackgroundForCleanup returns a background context for post-durability
// cleanup spans.
func contextBackgroundForCleanup() context.Context {
	return context.Background()
}
