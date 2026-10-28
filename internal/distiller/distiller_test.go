package distiller

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
	"compound/internal/writeback"
)

var t0 = time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

func setup(t *testing.T) (string, *mem.Store) {
	t.Helper()
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	return home, store
}

// run uses FixturePolicy (auto-promote) so the dup/supersede/contradict MECHANICS tests below stay
// about the ledger, not the promotion-basis gate. The basis gate has its own tests (live mode).
func run(t *testing.T, home string, store *mem.Store, raw string) RunResult {
	t.Helper()
	return runWith(t, home, store, raw, FixturePolicy())
}

func runWith(t *testing.T, home string, store *mem.Store, raw string, p Policy) RunResult {
	t.Helper()
	res, err := Run(home, store, []byte(raw), p, ReferenceExtractor{}, t0)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

func verdicts(res RunResult) map[string]string {
	m := map[string]string{} // kind -> verdict (last wins; fine for distinct kinds)
	for _, d := range res.Decisions {
		m[d.Kind] = d.Verdict
	}
	return m
}

// The headline acceptance set on a fresh store: low-confidence parks, new memory promotes,
// a method upgrade parks (never auto-applied), a capability observation + eval case
// operationalize, and an ordinary skill candidate parks.
func TestRunVerdicts(t *testing.T) {
	home, store := setup(t)
	trace := `{"trace_ref":"trace_1","candidates":[
	  {"kind":"memory","entity":"project:api","predicate":"db","object":"postgres","confidence":0.9},
	  {"kind":"memory","entity":"project:api","predicate":"cache","object":"redis","confidence":0.3},
	  {"kind":"method_upgrade","subject":"distill.memory","statement":"ground dates","confidence":0.8},
	  {"kind":"capability_observation","subject":"skill:cro","statement":"missed_when","confidence":0.7},
	  {"kind":"eval_case","subject":"distill.skill","statement":"missed a skill","confidence":1.0},
	  {"kind":"skill","subject":"paywall-cro","statement":"useful lens","confidence":0.8}
	]}`
	res := run(t, home, store, trace)
	v := verdicts(res)
	want := map[string]string{
		KindMethodUpgrade: Park,           // a method change is never auto-applied
		KindCapabilityObs: Operationalize, // feeds the capability graph
		KindEvalCase:      Operationalize, // distiller's own corpus
		KindSkill:         Park,
	}
	for k, exp := range want {
		if v[k] != exp {
			t.Errorf("%s verdict = %q, want %q", k, v[k], exp)
		}
	}
	// the high-confidence memory promoted to a claim; the low-confidence one parked.
	var promoted, parked *AdjudicationDecision
	for i := range res.Decisions {
		d := &res.Decisions[i]
		if d.Kind == KindMemory && d.Verdict == Promote {
			promoted = d
		}
		if d.Kind == KindMemory && d.Verdict == Park {
			parked = d
		}
	}
	if promoted == nil || promoted.ClaimID == "" {
		t.Error("the 0.9 memory candidate should promote to a claim")
	}
	if parked == nil {
		t.Error("the 0.3 memory candidate should park (below the confidence floor)")
	}
}

// Same trace replayed ⇒ identical candidate ids (idempotent distillation).
func TestReplayStableIDs(t *testing.T) {
	raw := []byte(`{"trace_ref":"trace_x","candidates":[{"kind":"memory","entity":"e","predicate":"p","object":"o","confidence":0.9}]}`)
	a, _ := ReferenceExtractor{}.Extract(raw, DefaultPolicy())
	b, _ := ReferenceExtractor{}.Extract(raw, DefaultPolicy())
	if a.Candidates[0].ID != b.Candidates[0].ID {
		t.Errorf("replay must produce stable ids: %q != %q", a.Candidates[0].ID, b.Candidates[0].ID)
	}
	if a.Candidates[0].PolicyVersion != "default.v1" {
		t.Errorf("candidate must carry the policy version, got %q", a.Candidates[0].PolicyVersion)
	}
}

func TestSeedCandidateParksLowSalienceWithExpectation(t *testing.T) {
	home, store := setup(t)
	// The seed clock is a horizon relative to the run clock, not an absolute
	// literal, so the trace can never expire out from under the test the way
	// the hardcoded 2026-08-01T00:00:00Z did (the incident the repo-wide
	// clock-hygiene guard post-mortems).
	expiresAt := t0.Add(365 * 24 * time.Hour).Format(time.RFC3339)
	trace := `{"trace_ref":"trace_seed","candidates":[
	  {
	    "kind":"memory",
	    "entity":"project:alpha",
	    "predicate":"operator_stance",
	    "object":"integration risk will matter later",
	    "confidence":0.92,
	    "promotion_basis":"explicit_confirmation",
	    "expectation":{"entity":"project:alpha","predicate":"runtime_outcome","object":"integration risk materialized"},
	    "expires_at":"` + expiresAt + `"
	  }
	]}`

	res := runWith(t, home, store, trace, FixturePolicy())
	if len(res.Decisions) != 1 {
		t.Fatalf("decisions = %d, want 1: %+v", len(res.Decisions), res.Decisions)
	}
	decision := res.Decisions[0]
	if decision.Verdict != Park || decision.ClaimID != "" {
		t.Fatalf("seed decision = verdict %q claim %q, want parked with no claim", decision.Verdict, decision.ClaimID)
	}
	if !strings.Contains(decision.Basis, "seed expectation") {
		t.Fatalf("seed basis should explain expectation parking, got %q", decision.Basis)
	}

	data, err := os.ReadFile(filepath.Join(home, candidates.InboxDir(candidates.Memory), decision.CandidateID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved mem.Candidate
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Expectation == nil ||
		saved.Expectation.Entity != "project:alpha" ||
		saved.Expectation.Predicate != "runtime_outcome" ||
		saved.Expectation.Object != "integration risk materialized" {
		t.Fatalf("saved expectation = %+v", saved.Expectation)
	}
	if saved.State != "parked" || saved.CreatedAt == "" {
		t.Fatalf("seed state = %q created_at=%q, want parked with timestamp", saved.State, saved.CreatedAt)
	}
	if saved.Salience != "low" || saved.Visibility != "contextual" || saved.ExpiresAt != expiresAt {
		t.Fatalf("seed class tier = salience %q visibility %q expires %q", saved.Salience, saved.Visibility, saved.ExpiresAt)
	}
	if !stringSliceContains(saved.Tags, "seed") {
		t.Fatalf("seed tags = %+v, want seed tag", saved.Tags)
	}
	recall, err := store.Recall("project:alpha", mem.RecallOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recall.Claims) != 0 {
		t.Fatalf("seed must never auto-promote into recall, got %+v", recall.Claims)
	}
}

func TestNormalizeTraceRejectsThinCandidates(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "memory missing object",
			raw:  `{"trace_ref":"trace_shape","candidates":[{"kind":"memory","entity":"svc","predicate":"lang","confidence":0.9}]}`,
			want: "memory requires object",
		},
		{
			name: "skill missing statement",
			raw:  `{"trace_ref":"trace_shape","candidates":[{"kind":"skill","subject":"skill:pricing","confidence":0.82}]}`,
			want: "skill requires statement",
		},
		{
			name: "confidence outside range",
			raw:  `{"trace_ref":"trace_shape","candidates":[{"kind":"decision","subject":"decision:router","statement":"prefer cheap sufficient lane","confidence":1.2}]}`,
			want: "outside [0,1]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReferenceExtractor{}.Extract([]byte(tt.raw), DefaultPolicy())
			if err == nil {
				t.Fatal("expected shape validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

// Distill against canon: a duplicate memory is rejected; a changed value supersedes the prior claim.
func TestDuplicateAndSupersession(t *testing.T) {
	home, store := setup(t)
	run(t, home, store, `{"trace_ref":"t1","candidates":[{"kind":"memory","entity":"svc","predicate":"lang","object":"node","confidence":0.9}]}`)
	res := run(t, home, store, `{"trace_ref":"t2","candidates":[
	  {"kind":"memory","entity":"svc","predicate":"lang","object":"node","confidence":0.9},
	  {"kind":"memory","entity":"svc","predicate":"lang","object":"go","confidence":0.9}
	]}`)
	var dup, sup *AdjudicationDecision
	for i := range res.Decisions {
		d := &res.Decisions[i]
		if d.Verdict == Reject {
			dup = d
		}
		if d.Verdict == Promote {
			sup = d
		}
	}
	if dup == nil {
		t.Error("the duplicate 'node' memory should be rejected against prior canon")
	}
	if sup == nil || sup.ClaimID == "" {
		t.Error("the changed 'go' memory should promote (superseding)")
	}
	// the ledger now holds 'go' as current.
	cur, _ := store.AllCurrent()
	if len(cur) != 1 || cur[0].Object != "go" {
		t.Errorf("after supersession, current should be 'go'; got %v", cur)
	}
}

func TestDuplicateMemoryRecordsNoPromote(t *testing.T) {
	home, store := setup(t)
	first := run(t, home, store, `{"trace_ref":"t1","candidates":[{"kind":"memory","entity":"svc","predicate":"lang","object":"node","confidence":0.9}]}`)
	claimID := first.Decisions[0].ClaimID
	if claimID == "" {
		t.Fatal("setup claim was not promoted")
	}

	dup := run(t, home, store, `{"trace_ref":"t2","candidates":[{"kind":"memory","entity":"svc","predicate":"lang","object":"node","confidence":0.9}]}`)
	rejected := firstDecisionWithVerdict(t, dup, Reject)
	if rejected.NoPromoteID == "" {
		t.Fatalf("duplicate decision missing no_promote_id: %+v", rejected)
	}
	if rejected.PriorClaimID != claimID {
		t.Fatalf("prior claim = %q, want %q", rejected.PriorClaimID, claimID)
	}

	nopRows := readNoPromoteRows(t, home)
	if len(nopRows) != 1 {
		t.Fatalf("no-promote rows = %d, want 1", len(nopRows))
	}
	nop := nopRows[0]
	if nop.ID != rejected.NoPromoteID {
		t.Fatalf("no-promote id = %q, want %q", nop.ID, rejected.NoPromoteID)
	}
	if nop.CandidateRef != rejected.CandidateID || nop.CandidateKind != KindMemory {
		t.Fatalf("candidate ref/kind = %q/%q, want %q/%q", nop.CandidateRef, nop.CandidateKind, rejected.CandidateID, KindMemory)
	}
	if nop.OwnerTarget.Kind != "memory_claim" || nop.OwnerTarget.Ref != claimID {
		t.Fatalf("owner target = %+v, want memory_claim %q", nop.OwnerTarget, claimID)
	}
	if !hasString(nop.EvidenceRefs, "prior_claim:"+claimID) {
		t.Fatalf("evidence refs missing prior claim: %v", nop.EvidenceRefs)
	}

	replayed := run(t, home, store, `{"trace_ref":"t2","candidates":[{"kind":"memory","entity":"svc","predicate":"lang","object":"node","confidence":0.9}]}`)
	replayedReject := firstDecisionWithVerdict(t, replayed, Reject)
	if replayedReject.NoPromoteID != rejected.NoPromoteID {
		t.Fatalf("replay no_promote_id = %q, want %q", replayedReject.NoPromoteID, rejected.NoPromoteID)
	}
	if rows := readNoPromoteRows(t, home); len(rows) != 1 {
		t.Fatalf("replay appended duplicate no-promote rows: %d", len(rows))
	}
}

// Duplicate non-memory candidate families use prior inbox coverage as an anti-churn
// gate; the no-promote row is the durable record, not another candidate file.
func TestDuplicateNonMemoryCandidateRecordsNoPromote(t *testing.T) {
	home, store := setup(t)
	first := run(t, home, store, `{"trace_ref":"t1","candidates":[{"kind":"skill","subject":"paywall-cro","statement":"use this lens","confidence":0.9}]}`)
	if first.Decisions[0].Verdict != Park {
		t.Fatalf("setup skill verdict = %q, want park", first.Decisions[0].Verdict)
	}

	dup := run(t, home, store, `{"trace_ref":"t2","candidates":[{"kind":"skill","subject":" Paywall-CRO ","statement":"USE THIS LENS","confidence":0.2}]}`)
	rejected := firstDecisionWithVerdict(t, dup, Reject)
	if rejected.PriorCandidateID != first.Decisions[0].CandidateID {
		t.Fatalf("prior candidate = %q, want %q", rejected.PriorCandidateID, first.Decisions[0].CandidateID)
	}
	if rejected.PriorClaimID != "" {
		t.Fatalf("non-memory duplicate must not point at prior claim: %+v", rejected)
	}
	if rejected.NoPromoteID == "" {
		t.Fatalf("duplicate non-memory candidate missing no_promote_id: %+v", rejected)
	}

	skillDir := candidates.InboxDir(candidates.Skill)
	files, _ := filepath.Glob(filepath.Join(home, skillDir, "*.json"))
	if len(files) != 1 {
		t.Fatalf("duplicate skill should not create another inbox row; found %d", len(files))
	}

	rows := readNoPromoteRows(t, home)
	if len(rows) != 1 {
		t.Fatalf("no-promote rows = %d, want 1", len(rows))
	}
	nop := rows[0]
	if nop.OwnerTarget.Kind != "candidate" || nop.OwnerTarget.Ref != first.Decisions[0].CandidateID {
		t.Fatalf("owner target = %+v, want candidate %q", nop.OwnerTarget, first.Decisions[0].CandidateID)
	}
	if !hasString(nop.EvidenceRefs, "prior_candidate:"+first.Decisions[0].CandidateID) {
		t.Fatalf("evidence refs missing prior candidate: %v", nop.EvidenceRefs)
	}
}

func TestSameTraceDuplicateNonMemoryCandidateUsesNoPromote(t *testing.T) {
	home, store := setup(t)
	res := run(t, home, store, `{"trace_ref":"t_same","candidates":[
		{"kind":"taste","subject":"product:visuals","statement":"avoid generic gradients","confidence":0.9},
		{"kind":"taste","subject":" PRODUCT:VISUALS ","statement":"Avoid Generic Gradients","confidence":0.9}
	]}`)
	if len(res.Decisions) != 2 {
		t.Fatalf("decisions = %d, want 2", len(res.Decisions))
	}
	if res.Decisions[0].Verdict != Park {
		t.Fatalf("first duplicate-family candidate verdict = %q, want park", res.Decisions[0].Verdict)
	}
	rejected := res.Decisions[1]
	if rejected.Verdict != Reject || rejected.Relation != "duplicate" {
		t.Fatalf("second duplicate-family candidate = %q/%q, want reject/duplicate", rejected.Verdict, rejected.Relation)
	}
	if rejected.PriorCandidateID != res.Decisions[0].CandidateID {
		t.Fatalf("prior candidate = %q, want first candidate %q", rejected.PriorCandidateID, res.Decisions[0].CandidateID)
	}
	if rejected.NoPromoteID == "" {
		t.Fatalf("same-trace duplicate missing no_promote_id: %+v", rejected)
	}
	tasteDir := candidates.InboxDir(candidates.Taste)
	files, _ := filepath.Glob(filepath.Join(home, tasteDir, "*.json"))
	if len(files) != 1 {
		t.Fatalf("same-trace duplicate should write one inbox row; found %d", len(files))
	}
}

// No candidate enters ambient recall without adjudication: a parked skill candidate is a file in
// the inbox, NOT a claim — recall/AllCurrent never surfaces it.
func TestNoCandidateToAmbientWithoutAdjudication(t *testing.T) {
	home, store := setup(t)
	run(t, home, store, `{"trace_ref":"t","candidates":[{"kind":"skill","subject":"s","statement":"x","confidence":0.9}]}`)
	cur, _ := store.AllCurrent()
	if len(cur) != 0 {
		t.Errorf("a parked skill candidate must not become a claim; got %d claims", len(cur))
	}
	// it IS recorded as a candidate in the inbox (candidate-first), and as an adjudication.
	skillDir := candidates.InboxDir(candidates.Skill)
	files, _ := filepath.Glob(filepath.Join(home, skillDir, "*.json"))
	if len(files) != 1 {
		t.Errorf("the skill candidate should be written to %s; found %d", skillDir, len(files))
	}
	adj, _ := os.ReadFile(filepath.Join(home, "ledger", "adjudications.jsonl"))
	var d AdjudicationDecision
	if json.Unmarshal([]byte(firstLine(adj)), &d) != nil || d.Verdict != Park {
		t.Errorf("the skill candidate should have a parked adjudication decision; got %q", d.Verdict)
	}
}

// A weaker candidate conflicting with HIGHER-confidence current canon is a CONTRADICTION → it parks
// for operator review, never silently supersedes the stronger claim.
func TestContradictionParks(t *testing.T) {
	home, store := setup(t)
	run(t, home, store, `{"trace_ref":"t1","candidates":[{"kind":"memory","entity":"svc","predicate":"lang","object":"go","confidence":0.95}]}`)
	res := run(t, home, store, `{"trace_ref":"t2","candidates":[{"kind":"memory","entity":"svc","predicate":"lang","object":"rust","confidence":0.4}]}`)
	d := res.Decisions[0]
	if d.Verdict != Park || d.Relation != "contradicts" {
		t.Errorf("a 0.4 conflict vs 0.95 canon should PARK as a contradiction; got verdict=%q relation=%q", d.Verdict, d.Relation)
	}
	// canon unchanged — the weak contradiction did not supersede the strong claim.
	cur, _ := store.AllCurrent()
	if len(cur) != 1 || cur[0].Object != "go" {
		t.Errorf("canon should still be 'go'; got %v", cur)
	}
}

// Two candidates differing ONLY in Detail get distinct ids (no silent inbox collision).
func TestDetailDistinguishesIDs(t *testing.T) {
	raw := []byte(`{"trace_ref":"t","candidates":[
	  {"kind":"capability_observation","subject":"skill:x","statement":"ctx","detail":"missed_when","confidence":0.8},
	  {"kind":"capability_observation","subject":"skill:x","statement":"ctx","detail":"false_triggered_by","confidence":0.8}
	]}`)
	ext, err := ReferenceExtractor{}.Extract(raw, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if ext.Candidates[0].ID == ext.Candidates[1].ID {
		t.Errorf("candidates differing in Detail must get distinct ids; both = %q", ext.Candidates[0].ID)
	}
}

// An unknown / path-bearing kind is rejected — Kind keys a filesystem path and must stay typed.
func TestUnknownKindRejected(t *testing.T) {
	for _, k := range []string{"bogus", "../../evil"} {
		raw := []byte(`{"trace_ref":"t","candidates":[{"kind":"` + k + `","subject":"x","confidence":0.9}]}`)
		if _, err := (ReferenceExtractor{}).Extract(raw, DefaultPolicy()); err == nil {
			t.Errorf("kind %q should be rejected", k)
		}
	}
}

// In live mode (the DEFAULT), a high-confidence model-extracted memory candidate with NO promotion
// basis PARKS — "the model was confident" is not a basis. Canon stays empty.
func TestLiveModeParksMemoryWithoutBasis(t *testing.T) {
	home, store := setup(t)
	res := runWith(t, home, store,
		`{"trace_ref":"t","candidates":[{"kind":"memory","entity":"svc","predicate":"lang","object":"go","confidence":0.95}]}`,
		DefaultPolicy())
	if res.Decisions[0].Verdict != Park {
		t.Errorf("live-mode memory without a basis must PARK; got %q (%s)", res.Decisions[0].Verdict, res.Decisions[0].Basis)
	}
	if cur, _ := store.AllCurrent(); len(cur) != 0 {
		t.Errorf("nothing should be canon without a basis; got %d claims", len(cur))
	}
	if res.Decisions[0].NoPromoteID != "" {
		t.Fatalf("parked review candidate should not get no-promote id: %+v", res.Decisions[0])
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "no-promote-decisions.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("parked review candidate should not write no-promote ledger, err=%v", err)
	}
}

func TestDuplicateParkedMemoryCandidateUsesNoPromote(t *testing.T) {
	home, store := setup(t)
	first := runWith(t, home, store, `{"trace_ref":"t1","candidates":[{"kind":"memory","entity":"svc","predicate":"lang","object":"go","confidence":0.95}]}`, DefaultPolicy())
	if first.Decisions[0].Verdict != Park {
		t.Fatalf("setup memory should park without basis, got %+v", first.Decisions[0])
	}

	dup := runWith(t, home, store, `{"trace_ref":"t2","candidates":[{"kind":"memory","entity":" SVC ","predicate":"LANG","object":"go","confidence":0.95}]}`, DefaultPolicy())
	rejected := firstDecisionWithVerdict(t, dup, Reject)
	if rejected.PriorCandidateID != first.Decisions[0].CandidateID {
		t.Fatalf("prior candidate = %q, want %q", rejected.PriorCandidateID, first.Decisions[0].CandidateID)
	}
	if rejected.NoPromoteID == "" {
		t.Fatalf("duplicate parked memory missing no_promote_id: %+v", rejected)
	}
	files, _ := filepath.Glob(filepath.Join(home, candidates.InboxDir(candidates.Memory), "*.json"))
	if len(files) != 1 {
		t.Fatalf("duplicate parked memory should not write another inbox row; found %d", len(files))
	}
}

// A trace may not self-mint explicit confirmation. That basis is reserved for
// the governed operator-confirmation adapter, so reference/model-shaped input
// parks without carrying authority into the candidate.
func TestLiveModeParksUnprovenExplicitConfirmation(t *testing.T) {
	home, store := setup(t)
	raw := `{"trace_ref":"t","candidates":[{"kind":"memory","entity":"svc","predicate":"lang","object":"go","confidence":0.95,"promotion_basis":"` + candidates.BasisExplicitConfirmation + `"}]}`
	res := runWith(t, home, store, raw, DefaultPolicy())
	d := res.Decisions[0]
	if d.Verdict != Park || d.ClaimID != "" {
		t.Fatalf("unproven explicit_confirmation = verdict %q claim=%q, want parked", d.Verdict, d.ClaimID)
	}
	if d.PromotionBasis != "" {
		t.Errorf("untrusted trace retained reserved basis %q", d.PromotionBasis)
	}
	cur, _ := store.AllCurrent()
	if len(cur) != 0 {
		t.Fatalf("unproven trace wrote claims: %+v", cur)
	}
	parked, err := store.AllCandidates()
	if err != nil || len(parked) != 1 || parked[0].State != "parked" || parked[0].PromotionBasis != "" {
		t.Fatalf("parked candidate = %+v, err=%v", parked, err)
	}
}

// An unknown basis string is treated as NO basis (parks) — only allowlisted bases promote.
func TestLiveModeInvalidBasisParks(t *testing.T) {
	home, store := setup(t)
	res := runWith(t, home, store,
		`{"trace_ref":"t","candidates":[{"kind":"memory","entity":"svc","predicate":"lang","object":"go","confidence":0.95,"promotion_basis":"vibes"}]}`,
		DefaultPolicy())
	if res.Decisions[0].Verdict != Park {
		t.Errorf("an unknown basis must be treated as no basis (park); got %q", res.Decisions[0].Verdict)
	}
}

func firstLine(b []byte) string {
	for i, c := range b {
		if c == '\n' {
			return string(b[:i])
		}
	}
	return string(b)
}

func firstDecisionWithVerdict(t *testing.T, res RunResult, verdict string) AdjudicationDecision {
	t.Helper()
	for _, d := range res.Decisions {
		if d.Verdict == verdict {
			return d
		}
	}
	t.Fatalf("no decision with verdict %q in %+v", verdict, res.Decisions)
	return AdjudicationDecision{}
}

func readNoPromoteRows(t *testing.T, home string) []writeback.NoPromoteDecision {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "ledger", "no-promote-decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	rows := make([]writeback.NoPromoteDecision, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var d writeback.NoPromoteDecision
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("parse no-promote row %q: %v", line, err)
		}
		rows = append(rows, d)
	}
	return rows
}

func hasString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
