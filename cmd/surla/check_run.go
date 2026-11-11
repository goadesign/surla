// This file is the composition root of `surla check-run`, the hidden check
// helper role that surla serve starts for each check_project and
// update_dependencies operation (see controlplane.RunCheckHelper).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"time"

	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/verify/candidate"
)

// checkRunFlushTimeout bounds how long a check helper flushes its spans to a
// configured exporter after the operation, as surla serve does at shutdown.
const checkRunFlushTimeout = 5 * time.Second

// runCheckRun runs `surla check-run`: it builds the helper's Docker runner
// and its trace provider from the environment surla serve passed on, runs
// the operation surla serve sent on stdin, and flushes the helper's spans.
// It takes no arguments.
func runCheckRun(args []string) (err error) {
	if len(args) != 0 {
		return fmt.Errorf("check-run takes no arguments, got %q", args)
	}
	tracerProvider, err := newTraceProvider(context.Background(), os.Stderr, os.Getenv)
	if err != nil {
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), checkRunFlushTimeout)
		defer cancel()
		err = errors.Join(err, tracerProvider.Shutdown(flushCtx))
	}()
	return controlplane.RunCheckHelper(candidate.NewContainerRunner(), tracerProvider, os.Stdin, os.Stdout)
}

// checkHelperCommand returns the controlplane.CheckHelper that starts
// `<self> check-run`, where self is the argument list that runs this surla
// executable.
func checkHelperCommand(self []string) controlplane.CheckHelper {
	return func() *exec.Cmd {
		return exec.Command(self[0], append(slices.Clone(self[1:]), "check-run")...)
	}
}
