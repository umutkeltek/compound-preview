package executewire

import (
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/work"
)

func TestValidateSavedSelectionTupleRefusesAnySubstitution(t *testing.T) {
	base := envelope.WorkEnvelope{ExecutionSelection: &envelope.ExecutionSelection{
		LaunchProfileID: "claude-minimax", Harness: "claude_minimax",
		ProviderRoute: "minimax/MiniMax-M3", RequestedModelID: "MiniMax-M3",
		Source: "explicit", Revision: 1, SelectedBy: "operator", SelectedAt: "2026-09-22T00:00:00Z",
	}}
	cases := []struct {
		name  string
		lane  string
		model string
		route string
		want  string
	}{
		{"profile", "claude-zai", "MiniMax-M3", "minimax/MiniMax-M3", "launch profile"},
		{"model", "claude-minimax", "other", "minimax/MiniMax-M3", "model"},
		{"route", "claude-minimax", "MiniMax-M3", "other", "provider route"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSavedSelectionTuple(base, work.ExecutionRouteCandidate{
				LaunchProfileID: tc.lane, Harness: base.ExecutionSelection.Harness,
				RequestedModelID: tc.model, ProviderRoute: tc.route,
			}, time.Time{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateSavedSelectionTuple error = %v, want %q", err, tc.want)
			}
		})
	}
}
