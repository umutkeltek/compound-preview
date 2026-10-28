// Package work CLI helpers — moved out of cmd/compound/work_cli.go so the
// cmd/compound LOC budget keeps its headroom. These helpers are the
// CLI-facing seams for the execution-selection contract; they wrap the
// store-level SaveExecutionSelection / ResolveSelection with the
// validated flag-parse path the dispatch handler calls.
//
// work_r7a_execution_selection_20260911.
package work

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"compound/internal/envelope"
)

// SaveExecutionSelectionFromFlags resolves a CLI flag pair into an
// explicit save on the work envelope. Resolver failure or CAS mismatch
// is returned as a non-nil error; the caller surfaces it verbatim.
//
// work_r7a_execution_selection_20260911.
func SaveExecutionSelectionFromFlags(
	store *Store,
	workID string,
	launchProfileID string,
	effortOverride string,
	expectedSelectionRevision string,
	actor string,
	now time.Time,
) (envelope.WorkEnvelope, error) {
	launchProfileID = strings.TrimSpace(launchProfileID)
	if launchProfileID == "" {
		return envelope.WorkEnvelope{}, fmt.Errorf("save execution selection: --launch-profile is required")
	}
	sel, err := ResolveSelection(ExecutionSelectionResolveRequest{
		HomePath:        store.HomePath(),
		LaunchProfileID: launchProfileID,
		EffortOverride:  effortOverride,
		Now:             now,
	}, actor, now)
	if err != nil {
		return envelope.WorkEnvelope{}, fmt.Errorf("resolve %q: %w", launchProfileID, err)
	}
	expectedRev := 0
	if v := strings.TrimSpace(expectedSelectionRevision); v != "" {
		n, parseErr := strconv.Atoi(v)
		if parseErr != nil {
			return envelope.WorkEnvelope{}, fmt.Errorf("--expected-selection-revision %q must be an integer: %w", v, parseErr)
		}
		expectedRev = n
	}
	return store.SaveExecutionSelection(workID, expectedRev, sel, actor, now)
}

// RunCommandAfterAdvance wires SaveExecutionSelectionFromFlags into the post-advance block of `compound work advance`: when --launch-profile is set, the saved selection is replaced (revision +1 + lifecycle event) AFTER the lifecycle advance has succeeded. Returns the new envelope (with the selection field set) when a save happened, or the original envelope when --launch-profile was absent.
//
// work_r7a_execution_selection_20260911.
func RunCommandAfterAdvance(
	store *Store,
	w envelope.WorkEnvelope,
	launchProfile string,
	effort string,
	expectedSelectionRevision string,
	actor string,
	now time.Time,
) (envelope.WorkEnvelope, error) {
	if strings.TrimSpace(launchProfile) == "" {
		return w, nil
	}
	return SaveExecutionSelectionFromFlags(store, w.ID, launchProfile, effort, expectedSelectionRevision, actor, now)
}

// ResolveExecutionSelectionFromFlags resolves --launch-profile and --effort into the populated ExecutionSelection the BuildWork call persists. Used by `compound work create` / `create-plan`.
//
// work_r7a_execution_selection_20260911.
func ResolveExecutionSelectionFromFlags(
	store *Store,
	launchProfileID string,
	effortOverride string,
	actor string,
	now time.Time,
) (*envelope.ExecutionSelection, error) {
	launchProfileID = strings.TrimSpace(launchProfileID)
	if launchProfileID == "" {
		return nil, nil
	}
	sel, err := ResolveSelection(ExecutionSelectionResolveRequest{
		HomePath:        store.HomePath(),
		LaunchProfileID: launchProfileID,
		EffortOverride:  effortOverride,
		Now:             now,
	}, actor, now)
	if err != nil {
		return nil, err
	}
	return &sel, nil
}

// RenderSavedSelection renders the saved selection as a one-glance human-readable block.
//
// work_r7a_execution_selection_20260911.
func RenderSavedSelection(sel envelope.ExecutionSelection) string {
	var b strings.Builder
	fmt.Fprintf(&b, "launch_profile: %s\n", sel.LaunchProfileID)
	if sel.LaunchProfileRevision != "" {
		fmt.Fprintf(&b, "launch_profile_revision: %s\n", sel.LaunchProfileRevision)
	}
	fmt.Fprintf(&b, "harness: %s\n", sel.Harness)
	if sel.ProviderRoute != "" {
		fmt.Fprintf(&b, "provider_route: %s\n", sel.ProviderRoute)
	}
	if sel.RequestedModelID != "" {
		fmt.Fprintf(&b, "requested_model: %s\n", sel.RequestedModelID)
	}
	if sel.RequestedEffort != "" {
		fmt.Fprintf(&b, "requested_effort: %s\n", sel.RequestedEffort)
	}
	fmt.Fprintf(&b, "source: %s\n", sel.Source)
	fmt.Fprintf(&b, "revision: %d\n", sel.Revision)
	fmt.Fprintf(&b, "selected_by: %s\n", sel.SelectedBy)
	fmt.Fprintf(&b, "selected_at: %s\n", sel.SelectedAt)
	return b.String()
}

// RenderSavedSelectionBlock emits the "--- execution_selection ---" header plus the saved-selection body. Returns "" when no selection.
//
// work_r7a_execution_selection_20260911.
func RenderSavedSelectionBlock(w envelope.WorkEnvelope) string {
	if w.ExecutionSelection == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("--- execution_selection (saved: what will run) ---\n")
	b.WriteString(RenderSavedSelection(*w.ExecutionSelection))
	return b.String()
}

// RenderWorkSelection is the top-level helper `compound work show` calls. It concatenates the saved-selection block (when present) and the per-attempt snapshot block (when attempts are attached).
//
// work_r7a_execution_selection_20260911.
func RenderWorkSelection(w envelope.WorkEnvelope, lookup RunAttemptLookup) string {
	var b strings.Builder
	b.WriteString(RenderSavedSelectionBlock(w))
	if len(w.RunAttemptIDs) > 0 {
		b.WriteString(RenderAttemptSnapshotLines(w, lookup))
	}
	return b.String()
}

// RunAttemptSnapshotLine is the minimal shape RenderAttemptSnapshotLines reads from each attempt.
//
// work_r7a_execution_selection_20260911.
type RunAttemptSnapshotLine struct {
	ID                string
	HasSnapshot       bool
	Source            string
	SelectionRevision int
	LaunchProfileID   string
	Harness           string
	ProviderRoute     string
	RequestedModelID  string
	SelectionDrift    string
	Unreadable        string
}

// RunAttemptLookup is the seam the cmd/compound caller wires to its runattempt store.
type RunAttemptLookup interface {
	Get(id string) (RunAttemptSnapshotLine, error)
}

// RenderAttemptSnapshotLines walks the work envelope's RunAttemptIDs and prints one human-readable line per attempt.
//
// work_r7a_execution_selection_20260911.
func RenderAttemptSnapshotLines(w envelope.WorkEnvelope, lookup RunAttemptLookup) string {
	if len(w.RunAttemptIDs) == 0 || lookup == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("--- attempts ---\n")
	for _, ratt := range w.RunAttemptIDs {
		snap, err := lookup.Get(ratt)
		if err != nil {
			fmt.Fprintf(&b, "- %s: (unreadable: %v)\n", ratt, err)
			continue
		}
		if snap.Unreadable != "" {
			fmt.Fprintf(&b, "- %s: %s\n", ratt, snap.Unreadable)
			continue
		}
		if !snap.HasSnapshot {
			fmt.Fprintf(&b, "- %s: (no snapshot)\n", ratt)
			continue
		}
		if rev := snap.SelectionRevision; rev > 0 {
			fmt.Fprintf(&b, "- %s: source=%s selection_rev=%d profile=%s harness=%s route=%s model=%s\n",
				snap.ID, snap.Source, rev, snap.LaunchProfileID, snap.Harness, snap.ProviderRoute, snap.RequestedModelID)
		} else {
			fmt.Fprintf(&b, "- %s: source=%s profile=%s harness=%s route=%s model=%s\n",
				snap.ID, snap.Source, snap.LaunchProfileID, snap.Harness, snap.ProviderRoute, snap.RequestedModelID)
		}
		if snap.SelectionDrift != "" {
			fmt.Fprintf(&b, "    selection_drift: %s\n", snap.SelectionDrift)
		}
	}
	return b.String()
}
