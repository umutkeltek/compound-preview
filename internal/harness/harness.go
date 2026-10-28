// Package harness is the HarnessRegistry — the spine's record of what each agent
// harness ACTUALLY supports and where it is weak. Governance must be capability-aware,
// not fantasy-uniform: a harness that cannot block a tool call natively must be enforced
// via a wrapper/adapter, never trusted to self-gate. This is the public-core, Go-native
// generalization of a control-spine's per-harness capability views.
// See docs/architecture/runtime.md §3.
package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const RegistryManifestSchemaVersion = "compound.harness_registry.v0.1"

// Posture is whether a human is at the keyboard while this harness runs. It is
// a registry fact, not a preference: it decides what a governed hold PROJECTS
// to at the native permission surface.
//
// An interactive harness can be asked. An unattended one cannot -- an "ask" on
// a lane running in a detached pane has no one to answer it, so the lane sits
// on the prompt until a human happens to look, and every downstream measure of
// that lane (heartbeat, transcript, run attempt) keeps reporting "running".
// That is strictly worse than a denial: a denial is a fact the agent can act on
// (take the alternative, record blocked-with-ticket against the minted approval
// request), while a silent freeze is an outage that looks like work.
//
// So the projection differs while the DECISION does not: the same hold, the
// same reason, the same approval-request id, delivered as `ask` where someone
// can answer and `deny` where no one can.
const (
	PostureInteractive = "interactive"
	PostureUnattended  = "unattended"
)

const OSEntrypointsAdapterID = "compound.os-harness-entrypoints"

// Capabilities is what a harness can verifiably do across the hook lifecycle.
//
// StopFeedback and SessionEndReceipt are deliberately distinct: some harnesses
// (e.g. codex) fire an end-of-turn Stop event that CAN inject/continue but have
// no SessionEnd event at all. Conflating them into one flag makes the registry
// lie for whichever event the flag doesn't actually describe — see
// docs_private/plan/workstreams/W2-hooks-context-injection.md "SMART HOOK DESIGN".
type Capabilities struct {
	SessionStartContext bool `json:"session_start_context"`
	UserPromptContext   bool `json:"user_prompt_context"`
	PreToolUseBlock     bool `json:"pre_tool_use_block"` // can it actually DENY a tool call?
	PostToolUseEvent    bool `json:"post_tool_use_event"`
	StopFeedback        bool `json:"stop_feedback"`       // can it inject/continue at end-of-turn (Stop)?
	SessionEndReceipt   bool `json:"session_end_receipt"` // does a SessionEnd event exist at all (telemetry-only)?
	NativeSkills        bool `json:"native_skills"`
	MCP                 bool `json:"mcp"`
}

// Harness is one registered executor.
type Harness struct {
	Name                 string       `json:"name"`
	InstructionMechanism string       `json:"instruction_mechanism"`
	Capabilities         Capabilities `json:"capabilities"`
	Limitations          []string     `json:"limitations"`
	InjectBudgetMS       int          `json:"inject_budget_ms,omitempty"`
	// HookBudgetMS is the whole-command wall-clock budget the compound hook
	// should honor for THIS harness: the internal fail-open deadline must sit
	// safely UNDER the harness's own hook kill timeout. Zero means "use the
	// compound default" (45s, under Claude Code's 60s); harnesses with
	// tighter outer timeouts (oh_my_pi's ~8s shim kill) declare theirs here.
	HookBudgetMS int  `json:"hook_budget_ms,omitempty"`
	Fallback     bool `json:"fallback,omitempty"` // true when this is the generic stand-in

	// Posture is PostureInteractive or PostureUnattended. Empty resolves through
	// DefaultPosture, which answers PostureUnattended for anything it does not
	// recognize: a harness nobody classified is far more likely to be a lane than
	// a person, and guessing "interactive" is the guess that freezes it.
	Posture string `json:"posture,omitempty"`

	// ExpectedVersion is the operator-blessed semver for the binary this
	// harness wraps. Empty means "unpinned" — doctor won't flag drift for
	// it. Non-empty values are the contract: doctor surfaces drift as the
	// "harness_version_drift" named signal when the installed version
	// (recovered by ObservedVersion) does not match.
	//
	// The pin names the binary that is INSTALLED and proven here, not
	// upstream's head. Pinning ahead of the installed binary makes the signal
	// fire on every probe, and a signal that is always on is one the operator
	// learns to skip — which is how a real drift would then go unread. Raising
	// a pin is an operator act that ships with the upgrade and a lane re-proof.
	//
	// A pin is only compared for harnesses in binaryForHarness (version.go).
	// It must also round-trip through that file's parseSemverToken, which
	// understands X.Y.Z and nothing else — see the deepseek_harness note there
	// for the pre-release case that is deliberately left unprobed.
	ExpectedVersion string `json:"expected_version,omitempty"`

	// Selection is what this harness's launch can do with a requested model,
	// effort, and session (selection_capability.go). Nil means no row: every
	// requested model or effort on it is refused before launch.
	Selection *SelectionCapability `json:"selection,omitempty"`
}

// Registry maps harness name -> capabilities.
type Registry struct{ Harnesses map[string]Harness }

type ManifestSourceRef struct {
	Path        string `json:"path" yaml:"path"`
	Kind        string `json:"kind" yaml:"kind"`
	Owner       string `json:"owner" yaml:"owner"`
	SourceClass string `json:"source_class" yaml:"source_class"`
	Revision    string `json:"revision" yaml:"revision"`
}

type RegistryManifest struct {
	SchemaVersion string            `json:"schema_version" yaml:"schema_version"`
	ID            string            `json:"id" yaml:"id"`
	AdapterID     string            `json:"adapter_id,omitempty" yaml:"adapter_id,omitempty"`
	SourceSystem  string            `json:"source_system,omitempty" yaml:"source_system,omitempty"`
	ObservedAt    string            `json:"observed_at,omitempty" yaml:"observed_at,omitempty"`
	SourceRef     ManifestSourceRef `json:"source_ref" yaml:"source_ref"`
	Harnesses     []ManifestHarness `json:"harnesses" yaml:"harnesses"`
	EvidenceRefs  []string          `json:"evidence_refs,omitempty" yaml:"evidence_refs,omitempty"`
}

type OSEntrypointsFile struct {
	Version      int                        `yaml:"version"`
	Generated    bool                       `yaml:"generated"`
	Status       string                     `yaml:"status"`
	LastReviewed string                     `yaml:"last_reviewed"`
	ReviewAfter  string                     `yaml:"review_after"`
	ExpiresAt    string                     `yaml:"expires_at"`
	MustInclude  string                     `yaml:"must_include"`
	Harnesses    map[string]OSEntrypointRow `yaml:"harnesses"`
}

type OSEntrypointRow struct {
	Entrypoint string `yaml:"entrypoint"`
	Status     string `yaml:"status"`
	Generator  string `yaml:"generator"`
	Check      string `yaml:"check"`
	ExpiresAt  string `yaml:"expires_at"`
}

type ManifestHarness struct {
	Name                 string               `json:"name" yaml:"name"`
	InstructionMechanism string               `json:"instruction_mechanism" yaml:"instruction_mechanism"`
	Capabilities         ManifestCapabilities `json:"capabilities" yaml:"capabilities"`
	Limitations          []string             `json:"limitations" yaml:"limitations"`
	InjectBudgetMS       int                  `json:"inject_budget_ms,omitempty" yaml:"inject_budget_ms,omitempty"`
	HookBudgetMS         int                  `json:"hook_budget_ms,omitempty" yaml:"hook_budget_ms,omitempty"`
	Fallback             bool                 `json:"fallback,omitempty" yaml:"fallback,omitempty"`
	ExpectedVersion      string               `json:"expected_version,omitempty" yaml:"expected_version,omitempty"`
	Posture              string               `json:"posture,omitempty" yaml:"posture,omitempty"`
}

type ManifestCapabilities struct {
	SessionStartContext bool `json:"session_start_context" yaml:"session_start_context"`
	UserPromptContext   bool `json:"user_prompt_context" yaml:"user_prompt_context"`
	PreToolUseBlock     bool `json:"pre_tool_use_block" yaml:"pre_tool_use_block"`
	PostToolUseEvent    bool `json:"post_tool_use_event" yaml:"post_tool_use_event"`
	StopFeedback        bool `json:"stop_feedback" yaml:"stop_feedback"`
	SessionEndReceipt   bool `json:"session_end_receipt" yaml:"session_end_receipt"`
	NativeSkills        bool `json:"native_skills" yaml:"native_skills"`
	MCP                 bool `json:"mcp" yaml:"mcp"`
}

// DefaultRegistry records what the common harnesses verifiably support. Conservative on
// purpose: PreToolUseBlock=false unless the harness is known to enforce it, because
// assuming a block that does not exist is a silent fail-open.
func DefaultRegistry() Registry {
	return Registry{Harnesses: map[string]Harness{
		"claude_code": {
			Name:                 "claude_code",
			Selection:            claudeLaneSelection(),
			Posture:              PostureInteractive,
			InstructionMechanism: "settings + hooks + CLAUDE.md",
			Capabilities: Capabilities{
				SessionStartContext: true, UserPromptContext: true, PreToolUseBlock: true,
				PostToolUseEvent: true, StopFeedback: true, SessionEndReceipt: true,
				NativeSkills: true, MCP: true,
			},
			Limitations: []string{"long-session instruction drift", "instruction/context budget cap"},
			// claude_code no longer binds UserPromptSubmit (see hookplan's
			// prepare_turn_context split), so this budget governs SessionStart alone --
			// one build per session, not one per turn. 1500ms was a per-turn figure and
			// the session build does not fit inside it: measured 1.1s idle and 2.3-4.3s
			// under load, which is why roughly a third of session starts fail open with
			// "Context build failed" and open unoriented. 5000ms clears the loaded
			// measurement with headroom; it is not a licence for the build to get slower.
			InjectBudgetMS: 5000,
		},
		"claude_zai": {
			Name:                 "claude_zai",
			Selection:            claudeLaneSelection(),
			Posture:              PostureUnattended,
			InstructionMechanism: "isolated Claude profile settings + repo CLAUDE.md + linked global CLAUDE.md",
			// Unlike the `_providers` lanes, this one is a FULL Claude profile with
			// hooks genuinely installed: its settings register SessionStart,
			// UserPromptSubmit, PreToolUse, PostToolUse, and the bounded SessionEnd
			// adapter. Stop remains unregistered and distinct from the receipt hook.
			//
			// `compound harness capability-drift` cannot yet verify this lane (it
			// publishes no launch profile to read), so these five hook flags rest on a
			// direct reading of the profile settings rather than on the check.
			Capabilities: Capabilities{
				SessionStartContext: true, UserPromptContext: true,
				PreToolUseBlock: true, PostToolUseEvent: true, SessionEndReceipt: true,
				NativeSkills: true, MCP: true,
			},
			Limitations: []string{
				"profile-local settings and history require separate hook installation",
				"global steering requires the managed profile link; repository CLAUDE.md discovery remains shared",
				"long-session instruction drift",
				"instruction/context budget cap",
			},
			InjectBudgetMS: 1500,
		},
		"claude_minimax": {
			Name:                 "claude_minimax",
			Selection:            claudeLaneSelection(),
			Posture:              PostureUnattended,
			InstructionMechanism: "isolated Claude profile settings + repo CLAUDE.md + linked global CLAUDE.md",
			// A lane declaration is a conservative FLOOR, not an aspiration. Every hook flag here
			// used to be true while the lane's settings registered no hooks at all -- the
			// "separate hook installation" limitation below described a step that had never been
			// performed, so the registry described the intended end state and routing believed
			// it. Work went to an executor that could not report back, and the failure looked
			// like silence.
			//
			// This is the raise: `compound hook install --harness claude-minimax` (see
			// internal/hookplan/install.go) now genuinely writes SessionStart, UserPromptSubmit,
			// PreToolUse (blocking, --enforce), and PostToolUse into the isolated minimax profile
			// settings.json -- mirroring claude_zai below. The floor now describes an installed
			// binding, not a plan. SessionEnd uses the bounded adapter; Stop remains
			// unregistered, same as claude_zai.
			//
			// Skills and MCP were off here while the launcher disabled them. It no longer
			// does: the wrapper launches `sb run claude --with minimax --workflows
			// --mcp-all --skills`, and the profile carries both bindings on disk (a skills
			// symlink and a generated MCP config). The floor stayed false after the
			// launcher changed, so `capability-drift` reported an underclaim and routing
			// withheld skill- and MCP-bearing work from a lane that could run it.
			// Verify any change here with `compound harness capability-drift`: an over-claim
			// fails; an under-claim against a genuinely installed binding is the signal that a
			// future install-path change regressed and this floor needs lowering again.
			Capabilities: Capabilities{
				SessionStartContext: true, UserPromptContext: true,
				PreToolUseBlock: true, PostToolUseEvent: true, SessionEndReceipt: true,
				NativeSkills: true, MCP: true,
			},
			Limitations: []string{
				"profile-local settings and history require separate hook installation",
				"global steering requires the managed profile link; repository CLAUDE.md discovery remains shared",
				"long-session instruction drift",
				"instruction/context budget cap",
				"token-plan provider lane (prepaid); context price cliff near 512k tokens",
			},
			InjectBudgetMS: 1500,
		},
		"claude_qwen": {
			Name:                 "claude_qwen",
			Selection:            claudeLaneSelection(),
			Posture:              PostureUnattended,
			InstructionMechanism: "isolated Claude profile settings + repo CLAUDE.md + linked global CLAUDE.md",
			// This is the raise, mirroring claude_minimax: `compound hook install
			// --harness claude-qwen` (see internal/hookplan/install.go) now genuinely
			// writes SessionStart, UserPromptSubmit, PreToolUse (blocking, --enforce),
			// and PostToolUse into the isolated qwen profile settings.json. The floor
			// describes an installed binding, not a plan; before enrollment it declared
			// zero hooks and qwen sessions could not report back, so their work never
			// compounded. SessionEnd uses the bounded adapter; Stop remains
			// unregistered, same as claude_zai and claude_minimax.
			//
			// Skills and MCP stay true -- unlike minimax, this lane's launcher does
			// not disable them. Verify any change here with `compound harness
			// capability-drift`: an over-claim fails; an under-claim against a
			// genuinely installed binding signals a regressed install path.
			Capabilities: Capabilities{
				SessionStartContext: true, UserPromptContext: true,
				PreToolUseBlock: true, PostToolUseEvent: true, SessionEndReceipt: true,
				NativeSkills: true, MCP: true,
			},
			Limitations: []string{
				"profile-local settings and history require separate hook installation",
				"global steering requires the managed profile link; repository CLAUDE.md discovery remains shared",
				"long-session instruction drift",
				"instruction/context budget cap",
				"token-plan provider lane (prepaid); night-discount window 22:00-08:00 UTC+8 for execution burns",
			},
			InjectBudgetMS: 1500,
		},
		"claude_zai_full": {
			Name:                 "claude_zai_full",
			Selection:            claudeLaneSelection(),
			Posture:              PostureUnattended,
			InstructionMechanism: "isolated Claude profile settings + repo CLAUDE.md + linked global CLAUDE.md",
			// Hook flags once sat at zero because this lane's settings registered no
			// hooks. They do now -- `compound harness capability-drift` observes all five
			// (SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, SessionEnd) in the
			// profile settings -- so the floor rises to the installed binding. Skills and
			// MCP stay true; its launcher keeps them.
			Capabilities: Capabilities{
				SessionStartContext: true, UserPromptContext: true,
				PreToolUseBlock: true, PostToolUseEvent: true, SessionEndReceipt: true,
				NativeSkills: true, MCP: true,
			},
			Limitations: []string{
				"profile-local settings and history require separate hook installation",
				"global steering requires the managed profile link; repository CLAUDE.md discovery remains shared",
				"long-session instruction drift",
				"instruction/context budget cap",
				"subscription window lane; busy-hour token weighting applies",
			},
			InjectBudgetMS: 1500,
		},
		"claude_neuralwatt": {
			Name:                 "claude_neuralwatt",
			Selection:            claudeLaneSelection(),
			Posture:              PostureUnattended,
			InstructionMechanism: "isolated Claude profile settings + repo CLAUDE.md + linked global CLAUDE.md",
			// Registry identity only. This lane is a Claude-family launch profile that
			// was previously absent from the registry; its settings have no
			// installed hook bindings yet, so the conservative floor stays at the
			// un-installed shape (every hook flag false) and the lane will fail a
			// readiness check that asks for PreToolUse block or SessionEnd receipt.
			// That is the next slice: the isolated lane installer must install and
			// attest hooks before raising the capability floor. (The earlier
			// `compound hook install --harness claude-neuralwatt` form is invalid
			// here — hook install accepts Claude-family harness types, not raw
			// lane IDs.) The credential is the GATEWAY token, not the provider
			// key, mirroring the switchback launch-profiles.json note.
			Capabilities: Capabilities{
				NativeSkills: true, MCP: true,
			},
			Limitations: []string{
				"profile-local settings and history require separate hook installation",
				"global steering requires the managed profile link; repository CLAUDE.md discovery remains shared",
				"long-session instruction drift",
				"instruction/context budget cap",
				"gateway-credential lane (not provider key) — the tap forwards to the gateway, which injects the upstream provider key",
				"registry identity only — hook installation and capability-floor raise land in a follow-up slice",
			},
			InjectBudgetMS: 1500,
		},
		"claude_gpt56_sol_wpcom": {
			Name:                 "claude_gpt56_sol_wpcom",
			Selection:            claudeLaneSelection(),
			Posture:              PostureUnattended,
			InstructionMechanism: "isolated Claude profile settings + repo CLAUDE.md + linked global CLAUDE.md",
			// Registry identity only. Same floor as claude_neuralwatt: no installed hook
			// bindings yet, so all hook flags stay false. The next slice installs hooks
			// and raises this floor the same way claude_minimax was raised.
			Capabilities: Capabilities{
				NativeSkills: true, MCP: true,
			},
			Limitations: []string{
				"profile-local settings and history require separate hook installation",
				"global steering requires the managed profile link; repository CLAUDE.md discovery remains shared",
				"long-session instruction drift",
				"instruction/context budget cap",
				"wpcom free-proxy lane — Opus/Sonnet/Haiku all route through the same target; the wpcom proxy does not differentiate them",
				"registry identity only — hook installation and capability-floor raise land in a follow-up slice",
			},
			InjectBudgetMS: 1500,
		},
		"claude_gpt56_sol_wpcom_headroom": {
			Name:                 "claude_gpt56_sol_wpcom_headroom",
			Selection:            claudeLaneSelection(),
			Posture:              PostureUnattended,
			InstructionMechanism: "isolated Claude profile settings + repo CLAUDE.md + linked global CLAUDE.md",
			// Registry identity only. Same floor as claude_gpt56_sol_wpcom; the
			// headroom transport (compression on, hosted tool-search off) is a
			// launch-profile detail, not a registry-floor differentiator.
			Capabilities: Capabilities{
				NativeSkills: true, MCP: true,
			},
			Limitations: []string{
				"profile-local settings and history require separate hook installation",
				"global steering requires the managed profile link; repository CLAUDE.md discovery remains shared",
				"long-session instruction drift",
				"instruction/context budget cap",
				"wpcom free-proxy lane via Headroom transport — compression enabled, hosted tool-search injection disabled for Switchback target compatibility",
				"registry identity only — hook installation and capability-floor raise land in a follow-up slice",
			},
			InjectBudgetMS: 1500,
		},
		"claude_gpt": {
			Name:                 "claude_gpt",
			Selection:            claudeLaneSelection(),
			Posture:              PostureUnattended,
			InstructionMechanism: "isolated Claude profile settings + repo CLAUDE.md + linked global CLAUDE.md",
			// Registry identity only. Same floor as the other new lanes; this one routes
			// through the strict native Codex OAuth lane (no wpcom, no provider fallback)
			// which is captured here as a limitation, not as a hook-flag override.
			Capabilities: Capabilities{
				NativeSkills: true, MCP: true,
			},
			Limitations: []string{
				"profile-local settings and history require separate hook installation",
				"global steering requires the managed profile link; repository CLAUDE.md discovery remains shared",
				"long-session instruction drift",
				"instruction/context budget cap",
				"strict native Codex OAuth lane — sole target codex-relay; no wpcom proxy, no provider fallback",
				"registry identity only — hook installation and capability-floor raise land in a follow-up slice",
			},
			InjectBudgetMS: 1500,
		},
		"claude_kimi": {
			Name:                 "claude_kimi",
			Selection:            claudeLaneSelection(),
			Posture:              PostureUnattended,
			InstructionMechanism: "isolated Claude profile settings + repo CLAUDE.md + linked global CLAUDE.md",
			// Registry identity only. Kimi K3 coding plan via rotating OAuth is a
			// subscription, not API billing; that is captured in the limitations string
			// without lifting any hook flag.
			Capabilities: Capabilities{
				NativeSkills: true, MCP: true,
			},
			Limitations: []string{
				"profile-local settings and history require separate hook installation",
				"global steering requires the managed profile link; repository CLAUDE.md discovery remains shared",
				"long-session instruction drift",
				"instruction/context budget cap",
				"subscription lane (Kimi K3 coding plan) via rotating OAuth, not API billing",
				"registry identity only — hook installation and capability-floor raise land in a follow-up slice",
			},
			InjectBudgetMS: 1500,
		},
		"claude_deepseek_flash_api": {
			Name:                 "claude_deepseek_flash_api",
			Selection:            claudeLaneSelection(),
			Posture:              PostureUnattended,
			InstructionMechanism: "isolated Claude profile settings + repo CLAUDE.md + linked global CLAUDE.md",
			// Registry identity only. Direct DEEPSEEK_API_KEY upstream, falls back to the
			// NVIDIA Build variant when the direct upstream rejects. The fallback is a
			// launch-profile detail, not a registry-floor differentiator.
			Capabilities: Capabilities{
				NativeSkills: true, MCP: true,
			},
			Limitations: []string{
				"profile-local settings and history require separate hook installation",
				"global steering requires the managed profile link; repository CLAUDE.md discovery remains shared",
				"long-session instruction drift",
				"instruction/context budget cap",
				"direct-API lane (api.deepseek.com) — falls back to the NVIDIA Build variant if the direct upstream rejects",
				"registry identity only — hook installation and capability-floor raise land in a follow-up slice",
			},
			InjectBudgetMS: 1500,
		},
		"claude_deepseek_flash_nvidia": {
			Name:                 "claude_deepseek_flash_nvidia",
			Selection:            claudeLaneSelection(),
			Posture:              PostureUnattended,
			InstructionMechanism: "isolated Claude profile settings + repo CLAUDE.md + linked global CLAUDE.md",
			// Registry identity only. NVIDIA Build free tier, rate-limited; multiple
			// targets keep the lane alive when a single target rejects. Same hook-floor
			// story as the other new lanes.
			Capabilities: Capabilities{
				NativeSkills: true, MCP: true,
			},
			Limitations: []string{
				"profile-local settings and history require separate hook installation",
				"global steering requires the managed profile link; repository CLAUDE.md discovery remains shared",
				"long-session instruction drift",
				"instruction/context budget cap",
				"NVIDIA Build free-tier lane (integrate.api.nvidia.com) — rate-limited; multiple targets ensure the lane survives single-target rejections",
				"registry identity only — hook installation and capability-floor raise land in a follow-up slice",
			},
			InjectBudgetMS: 1500,
		},
		"codex": {
			Name:                 "codex",
			Selection:            codexSelection(),
			Posture:              PostureInteractive,
			InstructionMechanism: "AGENTS.md + hooks + skills (progressive disclosure)",
			Capabilities: Capabilities{
				SessionStartContext: true, UserPromptContext: true, PreToolUseBlock: true,
				PostToolUseEvent: true, StopFeedback: true, SessionEndReceipt: true,
				NativeSkills: true, MCP: true,
			},
			Limitations: []string{
				"native PreToolUse requires the stable hooks feature and an installed trusted hook",
				// Codex 0.149.x fires a native SessionEnd event with transcript_path (binary
				// evidence: `strings /opt/homebrew/Caskroom/codex/0.149.1/bin/codex | grep`
				// surfaces session_end in the hook event vocabulary and session_id,
				// transcript_path, hook_event_name, stop_hook_active in the wire payload). The
				// Stop binding stays -- Stop is its own end-of-turn channel (JSON block /
				// continuation) and the SessionEnd receipt path adds rather than replaces it.
				// Receipt is routed through the generic `compound hook session-end --live`
				// verb (NOT session-end-adapter, which is the Claude-family bounded adapter),
				// so Stop and SessionEnd share hook.go's sessionEnd() handler but exercise
				// different code paths inside it.
				"SessionEnd receipt shares sessionEnd() with Stop; Stop's stop_hook_active short-circuit still owns end-of-turn continuation",
				"initial skill list capped (progressive disclosure)",
				"cwd-dependent guidance",
			},
			InjectBudgetMS: 1500,
		},
		"hermes": {
			Name:                 "hermes",
			Selection:            noSelectionControl("registry identity only; no executor adapter"),
			Posture:              PostureUnattended,
			InstructionMechanism: "AGENTS.md + plugin lifecycle hooks + skills + MCP",
			Capabilities: Capabilities{
				SessionStartContext: true, UserPromptContext: true, PreToolUseBlock: true,
				PostToolUseEvent: true, StopFeedback: false, SessionEndReceipt: true,
				NativeSkills: true, MCP: true,
			},
			Limitations: []string{
				"dynamic context uses pre_llm_call; on_session_start fires after the system prompt is built",
				"plugin callback exceptions fail open",
				"native blocking requires an installed and content-attested Compound plugin",
				"on_session_end is turn/run scoped, not final chat lifetime",
				"no-agent cron and script execution requires a wrapper or action contract",
			},
			InjectBudgetMS: 1000,
		},
		"opencode": {
			Name:                 "opencode",
			Selection:            openCodeSelection(),
			Posture:              PostureUnattended,
			InstructionMechanism: "config instructions + plugins + native skills",
			Capabilities: Capabilities{
				SessionStartContext: true, UserPromptContext: true, PreToolUseBlock: false,
				PostToolUseEvent: true, StopFeedback: false, SessionEndReceipt: false,
				NativeSkills: true, MCP: true,
			},
			Limitations: []string{
				"native hook/native-skills/MCP facts describe the interactive harness; the registered executor is prompt-only and disables those surfaces",
				"prompt-only execution accepts supplied-text classification or critique; it cannot build, edit a workspace, invoke tools, or reuse sessions",
				"Compound context injection uses stable chat.message message.system; the system context transform remains experimental and unused",
				"tool.execute.before cannot deny; the Compound integration does not register it",
				"injection and observation callbacks fail open",
				"no proven SessionEnd or Stop mapping",
				"installed state and content revision come from installed-hooks authority, not the static harness registry",
				"not a mutation-control lane in imported capability authority",
			},
			InjectBudgetMS: 1000,
		},
		"prime_agent": {
			Name:                 "prime_agent",
			Selection:            noSelectionControl("pi adapter never places RunInput model or effort on argv; only caller Args can"),
			Posture:              PostureUnattended,
			InstructionMechanism: "AGENTS.md + system-prompt arg + --no-session headless invocation + structured-IPC mode",
			// Prime-agent has no SessionStart/UserPromptSubmit/PreToolUse/PostToolUse hook
			// protocol -- its native hook surface is tool-policy allowlists (`-t/--tools`
			// and `-nbt/--no-builtin-tools`). The registry floor is therefore deliberately
			// empty on the hook flags: Compound injects context via `--system-prompt` /
			// `--append-system-prompt` argv at dispatch time and observes outcomes through
			// the shared runattempt.Store, not through Prime-agent hooks.
			//
			// NativeSkills and MCP are FALSE, and that is a correction rather than a
			// downgrade of the product. Prime-agent supports both: skills load from
			// <agentDir>/skills (user) and <cwd>/.prime/agent/skills (project), and MCP
			// servers are declared under the `mcpServers` key of the agent home's
			// settings.json (dist/core/settings-manager.d.ts declares
			// `mcpServers?: Record<string, McpServerConfig>`). Neither is INSTALLED: the
			// user skills directory does not exist and settings.json carries no
			// mcpServers key. The claude_minimax comment above sets the precedent that
			// these flags describe installed bindings on disk, so declaring them true
			// routed skill-bearing work at a lane where no skill body would load.
			//
			// The claim this replaces said MCP arrived "via the daemon's `--mcp` server
			// config, exactly like Codex's pattern." There is no `--mcp` flag in
			// `prime-agent --help` at 0.7.1. That was a wrong-but-plausible detail on its
			// way to becoming a load-bearing premise for the next reader. The raise path
			// is unchanged: install the binding (create the skills directory, write the
			// mcpServers key), observe it loading, then flip the flag in the same change.
			//
			// The "registry identity only" limitation is the same admission Codex used
			// before its switchback landing: the harness row exists so the runner and
			// work-delegate resolver have a stable id to dispatch into; the
			// capability-floor raise (and the persistent-kernel governor) land in a
			// follow-up slice.
			Capabilities: Capabilities{},
			Limitations: []string{
				"no SessionStart/UserPromptSubmit/PreToolUse/PostToolUse hook protocol — context is injected via `--system-prompt` / `--append-system-prompt` argv at dispatch",
				"no SessionEnd or Stop feedback — terminal status is derived from the subprocess exit code under the shared runWithSubprocess core",
				"prime-agent daemon listens on a Unix socket under $TMPDIR/prime-agent-<uid>/daemon.sock (NOT a localhost TCP port); port 18774 is Switchback's capture tap that prime's models.json points its LLM calls at — outbound model target, never prime IPC",
				"headless invocation is `--mode rpc` or `-p --no-session`; the run-attempt chrome is the only audit trail",
				"tool policy is enforced via `-t/--tools` allowlist at dispatch — see the pi-family adapter buildCmd in pi_coding_agent_adapter.go",
				"NativeSkills/MCP are SUPPORTED-not-installed: skills load from <agentDir>/skills (user) and <cwd>/.prime/agent/skills (project), but the user skills directory does not exist and settings.json carries no mcpServers key — the floor stays false until a binding is on disk",
				"MCP is configured through the `mcpServers` key of the agent home settings.json — there is no `--mcp` flag on the CLI",
				"registry identity only — capability-floor raise and persistent-kernel governor land in a follow-up slice",
			},
			InjectBudgetMS: 1500,
			// The pin tracks the INSTALLED and proven binary, not upstream's head.
			// 0.7.4 was upstream latest at 2026-08-19; upstream is 0.8.0 as of
			// 2026-08-22 (GitHub releases API, tag v0.8.0), so the old pin matched
			// neither what runs here nor what upstream ships — it just made
			// harness_version_drift fire forever, and a signal that is always on is a
			// signal the operator learns to ignore. Raising this is an operator act
			// that ships WITH the upgrade and a lane re-proof: a lane that has not
			// restarted since its upgrade is unproven. PI_SKIP_VERSION_CHECK=1 on
			// dispatch (see the pi-family adapter buildCmd) stops the binary
			// self-updating mid-run and invalidating the RunAttempt chrome.
			ExpectedVersion: "0.8.0",
		},
		"oh_my_pi": {
			Name:      "oh_my_pi",
			Selection: noSelectionControl("pi adapter never places RunInput model or effort on argv; only caller Args can"),
			// The OMP hook shim wraps compound with its own ~8s hard kill. The
			// internal fail-open watchdog MUST answer before that (5-6s, with a
			// margin to persist the timeout evidence), or the outer kill wins
			// and the hook dies with no output at all — the failure shape the
			// budget exists to prevent.
			HookBudgetMS:         6000,
			InstructionMechanism: "AGENTS.md/CLAUDE.md context-file discovery + --system-prompt/--append-system-prompt argv + -p/--mode headless invocation",
			// Same codebase family as prime_agent (both fork the pi coding agent), so
			// the standing pi-family boundaries are inherited verbatim: rented executor
			// only, private memory stays dark, disposable workspaces, provider traffic
			// only through a body-captured tap.
			//
			// Conservative FLOOR: this row describes what is INSTALLED, not what the
			// product can do. Its hook surface is the richest in the family — a typed
			// `agent.v1` RPC whose PreToolUseRequestResponse carries a `permission`
			// field (a real veto), alongside PostToolUse, PostToolUseFailure,
			// BeforeSubmitPrompt, Stop, SubagentStart/Stop and PreCompact. Every hook
			// flag is still false, because discovery is by FILE: the loader walks
			// <configDir>/hooks/pre and <configDir>/hooks/post and takes each entry's
			// base name as the tool matcher (shipped src/discovery/builtin.ts,
			// loadHooks). Compound's hookplan installer writes shell-command entries
			// into a settings.json instead, so nothing it installs is discoverable
			// here. Raising a flag before a shim exists AND an event has been observed
			// would repeat the claude_minimax failure: routing believes the intended
			// end state, work goes to an executor that cannot report back, and the
			// failure looks like silence.
			//
			// SessionStart and SessionEnd are not in the catalog at all, so those two
			// can never rise from a hook — terminal status comes from the subprocess
			// exit under the shared runWithSubprocess core, same as prime_agent.
			//
			// NativeSkills is true and it is an installed-binding claim, not a
			// capability claim: oh-my-pi registers a `.claude` discovery provider
			// ("Load skills from .claude/skills/*/SKILL.md") and that directory is
			// populated on this host, so the corpus loads with zero wiring. Scope it
			// with `--skills <glob>` — an unscoped corpus is a context tax paid on
			// every session, not a feature.
			//
			// MCP stays false: the loader reads <agentDir>/mcp.json,
			// <cwd>/.omp/.mcp.json, <cwd>/.mcp.json and .claude-plugin/plugin.json,
			// and none of them exist. Supported is not installed. Flip it in the same
			Capabilities: Capabilities{
				UserPromptContext: true,
				PreToolUseBlock:   true,
				PostToolUseEvent:  true,
				SessionEndReceipt: true,
				NativeSkills:      true,
			},
			Limitations: []string{
				"PreToolUseBlock is true WHEN the compound TS shim is installed (default install writes ~/.omp/agent/hooks/pre/compound.ts); the shim returns {block:true, reason} to omp's tool_call event and the runner stamps permissionDecision=deny on the live pre-tool-use answer. Without the shim this reverts to fail_open — the install is a precondition, never an assumption",
				"MCP discovery paths exist (<agentDir>/mcp.json, <cwd>/.omp/.mcp.json, <cwd>/.mcp.json, .claude-plugin/plugin.json) but no server config is installed for this harness",
				"skill discovery resolves its .claude root through CLAUDE_CONFIG_DIR (src/config/claude-paths.ts), so a lane launched under per-profile isolation sees that profile's skills and not the shared corpus — NativeSkills=true is host-scoped, not lane-scoped",
				"keeps a private memory store under <agentDir> (memories/, history.db) — same class as prime_agent's; it stays dark and Compound remains the meaning authority",
				"approval-mode defaults to a host config that can be yolo; governed dispatch in oh_my_pi MUST pass `--approval-mode yolo|write|always-ask` explicitly via PiCodingAgentAdapter.buildCmd (oh_my_piPostureArgs) and never inherit the config default; a `bypass` (yolo) launch without a loadable shim path is REFUSED before process start",
				"runs on Bun (engines.bun >=1.3.14) and reads Bun.env; its proxy support accepts only http:// and https:// proxy URLs — SOCKS (socks5://, socks5h://) is rejected outright (src/utils/fetch-timeout.ts)",
				"no version-check suppression env exists (unlike prime_agent's PI_SKIP_VERSION_CHECK); `omp update` is explicit but marketplace.autoUpdate can still fetch mid-session, so the ExpectedVersion pin is load-bearing at a 17.x release cadence",
				"SessionStartContext stays false: omp's session_start event has no context-return surface; ContextPack injection enters at the prompt boundary (before_agent_start), reported as UserPromptContext",
			},
			InjectBudgetMS:  1500,
			ExpectedVersion: "18.1.17",
		},
		"qwen_code": {
			Name:                 "qwen_code",
			Selection:            noSelectionControl("registry identity only; no executor adapter"),
			InstructionMechanism: "QWEN.md/AGENTS.md context files + .qwen/ project dirs + -p headless with -o text|json|stream-json",
			// Conservative FLOOR. qwen-code is the one harness here whose hook surface
			// has the same SHAPE as claude_code's — SessionStart, UserPromptSubmit,
			// PreToolUse, PostToolUse, SessionEnd, Stop and Notification all appear as
			// first-party event names, managed by `qwen hooks` and a .qwen/hooks/
			// project dir. Every hook flag stays false anyway, for the reason at the
			// top of DefaultRegistry: an event NAME is not an enforcement point, and
			// assuming a block that does not exist is a silent fail-open.
			// `qwen hooks --help` prints NOTHING at 0.21.15, so PreToolUse's veto power
			// is unproven. Raise these only after an installer genuinely writes the
			// bindings AND a denial is observed end to end.
			//
			// MCP is true and it is the one genuinely-installed capability in this
			// batch: the user settings file carries an mcpServers map with a live
			// entry, managed by `qwen mcp add|list|approve`.
			//
			// NativeSkills stays false: the loader is real
			// (.qwen/skills/<name>/SKILL.md, surfaced as /<name> commands) but the
			// skills directory is EMPTY. A loader with no bodies is exactly what the
			// floor rule exists to catch — it routes skill-bearing work into a hole.
			//
			// No adapter is registered. Gemini-CLI lineage, not the pi family: it
			// shares no argv, settings schema or session layout with prime_agent /
			// oh_my_pi and cannot reuse their adapter, and it has no `--cwd`, so a
			// governed dispatch can only set the workspace through the child process
			// working directory. The headless form IS verified
			// (`qwen -p "<prompt>" -o json --approval-mode <mode>`), so an adapter is
			// tractable — it is deferred, not blocked.
			Capabilities: Capabilities{
				MCP: true,
			},
			Limitations: []string{
				"hook events (SessionStart/UserPromptSubmit/PreToolUse/PostToolUse/SessionEnd/Stop/Notification) exist by name, but no binding is installed and PreToolUse veto power is UNVERIFIED — `qwen hooks --help` prints nothing at 0.21.15",
				"skills loader is present (.qwen/skills/<name>/SKILL.md, surfaced as /<name> commands) but the skills directory is empty — no bodies installed",
				"Gemini-CLI lineage, not the pi family — settings schema, session layout and ACP surface share nothing with prime_agent/oh_my_pi and cannot reuse their adapter",
				"no `--cwd` flag: the workspace can only be set through the child process working directory, so a governed dispatch cannot stamp it into argv the way the pi family does",
				"`--approval-mode plan|default|auto-edit|auto|yolo` and `-y/--yolo` ARE accepted on the top-level CLI but are omitted from `--help`; a governed lane must pass the posture explicitly rather than discover it from help output",
				"setting tools.approvalMode to yolo is refused through the runtime /config path and can only be written to settings.json — the CLI flag is the governed way to state a posture per dispatch",
				"--insecure / QWEN_TLS_INSECURE=1 disables TLS verification for EVERY upstream, not just a local tap — it must never be used to shortcut CA trust in a lane",
				"`qwen serve` exposes a local HTTP daemon (default port 4170) whose loopback bind is auth-free; --require-auth plus --token/QWEN_SERVER_TOKEN must be set before any shared-host use",
				"registry identity only — no adapter is registered for this harness; hook installation and the capability-floor raise land in a follow-up slice",
			},
			InjectBudgetMS:  1500,
			ExpectedVersion: "0.21.15",
		},
		"deepseek_harness": {
			Name:                 "deepseek_harness",
			Selection:            noSelectionControl("registry identity only; no executor adapter"),
			InstructionMechanism: "cordis plugin profile layers under $DSH_HOME/profiles/<name> + --patch overlays + `dsh --profile headless <task>` one-shot",
			// ADOPT-LATER row. The identity is recorded so the decision has a written
			// owner instead of being re-derived, and so a later lane has a stable id to
			// bind. There is no adapter and there must not be one yet: the entire
			// headless surface is `dsh --profile headless "<task>"`, whose own --help
			// lists exactly one option (-h). No --model, no --output-format, no session
			// handle, no structured result — nothing a governed adapter can hold onto.
			// At 0.1.0-rc.7 the plugin API will move under anything written against it.
			//
			// Every capability flag is false and each is a VERIFIED ABSENCE rather than
			// an unknown: no hook plugin of any kind exists in the shipped plugin tree,
			// the skills loader's roots are plugin config and none are set, and MCP
			// servers are declared inside the profile's cordis config rather than a
			// standalone file.
			//
			// The durable finding is capture: the DeepSeek LLM adapter calls bare fetch
			// with no global undici dispatcher, so an HTTPS_PROXY-style tap will not
			// intercept its model traffic. The lane mechanism is endpoint redirection
			// through DEEPSEEK_BASE_URL (declared as BASE_URL_ENV in the shipped
			// llm-deepseek plugin) — a difference in kind, not degree, and the reason
			// this row exists before the harness is adoptable.
			Capabilities: Capabilities{},
			Limitations: []string{
				"headless is one string in and one string out: `dsh --profile headless \"<task>\"` — its own --help lists only -h; no --model, no --output-format, no session id, no structured result",
				"no hook protocol of any kind — the only extension seam is publishing a cordis plugin package, which is a fork-scale commitment rather than an integration",
				"MCP servers are declared inside the profile's cordis config in the patch layer, not a standalone mcp.json",
				"skills loader exists but its discovery roots are plugin config and none are configured",
				"LLM calls use bare fetch with no global undici dispatcher, so proxy-based capture will NOT intercept them — a lane must redirect the endpoint with DEEPSEEK_BASE_URL instead",
				"DSH_PERMISSION_MODE (read-only|workspace-write|danger-full-access) is the only posture control, and danger-full-access also auto-disables the approval plugin",
				"profiles are real pnpm workspaces under $DSH_HOME/profiles/<name> and `dsh plugin` shells out to pnpm — a profile is a mutable dependency tree, not a config file",
				"`dsh --profile <name> --help` MATERIALISES the profile workspace on disk as a side effect — there is no read-only probe, so a readiness check cannot be run without mutating $DSH_HOME",
				"0.1.0-rc.7 — pre-1.0 plugin API; expect breaking change between any two versions, and the pre-release suffix is why this harness is deliberately absent from binaryForHarness (see version.go)",
				"registry identity only, adopt-later — no adapter is registered; re-evaluate at >=1.0 or when `--profile headless` gains --model and --output-format",
			},
			InjectBudgetMS:  1500,
			ExpectedVersion: "0.1.0-rc.7",
		},
	}}
}

func LoadRegistryManifest(path string) (RegistryManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RegistryManifest{}, fmt.Errorf("read harness registry %s: %w", path, err)
	}
	var manifest RegistryManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return RegistryManifest{}, fmt.Errorf("parse harness registry %s: %w", path, err)
	}
	if strings.TrimSpace(manifest.SchemaVersion) == "" {
		manifest.SchemaVersion = RegistryManifestSchemaVersion
	}
	if strings.TrimSpace(manifest.ID) == "" {
		manifest.ID = "harness-registry"
	}
	if strings.TrimSpace(manifest.AdapterID) == "" {
		manifest.AdapterID = "compound.harness-registry"
	}
	if strings.TrimSpace(manifest.SourceSystem) == "" {
		manifest.SourceSystem = "compound"
	}
	if strings.TrimSpace(manifest.SourceRef.Path) == "" {
		manifest.SourceRef.Path = path
	}
	if strings.TrimSpace(manifest.SourceRef.Kind) == "" {
		manifest.SourceRef.Kind = "registry"
	}
	if strings.TrimSpace(manifest.SourceRef.Owner) == "" {
		manifest.SourceRef.Owner = manifest.ID
	}
	if strings.TrimSpace(manifest.SourceRef.SourceClass) == "" {
		manifest.SourceRef.SourceClass = "generated"
	}
	if strings.TrimSpace(manifest.SourceRef.Revision) == "" {
		sum := sha256.Sum256(data)
		manifest.SourceRef.Revision = "sha256:" + hex.EncodeToString(sum[:])
	}
	if len(manifest.EvidenceRefs) == 0 {
		manifest.EvidenceRefs = []string{"harness_registry:" + manifest.SourceRef.Revision}
	}
	if _, err := manifest.Registry(); err != nil {
		return RegistryManifest{}, err
	}
	return manifest, nil
}

func LoadOSEntrypointsManifest(path string) (RegistryManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RegistryManifest{}, fmt.Errorf("read OS harness entrypoints %s: %w", path, err)
	}
	var source OSEntrypointsFile
	if err := yaml.Unmarshal(data, &source); err != nil {
		return RegistryManifest{}, fmt.Errorf("parse OS harness entrypoints %s: %w", path, err)
	}
	if len(source.Harnesses) == 0 {
		return RegistryManifest{}, fmt.Errorf("OS harness entrypoints needs at least one harness")
	}
	sum := sha256.Sum256(data)
	revision := "sha256:" + hex.EncodeToString(sum[:])
	manifest := RegistryManifest{
		SchemaVersion: RegistryManifestSchemaVersion,
		ID:            "os-harness-entrypoints",
		AdapterID:     OSEntrypointsAdapterID,
		SourceSystem:  "external-os",
		SourceRef: ManifestSourceRef{
			Path:        path,
			Kind:        "registry",
			Owner:       "harness-entrypoints",
			SourceClass: "os-harness-entrypoints",
			Revision:    revision,
		},
		EvidenceRefs: []string{"os_harness_entrypoints:" + revision},
	}
	names := make([]string, 0, len(source.Harnesses))
	for name := range source.Harnesses {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, sourceID := range names {
		row := source.Harnesses[sourceID]
		entrypoint := strings.TrimSpace(row.Entrypoint)
		if entrypoint == "" {
			return RegistryManifest{}, fmt.Errorf("OS harness entrypoint %q needs entrypoint", sourceID)
		}
		manifest.Harnesses = append(manifest.Harnesses, ManifestHarness{
			Name:                 canonicalHarnessName(sourceID),
			InstructionMechanism: osEntrypointInstruction(row),
			Capabilities: ManifestCapabilities{
				SessionStartContext: true,
			},
			Limitations: osEntrypointLimitations(sourceID, source, row),
		})
	}
	if _, err := manifest.Registry(); err != nil {
		return RegistryManifest{}, err
	}
	return manifest, nil
}

func (m RegistryManifest) Registry() (Registry, error) {
	if strings.TrimSpace(m.SchemaVersion) != RegistryManifestSchemaVersion {
		return Registry{}, fmt.Errorf("harness registry schema_version = %q, want %q", m.SchemaVersion, RegistryManifestSchemaVersion)
	}
	if len(m.Harnesses) == 0 {
		return Registry{}, fmt.Errorf("harness registry needs at least one harness")
	}
	byName := map[string]Harness{}
	for _, in := range m.Harnesses {
		name := strings.TrimSpace(in.Name)
		if name == "" {
			return Registry{}, fmt.Errorf("harness registry contains harness without name")
		}
		if _, exists := byName[name]; exists {
			return Registry{}, fmt.Errorf("harness registry duplicate harness %q", name)
		}
		if strings.TrimSpace(in.InstructionMechanism) == "" {
			return Registry{}, fmt.Errorf("harness %q needs instruction_mechanism", name)
		}
		byName[name] = Harness{
			Name:                 name,
			InstructionMechanism: strings.TrimSpace(in.InstructionMechanism),
			Capabilities: Capabilities{
				SessionStartContext: in.Capabilities.SessionStartContext,
				UserPromptContext:   in.Capabilities.UserPromptContext,
				PreToolUseBlock:     in.Capabilities.PreToolUseBlock,
				PostToolUseEvent:    in.Capabilities.PostToolUseEvent,
				StopFeedback:        in.Capabilities.StopFeedback,
				SessionEndReceipt:   in.Capabilities.SessionEndReceipt,
				NativeSkills:        in.Capabilities.NativeSkills,
				MCP:                 in.Capabilities.MCP,
			},
			Limitations:     append([]string(nil), in.Limitations...),
			InjectBudgetMS:  firstPositive(in.InjectBudgetMS, defaultInjectBudgetMS(name)),
			Fallback:        in.Fallback,
			ExpectedVersion: strings.TrimSpace(in.ExpectedVersion),
			// Resolved here rather than left empty so the manifest-loaded registry
			// answers the same posture as the built-in one for the same name.
			Posture: firstNonEmptyString(NormalizePosture(in.Posture), DefaultPosture(name)),
		}
	}
	return Registry{Harnesses: byName}, nil
}

func RegistryRevision(reg Registry) string {
	type harnessSnapshot struct {
		Name                 string       `json:"name"`
		InstructionMechanism string       `json:"instruction_mechanism"`
		Capabilities         Capabilities `json:"capabilities"`
		Limitations          []string     `json:"limitations"`
		InjectBudgetMS       int          `json:"inject_budget_ms,omitempty"`
		Fallback             bool         `json:"fallback,omitempty"`
		ExpectedVersion      string       `json:"expected_version,omitempty"`
		// Posture is part of the registry's identity: two registries that differ
		// only in whether a lane's hold projects to ask or deny are not the same
		// registry, and a revision that hashed them alike would let a rendered
		// instruction pack validate against the wrong one.
		Posture string `json:"posture,omitempty"`
	}
	names := reg.Names()
	snapshot := make([]harnessSnapshot, 0, len(names))
	for _, name := range names {
		h, _ := reg.Get(name)
		limitations := append([]string(nil), h.Limitations...)
		sort.Strings(limitations)
		snapshot = append(snapshot, harnessSnapshot{
			Name:                 h.Name,
			InstructionMechanism: h.InstructionMechanism,
			Capabilities:         h.Capabilities,
			Limitations:          limitations,
			InjectBudgetMS:       h.InjectBudgetMS,
			Fallback:             h.Fallback,
			ExpectedVersion:      h.ExpectedVersion,
			Posture:              reg.Posture(name),
		})
	}
	data, _ := json.Marshal(snapshot)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// canonicalHarnessName maps the names a caller may hold to the single identity
// the registry keys on. The hyphen rewrite matters as much as the "claude" alias:
// switchback names its launch profiles with hyphens (`claude-kimi`) while the
// registry keys on underscores, and every caller bridging the two used to do its
// own ReplaceAll. One that forgot got Generic() with found=false — a fail-open
// stand-in that looks like an unknown harness. Normalizing here prevents that
// static registry fallback so a lane resolves under either form, from any
// caller; this is purely a key-shape concern and does not imply model-router or
// work-delegate enrollment.
func canonicalHarnessName(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "claude" {
		return "claude_code"
	}
	return strings.ReplaceAll(trimmed, "-", "_")
}

func osEntrypointInstruction(row OSEntrypointRow) string {
	parts := []string{"entrypoint:" + strings.TrimSpace(row.Entrypoint)}
	if strings.TrimSpace(row.Generator) != "" {
		parts = append(parts, "generator:"+strings.TrimSpace(row.Generator))
	}
	return strings.Join(parts, " ")
}

func osEntrypointLimitations(sourceID string, source OSEntrypointsFile, row OSEntrypointRow) []string {
	var out []string
	add := func(key, value string) {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, key+"="+value)
		}
	}
	add("source_id", sourceID)
	add("registry_status", source.Status)
	add("entrypoint_status", row.Status)
	add("must_include", source.MustInclude)
	add("check", row.Check)
	add("generator", row.Generator)
	add("last_reviewed", source.LastReviewed)
	add("review_after", source.ReviewAfter)
	add("registry_expires_at", source.ExpiresAt)
	add("entrypoint_expires_at", row.ExpiresAt)
	return out
}

// Generic is the fallback for an unknown harness: assume fail-open, enforce everything in
// the adapter, not the harness.
func Generic() Harness {
	return Harness{
		Name:                 "generic",
		InstructionMechanism: "static instructions file",
		Capabilities:         Capabilities{},
		Limitations:          []string{"assume fail-open; enforce all gates in the adapter, not the harness"},
		InjectBudgetMS:       1000,
		Fallback:             true,
	}
}

// DefaultPosture answers the posture for a harness that did not declare one.
// Only the two harnesses a human actually sits in front of are interactive:
// `claude_code` is the operator's own session and `codex` is the interactive
// CLI. Every other registered name -- the `claude_*` executor lanes,
// prime_agent, hermes, opencode -- is dispatched into a pane or a subprocess
// with nobody watching, and so is every name this function has never heard of.
func DefaultPosture(name string) string {
	switch canonicalHarnessName(name) {
	case "claude_code", "codex":
		return PostureInteractive
	}
	return PostureUnattended
}

// NormalizePosture accepts only the two known values; anything else (including
// a typo in a manifest) resolves to empty so DefaultPosture decides. A posture
// the registry cannot read must not silently become "interactive".
func NormalizePosture(posture string) string {
	switch strings.ToLower(strings.TrimSpace(posture)) {
	case PostureInteractive:
		return PostureInteractive
	case PostureUnattended:
		return PostureUnattended
	}
	return ""
}

// Posture returns the posture in force for name: the registry's declaration
// where it has one, otherwise DefaultPosture.
func (r Registry) Posture(name string) string {
	if h, ok := r.Get(name); ok {
		if posture := NormalizePosture(h.Posture); posture != "" {
			return posture
		}
	}
	return DefaultPosture(name)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func defaultInjectBudgetMS(name string) int {
	switch canonicalHarnessName(name) {
	case "claude_code", "claude_zai", "claude_minimax", "claude_qwen", "claude_zai_full", "codex":
		return 1500
	default:
		return 1000
	}
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

// InjectionBudgetMS returns the fail-open context injection budget for a harness.
func (r Registry) InjectionBudgetMS(name string) int {
	h, _ := r.Get(name)
	return firstPositive(h.InjectBudgetMS, defaultInjectBudgetMS(name))
}

// HookBudgetMS returns the whole-command hook wall-clock budget for a
// harness: the internal fail-open deadline that must fire before the
// harness's own hook kill timeout does. Zero (unset everywhere) means the
// caller's default applies.
func (r Registry) HookBudgetMS(name string) int {
	h, _ := r.Get(name)
	return h.HookBudgetMS
}

// Get returns the harness by name, or the generic fallback with found=false.
// An exact key wins; otherwise the name is canonicalized (alias and hyphen form)
// so `claude-kimi` and `claude_kimi` resolve to the same entry rather than
// silently falling through to the generic stand-in.
func (r Registry) Get(name string) (Harness, bool) {
	if h, ok := r.Harnesses[name]; ok {
		return h, true
	}
	if canonical := canonicalHarnessName(name); canonical != name {
		if h, ok := r.Harnesses[canonical]; ok {
			return h, true
		}
	}
	return Generic(), false
}

// Supports reports whether a harness can verifiably honor a hook (by enum name:
// SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, SessionEnd, Stop).
func (r Registry) Supports(name, hookName string) bool {
	h, _ := r.Get(name)
	switch hookName {
	case "SessionStart":
		return h.Capabilities.SessionStartContext
	case "UserPromptSubmit":
		return h.Capabilities.UserPromptContext
	case "PreToolUse":
		return h.Capabilities.PreToolUseBlock
	case "PostToolUse":
		return h.Capabilities.PostToolUseEvent
	case "Stop":
		return h.Capabilities.StopFeedback
	case "SessionEnd":
		return h.Capabilities.SessionEndReceipt
	}
	return false
}

// Names lists registered harnesses, sorted.
func (r Registry) Names() []string {
	out := make([]string, 0, len(r.Harnesses))
	for n := range r.Harnesses {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
