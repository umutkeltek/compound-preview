package work

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/runattempt"
)

// CollaborationRecipientQuery is the native adapter evidence available before
// a hook executes. It intentionally contains only harness-owned coordinates;
// actor/work/run/scope values are resolved from existing Compound state.
type CollaborationRecipientQuery struct {
	Harness   string
	SessionID string
	Cwd       string
	// CheckPriorBindings is for a Stop consumer about to use a legacy prose
	// bridge. Only if normal resolution is absent, inspect prior delegate
	// evidence before permitting that fallback. Ordinary hooks leave it false:
	// the additional work-log snapshot is history-sized, not a hot-path lookup.
	CheckPriorBindings bool
}

// CollaborationRecipientResolution is the work owner's read model for one
// active native recipient. It is reference-only and creates no registry.
type CollaborationRecipientResolution struct {
	// UnresolvedReason is nonempty when known typed evidence cannot resolve.
	// With ok=false, an empty reason means no matching typed binding exists;
	// callers with a legacy bridge must never downgrade an unresolved binding.
	UnresolvedReason string
	Actor            string
	WorkID           string
	RunAttemptID     string
	ScopeRefs        []string
	DependencyRefs   []string
	PolicyRefs       []string
	BaseRevisions    []string
	ResourceRefs     []string
}

// ResolveCollaborationRecipient binds a native session/cwd/harness payload to
// one exact recipient, by either of two paths. Session identity is conjunctive
// authority on both: workspace/harness proximity never adopts an unknown native
// session.
//
//	spawned  — an active, attached harness-execution RunAttempt whose live
//	           delegate binding matches this exact session. Strongest evidence:
//	           this runtime launched the session, so it knows who it is.
//	declared — a self-announced identity corroborated by an independently held
//	           fresh lease (resolveDeclaredCollaborationRecipient below).
//
// The declared path exists because the spawned path can only ever see sessions
// Compound started. Measured 2026-07-31 against a live home: 533 delegated
// sessions, all exited or refused, zero addressable -- while a dozen leases were
// held under distinct named actors that were provably present and mutating the
// ledger. Keying addressability to spawn provenance made the agents doing the
// contending precisely the ones that could not be reached.
func (s *Store) ResolveCollaborationRecipient(query CollaborationRecipientQuery) (CollaborationRecipientResolution, bool, error) {
	query.Harness = normalizeCollaborationHarness(query.Harness)
	query.SessionID = strings.TrimSpace(query.SessionID)
	query.Cwd = strings.TrimSpace(query.Cwd)
	if query.Harness == "" || query.SessionID == "" || query.Cwd == "" {
		return CollaborationRecipientResolution{UnresolvedReason: "recipient coordinates are incomplete"}, false, nil
	}
	cwd, err := canonicalCollaborationPath(query.Cwd)
	if err != nil {
		return CollaborationRecipientResolution{}, false, err
	}
	works, err := s.List()
	if err != nil {
		return CollaborationRecipientResolution{}, false, err
	}
	workByID := make(map[string]envelope.WorkEnvelope, len(works))
	for _, item := range works {
		if terminalStates[item.State] {
			continue
		}
		workByID[item.ID] = item
	}
	// Runs inside every live PreToolUse hook: only live attempts can be a
	// recipient, so terminal records are skipped before sidecar validation.
	attempts, err := s.runAttempts().ListNonTerminalByStatus()
	if err != nil {
		return CollaborationRecipientResolution{}, false, err
	}
	type candidate struct {
		attempt      runattempt.RunAttempt
		work         envelope.WorkEnvelope
		exactSession bool
	}
	candidates := make([]candidate, 0)
	unsettledSession := false
	for _, attempt := range attempts {
		if attempt.Origin != runattempt.OriginHarnessExecution ||
			normalizeCollaborationHarness(attempt.Harness) != query.Harness {
			continue
		}
		item, ok := workByID[attempt.WorkID]
		if !ok || !collaborationContains(item.RunAttemptIDs, attempt.ID) {
			continue
		}
		workspace, err := canonicalCollaborationPath(attempt.Workspace)
		if err != nil || !collaborationPathContains(workspace, cwd) {
			continue
		}
		binding, alive, err := s.latestDelegateBinding(item.ID)
		if err != nil {
			return CollaborationRecipientResolution{}, false, err
		}
		exactSession := alive && binding != nil &&
			binding.Session == query.SessionID &&
			binding.RunAttemptID == attempt.ID &&
			normalizeCollaborationHarness(binding.Harness) == query.Harness
		if binding != nil && binding.Session == query.SessionID &&
			normalizeCollaborationHarness(binding.Harness) == query.Harness && !exactSession {
			unsettledSession = true
		}
		candidates = append(candidates, candidate{attempt: attempt, work: item, exactSession: exactSession})
	}
	exact := candidates[:0]
	for _, item := range candidates {
		if item.exactSession {
			exact = append(exact, item)
		}
	}
	if len(exact) == 1 {
		return s.collaborationRecipientResolution(exact[0].work, exact[0].attempt)
	}
	if len(exact) > 1 {
		return CollaborationRecipientResolution{UnresolvedReason: "multiple active spawned bindings match the exact session"}, false, nil
	}
	if unsettledSession {
		return CollaborationRecipientResolution{UnresolvedReason: "the known spawned session has no exact active binding"}, false, nil
	}
	// No delegate binding. A session this runtime LAUNCHED under a governed
	// execution is still addressable before the envelope forward-link exists:
	// the launch's own pinned snapshot, committed start boundary, and current
	// running reservation are the authority. A known-but-inactive native
	// session returns a reason, never silence, so no bridge may adopt it.
	native, ok, err := s.resolveNativeCollaborationRecipient(query, cwd, workByID, attempts)
	if err != nil || ok || native.UnresolvedReason != "" {
		return native, ok, err
	}
	// No spawned and no native binding. A session Compound never launched may
	// still identify itself, but only conjunctively -- see below.
	declared, ok, err := s.resolveDeclaredCollaborationRecipient(query, cwd, workByID, time.Now())
	if err != nil || ok || declared.UnresolvedReason != "" || !query.CheckPriorBindings {
		return declared, ok, err
	}
	return s.resolveAbsentCollaborationRecipient(query)
}

// resolveAbsentCollaborationRecipient is a Stop compatibility fence, not an
// alternate source of delivery authority. A terminal or unattached attempt is
// intentionally absent from the live-attempt inventory, but its owned session
// binding must not disappear and let prose authorize a continuation. Fold one
// work-event snapshot with the same latest-binding rules as the live path. No
// historical RunAttempt or launch sidecar is read here, and this fence never
// converts inactive evidence into a resolved recipient.
func (s *Store) resolveAbsentCollaborationRecipient(query CollaborationRecipientQuery) (CollaborationRecipientResolution, bool, error) {
	byWork, err := s.eventsByWorkID()
	if err != nil {
		return CollaborationRecipientResolution{}, false, err
	}
	for workID, events := range byWork {
		known := false
		binding, _, err := s.latestDelegateBindingFromEvents(workID, events, func(spawn DelegateBinding) {
			if spawn.Session == query.SessionID && normalizeCollaborationHarness(spawn.Harness) == query.Harness {
				known = true
			}
		})
		if err != nil {
			return CollaborationRecipientResolution{}, false, err
		}
		if binding != nil && binding.Session == query.SessionID &&
			normalizeCollaborationHarness(binding.Harness) == query.Harness {
			return CollaborationRecipientResolution{UnresolvedReason: "the known spawned session has no exact active binding"}, false, nil
		}
		if known {
			return CollaborationRecipientResolution{UnresolvedReason: "the known spawned session binding was superseded"}, false, nil
		}
	}
	// Same fence for a native session this runtime launched: an inactive or
	// superseded pinned session is still KNOWN, so prose must not adopt it.
	// Only the Stop consumer reaches here, so this read is history-sized by
	// the same argument as the delegate fold above.
	nativeKnown, err := s.fenceKnownNativeSession(query)
	if err != nil {
		return CollaborationRecipientResolution{}, false, err
	}
	if nativeKnown {
		return CollaborationRecipientResolution{UnresolvedReason: "the known native session has no exact active binding"}, false, nil
	}
	return CollaborationRecipientResolution{}, false, nil
}

// resolveDeclaredCollaborationRecipient is the path for a session this runtime
// did not spawn. It upholds the same conjunctive rule as the binding path
// above: a self-declaration is a claim any local process could make, so it
// adopts nothing on its own, and proximity still adopts nothing on its own.
// Both must hold.
//
//	declaration  — an exact (harness, session, cwd) announcement exists
//	corroboration — that actor independently holds a fresh lease
//	unambiguity  — exactly one non-terminal work, as the binding path requires
//
// Corroboration deliberately reads ONLY leases, not the work-event log. This
// runs on every hook fire for every session, so a tail read of the log here
// would put megabytes of I/O on the hottest path in the system. Leases are also
// the stricter signal: an actor with event history but no lease has no work
// context, hence no WorkID to deliver against, so requiring a lease is not just
// cheap but necessary. `compound presence` may therefore show an actor
// corroborated by events alone while resolution still declines it -- resolution
// is the stricter surface, deliberately.
//
// The returned RunAttemptID is empty because no harness-execution attempt
// exists. That is load-bearing rather than incidental: deliveryRelevant gates
// the interrupt class on a non-empty RunAttemptID, so a declared recipient can
// only ever receive next_boundary delivery -- injected at its next prompt,
// never mid-turn. The least-privilege outcome falls out of the data model
// instead of needing its own switch.
func (s *Store) resolveDeclaredCollaborationRecipient(
	query CollaborationRecipientQuery,
	cwd string,
	workByID map[string]envelope.WorkEnvelope,
	now time.Time,
) (CollaborationRecipientResolution, bool, error) {
	// Key exactly as AnnouncePresenceSession does. It normalizes harness, trims
	// the session, and canonicalizes cwd before hashing, so looking up with a
	// raw path silently misses every record -- on macOS /var and /private/var
	// hash differently. Normalizing here rather than trusting the caller keeps
	// the helper correct on its own terms instead of depending on an unstated
	// precondition that only today's single caller happens to satisfy.
	query.Cwd = cwd
	announcement, found, err := s.exactPresenceSession(query)
	if err != nil {
		return CollaborationRecipientResolution{UnresolvedReason: err.Error()}, false, nil
	}
	if !found {
		return CollaborationRecipientResolution{}, false, nil
	}
	actor := strings.TrimSpace(announcement.Actor)

	leases, err := s.ListLeases()
	if err != nil {
		return CollaborationRecipientResolution{}, false, err
	}
	corroborated := false
	held := make([]envelope.WorkEnvelope, 0, 1)
	for _, lease := range leases {
		if strings.TrimSpace(lease.By) != actor {
			continue
		}
		if at, ok := parsePresenceTime(lease.ClaimAt); ok && now.Sub(at) <= DefaultPresenceTTL {
			corroborated = true
		}
		if item, ok := workByID[lease.WorkID]; ok {
			held = append(held, item)
		}
	}
	// A lease that has gone cold is not corroboration: it would let an actor
	// that stopped weeks ago keep validating a session forever.
	if !corroborated {
		return CollaborationRecipientResolution{UnresolvedReason: "the exact session declaration has no fresh corroborating lease"}, false, nil
	}
	// Exactly one, mirroring the binding path. Two candidate works means we
	// cannot say which context a message belongs to, and guessing would deliver
	// one lane's coordination into another's.
	if len(held) != 1 {
		return CollaborationRecipientResolution{UnresolvedReason: "the exact session declaration does not hold one unambiguous nonterminal work"}, false, nil
	}
	return s.collaborationRecipientResolution(held[0], runattempt.RunAttempt{Actor: actor})
}

// collaborationRecipientCoordinates folds one work envelope's reference-only
// coordinates into a recipient read model. It is pure: both the delegate and
// the native path build on it, so the two paths can never disagree about what
// a recipient's own coordinates are. Policy references beyond the work's
// current decision stay the caller's concern -- the delegate path scans the
// event log for correlated decisions, the native path pins the exact current
// one -- so neither path inherits the other's I/O profile.
func collaborationRecipientCoordinates(item envelope.WorkEnvelope, attempt runattempt.RunAttempt) CollaborationRecipientResolution {
	resolution := CollaborationRecipientResolution{
		WorkID: item.ID, RunAttemptID: attempt.ID,
		ScopeRefs: []string{}, DependencyRefs: []string{}, PolicyRefs: []string{},
		BaseRevisions: []string{}, ResourceRefs: []string{},
	}
	resolution.ScopeRefs = appendCollaborationRef(resolution.ScopeRefs, item.CanonicalSurface)
	resolution.ScopeRefs = appendCollaborationRef(resolution.ScopeRefs, item.TargetSurface)
	resolution.PolicyRefs = appendCollaborationRef(resolution.PolicyRefs, item.PolicyDecisionID)
	resolution.BaseRevisions = appendCollaborationRef(resolution.BaseRevisions, item.ExpectedBaseSHA)
	if item.GitDelivery != nil {
		resolution.BaseRevisions = appendCollaborationRef(resolution.BaseRevisions, item.GitDelivery.ExpectedBaseSHA)
	}
	for _, ref := range item.EvidenceRefs {
		switch {
		case strings.HasPrefix(ref, "scope:"):
			resolution.ScopeRefs = appendCollaborationRef(resolution.ScopeRefs, ref)
		case strings.HasPrefix(ref, "dependency:"):
			resolution.DependencyRefs = appendCollaborationRef(resolution.DependencyRefs, ref)
		case strings.HasPrefix(ref, "pol_"), strings.HasPrefix(ref, "policy:"), strings.HasPrefix(ref, "compound://registries/"):
			resolution.PolicyRefs = appendCollaborationRef(resolution.PolicyRefs, ref)
		case strings.HasPrefix(ref, "git:"), strings.HasPrefix(ref, "base:"):
			resolution.BaseRevisions = appendCollaborationRef(resolution.BaseRevisions, ref)
		case strings.HasPrefix(ref, "resource:"):
			resolution.ResourceRefs = appendCollaborationRef(resolution.ResourceRefs, ref)
		}
	}
	return resolution
}

func (s *Store) collaborationRecipientResolution(item envelope.WorkEnvelope, attempt runattempt.RunAttempt) (CollaborationRecipientResolution, bool, error) {
	actor, err := s.collaborationRecipientActor(attempt)
	if err != nil {
		return CollaborationRecipientResolution{}, false, err
	}
	resolution := collaborationRecipientCoordinates(item, attempt)
	resolution.Actor = actor
	if attempt.ActionIntentID != "" {
		events, err := eventstore.Open(s.home)
		if err != nil {
			return CollaborationRecipientResolution{}, false, err
		}
		entries, err := events.Scan(eventstore.ScanOptions{Prefixes: []string{"pol_"}})
		if err != nil {
			return CollaborationRecipientResolution{}, false, err
		}
		for _, entry := range entries {
			record, err := entry.Read()
			if err != nil {
				return CollaborationRecipientResolution{}, false, err
			}
			var decision envelope.PolicyDecision
			if err := json.Unmarshal(record.Bytes, &decision); err != nil {
				return CollaborationRecipientResolution{}, false, fmt.Errorf("resolve recipient policy %s: %w", entry.ID, err)
			}
			if err := decision.Validate(); err != nil {
				return CollaborationRecipientResolution{}, false, fmt.Errorf("resolve recipient policy %s: %w", entry.ID, err)
			}
			if decision.IDs.WorkID == item.ID && decision.ActionIntentID == attempt.ActionIntentID {
				resolution.PolicyRefs = appendCollaborationRef(resolution.PolicyRefs, decision.ID)
			}
		}
	}
	sort.Strings(resolution.ScopeRefs)
	sort.Strings(resolution.DependencyRefs)
	sort.Strings(resolution.PolicyRefs)
	sort.Strings(resolution.BaseRevisions)
	sort.Strings(resolution.ResourceRefs)
	return resolution, true, nil
}

func normalizeCollaborationHarness(value string) string {
	return strings.ReplaceAll(strings.TrimSpace(value), "_", "-")
}

// collaborationRecipientActor is the actor an attempt's own evidence
// authorizes as the recipient.
//
// A governed attempt's authority is its authorizing ActionIntent: that record
// names who Compound asked to act. The harness name on the attempt is a lane
// label, not a person -- `compound work advance --execute` mints the attempt
// with the harness family as its actor -- so reading the actor from the
// attempt would hand every governed recipient the same identity. An attempt
// with no authorizing ActionIntent is authorized by the actor that minted it
// (the delegate path mints with the lane holder).
//
// The action must name the attempt's own work. An action that authorizes a
// different work cannot speak for this attempt, so it is refused rather than
// borrowed.
func (s *Store) collaborationRecipientActor(attempt runattempt.RunAttempt) (string, error) {
	if strings.TrimSpace(attempt.ActionIntentID) == "" {
		return strings.TrimSpace(attempt.Actor), nil
	}
	var action envelope.ActionIntent
	if err := s.readEventJSON(attempt.ActionIntentID, &action); err != nil {
		return "", fmt.Errorf("collaboration recipient action evidence %s: %w", attempt.ActionIntentID, err)
	}
	if err := action.Validate(); err != nil {
		return "", fmt.Errorf("collaboration recipient action evidence %s: %w", attempt.ActionIntentID, err)
	}
	if action.ID != attempt.ActionIntentID {
		return "", fmt.Errorf("collaboration recipient action evidence %s does not match the attempt", attempt.ActionIntentID)
	}
	if strings.TrimSpace(action.IDs.WorkID) != strings.TrimSpace(attempt.WorkID) {
		return "", fmt.Errorf("collaboration recipient action %s does not authorize work %s", action.ID, attempt.WorkID)
	}
	actor := strings.TrimSpace(action.Actor)
	if actor == "" {
		return "", fmt.Errorf("collaboration recipient action %s names no actor", action.ID)
	}
	return actor, nil
}

func canonicalCollaborationPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("collaboration recipient workspace is empty")
	}
	path, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path, nil
}

func collaborationPathContains(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func collaborationContains(values []string, want string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

func appendCollaborationRef(values []string, ref string) []string {
	ref = strings.TrimSpace(ref)
	if ref == "" || collaborationContains(values, ref) {
		return values
	}
	return append(values, ref)
}
