package work

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/eventstore"
	"compound/internal/localmodel"
)

const ExecutorsProjectionSchemaVersion = "compound.executors_projection.v0.1"
const ExecutorsAuthorityRef = "compound://executors/projection"

// ExecutorsProjection is the read-only executor board source. It groups
// Workbench and lease facts under Compound-owned executor profile rows; health
// remains source-labeled until a real harness probe feeds this surface.
type ExecutorsProjection struct {
	SchemaVersion string         `json:"schema_version"`
	AuthorityRef  string         `json:"authority_ref"`
	Status        string         `json:"status"`
	GeneratedAt   string         `json:"generated_at"`
	Counts        ExecutorCounts `json:"counts"`
	Executors     []ExecutorRow  `json:"executors"`
	// DelegatedSessions is the safe, reduced supervised-session board folded
	// from work.delegate.{spawned,steered,refused,exited}. Always a non-nil
	// slice (empty when nothing is delegated) so the console renders a clean
	// empty state instead of null.
	DelegatedSessions []DelegatedSessionRow  `json:"delegated_sessions"`
	DelegatedCounts   DelegatedSessionCounts `json:"delegated_counts"`
	SourceRefs        []string               `json:"source_refs"`
	NextCommand       []string               `json:"next_command"`
	Integrity         []string               `json:"integrity,omitempty"`
}

type ExecutorCounts struct {
	Total        int `json:"total"`
	Open         int `json:"open"`
	Running      int `json:"running"`
	Ready        int `json:"ready"`
	NeedsMe      int `json:"needs_me"`
	Assigned     int `json:"assigned"`
	Leases       int `json:"leases"`
	StaleLeases  int `json:"stale_leases,omitempty"`
	StalledJobs  int `json:"stalled_jobs,omitempty"`
	ProfileGuide int `json:"profile_guide"`
}

type ExecutorRow struct {
	ID                    string                  `json:"id"`
	Label                 string                  `json:"label"`
	Status                string                  `json:"status"`
	StatusSource          string                  `json:"status_source"`
	Tone                  string                  `json:"tone,omitempty"`
	Aliases               []string                `json:"aliases,omitempty"`
	BestFor               []string                `json:"best_for,omitempty"`
	CanUseTools           []string                `json:"can_use_tools,omitempty"`
	CostLimits            []string                `json:"cost_limits,omitempty"`
	AssignedCount         int                     `json:"assigned_count"`
	RunningCount          int                     `json:"running_count"`
	ReadyCount            int                     `json:"ready_count"`
	NeedsMeCount          int                     `json:"needs_me_count"`
	LeaseCount            int                     `json:"lease_count"`
	StaleLeaseCount       int                     `json:"stale_lease_count,omitempty"`
	StaleLeaseWorkIDs     []string                `json:"stale_lease_work_ids,omitempty"`
	OldestLeaseAgeSeconds *int64                  `json:"oldest_lease_age_seconds,omitempty"`
	RouteEvidenceCount    int                     `json:"route_evidence_count"`
	RouteEvidencePresent  bool                    `json:"route_evidence_present"`
	WaitReason            string                  `json:"wait_reason,omitempty"`
	LastHookEvent         *ExecutorHookEventRef   `json:"last_hook_event,omitempty"`
	CurrentWork           []ExecutorWorkRef       `json:"current_work,omitempty"`
	Leases                []ExecutorLeaseRef      `json:"leases,omitempty"`
	StalledJobs           []ExecutorStalledJobRef `json:"stalled_jobs,omitempty"`
	LastSuccess           *ExecutorWorkRef        `json:"last_success,omitempty"`
	LastFailure           *ExecutorWorkRef        `json:"last_failure,omitempty"`
	NextAssigned          *ExecutorWorkRef        `json:"next_assigned,omitempty"`
	ProjectionNote        string                  `json:"projection_note,omitempty"`
}

type ExecutorWorkRef struct {
	WorkID           string `json:"work_id"`
	State            string `json:"state"`
	Owner            string `json:"owner,omitempty"`
	ClaimedBy        string `json:"claimed_by,omitempty"`
	ClaimedAt        string `json:"claimed_at,omitempty"`
	Title            string `json:"title"`
	NextAction       string `json:"next_action,omitempty"`
	MoveType         string `json:"move_type,omitempty"`
	ReceiptID        string `json:"receipt_id,omitempty"`
	TransitionID     string `json:"transition_id,omitempty"`
	CanonicalSurface string `json:"canonical_surface,omitempty"`
	NeedsMe          string `json:"needs_me,omitempty"`
}

type ExecutorLeaseRef struct {
	WorkID     string `json:"work_id"`
	By         string `json:"by"`
	ClaimedAt  string `json:"claimed_at"`
	AgeSeconds int64  `json:"age_seconds"`
}

// ExecutorStalledJobRef surfaces a codex companion job that reports itself
// running but whose owning process is not alive (a dead executor). It is a
// projection-only truth signal; the underlying work item is never mutated.
type ExecutorStalledJobRef struct {
	JobID      string `json:"job_id"`
	WorkID     string `json:"work_id,omitempty"`
	Status     string `json:"status"`
	Phase      string `json:"phase,omitempty"`
	PID        int    `json:"pid,omitempty"`
	AgeSeconds int64  `json:"age_seconds"`
	LogPath    string `json:"log_path,omitempty"`
	Reason     string `json:"reason"`
}

type ExecutorHookEventRef struct {
	CreatedAt      string `json:"created_at"`
	Hook           string `json:"hook"`
	Status         string `json:"status"`
	FailOpenReason string `json:"fail_open_reason,omitempty"`
}

type ExecutorsProjectionOptions struct {
	ProbeLocal     bool
	LMStudioClient localmodel.LMStudioClient
	// ProbeJobs opts into the codex job-liveness cross-check: a running-status
	// job whose pid is not alive marks its work item as needing a re-dispatch
	// instead of counting as running.
	ProbeJobs bool
	// JobsStateDir overrides the codex jobs state directory scanned by the
	// probe. Empty falls back to DefaultCodexJobsStateDir (machine-local).
	JobsStateDir string
	// Workbench reuses a caller-built report instead of building a second one
	// (a full Workbench walks the whole event store — the dominant cost of
	// every projection verb). The report must have been built with
	// RecentLimit >= 8: the executor rows read Recent, and a shorter list
	// would silently change which observations they see.
	Workbench *WorkbenchReport
}

type executorProfile struct {
	ID             string
	Label          string
	Aliases        []string
	Status         string
	StatusSource   string
	Tone           string
	BestFor        []string
	CanUseTools    []string
	CostLimits     []string
	ProjectionNote string
}

func (s *Store) ExecutorsProjection(now time.Time, optionSets ...ExecutorsProjectionOptions) (ExecutorsProjection, error) {
	if now.IsZero() {
		now = time.Now()
	}
	profileExecs := os.Getenv("COMPOUND_PROFILE_EXECUTORS") == "1"
	epT := time.Now()
	epLog := func(label string) {
		if profileExecs {
			fmt.Fprintf(os.Stderr, "[EXEC-PROFILE] %s (cum=%s)\n", label, time.Since(epT))
		}
	}
	epLog("setup")
	opts := ExecutorsProjectionOptions{}
	if len(optionSets) > 0 {
		opts = optionSets[0]
	}
	var workbench WorkbenchReport
	if opts.Workbench != nil {
		workbench = *opts.Workbench
	} else {
		built, err := s.Workbench(now, WorkbenchOptions{RecentLimit: 8})
		if err != nil {
			return ExecutorsProjection{}, err
		}
		workbench = built
	}
	leases, err := s.ListLeases()
	if err != nil {
		return ExecutorsProjection{}, err
	}
	epLog("leases")

	projection := ExecutorsProjection{
		SchemaVersion: ExecutorsProjectionSchemaVersion,
		AuthorityRef:  ExecutorsAuthorityRef,
		Status:        "ok",
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		SourceRefs:    []string{"compound.workbench", "compound.work.leases", "compound.executor.profile_guide"},
		NextCommand:   []string{"compound", "executors", "--json"},
		Integrity:     append([]string{}, workbench.Integrity...),
	}

	rows := map[string]*ExecutorRow{}
	profiles := map[string]executorProfile{}
	for _, profile := range executorProfiles() {
		row := ExecutorRow{
			ID:             profile.ID,
			Label:          profile.Label,
			Status:         profile.Status,
			StatusSource:   profile.StatusSource,
			Tone:           profile.Tone,
			Aliases:        append([]string{}, profile.Aliases...),
			BestFor:        append([]string{}, profile.BestFor...),
			CanUseTools:    append([]string{}, profile.CanUseTools...),
			CostLimits:     append([]string{}, profile.CostLimits...),
			ProjectionNote: profile.ProjectionNote,
		}
		rows[row.ID] = &row
		profiles[row.ID] = profile
		projection.Counts.ProfileGuide++
	}

	for _, profile := range profiles {
		row := rows[profile.ID]
		if row == nil {
			continue
		}
		dispatch := workbenchDispatchNext(workbench, executorDispatchActor(profile))
		row.WaitReason = firstNonEmptyExecutorString(dispatch.WaitReason, dispatch.Reason)
	}
	epLog("profile_dispatch")

	// Hook evidence now comes from the authoritative hook_ records via the Scan
	// seam (the hook-events.ndjson compatibility stream is frozen post-migration).
	// The source ref is emitted only when hook records are actually present,
	// preserving the prior "present-and-non-authoritative" contract.
	hookEvents, hookWarnings, hookEventsPresent, err := s.loadExecutorHookEvents()
	if err != nil {
		return ExecutorsProjection{}, err
	}
	epLog("hook_events")
	if hookEventsPresent {
		projection.SourceRefs = appendSourceRef(projection.SourceRefs, "compound.events.hook_events")
		projection.Integrity = append(projection.Integrity, hookWarnings...)
		applyExecutorHookEvents(rows, hookEvents)
	}

	if routeEvidencePathExists(s.home) {
		projection.SourceRefs = appendSourceRef(projection.SourceRefs, "compound.ledger.route_evidence")
		report, warnings, err := loadExecutorRouteEvidence(s.home)
		if err != nil {
			return ExecutorsProjection{}, err
		}
		projection.Integrity = append(projection.Integrity, warnings...)
		applyExecutorRouteEvidence(rows, profiles, report)
	}
	epLog("route_evidence")

	if opts.ProbeLocal {
		applyLocalLMStudioProbe(rows["local_lm_studio"], opts.LMStudioClient)
	}

	deadExecutorWork := map[string]bool{}
	if opts.ProbeJobs {
		jobsDir := strings.TrimSpace(opts.JobsStateDir)
		if jobsDir == "" {
			if d, ok := DefaultCodexJobsStateDir(); ok {
				jobsDir = d
			}
		}
		projection.SourceRefs = appendSourceRef(projection.SourceRefs, ExecutorJobsProbeSourceRef)
		stalled, warnings := scanStalledCodexJobs(jobsDir, now)
		projection.Integrity = append(projection.Integrity, warnings...)
		applyStalledCodexJobs(rows["codex"], stalled, deadExecutorWork)
		projection.Counts.StalledJobs = len(stalled)
	}

	runningStates := map[string]bool{"claimed": true, "running": true, "in_progress": true, "verifying": true}
	readyStates := map[string]bool{"ready": true, "admitted": true, "proposed": true}
	needsMeStates := map[string]bool{"held": true, "blocked": true}
	for _, item := range workbench.Open {
		row := rows[executorIDForActor(executorActor(item))]
		if row == nil {
			row = rows["unassigned"]
		}
		ref := executorWorkRef(item)
		executorDied := runningStates[item.State] && deadExecutorWork[item.WorkID]
		if executorDied {
			ref.NeedsMe = executorDiedNeedsMe
		}
		row.AssignedCount++
		row.CurrentWork = append(row.CurrentWork, ref)
		projection.Counts.Assigned++
		switch {
		case executorDied:
			// A dead executor means the work is not really running; surface it
			// as needing me instead of an inflated running count.
			row.NeedsMeCount++
			projection.Counts.NeedsMe++
		case runningStates[item.State]:
			row.RunningCount++
			projection.Counts.Running++
		case readyStates[item.State]:
			row.ReadyCount++
			projection.Counts.Ready++
		case needsMeStates[item.State]:
			row.NeedsMeCount++
			projection.Counts.NeedsMe++
		}
	}

	for _, lease := range leases {
		row := rows[executorIDForActor(lease.By)]
		if row == nil {
			row = rows["unassigned"]
		}
		if s.leaseWorkTerminal(lease.WorkID) {
			// Terminal-work leases are residue, not active claims; keep them
			// out of the active lease list/count and surface a stale count so
			// `home archive-history --kind lease` sweep debt stays visible.
			row.StaleLeaseCount++
			row.StaleLeaseWorkIDs = append(row.StaleLeaseWorkIDs, lease.WorkID)
			projection.Counts.StaleLeases++
			continue
		}
		ageSeconds := executorAgeSeconds(now, lease.ClaimAt)
		row.LeaseCount++
		row.Leases = append(row.Leases, ExecutorLeaseRef{
			WorkID:     lease.WorkID,
			By:         lease.By,
			ClaimedAt:  lease.ClaimAt,
			AgeSeconds: ageSeconds,
		})
		if row.OldestLeaseAgeSeconds == nil || ageSeconds > *row.OldestLeaseAgeSeconds {
			row.OldestLeaseAgeSeconds = int64Ref(ageSeconds)
		}
		projection.Counts.Leases++
	}
	epLog("leases_loop")

	for _, item := range workbench.Recent {
		row := rows[executorIDForActor(executorActor(item))]
		if row == nil {
			row = rows["unassigned"]
		}
		ref := executorWorkRef(item)
		switch item.State {
		case "completed":
			if row.LastSuccess == nil {
				row.LastSuccess = &ref
			}
		case "failed":
			if row.LastFailure == nil {
				row.LastFailure = &ref
			}
		}
	}

	for _, row := range rows {
		sort.SliceStable(row.CurrentWork, func(i, j int) bool {
			leftRank := executorStateRank(row.CurrentWork[i].State)
			rightRank := executorStateRank(row.CurrentWork[j].State)
			if leftRank != rightRank {
				return leftRank > rightRank
			}
			if row.CurrentWork[i].ClaimedAt != row.CurrentWork[j].ClaimedAt {
				return row.CurrentWork[i].ClaimedAt > row.CurrentWork[j].ClaimedAt
			}
			return row.CurrentWork[i].WorkID < row.CurrentWork[j].WorkID
		})
		sort.SliceStable(row.Leases, func(i, j int) bool {
			if row.Leases[i].ClaimedAt != row.Leases[j].ClaimedAt {
				return row.Leases[i].ClaimedAt > row.Leases[j].ClaimedAt
			}
			return row.Leases[i].WorkID < row.Leases[j].WorkID
		})
		for _, work := range row.CurrentWork {
			if readyStates[work.State] {
				next := work
				row.NextAssigned = &next
				break
			}
		}
		projection.Executors = append(projection.Executors, *row)
	}
	sort.SliceStable(projection.Executors, func(i, j int) bool {
		return executorProfileRank(projection.Executors[i].ID) < executorProfileRank(projection.Executors[j].ID)
	})

	projection.Counts.Total = len(projection.Executors)
	projection.Counts.Open = workbench.Counts.Open

	delegated, delegatedCounts, err := s.reduceDelegatedSessions(workbench.eventsByWork)
	if err != nil {
		return ExecutorsProjection{}, err
	}
	projection.DelegatedSessions = delegated
	projection.DelegatedCounts = delegatedCounts
	projection.SourceRefs = append(projection.SourceRefs, DelegatedSessionsSourceRef)

	return projection, nil
}

type executorHookEventLine struct {
	CreatedAt      string `json:"created_at"`
	Harness        string `json:"harness"`
	HookName       string `json:"hook_name"`
	Hook           string `json:"hook"`
	Status         string `json:"status"`
	FailOpenReason string `json:"fail_open_reason,omitempty"`
}

// loadExecutorHookEvents reads the authoritative hook_ records through the Scan
// seam. present reports whether any hook record exists (the source-ref gate);
// a per-record decode/created_at problem degrades to a warning keyed by record
// id, matching the prior non-authoritative line-warning behavior. applyExecutor-
// HookEvents reduces by max(created_at) per harness, so Scan's record order is
// immaterial. An unreadable record (permission-denied / corrupt segment) is a
// hard error, preserving the attention-unavailable path.
func (s *Store) loadExecutorHookEvents() ([]executorHookEventLine, []string, bool, error) {
	// Presence is non-authoritative enrichment: a missing or malformed events
	// layout (e.g. the scan seam cannot enumerate) is treated as "absent" and the
	// projection proceeds, exactly as the prior lenient existence probe swallowed
	// stat errors. A record that EXISTS but cannot be read is a hard error below —
	// that is the attention-unavailable path.
	// RecentSegments caps the hook family to its most-recent day files
	// (applyExecutorHookEvents keeps only the LATEST event per harness, so a
	// 41-day × ~12MB hook family can be reduced to the most recent few days
	// without changing the projection result on a home whose executors
	// emitted within that window).
	entries, err := s.events.Scan(eventstore.ScanOptions{Prefixes: []string{"hook_"}, RegularOnly: true, RecentSegments: 2})
	if err != nil {
		return nil, nil, false, nil
	}
	if len(entries) == 0 {
		return nil, nil, false, nil
	}
	var out []executorHookEventLine
	var warnings []string
	for _, entry := range entries {
		rec, err := entry.Read()
		if err != nil {
			return nil, nil, true, fmt.Errorf("read hook events: %w", err)
		}
		var ev executorHookEventLine
		if err := json.Unmarshal(rec.Bytes, &ev); err != nil {
			warnings = append(warnings, fmt.Sprintf("hook_events record %s unreadable", entry.ID))
			continue
		}
		if _, err := time.Parse(time.RFC3339, ev.CreatedAt); err != nil {
			warnings = append(warnings, fmt.Sprintf("hook_events record %s missing created_at", entry.ID))
			continue
		}
		out = append(out, ev)
	}
	return out, warnings, true, nil
}

func applyExecutorHookEvents(rows map[string]*ExecutorRow, events []executorHookEventLine) {
	for _, ev := range events {
		row := rows[executorIDForActor(ev.Harness)]
		if row == nil {
			row = rows["unassigned"]
		}
		if row == nil {
			continue
		}
		hook := firstNonEmptyExecutorString(ev.HookName, ev.Hook)
		if row.LastHookEvent != nil && !executorTimestampAfter(ev.CreatedAt, row.LastHookEvent.CreatedAt) {
			continue
		}
		row.LastHookEvent = &ExecutorHookEventRef{
			CreatedAt:      ev.CreatedAt,
			Hook:           hook,
			Status:         ev.Status,
			FailOpenReason: ev.FailOpenReason,
		}
	}
}

type executorRouteEvidenceReport struct {
	RouteEvidenceCount int
	ProviderCounts     map[string]int
}

func loadExecutorRouteEvidence(homePath string) (executorRouteEvidenceReport, []string, error) {
	path := executorRouteEvidencePath(homePath)
	report := executorRouteEvidenceReport{ProviderCounts: map[string]int{}}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return report, nil, nil
		}
		return executorRouteEvidenceReport{}, nil, fmt.Errorf("read route evidence: %w", err)
	}
	defer f.Close()
	var warnings []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev struct {
			SelectedTarget struct {
				Provider string `json:"provider"`
			} `json:"selected_target"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			warnings = append(warnings, fmt.Sprintf("route_evidence line %d unreadable", lineNo))
			continue
		}
		report.RouteEvidenceCount++
		provider := strings.TrimSpace(ev.SelectedTarget.Provider)
		if provider != "" {
			report.ProviderCounts[provider]++
		}
	}
	if err := scanner.Err(); err != nil {
		return executorRouteEvidenceReport{}, warnings, fmt.Errorf("scan route evidence: %w", err)
	}
	return report, warnings, nil
}

func applyExecutorRouteEvidence(rows map[string]*ExecutorRow, profiles map[string]executorProfile, report executorRouteEvidenceReport) {
	row := rows["switchback_api"]
	if row == nil {
		return
	}
	count := routeEvidenceCountForProfile(profiles["switchback_api"], report)
	row.RouteEvidenceCount = count
	row.RouteEvidencePresent = count > 0
}

func routeEvidenceCountForProfile(profile executorProfile, report executorRouteEvidenceReport) int {
	aliases := append([]string{profile.ID}, profile.Aliases...)
	count := 0
	for provider, providerCount := range report.ProviderCounts {
		if executorAliasMatchesAny(provider, aliases) {
			count += providerCount
		}
	}
	if count == 0 {
		return report.RouteEvidenceCount
	}
	return count
}

func applyLocalLMStudioProbe(row *ExecutorRow, client localmodel.LMStudioClient) {
	if row == nil {
		return
	}
	row.StatusSource = "live_probe"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	models, err := client.Models(ctx)
	if err != nil {
		row.Status = "unreachable"
		row.Tone = "red"
		row.ProjectionNote = "LM Studio probe failed: " + err.Error()
		return
	}
	row.Status = "reachable"
	row.Tone = "green"
	if len(models) == 0 {
		row.ProjectionNote = "LM Studio reachable; no models reported"
		return
	}
	row.ProjectionNote = fmt.Sprintf("LM Studio reachable; %d model(s) reported", len(models))
}

// executorDiedNeedsMe is the needs-me card shown on a work item whose executor
// process died while the work still reads as running.
const executorDiedNeedsMe = "executor died — re-dispatch or release"

// applyStalledCodexJobs records stalled jobs on the codex executor row and marks
// their referenced work ids so a running work item with a dead executor can be
// reclassified as needing me. It never mutates work items.
func applyStalledCodexJobs(row *ExecutorRow, stalled []stalledCodexJob, deadWork map[string]bool) {
	for _, job := range stalled {
		for _, id := range job.WorkIDs {
			deadWork[id] = true
		}
		if row == nil {
			continue
		}
		workID := ""
		if len(job.WorkIDs) > 0 {
			workID = job.WorkIDs[0]
		}
		row.StalledJobs = append(row.StalledJobs, ExecutorStalledJobRef{
			JobID:      job.JobID,
			WorkID:     workID,
			Status:     job.Status,
			Phase:      job.Phase,
			PID:        job.PID,
			AgeSeconds: job.AgeSeconds,
			LogPath:    job.LogPath,
			Reason:     job.Reason,
		})
	}
}

// leaseWorkTerminal reports whether a lease's work is no longer live: its work
// is terminal, archived, or missing entirely. Such leases are stale residue,
// excluded from the active lease count; the governed sweep is `home
// archive-history --kind lease`. Mirrors internal/home staleLeaseCandidates.
func (s *Store) leaseWorkTerminal(workID string) bool {
	w, err := s.Get(workID)
	if err != nil {
		return true
	}
	return terminalStates[w.State]
}

func executorProfiles() []executorProfile {
	return []executorProfile{
		{
			ID:           "codex",
			Label:        "Codex",
			Aliases:      []string{"codex"},
			Status:       "available",
			StatusSource: "profile_guide",
			Tone:         "green",
			BestFor:      []string{"code edits", "tests", "local proof", "repo changes"},
			CanUseTools:  []string{"terminal", "files", "browser checks when needed"},
			CostLimits:   []string{"subscription lane", "proof before claim"},
		},
		{
			ID:           "claude_code",
			Label:        "Claude Code",
			Aliases:      []string{"claude", "claude_code", "claude-code"},
			Status:       "manual",
			StatusSource: "profile_guide",
			Tone:         "blue",
			BestFor:      []string{"architecture review", "larger refactors", "prose-heavy synthesis"},
			CanUseTools:  []string{"native Claude Code tool lane"},
			CostLimits:   []string{"subscription lane", "assign intentionally"},
		},
		{
			ID:             "local_lm_studio",
			Label:          "Local LM Studio",
			Aliases:        []string{"lm-studio", "local", "local_model"},
			Status:         "not_checked",
			StatusSource:   "profile_guide",
			Tone:           "gray",
			BestFor:        []string{"cheap private extraction when a model is loaded"},
			CanUseTools:    []string{"model API only"},
			CostLimits:     []string{"local watts", "context limits"},
			ProjectionNote: "availability requires a future model-health probe",
		},
		{
			ID:             "switchback_api",
			Label:          "Switchback/API",
			Aliases:        []string{"switchback", "api", "route_gateway"},
			Status:         "route_evidence_only",
			StatusSource:   "profile_guide",
			Tone:           "violet",
			BestFor:        []string{"provider routing", "cheap extraction", "route evidence"},
			CanUseTools:    []string{"OpenAI-compatible gateway"},
			CostLimits:     []string{"check registry before unattended use"},
			ProjectionNote: "route evidence is not task-success proof",
		},
		{
			ID:           "scripts",
			Label:        "Scripts",
			Aliases:      []string{"script", "scripts", "compound", "compound reviews"},
			Status:       "available",
			StatusSource: "profile_guide",
			Tone:         "green",
			BestFor:      []string{"deterministic preflights", "smoke tests", "projections"},
			CanUseTools:  []string{"local commands only"},
			CostLimits:   []string{"no model spend"},
		},
		{
			ID:           "unassigned",
			Label:        "Unassigned",
			Aliases:      []string{"", "unassigned"},
			Status:       "waiting",
			StatusSource: "workbench",
			Tone:         "orange",
			BestFor:      []string{"ready work that still needs an executor choice"},
			CanUseTools:  []string{"none until claimed"},
			CostLimits:   []string{"assign manually or Continue when safe"},
		},
	}
}

func executorActor(item WorkbenchItem) string {
	return firstNonEmptyExecutorString(item.ClaimedBy, item.Owner)
}

func executorDispatchActor(profile executorProfile) string {
	for _, alias := range profile.Aliases {
		if strings.TrimSpace(alias) != "" {
			return alias
		}
	}
	return profile.ID
}

func executorIDForActor(actor string) string {
	normalized := normalizeExecutorAlias(actor)
	for _, profile := range executorProfiles() {
		for _, alias := range profile.Aliases {
			alias = normalizeExecutorAlias(alias)
			if alias == "" && normalized == "" {
				return profile.ID
			}
			if alias != "" && strings.Contains(normalized, alias) {
				return profile.ID
			}
		}
	}
	return "unassigned"
}

func executorWorkRef(item WorkbenchItem) ExecutorWorkRef {
	return ExecutorWorkRef{
		WorkID:           item.WorkID,
		State:            item.State,
		Owner:            item.Owner,
		ClaimedBy:        item.ClaimedBy,
		ClaimedAt:        item.ClaimedAt,
		Title:            executorWorkTitle(item),
		NextAction:       item.NextAction,
		MoveType:         item.MoveType,
		ReceiptID:        item.ReceiptID,
		TransitionID:     item.TransitionID,
		CanonicalSurface: item.CanonicalSurface,
	}
}

func executorWorkTitle(item WorkbenchItem) string {
	if strings.HasPrefix(item.CanonicalSurface, "packet:") {
		return strings.ReplaceAll(strings.TrimPrefix(item.CanonicalSurface, "packet:"), "-", " ")
	}
	if item.ActionContext != nil && strings.TrimSpace(item.ActionContext.Title) != "" {
		return item.ActionContext.Title
	}
	return firstNonEmptyExecutorString(item.SuccessPredicate, item.DesiredOutcome, item.WorkID)
}

func executorStateRank(state string) int {
	switch state {
	case "running", "in_progress", "verifying", "claimed":
		return 50
	case "ready", "admitted":
		return 40
	case "proposed":
		return 30
	case "held", "blocked":
		return 20
	default:
		return 10
	}
}

func executorProfileRank(id string) int {
	switch id {
	case "codex":
		return 10
	case "claude_code":
		return 20
	case "local_lm_studio":
		return 30
	case "switchback_api":
		return 40
	case "scripts":
		return 50
	case "unassigned":
		return 60
	default:
		return 90
	}
}

func normalizeExecutorAlias(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, " ", "_")
	value = strings.ReplaceAll(value, "-", "_")
	return value
}

func executorAliasMatchesAny(value string, aliases []string) bool {
	normalized := normalizeExecutorAlias(value)
	if normalized == "" {
		return false
	}
	for _, alias := range aliases {
		alias = normalizeExecutorAlias(alias)
		if alias == "" {
			continue
		}
		if strings.Contains(normalized, alias) || strings.Contains(alias, normalized) {
			return true
		}
	}
	return false
}

func executorAgeSeconds(now time.Time, value string) int64 {
	claimedAt, ok := executorClaimedAtTime(value)
	if !ok {
		return 0
	}
	seconds := int64(now.Sub(claimedAt).Seconds())
	if seconds < 0 {
		return 0
	}
	return seconds
}

func executorTimestampAfter(candidate, current string) bool {
	candidateTime, candidateOK := executorClaimedAtTime(candidate)
	currentTime, currentOK := executorClaimedAtTime(current)
	if candidateOK && currentOK {
		return candidateTime.After(currentTime)
	}
	return candidate > current
}

func executorClaimedAtTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, true
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	switch {
	case n > 1e16:
		return time.Unix(0, n), true
	case n > 1e13:
		return time.Unix(0, n*int64(time.Microsecond)), true
	case n > 1e11:
		return time.Unix(0, n*int64(time.Millisecond)), true
	default:
		return time.Unix(n, 0), true
	}
}

func int64Ref(value int64) *int64 {
	v := value
	return &v
}

func routeEvidencePathExists(homePath string) bool {
	return regularFileExists(executorRouteEvidencePath(homePath))
}

func executorRouteEvidencePath(homePath string) string {
	return filepath.Join(homePath, "ledger", "execution-route-evidence.jsonl")
}

func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func appendSourceRef(sourceRefs []string, sourceRef string) []string {
	for _, existing := range sourceRefs {
		if existing == sourceRef {
			return sourceRefs
		}
	}
	return append(sourceRefs, sourceRef)
}

func firstNonEmptyExecutorString(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
