package candidate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

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

func TestEngineRunRemovesContainerOnEveryPath(t *testing.T) {
	cases := []struct {
		name     string
		failVerb string
		cancel   bool
		wantCode int
		wantErr  string
	}{
		{name: "passes", wantCode: 0},
		{name: "test failure exit code comes from wait", wantCode: 1},
		{name: "create fails", failVerb: "create", wantCode: -1, wantErr: "create container"},
		{name: "wait fails", failVerb: "wait", wantCode: -1, wantErr: "wait for container"},
		{name: "canceled during start", failVerb: "start", cancel: true, wantCode: -1, wantErr: "context canceled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e, s := newScriptedEngine(func(args []string) (string, string, error) {
				v := verb(args)
				if v == tc.failVerb {
					if tc.cancel {
						cancel()
					}
					return "", "boom", errors.New("exit status 1")
				}
				switch v {
				case "start":
					return "=== RUN TestX\n", "", nil
				case "wait":
					return fmt.Sprintf("%d\n", tc.wantCode), "", nil
				}
				return "", "", nil
			})
			var out bytes.Buffer
			code, err := e.Run(ctx, ContainerSpec{Name: "surla-check-op1-0", Operation: "op1", Network: "none", User: "501:20", WorkDir: "/w"}, &out)
			assert.Equal(t, tc.wantCode, code)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
			last := s.calls[len(s.calls)-1]
			assert.Equal(t, []string{"--host", "unix:///var/run/docker.sock", "rm", "--force", "surla-check-op1-0"}, last)
		})
	}
}

func TestEngineRunReportsUnconfirmedCleanup(t *testing.T) {
	e, _ := newScriptedEngine(func(args []string) (string, string, error) {
		switch verb(args) {
		case "wait":
			return "0\n", "", nil
		case "rm":
			return "", "Cannot connect to the Docker daemon", errors.New("exit status 1")
		}
		return "", "", nil
	})
	_, err := e.Run(context.Background(), ContainerSpec{Name: "surla-check-op1-0", Network: "none"}, io.Discard)
	var cleanup *CleanupError
	require.ErrorAs(t, err, &cleanup)
	assert.Equal(t, "surla-check-op1-0", cleanup.Name)
	assert.Contains(t, err.Error(), "docker rm -f surla-check-op1-0")
}

func TestEngineRemoveTreatsMissingContainerAsRemoved(t *testing.T) {
	e, _ := newScriptedEngine(func([]string) (string, string, error) {
		return "", "Error response from daemon: No such container: surla-check-op1-0", errors.New("exit status 1")
	})
	assert.NoError(t, e.Remove("surla-check-op1-0"))
}

func TestEngineCreateArgsAndPrivateConfig(t *testing.T) {
	e, s := newScriptedEngine(func(args []string) (string, string, error) {
		if verb(args) == "wait" {
			return "0\n", "", nil
		}
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
	_, err := e.Run(context.Background(), spec, io.Discard)
	require.NoError(t, err)
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
