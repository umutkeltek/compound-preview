package work

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/runattempt"
)

// collaborationNativePinnedEvidencePrefix is the only native-session evidence
// this boundary accepts: the id was PINNED on the launch argv by this runtime
// (pinned:--session-id on the Claude-family adapters). A derived or
// unobservable id says someone noticed a session, not that Compound launched
// it under governance, so it can never bind a recipient here.
const collaborationNativePinnedEvidencePrefix = "pinned:"

// resolveNativeCollaborationRecipient is the third recipient path: a session
// THIS runtime launched under a governed execution. It binds no registry and
// mutates nothing -- it reads the launch's own immutable evidence.
//
//	native — one OriginHarnessExecution attempt whose copied per-run
//	         selection snapshot pins this exact session id on this harness,
//	         whose durable launch sidecar records a COMMITTED start, whose
//	         work is still running under the exact ActionIntent/PolicyDecision
//	         reservation that authorized the launch, and whose actor holds the
//	         work lease.
//
// The path resolves BEFORE the RunAttempt forward-link exists on the envelope:
// the whole point is that a hook inside a freshly launched session is
// addressable immediately, without mutating the work to attach the attempt.
// Nothing here may write; attachment is the finalize path's job.
//
// Every refusal below returns a nonempty UnresolvedReason rather than silent
// absence: a session this runtime pinned is KNOWN, so a legacy Stop bridge must
// not fall back to prose for it (see CollaborationRecipientResolution).
func (s *Store) resolveNativeCollaborationRecipient(
	query CollaborationRecipientQuery,
	cwd string,
	workByID map[string]envelope.WorkEnvelope,
	live []runattempt.RunAttempt,
) (CollaborationRecipientResolution, bool, error) {
	refuse := func(reason string) (CollaborationRecipientResolution, bool, error) {
		return CollaborationRecipientResolution{UnresolvedReason: reason}, false, nil
	}
	var pinned []string
	for _, snapshot := range live {
		selection := snapshot.ExecutionSelection
		if snapshot.Origin == runattempt.OriginHarnessExecution && selection != nil &&
			strings.TrimSpace(selection.NativeSessionID) == query.SessionID &&
			normalizeCollaborationHarness(selection.Harness) == query.Harness &&
			normalizeCollaborationHarness(snapshot.Harness) != query.Harness {
			return refuse("the native session snapshot conflicts with its execution harness")
		}
		if snapshot.Origin == runattempt.OriginHarnessExecution && nativeSessionPinned(snapshot, query) {
			pinned = append(pinned, snapshot.ID)
		}
	}
	switch len(pinned) {
	case 0:
		return CollaborationRecipientResolution{}, false, nil
	case 1:
	default:
		return refuse("multiple live harness executions pin the exact native session")
	}
	// The list is a point-in-time snapshot and carries no launch sidecars.
	// Hydrate the one candidate through the exact record so the durable
	// prepared|committed|start_error boundary is read, not assumed.
	attempt, err := s.runAttempts().Get(pinned[0])
	if err != nil {
		return CollaborationRecipientResolution{}, false, err
	}
	if attempt.Origin != runattempt.OriginHarnessExecution || !nativeSessionPinned(attempt, query) {
		return refuse("the pinned native session no longer matches its launch record")
	}
	if reason := nativeLaunchBoundaryRefusal(attempt); reason != "" {
		return refuse(reason)
	}
	if attempt.Status != runattempt.StatusRunning {
		return refuse("the native session's harness execution is not running")
	}
	item, ok := workByID[attempt.WorkID]
	if !ok {
		return refuse("the native session's work is no longer live")
	}
	if item.State != "running" {
		return refuse("the native session's work is " + item.State + ", not running")
	}
	// The launch must still be authorized by the reservation that minted it:
	// a superseded, canceled, or otherwise replaced authority generation
	// refuses here even though the record still says running.
	posture := attempt.ExecutionPosture
	if attempt.ActionIntentID == "" || posture == nil || strings.TrimSpace(posture.PolicyDecisionID) == "" {
		return refuse("the native session's launch records no exact governing authority")
	}
	if err := validateExpectedExecutionReservation(item, "running", posture.PolicyDecisionID, attempt.ActionIntentID); err != nil {
		return refuse("the native session's governing authority changed: " + err.Error())
	}
	workspace, err := canonicalCollaborationPath(attempt.Workspace)
	if err != nil || !collaborationPathContains(workspace, cwd) {
		return refuse("the native session is not bound to this workspace")
	}
	actor, err := s.collaborationRecipientActor(attempt)
	if err != nil {
		return CollaborationRecipientResolution{}, false, err
	}
	if actor == "" {
		return refuse("the native session's authorizing action names no actor")
	}
	lease, err := s.readLease(attempt.WorkID)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return CollaborationRecipientResolution{}, false, err
		}
		return refuse("the native session's actor holds no lease on the work")
	}
	if lease.WorkID != attempt.WorkID || strings.TrimSpace(lease.By) != actor {
		return refuse("the native session's actor does not hold the work lease")
	}
	// The exact CURRENT policy reference, not a scan of every historical
	// pol_ event: this runs on every hook fire, and the posture already names
	// the one decision that authorized this launch.
	var decision envelope.PolicyDecision
	if err := s.readEventJSON(posture.PolicyDecisionID, &decision); err != nil {
		return CollaborationRecipientResolution{}, false, fmt.Errorf("native recipient policy evidence %s: %w", posture.PolicyDecisionID, err)
	}
	if err := decision.Validate(); err != nil {
		return CollaborationRecipientResolution{}, false, fmt.Errorf("native recipient policy evidence %s: %w", posture.PolicyDecisionID, err)
	}
	if decision.ID != posture.PolicyDecisionID || decision.IDs.WorkID != attempt.WorkID || decision.ActionIntentID != attempt.ActionIntentID || !policy.Allowed(decision) {
		return refuse("the native session's policy decision does not allow its action")
	}
	resolution := collaborationRecipientCoordinates(item, attempt)
	resolution.Actor = actor
	resolution.PolicyRefs = appendCollaborationRef(resolution.PolicyRefs, decision.ID)
	sort.Strings(resolution.PolicyRefs)
	return resolution, true, nil
}

// nativeLaunchBoundaryRefusal names the launch-side reason an attempt cannot
// carry a recipient yet. A prepared-only record is the startup race: the
// process may or may not have started, so this boundary refuses while the
// committed boundary on the same attempt may deliver later.
func nativeLaunchBoundaryRefusal(attempt runattempt.RunAttempt) string {
	if attempt.Launch == nil || attempt.Launch.PreparedAt == nil {
		return "the native session's launch has no durable prepared boundary"
	}
	if attempt.Launch.StartErrorAt != nil {
		return "the native session's launch recorded a start error"
	}
	if attempt.Launch.CommittedAt == nil {
		return "the native session's launch is prepared but not committed"
	}
	return ""
}

// nativeSessionPinned reports whether an attempt's immutable per-run selection
// snapshot pins exactly this session id on exactly this harness.
func nativeSessionPinned(attempt runattempt.RunAttempt, query CollaborationRecipientQuery) bool {
	snapshot := attempt.ExecutionSelection
	if snapshot == nil {
		return false
	}
	if strings.TrimSpace(snapshot.NativeSessionID) != query.SessionID {
		return false
	}
	if !strings.HasPrefix(snapshot.NativeSessionEvidence, collaborationNativePinnedEvidencePrefix) {
		return false
	}
	if normalizeCollaborationHarness(snapshot.Harness) != query.Harness || normalizeCollaborationHarness(attempt.Harness) != query.Harness {
		return false
	}
	if err := snapshot.Validate(); err != nil {
		return false
	}
	return true
}

// fenceKnownNativeSession is the Stop-only history fence. A terminal or
// reservation-lost attempt remains known even without a forward-link or a live
// work envelope. The runattempt owner probes session IDs before hydrating
// matched launch evidence; ordinary hooks never scan historical sidecars.
func (s *Store) fenceKnownNativeSession(query CollaborationRecipientQuery) (bool, error) {
	attempts, err := s.runAttempts().ListByNativeSessionID(query.SessionID)
	if err != nil {
		return false, err
	}
	for _, attempt := range attempts {
		if attempt.Origin == runattempt.OriginHarnessExecution && nativeSessionPinned(attempt, query) {
			return true, nil
		}
	}
	return false, nil
}
