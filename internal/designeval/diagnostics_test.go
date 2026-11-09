package designeval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSourceDiagnosticSeparatesSourceFromOperatorFailures verifies that the
// caller receives compiler facts and recovery without private paths, while the
// original error and CLI text keep their complete causes and raw diagnostics.
func TestSourceDiagnosticSeparatesSourceFromOperatorFailures(t *testing.T) {
	cause := errors.New("sandboxed build of .: exit status 1")
	paths := diagnosticPaths{
		source: "/operator/run/build/target", temporary: "/operator/run", work: "/operator/compiler",
		goroot: "/operator/toolchain", modCache: "/operator/modules",
	}
	output := "/operator/run/build/target/design/design.go:7:9: undefined: Missing\n" +
		"/operator/modules/example.com/lib@v1/lib.go:1:1: type error\n" +
		"/operator/toolchain/src/runtime/file.go /operator/compiler/cache /operator/run/logs/build.log\n" +
		`cwd="/operator/run/build/target" cache="/operator/compiler" toolchain="/operator/toolchain" modules="/operator/modules"`
	compiler := &buildError{err: cause, output: output, paths: paths}
	for _, test := range []struct {
		name           string
		err            error
		wantDiagnostic bool
		wantText       string
	}{
		{name: "compiler", err: compiler, wantDiagnostic: true, wantText: "design/design.go:7:9: undefined: Missing"},
		{name: "wrapped compiler", err: fmt.Errorf("generate: %w", errors.Join(compiler)), wantDiagnostic: true, wantText: "undefined: Missing"},
		{name: "snapshot", err: &snapshotError{errors.New("go.sum is missing")}, wantDiagnostic: true, wantText: "go.sum is missing"},
		{name: "module dependency stderr", err: &moduleCommandError{err: cause, output: "\nproxy https://private:credential@private-proxy.example failed"}, wantDiagnostic: true, wantText: "run go mod tidy"},
		{name: "operator setup", err: &os.PathError{Op: "open", Path: "/operator/run/build.log", Err: os.ErrPermission}},
		{name: "compiler log read failed", err: &buildError{err: cause}},
		{name: "canceled", err: context.Canceled},
		{name: "source and cleanup failure", err: errors.Join(compiler, errors.New("remove /operator/run: permission denied"))},
		{name: "stuck compiler", err: &stuckBuildError{err: cause}},
	} {
		t.Run(test.name, func(t *testing.T) {
			diagnostic, ok := SourceDiagnostic(test.err)
			assert.Equal(t, test.wantDiagnostic, ok)
			if ok {
				assert.Contains(t, diagnostic, test.wantText)
				assert.Contains(t, diagnostic, "call generate_code again")
				assert.NotContains(t, diagnostic, "/operator/")
				assert.NotContains(t, diagnostic, "private-proxy")
				assert.NotContains(t, diagnostic, "credential")
			} else {
				assert.Empty(t, diagnostic)
			}
		})
	}
	assert.Equal(t, cause.Error()+" ("+output+")", compiler.Error())
	assert.ErrorIs(t, compiler, cause)
}

// TestDiagnosticExitRequiresACompletedOrdinaryFailure proves that cancellation,
// sandbox startup failure, and log/setup failures never acquire source meaning.
func TestDiagnosticExitRequiresACompletedOrdinaryFailure(t *testing.T) {
	one := exec.Command("/bin/sh", "-c", "exit 1").Run()
	require.Error(t, one)
	seventyOne := exec.Command("/bin/sh", "-c", "exit 71").Run()
	require.Error(t, seventyOne)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, test := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{name: "ordinary failure", ctx: t.Context(), err: one, want: true},
		{name: "canceled command", ctx: canceled, err: one},
		{name: "sandbox launcher", ctx: t.Context(), err: seventyOne},
		{name: "log opening", ctx: t.Context(), err: os.ErrPermission},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, diagnosticExit(test.ctx, test.err))
		})
	}
}
