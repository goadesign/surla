// Package design defines the Goa and Goa-AI MCP contract for the Surla
// software factory control plane.
package design

import (
	. "goa.design/goa-ai/dsl"
	. "goa.design/goa/v3/dsl"
)

var _ = API("surla", func() {
	Title("Surla MCP Software Factory Control Plane")
	Description("MCP-native control plane for creating, verifying, and evolving production Goa-AI agentic platforms.")
	Version("1.0.0")
})

// AmbiguityQuestion represents a genuine domain question surfaced at Checkpoint 1.
var AmbiguityQuestion = Type("AmbiguityQuestion", func() {
	Description("Domain ambiguity surfaced with concrete examples at Checkpoint 1.")
	Attribute("id", String, "Stable question identifier.")
	Attribute("question", String, "Plain-English question for the operator.")
	Attribute("options", ArrayOf(String), "Concrete resolution choices.")
	Required("id", "question", "options")
})

// ExampleItem represents a concrete input/outcome row in a Checkpoint 1 Change Card.
var ExampleItem = Type("ExampleItem", func() {
	Description("Concrete input and expected outcome confirmed at Checkpoint 1.")
	Attribute("id", String, "Code-assigned stable example identifier.")
	Attribute("scenario", String, "Plain-English scenario description.")
	Attribute("input_summary", String, "Representative user request and domain fixture state.")
	Attribute("expected_outcome", String, "Expected side effect and response behavior.")
	Attribute("verification_kind", String, "Evaluator check classification (deterministic or model_judged).", func() {
		Enum("deterministic", "model_judged")
	})
	Required("id", "scenario", "input_summary", "expected_outcome", "verification_kind")
})

// ProposeChangePayload is the input to the CP1 propose_change tool.
var ProposeChangePayload = Type("ProposeChangePayload", func() {
	Description("Operator feature request submitted for Checkpoint 1 ambiguity and example analysis.")
	Attribute("change_id", String, "Unique change identifier.")
	Attribute("prompt", String, "Operator feature request.")
	Attribute("design_source", String, "Current target Goa design source inspected by the CP1 model.")
	Required("change_id", "prompt")
})

// ReviseChangePayload is the input to the CP1 revise_change tool.
var ReviseChangePayload = Type("ReviseChangePayload", func() {
	Description("Revised operator feature request submitted for an existing Checkpoint 1 Change Card.")
	Attribute("change_id", String, "Change identifier.")
	Attribute("request_revision", String, "Current CP1 request_revision digest being revised.")
	Attribute("prompt", String, "Revised operator feature request.")
	Attribute("design_source", String, "Updated target Goa design source inspected by the CP1 model; when omitted, the card's saved design source is preserved.")
	Required("change_id", "request_revision", "prompt")
})

// GetChangePayload is the input to the get_change tool.
var GetChangePayload = Type("GetChangePayload", func() {
	Description("Identifies the Change Card the authenticated owner wants to read.")
	Attribute("change_id", String, "Change identifier.")
	Required("change_id")
})

// UICompositionItem is an optional model-authored decision view for one
// Change Card revision.
var UICompositionItem = Type("UICompositionItem", func() {
	Description("Model-authored OpenUI Lang decision view for exactly one Change Card revision. It is presentation only: it is never hashed into request_revision, confirm_intent never reads it, and it describes the card only while composed_for_revision equals the card's request_revision.")
	Attribute("program", String, "OpenUI Lang program composed by the CP1 model from the Surla decision component catalog, exactly as the model returned it. The server does not check it; the checkpoint viewer validates it and shows its error state, with the rest of the card still usable, when the program is not a valid composition.")
	Attribute("composed_for_revision", String, "request_revision of the Change Card this program was composed for.")
	Required("program", "composed_for_revision")
})

// ApprovalItem records a passed CP5 promotion check for one Change Card
// revision.
var ApprovalItem = Type("ApprovalItem", func() {
	Description("Checkpoint 5 approval recorded by approve_merge after the evaluator-written verification report passed and matched the change being promoted: the exact candidate commit, suite, environment and binary digests it checked. It is saved with the card in the control plane's cards directory, merges and deploys nothing, is replaced by the next successful approve_merge for the same revision, and is dropped when confirm_intent or revise_change mints a new request_revision. get_change and confirm_intent return it in approval only while the promotion target's current suite, environment and binary digests still equal the ones it records, or in unverified_approval when current promotion inputs cannot be verified on that read.")
	Attribute("approved_by", String, "Authenticated bearer principal that owns the card and called approve_merge. Bearer authentication does not prove that a human, rather than a coding agent, made the call.")
	Attribute("report_digest", String, "SHA-256 digest of the approved CP4 verification report.")
	Attribute("candidate_commit", String, "Candidate commit the report was checked against.")
	Attribute("suite_id", String, "Evaluator suite the promotion target requires.")
	Attribute("suite_commit", String, "Evaluator suite commit the promotion target requires.")
	Attribute("environment", String, "Dependency profile the promotion target requires, such as sim.")
	Attribute("artifacts", MapOf(String, String), "SHA-256 digest of each candidate binary the promotion target measured, keyed by binary name.")
	Attribute("scenario_count", Int, "Number of declared evaluator scenarios, every one of which passed.")
	Required("approved_by", "report_digest", "candidate_commit", "suite_id", "suite_commit", "environment", "artifacts", "scenario_count")
})

// UnverifiedApprovalItem wraps a saved CP5 approval whose current promotion
// inputs could not be verified on this read.
var UnverifiedApprovalItem = Type("UnverifiedApprovalItem", func() {
	Description("Saved Checkpoint 5 promotion approval recorded on the Change Card whose current promotion inputs could not be verified on this read.")
	Attribute("approval", ApprovalItem, "Recorded promotion approval from the Change Card.")
	Attribute("reason", String, "Explanation of why the recorded approval could not be verified against current promotion inputs and how to recover.")
	Required("approval", "reason")
})

// ChangeCardResult is the output of CP1 propose_change, revise_change, confirm_intent and get_change.
var ChangeCardResult = Type("ChangeCardResult", func() {
	Description("Checkpoint 1 Change Card containing examples, preserved behaviors, and audit metadata.")
	Attribute("change_id", String, "Change identifier.")
	Attribute("status", String, "Checkpoint status (needs_confirmation or confirmed).", func() {
		Enum("needs_confirmation", "confirmed")
	})
	Attribute("prompt", String, "Operator feature request for this revision.")
	Attribute("summary", String, "Plain-English summary of requested behavior.")
	Attribute("ambiguities", ArrayOf(AmbiguityQuestion), "Domain questions surfaced at CP1, retained for audit after confirmation.")
	Attribute("answers", MapOf(String, String), "Confirmed resolution per ambiguity question ID: each value is exactly one string from that question's options.")
	Attribute("examples", ArrayOf(ExampleItem), "Concrete examples mapped to evaluator checks.")
	Attribute("preserved_behaviors", ArrayOf(String), "Existing contract invariants preserved by this change.")
	Attribute("confirmed_by", String, "Authenticated principal that confirmed the Change Card.")
	Attribute("request_revision", String, "SHA-256 digest of the canonical versioned Change Card.")
	Attribute("ui_composition", UICompositionItem, "Decision view the CP1 model composed for this revision, when it produced one. A confirm_intent that changes answers or examples returns the new revision without it.")
	Attribute("approval", ApprovalItem, "Checkpoint 5 approval of this revision, present only while it is verified against the promotion target's current inputs on this read. It is absent before approve_merge succeeds, whenever the target's suite, environment or binary digests have changed since, and whenever current inputs cannot be verified (see unverified_approval). It is never hashed into request_revision.")
	Attribute("unverified_approval", UnverifiedApprovalItem, "Saved Checkpoint 5 approval of this revision when current promotion inputs could not be verified on this read (because no promotion target is configured or the target could not report its current inputs). At most one of approval or unverified_approval is set. It is never saved to disk or hashed into request_revision.")
	Required("change_id", "status", "prompt", "summary", "examples", "preserved_behaviors", "request_revision")
})

// ConfirmIntentPayload locks the operator's confirmed ambiguity answers and example table at CP1.
var ConfirmIntentPayload = Type("ConfirmIntentPayload", func() {
	Description("Operator confirmation of Checkpoint 1 ambiguity answers and concrete examples.")
	Attribute("change_id", String, "Change identifier.")
	Attribute("request_revision", String, "Reviewed CP1 request_revision digest being confirmed.")
	Attribute("answers", MapOf(String, String), "Selected resolution per ambiguity question ID. Answer every question, and give each value exactly as one string from that question's options, character for character. To record a decision none of the options expresses, call revise_change with it in prompt, then confirm the revised card.")
	Attribute("examples", ArrayOf(ExampleItem), "Confirmed example table.")
	Required("change_id", "request_revision", "answers", "examples")
})

// C4ContainerItem describes one architecture element in a Checkpoint 2 preview.
var C4ContainerItem = Type("C4ContainerItem", func() {
	Description("Architecture element declared by the target Goa, Goa-AI, or Model design.")
	Attribute("id", String, "Stable container identifier.")
	Attribute("name", String, "Human-readable container name.")
	Attribute("kind", String, "Declared service, agent, toolset, person, system, container, or component.", func() {
		Enum("service", "agent", "toolset", "person", "system", "container", "component")
	})
	Attribute("description", String, "Summary of the container's responsibility.")
	Required("id", "name", "kind", "description")
})

// C4EdgeItem describes a directed contract relationship between two C4 containers.
var C4EdgeItem = Type("C4EdgeItem", func() {
	Description("Directed contract relationship between two C4 containers.")
	Attribute("from_id", String, "Source container ID.")
	Attribute("to_id", String, "Target container ID.")
	Attribute("protocol", String, "Contract or transport relationship.")
	Required("from_id", "to_id", "protocol")
})

// AgentPolicyItem reports the evaluated Goa-AI runtime guardrails and cache policy for one agent.
var AgentPolicyItem = Type("AgentPolicyItem", func() {
	Description("Evaluated Goa-AI cost, safety, history, and cache policy for a single agent.")
	Attribute("agent_id", String, "Scoped agent container identifier.")
	Attribute("max_execution_seconds", Int, "Per-run execution time budget in seconds.")
	Attribute("max_tool_calls", Int, "Per-run retrieval tool-call budget.")
	Attribute("max_recovery_turns", Int, "Maximum consecutive recovery turns after validation failure.")
	Attribute("compress_at_turns", Int, "Turn threshold for history compression.")
	Attribute("keep_recent_turns", Int, "Recent turns retained uncompressed.")
	Attribute("cache_after_system", Boolean, "True when prompt caching places a checkpoint after system prompts.")
	Attribute("cache_after_tools", Boolean, "True when prompt caching places a checkpoint after tool definitions.")
	Required(
		"agent_id",
		"max_execution_seconds",
		"max_tool_calls",
		"max_recovery_turns",
		"compress_at_turns",
		"keep_recent_turns",
		"cache_after_system",
		"cache_after_tools",
	)
})

// ToolInputBindingItem lists the compiled ordinary and lookup input choices for
// one consumer; it does not assert which installer the application invokes.
var ToolInputBindingItem = Type("ToolInputBindingItem", func() {
	Description("Compiled consumer input contracts declared for one selected tool. The application installs a binder and supplies values at runtime; compilation does not prove that wiring.")
	Attribute("args_schema", String, "Generated ordinary bound model input.")
	Attribute("text_only_args_schema", String, "Generated ordinary text-only input, when supported.")
	Attribute("lookup_args_schema", String, "Generated lookup-key input, when declared; application lookup values are not inspected.")
	Attribute("lookup_text_only_args_schema", String, "Generated text-only lookup-key input, when supported.")
	Attribute("injected_fields", ArrayOf(String), "Compiler-listed fields supplied outside the ordinary bound model input.")
	Required("args_schema", "injected_fields")
})

// ToolContractItem reports the evaluated tool schema, server-data injections,
// confirmation gate, and bounded-result contract for one tool.
var ToolContractItem = Type("ToolContractItem", func() {
	Description("Evaluated Goa-AI tool contract summary surfaced at Checkpoint 2.")
	Attribute("id", String, "Consumer agent ID followed by the compiler's tool ID (<agent_id>/<tool_id>).")
	Attribute("toolset", String, "Compiler-selected toolset registration route.")
	Attribute("name", String, "Tool name.")
	Attribute("description", String, "Tool description.")
	Attribute("args_schema", String, "Generated provider model input before consumer binding.")
	Attribute("text_only_args_schema", String, "Generated provider text-only input, when supported.")
	Attribute("input_binding", ToolInputBindingItem, "Compiled consumer input choices, when binding is declared.")
	Attribute("return_schema", String, "Generated model-visible result schema; empty when the tool declares no result.")
	Attribute("injected_fields", ArrayOf(String), "Compiler-listed runtime or provider fields omitted from the provider model input.")
	Attribute("requires_ui", Boolean, "True when the tool requires a UI-capable runtime.")
	Attribute("requires_confirmation", Boolean, "True when the tool declares a design-time Confirmation gate.")
	Attribute("bounded_result", Boolean, "True when the tool declares a BoundedResult cursor/truncation contract.")
	Required(
		"id",
		"toolset",
		"name",
		"description",
		"args_schema",
		"return_schema",
		"injected_fields",
		"requires_confirmation",
		"requires_ui",
		"bounded_result",
	)
})

// BuilderScopeItem defines an explicit file-ownership slice for one builder agent.
var BuilderScopeItem = Type("BuilderScopeItem", func() {
	Description("Explicit repository path ownership slice for a builder agent role.")
	Attribute("name", String, "Human-readable builder scope name (for example, orders or api).")
	Attribute("paths", ArrayOf(String), "Repository-relative path globs owned by this builder.")
	Attribute("preferred_profile_id", String, "Optional preference selecting a server-configured authenticated profile ID.")
	Required("name", "paths")
})

// SwarmRoleItem describes one proposed agent role assignment in a Surla build swarm.
var SwarmRoleItem = Type("SwarmRoleItem", func() {
	Description("Proposed Chorus agent role assignment derived from server-owned provider profiles and explicit workspace scopes. Owned paths and access modes are operator-declared coordination ownership boundaries for Chorus agents, not Surla-enforced OS isolation.")
	Attribute("handle", String, "Deterministic Chorus agent handle (without leading '@', at most 32 characters).")
	Attribute("role", String, "Assigned swarm role kind.", func() {
		Enum("architect", "builder", "eval_author", "reviewer")
	})
	Attribute("scope_name", String, "Human-readable scope label for this assignment.")
	Attribute("profile_id", String, "Assigned server-configured provider profile ID.")
	Attribute("provider", String, "Assigned coding-agent provider name.")
	Attribute("model", String, "Assigned model identifier.")
	Attribute("family", String, "Canonical model family identifier.")
	Attribute("owned_paths", ArrayOf(String), "Repository or workspace path globs owned by this role for Chorus coordination.")
	Attribute("access_mode", String, "Operator-declared workspace coordination boundary for this role.", func() {
		Enum("read_only_after_cp2", "read_write_worktree", "isolated_eval_workspace", "read_only_review")
	})
	Required(
		"handle",
		"role",
		"scope_name",
		"profile_id",
		"provider",
		"model",
		"family",
		"owned_paths",
		"access_mode",
	)
})

// SwarmPlanItem reports the proposed Chorus role roster and model-family routing at Checkpoint 2.
var SwarmPlanItem = Type("SwarmPlanItem", func() {
	Description("Proposed Chorus role assignments and model-family diversity facts surfaced at Checkpoint 2. Role, model, and workspace scope assignments are Chorus coordination proposals and do not prove agent launch or replace OS process and workspace isolation.")
	Attribute("status", String, "Roster configuration status.", func() {
		Enum("configured", "unconfigured", "missing_capability")
	})
	Attribute("assignments", ArrayOf(SwarmRoleItem), "Deterministic role assignments when status is configured.")
	Attribute("cross_family_review", Boolean, "True if and only if status is configured and the reviewer model family is disjoint from every assigned builder model family.")
	Attribute("cross_family_eval", Boolean, "True if and only if status is configured, an eval_author role is assigned, and the eval_author model family is disjoint from every assigned builder model family.")
	Attribute("single_family", Boolean, "True when status is configured and all assigned roles share one model family.")
	Attribute("missing_roles", ArrayOf(String), "Requested roles lacking an authenticated capable profile when status is unconfigured (with builder scopes supplied) or missing_capability; omitted on solo previews with no builder scopes.")
	Attribute("summary", String, "Plain-English summary of the proposed swarm roster and model-family posture.")
	Attribute("manifest", String, "Formatted Chorus collaboration manifest text when status is configured; does not claim publication until sent via Chorus.")
	Required(
		"status",
		"assignments",
		"cross_family_review",
		"cross_family_eval",
		"single_family",
		"summary",
	)
})

// DesignSnapshot is an immutable copy of a Go module holding a Goa design.
var DesignSnapshot = Type("DesignSnapshot", func() {
	Description("Exact files of one Go module containing a Goa/Goa-AI design package at design/. Paths are clean, relative to the module root, and name only .go, go.mod and go.sum files; go.mod, go.sum and at least one design/*.go file are required. The evaluator stages exactly these bytes, resolves dependencies only from go.mod and go.sum, and never reads host paths.")
	Attribute("files", MapOf(String, String), "File contents keyed by module-relative path, such as design/design.go or go.mod.")
	Required("files")
})

// PreviewArchitecturePayload is the input to the CP2 preview_architecture tool.
var PreviewArchitecturePayload = Type("PreviewArchitecturePayload", func() {
	Description("Request to derive the Checkpoint 2 C4 topology, guardrails, and proposed swarm roster from a candidate Goa design bound to a confirmed Change Card.")
	Attribute("change_id", String, "Confirmed Change Card identifier.")
	Attribute("request_revision", String, "Expected CP1 request_revision digest.")
	Attribute("design", DesignSnapshot, "Candidate module whose Goa and Goa-AI design is evaluated.")
	Attribute("builders", ArrayOf(BuilderScopeItem), "Optional explicit builder path scopes for proposing a Chorus build roster. When omitted, preview_architecture returns a solo preview with swarm.status unconfigured and no missing_roles.")
	Attribute("architect_paths", ArrayOf(String), "Optional repository-relative path globs for an @architect role in the proposed Chorus roster (requires builders).")
	Attribute("eval_paths", ArrayOf(String), "Optional repository or evaluator-workspace relative path globs for an @eval-author role in the proposed Chorus roster (requires builders).")
	Required("change_id", "request_revision", "design")
})

// ArchitecturePreviewResult is the output of the CP2 preview_architecture tool.
var ArchitecturePreviewResult = Type("ArchitecturePreviewResult", func() {
	Description("Checkpoint 2 C4 container topology, tool contracts, per-agent Goa-AI guardrails, and proposed Chorus swarm roster derived from the evaluated candidate design.")
	Attribute("change_id", String, "Bound Change Card identifier.")
	Attribute("request_revision", String, "Bound CP1 request_revision digest.")
	Attribute("design_digest", String, "SHA-256 digest of the evaluated design snapshot (sorted path and content pairs).")
	Attribute("system_name", String, "API or primary service name declared in the design.")
	Attribute("containers", ArrayOf(C4ContainerItem), "Derived C4 containers.")
	Attribute("edges", ArrayOf(C4EdgeItem), "Validated directed edges between declared containers.")
	Attribute("agent_policies", ArrayOf(AgentPolicyItem), "Evaluated Goa-AI runtime policies per declared agent.")
	Attribute("tools", ArrayOf(ToolContractItem), "Evaluated Goa-AI tool contracts.")
	Attribute("swarm", SwarmPlanItem, "Proposed Chorus multi-agent role assignments and model-family routing.")
	Attribute("banner", String, "Plain-English summary of topology and guardrails.")
	Required(
		"change_id",
		"request_revision",
		"design_digest",
		"system_name",
		"containers",
		"edges",
		"agent_policies",
		"tools",
		"swarm",
		"banner",
	)
})

// DraftOverridePayload is the input to the CP3 Fast Draft Mode tool.
var DraftOverridePayload = Type("DraftOverridePayload", func() {
	Description("Fast Draft Mode request to hot-reload a prompt template on the bound target platform or reject a compiled business-rule edit.")
	Attribute("target_kind", String, "Override target kind (prompt_template or compiled_rule).", func() {
		Enum("prompt_template", "compiled_rule")
	})
	Attribute("prompt_id", String, "Registered prompt identifier when target_kind is prompt_template.")
	Attribute("new_content", String, "Updated prompt template text or rule expression.")
	Required("target_kind", "new_content")
})

// DraftOverrideResult reports whether the CP3 edit was hot-reloaded in 0s or requires a full Go rebuild.
var DraftOverrideResult = Type("DraftOverrideResult", func() {
	Description("Result of a Checkpoint 3 Fast Draft Mode override request.")
	Attribute("applied_in_store", Boolean, "True when updated in the target platform's prompt.Store for 0s reload.")
	Attribute("requires_rebuild", Boolean, "True when the change modifies a compiled Go business rule.")
	Attribute("prompt_version", String, "New prompt version digest returned by the target platform.")
	Attribute("message", String, "Operator-facing status explanation.")
	Required("applied_in_store", "requires_rebuild", "message")
})

// StartLiveTargetPayload names the candidate commit a CP3 live target runs.
var StartLiveTargetPayload = Type("StartLiveTargetPayload", func() {
	Description("Candidate commit to build and run as this server's live CP3 target.")
	Attribute("candidate_commit", String, "Full commit ID to build, in the candidate repository's object format.", func() {
		Pattern("^([0-9a-f]{40}|[0-9a-f]{64})$")
		Example("4f0b3c1d2e5a6978877665544332211009988776")
	})
	Required("candidate_commit")
})

// LiveTargetResult describes the live CP3 target a server now runs.
var LiveTargetResult = Type("LiveTargetResult", func() {
	Description("The live CP3 target this server runs and binds draft_override to.")
	Attribute("candidate_commit", String, "The commit the live target was built from.")
	Attribute("url", String, "Loopback origin of the running candidate, for trying requests such as its own API directly.")
	Attribute("message", String, "Operator-facing status explanation.")
	Required("candidate_commit", "url", "message")
})

// StopLiveTargetResult reports whether a live CP3 target was stopped.
var StopLiveTargetResult = Type("StopLiveTargetResult", func() {
	Description("Result of stopping this server's live CP3 target.")
	Attribute("stopped", Boolean, "True when a live target was running and is now stopped; false when none was running.")
	Attribute("message", String, "Operator-facing status explanation.")
	Required("stopped", "message")
})

// ApproveMergePayload is the input to the CP5 approve_merge tool.
var ApproveMergePayload = Type("ApproveMergePayload", func() {
	Description("Identifies the confirmed Change Card revision, the candidate commit being promoted, and the evaluator-written verification report to check. The report itself and the expected suite, environment and binary digests come from the control plane's promotion target, never from the caller.")
	Attribute("change_id", String, "Confirmed Change Card identifier.")
	Attribute("request_revision", String, "Current request_revision of the confirmed Change Card.")
	Attribute("candidate_commit", String, "Full commit ID of the candidate being promoted (40 or 64 lowercase hex characters).", func() {
		Pattern("^([0-9a-f]{40}|[0-9a-f]{64})$")
	})
	Attribute("report_digest", String, "SHA-256 digest of the CP4 verification report written by surla verify (64 lowercase hex characters).", func() {
		Pattern("^[0-9a-f]{64}$")
	})
	Required("change_id", "request_revision", "candidate_commit", "report_digest")
})

// DiffPayload is the input to the structural design and module diff tool.
var DiffPayload = Type("DiffPayload", func() {
	Description("Previous and candidate design modules to compare structurally, each evaluated with its own go.mod dependencies.")
	Attribute("old", DesignSnapshot, "Previous design module.")
	Attribute("new", DesignSnapshot, "Candidate design module.")
	Required("old", "new")
})

// DiffResult reports structural declaration and go.mod version deltas without a blanket compatibility claim.
var DiffResult = Type("DiffResult", func() {
	Description("Structural diff of evaluated Goa/Goa-AI declarations and go.mod directives.")
	Attribute("has_changes", Boolean, "True when any declaration, evaluated contract schema, or module directive changed.")
	Attribute("added_declarations", ArrayOf(String), "Added scoped declarations.")
	Attribute("removed_declarations", ArrayOf(String), "Removed scoped declarations.")
	Attribute("modified_declarations", ArrayOf(String), "Declarations whose evaluated schema, route, or policy changed.")
	Attribute("version_deltas", ArrayOf(String), "Changed module require, replace, or exclude directives.")
	Attribute("summary", String, "Human-readable structural diff summary.")
	Required("has_changes", "added_declarations", "removed_declarations", "modified_declarations", "version_deltas", "summary")
})

// VerifyChangePayload starts Checkpoint 4 verification of one candidate commit.
var VerifyChangePayload = Type("VerifyChangePayload", func() {
	Description("Names the confirmed Change Card revision and the candidate commit to verify. What is built and which evaluator suite runs come from the operator's configuration of surla serve, never from the caller.")
	Attribute("change_id", String, "Confirmed Change Card identifier.")
	Attribute("request_revision", String, "Current request_revision of the confirmed Change Card.")
	Attribute("candidate_commit", String, "Full commit ID of the candidate to build and verify (40 or 64 lowercase hex characters).", func() {
		Pattern("^([0-9a-f]{40}|[0-9a-f]{64})$")
	})
	Required("change_id", "request_revision", "candidate_commit")
})

// GetVerificationPayload names one verification run.
var GetVerificationPayload = Type("GetVerificationPayload", func() {
	Description("Identifies a verification run returned by verify_change.")
	Attribute("run_id", String, "Verification run identifier returned by verify_change.")
	Required("run_id")
})

// VerificationCheck is one check of a repair scenario.
var VerificationCheck = Type("VerificationCheck", func() {
	Description("One deterministic check of a repair scenario.")
	Attribute("name", String, "Check name.")
	Attribute("passed", Boolean, "Whether the check passed.")
	Attribute("diagnostic", String, "Why the check failed; empty when it passed.")
	Required("name", "passed")
})

// VerificationScenario is one declared scenario in the builder view.
var VerificationScenario = Type("VerificationScenario", func() {
	Description("One declared scenario as builders see it. Repair scenarios carry their name, checks and environment error; holdout scenarios appear only as holdout-<n> with their outcome.")
	Attribute("id", String, "Scenario name for a repair scenario, or holdout-<n> for a holdout.")
	Attribute("outcome", String, "How the scenario ended.", func() {
		Enum("passed", "failed", "not_run", "harness_error")
	})
	Attribute("checks", ArrayOf(VerificationCheck), "The repair scenario's checks; empty for holdouts.")
	Attribute("error", String, "The repair scenario's environment error; empty for holdouts.")
	Required("id", "outcome")
})

// VerificationCounts counts declared scenarios by outcome.
var VerificationCounts = Type("VerificationCounts", func() {
	Description("Every declared scenario, counted by outcome.")
	Attribute("required", Int, "Scenarios the suite declares.")
	Attribute("passed", Int, "Scenarios that passed.")
	Attribute("failed", Int, "Scenarios whose checks failed.")
	Attribute("not_run", Int, "Declared scenarios the suite did not report.")
	Attribute("harness_error", Int, "Scenarios that could not be evaluated.")
	Required("required", "passed", "failed", "not_run", "harness_error")
})

// BuilderVerificationReport is the builder view of a CP4 report.
var BuilderVerificationReport = Type("BuilderVerificationReport", func() {
	Description("The builder view of a Checkpoint 4 verification report: counts, verdict and repair scenario checks. Holdout details, evidence and logs stay with the evaluator.")
	Attribute("report_digest", String, "SHA-256 digest of the full report the evaluator saved; approve_merge and surla approve take this digest.")
	Attribute("counts", VerificationCounts, "Every declared scenario, counted by outcome.")
	Attribute("passed", Boolean, "The verdict: true only when every declared scenario passed and the suite ran cleanly.")
	Attribute("suite_failed", Boolean, "Whether the evaluator suite itself failed or ended abnormally.")
	Attribute("scenarios", ArrayOf(VerificationScenario), "Every declared scenario in declaration order.")
	Required("report_digest", "counts", "passed", "suite_failed", "scenarios")
})

// VerificationRunning is the state of a run still in progress.
var VerificationRunning = Type("VerificationRunning", func() {
	Description("The run is still building or evaluating the candidate.")
	Attribute("started_at", String, "When the run was accepted.", func() { Format(FormatDateTime) })
	Required("started_at")
})

// VerificationCompleted is the state of a run that produced a report.
var VerificationCompleted = Type("VerificationCompleted", func() {
	Description("The run produced a verification report. Its verdict is report.passed.")
	Attribute("suite_id", String, "Evaluator suite the report was produced by.")
	Attribute("environment", String, "Dependency profile the suite ran the candidate with, such as sim.")
	Attribute("report", BuilderVerificationReport, "The builder view of the report.")
	Required("suite_id", "environment", "report")
})

// VerificationNotCompleted is the state of a run that produced no report.
var VerificationNotCompleted = Type("VerificationNotCompleted", func() {
	Description("The run was accepted but did not complete: a step failed, or its processes ended or were canceled or timed out before it finished. The summary states where it stopped, without command output; full evidence, including any partial report saved while it was being stopped, stays with the evaluator.")
	Attribute("kind", String, "execution_failed when a step failed; interrupted when the run's process ended before it finished.", func() {
		Enum("execution_failed", "interrupted")
	})
	Attribute("stage", String, "Where the run stopped.", func() {
		Enum("checkout", "build", "describe", "suite", "report", "supervise")
	})
	Attribute("summary", String, "Builder-safe description of what stopped the run; never command output.")
	Required("kind", "stage", "summary")
})

// VerificationStatusResult reports one CP4 verification run.
var VerificationStatusResult = Type("VerificationStatusResult", func() {
	Description("One Checkpoint 4 verification run of a candidate commit for a confirmed Change Card revision, and its current state. The run keeps the revision it was started for, so a later confirm_intent does not change it.")
	Attribute("run_id", String, "Verification run identifier; get_verification takes it.")
	Attribute("change_id", String, "Change Card the run verifies.")
	Attribute("request_revision", String, "Change Card revision the run verifies.")
	Attribute("candidate_commit", String, "Candidate commit the run builds and verifies.")
	Attribute("suite_commit", String, "Evaluator suite commit the operator configured for the run.")
	Attribute("accepted_at", String, "When the run was accepted.", func() { Format(FormatDateTime) })
	OneOf("state", "Exactly one of running, completed or not_completed.", func() {
		Attribute("running", VerificationRunning)
		Attribute("completed", VerificationCompleted)
		Attribute("not_completed", VerificationNotCompleted)
	})
	Required("run_id", "change_id", "request_revision", "candidate_commit", "suite_commit", "accepted_at", "state")
})

// ListVerificationsPayload names the Change Card whose verification runs are listed.
var ListVerificationsPayload = Type("ListVerificationsPayload", func() {
	Description("Identifies the Change Card whose verification runs are listed for its owner.")
	Attribute("change_id", String, "Change Card identifier.", func() {
		MinLength(1)
	})
	Required("change_id")
})

// VerificationListResult lists the owner's verification runs for one Change Card.
var VerificationListResult = Type("VerificationListResult", func() {
	Description("All Checkpoint 4 verification runs started by the owner for one Change Card, ordered newest first by accepted_at (tie-broken by run_id).")
	Attribute("change_id", String, "Change Card identifier.")
	Attribute("runs", ArrayOf(VerificationStatusResult), "Verification runs for change_id in newest-first order.")
	Required("change_id", "runs")
})

var _ = Service("surla", func() {
	Description("Surla MCP software factory control plane.")

	MCP("surla", "1.0.0")
	JSONRPC(func() {
		POST("/mcp")
	})

	Method("checkpoint_ui", func() {
		Description("Serve the interactive MCP Apps HTML host for Surla checkpoints.")
		Result(String)
		Resource("checkpoint_ui", "ui://surla/checkpoint", "text/html;profile=mcp-app")
		HTTP(func() {
			GET("/ui/checkpoint")
			Response(StatusOK)
		})
	})

	Method("golden_path", func() {
		Description("Serve the Surla Golden Path architecture playbook for Goa-AI agentic platforms.")
		Result(String)
		Resource("golden_path", "surla://golden-path", "text/markdown")
		HTTP(func() {
			GET("/golden-path")
			Response(StatusOK)
		})
	})

	Method("propose_change", func() {
		Description("Evaluate a feature request at Checkpoint 1, surfacing concrete examples and any genuine domain ambiguity.")
		Payload(ProposeChangePayload)
		Result(ChangeCardResult)
		Tool("propose_change", "Propose a feature change and generate a Checkpoint 1 Change Card.", func() {
			ToolUI("ui://surla/checkpoint")
		})
		HTTP(func() {
			POST("/changes/propose")
			Response(StatusOK)
		})
	})

	Method("revise_change", func() {
		Description("Revise an existing Checkpoint 1 Change Card with new or changed operator requirements bound to its current request_revision. Re-analyzes the revised prompt and design source alongside the prior card's confirmed decisions, open questions, examples and preserved behaviors, and saves a new revision in needs_confirmation status for operator review.")
		Payload(ReviseChangePayload)
		Result(ChangeCardResult)
		Tool("revise_change", "Revise an existing Checkpoint 1 Change Card with updated requirements bound to its current request_revision.", func() {
			ToolUI("ui://surla/checkpoint")
		})
		HTTP(func() {
			POST("/changes/revise")
			Response(StatusOK)
		})
	})

	Method("confirm_intent", func() {
		Description("Confirm or refine a Checkpoint 1 Change Card with the complete ambiguity answers and example table, bound to its current request_revision. Changed answers or examples produce a new request_revision; unchanged ones return the card as it is, with a saved approval returned in approval only while that approval still matches the promotion target's current inputs, or in unverified_approval when no promotion target is configured or the target cannot report its current inputs.")
		Payload(ConfirmIntentPayload)
		Result(ChangeCardResult)
		Tool("confirm_intent", "Confirm or refine Checkpoint 1 ambiguity answers and example checks for the card's current request_revision.", func() {
			ToolUI("ui://surla/checkpoint")
		})
		HTTP(func() {
			POST("/changes/confirm")
			Response(StatusOK)
		})
	})

	Method("get_change", func() {
		Description("Return the current Change Card, including its request_revision, to its owner, for example after confirm_intent or revise_change refused an edit made against an older revision. Reading never confirms or changes the card. A recorded approval is returned in approval only while it still matches the promotion target's current suite, environment and binary digests, or in unverified_approval when no promotion target is configured or the target cannot report its current inputs.")
		Payload(GetChangePayload)
		Result(ChangeCardResult)
		Tool("get_change", "Read the current Checkpoint 1 Change Card and its request_revision.", func() {
			ToolUI("ui://surla/checkpoint")
		})
		HTTP(func() {
			GET("/changes/{change_id}")
			Response(StatusOK)
		})
	})

	Method("preview_architecture", func() {
		Description("Extract and validate the Checkpoint 2 C4 container topology, tool contracts, Goa-AI guardrails, and proposed Chorus swarm roster from a candidate Goa design bound to a confirmed Change Card.")
		Payload(PreviewArchitecturePayload)
		Result(ArchitecturePreviewResult)
		Tool("preview_architecture", "Preview Checkpoint 2 C4 container topology, Goa-AI guardrails, and proposed Chorus swarm roster for a confirmed Change Card.", func() {
			ToolUI("ui://surla/checkpoint")
		})
		HTTP(func() {
			POST("/changes/preview")
			Response(StatusOK)
		})
	})

	Method("draft_override", func() {
		Description("Apply a Fast Draft Mode prompt override on the bound target or reject a compiled-rule prompt hack. On a live target started with start_live_target, a rejected update leaves the target's text unchanged; update_not_confirmed means the target may or may not have applied it.")
		Payload(DraftOverridePayload)
		Result(DraftOverrideResult)
		Error("prompt_not_found", ErrorResult, "The live target has no prompt with this ID; nothing changed.")
		Error("invalid_prompt", ErrorResult, "The live target rejected the template because it does not render; nothing changed.")
		Error("live_target_ended", ErrorResult, "The live target is no longer running; start it again with start_live_target.")
		Error("target_rejected_credential", ErrorResult, "The live target rejected this server's key; nothing changed. Start the target again with start_live_target.")
		Error("update_not_confirmed", ErrorResult, "The live target did not confirm the update, so it may or may not have applied it. Keep the draft and send it again.")
		Tool("draft_override", "Hot-reload a prompt template in 0s or flag a compiled business rule change.", func() {
			ToolUI("ui://surla/checkpoint")
		})
		HTTP(func() {
			POST("/sandbox/draft")
			Response(StatusOK)
			Response("prompt_not_found", StatusNotFound)
			Response("invalid_prompt", StatusUnprocessableEntity)
			Response("live_target_ended", StatusConflict)
			Response("target_rejected_credential", StatusBadGateway)
			Response("update_not_confirmed", StatusBadGateway)
		})
	})

	Method("start_live_target", func() {
		Description("Build a candidate commit exactly as Checkpoint 4 does and run it, with the operator's evaluator suite, as this server's one live CP3 target, then bind draft_override to it. A running target is stopped first, so there is never more than one. The target runs until stop_live_target, a later start_live_target, its own exit, or this server's shutdown; a server restart leaves none running. Requires Checkpoint 4 verification to be configured, because the build and launch settings come from it.")
		Payload(StartLiveTargetPayload)
		Result(LiveTargetResult)
		Error("live_target_unavailable", ErrorResult, "This server has no verification configuration, so it cannot build or run a live target.")
		Error("live_target_failed", ErrorResult, "The live target could not be built or started; the message says which step failed.")
		Tool("start_live_target", "Build a candidate commit and run it as this server's live CP3 target so draft_override can try prompt text on it. This first stops the target already running, which discards every prompt draft applied to it, even if the new build then fails. Prompt text survives only when it is committed.", func() {
			ToolUI("ui://surla/checkpoint")
		})
		HTTP(func() {
			POST("/live-target")
			Response(StatusOK)
			Response("live_target_unavailable", StatusNotImplemented)
			Response("live_target_failed", StatusUnprocessableEntity)
		})
	})

	Method("stop_live_target", func() {
		Description("Stop this server's live CP3 target, if one runs, and unbind draft_override from it. Stopping when none runs is not an error.")
		Result(StopLiveTargetResult)
		Error("live_target_stop_failed", ErrorResult, "The live target's processes could not be confirmed stopped; restart surla serve.")
		Tool("stop_live_target", "Stop this server's live CP3 target, if one runs. This discards every prompt draft applied to it; prompt text survives only when it is committed.", func() {
			ToolUI("ui://surla/checkpoint")
		})
		HTTP(func() {
			DELETE("/live-target")
			Response(StatusOK)
			Response("live_target_stop_failed", StatusInternalServerError)
		})
	})

	Method("verify_change", func() {
		Description("Start Checkpoint 4 verification of a candidate commit for the owner's confirmed Change Card revision and return its run_id at once. The server builds the commit in Surla's sandbox and runs the evaluator suite the operator configured, in a separate process that keeps running when the caller disconnects or the server restarts; get_verification reads its state. Only the builder view of the report is ever returned. A server started without verification configured rejects every call.")
		Payload(VerifyChangePayload)
		Result(VerificationStatusResult)
		Tool("verify_change", "Start CP4 verification of a candidate commit for the confirmed Change Card revision; returns a run_id to read with get_verification.", func() {
			ToolUI("ui://surla/checkpoint")
		})
		HTTP(func() {
			POST("/changes/{change_id}/verifications")
			Response(StatusOK)
		})
	})

	Method("get_verification", func() {
		Description("Return the current state of a verification run to the owner who started it: running, completed with the builder view of its report, or not completed with where it stopped. A run keeps the Change Card revision it was started for.")
		Payload(GetVerificationPayload)
		Result(VerificationStatusResult)
		Tool("get_verification", "Read the state of a CP4 verification run started by verify_change.", func() {
			ToolUI("ui://surla/checkpoint")
		})
		HTTP(func() {
			GET("/verifications/{run_id}")
			Response(StatusOK)
		})
	})

	Method("list_verifications", func() {
		Description("Return all Checkpoint 4 verification runs started by the owner for a Change Card, ordered newest first by accepted_at (tie-broken by run_id), with each run's current state and the Change Card revision it was started for. Works whether the Change Card is currently confirmed or in needs_confirmation after a later revision.")
		Payload(ListVerificationsPayload)
		Result(VerificationListResult)
		Tool("list_verifications", "List all CP4 verification runs started for a Change Card, newest first.", func() {
			ToolUI("ui://surla/checkpoint")
		})
		HTTP(func() {
			GET("/changes/{change_id}/verifications")
			Response(StatusOK)
		})
	})

	Method("approve_merge", func() {
		Description("Record Checkpoint 5 approval for the owner's confirmed Change Card revision. The control plane loads the verification report by digest and the expected suite, environment and candidate binary digests from its promotion target, re-derives the report from its evidence with verify.Approve, and records the approval on the card. A report that did not pass or describes different inputs is rejected. Nothing is merged or deployed. The promotion target is provided by the program that embeds the control plane; a control plane without one, such as standalone surla serve, rejects every call.")
		Payload(ApproveMergePayload)
		Result(ChangeCardResult)
		Tool("approve_merge", "Approve the confirmed Change Card revision after its CP4 verification report passed for the candidate commit.", func() {
			ToolUI("ui://surla/checkpoint")
		})
		HTTP(func() {
			POST("/changes/approve")
			Response(StatusOK)
		})
	})

	Method("diff", func() {
		Description("Compare evaluated Goa designs and go.mod directives without emitting a blanket compatibility verdict.")
		Payload(DiffPayload)
		Result(DiffResult)
		Tool("diff", "Compute structural Goa design and go.mod dependency diffs.", func() {
			ToolUI("ui://surla/checkpoint")
		})
		HTTP(func() {
			POST("/diff")
			Response(StatusOK)
		})
	})
})
