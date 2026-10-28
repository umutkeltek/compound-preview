package routeevidence

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/act"
	"compound/internal/envelope"
	"compound/internal/policy"
)

// Falsifiers for the governed model-router promotion door.
//
// Every case runs in a temp home over synthetic .invalid lanes. Nothing here
// touches a live COMPOUND_HOME, and no test seeds the real registries.

const promotionApplyFixtureYAML = `version: 1
purpose: fixture model router for the governed promotion door
rules:
  - free lanes never gate a verdict
capability_classes: [classify-extract, judge-critique]
models:
  synthetic-judge.invalid:
    lane: judge-executor.invalid
    harness: switchback
    tier: premium
    packet_ceiling: large
    cost_class: api-marginal
    capabilities:
      classify-extract:
        level: capable
        evidence: bakeoff round 3 on synthetic corpus
      judge-critique:
        level: primary
        evidence: panel agreement 0.94
  synthetic-local.invalid:
    lane: local-executor.invalid
    harness: switchback
    tier: cheap
    packet_ceiling: medium
    cost_class: free
    capabilities:
      classify-extract:
        level: capable
        evidence: local probe sweep
routing_defaults:
  classify-extract: [synthetic-judge.invalid, synthetic-local.invalid]
  judge-critique: [synthetic-judge.invalid]
`

// Review ids are DERIVED from lane + capability class, and the governed door
// requires that binding to hold. Fixtures therefore derive their ids the same way
// production does rather than inventing literals, so a change to the id scheme
// cannot leave these falsifiers silently testing an unreachable path.
var (
	fixturePromoteReviewID = promotionReviewID("synthetic-judge.invalid", "classify-extract")
	fixtureDemoteReviewID  = promotionReviewID("synthetic-local.invalid", "classify-extract")
)

// promotionApplyHome builds a temp home whose model-router authority carries the
// fixture above, whose policy holds local_reversible by default, and which has an
// imported standing grant scoped to exactly this capability/class/reversibility/
// blast-radius/target. Returns the home path plus the live authority revision.
func promotionApplyHome(t *testing.T) (string, string) {
	t.Helper()
	return newPromotionApplyHome(t, true)
}

// promotionApplyHomeWithoutGrant is the same home with NO standing grant, so the
// confirm path has no allowing PolicyDecision.
func promotionApplyHomeWithoutGrant(t *testing.T) (string, string) {
	t.Helper()
	return newPromotionApplyHome(t, false)
}

func newPromotionApplyHome(t *testing.T, withGrant bool) (string, string) {
	t.Helper()
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "model-capabilities.yaml")
	if err := os.WriteFile(source, []byte(promotionApplyFixtureYAML), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := ImportModelCapabilitiesYAML(home, source, t0); err != nil {
		t.Fatalf("import fixture authority: %v", err)
	}
	seedPromotionApplyPolicy(t, home, withGrant)
	records, err := LoadModelCapabilitiesSourceAuthority(home)
	if err != nil || len(records) == 0 {
		t.Fatalf("load source authority: %v (%d records)", err, len(records))
	}
	return home, records[len(records)-1].SourceRevision
}

// seedPromotionApplyPolicy writes a TEST-HOME policy that holds local_reversible
// so a standing grant is the only thing that can allow this door, then optionally
// imports that grant through the real operator import path. It never touches a
// live COMPOUND_HOME: every path here is under t.TempDir().
func seedPromotionApplyPolicy(t *testing.T, home string, withGrant bool) {
	t.Helper()
	configDir := filepath.Join(home, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	// Hold every non-read class, so nothing about this door is allowed by baseline.
	policyYAML := "class_outcome:\n" +
		"  read:\n    outcome: allow\n" +
		"  local_reversible:\n    outcome: hold\n" +
		"  external_side_effect:\n    outcome: hold\n" +
		"  public:\n    outcome: hold\n" +
		"  destructive:\n    outcome: hold\n" +
		"  financial:\n    outcome: hold\n" +
		"  security:\n    outcome: hold\n"
	if err := os.WriteFile(filepath.Join(configDir, "policy.yaml"), []byte(policyYAML), 0o644); err != nil {
		t.Fatalf("write policy.yaml: %v", err)
	}
	if !withGrant {
		return
	}
	grantSrc := filepath.Join(t.TempDir(), "grants.yaml")
	raw := "generated_at: \"2026-08-01T12:00:00Z\"\n" +
		"authority:\n" +
		"  decision_ref: \"grant:test-route-promotion-apply\"\n" +
		"  reviewed_by: \"operator:test\"\n" +
		"grants:\n" +
		"  - grant_id: grant_route_promotion_apply\n" +
		"    capabilities: [\"" + PromotionApplyCapability + "\"]\n" +
		"    target_scope: \"" + ModelCapabilitiesPath(home) + "\"\n" +
		"    mutation_class: local_reversible\n" +
		"    reversibility_ceiling: reversible\n" +
		"    blast_radius: local\n"
	if err := os.WriteFile(grantSrc, []byte(raw), 0o644); err != nil {
		t.Fatalf("write grant source: %v", err)
	}
	if _, err := policy.ImportStandingGrants(home, grantSrc, t0); err != nil {
		t.Fatalf("import standing grants: %v", err)
	}
}

// promotionReviewFile writes a synthetic route-promotion review artifact bound to
// the live authority revision. It is always review_required: the governed door
// applies operator review-debt only. A demote emitted by EvaluateLanePromotions
// carries `recorded` provenance and is deliberately NOT appliable, so a demote
// that should move authority has to be raised as a review_required artifact.
func promotionReviewFile(t *testing.T, home, revision, id, recommendation, laneID, class, current, recommended string) string {
	t.Helper()
	review := PromotionReview{
		SchemaVersion:                 PromotionReviewSchemaVersion,
		ID:                            id,
		CreatedAt:                     rfc3339(t0),
		Status:                        promotionReviewStatusReviewRequired,
		Recommendation:                recommendation,
		Reason:                        "criteria satisfied on synthetic evidence",
		LaneID:                        laneID,
		CapabilityClass:               class,
		CurrentClaimLevel:             current,
		RecommendedClaimLevel:         recommended,
		EvidenceRefs:                  []string{"rev_synthetic_0001"},
		ModelCapabilitiesAuthorityRef: ModelCapabilitiesAuthorityRef,
		ModelCapabilitiesPath:         ModelCapabilitiesPath(home),
		ModelCapabilitiesSourceID:     "mcsa_synthetic",
		ModelCapabilitiesRevision:     revision,
		CriteriaAuthorityRef:          PromotionCriteriaAuthorityRef,
		CriteriaPath:                  PromotionCriteriaPath(home),
		CriteriaSourceID:              "pcsa_synthetic",
		CriteriaRevision:              "sha256:criteria-fixture",
	}
	path := filepath.Join(t.TempDir(), id+".json")
	body, err := json.MarshalIndent(review, "", "  ")
	if err != nil {
		t.Fatalf("marshal review: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write review: %v", err)
	}
	return path
}

// mutateReviewFile rewrites one field of a review artifact on disk, so a
// tampered/stale input can be proven to fail closed.
func mutateReviewFile(t *testing.T, path string, mutate func(map[string]any)) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read review: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("parse review: %v", err)
	}
	mutate(payload)
	out := filepath.Join(t.TempDir(), "mutated.json")
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal mutated review: %v", err)
	}
	if err := os.WriteFile(out, body, 0o644); err != nil {
		t.Fatalf("write mutated review: %v", err)
	}
	return out
}

// authorityFingerprint captures every byte-surface a refused apply must leave
// untouched: the authority YAML, the source-authority ledger, and the event dir.
type authorityFingerprint struct {
	authority []byte
	ledger    []byte
	events    []string
}

func fingerprintAuthority(t *testing.T, home string) authorityFingerprint {
	t.Helper()
	authority, err := os.ReadFile(ModelCapabilitiesPath(home))
	if err != nil {
		t.Fatalf("read authority: %v", err)
	}
	ledger, _ := os.ReadFile(ModelCapabilitiesSourceAuthorityPath(home))
	var events []string
	_ = filepath.Walk(filepath.Join(home, "events"), func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			events = append(events, path)
		}
		return nil
	})
	return authorityFingerprint{authority: authority, ledger: ledger, events: events}
}

func (f authorityFingerprint) assertUnchanged(t *testing.T, home, label string) {
	t.Helper()
	got := fingerprintAuthority(t, home)
	if string(got.authority) != string(f.authority) {
		t.Fatalf("%s: authority bytes changed", label)
	}
	if string(got.ledger) != string(f.ledger) {
		t.Fatalf("%s: source-authority ledger changed", label)
	}
	if len(got.events) != len(f.events) {
		t.Fatalf("%s: event directory changed (%d -> %d entries)", label, len(f.events), len(got.events))
	}
}

// allowingDecision evaluates the real policy for the real intent. The default
// policy allows a local_reversible/easy/local action with obligations, which is
// exactly the envelope this door declares.
func allowingDecision(t *testing.T, home, work string, plan PromotionApplyPlan, now time.Time) (envelope.ActionIntent, envelope.PolicyDecision) {
	t.Helper()
	cfg, _, err := policy.LoadConfig(home)
	if err != nil {
		t.Fatalf("load policy config: %v", err)
	}
	ai := PromotionApplyIntent(work, plan, now)
	pd := policy.Evaluate(ai, cfg, now)
	if !policy.Allowed(pd) {
		t.Fatalf("expected an allowing decision for the governed door, got %s: %v", pd.Outcome, pd.Reasons)
	}
	return ai, pd
}

// FALSIFIER 1 — reviewed promote. Dry-run leaves every byte identical; a
// confirmed apply changes exactly one claim, returns the full proof chain, writes
// the applied record, and route planning reflects the new level.
func TestPromotionApplyReviewedPromoteMutatesOnlyTheReviewedClaim(t *testing.T) {
	home, revision := promotionApplyHome(t)
	reviewPath := promotionReviewFile(t, home, revision, fixturePromoteReviewID,
		promotionRecommendationPromote, "synthetic-judge.invalid", "classify-extract", "capable", "primary")

	before := fingerprintAuthority(t, home)

	// Dry-run: planning alone must not write.
	plan, err := PlanPromotionApply(home, reviewPath, "operator reviewed bakeoff evidence")
	if err != nil {
		t.Fatalf("plan promote: %v", err)
	}
	before.assertUnchanged(t, home, "dry-run plan")
	if plan.CurrentClaimLevel != "capable" || plan.ProposedClaimLevel != "primary" {
		t.Fatalf("bad plan levels: %+v", plan)
	}

	ai, pd := allowingDecision(t, home, "work_promote_fixture", plan, t0)
	res, err := ApplyPromotionReview(home, "work_promote_fixture", plan, pd, t0)
	if err != nil {
		t.Fatalf("apply promote: %v", err)
	}

	// Full proof chain present and resolvable in the home.
	rec := res.Record
	if rec.ActionIntentID != ai.ID || rec.PolicyDecisionID != pd.ID ||
		rec.ReceiptID == "" || rec.TransitionID == "" ||
		rec.PreImageRef == "" || rec.PostImageRef == "" {
		t.Fatalf("incomplete proof chain: %+v", rec)
	}
	if rec.AuthorityRevisionBefore == rec.AuthorityRevisionAfter {
		t.Fatalf("authority revision did not move: %s", rec.AuthorityRevisionAfter)
	}
	if _, err := os.Stat(PromotionAppliedRecordPath(home, fixturePromoteReviewID)); err != nil {
		t.Fatalf("applied record not written: %v", err)
	}

	// Exactly one claim moved; every other row, claim, default, and rule survived.
	after, _, _, err := loadModelCapabilitiesSnapshot(home)
	if err != nil {
		t.Fatalf("reload authority: %v", err)
	}
	if got := after.Models["synthetic-judge.invalid"].Capabilities["classify-extract"].Level; got != "primary" {
		t.Fatalf("reviewed claim not applied: %q", got)
	}
	if got := after.Models["synthetic-judge.invalid"].Capabilities["judge-critique"].Level; got != "primary" {
		t.Fatalf("sibling claim disturbed: %q", got)
	}
	if got := after.Models["synthetic-local.invalid"].Capabilities["classify-extract"].Level; got != "capable" {
		t.Fatalf("other lane claim disturbed: %q", got)
	}
	if len(after.Models) != 2 || len(after.RoutingDefaults) != 2 || len(after.Rules) != 1 {
		t.Fatalf("manifest shape changed: models=%d defaults=%d rules=%d", len(after.Models), len(after.RoutingDefaults), len(after.Rules))
	}

	// FALSIFIER 4 — provenance: the installed claim names the review and criteria.
	evidence := after.Models["synthetic-judge.invalid"].Capabilities["classify-extract"].Evidence
	for _, want := range []string{fixturePromoteReviewID, "pcsa_synthetic", "sha256:criteria-fixture", "operator reviewed bakeoff evidence", "bakeoff round 3"} {
		if !strings.Contains(evidence, want) {
			t.Fatalf("installed evidence missing %q: %q", want, evidence)
		}
	}
	if !strings.Contains(rec.AppliedClaimEvidence, fixturePromoteReviewID) || rec.CriteriaRevision != "sha256:criteria-fixture" {
		t.Fatalf("applied record provenance incomplete: %+v", rec)
	}

	// Route planning consumes the new level.
	routePlan, err := PlanCapabilityRoute(home, "classify-extract", CapabilityRoutePlanOptions{}, t0)
	if err != nil {
		t.Fatalf("plan classify-extract route: %v", err)
	}
	if len(routePlan.Chain) == 0 || routePlan.Chain[0].ModelID != "synthetic-judge.invalid" || routePlan.Chain[0].ClaimLevel != "primary" {
		t.Fatalf("route plan does not reflect the applied claim: %+v", routePlan.Chain)
	}
}

// FALSIFIER 2 — reviewed demote. Same proof chain; everything else identical.
func TestPromotionApplyReviewedDemoteMutatesOnlyTheReviewedClaim(t *testing.T) {
	home, revision := promotionApplyHome(t)
	reviewPath := promotionReviewFile(t, home, revision, fixtureDemoteReviewID,
		promotionRecommendationDemote, "synthetic-local.invalid", "classify-extract", "capable", "assumed")

	plan, err := PlanPromotionApply(home, reviewPath, "operator reviewed failing-rate evidence")
	if err != nil {
		t.Fatalf("plan demote: %v", err)
	}
	_, pd := allowingDecision(t, home, "work_demote_fixture", plan, t0)
	res, err := ApplyPromotionReview(home, "work_demote_fixture", plan, pd, t0)
	if err != nil {
		t.Fatalf("apply demote: %v", err)
	}
	if res.Record.ReceiptID == "" || res.Record.TransitionID == "" || res.Record.PreImageRef == "" {
		t.Fatalf("incomplete demote proof chain: %+v", res.Record)
	}

	after, _, _, err := loadModelCapabilitiesSnapshot(home)
	if err != nil {
		t.Fatalf("reload authority: %v", err)
	}
	if got := after.Models["synthetic-local.invalid"].Capabilities["classify-extract"].Level; got != "assumed" {
		t.Fatalf("demote not applied: %q", got)
	}
	if got := after.Models["synthetic-judge.invalid"].Capabilities["classify-extract"].Level; got != "capable" {
		t.Fatalf("other lane claim disturbed by demote: %q", got)
	}
	if got := after.Models["synthetic-judge.invalid"].Capabilities["judge-critique"].Evidence; got != "panel agreement 0.94" {
		t.Fatalf("untouched claim evidence rewritten: %q", got)
	}
	if len(after.RoutingDefaults["classify-extract"]) != 2 {
		t.Fatalf("routing defaults disturbed: %+v", after.RoutingDefaults)
	}
}

// FALSIFIER 3 — fail closed. Every malformed, stale, mismatched, or
// already-resolved input refuses BEFORE any mutation, leaving the authority, the
// source-authority ledger, and the event directory byte-identical.
func TestPromotionApplyFailsClosedBeforeAnyAuthorityMutation(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, home, revision, reviewPath string) string
		wantErr string
	}{
		{
			name: "hold recommendation",
			prepare: func(t *testing.T, home, revision, reviewPath string) string {
				return mutateReviewFile(t, reviewPath, func(p map[string]any) {
					p["recommendation"] = promotionRecommendationHoldInsufficient
				})
			},
			wantErr: "not applicable",
		},
		{
			name: "missing recommended claim",
			prepare: func(t *testing.T, home, revision, reviewPath string) string {
				return mutateReviewFile(t, reviewPath, func(p map[string]any) {
					p["recommended_claim_level"] = ""
				})
			},
			wantErr: "no recommended_claim_level",
		},
		{
			name: "stale current claim",
			prepare: func(t *testing.T, home, revision, reviewPath string) string {
				return mutateReviewFile(t, reviewPath, func(p map[string]any) {
					p["current_claim_level"] = "assumed"
				})
			},
			wantErr: "stale",
		},
		{
			name: "stale model authority revision",
			prepare: func(t *testing.T, home, revision, reviewPath string) string {
				return mutateReviewFile(t, reviewPath, func(p map[string]any) {
					p["model_capabilities_revision"] = "sha256:deadbeefdeadbeef"
				})
			},
			wantErr: "stale",
		},
		{
			name: "mismatched lane",
			prepare: func(t *testing.T, home, revision, reviewPath string) string {
				return mutateReviewFile(t, reviewPath, func(p map[string]any) {
					p["lane_id"] = "ghost/lane.invalid"
				})
			},
			wantErr: "absent from",
		},
		{
			name: "mismatched capability class",
			prepare: func(t *testing.T, home, revision, reviewPath string) string {
				return mutateReviewFile(t, reviewPath, func(p map[string]any) {
					p["capability_class"] = "not-a-class"
				})
			},
			wantErr: "does not claim",
		},
		{
			name: "recorded status is not appliable",
			prepare: func(t *testing.T, home, revision, reviewPath string) string {
				return mutateReviewFile(t, reviewPath, func(p map[string]any) {
					p["status"] = promotionReviewStatusFiled
				})
			},
			wantErr: "not appliable",
		},
		{
			name: "unknown status is not appliable",
			prepare: func(t *testing.T, home, revision, reviewPath string) string {
				return mutateReviewFile(t, reviewPath, func(p map[string]any) {
					p["status"] = "approved_by_hand"
				})
			},
			wantErr: "not appliable",
		},
		{
			name: "re-pointed review id",
			prepare: func(t *testing.T, home, revision, reviewPath string) string {
				return mutateReviewFile(t, reviewPath, func(p map[string]any) {
					p["id"] = "route_promotion_handauthored"
				})
			},
			wantErr: "does not match its lane/class",
		},
		{
			name: "promote that lowers the claim",
			prepare: func(t *testing.T, home, revision, reviewPath string) string {
				return mutateReviewFile(t, reviewPath, func(p map[string]any) {
					p["recommended_claim_level"] = "assumed"
				})
			},
			wantErr: "does not raise the claim",
		},
		{
			name: "demote that raises the claim",
			prepare: func(t *testing.T, home, revision, reviewPath string) string {
				return mutateReviewFile(t, reviewPath, func(p map[string]any) {
					p["recommendation"] = promotionRecommendationDemote
				})
			},
			wantErr: "does not lower the claim",
		},
		{
			name: "unknown recommended claim level",
			prepare: func(t *testing.T, home, revision, reviewPath string) string {
				return mutateReviewFile(t, reviewPath, func(p map[string]any) {
					p["recommended_claim_level"] = "godmode"
				})
			},
			wantErr: "not a known claim level",
		},
		{
			name: "malformed json",
			prepare: func(t *testing.T, home, revision, reviewPath string) string {
				path := filepath.Join(t.TempDir(), "malformed.json")
				if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
					t.Fatalf("write malformed: %v", err)
				}
				return path
			},
			wantErr: "parse route promotion review",
		},
		{
			name: "already hold-recorded",
			prepare: func(t *testing.T, home, revision, reviewPath string) string {
				dir := filepath.Join(home, "reviews", routePromotionReviewArea)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("mkdir reviews: %v", err)
				}
				if err := os.WriteFile(promotionHoldRecordPath(home, fixturePromoteReviewID), []byte("{}"), 0o644); err != nil {
					t.Fatalf("seed hold record: %v", err)
				}
				return reviewPath
			},
			wantErr: "already recorded as a hold",
		},
		{
			name: "already applied",
			prepare: func(t *testing.T, home, revision, reviewPath string) string {
				dir := filepath.Join(home, "reviews", routePromotionReviewArea)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("mkdir reviews: %v", err)
				}
				if err := os.WriteFile(PromotionAppliedRecordPath(home, fixturePromoteReviewID), []byte("{}"), 0o644); err != nil {
					t.Fatalf("seed applied record: %v", err)
				}
				return reviewPath
			},
			wantErr: "already has an applied record",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, revision := promotionApplyHome(t)
			reviewPath := promotionReviewFile(t, home, revision, fixturePromoteReviewID,
				promotionRecommendationPromote, "synthetic-judge.invalid", "classify-extract", "capable", "primary")
			before := fingerprintAuthority(t, home)

			input := tc.prepare(t, home, revision, reviewPath)

			_, err := PlanPromotionApply(home, input, "operator rationale")
			if err == nil {
				t.Fatalf("expected refusal for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("refusal for %s should mention %q, got %v", tc.name, tc.wantErr, err)
			}
			before.assertUnchanged(t, home, tc.name)
		})
	}
}

// FALSIFIER 3 (grant control) — with NO standing grant in the test home, the
// confirm path has no allowing PolicyDecision, refuses, and leaves the authority,
// the source-authority ledger, and the event directory byte-identical. The
// paired allow-case above proves the grant is what makes the difference, so this
// gate is load-bearing rather than decorative.
func TestPromotionApplyRefusesWhenNoStandingGrantCoversIt(t *testing.T) {
	home, revision := promotionApplyHomeWithoutGrant(t)
	reviewPath := promotionReviewFile(t, home, revision, fixturePromoteReviewID,
		promotionRecommendationPromote, "synthetic-judge.invalid", "classify-extract", "capable", "primary")
	plan, err := PlanPromotionApply(home, reviewPath, "operator rationale")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	before := fingerprintAuthority(t, home)

	cfg, _, err := policy.LoadConfig(home)
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	ai := PromotionApplyIntent("work_no_grant", plan, t0)
	pd := policy.Evaluate(ai, cfg, t0)
	if policy.Allowed(pd) {
		t.Fatalf("without a standing grant the decision must not allow, got %s: %v", pd.Outcome, pd.Reasons)
	}

	if _, err := ApplyPromotionReview(home, "work_no_grant", plan, pd, t0); err == nil {
		t.Fatal("apply must refuse without an allowing PolicyDecision")
	}
	before.assertUnchanged(t, home, "absent standing grant")
	if _, err := os.Stat(PromotionAppliedRecordPath(home, fixturePromoteReviewID)); err == nil {
		t.Fatal("a refused apply must not write an applied record")
	}
}

// The SAME home, SAME plan, SAME confirm path, but WITH the grant: proves the
// refusal above is caused by the missing grant and nothing else.
func TestPromotionApplySucceedsOnlyBecauseOfTheStandingGrant(t *testing.T) {
	home, revision := promotionApplyHome(t)
	reviewPath := promotionReviewFile(t, home, revision, fixturePromoteReviewID,
		promotionRecommendationPromote, "synthetic-judge.invalid", "classify-extract", "capable", "primary")
	plan, err := PlanPromotionApply(home, reviewPath, "operator rationale")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	cfg, _, err := policy.LoadConfig(home)
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	ai := PromotionApplyIntent("work_with_grant", plan, t0)
	pd := policy.Evaluate(ai, cfg, t0)
	if !policy.Allowed(pd) {
		t.Fatalf("the seeded standing grant must allow this exact action, got %s: %v", pd.Outcome, pd.Reasons)
	}
	if pd.StandingGrantID != "grant_route_promotion_apply" {
		t.Fatalf("the allow must come from the seeded grant, got %q", pd.StandingGrantID)
	}
	if _, err := ApplyPromotionReview(home, "work_with_grant", plan, pd, t0); err != nil {
		t.Fatalf("granted apply should succeed: %v", err)
	}
}

// FALSIFIER 3 (TOCTOU control) — the authority moving between plan and confirm is
// a refusal, not a silent overwrite of the concurrent lane's change.
func TestPromotionApplyRefusesWhenAuthorityMovesBetweenPlanAndConfirm(t *testing.T) {
	home, revision := promotionApplyHome(t)
	reviewPath := promotionReviewFile(t, home, revision, fixturePromoteReviewID,
		promotionRecommendationPromote, "synthetic-judge.invalid", "classify-extract", "capable", "primary")
	plan, err := PlanPromotionApply(home, reviewPath, "operator rationale")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	_, pd := allowingDecision(t, home, "work_toctou", plan, t0)

	// A concurrent lane changes the authority after the plan was validated.
	concurrent := filepath.Join(t.TempDir(), "concurrent.yaml")
	body := `version: 1
purpose: concurrent lane change
rules: []
capability_classes: [classify-extract]
models:
  synthetic-local.invalid:
    lane: local-executor.invalid
    tier: cheap
    packet_ceiling: medium
    capabilities:
      classify-extract:
        level: assumed
        evidence: concurrent lane change
routing_defaults:
  classify-extract: [synthetic-local.invalid]
`
	if err := os.WriteFile(concurrent, []byte(body), 0o644); err != nil {
		t.Fatalf("write concurrent input: %v", err)
	}
	if _, err := ImportModelCapabilitiesYAML(home, concurrent, t0); err != nil {
		t.Fatalf("concurrent import: %v", err)
	}
	before := fingerprintAuthority(t, home)

	if _, err := ApplyPromotionReview(home, "work_toctou", plan, pd, t0); err == nil {
		t.Fatal("apply must refuse when the authority moved between plan and confirm")
	}
	before.assertUnchanged(t, home, "toctou")
}

// The plan is an exported preview value, not mutation authority. Execution must
// rebuild it from the review path immediately before the act and must ignore a
// caller-replaced ProposedYAML payload, even when the PolicyDecision was valid
// for the original preview.
func TestPromotionApplyReplansAndIgnoresForgedPlanPayload(t *testing.T) {
	home, revision := promotionApplyHome(t)
	reviewPath := promotionReviewFile(t, home, revision, fixturePromoteReviewID,
		promotionRecommendationPromote, "synthetic-judge.invalid", "classify-extract", "capable", "primary")
	plan, err := PlanPromotionApply(home, reviewPath, "operator rationale")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	_, pd := allowingDecision(t, home, "work_forged_apply_plan", plan, t0)

	plan.ProposedYAML = []byte(`version: 1
purpose: attacker supplied plan payload
rules: []
capability_classes: [classify-extract]
models:
  attacker/model.invalid:
    lane: attacker.invalid
    tier: premium
    packet_ceiling: large
    capabilities:
      classify-extract:
        level: primary
        evidence: forged exported plan
routing_defaults:
  classify-extract: [attacker/model.invalid]
`)

	if _, err := ApplyPromotionReview(home, "work_forged_apply_plan", plan, pd, t0); err != nil {
		t.Fatalf("canonical reviewed apply should still succeed: %v", err)
	}
	manifest, _, _, err := loadModelCapabilitiesSnapshot(home)
	if err != nil {
		t.Fatalf("reload authority: %v", err)
	}
	if _, ok := manifest.Models["attacker/model.invalid"]; ok {
		t.Fatal("caller-supplied ProposedYAML reached authority")
	}
	if got := manifest.Models["synthetic-judge.invalid"].Capabilities["classify-extract"].Level; got != "primary" {
		t.Fatalf("canonical reviewed claim was not applied: %q", got)
	}
}

// A PolicyDecision must bind both sides of the authority transition. The
// proposed revision commits to the exact YAML bytes (and therefore rationale +
// provenance), while the authority revision commits to the before image.
func TestPromotionApplyIntentBindsBeforeAndProposedRevisions(t *testing.T) {
	home, revision := promotionApplyHome(t)
	reviewPath := promotionReviewFile(t, home, revision, fixturePromoteReviewID,
		promotionRecommendationPromote, "synthetic-judge.invalid", "classify-extract", "capable", "primary")
	plan, err := PlanPromotionApply(home, reviewPath, "operator rationale")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	wantProposedRevision := "sha256:" + digestHex(plan.ProposedYAML)
	if plan.ProposedRevision != wantProposedRevision {
		t.Fatalf("proposed revision = %q, want %q", plan.ProposedRevision, wantProposedRevision)
	}

	baseID := PromotionApplyIntent("work_revision_binding", plan, t0).ID
	changedBefore := plan
	changedBefore.AuthorityRevision = "sha256:" + strings.Repeat("b", 64)
	if got := PromotionApplyIntent("work_revision_binding", changedBefore, t0).ID; got == baseID {
		t.Fatal("action intent id did not bind the before authority revision")
	}
	changedAfter := plan
	changedAfter.ProposedRevision = "sha256:" + strings.Repeat("a", 64)
	if got := PromotionApplyIntent("work_revision_binding", changedAfter, t0).ID; got == baseID {
		t.Fatal("action intent id did not bind the proposed authority revision")
	}
}

// FALSIFIER 5 (evidence-binding control) — a hand-authored or tampered applied
// record cannot drive a governed restore. Without this, promotion-revert would be
// a write primitive aimed at arbitrary attacker-named artifact bytes.
func TestPromotionRevertRejectsAppliedRecordsNotBackedByCanonicalEvidence(t *testing.T) {
	build := func(t *testing.T) (string, string, PromotionAppliedRecord) {
		t.Helper()
		home, revision := promotionApplyHome(t)
		reviewPath := promotionReviewFile(t, home, revision, fixturePromoteReviewID,
			promotionRecommendationPromote, "synthetic-judge.invalid", "classify-extract", "capable", "primary")
		plan, err := PlanPromotionApply(home, reviewPath, "operator rationale")
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		_, pd := allowingDecision(t, home, "work_evidence", plan, t0)
		if _, err := ApplyPromotionReview(home, "work_evidence", plan, pd, t0); err != nil {
			t.Fatalf("apply: %v", err)
		}
		appliedPath := PromotionAppliedRecordPath(home, fixturePromoteReviewID)
		record, err := LoadPromotionAppliedRecord(appliedPath)
		if err != nil {
			t.Fatalf("load applied record: %v", err)
		}
		return home, appliedPath, record
	}

	cases := []struct {
		name    string
		tamper  func(r *PromotionAppliedRecord)
		wantErr string
	}{
		{
			name:    "forged receipt id",
			tamper:  func(r *PromotionAppliedRecord) { r.ReceiptID = "rec_forged000000" },
			wantErr: "does not resolve",
		},
		{
			name:    "forged transition id",
			tamper:  func(r *PromotionAppliedRecord) { r.TransitionID = "tr_forged000000" },
			wantErr: "does not resolve",
		},
		{
			name:    "swapped pre-image ref",
			tamper:  func(r *PromotionAppliedRecord) { r.PreImageRef = r.PostImageRef },
			wantErr: "disagrees with",
		},
		{
			name: "forged action intent id",
			tamper: func(r *PromotionAppliedRecord) {
				r.ActionIntentID = "act_forged000000"
				r.ActionIntent.ID = "act_forged000000"
			},
			wantErr: "transition",
		},
		// The embedded intent BODY is untrusted even when every id is genuine.
		// These keep the real receipt/transition/intent ids and change only the
		// body: without the deterministic rebuild, the revert would restore a
		// captured blob over whatever target the tampered body named.
		{
			// Caught by the earlier target-drift guard, which compares the embedded
			// target against the LIVE authority path before the body binding runs.
			name:    "retargeted embedded intent path",
			tamper:  func(r *PromotionAppliedRecord) { r.ActionIntent.Target.Path = "/tmp/attacker-owned.invalid.yaml" },
			wantErr: "refusing to revert across targets",
		},
		{
			name:    "retargeted embedded intent target id",
			tamper:  func(r *PromotionAppliedRecord) { r.ActionIntent.Target.ID = "other-lane.invalid/judge-critique" },
			wantErr: "target.id",
		},
		{
			name:    "escalated embedded capability",
			tamper:  func(r *PromotionAppliedRecord) { r.ActionIntent.Capability = "outbound.anything.write" },
			wantErr: "capability",
		},
		{
			name:    "relaxed embedded mutation class",
			tamper:  func(r *PromotionAppliedRecord) { r.ActionIntent.MutationClass = "read" },
			wantErr: "mutation_class",
		},
		{
			name:    "widened embedded blast radius",
			tamper:  func(r *PromotionAppliedRecord) { r.ActionIntent.BlastRadius = "fleet" },
			wantErr: "blast_radius",
		},
		{
			name:    "swapped applied claim level",
			tamper:  func(r *PromotionAppliedRecord) { r.AppliedClaimLevel = "avoid" },
			wantErr: "does not reproduce its own action intent id",
		},
		{
			name:    "swapped authority path",
			tamper:  func(r *PromotionAppliedRecord) { r.AuthorityPath = "/tmp/other-authority.invalid.yaml" },
			wantErr: "targets",
		},
		// Both revision strings are DERIVED facts, not record claims. A forged
		// authority_revision_before would stamp the ledger with a revision the
		// restored file does not carry, splitting authority from ledger.
		{
			name:    "forged authority_revision_before",
			tamper:  func(r *PromotionAppliedRecord) { r.AuthorityRevisionBefore = "sha256:" + strings.Repeat("b", 64) },
			wantErr: "does not reproduce its own action intent id",
		},
		{
			name:    "forged authority_revision_after",
			tamper:  func(r *PromotionAppliedRecord) { r.AuthorityRevisionAfter = "sha256:" + strings.Repeat("a", 64) },
			wantErr: "does not reproduce its own action intent id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, appliedPath, record := build(t)
			// Sanity: the untampered record IS revertible, so a red below is caused
			// by the tampering and not by an unrelated precondition.
			if _, err := PlanPromotionRevert(home, appliedPath); err != nil {
				t.Fatalf("untampered record should be revertible: %v", err)
			}

			tc.tamper(&record)
			body, err := json.MarshalIndent(record, "", "  ")
			if err != nil {
				t.Fatalf("marshal tampered record: %v", err)
			}
			if err := os.WriteFile(appliedPath, body, 0o644); err != nil {
				t.Fatalf("write tampered record: %v", err)
			}

			authorityBefore, _ := os.ReadFile(ModelCapabilitiesPath(home))
			_, err = PlanPromotionRevert(home, appliedPath)
			if err == nil {
				t.Fatalf("%s must be refused", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("%s refusal should mention %q, got %v", tc.name, tc.wantErr, err)
			}
			authorityAfter, _ := os.ReadFile(ModelCapabilitiesPath(home))
			if string(authorityBefore) != string(authorityAfter) {
				t.Fatalf("%s: a refused revert must not touch the authority", tc.name)
			}
		})
	}
}

// The exported revert plan is only a preview. Execution must re-plan from the
// applied-record path and restore the canonical content-addressed pre-image,
// never bytes substituted into plan.PreImage by a caller after policy review.
func TestPromotionRevertReplansAndIgnoresForgedPlanPreImage(t *testing.T) {
	home, revision := promotionApplyHome(t)
	original, err := os.ReadFile(ModelCapabilitiesPath(home))
	if err != nil {
		t.Fatalf("read original authority: %v", err)
	}
	reviewPath := promotionReviewFile(t, home, revision, fixturePromoteReviewID,
		promotionRecommendationPromote, "synthetic-judge.invalid", "classify-extract", "capable", "primary")
	applyPlan, err := PlanPromotionApply(home, reviewPath, "operator rationale")
	if err != nil {
		t.Fatalf("plan apply: %v", err)
	}
	_, applyPD := allowingDecision(t, home, "work_forged_revert_plan", applyPlan, t0)
	if _, err := ApplyPromotionReview(home, "work_forged_revert_plan", applyPlan, applyPD, t0); err != nil {
		t.Fatalf("apply: %v", err)
	}
	appliedPath := PromotionAppliedRecordPath(home, fixturePromoteReviewID)
	revertPlan, err := PlanPromotionRevert(home, appliedPath)
	if err != nil {
		t.Fatalf("plan revert: %v", err)
	}
	record, err := LoadPromotionAppliedRecord(appliedPath)
	if err != nil {
		t.Fatalf("load applied record: %v", err)
	}
	cfg, _, err := policy.LoadConfig(home)
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	revertAI := PromotionRevertIntent("work_forged_revert_plan", record, t0)
	revertPD := policy.Evaluate(revertAI, cfg, t0)
	if !policy.Allowed(revertPD) {
		t.Fatalf("revert decision should allow, got %s", revertPD.Outcome)
	}

	revertPlan.PreImage = []byte(`version: 1
purpose: attacker supplied revert payload
rules: []
capability_classes: [classify-extract]
models:
  attacker/model.invalid:
    lane: attacker.invalid
    tier: premium
    packet_ceiling: large
    capabilities:
      classify-extract:
        level: primary
        evidence: forged exported revert plan
routing_defaults:
  classify-extract: [attacker/model.invalid]
`)
	if _, err := RevertPromotionApply(home, "work_forged_revert_plan", revertPlan, revertPD, t0); err != nil {
		t.Fatalf("canonical revert should still succeed: %v", err)
	}
	restored, err := os.ReadFile(ModelCapabilitiesPath(home))
	if err != nil {
		t.Fatalf("read restored authority: %v", err)
	}
	if string(restored) != string(original) {
		t.Fatal("revert executed caller-supplied plan.PreImage instead of the canonical artifact")
	}
}

// FALSIFIER 5 — undo. The recorded one-line undo restores the EXACT pre-image,
// produces its own receipt/transition, and refuses once the post-image revision
// no longer matches.
func TestPromotionRevertRestoresExactPreImageAndRefusesOnDrift(t *testing.T) {
	home, revision := promotionApplyHome(t)
	original, err := os.ReadFile(ModelCapabilitiesPath(home))
	if err != nil {
		t.Fatalf("read original authority: %v", err)
	}
	reviewPath := promotionReviewFile(t, home, revision, fixturePromoteReviewID,
		promotionRecommendationPromote, "synthetic-judge.invalid", "classify-extract", "capable", "primary")

	plan, err := PlanPromotionApply(home, reviewPath, "operator rationale")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	_, pd := allowingDecision(t, home, "work_undo_fixture", plan, t0)
	applied, err := ApplyPromotionReview(home, "work_undo_fixture", plan, pd, t0)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	appliedPath := PromotionAppliedRecordPath(home, fixturePromoteReviewID)

	// The recorded undo command is literal and names this exact record.
	if !strings.Contains(applied.Record.UndoCommand, "compound route promotion-revert --applied "+appliedPath) ||
		!strings.Contains(applied.Record.UndoCommand, "--confirm") {
		t.Fatalf("undo command is not a runnable one-liner: %q", applied.Record.UndoCommand)
	}

	revertPlan, err := PlanPromotionRevert(home, appliedPath)
	if err != nil {
		t.Fatalf("plan revert: %v", err)
	}
	record, err := LoadPromotionAppliedRecord(appliedPath)
	if err != nil {
		t.Fatalf("load applied record: %v", err)
	}
	cfg, _, err := policy.LoadConfig(home)
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	revertAI := PromotionRevertIntent("work_undo_fixture", record, t0)
	revertPD := policy.Evaluate(revertAI, cfg, t0)
	if !policy.Allowed(revertPD) {
		t.Fatalf("revert decision should allow, got %s", revertPD.Outcome)
	}
	revertRes, err := RevertPromotionApply(home, "work_undo_fixture", revertPlan, revertPD, t0)
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if revertRes.ReceiptID == "" || revertRes.TransitionID == "" ||
		revertRes.ReceiptID == applied.Record.ReceiptID || revertRes.TransitionID == applied.Record.TransitionID {
		t.Fatalf("revert must produce its own receipt/transition: %+v", revertRes)
	}

	restored, err := os.ReadFile(ModelCapabilitiesPath(home))
	if err != nil {
		t.Fatalf("read restored authority: %v", err)
	}
	if string(restored) != string(original) {
		t.Fatalf("revert did not restore the exact pre-image bytes")
	}

	// Drift: the record now points at a post-image revision the authority no
	// longer carries, so a second revert must refuse.
	if _, err := PlanPromotionRevert(home, appliedPath); err == nil {
		t.Fatal("revert must refuse once the post-image revision no longer matches")
	}

	// After the revert, route planning still works: the source-authority ledger
	// head followed the restore instead of naming a reverted-away revision.
	if _, err := PlanCapabilityRoute(home, "classify-extract", CapabilityRoutePlanOptions{}, t0); err != nil {
		t.Fatalf("route planning broken after revert: %v", err)
	}
}

// FALSIFIER 5 (drift control) — a revert must refuse when the authority moved
// after the apply, rather than silently discarding the newer change. This proves
// the revision guard has teeth independently of the record's own status field.
func TestPromotionRevertRefusesWhenAuthorityMovedAfterApply(t *testing.T) {
	home, revision := promotionApplyHome(t)
	reviewPath := promotionReviewFile(t, home, revision, fixturePromoteReviewID,
		promotionRecommendationPromote, "synthetic-judge.invalid", "classify-extract", "capable", "primary")
	plan, err := PlanPromotionApply(home, reviewPath, "operator rationale")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	_, pd := allowingDecision(t, home, "work_drift_fixture", plan, t0)
	if _, err := ApplyPromotionReview(home, "work_drift_fixture", plan, pd, t0); err != nil {
		t.Fatalf("apply: %v", err)
	}
	appliedPath := PromotionAppliedRecordPath(home, fixturePromoteReviewID)

	// The undo is valid right now...
	if _, err := PlanPromotionRevert(home, appliedPath); err != nil {
		t.Fatalf("revert should be planable immediately after apply: %v", err)
	}

	// ...but a later authority change invalidates it.
	later := filepath.Join(t.TempDir(), "later.yaml")
	body := `version: 1
purpose: later change
rules: []
capability_classes: [classify-extract]
models:
  synthetic-local.invalid:
    lane: local-executor.invalid
    tier: cheap
    packet_ceiling: medium
    capabilities:
      classify-extract:
        level: assumed
        evidence: later independent change
routing_defaults:
  classify-extract: [synthetic-local.invalid]
`
	if err := os.WriteFile(later, []byte(body), 0o644); err != nil {
		t.Fatalf("write later input: %v", err)
	}
	if _, err := ImportModelCapabilitiesYAML(home, later, t0); err != nil {
		t.Fatalf("later import: %v", err)
	}

	authorityBefore, _ := os.ReadFile(ModelCapabilitiesPath(home))
	if _, err := PlanPromotionRevert(home, appliedPath); err == nil {
		t.Fatal("revert must refuse over a newer authority change")
	}
	authorityAfter, _ := os.ReadFile(ModelCapabilitiesPath(home))
	if string(authorityBefore) != string(authorityAfter) {
		t.Fatal("a refused revert must not touch the authority")
	}
}

// FALSIFIER 5 (persistence coherence) — if the applied record cannot be marked
// reverted AFTER the restore succeeded, the home must still be coherent: the
// authority and the ledger head are both back at the pre-apply revision, route
// planning works, and the stale record is refused by the revision guard rather
// than being replayable.
func TestPromotionRevertStaysCoherentWhenTheRecordCannotBeMarkedReverted(t *testing.T) {
	home, revision := promotionApplyHome(t)
	original, err := os.ReadFile(ModelCapabilitiesPath(home))
	if err != nil {
		t.Fatalf("read original authority: %v", err)
	}
	reviewPath := promotionReviewFile(t, home, revision, fixturePromoteReviewID,
		promotionRecommendationPromote, "synthetic-judge.invalid", "classify-extract", "capable", "primary")
	plan, err := PlanPromotionApply(home, reviewPath, "operator rationale")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	_, pd := allowingDecision(t, home, "work_revert_persist", plan, t0)
	if _, err := ApplyPromotionReview(home, "work_revert_persist", plan, pd, t0); err != nil {
		t.Fatalf("apply: %v", err)
	}
	appliedPath := PromotionAppliedRecordPath(home, fixturePromoteReviewID)

	revertPlan, err := PlanPromotionRevert(home, appliedPath)
	if err != nil {
		t.Fatalf("plan revert: %v", err)
	}
	record, err := LoadPromotionAppliedRecord(appliedPath)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	cfg, _, err := policy.LoadConfig(home)
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	revertAI := PromotionRevertIntent("work_revert_persist", record, t0)
	revertPD := policy.Evaluate(revertAI, cfg, t0)
	if !policy.Allowed(revertPD) {
		t.Fatalf("revert decision should allow, got %s", revertPD.Outcome)
	}

	// Fail exactly the post-revert record write. This injected seam is
	// deterministic even when the test process can bypass filesystem modes.
	realWriteAppliedRecord := writePromotionAppliedRecordFn
	writePromotionAppliedRecordFn = func(string, string, PromotionAppliedRecord) (string, error) {
		return "", errors.New("injected applied-record write failure")
	}
	t.Cleanup(func() { writePromotionAppliedRecordFn = realWriteAppliedRecord })

	_, err = RevertPromotionApply(home, "work_revert_persist", revertPlan, revertPD, t0)
	if err == nil {
		t.Fatal("expected injected applied-record write failure")
	}
	if !strings.Contains(err.Error(), "the undo IS in effect") {
		t.Fatalf("the failure must state that the undo took effect, got: %v", err)
	}

	// The undo really is in effect and the home is coherent.
	restored, err := os.ReadFile(ModelCapabilitiesPath(home))
	if err != nil {
		t.Fatalf("read restored authority: %v", err)
	}
	if string(restored) != string(original) {
		t.Fatal("authority must be restored even though the record write failed")
	}
	if _, err := PlanCapabilityRoute(home, "classify-extract", CapabilityRoutePlanOptions{}, t0); err != nil {
		t.Fatalf("ledger head incoherent after a failed record write: %v", err)
	}
	// The stale record is refused, not replayable.
	if _, err := PlanPromotionRevert(home, appliedPath); err == nil {
		t.Fatal("the stale applied record must be refused by the revision guard")
	}
}

// FALSIFIER 5 (ledger coherence) — when the revert's ledger re-stamp fails, the
// home must NOT be left with the authority at the pre revision while the ledger
// head names the post revision. That split makes latestModelCapabilitiesSource
// refuse and breaks every later route plan. The door compensates back to the
// revision the ledger names, so the two AGREE and route planning still works.
func TestPromotionRevertCompensatesWhenTheLedgerHeadCannotBeUpdated(t *testing.T) {
	home, revision := promotionApplyHome(t)
	reviewPath := promotionReviewFile(t, home, revision, fixturePromoteReviewID,
		promotionRecommendationPromote, "synthetic-judge.invalid", "classify-extract", "capable", "primary")
	plan, err := PlanPromotionApply(home, reviewPath, "operator rationale")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	_, pd := allowingDecision(t, home, "work_ledger_split", plan, t0)
	applied, err := ApplyPromotionReview(home, "work_ledger_split", plan, pd, t0)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	appliedPath := PromotionAppliedRecordPath(home, fixturePromoteReviewID)
	postAuthority, err := os.ReadFile(ModelCapabilitiesPath(home))
	if err != nil {
		t.Fatalf("read post authority: %v", err)
	}

	revertPlan, err := PlanPromotionRevert(home, appliedPath)
	if err != nil {
		t.Fatalf("plan revert: %v", err)
	}
	record, err := LoadPromotionAppliedRecord(appliedPath)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	cfg, _, err := policy.LoadConfig(home)
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	revertAI := PromotionRevertIntent("work_ledger_split", record, t0)
	revertPD := policy.Evaluate(revertAI, cfg, t0)
	if !policy.Allowed(revertPD) {
		t.Fatalf("revert decision should allow, got %s", revertPD.Outcome)
	}

	// Fail exactly the ledger append after the authority restore. The
	// compensation path must put authority back at the still-current ledger head.
	realAppendAuthorityRecord := appendPromotionAuthorityRecordFn
	appendPromotionAuthorityRecordFn = func(string, string, string, string, string, act.Result) error {
		return errors.New("injected source-authority ledger append failure")
	}
	t.Cleanup(func() { appendPromotionAuthorityRecordFn = realAppendAuthorityRecord })

	_, err = RevertPromotionApply(home, "work_ledger_split", revertPlan, revertPD, t0)
	if err == nil {
		t.Fatal("expected injected source-authority ledger append failure")
	}
	if !strings.Contains(err.Error(), "authority and ledger agree") {
		t.Fatalf("the failure must state that agreement was restored, got: %v", err)
	}

	// Compensation put the authority back to the revision the ledger names.
	nowAuthority, err := os.ReadFile(ModelCapabilitiesPath(home))
	if err != nil {
		t.Fatalf("read authority: %v", err)
	}
	if string(nowAuthority) != string(postAuthority) {
		t.Fatal("compensation must restore the authority to the revision the ledger head names")
	}
	// The decisive property: the home is still usable.
	if _, err := PlanCapabilityRoute(home, "classify-extract", CapabilityRoutePlanOptions{}, t0); err != nil {
		t.Fatalf("authority/ledger split left route planning broken: %v", err)
	}
	if applied.Record.AuthorityRevisionAfter == "" {
		t.Fatal("fixture precondition: applied record should carry a post revision")
	}
}

// FALSIFIER 5 (forged-revision attack) — the decisive control. The drift guard
// exists to stop a stale pre-image being waved over a newer change. If it trusted
// the record's own authority_revision_after, an attacker could simply set that
// field to the CURRENT revision and the guard would wave the restore through,
// destroying the newer authority. Revisions are therefore measured from the
// artifact bodies, and this test proves the attack is refused.
func TestPromotionRevertRefusesForgedAfterRevisionOverNewerAuthority(t *testing.T) {
	home, revision := promotionApplyHome(t)
	reviewPath := promotionReviewFile(t, home, revision, fixturePromoteReviewID,
		promotionRecommendationPromote, "synthetic-judge.invalid", "classify-extract", "capable", "primary")
	plan, err := PlanPromotionApply(home, reviewPath, "operator rationale")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	_, pd := allowingDecision(t, home, "work_forged_rev", plan, t0)
	if _, err := ApplyPromotionReview(home, "work_forged_rev", plan, pd, t0); err != nil {
		t.Fatalf("apply: %v", err)
	}
	appliedPath := PromotionAppliedRecordPath(home, fixturePromoteReviewID)

	// A LATER, independent change lands. A revert of the old record must now be
	// refused, because it would discard this newer authority.
	later := filepath.Join(t.TempDir(), "later.yaml")
	body := `version: 1
purpose: newer independent authority
rules: []
capability_classes: [classify-extract]
models:
  synthetic-local.invalid:
    lane: local-executor.invalid
    tier: cheap
    packet_ceiling: medium
    capabilities:
      classify-extract:
        level: assumed
        evidence: newer independent change
routing_defaults:
  classify-extract: [synthetic-local.invalid]
`
	if err := os.WriteFile(later, []byte(body), 0o644); err != nil {
		t.Fatalf("write later input: %v", err)
	}
	if _, err := ImportModelCapabilitiesYAML(home, later, t0); err != nil {
		t.Fatalf("later import: %v", err)
	}
	newerAuthority, err := os.ReadFile(ModelCapabilitiesPath(home))
	if err != nil {
		t.Fatalf("read newer authority: %v", err)
	}
	records, err := LoadModelCapabilitiesSourceAuthority(home)
	if err != nil || len(records) == 0 {
		t.Fatalf("load source authority: %v", err)
	}
	liveRevision := records[len(records)-1].SourceRevision

	// THE ATTACK: claim the post-image was the current revision.
	record, err := LoadPromotionAppliedRecord(appliedPath)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	if record.AuthorityRevisionAfter == liveRevision {
		t.Fatal("fixture precondition: the later import must have moved the revision")
	}
	record.AuthorityRevisionAfter = liveRevision
	tampered, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatalf("marshal tampered record: %v", err)
	}
	if err := os.WriteFile(appliedPath, tampered, 0o644); err != nil {
		t.Fatalf("write tampered record: %v", err)
	}

	if _, err := PlanPromotionRevert(home, appliedPath); err == nil {
		t.Fatal("a forged authority_revision_after must not satisfy the drift guard")
	} else if !strings.Contains(err.Error(), "does not reproduce its own action intent id") {
		t.Fatalf("refusal should show that the bound intent rejected the forged revision, got: %v", err)
	}

	// The newer authority survived untouched.
	afterAttack, err := os.ReadFile(ModelCapabilitiesPath(home))
	if err != nil {
		t.Fatalf("read authority: %v", err)
	}
	if string(afterAttack) != string(newerAuthority) {
		t.Fatal("the newer authority must survive a forged-revision revert attempt")
	}
}

// FALSIFIER 7 — the additive bridge is preserved. promotion-apply is the
// authoring door; import stays additive and cannot delete Compound-only rows,
// claims, classes, rules, or route positions — including a row this door just
// promoted.
func TestImportRemainsAdditiveBridgeAfterGovernedApply(t *testing.T) {
	home, revision := promotionApplyHome(t)
	reviewPath := promotionReviewFile(t, home, revision, fixturePromoteReviewID,
		promotionRecommendationPromote, "synthetic-judge.invalid", "classify-extract", "capable", "primary")
	plan, err := PlanPromotionApply(home, reviewPath, "operator rationale")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	_, pd := allowingDecision(t, home, "work_bridge_fixture", plan, t0)
	if _, err := ApplyPromotionReview(home, "work_bridge_fixture", plan, pd, t0); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// A later PARTIAL bridge input mentioning only one lane and one class.
	partial := filepath.Join(t.TempDir(), "partial.yaml")
	body := `version: 1
purpose: partial bridge input
rules: []
capability_classes: [classify-extract]
models:
  synthetic-local.invalid:
    lane: local-executor.invalid
    tier: cheap
    packet_ceiling: medium
    capabilities:
      classify-extract:
        level: capable
        evidence: refreshed local probe sweep
routing_defaults:
  classify-extract: [synthetic-local.invalid]
`
	if err := os.WriteFile(partial, []byte(body), 0o644); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	if _, err := ImportModelCapabilitiesYAML(home, partial, t0); err != nil {
		t.Fatalf("partial import: %v", err)
	}

	after, _, _, err := loadModelCapabilitiesSnapshot(home)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := after.Models["synthetic-judge.invalid"]; !ok {
		t.Fatal("bridge import deleted a Compound-only row")
	}
	if got := after.Models["synthetic-judge.invalid"].Capabilities["classify-extract"].Level; got != "primary" {
		t.Fatalf("bridge import reverted the governed claim: %q", got)
	}
	if _, ok := after.Models["synthetic-judge.invalid"].Capabilities["judge-critique"]; !ok {
		t.Fatal("bridge import deleted a Compound-only claim")
	}
	if _, ok := after.RoutingDefaults["judge-critique"]; !ok {
		t.Fatal("bridge import deleted a Compound-only route position")
	}
	if len(after.Rules) != 1 {
		t.Fatalf("bridge import deleted Compound-only rules: %+v", after.Rules)
	}
}
