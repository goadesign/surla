// Package diff compares generated sources, domain declarations, Model diagrams,
// agent policies, and module dependencies without claiming runtime compatibility.
package diff

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/goadesign/surla/internal/designeval"
	"golang.org/x/mod/modfile"
)

type (
	// Report summarizes the evaluated Goa/Goa-AI design declarations and go.mod
	// directives that changed between two snapshots.
	Report struct {
		// HasChanges is true when at least one declaration, evaluated contract schema, or module directive changed.
		HasChanges bool
		// AddedDeclarations lists new generated sources and scoped declarations.
		AddedDeclarations []string
		// RemovedDeclarations lists deleted generated sources and scoped declarations.
		RemovedDeclarations []string
		// ModifiedDeclarations lists generated sources or scoped declarations that changed.
		ModifiedDeclarations []string
		// VersionDeltas lists changed, added, or removed go.mod require, replace, or exclude directives.
		VersionDeltas []string
		// Summary is a factual human-readable summary of the structural changes.
		Summary string
	}
)

// Compare evaluates the old and new design module snapshots with ev, each with
// its own go.mod dependencies (see designeval.Evaluator.Evaluate), compares
// their go.mod directives, and returns a factual structural diff Report. Both
// evaluations share ev's build workspace, so the second reuses what the first
// compiled. The caller owns ev and closes it.
func Compare(ctx context.Context, ev *designeval.Evaluator, oldFiles, newFiles map[string]string) (Report, error) {
	oldEval, err := ev.Evaluate(ctx, oldFiles)
	if err != nil {
		return Report{}, fmt.Errorf("evaluate old design: %w", err)
	}
	newEval, err := ev.Evaluate(ctx, newFiles)
	if err != nil {
		return Report{}, fmt.Errorf("evaluate new design: %w", err)
	}
	deltas, err := compareGoMod(oldFiles["go.mod"], newFiles["go.mod"])
	if err != nil {
		return Report{}, err
	}

	added, removed, modified := diffDeclarations(oldEval.Declarations, newEval.Declarations)
	hasChanges := len(added) > 0 || len(removed) > 0 || len(modified) > 0 || len(deltas) > 0

	summary := "No generated source, declared metadata, or module dependency changes."
	if hasChanges {
		parts := make([]string, 0, 4)
		if len(added) > 0 {
			parts = append(parts, fmt.Sprintf("added %d declaration(s) (%s)", len(added), strings.Join(added, ", ")))
		}
		if len(removed) > 0 {
			parts = append(parts, fmt.Sprintf("removed %d declaration(s) (%s)", len(removed), strings.Join(removed, ", ")))
		}
		if len(modified) > 0 {
			parts = append(parts, fmt.Sprintf("modified %d declaration(s) (%s)", len(modified), strings.Join(modified, ", ")))
		}
		if len(deltas) > 0 {
			parts = append(parts, fmt.Sprintf("updated %d module directive(s) (%s)", len(deltas), strings.Join(deltas, ", ")))
		}
		summary = "Structural diff: " + strings.Join(parts, "; ") + "."
	}

	return Report{
		HasChanges:           hasChanges,
		AddedDeclarations:    added,
		RemovedDeclarations:  removed,
		ModifiedDeclarations: modified,
		VersionDeltas:        deltas,
		Summary:              summary,
	}, nil
}

// diffDeclarations partitions old and new evaluated declaration maps into
// sorted added, removed, and modified declaration slices.
func diffDeclarations(oldDecls, newDecls map[string]string) (added, removed, modified []string) {
	added = make([]string, 0)
	removed = make([]string, 0)
	modified = make([]string, 0)
	for k, newSig := range newDecls {
		oldSig, hadOld := oldDecls[k]
		if !hadOld {
			added = append(added, k)
		} else if oldSig != newSig {
			modified = append(modified, k)
		}
	}
	for k := range oldDecls {
		if _, hasNew := newDecls[k]; !hasNew {
			removed = append(removed, k)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	slices.Sort(modified)
	return added, removed, modified
}

// compareGoMod parses two go.mod files using golang.org/x/mod/modfile and
// returns sorted human-readable deltas across require, replace, and exclude
// directives.
func compareGoMod(oldMod, newMod string) ([]string, error) {
	oldPins, err := parseGoModDirectives(oldMod)
	if err != nil {
		return nil, fmt.Errorf("parse old go.mod: %w", err)
	}
	newPins, err := parseGoModDirectives(newMod)
	if err != nil {
		return nil, fmt.Errorf("parse new go.mod: %w", err)
	}
	seen := make(map[string]bool, len(oldPins)+len(newPins))
	for k := range oldPins {
		seen[k] = true
	}
	for k := range newPins {
		seen[k] = true
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	deltas := make([]string, 0)
	for _, key := range keys {
		oldVer, hadOld := oldPins[key]
		newVer, hasNew := newPins[key]
		switch {
		case !hadOld && hasNew:
			deltas = append(deltas, fmt.Sprintf("%s: added %s", key, newVer))
		case hadOld && !hasNew:
			deltas = append(deltas, fmt.Sprintf("%s: removed %s", key, oldVer))
		case oldVer != newVer:
			deltas = append(deltas, fmt.Sprintf("%s: %s -> %s", key, oldVer, newVer))
		}
	}
	return deltas, nil
}

// parseGoModDirectives parses require, replace, and version-qualified exclude
// directives from a go.mod file string using golang.org/x/mod/modfile.
func parseGoModDirectives(raw string) (map[string]string, error) {
	pins := make(map[string]string)
	if strings.TrimSpace(raw) == "" {
		return pins, nil
	}
	parsed, err := modfile.Parse("go.mod", []byte(raw), nil)
	if err != nil {
		return nil, err
	}
	for _, req := range parsed.Require {
		if req == nil {
			continue
		}
		pins[req.Mod.Path] = req.Mod.Version
	}
	for _, rep := range parsed.Replace {
		if rep == nil {
			continue
		}
		oldKey := "replace " + rep.Old.Path
		if rep.Old.Version != "" {
			oldKey += "@" + rep.Old.Version
		}
		target := rep.New.Path
		if rep.New.Version != "" {
			target += " " + rep.New.Version
		}
		pins[oldKey] = target
	}
	for _, exc := range parsed.Exclude {
		if exc == nil {
			continue
		}
		pins["exclude "+exc.Mod.Path+"@"+exc.Mod.Version] = "excluded"
	}
	return pins, nil
}
