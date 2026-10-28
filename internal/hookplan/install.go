package hookplan

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"compound/internal/harness"
	"compound/internal/lanemanifest"
)

// install.go is the WRITER half of the hook lifecycle. hookplan already models desired vs observed
// bindings and reconciles them; InstallHooks materializes canonical Compound bindings into the
// harness-owned configs the reconciler reads — same shape, same paths. It is NO-CLOBBER: it
// preserves every other key and every non-Compound hook, replacing only entries whose command is
// Compound's own for the same event.

// hookCommandMarker identifies compound's own PreToolUse binding so an update never clobbers a user's
// other hooks (the JSON analog of profiles' gen-marker).
const hookCommandMarker = "hook pre-tool-use --live"

const claudeZaiHarness = "claude-zai"

// claudeMinimaxHarness is the sibling isolated-profile harness to claudeZaiHarness: same NO-CLOBBER
// installer, same profile-scoped config, different provider lane.
const claudeMinimaxHarness = "claude-minimax"

// claudeQwenHarness is the third enrolled isolated-profile harness: same NO-CLOBBER installer and
// profile-scoped config as claudeZaiHarness and claudeMinimaxHarness, backing the qwen token-plan
// lane. Enrollment raises the claude_qwen registry floor to the four genuinely installed bindings.
const claudeQwenHarness = "claude-qwen"

type desiredHookBinding struct {
	Event    string
	Matcher  string
	Command  string
	Blocking bool
	// Timeout is the per-binding hook timeout in seconds. It is the integer
	// sibling to matcher in the harness hook JSON schema. Zero means "no
	// timeout override": the harness applies its own default. The install
	// path carries the installed Compound binding's timeout forward across
	// reinstalls so a hand-set timeout is not silently dropped when the
	// installer replaces Compound-owned entries wholesale.
	Timeout int
}

// HarnessMatcher deliberately covers every tool. Interactive postures need a
// complete action-evidence stream, while isolated guarded postures need the same
// coverage for deterministic authorization.
func HarnessMatcher(harness string) string {
	return "*"
}

// ConfigRel is the harness config path relative to the base directory. Claude
// Code uses a project-scoped settings overlay so Compound's lifecycle bindings
// can be scoped to the current repository.
func ConfigRel(harness, scope string) string {
	switch harness {
	case "codex":
		return filepath.Join(".codex", "hooks.json")
	case claudeZaiHarness:
		return filepath.Join(".config", "switchback", "claude", "zai-direct", "settings.json")
	case claudeMinimaxHarness:
		return filepath.Join(".config", "switchback", "claude", "_providers", "minimax-lane", "settings.json")
	case claudeQwenHarness:
		return filepath.Join(".config", "switchback", "claude", "_providers", "qwen", "settings.json")
	default: // claude-code
		return filepath.Join(".claude", claudeSettingsFile(scope))
	}
}

func claudeSettingsFile(scope string) string {
	parts := []string{"settings"}
	if scope == "project" {
		parts = append(parts, "local")
	}
	return strings.Join(append(parts, "json"), ".")
}

// ClaudeZaiProfileDir is the default isolated Claude profile home under a user's base directory.
func ClaudeZaiProfileDir(baseDir string) string {
	return filepath.Join(baseDir, ".config", "switchback", "claude", "zai-direct")
}

// ClaudeMinimaxProfileDir is the default isolated Claude profile home for the minimax lane, under
// a user's base directory.
func ClaudeMinimaxProfileDir(baseDir string) string {
	return filepath.Join(baseDir, ".config", "switchback", "claude", "_providers", "minimax-lane")
}

// ClaudeQwenProfileDir is the default isolated Claude profile home for the qwen lane, under a
// user's base directory. It matches the profile_default recorded for the qwen lane in
// adapters/_lib/lanes.json.
func ClaudeQwenProfileDir(baseDir string) string {
	return filepath.Join(baseDir, ".config", "switchback", "claude", "_providers", "qwen")
}

// LaneHookSettingsPath returns the settings.json path compound installs a
// governed claude lane's hooks into, under baseDir, or ("", false) if the
// harness is not a hook-governed claude lane. It is the one place that names a
// lane's hook settings path (the same convention ConfigRel encodes for install,
// plus the zai-full profile whose profile dir is _providers/zai-lane). Governed
// spawn reads the observed roster from this path; the install path writes it.
func LaneHookSettingsPath(baseDir, harnessName string) (string, bool) {
	var profileDir string
	switch harnessName {
	case "claude-minimax", "claude_minimax":
		profileDir = ClaudeMinimaxProfileDir(baseDir)
	case "claude-qwen", "claude_qwen":
		profileDir = ClaudeQwenProfileDir(baseDir)
	case "claude-zai", "claude_zai":
		profileDir = ClaudeZaiProfileDir(baseDir)
	case "claude_zai_full":
		// The full zai lane runs a separate subscription profile, not the
		// zai-direct profile the base zai lane shares with the direct tap.
		profileDir = filepath.Join(baseDir, ".config", "switchback", "claude", "_providers", "zai-lane")
	default:
		return "", false
	}
	return filepath.Join(profileDir, "settings.json"), true
}

// HookCommand is the exact observation-only command a harness runs for Compound PreToolUse.
func HookCommand(binaryPath, harness string) string {
	return HookCommandForEvent(binaryPath, harness, "PreToolUse")
}

// HookCommandForEvent is the exact command a harness runs for one Compound live-hook event.
func HookCommandForEvent(binaryPath, harness, event string) string {
	if event == "SessionEnd" {
		// Claude-family lanes route through the bounded adapter seam (claude_*|claude-* only;
		// session_end_adapter.go hard-allowlists those names). Every other harness with a
		// SessionEnd event uses the generic verb, which honours the same routing on the
		// handler side (hook.go routes "SessionEnd","Stop" to sessionEnd()) and degrades to
		// fail_open + WARNING when the registry lacks the capability (hook.go:~2091-2095).
		// Adding a SessionEnd binding for a non-claude harness therefore costs no new seam:
		// the same generic verb handles codex now and hermes once its plugin is installed.
		if isClaudeFamilyInstallerLabel(harness) {
			return fmt.Sprintf("%s hook session-end-adapter --live --harness %s", shellQuote(binaryPath), runtimeHarnessName(harness))
		}
		return fmt.Sprintf("%s hook session-end --live --harness %s", shellQuote(binaryPath), runtimeHarnessName(harness))
	}
	verb := map[string]string{
		"SessionStart":     "session-start",
		"UserPromptSubmit": "user-prompt-submit",
		"PreToolUse":       "pre-tool-use",
		"PostToolUse":      "post-tool-use",
	}[event]
	return fmt.Sprintf("%s hook %s --live --harness %s", shellQuote(binaryPath), verb, runtimeHarnessName(harness))
}

// isClaudeFamilyInstallerLabel matches the dashed installer labels (claude-code, claude-zai,
// claude-minimax, claude-qwen) AND the runtimeHarnessName translation (claude_code,
// claude_zai, claude_minimax, claude_qwen) so a SessionEnd binding installed under either
// label routes to the bounded adapter. Anything else -- codex, opencode, hermes, an
// unconfigured future lane -- falls through to the generic session-end verb.
func isClaudeFamilyInstallerLabel(harness string) bool {
	switch strings.TrimSpace(harness) {
	case "claude-code", "claude_code",
		"claude-zai", "claude_zai",
		"claude-minimax", "claude_minimax",
		"claude-qwen", "claude_qwen":
		return true
	default:
		return false
	}
}

// runtimeHarnessName translates the operator-facing installer/profile label to
// the HarnessRegistry identity consumed by live hooks and ContextPack compilation.
// Keeping the translation at the emitted-command boundary preserves readable
// launch labels without letting a dashed label fall through to Generic().
func runtimeHarnessName(installerHarness string) string {
	switch strings.TrimSpace(installerHarness) {
	case "claude-code":
		return "claude_code"
	case claudeZaiHarness:
		return "claude_zai"
	case claudeMinimaxHarness:
		return "claude_minimax"
	case claudeQwenHarness:
		return "claude_qwen"
	default:
		return strings.TrimSpace(installerHarness)
	}
}

// shellQuote wraps s in POSIX single quotes so a shell (or any shell-word splitter, including the
// harness that execs this command) treats it as exactly one token even when it contains spaces or
// other shell metacharacters, e.g. an app-bundle install path like `/tmp/Compound Apps/compound`.
// Without this, a spaced binaryPath silently truncates at the first space and the harness execs a
// nonexistent binary, which fails the mutation-policy guard open. An embedded single quote is
// escaped with the standard POSIX idiom: close the quote, insert a backslash-escaped quote
// character, then reopen the quote (see the ReplaceAll call below for the exact byte sequence).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// HookInstallBinding is one lifecycle event binding reported by an install/check pass.
type HookInstallBinding struct {
	Event    string `json:"event"`
	Matcher  string `json:"matcher,omitempty"`
	Command  string `json:"command"`
	Blocking bool   `json:"blocking"`
	// Timeout is the per-binding hook timeout in seconds carried into the
	// harness hook JSON. Empty when the binding has no timeout override
	// (the harness applies its own default). When set, the install path
	// round-trips it across reinstalls so a hand-set timeout is never
	// silently dropped with the wholesale Compound-entry replace.
	Timeout int `json:"timeout,omitempty"`
}

// HookInstallEntry is the per-harness outcome of an install/check pass.
type HookInstallEntry struct {
	Harness        string               `json:"harness"`
	ConfigPath     string               `json:"config_path"`
	ConfigRevision string               `json:"config_revision"` // sha256 of the exact desired config bytes
	Action         string               `json:"action"`          // installed | updated | ok | drift | absent
	Bindings       []HookInstallBinding `json:"bindings"`
	Matcher        string               `json:"matcher"` // blocking guard matcher; empty for observation-only posture
	Command        string               `json:"command"` // blocking guard command; empty for observation-only posture
}

// HookInstallReport is the full install/check result.
type HookInstallReport struct {
	Check   bool               `json:"check"`
	Scope   string             `json:"scope"`
	Entries []HookInstallEntry `json:"entries"`
}

// InstallHooks writes (or, with check=true, only reports) Compound bindings into each requested
// harness config under baseDir. An empty harness list means every managed harness.
func InstallHooks(baseDir, binaryPath, scope string, harnesses []string, check bool) (HookInstallReport, error) {
	return InstallHooksWithConfigDir(baseDir, binaryPath, scope, harnesses, "", check)
}

// InstallHooksWithConfigDir supports an explicit isolated profile directory for a single
// isolated-profile harness (claude-zai, claude-minimax, or claude-qwen). The override is
// intentionally narrow: mixing it with another harness would make one path flag carry two
// incompatible meanings.
func InstallHooksWithConfigDir(baseDir, binaryPath, scope string, harnesses []string, configDir string, check bool) (HookInstallReport, error) {
	return InstallHooksWithConfigDirAndPublish(baseDir, binaryPath, scope, harnesses, configDir, check, nil)
}

// InstallHooksWithConfigDirAndPublish installs every requested native config as one transaction
// with its authority publisher. All configs are parsed and staged before the first write. If a
// native commit, byte verification, or publication fails, every config already written by this
// call is restored to its exact prior bytes and mode.
func InstallHooksWithConfigDirAndPublish(
	baseDir, binaryPath, scope string,
	harnesses []string,
	configDir string,
	check bool,
	publish func(HookInstallReport) error,
) (HookInstallReport, error) {
	return installHooksImpl(baseDir, binaryPath, scope, harnesses, configDir, nil, check, publish)
}

// InstallHooksForLane installs the roster a lane manifest entitles into the
// lane's derived install harness. It is the compound-owned projection of the
// manifest's hooks.roster field. The harness set is exactly the derived install
// harness, so the config-dir override keeps its single-profile meaning.
func InstallHooksForLane(
	baseDir, binaryPath, scope, installHarness, configDir string,
	roster lanemanifest.Roster,
	check bool,
	publish func(HookInstallReport) error,
) (HookInstallReport, error) {
	allowedEvents, err := RosterEventSet(roster)
	if err != nil {
		return HookInstallReport{}, err
	}
	return InstallHooksWithRoster(baseDir, binaryPath, scope, []string{installHarness}, configDir, allowedEvents, check, publish)
}

// InstallHooksWithRoster is the roster-aware form of InstallHooksWithConfigDirAndPublish.
// A nil allowedEvents installs every compiled binding (the pre-roster default); a roster
// narrows the installed native events to the roster's set. `compound hook install
// --lane-file` and `--roster` route through this so one install path serves both the
// manifest-driven and the explicit-roster selection.
func InstallHooksWithRoster(
	baseDir, binaryPath, scope string,
	harnesses []string,
	configDir string,
	allowedEvents map[string]bool,
	check bool,
	publish func(HookInstallReport) error,
) (HookInstallReport, error) {
	return installHooksImpl(baseDir, binaryPath, scope, harnesses, configDir, allowedEvents, check, publish)
}

// installHooksImpl is the transaction body shared by every install entry point.
// A nil allowedEvents installs every compiled binding (the pre-roster default);
// a roster narrows the installed native events.
func installHooksImpl(
	baseDir, binaryPath, scope string,
	harnesses []string,
	configDir string,
	allowedEvents map[string]bool,
	check bool,
	publish func(HookInstallReport) error,
) (HookInstallReport, error) {
	if strings.TrimSpace(baseDir) == "" {
		return HookInstallReport{}, fmt.Errorf("base dir required")
	}
	if strings.TrimSpace(binaryPath) == "" {
		return HookInstallReport{}, fmt.Errorf("binary path required")
	}
	report := HookInstallReport{Check: check, Scope: scope}
	want := map[string]bool{}
	for _, h := range harnesses {
		want[strings.TrimSpace(h)] = true
	}
	if strings.TrimSpace(configDir) != "" {
		if len(want) != 1 || (!want["claude-code"] && !want[claudeZaiHarness] && !want[claudeMinimaxHarness] && !want[claudeQwenHarness]) {
			return report, fmt.Errorf(
				"config dir is supported only with a single Claude profile harness (claude-code, %s, %s, or %s)",
				claudeZaiHarness, claudeMinimaxHarness, claudeQwenHarness,
			)
		}
	}
	prepared := make([]preparedHookConfig, 0, 5)
	for _, harness := range []string{"claude-code", claudeZaiHarness, claudeMinimaxHarness, claudeQwenHarness, "codex"} {
		if len(want) > 0 && !want[harness] {
			continue
		}
		configPath := filepath.Join(baseDir, ConfigRel(harness, scope))
		if strings.TrimSpace(configDir) != "" && (harness == "claude-code" || harness == claudeZaiHarness || harness == claudeMinimaxHarness || harness == claudeQwenHarness) {
			configPath = filepath.Join(configDir, "settings.json")
		}
		bindings, _, err := compiledManagedHookBindings(binaryPath, harness, scope, allowedEvents)
		if err != nil {
			return report, fmt.Errorf("%s: %w", harness, err)
		}
		reportedBindings := make([]HookInstallBinding, 0, len(bindings))
		for _, binding := range bindings {
			reportedBindings = append(reportedBindings, HookInstallBinding{
				Event:    binding.Event,
				Matcher:  binding.Matcher,
				Command:  binding.Command,
				Blocking: binding.Blocking,
			})
		}
		matcher, command := installedGuardProjection(bindings)
		staged, installedTimeouts, err := prepareHookBindings(harness, configPath, bindings, check)
		if err != nil {
			return report, fmt.Errorf("%s: %w", harness, err)
		}
		// Project the installed per-event timeout onto the reported bindings so
		// the install report reflects the live, on-disk timeout that survived
		// the wholesale Compound-entry replace (hand-set timeout adopted from
		// the prior entry, or the desired timeout if pinned).
		for i := range reportedBindings {
			if t, ok := installedTimeouts[reportedBindings[i].Event]; ok {
				reportedBindings[i].Timeout = t
			}
		}
		prepared = append(prepared, staged)
		report.Entries = append(report.Entries, HookInstallEntry{
			Harness: harness, ConfigPath: configPath, ConfigRevision: hookConfigRevision(staged.after),
			Action: staged.action, Matcher: matcher, Command: command,
			Bindings: reportedBindings,
		})
	}
	if check {
		return report, nil
	}
	if err := commitHookInstallTransaction(prepared, report, publish, nil); err != nil {
		return report, err
	}
	return report, nil
}

func commitHookInstallTransaction(
	prepared []preparedHookConfig,
	report HookInstallReport,
	publish func(HookInstallReport) error,
	beforeCommit func(preparedHookConfig) error,
) error {
	committed := make([]preparedHookConfig, 0, len(prepared))
	for _, staged := range prepared {
		if !staged.shouldWrite {
			continue
		}
		if beforeCommit != nil {
			if err := beforeCommit(staged); err != nil {
				return errors.Join(
					fmt.Errorf("%s: before hook config commit: %w", staged.harness, err),
					rollbackHookConfigs(committed),
				)
			}
		}
		if err := commitPreparedHookConfig(staged); err != nil {
			rollbackErr := rollbackHookConfigs(committed)
			return errors.Join(
				fmt.Errorf("%s: write hook config: %w", staged.harness, err),
				rollbackErr,
			)
		}
		committed = append(committed, staged)
	}
	if err := verifyHookConfigs(prepared); err != nil {
		return errors.Join(err, rollbackHookConfigs(committed))
	}
	if publish != nil {
		if err := publish(report); err != nil {
			return errors.Join(
				fmt.Errorf("publish installed-hook authority: %w", err),
				rollbackHookConfigs(committed),
			)
		}
	}
	return nil
}

type preparedHookConfig struct {
	harness     string
	path        string
	existed     bool
	before      []byte
	after       []byte
	mode        os.FileMode
	preimage    os.FileInfo
	action      string
	shouldWrite bool
}

func compiledManagedHookBindings(
	binaryPath string,
	harnessName string,
	configScope string,
	allowedEvents map[string]bool,
) ([]desiredHookBinding, CompiledPosture, error) {
	registry := harness.DefaultRegistry()
	if harnessName == claudeZaiHarness || harnessName == claudeMinimaxHarness || harnessName == claudeQwenHarness {
		configScope = "profile"
	}
	compiled, err := CompileDefaultPosture(DefaultPostureRequest{
		Harness:         harnessName,
		Registry:        registry,
		HandlerRef:      "compound",
		HandlerRevision: "binary:" + binaryPath,
		ConfigScope:     configScope,
	})
	if err != nil {
		return nil, CompiledPosture{}, err
	}
	// Validation reads the FULL posture so the mutation-readiness and
	// observational-default contracts are judged on semantics, never on the
	// narrowed roster. Only the installed native bindings are filtered.
	if err := validateInstallableDefaultPosture(harnessName, compiled); err != nil {
		return nil, compiled, err
	}
	bindings := make([]desiredHookBinding, 0, len(compiled.HookPlan.Bindings))
	for _, binding := range selectBindings(compiled.HookPlan.Bindings, allowedEvents) {
		command := HookCommandForEvent(binaryPath, harnessName, binding.Event)
		if binding.Blocking {
			command += " --enforce"
		}
		bindings = append(bindings, desiredHookBinding{
			Event:    binding.Event,
			Matcher:  binding.Matcher,
			Command:  command,
			Blocking: binding.Blocking,
		})
	}
	return bindings, compiled, nil
}

func validateInstallableDefaultPosture(harnessName string, compiled CompiledPosture) error {
	if compiled.MutationReady {
		return nil
	}
	registryName := strings.ReplaceAll(strings.TrimSpace(harnessName), "-", "_")
	if (registryName == "codex" || registryName == "claude_code") &&
		compiled.CompiledFrom.PostureID == "hpost_default_"+registryName &&
		isInteractiveObservationalDefault(compiled.Intentions) {
		return nil
	}
	return fmt.Errorf("compiled posture is not mutation-ready")
}

func isInteractiveObservationalDefault(intentions []CompiledIntention) bool {
	// The upper bound is the size of the allowed set below -- prepare_context,
	// prepare_turn_context, observe_action, observe_outcome, record_receipt -- which
	// grew by one when per-turn injection became its own intention. It is a
	// nothing-unexpected-snuck-in count, not a substitute for the per-intention checks:
	// unknown intentions, guard_mutation, and any non-native disposition are still
	// refused one by one -- EXCEPT record_receipt, which carries its own attestation
	// gate (the adapter returns DispositionUnsupported when its capability feature is
	// false). The default posture for an interactive harness asks for record_receipt
	// unconditionally so the flip from "registry says false" to "registry says true" (or
	// vice versa) lands as a registry-only change; refusing the install because the receipt
	// compiled Unsupported would defeat the rule_delta seam (the codex rule_delta flipped
	// SessionEndReceipt false->true, see harness.go:458). Tolerating Unsupported for
	// record_receipt keeps the invariant the function exists to defend: no blocking guard
	// is added. record_receipt has no blocking shape by construction.
	if len(intentions) < 3 || len(intentions) > 5 {
		return false
	}
	seen := map[HarnessIntention]bool{}
	for _, intention := range intentions {
		if intention.Intention == IntentionGuardMutation {
			return false
		}
		if intention.Intention != IntentionRecordReceipt &&
			intention.Disposition != DispositionNative {
			return false
		}
		switch intention.Intention {
		case IntentionPrepareContext, IntentionPrepareTurnContext:
			if intention.EnforcementStrength != "context" {
				return false
			}
		case IntentionObserveAction, IntentionObserveOutcome:
			if intention.EnforcementStrength != "observe" {
				return false
			}
		case IntentionRecordReceipt:
			if intention.Disposition == DispositionNative &&
				intention.EnforcementStrength != "receipt" {
				return false
			}
		default:
			return false
		}
		seen[intention.Intention] = true
	}
	return seen[IntentionPrepareContext] &&
		seen[IntentionObserveAction] &&
		seen[IntentionObserveOutcome]
}

func installedGuardProjection(bindings []desiredHookBinding) (string, string) {
	for _, binding := range bindings {
		if binding.Event == "PreToolUse" && binding.Blocking {
			return binding.Matcher, binding.Command
		}
	}
	return "", ""
}

// mergePreToolUseBinding merges compound's PreToolUse binding into an existing harness config without
// disturbing any other content. It identifies compound's own entry by command marker so re-installs
// update in place and a user's other PreToolUse hooks are never touched.
func mergePreToolUseBinding(configPath, matcher, command string, check bool) (string, error) {
	return mergeHookBindings(configPath, []desiredHookBinding{{Event: "PreToolUse", Matcher: matcher, Command: command}}, check)
}

// mergeHookBindings retains the narrow single-config seam used by package tests. Multi-harness
// installs use InstallHooksWithConfigDirAndPublish so every selected config is prepared first.
func mergeHookBindings(configPath string, bindings []desiredHookBinding, check bool) (string, error) {
	prepared, _, err := prepareHookBindings("", configPath, bindings, check)
	if err != nil {
		return "", err
	}
	if !prepared.shouldWrite {
		return prepared.action, nil
	}
	if err := commitPreparedHookConfig(prepared); err != nil {
		return "", err
	}
	return prepared.action, nil
}

func prepareHookBindings(harnessName, configPath string, bindings []desiredHookBinding, check bool) (preparedHookConfig, map[string]int, error) {
	prepared := preparedHookConfig{
		harness: harnessName,
		path:    configPath,
		mode:    0o644,
	}
	for _, binding := range bindings {
		if strings.TrimSpace(binding.Event) == "" || strings.TrimSpace(binding.Command) == "" {
			return preparedHookConfig{}, nil, fmt.Errorf("hook binding needs event and command")
		}
	}

	root := map[string]any{}
	info, err := os.Lstat(configPath)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return preparedHookConfig{}, nil, fmt.Errorf("refuse symlinked hook config %s", configPath)
		}
		if !info.Mode().IsRegular() {
			return preparedHookConfig{}, nil, fmt.Errorf("hook config %s must be a regular file", configPath)
		}
		data, readErr := os.ReadFile(configPath)
		if readErr != nil {
			return preparedHookConfig{}, nil, readErr
		}
		afterReadInfo, statErr := os.Lstat(configPath)
		if statErr != nil {
			return preparedHookConfig{}, nil, fmt.Errorf("hook config %s changed during preflight: %w", configPath, statErr)
		}
		if afterReadInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, afterReadInfo) {
			return preparedHookConfig{}, nil, fmt.Errorf("hook config %s changed during preflight", configPath)
		}
		prepared.existed = true
		prepared.before = append([]byte(nil), data...)
		prepared.mode = afterReadInfo.Mode()
		prepared.preimage = afterReadInfo
		if len(strings.TrimSpace(string(data))) > 0 {
			if err := json.Unmarshal(data, &root); err != nil {
				return preparedHookConfig{}, nil, fmt.Errorf("parse %s: %w", configPath, err)
			}
			if root == nil {
				root = map[string]any{}
			}
		}
	case os.IsNotExist(err):
	case err != nil:
		return preparedHookConfig{}, nil, err
	}

	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	action := "ok"
	desiredByEvent := make(map[string]desiredHookBinding, len(bindings))
	for _, binding := range bindings {
		desiredByEvent[binding.Event] = binding
	}
	// installedTimeoutByEvent records the per-binding timeout (seconds) that
	// reconcileHookEventGroups actually installed for each event. Per-event
	// here so the install report can echo the live, on-disk timeout — not
	// just the desired timeout — so the caller observes the hand-set
	// timeout that survived the wholesale Compound-entry replace.
	installedTimeoutByEvent := make(map[string]int, len(bindings))
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd"} {
		groups, _ := hooks[event].([]any)
		binding, desired := desiredByEvent[event]
		var desiredBinding *desiredHookBinding
		if desired {
			desiredBinding = &binding
		}
		reconciled, removed, installedTimeout := reconcileHookEventGroups(groups, event, desiredBinding)
		if installedTimeout > 0 {
			installedTimeoutByEvent[event] = installedTimeout
		}
		if sameHookGroups(groups, reconciled) {
			continue
		}
		if check {
			next := "drift"
			if desired && removed == 0 {
				next = "absent"
			}
			action = combineHookInstallAction(action, next, true)
		} else {
			next := "updated"
			if desired && removed == 0 {
				next = "installed"
			}
			action = combineHookInstallAction(action, next, false)
		}
		if len(reconciled) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = reconciled
		}
	}
	prepared.action = action
	if action == "ok" {
		prepared.after = append([]byte(nil), prepared.before...)
		return prepared, installedTimeoutByEvent, nil
	}
	root["hooks"] = hooks
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return preparedHookConfig{}, nil, err
	}
	prepared.after = append(data, '\n')
	prepared.shouldWrite = !check
	return prepared, installedTimeoutByEvent, nil
}

func hookConfigRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum)
}

func verifyHookConfigs(configs []preparedHookConfig) error {
	for _, config := range configs {
		info, err := os.Lstat(config.path)
		if err != nil {
			return fmt.Errorf("verify installed %s hook config: %w", config.harness, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("verify installed %s hook config: target is not a regular file", config.harness)
		}
		actual, err := os.ReadFile(config.path)
		if err != nil {
			return fmt.Errorf("verify installed %s hook config: %w", config.harness, err)
		}
		afterReadInfo, err := os.Lstat(config.path)
		if err != nil {
			return fmt.Errorf("verify installed %s hook config: %w", config.harness, err)
		}
		if !afterReadInfo.Mode().IsRegular() || !os.SameFile(info, afterReadInfo) {
			return fmt.Errorf("verify installed %s hook config: target changed while reading", config.harness)
		}
		if !bytes.Equal(actual, config.after) {
			return fmt.Errorf(
				"verify installed %s hook config: revision mismatch: got %s want %s",
				config.harness,
				hookConfigRevision(actual),
				hookConfigRevision(config.after),
			)
		}
		if afterReadInfo.Mode() != config.mode {
			return fmt.Errorf(
				"verify installed %s hook config: mode mismatch: got %v want %v",
				config.harness,
				afterReadInfo.Mode(),
				config.mode,
			)
		}
	}
	return nil
}

func rollbackHookConfigs(configs []preparedHookConfig) error {
	var rollbackErr error
	for i := len(configs) - 1; i >= 0; i-- {
		config := configs[i]
		info, err := os.Lstat(config.path)
		if os.IsNotExist(err) && !config.existed {
			continue
		}
		if err != nil {
			rollbackErr = errors.Join(
				rollbackErr,
				fmt.Errorf(
					"rollback %s hook config %s refused; target changed after commit: %w",
					config.harness,
					config.path,
					err,
				),
			)
			continue
		}
		if !info.Mode().IsRegular() {
			rollbackErr = errors.Join(
				rollbackErr,
				fmt.Errorf(
					"rollback %s hook config %s refused; target changed after commit: not a regular file",
					config.harness,
					config.path,
				),
			)
			continue
		}
		current, err := os.ReadFile(config.path)
		if err != nil {
			rollbackErr = errors.Join(
				rollbackErr,
				fmt.Errorf(
					"rollback %s hook config %s refused; target changed after commit: %w",
					config.harness,
					config.path,
					err,
				),
			)
			continue
		}
		afterReadInfo, err := os.Lstat(config.path)
		if err != nil ||
			!afterReadInfo.Mode().IsRegular() ||
			!os.SameFile(info, afterReadInfo) ||
			!bytes.Equal(current, config.after) ||
			afterReadInfo.Mode() != config.mode {
			rollbackErr = errors.Join(
				rollbackErr,
				fmt.Errorf(
					"rollback %s hook config %s refused; target changed after commit",
					config.harness,
					config.path,
				),
			)
			continue
		}
		if config.existed {
			if err := writeHookConfigAtomically(config.path, config.before, config.mode); err != nil {
				rollbackErr = errors.Join(
					rollbackErr,
					fmt.Errorf("rollback %s hook config %s: %w", config.harness, config.path, err),
				)
			}
			continue
		}
		if err := os.Remove(config.path); err != nil && !os.IsNotExist(err) {
			rollbackErr = errors.Join(
				rollbackErr,
				fmt.Errorf("rollback %s hook config %s: %w", config.harness, config.path, err),
			)
		}
	}
	return rollbackErr
}

func commitPreparedHookConfig(config preparedHookConfig) error {
	if err := verifyHookConfigPreimage(config); err != nil {
		return err
	}
	return writeHookConfigAtomically(config.path, config.after, config.mode)
}

func verifyHookConfigPreimage(config preparedHookConfig) error {
	info, err := os.Lstat(config.path)
	if !config.existed {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("hook config %s changed since preflight: %w", config.path, err)
		}
		return fmt.Errorf("hook config %s changed since preflight: target appeared", config.path)
	}
	if err != nil {
		return fmt.Errorf("hook config %s changed since preflight: %w", config.path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("hook config %s changed since preflight: target became a symlink", config.path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("hook config %s changed since preflight: target is no longer a regular file", config.path)
	}
	if config.preimage == nil || !os.SameFile(config.preimage, info) {
		return fmt.Errorf("hook config %s changed since preflight: file identity changed", config.path)
	}
	current, err := os.ReadFile(config.path)
	if err != nil {
		return fmt.Errorf("hook config %s changed since preflight: %w", config.path, err)
	}
	afterReadInfo, err := os.Lstat(config.path)
	if err != nil {
		return fmt.Errorf("hook config %s changed since preflight: %w", config.path, err)
	}
	if afterReadInfo.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(info, afterReadInfo) ||
		!os.SameFile(config.preimage, afterReadInfo) {
		return fmt.Errorf("hook config %s changed since preflight: file identity changed", config.path)
	}
	if !bytes.Equal(current, config.before) {
		return fmt.Errorf("hook config %s changed since preflight: bytes changed", config.path)
	}
	if afterReadInfo.Mode() != config.mode {
		return fmt.Errorf("hook config %s changed since preflight: mode changed", config.path)
	}
	return nil
}

func writeHookConfigAtomically(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".compound-hook-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	keepTemp := true
	defer func() {
		if keepTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		return errors.Join(err, temp.Close())
	}
	if _, err := temp.Write(data); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := temp.Sync(); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	keepTemp = false
	return nil
}

func combineHookInstallAction(current, next string, check bool) string {
	if check {
		if current == "drift" || next == "drift" {
			return "drift"
		}
		if current == "absent" || next == "absent" {
			return "absent"
		}
		return "ok"
	}
	if current == "updated" || next == "updated" {
		return "updated"
	}
	if current == "installed" || next == "installed" {
		return "installed"
	}
	return "ok"
}

func hookIsCompoundEvent(hook any, event string) bool {
	hookMap, ok := hook.(map[string]any)
	if !ok {
		return false
	}
	command, _ := hookMap["command"].(string)
	if event == "SessionStart" && isLegacyRecallContextSessionStartCommand(command) {
		return true
	}
	if event == "SessionEnd" {
		trimmed := strings.TrimSpace(command)
		return trimmed == `"$HOME/.claude/hooks/SessionReceipt.hook.ts"` ||
			isCompoundSessionEndCommand(trimmed)
	}
	verb := map[string]string{
		"SessionStart":     "session-start",
		"UserPromptSubmit": "user-prompt-submit",
		"PreToolUse":       "pre-tool-use",
		"PostToolUse":      "post-tool-use",
	}[event]
	marker := "hook " + verb + " --live"
	return strings.Contains(command, marker)
}

func isLegacyRecallContextSessionStartCommand(command string) bool {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 1 {
		return isLegacyRecallContextPath(fields[0], ".claude")
	}
	if len(fields) == 3 {
		if fields[0] != "RECALL_HARNESS=codex" {
			return false
		}
		fields = fields[1:]
	}
	if len(fields) != 2 ||
		filepath.Base(filepath.Clean(trimHookShellToken(fields[0]))) != "bun" {
		return false
	}
	return isLegacyRecallContextPath(fields[1], ".codex")
}

func isLegacyRecallContextPath(token, harnessDir string) bool {
	token = filepath.ToSlash(filepath.Clean(trimHookShellToken(token)))
	return strings.HasSuffix(token, "/"+harnessDir+"/hooks/RecallContext.hook.ts")
}

func trimHookShellToken(token string) string {
	token = strings.TrimSpace(token)
	if len(token) >= 2 {
		if (token[0] == '\'' && token[len(token)-1] == '\'') ||
			(token[0] == '"' && token[len(token)-1] == '"') {
			return token[1 : len(token)-1]
		}
	}
	return token
}

func isCompoundSessionEndCommand(command string) bool {
	trimmed := strings.TrimSpace(command)
	// Two verb paths feed the SessionEnd binding: session-end-adapter (Claude family only,
	// bounded adapter seam) and session-end (generic; routes through sessionEnd() in
	// hook.go, which is what codex 0.149.x uses post rule_delta). The harness gate below
	// matches what HookCommandForEvent actually emits: the bounded adapter stays
	// Claude-family, the generic verb is permitted for codex and any future lane with a
	// native SessionEnd event. The legacy `$HOME/.claude/hooks/SessionReceipt.hook.ts`
	// path is captured earlier in hookIsCompoundEvent and does not reach this function.
	for _, c := range []struct {
		verb       string
		claudeOnly bool
	}{
		{verb: "session-end-adapter", claudeOnly: true},
		{verb: "session-end", claudeOnly: false},
	} {
		marker := " hook " + c.verb + " --live --harness "
		markerAt := strings.LastIndex(trimmed, marker)
		if markerAt < 0 {
			continue
		}
		executable := strings.TrimSpace(trimmed[:markerAt])
		harnessName := strings.TrimSpace(trimmed[markerAt+len(marker):])
		if c.claudeOnly && !isClaudeFamilyHarnessName(harnessName) {
			continue
		}
		if !isCompoundExecutableToken(executable) {
			continue
		}
		return true
	}
	return false
}

func isClaudeFamilyHarnessName(name string) bool {
	switch strings.TrimSpace(name) {
	case "claude-code", "claude_code",
		"claude-zai", "claude_zai",
		"claude-minimax", "claude_minimax",
		"claude-qwen", "claude_qwen":
		return true
	default:
		return false
	}
}

func isCompoundExecutableToken(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	if strings.HasPrefix(token, "'") {
		if !strings.HasSuffix(token, "'") || len(token) < 2 {
			return false
		}
		decoded := strings.ReplaceAll(token[1:len(token)-1], `'\''`, `'`)
		if shellQuote(decoded) != token {
			return false
		}
		token = decoded
	} else if strings.ContainsAny(token, " \t\r\n'\"`$;&|<>") {
		return false
	}
	return filepath.Base(filepath.Clean(token)) == "compound"
}

func reconcileHookEventGroups(
	groups []any,
	event string,
	desired *desiredHookBinding,
) ([]any, int, int) {
	reconciled := make([]any, 0, len(groups)+1)
	insertAt := -1
	removed := 0
	// adoptedTimeout is the integer seconds timeout observed on the existing
	// Compound entry (if any). The install path carries it forward across
	// wholesale replaces so a hand-set timeout survives reinstall. It is
	// only adopted when the desired binding did not pin a timeout and the
	// existing entry explicitly set one (timeout > 0); a zero valued
	// already-installed binding is a deliberate "no override" and is
	// preserved as such — adopting 0 would be a no-op anyway.
	adoptedTimeout := 0
	for _, group := range groups {
		entry, ok := group.(map[string]any)
		if !ok {
			reconciled = append(reconciled, group)
			continue
		}
		inner, ok := entry["hooks"].([]any)
		if !ok {
			reconciled = append(reconciled, group)
			continue
		}
		groupHasCompound := false
		for _, hook := range inner {
			if hookIsCompoundEvent(hook, event) {
				groupHasCompound = true
				break
			}
		}
		kept := make([]any, 0, len(inner))
		removedFromGroup := 0
		for _, hook := range inner {
			if hookIsCompoundEvent(hook, event) {
				removed++
				removedFromGroup++
				continue
			}
			kept = append(kept, hook)
		}
		if adoptedTimeout == 0 && groupHasCompound {
			if t, ok := entry["timeout"].(float64); ok && t > 0 {
				adoptedTimeout = int(t)
			} else if t, ok := entry["timeout"].(int); ok && t > 0 {
				adoptedTimeout = t
			}
		}
		if removedFromGroup == 0 {
			reconciled = append(reconciled, group)
			continue
		}
		if len(kept) > 0 {
			preserved := make(map[string]any, len(entry))
			for key, value := range entry {
				preserved[key] = value
			}
			preserved["hooks"] = kept
			reconciled = append(reconciled, preserved)
		}
		if desired != nil && insertAt < 0 {
			insertAt = len(reconciled)
		}
	}
	if desired == nil {
		if removed == 0 {
			return groups, 0, 0
		}
		return reconciled, removed, 0
	}
	desiredEntry := map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": desired.Command}},
	}
	if strings.TrimSpace(desired.Matcher) != "" {
		desiredEntry["matcher"] = desired.Matcher
	}
	// Per-binding timeout round-trip: when the desired binding did not pin
	// a timeout but the prior Compound entry had one, carry it forward so a
	// hand-set timeout is not silently dropped on reinstall. A desired-side
	// timeout (e.g. from the compiled posture) wins, matching the rest of
	// the desiredEntry semantics.
	timeout := desired.Timeout
	if timeout == 0 && adoptedTimeout > 0 {
		timeout = adoptedTimeout
	}
	if timeout > 0 {
		desiredEntry["timeout"] = timeout
	}
	if insertAt < 0 {
		insertAt = len(reconciled)
	}
	reconciled = append(reconciled, nil)
	copy(reconciled[insertAt+1:], reconciled[insertAt:])
	reconciled[insertAt] = desiredEntry
	return reconciled, removed, timeout
}

func sameHookGroups(a, b []any) bool {
	ad, _ := json.Marshal(a)
	bd, _ := json.Marshal(b)
	return bytes.Equal(ad, bd)
}
