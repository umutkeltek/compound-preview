package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"compound/internal/authority"
	"compound/internal/decisions"
	"compound/internal/home"
	"compound/internal/work"
)

// Injected seams (package vars) so tests can substitute fake Git plus one
// inventory-wide liveness snapshot.
var worktreeInventoryCLIGit authority.WorktreeRetirementGit = authority.ExecWorktreeRetirementGit{}
var worktreeInventoryCLILiveness authority.WorktreeInventoryLiveness = authority.LSOFWorktreeInventoryLiveness{}

// worktreeInventoryLedger carries the OPTIONAL ledger inputs the inventory joins
// over pure Git ground truth: operator keep annotations and exact nonterminal
// work identity evidence.
type worktreeInventoryLedger struct {
	Keeps         map[string]authority.WorktreeKeepAnnotation
	KeepsReadable bool
	Envelopes     []authority.WorktreeEnvelopeArtifact
}

// worktreeInventoryCLILedger loads keep + identity ledger inputs from the
// resolved COMPOUND_HOME. It is a package var so tests run hermetically (a real
// home is never read in-test); the default reads keeps fail-closed and lists
// nonterminal envelopes best-effort.
var worktreeInventoryCLILedger = defaultWorktreeInventoryLedger

func defaultWorktreeInventoryLedger() worktreeInventoryLedger {
	homePath, err := home.Resolve("")
	if err != nil {
		// Cannot resolve a home ⇒ keeps cannot be read (fail closed) and no
		// identity evidence is available.
		return worktreeInventoryLedger{KeepsReadable: false}
	}
	led := worktreeInventoryLedger{}
	if keeps, ok := decisions.ActiveWorktreeKeeps(homePath); ok {
		led.KeepsReadable = true
		led.Keeps = make(map[string]authority.WorktreeKeepAnnotation, len(keeps))
		for ref, k := range keeps {
			led.Keeps[ref] = authority.WorktreeKeepAnnotation{DecisionID: k.DecisionID, Revisit: k.Revisit}
		}
	}
	led.Envelopes = loadWorktreeInventoryEnvelopeArtifacts(homePath)
	return led
}

// loadWorktreeInventoryEnvelopeArtifacts lists nonterminal work envelopes and
// projects exact branch-artifact and running-main-root identity evidence. A
// read failure yields no evidence (identity is additive and never blocks the
// projection).
func loadWorktreeInventoryEnvelopeArtifacts(homePath string) []authority.WorktreeEnvelopeArtifact {
	store, err := work.Open(homePath)
	if err != nil {
		return nil
	}
	envs, err := store.List()
	if err != nil {
		return nil
	}
	holders := map[string]string{}
	if leases, leaseErr := store.ListLeases(); leaseErr == nil {
		for _, lease := range leases {
			holders[lease.WorkID] = lease.By
		}
	}
	var out []authority.WorktreeEnvelopeArtifact
	for _, w := range envs {
		if worktreeInventoryTerminalState(w.State) {
			continue
		}
		targetRoot := ""
		if w.GitDelivery != nil {
			targetRoot = w.GitDelivery.TargetWorktreeRoot
		}
		matchedBranch := false
		for _, art := range w.ArtifactIDs {
			if ref, ok := parseWorktreeBranchArtifact(art); ok {
				matchedBranch = true
				out = append(out, authority.WorktreeEnvelopeArtifact{
					WorkID: w.ID, State: w.State, Holder: holders[w.ID], BranchRef: ref,
					TargetWorktreeRoot: targetRoot, CreatedAt: w.CreatedAt,
				})
			}
		}
		if targetRoot != "" && !matchedBranch {
			out = append(out, authority.WorktreeEnvelopeArtifact{
				WorkID: w.ID, State: w.State, Holder: holders[w.ID],
				TargetWorktreeRoot: targetRoot, CreatedAt: w.CreatedAt,
			})
		}
	}
	return out
}

// parseWorktreeBranchArtifact extracts refs/heads/<name> from a
// worktree-branch:<ref> artifact. A ref that names no live worktree (e.g. the
// known dangling refs/heads/(pending-dispatch)) still parses here — the pure
// join ignores it because no inventory row matches it.
func parseWorktreeBranchArtifact(artifact string) (string, bool) {
	const prefix = "worktree-branch:"
	artifact = strings.TrimSpace(artifact)
	if !strings.HasPrefix(artifact, prefix) {
		return "", false
	}
	ref := strings.TrimSpace(strings.TrimPrefix(artifact, prefix))
	if !strings.HasPrefix(ref, "refs/heads/") || strings.TrimSpace(strings.TrimPrefix(ref, "refs/heads/")) == "" {
		return "", false
	}
	return ref, true
}

// worktreeInventoryTerminalState mirrors the work state machine's terminal set
// (completed/failed/cancelled): only nonterminal envelopes carry a live lane.
func worktreeInventoryTerminalState(state string) bool {
	switch strings.TrimSpace(state) {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

const executorsWorktreesUsage = "usage: compound executors worktrees [--json] [--repo PATH] [--activity-window-min N]"

func cmdExecutorsWorktrees(args []string) {
	// Inventory liveness owns a short bound around its one open-file snapshot;
	// retain a wider bound around the complete Git + ledger projection.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if code := runExecutorsWorktrees(ctx, args, os.Stdout, os.Stderr, time.Now()); code != 0 {
		os.Exit(code)
	}
}

func runExecutorsWorktrees(ctx context.Context, args []string, stdout, stderr io.Writer, now time.Time) int {
	jsonOut := false
	repo := ""
	windowMin := 60
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "--json":
			jsonOut = true
		case "--repo":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				fmt.Fprintln(stderr, executorsWorktreesUsage)
				return 2
			}
			repo = strings.TrimSpace(args[i])
		case "--activity-window-min":
			i++
			if i >= len(args) {
				fmt.Fprintln(stderr, executorsWorktreesUsage)
				return 2
			}
			n, err := strconv.Atoi(strings.TrimSpace(args[i]))
			if err != nil || n <= 0 {
				fmt.Fprintln(stderr, executorsWorktreesUsage)
				return 2
			}
			windowMin = n
		default:
			fmt.Fprintln(stderr, executorsWorktreesUsage)
			return 2
		}
	}

	if repo == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		repo = wd
	}

	led := worktreeInventoryCLILedger()
	keepsReadable := led.KeepsReadable
	inv, err := authority.BuildWorktreeInventory(ctx, authority.WorktreeInventoryDependencies{
		Git:      worktreeInventoryCLIGit,
		Liveness: worktreeInventoryCLILiveness,
	}, repo, authority.WorktreeInventoryOptions{
		Now:            now,
		ActivityWindow: time.Duration(windowMin) * time.Minute,
		Keeps:          led.Keeps,
		KeepsReadable:  &keepsReadable,
		Envelopes:      led.Envelopes,
	})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(inv); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	printWorktreeInventory(stdout, inv)
	return 0
}

func printWorktreeInventory(w io.Writer, inv authority.WorktreeInventory) {
	fmt.Fprintln(w, "# compound executors worktrees")
	fmt.Fprintf(w, "- repo: %s\n", inv.Repo)
	fmt.Fprintf(w, "- generated_at: %s\n", inv.GeneratedAt)
	fmt.Fprintf(w, "- authority: %s @ %s (%s)\n", inv.MainRef, shortWorktreeSHA(inv.MainTip), inv.AuthorityRef)
	fmt.Fprintf(w, "- counts: total=%d live=%d main_dirty=%d stalled_unlanded=%d landed_clean=%d probe=%d unknown=%d kept=%d scratch_worktree_debt=%d\n",
		inv.Counts.Total, inv.Counts.Live, inv.Counts.MainDirty, inv.Counts.StalledUnlanded, inv.Counts.LandedClean, inv.Counts.Probe, inv.Counts.Unknown, inv.Counts.Kept, inv.Counts.ScratchWorktreeDebt)
	if inv.KeepsReadable != nil && !*inv.KeepsReadable {
		fmt.Fprintln(w, "- keep records unreadable — kept status not shown; retirement stays blocked until keeps can be read")
	}
	fmt.Fprintln(w)

	// The WORK column appears only when at least one row joined exact singular
	// identity or exposed ambiguous candidates, so a ground-truth-only listing
	// stays uncluttered.
	showWork := false
	for _, row := range inv.Rows {
		if row.WorkID != "" || len(row.WorkCandidates) > 0 {
			showWork = true
			break
		}
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	if showWork {
		fmt.Fprintln(tw, "CLASS\tWORKTREE\tWORK\tUNLANDED\tAHEAD\tDIRTY\tAGE\tNEXT ACTION")
		for _, row := range inv.Rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\n",
				row.Class, worktreeRowLabel(row), worktreeWorkLabel(row), row.UnlandedCommits, row.AheadCommits, row.DirtyFiles, worktreeAgeLabel(row.AgeSeconds), row.NextAction)
		}
	} else {
		fmt.Fprintln(tw, "CLASS\tWORKTREE\tUNLANDED\tAHEAD\tDIRTY\tAGE\tNEXT ACTION")
		for _, row := range inv.Rows {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\t%s\n",
				row.Class, worktreeRowLabel(row), row.UnlandedCommits, row.AheadCommits, row.DirtyFiles, worktreeAgeLabel(row.AgeSeconds), row.NextAction)
		}
	}
	tw.Flush()

	fmt.Fprintln(w)
	fmt.Fprintln(w, "reasons:")
	for _, row := range inv.Rows {
		fmt.Fprintf(w, "- %s [%s]: %s\n", worktreeRowLabel(row), row.Class, row.Reason)
	}

	// Scratch-worktree-debt advisory (non-fatal): report the count and the exact
	// per-row sweep suggestion. Follows the archive-history/BackupWarnings pattern
	// — reported, never gates anything. The inventory is read-only; these are
	// copy-paste hints, and the governed retire path remains available per row.
	if inv.Counts.ScratchWorktreeDebt > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "scratch-worktree-debt: %d contained+clean+older-than-48h worktree(s) — sweep:\n", inv.Counts.ScratchWorktreeDebt)
		for _, suggestion := range inv.ScratchWorktreeDebtSweep {
			fmt.Fprintf(w, "  %s\n", suggestion)
		}
		fmt.Fprintln(w, "  (advisory — check lane liveness first; compound authority worktree-retire remains the governed path)")
	}
}

// worktreeWorkLabel renders a row's joined work identity for the text table.
func worktreeWorkLabel(row authority.WorktreeInventoryRow) string {
	if len(row.WorkCandidates) > 0 {
		return fmt.Sprintf("ambiguous (%d candidates)", len(row.WorkCandidates))
	}
	if row.WorkID == "" {
		return "-"
	}
	var details []string
	if row.WorkState != "" {
		details = append(details, row.WorkState)
	}
	if row.WorkHolder != "" {
		details = append(details, "holder="+row.WorkHolder)
	}
	if len(details) == 0 {
		return row.WorkID
	}
	return row.WorkID + " (" + strings.Join(details, "; ") + ")"
}

func worktreeRowLabel(row authority.WorktreeInventoryRow) string {
	if row.IsMain {
		return "(main)"
	}
	if row.Detached {
		return "(detached) " + filepath.Base(row.Path)
	}
	if row.Branch != "" {
		return row.Branch
	}
	return filepath.Base(row.Path)
}

func shortWorktreeSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "-"
	}
	return sha
}

func worktreeAgeLabel(sec *int64) string {
	if sec == nil {
		return "-"
	}
	s := *sec
	switch {
	case s < 90:
		return fmt.Sprintf("%ds", s)
	case s < 90*60:
		return fmt.Sprintf("%dm", s/60)
	case s < 48*3600:
		return fmt.Sprintf("%dh", s/3600)
	default:
		return fmt.Sprintf("%dd", s/86400)
	}
}
