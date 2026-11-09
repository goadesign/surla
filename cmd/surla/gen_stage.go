package main

import (
	"go/parser"
	"go/token"
	"strconv"

	"golang.org/x/mod/modfile"
)

// moduleRequirements returns the module path and required module paths of a
// go.mod; an unparsable go.mod yields none.
func moduleRequirements(gomod string) (string, []string) {
	f, err := modfile.ParseLax("go.mod", []byte(gomod), nil)
	if err != nil || f.Module == nil {
		return "", nil
	}
	requires := make([]string, 0, len(f.Require))
	for _, r := range f.Require {
		requires = append(requires, r.Mod.Path)
	}
	return f.Module.Mod.Path, requires
}

// goImports returns the import paths of one Go source file; a file that does
// not parse yields none.
func goImports(src []byte) []string {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.ImportsOnly)
	if err != nil {
		return nil
	}
	imports := make([]string, 0, len(f.Imports))
	for _, spec := range f.Imports {
		if p, err := strconv.Unquote(spec.Path.Value); err == nil {
			imports = append(imports, p)
		}
	}
	return imports
}
