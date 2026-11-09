// This file separates caller-safe design diagnostics from operator failures.
package designeval

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
)

type (
	// diagnosticPaths identifies only directories this evaluator allocated. Caller
	// diagnostics replace these exact prefixes; raw errors preserve them.
	diagnosticPaths struct {
		// source is the staged copy of the caller's module.
		source string
		// temporary holds this operation's build, logs, and generated output.
		temporary string
		// work is the evaluator-owned compiler workspace.
		work string
		// goroot is the exact compiler toolchain directory.
		goroot string
		// modCache is the operator's read-only module cache.
		modCache string
	}
	// snapshotError marks an error produced exclusively from the caller's
	// supplied files, before operator files or processes are accessed.
	snapshotError struct{ error }
	// moduleCommandError preserves the go command's raw failure and its output
	// once. Only a completed, noncanceled command with readable output creates it.
	moduleCommandError struct {
		// err is the command failure without appended output.
		err error
		// output is the existing diagnostic tail, including its leading newline.
		output string
	}
)

// SourceDiagnostic returns a recovery message for a rejected snapshot or a
// compiler/module command that completed with diagnostics. HTTP and MCP callers
// use it to explain which local source or dependency to fix, while retaining
// err unchanged for traces and CLI output. Exact evaluator-owned directory
// prefixes are replaced with source-relative or descriptive paths.
//
// Setup, log-read, cancellation, process-cleanup, and filesystem failures return
// false. A join with multiple independent causes also returns false, even if
// one cause is a source diagnostic: it may include an operator cleanup failure.
func SourceDiagnostic(err error) (string, bool) {
	for err != nil {
		switch e := err.(type) {
		case *snapshotError:
			return "The local module cannot be generated: " + e.Error() + ". Correct the module files and call generate_code again.", true
		case *buildError:
			if e.output != "" {
				return "The design did not compile:\n" + e.paths.forCaller(e.output) +
					"\nCorrect the design or its module dependencies and call generate_code again." +
					" If the compiler reports that generator.CompleteSources is undefined, update the module's goa.design/goa/v3 requirement or versioned replacement to " +
					completeSourcesGoa + " or later, run go mod tidy, and call generate_code again.", true
			}
			return "", false
		case *moduleCommandError:
			// Module commands use the operator's Go proxy and HOME settings.
			// Their output can contain credentials or private configuration,
			// so none of that output crosses the HTTP or MCP edge.
			return "The design's module dependencies could not be loaded. Check the design's imports, run go mod tidy in the selected local module, and call generate_code again. If loading still fails, ask the server operator to inspect the trace for dependency-access details.", true
		case interface{ Unwrap() []error }:
			causes := e.Unwrap()
			if len(causes) != 1 {
				return "", false
			}
			err = causes[0]
		default:
			err = errors.Unwrap(err)
		}
	}
	return "", false
}

// Error reproduces the existing Go command error and diagnostic tail.
func (e *moduleCommandError) Error() string { return e.err.Error() + e.output }

// Unwrap preserves the command's exit status for existing evaluator callers.
func (e *moduleCommandError) Unwrap() error { return e.err }

// Unwrap preserves the original snapshot validation error for Go callers.
func (e *snapshotError) Unwrap() error { return e.error }

// diagnosticExit requires a completed Go command with the ordinary error exit
// code. Cancellation, sandbox-launch errors, and setup failures have different
// causes or exit codes and must not be described as source rejections.
func diagnosticExit(ctx context.Context, err error) bool {
	var exit *exec.ExitError
	return ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1
}

// forCaller maps only known evaluator-owned prefixes. It never changes the
// underlying error or guesses which arbitrary paths belong to the operator.
func (p diagnosticPaths) forCaller(output string) string {
	for _, mapping := range []struct{ from, to string }{
		{p.source, ""}, {p.temporary, "<generation>/"}, {p.work, "<compiler>/"},
		{p.goroot, "<toolchain>/"}, {p.modCache, "<modules>/"},
	} {
		if mapping.from != "" {
			from := filepath.Clean(mapping.from)
			output = strings.ReplaceAll(output, from+string(filepath.Separator), mapping.to)
			// A diagnostic may name the directory itself, rather than a
			// child file. That exact allocated path is private as well.
			bare := strings.TrimSuffix(mapping.to, "/")
			if bare == "" {
				bare = "."
			}
			output = strings.ReplaceAll(output, from, bare)
		}
	}
	return output
}
