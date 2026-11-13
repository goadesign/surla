// These tests bind recorded protocol streams and HTTP replies, never real team effects.
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goadesign/surla/internal/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTeamRecordedNDJSON exercises initial and recovery snapshots, ordering metadata,
// heartbeats and each meaningful event while excluding work-bearing text from typed results.
func TestTeamRecordedNDJSON(t *testing.T) {
	stream := `{"seq":10,"terminals":[{"address":"terminal","state":"live","labels":{"chorus.handle":"guide","chorus.options":"PRIVATE OPTIONS"},"reported":true,"status":{"state":"idle","msg":"PRIVATE STATUS"},"title":"PRIVATE TITLE","screen":"exact","foreground":{"argv":["codex","arg1","arg2","PRIVATE ARGUMENT"]}}]}
{"jsonrpc":"2.0","method":"event","params":{"seq":11,"terminal":"terminal","type":"title","data":{"title":"PRIVATE TITLE"}}}
{"jsonrpc":"2.0","method":"event","params":{"seq":12,"terminal":"terminal","type":"status","data":{"reported":true,"status":{"state":"working","msg":"PRIVATE STATUS"}}}}
{"jsonrpc":"2.0","method":"heartbeat","params":{"seq":12}}
{"jsonrpc":"2.0","method":"missed","params":{"seq":15}}
{"seq":15,"terminals":[]}
{"jsonrpc":"2.0","method":"event","params":{"seq":16,"terminal":"terminal","type":"labels","data":{"labels":{"chorus.handle":"guide"}}}}
{"jsonrpc":"2.0","method":"event","params":{"seq":17,"terminal":"terminal","type":"turn","data":{"state":"done"}}}
{"jsonrpc":"2.0","method":"event","params":{"seq":18,"terminal":"terminal","type":"finished","data":{"reason":"exited"}}}
`
	var events []project.TeamEvent
	err := readTeamEvents(strings.NewReader(stream), "guide", func(event project.TeamEvent) { events = append(events, event) })
	require.Error(t, err, "EOF must cause reconnect, never a healthy ended reader")
	require.Len(t, events, 8)
	assert.Equal(t, "snapshot", events[0].Kind)
	assert.Equal(t, uint64(10), events[0].Sequence)
	assert.Equal(t, "status", events[1].Type)
	assert.Equal(t, "heartbeat", events[2].Kind)
	assert.Equal(t, "missed", events[3].Kind)
	assert.Equal(t, "snapshot", events[4].Kind)
	assert.Empty(t, events[4].Terminals)
	assert.Equal(t, "finished", events[7].Type)
	raw, err := json.Marshal(events)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "PRIVATE")
	assert.NotContains(t, string(raw), "screen")
	for _, shape := range []reflect.Type{reflect.TypeOf(project.TeamTerminal{}), reflect.TypeOf(project.TeamStatus{}), reflect.TypeOf(teamStreamLine{})} {
		for i := 0; i < shape.NumField(); i++ {
			assert.NotContains(t, []string{"Title", "Msg", "Screen", "Message"}, shape.Field(i).Name)
		}
	}
}

// TestTeamFilteredSubscription selects each registered handle with AND labels and
// joins all subscriptions after one fails, without inspecting unrelated terminals.
func TestTeamFilteredSubscription(t *testing.T) {
	var mu sync.Mutex
	var calls [][]string
	started := make(chan struct{}, 2)
	o := teamObservations{commands: teamCommands{core: "recorded-herdr"}, stream: func(ctx context.Context, program string, args []string, read func(io.Reader) error) error {
		assert.Equal(t, "recorded-herdr", program)
		mu.Lock()
		calls = append(calls, args)
		mu.Unlock()
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- o.Follow(ctx, project.TeamSelection{Root: "/recorded-project", Space: "owned-space", Roles: []project.TeamRole{{Role: "guide", Handle: "guide"}, {Role: "builder", Handle: "builder"}}}, func(project.TeamEvent) {})
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("subscriptions not started")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("subscriptions not joined")
	}
	for _, args := range calls {
		assert.Equal(t, []string{"events", "--json", "--label", "chorus.project=/recorded-project", "--label", "herdr.space=owned-space", "--label"}, args[:7])
		assert.Contains(t, []string{"chorus.handle=guide", "chorus.handle=builder"}, args[7])
	}
}

// TestTeamDiscoveryBeforeRead proves the recorded CLI receives all owner labels
// before any get, and the resulting narrow values contain no message or title.
func TestTeamDiscoveryBeforeRead(t *testing.T) {
	var calls [][]string
	c := teamCommands{core: "recorded", run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "ls" {
			return []byte(`{"terminals":[{"address":"owned"}]}`), nil
		}
		return []byte(`{"address":"owned","state":"live","labels":{"chorus.handle":"guide","chorus.project":"/project","herdr.space":"space"},"status":{"state":"idle","msg":"PRIVATE"},"title":"PRIVATE","screen":"exact"}`), nil
	}}
	o := teamObservations{commands: c}
	result, err := o.FindTerminals(t.Context(), project.TeamSelection{Root: "/project", Space: "space", Roles: []project.TeamRole{{Role: "guide", Handle: "guide"}}})
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, []string{"ls", "--json", "--label", "chorus.project=/project", "--label", "herdr.space=space", "--label", "chorus.handle=guide"}, calls[0])
	assert.Equal(t, []string{"get", "owned", "--json"}, calls[1])
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "PRIVATE")
}

// TestTeamWatcherHTTP drives conditional reads, unchanged replies and failures
// through a local recorded endpoint, retaining only registered handles' facts.
func TestTeamWatcherHTTP(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("ETag", "version")
		assert.Equal(t, "/swarms/team/view", r.URL.Path)
		assert.Equal(t, "60", r.URL.Query().Get("wait"))
		assert.Equal(t, http.MethodGet, r.Method)
		switch calls {
		case 1:
			_, err := io.WriteString(w, `{"swarm":"team","agents":[{"handle":"guide","watching_on_this_server":true},{"handle":"other","watching_on_this_server":false}],"inboxes":[{"messages":[{"text":"PRIVATE"}]}]}`)
			assert.NoError(t, err)
		case 2:
			assert.Equal(t, "version", r.Header.Get("If-None-Match"))
			w.WriteHeader(304)
		case 3:
			w.WriteHeader(503)
		case 4:
			_, err := io.WriteString(w, `{"swarm":"team","agents":[{"handle":"guide"}]}`)
			assert.NoError(t, err)
		}
	}))
	defer server.Close()
	o := teamObservations{client: server.Client(), chorusURL: server.URL}
	roles := []project.TeamRole{{Role: "guide", Handle: "guide"}}
	view, err := o.ReadWatchers(t.Context(), "team", "", roles)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"guide": true}, view.Held)
	assert.Equal(t, "version", view.Version)
	unchanged, err := o.ReadWatchers(t.Context(), "team", view.Version, roles)
	require.NoError(t, err)
	assert.True(t, unchanged.Unchanged)
	_, err = o.ReadWatchers(t.Context(), "team", view.Version, roles)
	assert.Error(t, err)
	_, err = o.ReadWatchers(t.Context(), "team", view.Version, roles)
	assert.Error(t, err)
}

// TestTeamSenderRecordedProtocol verifies conditions, exit refusals and the
// explicit unbound production sender without any actual typing or settings.
func TestTeamSenderRecordedProtocol(t *testing.T) {
	for _, condition := range []string{"idle", ""} {
		for _, code := range []int{0, 9, 10, 11, 1} {
			var args []string
			c := teamCommands{core: "recorded", run: func(_ context.Context, _ string, argv ...string) ([]byte, error) {
				args = argv
				if code != 0 {
					return nil, teamCommandExit{code: code}
				}
				return []byte(`{}`), nil
			}}
			outcome, err := c.Send(t.Context(), project.TeamReminder{Terminal: "terminal", Text: "Surla reminder.", Condition: condition})
			assert.Equal(t, map[int]string{0: "sent", 9: "blocked", 10: "status_changed", 11: "finished", 1: "unavailable"}[code], outcome)
			assert.Equal(t, code == 1, err != nil)
			want := []string{"send", "terminal", "Surla reminder.", "--json"}
			if condition != "" {
				want = append(want, "--if", "idle")
			}
			assert.Equal(t, want, args)
			assert.NotContains(t, args, "--force")
			assert.NotContains(t, args, "--wait")
		}
	}
	deps, err := projectTeamDependencies(t.Context())
	require.NoError(t, err)
	require.IsType(t, unboundTeamSender{}, deps.Sender)
	outcome, err := deps.Sender.Send(t.Context(), project.TeamReminder{})
	assert.NoError(t, err)
	assert.Equal(t, "unavailable", outcome)
}
