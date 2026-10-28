package tooltopology

import (
	"os"
	"strings"
	"testing"

	"compound/internal/capgraph"
)

// Falsifier 9: the harness-canon-file source class is a watched 14-day freshness policy whose
// refresh command carries no personal path or operator identity.
func TestHarnessCanonFreshnessPolicyWatched(t *testing.T) {
	maxAgeHours, refresh, watched := sourceFreshnessPolicy(capgraph.SourceAuthorityRecord{SourceClass: "harness-canon-file"})
	if !watched {
		t.Fatalf("harness-canon-file must be watched")
	}
	if maxAgeHours != 14*24 {
		t.Fatalf("harness-canon-file max age = %dh, want %dh", maxAgeHours, 14*24)
	}
	if strings.TrimSpace(refresh) == "" {
		t.Fatalf("harness-canon-file refresh command must be non-empty")
	}
	forbidden := []string{"/Users/", "/home/", ".claude/settings"}
	if u := strings.TrimSpace(os.Getenv("USER")); len(u) >= 4 {
		forbidden = append(forbidden, u)
	}
	for _, frag := range forbidden {
		if strings.Contains(refresh, frag) {
			t.Fatalf("refresh command leaks personal fragment %q: %q", frag, refresh)
		}
	}
}
