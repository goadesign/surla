// These checks exercise tool cards through the actual compiler catalog and the
// same sandboxed evaluator that serves architecture previews.
package designeval

import (
	"encoding/json"
	"testing"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/testmodule"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestToolCardsPreserveCompiledInputChoices verifies that compilation shows all
// declared choices without guessing which binder the application will install.
func TestToolCardsPreserveCompiledInputChoices(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	files := testmodule.Snapshot(t, "", `package design
import (
 . "goa.design/goa-ai/dsl"
 . "goa.design/goa/v3/dsl"
)
var scope = Type("Scope", func() { Attribute("label", String); Required("label") })
var records = Toolset("records", func() {
 for _, name := range []string{"read_orders", "readOrders"} {
  Tool(name, "Read selected records", func() {
   Args(func() {
    Attribute("scope", scope); Attribute("orderID", String); Attribute("render", Boolean)
    Required("scope", "orderID")
   })
   Return(func() { Attribute("items", ArrayOf(String)); Required("items") })
   UIOnly("render")
   BoundedResult()
   if name == "readOrders" { RequiresUI() }
  })
 }
})
var _ = Service("reader", func() {
 Agent("bound", "Supplies a scope", func() {
  Use(records, func() { BindField("scope") })
 })
 Agent("lookup", "Resolves a scope key", func() {
  Use(records, func() { BindField("scope", func() { Lookup(String, "Scope key", func() { MinLength(2) }) }) })
 })
})`)
	result, err := testEvaluator.Evaluate(t.Context(), files)
	require.NoError(t, err)
	require.Len(t, result.Tools, 4)
	seen := make(map[string]bool)
	for _, card := range result.Tools {
		assert.False(t, seen[card.ID])
		seen[card.ID] = true
		assert.Equal(t, "reader.records", card.Toolset)
		assert.Contains(t, []string{"read_orders", "readOrders"}, card.Name)
		assert.Empty(t, card.InjectedFields)
		require.NotNil(t, card.InputBinding)
		assert.Equal(t, []string{"scope"}, card.InputBinding.InjectedFields)
		assert.True(t, card.BoundedResult)
		var ordinary, provider, response map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(card.InputBinding.ArgsSchema), &ordinary))
		require.NoError(t, json.Unmarshal([]byte(card.ArgsSchema), &provider))
		require.NoError(t, json.Unmarshal([]byte(card.ReturnSchema), &response))
		assert.NotContains(t, string(ordinary["properties"]), `"scope"`)
		assert.Contains(t, string(ordinary["properties"]), `"order_id"`)
		assert.NotContains(t, string(ordinary["properties"]), `"orderID"`)
		assert.Contains(t, string(provider["properties"]), `"scope"`)
		assert.Contains(t, string(response["required"]), `"returned"`)
		assert.Contains(t, string(response["required"]), `"truncated"`)
		if card.ID == "reader.lookup/records."+card.Name {
			require.NotNil(t, card.InputBinding.LookupArgsSchema)
			assert.Contains(t, *card.InputBinding.LookupArgsSchema, `"minLength": 2`)
		} else {
			assert.Equal(t, "reader.bound/records."+card.Name, card.ID)
			assert.Nil(t, card.InputBinding.LookupArgsSchema)
		}
		if card.Name == "readOrders" {
			assert.True(t, card.RequiresUI)
			assert.Nil(t, card.TextOnlyArgsSchema)
			assert.Nil(t, card.InputBinding.TextOnlyArgsSchema)
			assert.Nil(t, card.InputBinding.LookupTextOnlyArgsSchema)
		} else {
			assert.False(t, card.RequiresUI)
			require.NotNil(t, card.TextOnlyArgsSchema)
			assert.NotContains(t, *card.TextOnlyArgsSchema, `"render"`)
			assert.Contains(t, *card.TextOnlyArgsSchema, `"scope"`)
			require.NotNil(t, card.InputBinding.TextOnlyArgsSchema)
			assert.NotContains(t, *card.InputBinding.TextOnlyArgsSchema, `"scope"`)
			assert.NotContains(t, *card.InputBinding.TextOnlyArgsSchema, `"render"`)
		}
		encoded, err := gensurla.EncodeToolContractItem(card)
		require.NoError(t, err)
		assert.NotEmpty(t, encoded)
	}
}
