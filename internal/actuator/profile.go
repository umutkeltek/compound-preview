package actuator

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/envelope"
)

// ProfileSchemaVersion identifies a ProjectProfile on disk / in a request.
const ProfileSchemaVersion = "compound.actuator_project_profile.v0.1"

// RoutingSpec declares how execution for a project is routed to a harness, and
// the single config key the switchback repoints to select it. This is the
// "switchback egress alias" internal/act's ConfigKeyAdapter was built for: a
// reversible, pre-image-captured repoint of one key — never a rewrite of the
// harness's whole config. ConfigPath/ConfigKey empty means "no routing repoint
// needed" (the harness is addressed directly by name).
type RoutingSpec struct {
	Harness     string `json:"harness" yaml:"harness"`
	Model       string `json:"model,omitempty" yaml:"model,omitempty"`
	ConfigPath  string `json:"config_path,omitempty" yaml:"config_path,omitempty"`
	ConfigKey   string `json:"config_key,omitempty" yaml:"config_key,omitempty"`
	ConfigValue string `json:"config_value,omitempty" yaml:"config_value,omitempty"`
}

// InstructionSurface is one agent-facing instruction file (AGENTS.md, CLAUDE.md)
// the project presents to its harness. Compound both materializes the body into
// the ContextPack (presentation) and writes it into the workspace (so the
// harness's native discovery finds it); hooks then audit the agent against it.
type InstructionSurface struct {
	Name string `json:"name" yaml:"name"` // e.g. "AGENTS.md", "CLAUDE.md"
	Path string `json:"path" yaml:"path"` // target path relative to the workspace
	Body string `json:"body" yaml:"body"` // content to materialize
}

// SettingsSurface is one harness settings target the project reconciles (the
// surgical permissions/settings ownership internal/harness already implements).
// The actuator does NOT reimplement that reconciler — it surfaces the exact
// authority to apply through the existing governed entry point.
type SettingsSurface struct {
	Harness    string         `json:"harness" yaml:"harness"`
	TargetPath string         `json:"target_path" yaml:"target_path"`
	Settings   map[string]any `json:"settings,omitempty" yaml:"settings,omitempty"`
}

// ProjectProfile is the per-project switching bundle: everything the actuator
// needs to make working ON this project look different from any other — its
// harness routing, its instruction files, its settings, its budget — applied as
// one smooth switchback. It is the "adapt based on what project I am working
// with" object. Envelope-shaped (schema_version + id + created_at), following
// the BudgetEnvelope / product.LoopModel pattern: own schema + Validate(), not a
// registry entry.
type ProjectProfile struct {
	SchemaVersion string               `json:"schema_version" yaml:"schema_version"`
	ID            string               `json:"id" yaml:"id"`
	ProductID     string               `json:"product_id,omitempty" yaml:"product_id,omitempty"`
	Slug          string               `json:"slug" yaml:"slug"`
	Workspace     string               `json:"workspace" yaml:"workspace"`
	Routing       RoutingSpec          `json:"routing" yaml:"routing"`
	Instructions  []InstructionSurface `json:"instructions,omitempty" yaml:"instructions,omitempty"`
	Settings      []SettingsSurface    `json:"settings,omitempty" yaml:"settings,omitempty"`
	CreatedAt     string               `json:"created_at" yaml:"created_at"`
}

// Validate enforces the profile's invariants. A profile must name a workspace
// (switchback writes are scoped to it, never COMPOUND_HOME) and a harness;
// instruction surfaces must have a name, a path, and a body.
func (p ProjectProfile) Validate() error {
	if p.SchemaVersion != ProfileSchemaVersion {
		return fmt.Errorf("actuator profile schema_version = %q, want %q", p.SchemaVersion, ProfileSchemaVersion)
	}
	if strings.TrimSpace(p.ID) == "" {
		return fmt.Errorf("actuator profile id required")
	}
	if strings.TrimSpace(p.Slug) == "" {
		return fmt.Errorf("actuator profile %s: slug required", p.ID)
	}
	if strings.TrimSpace(p.Workspace) == "" {
		return fmt.Errorf("actuator profile %s: workspace required (switchback is workspace-scoped)", p.ID)
	}
	if strings.TrimSpace(p.Routing.Harness) == "" {
		return fmt.Errorf("actuator profile %s: routing.harness required", p.ID)
	}
	seen := map[string]bool{}
	for _, ins := range p.Instructions {
		if strings.TrimSpace(ins.Name) == "" || strings.TrimSpace(ins.Path) == "" {
			return fmt.Errorf("actuator profile %s: instruction surface needs name + path", p.ID)
		}
		if strings.TrimSpace(ins.Body) == "" {
			return fmt.Errorf("actuator profile %s: instruction %q has empty body", p.ID, ins.Name)
		}
		if seen[ins.Path] {
			return fmt.Errorf("actuator profile %s: duplicate instruction path %q", p.ID, ins.Path)
		}
		seen[ins.Path] = true
		if isUnsafeInstructionPath(ins.Path) {
			return fmt.Errorf("actuator profile %s: instruction path %q escapes the workspace or targets a sensitive file", p.ID, ins.Path)
		}
	}
	return nil
}

// isUnsafeInstructionPath guards the instruction write surface: the path must
// be workspace-relative (no leading "/", no ".." traversal) and must not be a
// sensitive dotfile. The write is scoped to the workspace by the caller, but
// the profile itself refuses escape-shaped paths so a bad profile can't aim a
// write outside the project.
func isUnsafeInstructionPath(path string) bool {
	p := strings.TrimSpace(path)
	if p == "" || strings.HasPrefix(p, "/") {
		return true
	}
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part == ".." {
			return true
		}
	}
	base := strings.ToLower(filepath.Base(p))
	switch base {
	case ".env", ".gitconfig", "id_rsa", "authorized_keys":
		return true
	}
	return strings.HasSuffix(base, ".key") || strings.Contains(base, "secret")
}

// SwitchbackStepKind enumerates the four things a switchback applies.
const (
	StepInstructions = "instructions"
	StepRouting      = "routing"
	StepSettings     = "settings"
	StepBudget       = "budget"
)

// SwitchbackStep is one ordered unit of a switchback. Instruction + routing
// steps are applied by the actuator directly (local_reversible, policy-gated).
// Settings steps are surfaced as the exact governed command to run, because the
// harness permission reconciler already owns that write and has its own
// reserve/finalize protocol the actuator will not reimplement.
type SwitchbackStep struct {
	Kind        string                 `json:"kind" yaml:"kind"`
	Description string                 `json:"description" yaml:"description"`
	Intent      *envelope.ActionIntent `json:"intent,omitempty" yaml:"intent,omitempty"`
	NewValue    string                 `json:"new_value,omitempty" yaml:"new_value,omitempty"`
	FilePath    string                 `json:"file_path,omitempty" yaml:"file_path,omitempty"`
	Body        string                 `json:"body,omitempty" yaml:"body,omitempty"`
	ApplyHint   string                 `json:"apply_hint,omitempty" yaml:"apply_hint,omitempty"`
}

// SwitchbackPlan expands a profile into its ordered steps for a given work.
// Order matters for smoothness: instructions land first (so the harness's next
// session sees them), then routing is repointed, then settings + budget are
// surfaced. Every step is scoped to the work (intents carry ids.work_id) so a
// switchback is never an unmoored global mutation.
func SwitchbackPlan(p ProjectProfile, workID string, now time.Time) ([]SwitchbackStep, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(workID) == "" {
		return nil, fmt.Errorf("actuator switchback: work_id required (a switchback belongs to known work)")
	}
	var steps []SwitchbackStep

	// 1. Instruction surfaces — write AGENTS.md / CLAUDE.md into the workspace.
	for _, ins := range p.Instructions {
		target := ins.Path
		if !filepath.IsAbs(target) {
			target = filepath.Join(p.Workspace, ins.Path)
		}
		steps = append(steps, SwitchbackStep{
			Kind:        StepInstructions,
			Description: fmt.Sprintf("materialize %s into the %s workspace", ins.Name, p.Slug),
			FilePath:    target,
			Body:        ins.Body,
		})
	}

	// 2. Routing — repoint the single egress/routing config key, if declared.
	if p.Routing.ConfigPath != "" && p.Routing.ConfigKey != "" && p.Routing.ConfigValue != "" {
		steps = append(steps, SwitchbackStep{
			Kind:        StepRouting,
			Description: fmt.Sprintf("route %s execution to %s (repoint %s in %s)", p.Slug, p.Routing.Harness, p.Routing.ConfigKey, p.Routing.ConfigPath),
			Intent:      routingIntent(p, workID, now),
			NewValue:    p.Routing.ConfigValue,
		})
	}

	// 3. Settings — surface the governed harness-settings apply (not re-implemented).
	for _, s := range p.Settings {
		steps = append(steps, SwitchbackStep{
			Kind:        StepSettings,
			Description: fmt.Sprintf("reconcile %s settings for %s at %s", s.Harness, p.Slug, s.TargetPath),
			ApplyHint:   fmt.Sprintf("compound harness permissions apply --harness %s --target %s", s.Harness, s.TargetPath),
		})
	}

	// 4. Budget — surface the product budget the switchback executes within.
	steps = append(steps, SwitchbackStep{
		Kind:        StepBudget,
		Description: fmt.Sprintf("execute %s within its installed budget (channel limits enforced before dispatch)", p.Slug),
		ApplyHint:   fmt.Sprintf("compound actuator budget --product %s", firstNonEmpty(p.ProductID, p.Slug)),
	})

	return steps, nil
}

// routingIntent builds the local_reversible / easy / local ActionIntent for the
// switchback's routing repoint. local_reversible is honest here: act.Execute
// captures the key's pre-image and rollback is a re-write of it. Under the
// default policy local_reversible resolves to allow_with_obligations, so the
// switchback flows smoothly — while an operator who tightens local_reversible
// (or installs a placement/standing-grant guard) still holds it. Smooth within
// limits, never smooth past them.
func routingIntent(p ProjectProfile, workID string, now time.Time) *envelope.ActionIntent {
	target := envelope.ActionTarget{Kind: "config_key", Path: p.Routing.ConfigPath, ID: p.Routing.ConfigKey}
	return &envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            intentIDForInvocation(now, workID, "switchback_route", target.Path, target.ID, p.Routing.ConfigValue),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "actuator", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Scope:         envelope.Scope{ProjectID: p.Slug, ProductID: p.ProductID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "actuator",
		Capability:    "switchback_route",
		Action:        "repoint_config_key",
		Target:        target,
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       fmt.Sprintf("route %s to %s via %s=%q", p.Slug, p.Routing.Harness, p.Routing.ConfigKey, p.Routing.ConfigValue),
		RollbackPlan:  fmt.Sprintf("restore the captured pre-image of %s in %s", p.Routing.ConfigKey, p.Routing.ConfigPath),
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
