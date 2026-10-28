package harvest_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/decisions"
	"compound/internal/harvest"
	"compound/internal/mem"
)

// The real accepted record claim_f9160895524e carries a ~600-char Object and the
// slot slug "autonomy-by-envelope". A short historical utterance cannot cover a
// 600-char object under token coverage, so this mirrors the real record: a LONG
// object + the real slot slug, exercising the SAME slot-identity path the live
// run hits.
const realAutonomyObject = "Autonomy-by-envelope: reversible actions inside a scoped grant proceed WITHOUT per-item operator gates — the operator stays in charge by setting envelopes (scope, budget, blast radius), not by being paged. Per-item permission-asking inside a live grant is a defect (uniform caution = conservatism), not safety. The risk fence stays by construction: irreversible, external-facing, spend, and destructive actions remain operator-gated regardless of grants."

func recordRulingWithID(t *testing.T, store *mem.Store, dir, id, title string) decisions.RecordReport {
	t.Helper()
	rep, err := decisions.Record(store, dir, decisions.RecordInput{
		Kind:       decisions.KindRuling,
		ID:         id,
		Title:      title,
		ReviewedBy: "operator:test",
		Now:        time.Unix(1_700_000_100, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("record ruling %q: %v", id, err)
	}
	if rep.Import.ClaimID == "" {
		t.Fatalf("recorded ruling %q did not become an active claim: %+v", id, rep.Import)
	}
	return rep
}

type ledgerFixture struct {
	body   string
	conf   string // "" -> omit meta subline
	domain string
	quote  string // "" -> omit quote subline
	inline bool   // put conf/domain inline on the body line instead of a subline
}

// writeLedger writes one decision-ledger.generated.md under dir/<session>/ and
// returns the ledger dir root.
func writeLedger(t *testing.T, root, session, updatedAt string, fx ...ledgerFixture) {
	t.Helper()
	sessDir := filepath.Join(root, session)
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var b []byte
	add := func(s string) { b = append(b, []byte(s)...) }
	add("---\n")
	add("type: decision-ledger\n")
	add("generated_from: /Users/example/.claude/projects/" + session + ".jsonl\n")
	add("updated_at: " + updatedAt + "\n")
	add("status: active\n")
	add("---\n\n")
	add("# Decisions — " + session + "\n\n## Active decisions\n")
	for _, f := range fx {
		line := "- **[SELECT]** " + f.body
		if f.inline && f.conf != "" {
			line += " — confidence: " + f.conf + " · domain: " + f.domain
		}
		add(line + "\n")
		if !f.inline && f.conf != "" {
			add("  - confidence: " + f.conf + " · domain: " + f.domain + "\n")
		}
		if f.quote != "" {
			add("  - quote: \"" + f.quote + "\"\n")
		}
		add("\n")
	}
	if err := os.WriteFile(filepath.Join(sessDir, "decision-ledger.generated.md"), b, 0o644); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
}

// FALSIFIER f: a historical "autonomy only within a pre-authorised envelope"
// decision must reconcile as CORROBORATION of the accepted autonomy-by-envelope
// record — ZERO duplicate candidate. If it mints a duplicate, cluster
// reconciliation is broken.
func TestBackfill_EnvelopeAutonomyCorroboratesNoDuplicate(t *testing.T) {
	dir, store := mustHome(t)
	recordRulingWithID(t, store, dir, "autonomy-by-envelope-2026-07-11", realAutonomyObject)

	ledgerRoot := filepath.Join(dir, "ledgers")
	writeLedger(t, ledgerRoot, "sess-a", "2026-06-28",
		ledgerFixture{
			body: "Allow agents autonomy only within a pre-authorised envelope; any task outside escalates for approval.",
			conf: "high", domain: "autonomy", quote: "allow agents autonomy only within a pre-authorised envelope",
		},
	)

	report, err := harvest.Backfill(store, dir, harvest.BackfillOptions{
		LedgerDir: ledgerRoot, DryRun: false, Now: time.Unix(1_700_000_500, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if report.RouteCounts.System != 1 {
		t.Fatalf("envelope-autonomy entry did not route to system: %+v", report.RouteCounts)
	}
	if report.NovelClustersFound != 0 || report.NovelEmitted != 0 {
		t.Fatalf("envelope-autonomy minted a novel candidate (found=%d emitted=%d); falsifier f BROKEN", report.NovelClustersFound, report.NovelEmitted)
	}
	if n := decisionCandidateCount(t, dir); n != 0 {
		t.Fatalf("envelope-autonomy wrote %d decision candidate file(s); want 0 (corroboration only)", n)
	}
	if len(report.Corroborations) != 1 {
		t.Fatalf("expected exactly 1 corroborated record, got %d (%+v)", len(report.Corroborations), report.Corroborations)
	}
	cr := report.Corroborations[0]
	if cr.DecisionID != "autonomy-by-envelope-2026-07-11" {
		t.Fatalf("corroborated the wrong record: %+v", cr)
	}
	if cr.Via != "slot" {
		t.Fatalf("expected slot-identity corroboration, got via=%q", cr.Via)
	}
	// The reinforces edge must exist on the claim (idempotent corroboration writer).
	refs, err := store.CorroborationEvidence(cr.ClaimID)
	if err != nil {
		t.Fatalf("corroboration evidence: %v", err)
	}
	if len(refs) == 0 {
		t.Fatalf("no reinforces edge written for %s", cr.ClaimID)
	}

	// Idempotent replay: a second run must not mint a duplicate candidate.
	report2, err := harvest.Backfill(store, dir, harvest.BackfillOptions{
		LedgerDir: ledgerRoot, DryRun: false, Now: time.Unix(1_700_000_600, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("backfill re-run: %v", err)
	}
	if report2.NovelEmitted != 0 || decisionCandidateCount(t, dir) != 0 {
		t.Fatalf("re-run minted duplicates: emitted=%d files=%d", report2.NovelEmitted, decisionCandidateCount(t, dir))
	}
}

// Slot matching is precision-first: an unrelated system ruling must NOT
// corroborate the autonomy record just because it shares one slot token.
func TestBackfill_SlotMatchIsPrecise(t *testing.T) {
	dir, store := mustHome(t)
	recordRulingWithID(t, store, dir, "autonomy-by-envelope-2026-07-11", realAutonomyObject)

	ledgerRoot := filepath.Join(dir, "ledgers")
	writeLedger(t, ledgerRoot, "sess-b", "2026-06-28",
		// Mentions "autonomy" and "agents" but NOT "envelope" -> must not slot-match.
		ledgerFixture{body: "From now on grant agents more autonomy on cheap lanes; premium lanes only judge.", conf: "high", domain: "delegation"},
	)
	report, err := harvest.Backfill(store, dir, harvest.BackfillOptions{LedgerDir: ledgerRoot, DryRun: true, Now: time.Unix(1_700_000_500, 0).UTC()})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(report.Corroborations) != 0 {
		t.Fatalf("partial slot overlap wrongly corroborated: %+v", report.Corroborations)
	}
	if report.NovelClustersFound != 1 {
		t.Fatalf("expected the unrelated ruling to land novel, got %d clusters", report.NovelClustersFound)
	}
}

// Parser grammar: inline meta, subline meta, and a missing quote all parse.
func TestParseLedgerFile_Grammar(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, "sess-c", "2026-06-27",
		ledgerFixture{body: "Consolidate the hook into the spine", conf: "high", domain: "spine-coherence", quote: "okay go ahead"},
		ledgerFixture{body: "Treat approval as an execution envelope, not a courtroom ritual", conf: "high", domain: "approval-pattern", inline: true},
		ledgerFixture{body: "Reject ambient personal corpus injection into the system"}, // no meta, no quote
	)
	entries, issues, files, err := harvest.ParseLedgerDir(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if files != 1 {
		t.Fatalf("files parsed = %d, want 1", files)
	}
	if len(issues) != 0 {
		t.Fatalf("unexpected parse issues: %+v", issues)
	}
	if len(entries) != 3 {
		t.Fatalf("parsed %d entries, want 3", len(entries))
	}
	if entries[0].Kind != "SELECT" || entries[0].Confidence != "high" || entries[0].Domain != "spine-coherence" || entries[0].Quote != "okay go ahead" {
		t.Fatalf("subline-meta entry parsed wrong: %+v", entries[0])
	}
	if entries[1].Domain != "approval-pattern" || entries[1].Confidence != "high" {
		t.Fatalf("inline-meta entry parsed wrong: %+v", entries[1])
	}
	if entries[1].Body != "Treat approval as an execution envelope, not a courtroom ritual" {
		t.Fatalf("inline meta not stripped from body: %q", entries[1].Body)
	}
	if entries[2].Quote != "" || entries[2].Domain != "" {
		t.Fatalf("no-meta entry should have empty quote/domain: %+v", entries[2])
	}
	if entries[0].UpdatedAt != "2026-06-27" || entries[0].SourceTranscript == "" {
		t.Fatalf("frontmatter not attributed: %+v", entries[0])
	}
}

// Routing: a product decision goes to the product report; a bare approval is
// ephemeral; a system/harness ruling routes to reconcile.
func TestRouteEntry_ThreeDoors(t *testing.T) {
	sys := harvest.LedgerEntry{Body: "From now on all execution routes to the executor lane; premium lanes only judge.", Domain: "delegation"}
	if r, _ := harvest.RouteEntry(sys); r != harvest.RouteSystem {
		t.Fatalf("system ruling routed to %q", r)
	}
	prod := harvest.LedgerEntry{Body: "Lock the trial paywall to a 7-day trial then annual.", Domain: "monetization", Quote: "go with annual"}
	if r, _ := harvest.RouteEntry(prod); r != harvest.RouteProduct {
		t.Fatalf("product decision routed to %q", r)
	}
	eph := harvest.LedgerEntry{Body: "Proceed with the plan as written", Domain: "workflow", Quote: "go ahead"}
	if r, _ := harvest.RouteEntry(eph); r != harvest.RouteEphemeral {
		t.Fatalf("bare approval routed to %q, want ephemeral", r)
	}
}

// Novel clustering: two restatements of the SAME non-accepted system ruling form
// ONE cluster (corroboration_count 1) and emit exactly ONE parked candidate.
func TestBackfill_NovelClusterOneCandidate(t *testing.T) {
	dir, store := mustHome(t)
	ledgerRoot := filepath.Join(dir, "ledgers")
	writeLedger(t, ledgerRoot, "sess-old", "2026-06-27",
		ledgerFixture{body: "Route all memory recall through the semantic ranker, never the naive lexical ranker.", conf: "high", domain: "recall"},
	)
	writeLedger(t, ledgerRoot, "sess-new", "2026-06-28",
		ledgerFixture{body: "Memory recall must always use the semantic ranker and never the naive lexical ranker.", conf: "high", domain: "recall"},
	)
	report, err := harvest.Backfill(store, dir, harvest.BackfillOptions{LedgerDir: ledgerRoot, DryRun: false, Now: time.Unix(1_700_000_700, 0).UTC()})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if report.NovelClustersFound != 1 {
		t.Fatalf("two restatements formed %d clusters, want 1 (%+v)", report.NovelClustersFound, report.NovelSamples)
	}
	if report.NovelSamples[0].CorroborationCount != 1 {
		t.Fatalf("cluster corroboration_count = %d, want 1", report.NovelSamples[0].CorroborationCount)
	}
	if n := decisionCandidateCount(t, dir); n != 1 {
		t.Fatalf("novel cluster wrote %d candidate files, want 1 (cluster-granularity)", n)
	}
	// The head is the NEWEST instance (updated_at desc).
	if report.NovelSamples[0].Head == "" {
		t.Fatalf("cluster head empty")
	}
	// Candidate is proposed-only (verdict park).
	entries, _ := os.ReadDir(filepath.Join(dir, candidates.InboxDir(candidates.Decision)))
	var rec struct {
		Kind    string `json:"kind"`
		Verdict string `json:"verdict"`
	}
	name := entries[0].Name()
	if err := candidates.ReadCandidate(dir, candidates.Decision, name[:len(name)-len(".json")], &rec); err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	if rec.Verdict != candidates.Park {
		t.Fatalf("candidate verdict = %q, want park", rec.Verdict)
	}
}

// Bounded honesty: the cap truncates emitted candidates and logs the skipped
// count — no silent truncation.
func TestBackfill_NovelCapLogsSkipped(t *testing.T) {
	dir, store := mustHome(t)
	ledgerRoot := filepath.Join(dir, "ledgers")
	writeLedger(t, ledgerRoot, "sess-1", "2026-06-28",
		ledgerFixture{body: "From now on the orchestrator must always batch subagent fan-outs at three.", conf: "high", domain: "orchestration"},
		ledgerFixture{body: "Never run model training on this laptop; always use the batch box.", conf: "high", domain: "training"},
	)
	report, err := harvest.Backfill(store, dir, harvest.BackfillOptions{LedgerDir: ledgerRoot, DryRun: false, NovelCap: 1, Now: time.Unix(1_700_000_800, 0).UTC()})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if report.NovelClustersFound != 2 {
		t.Fatalf("expected 2 novel clusters, got %d", report.NovelClustersFound)
	}
	if report.NovelEmitted != 1 || report.NovelSkippedByCap != 1 {
		t.Fatalf("cap accounting wrong: emitted=%d skipped=%d", report.NovelEmitted, report.NovelSkippedByCap)
	}
	if n := decisionCandidateCount(t, dir); n != 1 {
		t.Fatalf("cap=1 wrote %d candidate files, want 1", n)
	}
}

// Dry-run writes nothing (the live-proof invariant).
func TestBackfill_DryRunWritesNothing(t *testing.T) {
	dir, store := mustHome(t)
	ledgerRoot := filepath.Join(dir, "ledgers")
	writeLedger(t, ledgerRoot, "sess-x", "2026-06-28",
		ledgerFixture{body: "From now on every executor lane must persist partial progress before a timeout.", conf: "high", domain: "execution"},
	)
	report, err := harvest.Backfill(store, dir, harvest.BackfillOptions{LedgerDir: ledgerRoot, DryRun: true, Now: time.Unix(1_700_000_900, 0).UTC()})
	if err != nil {
		t.Fatalf("backfill dry-run: %v", err)
	}
	if report.NovelClustersFound != 1 {
		t.Fatalf("dry-run should still find the novel cluster, got %d", report.NovelClustersFound)
	}
	if n := decisionCandidateCount(t, dir); n != 0 {
		t.Fatalf("dry-run wrote %d candidate files; want 0", n)
	}
}
