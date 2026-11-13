// Package genpublish replaces a module's generated files through one held
// directory handle. The CLI and control plane share the same publication rules.
package genpublish

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/goadesign/surla/internal/renameat"
)

// Result records what Publish wrote and the file changes it could observe.
// Every path is relative to the caller's module root and lists are sorted.
// Warnings describe problems that did not prevent publication, including
// leftover directories from earlier runs or incomplete change listings.
type Result struct {
	// WrittenFiles lists generated files written by this call, including the
	// root agent guide only when writing it succeeded.
	WrittenFiles []string
	// Added lists files under gen/ that did not exist before publication.
	Added []string
	// Changed lists files under gen/ whose content changed.
	Changed []string
	// Removed lists files removed from gen/.
	Removed []string
	// Warnings describe incomplete reporting or cleanup and guide-write errors.
	Warnings []string
}

// quickstartGuide is the only generated file allowed outside gen/.
const quickstartGuide = "AGENTS_QUICKSTART.md"

// noReplace and exchangeNames are replaced by package tests to exercise
// publication failures and races.
var (
	noReplace     = renameat.NoReplace
	exchangeNames = renameat.Exchange
)

// Publish writes generated files into a new staging directory inside root and
// replaces gen/ in one atomic exchange. Only clean paths inside gen/ and the
// root AGENTS_QUICKSTART.md are accepted. It preserves top-level regular user
// files and their permissions, removes stale generated subdirectories, and
// never writes go.mod or go.sum. The input map is neither changed nor retained.
// The caller owns root and keeps it open until Publish returns.
//
// An error means gen/ was not published. Cancellation is checked immediately
// before the exchange. After that success point, guide-write, change-listing,
// and cleanup failures become warnings and Publish returns a nil error, so the
// caller never reports failure for a tree it already replaced. Result may hold
// warnings about earlier staging directories even when publication fails.
func Publish(
	ctx context.Context, root *os.Root, generated map[string][]byte,
) (result Result, err error) {
	for p := range generated {
		if p != quickstartGuide &&
			(p != path.Clean(p) || !strings.HasPrefix(p, "gen/") || strings.HasPrefix(p, "/")) {
			return result, fmt.Errorf("refusing to write %q: generated files must be inside gen/", p)
		}
	}
	if leftovers := leftoverStages(root); len(leftovers) > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"%s holds staging directories from earlier runs, which Surla leaves alone: %s",
			root.Name(), strings.Join(leftovers, ", ")))
	}
	stage, err := newStage(root)
	if err != nil {
		return result, err
	}
	published := false
	defer func() {
		if !published {
			err = errors.Join(err, stage.cleanup(root))
		}
	}()
	if err := stage.write(root, generated); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	var exchangeErr error
	published, exchangeErr = stage.exchange(root)
	if !published {
		return result, exchangeErr
	}
	if exchangeErr != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"gen/ was updated; closing exchange directory handles failed: %v", exchangeErr))
	}

	// gen/ is now replaced. All remaining failures describe incomplete
	// auxiliary work, and cannot turn this completed publication into failure.
	result.WrittenFiles = slices.Sorted(maps.Keys(generated))
	if guide, ok := generated[quickstartGuide]; ok {
		if err := writeGuide(root, guide); err != nil {
			result.WrittenFiles = slices.DeleteFunc(result.WrittenFiles, func(p string) bool {
				return p == quickstartGuide
			})
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"gen/ was updated; %s was not: %v", quickstartGuide, err))
		}
	}
	if err := stage.report(root, &result); err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("could not list every change: %v", err))
	}
	if err := stage.cleanup(root); err != nil {
		result.Warnings = append(result.Warnings, err.Error())
	}
	return result, nil
}

// RemoveLeftoverStages removes from the module open at root the staging
// directories that earlier publications left there because their process
// died before cleaning up. Left in place, each one would be reported by every
// later Publish. It is for a module in which one owner publishes at a time,
// such as the test module of a test writer's working copy, whose publications
// take turns: a staging directory another publication is still writing looks
// the same and would be removed with the others, which is why Publish itself
// leaves them alone.
//
// The module's files are untrusted, and a staging name proves nothing. Every
// step therefore goes through root and a handle on the directory itself,
// never through a path: an entry is treated as a staging directory only when
// it has the shape Publish gives one (openLeftover); only the gen tree inside
// the opened directory is removed, without following any link in it, so a
// link or a hard link there loses its name and what it names is untouched;
// and the directory's own name is removed only while it still names the
// opened directory, with a non-recursive remove. An entry with a staging name
// that is a symbolic link, a file, or a directory holding anything else is
// the module author's and is left exactly as it is, without an error.
//
// The error names each staging directory that could not be removed. It never
// means the module is unusable: Publish works beside whatever is left.
func RemoveLeftoverStages(root *os.Root) error {
	var errs []error
	for _, name := range leftoverStages(root) {
		inspected, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("left %s: %w", name, err))
			continue
		}
		stage, err := openLeftover(root, name, inspected)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if stage != nil {
			errs = append(errs, stage.cleanup(root))
		}
	}
	return errors.Join(errs...)
}
