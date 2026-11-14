package gitenv

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestWithoutRedirectsDropsOnlyRedirectsAndTheCallersNames checks that every
// redirect, before and after the other entries, and every name the caller
// adds is removed, and that the rest of the environment stays in order,
// including the settings a remote repository's clone may need: its
// protocol, credentials and configuration.
func TestWithoutRedirectsDropsOnlyRedirectsAndTheCallersNames(t *testing.T) {
	kept := []string{
		"PATH=/usr/bin:/bin",
		"GIT_AUTHOR_NAME=Ada",
		"GIT_SSH_COMMAND=ssh -i key",
		"GIT_ASKPASS=/usr/bin/askpass",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=x",
		"GIT_ALLOW_PROTOCOL=https:ssh:file",
		"GIT_DIRECTORY=not a redirect",
	}
	var redirected []string
	for _, name := range Redirects() {
		redirected = append(redirected, name+"=/elsewhere")
	}
	environ := slices.Concat(redirected[:1], kept, redirected[1:], []string{"GIT_TEMPLATE_DIR=/templates"})

	for _, c := range []struct {
		name string
		also []string
		want []string
	}{
		{name: "redirects only", want: append(slices.Clone(kept), "GIT_TEMPLATE_DIR=/templates")},
		{name: "with the caller's names", also: []string{"GIT_TEMPLATE_DIR"}, want: kept},
	} {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, WithoutRedirects(environ, c.also...))
		})
	}
}

// TestRedirectsReturnsACopy checks that a caller changing the returned list
// does not change what WithoutRedirects removes.
func TestRedirectsReturnsACopy(t *testing.T) {
	names := Redirects()
	names[0] = "PATH"
	assert.Equal(t, []string{"PATH=/usr/bin"}, WithoutRedirects([]string{"PATH=/usr/bin", "GIT_DIR=/x"}))
}
