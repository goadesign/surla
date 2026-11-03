// Package diff tests structural evaluated Goa/Goa-AI design and go.mod
// directive comparison.
package diff

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/goadesign/surla/internal/testmodule"
)

func TestCompareReportsEvaluatedDeclarationsHelperChangesAndGoModDirectives(t *testing.T) {
	ctx := context.Background()
	oldDesign := `package design

import . "goa.design/goa/v3/dsl"

const maximumItems = 10

func orderPayload() {
	Attribute("limit", Int, func() {
		Maximum(maximumItems)
	})
}

var _ = Service("orders", func() {
	Method("get_order", func() {
		Payload(orderPayload)
	})
})
`
	newDesign := `package design

import (
	. "goa.design/goa-ai/dsl"
	. "goa.design/goa/v3/dsl"
)

const maximumItems = 20

func orderPayload() {
	Attribute("limit", Int, func() {
		Maximum(maximumItems)
	})
}

var RefundTools = Toolset("refund_tools", func() {
	Tool("issue_refund", "Issue a refund", func() {})
})

var _ = Service("orders", func() {
	Method("get_order", func() {
		Payload(orderPayload)
	})
	Method("approve_refund", func() {})
	Agent("refund_agent", "Handles order refunds", func() {
		Use(RefundTools)
		RunPolicy(func() {
			DefaultCaps(MaxToolCalls(6))
			TimeBudget("2m")
		})
	})
})
`
	oldMod := `module github.com/goadesign/surla/examples/orders

go 1.26.2

require (
	goa.design/goa/v3 v3.26.1
	goa.design/goa-ai v0.5.3
)

exclude (
	example.com/legacy v1.0.0
	example.com/legacy v1.1.0
)
`
	newMod := `module github.com/goadesign/surla/examples/orders

go 1.26.2

require (
	goa.design/goa/v3 v3.26.2
	goa.design/goa-ai v0.5.4
	goa.design/pulse v1.9.10
)

replace goa.design/goa-ai => ../goa-ai

exclude example.com/legacy v1.1.0
`

	rep, err := Compare(ctx, testEvaluator, testmodule.Snapshot(t, "", oldDesign), testmodule.Snapshot(t, "", newDesign))
	if err != nil {
		t.Fatalf("Compare returned unexpected error: %v", err)
	}
	if !rep.HasChanges {
		t.Fatal("expected HasChanges to be true")
	}
	for _, want := range []string{
		"Service:orders/Agent:refund_agent",
		"Generated:gen/orders/toolsets/refund_tools/specs.go",
		"Service:orders/Method:approve_refund",
	} {
		if !slices.Contains(rep.AddedDeclarations, want) {
			t.Fatalf("expected AddedDeclarations to contain %q, got %v", want, rep.AddedDeclarations)
		}
	}
	if !slices.Contains(rep.ModifiedDeclarations, "Service:orders/Method:get_order") {
		t.Fatalf("expected ModifiedDeclarations to contain Service:orders/Method:get_order when const maximumItems changed from 10 to 20, got %v", rep.ModifiedDeclarations)
	}
	// go.mod directive deltas are a text comparison of the two go.mod files.
	deltas, err := compareGoMod(oldMod, newMod)
	if err != nil {
		t.Fatalf("compareGoMod: %v", err)
	}
	wantDeltas := []string{
		"exclude example.com/legacy@v1.0.0: removed excluded",
		"goa.design/goa-ai: v0.5.3 -> v0.5.4",
		"goa.design/goa/v3: v3.26.1 -> v3.26.2",
		"goa.design/pulse: added v1.9.10",
		"replace goa.design/goa-ai: added ../goa-ai",
	}
	if strings.Join(deltas, ",") != strings.Join(wantDeltas, ",") {
		t.Fatalf("unexpected VersionDeltas: got %v, want %v", deltas, wantDeltas)
	}
	if strings.Contains(strings.ToLower(rep.Summary), "compatible") {
		t.Fatalf("summary must not emit a blanket compatibility claim: %q", rep.Summary)
	}
}

func TestCompareDetectsTransportMappingsProtobufTagsMCPAnnotationsBoundedResultsAndPerConsumerCards(t *testing.T) {
	ctx := context.Background()
	oldDesign := `package design

import (
	. "goa.design/goa-ai/dsl"
	. "goa.design/goa/v3/dsl"
)

var LookupArgs = Type("LookupArgs", func() {
	Field(1, "tenant_id", String)
	Field(2, "order_id", String)
	Field(3, "cursor", String)
	Required("tenant_id", "order_id")
})

var LookupResult = Type("LookupResult", func() {
	Field(1, "items", ArrayOf(String))
	Required("items")
})

var OrderTools = Toolset("order_tools", func() {
	Tool("list_orders", "List orders", func() {
		Args(LookupArgs)
		Return(LookupResult)
		BoundedResult(func() {
			ContinueWith("list_orders_page", "cursor")
			NextCursor("next_cursor")
		})
		Confirmation(func() {
			Title("A|B,C")
			PromptTemplate("D: {{ .order_id }}")
			DeniedResultTemplate("E")
		})
	})
	Tool("list_orders_page", "Continue listing orders", func() {
		Args(LookupArgs)
		Return(LookupResult)
		BoundedResult(func() {
			Cursor("cursor")
			NextCursor("next_cursor")
		})
		Confirmation(func() {
			Title("A|B,C")
			PromptTemplate("D: {{ .order_id }}")
			DeniedResultTemplate("E")
		})
	})
})

var _ = Service("orders", func() {
	JSONRPC(func() {
		POST("/mcp")
	})
	MCP("orders_mcp", "1.0.0")
	Method("get_order", func() {
		Payload(func() {
			Field(1, "id", String)
			Required("id")
		})
		Result(String)
		HTTP(func() {
			GET("/orders")
			Header("id:X-Order-ID")
			Response(StatusOK)
		})
		GRPC(func() {
			Response(CodeOK)
		})
		Tool("get_order", "Get order by ID", func() {
			ToolVisibility("model")
			ReadOnlyHint(true)
		})
	})
	Agent("tenant_agent", "Binds tenant_id", func() {
		Use(OrderTools, func() {
			BindField("tenant_id")
		})
		RunPolicy(func() {
			DefaultCaps(MaxToolCalls(4))
			TimeBudget("1m")
		})
	})
	Agent("order_agent", "Binds order_id", func() {
		Use(OrderTools, func() {
			BindInput("list_orders", "order_id")
		})
		RunPolicy(func() {
			DefaultCaps(MaxToolCalls(4))
			TimeBudget("1m")
		})
	})
})
`
	newDesign := `package design

import (
	. "goa.design/goa-ai/dsl"
	. "goa.design/goa/v3/dsl"
)

var LookupArgs = Type("LookupArgs", func() {
	Field(1, "tenant_id", String)
	Field(2, "order_id", String)
	Field(3, "cursor", String)
	Required("tenant_id", "order_id")
})

var LookupResult = Type("LookupResult", func() {
	Field(1, "items", ArrayOf(String))
	Required("items")
})

var OrderTools = Toolset("order_tools", func() {
	Tool("list_orders", "List orders", func() {
		Args(LookupArgs)
		Return(LookupResult)
		BoundedResult(func() {
			ContinueWith("list_orders_page", "cursor")
			NextCursor("next_cursor")
		})
		Confirmation(func() {
			Title("A")
			PromptTemplate("B,C|D: {{ .order_id }}")
			DeniedResultTemplate("E")
		})
	})
	Tool("list_orders_page", "Continue listing orders", func() {
		Args(LookupArgs)
		Return(LookupResult)
		BoundedResult(func() {
			Cursor("cursor")
			NextCursor("next_cursor")
		})
		Confirmation(func() {
			Title("A")
			PromptTemplate("B,C|D: {{ .order_id }}")
			DeniedResultTemplate("E")
		})
	})
})

var _ = Service("orders", func() {
	JSONRPC(func() {
		POST("/mcp")
	})
	MCP("orders_mcp", "1.0.0")
	Method("get_order", func() {
		Payload(func() {
			Field(2, "id", String)
			Required("id")
		})
		Result(String)
		HTTP(func() {
			GET("/orders")
			Param("id:order_id")
			Response(StatusOK)
		})
		GRPC(func() {
			Response(CodeOK)
		})
		Tool("get_order", "Get order by ID", func() {
			ToolVisibility("app")
			DestructiveHint(true)
		})
	})
	Agent("tenant_agent", "Binds tenant_id", func() {
		Use(OrderTools, func() {
			BindField("tenant_id")
		})
		RunPolicy(func() {
			DefaultCaps(MaxToolCalls(4))
			TimeBudget("1m")
		})
	})
	Agent("order_agent", "Binds order_id", func() {
		Use(OrderTools, func() {
			BindInput("list_orders", "order_id")
		})
		RunPolicy(func() {
			DefaultCaps(MaxToolCalls(4))
			TimeBudget("1m")
		})
	})
})
`
	rep, err := Compare(ctx, testEvaluator, testmodule.Snapshot(t, "", oldDesign), testmodule.Snapshot(t, "", newDesign))
	if err != nil {
		t.Fatalf("Compare returned unexpected error: %v", err)
	}
	for _, wantMod := range []string{
		"Generated:gen/grpc/orders/pb/goagen_design_orders.proto",
		"Generated:gen/mcp_orders/adapter_server.go",
		"Generated:gen/orders/toolsets/order_tools/specs.go",
	} {
		if !slices.Contains(rep.ModifiedDeclarations, wantMod) {
			t.Fatalf("expected ModifiedDeclarations to contain %q, got %v", wantMod, rep.ModifiedDeclarations)
		}
	}

	evaluated, err := testEvaluator.Evaluate(ctx, testmodule.Snapshot(t, "", newDesign))
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	var cardIDs []string
	for _, tc := range evaluated.Tools {
		cardIDs = append(cardIDs, tc.ID)
		if tc.ID == "orders.tenant_agent/order_tools.list_orders" {
			if strings.Contains(tc.ArgsSchema, `"cursor"`) {
				t.Fatalf("expected continuation cursor field to be hidden from initial tool args schema: %s", tc.ArgsSchema)
			}
			if !strings.Contains(tc.ReturnSchema, `"returned"`) || !strings.Contains(tc.ReturnSchema, `"truncated"`) {
				t.Fatalf("expected bounded-result fields in return schema: %s", tc.ReturnSchema)
			}
			if tc.InputBinding == nil || strings.Contains(tc.InputBinding.ArgsSchema, `"tenant_id"`) || !strings.Contains(tc.InputBinding.ArgsSchema, `"order_id"`) {
				t.Fatalf("expected tenant_agent card to hide tenant_id and keep order_id: %s", tc.ArgsSchema)
			}
		}
		if tc.ID == "orders.order_agent/order_tools.list_orders" {
			if tc.InputBinding == nil || strings.Contains(tc.InputBinding.ArgsSchema, `"order_id"`) || !strings.Contains(tc.InputBinding.ArgsSchema, `"tenant_id"`) {
				t.Fatalf("expected order_agent card to hide order_id and keep tenant_id: %s", tc.ArgsSchema)
			}
		}
	}
	for _, wantCard := range []string{
		"orders.tenant_agent/order_tools.list_orders",
		"orders.order_agent/order_tools.list_orders",
	} {
		if !slices.Contains(cardIDs, wantCard) {
			t.Fatalf("expected tool cards to include %q without overwriting, got %v", wantCard, cardIDs)
		}
	}
}

func TestCompareMultiFileDirectorySiblingPackagesModelPrimitivesLookupAndTotalRequired(t *testing.T) {
	ctx := context.Background()

	oldRoot := t.TempDir()
	newRoot := t.TempDir()

	writeModule := func(root string, maxLimit int, qtyType, authIn, promptMsg, lookupType string, methodReqTotal bool, sysDesc string) string {
		goMod := "module github.com/acme/orders\n\ngo 1.26.2\n"
		if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(goMod), 0o600); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		limitsDir := filepath.Join(root, "pkg", "limits")
		if err := os.MkdirAll(limitsDir, 0o750); err != nil {
			t.Fatalf("mkdir limits: %v", err)
		}
		limitsSrc := "package limits\n\nconst MaxItems = " + strconv.Itoa(maxLimit) + "\n"
		if err := os.WriteFile(filepath.Join(limitsDir, "limits.go"), []byte(limitsSrc), 0o600); err != nil {
			t.Fatalf("write limits.go: %v", err)
		}
		designDir := filepath.Join(root, "design")
		if err := os.MkdirAll(designDir, 0o750); err != nil {
			t.Fatalf("mkdir design: %v", err)
		}
		typesSrc := `package design

import (
	"github.com/acme/orders/pkg/limits"
	. "goa.design/goa/v3/dsl"
)

var APIKeyAuth = APIKeySecurity("api_key", func() {
	Description("API key auth")
})

var OrderQuery = Type("OrderQuery", func() {
	APIKeyField(1, "api_key", "key", String)
	Field(2, "tenant_id", Int)
	Field(3, "qty", ` + qtyType + `, func() {
		Maximum(limits.MaxItems)
	})
	Required("key", "tenant_id", "qty")
})

var MethodPageResult = Type("MethodPageResult", func() {
	Field(1, "items", ArrayOf(String))
	Field(2, "returned", Int)
	Field(3, "total", Int)
	Field(4, "truncated", Boolean)
	Field(5, "refinement_hint", String)
` + func() string {
			if methodReqTotal {
				return "\tRequired(\"items\", \"returned\", \"total\", \"truncated\")\n"
			}
			return "\tRequired(\"items\", \"returned\", \"truncated\")\n"
		}() + `})

var ToolPageReturn = Type("ToolPageReturn", func() {
	Field(1, "items", ArrayOf(String))
	Required("items")
})
`
		if err := os.WriteFile(filepath.Join(designDir, "types.go"), []byte(typesSrc), 0o600); err != nil {
			t.Fatalf("write types.go: %v", err)
		}

		svcSrc := `package design

import (
	. "goa.design/goa-ai/dsl"
	. "goa.design/goa/v3/dsl"
	m "goa.design/model/dsl"
)

var OrderTools = Toolset("order_tools", func() {
	Tool("search_orders", "Search orders", func() {
		BindTo("orders", "search_orders")
		Args(OrderQuery)
		Return(ToolPageReturn)
		BoundedResult()
	})
})

var _ = Service("orders", func() {
	Security(APIKeyAuth)
	JSONRPC(func() {
		POST("/mcp")
	})
	MCP("orders_mcp", "1.0.0")
	StaticPrompt("triage", "Triage prompt",
		"user", ` + "`" + promptMsg + "`" + `,
	)
	Method("search_orders", func() {
		Payload(OrderQuery)
		Result(MethodPageResult)
		HTTP(func() {
			GET("/orders")
			` + authIn + `("key:X-API-Key")
			Response(StatusOK)
		})
		GRPC(func() {
			Response(CodeOK)
		})
	})
	Method("legacy_orders", func() {
		NoSecurity()
		HTTP(func() {
			GET("/old-orders")
			Redirect("/orders", StatusMovedPermanently)
		})
	})
	Agent("lookup_agent", "Uses Lookup on BindField", func() {
		Use(OrderTools, func() {
			BindField("tenant_id", func() {
				Lookup(` + lookupType + `)
			})
		})
		RunPolicy(func() {
			DefaultCaps(MaxToolCalls(4))
			Timing(func() {
				Budget("1m")
				Plan("15s")
				Tools("10s")
			})
			OnMissingFields("await_clarification")
			History(func() {
				CompressAtTurns(10)
				KeepMaxTurns(4)
			})
		})
	})
})

var _ = m.Design("Architecture", "Orders architecture", func() {
	sys := m.SoftwareSystem("OrdersSystem", ` + "`" + sysDesc + "`" + `, func() {
		m.Container("OrdersAPI", "Goa service", "Go", func() {})
	})
	m.Person("Operator", "System operator", func() {
		m.Uses(sys, "Manages orders", "HTTPS", m.Synchronous)
	})
})
`
		if err := os.WriteFile(filepath.Join(designDir, "service.go"), []byte(svcSrc), 0o600); err != nil {
			t.Fatalf("write service.go: %v", err)
		}
		return goMod
	}

	writeModule(oldRoot, 10, "Int", "Header", "triage v1\npackage main\nfunc main() {}", "String", true, "Orders system v1")
	writeModule(newRoot, 20, "UInt32", "Param", "triage v2\npackage main\nfunc main() {}", "String", false, "Orders system v2")
	oldFiles := testmodule.Module(t, "github.com/acme/orders", "", goFiles(t, oldRoot))
	newFiles := testmodule.Module(t, "github.com/acme/orders", "", goFiles(t, newRoot))

	rep, err := Compare(ctx, testEvaluator, oldFiles, newFiles)
	if err != nil {
		t.Fatalf("Compare multi-file design directories failed: %v", err)
	}
	for _, wantMod := range []string{
		"Type:OrderQuery",
		"Service:orders/Method:search_orders",
		"Generated:gen/mcp_orders/adapter_server.go",
		"Generated:gen/orders/toolsets/order_tools/specs.go",
		"Model:System:OrdersSystem",
	} {
		if !slices.Contains(rep.ModifiedDeclarations, wantMod) {
			t.Fatalf("expected ModifiedDeclarations to contain %q, got %v", wantMod, rep.ModifiedDeclarations)
		}
	}

	evalOld, err := testEvaluator.Evaluate(ctx, oldFiles)
	if err != nil {
		t.Fatalf("Evaluate old failed: %v", err)
	}
	evalNew, err := testEvaluator.Evaluate(ctx, newFiles)
	if err != nil {
		t.Fatalf("Evaluate new failed: %v", err)
	}

	// Lookup declares a key-shaped alternative while ordinary binding omits
	// tenant_id. Neither static view claims which installer the application uses.
	var foundLookupCard bool
	for _, tc := range evalOld.Tools {
		if tc.ID == "orders.lookup_agent/order_tools.search_orders" {
			foundLookupCard = true
			if tc.InputBinding == nil || tc.InputBinding.LookupArgsSchema == nil || !strings.Contains(compactSchema(t, *tc.InputBinding.LookupArgsSchema), `"tenant_id":{"type":"string"}`) {
				t.Fatalf("expected lookup_agent ArgsSchema to keep tenant_id as string lookup, got %s", tc.ArgsSchema)
			}
			// MethodPageResult required "total" in oldRoot even though ToolPageReturn did not.
			if !strings.Contains(compactSchema(t, tc.ReturnSchema), `"required":["items","returned","total","truncated"]`) {
				t.Fatalf("expected ReturnSchema to require total when Method.Result requires total, got %s", tc.ReturnSchema)
			}
		}
	}
	if !foundLookupCard {
		t.Fatalf("expected per-consumer card orders.lookup_agent/order_tools.search_orders in %v", evalOld.Tools)
	}

	for _, tc := range evalNew.Tools {
		if tc.ID == "orders.lookup_agent/order_tools.search_orders" {
			// MethodPageResult did NOT require "total" in newRoot.
			if strings.Contains(compactSchema(t, tc.ReturnSchema), `"required":["items","returned","total","truncated"]`) {
				t.Fatalf("expected ReturnSchema not to require total when Method.Result does not require total, got %s", tc.ReturnSchema)
			}
		}
	}
}

// goFiles reads every .go file under root into a snapshot map keyed by
// slash-separated relative path.
func goFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// compactSchema validates a schema and removes presentation whitespace for
// assertions that inspect individual generated JSON fragments.
func compactSchema(t *testing.T, schema string) string {
	t.Helper()
	var output bytes.Buffer
	if err := json.Compact(&output, []byte(schema)); err != nil {
		t.Fatalf("invalid generated schema: %v", err)
	}
	return output.String()
}
