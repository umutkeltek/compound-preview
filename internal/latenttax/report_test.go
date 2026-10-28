package latenttax

import (
	"strings"
	"testing"
)

// ceremonyRun builds a ceremony-tagged run (dose-0 filler so it never pollutes the
// dose>0 latent-tax pool or the gate/both binary pool).
func ceremonyRun(runID, workID, mode, verdict, receiptRef string) RunRecord {
	r := run(runID, "tc", "cer", ArmFiller, 0, workID, verdict, receiptRef)
	r.CeremonyMode = mode
	return r
}

// seedReportHome fabricates the ledger for the four-arm + ceremony scenario used by
// the report tests. thrash comes from seeded PolicyDecision DENIALS; receipts back
// the cited-receipt traceability.
func seedReportHome(t *testing.T, home string) []RunRecord {
	t.Helper()
	// gate_only: 3 denials per work.
	for _, w := range []string{"work_gate1", "work_gate2"} {
		seedPolicy(t, home, "pol_"+w+"_a", w, "deny")
		seedPolicy(t, home, "pol_"+w+"_b", w, "deny")
		seedPolicy(t, home, "pol_"+w+"_c", w, "deny")
	}
	works := []string{
		"work_oic1", "work_oic2", "work_fill1", "work_fill2",
		"work_gate1", "work_gate2", "work_both1", "work_both2",
	}
	for _, w := range works {
		seedReceipt(t, home, "rec_"+w, w)
	}
	return []RunRecord{
		// obligation_in_context dose 20 — both FAIL (harm=1.0)
		run("r_oic1", "t1", "oic", ArmObligationInContext, 20, "work_oic1", VerdictFail, "rec_work_oic1"),
		run("r_oic2", "t1", "oic", ArmObligationInContext, 20, "work_oic2", VerdictFail, "rec_work_oic2"),
		// filler dose 20 — both PASS (harm=0.0)
		run("r_fill1", "t1", "fill", ArmFiller, 20, "work_fill1", VerdictPass, "rec_work_fill1"),
		run("r_fill2", "t1", "fill", ArmFiller, 20, "work_fill2", VerdictPass, "rec_work_fill2"),
		// gate_only — thrash 3, PASS
		run("r_gate1", "t1", "gate", ArmGateOnly, 20, "work_gate1", VerdictPass, "rec_work_gate1"),
		run("r_gate2", "t1", "gate", ArmGateOnly, 20, "work_gate2", VerdictPass, "rec_work_gate2"),
		// both — thrash 0, PASS
		run("r_both1", "t1", "both", ArmBoth, 20, "work_both1", VerdictPass, "rec_work_both1"),
		run("r_both2", "t1", "both", ArmBoth, 20, "work_both2", VerdictPass, "rec_work_both2"),
	}
}

// TestReportRendersThreePreRegisteredKillCriteria is the falsifier: on a synthetic
// home spanning all four arms (plus ceremony runs), the report renders per-arm
// distributions AND all three pre-registered kill-criteria with the correct verdicts.
func TestReportRendersThreePreRegisteredKillCriteria(t *testing.T) {
	home := t.TempDir()
	runs := seedReportHome(t, home)

	// ceremony runs: observer and in_band, all PASS -> equal downstream quality.
	seedReceipt(t, home, "rec_work_obs1", "work_obs1")
	seedReceipt(t, home, "rec_work_inb1", "work_inb1")
	runs = append(runs,
		ceremonyRun("r_obs1", "work_obs1", CeremonyObserver, VerdictPass, "rec_work_obs1"),
		ceremonyRun("r_obs2", "work_obs2", CeremonyObserver, VerdictPass, ""),
		ceremonyRun("r_inb1", "work_inb1", CeremonyInBand, VerdictPass, "rec_work_inb1"),
		ceremonyRun("r_inb2", "work_inb2", CeremonyInBand, VerdictPass, ""),
	)

	ts := TaskSet{TaskSetID: "ts-report"}
	reg := DefaultFalsifierRegistry()
	rep, err := BuildReport(ts, ArmConfig{ConfigID: "cfg-report"}, reg, runs, home, "testsha", "2026-07-13T00:00:00Z")
	if err != nil {
		t.Fatalf("build report: %v", err)
	}

	// Report identity + provenance.
	if rep.SchemaVersion != ReportSchemaVersion || rep.RegistryID != reg.RegistryID || rep.PreregisteredAt != reg.PreregisteredAt {
		t.Fatalf("report must echo registry provenance: %+v", rep)
	}
	if len(rep.Falsifiers) != 3 {
		t.Fatalf("report must render all three pre-registered criteria, got %d", len(rep.Falsifiers))
	}

	got := map[string]FalsifierResult{}
	for _, f := range rep.Falsifiers {
		got[f.ID] = f
	}
	if v := got["latent_tax_null"].Verdict; v != Survived {
		t.Fatalf("latent_tax_null: instructions degrade more than filler -> SURVIVED, got %s (%s)", v, got["latent_tax_null"].Detail)
	}
	if v := got["binary_placement"].Verdict; v != Killed {
		t.Fatalf("binary_placement: gate-only thrash 3 >> both 0 -> KILLED, got %s (%s)", v, got["binary_placement"].Detail)
	}
	if v := got["terminating_trajectory"].Verdict; v != Killed {
		t.Fatalf("terminating_trajectory: observer ~ in_band quality -> KILLED, got %s (%s)", v, got["terminating_trajectory"].Detail)
	}
	// Binary left/right are the actual thrash means.
	if got["binary_placement"].Left != 3 || got["binary_placement"].Right != 0 {
		t.Fatalf("binary left/right must be gate=3 both=0, got %v/%v", got["binary_placement"].Left, got["binary_placement"].Right)
	}

	// Adjudication is a REQUEST with no promotion path; it must cite run receipts.
	if !strings.Contains(rep.Adjudication.Note, "promotes nothing") {
		t.Fatalf("adjudication must declare it promotes nothing, got %q", rep.Adjudication.Note)
	}
	if len(rep.Adjudication.Proposed) != 3 {
		t.Fatalf("adjudication must propose a verdict per criterion, got %d", len(rep.Adjudication.Proposed))
	}
	if len(rep.Adjudication.CitedReceipts) == 0 {
		t.Fatal("adjudication must cite run receipts from the ledger")
	}
	for _, p := range rep.Adjudication.Proposed {
		if p.Criterion == "binary_placement" && !strings.Contains(p.Suggests, "BOTH") {
			t.Fatalf("binary_placement killed should suggest amending to BOTH, got %q", p.Suggests)
		}
	}

	// Counts: 12 dispatched runs, all graded, 0 pending.
	if rep.RunCount != 12 || rep.Dispatched != 12 || rep.Graded != 12 || rep.Pending != 0 {
		t.Fatalf("counts wrong: total=%d dispatched=%d graded=%d pending=%d", rep.RunCount, rep.Dispatched, rep.Graded, rep.Pending)
	}
}

// TestTerminatingTrajectoryInsufficientWithoutCeremony proves the observer criterion
// renders insufficient_data (not a silent pass) when no ceremony runs exist — the
// report still carries all three criteria.
func TestTerminatingTrajectoryInsufficientWithoutCeremony(t *testing.T) {
	home := t.TempDir()
	runs := seedReportHome(t, home) // no ceremony runs
	rep, err := BuildReport(TaskSet{TaskSetID: "ts"}, ArmConfig{}, DefaultFalsifierRegistry(), runs, home, "sha", "2026-07-13T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	var term FalsifierResult
	for _, f := range rep.Falsifiers {
		if f.ID == "terminating_trajectory" {
			term = f
		}
	}
	if term.Verdict != InsufficientData {
		t.Fatalf("without ceremony runs terminating_trajectory must be insufficient_data, got %s", term.Verdict)
	}
	if len(rep.Falsifiers) != 3 {
		t.Fatalf("all three criteria must still render, got %d", len(rep.Falsifiers))
	}
}

// TestLatentTaxNullKilledWhenFillerHarmsEqually proves the null fires: when filler
// degrades quality as much as instructions, the latent-tax framing is KILLED.
func TestLatentTaxNullKilledWhenFillerHarmsEqually(t *testing.T) {
	home := t.TempDir()
	// Both obligation_in_context and filler FAIL at dose 20 -> equal harm.
	runs := []RunRecord{
		run("r_oic1", "t", "oic", ArmObligationInContext, 20, "work_oic1", VerdictFail, ""),
		run("r_fill1", "t", "fill", ArmFiller, 20, "work_fill1", VerdictFail, ""),
	}
	rep, err := BuildReport(TaskSet{TaskSetID: "ts"}, ArmConfig{}, DefaultFalsifierRegistry(), runs, home, "sha", "2026-07-13T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.Falsifiers {
		if f.ID == "latent_tax_null" && f.Verdict != Killed {
			t.Fatalf("filler harming as much as instructions must KILL latent_tax, got %s (%s)", f.Verdict, f.Detail)
		}
	}
}

func TestFalsifierRegistryValidation(t *testing.T) {
	if err := DefaultFalsifierRegistry().Validate(); err != nil {
		t.Fatalf("default registry must validate, got %v", err)
	}
	bad := DefaultFalsifierRegistry()
	bad.Criteria[0].Comparison = "bogus"
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown comparison must fail registry validation")
	}
}
