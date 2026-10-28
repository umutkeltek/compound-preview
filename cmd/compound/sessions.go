package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/harvest"
	"compound/internal/home"
	"compound/internal/mem"
	"compound/internal/sessionmining"
	"compound/internal/work"
)

const sessionMiningReportSchemaVersion = "compound.sessions_mine_report.v0.1"
const sessionMiningCheckpointSchemaVersion = "compound.sessions_mine_checkpoint.v0.1"
const sessionMiningJobID = "sessions-mine-daily-dry-run"

type sessionMiningCheckpoint struct {
	SchemaVersion string             `json:"schema_version"`
	JobID         string             `json:"job_id"`
	Since         string             `json:"since"`
	Harness       string             `json:"harness,omitempty"`
	Cursor        string             `json:"cursor"`
	UpdatedAt     string             `json:"updated_at"`
	Events        []sessionHookEvent `json:"events"`
}

type sessionMiningSettings struct {
	HarnessActor        string `json:"harness_actor"`
	HarnessFilter       string `json:"harness_filter,omitempty"`
	RecentWorkLimit     int    `json:"recent_work_limit"`
	HookEventWindow     string `json:"hook_event_window"`
	HookEventScanMode   string `json:"hook_event_scan_mode"`
	IncrementalSafe     bool   `json:"incremental_safe"`
	IncrementalLimiting string `json:"incremental_limiting,omitempty"`
}

type sessionMiningReport struct {
	SchemaVersion         string                    `json:"schema_version"`
	Status                string                    `json:"status"`
	GeneratedAt           string                    `json:"generated_at"`
	Since                 string                    `json:"since"`
	SinceCutoff           string                    `json:"since_cutoff"`
	Harness               string                    `json:"harness,omitempty"`
	DryRun                bool                      `json:"dry_run"`
	SessionCount          int                       `json:"session_count"`
	HookEventCount        int                       `json:"hook_event_count"`
	HookEventsScanned     int                       `json:"hook_events_scanned"`
	HookEventScanMode     string                    `json:"hook_event_scan_mode"`
	JobID                 string                    `json:"job_id,omitempty"`
	EffectiveSettings     sessionMiningSettings     `json:"effective_settings"`
	HarnessCounts         map[string]int            `json:"harness_counts"`
	HookCounts            map[string]int            `json:"hook_counts"`
	PreferencesFound      []sessionMiningFinding    `json:"preferences_found"`
	StuckPoints           []sessionMiningFinding    `json:"stuck_points"`
	RepeatedReinventions  []sessionMiningFinding    `json:"repeated_reinventions"`
	CandidateSkillUpdates []sessionMiningFinding    `json:"candidate_skill_updates"`
	RulesToRetire         []sessionMiningFinding    `json:"rules_to_retire"`
	MemoryCandidates      []sessionMiningFinding    `json:"memory_candidates"`
	ProofRequired         []string                  `json:"proof_required"`
	Checks                []sessionMiningCheck      `json:"checks"`
	NextCommand           []string                  `json:"next_command"`
	Apply                 *sessionMiningApplyReport `json:"apply,omitempty"`
}

type sessionMiningFinding = sessionmining.Finding

type sessionMiningCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type sessionMiningApplyReport = sessionmining.ApplyReport

type sessionMiningRoutedArtifact = sessionmining.RoutedArtifact

type sessionHookEvent = envelope.HookEvent

func cmdSessions(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, sessionsUsage())
		os.Exit(2)
	}
	switch args[0] {
	case "mine":
		cmdSessionsMine(args[1:])
	case "harvest":
		cmdSessionsHarvest(args[1:])
	case "backfill":
		cmdSessionsBackfill(args[1:])
	case "live":
		cmdSessionsLive(args[1:])
	case "job":
		cmdSessionsJob(args[1:])
	default:
		fmt.Fprintln(os.Stderr, sessionsUsage())
		os.Exit(2)
	}
}

func sessionsUsage() string {
	return "usage:\n" +
		"  compound sessions mine [--dry-run|--apply] [--since 24h] [--harness codex] [--job-run ID] [--json]\n" +
		"  compound sessions job status [--json]\n" +
		"  compound sessions harvest --transcript <path> [--dry-run] [--json]\n" +
		"  compound sessions backfill --ledger-dir <dir> [--dry-run|--apply] [--limit N] [--json]\n" +
		"  compound sessions live [--json] [--ghosts-only]"
}

// cmdSessionsBackfill runs the ruling harvester BACKWARDS over the historical
// decision ledgers: it parses every decision-ledger.generated.md into candidate
// rows, routes each (system ruling / product decision / ephemeral one-off),
// reconciles system rulings against the accepted decision records FIRST
// (restatements corroborate, reversals conflict, novel rulings cluster among
// themselves), and emits ONE parked candidate per novel cluster up to --limit.
// --dry-run is read-only (the live-proof path for falsifier f); --apply appends
// corroboration evidence + writes parked candidates and per-product reports.
func cmdSessionsBackfill(args []string) {
	ledgerDir := ""
	jsonOut := false
	dryRun := false
	apply := false
	limit := harvest.DefaultNovelCap
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--ledger-dir":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, sessionsUsage())
				os.Exit(2)
			}
			ledgerDir = args[i+1]
			i++
		case "--limit":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, sessionsUsage())
				os.Exit(2)
			}
			if _, err := fmt.Sscanf(args[i+1], "%d", &limit); err != nil || limit < 0 {
				fmt.Fprintln(os.Stderr, "sessions backfill: --limit must be a non-negative integer")
				os.Exit(2)
			}
			i++
		case "--dry-run":
			dryRun = true
		case "--apply":
			apply = true
		case "--json":
			jsonOut = true
		default:
			fmt.Fprintln(os.Stderr, sessionsUsage())
			os.Exit(2)
		}
	}
	if dryRun && apply {
		fmt.Fprintln(os.Stderr, "sessions backfill: choose --dry-run or --apply")
		os.Exit(2)
	}
	// Default is read-only: applying side effects requires an explicit --apply.
	if !apply {
		dryRun = true
	}
	if strings.TrimSpace(ledgerDir) == "" {
		fmt.Fprintln(os.Stderr, "sessions backfill: --ledger-dir <dir> required")
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessions backfill:", err)
		os.Exit(1)
	}
	report, err := harvest.Backfill(store, homePath, harvest.BackfillOptions{
		LedgerDir: ledgerDir,
		DryRun:    dryRun,
		NovelCap:  limit,
		Now:       time.Now(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessions backfill:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(report)
		return
	}
	fmt.Printf("# session ruling backfill%s\n\n", map[bool]string{true: " (dry-run)"}[report.DryRun])
	fmt.Printf("- status: %s\n", report.Status)
	fmt.Printf("- ledger_dir: %s\n", report.LedgerDir)
	fmt.Printf("- files_parsed: %d\n", report.FilesParsed)
	fmt.Printf("- entries_total: %d\n", report.EntriesTotal)
	fmt.Printf("- accepted_records: %d\n", report.AcceptedRecords)
	fmt.Printf("- route[system/product/ephemeral]: %d / %d / %d\n", report.RouteCounts.System, report.RouteCounts.Product, report.RouteCounts.Ephemeral)
	fmt.Printf("- corroborated_records: %d\n", len(report.Corroborations))
	fmt.Printf("- conflicts: %d\n", len(report.Conflicts))
	fmt.Printf("- novel_clusters_found: %d (emitted %d, skipped_by_cap %d)\n", report.NovelClustersFound, report.NovelEmitted, report.NovelSkippedByCap)
	fmt.Printf("- parse_issues: %d\n", report.ParseIssueCount)
	for _, cr := range report.Corroborations {
		fmt.Printf("- corroboration [%s via %s] %s x%d\n", cr.ClaimID, cr.Via, cr.DecisionID, cr.Count)
	}
	for i, c := range report.NovelSamples {
		fmt.Printf("- novel#%d (x%d) %s\n", i+1, c.CorroborationCount, clipHarvestQuote(c.Head))
	}
}

// cmdSessionsHarvest turns operator utterances in one session transcript into
// decision-record candidates through the existing machinery: restatements of
// accepted records append corroboration evidence (no duplicate), novel rulings
// land as proposed-only decision candidates. --dry-run is read-only (the live
// proof path); default applies the corroborations/candidates.
func cmdSessionsHarvest(args []string) {
	transcript := ""
	jsonOut := false
	dryRun := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--transcript":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, sessionsUsage())
				os.Exit(2)
			}
			transcript = args[i+1]
			i++
		case "--dry-run":
			dryRun = true
		case "--json":
			jsonOut = true
		default:
			fmt.Fprintln(os.Stderr, sessionsUsage())
			os.Exit(2)
		}
	}
	if strings.TrimSpace(transcript) == "" {
		fmt.Fprintln(os.Stderr, "sessions harvest: --transcript <path> required")
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessions harvest:", err)
		os.Exit(1)
	}
	report, err := harvest.Run(store, homePath, harvest.Options{
		TranscriptPath: transcript,
		DryRun:         dryRun,
		Now:            time.Now(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessions harvest:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(report)
		return
	}
	fmt.Printf("# session ruling harvest%s\n\n", map[bool]string{true: " (dry-run)"}[dryRun])
	fmt.Printf("- status: %s\n", report.Status)
	fmt.Printf("- transcript: %s\n", report.TranscriptPath)
	fmt.Printf("- session: %s\n", report.SessionID)
	fmt.Printf("- utterances: %d\n", report.Utterances)
	fmt.Printf("- classified: %d\n", report.Classified)
	fmt.Printf("- corroborations: %d\n", report.Corroborations)
	fmt.Printf("- conflicts: %d\n", report.Conflicts)
	fmt.Printf("- novel: %d\n", report.Novel)
	for _, item := range report.Items {
		matched := ""
		if item.Match != nil {
			matched = fmt.Sprintf(" -> %s (%.2f)", item.Match.ClaimID, item.Match.Score)
		}
		fmt.Printf("- [%s/%s L%d]%s %s\n", item.Outcome, item.Kind, item.LineNo, matched, clipHarvestQuote(item.Quote))
	}
}

func clipHarvestQuote(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= 100 {
		return s
	}
	return string([]rune(s)[:97]) + "..."
}

func cmdSessionsMine(args []string) {
	since := "24h"
	jsonOut := false
	dryRun := false
	apply := false
	harnessName := ""
	jobID := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--since":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, sessionsUsage())
				os.Exit(2)
			}
			since = args[i+1]
			i++
		case "--harness":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, sessionsUsage())
				os.Exit(2)
			}
			harnessName = strings.TrimSpace(args[i+1])
			i++
		case "--job-run":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, sessionsUsage())
				os.Exit(2)
			}
			jobID = strings.TrimSpace(args[i+1])
			i++
		case "--dry-run":
			dryRun = true
		case "--apply":
			apply = true
		case "--json":
			jsonOut = true
		default:
			fmt.Fprintln(os.Stderr, sessionsUsage())
			os.Exit(2)
		}
	}
	if dryRun && apply {
		fmt.Fprintln(os.Stderr, "sessions mine: choose --dry-run or --apply")
		os.Exit(2)
	}
	if jobID != "" && (apply || !dryRun) {
		fmt.Fprintln(os.Stderr, "sessions mine: --job-run requires --dry-run and cannot apply")
		os.Exit(2)
	}
	if jobID != "" && jobID != sessionMiningJobID {
		fmt.Fprintf(os.Stderr, "sessions mine: unknown job id %q\n", jobID)
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	var report sessionMiningReport
	if jobID != "" {
		report, err = buildSessionMiningJobReport(homePath, since, harnessName, jobID, time.Now())
	} else {
		report, err = buildSessionMiningReport(homePath, since, harnessName, time.Now())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessions mine:", err)
		os.Exit(1)
	}
	if apply {
		applyReport, err := applySessionMiningReport(homePath, &report, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "sessions mine apply:", err)
			os.Exit(1)
		}
		report.Apply = &applyReport
		report.DryRun = false
		report.Status = applyReport.Status
		report.NextCommand = []string{"compound", "reviews", "--json"}
	} else if !dryRun {
		next := []string{"compound", "sessions", "mine", "--since", since}
		if harnessName != "" {
			next = append(next, "--harness", harnessName)
		}
		next = append(next, "--apply", "--json")
		report.NextCommand = next
	}
	if jsonOut {
		printJSON(report)
		return
	}
	if apply {
		fmt.Printf("# session mining apply\n\n")
	} else {
		fmt.Printf("# session mining dry-run\n\n")
	}
	fmt.Printf("- status: %s\n", report.Status)
	fmt.Printf("- since: %s\n", report.Since)
	if report.Harness != "" {
		fmt.Printf("- harness: %s\n", report.Harness)
	}
	fmt.Printf("- sessions: %d\n", report.SessionCount)
	fmt.Printf("- hook_events: %d (scanned=%d mode=%s)\n", report.HookEventCount, report.HookEventsScanned, report.HookEventScanMode)
	fmt.Printf("- stuck_points: %d\n", len(report.StuckPoints))
	fmt.Printf("- memory_candidates: %d\n", len(report.MemoryCandidates))
	fmt.Printf("- skill_updates: %d\n", len(report.CandidateSkillUpdates))
	fmt.Printf("- preferences_found: %d\n", len(report.PreferencesFound))
	if report.Apply != nil {
		fmt.Printf("- preference_candidates: %d\n", len(report.Apply.PreferenceCandidateIDs))
		fmt.Printf("- retire_proposals: %d\n", len(report.Apply.RetireProposalIDs))
		fmt.Printf("- skill_update_proposals: %d\n", len(report.Apply.SkillUpdateProposalIDs))
	}
	for _, proof := range report.ProofRequired {
		fmt.Printf("- proof_required: %s\n", proof)
	}
}

func buildSessionMiningReport(homePath, since, harnessName string, now time.Time) (sessionMiningReport, error) {
	window, err := parseSessionMiningSince(since)
	if err != nil {
		return sessionMiningReport{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-window)
	events, scan, warnings, err := loadRecentSessionHookEvents(homePath, cutoff, harnessName)
	if err != nil {
		return sessionMiningReport{}, err
	}
	return assembleSessionMiningReport(homePath, since, harnessName, now, events, scan, warnings)
}

func assembleSessionMiningReport(homePath, since, harnessName string, now time.Time, events []sessionHookEvent, scan sessionHookScan, warnings []string) (sessionMiningReport, error) {
	window, err := parseSessionMiningSince(since)
	if err != nil {
		return sessionMiningReport{}, err
	}
	cutoff := now.Add(-window)
	initNext := []string{"compound", "sessions", "mine", "--since", since}
	if harnessName != "" {
		initNext = append(initNext, "--harness", harnessName)
	}
	initNext = append(initNext, "--dry-run", "--json")
	report := sessionMiningReport{
		SchemaVersion: sessionMiningReportSchemaVersion,
		Status:        "ok",
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Since:         since,
		SinceCutoff:   cutoff.UTC().Format(time.RFC3339),
		Harness:       harnessName,
		DryRun:        true,
		HarnessCounts: map[string]int{},
		HookCounts:    map[string]int{},
		NextCommand:   initNext,
		ProofRequired: []string{
			"dry-run only; no session import, no memory promotion, no skill promotion",
			"every candidate must keep evidence refs before review",
			"one promoted memory must later prove recall changed a ContextPack or work result",
		},
	}

	report.HookEventCount = len(events)
	report.HookEventsScanned = scan.Scanned
	report.HookEventScanMode = scan.Mode
	report.EffectiveSettings = sessionMiningSettings{
		HarnessActor:        firstNonEmpty(harnessName, "codex"),
		HarnessFilter:       harnessName,
		RecentWorkLimit:     10,
		HookEventWindow:     since,
		HookEventScanMode:   scan.Mode,
		IncrementalSafe:     false,
		IncrementalLimiting: "windowed pattern detection requires a persisted rolling aggregate; a byte cursor alone can split a repeated pattern across runs",
	}
	sessions := map[string]struct{}{}
	for _, ev := range events {
		if ev.IDs.SessionID != "" {
			sessions[ev.IDs.SessionID] = struct{}{}
		}
		if ev.Harness != "" {
			report.HarnessCounts[ev.Harness]++
		}
		if ev.HookName != "" {
			report.HookCounts[ev.HookName]++
		}
	}
	report.SessionCount = len(sessions)
	if len(events) == 0 {
		report.Checks = append(report.Checks, sessionMiningCheck{Name: "recent_session_events", Status: "warn", Detail: "no hook events in window"})
	} else {
		report.Checks = append(report.Checks, sessionMiningCheck{Name: "recent_session_events", Status: "ok", Detail: fmt.Sprintf("%d sessions / %d hook events", report.SessionCount, report.HookEventCount)})
	}
	report.Checks = append(report.Checks, sessionMiningCheck{
		Name:   "hook_event_scan",
		Status: "ok",
		Detail: fmt.Sprintf("mode=%s scanned=%d selected=%d", report.HookEventScanMode, report.HookEventsScanned, report.HookEventCount),
	})
	for _, warning := range warnings {
		report.Checks = append(report.Checks, sessionMiningCheck{Name: "hook_event_warning", Status: "warn", Detail: warning})
	}

	store, err := work.Open(homePath)
	if err != nil {
		return report, err
	}
	actor := report.EffectiveSettings.HarnessActor
	workbench, err := store.Workbench(now, work.WorkbenchOptions{RecentLimit: report.EffectiveSettings.RecentWorkLimit, Actor: actor})
	if err != nil {
		return report, err
	}
	report.StuckPoints = sessionMiningStuckPoints(workbench)
	report.MemoryCandidates = sessionMiningMemoryCandidates(workbench)
	report.CandidateSkillUpdates = sessionMiningSkillUpdates(events)
	report.RepeatedReinventions = sessionMiningRepeatedReinventions(workbench)
	report.RulesToRetire = sessionMiningRulesToRetire(workbench)
	report.PreferencesFound = sessionMiningPreferences(events, workbench)
	return report, nil
}

func sessionMiningCheckpointPath(homePath, jobID string) string {
	return filepath.Join(homePath, "projections", "sessions-mine", jobID+".json")
}

func sessionMiningJobLockPath(homePath, jobID string) string {
	return filepath.Join(homePath, "lock", jobID+".lock")
}

func buildSessionMiningJobReport(homePath, since, harnessName, jobID string, now time.Time) (report sessionMiningReport, resultErr error) {
	if now.IsZero() {
		now = time.Now()
	}
	started := now
	receiptEnabled := false
	defer func() {
		if !receiptEnabled {
			return
		}
		finished := time.Now().UTC()
		if finished.Before(started) {
			finished = started
		}
		receipt := sessionMiningRunReceipt{
			SchemaVersion:     sessionMiningRunReceiptSchemaVersion,
			ID:                sessionMiningRunReceiptID(jobID, started),
			JobID:             jobID,
			StartedAt:         started.UTC().Format(time.RFC3339Nano),
			FinishedAt:        finished.Format(time.RFC3339Nano),
			DurationMS:        finished.Sub(started).Milliseconds(),
			Status:            "succeeded",
			ScanMode:          report.HookEventScanMode,
			EventsScanned:     report.HookEventsScanned,
			EventsRetained:    report.HookEventCount,
			SessionsSeen:      report.SessionCount,
			Findings:          sessionMiningFindingCount(report),
			EffectiveSettings: report.EffectiveSettings,
			CheckpointPath:    sessionMiningCheckpointPath(homePath, jobID),
		}
		if resultErr != nil {
			receipt.Status = "failed"
			receipt.Error = resultErr.Error()
		}
		if err := writeSessionMiningRunReceipt(homePath, receipt); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("record session mining run receipt: %w", err))
		}
	}()
	window, err := parseSessionMiningSince(since)
	if err != nil {
		return sessionMiningReport{}, err
	}
	lockPath := sessionMiningJobLockPath(homePath, jobID)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return sessionMiningReport{}, err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return sessionMiningReport{}, fmt.Errorf("open session mining job lock: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return sessionMiningReport{}, fmt.Errorf("session mining job %s is already running: %w", jobID, err)
	}
	receiptEnabled = true
	defer func() { resultErr = errors.Join(resultErr, syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)) }()

	checkpoint, checkpointOK, err := loadSessionMiningCheckpoint(homePath, jobID, since, harnessName)
	if err != nil {
		return sessionMiningReport{}, err
	}
	store, err := eventstore.Open(homePath)
	if err != nil {
		return sessionMiningReport{}, err
	}
	page, err := store.ScanFrom(eventstore.ScanOptions{Prefixes: []string{"hook_"}, RegularOnly: true, Cursor: checkpoint.Cursor})
	if err != nil {
		if os.IsNotExist(err) {
			page = eventstore.ScanPage{}
		} else {
			return sessionMiningReport{}, fmt.Errorf("scan session mining job events: %w", err)
		}
	}
	cutoff := now.Add(-window)
	base := checkpoint.Events
	mode := "incremental"
	if !checkpointOK || !page.Resumed {
		base = nil
		mode = "full"
	}
	events, warnings, err := foldSessionMiningEvents(base, page.Entries, cutoff, harnessName)
	if err != nil {
		return sessionMiningReport{}, err
	}
	scan := sessionHookScan{Mode: mode, Scanned: len(page.Entries)}
	report, err = assembleSessionMiningReport(homePath, since, harnessName, now, events, scan, warnings)
	if err != nil {
		return sessionMiningReport{}, err
	}
	report.JobID = jobID
	report.NextCommand = []string{"compound", "sessions", "mine", "--since", since}
	if harnessName != "" {
		report.NextCommand = append(report.NextCommand, "--harness", harnessName)
	}
	report.NextCommand = append(report.NextCommand, "--dry-run", "--job-run", jobID, "--json")
	if page.Cursor != "" {
		report.EffectiveSettings.IncrementalSafe = true
		report.EffectiveSettings.IncrementalLimiting = ""
	} else {
		report.EffectiveSettings.IncrementalSafe = false
		report.EffectiveSettings.IncrementalLimiting = "event store is still in legacy-fallback mode, so ScanFrom cannot issue a stable byte cursor"
	}

	next := sessionMiningCheckpoint{
		SchemaVersion: sessionMiningCheckpointSchemaVersion,
		JobID:         jobID,
		Since:         since,
		Harness:       harnessName,
		Cursor:        page.Cursor,
		UpdatedAt:     now.UTC().Format(time.RFC3339),
		Events:        events,
	}
	body, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return sessionMiningReport{}, err
	}
	if err := home.WriteFileAtomic(sessionMiningCheckpointPath(homePath, jobID), append(body, '\n'), 0o600); err != nil {
		return sessionMiningReport{}, fmt.Errorf("write session mining checkpoint: %w", err)
	}
	return report, nil
}

func loadSessionMiningCheckpoint(homePath, jobID, since, harnessName string) (sessionMiningCheckpoint, bool, error) {
	path := sessionMiningCheckpointPath(homePath, jobID)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return sessionMiningCheckpoint{}, false, nil
	}
	if err != nil {
		return sessionMiningCheckpoint{}, false, fmt.Errorf("read session mining checkpoint: %w", err)
	}
	var checkpoint sessionMiningCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		// The checkpoint is a cache, never authority. A malformed body must recover
		// through ScanFrom's full-rescan path rather than taking the scheduled job
		// down permanently.
		return sessionMiningCheckpoint{}, false, nil
	}
	if checkpoint.SchemaVersion != sessionMiningCheckpointSchemaVersion || checkpoint.JobID != jobID || checkpoint.Since != since || checkpoint.Harness != harnessName {
		return sessionMiningCheckpoint{}, false, nil
	}
	return checkpoint, true, nil
}

func foldSessionMiningEvents(base []sessionHookEvent, entries []eventstore.Entry, cutoff time.Time, harnessName string) ([]sessionHookEvent, []string, error) {
	byID := make(map[string]sessionHookEvent, len(base)+len(entries))
	for _, ev := range base {
		created, err := time.Parse(time.RFC3339, ev.CreatedAt)
		if err == nil && !created.Before(cutoff) && (harnessName == "" || ev.Harness == harnessName) {
			byID[ev.ID] = ev
		}
	}
	var warnings []string
	for _, entry := range entries {
		rec, err := entry.Read()
		if err != nil {
			return nil, nil, fmt.Errorf("read hook event %s: %w", entry.ID, err)
		}
		var ev sessionHookEvent
		if err := json.Unmarshal(rec.Bytes, &ev); err != nil {
			warnings = append(warnings, fmt.Sprintf("record %s unreadable", entry.ID))
			continue
		}
		created, err := time.Parse(time.RFC3339, ev.CreatedAt)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("record %s missing created_at", entry.ID))
			continue
		}
		if created.Before(cutoff) || (harnessName != "" && ev.Harness != harnessName) {
			delete(byID, ev.ID)
			continue
		}
		byID[ev.ID] = ev
	}
	out := make([]sessionHookEvent, 0, len(byID))
	for _, ev := range byID {
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, warnings, nil
}

func parseSessionMiningSince(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 24 * time.Hour, nil
	}
	if strings.HasSuffix(value, "d") {
		days := strings.TrimSuffix(value, "d")
		var n int
		if _, err := fmt.Sscanf(days, "%d", &n); err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid --since %q", value)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid --since %q", value)
	}
	return d, nil
}

type sessionHookScan struct {
	Mode    string
	Scanned int
}

func loadRecentSessionHookEvents(homePath string, cutoff time.Time, harnessName string) ([]sessionHookEvent, sessionHookScan, []string, error) {
	store, err := eventstore.Open(homePath)
	if err != nil {
		return nil, sessionHookScan{}, nil, err
	}
	// This is deliberately reported as a full scan: hook IDs are hashes rather
	// than time-sortable keys, and window mining needs every event in the window
	// to detect repeated patterns. A byte cursor alone would miss patterns split
	// across runs; a future incremental implementation must persist a rolling
	// aggregate alongside its cursor.
	entries, err := store.Scan(eventstore.ScanOptions{Prefixes: []string{"hook_"}, RegularOnly: true})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, sessionHookScan{Mode: "full"}, nil, nil
		}
		return nil, sessionHookScan{}, nil, fmt.Errorf("read hook events: %w", err)
	}
	scan := sessionHookScan{Mode: "full", Scanned: len(entries)}
	var out []sessionHookEvent
	var warnings []string
	for _, entry := range entries {
		rec, err := entry.Read()
		if err != nil {
			return nil, scan, nil, fmt.Errorf("read hook events: %w", err)
		}
		var ev sessionHookEvent
		if err := json.Unmarshal(rec.Bytes, &ev); err != nil {
			warnings = append(warnings, fmt.Sprintf("record %s unreadable", entry.ID))
			continue
		}
		created, err := time.Parse(time.RFC3339, ev.CreatedAt)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("record %s missing created_at", entry.ID))
			continue
		}
		if created.Before(cutoff) || (harnessName != "" && ev.Harness != harnessName) {
			continue
		}
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, scan, warnings, nil
}

func sessionMiningStuckPoints(workbench work.WorkbenchReport) []sessionMiningFinding {
	var out []sessionMiningFinding
	seen := map[string]bool{}
	for _, item := range workbench.Open {
		if item.State != "held" && item.State != "blocked" {
			continue
		}
		summary := strings.TrimSpace(item.NextAction)
		if summary == "" {
			summary = strings.TrimSpace(item.SuccessPredicate)
		}
		if summary == "" {
			summary = "work is waiting for input"
		}
		key := item.WorkID + ":" + summary
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, sessionMiningFinding{
			Kind:         "held_work",
			SemanticKey:  sessionMiningSemanticKey("held_work", item.WorkID),
			Summary:      summary,
			EvidenceRefs: []string{"work:" + item.WorkID},
			Confidence:   0.8,
		})
	}
	return out
}

func sessionMiningMemoryCandidates(workbench work.WorkbenchReport) []sessionMiningFinding {
	var out []sessionMiningFinding
	seen := map[string]bool{}
	for _, item := range workbench.Open {
		for _, card := range item.ReviewCards {
			if card.Kind != "memory_candidate_adjudication" || card.CandidateID == "" || seen[card.CandidateID] {
				continue
			}
			seen[card.CandidateID] = true
			summary := firstNonEmpty(card.CandidateObject, card.Title, "memory candidate needs review")
			out = append(out, sessionMiningFinding{
				Kind:         "memory_candidate",
				SemanticKey:  sessionMiningSemanticKey("memory_candidate", card.CandidateID),
				Summary:      summary,
				EvidenceRefs: compactSessionRefs("work:"+item.WorkID, "memory_candidate:"+card.CandidateID, "panel_run:"+card.PanelRunID),
				Confidence:   0.72,
			})
		}
	}
	return out
}

func compactSessionRefs(values ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func sessionMiningSkillUpdates(events []sessionHookEvent) []sessionMiningFinding {
	if len(events) == 0 {
		return nil
	}
	byHarness := map[string][]sessionHookEvent{}
	for _, ev := range events {
		if ev.Harness != "" {
			byHarness[ev.Harness] = append(byHarness[ev.Harness], ev)
		}
	}
	var out []sessionMiningFinding
	for harness, grouped := range byHarness {
		count := len(grouped)
		if count < 2 {
			continue
		}
		out = append(out, sessionMiningFinding{
			Kind:         "session_pattern_review",
			SemanticKey:  sessionMiningSemanticKey("session_pattern_review", harness),
			Summary:      fmt.Sprintf("Review %d recent %s hook events for repeated tool gaps or reusable skill updates.", count, harness),
			EvidenceRefs: sessionEventGroupEvidenceRefs(grouped),
			Confidence:   0.35,
		})
	}
	sortSessionFindings(out)
	return out
}

func sessionMiningRepeatedReinventions(workbench work.WorkbenchReport) []sessionMiningFinding {
	if workbench.Counts.ContinueRuns+workbench.Counts.DispatchStepRuns < 3 {
		return nil
	}
	return []sessionMiningFinding{{
		Kind:        "control_plane_movement",
		SemanticKey: sessionMiningSemanticKey("control_plane_movement", "continue-dispatch"),
		Summary:     "Multiple recent continue/dispatch runs exist; inspect whether repeated lifecycle movement should become a smoother control.",
		Confidence:  0.4,
	}}
}

func sessionMiningRulesToRetire(workbench work.WorkbenchReport) []sessionMiningFinding {
	if workbench.Counts.NeedsReview == 0 {
		return nil
	}
	refs := []string{}
	for _, item := range workbench.Open {
		if item.State != "held" && item.State != "blocked" {
			continue
		}
		refs = compactSessionRefs(append([]string{"work:" + item.WorkID}, item.EvidenceRefs...)...)
		if len(refs) > 0 {
			break
		}
	}
	return []sessionMiningFinding{{
		Kind:         "review_friction",
		SemanticKey:  sessionMiningSemanticKey("review_friction", "held-work-decision-cards"),
		Summary:      "Review-held work exists; retire or hide any rule that only creates a decision card without changing future execution.",
		EvidenceRefs: refs,
		Confidence:   0.3,
	}}
}

type sessionPreferenceSignal struct {
	Kind         string
	Label        string
	EvidenceRefs []string
}

type sessionPreferenceGroup struct {
	Kind         string
	Label        string
	Count        int
	EvidenceRefs []string
}

func sessionMiningPreferences(events []sessionHookEvent, workbench work.WorkbenchReport) []sessionMiningFinding {
	groups := map[string]*sessionPreferenceGroup{}
	add := func(sig sessionPreferenceSignal) {
		if sig.Kind == "" || sig.Label == "" || len(sig.EvidenceRefs) == 0 {
			return
		}
		key := sig.Kind + "\x00" + strings.ToLower(strings.TrimSpace(sig.Label))
		group := groups[key]
		if group == nil {
			group = &sessionPreferenceGroup{Kind: sig.Kind, Label: sig.Label}
			groups[key] = group
		}
		group.Count++
		group.EvidenceRefs = compactSessionRefs(append(group.EvidenceRefs, sig.EvidenceRefs...)...)
	}
	for _, ev := range events {
		for _, sig := range sessionPreferenceSignalsFromEvent(ev) {
			add(sig)
		}
	}
	for _, sig := range sessionPreferenceSignalsFromWorkbench(workbench) {
		add(sig)
	}
	out := make([]sessionMiningFinding, 0, len(groups))
	for _, group := range groups {
		if group.Count < 2 {
			continue
		}
		out = append(out, sessionMiningFinding{
			Kind:         group.Kind,
			SemanticKey:  sessionMiningSemanticKey(group.Kind, group.Label),
			Summary:      sessionPreferenceSummary(group.Kind, group.Label, group.Count),
			EvidenceRefs: group.EvidenceRefs,
			Confidence:   sessionPreferenceConfidence(group.Kind, group.Count),
		})
	}
	sortSessionFindings(out)
	return out
}

func sessionPreferenceSignalsFromEvent(ev sessionHookEvent) []sessionPreferenceSignal {
	refs := sessionEventEvidenceRefs(ev)
	if len(refs) == 0 {
		return nil
	}
	var out []sessionPreferenceSignal
	for _, effect := range ev.SideEffects {
		kind, label, ok := sessionPreferenceSignalFromSideEffect(effect, ev.HookName)
		if !ok {
			continue
		}
		out = append(out, sessionPreferenceSignal{Kind: kind, Label: label, EvidenceRefs: refs})
	}
	return out
}

func sessionPreferenceSignalFromSideEffect(effect, hookName string) (string, string, bool) {
	raw := strings.TrimSpace(effect)
	if raw == "" {
		return "", "", false
	}
	lower := strings.ToLower(raw)
	prefixes := []struct {
		Prefix string
		Kind   string
	}{
		{"operator_correction:", "operator_correction"},
		{"correction:", "operator_correction"},
		{"manual_override:", "manual_override"},
		{"operator_override:", "manual_override"},
		{"override:", "manual_override"},
		{"tool_choice:", "repeated_tool_choice"},
		{"tool:", "repeated_tool_choice"},
		{"lane_choice:", "repeated_lane_choice"},
		{"route_lane:", "repeated_lane_choice"},
		{"lane:", "repeated_lane_choice"},
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(lower, prefix.Prefix) {
			label := strings.TrimSpace(raw[len(prefix.Prefix):])
			if label == "" {
				return "", "", false
			}
			return prefix.Kind, label, true
		}
	}
	if strings.EqualFold(strings.TrimSpace(hookName), "PostToolUse") && plainSessionToken(raw) {
		return "repeated_tool_choice", raw, true
	}
	return "", "", false
}

func sessionPreferenceSignalsFromWorkbench(workbench work.WorkbenchReport) []sessionPreferenceSignal {
	var out []sessionPreferenceSignal
	items := append([]work.WorkbenchItem{}, workbench.Open...)
	items = append(items, workbench.Recent...)
	for _, item := range items {
		for _, obs := range item.RunObservations {
			for _, raw := range []string{obs.Note, obs.State} {
				kind, label, ok := sessionPreferenceSignalFromSideEffect(raw, "")
				if !ok {
					continue
				}
				out = append(out, sessionPreferenceSignal{
					Kind:         kind,
					Label:        label,
					EvidenceRefs: compactSessionRefs("work:"+item.WorkID, "run_observation:"+obs.ID, obs.ActionIntentID, obs.PolicyDecisionID),
				})
			}
		}
		for _, route := range item.RouteOutcomes {
			label := strings.Join(compactSessionRefs(route.Provider, route.Model, route.Protocol), "/")
			if label == "" {
				continue
			}
			out = append(out, sessionPreferenceSignal{
				Kind:         "repeated_lane_choice",
				Label:        label,
				EvidenceRefs: compactSessionRefs("work:"+item.WorkID, "receipt:"+route.ReceiptID, route.ReviewRef, route.RouteEvidenceID),
			})
		}
	}
	return out
}

func sessionPreferenceSummary(kind, label string, count int) string {
	switch kind {
	case "operator_correction":
		return fmt.Sprintf("Operator repeated the same correction %q across %d session evidence refs.", label, count)
	case "manual_override":
		return fmt.Sprintf("Operator repeatedly overrode the same suggestion %q across %d session evidence refs.", label, count)
	case "repeated_tool_choice":
		return fmt.Sprintf("Operator or harness repeatedly chose tool %q across %d session evidence refs.", label, count)
	case "repeated_lane_choice":
		return fmt.Sprintf("Operator or harness repeatedly chose lane %q across %d session evidence refs.", label, count)
	default:
		return fmt.Sprintf("Repeated session preference %q across %d evidence refs.", label, count)
	}
}

func sessionPreferenceConfidence(kind string, count int) float64 {
	base := map[string]float64{
		"operator_correction":  0.72,
		"manual_override":      0.7,
		"repeated_tool_choice": 0.62,
		"repeated_lane_choice": 0.62,
	}
	conf := base[kind]
	if conf == 0 {
		conf = 0.55
	}
	conf += float64(count-2) * 0.05
	if conf > 0.9 {
		return 0.9
	}
	return conf
}

func plainSessionToken(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "/") || strings.Contains(value, "\\") || strings.Contains(value, ":") || strings.Contains(value, " ") {
		return false
	}
	return len(value) <= 48
}

func sessionEventEvidenceRefs(ev sessionHookEvent) []string {
	refs := []string{}
	if ev.ID != "" {
		refs = append(refs, "hook:"+ev.ID)
	}
	if ev.IDs.SessionID != "" {
		refs = append(refs, "session:"+ev.IDs.SessionID)
	}
	if ev.IDs.WorkID != "" {
		refs = append(refs, "work:"+ev.IDs.WorkID)
	}
	if ev.OutputRef != "" {
		refs = append(refs, ev.OutputRef)
	}
	refs = append(refs, ev.EvidenceRefs...)
	return compactSessionRefs(refs...)
}

func sessionEventGroupEvidenceRefs(events []sessionHookEvent) []string {
	refs := []string{}
	for _, ev := range events {
		refs = append(refs, sessionEventEvidenceRefs(ev)...)
	}
	return compactSessionRefs(refs...)
}

func sortSessionFindings(findings []sessionMiningFinding) {
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Kind != findings[j].Kind {
			return findings[i].Kind < findings[j].Kind
		}
		return findings[i].Summary < findings[j].Summary
	})
}

func applySessionMiningReport(homePath string, report *sessionMiningReport, now time.Time) (sessionMiningApplyReport, error) {
	return sessionmining.Apply(homePath, sessionmining.ApplyInput{
		Preferences:   report.PreferencesFound,
		RulesToRetire: report.RulesToRetire,
		SkillUpdates:  report.CandidateSkillUpdates,
	}, now)
}

func sessionMiningSemanticKey(kind, identity string) string {
	return sessionmining.SemanticKey(kind, identity)
}
