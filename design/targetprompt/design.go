// Package targetprompt describes, from Surla's side, the one operation Surla
// calls on a live CP3 target to change a prompt's text. It is not part of the
// surla API. The target declares the same method in its own design and owns
// its implementation, its prompt state and its validation; Surla generates
// only the client it uses from this design, so neither module depends on the
// other. The contract is documented in docs/REFERENCE.md.
package targetprompt

import (
	. "goa.design/goa/v3/dsl"
)

// nonBlankPattern matches a string with at least one character outside the
// whitespace strings.TrimSpace removes: ASCII whitespace including vertical
// tab, U+0085, U+00A0 and Unicode White_Space (U+1680, U+2000-U+200A, U+2028,
// U+2029, U+202F, U+205F, U+3000). The characters are literal, not escapes,
// because engines disagree on what \s and \u mean.
const nonBlankPattern = "[^ \t\n\v\f\r\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]"

var _ = API("targetprompt", func() {
	Title("CP3 live target prompt operation")
	Description("The operation a CP3 live target serves so Surla can replace a registered prompt's text while it runs.")
	Version("1.0.0")
})

// LiveKey is the credential Surla mints for one live target and passes to it
// through its environment.
var LiveKey = APIKeySecurity("live_key", func() {
	Description("Key Surla minted for this live target and passed to it at start.")
})

var _ = Service("prompts", func() {
	Description("Prompt text a live target lets Surla replace. Each call replaces the text of one prompt the target registered, for every caller of that target process, until the process exits.")
	Error("unauthorized", String, "The key is missing or is not this target's key.")
	Error("unknown_prompt", ErrorResult, "No prompt with this ID is registered.")
	Error("invalid_prompt", ErrorResult, "The template does not render with the prompt's registered spec.")

	Method("override_prompt", func() {
		Description("Replace the text of a registered prompt. The target renders the template with the prompt's registered spec before it saves it, so text that does not render is rejected and the text in use stays the same. Returns the version of the accepted text.")
		Security(LiveKey)
		Payload(func() {
			APIKey("live_key", "key", String, "This live target's key.")
			Attribute("prompt_id", String, "Registered prompt to replace.", func() {
				MinLength(1)
				Example("homes.helper.system")
			})
			Attribute("template", String, "New prompt text, as a Go text/template.", func() {
				MinLength(1)
			})
			Required("key", "prompt_id", "template")
		})
		Result(func() {
			Attribute("prompt_id", String, "The prompt whose text was replaced.")
			Attribute("version", String, "Version of the accepted text: an opaque identifier that is not blank.", func() {
				// At least one character that strings.TrimSpace would keep. The
				// class lists that whitespace set as literal characters, so Go,
				// JavaScript and other regular expression engines agree on it.
				Pattern(nonBlankPattern)
			})
			Required("prompt_id", "version")
		})
		HTTP(func() {
			PUT("/prompts/{prompt_id}")
			Header("key:X-Live-Target-Key")
			Body(func() {
				Attribute("template")
			})
			Response(StatusOK)
			Response("unauthorized", StatusUnauthorized)
			Response("unknown_prompt", StatusNotFound)
			Response("invalid_prompt", StatusUnprocessableEntity)
		})
	})
})
