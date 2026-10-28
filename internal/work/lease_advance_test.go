package work

// Advance-gate lease enforcement. Store.Close got the lease-holder gate; Advance was the
// last ungated mutation verb left — a non-holder actor could walk a claimed envelope
// running->verifying->done-adjacent states, corrupting the holder's liveness signal. These
// tests pin the falsifier list f1-f7 for the Advance gate, mirroring lease_close_test.go's
// shape for Close.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Falsifier 1 (RED-first): a work claimed by one actor is advanceable by a different actor
// with no steal on ungated code. On the gated code this must refuse, naming the holder and
// both remedies (--by <holder> or --steal --reason), and leave the work state untouched.
func TestAdvanceRefusesNonLeaseHolder(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-ag", SuccessPredicate: "advance lease enforced"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	_, err = s.Advance(w.ID, AdvanceInput{To: "running", Actor: "actor-b"}, fixedNow)
	if err == nil {
		t.Fatal("expected advance by non-holder actor-b to be refused (work held by actor-a)")
	}
	if !strings.Contains(err.Error(), "actor-a") {
		t.Fatalf("refusal error must name the holder actor-a: %v", err)
	}
	if !strings.Contains(err.Error(), "--by") || !strings.Contains(err.Error(), "--steal") {
		t.Fatalf("refusal error must name both remedies (--by / --steal): %v", err)
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "claimed" {
		t.Fatalf("refused advance must not mutate work state: got %q", after.State)
	}
	_, evs, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		if strings.Contains(e.Note, "refused") {
			found = true
			if e.By != "actor-b" {
				t.Fatalf("refusal event must be attributed to the refused actor actor-b, got %q", e.By)
			}
		}
	}
	if !found {
		t.Fatal("trail missing a refusal event")
	}
}

// Falsifier 2: --steal with an empty reason refuses exactly like a plain non-holder advance
// (no mutation).
func TestAdvanceStealWithoutReasonRefused(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-ag", SuccessPredicate: "steal without reason"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "actor-b", Steal: true}, fixedNow); err == nil {
		t.Fatal("expected --steal with empty reason to be refused")
	}
	after, err := s.Get(w.ID)
	if err != nil || after.State != "claimed" {
		t.Fatalf("empty-reason steal attempt must not mutate work: state=%q err=%v", after.State, err)
	}
}

// Falsifier 3: a valid steal (--steal --reason "<non-empty>") proceeds; the trail carries
// the prior holder, the reason string, and the stealing actor.
func TestAdvanceStealOverridesLeaseHolder(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-ag", SuccessPredicate: "steal path"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	advanced, err := s.Advance(w.ID, AdvanceInput{
		To:          "running",
		Actor:       "actor-b",
		Steal:       true,
		StealReason: "actor-a's executor died mid-build",
	}, fixedNow)
	if err != nil {
		t.Fatalf("expected steal with non-empty reason to succeed: %v", err)
	}
	if advanced.State != "running" {
		t.Fatalf("stolen advance should land the new state, got %q", advanced.State)
	}
	_, evs, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stealNote string
	for _, e := range evs {
		if strings.Contains(e.Note, "stolen") {
			stealNote = e.Note
			if e.By != "actor-b" {
				t.Fatalf("steal event must be attributed to the stealing actor actor-b, got %q", e.By)
			}
		}
	}
	if stealNote == "" {
		t.Fatal("trail missing a steal event")
	}
	if !strings.Contains(stealNote, "actor-a") {
		t.Fatalf("steal event must name the prior holder actor-a: %q", stealNote)
	}
	if !strings.Contains(stealNote, "actor-a's executor died mid-build") {
		t.Fatalf("steal event must carry the reason string: %q", stealNote)
	}
	// P0: a steal must actually transfer the lease file, not just narrate it on the
	// trail. Before the fix, recordLeaseSteal wrote the ActionIntent/PolicyDecision/Event
	// trail but never rewrote work/leases/<id>.json, so the file still named actor-a —
	// the exact bug TestAdvanceStealTransfersLeaseBlockingOriginalHolder reproduces below.
	lease, err := s.readLease(w.ID)
	if err != nil {
		t.Fatalf("read lease after steal: %v", err)
	}
	if lease.By != "actor-b" {
		t.Fatalf("steal must rewrite the lease file's holder to the stealing actor: got %q, want %q", lease.By, "actor-b")
	}
}

// P0 falsifier (RED FIRST): a lease steal must transfer the lease, not just narrate it.
// actor-a claims; actor-b steals via Advance (succeeds, as proven above); THEN actor-a
// advances again WITHOUT --steal. Because recordLeaseSteal never rewrote
// work/leases/<id>.json, the file still says "actor-a", so leaseHolderStatusFor's
// holder==actor short-circuit lets actor-a sail through as if nothing happened — the exact
// "twin-collision" class lease_close.go's header says this gate exists to close. On the
// gated-but-unfixed code this test fails: actor-a's second advance succeeds when it must be
// blocked.
func TestAdvanceStealTransfersLeaseBlockingOriginalHolder(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-ag", SuccessPredicate: "steal transfers lease"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{
		To:          "running",
		Actor:       "actor-b",
		Steal:       true,
		StealReason: "actor-a's executor died mid-build",
	}, fixedNow); err != nil {
		t.Fatalf("expected actor-b's steal to succeed: %v", err)
	}

	_, err = s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "actor-a"}, fixedNow)
	if err == nil {
		t.Fatal("expected actor-a's post-steal advance (no --steal) to be BLOCKED: the lease is now actor-b's")
	}
	if !strings.Contains(err.Error(), "actor-b") {
		t.Fatalf("refusal must name the new holder actor-b: %v", err)
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "running" {
		t.Fatalf("actor-a's blocked advance must not mutate work state: got %q, want %q", after.State, "running")
	}
}

// Land-check/proof-attachment falsifier (collateral fix): a SAME-STATE advance steal
// (in.To == w.State) is the class LandCheck uses to attach a recorded proof under a
// different --actor (internal/work/land_check.go: "Steal and StealReason are forwarded only
// to the same-state Advance that attaches a successful recorded proof. They do not transfer
// the lease..."). --steal there authorizes exactly one evidence write, not a hand-off: the
// lease file must stay untouched, and the ORIGINAL holder's next real lifecycle advance must
// still pass unguarded (documented residual behavior). This is the exact contract
// TestWorkLandCheckCLIStealOnlyOverridesProofAttachment (cmd/compound) pins end-to-end
// through the CLI; this is the internal/work-level version of the same falsifier. If
// recordLeaseSteal transferred unconditionally (this repo's first pass at the P0 fix did),
// this test goes red: lease.By flips to the stealing actor and actor-a's follow-up advance
// gets blocked instead of passing.
func TestAdvanceSameStateStealNarratesWithoutTransferringLease(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-ag", SuccessPredicate: "same-state steal does not transfer"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "actor-a"}, fixedNow); err != nil {
		t.Fatalf("actor-a's own advance to running: %v", err)
	}

	// Same-state steal: in.To ("running") == w.State ("running"). Mirrors LandCheck's
	// proof-attachment Advance, which passes To: current.State.
	if _, err := s.Advance(w.ID, AdvanceInput{
		To:          "running",
		Actor:       "operator:test",
		Steal:       true,
		StealReason: "attaching landing proof",
	}, fixedNow); err != nil {
		t.Fatalf("expected same-state steal to succeed: %v", err)
	}

	// (a) lease.By must stay actor-a — a same-state steal narrates, it does not transfer.
	lease, err := s.readLease(w.ID)
	if err != nil {
		t.Fatalf("read lease after same-state steal: %v", err)
	}
	if lease.By != "actor-a" {
		t.Fatalf("same-state advance steal must NOT transfer the lease: got by=%q, want %q", lease.By, "actor-a")
	}

	// The steal is still narrated on the trail (loud override, not a silent bypass).
	_, evs, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	sawSteal := false
	for _, e := range evs {
		if strings.Contains(e.Note, "lease stolen from \"actor-a\": attaching landing proof") {
			sawSteal = true
		}
	}
	if !sawSteal {
		t.Fatalf("same-state steal must still record a steal event: %+v", evs)
	}

	// (c) the ORIGINAL holder's next real lifecycle advance must still pass unguarded —
	// the lease never moved, so actor-a is still the recorded holder.
	advanced, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "actor-a"}, fixedNow)
	if err != nil {
		t.Fatalf("original holder actor-a must still pass the lease gate after a same-state steal: %v", err)
	}
	if advanced.State != "verifying" {
		t.Fatalf("actor-a's follow-up advance should land the new state, got %q", advanced.State)
	}
}

// Failure-ordering falsifier: if the lease-transfer write itself fails, the steal must fail
// closed — no misleading "stolen" trail entry, no advance mutation, and the original holder
// left in place on both the WorkEnvelope and the lease file. Fault-injected the same way
// TestDelegateMintFailureMeansZeroCreate does (delegate_test.go): chmod the target directory
// read+execute-only so the atomic stage+rename write (publishClaimJSON) cannot create its temp
// file. On the unfixed code this test fails outright: there is no lease-transfer write at all,
// so the chmod has no effect and the steal succeeds.
func TestAdvanceStealFailsClosedWhenLeaseTransferWriteFails(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-ag", SuccessPredicate: "fail-closed transfer"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	leasesDir := filepath.Join(s.home, "work", "leases")
	if err := os.Chmod(leasesDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(leasesDir, 0o755) })

	_, err = s.Advance(w.ID, AdvanceInput{
		To:          "running",
		Actor:       "actor-b",
		Steal:       true,
		StealReason: "actor-a's executor died mid-build",
	}, fixedNow)
	if err == nil {
		t.Fatal("expected the steal to fail closed when the lease-transfer write cannot be committed")
	}

	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "claimed" {
		t.Fatalf("a steal that fails to transfer the lease must not let advance proceed: got state %q", after.State)
	}
	lease, err := s.readLease(w.ID)
	if err != nil {
		t.Fatalf("read lease after failed transfer: %v", err)
	}
	if lease.By != "actor-a" {
		t.Fatalf("a failed lease-transfer write must leave the original holder in place: got %q", lease.By)
	}
	_, evs, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if strings.Contains(e.Note, "stolen") {
			t.Fatalf("a steal that failed to transfer the lease must not record a misleading 'stolen' event: %+v", e)
		}
	}
}

// Falsifier 4 (part 1): claimed by A, advanced by A — byte-identical to pre-gate behavior,
// no extra refusal/steal events land on the trail.
func TestAdvanceSameActorPassesUnchanged(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-ag", SuccessPredicate: "same actor"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "actor-a"}, fixedNow); err != nil {
		t.Fatalf("same-actor advance should succeed: %v", err)
	}
	_, evs, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantStates := []string{"ready", "claimed", "running"}
	if len(evs) != len(wantStates) {
		t.Fatalf("same-actor advance must add no gate events beyond the normal chain: got %d events %+v, want %d", len(evs), evs, len(wantStates))
	}
	for i, want := range wantStates {
		if evs[i].To != want {
			t.Errorf("event[%d].To = %q, want %q", i, evs[i].To, want)
		}
		if strings.Contains(evs[i].Note, "refused") || strings.Contains(evs[i].Note, "stolen") {
			t.Errorf("same-actor advance must not add a lease-gate event: %+v", evs[i])
		}
	}
}

// Falsifier 4 (part 2): advance on a never-claimed work by anyone succeeds exactly as
// before — no lease means no gate interference, and no new envelopes are written for the
// no-op gate path.
func TestAdvanceUnclaimedWorkUnaffected(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-ag", SuccessPredicate: "never claimed"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	advanced, err := s.Advance(w.ID, AdvanceInput{To: "held", Actor: "whoever"}, fixedNow)
	if err != nil {
		t.Fatalf("unclaimed advance should succeed unconditionally: %v", err)
	}
	if advanced.State != "held" {
		t.Fatalf("unclaimed advance should land the new state, got %q", advanced.State)
	}
	_, evs, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].To != "ready" || evs[1].To != "held" {
		t.Fatalf("unclaimed advance must be the plain create+advance chain: %+v", evs)
	}
}

// Falsifier 6: AdmitAuto's applyAdvance direct path is provably NOT behind the gate. The
// gate lives in Advance's guard chain (autonomyHaltGuard, editableSurfaceGuard,
// heldReleaseOperatorError, directorProposedReleaseOperatorError, leaseHolderAdvanceGate)
// and applyAdvance itself stays ungated on purpose — AdmitAuto calls applyAdvance directly
// (see AdmitAuto in auto_admit.go), so a clean-window eligible auto-admit succeeds with no
// Steal/StealReason set at all, proposed->admitted, exactly as the pre-existing
// auto_admit_test.go suite (left unmodified by this change) already proves.
func TestAdvanceAutoAdmitBypassesLeaseGateByDesign(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	id := makeProposedDirectorWork(t, s, "job_health", "work_lease_gate_bypass", fixedNow)
	res, err := s.AdmitAuto(id, eligible, fixedNow)
	if err != nil {
		t.Fatalf("AdmitAuto must not be gated by the advance lease gate: %v", err)
	}
	if !res.Admitted || res.WorkState != "admitted" {
		t.Fatalf("AdmitAuto through applyAdvance should reach admitted ungated: %+v", res)
	}
}

// Falsifier 7 (close-gate regression net): proven by lease_close_test.go and
// close_plan_test.go, both left byte-for-byte unmodified by this change, continuing to pass
// in the same `go test ./internal/work/...` run as these advance-gate tests.
