package verifyrun

import "golang.org/x/sys/unix"

// parentStartingBlock reads the kernel's original argv/environment block,
// which late Setenv must not change. Tests export only family-name booleans.
func parentStartingBlock(pid int) ([]byte, error) {
	return unix.SysctlRaw("kern.procargs2", pid)
}
