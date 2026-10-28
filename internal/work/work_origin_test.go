package work

import (
	"testing"
)

func TestBuildWorkRecordsExplicitCreatorIdentity(t *testing.T) {
	t.Parallel()
	w := BuildWork(CreateInput{
		Owner:            "compound:memory-maintenance",
		Actor:            "codex:release-lane",
		CreatorHarness:   "codex",
		CreatorModel:     "gpt-5.6-sol",
		CreatorLaneID:    "release-lane",
		SuccessPredicate: "creator identity is explicit",
	}, fixedNow)

	if w.Origin == nil {
		t.Fatal("new work has no origin")
	}
	if w.Origin.AccountableOwner != "compound:memory-maintenance" || w.Origin.Actor != "codex:release-lane" {
		t.Fatalf("origin accountability = %+v", w.Origin)
	}
	if w.Origin.Harness != "codex" || w.Origin.RequestedModel != "gpt-5.6-sol" || w.Origin.LaneID != "release-lane" {
		t.Fatalf("origin execution identity = %+v", w.Origin)
	}
	if w.Origin.IdentitySource != "explicit_or_environment" {
		t.Fatalf("identity source = %q, want explicit_or_environment", w.Origin.IdentitySource)
	}
}

func TestBuildWorkDerivesSpecificOriginWithoutPrompting(t *testing.T) {
	t.Parallel()
	w := BuildWork(CreateInput{
		Owner:            "cmd/compound",
		Actor:            "orchestrator:claude_code",
		SuccessPredicate: "derived identity remains truthful",
	}, fixedNow)

	if w.Origin == nil {
		t.Fatal("new work has no derived origin")
	}
	if w.Origin.AccountableOwner != "surface:cmd-compound" {
		t.Fatalf("accountable owner = %q, want surface:cmd-compound", w.Origin.AccountableOwner)
	}
	if w.Origin.Actor != "orchestrator:claude_code" || w.Origin.Harness != "claude_code" {
		t.Fatalf("derived actor/harness = %+v", w.Origin)
	}
	if w.Origin.RequestedModel != "unknown" || w.Origin.LaneID != "orchestrator:claude_code" || w.Origin.IdentitySource != "derived" {
		t.Fatalf("derived provenance = %+v", w.Origin)
	}
}

func TestWorkbenchProjectsSpecificOriginBesideLegacyOwner(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	created, err := store.Create(CreateInput{
		Owner:            "claude",
		Actor:            "claude_code:memory-lane",
		CreatorHarness:   "claude_code",
		CreatorModel:     "claude-sonnet",
		CreatorLaneID:    "memory-lane",
		SuccessPredicate: "workbench shows exact ownership provenance",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	report, err := store.Workbench(fixedNow, WorkbenchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range report.Open {
		if item.WorkID != created.ID {
			continue
		}
		if item.Owner != "claude" || item.AccountableOwner != "harness:claude" ||
			item.CreatorHarness != "claude_code" || item.CreatorRequestedModel != "claude-sonnet" ||
			item.CreatorLaneID != "memory-lane" {
			t.Fatalf("workbench identity = %+v", item)
		}
		return
	}
	t.Fatalf("created work %s missing from workbench", created.ID)
}

func TestCreatePlanEqualityBindsOrigin(t *testing.T) {
	t.Parallel()
	first := BuildWork(CreateInput{
		Owner: "compound:memory", Actor: "codex:lane", CreatorHarness: "codex",
		CreatorModel: "model-a", CreatorLaneID: "lane", SuccessPredicate: "origin is bound",
	}, fixedNow)
	second := first
	origin := *second.Origin
	origin.RequestedModel = "model-b"
	second.Origin = &origin
	if sameCreatePlanWork(first, second) {
		t.Fatal("create-plan equality ignored creator model drift")
	}
}
