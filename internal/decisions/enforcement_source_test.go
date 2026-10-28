package decisions

import (
	"os"
	"strings"
	"testing"
)

// TestGrantFreezeEnforcementReadsOnlyDecisionRecords is falsifier e
// (grants-as-gates, 2026-07-10): "the enforcement path reads ONLY decision
// records — deleting the worklog/brain prose changes no gate behavior
// (prose = evidence, not authority)." A grep-shaped test is acceptable per
// the packet's house rule if honest: this reads the ACTUAL enforcement
// source files (director's grant resolution, work's freeze admission guard)
// and asserts they never open a markdown/worklog/brain path — their only
// inputs are mem.Store claims (AllCurrent/Capture/Adjudicate) and the
// envelope/policy shapes those claims project into.
//
// It deliberately does NOT scan internal/decisions itself: ImportPolicyDecision
// legitimately reads the markdown `compound decisions record` just wrote —
// that's the designed import path (markdown -> evidence -> claim), not the
// enforcement path this falsifier is about. The enforcement path is what
// director/grant.go and work's freeze guard read to DECIDE whether to
// admit/park a WorkEnvelope, and that is mem.Store claims only.
func TestGrantFreezeEnforcementReadsOnlyDecisionRecords(t *testing.T) {
	enforcementFiles := []string{
		"../director/grant.go",
		"../work/auto_admit.go",
	}
	// Function-call substrings that would indicate the enforcement path fell
	// back to direct file I/O instead of reading mem.Store claims. Doc
	// comments are allowed to MENTION markdown/worklog/brain prose (e.g. to
	// explain why the code deliberately avoids it, as grant.go and
	// auto_admit.go both do) — what matters is that no line actually OPENS
	// one of those paths.
	forbiddenCalls := []string{
		`os.ReadFile(`,
		`os.Open(`,
		`ioutil.ReadFile(`,
		`ioutil.ReadDir(`,
		`os.ReadDir(`,
	}
	for _, rel := range enforcementFiles {
		body, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("read enforcement source %s: %v", rel, err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue // doc comments may explain the boundary in prose
			}
			for _, call := range forbiddenCalls {
				if strings.Contains(line, call) {
					t.Fatalf("%s calls %s directly — the enforcement path must read ONLY mem.Store claims (AllCurrent), never a file on disk:\n%s", rel, call, line)
				}
			}
		}
	}
}
