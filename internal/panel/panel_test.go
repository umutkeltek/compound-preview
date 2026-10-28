package panel

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRunRoundTripDecodeValidate(t *testing.T) {
	run := validRun()

	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("validate run: %v", err)
	}
	if !reflect.DeepEqual(got, run) {
		t.Fatalf("decoded run mismatch:\ngot:  %+v\nwant: %+v", got, run)
	}
}

func TestDecodeRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	run := validRun()
	var raw map[string]any
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["unknown"] = true
	withUnknown, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(withUnknown); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v, want unknown field rejection", err)
	}

	trailing := append(data, []byte(` {}`)...)
	if _, err := Decode(trailing); err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("err = %v, want trailing JSON rejection", err)
	}
}

func TestRunValidateRejectsEnumViolations(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Run)
		want string
	}{
		{
			name: "status",
			edit: func(run *Run) { run.Status = "paused" },
			want: "status",
		},
		{
			name: "target kind",
			edit: func(run *Run) { run.TargetKind = "trace" },
			want: "target_kind",
		},
		{
			name: "lens finding status",
			edit: func(run *Run) { run.Findings[0].LensFindings[0].Status = "partial" },
			want: "lens_findings[0].status",
		},
		{
			name: "synthesis decision",
			edit: func(run *Run) { run.Findings[0].Synthesis.Decision = "defer" },
			want: "synthesis.decision",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := validRun()
			tt.edit(&run)
			err := run.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestStoreSaveLoadListRuns(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	first := validRun()
	second := validRun()
	second.ID = "prun_20260701_second"

	if err := store.SaveRun(first); err != nil {
		t.Fatalf("save first run: %v", err)
	}
	if err := store.SaveRun(second); err != nil {
		t.Fatalf("save second run: %v", err)
	}
	loaded, err := store.LoadRun(first.ID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if !reflect.DeepEqual(loaded, first) {
		t.Fatalf("loaded run mismatch:\ngot:  %+v\nwant: %+v", loaded, first)
	}
	runs, err := store.ListRuns()
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("len(runs) = %d, want 2", len(runs))
	}
	seen := map[string]bool{}
	for _, run := range runs {
		seen[run.ID] = true
	}
	if !seen[first.ID] || !seen[second.ID] {
		t.Fatalf("listed run ids = %+v, want %s and %s", seen, first.ID, second.ID)
	}
}

func validRun() Run {
	return Run{
		SchemaVersion: RunSchemaVersion,
		ID:            "prun_20260701_fixture",
		CreatedAt:     "2026-07-01T10:00:00Z",
		Status:        "verdicts_recorded",
		WorkID:        "work_fixture",
		TargetKind:    "memory_candidate",
		Targets: []Target{{
			Ref:         "mcand_fixture",
			Privacy:     "private",
			StateBefore: "parked",
		}},
		LensSet: LensSet{
			Included: []IncludedLens{{
				Ref:          "lens_risk",
				Reason:       "checks promotion risk",
				Weight:       1,
				PromptSHA256: strings.Repeat("a", 64),
			}},
			Excluded: []ExcludedLens{{
				Ref:    "lens_cost",
				Reason: "not relevant for this target",
			}},
		},
		Route: RoutePlan{
			AuthorityRef: "compound://registries/model-router",
			LensPlanID:   "route_lens_fixture",
			JudgePlanID:  "route_judge_fixture",
			JudgeLane: LaneRef{
				ModelID:  "fixture-judge",
				Tier:     "premium",
				GateRole: "judge",
			},
			OmittedLanes: []OmittedLane{{
				ModelID: "fixture-expensive",
				Reason:  "budget guard",
			}},
		},
		Budget: Budget{
			MaxItems:       2,
			MaxTotalTokens: 4000,
			MaxUSDMicros:   2500,
		},
		Findings: []TargetFinding{{
			TargetRef: "mcand_fixture",
			LensFindings: []LensFinding{{
				LensRef:          "lens_risk",
				ModelID:          "fixture-lens",
				LaneTier:         "free",
				RouteEvidenceRef: "route_fixture",
				Confidence:       0.82,
				LatencyMS:        125,
				Usage: Usage{
					TotalTokens: 321,
				},
				Findings: []Finding{{
					Claim:        "candidate has enough evidence to park",
					Severity:     "info",
					EvidenceRefs: []string{"evidence_fixture"},
				}},
				Status: "completed",
			}},
			Synthesis: Synthesis{
				SchemaVersion:         SynthesisSchemaVersion,
				Decision:              "park",
				Confidence:            0.77,
				Rationale:             "needs more evidence before promotion",
				Dissent:               []Dissent{{LensRef: "lens_risk", Position: "cautious", Note: "low sample size"}},
				GuardrailsApplied:     []string{"evidence_not_truth"},
				JudgeRouteEvidenceRef: "route_judge_fixture",
			},
			VerdictArtifact: "artifact_panel_verdict_fixture",
			Application: Application{
				Applied:           false,
				MappedVerdict:     "park",
				ActionIntentRef:   "act_fixture",
				PolicyDecisionRef: "pol_fixture",
				NoPromoteRef:      "nop_fixture",
				TransitionRef:     "tr_fixture",
			},
		}},
		CountsBefore: map[string]int{"parked": 1},
		CountsAfter:  map[string]int{"parked": 1},
		EvidenceRefs: []string{"evidence_fixture"},
	}
}
