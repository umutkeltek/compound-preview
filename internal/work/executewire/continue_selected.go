package executewire

import (
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/actuator"
	"compound/internal/envelope"
	"compound/internal/work"
)

// LaunchSelectedViaDoor uses the saved tuple, not unattended routing defaults.
// The child rechecks its source generation, policy, adapter, workspace, tuple,
// permission posture and exact running reservation through the public door.
func LaunchSelectedViaDoor(store *work.Store, source envelope.WorkEnvelope, actor string, resume bool, now time.Time) (work.DetachedLaunch, error) {
	if refusal := actuator.GovernedHarnessSourceRefusal(source, resume); refusal != nil {
		return work.DetachedLaunch{}, refusal
	}
	selection := source.ExecutionSelection
	if selection == nil {
		return work.DetachedLaunch{}, fmt.Errorf("selected Continue requires saved execution selection")
	}
	if source.GitDelivery == nil {
		return work.DetachedLaunch{}, fmt.Errorf("selected Continue requires a saved workspace")
	}
	workspace := strings.TrimSpace(source.GitDelivery.TargetWorktreeRoot)
	info, err := os.Stat(workspace)
	if err != nil || !info.IsDir() {
		return work.DetachedLaunch{}, fmt.Errorf("selected workspace is not a usable directory")
	}
	candidate := work.ExecutionRouteCandidate{
		LaunchProfileID: selection.LaunchProfileID, Harness: selection.Harness,
		RequestedModelID: selection.RequestedModelID, ProviderRoute: selection.ProviderRoute,
		SourceRevision: selection.LaunchProfileRevision, SelectionRevision: selection.Revision,
		CapacityLane: selection.ProviderRoute,
	}
	// Exact tuple validation precedes any door process. Adapter-specific
	// admission and fresh permission/resume gates remain owned by --execute.
	if _, err := resolveExecutionRouteCandidate(store.HomePath(), candidate, now); err != nil {
		return work.DetachedLaunch{}, err
	}
	if err := validateSavedSelectionTuple(source, candidate, now); err != nil {
		return work.DetachedLaunch{}, err
	}
	return launchDetachedCandidateViaDoor(store, source.ID, candidate,
		"Continue the saved work objective within its existing constraints. Verify the result and report evidence and unresolved decisions; do not claim acceptance from execution alone.", actor, now, &source)
}
