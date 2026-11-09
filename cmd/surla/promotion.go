// This file holds the promotion target `surla serve -promotion-config`
// binds: it approves staged release binaries for a fresh launch.

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

	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
)

type (
	// promotionFile is the operator's CP5 configuration read from
	// -promotion-config. Every field is required.
	promotionFile struct {
		// ReportDir holds the full reports the evaluator wrote, named by
		// digest; it is the report_dir verification writes to.
		ReportDir string `json:"report_dir"`
		// ReleaseDir holds one directory per candidate commit, each holding
		// the binaries to deploy for that commit.
		ReleaseDir string `json:"release_dir"`
		// SuiteID is the evaluator suite a promotion requires.
		SuiteID string `json:"suite_id"`
		// SuiteCommit is the evaluator suite commit a promotion requires.
		SuiteCommit string `json:"suite_commit"`
		// Environment is the dependency profile a promotion requires.
		Environment string `json:"environment"`
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

// CurrentInputs returns the required suite and environment and the SHA-256
// digest of each configured binary in <release_dir>/<candidateCommit>/. It
// opens that directory once and reads every binary through it, so the
// digests come from one directory even if the operator publishes a
// replacement meanwhile; a later replacement makes this answer stale, as any
// answer can become. candidateCommit must be a full commit ID, since it names
// a directory here; it comes from approve_merge's payload or from a saved
// approval. A missing, unreadable or non-regular binary fails the call.
func (t *releaseTarget) CurrentInputs(_ context.Context, candidateCommit string) (controlplane.PromotionInputs, error) {
	if !verify.IsCommitID(candidateCommit) {
		return controlplane.PromotionInputs{}, fmt.Errorf("candidate commit %q is not a full commit ID", candidateCommit)
	}
	dir := filepath.Join(t.cfg.ReleaseDir, candidateCommit)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return controlplane.PromotionInputs{}, fmt.Errorf("open release directory for commit %s: %w", candidateCommit, err)
	}
	artifacts := make(map[string]string, len(t.cfg.Binaries))
	for _, name := range t.cfg.Binaries {
		digest, err := hashFile(root, name)
		if err != nil {
			return controlplane.PromotionInputs{}, errors.Join(
				fmt.Errorf("read release binary %s for commit %s: %w", name, candidateCommit, err), root.Close())
		}
		artifacts[name] = digest
	}
	if err := root.Close(); err != nil {
		return controlplane.PromotionInputs{}, fmt.Errorf("close release directory for commit %s: %w", candidateCommit, err)
	}
	return controlplane.PromotionInputs{
		SuiteID:     t.cfg.SuiteID,
		SuiteCommit: t.cfg.SuiteCommit,
		Environment: t.cfg.Environment,
		Artifacts:   artifacts,
	}, nil
}

// LoadReport returns the full report the evaluator saved under digest in
// report_dir.
func (t *releaseTarget) LoadReport(_ context.Context, digest string) (verify.Report, error) {
	return verifyrun.LoadReport(t.cfg.ReportDir, digest)
}

// loadPromotionConfig reads and validates -promotion-config. An empty path
// means approve_merge stays unavailable; any missing field, unknown field,
// trailing data, suite_commit that is not a full commit ID, or binary name
// that is not a distinct artifact name stops startup. Relative directories
// are resolved against the working directory once, here.
func loadPromotionConfig(path string) (controlplane.PromotionTarget, error) {
	if path == "" {
		return nil, nil
	}
	f, err := decodePromotionFile(path)
	if err != nil {
		return nil, err
	}
	return &releaseTarget{cfg: f}, nil
}

// decodePromotionFile strictly decodes and validates a promotion configuration
// file and resolves its relative directories against the current working
// directory.
func decodePromotionFile(path string) (promotionFile, error) {
	wd, err := os.Getwd()
	if err != nil {
		return promotionFile{}, fmt.Errorf("-promotion-config %s: %w", path, err)
	}
	f, err := decodePromotionFileAtBase(path, wd)
	if err != nil {
		return promotionFile{}, err
	}
	f.ReportDir = filepath.Clean(f.ReportDir)
	f.ReleaseDir = filepath.Clean(f.ReleaseDir)
	return f, nil
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
