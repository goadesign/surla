package candidate

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedDocker records every docker invocation and answers from a table
// keyed by the first non-flag argument after --host, such as "create".
type scriptedDocker struct {
	// calls holds each invocation's args, in order.
	calls [][]string
	// envs holds each invocation's environment, in order.
	envs [][]string
	// reply returns the stdout, stderr and error for one invocation.
	reply func(args []string) (string, string, error)
}

func (s *scriptedDocker) run(_ context.Context, _ string, args, env []string, stdout, stderr io.Writer) error {
	s.calls = append(s.calls, args)
	s.envs = append(s.envs, env)
	out, errOut, err := s.reply(args)
	_, _ = io.WriteString(stdout, out)
	_, _ = io.WriteString(stderr, errOut)
	return err
}

// verb returns the docker subcommand of args, skipping a leading --host.
func verb(args []string) string {
	if len(args) >= 2 && args[0] == "--host" {
		args = args[2:]
	}
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func newScriptedEngine(reply func(args []string) (string, string, error)) (*Engine, *scriptedDocker) {
	s := &scriptedDocker{reply: reply}
	r := &ContainerRunner{lookPath: func(string) (string, error) { return "/usr/local/bin/docker", nil }, command: s.run}
	return r.Reopen("/usr/local/bin/docker", "unix:///var/run/docker.sock", "/ops/check-work/op1/docker-config"), s
}

// TestEngineCreateReportsOnlyAnAnsweredCreate checks that Create returns nil
// only when docker create exited 0, reports any other exit with the
// engine's text, and neither starts nor removes the container.
func TestEngineCreateReportsOnlyAnAnsweredCreate(t *testing.T) {
	cases := []struct {
		name    string
		stderr  string
		err     error
		wantErr string
	}{
		{name: "created"},
		{name: "engine refuses", stderr: "Error response from daemon: Conflict", err: errors.New("exit status 1"), wantErr: "create container surla-check-op1-0"},
		{name: "connection lost", stderr: "error during connect: EOF", err: errors.New("exit status 1"), wantErr: "error during connect: EOF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, s := newScriptedEngine(func([]string) (string, string, error) {
				return "", tc.stderr, tc.err
			})
			err := e.Create(context.Background(), ContainerSpec{Name: "surla-check-op1-0", Operation: "op1", Network: "none", User: "501:20", WorkDir: "/w"})
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
			require.Len(t, s.calls, 1, "Create neither starts nor removes")
			assert.Equal(t, "create", verb(s.calls[0]))
		})
	}
}

// TestEngineCreateIsNotAbandonedWhenItsContextEnds cancels the caller's
// context while create is in flight and checks that the docker command's own
// context stays live, so the engine's answer is still observed.
func TestEngineCreateIsNotAbandonedWhenItsContextEnds(t *testing.T) {
	// The operation is canceled while create is in flight: the docker
	// command keeps its context, so the engine's answer is still observed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var during error
	r := &ContainerRunner{command: func(cmdCtx context.Context, _ string, _, _ []string, _, _ io.Writer) error {
		cancel()
		during = cmdCtx.Err()
		return nil
	}}
	e := r.Reopen("/usr/local/bin/docker", "unix:///var/run/docker.sock", "/ops/check-work/op1/docker-config")
	require.NoError(t, e.Create(ctx, ContainerSpec{Name: "surla-check-op1-0", Operation: "op1", Network: "none", User: "501:20", WorkDir: "/w"}))
	assert.NoError(t, during, "the create kept its context after the operation's ended")
}

// TestAnswerContextEndsGraceAfterItsParent checks the bound on an answered
// request: it outlives its parent's cancellation within the grace, ends when
// stopped, and ends on its own once the grace has passed.
func TestAnswerContextEndsGraceAfterItsParent(t *testing.T) {
	t.Run("outlives its parent within the grace", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		answer, stop := answerContext(parent, time.Hour)
		cancel()
		assert.NoError(t, answer.Err())
		stop()
		assert.ErrorIs(t, answer.Err(), context.Canceled, "stop ends it")
	})
	t.Run("ends once the grace has passed", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		answer, stop := answerContext(parent, time.Millisecond)
		defer stop()
		cancel()
		select {
		case <-answer.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("the answer context outlived its grace")
		}
	})
}

// TestEngineStartReportsTheContainersExit checks that Start returns the exit
// code docker wait reports, separates a broken output stream from a test
// failure, stops on cancellation, and never creates or removes.
func TestEngineStartReportsTheContainersExit(t *testing.T) {
	cases := []struct {
		name      string
		startErr  error
		waitCode  int
		waitErr   error
		cancel    bool
		wantCode  int
		wantErr   string
		wantCalls []string
	}{
		{name: "passes", wantCode: 0, wantCalls: []string{"start", "wait"}},
		{name: "test failure exit code comes from wait", startErr: exitError(t, 1), waitCode: 1, wantCode: 1, wantCalls: []string{"start", "wait"}},
		{name: "wait fails", waitErr: errors.New("exit status 1"), wantCode: -1, wantErr: "wait for container", wantCalls: []string{"start", "wait"}},
		{name: "canceled during start", startErr: errors.New("signal: killed"), cancel: true, wantCode: -1, wantErr: "context canceled", wantCalls: []string{"start"}},
		{name: "stream breaks and the container exited 0", startErr: errors.New("unexpected EOF"), wantCode: -1, wantErr: "stream the output of container surla-check-op1-0, which exited 0", wantCalls: []string{"start", "wait"}},
		{name: "start fails differently from the container's exit", startErr: exitError(t, 1), waitCode: 2, wantCode: -1, wantErr: "which exited 2", wantCalls: []string{"start", "wait"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e, s := newScriptedEngine(func(args []string) (string, string, error) {
				switch verb(args) {
				case "start":
					if tc.cancel {
						cancel()
					}
					return "=== RUN TestX\n", "", tc.startErr
				case "wait":
					return fmt.Sprintf("%d\n", tc.waitCode), "", tc.waitErr
				}
				return "", "", nil
			})
			var out bytes.Buffer
			code, err := e.Start(ctx, "surla-check-op1-0", &out)
			assert.Equal(t, tc.wantCode, code)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
			var verbs []string
			for _, c := range s.calls {
				verbs = append(verbs, verb(c))
			}
			assert.Equal(t, tc.wantCalls, verbs, "Start never creates or removes")
			assert.Equal(t, "=== RUN TestX\n", out.String())
		})
	}
}

// exitError returns the *exec.ExitError of a trusted shell exiting with
// code, the type a docker command's exit status arrives as.
func exitError(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("/bin/sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	return err
}

func TestMountOptionKeepsCommaAndQuotePathsAsOneField(t *testing.T) {
	cases := []struct {
		name  string
		mount Mount
	}{
		{name: "comma in source and target", mount: Mount{Source: "/Users/dev/shop,old", Target: "/Users/dev/shop,old"}},
		{name: "quote in source", mount: Mount{Source: `/Users/dev/"quoted"`, Target: "/w", ReadOnly: true}},
		{name: "plain paths are unchanged", mount: Mount{Source: "/ops/src", Target: "/w"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opt, err := mountOption(tc.mount)
			require.NoError(t, err)
			// Docker reads the value with encoding/csv as one record.
			fields, err := csv.NewReader(strings.NewReader(opt)).Read()
			require.NoError(t, err)
			want := []string{"type=bind", "source=" + tc.mount.Source, "target=" + tc.mount.Target}
			if tc.mount.ReadOnly {
				want = append(want, "readonly")
			}
			assert.Equal(t, want, fields)
		})
	}
	opt, err := mountOption(Mount{Source: "/ops/src", Target: "/w"})
	require.NoError(t, err)
	assert.Equal(t, "type=bind,source=/ops/src,target=/w", opt)
}

// TestEngineRemoveReportsUnconfirmedCleanup checks that a failed docker rm
// --force is a *CleanupError naming the container to remove by hand.
func TestEngineRemoveReportsUnconfirmedCleanup(t *testing.T) {
	e, _ := newScriptedEngine(func([]string) (string, string, error) {
		return "", "Cannot connect to the Docker daemon", errors.New("exit status 1")
	})
	err := e.Remove("surla-check-op1-0")
	var cleanup *CleanupError
	require.ErrorAs(t, err, &cleanup)
	assert.Equal(t, "surla-check-op1-0", cleanup.Name)
	assert.Contains(t, err.Error(), "docker rm -f surla-check-op1-0")
}

// TestEngineRemoveNeverReadsItsOutput checks that Remove's result depends on
// docker rm's exit status alone: its text is never read as evidence that a
// container is gone.
func TestEngineRemoveNeverReadsItsOutput(t *testing.T) {
	cases := []struct {
		name    string
		stderr  string
		err     error
		wantErr bool
	}{
		// docker rm --force can exit 0 for a container that does not
		// exist: exit 0 only says the command ran.
		{name: "exit 0", wantErr: false},
		{name: "a failure whose text looks like absence is still a failure", stderr: "Error response from daemon: No such container: surla-check-op1-0", err: errors.New("exit status 1"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, s := newScriptedEngine(func([]string) (string, string, error) {
				return "", tc.stderr, tc.err
			})
			err := e.Remove("surla-check-op1-0")
			if tc.wantErr {
				var cleanup *CleanupError
				require.ErrorAs(t, err, &cleanup)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, []string{"--host", "unix:///var/run/docker.sock", "rm", "--force", "surla-check-op1-0"}, s.calls[0])
		})
	}
}

// TestEngineListReturnsTheLabelledNames checks the exact listing List sends
// and that it returns every listed name, and an error that proves nothing
// when the listing fails.
func TestEngineListReturnsTheLabelledNames(t *testing.T) {
	cases := []struct {
		name      string
		stdout    string
		err       error
		wantNames []string
		wantErr   string
	}{
		{name: "none", wantNames: []string{}},
		{name: "created and exited", stdout: "surla-check-op1-0\nsurla-check-op1-1\n", wantNames: []string{"surla-check-op1-0", "surla-check-op1-1"}},
		{name: "several names of one container", stdout: "surla-check-op1-0,alias\n", wantNames: []string{"surla-check-op1-0", "alias"}},
		{name: "failed listing proves nothing", err: errors.New("exit status 1"), wantErr: "list the containers of operation op1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, s := newScriptedEngine(func([]string) (string, string, error) {
				return tc.stdout, "", tc.err
			})
			names, err := e.List("op1")
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Nil(t, names)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantNames, names)
			}
			assert.Equal(t, []string{"--host", "unix:///var/run/docker.sock", "container", "ls", "--all", "--filter", "label=surla.check=op1", "--format", "{{.Names}}"}, s.calls[0])
		})
	}
}

func TestEngineCreateArgsAndPrivateConfig(t *testing.T) {
	e, s := newScriptedEngine(func([]string) (string, string, error) {
		return "", "", nil
	})
	spec := ContainerSpec{
		Name: "surla-check-op1-0", Operation: "op1", Network: "none", User: "501:20",
		WorkDir: "/Users/dev/repo/shop",
		Mounts: []Mount{
			{Source: "/ops/check-work/op1/src", Target: "/Users/dev/repo"},
			{Source: "/ops/check-work/op1/modcache", Target: "/gomodcache", ReadOnly: true},
		},
		Env:  []string{"GOMODCACHE=/gomodcache", "GOPROXY=off"},
		Args: []string{"test", "./..."},
	}
	require.NoError(t, e.Create(context.Background(), spec))
	assert.Equal(t, []string{
		"--host", "unix:///var/run/docker.sock",
		"create",
		"--name", "surla-check-op1-0",
		"--label", "surla.check=op1",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", "4096",
		"--log-driver", "none",
		"--network", "none",
		"--user", "501:20",
		"--entrypoint", "/usr/local/go/bin/go",
		"--workdir", "/Users/dev/repo/shop",
		"--mount", "type=bind,source=/ops/check-work/op1/src,target=/Users/dev/repo",
		"--mount", "type=bind,source=/ops/check-work/op1/modcache,target=/gomodcache,readonly",
		"--env", "GOMODCACHE=/gomodcache",
		"--env", "GOPROXY=off",
		CheckImage,
		"test", "./...",
	}, s.calls[0])
	for _, env := range s.envs {
		require.Len(t, env, 2)
		assert.Equal(t, "DOCKER_CONFIG=/ops/check-work/op1/docker-config", env[0])
		assert.True(t, strings.HasPrefix(env[1], "PATH="))
	}
}

func TestContainerRunnerOpen(t *testing.T) {
	cases := []struct {
		name       string
		dockerHost string
		missing    bool
		context    string
		serverOS   string
		versionErr error
		wantHost   string
		wantErr    string
	}{
		{name: "local context", context: "unix:///Users/dev/.docker/run/docker.sock\n", serverOS: "linux\n", wantHost: "unix:///Users/dev/.docker/run/docker.sock"},
		{name: "DOCKER_HOST wins", dockerHost: "unix:///var/run/docker.sock", serverOS: "linux", wantHost: "unix:///var/run/docker.sock"},
		{name: "docker missing", missing: true, wantErr: "docker is not installed"},
		{name: "remote endpoint", dockerHost: "tcp://build.example:2376", wantErr: "needs a local Docker engine"},
		{name: "engine down", context: "unix:///var/run/docker.sock", versionErr: errors.New("exit status 1"), wantErr: "does not answer"},
		{name: "windows containers", context: "unix:///var/run/docker.sock", serverOS: "windows", wantErr: "not linux"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", tc.dockerHost)
			s := &scriptedDocker{reply: func(args []string) (string, string, error) {
				switch verb(args) {
				case "context":
					return tc.context, "", nil
				case "version":
					return tc.serverOS, "", tc.versionErr
				}
				return "", "", nil
			}}
			r := &ContainerRunner{lookPath: func(string) (string, error) {
				if tc.missing {
					return "", errors.New("executable file not found in $PATH")
				}
				return "/usr/local/bin/docker", nil
			}, command: s.run}
			e, err := r.Open(context.Background(), "/ops/check-work/op1/docker-config")
			if tc.wantErr != "" {
				require.ErrorIs(t, err, ErrEngineUnavailable)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantHost, e.Host())
		})
	}
}

func TestEngineEnsureImage(t *testing.T) {
	cases := []struct {
		name     string
		present  bool
		pullErr  error
		wantPull bool
		wantErr  bool
	}{
		{name: "present", present: true},
		{name: "pulled by digest", wantPull: true},
		{name: "pull fails", wantPull: true, pullErr: errors.New("exit status 1"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, s := newScriptedEngine(func(args []string) (string, string, error) {
				switch verb(args) {
				case "image":
					if tc.present {
						return "", "", nil
					}
					return "", "", errors.New("exit status 1")
				case "pull":
					return "", "", tc.pullErr
				}
				return "", "", nil
			})
			err := e.EnsureImage(context.Background())
			if tc.wantErr {
				require.ErrorIs(t, err, ErrEngineUnavailable)
			} else {
				require.NoError(t, err)
			}
			pulled := false
			for _, c := range s.calls {
				if verb(c) == "pull" {
					pulled = true
					assert.Equal(t, CheckImage, c[len(c)-1])
				}
			}
			assert.Equal(t, tc.wantPull, pulled)
		})
	}
}
