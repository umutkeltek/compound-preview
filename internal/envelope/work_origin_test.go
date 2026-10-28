package envelope

import (
	"strings"
	"testing"
)

func TestWorkOriginIsOptionalForLegacyAndCompleteWhenPresent(t *testing.T) {
	legacy := WorkEnvelope{
		Header:             hdr(SVWork, "work_origin_legacy"),
		Owner:              "legacy",
		State:              "ready",
		SuccessPredicate:   "legacy work remains readable",
		NonGoals:           []string{},
		ActionIntentIDs:    []string{},
		ArtifactIDs:        []string{},
		VerificationIDs:    []string{},
		MemoryCandidateIDs: []string{},
	}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy work without origin must remain valid: %v", err)
	}

	current := legacy
	current.ID = "work_origin_current"
	current.Origin = &WorkOrigin{
		AccountableOwner: "compound:memory-maintenance",
		Actor:            "codex:release-lane",
		Harness:          "codex",
		RequestedModel:   "gpt-5.6-sol",
		LaneID:           "release-lane",
		IdentitySource:   "explicit_or_environment",
	}
	if err := current.Validate(); err != nil {
		t.Fatalf("complete origin rejected: %v", err)
	}

	current.Origin.RequestedModel = ""
	if err := current.Validate(); err == nil || !strings.Contains(err.Error(), "origin.requested_model") {
		t.Fatalf("incomplete origin must name requested_model, got %v", err)
	}
}
