package harvest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/decisions"
	"compound/internal/harvest"
	"compound/internal/home"
	"compound/internal/mem"
)

func mustHome(t *testing.T) (string, *mem.Store) {
	t.Helper()
	dir := t.TempDir()
	if err := home.Init(dir, "harvest-test", time.Unix(1_700_000_000, 0).UTC()); err != nil {
		t.Fatalf("home init: %v", err)
	}
	store, err := mem.Open(dir)
	if err != nil {
		t.Fatalf("mem open: %v", err)
	}
	return dir, store
}

func writeTranscript(t *testing.T, dir string, utterances ...string) string {
	t.Helper()
	var b strings.Builder
	// A non-operator noise line the reader must drop.
	b.WriteString(`{"type":"user","timestamp":"2026-07-11T00:00:00Z","message":{"role":"user","content":[{"type":"tool_result","text":""}]}}` + "\n")
	b.WriteString(`{"type":"user","timestamp":"2026-07-11T00:00:01Z","message":{"role":"user","content":"<command-name>/effort</command-name> <command-message>effort</command-message>"}}` + "\n")
	ts := time.Date(2026, 7, 11, 1, 0, 0, 0, time.UTC)
	for i, u := range utterances {
		line := `{"type":"user","timestamp":"` + ts.Add(time.Duration(i)*time.Minute).Format(time.RFC3339) +
			`","message":{"role":"user","content":[{"type":"text","text":` + jsonQuote(u) + `}]}}`
		b.WriteString(line + "\n")
	}
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func jsonQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func recordRuling(t *testing.T, store *mem.Store, dir, title string) decisions.RecordReport {
	t.Helper()
	rep, err := decisions.Record(store, dir, decisions.RecordInput{
		Kind:       decisions.KindRuling,
		Title:      title,
		ReviewedBy: "operator:test",
		Now:        time.Unix(1_700_000_100, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("record ruling: %v", err)
	}
	if rep.Import.ClaimID == "" {
		t.Fatalf("recorded ruling did not become an active claim: %+v", rep.Import)
	}
	return rep
}

func decisionCandidateCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, candidates.InboxDir(candidates.Decision)))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read decision candidates: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// Falsifier (d): a task instruction is NOT a ruling; it must classify to nothing.
func TestClassify_TaskInstructionProducesNothing(t *testing.T) {
	for _, task := range []string{
		"fix the sync bug",
		"add a test for the exporter and run it",
		"refactor internal/mem/mem.go to split the store",
		"implement the harvest verb please",
	} {
		if _, ok := harvest.Classify(task); ok {
			t.Fatalf("task instruction %q wrongly classified as a ruling", task)
		}
	}
}

// A standing-law imperative about system behavior IS a ruling.
func TestClassify_RulingShaped(t *testing.T) {
	for _, r := range []string{
		"From now on all execution must be routed to Codex; premium subscription lanes never execute, they only judge.",
		"Never run ML training on this Mac from now on; always use the fedora batch box.",
		"The rule is: agents get autonomy only inside a pre-authorised envelope.",
	} {
		if _, ok := harvest.Classify(r); !ok {
			t.Fatalf("ruling-shaped utterance %q was not classified", r)
		}
	}
}

// Precision: an essay that mentions the system in passing is NOT a ruling, even
// if it contains standing-law words somewhere (live 2026-07-11 leak: 4000-char
// strategy dumps and skill bodies were classifying as grants).
func TestClassify_EssayIsNotARuling(t *testing.T) {
	essay := "So what should be done, what are next steps? " +
		strings.Repeat("I have a lot of context here about agents, execution, lanes, codex and the system that I always keep in mind and never forget. ", 6) +
		"From now on think about it."
	if _, ok := harvest.Classify(essay); ok {
		t.Fatalf("a %d-char essay was classified as a ruling; precision cap failed", len(essay))
	}
}

// Falsifier (b): a paraphrase of an accepted ruling reconciles as corroboration,
// never a new candidate.
func TestReconcile_ParaphraseCorroborates(t *testing.T) {
	dir, store := mustHome(t)
	recordRuling(t, store, dir, "gbrain database stays external to compound and is never merged into the compound home")
	claims, err := store.AllCurrent()
	if err != nil {
		t.Fatalf("all current: %v", err)
	}
	active := decisions.AllActiveDecisions(claims)
	if len(active) == 0 {
		t.Fatalf("no active decision records to reconcile against")
	}
	paraphrase := "From now on keep the gbrain database external to compound; it must never be folded into the compound home."
	cls, ok := harvest.Classify(paraphrase)
	if !ok {
		t.Fatalf("paraphrase was not classified as a ruling")
	}
	outcome, match := harvest.Reconcile(paraphrase, cls.Kind, active)
	if outcome != harvest.OutcomeCorroboration {
		t.Fatalf("paraphrase reconciled as %q, want corroboration (match=%+v)", outcome, match)
	}
	if match.ClaimID == "" {
		t.Fatalf("corroboration produced no matched claim id")
	}
}

// Falsifier (a): replaying restatements of accepted records yields corroborations
// and ZERO duplicate new candidates.
func TestRun_ReplayCorroboratesNoDuplicates(t *testing.T) {
	dir, store := mustHome(t)
	recordRuling(t, store, dir, "gbrain database stays external to compound and is never merged into the compound home")

	path := writeTranscript(t, dir,
		"From now on the gbrain database stays external to compound and must never be merged into the compound home.",
		"fix the sync bug in the exporter", // task instruction → nothing
	)

	report, err := harvest.Run(store, dir, harvest.Options{TranscriptPath: path, Now: time.Unix(1_700_000_200, 0).UTC()})
	if err != nil {
		t.Fatalf("harvest run: %v", err)
	}
	if report.Corroborations < 1 {
		t.Fatalf("expected at least one corroboration, got report=%+v", report)
	}
	if report.Novel != 0 {
		t.Fatalf("restatement of an accepted record minted %d novel candidate(s); want 0", report.Novel)
	}
	if n := decisionCandidateCount(t, dir); n != 0 {
		t.Fatalf("restatement minted %d decision candidate file(s); want 0 (corroboration only)", n)
	}

	// Idempotent replay: running again must not mint duplicates.
	report2, err := harvest.Run(store, dir, harvest.Options{TranscriptPath: path, Now: time.Unix(1_700_000_300, 0).UTC()})
	if err != nil {
		t.Fatalf("harvest re-run: %v", err)
	}
	if report2.Novel != 0 || decisionCandidateCount(t, dir) != 0 {
		t.Fatalf("re-run minted duplicate candidates: novel=%d files=%d", report2.Novel, decisionCandidateCount(t, dir))
	}
}

// Falsifier (e): a novel ruling lands as a proposed-only decision candidate and
// CANNOT reach an accepted decision claim without operator ratification.
func TestRun_NovelLandsProposedOnly(t *testing.T) {
	dir, store := mustHome(t)

	acceptedBefore := countAcceptedDecisions(t, store)

	path := writeTranscript(t, dir,
		"From now on all execution must be routed to Codex; premium subscription lanes never execute, they only judge.",
	)
	report, err := harvest.Run(store, dir, harvest.Options{TranscriptPath: path, Now: time.Unix(1_700_000_400, 0).UTC()})
	if err != nil {
		t.Fatalf("harvest run: %v", err)
	}
	if report.Novel != 1 {
		t.Fatalf("novel ruling produced %d novel candidate(s), want 1 (report=%+v)", report.Novel, report)
	}
	if n := decisionCandidateCount(t, dir); n != 1 {
		t.Fatalf("novel ruling wrote %d decision candidate file(s), want 1", n)
	}
	// The candidate is proposed-only: verdict park, and no accepted claim minted.
	entries, _ := os.ReadDir(filepath.Join(dir, candidates.InboxDir(candidates.Decision)))
	var rec struct {
		Kind    string `json:"kind"`
		Verdict string `json:"verdict"`
	}
	if err := candidates.ReadCandidate(dir, candidates.Decision, strings.TrimSuffix(entries[0].Name(), ".json"), &rec); err != nil {
		t.Fatalf("read decision candidate: %v", err)
	}
	if rec.Kind != string(candidates.Decision) || rec.Verdict != candidates.Park {
		t.Fatalf("harvested candidate is not proposed-only: kind=%q verdict=%q", rec.Kind, rec.Verdict)
	}
	if got := countAcceptedDecisions(t, store); got != acceptedBefore {
		t.Fatalf("harvest changed accepted decision count %d -> %d; must not auto-accept", acceptedBefore, got)
	}
}

func countAcceptedDecisions(t *testing.T, store *mem.Store) int {
	t.Helper()
	claims, err := store.AllCurrent()
	if err != nil {
		t.Fatalf("all current: %v", err)
	}
	return len(decisions.AllActiveDecisions(claims))
}

// TestHarvestConflictOutcomeMatches pins decisions.HarvestConflictOutcome (the
// literal the standing-law view filters parked candidates on) to the harvester's
// own OutcomeConflict value. They live in two packages because harvest imports
// decisions (a cycle blocks the reverse import); if a rename split them, the
// console's contradiction strip would silently go empty — this reds first.
func TestHarvestConflictOutcomeMatches(t *testing.T) {
	if decisions.HarvestConflictOutcome != string(harvest.OutcomeConflict) {
		t.Fatalf("standing-law conflict filter %q != harvest.OutcomeConflict %q — the contradiction strip would silently empty",
			decisions.HarvestConflictOutcome, string(harvest.OutcomeConflict))
	}
}
