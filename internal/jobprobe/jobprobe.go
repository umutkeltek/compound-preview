// Package jobprobe is the job-roster probe-refresh collector. The imported
// job-roster (compound/internal/authority) is a BLIND snapshot: a row can claim
// its unit is alive/ok while the real launchd/cron unit is dead. This collector
// re-observes the CURRENT liveness of declared roster rows and re-imports the
// observed state through authority's refresh surface, so job_health director
// candidates derive from observed state, not stale imported claims.
//
// Probe target definitions live ONLY under COMPOUND_HOME (never hardcoded hosts
// or units in repo code): the collector reads a targets file the operator
// authors. Probes run through an injectable ProbeRunner so tests inject fake
// results and never touch the real system; the SystemProbeRunner shells out to
// launchctl/crontab for local units and to a declared command for remote hosts.
// The collector owns no authority object — authority stays the roster owner.
package jobprobe

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/authority"
)

const (
	// ProbeTargetsSchema versions the operator-authored targets file.
	ProbeTargetsSchema = "compound.job_probe_targets.v0.1"
	// CollectReportSchema versions the collector run receipt.
	CollectReportSchema = "compound.job_probe_collect_report.v0.1"

	probeTargetsFile = "job-probe-targets.json"

	// KindLocalLaunchd probes a local launchd unit by label via launchctl.
	KindLocalLaunchd = "local_launchd"
	// KindLocalCron probes a local crontab entry by substring match.
	KindLocalCron = "local_cron"
	// KindRemote probes a remote host via a declared probe command whose argv
	// comes ENTIRELY from the operator's private targets file (no host or unit
	// is ever hardcoded in repo code).
	KindRemote = "remote"
)

// ProbeTarget maps one roster row to a probe. It is loaded from a file under
// COMPOUND_HOME; the host/unit/command are the operator's private values, never
// repo constants. A row is matched by JobID first, then Name.
type ProbeTarget struct {
	JobID        string   `json:"job_id,omitempty"`
	Name         string   `json:"name,omitempty"`
	Kind         string   `json:"kind"`
	Unit         string   `json:"unit,omitempty"`          // launchd label (local_launchd)
	Match        string   `json:"match,omitempty"`         // crontab substring (local_cron)
	HostRef      string   `json:"host_ref,omitempty"`      // opaque host alias for legibility (remote)
	ProbeCommand []string `json:"probe_command,omitempty"` // declared remote probe argv (remote)
}

// ProbeTargetsFile is the operator-authored targets document.
type ProbeTargetsFile struct {
	SchemaVersion string        `json:"schema_version"`
	Targets       []ProbeTarget `json:"targets"`
}

// ProbeResult is one observed liveness verdict from a runner. A returned error
// (from ProbeRunner.Probe) means the probe itself could not run and is NOT
// treated as a dead unit; a runner that wants "unreachable == dead" must encode
// that as ProbeResult{Alive: false}.
type ProbeResult struct {
	Alive  bool
	Detail string
}

// ProbeRunner observes the current liveness of a declared target. Tests inject a
// fake so no real system is probed.
type ProbeRunner interface {
	Probe(target ProbeTarget, now time.Time) (ProbeResult, error)
}

// Observation is the per-target outcome recorded in the collect report.
type Observation struct {
	JobID  string `json:"job_id,omitempty"`
	Name   string `json:"name,omitempty"`
	Kind   string `json:"kind"`
	Alive  bool   `json:"alive"`
	Detail string `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
}

// CollectReport is the receipt of one collector run.
type CollectReport struct {
	SchemaVersion string                           `json:"schema_version"`
	GeneratedAt   string                           `json:"generated_at"`
	TargetsPath   string                           `json:"targets_path"`
	TargetCount   int                              `json:"target_count"`
	ProbedCount   int                              `json:"probed_count"`
	AliveCount    int                              `json:"alive_count"`
	DeadCount     int                              `json:"dead_count"`
	ErrorCount    int                              `json:"error_count"`
	Observations  []Observation                    `json:"observations,omitempty"`
	Refresh       authority.JobRosterRefreshReport `json:"refresh"`
	Warnings      []string                         `json:"warnings,omitempty"`
}

// ProbeTargetsPath is the COMPOUND_HOME path of the operator-authored targets
// file. Kept under registries/ next to the roster authority it refreshes.
func ProbeTargetsPath(homePath string) string {
	return filepath.Join(homePath, "registries", probeTargetsFile)
}

// LoadProbeTargets reads and parses the targets file. A missing file is returned
// as an os.IsNotExist error so callers can treat "no targets declared" as a
// clean no-op.
func LoadProbeTargets(homePath string) (ProbeTargetsFile, error) {
	data, err := os.ReadFile(ProbeTargetsPath(homePath))
	if err != nil {
		return ProbeTargetsFile{}, err
	}
	var f ProbeTargetsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return ProbeTargetsFile{}, fmt.Errorf("parse probe targets %s: %w", ProbeTargetsPath(homePath), err)
	}
	return f, nil
}

// Collect runs every declared probe through the injected runner, then re-imports
// the observed liveness onto the job-roster authority. A missing targets file is
// a clean no-op (warning, no refresh). A per-target validation or probe error is
// recorded as a warning and does NOT fabricate a failure; only real observed
// verdicts refresh rows.
func Collect(homePath string, runner ProbeRunner, now time.Time) (CollectReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return CollectReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if runner == nil {
		return CollectReport{}, fmt.Errorf("probe runner required")
	}
	report := CollectReport{
		SchemaVersion: CollectReportSchema,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		TargetsPath:   ProbeTargetsPath(homePath),
	}
	targets, err := LoadProbeTargets(homePath)
	if err != nil {
		if os.IsNotExist(err) {
			report.Warnings = append(report.Warnings, "no probe targets declared at "+report.TargetsPath)
			return report, nil
		}
		return CollectReport{}, err
	}
	report.TargetCount = len(targets.Targets)

	var observations []authority.JobRosterObservation
	for _, target := range targets.Targets {
		if err := validateTarget(target); err != nil {
			report.ErrorCount++
			report.Warnings = append(report.Warnings, err.Error())
			report.Observations = append(report.Observations, Observation{
				JobID: target.JobID, Name: target.Name, Kind: target.Kind, Error: err.Error(),
			})
			continue
		}
		result, perr := runner.Probe(target, now)
		obs := Observation{JobID: target.JobID, Name: target.Name, Kind: target.Kind}
		if perr != nil {
			report.ErrorCount++
			obs.Error = perr.Error()
			report.Warnings = append(report.Warnings, "probe "+targetLabel(target)+": "+perr.Error())
			report.Observations = append(report.Observations, obs)
			continue
		}
		report.ProbedCount++
		obs.Alive = result.Alive
		obs.Detail = result.Detail
		if result.Alive {
			report.AliveCount++
		} else {
			report.DeadCount++
		}
		report.Observations = append(report.Observations, obs)
		observations = append(observations, authority.JobRosterObservation{
			JobID:      target.JobID,
			Name:       target.Name,
			Alive:      result.Alive,
			Detail:     result.Detail,
			ObservedAt: now.UTC().Format(time.RFC3339),
			Source:     "probe:" + target.Kind,
		})
	}

	if len(observations) == 0 {
		report.Warnings = append(report.Warnings, "no observed verdicts to refresh")
		return report, nil
	}
	refresh, err := authority.RefreshJobRosterFromObservations(homePath, "jobprobe:"+probeTargetsFile, observations, now)
	if err != nil {
		return report, err
	}
	report.Refresh = refresh
	return report, nil
}

func validateTarget(target ProbeTarget) error {
	if strings.TrimSpace(target.JobID) == "" && strings.TrimSpace(target.Name) == "" {
		return fmt.Errorf("probe target %s missing job_id and name", targetLabel(target))
	}
	switch target.Kind {
	case KindLocalLaunchd:
		if strings.TrimSpace(target.Unit) == "" {
			return fmt.Errorf("probe target %s (local_launchd) missing unit", targetLabel(target))
		}
	case KindLocalCron:
		if strings.TrimSpace(target.Match) == "" {
			return fmt.Errorf("probe target %s (local_cron) missing match", targetLabel(target))
		}
	case KindRemote:
		if len(target.ProbeCommand) == 0 {
			return fmt.Errorf("probe target %s (remote) missing probe_command", targetLabel(target))
		}
	default:
		return fmt.Errorf("probe target %s has unknown kind %q", targetLabel(target), target.Kind)
	}
	return nil
}

func targetLabel(target ProbeTarget) string {
	if id := strings.TrimSpace(target.JobID); id != "" {
		return id
	}
	if name := strings.TrimSpace(target.Name); name != "" {
		return name
	}
	return "<unnamed>"
}
