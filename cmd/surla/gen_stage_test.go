package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMissingRequirementsNamesImportsGoModLacks(t *testing.T) {
	files := map[string]string{"go.mod": "module example.com/shop\n\ngo 1.24\n\nrequire goa.design/goa/v3 v3.0.0\n"}
	generated := map[string][]byte{
		"gen/a/a.go":   []byte("package a\nimport (\n\t\"context\"\n\t\"goa.design/goa/v3/pkg\"\n\t\"example.com/shop/gen/b\"\n\t\"goa.design/goa-ai/runtime\"\n)\n"),
		"gen/goa.json": []byte("{}"),
	}
	assert.Equal(t, []string{"goa.design/goa-ai/runtime"}, missingRequirements(files, generated))
}

// layout writes files, keyed by slash-separated path, under dir.
func layout(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
}

// files returns every regular file under dir/sub, keyed by its path relative
// to dir, with its content. A missing sub is empty.
func files(t *testing.T, dir, sub string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(filepath.Join(dir, sub), func(p string, d os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return filepath.SkipDir
		}
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			content, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			out[filepath.ToSlash(rel)] = string(content)
		}
		return nil
	})
	require.NoError(t, err)
	return out
}
