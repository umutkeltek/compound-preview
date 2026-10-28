package runattempt

import (
	"errors"
	"fmt"
	"strings"
)

// ExecutionSelectionSnapshot is the ATTEMPT-LEVEL record of what this launch
// was asked to run and what the adapter could actually apply. It is stamped
// once, before the subprocess starts, and never rewritten: a later change of
// the work's saved selection, a renamed preset alias, or an edited global
// default must not change what a historical attempt says it ran.
//
// Requested and applied values are kept apart on purpose. "high" as a label
// passed through a wrapper is not the same fact as "--thinking=high" on the
// argv the harness accepted, and neither is the same fact as a provider
// receipt. EffortEvidence names which of those this record can honestly claim.
type ExecutionSelectionSnapshot struct {
	// Source is where the selection came from for THIS run
	// (ExecutionSnapshotSources): override | saved | project_default | system_default.
	Source string `json:"source"`
	// SelectionRevision is the work-level ExecutionSelection.Revision consumed
	// when Source == saved; zero otherwise.
	SelectionRevision int `json:"selection_revision,omitempty"`
	// LaunchProfileID / LaunchProfileRevision identify the owner profile and the
	// revision observed at launch (may differ from the saved one — see
	// SelectionDrift).
	LaunchProfileID       string `json:"launch_profile_id,omitempty"`
	LaunchProfileRevision string `json:"launch_profile_revision,omitempty"`
	// SelectionDrift is non-empty when the launched profile revision differs
	// from the one the work saved: "launch_profile_revision saved=sha256:… launched=sha256:…".
	// Recording it is what lets a reader see WHY a run differed from the plan.
	SelectionDrift string `json:"selection_drift,omitempty"`
	// Harness is the Compound harness family; HarnessVersion is the observed
	// executable version at launch (e.g. "2.1.268", "18.1.17"), empty when the
	// adapter could not observe it.
	Harness        string `json:"harness"`
	HarnessVersion string `json:"harness_version,omitempty"`
	// ProviderRoute and RequestedModelID are the requested route and resolved
	// model id (not an alias). Observed identity stays on ModelID/ProviderRoute
	// of the RunAttempt and is attested only by route evidence.
	ProviderRoute    string `json:"provider_route,omitempty"`
	RequestedModelID string `json:"requested_model_id,omitempty"`
	// RequestedEffort is the semantic effort asked for; AppliedEffort is the
	// harness-native value the adapter actually passed (may be a translation);
	// EffortEvidence says how AppliedEffort is known (ExecutionEffortEvidencePrefixes).
	RequestedEffort string `json:"requested_effort,omitempty"`
	AppliedEffort   string `json:"applied_effort,omitempty"`
	EffortEvidence  string `json:"effort_evidence,omitempty"`
	// SettingsDigest is sha256 of the harness settings snapshot the launch read
	// (permissions, hooks, env names). Credentials are REFERENCED by
	// CredentialRef ("env:NAME"), never copied.
	SettingsDigest string `json:"settings_digest,omitempty"`
	CredentialRef  string `json:"credential_ref,omitempty"`
	// NativeSessionID is the harness's own session identity for this attempt;
	// NativeSessionEvidence says how it is known (ExecutionSessionEvidencePrefixes).
	NativeSessionID       string `json:"native_session_id,omitempty"`
	NativeSessionEvidence string `json:"native_session_evidence,omitempty"`
	// ContinuationOf names what this launch continued: a predecessor ratt_* id,
	// or "native:<session-id>" when a native resume was requested. Empty for a
	// fresh launch. Cross-harness continuation keeps the Compound lineage here
	// and never pretends the native session format carried over.
	ContinuationOf string `json:"continuation_of,omitempty"`
}

// ExecutionSnapshotSources are the legal per-run selection sources.
var ExecutionSnapshotSources = []string{"override", "saved", "project_default", "system_default"}

// ExecutionEffortEvidencePrefixes are the honest ways an applied effort can be known:
//   - argv:<flag>         the adapter placed the value on the process argv (e.g. argv:--thinking=high)
//   - env:<VAR>           the value travelled as an environment variable the harness reads
//   - settings:<path>     the value is pinned in the settings file the launch consumed
//   - receipt:<ref>       a provider/route receipt attests the effective value
//   - unsupported:<why>   the harness has no effort control; AppliedEffort MUST be empty
//   - unobservable:<why>  a value was requested but nothing proves it was applied
var ExecutionEffortEvidencePrefixes = []string{"argv:", "env:", "settings:", "receipt:", "unsupported:", "unobservable:"}

// ExecutionSessionEvidencePrefixes are the honest ways a native session id can be known:
//   - pinned:<flag>       the launch pinned the id (e.g. pinned:--session-id)
//   - derived:<path>      the id was read back from the harness's own session store
//   - unsupported:<why>  the launch shape cannot produce a session (e.g. --no-session)
//   - unobservable:<why> a session may exist but the adapter did not capture its id
var ExecutionSessionEvidencePrefixes = []string{"pinned:", "derived:", "unsupported:", "unobservable:"}

var errExecutionSelectionSnapshotSet = errors.New("execution selection snapshot already set with different content")

func hasAnyPrefix(value string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(value, p) && len(value) > len(p) {
			return true
		}
	}
	return false
}

// Validate refuses a snapshot that would let a reader over-claim.
func (s ExecutionSelectionSnapshot) Validate() error {
	var errs []string
	if !contains(ExecutionSnapshotSources, s.Source) {
		errs = append(errs, fmt.Sprintf("source must be one of %v, got %q", ExecutionSnapshotSources, s.Source))
	}
	if s.Source == "saved" && s.SelectionRevision < 1 {
		errs = append(errs, "selection_revision must be >= 1 when source is saved")
	}
	if s.Source != "saved" && s.SelectionRevision != 0 {
		errs = append(errs, "selection_revision must be 0 unless source is saved")
	}
	if strings.TrimSpace(s.Harness) == "" {
		errs = append(errs, "harness is required")
	}
	if s.RequestedEffort != "" || s.AppliedEffort != "" || s.EffortEvidence != "" {
		if !hasAnyPrefix(s.EffortEvidence, ExecutionEffortEvidencePrefixes) {
			errs = append(errs, fmt.Sprintf("effort_evidence must carry one of %v with detail, got %q", ExecutionEffortEvidencePrefixes, s.EffortEvidence))
		}
		if strings.HasPrefix(s.EffortEvidence, "unsupported:") && s.AppliedEffort != "" {
			errs = append(errs, "applied_effort must be empty when effort_evidence is unsupported")
		}
		if strings.HasPrefix(s.EffortEvidence, "unobservable:") && s.AppliedEffort != "" {
			errs = append(errs, "applied_effort must be empty when effort_evidence is unobservable")
		}
	}
	if s.NativeSessionID != "" || s.NativeSessionEvidence != "" {
		if !hasAnyPrefix(s.NativeSessionEvidence, ExecutionSessionEvidencePrefixes) {
			errs = append(errs, fmt.Sprintf("native_session_evidence must carry one of %v with detail, got %q", ExecutionSessionEvidencePrefixes, s.NativeSessionEvidence))
		}
		if (strings.HasPrefix(s.NativeSessionEvidence, "unsupported:") || strings.HasPrefix(s.NativeSessionEvidence, "unobservable:")) && s.NativeSessionID != "" {
			errs = append(errs, "native_session_id must be empty when native_session_evidence is unsupported or unobservable")
		}
	}
	for _, rev := range []string{s.LaunchProfileRevision} {
		if rev != "" && !strings.HasPrefix(rev, "sha256:") {
			errs = append(errs, fmt.Sprintf("launch_profile_revision must be sha256:<hex> when set, got %q", rev))
		}
	}
	if s.SettingsDigest != "" && !strings.HasPrefix(s.SettingsDigest, "sha256:") {
		errs = append(errs, fmt.Sprintf("settings_digest must be sha256:<hex> when set, got %q", s.SettingsDigest))
	}
	if s.CredentialRef != "" && !strings.HasPrefix(s.CredentialRef, "env:") {
		errs = append(errs, fmt.Sprintf("credential_ref must be a reference (env:NAME), got %q", s.CredentialRef))
	}
	if len(errs) > 0 {
		return errors.New("execution selection snapshot: " + strings.Join(errs, "; "))
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// SetExecutionSelectionSnapshot stamps the per-run selection snapshot. It
// refuses on a terminal attempt (like every S2/S3 setter), refuses an invalid
// snapshot, and is APPEND-ONLY: a second call with identical content is an
// idempotent no-op, a second call with different content is refused, so the
// historical record can never be rewritten to the latest configuration.
func (s *Store) SetExecutionSelectionSnapshot(id string, snap ExecutionSelectionSnapshot) (RunAttempt, error) {
	if err := snap.Validate(); err != nil {
		return RunAttempt{}, fmt.Errorf("runattempt %s: %w", id, err)
	}
	return s.mutateMutable(id, func(ra *RunAttempt) (bool, error) {
		if ra.ExecutionSelection != nil {
			if *ra.ExecutionSelection == snap {
				return false, nil
			}
			return false, fmt.Errorf("runattempt %s: %w", id, errExecutionSelectionSnapshotSet)
		}
		copy := snap
		ra.ExecutionSelection = &copy
		return true, nil
	})
}
