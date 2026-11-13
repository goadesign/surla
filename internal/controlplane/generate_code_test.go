// This file exercises local generation through the generated HTTP and MCP
// clients with a real minimal module, then tests workspace trust refusals.
package controlplane

import (
	"context"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goahttp "goa.design/goa/v3/http"

	genhttpclient "github.com/goadesign/surla/gen/http/surla/client"
	genmcpclient "github.com/goadesign/surla/gen/jsonrpc/mcp_surla/client"
	genmcpsurla "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/testmodule"
)

// generationDoer authenticates test requests through the real ingress handler.
type generationDoer struct{ client *http.Client }

// generationDesign declares only a small Goa HTTP service; no model is needed.
const generationDesign = `package design
import . "goa.design/goa/v3/dsl"
var _ = Service("orders", func() {
	Method("get_order", func() {
		Payload(String)
		Result(String)
		HTTP(func() { GET("/orders/{id}") })
	})
})
`

// TestGenerateCodeHTTPMCPAndIteration verifies the local edit/generate loop
// against one running server, including default target, local eval isolation,
// exact output lists, preserved user files, and safe source-error recovery.
func TestGenerateCodeHTTPMCPAndIteration(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	workspace := t.TempDir()
	snapshot := testmodule.Snapshot(t, "", generationDesign)
	writeGenerationFiles(t, workspace, snapshot)
	writeGenerationFiles(t, workspace, map[string]string{
		"gen/stale/old.go": "old\n", "gen/NOTES.md": "mine\n",
	})
	require.NoError(t, os.Chmod(filepath.Join(workspace, "gen/NOTES.md"), 0o600))
	writeGenerationFiles(t, filepath.Join(workspace, "eval"), snapshot)
	writeGenerationFiles(t, filepath.Join(workspace, "eval"), map[string]string{"gen/stale/old.go": "eval old\n"})
	evalBefore := generationFiles(t, workspace, "eval")
	privateCards := filepath.Join(t.TempDir(), "operator\"slash\\storage")
	privateEval := filepath.Join(t.TempDir(), "physical\"compiler\\storage")
	require.NoError(t, os.Mkdir(privateEval, 0o700))
	t.Chdir(t.TempDir())
	require.NoError(t, os.Symlink(privateEval, "operator-link"))
	cp := newControlPlane(t, Options{
		WorkspaceDir: workspace, CardsDir: privateCards,
		Evaluator: designeval.NewEvaluatorInDir(filepath.Join("operator-link", "work")),
	})
	srv := httptest.NewServer(NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{
		"generation-token": {ID: "developer@example.com"},
	})))
	t.Cleanup(srv.Close)
	origin, err := url.Parse(srv.URL)
	require.NoError(t, err)
	doer := generationDoer{client: srv.Client()}
	httpTransport := genhttpclient.NewClient(origin.Scheme, origin.Host, doer, goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	httpClient := &gensurla.Client{GenerateCodeEndpoint: httpTransport.GenerateCode()}
	mcpTransport := genmcpclient.NewClient(origin.Scheme, origin.Host, doer, goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	mcpClient := &genmcpsurla.Client{ToolsCallEndpoint: mcpTransport.ToolsCall(), ToolsListEndpoint: mcpTransport.ToolsList()}

	tools, err := mcpClient.ToolsList(ctx, &genmcpsurla.ToolsListPayload{})
	require.NoError(t, err)
	index := slices.IndexFunc(tools.Tools, func(tool *genmcpsurla.ToolInfo) bool { return tool.Name == "generate_code" })
	require.NotEqual(t, -1, index)
	assert.Empty(t, tools.Tools[index].Meta, "generation leaves an open checkpoint view undisturbed")
	args, err := gensurla.EncodeGenerateCodePayload(&gensurla.GenerateCodePayload{})
	require.NoError(t, err)
	call, err := mcpClient.ToolsCall(ctx, &genmcpsurla.ToolsCallPayload{Name: "generate_code", Arguments: args})
	require.NoError(t, err)
	complete, ok := call.Outcome.AsComplete()
	require.True(t, ok)
	require.False(t, complete.IsError != nil && *complete.IsError, "%+v", complete.Content)
	first, err := gensurla.DecodeGenerateCodeResult(complete.StructuredContent)
	require.NoError(t, err)
	assert.Equal(t, "candidate", first.Target)
	assert.Empty(t, first.Warnings)
	physicalEvalParent, err := filepath.EvalSymlinks(filepath.Join(privateEval, "work"))
	require.NoError(t, err)
	generated := generationFiles(t, workspace, "gen")
	assert.NotContains(t, generated, "gen/stale/old.go")
	assert.Equal(t, "mine\n", generated["gen/NOTES.md"])
	delete(generated, "gen/NOTES.md")
	assert.Equal(t, slices.Sorted(maps.Keys(generated)), first.FilesWritten)
	info, err := os.Stat(filepath.Join(workspace, "gen/NOTES.md"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.Equal(t, evalBefore, generationFiles(t, workspace, "eval"))
	for _, name := range []string{"go.mod", "go.sum"} {
		content, err := os.ReadFile(filepath.Join(workspace, name))
		require.NoError(t, err)
		assert.Equal(t, snapshot[name], string(content))
	}

	// Editing the same workspace regenerates immediately through the same
	// server. No source upload, commit, model, or verification config is used.
	updated := strings.Replace(generationDesign, `Service("orders", func() {`, `Service("orders", func() { Description("edited on the running server")`, 1)
	writeGenerationFiles(t, workspace, map[string]string{"design/design.go": updated})
	candidate := "candidate"
	second, err := cp.GenerateCode(
		WithPrincipal(ctx, Principal{ID: "developer@example.com", CallerID: "developer@example.com", Kind: CallerUser}),
		&gensurla.GenerateCodePayload{Target: &candidate},
	)
	require.NoError(t, err)
	require.NoError(t, gensurla.ValidateGenerateCodeResultValue(second))
	assert.Empty(t, second.Warnings)
	encoded, err := gensurla.EncodeGenerateCodeResult(second)
	require.NoError(t, err)
	decoded, err := gensurla.DecodeGenerateCodeResult(encoded)
	require.NoError(t, err)
	assert.Equal(t, second, decoded, "direct results satisfy the same required collection contract as HTTP/MCP")
	assert.Equal(t, first.FilesWritten, second.FilesWritten)
	assert.Contains(t, generationFiles(t, workspace, "gen")["gen/orders/service.go"], "edited on the running server")
	assert.Equal(t, evalBefore, generationFiles(t, workspace, "eval"))
	candidateBeforeEval := generationFiles(t, workspace, "gen")

	eval := "eval"
	args, err = gensurla.EncodeGenerateCodePayload(&gensurla.GenerateCodePayload{Target: &eval})
	require.NoError(t, err)
	call, err = mcpClient.ToolsCall(ctx, &genmcpsurla.ToolsCallPayload{Name: "generate_code", Arguments: args})
	require.NoError(t, err)
	complete, ok = call.Outcome.AsComplete()
	require.True(t, ok)
	require.False(t, complete.IsError != nil && *complete.IsError, "%+v", complete.Content)
	evalResult, err := gensurla.DecodeGenerateCodeResult(complete.StructuredContent)
	require.NoError(t, err)
	assert.Equal(t, "eval", evalResult.Target)
	evalGenerated := generationFiles(t, filepath.Join(workspace, "eval"), "gen")
	assert.Equal(t, slices.Sorted(maps.Keys(evalGenerated)), evalResult.FilesWritten)
	assert.NotContains(t, evalGenerated, "gen/stale/old.go")
	assert.Equal(t, candidateBeforeEval, generationFiles(t, workspace, "gen"))
	for _, name := range []string{"go.mod", "go.sum"} {
		content, err := os.ReadFile(filepath.Join(workspace, "eval", name))
		require.NoError(t, err)
		assert.Equal(t, snapshot[name], string(content))
	}

	// Snapshot validation uses only caller-supplied files. Its exact remedies
	// remain visible, and restoring those files allows same-server generation.
	for _, failure := range []struct {
		name   string
		files  map[string]string
		remove []string
		want   string
	}{
		{name: "missing go.sum", remove: []string{"go.sum"}, want: "design snapshot has no go.sum"},
		{name: "malformed go.mod", files: map[string]string{"go.mod": "module (\n"}, want: "parse snapshot go.mod"},
		{name: "no design source", remove: []string{"design/design.go"}, want: "design snapshot has no design/*.go file"},
		{name: "local replacement", files: map[string]string{"go.mod": snapshot["go.mod"] + "\nreplace example.com/local => ./local-dependency\n"}, want: "only versioned replacements are supported"},
	} {
		t.Run(failure.name, func(t *testing.T) {
			writeGenerationFiles(t, workspace, failure.files)
			for _, name := range failure.remove {
				require.NoError(t, os.Remove(filepath.Join(workspace, name)))
			}
			_, err := mcpClient.ToolsCall(ctx, &genmcpsurla.ToolsCallPayload{Name: "generate_code", Arguments: []byte(`{}`)})
			require.Error(t, err)
			assert.ErrorContains(t, err, failure.want)
			if failure.name == "missing go.sum" {
				assert.ErrorContains(t, err, "go mod tidy")
			}
			assert.NotContains(t, err.Error(), physicalEvalParent)
			assert.Equal(t, candidateBeforeEval, generationFiles(t, workspace, "gen"))
			writeGenerationFiles(t, workspace, map[string]string{
				"go.mod": snapshot["go.mod"], "go.sum": snapshot["go.sum"], "design/design.go": updated,
			})
			_, err = httpClient.GenerateCode(ctx, &gensurla.GenerateCodePayload{})
			require.NoError(t, err)
		})
	}

	// A compiler rejection identifies the builder's broken source without
	// returning the operator-owned compiler workspace or log locations.
	writeGenerationFiles(t, workspace, map[string]string{"design/design.go": updated + "\nvar _ = MissingSourceSymbol\n"})
	_, err = httpClient.GenerateCode(ctx, &gensurla.GenerateCodePayload{})
	require.Error(t, err)
	var httpFailure *goahttp.ClientError
	require.ErrorAs(t, err, &httpFailure)
	assert.True(t, httpFailure.Fault, "the generated client preserves the HTTP 500 failure")
	assert.ErrorContains(t, err, "undefined: MissingSourceSymbol")
	assert.ErrorContains(t, err, "call generate_code again")
	assert.NotContains(t, err.Error(), cp.cards.dir)
	assert.NotContains(t, err.Error(), "surla-designeval-")
	assert.Equal(t, candidateBeforeEval, generationFiles(t, workspace, "gen"))

	// Both designs compile as Go but fail inside the sandboxed generator.
	// Their explanations must survive the MCP protocol-error path, followed
	// by successful generation after correction on this same server.
	for _, failure := range []struct {
		name, design, want string
	}{
		{
			name:   "invalid DSL",
			design: strings.Replace(updated, "Payload(String)", `Payload(func() { Attribute("id", String); Required("missing") })`, 1),
			want:   `required field "missing" does not exist`,
		},
		{
			name:   "unsupported gRPC finalization",
			design: strings.Replace(updated, `Method("get_order", func() {`, `Method("get_order", func() { GRPC(func() {})`, 1),
			want:   ".proto (finalizer)",
		},
	} {
		t.Run(failure.name, func(t *testing.T) {
			writeGenerationFiles(t, workspace, map[string]string{"design/design.go": failure.design})
			_, err := mcpClient.ToolsCall(ctx, &genmcpsurla.ToolsCallPayload{Name: "generate_code", Arguments: []byte(`{}`)})
			require.Error(t, err, "a failed generation uses the MCP protocol-error path")
			assert.ErrorContains(t, err, "evaluating the design and generating files")
			assert.ErrorContains(t, err, failure.want)
			assert.ErrorContains(t, err, "call generate_code again on the same server")
			assert.NotContains(t, err.Error(), filepath.Base(privateCards))
			quotedBase := strconv.Quote(filepath.Base(privateCards))
			assert.NotContains(t, err.Error(), quotedBase[1:len(quotedBase)-1])
			assert.Equal(t, candidateBeforeEval, generationFiles(t, workspace, "gen"))
			writeGenerationFiles(t, workspace, map[string]string{"design/design.go": updated})
			result, err := httpClient.GenerateCode(ctx, &gensurla.GenerateCodePayload{})
			require.NoError(t, err)
			assert.Equal(t, first.FilesWritten, result.FilesWritten)
		})
	}

	// A real Go build cache setup failure also exits 1. It is reported as
	// an operation failure with safe details, without blaming source code.
	// Preserve the valid cache so restoring it proves same-server recovery.
	entries, err := os.ReadDir(physicalEvalParent)
	require.NoError(t, err)
	entries = slices.DeleteFunc(entries, func(entry os.DirEntry) bool {
		return !strings.HasPrefix(entry.Name(), "surla-designeval-work-")
	})
	require.Len(t, entries, 1)
	cache := filepath.Join(physicalEvalParent, entries[0].Name(), "gocache")
	info, err = os.Stat(cache)
	require.NoError(t, err)
	require.True(t, info.IsDir())
	require.NoError(t, os.Rename(cache, cache+"-saved"))
	require.NoError(t, os.WriteFile(cache, []byte("cache directory replaced by a file\n"), 0o600))
	_, cacheErr := httpClient.GenerateCode(ctx, &gensurla.GenerateCodePayload{})
	require.NoError(t, os.Remove(cache))
	require.NoError(t, os.Rename(cache+"-saved", cache))
	require.Error(t, cacheErr)
	require.ErrorAs(t, cacheErr, &httpFailure)
	assert.True(t, httpFailure.Fault)
	assert.ErrorContains(t, cacheErr, "failed while compiling the design")
	assert.ErrorContains(t, cacheErr, "failed to initialize build cache")
	assert.NotContains(t, cacheErr.Error(), "The design did not compile")
	assert.NotContains(t, cacheErr.Error(), privateCards)
	assert.NotContains(t, cacheErr.Error(), physicalEvalParent)
	assert.NotContains(t, cacheErr.Error(), filepath.Base(privateEval))
	quotedCards := strconv.Quote(privateCards)
	assert.NotContains(t, cacheErr.Error(), quotedCards[1:len(quotedCards)-1])
	quotedEval := strconv.Quote(physicalEvalParent)
	assert.NotContains(t, cacheErr.Error(), quotedEval[1:len(quotedEval)-1])
	assert.Equal(t, candidateBeforeEval, generationFiles(t, workspace, "gen"))
	_, err = httpClient.GenerateCode(ctx, &gensurla.GenerateCodePayload{})
	require.NoError(t, err)

	require.NoError(t, cp.Close())
	_, err = cp.GenerateCode(WithPrincipal(ctx, Principal{ID: "developer@example.com", CallerID: "developer@example.com", Kind: CallerUser}), &gensurla.GenerateCodePayload{})
	require.Error(t, err)
	assert.Equal(t, candidateBeforeEval, generationFiles(t, workspace, "gen"), "a closed server never publishes")
}

// TestGenerateCodeRejectsUnavailableModules verifies trust failures before a
// compiler workspace or generated output can be created.
func TestGenerateCodeRejectsUnavailableModules(t *testing.T) {
	for _, name := range []string{"missing module", "replaced workspace", "symlink eval", "missing eval", "eval without go.mod"} {
		t.Run(name, func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), "module")
			require.NoError(t, os.Mkdir(workspace, 0o755))
			cp := newControlPlane(t, Options{WorkspaceDir: workspace})
			payload := &gensurla.GenerateCodePayload{}
			switch name {
			case "replaced workspace":
				require.NoError(t, os.Rename(workspace, workspace+"-old"))
				require.NoError(t, os.Mkdir(workspace, 0o755))
			case "symlink eval":
				require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(workspace, "eval")))
				fallthrough
			case "missing eval", "eval without go.mod":
				eval := "eval"
				payload.Target = &eval
				if name == "eval without go.mod" {
					require.NoError(t, os.Mkdir(filepath.Join(workspace, "eval"), 0o755))
				}
			}
			_, err := cp.GenerateCode(WithPrincipal(t.Context(), Principal{ID: "developer@example.com", CallerID: "developer@example.com", Kind: CallerUser}), payload)
			require.Error(t, err)
			assert.Equal(t, "unprocessable", goaErrorName(err))
			matches, err := filepath.Glob(filepath.Join(workspace, "gen"))
			require.NoError(t, err)
			assert.Empty(t, matches)
		})
	}
	for _, raw := range []string{`{"target":"other"}`, `{"path":"/tmp/module"}`, `{"design":{}}`} {
		_, err := gensurla.DecodeGenerateCodePayload([]byte(raw))
		assert.Error(t, err, raw)
	}
}

// Do supplies the configured bearer token to the generated clients' requests.
func (d generationDoer) Do(request *http.Request) (*http.Response, error) {
	request.Header.Set("Authorization", "Bearer generation-token-builder")
	return d.client.Do(request)
}

// writeGenerationFiles lays out trusted test fixtures only in t.TempDir roots.
func writeGenerationFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
}

// generationFiles reads fixture output as module-relative paths and bytes.
func generationFiles(t *testing.T, dir, sub string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(filepath.Join(dir, sub), func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			content, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, name)
			if err != nil {
				return err
			}
			out[filepath.ToSlash(rel)] = string(content)
		}
		return nil
	}))
	return out
}
