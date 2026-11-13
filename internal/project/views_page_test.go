// This file drives the views page against the real project server: the page
// shell and its policy, the one-use link, the in-memory session, the browser
// marks the page's reads need, and what happens when the server restarts.
package project

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	genproject "github.com/goadesign/surla/gen/project"
	"github.com/goadesign/surla/internal/controlplane"
)

// viewsProject is one running project server composed as the `surla serve`
// composition root composes it, with a test-only restart that replaces the
// project server (and so every in-memory session) behind the same address.
type viewsProject struct {
	// state is the project's private state.
	state *State
	// cp is the project's control plane.
	cp *controlplane.ControlPlane
	// server is the project server currently answering.
	server atomic.Pointer[Server]
	// handler is server's guarded handler.
	handler atomic.Pointer[http.Handler]
}

// TestViewsPageSessionLifecycle walks the page's session: the served shell
// and its policy, no link, a link exchanged once and removed from the
// address, nothing stored in the browser, a reload, a second use of the
// link, the browser marks reads carry, and a server restart while the user
// has typed edits.
func TestViewsPageSessionLifecycle(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		missingPageTool(t, "node is required to exercise the views page")
	}
	if _, err := os.Stat("../../ui/checkpoint/node_modules/jsdom"); err != nil {
		missingPageTool(t, "ui/checkpoint/node_modules is missing; run npm ci --prefix ui/checkpoint")
	}
	vp := startViewsProject(t)
	management := controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: vp.state.Record.Owner, CallerID: "management", Kind: controlplane.CallerManagement})
	agent, err := vp.server.Load().AddAgent(management, &genproject.AddAgentPayload{ActorID: "guide", Role: "guide"})
	require.NoError(t, err)
	guide, err := os.ReadFile(agent.CredentialFile)
	require.NoError(t, err)
	link := func() string {
		minted, err := vp.server.Load().BrowserLink(management)
		require.NoError(t, err)
		u, err := url.Parse(minted.URL)
		require.NoError(t, err)
		require.Equal(t, vp.state.Origin(), u.Scheme+"://"+u.Host)
		return strings.TrimPrefix(u.Fragment, "link=")
	}

	shell, err := http.Get(vp.state.Origin() + "/views")
	require.NoError(t, err)
	require.NoError(t, shell.Body.Close())
	policy := shell.Header.Get("Content-Security-Policy")
	assert.Equal(t, http.StatusOK, shell.StatusCode)
	assert.Equal(t, "text/html; charset=utf-8", shell.Header.Get("Content-Type"))
	assert.Equal(t, "nosniff", shell.Header.Get("X-Content-Type-Options"))
	for _, directive := range []string{"default-src 'none'", "connect-src 'self'", "frame-ancestors 'none'", "form-action 'none'", "base-uri 'none'"} {
		assert.Contains(t, policy, directive, "the page's policy, as sent by the server")
	}

	input := map[string]any{
		"origin": vp.state.Origin(), "first": link(), "guide": string(guide),
	}
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	inputPath := filepath.Join(t.TempDir(), "input.json")
	require.NoError(t, os.WriteFile(inputPath, raw, 0o600))
	harness, err := filepath.Abs("../../ui/checkpoint/testing/page_harness.cjs")
	require.NoError(t, err)
	harnessLiteral, err := json.Marshal(harness)
	require.NoError(t, err)
	script := `
const fs = require("fs");
const { api, control, expect, openPage, sleep, until } = require(` + string(harnessLiteral) + `);
const input = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
const origin = input.origin;
async function main() {
  // Without a link the page explains how to open it and asks the server nothing.
  const bare = await openPage(origin);
  await until(() => bare.$("#surla-signed-out"), "a page without a link did not say how to open it");
  expect(bare.text("#surla-signed-out").includes("Run surla in your project folder"), "the signed-out page does not name surla");
  await sleep(200);
  expect(bare.requests.length === 0, "a page without a link sent requests: " + JSON.stringify(bare.requests));
  bare.close();
  for (const path of ["/changes", "/change-updates?since=x", "/changes/chg", "/traces"]) {
    const refused = await api(origin, "GET", path);
    expect(refused.status === 401, "a data request without a credential answered " + refused.status + " for " + path);
  }

  // The link becomes a session held only in memory, and leaves the address.
  const page = await openPage(origin, { link: input.first });
  await until(() => page.$("#surla-change-list-empty"), "the page did not obtain a session and read the empty project");
  expect(page.win.location.href === origin + "/views" && page.win.location.hash === "", "the link stayed in the address: " + page.win.location.href);
  const exchange = page.sent("POST", "/project/browser-session");
  expect(exchange.length === 1 && exchange[0].body.link === input.first && !exchange[0].headers.Authorization, "the link was not exchanged exactly once");
  const reads = page.requests.filter((r) => r.method === "GET");
  expect(reads.length > 0 && reads.every((r) => !("Origin" in r.headers) && r.headers.Authorization === reads[0].headers.Authorization), "reads did not carry the session without stating the page's address");
  expect(page.win.localStorage.length === 0 && page.win.sessionStorage.length === 0 && page.doc.cookie === "", "the session was left in browser storage");
  const session = reads[0].headers.Authorization;

  // The server refuses the session from another page or from a program.
  const fromOtherPage = await fetch(origin + "/changes", { headers: { Authorization: session, "Sec-Fetch-Site": "same-site", Origin: "http://127.0.0.1:1" } });
  expect(fromOtherPage.status === 403, "a read from another address answered " + fromOtherPage.status);
  const replayed = await api(origin, "GET", "/changes", undefined, session.replace("Bearer ", ""));
  expect(replayed.status === 401, "a replay without the page's mark answered " + replayed.status);

  // A change the guide proposes appears by itself; the user types into it.
  const proposed = await api(origin, "POST", "/changes/propose", {
    change_id: "chg-session", prompt: "Route refunds to a manager.",
    analysis: { summary: "Route refunds to a manager.", ambiguities: [], examples: [{ scenario: "Refund", input_summary: "$40", expected_outcome: "Approved", verification_kind: "deterministic" }], preserved_behaviors: [] },
  }, input.guide);
  expect(proposed.status === 200, "the guide could not propose: " + JSON.stringify(proposed));
  await until(() => page.$("#surla-open-change-chg-session"), "the proposed change did not appear");
  page.$("#surla-open-change-chg-session").click();
  await until(() => page.$('textarea[data-field="expected_outcome"]'), "the change did not open");
  const field = page.$('textarea[data-field="expected_outcome"]');
  page.type(field, "Typed before the restart");

  // A reload is a new page without the link: it has no session.
  const reloaded = await openPage(origin);
  await until(() => reloaded.$("#surla-signed-out"), "a reloaded page did not say how to open it again");
  reloaded.close();

  // A second use of the same link is refused and reported.
  const reused = await openPage(origin, { link: input.first });
  await until(() => reused.$("#surla-link-used"), "a used link was not reported");
  expect(reused.text("#surla-link-used").includes("already used"), "the used link was not reported plainly");
  expect(reused.win.location.hash === "", "a refused link stayed in the address");
  reused.close();

  // After a restart the old session is refused; typed edits stay and the page says how to continue.
  await control(origin, "restart");
  page.$("#surla-confirm-submit").click();
  await until(() => page.$("#surla-session-ended"), "the page did not say its session ended");
  expect(page.text("#surla-session-ended").includes("Run surla in your project folder"), "the page did not say how to continue");
  expect(page.$('textarea[data-field="expected_outcome"]').value === "Typed before the restart", "typed edits were lost");
  expect(page.text("#surla-session-ended-edits").includes("Typed before the restart"), "the unsaved edits are not shown for copying");
  const afterRestart = await fetch(origin + "/changes", { headers: { Authorization: session, "Sec-Fetch-Site": "same-origin" } });
  expect(afterRestart.status === 401, "a session from before the restart answered " + afterRestart.status);
  page.close();
}
main().then(() => { process.stdout.write("PAGE-PASSED"); process.exit(0); }, (err) => { console.error(err && err.stack || err); process.exit(1); });
`
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "-e", script, inputPath)
	cmd.Env = append(os.Environ(), "CI=true")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "views page session script failed:\n%s", out)
	require.True(t, strings.HasSuffix(string(out), "PAGE-PASSED"), "views page session script output:\n%s", out)
}

// TestViewsPageReadsNeedTheSameOriginMark sends the page's two reads with a
// browser session straight to the project server, without a browser: a
// browser's same-origin mark is enough without an Origin header (browsers
// omit Origin on same-origin GET requests), and a missing mark, another
// site's mark, or another page's Origin is refused.
func TestViewsPageReadsNeedTheSameOriginMark(t *testing.T) {
	vp := startViewsProject(t)
	management := controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: vp.state.Record.Owner, CallerID: "management", Kind: controlplane.CallerManagement})
	minted, err := vp.server.Load().BrowserLink(management)
	require.NoError(t, err)
	u, err := url.Parse(minted.URL)
	require.NoError(t, err)
	body, err := json.Marshal(&genproject.ExchangeBrowserLinkPayload{Link: strings.TrimPrefix(u.Fragment, "link=")})
	require.NoError(t, err)
	exchange, err := http.NewRequest(http.MethodPost, vp.state.Origin()+"/project/browser-session", strings.NewReader(string(body)))
	require.NoError(t, err)
	exchange.Header.Set("Content-Type", "application/json")
	exchange.Header.Set("Sec-Fetch-Site", "same-origin")
	exchange.Header.Set("Origin", vp.state.Origin())
	exchanged, err := http.DefaultClient.Do(exchange)
	require.NoError(t, err)
	var session genproject.ExchangeBrowserLinkResult
	require.NoError(t, json.NewDecoder(exchanged.Body).Decode(&session))
	require.NoError(t, exchanged.Body.Close())
	require.Equal(t, http.StatusCreated, exchanged.StatusCode)

	cases := []struct {
		name   string
		site   string
		origin string
		want   int
	}{
		{name: "same-origin mark without Origin", site: "same-origin", want: http.StatusOK},
		{name: "same-origin mark with the project's Origin", site: "same-origin", origin: vp.state.Origin(), want: http.StatusOK},
		{name: "no mark and no Origin", want: http.StatusUnauthorized},
		{name: "cross-site mark", site: "cross-site", want: http.StatusForbidden},
		{name: "same-site mark", site: "same-site", want: http.StatusForbidden},
		{name: "same-origin mark with another page's Origin", site: "same-origin", origin: "http://127.0.0.1:1", want: http.StatusForbidden},
	}
	for _, path := range []string{"/changes", "/change-updates?since=other"} {
		for _, tc := range cases {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				req, err := http.NewRequest(http.MethodGet, vp.state.Origin()+path, nil)
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer "+session.Session)
				if tc.site != "" {
					req.Header.Set("Sec-Fetch-Site", tc.site)
				}
				if tc.origin != "" {
					req.Header.Set("Origin", tc.origin)
				}
				resp, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				assert.Equal(t, tc.want, resp.StatusCode)
			})
		}
	}
}

// startViewsProject starts a project's server on its saved address.
func startViewsProject(t *testing.T) *viewsProject {
	t.Helper()
	state := testState(t)
	cp, err := controlplane.New(controlplane.Options{CardsDir: state.CardsDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cp.Close()) })
	vp := &viewsProject{state: state, cp: cp}
	vp.restart()
	api := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/__test/restart" {
			vp.restart()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		(*vp.handler.Load()).ServeHTTP(w, r)
	}))
	require.NoError(t, api.Listener.Close())
	api.Listener, err = net.Listen("tcp", state.Address())
	require.NoError(t, err)
	api.Start()
	t.Cleanup(api.Close)
	return vp
}

// restart replaces the project server, as a restarted `surla serve` does:
// agent credentials survive in the state folder and every session ends.
func (vp *viewsProject) restart() {
	server := NewServer(vp.state, func() {})
	application := http.NewServeMux()
	application.Handle("GET /views", controlplane.ViewsPage())
	application.Handle("/", controlplane.NewHTTPHandler(vp.cp, server.Authenticate))
	handler := server.Handler(application)
	vp.server.Store(server)
	vp.handler.Store(&handler)
}

// missingPageTool fails the test under CI=true, because CI installs node and
// ui/checkpoint's packages before running Go tests, and otherwise skips it
// with reason, which names the step that provides the missing tool. Tests
// never install the packages themselves: that writes the source tree and
// races other test binaries reading it.
func missingPageTool(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv("CI") == "true" {
		t.Fatal(reason)
	}
	t.Skip(reason)
}
