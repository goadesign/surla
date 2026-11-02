package suite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/verify/candidate"
)

// fakeLive is a Command.Live that records what it was given and simulates a
// target whose processes stop when ctx is canceled or when end is closed.
type fakeLive struct {
	// url is the origin the fake target reports.
	url string
	// startErr makes Live fail before the target listens.
	startErr error
	// stopErr is what Done delivers after a requested stop.
	stopErr error
	// end, when closed, makes the target end on its own with endErr.
	end    chan struct{}
	endErr error

	mu sync.Mutex
	// key and pinned record Live's arguments.
	key    string
	pinned map[string]candidate.Artifact
}

// live implements Command.Live.
func (f *fakeLive) live(ctx context.Context, pinned map[string]candidate.Artifact, key string) (LiveTarget, error) {
	f.mu.Lock()
	f.key, f.pinned = key, pinned
	f.mu.Unlock()
	if f.startErr != nil {
		return LiveTarget{}, f.startErr
	}
	done := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			done <- f.stopErr
		case <-f.end:
			done <- f.endErr
		}
	}()
	return LiveTarget{URL: f.url, Done: done}, nil
}

func TestLiveFollowsTheSuiteCommandContract(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "app")
	require.NoError(t, os.WriteFile(bin, []byte("app binary"), 0o700))
	artifact := "--artifact=app=" + bin

	cases := []struct {
		name       string
		args       []string
		key        string
		noLive     bool
		fake       fakeLive
		stopAfter  bool
		endOnOwn   bool
		wantCode   int
		wantReady  bool
		wantStderr string
	}{
		{name: "stopped on request", args: []string{"--live", artifact}, key: "k1", fake: fakeLive{url: "http://127.0.0.1:4100"},
			stopAfter: true, wantCode: ExitPassed, wantReady: true},
		{name: "target ends on its own", args: []string{"--live", artifact}, key: "k1", fake: fakeLive{url: "http://127.0.0.1:4100", endErr: errors.New("candidate exited: status 3")},
			endOnOwn: true, wantCode: ExitError, wantReady: true, wantStderr: "live target ended: candidate exited: status 3"},
		{name: "unclean stop", args: []string{"--live", artifact}, key: "k1", fake: fakeLive{url: "http://127.0.0.1:4100", stopErr: errors.New("gateway did not stop")},
			stopAfter: true, wantCode: ExitError, wantReady: true, wantStderr: "stop live target: gateway did not stop"},
		{name: "suite without Live", args: []string{"--live", artifact}, key: "k1", noLive: true,
			wantCode: ExitError, wantStderr: "this evaluator suite does not support --live"},
		{name: "missing key", args: []string{"--live", artifact}, fake: fakeLive{url: "http://127.0.0.1:4100"},
			wantCode: ExitError, wantStderr: "--live requires the target key in " + LiveKeyEnv},
		{name: "start fails", args: []string{"--live", artifact}, key: "k1", fake: fakeLive{startErr: errors.New("model gateway: no provider key")},
			wantCode: ExitError, wantStderr: "start live target: model gateway: no provider key"},
		{name: "non-loopback URL", args: []string{"--live", artifact}, key: "k1", fake: fakeLive{url: "http://10.0.0.1:4100"},
			wantCode: ExitError, wantStderr: `live target URL "http://10.0.0.1:4100" is not an http://127.0.0.1:<port> origin`},
		{name: "with describe", args: []string{"--live", "--describe"}, key: "k1",
			wantCode: ExitError, wantStderr: "--live cannot be combined with --describe, --scenario or --tag"},
		{name: "with tag", args: []string{"--live", artifact, "--tag", "holdout"}, key: "k1",
			wantCode: ExitError, wantStderr: "--live cannot be combined"},
		{name: "missing artifact", args: []string{"--live"}, key: "k1", fake: fakeLive{url: "http://127.0.0.1:4100"},
			wantCode: ExitError, wantStderr: "--artifact app=<path> is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			t.Setenv(LiveKeyEnv, tc.key)
			defer assertEmptyDir(t, tmp)
			fake := tc.fake
			fake.end = make(chan struct{})
			c := testCommand(false)
			if !tc.noLive {
				c.Live = fake.live
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stdout := &syncBuffer{}
			var stderr bytes.Buffer
			codes := make(chan int, 1)
			go func() { codes <- runCommand(ctx, tc.args, stdout, &stderr, c) }()
			if tc.wantReady {
				require.Eventually(t, func() bool { return strings.Contains(stdout.String(), "\n") }, 5*time.Second, 10*time.Millisecond)
				var ready liveReady
				require.NoError(t, json.Unmarshal([]byte(stdout.String()), &ready))
				assert.Equal(t, tc.fake.url, ready.URL)
				fake.mu.Lock()
				assert.Equal(t, tc.key, fake.key, "Live receives the key from the environment")
				assert.Contains(t, fake.pinned, "app")
				fake.mu.Unlock()
			}
			switch {
			case tc.stopAfter:
				cancel()
			case tc.endOnOwn:
				close(fake.end)
			}
			select {
			case code := <-codes:
				assert.Equal(t, tc.wantCode, code, stderr.String())
			case <-time.After(5 * time.Second):
				t.Fatal("runCommand did not return")
			}
			assert.Contains(t, stderr.String(), tc.wantStderr)
			if !tc.wantReady {
				assert.Empty(t, stdout.String())
			}
		})
	}
}

func TestLiveKeepsTheStoreUntilTheTargetHasStopped(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "app")
	require.NoError(t, os.WriteFile(bin, []byte("app binary"), 0o700))
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv(LiveKeyEnv, "k1")
	release := make(chan struct{})
	storeSeen := make(chan bool, 1)
	c := testCommand(false)
	c.Live = func(ctx context.Context, _ map[string]candidate.Artifact, _ string) (LiveTarget, error) {
		done := make(chan error, 1)
		go func() {
			<-ctx.Done()
			<-release
			// The artifact store must still exist while the target stops.
			entries, err := os.ReadDir(tmp)
			storeSeen <- err == nil && len(entries) == 1
			done <- nil
		}()
		return LiveTarget{URL: "http://127.0.0.1:4100", Done: done}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	stdout := &syncBuffer{}
	var stderr bytes.Buffer
	codes := make(chan int, 1)
	go func() { codes <- runCommand(ctx, []string{"--live", "--artifact", "app=" + bin}, stdout, &stderr, c) }()
	require.Eventually(t, func() bool { return stdout.String() != "" }, 5*time.Second, 10*time.Millisecond)
	cancel()
	close(release)
	assert.Equal(t, ExitPassed, <-codes, stderr.String())
	assert.True(t, <-storeSeen, "the store outlives the target's processes")
	assertEmptyDir(t, tmp)
}

// syncBuffer is a bytes.Buffer safe for one writer and one reader.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns what was written so far.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
