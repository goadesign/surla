package designeval

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/testmodule"
)

// preCompleteSourcesGoa is the goa.design/goa/v3 version starters created
// before generator.CompleteSources require; it has no CompleteSources.
const preCompleteSourcesGoa = "v3.35.1-0.20261030163856-3e08a6128171"

func TestGenerateReturnsWhatGoaGenWrites(t *testing.T) {
	files := testmodule.Snapshot(t, completeSourcesGoa, minimalDesign)
	out, err := testEvaluator.Generate(t.Context(), files)
	require.NoError(t, err)
	require.Contains(t, out, "gen/orders/service.go")
	assert.Contains(t, string(out["gen/goa.json"]), "goa_version", "the gen command's version file is included")
	assert.Contains(t, string(out["gen/orders/service.go"]), "$ surla gen", "headers name the command that generated them")
	for p := range out {
		assert.True(t, strings.HasPrefix(p, "gen/") || p == "AGENTS_QUICKSTART.md", p)
	}

	res, err := testEvaluator.Evaluate(t.Context(), files)
	require.NoError(t, err, "evaluation of the same module is unchanged")
	assert.Contains(t, res.Declarations, "Service:orders/Method:get_order")
}

func TestGenerateNeedsAGoaWithCompleteSources(t *testing.T) {
	cases := []struct {
		name string
		// files is the target module.
		files func(t *testing.T) map[string]string
		// wantErr lists fragments of the expected error; empty means success.
		wantErr []string
	}{
		{
			name: "goa without CompleteSources",
			files: func(t *testing.T) map[string]string {
				return testmodule.Snapshot(t, preCompleteSourcesGoa, minimalDesign)
			},
			wantErr: []string{
				"If that output shows that generator.CompleteSources is undefined",
				"(" + preCompleteSourcesGoa + ") lacks it",
				"requirement, or its replace directive",
			},
		},
		{
			name: "old requirement replaced by a goa that has it",
			files: func(t *testing.T) map[string]string {
				return withGoaReplace(t, preCompleteSourcesGoa, completeSourcesGoa)
			},
		},
		{
			name: "new requirement replaced by a goa that lacks it",
			files: func(t *testing.T) map[string]string {
				return withGoaReplace(t, completeSourcesGoa, preCompleteSourcesGoa)
			},
			wantErr: []string{"=> goa.design/goa/v3@" + preCompleteSourcesGoa, "CompleteSources"},
		},
		{
			name: "goa without CompleteSources and an unrelated design error",
			files: func(t *testing.T) map[string]string {
				return testmodule.Snapshot(t, preCompleteSourcesGoa, minimalDesign+"\nvar _ = undefinedInDesign\n")
			},
			wantErr: []string{"undefinedInDesign", "If that output shows that generator.CompleteSources is undefined"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := testEvaluator.Generate(t.Context(), tc.files(t))
			if len(tc.wantErr) == 0 {
				require.NoError(t, err)
				assert.Contains(t, out, "gen/orders/service.go")
				return
			}
			require.Error(t, err)
			for _, w := range tc.wantErr {
				assert.ErrorContains(t, err, w)
			}
			assert.True(t, strings.HasPrefix(err.Error(), "compile Goa design"),
				"the compiler's error leads; the CompleteSources recovery follows it: %v", err)
			assert.Nil(t, out)
		})
	}
}

func TestGenerateRefusesAGRPCDesign(t *testing.T) {
	design := strings.Replace(minimalDesign, `Method("get_order", func() {`, "Method(\"get_order\", func() {\n\t\tGRPC(func() {})", 1)
	_, err := testEvaluator.Generate(t.Context(), testmodule.Snapshot(t, completeSourcesGoa, design))
	require.Error(t, err)
	assert.ErrorContains(t, err, ".proto (finalizer)", "Goa names the protobuf file protoc would finish")
}

// withGoaReplace returns the minimal design's module requiring goa at
// required and replacing it with goa at replaced, tidied.
func withGoaReplace(t *testing.T, required, replaced string) map[string]string {
	t.Helper()
	files := map[string]string{
		"go.mod": "module example.com/design\n\ngo 1.24\n\nrequire goa.design/goa/v3 " + required +
			"\n\nreplace goa.design/goa/v3 => goa.design/goa/v3 " + replaced + "\n",
		"design/design.go": minimalDesign,
	}
	return testmodule.Tidy(t, files)
}
