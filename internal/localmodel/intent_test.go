package localmodel

import (
	"testing"
	"time"
)

func TestEvaluateAddsLifecycleIntentForRoutingActions(t *testing.T) {
	loaded := 8192
	report := Evaluate(map[string]Model{
		"judge": {ID: "judge", State: "loaded", MaxContextLength: 262144, LoadedContextLength: &loaded},
	}, []Requirement{{ModelID: "judge", MinContextTokens: 12000}}, Policy{ReserveTokens: 1000})

	if report.Status != "blocked" || len(report.Findings) != 1 {
		t.Fatalf("report = %+v, want one blocked finding", report)
	}
	intent := report.Findings[0].LifecycleIntent
	if intent == nil {
		t.Fatalf("lifecycle intent missing: %+v", report.Findings[0])
	}
	if intent.Owner != LifecycleIntentOwner ||
		intent.Action != ActionReloadHigherContext ||
		intent.ModelID != "judge" ||
		intent.ContextTokens != 13000 ||
		intent.MutationClass != LifecycleIntentMutationClass ||
		intent.Delegation != LifecycleIntentDelegation {
		t.Fatalf("intent = %+v, want local-routing reload intent", intent)
	}
}

func TestEvaluateDoesNotAddLifecycleIntentForAmbiguousMetadata(t *testing.T) {
	report := Evaluate(map[string]Model{
		"judge": {ID: "judge", State: "loaded", MaxContextLength: 262144},
	}, []Requirement{{ModelID: "judge"}}, Policy{})

	if report.Status != "blocked" || len(report.Findings) != 1 {
		t.Fatalf("report = %+v, want one blocked finding", report)
	}
	if report.Findings[0].Action != ActionUnknownLoadedContext {
		t.Fatalf("action = %q, want unknown loaded context", report.Findings[0].Action)
	}
	if report.Findings[0].LifecycleIntent != nil {
		t.Fatalf("intent = %+v, want nil for ambiguous metadata", report.Findings[0].LifecycleIntent)
	}
}

func TestEvaluateAddsUnloadIntentForIdleUnrequiredModel(t *testing.T) {
	loaded := 262144
	lastUsed := time.Date(2026, 7, 3, 8, 0, 0, 0, time.UTC)
	report := Evaluate(map[string]Model{
		"unused": {
			ID:                  "unused",
			State:               "loaded",
			MaxContextLength:    262144,
			LoadedContextLength: &loaded,
			LastUsedAt:          &lastUsed,
		},
	}, nil, Policy{IdleTTL: time.Hour, Now: lastUsed.Add(2 * time.Hour)})

	if report.Status != "actionable" || len(report.Findings) != 1 {
		t.Fatalf("report = %+v, want one actionable finding", report)
	}
	intent := report.Findings[0].LifecycleIntent
	if intent == nil || intent.Action != ActionUnloadIdle || intent.ModelID != "unused" || intent.ContextTokens != 0 {
		t.Fatalf("intent = %+v, want unload intent", intent)
	}
}
