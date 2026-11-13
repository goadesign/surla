// This file owns the repeatable team-start operation and the Chorus roster contract.
package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	genproject "github.com/goadesign/surla/gen/project"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	goa "goa.design/goa/v3/pkg"
)

type (
	// TeamHost supplies typed Herdr effects; the executable resolves programs and configuration.
	TeamHost interface {
		Spaces(context.Context) ([]TeamSpace, error)
		CreateSpace(context.Context, string, string) (TeamSpace, error)
		Terminals(context.Context, string, string) ([]TeamTerminal, error)
		Focus(context.Context, TeamSpace) error
		Overlays(context.Context) ([]string, error)
		OpenLauncher(context.Context) error
	}
	// TeamDependencies binds every external team effect and the server's accepted-work lifetime.
	TeamDependencies struct {
		// Lifetime ends accepted work only when the owning server shuts down.
		Lifetime context.Context
		// Host is the root-injected Herdr command adapter.
		Host TeamHost
		// RosterDirectory is Herdr's local launcher roster directory.
		RosterDirectory string
		// Shell is the server login shell used by the shared running predicate.
		Shell string
	}
	// TeamSpace is a local space and its tab-to-terminal mapping reported by Herdr.
	TeamSpace struct {
		ID     string    `json:"id"`
		Name   string    `json:"name"`
		Folder string    `json:"folder"`
		Tabs   []TeamTab `json:"tabs"`
	}
	// TeamTab identifies the terminals belonging to a Herdr tab.
	TeamTab struct {
		ID        string     `json:"id"`
		Terminals []TeamPane `json:"terminals"`
	}
	// TeamPane supplies the terminal identifier accepted by pane.focus.
	TeamPane struct {
		Terminal string `json:"terminal"`
	}
	// TeamTerminal contains only the trusted observations needed to identify a running role.
	TeamTerminal struct {
		State      string            `json:"state"`
		Labels     map[string]string `json:"labels"`
		Foreground *TeamForeground   `json:"foreground"`
	}
	// TeamForeground preserves argv rather than Herdr's display name (which may be a version).
	TeamForeground struct {
		Argv []string `json:"argv"`
	}
	// teamOperation serializes accepted work without tying its lifetime to the HTTP caller.
	teamOperation struct {
		sync.Mutex
		dependencies TeamDependencies
	}
	// launcherRoster mirrors Chorus load_roster's exact closed schema.
	launcherRoster struct {
		Root  string         `json:"root"`
		Swarm string         `json:"swarm"`
		Roles []launcherRole `json:"roles"`
	}
	// launcherRole binds one trusted role to its existing folder and private credential path.
	launcherRole struct {
		Role         string               `json:"role"`
		Title        string               `json:"title"`
		Handle       string               `json:"handle"`
		Folder       string               `json:"folder"`
		FirstMessage string               `json:"first_message"`
		Connections  []launcherConnection `json:"connections"`
		Place        *launcherPlace       `json:"place"`
	}
	// launcherConnection contains a file path, never credential bytes.
	launcherConnection struct {
		Name           string `json:"name"`
		URL            string `json:"url"`
		CredentialFile string `json:"credential_file"`
	}
	// launcherPlace places each worker relative to an earlier role.
	launcherPlace struct {
		Beside    string `json:"beside"`
		Direction string `json:"direction"`
	}
)

// EnsureTeam accepts management authority once, then completes bounded server-owned work.
// A disconnected caller can repeat the operation; saved identity and space adoption avoid duplication.
func (s *Server) EnsureTeam(ctx context.Context) (*genproject.ProjectTeam, error) {
	if err := managementOnly(ctx, "ensure_team"); err != nil {
		return nil, err
	}
	// Retain request values and tracing while the server, rather than the client,
	// owns cancellation of accepted work.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	stopLifetime := context.AfterFunc(s.team.dependencies.Lifetime, cancel)
	defer stopLifetime()
	if s.team.dependencies.Lifetime.Err() != nil {
		cancel()
	}
	ctx, span := otel.Tracer("surla/project").Start(ctx, "surla.project.ensure_team")
	defer span.End()
	result, err := s.ensureTeam(ctx)
	if err != nil {
		var serviceErr *goa.ServiceError
		if errors.As(err, &serviceErr) && serviceErr.Name == "team_unavailable" {
			span.RecordError(err)
			span.SetStatus(codes.Error, "team start failed")
		}
	}
	return result, err
}

// ensureTeam holds the operation lock across the complete space/roster/dialog transition.
func (s *Server) ensureTeam(ctx context.Context) (*genproject.ProjectTeam, error) {
	s.team.Lock()
	defer s.team.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, genproject.MakeTeamUnavailable(err)
	}
	host := s.team.dependencies.Host
	s.state.mu.Lock()
	if s.state.Record.Team == "" {
		s.state.Record.Team = teamName(filepath.Base(s.state.Record.Directory), s.state.Record.ID)
		if err := s.state.save(); err != nil {
			s.state.Record.Team = ""
			s.state.mu.Unlock()
			return nil, genproject.MakeTeamUnavailable(err)
		}
	}
	record := s.state.Record
	s.state.mu.Unlock()
	spaces, err := host.Spaces(ctx)
	if err != nil {
		return nil, genproject.MakeTeamUnavailable(err)
	}
	var space TeamSpace
	matches := 0
	for _, candidate := range spaces {
		if candidate.ID == record.Space && record.Space != "" {
			space = candidate
			break
		}
	}
	if space.ID == "" {
		for _, candidate := range spaces {
			if candidate.Name == record.Team && candidate.Folder == record.Directory {
				space = candidate
				matches++
			}
		}
		if matches > 1 {
			return nil, &genproject.TeamSpaceConflict{Name: "ambiguous_space", Count: matches}
		}
		if matches == 0 {
			space, err = host.CreateSpace(ctx, record.Team, record.Directory)
			if err != nil {
				return nil, genproject.MakeTeamUnavailable(err)
			}
		}
	}
	if !safeSpaceID(space.ID) {
		return nil, genproject.MakeTeamUnavailable(errors.New("invalid local space identifier"))
	}
	// Retain the old space identity until its roster is removed. A lost creation reply
	// or failed save is recovered by deterministic name/folder adoption on the next call.
	if record.Space != "" && record.Space != space.ID {
		err = os.Remove(filepath.Join(s.team.dependencies.RosterDirectory, record.Space+".json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, genproject.MakeTeamUnavailable(err)
		}
	}
	s.state.mu.Lock()
	s.state.Record.Space = space.ID
	err = s.state.save()
	if err != nil {
		s.state.Record.Space = record.Space
	}
	s.state.mu.Unlock()
	if err != nil {
		return nil, genproject.MakeTeamUnavailable(err)
	}
	handle := teamName("guide", record.ID)
	s.state.mu.Lock()
	role, exists := s.state.Record.Agents[handle]
	s.state.mu.Unlock()
	if exists && role != "guide" {
		return nil, genproject.MakeTeamUnavailable(errors.New("saved guide handle has a different role"))
	}
	if !exists {
		_, err := s.addAgent(&genproject.AddAgentPayload{ActorID: handle, Role: "guide"})
		if err != nil {
			return nil, genproject.MakeTeamUnavailable(err)
		}
	}
	roster, err := s.teamRoster([]launcherRole{{Role: "guide", Title: "Guide", Handle: handle, Folder: record.Directory, FirstMessage: "Work through the Story and the change with the user in this project's pane. Use your surla connection, starting with the surla://golden-path guidance resource and list_changes/get_change. Only the user confirms intent and approves results, on the views page."}})
	if err != nil {
		return nil, genproject.MakeTeamUnavailable(err)
	}
	if err := os.MkdirAll(s.team.dependencies.RosterDirectory, 0o700); err != nil {
		return nil, genproject.MakeTeamUnavailable(err)
	}
	if err := writeJSON(filepath.Join(s.team.dependencies.RosterDirectory, space.ID+".json"), roster); err != nil {
		return nil, genproject.MakeTeamUnavailable(err)
	}
	terminals, err := host.Terminals(ctx, record.Directory, space.ID)
	if err != nil {
		return nil, genproject.MakeTeamUnavailable(err)
	}
	running := false
	for _, terminal := range terminals {
		if terminal.Labels["chorus.handle"] != handle || terminal.Labels["chorus.project"] != record.Directory || terminal.Labels["herdr.space"] != space.ID {
			continue
		}
		for _, tab := range space.Tabs {
			if terminal.Labels["herdr.tab"] == tab.ID && roleRunning(terminal, s.team.dependencies.Shell) {
				running = true
			}
		}
	}
	result := &genproject.ProjectTeam{Team: record.Team, Space: space.ID, Dialog: "not_needed", Roles: []*genproject.ProjectTeamRole{{Role: "guide", Handle: handle, Running: running}}}
	if running {
		return result, nil
	}
	if err := host.Focus(ctx, space); err != nil {
		return nil, genproject.MakeTeamUnavailable(err)
	}
	overlays, err := host.Overlays(ctx)
	if err != nil {
		return nil, genproject.MakeTeamUnavailable(err)
	}
	for _, overlay := range overlays {
		if overlay == "chorus-launcher" {
			result.Dialog = "already_open"
			return result, nil
		}
	}
	if len(overlays) > 0 {
		return nil, genproject.MakeWindowBusy(errors.New("close the open Herdr dialog and run surla again"))
	}
	if err := host.OpenLauncher(ctx); err != nil {
		return nil, genproject.MakeTeamUnavailable(err)
	}
	result.Dialog = "opened"
	return result, nil
}

// teamRoster validates folders and credentials before constructing the complete closed launcher schema.
func (s *Server) teamRoster(roles []launcherRole) (launcherRoster, error) {
	roster := launcherRoster{Root: s.state.Record.Directory, Swarm: s.state.Record.Team, Roles: roles}
	for i := range roster.Roles {
		role := &roster.Roles[i]
		info, err := os.Stat(role.Folder)
		if err != nil {
			return launcherRoster{}, err
		}
		if !filepath.IsAbs(role.Folder) || !info.IsDir() {
			return launcherRoster{}, errors.New("role folder must be an existing absolute directory")
		}
		if _, err := readCredential(s.state.AgentFile(role.Handle)); err != nil {
			return launcherRoster{}, err
		}
		role.Connections = []launcherConnection{{Name: "surla", URL: s.state.Origin() + "/mcp", CredentialFile: s.state.AgentFile(role.Handle)}}
		if i > 0 {
			direction := "down"
			if i == 1 {
				direction = "right"
			}
			role.Place = &launcherPlace{Beside: roster.Roles[i-1].Handle, Direction: direction}
		}
	}
	return roster, nil
}

// teamName uses the same project suffix for team and handles within Chorus's 32-character limit.
func teamName(prefix, id string) string {
	var clean strings.Builder
	for _, c := range prefix {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			clean.WriteRune(c)
		}
	}
	base := strings.TrimLeft(clean.String(), "_-")
	if base == "" {
		base = "project"
	}
	suffix := id[:12]
	return base[:min(len(base), 19)] + "-" + suffix
}

// safeSpaceID restricts roster filenames to local identifiers accepted by Chorus.
func safeSpaceID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00")
}

// roleRunning shares Contract 1b with the Chorus launcher: a live terminal with
// unreported foreground is conservatively running; reported argv is running
// unless it is an idle known/custom shell. A shell's first two arguments containing -c
// or a single-dash flag group containing c indicate an active command wrapper.
func roleRunning(t TeamTerminal, shell string) bool {
	if t.State != "live" {
		return false
	}
	if t.Foreground == nil {
		return true
	}
	if len(t.Foreground.Argv) == 0 {
		return false
	}
	argv := t.Foreground.Argv
	name := strings.TrimLeft(filepath.Base(argv[0]), "-")
	idleShell := name == filepath.Base(shell) && shell != ""
	for _, candidate := range []string{"sh", "bash", "zsh", "fish", "dash", "ksh", "csh", "tcsh", "nu", "pwsh", "xonsh", "elvish"} {
		if name == candidate {
			idleShell = true
		}
	}
	if !idleShell {
		return true
	}
	for _, arg := range argv[1:min(len(argv), 3)] {
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg[1:], "c") {
			return true
		}
	}
	return false
}

// validTeamName enforces the persisted launcher identifier contract at the storage boundary.
func validTeamName(value string) bool {
	if len(value) == 0 || len(value) > 32 {
		return false
	}
	for i, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if i > 0 && (c == '_' || c == '-') {
			continue
		}
		return false
	}
	return true
}
