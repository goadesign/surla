package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	genhttpclient "github.com/goadesign/surla/gen/http/surla/client"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/verify/candidate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goahttp "goa.design/goa/v3/http"
	goa "goa.design/goa/v3/pkg"
)

// newPublishOp returns an operation that holds the live grant open and
// whose copy is src, with before holding the manifest bytes read during the
// copy.
func newPublishOp(t *testing.T, grant string, before map[string][]byte) *checkOperation {
	t.Helper()
	dir := realTempDir(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "src"), 0o700))
	fd, err := syscall.Open(grant, openDirFlags, 0)
	require.NoError(t, err)
	wsFD, err := syscall.Open(grant, openDirFlags, 0)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, errors.Join(syscall.Close(fd), syscall.Close(wsFD))) })
	return &checkOperation{
		dir:          dir,
		grantRoot:    grant,
		grantFD:      fd,
		workspaceFD:  wsFD,
		workspaceRel: ".",
		copy:         grantCopy{modules: []string{"."}, manifests: before},
	}
}

func TestPublishManifest(t *testing.T) {
	cases := []struct {
		name        string
		live        map[string]string
		before      map[string][]byte
		staged      map[string]string
		mutate      func(t *testing.T, grant, src string)
		wantChanged bool
		wantErr     string
		wantLive    string
	}{
		{
			name:        "changed go.sum is written",
			live:        map[string]string{"go.sum": "old\n"},
			before:      map[string][]byte{"go.sum": []byte("old\n")},
			staged:      map[string]string{"go.sum": "new\n"},
			wantChanged: true,
			wantLive:    "new\n",
		},
		{
			name:     "unchanged go.sum writes nothing",
			live:     map[string]string{"go.sum": "same\n"},
			before:   map[string][]byte{"go.sum": []byte("same\n")},
			staged:   map[string]string{"go.sum": "same\n"},
			wantLive: "same\n",
		},
		{
			name:        "absent go.sum is created exclusively",
			before:      map[string][]byte{},
			staged:      map[string]string{"go.sum": "created\n"},
			wantChanged: true,
			wantLive:    "created\n",
		},
		{
			name:     "go.sum created concurrently is not overwritten",
			live:     map[string]string{"go.sum": "someone else\n"},
			before:   map[string][]byte{},
			staged:   map[string]string{"go.sum": "ours\n"},
			wantErr:  "was created during update_dependencies",
			wantLive: "someone else\n",
		},
		{
			name:     "go.sum edited during the call is refused",
			live:     map[string]string{"go.sum": "edited\n"},
			before:   map[string][]byte{"go.sum": []byte("old\n")},
			staged:   map[string]string{"go.sum": "new\n"},
			wantErr:  "changed during update_dependencies",
			wantLive: "edited\n",
		},
		{
			name:   "staged go.sum symlink is refused without a host read",
			before: map[string][]byte{"go.sum": []byte("old\n")},
			mutate: func(t *testing.T, _, src string) {
				require.NoError(t, os.Symlink("/etc/hosts", filepath.Join(src, "go.sum")))
			},
			live:     map[string]string{"go.sum": "old\n"},
			wantErr:  "is a symbolic link",
			wantLive: "old\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grant := realTempDir(t)
			writeTree(t, grant, tc.live)
			op := newPublishOp(t, grant, tc.before)
			src := op.path("src")
			writeTree(t, src, tc.staged)
			if tc.mutate != nil {
				tc.mutate(t, grant, src)
			}
			changed, err := op.publishManifest("go.sum")
			if tc.wantErr != "" {
				require.ErrorIs(t, err, errManifestConflict)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantChanged, changed)
			got, err := os.ReadFile(filepath.Join(grant, "go.sum"))
			require.NoError(t, err)
			assert.Equal(t, tc.wantLive, string(got))
		})
	}
}

func TestPublishManifestRefusesLiveSymlinkAndKeepsItsTarget(t *testing.T) {
	// The live go.sum is a symlink to an operator file outside the
	// workspace, which a go mod tidy run on the host would overwrite.
	grant := realTempDir(t)
	outside := filepath.Join(realTempDir(t), "operator_file.txt")
	require.NoError(t, os.WriteFile(outside, []byte(`{"operator_secret":"do-not-clobber"}`), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(grant, "go.sum")))
	op := newPublishOp(t, grant, map[string][]byte{})
	writeTree(t, op.path("src"), map[string]string{"go.sum": "tidy output\n"})

	changed, err := op.publishManifest("go.sum")
	require.ErrorIs(t, err, errManifestConflict)
	assert.Contains(t, err.Error(), "is a symbolic link")
	assert.False(t, changed)
	got, err := os.ReadFile(outside)
	require.NoError(t, err)
	assert.Equal(t, `{"operator_secret":"do-not-clobber"}`, string(got))
	link, err := os.Readlink(filepath.Join(grant, "go.sum"))
	require.NoError(t, err)
	assert.Equal(t, outside, link)
}

func TestPublishManifestWritesThroughTheHeldGrant(t *testing.T) {
	// The workspace is renamed away and a new directory takes its path while
	// go mod tidy runs: the write lands in the verified tree, not the
	// replacement.
	parent := realTempDir(t)
	grant := filepath.Join(parent, "shop")
	writeTree(t, grant, map[string]string{"go.sum": "old\n"})
	op := newPublishOp(t, grant, map[string][]byte{"go.sum": []byte("old\n")})
	writeTree(t, op.path("src"), map[string]string{"go.sum": "new\n"})
	moved := filepath.Join(parent, "shop-moved")
	require.NoError(t, os.Rename(grant, moved))
	writeTree(t, grant, map[string]string{"go.sum": "old\n"})

	changed, err := op.publishManifest("go.sum")
	require.NoError(t, err)
	assert.True(t, changed)
	got, err := os.ReadFile(filepath.Join(moved, "go.sum"))
	require.NoError(t, err)
	assert.Equal(t, "new\n", string(got))
	got, err = os.ReadFile(filepath.Join(grant, "go.sum"))
	require.NoError(t, err)
	assert.Equal(t, "old\n", string(got), "the replacement directory is untouched")
}

func TestPublishManifestWritesThroughTheVerifiedNestedWorkspace(t *testing.T) {
	// grant=/repo, workspace=/repo/app: app is replaced while go mod tidy
	// runs. The write goes to the verified app, never the replacement.
	repo := realTempDir(t)
	writeTree(t, repo, map[string]string{"app/go.mod": "module example.com/app\n", "app/go.sum": "old\n"})
	ws, err := os.OpenRoot(filepath.Join(repo, "app"))
	require.NoError(t, err)
	op := &checkOperation{dir: realTempDir(t), grantFD: -1, workspaceFD: -1}
	require.NoError(t, op.openLive(ws, []string{filepath.Join(repo, "app"), repo}))
	require.NoError(t, ws.Close())
	t.Cleanup(func() { require.NoError(t, op.closeLive()) })
	assert.Equal(t, repo, op.grantRoot)
	assert.Equal(t, "app", op.workspaceRel)
	op.copy = grantCopy{modules: []string{"."}, manifests: map[string][]byte{"app/go.sum": []byte("old\n")}}
	writeTree(t, op.path("src"), map[string]string{"app/go.sum": "new\n"})
	require.NoError(t, os.Rename(filepath.Join(repo, "app"), filepath.Join(repo, "app-old")))
	writeTree(t, repo, map[string]string{"app/go.sum": "old\n"})

	changed, err := op.publishManifest("app/go.sum")
	require.NoError(t, err)
	assert.True(t, changed)
	got, err := os.ReadFile(filepath.Join(repo, "app-old", "go.sum"))
	require.NoError(t, err)
	assert.Equal(t, "new\n", string(got))
	got, err = os.ReadFile(filepath.Join(repo, "app", "go.sum"))
	require.NoError(t, err)
	assert.Equal(t, "old\n", string(got), "the replacement is untouched")
}

func TestOpenLiveUsesTheSourceRootsAcceptedAtStartup(t *testing.T) {
	parent := realTempDir(t)
	repo := filepath.Join(parent, "repo")
	writeTree(t, parent, map[string]string{
		"repo/app/go.mod":          "module example.com/app\n",
		"repo/app/child/go.mod":    "module example.com/child\n",
		"operator-private/key.txt": "private\n",
	})
	runGitForTest(t, repo, "init", "-q")
	accepted := []string{filepath.Join(repo, "app"), repo}
	// Git metadata changed after startup now names the parent as the
	// working tree; the accepted roots still bound the operation.
	runGitForTest(t, repo, "config", "core.worktree", parent)

	cases := []struct {
		name    string
		ws      string
		wantRel string
		wantErr string
	}{
		{name: "initial workspace", ws: filepath.Join(repo, "app"), wantRel: "app"},
		{name: "init_project child of the initial workspace", ws: filepath.Join(repo, "app", "child"), wantRel: "app/child"},
		{name: "outside the accepted roots", ws: filepath.Join(parent, "operator-private"), wantErr: "outside " + repo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, err := os.OpenRoot(tc.ws)
			require.NoError(t, err)
			defer func() { require.NoError(t, root.Close()) }()
			op := &checkOperation{grantFD: -1, workspaceFD: -1}
			err = op.openLive(root, accepted)
			defer func() { require.NoError(t, op.closeLive()) }()
			if tc.wantErr != "" {
				assertServiceError(t, err, "unprocessable")
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, repo, op.grantRoot)
			assert.Equal(t, tc.wantRel, op.workspaceRel)
			dst := realTempDir(t)
			_, err = copyGrant(context.Background(), op.grantFD, op.workspaceRel, op.workspaceID, dst, nil)
			require.NoError(t, err)
			assert.NotContains(t, copiedFiles(t, dst), "operator-private/key.txt")
			assert.Contains(t, copiedFiles(t, dst), "app/go.mod")
		})
	}
}

func TestOpenLiveRefusesAWorkspaceReparentedBeforeBinding(t *testing.T) {
	// The server verified /repo/app; the same app directory is then moved
	// under another parent before the operation binds its grant. Walking
	// ".." from it reaches the other parent, which is not the accepted root.
	parent := realTempDir(t)
	repo := filepath.Join(parent, "repo")
	writeTree(t, parent, map[string]string{
		"repo/app/go.mod":                 "module example.com/app\n",
		"private-parent/operator-key.txt": "private\n",
	})
	wsRoot, err := os.OpenRoot(filepath.Join(repo, "app"))
	require.NoError(t, err)
	defer func() { require.NoError(t, wsRoot.Close()) }()
	require.NoError(t, os.Rename(filepath.Join(repo, "app"), filepath.Join(parent, "private-parent", "app")))

	op := &checkOperation{grantFD: -1, workspaceFD: -1}
	err = op.openLive(wsRoot, []string{filepath.Join(repo, "app"), repo})
	assertServiceError(t, err, "unprocessable")
	assert.Contains(t, err.Error(), "is outside "+repo)
	require.NoError(t, op.closeLive())
}

func TestCheckBindingAcceptsOtherSpellingsThroughTheStartupPipeline(t *testing.T) {
	// The operator starts the server with a spelling the volume resolves to
	// the stored directories, while Git reports the stored spelling of the
	// working tree. New records both; the operation must still bind.
	cases := []struct {
		name    string
		stored  string
		started string
	}{
		{name: "upper-case repository", stored: "repo", started: "REPO/app"},
		{name: "upper-case workspace", stored: "repo", started: "repo/APP"},
		{name: "decomposed Unicode repository", stored: "caf\u00e9", started: "cafe\u0301/app"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := realTempDir(t)
			if !caseInsensitive(t, parent) {
				t.Skip("needs a case- and normalization-insensitive volume")
			}
			writeTree(t, parent, map[string]string{tc.stored + "/app/go.mod": "module example.com/app\n"})
			runGitForTest(t, filepath.Join(parent, tc.stored), "init", "-q")
			cp, err := New(Options{CardsDir: filepath.Join(realTempDir(t), "cards"), WorkspaceDir: filepath.Join(parent, tc.started)})
			require.NoError(t, err)
			defer func() { require.NoError(t, cp.Close()) }()

			wsDir, dev, ino := cp.currentWorkspaceTarget()
			_, wsRoot, err := openModuleRootWithIdentity(wsDir, dev, ino)
			require.NoError(t, err)
			op := &checkOperation{grantFD: -1, workspaceFD: -1}
			require.NoError(t, errors.Join(op.openLive(wsRoot, cp.protectedDirs), wsRoot.Close()))
			defer func() { require.NoError(t, op.closeLive()) }()
			assert.Equal(t, "app", op.workspaceRel)
			dst := realTempDir(t)
			got, err := copyGrant(context.Background(), op.grantFD, op.workspaceRel, op.workspaceID, dst, nil)
			require.NoError(t, err)
			assert.Equal(t, []string{"."}, got.modules)
			assert.Contains(t, copiedFiles(t, dst), "app/go.mod")
		})
	}
}

func TestPublishManifestRefusesWhileAnotherWriterHoldsTheDirectory(t *testing.T) {
	// Two update_dependencies calls read the same old go.sum. While one
	// holds the directory, which is what a second server does with its own
	// open directory, the other refuses; afterwards it sees the first one's
	// write and refuses instead of overwriting it.
	grant := realTempDir(t)
	writeTree(t, grant, map[string]string{"go.sum": "old\n"})
	first := newPublishOp(t, grant, map[string][]byte{"go.sum": []byte("old\n")})
	writeTree(t, first.path("src"), map[string]string{"go.sum": "first\n"})
	second := newPublishOp(t, grant, map[string][]byte{"go.sum": []byte("old\n")})
	writeTree(t, second.path("src"), map[string]string{"go.sum": "second\n"})

	holder, err := syscall.Open(grant, openDirFlags, 0)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(holder, syscall.LOCK_EX|syscall.LOCK_NB))
	changed, err := second.publishManifest("go.sum")
	require.ErrorIs(t, err, errManifestConflict)
	assert.Contains(t, err.Error(), "another update_dependencies is writing the directory of go.sum")
	assert.False(t, changed)
	require.NoError(t, syscall.Close(holder))

	changed, err = first.publishManifest("go.sum")
	require.NoError(t, err)
	assert.True(t, changed)
	changed, err = second.publishManifest("go.sum")
	require.ErrorIs(t, err, errManifestConflict)
	assert.Contains(t, err.Error(), "changed during update_dependencies")
	assert.False(t, changed)
	got, err := os.ReadFile(filepath.Join(grant, "go.sum"))
	require.NoError(t, err)
	assert.Equal(t, "first\n", string(got))
}

func TestOutputKeepsMultiByteCharactersWhole(t *testing.T) {
	// "é" is two bytes; place one across the 16 KiB page boundary.
	workDir := realTempDir(t)
	id := strings.Repeat("ab", 16)
	text := strings.Repeat("x", maxCheckMessageBytes-1) + "é" + strings.Repeat("y", 10)
	writeTree(t, filepath.Join(workDir, id), map[string]string{
		outputFile:     text,
		outputMetaFile: fmt.Sprintf(`{"captured":%d,"dropped":0}`, len(text)),
	})
	c := &ControlPlane{checkWorkDir: workDir}
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	var pages []string
	p := &gensurla.ReadCheckOutputPayload{OpID: id}
	for {
		page, err := c.ReadCheckOutput(ctx, p)
		require.NoError(t, err)
		assert.True(t, utf8.ValidString(page.Text), "page %d is valid UTF-8", len(pages))
		pages = append(pages, page.Text)
		if page.NextCursor == nil {
			break
		}
		p.Cursor = page.NextCursor
	}
	require.Len(t, pages, 2)
	assert.Equal(t, text, strings.Join(pages, ""), "paging loses nothing")

	// The tail of a failed step cut inside "é" starts after it.
	dir := realTempDir(t)
	w, err := newCapWriter(filepath.Join(dir, outputFile))
	require.NoError(t, err)
	_, err = w.Write([]byte("aé--- FAIL"))
	require.NoError(t, err)
	require.NoError(t, w.close())
	tail := w.tail(0, int64(len("aé--- FAIL")), len("--- FAIL")+1)
	assert.Equal(t, "--- FAIL", tail)
	assert.Equal(t, "é--- FAIL", w.tail(0, int64(len("aé--- FAIL")), len("é--- FAIL")))
}

func TestCheckAdmissionIsOrderedWithClose(t *testing.T) {
	cardsDir := filepath.Join(realTempDir(t), "cards")
	cp, err := New(Options{CardsDir: cardsDir, Checker: candidate.NewContainerRunner(), CheckTimeout: time.Minute})
	require.NoError(t, err)

	// An admission is in progress (it holds the read lock between its
	// shutdown check and its registration) when Close starts.
	cp.workspaceMu.RLock()
	closed := make(chan error, 1)
	go func() { closed <- cp.Close() }()
	<-cp.serverCtx.Done()
	cp.checkOps.Add(1)
	cp.workspaceMu.RUnlock()
	// The admitted operation now owns scratch without owner.json. Close
	// must wait for it before recovery may remove anything.
	id := strings.Repeat("cd", 16)
	writeTree(t, filepath.Join(cp.checkWorkDir, id), map[string]string{"src/go.mod": "module example.com/shop\n"})
	select {
	case err := <-closed:
		t.Fatalf("Close returned while an admitted operation was running: %v", err)
	default:
	}
	cp.checkOps.Done()
	require.NoError(t, <-closed)
	assert.NoDirExists(t, filepath.Join(cp.checkWorkDir, id, "src"), "recovery ran after the operation ended")

	// Calls after Close are refused before any work.
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	_, err = cp.CheckProject(ctx)
	assertServiceError(t, err, "unavailable")
	_, err = cp.UpdateDependencies(ctx)
	assertServiceError(t, err, "unavailable")
	entries, err := os.ReadDir(cp.checkWorkDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no new operation directory")
}

func TestReadCheckOutputBudgetsTheTextTheCallerReceives(t *testing.T) {
	// Arbitrary test output: invalid bytes (each becomes a 3-byte U+FFFD),
	// multi-byte characters and ASCII, read through the generated HTTP
	// client so the budget is checked after transport.
	raw := append(bytes.Repeat([]byte{0xff}, maxCheckMessageBytes), []byte(strings.Repeat("é", 5000)+"ok")...)
	cp := newControlPlane(t, Options{Checker: candidate.NewContainerRunner(), CheckTimeout: time.Minute})
	id := strings.Repeat("ab", 16)
	writeTree(t, filepath.Join(cp.checkWorkDir, id), map[string]string{
		outputFile:     string(raw),
		outputMetaFile: fmt.Sprintf(`{"captured":%d,"dropped":0}`, len(raw)),
	})
	srv := httptest.NewServer(NewHTTPHandler(cp, NewTokenAuthenticator(map[string]Principal{
		"generation-token": {ID: "developer@example.com"},
	})))
	t.Cleanup(srv.Close)
	origin, err := url.Parse(srv.URL)
	require.NoError(t, err)
	transport := genhttpclient.NewClient(origin.Scheme, origin.Host, generationDoer{client: srv.Client()}, goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	client := &gensurla.Client{ReadCheckOutputEndpoint: transport.ReadCheckOutput()}

	var got strings.Builder
	p := &gensurla.ReadCheckOutputPayload{OpID: id}
	pages := 0
	for {
		page, err := client.ReadCheckOutput(context.Background(), p)
		require.NoError(t, err)
		pages++
		assert.LessOrEqual(t, len(page.Text), maxCheckMessageBytes, "page %d", pages)
		assert.True(t, utf8.ValidString(page.Text))
		assert.Equal(t, int64(len(raw)), page.CapturedBytes)
		got.WriteString(page.Text)
		if page.NextCursor == nil {
			break
		}
		if p.Cursor != nil {
			assert.NotEqual(t, *p.Cursor, *page.NextCursor, "the cursor advances")
		}
		p.Cursor = page.NextCursor
	}
	assert.Equal(t, validText(raw), got.String(), "the pages hold every captured byte, invalid ones as U+FFFD")
	// A page holds 5461 replacement characters (16383 bytes), so the 16384
	// invalid bytes fill three pages with one left; it and the 10002 bytes
	// of "é…ok" fit the fourth.
	assert.Equal(t, 4, pages)
}

func TestReadCheckOutputRefusesDamagedOutput(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	cases := []struct {
		name    string
		output  string
		meta    string
		wantErr string
	}{
		{name: "output shorter than its accounting", output: "short", meta: `{"captured":100,"dropped":0}`, wantErr: "ends before byte 100"},
		{name: "null accounting", output: "x", meta: `null`, wantErr: "not a JSON object"},
		{name: "unknown field", output: "x", meta: `{"captured":1,"dropped":0,"extra":1}`, wantErr: "unknown field"},
		{name: "trailing data", output: "x", meta: `{"captured":1,"dropped":0} {}`, wantErr: "trailing data"},
		{name: "missing count", output: "x", meta: `{"captured":1}`, wantErr: "captured and dropped are required"},
		{name: "negative count", output: "x", meta: `{"captured":1,"dropped":-1}`, wantErr: "out of range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workDir := realTempDir(t)
			id := strings.Repeat("ab", 16)
			writeTree(t, filepath.Join(workDir, id), map[string]string{outputFile: tc.output, outputMetaFile: tc.meta})
			c := &ControlPlane{checkWorkDir: workDir}
			page, err := c.ReadCheckOutput(ctx, &gensurla.ReadCheckOutputPayload{OpID: id})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Nil(t, page)
		})
	}
}

func TestRecoverCheckOperationsKeepsScratchItCannotAttribute(t *testing.T) {
	id := strings.Repeat("ef", 16)
	cases := []struct {
		name      string
		owner     string
		wantKept  bool
		wantError string
	}{
		{name: "null", owner: `null`, wantKept: true, wantError: "not a JSON object"},
		{name: "unknown field", owner: `{"docker":"/usr/local/bin/docker","host":"unix:///var/run/docker.sock","containers":[],"extra":1}`, wantKept: true, wantError: "unknown field"},
		{name: "trailing data", owner: `{"docker":"/usr/local/bin/docker","host":"unix:///var/run/docker.sock","containers":[]} {}`, wantKept: true, wantError: "trailing data"},
		{name: "missing containers", owner: `{"docker":"/usr/local/bin/docker","host":"unix:///var/run/docker.sock"}`, wantKept: true, wantError: "containers is required"},
		{name: "relative docker", owner: `{"docker":"docker","host":"unix:///var/run/docker.sock","containers":[]}`, wantKept: true, wantError: "not an absolute path"},
		{name: "remote engine", owner: `{"docker":"/usr/local/bin/docker","host":"tcp://build.example:2376","containers":[]}`, wantKept: true, wantError: "not a local engine endpoint"},
		{name: "foreign container", owner: `{"docker":"/usr/local/bin/docker","host":"unix:///var/run/docker.sock","containers":["production-db"]}`, wantKept: true, wantError: "does not belong to operation"},
		{name: "completed operation with no containers left", owner: `{"docker":"/usr/local/bin/docker","host":"unix:///var/run/docker.sock","containers":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workDir := realTempDir(t)
			writeTree(t, filepath.Join(workDir, id), map[string]string{
				"src/go.mod": "module example.com/shop\n",
				ownerFile:    tc.owner,
				outputFile:   "out\n",
			})
			// A zero runner has no command: any engine action would panic,
			// so passing proves recovery took none.
			c := &ControlPlane{checker: &candidate.ContainerRunner{}, checkWorkDir: workDir}
			err := c.recoverCheckOperations()
			if tc.wantError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantError)
				assert.Contains(t, err.Error(), "kept the operation directory")
			} else {
				require.NoError(t, err)
			}
			if tc.wantKept {
				assert.FileExists(t, filepath.Join(workDir, id, "src", "go.mod"))
			} else {
				assert.NoDirExists(t, filepath.Join(workDir, id, "src"))
			}
			assert.FileExists(t, filepath.Join(workDir, id, outputFile))
		})
	}
}

func TestPublishManifestRefusesALinkedGoModInTheCopy(t *testing.T) {
	// A module whose go.mod is a symlink is checked, but update_dependencies
	// never writes through or replaces the link.
	grant := realTempDir(t)
	writeTree(t, grant, map[string]string{"manifests/eval.mod": "module example.com/shop/eval\n"})
	require.NoError(t, os.MkdirAll(filepath.Join(grant, "eval"), 0o755))
	require.NoError(t, os.Symlink("../manifests/eval.mod", filepath.Join(grant, "eval", "go.mod")))
	op := newPublishOp(t, grant, map[string][]byte{})
	writeTree(t, op.path("src"), map[string]string{"manifests/eval.mod": "module example.com/shop/eval\n\nrequire example.com/x v1.0.0\n"})
	require.NoError(t, os.MkdirAll(op.path("src/eval"), 0o755))
	require.NoError(t, os.Symlink("../manifests/eval.mod", op.path("src/eval/go.mod")))

	changed, err := op.publishManifest("eval/go.mod")
	require.ErrorIs(t, err, errManifestConflict)
	assert.Contains(t, err.Error(), "is a symbolic link")
	assert.False(t, changed)
	link, err := os.Readlink(filepath.Join(grant, "eval", "go.mod"))
	require.NoError(t, err)
	assert.Equal(t, "../manifests/eval.mod", link)
	target, err := os.ReadFile(filepath.Join(grant, "manifests", "eval.mod"))
	require.NoError(t, err)
	assert.Equal(t, "module example.com/shop/eval\n", string(target), "the link target is untouched")
}

// runGitForTest runs a trusted git command in dir for a test fixture.
func runGitForTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestRecoverCheckOperationsRemovesScratchOfOperationsWithoutContainers(t *testing.T) {
	// The server stopped after copying but before saving owner.json, so no
	// container was ever created: the scratch goes, the output stays.
	workDir := realTempDir(t)
	id := strings.Repeat("ef", 16)
	writeTree(t, filepath.Join(workDir, id), map[string]string{
		"src/go.mod":   "module example.com/shop\n",
		outputFile:     "copied\n",
		outputMetaFile: `{"captured":7,"dropped":0}`,
	})
	writeTree(t, workDir, map[string]string{"not-an-operation/keep.txt": "x"})
	c := &ControlPlane{checker: &candidate.ContainerRunner{}, checkWorkDir: workDir}
	require.NoError(t, c.recoverCheckOperations())
	assert.ElementsMatch(t, []string{id + "/" + outputFile, id + "/" + outputMetaFile, "not-an-operation/keep.txt"}, copiedFiles(t, workDir))
}

func TestNewRejectsANonPositiveCheckTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		_, err := New(Options{CardsDir: filepath.Join(realTempDir(t), "cards"), Checker: candidate.NewContainerRunner(), CheckTimeout: timeout})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "check timeout must be positive")
	}
}

func TestPublishManifestsReportsAWrittenFileWhoseSyncFailed(t *testing.T) {
	grant := realTempDir(t)
	writeTree(t, grant, map[string]string{"go.mod": "module example.com/shop\n", "go.sum": "old\n"})
	op := newPublishOp(t, grant, map[string][]byte{"go.mod": []byte("module example.com/shop\n"), "go.sum": []byte("old\n")})
	writeTree(t, op.path("src"), map[string]string{"go.mod": "module example.com/shop\n", "go.sum": "new\n"})
	prior := fsyncDir
	fsyncDir = func(int) error { return syscall.EIO }
	t.Cleanup(func() { fsyncDir = prior })

	written, notes := op.publishManifests(context.Background(), nil)
	assert.Equal(t, []string{"go.sum"}, written, "a renamed file is reported written")
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "go.sum was written, but making the write durable failed")
	got, err := os.ReadFile(filepath.Join(grant, "go.sum"))
	require.NoError(t, err)
	assert.Equal(t, "new\n", string(got))
}

func TestCapWriterReportsWriteFailure(t *testing.T) {
	dir := realTempDir(t)
	w, err := newCapWriter(filepath.Join(dir, outputFile))
	require.NoError(t, err)
	_, err = w.Write([]byte("0123"))
	require.NoError(t, err)
	// The disk fails: the stream keeps draining, every later byte counts
	// as dropped, and close reports the failure.
	require.NoError(t, w.f.Close())
	n, err := w.Write([]byte("456"))
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	n, err = w.Write([]byte("78"))
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	err = w.close()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "capture check output after byte 4 (5 bytes not captured)")
	raw, err := os.ReadFile(filepath.Join(dir, outputMetaFile))
	require.NoError(t, err)
	var meta outputMeta
	require.NoError(t, json.Unmarshal(raw, &meta))
	assert.Equal(t, outputMeta{Captured: 4, Dropped: 5}, meta)
}

func TestResultMessagesAreBoundedAsAWhole(t *testing.T) {
	newOp := func(t *testing.T, output string, dropped int64) *checkOperation {
		t.Helper()
		dir := realTempDir(t)
		w, err := newCapWriter(filepath.Join(dir, outputFile))
		require.NoError(t, err)
		_, err = w.Write([]byte(output))
		require.NoError(t, err)
		w.dropped = dropped
		require.NoError(t, w.close())
		return &checkOperation{dir: dir, out: w}
	}
	manyModules := make([]string, 3000)
	for i := range manyModules {
		manyModules[i] = fmt.Sprintf("services/module-%04d", i)
	}
	output := strings.Repeat("x", 40<<10) + "--- FAIL: TestLast"

	t.Run("check passes with many modules", func(t *testing.T) {
		op := newOp(t, "", 0)
		op.copy.modules = manyModules
		msg := op.message(nil)
		assert.LessOrEqual(t, len(msg), maxCheckMessageBytes)
		assert.True(t, strings.HasPrefix(msg, "tested 3000 module(s): services/module-0000"))
		assert.Contains(t, msg, "read_check_output(op_id) has the full list")
		assert.True(t, strings.HasSuffix(msg, ". All passed."))
	})
	t.Run("check fails with many modules and long output", func(t *testing.T) {
		op := newOp(t, output, 7)
		op.copy.modules = manyModules
		f := moduleOutcome{dir: "services/module-2999", step: "test", exitCode: 1, start: 0, end: int64(len(output))}
		msg := op.message([]moduleOutcome{f, f})
		assert.LessOrEqual(t, len(msg), maxCheckMessageBytes)
		assert.Contains(t, msg, "services/module-2999 failed: go test exited 1. 1 other module step(s) also failed.")
		assert.Contains(t, msg, "--- FAIL: TestLast")
		assert.True(t, strings.HasSuffix(msg, "Output after byte "+fmt.Sprint(len(output))+" was not captured: 7 bytes dropped."))
	})
	t.Run("check fails with invalid bytes in its output", func(t *testing.T) {
		raw := strings.Repeat("\xff", 40<<10)
		op := newOp(t, raw, 0)
		op.copy.modules = []string{"."}
		f := moduleOutcome{dir: ".", step: "test", exitCode: 2, start: 0, end: int64(len(raw))}
		msg := op.message([]moduleOutcome{f})
		assert.True(t, utf8.ValidString(msg))
		assert.LessOrEqual(t, len(msg), maxCheckMessageBytes, "the replacement characters fit the budget")
		assert.Contains(t, msg, "\ufffd")
	})
	t.Run("update with many refused files", func(t *testing.T) {
		op := newOp(t, "", 0)
		written := []string{"go.mod", "go.sum"}
		var notes []string
		for _, m := range manyModules {
			notes = append(notes, m+"/go.sum was not written: manifest conflict: go.sum changed during update_dependencies; call again")
		}
		msg := op.updateMessage(written, notes, " The files were written, but finishing the operation failed: boom.", nil)
		assert.LessOrEqual(t, len(msg), maxCheckMessageBytes)
		assert.True(t, strings.HasPrefix(msg, "Wrote 2 file(s): go.mod, go.sum."))
		assert.Contains(t, msg, "read_check_output(op_id) has the full list")
		assert.True(t, strings.HasSuffix(msg, "finishing the operation failed: boom."))
	})
}

func TestCheckToolsRequireAnAuthenticatedPrincipal(t *testing.T) {
	workDir := realTempDir(t)
	c := &ControlPlane{checkWorkDir: workDir}
	_, err := c.CheckProject(context.Background())
	require.EqualError(t, err, "authenticated principal is required")
	_, err = c.UpdateDependencies(context.Background())
	require.EqualError(t, err, "authenticated principal is required")
	id := strings.Repeat("ab", 16)
	writeTree(t, filepath.Join(workDir, id), map[string]string{outputFile: "secret test output", outputMetaFile: `{"captured":18,"dropped":0}`})
	page, err := c.ReadCheckOutput(context.Background(), &gensurla.ReadCheckOutputPayload{OpID: id})
	require.EqualError(t, err, "authenticated principal is required")
	assert.Nil(t, page)
	entries, err := os.ReadDir(workDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no operation directory was created")
}

func TestCapWriterRecordsGap(t *testing.T) {
	dir := realTempDir(t)
	w, err := newCapWriter(filepath.Join(dir, outputFile))
	require.NoError(t, err)
	w.limit = 10
	n, err := w.Write([]byte("0123456789abcdef"))
	require.NoError(t, err)
	assert.Equal(t, 16, n)
	require.NoError(t, w.close())
	got, err := os.ReadFile(filepath.Join(dir, outputFile))
	require.NoError(t, err)
	assert.Equal(t, "0123456789", string(got))
	raw, err := os.ReadFile(filepath.Join(dir, outputMetaFile))
	require.NoError(t, err)
	var meta outputMeta
	require.NoError(t, json.Unmarshal(raw, &meta))
	assert.Equal(t, outputMeta{Captured: 10, Dropped: 6}, meta)
}

func TestReadCheckOutputPages(t *testing.T) {
	workDir := realTempDir(t)
	id := strings.Repeat("ab", 16)
	opDir := filepath.Join(workDir, id)
	require.NoError(t, os.Mkdir(opDir, 0o700))
	text := strings.Repeat("x", maxCheckMessageBytes) + "tail"
	require.NoError(t, os.WriteFile(filepath.Join(opDir, outputFile), []byte(text), 0o600))
	raw, _ := json.Marshal(outputMeta{Captured: int64(len(text)), Dropped: 3})
	require.NoError(t, os.WriteFile(filepath.Join(opDir, outputMetaFile), raw, 0o600))
	c := &ControlPlane{checkWorkDir: workDir}
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})

	first, err := c.ReadCheckOutput(ctx, &gensurla.ReadCheckOutputPayload{OpID: id})
	require.NoError(t, err)
	assert.Len(t, first.Text, maxCheckMessageBytes)
	assert.Equal(t, int64(3), first.DroppedBytes)
	require.NotNil(t, first.NextCursor)

	second, err := c.ReadCheckOutput(ctx, &gensurla.ReadCheckOutputPayload{OpID: id, Cursor: first.NextCursor})
	require.NoError(t, err)
	assert.Equal(t, "tail", second.Text)
	assert.Nil(t, second.NextCursor)

	otherID := strings.Repeat("cd", 16)
	foreign := encodeCursor(otherID, 0)
	_, err = c.ReadCheckOutput(ctx, &gensurla.ReadCheckOutputPayload{OpID: id, Cursor: &foreign})
	assertServiceError(t, err, "unprocessable")

	bad := "not-base64!"
	_, err = c.ReadCheckOutput(ctx, &gensurla.ReadCheckOutputPayload{OpID: id, Cursor: &bad})
	assertServiceError(t, err, "unprocessable")

	_, err = c.ReadCheckOutput(ctx, &gensurla.ReadCheckOutputPayload{OpID: otherID})
	assertServiceError(t, err, "not_found")

	_, err = c.ReadCheckOutput(ctx, &gensurla.ReadCheckOutputPayload{OpID: "../../etc"})
	assertServiceError(t, err, "not_found")
}

// assertServiceError requires err to be a goa ServiceError named name.
func assertServiceError(t *testing.T, err error, name string) {
	t.Helper()
	var se *goa.ServiceError
	require.True(t, errors.As(err, &se), "want ServiceError %s, got %v", name, err)
	assert.Equal(t, name, se.Name)
}
