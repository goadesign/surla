// Package swarm tests deterministic role planning, cross-family model routing,
// handle collision safety, and Chorus manifest formatting.
package swarm

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	// testChorusHandlePattern matches valid Chorus agent handles (`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`).
	testChorusHandlePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
)

// TestPlanCrossFamilyRoutingOrderInvarianceAndManifestFormatting verifies
// deterministic multi-family role assignment, input-order invariance, and
// Chorus manifest formatting.
func TestPlanCrossFamilyRoutingOrderInvarianceAndManifestFormatting(t *testing.T) {
	t.Parallel()

	profiles := []ProviderProfile{
		{
			ID:            "claude-opus",
			Provider:      "anthropic",
			Model:         "claude-opus-4-6",
			Family:        "claude",
			Authenticated: true,
			Capabilities:  allRoles(),
		},
		{
			ID:            "codex-gpt5",
			Provider:      "openai",
			Model:         "gpt-5.2-codex",
			Family:        "gpt",
			Authenticated: true,
			Capabilities:  allRoles(),
		},
		{
			ID:            "gemini-argon",
			Provider:      "google",
			Model:         "gemini-4-argon",
			Family:        "gemini",
			Authenticated: true,
			Capabilities:  allRoles(),
		},
	}
	builders := []BuilderScope{
		{Name: "orders", Paths: []string{"internal/orders/**", "cmd/orders/**"}},
		{Name: "refundflow", Paths: []string{"internal/refundflow/**", "cmd/orders-worker/**"}},
	}

	planForward, err := Plan(Request{
		ChangeID:       "chg-101",
		DesignDigest:   "sha256:abc123",
		Profiles:       profiles,
		ArchitectPaths: []string{"design/**"},
		Builders:       builders,
		EvalPaths:      []string{"*-eval/**"},
	})
	require.NoError(t, err)
	assert.Equal(t, StatusConfigured, planForward.Status)
	assert.True(t, planForward.CrossFamilyReview)
	assert.True(t, planForward.CrossFamilyEval)
	assert.False(t, planForward.SingleFamily)
	require.Len(t, planForward.Assignments, 5)

	// Reversing input slices must produce the exact same PlanResult and Manifest.
	reversedProfiles := slicesCloneReverse(profiles)
	reversedBuilders := slicesCloneReverse(builders)
	planReverse, err := Plan(Request{
		ChangeID:       "chg-101",
		DesignDigest:   "sha256:abc123",
		Profiles:       reversedProfiles,
		ArchitectPaths: []string{"design/**"},
		Builders:       reversedBuilders,
		EvalPaths:      []string{"*-eval/**"},
	})
	require.NoError(t, err)
	assert.Equal(t, planForward, planReverse)

	manifest1, err := FormatManifest("chg-101", "sha256:abc123", planForward)
	require.NoError(t, err)
	manifest2, err := FormatManifest("chg-101", "sha256:abc123", planReverse)
	require.NoError(t, err)
	assert.Equal(t, manifest1, manifest2)
	assert.Contains(t, manifest1, "Status: proposed/configured (cross_family_review=true, cross_family_eval=true, single_family=false)")
	assert.Contains(t, manifest1, "Boundary Note: Owned paths and access modes are operator-declared Chorus coordination scopes")
	assert.Contains(t, manifest1, "@builder-orders")
	assert.Contains(t, manifest1, "@builder-refundflow")

	// Progressive roster sizing on the exact same full-capability server profiles:
	// 1. Builders only -> 2-role roster (@builder-* + @reviewer), CrossFamilyEval=false, honest "eval-author not assigned".
	plan2Role, err := Plan(Request{
		ChangeID:     "chg-101",
		DesignDigest: "sha256:abc123",
		Profiles:     profiles,
		Builders:     builders,
	})
	require.NoError(t, err)
	assert.Equal(t, StatusConfigured, plan2Role.Status)
	assert.True(t, plan2Role.CrossFamilyReview)
	assert.False(t, plan2Role.CrossFamilyEval)
	assert.False(t, plan2Role.SingleFamily)
	assert.Contains(t, plan2Role.Summary, "eval-author not assigned in this roster")
	assert.NotContains(t, plan2Role.Summary, "eval-author shares a builder family")
	require.Len(t, plan2Role.Assignments, 3)
	assert.Equal(t, RoleBuilder, plan2Role.Assignments[0].Role)
	assert.Equal(t, RoleBuilder, plan2Role.Assignments[1].Role)
	assert.Equal(t, RoleReviewer, plan2Role.Assignments[2].Role)
	manifest2Role, err := FormatManifest("chg-101", "sha256:abc123", plan2Role)
	require.NoError(t, err)
	assert.Contains(t, manifest2Role, "cross_family_eval=false [eval-author not assigned]")

	// 2. Builders + ArchitectPaths only -> 3-role roster (@architect + @builder-* + @reviewer).
	plan3RoleArch, err := Plan(Request{
		ChangeID:       "chg-101",
		DesignDigest:   "sha256:abc123",
		Profiles:       profiles,
		ArchitectPaths: []string{"design/**"},
		Builders:       builders[:1],
	})
	require.NoError(t, err)
	require.Len(t, plan3RoleArch.Assignments, 3)
	assert.Equal(t, RoleArchitect, plan3RoleArch.Assignments[0].Role)
	assert.Equal(t, RoleBuilder, plan3RoleArch.Assignments[1].Role)
	assert.Equal(t, RoleReviewer, plan3RoleArch.Assignments[2].Role)
	assert.False(t, plan3RoleArch.CrossFamilyEval)
	assert.Contains(t, plan3RoleArch.Summary, "eval-author not assigned in this roster")

	// 3. Builders + EvalPaths only -> 3-role roster (@builder-* + @eval-author + @reviewer).
	plan3RoleEval, err := Plan(Request{
		ChangeID:     "chg-101",
		DesignDigest: "sha256:abc123",
		Profiles:     profiles,
		Builders:     builders[:1],
		EvalPaths:    []string{"eval/**"},
	})
	require.NoError(t, err)
	require.Len(t, plan3RoleEval.Assignments, 3)
	assert.Equal(t, RoleBuilder, plan3RoleEval.Assignments[0].Role)
	assert.Equal(t, RoleEvalAuthor, plan3RoleEval.Assignments[1].Role)
	assert.Equal(t, RoleReviewer, plan3RoleEval.Assignments[2].Role)
	assert.True(t, plan3RoleEval.CrossFamilyEval)
}

// TestPlanCounterexamplesSingleFamilyUnauthenticatedAndMixedBuilders verifies
// single-family routing, unauthenticated profile filtering, mixed-family
// builder counterexamples, missing capability reporting, unconfigured fallback,
// duplicate profile ID rejection, and Chorus handle length/collision bounds.
func TestPlanCounterexamplesSingleFamilyUnauthenticatedAndMixedBuilders(t *testing.T) {
	t.Parallel()

	// Case 1: 2 installed CLIs, but only 1 is authenticated.
	t.Run("two_installed_only_one_authenticated", func(t *testing.T) {
		t.Parallel()
		plan, err := Plan(Request{
			ChangeID:     "chg-1",
			DesignDigest: "sha256:1",
			Profiles: []ProviderProfile{
				{
					ID:            "claude-opus",
					Provider:      "anthropic",
					Model:         "claude-opus-4-6",
					Family:        "claude",
					Authenticated: true,
					Capabilities:  allRoles(),
				},
				{
					ID:            "codex-unauth",
					Provider:      "openai",
					Model:         "gpt-5.2-codex",
					Family:        "gpt",
					Authenticated: false,
					Capabilities:  allRoles(),
				},
			},
			Builders: []BuilderScope{
				{Name: "orders", Paths: []string{"orders.go", "internal/orders/**"}},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, StatusConfigured, plan.Status)
		assert.True(t, plan.SingleFamily)
		assert.False(t, plan.CrossFamilyReview)
		assert.False(t, plan.CrossFamilyEval)
		assert.Contains(t, plan.Summary, "single-family routing")
		for _, a := range plan.Assignments {
			assert.Equal(t, "claude", a.Family)
		}
	})

	// Case 2: 2 authenticated profiles from the SAME model family (must never be labeled cross-family).
	t.Run("two_profiles_same_family_never_labeled_cross_family", func(t *testing.T) {
		t.Parallel()
		plan, err := Plan(Request{
			ChangeID:     "chg-2",
			DesignDigest: "sha256:2",
			Profiles: []ProviderProfile{
				{
					ID:            "claude-opus",
					Provider:      "anthropic",
					Model:         "claude-opus-4-6",
					Family:        "claude",
					Authenticated: true,
					Capabilities:  allRoles(),
				},
				{
					ID:            "claude-sonnet",
					Provider:      "anthropic",
					Model:         "claude-sonnet-4-6",
					Family:        "claude",
					Authenticated: true,
					Capabilities:  allRoles(),
				},
			},
			Builders: []BuilderScope{
				{Name: "orders", Paths: []string{"orders.go", "internal/orders/**"}},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, StatusConfigured, plan.Status)
		assert.True(t, plan.SingleFamily)
		assert.False(t, plan.CrossFamilyReview)
		assert.False(t, plan.CrossFamilyEval)
		assert.Contains(t, plan.Summary, "single-family routing")
	})

	// Case 2b: Pinned builder in family B + unpinned builder with profiles A and B
	// aligns unpinned builder with family B so family A stays disjoint for review/eval.
	t.Run("pinned_builder_family_aligns_unpinned_builder_to_preserve_cross_family_review", func(t *testing.T) {
		t.Parallel()
		plan, err := Plan(Request{
			ChangeID:     "chg-2b",
			DesignDigest: "sha256:2b",
			Profiles: []ProviderProfile{
				{
					ID:            "claude-opus",
					Provider:      "anthropic",
					Model:         "claude-opus-4-6",
					Family:        "claude",
					Authenticated: true,
					Capabilities:  allRoles(),
				},
				{
					ID:            "codex-gpt5",
					Provider:      "openai",
					Model:         "gpt-5.2-codex",
					Family:        "gpt",
					Authenticated: true,
					Capabilities:  allRoles(),
				},
			},
			Builders: []BuilderScope{
				{Name: "api", Paths: []string{"cmd/orders/**"}, PreferredProfileID: "codex-gpt5"},
				{Name: "orders", Paths: []string{"internal/orders/**"}},
			},
			EvalPaths: []string{"*-eval/**"},
		})
		require.NoError(t, err)
		assert.Equal(t, StatusConfigured, plan.Status)
		assert.True(t, plan.CrossFamilyReview)
		assert.True(t, plan.CrossFamilyEval)
		assert.False(t, plan.SingleFamily)
	})

	// Case 3: Mixed-family builders forced across the only 2 families with 1 global reviewer.
	// Because reviewer shares a family with one of the builders, CrossFamilyReview must be false.
	t.Run("mixed_family_builders_with_two_total_families_reports_false_cross_review", func(t *testing.T) {
		t.Parallel()
		plan, err := Plan(Request{
			ChangeID:     "chg-3",
			DesignDigest: "sha256:3",
			Profiles: []ProviderProfile{
				{
					ID:            "claude-opus",
					Provider:      "anthropic",
					Model:         "claude-opus-4-6",
					Family:        "claude",
					Authenticated: true,
					Capabilities:  allRoles(),
				},
				{
					ID:            "codex-gpt5",
					Provider:      "openai",
					Model:         "gpt-5.2-codex",
					Family:        "gpt",
					Authenticated: true,
					Capabilities:  allRoles(),
				},
			},
			Builders: []BuilderScope{
				{Name: "api", Paths: []string{"internal/api/**"}, PreferredProfileID: "claude-opus"},
				{Name: "orders", Paths: []string{"internal/orders/**"}, PreferredProfileID: "codex-gpt5"},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, StatusConfigured, plan.Status)
		assert.False(t, plan.SingleFamily)
		assert.False(t, plan.CrossFamilyReview)
		assert.False(t, plan.CrossFamilyEval)
		assert.Contains(t, plan.Summary, "cross_family_review=false")
	})

	// Case 4: Missing requested capability returns explicit missing_capability status and blocks FormatManifest.
	t.Run("missing_requested_capability", func(t *testing.T) {
		t.Parallel()
		plan, err := Plan(Request{
			ChangeID:     "chg-4",
			DesignDigest: "sha256:4",
			Profiles: []ProviderProfile{
				{
					ID:            "builder-only",
					Provider:      "openai",
					Model:         "gpt-5.2-codex",
					Family:        "gpt",
					Authenticated: true,
					Capabilities:  []RoleKind{RoleArchitect, RoleBuilder, RoleEvalAuthor},
				},
			},
			Builders: []BuilderScope{
				{Name: "orders", Paths: []string{"internal/orders/**"}},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, StatusMissingCapability, plan.Status)
		assert.Equal(t, []RoleKind{RoleReviewer}, plan.MissingRoles)
		assert.Empty(t, plan.Assignments)
		_, err = FormatManifest("chg-4", "sha256:4", plan)
		assert.ErrorContains(t, err, "missing_capability")

		// A profile with only builder + reviewer capabilities succeeds on a 2-role request without requiring architect or eval_author.
		plan2Cap, err := Plan(Request{
			ChangeID:     "chg-4-2cap",
			DesignDigest: "sha256:4",
			Profiles: []ProviderProfile{
				{
					ID:            "builder-reviewer",
					Provider:      "openai",
					Model:         "gpt-5.2-codex",
					Family:        "gpt",
					Authenticated: true,
					Capabilities:  []RoleKind{RoleBuilder, RoleReviewer},
				},
			},
			Builders: []BuilderScope{
				{Name: "orders", Paths: []string{"internal/orders/**"}},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, StatusConfigured, plan2Cap.Status)
		require.Len(t, plan2Cap.Assignments, 2)
	})

	// Case 5: Solo preview (no builders, no architect_paths/eval_paths) returns unconfigured with nil MissingRoles.
	t.Run("solo_preview_without_builders_returns_unconfigured_with_nil_missing_roles", func(t *testing.T) {
		t.Parallel()
		planNoProfiles, err := Plan(Request{
			ChangeID:     "chg-5",
			DesignDigest: "sha256:5",
		})
		require.NoError(t, err)
		assert.Equal(t, StatusUnconfigured, planNoProfiles.Status)
		assert.Nil(t, planNoProfiles.MissingRoles)
		assert.Empty(t, planNoProfiles.Assignments)
		assert.Contains(t, planNoProfiles.Summary, "Swarm roster: off (solo workflow")
		_, err = FormatManifest("chg-5", "sha256:5", planNoProfiles)
		assert.ErrorContains(t, err, "unconfigured")

		planWithProfiles, err := Plan(Request{
			ChangeID:     "chg-5b",
			DesignDigest: "sha256:5b",
			Profiles: []ProviderProfile{
				{ID: "p1", Provider: "anthropic", Model: "m1", Family: "claude", Authenticated: true, Capabilities: allRoles()},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, StatusUnconfigured, planWithProfiles.Status)
		assert.Nil(t, planWithProfiles.MissingRoles)
		assert.Empty(t, planWithProfiles.Assignments)
		assert.Contains(t, planWithProfiles.Summary, "Swarm roster: off (solo workflow")
	})

	// Case 5b: Builders supplied without authenticated profiles returns unconfigured with requested roles in MissingRoles,
	// and architect_paths/eval_paths without builders is rejected.
	t.Run("builders_without_authenticated_profiles_and_orphan_scopes_rejected", func(t *testing.T) {
		t.Parallel()
		plan2Missing, err := Plan(Request{
			ChangeID:     "chg-5b-2",
			DesignDigest: "sha256:5b",
			Builders: []BuilderScope{
				{Name: "orders", Paths: []string{"internal/orders/**"}},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, StatusUnconfigured, plan2Missing.Status)
		assert.Equal(t, []RoleKind{RoleBuilder, RoleReviewer}, plan2Missing.MissingRoles)

		plan4Missing, err := Plan(Request{
			ChangeID:       "chg-5b-4",
			DesignDigest:   "sha256:5b",
			ArchitectPaths: []string{"design/**"},
			Builders: []BuilderScope{
				{Name: "orders", Paths: []string{"internal/orders/**"}},
			},
			EvalPaths: []string{"*-eval/**"},
		})
		require.NoError(t, err)
		assert.Equal(t, StatusUnconfigured, plan4Missing.Status)
		assert.Equal(t, allRoles(), plan4Missing.MissingRoles)

		_, err = Plan(Request{
			ChangeID:       "chg-orphan-arch",
			DesignDigest:   "sha256:orphan",
			ArchitectPaths: []string{"design/**"},
		})
		require.ErrorContains(t, err, "architect_paths and eval_paths require at least one builder scope")

		_, err = Plan(Request{
			ChangeID:     "chg-orphan-eval",
			DesignDigest: "sha256:orphan",
			EvalPaths:    []string{"eval/**"},
		})
		require.ErrorContains(t, err, "architect_paths and eval_paths require at least one builder scope")
	})

	// Case 5c: Absolute, parent-traversing, NUL-containing, or empty ownership paths are rejected,
	// while valid paths with significant spaces or commas are preserved verbatim.
	t.Run("invalid_paths_rejected_and_valid_spaces_and_commas_preserved", func(t *testing.T) {
		t.Parallel()
		for _, badPath := range []string{"", "   ", "/outside/**", "../outside/**", "internal/../../outside/**", "internal/orders\x00.go"} {
			_, err := Plan(Request{
				ChangeID:     "chg-bad-path",
				DesignDigest: "sha256:bad",
				Profiles: []ProviderProfile{
					{ID: "p1", Provider: "anthropic", Model: "m1", Family: "claude", Authenticated: true, Capabilities: allRoles()},
				},
				Builders: []BuilderScope{
					{Name: "orders", Paths: []string{"internal/orders/**", badPath}},
				},
			})
			require.Error(t, err, "expected path %q to be rejected", badPath)
		}

		planSpaces, err := Plan(Request{
			ChangeID:       "chg-spaces",
			DesignDigest:   "sha256:spaces",
			ArchitectPaths: []string{" design/a,b/**"},
			Profiles: []ProviderProfile{
				{ID: "p1", Provider: "anthropic", Model: "m1", Family: "claude", Authenticated: true, Capabilities: allRoles()},
			},
			Builders: []BuilderScope{
				{Name: "orders", Paths: []string{" reports/**"}},
			},
			EvalPaths: []string{"eval/a,b/**"},
		})
		require.NoError(t, err)
		require.Len(t, planSpaces.Assignments, 4)
		assert.Equal(t, []string{" design/a,b/**"}, planSpaces.Assignments[0].OwnedPaths)
		assert.Equal(t, []string{" reports/**"}, planSpaces.Assignments[1].OwnedPaths)
		assert.Equal(t, []string{"eval/a,b/**"}, planSpaces.Assignments[2].OwnedPaths)
	})

	// Case 6: Duplicate profile IDs and long/colliding builder names.
	t.Run("duplicate_profile_rejected_and_long_colliding_builder_handles_safe", func(t *testing.T) {
		t.Parallel()
		_, err := Plan(Request{
			ChangeID:     "chg-dup",
			DesignDigest: "sha256:dup",
			Profiles: []ProviderProfile{
				{ID: "dup", Provider: "anthropic", Model: "m1", Family: "claude", Authenticated: true, Capabilities: allRoles()},
				{ID: "dup", Provider: "openai", Model: "m2", Family: "gpt", Authenticated: true, Capabilities: allRoles()},
			},
		})
		require.ErrorContains(t, err, "duplicate provider profile id")

		plan, err := Plan(Request{
			ChangeID:     "chg-long",
			DesignDigest: "sha256:long",
			Profiles: []ProviderProfile{
				{ID: "p1", Provider: "anthropic", Model: "m1", Family: "claude", Authenticated: true, Capabilities: allRoles()},
			},
			Builders: []BuilderScope{
				{Name: "Customer Order Refund Processing Subsystem Alpha", Paths: []string{"internal/a/**"}},
				{Name: "Customer Order Refund Processing Subsystem Beta", Paths: []string{"internal/b/**"}},
			},
		})
		require.NoError(t, err)
		seenHandles := make(map[string]bool)
		for _, a := range plan.Assignments {
			assert.LessOrEqual(t, len(a.Handle), maxChorusHandleLen)
			assert.True(t, testChorusHandlePattern.MatchString(a.Handle), "invalid Chorus handle %q", a.Handle)
			assert.False(t, seenHandles[a.Handle], "colliding Chorus handle %q", a.Handle)
			seenHandles[a.Handle] = true
			assert.False(t, strings.HasSuffix(a.Handle, "-"))
		}
	})
}

// allRoles returns the complete slice of required Surla swarm roles.
func allRoles() []RoleKind {
	return []RoleKind{RoleArchitect, RoleBuilder, RoleEvalAuthor, RoleReviewer}
}

// slicesCloneReverse returns a new slice containing the elements of in in reverse order.
func slicesCloneReverse[T any](in []T) []T {
	out := make([]T, len(in))
	for i := range in {
		out[len(in)-1-i] = in[i]
	}
	return out
}
