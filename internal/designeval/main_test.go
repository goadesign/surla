package designeval

import (
	"fmt"
	"os"
	"testing"
)

// testEvaluator is shared by this package's tests, as one owner's evaluator
// is shared by its evaluations, so later tests reuse compiled dependencies.
// TestMain closes it and fails the run if its workspace cannot be removed.
var testEvaluator *Evaluator

// TestMain runs the package's tests with testEvaluator and then closes it.
func TestMain(m *testing.M) {
	testEvaluator = NewEvaluator()
	code := m.Run()
	if err := testEvaluator.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "close test evaluator:", err)
		code = max(code, 1)
	}
	os.Exit(code)
}
