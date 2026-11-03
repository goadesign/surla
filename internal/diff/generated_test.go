// These tests compare compiler output for transport changes that leave domain
// input and result fields unchanged.
package diff

import (
	"strings"
	"testing"

	"github.com/goadesign/surla/internal/testmodule"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompareGeneratedTransportChanges verifies that the actual generators,
// rather than a selected list of expression fields, decide which sources change.
func TestCompareGeneratedTransportChanges(t *testing.T) {
	cases := []struct {
		// name identifies the transport contract under test.
		name string
		// design is the complete old design package.
		design string
		// old and replacement change only the selected transport declaration.
		old, replacement string
		// generated is the source file that must change.
		generated string
	}{
		{
			name:   "grpc_result_stream",
			design: grpcResultDesign,
			old:    "  Result(", replacement: "  StreamingResult(",
			generated: "Generated:gen/grpc/orders/pb/goagen_design_orders.proto",
		},
		{
			name: "sse_data_field",
			design: `package design
import . "goa.design/goa/v3/dsl"
var _ = Service("orders", func() {
 Method("events", func() {
  StreamingResult(func() { Attribute("first", String); Attribute("second", String) })
  HTTP(func() { GET("/events"); ServerSentEvents("first") })
 })
})`,
			old: `ServerSentEvents("first")`, replacement: `ServerSentEvents("second")`,
			generated: "Generated:gen/http/orders/server/sse.go",
		},
		{
			name:   "mcp_resource_reader",
			design: resourceReaderDesign,
			old:    `if name == "first"`, replacement: `if name == "second"`,
			generated: "Generated:gen/mcp_records/adapter_server.go",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := testmodule.Snapshot(t, "", tc.design)
			updated := testmodule.Replace(t, old, "design/design.go", tc.old, tc.replacement)
			report, err := Compare(t.Context(), testEvaluator, old, updated)
			require.NoError(t, err)
			assert.True(t, report.HasChanges)
			assert.Contains(t, report.ModifiedDeclarations, tc.generated)
			assert.NotContains(t, strings.ToLower(report.Summary), "compatible")
		})
	}
}

// TestCompareIdenticalCompiledSources has no source normalization or ignored
// files: independent evaluator processes must return the same declarations.
func TestCompareIdenticalCompiledSources(t *testing.T) {
	files := testmodule.Snapshot(t, "", resourceReaderDesign)
	report, err := Compare(t.Context(), testEvaluator, files, files)
	require.NoError(t, err)
	assert.False(t, report.HasChanges)
	assert.Empty(t, report.AddedDeclarations)
	assert.Empty(t, report.RemovedDeclarations)
	assert.Empty(t, report.ModifiedDeclarations)
}

// TestCompareTargetModulePathChangesGeneratedOutput proves that generated names
// and imports come from the submitted module, never the evaluator's own module.
func TestCompareTargetModulePathChangesGeneratedOutput(t *testing.T) {
	old := testmodule.Snapshot(t, "", grpcResultDesign)
	updated := testmodule.Replace(t, old, "go.mod", "module example.com/design", "module example.com/renamed")
	report, err := Compare(t.Context(), testEvaluator, old, updated)
	require.NoError(t, err)
	assert.True(t, report.HasChanges)
	assert.Contains(t, report.RemovedDeclarations, "Generated:gen/grpc/orders/pb/goagen_design_orders.proto")
	assert.Contains(t, report.AddedDeclarations, "Generated:gen/grpc/orders/pb/goagen_renamed_orders.proto")
}

// grpcResultDesign keeps domain fields fixed while transport or module identity changes.
const grpcResultDesign = `package design
import . "goa.design/goa/v3/dsl"
var _ = Service("orders", func() {
 Method("read", func() {
  Result(func() { Field(1, "value", String) })
  GRPC(func() { Response(CodeOK) })
 })
})`

// resourceReaderDesign gives both methods identical input and output contracts;
// only ResourceReader decides which method receives resources/read requests.
const resourceReaderDesign = `package design
import (
 . "goa.design/goa-ai/dsl"
 . "goa.design/goa/v3/dsl"
)
var uri = Type("ResourceURI", String, func() { Format(FormatURI) })
var text = Type("ResourceText", func() {
 Field(1, "uri", uri); Field(2, "text", String); Required("uri", "text")
})
var blob = Type("ResourceBlob", func() {
 Field(1, "uri", uri); Field(2, "blob", Bytes); Required("uri", "blob")
})
var item = Type("ResourceItem", func() {
 OneOf("content", "Text or binary contents", func() {
  Attribute("text", text); Attribute("blob", blob)
 })
 Required("content")
})
var _ = Service("records", func() {
 MCP("records", "1")
 JSONRPC(func() { POST("/mcp") })
 for _, name := range []string{"first", "second"} {
  Method(name, func() {
   Payload(func() { Field(1, "uri", uri); Required("uri") })
   Result(func() { Field(1, "contents", ArrayOfRequired(item)) })
   if name == "first" { ResourceReader() }
  })
 }
})`
