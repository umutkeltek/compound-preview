package mem

import (
	"os"
	"path/filepath"
	"testing"

	"compound/internal/candidates"
)

func TestImportKnowledgePageRecordsEvidenceAndProposedCandidatesOnly(t *testing.T) {
	homePath := t.TempDir()
	store, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	pagePath := filepath.Join(t.TempDir(), "brain", "projects", "claim-guard.md")
	writeKnowledgePage(t, pagePath, `---
schema_version: compound.knowledge_page.v0.1
id: kg-claim-guard-visuals
title: Claim Guard Visual Standards
entity: product:claim-guard-ios
owner: product
visibility: contextual
claims:
  - predicate: app_store_visual_standard
    object: screenshots must show real review workflow states
    claim_type: constraint
    confidence: 0.86
    qualifiers:
      domain: app_store_visuals
  - entity: product:claim-guard-ios
    predicate: product_loop
    object: measure screenshot changes against App Store Connect signals
    claim_type: architecture
---
# Claim Guard Visual Standards

Screenshots should show the actual review workflow, not generic device art.
`)

	report, err := store.ImportKnowledgePage(pagePath, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != KnowledgePageSchemaVersion || report.EvidenceID == "" || len(report.CandidateIDs) != 2 || report.Created != 2 || report.Reused != 0 {
		t.Fatalf("report = %+v", report)
	}
	if _, err := os.Stat(store.claimsLog()); !os.IsNotExist(err) {
		t.Fatalf("knowledge page import must not create promoted claims ledger, stat err=%v", err)
	}
	ev, ok, err := store.GetEvidence(report.EvidenceID)
	if err != nil || !ok {
		t.Fatalf("evidence ok=%v err=%v", ok, err)
	}
	if ev.SourceKind != "note" || ev.SourceID != "knowledge_page:kg-claim-guard-visuals" || ev.Visibility != "contextual" {
		t.Fatalf("evidence = %+v", ev)
	}
	cands, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %+v", cands)
	}
	for _, c := range cands {
		if c.State != "proposed" || c.SourceTraceRef != report.EvidenceID || c.PromotionBasis != candidates.BasisCuratedPage {
			t.Fatalf("candidate not page-grounded/proposed: %+v", c)
		}
		if c.Qualifiers["source"] != "knowledge_page" || c.Qualifiers["page_id"] != "kg-claim-guard-visuals" || c.Qualifiers["owner"] != "product" {
			t.Fatalf("candidate qualifiers = %+v", c.Qualifiers)
		}
	}
}

func TestKnowledgePageDistinctPredicatesDoNotSupersedeWithinSharedDomain(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pagePath := filepath.Join(t.TempDir(), "compound-rulings.md")
	writeKnowledgePage(t, pagePath, `---
schema_version: compound.knowledge_page.v0.1
id: compound-rulings
entity: system:compound
claims:
  - predicate: os_absorption_end_state
    object: compound owns the absorbed control-plane meaning
    claim_type: decision
    qualifiers:
      domain: os-absorption
  - predicate: os_absorption_stance
    object: preserve source evidence while retiring parallel authority
    claim_type: decision
    qualifiers:
      domain: os-absorption
  - predicate: os_absorption_method
    object: absorb one proven vertical slice at a time
    claim_type: decision
    qualifiers:
      domain: os-absorption
---
# Compound rulings
`)

	report, err := store.ImportKnowledgePage(pagePath, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.CandidateIDs) != 3 {
		t.Fatalf("candidate ids = %v, want three explicit page claims", report.CandidateIDs)
	}
	promotedClaims := map[string]string{}
	for _, candidateID := range report.CandidateIDs {
		promoted, err := store.Adjudicate(candidateID, "promote", "work_cluster_collapse", t0)
		if err != nil {
			t.Fatalf("promote %s: %v", candidateID, err)
		}
		if promoted.Superseded != "" {
			t.Fatalf("distinct predicate %q superseded claim %s", promoted.Claim.Predicate, promoted.Superseded)
		}
		promotedClaims[promoted.Claim.Predicate] = promoted.Claim.ID
	}
	edges, err := store.loadEdges()
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range edges {
		if edge.Kind == EdgeKindSupersedes {
			t.Fatalf("distinct page predicates created supersession edge %+v", edge)
		}
	}
	sweep, err := store.SynthesisSweep([]string{"contradict"}, t0.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if sweep.ContradictionProposalCount != 0 || len(sweep.ReopenedClaims) != 0 {
		t.Fatalf("distinct page predicates reopened as contradictions: %+v", sweep)
	}

	current, err := store.Recall("system:compound", RecallOpts{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, claim := range current.Claims {
		got[claim.Predicate] = true
	}
	for _, predicate := range []string{"os_absorption_end_state", "os_absorption_stance", "os_absorption_method"} {
		if !got[predicate] {
			t.Fatalf("current predicates = %v, missing %q", got, predicate)
		}
	}
	positions, err := store.Positions()
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 3 {
		t.Fatalf("positions = %+v, want one independent position per predicate", positions)
	}
	for _, position := range positions {
		claimID := promotedClaims[position.Predicate]
		if claimID == "" || position.ActiveClaimID != claimID {
			t.Fatalf("position predicate/active claim mismatch: %+v promoted=%v", position, promotedClaims)
		}
	}
}

func TestImportKnowledgePageIsIdempotentAcrossPathMove(t *testing.T) {
	homePath := t.TempDir()
	store, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	body := `---
schema_version: compound.knowledge_page.v0.1
id: kg-stable-page
title: Stable Page
entity: system:compound
claims:
  - predicate: source_adapter_policy
    object: curated markdown imports create candidates before claims
    claim_type: architecture
---
# Stable Page

Moving this page must not create another memory candidate.
`
	firstPath := filepath.Join(t.TempDir(), "brain", "stable.md")
	secondPath := filepath.Join(t.TempDir(), "moved", "stable.md")
	writeKnowledgePage(t, firstPath, body)
	writeKnowledgePage(t, secondPath, body)

	first, err := store.ImportKnowledgePage(firstPath, t0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.ImportKnowledgePage(secondPath, t0.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if first.EvidenceID != second.EvidenceID {
		t.Fatalf("evidence changed across path move: %s != %s", first.EvidenceID, second.EvidenceID)
	}
	if len(first.CandidateIDs) != 1 || len(second.CandidateIDs) != 1 || first.CandidateIDs[0] != second.CandidateIDs[0] {
		t.Fatalf("candidate ids changed across path move: %+v then %+v", first.CandidateIDs, second.CandidateIDs)
	}
	if first.Created != 1 || second.Reused != 1 || second.Created != 0 {
		t.Fatalf("unexpected create/reuse counts: first=%+v second=%+v", first, second)
	}
	cands, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("duplicate candidates created: %+v", cands)
	}
}

func TestImportKnowledgePageRequiresExplicitClaimShape(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pagePath := filepath.Join(t.TempDir(), "bad.md")
	writeKnowledgePage(t, pagePath, `---
schema_version: compound.knowledge_page.v0.1
id: kg-bad
claims:
  - predicate: incomplete
    object: missing entity
---
# Bad
`)

	if _, err := store.ImportKnowledgePage(pagePath, t0); err == nil {
		t.Fatal("expected missing entity/predicate/object error")
	}
}

func writeKnowledgePage(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
