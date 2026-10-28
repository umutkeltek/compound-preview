package lanebeliefs

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
)

// --- helpers -------------------------------------------------------------------------------------

func promoteResolver(t *testing.T, home string) mem.LaneResolver {
	t.Helper()
	r, err := openLaneResolver(home)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// seedTendencyBelief imports the lane (B0) and records a PARKED tendency-belief candidate built ONLY
// through the B1 door + valve — the exact shape B2's fold proposes. Returns the candidate + node id.
func seedTendencyBelief(t *testing.T, home, shortLane, domain string) (mem.Candidate, string) {
	t.Helper()
	node := seedLaneNode(t, home, shortLane)
	draft, err := mem.TendencyBeliefDraft(promoteResolver(t, home), shortLane, domain, tendencyObjectObserved, foldProvenanceTag)
	if err != nil {
		t.Fatalf("build tendency draft (%s): %v", domain, err)
	}
	draft.ID = beliefCandidateID(draft)
	draft.Confidence = 0.7
	draft.ObservationDate = foldTestTime.Format(time.RFC3339)
	draft.CreatedAt = foldTestTime.Format(time.RFC3339)
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordStableCandidate(draft); err != nil {
		t.Fatalf("record candidate: %v", err)
	}
	return draft, node
}

// seedCapabilityBelief is the capability-shape analogue (always standing-raising).
func seedCapabilityBelief(t *testing.T, home, shortLane, taskClass string) (mem.Candidate, string) {
	t.Helper()
	node := seedLaneNode(t, home, shortLane)
	draft, err := mem.CapabilityBeliefDraft(promoteResolver(t, home), shortLane, taskClass, foldProvenanceTag)
	if err != nil {
		t.Fatalf("build capability draft (%s): %v", taskClass, err)
	}
	draft.ID = beliefCandidateID(draft)
	draft.Confidence = 0.7
	draft.ObservationDate = foldTestTime.Format(time.RFC3339)
	draft.CreatedAt = foldTestTime.Format(time.RFC3339)
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordStableCandidate(draft); err != nil {
		t.Fatalf("record candidate: %v", err)
	}
	return draft, node
}

// seedTrial appends ONE observation trial (one work) for a (node, kind, domain) belief. hits ∈ {0,1}
// (a miss vs a hit); opportunities is normally 1. receiptCount is raw volume (must not move the Beta).
func seedTrial(t *testing.T, home, node, kind, domain, workID, evidenceID string, hits, opportunities, receiptCount int) {
	t.Helper()
	obs := observationRecord{
		DedupKey:          hashParts(node, domain, workID, evidenceID),
		GroupKey:          hashParts(node, kind, domain, workID),
		SubjectLaneNodeID: node,
		Domain:            domain,
		Kind:              kind,
		Object:            tendencyObjectObserved,
		WorkID:            workID,
		EvidenceEventID:   evidenceID,
		MoveType:          "control_plane_repair",
		Outcome:           "completed",
		ReceiptCount:      receiptCount,
		Hits:              hits,
		Opportunities:     opportunities,
		CreatedAt:         foldTestTime.Format(time.RFC3339),
	}
	if err := appendObservations(home, []observationRecord{obs}); err != nil {
		t.Fatal(err)
	}
}

func hitTrials(t *testing.T, home, node, kind, domain, prefix string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		seedTrial(t, home, node, kind, domain, fmt.Sprintf("%s_w%d", prefix, i), fmt.Sprintf("rec_%s_%d", prefix, i), 1, 1, 1)
	}
}

func findResult(t *testing.T, rep PromoteReport, candID string) PromoteResult {
	t.Helper()
	for _, r := range rep.Results {
		if r.CandidateID == candID {
			return r
		}
	}
	t.Fatalf("no result for candidate %s in %+v", candID, rep.Results)
	return PromoteResult{}
}

func mustPromote(t *testing.T, home string) PromoteReport {
	t.Helper()
	rep, err := Promote(home, PromoteOptions{Now: foldTestTime})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// --- F-a: self-report cannot install a standing-raising belief -----------------------------------

func TestPromoteFa_SelfReportCannotInstallStandingRaising(t *testing.T) {
	home := t.TempDir()
	cand, node := seedTendencyBelief(t, home, "fixture/judge", tendencyReceiptFaithful) // raising
	hitTrials(t, home, node, beliefKindTendency, tendencyReceiptFaithful, "a", 2)       // 2 works, both hits

	// RED half: only the subject lane's evidence exists → no cross-lane mark → refused.
	rep := mustPromote(t, home)
	r := findResult(t, rep, cand.ID)
	if r.Disposition != DispositionParked || r.Reason != ReasonNoCrossLaneMark {
		t.Fatalf("self-only standing-raising belief = %+v, want parked/no_cross_lane_mark", r)
	}
	if r.PosteriorMean < PROMOTE_POSTERIOR_FLOOR || r.WorkCount < MIN_CORROBORATION_WORKS {
		t.Fatalf("fixture must clear corroboration+floor so ONLY the mark gates: %+v", r)
	}

	// GREEN half: one cross-lane proof-bearing receipt on one of its works → promotes.
	seedLaneNode(t, home, "fixture/verify")
	writeLaneReceiptEvent(t, home, laneReceiptFixture{
		ReceiptID: "rec_verify_a", WorkID: "a_w0", LaneID: "fixture/verify",
		MoveType: "control_plane_repair", Outcome: "completed", Changed: true,
	})
	rep2 := mustPromote(t, home)
	r2 := findResult(t, rep2, cand.ID)
	if r2.Disposition != DispositionPromoted || !r2.HasCrossLaneMark || r2.PromotionBasis != candidates.BasisRuntimeProof {
		t.Fatalf("cross-lane mark should install the belief: %+v", r2)
	}
}

// --- F-h: the valence split — same evidence, opposite outcomes -----------------------------------

func TestPromoteFh_ValenceSplitSameEvidenceOppositeOutcome(t *testing.T) {
	home := t.TempDir()
	// One subject lane, two tendencies, IDENTICAL evidence: 2 works, both hits, NO cross-lane marks.
	lowering, node := seedTendencyBelief(t, home, "fixture/judge", "scope-creep") // standing-lowering
	raising, _ := seedTendencyBelief(t, home, "fixture/judge", "falsifier-first") // standing-raising
	hitTrials(t, home, node, beliefKindTendency, "scope-creep", "lo", 2)
	hitTrials(t, home, node, beliefKindTendency, "falsifier-first", "ra", 2)

	rep := mustPromote(t, home)
	lo := findResult(t, rep, lowering.ID)
	ra := findResult(t, rep, raising.ID)

	if lo.Disposition != DispositionPromoted || lo.PromotionBasis != candidates.BasisRuntimeProof {
		t.Fatalf("scope-creep (lowering) on self-receipt alone must PROMOTE (self-incrimination valid): %+v", lo)
	}
	if ra.Disposition != DispositionParked || ra.Reason != ReasonNoCrossLaneMark {
		t.Fatalf("falsifier-first (raising) on IDENTICAL evidence must REFUSE (no cross-lane mark): %+v", ra)
	}
}

// --- F-grief: attribution is always self — a third party can never grief a lane ------------------

func TestPromoteFgrief_AttributionAlwaysSelf(t *testing.T) {
	home := t.TempDir()
	xNode := seedLaneNode(t, home, "fixture/judge")
	yNode := seedLaneNode(t, home, "fixture/verify")
	// Both lanes complete-with-proof on the SAME work.
	writeLaneReceiptEvent(t, home, laneReceiptFixture{ReceiptID: "rec_x", WorkID: "work_shared", LaneID: "fixture/judge", MoveType: "control_plane_repair", Outcome: "completed", Changed: true})
	writeLaneReceiptEvent(t, home, laneReceiptFixture{ReceiptID: "rec_y", WorkID: "work_shared", LaneID: "fixture/verify", MoveType: "control_plane_repair", Outcome: "completed", Changed: true})
	if _, err := Fold(home, FoldOptions{Now: foldTestTime}); err != nil {
		t.Fatal(err)
	}
	obs, err := loadObservations(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) == 0 {
		t.Fatal("expected observations from two lanes on one work")
	}
	resolver := promoteResolver(t, home)
	sawX, sawY := false, false
	for _, o := range obs {
		rec, err := loadReceipt(home, o.EvidenceEventID)
		if err != nil {
			t.Fatalf("observation evidence receipt %q missing: %v", o.EvidenceEventID, err)
		}
		got := resolveReceiptLane(resolver, rec)
		if !got.Resolved() || got.NodeID != o.SubjectLaneNodeID {
			t.Fatalf("observation %+v attributed to %q, not to its own receipt's producer lane", o, got.NodeID)
		}
		// The structural grief invariant: X's receipt can never source a belief about Y, or vice versa.
		if o.SubjectLaneNodeID == yNode && o.EvidenceEventID == "rec_x" {
			t.Fatalf("grief: a belief about lane Y was sourced from lane X's receipt: %+v", o)
		}
		if o.SubjectLaneNodeID == xNode && o.EvidenceEventID == "rec_y" {
			t.Fatalf("grief: a belief about lane X was sourced from lane Y's receipt: %+v", o)
		}
		sawX = sawX || o.SubjectLaneNodeID == xNode
		sawY = sawY || o.SubjectLaneNodeID == yNode
	}
	if !sawX || !sawY {
		t.Fatalf("each lane must form its OWN self-sourced observations: sawX=%v sawY=%v", sawX, sawY)
	}
}

// --- F-floor: the posterior boundary — below / park / at-floor -----------------------------------

func TestPromoteFfloor_PosteriorBands(t *testing.T) {
	home := t.TempDir()
	// All standing-lowering (scope-creep) so ONLY the posterior band gates — no mark required.
	// park_band: 3 hits + 2 misses over 5 works → E[Beta] = (2+3)/(4+5) = 0.556 ∈ [0.50,0.66).
	park, pnode := seedTendencyBelief(t, home, "fixture/judge", "scope-creep")
	for i, h := range []int{1, 1, 1, 0, 0} {
		seedTrial(t, home, pnode, beliefKindTendency, "scope-creep", fmt.Sprintf("wp%d", i), fmt.Sprintf("rec_p%d", i), h, 1, 1)
	}
	// below_band: 0 hits over 3 works → (2+0)/(4+3) = 0.286 < 0.50.
	below, bnode := seedTendencyBelief(t, home, "fixture/verify", "scope-creep")
	for i := 0; i < 3; i++ {
		seedTrial(t, home, bnode, beliefKindTendency, "scope-creep", fmt.Sprintf("wb%d", i), fmt.Sprintf("rec_b%d", i), 0, 1, 1)
	}
	// at/above floor: 2 hits over 2 works → (2+2)/(4+2) = 0.667 ≥ 0.66.
	atfloor, onode := seedTendencyBelief(t, home, "fixture/extract", "scope-creep")
	hitTrials(t, home, onode, beliefKindTendency, "scope-creep", "wo", 2)

	rep := mustPromote(t, home)
	if r := findResult(t, rep, park.ID); r.Disposition != DispositionParked || r.Reason != ReasonParkBand {
		t.Fatalf("park fixture = %+v, want parked/park_band (posterior %.4f)", r, r.PosteriorMean)
	}
	if r := findResult(t, rep, below.ID); r.Disposition != DispositionParked || r.Reason != ReasonBelowBand {
		t.Fatalf("below fixture = %+v, want parked/below_band (posterior %.4f)", r, r.PosteriorMean)
	}
	if r := findResult(t, rep, atfloor.ID); r.Disposition != DispositionPromoted {
		t.Fatalf("at-floor fixture = %+v, want promoted (posterior %.4f)", r, r.PosteriorMean)
	}
}

// --- F-corroboration: all evidence in ONE work (any volume) → under_corroborated -----------------

func TestPromoteFcorroboration_SingleWorkAnyVolumeRefused(t *testing.T) {
	home := t.TempDir()
	cand, node := seedTendencyBelief(t, home, "fixture/judge", "scope-creep") // lowering → only corroboration gates
	// One work, 20 receipts of volume — must NOT reach the corroboration floor of 2 distinct works.
	seedTrial(t, home, node, beliefKindTendency, "scope-creep", "work_only", "rec_only", 1, 1, 20)

	rep := mustPromote(t, home)
	r := findResult(t, rep, cand.ID)
	if r.Disposition != DispositionParked || r.Reason != ReasonUnderCorroborated {
		t.Fatalf("single-work evidence (volume 20) = %+v, want parked/under_corroborated", r)
	}
	if r.WorkCount != 1 {
		t.Fatalf("work count = %d, want 1 (volume must not inflate distinct works)", r.WorkCount)
	}
}

// --- F-mark-quality: a bad cross-lane receipt is NOT a mark --------------------------------------

func TestPromoteFmarkQuality_BadMarksDoNotCount(t *testing.T) {
	// Each variant: a standing-raising belief clears corroboration+floor; only a proof-bearing
	// cross-lane receipt may install it. A bad receipt must leave it refused.
	bad := []struct {
		name string
		fx   laneReceiptFixture
	}{
		{"audit_only_move", laneReceiptFixture{LaneID: "fixture/verify", MoveType: "audit_only", Outcome: "completed"}},
		{"missing_proof_refs", laneReceiptFixture{LaneID: "fixture/verify", MoveType: "control_plane_repair", Outcome: "completed", NoProof: true}},
		{"not_completed", laneReceiptFixture{LaneID: "fixture/verify", MoveType: "control_plane_repair", Outcome: "failed"}},
		{"unresolvable_producer_lane", laneReceiptFixture{LaneID: "*", MoveType: "control_plane_repair", Outcome: "completed"}},
	}
	for _, v := range bad {
		t.Run(v.name, func(t *testing.T) {
			home := t.TempDir()
			cand, node := seedTendencyBelief(t, home, "fixture/judge", tendencyReceiptFaithful) // raising
			hitTrials(t, home, node, beliefKindTendency, tendencyReceiptFaithful, "mq", 2)
			seedLaneNode(t, home, "fixture/verify") // an imported cross-lane node (except the wildcard case)
			fx := v.fx
			fx.ReceiptID = "rec_badmark"
			fx.WorkID = "mq_w0"
			fx.Changed = true
			writeLaneReceiptEvent(t, home, fx)

			rep := mustPromote(t, home)
			r := findResult(t, rep, cand.ID)
			if r.Disposition != DispositionParked || r.Reason != ReasonNoCrossLaneMark || r.HasCrossLaneMark {
				t.Fatalf("bad mark (%s) must not install the belief: %+v", v.name, r)
			}
		})
	}

	t.Run("good_mark_promotes", func(t *testing.T) {
		home := t.TempDir()
		cand, node := seedTendencyBelief(t, home, "fixture/judge", tendencyReceiptFaithful)
		hitTrials(t, home, node, beliefKindTendency, tendencyReceiptFaithful, "mq", 2)
		seedLaneNode(t, home, "fixture/verify")
		writeLaneReceiptEvent(t, home, laneReceiptFixture{
			ReceiptID: "rec_goodmark", WorkID: "mq_w0", LaneID: "fixture/verify",
			MoveType: "control_plane_repair", Outcome: "completed", Changed: true,
		})
		rep := mustPromote(t, home)
		if r := findResult(t, rep, cand.ID); r.Disposition != DispositionPromoted || !r.HasCrossLaneMark {
			t.Fatalf("a proof-bearing cross-lane mark must promote: %+v", r)
		}
	})
}

// --- F-basis: the promoted claim carries runtime_proof + contextual visibility -------------------

func TestPromoteFbasis_RuntimeProofContextualReadback(t *testing.T) {
	home := t.TempDir()
	cand, node := seedTendencyBelief(t, home, "fixture/judge", "scope-creep") // lowering → promotes on self
	hitTrials(t, home, node, beliefKindTendency, "scope-creep", "ba", 2)

	rep := mustPromote(t, home)
	r := findResult(t, rep, cand.ID)
	if r.Disposition != DispositionPromoted {
		t.Fatalf("fixture must promote to test the basis readback: %+v", r)
	}
	if r.PromotionBasis != candidates.BasisRuntimeProof {
		t.Fatalf("report basis readback = %q, want runtime_proof", r.PromotionBasis)
	}
	if r.Visibility != "contextual" {
		t.Fatalf("report visibility readback = %q, want contextual", r.Visibility)
	}

	// Belt-and-suspenders: the durable ledger claim itself carries both.
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := store.LedgerClaims()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, cl := range claims {
		if cl.Entity == node && cl.Predicate == mem.BeliefPredicateTendsTo && cl.Qualifiers["domain"] == "scope-creep" {
			found = true
			if cl.PromotionBasis != candidates.BasisRuntimeProof || cl.Visibility != "contextual" {
				t.Fatalf("persisted claim basis/visibility = %q/%q, want runtime_proof/contextual", cl.PromotionBasis, cl.Visibility)
			}
		}
	}
	if !found {
		t.Fatalf("no persisted claim for the promoted belief; claims=%+v", claims)
	}
}

// --- F-idempotent: a second run over unchanged state promotes nothing new ------------------------

func TestPromoteFidempotent_SecondRunNoNewPromotions(t *testing.T) {
	home := t.TempDir()
	cand, node := seedTendencyBelief(t, home, "fixture/judge", "scope-creep")
	hitTrials(t, home, node, beliefKindTendency, "scope-creep", "id", 2)

	rep1 := mustPromote(t, home)
	if rep1.PromotedCount != 1 {
		t.Fatalf("first run promoted %d, want 1: %+v", rep1.PromotedCount, rep1)
	}
	if findResult(t, rep1, cand.ID).Disposition != DispositionPromoted {
		t.Fatalf("first run must promote the candidate")
	}

	rep2 := mustPromote(t, home)
	if rep2.PromotedCount != 0 {
		t.Fatalf("second run promoted %d, want 0 (idempotent)", rep2.PromotedCount)
	}
	if rep2.EvaluatedCount != 0 {
		t.Fatalf("second run re-evaluated a promoted candidate (%d evaluated); it must have left the parked set", rep2.EvaluatedCount)
	}
}

// --- F-dry-run: --dry-run leaves candidates byte-identical and writes no claim -------------------

func TestPromoteFdryRun_ByteIdenticalNoWrite(t *testing.T) {
	home := t.TempDir()
	cand, node := seedTendencyBelief(t, home, "fixture/judge", "scope-creep")
	hitTrials(t, home, node, beliefKindTendency, "scope-creep", "dr", 2)

	candPath := filepath.Join(home, "inbox", "memory-candidates", cand.ID+".json")
	before, err := os.ReadFile(candPath)
	if err != nil {
		t.Fatal(err)
	}

	rep, err := Promote(home, PromoteOptions{DryRun: true, Now: foldTestTime})
	if err != nil {
		t.Fatal(err)
	}
	if rep.PromotedCount != 1 || !rep.DryRun {
		t.Fatalf("dry-run must report 1 would-promote and DryRun=true: %+v", rep)
	}

	after, err := os.ReadFile(candPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("dry-run mutated the candidate file:\nbefore=%s\nafter=%s", before, after)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote a claim (claims.jsonl stat err = %v, want not-exist)", err)
	}
	// And the candidate is still parked (no adjudication happened).
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if c.ID == cand.ID && c.State != "parked" {
			t.Fatalf("dry-run changed candidate state to %q, want parked", c.State)
		}
	}
}

// --- capability beliefs are always standing-raising ---------------------------------------------

func TestPromoteCapabilityBeliefIsStandingRaising(t *testing.T) {
	home := t.TempDir()
	cand, node := seedCapabilityBelief(t, home, "fixture/judge", "control-plane-repair")
	hitTrials(t, home, node, beliefKindCapability, "control-plane-repair", "cap", 2)

	rep := mustPromote(t, home)
	r := findResult(t, rep, cand.ID)
	if r.Valence != ValenceStandingRaising {
		t.Fatalf("capability belief valence = %q, want standing-raising", r.Valence)
	}
	if r.Disposition != DispositionParked || r.Reason != ReasonNoCrossLaneMark {
		t.Fatalf("capability belief on self-evidence must refuse: %+v", r)
	}
}

// --- valence table guards the WHOLE tendency vocabulary -----------------------------------------

// The two valence sets must PARTITION the closed tendency vocabulary: every tendency is classified
// exactly once. If a new tendency joins mem.TendencyVocabulary without a valence, this reds — so a
// new tendency can never silently default to standing-raising (or standing-lowering) unreviewed.
func TestPromoteValencePartitionsTendencyVocabulary(t *testing.T) {
	for _, slug := range mem.TendencyVocabulary {
		raising := standingRaisingTendencies[slug]
		lowering := standingLoweringTendencies[slug]
		if raising == lowering { // both false (unclassified) or both true (double-classified)
			t.Fatalf("tendency %q must be classified in EXACTLY one valence set (raising=%v lowering=%v)", slug, raising, lowering)
		}
		if got := beliefValence(mem.BeliefPredicateTendsTo, slug); (got == ValenceStandingLowering) != lowering {
			t.Fatalf("beliefValence(%q) = %q, disagrees with the valence table", slug, got)
		}
	}
	// Neither set may contain a slug outside the vocabulary.
	for slug := range standingRaisingTendencies {
		if !containsStr(mem.TendencyVocabulary, slug) {
			t.Fatalf("standing-raising tendency %q is not in the closed vocabulary", slug)
		}
	}
	for slug := range standingLoweringTendencies {
		if !containsStr(mem.TendencyVocabulary, slug) {
			t.Fatalf("standing-lowering tendency %q is not in the closed vocabulary", slug)
		}
	}
}

func containsStr(set []string, want string) bool {
	for _, s := range set {
		if s == want {
			return true
		}
	}
	return false
}

// --- unresolved evidence: a parked belief with no backing observations --------------------------

func TestPromoteUnresolvedEvidence(t *testing.T) {
	home := t.TempDir()
	cand, _ := seedTendencyBelief(t, home, "fixture/judge", "scope-creep")
	// No observations seeded → the candidate has no resolvable evidence aggregate.
	rep := mustPromote(t, home)
	r := findResult(t, rep, cand.ID)
	if r.Disposition != DispositionParked || r.Reason != ReasonUnresolvedEvidence {
		t.Fatalf("orphan candidate = %+v, want parked/unresolved_evidence", r)
	}
}
