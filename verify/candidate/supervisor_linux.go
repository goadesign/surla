//go:build linux

package candidate

import (
	"context"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

type (
	// listenerHandoffMeta carries child metadata alongside the SCM_RIGHTS
	// seccomp notification listener descriptor on FD 5.
	listenerHandoffMeta struct {
		// ChildPID is the launcher's thread-group ID in the child process.
		ChildPID int `json:"child_pid"`
		// DevNullFD is the child's open /dev/null descriptor during CheckSandboxProbe.
		DevNullFD int `json:"dev_null_fd"`
		// CanaryAddr is the child memory address of checkSandboxCanaryWord.
		CanaryAddr uint64 `json:"canary_addr"`
	}

	// refusalTracker records deduplicated "surla jail refused <name> (<nr>)"
	// diagnostics from the seccomp user-notification supervisor.
	refusalTracker struct {
		// mu serializes updates to refused and writes to logFile.
		mu sync.Mutex
		// refused holds the deduplicated refusal strings in arrival order.
		refused []string
		// logFile receives refusal lines as they occur when non-nil.
		logFile *os.File
		// tgkillSupervisedDenials counts build-mode foreign-target tgkill
		// notifications denied by the supervisor on Landlock ABI 4-5 (or when
		// NoScope is forced).
		tgkillSupervisedDenials atomic.Int32
	}

	// activeDupSet tracks socket descriptors duplicated from a jailed child via
	// pidfd_getfd while a supervised connect, bind, or listen call is in flight.
	// Locking mu around registration, shutdownAll, and closeAndRemove prevents
	// a child-exit Shutdown from racing a handler's Close.
	activeDupSet struct {
		// mu serializes registration, shutdownAll, and closeAndRemove.
		mu sync.Mutex
		// closed is set once the jailed child has exited and shutdownAll ran.
		closed bool
		// fds holds the currently open duplicated socket descriptors.
		fds map[int]struct{}
	}
)

// newRefusalTracker returns an initialized refusalTracker.
func newRefusalTracker() *refusalTracker {
	return &refusalTracker{}
}

// resolveEffectiveNoScope queries the kernel's Landlock ABI once in the parent
// and sets cfg.NoScope when Landlock IPC scopes (ABI >= 6) are unavailable, so
// the child's Seccomp filter, the child's applyLandlock, and the parent's
// supervisor all act on the same effective cfg.NoScope value.
func resolveEffectiveNoScope(cfg *jailConfig) error {
	if cfg.CheckSandboxProbe {
		return nil
	}
	abi, errno := detectLandlockABI()
	if errno != 0 || abi < 4 {
		return fmt.Errorf("%w: landlock ABI %d (errno %v); Linux kernel >= 6.7 with Landlock ABI >= 4 is required", ErrIsolationUnavailable, abi, errno)
	}
	cfg.NoScope = cfg.NoScope || abi < 6
	return nil
}

// runSupervisedJailed runs cfg inside the Linux jail using background context.
func runSupervisedJailed(
	cfg jailConfig,
	outWriter io.Writer,
	tracker *refusalTracker,
	run func(*exec.Cmd) error,
) error {
	return runSupervisedJailedWithContext(context.Background(), cfg, outWriter, tracker, run)
}

// runSupervisedJailedWithContext launches cfg in the Linux jail, supervises
// seccomp user notifications until the child exits, and returns any launcher
// or process wait error annotated with recorded refusal strings.
func runSupervisedJailedWithContext(
	ctx context.Context,
	cfg jailConfig,
	outWriter io.Writer,
	tracker *refusalTracker,
	run func(*exec.Cmd) error,
) error {
	if err := resolveEffectiveNoScope(&cfg); err != nil {
		return err
	}
	cfgR, cfgW, err := os.Pipe()
	if err != nil {
		return err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		cfgR.Close()
		cfgW.Close()
		return err
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		cfgR.Close()
		cfgW.Close()
		errR.Close()
		errW.Close()
		return err
	}
	parentSock := fds[0]
	spChild := os.NewFile(uintptr(fds[1]), "sp-child")

	rawCfg, err := json.Marshal(cfg)
	if err != nil {
		cfgR.Close()
		cfgW.Close()
		errR.Close()
		errW.Close()
		syscall.Close(parentSock)
		spChild.Close()
		return err
	}
	if _, err := cfgW.Write(rawCfg); err != nil {
		cfgR.Close()
		cfgW.Close()
		errR.Close()
		errW.Close()
		syscall.Close(parentSock)
		spChild.Close()
		return err
	}
	if err := cfgW.Close(); err != nil {
		cfgR.Close()
		errR.Close()
		errW.Close()
		syscall.Close(parentSock)
		spChild.Close()
		return err
	}

	cmd := exec.CommandContext(ctx, "/proc/self/exe")
	cmd.Args = append([]string{surlaJailArgv0}, cfg.Args...)
	cmd.Dir = "/"
	cmd.Env = []string{}
	cmd.Stdout, cmd.Stderr = outWriter, outWriter
	cmd.ExtraFiles = []*os.File{cfgR, errW, spChild}

	var childDone atomic.Bool
	activeDups := newActiveDupSet()
	coordDone := make(chan struct{})
	go func() {
		defer close(coordDone)
		lfd, meta, recvErr := recvListenerFD(parentSock)
		if recvErr != nil {
			return
		}
		serveUserNotif(lfd, meta.ChildPID, cfg, tracker, &childDone, activeDups)
		syscall.Close(lfd)
	}()

	runErr := run(cmd)
	childDone.Store(true)
	activeDups.shutdownAll()
	syscall.Shutdown(parentSock, syscall.SHUT_RDWR)
	cfgR.Close()
	errW.Close()
	spChild.Close()
	<-coordDone
	syscall.Close(parentSock)
	errBytes, readErr := io.ReadAll(errR)
	errR.Close()
	if readErr != nil && runErr == nil {
		return readErr
	}
	if len(errBytes) > 0 {
		return fmt.Errorf("%w: launcher error (%v): %s", ErrIsolationUnavailable, runErr, string(errBytes))
	}
	if runErr != nil {
		if refused := tracker.list(); len(refused) > 0 {
			return fmt.Errorf("%w (%s)", runErr, strings.Join(refused, "; "))
		}
		return runErr
	}
	return nil
}

// readELFInterp returns the PT_INTERP dynamic loader path from path, or "" if
// path has no PT_INTERP program header.
func readELFInterp(path string) (string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			b, err := io.ReadAll(p.Open())
			if err != nil {
				return "", err
			}
			return strings.TrimRight(string(b), "\x00"), nil
		}
	}
	return "", nil
}

// checkStaticELFNoInterp verifies that path is a static ELF executable with no
// PT_INTERP dynamic loader, wrapping failures with ErrIsolationUnavailable.
func checkStaticELFNoInterp(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("%w: %s is not a valid ELF executable: %v", ErrIsolationUnavailable, path, err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			b, _ := io.ReadAll(p.Open())
			interp := strings.TrimRight(string(b), "\x00")
			return fmt.Errorf(
				"%w: candidate binary %s has ELF PT_INTERP %q; Linux candidate binaries must be static ELF executables (CGO_ENABLED=0)",
				ErrIsolationUnavailable,
				path,
				interp,
			)
		}
	}
	return nil
}

// recvListenerFD receives the seccomp notification listener descriptor and
// handoff metadata from sockFD.
func recvListenerFD(sockFD int) (int, listenerHandoffMeta, error) {
	var meta listenerHandoffMeta
	buf := make([]byte, 256)
	oob := make([]byte, 64)
	n, oobn, _, _, err := syscall.Recvmsg(sockFD, buf, oob, 0)
	if err != nil {
		return -1, meta, err
	}
	if n == 0 && oobn == 0 {
		return -1, meta, io.EOF
	}
	if n > 0 {
		if err := json.Unmarshal(buf[:n], &meta); err != nil {
			return -1, meta, fmt.Errorf("decode handoff metadata: %w", err)
		}
	}
	scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(scms) == 0 {
		return -1, meta, fmt.Errorf("parse scm: %w", err)
	}
	fds, err := syscall.ParseUnixRights(&scms[0])
	if err != nil || len(fds) == 0 {
		return -1, meta, fmt.Errorf("parse rights: %w", err)
	}
	return fds[0], meta, nil
}

// newActiveDupSet returns an initialized activeDupSet.
func newActiveDupSet() *activeDupSet {
	return &activeDupSet{fds: make(map[int]struct{})}
}

// serveUserNotif receives seccomp notifications from listenerFD and dispatches
// each notification to a bounded worker pool. The caller owns closing
// listenerFD after serveUserNotif returns.
func serveUserNotif(
	listenerFD int,
	childTGID int,
	cfg jailConfig,
	tracker *refusalTracker,
	childDone *atomic.Bool,
	activeDups *activeDupSet,
) {
	sys := currentArchSyscalls()

	r1, _, errno := syscall.Syscall(uintptr(sys.sysPidfdOpen), uintptr(childTGID), 0, 0)
	pidfd := -1
	if errno == 0 {
		pidfd = int(r1)
		defer syscall.Close(pidfd)
	}

	var sendMu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 32)

	for {
		var notif seccompNotif
		_, _, errno := syscall.Syscall(
			syscall.SYS_IOCTL,
			uintptr(listenerFD),
			seccompIoctlNotifRecv,
			uintptr(unsafe.Pointer(&notif)),
		)
		if errno != 0 {
			if errno == syscall.EINTR {
				continue
			}
			if errno == syscall.ENOENT && !childDone.Load() {
				runtime.Gosched()
				continue
			}
			break
		}

		notifCopy := notif
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			resp := handleOneNotif(sys, listenerFD, pidfd, childTGID, &notifCopy, cfg, tracker, activeDups)
			sendMu.Lock()
			syscall.Syscall(
				syscall.SYS_IOCTL,
				uintptr(listenerFD),
				seccompIoctlNotifSend,
				uintptr(unsafe.Pointer(&resp)),
			)
			sendMu.Unlock()
		}()
	}
	wg.Wait()
}

// notifIDValid reports whether notification id is still valid on listenerFD.
func notifIDValid(listenerFD int, id uint64) bool {
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(listenerFD),
		seccompIoctlNotifIDValid,
		uintptr(unsafe.Pointer(&id)),
	)
	return errno == 0
}

// handleOneNotif processes a single seccomp user notification.
func handleOneNotif(
	sys archSyscalls,
	listenerFD, pidfd, childTGID int,
	notif *seccompNotif,
	cfg jailConfig,
	tracker *refusalTracker,
	activeDups *activeDupSet,
) seccompNotifResp {
	nr := int(notif.Data.Nr)

	if cfg.Mode == modeBuild && cfg.NoScope && nr == sys.sysTgkill {
		targetTGID := int(notif.Data.Args[0])
		targetTID := int(notif.Data.Args[1])
		sig := int(notif.Data.Args[2])
		if targetTGID <= 0 || targetTID <= 0 {
			tracker.tgkillSupervisedDenials.Add(1)
			return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.EINVAL)}
		}
		taskPath := fmt.Sprintf("/proc/%d/task/%d", targetTGID, notif.Pid)
		if _, err := os.Stat(taskPath); err != nil {
			tracker.tgkillSupervisedDenials.Add(1)
			return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.EPERM)}
		}
		if !notifIDValid(listenerFD, notif.ID) {
			return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.ESRCH)}
		}
		_, _, errno := syscall.RawSyscall(uintptr(sys.sysTgkill), uintptr(targetTGID), uintptr(targetTID), uintptr(sig))
		if errno != 0 {
			return seccompNotifResp{ID: notif.ID, Error: -int32(errno)}
		}
		return seccompNotifResp{ID: notif.ID, Val: 0}
	}

	if cfg.Mode == modeExecWithPorts && (cfg.ListenPort > 0 || len(cfg.ConnectPorts) > 0) && nr == sys.sysListen {
		if cfg.ListenPort <= 0 || pidfd < 0 {
			return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.EACCES)}
		}
		sockfd := int(int32(notif.Data.Args[0]))
		backlog := int(int32(notif.Data.Args[1]))
		if sockfd < 0 {
			return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.EBADF)}
		}
		dupR1, _, dupErrno := syscall.Syscall(uintptr(sys.sysPidfdGetfd), uintptr(pidfd), uintptr(sockfd), 0)
		if dupErrno != 0 {
			return seccompNotifResp{ID: notif.ID, Error: -int32(dupErrno)}
		}
		dupFD := int(dupR1)
		if !activeDups.add(dupFD) {
			return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.ESRCH)}
		}
		defer activeDups.closeAndRemove(dupFD)

		if !notifIDValid(listenerFD, notif.ID) {
			return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.ESRCH)}
		}
		if !validateBoundLoopbackSocket(dupFD, cfg.ListenPort) {
			return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.EACCES)}
		}
		if err := syscall.Listen(dupFD, backlog); err != nil {
			return seccompNotifResp{ID: notif.ID, Error: -int32(errnoFromErr(err))}
		}
		return seccompNotifResp{ID: notif.ID, Val: 0}
	}

	isBind := cfg.Mode == modeExecWithPorts && (cfg.ListenPort > 0 || len(cfg.ConnectPorts) > 0) && nr == sys.sysBind
	isConnect := cfg.Mode == modeExecWithPorts && (cfg.ListenPort > 0 || len(cfg.ConnectPorts) > 0) && nr == sys.sysConnect
	if !isBind && !isConnect {
		entry := fmt.Sprintf("surla jail refused %s (%d)", sys.syscallName(nr), nr)
		tracker.record(entry)
		return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.ENOSYS)}
	}

	sockfd := int(int32(notif.Data.Args[0]))
	addrPtr := uintptr(notif.Data.Args[1])
	addrLen := int(uint32(notif.Data.Args[2]))
	if sockfd < 0 || addrLen < 2 || addrLen > 128 {
		return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.EACCES)}
	}

	rawAddr := make([]byte, addrLen)
	localIov := syscall.Iovec{Base: &rawAddr[0], Len: uint64(addrLen)}
	remoteIov := struct {
		Base uintptr
		Len  uint64
	}{Base: addrPtr, Len: uint64(addrLen)}
	_, _, errno := syscall.Syscall6(
		uintptr(sys.sysProcessVMReadv),
		uintptr(childTGID),
		uintptr(unsafe.Pointer(&localIov)),
		1,
		uintptr(unsafe.Pointer(&remoteIov)),
		1,
		0,
	)
	if errno != 0 {
		return seccompNotifResp{ID: notif.ID, Error: -int32(errno)}
	}

	if !notifIDValid(listenerFD, notif.ID) {
		return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.ESRCH)}
	}

	sa, family, _, ok := parseAndValidateLoopbackSockaddr(rawAddr, isBind, cfg)
	if !ok {
		return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.EACCES)}
	}
	if pidfd < 0 {
		return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.ENOSYS)}
	}
	dupR1, _, dupErrno := syscall.Syscall(uintptr(sys.sysPidfdGetfd), uintptr(pidfd), uintptr(sockfd), 0)
	if dupErrno != 0 {
		return seccompNotifResp{ID: notif.ID, Error: -int32(dupErrno)}
	}
	dupFD := int(dupR1)
	if !activeDups.add(dupFD) {
		return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.ESRCH)}
	}
	defer activeDups.closeAndRemove(dupFD)

	if !notifIDValid(listenerFD, notif.ID) {
		return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.ESRCH)}
	}

	soDomain, err := syscall.GetsockoptInt(dupFD, syscall.SOL_SOCKET, syscall.SO_DOMAIN)
	if err != nil || soDomain != family {
		return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.EACCES)}
	}
	soType, err := syscall.GetsockoptInt(dupFD, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil || soType != syscall.SOCK_STREAM {
		return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.EACCES)}
	}
	soProto, err := syscall.GetsockoptInt(dupFD, syscall.SOL_SOCKET, syscall.SO_PROTOCOL)
	if err != nil || (soProto != 0 && soProto != syscall.IPPROTO_TCP) {
		return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.EACCES)}
	}

	if isBind {
		if family == syscall.AF_INET6 {
			v6only, err := syscall.GetsockoptInt(dupFD, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY)
			if err != nil || v6only != 1 {
				return seccompNotifResp{ID: notif.ID, Error: -int32(syscall.EACCES)}
			}
		}
		if err := syscall.Bind(dupFD, sa); err != nil {
			return seccompNotifResp{ID: notif.ID, Error: -int32(errnoFromErr(err))}
		}
		return seccompNotifResp{ID: notif.ID, Val: 0}
	}

	if err := syscall.Connect(dupFD, sa); err != nil {
		return seccompNotifResp{ID: notif.ID, Error: -int32(errnoFromErr(err))}
	}
	return seccompNotifResp{ID: notif.ID, Val: 0}
}

// validateBoundLoopbackSocket verifies that dupFD is a TCP SOCK_STREAM socket
// already bound to 127.0.0.0/8:listenPort or [::1]:listenPort (with
// IPV6_V6ONLY == 1) before listen().
func validateBoundLoopbackSocket(dupFD, listenPort int) bool {
	soType, err := syscall.GetsockoptInt(dupFD, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil || soType != syscall.SOCK_STREAM {
		return false
	}
	soProto, err := syscall.GetsockoptInt(dupFD, syscall.SOL_SOCKET, syscall.SO_PROTOCOL)
	if err != nil || (soProto != 0 && soProto != syscall.IPPROTO_TCP) {
		return false
	}
	sa, err := syscall.Getsockname(dupFD)
	if err != nil {
		return false
	}
	switch a := sa.(type) {
	case *syscall.SockaddrInet4:
		return a.Port == listenPort && a.Addr[0] == 127
	case *syscall.SockaddrInet6:
		v6only, err := syscall.GetsockoptInt(dupFD, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY)
		if err != nil || v6only != 1 {
			return false
		}
		return a.Port == listenPort && a.Addr == [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	default:
		return false
	}
}

// errnoFromErr extracts a syscall.Errno from err or returns EACCES.
func errnoFromErr(err error) syscall.Errno {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}
	return syscall.EACCES
}

// parseAndValidateLoopbackSockaddr parses raw sockaddr bytes and checks that
// the address is a loopback IPv4 or IPv6 address on an allowed port in cfg.
func parseAndValidateLoopbackSockaddr(
	raw []byte,
	isBind bool,
	cfg jailConfig,
) (syscall.Sockaddr, int, int, bool) {
	if len(raw) < 2 {
		return nil, 0, 0, false
	}
	family := int(binary.LittleEndian.Uint16(raw[0:2]))
	switch family {
	case syscall.AF_INET:
		if len(raw) < 16 {
			return nil, 0, 0, false
		}
		port := int(binary.BigEndian.Uint16(raw[2:4]))
		var ip [4]byte
		copy(ip[:], raw[4:8])
		if ip[0] != 127 || !portAllowed(port, isBind, cfg) {
			return nil, 0, 0, false
		}
		return &syscall.SockaddrInet4{Port: port, Addr: ip}, syscall.AF_INET, port, true

	case syscall.AF_INET6:
		if len(raw) < 28 {
			return nil, 0, 0, false
		}
		port := int(binary.BigEndian.Uint16(raw[2:4]))
		var ip [16]byte
		copy(ip[:], raw[8:24])
		scopeID := binary.LittleEndian.Uint32(raw[24:28])
		isLoopbackV6 := ip == [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
		isV4MappedLoopback := !isBind &&
			ip[0] == 0 && ip[1] == 0 && ip[2] == 0 && ip[3] == 0 &&
			ip[4] == 0 && ip[5] == 0 && ip[6] == 0 && ip[7] == 0 &&
			ip[8] == 0 && ip[9] == 0 && ip[10] == 0xff && ip[11] == 0xff &&
			ip[12] == 127
		if (!isLoopbackV6 && !isV4MappedLoopback) || !portAllowed(port, isBind, cfg) {
			return nil, 0, 0, false
		}
		return &syscall.SockaddrInet6{Port: port, ZoneId: scopeID, Addr: ip}, syscall.AF_INET6, port, true

	default:
		return nil, 0, 0, false
	}
}

// portAllowed reports whether port is permitted by cfg for bind or connect.
func portAllowed(port int, isBind bool, cfg jailConfig) bool {
	if port <= 0 || port > 65535 {
		return false
	}
	if isBind {
		return cfg.ListenPort > 0 && port == cfg.ListenPort
	}
	return slices.Contains(cfg.ConnectPorts, port)
}

// record adds entry to r if not already present and writes it to r.logFile.
func (r *refusalTracker) record(entry string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(r.refused, entry) {
		r.refused = append(r.refused, entry)
		if r.logFile != nil {
			fmt.Fprintf(r.logFile, "%s\n", entry)
		}
	}
}

// list returns a copy of the recorded refusal strings.
func (r *refusalTracker) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.refused)
}

// add registers fd in s, or closes fd immediately and returns false if s has
// already been shut down.
func (s *activeDupSet) add(fd int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		syscall.Close(fd)
		return false
	}
	s.fds[fd] = struct{}{}
	return true
}

// closeAndRemove removes fd from s and closes it under s.mu so Shutdown never
// races Close.
func (s *activeDupSet) closeAndRemove(fd int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.fds[fd]; !ok {
		return
	}
	delete(s.fds, fd)
	syscall.Close(fd)
}

// shutdownAll marks s closed and shuts down every registered socket descriptor
// to unblock any in-flight Connect call when the jailed child exits.
func (s *activeDupSet) shutdownAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for fd := range s.fds {
		syscall.Shutdown(fd, syscall.SHUT_RDWR)
	}
}
