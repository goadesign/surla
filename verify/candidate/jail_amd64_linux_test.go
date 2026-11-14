//go:build linux && amd64

package candidate

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLinuxJailAMD64ArchMismatchAndX32Kill verifies on a native x86_64 kernel
// that the Seccomp-BPF filter terminates a jailed process immediately with
// SECCOMP_RET_KILL_PROCESS (SIGSYS) when it invokes an IA-32 compatibility
// syscall via int 0x80 (AUDIT_ARCH_I386 != AUDIT_ARCH_X86_64) or sets the x32
// ABI bit (0x40000000 | SYS_getpid).
func TestLinuxJailAMD64ArchMismatchAndX32Kill(t *testing.T) {
	require.NoError(t, CheckSandbox())
	probeBin := buildLinuxProbe(t)

	for _, tc := range []struct {
		name string
		arg  string
	}{
		{name: "x32_bit_syscall_killed_with_sigsys", arg: "amd64-x32-syscall"},
		{name: "ia32_int80_arch_mismatch_killed_with_sigsys", arg: "amd64-ia32-int80"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			out, _, err := runTestJailed(t, jailConfig{
				Mode:     modeExecNoPorts,
				Binary:   probeBin,
				Args:     []string{tc.arg},
				Dir:      stateDir,
				Env:      []string{"PATH=/usr/bin:/bin", "HOME=" + stateDir, "TMPDIR=" + stateDir},
				StateDir: stateDir,
			}, nil)
			require.Error(t, err, "jailed %s must not survive", tc.arg)
			assert.NotContains(t, out, "survived")
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr)
			ws, ok := exitErr.Sys().(syscall.WaitStatus)
			require.True(t, ok)
			assert.True(t, ws.Signaled(), "expected signal termination, got status %v", ws)
			assert.Equal(t, syscall.SIGSYS, ws.Signal(), "expected SIGSYS from SECCOMP_RET_KILL_PROCESS")
		})
	}

	negOut, negErr := exec.Command(probeBin, "amd64-ia32-int80").CombinedOutput()
	t.Logf("amd64-ia32-int80 NEG_CONTROL(unjailed): err=%v out=%q", negErr, strings.TrimSpace(string(negOut)))
}
