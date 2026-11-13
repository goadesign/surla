// This file proves shutdown drains generation after cancellation is observed.
package controlplane

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/scaffold"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStartBuildCloseDrainsAfterCancellation holds the owner inside generation
// after it acknowledges cancellation. Once every goroutine is durably blocked,
// Close must still wait for explicit release; deleting its drain fails this test.
func TestStartBuildCloseDrainsAfterCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cards := t.TempDir()
		cp := newBuildControlPlane(t, Options{CardsDir: cards})
		card := confirmedChange(t, cp, "chg")
		entered, canceled := make(chan struct{}), make(chan struct{})
		release := make(chan struct{})
		releaseGeneration := sync.OnceFunc(func() { close(release) })
		defer releaseGeneration()
		copy := &buildCopyFixture{folder: t.TempDir()}
		copy.prepare = func(ctx context.Context, _ []designeval.StorySummary) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return errors.New("generation subprocess exited after cancellation")
		}
		cp.buildCopyFactory = func(context.Context, string, scaffold.Layout) (buildCopy, error) { return copy, nil }
		type outcome struct {
			result *gensurla.StartBuildResult
			err    error
		}
		started := make(chan outcome, 1)
		go func() {
			result, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{
				ChangeID: "chg", RequestRevision: card.RequestRevision,
			})
			started <- outcome{result: result, err: err}
		}()
		<-entered
		closed := make(chan error, 1)
		go func() { closed <- cp.Close() }()
		<-canceled
		synctest.Wait()
		returned := false
		select {
		case err := <-closed:
			returned = true
			t.Errorf("Close returned while canceled generation was still held: %v", err)
		default:
		}
		releaseGeneration()
		got := <-started
		require.NoError(t, got.err)
		if !returned {
			assert.NoError(t, <-closed)
		}
		failure, ok := got.result.Build.TestWriter.Tests.AsCouldNotRun()
		require.True(t, ok)
		assert.Equal(t, "The server stopped before the required tests could be prepared.", failure.Reason)
		assert.Equal(t, gensurla.TeamKindUnavailable, got.result.Team.Kind())
		reopened := newBuildControlPlane(t, Options{CardsDir: cards})
		assert.Equal(t, got.result.Build, mustStartedBuild(t, reopened, asCaller("alice@example.com", CallerUser), "chg"))
	})
}
