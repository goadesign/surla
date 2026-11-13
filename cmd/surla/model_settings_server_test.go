// This file proves where the project server's provider settings come from:
// serve removes every provider family from its own process at entry, keeps
// the original environment only for Herdr's programs, and its children get
// provider settings only from a hand-over.
package main

import (
	"io"
	"os"
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
