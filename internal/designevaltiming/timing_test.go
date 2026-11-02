//go:build timing

// Package designevaltiming measures how long design evaluation takes with the
// designeval workspace as it is built in this tree. It is an experiment, not
// a regular test: it builds only with the timing tag, and each test is meant
// to run in its own fresh `go test -v` process whose TMPDIR is a directory
// owned by that one probe, so the evaluator's build cache starts empty and
// every directory the probe leaves can be attributed to it. Each measurement
// prints one "TIMING {json}" line.
package designevaltiming

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	// identity is what every measurement of one probe process shares: the
	// exact source, toolchain and probe-owned temporary directory.
	identity struct {
		// Variant is $TIMING_VARIANT, such as baseline or hypothesis0.
		Variant string `json:"variant"`
		// Head is the checked-out commit.
		Head string `json:"head"`
		// EvalSource is the git blob hash of internal/designeval/eval.go as
		// checked out, which tells the variants apart on one commit.
		EvalSource string `json:"eval_source"`
		// GoVersion is the toolchain `go` selects here.
		GoVersion string `json:"go_version"`
		// TempDir is the probe-owned TMPDIR.
		TempDir string `json:"temp_dir"`
	}

	// measurement is one printed result.
	measurement struct {
		identity
		// Probe names the operation and its order in the process.
		Probe string `json:"probe"`
		// Inputs are the SHA-256 snapshot digests the operation read.
		Inputs []string `json:"inputs"`
		// Seconds is the operation's wall-clock duration, taken as soon as
		// it returned.
		Seconds float64 `json:"seconds"`
		// Outcome is "ok", "client_deadline" for the studied 2-minute
		// preview deadline, or the operation's error.
		Outcome string `json:"outcome"`
		// Result identifies the returned result: a SHA-256 of its
		// structure, and the concrete facts the fixture should produce.
		Result resultFacts `json:"result"`
		// TempEntries lists TempDir's entries after the operation and any
		// server request it started have stopped.
		TempEntries []string `json:"temp_entries"`
	}

	// resultFacts identifies a returned result.
	resultFacts struct {
		// Digest is a SHA-256 of the result's canonical JSON.
		Digest string `json:"digest,omitempty"`
		// Facts are concrete, readable checks of the fixture's content.
		Facts []string `json:"facts,omitempty"`
	}

	// scriptedAnalyzer is a scripted CP1 analyzer with no open questions, so
	// a proposed card is confirmed at once. It is not a model.
	scriptedAnalyzer struct{}

	// bearer adds the bearer token to every request.
	bearer struct {
		// token is the bearer credential.
		token string
	}
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
// one with an added cancel_order method. Any failure is fatal.
func TestTimingProbeDirect(t *testing.T) {
	id := setup(t)
	files, changed := fixtures(t)
	for _, probe := range []string{"evaluate_first", "evaluate_second"} {
		m := measure(t, id, probe, []map[string]string{files}, func(ctx context.Context) (resultFacts, error) {
			res, err := designeval.Evaluate(ctx, files)
			if err != nil {
				return resultFacts{}, err
			}
			return evaluationFacts(t, res), nil
		})
		emit(t, m)
		if m.Outcome != "ok" {
			t.Fatalf("%s: %s", probe, m.Outcome)
		}
	}
	m := measure(t, id, "diff_third_and_fourth_evaluations", []map[string]string{files, changed}, func(ctx context.Context) (resultFacts, error) {
		rep, err := diff.Compare(ctx, files, changed)
		if err != nil {
			return resultFacts{}, err
		}
		facts := resultFacts{Digest: digestJSON(t, rep), Facts: []string{fmt.Sprintf("has_changes=%t", rep.HasChanges)}}
		for _, d := range rep.AddedDeclarations {
			facts.Facts = append(facts.Facts, "added="+d)
		}
		return facts, nil
	})
	emit(t, m)
	if m.Outcome != "ok" {
		t.Fatalf("diff: %s", m.Outcome)
	}
}

// TestTimingProbePreviewHTTP measures, in its own fresh process, two
// preview_architecture requests for a confirmed Change Card through the
// generated, authenticated HTTP client with the 2-minute budget `surla
// preview-change` uses by default. It exercises that client and the server,
// not the CLI's file collection, flags, signals or output. Reaching the
// 2-minute deadline is a studied outcome, recorded as client_deadline; the
// probe then waits for the server's request to finish before measuring
// again. Any other failure is fatal.
func TestTimingProbePreviewHTTP(t *testing.T) {
	id := setup(t)
	files, _ := fixtures(t)
	cp, err := controlplane.New(controlplane.Options{CardsDir: t.TempDir(), Analyzer: scriptedAnalyzer{}})
	if err != nil {
		t.Fatal(err)
	}
	alice := controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: "alice@example.com"})
	card, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "timing", Prompt: "Add order cancellation."})
	if err != nil || card.Status != controlplane.StatusConfirmed {
		t.Fatalf("confirmed card: %v %+v", err, card)
	}
	auth := controlplane.NewTokenAuthenticator(map[string]controlplane.Principal{"tok": {ID: "alice@example.com"}})
	handler := controlplane.NewHTTPHandler(cp, auth)
	var inflight sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Done()
		handler.ServeHTTP(w, r)
	}))
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := genhttpsurla.NewClient(u.Scheme, u.Host, &http.Client{Transport: bearer{token: "tok"}},
		goahttp.RequestEncoder, goahttp.ResponseDecoder, false).PreviewArchitecture()
	for _, probe := range []string{"preview_http_first", "preview_http_second"} {
		m := measure(t, id, probe, []map[string]string{files}, func(ctx context.Context) (resultFacts, error) {
			reqCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			res, err := endpoint(reqCtx, &gensurla.PreviewArchitecturePayload{
				ChangeID: card.ChangeID, RequestRevision: card.RequestRevision,
				Design: &gensurla.DesignSnapshot{Files: files},
			})
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
					return resultFacts{}, errClientDeadline
				}
				return resultFacts{}, err
			}
			out := res.(*gensurla.ArchitecturePreviewResult)
			return previewFacts(t, out), nil
		}, func() {
			// The server keeps evaluating after the client gives up; wait for
			// it, bounded, before listing files or measuring again.
			if !waitBounded(&inflight, operationTimeout) {
				t.Fatalf("%s: the server request did not finish within %s", probe, operationTimeout)
			}
		})
		emit(t, m)
		if m.Outcome != "ok" && m.Outcome != "client_deadline" {
			t.Fatalf("%s: %s", probe, m.Outcome)
		}
	}
	srv.Close()
	if err := cp.Close(); err != nil {
		t.Fatal(err)
	}
	emit(t, measurement{identity: id, Probe: "after_server_stopped", Outcome: "ok", TempEntries: tempEntries(t, id.TempDir)})
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

// RoundTrip adds the Authorization header.
func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// errClientDeadline marks the studied 2-minute client deadline.
var errClientDeadline = errors.New("client_deadline")

// setup collects the probe's identity. It requires TMPDIR to be an empty,
// probe-owned directory and fails the test when any identity fact is
// missing.
func setup(t *testing.T) identity {
	t.Helper()
	dir := os.Getenv("TMPDIR")
	if dir == "" {
		t.Fatal("TMPDIR must name a directory owned by this probe")
	}
	if got := tempEntries(t, dir); len(got) != 0 {
		t.Fatalf("probe TMPDIR %s is not empty: %v", dir, got)
	}
	variant := os.Getenv("TIMING_VARIANT")
	if variant == "" {
		t.Fatal("TIMING_VARIANT is required")
	}
	return identity{
		Variant:    variant,
		Head:       command(t, "git", "rev-parse", "HEAD"),
		EvalSource: command(t, "git", "hash-object", "../designeval/eval.go"),
		GoVersion:  command(t, "go", "env", "GOVERSION"),
		TempDir:    dir,
	}
}

// fixtures returns the design snapshot and the same design with an added
// cancel_order method.
func fixtures(t *testing.T) (files, changed map[string]string) {
	t.Helper()
	changed = testmodule.Snapshot(t, "", strings.Replace(design, `Method("approve_refund", func() {})`,
		"Method(\"approve_refund\", func() {})\n\tMethod(\"cancel_order\", func() {})", 1))
	return testmodule.Snapshot(t, "", design), changed
}

// measure times op under operationTimeout, taking the elapsed time as soon as
// op returns, then runs after, then lists the probe's TMPDIR.
func measure(t *testing.T, id identity, probe string, inputs []map[string]string, op func(context.Context) (resultFacts, error), after ...func()) measurement {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), operationTimeout)
	defer cancel()
	start := time.Now()
	facts, err := op(ctx)
	elapsed := time.Since(start).Seconds()
	for _, f := range after {
		f()
	}
	m := measurement{identity: id, Probe: probe, Seconds: elapsed, Outcome: "ok", Result: facts, TempEntries: tempEntries(t, id.TempDir)}
	for _, in := range inputs {
		m.Inputs = append(m.Inputs, designeval.SnapshotDigest(in))
	}
	switch {
	case errors.Is(err, errClientDeadline):
		m.Outcome = "client_deadline"
	case err != nil:
		m.Outcome = err.Error()
	}
	return m
}

// evaluationFacts identifies an evaluation result and checks the fixture's
// concrete content.
func evaluationFacts(t *testing.T, res designeval.Result) resultFacts {
	t.Helper()
	keys := make([]string, 0, len(res.Declarations))
	for k := range res.Declarations {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	facts := []string{
		"system=" + res.SystemName,
		fmt.Sprintf("declarations=%d containers=%d edges=%d policies=%d tools=%d",
			len(res.Declarations), len(res.Containers), len(res.Edges), len(res.AgentPolicies), len(res.Tools)),
	}
	for _, k := range keys {
		if strings.Contains(k, "get_order") || strings.Contains(k, "approve_refund") || strings.Contains(k, "cancel_order") {
			facts = append(facts, "declaration="+k)
		}
	}
	return resultFacts{Digest: digestJSON(t, res), Facts: facts}
}

// previewFacts identifies a preview result's evaluated architecture.
func previewFacts(t *testing.T, out *gensurla.ArchitecturePreviewResult) resultFacts {
	t.Helper()
	arch := struct {
		SystemName    string
		Containers    any
		Edges         any
		AgentPolicies any
		Tools         any
	}{out.SystemName, out.Containers, out.Edges, out.AgentPolicies, out.Tools}
	return resultFacts{Digest: digestJSON(t, arch), Facts: []string{
		"system=" + out.SystemName, "design_digest=" + out.DesignDigest,
		fmt.Sprintf("containers=%d edges=%d policies=%d tools=%d", len(out.Containers), len(out.Edges), len(out.AgentPolicies), len(out.Tools)),
	}}
}

// digestJSON returns the SHA-256 of v's JSON encoding, which sorts map keys.
func digestJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// emit writes one TIMING line.
func emit(t *testing.T, m measurement) {
	t.Helper()
	line, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("TIMING " + string(line))
}

// tempEntries lists dir's top-level entries with their sizes. Failing to
// read the probe's own directory is fatal.
func tempEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("list probe TMPDIR: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		size, err := treeSize(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("size %s: %v", e.Name(), err)
		}
		out = append(out, fmt.Sprintf("%s=%dB", e.Name(), size))
	}
	return out
}

// treeSize sums the sizes of the regular files under path.
func treeSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// command runs a short identity command and fails the test when it fails or
// prints nothing.
func command(t *testing.T, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	value := strings.TrimSpace(string(out))
	if err != nil || value == "" {
		t.Fatalf("%s %v: %q %v", name, args, value, err)
	}
	return value
}

// waitBounded waits for wg up to limit and reports whether it finished.
func waitBounded(wg *sync.WaitGroup, limit time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(limit):
		return false
	}
}
