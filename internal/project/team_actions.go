// This file exposes the team owner's non-creating read and the user's explicit actions.
package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	genproject "github.com/goadesign/surla/gen/project"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	goa "goa.design/goa/v3/pkg"
)

// ReadTeam returns registered roles only and never creates a space or agent.
// Missing observations are unknown; expected absence before team preparation is a value.
func (s *Server) ReadTeam(ctx context.Context) (*gensurla.TeamSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	selection := s.teamSelection()
	result := &gensurla.TeamSnapshot{Team: selection.Team, Roles: make([]*gensurla.TeamRoleState, 0, len(selection.Roles))}
	s.team.observationMu.RLock()
	defer s.team.observationMu.RUnlock()
	for _, role := range selection.Roles {
		value := gensurla.TeamRoleState{Role: role.Role, Handle: role.Handle, State: "unknown", Attention: "none", Watcher: "unknown", Assignment: "unknown"}
		if role.Role == "guide" {
			value.Assignment = "nothing_owed"
		}
		if observation, ok := s.team.observed[role.Role]; ok && observation.handle == role.Handle && observation.space == selection.Space {
			value = observation.public
		}
		result.Roles = append(result.Roles, &value)
	}
	return result, nil
}

// StartAgents shares the existing private ensureTeam workflow and its accepted-work lifetime.
// The control plane authorizes the user once; this method does not repeat entry policy.
func (s *Server) StartAgents(ctx context.Context) (*gensurla.ProjectTeam, error) {
	ctx, cancel := s.acceptedTeamContext(ctx)
	defer cancel()
	result, err := s.ensureTeam(ctx)
	if err != nil {
		return nil, userTeamError(err)
	}
	roles := make([]*gensurla.ProjectTeamRole, 0, len(result.Roles))
	for _, role := range result.Roles {
		roles = append(roles, &gensurla.ProjectTeamRole{Role: role.Role, Handle: role.Handle, Running: role.Running})
	}
	return &gensurla.ProjectTeam{Team: result.Team, Space: result.Space, Dialog: result.Dialog, Step: result.Step, Roles: roles}, nil
}

// RegisterBuildRoles durably records the accepted build's exact worker roles and folders.
// Registration completes even when the post-save phase context has ended: the next
// StartAgents must recover the saved build without starting it again. The context
// carries tracing only; this method performs no window work or external commands.
func (s *Server) RegisterBuildRoles(ctx context.Context, folders controlplane.BuildFolders) error {
	ctx, span := otel.Tracer("surla/project").Start(ctx, "surla.project.register_build_roles")
	defer span.End()
	var roles []agentRecord
	switch value := folders.(type) {
	case controlplane.BuilderOnly:
		roles = []agentRecord{{Role: "builder", Folder: value.BuilderFolder}}
	case controlplane.BuilderAndTestWriter:
		roles = []agentRecord{{Role: "builder", Folder: value.BuilderFolder}, {Role: "test_writer", Folder: value.TestWriterFolder}}
	default:
		err := errors.New("build registration requires builder working folders")
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid build role folders")
		return err
	}
	span.SetAttributes(attribute.Int("team.worker_roles", len(roles)))
	// Validate the complete constructed input before changing the registry.
	for _, agent := range roles {
		if err := validateAgent(teamName(agent.Role, s.state.Record.ID), agent); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "invalid build role folders")
			return err
		}
	}
	s.team.Lock()
	defer s.team.Unlock()
	for _, agent := range roles {
		if _, err := s.registerAgent(teamName(agent.Role, s.state.Record.ID), agent); err != nil {
			return teamUnavailable(ctx, "register_role", "project_state")
		}
	}
	if len(roles) == 1 {
		if err := s.removeTestWriter(); err != nil {
			return teamUnavailable(ctx, "remove_test_writer", "project_state")
		}
	}
	return nil
}

// ShowPane first checks overlays, then re-reads the registered role's owned terminals
// and space. It never changes a window for an unregistered or differently labelled pane.
func (s *Server) ShowPane(ctx context.Context, role string) error {
	s.team.Lock()
	defer s.team.Unlock()
	host := s.team.dependencies.Host
	overlays, err := host.Overlays(ctx)
	if err != nil {
		return userTeamError(teamUnavailable(ctx, "overlays", "herdr"))
	}
	if len(overlays) > 0 {
		return gensurla.MakeWindowBusy(errors.New("close the open Herdr dialog before showing a pane"))
	}
	selection := s.teamSelection()
	var handle string
	for _, registered := range selection.Roles {
		if registered.Role == role {
			handle = registered.Handle
		}
	}
	if handle == "" || selection.Space == "" {
		return gensurla.MakePaneUnavailable(errors.New("this role has no owned pane to show; start its agent"))
	}
	if s.team.dependencies.Observer == nil {
		return gensurla.MakeTeamUnavailable(errors.New("team observations are unavailable"))
	}
	// Discover through all three labels before reading any terminal facts.
	terminals, err := s.team.dependencies.Observer.FindTerminals(ctx, TeamSelection{Root: selection.Root, Space: selection.Space, Team: selection.Team, Roles: []TeamRole{{Role: role, Handle: handle}}})
	if err != nil {
		return userTeamError(teamUnavailable(ctx, "terminals", "herdr"))
	}
	var address string
	for _, terminal := range terminals {
		if !selection.owns(terminal, handle) || terminal.State != "live" {
			continue
		}
		if address != "" {
			return gensurla.MakePaneUnavailable(errors.New("this role has several owned panes; choose one in Herdr"))
		}
		address = terminal.Address
	}
	if address == "" {
		return gensurla.MakePaneUnavailable(errors.New("this role has no owned pane to show; start its agent"))
	}
	spaces, err := host.Spaces(ctx)
	if err != nil {
		return userTeamError(teamUnavailable(ctx, "spaces", "herdr"))
	}
	for _, space := range spaces {
		if space.ID != selection.Space {
			continue
		}
		for _, tab := range space.Tabs {
			for _, pane := range tab.Terminals {
				if pane.Terminal == address {
					fresh, err := s.team.dependencies.Observer.ReadTerminal(ctx, address)
					if err != nil || !selection.owns(fresh, handle) || fresh.State != "live" {
						return gensurla.MakePaneUnavailable(errors.New("the requested pane changed; read the team again"))
					}
					space.Tabs = []TeamTab{{ID: tab.ID, Terminals: []TeamPane{{Terminal: address}}}}
					if err := host.Focus(ctx, space); err != nil {
						return userTeamError(teamUnavailable(ctx, "focus", "herdr"))
					}
					return nil
				}
			}
		}
	}
	return gensurla.MakePaneUnavailable(errors.New("the requested pane is no longer in this project's space"))
}

// teamSelection copies only immutable selection facts, never credentials or external replies.
func (s *Server) teamSelection() TeamSelection {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	record := s.state.Record
	selection := TeamSelection{Root: record.Directory, Space: record.Space, Team: record.Team}
	if selection.Team == "" {
		selection.Team = teamName(filepath.Base(record.Directory), record.ID)
	}
	for _, role := range []string{"guide", "builder", "test_writer"} {
		for handle, registered := range record.Agents {
			if registered.Role == role {
				selection.Roles = append(selection.Roles, TeamRole{Role: role, Handle: handle})
			}
		}
	}
	return selection
}

// userTeamError preserves the owner's typed space conflict and sanitizes other
// management failures before the control plane chooses its user-facing transport.
// A typed team failure becomes the page's step for its cause and stays its
// error's cause, so a build start can give its own step for the same cause.
func userTeamError(err error) error {
	var conflict *genproject.TeamSpaceConflict
	if errors.As(err, &conflict) {
		return err
	}
	var domain *goa.ServiceError
	if errors.As(err, &domain) && domain.Name == "window_busy" {
		return gensurla.MakeWindowBusy(errors.New("close the open Herdr dialog before starting agents"))
	}
	var unavailable *genproject.TeamUnavailable
	if !errors.As(err, &unavailable) {
		return gensurla.MakeTeamUnavailable(errors.New("project team could not be prepared; run surla in the project folder to see the step"))
	}
	// Wrapping only the typed failure keeps any peer text around it out.
	failure := gensurla.MakeTeamUnavailable(unavailable)
	failure.Message = userTeamStep(unavailable.Cause)
	return failure
}

// userTeamStep gives the page's fixed sentence for one team failure cause. It names
// no private path: the CLI, run in the project folder, names the folder to repair.
func userTeamStep(cause string) string {
	switch cause {
	case "stopping":
		return "the project server is stopping; run surla in the project folder to start it again"
	case "herdr":
		// Starting agents and showing a pane both fail with this cause, so the step
		// retries whichever action the user took.
		return "Herdr did not complete a team command; open the Herdr app, then try again"
	case "roster_folder":
		return "Herdr's launcher roster folder could not be written; make data/chorus-launcher/rosters in Herdr's app folder (~/.config/herdr-app unless HERDR_APP_HOME is set) writable by this account, then start the agents again"
	case "project_state":
		return "this project's private state could not be read or written; run surla in the project folder to see which folder to repair"
	case "role_folder":
		return "a team role's working folder could not be read; make sure this account can open the project folder and its build working folders, then start the agents again"
	}
	return "project team could not be prepared; run surla in the project folder to see the step"
}

// removeTestWriter revokes an unused test writer before withdrawing its saved role.
// The operation lock serializes this exact-role update with roster publication.
func (s *Server) removeTestWriter() error {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	for handle, agent := range s.state.Record.Agents {
		if agent.Role != "test_writer" {
			continue
		}
		if err := os.Remove(s.state.AgentFile(handle)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		delete(s.state.Record.Agents, handle)
		return s.state.save()
	}
	return nil
}
