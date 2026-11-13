// Package shquote quotes text so that a POSIX shell reads it as one word.
// Surla prints commands for the user to run, in the next steps after
// surla init and in refusals that name a git command. Every value such a
// command line carries, a folder's path for instance, goes through Quote, so
// that a name chosen by someone else cannot be read as shell syntax.
package shquote

import "strings"

// Quote returns s as one POSIX shell word: s wrapped in single quotes, with
// each single quote inside s written as a closing quote, an escaped quote and
// an opening quote. For example, my dir becomes 'my dir'. A shell expands
// nothing inside single quotes, so a command substitution, a backquote or a
// variable in s stays text.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
