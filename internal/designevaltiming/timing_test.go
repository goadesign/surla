//go:build timing

// Package designevaltiming measures how long design evaluation takes with the
// designeval workspace as it is built in this tree. It is an experiment, not
// a regular test: it builds only with the timing tag, and each test is meant
// to run in its own fresh `go test` process so the evaluator's build cache
// starts empty. It prints one JSON line per measurement and fails only when
// an operation fails.
package designevaltiming

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	goahttp "goa.design/goa/v3/http"

	genhttpsurla "github.com/goadesign/surla/gen/http/surla/client"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/diff"
	"github.com/goadesign/surla/internal/testmodule"
)

type (
	// measurement is one printed result.
	measurement struct {
		// Probe names the operation and its order in the process.
		Probe string `json:"probe"`
		// Variant is $TIMING_VARIANT, such as baseline or hypothesis0.
		Variant string `json:"variant"`
		// Head is the commit the probe ran on.
		Head string `json:"head"`
		// EvalSource is the git blob hash of internal/designeval/eval.go as
		// checked out, which tells the variants apart on one commit.
		EvalSource string `json:"eval_source"`
		// GoVersion is the selected toolchain.
		GoVersion string `json:"go_version"`
		// Fixture is the SHA-256 of the evaluated snapshot.
		Fixture string `json:"fixture"`
		// Seconds is the operation's wall-clock duration.
		Seconds float64 `json:"seconds"`
		// Outcome is "ok" or the operation's error.
		Outcome string `json:"outcome"`
		// Detail is a short description of the returned result.
		Detail string `json:"detail"`
		// WorkDirs counts surla-designeval-work-* directories in TMPDIR
		// after the operation.
		WorkDirs int `json:"work_dirs"`
	}

	// scriptedAnalyzer is a scripted CP1 analyzer with no open questions, so
	// a proposed card is confirmed at once. It is not a model.
	scriptedAnalyzer struct{}
)

// operationTimeout bounds one operation, leaving room for designeval to stop
// its build process group before the test binary's own deadline.
const operationTimeout = 8 * time.Minute

// design is the agent design the diff tests use; its build includes goa-ai.
const design = `package design

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

// TestTimingProbeDirect measures, in one fresh process, a first Evaluate, a
// second Evaluate of the same snapshot, and a diff of that snapshot against
// one with an added method.
func TestTimingProbeDirect(t *testing.T) {
	files := testmodule.Snapshot(t, "", design)
	changed := testmodule.Snapshot(t, "", strings.Replace(design, `Method("approve_refund", func() {})`,
		"Method(\"approve_refund\", func() {})\n\tMethod(\"cancel_order\", func() {})", 1))
	for _, probe := range []string{"evaluate_first", "evaluate_second"} {
		run(t, probe, files, func(ctx context.Context) (string, error) {
			res, err := designeval.Evaluate(ctx, files)
			return fmt.Sprintf("system=%s declarations=%d tools=%d", res.SystemName, len(res.Declarations), len(res.Tools)), err
		})
	}
	run(t, "diff_third_and_fourth_evaluations", changed, func(ctx context.Context) (string, error) {
		rep, err := diff.Compare(ctx, files, changed)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("has_changes=%t added=%d", rep.HasChanges, len(rep.AddedDeclarations)), nil
	})
}

// TestTimingProbePreviewHTTP measures, in its own fresh process, two
// preview_architecture requests for a confirmed Change Card through the
// generated HTTP client that `surla preview-change` uses, each with the
// command's default 2-minute client deadline.
func TestTimingProbePreviewHTTP(t *testing.T) {
	files := testmodule.Snapshot(t, "", design)
	cp, err := controlplane.New(controlplane.Options{CardsDir: t.TempDir(), Analyzer: scriptedAnalyzer{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cp.Close(); err != nil {
			t.Error(err)
		}
	})
	alice := controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: "alice@example.com"})
	card, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "timing", Prompt: "Add order cancellation."})
	if err != nil || card.Status != controlplane.StatusConfirmed {
		t.Fatalf("confirmed card: %v %+v", err, card)
	}
	auth := controlplane.NewTokenAuthenticator(map[string]controlplane.Principal{"tok": {ID: "alice@example.com"}})
	srv := httptest.NewServer(controlplane.NewHTTPHandler(cp, auth))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	doer := &http.Client{Transport: bearer{token: "tok"}}
	endpoint := genhttpsurla.NewClient(u.Scheme, u.Host, doer, goahttp.RequestEncoder, goahttp.ResponseDecoder, false).PreviewArchitecture()
	snapshot := make(map[string]string, len(files))
	for k, v := range files {
		snapshot[k] = v
	}
	for _, probe := range []string{"preview_http_first", "preview_http_second"} {
		run(t, probe, files, func(ctx context.Context) (string, error) {
			reqCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			res, err := endpoint(reqCtx, &gensurla.PreviewArchitecturePayload{
				ChangeID: card.ChangeID, RequestRevision: card.RequestRevision,
				Design: &gensurla.DesignSnapshot{Files: snapshot},
			})
			if err != nil {
				return "", err
			}
			out := res.(*gensurla.ArchitecturePreviewResult)
			return fmt.Sprintf("system=%s containers=%d", out.SystemName, len(out.Containers)), nil
		})
	}
}

// Analyze returns a fixed analysis with no open questions.
func (scriptedAnalyzer) Analyze(context.Context, string, string) (controlplane.CP1SemanticAnalysis, error) {
	return controlplane.CP1SemanticAnalysis{
		Summary:            "Add order cancellation.",
		Examples:           []controlplane.CP1ModelExample{{Scenario: "Cancel", InputSummary: "Cancel order 1", ExpectedOutcome: "Cancelled", VerificationKind: "deterministic"}},
		PreservedBehaviors: []string{"Orders keep their limits."},
	}, nil
}

// Revise returns the same fixed analysis.
func (a scriptedAnalyzer) Revise(ctx context.Context, _ controlplane.CP1PriorCard, prompt, design string) (controlplane.CP1SemanticAnalysis, error) {
	return a.Analyze(ctx, prompt, design)
}

// bearer adds the bearer token, as `surla preview-change` does.
type bearer struct {
	// token is the bearer credential.
	token string
}

// RoundTrip adds the Authorization header.
func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// run times op under operationTimeout and prints its measurement. A failed
// operation fails the test after printing.
func run(t *testing.T, probe string, files map[string]string, op func(context.Context) (string, error)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), operationTimeout)
	defer cancel()
	start := time.Now()
	detail, err := op(ctx)
	m := measurement{
		Probe: probe, Variant: os.Getenv("TIMING_VARIANT"), Head: gitHead(t), EvalSource: evalSource(t), GoVersion: goVersion(t),
		Fixture: designeval.SnapshotDigest(files), Seconds: time.Since(start).Seconds(), Outcome: "ok",
		Detail: detail, WorkDirs: workDirs(t),
	}
	if err != nil {
		m.Outcome = err.Error()
	}
	line, jerr := json.Marshal(m)
	if jerr != nil {
		t.Fatal(jerr)
	}
	fmt.Println("TIMING " + string(line))
	if err != nil {
		t.Fatalf("%s: %v", probe, err)
	}
}

// gitHead returns the commit the probe runs on.
func gitHead(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown: " + err.Error()
	}
	return strings.TrimSpace(string(out))
}

// evalSource returns the git blob hash of the checked-out eval.go.
func evalSource(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "hash-object", "../designeval/eval.go").Output()
	if err != nil {
		return "unknown: " + err.Error()
	}
	return strings.TrimSpace(string(out))
}

// goVersion returns the toolchain `go` selects here.
func goVersion(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		return "unknown: " + err.Error()
	}
	return strings.TrimSpace(string(out))
}

// workDirs counts surla-designeval-work-* directories in TMPDIR.
func workDirs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return -1
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "surla-designeval-work-") {
			n++
		}
	}
	return n
}
