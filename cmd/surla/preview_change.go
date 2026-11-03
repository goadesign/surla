// This file holds the `surla preview-change` CLI command, which reads a
// local design module from disk and sends it to a Surla server's CP2
// preview_architecture endpoint.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	goahttp "goa.design/goa/v3/http"

	gensurlaclient "github.com/goadesign/surla/gen/http/surla/client"
	gensurlaserver "github.com/goadesign/surla/gen/http/surla/server"
	gensurla "github.com/goadesign/surla/gen/surla"
)

type (
	// repeatStringFlag collects repeated string flags (such as -architect-path and
	// -eval-path) verbatim without splitting on commas or trimming significant
	// spaces, rejecting invalid UTF-8 at parse time.
	repeatStringFlag []string

	// previewChangeConfig holds the parsed flags for `surla preview-change`.
	previewChangeConfig struct {
		// serverURL is the HTTP or HTTPS origin of the Surla server.
		serverURL string
		// changeID is the confirmed Change Card identifier.
		changeID string
		// requestRevision is the confirmed CP1 request_revision digest.
		requestRevision string
		// dir is the root directory of the local design module.
		dir string
		// token is the resolved bearer token sent to the Surla server.
		token string
		// buildersPath is the optional path of a JSON array of builder scopes.
		buildersPath string
		// architectPaths lists optional repository-relative path globs for @architect.
		architectPaths repeatStringFlag
		// evalPaths lists optional repository or evaluator-workspace relative path globs for @eval-author.
		evalPaths repeatStringFlag
		// timeout bounds the HTTP request to the Surla server.
		timeout time.Duration
	}

	// bearerDoer is a goahttp.Doer that attaches the Surla bearer token to
	// each outgoing HTTP request.
	bearerDoer struct {
		// client executes the HTTP request with redirects disabled.
		client *http.Client
		// token is the bearer token sent in the Authorization header.
		token string
	}
)

// String returns the formatted slice of collected flag values.
func (f *repeatStringFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ", ")
}

// Set validates that val is valid, non-empty UTF-8 and appends it verbatim to
// *f.
func (f *repeatStringFlag) Set(val string) error {
	if !utf8.ValidString(val) {
		return errors.New("path must be valid UTF-8")
	}
	if strings.TrimSpace(val) == "" {
		return errors.New("path must not be empty")
	}
	*f = append(*f, val)
	return nil
}

// Do sets the Authorization bearer header on req and executes it with
// d.client.
func (d *bearerDoer) Do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+d.token)
	return d.client.Do(req)
}

// runPreviewChange reads the design module in -dir with readModuleSnapshot,
// checks that every path and file is valid UTF-8, sends it to the Surla server
// at -server via the generated PreviewArchitecture HTTP client bound to
// -change-id and -request-revision, and writes the snake_case JSON
// ArchitecturePreviewResult to w. Redirects are refused so credentials are
// never forwarded, and SIGINT, SIGTERM or -timeout cancels the request.
func runPreviewChange(args []string, w io.Writer, getenv func(string) string) error {
	cfg, err := parsePreviewChangeFlags(args, getenv)
	if err != nil {
		return err
	}
	scheme, host, err := parseServerOrigin(cfg.serverURL)
	if err != nil {
		return err
	}
	builders, err := loadBuilderScopes(cfg.buildersPath)
	if err != nil {
		return err
	}
	files, err := readModuleSnapshot(cfg.dir)
	if err != nil {
		return err
	}
	if err := validateUTF8Snapshot(files); err != nil {
		return err
	}
	reqBody := &gensurlaserver.PreviewArchitectureRequestBody{
		ChangeID:        &cfg.changeID,
		RequestRevision: &cfg.requestRevision,
		Design:          &gensurlaserver.DesignSnapshotRequestBody{Files: files},
		Builders:        builders,
		ArchitectPaths:  []string(cfg.architectPaths),
		EvalPaths:       []string(cfg.evalPaths),
	}
	if err := gensurlaserver.ValidatePreviewArchitectureRequestBody(reqBody); err != nil {
		return err
	}
	payload := gensurlaserver.NewPreviewArchitecturePayload(reqBody)
	doer := &bearerDoer{
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("redirects are not followed")
			},
		},
		token: cfg.token,
	}
	endpoint := gensurlaclient.NewClient(
		scheme,
		host,
		doer,
		goahttp.RequestEncoder,
		goahttp.ResponseDecoder,
		false,
	).PreviewArchitecture()
	client := gensurla.NewClient(nil, nil, nil, nil, nil, nil, endpoint, nil, nil, nil, nil, nil, nil, nil, nil)
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(sigCtx, cfg.timeout)
	defer cancel()
	res, err := client.PreviewArchitecture(ctx, payload)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(gensurlaserver.NewPreviewArchitectureResponseBody(res))
}

// parsePreviewChangeFlags parses and validates the CLI flags for
// `surla preview-change`. The bearer token is resolved from -auth-token or
// SURLA_AUTH_TOKEN via getenv so `surla preview-change -h` never prints it.
func parsePreviewChangeFlags(args []string, getenv func(string) string) (previewChangeConfig, error) {
	fs := flag.NewFlagSet("preview-change", flag.ContinueOnError)
	var cfg previewChangeConfig
	var rawToken string
	fs.StringVar(&cfg.serverURL, "server", "", "HTTP or HTTPS origin URL of the Surla server, such as http://127.0.0.1:8090 (required)")
	fs.StringVar(&cfg.changeID, "change-id", "", "confirmed Change Card ID to bind the preview to (required)")
	fs.StringVar(&cfg.requestRevision, "request-revision", "", "confirmed CP1 request_revision of the Change Card (required)")
	fs.StringVar(&cfg.dir, "dir", ".", "root directory of the design module (holding go.mod, go.sum and design/)")
	fs.StringVar(&rawToken, "auth-token", "", "Bearer token required on HTTP requests (default: $SURLA_AUTH_TOKEN)")
	fs.StringVar(&cfg.buildersPath, "builders", "", "optional JSON file listing explicit builder path scopes for a CP2 swarm roster")
	fs.Var(&cfg.architectPaths, "architect-path", "optional repository-relative path glob for @architect in the CP2 swarm roster (repeatable; requires -builders)")
	fs.Var(&cfg.evalPaths, "eval-path", "optional repository or evaluator-workspace relative path glob for @eval-author in the CP2 swarm roster (repeatable; requires -builders)")
	fs.DurationVar(&cfg.timeout, "timeout", 2*time.Minute, "how long the HTTP preview request may take before it is stopped")
	if err := fs.Parse(args); err != nil {
		return previewChangeConfig{}, err
	}
	if fs.NArg() > 0 {
		return previewChangeConfig{}, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	cfg.token = resolveAuthToken(rawToken, getenv)
	var missing []string
	for _, r := range []struct {
		flag  string
		value string
	}{
		{"-server", cfg.serverURL},
		{"-change-id", cfg.changeID},
		{"-request-revision", cfg.requestRevision},
		{"-auth-token (or SURLA_AUTH_TOKEN)", cfg.token},
	} {
		if strings.TrimSpace(r.value) == "" {
			missing = append(missing, r.flag)
		}
	}
	if len(missing) > 0 {
		return previewChangeConfig{}, fmt.Errorf("preview-change requires %s", strings.Join(missing, ", "))
	}
	if strings.TrimSpace(cfg.dir) == "" {
		return previewChangeConfig{}, errors.New("-dir must not be empty")
	}
	if cfg.timeout <= 0 {
		return previewChangeConfig{}, errors.New("-timeout must be greater than 0")
	}
	if !utf8.ValidString(cfg.changeID) {
		return previewChangeConfig{}, errors.New("-change-id must be valid UTF-8")
	}
	if !utf8.ValidString(cfg.requestRevision) {
		return previewChangeConfig{}, errors.New("-request-revision must be valid UTF-8")
	}
	if strings.TrimSpace(cfg.buildersPath) == "" && (len(cfg.architectPaths) > 0 || len(cfg.evalPaths) > 0) {
		return previewChangeConfig{}, errors.New("-architect-path and -eval-path require -builders")
	}
	return cfg, nil
}

// parseServerOrigin validates that raw is an HTTP or HTTPS origin URL with a
// non-empty host, an empty or "/" path, and no user credentials, query,
// fragment or opaque data. Errors never echo raw so embedded credentials
// cannot leak to stderr.
func parseServerOrigin(raw string) (string, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", errors.New("invalid -server URL")
	}
	if u.User != nil {
		return "", "", errors.New("-server must not include user credentials")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", errors.New("-server scheme must be http or https")
	}
	if u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return "", "", errors.New("-server must include a host")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", "", errors.New("-server must be an origin URL without a path, query, or fragment")
	}
	return u.Scheme, u.Host, nil
}

// loadBuilderScopes reads and decodes the optional JSON array of builder
// scopes at path using the generated wire type BuilderScopeItemRequestBody,
// rejecting invalid raw UTF-8 before JSON decoding (because encoding/json
// replaces invalid UTF-8 bytes with U+FFFD), null arrays or elements, unknown
// fields, trailing data, and entries missing required fields. An empty path
// means no builder scopes were given.
func loadBuilderScopes(path string) ([]*gensurlaserver.BuilderScopeItemRequestBody, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read -builders: %w", err)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("-builders %s is not valid UTF-8", path)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var scopes *[]*gensurlaserver.BuilderScopeItemRequestBody
	if err := dec.Decode(&scopes); err != nil {
		return nil, fmt.Errorf("decode -builders %s: %w", path, err)
	}
	if scopes == nil {
		return nil, fmt.Errorf("decode -builders %s: expected a JSON array, got null", path)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode -builders %s: trailing data after the builder array", path)
	}
	for i, s := range *scopes {
		if s == nil {
			return nil, fmt.Errorf("-builders %s[%d]: expected an object, got null", path, i)
		}
		if err := gensurlaserver.ValidateBuilderScopeItemRequestBody(s); err != nil {
			return nil, fmt.Errorf("-builders %s[%d]: %w", path, i, err)
		}
	}
	return *scopes, nil
}

// validateUTF8Snapshot checks that every file path and content in files is
// valid UTF-8 before encoding the snapshot as JSON over HTTP, because
// encoding/json silently replaces invalid UTF-8 bytes with U+FFFD.
func validateUTF8Snapshot(files map[string]string) error {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		if !utf8.ValidString(p) {
			return fmt.Errorf("design snapshot path %q is not valid UTF-8", p)
		}
		if !utf8.ValidString(files[p]) {
			return fmt.Errorf("design snapshot file %s is not valid UTF-8", p)
		}
	}
	return nil
}
