//go:build linux

// Command linuxprobe is a static Cgo-free helper binary invoked by
// jail_linux_test.go and jail_amd64_linux_test.go to exercise every Linux
// Landlock, Seccomp-BPF, and SECCOMP_RET_USER_NOTIF invariant and negative
// control from inside a real child process.
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	ipprotoSCTP              = 132
	ipprotoMPTCP             = 262
	msgFastopen              = 0x20000000
	fSetown                  = 8
	fSetsig                  = 10
	fioSetown                = 0x8901
	fioAsync                 = 0x5452
	keyctlJoinSessionKeyring = 1
	prSetDumpable            = 4
	prGetNoNewPrivs          = 39
)

type (
	// probeSyscalls holds architecture-specific syscall numbers used by raw
	// syscall probes.
	probeSyscalls struct {
		sysKeyctl          uintptr
		sysMemfdCreate     uintptr
		sysExecveat        uintptr
		sysIoUringSetup    uintptr
		sysPrlimit64       uintptr
		sysSchedSetaff     uintptr
		sysSchedGetaff     uintptr
		sysIoprioSet       uintptr
		sysFcntl           uintptr
		sysIoctl           uintptr
		sysPrctl           uintptr
		sysKill            uintptr
		sysTkill           uintptr
		sysTgkill          uintptr
		sysPidfdSendSignal uintptr
		sysConnect         uintptr
		sysSendmsg         uintptr
		sysSendmmsg        uintptr
		sysUtimensat       uintptr
	}

	buildExecEscapesResult struct {
		ExecWorkDir string `json:"exec_workdir"`
		ExecGit     string `json:"exec_git"`
		ExecLdLinux string `json:"exec_ld_linux"`
	}

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

	listenWithoutBindResult struct {
		Listen4Err string `json:"listen4_err"`
		Bound4Type string `json:"bound4_type"`
		Listen6Err string `json:"listen6_err"`
		Bound6Type string `json:"bound6_type"`
	}

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

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	sys := syscallsForArch()
	enc := json.NewEncoder(os.Stdout)

	switch os.Args[1] {
	case "bystander-sleep":
		time.Sleep(30 * time.Second)

	case "noop":
		fmt.Println("noop-ok")

	case "fail-on-refused-syscall":
		_, _, errno := syscall.Syscall(sys.sysKeyctl, keyctlJoinSessionKeyring, 0, 0)
		_, errSid := syscall.Setsid()
		if errno != 0 || errSid != nil {
			os.Exit(1)
		}

	case "amd64-x32-syscall":
		syscall.RawSyscall(0x40000000|39, 0, 0, 0)
		fmt.Println("survived")

	case "amd64-ia32-int80":
		code, err := syscall.Mmap(
			-1,
			0,
			4096,
			syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC,
			syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS,
		)
		if err != nil {
			fmt.Println("mmap-err:" + err.Error())
			os.Exit(1)
		}
		// mov eax, 20; int 0x80; ret
		copy(code, []byte{0xb8, 0x14, 0x00, 0x00, 0x00, 0xcd, 0x80, 0xc3})
		entry := uintptr(unsafe.Pointer(&code[0]))
		fnVal := &entry
		fn := *(*func())(unsafe.Pointer(&fnVal))
		fn()
		fmt.Println("survived")

	case "build-exec-escapes":
		probeInWork := os.Args[2]
		ldLinux := os.Args[3]
		enc.Encode(buildExecEscapesResult{
			ExecWorkDir: errStr(exec.Command(probeInWork, "noop").Run()),
			ExecGit:     errStr(exec.Command("/usr/lib/git-core/git", "--version").Run()),
			ExecLdLinux: errStr(exec.Command(ldLinux, probeInWork, "noop").Run()),
		})

	case "build-signals-and-dgram":
		parentPID, _ := strconv.Atoi(os.Args[2])
		bystanderPID, _ := strconv.Atoi(os.Args[3])
		dgramPath := os.Args[4]
		absDgramName := os.Args[5]

		_, _, eKill := syscall.RawSyscall(sys.sysKill, uintptr(parentPID), 0, 0)
		_, _, eTkill := syscall.RawSyscall(sys.sysTkill, uintptr(parentPID), 0, 0)
		_, _, eTgkillParent := syscall.RawSyscall(sys.sysTgkill, uintptr(parentPID), uintptr(parentPID), 0)
		_, _, eTgkillSelf := syscall.RawSyscall(sys.sysTgkill, uintptr(os.Getpid()), uintptr(syscall.Gettid()), 0)

		var eIoctlSetown syscall.Errno
		if sp, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0); err == nil {
			pid32 := int32(bystanderPID)
			one := int32(1)
			_, _, eIoctlSetown = syscall.Syscall(sys.sysIoctl, uintptr(sp[0]), fioSetown, uintptr(unsafe.Pointer(&pid32)))
			syscall.Syscall(sys.sysIoctl, uintptr(sp[0]), fioAsync, uintptr(unsafe.Pointer(&one)))
			syscall.Write(sp[1], []byte("x"))
			syscall.Close(sp[0])
			syscall.Close(sp[1])
		}

		spDgram, errSpDgram := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
		var eSendmsgPath, eSendmsgAbs syscall.Errno
		if errSpDgram == nil {
			eSendmsgPath = sendmsgUnixDgram(sys, spDgram[0], dgramPath, []byte("dgram-from-build"))
			eSendmsgAbs = sendmsgUnixDgram(sys, spDgram[0], absDgramName, []byte("dgram-from-build"))
			syscall.Close(spDgram[0])
			syscall.Close(spDgram[1])
		} else if spStream, errS := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0); errS == nil {
			eSendmsgPath = sendmsgUnixDgram(sys, spStream[0], dgramPath, []byte("dgram-from-build"))
			eSendmsgAbs = sendmsgUnixDgram(sys, spStream[0], absDgramName, []byte("dgram-from-build"))
			syscall.Close(spStream[0])
			syscall.Close(spStream[1])
		}
		enc.Encode(buildSignalsDgramResult{
			KillParent:   errnoStr(eKill),
			TkillParent:  errnoStr(eTkill),
			TgkillParent: errnoStr(eTgkillParent),
			TgkillSelf:   errnoStr(eTgkillSelf),
			IoctlSetown:  errnoStr(eIoctlSetown),
			SpDgram:      errStr(errSpDgram),
			SendmsgPath:  errnoStr(eSendmsgPath),
			SendmsgAbs:   errnoStr(eSendmsgAbs),
		})

	case "listen-without-bind-only":
		fd4, _ := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
		errL4 := syscall.Listen(fd4, 128)
		sa4, _ := syscall.Getsockname(fd4)
		syscall.Close(fd4)

		fd6, _ := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
		errL6 := syscall.Listen(fd6, 128)
		sa6, _ := syscall.Getsockname(fd6)
		syscall.Close(fd6)
		enc.Encode(listenWithoutBindResult{
			Listen4Err: errStr(errL4),
			Bound4Type: fmt.Sprintf("%T", sa4),
			Listen6Err: errStr(errL6),
			Bound6Type: fmt.Sprintf("%T", sa6),
		})

	case "net-suite":
		enc.Encode(runNetSuite(sys, os.Args[2:]))

	case "attack-suite":
		enc.Encode(runAttackSuiteProbe(sys, os.Args[2:]))
	}
}

func syscallsForArch() probeSyscalls {
	if runtime.GOARCH == "amd64" {
		return probeSyscalls{
			sysKeyctl:          250,
			sysMemfdCreate:     319,
			sysExecveat:        322,
			sysIoUringSetup:    425,
			sysPrlimit64:       302,
			sysSchedSetaff:     203,
			sysSchedGetaff:     204,
			sysIoprioSet:       251,
			sysFcntl:           72,
			sysIoctl:           16,
			sysPrctl:           157,
			sysKill:            62,
			sysTkill:           200,
			sysTgkill:          234,
			sysPidfdSendSignal: 424,
			sysConnect:         42,
			sysSendmsg:         46,
			sysSendmmsg:        307,
			sysUtimensat:       280,
		}
	}
	return probeSyscalls{
		sysKeyctl:          219,
		sysMemfdCreate:     279,
		sysExecveat:        281,
		sysIoUringSetup:    425,
		sysPrlimit64:       261,
		sysSchedSetaff:     122,
		sysSchedGetaff:     123,
		sysIoprioSet:       30,
		sysFcntl:           25,
		sysIoctl:           29,
		sysPrctl:           167,
		sysKill:            129,
		sysTkill:           130,
		sysTgkill:          131,
		sysPidfdSendSignal: 424,
		sysConnect:         203,
		sysSendmsg:         211,
		sysSendmmsg:        269,
		sysUtimensat:       88,
	}
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func errnoStr(errno syscall.Errno) string {
	if errno == 0 {
		return ""
	}
	return errno.Error()
}

func sendUnixDgram(fd int, name string, payload []byte) error {
	sa := &syscall.SockaddrUnix{Name: name}
	return syscall.Sendto(fd, payload, 0, sa)
}

func sendmsgUnixDgram(sys probeSyscalls, fd int, name string, payload []byte) syscall.Errno {
	var rawSa [110]byte
	binary.LittleEndian.PutUint16(rawSa[0:2], syscall.AF_UNIX)
	var saLen uint32
	if strings.HasPrefix(name, "@") {
		rawSa[2] = 0
		copy(rawSa[3:], name[1:])
		saLen = uint32(3 + len(name) - 1)
	} else {
		copy(rawSa[2:], name)
		saLen = uint32(2 + len(name) + 1)
	}
	iov := syscall.Iovec{Base: &payload[0], Len: uint64(len(payload))}
	msg := syscall.Msghdr{
		Name:    &rawSa[0],
		Namelen: saLen,
		Iov:     &iov,
		Iovlen:  1,
	}
	_, _, errno := syscall.Syscall(sys.sysSendmsg, uintptr(fd), uintptr(unsafe.Pointer(&msg)), 0)
	return errno
}

func runNetSuite(sys probeSyscalls, args []string) netSuiteResult {
	listenPort, _ := strconv.Atoi(args[0])
	allowedPort, _ := strconv.Atoi(args[1])
	unlistedPort, _ := strconv.Atoi(args[2])
	bridgeIP := args[3]
	bridgePort, _ := strconv.Atoi(args[4])

	var res netSuiteResult

	c, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", allowedPort), 2*time.Second)
	res.DialAllowedErr = errStr(err)
	if err == nil {
		b, _ := io.ReadAll(c)
		res.DialAllowedBody = string(b)
		c.Close()
	}

	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", listenPort))
	res.BindListen4Err = errStr(err)
	if err == nil {
		go func() {
			conn, acceptErr := ln.Accept()
			if acceptErr == nil {
				conn.Write([]byte("self-ok"))
				conn.Close()
			}
		}()
		sc, dialErr := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", listenPort), 2*time.Second)
		res.SelfDialErr = errStr(dialErr)
		if dialErr == nil {
			b, _ := io.ReadAll(sc)
			res.SelfDialBody = string(b)
			sc.Close()
		}
		ln.Close()
	}

	fdUnbound4, _ := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	res.ListenUnbound4Err = errStr(syscall.Listen(fdUnbound4, 128))
	syscall.Close(fdUnbound4)

	fdUnbound6, _ := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	res.ListenUnbound6Err = errStr(syscall.Listen(fdUnbound6, 128))
	syscall.Close(fdUnbound6)

	cBridge, errBridge := net.DialTimeout("tcp4", fmt.Sprintf("%s:%d", bridgeIP, bridgePort), 1*time.Second)
	if cBridge != nil {
		cBridge.Close()
	}
	res.BridgeDialErr = errStr(errBridge)

	fd, _ := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	res.UnlistedConnectErr = errStr(syscall.Connect(fd, &syscall.SockaddrInet4{Port: unlistedPort, Addr: [4]byte{127, 0, 0, 1}}))
	syscall.Close(fd)

	fd, _ = syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	res.Wild4BindErr = errStr(syscall.Bind(fd, &syscall.SockaddrInet4{Port: listenPort, Addr: [4]byte{0, 0, 0, 0}}))
	syscall.Close(fd)

	fd, _ = syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 1)
	res.Wild6BindErr = errStr(syscall.Bind(fd, &syscall.SockaddrInet6{Port: listenPort}))
	syscall.Close(fd)

	fd, _ = syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 0)
	res.Dual6BindErr = errStr(syscall.Bind(fd, &syscall.SockaddrInet6{Port: listenPort, Addr: [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}}))
	syscall.Close(fd)

	fd, _ = syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 1)
	errLoop6 := syscall.Bind(fd, &syscall.SockaddrInet6{Port: listenPort, Addr: [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}})
	res.Loop6BindErr = errStr(errLoop6)
	if errLoop6 == nil {
		res.Loop6ListenErr = errStr(syscall.Listen(fd, 128))
	}
	syscall.Close(fd)

	fd, _ = syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	res.UnlistedBindErr = errStr(syscall.Bind(fd, &syscall.SockaddrInet4{Port: unlistedPort, Addr: [4]byte{127, 0, 0, 1}}))
	syscall.Close(fd)

	fd, _ = syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	var rawUnspec [16]byte
	_, _, errUnspec := syscall.Syscall(sys.sysConnect, uintptr(fd), uintptr(unsafe.Pointer(&rawUnspec[0])), 16)
	syscall.Close(fd)
	res.UnspecConnectErr = errnoStr(errUnspec)

	bridgeIP4 := net.ParseIP(bridgeIP).To4()
	var racingSa [16]byte
	binary.LittleEndian.PutUint16(racingSa[0:2], syscall.AF_INET)
	binary.BigEndian.PutUint16(racingSa[2:4], uint16(bridgePort))
	stopFlip := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopFlip:
				return
			default:
				racingSa[4], racingSa[5], racingSa[6], racingSa[7] = 127, 0, 0, 1
				racingSa[4], racingSa[5], racingSa[6], racingSa[7] = bridgeIP4[0], bridgeIP4[1], bridgeIP4[2], bridgeIP4[3]
			}
		}
	}()
	for i := 0; i < 200; i++ {
		rfd, sockErr := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0)
		if sockErr == nil {
			syscall.Syscall(sys.sysConnect, uintptr(rfd), uintptr(unsafe.Pointer(&racingSa[0])), 16)
			syscall.Close(rfd)
		}
	}
	close(stopFlip)
	res.TOCTOUCompleted = true
	return res
}

func runAttackSuiteProbe(sys probeSyscalls, args []string) attackSuiteResult {
	unixPath := args[0]
	absName := args[1]
	outsideFile := args[2]
	outsidePrivateDir := args[3]
	parentPID, _ := strconv.Atoi(args[4])
	connectPort, _ := strconv.Atoi(args[5])
	bridgeIP := args[6]
	bridgePort, _ := strconv.Atoi(args[7])
	bystanderPID, _ := strconv.Atoi(args[8])
	dgramPath := args[9]
	absDgramName := args[10]

	var res attackSuiteResult

	c1, err1 := net.Dial("unix", unixPath)
	if c1 != nil {
		c1.Close()
	}
	res.UnixPathnameErr = errStr(err1)

	c2, err2 := net.Dial("unix", absName)
	if c2 != nil {
		c2.Close()
	}
	res.UnixAbstractErr = errStr(err2)

	sp, errSp := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if errSp == nil {
		syscall.Close(sp[0])
		syscall.Close(sp[1])
	}
	res.SocketpairStream = errStr(errSp)

	spDgram, errSpDgram := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	var errSendtoPath error
	var errSendmsgPath, errSendmsgAbs syscall.Errno
	if errSpDgram == nil {
		errSendtoPath = sendUnixDgram(spDgram[0], dgramPath, []byte("dgram-sendto-path"))
		errSendmsgPath = sendmsgUnixDgram(sys, spDgram[0], dgramPath, []byte("dgram-sendmsg-path"))
		errSendmsgAbs = sendmsgUnixDgram(sys, spDgram[0], absDgramName, []byte("dgram-sendmsg-abs"))
		syscall.Close(spDgram[0])
		syscall.Close(spDgram[1])
	} else if spStream, errS := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0); errS == nil {
		errSendtoPath = sendUnixDgram(spStream[0], dgramPath, []byte("dgram-sendto-path"))
		errSendmsgPath = sendmsgUnixDgram(sys, spStream[0], dgramPath, []byte("dgram-sendmsg-path"))
		errSendmsgAbs = sendmsgUnixDgram(sys, spStream[0], absDgramName, []byte("dgram-sendmsg-abs"))
		syscall.Close(spStream[0])
		syscall.Close(spStream[1])
	}
	res.SocketpairDgram = errStr(errSpDgram)
	res.SendtoPath = errStr(errSendtoPath)
	res.SendmsgPath = errnoStr(errSendmsgPath)
	res.SendmsgAbs = errnoStr(errSendmsgAbs)

	tfoFD, errTfoSock := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if errTfoSock == nil {
		res.TfoSendto = errStr(syscall.Sendto(tfoFD, []byte("hi"), msgFastopen, &syscall.SockaddrInet4{Port: connectPort, Addr: [4]byte{127, 0, 0, 1}}))
		_, _, eMsg := syscall.Syscall(sys.sysSendmsg, uintptr(tfoFD), 0, msgFastopen)
		res.TfoSendmsg = errnoStr(eMsg)
		_, _, eMmsg := syscall.Syscall6(sys.sysSendmmsg, uintptr(tfoFD), 0, 0, msgFastopen, 0, 0)
		res.TfoSendmmsg = errnoStr(eMmsg)
		syscall.Close(tfoFD)
	}

	sSCTP, errSCTP := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, ipprotoSCTP)
	if sSCTP >= 0 {
		syscall.Close(sSCTP)
	}
	sMPTCP, errMPTCP := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, ipprotoMPTCP)
	if sMPTCP >= 0 {
		syscall.Close(sMPTCP)
	}
	sUDP, errUDP := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if sUDP >= 0 {
		syscall.Close(sUDP)
	}
	res.SCTPErr = errStr(errSCTP)
	res.MPTCPErr = errStr(errMPTCP)
	res.UDPErr = errStr(errUDP)

	cBridge, errBridge := net.DialTimeout("tcp4", fmt.Sprintf("%s:%d", bridgeIP, bridgePort), 1*time.Second)
	res.BridgeErr = errStr(errBridge)
	if errBridge == nil {
		b, _ := io.ReadAll(cBridge)
		res.BridgeBody = string(b)
		cBridge.Close()
	}

	res.ChmodOut = errStr(os.Chmod(outsideFile, 0o600))
	os.WriteFile("inside.txt", []byte("x"), 0o600)
	res.ChmodIn = errStr(os.Chmod("inside.txt", 0o644))
	res.ChownOut = errStr(os.Chown(outsideFile, os.Getuid(), os.Getgid()))
	now := time.Now()
	res.ChtimesOut = errStr(os.Chtimes(outsideFile, now, now))
	res.SetxattrOut = errStr(syscall.Setxattr(outsideFile, "user.probe", []byte("1"), 0))
	inFile, _ := os.OpenFile("inside.txt", os.O_RDWR, 0)
	if inFile != nil {
		res.FchmodIn = errStr(inFile.Chmod(0o640))
		_, _, errFutimensIn := syscall.Syscall6(sys.sysUtimensat, inFile.Fd(), 0, 0, 0, 0, 0)
		res.FutimensIn = errnoStr(errFutimensIn)
		inFile.Close()
	}

	_, _, errKeyctl := syscall.Syscall(sys.sysKeyctl, keyctlJoinSessionKeyring, 0, 0)
	res.KeyctlErr = errnoStr(errKeyctl)

	var rlim syscall.Rlimit
	_, _, errPrlimParent := syscall.RawSyscall6(sys.sysPrlimit64, uintptr(parentPID), uintptr(syscall.RLIMIT_NOFILE), 0, uintptr(unsafe.Pointer(&rlim)), 0, 0)
	_, _, errPrlimSelf := syscall.RawSyscall6(sys.sysPrlimit64, 0, uintptr(syscall.RLIMIT_NOFILE), 0, uintptr(unsafe.Pointer(&rlim)), 0, 0)
	errSetprioParent := syscall.Setpriority(syscall.PRIO_PROCESS, parentPID, 10)
	var mask [16]uint64
	mask[0] = 1
	_, _, errSetaffParent := syscall.RawSyscall(sys.sysSchedSetaff, uintptr(parentPID), 8, uintptr(unsafe.Pointer(&mask[0])))
	_, _, errGetaffSelf := syscall.RawSyscall(sys.sysSchedGetaff, 0, 128, uintptr(unsafe.Pointer(&mask[0])))
	_, _, errIoprioParent := syscall.RawSyscall(sys.sysIoprioSet, 1, uintptr(parentPID), 0x4000)
	res.PrlimitParent = errnoStr(errPrlimParent)
	res.PrlimitSelf = errnoStr(errPrlimSelf)
	res.SetprioParent = errStr(errSetprioParent)
	res.SetaffParent = errnoStr(errSetaffParent)
	res.GetaffSelf = errnoStr(errGetaffSelf)
	res.IoprioParent = errnoStr(errIoprioParent)

	var pfd [2]int
	syscall.Pipe2(pfd[:], syscall.O_CLOEXEC)
	_, _, errSetownParent := syscall.Syscall(sys.sysFcntl, uintptr(pfd[0]), fSetown, uintptr(parentPID))
	_, _, errSetownZero := syscall.Syscall(sys.sysFcntl, uintptr(pfd[0]), fSetown, 0)
	_, _, errSetsigNonZero := syscall.Syscall(sys.sysFcntl, uintptr(pfd[0]), fSetsig, uintptr(syscall.SIGKILL))
	_, _, errSetsigZero := syscall.Syscall(sys.sysFcntl, uintptr(pfd[0]), fSetsig, 0)
	syscall.Close(pfd[0])
	syscall.Close(pfd[1])
	res.FcntlSetownParent = errnoStr(errSetownParent)
	res.FcntlSetownZero = errnoStr(errSetownZero)
	res.FcntlSetsigKill = errnoStr(errSetsigNonZero)
	res.FcntlSetsigZero = errnoStr(errSetsigZero)

	if spIoctl, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0); err == nil {
		pid32 := int32(bystanderPID)
		one := int32(1)
		_, _, errIoctlSetown := syscall.Syscall(sys.sysIoctl, uintptr(spIoctl[0]), fioSetown, uintptr(unsafe.Pointer(&pid32)))
		_, _, errIoctlAsync := syscall.Syscall(sys.sysIoctl, uintptr(spIoctl[0]), fioAsync, uintptr(unsafe.Pointer(&one)))
		res.IoctlSetown = errnoStr(errIoctlSetown)
		res.IoctlAsync = errnoStr(errIoctlAsync)
		syscall.Write(spIoctl[1], []byte("x"))
		syscall.Close(spIoctl[0])
		syscall.Close(spIoctl[1])
	}

	_, _, errKill := syscall.RawSyscall(sys.sysKill, uintptr(parentPID), 0, 0)
	_, _, errTkill := syscall.RawSyscall(sys.sysTkill, uintptr(parentPID), 0, 0)
	_, _, errTgkillParent := syscall.RawSyscall(sys.sysTgkill, uintptr(parentPID), uintptr(parentPID), 0)
	_, _, errTgkillSelf := syscall.RawSyscall(sys.sysTgkill, uintptr(os.Getpid()), uintptr(syscall.Gettid()), 0)
	_, _, errPidfdSig := syscall.RawSyscall6(sys.sysPidfdSendSignal, 0, 0, 0, 0, 0, 0)
	res.KillParent = errnoStr(errKill)
	res.TkillParent = errnoStr(errTkill)
	res.TgkillParent = errnoStr(errTgkillParent)
	res.TgkillSelf = errnoStr(errTgkillSelf)
	res.PidfdSendSignal = errnoStr(errPidfdSig)

	ifd, errInotify := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if ifd >= 0 {
		syscall.InotifyAddWatch(ifd, outsidePrivateDir, syscall.IN_ALL_EVENTS)
		syscall.Close(ifd)
	}
	res.InotifyErr = errStr(errInotify)

	mfd, _, errMemfd := syscall.Syscall(sys.sysMemfdCreate, uintptr(unsafe.Pointer(&[]byte("m\x00")[0])), 1, 0)
	if errMemfd == 0 {
		syscall.Close(int(mfd))
	}
	_, _, errExecveat := syscall.Syscall6(sys.sysExecveat, ^uintptr(0), 0, 0, 0, 0, 0)
	res.MemfdErr = errnoStr(errMemfd)
	res.ExecveatErr = errnoStr(errExecveat)

	hiddenFile := filepath.Join(outsidePrivateDir, "hidden-holdout.txt")
	_, errStatHidden := os.Stat(hiddenFile)
	fOpen, errOpenHidden := os.Open(hiddenFile)
	if fOpen != nil {
		fOpen.Close()
	}
	_, errReaddirHidden := os.ReadDir(outsidePrivateDir)
	res.StatHiddenErr = errStr(errStatHidden)
	res.OpenHiddenErr = errStr(errOpenHidden)
	res.ReaddirHiddenErr = errStr(errReaddirHidden)

	selfExe, _ := os.Executable()
	res.ForkSelfErr = errStr(exec.Command(selfExe, "noop").Run())

	var buf [6]byte
	nFD6, errReadFD6 := syscall.Read(6, buf[:])
	res.ReadFD6Err = errStr(errReadFD6)
	res.ReadFD6Data = string(buf[:max(0, nFD6)])

	_, _, errIoUring := syscall.Syscall(sys.sysIoUringSetup, 1, 0, 0)
	res.IoUringErr = errnoStr(errIoUring)

	res.UnshareErr = errStr(syscall.Unshare(syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS))
	res.SetpgidErr = errStr(syscall.Setpgid(0, 0))
	_, errSetsid := syscall.Setsid()
	res.SetsidErr = errStr(errSetsid)

	_, _, errPrctlAllowed := syscall.RawSyscall6(sys.sysPrctl, prGetNoNewPrivs, 0, 0, 0, 0, 0)
	_, _, errPrctlRefused := syscall.RawSyscall6(sys.sysPrctl, prSetDumpable, 1, 0, 0, 0, 0)
	res.PrctlAllowed = errnoStr(errPrctlAllowed)
	res.PrctlRefused = errnoStr(errPrctlRefused)
	return res
}
