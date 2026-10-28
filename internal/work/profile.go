package work

import (
	"fmt"
	"io"
	"os"
	"time"
)

// ProfileLog returns a stage-timing logger gated by an env var — the
// process-local profiling seam for the workbench latency work (WB-PROFILE
// instrumentation). When the env var is not "1" the returned function is a
// no-op closure (zero cost in production: one env read at construction).
// Callers pass a fresh t0 per report build; each call logs stage delta +
// cumulative time to w (typically os.Stderr).
func ProfileLog(envKey, prefix string, w io.Writer, t0 time.Time) func(label string) {
	if os.Getenv(envKey) != "1" {
		return func(string) {}
	}
	return func(label string) {
		now := time.Now()
		fmt.Fprintf(w, "[%s] %s: %s (cum=%s)\n", prefix, label, now.Sub(t0), now.Sub(t0))
	}
}
