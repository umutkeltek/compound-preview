package work

import (
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
)

// seedActiveFreeze records an accepted freeze decision claim directly through
// mem.Store — the SAME shape `compound decisions record --kind freeze`
// produces (kind/lifecycle_status/decision_id/freeze_owner_globs/
// freeze_keywords qualifiers) — WITHOUT importing internal/decisions:
// internal/decisions imports internal/writeback, which imports internal/work,
// so internal/work importing internal/decisions (in production code OR an
// internal test file, which links into the same package) would be an import
// cycle. This helper hand-builds the identical qualifier shape instead.
func seedActiveFreeze(t *testing.T, s *Store, id string, ownerGlobs, keywords []string, now time.Time) {
	t.Helper()
	seedDecisionClaim(t, s, "freeze", id, map[string]string{
		"freeze_owner_globs": strings.Join(ownerGlobs, ","),
		"freeze_keywords":    strings.Join(keywords, ","),
	}, now)
}

// seedFreezeUndo records a superseding freeze decision claim (falsifier c):
// a NEW accepted record whose supersedes qualifier names the prior freeze id.
func seedFreezeUndo(t *testing.T, s *Store, id, undoOf string, now time.Time) {
	t.Helper()
	seedDecisionClaim(t, s, "freeze", id, map[string]string{
		"supersedes": undoOf,
	}, now)
}

func seedDecisionClaim(t *testing.T, s *Store, kind, id string, extra map[string]string, now time.Time) {
	t.Helper()
	memStore, err := mem.Open(s.home)
	if err != nil {
		t.Fatalf("open mem store: %v", err)
	}
	q := map[string]string{
		"kind":             kind,
		"source":           "decisions-repo",
		"decision_id":      id,
		"lifecycle_status": "accepted",
		"reviewed_by":      "operator:test",
	}
	for k, v := range extra {
		if strings.TrimSpace(v) != "" {
			q[k] = v
		}
	}
	cand := mem.Candidate{
		Entity:         "decision:" + id,
		Predicate:      "decided",
		Object:         "test decision " + id,
		Confidence:     1.0,
		Derivation:     "deterministic",
		ClaimType:      "decision",
		Qualifiers:     q,
		PromotionBasis: candidates.BasisDecisionRecord,
		SourceTraceRef: "test:" + id,
		SnippetHash:    id,
		Visibility:     "operational",
	}
	captured, err := memStore.Capture([]mem.Candidate{cand}, now)
	if err != nil {
		t.Fatalf("capture decision claim: %v", err)
	}
	if _, err := memStore.AdjudicateOperationalDecision(captured[0].ID, "promote", "test-seed", now); err != nil {
		t.Fatalf("adjudicate decision claim: %v", err)
	}
}

func TestGenericMemoryCannotRevokeRecordedFreeze(t *testing.T) {
	t.Parallel()
	legacyFreeze := mem.Claim{
		ID:        "claim_legacy_freeze",
		ClaimType: "decision",
		Qualifiers: map[string]string{
			"kind":             "freeze",
			"decision_id":      "legacy-freeze",
			"lifecycle_status": "accepted",
			"reviewed_by":      "operator:test",
			"freeze_keywords":  "absorption_debt",
		},
	}
	forgedRevocation := mem.Claim{
		ID:        "claim_forged_unfreeze",
		ClaimType: "decision",
		Qualifiers: map[string]string{
			"kind":             "ruling",
			"decision_id":      "forged-unfreeze",
			"lifecycle_status": "accepted",
			"supersedes":       "legacy-freeze",
		},
	}
	if got := activeFreezeRecords([]mem.Claim{legacyFreeze, forgedRevocation}); len(got) != 1 || got[0].ID != "legacy-freeze" {
		t.Fatalf("generic memory revoked recorded freeze: %+v", got)
	}
	forgedRevocation.Qualifiers[mem.OperationalDecisionAuthorityQualifier] = mem.OperationalDecisionAuthorityValue
	forgedRevocation.Qualifiers["reviewed_by"] = "operator:test"
	if got := activeFreezeRecords([]mem.Claim{legacyFreeze, forgedRevocation}); len(got) != 0 {
		t.Fatalf("authoritative superseder did not revoke recorded freeze: %+v", got)
	}
}

func TestActiveFreezeRecordsRejectsPersistedNonOperatorAuthority(t *testing.T) {
	t.Parallel()
	claim := mem.Claim{
		ID:        "claim_non_operator_freeze",
		ClaimType: "decision",
		Qualifiers: map[string]string{
			"kind":             "freeze",
			"decision_id":      "non-operator-freeze",
			"lifecycle_status": "accepted",
			"reviewed_by":      "codex",
			"freeze_keywords":  "job_health",
			mem.OperationalDecisionAuthorityQualifier: mem.OperationalDecisionAuthorityValue,
		},
	}
	if got := activeFreezeRecords([]mem.Claim{claim}); len(got) != 0 {
		t.Fatalf("persisted non-operator freeze retained authority: %+v", got)
	}
}

// TestFreezeGatesAutoAdmitByKeyword is the headline falsifier b proof: a
// new-organ/absorption-shaped director proposal (its director_trigger
// evidence ref matches an active freeze keyword) is denied and stays
// proposed, naming the freeze record; a smoothness/wiring proposal that
// matches nothing passes untouched.
func TestFreezeGatesAutoAdmitByKeyword(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	seedActiveFreeze(t, s, "direction-freeze-2026-07-10", nil, []string{"absorption_debt"}, fixedNow)

	// A new-organ/absorption-shaped proposal matches the freeze keyword and is denied.
	frozen := makeProposedDirectorWork(t, s, "absorption_debt", "work_freeze_a", fixedNow)
	res, err := s.AdmitAuto(frozen, eligible, fixedNow)
	if err == nil || res.Admitted {
		t.Fatalf("frozen scope must deny auto-admit: err=%v res=%+v", err, res)
	}
	if !strings.Contains(err.Error(), "direction-freeze-2026-07-10") {
		t.Fatalf("denial must name the freeze record, got %v", err)
	}
	if got, _ := s.Get(frozen); got.State != "proposed" {
		t.Fatalf("frozen proposal must stay proposed, got %q", got.State)
	}

	// A smoothness/wiring proposal (different class, no keyword hit) passes untouched.
	wiring := makeProposedDirectorWork(t, s, "job_health", "work_freeze_b", fixedNow)
	resB, err := s.AdmitAuto(wiring, eligible, fixedNow)
	if err != nil || !resB.Admitted {
		t.Fatalf("non-matching proposal must pass the freeze: err=%v res=%+v", err, resB)
	}
	if got, _ := s.Get(wiring); got.State != "admitted" {
		t.Fatalf("non-matching proposal should be admitted, got %q", got.State)
	}
}

// TestFreezeGatesAutoAdmitByOwnerGlob proves owner-glob matching also gates.
func TestFreezeGatesAutoAdmitByOwnerGlob(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	seedActiveFreeze(t, s, "direction-freeze-2026-07-10", []string{"compound-authority"}, nil, fixedNow)

	w, err := s.Create(CreateInput{
		WorkID:           "work_freeze_owner",
		Owner:            "compound-authority",
		Actor:            "director",
		SuccessPredicate: "new organ work",
		State:            "proposed",
		EvidenceRefs:     []string{"proposed_by:director", "director_trigger:job_health:work_freeze_owner"},
	}, fixedNow)
	if err != nil {
		t.Fatalf("create proposed work: %v", err)
	}
	res, err := s.AdmitAuto(w.ID, eligible, fixedNow)
	if err == nil || res.Admitted {
		t.Fatalf("owner-glob match must deny auto-admit: err=%v res=%+v", err, res)
	}
}

// TestFreezeDoesNotBlockOperatorAdmit is the required negative (settled
// design decision 3, Law 6): an operator-provenance advance to admitted
// passes even while a matching freeze is active — the freeze binds the
// SYSTEM, not the operator.
func TestFreezeDoesNotBlockOperatorAdmit(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedActiveFreeze(t, s, "direction-freeze-2026-07-10", nil, []string{"absorption_debt"}, fixedNow)
	id := makeProposedDirectorWork(t, s, "absorption_debt", "work_freeze_op", fixedNow)

	out, err := s.Advance(id, AdvanceInput{Actor: "operator-test", To: "admitted"}, fixedNow.Add(1*time.Minute))
	if err != nil {
		t.Fatalf("operator admit must not be blocked by an active freeze: %v", err)
	}
	if out.State != "admitted" {
		t.Fatalf("operator admit should reach admitted under freeze, got %q", out.State)
	}
}

// TestFreezeHaltsAdvanceSeamDefenseInDepth proves the generic Advance seam
// carries its own freeze check, mirroring
// TestKillSwitchHaltsAdvanceSeamDefenseInDepth: an autonomous (non-operator)
// admission is stopped by the freeze through the generic seam too, not only
// AdmitAuto.
func TestFreezeHaltsAdvanceSeamDefenseInDepth(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedActiveFreeze(t, s, "direction-freeze-2026-07-10", nil, []string{"absorption_debt"}, fixedNow)
	id := makeProposedDirectorWork(t, s, "absorption_debt", "work_freeze_seam", fixedNow)

	_, err := s.Advance(id, AdvanceInput{Actor: "auto-admit:director", To: "admitted"}, fixedNow.Add(1*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "freeze active") {
		t.Fatalf("generic seam must deny an autonomous admission with a freeze-active error, got %v", err)
	}
	if got, _ := s.Get(id); got.State != "proposed" {
		t.Fatalf("frozen advance must leave work proposed, got %q", got.State)
	}
}

// TestFreezeAbsentMeansNothingFrozen is the fail-open-for-freeze negative:
// with zero freeze records in the store, every autonomous admission passes
// untouched (an absent freeze must not accidentally block work).
func TestFreezeAbsentMeansNothingFrozen(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	id := makeProposedDirectorWork(t, s, "job_health", "work_freeze_none", fixedNow)

	res, err := s.AdmitAuto(id, eligible, fixedNow)
	if err != nil || !res.Admitted {
		t.Fatalf("no freeze records must never block admission: err=%v res=%+v", err, res)
	}
}

// TestFreezeUndoneRecordNoLongerBlocks wires falsifier c through the freeze
// admission guard: once the operator records a superseding undo, the same
// scope that used to freeze admission no longer does.
func TestFreezeUndoneRecordNoLongerBlocks(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	seedActiveFreeze(t, s, "direction-freeze-2026-07-10", nil, []string{"job_health"}, fixedNow)
	seedFreezeUndo(t, s, "direction-freeze-2026-07-10-lifted", "direction-freeze-2026-07-10", fixedNow.Add(time.Minute))

	id := makeProposedDirectorWork(t, s, "job_health", "work_freeze_lifted", fixedNow)
	res, err := s.AdmitAuto(id, eligible, fixedNow)
	if err != nil || !res.Admitted {
		t.Fatalf("lifted freeze must no longer block admission: err=%v res=%+v", err, res)
	}
}
