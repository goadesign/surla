package controlplane

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reportName is a valid recorded report basename.
const reportName = "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4.json"

// realTempDir returns a symlink-free temporary directory.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

// writeTree creates files (path -> content) under root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
}

// copiedFiles lists the slash paths of every non-directory entry under root.
func copiedFiles(t *testing.T, root string) []string {
	t.Helper()
	var got []string
	require.NoError(t, filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		require.NoError(t, err)
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	}))
	sort.Strings(got)
	return got
}

// caseInsensitive reports whether dir's volume resolves case aliases.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "case-probe")
	require.NoError(t, os.WriteFile(probe, nil, 0o600))
	_, err := os.Stat(filepath.Join(dir, "CASE-PROBE"))
	require.NoError(t, os.Remove(probe))
	return err == nil
}

func TestCopyGrantCopiesLiveTree(t *testing.T) {
	grant := realTempDir(t)
	writeTree(t, grant, map[string]string{
		"go.mod":                      "module example.com/shop\n",
		"go.sum":                      "sum\n",
		"main_test.go":                "package main\n",
		"bin/ignored.out":             "generated\n",
		".gitignore":                  "bin/\n",
		"testdata/fixture.json":       "{}\n",
		"testdata/mod/go.mod":         "module example.com/fixture\n",
		"eval/go.mod":                 "module example.com/shop/eval\n",
		"eval/acceptance/a_test.go":   "package acceptance\n",
		"_tools/go.mod":               "module example.com/tools\n",
		".git/HEAD":                   "ref: refs/heads/main\n",
		".surla/cards/card.json":      "{}\n",
		".surla-gen-123/gen.go":       "package gen\n",
		"reports/public-fixture.json": "{}\n",
	})
	require.NoError(t, os.Symlink("/etc/hosts", filepath.Join(grant, "hosts-link")))
	dst := realTempDir(t)

	got, err := copyGrant(grant, grant, dst, nil)
	require.NoError(t, err)

	assert.Equal(t, []string{".", "eval"}, got.modules)
	assert.Equal(t, []string{
		".gitignore", "_tools/go.mod", "bin/ignored.out", "eval/acceptance/a_test.go", "eval/go.mod",
		"go.mod", "go.sum", "hosts-link", "main_test.go", "reports/public-fixture.json",
		"testdata/fixture.json", "testdata/mod/go.mod",
	}, copiedFiles(t, dst))
	link, err := os.Readlink(filepath.Join(dst, "hosts-link"))
	require.NoError(t, err)
	assert.Equal(t, "/etc/hosts", link, "symlinks keep their raw target")
	assert.Equal(t, "sum\n", string(got.manifests["go.sum"]))
	assert.Equal(t, "module example.com/shop/eval\n", string(got.manifests["eval/go.mod"]))
}

func TestCopyGrantWorkspaceInsideMonorepo(t *testing.T) {
	grant := realTempDir(t)
	writeTree(t, grant, map[string]string{
		"lib/go.mod":           "module example.com/lib\n",
		"services/shop/go.mod": "module example.com/shop\n",
		"services/shop/go.sum": "\n",
	})
	dst := realTempDir(t)
	got, err := copyGrant(grant, filepath.Join(grant, "services", "shop"), dst, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"."}, got.modules, "only modules under the workspace are tested")
	assert.Contains(t, copiedFiles(t, dst), "lib/go.mod", "siblings in the grant are copied for replace directives")
}

func TestCopyGrantExcludesRecordedReports(t *testing.T) {
	type setup func(t *testing.T, grant string) (recorded string)
	cases := []struct {
		name            string
		needInsensitive bool
		setup           setup
		wantAbsent      []string
		wantPresent     []string
	}{
		{
			name: "same spelling",
			setup: func(t *testing.T, grant string) string {
				writeTree(t, grant, map[string]string{"reports/" + reportName: "full report\n"})
				return filepath.Join(grant, "reports", reportName)
			},
			wantAbsent:  []string{"reports/" + reportName},
			wantPresent: []string{"reports/public-fixture.json"},
		},
		{
			name:            "directory case alias",
			needInsensitive: true,
			setup: func(t *testing.T, grant string) string {
				writeTree(t, grant, map[string]string{"reports/" + reportName: "full report\n"})
				return filepath.Join(grant, "Reports", reportName)
			},
			wantAbsent:  []string{"reports/" + reportName},
			wantPresent: []string{"reports/public-fixture.json"},
		},
		{
			name:            "directory NFD alias",
			needInsensitive: true,
			setup: func(t *testing.T, grant string) string {
				writeTree(t, grant, map[string]string{"café/" + reportName: "full report\n"})
				return filepath.Join(grant, "café", reportName)
			},
			wantAbsent: []string{"café/" + reportName},
		},
		{
			name:            "file name upper-case alias",
			needInsensitive: true,
			setup: func(t *testing.T, grant string) string {
				writeTree(t, grant, map[string]string{"reports/A1B2C3D4A1B2C3D4A1B2C3D4A1B2C3D4A1B2C3D4A1B2C3D4A1B2C3D4A1B2C3D4.JSON": "full report\n"})
				return filepath.Join(grant, "reports", reportName)
			},
			wantAbsent:  []string{"reports/A1B2C3D4A1B2C3D4A1B2C3D4A1B2C3D4A1B2C3D4A1B2C3D4A1B2C3D4A1B2C3D4.JSON"},
			wantPresent: []string{"reports/public-fixture.json"},
		},
		{
			name:            "file name long-s alias",
			needInsensitive: true,
			setup: func(t *testing.T, grant string) string {
				writeTree(t, grant, map[string]string{"reports/a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4.jſon": "full report\n"})
				return filepath.Join(grant, "reports", reportName)
			},
			wantAbsent: []string{"reports/a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4.jſon"},
		},
		{
			name: "same-directory hard link is the protected file",
			setup: func(t *testing.T, grant string) string {
				writeTree(t, grant, map[string]string{"reports/" + reportName: "full report\n"})
				require.NoError(t, os.Remove(filepath.Join(grant, "reports", "public-fixture.json")))
				require.NoError(t, os.Link(filepath.Join(grant, "reports", reportName), filepath.Join(grant, "reports", "public-fixture.json")))
				return filepath.Join(grant, "reports", reportName)
			},
			wantAbsent: []string{"reports/" + reportName, "reports/public-fixture.json"},
		},
		{
			name: "distinct adjacent file with equal bytes is copied",
			setup: func(t *testing.T, grant string) string {
				writeTree(t, grant, map[string]string{"reports/" + reportName: "full report\n", "reports/copy.json": "full report\n"})
				return filepath.Join(grant, "reports", reportName)
			},
			wantAbsent:  []string{"reports/" + reportName},
			wantPresent: []string{"reports/copy.json"},
		},
		{
			name: "symlink at the recorded name is excluded without following it",
			setup: func(t *testing.T, grant string) string {
				require.NoError(t, os.Symlink("/nonexistent/target", filepath.Join(grant, "reports", reportName)))
				return filepath.Join(grant, "reports", reportName)
			},
			wantAbsent:  []string{"reports/" + reportName},
			wantPresent: []string{"reports/public-fixture.json"},
		},
		{
			name: "deleted original excludes nothing",
			setup: func(t *testing.T, grant string) string {
				return filepath.Join(grant, "reports", reportName)
			},
			wantPresent: []string{"reports/public-fixture.json"},
		},
		{
			name: "recorded parent missing excludes nothing",
			setup: func(t *testing.T, grant string) string {
				return filepath.Join(grant, "gone", reportName)
			},
			wantPresent: []string{"reports/public-fixture.json"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grant := realTempDir(t)
			if tc.needInsensitive && !caseInsensitive(t, grant) {
				t.Skip("needs a case-insensitive volume")
			}
			writeTree(t, grant, map[string]string{"go.mod": "module example.com/shop\n", "reports/public-fixture.json": "{}\n"})
			recorded := tc.setup(t, grant)
			dst := realTempDir(t)
			_, err := copyGrant(grant, grant, dst, []string{recorded})
			require.NoError(t, err)
			got := copiedFiles(t, dst)
			for _, p := range tc.wantAbsent {
				assert.NotContains(t, got, p)
			}
			for _, p := range tc.wantPresent {
				assert.Contains(t, got, p)
			}
		})
	}
}

func TestCopyEntryReportsVanishedEntry(t *testing.T) {
	grant := realTempDir(t)
	fd, err := syscall.Open(grant, openDirFlags, 0)
	require.NoError(t, err)
	defer syscall.Close(fd)
	c := &grantCopier{workspaceRel: ".", recorded: map[string][]string{}, out: grantCopy{manifests: map[string][]byte{}}}
	err = c.copyEntry(fd, "gone.txt", "gone.txt", realTempDir(t), nil)
	var changed *errSourceChanged
	require.True(t, errors.As(err, &changed))
	assert.Contains(t, err.Error(), "the workspace changed while check_project was copying gone.txt")
}
