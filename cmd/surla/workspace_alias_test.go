// These tests exercise CLI admission before operator files can be written
// through filesystem aliases of a registered builder workspace.
package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/verify"
)

// TestWorkspaceCaseAliasAdmission covers configuration files, private storage,
// and implicit cards selection without starting a server or evaluating code.
func TestWorkspaceCaseAliasAdmission(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	alias := filepath.Join(base, "WORKSPACE")
	require.NoError(t, os.Mkdir(workspace, 0o700))
	a, err := os.Stat(workspace)
	require.NoError(t, err)
	b, err := os.Stat(alias)
	if os.IsNotExist(err) {
		t.Skip("filesystem does not provide this case alias")
	}
	require.NoError(t, err)
	require.True(t, os.SameFile(a, b))
	for _, pair := range [][2]string{{workspace, alias}, {alias, workspace}} {
		inside := filepath.Join(pair[1], "missing", "operator")
		err := validateWorkspaceDirSeparation("cards directory", inside, pair[0])
		require.ErrorContains(t, err, "must be outside the workspace")
		err = validatePromotionPathSeparation(inside, pair[0])
		require.ErrorContains(t, err, "must be outside the workspace")
		outside := filepath.Join(base, "outside")
		for _, field := range []string{"config", "runs", "reports"} {
			config := outside
			cfg := &controlplane.VerificationConfig{RunsDir: outside, Plan: verify.Plan{ReportDir: outside}}
			switch field {
			case "config":
				config = inside
			case "runs":
				cfg.RunsDir = inside
			case "reports":
				cfg.Plan.ReportDir = inside
			}
			require.ErrorContains(t, validateVerificationConfigSeparation(config, cfg, pair[0]), "must be outside the workspace")
		}
		insideCards, err := withinAnyDir(filepath.Join(pair[1], ".surla", "cards"), []string{pair[0]})
		require.NoError(t, err)
		assert.True(t, insideCards, "implicit cards and import routing must recognize the actual builder directory")
	}
	priorConfigDir := userConfigDir
	userConfigDir = func() (string, error) { return filepath.Join(alias, "config"), nil }
	t.Cleanup(func() { userConfigDir = priorConfigDir })
	_, err = defaultOperatorRoot(workspace)
	require.ErrorContains(t, err, "must be outside the workspace")
	assert.NoDirExists(t, filepath.Join(workspace, "config"))
	assert.NoDirExists(t, filepath.Join(workspace, "missing"))

	// A missing registered root is an error, never permission to create a store.
	err = validateWorkspaceDirSeparation("cards directory", filepath.Join(base, "outside"), filepath.Join(base, "missing-root"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
