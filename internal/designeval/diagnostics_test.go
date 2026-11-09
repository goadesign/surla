package designeval

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestGenerationDiagnosticReportsFactsWithoutBlamingSource verifies phase and
// sandbox-output reporting without inferring a source cause from an exit code.
// Errors from module commands retain raw private output only for CLI/traces.
func TestGenerationDiagnosticReportsFactsWithoutBlamingSource(t *testing.T) {
	cause := errors.New("exit status 1")
	paths := diagnosticPaths{temporary: "/operator/run", work: "/operator/compiler"}
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{name: "compiler source error", err: &phaseError{phase: "compiling the design", err: &buildError{err: &sandboxOutputError{err: cause, output: "design/design.go:7:9: undefined: Missing", paths: paths}}}, want: "undefined: Missing"},
		{name: "compiler storage error", err: &phaseError{phase: "compiling the design", err: &buildError{err: &sandboxOutputError{err: cause, output: "failed to initialize build cache at /operator/compiler/gocache: not a directory", paths: paths}}}, want: "failed to initialize build cache at <compiler>/gocache"},
		{name: "DSL error", err: &phaseError{phase: "evaluating the design and generating files", err: &sandboxOutputError{err: cause, output: "service orders: method get_order: result type is invalid", paths: paths}}, want: "result type is invalid"},
		{name: "module command", err: &phaseError{phase: "loading the design module dependencies", err: fmt.Errorf("private proxy https://private:credential@private-proxy.example: %w", cause)}, want: "loading the design module dependencies"},
		{name: "file setup", err: &phaseError{phase: "preparing the temporary compiler files", err: &os.PathError{Op: "mkdir", Path: "/operator/run", Err: os.ErrPermission}}, want: "preparing the temporary compiler files"},
		{name: "before admission", err: errors.New("create private /operator/run failed"), want: "starting the design compiler"},
	} {
		t.Run(test.name, func(t *testing.T) {
			diagnostic := GenerationDiagnostic(test.err)
			assert.Contains(t, diagnostic, test.want)
			assert.NotContains(t, diagnostic, "private-proxy")
			assert.NotContains(t, diagnostic, "credential")
			assert.NotContains(t, diagnostic, "/operator/")
			assert.NotContains(t, diagnostic, "did not compile")
			assert.NotContains(t, diagnostic, "Correct the design")
		})
	}
	original := &sandboxOutputError{err: cause, output: "raw /operator/run/output", paths: paths}
	wrapped := &phaseError{phase: "compiling the design", err: errors.Join(original, os.ErrPermission)}
	assert.Equal(t, cause.Error()+" (raw /operator/run/output)", original.Error())
	assert.Equal(t, errors.Join(original, os.ErrPermission).Error(), wrapped.Error())
	assert.ErrorIs(t, wrapped, cause)
	assert.ErrorIs(t, wrapped, os.ErrPermission)
	assert.Contains(t, GenerationDiagnostic(wrapped), "raw <generation>/output")
}

// TestGenerationDiagnosticMapsExactEscapedPaths covers raw and Go %q spellings
// before less-specific parent paths can alter their escaped representation.
func TestGenerationDiagnosticMapsExactEscapedPaths(t *testing.T) {
	for _, special := range []string{"quote\"", "back\\slash", "tab\tline\nreturn\r", "control\x1bbyte\xff"} {
		t.Run(strconv.Quote(special), func(t *testing.T) {
			parent := "/operator/" + special
			paths := diagnosticPaths{
				source: parent + "/run/build/target", temporary: parent + "/run",
				work: parent + "/compiler", goroot: parent + "/toolchain", modCache: parent + "/modules",
			}
			var output strings.Builder
			for _, dir := range []string{paths.source, paths.temporary, paths.work, paths.goroot, paths.modCache} {
				fmt.Fprintf(&output, "%s\n%q\n%s/file.go\n%q\n", dir, dir, dir, dir+"/file.go")
			}
			raw := output.String()
			failure := &sandboxOutputError{err: errors.New("exit status 1"), output: raw, paths: paths}
			diagnostic := GenerationDiagnostic(failure)
			assert.NotContains(t, diagnostic, "/operator/")
			assert.NotContains(t, diagnostic, special)
			for _, label := range []string{"./file.go", "<generation>/file.go", "<compiler>/file.go", "<toolchain>/file.go", "<modules>/file.go"} {
				assert.Contains(t, diagnostic, label)
			}
			assert.Equal(t, "exit status 1 ("+raw+")", failure.Error(), "raw CLI/trace text is unchanged")
		})
	}
}
