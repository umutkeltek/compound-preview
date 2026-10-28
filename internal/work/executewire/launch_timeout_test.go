package executewire

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"compound/internal/work"
)

// The governed door's execution budget is caller-requested, not fixed. Before
// this seam, LaunchTimeout computed a ceiling and then returned the 5-minute
// default unconditionally, so every governed lane (and the 45-minute detached
// door) died at exactly 5m0s. These falsifiers pin the resolution contract:
// absent/zero keeps the finite default, a request is honoured, a request above
// the ceiling is capped, and a malformed or negative request is refused.

func TestLaunchTimeoutForHonoursRequestedBudgetWithinCeiling(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested time.Duration
		want      time.Duration
	}{
		{"absent keeps default", 0, DefaultLaunchTimeout},
		{"negative keeps default", -time.Minute, DefaultLaunchTimeout},
		{"below default is honoured", 90 * time.Second, 90 * time.Second},
		{"above default is honoured", 40 * time.Minute, 40 * time.Minute},
		{"exact ceiling is honoured", MaxLaunchTimeout, MaxLaunchTimeout},
		{"above ceiling is capped", 3 * time.Hour, MaxLaunchTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LaunchTimeoutFor(tc.requested); got != tc.want {
				t.Fatalf("LaunchTimeoutFor(%v) = %v, want %v", tc.requested, got, tc.want)
			}
		})
	}
	if DefaultLaunchTimeout != 5*time.Minute {
		t.Fatalf("DefaultLaunchTimeout = %v; the unrequested budget stays 5m", DefaultLaunchTimeout)
	}
	if MaxLaunchTimeout != DefaultDetachedLaunchBudget {
		t.Fatalf("MaxLaunchTimeout = %v, want the detached door budget %v", MaxLaunchTimeout, DefaultDetachedLaunchBudget)
	}
}

func TestParseLaunchTimeoutFlag(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"", DefaultLaunchTimeout},
		{"0", DefaultLaunchTimeout},
		{"0s", DefaultLaunchTimeout},
		{"40m", 40 * time.Minute},
		{" 12m30s ", 12*time.Minute + 30*time.Second},
		{"2h", MaxLaunchTimeout},
	} {
		got, err := ParseLaunchTimeoutFlag(tc.raw)
		if err != nil || got != tc.want {
			t.Fatalf("ParseLaunchTimeoutFlag(%q) = %v, %v; want %v", tc.raw, got, err, tc.want)
		}
	}
	for _, raw := range []string{"-5m", "forty", "40", "5 minutes"} {
		if got, err := ParseLaunchTimeoutFlag(raw); err == nil {
			t.Fatalf("ParseLaunchTimeoutFlag(%q) = %v, nil; want a usage refusal", raw, got)
		} else if !strings.Contains(err.Error(), "--timeout") {
			t.Fatalf("ParseLaunchTimeoutFlag(%q) error %q does not name the flag", raw, err)
		}
	}
}

// The detached door is wrapped in GNU timeout at its budget. The door inside
// must carry its own shorter --timeout so the adapter deadline fires first and
// the door finalizes the RunAttempt before the wrapper TERMs it; without the
// flag the door fell back to the 5-minute default inside a 45-minute wrapper.
func TestDetachedLaunchArgvCarriesInnerTimeoutBelowWrapperBudget(t *testing.T) {
	for _, budget := range []time.Duration{DefaultDetachedLaunchBudget, 30 * time.Second, 3 * time.Minute} {
		argv := DetachedCandidateLaunchArgv("/bin/timeout", "/bin/compound", budget, "work_x", work.ExecutionRouteCandidate{LaunchProfileID: "claude_minimax", Harness: "claude_minimax"}, "p", "actor")
		outer, err := strconv.Atoi(argv[3])
		if err != nil || time.Duration(outer)*time.Second != budget {
			t.Fatalf("budget %v: wrapper seconds = %q, want %d", budget, argv[3], int(budget.Seconds()))
		}
		raw := ""
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "--timeout" {
				raw = argv[i+1]
			}
		}
		if raw == "" {
			t.Fatalf("budget %v: detached door argv carries no --timeout: %q", budget, argv)
		}
		inner, err := ParseLaunchTimeoutFlag(raw)
		if err != nil {
			t.Fatalf("budget %v: inner --timeout %q does not parse: %v", budget, raw, err)
		}
		if inner <= 0 || inner >= budget {
			t.Fatalf("budget %v: inner --timeout %v must be positive and shorter than the wrapper", budget, inner)
		}
		if budget == DefaultDetachedLaunchBudget && inner != budget-2*time.Minute {
			t.Fatalf("production detached door inner --timeout = %v, want %v (2m finalize margin)", inner, budget-2*time.Minute)
		}
	}
}
