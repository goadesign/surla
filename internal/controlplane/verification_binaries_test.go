// Verification binary guidance exposes the completed run's recorded output
// folder only to the user whose promotion target can consume those binaries.
package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/verifyrun"
)

// TestVerificationBinariesVisibility preserves agent reads without resolving
// promotion settings, even when the agent shares the user's owner identity.
func TestVerificationBinariesVisibility(t *testing.T) {
	for _, tc := range []struct {
		name       string
		kind       CallerKind
		configured bool
		visible    bool
	}{
		{"configured user", CallerUser, true, true},
		{"report-only user", CallerUser, false, false},
		{"guide", CallerGuide, true, false},
		{"builder", CallerBuilder, true, false},
		{"assigned test writer", CallerTestWriter, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp, card, runsDir, runID := binariesReadFixture(t)
			if tc.kind == CallerTestWriter {
				_, err := cp.StartBuild(asCaller("alice@example.com", CallerUser),
					&gensurla.StartBuildPayload{ChangeID: card.ChangeID, RequestRevision: card.RequestRevision})
				require.NoError(t, err, "the test writer must hold an actual assignment")
			}
			calls := 0
			cp.resolvePromotionTarget = func(context.Context, string) (PromotionTarget, error) {
				calls++
				if tc.kind != CallerUser {
					return nil, errors.New("agent must not resolve private promotion settings")
				}
				if tc.configured {
					return &promotionTargetStub{}, nil
				}
				return nil, nil
			}
			for _, list := range []bool{false, true} {
				runs, err := readBinariesRuns(cp, asCaller("alice@example.com", tc.kind), card.ChangeID, runID, list)
				require.NoError(t, err)
				require.Len(t, runs, 1)
				if tc.visible {
					require.NotNil(t, runs[0].CandidateBinariesDir)
					assert.Equal(t, filepath.Join(runsDir, runID, "work", "candidate-bin"), *runs[0].CandidateBinariesDir)
				} else {
					assert.Nil(t, runs[0].CandidateBinariesDir)
				}
				completed, ok := runs[0].State.AsCompleted()
				require.True(t, ok)
				assert.True(t, completed.Report.Passed, "guidance does not change report-only verification")
			}
			if tc.kind == CallerUser {
				assert.Equal(t, 2, calls, "each read resolves the current promotion target")
			} else {
				assert.Zero(t, calls)
			}
		})
	}
}

// TestVerificationBinariesUseRecordedWorkDirectory proves that report identity
// and conventional run folders cannot substitute for a run's saved WorkDir.
func TestVerificationBinariesUseRecordedWorkDirectory(t *testing.T) {
	cp, card, runsDir, older := binariesReadFixture(t)
	newer := strings.Repeat("2", 32)
	writeListedRun(t, runsDir, "alice@example.com", card, newer,
		time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC), true)
	// Reuse exactly one report to distinguish run identity from report identity.
	result, err := os.ReadFile(filepath.Join(runsDir, older, "result.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(runsDir, newer, "result.json"), result, 0o600))
	want := map[string]string{}
	for _, runID := range []string{older, newer} {
		workDir := filepath.Join(t.TempDir(), "retained work", runID)
		editVerificationRequest(t, runsDir, runID, func(req *verifyrun.Request) {
			req.Plan.WorkDir = workDir
		})
		want[runID] = filepath.Join(workDir, "candidate-bin")
	}
	calls := 0
	cp.resolvePromotionTarget = func(context.Context, string) (PromotionTarget, error) {
		calls++
		return &promotionTargetStub{}, nil
	}
	user := asCaller("alice@example.com", CallerUser)
	listed, err := cp.ListVerifications(user, &gensurla.ListVerificationsPayload{ChangeID: card.ChangeID})
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "a list resolves promotion once, rather than once per run")
	require.Len(t, listed.Runs, 2)
	assert.Equal(t, newer, listed.Runs[0].RunID)
	for _, run := range listed.Runs {
		require.NotNil(t, run.CandidateBinariesDir)
		assert.Equal(t, want[run.RunID], *run.CandidateBinariesDir)
		got, err := cp.GetVerification(user, &gensurla.GetVerificationPayload{RunID: run.RunID})
		require.NoError(t, err)
		assert.Equal(t, run, got)
	}
	first, ok := listed.Runs[0].State.AsCompleted()
	require.True(t, ok)
	second, ok := listed.Runs[1].State.AsCompleted()
	require.True(t, ok)
	assert.Equal(t, first.Report.ReportDigest, second.Report.ReportDigest)
	assert.NotEqual(t, *listed.Runs[0].CandidateBinariesDir, *listed.Runs[1].CandidateBinariesDir)

	// A new server can read retained runs even without the original Change Card.
	restarted := newControlPlane(t, Options{RunsDir: runsDir, PromotionTarget: &promotionTargetStub{}})
	got, err := restarted.GetVerification(user, &gensurla.GetVerificationPayload{RunID: older})
	require.NoError(t, err)
	require.NotNil(t, got.CandidateBinariesDir)
	assert.Equal(t, want[older], *got.CandidateBinariesDir)
	assert.False(t, got.InputsCurrent, "historical binaries survive without current card inputs")
}

// TestVerificationBinariesAbsentBeforeCompletion keeps promotion configuration
// and malformed output locations irrelevant while no completed report exists.
func TestVerificationBinariesAbsentBeforeCompletion(t *testing.T) {
	for _, state := range []string{"running", "interrupted", "build failed"} {
		t.Run(state, func(t *testing.T) {
			cp, card, runsDir, runID := binariesReadFixture(t)
			editVerificationRequest(t, runsDir, runID, func(req *verifyrun.Request) {
				req.Plan.WorkDir = "private-relative-work"
			})
			switch state {
			case "running":
				lock, err := os.OpenFile(filepath.Join(runsDir, runID, "lock"), os.O_RDWR, 0o600)
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, lock.Close()) })
				require.NoError(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
			case "interrupted":
				require.NoError(t, os.Remove(filepath.Join(runsDir, runID, "result.json")))
			case "build failed":
				require.NoError(t, os.WriteFile(filepath.Join(runsDir, runID, "result.json"),
					[]byte(`{"kind":"no_report","failure":"execution_failed","stage":"build","summary":"The candidate did not build."}`), 0o600))
			}
			calls := 0
			cp.resolvePromotionTarget = func(context.Context, string) (PromotionTarget, error) {
				calls++
				return nil, errors.New("promotion settings must not be needed yet")
			}
			for _, list := range []bool{false, true} {
				runs, err := readBinariesRuns(cp, asCaller("alice@example.com", CallerUser), card.ChangeID, runID, list)
				require.NoError(t, err)
				require.Len(t, runs, 1)
				assert.Nil(t, runs[0].CandidateBinariesDir)
				if state == "running" {
					assert.Equal(t, gensurla.StateKindRunning, runs[0].State.Kind())
				} else {
					assert.Equal(t, gensurla.StateKindNotCompleted, runs[0].State.Kind())
				}
			}
			assert.Zero(t, calls)
		})
	}
}

// TestVerificationBinariesSafeFailures checks that only an eligible user read
// validates the path and that public errors never disclose private settings.
func TestVerificationBinariesSafeFailures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kind        CallerKind
		configured  bool
		resolverErr bool
		wantErr     string
	}{
		{"malformed exposed path", CallerUser, true, false, "saved binaries location could not be read"},
		{"report-only malformed path", CallerUser, false, false, ""},
		{"agent malformed path", CallerBuilder, true, false, ""},
		{"resolver failure", CallerUser, true, true, "promotion settings could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp, card, runsDir, runID := binariesReadFixture(t)
			privatePath := "private-relative-work/secret-release-folder"
			editVerificationRequest(t, runsDir, runID, func(req *verifyrun.Request) {
				req.Plan.WorkDir = privatePath
			})
			cp.resolvePromotionTarget = func(context.Context, string) (PromotionTarget, error) {
				if tc.resolverErr {
					return nil, errors.New("private promotion configuration: " + privatePath)
				}
				if tc.configured {
					return &promotionTargetStub{}, nil
				}
				return nil, nil
			}
			for _, list := range []bool{false, true} {
				runs, err := readBinariesRuns(cp, asCaller("alice@example.com", tc.kind), card.ChangeID, runID, list)
				if tc.wantErr == "" {
					require.NoError(t, err)
					require.Len(t, runs, 1)
					assert.Nil(t, runs[0].CandidateBinariesDir)
					continue
				}
				require.ErrorContains(t, err, tc.wantErr)
				assert.NotContains(t, err.Error(), privatePath)
				assert.NotContains(t, err.Error(), runsDir)
				assert.NotContains(t, err.Error(), "private promotion configuration")
				assert.Nil(t, runs)
			}
		})
	}
}

// TestVerificationBinariesAuthorizeBeforeResolution keeps foreign-owner reads
// from consulting or revealing even a failing promotion resolver.
func TestVerificationBinariesAuthorizeBeforeResolution(t *testing.T) {
	cp, card, _, runID := binariesReadFixture(t)
	calls := 0
	cp.resolvePromotionTarget = func(context.Context, string) (PromotionTarget, error) {
		calls++
		return nil, errors.New("private promotion configuration")
	}
	for _, list := range []bool{false, true} {
		runs, err := readBinariesRuns(cp, asCaller("bob@example.com", CallerUser), card.ChangeID, runID, list)
		require.ErrorContains(t, err, "is not the owner")
		assert.NotContains(t, err.Error(), "private promotion configuration")
		assert.Nil(t, runs)
	}
	assert.Zero(t, calls)
}

// TestVerificationBinariesResolveOutsideControlPlaneLock lets the embedding
// host acquire the control-plane write lock during each read's resolution.
func TestVerificationBinariesResolveOutsideControlPlaneLock(t *testing.T) {
	for _, list := range []bool{false, true} {
		name := "get"
		if list {
			name = "list"
		}
		t.Run(name, func(t *testing.T) {
			cp, card, _, runID := binariesReadFixture(t)
			acquired := make(chan struct{})
			cp.resolvePromotionTarget = func(ctx context.Context, _ string) (PromotionTarget, error) {
				go func() {
					cp.mu.Lock()
					cp.mu.Unlock()
					close(acquired)
				}()
				select {
				case <-acquired:
					return &promotionTargetStub{}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			ctx, cancel := context.WithTimeout(asCaller("alice@example.com", CallerUser), 5*time.Second)
			defer cancel()
			runs, err := readBinariesRuns(cp, ctx, card.ChangeID, runID, list)
			// If a regression holds the lock during resolution, the callback's
			// deadline lets the read release it and its lock probe finish.
			select {
			case <-acquired:
			case <-time.After(5 * time.Second):
				t.Fatal("promotion resolver's lock probe did not finish")
			}
			require.NoError(t, err, "promotion resolution must run without the control-plane read lock")
			require.Len(t, runs, 1)
			assert.NotNil(t, runs[0].CandidateBinariesDir)
		})
	}
}

// binariesReadFixture saves a completed run without candidate execution and
// leaves promotion resolution unbound until a test selects its behavior.
func binariesReadFixture(t *testing.T) (*ControlPlane, *gensurla.ChangeCardResult, string, string) {
	t.Helper()
	runsDir := t.TempDir()
	cp := newBuildControlPlane(t, Options{
		RunsDir:         runsDir,
		WrittenTestRuns: &WrittenTestRuns{ReportDir: t.TempDir(), Timeout: time.Minute},
	})
	card := confirmedChange(t, cp, "chg")
	runID := strings.Repeat("1", 32)
	writeListedRun(t, runsDir, "alice@example.com", card, runID,
		time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), true)
	return cp, card, runsDir, runID
}

// readBinariesRuns applies the same guidance assertions to the public single
// and list reads while preserving each method's returned error.
func readBinariesRuns(
	cp *ControlPlane,
	ctx context.Context,
	changeID, runID string,
	list bool,
) ([]*gensurla.VerificationStatusResult, error) {
	if list {
		result, err := cp.ListVerifications(ctx, &gensurla.ListVerificationsPayload{ChangeID: changeID})
		if err != nil {
			return nil, err
		}
		return result.Runs, nil
	}
	result, err := cp.GetVerification(ctx, &gensurla.GetVerificationPayload{RunID: runID})
	if err != nil {
		return nil, err
	}
	return []*gensurla.VerificationStatusResult{result}, nil
}
