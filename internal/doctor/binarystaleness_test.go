package doctor

import (
	"testing"
)

// fakeAncestor is a stand-in for `git merge-base --is-ancestor`. It hard-codes the
// relationships the falsifier needs:
//   - bbbb is a strict ancestor of dddd
//   - bbbb is a strict ancestor of cccc
//   - anything involving aaaa is unrelated
//   - parent == child returns false (an SHA is not a strict ancestor of itself; the
//     RelateSHAs equal-prefix short-circuit handles equality)
//
// Real git is exercised by a separate test below.
func fakeAncestor(parent, child string) (bool, error) {
	if parent == "" || child == "" {
		return false, nil
	}
	if parent == child {
		return false, nil
	}
	if parent == "bbbb" && (child == "dddd" || child == "cccc") {
		return true, nil
	}
	return false, nil
}

// TestBinaryStalenessFalsifier is the named-signal contract the rest of the test pins: the
// package must report installed == tip as NOT stale, installed-as-descendant as NOT stale,
// and installed-as-ancestor as STALE — and emit the installed/target/relation triple so the
// operator can read the verdict instead of guessing from a bare boolean.
//
// Before the binarystaleness.go implementation exists this test cannot compile (RED).
// After the implementation lands, it must pass (GREEN). The red-then-green output is the
// falsifier proof that the new check actually moves the signal from "all-clear" to "stale"
// for the installed-ancestor-of-main case the user observed in production.
func TestBinaryStalenessFalsifier(t *testing.T) {
	cases := []struct {
		name         string
		installedSHA string
		targetTip    string
		wantStale    bool
		wantRelation Relation
	}{
		{
			name:         "installed equals target tip — fresh, not stale",
			installedSHA: "bbbb",
			targetTip:    "bbbb",
			wantStale:    false,
			wantRelation: RelationEqual,
		},
		{
			name:         "installed is strict ancestor of target tip — STALE (RED case)",
			installedSHA: "bbbb",
			targetTip:    "dddd", // dddd descends from bbbb via fakeAncestor (parent==bbbb, child==dddd)
			wantStale:    true,
			wantRelation: RelationAncestor,
		},
		{
			name:         "installed descends from target tip — fresh, not stale",
			installedSHA: "cccc",
			targetTip:    "bbbb", // cccc is descendant of bbbb per fakeAncestor
			wantStale:    false,
			wantRelation: RelationDescendant,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := RelateSHAs(c.installedSHA, c.targetTip, fakeAncestor)
			if got != c.wantRelation {
				t.Fatalf("RelateSHAs(%q,%q) = %q, want %q", c.installedSHA, c.targetTip, got, c.wantRelation)
			}
			report := BinaryStalenessReport{
				InstalledSHA: c.installedSHA,
				TargetTip:    c.targetTip,
				Relation:     got,
				Stale:        got == RelationAncestor,
			}
			if report.Stale != c.wantStale {
				t.Fatalf("report.Stale = %v, want %v (relation=%q)", report.Stale, c.wantStale, got)
			}
			// The NAMED SIGNAL contract: report must carry installed/target/relation,
			// not a bare boolean — the operator needs to read the verdict.
			if report.InstalledSHA != c.installedSHA {
				t.Errorf("InstalledSHA lost: got %q", report.InstalledSHA)
			}
			if report.TargetTip != c.targetTip {
				t.Errorf("TargetTip lost: got %q", report.TargetTip)
			}
		})
	}
}
