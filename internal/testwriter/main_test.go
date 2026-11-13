package testwriter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/goadesign/surla/internal/designeval"
)

// testEvaluator and testChecker are shared by this package's tests, as one
// server's are shared by its checks, so later checks reuse compiled
// dependencies. TestMain closes the evaluator and removes their folder.
var (
	testEvaluator *designeval.Evaluator
	testChecker   *Checker
)

// TestMain runs the package's tests with testEvaluator and testChecker
// working in one temporary folder, then closes and removes them.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "surla-testwriter-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create the test folder:", err)
		os.Exit(1)
	}
	testEvaluator = designeval.NewEvaluatorInDir(filepath.Join(dir, "evaluator"))
	testChecker = NewChecker(testEvaluator, filepath.Join(dir, "checks"))
	code := m.Run()
	if err := errors.Join(testEvaluator.Close(), os.RemoveAll(dir)); err != nil {
		fmt.Fprintln(os.Stderr, "clean up the test folder:", err)
		code = max(code, 1)
	}
	os.Exit(code)
}
