package verifyrun

import (
	"fmt"
	"os"
)

// parentStartingBlock reads the kernel's original environment, which late
// Setenv must not change. Tests export only family-name booleans.
func parentStartingBlock(pid int) ([]byte, error) {
	return os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
}
