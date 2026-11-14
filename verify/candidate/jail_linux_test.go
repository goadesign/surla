//go:build linux

package candidate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type (
	// buildExecEscapesResult decodes the JSON output of linuxprobe
	// "build-exec-escapes".
	buildExecEscapesResult struct {
		ExecWorkDir string `json:"exec_workdir"`
		ExecGit     string `json:"exec_git"`
		ExecLdLinux string `json:"exec_ld_linux"`
	}

	// buildSignalsDgramResult decodes the JSON output of linuxprobe
	// "build-signals-and-dgram".
	buildSignalsDgramResult struct {
		KillParent   string `json:"kill_parent"`
		TkillParent  string `json:"tkill_parent"`
		TgkillParent string `json:"tgkill_parent"`
		TgkillSelf   string `json:"tgkill_self"`
		IoctlSetown  string `json:"ioctl_setown"`
		SpDgram      string `json:"sp_dgram"`
		SendmsgPath  string `json:"sendmsg_path"`
		SendmsgAbs   string `json:"sendmsg_abs"`
	}

	// listenWithoutBindResult decodes the JSON output of linuxprobe
	// "listen-without-bind-only".
	listenWithoutBindResult struct {
		Listen4Err string `json:"listen4_err"`
		Bound4Type string `json:"bound4_type"`
		Listen6Err string `json:"listen6_err"`
		Bound6Type string `json:"bound6_type"`
	}

	// netSuiteResult decodes the JSON output of linuxprobe "net-suite".
	netSuiteResult struct {
		DialAllowedErr     string `json:"dial_allowed_err"`
		DialAllowedBody    string `json:"dial_allowed_body"`
		BindListen4Err     string `json:"bind_listen4_err"`
		SelfDialErr        string `json:"self_dial_err"`
		SelfDialBody       string `json:"self_dial_body"`
		ListenUnbound4Err  string `json:"listen_unbound4_err"`
		ListenUnbound6Err  string `json:"listen_unbound6_err"`
		BridgeDialErr      string `json:"bridge_dial_err"`
		UnlistedConnectErr string `json:"unlisted_connect_err"`
		Wild4BindErr       string `json:"wild4_bind_err"`
		Wild6BindErr       string `json:"wild6_bind_err"`
		Dual6BindErr       string `json:"dual6_bind_err"`
		Loop6BindErr       string `json:"loop6_bind_err"`
		Loop6ListenErr     string `json:"loop6_listen_err"`
		UnlistedBindErr    string `json:"unlisted_bind_err"`
		UnspecConnectErr   string `json:"unspec_connect_err"`
		TOCTOUCompleted    bool   `json:"toctou_completed"`
	}

	// attackSuiteResult decodes the JSON output of linuxprobe "attack-suite".
	attackSuiteResult struct {
		UnixPathnameErr   string `json:"unix_pathname_err"`
		UnixAbstractErr   string `json:"unix_abstract_err"`
		SocketpairStream  string `json:"socketpair_stream"`
		SocketpairDgram   string `json:"socketpair_dgram"`
		SendtoPath        string `json:"sendto_path"`
		SendmsgPath       string `json:"sendmsg_path"`
		SendmsgAbs        string `json:"sendmsg_abs"`
		TfoSendto         string `json:"tfo_sendto"`
		TfoSendmsg        string `json:"tfo_sendmsg"`
		TfoSendmmsg       string `json:"tfo_sendmmsg"`
		SCTPErr           string `json:"sctp_err"`
		MPTCPErr          string `json:"mptcp_err"`
		UDPErr            string `json:"udp_err"`
		BridgeErr         string `json:"bridge_err"`
		BridgeBody        string `json:"bridge_body"`
		ChmodOut          string `json:"chmod_out"`
		ChmodIn           string `json:"chmod_in"`
		ChownOut          string `json:"chown_out"`
		ChtimesOut        string `json:"chtimes_out"`
		SetxattrOut       string `json:"setxattr_out"`
		FchmodIn          string `json:"fchmod_in"`
		FutimensIn        string `json:"futimens_in"`
		KeyctlErr         string `json:"keyctl_err"`
		PrlimitParent     string `json:"prlimit_parent"`
		PrlimitSelf       string `json:"prlimit_self"`
		SetprioParent     string `json:"setprio_parent"`
		SetaffParent      string `json:"setaff_parent"`
		GetaffSelf        string `json:"getaff_self"`
		IoprioParent      string `json:"ioprio_parent"`
		FcntlSetownParent string `json:"fcntl_setown_parent"`
		FcntlSetownZero   string `json:"fcntl_setown_zero"`
		FcntlSetsigKill   string `json:"fcntl_setsig_kill"`
		FcntlSetsigZero   string `json:"fcntl_setsig_zero"`
		IoctlSetown       string `json:"ioctl_setown"`
		IoctlAsync        string `json:"ioctl_async"`
		KillParent        string `json:"kill_parent"`
		TkillParent       string `json:"tkill_parent"`
		TgkillParent      string `json:"tgkill_parent"`
		TgkillSelf        string `json:"tgkill_self"`
		PidfdSendSignal   string `json:"pidfd_send_signal"`
		InotifyErr        string `json:"inotify_err"`
		MemfdErr          string `json:"memfd_err"`
		ExecveatErr       string `json:"execveat_err"`
		StatHiddenErr     string `json:"stat_hidden_err"`
		OpenHiddenErr     string `json:"open_hidden_err"`
		ReaddirHiddenErr  string `json:"readdir_hidden_err"`
		ForkSelfErr       string `json:"fork_self_err"`
		ReadFD6Err        string `json:"read_fd6_err"`
		ReadFD6Data       string `json:"read_fd6_data"`
		IoUringErr        string `json:"io_uring_err"`
		UnshareErr        string `json:"unshare_err"`
		SetpgidErr        string `json:"setpgid_err"`
		SetsidErr         string `json:"setsid_err"`
		PrctlAllowed      string `json:"prctl_allowed"`
		PrctlRefused      string `json:"prctl_refused"`
	}
)

var (
	// linuxProbeOnce compiles testdata/linuxprobe once per test binary run.
	linuxProbeOnce sync.Once
	// linuxProbePath holds the compiled static linuxprobe binary path.
	linuxProbePath string
	// linuxProbeErr holds any compilation error from building linuxprobe.
	linuxProbeErr error
)

// TestLinuxBPFSimulatorDualArch deterministically exercises the Seccomp-BPF
// instruction stream and argument filters for both arm64 and amd64.
func TestLinuxBPFSimulatorDualArch(t *testing.T) {
	for _, arch := range []string{"arm64", "amd64"} {
		t.Run(arch, func(t *testing.T) {
			f1Len, f2Len := runBPFSimulatorForArch(t, arch)
			t.Logf("BPF_SIMULATOR(%s): PASS (filter1_insns=%d filter2_insns=%d)", arch, f1Len, f2Len)
		})
	}
}

// TestLinuxJailPrerequisitesAndGuards verifies kernel availability logging,
// staged output symlink refusal (N4/N-d), and PT_INTERP dynamic-binary refusal
// (B8c).
func TestLinuxJailPrerequisitesAndGuards(t *testing.T) {
	abi, abiErrno := landlockABIVersion()
	ptraceScope := readSysctlTrimmed("/proc/sys/kernel/yama/ptrace_scope")
	mptcpEnabled := readSysctlTrimmed("/proc/sys/net/mptcp/enabled")
	unameOut, _ := exec.Command("uname", "-r").Output()
	t.Logf(
		"Linux host: kernel=%s arch=%s uid=%d euid=%d landlock_abi=%d (errno=%v) ptrace_scope=%q mptcp=%q",
		strings.TrimSpace(string(unameOut)),
		runtime.GOARCH,
		os.Getuid(),
		os.Geteuid(),
		abi,
		abiErrno,
		ptraceScope,
		mptcpEnabled,
	)
	require.GreaterOrEqual(t, abi, 4, "Linux jail requires Landlock ABI >= 4")
	require.NoError(t, CheckSandbox())

	t.Run("Bv_introspection_unavailable_error_message", func(t *testing.T) {
		for _, op := range []string{"pidfd_getfd", "process_vm_readv"} {
			err := introspectionUnavailableError(op, syscall.EPERM)
			require.ErrorIs(t, err, ErrIsolationUnavailable)
			assert.Contains(t, err.Error(), op+"(child) failed (operation not permitted)")
			assert.Contains(t, err.Error(), "--cap-add SYS_PTRACE")
			assert.Contains(t, err.Error(), "--security-opt seccomp=unconfined")
			assert.Contains(t, err.Error(), "kernel.yama.ptrace_scope=1")
		}
	})

	t.Run("Nd_staged_output_symlink_refused", func(t *testing.T) {
		tmp := t.TempDir()
		stagedLink := filepath.Join(tmp, "staged-link")
		outsideTarget := filepath.Join(tmp, "outside-target")
		require.NoError(t, os.WriteFile(outsideTarget, []byte("secret"), 0o600))
		require.NoError(t, os.Symlink(outsideTarget, stagedLink))

		err := publishStagedOutput(stagedLink, filepath.Join(tmp, "published"))
		require.Error(t, err)
		assert.ErrorIs(t, err, syscall.ELOOP)
	})

	t.Run("B8c_refuse_elf_with_pt_interp", func(t *testing.T) {
		interp, err := readELFInterp("/bin/sh")
		require.NoError(t, err)
		assert.NotEmpty(t, interp)
		err = checkStaticELFNoInterp("/bin/sh")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrIsolationUnavailable)
	})
}

// TestLinuxJailBuildAndBuildTest verifies POS_build, POS_build_test, and the
// B8b/N-a/B6c/B-i/B-iii build-mode attack checks under both full Landlock
// scope (noscope=false) and simulated Landlock ABI 4-5 mode (noscope=true).
func TestLinuxJailBuildAndBuildTest(t *testing.T) {
	require.NoError(t, CheckSandbox())
	probeBin := buildLinuxProbe(t)
	goroot := runtime.GOROOT()

	srcDir := t.TempDir()
	modCache := filepath.Join(t.TempDir(), "modcache")
	require.NoError(t, os.MkdirAll(modCache, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "go.mod"), []byte("module example.com/cand\n\ngo 1.24\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "main.go"), []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"hello\") }\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "main_test.go"), []byte("package main\nimport \"testing\"\nfunc TestOne(t *testing.T) {}\n"), 0o644))

	ldLinux, err := readELFInterp("/bin/sh")
	require.NoError(t, err)

	for _, noScope := range []bool{false, true} {
		t.Run(fmt.Sprintf("noscope_%v", noScope), func(t *testing.T) {
			workDir := t.TempDir()
			outDir := t.TempDir()
			stagedOut := filepath.Join(workDir, "staged-app")
			finalOut := filepath.Join(outDir, "app")
			buildEnv := []string{
				"PATH=" + filepath.Join(goroot, "bin") + ":/usr/bin:/bin",
				"GOROOT=" + goroot,
				"GOPATH=" + modCache,
				"GOMODCACHE=" + modCache,
				"GOCACHE=" + filepath.Join(workDir, "gocache"),
				"TMPDIR=" + workDir,
				"GOTOOLCHAIN=local",
				"GOFLAGS=-mod=readonly",
				"GOPROXY=off",
				"GONOSUMDB=*",
				"CGO_ENABLED=0",
			}

			out, refused, err := runTestJailed(t, jailConfig{
				Mode:      modeBuild,
				NoScope:   noScope,
				Binary:    filepath.Join(goroot, "bin/go"),
				Args:      []string{"build", "-trimpath", "-o", stagedOut, "."},
				Dir:       srcDir,
				Env:       buildEnv,
				GOROOT:    goroot,
				SourceDir: srcDir,
				ModCache:  modCache,
				WorkDir:   workDir,
			}, nil)
			require.NoError(t, err, "POS_build output: %s", out)
			assert.Empty(t, refused)
			require.NoError(t, publishStagedOutput(stagedOut, finalOut))

			workDirTest := t.TempDir()
			stagedTestOut := filepath.Join(workDirTest, "staged-test")
			finalTestOut := filepath.Join(outDir, "cand.test")
			testEnv := append([]string(nil), buildEnv[:4]...)
			testEnv = append(testEnv,
				"GOCACHE="+filepath.Join(workDirTest, "gocache"),
				"TMPDIR="+workDirTest,
				"GOTOOLCHAIN=local",
				"GOFLAGS=-mod=readonly",
				"GOPROXY=off",
				"GONOSUMDB=*",
				"CGO_ENABLED=0",
			)
			outTest, refusedTest, err := runTestJailed(t, jailConfig{
				Mode:      modeBuild,
				NoScope:   noScope,
				Binary:    filepath.Join(goroot, "bin/go"),
				Args:      []string{"test", "-c", "-trimpath", "-vet=off", "-o", stagedTestOut, "."},
				Dir:       srcDir,
				Env:       testEnv,
				GOROOT:    goroot,
				SourceDir: srcDir,
				ModCache:  modCache,
				WorkDir:   workDirTest,
			}, nil)
			require.NoError(t, err, "POS_build_test output: %s", outTest)
			assert.Empty(t, refusedTest)
			require.NoError(t, publishStagedOutput(stagedTestOut, finalTestOut))

			workDirEsc := t.TempDir()
			probeInWork := filepath.Join(workDirEsc, "workdir-exec-probe")
			probeBytes, err := os.ReadFile(probeBin)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(probeInWork, probeBytes, 0o755))

			fakeGoroot := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(fakeGoroot, "bin"), 0o755))
			fakeGo := filepath.Join(fakeGoroot, "bin/go")
			require.NoError(t, os.WriteFile(fakeGo, probeBytes, 0o755))

			escOut, _, err := runTestJailed(t, jailConfig{
				Mode:      modeBuild,
				NoScope:   noScope,
				Binary:    fakeGo,
				Args:      []string{"build-exec-escapes", probeInWork, ldLinux},
				Dir:       srcDir,
				Env:       buildEnv,
				GOROOT:    fakeGoroot,
				SourceDir: srcDir,
				ModCache:  modCache,
				WorkDir:   workDirEsc,
			}, nil)
			require.NoError(t, err)
			var escRes buildExecEscapesResult
			require.NoError(t, json.Unmarshal([]byte(escOut), &escRes))
			assert.Contains(t, escRes.ExecWorkDir, "permission denied")
			assert.Contains(t, escRes.ExecGit, "permission denied")
			assert.Contains(t, escRes.ExecLdLinux, "permission denied")

			bystander := exec.Command(probeBin, "bystander-sleep")
			require.NoError(t, bystander.Start())
			bystanderExited := make(chan error, 1)
			go func() { bystanderExited <- bystander.Wait() }()
			t.Cleanup(func() {
				if bystander.Process != nil {
					bystander.Process.Kill()
				}
			})

			dgramPath := filepath.Join(t.TempDir(), "build-dgram.sock")
			dgramConn, err := net.ListenPacket("unixgram", dgramPath)
			require.NoError(t, err)
			defer dgramConn.Close()
			dgramPathRecvd := startPacketProbeListener(dgramConn)

			absDgramName := fmt.Sprintf("@surla-build-dgram-%d-%v", os.Getpid(), noScope)
			absDgramConn, err := net.ListenPacket("unixgram", absDgramName)
			require.NoError(t, err)
			defer absDgramConn.Close()
			dgramAbsRecvd := startPacketProbeListener(absDgramConn)

			sigOut, _, err := runTestJailed(t, jailConfig{
				Mode:      modeBuild,
				NoScope:   noScope,
				Binary:    fakeGo,
				Args:      []string{"build-signals-and-dgram", strconv.Itoa(os.Getpid()), strconv.Itoa(bystander.Process.Pid), dgramPath, absDgramName},
				Dir:       srcDir,
				Env:       buildEnv,
				GOROOT:    fakeGoroot,
				SourceDir: srcDir,
				ModCache:  modCache,
				WorkDir:   workDirEsc,
			}, nil)
			require.NoError(t, err)
			var sigRes buildSignalsDgramResult
			require.NoError(t, json.Unmarshal([]byte(sigOut), &sigRes))
			assert.Equal(t, "function not implemented", sigRes.KillParent)
			assert.Equal(t, "function not implemented", sigRes.TkillParent)
			assert.Equal(t, "operation not permitted", sigRes.TgkillParent)
			assert.Empty(t, sigRes.TgkillSelf)
			assert.Equal(t, "function not implemented", sigRes.IoctlSetown)
			assert.Equal(t, "operation not permitted", sigRes.SpDgram)
			assert.Equal(t, "operation not permitted", sigRes.SendmsgPath)
			assert.Equal(t, "operation not permitted", sigRes.SendmsgAbs)

			time.Sleep(30 * time.Millisecond)
			select {
			case <-bystanderExited:
				t.Fatal("bystander process was killed by jailed build child")
			default:
			}
			assert.False(t, dgramPathRecvd.Load())
			assert.False(t, dgramAbsRecvd.Load())
		})
	}
}

// TestLinuxJailSupervisorNetworkSuite exercises the SECCOMP_RET_USER_NOTIF
// loopback/port supervisor (B3, B-ii, R9 TOCTOU) in both normal and
// noscope=true modes, plus unjailed negative controls.
func TestLinuxJailSupervisorNetworkSuite(t *testing.T) {
	require.NoError(t, CheckSandbox())
	probeBin := buildLinuxProbe(t)

	allowedLn, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer allowedLn.Close()
	allowedPort := allowedLn.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, acceptErr := allowedLn.Accept()
			if acceptErr != nil {
				return
			}
			c.Write([]byte("pong"))
			c.Close()
		}
	}()

	unlistedLn, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer unlistedLn.Close()
	unlistedPort := unlistedLn.Addr().(*net.TCPAddr).Port

	bridgeIP := nonLoopbackIPv4()
	var bridgeHits atomic.Int32
	bridgeLn, err := net.Listen("tcp4", fmt.Sprintf("%s:0", bridgeIP))
	require.NoError(t, err)
	defer bridgeLn.Close()
	bridgePort := bridgeLn.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, acceptErr := bridgeLn.Accept()
			if acceptErr != nil {
				return
			}
			bridgeHits.Add(1)
			c.Write([]byte("bridge-pong"))
			c.Close()
		}
	}()

	for _, noScope := range []bool{false, true} {
		t.Run(fmt.Sprintf("noscope_%v", noScope), func(t *testing.T) {
			listenPort := reserveFreeLoopbackPort(t)
			stateDir := t.TempDir()
			bridgeHits.Store(0)

			out, refused, err := runTestJailed(t, jailConfig{
				Mode:         modeExecWithPorts,
				NoScope:      noScope,
				Binary:       probeBin,
				Args:         []string{"net-suite", strconv.Itoa(listenPort), strconv.Itoa(allowedPort), strconv.Itoa(unlistedPort), bridgeIP, strconv.Itoa(bridgePort)},
				Dir:          stateDir,
				Env:          []string{"PATH=/usr/bin:/bin", "HOME=" + stateDir, "TMPDIR=" + stateDir},
				StateDir:     stateDir,
				ListenPort:   listenPort,
				ConnectPorts: []int{allowedPort, listenPort, bridgePort},
			}, nil)
			require.NoError(t, err)
			assert.Empty(t, refused)

			var res netSuiteResult
			require.NoError(t, json.Unmarshal([]byte(out), &res))
			assert.Empty(t, res.DialAllowedErr)
			assert.Equal(t, "pong", res.DialAllowedBody)
			assert.Empty(t, res.BindListen4Err)
			assert.Empty(t, res.SelfDialErr)
			assert.Equal(t, "self-ok", res.SelfDialBody)
			assert.Contains(t, res.ListenUnbound4Err, "permission denied")
			assert.Contains(t, res.ListenUnbound6Err, "permission denied")
			assert.Contains(t, res.BridgeDialErr, "permission denied")
			assert.Contains(t, res.UnlistedConnectErr, "permission denied")
			assert.Contains(t, res.Wild4BindErr, "permission denied")
			assert.Contains(t, res.Wild6BindErr, "permission denied")
			assert.Contains(t, res.Dual6BindErr, "permission denied")
			assert.Empty(t, res.Loop6BindErr)
			assert.Empty(t, res.Loop6ListenErr)
			assert.Contains(t, res.UnlistedBindErr, "permission denied")
			assert.Contains(t, res.UnspecConnectErr, "permission denied")
			assert.True(t, res.TOCTOUCompleted)
			assert.Zero(t, bridgeHits.Load())
		})
	}

	negOut, err := exec.Command(probeBin, "listen-without-bind-only").Output()
	require.NoError(t, err)
	var negRes listenWithoutBindResult
	require.NoError(t, json.Unmarshal(negOut, &negRes))
	assert.Empty(t, negRes.Listen4Err)
	assert.Equal(t, "*syscall.SockaddrInet4", negRes.Bound4Type)
}

// TestLinuxJailAttackSuite exercises B1-B9, B-i, B-iii, N3, N5, N-b, and B-vi
// under both normal (noscope=false) and simulated ABI 4-5 (noscope=true) modes,
// plus unjailed negative controls.
func TestLinuxJailAttackSuite(t *testing.T) {
	require.NoError(t, CheckSandbox())
	probeBin := buildLinuxProbe(t)

	unixDir := t.TempDir()
	unixPath := filepath.Join(unixDir, "outside.sock")
	unixLn, err := net.Listen("unix", unixPath)
	require.NoError(t, err)
	defer unixLn.Close()

	absName := fmt.Sprintf("@surla-test-abs-%d", os.Getpid())
	absLn, err := net.Listen("unix", absName)
	require.NoError(t, err)
	defer absLn.Close()

	dgramPath := filepath.Join(unixDir, "outside-dgram.sock")
	dgramConn, err := net.ListenPacket("unixgram", dgramPath)
	require.NoError(t, err)
	defer dgramConn.Close()
	dgramPathRecvd := startPacketProbeListener(dgramConn)

	absDgramName := fmt.Sprintf("@surla-test-dgram-abs-%d", os.Getpid())
	absDgramConn, err := net.ListenPacket("unixgram", absDgramName)
	require.NoError(t, err)
	defer absDgramConn.Close()
	dgramAbsRecvd := startPacketProbeListener(absDgramConn)

	bridgeIP := nonLoopbackIPv4()
	bridgeLn, err := net.Listen("tcp4", fmt.Sprintf("%s:0", bridgeIP))
	require.NoError(t, err)
	defer bridgeLn.Close()
	bridgePort := bridgeLn.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, acceptErr := bridgeLn.Accept()
			if acceptErr != nil {
				return
			}
			c.Write([]byte("bridge-pong"))
			c.Close()
		}
	}()

	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("secret"), 0o644))
	outsidePrivateDir := filepath.Join(outsideDir, "outside-private-0700")
	require.NoError(t, os.MkdirAll(outsidePrivateDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(outsidePrivateDir, "hidden-holdout.txt"), []byte("holdout"), 0o600))

	bystander := exec.Command(probeBin, "bystander-sleep")
	require.NoError(t, bystander.Start())
	bystanderExited := make(chan error, 1)
	go func() { bystanderExited <- bystander.Wait() }()
	t.Cleanup(func() {
		if bystander.Process != nil {
			bystander.Process.Kill()
		}
	})

	for _, noScope := range []bool{false, true} {
		t.Run(fmt.Sprintf("noscope_%v", noScope), func(t *testing.T) {
			dgramPathRecvd.Store(false)
			dgramAbsRecvd.Store(false)
			stateDir := t.TempDir()
			extraFD6 := makeNonCloexecSecretPipe(t)

			out, _, err := runTestJailed(t, jailConfig{
				Mode:         modeExecWithPorts,
				NoScope:      noScope,
				Binary:       probeBin,
				Args:         []string{"attack-suite", unixPath, absName, outsideFile, outsidePrivateDir, strconv.Itoa(os.Getpid()), strconv.Itoa(bridgePort), bridgeIP, strconv.Itoa(bridgePort), strconv.Itoa(bystander.Process.Pid), dgramPath, absDgramName},
				Dir:          stateDir,
				Env:          []string{"PATH=/usr/bin:/bin", "HOME=" + stateDir, "TMPDIR=" + stateDir},
				StateDir:     stateDir,
				ConnectPorts: []int{bridgePort},
			}, extraFD6)
			require.NoError(t, err)

			var res attackSuiteResult
			require.NoError(t, json.Unmarshal([]byte(out), &res))
			assert.Contains(t, res.UnixPathnameErr, "operation not permitted")
			assert.Contains(t, res.UnixAbstractErr, "operation not permitted")
			assert.Empty(t, res.SocketpairStream)
			assert.Equal(t, "operation not permitted", res.SocketpairDgram)
			assert.Equal(t, "function not implemented", res.SendtoPath)
			assert.Equal(t, "operation not permitted", res.SendmsgPath)
			assert.Equal(t, "operation not permitted", res.SendmsgAbs)
			assert.Equal(t, "function not implemented", res.TfoSendto)
			assert.Equal(t, "operation not permitted", res.TfoSendmsg)
			assert.Equal(t, "function not implemented", res.TfoSendmmsg)
			assert.Equal(t, "operation not permitted", res.SCTPErr)
			assert.Equal(t, "operation not permitted", res.MPTCPErr)
			assert.Equal(t, "operation not permitted", res.UDPErr)
			assert.Contains(t, res.BridgeErr, "permission denied")
			assert.Empty(t, res.BridgeBody)
			assert.Contains(t, res.ChmodOut, "operation not permitted")
			assert.Contains(t, res.ChmodIn, "operation not permitted")
			assert.Contains(t, res.ChownOut, "operation not permitted")
			assert.Contains(t, res.ChtimesOut, "operation not permitted")
			assert.Equal(t, "operation not permitted", res.SetxattrOut)
			assert.Empty(t, res.FchmodIn)
			assert.Empty(t, res.FutimensIn)
			assertDeniedByJailOrContainer(t, res.KeyctlErr)
			assert.Equal(t, "operation not permitted", res.PrlimitParent)
			assert.Empty(t, res.PrlimitSelf)
			assert.Equal(t, "operation not permitted", res.SetprioParent)
			assert.Equal(t, "operation not permitted", res.SetaffParent)
			assert.Empty(t, res.GetaffSelf)
			assert.Equal(t, "operation not permitted", res.IoprioParent)
			assert.Equal(t, "operation not permitted", res.FcntlSetownParent)
			assert.Empty(t, res.FcntlSetownZero)
			assert.Equal(t, "operation not permitted", res.FcntlSetsigKill)
			assert.Empty(t, res.FcntlSetsigZero)
			assert.Equal(t, "function not implemented", res.IoctlSetown)
			assert.Equal(t, "function not implemented", res.IoctlAsync)
			assert.Equal(t, "function not implemented", res.KillParent)
			assert.Equal(t, "function not implemented", res.TkillParent)
			assert.Equal(t, "operation not permitted", res.TgkillParent)
			assert.Empty(t, res.TgkillSelf)
			assert.Equal(t, "function not implemented", res.PidfdSendSignal)
			assert.Equal(t, "function not implemented", res.InotifyErr)
			assert.Equal(t, "function not implemented", res.MemfdErr)
			assert.Equal(t, "function not implemented", res.ExecveatErr)
			assert.Empty(t, res.StatHiddenErr)
			assert.Contains(t, res.OpenHiddenErr, "permission denied")
			assert.Contains(t, res.ReaddirHiddenErr, "permission denied")
			assert.Contains(t, res.ForkSelfErr, "operation not permitted")
			assert.Equal(t, "bad file descriptor", res.ReadFD6Err)
			assert.Empty(t, res.ReadFD6Data)
			assertDeniedByJailOrContainer(t, res.IoUringErr)
			assertDeniedByJailOrContainer(t, res.UnshareErr)
			assert.Equal(t, "function not implemented", res.SetpgidErr)
			assert.Equal(t, "function not implemented", res.SetsidErr)
			assert.Empty(t, res.PrctlAllowed)
			assert.Equal(t, "function not implemented", res.PrctlRefused)

			time.Sleep(30 * time.Millisecond)
			select {
			case <-bystanderExited:
				t.Fatal("bystander process was killed by jailed child")
			default:
			}
			assert.False(t, dgramPathRecvd.Load())
			assert.False(t, dgramAbsRecvd.Load())

			_, refused, failErr := runTestJailed(t, jailConfig{
				Mode:     modeExecNoPorts,
				NoScope:  noScope,
				Binary:   probeBin,
				Args:     []string{"fail-on-refused-syscall"},
				Dir:      stateDir,
				Env:      []string{"PATH=/usr/bin:/bin", "HOME=" + stateDir, "TMPDIR=" + stateDir},
				StateDir: stateDir,
			}, nil)
			require.Error(t, failErr)
			assert.NotEmpty(t, refused)
			assert.Contains(t, failErr.Error(), "surla jail refused")
		})
	}
}

// buildLinuxProbe compiles verify/candidate/testdata/linuxprobe as a static
// Cgo-free ELF binary once per test binary invocation.
func buildLinuxProbe(t *testing.T) string {
	t.Helper()
	linuxProbeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "surla-linuxprobe-*")
		if err != nil {
			linuxProbeErr = err
			return
		}
		out := filepath.Join(dir, "linuxprobe")
		cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", out, "./testdata/linuxprobe")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if output, err := cmd.CombinedOutput(); err != nil {
			linuxProbeErr = fmt.Errorf("build linuxprobe: %w: %s", err, output)
			return
		}
		linuxProbePath = out
	})
	require.NoError(t, linuxProbeErr)
	return linuxProbePath
}

// runTestJailed executes cfg inside the production Linux jail launcher and
// supervisor, optionally passing extraFD6 at descriptor 6 to test close_range.
func runTestJailed(t *testing.T, cfg jailConfig, extraFD6 *os.File) (string, []string, error) {
	t.Helper()
	if cfg.Mode == modeExecNoPorts || cfg.Mode == modeExecWithPorts {
		if err := checkStaticELFNoInterp(cfg.Binary); err != nil {
			return "", nil, err
		}
	}
	var outBuf bytes.Buffer
	tracker := newRefusalTracker()

	err := runSupervisedJailed(
		cfg,
		&outBuf,
		tracker,
		func(cmd *exec.Cmd) error {
			if extraFD6 != nil {
				cmd.ExtraFiles = append(cmd.ExtraFiles, extraFD6)
				defer extraFD6.Close()
			}
			if err := cmd.Start(); err != nil {
				return err
			}
			return cmd.Wait()
		},
	)
	return strings.TrimSpace(outBuf.String()), tracker.list(), err
}

// runBPFSimulatorForArch verifies the generated Seccomp-BPF instruction streams
// for arch against the in-process BPF virtual machine.
func runBPFSimulatorForArch(t *testing.T, arch string) (int, int) {
	t.Helper()
	sys := archSyscallsFor(arch)
	prog := buildSeccompAllowlist(sys, jailConfig{Mode: modeExecWithPorts, ListenPort: 18080}, 12345)
	postProg := buildPostHandoffDenySendmsgFilter(sys)

	data := func(archVal uint32, nr int, a0, a1, a2 uint64) seccompData {
		return seccompData{
			Nr:   int32(nr),
			Arch: archVal,
			Args: [6]uint64{a0, a1, a2},
		}
	}

	readNr := int(sys.commonAllow[0])
	require.Equal(t, uint32(seccompRetKillProcess), evalBPF(prog, data(0x40000003, readNr, 0, 0, 0)))
	if sys.hasX32Bit {
		require.Equal(t, uint32(seccompRetKillProcess), evalBPF(prog, data(sys.auditArch, 0x40000000|readNr, 0, 0, 0)))
	}
	require.Equal(t, uint32(seccompRetAllow), evalBPF(prog, data(sys.auditArch, sys.sysSocket, syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK, 0)))
	require.Equal(t, uint32(seccompRetErrno|uint32(syscall.EPERM)), evalBPF(prog, data(sys.auditArch, sys.sysSocket, syscall.AF_UNIX, syscall.SOCK_STREAM, 0)))
	require.Equal(t, uint32(seccompRetErrno|uint32(syscall.EPERM)), evalBPF(prog, data(sys.auditArch, sys.sysSocket, syscall.AF_INET, syscall.SOCK_DGRAM, 0)))
	require.Equal(t, uint32(seccompRetErrno|uint32(syscall.EPERM)), evalBPF(prog, data(sys.auditArch, sys.sysSocket, syscall.AF_INET, syscall.SOCK_STREAM, 132)))
	require.Equal(t, uint32(seccompRetAllow), evalBPF(prog, data(sys.auditArch, sys.sysSocketpair, syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)))
	require.Equal(t, uint32(seccompRetErrno|uint32(syscall.EPERM)), evalBPF(prog, data(sys.auditArch, sys.sysSocketpair, syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)))
	require.Equal(t, uint32(seccompRetUserNotif), evalBPF(prog, data(sys.auditArch, sys.sysIoctl, 0, 0x8901, 0)))
	require.Equal(t, uint32(seccompRetAllow), evalBPF(prog, data(sys.auditArch, sys.sysPrctl, 39, 0, 0)))
	require.Equal(t, uint32(seccompRetUserNotif), evalBPF(prog, data(sys.auditArch, sys.sysPrctl, 4, 1, 0)))
	require.Equal(t, uint32(seccompRetAllow), evalBPF(prog, data(sys.auditArch, sys.sysTgkill, 12345, 12345, 0)))
	require.Equal(t, uint32(seccompRetErrno|uint32(syscall.EPERM)), evalBPF(prog, data(sys.auditArch, sys.sysTgkill, 99999, 99999, 0)))
	require.Equal(t, uint32(seccompRetAllow), evalBPF(prog, data(sys.auditArch, sys.sysUtimensat, 3, 0, 0)))
	require.Equal(t, uint32(seccompRetErrno|uint32(syscall.EPERM)), evalBPF(prog, data(sys.auditArch, sys.sysUtimensat, 3, 0x1000, 0)))
	require.Equal(t, uint32(seccompRetErrno|uint32(syscall.EPERM)), evalBPF(prog, data(sys.auditArch, sys.sysPrlimit64, 0x100000000, 7, 0)))
	require.Equal(t, uint32(seccompRetUserNotif), evalBPF(prog, data(sys.auditArch, sys.sysConnect, 3, 0x1000, 16)))
	require.Equal(t, uint32(seccompRetUserNotif), evalBPF(prog, data(sys.auditArch, sys.sysBind, 3, 0x1000, 16)))
	require.Equal(t, uint32(seccompRetUserNotif), evalBPF(prog, data(sys.auditArch, sys.sysListen, 3, 128, 0)))
	require.Equal(t, uint32(seccompRetErrno|uint32(syscall.ENOSYS)), evalBPF(prog, data(sys.auditArch, sys.sysClone3, 0, 0, 0)))
	require.Equal(t, uint32(seccompRetErrno|uint32(syscall.EPERM)), evalBPF(postProg, data(sys.auditArch, sys.sysSendmsg, 5, 0, 0)))
	require.Equal(t, uint32(seccompRetErrno|uint32(syscall.EPERM)), evalBPF(postProg, data(sys.auditArch, sys.sysSeccomp, 1, 0, 0)))
	require.Equal(t, uint32(seccompRetAllow), evalBPF(postProg, data(sys.auditArch, readNr, 0, 0, 0)))
	return len(prog), len(postProg)
}

func startPacketProbeListener(conn net.PacketConn) *atomic.Bool {
	recvd := &atomic.Bool{}
	go func() {
		buf := make([]byte, 128)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if n > 0 {
				recvd.Store(true)
			}
		}
	}()
	return recvd
}

func makeNonCloexecSecretPipe(t *testing.T) *os.File {
	t.Helper()
	var p [2]int
	require.NoError(t, syscall.Pipe2(p[:], 0))
	_, err := syscall.Write(p[1], []byte("secret"))
	require.NoError(t, err)
	require.NoError(t, syscall.Close(p[1]))
	return os.NewFile(uintptr(p[0]), "secret-fd6")
}

func reserveFreeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

func nonLoopbackIPv4() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				return ip4.String()
			}
		}
	}
	return "127.0.0.2"
}

func readSysctlTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "unavailable"
	}
	return strings.TrimSpace(string(b))
}

func assertDeniedByJailOrContainer(t *testing.T, got string) {
	t.Helper()
	assert.True(
		t,
		got == "function not implemented" || got == "operation not permitted",
		"expected ENOSYS (jail supervisor) or EPERM (outer container seccomp), got %q",
		got,
	)
}
