// Package gitenv owns the environment variables that make git act on another
// repository, work tree, index, object store or reference store than the one
// a command names, and the filter that removes them from an environment. Git
// exports some of them (GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, GIT_PREFIX,
// GIT_OBJECT_DIRECTORY, GIT_QUARANTINE_PATH) to the commands its hooks run, so
// a process started from a hook inherits them.
//
// Its callers run git, or go commands that run git, on a repository they name
// while keeping the user's other Git settings: verification's checkout of the
// candidate and the suite and its go mod download and go env, the test
// writer's working copy, and the commands of the starter's first commit.
// Everything else in their environment stays, so the user's identity,
// protocol, credential and configuration settings still reach git. Callers
// that only read a repository and need none of those settings drop every
// GIT_* variable instead (the control plane's runGitRead and verification's
// repository queries).
package gitenv

import (
	"slices"
	"strings"
)

// redirects are the variables that make git use another repository, work
// tree, index, object store, reference store, namespace or path prefix, or
// that bound where it looks for one.
var redirects = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_REFERENCE_BACKEND", "GIT_NAMESPACE",
	"GIT_REPLACE_REF_BASE", "GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
	"GIT_PREFIX", "GIT_SUPER_PREFIX", "GIT_SHALLOW_FILE", "GIT_QUARANTINE_PATH",
}

// Redirects returns, in a new slice, the names WithoutRedirects removes.
// Tests use it to point each of them elsewhere.
func Redirects() []string {
	return slices.Clone(redirects)
}

// WithoutRedirects returns environ, a list of NAME=value entries such as
// os.Environ returns, without the redirect variables and without the names
// in also, which a caller drops beyond the redirects for reasons of its own
// and states where it calls this. The other entries keep their order.
func WithoutRedirects(environ []string, also ...string) []string {
	kept := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(redirects, name) && !slices.Contains(also, name) {
			kept = append(kept, kv)
		}
	}
	return kept
}
