// This file serves the built views page and the real generated HTTP API to
// jsdom tests, which drive the page through the requests it actually sends.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	goahttp "goa.design/goa/v3/http"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/verify"
)

type (
	// pageServer stands in for the project server only in how the page gets
	// its session: pageLink exchanges once for pageSession. Every other
	// request reaches NewHTTPHandler with real generated codecs. Test-only
	// /__test endpoints let a script hold a request before or after the
	// server handles it, answer one with raw bytes, or run a named server-side
	// action, to observe busy states, interleavings and malformed answers.
	pageServer struct {
		// t reports answers the server could not write.
		t *testing.T
		// server listens on loopback; its URL is the page's origin.
		server *httptest.Server
		// mu guards usedLinks, gates, replies and actions.
		mu sync.Mutex
		// usedLinks records the links already exchanged; pageLink and
		// pageLink-<n> each exchange once, so a test can open several pages.
		usedLinks map[string]bool
		// gates hold requests by "METHOD /path" until released.
		gates map[string]*pageGate
		// replies answers the next requests by "METHOD /path" with raw bytes.
		replies map[string][]pageReply
		// actions are the server-side changes a script may ask for by name.
		actions map[string]func()
		// fixtures, when set, runs an action named "<verb> <arguments>" that
		// actions does not name; it reports whether it knew the verb.
		fixtures func(key string) bool
	}

	// pageGate holds matching requests until a script releases it.
	pageGate struct {
		// open closes on release.
		open chan struct{}
		// answered is true when the server handles the request first and
		// only its answer waits.
		answered bool
	}

	// pageReply is one raw answer a test queues for a request.
	pageReply struct {
		// Key is "METHOD /path" of the request to answer.
		Key string `json:"key"`
		// Status is the HTTP status written.
		Status int `json:"status"`
		// Body is the exact JSON body written.
		Body json.RawMessage `json:"body"`
	}
)

const (
	// pageOwner owns every card the page tests create.
	pageOwner = "alice@example.com"
	// pageLink is the one-use link the test page opens with.
	pageLink = "page-test-link"
	// pageSession is the session pageLink exchanges for.
	pageSession = "page-test-session"
	// pageUserID is the caller identity of the page's session.
	pageUserID = "user-page-test"
	// pageGuide is the guide's credential for changes made by another caller.
	pageGuide = "page-test-guide"
)

var checkpointUIDepsOnce sync.Once

// ensureCheckpointUIDeps installs ui/checkpoint npm dependencies once when
// running tests in a clean checkout where node_modules is not yet populated.
func ensureCheckpointUIDeps(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("../../ui/checkpoint/node_modules/jsdom"); err == nil {
		return
	}
	npmPath, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm not found in PATH and ui/checkpoint/node_modules is missing")
	}
	checkpointUIDepsOnce.Do(func() {
		cmd := exec.Command(npmPath, "ci", "--prefix", "../../ui/checkpoint", "--no-audit", "--no-fund")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("npm ci --prefix ../../ui/checkpoint: %v\n%s", err, string(out))
		}
	})
}

// startPageServer serves svc behind the page session for one test.
func startPageServer(t *testing.T, svc gensurla.Service) *pageServer {
	t.Helper()
	ps := &pageServer{t: t, usedLinks: map[string]bool{}, gates: map[string]*pageGate{}, replies: map[string][]pageReply{}, actions: map[string]func(){}}
	mux := http.NewServeMux()
	mux.Handle("GET /views", ViewsPage())
	mux.HandleFunc("POST /project/browser-session", ps.exchange)
	mux.HandleFunc("POST /__test/hold", ps.control(func(r pageReply) { ps.gates[r.Key] = &pageGate{open: make(chan struct{})} }))
	mux.HandleFunc("POST /__test/answer-later", ps.control(func(r pageReply) {
		ps.gates[r.Key] = &pageGate{open: make(chan struct{}), answered: true}
	}))
	mux.HandleFunc("POST /__test/release", ps.control(func(r pageReply) {
		if gate, ok := ps.gates[r.Key]; ok {
			close(gate.open)
			delete(ps.gates, r.Key)
		}
	}))
	mux.HandleFunc("POST /__test/reply", ps.control(func(r pageReply) { ps.replies[r.Key] = append(ps.replies[r.Key], r) }))
	mux.HandleFunc("POST /__test/act", ps.control(func(r pageReply) {
		action, ok := ps.actions[r.Key]
		switch {
		case ok:
			action()
		case ps.fixtures == nil || !ps.fixtures(r.Key):
			t.Errorf("no test action %q", r.Key)
		}
	}))
	mux.Handle("/", ps.intercept(NewHTTPHandler(svc, ps.authenticate)))
	ps.server = httptest.NewServer(mux)
	t.Cleanup(func() {
		ps.mu.Lock()
		for key, gate := range ps.gates {
			close(gate.open)
			delete(ps.gates, key)
		}
		ps.mu.Unlock()
		ps.server.Close()
	})
	return ps
}

// act registers a named server-side change a script runs with
// control(origin, "act", {key}).
func (ps *pageServer) act(name string, action func()) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.actions[name] = action
}

// pageControlPlane returns a control plane whose waiting read answers
// quickly, so tests observe several waits and changes within seconds.
func pageControlPlane(t *testing.T, opts Options) *ControlPlane {
	t.Helper()
	cp := newControlPlane(t, opts)
	cp.changesWait = 400 * time.Millisecond
	cp.changesPoll = 20 * time.Millisecond
	return cp
}

// runPageScript runs body, an async JavaScript function body, in node with
// harness (ui/checkpoint/testing/page_harness.cjs), origin, and input in
// scope. It fails the test with the script's output when the script throws.
func runPageScript(t *testing.T, ps *pageServer, input map[string]any, body string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to exercise the views page")
	}
	ensureCheckpointUIDeps(t)
	harness, err := filepath.Abs("../../ui/checkpoint/testing/page_harness.cjs")
	require.NoError(t, err)
	if input == nil {
		input = map[string]any{}
	}
	input["origin"] = ps.server.URL
	input["link"] = pageLink
	input["guide"] = pageGuide
	input["user"] = pageUserID
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	harnessLiteral, err := json.Marshal(harness)
	require.NoError(t, err)
	inputPath := filepath.Join(t.TempDir(), "page-input.json")
	require.NoError(t, os.WriteFile(inputPath, raw, 0o600))
	script := `
const fs = require("fs");
const harness = require(` + string(harnessLiteral) + `);
const { api, control, expect, openPage, sleep, until } = harness;
const input = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
const origin = input.origin;
async function main() {
` + body + `
}
main().then(() => { process.stdout.write("PAGE-PASSED"); process.exit(0); }, (err) => { console.error(err && err.stack || err); process.exit(1); });
`
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "-e", script, inputPath)
	cmd.Env = append(os.Environ(), "CI=true")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "views page script failed:\n%s", out)
	require.True(t, strings.HasSuffix(string(out), "PAGE-PASSED"), "views page script output:\n%s", out)
}

// authenticate accepts the page session only with the same-origin mark a
// browser adds, and the guide's credential from any program.
func (ps *pageServer) authenticate(r *http.Request) (Principal, error) {
	switch r.Header.Get("Authorization") {
	case "Bearer " + pageSession:
		if r.Header.Get("Sec-Fetch-Site") != "same-origin" {
			return Principal{}, errors.New("browser session requires a same-origin page request")
		}
		return Principal{ID: pageOwner, CallerID: pageUserID, Kind: CallerUser}, nil
	case "Bearer " + pageGuide:
		return Principal{ID: pageOwner, CallerID: "guide-page-test", Kind: CallerGuide}, nil
	}
	return Principal{}, errors.New("session or credential is no longer valid")
}

// exchange trades pageLink once for pageSession, answering like the project
// server's generated exchange operation.
func (ps *pageServer) exchange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Link string `json:"link"`
	}
	ps.mu.Lock()
	ok := json.NewDecoder(r.Body).Decode(&body) == nil &&
		(body.Link == pageLink || strings.HasPrefix(body.Link, pageLink+"-")) && !ps.usedLinks[body.Link]
	if ok {
		ps.usedLinks[body.Link] = true
	}
	ps.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		serviceErr := gensurla.MakeUnprocessable(errors.New("browser link is used or expired; open the project's views with surla"))
		w.WriteHeader(http.StatusUnprocessableEntity)
		if err := goahttp.ResponseEncoder(r.Context(), w).Encode(goahttp.NewErrorResponse(r.Context(), serviceErr)); err != nil {
			ps.t.Logf("write exchange refusal: %v", err)
		}
		return
	}
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(map[string]string{"session": pageSession}); err != nil {
		ps.t.Logf("write exchange answer: %v", err)
	}
}

// control decodes one test control request and applies it under ps.mu.
func (ps *pageServer) control(apply func(pageReply)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var reply pageReply
		if err := json.NewDecoder(r.Body).Decode(&reply); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ps.mu.Lock()
		apply(reply)
		ps.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
}

// intercept holds or answers a request when a test asked for it, and
// otherwise passes it to the real handler.
func (ps *pageServer) intercept(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		ps.mu.Lock()
		gate := ps.gates[key]
		ps.mu.Unlock()
		if gate != nil && gate.answered {
			// The server does the work now; only its answer waits.
			recorded := httptest.NewRecorder()
			next.ServeHTTP(recorded, r)
			select {
			case <-gate.open:
			case <-r.Context().Done():
				return
			}
			for name, values := range recorded.Header() {
				w.Header()[name] = values
			}
			w.WriteHeader(recorded.Code)
			if _, err := w.Write(recorded.Body.Bytes()); err != nil {
				ps.t.Logf("write delayed answer for %s: %v", key, err)
			}
			return
		}
		if gate != nil {
			select {
			case <-gate.open:
			case <-r.Context().Done():
				return
			}
		}
		ps.mu.Lock()
		queued := ps.replies[key]
		var reply *pageReply
		if len(queued) > 0 {
			reply = &queued[0]
			ps.replies[key] = queued[1:]
		}
		ps.mu.Unlock()
		if reply != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(reply.Status)
			if _, err := w.Write(reply.Body); err != nil {
				ps.t.Logf("write queued reply for %s: %v", key, err)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

type (
	// pageVerifications serves verification runs from fixtures through the
	// real generated encoders, standing in for runs a builder started. Every
	// other operation reaches the embedded control plane.
	pageVerifications struct {
		*ControlPlane
		// mu guards runs and lists, which test actions change while the
		// server answers.
		mu sync.Mutex
		// runs holds each run's current snapshot by run_id.
		runs map[string]*gensurla.VerificationStatusResult
		// lists holds each change's run_ids, newest first.
		lists map[string][]string
	}

	// pageTarget is a promotion target whose inputs a test action changes
	// while the server answers; absent makes the project have none.
	pageTarget struct {
		// mu guards every field.
		mu sync.Mutex
		// report is the report LoadReport returns.
		report verify.Report
		// inputs is what CurrentInputs returns.
		inputs PromotionInputs
		// inputsErr, when set, is what CurrentInputs fails with.
		inputsErr error
		// absent makes resolve report no promotion target.
		absent bool
	}
)

// newPageVerifications wraps cp with an empty run fixture set.
func newPageVerifications(cp *ControlPlane) *pageVerifications {
	return &pageVerifications{ControlPlane: cp, runs: map[string]*gensurla.VerificationStatusResult{}, lists: map[string][]string{}}
}

// set stores each run's snapshot, replacing an earlier one with its run_id.
func (s *pageVerifications) set(runs ...*gensurla.VerificationStatusResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, run := range runs {
		s.runs[run.RunID] = run
	}
}

// list makes changeID's saved runs exactly runIDs, newest first.
func (s *pageVerifications) list(changeID string, runIDs ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists[changeID] = runIDs
}

// GetVerification returns the run's current fixture snapshot.
func (s *pageVerifications) GetVerification(
	_ context.Context,
	p *gensurla.GetVerificationPayload,
) (*gensurla.VerificationStatusResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[p.RunID]
	if !ok {
		return nil, gensurla.MakeNotFound(errors.New("unknown run_id"))
	}
	return run, nil
}

// ListVerifications returns the change's fixture runs, newest first.
func (s *pageVerifications) ListVerifications(
	_ context.Context,
	p *gensurla.ListVerificationsPayload,
) (*gensurla.VerificationListResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runs := make([]*gensurla.VerificationStatusResult, 0, len(s.lists[p.ChangeID]))
	for _, id := range s.lists[p.ChangeID] {
		runs = append(runs, s.runs[id])
	}
	return &gensurla.VerificationListResult{ChangeID: p.ChangeID, Runs: runs}, nil
}

// LoadReport returns the report the test made.
func (p *pageTarget) LoadReport(context.Context, string) (verify.Report, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.report, nil
}

// CurrentInputs returns the inputs, or the failure, a test action chose.
func (p *pageTarget) CurrentInputs(context.Context, string) (PromotionInputs, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inputs, p.inputsErr
}

// resolve is Options.ResolvePromotionTarget: the target, or none while absent.
func (p *pageTarget) resolve(context.Context, string) (PromotionTarget, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.absent {
		return nil, nil
	}
	return p, nil
}

// update changes the target under its lock.
func (p *pageTarget) update(change func(*pageTarget)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	change(p)
}

// passedRun returns a completed, passed run of card whose report digest is
// digest, as a builder's verification would report it.
func passedRun(card *gensurla.ChangeCardResult, runID, commit, digest string) *gensurla.VerificationStatusResult {
	return &gensurla.VerificationStatusResult{
		RunID: runID, ChangeID: card.ChangeID, RequestRevision: card.RequestRevision,
		CandidateCommit: commit, SuiteCommit: strings.Repeat("2", 40), AcceptedAt: "2026-10-31T12:34:56.123Z",
		State: gensurla.NewStateCompleted(&gensurla.VerificationCompleted{
			SuiteID: "orders_acceptance", Environment: "sim",
			Report: &gensurla.BuilderVerificationReport{
				ReportDigest: digest, Passed: true,
				Counts: &gensurla.VerificationCounts{Required: 2, Passed: 2},
				Scenarios: []*gensurla.VerificationScenario{
					{ID: "a", Outcome: "passed", Checks: []*gensurla.VerificationCheck{{Name: "check", Passed: true}}},
					{ID: "holdout-1", Outcome: "passed"},
				},
			},
		}),
	}
}
