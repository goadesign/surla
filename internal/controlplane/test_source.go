// This file answers where a project's tests come from: the evaluator stored
// with the project, Surla's own test layout, or neither.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"

	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/scaffold"
)

// testSource is where a project's tests come from.
type testSource string

const (
	// testSourceStoredEvaluator means the project verifies with the
	// evaluator the operator stored with `surla verification set`, so no
	// test writer is used.
	testSourceStoredEvaluator testSource = "stored_evaluator"
	// testSourceStarterLayout means the bound workspace has the test layout
	// `surla init` makes: a test module in eval/ beside the service module.
	testSourceStarterLayout testSource = "starter_layout"
	// testSourceNone means neither: no stored evaluator and no layout Surla
	// knows.
	testSourceNone testSource = "none"
	// maxLayoutFileBytes bounds each go.mod projectTestSource reads, so an
	// agent's file cannot make the server read without limit. A starter's
	// go.mod files hold a few kilobytes; any go.mod past this bound is not
	// one Surla wrote.
	maxLayoutFileBytes = 1 << 20
)

// projectTestSource answers where the project's tests come from, from facts
// the control plane holds today, in this order:
//
//  1. A stored evaluator when storedEvaluator reports one.
//  2. Surla's own test layout when the bound workspace, opened through
//     openModuleRootWithIdentity (the identity-checked root the design and
//     Story reads use), holds go.mod and eval/go.mod in its working tree as
//     regular files of at most maxLayoutFileBytes each, and
//     scaffold.StarterLayout accepts both: the service module path ends in a
//     service name and the test module's path is that path followed by
//     /eval. The project server reads the same two files, at a commit, and
//     passes them to the same function to verify a starter project.
//  3. Neither, including when no workspace is bound, or either file is
//     absent, is not a regular file there (a symbolic link at the file or at
//     eval, a directory, a FIFO), is larger than the bound, or StarterLayout
//     refuses them: the user can change any of those.
//
// The files are agents' files, so they are read with
// designeval.ReadRegularFile, which never follows a link or blocks on a
// special file. It changes nothing. An error means ctx ended, a file could not
// be read for another reason, or the bound workspace was replaced
// (unprocessable).
func (c *ControlPlane) projectTestSource(ctx context.Context) (testSource, error) {
	source, _, _, err := c.projectTestLayout(ctx)
	return source, err
}

// projectTestLayout returns the same source decision with its parsed starter
// layout and workspace, so a start opens the same project whose suite it checked.
func (c *ControlPlane) projectTestLayout(
	ctx context.Context,
) (_ testSource, _ scaffold.Layout, _ string, err error) {
	wsDir, wsDev, wsIno := c.currentWorkspaceTarget()
	if c.storedEvaluator() {
		return testSourceStoredEvaluator, scaffold.Layout{}, wsDir, nil
	}
	if wsDir == "" {
		return testSourceNone, scaffold.Layout{}, wsDir, nil
	}
	_, root, err := openModuleRootWithIdentity(wsDir, wsDev, wsIno)
	if err != nil {
		return "", scaffold.Layout{}, wsDir, err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close workspace %s: %w", wsDir, closeErr))
		}
	}()
	var goMods [2][]byte
	for i, rel := range []string{"go.mod", path.Join(scaffold.SuiteDir, "go.mod")} {
		if err := ctx.Err(); err != nil {
			return "", scaffold.Layout{}, wsDir, err
		}
		goMods[i], err = designeval.ReadRegularFile(root, rel, maxLayoutFileBytes)
		switch {
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, designeval.ErrNotRegularFile), errors.Is(err, designeval.ErrFileTooLarge):
			return testSourceNone, scaffold.Layout{}, wsDir, nil
		case err != nil:
			return "", scaffold.Layout{}, wsDir, fmt.Errorf("read the test layout of workspace %s: %w", wsDir, err)
		}
	}
	layout, err := scaffold.StarterLayout(goMods[0], goMods[1])
	if err != nil {
		return testSourceNone, scaffold.Layout{}, wsDir, nil
	}
	return testSourceStarterLayout, layout, wsDir, nil
}

// storedEvaluator reports projectTestSource's first answer without reading
// any file: this control plane verifies every change with the evaluator the
// operator stored with `surla verification set` (Options.Verification). The
// project server binds that exactly when the project has stored verification
// settings. Reads of a build use it to show that no test writer is used.
func (c *ControlPlane) storedEvaluator() bool {
	_, ok := c.verifier.(storedVerification)
	return ok
}
