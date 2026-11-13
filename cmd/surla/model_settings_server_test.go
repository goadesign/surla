// This file proves where the project server's provider settings come from:
// serve removes every provider family from its own process at entry, keeps
// the original environment only for Herdr's programs, and its children get
// provider settings only from a hand-over.
package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/modelsettings"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
)

// coreFamily stands for the provider variables Herdr's core environment may
// hold, which the project server inherits; none is a real credential.
var coreFamily = map[string]string{
	"ANTHROPIC_API_KEY":    "sentinel-core-key",
	"ANTHROPIC_AUTH_TOKEN": "sentinel-core-token",
	"ANTHROPIC_BASE_URL":   "https://core.invalid/sentinel-core-path/",
	"ANTHROPIC_ADMIN_KEY":  "sentinel-core-admin",
	"OPENAI_API_KEY":       "sentinel-core-openai",
}

// presenceNames are the provider variables presence stand-ins report.
var presenceNames = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_ADMIN_KEY", "OPENAI_API_KEY"}

// TestServeEntryRemovesProviderFamiliesAndKeepsHerdrsCopy runs serve's
// entry step with inherited core values: afterwards the process holds none,
// Herdr's own programs (herdr-app, herdr-core and the event reader) still
// receive them unchanged, and the check helper and a verification
// supervisor receive none but the handed-over snapshot.
func TestServeEntryRemovesProviderFamiliesAndKeepsHerdrsCopy(t *testing.T) {
	isolateServeWorkspace(t)
	isolateProviderFamilies(t)
	reports := t.TempDir()
	bin := t.TempDir()
	t.Setenv("SURLA_TEST_PRESENCE_DIR", reports)
	for name, value := range coreFamily {
		t.Setenv(name, value)
	}
	for _, program := range []string{"herdr-app", "herdr-core"} {
		writePresenceStandIn(t, filepath.Join(bin, program), `{"overlays":[]}`)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	herdrEnv, err := takeProviderFamilies()
	require.NoError(t, err)
	for name, value := range coreFamily {
		_, present := os.LookupEnv(name)
		assert.False(t, present, "serve's process keeps no %s", name)
		assert.Contains(t, herdrEnv, name+"="+value, "Herdr's copy keeps %s unchanged", name)
	}

	deps, err := projectTeamDependencies(t.Context(), herdrEnv)
	require.NoError(t, err)
	_, err = deps.Host.Overlays(t.Context())
	require.NoError(t, err)
	// The reader ends the subscriber once its output ends, which it reports as
	// the stream not continuing; the stand-in has reported by then.
	err = runTeamStream(t.Context(), herdrEnv, "herdr-core", []string{"subscribe"}, func(r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err
	})
	assert.EqualError(t, err, "Herdr event reader could not continue")
	allSet := "ANTHROPIC_API_KEY=set\nANTHROPIC_AUTH_TOKEN=set\nANTHROPIC_BASE_URL=set\nANTHROPIC_ADMIN_KEY=set\nOPENAI_API_KEY=set\nARGS_HOLD_SENTINEL=no\n"
	assert.Equal(t, allSet, readPresence(t, reports, "herdr-app"), "herdr-app gets Herdr's environment unchanged")
	assert.Equal(t, allSet, readPresence(t, reports, "herdr-core"), "the event reader gets Herdr's environment unchanged")

	opts, err := serveOptions(os.Environ())
	require.NoError(t, err)
	closeServeOptions(t, opts)
	check := opts.CheckHelper()
	check.Path, check.Args = "/bin/sh", []string{"/bin/sh", "-c", presenceScript, "check-run"}
	require.NoError(t, check.Run())
	none := "ANTHROPIC_API_KEY=\nANTHROPIC_AUTH_TOKEN=\nANTHROPIC_BASE_URL=\nANTHROPIC_ADMIN_KEY=\nOPENAI_API_KEY=\nARGS_HOLD_SENTINEL=no\n"
	assert.Equal(t, none, readPresence(t, reports, "check-run"), "the check helper gets no provider variable")
	assert.Nil(t, check.Env, "the check helper inherits the server's environment")

	handed, err := modelsettings.New("anthropic", map[string]string{"ANTHROPIC_API_KEY": "sentinel-handed-key"})
	require.NoError(t, err)
	runs := t.TempDir()
	runID := verifyrun.NewRunID()
	_, err = verifyrun.Start(runs, verifyrun.Request{
		RunID: runID, Owner: "operator", ChangeID: "chg", AcceptedAt: time.Now().UTC(), Timeout: time.Hour,
		Plan: verify.Plan{WorkDir: filepath.Join(runs, runID, "work"), Tests: verify.TestsEvaluator},
	}, []string{"/bin/sh", "-c", presenceScript, "supervise"}, []modelsettings.Settings{handed})
	require.NoError(t, err)
	require.NoError(t, verifyrun.Wait(runs, runID))
	assert.Equal(t, "ANTHROPIC_API_KEY=handed\nANTHROPIC_AUTH_TOKEN=\nANTHROPIC_BASE_URL=\nANTHROPIC_ADMIN_KEY=\nOPENAI_API_KEY=\nARGS_HOLD_SENTINEL=no\n",
		readPresence(t, reports, "supervise"), "a stored evaluator run gets only the handed-over snapshot")
}

// TestServeProcessKeepsProviderValuesOutOfItsOutputAndFiles runs the real
// serve entry in its own process with inherited core values, hands settings
// over through the surla command's client and stops it: the server's
// process held no inherited value while Herdr's copy kept them all, and
// neither its standard output and error nor any file in the project's state
// folder holds a value.
func TestServeProcessKeepsProviderValuesOutOfItsOutputAndFiles(t *testing.T) {
	isolateServeWorkspace(t)
	isolateProviderFamilies(t)
	reports := t.TempDir()
	state, err := fixtureProjectState("")
	require.NoError(t, err)
	serve := exec.Command(os.Args[0], "serve", "-state", state.Dir)
	serve.Env = append(os.Environ(), "SURLA_TEST_PRESENCE_DIR="+reports)
	for name, value := range coreFamily {
		serve.Env = append(serve.Env, name+"="+value)
	}
	var stdout, stderr bytes.Buffer
	serve.Stdout, serve.Stderr = &stdout, &stderr
	require.NoError(t, serve.Start())
	// exited is closed once serve was reaped; waitErr is then its result.
	exited := make(chan struct{})
	var waitErr error
	go func() { waitErr = serve.Wait(); close(exited) }()
	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			assert.NoError(t, serve.Process.Kill())
			<-exited
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, err = waitForRunning(ctx, state)
	require.NoError(t, err)
	var line bytes.Buffer
	shell := modelsettings.Select([]string{
		"ANTHROPIC_API_KEY=sentinel-handed-key", "ANTHROPIC_BASE_URL=https://gateway.invalid/sentinel-url-credential/",
		"ANTHROPIC_ADMIN_KEY=sentinel-shell-admin",
	})
	require.NoError(t, handOverModelSettings(ctx, state, shell, &line))
	assert.Contains(t, line.String(), "Model settings held by the project server: ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL (from this shell)")
	require.NoError(t, projectClient(state).Stop(ctx))
	select {
	case <-exited:
		require.NoError(t, waitErr, "serve output: %s", stderr.String())
	case <-ctx.Done():
		t.Fatal("serve did not stop")
	}

	assert.Equal(t, "ANTHROPIC_API_KEY=\nANTHROPIC_AUTH_TOKEN=\nANTHROPIC_BASE_URL=\nANTHROPIC_ADMIN_KEY=\nOPENAI_API_KEY=\nARGS_HOLD_SENTINEL=no\n",
		readPresence(t, reports, "server-process"), "serve's process keeps no inherited provider value")
	assert.Equal(t, "ANTHROPIC_API_KEY=set\nANTHROPIC_AUTH_TOKEN=set\nANTHROPIC_BASE_URL=set\nANTHROPIC_ADMIN_KEY=set\nOPENAI_API_KEY=set\nARGS_HOLD_SENTINEL=no\n",
		readPresence(t, reports, "server-herdr"), "Herdr's copy keeps the core's values")
	for _, out := range []string{stdout.String(), stderr.String(), line.String()} {
		assert.NotContains(t, out, "sentinel")
	}
	require.NoError(t, filepath.WalkDir(state.Dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		assert.NotContains(t, string(raw), "sentinel", path)
		return nil
	}))
}

// writePresence writes, to path, whether environ sets each provider variable
// presenceScript reports, in its format; it reports names and booleans only.
func writePresence(path string, environ []string) error {
	var b strings.Builder
	for _, name := range presenceNames {
		state := ""
		for _, kv := range environ {
			if k, v, _ := strings.Cut(kv, "="); k == name && v != "" {
				state = "set"
			}
		}
		b.WriteString(name + "=" + state + "\n")
	}
	b.WriteString("ARGS_HOLD_SENTINEL=no\n")
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// presenceScript reports, to $SURLA_TEST_PRESENCE_DIR/$0, whether each
// provider variable is set (ANTHROPIC_API_KEY reports "handed" for the value
// tests hand over) and whether its arguments hold a sentinel. It reports
// names and booleans only.
const presenceScript = `out="$SURLA_TEST_PRESENCE_DIR/$0"
args=no; for a in "$@"; do case "$a" in *sentinel*) args=yes;; esac; done
key=; if [ "$ANTHROPIC_API_KEY" = sentinel-handed-key ]; then key=handed; elif [ -n "$ANTHROPIC_API_KEY" ]; then key=set; fi
{ printf 'ANTHROPIC_API_KEY=%s\n' "$key"
printf 'ANTHROPIC_AUTH_TOKEN=%s\n' "${ANTHROPIC_AUTH_TOKEN:+set}"
printf 'ANTHROPIC_BASE_URL=%s\n' "${ANTHROPIC_BASE_URL:+set}"
printf 'ANTHROPIC_ADMIN_KEY=%s\n' "${ANTHROPIC_ADMIN_KEY:+set}"
printf 'OPENAI_API_KEY=%s\n' "${OPENAI_API_KEY:+set}"
printf 'ARGS_HOLD_SENTINEL=%s\n' "$args"; } > "$out.tmp" && mv "$out.tmp" "$out"`

// writePresenceStandIn writes an executable at path that runs presenceScript
// under its own base name and then prints reply.
func writePresenceStandIn(t *testing.T, path, reply string) {
	t.Helper()
	script := "#!/bin/sh\n/bin/sh -c '" + strings.ReplaceAll(presenceScript, "'", `'\''`) + "' " + filepath.Base(path) + " \"$@\"\nprintf '%s' '" + reply + "'\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o700))
}

// readPresence returns what a presence stand-in named name reported, waiting
// up to ten seconds for a detached one.
func readPresence(t *testing.T, dir, name string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			return string(raw)
		}
		require.ErrorIs(t, err, os.ErrNotExist)
		require.True(t, time.Now().Before(deadline), "%s did not report", name)
		time.Sleep(20 * time.Millisecond)
	}
}

// isolateProviderFamilies hides the machine's own provider variables from a
// test and restores each when it ends, so no test reads a real key.
func isolateProviderFamilies(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if modelsettings.InFamily(name) {
			t.Setenv(name, value)
			require.NoError(t, os.Unsetenv(name))
		}
	}
}
