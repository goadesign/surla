// Package modelsettings names the environment variables of the direct model
// providers a live target or an operator's evaluator may call, and holds
// their values so that no formatting or serialization reveals them.
//
// A provider's family is every variable whose name begins with its prefix,
// such as ANTHROPIC_. Its request settings are the listed members of the
// family that reach the provider's client: its API key and the settings that
// route and authenticate its requests. Only request settings are handed to
// the project server; the whole family is removed wherever Surla removes it.
//
// The package holds pure values. It reads no process environment and builds
// no client: the surla command selects what it hands over from its own
// environment with Select, the project server holds what arrives as Settings
// and builds program environments from them, and processes that must not
// pass a family on remove it with Split.
package modelsettings

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"
)

type (
	// Provider is one row of the provider table: a direct model provider,
	// its family and the request settings that cross to the project server.
	Provider struct {
		// Name names the provider in configure_model and
		// hand_over_model_settings, such as "anthropic".
		Name string
		// Prefix begins the name of every variable in the provider's family.
		Prefix string
		// Key is the variable holding the provider's API key. Request
		// settings cross only together with it.
		Key string
		// DefaultModel is the model live targets use when the provider's
		// settings arrive while no model is chosen.
		DefaultModel string
		// settings lists the request settings that cross, in table order.
		settings []setting
	}

	// setting is one request setting of a provider.
	setting struct {
		// name is the variable's full name.
		name string
		// kind says how text the server exports protects its value.
		kind settingKind
	}

	// settingKind says how ProtectExcerpt treats a held value.
	settingKind int

	// Value is one held setting value. String, GoString, Format (every
	// verb), LogValue, MarshalJSON and MarshalText all answer Redacted; only
	// Settings.Environ and ProtectExcerpt read the value itself.
	Value struct {
		// raw returns the value. It is a function so that printing a struct
		// that holds a Value in an unexported field, which reflection cannot
		// format through Value's methods, shows only a function address.
		raw func() string
	}

	// Settings is one provider's request settings as the project server
	// holds them: every value is a Value, and the key is always present.
	// The zero Settings holds nothing and belongs to no provider.
	Settings struct {
		// provider is the provider whose settings these are.
		provider string
		// values maps each held setting's name to its value.
		values map[string]Value
	}

	// Selection is what the surla command hands over from its environment.
	Selection struct {
		// Send maps the name of each provider whose key is set to its
		// request settings that have values, keyed by variable name.
		Send map[string]map[string]string
		// NotSent lists the family variables with values that are not sent,
		// sorted by name.
		NotSent []Unsent
	}

	// Unsent is one family variable the surla command does not hand over.
	Unsent struct {
		// Name is the variable's name.
		Name string
		// MissingKey names the provider key whose absence stops a request
		// setting from crossing; it is empty for a variable that is not a
		// request setting, which never crosses.
		MissingKey string
	}
)

const (
	// Redacted is what every formatting of a Value shows, and what
	// ProtectExcerpt writes in place of a held secret.
	Redacted = "[redacted]"
	// MaxValueLength bounds one request setting value, in characters. New
	// refuses a longer value, the hand-over's design validates it, and the
	// surla command names it when the project server refuses a longer value.
	MaxValueLength = 4096
	// ExcerptContext is how many bytes before an excerpt ProtectExcerpt
	// needs to read: an occurrence of a held value that overlaps the excerpt
	// starts at most this far before it, because a held value has at most
	// MaxValueLength characters of at most utf8.UTFMax bytes each.
	ExcerptContext = MaxValueLength*utf8.UTFMax - 1
)

const (
	// kindSecret is a credential: ProtectExcerpt replaces it with Redacted.
	kindSecret settingKind = iota
	// kindURL is an endpoint that may carry credentials in its user
	// information, path or query: ProtectExcerpt keeps only its scheme and host.
	kindURL
	// kindPlain names an account or project: ProtectExcerpt leaves it visible.
	kindPlain
)

// table is the provider table, in the order a default model is chosen when
// several providers arrive at once. The crossing lists are explicit
// allowlists: a family variable missing from them is removed and never sent.
var table = []Provider{
	{
		Name: "anthropic", Prefix: "ANTHROPIC_", Key: "ANTHROPIC_API_KEY", DefaultModel: "claude-sonnet-4-6",
		settings: []setting{
			{"ANTHROPIC_API_KEY", kindSecret},
			{"ANTHROPIC_AUTH_TOKEN", kindSecret},
			{"ANTHROPIC_BASE_URL", kindURL},
			{"ANTHROPIC_CUSTOM_HEADERS", kindSecret},
		},
	},
	{
		Name: "openai", Prefix: "OPENAI_", Key: "OPENAI_API_KEY", DefaultModel: "gpt-5.4-mini",
		settings: []setting{
			{"OPENAI_API_KEY", kindSecret},
			{"OPENAI_BASE_URL", kindURL},
			{"OPENAI_ORG_ID", kindPlain},
			{"OPENAI_PROJECT_ID", kindPlain},
			{"OPENAI_CUSTOM_HEADERS", kindSecret},
		},
	},
}

// Providers returns the provider table in the order a default model is
// chosen when several providers arrive at once.
func Providers() []Provider {
	out := make([]Provider, len(table))
	for i, p := range table {
		out[i] = p
		out[i].settings = slices.Clone(p.settings)
	}
	return out
}

// Lookup returns the provider named name, and false for a name the table
// does not hold.
func Lookup(name string) (Provider, bool) {
	for _, p := range Providers() {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// InFamily reports whether name belongs to any provider's family.
func InFamily(name string) bool {
	for _, p := range table {
		if strings.HasPrefix(name, p.Prefix) {
			return true
		}
	}
	return false
}

// Split separates environ, a list of NAME=value entries, into the entries of
// every provider family and the rest, each in environ's order.
func Split(environ []string) (family, rest []string) {
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if InFamily(name) {
			family = append(family, kv)
		} else {
			rest = append(rest, kv)
		}
	}
	return family, rest
}

// Select chooses what the surla command hands over from environ, its own
// environment as NAME=value entries. For each provider whose key is not
// blank after trimming spaces, it sends the provider's request settings that
// have non-empty values, exactly as they are. Every other family variable
// with a value is named in NotSent: a request setting whose key is not set,
// with that key, and any variable that is not a request setting. A blank key
// counts as not set and is not named itself.
func Select(environ []string) Selection {
	values := map[string]string{}
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if ok && InFamily(name) {
			values[name] = value
		}
	}
	sel := Selection{Send: map[string]map[string]string{}}
	for _, p := range table {
		keySet := strings.TrimSpace(values[p.Key]) != ""
		for _, s := range p.settings {
			value := values[s.name]
			switch {
			case value == "":
			case keySet:
				if sel.Send[p.Name] == nil {
					sel.Send[p.Name] = map[string]string{}
				}
				sel.Send[p.Name][s.name] = value
			case s.name != p.Key:
				sel.NotSent = append(sel.NotSent, Unsent{Name: s.name, MissingKey: p.Key})
			}
			delete(values, s.name)
		}
	}
	for name, value := range values {
		if value != "" {
			sel.NotSent = append(sel.NotSent, Unsent{Name: name})
		}
	}
	slices.SortFunc(sel.NotSent, func(a, b Unsent) int { return cmp.Compare(a.Name, b.Name) })
	return sel
}

// New returns the request settings of the provider named provider from
// values, keyed by variable name. It refuses a provider the table does not
// hold, a name that is not one of its request settings, settings without its
// key, an empty value, one containing NUL, which no program environment can
// carry, and one longer than MaxValueLength characters, which ExcerptContext
// relies on. Every error names the provider and at most one variable, never
// a value.
func New(provider string, values map[string]string) (Settings, error) {
	p, ok := Lookup(provider)
	if !ok {
		return Settings{}, fmt.Errorf("unknown model provider %q", provider)
	}
	if _, ok := values[p.Key]; !ok {
		return Settings{}, fmt.Errorf("provider %s settings must include %s", p.Name, p.Key)
	}
	held := make(map[string]Value, len(values))
	for _, name := range slices.Sorted(maps.Keys(values)) {
		value := values[name]
		switch {
		case !p.Crosses(name):
			return Settings{}, fmt.Errorf("%s is not a request setting of provider %s", name, p.Name)
		case value == "":
			return Settings{}, fmt.Errorf("provider %s setting %s is empty", p.Name, name)
		case strings.ContainsRune(value, 0):
			return Settings{}, fmt.Errorf("provider %s setting %s contains a NUL character", p.Name, name)
		case utf8.RuneCountInString(value) > MaxValueLength:
			return Settings{}, fmt.Errorf("provider %s setting %s is longer than %d characters", p.Name, name, MaxValueLength)
		}
		held[name] = newValue(value)
	}
	return Settings{provider: p.Name, values: held}, nil
}

// ProtectExcerpt returns the excerpt text[start:] of a longer text, such as
// the end of a live target's log, with the held secret and endpoint values
// in it replaced. text holds the excerpt and, before it, up to
// ExcerptContext bytes of what preceded it in the longer text (all of it
// when there is less); it ends where the longer text ended, possibly inside
// a value its writer had not finished.
//
// In all of text it marks the bytes of every exact occurrence of every held
// secret or endpoint value, overlapping occurrences included, and the
// longest end of text that is a proper prefix of one. It then returns the
// excerpt's unmarked bytes as they are and, in place of each maximal run of
// marked bytes that reaches the excerpt, one replacement: the scheme and
// host of the endpoint when the run is exactly one occurrence of a held
// endpoint and of no secret (Redacted when no scheme and host can be read
// from it or they contain a held secret), and Redacted otherwise. Account
// and project names stay.
//
// Every occurrence that overlaps the excerpt starts at most ExcerptContext
// bytes before it, so it lies wholly inside text and is marked. Hence no
// byte of an exact occurrence of a held value, or of a held value whose
// writing stopped at the end of the longer text, is returned. The same value
// printed in another form, quoted, escaped or re-encoded, is not recognized
// and is returned as printed.
func ProtectExcerpt(text string, start int, held ...Settings) string {
	// span is the byte interval [from, to) of text one mark covers.
	type span struct{ from, to int }
	var secrets []string
	for _, s := range held {
		p, _ := Lookup(s.provider)
		for name, v := range s.values {
			if p.kind(name) == kindSecret {
				secrets = append(secrets, v.raw())
			}
		}
	}
	// depth counts, as differences, how many marks cover each byte.
	depth := make([]int, len(text)+1)
	mark := func(m span) { depth[m.from]++; depth[m.to]-- }
	endpoints := map[span]string{}
	secretSpans := map[span]bool{}
	for _, s := range held {
		p, _ := Lookup(s.provider)
		for name, v := range s.values {
			kind, raw := p.kind(name), v.raw()
			if kind == kindPlain {
				continue
			}
			for i := 0; ; {
				j := strings.Index(text[i:], raw)
				if j < 0 {
					break
				}
				m := span{i + j, i + j + len(raw)}
				mark(m)
				if kind == kindURL {
					endpoints[m] = endpoint(raw, secrets)
				} else {
					secretSpans[m] = true
				}
				i += j + 1
			}
			for k := min(len(raw)-1, len(text)); k > 0; k-- {
				if strings.HasSuffix(text, raw[:k]) {
					mark(span{len(text) - k, len(text)})
					break
				}
			}
		}
	}
	marked := make([]bool, len(text))
	for i, covering := 0, 0; i < len(text); i++ {
		covering += depth[i]
		marked[i] = covering > 0
	}
	var out strings.Builder
	for i := 0; i < len(text); {
		if !marked[i] {
			if i >= start {
				out.WriteByte(text[i])
			}
			i++
			continue
		}
		j := i
		for j < len(text) && marked[j] {
			j++
		}
		if j > start {
			replacement := Redacted
			if ep, ok := endpoints[span{i, j}]; ok && !secretSpans[span{i, j}] {
				replacement = ep
			}
			out.WriteString(replacement)
		}
		i = j
	}
	return out.String()
}

// Settings returns the names of p's request settings, in table order.
func (p Provider) Settings() []string {
	names := make([]string, len(p.settings))
	for i, s := range p.settings {
		names[i] = s.name
	}
	return names
}

// Crosses reports whether name is one of p's request settings.
func (p Provider) Crosses(name string) bool {
	return slices.ContainsFunc(p.settings, func(s setting) bool { return s.name == name })
}

// Provider returns the name of the provider whose settings s holds, or ""
// for the zero Settings.
func (s Settings) Provider() string {
	return s.provider
}

// Names returns the names of the settings s holds, sorted.
func (s Settings) Names() []string {
	return slices.Sorted(maps.Keys(s.values))
}

// Environ returns the settings s holds as NAME=value entries sorted by name,
// for the environment of a program permitted to receive them. It is the one
// place besides ProtectExcerpt where a held value is read.
func (s Settings) Environ() []string {
	env := make([]string, 0, len(s.values))
	for _, name := range s.Names() {
		env = append(env, name+"="+s.values[name].raw())
	}
	return env
}

// String returns Redacted.
func (Value) String() string { return Redacted }

// GoString returns Redacted, so %#v shows no value.
func (Value) GoString() string { return Redacted }

// Format writes Redacted for every verb and flag. The write goes to the
// print call's own buffer, which cannot fail.
func (Value) Format(f fmt.State, _ rune) { io.WriteString(f, Redacted) }

// LogValue returns Redacted, so structured logs show no value.
func (Value) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON encodes Redacted as a JSON string.
func (Value) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }

// MarshalText returns Redacted.
func (Value) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// kind returns how ProtectExcerpt treats the setting name of p; a name p does not
// list is plain.
func (p Provider) kind(name string) settingKind {
	for _, s := range p.settings {
		if s.name == name {
			return s.kind
		}
	}
	return kindPlain
}

// newValue wraps raw as a Value.
func newValue(raw string) Value {
	return Value{raw: func() string { return raw }}
}

// endpoint returns the scheme and host of the URL raw, or Redacted when raw
// has no absolute scheme and host or they contain one of secrets. It never
// returns raw itself or a parser error, which could quote it.
func endpoint(raw string, secrets []string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return Redacted
	}
	safe := u.Scheme + "://" + u.Host
	for _, secret := range secrets {
		if strings.Contains(safe, secret) {
			return Redacted
		}
	}
	return safe
}
