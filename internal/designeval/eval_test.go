// Package designeval tests sandboxed Goa/Goa-AI design evaluation and JSON
// Schema extraction.
package designeval

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestSandboxedExit71AppendsConditionalRecoveryGuidance(t *testing.T) {
	ctx := t.Context()

	exit71Design := `package design

import (
	"os"
	. "goa.design/goa/v3/dsl"
)

func init() {
	os.Exit(71)
}

var _ = API("orders", func() {})
`
	snap := testmodule.Snapshot(t, "", exit71Design)
	_, err := testEvaluator.Evaluate(ctx, snap)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit status 71")
	assert.Contains(t, err.Error(), exit71Guidance)

	_, err = testEvaluator.Generate(ctx, snap)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit status 71")
	assert.Contains(t, err.Error(), exit71Guidance)

	// Ordinary compiler failures do not append exit-71 guidance.
	brokenSnap := testmodule.Snapshot(t, "", minimalDesign+"\nvar _ = undefinedInDesign\n")
	_, err = testEvaluator.Evaluate(ctx, brokenSnap)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), exit71Guidance)

	// Direct check of build exit-71 and canceled-context handling in withExit71Guidance.
	exit71CmdErr := exec.Command("sh", "-c", "exit 71").Run()
	require.Error(t, exit71CmdErr)
	wrapped := withExit71Guidance(ctx, fmt.Errorf("compile Goa design: %w", exit71CmdErr))
	assert.Contains(t, wrapped.Error(), exit71Guidance)

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	canceledWrapped := withExit71Guidance(canceledCtx, fmt.Errorf("compile Goa design: %w", exit71CmdErr))
	assert.NotContains(t, canceledWrapped.Error(), exit71Guidance, "a canceled build keeps its error without exit-71 guidance")
}
