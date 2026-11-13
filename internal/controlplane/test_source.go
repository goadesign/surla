// This file answers where a project's tests come from: the evaluator stored
// with the project, Surla's own test layout, or neither.
package controlplane

import (
	"errors"
	"fmt"
	"io/fs"
	"path"

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
)

// projectTestSource answers where the project's tests come from, from facts
// the control plane holds today, in this order:
//
//  1. A stored evaluator when storedEvaluator reports one.
//  2. Surla's own test layout when the bound workspace, opened through
//     openModuleRootWithIdentity (the identity-checked root the design and
//     Story reads use), holds go.mod and eval/go.mod in its working tree and
//     scaffold.StarterLayout accepts both: the service module path ends in a
//     service name and the test module's path is that path followed by
//     /eval. The project server reads the same two files, at a commit, and
//     passes them to the same function to verify a starter project.
//  3. Neither, including when no workspace is bound, either file is absent,
//     or StarterLayout refuses them.
//
// It changes nothing. An error means a file could not be read for another
// reason, or the bound workspace was replaced (unprocessable).
func (c *ControlPlane) projectTestSource() (testSource, error) {
	if c.storedEvaluator() {
		return testSourceStoredEvaluator, nil
	}
	wsDir, wsDev, wsIno := c.currentWorkspaceTarget()
	if wsDir == "" {
		return testSourceNone, nil
	}
	_, root, err := openModuleRootWithIdentity(wsDir, wsDev, wsIno)
	if err != nil {
		return "", err
	}
	rootGoMod, rootErr := root.ReadFile("go.mod")
	testGoMod, testErr := root.ReadFile(path.Join(scaffold.SuiteDir, "go.mod"))
	if err := root.Close(); err != nil {
		return "", fmt.Errorf("close workspace %s: %w", wsDir, err)
	}
	for _, err := range []error{rootErr, testErr} {
		if errors.Is(err, fs.ErrNotExist) {
			return testSourceNone, nil
		}
		if err != nil {
			return "", fmt.Errorf("read the test layout of workspace %s: %w", wsDir, err)
		}
	}
	if _, err := scaffold.StarterLayout(rootGoMod, testGoMod); err != nil {
		return testSourceNone, nil
	}
	return testSourceStarterLayout, nil
}

// storedEvaluator reports projectTestSource's first answer without reading
// any file: the composition root bound Options.Verification and no
// Options.ResolveVerification, so verification always runs the operator's
// evaluator. The project server binds exactly that when the project has
// stored verification settings, and binds a resolver that derives the
// starter's tests from the workspace otherwise. Reads of a build use it to
// show that no test writer is used.
func (c *ControlPlane) storedEvaluator() bool {
	return c.verification != nil && c.resolveVerification == nil
}
