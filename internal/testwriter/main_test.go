package testwriter

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/designeval"
)

// testEvaluator and testChecker are shared by this package's tests, as one
// server's are shared by its checks, so later checks reuse compiled
// dependencies. TestMain closes the evaluator and removes their folder.
var (
	testEvaluator *designeval.Evaluator
	testChecker   *Checker
	// testDir is the folder TestMain creates for both and removes.
	testDir string
)

// helperFailEnv, when set, makes TestMainHelper run, and fail when it is
// "true"; TestMainRemovesItsFolder sets it for its child test process.
const helperFailEnv = "SURLA_TESTWRITER_HELPER_FAILS"

// TestMain runs the package's tests with testEvaluator and testChecker
// working in one temporary folder, then closes them and removes the folder
// before exiting, whether the tests passed or failed: os.Exit runs no deferred
// call, so the cleanup runs first. A run killed or stopped by a panic or the
// test timeout exits inside m.Run and leaves the folder, as it leaves any
// test's temporary folders.
func TestMain(m *testing.M) {
	var err error
	if testDir, err = os.MkdirTemp("", "surla-testwriter-*"); err != nil {
		fmt.Fprintln(os.Stderr, "create the test folder:", err)
		os.Exit(1)
	}
	testEvaluator = designeval.NewEvaluatorInDir(filepath.Join(testDir, "evaluator"))
	testChecker = NewChecker(testEvaluator, filepath.Join(testDir, "checks"))
	code := m.Run()
	if err := errors.Join(testEvaluator.Close(), os.RemoveAll(testDir)); err != nil {
		fmt.Fprintln(os.Stderr, "clean up the test folder:", err)
		code = max(code, 1)
	}
	os.Exit(code)
}

// TestMainRemovesItsFolder runs this package's test binary again, with a
// temporary folder of its own, once with a passing test and once with a
// failing one, and checks that TestMain removed its folder both times.
func TestMainRemovesItsFolder(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		t.Run("fails="+strconv.FormatBool(fail), func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestMainHelper$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), "TMPDIR="+tmp, helperFailEnv+"="+strconv.FormatBool(fail))
			out, err := cmd.CombinedOutput()
			if fail {
				require.Error(t, err, "%s", out)
			} else {
				require.NoError(t, err, "%s", out)
			}
			assert.Contains(t, string(out), "TestMain's folder exists: "+filepath.Join(tmp, "surla-testwriter-"),
				"the helper ran and TestMain's folder was in the child's temporary folder")
			left, err := filepath.Glob(filepath.Join(tmp, "surla-testwriter-*"))
			require.NoError(t, err)
			assert.Empty(t, left, "TestMain removed its folder")
		})
	}
}

// TestMainHelper runs only in TestMainRemovesItsFolder's child process. It
// checks that TestMain's folder exists in the child's temporary folder, then
// passes or fails as the parent asked.
func TestMainHelper(t *testing.T) {
	fail, ok := os.LookupEnv(helperFailEnv)
	if !ok {
		t.Skip("runs only in TestMainRemovesItsFolder's child process")
	}
	require.DirExists(t, testDir)
	t.Log("TestMain's folder exists:", testDir)
	if fail == "true" {
		t.Fatal("failing as the parent asked")
	}
}
