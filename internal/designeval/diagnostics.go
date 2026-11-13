// This file exposes failed generation phases and offline sandbox output with
// exact known path spellings replaced. It preserves the complete original error
// for CLI callers and operator traces.
package designeval

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type (
	// diagnosticPaths identifies the exact directories supplied to the build
	// and execution sandboxes. Only their known path spellings are replaced.
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
	// phaseError identifies the operation in progress when evaluation failed.
	// Its phase is one of the descriptions runSandboxed sets; modulePhases
	// lists those that work on the target module's own files.
	// It makes no claim about whether source, storage, or setup caused failure.
	phaseError struct {
		// phase is the owner-selected operation description, never raw output.
		phase string
		// err retains every original cause, including cleanup failures.
		err error
	}
	// snapshotValidationError contains only errors derived from caller-supplied
	// strings by validateSnapshot, before operator files or commands are used.
	// Its original detail is safe for the caller, including missing-file remedies.
	snapshotValidationError struct {
		// err is the unchanged validation failure and its original Go causes.
		err error
	}
	// sandboxOutputError retains output from an offline sandbox once. Module
	// commands outside that sandbox never construct it because their output
	// can include operator proxy credentials and private configuration.
	sandboxOutputError struct {
		// err is the operation failure without appended output.
		err error
		// output is the original sandbox output, without another formatted copy.
		output string
		// paths identifies the exact private directories used by this sandbox.
		paths diagnosticPaths
	}
)

// The phases of runSandboxed that check, resolve, compile or evaluate the
// target module's own files. GenerationDiagnostic names every phase in its
// message; FailedOnModule reports these four.
const (
	phaseCheckFiles  = "checking the local module files"
	phaseLoadModules = "loading the design module dependencies"
	phaseCompile     = "compiling the design"
	phaseEvaluateDSL = "evaluating the design and generating files"
)

// modulePhases lists the module phases FailedOnModule reports.
var modulePhases = []string{phaseCheckFiles, phaseLoadModules, phaseCompile, phaseEvaluateDSL}

// GenerationDiagnostic describes the phase where generation failed and any
// available sandbox output, with exact private directory names replaced. HTTP
// and MCP callers receive this message while CLI callers and traces keep err
// unchanged. A compiler error exit does not distinguish invalid source from
// cache, disk, or setup failures, so this function never makes that verdict.
// Module-loading stderr is always withheld because those commands use operator
// HOME and proxy settings outside the sandbox.
//
// Known-path substitution is diagnostic hygiene, not confidentiality for
// arbitrary output from an executed design. The OS sandbox governs access to
// files and credentials.
func GenerationDiagnostic(err error) string {
	phase := "starting the design compiler"
	var operation *phaseError
	if errors.As(err, &operation) {
		phase = operation.phase
	}
	message := "Code generation failed while " + phase + "."
	var validation *snapshotValidationError
	if errors.As(err, &validation) {
		message += "\n" + validation.Error()
	}
	var sandbox *sandboxOutputError
	if errors.As(err, &sandbox) && sandbox.output != "" {
		message += "\nSandbox output:\n" + sandbox.paths.forCaller(sandbox.output)
	}
	var build *buildError
	if errors.As(err, &build) {
		message += "\nIf the compiler reports that generator.CompleteSources is undefined, the module needs a goa.design/goa/v3 requirement or versioned replacement at " + completeSourcesGoa + " or later."
	}
	return message
}

// FailedOnModule reports whether err, from Evaluate, Generate or
// GenerateWithStories, ended in a phase that works on the target module's own
// files: checking them, loading the dependencies its go.mod and go.sum name,
// compiling its design, or evaluating it. Such a failure is first the
// module's author's to look at. A failure while preparing or removing the
// compiler's own files, while starting the sandboxed generator or reading its
// output, or because the evaluator was closed, is not. Like
// GenerationDiagnostic it makes no verdict that the module is wrong: a full
// disk while the design compiles still fails in a module phase.
func FailedOnModule(err error) bool {
	if validation := (*snapshotValidationError)(nil); errors.As(err, &validation) {
		return true
	}
	operation := (*phaseError)(nil)
	return errors.As(err, &operation) && slices.Contains(modulePhases, operation.phase)
}

// Error preserves the original CLI and trace error text without adding a phase.
func (e *phaseError) Error() string { return e.err.Error() }

// Unwrap preserves the original error causes for Go callers.
func (e *phaseError) Unwrap() error { return e.err }

// Error preserves the original validation error text for CLI and trace callers.
func (e *snapshotValidationError) Error() string { return e.err.Error() }

// Unwrap preserves the validator's original error causes for Go callers.
func (e *snapshotValidationError) Unwrap() error { return e.err }

// Error reproduces the existing parenthesized sandbox-output error text.
func (e *sandboxOutputError) Error() string { return fmt.Sprintf("%s (%s)", e.err, e.output) }

// Unwrap preserves the sandbox's actual failure without inferring its cause.
func (e *sandboxOutputError) Unwrap() error { return e.err }

// forCaller replaces only exact known directory names, both as raw text and as
// the interior of a Go %q string. Replacing the encoded spelling is necessary
// for filenames containing quotes, backslashes, control bytes, or invalid UTF-8.
// Raw errors and their output remain unchanged.
func (p diagnosticPaths) forCaller(output string) string {
	for _, mapping := range []struct{ from, to string }{
		{p.source, "."}, {p.temporary, "<generation>"}, {p.work, "<compiler>"},
		{p.goroot, "<toolchain>"}, {p.modCache, "<modules>"},
	} {
		if mapping.from != "" {
			from := filepath.Clean(mapping.from)
			quoted := strconv.Quote(from)
			for _, spelling := range []string{from, quoted[1 : len(quoted)-1]} {
				output = strings.ReplaceAll(output, spelling, mapping.to)
			}
		}
	}
	return output
}
