// This file binds the project-owned team to user operations and read-only assignment facts.
package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"go.opentelemetry.io/otel/codes"
)

type (
	// Team is the project server's sole in-memory team owner. Authorization belongs
	// to the control plane's generated endpoint policy, never these internal calls.
	// Supervise returns nil when either of its owning lifetimes ends; a non-nil
	// error means unexpected failure and permits a bounded retry.
	Team interface {
		ReadTeam(context.Context) (*gensurla.TeamSnapshot, error)
		ShowPane(context.Context, string) error
		StartAgents(context.Context) (*gensurla.ProjectTeam, error)
		RegisterBuildRoles(context.Context, BuildFolders) error
		Supervise(context.Context, AssignmentReader) error
	}
	// BuildFolders selects the roles a build adds to the project-owned team.
	// Its two cases carry only working folders: admission cannot name the guide,
	// duplicate a role, or supply credentials or first messages. Registration saves
	// exactly these roles and folders, removing an old test writer and credential
	// for BuilderOnly. It performs no window operation; StartAgents owns that phase.
	BuildFolders interface {
		buildFolders()
	}
	// BuilderOnly admits the builder when stored verification settings supply tests.
	BuilderOnly struct {
		BuilderFolder string
	}
	// BuilderAndTestWriter admits both roles with their independently owned folders.
	BuilderAndTestWriter struct {
		BuilderFolder    string
		TestWriterFolder string
	}
	// TeamBinding associates the one team owner with its immutable resource owner.
	// The composition root binds it once before New; it never creates user credentials.
	TeamBinding struct {
		Owner   string
		Service Team
	}
	// AssignmentGeneration names one currently owed read. Generation is opaque to
	// the team owner and contains no work text, identifiers or findings.
	AssignmentGeneration struct {
		Role       string
		Generation string
	}
	// AssignmentReader returns only the bound owner's unread assignment generations.
	// An error means unknown, not an empty owed set. Reading never marks work read.
	AssignmentReader func(context.Context) ([]AssignmentGeneration, error)
)

// GetTeam reads only the project owner's current snapshot and creates no team.
func (c *ControlPlane) GetTeam(ctx context.Context) (*gensurla.TeamSnapshot, error) {
	team, err := c.ownedTeam(ctx)
	if err != nil {
		return nil, err
	}
	return team.ReadTeam(ctx)
}

// ShowTeamPane delegates the user's role after purpose and resource authorization.
func (c *ControlPlane) ShowTeamPane(ctx context.Context, p *gensurla.ShowTeamPanePayload) error {
	team, err := c.ownedTeam(ctx)
	if err != nil {
		return err
	}
	return team.ShowPane(ctx, p.Role)
}

// StartTeamAgents uses exactly the project owner's existing repeatable team preparation.
func (c *ControlPlane) StartTeamAgents(ctx context.Context) (*gensurla.ProjectTeam, error) {
	team, err := c.ownedTeam(ctx)
	if err != nil {
		return nil, err
	}
	return team.StartAgents(ctx)
}

// buildFolders seals the builder-only case to this package.
func (BuilderOnly) buildFolders() {}

// buildFolders seals the builder-and-test-writer case to this package.
func (BuilderAndTestWriter) buildFolders() {}

// ownedTeam checks resource ownership once at each operation's entry, independently
// of the generated endpoint's caller-purpose policy. Internal observation reads
// already belong to the fixed project owner and perform no repeated authorization.
func (c *ControlPlane) ownedTeam(ctx context.Context) (Team, error) {
	if c.team == nil {
		return nil, gensurla.MakeNotImplemented(errors.New("start this project with surla to use its team"))
	}
	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.ID != c.team.Owner {
		return nil, gensurla.MakeForbidden(errors.New("caller may not access this project team"))
	}
	return c.team.Service, nil
}

// startTeamSupervisor admits the one lifetime worker only after all stores and fields are ready.
func (c *ControlPlane) startTeamSupervisor() {
	if c.team == nil {
		return
	}
	c.teamWorker.Go(c.superviseTeam)
}

// superviseTeam prevents an unexpected dependency return from ending the server or spinning.
func (c *ControlPlane) superviseTeam() {
	for c.serverCtx.Err() == nil {
		err := c.team.Service.Supervise(c.serverCtx, c.assignmentGenerations)
		if err == nil || c.serverCtx.Err() != nil {
			return
		}
		_, span := c.tracer("surla/controlplane").Start(c.serverCtx, "surla.team.supervisor_failed")
		// Dependency details never carry external output or private paths into spans.
		span.RecordError(errors.New("team supervisor could not continue"))
		span.SetStatus(codes.Error, "team supervisor ended")
		span.End()
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-c.serverCtx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// assignmentGenerations deliberately uses the store owner's nonmutating read,
// including builds of earlier revisions. Verdict stamps join the identity when
// the verdict producer exists; current builds have no verdict/report counter.
func (c *ControlPlane) assignmentGenerations(ctx context.Context) ([]AssignmentGeneration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	builds, err := c.ownedBuilds(c.team.Owner)
	if err != nil {
		return nil, err
	}
	owed := make([]AssignmentGeneration, 0, 2*len(builds))
	for _, build := range builds {
		raw, err := json.Marshal(struct {
			Change, Revision string
			Started          time.Time
		}{build.file.ChangeID, build.file.RequestRevision, build.file.StartedAt})
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		generation := hex.EncodeToString(sum[:])
		if !build.file.Builder.Read {
			owed = append(owed, AssignmentGeneration{Role: "builder", Generation: generation})
		}
		if !build.storedEvaluator && !build.file.TestWriter.Read {
			owed = append(owed, AssignmentGeneration{Role: "test_writer", Generation: generation})
		}
	}
	return owed, nil
}
