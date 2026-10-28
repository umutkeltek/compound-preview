package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"strings"
	"sync"

	"compound/internal/runattempt"
)

// SelectionCapability is what a harness launch can actually do with a
// requested selection. It lives on the DefaultRegistry row (Harness.Selection)
// so the registry is the one source: the selection snapshot builder reads it
// for evidence, the shared runner reads it to refuse an unapplicable request,
// and the settings matrix shows it.
//
// The flags are a conservative floor, the same discipline as Capabilities: a
// value is true only when the adapter code places it on the launch. A request
// the launch cannot apply is refused before mint (selection_unsupported:*),
// never recorded-and-dropped.
type SelectionCapability struct {
	// ModelPassable: the launch binds the requested model (argv flag or a
	// model-router tuple that selects the executable for that model).
	ModelPassable bool `json:"model_passable"`
	// EffortPassable: the adapter places the requested effort on the launch.
	EffortPassable bool `json:"effort_passable"`
	// EffortObservable: the launch can read back the effort that will run.
	EffortObservable bool `json:"effort_observable"`
	// SessionPinnable: the launch can pin a native session id.
	SessionPinnable bool `json:"session_pinnable"`
	// ObservedVia names where the observed leg is read: "argv", "wrapper_env", or "".
	ObservedVia string `json:"observed_via,omitempty"`
	// SessionEvidence is the honest native-session evidence when the session
	// is not pinned (an ExecutionSessionEvidencePrefixes value).
	SessionEvidence string `json:"session_evidence,omitempty"`
	// Basis says, in one line, why the row says what it says. Required.
	Basis string `json:"basis"`
}

// claudeLaneSelection is shared by every tuple-backed claude_* lane adapter.
//
// SessionPinnable is true because the Claude adapter allocates a fresh UUIDv4
// per launch and pins it on the argv as --session-id, so the launch CAN pin a
// native session. SessionEvidence still describes the WIRE's contribution --
// the wire supplies no session id of its own -- and the adapter's copied
// per-run snapshot upgrades that to pinned:--session-id evidence once the
// launch has actually pinned it.
func claudeLaneSelection() *SelectionCapability {
	return &SelectionCapability{
		ModelPassable:    true,
		EffortObservable: true,
		ObservedVia:      "wrapper_env",
		SessionPinnable:  true,
		SessionEvidence:  "unsupported:wire does not pin --session-id on the argv; the claude adapter allocates a fresh uuid per launch and pins --session-id on the launch argv, and its copied per-run snapshot records pinned evidence",
		Basis:            "adapter resolves the model-router tuple and launches its wrapper, which pins SB_LANE_CLAUDE_MODEL; argv is --print + permission + the adapter-pinned --session-id, and the wrapper pins its own SB_LANE_REQUESTED_EFFORT, so a requested effort cannot be applied",
	}
}

func codexSelection() *SelectionCapability {
	return &SelectionCapability{
		ModelPassable:    true,
		EffortPassable:   true,
		EffortObservable: true,
		ObservedVia:      "argv",
		SessionEvidence:  "unobservable:Codex native session ID is not captured by this adapter",
		Basis:            "native codex argv carries -m and -c model_reasoning_effort; the adapter refuses a wrapper that cannot attest effort",
	}
}

func openCodeSelection() *SelectionCapability {
	return &SelectionCapability{
		ModelPassable:   true,
		EffortPassable:  true,
		ObservedVia:     "opencode_argv",
		SessionEvidence: "unsupported:prompt-only adapter creates a fresh isolated run; session reuse is forbidden",
		Basis:           "native prompt-only adapter binds the Compound provider/model/endpoint tuple; --model argv is launch evidence, not provider-request capture; explicit --effort low places provider.models[model].options.reasoningEffort='low' in the immutable settings file the subprocess reads; any other requested effort is refused before mint",
	}
}

// noSelectionControl is the floor for a harness whose launch places neither
// model nor effort from the selection.
func noSelectionControl(basis string) *SelectionCapability {
	return &SelectionCapability{Basis: basis}
}

// SelectionCapabilityFor returns the registry row's selection capability.
// Lookup is exact on the registry name (dashes normalized to underscores);
// "claude" is the raw, non-tuple adapter and deliberately does NOT alias to
// claude_code. A name with no row reports ok=false and the zero capability,
// so every requested model or effort on it is refused.
func SelectionCapabilityFor(harnessName string) (SelectionCapability, bool) {
	name := strings.ReplaceAll(strings.TrimSpace(harnessName), "-", "_")
	h, ok := DefaultRegistry().Harnesses[name]
	if !ok || h.Selection == nil {
		return SelectionCapability{}, false
	}
	return *h.Selection, true
}

// SelectionUnsupportedError is the typed refusal for a requested selection
// field the harness cannot apply. Error() is "selection_unsupported:<harness>:<field>".
type SelectionUnsupportedError struct {
	Harness string
	Field   string // model | effort
}

func (e *SelectionUnsupportedError) Error() string {
	return "selection_unsupported:" + e.Harness + ":" + e.Field
}

// CheckSelectionApplicable refuses a snapshot that requests a model or effort
// the named harness cannot apply. A nil snapshot requests nothing.
func CheckSelectionApplicable(harnessName string, snap *runattempt.ExecutionSelectionSnapshot) error {
	if snap == nil {
		return nil
	}
	capability, _ := SelectionCapabilityFor(harnessName)
	name := strings.TrimSpace(harnessName)
	if strings.TrimSpace(snap.RequestedModelID) != "" && !capability.ModelPassable {
		return &SelectionUnsupportedError{Harness: name, Field: "model"}
	}
	if strings.TrimSpace(snap.RequestedEffort) != "" && !capability.EffortPassable {
		return &SelectionUnsupportedError{Harness: name, Field: "effort"}
	}
	return nil
}

// HookPostureReading is one live read of a harness's installed Compound
// PreToolUse posture (enforce | observe | none) and the config it came from.
type HookPostureReading struct {
	Posture      string
	ConfigPath   string
	ConfigDigest string
}

var (
	hookPostureMu     sync.RWMutex
	hookPostureReader func(harnessName string) (HookPostureReading, bool)
)

// SwapHookPostureReader installs the live hook-posture reader (production:
// hookplan's, which is the reader `compound doctor` uses; this package cannot
// import hookplan). A nil reader means the posture is unknown and nothing is
// stamped from it. It returns a restore func for tests.
func SwapHookPostureReader(fn func(harnessName string) (HookPostureReading, bool)) (restore func()) {
	hookPostureMu.Lock()
	prev := hookPostureReader
	hookPostureReader = fn
	hookPostureMu.Unlock()
	return func() {
		hookPostureMu.Lock()
		hookPostureReader = prev
		hookPostureMu.Unlock()
	}
}

func readHookPosture(harnessName string) (HookPostureReading, bool) {
	hookPostureMu.RLock()
	reader := hookPostureReader
	hookPostureMu.RUnlock()
	if reader == nil {
		return HookPostureReading{}, false
	}
	reading, ok := reader(harnessName)
	if !ok || !validHookPosture(reading.Posture) {
		return HookPostureReading{}, false
	}
	return reading, true
}

func validHookPosture(posture string) bool {
	for _, p := range runattempt.ObservedHookPostures {
		if posture == p {
			return true
		}
	}
	return false
}

// settingsDigest is sha256 over the harness, its live hook posture, and the
// digest of the config bytes that posture was read from.
func settingsDigest(harnessName string, reading HookPostureReading) string {
	sum := sha256.Sum256([]byte("compound.settings_digest.v0.1\nharness=" + strings.TrimSpace(harnessName) +
		"\nhook_posture=" + reading.Posture + "\nconfig=" + reading.ConfigDigest + "\n"))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// observeLaunch reads back what the built command will run: codex model and
// effort from argv, claude lane model and effort from the wrapper env, and the
// live hook posture. ok=false when nothing was observable.
func observeLaunch(harnessName string, cmd *exec.Cmd, hookReading HookPostureReading, hookKnown bool) (runattempt.ObservedExecution, bool) {
	var obs runattempt.ObservedExecution
	var sources []string
	capability, _ := SelectionCapabilityFor(harnessName)
	if cmd != nil {
		switch capability.ObservedVia {
		case "argv":
			obs.ObservedModelID, obs.ObservedEffort, sources = observeCodexArgv(cmd.Args)
		case "opencode_argv":
			for i := 1; i+1 < len(cmd.Args); i++ {
				if cmd.Args[i] == "--model" {
					obs.ObservedModelID = cmd.Args[i+1]
					sources = append(sources, "argv:opencode:--model")
					break
				}
			}
		case "wrapper_env":
			obs.ObservedModelID, obs.ObservedEffort, sources = observeWrapperEnv(cmd)
		}
	}
	if hookKnown {
		obs.ObservedHookPosture = hookReading.Posture
		sources = append(sources, "hook_config:"+firstNonEmptyString(hookReading.ConfigPath, "none"))
	}
	if obs.Empty() {
		return runattempt.ObservedExecution{}, false
	}
	obs.Source = strings.Join(sources, ",")
	return obs, true
}

// observeCodexArgv reads -m and -c model_reasoning_effort from the codex
// global flags (everything before the exec subcommand; the prompt is after it).
func observeCodexArgv(argv []string) (model, effort string, sources []string) {
	for i := 1; i+1 < len(argv); i++ {
		if argv[i] == "exec" {
			break
		}
		switch {
		case argv[i] == "-m":
			model = argv[i+1]
			sources = append(sources, "argv:-m")
			i++
		case argv[i] == "-c" && strings.HasPrefix(argv[i+1], "model_reasoning_effort="):
			effort = strings.Trim(strings.TrimPrefix(argv[i+1], "model_reasoning_effort="), `"`)
			sources = append(sources, "argv:model_reasoning_effort")
			i++
		}
	}
	return model, effort, sources
}

// observeWrapperEnv reads SB_LANE_CLAUDE_MODEL / SB_LANE_REQUESTED_EFFORT from
// the launch env, then from the wrapper's own literal exports, which win
// because the wrapper sets them after it starts. The wrapper is read with the
// settings-source reader (bounded regular leaf, literal values only, never
// sourced); a duplicate or non-literal assignment is not observed.
func observeWrapperEnv(cmd *exec.Cmd) (model, effort string, sources []string) {
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, "SB_LANE_CLAUDE_MODEL="); ok {
			model = v
		} else if v, ok := strings.CutPrefix(kv, "SB_LANE_REQUESTED_EFFORT="); ok {
			effort = v
		}
	}
	if model != "" || effort != "" {
		sources = append(sources, "launch_env")
	}
	body, err := readSettingSource(cmd.Path)
	if err != nil {
		return model, effort, sources
	}
	values, statuses := literalShellSettings(string(body), map[string]bool{"SB_LANE_CLAUDE_MODEL": true, "SB_LANE_REQUESTED_EFFORT": true})
	found := false
	if statuses["SB_LANE_CLAUDE_MODEL"] == "literal" {
		model, found = values["SB_LANE_CLAUDE_MODEL"], true
	}
	if statuses["SB_LANE_REQUESTED_EFFORT"] == "literal" {
		effort, found = values["SB_LANE_REQUESTED_EFFORT"], true
	}
	if found {
		sources = append(sources, "wrapper_env:"+cmd.Path)
	}
	return model, effort, sources
}
