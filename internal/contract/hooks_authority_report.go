package contract

import (
	"compound/internal/harness"
	"compound/internal/hookplan"
)

// HooksAuthorityReport is the projection `compound contract hooks-authority`
// serves. It lives with the authority that produces it rather than in the CLI:
// every field is a contract or hookplan type, so the CLI's only remaining job
// is to fill it and print it.
type HooksAuthorityReport struct {
	Privacy                     string                          `json:"privacy"`
	AuthorityRef                string                          `json:"authority_ref"`
	AuthorityPath               string                          `json:"authority_path"`
	Installed                   bool                            `json:"installed"`
	HookCount                   int                             `json:"hook_count,omitempty"`
	Harnesses                   []string                        `json:"harnesses,omitempty"`
	PreToolUseReadiness         []HookGateReadiness             `json:"pre_tool_use_readiness,omitempty"`
	HookReadiness               []InstalledHookRuntimeReadiness `json:"hook_readiness,omitempty"`
	HookPlan                    *hookplan.HookPlan              `json:"hook_plan,omitempty"`
	InstallationObservation     *hookplan.InstallationSnapshot  `json:"installation_observation,omitempty"`
	Reconciliation              *hookplan.Reconciliation        `json:"reconciliation,omitempty"`
	SourceRoleCounts            map[string]int                  `json:"source_role_counts,omitempty"`
	RetiredSourceAuthorityCount int                             `json:"retired_source_authority_count"`
	RetiredSourceAuthorityRefs  []string                        `json:"retired_source_authority_refs,omitempty"`
	SourceAuthority             []InstalledHooksSourceAuthority `json:"source_authority,omitempty"`
	SourceRecordPath            string                          `json:"source_record_path"`
}

// PopulateLiveHookReconciliation fills the live-observation fields by
// reconciling the installed hooks against the harness registry.
//
// harnessHome is a parameter rather than an os.UserHomeDir() call inside this
// function: reading the environment is the composition root's job, so the organ
// stays injectable and testable against a fixture home.
func PopulateLiveHookReconciliation(
	report *HooksAuthorityReport,
	compoundHome string,
	harnessHome string,
) error {
	state, err := ReconcileInstalledHooks(compoundHome, harnessHome, harness.DefaultRegistry())
	if err != nil {
		return err
	}
	report.HookPlan = &state.Plan
	report.InstallationObservation = &state.Observation
	report.Reconciliation = &state.Reconciliation
	return nil
}
