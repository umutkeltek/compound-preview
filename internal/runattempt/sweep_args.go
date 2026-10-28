package runattempt

import (
	"fmt"
	"strings"
	"time"
)

// SweepArgs is the pure command grammar for stale RunAttempt recovery. It
// deliberately has no home or store dependency so a malformed mutating
// request fails before the CLI resolves or materializes runtime state.
type SweepArgs struct {
	AttemptID  string
	HasAttempt bool
	HomePath   string
	Threshold  time.Duration
	JSON       bool
}

// ParseSweepArgs accepts the run-attempt sweep flags and validates the exact
// target selector and stale threshold. It retains first-value semantics for
// repeatable --home and --threshold, while --attempt is intentionally unique.
func ParseSweepArgs(args []string) (SweepArgs, error) {
	parsed := SweepArgs{Threshold: DefaultSweepThreshold}
	var thresholdRaw string
	thresholdSet := false
	homeSet := false
	attemptCount := 0
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "--attempt", "--threshold", "--home":
			if index+1 >= len(args) {
				return SweepArgs{}, fmt.Errorf("%s requires a value", arg)
			}
			value := args[index+1]
			if strings.HasPrefix(value, "-") {
				return SweepArgs{}, fmt.Errorf("%s value got another flag %q", arg, value)
			}
			index++
			switch arg {
			case "--attempt":
				attemptCount++
				if attemptCount > 1 {
					return SweepArgs{}, fmt.Errorf("--attempt may be supplied only once")
				}
				parsed.HasAttempt = true
				parsed.AttemptID = value
			case "--threshold":
				if !thresholdSet {
					thresholdRaw = value
					thresholdSet = true
				}
			case "--home":
				if !homeSet {
					parsed.HomePath = value
					homeSet = true
				}
			}
		case "--json":
			parsed.JSON = true
		default:
			if strings.HasPrefix(arg, "-") {
				return SweepArgs{}, fmt.Errorf("unknown flag %s", arg)
			}
			return SweepArgs{}, fmt.Errorf("unexpected positional argument %q", arg)
		}
	}
	if parsed.HasAttempt && !Valid(parsed.AttemptID) {
		return SweepArgs{}, fmt.Errorf("invalid --attempt %q", parsed.AttemptID)
	}
	if !thresholdSet || thresholdRaw == "" {
		return parsed, nil
	}
	threshold, err := time.ParseDuration(thresholdRaw)
	if err != nil {
		return SweepArgs{}, fmt.Errorf("invalid threshold %q: %w", thresholdRaw, err)
	}
	if threshold <= 0 {
		return SweepArgs{}, fmt.Errorf("threshold must be greater than zero, got %q", thresholdRaw)
	}
	parsed.Threshold = threshold
	return parsed, nil
}
