// This file tests the code-authored OpenTelemetry span summary
// (`surla.public_summary`) that `propose_change` and `revise_change` record for
// operators: its exact text on success, refused analyses, conflicts, missing
// or foreign cards and storage faults, error status only for real faults, and
// that no span carries the caller's request content, bearer token or host
// paths. Concurrent cases hold one request at its span start, before it
// checks and saves the card, so another request can change the card.
package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	gensurla "github.com/goadesign/surla/gen/surla"
)

type (
	// spanHold is a span processor that blocks the next span with a given
	// name in OnStart, once armed, until the test releases it.
	// propose_change starts its span before it checks and saves the card, and
	// revise_change after its first read of the card and before it checks it
	// again and saves, so a held request lets the test change the card, or
	// close the control plane, in between.
	spanHold struct {
		// name is the span name to hold.
		name string
		// armed is true while the next matching span should be held.
		armed atomic.Bool
		// started is closed when a span is held.
		started chan struct{}
		// release lets the held span start.
		release chan struct{}
	}
)

// TestCP1SpanSummariesAreCodeAuthoredAndExcludeRequestContent drives
// propose_change and revise_change through success, a refused analysis, a
// duplicate change_id, and storage read and save faults, and checks each
// span's surla.public_summary text, its status (error only for storage
// faults), and that no span attribute or event, and no saved diagnostic,
// contains the caller's prompt, analysis text, ui_composition, design source,
// bearer token or host paths. Exported exception events of storage faults are
// the operator's full error evidence and may name the cards directory.
func TestCP1SpanSummariesAreCodeAuthoredAndExcludeRequestContent(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.Background())) })
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	wsDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(wsDir, "design"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, "design", "design.go"), []byte("package design // private-design-source-secret\n"), 0o644))
	cardsDir := t.TempDir()
	cp := newControlPlane(t, Options{CardsDir: cardsDir, WorkspaceDir: wsDir, TracerProvider: tp})

	program := "root = DecisionWorkspace(\"private-ui-program-secret\", [])"
	secretAnalysis := func() *gensurla.CP1Analysis {
		return &gensurla.CP1Analysis{
			Summary: "private-analysis-summary-secret",
			Ambiguities: []*gensurla.CP1AnalysisAmbiguity{{
				Question: "private-question-secret", Options: []string{"private-option-a-secret", "private-option-b-secret"},
			}},
			Examples: []*gensurla.CP1AnalysisExample{{
				Scenario: "private-scenario-secret", InputSummary: "private-input-secret",
				ExpectedOutcome: "private-outcome-secret", VerificationKind: "deterministic",
			}},
			PreservedBehaviors: []string{"private-preserved-secret"},
			UIComposition:      &program,
		}
	}
	secretRevision := func(priorID string) *gensurla.CP1RevisionAnalysis {
		return &gensurla.CP1RevisionAnalysis{
			Summary:     "private-analysis-summary-secret",
			Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
			Examples: []*gensurla.CP1RevisionAnalysisExample{{
				PriorID: &priorID, Scenario: "private-scenario-secret", InputSummary: "private-input-secret",
				ExpectedOutcome: "private-outcome-secret", VerificationKind: "deterministic",
			}},
			RemovedExampleIds:  []string{},
			PreservedBehaviors: []string{"private-preserved-secret"},
			UIComposition:      &program,
		}
	}
	propose := func(changeID string, analysis *gensurla.CP1Analysis) (*gensurla.ChangeCardResult, error) {
		return cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: changeID, Prompt: "private-prompt-secret", Analysis: analysis})
	}
	revise := func(changeID, rev string, analysis *gensurla.CP1RevisionAnalysis) (*gensurla.ChangeCardResult, error) {
		return cp.ReviseChange(alice, &gensurla.ReviseChangePayload{ChangeID: changeID, RequestRevision: rev, Prompt: "private-prompt-secret", Analysis: analysis})
	}

	// Success over HTTP, so the bearer token reaches the server.
	srv := httptest.NewServer(NewHTTPHandler(cp, NewTokenAuthenticator(map[string]Principal{"private-bearer-token-secret": {ID: "alice@example.com"}})))
	t.Cleanup(srv.Close)
	status, body := postJSON(t, srv.URL+"/changes/propose", "private-bearer-token-secret", string(proposeArgs(t, &gensurla.ProposeChangePayload{
		ChangeID: "chg", Prompt: "private-prompt-secret", Analysis: secretAnalysis(),
	})))
	require.Equal(t, http.StatusOK, status, body)
	assertSummary(t, exp, "surla.controlplane.propose_change", "Proposed Change Card (needs_confirmation).", codes.Unset)
	card, err := cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)

	// A repeated option breaks a proposal rule: refused, not a server failure.
	duplicate := secretAnalysis()
	duplicate.Ambiguities[0].Options[1] = duplicate.Ambiguities[0].Options[0]
	_, err = propose("chg-dup", duplicate)
	assertServiceError(t, err, "unprocessable")
	assertSummary(t, exp, "surla.controlplane.propose_change",
		"Change Card analysis broke a proposal rule (this request did not save a Change Card); correct the analysis and retry propose_change.", codes.Unset)

	// A duplicate change_id is a conflict, not a server failure.
	_, err = propose("chg", secretAnalysis())
	assertServiceError(t, err, "conflict")
	assertSummary(t, exp, "surla.controlplane.propose_change",
		"Change Card ID already exists (this request did not overwrite the existing Change Card); call get_change or choose a new change_id.", codes.Unset)

	// Revision success, then an accounting error, which the caller's error
	// names but the span summary does not.
	revised, err := revise("chg", card.RequestRevision, secretRevision("check_01"))
	require.NoError(t, err)
	assertSummary(t, exp, "surla.controlplane.revise_change", "Revised Change Card (needs_confirmation).", codes.Unset)
	_, err = revise("chg", revised.RequestRevision, secretRevision("private-prior-id-secret"))
	assertServiceError(t, err, "unprocessable")
	assert.ErrorContains(t, err, "private-prior-id-secret", "the caller's error names the broken reference")
	assertSummary(t, exp, "surla.controlplane.revise_change",
		"Change Card analysis broke a revision rule (this request did not modify the Change Card); correct the analysis and retry revise_change.", codes.Unset)

	// A card file that cannot be decoded is a storage read fault.
	require.NoError(t, os.WriteFile(cp.cards.path("chg-corrupt"), []byte("{"), 0o600))
	_, err = propose("chg-corrupt", secretAnalysis())
	require.Error(t, err)
	assertSummary(t, exp, "surla.controlplane.propose_change",
		"CP1 proposal could not read Change Card storage (this request did not save a Change Card); preserve your request and ask the server operator to check private card storage.", codes.Error)

	// A staging path that cannot be written makes both saves fail after
	// their checks: the summaries cannot confirm the save either way.
	require.NoError(t, os.Mkdir(filepath.Join(cp.cards.dir, stagingFile), 0o700))
	_, err = propose("chg-save-fault", secretAnalysis())
	require.Error(t, err)
	assertSummary(t, exp, "surla.controlplane.propose_change",
		"CP1 proposal could not confirm Change Card save; preserve your request, call get_change to check whether the card was saved, and ask the server operator to check private card storage before retrying.", codes.Error)
	_, err = revise("chg", revised.RequestRevision, secretRevision("check_01"))
	require.Error(t, err)
	assertSummary(t, exp, "surla.controlplane.revise_change",
		"CP1 revision could not confirm Change Card save; preserve your request, call get_change to check the saved card revision, and ask the server operator to check private card storage before retrying.", codes.Error)

	secrets := []string{
		"private-prompt-secret", "private-analysis-summary-secret", "private-question-secret",
		"private-option-a-secret", "private-option-b-secret", "private-scenario-secret", "private-input-secret",
		"private-outcome-secret", "private-preserved-secret", "private-ui-program-secret",
		"private-design-source-secret", "private-prior-id-secret", "private-bearer-token-secret",
	}
	hostPaths := []string{cardsDir, cp.cards.dir, wsDir}
	var checked int
	for _, sp := range exp.GetSpans() {
		if sp.Name != "surla.controlplane.propose_change" && sp.Name != "surla.controlplane.revise_change" {
			continue
		}
		checked++
		for _, kv := range sp.Attributes {
			for _, s := range append(secrets, hostPaths...) {
				assert.NotContains(t, kv.Value.Emit(), s, "%s attribute %s", sp.Name, kv.Key)
			}
		}
		for _, ev := range sp.Events {
			for _, kv := range ev.Attributes {
				for _, s := range secrets {
					assert.NotContains(t, kv.Value.Emit(), s, "%s event %s", sp.Name, ev.Name)
				}
				if ev.Name != "exception" {
					for _, p := range hostPaths {
						assert.NotContains(t, kv.Value.Emit(), p, "%s event %s", sp.Name, ev.Name)
					}
				}
			}
		}
	}
	assert.Equal(t, 8, checked, "every operation above recorded its span")

	// The caller's own diagnostic view holds no content and no host paths,
	// including the raw error text of the faults.
	page, err := cp.ListTraces(alice, &gensurla.ListTracesPayload{Status: "all", Limit: 100})
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(cp.cards.dir, "traces.jsonl"))
	require.NoError(t, err)
	for _, s := range append(append(secrets, hostPaths...), "alice@example.com") {
		assert.NotContains(t, string(raw), s)
		for _, sp := range page.Spans {
			assert.NotContains(t, sp.Summary, s)
			for _, v := range sp.Attributes {
				assert.NotContains(t, v, s)
			}
		}
	}
	assert.NotEmpty(t, page.Spans)
}

// TestCP1ConcurrentChangesKeepTheWinningCard holds one propose_change or
// revise_change at its span start, before it checks and saves the card,
// changes the card meanwhile, and checks that the held request refuses to overwrite the
// change, says in its span summary that only this request did not save, and
// leaves the other request's card intact: a concurrent proposal of the same
// change_id, a concurrent revision or confirmation, a card removed, a card
// now owned by someone else, and a card file that can no longer be read.
func TestCP1ConcurrentChangesKeepTheWinningCard(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	check01 := "check_01"
	revision := func(summary string, priorID *string) *gensurla.CP1RevisionAnalysis {
		return &gensurla.CP1RevisionAnalysis{
			Summary:     summary,
			Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
			Examples: []*gensurla.CP1RevisionAnalysisExample{{
				PriorID: priorID, Scenario: "Refund at the limit", InputSummary: "Order total $80.00",
				ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
			}},
			RemovedExampleIds:  []string{},
			PreservedBehaviors: []string{},
		}
	}
	type setup struct {
		cp   *ControlPlane
		exp  *tracetest.InMemoryExporter
		hold *spanHold
		card *gensurla.ChangeCardResult
	}
	newSetup := func(t *testing.T, held string) setup {
		t.Helper()
		exp := tracetest.NewInMemoryExporter()
		hold := &spanHold{name: held, started: make(chan struct{}), release: make(chan struct{})}
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(hold), sdktrace.WithSyncer(exp))
		t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.Background())) })
		cp := newControlPlane(t, Options{TracerProvider: tp})
		card := mustPropose(t, cp, alice, "chg", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
		hold.armed.Store(true)
		return setup{cp: cp, exp: exp, hold: hold, card: card}
	}
	// heldRevise starts a revision of s.card that stops at its span start and
	// returns the channel its error arrives on.
	heldRevise := func(t *testing.T, s setup, analysis *gensurla.CP1RevisionAnalysis) <-chan error {
		t.Helper()
		done := s.hold.run(t, func() error {
			_, err := s.cp.ReviseChange(alice, &gensurla.ReviseChangePayload{
				ChangeID: "chg", RequestRevision: s.card.RequestRevision, Prompt: "Refunds up to $80 are instant.", Analysis: analysis,
			})
			return err
		})
		return done
	}

	t.Run("concurrent proposal of the same change_id", func(t *testing.T) {
		s := newSetup(t, "surla.controlplane.propose_change")
		done := s.hold.run(t, func() error {
			_, err := s.cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "chg-race", Prompt: "Request A", Analysis: refundAnalysis("Request A")})
			return err
		})
		winner := mustPropose(t, s.cp, alice, "chg-race", "Request B", refundAnalysis("Request B"))
		s.hold.releaseOnce()
		assertServiceError(t, awaitHeld(t, done), "conflict")
		assertSummary(t, s.exp, "surla.controlplane.propose_change",
			"Change Card ID already exists (this request did not overwrite the existing Change Card); call get_change or choose a new change_id.", codes.Unset)
		got, err := s.cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg-race"})
		require.NoError(t, err)
		assert.Equal(t, winner.RequestRevision, got.RequestRevision)
	})

	t.Run("refused analysis while another request saves", func(t *testing.T) {
		s := newSetup(t, "surla.controlplane.revise_change")
		unknown := "check_09"
		done := heldRevise(t, s, revision("Request A", &unknown))
		winner, err := s.cp.ReviseChange(alice, &gensurla.ReviseChangePayload{
			ChangeID: "chg", RequestRevision: s.card.RequestRevision, Prompt: "Request B", Analysis: revision("Request B", &check01),
		})
		require.NoError(t, err)
		s.hold.releaseOnce()
		errA := awaitHeld(t, done)
		assertServiceError(t, errA, "unprocessable")
		assert.NotContains(t, errA.Error(), "Request B")
		got, err := s.cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
		require.NoError(t, err)
		assert.Equal(t, winner.RequestRevision, got.RequestRevision, "the refused request leaves the other request's revision intact")
	})

	for _, tc := range []struct {
		name string
		// change changes the card while the revision is held.
		change func(t *testing.T, s setup)
		// wantErr is the Goa error name the held revision returns.
		wantErr string
		// wantSummary is the held revision's span summary.
		wantSummary string
		// wantStatus is the held revision's span status.
		wantStatus codes.Code
	}{
		{
			name: "concurrent revision",
			change: func(t *testing.T, s setup) {
				_, err := s.cp.ReviseChange(alice, &gensurla.ReviseChangePayload{
					ChangeID: "chg", RequestRevision: s.card.RequestRevision, Prompt: "Request B", Analysis: revision("Request B", &check01),
				})
				require.NoError(t, err)
			},
			wantErr:     "conflict",
			wantSummary: "Change Card request_revision changed concurrently (this request did not modify the Change Card); call get_change and retry revise_change.",
			wantStatus:  codes.Unset,
		},
		{
			name: "concurrent confirmation",
			change: func(t *testing.T, s setup) {
				ex := *s.card.Examples[0]
				ex.ExpectedOutcome = "Instant store credit within a minute"
				_, err := s.cp.ConfirmIntent(alice, &gensurla.ConfirmIntentPayload{
					ChangeID: "chg", RequestRevision: s.card.RequestRevision, Answers: map[string]string{}, Examples: []*gensurla.ExampleItem{&ex},
				})
				require.NoError(t, err)
			},
			wantErr:     "conflict",
			wantSummary: "Change Card request_revision changed concurrently (this request did not modify the Change Card); call get_change and retry revise_change.",
			wantStatus:  codes.Unset,
		},
		{
			name: "card removed",
			change: func(t *testing.T, s setup) {
				require.NoError(t, os.Remove(s.cp.cards.path("chg")))
			},
			wantErr:     "not_found",
			wantSummary: "Change Card was not found; check change_id or call propose_change.",
			wantStatus:  codes.Unset,
		},
		{
			name: "card now owned by someone else",
			change: func(t *testing.T, s setup) {
				foreign := *s.card
				rev, err := computeCanonicalRevision(&foreign, "bob@example.com", "")
				require.NoError(t, err)
				foreign.RequestRevision = rev
				require.NoError(t, s.cp.cards.save(storedChangeCard{card: &foreign, owner: "bob@example.com", sourceOrigin: sourceOriginSupplied}))
			},
			wantErr:     "forbidden",
			wantSummary: "Caller does not own this Change Card (this request did not modify the Change Card).",
			wantStatus:  codes.Unset,
		},
		{
			name: "card file no longer readable",
			change: func(t *testing.T, s setup) {
				require.NoError(t, os.WriteFile(s.cp.cards.path("chg"), []byte("{"), 0o600))
			},
			wantSummary: "CP1 revision could not read Change Card storage (this request did not modify the Change Card); preserve your request and ask the server operator to check private card storage.",
			wantStatus:  codes.Error,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSetup(t, "surla.controlplane.revise_change")
			done := heldRevise(t, s, revision("Request A", &check01))
			before, beforeErr := os.ReadFile(s.cp.cards.path("chg"))
			tc.change(t, s)
			after, afterErr := os.ReadFile(s.cp.cards.path("chg"))
			s.hold.releaseOnce()
			errA := awaitHeld(t, done)
			require.Error(t, errA)
			if tc.wantErr != "" {
				assertServiceError(t, errA, tc.wantErr)
			}
			assertSummary(t, s.exp, "surla.controlplane.revise_change", tc.wantSummary, tc.wantStatus)
			now, nowErr := os.ReadFile(s.cp.cards.path("chg"))
			assert.Equal(t, afterErr == nil, nowErr == nil, "the held request neither created nor removed the card file")
			assert.Equal(t, string(after), string(now), "the held request left the card file as the other change left it")
			if beforeErr == nil && afterErr == nil {
				assert.NotEqual(t, string(before), string(now), "the card changed while the request was held")
			}
		})
	}
}

// OnStart holds a span named h.name, once armed, until release is closed.
func (h *spanHold) OnStart(_ context.Context, s sdktrace.ReadWriteSpan) {
	if s.Name() != h.name || !h.armed.CompareAndSwap(true, false) {
		return
	}
	close(h.started)
	<-h.release
}

// OnEnd does nothing.
func (h *spanHold) OnEnd(sdktrace.ReadOnlySpan) {}

// Shutdown does nothing.
func (h *spanHold) Shutdown(context.Context) error { return nil }

// ForceFlush does nothing.
func (h *spanHold) ForceFlush(context.Context) error { return nil }

// run calls op in a goroutine, waits until op's span is held, and returns the
// channel op's error arrives on. If the test ends first, cleanup releases the
// span and waits for op, so no request outlives the test's directories.
func (h *spanHold) run(t *testing.T, op func() error) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- op()
	}()
	t.Cleanup(func() {
		h.releaseOnce()
		<-finished
	})
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the request did not reach its span start within 5s")
	}
	return done
}

// releaseOnce lets a held span start; later calls do nothing.
func (h *spanHold) releaseOnce() {
	select {
	case <-h.release:
	default:
		close(h.release)
	}
}

// awaitHeld waits up to 5 seconds for a released request's error.
func awaitHeld(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the released request did not finish within 5s")
		return nil
	}
}

// assertSummary checks the latest ended span named name: its
// surla.public_summary equals want and its status code is wantStatus.
func assertSummary(t *testing.T, exp *tracetest.InMemoryExporter, name, want string, wantStatus codes.Code) {
	t.Helper()
	sp := lastSpanByName(t, exp.GetSpans(), name)
	got, ok := findSpanAttr(sp.Attributes, "surla.public_summary")
	require.True(t, ok, "%s has no summary", name)
	assert.Equal(t, want, got.AsString())
	assert.Equal(t, wantStatus, sp.Status.Code, "%s status", name)
}

// lastSpanByName returns the most recent span with name from spans.
func lastSpanByName(t *testing.T, spans []tracetest.SpanStub, name string) tracetest.SpanStub {
	t.Helper()
	for i := len(spans) - 1; i >= 0; i-- {
		if spans[i].Name == name {
			return spans[i]
		}
	}
	t.Fatalf("span %q not found", name)
	return tracetest.SpanStub{}
}

// findSpanAttr finds the attribute value for key in attrs.
func findSpanAttr(attrs []attribute.KeyValue, key attribute.Key) (attribute.Value, bool) {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value, true
		}
	}
	return attribute.Value{}, false
}
