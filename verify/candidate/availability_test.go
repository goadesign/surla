// This file checks the read-only preflight shared by candidate execution paths.
package candidate

import (
	"errors"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSandboxAvailabilityIsShared verifies that preflight and both execution
// entries agree before any candidate path is opened or any process is started.
func TestSandboxAvailabilityIsShared(t *testing.T) {
	err := CheckSandbox()
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		require.NoError(t, err)
	} else {
		assert.ErrorIs(t, err, ErrIsolationUnavailable)
	}
	_, execErr := Exec(t.Context(), Artifact{}, Spec{})
	_, buildErr := resolveBuild(BuildSpec{})
	if err != nil {
		assert.ErrorIs(t, err, ErrIsolationUnavailable)
		assert.EqualError(t, execErr, err.Error())
		assert.EqualError(t, buildErr, err.Error())
		return
	}
	assert.ErrorContains(t, execErr, "artifact was not pinned")
	assert.Error(t, buildErr)
	assert.False(t, errors.Is(buildErr, ErrIsolationUnavailable))
}
