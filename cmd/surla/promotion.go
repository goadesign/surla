// This file holds the project server's promotion target for independently measured staged binaries.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
)

type (
	// promotionFile is a validated CP5 settings snapshot plus its derived report store.
	promotionFile struct {
		// ReportDir is derived from private project state and is neither accepted
		// nor written in the operator settings JSON.
		ReportDir string `json:"-"`
		// ReleaseDir holds one directory per candidate commit, each holding
		// the binaries to deploy for that commit.
		ReleaseDir string `json:"release_dir"`
		// Binaries names the binaries every release directory must hold.
		Binaries []string `json:"binaries"`
	}

	// releaseTarget approves the binaries staged for a fresh launch: the
	// files in <release_dir>/<commit>/, which the operator's deploy step
	// starts as new processes with the evaluator suite's fixed
	// configuration. It says nothing about processes already running, which
	// may load runtime prompt overrides no report covers. The operator
	// publishes a complete commit directory in one step (for example by
	// renaming a fully written directory into release_dir) and never changes
	// its files in place.
	releaseTarget struct {
		// cfg is the validated configuration.
		cfg promotionFile
	}
)

// publishReleaseBinariesStep tells the user how to recover when a configured
// promotion target's release directory or binary cannot be read.
const publishReleaseBinariesStep = "publish the binaries the run built to <release_dir>/%s/, or remove the promotion settings"

// CurrentArtifacts returns the SHA-256 digest of each configured binary in
// <release_dir>/<candidateCommit>/. It opens that directory once and reads
// every binary through it, so the digests come from one directory even if the
// operator publishes a replacement meanwhile; a later replacement makes this
// answer stale, as any answer can become. candidateCommit must be a full commit
// ID, since it names a directory here; it comes from approve_merge's payload or
// from a saved approval. A missing, unreadable or non-regular binary fails the
// call with a path-free error naming the recovery step.
func (t *releaseTarget) CurrentArtifacts(_ context.Context, candidateCommit string) (map[string]string, error) {
	if !verify.IsCommitID(candidateCommit) {
		return nil, fmt.Errorf("candidate commit %q is not a full commit ID", candidateCommit)
	}
	dir := filepath.Join(t.cfg.ReleaseDir, candidateCommit)
	root, err := os.OpenRoot(dir)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		return nil, fmt.Errorf(
			"open release directory for commit %s: %w: "+publishReleaseBinariesStep,
			candidateCommit,
			err,
			candidateCommit,
		)
	}
	artifacts := make(map[string]string, len(t.cfg.Binaries))
	for _, name := range t.cfg.Binaries {
		digest, err := hashFile(root, name)
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf(
					"read release binary %s for commit %s: %w: "+publishReleaseBinariesStep,
					name,
					candidateCommit,
					err,
					candidateCommit,
				),
				root.Close(),
			)
		}
		artifacts[name] = digest
	}
	if err := root.Close(); err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		return nil, fmt.Errorf("close release directory for commit %s: %w", candidateCommit, err)
	}
	return artifacts, nil
}

// LoadReport returns the full report the evaluator saved under digest in
// report_dir.
func (t *releaseTarget) LoadReport(_ context.Context, digest string) (verify.Report, error) {
	return verifyrun.LoadReport(t.cfg.ReportDir, digest)
}

// hashFile returns the hex SHA-256 digest of the regular file name inside
// root. It opens without blocking and checks the opened file, so a FIFO or
// device staged under a binary's name fails instead of blocking the call.
func hashFile(root *os.Root, name string) (string, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", name)
	}
	if err != nil {
		return "", errors.Join(err, f.Close())
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	if err := errors.Join(err, f.Close()); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
