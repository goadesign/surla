package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closeProbe observes the invariant at the actual listener-close boundary.
type closeProbe struct {
	net.Listener
	beforeClose func()
	accepted    chan struct{}
	failAccept  chan struct{}
	closed      chan struct{}
	once        sync.Once
	accepts     int
}

// recordWaitContext exposes entry into the file-availability wait without a timed sleep.
type recordWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

// stopResponseGate makes response drainage observable after listener revocation.
type stopResponseGate struct {
	http.ResponseWriter
	closed <-chan struct{}
}

func (w stopResponseGate) WriteHeader(status int) {
	<-w.closed
	w.ResponseWriter.WriteHeader(status)
}

func (c *recordWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func (l *closeProbe) Accept() (net.Conn, error) {
	if l.failAccept == nil {
		return l.Listener.Accept()
	}
	if l.accepts == 0 {
		l.accepts++
		conn, err := l.Listener.Accept()
		if err == nil {
			close(l.accepted)
		}
		return conn, err
	}
	select {
	case <-l.failAccept:
		return nil, errors.New("injected fatal accept failure")
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *closeProbe) Close() error {
	l.once.Do(func() {
		l.beforeClose()
		if l.closed != nil {
			close(l.closed)
		}
	})
	return l.Listener.Close()
}

// TestProjectListenerRevokesBeforeClose exercises the real HTTP lifecycle with injected
// accept failure and close observation, using only this test's private project and listener.
func TestProjectListenerRevokesBeforeClose(t *testing.T) {
	for _, scenario := range []string{"requested stop", "handled signal", "fatal accept", "publication failure", "withdrawal failure"} {
		t.Run(scenario, func(t *testing.T) {
			isolateServeWorkspace(t)
			state, err := fixtureProjectState("")
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			management, err := project.NewServer(state, cancel, fixtureTeamDependencies(state))
			require.NoError(t, err)
			listener, err := net.Listen("tcp", state.Address())
			require.NoError(t, err)
			probe := &closeProbe{Listener: listener}
			owned := &ownedProjectListener{Listener: probe, management: management}
			var old atomic.Value
			old.Store("")
			closed := make(chan struct{})
			probe.beforeClose = func() {
				request := httptest.NewRequest(http.MethodGet, state.Origin()+"/project/identity", nil)
				request.Header.Set("Authorization", "Bearer "+old.Load().(string))
				_, authErr := management.Authenticate(request)
				assert.Error(t, authErr, "revocation must precede the real listener close")
				if scenario != "withdrawal failure" {
					_, recordErr := state.ReadRunning()
					assert.ErrorIs(t, recordErr, os.ErrNotExist)
				}
				close(closed)
			}
			if scenario == "fatal accept" {
				probe.accepted = make(chan struct{})
				probe.failAccept = make(chan struct{})
				probe.closed = make(chan struct{})
			}
			if scenario == "publication failure" {
				// A directory at the record name makes the atomic rename fail.
				require.NoError(t, management.PublishRunning(os.Getpid(), "test"))
				running, err := state.ReadRunning()
				require.NoError(t, err)
				old.Store(running.ManagementCredential)
				require.NoError(t, state.RemoveRunning())
				require.NoError(t, os.Mkdir(filepath.Join(state.Dir, "running.json"), 0o700))
			}
			handler := management.Handler(http.NotFoundHandler())
			if scenario == "requested stop" {
				inner := handler
				handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					inner.ServeHTTP(stopResponseGate{ResponseWriter: w, closed: closed}, r)
				})
			}
			server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
			done := make(chan error, 1)
			go func() { done <- serveProjectHTTP(ctx, server, owned) }()
			if scenario != "publication failure" {
				waitCtx, stop := context.WithTimeout(t.Context(), time.Second)
				running, err := waitForRunning(waitCtx, state)
				stop()
				require.NoError(t, err)
				old.Store(running.ManagementCredential)
			}
			switch scenario {
			case "requested stop":
				// Generated Stop must receive its Accepted response while Shutdown drains it.
				stopCtx, stop := context.WithTimeout(t.Context(), time.Second)
				require.NoError(t, projectClient(state).Stop(stopCtx))
				stop()
			case "fatal accept":
				conn, err := net.DialTimeout("tcp", state.Address(), time.Second)
				require.NoError(t, err)
				defer func() { assert.NoError(t, conn.Close()) }()
				select {
				case <-probe.accepted:
				case <-time.After(time.Second):
					t.Fatal("connection not accepted")
				}
				close(probe.failAccept)
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("listener did not close")
				}
				require.NoError(t, conn.SetDeadline(time.Now().Add(time.Second)))
				_, err = fmt.Fprintf(conn, "GET /project/identity HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nConnection: close\r\n\r\n", state.Address(), old.Load().(string))
				require.NoError(t, err)
				response, err := http.ReadResponse(bufio.NewReader(conn), nil)
				if err != nil {
					assert.True(t, errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF),
						"shutdown may close a preaccepted connection without sending an HTTP response")
				} else {
					assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
					require.NoError(t, response.Body.Close())
				}
			case "withdrawal failure":
				path := filepath.Join(state.Dir, "running.json")
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Mkdir(path, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(path, "blocker"), nil, 0o600))
				cancel()
			case "handled signal":
				cancel()
			}
			select {
			case err := <-done:
				if scenario == "fatal accept" {
					assert.ErrorContains(t, err, "injected fatal accept failure")
				} else if scenario == "publication failure" || scenario == "withdrawal failure" {
					assert.Error(t, err)
				} else {
					assert.NoError(t, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("server lifecycle did not finish")
			}
			select {
			case <-closed:
			default:
				t.Fatal("underlying listener was not closed")
			}
			request := httptest.NewRequest(http.MethodGet, state.Origin()+"/project/identity", nil)
			request.Header.Set("Authorization", "Bearer "+old.Load().(string))
			_, err = management.Authenticate(request)
			assert.Error(t, err)
		})
	}
}

// TestManagementClientRefreshesOnlyChangedRefusals proves a refusal cannot create a resend loop.
func TestManagementClientRefreshesOnlyChangedRefusals(t *testing.T) {
	for _, scenario := range []string{"unchanged", "changed", "changed then refused", "changed then malformed", "fresh next request"} {
		t.Run(scenario, func(t *testing.T) {
			isolateServeWorkspace(t)
			state, err := fixtureProjectState("")
			require.NoError(t, err)
			first, second := strings.Repeat("a", 64), strings.Repeat("b", 64)
			require.NoError(t, state.SaveRunning(project.Running{ManagementCredential: first}))
			var requests atomic.Int32
			var firstPeer string
			build := testBuild(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := requests.Add(1)
				if attempt == 1 {
					firstPeer = r.RemoteAddr
				} else {
					assert.NotEqual(t, firstPeer, r.RemoteAddr, "each credential read must precede a fresh connection")
				}
				w.Header().Set("Content-Type", "application/json")
				if attempt == 1 && scenario != "fresh next request" {
					if scenario != "unchanged" {
						assert.NoError(t, state.SaveRunning(project.Running{ManagementCredential: second}))
					}
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if scenario == "changed then refused" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if scenario == "changed then malformed" {
					conn, writer, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					defer func() { assert.NoError(t, conn.Close()) }()
					_, err = fmt.Fprintf(writer, "HTTP/1.1 %s peer-must-never-print\r\n\r\n", second)
					assert.NoError(t, err)
					assert.NoError(t, writer.Flush())
					return
				}
				expected := first
				if attempt > 1 {
					expected = second
				}
				assert.True(t, r.Header.Get("Authorization") == "Bearer "+expected)
				if scenario == "changed" {
					w.WriteHeader(http.StatusCreated)
					_, err := io.WriteString(w, `{"url":"http://127.0.0.1/views#link=value","expires_at":"2026-11-13T00:00:00Z"}`)
					assert.NoError(t, err)
					return
				}
				_, err := fmt.Fprintf(w, `{"project_id":%q,"directory":%q,"owner":%q,"build":%q}`, state.Record.ID, state.Record.Directory, state.Record.Owner, build)
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			state.Record.Port = server.Listener.Addr().(*net.TCPAddr).Port
			client := projectClient(state)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if scenario == "changed" {
				_, err = client.BrowserLink(ctx)
				require.NoError(t, err)
			} else {
				_, err = client.Identity(ctx)
				if scenario == "fresh next request" {
					require.NoError(t, err)
					require.NoError(t, state.SaveRunning(project.Running{ManagementCredential: second}))
					_, err = client.Identity(ctx)
					require.NoError(t, err)
				} else {
					assert.Error(t, err)
					if scenario == "changed then malformed" {
						assert.ErrorContains(t, err, "project server connection failed")
						assert.NotContains(t, err.Error(), "HTTP 401")
						assert.False(t, strings.Contains(err.Error(), second))
						assert.False(t, strings.Contains(err.Error(), "peer-must-never-print"))
					}
				}
			}
			expected := int32(2)
			if scenario == "unchanged" {
				expected = 1
			}
			assert.Equal(t, expected, requests.Load())
		})
	}
}

// TestManagementWaitsForRecord bounds file availability without contacting the saved port.
//
// The deadlines express contracts, not latency. Without a record the wait must end at the
// short client deadline and report that management access was never published. With a record
// that appears while the client waits, the client must make exactly one request and succeed;
// its deadline is only a hang detector, so it is far above the 50 ms record poll, one file
// write, and one HTTP round trip even on a loaded race-detector runner.
func TestManagementWaitsForRecord(t *testing.T) {
	for _, tc := range []struct {
		available bool
		deadline  time.Duration
	}{
		{available: false, deadline: 150 * time.Millisecond},
		{available: true, deadline: 10 * time.Second},
	} {
		available := tc.available
		t.Run(fmt.Sprint(available), func(t *testing.T) {
			isolateServeWorkspace(t)
			state, err := fixtureProjectState("")
			require.NoError(t, err)
			var requests atomic.Int32
			build := testBuild(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, err := fmt.Fprintf(w, `{"project_id":%q,"directory":%q,"owner":%q,"build":%q}`, state.Record.ID, state.Record.Directory, state.Record.Owner, build)
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			state.Record.Port = server.Listener.Addr().(*net.TCPAddr).Port
			ctx, cancel := context.WithTimeout(t.Context(), tc.deadline)
			defer cancel()
			waitContext := &recordWaitContext{Context: ctx, waiting: make(chan struct{})}
			done := make(chan error, 1)
			go func() { _, err := projectClient(state).Identity(waitContext); done <- err }()
			if available {
				select {
				case <-waitContext.waiting:
				case <-time.After(5 * time.Second):
					t.Fatal("file availability wait was not entered")
				}
				assert.Zero(t, requests.Load())
				require.NoError(t, state.SaveRunning(project.Running{ManagementCredential: strings.Repeat("a", 64)}))
			}
			select {
			case err := <-done:
				if available {
					assert.NoError(t, err)
					assert.EqualValues(t, 1, requests.Load())
				} else {
					assert.ErrorContains(t, err, "has not published management access")
					assert.Zero(t, requests.Load())
				}
			case <-time.After(tc.deadline + 5*time.Second):
				t.Fatal("record wait was not bounded")
			}
		})
	}
}

// TestManagementConnectionIsLocalAndProxyFree checks the production client configuration
// and its generated request consumer with an environment proxy and redirect trap.
func TestManagementConnectionIsLocalAndProxyFree(t *testing.T) {
	isolateServeWorkspace(t)
	state, err := fixtureProjectState("")
	require.NoError(t, err)
	var proxyCalls, serverCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	client := projectHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	assert.Nil(t, transport.Proxy)
	assert.True(t, transport.DisableKeepAlives)
	assert.False(t, transport.ForceAttemptHTTP2)
	assert.True(t, transport.Protocols.HTTP1())
	assert.False(t, transport.Protocols.HTTP2())
	assert.False(t, transport.Protocols.UnencryptedHTTP2())
	t.Cleanup(client.CloseIdleConnections)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverCalls.Add(1)
		assert.Equal(t, state.Address(), r.Host)
		assert.True(t, strings.HasPrefix(r.Host, "127.0.0.1:"))
		w.Header().Set("Location", proxy.URL)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)
	state.Record.Port = server.Listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, state.SaveRunning(project.Running{ManagementCredential: strings.Repeat("a", 64)}))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err = projectClient(state).Identity(ctx)
	assert.Error(t, err)
	assert.EqualValues(t, 1, serverCalls.Load())
	assert.Zero(t, proxyCalls.Load(), "neither environment proxies nor redirects receive a management request")
}

// TestManagementRetryDoesNotReuseFormerPortOwner keeps the former peer's accepted
// connection alive after it relinquishes the port. A replacement's live credential
// must travel only over a fresh connection opened after publication is read.
func TestManagementRetryDoesNotReuseFormerPortOwner(t *testing.T) {
	isolateServeWorkspace(t)
	state, err := fixtureProjectState("")
	require.NoError(t, err)
	oldServer, err := project.NewServer(state, func() {}, fixtureTeamDependencies(state))
	require.NoError(t, err)
	require.NoError(t, oldServer.PublishRunning(os.Getpid(), "old"))
	old, err := state.ReadRunning()
	require.NoError(t, err)
	require.NoError(t, oldServer.RevokeManagement())
	// A crash can leave the now-powerless record for the next CLI invocation.
	require.NoError(t, state.SaveRunning(old))
	former, err := net.Listen("tcp", state.Address())
	require.NoError(t, err)
	deadline := time.Now().Add(2 * time.Second)
	require.NoError(t, former.(*net.TCPListener).SetDeadline(deadline))
	requestCtx, stopRequest := context.WithDeadline(t.Context(), deadline)
	defer stopRequest()
	done := make(chan error, 1)
	go func() { _, err := confirmProject(requestCtx, state, projectClient(state)); done <- err }()
	conn, err := former.Accept()
	require.NoError(t, err)
	defer func() { assert.NoError(t, conn.Close()) }()
	require.NoError(t, conn.SetDeadline(deadline))
	reader := bufio.NewReader(conn)
	first, err := http.ReadRequest(reader)
	require.NoError(t, err)
	assert.True(t, first.Header.Get("Authorization") == "Bearer "+old.ManagementCredential)
	require.NoError(t, first.Body.Close())
	require.NoError(t, former.Close(), "the accepted connection deliberately stays open")

	listener, err := net.Listen("tcp", state.Address())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	replacement, err := project.NewServer(state, cancel, fixtureTeamDependencies(state))
	require.NoError(t, err)
	server := &http.Server{Handler: replacement.Handler(http.NotFoundHandler()), ReadHeaderTimeout: time.Second}
	serving := make(chan error, 1)
	go func() {
		serving <- serveProjectHTTP(ctx, server, &ownedProjectListener{Listener: listener, management: replacement})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serving:
			assert.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Error("replacement did not shut down")
		}
	})
	// The old record still exists until atomic replacement; wait for the new key.
	require.Eventually(t, func() bool {
		current, err := state.ReadRunning()
		return err == nil && current.ManagementCredential != old.ManagementCredential
	}, time.Second, time.Millisecond)
	_, err = io.WriteString(conn, "HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n")
	require.NoError(t, err)
	second, readErr := http.ReadRequest(reader)
	assert.True(t, second == nil, "the former peer must never receive another management request")
	assert.ErrorIs(t, readErr, io.EOF, "management transport must close the accepted connection")
	select {
	case err := <-done:
		assert.NoError(t, err, "changed-key retry must reach the replacement on its new connection")
	case <-time.After(2 * time.Second):
		t.Fatal("management retry did not finish")
	}
}

// TestManagementCredentialStaysOutOfMetadata uses an owned hc executable stand-in;
// no terminal or browser is started while inspecting the actual launcher arguments.
// The launcher hands the server the state folder this command found, and no credential.
// The configuration folder's name contains a space, as macOS's Application Support does,
// and the folder must still arrive as one argument.
func TestManagementCredentialStaysOutOfMetadata(t *testing.T) {
	isolateServeWorkspace(t)
	spaced := filepath.Join(t.TempDir(), "Application Support")
	require.NoError(t, os.Mkdir(spaced, 0o700))
	t.Setenv("SURLA_TEST_USER_CONFIG_DIR", spaced)
	state, err := fixtureProjectState("")
	require.NoError(t, err)
	require.Contains(t, state.Dir, " ")
	management, err := project.NewServer(state, func() {}, fixtureTeamDependencies(state))
	require.NoError(t, err)
	require.NoError(t, management.PublishRunning(os.Getpid(), "test"))
	running, err := state.ReadRunning()
	require.NoError(t, err)
	agent, err := management.StartAgents(t.Context())
	require.NoError(t, err)
	roster, err := json.Marshal(state.Record)
	require.NoError(t, err)
	settings, err := json.Marshal(agent)
	require.NoError(t, err)
	bin := t.TempDir()
	arguments := filepath.Join(bin, "arguments")
	require.NoError(t, os.WriteFile(filepath.Join(bin, "hc"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SURLA_TEST_HC_ARGS\"\nprintf '{\"address\":\"stand-in\"}'\n"), 0o700))
	t.Setenv("PATH", bin)
	t.Setenv("SURLA_TEST_HC_ARGS", arguments)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	terminal, err := startProjectTerminal(ctx, state, func() (string, error) { return filepath.Join(bin, "hc"), nil })
	require.NoError(t, err)
	assert.Equal(t, "stand-in", terminal)
	argv, err := os.ReadFile(arguments)
	require.NoError(t, err)
	launched := strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n")
	require.GreaterOrEqual(t, len(launched), 3)
	assert.Equal(t, []string{"serve", "-state", state.Dir}, launched[len(launched)-3:])
	var output bytes.Buffer
	writeServeStatus(&output, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: state.Record.Port}, controlplane.Options{CardsDir: state.CardsDir()})
	for _, value := range [][]byte{roster, settings, argv, output.Bytes()} {
		assert.False(t, bytes.Contains(value, []byte(running.ManagementCredential)), "management credential must remain private")
	}
}
