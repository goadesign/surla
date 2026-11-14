//go:build darwin

package candidate

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// sandboxExec is the macOS sandbox launcher. It applies the profile and then
// executes the candidate in place, so the candidate keeps the started PID.
const sandboxExec = "/usr/bin/sandbox-exec"

// checkSandbox reports whether this macOS host has /usr/bin/sandbox-exec.
func checkSandbox() error {
	if _, err := os.Stat(sandboxExec); err != nil {
		return fmt.Errorf("%w: %v", ErrIsolationUnavailable, err)
	}
	return nil
}

// ensureGOROOTCanary is a no-op on macOS, where Seatbelt profiles are path-based.
func ensureGOROOTCanary(string) error {
	return nil
}

// startProcess starts binary inside the macOS Seatbelt sandbox described by spec.
func startProcess(
	ctx context.Context,
	binary string,
	spec Spec,
	logFile *os.File,
	stdout io.Writer,
) (*Process, error) {
	profile := Profile(binary, spec.StateDir, spec.ConnectPorts, spec.ListenPort)
	cmd := exec.CommandContext(ctx, sandboxExec, append([]string{"-p", profile, binary}, spec.Args...)...)
	cmd.Env = append([]string{"HOME=" + spec.StateDir, "TMPDIR=" + spec.StateDir}, spec.Env...)
	cmd.Dir = spec.StateDir
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if stdout != nil {
		cmd.Stdout = stdout
	}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start candidate %s: %w", binary, err)
	}
	p := &Process{cmd: cmd, logFile: spec.LogFile, done: make(chan struct{})}
	go func() {
		p.waitErr = cmd.Wait()
		_ = logFile.Close()
		close(p.done)
	}()
	return p, nil
}

// runGoBuild runs `go <args> -o <outPath> <Package>` inside the macOS Seatbelt
// build sandbox.
func (b resolvedBuild) runGoBuild(
	ctx context.Context,
	run func(*exec.Cmd) error,
	logFile *os.File,
	outPath string,
	args ...string,
) error {
	spec := b.spec
	goBin := filepath.Join(spec.GOROOT, "bin", "go")
	profile := BuildProfile(spec.SourceDir, spec.GOROOT, spec.ModCache, spec.WorkDir, b.outDir)
	cmdArgs := append([]string{"-p", profile, goBin}, args...)
	cmd := exec.CommandContext(ctx, sandboxExec, append(cmdArgs, "-o", outPath, spec.Package)...)
	cmd.Dir = spec.ModuleDir
	cmd.Env = b.buildEnv()
	cmd.Stdout, cmd.Stderr = logFile, logFile
	return run(cmd)
}
