package act

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/policy"
	"compound/internal/runattempt"
)

const credentialPasswordAlignmentFenceSchema = "compound.credential_password_alignment_fence.v0.1"

// This immutable native launch-intent event is not an outcome. It is published
// under admission before OS start and must be read back by the launch guard.
// Its scoped prefix avoids enumerating unrelated or historical RunAttempts.
type credentialPasswordAlignmentFence struct {
	envelope.Header
	AttemptID                string                                    `json:"run_attempt_id"`
	ActionID                 string                                    `json:"action_intent_id"`
	PolicyID                 string                                    `json:"policy_decision_id"`
	OriginalActionID         string                                    `json:"original_action_intent_id"`
	RequestSHA256            string                                    `json:"request_sha256"`
	Request                  policy.CredentialPasswordAlignmentRequest `json:"request"`
	PreviousRecoveryActionID string                                    `json:"previous_recovery_action_id"`
}

func newCredentialPasswordAlignmentFence(p CredentialPasswordAlignmentPlan, ai envelope.ActionIntent, pd envelope.PolicyDecision, attempt string, now time.Time) credentialPasswordAlignmentFence {
	return credentialPasswordAlignmentFence{Header: header(credentialPasswordAlignmentFenceSchema, idFor("cpaf_", attempt), ai, now), AttemptID: attempt, ActionID: ai.ID, PolicyID: pd.ID, OriginalActionID: credentialPasswordAlignOriginalAction(p.Request, ai.ID), RequestSHA256: p.RequestSHA256, Request: p.Request}
}

func credentialPasswordAlignmentReadFence(h, id string) (credentialPasswordAlignmentFence, runattempt.RunAttempt, error) {
	var f credentialPasswordAlignmentFence
	var a runattempt.RunAttempt
	bad := func() (credentialPasswordAlignmentFence, runattempt.RunAttempt, error) {
		return f, a, credentialPasswordAlignError("attempt_fence_unavailable")
	}
	v := &CredentialPasswordAligner{home: h, now: time.Now}
	if v.event(id, &f) != nil || f.SchemaVersion != credentialPasswordAlignmentFenceSchema || f.ID != id || f.ID != idFor("cpaf_", f.AttemptID) || f.Request.Operation == "inspect" || f.RequestSHA256 != policy.CredentialPasswordAlignmentDigest(f.Request) || f.OriginalActionID != credentialPasswordAlignOriginalAction(f.Request, f.ActionID) {
		return bad()
	}
	var ai envelope.ActionIntent
	var pd envelope.PolicyDecision
	if v.event(f.ActionID, &ai) != nil || v.event(f.PolicyID, &pd) != nil || ai.Validate() != nil || pd.Validate() != nil || !ai.RequiresExactApproval || ai.IDs.WorkID != f.IDs.WorkID || ai.Target.Kind != policy.CredentialPasswordAlignTarget || ai.Target.Path != f.Request.File.Path || ai.Destination != policy.CredentialVerificationDestination(f.Request.CredentialVerificationRequest) || ai.ArgumentsHash != "sha256:"+policy.CredentialPasswordAlignmentActionDigest(f.Request) || ai.Capability != policy.CredentialPasswordAlignmentCapability(f.Request) || ai.Action != policy.CredentialPasswordAlignmentAction(f.Request) || policy.NewExactActionApprovalStore(h).ValidateConsumedDecision(ai, pd) != nil {
		return bad()
	}
	if ai.Target.ID != f.Request.ProfileID || ai.SchemaRevision != policy.CredentialPasswordAlignSchema || ai.MutationClass != "security" || ai.Reversibility != "manual" || ai.BlastRadius != "host" || !slices.Contains(ai.EvidenceRefs, "request-sha256:"+f.RequestSHA256) {
		return bad()
	}
	var e error
	a, e = runattempt.Open(h).Get(f.AttemptID)
	if e != nil || a.WorkID != f.IDs.WorkID || a.ActionIntentID != f.ActionID {
		return bad()
	}
	return f, a, nil
}

// The target installer receives only safe native-derived acceptances. It never
// opens the remote private SCRAM pre/postimages.
type credentialPasswordAlignmentAcceptance struct {
	ActionID          string `json:"action_intent_id"`
	OriginalActionID  string `json:"original_action_intent_id"`
	RequestSHA256     string `json:"request_sha256"`
	ObservationSHA256 string `json:"observation_sha256"`
	Operation         string `json:"operation"`
	Effect            string `json:"effect"`
}

type credentialPasswordAlignmentNativeOutcome struct {
	NativeProof bool
	Request     policy.CredentialPasswordAlignmentRequest
	Observation CredentialPasswordAlignmentObservation
	Receipt     envelope.Receipt
}

func credentialPasswordAlignmentReadOutcome(h string, a runattempt.RunAttempt) (credentialPasswordAlignmentNativeOutcome, error) {
	var out credentialPasswordAlignmentNativeOutcome
	bad := func() (credentialPasswordAlignmentNativeOutcome, error) {
		return out, credentialPasswordAlignError("native_outcome_chain")
	}
	if !runattempt.IsTerminal(a.Status) || a.Launch == nil || a.Launch.CommittedAt == nil || a.Launch.StartErrorAt != nil {
		return bad()
	}
	v := &CredentialPasswordAligner{home: h, now: time.Now}
	var ai envelope.ActionIntent
	var receipt envelope.Receipt
	var tr envelope.Transition
	if v.event(a.ActionIntentID, &ai) != nil || v.event(a.TerminalizationReceiptID, &receipt) != nil || v.event(a.TerminalizationTransitionID, &tr) != nil || ai.Validate() != nil || receipt.Validate() != nil || tr.Validate() != nil || ai.IDs.WorkID != a.WorkID || receipt.IDs.WorkID != a.WorkID || tr.IDs.WorkID != a.WorkID || !slices.Contains(tr.ActionRefs, ai.ID) || !slices.Contains(tr.AfterRefs, receipt.ID) || !slices.Contains(receipt.ProofRefs, a.ID) {
		return bad()
	}
	store, e := NewArtifactStore(h)
	if e != nil {
		return bad()
	}
	var status struct {
		SchemaVersion    string                                    `json:"schema_version"`
		Request          policy.CredentialPasswordAlignmentRequest `json:"request"`
		State            string                                    `json:"state"`
		Outcome          string                                    `json:"outcome"`
		RequestSHA256    string                                    `json:"request_sha256"`
		ActionID         string                                    `json:"action_intent_id"`
		PolicyID         string                                    `json:"policy_decision_id"`
		AttemptID        string                                    `json:"run_attempt_id"`
		ObservationRef   string                                    `json:"observation_ref"`
		ApprovalConsumed bool                                      `json:"approval_consumed"`
	}
	seen, matches := map[string]bool{}, 0
	for _, ref := range receipt.ProofRefs {
		if !strings.HasPrefix(ref, "artifact:sha256:") || seen[ref] {
			continue
		}
		seen[ref] = true
		raw, e := store.Get(ref)
		if e != nil {
			return bad()
		}
		var header struct {
			SchemaVersion string `json:"schema_version"`
		}
		if json.Unmarshal(raw, &header) != nil {
			return bad()
		}
		if header.SchemaVersion == "compound.credential_password_alignment_status.v0.1" {
			if !credentialVerifyUniqueJSON(raw) || json.Unmarshal(raw, &status) != nil {
				return bad()
			}
			matches++
		}
	}
	if matches != 1 || !status.ApprovalConsumed || status.Request.ProgramSHA256 != CredentialPasswordAlignmentProgramSHA256() || status.RequestSHA256 != policy.CredentialPasswordAlignmentDigest(status.Request) || ai.ArgumentsHash != "sha256:"+policy.CredentialPasswordAlignmentActionDigest(status.Request) || ai.Capability != policy.CredentialPasswordAlignmentCapability(status.Request) || ai.Action != policy.CredentialPasswordAlignmentAction(status.Request) || status.ActionID != ai.ID || status.AttemptID != a.ID || a.TerminalizationOwner != credentialPasswordAlignOwnerFor(status.Request.Operation) || status.Outcome != receipt.Outcome {
		return bad()
	}
	var pd envelope.PolicyDecision
	if v.event(status.PolicyID, &pd) != nil || policy.NewExactActionApprovalStore(h).ValidateConsumedDecision(ai, pd) != nil {
		return bad()
	}
	out.Request, out.Receipt = status.Request, receipt
	out.NativeProof = true
	if a.Status != runattempt.StatusSucceeded || receipt.Outcome != "completed" || status.State != "completed" {
		return bad()
	}
	if !slices.Contains(receipt.ProofRefs, status.ObservationRef) {
		return bad()
	}
	raw, e := store.Get(status.ObservationRef)
	if e != nil {
		return bad()
	}
	out.Observation, e = decodeCredentialPasswordAlignmentObservation(raw, status.Request)
	if e != nil || out.Observation.ActionID != ai.ID {
		return bad()
	}
	return out, nil
}

type credentialPasswordAlignmentFencedAttempt struct {
	Fence   credentialPasswordAlignmentFence
	Attempt runattempt.RunAttempt
}

func (v *CredentialPasswordAligner) alignmentAttempts(path, destination, skipAction string) ([]credentialPasswordAlignmentFencedAttempt, error) {
	events, e := eventstore.Open(v.home)
	if e != nil {
		return nil, credentialPasswordAlignError("attempt_fence_unavailable")
	}
	scan := v.fenceScan
	if scan == nil {
		// Enumeration returns the complete ID set without decoding unrelated
		// history. Each ID is still freshly read and validated below.
		scan = func(eventstore.ScanOptions) (eventstore.ScanPage, error) {
			names, err := events.EnumerateIDs("cpaf_", false)
			if err != nil {
				return eventstore.ScanPage{}, err
			}
			entries := make([]eventstore.Entry, 0, len(names))
			for _, id := range names {
				entries = append(entries, eventstore.Entry{ID: id})
			}
			return eventstore.ScanPage{Entries: entries}, nil
		}
	}
	page, e := scan(eventstore.ScanOptions{Prefixes: []string{"cpaf_"}, Cursor: v.fenceCursor})
	if e != nil {
		return nil, credentialPasswordAlignError("attempt_fence_unavailable")
	}
	// Only discovery is optimized. A cursor is never authority for a fence
	// or attempt: every known ID is read and validated below on every check.
	// Union even non-resumed pages; a stale cursor may re-serve a superset.
	ids := make(map[string]struct{}, len(v.fenceIDs)+len(page.Entries))
	for id := range v.fenceIDs {
		ids[id] = struct{}{}
	}
	for _, entry := range page.Entries {
		ids[entry.ID] = struct{}{}
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	var matching []credentialPasswordAlignmentFencedAttempt
	for _, id := range ordered {
		// Never take the current attempt's lock from inside CommitLaunch's
		// callback. The guard separately rechecks its immutable fence bytes.
		if skipAction != "" {
			var peek credentialPasswordAlignmentFence
			if v.event(id, &peek) != nil {
				return nil, credentialPasswordAlignError("attempt_fence_unavailable")
			}
			if peek.ActionID == skipAction {
				continue
			}
		}
		f, a, e := credentialPasswordAlignmentReadFence(v.home, id)
		if e != nil {
			return nil, e
		}
		if f.Request.File.Path == path && policy.CredentialVerificationDestination(f.Request.CredentialVerificationRequest) == destination && f.ActionID != skipAction {
			matching = append(matching, credentialPasswordAlignmentFencedAttempt{f, a})
		}
	}
	v.fenceCursor, v.fenceIDs = page.Cursor, ids
	return matching, nil
}

func credentialPasswordAlignmentDidNotStart(a runattempt.RunAttempt) bool {
	return runattempt.IsTerminal(a.Status) && (a.Launch == nil || (a.Launch.StartErrorAt != nil && a.Launch.CommittedAt == nil))
}

func (v *CredentialPasswordAligner) priorAlignmentFence(r policy.CredentialPasswordAlignmentRequest, action string) error {
	if r.Operation != "align" {
		return nil
	}
	all, e := v.alignmentAttempts(r.File.Path, policy.CredentialVerificationDestination(r.CredentialVerificationRequest), action)
	if e != nil {
		return e
	}
	for _, entry := range all {
		if !credentialPasswordAlignmentDidNotStart(entry.Attempt) {
			return credentialPasswordAlignError("prior_alignment_requires_reconciliation")
		}
	}
	return nil
}

// Recovery operations share one derived Work. Their immutable predecessor chain
// is minted under native admission; neither wall clocks nor invented Work
// RunAttempt forward links determine which recovery came last.
func credentialPasswordAlignmentLatestRecovery(h, origin string, all []credentialPasswordAlignmentFencedAttempt) (*credentialPasswordAlignmentFencedAttempt, error) {
	chain, e := credentialPasswordAlignmentRecoveryChain(origin, all)
	if e != nil {
		return nil, e
	}
	var latest *credentialPasswordAlignmentFencedAttempt
	for i := range chain {
		entry := chain[i]
		if !runattempt.IsTerminal(entry.Attempt.Status) {
			return nil, credentialPasswordAlignError("unresolved_alignment")
		}
		if !credentialPasswordAlignmentDidNotStart(entry.Attempt) {
			copy := entry
			latest = &copy
		}
	}
	return latest, nil
}

func credentialPasswordAlignmentRecoveryChain(origin string, all []credentialPasswordAlignmentFencedAttempt) ([]credentialPasswordAlignmentFencedAttempt, error) {
	byAction := map[string]credentialPasswordAlignmentFencedAttempt{}
	next := map[string]string{}
	for _, entry := range all {
		if entry.Fence.OriginalActionID != origin || entry.Fence.Request.Operation == "align" {
			continue
		}
		o := entry.Fence.Request.Origin
		if o == nil || entry.Attempt.WorkID != credentialPasswordAlignRecoveryWorkID(o.ActionID, o.AttemptID) {
			return nil, credentialPasswordAlignError("recovery_work_binding")
		}
		previous := entry.Fence.PreviousRecoveryActionID
		if _, duplicate := next[previous]; duplicate {
			return nil, credentialPasswordAlignError("recovery_chain_branch")
		}
		byAction[entry.Fence.ActionID] = entry
		next[previous] = entry.Fence.ActionID
	}
	var ordered []credentialPasswordAlignmentFencedAttempt
	seen := map[string]bool{}
	previous := ""
	for {
		action, ok := next[previous]
		if !ok {
			break
		}
		entry, ok := byAction[action]
		if !ok || seen[action] {
			return nil, credentialPasswordAlignError("recovery_chain_invalid")
		}
		seen[action] = true
		ordered = append(ordered, entry)
		previous = action
	}
	if len(ordered) != len(byAction) {
		return nil, credentialPasswordAlignError("recovery_chain_incomplete")
	}
	return ordered, nil
}

func (v *CredentialPasswordAligner) previousRecovery(r policy.CredentialPasswordAlignmentRequest) (string, error) {
	if r.Origin == nil {
		return "", nil
	}
	all, e := v.alignmentAttempts(r.File.Path, policy.CredentialVerificationDestination(r.CredentialVerificationRequest), "")
	if e != nil {
		return "", e
	}
	chain, e := credentialPasswordAlignmentRecoveryChain(r.Origin.ActionID, all)
	if e != nil {
		return "", e
	}
	if len(chain) == 0 {
		return "", nil
	}
	return chain[len(chain)-1].Fence.ActionID, nil
}

func (v *CredentialPasswordAligner) priorRecoveryFence(r policy.CredentialPasswordAlignmentRequest, action string) error {
	if r.Origin == nil {
		return nil
	}
	all, e := v.alignmentAttempts(r.File.Path, policy.CredentialVerificationDestination(r.CredentialVerificationRequest), action)
	if e != nil {
		return e
	}
	latest, e := credentialPasswordAlignmentLatestRecovery(v.home, r.Origin.ActionID, all)
	if e != nil {
		return e
	}
	if latest != nil && r.Operation == "restore" {
		outcome, e := credentialPasswordAlignmentReadOutcome(v.home, latest.Attempt)
		if e != nil || outcome.Request.Operation != "reconcile" {
			return credentialPasswordAlignError("restore_requires_prior_reconciliation")
		}
	}
	return nil
}

func credentialPasswordAlignmentInstallerFence(h, path, destination string, qualificationCreatedAt time.Time) ([]credentialPasswordAlignmentAcceptance, error) {
	return (&CredentialPasswordAligner{home: h}).installerFence(path, destination, qualificationCreatedAt)
}

func (v *CredentialPasswordAligner) installerFence(path, destination string, qualificationCreatedAt time.Time) ([]credentialPasswordAlignmentAcceptance, error) {
	h := v.home
	all, e := v.alignmentAttempts(path, destination, "")
	if e != nil {
		return nil, e
	}
	groups := map[string]credentialPasswordAlignmentFencedAttempt{}
	for _, entry := range all {
		if credentialPasswordAlignmentDidNotStart(entry.Attempt) {
			continue
		}
		if !runattempt.IsTerminal(entry.Attempt.Status) {
			return nil, credentialPasswordAlignError("unresolved_alignment")
		}
		if entry.Fence.Request.Operation == "align" {
			groups[entry.Fence.ActionID] = entry
		}
	}
	var result []credentialPasswordAlignmentAcceptance
	for origin, original := range groups {
		latest, e := credentialPasswordAlignmentLatestRecovery(h, origin, all)
		if e != nil {
			return nil, e
		}
		selected := original
		if latest != nil {
			selected = *latest
		}
		outcome, e := credentialPasswordAlignmentReadOutcome(h, selected.Attempt)
		if e != nil {
			return nil, credentialPasswordAlignError("unresolved_alignment")
		}
		if policy.CredentialPasswordAlignmentDigest(outcome.Request) != selected.Fence.RequestSHA256 {
			return nil, credentialPasswordAlignError("alignment_outcome_request_binding")
		}
		created, e := time.Parse(time.RFC3339Nano, outcome.Receipt.CreatedAt)
		if e != nil || !qualificationCreatedAt.After(created) {
			return nil, credentialPasswordAlignError("fresh_unchanged_verifier_after_alignment_required")
		}
		o := outcome.Observation
		result = append(result, credentialPasswordAlignmentAcceptance{ActionID: o.ActionID, OriginalActionID: o.OriginalActionID, RequestSHA256: o.RequestSHA256, ObservationSHA256: credentialPasswordAlignmentObservationDigest(o), Operation: o.Operation, Effect: o.Effect})
	}
	for _, entry := range all {
		if !credentialPasswordAlignmentDidNotStart(entry.Attempt) {
			if _, ok := groups[entry.Fence.OriginalActionID]; !ok {
				return nil, credentialPasswordAlignError("unresolved_alignment")
			}
		}
	}
	slices.SortFunc(result, func(a, b credentialPasswordAlignmentAcceptance) int {
		return strings.Compare(a.OriginalActionID, b.OriginalActionID)
	})
	return result, nil
}
