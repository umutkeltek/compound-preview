package work

import (
	"strings"
	"testing"
	"time"
)

// The bounded execution budget is a governed option: a malformed, zero, or
// negative duration is refused, and the option never silently disappears.
func TestParseProofCheckExecutionTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		want    time.Duration
		wantErr bool
	}{
		{name: "omitted", args: []string{"--work", "work_exact", "--actor", "codex:alpha"}},
		{name: "space separated", args: []string{"--execution-timeout", "30m"}, want: 30 * time.Minute},
		{name: "inline", args: []string{"--execution-timeout=90s"}, want: 90 * time.Second},
		{name: "sub second", args: []string{"--execution-timeout", "250ms"}, want: 250 * time.Millisecond},
		{name: "malformed", args: []string{"--execution-timeout", "soon"}, wantErr: true},
		{name: "malformed unit", args: []string{"--execution-timeout", "30minutes"}, wantErr: true},
		{name: "zero", args: []string{"--execution-timeout", "0s"}, wantErr: true},
		{name: "zero bare", args: []string{"--execution-timeout", "0"}, wantErr: true},
		{name: "negative", args: []string{"--execution-timeout", "-1m"}, wantErr: true},
		{name: "missing value", args: []string{"--execution-timeout"}, wantErr: true},
		{name: "empty inline", args: []string{"--execution-timeout="}, wantErr: true},
		{name: "next flag as value", args: []string{"--execution-timeout", "--work"}, wantErr: true},
		{name: "duplicated", args: []string{"--execution-timeout", "1m", "--execution-timeout", "2m"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseProofCheckExecutionTimeout(tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseProofCheckExecutionTimeout(%v) error = %v, wantErr %v", tc.args, err, tc.wantErr)
			}
			if tc.wantErr {
				if err != nil && !strings.Contains(err.Error(), ProofCheckExecutionTimeoutFlag) {
					t.Fatalf("refusal %q does not name the option", err)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("budget = %v, want %v", got, tc.want)
			}
		})
	}
}

// A budget that bounds the canonical battery is meaningless in a mode that
// never runs it, and silently accepting it there would promise a bound that
// does not exist.
func TestParseProofCheckExecutionTimeoutRefusesNonCanonicalModes(t *testing.T) {
	for _, mode := range []string{"--dev", "--dev=true", "--dry-run", "--ci", "--ci-console", "--recover-legacy-retained", "--timings"} {
		for _, args := range [][]string{
			{"--execution-timeout", "5m", mode},
			{mode, "--execution-timeout", "5m"},
			{mode, "--execution-timeout=5m"},
		} {
			if got, err := ParseProofCheckExecutionTimeout(args); err == nil {
				t.Fatalf("%v admitted the bounded execution budget (%v)", args, got)
			}
		}
	}
	got, err := ParseProofCheckExecutionTimeout([]string{"--execution-timeout", "5m", "--work", "work_exact", "--actor", "codex:alpha"})
	if err != nil || got != 5*time.Minute {
		t.Fatalf("canonical lane refused the budget: %v %v", got, err)
	}
}

// ProofCheckLightweightMode is the single reading of which modes skip the
// canonical battery. It must agree with the route-option compatibility gate
// that already refuses canonical-only obligations.
func TestProofCheckLightweightModeMatchesRouteOptionCompatibility(t *testing.T) {
	for _, mode := range []string{"--dev", "--dev=true", "--dry-run", "--ci", "--ci-console", "--recover-legacy-retained", "--timings"} {
		if !ProofCheckLightweightMode([]string{mode}) {
			t.Fatalf("%s is not a lightweight mode", mode)
		}
		if _, err := ParseProofCheckRouteOptions([]string{"--remote-go-test", "route.json", mode}); err == nil {
			t.Fatalf("%s admitted a canonical-only obligation", mode)
		}
	}
	for _, args := range [][]string{
		{"--work", "work_exact"},
		{"--json"},
		{"--base", "main"},
		{"--remote-go-test", "route.json"},
		{"--execution-timeout", "5m"},
	} {
		if ProofCheckLightweightMode(args) {
			t.Fatalf("%v reported as a lightweight mode", args)
		}
	}
}
