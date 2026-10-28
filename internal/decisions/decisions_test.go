package decisions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
	"compound/internal/reviewartifact"
	"compound/internal/writeback"
)

func TestImportPolicyDecisionDirRecordsEvidenceLifecycle(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "decisions", "DEC-0001.md")
	writeFile(t, path, `---
id: DEC-0001
title: Use spine memory for decisions
status: accepted
domains: [architecture]
supersedes: [DEC-0000]
falsifier: Decision imports cannot preserve review triggers.
review_trigger: Re-check when decisions become a first-class ledger.
owner: compound
scope: public-core
---

# Use spine memory for decisions
`)

	got, err := ImportPolicyDecisionDir(store, dir, time.Date(2026, 6, 29, 20, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ImportPolicyDecisionDir got %d report(s), want 1", len(got))
	}

	report := got[0]
	if report.EvidenceID == "" || report.CandidateID == "" || report.ClaimID == "" || report.Verdict != candidates.Promote {
		t.Fatalf("report = %+v", report)
	}
	ev, ok, err := store.GetEvidence(report.EvidenceID)
	if err != nil || !ok {
		t.Fatalf("evidence lookup ok=%v err=%v", ok, err)
	}
	if ev.SourceKind != "repo" || ev.SourceID == "" {
		t.Fatalf("evidence = %+v", ev)
	}
	l := report.Lifecycle
	if l.ID != "DEC-0001" || l.Kind != KindPolicy || l.Status != StatusAccepted {
		t.Fatalf("lifecycle = %+v", l)
	}
	if l.Scope != "public-core" || l.Owner != "compound" {
		t.Fatalf("scope/owner not preserved: %+v", l)
	}
	if l.Falsifier == "" || l.ReviewTrigger == "" {
		t.Fatalf("missing lifecycle proof fields: %+v", l)
	}
	if len(l.Supersedes) != 1 || l.Supersedes[0] != "DEC-0000" {
		t.Fatalf("supersedes = %v", l.Supersedes)
	}
	if missing := MissingRequired(l); len(missing) != 0 {
		t.Fatalf("MissingRequired = %v", missing)
	}
}

func TestEligibleNoteDecisionFilesExcludesIndexesAndTemplates(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "2026-06-30-real-decision.md")
	writeFile(t, want, "# Real decision")
	writeFile(t, filepath.Join(dir, "README.md"), "# Index")
	writeFile(t, filepath.Join(dir, "decision-template.md"), "# Template")
	got, err := EligibleNoteDecisionFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("EligibleNoteDecisionFiles = %v, want %s", got, want)
	}
}

func TestSourceAuthorityCoverageCollapsesBySourceAndStatusReflectsVerdict(t *testing.T) {
	home := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), "decision.md")
	parked, err := RecordSourceAuthority(home, DecisionImportReport{
		SourcePath:  sourcePath,
		SourceKind:  "note",
		EvidenceID:  "ev_1",
		CandidateID: "cand_1",
		Verdict:     candidates.Park,
		Lifecycle:   Lifecycle{ID: "brain-one"},
	}, "decisions import --source-dir", time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if parked.AuthorityStatus != "compound_parked" {
		t.Fatalf("parked source authority = %+v", parked)
	}
	promoted, err := RecordSourceAuthority(home, DecisionImportReport{
		SourcePath:  sourcePath,
		SourceKind:  "note",
		EvidenceID:  "ev_2",
		CandidateID: "cand_2",
		ClaimID:     "claim_2",
		Verdict:     candidates.Promote,
		Lifecycle:   Lifecycle{ID: "brain-one"},
	}, "decisions import --source-dir", time.Date(2026, 6, 30, 12, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if promoted.ID != parked.ID || promoted.AuthorityStatus != "compound_primary" {
		t.Fatalf("promoted source authority = %+v parked=%+v", promoted, parked)
	}
	coverage, err := SourceAuthorityCoverage(home, []string{sourcePath, sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Status != "ok" || len(coverage.Records) != 1 || len(coverage.Eligible) != 1 || len(coverage.Missing) != 0 {
		t.Fatalf("coverage = %+v", coverage)
	}
	if coverage.Records[0].ClaimID != "claim_2" || coverage.CountsByStatus["compound_primary"] != 1 {
		t.Fatalf("coverage records/status = %+v", coverage)
	}
}

func TestImportBrainDecisionDirPreservesBrainSourceKind(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "decision.md"), `---
title: Brain decision
status: accepted
falsifier: Brain source no longer applies.
review_trigger: Re-check source.
---
# Brain decision
`)
	reports, err := ImportBrainDecisionDir(store, dir, time.Date(2026, 6, 30, 12, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 {
		t.Fatalf("reports = %+v", reports)
	}
	if reports[0].SourceKind != "brain" || !strings.HasPrefix(reports[0].Lifecycle.ID, "brain-") {
		t.Fatalf("brain dir report = %+v", reports[0])
	}
}

func TestImportNoteDecisionReusesLegacyGeneratedBrainDecision(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "decision.md")
	writeFile(t, path, `---
title: Generated id decision
status: accepted
falsifier: Decision no longer protects behavior.
review_trigger: Re-check behavior.
---
# Generated id decision
`)
	brainReport, err := ImportBrainDecision(store, path, time.Date(2026, 6, 30, 12, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	noteReport, err := ImportNoteDecision(store, path, time.Date(2026, 6, 30, 12, 31, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if brainReport.ClaimID == "" || noteReport.ClaimID != brainReport.ClaimID {
		t.Fatalf("note import should reuse legacy brain claim: brain=%+v note=%+v", brainReport, noteReport)
	}
	if !strings.HasPrefix(noteReport.Lifecycle.ID, "brain-") {
		t.Fatalf("note import should keep existing brain decision id: %+v", noteReport.Lifecycle)
	}
	claims, err := store.AllCurrent()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %+v", claims)
	}
}

func TestImportNoteDecisionReusesLegacyParkedBrainCandidate(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "decision.md")
	writeFile(t, path, `---
title: Incomplete generated id decision
status: accepted
---
# Incomplete generated id decision
`)
	brainReport, err := ImportBrainDecision(store, path, time.Date(2026, 6, 30, 12, 40, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if brainReport.Verdict != candidates.Park {
		t.Fatalf("brain report should park: %+v", brainReport)
	}
	noteReport, err := ImportNoteDecision(store, path, time.Date(2026, 6, 30, 12, 41, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if noteReport.CandidateID != brainReport.CandidateID || noteReport.Lifecycle.ID != brainReport.Lifecycle.ID {
		t.Fatalf("note import should reuse legacy parked candidate: brain=%+v note=%+v", brainReport, noteReport)
	}
	cands, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %+v", cands)
	}
}

func TestImportReplayKeepsSourceAuthorityPrimary(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "decisions", "DEC-0001.md")
	writeFile(t, path, `---
id: DEC-0001
title: Use Compound decisions
status: accepted
falsifier: Decision stops protecting the intended behavior.
review_trigger: Re-check when the behavior changes.
---
# Use Compound decisions
`)
	first, err := ImportPolicyDecision(store, path, time.Date(2026, 6, 30, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if first.ClaimID == "" {
		t.Fatalf("first import = %+v", first)
	}
	beforeCandidates, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	second, err := ImportPolicyDecision(store, path, time.Date(2026, 6, 30, 13, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if second.ClaimID != first.ClaimID {
		t.Fatalf("replay claim id = %q, want %q; report=%+v", second.ClaimID, first.ClaimID, second)
	}
	afterCandidates, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(afterCandidates) != len(beforeCandidates) {
		t.Fatalf("replay created duplicate candidates: before=%d after=%d", len(beforeCandidates), len(afterCandidates))
	}
	rec, err := RecordSourceAuthority(home, second, "decisions import --policy", time.Date(2026, 6, 30, 13, 2, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if rec.AuthorityStatus != "compound_primary" {
		t.Fatalf("source authority after replay = %+v", rec)
	}
}

func TestImportPolicyDecisionSourceEditCreatesNewClaim(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "decisions", "DEC-0001.md")
	writeFile(t, path, `---
id: DEC-0001
title: Use Compound decisions
status: accepted
falsifier: Original falsifier.
review_trigger: Re-check when the behavior changes.
---
# Use Compound decisions
`)
	first, err := ImportPolicyDecision(store, path, time.Date(2026, 6, 30, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, `---
id: DEC-0001
title: Use Compound decisions
status: accepted
falsifier: Updated falsifier.
review_trigger: Re-check when the behavior changes.
---
# Use Compound decisions
`)
	second, err := ImportPolicyDecision(store, path, time.Date(2026, 6, 30, 13, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if second.ClaimID == "" || second.ClaimID == first.ClaimID {
		t.Fatalf("source edit should create a new claim: first=%+v second=%+v", first, second)
	}
	if second.Lifecycle.Falsifier != "Updated falsifier." {
		t.Fatalf("updated lifecycle not reflected: %+v", second.Lifecycle)
	}
}

func TestParkedDecisionImportReplayReusesCandidate(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "decisions", "DEC-0007.md")
	writeFile(t, path, `---
id: DEC-0007
title: Older accepted source shape
status: accepted
---
# Older accepted source shape
`)
	first, err := ImportPolicyDecision(store, path, time.Date(2026, 6, 30, 14, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if first.Verdict != candidates.Park || first.ClaimID != "" {
		t.Fatalf("first import = %+v", first)
	}
	beforeCandidates, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	second, err := ImportPolicyDecision(store, path, time.Date(2026, 6, 30, 14, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if second.CandidateID != first.CandidateID || second.Verdict != candidates.Park {
		t.Fatalf("second import = %+v want candidate %s", second, first.CandidateID)
	}
	afterCandidates, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(afterCandidates) != len(beforeCandidates) {
		t.Fatalf("parked replay created duplicate candidates: before=%d after=%d", len(beforeCandidates), len(afterCandidates))
	}
}

func TestImportPolicyDecisionDirKeepsDecisionEntitiesSeparate(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "decisions", "DEC-0100.md"), `---
id: DEC-0100
title: First accepted decision
status: accepted
falsifier: First decision can be lost after importing another decision.
review_trigger: Re-check after importing more than one decision.
---

# First accepted decision
`)
	writeFile(t, filepath.Join(dir, "decisions", "DEC-0101.md"), `---
id: DEC-0101
title: Second accepted decision
status: accepted
falsifier: Second decision supersedes unrelated accepted decisions.
review_trigger: Re-check after importing more than one decision.
---

# Second accepted decision
`)

	reports, err := ImportPolicyDecisionDir(store, dir, time.Date(2026, 6, 29, 21, 10, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %+v", reports)
	}
	claims, err := store.AllCurrent()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 {
		t.Fatalf("claims = %+v", claims)
	}
	got := map[string]bool{}
	for _, claim := range claims {
		got[claim.Entity] = true
	}
	if !got["decision:DEC-0100"] || !got["decision:DEC-0101"] {
		t.Fatalf("decision entities = %v", got)
	}
}

func TestImportBrainDecisionRecordsEvidenceAndDecision(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "brain", "operating-system", "decision.md")
	writeFile(t, path, `---
id: brain-dec-0001
title: Receipts are proof
kind: doctrine
status: accepted
domains: [governance]
falsifier: Agents can claim success without a state delta.
review_trigger: Revisit when receipts stop changing future runs.
owner: compound
scope: public-core
---

# Receipts are proof

Every non-trivial run leaves evidence before it can be treated as done.
`)
	report, err := ImportBrainDecision(store, path, time.Date(2026, 6, 29, 18, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.EvidenceID == "" || report.CandidateID == "" || report.ClaimID == "" || report.Verdict != candidates.Promote {
		t.Fatalf("report = %+v", report)
	}
	ev, ok, err := store.GetEvidence(report.EvidenceID)
	if err != nil || !ok {
		t.Fatalf("evidence lookup ok=%v err=%v", ok, err)
	}
	if ev.SourceKind != "note" || ev.SourceID == "" || ev.QuoteText == "" {
		t.Fatalf("evidence = %+v", ev)
	}
	claims, err := store.AllCurrent()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %+v", claims)
	}
	l := LifecycleFromClaim(claims[0])
	if l.ID != "brain-dec-0001" || l.Kind != KindDoctrine || l.Status != StatusAccepted {
		t.Fatalf("lifecycle = %+v", l)
	}
	if l.Falsifier == "" || l.ReviewTrigger == "" || len(l.EvidenceRefs) < 2 || l.EvidenceRefs[1] != report.EvidenceID {
		t.Fatalf("lifecycle evidence/proof fields = %+v", l)
	}
	rec, err := RecordSourceAuthority(home, DecisionImportReport(report), "decisions import --brain", time.Date(2026, 6, 29, 18, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if rec.SourceRole != "source_in_place_evidence" || rec.AuthorityRef != "decision://brain-dec-0001" || rec.AuthorityStatus != "compound_primary" {
		t.Fatalf("source authority = %+v", rec)
	}
	loaded, err := LoadSourceAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].ID != rec.ID || loaded[0].ClaimID != report.ClaimID {
		t.Fatalf("loaded source authority = %+v", loaded)
	}
}

func TestImportBrainDecisionAdaptsNarrativeBrainPageConservatively(t *testing.T) {
	body := `---
type: reflection
title: 2026 06 11 Memory Engine Ownership Decision 537a02
date: '2026-06-11T00:00:00.000Z'
dream_generated: true
tags:
  - brain-ownership
---

# memory engine ownership decision - own contract, rent engine

## decision Oracle review

Oracle consulted, operator accepted as-is. The verdict:

> **"Keep the rented memory engine continuously updated. Don't rebuild. Don't freeze. Own the contract seam."**

## How apply

When ReleaseX ships or is externally blocked, execute the parked spec. Promote this decision to canon when out of the coordination-sensitive window.
`
	pathA := filepath.Join(t.TempDir(), "brain", "reflections", "decision.md")
	pathB := filepath.Join(t.TempDir(), "moved", "brain", "renamed.md")
	writeFile(t, pathA, body)
	writeFile(t, pathB, body)

	storeA, err := mem.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reportA, err := ImportBrainDecision(storeA, pathA, time.Date(2026, 6, 29, 22, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := mem.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reportB, err := ImportBrainDecision(storeB, pathB, time.Date(2026, 6, 29, 22, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	if reportA.Lifecycle.ID != reportB.Lifecycle.ID {
		t.Fatalf("stable brain decision id changed across paths: %q != %q", reportA.Lifecycle.ID, reportB.Lifecycle.ID)
	}
	if reportA.Verdict != candidates.Park || reportA.ClaimID != "" {
		t.Fatalf("narrative decision should park until lifecycle proof exists: %+v", reportA)
	}
	if !strings.Contains(reportA.Lifecycle.Choice, "Keep the rented memory engine continuously updated") {
		t.Fatalf("choice did not come from narrative decision section: %+v", reportA.Lifecycle)
	}
	if !strings.Contains(reportA.Lifecycle.ReviewTrigger, "When ReleaseX ships") {
		t.Fatalf("review trigger was not extracted from narrative section: %+v", reportA.Lifecycle)
	}
	missing := MissingRequired(reportA.Lifecycle)
	if len(missing) != 1 || missing[0] != "falsifier" {
		t.Fatalf("missing lifecycle proof = %v", missing)
	}
}

func TestImportPolicyDecisionRecordsEvidenceAndAcceptedLifecycle(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "decisions", "DEC-0042.md")
	writeFile(t, path, `---
id: DEC-0042
title: Gate source mutations
status: accepted
domains: [architecture]
supersedes: [DEC-0041]
falsifier: Source adapters can mutate owners without a policy decision.
review_trigger: Re-check after first source adapter writes through a gate.
owner: compound
scope: public-core
---

# Gate source mutations

Source-specific adapters must enter through evidence and policy gates.
`)

	report, err := ImportPolicyDecision(store, path, time.Date(2026, 6, 29, 19, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.SourceKind != "policy" || report.EvidenceID == "" || report.CandidateID == "" || report.ClaimID == "" || report.Verdict != candidates.Promote {
		t.Fatalf("report = %+v", report)
	}
	ev, ok, err := store.GetEvidence(report.EvidenceID)
	if err != nil || !ok {
		t.Fatalf("evidence lookup ok=%v err=%v", ok, err)
	}
	if ev.SourceKind != "repo" || ev.SourceID == "" || ev.QuoteText == "" || ev.Visibility != "operational" {
		t.Fatalf("evidence = %+v", ev)
	}
	l := report.Lifecycle
	if l.ID != "DEC-0042" || l.Kind != KindPolicy || l.Status != StatusAccepted {
		t.Fatalf("lifecycle = %+v", l)
	}
	if l.Owner != "compound" || l.Scope != "public-core" || len(l.Supersedes) != 1 || l.Supersedes[0] != "DEC-0041" {
		t.Fatalf("lifecycle ownership/supersession = %+v", l)
	}
	if missing := MissingRequired(l); len(missing) != 0 {
		t.Fatalf("MissingRequired = %v", missing)
	}
}

func TestImportPolicyDecisionParksAcceptedDecisionMissingLifecycleProof(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "decisions", "DEC-0007.md")
	writeFile(t, path, `---
id: DEC-0007
title: Older accepted source shape
status: accepted
domains: [agents]
supersedes: []
---

# DEC-0007: Older accepted source shape

The source says accepted, but lacks falsifier and review trigger.
`)

	report, err := ImportPolicyDecision(store, path, time.Date(2026, 6, 29, 21, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != candidates.Park || report.ClaimID != "" || report.EvidenceID == "" {
		t.Fatalf("report = %+v", report)
	}
	claims, err := store.AllCurrent()
	if err != nil {
		t.Fatal(err)
	}
	cands, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	doctor := DoctorReportFromClaimsAndCandidates(claims, cands)
	if doctor.Current != 0 || doctor.Parked != 1 || doctor.MissingLifecycleFields != 1 || !NeedsWriteback(doctor) {
		t.Fatalf("doctor = %+v", doctor)
	}
	if len(doctor.LifecycleGaps) != 1 || doctor.LifecycleGaps[0].CandidateID != report.CandidateID {
		t.Fatalf("gaps = %+v", doctor.LifecycleGaps)
	}
	if len(doctor.LifecycleGaps[0].EvidenceRefs) < 2 || doctor.LifecycleGaps[0].EvidenceRefs[1] != report.EvidenceID {
		t.Fatalf("gap evidence refs = %+v", doctor.LifecycleGaps[0].EvidenceRefs)
	}
}

func TestImportPolicyDecisionParksSupersededLifecycleWithReplacement(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "decisions", "DEC-0041.md")
	writeFile(t, path, `---
id: DEC-0041
title: Import decisions as loose claims
status: superseded
replaced_by: DEC-0042
falsifier: Superseded decisions still influence current agent behavior.
review_trigger: Re-check if current recall surfaces this as active guidance.
---

# Import decisions as loose claims

Old decision kept for provenance only.
`)

	report, err := ImportPolicyDecision(store, path, time.Date(2026, 6, 29, 19, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != candidates.Park || report.ClaimID != "" {
		t.Fatalf("report = %+v", report)
	}
	if report.Lifecycle.Status != StatusSuperseded || report.Lifecycle.ReplacedBy != "DEC-0042" {
		t.Fatalf("lifecycle transition = %+v", report.Lifecycle)
	}
	claims, err := store.AllCurrent()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("superseded decision should not become current claim: %+v", claims)
	}
}

func TestImportOperatorLedgerLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operator.md")
	writeFile(t, path, `### O2 — Flip PlacementGuard

Falsifier: Blocking breaks legitimate generated-state writes.
Review trigger: Re-check after three warn-only violations.
`)

	got, err := ImportOperatorLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ImportOperatorLedger got %d candidate(s), want 1", len(got))
	}

	c := got[0]
	l := LifecycleFromCandidate(c)
	if l.ID != "O2" || l.Kind != KindDoctrine || l.Status != StatusProposed {
		t.Fatalf("lifecycle = %+v", l)
	}
	if c.PromotionBasis != "" {
		t.Fatalf("open operator decision should not auto-promote, got %q", c.PromotionBasis)
	}
	if missing := MissingRequired(l); len(missing) != 0 {
		t.Fatalf("MissingRequired = %v", missing)
	}
}

func TestMissingRequiredLifecycleFields(t *testing.T) {
	missing := MissingRequired(Lifecycle{Status: StatusAccepted, ReviewTrigger: "on drift"})
	if len(missing) != 1 || missing[0] != "falsifier" {
		t.Fatalf("MissingRequired = %v", missing)
	}
}

func TestDoctorReportFromClaims(t *testing.T) {
	claims := []mem.Claim{
		{
			ID:           "claim_decision_1",
			ClaimType:    "decision",
			Object:       "Use governed writeback",
			Qualifiers:   lifecycleQualifiers(KindPolicy, "decisions-repo", "DEC-0001", StatusAccepted),
			EvidenceRefs: []string{"evidence:dec-0001"},
		},
		{
			ID:        "claim_memory_1",
			ClaimType: "memory",
			Object:    "ignored",
		},
	}

	report := DoctorReportFromClaims(claims)
	if report.Total != 1 {
		t.Fatalf("Total = %d, want 1", report.Total)
	}
	if report.Counts[KindPolicy] != 1 || report.Statuses[StatusAccepted] != 1 {
		t.Fatalf("counts/statuses = %+v %+v", report.Counts, report.Statuses)
	}
	if report.MissingLifecycleFields != 1 || len(report.LifecycleGaps) != 1 {
		t.Fatalf("gaps = %+v", report)
	}
	gap := report.LifecycleGaps[0]
	if gap.ClaimID != "claim_decision_1" || gap.DecisionID != "DEC-0001" {
		t.Fatalf("gap identity = %+v", gap)
	}
	if len(gap.Missing) != 2 {
		t.Fatalf("gap missing = %v", gap.Missing)
	}
	if !NeedsWriteback(report) {
		t.Fatal("NeedsWriteback = false, want true")
	}
}

func TestDecisionWritebackGate(t *testing.T) {
	report := DoctorReport{
		Total:                  1,
		Counts:                 map[string]int{KindPolicy: 1},
		Statuses:               map[string]int{StatusAccepted: 1},
		MissingLifecycleFields: 1,
		LifecycleGaps: []LifecycleGap{
			{
				ClaimID:    "claim_decision_1",
				DecisionID: "DEC-0001",
				Kind:       KindPolicy,
				Status:     StatusAccepted,
				Missing:    []string{"falsifier"},
			},
		},
	}
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

	gate := WritebackGate(report, "act_1", "pol_1", now)
	if gate.SchemaVersion != writeback.GateSchemaVersion {
		t.Fatalf("schema = %q", gate.SchemaVersion)
	}
	wantRef := "decisions:lifecycle_gaps:1:" + lifecycleGapDigest(report)
	if gate.CandidateRef != wantRef {
		t.Fatalf("candidate_ref = %q", gate.CandidateRef)
	}
	if gate.CandidateKind != string(candidates.Decision) {
		t.Fatalf("candidate_kind = %q", gate.CandidateKind)
	}
	if gate.OwnerTarget.Kind != "decision_lifecycle" || gate.OwnerTarget.Ref == "" {
		t.Fatalf("owner target = %+v", gate.OwnerTarget)
	}
	if len(gate.EvidenceRefs) < 3 {
		t.Fatalf("evidence refs = %v", gate.EvidenceRefs)
	}
}

func TestBuildLifecycleRepairPlanFindsAcceptedMissingFields(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "decisions", "DEC-0200.md"), `---
id: DEC-0200
title: Accepted without lifecycle proof
status: accepted
domains: [agents]
---

# DEC-0200: Accepted without lifecycle proof

## Requirements

- Agents must not treat accepted status alone as proof.
`)
	writeFile(t, filepath.Join(dir, "decisions", "DEC-0201.md"), `---
id: DEC-0201
title: Complete accepted decision
status: accepted
falsifier: Complete decision still lacks a review trigger.
review_trigger: Re-check after importer changes.
---

# DEC-0201: Complete accepted decision
`)
	writeFile(t, filepath.Join(dir, "decisions", "DEC-0202.md"), `---
id: DEC-0202
title: Proposed missing lifecycle proof
status: proposed
---

# DEC-0202: Proposed missing lifecycle proof
`)
	writeFile(t, filepath.Join(dir, "decisions", "DEC-0203.md"), `---
id: DEC-0203
title: Body carries lifecycle proof
status: accepted
---

# DEC-0203: Body carries lifecycle proof

Falsifier: The importer stops recognizing body lifecycle fields.
Review trigger: Re-check after decision parsing changes.
`)

	plan, err := BuildLifecycleRepairPlan(dir, time.Date(2026, 6, 29, 22, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != RepairPlanSchemaVersion || plan.Status != "review_required" || plan.RepairCount != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.MissingFieldCounts["falsifier"] != 1 || plan.MissingFieldCounts["review_trigger"] != 1 {
		t.Fatalf("missing counts = %+v", plan.MissingFieldCounts)
	}
	repair := plan.Repairs[0]
	if repair.DecisionID != "DEC-0200" || len(repair.Fields) != 2 || len(repair.BodySignals) != 1 || repair.BodySignals[0] != "Requirements" {
		t.Fatalf("repair = %+v", repair)
	}
	if repair.EvidenceRef == "" || !strings.Contains(repair.EvidenceRef, "DEC-0200") {
		t.Fatalf("evidence ref = %q", repair.EvidenceRef)
	}
}

func TestBuildBrainLifecycleRepairPlanFindsNarrativeDecisionDebt(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "wiki", "reflections", "decision.md")
	pathB := filepath.Join(dir, "wiki", "ideas", "complete.md")
	writeFile(t, pathA, `---
type: reflection
title: 2026 06 11 Memory Engine Ownership Decision 537a02
date: '2026-06-11T00:00:00.000Z'
dream_generated: true
---

# memory engine ownership decision - own contract, rent engine

## decision Oracle review

The verdict:

> **"Keep the rented memory engine continuously updated. Don't rebuild. Own the contract seam."**

## How apply

When ReleaseX ships or is externally blocked, execute the parked spec.
`)
	writeFile(t, pathB, `---
type: original
title: Complete narrative decision
date: '2026-06-12T00:00:00.000Z'
falsifier: Complete record still creates duplicate owners.
review_trigger: Re-check after owner changes.
---

# complete narrative decision
`)

	plan, err := BuildBrainLifecycleRepairPlan([]string{pathA, pathB}, time.Date(2026, 6, 29, 23, 10, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourceKind != "brain" || plan.Status != "review_required" || plan.RepairCount != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.MissingFieldCounts["falsifier"] != 1 || plan.MissingFieldCounts["review_trigger"] != 0 {
		t.Fatalf("missing counts = %+v", plan.MissingFieldCounts)
	}
	repair := plan.Repairs[0]
	if !strings.HasPrefix(repair.DecisionID, "brain-") {
		t.Fatalf("decision id = %q", repair.DecisionID)
	}
	if !strings.Contains(repair.Title, "Keep the rented memory engine continuously updated") {
		t.Fatalf("title = %q", repair.Title)
	}
	if repair.EvidenceRef == "" || !strings.HasPrefix(repair.EvidenceRef, "brain_source:") {
		t.Fatalf("evidence ref = %q", repair.EvidenceRef)
	}
	if !pathWithin(plan.SourceRoot, pathA) || !pathWithin(plan.SourceRoot, pathB) {
		t.Fatalf("source root %q does not contain brain paths", plan.SourceRoot)
	}
}

func TestRecordLifecycleRepairPlanWritesReviewArtifacts(t *testing.T) {
	plan := LifecycleRepairPlan{
		SchemaVersion:      RepairPlanSchemaVersion,
		ID:                 "dlrp_test",
		CreatedAt:          "2026-06-29T22:00:00Z",
		SourceRoot:         "/tmp/source",
		Status:             "review_required",
		RepairCount:        1,
		MissingFieldCounts: map[string]int{"falsifier": 1},
		Repairs: []LifecycleRepair{{
			DecisionID: "DEC-0200",
			Title:      "Accepted without lifecycle proof",
			Status:     StatusAccepted,
			SourcePath: "/tmp/source/DEC-0200.md",
			Missing:    []string{"falsifier"},
			Fields: []LifecycleRepairField{{
				Name:           "falsifier",
				SuggestedValue: "Evidence proves the decision wrong.",
				Basis:          "test",
				ReviewNote:     "review",
			}},
			EvidenceRef: "decision_source:/tmp/source/DEC-0200.md",
		}},
		EvidenceRefs: []string{"decision_source:/tmp/source/DEC-0200.md"},
	}

	paths, err := RecordLifecycleRepairPlan(t.TempDir(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.PlanPath); err != nil {
		t.Fatalf("plan artifact missing: %v", err)
	}
	review, err := os.ReadFile(paths.ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(review), "Suggested frontmatter additions") || !strings.Contains(string(review), "DEC-0200") {
		t.Fatalf("review artifact = %s", review)
	}
}

func TestDoctorSourceFormatDetectsTemplateLifecycleDebt(t *testing.T) {
	dir := t.TempDir()
	template := filepath.Join(dir, "decision.md")
	formatDoc := filepath.Join(dir, "README.md")
	writeFile(t, template, `---
id: DEC-0000
title: Short Decision Title
status: proposed
---

# DEC-0000: Short Decision Title

## Decision State

decision in one two direct paragraphs.
`)
	writeFile(t, formatDoc, `# Decisions

Required front matter:

- id
- title
- status
`)

	report, err := DoctorSourceFormat(template, formatDoc)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != SourceFormatDoctorNeedsRepair {
		t.Fatalf("status = %q", report.Status)
	}
	if !hasString(report.MissingTemplateFrontmatter, "falsifier") ||
		!hasString(report.MissingTemplateFrontmatter, "review_trigger") {
		t.Fatalf("missing frontmatter = %v", report.MissingTemplateFrontmatter)
	}
	if !hasString(report.MissingTemplateGuidance, "falsifier") ||
		!hasString(report.MissingTemplateGuidance, "review_trigger") {
		t.Fatalf("missing guidance = %v", report.MissingTemplateGuidance)
	}
	if !hasString(report.MissingFormatDocumentation, "falsifier") ||
		!hasString(report.MissingFormatDocumentation, "review_trigger") {
		t.Fatalf("missing format documentation = %v", report.MissingFormatDocumentation)
	}
}

func TestDoctorSourceFormatAcceptsLifecycleTemplateAndFormatDoc(t *testing.T) {
	dir := t.TempDir()
	template := filepath.Join(dir, "decision.md")
	formatDoc := filepath.Join(dir, "README.md")
	writeFile(t, template, `---
id: DEC-0000
title: Short Decision Title
status: proposed
falsifier:
review_trigger:
---

# DEC-0000: Short Decision Title

## Lifecycle Proof

- Falsifier: describe what evidence would prove the decision wrong.
- Review trigger: describe when humans or agents should re-check the decision.
`)
	writeFile(t, formatDoc, `# Decisions

Required front matter:

- falsifier: evidence that would prove the decision wrong.
- review_trigger: condition that should force review.
`)

	report, err := DoctorSourceFormat(template, formatDoc)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != SourceFormatDoctorOK {
		t.Fatalf("report = %+v", report)
	}
}

func TestAnnotateLifecycleRepairQueueAddsApplyActions(t *testing.T) {
	report := reviewartifact.QueueReport{
		Counts: reviewartifact.QueueCounts{},
		Items: []reviewartifact.QueueItem{
			{
				Area:        lifecycleRepairArea,
				ID:          "dlrp_review",
				Status:      "review_required",
				JSONPath:    "/tmp/plan with space.json",
				RepairCount: 2,
			},
			{
				Area:        lifecycleRepairArea,
				ID:          "dlrp_ok",
				Status:      "ok",
				JSONPath:    "/tmp/ok.json",
				RepairCount: 0,
			},
			{
				Area:        lifecycleRepairArea,
				ID:          "dlrp_ready",
				Status:      "reviewed",
				JSONPath:    "/tmp/ready.json",
				RepairCount: 1,
			},
			{
				Area:     "route-policy",
				ID:       "rpp_other",
				Status:   "review_required",
				JSONPath: "/tmp/route.json",
			},
		},
	}
	AnnotateLifecycleRepairQueue(&report)

	review := report.Items[0]
	if review.Readiness != "needs_review" || len(review.Actions) != 2 {
		t.Fatalf("review-required item not actionable: %+v", review)
	}
	if !review.Actions[1].RequiresReview || !strings.Contains(review.Actions[1].Command, "'/tmp/plan with space.json'") {
		t.Fatalf("write action should stay review-gated and shell-safe: %+v", review.Actions[1])
	}
	if report.Items[1].Readiness != "no_action" || len(report.Items[1].Actions) != 0 {
		t.Fatalf("ok zero-repair item should have no action: %+v", report.Items[1])
	}
	ready := report.Items[2]
	if report.Counts.ReadyToApply != 1 || ready.Readiness != "ready_to_apply" || ready.Actions[1].RequiresReview {
		t.Fatalf("reviewed item should be ready to apply: counts=%+v item=%+v", report.Counts, ready)
	}
	if report.Items[3].Readiness != "" || len(report.Items[3].Actions) != 0 {
		t.Fatalf("non-decision area should stay generic: %+v", report.Items[3])
	}
}

func TestApplyLifecycleRepairPlanWritesReviewedFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "decisions", "DEC-0300.md")
	writeFile(t, path, `---
id: DEC-0300
title: Needs lifecycle proof
status: accepted
---

# DEC-0300: Needs lifecycle proof
`)
	plan, err := BuildLifecycleRepairPlan(dir, time.Date(2026, 6, 29, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if plan.RepairCount != 1 {
		t.Fatalf("repair count = %d", plan.RepairCount)
	}

	dry, err := ApplyLifecycleRepairPlan(plan, RepairApplyOptions{DryRun: true}, time.Date(2026, 6, 29, 23, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if dry.AppliedFields != 2 || len(dry.ChangedFiles) != 1 || strings.Contains(string(body), "falsifier:") {
		t.Fatalf("dry run report=%+v body=%s", dry, body)
	}

	plan.Status = "reviewed"
	written, err := ApplyLifecycleRepairPlan(plan, RepairApplyOptions{DryRun: false}, time.Date(2026, 6, 29, 23, 2, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if written.AppliedFields != 2 || len(written.ChangedFiles) != 1 {
		t.Fatalf("write report=%+v", written)
	}
	if dry.ID == written.ID {
		t.Fatalf("dry-run and write reports should have distinct ids: %s", dry.ID)
	}
	if !strings.Contains(string(body), "falsifier:") || !strings.Contains(string(body), "review_trigger:") {
		t.Fatalf("body missing lifecycle fields:\n%s", body)
	}
	after, err := BuildLifecycleRepairPlan(dir, time.Date(2026, 6, 29, 23, 3, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if after.RepairCount != 0 || after.Status != "ok" {
		t.Fatalf("after repair plan = %+v", after)
	}
}

func TestRecordAppliedLifecycleRepairPlanTerminalizesReviewQueue(t *testing.T) {
	home := t.TempDir()
	plan := LifecycleRepairPlan{
		SchemaVersion:      RepairPlanSchemaVersion,
		ID:                 "dlrp_done",
		Status:             "reviewed",
		SourceRoot:         filepath.Join(home, "decisions"),
		RepairCount:        1,
		MissingFieldCounts: map[string]int{"falsifier": 1},
		Repairs: []LifecycleRepair{{
			DecisionID: "DEC-0302",
			SourcePath: filepath.Join(home, "decisions", "DEC-0302.md"),
			Missing:    []string{"falsifier"},
			Fields: []LifecycleRepairField{{
				Name:           "falsifier",
				SuggestedValue: "done",
			}},
		}},
	}
	paths, err := RecordAppliedLifecycleRepairPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := LoadLifecycleRepairPlan(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	report := reviewartifact.QueueReport{
		Items: []reviewartifact.QueueItem{{
			Area:        lifecycleRepairArea,
			ID:          stored.ID,
			Status:      stored.Status,
			JSONPath:    paths.PlanPath,
			RepairCount: stored.RepairCount,
		}},
	}
	AnnotateLifecycleRepairQueue(&report)
	item := report.Items[0]
	if stored.Status != "ok" || stored.RepairCount != 0 || len(stored.Repairs) != 0 {
		t.Fatalf("stored plan not terminal: %+v", stored)
	}
	if report.Counts.ReadyToApply != 0 || item.Readiness != "no_action" || len(item.Actions) != 0 {
		t.Fatalf("terminal plan still actionable: counts=%+v item=%+v", report.Counts, item)
	}
	if !LifecycleRepairApplyTerminal(LifecycleRepairApplyReport{Status: "ok"}) {
		t.Fatal("successful write report should terminalize plan")
	}
	if !LifecycleRepairApplyTerminal(LifecycleRepairApplyReport{
		Status: "no_changes",
		Skipped: []RepairApplySkip{{
			Reason: "frontmatter field already present",
		}},
	}) {
		t.Fatal("already-present fields should terminalize reviewed plan")
	}
	if LifecycleRepairApplyTerminal(LifecycleRepairApplyReport{
		Status: "no_changes",
		Skipped: []RepairApplySkip{{
			Reason: "source path outside repair plan source_root",
		}},
	}) {
		t.Fatal("unsafe skips must stay actionable")
	}
}

func TestApplyLifecycleRepairPlanBlocksUnreviewedWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "decisions", "DEC-0301.md")
	writeFile(t, path, `---
id: DEC-0301
title: Needs reviewed write
status: accepted
---

# DEC-0301: Needs reviewed write
`)
	plan, err := BuildLifecycleRepairPlan(dir, time.Date(2026, 6, 29, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	report, err := ApplyLifecycleRepairPlan(plan, RepairApplyOptions{DryRun: false}, time.Date(2026, 6, 29, 23, 2, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("expected unreviewed write to fail")
	}
	body, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(body), "falsifier:") || strings.Contains(string(body), "review_trigger:") {
		t.Fatalf("unreviewed write mutated source:\n%s", body)
	}
	if report.Status != "blocked" || report.PlanStatus != "review_required" || len(report.Skipped) != 1 {
		t.Fatalf("blocked report = %+v", report)
	}
	if !strings.Contains(report.Skipped[0].Reason, "not reviewed") {
		t.Fatalf("blocked reason = %+v", report.Skipped[0])
	}
}

func TestApplyLifecycleRepairPlanRejectsPathsOutsideSourceRoot(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "DEC-0400.md")
	writeFile(t, outside, `---
id: DEC-0400
title: Outside root
status: accepted
---

# DEC-0400: Outside root
`)
	plan := LifecycleRepairPlan{
		SchemaVersion: RepairPlanSchemaVersion,
		ID:            "dlrp_outside",
		Status:        "reviewed",
		SourceRoot:    dir,
		Repairs: []LifecycleRepair{{
			DecisionID: "DEC-0400",
			SourcePath: outside,
			Missing:    []string{"falsifier"},
			Fields: []LifecycleRepairField{{
				Name:           "falsifier",
				SuggestedValue: "outside root",
			}},
		}},
	}
	report, err := ApplyLifecycleRepairPlan(plan, RepairApplyOptions{}, time.Date(2026, 6, 29, 23, 4, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.AppliedFields != 0 || len(report.Skipped) != 1 || report.Skipped[0].Reason != "source path outside repair plan source_root" {
		t.Fatalf("report = %+v", report)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
