// This file is compiled twice: as part of package designeval and, with its
// package clause changed to main, as a source file of the generation program
// that Evaluator.Generate builds. It holds the one rule that recognizes a
// Surla test module and names the program the generator writes for it:
// scaffold uses it for the starter's layout, and the generation program for
// what it writes, so the two cannot name different packages. A module is a
// Surla test module, for which the generator writes the program, when three
// facts hold: its path is <service module>/eval (TestModuleService), its
// design declares exactly one evaluation suite, and it has an acceptance
// package (HasAcceptancePackage), which provides the five names the program
// calls. A path alone never makes someone's module Surla's. The generation
// program is compiled in its own harness module, which cannot import another
// package of Surla's module, so this file imports only the standard library.
package designeval

import (
	"go/token"
	"path"
	"strings"
)

const (
	// TestModuleDir is the folder, inside a service module, that holds
	// Surla's test module: surla init makes the module <service module>/eval
	// there.
	TestModuleDir = "eval"
	// AcceptancePackage is the folder, inside a Surla test module, of the
	// author's package that provides the names the generated program calls:
	// Name, Hooks, New, Live and LiveThroughGateway.
	AcceptancePackage = "acceptance"
)

// ValidServiceName reports whether name can name a service in Surla's layout:
// lowercase ASCII letters and digits, starting with a letter, and neither a Go
// keyword nor main. The service module's path ends in it, and the program the
// generator writes for its test module is named after it.
func ValidServiceName(name string) bool {
	if name == "main" || !token.IsIdentifier(name) {
		return false
	}
	for i, r := range name {
		if !('a' <= r && r <= 'z' || i > 0 && '0' <= r && r <= '9') {
			return false
		}
	}
	return true
}

// TestModuleService returns the service whose Surla test module has the path
// modulePath, <service module>/eval, and whether modulePath is one: its last
// element is TestModuleDir and the element before it is a ValidServiceName.
// For example, example.com/shop/eval gives shop.
func TestModuleService(modulePath string) (string, bool) {
	serviceModule, dir := path.Split(modulePath)
	if dir != TestModuleDir || serviceModule == "" {
		return "", false
	}
	service := path.Base(serviceModule)
	if !ValidServiceName(service) {
		return "", false
	}
	return service, true
}

// HasAcceptancePackage reports whether the module whose files are keyed by
// slash path relative to its root has an acceptance package: a non-test Go
// file directly in AcceptancePackage.
func HasAcceptancePackage(files map[string]string) bool {
	for p := range files {
		if path.Dir(p) == AcceptancePackage && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			return true
		}
	}
	return false
}

// SuiteCommandPackage returns the package, relative to the test module, of
// the program the generator writes for service's test module: the suite
// command that builds the module's evaluation suite from the generated tests
// and the author's acceptance package. For example, shop gives
// ./gen/cmd/shop-evals.
func SuiteCommandPackage(service string) string {
	return "./gen/cmd/" + service + "-evals"
}
