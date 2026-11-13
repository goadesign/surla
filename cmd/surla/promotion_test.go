// This file tests the staged-release promotion target behind
// the project's stored promotion settings.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/verify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// suiteCommit is the evaluator suite commit these tests require.
var suiteCommit = strings.Repeat("b", 40)

// TestLoadPromotionConfig checks that only a complete configuration with
// distinct plain binary names outside the bound workspace starts serve.
func TestLoadPromotionConfig(t *testing.T) {
	valid := `{"release_dir":"/rel","suite_id":"orders","suite_commit":"` + suiteCommit +
		`","environment":"sim","binaries":["orders","orders-worker"]}`
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{"complete", valid, ""},
		{"missing fields", `{"binaries":[]}`, "is missing release_dir, suite_id, suite_commit, environment, binaries"},
		{"unknown field", strings.TrimSuffix(valid, "}") + `,"extra":1}`, "unknown field"},
		{"trailing data", valid + "{}", "data after the configuration"},
		{"64-digit suite commit", strings.Replace(valid, suiteCommit, strings.Repeat("b", 64), 1), ""},
		{"binary with a path", strings.Replace(valid, `"orders-worker"`, `"bin/orders-worker"`, 1), "must not contain /"},
		{"parent directory", strings.Replace(valid, `"orders-worker"`, `".."`, 1), "is not a valid artifact name"},
		{"repeated binary", strings.Replace(valid, `"orders-worker"`, `"orders"`, 1), "is listed twice"},
	}
	for _, bad := range []string{strings.Repeat("b", 39), strings.Repeat("b", 41), strings.Repeat("b", 63),
		strings.Repeat("b", 65), strings.Repeat("g", 40), strings.Repeat("B", 40), "garbage"} {
		cases = append(cases, struct {
			name    string
			content string
			wantErr string
		}{"suite commit " + bad, strings.Replace(valid, suiteCommit, bad, 1), "must be a full commit ID"})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			wsDir := filepath.Join(root, "ws")
			require.NoError(t, os.MkdirAll(wsDir, 0o755))
			path := filepath.Join(root, "promotion.json")
			require.NoError(t, os.WriteFile(path, []byte(c.content), 0o600))
			target, err := storedPromotionTarget(t, path)
			if c.wantErr != "" {
				assert.ErrorContains(t, err, c.wantErr)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, target)
		})
	}

	// Relative directories are resolved against baseDir once, at load, and
	// paths inside the workspace are rejected.
	root := t.TempDir()
	baseDir, err := filepath.EvalSymlinks(filepath.Join(root, "ops"))
	if err != nil {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "ops"), 0o755))
		baseDir, err = filepath.EvalSymlinks(filepath.Join(root, "ops"))
		require.NoError(t, err)
	}
	wsDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	path := filepath.Join(baseDir, "promotion.json")
	relative := strings.Replace(strings.Replace(valid, `"/r"`, `"reports"`, 1), `"/rel"`, `"release"`, 1)
	require.NoError(t, os.WriteFile(path, []byte(relative), 0o600))
	target, err := storedPromotionTarget(t, path)
	require.NoError(t, err)
	cfg := target.(*releaseTarget).cfg
	assertOutsideDirectory(t, cfg.ReportDir, wsDir)
	assert.NotEqual(t, filepath.Join(baseDir, "reports"), cfg.ReportDir, "project owns reports")
	assert.Equal(t, filepath.Join(baseDir, "release"), cfg.ReleaseDir)
}

// TestReleaseTargetMeasuresStagedBinaries checks that CurrentInputs hashes
// the files in the commit's release directory and fails, naming the binary,
// when one is missing or reached through a link that leaves the directory.
func TestReleaseTargetMeasuresStagedBinaries(t *testing.T) {
	commit := strings.Repeat("c", 40)
	release := t.TempDir()
	target := newReleaseTarget(t, release, t.TempDir())
	publish(t, release, commit, map[string]string{"orders": "api v1", "orders-worker": "worker v1"})

	inputs, err := target.CurrentInputs(context.Background(), commit)
	require.NoError(t, err)
	assert.Equal(t, controlplane.PromotionInputs{
		SuiteID: "orders", SuiteCommit: suiteCommit, Environment: "sim",
		Artifacts: map[string]string{"orders": digestOf("api v1"), "orders-worker": digestOf("worker v1")},
	}, inputs)

	_, err = target.CurrentInputs(context.Background(), strings.Repeat("d", 40))
	assert.ErrorContains(t, err, "open release directory for commit "+strings.Repeat("d", 40))

	missing := strings.Repeat("e", 40)
	publish(t, release, missing, map[string]string{"orders": "api v1"})
	_, err = target.CurrentInputs(context.Background(), missing)
	assert.ErrorContains(t, err, "read release binary orders-worker for commit "+missing)

	escaping := strings.Repeat("f", 40)
	publish(t, release, escaping, map[string]string{"orders": "api v1"})
	outside := filepath.Join(t.TempDir(), "worker")
	require.NoError(t, os.WriteFile(outside, []byte("worker v1"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(release, escaping, "orders-worker")))
	_, err = target.CurrentInputs(context.Background(), escaping)
	assert.ErrorContains(t, err, "path escapes from parent", "a link out of the release directory is not followed")

	long := strings.Repeat("a", 64)
	publish(t, release, long, map[string]string{"orders": "api v1", "orders-worker": "worker v1"})
	_, err = target.CurrentInputs(context.Background(), long)
	assert.NoError(t, err, "a SHA-256 commit ID names a release directory too")

	for _, bad := range []string{"../" + commit, commit + "/..", strings.Repeat("c", 39), strings.ToUpper(commit)} {
		_, err = target.CurrentInputs(context.Background(), bad)
		assert.ErrorContains(t, err, "is not a full commit ID", bad)
	}
}

// TestReleaseTargetRefusesNonRegularFiles stages a FIFO under a binary's
// name and checks that CurrentInputs fails promptly instead of blocking on it.
func TestReleaseTargetRefusesNonRegularFiles(t *testing.T) {
	commit := strings.Repeat("c", 40)
	release := t.TempDir()
	target := newReleaseTarget(t, release, t.TempDir())
	publish(t, release, commit, map[string]string{"orders": "api v1"})
	require.NoError(t, syscall.Mkfifo(filepath.Join(release, commit, "orders-worker"), 0o600))

	done := make(chan error, 1)
	go func() {
		_, err := target.CurrentInputs(context.Background(), commit)
		done <- err
	}()
	select {
	case err := <-done:
		assert.ErrorContains(t, err, "orders-worker is not a regular file")
	case <-time.After(10 * time.Second):
		t.Fatal("CurrentInputs blocked on a FIFO")
	}
}

// TestApproveMergeChecksTheStagedRelease approves a confirmed card against a
// real saved report and staged release, then replaces the release directory
// with different binaries and checks that the saved approval is no longer
// returned, and comes back when the approved binaries are published again.
func TestApproveMergeChecksTheStagedRelease(t *testing.T) {
	commit := strings.Repeat("c", 40)
	release := t.TempDir()
	publish(t, release, commit, map[string]string{"orders": "api v1", "orders-worker": "worker v1"})
	alice := controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: controlplane.CallerUser})

	// The card is confirmed first, then the evaluator's report for its
	// revision is saved, and serve restarts with the promotion target.
	cardsDir := t.TempDir()
	cp, err := controlplane.New(controlplane.Options{CardsDir: cardsDir})
	require.NoError(t, err)
	card, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant.", Analysis: confirmedAnalysis()})
	require.NoError(t, err)
	card, err = cp.ConfirmIntent(alice, &gensurla.ConfirmIntentPayload{ChangeID: card.ChangeID, RequestRevision: card.RequestRevision, Answers: map[string]string{}, Examples: card.Examples})
	require.NoError(t, err)
	require.NoError(t, cp.Close())
	report := saveReport(t, verify.Binding{
		CandidateCommit: commit, RequestRevision: card.RequestRevision, SuiteID: "orders", SuiteCommit: suiteCommit,
		Environment: "sim", Artifacts: map[string]string{"orders": digestOf("api v1"), "orders-worker": digestOf("worker v1")},
	}, true)
	cp, err = controlplane.New(controlplane.Options{
		CardsDir: cardsDir, PromotionTarget: newReleaseTarget(t, release, filepath.Dir(report.path)),
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })

	approved, err := cp.ApproveMerge(alice, &gensurla.ApproveMergePayload{
		ChangeID: "chg", RequestRevision: card.RequestRevision, CandidateCommit: commit, ReportDigest: report.digest,
	})
	require.NoError(t, err)
	require.NotNil(t, approved.Approval)
	assert.Equal(t, digestOf("worker v1"), approved.Approval.Artifacts["orders-worker"])

	publish(t, release, commit, map[string]string{"orders": "api v1", "orders-worker": "worker v2"})
	got, err := cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Nil(t, got.Approval, "the staged binaries no longer match the approved ones")

	publish(t, release, commit, map[string]string{"orders": "api v1", "orders-worker": "worker v1"})
	got, err = cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, approved.Approval, got.Approval, "the approval holds again for the approved binaries")
	assert.Nil(t, got.UnverifiedApproval)

	// Removing a configured binary or the staged commit directory causes the
	// real releaseTarget.CurrentInputs to fail; GetChange still returns the
	// card with UnverifiedApproval and a path-free reason.
	require.NoError(t, os.Remove(filepath.Join(release, commit, "orders-worker")))
	got, err = cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Nil(t, got.Approval)
	require.NotNil(t, got.UnverifiedApproval)
	assert.Equal(t, approved.Approval, got.UnverifiedApproval.Approval)
	assert.Contains(t, got.UnverifiedApproval.Reason, "could not report its current inputs")
	assert.NotContains(t, got.UnverifiedApproval.Reason, release)

	require.NoError(t, os.RemoveAll(filepath.Join(release, commit)))
	got, err = cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Nil(t, got.Approval)
	require.NotNil(t, got.UnverifiedApproval)
	assert.Equal(t, approved.Approval, got.UnverifiedApproval.Approval)
	assert.Contains(t, got.UnverifiedApproval.Reason, "could not report its current inputs")
	assert.NotContains(t, got.UnverifiedApproval.Reason, release)
}

// confirmedAnalysis returns the analysis these tests propose: the
// instant-refund request with no open questions, so the card is confirmed at
// once, one deterministic example at the $50.00 limit, and no preserved
// behaviors.
func confirmedAnalysis() *gensurla.CP1Analysis {
	return &gensurla.CP1Analysis{
		Summary:     "Refunds up to $50 are instant.",
		Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
		Examples: []*gensurla.CP1AnalysisExample{{
			Scenario: "Refund at the limit", InputSummary: "Order total $50.00",
			ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
		}},
		PreservedBehaviors: []string{},
	}
}

// newReleaseTarget loads a promotion configuration for the orders binaries
// with the given release and report directories.
func newReleaseTarget(t *testing.T, release, reports string) controlplane.PromotionTarget {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "promotion.json")
	content := `{"release_dir":"` + release + `","suite_id":"orders","suite_commit":"` +
		suiteCommit + `","environment":"sim","binaries":["orders","orders-worker"]}`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	target, err := storedPromotionTarget(t, path)
	require.NoError(t, err)
	target.(*releaseTarget).cfg.ReportDir = reports
	return target
}

// publish writes files into a new directory and renames it to
// <release>/<commit>, the way an operator publishes a release: an earlier
// directory for that commit is first renamed aside, complete, never changed
// in place.
func publish(t *testing.T, release, commit string, files map[string]string) {
	t.Helper()
	staged, err := os.MkdirTemp(release, "staging-")
	require.NoError(t, err)
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(staged, name), []byte(content), 0o700))
	}
	dst := filepath.Join(release, commit)
	if _, err := os.Stat(dst); err == nil {
		retired, err := os.MkdirTemp(release, "retired-")
		require.NoError(t, err)
		require.NoError(t, os.Rename(dst, filepath.Join(retired, commit)))
	}
	require.NoError(t, os.Rename(staged, dst))
}

// digestOf returns the hex SHA-256 digest of content.
func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// storedPromotionTarget exercises named-file management and derives report storage at startup.
func storedPromotionTarget(t *testing.T, path string) (controlplane.PromotionTarget, error) {
	t.Helper()
	isolateServeWorkspace(t)
	if err := runProjectSettings("promotion", []string{"set", path}, io.Discard); err != nil {
		return nil, err
	}
	ws, err := configureServeWorkspace()
	return ws.promotionTarget, err
}
