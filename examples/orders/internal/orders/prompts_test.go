package orders

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	genpromptsserver "github.com/goadesign/surla/examples/orders/gen/http/prompts/server"
	genprompts "github.com/goadesign/surla/examples/orders/gen/prompts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goahttp "goa.design/goa/v3/http"
)

// fakePrompts accepts templates other than "bad" for the prompt "known" and
// records them.
type fakePrompts struct {
	// set records the accepted templates.
	set []string
}

// liveKey is the key the tests start the prompts service with.
const liveKey = "live-key-for-tests"

// TestOverridePromptFollowsTheLiveTargetContract verifies the HTTP contract
// Surla's draft_override relies on: PUT /prompts/{prompt_id} with the
// template in the body and the key in X-Live-Target-Key. A missing or wrong
// key, or any key when the process has none, answers 401 with a plain-text
// unauthorized error; an unregistered prompt answers 404 unknown_prompt and an
// unrenderable template 422 invalid_prompt. Rejected calls change nothing; an
// accepted one returns the prompt ID and the accepted text's version.
func TestOverridePromptFollowsTheLiveTargetContract(t *testing.T) {
	cases := []struct {
		name       string
		processKey string
		header     string
		promptID   string
		template   string
		wantStatus int
		wantError  string
		wantSet    []string
	}{
		{"live key", liveKey, liveKey, "known", "New text.", http.StatusOK, "", []string{"New text."}},
		{"missing key", liveKey, "", "known", "New text.", http.StatusUnauthorized, "unauthorized", nil},
		{"wrong key", liveKey, "other-key", "known", "New text.", http.StatusUnauthorized, "unauthorized", nil},
		{"process started without a live key", "", "", "known", "New text.", http.StatusUnauthorized, "unauthorized", nil},
		{"unregistered prompt", liveKey, liveKey, "other", "New text.", http.StatusNotFound, "unknown_prompt", nil},
		{"template does not render", liveKey, liveKey, "known", "bad", http.StatusUnprocessableEntity, "invalid_prompt", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prompts := &fakePrompts{}
			srv := httptest.NewServer(promptHandler(NewPromptService(prompts, c.processKey)))
			defer srv.Close()
			body := strings.NewReader(fmt.Sprintf(`{"template":%q}`, c.template))
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/prompts/"+c.promptID, body)
			require.NoError(t, err)
			if c.header != "" {
				req.Header.Set("X-Live-Target-Key", c.header)
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			assert.Equal(t, c.wantStatus, resp.StatusCode)
			assert.Equal(t, c.wantError, resp.Header.Get("goa-error"), "the error name tells clients which failure it was")
			assert.Equal(t, c.wantSet, prompts.set)
			switch c.wantStatus {
			case http.StatusOK:
				var got genpromptsserver.OverridePromptResponseBody
				require.NoError(t, json.Unmarshal(raw, &got))
				assert.Equal(t, "known", got.PromptID)
				assert.Equal(t, "version-of-New text.", got.Version)
			case http.StatusUnauthorized:
				var text string
				assert.NoError(t, json.Unmarshal(raw, &text), "unauthorized is a JSON string, the shape Surla's client decodes: %s", raw)
			}
		})
	}
}

// Override accepts templates for the prompt "known" other than "bad".
func (f *fakePrompts) Override(_ context.Context, promptID, template string) (string, error) {
	if promptID != "known" {
		return "", ErrPromptNotFound
	}
	if template == "bad" {
		return "", ErrInvalidPrompt
	}
	f.set = append(f.set, template)
	return "version-of-" + template, nil
}

// promptHandler serves svc's HTTP API the way cmd/orders mounts it.
func promptHandler(svc *PromptService) http.Handler {
	mux := goahttp.NewMuxer()
	genpromptsserver.Mount(mux, genpromptsserver.New(genprompts.NewEndpoints(svc), mux, goahttp.RequestDecoder, goahttp.ResponseEncoder, nil, nil))
	return mux
}
