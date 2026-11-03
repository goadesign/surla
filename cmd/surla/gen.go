package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/goadesign/surla/internal/designeval"
)

// quickstartGuide is the one generated file outside gen/: goa-ai's guide for
// coding agents, written at the module root.
const quickstartGuide = "AGENTS_QUICKSTART.md"

// renameatx is unix.RenameatxNp; gen_test.go replaces it to inject failures
// into the exchange.
var renameatx = unix.RenameatxNp

// runGen regenerates the gen/ directory of the design module in -dir from its
// design, inside Surla's sandbox, and replaces gen/ in one atomic exchange.
//
// It reads the module through one os.Root handle that it keeps until
// publication ends, generates with a design evaluator it owns
// (designeval.Evaluator.Generate), and writes the result into a new
// .surla-gen-<random> directory in the module. Before the exchange it
// rejects any path outside gen/ other than the root agent guide, writes and
// syncs every file, closes the evaluator, and checks for cancellation; any
// failure until then leaves gen/ untouched and returns an error. The exchange
// swaps the staged gen/ with the current one (or moves it in when there is
// none) with renameatx_np on the two held directory handles. That is the
// success point: afterwards the command returns nil, and writing the agent
// guide, removing the old tree and reporting the changes only print
// warnings. go.mod and go.sum are never written.
func runGen(args []string, stdout, stderr io.Writer) (err error) {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	dir := fs.String("dir", ".", "root directory of the design module (holding go.mod, go.sum and design/)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	cleanDir, root, err := openModuleRoot(*dir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	files, err := readModuleSnapshotFromRoot(cleanDir, root)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ev := designeval.NewEvaluator()
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
	guide, hasGuide := generated[quickstartGuide]
	delete(generated, quickstartGuide)
	for p := range generated {
		if p != path.Clean(p) || !strings.HasPrefix(p, "gen/") || strings.HasPrefix(p, "/") {
			return fmt.Errorf("refusing to write %q: generated files must be inside gen/", p)
		}
	}

	if leftovers := leftoverStages(root); len(leftovers) > 0 {
		fmt.Fprintf(stderr, "warning: %s holds staging directories from earlier runs, which surla gen leaves alone: %s\n",
			cleanDir, strings.Join(leftovers, ", "))
	}
	stage, err := newStage(root)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			err = errors.Join(err, stage.cleanup(root))
		}
	}()
	if err := stage.write(root, generated); err != nil {
		return err
	}

	// Everything that can fail runs before the exchange, so a failure leaves
	// gen/ untouched.
	evClosed = true
	if err := ev.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := stage.exchange(root); err != nil {
		return err
	}
	published = true

	fmt.Fprintf(stdout, "gen/ updated in %s\n", cleanDir)
	if hasGuide {
		if err := writeGuide(root, guide); err != nil {
			fmt.Fprintf(stderr, "warning: gen/ was updated; %s was not: %v\n", quickstartGuide, err)
		}
	}
	report, reportErr := stage.report(root)
	fmt.Fprint(stdout, report)
	if reportErr != nil {
		fmt.Fprintf(stderr, "warning: could not list every change: %v\n", reportErr)
	}
	if err := stage.cleanup(root); err != nil {
		fmt.Fprintf(stderr, "warning: %v\n", err)
	}
	if missing := missingRequirements(files, generated); len(missing) > 0 {
		fmt.Fprintf(stdout, "The generated code imports %s, which go.mod does not require. Run `go mod tidy` in %s.\n",
			strings.Join(missing, ", "), cleanDir)
	}
	fmt.Fprintln(stdout, "Next: surla preview -dir", cleanDir)
	return nil
}

// openModuleRoot opens dir as an os.Root after checking that dir is not a
// symbolic link and that the opened directory is the one checked.
func openModuleRoot(dir string) (string, *os.Root, error) {
	cleanDir := filepath.Clean(dir)
	lstatInfo, err := os.Lstat(cleanDir)
	if err != nil {
		return "", nil, err
	}
	if lstatInfo.Mode()&fs.ModeSymlink != 0 {
		return "", nil, fmt.Errorf("%s is a symbolic link; a design snapshot holds only regular files", dir)
	}
	root, err := os.OpenRoot(cleanDir)
	if err != nil {
		return "", nil, err
	}
	rootInfo, err := root.Stat(".")
	if err != nil {
		return "", nil, errors.Join(err, root.Close())
	}
	if !os.SameFile(lstatInfo, rootInfo) {
		return "", nil, errors.Join(fmt.Errorf("%s changed while opening design snapshot root", dir), root.Close())
	}
	return cleanDir, root, nil
}

// writeGuide writes the agent guide at the module root through a temporary
// file renamed over the old one, both inside root.
func writeGuide(root *os.Root, content []byte) error {
	tmp := "." + quickstartGuide + ".surla-" + strings.ToLower(rand.Text())
	if err := writeSynced(root, tmp, content); err != nil {
		return errors.Join(err, removeIfPresent(root, tmp))
	}
	if err := root.Rename(tmp, quickstartGuide); err != nil {
		return errors.Join(err, removeIfPresent(root, tmp))
	}
	return nil
}

// writeSynced creates name inside root, which must not exist, writes content,
// syncs it and closes it.
func writeSynced(root *os.Root, name string, content []byte) error {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

// removeIfPresent removes name inside root, treating a missing name as done.
func removeIfPresent(root *os.Root, name string) error {
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
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

// leftoverStages returns the names of .surla-gen-* entries in the module
// root, sorted. A directory that cannot be read yields none.
func leftoverStages(root *os.Root) []string {
	dir, err := root.Open(".")
	if err != nil {
		return nil
	}
	names, err := dir.Readdirnames(-1)
	if err := errors.Join(err, dir.Close()); err != nil {
		return nil
	}
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, ".surla-gen-") {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}
