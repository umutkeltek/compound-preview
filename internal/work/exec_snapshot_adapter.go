// Package work: thin adapter that maps a runattempt.Store to the
// RunAttemptLookup seam RenderAttemptSnapshotLines reads. The adapter
// lives in internal/work so cmd/compound does not have to import the
// runattempt types just to render a per-attempt snapshot line.
//
// work_r7a_execution_selection_20260911.
package work

import (
	"compound/internal/runattempt"
)

// RunAttemptStoreLookup implements RunAttemptLookup on top of a
// runattempt.Store. Constructed by the cmd/compound caller; never used
// inside internal/work (the work package owns the text format, the
// adapter owns the concrete type).
//
// work_r7a_execution_selection_20260911.
type RunAttemptStoreLookup struct {
	Store *runattempt.Store
}

// Get reads the attempt by id and projects its ExecutionSelection
// snapshot to the bounded RunAttemptSnapshotLine shape the renderer
// reads. A nil snapshot stays nil (HasSnapshot=false), and a read
// error surfaces verbatim — the renderer renders "(unreadable: …)".
func (l *RunAttemptStoreLookup) Get(id string) (RunAttemptSnapshotLine, error) {
	if l == nil || l.Store == nil {
		return RunAttemptSnapshotLine{}, nil
	}
	ra, err := l.Store.Get(id)
	if err != nil {
		return RunAttemptSnapshotLine{}, err
	}
	if ra.ExecutionSelection == nil {
		return RunAttemptSnapshotLine{ID: id, HasSnapshot: false}, nil
	}
	snap := *ra.ExecutionSelection
	return RunAttemptSnapshotLine{
		ID:                id,
		HasSnapshot:       true,
		Source:            snap.Source,
		SelectionRevision: snap.SelectionRevision,
		LaunchProfileID:   snap.LaunchProfileID,
		Harness:           snap.Harness,
		ProviderRoute:     snap.ProviderRoute,
		RequestedModelID:  snap.RequestedModelID,
		SelectionDrift:    snap.SelectionDrift,
	}, nil
}
