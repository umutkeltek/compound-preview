package lanebeliefs

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/mem"
)

// --- helpers -------------------------------------------------------------------------------------

// promoteObservedTendency seeds a PARKED tendency belief (via the B1 door) and promotes it through
// the B3 gate to a current observed claim. It uses a standing-lowering domain so the subject lane's
// OWN receipt self-promotes (no cross-lane mark needed) — the fast path to a promoted belief for the
// cooling tests. Returns the observed claim id + the resolved node id.
func promoteObservedTendency(t *testing.T, home, shortLane, domain, prefix string) (string, string) {
	t.Helper()
	cand, node := seedTendencyBelief(t, home, shortLane, domain)
	hitTrials(t, home, node, beliefKindTendency, domain, prefix, 2)
	rep := mustPromote(t, home)
	r := findResult(t, rep, cand.ID)
	if r.Disposition != DispositionPromoted {
		t.Fatalf("setup: observed %q belief did not promote: %+v", domain, r)
	}
	return r.ClaimID, node
}

// setRunState writes the fold run-state directly (unit precision): a run counter + a
// last_reinforced_run for one (node, tendency, domain) aggregate. Lets a cooling test place a belief
// at an exact number of quiet folds without driving real folds.
func setRunState(t *testing.T, home string, runCount int64, node, domain string, lastReinforced int64) {
	t.Helper()
	st := foldRunState{
		RunCount:       runCount,
		LastReinforced: map[string]int64{reinforcementKey(node, beliefKindTendency, domain): lastReinforced},
	}
	if err := saveRunState(home, st); err != nil {
		t.Fatalf("save run state: %v", err)
	}
}

func mustCool(t *testing.T, home string, dryRun bool) CoolReport {
	t.Helper()
	rep, err := Cool(home, CoolOptions{DryRun: dryRun, Now: foldTestTime})
	if err != nil {
		t.Fatalf("cool: %v", err)
	}
	return rep
}

func currentTendencyClaim(t *testing.T, home, node, domain string) (mem.Claim, bool) {
	t.Helper()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := store.AllCurrent()
	if err != nil {
		t.Fatal(err)
	}
	for _, cl := range cur {
		if cl.Entity == node && cl.Predicate == mem.BeliefPredicateTendsTo && cl.Qualifiers[beliefDomainKey] == domain {
			return cl, true
		}
	}
	return mem.Claim{}, false
}

func ledgerTendencyChain(t *testing.T, home, node, domain string) []mem.Claim {
	t.Helper()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.LedgerClaims()
	if err != nil {
		t.Fatal(err)
	}
	var out []mem.Claim
	for _, cl := range all {
		if cl.Entity == node && cl.Predicate == mem.BeliefPredicateTendsTo && cl.Qualifiers[beliefDomainKey] == domain {
			out = append(out, cl)
		}
	}
	return out
}

func coolResultFor(t *testing.T, rep CoolReport, observedClaimID string) CoolResult {
	t.Helper()
	for _, r := range rep.Results {
		if r.ObservedClaimID == observedClaimID {
			return r
		}
	}
	t.Fatalf("no cool result for observed claim %s in %+v", observedClaimID, rep.Results)
	return CoolResult{}
}

// --- F-c: decay, NOT delete ----------------------------------------------------------------------

// A promoted observed tendency belief that has been quiet for COOL_AFTER_FOLDS folds is superseded to
// cooled: the observed claim REMAINS in ledger history (never deleted), the current stance is cooled,
// the cooled claim supersedes the observed one, and the reason zero_reinforcement_cooled is durably
// findable. RED without cool.go: the claim stays observed forever.
func TestCoolFc_DecayNotDelete(t *testing.T) {
	home := t.TempDir()
	observedID, node := promoteObservedTendency(t, home, "fixture/judge", "scope-creep", "fc")
	// run 4, last reinforced on run 1 → 3 quiet folds ≥ COOL_AFTER_FOLDS.
	setRunState(t, home, 4, node, "scope-creep", 1)

	rep := mustCool(t, home, false)
	if rep.CooledCount != 1 || rep.EvaluatedCount != 1 {
		t.Fatalf("cool report = %+v, want 1 evaluated / 1 cooled", rep)
	}
	res := coolResultFor(t, rep, observedID)
	if res.Disposition != CoolDispositionCooled || res.Reason != CoolReason {
		t.Fatalf("result = %+v, want cooled/zero_reinforcement_cooled", res)
	}
	if res.PromotionBasis != candidates.BasisRuntimeProof || res.Visibility != "contextual" {
		t.Fatalf("cooled claim basis/visibility = %q/%q, want runtime_proof/contextual", res.PromotionBasis, res.Visibility)
	}

	// Current stance is cooled.
	cur, ok := currentTendencyClaim(t, home, node, "scope-creep")
	if !ok || cur.Object != tendencyObjectCooled {
		t.Fatalf("current tendency = %+v ok=%v, want object=cooled", cur, ok)
	}
	if cur.Supersedes != observedID {
		t.Fatalf("cooled claim supersedes %q, want the observed claim %q", cur.Supersedes, observedID)
	}

	// Decay, not delete: the observed claim is STILL readable in ledger history.
	chain := ledgerTendencyChain(t, home, node, "scope-creep")
	var sawObserved, sawCooled bool
	for _, cl := range chain {
		if cl.ID == observedID && cl.Object == tendencyObjectObserved {
			sawObserved = true
		}
		if cl.ID == cur.ID && cl.Object == tendencyObjectCooled {
			sawCooled = true
		}
	}
	if !sawObserved {
		t.Fatalf("observed claim %s absent from ledger history after cooling (delete, not decay): %+v", observedID, chain)
	}
	if !sawCooled {
		t.Fatalf("cooled claim missing from ledger: %+v", chain)
	}

	// The reason zero_reinforcement_cooled is durably findable (report-artifact path).
	body, err := os.ReadFile(coolingsPath(home))
	if err != nil {
		t.Fatalf("coolings artifact missing: %v", err)
	}
	if !strings.Contains(string(body), CoolReason) || !strings.Contains(string(body), observedID) || !strings.Contains(string(body), cur.ID) {
		t.Fatalf("coolings artifact must record the reason + both claim ids:\n%s", body)
	}
}

// --- F-boundary: 2 quiet runs NOT cooled, the 3rd cools ------------------------------------------

func TestCoolFboundary_ThirdQuietRunCools(t *testing.T) {
	home := t.TempDir()
	observedID, node := promoteObservedTendency(t, home, "fixture/judge", "scope-creep", "fb")

	// After 2 quiet folds (run_count 3, last reinforced run 1): quiet = 2 < 3 → NOT cooled.
	setRunState(t, home, 3, node, "scope-creep", 1)
	rep2 := mustCool(t, home, false)
	if rep2.CooledCount != 0 {
		t.Fatalf("2 quiet folds cooled the belief early: %+v", rep2)
	}
	if r := coolResultFor(t, rep2, observedID); r.Reason != ReasonReinforcedWithinWindow || r.QuietFolds != 2 {
		t.Fatalf("2-quiet result = %+v, want skipped reinforced_within_window quiet=2", r)
	}
	if cur, _ := currentTendencyClaim(t, home, node, "scope-creep"); cur.Object != tendencyObjectObserved {
		t.Fatalf("belief cooled after only 2 quiet folds: %+v", cur)
	}

	// The 3rd quiet fold (run_count 4): quiet = 3 → cools.
	setRunState(t, home, 4, node, "scope-creep", 1)
	rep3 := mustCool(t, home, false)
	if rep3.CooledCount != 1 {
		t.Fatalf("3rd quiet fold did not cool: %+v", rep3)
	}
	if r := coolResultFor(t, rep3, observedID); r.Disposition != CoolDispositionCooled || r.QuietFolds != 3 {
		t.Fatalf("3-quiet result = %+v, want cooled quiet=3", r)
	}
}

// --- F-reinforced: reinforcement resets the counter ---------------------------------------------

func TestCoolFreinforced_ReinforcementResetsCounter(t *testing.T) {
	home := t.TempDir()
	observedID, node := promoteObservedTendency(t, home, "fixture/judge", "scope-creep", "fr")
	// Reinforced on run 2; now at run 4 → only 2 quiet folds since reinforcement → NOT cooled.
	setRunState(t, home, 4, node, "scope-creep", 2)

	rep := mustCool(t, home, false)
	if rep.CooledCount != 0 {
		t.Fatalf("belief cooled despite recent reinforcement: %+v", rep)
	}
	r := coolResultFor(t, rep, observedID)
	if r.Reason != ReasonReinforcedWithinWindow || r.QuietFolds != 2 {
		t.Fatalf("result = %+v, want skipped reinforced_within_window quiet=2", r)
	}
}

// --- F-immune: capability beliefs never cool ----------------------------------------------------

// A promoted capability belief under an IDENTICAL quiet window is untouched, while a co-resident
// tendency belief on the same lane cools — proving the immunity is a real filter, not an artifact of
// the fixture never reaching the gate.
func TestCoolFimmune_CapabilityBeliefNeverCools(t *testing.T) {
	home := t.TempDir()

	// A promoted tendency belief (self-promotes) that WILL cool.
	tendencyID, node := promoteObservedTendency(t, home, "fixture/judge", "scope-creep", "imm")

	// A promoted capability belief on the SAME lane (needs a cross-lane mark to promote).
	capCand, _ := seedCapabilityBelief(t, home, "fixture/judge", "control-plane-repair")
	hitTrials(t, home, node, beliefKindCapability, "control-plane-repair", "immcap", 2)
	seedLaneNode(t, home, "fixture/verify")
	writeLaneReceiptEvent(t, home, laneReceiptFixture{
		ReceiptID: "rec_imm_verify", WorkID: "immcap_w0", LaneID: "fixture/verify",
		MoveType: "control_plane_repair", Outcome: "completed", Changed: true,
	})
	capRep := mustPromote(t, home)
	if findResult(t, capRep, capCand.ID).Disposition != DispositionPromoted {
		t.Fatalf("setup: capability belief did not promote: %+v", capRep)
	}

	// Both aggregates are quiet for 3 folds.
	st := foldRunState{RunCount: 4, LastReinforced: map[string]int64{
		reinforcementKey(node, beliefKindTendency, "scope-creep"):            1,
		reinforcementKey(node, beliefKindCapability, "control-plane-repair"): 1,
	}}
	if err := saveRunState(home, st); err != nil {
		t.Fatal(err)
	}

	rep := mustCool(t, home, false)
	// Only the tendency is evaluated and cooled; the capability belief is never even considered.
	if rep.EvaluatedCount != 1 || rep.CooledCount != 1 {
		t.Fatalf("cool report = %+v, want exactly the tendency evaluated+cooled (capability immune)", rep)
	}
	if r := coolResultFor(t, rep, tendencyID); r.Disposition != CoolDispositionCooled {
		t.Fatalf("tendency should cool: %+v", r)
	}
	// The capability claim is untouched: still object=confirmed and current.
	store, _ := mem.Open(home)
	cur, _ := store.AllCurrent()
	sawCapability := false
	for _, cl := range cur {
		if cl.Predicate == mem.BeliefPredicateCapableOf && cl.Entity == node {
			sawCapability = true
			if cl.Object != mem.CapabilityObject {
				t.Fatalf("capability belief mutated by cooling: object=%q", cl.Object)
			}
		}
	}
	if !sawCapability {
		t.Fatalf("capability claim vanished after cooling; current=%+v", cur)
	}
}

// --- F-recovery: fresh evidence re-promotes observed over cooled --------------------------------

// The full observed → cooled → observed chain, driven by the REAL pipeline (fold → promote → cool →
// fold → promote). ceremony-skipper is proof-absent and standing-lowering, so both the initial
// promotion and the recovery promote on the subject lane's own receipts.
func TestCoolFrecovery_FreshEvidenceRepromotesOverCooled(t *testing.T) {
	home := t.TempDir()
	seedLaneNode(t, home, "fixture/judge")
	// Round 1: 5 proof-ABSENT receipts across 5 works → ceremony-skipper (standing-lowering).
	for i := 0; i < MIN_LANE_RECEIPTS; i++ {
		writeLaneReceiptEvent(t, home, laneReceiptFixture{
			ReceiptID: fmt.Sprintf("rec_rec1_%02d", i), WorkID: fmt.Sprintf("work_rec1_%02d", i),
			LaneID: "fixture/judge", MoveType: "control_plane_repair", Outcome: "completed",
			Changed: true, NoProof: true,
		})
	}
	if _, err := Fold(home, FoldOptions{Now: foldTestTime}); err != nil { // run 1
		t.Fatal(err)
	}
	node, _ := currentOrResolve(t, home, "fixture/judge")
	repP := mustPromote(t, home)
	var observedID string
	for _, r := range repP.Results {
		if r.Domain == tendencyCeremonySkipper && r.Disposition == DispositionPromoted {
			observedID = r.ClaimID
		}
	}
	if observedID == "" {
		t.Fatalf("ceremony-skipper belief did not promote in round 1: %+v", repP)
	}

	// 3 quiet folds (runs 2,3,4) — no new events.
	for i := 0; i < COOL_AFTER_FOLDS; i++ {
		if _, err := Fold(home, FoldOptions{Now: foldTestTime}); err != nil {
			t.Fatal(err)
		}
	}
	coolRep := mustCool(t, home, false)
	if coolRep.CooledCount != 1 {
		t.Fatalf("belief did not cool after 3 quiet folds: %+v", coolRep)
	}
	cooled, _ := currentTendencyClaim(t, home, node, tendencyCeremonySkipper)
	if cooled.Object != tendencyObjectCooled {
		t.Fatalf("current stance not cooled: %+v", cooled)
	}
	cooledID := cooled.ID

	// Round 2: fresh evidence (2 new works) → fold mints a RECOVERY candidate → promote re-installs observed.
	for i := 0; i < MIN_CORROBORATION_WORKS; i++ {
		writeLaneReceiptEvent(t, home, laneReceiptFixture{
			ReceiptID: fmt.Sprintf("rec_rec2_%02d", i), WorkID: fmt.Sprintf("work_rec2_%02d", i),
			LaneID: "fixture/judge", MoveType: "control_plane_repair", Outcome: "completed",
			Changed: true, NoProof: true,
		})
	}
	if _, err := Fold(home, FoldOptions{Now: foldTestTime}); err != nil { // run 5 — recovery candidate
		t.Fatal(err)
	}
	repR := mustPromote(t, home)
	var recoveredID string
	for _, r := range repR.Results {
		if r.Domain == tendencyCeremonySkipper && r.Disposition == DispositionPromoted {
			recoveredID = r.ClaimID
		}
	}
	if recoveredID == "" {
		t.Fatalf("recovery did not re-promote observed over cooled: %+v", repR)
	}

	// The full chain observed → cooled → observed is readable in history; current is observed.
	cur, ok := currentTendencyClaim(t, home, node, tendencyCeremonySkipper)
	if !ok || cur.Object != tendencyObjectObserved || cur.ID != recoveredID {
		t.Fatalf("current stance after recovery = %+v ok=%v, want observed (%s)", cur, ok, recoveredID)
	}
	if cur.Supersedes != cooledID {
		t.Fatalf("recovered claim supersedes %q, want the cooled claim %q", cur.Supersedes, cooledID)
	}
	chain := ledgerTendencyChain(t, home, node, tendencyCeremonySkipper)
	objects := map[string]string{} // id -> object, for the three we care about
	for _, cl := range chain {
		objects[cl.ID] = cl.Object
	}
	if objects[observedID] != tendencyObjectObserved || objects[cooledID] != tendencyObjectCooled || objects[recoveredID] != tendencyObjectObserved {
		t.Fatalf("observed→cooled→observed chain not fully readable: observed=%q cooled=%q recovered=%q chain=%+v",
			objects[observedID], objects[cooledID], objects[recoveredID], chain)
	}
	// Distinct versions (decay, not overwrite).
	if observedID == cooledID || cooledID == recoveredID || observedID == recoveredID {
		t.Fatalf("chain versions collapsed: %s / %s / %s", observedID, cooledID, recoveredID)
	}
}

// --- F-idempotent: a second cool run over unchanged state does nothing --------------------------

func TestCoolFidempotent_SecondRunNoNewActions(t *testing.T) {
	home := t.TempDir()
	_, node := promoteObservedTendency(t, home, "fixture/judge", "scope-creep", "id")
	setRunState(t, home, 4, node, "scope-creep", 1)

	first := mustCool(t, home, false)
	if first.CooledCount != 1 {
		t.Fatalf("first cool must cool the belief: %+v", first)
	}
	claimsBefore, err := os.ReadFile(filepath.Join(home, "ledger", "claims.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	coolingsBefore, err := os.ReadFile(coolingsPath(home))
	if err != nil {
		t.Fatal(err)
	}

	second := mustCool(t, home, false)
	if second.CooledCount != 0 {
		t.Fatalf("second cool over unchanged state cooled %d, want 0 (idempotent)", second.CooledCount)
	}
	if second.EvaluatedCount != 0 {
		t.Fatalf("second cool re-evaluated a superseded observed claim (%d evaluated); it must no longer be current", second.EvaluatedCount)
	}
	claimsAfter, err := os.ReadFile(filepath.Join(home, "ledger", "claims.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	coolingsAfter, err := os.ReadFile(coolingsPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(claimsBefore, claimsAfter) {
		t.Fatalf("second cool mutated the claims ledger")
	}
	if !bytes.Equal(coolingsBefore, coolingsAfter) {
		t.Fatalf("second cool appended a cooling record")
	}
}

// --- F-dry-run: --dry-run leaves the ledger and candidates byte-identical -----------------------

func TestCoolFdryRun_ByteIdentical(t *testing.T) {
	home := t.TempDir()
	observedID, node := promoteObservedTendency(t, home, "fixture/judge", "scope-creep", "dr")
	setRunState(t, home, 4, node, "scope-creep", 1)

	claimsBefore, err := os.ReadFile(filepath.Join(home, "ledger", "claims.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	candDir := filepath.Join(home, "inbox", "memory-candidates")
	candsBefore := dirDigest(t, candDir)

	rep := mustCool(t, home, true)
	if !rep.DryRun || rep.CooledCount != 1 {
		t.Fatalf("dry-run report = %+v, want DryRun=true and 1 would-cool", rep)
	}
	if r := coolResultFor(t, rep, observedID); r.Disposition != CoolDispositionCooled {
		t.Fatalf("dry-run must report the would-cool disposition: %+v", r)
	}

	claimsAfter, err := os.ReadFile(filepath.Join(home, "ledger", "claims.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(claimsBefore, claimsAfter) {
		t.Fatalf("dry-run mutated the claims ledger")
	}
	if got := dirDigest(t, candDir); got != candsBefore {
		t.Fatalf("dry-run mutated the candidate inbox")
	}
	if _, err := os.Stat(coolingsPath(home)); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote the coolings artifact (stat err = %v, want not-exist)", err)
	}
	// The belief is still observed.
	if cur, _ := currentTendencyClaim(t, home, node, "scope-creep"); cur.Object != tendencyObjectObserved {
		t.Fatalf("dry-run changed the current stance to %q", cur.Object)
	}
}

func dirDigest(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s=%x\n", e.Name(), len(data))
		b.Write(data)
	}
	return b.String()
}

// --- F-valve: the cooled draft is valve-clean; cooling can only go through the governed write -----

func TestCoolFvalve_CooledDraftIsValveClean(t *testing.T) {
	home := t.TempDir()
	observedID, node := promoteObservedTendency(t, home, "fixture/judge", "scope-creep", "vv")
	resolver := promoteResolver(t, home)

	// The cooled draft the pass constructs passes ValidateBeliefDraft (object=cooled is ratified).
	draft, err := mem.TendencyBeliefDraft(resolver, node, "scope-creep", tendencyObjectCooled, foldProvenanceTag)
	if err != nil {
		t.Fatalf("cooled draft build: %v", err)
	}
	draft.ID = coolCandidateID(observedID)
	if err := mem.ValidateBeliefDraft(draft); err != nil {
		t.Fatalf("cooled draft must pass the valve: %v", err)
	}
	// An object outside {observed, cooled} is unrepresentable — the valve is the gate cooling rides.
	if _, err := mem.TendencyBeliefDraft(resolver, node, "scope-creep", "retired", foldProvenanceTag); err == nil {
		t.Fatalf("valve accepted a bogus tendency object; cooling must ride the ratified vocabulary only")
	}

	// End-to-end: after a real cool, the cooled ledger claim keeps the belief shape (contextual
	// visibility, tends_to, domain qualifier) — valve-clean, never a raw hand-write.
	setRunState(t, home, 4, node, "scope-creep", 1)
	mustCool(t, home, false)
	cur, ok := currentTendencyClaim(t, home, node, "scope-creep")
	if !ok || cur.Object != tendencyObjectCooled || cur.Visibility != "contextual" || cur.Qualifiers[beliefDomainKey] != "scope-creep" {
		t.Fatalf("cooled claim not valve-clean: %+v ok=%v", cur, ok)
	}
}

// --- fold run-bookkeeping is additive and dry-run-safe ------------------------------------------

func TestFoldRunBookkeeping(t *testing.T) {
	home := t.TempDir()
	seedLaneNode(t, home, "fixture/judge")
	for i := 0; i < MIN_LANE_RECEIPTS; i++ {
		writeLaneReceiptEvent(t, home, laneReceiptFixture{
			ReceiptID: fmt.Sprintf("rec_rb_%02d", i), WorkID: fmt.Sprintf("work_rb_%02d", i),
			LaneID: "fixture/judge", MoveType: "control_plane_repair", Outcome: "completed", Changed: true,
		})
	}

	// Dry-run must not create the run state.
	dry, err := Fold(home, FoldOptions{DryRun: true, Now: foldTestTime})
	if err != nil {
		t.Fatal(err)
	}
	if dry.RunCount != 0 {
		t.Fatalf("dry-run reported RunCount=%d, want 0 (no persistence)", dry.RunCount)
	}
	if _, err := os.Stat(runStatePath(home)); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote run state (stat err = %v, want not-exist)", err)
	}

	// Real fold 1: run_count=1, both aggregates reinforced this run.
	r1, err := Fold(home, FoldOptions{Now: foldTestTime})
	if err != nil {
		t.Fatal(err)
	}
	if r1.RunCount != 1 || r1.ReinforcedAggregateCount != 2 {
		t.Fatalf("fold 1 = run %d reinforced %d, want 1/2 (capability + tendency): %+v", r1.RunCount, r1.ReinforcedAggregateCount, r1)
	}

	// Quiet fold 2: run_count advances, nothing reinforced.
	r2, err := Fold(home, FoldOptions{Now: foldTestTime})
	if err != nil {
		t.Fatal(err)
	}
	if r2.RunCount != 2 || r2.ReinforcedAggregateCount != 0 {
		t.Fatalf("quiet fold 2 = run %d reinforced %d, want 2/0: %+v", r2.RunCount, r2.ReinforcedAggregateCount, r2)
	}

	// The persisted last_reinforced_run for the tendency stays at 1 across the quiet fold.
	node, _ := currentOrResolve(t, home, "fixture/judge")
	st, err := loadRunState(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.LastReinforced[reinforcementKey(node, beliefKindTendency, tendencyReceiptFaithful)]; got != 1 {
		t.Fatalf("tendency last_reinforced = %d after a quiet fold, want 1", got)
	}
}

func currentOrResolve(t *testing.T, home, shortLane string) (string, bool) {
	t.Helper()
	r, err := openLaneResolver(home)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := r.ResolveLaneRef(shortLane)
	if !ok {
		t.Fatalf("lane %q did not resolve", shortLane)
	}
	return node, true
}
