// These tests post reminders to recorded local HTTP servers and Herdr stand-ins;
// no Chorus server, terminal or agent is contacted.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goadesign/surla/internal/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type (
	// recordedPost is one request a recorded Chorus server received.
	recordedPost struct {
		method, path string
		header       http.Header
		body         map[string]any
	}
	// recordedChorus answers each tool with a recorded handler and keeps every request.
	recordedChorus struct {
		mu     sync.Mutex
		posts  []recordedPost
		answer map[string]http.HandlerFunc
		// writeErrs keeps failures to write the default answer.
		writeErrs []error
	}
	// countingReader yields an endless JSON string and counts the bytes read from it.
	countingReader struct{ read int }
)

// testReminder is a reminder whose handles and team are the longest Chorus allows.
var testReminder = project.TeamReminder{
	Team: "example-team-0123456789",
	From: "surla-7c8f9f114040",
	To:   "builder-7c8f9f114040",
	Text: "Surla: you have an unread assignment. Use list_builder_assignments and get_builder_assignment to read it. Surla reads no replies.",
}

// okToolResult is a successful tool result carrying unread messages Surla must drop.
const okToolResult = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"sent\n--- unread ---\n@builder: PRIVATE unread message"}]}}`

// ServeHTTP records the request, then answers with the tool's recorded handler.
func (c *recordedChorus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	raw, err := io.ReadAll(r.Body)
	if err == nil {
		err = json.Unmarshal(raw, &body)
	}
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.posts = append(c.posts, recordedPost{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body})
	c.mu.Unlock()
	tool := r.Header.Get("Mcp-Name")
	if answer := c.answer[tool]; answer != nil {
		answer(w, r)
		return
	}
	if _, err := io.WriteString(w, okToolResult); err != nil {
		c.mu.Lock()
		c.writeErrs = append(c.writeErrs, err)
		c.mu.Unlock()
	}
}

// tools lists the tool names of the recorded posts in order.
func (c *recordedChorus) tools() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var tools []string
	for _, post := range c.posts {
		tools = append(tools, post.header.Get("Mcp-Name"))
	}
	return tools
}

// Read fills p with the bytes of an endless JSON string.
func (r *countingReader) Read(p []byte) (int, error) {
	prefix := `{"result":{"content":[{"text":"`
	for i := range p {
		if r.read+i < len(prefix) {
			p[i] = prefix[r.read+i]
		} else {
			p[i] = 'a'
		}
	}
	r.read += len(p)
	return len(p), nil
}

// startRecordedChorus serves one recorded Chorus and returns a production
// sender bound to it.
func startRecordedChorus(t *testing.T, answer map[string]http.HandlerFunc) (*recordedChorus, teamChorusSender) {
	t.Helper()
	chorus := &recordedChorus{answer: answer}
	server := httptest.NewServer(chorus)
	t.Cleanup(server.Close)
	sender := newTeamChorusSender()
	sender.chorusURL = server.URL
	return chorus, sender
}

// TestTeamReminderPostsWhoThenSend makes exactly two POSTs to /mcp, who then
// send, and no other request. The arguments of who are exactly agent, swarm,
// model and role; those of send are exactly agent, to and text.
func TestTeamReminderPostsWhoThenSend(t *testing.T) {
	chorus, sender := startRecordedChorus(t, nil)
	require.NoError(t, sender.Send(t.Context(), testReminder))
	require.Len(t, chorus.posts, 2)
	assert.Empty(t, chorus.writeErrs)
	wantArguments := map[string]map[string]any{
		"who": {
			"agent": "surla-7c8f9f114040",
			"swarm": "example-team-0123456789",
			"model": "surla",
			"role":  "Surla server. Sends reminders only. Reads no replies.",
		},
		"send": {
			"agent": "surla-7c8f9f114040",
			"to":    "@builder-7c8f9f114040",
			"text":  testReminder.Text,
		},
	}
	for i, tool := range []string{"who", "send"} {
		post := chorus.posts[i]
		assert.Equal(t, http.MethodPost, post.method)
		assert.Equal(t, "/mcp", post.path)
		assert.Equal(t, "application/json", post.header.Get("Content-Type"))
		assert.Equal(t, "application/json", post.header.Get("Accept"))
		assert.Equal(t, "2026-07-28", post.header.Get("MCP-Protocol-Version"))
		assert.Equal(t, "tools/call", post.header.Get("Mcp-Method"))
		assert.Equal(t, tool, post.header.Get("Mcp-Name"))
		assert.Equal(t, "2.0", post.body["jsonrpc"])
		assert.Equal(t, "tools/call", post.body["method"])
		params, ok := post.body["params"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, tool, params["name"])
		assert.Equal(t, wantArguments[tool], params["arguments"])
		meta, ok := params["_meta"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "2026-07-28", meta["io.modelcontextprotocol/protocolVersion"])
		assert.Equal(t, map[string]any{}, meta["io.modelcontextprotocol/clientCapabilities"])
		assert.Equal(t, map[string]any{"name": "surla", "version": "1"}, meta["io.modelcontextprotocol/clientInfo"])
		assert.ElementsMatch(t, []string{"jsonrpc", "id", "method", "params"}, slices.Collect(maps.Keys(post.body)))
		assert.ElementsMatch(t, []string{"_meta", "name", "arguments"}, slices.Collect(maps.Keys(params)))
	}
}

// TestTeamReminderResponseBound keeps no byte of a response: unread messages
// carried by send leave nothing in the result, and the decoded response type
// has no field that could hold text. A response of 2 MiB or less is decoded; a
// longer one is read no further than the bound and fails with response.
func TestTeamReminderResponseBound(t *testing.T) {
	t.Run("unread messages are dropped", func(t *testing.T) {
		_, sender := startRecordedChorus(t, nil)
		assert.NoError(t, sender.Send(t.Context(), testReminder))
		assertNoTextFields(t, reflect.TypeFor[teamToolResponse]())
		assertNoTextFields(t, reflect.TypeFor[project.TeamSendFailure]())
	})
	for _, tc := range []struct {
		name  string
		size  int
		error bool
	}{
		{"just under the bound", teamPostResponseMaxBytes - 1, false},
		{"at the bound", teamPostResponseMaxBytes, false},
		{"over the bound", teamPostResponseMaxBytes + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"`
			suffix := `"}]}}`
			body := prefix + strings.Repeat("u", tc.size-len(prefix)-len(suffix)) + suffix
			require.Len(t, body, tc.size)
			_, sender := startRecordedChorus(t, map[string]http.HandlerFunc{"send": func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, body)
				if err != nil && !tc.error {
					t.Error(err)
				}
			}})
			err := sender.Send(t.Context(), testReminder)
			if tc.error {
				assert.Equal(t, project.TeamSendFailure{Post: "send", Kind: "response"}, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
	// A response whose first bytes up to the bound are already a complete result
	// is still longer than the bound when anything follows: it fails as response.
	for _, tc := range []struct {
		name  string
		size  int
		error bool
	}{
		{"complete result padded to just under the bound", teamPostResponseMaxBytes - 1, false},
		{"complete result padded to the bound", teamPostResponseMaxBytes, false},
		{"complete result padded past the bound", teamPostResponseMaxBytes + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`
			body := result + strings.Repeat(" ", tc.size-len(result))
			require.Len(t, body, tc.size)
			kind := readTeamToolResponse(strings.NewReader(body))
			if tc.error {
				assert.Equal(t, "response", kind)
			} else {
				assert.Empty(t, kind)
			}
		})
	}
	t.Run("reading stops one byte past the bound", func(t *testing.T) {
		endless := &countingReader{}
		assert.Equal(t, "response", readTeamToolResponse(endless))
		assert.Equal(t, teamPostResponseMaxBytes+1, endless.read)
	})
}

// TestTeamReminderPostFailures gives each post, for each kind of failure, a
// TeamSendFailure naming that post and kind with fixed error text. A failed who
// sends no send. A post that hangs returns at the 5-second bound.
func TestTeamReminderPostFailures(t *testing.T) {
	respond := func(t *testing.T, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			if _, err := io.WriteString(w, body); err != nil {
				t.Error(err)
			}
		}
	}
	answers := map[string]func(*testing.T) http.HandlerFunc{
		"timeout": func(*testing.T) http.HandlerFunc {
			return func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
		},
		"http_status": func(*testing.T) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "PRIVATE failure", http.StatusInternalServerError)
			}
		},
		"rpc_error": func(t *testing.T) http.HandlerFunc {
			return respond(t, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"PRIVATE unsupported protocol version"}}`)
		},
		"tool_error": func(t *testing.T) http.HandlerFunc {
			return respond(t, `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"PRIVATE unknown agent"}]}}`)
		},
		"response": func(t *testing.T) http.HandlerFunc { return respond(t, "PRIVATE not JSON") },
		"connection": func(t *testing.T) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) {
				hijacker, ok := w.(http.Hijacker)
				if !ok {
					t.Error("recorded server cannot break a connection")
					return
				}
				conn, _, err := hijacker.Hijack()
				if err == nil {
					err = conn.Close()
				}
				if err != nil {
					t.Error(err)
				}
			}
		},
	}
	for _, post := range []string{"who", "send"} {
		for _, kind := range []string{"connection", "timeout", "http_status", "rpc_error", "tool_error", "response"} {
			t.Run(post+" "+kind, func(t *testing.T) {
				t.Parallel()
				chorus, sender := startRecordedChorus(t, map[string]http.HandlerFunc{post: answers[kind](t)})
				if post == "who" && kind == "connection" {
					// A closed listener refuses the connection outright.
					closed := httptest.NewServer(chorus)
					closed.Close()
					sender.chorusURL = closed.URL
				}
				started := time.Now()
				err := sender.Send(t.Context(), testReminder)
				elapsed := time.Since(started)
				var failure project.TeamSendFailure
				require.True(t, errors.As(err, &failure), "%v", err)
				assert.Equal(t, project.TeamSendFailure{Post: post, Kind: kind}, failure)
				assert.Equal(t, "team reminder post failed", err.Error())
				if kind == "timeout" {
					assert.GreaterOrEqual(t, elapsed, teamPostTimeout)
					assert.Less(t, elapsed, teamPostTimeout+2*time.Second)
				} else {
					assert.Less(t, elapsed, teamPostTimeout)
				}
				if post == "who" {
					assert.NotContains(t, chorus.tools(), "send", "a failed who sends no send")
				}
			})
		}
	}
}

// TestTeamReminderProductionBinding binds the Chorus sender, on a client of its
// own bounded at 5 seconds per post that follows no redirect, to the same local
// Chorus server the view reader uses.
func TestTeamReminderProductionBinding(t *testing.T) {
	deps, err := projectTeamDependencies(t.Context())
	require.NoError(t, err)
	sender, ok := deps.Sender.(teamChorusSender)
	require.True(t, ok, "production binds %T", deps.Sender)
	assert.Equal(t, "http://127.0.0.1:9440", sender.chorusURL)
	assert.Equal(t, 5*time.Second, sender.client.Timeout)
	require.NotNil(t, sender.client.CheckRedirect)
	assert.ErrorIs(t, sender.client.CheckRedirect(nil, nil), http.ErrUseLastResponse)
	observer, ok := deps.Observer.(teamObservations)
	require.True(t, ok)
	assert.Equal(t, sender.chorusURL, observer.chorusURL)
	assert.NotSame(t, observer.client, sender.client, "the sender's bound is its own")
}

// TestTeamCommandsNeverType drives every Herdr adapter operation through a
// recording stand-in: no argument list ever starts with send or keys, and
// neither adapter has an operation that could type into a terminal.
func TestTeamCommandsNeverType(t *testing.T) {
	var mu sync.Mutex
	var calls [][]string
	record := func(args []string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, slices.Clone(args))
	}
	commands := teamCommands{core: "recorded-core", app: "recorded-app", run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		record(args)
		switch {
		case args[0] == "ls" && len(args) > 1:
			return []byte(`{"terminals":[{"address":"owned"}]}`), nil
		case args[0] == "ls":
			return []byte(`{"spaces":[]}`), nil
		case args[0] == "status":
			return []byte(`{"overlays":[]}`), nil
		case args[0] == "do" && args[1] == "space.new":
			return []byte(`{"space":"s","tab":"t","terminal":"owned"}`), nil
		}
		return []byte(`{"address":"owned","state":"live","labels":{}}`), nil
	}}
	observations := teamObservations{commands: commands, stream: func(ctx context.Context, _ string, args []string, _ func(io.Reader) error) error {
		record(args)
		return errors.New("recorded stream ended")
	}}
	ctx := t.Context()
	selection := project.TeamSelection{Root: "/project", Space: "s", Roles: []project.TeamRole{{Role: "builder", Handle: "builder"}}}
	_, err := commands.Spaces(ctx)
	assert.NoError(t, err)
	space, err := commands.CreateSpace(ctx, "team", "/project")
	assert.NoError(t, err)
	_, err = commands.Terminals(ctx, "/project", "s", []string{"builder"})
	assert.NoError(t, err)
	assert.NoError(t, commands.Focus(ctx, space))
	_, err = commands.Overlays(ctx)
	assert.NoError(t, err)
	assert.NoError(t, commands.OpenLauncher(ctx))
	_, err = observations.FindTerminals(ctx, selection)
	assert.NoError(t, err)
	_, err = observations.ReadTerminal(ctx, "owned")
	assert.NoError(t, err)
	assert.Error(t, observations.Follow(ctx, selection, func(project.TeamEvent) {}))
	require.NotEmpty(t, calls)
	for _, args := range calls {
		require.NotEmpty(t, args)
		assert.NotContains(t, []string{"send", "keys"}, args[0], "%v", args)
	}
	assert.ElementsMatch(t, []string{"CreateSpace", "Focus", "OpenLauncher", "Overlays", "Spaces", "Terminals"}, exportedMethods(reflect.TypeFor[teamCommands]()))
	assert.ElementsMatch(t, []string{"FindTerminals", "Follow", "ReadTerminal", "ReadWatchers"}, exportedMethods(reflect.TypeFor[teamObservations]()))
}

// exportedMethods names a type's exported methods, so a new adapter operation
// cannot appear without this test listing it.
func exportedMethods(shape reflect.Type) []string {
	var names []string
	for method := range shape.Methods() {
		names = append(names, method.Name)
	}
	return names
}

// assertNoTextFields fails when a type, or any type it holds, has a field that
// could keep response text: a string, byte slice, raw message or interface.
func assertNoTextFields(t *testing.T, shape reflect.Type) {
	t.Helper()
	for shape.Kind() == reflect.Pointer || shape.Kind() == reflect.Slice {
		assert.NotEqual(t, reflect.TypeFor[[]byte](), shape)
		shape = shape.Elem()
	}
	switch shape.Kind() {
	case reflect.Struct:
		for field := range shape.Fields() {
			if shape == reflect.TypeFor[project.TeamSendFailure]() {
				// Post and Kind are code-chosen enum values, never response text.
				assert.Contains(t, []string{"Post", "Kind"}, field.Name)
				continue
			}
			assertNoTextFields(t, field.Type)
		}
	case reflect.String, reflect.Interface, reflect.Map:
		t.Errorf("%s can keep response text", shape)
	}
}
