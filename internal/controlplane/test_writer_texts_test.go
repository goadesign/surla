// This file checks what the test writer's tool and assignment texts say
// about regenerating its test module.
package controlplane

import (
	"testing"

	"github.com/stretchr/testify/assert"

	gensurlatools "github.com/goadesign/surla/gen/surla/toolsets/surla"
)

// TestTestWriterTextsLetItRegenerate checks the get_test_writer_assignment
// tool's description and the assignment's description in its result schema:
// each says that Surla writes the Story table, that the test writer leaves it
// as it is, and that regenerating keeps the required tests; none forbids
// regenerating or says it drops a test.
func TestTestWriterTextsLetItRegenerate(t *testing.T) {
	t.Parallel()
	spec := gensurlatools.SpecGetTestWriterAssignment()
	for name, text := range map[string]string{
		"tool description":       spec.Description,
		"assignment description": string(spec.Result.Schema),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Contains(t, text, "Surla writes eval/design/story_tests.go, which declares the required tests")
			assert.Contains(t, text, "surla gen -dir eval in the working folder")
			assert.Contains(t, text, "regenerating keeps the required tests")
			assert.NotContains(t, text, "drop")
			assert.NotContains(t, text, "never regenerate")
		})
	}
}
