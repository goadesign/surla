package designeval

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/testmodule"
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

// TestGenerationDiagnosticPreservesSnapshotValidationDetail exercises the
// validator's exact safe provenance before commands or temporary files run.
func TestGenerationDiagnosticPreservesSnapshotValidationDetail(t *testing.T) {
	for _, test := range []struct {
		name, mod, want     string
		omitSum, omitDesign bool
	}{
		{name: "missing checksum file", omitSum: true, want: "run `go mod tidy`"},
		{name: "malformed module", mod: "module (\n", want: "parse snapshot go.mod"},
		{name: "missing design", omitDesign: true, want: "design snapshot has no design/*.go file"},
		{name: "local replacement", mod: "module example.com/design\ngo 1.24\nreplace example.com/local => ./local\n", want: "only versioned replacements are supported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := validSnapshotStub()
			if test.mod != "" {
				files["go.mod"] = test.mod
			}
			if test.omitSum {
				delete(files, "go.sum")
			}
			if test.omitDesign {
				delete(files, "design/design.go")
			}
			ev := NewEvaluatorInDir(t.TempDir())
			t.Cleanup(func() { assert.NoError(t, ev.Close()) })
			_, err := ev.Generate(t.Context(), files)
			require.Error(t, err)
			var validation *snapshotValidationError
			require.ErrorAs(t, err, &validation)
			assert.Equal(t, validation.err.Error(), err.Error(), "raw CLI/trace text is unchanged")
			assert.Contains(t, GenerationDiagnostic(err), test.want)
		})
	}
}

// TestGenerationUsesPhysicalOperatorPaths observes the exact facts supplied to
// a real sandboxed Go build, despite relative and symlinked owner settings.
func TestGenerationUsesPhysicalOperatorPaths(t *testing.T) {
	files := testmodule.Snapshot(t, "", minimalDesign)
	toolchain, err := resolveToolchainEnv(t.Context())
	require.NoError(t, err)
	modCache, err := filepath.EvalSymlinks(toolchain.modCache)
	require.NoError(t, err)
	root := t.TempDir()
	t.Chdir(root)
	require.NoError(t, os.Mkdir("physical\"compiler\\storage", 0o700))
	require.NoError(t, os.Symlink("physical\"compiler\\storage", "compiler-link"))
	require.NoError(t, os.Symlink(toolchain.goroot, "toolchain-link"))
	require.NoError(t, os.Symlink(modCache, "modules-link"))
	t.Setenv("GOROOT", filepath.Join(root, "toolchain-link"))
	t.Setenv("GOMODCACHE", filepath.Join(root, "modules-link"))
	ev := NewEvaluatorInDir(filepath.Join("compiler-link", "work"))
	t.Cleanup(func() { assert.NoError(t, ev.Close()) })
	work, err := ev.admit()
	require.NoError(t, err)
	ev.finish(nil)
	require.NoError(t, os.WriteFile(filepath.Join(work, "gocache"), []byte("a file prevents cache setup\n"), 0o600))
	_, err = ev.Generate(t.Context(), files)
	require.ErrorContains(t, err, "failed to initialize build cache")
	var output *sandboxOutputError
	require.ErrorAs(t, err, &output)
	assert.Equal(t, toolchain.goroot, output.paths.goroot)
	assert.Equal(t, modCache, output.paths.modCache)
	assert.Equal(t, work, output.paths.work)
	assert.Equal(t, filepath.Dir(work), filepath.Dir(output.paths.temporary))
	assert.Equal(t, filepath.Join(output.paths.temporary, "build", "target"), output.paths.source)
	diagnostic := GenerationDiagnostic(err)
	assert.NotContains(t, diagnostic, work)
	assert.NotContains(t, diagnostic, filepath.Base(filepath.Dir(filepath.Dir(work))))
	quoted := strconv.Quote(work)
	assert.NotContains(t, diagnostic, quoted[1:len(quoted)-1])
	assert.Contains(t, diagnostic, "<compiler>/gocache")
}

// TestToolchainSettingsLeaveMissingModuleCacheForGo preserves first-run Go
// behavior: reading settings does not create a module cache before loading deps.
func TestToolchainSettingsLeaveMissingModuleCacheForGo(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "not-created-yet")
	t.Setenv("GOMODCACHE", cache)
	settings, err := resolveToolchainEnv(t.Context())
	require.NoError(t, err)
	assert.Equal(t, cache, settings.modCache)
	assert.NoDirExists(t, cache)
}
