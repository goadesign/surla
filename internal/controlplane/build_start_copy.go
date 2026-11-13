// This file binds build starts to the working-copy and generation owners.
package controlplane

import (
	"context"
	"path/filepath"

	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/scaffold"
	"github.com/goadesign/surla/internal/testwriter"
)

type (
	// buildCopy is the start's narrow execution boundary. The production owner
	// is testwriter; deterministic stand-ins exercise ordering and races.
	buildCopy interface {
		Project() string
		Refresh(context.Context) (testwriter.RefreshResult, error)
		Prepare(context.Context, []designeval.StorySummary) error
	}
	// writerBuildCopy keeps one opened copy and the suite checked before opening.
	writerBuildCopy struct {
		copy    testwriter.WorkingCopy
		checker *testwriter.Checker
		suite   testwriter.Suite
	}
)

// openBuildCopy creates no second owner or stored copy of working-copy facts.
func (c *ControlPlane) openBuildCopy(
	ctx context.Context, workspace string, layout scaffold.Layout,
) (buildCopy, error) {
	if c.buildCopyFactory != nil {
		return c.buildCopyFactory(ctx, workspace, layout)
	}
	wc, err := testwriter.OpenWorkingCopy(ctx, workspace)
	if err != nil {
		return nil, err
	}
	return writerBuildCopy{
		copy:    wc,
		checker: testwriter.NewChecker(c.evaluator, filepath.Join(c.cards.dir, "test-writer")),
		suite:   testwriter.Suite{Module: scaffold.SuiteDir, Command: layout.GeneratedSuiteCommand()},
	}, nil
}

// Project returns the project inside the copy, including a repository subfolder.
func (w writerBuildCopy) Project() string { return w.copy.Project() }

// Refresh delegates the safe branch transition to the copy owner.
func (w writerBuildCopy) Refresh(ctx context.Context) (testwriter.RefreshResult, error) {
	return w.copy.Refresh(ctx)
}

// Prepare writes the required tests from the saved Story through its owner.
func (w writerBuildCopy) Prepare(ctx context.Context, stories []designeval.StorySummary) error {
	_, err := w.checker.Prepare(ctx, w.copy, w.suite, stories)
	return err
}
