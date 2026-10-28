// Package console owns the launch-plan construction, capability validation,
// child-launch environment sanitization, and instance-pinning inputs for the
// `compound console` CLI. It is a Leaf organ under two-class law:
//
//   - Enforcement: BuildLifecycleLaunchPlan validates mutation posture and
//     marks the resulting plan. LaunchEnv refuses mutation posture from plans
//     that did not pass that builder gate.
//   - Evidence: LaunchPlan carries the schema_version, mode, lifecycle, env
//     keyset, and an explicit CapabilityRequired bool. Its JSON marshaler
//     always removes the out-of-band capability value.
//
// Choosing MintEnforcement: the launch validator blocks unsafe activation in
// the runtime gate, so the contract is enforcement rather than advisory.
package console

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/home"
)

const (
	// LaunchPlanSchemaVersion is published with every JSON LaunchPlan; consumers
	// must reject older majors.
	LaunchPlanSchemaVersion = "compound.console_launch.v0.3"

	// CapabilityEnv is the exact env var name that carries the out-of-band 64-hex
	// runtime capability. It is NEVER serialized into the LaunchPlan JSON and
	// is stripped from operator-shell ambient before child launch.
	CapabilityEnv = "COMPOUND_CONSOLE_CAPABILITY"

	// LegacySessionEnv is the legacy session token name (v0.2 and earlier). It
	// is stripped from launch env because valid public-alpha flows use the
	// out-of-band capability, not the legacy header.
	LegacySessionEnv = "COMPOUND_CONSOLE_SESSION_TOKEN"

	// MutationsEnv and PrivateFallbacksEnv carry the per-process posture that
	// the child inherits. They are published by the launch plan and never read
	// from operator-shell ambient.
	MutationsEnv        = "COMPOUND_CONSOLE_MUTATIONS"
	PrivateFallbacksEnv = "COMPOUND_CONSOLE_PRIVATE_FALLBACKS"

	listenHost            = "127.0.0.1"
	listenPort            = "3777"
	ListenAddress         = listenHost + ":" + listenPort
	consoleCompoundExeKey = "COMPOUND_BIN"
	consoleBuildKindDev   = "development"
	consoleBuildKindProd  = "production"
)

// Lifecycle names. Build refuses mutation; serve refuses private fallbacks.
const (
	LifecycleDev   = "dev"
	LifecycleBuild = "build"
	LifecycleServe = "serve"
)

// LaunchPlan is the operator-facing JSON model of the next console child
// process. CapabilityRequired is reported so the operator can see *that* a
// capability was required, but the value itself never appears.
type LaunchPlan struct {
	SchemaVersion      string            `json:"schema_version"`
	GeneratedAt        string            `json:"generated_at"`
	Mode               string            `json:"mode"`
	Lifecycle          string            `json:"lifecycle"`
	ConsoleDir         string            `json:"console_dir"`
	Command            []string          `json:"command"`
	Env                map[string]string `json:"env"`
	MutationsEnabled   bool              `json:"mutations_enabled"`
	PrivateFallbacks   bool              `json:"private_fallbacks"`
	CapabilityRequired bool              `json:"capability_required"`
	DryRun             bool              `json:"dry_run"`
	validated          bool
}

// MarshalJSON makes capability removal part of the live serialization path,
// including for defensively hand-constructed LaunchPlans.
func (p LaunchPlan) MarshalJSON() ([]byte, error) {
	return jsonMarshalNoCapability(p)
}

// BuildLaunchPlan constructs a dev-lifecycle launch plan.
func BuildLaunchPlan(consoleDirFlag string, privateFallbacks, dryRun bool) (LaunchPlan, error) {
	return BuildLifecycleLaunchPlan(LifecycleDev, consoleDirFlag, false, privateFallbacks, dryRun)
}

// BuildLifecycleLaunchPlan composes the operator posture, validates home +
// capability gates, resolves compound binary and console dir, and emits the
// LaunchPlan. It is the single authority for "how does the next console run?"
// and is the one caller of every gate in this package.
func BuildLifecycleLaunchPlan(lifecycle, consoleDirFlag string, mutations, privateFallbacks, dryRun bool) (LaunchPlan, error) {
	switch lifecycle {
	case LifecycleDev, LifecycleBuild, LifecycleServe:
	default:
		return LaunchPlan{}, fmt.Errorf("unknown console lifecycle %q", lifecycle)
	}
	if lifecycle == LifecycleBuild && mutations {
		return LaunchPlan{}, errors.New("console build cannot enable the runtime mutation extension")
	}
	if privateFallbacks {
		return LaunchPlan{}, errors.New("private fallbacks are unavailable; use a Compound-owned projection or CLI read surface")
	}
	if err := ValidateMutationLaunchEnvironment(mutations); err != nil {
		return LaunchPlan{}, err
	}
	consoleDir, err := ResolveDir(consoleDirFlag)
	if err != nil {
		return LaunchPlan{}, err
	}
	homePath, err := home.Resolve("")
	if err != nil {
		return LaunchPlan{}, fmt.Errorf("resolve COMPOUND_HOME: %w", err)
	}
	exePath, err := os.Executable()
	if err != nil {
		return LaunchPlan{}, fmt.Errorf("resolve compound executable: %w", err)
	}
	binary := ResolveCompoundBin(exePath, exec.LookPath)
	env := map[string]string{
		consoleCompoundExeKey:         binary,
		"COMPOUND_CONSOLE_BUILD_KIND": consoleBuildKindDev,
		MutationsEnv:                  "0",
		PrivateFallbacksEnv:           "0",
		"COMPOUND_HOME":               homePath,
	}
	if mutations {
		env[MutationsEnv] = "1"
	}
	command := []string{"bun", "run", "next", "dev", "-H", listenHost, "-p", listenPort}
	switch lifecycle {
	case LifecycleBuild:
		env["COMPOUND_CONSOLE_BUILD_KIND"] = consoleBuildKindProd
		command = []string{"bun", "run", "next", "build"}
	case LifecycleServe:
		env["COMPOUND_CONSOLE_BUILD_KIND"] = consoleBuildKindProd
		command = []string{"bun", "run", "next", "start", "-H", listenHost, "-p", listenPort}
	}
	return LaunchPlan{
		SchemaVersion:      LaunchPlanSchemaVersion,
		GeneratedAt:        time.Now().UTC().Format(time.RFC3339),
		Mode:               "operator",
		Lifecycle:          lifecycle,
		ConsoleDir:         consoleDir,
		Command:            command,
		Env:                env,
		MutationsEnabled:   mutations,
		PrivateFallbacks:   false,
		CapabilityRequired: mutations,
		DryRun:             dryRun,
		validated:          true,
	}, nil
}

// ValidateMutationLaunchEnvironment enforces the runtime capability gate.
//
//   - When `mutations` is false, the gate is skipped.
//   - When `mutations` is true, an explicit COMPOUND_HOME is required.
//   - When `mutations` is true, CapabilityEnv must carry exactly 64 hexadecimal
//     characters. Other lengths and non-hex bytes are rejected.
func ValidateMutationLaunchEnvironment(mutations bool) error {
	if !mutations {
		return nil
	}
	if strings.TrimSpace(os.Getenv("COMPOUND_HOME")) == "" {
		return errors.New("console mutation extension requires an explicit COMPOUND_HOME selected by the operator")
	}
	capability := os.Getenv(CapabilityEnv)
	if len(capability) != 64 {
		return fmt.Errorf("console mutation extension requires %s encoded as exactly 64 hexadecimal characters; generate it out of band", CapabilityEnv)
	}
	if _, err := hex.DecodeString(capability); err != nil {
		return fmt.Errorf("console mutation extension requires %s encoded as exactly 64 hexadecimal characters; generate it out of band", CapabilityEnv)
	}
	return nil
}

// ResolveCompoundBin returns a stable compound binary path, preferring PATH-
// resolved binaries over a temporary or go-build executable path so child
// launches survive smoke binaries without inheriting a build artifact.
func ResolveCompoundBin(currentExe string, lookPath func(string) (string, error)) string {
	currentExe = strings.TrimSpace(currentExe)
	if currentExe == "" || ExecutableIsTemporary(currentExe) {
		if stable, err := lookPath("compound"); err == nil && strings.TrimSpace(stable) != "" {
			return stable
		}
	}
	if currentExe != "" {
		return currentExe
	}
	if stable, err := lookPath("compound"); err == nil && strings.TrimSpace(stable) != "" {
		return stable
	}
	return "compound"
}

// ExecutableIsTemporary reports whether `path` looks like a smoke binary built
// by `go run`, `go test`, or a Bun-compile-dashboard. Stable PATH-resolved
// binaries return false.
func ExecutableIsTemporary(path string) bool {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	if cleaned == "." || cleaned == "" {
		return false
	}
	tempRoots := []string{filepath.Clean(os.TempDir()), string(filepath.Separator) + "tmp"}
	for _, root := range tempRoots {
		if cleaned == root || strings.HasPrefix(cleaned, root+string(filepath.Separator)) {
			return true
		}
	}
	base := filepath.Base(cleaned)
	return strings.Contains(cleaned, string(filepath.Separator)+"go-build") ||
		strings.HasPrefix(base, "compound-dashboard")
}

// ResolveDir honors an explicit --console-dir, then COMPOUND_CONSOLE_DIR, then
// walks parent directories looking for `console/package.json`. Returns the
// first match or a refusal.
func ResolveDir(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return ValidateDir(explicit)
	}
	if fromEnv := strings.TrimSpace(os.Getenv("COMPOUND_CONSOLE_DIR")); fromEnv != "" {
		return ValidateDir(fromEnv)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if candidate, err := ValidateDir(filepath.Join(dir, "console")); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return "", errors.New("console directory not found; run from the compound repo or pass --console-dir")
}

// ValidateDir absolute-resolves `dir` and confirms it contains
// console/package.json. Errors if not.
func ValidateDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(filepath.Join(abs, "package.json"))
	if err != nil {
		return "", fmt.Errorf("console package.json not found at %s: %w", abs, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("console package.json is a directory at %s", abs)
	}
	return abs, nil
}

// HomeShortHash returns the first seven lowercase hex chars of sha256(home).
func HomeShortHash(homePath string) string {
	sum := sha256.Sum256([]byte(homePath))
	return hex.EncodeToString(sum[:])[:7]
}

// InstancePinLabel renders "<leaf> #<hash> (<abs>)" or "(unpinned)" if empty.
func InstancePinLabel(homePath string) string {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return "(unpinned)"
	}
	return fmt.Sprintf("%s #%s (%s)", filepath.Base(homePath), HomeShortHash(homePath), homePath)
}

// ValidateProductionBuild enforces BUILD_ID + manifest presence in a
// production console dir. It is the build-lifecycle gate.
//
// The on-disk layout matches what `next build` emits under the custom
// production distDir declared in console/next.config.ts (`.next/production`).
// This is the SAME tree `next start` reads from when serve runs the
// production server, so the gate cannot point at a different path or it
// will refuse a tree the serve step then happily boots from — a crash
// loop the launchd console service hit once before this contract was
// pinned (commit be911fd6 originally looked at `.next/standalone`, which
// is only produced when the standalone output feature is enabled and
// otherwise never exists).
func ValidateProductionBuild(consoleDir string) error {
	productionDir := filepath.Join(consoleDir, ".next", "production")
	buildID, err := os.ReadFile(filepath.Join(productionDir, "BUILD_ID"))
	if err != nil || strings.TrimSpace(string(buildID)) == "" {
		return fmt.Errorf("production BUILD_ID is missing; run compound console build --operator --console-dir %s", consoleDir)
	}
	for _, name := range []string{"prerender-manifest.json", "required-server-files.json", "routes-manifest.json"} {
		info, err := os.Stat(filepath.Join(productionDir, name))
		if err != nil || info.IsDir() {
			return fmt.Errorf("production %s is missing; run compound console build --operator --console-dir %s", name, consoleDir)
		}
	}
	return nil
}

// LaunchEnv renders a child process environment from a plan produced by
// BuildLifecycleLaunchPlan. Unvalidated plans fail closed to projection-only.
// Ambient capability, session, mutation, and private-fallback values are
// stripped before the exact validated posture is added.
func LaunchEnv(plan LaunchPlan) []string {
	blockedAmbient := map[string]struct{}{
		CapabilityEnv:       {},
		LegacySessionEnv:    {},
		MutationsEnv:        {},
		PrivateFallbacksEnv: {},
	}
	env := make([]string, 0, len(os.Environ())+len(plan.Env)+3)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, blocked := blockedAmbient[key]; blocked {
			continue
		}
		env = append(env, entry)
	}
	upsert := func(key, value string) {
		prefix := key + "="
		for i, entry := range env {
			if strings.HasPrefix(entry, prefix) {
				env[i] = prefix + value
				return
			}
		}
		env = append(env, prefix+value)
	}
	for key, value := range plan.Env {
		if key == CapabilityEnv {
			continue
		}
		upsert(key, value)
	}
	mutationExtension := plan.validated && plan.MutationsEnabled && (plan.Lifecycle == LifecycleDev || plan.Lifecycle == LifecycleServe)
	if mutationExtension {
		upsert(MutationsEnv, "1")
		if plan.CapabilityRequired {
			upsert(CapabilityEnv, os.Getenv(CapabilityEnv))
		}
	} else {
		upsert(MutationsEnv, "0")
	}
	upsert(PrivateFallbacksEnv, "0")
	return env
}

// jsonMarshalNoCapability is the implementation behind LaunchPlan.MarshalJSON.
func jsonMarshalNoCapability(p LaunchPlan) ([]byte, error) {
	type envAlias struct {
		SchemaVersion      string            `json:"schema_version"`
		GeneratedAt        string            `json:"generated_at"`
		Mode               string            `json:"mode"`
		Lifecycle          string            `json:"lifecycle"`
		ConsoleDir         string            `json:"console_dir"`
		Command            []string          `json:"command"`
		Env                map[string]string `json:"env"`
		MutationsEnabled   bool              `json:"mutations_enabled"`
		PrivateFallbacks   bool              `json:"private_fallbacks"`
		CapabilityRequired bool              `json:"capability_required"`
		DryRun             bool              `json:"dry_run"`
	}
	env := make(map[string]string, len(p.Env))
	for k, v := range p.Env {
		if k == CapabilityEnv {
			continue
		}
		env[k] = v
	}
	return json.Marshal(envAlias{
		SchemaVersion:      p.SchemaVersion,
		GeneratedAt:        p.GeneratedAt,
		Mode:               p.Mode,
		Lifecycle:          p.Lifecycle,
		ConsoleDir:         p.ConsoleDir,
		Command:            p.Command,
		Env:                env,
		MutationsEnabled:   p.MutationsEnabled,
		PrivateFallbacks:   p.PrivateFallbacks,
		CapabilityRequired: p.CapabilityRequired,
		DryRun:             p.DryRun,
	})
}
