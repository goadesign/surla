// This file binds typed read-only Herdr streams and Chorus views at the executable boundary.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/goadesign/surla/internal/project"
)

// teamViewPrefixMaxBytes bounds the temporary parsing/agent allocation for one
// watcher prefix. The generated Chorus response orders swarm then agents before
// conversation data; 256 KiB allows ample agent metadata without consuming messages.
// An oversized or incomplete prefix is unknown, never an empty watcher set.
const teamViewPrefixMaxBytes = 256 << 10

type (
	// teamObservations keeps only clients and program bindings, never outside snapshots.
	teamObservations struct {
		commands  teamCommands
		stream    func(context.Context, []string, string, []string, func(io.Reader) error) error
		client    *http.Client
		chorusURL string
	}
	// teamLabels decodes only the four labels needed for ownership and placement.
	// Launcher options and arbitrary terminal labels never enter retained state.
	teamLabels struct {
		Project string `json:"chorus.project"`
		Space   string `json:"herdr.space"`
		Handle  string `json:"chorus.handle"`
		Tab     string `json:"herdr.tab"`
	}
	// teamTerminalFacts is the external Herdr projection, excluding all free text.
	teamTerminalFacts struct {
		Address    string                  `json:"address"`
		State      string                  `json:"state"`
		Labels     teamLabels              `json:"labels"`
		Foreground *project.TeamForeground `json:"foreground"`
		Reported   bool                    `json:"reported"`
		Status     *project.TeamStatus     `json:"status"`
		CreatedMS  int64                   `json:"created_ms"`
		Reason     string                  `json:"reason"`
	}
	// teamStreamLine intentionally omits screen, title, status.msg, argv and all work text.
	teamStreamLine struct {
		Sequence  uint64               `json:"seq"`
		Terminals *[]teamTerminalFacts `json:"terminals"`
		Method    string               `json:"method"`
		Params    struct {
			Sequence uint64 `json:"seq"`
			Terminal string `json:"terminal"`
			Type     string `json:"type"`
			Data     struct {
				Labels *teamLabels `json:"labels"`
			} `json:"data"`
		} `json:"params"`
	}
)

// FindTerminals applies all ownership labels before every fact read.
func (o teamObservations) FindTerminals(ctx context.Context, selection project.TeamSelection) ([]project.TeamTerminal, error) {
	handles := make([]string, 0, len(selection.Roles))
	for _, role := range selection.Roles {
		handles = append(handles, role.Handle)
	}
	return o.commands.Terminals(ctx, selection.Root, selection.Space, handles)
}

// ReadTerminal loads only narrow facts for an identifier the owner already established.
func (o teamObservations) ReadTerminal(ctx context.Context, address string) (project.TeamTerminal, error) {
	var result teamTerminalFacts
	err := o.commands.json(ctx, o.commands.core, &result, "get", address, "--json")
	if err == nil && result.Address != address {
		err = errors.New("Herdr returned a different terminal")
	}
	return result.terminal(), err
}

// Follow subscribes separately per registered handle because Herdr label filters
// are conjunctions. Each subscription includes both project root and saved space.
func (o teamObservations) Follow(ctx context.Context, selection project.TeamSelection, emit func(project.TeamEvent)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	results := make(chan error, len(selection.Roles))
	for _, role := range selection.Roles {
		workers.Go(func() {
			args := []string{"events", "--json", "--label", "chorus.project=" + selection.Root, "--label", "herdr.space=" + selection.Space, "--label", "chorus.handle=" + role.Handle}
			err := o.stream(ctx, o.commands.herdrEnv, o.commands.core, args, func(reader io.Reader) error { return readTeamEvents(reader, role.Role, emit) })
			results <- err
		})
	}
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-results:
	}
	cancel()
	workers.Wait()
	if err == nil {
		err = errors.New("Herdr observation stream ended")
	}
	return err
}

// readTeamEvents decodes only typed selection/state metadata, ignoring work-bearing fields.
// Unknown event shapes reconnect through a fresh snapshot instead of reading an unknown pane.
func readTeamEvents(reader io.Reader, role string, emit func(project.TeamEvent)) error {
	decoder := json.NewDecoder(reader)
	for {
		var line teamStreamLine
		if err := decoder.Decode(&line); err != nil {
			return errors.New("Herdr event reader ended or returned invalid JSON")
		}
		if line.Terminals != nil {
			terminals := make([]project.TeamTerminal, 0, len(*line.Terminals))
			for _, facts := range *line.Terminals {
				terminals = append(terminals, facts.terminal())
			}
			emit(project.TeamEvent{Kind: "snapshot", Role: role, Sequence: line.Sequence, Terminals: terminals})
			continue
		}
		switch line.Method {
		case "heartbeat":
			emit(project.TeamEvent{Kind: "heartbeat", Role: role, Sequence: line.Params.Sequence})
		case "missed":
			emit(project.TeamEvent{Kind: "missed", Role: role})
		case "event":
			switch line.Params.Type {
			case "title", "size", "doc", "keep":
				continue
			case "created", "restarted":
				if line.Params.Data.Labels == nil {
					return errors.New("Herdr membership requires a fresh snapshot")
				}
			case "labels", "lost", "removed", "finished", "status", "turn":
			default:
				continue
			}
			var labels map[string]string
			if line.Params.Data.Labels != nil {
				labels = line.Params.Data.Labels.values()
			}
			emit(project.TeamEvent{Kind: "event", Role: role, Sequence: line.Params.Sequence, Terminal: line.Params.Terminal, Type: line.Params.Type, Labels: labels})
		}
	}
}

// terminal drops unused foreground arguments rather than retaining their backing array.
// The shared shell predicate needs only argv[0] and its first two arguments.
func (facts teamTerminalFacts) terminal() project.TeamTerminal {
	var foreground *project.TeamForeground
	if facts.Foreground != nil {
		foreground = &project.TeamForeground{Argv: slices.Clone(facts.Foreground.Argv[:min(len(facts.Foreground.Argv), 3)])}
	}
	return project.TeamTerminal{Address: facts.Address, State: facts.State, Labels: facts.Labels.values(), Foreground: foreground, Reported: facts.Reported, Status: facts.Status, CreatedMS: facts.CreatedMS, Reason: facts.Reason}
}

// values materializes only the ownership and tab facts consumed by the project owner.
func (labels teamLabels) values() map[string]string {
	return map[string]string{"chorus.project": labels.Project, "herdr.space": labels.Space, "chorus.handle": labels.Handle, "herdr.tab": labels.Tab}
}

// Surla itself runs in a Core terminal: a deliberate Core stop ends the server
// through HUP. Native clients may recover Core after a crash/update; supervision
// therefore uses ordinary reads without a precheck and issues no explicit start command.
// runTeamStream closes stdin, never reads screen output, and drains the owned
// read-only subscriber process on cancellation or decode failure. The process
// runs with env, Herdr's own environment.
func runTeamStream(ctx context.Context, env []string, program string, args []string, read func(io.Reader) error) error {
	resolved, err := exec.LookPath(program)
	if err != nil {
		return errors.New("Herdr event reader is unavailable")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(ctx, resolved, args...)
	command.Env = env
	command.WaitDelay = time.Second
	output, err := command.StdoutPipe()
	if err != nil {
		return errors.New("Herdr event reader could not be opened")
	}
	if err := command.Start(); err != nil {
		return errors.New("Herdr event reader could not start")
	}
	readErr := read(output)
	cancel()
	waitErr := command.Wait()
	if readErr != nil || waitErr != nil {
		return errors.New("Herdr event reader could not continue")
	}
	return nil
}

// ReadWatchers uses Chorus's existing local-only conditional view. Only registered
// handle/watcher facts survive decoding; channels, inboxes and messages have no fields here.
func (o teamObservations) ReadWatchers(ctx context.Context, team, version string, roles []project.TeamRole) (project.TeamWatcherView, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, o.chorusURL+"/swarms/"+url.PathEscape(team)+"/view?wait=60", nil)
	if err != nil {
		return project.TeamWatcherView{}, errors.New("Chorus view request could not be created")
	}
	if version != "" {
		request.Header.Set("If-None-Match", version)
	}
	response, err := o.client.Do(request)
	if err != nil {
		return project.TeamWatcherView{}, errors.New("Chorus view is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified && version != "" {
		return project.TeamWatcherView{Unchanged: true, Version: version}, nil
	}
	if response.StatusCode != http.StatusOK {
		return project.TeamWatcherView{}, errors.New("Chorus view was refused")
	}
	held, err := readTeamWatcherPrefix(response.Body, team, roles)
	version = response.Header.Get("ETag")
	if err != nil || version == "" {
		return project.TeamWatcherView{}, errors.New("Chorus view returned an unavailable watcher prefix")
	}
	return project.TeamWatcherView{Version: version, Held: held}, nil
}

// watcherPrefixReader prevents json.Decoder's read-ahead from consuming even
// the first byte after the agents array. HTTP transport buffering is not retained.
type watcherPrefixReader struct{ source io.Reader }

// Read gives the JSON tokenizer one byte at a time, within the prefix byte bound.
func (r watcherPrefixReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return r.source.Read(p[:1])
}

// readTeamWatcherPrefix consumes only the owning generated response's ordered
// swarm/agents prefix, closes before channels/inboxes, and retains registered
// watcher facts only. A changed key order is unavailable evidence, not absence.
func readTeamWatcherPrefix(reader io.Reader, team string, roles []project.TeamRole) (map[string]bool, error) {
	decoder := json.NewDecoder(watcherPrefixReader{source: io.LimitReader(reader, teamViewPrefixMaxBytes)})
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("invalid Chorus watcher prefix")
	}
	token, err = decoder.Token()
	if err != nil || token != "swarm" {
		return nil, errors.New("invalid Chorus watcher prefix order")
	}
	var swarm string
	if err := decoder.Decode(&swarm); err != nil || swarm != team {
		return nil, errors.New("invalid Chorus watcher prefix owner")
	}
	token, err = decoder.Token()
	if err != nil || token != "agents" {
		return nil, errors.New("invalid Chorus watcher prefix order")
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, errors.New("invalid Chorus watcher agents")
	}
	held := make(map[string]bool, len(roles))
	for decoder.More() {
		var agent struct {
			Handle   *string `json:"handle"`
			Watching *bool   `json:"watching_on_this_server"`
		}
		if err := decoder.Decode(&agent); err != nil || agent.Handle == nil || agent.Watching == nil {
			return nil, errors.New("incomplete Chorus watcher agents")
		}
		for _, role := range roles {
			if *agent.Handle == role.Handle {
				held[role.Handle] = *agent.Watching
			}
		}
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim(']') {
		return nil, errors.New("incomplete Chorus watcher prefix")
	}
	return held, nil
}

// teamCommandExit preserves only a bounded exit code, never stderr or arguments.
type teamCommandExit struct{ code int }

func (e teamCommandExit) Error() string { return "team command failed" }
func (e teamCommandExit) ExitCode() int { return e.code }
