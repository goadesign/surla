// Package swarm plans deterministic Chorus multi-agent role assignments,
// capability- and cross-family model routing, and pinned collaboration
// manifests for Surla builds.
//
// Chorus (`who`, `join`, `send`, `read`) owns real-time message routing,
// channel subscriptions, and pinned channel manifests. This package owns the
// server-side mapping from verified coding-agent provider profiles and explicit
// workspace path scopes to proposed role assignments (`@builder-*` and
// `@reviewer` when `Builders` are supplied, plus optional `@architect` when
// `ArchitectPaths` are supplied and `@eval-author` when `EvalPaths` are
// supplied). Declared path scopes and access modes are operator-declared
// coordination ownership boundaries for Chorus agents, not Surla-enforced
// secrecy or OS isolation; at Checkpoint 4, `verify.Run` builds the candidate
// and runs candidate processes inside `verify/candidate`'s OS sandbox while
// executing the trusted evaluator suite binary directly on the host.
package swarm

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

type (
	// RoleKind identifies a specialized agent role in a Surla build swarm.
	RoleKind string

	// ProviderProfile describes one server-configured coding-agent model profile.
	// Authentication, model family, and role capabilities come from server-owned
	// configuration, never from model-authored arguments or Chorus role strings.
	ProviderProfile struct {
		// ID is the unique profile identifier (for example, "claude-opus" or "codex-gpt5").
		ID string `json:"id"`
		// Provider names the backing CLI or model provider (for example, "anthropic", "openai", "google").
		Provider string `json:"provider"`
		// Model is the concrete model identifier (for example, "claude-opus-4-6", "gpt-5.2-codex").
		Model string `json:"model"`
		// Family is the canonical model family (for example, "claude", "gpt", "gemini").
		// Distinct profiles from the same vendor family share the same Family value.
		Family string `json:"family"`
		// Authenticated is true only when the server has verified usable credentials for this profile.
		Authenticated bool `json:"authenticated"`
		// Capabilities lists the Surla swarm roles this profile can perform.
		Capabilities []RoleKind `json:"capabilities"`
	}

	// BuilderScope defines an explicit file-ownership slice for one builder agent.
	BuilderScope struct {
		// Name is the human-readable scope name (for example, "orders" or "api").
		Name string `json:"name"`
		// Paths lists the repository-relative path globs owned by this builder.
		Paths []string `json:"paths"`
		// PreferredProfileID optionally pins a specific authenticated profile ID for this builder.
		PreferredProfileID string `json:"preferred_profile_id,omitempty"`
	}

	// Request specifies the inputs for planning a Surla build swarm roster.
	Request struct {
		// ChangeID is the Change Card identifier being planned.
		ChangeID string `json:"change_id"`
		// DesignDigest is the SHA-256 digest of the evaluated CP2 design snapshot.
		DesignDigest string `json:"design_digest"`
		// Profiles lists the server-configured coding-agent profiles.
		Profiles []ProviderProfile `json:"profiles"`
		// ArchitectPaths lists the optional repository-relative path globs owned
		// by @architect. When non-empty (requires Builders), Plan includes
		// RoleArchitect in the proposed roster without inventing default globs.
		ArchitectPaths []string `json:"architect_paths,omitempty"`
		// Builders lists the explicit builder write scopes. When empty, Plan
		// returns a solo preview (Status "unconfigured", MissingRoles nil) and
		// rejects non-empty ArchitectPaths or EvalPaths.
		Builders []BuilderScope `json:"builders,omitempty"`
		// EvalPaths lists the optional repository or evaluator-workspace relative
		// path globs owned by @eval-author. When non-empty (requires Builders),
		// Plan includes RoleEvalAuthor in the proposed roster without inventing
		// default globs.
		EvalPaths []string `json:"eval_paths,omitempty"`
	}

	// Assignment describes one proposed agent role in the Surla build swarm.
	Assignment struct {
		// Handle is the deterministic Chorus agent handle (without leading '@', <= 32 chars).
		Handle string `json:"handle"`
		// Role is the assigned RoleKind ("architect", "builder", "eval_author", or "reviewer").
		Role RoleKind `json:"role"`
		// ScopeName is the human-readable scope label for this assignment.
		ScopeName string `json:"scope_name"`
		// ProfileID is the assigned ProviderProfile.ID.
		ProfileID string `json:"profile_id"`
		// Provider is the assigned ProviderProfile.Provider.
		Provider string `json:"provider"`
		// Model is the assigned ProviderProfile.Model.
		Model string `json:"model"`
		// Family is the assigned ProviderProfile.Family.
		Family string `json:"family"`
		// OwnedPaths lists the operator-declared repository or workspace path globs for Chorus coordination.
		OwnedPaths []string `json:"owned_paths"`
		// AccessMode describes the operator-declared workspace coordination boundary ("read_only_after_cp2", "read_write_worktree", "isolated_eval_workspace", or "read_only_review").
		AccessMode string `json:"access_mode"`
	}

	// PlanResult holds the proposed swarm role roster and model-diversity facts
	// surfaced at Checkpoint 2 (CP2).
	PlanResult struct {
		// Status reports whether a roster was configured ("configured",
		// "unconfigured" on solo previews or when no authenticated profiles exist,
		// or "missing_capability").
		Status string `json:"status"`
		// Assignments lists the deterministic role assignments when Status is "configured".
		Assignments []Assignment `json:"assignments"`
		// CrossFamilyReview is true if and only if Status is "configured" and the
		// @reviewer role's Family is disjoint from every assigned @builder role's Family.
		CrossFamilyReview bool `json:"cross_family_review"`
		// CrossFamilyEval is true if and only if Status is "configured", an
		// @eval-author role is assigned, and the @eval-author role's Family is
		// disjoint from every assigned @builder role's Family.
		CrossFamilyEval bool `json:"cross_family_eval"`
		// SingleFamily is true when Status is "configured" and all assigned roles
		// use the same model Family.
		SingleFamily bool `json:"single_family"`
		// MissingRoles lists requested roles that lacked an authenticated capable
		// profile when Status is "unconfigured" (with Builders supplied) or
		// "missing_capability"; nil on solo previews (len(Builders) == 0).
		MissingRoles []RoleKind `json:"missing_roles,omitempty"`
		// Summary is a plain-English description of the proposed roster and
		// cross-family or single-family status.
		Summary string `json:"summary"`
	}
)

const (
	// RoleArchitect owns the Goa/Goa-AI/Model DSL contract at CP1 and CP2.
	RoleArchitect RoleKind = "architect"
	// RoleBuilder implements Go service and agent code for an assigned scope.
	RoleBuilder RoleKind = "builder"
	// RoleEvalAuthor wires scenario hooks and simulators in the declared evaluator scope.
	RoleEvalAuthor RoleKind = "eval_author"
	// RoleReviewer performs a zero-sunk-cost read-only adversarial review before CP4.
	RoleReviewer RoleKind = "reviewer"

	// StatusConfigured indicates all requested roles have authenticated capable profiles assigned.
	StatusConfigured = "configured"
	// StatusUnconfigured indicates either a solo preview (no builders supplied) or no authenticated provider profiles.
	StatusUnconfigured = "unconfigured"
	// StatusMissingCapability indicates at least one requested role has no authenticated capable profile.
	StatusMissingCapability = "missing_capability"

	// AccessReadOnlyAfterCP2 indicates @architect's design files are frozen during the build phase.
	AccessReadOnlyAfterCP2 = "read_only_after_cp2"
	// AccessReadWriteWorktree indicates an operator-declared per-builder Git worktree scope.
	AccessReadWriteWorktree = "read_write_worktree"
	// AccessIsolatedEvalWorkspace indicates an operator-declared evaluator workspace coordination scope.
	AccessIsolatedEvalWorkspace = "isolated_eval_workspace"
	// AccessReadOnlyReview indicates a fresh read-only review context.
	AccessReadOnlyReview = "read_only_review"

	// maxChorusHandleLen is the maximum length of a Chorus agent handle (`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`).
	maxChorusHandleLen = 32
)

// Plan computes a deterministic proposed swarm role roster and model routing
// plan from server-configured provider profiles and explicit workspace scopes.
//
// When no builders are supplied (and ArchitectPaths and EvalPaths are empty),
// Plan returns a solo preview PlanResult with Status "unconfigured" and
// MissingRoles nil. When builders are supplied, Plan derives the requested
// roles from the supplied scopes (RoleBuilder and RoleReviewer, plus
// RoleArchitect when ArchitectPaths is non-empty and RoleEvalAuthor when
// EvalPaths is non-empty). Plan returns an error only when the supplied
// profiles, scopes, or path globs are malformed (such as duplicate profile IDs,
// invalid path globs, or ArchitectPaths/EvalPaths without Builders).
func Plan(req Request) (PlanResult, error) {
	profiles, err := validateAndSortProfiles(req.Profiles)
	if err != nil {
		return PlanResult{}, err
	}
	builders, err := normalizeBuilders(req.Builders)
	if err != nil {
		return PlanResult{}, err
	}
	archPaths, err := normalizePaths(req.ArchitectPaths)
	if err != nil {
		return PlanResult{}, fmt.Errorf("architect_paths: %w", err)
	}
	evalPaths, err := normalizePaths(req.EvalPaths)
	if err != nil {
		return PlanResult{}, fmt.Errorf("eval_paths: %w", err)
	}

	if len(builders) == 0 {
		if len(archPaths) > 0 || len(evalPaths) > 0 {
			return PlanResult{}, errors.New("architect_paths and eval_paths require at least one builder scope")
		}
		return PlanResult{
			Status:       StatusUnconfigured,
			MissingRoles: nil,
			Summary:      "Swarm roster: off (solo workflow; proceed directly to build and CP4 verification, or pass builder scopes with -swarm-profiles to propose a Chorus roster).",
		}, nil
	}

	hasArchitect := len(archPaths) > 0
	hasEvalAuthor := len(evalPaths) > 0
	requestedRoles := make([]RoleKind, 0, 4)
	if hasArchitect {
		requestedRoles = append(requestedRoles, RoleArchitect)
	}
	requestedRoles = append(requestedRoles, RoleBuilder)
	if hasEvalAuthor {
		requestedRoles = append(requestedRoles, RoleEvalAuthor)
	}
	requestedRoles = append(requestedRoles, RoleReviewer)

	var eligible []ProviderProfile
	for _, p := range profiles {
		if p.Authenticated {
			eligible = append(eligible, p)
		}
	}
	if len(eligible) == 0 {
		names := roleKindStrings(requestedRoles)
		return PlanResult{
			Status:       StatusUnconfigured,
			MissingRoles: slices.Clone(requestedRoles),
			Summary: fmt.Sprintf(
				"Proposed swarm: unconfigured (builder scopes supplied without authenticated coding-agent profiles; configure -swarm-profiles supporting %s or omit builder scopes for a solo preview).",
				strings.Join(names, ", "),
			),
		}, nil
	}

	var missing []RoleKind
	for _, role := range requestedRoles {
		if len(profilesWithCapability(eligible, role)) == 0 {
			missing = append(missing, role)
		}
	}
	if len(missing) > 0 {
		names := roleKindStrings(missing)
		return PlanResult{
			Status:       StatusMissingCapability,
			MissingRoles: missing,
			Summary: fmt.Sprintf(
				"Proposed swarm: missing_capability (no authenticated profile supports requested role(s): %s).",
				strings.Join(names, ", "),
			),
		}, nil
	}

	builderProfiles, err := selectBuilderProfiles(eligible, builders, hasEvalAuthor)
	if err != nil {
		return PlanResult{}, err
	}
	builderFamilies := make(map[string]bool, len(builderProfiles))
	for _, bp := range builderProfiles {
		builderFamilies[bp.Family] = true
	}

	reviewerProfile := selectDisjointOrFirst(eligible, RoleReviewer, builderFamilies)
	builderHandles := allocateBuilderHandles(builders)
	assignments := make([]Assignment, 0, len(builders)+3)
	if hasArchitect {
		archProfile := selectDisjointOrFirst(eligible, RoleArchitect, builderFamilies, reviewerProfile.Family)
		assignments = append(assignments, Assignment{
			Handle:     "architect",
			Role:       RoleArchitect,
			ScopeName:  "design",
			ProfileID:  archProfile.ID,
			Provider:   archProfile.Provider,
			Model:      archProfile.Model,
			Family:     archProfile.Family,
			OwnedPaths: archPaths,
			AccessMode: AccessReadOnlyAfterCP2,
		})
	}
	for i, b := range builders {
		bp := builderProfiles[i]
		assignments = append(assignments, Assignment{
			Handle:     builderHandles[i],
			Role:       RoleBuilder,
			ScopeName:  b.Name,
			ProfileID:  bp.ID,
			Provider:   bp.Provider,
			Model:      bp.Model,
			Family:     bp.Family,
			OwnedPaths: b.Paths,
			AccessMode: AccessReadWriteWorktree,
		})
	}
	var evalProfile ProviderProfile
	if hasEvalAuthor {
		evalProfile = selectDisjointOrFirst(eligible, RoleEvalAuthor, builderFamilies, reviewerProfile.Family)
		assignments = append(assignments, Assignment{
			Handle:     "eval-author",
			Role:       RoleEvalAuthor,
			ScopeName:  "eval",
			ProfileID:  evalProfile.ID,
			Provider:   evalProfile.Provider,
			Model:      evalProfile.Model,
			Family:     evalProfile.Family,
			OwnedPaths: evalPaths,
			AccessMode: AccessIsolatedEvalWorkspace,
		})
	}
	assignments = append(assignments, Assignment{
		Handle:     "reviewer",
		Role:       RoleReviewer,
		ScopeName:  "review",
		ProfileID:  reviewerProfile.ID,
		Provider:   reviewerProfile.Provider,
		Model:      reviewerProfile.Model,
		Family:     reviewerProfile.Family,
		OwnedPaths: []string{"*"},
		AccessMode: AccessReadOnlyReview,
	})

	crossReview := !builderFamilies[reviewerProfile.Family]
	crossEval := hasEvalAuthor && !builderFamilies[evalProfile.Family]

	allFamilies := make(map[string]bool, len(assignments))
	for _, a := range assignments {
		allFamilies[a.Family] = true
	}
	singleFamily := len(allFamilies) == 1

	summary := formatPlanSummary(assignments, hasEvalAuthor, crossReview, crossEval, singleFamily)
	return PlanResult{
		Status:            StatusConfigured,
		Assignments:       assignments,
		CrossFamilyReview: crossReview,
		CrossFamilyEval:   crossEval,
		SingleFamily:      singleFamily,
		Summary:           summary,
	}, nil
}

// FormatManifest renders the deterministic Chorus Collaboration Manifest text
// for a configured PlanResult. Formatting is pure and does not claim that the
// manifest has been published to Chorus or that Surla enforces agent filesystem
// scopes outside CP4 verification.
func FormatManifest(changeID, designDigest string, plan PlanResult) (string, error) {
	if plan.Status != StatusConfigured || len(plan.Assignments) == 0 {
		return "", fmt.Errorf("swarm plan is not configured (status=%q): %s", plan.Status, plan.Summary)
	}
	if strings.TrimSpace(changeID) == "" {
		return "", errors.New("change_id is required to format swarm manifest")
	}
	if strings.TrimSpace(designDigest) == "" {
		return "", errors.New("design_digest is required to format swarm manifest")
	}

	hasEvalAuthor := false
	for _, a := range plan.Assignments {
		if a.Role == RoleEvalAuthor {
			hasEvalAuthor = true
			break
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[SURLA SWARM MANIFEST | change=%s | design=%s]\n", strings.TrimSpace(changeID), strings.TrimSpace(designDigest))
	if hasEvalAuthor {
		fmt.Fprintf(
			&b,
			"Status: proposed/configured (cross_family_review=%t, cross_family_eval=%t, single_family=%t)\n",
			plan.CrossFamilyReview,
			plan.CrossFamilyEval,
			plan.SingleFamily,
		)
	} else {
		fmt.Fprintf(
			&b,
			"Status: proposed/configured (cross_family_review=%t, cross_family_eval=%t [eval-author not assigned], single_family=%t)\n",
			plan.CrossFamilyReview,
			plan.CrossFamilyEval,
			plan.SingleFamily,
		)
	}
	b.WriteString("Boundary Note: Owned paths and access modes are operator-declared Chorus coordination scopes (unverified by Surla, not OS isolation); in-repo eval/ is readable by builders, while unseen holdouts require an external evaluator workspace inaccessible to builders, and CP4 runs candidate builds/processes in verify/candidate's OS sandbox while executing the trusted evaluator binary on the host.\n")
	b.WriteString("Roles & Workspace Boundaries:\n")
	for _, a := range plan.Assignments {
		fmt.Fprintf(
			&b,
			"- @%s (%s | profile=%s | model=%s | family=%s | mode=%s): %s\n",
			a.Handle,
			a.Role,
			a.ProfileID,
			a.Model,
			a.Family,
			a.AccessMode,
			strings.Join(a.OwnedPaths, ", "),
		)
	}
	return b.String(), nil
}

// roleKindStrings converts a slice of RoleKind values to their string names.
func roleKindStrings(roles []RoleKind) []string {
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i] = string(r)
	}
	return out
}

// validateAndSortProfiles validates all configured ProviderProfile entries,
// rejects duplicate profile IDs or empty metadata, and returns a canonically
// sorted copy so Plan is invariant to input slice order.
func validateAndSortProfiles(in []ProviderProfile) ([]ProviderProfile, error) {
	if len(in) == 0 {
		return nil, nil
	}
	seenIDs := make(map[string]bool, len(in))
	out := make([]ProviderProfile, 0, len(in))
	for i, p := range in {
		id := strings.TrimSpace(p.ID)
		provider := strings.TrimSpace(p.Provider)
		modelName := strings.TrimSpace(p.Model)
		family := strings.ToLower(strings.TrimSpace(p.Family))
		if id == "" || provider == "" || modelName == "" || family == "" {
			return nil, fmt.Errorf("profiles[%d] requires non-empty id, provider, model, and family", i)
		}
		if seenIDs[id] {
			return nil, fmt.Errorf("duplicate provider profile id %q", id)
		}
		seenIDs[id] = true

		caps := make([]RoleKind, 0, len(p.Capabilities))
		seenCaps := make(map[RoleKind]bool, len(p.Capabilities))
		for _, c := range p.Capabilities {
			switch c {
			case RoleArchitect, RoleBuilder, RoleEvalAuthor, RoleReviewer:
				if !seenCaps[c] {
					seenCaps[c] = true
					caps = append(caps, c)
				}
			default:
				return nil, fmt.Errorf("profile %q declares unsupported capability %q", id, c)
			}
		}
		slices.Sort(caps)
		out = append(out, ProviderProfile{
			ID:            id,
			Provider:      provider,
			Model:         modelName,
			Family:        family,
			Authenticated: p.Authenticated,
			Capabilities:  caps,
		})
	}
	slices.SortFunc(out, func(a, b ProviderProfile) int {
		if a.Family != b.Family {
			return strings.Compare(a.Family, b.Family)
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// normalizeBuilders validates and canonically sorts BuilderScope entries so
// Plan is invariant to input slice order. When in is empty, normalizeBuilders
// returns nil without inventing guessed ownership paths.
func normalizeBuilders(in []BuilderScope) ([]BuilderScope, error) {
	if len(in) == 0 {
		return nil, nil
	}
	seenNames := make(map[string]bool, len(in))
	out := make([]BuilderScope, 0, len(in))
	for i, b := range in {
		name := strings.TrimSpace(b.Name)
		if name == "" {
			return nil, fmt.Errorf("builders[%d] requires non-empty name", i)
		}
		if seenNames[name] {
			return nil, fmt.Errorf("duplicate builder scope name %q", name)
		}
		seenNames[name] = true
		paths, err := normalizePaths(b.Paths)
		if err != nil {
			return nil, fmt.Errorf("builder scope %q: %w", name, err)
		}
		if len(paths) == 0 {
			return nil, fmt.Errorf("builder scope %q requires at least one owned path", name)
		}
		out = append(out, BuilderScope{
			Name:               name,
			Paths:              paths,
			PreferredProfileID: strings.TrimSpace(b.PreferredProfileID),
		})
	}
	slices.SortFunc(out, func(a, b BuilderScope) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// normalizePaths validates repository-relative path globs without rewriting
// supplied path bytes, deduplicates and sorts them, and returns nil when in is
// empty.
func normalizePaths(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(in))
	var out []string
	for _, p := range in {
		if err := validateRelativePathGlob(p); err != nil {
			return nil, err
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out, nil
}

// validateRelativePathGlob verifies that p is a non-empty repository-relative
// path glob without NUL bytes, leading slashes, volume prefixes, backslashes,
// or `.` / `..` path traversal segments.
func validateRelativePathGlob(p string) error {
	if p == "" || strings.TrimSpace(p) == "" {
		return errors.New("ownership path must not be empty")
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("ownership path %q must not contain NUL bytes", p)
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || (len(p) >= 2 && p[1] == ':') {
		return fmt.Errorf("ownership path %q must be repository-relative", p)
	}
	segs := strings.Split(p, "/")
	if slices.Contains(segs, "") || slices.Contains(segs, ".") || slices.Contains(segs, "..") {
		return fmt.Errorf("ownership path %q must not contain empty, current-directory, or parent-directory segments", p)
	}
	return nil
}

// profilesWithCapability filters profiles to those advertising role.
func profilesWithCapability(profiles []ProviderProfile, role RoleKind) []ProviderProfile {
	var out []ProviderProfile
	for _, p := range profiles {
		if slices.Contains(p.Capabilities, role) {
			out = append(out, p)
		}
	}
	return out
}

// selectBuilderProfiles chooses an authenticated builder-capable profile for
// each BuilderScope, resolving pinned builder profiles first and scoring the
// default builder family against the union of pinned and unpinned builder
// families so @reviewer (and @eval-author when requested) retain disjoint
// families whenever possible.
func selectBuilderProfiles(eligible []ProviderProfile, builders []BuilderScope, hasEvalAuthor bool) ([]ProviderProfile, error) {
	builderCapable := profilesWithCapability(eligible, RoleBuilder)
	reviewerCapable := profilesWithCapability(eligible, RoleReviewer)
	var evalCapable []ProviderProfile
	if hasEvalAuthor {
		evalCapable = profilesWithCapability(eligible, RoleEvalAuthor)
	}

	byID := make(map[string]ProviderProfile, len(builderCapable))
	for _, cand := range builderCapable {
		byID[cand.ID] = cand
	}

	out := make([]ProviderProfile, len(builders))
	pinnedFamilies := make(map[string]bool, len(builders))
	hasUnpinned := false
	for i, b := range builders {
		if b.PreferredProfileID != "" {
			prof, ok := byID[b.PreferredProfileID]
			if !ok {
				return nil, fmt.Errorf("builder %q preferred_profile_id %q is not an authenticated builder-capable profile", b.Name, b.PreferredProfileID)
			}
			out[i] = prof
			pinnedFamilies[prof.Family] = true
			continue
		}
		hasUnpinned = true
	}
	if !hasUnpinned {
		return out, nil
	}

	// Score each candidate default builder family over the union of already-pinned
	// builder families and cand.Family so unpinned builders align with pinned
	// builders when doing so preserves a disjoint family for @reviewer/@eval-author.
	var chosenDefault ProviderProfile
	bestScore := -1
	for _, cand := range builderCapable {
		score := 0
		if hasDisjointFamily(reviewerCapable, pinnedFamilies, cand.Family) {
			score += 4
		}
		if hasEvalAuthor && hasDisjointFamily(evalCapable, pinnedFamilies, cand.Family) {
			score += 2
		}
		if pinnedFamilies[cand.Family] {
			score++
		}
		if score > bestScore {
			bestScore = score
			chosenDefault = cand
		}
	}

	var familyPool []ProviderProfile
	for _, cand := range builderCapable {
		if cand.Family == chosenDefault.Family {
			familyPool = append(familyPool, cand)
		}
	}

	unpinnedIdx := 0
	for i, b := range builders {
		if b.PreferredProfileID != "" {
			continue
		}
		out[i] = familyPool[unpinnedIdx%len(familyPool)]
		unpinnedIdx++
	}
	return out, nil
}

// hasDisjointFamily reports whether any profile in candidates has a Family
// distinct from candFamily and absent from pinnedFamilies.
func hasDisjointFamily(candidates []ProviderProfile, pinnedFamilies map[string]bool, candFamily string) bool {
	for _, c := range candidates {
		if c.Family != candFamily && !pinnedFamilies[c.Family] {
			return true
		}
	}
	return false
}

// selectDisjointOrFirst chooses a profile capable of role whose Family is not
// in avoidFamilies (and, when possible, differs from secondaryAvoidFamily),
// falling back deterministically to the first capable profile.
func selectDisjointOrFirst(
	eligible []ProviderProfile,
	role RoleKind,
	avoidFamilies map[string]bool,
	secondaryAvoidFamily ...string,
) ProviderProfile {
	candidates := profilesWithCapability(eligible, role)
	var secondary string
	if len(secondaryAvoidFamily) > 0 {
		secondary = secondaryAvoidFamily[0]
	}
	for _, c := range candidates {
		if !avoidFamilies[c.Family] && (secondary == "" || c.Family != secondary) {
			return c
		}
	}
	for _, c := range candidates {
		if !avoidFamilies[c.Family] {
			return c
		}
	}
	return candidates[0]
}

// allocateBuilderHandles generates deterministic, collision-free Chorus agent
// handles (`<= 32` characters, matching `^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
// for each BuilderScope.
func allocateBuilderHandles(builders []BuilderScope) []string {
	reserved := map[string]bool{
		"architect":   true,
		"eval-author": true,
		"reviewer":    true,
	}
	handles := make([]string, len(builders))
	for i, b := range builders {
		slug := sanitizeSlug(b.Name)
		if slug == "" {
			slug = "service"
		}
		base := "builder-" + slug
		if len(base) > maxChorusHandleLen {
			base = strings.TrimRight(base[:maxChorusHandleLen], "-_")
		}
		candidate := base
		for suffix := 2; reserved[candidate]; suffix++ {
			sfx := fmt.Sprintf("-%d", suffix)
			maxPrefix := maxChorusHandleLen - len(sfx)
			prefix := base
			if len(prefix) > maxPrefix {
				prefix = strings.TrimRight(prefix[:maxPrefix], "-_")
			}
			candidate = prefix + sfx
		}
		reserved[candidate] = true
		handles[i] = candidate
	}
	return handles
}

// sanitizeSlug converts an arbitrary scope name into a lowercase alphanumeric
// hyphen-separated slug suitable for a Chorus handle suffix.
func sanitizeSlug(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		default:
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// formatPlanSummary builds the honest plain-English summary of a configured
// PlanResult, explicitly distinguishing cross-family vs. single-family routing,
// reporting whether @eval-author was assigned, and stating that Chorus path
// scopes are operator-declared coordination boundaries while CP4 sandboxes
// candidate builds and processes in verify/candidate.
func formatPlanSummary(assignments []Assignment, hasEvalAuthor, crossReview, crossEval, singleFamily bool) string {
	parts := make([]string, 0, len(assignments))
	for _, a := range assignments {
		parts = append(parts, fmt.Sprintf("@%s (%s/%s, family=%s)", a.Handle, a.Provider, a.Model, a.Family))
	}
	var posture string
	switch {
	case singleFamily && hasEvalAuthor:
		posture = fmt.Sprintf(
			"single-family routing (all roles use family %q; cross_family_review=false, cross_family_eval=false; Chorus path scopes are operator-declared coordination boundaries, and unseen holdouts require an external evaluator workspace inaccessible to builders while CP4 sandboxes candidate builds/processes in verify/candidate)",
			assignments[0].Family,
		)
	case singleFamily:
		posture = fmt.Sprintf(
			"single-family routing (all roles use family %q; cross_family_review=false, eval-author not assigned in this roster; Chorus path scopes are operator-declared coordination boundaries while CP4 sandboxes candidate builds/processes in verify/candidate)",
			assignments[0].Family,
		)
	case crossReview && crossEval:
		posture = "cross-family review and eval-author routing configured (reviewer and eval-author model families are disjoint from all builders; Chorus path scopes are operator-declared coordination boundaries, and unseen holdouts require an external evaluator workspace inaccessible to builders while CP4 sandboxes candidate builds/processes in verify/candidate)"
	case crossReview && !hasEvalAuthor:
		posture = "cross-family review configured (reviewer family is disjoint from all builders; eval-author not assigned in this roster; Chorus path scopes are operator-declared coordination boundaries while CP4 sandboxes candidate builds/processes in verify/candidate)"
	case crossReview:
		posture = "cross-family review configured (reviewer family is disjoint from all builders; eval-author shares a builder family; Chorus path scopes are operator-declared coordination boundaries, and unseen holdouts require an external evaluator workspace inaccessible to builders while CP4 sandboxes candidate builds/processes in verify/candidate)"
	case !hasEvalAuthor:
		posture = "mixed-family routing without disjoint global reviewer (reviewer shares a model family with at least one builder; cross_family_review=false, eval-author not assigned in this roster; Chorus path scopes are operator-declared coordination boundaries while CP4 sandboxes candidate builds/processes in verify/candidate)"
	default:
		posture = "mixed-family routing without disjoint global reviewer (reviewer shares a model family with at least one builder; cross_family_review=false; Chorus path scopes are operator-declared coordination boundaries, and unseen holdouts require an external evaluator workspace inaccessible to builders while CP4 sandboxes candidate builds/processes in verify/candidate)"
	}
	return fmt.Sprintf("Proposed swarm (%s): %s.", posture, strings.Join(parts, ", "))
}
