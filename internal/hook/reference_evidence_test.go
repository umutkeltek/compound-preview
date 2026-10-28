package hook

import (
	"testing"

	"compound/internal/envelope"
)

func TestTranscriptReferenceIsNotApplication(t *testing.T) {
	deps, home, store := consumptionEnv(t)
	const ref = "claim://claim_reference_only01"
	writeConsumePack(t, home, "ctx_reference_only", ref)
	wid := closeableWork(t, store, "ctx_reference_only")
	path := writeTranscript(t, home, `{"role":"assistant","content":"The brief cites claim_reference_only01, but that claim is wrong; do not apply it."}`+"\n")
	res, err := Run(Fixture{Hook: "session-end", Harness: "claude_code", WorkID: wid, MoveType: "outward_delta", Outcome: "completed", Summary: "shipped", TranscriptPath: path}, deps, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	if containsString(rec.Skills.Used, ref) {
		t.Fatalf("refutation credited as use: %+v", rec.Skills)
	}
	if !containsString(rec.Skills.Referenced, ref) {
		t.Fatalf("reference evidence missing: %+v", rec.Skills)
	}
	if rec.Skills.UsedDeclared {
		t.Fatalf("reference scanner cannot declare application: %+v", rec.Skills)
	}
}

func TestReadableNoMatchRemainsUnknown(t *testing.T) {
	deps, home, store := consumptionEnv(t)
	const ref = "claim://claim_nomatch01"
	writeConsumePack(t, home, "ctx_nomatch", ref)
	wid := closeableWork(t, store, "ctx_nomatch")
	path := writeTranscript(t, home, `{"role":"assistant","content":"Finished unrelated work."}`+"\n")
	res, err := Run(Fixture{Hook: "session-end", Harness: "claude_code", WorkID: wid, MoveType: "outward_delta", Outcome: "completed", Summary: "shipped", TranscriptPath: path}, deps, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	if rec.Skills.UsedDeclared || len(rec.Skills.ExplicitNonUse) != 0 {
		t.Fatalf("silence scored as non-use: %+v", rec.Skills)
	}
	if rec.Skills.ObservationQuality != "readable" {
		t.Fatalf("readable transcript quality lost: %+v", rec.Skills)
	}
}
