// Package designeval tests sandboxed evaluation of portable Story declarations
// alongside Goa designs and in Story-only modules.
package designeval

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/testmodule"
)

// publishedStoryVersion is the module version of github.com/goadesign/surla/story
// used by target snapshots in Story observation tests.
const publishedStoryVersion = "v0.0.0-20261111183703-4568fcf5bb85"

func TestEvaluateProgramSpecializesStoryLoading(t *testing.T) {
	withoutStory := evaluateProgram(map[string]bool{
		harnessGoaModule: true,
	})
	require.Len(t, withoutStory, 1)
	assert.Contains(t, withoutStory, "main.go")
	assert.NotContains(t, withoutStory, "story.go")
	assert.NotContains(t, withoutStory["main.go"], harnessStoryModule)
	assert.NotContains(t, withoutStory["main.go"], "storySummaries")
	assert.NotContains(t, withoutStory["main.go"], "storySummary")
	assert.NotContains(t, withoutStory["main.go"], "hadGoaAPI")

	withStory := evaluateProgram(map[string]bool{
		harnessGoaModule:   true,
		harnessStoryModule: true,
	})
	require.Len(t, withStory, 2)
	assert.Contains(t, withStory["main.go"], "Stories       []storySummary")
	assert.Contains(t, withStory["main.go"], "hadGoaAPI := goaexpr.Root.API != nil || len(goaexpr.Root.Services) > 0")
	assert.Contains(t, withStory["main.go"], "out.Stories = storySummaries()")
	assert.Contains(t, withStory["main.go"], "if (hadGoaAPI || len(out.Stories) == 0) && api != nil")
	assert.Contains(t, withStory["story.go"], `storyexpr "github.com/goadesign/surla/story/expr"`)
	assert.True(t, strings.HasPrefix(withStory["story.go"], "package main\n"), "story_reader.go becomes the program's story.go")
	assert.Contains(t, withStory["story.go"], "func storySummaries() []storySummary {")
}

func TestEvaluateObservesStoryOnlyAndCombinedDesigns(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	ctx := t.Context()

	t.Run("story_only_with_helper_package_and_unfinished_drafts", func(t *testing.T) {
		files := storySnapshot(t, preCompleteSourcesGoa, map[string]string{
			"storyhelpers/actors.go": `package storyhelpers

import . "github.com/goadesign/surla/story/dsl"

const (
	WarehouseWorker   = "warehouse-worker"
	InsuranceAgent    = "insurance-agent"
	ComplianceOfficer = "compliance-officer"
)

func DeclareReturnActors() {
	Actor(WarehouseWorker, "Warehouse Worker", func() {
		Description("Inspects returned parcels")
	})
	Actor(InsuranceAgent, "Insurance Agent", func() {
		Description("Reviews damaged-parcel claims")
	})
	Actor(ComplianceOfficer, "Compliance Officer", func() {
		Description("Audits high-value write-offs")
	})
}
`,
			"design/stories.go": `package design

import (
	"example.com/story-design/storyhelpers"
	. "github.com/goadesign/surla/story/dsl"
)

var _ = Story("Warehouse Returns", func() {
	Description("Handles damaged returns and carrier claims")
	storyhelpers.DeclareReturnActors()

	Flow("damaged-claim-approved", "Damaged parcel claim is approved", func() {
		Description("Complete path from damaged parcel intake to payout")
		StartsWith("parcel-received", storyhelpers.WarehouseWorker, "Logs a damaged return parcel", func() {
			Description("Photos and barcode attached")
		})
		Step("claim-submitted", storyhelpers.InsuranceAgent, "Approves the damaged parcel claim")
		Outcome("refund-authorized", storyhelpers.WarehouseWorker, "Marks the return ready for payout")
	})

	Flow("missing-start-draft", "Draft flow without a starting step", func() {
		Step("audit-logged", storyhelpers.ComplianceOfficer, "Records an audit note")
		Outcome("audit-filed", storyhelpers.ComplianceOfficer, "Archives the audit record")
	})

	Flow("missing-outcome-draft", "Draft flow without an outcome", func() {
		StartsWith("claim-opened", storyhelpers.InsuranceAgent, "Opens a manual review")
	})

	Flow("flow-question-draft", "Draft flow with a flow-level question", func() {
		StartsWith("high-value-return", storyhelpers.WarehouseWorker, "Flags a return over the write-off limit")
		Outcome("write-off-recorded", storyhelpers.ComplianceOfficer, "Records the write-off decision")
		Question("write-off-threshold", "Which threshold requires compliance sign-off?", func() {
			Description("Applies to damaged warehouse stock")
			Option("$500")
			Option("$1,000")
		})
	})

	Flow("step-question-draft", "Draft flow with a step-level question", func() {
		StartsWith("carrier-dispute", storyhelpers.WarehouseWorker, "Opens a carrier damage dispute", func() {
			Question("carrier-evidence", "Is a signed carrier manifest required?")
		})
		Outcome("dispute-sent", storyhelpers.InsuranceAgent, "Sends the dispute packet to the carrier")
	})
})

var _ = Story("Carrier Settlements", func() {
	Description("Monthly carrier reconciliation")
	Actor(storyhelpers.InsuranceAgent, "Insurance Agent")

	Flow("monthly-close", "Monthly settlement closes cleanly", func() {
		StartsWith("statement-imported", storyhelpers.InsuranceAgent, "Imports the monthly carrier statement")
		Outcome("ledger-balanced", storyhelpers.InsuranceAgent, "Confirms the carrier ledger balances")
	})
})
`,
		})

		res, err := testEvaluator.Evaluate(ctx, files)
		require.NoError(t, err)
		assert.Empty(t, res.SystemName, "Story-only design must not fabricate a Goa system name")
		assert.Empty(t, res.Containers, "Story-only design declares no architecture containers")
		assert.Empty(t, res.Edges)
		assert.NotContains(t, res.Declarations, "API:API")

		want := []StorySummary{
			{
				Name:        "Warehouse Returns",
				Description: "Handles damaged returns and carrier claims",
				Actors: []StoryActor{
					{
						Key:         "warehouse-worker",
						Title:       "Warehouse Worker",
						Description: "Inspects returned parcels",
					},
					{
						Key:         "insurance-agent",
						Title:       "Insurance Agent",
						Description: "Reviews damaged-parcel claims",
					},
					{
						Key:         "compliance-officer",
						Title:       "Compliance Officer",
						Description: "Audits high-value write-offs",
					},
				},
				Flows: []StoryFlow{
					{
						Key:         "damaged-claim-approved",
						Title:       "Damaged parcel claim is approved",
						Description: "Complete path from damaged parcel intake to payout",
						Complete:    true,
						Steps: []StoryStep{
							{
								Key:         "parcel-received",
								ActorKey:    "warehouse-worker",
								Summary:     "Logs a damaged return parcel",
								Description: "Photos and barcode attached",
								Kind:        "start",
							},
							{
								Key:      "claim-submitted",
								ActorKey: "insurance-agent",
								Summary:  "Approves the damaged parcel claim",
								Kind:     "step",
							},
							{
								Key:      "refund-authorized",
								ActorKey: "warehouse-worker",
								Summary:  "Marks the return ready for payout",
								Kind:     "outcome",
							},
						},
					},
					{
						Key:      "missing-start-draft",
						Title:    "Draft flow without a starting step",
						Complete: false,
						Steps: []StoryStep{
							{
								Key:      "audit-logged",
								ActorKey: "compliance-officer",
								Summary:  "Records an audit note",
								Kind:     "step",
							},
							{
								Key:      "audit-filed",
								ActorKey: "compliance-officer",
								Summary:  "Archives the audit record",
								Kind:     "outcome",
							},
						},
					},
					{
						Key:      "missing-outcome-draft",
						Title:    "Draft flow without an outcome",
						Complete: false,
						Steps: []StoryStep{
							{
								Key:      "claim-opened",
								ActorKey: "insurance-agent",
								Summary:  "Opens a manual review",
								Kind:     "start",
							},
						},
					},
					{
						Key:      "flow-question-draft",
						Title:    "Draft flow with a flow-level question",
						Complete: false,
						Steps: []StoryStep{
							{
								Key:      "high-value-return",
								ActorKey: "warehouse-worker",
								Summary:  "Flags a return over the write-off limit",
								Kind:     "start",
							},
							{
								Key:      "write-off-recorded",
								ActorKey: "compliance-officer",
								Summary:  "Records the write-off decision",
								Kind:     "outcome",
							},
						},
						Questions: []StoryQuestion{
							{
								Key:         "write-off-threshold",
								Prompt:      "Which threshold requires compliance sign-off?",
								Description: "Applies to damaged warehouse stock",
								Options:     []string{"$500", "$1,000"},
							},
						},
					},
					{
						Key:      "step-question-draft",
						Title:    "Draft flow with a step-level question",
						Complete: false,
						Steps: []StoryStep{
							{
								Key:      "carrier-dispute",
								ActorKey: "warehouse-worker",
								Summary:  "Opens a carrier damage dispute",
								Kind:     "start",
								Questions: []StoryQuestion{
									{
										Key:    "carrier-evidence",
										Prompt: "Is a signed carrier manifest required?",
									},
								},
							},
							{
								Key:      "dispute-sent",
								ActorKey: "insurance-agent",
								Summary:  "Sends the dispute packet to the carrier",
								Kind:     "outcome",
							},
						},
					},
				},
			},
			{
				Name:        "Carrier Settlements",
				Description: "Monthly carrier reconciliation",
				Actors: []StoryActor{
					{
						Key:   "insurance-agent",
						Title: "Insurance Agent",
					},
				},
				Flows: []StoryFlow{
					{
						Key:      "monthly-close",
						Title:    "Monthly settlement closes cleanly",
						Complete: true,
						Steps: []StoryStep{
							{
								Key:      "statement-imported",
								ActorKey: "insurance-agent",
								Summary:  "Imports the monthly carrier statement",
								Kind:     "start",
							},
							{
								Key:      "ledger-balanced",
								ActorKey: "insurance-agent",
								Summary:  "Confirms the carrier ledger balances",
								Kind:     "outcome",
							},
						},
					},
				},
			},
		}
		assert.Equal(t, want, res.Stories)
	})

	t.Run("combined_story_and_goa_service", func(t *testing.T) {
		files := storySnapshot(t, completeSourcesGoa, map[string]string{
			"design/design.go": minimalDesign,
			"design/story.go": `package design

import . "github.com/goadesign/surla/story/dsl"

var _ = Story("Order Lookup", func() {
	Actor("customer-support", "Customer Support")
	Flow("lookup-order", "Support looks up an order", func() {
		StartsWith("request-order", "customer-support", "Enters the order identifier")
		Outcome("order-shown", "customer-support", "Sees the matching order details")
	})
})
`,
		})

		res, err := testEvaluator.Evaluate(ctx, files)
		require.NoError(t, err)
		assert.Equal(t, "orders", res.SystemName)
		assert.Contains(t, res.Declarations, "API:orders")
		assert.Contains(t, res.Declarations, "Service:orders/Method:get_order")
		require.Len(t, res.Stories, 1)
		assert.Equal(t, "Order Lookup", res.Stories[0].Name)
		require.Len(t, res.Stories[0].Flows, 1)
		assert.True(t, res.Stories[0].Flows[0].Complete)

		generated, err := testEvaluator.Generate(ctx, files)
		require.NoError(t, err)
		assert.Contains(t, generated, "gen/orders/service.go")
	})

	t.Run("architecture_only_without_story_preserves_model_and_goa_outputs", func(t *testing.T) {
		goaFiles := testmodule.Snapshot(t, preCompleteSourcesGoa, minimalDesign)
		goaRes, err := testEvaluator.Evaluate(ctx, goaFiles)
		require.NoError(t, err)
		assert.Equal(t, "orders", goaRes.SystemName)
		assert.Contains(t, goaRes.Declarations, "API:orders")
		assert.Contains(t, goaRes.Declarations, "Service:orders/Method:get_order")
		assert.Empty(t, goaRes.Stories)

		target, err := validateSnapshot(goaFiles)
		require.NoError(t, err)
		evalMod, err := evaluatorGoMod()
		require.NoError(t, err)
		tc, err := resolveToolchainEnv(ctx)
		require.NoError(t, err)
		buildDir := filepath.Join(t.TempDir(), "build")
		logDir := filepath.Join(t.TempDir(), "logs")
		require.NoError(t, os.MkdirAll(buildDir, 0o750))
		require.NoError(t, os.MkdirAll(logDir, 0o750))
		require.NoError(t, stageSnapshot(filepath.Join(buildDir, "target"), goaFiles))
		gocmd := goCommand{bin: filepath.Join(tc.goroot, "bin", "go")}
		selectedGoa, err := prepareHarnessModule(ctx, gocmd, buildDir, logDir, target, evalMod, evaluateProgram)
		require.NoError(t, err)
		assert.Equal(t, preCompleteSourcesGoa, selectedGoa.Version)
		_, storyErr := os.Stat(filepath.Join(buildDir, "story.go"))
		assert.ErrorIs(t, storyErr, os.ErrNotExist)
		harnessGoMod, err := os.ReadFile(filepath.Join(buildDir, "go.mod"))
		require.NoError(t, err)
		assert.NotContains(t, string(harnessGoMod), harnessStoryModule)
		harnessModules, err := gocmd.listModules(ctx, buildDir, filepath.Join(logDir, "verify-modules.log"), "-mod=readonly")
		require.NoError(t, err)
		assert.NotContains(t, harnessModules, harnessStoryModule)

		modelSource := `package design

import m "goa.design/model/dsl"

var _ = m.Design("Architecture", "Model-only system", func() {
	m.SoftwareSystem("Fulfillment", "Order fulfillment system", func() {
		m.Container("Worker", "Processes fulfillment jobs", "Go")
	})
})
`
		modelRes, err := testEvaluator.Evaluate(ctx, testmodule.Snapshot(t, "", modelSource))
		require.NoError(t, err)
		assert.Equal(t, "API", modelRes.SystemName)
		assert.Contains(t, modelRes.Declarations, "API:API")
		assert.Contains(t, modelRes.Declarations, "Model:System:Fulfillment")
		assert.Contains(t, modelRes.Declarations, "Model:System:Fulfillment/Container:Worker")
		assert.Len(t, modelRes.Containers, 2)
		assert.Empty(t, modelRes.Stories)
	})
}

func TestEvaluateRejectsInvalidStoryDeclarations(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	ctx := t.Context()

	cases := []struct {
		name    string
		design  string
		wantErr string
	}{
		{
			name: "undeclared_actor_in_step",
			design: `package design

import . "github.com/goadesign/surla/story/dsl"

var _ = Story("Returns", func() {
	Actor("warehouse-worker", "Warehouse Worker")
	Flow("return-parcel", "Return a parcel", func() {
		StartsWith("logged", "ghost-actor", "Logs a parcel")
	})
})
`,
			wantErr: `actor "ghost-actor" is not declared in Story "Returns"`,
		},
		{
			name: "duplicate_step_key_in_flow",
			design: `package design

import . "github.com/goadesign/surla/story/dsl"

var _ = Story("Returns", func() {
	Actor("warehouse-worker", "Warehouse Worker")
	Flow("return-parcel", "Return a parcel", func() {
		StartsWith("logged", "warehouse-worker", "Logs a parcel")
		Outcome("logged", "warehouse-worker", "Finishes logging")
	})
})
`,
			wantErr: `duplicate key "logged"`,
		},
		{
			name: "multiple_starts_in_flow",
			design: `package design

import . "github.com/goadesign/surla/story/dsl"

var _ = Story("Returns", func() {
	Actor("warehouse-worker", "Warehouse Worker")
	Flow("return-parcel", "Return a parcel", func() {
		StartsWith("first-start", "warehouse-worker", "Logs a parcel")
		StartsWith("second-start", "warehouse-worker", "Logs another parcel")
	})
})
`,
			wantErr: `flow "return-parcel" has more than one starting step`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := storySnapshot(t, preCompleteSourcesGoa, map[string]string{
				"design/stories.go": tc.design,
			})
			_, err := testEvaluator.Evaluate(ctx, files)
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// storySnapshot builds a tidy module snapshot requiring github.com/goadesign/surla/story
// and goa.design/goa/v3 at goaVersion without requiring goa-ai or model unless
// the provided files import them.
func storySnapshot(t *testing.T, goaVersion string, files map[string]string) map[string]string {
	t.Helper()
	out := maps.Clone(files)
	out["go.mod"] = "module example.com/story-design\n\ngo 1.27.3\n\nrequire (\n" +
		"\t" + harnessStoryModule + " " + publishedStoryVersion + "\n" +
		"\t" + harnessGoaModule + " " + goaVersion + "\n)\n"
	return testmodule.Tidy(t, out)
}
