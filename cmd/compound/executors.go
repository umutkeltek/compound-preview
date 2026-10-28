package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/home"
	"compound/internal/localmodel"
	"compound/internal/work"
)

const executorsUsage = "usage: compound executors [--json] [--probe-local] [--lm-studio-url URL] [--probe-jobs] [--jobs-state-dir DIR]\n       compound executors worktrees [--json] [--repo PATH] [--activity-window-min N]"

func cmdExecutors(args []string) {
	if len(args) > 0 && args[0] == "worktrees" {
		cmdExecutorsWorktrees(args[1:])
		return
	}
	jsonOut := false
	probeLocal := false
	probeJobs := false
	lmStudioURL := ""
	jobsStateDir := ""
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "--json":
			jsonOut = true
		case "--probe-local":
			probeLocal = true
		case "--probe-jobs":
			probeJobs = true
		case "--lm-studio-url":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				fmt.Fprintln(os.Stderr, executorsUsage)
				os.Exit(2)
			}
			lmStudioURL = strings.TrimSpace(args[i])
		case "--jobs-state-dir":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				fmt.Fprintln(os.Stderr, executorsUsage)
				os.Exit(2)
			}
			jobsStateDir = strings.TrimSpace(args[i])
		default:
			fmt.Fprintln(os.Stderr, executorsUsage)
			os.Exit(2)
		}
	}

	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	// Read-only by registry contract: the executor projection inspects leases
	// and work logs and must not materialize work/ or events/ just by looking
	// (see work.OpenReadOnly).
	store, err := work.OpenReadOnly(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	opts := work.ExecutorsProjectionOptions{ProbeLocal: probeLocal, ProbeJobs: probeJobs, JobsStateDir: jobsStateDir}
	if probeLocal {
		if lmStudioURL == "" {
			lmStudioURL = localRoutingBaseURL()
		}
		opts.LMStudioClient = localmodel.LMStudioClient{BaseURL: lmStudioURL}
	}
	projection, err := store.ExecutorsProjection(time.Now(), opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	output := buildExecutorsProjectionOutput(projection)

	if jsonOut {
		if err := json.NewEncoder(os.Stdout).Encode(output); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	printExecutorsProjection(output)
}

type executorsProjectionOutput struct {
	work.ExecutorsProjection
	Executors []executorProjectionRow `json:"executors"`
}

type executorProjectionRow struct {
	work.ExecutorRow
	ReadyForAnyCount int                    `json:"ready_for_any_count"`
	ReadyForAny      []work.ExecutorWorkRef `json:"ready_for_any,omitempty"`
}

func buildExecutorsProjectionOutput(projection work.ExecutorsProjection) executorsProjectionOutput {
	sourceRows := projection.Executors
	leasedWork := make(map[string]struct{})
	for _, row := range sourceRows {
		for _, lease := range row.Leases {
			leasedWork[lease.WorkID] = struct{}{}
		}
	}

	readyForAny := make([]work.ExecutorWorkRef, 0)
	readyIDs := make(map[string]struct{})
	for _, row := range sourceRows {
		for _, item := range row.CurrentWork {
			if !isReadyForAnyExecutor(item, leasedWork) {
				continue
			}
			if _, seen := readyIDs[item.WorkID]; seen {
				continue
			}
			readyIDs[item.WorkID] = struct{}{}
			readyForAny = append(readyForAny, item)
		}
	}

	projection.Executors = nil
	projection.Counts.Assigned -= len(readyForAny)
	output := executorsProjectionOutput{
		ExecutorsProjection: projection,
		Executors:           make([]executorProjectionRow, 0, len(sourceRows)),
	}
	for _, sourceRow := range sourceRows {
		row := sourceRow
		row.CurrentWork = make([]work.ExecutorWorkRef, 0, len(sourceRow.CurrentWork))
		removed := 0
		for _, item := range sourceRow.CurrentWork {
			if _, available := readyIDs[item.WorkID]; available {
				removed++
				continue
			}
			row.CurrentWork = append(row.CurrentWork, item)
		}
		row.AssignedCount -= removed
		row.ReadyCount -= removed
		row.NextAssigned = nextReadyExecutorWork(row.CurrentWork)
		if row.ID == "unassigned" && row.NextAssigned == nil && row.WaitReason == "ready_work" {
			row.WaitReason = ""
		}

		projectedRow := executorProjectionRow{ExecutorRow: row}
		if row.ID != "unassigned" {
			projectedRow.ReadyForAnyCount = len(readyForAny)
			projectedRow.ReadyForAny = append([]work.ExecutorWorkRef(nil), readyForAny...)
		}
		output.Executors = append(output.Executors, projectedRow)
	}
	return output
}

func isReadyForAnyExecutor(item work.ExecutorWorkRef, leasedWork map[string]struct{}) bool {
	if item.ClaimedBy != "" {
		return false
	}
	if _, leased := leasedWork[item.WorkID]; leased {
		return false
	}
	switch item.State {
	case "ready", "admitted", "proposed":
		return true
	default:
		return false
	}
}

func nextReadyExecutorWork(items []work.ExecutorWorkRef) *work.ExecutorWorkRef {
	for _, item := range items {
		switch item.State {
		case "ready", "admitted", "proposed":
			next := item
			return &next
		}
	}
	return nil
}

func printExecutorsProjection(projection executorsProjectionOutput) {
	fmt.Println("# compound executors")
	fmt.Printf("- status: %s\n", projection.Status)
	fmt.Printf("- generated_at: %s\n", projection.GeneratedAt)
	fmt.Printf("- counts: total=%d open=%d running=%d ready=%d needs_me=%d leases=%d\n",
		projection.Counts.Total,
		projection.Counts.Open,
		projection.Counts.Running,
		projection.Counts.Ready,
		projection.Counts.NeedsMe,
		projection.Counts.Leases,
	)
	if projection.Counts.StaleLeases > 0 || projection.Counts.StalledJobs > 0 {
		fmt.Printf("- residue: stale_leases=%d stalled_jobs=%d\n", projection.Counts.StaleLeases, projection.Counts.StalledJobs)
	}
	fmt.Printf("- source_refs: %s\n", strings.Join(projection.SourceRefs, ", "))
	printDelegatedSessions(projection.DelegatedSessions, projection.DelegatedCounts)
	for _, executor := range projection.Executors {
		fmt.Printf("\n## %s\n", executor.Label)
		fmt.Printf("- status: %s (%s)\n", executor.Status, executor.StatusSource)
		if executor.WaitReason != "" {
			fmt.Printf("- wait_reason: %s\n", executor.WaitReason)
		}
		if executor.LastHookEvent != nil {
			fmt.Printf("- last_hook_event: %s hook=%s status=%s", executor.LastHookEvent.CreatedAt, executor.LastHookEvent.Hook, executor.LastHookEvent.Status)
			if executor.LastHookEvent.FailOpenReason != "" {
				fmt.Printf(" (%s)", executor.LastHookEvent.FailOpenReason)
			}
			fmt.Println()
		}
		if executor.RouteEvidencePresent || executor.RouteEvidenceCount > 0 {
			fmt.Printf("- route_evidence: present=%t count=%d\n", executor.RouteEvidencePresent, executor.RouteEvidenceCount)
		}
		if executor.OldestLeaseAgeSeconds != nil {
			fmt.Printf("- oldest_lease_age_seconds: %d\n", *executor.OldestLeaseAgeSeconds)
		}
		fmt.Printf("- counts: assigned=%d running=%d ready=%d needs_me=%d leases=%d\n",
			executor.AssignedCount,
			executor.RunningCount,
			executor.ReadyCount,
			executor.NeedsMeCount,
			executor.LeaseCount,
		)
		if executor.StaleLeaseCount > 0 {
			fmt.Printf("- stale_leases: %d\n", executor.StaleLeaseCount)
		}
		if len(executor.BestFor) > 0 {
			fmt.Printf("- best_for: %s\n", strings.Join(executor.BestFor, ", "))
		}
		if len(executor.CanUseTools) > 0 {
			fmt.Printf("- tools: %s\n", strings.Join(executor.CanUseTools, ", "))
		}
		if len(executor.CostLimits) > 0 {
			fmt.Printf("- cost_limits: %s\n", strings.Join(executor.CostLimits, ", "))
		}
		if executor.ProjectionNote != "" {
			fmt.Printf("- note: %s\n", executor.ProjectionNote)
		}
		if executor.NextAssigned != nil {
			fmt.Printf("- next_assigned: %s %s\n", executor.NextAssigned.WorkID, executor.NextAssigned.Title)
		}
		if executor.ReadyForAnyCount > 0 {
			fmt.Printf("- ready_for_any_count: %d\n", executor.ReadyForAnyCount)
			fmt.Println("- ready_for_any:")
			for _, item := range executor.ReadyForAny {
				fmt.Printf("  - %s state=%s title=%q\n", item.WorkID, item.State, item.Title)
			}
		}
		if len(executor.CurrentWork) > 0 {
			fmt.Println("- current_work:")
			for _, item := range executor.CurrentWork {
				fmt.Printf("  - %s state=%s title=%q\n", item.WorkID, item.State, item.Title)
				if item.NeedsMe != "" {
					fmt.Printf("    needs_me: %s\n", item.NeedsMe)
				}
			}
		}
		if len(executor.Leases) > 0 {
			fmt.Println("- leases:")
			for _, lease := range executor.Leases {
				fmt.Printf("  - %s by=%s age_seconds=%d\n", lease.WorkID, lease.By, lease.AgeSeconds)
			}
		}
		if len(executor.StalledJobs) > 0 {
			fmt.Println("- stalled_jobs:")
			for _, job := range executor.StalledJobs {
				fmt.Printf("  - %s work=%s pid=%d age_seconds=%d reason=%q\n", job.JobID, job.WorkID, job.PID, job.AgeSeconds, job.Reason)
			}
		}
		if executor.LastSuccess != nil {
			fmt.Printf("- last_success: %s %s\n", executor.LastSuccess.WorkID, executor.LastSuccess.Title)
		}
		if executor.LastFailure != nil {
			fmt.Printf("- last_failure: %s %s\n", executor.LastFailure.WorkID, executor.LastFailure.Title)
		}
	}
}

// printDelegatedSessions renders the safe supervised-session board in the
// human view. It prints only reduced, privacy-safe fields (lane, session name,
// status, last steer TIME, exact RunAttempt id/status) — never prompt text,
// pane content, argv, or the refusal message.
func printDelegatedSessions(rows []work.DelegatedSessionRow, counts work.DelegatedSessionCounts) {
	fmt.Printf("\n## delegated sessions (%d)\n", counts.Total)
	fmt.Printf("- counts: running=%d exited=%d refused=%d unknown=%d\n",
		counts.Running, counts.Exited, counts.Refused, counts.Unknown)
	if len(rows) == 0 {
		fmt.Println("- none")
		return
	}
	for _, r := range rows {
		fmt.Printf("- %s [%s] work=%s", r.Status, r.Lane, r.WorkID)
		if r.Session != "" {
			fmt.Printf(" session=%s", r.Session)
		}
		if r.RunAttemptID != "" {
			fmt.Printf(" run_attempt=%s(%s)", r.RunAttemptID, r.RunAttemptStatus)
		}
		if r.LastSteerAt != "" {
			fmt.Printf(" last_steer=%s", r.LastSteerAt)
		}
		if r.Progress != nil {
			fmt.Printf(" progress=%d/%d/%d remaining=%d", r.Progress.Done, r.Progress.Empty, r.Progress.Failed, r.Progress.Remaining)
		}
		if r.Status == "refused" && r.RefusedReason != "" {
			fmt.Printf(" reason=%s", r.RefusedReason)
		}
		if r.Status == "exited" && r.ObservedExit != "" {
			fmt.Printf(" observed=%s", r.ObservedExit)
		}
		fmt.Println()
	}
}
