//go:build linux

package candidate

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

type (
	// jailMode selects the Landlock and Seccomp ruleset for one jailed invocation.
	jailMode string

	// jailConfig is the configuration sent from the parent to the self-exec
	// launcher over FD 3 before Landlock, Seccomp, and execve run.
	jailConfig struct {
		// Mode selects the sandbox ruleset ("exec_no_ports", "exec_with_ports", or "build").
		Mode jailMode `json:"mode"`
		// NoScope forces the ABI 4-5 fallback path without Landlock IPC scopes.
		NoScope bool `json:"no_scope"`
		// CheckSandboxProbe runs the one-shot child ptrace/pidfd/process_vm_readv probe.
		CheckSandboxProbe bool `json:"check_sandbox_probe"`
		// Binary is the absolute path of the executable to start.
		Binary string `json:"binary"`
		// Args are the command-line arguments after Binary.
		Args []string `json:"args"`
		// Dir is the working directory the launcher chdirs into before Landlock.
		Dir string `json:"dir"`
		// Env is the complete environment passed to syscall.Exec.
		Env []string `json:"env"`
		// StateDir is the writable state directory for Exec and Output.
		StateDir string `json:"state_dir"`
		// SourceDir is the read-only source root for Build and BuildTest.
		SourceDir string `json:"source_dir"`
		// GOROOT is the read/execute Go toolchain root for Build and BuildTest.
		GOROOT string `json:"goroot"`
		// ModCache is the read-only module cache for Build and BuildTest.
		ModCache string `json:"modcache"`
		// WorkDir is the writable work directory for Build and BuildTest.
		WorkDir string `json:"workdir"`
		// OutDir is the resolved output directory for Build and BuildTest.
		OutDir string `json:"outdir"`
		// ListenPort is the loopback TCP port the process may bind and listen on.
		ListenPort int `json:"listen_port"`
		// ConnectPorts are the loopback TCP ports the process may connect to.
		ConnectPorts []int `json:"connect_ports"`
	}

	// landlockRulesetAttrV6 mirrors struct landlock_ruleset_attr through ABI v6.
	landlockRulesetAttrV6 struct {
		HandledAccessFS  uint64
		HandledAccessNet uint64
		Scoped           uint64
	}

	// landlockPathBeneathAttr mirrors struct landlock_path_beneath_attr.
	landlockPathBeneathAttr struct {
		AllowedAccess uint64
		ParentFD      int32
	}

	// landlockNetPortAttr mirrors struct landlock_net_port_attr.
	landlockNetPortAttr struct {
		AllowedAccess uint64
		Port          uint64
	}
)

const (
	// surlaJailArgv0 is the argv[0] sentinel that triggers runJailLauncher in init.
	surlaJailArgv0 = "surla-linux-jail-launcher"

	// jailMode values for Exec, Output, Build, and BuildTest.
	modeExecNoPorts   jailMode = "exec_no_ports"
	modeExecWithPorts jailMode = "exec_with_ports"
	modeBuild         jailMode = "build"

	// Landlock syscall flags, rule types, filesystem/network access bits, and scopes.
	landlockCreateRulesetVersion = 1 << 0
	landlockRulePathBeneath      = 1
	landlockRuleNetPort          = 2

	landlockAccessFSExecute    = 1 << 0
	landlockAccessFSWriteFile  = 1 << 1
	landlockAccessFSReadFile   = 1 << 2
	landlockAccessFSReadDir    = 1 << 3
	landlockAccessFSRemoveDir  = 1 << 4
	landlockAccessFSRemoveFile = 1 << 5
	landlockAccessFSMakeChar   = 1 << 6
	landlockAccessFSMakeDir    = 1 << 7
	landlockAccessFSMakeReg    = 1 << 8
	landlockAccessFSMakeSock   = 1 << 9
	landlockAccessFSMakeFifo   = 1 << 10
	landlockAccessFSMakeBlock  = 1 << 11
	landlockAccessFSMakeSym    = 1 << 12
	landlockAccessFSRefer      = 1 << 13
	landlockAccessFSTruncate   = 1 << 14
	landlockAccessFSIoctlDev   = 1 << 15

	landlockAccessNetBindTCP    = 1 << 0
	landlockAccessNetConnectTCP = 1 << 1

	landlockScopeAbstractUnixSocket = 1 << 0
	landlockScopeSignal             = 1 << 1

	// closeRangeCloexec marks descriptors in [first, last] as O_CLOEXEC.
	closeRangeCloexec = 1 << 2

	// oPath opens a descriptor for Landlock path-beneath rules without reading it.
	oPath = 02000000

	// checkSandboxCanaryWord is read from the probe child via process_vm_readv.
	checkSandboxCanaryWord uint64 = 0x5355524c41563300

	// ptraceRemedyHint explains how to enable pidfd_getfd and process_vm_readv
	// in containers and on hosts with Yama ptrace_scope == 2.
	ptraceRemedyHint = "if running inside a container, pass --cap-add SYS_PTRACE or --security-opt seccomp=unconfined (or allow pidfd_getfd and process_vm_readv in the container seccomp profile); on a host with Yama ptrace_scope == 2, set sysctl kernel.yama.ptrace_scope=1 or grant CAP_SYS_PTRACE"
)

var (
	// checkSandboxMu serializes checkSandbox until a check succeeds; failed
	// checks are not cached so an operator who fixes a host setting can
	// retry without restarting surla.
	checkSandboxMu sync.Mutex
	checkSandboxOK bool

	// detectLandlockABI queries the kernel's Landlock ABI version; tests
	// may override it to simulate an ABI 4-5 kernel through the production
	// path.
	detectLandlockABI = landlockABIVersion

	// validatedGOROOTs caches successful per-GOROOT canary builds keyed by the
	// resolved GOROOT path and its bin/go device, inode, size, and mtime.
	validatedGOROOTs sync.Map
)

func init() {
	if len(os.Args) >= 1 && os.Args[0] == surlaJailArgv0 {
		runJailLauncher()
	}
}

// checkSandbox reports whether this Linux host supports the Landlock, Seccomp,
// pidfd_getfd, and process_vm_readv primitives and can compile a canary Go
// binary inside the build jail.
func checkSandbox() error {
	checkSandboxMu.Lock()
	defer checkSandboxMu.Unlock()
	if checkSandboxOK {
		return nil
	}
	if err := runCheckSandboxInit(); err != nil {
		return err
	}
	checkSandboxOK = true
	return nil
}

// runCheckSandboxInit executes the one-time kernel prerequisite probe and, when
// runtime.GOROOT() has bin/go on disk, validates the default toolchain in the
// build jail.
func runCheckSandboxInit() error {
	if err := checkKernelPrerequisites(); err != nil {
		return err
	}
	if goroot := runtime.GOROOT(); goroot != "" {
		if err := ensureGOROOTCanary(goroot); err != nil {
			return err
		}
	}
	return nil
}

// checkKernelPrerequisites verifies unprivileged execution, Landlock ABI >= 4,
// and parent-to-child pidfd_open, pidfd_getfd, and process_vm_readv against a
// jailed child process.
func checkKernelPrerequisites() error {
	if err := checkNotSecureOrSetuid(); err != nil {
		return fmt.Errorf("%w: %v", ErrIsolationUnavailable, err)
	}
	abi, errno := detectLandlockABI()
	if errno != 0 || abi < 4 {
		return fmt.Errorf("%w: landlock ABI %d (errno %v); Linux kernel >= 6.7 with Landlock ABI >= 4 is required", ErrIsolationUnavailable, abi, errno)
	}

	cfgR, cfgW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrIsolationUnavailable, err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		cfgR.Close()
		cfgW.Close()
		return fmt.Errorf("%w: %v", ErrIsolationUnavailable, err)
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		cfgR.Close()
		cfgW.Close()
		errR.Close()
		errW.Close()
		return fmt.Errorf("%w: socketpair: %v", ErrIsolationUnavailable, err)
	}
	parentSock := fds[0]
	spChild := os.NewFile(uintptr(fds[1]), "sp-child")

	cmd := exec.Command("/proc/self/exe")
	cmd.Args = []string{surlaJailArgv0}
	cmd.Dir = "/"
	cmd.Env = []string{}
	cmd.ExtraFiles = []*os.File{cfgR, errW, spChild}
	if err := cmd.Start(); err != nil {
		cfgR.Close()
		cfgW.Close()
		errR.Close()
		errW.Close()
		syscall.Close(parentSock)
		spChild.Close()
		return fmt.Errorf("%w: start child probe: %v", ErrIsolationUnavailable, err)
	}
	cfgR.Close()
	errW.Close()
	spChild.Close()

	rawCfg, err := json.Marshal(jailConfig{
		Mode:              modeExecNoPorts,
		CheckSandboxProbe: true,
		Dir:               "/",
	})
	if err != nil {
		cfgW.Close()
		syscall.Close(parentSock)
		errR.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("%w: marshal probe config: %v", ErrIsolationUnavailable, err)
	}
	if _, err := cfgW.Write(rawCfg); err != nil {
		cfgW.Close()
		syscall.Close(parentSock)
		errR.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("%w: write probe config: %v", ErrIsolationUnavailable, err)
	}
	cfgW.Close()

	listenerFD, meta, recvErr := recvListenerFD(parentSock)
	if recvErr != nil {
		syscall.Close(parentSock)
		cmd.Wait()
		errBytes, _ := io.ReadAll(errR)
		errR.Close()
		return fmt.Errorf("%w: recv listener from child: %v (%s)", ErrIsolationUnavailable, recvErr, string(errBytes))
	}
	defer syscall.Close(listenerFD)

	childPID := cmd.Process.Pid
	sys := currentArchSyscalls()
	pidfdR1, _, pidfdErrno := syscall.Syscall(uintptr(sys.sysPidfdOpen), uintptr(childPID), 0, 0)
	if pidfdErrno != 0 {
		syscall.Write(parentSock, []byte{1})
		syscall.Close(parentSock)
		cmd.Wait()
		errR.Close()
		return fmt.Errorf("%w: pidfd_open(child): %v", ErrIsolationUnavailable, pidfdErrno)
	}
	pidfd := int(pidfdR1)
	defer syscall.Close(pidfd)

	dupR1, _, dupErrno := syscall.Syscall(uintptr(sys.sysPidfdGetfd), uintptr(pidfd), uintptr(meta.DevNullFD), 0)
	if dupErrno != 0 {
		syscall.Write(parentSock, []byte{1})
		syscall.Close(parentSock)
		cmd.Wait()
		errR.Close()
		return introspectionUnavailableError("pidfd_getfd", dupErrno)
	}
	syscall.Close(int(dupR1))

	var wordBuf [8]byte
	localIov := syscall.Iovec{Base: &wordBuf[0], Len: 8}
	remoteIov := struct {
		Base uintptr
		Len  uint64
	}{Base: uintptr(meta.CanaryAddr), Len: 8}
	_, _, vmErrno := syscall.Syscall6(
		uintptr(sys.sysProcessVMReadv),
		uintptr(childPID),
		uintptr(unsafe.Pointer(&localIov)),
		1,
		uintptr(unsafe.Pointer(&remoteIov)),
		1,
		0,
	)
	if vmErrno != 0 || binary.LittleEndian.Uint64(wordBuf[:]) != checkSandboxCanaryWord {
		syscall.Write(parentSock, []byte{1})
		syscall.Close(parentSock)
		cmd.Wait()
		errR.Close()
		return introspectionUnavailableError("process_vm_readv", vmErrno)
	}

	syscall.Write(parentSock, []byte{1})
	syscall.Close(parentSock)
	errR.Close()
	if waitErr := cmd.Wait(); waitErr != nil {
		return fmt.Errorf("%w: child probe wait: %v", ErrIsolationUnavailable, waitErr)
	}
	return nil
}

// introspectionUnavailableError formats a parent-to-child pidfd_getfd or
// process_vm_readv failure with container and host remediation steps.
func introspectionUnavailableError(op string, errno syscall.Errno) error {
	return fmt.Errorf("%w: %s(child) failed (%v): %s", ErrIsolationUnavailable, op, errno, ptraceRemedyHint)
}

// ensureGOROOTCanary runs a one-time jailed canary `go build` for each distinct
// GOROOT toolchain so any host toolchain syscall incompatibility fails with
// ErrIsolationUnavailable and names the refused syscall.
func ensureGOROOTCanary(goroot string) error {
	resolved, err := filepath.EvalSymlinks(goroot)
	if err == nil {
		goroot = resolved
	}
	goBin := filepath.Join(goroot, "bin", "go")
	var st syscall.Stat_t
	if err := syscall.Stat(goBin, &st); err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil
		}
		return fmt.Errorf("%w: stat toolchain %s: %v", ErrIsolationUnavailable, goBin, err)
	}
	key := fmt.Sprintf("%s\x00%d:%d:%d:%d", goroot, st.Dev, st.Ino, st.Size, st.Mtim.Sec)
	if _, ok := validatedGOROOTs.Load(key); ok {
		return nil
	}

	canaryDir, err := os.MkdirTemp("", "surla-canary-*")
	if err != nil {
		return fmt.Errorf("%w: create canary directory: %v", ErrIsolationUnavailable, err)
	}
	defer os.RemoveAll(canaryDir)

	canarySrc, canaryWork, canaryMod := filepath.Join(canaryDir, "src"), filepath.Join(canaryDir, "work"), filepath.Join(canaryDir, "mod")
	for _, d := range []string{canarySrc, canaryWork, canaryMod} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("%w: prepare canary directory: %v", ErrIsolationUnavailable, err)
		}
	}
	if err := os.WriteFile(filepath.Join(canarySrc, "go.mod"), []byte("module example.com/canary\n\ngo 1.26\n"), 0o600); err != nil {
		return fmt.Errorf("%w: write canary go.mod: %v", ErrIsolationUnavailable, err)
	}
	if err := os.WriteFile(filepath.Join(canarySrc, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		return fmt.Errorf("%w: write canary main.go: %v", ErrIsolationUnavailable, err)
	}
	canaryOut := filepath.Join(canaryWork, "canary-bin")
	b := resolvedBuild{
		spec: BuildSpec{
			SourceDir: canarySrc, ModuleDir: canarySrc, Package: ".",
			Out: canaryOut, GOROOT: goroot, ModCache: canaryMod, WorkDir: canaryWork,
		},
		outDir: canaryWork,
	}
	canaryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var logBuf bytes.Buffer
	if err := b.runGoBuildWithWriter(canaryCtx, (*exec.Cmd).Run, &logBuf, nil, canaryOut, "build", "-trimpath"); err != nil {
		return fmt.Errorf("%w: toolchain canary failed for %s: %v (%s)", ErrIsolationUnavailable, goroot, err, strings.TrimSpace(logBuf.String()))
	}
	validatedGOROOTs.Store(key, struct{}{})
	return nil
}

// startProcess starts binaryPath inside the Linux Landlock and Seccomp sandbox
// described by spec.
func startProcess(
	ctx context.Context,
	binaryPath string,
	spec Spec,
	logFile *os.File,
	stdout io.Writer,
) (*Process, error) {
	if err := checkStaticELFNoInterp(binaryPath); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start candidate %s: %w", binaryPath, err)
	}
	mode := modeExecNoPorts
	if spec.ListenPort > 0 || len(spec.ConnectPorts) > 0 {
		mode = modeExecWithPorts
	}
	cfg := jailConfig{
		Mode:         mode,
		Binary:       binaryPath,
		Args:         spec.Args,
		Dir:          spec.StateDir,
		Env:          append([]string{"HOME=" + spec.StateDir, "TMPDIR=" + spec.StateDir}, spec.Env...),
		StateDir:     spec.StateDir,
		ListenPort:   spec.ListenPort,
		ConnectPorts: spec.ConnectPorts,
	}
	return startProcessWithConfig(ctx, cfg, spec.LogFile, logFile, stdout)
}

// startProcessWithConfig launches cfg in the Linux jail and wires its
// user-notification supervisor and background Wait goroutine into *Process.
func startProcessWithConfig(
	ctx context.Context,
	cfg jailConfig,
	logPath string,
	logFile *os.File,
	stdout io.Writer,
) (*Process, error) {
	if err := resolveEffectiveNoScope(&cfg); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start candidate %s: %w", cfg.Binary, err)
	}
	cfgR, cfgW, err := os.Pipe()
	if err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start candidate %s: %w", cfg.Binary, err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		cfgR.Close()
		cfgW.Close()
		logFile.Close()
		return nil, fmt.Errorf("start candidate %s: %w", cfg.Binary, err)
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		cfgR.Close()
		cfgW.Close()
		errR.Close()
		errW.Close()
		logFile.Close()
		return nil, fmt.Errorf("start candidate %s: socketpair: %w", cfg.Binary, err)
	}
	parentSock := fds[0]
	spChild := os.NewFile(uintptr(fds[1]), "sp-child")

	cmd := exec.CommandContext(ctx, "/proc/self/exe")
	cmd.Args = []string{surlaJailArgv0}
	cmd.Dir = "/"
	cmd.Env = []string{}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if stdout != nil {
		cmd.Stdout = stdout
	}
	cmd.ExtraFiles = []*os.File{cfgR, errW, spChild}
	if err := cmd.Start(); err != nil {
		cfgR.Close()
		cfgW.Close()
		errR.Close()
		errW.Close()
		syscall.Close(parentSock)
		spChild.Close()
		logFile.Close()
		return nil, fmt.Errorf("start candidate %s: %w", cfg.Binary, err)
	}
	cfgR.Close()
	errW.Close()
	spChild.Close()

	rawCfg, err := json.Marshal(cfg)
	if err != nil {
		cfgW.Close()
		syscall.Close(parentSock)
		errR.Close()
		cmd.Process.Kill()
		cmd.Wait()
		logFile.Close()
		return nil, fmt.Errorf("start candidate %s: %w", cfg.Binary, err)
	}
	if _, err := cfgW.Write(rawCfg); err != nil {
		cfgW.Close()
		syscall.Close(parentSock)
		errR.Close()
		cmd.Process.Kill()
		cmd.Wait()
		logFile.Close()
		return nil, fmt.Errorf("start candidate %s: %w", cfg.Binary, err)
	}
	cfgW.Close()

	listenerFD, meta, recvErr := recvListenerFD(parentSock)
	syscall.Close(parentSock)
	if recvErr != nil || meta.ChildPID != cmd.Process.Pid {
		if recvErr == nil {
			syscall.Close(listenerFD)
		}
		errBytes, _ := io.ReadAll(errR)
		errR.Close()
		waitErr := cmd.Wait()
		logFile.Close()
		return nil, fmt.Errorf("%w: launcher error (%v): %s", ErrIsolationUnavailable, waitErr, string(errBytes))
	}

	tracker := &refusalTracker{logFile: logFile}
	activeDups := newActiveDupSet()
	var childDone atomic.Bool
	var supWG sync.WaitGroup
	supWG.Add(1)
	go func() {
		defer supWG.Done()
		serveUserNotif(listenerFD, cmd.Process.Pid, cfg, tracker, &childDone, activeDups)
	}()

	p := &Process{cmd: cmd, logFile: logPath, done: make(chan struct{})}
	go func() {
		errBytes, _ := io.ReadAll(errR)
		errR.Close()
		waitErr := cmd.Wait()
		childDone.Store(true)
		activeDups.shutdownAll()
		supWG.Wait()
		syscall.Close(listenerFD)
		refused := tracker.list()
		switch {
		case len(errBytes) > 0:
			p.waitErr = fmt.Errorf("%w: launcher error (%v): %s", ErrIsolationUnavailable, waitErr, string(errBytes))
		case waitErr != nil && len(refused) > 0:
			p.waitErr = fmt.Errorf("%w (%s)", waitErr, strings.Join(refused, "; "))
		default:
			p.waitErr = waitErr
		}
		p.waitErr = errors.Join(p.waitErr, logFile.Close())
		close(p.done)
	}()
	return p, nil
}

// publishStagedOutput opens stagedOut with O_RDONLY|O_NOFOLLOW|O_CLOEXEC,
// verifies via fstat on the open descriptor that it is a regular file, and
// publishes its bytes to outPath.
func publishStagedOutput(stagedOut, outPath string) error {
	return publishStagedOutputAllowMissing(stagedOut, outPath, false)
}

// publishStagedOutputAllowMissing publishes stagedOut to outPath, returning nil
// when allowMissing is true and stagedOut does not exist.
func publishStagedOutputAllowMissing(stagedOut, outPath string, allowMissing bool) error {
	fd, err := syscall.Open(stagedOut, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if allowMissing && errors.Is(err, syscall.ENOENT) {
			return nil
		}
		return fmt.Errorf("open staged build output: %w", err)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return fmt.Errorf("stat staged build output: %w", err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		syscall.Close(fd)
		return fmt.Errorf("staged build output %s is not a regular file (mode %#o)", outPath, st.Mode)
	}
	srcFile := os.NewFile(uintptr(fd), stagedOut)
	defer srcFile.Close()

	tmp, err := os.CreateTemp(filepath.Dir(outPath), ".surla-out-*")
	if err != nil {
		return fmt.Errorf("create output binary %s: %w", outPath, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmp, srcFile); err != nil {
		tmp.Close()
		return fmt.Errorf("write output binary %s: %w", outPath, err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod output binary %s: %w", outPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close output binary %s: %w", outPath, err)
	}
	if err := os.Rename(tmpName, outPath); err != nil {
		return fmt.Errorf("publish output binary %s: %w", outPath, err)
	}
	return nil
}

// runJailLauncher executes inside the child /proc/self/exe process before main:
// it reads jailConfig from FD 3, refuses setuid/setgid or AT_SECURE execution,
// chdirs to cfg.Dir, sets PR_SET_NO_NEW_PRIVS, applies Landlock, installs the
// primary Seccomp filter and sends its listener FD over FD 5, installs the
// post-handoff sendmsg/seccomp deny filter, marks all descriptors >= 3
// O_CLOEXEC via close_range, and execves cfg.Binary.
func runJailLauncher() {
	runtime.LockOSThread()

	cfgFile := os.NewFile(3, "cfg")
	cfgBytes, err := io.ReadAll(cfgFile)
	if err != nil {
		failLauncher("read_cfg", err)
	}
	cfgFile.Close()
	syscall.CloseOnExec(4)

	var cfg jailConfig
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		failLauncher("decode_cfg", err)
	}

	if err := checkNotSecureOrSetuid(); err != nil {
		failLauncher("setuid_or_at_secure_refused", err)
	}

	if err := syscall.Chdir(cfg.Dir); err != nil {
		failLauncher("chdir", err)
	}

	syscall.CloseOnExec(5)
	sys := currentArchSyscalls()

	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		failLauncher("no_new_privs", errno)
	}

	if err := applyLandlock(sys, cfg); err != nil {
		failLauncher("landlock", err)
	}

	// Pre-allocate all Filter 1, Filter 2, metadata, and SCM_RIGHTS buffers
	// before installing Filter 1 so zero heap allocations occur between Filter 1
	// installation and syscall.Exec.
	selfPID := uint32(os.Getpid())
	prog := buildSeccompAllowlist(sys, cfg, selfPID)
	fprog := sockFprog{
		Len:    uint16(len(prog)),
		Filter: &prog[0],
	}
	postProg := buildPostHandoffDenySendmsgFilter(sys)
	postFprog := sockFprog{
		Len:    uint16(len(postProg)),
		Filter: &postProg[0],
	}

	var canaryWord uint64 = checkSandboxCanaryWord
	devNullFD := -1
	if cfg.CheckSandboxProbe {
		var openErr error
		devNullFD, openErr = syscall.Open("/dev/null", syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		if openErr != nil {
			failLauncher("open_devnull", openErr)
		}
	}
	metaBytes, err := json.Marshal(listenerHandoffMeta{
		ChildPID:   int(selfPID),
		DevNullFD:  devNullFD,
		CanaryAddr: uint64(uintptr(unsafe.Pointer(&canaryWord))),
	})
	if err != nil {
		failLauncher("marshal_meta", err)
	}
	rights := syscall.UnixRights(0)
	cmsgHdrLen := syscall.CmsgLen(0)

	r1, _, errno := syscall.Syscall(
		uintptr(sys.sysSeccomp),
		seccompSetModeFilter,
		seccompFilterFlagNewListener,
		uintptr(unsafe.Pointer(&fprog)),
	)
	if errno != 0 {
		failLauncher("seccomp_listener", errno)
	}
	listenerFD := int(r1)
	binary.LittleEndian.PutUint32(rights[cmsgHdrLen:cmsgHdrLen+4], uint32(listenerFD))
	if err := syscall.Sendmsg(5, metaBytes, rights, nil, 0); err != nil {
		failLauncher("send_listener_fd", err)
	}
	syscall.Close(listenerFD)

	if _, _, errno := syscall.Syscall(
		uintptr(sys.sysSeccomp),
		seccompSetModeFilter,
		0,
		uintptr(unsafe.Pointer(&postFprog)),
	); errno != 0 {
		failLauncher("seccomp_post_handoff", errno)
	}

	if cfg.CheckSandboxProbe {
		var ack [1]byte
		syscall.Read(5, ack[:])
		runtime.KeepAlive(&canaryWord)
		syscall.Close(devNullFD)
		syscall.Close(5)
		syscall.Close(4)
		syscall.Exit(0)
	}
	syscall.Close(5)

	if _, _, errno := syscall.RawSyscall(uintptr(sys.sysCloseRange), 3, ^uintptr(0), closeRangeCloexec); errno != 0 {
		failLauncher("close_range", errno)
	}
	if err := syscall.Exec(cfg.Binary, append([]string{cfg.Binary}, cfg.Args...), cfg.Env); err != nil {
		failLauncher("execve", err)
	}
}

// failLauncher writes step and err to FD 4 and exits with status 126.
func failLauncher(step string, err error) {
	msg := fmt.Sprintf("%s: %v", step, err)
	syscall.Write(4, []byte(msg))
	syscall.Close(4)
	syscall.Exit(126)
}

// checkNotSecureOrSetuid refuses execution when uid != euid, gid != egid,
// /proc/self/auxv is unreadable, or AT_SECURE != 0.
func checkNotSecureOrSetuid() error {
	if syscall.Getuid() != syscall.Geteuid() || syscall.Getgid() != syscall.Getegid() {
		return errors.New("setuid/setgid execution refused")
	}
	auxv, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		return fmt.Errorf("read /proc/self/auxv: %w", err)
	}
	const atSecure = 23
	for i := 0; i+16 <= len(auxv); i += 16 {
		tag := binary.LittleEndian.Uint64(auxv[i : i+8])
		val := binary.LittleEndian.Uint64(auxv[i+8 : i+16])
		if tag == 0 {
			break
		}
		if tag == atSecure && val != 0 {
			return errors.New("AT_SECURE execution refused")
		}
	}
	return nil
}

// applyLandlock creates and enforces the Landlock filesystem, network, and IPC
// scope ruleset for cfg.
func applyLandlock(sys archSyscalls, cfg jailConfig) error {
	r1, _, errno := syscall.Syscall(uintptr(sys.sysLandlockCreateRuleset), 0, 0, landlockCreateRulesetVersion)
	if errno != 0 || int(r1) < 4 {
		return fmt.Errorf("landlock ABI %d (errno %v)", int(r1), errno)
	}
	abi := int(r1)

	allFS := uint64(
		landlockAccessFSExecute | landlockAccessFSWriteFile | landlockAccessFSReadFile |
			landlockAccessFSReadDir | landlockAccessFSRemoveDir | landlockAccessFSRemoveFile |
			landlockAccessFSMakeChar | landlockAccessFSMakeDir | landlockAccessFSMakeReg |
			landlockAccessFSMakeSock | landlockAccessFSMakeFifo | landlockAccessFSMakeBlock |
			landlockAccessFSMakeSym | landlockAccessFSRefer | landlockAccessFSTruncate)
	if abi >= 5 {
		allFS |= landlockAccessFSIoctlDev
	}
	readOnlyDir := uint64(landlockAccessFSReadFile | landlockAccessFSReadDir)
	readWriteNoExec := allFS &^ uint64(landlockAccessFSExecute)

	attr := landlockRulesetAttrV6{
		HandledAccessFS:  allFS,
		HandledAccessNet: landlockAccessNetBindTCP | landlockAccessNetConnectTCP,
	}
	attrSize := uintptr(16)
	if abi >= 6 && !cfg.NoScope {
		attr.Scoped = landlockScopeAbstractUnixSocket | landlockScopeSignal
		attrSize = 24
	}

	fdR1, _, errno := syscall.Syscall(uintptr(sys.sysLandlockCreateRuleset), uintptr(unsafe.Pointer(&attr)), attrSize, 0)
	if errno != 0 {
		return fmt.Errorf("create_ruleset: %v", errno)
	}
	rulesetFD := int(fdR1)
	defer syscall.Close(rulesetFD)

	addPath := func(path string, access uint64, optional bool) error {
		fd, err := syscall.Open(path, oPath|syscall.O_CLOEXEC, 0)
		if err != nil {
			if optional && errors.Is(err, syscall.ENOENT) {
				return nil
			}
			return fmt.Errorf("open %s: %w", path, err)
		}
		defer syscall.Close(fd)
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			return err
		}
		if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			fileMask := uint64(landlockAccessFSExecute | landlockAccessFSWriteFile | landlockAccessFSReadFile | landlockAccessFSTruncate)
			if abi >= 5 {
				fileMask |= landlockAccessFSIoctlDev
			}
			access &= fileMask
		}
		rule := landlockPathBeneathAttr{AllowedAccess: access, ParentFD: int32(fd)}
		_, _, errno := syscall.Syscall6(
			uintptr(sys.sysLandlockAddRule), uintptr(rulesetFD), landlockRulePathBeneath, uintptr(unsafe.Pointer(&rule)), 0, 0, 0,
		)
		if errno != 0 {
			return fmt.Errorf("add_rule %s: %v", path, errno)
		}
		return nil
	}

	addPort := func(port int, access uint64) error {
		rule := landlockNetPortAttr{
			AllowedAccess: access,
			Port:          uint64(port),
		}
		_, _, errno := syscall.Syscall6(
			uintptr(sys.sysLandlockAddRule),
			uintptr(rulesetFD),
			landlockRuleNetPort,
			uintptr(unsafe.Pointer(&rule)),
			0, 0, 0,
		)
		if errno != 0 {
			return fmt.Errorf("add_port %d: %v", port, errno)
		}
		return nil
	}

	if err := addPath("/dev/null", landlockAccessFSReadFile|landlockAccessFSWriteFile|landlockAccessFSTruncate, false); err != nil {
		return err
	}
	for _, dev := range []string{"/dev/urandom", "/dev/random", "/dev/zero", "/proc/sys/net/core/somaxconn"} {
		if err := addPath(dev, landlockAccessFSReadFile, true); err != nil {
			return err
		}
	}

	switch {
	case cfg.CheckSandboxProbe:
	case cfg.Mode == modeBuild:
		if err := addPath(cfg.GOROOT, readOnlyDir|landlockAccessFSExecute, false); err != nil {
			return err
		}
		if !strings.HasPrefix(cfg.Binary, cfg.GOROOT+"/") {
			if err := addPath(cfg.Binary, landlockAccessFSReadFile|landlockAccessFSExecute, false); err != nil {
				return err
			}
		}
		goBin := filepath.Join(cfg.GOROOT, "bin", "go")
		if interp, err := readELFInterp(goBin); err == nil && interp != "" {
			if resolvedInterp, evalErr := filepath.EvalSymlinks(interp); evalErr == nil {
				interp = resolvedInterp
			}
			if err := addPath(interp, landlockAccessFSReadFile|landlockAccessFSExecute, false); err != nil {
				return err
			}
			for _, sysLib := range []string{"/lib", "/usr/lib", "/lib64", "/etc/ld.so.cache"} {
				if err := addPath(sysLib, readOnlyDir, true); err != nil {
					return err
				}
			}
		}
		if err := addPath(cfg.SourceDir, readOnlyDir, false); err != nil {
			return err
		}
		if err := addPath(cfg.ModCache, readOnlyDir, false); err != nil {
			return err
		}
		if err := addPath(cfg.WorkDir, readWriteNoExec, false); err != nil {
			return err
		}
		if cfg.OutDir != "" && cfg.OutDir != cfg.WorkDir {
			if err := addPath(cfg.OutDir, readWriteNoExec, false); err != nil {
				return err
			}
		}
	default:
		if err := addPath(cfg.Binary, landlockAccessFSReadFile|landlockAccessFSExecute, false); err != nil {
			return err
		}
		if err := addPath(cfg.StateDir, readWriteNoExec, false); err != nil {
			return err
		}
		if cfg.Mode == modeExecWithPorts {
			if cfg.ListenPort > 0 {
				if err := addPort(cfg.ListenPort, landlockAccessNetBindTCP); err != nil {
					return err
				}
			}
			for _, p := range cfg.ConnectPorts {
				if err := addPort(p, landlockAccessNetConnectTCP); err != nil {
					return err
				}
			}
		}
	}

	if _, _, errno = syscall.Syscall(uintptr(sys.sysLandlockRestrictSelf), uintptr(rulesetFD), 0, 0); errno != 0 {
		return fmt.Errorf("restrict_self: %v", errno)
	}
	return nil
}

// landlockABIVersion queries the kernel's Landlock ABI version.
func landlockABIVersion() (int, syscall.Errno) {
	sys := currentArchSyscalls()
	r1, _, errno := syscall.Syscall(uintptr(sys.sysLandlockCreateRuleset), 0, 0, landlockCreateRulesetVersion)
	if errno != 0 {
		return -1, errno
	}
	return int(r1), 0
}

// runGoBuild executes one jailed `go build` or `go test -c` invocation for b.
func (b resolvedBuild) runGoBuild(
	ctx context.Context,
	run func(*exec.Cmd) error,
	logFile *os.File,
	outPath string,
	args ...string,
) error {
	if err := ensureGOROOTCanary(b.spec.GOROOT); err != nil {
		return err
	}
	return b.runGoBuildWithWriter(ctx, run, logFile, logFile, outPath, args...)
}

// runGoBuildWithWriter runs one jailed `go` invocation, staging `-o` inside
// spec.WorkDir, supervising seccomp user notifications, and publishing the
// verified regular output file to outPath when the command succeeds.
func (b resolvedBuild) runGoBuildWithWriter(
	ctx context.Context,
	run func(*exec.Cmd) error,
	outWriter io.Writer,
	logFile *os.File,
	outPath string,
	args ...string,
) error {
	spec := b.spec
	stageDir, err := os.MkdirTemp(spec.WorkDir, ".surla-stage-*")
	if err != nil {
		return fmt.Errorf("create build stage directory: %w", err)
	}
	defer os.RemoveAll(stageDir)

	stagedOut := filepath.Join(stageDir, "out")
	goArgs := append([]string{}, args...)
	if len(goArgs) > 0 && goArgs[0] == "build" {
		goArgs = append(goArgs, "-buildvcs=false")
	}
	goArgs = append(goArgs, "-o", stagedOut, spec.Package)
	env := b.buildEnv()
	goBin := filepath.Join(spec.GOROOT, "bin", "go")

	cfg := jailConfig{
		Mode:      modeBuild,
		Binary:    goBin,
		Args:      goArgs,
		Dir:       spec.ModuleDir,
		Env:       env,
		SourceDir: spec.SourceDir,
		GOROOT:    spec.GOROOT,
		ModCache:  spec.ModCache,
		WorkDir:   spec.WorkDir,
		OutDir:    b.outDir,
	}
	tracker := &refusalTracker{logFile: logFile}
	if err := runSupervisedJailedWithContext(ctx, cfg, outWriter, tracker, run); err != nil {
		return err
	}
	allowMissing := len(args) > 0 && args[0] == "test"
	return publishStagedOutputAllowMissing(stagedOut, outPath, allowMissing)
}
