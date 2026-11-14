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
	Attribute("id", String, "Stable example identifier. Default code-assigned IDs use check_01, check_02, ... (valid goa-ai/eval Scenario names). confirm_intent preserves caller-chosen IDs.")
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
	Description("Operator feature request and the caller's analysis of it, saved as a new Checkpoint 1 Change Card.")
	Attribute("change_id", String, "Unique change identifier.")
	Attribute("prompt", String, "Operator feature request.")
	Attribute("story_refresh_op_id", String, "Optional op_id of a succeeded refresh_story operation for the bound workspace whose evaluated Stories this card adopts. When set, Surla reads one workspace snapshot, requires its snapshot_digest to match the operation's, and records both the evaluated Stories and that snapshot's design/*.go on the card. Omit when the card has no Story.")
	Attribute("analysis", CP1Analysis, "Your analysis of prompt against the target's current design.")
	Required("change_id", "prompt", "analysis")
})

// ReviseChangePayload is the input to the CP1 revise_change tool.
var ReviseChangePayload = Type("ReviseChangePayload", func() {
	Description("Revised operator feature request and the caller's analysis of it, saved as the next revision of an existing Checkpoint 1 Change Card.")
	Attribute("change_id", String, "Change identifier.")
	Attribute("request_revision", String, "request_revision of the latest revision you read: the proposed revision's when the change shows one waiting (proposed_revision.request_revision), otherwise the change's own.")
	Attribute("prompt", String, "Revised operator feature request.")
	Attribute("story_refresh_op_id", String, "Optional op_id of a succeeded refresh_story operation for the bound workspace whose evaluated Stories this revision adopts. When set, Surla reads one workspace snapshot, requires its snapshot_digest to match the operation's, and records both the evaluated Stories and that snapshot's design/*.go on the revised card. When omitted on a card that already has Stories, the card's saved Stories and their design source are kept together.")
	Attribute("analysis", CP1RevisionAnalysis, "Your analysis of the revised prompt against the current card and the target's current design.")
	Required("change_id", "request_revision", "prompt", "analysis")
})

// GetChangePayload is the input to the get_change tool.
var GetChangePayload = Type("GetChangePayload", func() {
	Description("Identifies the Change Card the authenticated owner wants to read.")
	Attribute("change_id", String, "Change identifier.")
	Required("change_id")
})

// DeclineRevisionPayload names the proposed revision the user declines.
var DeclineRevisionPayload = Type("DeclineRevisionPayload", func() {
	Description("Identifies the proposed revision of a confirmed Change Card that the user declines.")
	Attribute("change_id", String, "Change identifier.", func() {
		MinLength(1)
	})
	Attribute("request_revision", String, "request_revision of the proposed revision being declined (proposed_revision.request_revision): 64 lowercase hex characters.", func() {
		Pattern("^[0-9a-f]{64}$")
	})
	Required("change_id", "request_revision")
})

// InitProjectPayload is the input to the init_project tool.
var InitProjectPayload = Type("InitProjectPayload", func() {
	Description("Go module path and optional direct child directory name for initializing a new Surla starter project inside the bound workspace.")
	Attribute("module_path", String, "Go module path of the new service (for example example.com/shop).", func() {
		MinLength(3)
		Example("example.com/shop")
	})
	Attribute("dir", String, "Direct child directory name inside the bound workspace root. When omitted or empty, defaults to the service name derived from module_path.", func() {
		Example("shop")
	})
	Required("module_path")
})

// InitProjectResult is the output of the init_project tool.
var InitProjectResult = Type("InitProjectResult", func() {
	Description("Newly initialized Surla starter project directory, derived service identity, and initial Git commit.")
	Attribute("dir", String, "Physical directory path of the newly created starter project (now the server's active workspace).")
	Attribute("module_path", String, "Initialized Go module path.")
	Attribute("service_name", String, "Derived service name.")
	Attribute("commit", String, "Full commit ID of the initial starter commit created in the initialized repository (40 or 64 lowercase hex characters); start_live_target and verify_change take this commit ID.", func() {
		Pattern("^([0-9a-f]{40}|[0-9a-f]{64})$")
	})
	Required("dir", "module_path", "service_name", "commit")
})

// GenerateCodePayload selects the local module whose edited design is compiled.
var GenerateCodePayload = Type("GenerateCodePayload", func() {
	Description("Selects the application module or its public local eval/ module for code generation from the active workspace's files.")
	Attribute("target", String, "Module to regenerate: candidate (the application, the default when omitted) or eval (the local eval/ module).", func() {
		Enum("candidate", "eval")
	})
})

// GenerateCodeResult reports the generated files actually published locally.
var GenerateCodeResult = Type("GenerateCodeResult", func() {
	Description("Generated files written in the selected local module and warnings about auxiliary work. Generation compiles the design only; it does not build the application or run tests.")
	Attribute("target", String, "Module that received the generated files.", func() {
		Enum("candidate", "eval")
	})
	Attribute("files_written", ArrayOf(String), "Sorted paths relative to the selected module that were written, including AGENTS_QUICKSTART.md when produced and written successfully.")
	Attribute("warnings", ArrayOf(String), "Problems that did not prevent gen/ publication, such as incomplete cleanup or a guide that could not be written.")
	Required("target", "files_written", "warnings")
})

// CheckProjectResult reports one check_project operation.
var CheckProjectResult = Type("CheckProjectResult", func() {
	Description("Outcome of running every Go module's tests in a protected copy of the workspace inside Linux containers.")
	Attribute("op_id", String, "Operation identifier; pass it to read_check_output to read the full output.")
	Attribute("passed", Boolean, "True only when go test ./... exited 0 in every module.")
	Attribute("message", String, "At most 16 KiB: the modules tested, the first failure and the end of its output, and how to read the rest.")
	Required("op_id", "passed", "message")
})

// ReadCheckOutputPayload selects one page of a check operation's output.
var ReadCheckOutputPayload = Type("ReadCheckOutputPayload", func() {
	Description("Selects a page of the full output that check_project or update_dependencies captured.")
	Attribute("op_id", String, "Operation identifier returned by check_project or update_dependencies.")
	Attribute("cursor", String, "Opaque next_cursor from the previous page; omit for the first page.")
	Required("op_id")
})

// CheckOutputPage is one bounded page of captured output.
var CheckOutputPage = Type("CheckOutputPage", func() {
	Description("One page of at most 16 KiB of a check operation's captured output.")
	Attribute("text", String, "The page text.")
	Attribute("next_cursor", String, "Cursor for the following page; absent when this page reaches the end.")
	Attribute("captured_bytes", Int64, "Total bytes captured for the operation.")
	Attribute("dropped_bytes", Int64, "Bytes the operation produced beyond its capture limit and did not keep; nonzero means the output has a stated gap.")
	Required("text", "captured_bytes", "dropped_bytes")
})

// UpdateDependenciesResult reports the manifests written by update_dependencies.
var UpdateDependenciesResult = Type("UpdateDependenciesResult", func() {
	Description("Go module manifests rewritten in the workspace after go mod tidy ran in a protected copy.")
	Attribute("op_id", String, "Operation identifier; pass it to read_check_output to read the full output.")
	Attribute("files_written", ArrayOf(String), "Workspace-relative go.mod, go.sum and go.work.sum files whose bytes changed and were written.")
	Attribute("message", String, "At most 16 KiB: what was updated, and Go's message for any module that could not be.")
	Required("op_id", "files_written", "message")
})

// ConfigureModelPayload is the input to the configure_model tool.
var ConfigureModelPayload = Type("ConfigureModelPayload", func() {
	Description("Selects either a direct model provider and model name (whose request settings the surla command handed to the project server) or a running goa-ai model gateway address.")
	Attribute("provider", String, "Direct model provider name when configuring a direct provider; must be paired with model_name and omitted when gateway_addr is set.", func() {
		Enum("anthropic", "openai")
		Example("anthropic")
	})
	Attribute("model_name", String, "Provider model identifier when configuring a direct provider (for example claude-sonnet-4-6 or gpt-5.4-mini); must be paired with provider and omitted when gateway_addr is set.", func() {
		MinLength(1)
		Example("claude-sonnet-4-6")
	})
	Attribute("gateway_addr", String, "Plaintext gRPC host:port of a running goa-ai model gateway; mutually exclusive with provider and model_name.", func() {
		MinLength(1)
		Example("127.0.0.1:9090")
	})
})

// ModelConfigResult is the output of the configure_model tool.
var ModelConfigResult = Type("ModelConfigResult", func() {
	Description("Model configuration that live targets started afterwards receive, after configure_model succeeds.")
	Attribute("provider", String, "Active direct model provider when a direct provider is configured.", func() {
		Enum("anthropic", "openai")
	})
	Attribute("model_name", String, "Active model identifier when a direct provider is configured.", func() {
		MinLength(1)
	})
	Attribute("gateway_addr", String, "Active gRPC model gateway host:port when a model gateway is configured.", func() {
		MinLength(1)
	})
})

// UICompositionItem is an optional caller-authored decision view for one
// Change Card revision.
var UICompositionItem = Type("UICompositionItem", func() {
	Description("Caller-authored OpenUI Lang decision view for exactly one Change Card revision. It is presentation only: it is never hashed into request_revision, confirm_intent never reads it, and it describes the card only while composed_for_revision equals the card's request_revision.")
	Attribute("program", String, "OpenUI Lang program from the analysis's ui_composition, exactly as the caller sent it. The server does not check it; the views page validates it and shows its error state, with the rest of the card still usable, when the program is not a valid composition.")
	Attribute("composed_for_revision", String, "request_revision of the Change Card this program was composed for.")
	Required("program", "composed_for_revision")
})

// ApprovalItem records a passed CP5 promotion check for one Change Card
// revision.
var ApprovalItem = Type("ApprovalItem", func() {
	Description("Checkpoint 5 approval recorded by approve_merge after the verification report passed and matched the change being promoted: the exact candidate commit, the tests' commit chosen for the revision, and (when promotion settings are configured) the binary digests the promotion target measured. suite_id and environment record what the tests at suite_commit declared in the report and are shown, not compared against separate promotion settings. test_source is derived on each read from how the server chooses tests. The approval is saved with the card in the control plane's cards directory, merges and deploys nothing, is replaced by the next successful approve_merge for the same revision, and is dropped when the user refines the confirmed revision or adopts a proposed one with confirm_intent. A revise_change on a confirmed card leaves it on the confirmed revision, which stays in force until the user adopts the proposal. get_change and confirm_intent return it in approval while the tests' commit chosen for the revision still equals suite_commit and (when a promotion target is configured) the promotion target's current binary digests still equal artifacts, or in unverified_approval when the tests' commit still matches but a configured promotion target cannot verify the candidate's binaries on that read.")
	Attribute("approved_by", String, "User who approved the result.")
	Attribute("report_digest", String, "SHA-256 digest of the approved CP4 verification report.")
	Attribute("candidate_commit", String, "Candidate commit the report was checked against.")
	Attribute("suite_id", String, "Suite identifier the tests at suite_commit declared in the report (shown, not compared).")
	Attribute("suite_commit", String, "Commit of the tests the report ran: the operator's stored evaluator, or the tests Surla accepted for the change's build.")
	Attribute("environment", String, "Dependency profile the tests at suite_commit declared in the report, such as sim or scripted-model (shown, not compared).")
	Attribute("artifacts", MapOf(String, String), "SHA-256 digest of each candidate binary the promotion target measured, keyed by binary name; empty when no promotion settings are configured.")
	Attribute("scenario_count", Int, "Number of declared scenarios in the report, every one of which passed.")
	OneOf("test_source", "Where the tests at suite_commit came from, derived on each read: evaluator, or written_tests with their commit and a notice.", func() {
		TypeName("ApprovalTestSource")
		Meta("oneof:json:flatten")
		Meta("oneof:type:field", "source")
		Attribute("evaluator", OperatorEvaluatorTests)
		Attribute("written_tests", AgentWrittenTests)
	})
	Required("approved_by", "report_digest", "candidate_commit", "suite_id", "suite_commit", "environment", "artifacts", "scenario_count", "test_source")
})

// UnverifiedApprovalItem wraps a saved CP5 approval whose current promotion
// inputs could not be verified on this read.
var UnverifiedApprovalItem = Type("UnverifiedApprovalItem", func() {
	Description("Saved Checkpoint 5 promotion approval recorded on the Change Card whose current release binaries could not be verified on this read.")
	Attribute("approval", ApprovalItem, "Recorded promotion approval from the Change Card.")
	Attribute("reason", String, "Explanation of why the recorded approval could not be verified against current release binaries and how to recover.")
	Required("approval", "reason")
})

// ProposedRevisionItem is a revision the guide proposed beside a confirmed
// Change Card, waiting for the user to adopt or decline it.
var ProposedRevisionItem = Type("ProposedRevisionItem", func() {
	Description("A revision the guide proposed with revise_change beside the confirmed revision in force. It waits until the user adopts it, by answering its questions and confirming its request_revision with confirm_intent, or declines it with decline_revision. Until then the confirmed revision stays in force with its answers, confirmation, approval, Stories and design source, and architecture preview, verification and approval keep working on it. A later revise_change naming this request_revision replaces it.")
	Attribute("prompt", String, "Operator feature request for this revision.")
	Attribute("summary", String, "Plain-English summary of the proposed change.")
	Attribute("ambiguities", ArrayOf(AmbiguityQuestion), "Domain questions the user answers when adopting this revision.")
	Attribute("examples", ArrayOf(ExampleItem), "Concrete examples mapped to evaluator checks.")
	Attribute("preserved_behaviors", ArrayOf(String), "Existing contract invariants preserved by this change.")
	Attribute("stories", ArrayOf(StorySummaryItem), "Evaluated domain Stories of this revision in declaration order; omitted when it has none. Included in its request_revision.")
	Attribute("request_revision", String, "SHA-256 digest of this proposed revision: confirm_intent adopts it, decline_revision declines it, and revise_change replaces it when given this value.")
	Attribute("ui_composition", UICompositionItem, "Decision view the guide composed for this revision in its analysis, when it sent one.")
	Attribute("changed_design_files", ArrayOf(String), "Paths of the design/*.go files that differ between the design source the confirmed revision was made from and the one this revision was made from (added, removed or changed), in path order; empty when both were made from the same design files. Adopting the revision binds it to its own design source.")
	Required("prompt", "summary", "examples", "preserved_behaviors", "request_revision", "changed_design_files")
})

// ChangeCardResult is the output of CP1 propose_change, revise_change,
// confirm_intent, decline_revision, approve_merge and get_change.
var ChangeCardResult = Type("ChangeCardResult", func() {
	Description("Checkpoint 1 Change Card containing examples, preserved behaviors, optional adopted domain Stories, and audit metadata.")
	Attribute("change_id", String, "Change identifier.")
	Attribute("status", String, "Checkpoint status (needs_confirmation or confirmed).", func() {
		Enum("needs_confirmation", "confirmed")
	})
	Attribute("prompt", String, "Operator feature request for this revision.")
	Attribute("summary", String, "Plain-English summary of the proposed change from propose_change or revise_change. confirm_intent does not rewrite this text; when status is confirmed, answers records the owner's resolutions for ambiguities.")
	Attribute("ambiguities", ArrayOf(AmbiguityQuestion), "Domain questions surfaced at CP1, retained for audit after confirmation.")
	Attribute("answers", MapOf(String, String), "Owner-confirmed answers keyed by ambiguity question ID when status is confirmed and ambiguities is non-empty (empty when a confirmed card has no ambiguity questions). Each answer equals one of its question's declared options and takes precedence over open phrasing in summary.")
	Attribute("examples", ArrayOf(ExampleItem), "Concrete examples mapped to evaluator checks.")
	Attribute("preserved_behaviors", ArrayOf(String), "Existing contract invariants preserved by this change.")
	Attribute("stories", ArrayOf(StorySummaryItem), "Evaluated domain Stories adopted from refresh_story for this Change Card revision in declaration order; omitted when the card has no Stories. Included in request_revision.")
	Attribute("confirmed_by", String, "User who confirmed the Change Card.")
	Attribute("request_revision", String, "SHA-256 digest of the canonical versioned Change Card.")
	Attribute("ui_composition", UICompositionItem, "Decision view the caller composed for this revision in its analysis, when it sent one. A confirm_intent that changes answers or examples returns the new revision without it.")
	Attribute("approval", ApprovalItem, "Checkpoint 5 approval of this revision, present while suite_commit is still the tests' commit chosen for this revision and (when a promotion target is configured) the promotion target's current binary digests still equal artifacts on this read. It is absent before approve_merge succeeds, whenever the chosen tests' commit or the staged binary digests have changed since, and whenever a configured promotion target's staged binaries cannot be verified (see unverified_approval). It is never hashed into request_revision.")
	Attribute("unverified_approval", UnverifiedApprovalItem, "Saved Checkpoint 5 approval of this revision when suite_commit still matches the chosen tests' commit and promotion settings are configured, but the staged binaries could not be verified on this read. At most one of approval or unverified_approval is set. It is never saved to disk or hashed into request_revision.")
	Attribute("proposed_revision", ProposedRevisionItem, "The revision the guide proposed beside this confirmed revision, while it waits for the user to adopt or decline it; absent otherwise. This card's own fields describe the revision in force.")
	Required("change_id", "status", "prompt", "summary", "examples", "preserved_behaviors", "request_revision")
})

// ConfirmIntentPayload locks the operator's confirmed ambiguity answers and example table at CP1.
var ConfirmIntentPayload = Type("ConfirmIntentPayload", func() {
	Description("Operator confirmation of Checkpoint 1 ambiguity answers and concrete examples.")
	Attribute("change_id", String, "Change identifier.")
	Attribute("request_revision", String, "Reviewed CP1 request_revision digest being confirmed.")
	Attribute("answers", MapOf(String, String), "Selected resolution per ambiguity question ID. Answer every current question, and give each value exactly as one string from that question's options, character for character. A decision that no option expresses goes to the guide, which records it with revise_change; the user then confirms the resulting revision.")
	Attribute("examples", ArrayOf(ExampleItem), "The complete confirmed example table, including examples you edited or added. Keep an existing example's id when you edit it, and give an example you add a new, unique, non-blank id; ids are saved as sent.")
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
	Attribute("requires_confirmation", Boolean, "True when the tool declares a design-time Confirmation gate. This is a declaration: it does not show that the application delivers the approval request or continues after the answer.")
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

// DesignSnapshot is an immutable copy of a Go module holding a Goa design.
var DesignSnapshot = Type("DesignSnapshot", func() {
	Description("Exact files of one Go module containing a Goa/Goa-AI design package at design/. Paths are clean, relative to the module root, and name only .go, go.mod and go.sum files; go.mod, go.sum and at least one design/*.go file are required. The evaluator stages exactly these bytes, resolves dependencies only from go.mod and go.sum, and never reads host paths.")
	Attribute("files", MapOf(String, String), "File contents keyed by module-relative path, such as design/design.go or go.mod.")
	Required("files")
})

// PreviewArchitecturePayload is the input to the CP2 preview_architecture tool.
var PreviewArchitecturePayload = Type("PreviewArchitecturePayload", func() {
	Description("Request to derive Checkpoint 2 C4 topology and guardrails from the bound workspace's candidate Goa design for a confirmed Change Card.")
	Attribute("change_id", String, "Confirmed Change Card identifier.")
	Attribute("request_revision", String, "Expected CP1 request_revision digest.")
	Required("change_id", "request_revision")
})

// ArchitecturePreviewResult is the output of the CP2 preview_architecture tool.
var ArchitecturePreviewResult = Type("ArchitecturePreviewResult", func() {
	Description("Checkpoint 2 C4 container topology, tool contracts, per-agent Goa-AI guardrails, derived from the evaluated candidate design.")
	Attribute("change_id", String, "Bound Change Card identifier.")
	Attribute("request_revision", String, "Bound CP1 request_revision digest.")
	Attribute("design_digest", String, "SHA-256 digest of the evaluated design snapshot (sorted path and content pairs).")
	Attribute("system_name", String, "API or primary service name declared in the design.")
	Attribute("containers", ArrayOf(C4ContainerItem), "Derived C4 containers.")
	Attribute("edges", ArrayOf(C4EdgeItem), "Validated directed edges between declared containers.")
	Attribute("agent_policies", ArrayOf(AgentPolicyItem), "Evaluated Goa-AI runtime policies per declared agent.")
	Attribute("tools", ArrayOf(ToolContractItem), "Evaluated Goa-AI tool contracts.")
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
		"banner",
	)
})

// DraftOverridePayload is the input to the CP3 Fast Draft Mode tool.
var DraftOverridePayload = Type("DraftOverridePayload", func() {
	Description("Fast Draft Mode request to hot-reload a prompt template on the live target start_live_target runs, or reject a compiled business-rule edit.")
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
	Attribute("applied_in_store", Boolean, "True when the live target saved the prompt text in its prompt store for 0s reload.")
	Attribute("requires_rebuild", Boolean, "True when the change modifies a compiled Go business rule.")
	Attribute("prompt_version", String, "Version of the accepted prompt text, as the live target returned it.")
	Attribute("message", String, "Operator-facing status explanation.")
	Required("applied_in_store", "requires_rebuild", "message")
})

// StartLiveTargetPayload names the candidate commit a CP3 live target runs.
var StartLiveTargetPayload = Type("StartLiveTargetPayload", func() {
	Description("Owned, confirmed change and candidate commit to build and run as this server's live CP3 target.")
	Attribute("change_id", String, "Owned, confirmed change from list_builder_assignments.", func() { MinLength(1) })
	Attribute("candidate_commit", String, "Full commit ID to build, in the candidate repository's object format.", func() {
		Pattern("^([0-9a-f]{40}|[0-9a-f]{64})$")
		Example("4f0b3c1d2e5a6978877665544332211009988776")
	})
	Required("change_id", "candidate_commit")
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

// LiveTargetNone is the live target state when none runs.
var LiveTargetNone = Type("LiveTargetNone", func() {
	Description("No live target is running.")
})

// LiveTargetStarting is a live target that is being built and started.
var LiveTargetStarting = Type("LiveTargetStarting", func() {
	Description("A live target is being built from a commit and started. It cannot be stopped until the start finishes.")
	Attribute("candidate_commit", String, "The commit being built.")
	Attribute("since", String, "When the start began.", func() { Format(FormatDateTime) })
	Required("candidate_commit", "since")
})

// LivePromptItem is one prompt whose draft a running live target accepted.
var LivePromptItem = Type("LivePromptItem", func() {
	Description("A prompt whose text the live target replaced with an accepted draft, and the version now in force. There is one entry per prompt: a later draft of the same prompt replaces it. The text itself is never returned.")
	Attribute("prompt_id", String, "Registered prompt identifier.")
	Attribute("version", String, "Version of the text in force, as the live target returned it.")
	Attribute("accepted_at", String, "When the live target first accepted the version in force. A later draft that the target answers with the same version, such as the same text sent again, keeps this time.", func() { Format(FormatDateTime) })
	Required("prompt_id", "version", "accepted_at")
})

// RunningLiveTarget is a live target that is running.
var RunningLiveTarget = Type("RunningLiveTarget", func() {
	Description("A running live target.")
	Attribute("candidate_commit", String, "The commit it was built from.")
	Attribute("url", String, "The origin it listens on, as its evaluator suite reported it.")
	Attribute("started_at", String, "When it became ready.", func() { Format(FormatDateTime) })
	Attribute("prompts", ArrayOf(LivePromptItem), "One entry per prompt whose draft it accepted, sorted by prompt_id; empty when none.")
	Required("candidate_commit", "url", "started_at", "prompts")
})

// LiveTargetNotConfirmedStopped is a live target whose processes could not
// be confirmed stopped.
var LiveTargetNotConfirmedStopped = Type("LiveTargetNotConfirmedStopped", func() {
	Description("A live target whose processes could not be confirmed stopped. No other live target can start until they are.")
	Attribute("candidate_commit", String, "The commit it was built from.")
	Attribute("message", String, "What happened and what to do next.")
	Required("candidate_commit", "message")
})

// LiveTargetEnded is the last ready target that ended unexpectedly after
// the server confirmed that all its processes stopped.
var LiveTargetEnded = Type("LiveTargetEnded", func() {
	Description("The last ready target ended unexpectedly and its processes are confirmed stopped. This in-memory observation lasts until an explicit stop, an accepted replacement, or a server restart. It contains no address, prompt, credential or provider detail.")
	Attribute("candidate_commit", String, "The commit the ended target ran.")
	Attribute("started_at", String, "When that target became ready.", func() { Format(FormatDateTime) })
	Attribute("ended_at", String, "When the server observed that target ending.", func() { Format(FormatDateTime) })
	Attribute("message", String, "Fixed explanation of what is known and the builder's next step.")
	Required("candidate_commit", "started_at", "ended_at", "message")
})

// LiveTargetStatus is this server's live target in exactly one state.
var LiveTargetStatus = Type("LiveTargetStatus", func() {
	Description("This server's live target, as get_live_target reports it.")
	OneOf("live_target", "none when no live target runs; starting while one is built and started; running with its commit, address, start time and the prompts whose drafts are in force; not_confirmed_stopped when its processes could not be confirmed stopped; ended when a ready target ended unexpectedly and cleanup is confirmed.", func() {
		Meta("oneof:json:flatten")
		Meta("oneof:type:field", "state")
		Attribute("none", LiveTargetNone)
		Attribute("starting", LiveTargetStarting)
		Attribute("running", RunningLiveTarget)
		Attribute("not_confirmed_stopped", LiveTargetNotConfirmedStopped)
		Attribute("ended", LiveTargetEnded)
	})
	Required("live_target")
})

// ModelNone is the model connection when no model is chosen.
var ModelNone = Type("ModelNone", func() {
	Description("No model is chosen. A written-tests live target requires a model choice; a stored evaluator may start without one.")
})

// ModelDirect is a direct model provider and model name.
var ModelDirect = Type("ModelDirect", func() {
	Description("A direct model provider and model name. The provider's request settings stay in the project server's memory and are never returned.")
	Attribute("provider", String, "Direct model provider.", func() {
		Enum("anthropic", "openai")
	})
	Attribute("model_name", String, "Provider model identifier.", func() {
		MinLength(1)
	})
	Required("provider", "model_name")
})

// ModelGateway is a running goa-ai model gateway.
var ModelGateway = Type("ModelGateway", func() {
	Description("A running goa-ai model gateway, which chooses the model.")
	Attribute("gateway_addr", String, "Plaintext gRPC host:port of the gateway.", func() {
		MinLength(1)
	})
	Required("gateway_addr")
})

// ModelConnection is the model choice live targets started afterwards use.
var ModelConnection = Type("ModelConnection", func() {
	Description("The model live targets started afterwards use, as get_model_connection reports it.")
	OneOf("connection", "none when no model is chosen; direct with a provider and model name; gateway with a model gateway address.", func() {
		Meta("oneof:json:flatten")
		Meta("oneof:type:field", "kind")
		Attribute("none", ModelNone)
		Attribute("direct", ModelDirect)
		Attribute("gateway", ModelGateway)
	})
	Required("connection")
})

// ApproveMergePayload is the input to the CP5 approve_merge tool.
var ApproveMergePayload = Type("ApproveMergePayload", func() {
	Description("Identifies the confirmed Change Card revision, the candidate commit being promoted, and the verification report to check. Which tests' commit the report must have run comes from the project's stored verification settings or the tests Surla accepted for the revision's build, and the report itself and the measured candidate binary digests come from the control plane's promotion target, never from the caller.")
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
	Description("Names the confirmed Change Card revision and the candidate commit to verify. What is built and which evaluator suite runs come from the operator's project settings, never from the caller.")
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
	Attribute("suite_commit", String, "Commit of the tests the run uses: the operator's evaluator, or the tests accepted for the change's build.")
	Attribute("accepted_at", String, "When the run was accepted.", func() { Format(FormatDateTime) })
	Attribute("inputs_current", Boolean, "Whether this run's agreed revision and selected tests match the current Change Card and usable build or stored evaluator. This observation does not establish the current candidate working tree, deployment or approval.")
	Attribute("candidate_binaries_dir", String, "Absolute folder containing the binaries built by this completed run. Returned only to the user when promotion settings are configured, so the user can publish these exact binaries before approval. Agents never receive this private path.", func() { MinLength(1) })
	OneOf("test_source", "Where the run's tests came from, recorded when the run started: evaluator, or written_tests with their commit and a notice.", func() {
		Meta("oneof:json:flatten")
		Meta("oneof:type:field", "source")
		Attribute("evaluator", OperatorEvaluatorTests)
		Attribute("written_tests", AgentWrittenTests)
	})
	OneOf("state", "Exactly one of running, completed or not_completed.", func() {
		Attribute("running", VerificationRunning)
		Attribute("completed", VerificationCompleted)
		Attribute("not_completed", VerificationNotCompleted)
	})
	Required("run_id", "change_id", "request_revision", "candidate_commit", "suite_commit", "accepted_at", "inputs_current", "test_source", "state")
})

// OperatorEvaluatorTests is a run whose tests are the operator's evaluator.
var OperatorEvaluatorTests = Type("OperatorEvaluatorTests", func() {
	Description("The run used the operator's evaluator: the one stored with surla verification set, or the one named on surla verify's command line.")
})

// AgentWrittenTests is a run whose tests a test writer on this machine wrote.
var AgentWrittenTests = Type("AgentWrittenTests", func() {
	Description("The run used the tests Surla accepted for the change's build, which a separate agent on this machine wrote.")
	Attribute("commit", String, "Commit Surla recorded the accepted tests at; the same as suite_commit.")
	Attribute("notice", String, "Always: These tests were written by a separate agent on this machine and are not isolated from the builder.")
	Required("commit", "notice")
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
	Error("forbidden", ErrorResult, "The authenticated caller may not perform this operation or does not own the resource.")
	HTTP(func() { Response("forbidden", StatusForbidden) })

	MCP("surla", "1.0.0")
	JSONRPC(func() {
		POST("/mcp")
	})

	Method("golden_path", func() {
		Description("Serve the Surla Golden Path architecture playbook for Goa-AI agentic platforms, followed by the decision component catalog for the optional ui_composition of a propose_change or revise_change analysis.")
		Result(String)
		Resource("golden_path", "surla://golden-path", "text/markdown")
		HTTP(func() {
			GET("/golden-path")
			Response(StatusOK)
		})
	})

	Method("propose_change", func() {
		Description("Save a new Checkpoint 1 Change Card from a feature request and the caller's analysis of it. Surla checks the analysis's structure and cross-field rules, gives ambiguities the IDs <change_id>-ambiguity-NN and examples the IDs check_01, check_02, ... in list order, binds the card to the authenticated owner, records the design source (or, when story_refresh_op_id is given, reads one workspace snapshot, verifies its digest against the owner's current succeeded refresh_story operation, and records both its evaluated Stories and its design/*.go files), and computes request_revision. Every card waits for user confirmation, including a card without ambiguities. Surla runs no model for this step and cannot judge whether the analysis is complete or correct.")
		Payload(ProposeChangePayload)
		Result(ChangeCardResult)
		Error("conflict", ErrorResult, "A Change Card with this change_id already exists; use revise_change or a new change_id.")
		Error("unprocessable", ErrorResult, "The request could not be accepted: change_id or prompt is blank, an ambiguity repeats an option, story_refresh_op_id does not name a current succeeded refresh_story operation matching the bound workspace snapshot, or the bound workspace's design/*.go could not be read. The message says which; nothing was saved.")
		Tool("propose_change", "Propose a feature change as a Checkpoint 1 Change Card. Read the target's design/*.go and the surla://golden-path playbook first, then send the request in prompt with your analysis of it: a summary, the open questions only the requester can answer (an empty list when none), concrete examples that become checks, and the existing behaviors the change must keep. When adopting a workspace Story, pass the succeeded refresh_story operation's op_id in story_refresh_op_id. Surla assigns the IDs and request_revision and saves the card; every card awaits the user's confirmation, including a card without open questions. When story_refresh_op_id is omitted and a workspace is bound, Surla records design/*.go from the bound workspace.")
		HTTP(func() {
			POST("/changes/propose")
			Response(StatusOK)
			Response("conflict", StatusConflict)
			Response("unprocessable", StatusUnprocessableEntity)
		})
	})

	Method("revise_change", func() {
		Description("Save the next revision of an existing Checkpoint 1 Change Card from a revised feature request and the guide's analysis of it. Only the guide revises a change; only the user confirms or adopts a revision, with confirm_intent on the views page. The request is bound to the latest revision: the proposed revision's request_revision when one waits, otherwise the card's. On a card that was never confirmed, the new revision replaces it. On a confirmed card, the new revision is saved as the proposed revision beside the confirmed one, replacing any earlier proposal, and the confirmed revision stays in force with its answers, confirmation, approval, Stories and design source until the user adopts the proposal with confirm_intent or declines it. The analysis accounts for every example of the revision it revises exactly once, keeping or editing it through prior_id or dropping it through removed_example_ids. Surla keeps the IDs of kept examples, gives new examples the lowest check_NN ID neither card uses, adopts the evaluated Stories and design/*.go from the single workspace snapshot verified against story_refresh_op_id when story_refresh_op_id is given (or keeps the saved Stories and their design source together when story_refresh_op_id is omitted on a card with Stories, or records design/*.go from the bound workspace again when omitted on a card without Stories), and saves the new revision in needs_confirmation status for the user's review. The new revision has no answers, confirmed_by or approval: earlier decisions carry over only where the analysis restates them in its summary, examples or preserved behaviors. The result is the change as get_change returns it. Surla runs no model for this step and cannot judge whether the analysis is complete or correct.")
		Payload(ReviseChangePayload)
		Result(ChangeCardResult)
		Error("not_found", ErrorResult, "No Change Card exists with this change_id.")
		Error("conflict", ErrorResult, "request_revision does not name the latest revision (the waiting proposal's, when one waits); read the change with get_change.")
		Error("unprocessable", ErrorResult, "The request could not be accepted: change_id, request_revision or prompt is blank, an ambiguity repeats an option, the analysis does not account for every example of the revision it revises exactly once, story_refresh_op_id does not name a current succeeded refresh_story operation matching the bound workspace snapshot, or the bound workspace's design/*.go could not be read. The message says which; the card did not change.")
		Tool("revise_change", "Revise an existing Checkpoint 1 Change Card with updated requirements. Name the latest revision you read in request_revision: proposed_revision.request_revision when get_change shows a proposal waiting, otherwise the card's. Send the revised request in prompt with a new analysis made against that revision and the target's design: keep or edit each of its examples through prior_id, list the dropped ones in removed_example_ids, and restate the confirmed decisions that still hold in summary, examples or preserved_behaviors. Pass story_refresh_op_id from a succeeded refresh_story operation to adopt an updated workspace Story, or omit it to keep the Story and design source of the revision you revise. On a confirmed card your revision becomes a proposal beside the confirmed one, which stays in force until the user adopts or declines the proposal on the views page; you cannot withdraw a proposal, only replace it by revising again.")
		HTTP(func() {
			POST("/changes/revise")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
			Response("conflict", StatusConflict)
			Response("unprocessable", StatusUnprocessableEntity)
		})
	})

	Method("confirm_intent", func() {
		Description("Confirm or refine a Checkpoint 1 Change Card with the complete ambiguity answers and example table, or adopt the revision the guide proposed beside a confirmed card. Only the user's browser session calls it, from the views page; the guide revises with revise_change and never confirms. Naming the proposed revision's request_revision adopts it: its questions are answered, it becomes the confirmed revision in force under a new request_revision with no approval, and the proposal is gone. While a proposal waits, refining the confirmed revision is refused: adopt or decline the proposal first. Otherwise the request names the card's request_revision: changed answers or examples produce a new request_revision; unchanged ones return the card as it is, with a saved approval returned in approval only while its suite_commit is still the tests' commit chosen for this revision and either no promotion target is configured or its binary digests still match the promotion target's current binaries, or in unverified_approval when a promotion target is configured and cannot measure the candidate's binaries. A decision that no option expresses goes to the guide, which records it with revise_change; the user then confirms the resulting revision.")
		Payload(ConfirmIntentPayload)
		Result(ChangeCardResult)
		Error("not_found", ErrorResult, "No Change Card exists with this change_id.")
		Error("conflict", ErrorResult, "request_revision names neither the card's revision nor a waiting proposal's, or it names the confirmed revision while a proposal waits (adopt or decline the proposal first); read the change with get_change.")
		Error("unprocessable", ErrorResult, "The submitted answers or examples did not satisfy the Change Card's requirements; the message states which check failed.")
		Tool("confirm_intent", "Confirm or refine Checkpoint 1 ambiguity answers and example checks for the card's current request_revision, or adopt a waiting proposed revision by naming its request_revision with answers to its questions. A decision that no option expresses goes to the guide, which records it with revise_change; the user then confirms the resulting revision.")
		HTTP(func() {
			POST("/changes/confirm")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
			Response("conflict", StatusConflict)
			Response("unprocessable", StatusUnprocessableEntity)
		})
	})

	Method("get_change", func() {
		Description("Return the current Change Card, including its request_revision, to its owner, for example after confirm_intent or revise_change refused an edit made against an older revision. The card's fields describe the revision in force; a revision the guide proposed beside a confirmed card is returned in proposed_revision while it waits. Reading never confirms or changes the card. A recorded approval is returned in approval only while its suite_commit is still the tests' commit chosen for this revision and either no promotion target is configured or its binary digests still match the promotion target's current binaries, or in unverified_approval when a promotion target is configured and cannot measure the candidate's binaries.")
		Payload(GetChangePayload)
		Result(ChangeCardResult)
		Error("not_found", ErrorResult, "No Change Card exists with this change_id.")
		Tool("get_change", "Read the current Checkpoint 1 Change Card, its request_revision, and any proposed revision waiting beside it.")
		HTTP(func() {
			GET("/changes/{change_id}")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
		})
	})

	Method("decline_revision", func() {
		Description("Decline the revision the guide proposed beside a confirmed Checkpoint 1 Change Card. The proposal is removed and the confirmed revision in force stays exactly as it was, with its answers, confirmation, approval, Stories and design source. Only the user may decline; the guide replaces a proposal by revising again. The result is the change as get_change returns it.")
		Payload(DeclineRevisionPayload)
		Result(ChangeCardResult)
		Error("not_found", ErrorResult, "No Change Card exists with this change_id.")
		Error("conflict", ErrorResult, "No proposed revision waits, or request_revision does not name it; read the change with get_change.")
		Tool("decline_revision", "Decline the revision the guide proposed beside a confirmed Change Card, naming proposed_revision.request_revision; the confirmed revision stays in force.")
		HTTP(func() {
			POST("/changes/decline")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
			Response("conflict", StatusConflict)
		})
	})

	Method("preview_architecture", func() {
		Description("Extract and validate the Checkpoint 2 C4 container topology, tool contracts, and Goa-AI guardrails from a candidate Goa design bound to a confirmed Change Card. Surla reads the candidate module snapshot from the bound workspace. The result reports what the design declares; it does not compare the design with the Change Card's requirements.")
		Payload(PreviewArchitecturePayload)
		Result(ArchitecturePreviewResult)
		Error("not_found", ErrorResult, "No Change Card exists with this change_id.")
		Error("conflict", ErrorResult, "The Change Card is not confirmed or its request_revision does not match; confirm the card or read the current revision with get_change.")
		Error("unprocessable", ErrorResult, "The request or evaluated design did not meet Checkpoint 2 requirements; the message states which check failed.")
		Tool("preview_architecture", "Preview Checkpoint 2 C4 container topology and Goa-AI guardrails for a confirmed Change Card. Surla reads the candidate module from the bound workspace. It reports what the design declares; it does not compare the design with the Change Card's requirements.")
		HTTP(func() {
			POST("/changes/preview")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
			Response("conflict", StatusConflict)
			Response("unprocessable", StatusUnprocessableEntity)
		})
	})

	Method("draft_override", func() {
		Description("Apply a Fast Draft Mode prompt override on the live target start_live_target runs, or reject a compiled-rule prompt hack. Surla changes prompts only in a live target it built and started itself, so a prompt_template override with no live target running is refused with no_live_target. A rejected update leaves the target's text unchanged; update_not_confirmed means the target may or may not have applied it.")
		Payload(DraftOverridePayload)
		Result(DraftOverrideResult)
		Error("no_live_target", ErrorResult, "No live target is running, so nothing changed. Call start_live_target with a candidate commit first.")
		Error("prompt_not_found", ErrorResult, "The live target has no prompt with this ID; nothing changed.")
		Error("invalid_prompt", ErrorResult, "The live target rejected the template because it does not render; nothing changed.")
		Error("live_target_ended", ErrorResult, "The live target is no longer running; start it again with start_live_target.")
		Error("target_rejected_credential", ErrorResult, "The live target rejected this server's key; nothing changed. Start the target again with start_live_target.")
		Error("update_not_confirmed", ErrorResult, "The live target did not confirm the update, so it may or may not have applied it. Keep the draft and send it again.")
		Tool("draft_override", "Hot-reload a prompt template in 0s or flag a compiled business rule change. A prompt template needs a running live target: call start_live_target with a candidate commit first.")
		HTTP(func() {
			POST("/sandbox/draft")
			Response(StatusOK)
			Response("no_live_target", StatusConflict)
			Response("prompt_not_found", StatusNotFound)
			Response("invalid_prompt", StatusUnprocessableEntity)
			Response("live_target_ended", StatusConflict)
			Response("target_rejected_credential", StatusBadGateway)
			Response("update_not_confirmed", StatusBadGateway)
		})
	})

	Method("start_live_target", func() {
		Description("Build the candidate commit for the owner's confirmed change and bind draft_override to this server's one live target. Stored verification settings use the operator's evaluator and need no build. Otherwise the change's current revision must have a build with accepted tests: the candidate programs come from candidate_commit and the test program from the accepted tests commit. Written tests require a chosen model; with a direct provider the executor serves a private loopback model gateway and passes no provider setting to the program. Card, tests, model and plan refusals preserve the running target. After those checks a replacement stops the old target and discards its drafts, even if the new startup fails. A target that ends unexpectedly is reported as ended after confirmed cleanup; a server restart leaves none.")
		Error("not_found", ErrorResult, "No owned Change Card exists with this change_id.")
		Error("conflict", ErrorResult, "The change is not confirmed, a required commit is unavailable, or the project folder was replaced.")
		Error("no_build", ErrorResult, "The current change revision has no usable build.")
		Error("tests_not_accepted", ErrorResult, "The current build has no accepted tests; the message names who acts next.")
		Error("no_test_layout", ErrorResult, "The candidate or accepted tests lacks the required test layout.")
		Payload(StartLiveTargetPayload)
		Result(LiveTargetResult)
		Error("live_target_unavailable", ErrorResult, "This server has no verification configuration, so it cannot build or run a live target.")
		Error("live_target_failed", ErrorResult, "The live target could not be built or started; the message says which step failed.")
		Tool("start_live_target", "Start a live target for change_id from list_builder_assignments and candidate_commit so draft_override can try prompt text. The change must be owned and confirmed. Without stored verification settings its current build must have accepted tests, which start the target, and a model must be chosen. A refusal naming the user's step means tell the user. Refused preflight preserves a running target; an accepted replacement discards its drafts even if startup then fails. Commit prompt text to preserve it.")
		HTTP(func() {
			POST("/live-target")
			Response("not_found", StatusNotFound)
			Response("conflict", StatusConflict)
			Response("no_build", StatusConflict)
			Response("tests_not_accepted", StatusConflict)
			Response("no_test_layout", StatusConflict)
			Response(StatusOK)
			Response("live_target_unavailable", StatusNotImplemented)
			Response("live_target_failed", StatusUnprocessableEntity)
		})
	})

	Method("stop_live_target", func() {
		Description("Stop this server's live CP3 target, if one runs, and unbind draft_override from it. Stopping when none runs is not an error.")
		Result(StopLiveTargetResult)
		Error("live_target_stop_failed", ErrorResult, "The live target's processes could not be confirmed stopped; stop and start Surla.")
		Tool("stop_live_target", "Stop this server's live CP3 target, if one runs. This discards every prompt draft applied to it; prompt text survives only when it is committed.")
		HTTP(func() {
			DELETE("/live-target")
			Response(StatusOK)
			Response("live_target_stop_failed", StatusInternalServerError)
		})
	})

	Method("get_live_target", func() {
		Description("Report this server's live target: none; starting, with the commit being built; running, with its commit, its address, when it became ready and, for each prompt whose draft it accepted, the prompt identifier, the version in force and when it was accepted; ended after an unexpected ending with confirmed cleanup, or not confirmed stopped, with what to do next. Prompt text is never returned. It changes nothing and saves nothing: the answer lasts as long as the live target and this server.")
		Result(LiveTargetStatus)
		Tool("get_live_target", "Report this server's live target. It changes nothing. It answers none; starting, with the commit being built; running, with its commit, its address, when it became ready and, for each prompt whose draft it accepted, the prompt identifier, the version in force and when it was accepted (never the text); ended with commit, start and observed end times and a safe next step, or not_confirmed_stopped, with what to do next.")
		HTTP(func() {
			GET("/live-target")
			Response(StatusOK)
		})
	})

	Method("verify_change", func() {
		Description("Start Checkpoint 4 verification of a candidate commit for the owner's confirmed Change Card revision and return its run_id at once. With verification settings stored with surla verification set, the run uses the operator's evaluator and needs no build. Otherwise the user's build of that revision must exist and Surla must have accepted its tests: the run builds the candidate's programs from the candidate commit and runs the generated entry point of the tests at the accepted commit. The server builds the commit in Surla's sandbox and runs the tests in a separate process that keeps running when the caller disconnects or the server restarts; get_verification reads its state. Only the builder view of the report is ever returned, with where its tests came from.")
		Payload(VerifyChangePayload)
		Result(VerificationStatusResult)
		Error("not_found", ErrorResult, "No Change Card exists with this change_id.")
		Error("conflict", ErrorResult, "The Change Card is not confirmed or its request_revision does not match, the candidate commit is not in the project's repository, or the project folder was replaced after the server started; the message ends with the step.")
		Error("no_build", ErrorResult, "Without stored verification settings: the user has not built the change's revision in force with a test writer, so no tests are accepted for it. The message names the revision built, if any, and ends with the user's step.")
		Error("tests_not_accepted", ErrorResult, "Without stored verification settings: Surla has not accepted the tests of the change's build. The message names the test writer's state and who acts, and ends with the step.")
		Error("no_test_layout", ErrorResult, "Without stored verification settings: a commit lacks the test layout surla init makes, the accepted tests' commit is no longer in the project's repository, or the project folder is not in a git repository. The message names the commit or folder and what is missing, and ends with the step.")
		Error("not_implemented", ErrorResult, "This server has no verification configuration, so it cannot run Checkpoint 4 verifications.")
		Tool("verify_change", "Start CP4 verification of a candidate commit for the confirmed Change Card revision; returns a run_id to read with get_verification. Unless the project stores verification settings, it is refused until the user has built that revision and Surla has accepted its tests: your build assignment says when they are.")
		HTTP(func() {
			POST("/changes/{change_id}/verifications")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
			Response("conflict", StatusConflict)
			Response("no_build", StatusConflict)
			Response("tests_not_accepted", StatusConflict)
			Response("no_test_layout", StatusConflict)
			Response("not_implemented", StatusNotImplemented)
		})
	})

	Method("get_verification", func() {
		Description("Return the current state of a verification run to the owner who started it: running, completed with the builder view of its report, or not completed with where it stopped, and where its tests came from. A run keeps the Change Card revision it was started for. The test writer reads only the runs of a change it has an assignment for.")
		Payload(GetVerificationPayload)
		Result(VerificationStatusResult)
		Error("not_found", ErrorResult, "No verification run exists with this run_id.")
		Error("not_implemented", ErrorResult, "This server has no verification configuration, so it cannot read verification runs.")
		Tool("get_verification", "Read the state of a CP4 verification run started by verify_change.")
		HTTP(func() {
			GET("/verifications/{run_id}")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
			Response("not_implemented", StatusNotImplemented)
		})
	})

	Method("list_verifications", func() {
		Description("Return all Checkpoint 4 verification runs started by the owner for a Change Card, ordered newest first by accepted_at (tie-broken by run_id), with each run's current state, the Change Card revision it was started for and where its tests came from. Works whether the Change Card is currently confirmed or in needs_confirmation after a later revision. The test writer lists only the runs of a change it has an assignment for.")
		Payload(ListVerificationsPayload)
		Result(VerificationListResult)
		Error("not_found", ErrorResult, "No Change Card exists with this change_id.")
		Error("not_implemented", ErrorResult, "This server has no verification configuration, so it cannot list verification runs.")
		Tool("list_verifications", "List all CP4 verification runs started for a Change Card, newest first.")
		HTTP(func() {
			GET("/changes/{change_id}/verifications")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
			Response("not_implemented", StatusNotImplemented)
		})
	})

	Method("approve_merge", func() {
		Description("Record Checkpoint 5 approval for the owner's confirmed Change Card revision. The control plane chooses the tests' commit the same way verify_change does (the suite commit in the project's stored verification settings, or the tests Surla accepted for the change's build at request_revision), loads the verification report by digest, re-derives the report from its evidence with verify.Approve, and records the approval on the card when the report passed for candidate_commit, request_revision, and that tests' commit. When promotion settings are configured with surla promotion set (or a PromotionTarget is bound), approve_merge also checks that the staged binaries in <release_dir>/<candidate_commit>/ match the report's binary digests and records those digests in artifacts; without promotion settings, no release folder or staged binaries are required and artifacts is empty. The report's suite_id and environment are what the tests at suite_commit declared and are recorded on the approval without a separate comparison. Nothing is merged or deployed.")
		Payload(ApproveMergePayload)
		Result(ChangeCardResult)
		Error("not_found", ErrorResult, "No Change Card exists with this change_id.")
		Error("conflict", ErrorResult, "The Change Card is not confirmed, its request_revision does not match, or the accepted tests changed while approving; confirm the card, read the current revision with get_change, or approve again.")
		Error("no_build", ErrorResult, "Without stored verification settings: the user has not built the change's revision in force with a test writer, so no tests are accepted for it. The message names the revision built, if any, and ends with the user's step.")
		Error("tests_not_accepted", ErrorResult, "Without stored verification settings: Surla has not accepted the tests of the change's build. The message names the test writer's state and who acts, and ends with the step.")
		Error("unprocessable", ErrorResult, "The verification report did not pass or does not match the Change Card, the chosen tests' commit, or (when promotion settings are configured) the promotion target's staged binary digests.")
		Error("not_implemented", ErrorResult, "This control plane has no verification configuration bound to it.")
		Tool("approve_merge", "Approve the confirmed Change Card revision after its CP4 verification report passed for the candidate commit and the tests chosen for the revision. When promotion settings are configured with surla promotion set, also checks that the staged release binaries match the report.")
		HTTP(func() {
			POST("/changes/approve")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
			Response("conflict", StatusConflict)
			Response("no_build", StatusConflict)
			Response("tests_not_accepted", StatusConflict)
			Response("unprocessable", StatusUnprocessableEntity)
			Response("not_implemented", StatusNotImplemented)
		})
	})

	Method("diff", func() {
		Description("Compare evaluated Goa designs and go.mod directives without emitting a blanket compatibility verdict.")
		Payload(DiffPayload)
		Result(DiffResult)
		Tool("diff", "Compute structural Goa design and go.mod dependency diffs.")
		HTTP(func() {
			POST("/diff")
			Response(StatusOK)
		})
	})

	Method("configure_model", func() {
		Description("Choose, on a running control plane without restarting, the model that CP3 live targets started afterwards receive: a direct provider and model name, whose request settings the surla command handed to the project server with hand_over_model_settings, or a running gRPC model gateway. start_live_target passes the choice to the target's evaluator suite as SURLA_LIVE_MODEL_PROVIDER and SURLA_LIVE_MODEL_NAME, or as SURLA_LIVE_MODEL_GATEWAY, and gives it the chosen provider's held request settings.")
		Payload(ConfigureModelPayload)
		Result(ModelConfigResult)
		Error("unprocessable", ErrorResult, "The field combination is invalid, or the project server holds no request settings for the chosen provider; the message names the provider's key variable and how to hand it over.")
		Tool("configure_model", "Switch the model provider and model name (such as anthropic/claude-sonnet-4-6 or openai/gpt-5.4-mini, whose key the user exported in the shell that ran surla) or attach a running model gateway address, without restarting the project server. Live targets that start_live_target starts afterwards plan with this model.")
		HTTP(func() {
			POST("/cp1/model")
			Response(StatusOK)
			Response("unprocessable", StatusUnprocessableEntity)
		})
	})

	Method("get_model_connection", func() {
		Description("Report the model choice live targets started afterwards receive: none, a direct provider and model name, or a model gateway address. It starts as none. While no model is chosen, a hand_over_model_settings call that holds a provider's settings selects that provider's default model; every accepted configure_model call replaces the choice. It never returns an API key or any other setting value, and it changes nothing. Verification runs do not use this choice.")
		Result(ModelConnection)
		HTTP(func() {
			GET("/cp1/model")
			Response(StatusOK)
		})
	})

	Method("hand_over_model_settings", func() {
		Description("The surla command, after it confirms the project server's identity, hands over the request settings of each model provider whose key is set in the shell it runs in: the key and the listed settings that route and authenticate the provider's requests. The project server holds them in memory only and forgets them when it stops. A live target's test program, including a candidate commit's test module that agents wrote, receives the chosen provider's held settings; a verification with the operator's stored evaluator receives every held provider's settings; tests a separate agent wrote for a verification, and the check helper, receive none. An absent provider attribute keeps what is held for that provider. A present one replaces everything held for it and must include the provider's key variable; an empty one is refused, because it is neither a way to keep nor a way to forget. A request with neither attribute changes nothing and reports what is held. When no model is chosen and a provider is now held, live targets use that provider's default model, anthropic before openai; any other choice stays. The result names the held variables and the model choice, never a value. Only the surla command's management credential may call this operation; it is never an MCP tool.")
		Payload(HandOverModelSettingsPayload)
		Result(ModelSettingsResult)
		Error("unprocessable", ErrorResult, "A provider's settings do not include its key variable, or a value contains a NUL character. The message names the provider and the variable, never a value; nothing held changed.")
		HTTP(func() {
			POST("/cp1/model/settings")
			Response(StatusOK)
			Response("unprocessable", StatusUnprocessableEntity)
		})
	})

	Method("init_project", func() {
		Description("Initialize a new Surla starter project directory inside the server's bound workspace, generating the candidate Go module, Goa and Goa-AI code, and the eval/ verification harness, creating the initial Git commit, and switching the server's active workspace and trusted evaluator suite commit to that project. When the request includes a progressToken, the tool sends MCP progress updates as starter generation/tests, initial commit creation, and publication start.")
		Payload(InitProjectPayload)
		Result(InitProjectResult)
		Error("conflict", ErrorResult, "The workspace or target directory is already initialized.")
		Error("unprocessable", ErrorResult, "The module path or child directory name is invalid.")
		Error("not_implemented", ErrorResult, "This control plane has no bound workspace or starter generator configured.")
		Tool("init_project", "Initialize a new Surla starter project inside the bound workspace directory, create its initial Git commit, and switch the server's active workspace to that project. Returns the created commit ID in commit for start_live_target and verify_change.")
		HTTP(func() {
			POST("/workspace/init")
			Response(StatusOK)
			Response("conflict", StatusConflict)
			Response("unprocessable", StatusUnprocessableEntity)
			Response("not_implemented", StatusNotImplemented)
		})
	})

	Method("generate_code", func() {
		Description("Read the active workspace's application module, or its public local eval/ module when target is eval, compile that module's edited Goa design in Surla's sandbox, and atomically replace its gen/ directory. Preserve top-level user files and their permissions and leave go.mod and go.sum unchanged. Return the written paths and warnings; a warning after gen/ publication still means generation succeeded. The tool compiles the design only; it does not build the application or run tests. Fix a rejected design and call again on the same running server.")
		Payload(GenerateCodePayload)
		Result(GenerateCodeResult)
		Error("unprocessable", ErrorResult, "The selected local module is absent, replaced, or invalid; correct its local files and call again.")
		Error("not_implemented", ErrorResult, "No workspace directory is bound to this control plane.")
		Tool("generate_code", "Regenerate code from local edited design files without uploading source. Omit target for the application, or use target eval for the public local eval/ module. Return sorted module-relative files_written and warnings. This compiles the design only; run check_project to build and test the application before verification. Fix a rejected design and call again on the same server.")
		HTTP(func() {
			POST("/workspace/generate")
			Response(StatusOK)
			Response("unprocessable", StatusUnprocessableEntity)
			Response("not_implemented", StatusNotImplemented)
		})
	})

	Method("check_project", func() {
		Description("Copy the active workspace's live files (uncommitted edits included) into a protected per-operation copy and run go test ./... for every Go module in it, each in a Linux container with no network. Nothing in the workspace changes. Returns passed and a bounded message; read the full output with read_check_output.")
		Result(CheckProjectResult)
		Error("unprocessable", ErrorResult, "The workspace contains no Go module.")
		Error("not_implemented", ErrorResult, "No workspace directory is bound to this control plane.")
		Error("unavailable", ErrorResult, "No local Docker engine is usable: docker is missing, the engine does not answer or is not local, or the Go image cannot be pulled.")
		Tool("check_project", "Run the workspace's Go tests on the current uncommitted files in an isolated Linux container. No input. Returns passed and a message with the first failure; when it says the output was cut, read the rest with read_check_output(op_id). If the message says go.sum or go.work.sum needs entries or go.mod needs an update, call update_dependencies and check again.")
		HTTP(func() {
			POST("/workspace/check")
			Response(StatusOK)
			Response("unprocessable", StatusUnprocessableEntity)
			Response("not_implemented", StatusNotImplemented)
			Response("unavailable", StatusServiceUnavailable)
		})
	})

	Method("update_dependencies", func() {
		Description("Run go mod tidy for every Go module of the active workspace in a protected copy inside Linux containers, then write only the changed go.mod, go.sum and go.work.sum files back to the workspace through verified handles. A symlinked manifest or one edited during the call is refused for that module.")
		Result(UpdateDependenciesResult)
		Error("unprocessable", ErrorResult, "The workspace contains no Go module.")
		Error("not_implemented", ErrorResult, "No workspace directory is bound to this control plane.")
		Error("unavailable", ErrorResult, "No local Docker engine is usable: docker is missing, the engine does not answer or is not local, or the Go image cannot be pulled.")
		Tool("update_dependencies", "Add missing requirements and checksums for the workspace's Go modules (go mod tidy) and write the changed go.mod, go.sum and go.work.sum. No input. Call it when check_project reports a missing go.sum or go.work.sum entry or a needed go.mod update, then call check_project again.")
		HTTP(func() {
			POST("/workspace/dependencies")
			Response(StatusOK)
			Response("unprocessable", StatusUnprocessableEntity)
			Response("not_implemented", StatusNotImplemented)
			Response("unavailable", StatusServiceUnavailable)
		})
	})

	Method("read_check_output", func() {
		Description("Return one page of at most 16 KiB of the full output a check_project or update_dependencies operation captured, with a cursor for the next page and the count of bytes not captured.")
		Payload(ReadCheckOutputPayload)
		Result(CheckOutputPage)
		Error("not_found", ErrorResult, "No output exists for this op_id.")
		Error("unprocessable", ErrorResult, "The cursor is malformed or belongs to another operation.")
		Tool("read_check_output", "Read the next page of a check_project or update_dependencies output. Pass op_id from that result and next_cursor from the previous page; omit cursor for the first page. dropped_bytes > 0 means part of the output was not captured.")
		HTTP(func() {
			POST("/workspace/check/output")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
			Response("unprocessable", StatusUnprocessableEntity)
		})
	})
	traceMethods()
	storyRefreshMethods()
	teamMethods()
	changeListMethods()
	buildMethods()
})
