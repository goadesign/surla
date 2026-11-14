// This file hands the shell's model provider request settings to the project
// server and tells the user, by variable name only, what the server holds.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/modelsettings"
	"github.com/goadesign/surla/internal/project"
)

// handOverModelSettings sends sel, what the surla command selected from its
// own environment, to the project server that answered for state, through
// the authenticated management connection, then writes to stdout the line
// naming what the server holds (modelSettingsLine). A provider sel does not
// send keeps what the server holds for it. Errors carry fixed wording and
// never a value.
func handOverModelSettings(ctx context.Context, state *project.State, sel modelsettings.Selection, stdout io.Writer) error {
	payload := &gensurla.HandOverModelSettingsPayload{Anthropic: sel.Send["anthropic"], Openai: sel.Send["openai"]}
	raw, err := modelSettingsEndpoint(state)(ctx, payload)
	if err != nil {
		return err
	}
	result, ok := raw.(*gensurla.ModelSettingsResult)
	if !ok {
		return errors.New("project server returned no model settings result; run surla again")
	}
	line := modelSettingsLine(sel, result)
	if line == "" {
		return nil
	}
	_, err = fmt.Fprintln(stdout, line)
	return err
}

// modelSettingsLine is the one line the surla command prints after a
// hand-over, or "" when the server holds nothing and nothing was left
// unsent. It names variables only, never a value, because a value such as a
// base URL can carry credentials. Each provider's held names say whether
// they came from this shell or an earlier surla; family variables left
// unsent are named, with the missing key when that is the reason; then the
// model live targets use, and how to forget the settings.
//
// For example: "Model settings held by the project server:
// ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL (from this shell); not sent:
// ANTHROPIC_ADMIN_KEY. Live targets use anthropic/claude-sonnet-4-6; change
// it on the views page. surla stop makes the project server forget them."
func modelSettingsLine(sel modelsettings.Selection, result *gensurla.ModelSettingsResult) string {
	type group struct {
		names  []string
		source string
	}
	var held []group
	for _, p := range modelsettings.Providers() {
		names := result.Held[p.Name]
		if len(names) == 0 {
			continue
		}
		source := "from an earlier surla"
		if _, sent := sel.Send[p.Name]; sent {
			source = "from this shell"
		}
		if last := len(held) - 1; last >= 0 && held[last].source == source {
			held[last].names = append(held[last].names, names...)
			continue
		}
		held = append(held, group{names: slices.Clone(names), source: source})
	}
	var unsent []string
	for _, p := range modelsettings.Providers() {
		var names []string
		for _, u := range sel.NotSent {
			if u.MissingKey == p.Key {
				names = append(names, u.Name)
			}
		}
		if len(names) > 0 {
			unsent = append(unsent, strings.Join(names, ", ")+" ("+p.Key+" is not set)")
		}
	}
	for _, u := range sel.NotSent {
		if u.MissingKey == "" {
			unsent = append(unsent, u.Name)
		}
	}
	if len(held) == 0 && len(unsent) == 0 {
		return ""
	}

	var line strings.Builder
	if len(held) == 0 {
		line.WriteString("No model settings are held by the project server")
	} else {
		parts := make([]string, len(held))
		for i, g := range held {
			parts[i] = strings.Join(g.names, ", ") + " (" + g.source + ")"
		}
		line.WriteString("Model settings held by the project server: " + strings.Join(parts, ", "))
	}
	if len(unsent) > 0 {
		line.WriteString("; not sent: " + strings.Join(unsent, ", "))
	}
	line.WriteString(". " + liveModelSentence(result.Connection))
	if len(held) > 0 {
		line.WriteString(" surla stop makes the project server forget them.")
	}
	return line.String()
}

// liveModelSentence says which model live targets use and where to change it.
// The model name and gateway address come from the server, so only their
// printable text is shown.
func liveModelSentence(choice gensurla.Connection) string {
	if direct, ok := choice.AsDirect(); ok {
		return "Live targets use " + printableTerminalText(direct.Provider+"/"+direct.ModelName) + "; change it on the views page."
	}
	if gateway, ok := choice.AsGateway(); ok {
		return "Live targets use the model gateway at " + printableTerminalText(gateway.GatewayAddr) + "; change it on the views page."
	}
	return "Live targets have no model: export a provider key in a shell and run surla there, or choose a model gateway on the views page."
}
