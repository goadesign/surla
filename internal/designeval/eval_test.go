// Package designeval tests sandboxed Goa/Goa-AI design evaluation and JSON
// Schema extraction.
package designeval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goadesign/surla/internal/testmodule"
)

func TestEvaluateEnforcesSandboxOnDesignInitAndBuildsJSONSchemas(t *testing.T) {
	ctx := context.Background()

	// Create a secret file outside the evaluator's sandbox state directory.
	secretDir := t.TempDir()
	secretFile := filepath.Join(secretDir, "evaluator-secret.txt")
	if err := os.WriteFile(secretFile, []byte("top-secret-holdout-data"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}

	// Hostile design whose init() attempts to read secretFile and fail if allowed.
	hostileDesign := `package custom_pkg

import (
	"os"
	. "goa.design/goa/v3/dsl"
)

func init() {
	if _, err := os.ReadFile(` + "`" + secretFile + "`" + `); err == nil {
		panic("sandbox violation: candidate design init() read protected secret file!")
	}
}

var _ = API("orders", func() {
	HTTP(func() {
		Path("/api/v1")
	})
})

var _ = Service("orders", func() {
	HTTP(func() {
		Path("/orders")
	})
	Method("get_order", func() {
		Payload(func() {
			Attribute("id", String, func() {
				MinLength(3)
			})
			Required("id")
		})
		Result(String)
		HTTP(func() {
			GET("/{id}")
			Response(StatusOK)
		})
	})
})
`
	res, err := testEvaluator.Evaluate(ctx, testmodule.Snapshot(t, "", hostileDesign))
	if err != nil {
		t.Fatalf("Evaluate failed on sandboxed design: %v", err)
	}
	if res.SystemName != "orders" {
		t.Fatalf("expected SystemName orders, got %q", res.SystemName)
	}
	methodSig := res.Declarations["Service:orders/Method:get_order"]
	if res.Declarations["Generated:gen/http/orders/server/paths.go"] == "" {
		t.Fatal("expected the actual generated HTTP path source to be compared")
	}
	if !strings.Contains(methodSig, `"minLength":3`) {
		t.Fatalf("expected JSON Schema minLength:3 in method payload schema, got %q", methodSig)
	}
}
