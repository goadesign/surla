// This file owns the repeatable team-start operation and the Chorus roster contract.
package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	genproject "github.com/goadesign/surla/gen/project"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type (
	// TeamHost supplies typed Herdr effects; the executable resolves programs and configuration.
	TeamHost interface {
		Spaces(context.Context) ([]TeamSpace, error)
		CreateSpace(context.Context, string, string) (TeamSpace, error)
		Terminals(context.Context, string, string, []string) ([]TeamTerminal, error)
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
		// Observer follows only the registered selection. Nil refuses all observations.
		Observer TeamObserver
		// Sender is explicitly injected; nil refuses typing. Production remains unbound
		// until an agent receiver proves exact, untagged reminder delivery.
		Sender TeamSender
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
		Address    string            `json:"address"`
		State      string            `json:"state"`
		Labels     map[string]string `json:"labels"`
		Foreground *TeamForeground   `json:"foreground"`
		Reported   bool              `json:"reported"`
		Status     *TeamStatus       `json:"status"`
		CreatedMS  int64             `json:"created_ms"`
		Reason     string            `json:"reason"`
	}
	// TeamForeground preserves argv rather than Herdr's display name (which may be a version).
	TeamForeground struct {
		Argv []string `json:"argv"`
	}
	// teamOperation serializes accepted work without tying its lifetime to the HTTP caller.
	teamOperation struct {
		sync.Mutex
		dependencies TeamDependencies
		// withTimeout privately injects the accepted-operation deadline; nil uses context.WithTimeout.
		withTimeout func(context.Context, time.Duration) (context.Context, context.CancelFunc)
		// observationMu protects only the small public snapshot, never external replies.
		observationMu sync.RWMutex
		observed      map[string]roleObservation
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

// teamStartTimeout bounds one accepted roster/window transition so an unresponsive
// Herdr command cannot hold the serialized team operation indefinitely. This budget
// is independent of a build's working-copy and generation phase.
const teamStartTimeout = 45 * time.Second

// errRoleFolder marks a roster failure caused by a role's working folder rather
// than the project's private state, so the team failure names that folder's step.
var errRoleFolder = errors.New("role working folder cannot be used")

// EnsureTeam accepts management authority once, then completes bounded server-owned work.
// A disconnected caller can repeat the operation; saved identity and space adoption avoid duplication.
func (s *Server) EnsureTeam(ctx context.Context) (*genproject.ProjectTeam, error) {
	if err := managementOnly(ctx, "ensure_team"); err != nil {
		return nil, err
	}
	ctx, cancel := s.acceptedTeamContext(ctx)
	defer cancel()
	ctx, span := otel.Tracer("surla/project").Start(ctx, "surla.project.ensure_team")
	defer span.End()
	result, err := s.ensureTeam(ctx)
	if result != nil {
		running := 0
		for _, role := range result.Roles {
			if role.Running {
				running++
			}
		}
		span.SetAttributes(attribute.String("team.dialog", result.Dialog), attribute.Int("team.roles", len(result.Roles)), attribute.Int("team.roles_running", running))
	} else {
		var named interface{ GoaErrorName() string }
		if errors.As(err, &named) {
			span.SetAttributes(attribute.String("team.outcome", named.GoaErrorName()))
		}
	}
	return result, err
}

// ensureTeam holds the operation lock across the complete space/roster/dialog transition.
func (s *Server) ensureTeam(ctx context.Context) (result *genproject.ProjectTeam, err error) {
	s.team.Lock()
	defer s.team.Unlock()
	defer func() {
		if result != nil {
			result.Step = teamNextStep(result.Dialog)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, teamUnavailable(ctx, "lifetime", "stopping")
	}
	host := s.team.dependencies.Host
	// Observe dialogs before any space creation or focus change: opening the launcher is a toggle.
	overlays, err := host.Overlays(ctx)
	if err != nil {
		return nil, teamUnavailable(ctx, "overlays", "herdr")
	}
	launcherOpen := false
	for _, overlay := range overlays {
		if overlay == "chorus-launcher" {
			launcherOpen = true
		} else {
			return nil, genproject.MakeWindowBusy(errors.New("close the open Herdr dialog and run surla again"))
		}
	}
	s.state.mu.Lock()
	record := s.state.Record
	s.state.mu.Unlock()
	team := record.Team
	if team == "" {
		team = teamName(filepath.Base(record.Directory), record.ID)
	}
	spaces, err := host.Spaces(ctx)
	if err != nil {
		return nil, teamUnavailable(ctx, "spaces", "herdr")
	}
	var space TeamSpace
	for _, candidate := range spaces {
		if candidate.ID == record.Space && record.Space != "" {
			space = candidate
			break
		}
	}
	if space.ID == "" {
		matches := 0
		for _, candidate := range spaces {
			if candidate.Name == team && candidate.Folder == record.Directory {
				space = candidate
				matches++
			}
		}
		if matches > 1 {
			return nil, &genproject.TeamSpaceConflict{Name: "ambiguous_space", Count: matches}
		}
		if matches == 0 {
			// An unrelated open launcher cannot identify this project's missing space.
			// Refuse without saving identity, admitting an agent, or changing the window.
			if len(overlays) > 0 {
				return nil, genproject.MakeWindowBusy(errors.New("close the open Herdr dialog and run surla again"))
			}
			space, err = host.CreateSpace(ctx, team, record.Directory)
			if err != nil {
				return nil, teamUnavailable(ctx, "create_space", "herdr")
			}
		}
	}
	if !safeSpaceID(space.ID) {
		return nil, teamUnavailable(ctx, "space_identity", "herdr")
	}
	// Keep the old identity until its roster is removed; deterministic adoption recovers a lost reply.
	if record.Space != "" && record.Space != space.ID {
		err = os.Remove(filepath.Join(s.team.dependencies.RosterDirectory, record.Space+".json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, teamUnavailable(ctx, "remove_roster", "roster_folder")
		}
	}
	s.state.mu.Lock()
	s.state.Record.Team = team
	s.state.Record.Space = space.ID
	err = s.state.save()
	if err != nil {
		s.state.Record.Team = record.Team
		s.state.Record.Space = record.Space
	}
	s.state.mu.Unlock()
	if err != nil {
		return nil, teamUnavailable(ctx, "save_team", "project_state")
	}
	if _, err := s.registerAgent(teamName("guide", record.ID), agentRecord{Role: "guide", Folder: record.Directory}); err != nil {
		return nil, teamUnavailable(ctx, "register_guide", "project_state")
	}
	// Snapshot after admission: every registered actor survives this operation.
	s.state.mu.Lock()
	agents := make(map[string]agentRecord, len(s.state.Record.Agents))
	for actor, agent := range s.state.Record.Agents {
		agents[actor] = agent
	}
	s.state.mu.Unlock()
	roles := registeredTeamRoles(agents)
	roster, err := s.teamRoster(roles)
	if errors.Is(err, errRoleFolder) {
		return nil, teamUnavailable(ctx, "roster", "role_folder")
	}
	if err != nil {
		return nil, teamUnavailable(ctx, "roster", "project_state")
	}
	if err := os.MkdirAll(s.team.dependencies.RosterDirectory, 0o700); err != nil {
		return nil, teamUnavailable(ctx, "roster_directory", "roster_folder")
	}
	if err := writeJSON(filepath.Join(s.team.dependencies.RosterDirectory, space.ID+".json"), roster); err != nil {
		return nil, teamUnavailable(ctx, "write_roster", "roster_folder")
	}
	handles := make([]string, 0, len(agents))
	for handle := range agents {
		handles = append(handles, handle)
	}
	terminals, err := host.Terminals(ctx, record.Directory, space.ID, handles)
	if err != nil {
		return nil, teamUnavailable(ctx, "terminals", "herdr")
	}
	runningHandles := map[string]bool{}
	for _, terminal := range terminals {
		if terminal.Labels["chorus.project"] != record.Directory || terminal.Labels["herdr.space"] != space.ID || !roleRunning(terminal, s.team.dependencies.Shell) {
			continue
		}
		for _, tab := range space.Tabs {
			if terminal.Labels["herdr.tab"] == tab.ID {
				runningHandles[terminal.Labels["chorus.handle"]] = true
			}
		}
	}
	listed := map[string]bool{}
	for _, role := range roster.Roles {
		listed[role.Handle] = true
	}
	result = &genproject.ProjectTeam{Team: team, Space: space.ID, Dialog: "not_needed", Roles: make([]*genproject.ProjectTeamRole, 0, len(roles))}
	missing := false
	for _, role := range roles {
		running := runningHandles[role.Handle]
		result.Roles = append(result.Roles, &genproject.ProjectTeamRole{Role: agents[role.Handle].Role, Handle: role.Handle, Running: running})
		if listed[role.Handle] && !running {
			missing = true
		}
	}
	if launcherOpen {
		result.Dialog = "already_open"
		return result, nil
	}
	if !missing {
		return result, nil
	}
	if err := host.Focus(ctx, space); err != nil {
		return nil, teamUnavailable(ctx, "focus", "herdr")
	}
	if err := host.OpenLauncher(ctx); err != nil {
		return nil, teamUnavailable(ctx, "launcher", "herdr")
	}
	result.Dialog = "opened"
	return result, nil
}

// registeredTeamRoles derives current launcher text from each saved role and folder.
func registeredTeamRoles(agents map[string]agentRecord) []launcherRole {
	byRole := make(map[string]string, len(agents))
	for actor, agent := range agents {
		byRole[agent.Role] = actor
	}
	roles := make([]launcherRole, 0, len(agents))
	for _, role := range []string{"guide", "builder", "test_writer"} {
		handle, exists := byRole[role]
		if !exists {
			continue
		}
		title, message := roleFirstMessage(role)
		roles = append(roles, launcherRole{Role: role, Title: title, Handle: handle, Folder: agents[handle].Folder, FirstMessage: message})
	}
	return roles
}

// roleFirstMessage owns the fixed product instructions; messages never enter saved state.
func roleFirstMessage(role string) (string, string) {
	switch role {
	case "guide":
		return "Guide", "Work through the Story and the change with the user in this project's pane. Use your surla connection, starting with the surla://golden-path guidance resource and list_changes/get_change. Only the user confirms intent and approves results, on the views page."
	case "builder":
		return "Builder", "Use your surla connection, starting with the surla://golden-path guidance resource. Use list_builder_assignments to find the build, then read get_builder_assignment. Build the agreed revision in this working folder, use generate_code and check_project, and commit. Read the assignment before verification: when a test writer is used, verify_change is refused until its tests are accepted. Only the user confirms intent and approves results on the views page."
	case "test_writer":
		return "Test writer", "Use your surla connection, starting with the surla://golden-path guidance resource. Use list_test_writer_assignments to find the build, then read get_test_writer_assignment. Implement one test per required entry in the test module, eval/ in the working folder the assignment names, and change nothing outside that module. Your tests must also hold the service to every answer the user confirmed, every confirmation example and every preserved behavior the assignment lists. Surla generates the test module with the required tests when the build starts and again at each report: never regenerate it yourself (generating is the builder's step, and surla gen would drop the required tests); after changing its design, report to have it generated again. To check your work, run go build ./... in eval/: it fails naming any required method you have not written. Then call report_tests_finished. Surla's check of your report is the judge: read get_test_writer_assignment again for its verdict, with any findings to fix. Only the user confirms intent and approves results on the views page."
	}
	panic("unregistered team role")
}

// teamRoster validates folders and credentials before constructing the complete closed launcher schema.
func (s *Server) teamRoster(roles []launcherRole) (launcherRoster, error) {
	roster := launcherRoster{Root: s.state.Record.Directory, Swarm: s.state.Record.Team, Roles: make([]launcherRole, 0, len(roles))}
	for _, role := range roles {
		if !validTeamName(role.Role) || !validTeamName(role.Handle) {
			return launcherRoster{}, errors.New("agent identifier cannot be represented by the Chorus launcher")
		}
		info, err := os.Stat(role.Folder)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return launcherRoster{}, fmt.Errorf("%w: %w", errRoleFolder, err)
		}
		if !filepath.IsAbs(role.Folder) || !info.IsDir() {
			return launcherRoster{}, fmt.Errorf("%w: it must be an existing absolute directory", errRoleFolder)
		}
		if _, err := readCredential(s.state.AgentFile(role.Handle)); err != nil {
			return launcherRoster{}, err
		}
		role.Connections = []launcherConnection{{Name: "surla", URL: s.state.Origin() + "/mcp", CredentialFile: s.state.AgentFile(role.Handle)}}
		role.Place = nil
		if i := len(roster.Roles); i > 0 {
			direction := "down"
			if i == 1 {
				direction = "right"
			}
			role.Place = &launcherPlace{Beside: roster.Roles[i-1].Handle, Direction: direction}
		}
		roster.Roles = append(roster.Roles, role)
	}
	if len(roster.Roles) == 0 {
		return launcherRoster{}, fmt.Errorf("%w: no team working folder exists", errRoleFolder)
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

// roleRunning reports whether a terminal is running an agent rather than an idle shell.
//
// Shared contract with Surla: a live terminal for which Herdr reports no
// foreground program counts as running; only a reported foreground that is an
// idle shell makes a role not running. An idle shell has the base name of
// argv[0] (leading dashes removed) in IDLE_SHELLS or the base name of $SHELL,
// and neither argv[1] nor argv[2] is -c or a single-dash flag group containing
// c (such as -lic).
func roleRunning(t TeamTerminal, shell string) bool {
	if t.State != "live" {
		return false
	}
	if t.Foreground == nil {
		return true
	}
	if len(t.Foreground.Argv) == 0 || t.Foreground.Argv[0] == "" {
		return true
	}
	argv := t.Foreground.Argv
	name := strings.TrimLeft(filepath.Base(strings.TrimLeft(argv[0], "-")), "-")
	idleShell := name == strings.TrimLeft(filepath.Base(strings.TrimLeft(shell, "-")), "-") && shell != ""
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

// teamUnavailable records a fixed bounded failure without exporting private paths or command arguments.
// The failed step stays on the span; the returned cause is one of the design's
// TeamUnavailable values and selects the recovery step each caller shows.
func teamUnavailable(ctx context.Context, step, cause string) error {
	failure := errors.New("project team could not be prepared")
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("team.failed_step", step), attribute.String("team.failed_cause", cause))
	span.RecordError(failure)
	span.SetStatus(codes.Error, "team start failed")
	return &genproject.TeamUnavailable{Name: "team_unavailable", Cause: cause}
}

// teamNextStep owns the user's next action after preparing a team. The launcher
// reads its rows when it opens; an already open dialog rereads a rewritten roster
// only when the user clicks its existing Reload team action.
func teamNextStep(dialog string) string {
	switch dialog {
	case "opened":
		return "Choose an agent for each role in the open Chorus dialog in Herdr."
	case "already_open":
		return "If the open Chorus dialog is for this project, click \"Reload team\", then choose the agents. If it is for another project, close it, then start the agents again (Start agents on the views page, or run surla)."
	case "not_needed":
		return "Every listed role is running."
	}
	panic("unrecognized team dialog state")
}

// acceptedTeamContext retains caller values and tracing while the server owns
// cancellation and the one-operation deadline. Cleanup stops the lifetime callback.
func (s *Server) acceptedTeamContext(ctx context.Context) (context.Context, context.CancelFunc) {
	withTimeout := s.team.withTimeout
	if withTimeout == nil {
		withTimeout = context.WithTimeout
	}
	ctx, cancel := withTimeout(context.WithoutCancel(ctx), teamStartTimeout)
	stop := context.AfterFunc(s.team.dependencies.Lifetime, cancel)
	if s.team.dependencies.Lifetime.Err() != nil {
		cancel()
	}
	return ctx, func() {
		stop()
		cancel()
	}
}
