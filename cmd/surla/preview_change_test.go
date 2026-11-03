package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goahttp "goa.design/goa/v3/http"

	gensurlaserver "github.com/goadesign/surla/gen/http/surla/server"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/swarm"
	"github.com/goadesign/surla/internal/testmodule"
)

type (
	// staticCP1Analyzer returns one fixed CP1SemanticAnalysis for controlplane
	// tests of `surla preview-change`.
	staticCP1Analyzer struct {
		// analysis is the CP1 semantic result returned for every Analyze call.
		analysis controlplane.CP1SemanticAnalysis
	}

	// recordingPreviewService implements gensurla.Service for HTTP transport
	// tests that inspect the exact wire payload `surla preview-change` sends.
	recordingPreviewService struct {
		gensurla.Service
		// mu guards lastPayload.
		mu sync.Mutex
		// lastPayload is the most recent PreviewArchitecturePayload decoded by
		// the generated HTTP server.
		lastPayload *gensurla.PreviewArchitecturePayload
		// result is the ArchitecturePreviewResult returned by PreviewArchitecture.
		result *gensurla.ArchitecturePreviewResult
	}
)

// previewTestDesign is a compact Goa + Goa-AI design with Unicode and CRLF
// line endings used to test end-to-end `surla preview-change` evaluation.
const previewTestDesign = "package design\r\n\r\n" +
	"import (\r\n" +
	"\t. \"goa.design/goa/v3/dsl\"\r\n" +
	"\t. \"goa.design/goa-ai/dsl\"\r\n" +
	")\r\n\r\n" +
	"// VIP refund limit: €150 instant store credit.\r\n" +
	"var _ = API(\"orders\", func() {})\r\n\r\n" +
	"var CreditTools = Toolset(\"credit\", func() {\r\n" +
	"\tTool(\"issue_credit\", \"Issue instant store credit up to €150\", func() {\r\n" +
	"\t\tArgs(func() {\r\n" +
	"\t\t\tAttribute(\"tenant_id\", String)\r\n" +
	"\t\t\tAttribute(\"order_id\", String)\r\n" +
	"\t\t\tRequired(\"tenant_id\", \"order_id\")\r\n" +
	"\t\t})\r\n" +
	"\t\tReturn(func() {\r\n" +
	"\t\t\tAttribute(\"status\", String)\r\n" +
	"\t\t\tRequired(\"status\")\r\n" +
	"\t\t})\r\n" +
	"\t\tInject(\"tenant_id\")\r\n" +
	"\t})\r\n" +
	"})\r\n\r\n" +
	"var _ = Service(\"orders\", func() {\r\n" +
	"\tAgent(\"refund\", \"VIP refund assistant\", func() {\r\n" +
	"\t\tUse(CreditTools)\r\n" +
	"\t\tRunPolicy(func() {\r\n" +
	"\t\t\tTimeBudget(\"2m\")\r\n" +
	"\t\t\tDefaultCaps(MaxToolCalls(4))\r\n" +
	"\t\t})\r\n" +
	"\t})\r\n" +
	"})\r\n"

// tidyPreviewModuleOnce caches the tidy module snapshot for previewTestDesign
// so parallel subtests do not repeat `go mod tidy`.
var (
	tidyPreviewModuleOnce  sync.Once
	tidyPreviewModuleFiles map[string]string
)

// Analyze returns a.analysis for every CP1 ProposeChange call.
func (a staticCP1Analyzer) Analyze(context.Context, string, string) (controlplane.CP1SemanticAnalysis, error) {
	return a.analysis, nil
}

// Revise returns a.analysis for every CP1 ReviseChange call.
func (a staticCP1Analyzer) Revise(context.Context, controlplane.CP1PriorCard, string, string) (controlplane.CP1SemanticAnalysis, error) {
	return a.analysis, nil
}

// PreviewArchitecture records p and returns s.result.
func (s *recordingPreviewService) PreviewArchitecture(_ context.Context, p *gensurla.PreviewArchitecturePayload) (*gensurla.ArchitecturePreviewResult, error) {
	s.mu.Lock()
	s.lastPayload = p
	s.mu.Unlock()
	return s.result, nil
}

func TestRunPreviewChangeEndToEndThroughControlPlane(t *testing.T) {
	files := writeTidyPreviewModule(t)
	profiles := []swarm.ProviderProfile{
		{
			ID:            "claude-builder",
			Provider:      "anthropic",
			Model:         "claude-sonnet-4-6",
			Family:        "claude",
			Authenticated: true,
			Capabilities:  []swarm.RoleKind{swarm.RoleBuilder},
		},
		{
			ID:            "gpt-lead",
			Provider:      "openai",
			Model:         "gpt-5.4",
			Family:        "gpt",
			Authenticated: true,
			Capabilities:  []swarm.RoleKind{swarm.RoleArchitect, swarm.RoleEvalAuthor, swarm.RoleReviewer},
		},
	}
	cp, err := controlplane.New(controlplane.Options{
		CardsDir:      t.TempDir(),
		SwarmProfiles: profiles,
		Analyzer: staticCP1Analyzer{
			analysis: controlplane.CP1SemanticAnalysis{
				Summary:     "Give VIP customers instant store credit up to €150.",
				Ambiguities: []controlplane.CP1ModelAmbiguity{},
				Examples: []controlplane.CP1ModelExample{{
					Scenario:         "VIP refund within €150",
					InputSummary:     "VIP customer, €120 order total",
					ExpectedOutcome:  "Instant store credit",
					VerificationKind: "deterministic",
				}},
				PreservedBehaviors: []string{"Non-VIP orders over €50 still require manager approval."},
			},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })

	aliceCtx := controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: "alice@example.com"})
	card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "vip-refunds",
		Prompt:   "Give VIP customers instant store credit up to €150.",
	})
	require.NoError(t, err)
	require.Equal(t, controlplane.StatusConfirmed, card.Status)

	var reqCount atomic.Int32
	auth := controlplane.NewTokenAuthenticator(map[string]controlplane.Principal{
		"alice-token": {ID: "alice@example.com"},
		"bob-token":   {ID: "bob@example.com"},
	})
	handler := controlplane.NewHTTPHandler(cp, auth)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	modDir := writeFilesToDir(t, files)
	buildersPath := filepath.Join(t.TempDir(), "builders.json")
	require.NoError(t, os.WriteFile(buildersPath, []byte(`[
		{"name": "orders", "paths": ["internal/orders/**"], "preferred_profile_id": "claude-builder"}
	]`), 0o600))

	// 1a. Confirmed card + -builders alone produces a 2-role (@builder-orders + @reviewer) roster.
	var stdout2Role bytes.Buffer
	err = runPreviewChange(
		[]string{
			"-server", srv.URL,
			"-change-id", card.ChangeID,
			"-request-revision", card.RequestRevision,
			"-dir", modDir,
			"-builders", buildersPath,
		},
		&stdout2Role,
		func(k string) string {
			if k == "SURLA_AUTH_TOKEN" {
				return "alice-token"
			}
			return ""
		},
	)
	require.NoError(t, err)
	var got2Role struct {
		Swarm struct {
			Status            string `json:"status"`
			CrossFamilyReview bool   `json:"cross_family_review"`
			CrossFamilyEval   bool   `json:"cross_family_eval"`
			Manifest          string `json:"manifest"`
			Assignments       []struct {
				Role       string   `json:"role"`
				Handle     string   `json:"handle"`
				OwnedPaths []string `json:"owned_paths"`
			} `json:"assignments"`
		} `json:"swarm"`
	}
	require.NoError(t, json.Unmarshal(stdout2Role.Bytes(), &got2Role))
	assert.Equal(t, string(swarm.StatusConfigured), got2Role.Swarm.Status)
	assert.True(t, got2Role.Swarm.CrossFamilyReview)
	assert.False(t, got2Role.Swarm.CrossFamilyEval)
	assert.Contains(t, got2Role.Swarm.Manifest, "eval-author not assigned")
	require.Len(t, got2Role.Swarm.Assignments, 2)
	assert.Equal(t, "builder", got2Role.Swarm.Assignments[0].Role)
	assert.Equal(t, "reviewer", got2Role.Swarm.Assignments[1].Role)

	// 1b. Confirmed card + -builders + repeatable -architect-path and -eval-path (preserving commas and spaces verbatim) produces a full 4-role roster.
	var stdout bytes.Buffer
	err = runPreviewChange(
		[]string{
			"-server", srv.URL,
			"-change-id", card.ChangeID,
			"-request-revision", card.RequestRevision,
			"-dir", modDir,
			"-builders", buildersPath,
			"-architect-path", "design/**",
			"-eval-path", "*-eval/**",
			"-eval-path", " eval/a,b/**",
		},
		&stdout,
		func(k string) string {
			if k == "SURLA_AUTH_TOKEN" {
				return "alice-token"
			}
			return ""
		},
	)
	require.NoError(t, err)

	var got struct {
		ChangeID        string `json:"change_id"`
		RequestRevision string `json:"request_revision"`
		DesignDigest    string `json:"design_digest"`
		SystemName      string `json:"system_name"`
		Containers      []struct {
			ID string `json:"id"`
		} `json:"containers"`
		Edges         []any `json:"edges"`
		AgentPolicies []struct {
			AgentID             string `json:"agent_id"`
			MaxExecutionSeconds int    `json:"max_execution_seconds"`
			MaxToolCalls        int    `json:"max_tool_calls"`
		} `json:"agent_policies"`
		Tools []struct {
			Name           string   `json:"name"`
			InjectedFields []string `json:"injected_fields"`
		} `json:"tools"`
		Swarm struct {
			Status            string `json:"status"`
			CrossFamilyReview bool   `json:"cross_family_review"`
			CrossFamilyEval   bool   `json:"cross_family_eval"`
			Manifest          string `json:"manifest"`
			Assignments       []struct {
				Role       string   `json:"role"`
				Handle     string   `json:"handle"`
				ProfileID  string   `json:"profile_id"`
				OwnedPaths []string `json:"owned_paths"`
			} `json:"assignments"`
		} `json:"swarm"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, card.ChangeID, got.ChangeID)
	assert.Equal(t, card.RequestRevision, got.RequestRevision)
	assert.Equal(t, designeval.SnapshotDigest(files), got.DesignDigest)
	assert.Equal(t, "orders", got.SystemName)
	assert.NotEmpty(t, got.Containers)
	assert.NotNil(t, got.Edges)
	require.Len(t, got.AgentPolicies, 1)
	assert.Equal(t, "agent:orders.refund", got.AgentPolicies[0].AgentID)
	assert.Equal(t, 120, got.AgentPolicies[0].MaxExecutionSeconds)
	assert.Equal(t, 4, got.AgentPolicies[0].MaxToolCalls)
	require.Len(t, got.Tools, 1)
	assert.Equal(t, "issue_credit", got.Tools[0].Name)
	assert.Equal(t, []string{"tenant_id"}, got.Tools[0].InjectedFields)
	assert.Equal(t, string(swarm.StatusConfigured), got.Swarm.Status)
	assert.True(t, got.Swarm.CrossFamilyReview)
	assert.True(t, got.Swarm.CrossFamilyEval)
	assert.Contains(t, got.Swarm.Manifest, "@builder-orders")
	require.Len(t, got.Swarm.Assignments, 4)
	assert.Equal(t, []string{" eval/a,b/**", "*-eval/**"}, got.Swarm.Assignments[2].OwnedPaths)

	// 2. Omitting -builders when profiles are configured returns a solo unconfigured roster with no missing_roles.
	stdout.Reset()
	require.NoError(t, run(
		[]string{
			"preview-change",
			"-server", srv.URL,
			"-change-id", card.ChangeID,
			"-request-revision", card.RequestRevision,
			"-dir", modDir,
			"-auth-token", "alice-token",
		},
		&stdout,
	))
	var withoutBuilders struct {
		Swarm struct {
			Status       string   `json:"status"`
			MissingRoles []string `json:"missing_roles"`
			Summary      string   `json:"summary"`
		} `json:"swarm"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &withoutBuilders))
	assert.Equal(t, string(swarm.StatusUnconfigured), withoutBuilders.Swarm.Status)
	assert.Empty(t, withoutBuilders.Swarm.MissingRoles)
	assert.Contains(t, withoutBuilders.Swarm.Summary, "Swarm roster: off (solo workflow")

	// 3. Wrong bearer token, wrong principal, missing card, unconfirmed card, and stale request_revision fail without retries.
	unconfirmedCard, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "pending-card",
		Prompt:   "Ambiguous change.",
	})
	require.NoError(t, err)
	// Create an unconfirmed card using a second controlplane analyzer with an ambiguity.
	cpAmbiguous, err := controlplane.New(controlplane.Options{
		CardsDir: t.TempDir(),
		Analyzer: staticCP1Analyzer{
			analysis: controlplane.CP1SemanticAnalysis{
				Summary: "Needs answer.",
				Ambiguities: []controlplane.CP1ModelAmbiguity{{
					Question: "Include shipping?",
					Options:  []string{"Yes", "No"},
				}},
				Examples: []controlplane.CP1ModelExample{{
					Scenario:         "Example",
					InputSummary:     "Input",
					ExpectedOutcome:  "Outcome",
					VerificationKind: "deterministic",
				}},
				PreservedBehaviors: []string{"Preserve auth."},
			},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cpAmbiguous.Close()) })
	pending, err := cpAmbiguous.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "needs-confirm",
		Prompt:   "Needs answer.",
	})
	require.NoError(t, err)
	require.Equal(t, controlplane.StatusNeedsConfirmation, pending.Status)
	ambiguousSrv := httptest.NewServer(controlplane.NewHTTPHandler(cpAmbiguous, auth))
	t.Cleanup(ambiguousSrv.Close)

	for _, tc := range []struct {
		name      string
		serverURL string
		token     string
		changeID  string
		revision  string
		wantErr   string
	}{
		{
			name:      "wrong bearer token",
			serverURL: srv.URL,
			token:     "wrong-token",
			changeID:  card.ChangeID,
			revision:  card.RequestRevision,
			wantErr:   "invalid response code 401, body: unauthorized",
		},
		{
			name:      "wrong principal",
			serverURL: srv.URL,
			token:     "bob-token",
			changeID:  card.ChangeID,
			revision:  card.RequestRevision,
			wantErr:   `principal \"bob@example.com\" is not the owner of change_id \"vip-refunds\"`,
		},
		{
			name:      "missing card",
			serverURL: srv.URL,
			token:     "alice-token",
			changeID:  "no-such-card",
			revision:  card.RequestRevision,
			wantErr:   `unknown change_id \"no-such-card\"`,
		},
		{
			name:      "unconfirmed card",
			serverURL: ambiguousSrv.URL,
			token:     "alice-token",
			changeID:  pending.ChangeID,
			revision:  pending.RequestRevision,
			wantErr:   `change_id \"needs-confirm\" must be confirmed before architecture preview`,
		},
		{
			name:      "stale request_revision makes one call with no retry",
			serverURL: srv.URL,
			token:     "alice-token",
			changeID:  unconfirmedCard.ChangeID,
			revision:  "0000000000000000000000000000000000000000000000000000000000000000",
			wantErr:   "request_revision mismatch",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := reqCount.Load()
			var out bytes.Buffer
			err := runPreviewChange(
				[]string{
					"-server", tc.serverURL,
					"-change-id", tc.changeID,
					"-request-revision", tc.revision,
					"-dir", modDir,
					"-auth-token", tc.token,
				},
				&out,
				func(string) string { return "" },
			)
			require.ErrorContains(t, err, tc.wantErr)
			assert.Empty(t, out.String())
			if tc.serverURL == srv.URL {
				assert.Equal(t, before+1, reqCount.Load(), "must make exactly 1 HTTP request with no automatic retry")
			}
		})
	}
}

func TestRunPreviewChangePreservesExactSnapshotBytesAndEmptyArrays(t *testing.T) {
	t.Parallel()
	modDir := t.TempDir()
	wantFiles := map[string]string{
		"go.mod":           "module example.com/orders\r\n\r\ngo 1.27.3\r\n",
		"go.sum":           "example.com/dep v1.0.0 h1:checksum==\nexample.com/dep v1.0.0/go.mod h1:modsum==\n",
		"design/design.go": "package design\r\n// Unicode €150 — exact CRLF\r\n",
	}
	for rel, content := range wantFiles {
		p := filepath.Join(modDir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}

	svc := &recordingPreviewService{
		result: &gensurla.ArchitecturePreviewResult{
			ChangeID:        "vip-refunds",
			RequestRevision: "rev-123",
			DesignDigest:    "digest-456",
			SystemName:      "orders",
			Swarm: &gensurla.SwarmPlanItem{
				Status:       string(swarm.StatusUnconfigured),
				MissingRoles: []string{"architect", "builder", "eval_author", "reviewer"},
				Summary:      "No profiles configured.",
			},
			Banner: "orders: 0 container(s), 0 boundary edge(s), 0 agent policy(s), 0 tool contract(s)",
		},
	}
	mux := goahttp.NewMuxer()
	endpoints := gensurla.NewEndpoints(svc)
	server := gensurlaserver.New(endpoints, mux, goahttp.RequestDecoder, goahttp.ResponseEncoder, nil, nil)
	gensurlaserver.Mount(mux, server)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var stdout bytes.Buffer
	err := runPreviewChange(
		[]string{
			"-server", srv.URL,
			"-change-id", "vip-refunds",
			"-request-revision", "rev-123",
			"-dir", modDir,
			"-auth-token", "secret-token",
		},
		&stdout,
		func(string) string { return "" },
	)
	require.NoError(t, err)

	svc.mu.Lock()
	gotPayload := svc.lastPayload
	svc.mu.Unlock()
	require.NotNil(t, gotPayload)
	require.NotNil(t, gotPayload.Design)
	assert.Equal(t, wantFiles, gotPayload.Design.Files)

	out := stdout.String()
	assert.Contains(t, out, `"containers": []`)
	assert.Contains(t, out, `"edges": []`)
	assert.Contains(t, out, `"agent_policies": []`)
	assert.Contains(t, out, `"tools": []`)
	assert.Contains(t, out, `"assignments": []`)
}

func TestRunPreviewChangeRefusesRedirectsTimeoutAndInvalidUTF8(t *testing.T) {
	t.Parallel()
	validDir := writeFilesToDir(t, map[string]string{
		"go.mod":           "module example.com/orders\n",
		"design/design.go": "package design\n",
	})

	t.Run("redirect refusal never forwards credentials to target", func(t *testing.T) {
		t.Parallel()
		var targetRequests atomic.Int32
		var targetAuth atomic.Value
		targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			targetRequests.Add(1)
			targetAuth.Store(r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(targetSrv.Close)

		redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, targetSrv.URL+r.URL.Path, http.StatusFound)
		}))
		t.Cleanup(redirectSrv.Close)

		var out bytes.Buffer
		err := runPreviewChange(
			[]string{
				"-server", redirectSrv.URL,
				"-change-id", "vip-refunds",
				"-request-revision", "rev-123",
				"-dir", validDir,
				"-auth-token", "secret-token",
			},
			&out,
			func(string) string { return "" },
		)
		require.ErrorContains(t, err, "redirects are not followed")
		assert.EqualValues(t, 0, targetRequests.Load(), "redirect target must receive 0 requests")
		assert.Nil(t, targetAuth.Load(), "redirect target must never receive Authorization header")
	})

	t.Run("timeout stops slow HTTP preview request", func(t *testing.T) {
		t.Parallel()
		unblock := make(chan struct{})
		slowSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			<-unblock
		}))
		t.Cleanup(func() {
			close(unblock)
			slowSrv.Close()
		})

		var out bytes.Buffer
		err := runPreviewChange(
			[]string{
				"-server", slowSrv.URL,
				"-change-id", "vip-refunds",
				"-request-revision", "rev-123",
				"-dir", validDir,
				"-auth-token", "secret-token",
				"-timeout", "25ms",
			},
			&out,
			func(string) string { return "" },
		)
		require.Error(t, err)
		assert.Empty(t, out.String())
	})

	t.Run("invalid UTF-8 in snapshot fails before any network request", func(t *testing.T) {
		t.Parallel()
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)

		badDir := writeFilesToDir(t, map[string]string{
			"go.mod":           "module example.com/orders\n",
			"design/design.go": "package design\n// invalid utf8: \xff\xfe\n",
		})
		var out bytes.Buffer
		err := runPreviewChange(
			[]string{
				"-server", srv.URL,
				"-change-id", "vip-refunds",
				"-request-revision", "rev-123",
				"-dir", badDir,
				"-auth-token", "secret-token",
			},
			&out,
			func(string) string { return "" },
		)
		require.ErrorContains(t, err, "design snapshot file design/design.go is not valid UTF-8")
		assert.EqualValues(t, 0, requests.Load(), "server must not be contacted when snapshot contains invalid UTF-8")
	})

	t.Run("invalid raw UTF-8 in -builders fails before any network request", func(t *testing.T) {
		t.Parallel()
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)

		buildersPath := filepath.Join(t.TempDir(), "builders.json")
		require.NoError(t, os.WriteFile(buildersPath, []byte("[{\"name\":\"orders\",\"paths\":[\"internal/\xff/**\"]}]"), 0o600))
		var out bytes.Buffer
		err := runPreviewChange(
			[]string{
				"-server", srv.URL,
				"-change-id", "vip-refunds",
				"-request-revision", "rev-123",
				"-dir", validDir,
				"-auth-token", "secret-token",
				"-builders", buildersPath,
			},
			&out,
			func(string) string { return "" },
		)
		require.ErrorContains(t, err, "is not valid UTF-8")
		assert.EqualValues(t, 0, requests.Load(), "server must not be contacted when -builders contains invalid UTF-8")
	})
}

func TestParsePreviewChangeFlagsAndServerOrigin(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		args    []string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "missing all required flags",
			args:    nil,
			wantErr: "preview-change requires -server, -change-id, -request-revision, -auth-token (or SURLA_AUTH_TOKEN)",
		},
		{
			name:    "unexpected positional argument",
			args:    []string{"-server", "http://127.0.0.1:8090", "extra"},
			wantErr: `unexpected arguments ["extra"]`,
		},
		{
			name: "zero timeout",
			args: []string{
				"-server", "http://127.0.0.1:8090",
				"-change-id", "vip",
				"-request-revision", "rev",
				"-auth-token", "tok",
				"-timeout", "0s",
			},
			wantErr: "-timeout must be greater than 0",
		},
		{
			name: "negative timeout",
			args: []string{
				"-server", "http://127.0.0.1:8090",
				"-change-id", "vip",
				"-request-revision", "rev",
				"-auth-token", "tok",
				"-timeout", "-1s",
			},
			wantErr: "-timeout must be greater than 0",
		},
		{
			name: "empty dir",
			args: []string{
				"-server", "http://127.0.0.1:8090",
				"-change-id", "vip",
				"-request-revision", "rev",
				"-auth-token", "tok",
				"-dir", "   ",
			},
			wantErr: "-dir must not be empty",
		},
		{
			name: "invalid UTF-8 in change-id",
			args: []string{
				"-server", "http://127.0.0.1:8090",
				"-change-id", "vip-\xff",
				"-request-revision", "rev",
				"-auth-token", "tok",
			},
			wantErr: "-change-id must be valid UTF-8",
		},
		{
			name: "invalid UTF-8 in request-revision",
			args: []string{
				"-server", "http://127.0.0.1:8090",
				"-change-id", "vip",
				"-request-revision", "rev-\xff",
				"-auth-token", "tok",
			},
			wantErr: "-request-revision must be valid UTF-8",
		},
		{
			name: "architect-path without builders",
			args: []string{
				"-server", "http://127.0.0.1:8090",
				"-change-id", "vip",
				"-request-revision", "rev",
				"-auth-token", "tok",
				"-architect-path", "design/**",
			},
			wantErr: "-architect-path and -eval-path require -builders",
		},
		{
			name: "eval-path without builders",
			args: []string{
				"-server", "http://127.0.0.1:8090",
				"-change-id", "vip",
				"-request-revision", "rev",
				"-auth-token", "tok",
				"-eval-path", "eval/**",
			},
			wantErr: "-architect-path and -eval-path require -builders",
		},
		{
			name: "invalid UTF-8 in architect-path",
			args: []string{
				"-server", "http://127.0.0.1:8090",
				"-change-id", "vip",
				"-request-revision", "rev",
				"-auth-token", "tok",
				"-builders", "builders.json",
				"-architect-path", "design/\xff",
			},
			wantErr: "path must be valid UTF-8",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parsePreviewChangeFlags(tc.args, func(k string) string { return tc.env[k] })
			require.ErrorContains(t, err, tc.wantErr)
		})
	}

	for _, tc := range []struct {
		name       string
		raw        string
		wantScheme string
		wantHost   string
		wantErr    string
	}{
		{
			name:       "http origin",
			raw:        "http://127.0.0.1:8090",
			wantScheme: "http",
			wantHost:   "127.0.0.1:8090",
		},
		{
			name:       "https origin with root slash",
			raw:        "https://surla.example.com:8443/",
			wantScheme: "https",
			wantHost:   "surla.example.com:8443",
		},
		{
			name:    "userinfo rejected without echoing secret",
			raw:     "http://operator:super-secret-cred@127.0.0.1:8090",
			wantErr: "-server must not include user credentials",
		},
		{
			name:    "malformed URL with userinfo rejected without echoing secret",
			raw:     "http://operator:super-secret-cred@127.0.0.1:badport",
			wantErr: "invalid -server URL",
		},
		{
			name:    "unsupported scheme",
			raw:     "grpc://127.0.0.1:8090",
			wantErr: "-server scheme must be http or https",
		},
		{
			name:    "missing host",
			raw:     "http:///mcp",
			wantErr: "-server must include a host",
		},
		{
			name:    "empty hostname with port",
			raw:     "http://:8090",
			wantErr: "-server must include a host",
		},
		{
			name:    "non-root path rejected",
			raw:     "http://127.0.0.1:8090/mcp",
			wantErr: "-server must be an origin URL without a path, query, or fragment",
		},
		{
			name:    "query rejected",
			raw:     "http://127.0.0.1:8090?token=super-secret-cred",
			wantErr: "-server must be an origin URL without a path, query, or fragment",
		},
		{
			name:    "fragment rejected",
			raw:     "http://127.0.0.1:8090#frag",
			wantErr: "-server must be an origin URL without a path, query, or fragment",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scheme, host, err := parseServerOrigin(tc.raw)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.NotContains(t, err.Error(), "super-secret-cred")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantScheme, scheme)
			assert.Equal(t, tc.wantHost, host)
		})
	}
}

func TestLoadBuilderScopesDecodesStrictly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		content     string
		wantName    string
		wantPaths   []string
		wantProfile string
		wantErr     string
	}{
		{
			name:        "valid builder scopes with preferred profile",
			content:     `[{"name":"orders","paths":["internal/orders/**"],"preferred_profile_id":"claude-builder"}]`,
			wantName:    "orders",
			wantPaths:   []string{"internal/orders/**"},
			wantProfile: "claude-builder",
		},
		{
			name:        "valid Unicode including literal U+FFFD is preserved",
			content:     "[{\"name\":\"orders-€-\xef\xbf\xbd\",\"paths\":[\"internal/€-\\ufffd/**\"],\"preferred_profile_id\":\"claude-\xef\xbf\xbd\"}]",
			wantName:    "orders-€-\ufffd",
			wantPaths:   []string{"internal/€-\ufffd/**"},
			wantProfile: "claude-\ufffd",
		},
		{
			name:    "raw invalid UTF-8 in name",
			content: "[{\"name\":\"orders-\xff\",\"paths\":[\"internal/**\"]}]",
			wantErr: "is not valid UTF-8",
		},
		{
			name:    "raw invalid UTF-8 in path",
			content: "[{\"name\":\"orders\",\"paths\":[\"internal/\xff/**\"]}]",
			wantErr: "is not valid UTF-8",
		},
		{
			name:    "raw invalid UTF-8 in preferred_profile_id",
			content: "[{\"name\":\"orders\",\"paths\":[\"internal/**\"],\"preferred_profile_id\":\"claude-\xff\"}]",
			wantErr: "is not valid UTF-8",
		},
		{
			name:    "null array",
			content: `null`,
			wantErr: "expected a JSON array, got null",
		},
		{
			name:    "null element",
			content: `[null]`,
			wantErr: "expected an object, got null",
		},
		{
			name:    "unknown field",
			content: `[{"name":"orders","paths":["internal/**"],"extra":true}]`,
			wantErr: `unknown field "extra"`,
		},
		{
			name:    "trailing data",
			content: `[{"name":"orders","paths":["internal/**"]}] {}`,
			wantErr: "trailing data after the builder array",
		},
		{
			name:    "missing name",
			content: `[{"paths":["internal/**"]}]`,
			wantErr: `"name" is missing from body`,
		},
		{
			name:    "missing paths",
			content: `[{"name":"orders"}]`,
			wantErr: `"paths" is missing from body`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "builders.json")
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))
			scopes, err := loadBuilderScopes(path)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, scopes, 1)
			assert.Equal(t, tc.wantName, *scopes[0].Name)
			assert.Equal(t, tc.wantPaths, scopes[0].Paths)
			require.NotNil(t, scopes[0].PreferredProfileID)
			assert.Equal(t, tc.wantProfile, *scopes[0].PreferredProfileID)
		})
	}
}

func TestPreviewChangeHelpNeverPrintsTheEnvironmentToken(t *testing.T) {
	const token = "synthetic-preview-token-9b2e"
	t.Setenv("SURLA_AUTH_TOKEN", token)
	for _, args := range [][]string{{"preview-change", "-h"}, {"help", "preview-change"}} {
		stderr := captureStderr(t, func() {
			var stdout bytes.Buffer
			require.NoError(t, run(args, &stdout))
			assert.NotContains(t, stdout.String(), token)
		})
		assert.Contains(t, stderr, "-auth-token")
		assert.Contains(t, stderr, "-server")
		assert.Contains(t, stderr, "-change-id")
		assert.Contains(t, stderr, "-request-revision")
		assert.NotContains(t, stderr, token, "help for %v", args)
	}
}

// writeTidyPreviewModule returns the cached tidy Go module snapshot for
// previewTestDesign.
func writeTidyPreviewModule(t *testing.T) map[string]string {
	t.Helper()
	tidyPreviewModuleOnce.Do(func() {
		tidyPreviewModuleFiles = testmodule.Snapshot(t, "", previewTestDesign)
	})
	return tidyPreviewModuleFiles
}

// writeFilesToDir writes each slash-separated path and content in files to a
// fresh temporary directory and returns its root path.
func writeFilesToDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
	return dir
}
