// These recorded transport cases bound the watcher prefix without consuming conversation data.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/goadesign/surla/internal/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type (
	// watcherBodyFixture counts application reads and closes without a live connection.
	watcherBodyFixture struct {
		reader io.Reader
		bytes  int
		closed bool
	}
	// watcherTransportFixture supplies one captured response without calling a service.
	watcherTransportFixture struct {
		body   *watcherBodyFixture
		status int
	}
)

// Read counts only bytes requested by the application prefix reader.
func (f *watcherBodyFixture) Read(p []byte) (int, error) {
	n, err := f.reader.Read(p)
	f.bytes += n
	return n, err
}

// Close records early release without draining the conversation suffix.
func (f *watcherBodyFixture) Close() error { f.closed = true; return nil }

// RoundTrip returns the native response shape: quoted ETag header, no body etag.
func (f watcherTransportFixture) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: f.status, Header: http.Header{"Etag": []string{`"version"`}}, Body: f.body, Request: r}, nil
}

// watcherPrefixObservation binds a recorded body and registered watcher selection.
func watcherPrefixObservation(t *testing.T, body string, status int) (project.TeamWatcherView, error, *watcherBodyFixture) {
	t.Helper()
	f := &watcherBodyFixture{reader: strings.NewReader(body)}
	o := teamObservations{client: &http.Client{Transport: watcherTransportFixture{body: f, status: status}}, chorusURL: "http://127.0.0.1:1"}
	view, err := o.ReadWatchers(t.Context(), "team", `"prior"`, []project.TeamRole{{Role: "guide", Handle: "guide"}})
	return view, err, f
}

// TestTeamWatcherPrefixSkipsConversation preserves a busy team whose conversation
// exceeds the prefix bound and proves not one byte of that suffix is consumed.
func TestTeamWatcherPrefixSkipsConversation(t *testing.T) {
	prefix := `{"swarm":"team","agents":[{"handle":"guide","watching_on_this_server":true,"channels":["general"]}]`
	conversation := `,"channels":[{"messages":[{"text":"` + strings.Repeat("private conversation ", teamViewPrefixMaxBytes/10) + `"}]}],"inboxes":[]}`
	view, err, body := watcherPrefixObservation(t, prefix+conversation, 200)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"guide": true}, view.Held)
	assert.Equal(t, `"version"`, view.Version)
	assert.Equal(t, len(prefix), body.bytes, "decoder must stop exactly at the agents array boundary")
	assert.True(t, body.closed)
	assert.Greater(t, len(conversation), teamViewPrefixMaxBytes)
}

// TestTeamWatcherPrefixByteBoundary tests bytes below/at/above the one-read bound,
// including valid empty facts versus unavailable/incomplete evidence.
func TestTeamWatcherPrefixByteBoundary(t *testing.T) {
	base := `{"swarm":"team","agents":[{"handle":"guide","watching_on_this_server":true}]`
	for _, size := range []int{teamViewPrefixMaxBytes - 1, teamViewPrefixMaxBytes, teamViewPrefixMaxBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			prefix := `{"swarm":"team",` + strings.Repeat(" ", size-len(base)) + `"agents":[{"handle":"guide","watching_on_this_server":true}]`
			require.Len(t, prefix, size)
			view, err, body := watcherPrefixObservation(t, prefix+`,"channels":[],"inboxes":[]}`, 200)
			if size <= teamViewPrefixMaxBytes {
				require.NoError(t, err)
				assert.True(t, view.Held["guide"])
			} else {
				assert.Error(t, err)
				assert.Nil(t, view.Held)
				assert.Empty(t, view.Version)
			}
			assert.LessOrEqual(t, body.bytes, teamViewPrefixMaxBytes)
			assert.True(t, body.closed)
		})
	}
}

// TestTeamWatcherPrefixUnavailable never turns invalid order or incomplete agent
// data into an empty successful result, and never consumes a 304 response body.
func TestTeamWatcherPrefixUnavailable(t *testing.T) {
	empty, err, _ := watcherPrefixObservation(t, `{"swarm":"team","agents":[],"channels":[]}`, 200)
	require.NoError(t, err)
	assert.NotNil(t, empty.Held)
	assert.Empty(t, empty.Held)

	for _, body := range []string{`{"swarm":"team","channels":["PRIVATE"],"agents":[]}`, `{"swarm":"team","agents":[`, `{"swarm":"team","agents":[{"handle":"guide"}]}`, `{"swarm":"another","agents":[]}`} {
		view, err, read := watcherPrefixObservation(t, body, 200)
		assert.Error(t, err)
		assert.Nil(t, view.Held)
		assert.Empty(t, view.Version)
		assert.True(t, read.closed)
		if strings.Contains(body, "channels") {
			assert.Less(t, read.bytes, strings.Index(body, "PRIVATE"), "wrong key order must stop before conversation values")
		}
	}
	view, err, body := watcherPrefixObservation(t, "PRIVATE", 304)
	require.NoError(t, err)
	assert.True(t, view.Unchanged)
	assert.Equal(t, `"prior"`, view.Version)
	assert.Zero(t, body.bytes)
	assert.True(t, body.closed)
}

// TestTeamWatcherPrefixUsesTransportHeader rejects a missing header even if a
// handwritten body claims an etag; the owning generated body has no such field.
func TestTeamWatcherPrefixUsesTransportHeader(t *testing.T) {
	client := &http.Client{Transport: roundTripperNoETag{}}
	o := teamObservations{client: client, chorusURL: "http://127.0.0.1:1"}
	view, err := o.ReadWatchers(context.Background(), "team", "", nil)
	assert.Error(t, err)
	assert.Nil(t, view.Held)
}

// roundTripperNoETag models a malformed response while making no network request.
type roundTripperNoETag struct{}

func (roundTripperNoETag) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"swarm":"team","agents":[],"etag":"invented"}`)), Request: r}, nil
}
