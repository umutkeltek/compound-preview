package harness

import (
	"time"

	"compound/internal/runattempt"
)

// OhMyPiAdapter is the oh-my-pi instantiation of the pi coding-agent family
// adapter (pi_coding_agent_adapter.go). Adopting oh-my-pi is a parameterisation
// of an adapter that already exists and has already been proven against a real
// binary — not a new integration — because both forks descend from the same
// upstream coding agent and share the headless argv verbatim.
type OhMyPiAdapter = PiCodingAgentAdapter

// ohMyPiSpec pins oh-my-pi's environment facts.
//
// PI_CODING_AGENT_DIR is the agent home override, documented in the shipped
// help text as "Session storage directory (default: ~/<configDir>/agent)" and
// read as process.env.PI_CODING_AGENT_DIR in the bundle. Note the interaction
// the adapter deliberately avoids: passing `--profile <name>` makes oh-my-pi
// OVERWRITE PI_CODING_AGENT_DIR with the profile's own agent dir, which would
// silently defeat per-run isolation. The adapter never passes --profile, and a
// caller that adds one through RunInput.Args takes that isolation loss on.
//
// extraEnv is empty on purpose: oh-my-pi has no PI_SKIP_VERSION_CHECK
// equivalent. `omp update` is an explicit subcommand, but the marketplace
// auto-update path is a settings key rather than an env var, so a governed lane
// pins the version at install time instead — which is what makes the registry
// row's ExpectedVersion load-bearing rather than decorative.
var ohMyPiSpec = piCodingAgentSpec{
	name:           "oh_my_pi",
	binary:         "omp",
	homeEnv:        "PI_CODING_AGENT_DIR",
	homeTempPrefix: "oh-my-pi-",
	// Compound OMP hook shim env var; read at buildCmd time. The launcher
	// sets it (an operator launcher may pass --hook= explicitly;
	// the work dispatcher passes through COMPOUND_OMP_HOOK_PATH or
	// PI_CODING_AGENT_DIR/hooks/pre/compound.ts).
	shimEnvVar: "COMPOUND_OMP_HOOK_PATH",
	// postureFlagFn maps the governed PermissionMode to the OMP
	// --approval-mode argv fragment. The OMP CLI accepts yolo|write|always-ask;
	// unknown modes fail closed inside OhMyPiApprovalArgs so buildCmd never
	// emits a default. Empty / unknown modes map to always-ask, which is
	// the conservative non-interactive default.
	postureFlagFn: OhMyPiApprovalArgs,
	// hookFlagFn maps the resolved shim path to the --hook=<path> argv
	// fragment that loads the shim into omp. An empty / whitespace path is
	// refused here so buildCmd never emits --hook= without a loadable file.
	hookFlagFn: OhMyPiHookArgs,
	// A bypass (yolo) launch without a loadable shim is the silent fail-open
	// the entire R7B packet exists to close: refuse before process start.
	requiresShimForBypass: true,
}

// NewOhMyPiAdapter returns the production adapter. Heartbeats every 5s.
func NewOhMyPiAdapter(store *runattempt.Store) *OhMyPiAdapter {
	return newPiCodingAgentAdapter(ohMyPiSpec, store, defaultHeartbeatInterval)
}

// NewOhMyPiAdapterWithInterval returns an adapter with a custom heartbeat
// cadence. Test-only.
func NewOhMyPiAdapterWithInterval(store *runattempt.Store, interval time.Duration) *OhMyPiAdapter {
	return newPiCodingAgentAdapter(ohMyPiSpec, store, interval)
}

// FormatOhMyPiDryRunPrompt is the canonical argv-printed summary the verifier
// prints when running an oh-my-pi `--dry-run` proof.
func FormatOhMyPiDryRunPrompt(in RunInput) string {
	return formatPiCodingAgentDryRunPrompt(ohMyPiSpec.binary, in)
}
