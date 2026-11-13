//go:build browser && (darwin || linux)

// This file proves in a real Chromium-family browser what the jsdom tests in
// views_page_test.go cannot: the browser enforces the views page's
// Content-Security-Policy and framing rule, itself sends the Sec-Fetch-Site
// mark every session request depends on, and lays the page out as its
// stylesheet says, such as the Live target view at a phone's width. It is
// opt-in:
//
//	CI=true go test -race -count=1 -tags browser ./internal/project </dev/null
//
// The test starts a headless browser with a throwaway profile inside its own
// temporary directory, never the user's browser, profile, windows or tabs
// (real_browser_test.go). Set SURLA_TEST_BROWSER to the browser's executable
// when it is not installed in a well-known location. It drives the browser
// with browser_harness.cjs, which needs Node 22 or newer for its built-in
// WebSocket client; with an older Node the test skips.
package project

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/testmodule"
)

type (
	// browserInput is what browser_harness.cjs reads: where the browser's
	// DevTools endpoint and the pages are, and the one-use link to open.
	browserInput struct {
		// DevTools is the browser-wide DevTools WebSocket address.
		DevTools string `json:"devtools"`
		// Phone selects the focused Team and Build layout scenario.
		Phone bool `json:"phone,omitempty"`
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
		// StoryChangeID is the change saved with the Story, and StoryFlow
		// the key of its flow whose row of steps is wider than the page.
		StoryChangeID string `json:"story_change_id"`
		StoryFlow     string `json:"story_flow"`
		// ConfirmBody is a confirm_intent body that the user's session would
		// have the server accept; pages on other origins send it.
		ConfirmBody string `json:"confirm_body"`
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
		// Story is the Story stage of the change saved with a Story.
		Story storyFacts `json:"story"`
		// Live is the Live target view at a phone's width.
		Live liveFacts `json:"live"`
		// Phone records Team and Build at 375 by 800 CSS pixels.
		Phone phoneFacts `json:"phone"`
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
		WithSession string `json:"with_session"`
		// ConfirmWithoutCredentials and ConfirmWithSession are what the
		// page saw of the same two attempts to confirm a change.
		ConfirmWithoutCredentials string `json:"confirm_without_credentials"`
		ConfirmWithSession        string `json:"confirm_with_session"`
		// RequestsSettled says every expected request, preflights included,
		// was reported and answered or failed.
		RequestsSettled bool              `json:"requests_settled"`
		Requests        []observedRequest `json:"requests"`
		Console         []string          `json:"console"`
	}

	// storyFacts is how the browser laid out a row of Story steps wider than
	// the page. Widths are CSS pixels.
	storyFacts struct {
		// Found says the long flow's row of steps rendered.
		Found bool `json:"found"`
		// TabStopTimedOut says the row never became a tab stop, which the
		// page makes it once it measures that the row overflows.
		TabStopTimedOut bool   `json:"tab_stop_timed_out"`
		Steps           int    `json:"steps"`
		TabIndex        int    `json:"tab_index"`
		OverflowX       string `json:"overflow_x"`
		RowScrollWidth  int    `json:"row_scroll_width"`
		RowClientWidth  int    `json:"row_client_width"`
		// ScrolledTo is the row's scroll offset after scrolling it to its end.
		ScrolledTo float64 `json:"scrolled_to"`
		// RowRight and StageRight are the right edges of the row and of the
		// Story stage.
		RowRight   float64 `json:"row_right"`
		StageRight float64 `json:"stage_right"`
		// DocumentScrollWidth is the page's scroll width and ViewportWidth
		// the width of the browser's viewport.
		DocumentScrollWidth int `json:"document_scroll_width"`
		ViewportWidth       int `json:"viewport_width"`
		// Violations are policy reports raised while the change and its
		// Story stage opened.
		Violations []violation `json:"violations"`
	}

	// liveFacts is how the browser laid out the Live target view at a
	// viewport 375 CSS pixels wide.
	liveFacts struct {
		// Found says the view and its model form rendered after the live
		// target was read.
		Found bool `json:"found"`
		// LiveState is the live target's case the view drew.
		LiveState string `json:"live_state"`
		// DirectDisplay and GatewayDisplay are the computed display of the
		// model form's two groups of fields.
		DirectDisplay  string `json:"direct_display"`
		GatewayDisplay string `json:"gateway_display"`
		// DocumentScrollWidth is the page's scroll width and ViewportWidth
		// the width of the browser's viewport.
		DocumentScrollWidth int `json:"document_scroll_width"`
		ViewportWidth       int `json:"viewport_width"`
	}

	// phoneTeamOwner retains real project operations while keeping observations
	// under this layout fixture's explicit snapshot lifetime.
	phoneTeamOwner struct{ *Server }

	// phoneFacts records real layout and keyboard observations, not worker execution.
	phoneFacts struct {
		Team  phoneViewFacts `json:"team"`
		Build phoneViewFacts `json:"build"`
		// Findings includes the expanded user-only findings disclosure.
		Findings                 phoneViewFacts `json:"findings"`
		FindingsRequestCount     int            `json:"findings_request_count"`
		FindingsStatus           int            `json:"findings_status"`
		BuildStatus              int            `json:"build_status"`
		BuildFindingCount        int            `json:"build_finding_count"`
		BuildHasFindingsField    bool           `json:"build_has_findings_field"`
		BuildHasFindingText      bool           `json:"build_has_finding_text"`
		ResponseFindings         []string       `json:"response_findings"`
		RenderedFindings         []string       `json:"rendered_findings"`
		FindingsWrapped          bool           `json:"findings_wrapped"`
		FindingsDisclosureOpened bool           `json:"findings_disclosure_opened"`
		ConfirmationOpened       bool           `json:"confirmation_opened"`
		ConfirmationCancelled    bool           `json:"confirmation_cancelled"`
	}

	// phoneViewFacts is one view with disclosures open and long text present.
	phoneViewFacts struct {
		Found               bool     `json:"found"`
		DocumentScrollWidth int      `json:"document_scroll_width"`
		ViewportWidth       int      `json:"viewport_width"`
		ViewportHeight      int      `json:"viewport_height"`
		AttentionRoles      []string `json:"attention_roles"`
		RoleStates          []string `json:"role_states"`
		Controls            []string `json:"controls"`
		KeyboardControls    []string `json:"keyboard_controls"`
		Overflowing         []string `json:"overflowing"`
		LongText            string   `json:"long_text"`
		WrappedLongText     bool     `json:"wrapped_long_text"`
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

	// storyRefresh is the part of a refresh_story or get_story_refresh
	// answer the test reads.
	storyRefresh struct {
		State      string `json:"state"`
		OpID       string `json:"op_id"`
		Diagnostic string `json:"diagnostic"`
	}
)

const (
	// storyDeadline bounds the guide's Story evaluation, which compiles the
	// workspace's design with the Go toolchain in its sandbox.
	storyDeadline = 3 * time.Minute
	// longStoryFlow is the key of the Story flow whose row of steps is wider
	// than the page.
	longStoryFlow = "audited-refund"
)

// TestViewsPageInARealBrowser opens the views page in a real headless
// browser against the real project server and checks, each in its own
// subtest, what only a real browser shows: the link exchange and the address,
// the browser's own same-origin mark, empty browser storage, a load without
// policy violations, the enforced policy, refused framing, refused requests
// from other origins, a long row of Story steps scrolling inside its stage,
// a reload ending the session, and the browser's cleanup. The Story comes
// from a real evaluation of a workspace design, which takes most of a minute.
func TestViewsPageInARealBrowser(t *testing.T) {
	executable := findBrowser(t)
	node, err := exec.LookPath("node")
	if err != nil {
		missingPageTool(t, "node is required to drive the browser through its DevTools protocol")
	}
	skipWithoutNodeWebSocket(t, node)
	harness, err := filepath.Abs("../../ui/checkpoint/testing/browser_harness.cjs")
	require.NoError(t, err)

	vp := startViewsProject(t, controlplane.Options{WorkspaceDir: longStoryWorkspace(t)})
	origin := vp.state.Origin()
	guide := vp.agent(t, "guide")
	target := proposeChange(t, origin, guide, "chg-refunds", "")
	confirmBody, err := gensurla.EncodeConfirmIntentPayload(&gensurla.ConfirmIntentPayload{
		ChangeID: target.ChangeID, RequestRevision: target.RequestRevision, Answers: map[string]string{}, Examples: target.Examples,
	})
	require.NoError(t, err)
	proposeChange(t, origin, guide, "chg-invoices", "")
	proposeChange(t, origin, guide, "chg-story", evaluateStory(t, origin, guide))
	changeIDs := []string{"chg-refunds", "chg-invoices", "chg-story"}
	link, err := vp.link()
	require.NoError(t, err)

	other := startOtherOrigin(t, origin+"/views")
	started := time.Now()
	browser := startBrowser(t, executable, t.TempDir())
	listening := time.Now()
	facts := driveBrowser(t, node, harness, browserInput{
		DevTools:        browser.devtools,
		Origin:          origin,
		SameSiteOrigin:  other.sameSite,
		CrossSiteOrigin: other.crossSite,
		Link:            link,
		ChangeIDs:       changeIDs,
		StoryChangeID:   "chg-story",
		StoryFlow:       longStoryFlow,
		ConfirmBody:     string(confirmBody),
	})
	targetAfter := readChange(t, origin, guide, target.ChangeID)
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
			assert.True(t, page.RequestsSettled, "the requests from %s did not settle: %+v", page.Origin, page.Requests)
			assert.Equal(t, "answered: opaque 0", page.WithoutCredentials, "what %s could see of its read", page.Origin)
			assert.True(t, strings.HasPrefix(page.WithSession, "refused: TypeError"), "what %s could see of its read with the session: %s", page.Origin, page.WithSession)
			assert.Equal(t, "answered: opaque 0", page.ConfirmWithoutCredentials, "what %s could see of its confirm", page.Origin)
			assert.True(t, strings.HasPrefix(page.ConfirmWithSession, "refused: TypeError"), "what %s could see of its confirm with the session: %s", page.Origin, page.ConfirmWithSession)
			assert.Len(t, requestsTo(page.Requests, http.MethodPost, "/changes/confirm"), 2, "the confirms %s sent", page.Origin)
			reached, refusedOnMark, confirmRefused := 0, 0, 0
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
				if r.Method == http.MethodPost && pathOf(r.URL) == "/changes/confirm" && r.Status == http.StatusForbidden {
					confirmRefused++
				}
				assert.Equal(t, marks[page.Origin], r.SentHeaders["sec-fetch-site"], "Sec-Fetch-Site from %s: %s %s", page.Origin, r.Method, r.URL)
				assert.Equal(t, http.StatusForbidden, r.Status, "the server's answer to %s %s from %s", r.Method, r.URL, page.Origin)
				t.Logf("from %s: %s %s (%s) -> %d sec-fetch-site=%q origin=%q failure=%q", page.Origin, r.Method, pathOf(r.URL), r.Type, r.Status, r.SentHeaders["sec-fetch-site"], r.SentHeaders["origin"], r.Failure)
			}
			assert.Positive(t, reached, "no request from %s reached the server", page.Origin)
			assert.Positive(t, refusedOnMark, "no request from %s without an Origin header was refused on its mark alone", page.Origin)
			assert.Positive(t, confirmRefused, "no confirm from %s reached the server and was refused", page.Origin)
		}
		assert.Equal(t, controlplane.StatusNeedsConfirmation, target.Status, "the targeted change's status before")
		assert.Equal(t, controlplane.StatusNeedsConfirmation, targetAfter.Status, "the targeted change's saved status after the other origins tried to confirm it")
		assert.Equal(t, target.RequestRevision, targetAfter.RequestRevision, "the targeted change's saved request_revision")
		assert.Nil(t, targetAfter.ConfirmedBy, "the targeted change records a confirmation")
		t.Logf("targeted change %s: status %s, request_revision %s before and after", target.ChangeID, targetAfter.Status, targetAfter.RequestRevision)
	})

	t.Run("i the Live target view at a phone's width shows one kind of model field and no horizontal scroll", func(t *testing.T) {
		live := facts.Live
		require.True(t, live.Found, "the Live target view did not render")
		assert.Equal(t, 375, live.ViewportWidth, "the viewport width")
		assert.NotEqual(t, "none", live.DirectDisplay, "the provider fields of the current kind are not displayed")
		assert.Equal(t, "none", live.GatewayDisplay, "the unused gateway fields are displayed")
		assert.LessOrEqual(t, live.DocumentScrollWidth, live.ViewportWidth, "the Live target view widened the page")
		t.Logf("live target view at phone width: state %s, provider fields %s, gateway fields %s, page %d in viewport %d",
			live.LiveState, live.DirectDisplay, live.GatewayDisplay, live.DocumentScrollWidth, live.ViewportWidth)
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

	t.Run("h a row of Story steps wider than the page scrolls inside the stage", func(t *testing.T) {
		story := facts.Story
		require.True(t, story.Found, "the Story stage did not show the change's long flow")
		assert.Equal(t, 10, story.Steps, "steps drawn for the long flow")
		assert.Greater(t, story.RowScrollWidth, story.RowClientWidth, "the row of steps does not overflow")
		assert.Equal(t, "auto", story.OverflowX, "the row's overflow-x")
		assert.Positive(t, story.ScrolledTo, "scrolling the row did not move it")
		assert.False(t, story.TabStopTimedOut, "the overflowing row never became a tab stop")
		assert.Zero(t, story.TabIndex, "the overflowing row's tab index")
		assert.LessOrEqual(t, story.RowRight, story.StageRight, "the row reaches past the Story stage")
		assert.LessOrEqual(t, story.DocumentScrollWidth, story.ViewportWidth, "the row widened the page")
		assert.Empty(t, story.Violations, "opening the Story stage raised policy violations")
		t.Logf("story row: %d steps, scroll width %d in %d, scrolled to %.0f, row right %.0f, stage right %.0f; page %d in viewport %d",
			story.Steps, story.RowScrollWidth, story.RowClientWidth, story.ScrolledTo, story.RowRight, story.StageRight, story.DocumentScrollWidth, story.ViewportWidth)
	})

	t.Run("browser ended and left nothing", func(t *testing.T) {
		assert.Contains(t, facts.Browser.UserAgent, "HeadlessChrome", "the browser did not run headless")
		browser.stopAndCheck(t)
	})
}

// Supervise holds the recorded observations until this test's control plane closes.
func (o *phoneTeamOwner) Supervise(ctx context.Context, _ controlplane.AssignmentReader) error {
	<-ctx.Done()
	return nil
}

// TestTeamAndBuildAtPhoneWidth proves layout against the real server and generated
// HTTP handlers. Terminal observations and saved verdicts are bounded fixtures;
// this does not establish real worker execution, judgment or test acceptance.
func TestTeamAndBuildAtPhoneWidth(t *testing.T) {
	executable := findBrowser(t)
	node, err := exec.LookPath("node")
	require.NoError(t, err)
	skipWithoutNodeWebSocket(t, node)
	harness, err := filepath.Abs("../../ui/checkpoint/testing/browser_harness.cjs")
	require.NoError(t, err)
	vp := startViewsProject(t, controlplane.Options{})
	server := vp.server.Load()
	// This older views fixture leaves Team unbound. Recompose its control plane
	// with the same owner as production, replacing only the observation driver.
	require.NoError(t, vp.cp.Load().Close())
	cp, err := controlplane.New(controlplane.Options{
		CardsDir: vp.state.CardsDir(),
		Team:     &controlplane.TeamBinding{Owner: vp.state.Record.Owner, Service: &phoneTeamOwner{Server: server}},
	})
	require.NoError(t, err)
	application := http.NewServeMux()
	application.Handle("GET /views", controlplane.ViewsPage())
	application.Handle("/", controlplane.NewHTTPHandler(cp, server.Authenticate))
	handler := server.Handler(application)
	vp.cp.Store(cp)
	vp.handler.Store(&handler)
	// Register the longest legal handles and derive needs_you through the existing
	// supervisor owner from three independent, recorded blocked terminal snapshots.
	vp.state.Record.Team = strings.Repeat("t", 32)
	vp.state.Record.Space = "phone-space"
	guide := ""
	for _, role := range []string{"guide", "builder", "test_writer"} {
		handle := role + strings.Repeat("x", 32-len(role))
		agent, err := registerTestAgent(server, handle, role, vp.state.Record.Directory)
		require.NoError(t, err)
		if role == "guide" {
			raw, err := os.ReadFile(agent.CredentialFile)
			require.NoError(t, err)
			guide = string(raw)
		}
	}
	now := time.Now()
	o := &teamSupervisor{server: server, roles: map[string]*supervisedRole{}, started: now}
	o.syncSelection(server.teamSelection())
	for _, role := range o.selection.Roles {
		o.event(t.Context(), TeamEvent{Kind: "snapshot", Role: role.Role, Sequence: 1, Terminals: []TeamTerminal{{
			Address: role.Role, State: "live", Reported: true,
			Status:     &TeamStatus{State: "blocked", Kind: "permission"},
			Foreground: &TeamForeground{Argv: []string{"codex"}},
			Labels:     map[string]string{"chorus.project": vp.state.Record.Directory, "herdr.space": "phone-space", "chorus.handle": role.Handle},
		}}}, now)
	}
	o.publish(t.Context(), now, false)
	// These are layout inputs, not claims that a worker ran or a checker judged tests.
	findingTexts := make([]string, 4)
	for i := range findingTexts {
		findingTexts[i] = fmt.Sprintf("Recorded finding %d: %s. %s", i+1,
			strings.Repeat("unbrokenfinding", 24),
			strings.Repeat("Exercise the preserved behavior before reporting the tests again. ", 3))
	}
	longReason := strings.Repeat("compilerdiagnostic", 24) + ": the recorded compiler result needs the project owner's attention."
	for _, id := range []string{"phone-build", "phone-findings"} {
		card := proposeChange(t, vp.state.Origin(), guide, id, "")
		user := controlplane.WithPrincipal(t.Context(), controlplane.Principal{ID: vp.state.Record.Owner, CallerID: "phone-user", Kind: controlplane.CallerUser})
		confirmed, err := vp.cp.Load().ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
			ChangeID: id, RequestRevision: card.RequestRevision, Answers: map[string]string{}, Examples: card.Examples,
		})
		require.NoError(t, err)
		writer := map[string]any{
			"state": "could_not_run", "read": true, "working_folder": vp.state.Record.Directory,
			"findings": []string{}, "commit": "", "reason": longReason,
			"step": strings.Repeat("Review the recorded project configuration before retrying. ", 7), "checked_at": now.UTC(),
		}
		if id == "phone-findings" {
			writer = map[string]any{
				"state": "needs_changes", "read": true, "working_folder": vp.state.Record.Directory,
				"findings": findingTexts, "commit": "", "checked_at": now.UTC(),
			}
		}
		// Seed the strict owner format only before the browser starts reading. The
		// normal get_build HTTP handler decodes and validates this fixture.
		raw, err := json.Marshal(map[string]any{
			"change_id": id, "request_revision": confirmed.RequestRevision, "started_at": now.UTC(),
			"roles": "builder_and_test_writer", "stories": []any{}, "design_source": "",
			"builder": map[string]any{"read": true}, "test_writer": writer, "report_number": 3,
		})
		require.NoError(t, err)
		sum := sha256.Sum256([]byte(id))
		require.NoError(t, os.WriteFile(filepath.Join(vp.state.CardsDir(), fmt.Sprintf("%x.build.json", sum)), raw, 0o600))
	}
	link, err := vp.link()
	require.NoError(t, err)
	browser := startBrowser(t, executable, t.TempDir())
	facts := driveBrowser(t, node, harness, browserInput{
		DevTools: browser.devtools, Origin: vp.state.Origin(), Link: link,
		ChangeIDs: []string{"phone-build", "phone-findings"}, Phone: true,
	})
	t.Logf("browser: %s; phone observations: %+v", facts.Browser.Product, facts.Phone)
	for name, view := range map[string]phoneViewFacts{"Team": facts.Phone.Team, "Build": facts.Phone.Build, "Findings": facts.Phone.Findings} {
		t.Run(name, func(t *testing.T) {
			assert.True(t, view.Found)
			assert.Equal(t, 375, view.ViewportWidth)
			assert.Equal(t, 800, view.ViewportHeight)
			assert.LessOrEqual(t, view.DocumentScrollWidth, view.ViewportWidth)
			assert.Empty(t, view.Overflowing, "content extends outside the phone viewport")
			assert.True(t, view.WrappedLongText, "long text did not wrap")
			assert.ElementsMatch(t, []string{"guide", "builder", "test_writer"}, view.AttentionRoles)
			for _, role := range []string{"guide", "builder", "test_writer"} {
				assert.Contains(t, view.Controls, "surla-team-attention-show-pane-"+role)
				assert.Contains(t, view.KeyboardControls, "surla-team-attention-show-pane-"+role)
			}
		})
	}
	assert.ElementsMatch(t, []string{"needs_you", "needs_you", "needs_you"}, facts.Phone.Team.RoleStates)
	assert.Contains(t, facts.Phone.Team.LongText, strings.Repeat("x", 21))
	for _, id := range []string{"surla-team-start", "surla-team-refresh", "surla-team-show-guide", "surla-team-show-builder", "surla-team-show-test_writer"} {
		assert.Contains(t, facts.Phone.Team.Controls, id)
		assert.Contains(t, facts.Phone.Team.KeyboardControls, id)
	}
	assert.Contains(t, facts.Phone.Build.LongText, longReason)
	for _, id := range []string{"surla-build-view-team", "surla-build-again"} {
		assert.Contains(t, facts.Phone.Build.Controls, id)
		assert.Contains(t, facts.Phone.Build.KeyboardControls, id)
	}
	assert.True(t, facts.Phone.ConfirmationOpened, "Enter did not open the repeat-build question")
	assert.True(t, facts.Phone.ConfirmationCancelled, "Escape did not cancel the repeat-build question")
	assert.GreaterOrEqual(t, facts.Phone.FindingsRequestCount, 1, "the page never read user findings")
	assert.Equal(t, http.StatusOK, facts.Phone.FindingsStatus)
	assert.Equal(t, http.StatusOK, facts.Phone.BuildStatus)
	assert.Equal(t, len(findingTexts), facts.Phone.BuildFindingCount)
	assert.False(t, facts.Phone.BuildHasFindingsField, "get_build exposed a findings array")
	assert.False(t, facts.Phone.BuildHasFindingText, "get_build exposed finding text")
	assert.Equal(t, findingTexts, facts.Phone.ResponseFindings, "user findings HTTP response")
	assert.Equal(t, findingTexts, facts.Phone.RenderedFindings, "expanded findings list")
	assert.True(t, facts.Phone.FindingsWrapped, "each long finding must wrap")
	assert.True(t, facts.Phone.FindingsDisclosureOpened, "Enter did not open the findings disclosure")
	assert.Contains(t, facts.Phone.Findings.KeyboardControls, "SUMMARY:Read 4 findings")
	for _, id := range []string{"surla-build-view-team", "surla-build-again"} {
		assert.Contains(t, facts.Phone.Findings.Controls, id)
		assert.Contains(t, facts.Phone.Findings.KeyboardControls, id)
	}
	assert.Contains(t, facts.Browser.UserAgent, "HeadlessChrome")
	browser.stopAndCheck(t)
}

// skipWithoutNodeWebSocket skips the test when node is older than 22, the
// first release whose built-in WebSocket client browser_harness.cjs uses.
func skipWithoutNodeWebSocket(t *testing.T, node string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "--version").Output()
	require.NoError(t, err)
	version := strings.TrimSpace(string(out))
	major, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(version, "v"), ".", 2)[0])
	require.NoError(t, err, "node --version printed %q", version)
	if major < 22 {
		t.Skipf("the browser harness needs Node 22 or newer for its built-in WebSocket; %s is %s", node, version)
	}
}

// proposeChange saves change id through the guide's credential, as the
// guide agent does, and returns the saved card. A non-empty storyOp adopts
// that succeeded Story evaluation into the change.
func proposeChange(t *testing.T, origin, credential, id, storyOp string) *gensurla.ChangeCardResult {
	t.Helper()
	payload := map[string]any{
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
	}
	if storyOp != "" {
		payload["story_refresh_op_id"] = storyOp
	}
	body, err := json.Marshal(payload)
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
	card, err := gensurla.DecodeChangeCardResult(answer)
	require.NoError(t, err)
	return card
}

// readChange reads change id's saved card with the guide's credential.
func readChange(t *testing.T, origin, credential, id string) *gensurla.ChangeCardResult {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, origin+"/changes/"+url.PathEscape(id), nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+credential)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	answer, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode, "reading %s: %s", id, answer)
	card, err := gensurla.DecodeChangeCardResult(answer)
	require.NoError(t, err)
	return card
}

// longStoryWorkspace writes a Goa design workspace whose Story has one flow
// of ten steps, each naming an unbroken receipt identifier, so the flow's
// row of steps is wider than the page, and tidies its module.
func longStoryWorkspace(t *testing.T) string {
	t.Helper()
	steps := ""
	for i := range 8 {
		steps += fmt.Sprintf("\t\tStep(\"check-part-%c\", \"manager\", \"Manager checks part %c of receipt %s\")\n",
			'a'+i, 'A'+i, strings.Repeat("0123456789", 6))
	}
	design := `package design

import . "github.com/goadesign/surla/story/dsl"

var _ = Story("Order Refunds", func() {
	Actor("customer", "Customer")
	Actor("manager", "Support Manager")

	Flow("` + longStoryFlow + `", "Refund checked part by part", func() {
		StartsWith("request-refund", "customer", "Customer requests a refund for a split receipt")
` + steps + `		Outcome("refund-paid", "customer", "Every checked part is refunded")
	})
})
`
	return storyWorkspace(t, testmodule.Tidy(t, map[string]string{
		"go.mod": "module example.com/story-design\n\ngo 1.27.3\n\nrequire (\n" +
			"\tgithub.com/goadesign/surla/story v0.0.0-20261111183703-4568fcf5bb85\n" +
			"\tgoa.design/goa/v3 " + testmodule.SurlaVersion(t, "goa.design/goa/v3") + "\n)\n",
		"design/design.go": design,
	}))
}

// evaluateStory has the guide evaluate the workspace's Story through the
// project server, as the guide agent does, waits until the evaluation ends,
// and returns the op_id of the succeeded evaluation.
func evaluateStory(t *testing.T, origin, guide string) string {
	t.Helper()
	started := storyRefreshCall(t, origin, guide, http.MethodPost)
	require.Equal(t, "running", started.State, "the guide could not start a Story evaluation")
	deadline := time.Now().Add(storyDeadline)
	for {
		current := storyRefreshCall(t, origin, guide, http.MethodGet)
		require.Equal(t, started.OpID, current.OpID, "the guide reads another evaluation")
		if current.State != "running" {
			require.Equal(t, "succeeded", current.State, "the Story evaluation: %s", current.Diagnostic)
			return current.OpID
		}
		require.True(t, time.Now().Before(deadline), "the Story evaluation did not end within %s", storyDeadline)
		time.Sleep(250 * time.Millisecond)
	}
}

// storyRefreshCall starts (POST) or reads (GET) the guide's Story
// evaluation with the guide's credential.
func storyRefreshCall(t *testing.T, origin, guide, method string) storyRefresh {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, origin+"/workspace/story/refresh", nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+guide)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	answer, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s /workspace/story/refresh: %s", method, answer)
	var refresh storyRefresh
	require.NoError(t, json.Unmarshal(answer, &refresh))
	return refresh
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
