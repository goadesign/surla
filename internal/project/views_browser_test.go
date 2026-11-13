//go:build browser && (darwin || linux)

// This file proves in a real Chromium-family browser what the jsdom tests in
// views_page_test.go cannot: the browser enforces the views page's
// Content-Security-Policy and framing rule, and itself sends the
// Sec-Fetch-Site mark every session request depends on. It is opt-in:
//
//	CI=true go test -race -count=1 -tags browser ./internal/project </dev/null
//
// The test starts a headless browser with a throwaway profile inside its own
// temporary directory, never the user's browser, profile, windows or tabs.
// Set SURLA_TEST_BROWSER to the browser's executable when it is not installed
// in a well-known location.
package project

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/controlplane"
)

type (
	// browserInput is what browser_harness.cjs reads: where the browser's
	// DevTools endpoint and the pages are, and the one-use link to open.
	browserInput struct {
		// DevTools is the browser-wide DevTools WebSocket address.
		DevTools string `json:"devtools"`
		// Origin is the project server's origin.
		Origin string `json:"origin"`
		// SameSiteOrigin is the test's other listener on the project's host.
		SameSiteOrigin string `json:"same_site_origin"`
		// CrossSiteOrigin is the same listener under another host name.
		CrossSiteOrigin string `json:"cross_site_origin"`
		// Link is the one-use link the page exchanges.
		Link string `json:"link"`
		// ChangeIDs are the saved changes the page must list.
		ChangeIDs []string `json:"change_ids"`
	}

	// browserFacts is everything browser_harness.cjs observed in the browser.
	// Its fields mirror the harness's output exactly; decoding rejects any
	// other field so the two cannot drift apart silently.
	browserFacts struct {
		// Browser names the browser that ran the scenario.
		Browser struct {
			Product   string `json:"product"`
			Protocol  string `json:"protocol"`
			UserAgent string `json:"user_agent"`
		} `json:"browser"`
		// Views is the views page opened with the link.
		Views viewsFacts `json:"views"`
		// Framing is a page on another origin that frames the views page.
		Framing framingFacts `json:"framing"`
		// OtherPages are pages on other origins that call the server.
		OtherPages []otherPageFacts `json:"other_pages"`
		// Reload is the views page after a reload.
		Reload reloadFacts `json:"reload"`
	}

	// viewsFacts is what the views page showed, sent, stored and reported.
	viewsFacts struct {
		ChangeIDs          []string `json:"change_ids"`
		ChangeListTimedOut bool     `json:"change_list_timed_out"`
		// Href is the page's location after it exchanged the link.
		Href string `json:"href"`
		// HistoryURLs are the tab's navigation history entries, as the
		// browser's address bar and back button know them.
		HistoryURLs       []string `json:"history_urls"`
		CurrentHistoryURL string   `json:"current_history_url"`
		// TargetURL is the tab's address as the browser reports it.
		TargetURL string `json:"target_url"`
		// RequestsSettled says every request to the server was sent and,
		// except the open wait_for_changes read, answered.
		RequestsSettled bool              `json:"requests_settled"`
		Requests        []observedRequest `json:"requests"`
		Storage         struct {
			LocalStorage   int    `json:"local_storage"`
			SessionStorage int    `json:"session_storage"`
			Cookie         string `json:"cookie"`
			IndexedDB      int    `json:"indexed_db"`
			BrowserCookies int    `json:"browser_cookies"`
		} `json:"storage"`
		// LoadViolations are policy reports raised while the page loaded,
		// exchanged the link and listed the changes.
		LoadViolations         []violation       `json:"load_violations"`
		InjectedScriptRan      bool              `json:"injected_script_ran"`
		InjectedViolations     []violation       `json:"injected_violations"`
		ForeignFetch           string            `json:"foreign_fetch"`
		ForeignFetchViolations []violation       `json:"foreign_fetch_violations"`
		ForeignFetchRequests   []observedRequest `json:"foreign_fetch_requests"`
		SessionSeen            bool              `json:"session_seen"`
		Console                []string          `json:"console"`
	}

	// observedRequest is one request as the browser's network layer saw it.
	// Credential values are redacted by the harness.
	observedRequest struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		URL    string `json:"url"`
		Type   string `json:"type"`
		// PageHeaders are the headers the page or the browser's loader set.
		PageHeaders map[string]string `json:"page_headers"`
		// SentHeaders are the headers the browser put on the wire, with
		// lowercase names; nil when the browser never sent the request.
		SentHeaders map[string]string `json:"sent_headers"`
		// Status is the server's answer, or 0 when none arrived.
		Status  int    `json:"status"`
		Failure string `json:"failure"`
	}

	// violation is one securitypolicyviolation report.
	violation struct {
		EffectiveDirective string `json:"effective_directive"`
		ViolatedDirective  string `json:"violated_directive"`
		BlockedURI         string `json:"blocked_uri"`
		Disposition        string `json:"disposition"`
		SourceFile         string `json:"source_file"`
	}

	// framingFacts is what the browser did with a page that frames /views.
	framingFacts struct {
		// Frames is the tab's frame tree after its load event.
		Frames []observedFrame `json:"frames"`
		// Navigated is every frame commit the tab reported.
		Navigated []observedFrame `json:"navigated"`
		// Contexts is every script context the tab created.
		Contexts []struct {
			Origin    string `json:"origin"`
			FrameID   string `json:"frame_id"`
			IsDefault bool   `json:"is_default"`
		} `json:"contexts"`
		Requests []observedRequest `json:"requests"`
		Console  []string          `json:"console"`
	}

	// observedFrame is one frame and, in Frames, what its document shows.
	observedFrame struct {
		ID             string `json:"id"`
		ParentID       string `json:"parent_id"`
		URL            string `json:"url"`
		SecurityOrigin string `json:"security_origin"`
		UnreachableURL string `json:"unreachable_url"`
		View           *struct {
			Href         string `json:"href"`
			Origin       string `json:"origin"`
			HasViewsRoot bool   `json:"has_views_root"`
			Text         string `json:"text"`
			Error        string `json:"error"`
		} `json:"view"`
	}

	// otherPageFacts is what a page on another origin could do to the server.
	otherPageFacts struct {
		Origin string `json:"origin"`
		// WithoutCredentials is what the page saw of a no-cors read.
		WithoutCredentials string `json:"without_credentials"`
		// WithSession is what the page saw of a read carrying the views
		// page's session.
		WithSession string            `json:"with_session"`
		Requests    []observedRequest `json:"requests"`
		Console     []string          `json:"console"`
	}

	// reloadFacts is the views page after a reload.
	reloadFacts struct {
		SignedOutText     string `json:"signed_out_text"`
		SignedOutTimedOut bool   `json:"signed_out_timed_out"`
		Href              string `json:"href"`
		// PageRequests are the reloaded page's own requests until it showed
		// how to open it again.
		PageRequests []observedRequest `json:"page_requests"`
		// DataRequestStatus is what a data read sent from the reloaded page
		// answered: a status number, or the error the page saw.
		DataRequestStatus json.RawMessage   `json:"data_request_status"`
		DataRequest       []observedRequest `json:"data_request"`
	}

	// otherOrigin is a second listener the test owns. The browser reaches it
	// as http://127.0.0.1:<port>, the project server's site but another
	// origin, and as http://localhost:<port>, another site.
	otherOrigin struct {
		server    *httptest.Server
		sameSite  string
		crossSite string
		// probes counts requests that reached /csp-probe.
		probes atomic.Int64
	}

	// realBrowser is one headless browser process the test started on a
	// throwaway profile. The test ends it and checks that nothing it started
	// is left.
	realBrowser struct {
		cmd      *exec.Cmd
		profile  string
		devtools string
		output   *browserOutput
		// exited is closed once cmd.Wait returned waitErr.
		exited  chan struct{}
		waitErr error
	}

	// browserOutput keeps the end of the browser's output for diagnostics
	// and hands over the DevTools address the browser prints once.
	browserOutput struct {
		mu       sync.Mutex
		tail     []byte
		line     []byte
		found    bool
		devtools chan string
	}

	// listedProcess is one process as ps lists it.
	listedProcess struct {
		pid, ppid, pgid int
		command         string
	}
)

const (
	// browserEnv names the environment variable that selects the browser's
	// executable explicitly.
	browserEnv = "SURLA_TEST_BROWSER"
	// maxBrowserOutput bounds the browser output kept for diagnostics.
	maxBrowserOutput = 64 << 10
	// browserDeadline bounds each wait on the browser process.
	browserDeadline = 30 * time.Second
)

// wellKnownBrowsers are the executables tried, in order, when browserEnv is
// unset: macOS application bundles, then names on PATH.
var wellKnownBrowsers = []string{
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	"/Applications/Chromium.app/Contents/MacOS/Chromium",
	"google-chrome",
	"google-chrome-stable",
	"chromium",
	"chromium-browser",
}

// TestViewsPageInARealBrowser opens the views page in a real headless
// browser against the real project server and checks, each in its own
// subtest, what only a real browser shows: the link exchange and the address,
// the browser's own same-origin mark, empty browser storage, a load without
// policy violations, the enforced policy, refused framing, refused requests
// from other origins, a reload ending the session, and the browser's cleanup.
func TestViewsPageInARealBrowser(t *testing.T) {
	executable := findBrowser(t)
	node, err := exec.LookPath("node")
	if err != nil {
		missingPageTool(t, "node is required to drive the browser through its DevTools protocol")
	}
	harness, err := filepath.Abs("../../ui/checkpoint/testing/browser_harness.cjs")
	require.NoError(t, err)

	vp := startViewsProject(t, controlplane.Options{})
	origin := vp.state.Origin()
	guide := vp.agent(t, "guide")
	changeIDs := []string{"chg-refunds", "chg-invoices"}
	for _, id := range changeIDs {
		proposeChange(t, origin, guide, id)
	}
	link, err := vp.link()
	require.NoError(t, err)

	other := startOtherOrigin(t, origin+"/views")
	started := time.Now()
	browser := startBrowser(t, executable)
	listening := time.Now()
	facts := driveBrowser(t, node, harness, browserInput{
		DevTools:        browser.devtools,
		Origin:          origin,
		SameSiteOrigin:  other.sameSite,
		CrossSiteOrigin: other.crossSite,
		Link:            link,
		ChangeIDs:       changeIDs,
	})
	t.Logf("browser: %s (DevTools protocol %s); listening after %s, scenario took %s",
		facts.Browser.Product, facts.Browser.Protocol, listening.Sub(started).Round(time.Millisecond), time.Since(listening).Round(time.Millisecond))

	t.Run("a link exchanged, removed from the address, change list rendered", func(t *testing.T) {
		assert.False(t, facts.Views.ChangeListTimedOut, "the change list never listed the saved changes")
		assert.ElementsMatch(t, changeIDs, facts.Views.ChangeIDs, "the change list the browser rendered")
		exchanges := requestsTo(facts.Views.Requests, http.MethodPost, "/project/browser-session")
		if assert.Len(t, exchanges, 1, "the page exchanged the link exactly once") {
			assert.Equal(t, http.StatusCreated, exchanges[0].Status, "the server's answer to the exchange")
		}
		assert.Equal(t, origin+"/views", facts.Views.Href, "the page's location")
		assert.Equal(t, origin+"/views", facts.Views.CurrentHistoryURL, "the tab's current history entry")
		assert.Equal(t, origin+"/views", facts.Views.TargetURL, "the tab's address")
		for _, address := range append([]string{facts.Views.Href, facts.Views.TargetURL}, facts.Views.HistoryURLs...) {
			assert.NotContains(t, address, link, "an address the browser keeps still holds the link")
		}
	})

	t.Run("b the browser itself sent the same-origin mark and the server accepted", func(t *testing.T) {
		assert.True(t, facts.Views.RequestsSettled, "the page's requests did not settle")
		data := pageFetches(facts.Views.Requests)
		assert.NotEmpty(t, requestsTo(data, http.MethodGet, "/changes"), "the page read no change list")
		assert.NotEmpty(t, requestsTo(data, http.MethodGet, "/change-updates"), "the page never waited for changes")
		for _, r := range data {
			if !assert.NotNil(t, r.SentHeaders, "%s %s was never put on the wire", r.Method, r.URL) {
				continue
			}
			assert.Equal(t, "same-origin", r.SentHeaders["sec-fetch-site"], "Sec-Fetch-Site of %s %s", r.Method, r.URL)
			assert.Equal(t, vp.state.Address(), r.SentHeaders["host"], "Host of %s %s", r.Method, r.URL)
			if pathOf(r.URL) == "/project/browser-session" {
				assert.NotContains(t, r.SentHeaders, "authorization", "the link exchange carried a credential")
			} else {
				assert.Equal(t, "Bearer <redacted>", r.SentHeaders["authorization"], "Authorization of %s %s", r.Method, r.URL)
			}
			if r.Method != http.MethodGet {
				assert.Equal(t, origin, r.SentHeaders["origin"], "Origin of %s %s", r.Method, r.URL)
			}
			if r.Status != 0 || pathOf(r.URL) != "/change-updates" {
				assert.True(t, r.Status >= 200 && r.Status < 300, "the server refused %s %s with %d (%s)", r.Method, r.URL, r.Status, r.Failure)
			}
		}
		for _, r := range facts.Views.Requests {
			t.Logf("observed %s %s -> %d sec-fetch-site=%q sec-fetch-mode=%q sec-fetch-dest=%q origin=%q",
				r.Method, pathOf(r.URL), r.Status, r.SentHeaders["sec-fetch-site"], r.SentHeaders["sec-fetch-mode"], r.SentHeaders["sec-fetch-dest"], r.SentHeaders["origin"])
		}
	})

	t.Run("c nothing left in browser storage", func(t *testing.T) {
		storage := facts.Views.Storage
		assert.Zero(t, storage.LocalStorage, "localStorage entries")
		assert.Zero(t, storage.SessionStorage, "sessionStorage entries")
		assert.Empty(t, storage.Cookie, "document.cookie")
		assert.Zero(t, storage.IndexedDB, "IndexedDB databases")
		assert.Zero(t, storage.BrowserCookies, "cookies in the browser's cookie store")
	})

	t.Run("the page's own load raises no policy violation", func(t *testing.T) {
		assert.Empty(t, facts.Views.LoadViolations, "violations the page raised while it loaded, exchanged the link and listed the changes")
		t.Logf("load violations: %+v", facts.Views.LoadViolations)
	})

	t.Run("d the policy blocks an injected inline script and a fetch to another origin", func(t *testing.T) {
		assert.False(t, facts.Views.InjectedScriptRan, "an inline script inserted into the page ran")
		assert.True(t, hasViolation(facts.Views.InjectedViolations, "script-src-elem", "inline"),
			"no script-src-elem violation for the inserted script: %+v", facts.Views.InjectedViolations)
		assert.True(t, strings.HasPrefix(facts.Views.ForeignFetch, "refused: TypeError"), "the page's fetch to another origin: %s", facts.Views.ForeignFetch)
		assert.True(t, hasViolation(facts.Views.ForeignFetchViolations, "connect-src", other.sameSite+"/csp-probe"),
			"no connect-src violation for the fetch to another origin: %+v", facts.Views.ForeignFetchViolations)
		assert.Zero(t, other.probes.Load(), "the other origin received the page's blocked fetch")
		for _, r := range facts.Views.ForeignFetchRequests {
			assert.Nil(t, r.SentHeaders, "the browser put the blocked fetch on the wire: %+v", r)
		}
		t.Logf("views page console: %q", facts.Views.Console)
		t.Logf("injected script: ran=%t violations=%+v", facts.Views.InjectedScriptRan, facts.Views.InjectedViolations)
		t.Logf("foreign fetch: %s violations=%+v requests=%+v", facts.Views.ForeignFetch, facts.Views.ForeignFetchViolations, facts.Views.ForeignFetchRequests)
	})

	t.Run("e framing is refused", func(t *testing.T) {
		framed := requestsTo(facts.Framing.Requests, http.MethodGet, "/views")
		if assert.Len(t, framed, 1, "the framing page's frame did not request /views") {
			assert.Equal(t, http.StatusOK, framed[0].Status, "the server serves the shell to a same-site frame; the browser must refuse it")
			assert.Equal(t, "iframe", framed[0].SentHeaders["sec-fetch-dest"], "Sec-Fetch-Dest of the framed request")
			t.Logf("framed request: GET /views -> %d sec-fetch-site=%q sec-fetch-dest=%q", framed[0].Status, framed[0].SentHeaders["sec-fetch-site"], framed[0].SentHeaders["sec-fetch-dest"])
		}
		children := 0
		for _, frame := range facts.Framing.Frames {
			assert.NotEqual(t, origin, frame.SecurityOrigin, "a frame committed the views page's origin: %+v", frame)
			if frame.ParentID == "" {
				continue
			}
			children++
			if assert.NotNil(t, frame.View, "the frame's document was not read") {
				assert.False(t, frame.View.HasViewsRoot, "the views page rendered inside the frame: %+v", frame.View)
				assert.NotEqual(t, origin, frame.View.Origin, "the frame's document has the views page's origin")
			}
		}
		assert.Equal(t, 1, children, "the framing page's frames: %+v", facts.Framing.Frames)
		for _, frame := range facts.Framing.Navigated {
			assert.NotEqual(t, origin, frame.SecurityOrigin, "a frame committed the views page's origin: %+v", frame)
		}
		for _, context := range facts.Framing.Contexts {
			assert.NotEqual(t, origin, context.Origin, "a script context ran with the views page's origin")
		}
		assert.True(t, containsText(facts.Framing.Console, "frame-ancestors"), "the browser did not report the frame-ancestors refusal: %q", facts.Framing.Console)
		t.Logf("framing frames=%+v console=%q", facts.Framing.Frames, facts.Framing.Console)
	})

	t.Run("f other origins cannot read or act on the server", func(t *testing.T) {
		marks := map[string]string{other.sameSite: "same-site", other.crossSite: "cross-site"}
		assert.True(t, facts.Views.SessionSeen, "the views page's session was not captured for the other origins to try")
		require.Len(t, facts.OtherPages, len(marks))
		for _, page := range facts.OtherPages {
			assert.Equal(t, "answered: opaque 0", page.WithoutCredentials, "what %s could see of its read", page.Origin)
			assert.True(t, strings.HasPrefix(page.WithSession, "refused: TypeError"), "what %s could see of its read with the session: %s", page.Origin, page.WithSession)
			reached, refusedOnMark := 0, 0
			for _, r := range page.Requests {
				if r.SentHeaders == nil {
					assert.NotEmpty(t, r.Failure, "%s %s from %s was neither sent nor failed", r.Method, r.URL, page.Origin)
					t.Logf("from %s: %s %s (%s) never sent: %s", page.Origin, r.Method, pathOf(r.URL), r.Type, r.Failure)
					continue
				}
				reached++
				if _, ok := r.SentHeaders["origin"]; !ok && r.Status == http.StatusForbidden {
					refusedOnMark++
				}
				assert.Equal(t, marks[page.Origin], r.SentHeaders["sec-fetch-site"], "Sec-Fetch-Site from %s: %s %s", page.Origin, r.Method, r.URL)
				assert.Equal(t, http.StatusForbidden, r.Status, "the server's answer to %s %s from %s", r.Method, r.URL, page.Origin)
				t.Logf("from %s: %s %s (%s) -> %d sec-fetch-site=%q origin=%q failure=%q", page.Origin, r.Method, pathOf(r.URL), r.Type, r.Status, r.SentHeaders["sec-fetch-site"], r.SentHeaders["origin"], r.Failure)
			}
			assert.Positive(t, reached, "no request from %s reached the server", page.Origin)
			assert.Positive(t, refusedOnMark, "no request from %s without an Origin header was refused on its mark alone", page.Origin)
		}
	})

	t.Run("g a reload ends the session", func(t *testing.T) {
		assert.False(t, facts.Reload.SignedOutTimedOut, "the reloaded page did not say how to open it")
		assert.Contains(t, facts.Reload.SignedOutText, "Run surla in your project folder", "the reloaded page's message")
		assert.Equal(t, origin+"/views", facts.Reload.Href, "the reloaded page's address")
		assert.Len(t, requestsTo(facts.Reload.PageRequests, http.MethodGet, "/views"), 1, "the reload did not load the page again")
		assert.Empty(t, pageFetches(facts.Reload.PageRequests), "the reloaded page sent requests of its own")
		assert.JSONEq(t, strconv.Itoa(http.StatusUnauthorized), string(facts.Reload.DataRequestStatus), "a data read from the reloaded page")
		if assert.Len(t, facts.Reload.DataRequest, 1) && assert.NotNil(t, facts.Reload.DataRequest[0].SentHeaders) {
			sent := facts.Reload.DataRequest[0].SentHeaders
			assert.Equal(t, "same-origin", sent["sec-fetch-site"], "the reloaded page's read carried the mark")
			assert.NotContains(t, sent, "authorization", "the reloaded page still held a session")
			assert.NotContains(t, sent, "cookie", "the browser attached a cookie")
		}
	})

	t.Run("browser ended and left nothing", func(t *testing.T) {
		assert.Contains(t, facts.Browser.UserAgent, "HeadlessChrome", "the browser did not run headless")
		browser.stopAndCheck(t)
	})
}

// findBrowser returns the browser executable named by browserEnv, or the
// first well-known one installed, and skips the test when there is none.
func findBrowser(t *testing.T) string {
	t.Helper()
	if explicit := os.Getenv(browserEnv); explicit != "" {
		info, err := os.Stat(explicit)
		require.NoError(t, err, "%s names a browser that does not exist", browserEnv)
		require.False(t, info.IsDir(), "%s must name the browser's executable, not a directory", browserEnv)
		return explicit
	}
	for _, candidate := range wellKnownBrowsers {
		if filepath.IsAbs(candidate) {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
			continue
		}
		if found, err := exec.LookPath(candidate); err == nil {
			return found
		}
	}
	t.Skipf("no Chromium-family browser found; install Google Chrome or Chromium, or set %s to its executable", browserEnv)
	return ""
}

// proposeChange saves change id through the guide's credential, as the
// guide agent does.
func proposeChange(t *testing.T, origin, credential, id string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"change_id": id,
		"prompt":    "Route " + id + " to a manager.",
		"analysis": map[string]any{
			"summary":     "Route " + id + " to a manager.",
			"ambiguities": []any{},
			"examples": []any{map[string]any{
				"scenario": "Refund", "input_summary": "$40", "expected_outcome": "Approved", "verification_kind": "deterministic",
			}},
			"preserved_behaviors": []any{},
		},
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, origin+"/changes/propose", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+credential)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	answer, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode, "proposing %s: %s", id, answer)
}

// startOtherOrigin starts the test's second listener. It serves a page that
// frames framed, a blank page to run requests from, and a probe that counts
// the requests reaching it.
func startOtherOrigin(t *testing.T, framed string) *otherOrigin {
	t.Helper()
	o := &otherOrigin{}
	page := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if _, err := io.WriteString(w, "<!doctype html><html><head><title>other origin</title></head><body>"+body+"</body></html>"); err != nil {
				t.Errorf("serving the other origin's page: %v", err)
			}
		}
	}
	mux := http.NewServeMux()
	mux.Handle("GET /frame", page(`<iframe id="views" src="`+html.EscapeString(framed)+`"></iframe>`))
	mux.Handle("GET /blank", page(""))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/csp-probe", func(w http.ResponseWriter, _ *http.Request) {
		o.probes.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	o.server = httptest.NewServer(mux)
	t.Cleanup(o.server.Close)
	port := strconv.Itoa(o.server.Listener.Addr().(*net.TCPAddr).Port)
	o.sameSite = "http://127.0.0.1:" + port
	o.crossSite = "http://localhost:" + port
	return o
}

// startBrowser starts executable headless on a new profile inside the test's
// temporary directory and returns once the browser printed its DevTools
// address. The browser leads its own process group so the test can find and
// end everything it starts.
func startBrowser(t *testing.T, executable string) *realBrowser {
	t.Helper()
	profile := t.TempDir()
	output := &browserOutput{devtools: make(chan string, 1)}
	cmd := exec.Command(executable,
		"--headless=new",
		"--remote-debugging-port=0",
		"--user-data-dir="+profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--use-mock-keychain",
		"--password-store=basic",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-default-apps",
		"--disable-extensions",
		"--disable-sync",
		"--disable-updater-scheduler",
		"--disable-breakpad",
		"--mute-audio",
		"about:blank",
	)
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	require.NoError(t, cmd.Start())
	b := &realBrowser{cmd: cmd, profile: profile, output: output, exited: make(chan struct{})}
	go func() {
		b.waitErr = cmd.Wait()
		close(b.exited)
	}()
	t.Cleanup(func() { b.kill(t) })
	select {
	case b.devtools = <-output.devtools:
	case <-b.exited:
		t.Fatalf("the browser exited before it listened for DevTools: %v\n%s", b.waitErr, output.String())
	case <-time.After(browserDeadline):
		t.Fatalf("the browser printed no DevTools address within %s:\n%s", browserDeadline, output.String())
	}
	return b
}

// driveBrowser runs browser_harness.cjs with input and returns what it
// observed. A harness failure fails the test with its output.
func driveBrowser(t *testing.T, node, harness string, input browserInput) browserFacts {
	t.Helper()
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	inputPath := filepath.Join(t.TempDir(), "input.json")
	require.NoError(t, os.WriteFile(inputPath, raw, 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, harness, inputPath)
	cmd.Env = append(os.Environ(), "CI=true")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "the browser harness failed:\n%s\nobserved so far: %s", stderr.String(), out)
	var facts browserFacts
	decoder := json.NewDecoder(bytes.NewReader(out))
	decoder.DisallowUnknownFields()
	require.NoError(t, decoder.Decode(&facts), "the browser harness's observations: %s", out)
	return facts
}

// requestsTo returns the requests in rs with method to path.
func requestsTo(rs []observedRequest, method, path string) []observedRequest {
	var out []observedRequest
	for _, r := range rs {
		if r.Method == method && pathOf(r.URL) == path {
			out = append(out, r)
		}
	}
	return out
}

// pageFetches returns the requests the page's script sent with fetch,
// leaving out the document request that loaded the page.
func pageFetches(rs []observedRequest) []observedRequest {
	var out []observedRequest
	for _, r := range rs {
		if r.Type == "Fetch" {
			out = append(out, r)
		}
	}
	return out
}

// pathOf returns the path of address.
func pathOf(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return address
	}
	return u.Path
}

// hasViolation reports whether vs holds an enforced report of directive for
// blocked.
func hasViolation(vs []violation, directive, blocked string) bool {
	for _, v := range vs {
		if v.EffectiveDirective == directive && v.BlockedURI == blocked && v.Disposition == "enforce" {
			return true
		}
	}
	return false
}

// containsText reports whether any of lines contains text.
func containsText(lines []string, text string) bool {
	for _, line := range lines {
		if strings.Contains(line, text) {
			return true
		}
	}
	return false
}

// Write keeps the end of the browser's output and hands over the first
// "DevTools listening on <address>" line's address.
func (o *browserOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tail = append(o.tail, p...)
	if len(o.tail) > maxBrowserOutput {
		o.tail = o.tail[len(o.tail)-maxBrowserOutput:]
	}
	if o.found {
		return len(p), nil
	}
	o.line = append(o.line, p...)
	for {
		end := bytes.IndexByte(o.line, '\n')
		if end < 0 {
			break
		}
		line := strings.TrimSpace(string(o.line[:end]))
		o.line = o.line[end+1:]
		if address, ok := strings.CutPrefix(line, "DevTools listening on "); ok {
			o.found = true
			o.line = nil
			o.devtools <- address
			break
		}
	}
	return len(p), nil
}

// String returns the kept end of the browser's output.
func (o *browserOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.tail)
}

// stopAndCheck asks the browser to shut down with SIGTERM, waits for it,
// and checks that it used only the test's profile and that no process it
// started is left. A leftover process in the browser's group or naming its
// profile is ended; any other leftover, such as a system updater the browser
// woke, is only reported.
func (b *realBrowser) stopAndCheck(t *testing.T) {
	pid := b.cmd.Process.Pid
	tempRoot, err := filepath.EvalSymlinks(os.TempDir())
	require.NoError(t, err)
	profile, err := filepath.EvalSymlinks(b.profile)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(profile, tempRoot+string(filepath.Separator)), "the profile %s is not inside the test's temporary directory", profile)
	active, err := os.ReadFile(filepath.Join(b.profile, "DevToolsActivePort"))
	require.NoError(t, err, "the browser did not write its state into the throwaway profile")
	devtools, err := url.Parse(b.devtools)
	require.NoError(t, err)
	assert.Equal(t, devtools.Port(), strings.SplitN(string(active), "\n", 2)[0], "the profile's DevTools port")

	started := b.processes(t)
	require.NotEmpty(t, started, "the browser process is not listed")
	for _, p := range started {
		t.Logf("browser process %d (group %d): %s", p.pid, p.pgid, truncate(p.command, 160))
	}
	require.NoError(t, syscall.Kill(pid, syscall.SIGTERM))
	select {
	case <-b.exited:
	case <-time.After(browserDeadline):
		b.kill(t)
		t.Fatalf("the browser did not exit within %s of SIGTERM:\n%s", browserDeadline, b.output.String())
	}
	t.Logf("browser exit: %s", b.cmd.ProcessState)
	assert.True(t, b.cmd.ProcessState.Exited(), "the browser did not shut down by itself: %s", b.cmd.ProcessState)
	if errors.Is(b.waitErr, exec.ErrWaitDelay) {
		t.Logf("a process the browser started kept its output open after the browser exited; it is checked below")
	} else if b.waitErr != nil && !errors.As(b.waitErr, new(*exec.ExitError)) {
		t.Errorf("waiting for the browser: %v", b.waitErr)
	}

	deadline := time.Now().Add(browserDeadline)
	var left []listedProcess
	for {
		left = b.leftover(t, started)
		if len(left) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, p := range left {
		t.Errorf("process %d (group %d) outlived the browser: %s", p.pid, p.pgid, truncate(p.command, 160))
		if !b.belongs(p) {
			continue
		}
		if err := syscall.Kill(p.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("ending process %d: %v", p.pid, err)
		}
	}
}

// processes lists the browser and every process it started: its
// descendants, its process group, and anything naming its profile.
func (b *realBrowser) processes(t *testing.T) []listedProcess {
	all := listProcesses(t)
	root := b.cmd.Process.Pid
	family := map[int]bool{root: true}
	for grew := true; grew; {
		grew = false
		for _, p := range all {
			if family[p.ppid] && !family[p.pid] {
				family[p.pid] = true
				grew = true
			}
		}
	}
	var out []listedProcess
	for _, p := range all {
		if family[p.pid] || b.belongs(p) {
			out = append(out, p)
		}
	}
	return out
}

// leftover returns the processes of started still running, and any other
// process that belongs to the browser.
func (b *realBrowser) leftover(t *testing.T, started []listedProcess) []listedProcess {
	alive := map[int]string{}
	for _, p := range started {
		alive[p.pid] = p.command
	}
	var out []listedProcess
	for _, p := range listProcesses(t) {
		if command, ok := alive[p.pid]; ok && command == p.command || b.belongs(p) {
			out = append(out, p)
		}
	}
	return out
}

// belongs reports whether p is in the browser's process group or names the
// browser's profile.
func (b *realBrowser) belongs(p listedProcess) bool {
	if p.pgid == b.cmd.Process.Pid {
		return true
	}
	resolved, err := filepath.EvalSymlinks(b.profile)
	if err != nil {
		resolved = b.profile
	}
	return strings.Contains(p.command, b.profile) || strings.Contains(p.command, resolved)
}

// kill ends the browser's whole process group when the test ends without
// stopping it, so a failed test leaves no browser behind.
func (b *realBrowser) kill(t *testing.T) {
	select {
	case <-b.exited:
		return
	default:
	}
	t.Errorf("the browser was still running; ending its process group")
	if err := syscall.Kill(-b.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Errorf("ending the browser's process group: %v", err)
	}
	select {
	case <-b.exited:
	case <-time.After(browserDeadline):
		t.Errorf("the browser %d did not exit after SIGKILL", b.cmd.Process.Pid)
	}
}

// listProcesses lists every process on the machine with ps, which macOS and
// Linux both provide.
func listProcesses(t *testing.T) []listedProcess {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-ww", "-o", "pid=,ppid=,pgid=,command=").Output()
	require.NoError(t, err)
	var processes []listedProcess
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		var numbers [3]int
		for i := range numbers {
			numbers[i], err = strconv.Atoi(fields[i])
			require.NoError(t, err, "ps line %q", line)
		}
		processes = append(processes, listedProcess{pid: numbers[0], ppid: numbers[1], pgid: numbers[2], command: strings.Join(fields[3:], " ")})
	}
	return processes
}

// truncate shortens s to at most n bytes for logs.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
