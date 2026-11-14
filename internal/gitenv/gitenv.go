// Package gitenv owns the environment variables that make git act on another
// repository, work tree, index or object store than the one a command names,
// and the filter that removes them from an environment. Git exports some of
// them (GIT_DIR, GIT_WORK_TREE and GIT_INDEX_FILE) to the commands its hooks
// run, so a process started from a hook inherits them. Every Surla package
// that runs git on a repository it names (verification's checkout, the test
// writer's working copy, the starter's first commit) filters its environment
// here. Everything else stays: the user's identity, protocol, credential and
// configuration settings still reach git.
package gitenv

import (
	"slices"
	"strings"
)

// redirects are the variables that point git at another repository, work
// tree, index, object store, namespace or path prefix.
var redirects = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE",
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
// in also, which a caller gives for what it drops beyond redirects (the
// starter's first commit also drops GIT_TEMPLATE_DIR). The other entries keep
// their order.
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
