package designeval

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/testmodule"
	"github.com/goadesign/surla/verify/suite"
)

// preCompleteSourcesGoa is goa-ai's minimum Goa version, which lacks
// generator.CompleteSources. Tests use it to verify generation rejection and
// evaluation with a target version older than Surla's own.
const preCompleteSourcesGoa = "v3.35.1-0.20261102054348-8481e49a6c89"

func TestGenerateReturnsWhatGoaGenWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
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
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
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
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
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

// TestGenerateWritesSurlasQuickstartSections runs the sandboxed generation
// program on designs with an agent, without and with an evaluation suite.
// AGENTS_QUICKSTART.md carries Surla's header, liftoff and golden-rules
// sections, and Surla's evaluations section only when no suite is declared;
// Goa-AI's description of the design, its suites and the generated example
// code stays.
func TestGenerateWritesSurlasQuickstartSections(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	const design = `package design
import (
 . "goa.design/goa-ai/dsl"
 evaldsl "goa.design/goa-ai/eval/dsl"
 . "goa.design/goa/v3/dsl"
)
var _ = evaldsl.Suite
var _ = Service("helpdesk", func() {
 Agent("triage", "Routes support questions", func() {
  Use("lookup", func() {
   Tool("find_ticket", "Find a ticket", func() {
    Args(func() { Attribute("id", String); Required("id") })
    Return(func() { Attribute("title", String); Required("title") })
   })
  })
  SUITE
 })
})`
	const suite = `evaldsl.Suite("routing", func() {
   Description("Routes questions to the right queue.")
   Timeout("30s")
   evaldsl.Scenario("billing_question", func() { Description("A billing question is routed to billing.") })
  })`
	cases := []struct {
		name, suite string
		replaced    []string
		kept        string
	}{
		{"no suite", "", []string{"header", "liftoff", "evaluations", "golden-rules"}, ""},
		{"declared suite", suite, []string{"header", "liftoff", "golden-rules"}, "### Suite `routing`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := testmodule.Snapshot(t, "", strings.Replace(design, "SUITE", tc.suite, 1))
			out, err := testEvaluator.Generate(t.Context(), files)
			require.NoError(t, err)
			guide := string(out[quickstartPath])
			require.NotEmpty(t, guide)
			for _, name := range tc.replaced {
				assert.Contains(t, guide, quickstartSections[name], "Surla's %s section", name)
			}
			assert.Contains(t, guide, "helpdesk.triage", "Goa-AI's design overview stays")
			assert.Contains(t, guide, "### Understanding the Generated Code", "Goa-AI's liftoff-guide stays")
			assert.NotContains(t, guide, "goa example <module>/design")
			if tc.kept != "" {
				assert.Contains(t, guide, tc.kept, "Goa-AI's suite description stays")
				assert.NotContains(t, guide, quickstartSections["evaluations"])
			}
		})
	}
}

func TestGenerateWithStoriesStarterEvaluatorLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	evalInput, fullTree := starterEvalModule(t)

	// Round 1: two finished flows and one incomplete draft.
	round1Stories := []StorySummary{
		{
			Name: "Refunds",
			Flows: []StoryFlow{
				{
					Key:      "refund-over-limit",
					Title:    "A refund above the automatic limit waits for a manager.",
					Complete: true,
					Steps: []StoryStep{
						{Key: "ask-refund", ActorKey: "customer", Summary: "Customer asks for a refund", Kind: "start"},
						{Key: "refund-paid", ActorKey: "manager", Summary: "Manager approves and pays", Kind: "outcome"},
					},
				},
				{
					Key:      "store-credit-within-limit",
					Title:    "A small refund is issued as instant store credit.",
					Complete: true,
					Steps: []StoryStep{
						{Key: "ask-credit", ActorKey: "customer", Summary: "Customer asks for credit", Kind: "start"},
						{Key: "credit-issued", ActorKey: "agent", Summary: "Agent issues store credit", Kind: "outcome"},
					},
				},
				{
					Key:      "disputed-chargeback",
					Title:    "A customer opens a bank chargeback.",
					Complete: false,
				},
			},
		},
	}

	gen1, err := testEvaluator.GenerateWithStories(t.Context(), evalInput, round1Stories)
	require.NoError(t, err)
	assert.Equal(t, []StoryScenario{
		{
			Story:    "Refunds",
			Key:      "refund-over-limit",
			Scenario: "refund_over_limit",
			Hook:     "RefundOverLimit",
		},
		{
			Story:    "Refunds",
			Key:      "store-credit-within-limit",
			Scenario: "store_credit_within_limit",
			Hook:     "StoreCreditWithinLimit",
		},
	}, gen1.Included)
	assert.Equal(t, []StoryDraft{
		{Story: "Refunds", Key: "disputed-chargeback"},
	}, gen1.Excluded)

	suiteGo1 := string(gen1.Files["gen/evals/shop_acceptance/suite.go"])
	require.NotEmpty(t, suiteGo1)
	assert.Contains(t, suiteGo1, "RefundOverLimit(context.Context) (eval.Result, error)")
	assert.Contains(t, suiteGo1, "StoreCreditWithinLimit(context.Context) (eval.Result, error)")
	assert.NotContains(t, suiteGo1, "DisputedChargeback")

	// With the starter's unchanged hooks, building the suite command fails
	// naming the missing methods on *acceptance.Hooks.
	modDir := stageStarterEvalTree(t, fullTree, gen1.Files)
	_, buildOut, buildErr := buildEvalCommand(t, modDir)
	require.Error(t, buildErr, "unchanged starter hooks must fail to compile until flow methods are implemented")
	assert.Contains(t, buildOut, "RefundOverLimit")

	// Implementing the two methods allows the suite command to build and
	// --describe lists the Story scenarios after the starter's own scenarios.
	hooksWithRound1 := fullTree["acceptance/acceptance.go"] + `
func (h *Hooks) RefundOverLimit(ctx context.Context) (eval.Result, error) {
	return eval.Result{}, nil
}

func (h *Hooks) StoreCreditWithinLimit(ctx context.Context) (eval.Result, error) {
	return eval.Result{}, nil
}
`
	require.NoError(t, os.WriteFile(filepath.Join(modDir, "acceptance", "acceptance.go"), []byte(hooksWithRound1), 0o600))
	bin1, buildOut, buildErr := buildEvalCommand(t, modDir)
	require.NoError(t, buildErr, "compile with round 1 hooks:\n%s", buildOut)
	decl1 := describeEvalSuite(t, bin1)
	assert.Equal(t, "shop_acceptance", decl1.ID)
	assert.Equal(t, []string{
		"prices_tea",
		"prompt_override_reaches_model",
		"unknown_item",
		"refund_over_limit",
		"store_credit_within_limit",
	}, scenarioIDs(decl1))

	// Round 2: remove one flow (store-credit-within-limit), rename a flow key
	// (disputed-chargeback -> chargeback-review, now complete, plus test
	// renaming store-credit-within-limit -> instant-store-credit while removing
	// a prior flow), and only relabel a step on refund-over-limit.
	// First establish a 3-flow state so Round 2 removes one finished flow,
	// renames another finished flow's key, and relabels a step on the third.
	baseline3Stories := []StorySummary{
		{
			Name: "Refunds",
			Flows: []StoryFlow{
				{
					Key:      "refund-over-limit",
					Title:    "A refund above the automatic limit waits for a manager.",
					Complete: true,
					Steps: []StoryStep{
						{Key: "ask-refund", ActorKey: "customer", Summary: "Customer asks for a refund", Kind: "start"},
						{Key: "refund-paid", ActorKey: "manager", Summary: "Manager approves and pays", Kind: "outcome"},
					},
				},
				{
					Key:      "store-credit-within-limit",
					Title:    "A small refund is issued as instant store credit.",
					Complete: true,
				},
				{
					Key:      "vip-exchange",
					Title:    "A VIP exchange ships immediately.",
					Complete: true,
				},
			},
		},
	}
	gen3, err := testEvaluator.GenerateWithStories(t.Context(), evalInput, baseline3Stories)
	require.NoError(t, err)
	hooksWith3 := hooksWithRound1 + `
func (h *Hooks) VipExchange(ctx context.Context) (eval.Result, error) {
	return eval.Result{}, nil
}
`
	fullTree["acceptance/acceptance.go"] = hooksWith3
	modDir3 := stageStarterEvalTree(t, fullTree, gen3.Files)
	bin3, buildOut, buildErr := buildEvalCommand(t, modDir3)
	require.NoError(t, buildErr, "compile with 3-flow hooks:\n%s", buildOut)
	assert.Equal(t, []string{
		"prices_tea",
		"prompt_override_reaches_model",
		"unknown_item",
		"refund_over_limit",
		"store_credit_within_limit",
		"vip_exchange",
	}, scenarioIDs(describeEvalSuite(t, bin3)))

	// Now run the second round with:
	// - vip-exchange removed,
	// - store-credit-within-limit key renamed to instant-store-credit,
	// - refund-over-limit's step summary relabeled without changing its key.
	round2Stories := []StorySummary{
		{
			Name: "Refunds",
			Flows: []StoryFlow{
				{
					Key:      "refund-over-limit",
					Title:    "A refund above the automatic limit waits for a manager.",
					Complete: true,
					Steps: []StoryStep{
						{
							Key:      "ask-refund",
							ActorKey: "customer",
							Summary:  "Customer submits a refund request with receipt",
							Kind:     "start",
						},
						{
							Key:      "refund-paid",
							ActorKey: "manager",
							Summary:  "Regional manager approves and triggers payout",
							Kind:     "outcome",
						},
					},
				},
				{
					Key:      "instant-store-credit",
					Title:    "A small refund is issued as instant store credit.",
					Complete: true,
				},
			},
		},
	}
	gen2, err := testEvaluator.GenerateWithStories(t.Context(), evalInput, round2Stories)
	require.NoError(t, err)
	assert.Equal(t, []StoryScenario{
		{
			Story:    "Refunds",
			Key:      "refund-over-limit",
			Scenario: "refund_over_limit",
			Hook:     "RefundOverLimit",
		},
		{
			Story:    "Refunds",
			Key:      "instant-store-credit",
			Scenario: "instant_store_credit",
			Hook:     "InstantStoreCredit",
		},
	}, gen2.Included)
	assert.Empty(t, gen2.Excluded)

	suiteGo2 := string(gen2.Files["gen/evals/shop_acceptance/suite.go"])
	assert.Contains(t, suiteGo2, "RefundOverLimit(context.Context) (eval.Result, error)")
	assert.Contains(t, suiteGo2, "InstantStoreCredit(context.Context) (eval.Result, error)")
	assert.NotContains(t, suiteGo2, "StoreCreditWithinLimit")
	assert.NotContains(t, suiteGo2, "VipExchange")

	// Before adding InstantStoreCredit, compilation fails naming only the
	// renamed method (RefundOverLimit is unchanged).
	modDir2 := stageStarterEvalTree(t, fullTree, gen2.Files)
	_, buildOut, buildErr = buildEvalCommand(t, modDir2)
	require.Error(t, buildErr)
	assert.Contains(t, buildOut, "InstantStoreCredit")

	// Adding InstantStoreCredit while keeping the old StoreCreditWithinLimit and
	// VipExchange methods on *Hooks compiles cleanly and updates --describe for
	// only the removed and renamed scenarios.
	hooksWithRound2 := hooksWith3 + `
func (h *Hooks) InstantStoreCredit(ctx context.Context) (eval.Result, error) {
	return eval.Result{}, nil
}
`
	require.NoError(t, os.WriteFile(filepath.Join(modDir2, "acceptance", "acceptance.go"), []byte(hooksWithRound2), 0o600))
	bin2, buildOut, buildErr := buildEvalCommand(t, modDir2)
	require.NoError(t, buildErr, "old hand-written methods must still compile alongside the renamed hook:\n%s", buildOut)
	decl2 := describeEvalSuite(t, bin2)
	assert.Equal(t, []string{
		"prices_tea",
		"prompt_override_reaches_model",
		"unknown_item",
		"refund_over_limit",
		"instant_store_credit",
	}, scenarioIDs(decl2))
}

func TestGenerateWithStoriesNoFinishedFlowMatchesGenerate(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	files := testmodule.Snapshot(t, "", minimalDesign)
	wantFiles, err := testEvaluator.Generate(t.Context(), files)
	require.NoError(t, err)

	cases := []struct {
		name         string
		stories      []StorySummary
		wantExcluded []StoryDraft
	}{
		{
			name:    "nil stories",
			stories: nil,
		},
		{
			name: "only incomplete drafts on a design with zero suites",
			stories: []StorySummary{
				{
					Name: "Refunds",
					Flows: []StoryFlow{
						{Key: "draft-one", Title: "First draft", Complete: false},
						{Key: "draft-two", Title: "Second draft", Complete: false},
					},
				},
			},
			wantExcluded: []StoryDraft{
				{Story: "Refunds", Key: "draft-one"},
				{Story: "Refunds", Key: "draft-two"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := testEvaluator.GenerateWithStories(t.Context(), files, tc.stories)
			require.NoError(t, err)
			assert.Equal(t, wantFiles, got.Files)
			assert.Empty(t, got.Included)
			assert.Equal(t, tc.wantExcluded, got.Excluded)
		})
	}
}

func TestGenerateWithStoriesRejectsFlowCollisionsAndSuiteCountErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	singleSuiteDesign := `package design
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
	Scenario("prices_tea", func() {
		Description("Existing manual scenario.")
	})
})
`
	twoSuitesDesign := singleSuiteDesign + `
var _ = Suite("second_suite", func() {
	Description("Second suite.")
	Timeout("30s")
	Scenario("other_scenario", func() {
		Description("Other scenario.")
	})
})
`
	singleSuiteFiles := testmodule.Snapshot(t, "", singleSuiteDesign)
	zeroSuitesFiles := testmodule.Snapshot(t, "", minimalDesign)
	twoSuitesFiles := testmodule.Snapshot(t, "", twoSuitesDesign)

	cases := []struct {
		name    string
		files   map[string]string
		stories []StorySummary
		wantErr []string
	}{
		{
			name:  "two Stories sharing a finished flow key",
			files: singleSuiteFiles,
			stories: []StorySummary{
				{
					Name:  "Refunds",
					Flows: []StoryFlow{{Key: "refund-over-limit", Title: "First", Complete: true}},
				},
				{
					Name:  "Disputes",
					Flows: []StoryFlow{{Key: "refund-over-limit", Title: "Second", Complete: true}},
				},
			},
			wantErr: []string{
				`"refund-over-limit" in story "Refunds"`,
				`"refund-over-limit" in story "Disputes"`,
				`scenario "refund_over_limit"`,
			},
		},
		{
			name:  "a-b and a_b map to the same scenario",
			files: singleSuiteFiles,
			stories: []StorySummary{
				{
					Name: "Refunds",
					Flows: []StoryFlow{
						{Key: "a-b", Title: "Hyphen flow", Complete: true},
						{Key: "a_b", Title: "Underscore flow", Complete: true},
					},
				},
			},
			wantErr: []string{
				`"a-b" in story "Refunds"`,
				`"a_b" in story "Refunds"`,
				`scenario "a_b"`,
			},
		},
		{
			name:  "flow colliding with a manually declared scenario",
			files: singleSuiteFiles,
			stories: []StorySummary{
				{
					Name:  "Refunds",
					Flows: []StoryFlow{{Key: "prices-tea", Title: "Collides with manual prices_tea", Complete: true}},
				},
			},
			wantErr: []string{
				`scenario name "prices_tea" duplicates scenario "prices_tea"`,
				`both generate hook method "PricesTea"`,
			},
		},
		{
			name:  "zero suites declared when a finished flow is present",
			files: zeroSuitesFiles,
			stories: []StorySummary{
				{
					Name:  "Refunds",
					Flows: []StoryFlow{{Key: "refund-over-limit", Title: "Needs a suite", Complete: true}},
				},
			},
			wantErr: []string{
				"Story scenario generation needs exactly one evaluation suite, but the design declares 0",
				"keep exactly one Suite(...) in design/",
			},
		},
		{
			name:  "two suites declared when a finished flow is present",
			files: twoSuitesFiles,
			stories: []StorySummary{
				{
					Name:  "Refunds",
					Flows: []StoryFlow{{Key: "refund-over-limit", Title: "Needs one suite", Complete: true}},
				},
			},
			wantErr: []string{
				"Story scenario generation needs exactly one evaluation suite, but the design declares 2",
				"keep exactly one Suite(...) in design/",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := testEvaluator.GenerateWithStories(t.Context(), tc.files, tc.stories)
			require.Error(t, err)
			for _, w := range tc.wantErr {
				assert.ErrorContains(t, err, w)
			}
		})
	}
}

func TestGenerateWithStoriesQuotesSpecialTitlesAndBlankTitles(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	evalInput, fullTree := starterEvalModule(t)
	specialTitle := "Line 1: \"quoted\" `backtick` \\backslash\\\nLine 2"
	stories := []StorySummary{
		{
			Name: "Refunds",
			Flows: []StoryFlow{
				{
					Key:      "tricky-title",
					Title:    specialTitle,
					Complete: true,
				},
				{
					Key:      "blank-title-flow",
					Title:    "",
					Complete: true,
				},
			},
		},
	}

	gen, err := testEvaluator.GenerateWithStories(t.Context(), evalInput, stories)
	require.NoError(t, err)
	suiteGo := string(gen.Files["gen/evals/shop_acceptance/suite.go"])
	assert.Contains(t, suiteGo, `Description: "Line 1: \"quoted\" `+"`"+`backtick`+"`"+` \\backslash\\\nLine 2"`)
	assert.Contains(t, suiteGo, `Description: "blank-title-flow"`)

	fullTree["acceptance/acceptance.go"] += `
func (h *Hooks) TrickyTitle(ctx context.Context) (eval.Result, error) {
	return eval.Result{}, nil
}

func (h *Hooks) BlankTitleFlow(ctx context.Context) (eval.Result, error) {
	return eval.Result{}, nil
}
`
	modDir := stageStarterEvalTree(t, fullTree, gen.Files)
	bin, buildOut, buildErr := buildEvalCommand(t, modDir)
	require.NoError(t, buildErr, "compile generated suite with special characters:\n%s", buildOut)
	assert.Equal(t, []string{
		"prices_tea",
		"prompt_override_reaches_model",
		"unknown_item",
		"tricky_title",
		"blank_title_flow",
	}, scenarioIDs(describeEvalSuite(t, bin)))
}

func TestEvaluateAndGeneratePreserveGoaOnlyTargetWithGoModOnlyGoaAIChecksum(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	// Build a tidy module that requires goa-ai, then strip goa-ai's zip
	// checksum line from go.sum while keeping its /go.mod checksum line and a
	// design that imports only Goa.
	withGoaAI := testmodule.Snapshot(t, "", strings.Replace(minimalDesign, "package design\n", "package design\nimport _ \"goa.design/goa-ai/dsl\"\n", 1))
	goaAIVer := testmodule.SurlaVersion(t, harnessGoaAIModule)
	zipPrefix := harnessGoaAIModule + " " + goaAIVer + " h1:"
	goModPrefix := harnessGoaAIModule + " " + goaAIVer + "/go.mod h1:"

	var keptLines []string
	hasGoModSum := false
	for _, line := range strings.Split(withGoaAI["go.sum"], "\n") {
		if strings.HasPrefix(line, zipPrefix) {
			continue
		}
		if strings.HasPrefix(line, goModPrefix) {
			hasGoModSum = true
		}
		keptLines = append(keptLines, line)
	}
	require.True(t, hasGoModSum, "expected /go.mod checksum line for %s %s", harnessGoaAIModule, goaAIVer)

	files := map[string]string{
		"go.mod":           withGoaAI["go.mod"],
		"go.sum":           strings.Join(keptLines, "\n"),
		"design/design.go": minimalDesign,
	}
	require.NotContains(t, files["go.sum"], zipPrefix)

	res, err := testEvaluator.Evaluate(t.Context(), files)
	require.NoError(t, err)
	assert.Contains(t, res.Declarations, "Service:orders/Method:get_order")

	genFiles, err := testEvaluator.Generate(t.Context(), files)
	require.NoError(t, err)
	assert.Contains(t, genFiles, "gen/orders/service.go")

	storyGen, err := testEvaluator.GenerateWithStories(t.Context(), files, nil)
	require.NoError(t, err)
	assert.Equal(t, genFiles, storyGen.Files)
}

func TestGenerateWithStoriesFailsAfterClose(t *testing.T) {
	ev := NewEvaluatorInDir(t.TempDir())
	require.NoError(t, ev.Close())
	_, err := ev.GenerateWithStories(t.Context(), map[string]string{}, nil)
	assert.ErrorIs(t, err, errEvaluatorClosed)
}

// starterEvalModule renders the starter evaluator templates from
// internal/scaffold/templates/eval for module example.com/shop/eval, tidies
// the module against Surla's own dependency versions, and returns both the
// generator input snapshot (go.mod, go.sum, design/design.go and the
// acceptance package, as surla init generates with it) and the complete
// hand-written module tree (acceptance/ and scripted/); the module's program,
// gen/cmd/shop-evals, comes from the generator.
func starterEvalModule(t *testing.T) (evalInput, fullTree map[string]string) {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	tmplRoot := filepath.Join(repoRoot, "internal", "scaffold", "templates", "eval")
	readRendered := func(rel string) string {
		raw, err := os.ReadFile(filepath.Join(tmplRoot, filepath.FromSlash(rel)))
		require.NoError(t, err)
		s := string(raw)
		s = strings.ReplaceAll(s, "[[.Module]]", "example.com/shop")
		s = strings.ReplaceAll(s, "[[.Name]]", "shop")
		s = strings.ReplaceAll(s, "[[.Pkg]]", "shop")
		return s
	}

	designSrc := readRendered("design/design.go.tmpl")
	evalInput = testmodule.Module(t, "example.com/shop/eval", "", map[string]string{
		"design/design.go": designSrc,
	})
	// The acceptance package imports code generation writes, so it joins the
	// input after tidying; generation loads only the design packages.
	evalInput["acceptance/acceptance.go"] = readRendered("acceptance/acceptance.go.tmpl")
	evalInput["acceptance/forwarder.go"] = readRendered("acceptance/forwarder.go.tmpl")
	initGen, err := testEvaluator.Generate(t.Context(), evalInput)
	require.NoError(t, err)

	fullTree = map[string]string{
		"go.mod": "module example.com/shop/eval\n\ngo 1.24\n\nrequire (\n" +
			"\tgithub.com/goadesign/surla v0.0.0\n" +
			"\tgoa.design/goa/v3 " + testmodule.SurlaVersion(t, "goa.design/goa/v3") + "\n" +
			"\tgoa.design/goa-ai " + testmodule.SurlaVersion(t, "goa.design/goa-ai") + "\n)\n\n" +
			"replace github.com/goadesign/surla => " + repoRoot + "\n",
		"design/design.go":         designSrc,
		"acceptance/acceptance.go": readRendered("acceptance/acceptance.go.tmpl"),
		"acceptance/forwarder.go":  readRendered("acceptance/forwarder.go.tmpl"),
		"scripted/scripted.go":     readRendered("scripted/scripted.go.tmpl"),
	}
	withGen := maps.Clone(fullTree)
	for path, content := range initGen {
		if strings.HasSuffix(path, ".go") {
			withGen[path] = string(content)
		}
	}
	tidied := testmodule.Tidy(t, withGen)
	fullTree["go.mod"] = tidied["go.mod"]
	fullTree["go.sum"] = tidied["go.sum"]
	return evalInput, fullTree
}

// stageStarterEvalTree writes baseFiles and generatedFiles into a fresh
// temporary directory and returns its path.
func stageStarterEvalTree(t *testing.T, baseFiles map[string]string, generatedFiles map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range baseFiles {
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o750))
		require.NoError(t, os.WriteFile(dst, []byte(content), 0o600))
	}
	for rel, content := range generatedFiles {
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o750))
		require.NoError(t, os.WriteFile(dst, content, 0o600))
	}
	return dir
}

// buildEvalCommand runs `go build -mod=readonly` on the generated program,
// ./gen/cmd/shop-evals, in dir and returns the output binary path, combined
// compiler output, and build error.
func buildEvalCommand(t *testing.T, dir string) (string, string, error) {
	t.Helper()
	bin := filepath.Join(dir, "shop-evals")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-mod=readonly", "-o", bin, "./gen/cmd/shop-evals")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off", "GOTOOLCHAIN=local")
	out, err := cmd.CombinedOutput()
	return bin, string(out), err
}

// describeEvalSuite runs the compiled suite binary with --describe and decodes
// its JSON suite.Declaration output.
func describeEvalSuite(t *testing.T, bin string) suite.Declaration {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), bin, "--describe")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "--describe output:\n%s", out)
	var decl suite.Declaration
	require.NoError(t, json.Unmarshal(out, &decl))
	return decl
}

// scenarioIDs returns the ordered scenario IDs in decl.
func scenarioIDs(decl suite.Declaration) []string {
	ids := make([]string, 0, len(decl.Scenarios))
	for _, sc := range decl.Scenarios {
		ids = append(ids, sc.ID)
	}
	return ids
}
