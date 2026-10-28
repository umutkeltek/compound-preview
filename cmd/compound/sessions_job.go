package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"compound/internal/authority"
	"compound/internal/home"
)

const (
	sessionMiningRunReceiptSchemaVersion = "compound.sessions_mine_run_receipt.v0.1"
	sessionMiningJobStatusSchemaVersion  = "compound.sessions_mine_job_status.v0.1"
)

type sessionMiningRunReceipt struct {
	SchemaVersion     string                `json:"schema_version"`
	ID                string                `json:"id"`
	JobID             string                `json:"job_id"`
	StartedAt         string                `json:"started_at"`
	FinishedAt        string                `json:"finished_at"`
	DurationMS        int64                 `json:"duration_ms"`
	Status            string                `json:"status"`
	Error             string                `json:"error,omitempty"`
	ScanMode          string                `json:"scan_mode,omitempty"`
	EventsScanned     int                   `json:"events_scanned"`
	EventsRetained    int                   `json:"events_retained"`
	SessionsSeen      int                   `json:"sessions_seen"`
	Findings          int                   `json:"findings"`
	EffectiveSettings sessionMiningSettings `json:"effective_settings"`
	CheckpointPath    string                `json:"checkpoint_path"`
}

type sessionMiningJobStatus struct {
	SchemaVersion      string                   `json:"schema_version"`
	JobID              string                   `json:"job_id"`
	Status             string                   `json:"status"`
	Declared           bool                     `json:"declared"`
	RosterInstalled    bool                     `json:"roster_installed"`
	SchedulerObserved  bool                     `json:"scheduler_observed"`
	SchedulerState     string                   `json:"scheduler_state,omitempty"`
	SchedulerDetail    string                   `json:"scheduler_detail,omitempty"`
	EnabledClaim       bool                     `json:"enabled_claim"`
	RosterState        string                   `json:"roster_state,omitempty"`
	Schedule           string                   `json:"schedule,omitempty"`
	Command            string                   `json:"command,omitempty"`
	HomePath           string                   `json:"home_path"`
	Running            bool                     `json:"running"`
	CheckpointPresent  bool                     `json:"checkpoint_present"`
	CheckpointPath     string                   `json:"checkpoint_path"`
	CheckpointUpdated  string                   `json:"checkpoint_updated_at,omitempty"`
	CheckpointEvents   int                      `json:"checkpoint_events"`
	IncrementalReady   bool                     `json:"incremental_ready"`
	LastReceipt        *sessionMiningRunReceipt `json:"last_receipt,omitempty"`
	LastSuccessAt      string                   `json:"last_success_at,omitempty"`
	LastAttemptAt      string                   `json:"last_attempt_at,omitempty"`
	LastDurationMS     int64                    `json:"last_duration_ms,omitempty"`
	LastError          string                   `json:"last_error,omitempty"`
	RecommendedAction  string                   `json:"recommended_action"`
	EffectiveSettings  sessionMiningSettings    `json:"effective_settings"`
	RuntimeEvidenceRef string                   `json:"runtime_evidence_ref,omitempty"`
}

func sessionMiningReceiptDir(homePath, jobID string) string {
	return filepath.Join(homePath, "observed", "jobs", jobID, "runs")
}

func sessionMiningLatestReceiptPath(homePath, jobID string) string {
	return filepath.Join(homePath, "projections", "sessions-mine", jobID+"-latest-run.json")
}

func sessionMiningRunReceiptID(jobID string, started time.Time) string {
	sum := sha256.Sum256([]byte(jobID + "\x00" + started.UTC().Format(time.RFC3339Nano)))
	return "jobrun_" + hex.EncodeToString(sum[:])[:16]
}

func writeSessionMiningRunReceipt(homePath string, receipt sessionMiningRunReceipt) error {
	body, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	dir := sessionMiningReceiptDir(homePath, receipt.JobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, receipt.ID+".json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create session mining run receipt: %w", err)
	}
	writeErr := error(nil)
	if _, err := file.Write(body); err != nil {
		writeErr = err
	}
	if err := file.Close(); writeErr == nil {
		writeErr = err
	}
	if writeErr != nil {
		_ = os.Remove(path)
		return writeErr
	}
	if err := home.WriteFileAtomic(sessionMiningLatestReceiptPath(homePath, receipt.JobID), body, 0o600); err != nil {
		return fmt.Errorf("publish latest session mining receipt: %w", err)
	}
	return nil
}

func cmdSessionsJob(args []string) {
	if len(args) == 0 || args[0] != "status" {
		fmt.Fprintln(os.Stderr, "usage: compound sessions job status [--json]")
		os.Exit(2)
	}
	jsonOut := false
	for _, arg := range args[1:] {
		if arg == "--json" {
			jsonOut = true
			continue
		}
		fmt.Fprintln(os.Stderr, "usage: compound sessions job status [--json]")
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessions job status:", err)
		os.Exit(1)
	}
	status, err := buildSessionMiningJobStatus(homePath, sessionMiningJobID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessions job status:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(status)
		return
	}
	fmt.Printf("# session mining job status\n\n")
	fmt.Printf("- job_id: %s\n", status.JobID)
	fmt.Printf("- status: %s\n", status.Status)
	fmt.Printf("- declared: %t\n", status.Declared)
	fmt.Printf("- scheduler: observed=%t state=%s\n", status.SchedulerObserved, status.SchedulerState)
	fmt.Printf("- running: %t\n", status.Running)
	fmt.Printf("- checkpoint: present=%t events=%d incremental_ready=%t\n", status.CheckpointPresent, status.CheckpointEvents, status.IncrementalReady)
	if status.LastReceipt != nil {
		fmt.Printf("- last_run: %s status=%s duration_ms=%d scan=%s/%d\n", status.LastAttemptAt, status.LastReceipt.Status, status.LastDurationMS, status.LastReceipt.ScanMode, status.LastReceipt.EventsScanned)
	}
	fmt.Printf("- recommended_action: %s\n", status.RecommendedAction)
}

func buildSessionMiningJobStatus(homePath, jobID string) (sessionMiningJobStatus, error) {
	status := sessionMiningJobStatus{
		SchemaVersion:     sessionMiningJobStatusSchemaVersion,
		JobID:             jobID,
		Status:            "not_declared",
		HomePath:          homePath,
		CheckpointPath:    sessionMiningCheckpointPath(homePath, jobID),
		RecommendedAction: "import the governed job roster before scheduling this job",
	}
	roster, err := authority.LoadJobRosterAuthority(homePath)
	if err == nil {
		status.RosterInstalled = true
		for _, job := range roster.Jobs {
			if job.JobID != jobID {
				continue
			}
			status.Declared = true
			status.EnabledClaim = job.Enabled
			status.RosterState = job.State
			status.Schedule = job.Schedule
			status.Command = job.Script
			status.SchedulerObserved = job.ProbeObservedAt != ""
			status.SchedulerState = job.ProbeState
			status.SchedulerDetail = job.ProbeDetail
			break
		}
	} else if !os.IsNotExist(err) {
		return status, err
	}

	status.Running, err = sessionMiningJobLockHeld(homePath, jobID)
	if err != nil {
		return status, err
	}
	checkpoint, ok, err := loadSessionMiningCheckpoint(homePath, jobID, statusWindow(status.Command), statusHarness(status.Command))
	if err != nil {
		return status, err
	}
	if ok {
		status.CheckpointPresent = true
		status.CheckpointUpdated = checkpoint.UpdatedAt
		status.CheckpointEvents = len(checkpoint.Events)
		status.IncrementalReady = checkpoint.Cursor != ""
		status.EffectiveSettings = sessionMiningSettings{
			HarnessActor:      firstNonEmpty(checkpoint.Harness, "codex"),
			HarnessFilter:     checkpoint.Harness,
			RecentWorkLimit:   10,
			HookEventWindow:   checkpoint.Since,
			HookEventScanMode: "incremental",
			IncrementalSafe:   checkpoint.Cursor != "",
		}
	}
	receipt, receiptOK, err := loadLatestSessionMiningReceipt(homePath, jobID)
	if err != nil {
		return status, err
	}
	if receiptOK {
		status.LastReceipt = &receipt
		status.LastAttemptAt = receipt.StartedAt
		status.LastDurationMS = receipt.DurationMS
		status.LastError = receipt.Error
		status.RuntimeEvidenceRef = "file:" + sessionMiningLatestReceiptPath(homePath, jobID)
		if receipt.Status == "succeeded" {
			status.LastSuccessAt = receipt.FinishedAt
		}
		if status.EffectiveSettings.HookEventWindow == "" {
			status.EffectiveSettings = receipt.EffectiveSettings
		}
	}

	switch {
	case !status.Declared:
		status.Status = "not_declared"
	case status.Running:
		status.Status = "running"
		status.RecommendedAction = "wait for the active run to finish, then inspect its receipt"
	case status.SchedulerObserved && status.SchedulerState == "alive" && receiptOK && receipt.Status == "succeeded":
		status.Status = "healthy"
		status.RecommendedAction = "no action; monitor the next scheduled receipt"
	case status.SchedulerObserved && status.SchedulerState == "dead":
		status.Status = "scheduler_unavailable"
		status.RecommendedAction = "run authority job-health-plan and review scheduler repair"
	case receiptOK && receipt.Status == "failed":
		status.Status = "last_run_failed"
		status.RecommendedAction = "inspect the last receipt and run authority job-health-plan"
	case !status.SchedulerObserved:
		status.Status = "scheduler_unobserved"
		status.RecommendedAction = "operator installs or probes the scheduler; declaration alone is not runtime proof"
	default:
		status.Status = "awaiting_first_run"
		status.RecommendedAction = "run the governed dry-run command once and inspect its receipt"
	}
	return status, nil
}

func loadLatestSessionMiningReceipt(homePath, jobID string) (sessionMiningRunReceipt, bool, error) {
	data, err := os.ReadFile(sessionMiningLatestReceiptPath(homePath, jobID))
	if os.IsNotExist(err) {
		return sessionMiningRunReceipt{}, false, nil
	}
	if err != nil {
		return sessionMiningRunReceipt{}, false, err
	}
	var receipt sessionMiningRunReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return sessionMiningRunReceipt{}, false, err
	}
	if receipt.SchemaVersion != sessionMiningRunReceiptSchemaVersion || receipt.JobID != jobID {
		return sessionMiningRunReceipt{}, false, fmt.Errorf("unexpected session mining receipt identity")
	}
	return receipt, true, nil
}

func sessionMiningJobLockHeld(homePath, jobID string) (bool, error) {
	path := sessionMiningJobLockPath(homePath, jobID)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return true, nil
		}
		return false, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return false, err
	}
	return false, nil
}

func statusWindow(command string) string {
	fields := strings.Fields(command)
	for i, field := range fields {
		if field == "--since" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return "24h"
}

func statusHarness(command string) string {
	fields := strings.Fields(command)
	for i, field := range fields {
		if field == "--harness" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

func sessionMiningFindingCount(report sessionMiningReport) int {
	return len(report.StuckPoints) + len(report.MemoryCandidates) + len(report.CandidateSkillUpdates) + len(report.RepeatedReinventions) + len(report.RulesToRetire) + len(report.PreferencesFound)
}

func sessionMiningReceiptFiles(homePath, jobID string) ([]string, error) {
	entries, err := os.ReadDir(sessionMiningReceiptDir(homePath, jobID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			out = append(out, entry.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
