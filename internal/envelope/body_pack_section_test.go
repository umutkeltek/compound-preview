package envelope

import (
	"encoding/json"
	"strings"
	"testing"
)

// The body_pack section is additive and optional: a pack WITHOUT it stays valid and
// never serializes a "body_pack" key (the additive precedent — like peers /
// task_contract / active_positions), and a pack WITH it validates and round-trips
// losslessly through the typed spine. This is the envelope-side half of falsifier F4
// (the contract/CLI half lives in internal/contract and cmd/compound).
func TestContextPackBodyPackSectionIsAdditiveAndRoundTrips(t *testing.T) {
	base := func() ContextPack {
		return ContextPack{
			Header:             hdr(SVContextPack, "ctx_bodypack_roundtrip"),
			SummaryMD:          "additive-field check",
			Included:           []IncludedRef{},
			Excluded:           []ExcludedRef{},
			ActiveInstructions: []string{},
			ActiveHooks:        []string{},
			ActiveDecisions:    []string{},
			ActiveMemory:       []string{},
			SourceRevisions:    []SourceRev{},
			RequiredChecks:     []string{},
			Avoid:              []string{},
		}
	}

	// Absent body_pack: valid, and no "body_pack" key in the JSON (omitempty on the pointer).
	without := base()
	if err := without.Validate(); err != nil {
		t.Fatalf("pack without body_pack invalid: %v", err)
	}
	data, err := json.Marshal(&without)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"body_pack":`) {
		t.Fatalf("pack without body_pack unexpectedly serialized a body_pack key: %s", data)
	}

	// Present body_pack: validates and round-trips (all fields survive).
	with := base()
	with.BodyPack = &BodyPackSection{
		BudgetTokens:        2000,
		EstimatedUsedTokens: 8,
		Bodies: []BodyPackItem{{
			Ref:             "claim_body_one",
			Source:          "corpus:personal-corpus",
			Rank:            0.92,
			EstimatedTokens: 8,
			SHA256:          "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			Status:          "ok",
			Text:            "small body",
		}},
		Omitted: []BodyPackOmission{{
			Ref:             "claim_too_big",
			Rank:            0.40,
			EstimatedTokens: 9000,
			Reason:          "budget_exhausted",
		}},
	}
	if err := with.Validate(); err != nil {
		t.Fatalf("pack with body_pack invalid: %v", err)
	}
	if err := RoundTrip(&with); err != nil {
		t.Fatalf("pack with body_pack round-trip: %v", err)
	}
	roundData, err := json.Marshal(&with)
	if err != nil {
		t.Fatal(err)
	}
	var back ContextPack
	if err := json.Unmarshal(roundData, &back); err != nil {
		t.Fatal(err)
	}
	if back.BodyPack == nil {
		t.Fatalf("body_pack did not survive round-trip: nil")
	}
	if back.BodyPack.BudgetTokens != 2000 || back.BodyPack.EstimatedUsedTokens != 8 {
		t.Fatalf("budget counters lost in round-trip: %+v", back.BodyPack)
	}
	if len(back.BodyPack.Bodies) != 1 || back.BodyPack.Bodies[0].Ref != "claim_body_one" {
		t.Fatalf("body item lost in round-trip: %+v", back.BodyPack.Bodies)
	}
	if len(back.BodyPack.Omitted) != 1 || back.BodyPack.Omitted[0].Reason != "budget_exhausted" {
		t.Fatalf("omission lost in round-trip: %+v", back.BodyPack.Omitted)
	}
}
