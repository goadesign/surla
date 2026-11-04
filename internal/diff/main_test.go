package diff

import (
	"fmt"
	"os"
	"testing"

	"github.com/goadesign/surla/internal/designeval"
)

// testEvaluator is shared by this package's tests, so later comparisons
// reuse compiled dependencies. TestMain closes it and fails the run if its
// workspace cannot be removed.
var testEvaluator *designeval.Evaluator

// TestMain runs the package's tests with testEvaluator and then closes it.
func TestMain(m *testing.M) {
	testEvaluator = designeval.NewEvaluator()
	code := m.Run()
	if err := testEvaluator.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "close test evaluator:", err)
		code = max(code, 1)
	}
	os.Exit(code)
}
