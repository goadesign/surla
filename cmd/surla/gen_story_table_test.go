package main

import (
	"bytes"
	"io"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/testmodule"
)

// storySuiteDesign is a test module's design with one service and one
// evaluation suite, to which a Story table adds its scenarios.
const storySuiteDesign = `package design

import (
	. "goa.design/goa-ai/eval/dsl"
	. "goa.design/goa/v3/dsl"
)

var _ = Service("orders", func() {
	Method("get_order", func() {
		Payload(String)
		Result(String)
		HTTP(func() { GET("/orders/{id}") })
	})
})

var _ = Suite("orders_acceptance", func() {
	Description("Acceptance suite.")
	Timeout("30s")
	Scenario("gets_order", func() {
		Description("Gets an order.")
	})
})
`

// TestGenKeepsTheStoryTablesTests generates a test module whose design holds
// a Story table with one finished flow, as a build leaves it after the user
// merges its tests, then runs surla gen on it again: nothing changes, and the
// suite still requires the flow's hook.
func TestGenKeepsTheStoryTablesTests(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	stories := []designeval.StorySummary{{
		Name:  "Refunds",
		Flows: []designeval.StoryFlow{{Key: "refund-over-limit", Title: "A refund over the limit.", Complete: true}},
	}}
	table, _, err := designeval.StoryTests(fstest.MapFS{"design/design.go": {Data: []byte(storySuiteDesign)}}, stories)
	require.NoError(t, err)
	dir := writeModule(t, testmodule.Module(t, "example.com/orders/eval", "", map[string]string{
		"design/design.go":        storySuiteDesign,
		designeval.StoryTestsPath: string(table),
	}))
	const suite, hook = "gen/evals/orders_acceptance/suite.go", "RefundOverLimit(context.Context) (eval.Result, error)"

	require.NoError(t, runGen([]string{"-dir", dir}, io.Discard, io.Discard))
	assert.Contains(t, files(t, dir, "gen")[suite], hook, "the table's flow is a scenario of the suite")
	var again bytes.Buffer
	require.NoError(t, runGen([]string{"-dir", dir}, &again, io.Discard))
	assert.Contains(t, again.String(), "added: 0\nchanged: 0\nremoved: 0\n", "regenerating keeps the required tests")
	assert.Contains(t, files(t, dir, "gen")[suite], hook)
	assertNothingLeft(t, dir, tmp)
}
