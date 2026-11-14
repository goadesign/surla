// This file names the environment a test program starts with. A program the
// operator wrote keeps its caller's environment. A program agents on this
// machine wrote gets only the names below, each with its reason: it runs
// outside the sandbox; these names prevent Surla from handing it settings.
// Its operating-system user can still read other same-user programs.
package verify

import (
	"fmt"
	"strings"

	"github.com/goadesign/surla/verify/suite"
)

const (
	// TestsEvaluator marks a test program the operator wrote: the evaluator
	// stored with `surla verification set`, or the one named on `surla
	// verify`'s command line. It keeps the environment its caller has.
	TestsEvaluator TestsSource = "evaluator"
	// TestsWritten marks a test program agents on this machine wrote: the
	// tests accepted for a change. It gets only the names ProgramEnv and LiveProgramEnv
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
	// provider and model the user chose. For written tests only the executor
	// reads them; an operator's evaluator may still plan with them.
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

// LiveProgramEnv returns the environment of a live target's test program.
// An operator's evaluator keeps environ unchanged. Accepted written tests get
// only their helpers' PATH, private TMPDIR, live key and the chosen gateway:
// Surla serves a direct model in the executor, never in the test program.
func LiveProgramEnv(p Plan, environ []string) []string {
	if p.Tests == TestsEvaluator {
		return environ
	}
	return pick(environ, pathEnv, tmpEnv, suite.LiveKeyEnv, LiveModelGatewayEnv)
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
