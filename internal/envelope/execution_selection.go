package envelope

import "strings"

// ExecutionSelection is the WORK-LEVEL saved choice of what runs this work:
// the executor owner's launch profile (the stable lane identity), the Compound
// harness family, the provider route, the resolved model id, and the requested
// semantic effort. It answers "what will run next time this work is opened"
// and survives process exit, reopen, and unrelated global-default edits,
// because it is stored ON the envelope rather than derived from whatever the
// launcher, route plan, or preset currently says.
//
// It is a SELECTION, not evidence: what actually ran is recorded per attempt
// in runattempt.ExecutionSelectionSnapshot, which is immutable once set. A
// later change of this field records a new Revision and a lifecycle event;
// historical attempts keep the snapshot they were launched with.
//
// Precedence at launch (see docs/architecture/execution-plane.md, "Execution
// selection"): an explicit one-run override on the launch call wins for THAT
// run and is recorded on the attempt as source=override without touching this
// field; otherwise this saved selection; otherwise the project/profile default
// (route plan for the capability); otherwise the system default. Only an
// explicit save (a producer setting Source=explicit) replaces this field.
type ExecutionSelection struct {
	// LaunchProfileID is the executor owner's launch profile id
	// (e.g. "claude-minimax", "omp-minimax"): the stable lane identity.
	LaunchProfileID string `json:"launch_profile_id"`
	// LaunchProfileRevision is the owner revision observed when the selection
	// was saved (sha256:...). It is evidence of what the profile meant at
	// selection time, not a pin: a changed revision is reported at launch as a
	// settings change, never silently applied and never silently ignored.
	LaunchProfileRevision string `json:"launch_profile_revision,omitempty"`
	// Harness is the Compound harness family that drives the profile
	// (claude_code | oh_my_pi | codex | ...), never a display alias.
	Harness string `json:"harness"`
	// ProviderRoute is the provider route the profile binds (minimax/MiniMax-M3).
	ProviderRoute string `json:"provider_route,omitempty"`
	// RequestedModelID is the resolved model identifier (MiniMax-M3), never a
	// preset alias such as "opus" or "default".
	RequestedModelID string `json:"requested_model_id,omitempty"`
	// RequestedEffort is the semantic effort the work asks for (xhigh | high |
	// medium | low). Empty means "profile default". Whether a harness can
	// apply it is decided and recorded per attempt, never assumed here.
	RequestedEffort string `json:"requested_effort,omitempty"`
	// Source says where the saved selection came from (ExecutionSelectionSources).
	Source string `json:"source"`
	// Revision starts at 1 and increments on every explicit save, so a
	// concurrent or stale writer can be detected by compare-and-set.
	Revision int `json:"revision"`
	// SelectedBy is the actor that saved this selection; SelectedAt is RFC3339.
	SelectedBy string `json:"selected_by"`
	SelectedAt string `json:"selected_at"`
}

// ExecutionSelectionSources are the legal work-level selection sources.
//   - explicit: a producer (operator, orchestrator, CLI flag) saved it on purpose.
//   - project_default: copied from the project/profile default at creation so
//     the work keeps meaning even if the default later changes.
//   - system_default: copied from the system default for the same reason.
var ExecutionSelectionSources = []string{"explicit", "project_default", "system_default"}

func validateExecutionSelection(v *verr, sel ExecutionSelection) {
	v.req("execution_selection.launch_profile_id", sel.LaunchProfileID)
	v.req("execution_selection.harness", sel.Harness)
	v.enum("execution_selection.source", sel.Source, ExecutionSelectionSources, true)
	if sel.Revision < 1 {
		v.addf("execution_selection.revision must be >= 1, got %d", sel.Revision)
	}
	v.req("execution_selection.selected_by", sel.SelectedBy)
	v.req("execution_selection.selected_at", sel.SelectedAt)
	if strings.TrimSpace(sel.LaunchProfileRevision) != "" && !strings.HasPrefix(sel.LaunchProfileRevision, "sha256:") {
		v.addf("execution_selection.launch_profile_revision must be sha256:<hex> when set, got %q", sel.LaunchProfileRevision)
	}
}

// Validate runs the same field rules envelope.WorkEnvelope.Validate uses,
// on the selection alone. Callers (e.g. the work SaveExecutionSelection
// seam) use this to refuse an invalid selection BEFORE persisting; the
// envelope's own Validate will re-run these rules after the field lands
// on the envelope, so the two paths share one definition.
//
// work_r7a_execution_selection_20260911.
func (s ExecutionSelection) Validate() error {
	v := &verr{}
	validateExecutionSelection(v, s)
	return v.result("execution_selection")
}
