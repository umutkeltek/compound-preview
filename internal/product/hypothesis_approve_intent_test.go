package product

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/eventstore"
)

// approvalTransitionTrail returns every recorded transition that belongs to
// one candidate's approval act, so a test can read the whole receipt trail
// instead of only the id the call handed back. A refused approval returns no
// id at all, and an id-only assertion would therefore prove nothing.
func approvalTransitionTrail(t *testing.T, home, candidateID string) []envelope.Transition {
	t.Helper()
	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.Scan(eventstore.ScanOptions{Prefixes: []string{"tr_"}})
	if err != nil {
		t.Fatal(err)
	}
	trace := "product_hypothesis_approve:" + candidateID
	var trail []envelope.Transition
	for _, entry := range entries {
		record, err := entry.Read()
		if err != nil {
			t.Fatal(err)
		}
		var tr envelope.Transition
		if err := json.Unmarshal(record.Bytes, &tr); err != nil {
			continue
		}
		if tr.SourceTraceID == trace {
			trail = append(trail, tr)
		}
	}
	return trail
}

// claimsCommittedApproval reports whether a transition tells a reader that the
// approval actually landed: either it names the approval act among the refs
// that describe the state AFTER the move, or its summary speaks in the
// committed voice the happy path uses.
func claimsCommittedApproval(tr envelope.Transition) bool {
	for _, ref := range tr.AfterRefs {
		if strings.HasPrefix(ref, "product_hypothesis_approve:") {
			return true
		}
	}
	return strings.Contains(tr.StateDelta, "approved by") ||
		strings.Contains(tr.StateDelta, "expected_effect written")
}

// TestHypothesisApproveRefusedGenerationRecordsNoCommittedApproval is the
// approval-intent falsifier, and it asserts BOTH halves of one invariant: when
// a concurrent writer wins the candidate generation, (a) that writer's row
// survives byte-for-byte and (b) nothing in the recorded trail reports a
// committed approval. Half (a) alone was already true before the intent split:
// the candidate bytes were protected while a success-shaped transition —
// "approved ... expected_effect written" — stayed on the record for an approval
// the CAS had refused. Protecting bytes is half the invariant; the receipt has
// to be honest too.
func TestHypothesisApproveRefusedGenerationRecordsNoCommittedApproval(t *testing.T) {
	home := t.TempDir()
	productID := "product:synth-notes"
	changeEvent := "v2.4 ships a new onboarding screen"
	candidateID := mintHypothesisDraftFixture(t, home, productID, changeEvent, []string{"synthetic_first_time_downloads"})
	candidatePath := filepath.Join(home, candidates.InboxDir(candidates.ProductHypothesis), candidateID+".json")

	// The concurrent writer lands exactly once, inside the window the approval
	// opens between its gate read and its candidate write.
	var racedBytes []byte
	restore := swapRecordLoopTransition(func(homePath string, tr envelope.Transition) error {
		if err := RecordTransition(homePath, tr); err != nil {
			return err
		}
		if racedBytes != nil {
			return nil
		}
		gateRead, err := os.ReadFile(candidatePath)
		if err != nil {
			return err
		}
		var raced HypothesisCandidate
		if err := json.Unmarshal(gateRead, &raced); err != nil {
			return err
		}
		raced.Verdict = candidates.Reject
		raced.NextAction = "a concurrent review verdict won the window"
		if err := writeHypothesisCandidate(homePath, raced); err != nil {
			return err
		}
		racedBytes, err = os.ReadFile(candidatePath)
		return err
	})
	defer restore()

	res, err := HypothesisApprove(home, HypothesisApproveInput{
		CandidateID:    candidateID,
		ExpectedEffect: tApproveSentence,
		ReviewedBy:     "sandbox-operator",
	}, tProduct)
	if err == nil {
		t.Fatalf("a lost generation race must refuse the approval, got %+v", res)
	}
	if !strings.Contains(err.Error(), "changed since the gate read") {
		t.Fatalf("the refusal must name the generation change, got: %v", err)
	}
	if res.ApprovalID != "" || res.TransitionID != "" {
		t.Fatalf("a refused approval must not hand back approval identity, got %+v", res)
	}
	if racedBytes == nil {
		t.Fatal("the concurrent writer never ran: the race window was never opened")
	}

	// Half (a): the concurrent writer's row stands, byte-for-byte.
	after, readErr := os.ReadFile(candidatePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(racedBytes) {
		t.Fatalf("the concurrent writer's candidate must survive byte-for-byte:\nwant %s\ngot  %s", racedBytes, after)
	}

	// Half (b): the receipt trail reads "intent, then refusal" and never
	// reports a committed approval.
	trail := approvalTransitionTrail(t, home, candidateID)
	var intents, refusals int
	for _, tr := range trail {
		if claimsCommittedApproval(tr) {
			t.Fatalf("a refused approval reported a committed approval: after_refs=%v summary=%q", tr.AfterRefs, tr.StateDelta)
		}
		switch {
		case strings.Contains(tr.StateDelta, "approval intent"):
			intents++
		case strings.Contains(tr.StateDelta, "approval refused"):
			refusals++
		}
	}
	if intents != 1 || refusals != 1 {
		t.Fatalf("a refused approval must read as one intent plus one refusal, got intents=%d refusals=%d trail=%d", intents, refusals, len(trail))
	}
}

// TestHypothesisApproveRecordsIntentThenCommittedOutcome keeps the uncontended
// path honest in the other direction: the intent is recorded first and the
// committed outcome — the transition the result hands back — is recorded only
// after the CAS actually replaced the candidate.
func TestHypothesisApproveRecordsIntentThenCommittedOutcome(t *testing.T) {
	home := t.TempDir()
	productID := "product:synth-notes"
	changeEvent := "v2.4 ships a new onboarding screen"
	candidateID := mintHypothesisDraftFixture(t, home, productID, changeEvent, []string{"synthetic_first_time_downloads"})

	res, err := HypothesisApprove(home, HypothesisApproveInput{
		CandidateID:    candidateID,
		ExpectedEffect: tApproveSentence,
		ReviewedBy:     "sandbox-operator",
	}, tProduct)
	if err != nil {
		t.Fatal(err)
	}

	var intent, committed *envelope.Transition
	trail := approvalTransitionTrail(t, home, candidateID)
	for i := range trail {
		switch {
		case strings.Contains(trail[i].StateDelta, "approval intent"):
			intent = &trail[i]
		case claimsCommittedApproval(trail[i]):
			committed = &trail[i]
		}
	}
	if intent == nil || committed == nil {
		t.Fatalf("an uncontended approval must record an intent and a committed outcome, got trail=%d", len(trail))
	}
	if claimsCommittedApproval(*intent) {
		t.Fatalf("the intent must not read as a committed approval: after_refs=%v summary=%q", intent.AfterRefs, intent.StateDelta)
	}
	if committed.ID != res.TransitionID {
		t.Fatalf("the returned transition id %q must be the committed outcome %q", res.TransitionID, committed.ID)
	}
	if !containsRef(committed.AfterRefs, "product_hypothesis_approve:"+res.ApprovalID) {
		t.Fatalf("the committed outcome must name the approval act, got after_refs=%v", committed.AfterRefs)
	}
}
