//go:build linux

package candidate

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"slices"
	"syscall"
)

type (
	// archSyscalls holds the architecture-specific syscall numbers, audit
	// architecture tag, and Seccomp-BPF allowlist sets for one Linux target
	// architecture (arm64 or amd64).
	archSyscalls struct {
		// goarch is the Go architecture name ("arm64" or "amd64").
		goarch string
		// auditArch is the Linux AUDIT_ARCH_* constant for this architecture.
		auditArch uint32
		// hasX32Bit is true on amd64, where syscalls >= 0x40000000 use the x32 ABI.
		hasX32Bit bool

		sysLandlockCreateRuleset int
		sysLandlockAddRule       int
		sysLandlockRestrictSelf  int
		sysSeccomp               int
		sysPidfdOpen             int
		sysPidfdGetfd            int
		sysPidfdSendSignal       int
		sysCloseRange            int
		sysProcessVMReadv        int
		sysIoUringSetup          int
		sysMemfdCreate           int
		sysExecveat              int
		sysKeyctl                int
		sysInotifyInit1          int
		sysInotifyAddWatch       int
		sysFchmodat              int
		sysUtimensat             int
		sysSetxattr              int
		sysPrlimit64             int
		sysSetpriority           int
		sysIoprioSet             int
		sysSchedSetaffinity      int
		sysSchedGetaffinity      int
		sysSchedSetattr          int
		sysSchedSetscheduler     int
		sysSchedSetparam         int
		sysFcntl                 int
		sysIoctl                 int
		sysPrctl                 int
		sysKill                  int
		sysTkill                 int
		sysTgkill                int
		sysSocket                int
		sysSocketpair            int
		sysBind                  int
		sysListen                int
		sysConnect               int
		sysSendto                int
		sysSendmsg               int
		sysSendmmsg              int
		sysClone                 int
		sysClone3                int
		sysFork                  int
		sysVfork                 int
		sysExecve                int
		sysUnshare               int
		sysSetpgid               int
		sysSetsid                int

		// silentFallbackENOSYS lists optional capability probes (clone3,
		// pidfd_open, pidfd_send_signal) that return SECCOMP_RET_ERRNO|ENOSYS
		// in-kernel without logging a supervisor refusal.
		silentFallbackENOSYS []uint32

		// inKernelDenyEPERM lists path-based metadata syscalls not mediated by
		// Landlock that return SECCOMP_RET_ERRNO|EPERM in-kernel.
		inKernelDenyEPERM []uint32

		// commonAllow lists syscalls allowed unconditionally across all modes.
		commonAllow []uint32
		// netPortAllow lists socket syscalls added only when Exec has ports.
		netPortAllow []uint32
		// buildExtraAllow lists process-wait and statfs syscalls added in Build.
		buildExtraAllow []uint32

		// names maps syscall numbers to human-readable names for refusal logs.
		names map[int]string
	}

	// sockFilter is one classic BPF instruction (struct sock_filter).
	sockFilter struct {
		Code uint16
		Jt   uint8
		Jf   uint8
		K    uint32
	}

	// sockFprog is the classic BPF program header passed to seccomp(2).
	sockFprog struct {
		Len    uint16
		Filter *sockFilter
	}

	// seccompData mirrors struct seccomp_data passed to BPF filters and
	// user-notification supervisors.
	seccompData struct {
		Nr                 int32
		Arch               uint32
		InstructionPointer uint64
		Args               [6]uint64
	}

	// seccompNotif mirrors struct seccomp_notif (80 bytes).
	seccompNotif struct {
		ID    uint64
		Pid   uint32
		Flags uint32
		Data  seccompData
	}

	// seccompNotifResp mirrors struct seccomp_notif_resp (24 bytes).
	seccompNotifResp struct {
		ID    uint64
		Val   int64
		Error int32
		Flags uint32
	}

	// bpfBuilder accumulates classic BPF instructions for a Seccomp filter.
	bpfBuilder struct {
		// insns holds the instructions emitted so far.
		insns []sockFilter
	}
)

const (
	// prctl option constants used by the launcher and Seccomp prctl filter.
	prSetPdeathsig  = 1
	prGetPdeathsig  = 2
	prSetDumpable   = 4
	prSetName       = 15
	prGetName       = 16
	prCapBsetRead   = 23
	prSetNoNewPrivs = 38
	prGetNoNewPrivs = 39
	prSetTHPDisable = 41
	prGetTHPDisable = 42
	prSetVMA        = 0x53564d41

	// Seccomp mode, flags, return actions, and user-notification ioctl numbers.
	seccompSetModeFilter         = 1
	seccompFilterFlagNewListener = 1 << 3
	seccompRetKillProcess        = 0x80000000
	seccompRetErrno              = 0x00050000
	seccompRetUserNotif          = 0x7fc00000
	seccompRetAllow              = 0x7fff0000

	// _IOWR('!', 0, struct seccomp_notif) = 0xc0502100
	// _IOWR('!', 1, struct seccomp_notif_resp) = 0xc0182101
	// _IOW('!', 2, __u64) = 0x40082102
	seccompIoctlNotifRecv    = 0xc0502100
	seccompIoctlNotifSend    = 0xc0182101
	seccompIoctlNotifIDValid = 0x40082102

	// Linux audit architecture identifiers for seccomp_data.arch.
	auditArchX86_64  = 0xc000003e
	auditArchAarch64 = 0xc00000b7

	// Classic BPF instruction class, mode, and operation bits.
	bpfLD   = 0x00
	bpfW    = 0x00
	bpfABS  = 0x20
	bpfJMP  = 0x05
	bpfJEQ  = 0x10
	bpfJGE  = 0x30
	bpfJSET = 0x40
	bpfK    = 0x00
	bpfRET  = 0x06
	bpfALU  = 0x04
	bpfAND  = 0x50

	// Socket and fcntl argument constants inspected by the BPF filter.
	msgFastopen = 0x20000000
	fSetown     = 8
	fSetsig     = 10
	fSetownEx   = 15
)

// currentArchSyscalls returns the syscall table for runtime.GOARCH.
func currentArchSyscalls() archSyscalls {
	return archSyscallsFor(runtime.GOARCH)
}

// archSyscallsFor returns the Linux syscall table and Seccomp allowlists for
// goarch ("arm64" or "amd64"). Both tables are compiled in on all architectures
// so the in-process BPF simulator verifies both on every test run.
func archSyscallsFor(goarch string) archSyscalls {
	switch goarch {
	case "arm64":
		return archSyscalls{
			goarch:                   "arm64",
			auditArch:                auditArchAarch64,
			hasX32Bit:                false,
			sysLandlockCreateRuleset: 444,
			sysLandlockAddRule:       445,
			sysLandlockRestrictSelf:  446,
			sysSeccomp:               277,
			sysPidfdOpen:             434,
			sysPidfdGetfd:            438,
			sysPidfdSendSignal:       424,
			sysCloseRange:            436,
			sysProcessVMReadv:        270,
			sysIoUringSetup:          425,
			sysMemfdCreate:           279,
			sysExecveat:              281,
			sysKeyctl:                219,
			sysInotifyInit1:          26,
			sysInotifyAddWatch:       27,
			sysFchmodat:              53,
			sysUtimensat:             88,
			sysSetxattr:              5,
			sysPrlimit64:             261,
			sysSetpriority:           140,
			sysIoprioSet:             30,
			sysSchedSetaffinity:      122,
			sysSchedGetaffinity:      123,
			sysSchedSetattr:          274,
			sysSchedSetscheduler:     119,
			sysSchedSetparam:         118,
			sysFcntl:                 25,
			sysIoctl:                 29,
			sysPrctl:                 167,
			sysKill:                  129,
			sysTkill:                 130,
			sysTgkill:                131,
			sysSocket:                198,
			sysSocketpair:            199,
			sysBind:                  200,
			sysListen:                201,
			sysConnect:               203,
			sysSendto:                206,
			sysSendmsg:               211,
			sysSendmmsg:              269,
			sysClone:                 220,
			sysClone3:                435,
			sysFork:                  -1,
			sysVfork:                 -1,
			sysExecve:                221,
			sysUnshare:               97,
			sysSetpgid:               154,
			sysSetsid:                157,
			silentFallbackENOSYS: []uint32{
				424, // pidfd_send_signal (probed by cmd/go and os/exec; falls back to kill/tgkill)
				434, // pidfd_open (probed by cmd/go and os/exec; falls back to PID/wait4)
				435, // clone3 (probed by runtime/os/exec; falls back to clone)
			},
			inKernelDenyEPERM: []uint32{
				5,   // setxattr
				6,   // lsetxattr
				7,   // fsetxattr
				14,  // removexattr
				15,  // lremovexattr
				16,  // fremovexattr
				53,  // fchmodat
				54,  // fchownat
				452, // fchmodat2
			},
			commonAllow: []uint32{
				// File and directory operations (confined by Landlock; ioctl is omitted).
				17,  // getcwd
				23,  // dup
				24,  // dup3
				32,  // flock
				34,  // mkdirat
				35,  // unlinkat
				36,  // symlinkat
				37,  // linkat
				38,  // renameat
				46,  // ftruncate
				47,  // fallocate
				48,  // faccessat
				49,  // chdir
				50,  // fchdir
				52,  // fchmod
				55,  // fchown
				56,  // openat
				57,  // close
				61,  // getdents64
				62,  // lseek
				63,  // read
				64,  // write
				65,  // readv
				66,  // writev
				67,  // pread64
				68,  // pwrite64
				73,  // ppoll
				78,  // readlinkat
				79,  // newfstatat
				80,  // fstat
				82,  // fsync
				83,  // fdatasync
				276, // renameat2
				285, // copy_file_range
				291, // statx
				436, // close_range
				437, // openat2
				439, // faccessat2
				// Event loop and pipes (socketpair is argument-filtered to AF_UNIX+SOCK_STREAM).
				19, // eventfd2
				20, // epoll_create1
				21, // epoll_ctl
				22, // epoll_pwait
				59, // pipe2
				// Memory management.
				214, // brk
				215, // munmap
				222, // mmap
				226, // mprotect
				233, // madvise
				// Threads, futex, clocks, signals, and exit.
				93,  // exit
				94,  // exit_group
				96,  // set_tid_address
				98,  // futex
				99,  // set_robust_list
				100, // get_robust_list
				101, // nanosleep
				113, // clock_gettime
				114, // clock_getres
				115, // clock_nanosleep
				124, // sched_yield
				128, // restart_syscall
				132, // sigaltstack
				134, // rt_sigaction
				135, // rt_sigprocmask
				139, // rt_sigreturn
				160, // uname
				166, // umask
				172, // getpid
				173, // getppid
				174, // getuid
				175, // geteuid
				176, // getgid
				177, // getegid
				178, // gettid
				179, // sysinfo
				221, // execve (Landlock restricts to pinned binary / GOROOT)
				278, // getrandom
				293, // rseq
				441, // epoll_pwait2
			},
			netPortAllow: []uint32{
				// listen is routed to SECCOMP_RET_USER_NOTIF; sendto/sendmsg/sendmmsg are omitted.
				202, // accept
				204, // getsockname
				205, // getpeername
				207, // recvfrom
				208, // setsockopt
				209, // getsockopt
				210, // shutdown
				212, // recvmsg
				242, // accept4
			},
			buildExtraAllow: []uint32{
				43,  // statfs
				44,  // fstatfs
				95,  // waitid
				260, // wait4
			},
			names: map[int]string{
				5: "setxattr", 25: "fcntl", 26: "inotify_init1", 27: "inotify_add_watch",
				29: "ioctl", 30: "ioprio_set", 53: "fchmodat", 88: "utimensat",
				97: "unshare", 118: "sched_setparam", 119: "sched_setscheduler",
				122: "sched_setaffinity", 123: "sched_getaffinity", 129: "kill",
				130: "tkill", 131: "tgkill", 140: "setpriority", 154: "setpgid",
				157: "setsid", 167: "prctl", 198: "socket", 199: "socketpair",
				200: "bind", 201: "listen", 203: "connect", 206: "sendto",
				211: "sendmsg", 219: "keyctl", 261: "prlimit64", 269: "sendmmsg",
				270: "process_vm_readv", 274: "sched_setattr", 279: "memfd_create",
				281: "execveat", 424: "pidfd_send_signal", 425: "io_uring_setup",
				434: "pidfd_open", 435: "clone3", 438: "pidfd_getfd",
			},
		}
	case "amd64":
		return archSyscalls{
			goarch:                   "amd64",
			auditArch:                auditArchX86_64,
			hasX32Bit:                true,
			sysLandlockCreateRuleset: 444,
			sysLandlockAddRule:       445,
			sysLandlockRestrictSelf:  446,
			sysSeccomp:               317,
			sysPidfdOpen:             434,
			sysPidfdGetfd:            438,
			sysPidfdSendSignal:       424,
			sysCloseRange:            436,
			sysProcessVMReadv:        310,
			sysIoUringSetup:          425,
			sysMemfdCreate:           319,
			sysExecveat:              322,
			sysKeyctl:                250,
			sysInotifyInit1:          294,
			sysInotifyAddWatch:       254,
			sysFchmodat:              268,
			sysUtimensat:             280,
			sysSetxattr:              188,
			sysPrlimit64:             302,
			sysSetpriority:           141,
			sysIoprioSet:             251,
			sysSchedSetaffinity:      203,
			sysSchedGetaffinity:      204,
			sysSchedSetattr:          314,
			sysSchedSetscheduler:     144,
			sysSchedSetparam:         142,
			sysFcntl:                 72,
			sysIoctl:                 16,
			sysPrctl:                 157,
			sysKill:                  62,
			sysTkill:                 200,
			sysTgkill:                234,
			sysSocket:                41,
			sysSocketpair:            53,
			sysBind:                  49,
			sysListen:                50,
			sysConnect:               42,
			sysSendto:                44,
			sysSendmsg:               46,
			sysSendmmsg:              307,
			sysClone:                 56,
			sysClone3:                435,
			sysFork:                  57,
			sysVfork:                 58,
			sysExecve:                59,
			sysUnshare:               272,
			sysSetpgid:               109,
			sysSetsid:                112,
			silentFallbackENOSYS: []uint32{
				424, // pidfd_send_signal
				434, // pidfd_open
				435, // clone3
			},
			inKernelDenyEPERM: []uint32{
				90,  // chmod
				92,  // chown
				94,  // lchown
				188, // setxattr
				189, // lsetxattr
				190, // fsetxattr
				197, // removexattr
				198, // lremovexattr
				199, // fremovexattr
				260, // fchownat
				268, // fchmodat
				452, // fchmodat2
			},
			commonAllow: []uint32{
				// File and directory operations (confined by Landlock; ioctl=16 is omitted).
				0,   // read
				1,   // write
				2,   // open
				3,   // close
				4,   // stat
				5,   // fstat
				6,   // lstat
				7,   // poll
				8,   // lseek
				17,  // pread64
				18,  // pwrite64
				19,  // readv
				20,  // writev
				21,  // access
				32,  // dup
				33,  // dup2
				59,  // execve
				73,  // flock
				74,  // fsync
				75,  // fdatasync
				77,  // ftruncate
				79,  // getcwd
				80,  // chdir
				81,  // fchdir
				82,  // rename
				83,  // mkdir
				84,  // rmdir
				85,  // creat
				86,  // link
				87,  // unlink
				88,  // symlink
				89,  // readlink
				91,  // fchmod
				93,  // fchown
				217, // getdents64
				257, // openat
				258, // mkdirat
				262, // newfstatat
				263, // unlinkat
				264, // renameat
				265, // linkat
				266, // symlinkat
				267, // readlinkat
				269, // faccessat
				271, // ppoll
				285, // fallocate
				292, // dup3
				316, // renameat2
				326, // copy_file_range
				332, // statx
				436, // close_range
				437, // openat2
				439, // faccessat2
				// Event loop and pipes (socketpair=53 is argument-filtered to AF_UNIX+SOCK_STREAM).
				22,  // pipe
				213, // epoll_create
				232, // epoll_wait
				233, // epoll_ctl
				281, // epoll_pwait
				290, // eventfd2
				291, // epoll_create1
				293, // pipe2
				// Memory management.
				9,  // mmap
				10, // mprotect
				11, // munmap
				12, // brk
				28, // madvise
				// Threads, futex, clocks, signals, and exit.
				13,  // rt_sigaction
				14,  // rt_sigprocmask
				15,  // rt_sigreturn
				24,  // sched_yield
				35,  // nanosleep
				39,  // getpid
				60,  // exit
				63,  // uname
				95,  // umask
				97,  // getrlimit
				99,  // sysinfo
				102, // getuid
				104, // getgid
				107, // geteuid
				108, // getegid
				110, // getppid
				131, // sigaltstack
				158, // arch_prctl
				186, // gettid
				202, // futex
				218, // set_tid_address
				219, // restart_syscall
				228, // clock_gettime
				229, // clock_getres
				230, // clock_nanosleep
				231, // exit_group
				273, // set_robust_list
				274, // get_robust_list
				318, // getrandom
				334, // rseq
				441, // epoll_pwait2
			},
			netPortAllow: []uint32{
				// listen=50 is routed to SECCOMP_RET_USER_NOTIF; sendto/sendmsg/sendmmsg are omitted.
				43,  // accept
				45,  // recvfrom
				47,  // recvmsg
				48,  // shutdown
				51,  // getsockname
				52,  // getpeername
				54,  // setsockopt
				55,  // getsockopt
				288, // accept4
			},
			buildExtraAllow: []uint32{
				57,  // fork
				58,  // vfork
				61,  // wait4
				137, // statfs
				138, // fstatfs
				247, // waitid
			},
			names: map[int]string{
				16: "ioctl", 41: "socket", 42: "connect", 44: "sendto", 46: "sendmsg",
				49: "bind", 50: "listen", 53: "socketpair", 62: "kill", 72: "fcntl",
				109: "setpgid", 112: "setsid", 141: "setpriority", 142: "sched_setparam",
				144: "sched_setscheduler", 157: "prctl", 188: "setxattr", 200: "tkill",
				203: "sched_setaffinity", 204: "sched_getaffinity", 234: "tgkill",
				250: "keyctl", 251: "ioprio_set", 254: "inotify_add_watch", 268: "fchmodat",
				272: "unshare", 280: "utimensat", 294: "inotify_init1", 302: "prlimit64",
				307: "sendmmsg", 310: "process_vm_readv", 314: "sched_setattr",
				319: "memfd_create", 322: "execveat", 424: "pidfd_send_signal",
				425: "io_uring_setup", 434: "pidfd_open", 435: "clone3", 438: "pidfd_getfd",
			},
		}
	default:
		panic("unsupported arch: " + goarch)
	}
}

// syscallName returns the human-readable name for nr or "syscall_<nr>".
func (s archSyscalls) syscallName(nr int) string {
	if name, ok := s.names[nr]; ok {
		return name
	}
	return fmt.Sprintf("syscall_%d", nr)
}

// emit appends one classic BPF instruction to b.
func (b *bpfBuilder) emit(code uint16, jt, jf uint8, k uint32) {
	b.insns = append(b.insns, sockFilter{Code: code, Jt: jt, Jf: jf, K: k})
}

// buildPostHandoffDenySendmsgFilter builds Filter 2 (installed immediately
// after the launcher sends listenerFD over FD 5) which denies SYS_SENDMSG and
// SYS_SECCOMP with EPERM and returns SECCOMP_RET_ALLOW for all other syscalls
// so Filter 1's rules and SECCOMP_RET_USER_NOTIF notifications remain active.
func buildPostHandoffDenySendmsgFilter(sys archSyscalls) []sockFilter {
	var b bpfBuilder
	errnoEPERM := uint32(seccompRetErrno | uint32(syscall.EPERM))
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 0)
	b.emit(bpfJMP|bpfJEQ|bpfK, 1, 0, uint32(sys.sysSendmsg))
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 1, uint32(sys.sysSeccomp))
	b.emit(bpfRET|bpfK, 0, 0, errnoEPERM)
	b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
	return b.insns
}

// buildSeccompAllowlist builds the default-deny Seccomp-BPF allowlist program:
//   - Known harmless fallback probes (clone3, pidfd_open, pidfd_send_signal)
//     return SECCOMP_RET_ERRNO | ENOSYS in-kernel without logging a refusal;
//   - Path-based metadata syscalls not hooked by Landlock (fchmodat/fchownat/*xattr)
//     return SECCOMP_RET_ERRNO | EPERM in-kernel;
//   - Every unlisted syscall (and any unlisted prctl option) returns
//     SECCOMP_RET_USER_NOTIF so the supervisor records
//     "surla jail refused <name> (<nr>)" and returns ENOSYS.
func buildSeccompAllowlist(sys archSyscalls, cfg jailConfig, selfPID uint32) []sockFilter {
	var b bpfBuilder
	errnoENOSYS := uint32(seccompRetErrno | uint32(syscall.ENOSYS))
	errnoEPERM := uint32(seccompRetErrno | uint32(syscall.EPERM))

	// 1. Validate seccomp_data.arch (offset 4); kill process on mismatch.
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 4)
	b.emit(bpfJMP|bpfJEQ|bpfK, 1, 0, sys.auditArch)
	b.emit(bpfRET|bpfK, 0, 0, seccompRetKillProcess)

	// 2. Load seccomp_data.nr (offset 0). On amd64, kill process if x32 bit (>= 0x40000000) is set.
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 0)
	if sys.hasX32Bit {
		b.emit(bpfJMP|bpfJGE|bpfK, 0, 1, 0x40000000)
		b.emit(bpfRET|bpfK, 0, 0, seccompRetKillProcess)
	}

	// 2b. Silent fallback probes (clone3, pidfd_open, pidfd_send_signal) -> SECCOMP_RET_ERRNO | ENOSYS.
	for _, nr := range sys.silentFallbackENOSYS {
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 1, nr)
		b.emit(bpfRET|bpfK, 0, 0, errnoENOSYS)
	}

	// 2c. Path-based metadata syscalls not hooked by Landlock -> SECCOMP_RET_ERRNO | EPERM in-kernel.
	for _, nr := range sys.inKernelDenyEPERM {
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 1, nr)
		b.emit(bpfRET|bpfK, 0, 0, errnoEPERM)
	}

	// 3. Unconditionally allowed syscalls for this entry point.
	allowSet := slices.Clone(sys.commonAllow)
	if cfg.Mode == modeExecWithPorts && (cfg.ListenPort > 0 || len(cfg.ConnectPorts) > 0) {
		allowSet = append(allowSet, sys.netPortAllow...)
	}
	if cfg.Mode == modeBuild {
		allowSet = append(allowSet, sys.buildExtraAllow...)
	}
	for _, nr := range allowSet {
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 1, nr)
		b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
	}

	// 4. Argument-filtered syscalls (A holds seccomp_data.nr on entry to each block):

	// 4a-0. seccomp(op, flags, args): allowed in Filter 1 ONLY when op == SECCOMP_SET_MODE_FILTER (1)
	// and flags == 0 for the launcher's one-shot installation of Filter 2; Filter 2 then denies seccomp.
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 9, uint32(sys.sysSeccomp))
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 16)
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 5, seccompSetModeFilter)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 24)
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 3, 0)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 28)
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 1, 0)
	b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
	b.emit(bpfRET|bpfK, 0, 0, errnoEPERM)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 0)

	// 4a. sendmsg: allowed in Filter 1 ONLY on FD 5 (args[0] == 5) without MSG_FASTOPEN for the
	// launcher's one-shot listenerFD handoff; Filter 2 then denies sendmsg completely before execve.
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 9, uint32(sys.sysSendmsg))
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 16)
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 5, 5)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 20)
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 3, 0)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 32)
	b.emit(bpfJMP|bpfJSET|bpfK, 1, 0, msgFastopen)
	b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
	b.emit(bpfRET|bpfK, 0, 0, errnoEPERM)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 0)

	// 4b. socketpair(domain, type, protocol): allow ONLY when domain (args[0]) == AF_UNIX (1)
	// and (type (args[1]) & ~(SOCK_NONBLOCK|SOCK_CLOEXEC)) == SOCK_STREAM (1).
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 8, uint32(sys.sysSocketpair))
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 16)
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 4, uint32(syscall.AF_UNIX))
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 24)
	b.emit(bpfALU|bpfAND|bpfK, 0, 0, ^uint32(syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC))
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 1, uint32(syscall.SOCK_STREAM))
	b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
	b.emit(bpfRET|bpfK, 0, 0, errnoEPERM)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 0)

	// 4c. prctl(option, ...): allow only the options used by the Go runtime and launcher;
	// any other option falls through to SECCOMP_RET_USER_NOTIF ("surla jail refused prctl" -> ENOSYS).
	allowedPrctl := []uint32{
		prSetPdeathsig,
		prGetPdeathsig,
		prSetName,
		prGetName,
		prCapBsetRead,
		prSetNoNewPrivs,
		prGetNoNewPrivs,
		prSetTHPDisable,
		prGetTHPDisable,
		prSetVMA,
	}
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, uint8(len(allowedPrctl)+3), uint32(sys.sysPrctl))
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 16)
	for i, opt := range allowedPrctl {
		rem := uint8(len(allowedPrctl) - i)
		b.emit(bpfJMP|bpfJEQ|bpfK, rem, 0, opt)
	}
	b.emit(bpfRET|bpfK, 0, 0, seccompRetUserNotif)
	b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 0)

	// 4d. fcntl:
	// - F_SETOWN_EX (15) on args[1] (offset 24) -> always EPERM (takes pointer).
	// - F_SETOWN (8) or F_SETSIG (10) on args[1] (offset 24) -> allow ONLY when arg (args[2], offsets 32 and 36) == 0.
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 12, uint32(sys.sysFcntl))
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 24)
	b.emit(bpfJMP|bpfJEQ|bpfK, 8, 0, fSetownEx)
	b.emit(bpfJMP|bpfJEQ|bpfK, 2, 0, fSetown)
	b.emit(bpfJMP|bpfJEQ|bpfK, 1, 0, fSetsig)
	b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 32)
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 3, 0)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 28+8)
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 1, 0)
	b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
	b.emit(bpfRET|bpfK, 0, 0, errnoEPERM)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 0)

	// 4e. utimensat: allow ONLY when pathname (args[1], offsets 24 and 28) == NULL (0) (fd-based futimens).
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 7, uint32(sys.sysUtimensat))
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 24)
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 3, 0)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 28)
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 1, 0)
	b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
	b.emit(bpfRET|bpfK, 0, 0, errnoEPERM)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 0)

	// 4f. prlimit64, sched_getaffinity, sched_setaffinity, sched_setattr, sched_setscheduler, sched_setparam:
	// allow only when pid (args[0], offsets 16 and 20) == 0 (self).
	for _, sysNr := range []int{
		sys.sysPrlimit64,
		sys.sysSchedGetaffinity,
		sys.sysSchedSetaffinity,
		sys.sysSchedSetattr,
		sys.sysSchedSetscheduler,
		sys.sysSchedSetparam,
	} {
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 7, uint32(sysNr))
		b.emit(bpfLD|bpfW|bpfABS, 0, 0, 16)
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 3, 0)
		b.emit(bpfLD|bpfW|bpfABS, 0, 0, 20)
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 1, 0)
		b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
		b.emit(bpfRET|bpfK, 0, 0, errnoEPERM)
		b.emit(bpfLD|bpfW|bpfABS, 0, 0, 0)
	}

	// 4g. setpriority(which, who, prio): allow only when which (args[0]==0) and who (args[1]==0) (self).
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 9, uint32(sys.sysSetpriority))
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 16)
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 5, 0)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 24)
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 3, 0)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 28)
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 1, 0)
	b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
	b.emit(bpfRET|bpfK, 0, 0, errnoEPERM)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 0)

	// 4h. ioprio_set(which, who, ioprio): allow only when which (args[0]==1 IOPRIO_WHO_PROCESS) and who (args[1]==0).
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 9, uint32(sys.sysIoprioSet))
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 16)
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 5, 1)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 24)
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 3, 0)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 28)
	b.emit(bpfJMP|bpfJEQ|bpfK, 0, 1, 0)
	b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
	b.emit(bpfRET|bpfK, 0, 0, errnoEPERM)
	b.emit(bpfLD|bpfW|bpfABS, 0, 0, 0)

	// 4i. tgkill:
	// - In Exec/Output: allow when args[0] == selfPID; else EPERM.
	// - In Build with Landlock scopes (ABI >= 6 && !NoScope): LANDLOCK_SCOPE_SIGNAL confines signals in-kernel, so allow.
	// - In Build without Landlock scopes (NoScope / ABI 4-5): if args[0] != selfPID, send to SECCOMP_RET_USER_NOTIF supervisor.
	if cfg.Mode == modeBuild && !cfg.NoScope {
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 1, uint32(sys.sysTgkill))
		b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
	} else if cfg.Mode == modeBuild && cfg.NoScope {
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 7, uint32(sys.sysTgkill))
		b.emit(bpfLD|bpfW|bpfABS, 0, 0, 16)
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 3, selfPID)
		b.emit(bpfLD|bpfW|bpfABS, 0, 0, 20)
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 1, 0)
		b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
		b.emit(bpfRET|bpfK, 0, 0, seccompRetUserNotif)
		b.emit(bpfLD|bpfW|bpfABS, 0, 0, 0)
	} else {
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 7, uint32(sys.sysTgkill))
		b.emit(bpfLD|bpfW|bpfABS, 0, 0, 16)
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 3, selfPID)
		b.emit(bpfLD|bpfW|bpfABS, 0, 0, 20)
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 1, 0)
		b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
		b.emit(bpfRET|bpfK, 0, 0, errnoEPERM)
		b.emit(bpfLD|bpfW|bpfABS, 0, 0, 0)
	}

	// 4j. clone:
	// - In Exec/Output: require CLONE_THREAD (0x00010000) and forbid CLONE_NEW* (0x7e020000).
	// - In Build: forbid CLONE_NEW* (0x7e020000).
	if cfg.Mode == modeBuild {
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 5, uint32(sys.sysClone))
		b.emit(bpfLD|bpfW|bpfABS, 0, 0, 16)
		b.emit(bpfJMP|bpfJSET|bpfK, 1, 0, 0x7e020000)
		b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
		b.emit(bpfRET|bpfK, 0, 0, errnoEPERM)
		b.emit(bpfLD|bpfW|bpfABS, 0, 0, 0)
	} else {
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 6, uint32(sys.sysClone))
		b.emit(bpfLD|bpfW|bpfABS, 0, 0, 16)
		b.emit(bpfJMP|bpfJSET|bpfK, 2, 0, 0x7e020000)
		b.emit(bpfJMP|bpfJSET|bpfK, 0, 1, uint32(syscall.CLONE_THREAD))
		b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
		b.emit(bpfRET|bpfK, 0, 0, errnoEPERM)
		b.emit(bpfLD|bpfW|bpfABS, 0, 0, 0)
	}

	// 4k. Network syscalls in ExecWithPorts:
	// - socket(domain, type, protocol): AF_INET/AF_INET6 + SOCK_STREAM + (0|IPPROTO_TCP) only; else EPERM.
	// - connect, bind, and listen: routed to SECCOMP_RET_USER_NOTIF supervisor.
	if cfg.Mode == modeExecWithPorts && (cfg.ListenPort > 0 || len(cfg.ConnectPorts) > 0) {
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 12, uint32(sys.sysSocket))
		b.emit(bpfLD|bpfW|bpfABS, 0, 0, 16)
		b.emit(bpfJMP|bpfJEQ|bpfK, 1, 0, uint32(syscall.AF_INET))
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 7, uint32(syscall.AF_INET6))
		b.emit(bpfLD|bpfW|bpfABS, 0, 0, 24)
		b.emit(bpfALU|bpfAND|bpfK, 0, 0, ^uint32(syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC))
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 4, uint32(syscall.SOCK_STREAM))
		b.emit(bpfLD|bpfW|bpfABS, 0, 0, 32)
		b.emit(bpfJMP|bpfJEQ|bpfK, 1, 0, 0)
		b.emit(bpfJMP|bpfJEQ|bpfK, 0, 1, uint32(syscall.IPPROTO_TCP))
		b.emit(bpfRET|bpfK, 0, 0, seccompRetAllow)
		b.emit(bpfRET|bpfK, 0, 0, errnoEPERM)
		b.emit(bpfLD|bpfW|bpfABS, 0, 0, 0)

		for _, supNr := range []int{sys.sysConnect, sys.sysBind, sys.sysListen} {
			b.emit(bpfJMP|bpfJEQ|bpfK, 0, 1, uint32(supNr))
			b.emit(bpfRET|bpfK, 0, 0, seccompRetUserNotif)
		}
	}

	// 5. Default action for every other syscall: SECCOMP_RET_USER_NOTIF.
	// The supervisor logs "surla jail refused <name> (<nr>)" and returns -ENOSYS.
	b.emit(bpfRET|bpfK, 0, 0, seccompRetUserNotif)
	return b.insns
}

// evalBPF deterministically executes a classic BPF program against seccompData
// in pure Go so both arm64 and amd64 programs can be verified instruction by
// instruction on any Linux host.
func evalBPF(prog []sockFilter, d seccompData) uint32 {
	var raw [64]byte
	binary.LittleEndian.PutUint32(raw[0:4], uint32(d.Nr))
	binary.LittleEndian.PutUint32(raw[4:8], d.Arch)
	binary.LittleEndian.PutUint64(raw[8:16], d.InstructionPointer)
	for i := 0; i < 6; i++ {
		binary.LittleEndian.PutUint64(raw[16+i*8:24+i*8], d.Args[i])
	}
	var a uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.Code {
		case bpfLD | bpfW | bpfABS:
			if int(in.K)+4 > len(raw) {
				return 0
			}
			a = binary.LittleEndian.Uint32(raw[in.K : in.K+4])
		case bpfALU | bpfAND | bpfK:
			a &= in.K
		case bpfJMP | bpfJEQ | bpfK:
			if a == in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case bpfJMP | bpfJGE | bpfK:
			if a >= in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case bpfJMP | bpfJSET | bpfK:
			if (a & in.K) != 0 {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case bpfRET | bpfK:
			return in.K
		default:
			panic(fmt.Sprintf("unsupported BPF opcode 0x%x at pc=%d", in.Code, pc))
		}
	}
	panic("BPF program did not return")
}
