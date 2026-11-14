package modelsettings

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// keySentinel, tokenSentinel, headerSentinel and urlSentinel stand for
	// held values; none is a real credential.
	keySentinel    = "sentinel-key-4f1c"
	tokenSentinel  = "sentinel-token-9a2e"
	headerSentinel = "x-sentinel-header: 7d3b"
	// urlCredential is an independent credential inside a base URL's path.
	urlCredential = "sentinel-url-credential-61aa"
	urlSentinel   = "https://gateway.invalid/" + urlCredential + "/"
)

// realShapedKey has the shape of a real Anthropic key: a fixed prefix, a
// long random part and a fixed end; it is not a credential.
const realShapedKey = "sk-ant-api03-" + "sentinelRANDOMPART" +
	"0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ" + "q7w3e9r1t5u" + "ZZAA"

// sentinels lists every raw value a test holds.
var sentinels = []string{keySentinel, tokenSentinel, headerSentinel, urlCredential, urlSentinel}

// TestTableListsEachProvidersRequestSettings fixes the table's rows: the
// crossing allowlists, the key that must accompany them, the family prefix
// and the default model, in the order defaults are chosen.
func TestTableListsEachProvidersRequestSettings(t *testing.T) {
	providers := Providers()
	require.Len(t, providers, 2)
	assert.Equal(t, "anthropic", providers[0].Name)
	assert.Equal(t, "ANTHROPIC_", providers[0].Prefix)
	assert.Equal(t, "ANTHROPIC_API_KEY", providers[0].Key)
	assert.Equal(t, "claude-sonnet-4-6", providers[0].DefaultModel)
	assert.Equal(t, []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS"}, providers[0].Settings())
	assert.Equal(t, "openai", providers[1].Name)
	assert.Equal(t, "OPENAI_", providers[1].Prefix)
	assert.Equal(t, "OPENAI_API_KEY", providers[1].Key)
	assert.Equal(t, "gpt-5.4-mini", providers[1].DefaultModel)
	assert.Equal(t, []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_ORG_ID", "OPENAI_PROJECT_ID", "OPENAI_CUSTOM_HEADERS"}, providers[1].Settings())

	for _, tc := range []struct {
		name              string
		family            bool
		anthropic, openai bool
	}{
		{name: "ANTHROPIC_API_KEY", family: true, anthropic: true},
		{name: "ANTHROPIC_ADMIN_KEY", family: true},
		{name: "ANTHROPIC_PROFILE", family: true},
		{name: "ANTHROPIC_WEBHOOK_SIGNING_KEY", family: true},
		{name: "OPENAI_ORG_ID", family: true, openai: true},
		{name: "OPENAI_ADMIN_KEY", family: true},
		{name: "OPENAI_WEBHOOK_SECRET", family: true},
		{name: "SURLA_LIVE_MODEL_PROVIDER"},
		{name: "XANTHROPIC_API_KEY"},
		{name: "anthropic_api_key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.family, InFamily(tc.name))
			assert.Equal(t, tc.anthropic, providers[0].Crosses(tc.name))
			assert.Equal(t, tc.openai, providers[1].Crosses(tc.name))
		})
	}

	// The returned table is a copy: changing it changes no later lookup.
	providers[0].settings[0].name = "CHANGED"
	again, ok := Lookup("anthropic")
	require.True(t, ok)
	assert.Equal(t, "ANTHROPIC_API_KEY", again.Settings()[0])
	_, ok = Lookup("gemini")
	assert.False(t, ok)
}

// TestHeldValuesRevealNothingWhenFormatted prints and serializes a Value, a
// provider's Settings and a holder that keeps them in unexported fields
// with every verb, slog handler and marshaller; no output holds a value.
func TestHeldValuesRevealNothingWhenFormatted(t *testing.T) {
	anthropic := mustNew(t, "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": keySentinel, "ANTHROPIC_AUTH_TOKEN": tokenSentinel,
		"ANTHROPIC_BASE_URL": urlSentinel, "ANTHROPIC_CUSTOM_HEADERS": headerSentinel,
	})
	// holder mirrors how a server keeps settings: unexported fields, which
	// reflection prints without calling Value's methods.
	type holder struct {
		held   map[string]Settings
		values []Value
		one    Value
	}
	held := holder{held: map[string]Settings{"anthropic": anthropic}, values: []Value{newValue(keySentinel)}, one: newValue(tokenSentinel)}
	// Exported mirrors a holder whose fields marshallers can see.
	type exported struct {
		Held  map[string]Settings
		Value Value
		Map   map[string]Value
	}
	visible := exported{Held: held.held, Value: newValue(keySentinel), Map: map[string]Value{"k": newValue(urlSentinel)}}

	operands := map[string]any{
		"value": newValue(keySentinel), "settings": anthropic, "holder": held, "pointer to holder": &held,
		"exported": visible, "value map": map[string]Value{"ANTHROPIC_API_KEY": newValue(keySentinel)},
	}
	for name, operand := range operands {
		t.Run(name, func(t *testing.T) {
			var outputs []string
			for _, verb := range append(slices.Clone(formattingVerbs), "%T", "%p") {
				outputs = append(outputs, fmt.Sprintf(verb, operand))
			}
			outputs = append(outputs, fmt.Sprint(operand), fmt.Sprintln(operand))
			outputs = append(outputs, fmt.Errorf("wrapped: %v", operand).Error())
			for _, handler := range []func(*bytes.Buffer) slog.Handler{
				func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
				func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
			} {
				var buf bytes.Buffer
				slog.New(handler(&buf)).Info("held", "operand", operand)
				outputs = append(outputs, buf.String())
			}
			raw, err := json.Marshal(operand)
			require.NoError(t, err)
			outputs = append(outputs, string(raw))
			for _, out := range outputs {
				assertNoEncodedSentinel(t, out)
			}
		})
	}

	text, err := newValue(keySentinel).MarshalText()
	require.NoError(t, err)
	assert.Equal(t, Redacted, string(text))
	assert.Equal(t, Redacted, newValue(keySentinel).String())
	assert.Equal(t, Redacted, newValue(keySentinel).GoString())
	assert.Equal(t, Redacted, newValue(keySentinel).LogValue().String())
}

// TestValueFormatsAsTheRedactionWithEveryVerb formats a bare Value, and a
// pointer to one, with every verb and a range of flags, widths and
// precisions: each output is exactly the redaction, so no verb can render
// the value in another form, such as hexadecimal.
func TestValueFormatsAsTheRedactionWithEveryVerb(t *testing.T) {
	value := newValue(keySentinel)
	for _, verb := range formattingVerbs {
		assert.Equal(t, Redacted, fmt.Sprintf(verb, value), verb)
		assert.Equal(t, Redacted, fmt.Sprintf(verb, &value), "pointer, "+verb)
	}
	for _, verb := range []string{"%T", "%p"} {
		assertNoEncodedSentinel(t, fmt.Sprintf(verb, value))
		assertNoEncodedSentinel(t, fmt.Sprintf(verb, &value))
	}
}

// TestSettingsKeepNamesAndBuildProgramEnvironments holds a provider's
// settings and returns their names and their program environment, sorted.
func TestSettingsKeepNamesAndBuildProgramEnvironments(t *testing.T) {
	openai := mustNew(t, "openai", map[string]string{"OPENAI_ORG_ID": "org-1", "OPENAI_API_KEY": keySentinel, "OPENAI_BASE_URL": urlSentinel})
	assert.Equal(t, "openai", openai.Provider())
	assert.Equal(t, []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_ORG_ID"}, openai.Names())
	assert.Equal(t, []string{"OPENAI_API_KEY=" + keySentinel, "OPENAI_BASE_URL=" + urlSentinel, "OPENAI_ORG_ID=org-1"}, openai.Environ())
	var none Settings
	assert.Empty(t, none.Provider())
	assert.Empty(t, none.Names())
	assert.Empty(t, none.Environ())
}

// TestNewRefusesSettingsThatCannotBeHeld names the provider and the variable
// in each refusal, never a value.
func TestNewRefusesSettingsThatCannotBeHeld(t *testing.T) {
	for _, tc := range []struct {
		name, provider string
		values         map[string]string
		want           string
	}{
		{name: "unknown provider", provider: "gemini", values: map[string]string{"GEMINI_API_KEY": keySentinel}, want: `unknown model provider "gemini"`},
		{name: "missing key", provider: "anthropic", values: map[string]string{"ANTHROPIC_BASE_URL": urlSentinel}, want: "provider anthropic settings must include ANTHROPIC_API_KEY"},
		{name: "empty map", provider: "openai", values: map[string]string{}, want: "provider openai settings must include OPENAI_API_KEY"},
		{name: "unlisted name", provider: "anthropic", values: map[string]string{"ANTHROPIC_API_KEY": keySentinel, "ANTHROPIC_ADMIN_KEY": tokenSentinel}, want: "ANTHROPIC_ADMIN_KEY is not a request setting of provider anthropic"},
		{name: "other provider's name", provider: "anthropic", values: map[string]string{"ANTHROPIC_API_KEY": keySentinel, "OPENAI_ORG_ID": tokenSentinel}, want: "OPENAI_ORG_ID is not a request setting of provider anthropic"},
		{name: "empty value", provider: "openai", values: map[string]string{"OPENAI_API_KEY": keySentinel, "OPENAI_ORG_ID": ""}, want: "provider openai setting OPENAI_ORG_ID is empty"},
		{name: "NUL", provider: "anthropic", values: map[string]string{"ANTHROPIC_API_KEY": keySentinel, "ANTHROPIC_BASE_URL": urlSentinel + "\x00" + tokenSentinel}, want: "provider anthropic setting ANTHROPIC_BASE_URL contains a NUL character"},
		{name: "too long", provider: "anthropic", values: map[string]string{"ANTHROPIC_API_KEY": keySentinel, "ANTHROPIC_CUSTOM_HEADERS": strings.Repeat("é", MaxValueLength+1)}, want: "provider anthropic setting ANTHROPIC_CUSTOM_HEADERS is longer than 4096 characters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.provider, tc.values)
			require.Error(t, err)
			assert.Equal(t, tc.want, err.Error())
			for _, sentinel := range sentinels {
				assert.NotContains(t, err.Error(), sentinel)
			}
		})
	}
}

// TestSelectSendsRequestSettingsWithTheirKeyAndNamesTheRest covers what the
// surla command hands over from its shell.
func TestSelectSendsRequestSettingsWithTheirKeyAndNamesTheRest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		environ []string
		send    map[string]map[string]string
		notSent []Unsent
	}{
		{
			name:    "key and base URL send both; an admin key is named",
			environ: []string{"PATH=/bin", "ANTHROPIC_API_KEY=" + keySentinel, "ANTHROPIC_BASE_URL=" + urlSentinel, "ANTHROPIC_ADMIN_KEY=" + tokenSentinel},
			send:    map[string]map[string]string{"anthropic": {"ANTHROPIC_API_KEY": keySentinel, "ANTHROPIC_BASE_URL": urlSentinel}},
			notSent: []Unsent{{Name: "ANTHROPIC_ADMIN_KEY"}},
		},
		{
			name:    "a base URL without its key sends nothing and is named with the key",
			environ: []string{"ANTHROPIC_BASE_URL=" + urlSentinel},
			send:    map[string]map[string]string{},
			notSent: []Unsent{{Name: "ANTHROPIC_BASE_URL", MissingKey: "ANTHROPIC_API_KEY"}},
		},
		{
			name:    "a blank key counts as not set",
			environ: []string{"OPENAI_API_KEY= \t", "OPENAI_ORG_ID=org-1"},
			send:    map[string]map[string]string{},
			notSent: []Unsent{{Name: "OPENAI_ORG_ID", MissingKey: "OPENAI_API_KEY"}},
		},
		{
			name:    "both providers, empty values skipped, an unlisted variable without a key named plainly",
			environ: []string{"OPENAI_API_KEY=" + keySentinel, "OPENAI_PROJECT_ID=", "ANTHROPIC_API_KEY=" + tokenSentinel, "ANTHROPIC_PROFILE=work", "OPENAI_WEBHOOK_SECRET="},
			send: map[string]map[string]string{
				"anthropic": {"ANTHROPIC_API_KEY": tokenSentinel},
				"openai":    {"OPENAI_API_KEY": keySentinel},
			},
			notSent: []Unsent{{Name: "ANTHROPIC_PROFILE"}},
		},
		{
			name:    "the last of repeated entries counts, as a program sees it",
			environ: []string{"ANTHROPIC_API_KEY=", "ANTHROPIC_API_KEY=" + keySentinel},
			send:    map[string]map[string]string{"anthropic": {"ANTHROPIC_API_KEY": keySentinel}},
		},
		{
			name:    "no family",
			environ: []string{"PATH=/bin", "HOME=/home/user"},
			send:    map[string]map[string]string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Select(tc.environ)
			assert.Equal(t, tc.send, got.Send)
			assert.Equal(t, tc.notSent, got.NotSent)
		})
	}
}

// TestSplitSeparatesEveryProviderFamily keeps the order of both halves.
func TestSplitSeparatesEveryProviderFamily(t *testing.T) {
	family, rest := Split([]string{"PATH=/bin", "ANTHROPIC_ADMIN_KEY=a", "OPENAI_API_KEY=b", "HOME=/h", "ANTHROPIC_BASE_URL=c"})
	assert.Equal(t, []string{"ANTHROPIC_ADMIN_KEY=a", "OPENAI_API_KEY=b", "ANTHROPIC_BASE_URL=c"}, family)
	assert.Equal(t, []string{"PATH=/bin", "HOME=/h"}, rest)
}

// TestProtectReplacesHeldValuesInExportedText protects text the server takes
// from a live target's log: secrets become the redaction, a base URL keeps
// only its scheme and host, and a URL without a safe scheme and host becomes
// the redaction without quoting a parser error.
func TestProtectReplacesHeldValuesInExportedText(t *testing.T) {
	for _, tc := range []struct {
		name    string
		baseURL string
		want    string
		absent  []string
	}{
		{name: "independent credential in the path", baseURL: urlSentinel, want: "https://gateway.invalid", absent: []string{urlCredential}},
		{name: "user information, query and fragment", baseURL: "https://user:sentinel-pass-1@gateway.invalid:8443/p/sentinel-path-2?token=sentinel-query-3#sentinel-fragment-4",
			want: "https://gateway.invalid:8443", absent: []string{"sentinel-pass-1", "sentinel-path-2", "sentinel-query-3", "sentinel-fragment-4", "user:"}},
		{name: "no scheme", baseURL: "gateway.invalid/sentinel-path-5", want: Redacted, absent: []string{"sentinel-path-5"}},
		{name: "invalid escape", baseURL: "https://gateway.invalid/%zz-sentinel-path-6", want: Redacted, absent: []string{"sentinel-path-6", "invalid URL escape"}},
		{name: "invalid host", baseURL: "https://gate way.invalid/sentinel-path-7", want: Redacted, absent: []string{"sentinel-path-7", "invalid character"}},
		{name: "unclosed IPv6 host", baseURL: "https://[::1/sentinel-path-8", want: Redacted, absent: []string{"sentinel-path-8", "missing ']'"}},
		{name: "scheme only", baseURL: "https:///sentinel-path-9", want: Redacted, absent: []string{"sentinel-path-9"}},
		{name: "host holds a secret", baseURL: "https://" + keySentinel + ".invalid/x", want: Redacted, absent: []string{keySentinel}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			anthropic := mustNew(t, "anthropic", map[string]string{
				"ANTHROPIC_API_KEY": keySentinel, "ANTHROPIC_AUTH_TOKEN": tokenSentinel, "ANTHROPIC_BASE_URL": tc.baseURL,
			})
			openai := mustNew(t, "openai", map[string]string{"OPENAI_API_KEY": "sentinel-openai-key-0b", "OPENAI_ORG_ID": "org-visible", "OPENAI_PROJECT_ID": "proj-visible"})
			text := "key " + keySentinel + "\nurl " + tc.baseURL + "\n" + `json: unknown field "` + tokenSentinel + `"` + "\norg org-visible project proj-visible openai sentinel-openai-key-0b\n"
			got := ProtectExcerpt(text, 0, anthropic, openai)
			assert.Contains(t, got, "url "+tc.want+"\n")
			assert.Contains(t, got, "key "+Redacted+"\n")
			assert.Contains(t, got, `json: unknown field "`+Redacted+`"`)
			assert.Contains(t, got, "org org-visible project proj-visible", "organization and project stay visible")
			for _, absent := range append(tc.absent, keySentinel, tokenSentinel, tc.baseURL, "sentinel-openai-key-0b") {
				assert.NotContains(t, got, absent)
			}
		})
	}

	t.Run("values inside other values make one replacement", func(t *testing.T) {
		openai := mustNew(t, "openai", map[string]string{"OPENAI_API_KEY": "abc", "OPENAI_CUSTOM_HEADERS": "abc-def", "OPENAI_BASE_URL": "https://abc.invalid/abc"})
		assert.Equal(t, Redacted+" "+Redacted+" "+Redacted+" x", ProtectExcerpt("abc-def abc https://abc.invalid/abc x", 0, openai))
	})
	t.Run("nothing held", func(t *testing.T) {
		assert.Equal(t, "unchanged "+keySentinel, ProtectExcerpt("unchanged "+keySentinel, 0))
		assert.Equal(t, "unchanged", ProtectExcerpt("unchanged", 0, Settings{}))
	})
}

// TestProtectExcerptExportsValidUTF8 cuts four-byte characters at the
// excerpt's start and at the end of the text, with and without a held value
// next to the cut, and prints bytes that are not UTF-8. The excerpt is
// valid UTF-8 each time and keeps every whole character.
func TestProtectExcerptExportsValidUTF8(t *testing.T) {
	const clef = "\U0001D11E"
	held := mustNew(t, "anthropic", map[string]string{"ANTHROPIC_API_KEY": keySentinel, "ANTHROPIC_CUSTOM_HEADERS": "X-Sentinel: " + clef + "q" + clef})
	type excerpt struct {
		name, text string
		start      int
		want       string
	}
	var cases []excerpt
	for c := 1; c < utf8.UTFMax; c++ {
		before := "earlier " + clef[:c]
		cases = append(cases,
			excerpt{fmt.Sprintf("start cuts %d bytes into a character", c), "earlier " + clef + " later " + clef + "\n", len(before), " later " + clef + "\n"},
			excerpt{fmt.Sprintf("start cuts %d bytes into a character before a key", c), "earlier " + clef + keySentinel + "\n", len(before), Redacted + "\n"},
			excerpt{fmt.Sprintf("the text ends %d bytes into a character", c), "line " + clef + " end " + clef[:c], 0, "line " + clef + " end "},
			excerpt{fmt.Sprintf("the text ends %d bytes into a held value's character", c), "line X-Sentinel: " + clef[:c], 0, "line " + Redacted},
			excerpt{fmt.Sprintf("both edges cut, %d bytes", c), "x" + clef + " key " + keySentinel + " " + clef + clef[:c], len("x") + c, " key " + Redacted + " " + clef},
		)
	}
	cases = append(cases,
		excerpt{"a byte that is no character inside the excerpt", "a\xffb\n", 0, "a\uFFFDb\n"},
		excerpt{"a continuation byte that follows no character", "ab\x80c\n", 2, "\uFFFDc\n"},
		excerpt{"the whole log, starting with a continuation byte", "\x80log\n", 0, "\uFFFDlog\n"},
	)
	for _, tc := range cases {
		got := ProtectExcerpt(tc.text, tc.start, held)
		assert.Equal(t, tc.want, got, tc.name)
		assert.True(t, utf8.ValidString(got), tc.name)
	}
}

// TestProtectExcerptReplacesEveryByteOfHeldValues covers excerpts whose
// edges or whole extent cut held values: a cut at every offset of a key
// shaped like a real one, a token and a base URL with a path credential,
// with the cut value in the context; a text ending at every offset inside
// one; values overlapping each other, the second unfinished; a value whose
// copies overlap each other, which a search that resumes after each whole
// match would leave half exported; a value that begins with its own end
// printed back to back across the excerpt; a value
// longer than the excerpt, finished and unfinished; an excerpt shorter than
// a value; text that merely looks like a value's end, which stays whole;
// and values at the edge of ExcerptContext. No byte of a held value is
// returned, and every other byte is.
func TestProtectExcerptReplacesEveryByteOfHeldValues(t *testing.T) {
	anthropic := mustNew(t, "anthropic", map[string]string{
		"ANTHROPIC_API_KEY": realShapedKey, "ANTHROPIC_AUTH_TOKEN": tokenSentinel, "ANTHROPIC_BASE_URL": urlSentinel,
	})
	for name, tc := range map[string]struct{ value, replacement string }{
		"key":      {realShapedKey, Redacted},
		"token":    {tokenSentinel, Redacted},
		"base URL": {urlSentinel, "https://gateway.invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			before := "earlier log lines\n"
			for c := 0; c < len(tc.value); c++ {
				text := before + tc.value + "\nlater log lines\n"
				assert.Equal(t, tc.replacement+"\nlater log lines\n", ProtectExcerpt(text, len(before)+c, anthropic),
					"excerpt starting %d bytes into the value", c)
			}
			for c := 1; c < len(tc.value); c++ {
				assert.Equal(t, before+Redacted, ProtectExcerpt(before+tc.value[:c], 0, anthropic),
					"text ending %d bytes into the value", c)
			}
		})
	}

	t.Run("text that only looks like a value's end stays", func(t *testing.T) {
		key := mustNew(t, "anthropic", map[string]string{"ANTHROPIC_API_KEY": "sentinel-keyAA"})
		text := strings.Repeat("A", 8000) + " ok " + "sentinel-keyAA" + "\n"
		for _, start := range []int{0, 11, 7999} {
			assert.Equal(t, text[start:len(text)-15]+Redacted+"\n", ProtectExcerpt(text, start, key), "excerpt from %d", start)
		}
		assert.Equal(t, "AA starts the log "+Redacted+"\n", ProtectExcerpt("AA starts the log "+realShapedKey+"\n", 0, anthropic))
	})

	t.Run("overlapping values make one replacement", func(t *testing.T) {
		chained := mustNew(t, "openai", map[string]string{"OPENAI_API_KEY": "sentinel-chain-abcd", "OPENAI_CUSTOM_HEADERS": "abcd-efgh-sentinel"})
		text := "sentinel-chain-abcd-efgh-sentinel tail"
		for c := 0; c < len("sentinel-chain-abcd-efgh-sentinel"); c++ {
			assert.Equal(t, Redacted+" tail", ProtectExcerpt(text, c, chained), "excerpt from %d", c)
		}
		assert.Equal(t, "x "+Redacted, ProtectExcerpt("x sentinel-chain-abcd-efgh-", 0, chained), "the second value is unfinished")
		shared := mustNew(t, "openai", map[string]string{"OPENAI_API_KEY": "sentinel-AAAA-shared", "OPENAI_CUSTOM_HEADERS": "shared-sentinelBBBB"})
		assert.Equal(t, "x "+Redacted+" y", ProtectExcerpt("x sentinel-AAAA-shared-sentinelBBBB y", 0, shared))
	})

	t.Run("a value overlapping its own next copy", func(t *testing.T) {
		// The second copy begins with the first copy's last "zq".
		self := mustNew(t, "anthropic", map[string]string{"ANTHROPIC_API_KEY": "zq-sentinelSELF-zq"})
		text := "log zq-sentinelSELF-zq-sentinelSELF-zq\n"
		assert.Equal(t, "log "+Redacted+"\n", ProtectExcerpt(text, 0, self))
		for c := len("log "); c < len(text)-1; c++ {
			assert.Equal(t, Redacted+"\n", ProtectExcerpt(text, c, self), "excerpt from %d", c)
		}
	})

	t.Run("a value beginning with its own end, back to back", func(t *testing.T) {
		const border = "sk-proj-sentinelBORDERKEY-0123456789abcdefghijklmnopqrs"
		key := mustNew(t, "openai", map[string]string{"OPENAI_API_KEY": border})
		text := strings.Repeat(border, 200) + border[:40]
		assert.Equal(t, Redacted, ProtectExcerpt(text, len(text)-8192, key), "the unfinished last copy and every whole one form one run")
		separated := strings.Repeat(border+"\n", 200) + border[:40]
		got := ProtectExcerpt(separated, len(separated)-8192, key)
		assert.Empty(t, strings.Trim(strings.ReplaceAll(got, Redacted, ""), "\n"), "only separators remain")
	})

	t.Run("a value longer than the excerpt", func(t *testing.T) {
		text := "log\n" + realShapedKey
		assert.Equal(t, Redacted, ProtectExcerpt(text, len(text)-10, anthropic), "finished")
		unfinished := "log\n" + realShapedKey[:50]
		assert.Equal(t, Redacted, ProtectExcerpt(unfinished, len(unfinished)-5, anthropic), "unfinished")
		header := "j" + strings.Repeat("q", 28) + "j"
		headers := mustNew(t, "anthropic", map[string]string{"ANTHROPIC_API_KEY": keySentinel, "ANTHROPIC_CUSTOM_HEADERS": header})
		assert.Equal(t, Redacted, ProtectExcerpt(header[:29], 0, headers), "an excerpt shorter than the value, missing its last byte")
	})

	t.Run("values at the edge of the context", func(t *testing.T) {
		// The longest value New accepts: MaxValueLength four-byte characters.
		longest := strings.Repeat("\U0001D11E", MaxValueLength)
		require.Len(t, longest, ExcerptContext+1)
		held := mustNew(t, "anthropic", map[string]string{"ANTHROPIC_API_KEY": keySentinel, "ANTHROPIC_CUSTOM_HEADERS": longest})
		text := longest + "\nrest\n"
		assert.Equal(t, Redacted+"\nrest\n", ProtectExcerpt(text, ExcerptContext-(utf8.UTFMax-1), held), "the value's last character begins the excerpt")
		assert.Equal(t, "\nrest\n", ProtectExcerpt(text, ExcerptContext, held), "the value's last byte begins the excerpt, which leaves out the character it cuts")
		assert.Equal(t, "\nrest\n", ProtectExcerpt(text, len(longest), held), "the value ends just before the excerpt")
		shorter := "earlier " + keySentinel + "\nrest\n"
		assert.Equal(t, "\nrest\n", ProtectExcerpt(shorter, len("earlier "+keySentinel), held))
	})
}

// mustNew returns the settings New builds from values, or stops the test.
func mustNew(t *testing.T, provider string, values map[string]string) Settings {
	t.Helper()
	s, err := New(provider, values)
	require.NoError(t, err)
	return s
}

// formattingVerbs are the verbs, flags, widths and precisions Value is
// formatted with; each must print exactly the redaction.
var formattingVerbs = []string{
	"%v", "%+v", "%#v", "%s", "%q", "%+q", "%#q", "%x", "%X", "% x", "% X", "%#x", "%#X", "%08x",
	"%d", "%o", "%O", "%b", "%c", "%U", "%e", "%E", "%f", "%F", "%g", "%G", "%t",
	"%10s", "%-10v", "%.3s", "%10.4q", "%+#v",
}

// encodedForms returns s as printed in the forms a formatter or marshaller
// could reveal it: as is, quoted or JSON-escaped, in hexadecimal (lower and
// upper case, with and without spaces) and in every base64 alphabet.
func encodedForms(s string) []string {
	quoted := strconv.Quote(s)
	escaped, _ := json.Marshal(s)
	return []string{
		s, quoted[1 : len(quoted)-1], string(escaped[1 : len(escaped)-1]),
		hex.EncodeToString([]byte(s)), strings.ToUpper(hex.EncodeToString([]byte(s))),
		fmt.Sprintf("% x", s), fmt.Sprintf("% X", s),
		base64.StdEncoding.EncodeToString([]byte(s)), base64.URLEncoding.EncodeToString([]byte(s)),
		base64.RawStdEncoding.EncodeToString([]byte(s)), base64.RawURLEncoding.EncodeToString([]byte(s)),
	}
}

// assertNoEncodedSentinel fails for every sentinel text holds in any of its
// encoded forms.
func assertNoEncodedSentinel(t *testing.T, text string) {
	t.Helper()
	for _, sentinel := range sentinels {
		for _, form := range encodedForms(sentinel) {
			assert.NotContains(t, text, form)
		}
	}
}
