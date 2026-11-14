// This file names the environment a test program starts with. A program the
// operator wrote keeps its caller's environment. A program agents on this
// machine wrote gets only the names below, each with its reason: it runs
// outside the sandbox, so what it is not given it cannot read or send.
package verify

import (
	"fmt"
	"strings"

	"github.com/goadesign/surla/internal/modelsettings"
	"github.com/goadesign/surla/verify/suite"
)

const (
	// TestsEvaluator marks a test program the operator wrote: the evaluator
	// stored with `surla verification set`, or the one named on `surla
	// verify`'s command line. It keeps the environment its caller has.
	TestsEvaluator TestsSource = "evaluator"
	// TestsWritten marks a test program agents on this machine wrote: the
	// tests accepted for a change, or the builder's own test module that a
	// live copy runs. It gets only the names ProgramEnv and LiveProgramEnv
	// list.
	TestsWritten TestsSource = "written_tests"

	// pathEnv is the search path for programs, so the test program finds
	// helpers it starts, such as a workflow development server. Its value is
	// the caller's own.
	pathEnv = "PATH"
	// tmpEnv is a temporary folder inside the run's own folder, so nothing
	// the program writes lands in a folder another run or the server uses.
	tmpEnv = "TMPDIR"
	// LiveModelProviderEnv and LiveModelNameEnv name the direct model
	// provider and model the user chose for live copies; a live copy's test
	// program reads them to plan with that model.
	LiveModelProviderEnv = "SURLA_LIVE_MODEL_PROVIDER"
	LiveModelNameEnv     = "SURLA_LIVE_MODEL_NAME"
	// LiveModelGatewayEnv names the model gateway the user chose for live
	// copies instead of a direct provider.
	LiveModelGatewayEnv = "SURLA_LIVE_MODEL_GATEWAY"
)

// ProgramEnv returns the environment p's test program starts with in a
// verification run, from environ, the caller's environment, which holds the
// provider request settings the run was given. The operator's evaluator
// (TestsEvaluator) gets environ unchanged. A program agents wrote
// (TestsWritten) gets only PATH from environ and TMPDIR set to tmpDir, a
// folder inside the run's own folder: no provider setting, because Surla's
// own test layout plans with a scripted model in verification.
func ProgramEnv(p Plan, environ []string, tmpDir string) []string {
	if p.Tests == TestsEvaluator {
		return environ
	}
	env := pick(environ, pathEnv)
	return append(env, tmpEnv+"="+tmpDir)
}

// LiveProgramEnv returns the environment p's test program starts with in a
// live copy, from environ, the live copy's executor's environment, which
// already holds the live copy's TMPDIR, its key, the model connection the
// user chose and that provider's handed-over request settings. The
// operator's evaluator (TestsEvaluator) gets environ unchanged. A program
// agents wrote (TestsWritten) gets only PATH, TMPDIR, the live copy's key
// (suite.LiveKeyEnv, which it hands to the candidate), the model connection
// (LiveModelProviderEnv and LiveModelNameEnv, or LiveModelGatewayEnv) and
// the chosen provider's request settings (modelsettings), no other
// provider's.
func LiveProgramEnv(p Plan, environ []string) []string {
	if p.Tests == TestsEvaluator {
		return environ
	}
	names := []string{pathEnv, tmpEnv, suite.LiveKeyEnv, LiveModelProviderEnv, LiveModelNameEnv, LiveModelGatewayEnv}
	if name, ok := lookup(environ, LiveModelProviderEnv); ok {
		if provider, ok := modelsettings.Lookup(name); ok {
			names = append(names, provider.Settings()...)
		}
	}
	return pick(environ, names...)
}

// Check returns an error unless s is TestsEvaluator or TestsWritten, the only
// sources a plan may name: Prepare refuses any other, and so do the run and
// live-copy records verifyrun saves and reads.
func (s TestsSource) Check() error {
	if s != TestsEvaluator && s != TestsWritten {
		return fmt.Errorf("unknown test source %q", string(s))
	}
	return nil
}

// pick returns the entries of environ named by names, in environ's order.
func pick(environ []string, names ...string) []string {
	var env []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		for _, want := range names {
			if name == want {
				env = append(env, kv)
				break
			}
		}
	}
	return env
}

// lookup returns the value of the last entry of environ named name, as the
// program would see it.
func lookup(environ []string, name string) (string, bool) {
	var value string
	var found bool
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			value, found = v, true
		}
	}
	return value, found
}
