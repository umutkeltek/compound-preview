package work

import (
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

// makeSurfaceDirectorWork creates a director-proposed work that explicitly targets a
// harness surface (an EVOLVE-BLOCK harness-edit proposal), in state `proposed`.
func makeSurfaceDirectorWork(t *testing.T, s *Store, surface, id string, now time.Time) string {
	t.Helper()
	plan := ensureJobHealthAutonomyPlan(t, s)
	w, err := s.Create(CreateInput{
		WorkID:           id,
		Owner:            "compound-authority",
		Actor:            "director",
		TargetSurface:    surface,
		ProposalKind:     envelope.ProposalKindHarnessEdit,
		SuccessPredicate: "edit " + surface + " via " + id,
		State:            "proposed",
		EvidenceRefs:     []string{"proposed_by:director", "director_trigger:job_health:" + id, "route_plan:" + plan.ID},
		RouteDecisionID:  plan.ID,
	}, now)
	if err != nil {
		t.Fatalf("create proposed surface work: %v", err)
	}
	if w.State != "proposed" || w.TargetSurface != surface {
		t.Fatalf("surface work should start proposed with target_surface=%q, got state=%q surface=%q", surface, w.State, w.TargetSurface)
	}
	return w.ID
}

// TestEditableSurfaceGatesSystemProposal is the headline EVOLVE-BLOCK falsifier: a
// AGAIN once the surface is retired. It fails if the admission seam ignores the registry.
func TestEditableSurfaceGatesSystemProposal(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)

	// Phase A — registry ABSENT: fail-closed, the surface proposal is denied and stays proposed.
	a := makeSurfaceDirectorWork(t, s, "skill_descriptions", "work_surf_a", fixedNow)
	resA, err := s.AdmitAuto(a, eligible, fixedNow)
	if err == nil || resA.Admitted {
		t.Fatalf("absent registry must deny a targeted surface proposal: err=%v res=%+v", err, resA)
	}
	if !strings.Contains(err.Error(), "editable surface denied") {
		t.Fatalf("denial must name the editable-surfaces registry, got %v", err)
	}
	if got, _ := s.Get(a); got.State != "proposed" {
		t.Fatalf("denied surface proposal must stay proposed, got %q", got.State)
	}

	// Phase B — surface ACTIVELY whitelisted: the same eligible proposal now admits.
	if err := policy.WriteEditableSurfaces(s.home, policy.EditableSurfaces{Entries: []policy.EditableSurface{
		{Surface: "skill_descriptions", ProposalClass: "local_reversible", Status: policy.EditableSurfaceStatusActive},
	}}); err != nil {
		t.Fatalf("whitelist surface: %v", err)
	}
	b := makeSurfaceDirectorWork(t, s, "skill_descriptions", "work_surf_b", fixedNow)
	resB, err := s.AdmitAuto(b, eligible, fixedNow)
	if err != nil || !resB.Admitted {
		t.Fatalf("actively whitelisted surface must admit: err=%v res=%+v", err, resB)
	}
	if got, _ := s.Get(b); got.State != "admitted" {
		t.Fatalf("whitelisted surface proposal should be admitted, got %q", got.State)
	}

	// Phase C — surface RETIRED: a fresh proposal for the now-retired surface is denied again.
	if err := policy.WriteEditableSurfaces(s.home, policy.EditableSurfaces{Entries: []policy.EditableSurface{
		{Surface: "skill_descriptions", ProposalClass: "local_reversible", Status: policy.EditableSurfaceStatusRetired},
	}}); err != nil {
		t.Fatalf("retire surface: %v", err)
	}
	c := makeSurfaceDirectorWork(t, s, "skill_descriptions", "work_surf_c", fixedNow)
	resC, err := s.AdmitAuto(c, eligible, fixedNow)
	if err == nil || resC.Admitted {
		t.Fatalf("retired surface must deny again: err=%v res=%+v", err, resC)
	}
	if got, _ := s.Get(c); got.State != "proposed" {
		t.Fatalf("retired surface proposal must stay proposed, got %q", got.State)
	}
}

// TestEditableSurfaceHaltWins proves the two governance switches compose safely: with the
// autonomy kill-switch engaged, even an ACTIVELY whitelisted surface proposal is denied —
// the halt runs first and wins. It fails if the surface gate could route around the halt.
func TestEditableSurfaceHaltWins(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)

	// Surface actively whitelisted...
	if err := policy.WriteEditableSurfaces(s.home, policy.EditableSurfaces{Entries: []policy.EditableSurface{
		{Surface: "skill_descriptions", ProposalClass: "local_reversible", Status: policy.EditableSurfaceStatusActive},
	}}); err != nil {
		t.Fatalf("whitelist surface: %v", err)
	}
	// ...but the global autonomy halt is engaged.
	if err := policy.WriteAutonomyHalt(s.home, policy.AutonomyHalt{Global: true, Reason: "incident review"}); err != nil {
		t.Fatalf("engage global halt: %v", err)
	}

	id := makeSurfaceDirectorWork(t, s, "skill_descriptions", "work_surf_halt", fixedNow)
	res, err := s.AdmitAuto(id, eligible, fixedNow)
	if err == nil || res.Admitted {
		t.Fatalf("halt must deny even a whitelisted surface proposal: err=%v res=%+v", err, res)
	}
	if !res.Halted || !strings.Contains(err.Error(), "autonomy halted") {
		t.Fatalf("the halt (not the surface gate) must be the denial reason: halted=%t err=%v", res.Halted, err)
	}
	if got, _ := s.Get(id); got.State != "proposed" {
		t.Fatalf("halted surface proposal must stay proposed, got %q", got.State)
	}
}

// TestEditableSurfaceIgnoresProposalsWithoutTargetSurface proves the registry is additive:
// an ordinary director proposal (no target_surface) auto-admits normally even with no
// registry installed, so the EVOLVE-BLOCK gate constrains ONLY explicit harness-edit
// proposals.
func TestEditableSurfaceIgnoresProposalsWithoutTargetSurface(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	id := makeProposedDirectorWork(t, s, "job_health", "work_no_surface", fixedNow)
	res, err := s.AdmitAuto(id, eligible, fixedNow)
	if err != nil || !res.Admitted {
		t.Fatalf("ordinary proposal (no target_surface) must be unaffected by the registry: err=%v res=%+v", err, res)
	}
	if got, _ := s.Get(id); got.State != "admitted" {
		t.Fatalf("ordinary proposal should admit, got %q", got.State)
	}
}

// TestEditableSurfaceGuardsGenericAdvanceSeam proves the defense-in-depth check on the
// generic Advance seam: a system-originated advance that targets an unwhitelisted surface is
// stopped BY THE REGISTRY, not merely by the operator-provenance guard.
func TestEditableSurfaceGuardsGenericAdvanceSeam(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id := makeSurfaceDirectorWork(t, s, "steering_config", "work_surf_adv", fixedNow)
	_, err := s.Advance(id, AdvanceInput{Actor: "auto-admit:director", To: "admitted"}, fixedNow.Add(1*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "editable surface denied") {
		t.Fatalf("generic seam must deny an unwhitelisted surface admission, got %v", err)
	}
	if got, _ := s.Get(id); got.State != "proposed" {
		t.Fatalf("denied advance must leave work proposed, got %q", got.State)
	}
}

// makeUnclassifiedDirectorWork is a test seam: it bypasses BuildWork's natural
// ProposalKind stamp by writing the envelope through Create with no ProposalKind
// set, simulating a system producer that omits the classification. Used only by
// the EVOLVE-BLOCK omission-seam falsifiers below; production code paths
// (BuildWork, internal/director.proposalKindFor) always stamp ProposalKind.
func makeUnclassifiedDirectorWork(t *testing.T, s *Store, targetSurface, id string, now time.Time) string {
	t.Helper()
	plan := ensureJobHealthAutonomyPlan(t, s)
	w, err := s.Create(CreateInput{
		WorkID:           id,
		Owner:            "compound-authority",
		Actor:            "director",
		TargetSurface:    targetSurface,
		SuccessPredicate: "bypass-classification via " + id,
		State:            "proposed",
		EvidenceRefs:     []string{"proposed_by:director", "director_trigger:job_health:" + id, "route_plan:" + plan.ID},
		RouteDecisionID:  plan.ID,
	}, now)
	if err != nil {
		t.Fatalf("create unclassified proposed work: %v", err)
	}
	return w.ID
}

// TestEVOLVEOmissionClosesUnknownClassification is the round-2 oracle falsifier
// for the EVOLVE-BLOCK omission seam (oracle-prompt-scope-omission-02): a
// system-originated director-proposed work WITHOUT a ProposalKind must be
// denied at AdmitAuto AND at Advance — the empty classification cannot grant
// the ordinary-work exemption. It is the seam's headline proof.
func TestEVOLVEOmissionClosesUnknownClassification(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	id := makeUnclassifiedDirectorWork(t, s, "skill_descriptions", "work_omit_auto", fixedNow)

	// AdmitAuto path.
	res, err := s.AdmitAuto(id, eligible, fixedNow)
	if err == nil || res.Admitted {
		t.Fatalf("AdmitAuto must deny an unclassified autonomous proposal: err=%v res=%+v", err, res)
	}
	if !strings.Contains(err.Error(), "unclassified autonomous proposal") {
		t.Fatalf("AdmitAuto denial must name the missing-classification cause, got %q", err.Error())
	}
	if got, _ := s.Get(id); got.State != "proposed" {
		t.Fatalf("denied AdmitAuto must leave work proposed, got %q", got.State)
	}

	// Generic Advance path (defense in depth).
	_, err = s.Advance(id, AdvanceInput{Actor: "auto-admit:director", To: "admitted"}, fixedNow.Add(1*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "unclassified autonomous proposal") {
		t.Fatalf("Advance must deny an unclassified autonomous proposal with the same reason: %v", err)
	}
	if got, _ := s.Get(id); got.State != "proposed" {
		t.Fatalf("denied Advance must leave work proposed, got %q", got.State)
	}
}

// TestEVOLVEOmissionClosesHarnessEditMissingTarget is the second oracle
// falsifier: a harness_edit-classified proposal that omits TargetSurface is
// denied — the classification cannot paper over the missing target. The
// decision table at editableSurfaceGuard is enforced.
func TestEVOLVEOmissionClosesHarnessEditMissingTarget(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	// Surface ACTIVELY whitelisted so the harness_edit path would otherwise admit.
	if err := policy.WriteEditableSurfaces(s.home, policy.EditableSurfaces{Entries: []policy.EditableSurface{
		{Surface: "skill_descriptions", ProposalClass: "local_reversible", Status: policy.EditableSurfaceStatusActive},
	}}); err != nil {
		t.Fatalf("seed whitelist: %v", err)
	}
	// Producer stamps ProposalKind=harness_edit but the target is empty.
	plan := ensureJobHealthAutonomyPlan(t, s)
	id := "work_he_no_target"
	_, err := s.Create(CreateInput{
		WorkID:           id,
		Owner:            "compound-authority",
		Actor:            "director",
		ProposalKind:     envelope.ProposalKindHarnessEdit,
		SuccessPredicate: "edit via " + id,
		State:            "proposed",
		EvidenceRefs:     []string{"proposed_by:director", "director_trigger:job_health:" + id, "route_plan:" + plan.ID},
		RouteDecisionID:  plan.ID,
	}, fixedNow)
	if err != nil {
		t.Fatalf("create harness_edit-without-target work: %v", err)
	}
	res, err := s.AdmitAuto(id, eligible, fixedNow)
	if err == nil || res.Admitted {
		t.Fatalf("AdmitAuto must deny a harness_edit with missing target: err=%v res=%+v", err, res)
	}
	if !strings.Contains(err.Error(), "harness_edit missing target_surface") {
		t.Fatalf("denial must name the missing-target cause, got %q", err.Error())
	}
}
