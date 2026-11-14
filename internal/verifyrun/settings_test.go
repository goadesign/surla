// This file proves how provider request settings travel through the
// verification and live executors: only the test program receives them,
// preparation and argument lists never do, and the start detail the server
// exports protects them.
package verifyrun

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/modelsettings"
	"github.com/goadesign/surla/internal/verify"
)

// reportedSettings are the provider variables writeSettingsReport reports.
var reportedSettings = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_ADMIN_KEY", "OPENAI_API_KEY", "OPENAI_ORG_ID",
}

// TestLiveExecutorGivesSettingsOnlyToTheTestProgram launches a live target
// with handed-over settings from an environment that also inherited family
// values. Preparation, in the executor's process, sees no provider
// variable; the test program sees exactly the handed-over settings, for the
// operator's evaluator and for written tests alike; no argument list holds a
// value.
func TestLiveExecutorGivesSettingsOnlyToTheTestProgram(t *testing.T) {
	isolateProviderFamilies(t)
	for _, tests := range []verify.TestsSource{verify.TestsEvaluator, verify.TestsWritten} {
		t.Run(string(tests), func(t *testing.T) {
			t.Setenv(liveFakeEnv, "ready")
			self, err := os.Executable()
			require.NoError(t, err)
			env := append(os.Environ(), "ANTHROPIC_ADMIN_KEY=sentinel-inherited-admin", "OPENAI_API_KEY=sentinel-inherited-openai",
				verify.LiveModelProviderEnv+"=anthropic", verify.LiveModelNameEnv+"=claude")
			anthropic, err := modelsettings.New("anthropic", map[string]string{
				"ANTHROPIC_API_KEY": "sentinel-handed-key", "ANTHROPIC_BASE_URL": "https://gateway.invalid/sentinel-url-credential/",
			})
			require.NoError(t, err)
			p, err := startLiveWith(t.Context(), t.TempDir(), verify.Plan{Tests: tests}, "secret-key", []string{self}, env, anthropic)
			require.NoError(t, err)
			prepared, prepErr := os.ReadFile(filepath.Join(p.Dir(), liveTmpDir, "prepare-report"))
			suite, suiteErr := os.ReadFile(filepath.Join(p.Dir(), liveTmpDir, "suite-report"))
			raw, rawErr := os.ReadFile(filepath.Join(p.Dir(), liveRequestFile))
			require.NoError(t, p.Stop())
			require.NoError(t, errors.Join(prepErr, suiteErr, rawErr))
			assert.Equal(t, "ANTHROPIC_API_KEY=\nANTHROPIC_AUTH_TOKEN=\nANTHROPIC_BASE_URL=\nANTHROPIC_ADMIN_KEY=\nOPENAI_API_KEY=\nOPENAI_ORG_ID=\nARGS_HOLD_SENTINEL=no\n",
				string(prepared), "preparation sees no provider variable")
			assert.Equal(t, "ANTHROPIC_API_KEY=handed\nANTHROPIC_AUTH_TOKEN=\nANTHROPIC_BASE_URL=set\nANTHROPIC_ADMIN_KEY=\nOPENAI_API_KEY=\nOPENAI_ORG_ID=\nARGS_HOLD_SENTINEL=no\n",
				string(suite), "the test program sees only the handed-over settings")
			assert.NotContains(t, string(raw), "sentinel", "live.json holds no setting")
		})
	}
}

// TestVerificationExecutorGivesSettingsOnlyToTheJudgingSuite accepts runs
// whose operator suite needs its judge key, through the real supervisor and
// executor: without settings the suite's report does not pass; with the
// handed-over snapshot it passes, while preparation in the executor's
// process never sees a provider variable and no argument list holds one.
func TestVerificationExecutorGivesSettingsOnlyToTheJudgingSuite(t *testing.T) {
	isolateProviderFamilies(t)
	t.Setenv(fakeModeEnv, "judge")
	anthropic, err := modelsettings.New("anthropic", map[string]string{"ANTHROPIC_API_KEY": "sentinel-judge-key", "ANTHROPIC_AUTH_TOKEN": "sentinel-judge-token"})
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		settings []modelsettings.Settings
		passed   bool
		suite    string
	}{
		{name: "no settings", passed: false,
			suite: "ANTHROPIC_API_KEY=\nANTHROPIC_AUTH_TOKEN=\nANTHROPIC_BASE_URL=\nANTHROPIC_ADMIN_KEY=\nOPENAI_API_KEY=\nOPENAI_ORG_ID=\nARGS_HOLD_SENTINEL=no\n"},
		{name: "handed-over snapshot", settings: []modelsettings.Settings{anthropic}, passed: true,
			suite: "ANTHROPIC_API_KEY=set\nANTHROPIC_AUTH_TOKEN=set\nANTHROPIC_BASE_URL=\nANTHROPIC_ADMIN_KEY=\nOPENAI_API_KEY=\nOPENAI_ORG_ID=\nARGS_HOLD_SENTINEL=no\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, reports := t.TempDir(), t.TempDir()
			runID := NewRunID()
			_, err := Start(root, Request{
				RunID: runID, Owner: "alice", ChangeID: "chg", AcceptedAt: time.Now().UTC(), Timeout: time.Hour,
				Plan: verify.Plan{
					CandidateCommit: strings.Repeat("a", 40), RequestRevision: "rev-1", SuiteCommit: strings.Repeat("b", 40),
					ReportDir: reports, WorkDir: filepath.Join(root, runID, "work"), Tests: verify.TestsEvaluator,
				},
			}, []string{os.Args[0]}, tc.settings)
			require.NoError(t, err)
			require.NoError(t, Wait(root, runID))
			_, state, err := Read(root, runID)
			require.NoError(t, err)
			require.Equal(t, StateCompleted, state.Kind)
			assert.Equal(t, tc.passed, state.Report.Passed)
			prepared, err := os.ReadFile(filepath.Join(root, runID, "prepare-report"))
			require.NoError(t, err)
			assert.Equal(t, "ANTHROPIC_API_KEY=\nANTHROPIC_AUTH_TOKEN=\nANTHROPIC_BASE_URL=\nANTHROPIC_ADMIN_KEY=\nOPENAI_API_KEY=\nOPENAI_ORG_ID=\nARGS_HOLD_SENTINEL=no\n", string(prepared))
			suite, err := os.ReadFile(filepath.Join(root, runID, "suite-report"))
			require.NoError(t, err)
			assert.Equal(t, tc.suite, string(suite))
			for _, file := range []string{"request.json", "result.json"} {
				raw, err := os.ReadFile(filepath.Join(root, runID, file))
				require.NoError(t, err)
				assert.NotContains(t, string(raw), "sentinel", "%s holds no setting", file)
			}
		})
	}
}

// TestLiveStartDetailProtectsHeldSettings starts a live target whose suite
// prints every setting it was given and a first line whose unknown field is
// its key, which the executor then quotes. The exported Detail replaces the
// key and token, keeps a base URL's scheme and host only, and never quotes a
// malformed base URL or a parser's error, while the log the suite wrote keeps
// what it printed.
func TestLiveStartDetailProtectsHeldSettings(t *testing.T) {
	for _, tc := range []struct {
		name, baseURL, want string
	}{
		{name: "independent credential in the path", baseURL: "https://gateway.invalid/sentinel-independent-credential/", want: "https://gateway.invalid"},
		{name: "user information, query and fragment", baseURL: "https://user:sentinel-pass@gateway.invalid:8443/p?token=sentinel-query#sentinel-fragment", want: "https://gateway.invalid:8443"},
		{name: "invalid escape", baseURL: "https://gateway.invalid/%zz-sentinel-malformed", want: modelsettings.Redacted},
		{name: "no scheme", baseURL: "gateway.invalid/sentinel-no-scheme", want: modelsettings.Redacted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(liveFakeEnv, "leak")
			self, err := os.Executable()
			require.NoError(t, err)
			anthropic, err := modelsettings.New("anthropic", map[string]string{
				"ANTHROPIC_API_KEY": "sentinel-leaked-key", "ANTHROPIC_AUTH_TOKEN": "sentinel-leaked-token", "ANTHROPIC_BASE_URL": tc.baseURL,
			})
			require.NoError(t, err)
			p, err := LaunchLive(t.TempDir(), evaluatorPlan, "secret-key", []string{self}, os.Environ(), []modelsettings.Settings{anthropic})
			require.NoError(t, err)
			awaitErr := p.AwaitReady(t.Context())
			written, readErr := os.ReadFile(filepath.Join(p.Dir(), "executor.log"))
			require.NoError(t, p.Stop())
			require.NoError(t, readErr)
			var start *LiveStartError
			require.ErrorAs(t, awaitErr, &start)

			assert.Contains(t, string(written), "sentinel-leaked-key", "the log is the suite's own output and keeps what it printed")
			assert.Contains(t, start.Detail, `json: unknown field "`+modelsettings.Redacted+`"`, "the executor's own line is protected")
			assert.Contains(t, start.Detail, "ANTHROPIC_AUTH_TOKEN="+modelsettings.Redacted)
			assert.Contains(t, start.Detail, "ANTHROPIC_BASE_URL="+tc.want+"\n")
			assert.NotContains(t, start.Detail, "sentinel")
			assert.NotContains(t, start.Detail, "invalid URL escape")
			assert.NotContains(t, start.Summary, "sentinel")
		})
	}
}

// TestStartErrorReplacesValuesTheTailCuts writes executor logs longer than
// the exported tail, whose cut falls inside a held key (in its random part
// and in its fixed prefix), a token and a base URL's path credential, and a
// log a writer stopped inside a key. The Detail startError exports replaces
// each such value once, from the context it reads before the tail, and
// keeps all the log text around it.
func TestStartErrorReplacesValuesTheTailCuts(t *testing.T) {
	const key = "sk-ant-api03-sentinelRANDOMPART0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZq7w3e9r1t5uZZAA"
	const token = "sentinel-cut-token-5d2c"
	const baseURL = "https://gateway.invalid/sentinel-path-credential-7f3a/"
	anthropic, err := modelsettings.New("anthropic", map[string]string{
		"ANTHROPIC_API_KEY": key, "ANTHROPIC_AUTH_TOKEN": token, "ANTHROPIC_BASE_URL": baseURL,
	})
	require.NoError(t, err)
	// credential is the base URL's part after its scheme and host, which the
	// replacement keeps.
	credential := baseURL[strings.Index(baseURL, "sentinel"):]
	before := strings.Repeat("p", 99) + "\n"
	for _, tc := range []struct {
		name, value, replacement string
		cut                      int
	}{
		{name: "inside the key's random part", value: key, replacement: modelsettings.Redacted, cut: 13},
		{name: "inside the key's fixed prefix", value: key, replacement: modelsettings.Redacted, cut: 5},
		{name: "inside the token", value: token, replacement: modelsettings.Redacted, cut: 10},
		{name: "inside the base URL's path credential", value: baseURL, replacement: "https://gateway.invalid", cut: strings.Index(baseURL, "sentinel") + 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The tail starts len(before)+cut bytes into the log: cut bytes
			// into the value.
			after := "\n" + strings.Repeat("y", maxLiveDetail-(len(tc.value)-tc.cut)-1)
			detail := startErrorDetail(t, before+tc.value+after, anthropic)
			assert.Equal(t, tc.replacement+after, detail, "the cut value is replaced once and the text after it kept")
			assertNoPiece(t, detail, key, token, credential)
		})
	}
	t.Run("log ending inside the key", func(t *testing.T) {
		earlier := strings.Repeat("y", maxLiveDetail) + "\n"
		detail := startErrorDetail(t, earlier+key[:71], anthropic)
		assert.Equal(t, earlier[len(earlier)+71-maxLiveDetail:]+modelsettings.Redacted, detail, "the unfinished key is replaced")
		assertNoPiece(t, detail, key, token, credential)
	})
}

// TestStartErrorReplacesValuesThatFillTheTail exports the tails the review
// found leaking: a key that begins with its own last character, printed
// back to back across the whole tail and read while its last copy is being
// written; a custom-headers value of 3,000 CJK characters, longer than the
// tail, read while it is being written and once written; and the longest
// value a hand-over allows, whose last character begins the tail, which only
// the context of ExcerptContext bytes before the tail reveals, and whose last
// byte begins the tail, which leaves out the character the tail cuts.
func TestStartErrorReplacesValuesThatFillTheTail(t *testing.T) {
	const border = "sk-proj-sentinelBORDERKEY-0123456789abcdefghijklmnopqrs"
	borderKey, err := modelsettings.New("openai", map[string]string{"OPENAI_API_KEY": border})
	require.NoError(t, err)
	assert.Equal(t, modelsettings.Redacted, startErrorDetail(t, strings.Repeat(border, 200)+border[:40], borderKey))

	var cjk strings.Builder
	cjk.WriteString("X-Sentinel-Header: ")
	for i := range 3000 {
		cjk.WriteRune(rune(0x4E00 + (i*7919)%20000))
	}
	header := cjk.String()
	require.Greater(t, len(header), maxLiveDetail)
	headers, err := modelsettings.New("anthropic", map[string]string{"ANTHROPIC_API_KEY": "sentinel-cjk-key", "ANTHROPIC_CUSTOM_HEADERS": header})
	require.NoError(t, err)
	assert.Equal(t, modelsettings.Redacted, startErrorDetail(t, "suite configuration:\n"+header[:len(header)-100], headers), "being written")
	assert.Equal(t, modelsettings.Redacted+"\n", startErrorDetail(t, "suite configuration:\n"+header+"\n", headers), "written")

	longest := strings.Repeat("\U0001D11E", modelsettings.MaxValueLength)
	edge, err := modelsettings.New("anthropic", map[string]string{"ANTHROPIC_API_KEY": "sentinel-edge-key", "ANTHROPIC_CUSTOM_HEADERS": longest})
	require.NoError(t, err)
	before := strings.Repeat("p", 99) + "\n"
	after := "\n" + strings.Repeat("y", maxLiveDetail-utf8.UTFMax-1)
	assert.Equal(t, modelsettings.Redacted+after, startErrorDetail(t, before+longest+after, edge), "the value's last character begins the tail")
	after = "\n" + strings.Repeat("y", maxLiveDetail-2)
	assert.Equal(t, after, startErrorDetail(t, before+longest+after, edge), "the value's last byte begins the tail")
	after += "y"
	assert.Equal(t, after, startErrorDetail(t, before+longest+after, edge), "the value ends just before the tail")
}

// TestStartErrorExportsValidUTF8 exports logs of four-byte characters
// whose tail starts inside a character, at each byte of it and at a
// character boundary, and a log whose writer stopped inside a character.
// The detail is valid UTF-8, which the span exporter requires, and keeps
// every whole character of the tail.
func TestStartErrorExportsValidUTF8(t *testing.T) {
	const clef = "\U0001D11E"
	key, err := modelsettings.New("anthropic", map[string]string{"ANTHROPIC_API_KEY": "sentinel-utf8-key"})
	require.NoError(t, err)
	for pad := range utf8.UTFMax {
		// The tail holds maxLiveDetail-1-pad bytes of characters, the pad
		// and a newline, so it starts pad bytes later each time.
		log := "head\n" + strings.Repeat(clef, 3000) + strings.Repeat("y", pad) + "\n"
		detail := startErrorDetail(t, log, key)
		whole := (maxLiveDetail - 1 - pad) / utf8.UTFMax
		assert.Equal(t, strings.Repeat(clef, whole)+strings.Repeat("y", pad)+"\n", detail, "pad %d", pad)
		assert.True(t, utf8.ValidString(detail), "pad %d", pad)
	}
	for c := 1; c < utf8.UTFMax; c++ {
		detail := startErrorDetail(t, "head\nline "+clef+clef[:c], key)
		assert.Equal(t, "head\nline "+clef, detail, "the log ends %d bytes into a character", c)
		assert.True(t, utf8.ValidString(detail))
	}
}

// startErrorDetail writes log as a live directory's executor log and
// returns the Detail startError exports for a target given held.
func startErrorDetail(t *testing.T, log string, held ...modelsettings.Settings) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "executor.log"), []byte(log), 0o600))
	p := &LiveProcess{dir: dir, settings: held}
	return p.startError("summary", nil).Detail
}

// assertNoPiece fails when text holds any eight bytes of a value after its
// first eight, which are the parts a cut can leave.
func assertNoPiece(t *testing.T, text string, values ...string) {
	t.Helper()
	for _, value := range values {
		for i := 8; i+8 <= len(value); i++ {
			assert.NotContains(t, text, value[i:i+8])
		}
	}
}

// writeSettingsReport writes, to path, whether each reported provider
// variable is set in environ (ANTHROPIC_API_KEY reports "handed" when it is
// the value tests hand over) and whether this process's arguments hold a
// sentinel. It writes names and booleans only.
func writeSettingsReport(path string, environ []string) error {
	var b strings.Builder
	for _, name := range reportedSettings {
		value, ok := lookupEnv(environ, name)
		state := ""
		switch {
		case ok && name == "ANTHROPIC_API_KEY" && value == "sentinel-handed-key":
			state = "handed"
		case ok && value != "":
			state = "set"
		}
		b.WriteString(name + "=" + state + "\n")
	}
	args := "no"
	for _, arg := range os.Args {
		if strings.Contains(arg, "sentinel") {
			args = "yes"
		}
	}
	b.WriteString("ARGS_HOLD_SENTINEL=" + args + "\n")
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// lookupEnv returns the last value environ gives name, as a program sees it.
func lookupEnv(environ []string, name string) (string, bool) {
	var value string
	var found bool
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			value, found = v, true
		}
	}
	return value, found
}

// isolateProviderFamilies hides the machine's own provider variables from a
// test and restores each when it ends: this process's environment is what a
// verification run inherits, and no test may depend on a real key.
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
