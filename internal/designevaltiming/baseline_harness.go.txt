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
	"regexp"
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
		// Server is the server's own outcome for a preview request, captured
		// from the service method before the generated HTTP error mapping.
		Server *serverOutcome `json:"server,omitempty"`
		// Retained lists per-evaluation directories still present after the
		// operation; they mean build processes may still be running.
		Retained []string `json:"retained,omitempty"`
		// TempEntries lists TempDir's entries after the operation and any
		// server request it started have stopped.
		TempEntries []string `json:"temp_entries"`
	}

	// serverOutcome is what the server's PreviewArchitecture returned.
	serverOutcome struct {
		// Error is the returned error's text, empty on success.
		Error string `json:"error,omitempty"`
		// Cancelled reports that the returned error wraps the request
		// context's own cancellation or deadline error.
		Cancelled bool `json:"cancelled"`
		// ContextErr is the request context's error when the method
		// returned, empty while it was still live.
		ContextErr string `json:"context_err,omitempty"`
		// err is the returned error.
		err error
	}

	// observedService is test-only composition: it records each
	// PreviewArchitecture outcome before the generated HTTP server maps
	// errors to statuses, and passes every call through unchanged.
	observedService struct {
		gensurla.Service
		// mu guards outcomes.
		mu sync.Mutex
		// outcomes holds one entry per finished PreviewArchitecture call.
		outcomes []serverOutcome
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

// maximum20 finds a JSON schema maximum of 20.
var maximum20 = regexp.MustCompile(`"maximum":\s*20\b`)

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
// one with an added cancel_order method. Only the Evaluate or Compare call is
// timed. Any failure, a result that does not match the fixture, or an
// evaluation directory left behind is fatal.
func TestTimingProbeDirect(t *testing.T) {
	id := setup(t)
	files, changed := fixtures(t)
	for _, probe := range []string{"evaluate_first", "evaluate_second"} {
		var res designeval.Result
		m := measure(t, id, probe, []map[string]string{files}, func(ctx context.Context) error {
			var err error
			res, err = designeval.Evaluate(ctx, files)
			return err
		})
		requireSafeCleanup(t, &m)
		if m.Outcome == "ok" {
			m.Result = evaluationFacts(t, res)
		}
		emit(t, m)
		if m.Outcome != "ok" {
			t.Fatalf("%s: %s", probe, m.Outcome)
		}
		requireFixture(t, probe, res.Declarations, res.AgentPolicies, res.Tools)
	}
	var rep diff.Report
	m := measure(t, id, "diff_third_and_fourth_evaluations", []map[string]string{files, changed}, func(ctx context.Context) error {
		var err error
		rep, err = diff.Compare(ctx, files, changed)
		return err
	})
	requireSafeCleanup(t, &m)
	if m.Outcome == "ok" {
		m.Result = resultFacts{Digest: digestJSON(t, rep), Facts: append([]string{fmt.Sprintf("has_changes=%t", rep.HasChanges)}, prefixed("added=", rep.AddedDeclarations)...)}
	}
	emit(t, m)
	if m.Outcome != "ok" {
		t.Fatalf("diff: %s", m.Outcome)
	}
	if !rep.HasChanges || !slices.Contains(rep.AddedDeclarations, "Service:orders/Method:cancel_order") {
		t.Fatalf("diff: want the added cancel_order method, got %+v", rep)
	}
}

// TestTimingProbePreviewHTTP measures, in its own fresh process, two
// preview_architecture requests for a confirmed Change Card through the
// generated, authenticated HTTP client with the 2-minute budget `surla
// preview-change` uses by default. It exercises that client and the server,
// not the CLI's file collection, flags, signals or output. Only the endpoint
// call is timed. Reaching the 2-minute deadline is a studied outcome,
// recorded as client_deadline. The server's own outcome is then awaited and
// recorded from the service method. The probe continues only when that
// outcome is a success or wraps the request's cancellation, and no
// evaluation directory remains. Anything else, including a server error that
// cannot be shown to be the cancellation, stops the experiment.
func TestTimingProbePreviewHTTP(t *testing.T) {
	id := setup(t)
	files, _ := fixtures(t)
	cp, err := controlplane.New(controlplane.Options{CardsDir: t.TempDir(), Analyzer: scriptedAnalyzer{}})
	if err != nil {
		t.Fatal(err)
	}
	// closeCP runs once, whether the test reaches its explicit stop or a
	// fatal path leaves it to t.Cleanup.
	closeCP := sync.OnceValue(cp.Close)
	t.Cleanup(func() {
		if err := closeCP(); err != nil {
			t.Error(err)
		}
	})
	alice := controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: "alice@example.com"})
	card, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "timing", Prompt: "Add order cancellation."})
	if err != nil || card.Status != controlplane.StatusConfirmed {
		t.Fatalf("confirmed card: %v %+v", err, card)
	}
	auth := controlplane.NewTokenAuthenticator(map[string]controlplane.Principal{"tok": {ID: "alice@example.com"}})
	svc := &observedService{Service: cp}
	handler := controlplane.NewHTTPHandler(svc, auth)
	var inflight sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Done()
		handler.ServeHTTP(w, r)
	}))
	// httptest.Server.Close waits for active handlers, and ControlPlane.Close
	// does not cancel a running preview, so closing is bounded: a server that
	// cannot close is reported, with what TMPDIR holds, as unresolved.
	t.Cleanup(func() { closeServer(t, id, srv) })
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := genhttpsurla.NewClient(u.Scheme, u.Host, &http.Client{Transport: bearer{token: "tok"}},
		goahttp.RequestEncoder, goahttp.ResponseDecoder, false).PreviewArchitecture()
	for i, probe := range []string{"preview_http_first", "preview_http_second"} {
		var res any
		m := measure(t, id, probe, []map[string]string{files}, func(ctx context.Context) error {
			reqCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			var err error
			res, err = endpoint(reqCtx, &gensurla.PreviewArchitecturePayload{
				ChangeID: card.ChangeID, RequestRevision: card.RequestRevision,
				Design: &gensurla.DesignSnapshot{Files: files},
			})
			if err != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return errClientDeadline
			}
			return err
		})
		// The server keeps evaluating after the client gives up; its own
		// outcome must be known before anything else is measured.
		if !waitBounded(&inflight, operationTimeout) {
			// Keep the client measurement. The handler may still be changing
			// files, so list only top-level names, and don't claim it stopped.
			m.TempEntries = entryNames(id.TempDir)
			emit(t, m)
			t.Fatalf("%s: the server request did not finish within %s; its outcome and build processes are unresolved", probe, operationTimeout)
		}
		server, ok := svc.outcome(i)
		if !ok {
			emit(t, m)
			t.Fatalf("%s: the server recorded no outcome for request %d", probe, i+1)
		}
		m.Server = &server
		requireSafeCleanup(t, &m)
		if m.Outcome == "ok" {
			out := res.(*gensurla.ArchitecturePreviewResult)
			m.Result = resultFacts{Digest: digestJSON(t, out), Facts: []string{
				"system=" + out.SystemName, "design_digest=" + out.DesignDigest, "banner=" + out.Banner,
				fmt.Sprintf("containers=%d edges=%d policies=%d tools=%d", len(out.Containers), len(out.Edges), len(out.AgentPolicies), len(out.Tools)),
			}}
		}
		emit(t, m)
		if m.Outcome != "ok" && m.Outcome != "client_deadline" {
			t.Fatalf("%s: %s", probe, m.Outcome)
		}
		// A client success needs a server success. After a client deadline,
		// the server may have finished anyway or returned the request's
		// cancellation; any other server error is an independent failure or
		// cannot be told apart from one, so the experiment stops.
		if m.Outcome == "ok" && server.err != nil {
			t.Fatalf("%s: the client succeeded but the server returned %v", probe, server.err)
		}
		if m.Outcome == "client_deadline" && server.err != nil && !server.Cancelled {
			t.Fatalf("%s: after the client deadline the server returned an error that does not wrap the request's cancellation (%v); its outcome is unresolved", probe, server.err)
		}
		if m.Outcome == "ok" {
			out := res.(*gensurla.ArchitecturePreviewResult)
			requirePreviewFixture(t, probe, out)
		}
	}
	closeServer(t, id, srv)
	if err := closeCP(); err != nil {
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

// measure times only op, under operationTimeout. The caller lists the
// probe's TMPDIR and computes result evidence once the operation, and any
// server request it started, have finished.
func measure(t *testing.T, id identity, probe string, inputs []map[string]string, op func(context.Context) error) measurement {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), operationTimeout)
	defer cancel()
	start := time.Now()
	err := op(ctx)
	elapsed := time.Since(start).Seconds()
	m := measurement{identity: id, Probe: probe, Seconds: elapsed, Outcome: "ok"}
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

// serverCloseTimeout bounds closing the test server.
const serverCloseTimeout = time.Minute

// closeServer closes srv within serverCloseTimeout. When it cannot, a handler
// is still running, so its evaluation's build processes are unresolved: it
// prints server_close_unresolved with the TMPDIR listing and fails the test,
// which stops the experiment. It never claims those processes stopped.
func closeServer(t *testing.T, id identity, srv *httptest.Server) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		srv.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(serverCloseTimeout):
		emit(t, measurement{identity: id, Probe: "server_close_unresolved",
			Outcome: "a preview handler is still running; its build processes are unresolved", TempEntries: entryNames(id.TempDir)})
		t.Errorf("the test server did not close within %s", serverCloseTimeout)
	}
}

// requireFixture fails the test unless an evaluation produced the fixture's
// concrete content: the orders service with get_order (limit maximum 20) and
// approve_refund, the refund_agent policy of 6 tool calls in 120 seconds,
// and the issue_refund tool.
func requireFixture(t *testing.T, probe string, decls map[string]string, policies []*gensurla.AgentPolicyItem, tools []*gensurla.ToolContractItem) {
	t.Helper()
	for _, key := range []string{"Service:orders", "Service:orders/Method:get_order", "Service:orders/Method:approve_refund", "Service:orders/Agent:refund_agent"} {
		if _, ok := decls[key]; !ok {
			t.Fatalf("%s: declaration %s is missing", probe, key)
		}
	}
	if !maximum20.MatchString(decls["Service:orders/Method:get_order"]) {
		t.Fatalf("%s: get_order's payload has no limit maximum of 20: %s", probe, decls["Service:orders/Method:get_order"])
	}
	requireAgentAndTool(t, probe, policies, tools)
}

// requirePreviewFixture checks a preview's policies and tools.
func requirePreviewFixture(t *testing.T, probe string, out *gensurla.ArchitecturePreviewResult) {
	t.Helper()
	requireAgentAndTool(t, probe, out.AgentPolicies, out.Tools)
}

// requireAgentAndTool checks the refund_agent policy and issue_refund tool.
func requireAgentAndTool(t *testing.T, probe string, policies []*gensurla.AgentPolicyItem, tools []*gensurla.ToolContractItem) {
	t.Helper()
	policyOK := slices.ContainsFunc(policies, func(p *gensurla.AgentPolicyItem) bool {
		return strings.Contains(p.AgentID, "refund_agent") && p.MaxToolCalls == 6 && p.MaxExecutionSeconds == 120
	})
	if !policyOK {
		t.Fatalf("%s: no refund_agent policy with 6 tool calls in 120 seconds: %+v", probe, policies)
	}
	if !slices.ContainsFunc(tools, func(c *gensurla.ToolContractItem) bool { return c.Name == "issue_refund" }) {
		t.Fatalf("%s: the issue_refund tool is missing", probe)
	}
}

// requireSafeCleanup records the probe's TMPDIR in m once the operation, and
// any server request it started, have finished. It first looks for retained
// per-evaluation directories (surla-designeval-<n>), which Evaluate keeps
// only when its build processes may still be running. If any remain, it
// emits m with only top-level names, since those processes may still be
// changing files, and stops the experiment. Otherwise it records every
// entry with its size. The baseline's process-wide surla-designeval-work-<n>
// is expected and only listed.
func requireSafeCleanup(t *testing.T, m *measurement) {
	t.Helper()
	names := entryNames(m.TempDir)
	for _, name := range names {
		if strings.HasPrefix(name, "surla-designeval-") && !strings.HasPrefix(name, "surla-designeval-work-") {
			m.Retained = append(m.Retained, name)
		}
	}
	if len(m.Retained) > 0 {
		m.TempEntries = names
		emit(t, *m)
		t.Fatalf("%s: evaluation directories %v were kept; their build processes are unresolved, so the experiment stops", m.Probe, m.Retained)
	}
	m.TempEntries = tempEntries(t, m.TempDir)
}

// prefixed returns each value with prefix.
func prefixed(prefix string, values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, prefix+v)
	}
	return out
}

// PreviewArchitecture calls the control plane and records its outcome.
func (s *observedService) PreviewArchitecture(ctx context.Context, p *gensurla.PreviewArchitecturePayload) (*gensurla.ArchitecturePreviewResult, error) {
	res, err := s.Service.PreviewArchitecture(ctx, p)
	out := serverOutcome{err: err}
	if err != nil {
		out.Error = err.Error()
		out.Cancelled = errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	}
	if cerr := ctx.Err(); cerr != nil {
		out.ContextErr = cerr.Error()
	}
	s.mu.Lock()
	s.outcomes = append(s.outcomes, out)
	s.mu.Unlock()
	return res, err
}

// outcome returns the i-th recorded outcome.
func (s *observedService) outcome(i int) (serverOutcome, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.outcomes) {
		return serverOutcome{}, false
	}
	return s.outcomes[i], true
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

// entryNames lists dir's top-level names without sizes, for use while a
// handler may still be removing files; a read error becomes the only entry.
func entryNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{"list error: " + err.Error()}
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
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
