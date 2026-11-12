// This file implements bound-workspace Story refresh operations: refresh_story
// reads the active workspace's Go module snapshot, starts or reuses an
// in-memory sandboxed evaluation per authenticated principal and workspace
// identity, and get_story_refresh reads the current operation state.
package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
)

type (
	// storyRefreshKey identifies one caller's current Story refresh slot for a
	// verified bound workspace directory identity.
	storyRefreshKey struct {
		// principalID is the authenticated caller's Principal.ID.
		principalID string
		// workspaceDir is the bound physical workspace path.
		workspaceDir string
		// workspaceDev is the device ID of workspaceDir.
		workspaceDev uint64
		// workspaceIno is the inode number of workspaceDir.
		workspaceIno uint64
	}

	// storyRefreshOp holds the in-memory state of one Story refresh operation.
	// All reads and mutations after creation occur under ControlPlane.storyRefreshMu.
	storyRefreshOp struct {
		// id is the 32-character lowercase hex operation identifier.
		id string
		// state is storyRefreshStateRunning, storyRefreshStateSucceeded, or
		// storyRefreshStateFailed.
		state string
		// snapshotDigest is designeval.SnapshotDigest(files) for the evaluated snapshot.
		snapshotDigest string
		// stories holds the evaluated Story summaries when state is
		// storyRefreshStateSucceeded.
		stories []designeval.StorySummary
		// diagnostic holds the safe failure or timeout message when state is
		// storyRefreshStateFailed.
		diagnostic string
		// cancel ends this operation's evaluation context when superseded.
		cancel context.CancelFunc
	}
)

const (
	// storyRefreshStateNone reports that no Story refresh has run yet for the
	// caller and bound workspace in this server process.
	storyRefreshStateNone = "none"
	// storyRefreshStateRunning reports that sandboxed Story evaluation is in progress.
	storyRefreshStateRunning = "running"
	// storyRefreshStateSucceeded reports that sandboxed Story evaluation completed.
	storyRefreshStateSucceeded = "succeeded"
	// storyRefreshStateFailed reports that sandboxed Story evaluation failed or timed out.
	storyRefreshStateFailed = "failed"
	// defaultStoryRefreshTimeout bounds one Story refresh operation in the
	// control plane, sized for a cold module download and compilation.
	defaultStoryRefreshTimeout = 5 * time.Minute
	// completeSourcesDiagnosticTrailer is the generate_code-only guidance line
	// appended by designeval.GenerationDiagnostic on build failures.
	completeSourcesDiagnosticTrailer = "\nIf the compiler reports that generator.CompleteSources is undefined,"
)

// RefreshStory reads the bound workspace's module snapshot, computes its
// canonical snapshot digest, and starts or reuses the authenticated caller's
// current in-memory Story refresh operation for that workspace.
func (c *ControlPlane) RefreshStory(ctx context.Context) (*gensurla.StoryRefreshResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	wsDir, wsDev, wsIno := c.currentWorkspaceTarget()
	if wsDir == "" {
		return nil, gensurla.MakeNotImplemented(errors.New(
			"no workspace directory is bound to this control plane; start Surla from a project directory or pass -project",
		))
	}
	files, err := readWorkspaceSnapshotForStoryRefresh(wsDir, wsDev, wsIno)
	if err != nil {
		return nil, err
	}
	digest := designeval.SnapshotDigest(files)

	c.workspaceMu.RLock()
	if c.serverCtx.Err() != nil {
		c.workspaceMu.RUnlock()
		return nil, gensurla.MakeUnavailable(errors.New(
			"the control plane is shutting down and is no longer starting Story refresh operations",
		))
	}
	if c.activeWorkspaceDir != wsDir || c.boundWorkspaceDev != wsDev || c.boundWorkspaceIno != wsIno {
		c.workspaceMu.RUnlock()
		return nil, gensurla.MakeUnprocessable(errors.New(
			"bound workspace changed while reading the module snapshot; call refresh_story again",
		))
	}

	key := storyRefreshKey{
		principalID:  principal.ID,
		workspaceDir: wsDir,
		workspaceDev: wsDev,
		workspaceIno: wsIno,
	}

	c.storyRefreshMu.Lock()
	current := c.storyRefreshes[key]
	if current != nil && current.snapshotDigest == digest &&
		(current.state == storyRefreshStateRunning || current.state == storyRefreshStateSucceeded) {
		res := storyRefreshResultFromOp(current)
		c.storyRefreshMu.Unlock()
		c.workspaceMu.RUnlock()
		return res, nil
	}

	opID, err := newStoryRefreshOpID()
	if err != nil {
		c.storyRefreshMu.Unlock()
		c.workspaceMu.RUnlock()
		return nil, err
	}
	timeout := c.storyRefreshDeadline()
	evalCtx, cancel := context.WithTimeout(c.serverCtx, timeout)
	op := &storyRefreshOp{
		id:             opID,
		state:          storyRefreshStateRunning,
		snapshotDigest: digest,
		cancel:         cancel,
	}
	if c.storyRefreshes == nil {
		c.storyRefreshes = make(map[storyRefreshKey]*storyRefreshOp)
	}
	c.storyRefreshes[key] = op
	var prevCancel context.CancelFunc
	if current != nil {
		prevCancel = current.cancel
	}
	res := storyRefreshResultFromOp(op)
	c.storyRefreshOps.Add(1)
	c.storyRefreshMu.Unlock()
	c.workspaceMu.RUnlock()

	if prevCancel != nil {
		prevCancel()
	}
	go c.runStoryRefresh(evalCtx, cancel, timeout, key, op, files)
	return res, nil
}

// GetStoryRefresh returns the authenticated caller's current in-memory Story
// refresh operation for the bound workspace, or state "none" when none has run.
func (c *ControlPlane) GetStoryRefresh(ctx context.Context) (*gensurla.StoryRefreshResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	wsDir, wsDev, wsIno := c.currentWorkspaceTarget()
	if wsDir == "" {
		return nil, gensurla.MakeNotImplemented(errors.New(
			"no workspace directory is bound to this control plane; start Surla from a project directory or pass -project",
		))
	}
	key := storyRefreshKey{
		principalID:  principal.ID,
		workspaceDir: wsDir,
		workspaceDev: wsDev,
		workspaceIno: wsIno,
	}
	c.storyRefreshMu.Lock()
	op := c.storyRefreshes[key]
	res := storyRefreshResultFromOp(op)
	c.storyRefreshMu.Unlock()
	return res, nil
}

// runStoryRefresh executes one sandboxed design evaluation for op on a
// server-owned context and publishes its result only if the server is still
// running and op is still the current operation for key.
func (c *ControlPlane) runStoryRefresh(
	evalCtx context.Context,
	cancel context.CancelFunc,
	timeout time.Duration,
	key storyRefreshKey,
	op *storyRefreshOp,
	files map[string]string,
) {
	defer c.storyRefreshOps.Done()
	defer cancel()

	res, err := c.evaluator.Evaluate(evalCtx, files)
	timedOut := errors.Is(evalCtx.Err(), context.DeadlineExceeded) ||
		errors.Is(err, context.DeadlineExceeded)

	c.storyRefreshMu.Lock()
	defer c.storyRefreshMu.Unlock()
	if c.serverCtx.Err() != nil || c.storyRefreshes[key] != op {
		return
	}
	if err == nil && evalCtx.Err() != nil {
		err = evalCtx.Err()
		timedOut = timedOut || errors.Is(err, context.DeadlineExceeded)
	}
	op.cancel = nil
	if err != nil {
		op.state = storyRefreshStateFailed
		op.stories = nil
		op.diagnostic = storyRefreshDiagnostic(err, timeout, timedOut)
		return
	}
	op.state = storyRefreshStateSucceeded
	op.stories = cloneStorySummaries(res.Stories)
	op.diagnostic = ""
}

// closeStoryRefreshes waits for all admitted Story refresh evaluations to
// finish after c.cancelServer() and removes any interrupted running entries so
// shutdown never leaves an operation running or publishes after Close.
func (c *ControlPlane) closeStoryRefreshes() {
	c.storyRefreshOps.Wait()
	c.storyRefreshMu.Lock()
	for k, op := range c.storyRefreshes {
		if op.state == storyRefreshStateRunning {
			delete(c.storyRefreshes, k)
		}
	}
	c.storyRefreshMu.Unlock()
}

// storyRefreshDeadline returns the per-refresh evaluation timeout for c.
func (c *ControlPlane) storyRefreshDeadline() time.Duration {
	if c.storyRefreshTimeout > 0 {
		return c.storyRefreshTimeout
	}
	return defaultStoryRefreshTimeout
}

// readWorkspaceSnapshotForStoryRefresh reads the bound workspace's Go module
// snapshot through an identity-verified os.Root handle without holding any
// control-plane lock.
func readWorkspaceSnapshotForStoryRefresh(wsDir string, wsDev, wsIno uint64) (map[string]string, error) {
	cleanDir, root, err := openModuleRootWithIdentity(wsDir, wsDev, wsIno)
	if err != nil {
		return nil, err
	}
	files, readErr := designeval.ReadModuleSnapshotFromRoot(cleanDir, root)
	if err := errors.Join(readErr, root.Close()); err != nil {
		return nil, gensurla.MakeUnprocessable(err)
	}
	if err := designeval.ValidateUTF8Snapshot(files); err != nil {
		return nil, gensurla.MakeUnprocessable(err)
	}
	if strings.TrimSpace(files["go.mod"]) == "" {
		return nil, gensurla.MakeUnprocessable(fmt.Errorf(
			"workspace %s does not contain a go.mod file; run init_project or create go.mod before calling refresh_story",
			cleanDir,
		))
	}
	return files, nil
}

// storyRefreshResultFromOp projects op (or nil when no refresh has run) into a
// fresh gensurla.StoryRefreshResult. ControlPlane.storyRefreshMu must be held.
func storyRefreshResultFromOp(op *storyRefreshOp) *gensurla.StoryRefreshResult {
	if op == nil {
		return &gensurla.StoryRefreshResult{
			State:   storyRefreshStateNone,
			Stories: []*gensurla.StorySummaryItem{},
			Message: "No Story refresh has run for this workspace yet; call refresh_story to evaluate the workspace design.",
		}
	}
	id := op.id
	digest := op.snapshotDigest
	res := &gensurla.StoryRefreshResult{
		OpID:           &id,
		State:          op.state,
		SnapshotDigest: &digest,
		Stories:        []*gensurla.StorySummaryItem{},
	}
	switch op.state {
	case storyRefreshStateRunning:
		res.Message = fmt.Sprintf(
			"Story refresh %s is running for workspace snapshot %s; call get_story_refresh to check its result.",
			op.id, op.snapshotDigest,
		)
	case storyRefreshStateSucceeded:
		res.Stories = storySummaryItems(op.stories)
		if len(res.Stories) == 0 {
			res.Message = fmt.Sprintf(
				"Story refresh %s succeeded for workspace snapshot %s: the design declares no Stories.",
				op.id, op.snapshotDigest,
			)
		} else {
			res.Message = fmt.Sprintf(
				"Story refresh %s succeeded for workspace snapshot %s: %d Story declaration(s).",
				op.id, op.snapshotDigest, len(res.Stories),
			)
		}
	case storyRefreshStateFailed:
		diag := op.diagnostic
		res.Diagnostic = &diag
		res.Message = fmt.Sprintf(
			"Story refresh %s failed for workspace snapshot %s.",
			op.id, op.snapshotDigest,
		)
	}
	return res
}

// storyRefreshDiagnostic formats a caller-safe diagnostic from err with private
// filesystem paths replaced and operator proxy credentials withheld.
func storyRefreshDiagnostic(err error, timeout time.Duration, timedOut bool) string {
	diag := designeval.GenerationDiagnostic(err)
	if idx := strings.Index(diag, completeSourcesDiagnosticTrailer); idx >= 0 {
		diag = diag[:idx]
	}
	if timedOut {
		if rest, ok := strings.CutPrefix(diag, "Code generation failed while "); ok {
			return fmt.Sprintf("Story refresh timed out after %s while %s", timeout, rest)
		}
		return fmt.Sprintf("Story refresh timed out after %s.", timeout)
	}
	if rest, ok := strings.CutPrefix(diag, "Code generation failed while "); ok {
		return "Story evaluation failed while " + rest
	}
	return diag
}

// cloneStorySummaries returns a deep copy of stories.
func cloneStorySummaries(stories []designeval.StorySummary) []designeval.StorySummary {
	if len(stories) == 0 {
		return nil
	}
	out := make([]designeval.StorySummary, len(stories))
	for i, s := range stories {
		out[i] = designeval.StorySummary{
			Name:        s.Name,
			Description: s.Description,
			Actors:      slices.Clone(s.Actors),
		}
		if len(s.Flows) > 0 {
			flows := make([]designeval.StoryFlow, len(s.Flows))
			for j, f := range s.Flows {
				flows[j] = designeval.StoryFlow{
					Key:         f.Key,
					Title:       f.Title,
					Description: f.Description,
					Complete:    f.Complete,
					Questions:   cloneStoryQuestions(f.Questions),
				}
				if len(f.Steps) > 0 {
					steps := make([]designeval.StoryStep, len(f.Steps))
					for k, st := range f.Steps {
						steps[k] = designeval.StoryStep{
							Key:         st.Key,
							ActorKey:    st.ActorKey,
							Summary:     st.Summary,
							Description: st.Description,
							Kind:        st.Kind,
							Questions:   cloneStoryQuestions(st.Questions),
						}
					}
					flows[j].Steps = steps
				}
			}
			out[i].Flows = flows
		}
	}
	return out
}

// cloneStoryQuestions returns a deep copy of questions.
func cloneStoryQuestions(questions []designeval.StoryQuestion) []designeval.StoryQuestion {
	if len(questions) == 0 {
		return nil
	}
	out := make([]designeval.StoryQuestion, len(questions))
	for i, q := range questions {
		out[i] = designeval.StoryQuestion{
			Key:         q.Key,
			Prompt:      q.Prompt,
			Description: q.Description,
			Options:     slices.Clone(q.Options),
		}
	}
	return out
}

// storySummaryItems converts canonical Story summaries into generated transport items.
func storySummaryItems(stories []designeval.StorySummary) []*gensurla.StorySummaryItem {
	out := make([]*gensurla.StorySummaryItem, len(stories))
	for i, s := range stories {
		actors := make([]*gensurla.StoryActorItem, len(s.Actors))
		for j, a := range s.Actors {
			actors[j] = &gensurla.StoryActorItem{
				Key:         a.Key,
				Title:       a.Title,
				Description: optionalString(a.Description),
			}
		}
		flows := make([]*gensurla.StoryFlowItem, len(s.Flows))
		for j, f := range s.Flows {
			steps := make([]*gensurla.StoryStepItem, len(f.Steps))
			for k, st := range f.Steps {
				steps[k] = &gensurla.StoryStepItem{
					Key:         st.Key,
					ActorKey:    st.ActorKey,
					Summary:     st.Summary,
					Description: optionalString(st.Description),
					Kind:        st.Kind,
					Questions:   storyQuestionItems(st.Questions),
				}
			}
			flows[j] = &gensurla.StoryFlowItem{
				Key:         f.Key,
				Title:       f.Title,
				Description: optionalString(f.Description),
				Complete:    f.Complete,
				Steps:       steps,
				Questions:   storyQuestionItems(f.Questions),
			}
		}
		out[i] = &gensurla.StorySummaryItem{
			Name:        s.Name,
			Description: optionalString(s.Description),
			Actors:      actors,
			Flows:       flows,
		}
	}
	return out
}

// storyQuestionItems converts canonical Story questions into generated transport items.
func storyQuestionItems(questions []designeval.StoryQuestion) []*gensurla.StoryQuestionItem {
	out := make([]*gensurla.StoryQuestionItem, len(questions))
	for i, q := range questions {
		options := slices.Clone(q.Options)
		if options == nil {
			options = []string{}
		}
		out[i] = &gensurla.StoryQuestionItem{
			Key:         q.Key,
			Prompt:      q.Prompt,
			Description: optionalString(q.Description),
			Options:     options,
		}
	}
	return out
}

// optionalString returns nil when s is empty or a pointer to a copy of s.
func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// newStoryRefreshOpID returns a 32-character lowercase hex identifier.
func newStoryRefreshOpID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate story refresh operation id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
