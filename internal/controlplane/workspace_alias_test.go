// These tests keep private storage outside builder directories when the
// filesystem accepts more than one spelling for a directory's name.
package controlplane

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/verify"
)

// TestNewRejectsAliasedWorkspaceStorage checks each operator-owned store before
// New can create its lock, for both workspace and enclosing-worktree grants.
func TestNewRejectsAliasedWorkspaceStorage(t *testing.T) {
	for _, gitTree := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			for _, exists := range []bool{false, true} {
				for _, field := range []string{"cards", "runs", "stage", "verification_runs", "reports"} {
					t.Run(fmt.Sprintf("%s/git=%t/reverse=%t/existing=%t", field, gitTree, reverse, exists), func(t *testing.T) {
						base := t.TempDir()
						actual := filepath.Join(base, "workspace")
						alias := filepath.Join(base, "WORKSPACE")
						ws := actual
						require.NoError(t, os.Mkdir(actual, 0o700))
						a, err := os.Stat(actual)
						require.NoError(t, err)
						b, err := os.Stat(alias)
						if os.IsNotExist(err) {
							t.Skip("filesystem does not provide this case alias; distinct case-sensitive paths are covered by abspath tests")
						}
						require.NoError(t, err)
						require.True(t, os.SameFile(a, b))
						store := filepath.Join(alias, "operator-store")
						if reverse {
							ws = alias
							store = filepath.Join(actual, "operator-store")
						}
						if gitTree {
							initGitWorktreeForBoundaryTest(t, actual)
							ws = filepath.Join(ws, "module")
							require.NoError(t, os.Mkdir(ws, 0o700))
						}
						if exists {
							require.NoError(t, os.Mkdir(store, 0o700))
						}
						opts := Options{WorkspaceDir: ws, CardsDir: filepath.Join(base, "cards")}
						switch field {
						case "cards":
							opts.CardsDir = store
						case "runs":
							opts.RunsDir = store
						case "stage":
							opts.InitStageDir = store
						case "verification_runs":
							opts.Verification = &VerificationConfig{RunsDir: store}
						case "reports":
							opts.Verification = &VerificationConfig{Plan: verify.Plan{ReportDir: store}}
						}
						cp, err := New(opts)
						if cp != nil {
							require.NoError(t, cp.Close())
						}
						require.ErrorContains(t, err, "must be outside")
						assert.NoFileExists(t, filepath.Join(opts.CardsDir, "lock"))
						if !exists {
							assert.NoDirExists(t, store, "admission must reject before creating storage")
						}
					})
				}
			}
		}
	}
}
