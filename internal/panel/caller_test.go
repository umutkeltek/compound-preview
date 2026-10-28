package panel

import (
	"context"
	"strings"
	"testing"
)

func TestFixtureCallerHitAndMiss(t *testing.T) {
	caller := FixtureCaller{Dir: "../../fixtures/panel"}
	targetRef := "mcand_example_api_clean"

	got, err := caller.Call(context.Background(), CallRequest{
		LensID:    "lens:duplicate-risk",
		TargetRef: targetRef,
		User:      "target_ref: " + targetRef,
	})
	if err != nil {
		t.Fatalf("fixture lens call: %v", err)
	}
	if !strings.Contains(string(got.Content), "No near-duplicate") {
		t.Fatalf("unexpected lens content: %s", got.Content)
	}
	if got.TotalTokens == 0 {
		t.Fatalf("total tokens not populated: %+v", got)
	}

	judge, err := caller.Call(context.Background(), CallRequest{
		LensID:    "judge",
		TargetRef: targetRef,
	})
	if err != nil {
		t.Fatalf("fixture judge call: %v", err)
	}
	if !strings.Contains(string(judge.Content), `"decision": "promote"`) {
		t.Fatalf("unexpected judge content: %s", judge.Content)
	}

	if _, err := caller.Call(context.Background(), CallRequest{
		LensID:    "lens:not-present",
		TargetRef: targetRef,
	}); err == nil {
		t.Fatalf("missing fixture response returned nil error")
	}
}
