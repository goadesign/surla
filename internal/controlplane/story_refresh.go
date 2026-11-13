// This file implements bound-workspace Story refresh operations: refresh_story
// reads the active workspace's Go module snapshot, starts or reuses an
// in-memory sandboxed evaluation per project owner and workspace identity,
// and get_story_refresh reads the current operation state. Every caller acts
// for the owner, so the user, the guide and the builder share one operation.
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
	// storyRefreshKey identifies the owner's current Story refresh slot for a
	// verified bound workspace directory identity.
	storyRefreshKey struct {
		// owner is the project owner every caller acts for (Principal.ID).
		owner string
		// workspaceDir is the bound physical workspace path.
		workspaceDir string
		// workspaceDev is the device ID of workspaceDir.
		workspaceDev uint64
		// workspaceIno is the inode number of workspaceDir.
		workspaceIno uint64
	}

	// storyEvaluationVersion is what the change list's version covers of the
	// owner's current Story evaluation: enough to change whenever the
	// evaluation starts, finishes or is replaced, so a page waiting on the
	// list reads the evaluation again. All fields are empty when no
	// workspace is bound; State is storyRefreshStateNone when no evaluation
	// has run in this server process.
	storyEvaluationVersion struct {
		// OpID identifies the current operation; a replacement on the same
		// snapshot changes it even when its state and digest do not.
		OpID string `json:"op_id"`
		// State is the operation's state.
		State string `json:"state"`
		// SnapshotDigest identifies the evaluated workspace snapshot.
		SnapshotDigest string `json:"snapshot_digest"`
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
	// owner and bound workspace in this server process.
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
// canonical snapshot digest, and starts or reuses the owner's current
// in-memory Story refresh operation for that workspace.
func (c *ControlPlane) RefreshStory(ctx context.Context) (*gensurla.StoryRefreshResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	wsDir, wsDev, wsIno := c.currentWorkspaceTarget()
	if wsDir == "" {
		return nil, gensurla.MakeNotImplemented(errors.New(
			"no workspace directory is bound to this control plane; start Surla from a project directory",
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
		owner:        principal.ID,
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
	evalCtx, cancel := c.storyRefreshDeadline(c.serverCtx)
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
	go c.runStoryRefresh(evalCtx, cancel, defaultStoryRefreshTimeout, key, op, files)
	return res, nil
}

// GetStoryRefresh returns the owner's current in-memory Story refresh
// operation for the bound workspace, or state "none" when none has run.
func (c *ControlPlane) GetStoryRefresh(ctx context.Context) (*gensurla.StoryRefreshResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	wsDir, wsDev, wsIno := c.currentWorkspaceTarget()
	if wsDir == "" {
		return nil, gensurla.MakeNotImplemented(errors.New(
			"no workspace directory is bound to this control plane; start Surla from a project directory",
		))
	}
	key := storyRefreshKey{
		owner:        principal.ID,
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

// storyEvaluation returns what the change list's version covers of owner's
// current Story evaluation for the bound workspace. It reads the same slot
// GetStoryRefresh reads, without starting anything.
func (c *ControlPlane) storyEvaluation(owner string) storyEvaluationVersion {
	wsDir, wsDev, wsIno := c.currentWorkspaceTarget()
	if wsDir == "" {
		return storyEvaluationVersion{}
	}
	key := storyRefreshKey{owner: owner, workspaceDir: wsDir, workspaceDev: wsDev, workspaceIno: wsIno}
	c.storyRefreshMu.Lock()
	defer c.storyRefreshMu.Unlock()
	op := c.storyRefreshes[key]
	if op == nil {
		return storyEvaluationVersion{State: storyRefreshStateNone}
	}
	return storyEvaluationVersion{OpID: op.id, State: op.state, SnapshotDigest: op.snapshotDigest}
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
	timedOut := errors.Is(context.Cause(evalCtx), context.DeadlineExceeded) ||
		errors.Is(err, context.DeadlineExceeded)

	c.storyRefreshMu.Lock()
	defer c.storyRefreshMu.Unlock()
	if c.serverCtx.Err() != nil || c.storyRefreshes[key] != op {
		return
	}
	if err == nil && evalCtx.Err() != nil {
		err = context.Cause(evalCtx)
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

// adoptStoryRefresh reads one module snapshot from the bound workspace,
// verifies that rawOpID is the owner's current succeeded Story refresh
// operation for that exact workspace identity and snapshot digest, and returns
// the snapshot's encoded design/*.go source and a deep copy of its evaluated
// Stories (nil when the design declared zero Stories). Every rejection is an
// unprocessable error naming refresh_story.
func (c *ControlPlane) adoptStoryRefresh(
	principal Principal,
	rawOpID string,
) (string, []designeval.StorySummary, error) {
	opID := strings.TrimSpace(rawOpID)
	if opID == "" {
		return "", nil, gensurla.MakeUnprocessable(errors.New(
			"story_refresh_op_id must name a succeeded refresh_story operation; call refresh_story and pass its op_id",
		))
	}
	wsDir, wsDev, wsIno := c.currentWorkspaceTarget()
	if wsDir == "" {
		return "", nil, gensurla.MakeUnprocessable(fmt.Errorf(
			"cannot adopt story_refresh_op_id %q because no workspace directory is bound to this control plane; start Surla from a project directory, then call refresh_story",
			opID,
		))
	}
	files, err := readWorkspaceSnapshotForStoryRefresh(wsDir, wsDev, wsIno)
	if err != nil {
		return "", nil, gensurla.MakeUnprocessable(fmt.Errorf("%w; call refresh_story again", err))
	}
	digest := designeval.SnapshotDigest(files)

	c.workspaceMu.RLock()
	sameWorkspace := c.activeWorkspaceDir == wsDir &&
		c.boundWorkspaceDev == wsDev &&
		c.boundWorkspaceIno == wsIno
	key := storyRefreshKey{
		owner:        principal.ID,
		workspaceDir: wsDir,
		workspaceDev: wsDev,
		workspaceIno: wsIno,
	}
	c.storyRefreshMu.Lock()
	var opState, opDigest string
	var stories []designeval.StorySummary
	matched := false
	if sameWorkspace {
		if op := c.storyRefreshes[key]; op != nil && op.id == opID {
			matched = true
			opState = op.state
			opDigest = op.snapshotDigest
			if opState == storyRefreshStateSucceeded && opDigest == digest {
				stories = cloneStorySummaries(op.stories)
			}
		}
	}
	c.storyRefreshMu.Unlock()
	c.workspaceMu.RUnlock()

	if !matched {
		return "", nil, gensurla.MakeUnprocessable(fmt.Errorf(
			"story_refresh_op_id %q is not the current Story refresh operation for this project and workspace; call refresh_story and pass the latest succeeded op_id",
			opID,
		))
	}
	if opState != storyRefreshStateSucceeded {
		return "", nil, gensurla.MakeUnprocessable(fmt.Errorf(
			"story_refresh_op_id %q has state %q, not succeeded; wait for get_story_refresh to report succeeded or call refresh_story again",
			opID, opState,
		))
	}
	if opDigest != digest {
		return "", nil, gensurla.MakeUnprocessable(fmt.Errorf(
			"workspace snapshot (%s) no longer matches story_refresh_op_id %q (%s); call refresh_story again",
			digest, opID, opDigest,
		))
	}
	designSrc, err := designSourceFromModuleSnapshot(files)
	if err != nil {
		return "", nil, err
	}
	return designSrc, stories, nil
}

// storyRefreshDeadline bounds one Story refresh evaluation.
func (c *ControlPlane) storyRefreshDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	withTimeout := c.storyRefreshWithTimeout
	if withTimeout == nil {
		withTimeout = context.WithTimeout
	}
	return withTimeout(ctx, defaultStoryRefreshTimeout)
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

// cloneStorySummaries returns a deep copy of stories, normalizing empty slices to nil.
func cloneStorySummaries(stories []designeval.StorySummary) []designeval.StorySummary {
	if len(stories) == 0 {
		return nil
	}
	out := make([]designeval.StorySummary, len(stories))
	for i, s := range stories {
		var actors []designeval.StoryActor
		if len(s.Actors) > 0 {
			actors = slices.Clone(s.Actors)
		}
		out[i] = designeval.StorySummary{
			Name:        s.Name,
			Description: s.Description,
			Actors:      actors,
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

// cloneStoryQuestions returns a deep copy of questions, normalizing empty slices to nil.
func cloneStoryQuestions(questions []designeval.StoryQuestion) []designeval.StoryQuestion {
	if len(questions) == 0 {
		return nil
	}
	out := make([]designeval.StoryQuestion, len(questions))
	for i, q := range questions {
		var options []string
		if len(q.Options) > 0 {
			options = slices.Clone(q.Options)
		}
		out[i] = designeval.StoryQuestion{
			Key:         q.Key,
			Prompt:      q.Prompt,
			Description: q.Description,
			Options:     options,
		}
	}
	return out
}

// storySummaryItemsOrNil returns storySummaryItems(stories) when stories is
// non-empty, or nil so ChangeCardResult omits stories when none were adopted.
func storySummaryItemsOrNil(stories []designeval.StorySummary) []*gensurla.StorySummaryItem {
	if len(stories) == 0 {
		return nil
	}
	return storySummaryItems(stories)
}

// canonicalStorySummariesFromItems converts generated transport Story items
// into canonical designeval.StorySummary values, normalizing empty slices to nil.
func canonicalStorySummariesFromItems(items []*gensurla.StorySummaryItem) []designeval.StorySummary {
	if len(items) == 0 {
		return nil
	}
	out := make([]designeval.StorySummary, 0, len(items))
	for _, s := range items {
		if s == nil {
			continue
		}
		var actors []designeval.StoryActor
		if len(s.Actors) > 0 {
			actors = make([]designeval.StoryActor, 0, len(s.Actors))
			for _, a := range s.Actors {
				if a == nil {
					continue
				}
				actors = append(actors, designeval.StoryActor{
					Key:         a.Key,
					Title:       a.Title,
					Description: derefString(a.Description),
				})
			}
			if len(actors) == 0 {
				actors = nil
			}
		}
		var flows []designeval.StoryFlow
		if len(s.Flows) > 0 {
			flows = make([]designeval.StoryFlow, 0, len(s.Flows))
			for _, f := range s.Flows {
				if f == nil {
					continue
				}
				var steps []designeval.StoryStep
				if len(f.Steps) > 0 {
					steps = make([]designeval.StoryStep, 0, len(f.Steps))
					for _, st := range f.Steps {
						if st == nil {
							continue
						}
						steps = append(steps, designeval.StoryStep{
							Key:         st.Key,
							ActorKey:    st.ActorKey,
							Summary:     st.Summary,
							Description: derefString(st.Description),
							Kind:        st.Kind,
							Questions:   canonicalStoryQuestionsFromItems(st.Questions),
						})
					}
					if len(steps) == 0 {
						steps = nil
					}
				}
				flows = append(flows, designeval.StoryFlow{
					Key:         f.Key,
					Title:       f.Title,
					Description: derefString(f.Description),
					Complete:    f.Complete,
					Steps:       steps,
					Questions:   canonicalStoryQuestionsFromItems(f.Questions),
				})
			}
			if len(flows) == 0 {
				flows = nil
			}
		}
		out = append(out, designeval.StorySummary{
			Name:        s.Name,
			Description: derefString(s.Description),
			Actors:      actors,
			Flows:       flows,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// canonicalStoryQuestionsFromItems converts generated transport Story question
// items into canonical designeval.StoryQuestion values.
func canonicalStoryQuestionsFromItems(items []*gensurla.StoryQuestionItem) []designeval.StoryQuestion {
	if len(items) == 0 {
		return nil
	}
	out := make([]designeval.StoryQuestion, 0, len(items))
	for _, q := range items {
		if q == nil {
			continue
		}
		var options []string
		if len(q.Options) > 0 {
			options = slices.Clone(q.Options)
		}
		out = append(out, designeval.StoryQuestion{
			Key:         q.Key,
			Prompt:      q.Prompt,
			Description: derefString(q.Description),
			Options:     options,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// derefString returns *s when s is non-nil or "".
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
