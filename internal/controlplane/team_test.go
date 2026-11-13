// This file tests the generated user boundary, assignment owner and supervisor shutdown.
package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// teamServiceFixture models the narrow owner without any outside effect.
type teamServiceFixture struct {
	mu        sync.Mutex
	snapshot  gensurla.TeamSnapshot
	calls     []string
	supervise func(context.Context, AssignmentReader) error
}

// ReadTeam returns a copied snapshot and records a read, never an assignment mutation.
func (f *teamServiceFixture) ReadTeam(context.Context) (*gensurla.TeamSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "read")
	copy := f.snapshot
	return &copy, nil
}

// ShowPane records the authorized requested role.
func (f *teamServiceFixture) ShowPane(_ context.Context, role string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "show:"+role)
	return nil
}

// StartAgents records the authorized action without starting any process.
func (f *teamServiceFixture) StartAgents(context.Context) (*gensurla.ProjectTeam, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start")
	return &gensurla.ProjectTeam{Team: "team", Space: "space", Dialog: "opened", Roles: []*gensurla.ProjectTeamRole{}}, nil
}

// Supervise is a bounded stand-in that ends when its owner does.
func (f *teamServiceFixture) Supervise(ctx context.Context, read AssignmentReader) error {
	if f.supervise != nil {
		return f.supervise(ctx, read)
	}
	<-ctx.Done()
	return nil
}

// TestTeamBindingValidation refuses partial bindings without leaking the owner.
func TestTeamBindingValidation(t *testing.T) {
	for _, binding := range []*TeamBinding{{Owner: "private-owner"}, {Service: &teamServiceFixture{}}} {
		cp, err := New(Options{CardsDir: t.TempDir(), Team: binding})
		assert.Nil(t, cp)
		assert.Error(t, err)
		assert.NotContains(t, err.Error(), "private-owner")
	}
}

// TestTeamUserTransport enforces user-only access before the owner sees a call,
// including management, and validates the role with the generated decoder.
func TestTeamUserTransport(t *testing.T) {
	for _, kind := range []CallerKind{CallerUser, CallerGuide, CallerBuilder, CallerTestWriter, CallerManagement} {
		t.Run(string(kind), func(t *testing.T) {
			f := &teamServiceFixture{snapshot: gensurla.TeamSnapshot{Team: "team", Roles: []*gensurla.TeamRoleState{}}}
			cp := newControlPlane(t, Options{Team: &TeamBinding{Owner: "owner", Service: f}})
			handler := NewHTTPHandler(cp, func(*http.Request) (Principal, error) { return Principal{ID: "owner", Kind: kind}, nil })
			for _, route := range []struct{ method, path, body string }{{"GET", "/team", ""}, {"POST", "/team/pane", `{"role":"builder"}`}, {"POST", "/team/agents", ""}} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(route.method, route.path, strings.NewReader(route.body)))
				if kind == CallerUser {
					assert.Less(t, response.Code, 300, response.Body.String())
				} else {
					assert.Equal(t, 403, response.Code, response.Body.String())
				}
			}
			if kind == CallerUser {
				assert.Equal(t, []string{"read", "show:builder", "start"}, f.calls)
			} else {
				assert.Empty(t, f.calls)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("POST", "/team/pane", strings.NewReader(`{"role":"other"}`)))
			assert.Equal(t, 400, response.Code)
		})
	}
}

// TestTeamAgentChangeListDescription exposes only readable facts to an agent;
// its opaque version must not instruct the agent to read the user-only team API.
func TestTeamAgentChangeListDescription(t *testing.T) {
	cp := newControlPlane(t, Options{})
	server := httptest.NewServer(NewHTTPHandler(cp, fixtureCredentials(map[string]Principal{"builder": {ID: "owner", Kind: CallerBuilder}})))
	defer server.Close()
	raw := mcpRequest(t, server.URL, "builder", "tools/list", "", `{}`)
	var catalog struct {
		Tools []struct {
			Name         string          `json:"name"`
			Description  string          `json:"description"`
			OutputSchema json.RawMessage `json:"outputSchema"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(raw, &catalog))
	found := false
	for _, tool := range catalog.Tools {
		assert.NotContains(t, []string{"get_team", "show_team_pane", "start_team_agents"}, tool.Name)
		if tool.Name == "list_changes" {
			found = true
			assert.NotContains(t, tool.Description, "get_team")
			assert.NotContains(t, string(tool.OutputSchema), "team snapshot")
			assert.Contains(t, string(tool.OutputSchema), "Compare it only for equality")
		}
	}
	assert.True(t, found)
}

// TestTeamUserResourceOwner refuses another user principal before the single
// bound project owner sees any read or action, independently of caller purpose.
func TestTeamUserResourceOwner(t *testing.T) {
	f := &teamServiceFixture{}
	cp := newControlPlane(t, Options{Team: &TeamBinding{Owner: "alice", Service: f}})
	handler := NewHTTPHandler(cp, func(*http.Request) (Principal, error) {
		return Principal{ID: "bob", Kind: CallerUser}, nil
	})
	for _, request := range []struct{ method, path, body string }{
		{"GET", "/team", ""}, {"POST", "/team/pane", `{"role":"builder"}`}, {"POST", "/team/agents", ""},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(request.method, request.path, strings.NewReader(request.body)))
		assert.Equal(t, http.StatusForbidden, response.Code)
		assert.NotContains(t, response.Body.String(), "alice")
		assert.NotContains(t, response.Body.String(), "bob")
	}
	assert.Empty(t, f.calls)
}

// TestTeamVersionBelongsToResourceOwner keeps other owners' change-list waits
// independent of the bound team's activity and never reads that team for them.
func TestTeamVersionBelongsToResourceOwner(t *testing.T) {
	f := &teamServiceFixture{snapshot: gensurla.TeamSnapshot{Team: "team", Roles: []*gensurla.TeamRoleState{}}}
	cp := newControlPlane(t, Options{Team: &TeamBinding{Owner: "alice", Service: f}})
	cp.changesWait = time.Millisecond
	alice, bob := asCaller("alice", CallerUser), asCaller("bob", CallerUser)
	a, err := cp.ListChanges(alice)
	require.NoError(t, err)
	b, err := cp.ListChanges(bob)
	require.NoError(t, err)
	assert.Equal(t, []string{"read"}, f.calls)
	f.mu.Lock()
	f.snapshot.Roles = []*gensurla.TeamRoleState{{Role: "builder", Handle: "builder", State: "waiting", Attention: "none", Watcher: "gone", Assignment: "unread"}}
	f.calls = nil
	f.mu.Unlock()
	bobWait, err := cp.WaitForChanges(bob, &gensurla.WaitForChangesPayload{Since: b.Version})
	require.NoError(t, err)
	assert.Equal(t, b.Version, bobWait.Version)
	bobList, err := cp.ListChanges(bob)
	require.NoError(t, err)
	assert.Equal(t, b.Version, bobList.Version)
	assert.Empty(t, f.calls, "other owners never read the bound team's state")
	aliceWait, err := cp.WaitForChanges(alice, &gensurla.WaitForChangesPayload{Since: a.Version})
	require.NoError(t, err)
	assert.NotEqual(t, a.Version, aliceWait.Version)
}

// TestTeamAssignmentGenerations reads only the binding's owner, preserves stale
// revision debt, and proves neither inspection nor a failed read marks it read.
func TestTeamAssignmentGenerations(t *testing.T) {
	f := &teamServiceFixture{}
	cp := newControlPlane(t, Options{Team: &TeamBinding{Owner: "alice@example.com", Service: f}})
	for _, owner := range []string{"alice@example.com", "bob@example.com"} {
		guide := asCaller(owner, CallerGuide)
		user := asCaller(owner, CallerUser)
		card := mustPropose(t, cp, guide, owner, "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
		confirmed, err := cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{ChangeID: owner, RequestRevision: card.RequestRevision, Answers: map[string]string{}, Examples: card.Examples})
		require.NoError(t, err)
		_, err = cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: owner, RequestRevision: confirmed.RequestRevision})
		require.NoError(t, err)
	}
	first, err := cp.assignmentGenerations(t.Context())
	require.NoError(t, err)
	require.Len(t, first, 2)
	assert.Equal(t, "builder", first[0].Role)
	assert.Len(t, first[0].Generation, 64)
	second, err := cp.assignmentGenerations(t.Context())
	require.NoError(t, err)
	assert.Equal(t, first, second)
	builds, err := cp.ownedBuilds("alice@example.com")
	require.NoError(t, err)
	assert.False(t, builds[0].file.Builder.Read)
	assert.False(t, builds[0].file.TestWriter.Read)
	// A later request revision does not automatically erase an earlier assignment.
	cp.mu.Lock()
	record, found, err := cp.cards.load("alice@example.com")
	require.NoError(t, err)
	require.True(t, found)
	record.card.Prompt = "Changed request"
	record.card.RequestRevision, err = computeCanonicalRevision(record.card, record.owner, record.designSource)
	require.NoError(t, err)
	require.NoError(t, cp.cards.save(record))
	cp.mu.Unlock()
	stale, err := cp.assignmentGenerations(t.Context())
	require.NoError(t, err)
	assert.Equal(t, first, stale)
	_, err = cp.GetBuilderAssignment(asCaller("alice@example.com", CallerBuilder), &gensurla.GetBuilderAssignmentPayload{ChangeID: "alice@example.com"})
	require.NoError(t, err)
	owed, err := cp.assignmentGenerations(t.Context())
	require.NoError(t, err)
	require.Len(t, owed, 1)
	assert.Equal(t, "test_writer", owed[0].Role)
	// Owner read errors remain errors, rather than an empty successful owed set.
	name := filepath.Join(cp.cards.dir, buildFileName("alice@example.com"))
	original, err := os.ReadFile(name)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(name, []byte("broken"), 0600))
	_, err = cp.assignmentGenerations(t.Context())
	assert.Error(t, err)
	require.NoError(t, os.WriteFile(name, original, 0600))
}

// TestTeamSupervisorOwnedShutdown proves startup admission, cancel and drain
// happen even without Live configured, and failures have a bounded retry delay.
func TestTeamSupervisorOwnedShutdown(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(map[bool]string{false: "drain", true: "early return"}[early], func(t *testing.T) {
			started := make(chan struct{}, 2)
			stopped := make(chan struct{})
			f := &teamServiceFixture{}
			f.supervise = func(ctx context.Context, read AssignmentReader) error {
				if early {
					started <- struct{}{}
					return assert.AnError
				}
				_, err := read(ctx)
				assert.NoError(t, err)
				started <- struct{}{}
				<-ctx.Done()
				close(stopped)
				return nil
			}
			cp, err := New(Options{CardsDir: t.TempDir(), Team: &TeamBinding{Owner: "owner", Service: f}})
			require.NoError(t, err)
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("supervisor did not start")
			}
			require.NoError(t, cp.Close())
			if !early {
				select {
				case <-stopped:
				default:
					t.Fatal("close returned before supervisor drained")
				}
			}
			assert.Empty(t, started, "unexpected immediate retry")
		})
	}
}

// TestTeamSupervisorNormalOwnerCompletion ends the worker without a failure
// or a retry when the project's independent lifetime finishes before CP.Close.
func TestTeamSupervisorNormalOwnerCompletion(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	started, lifetime := make(chan struct{}, 2), make(chan struct{})
	f := &teamServiceFixture{supervise: func(context.Context, AssignmentReader) error {
		started <- struct{}{}
		<-lifetime
		return nil
	}}
	cp := newControlPlane(t, Options{TracerProvider: provider, Team: &TeamBinding{Owner: "owner", Service: f}})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	close(lifetime)
	drained := make(chan struct{})
	go func() { cp.teamWorker.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("normal project completion did not drain CP worker")
	}
	assert.NoError(t, cp.serverCtx.Err(), "the control plane remains live")
	assert.Empty(t, started, "normal completion must not restart the owner")
	assert.Empty(t, exporter.GetSpans(), "normal completion must not fabricate an error span")
}

// TestTeamWaitingVersionAndHistory proves the existing wait covers every public
// team fact and neither its request nor its inner span reaches bounded history.
func TestTeamWaitingVersionAndHistory(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
	f := &teamServiceFixture{snapshot: gensurla.TeamSnapshot{Team: "team", Roles: []*gensurla.TeamRoleState{{Role: "builder", Handle: "builder", State: "working", Attention: "none", Watcher: "held", Assignment: "nothing_owed"}}}}
	cp := newControlPlane(t, Options{TracerProvider: provider, Team: &TeamBinding{Owner: "owner", Service: f}})
	cp.changesWait = time.Millisecond
	user := asCaller("owner", CallerUser)
	list, err := cp.ListChanges(user)
	require.NoError(t, err)
	endpoints := gensurla.NewEndpoints(cp)
	applyOperationPolicy(endpoints, cp.tracer("test"))
	for _, field := range []string{"state", "attention", "watcher", "assignment"} {
		f.mu.Lock()
		old := f.snapshot.Roles[0]
		next := *old
		switch field {
		case "state":
			next.State = "needs_you"
		case "attention":
			next.Attention = "permission"
		case "watcher":
			next.Watcher = "gone"
		case "assignment":
			next.Assignment = "unread"
		}
		f.snapshot.Roles = []*gensurla.TeamRoleState{&next}
		f.mu.Unlock()
		result, err := endpoints.WaitForChanges(user, &gensurla.WaitForChangesPayload{Since: list.Version})
		require.NoError(t, err)
		assert.NotEqual(t, list.Version, result.(*gensurla.ChangeListVersion).Version)
		list, err = cp.ListChanges(user)
		require.NoError(t, err)
	}
	traces, err := cp.traces.List("owner", "all", 100, nil)
	require.NoError(t, err)
	raw, err := json.Marshal(traces)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "wait_for_changes")
	var names []string
	for _, span := range exporter.GetSpans() {
		names = append(names, span.Name)
	}
	assert.Contains(t, names, waitForChangesSpan)
	assert.Contains(t, names, "surla.request.wait_for_changes")
}
