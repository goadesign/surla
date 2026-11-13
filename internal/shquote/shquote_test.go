package shquote

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQuoteMakesOneShellWord checks the quoted text itself for the plain
// cases a reader can verify by eye.
func TestQuoteMakesOneShellWord(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"./shop":      `'./shop'`,
		"my dir":      `'my dir'`,
		"it's":        `'it'\''s'`,
		"$(rm -rf /)": `'$(rm -rf /)'`,
		"":            `''`,
	} {
		assert.Equal(t, want, Quote(in), in)
	}
}

// TestAShellReadsTheQuotedTextBack gives each quoted value to a real shell as
// the argument of printf: the shell must print the value unchanged, which
// fails if any part of it is expanded, split or run.
func TestAShellReadsTheQuotedTextBack(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"plain",
		"two words",
		"a$(echo run)b",
		"a`echo run`b",
		"$HOME and ${HOME}",
		`it's "quoted"`,
		`back\slash`,
		"star * and ? and [x]",
		"semi; colon & amp | pipe > out",
		"-leading-dash",
		"line\nbreak",
		"~tilde and #hash and !bang",
	} {
		out, err := exec.CommandContext(t.Context(), "sh", "-c", "printf %s "+Quote(in)).Output()
		require.NoError(t, err, in)
		assert.Equal(t, in, string(out))
	}
}
