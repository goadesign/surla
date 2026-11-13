package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/genpublish"
)

// quickstartGuide is the one generated file outside gen/: goa-ai's guide for
// coding agents, written at the module root.
const quickstartGuide = "AGENTS_QUICKSTART.md"

// gen regenerates the gen/ directory of the design module in -dir from its
// design, inside Surla's sandbox, and replaces gen/ in one atomic exchange.
//
// It reads the module through one os.Root handle that it keeps until
// publication ends, generates with a design evaluator it owns
// (designeval.Evaluator.Generate), and writes the result into a new
// .surla-gen-<random> directory in the module. Before the exchange it
// closes the evaluator, then genpublish.Publish rejects any path outside
// gen/ other than the root agent guide, writes and syncs every file, and
// checks for cancellation. Any failure until then leaves gen/ untouched and
// returns an error. The exchange
// swaps the staged gen/ with the current one (or moves it in without
// overwriting when there is none) with renameat.Exchange or renameat.NoReplace
// on the two held directory handles. That is the
// success point: afterwards the command returns nil, and writing the agent
// guide, removing the old tree and reporting the changes only print
// warnings. go.mod and go.sum are never written.
func (runtime projectRuntime) gen(args []string, stdout, stderr io.Writer) (err error) {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	dir := fs.String("dir", ".", "root directory of the design module (holding go.mod, go.sum and design/)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	cleanDir, root, err := designeval.OpenModuleRoot(*dir)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		closeErr := root.Close()
		switch {
		case closeErr == nil:
		case published:
			// gen/ is already updated; a failure to close the handle does not
			// undo that.
			fmt.Fprintf(stderr, "warning: gen/ was updated; closing the module directory failed: %v\n", closeErr)
		default:
			err = errors.Join(err, closeErr)
		}
	}()
	files, err := designeval.ReadModuleSnapshotFromRoot(cleanDir, root)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	evalWorkDir, err := runtime.localEvalWorkDir(cleanDir)
	if err != nil {
		return err
	}
	ev := designeval.NewEvaluatorInDir(evalWorkDir)
	evClosed := false
	defer func() {
		if !evClosed {
			err = errors.Join(err, ev.Close())
		}
	}()
	generated, err := ev.Generate(ctx, files)
	if err != nil {
		return err
	}
	// The command owns its evaluator; a failed close leaves gen/ untouched.
	evClosed = true
	if err := ev.Close(); err != nil {
		return err
	}
	result, publishErr := genpublish.Publish(ctx, root, generated)
	published = publishErr == nil
	if published {
		fmt.Fprintf(stdout, "gen/ updated in %s\n", cleanDir)
		for _, group := range []struct {
			label string
			paths []string
		}{{"added", result.Added}, {"changed", result.Changed}, {"removed", result.Removed}} {
			fmt.Fprintf(stdout, "%s: %d\n", group.label, len(group.paths))
			for _, p := range group.paths {
				fmt.Fprintf(stdout, "  %s\n", p)
			}
		}
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", warning)
	}
	if publishErr != nil {
		return publishErr
	}
	if missing := missingRequirements(files, generated); len(missing) > 0 {
		fmt.Fprintf(stdout, "The generated code imports %s, which go.mod does not require. Run `go mod tidy` in %s.\n",
			strings.Join(missing, ", "), cleanDir)
	}
	// Generation compiles only what the design imports; it does not build the
	// whole application against the new gen/, so it cannot tell whether the
	// rest of the hand-written code still matches it.
	fmt.Fprintln(stdout, "surla gen did not build the whole application against the new gen/;"+
		" check that hand-written code such as internal/ and cmd/ still matches it"+
		" before you run surla verify or start a live target.")
	fmt.Fprintln(stdout, "Next: surla preview -dir", cleanDir)
	return nil
}

// missingRequirements returns, sorted, the import paths in the generated Go
// files whose module go.mod does not require, ignoring the standard library
// and the module's own packages. It reads import lines only.
func missingRequirements(files map[string]string, generated map[string][]byte) []string {
	mod, requires := moduleRequirements(files["go.mod"])
	seen := map[string]bool{}
	for p, content := range generated {
		if !strings.HasSuffix(p, ".go") {
			continue
		}
		for _, imp := range goImports(content) {
			first, _, _ := strings.Cut(imp, "/")
			if !strings.Contains(first, ".") || imp == mod || strings.HasPrefix(imp, mod+"/") {
				continue
			}
			if !slices.ContainsFunc(requires, func(r string) bool { return imp == r || strings.HasPrefix(imp, r+"/") }) {
				seen[imp] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}
