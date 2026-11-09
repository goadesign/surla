// Package designeval checks that Model relationships retain their resolved
// identities when architecture previews and structural diffs consume them.
package designeval

import (
	"context"
	"fmt"
	"testing"

	gensurlaclient "github.com/goadesign/surla/gen/http/surla/client"
	"github.com/goadesign/surla/internal/testmodule"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestModelRelationshipsUseResolvedScopedIdentities exercises the real evaluator
// with same-named containers and an outgoing component relationship.
func TestModelRelationshipsUseResolvedScopedIdentities(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	var previous Result
	for _, destination := range []string{"First/API", "Second/API"} {
		t.Run(destination, func(t *testing.T) {
			source := fmt.Sprintf(`package design

import m "goa.design/model/dsl"

var _ = m.Design("Architecture", "Scoped relationships", func() {
	m.SoftwareSystem("First", "First system", func() {
		m.Container("API", "First API", "Go", func() {
			m.Component("Handler", "Request handler", "Go", func() {
				m.Uses("Second/API", "Calls API", "gRPC")
			})
		})
	})
	m.SoftwareSystem("Second", "Second system", func() {
		m.Container("API", "Second API", "Go")
	})
	m.Person("Operator", "Operator", func() {
		m.Uses(%q, "Calls API", "HTTPS")
	})
})
`, destination)
			result, err := testEvaluator.Evaluate(context.Background(), testmodule.Snapshot(t, "", source))
			require.NoError(t, err)
			require.Len(t, result.Containers, 6)
			require.Len(t, result.Edges, 2)
			identities := make(map[string]bool, len(result.Containers))
			apiCount := 0
			componentID := ""
			for _, node := range result.Containers {
				require.NoError(t, gensurlaclient.ValidateC4ContainerItemResponseBody(&gensurlaclient.C4ContainerItemResponseBody{
					ID:          &node.ID,
					Name:        &node.Name,
					Kind:        &node.Kind,
					Description: &node.Description,
				}))
				assert.NotEmpty(t, node.ID)
				assert.False(t, identities[node.ID], "duplicate node identity")
				identities[node.ID] = true
				if node.Name == "API" {
					apiCount++
				}
				if node.Kind == "component" {
					componentID = node.ID
				}
			}
			assert.Equal(t, 2, apiCount)
			require.NotEmpty(t, componentID)
			componentHasEdge := false
			for _, edge := range result.Edges {
				assert.True(t, identities[edge.FromID], "source %q missing", edge.FromID)
				assert.True(t, identities[edge.ToID], "destination %q missing", edge.ToID)
				componentHasEdge = componentHasEdge || edge.FromID == componentID
			}
			assert.True(t, componentHasEdge)
			if previous.Declarations != nil {
				assert.NotEqual(t, previous.Declarations["Model:Person:Operator"], result.Declarations["Model:Person:Operator"])
			}
			previous = result
		})
	}
}
