package authority

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	jobRosterProbeRefreshedVia = "jobprobe collect"
	jobRosterProbeRetirement   = "job status reads compound://registries/job-roster refreshed from observed probe state; declared probe targets under COMPOUND_HOME remain the observation source"
	jobRosterProbeRollback     = "remove COMPOUND_HOME registries/job-probe-targets.json to stop probe refresh, or re-import the source job registry to reset roster rows"
)

// JobRosterObservation is one CURRENT-state liveness observation for a roster
// row, produced by a probe-refresh collector. It carries no probe mechanics
// (launchctl/crontab/remote details stay in the collector); authority only
// consumes the observed liveness verdict so the roster is the one owner of the
// refreshed state. A row is matched by JobID first, then Name.
type JobRosterObservation struct {
	JobID      string `json:"job_id,omitempty"`
	Name       string `json:"name,omitempty"`
	Alive      bool   `json:"alive"`
	Detail     string `json:"detail,omitempty"`
	ObservedAt string `json:"observed_at,omitempty"`
	Source     string `json:"source,omitempty"`
}

// JobRosterRefreshReport is the receipt of one probe-refresh pass over the
// job-roster authority.
type JobRosterRefreshReport struct {
	AuthorityRef          string   `json:"authority_ref"`
	AuthorityPath         string   `json:"authority_path"`
	GeneratedAt           string   `json:"generated_at"`
	SourceLabel           string   `json:"source_label,omitempty"`
	ObservedCount         int      `json:"observed_count"`
	MatchedCount          int      `json:"matched_count"`
	DeadCount             int      `json:"dead_count"`
	AliveCount            int      `json:"alive_count"`
	FailureRefreshedCount int      `json:"failure_refreshed_count"`
	UnmatchedRefs         []string `json:"unmatched_refs,omitempty"`
	SourceAuthorityID     string   `json:"source_authority_id,omitempty"`
}

// RefreshJobRosterFromObservations re-observes the CURRENT state of roster rows
// and writes the observed liveness back onto the job-roster authority, so
// job_health director candidates derive from observed state instead of stale
// imported claims. The imported roster is a blind snapshot: a row can claim its
// unit is alive/ok while the real unit is dead. This surface closes that gap by
// letting a collector (compound/internal/jobprobe) feed observed verdicts here.
//
// The bridge is deliberately asymmetric: an observed-DEAD verdict overrides a
// stale alive/ok claim (LastStatus -> "error", LastError <- observed detail),
// which is exactly what makes a dead unit surface through BuildJobHealthPlans.
// An observed-ALIVE verdict only records probe metadata; it never downgrades an
// existing error, because a loaded/scheduled unit can still have a failed last
// run — liveness of the unit and success of the last run are different axes,
// and the probe only re-observes the former. Rows without a matching
// observation are left untouched (their imported claim stands until a probe
// covers them). authority remains the sole roster owner; the collector owns no
// authority object.
func RefreshJobRosterFromObservations(homePath, sourceLabel string, observations []JobRosterObservation, now time.Time) (JobRosterRefreshReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return JobRosterRefreshReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	roster, err := LoadJobRosterAuthority(homePath)
	if err != nil {
		return JobRosterRefreshReport{}, err
	}
	byKey := make(map[string]int, len(roster.Jobs)*2)
	for i, job := range roster.Jobs {
		if strings.TrimSpace(job.JobID) != "" {
			byKey["id:"+job.JobID] = i
		}
		if strings.TrimSpace(job.Name) != "" {
			byKey["name:"+job.Name] = i
		}
	}
	report := JobRosterRefreshReport{
		AuthorityRef:  roster.AuthorityRef,
		AuthorityPath: JobRosterAuthorityPath(homePath),
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		SourceLabel:   strings.TrimSpace(sourceLabel),
		ObservedCount: len(observations),
	}
	for _, obs := range observations {
		idx, ok := -1, false
		if id := strings.TrimSpace(obs.JobID); id != "" {
			idx, ok = byKey["id:"+id]
		}
		if !ok {
			if name := strings.TrimSpace(obs.Name); name != "" {
				idx, ok = byKey["name:"+name]
			}
		}
		if !ok {
			report.UnmatchedRefs = append(report.UnmatchedRefs, unmatchedObservationRef(obs))
			continue
		}
		report.MatchedCount++
		observedAt := strings.TrimSpace(obs.ObservedAt)
		if observedAt == "" {
			observedAt = now.UTC().Format(time.RFC3339)
		}
		job := &roster.Jobs[idx]
		job.ProbeObservedAt = observedAt
		job.ProbeDetail = strings.TrimSpace(obs.Detail)
		if obs.Alive {
			report.AliveCount++
			// Clear a probe-derived error: if we previously observed THIS row
			// dead (ProbeState was "dead" from a prior probe pass), the
			// LastStatus=error / LastError claim came from us, and a fresh
			// alive observation now retracts it. Otherwise recovered jobs
			// would stay red forever and inflate the operator's needs_you
			// backlog with error claims the probe itself wrote. The existing
			// TestRefreshAliveDoesNotDowngradeExistingError pins the
			// asymmetric bridge for IMPORTED errors (ProbeState was empty, so
			// an alive unit can still have a failing last run — liveness and
			// run success are distinct axes).
			if job.ProbeState == "dead" {
				job.LastStatus = ""
				job.LastError = ""
			}
			job.ProbeState = "alive"
			continue
		}
		report.DeadCount++
		job.ProbeState = "dead"
		if job.LastStatus != "error" {
			report.FailureRefreshedCount++
		}
		job.LastStatus = "error"
		if detail := strings.TrimSpace(obs.Detail); detail != "" {
			job.LastError = detail
		} else if strings.TrimSpace(job.LastError) == "" {
			job.LastError = "probe observed the unit is not alive"
		}
	}
	sort.Strings(report.UnmatchedRefs)

	roster.GeneratedAt = now.UTC().Format(time.RFC3339)
	roster.ProbeRefreshedAt = now.UTC().Format(time.RFC3339)
	normalizeJobRosterDerivedFields(&roster, &now)

	authorityPath := JobRosterAuthorityPath(homePath)
	if err := os.MkdirAll(filepath.Dir(authorityPath), 0o755); err != nil {
		return JobRosterRefreshReport{}, err
	}
	body, err := json.MarshalIndent(roster, "", "  ")
	if err != nil {
		return JobRosterRefreshReport{}, err
	}
	if err := os.WriteFile(authorityPath, append(body, '\n'), 0o644); err != nil {
		return JobRosterRefreshReport{}, err
	}

	label := strings.TrimSpace(sourceLabel)
	if label == "" {
		label = "jobprobe:observations"
	}
	revision := "sha256:" + digestBytes(mustMarshalObservations(observations))
	rec := JobRosterSourceAuthorityRecord{
		SchemaVersion:       JobRosterSourceAuthoritySchema,
		ID:                  jobRosterSourceAuthorityID(label, revision),
		CreatedAt:           now.UTC().Format(time.RFC3339),
		SourcePath:          label,
		SourceRevision:      revision,
		ImportedVia:         jobRosterProbeRefreshedVia,
		AuthorityRef:        JobRosterAuthorityRef,
		AuthorityStatus:     "compound_primary",
		SourceRole:          "observed_state_refresh_input",
		Retires:             "blind_job_roster_snapshot:" + roster.SourcePath,
		RetirementCondition: jobRosterProbeRetirement,
		Rollback:            jobRosterProbeRollback,
		JobCount:            roster.JobCount,
		EnabledCount:        roster.EnabledCount,
		ErrorCount:          roster.ErrorCount,
		EvidenceRefs:        []string{revision, JobRosterAuthorityRef},
	}
	if err := appendJobRosterSourceAuthority(homePath, rec); err != nil {
		return JobRosterRefreshReport{}, err
	}
	report.SourceAuthorityID = rec.ID
	return report, nil
}

func unmatchedObservationRef(obs JobRosterObservation) string {
	key := strings.TrimSpace(obs.JobID)
	if key == "" {
		key = strings.TrimSpace(obs.Name)
	}
	if key == "" {
		key = "unknown"
	}
	return "unmatched_probe_observation:" + key
}

func mustMarshalObservations(observations []JobRosterObservation) []byte {
	body, err := json.Marshal(observations)
	if err != nil {
		return []byte(fmt.Sprintf("%v", observations))
	}
	return body
}
