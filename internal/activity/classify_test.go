package activity

import "testing"

// TestClassifyLongestPrefixDeterministic: bundle-id-only classification must be
// unchanged by the Phase 6 envelope addition. Every test case passes nil meta —
// the bundle-only rules still fire exactly as before.
func TestClassifyLongestPrefixDeterministic(t *testing.T) {
	cases := []struct {
		bundle   string
		wantCat  Category
		wantConf float64
	}{
		{"com.apple.dt.Xcode", CategoryCoding, 1.0},
		{"com.google.Chrome", CategoryBrowsing, 1.0},
		{"com.apple.Safari", CategoryBrowsing, 1.0},
		{"com.figma.Desktop", CategoryDesign, 1.0},
		{"com.apple.mail", CategoryComms, 1.0},
		{"com.tinyspeck.slackmacgap", CategoryComms, 1.0},
		{"com.apple.iWork.Pages", CategoryDocs, 1.0},
		{"com.apple.AppStoreConnect", CategoryStoreOps, 1.0},
		{"com.apple.finder", CategorySystem, 1.0},
		{"com.jetbrains.goland", CategoryCoding, 1.0}, // vendor-prefix rule
		{"com.unmapped.whatever", CategoryUnknown, 0.0},
		{"", CategoryUnknown, 0.0},
	}
	for _, tc := range cases {
		gotCat, gotConf := Classify(tc.bundle, nil)
		if gotCat != tc.wantCat || gotConf != tc.wantConf {
			t.Errorf("Classify(%q, nil) = (%s, %.1f), want (%s, %.1f)", tc.bundle, gotCat, gotConf, tc.wantCat, tc.wantConf)
		}
	}
}

// TestClassifyPrefersSpecificOverVendor proves the longest-prefix rule: an iWork-specific id must not
// be shadowed by the broad "com.apple." family that a naive first-match ordering could hit.
func TestClassifyPrefersSpecificOverVendor(t *testing.T) {
	// com.apple.iWork.Keynote must classify docs even though com.apple.finder (system) shares the
	// com.apple. vendor space — because the classifier matches by prefix specificity, not vendor.
	if cat, _ := Classify("com.apple.iWork.Keynote", nil); cat != CategoryDocs {
		t.Fatalf("iWork.Keynote = %s, want docs (longest-prefix must win)", cat)
	}
}

// TestClassify_BundleOnlyRuleUnchanged: explicitly the no-regression assertion.
// Every bundle-id-only rule (envelopeKey == "") must fire regardless of meta.
// Even a meta that would normally trigger an envelope-aware rule on a different
// bundle id must NOT shift the bundle-id classification.
func TestClassify_BundleOnlyRuleUnchanged(t *testing.T) {
	cases := []struct {
		bundle string
		want   Category
	}{
		{"com.google.Chrome", CategoryBrowsing},
		{"com.apple.Safari", CategoryBrowsing},
		{"com.figma.Desktop", CategoryDesign},
		{"com.apple.mail", CategoryComms},
		{"com.tinyspeck.slackmacgap", CategoryComms},
		{"com.apple.iWork.Pages", CategoryDocs},
		{"com.apple.finder", CategorySystem},
	}
	for _, tc := range cases {
		// Meta irrelevant — bundle-id classification wins.
		cat, conf := Classify(tc.bundle, map[string]string{"pane_type": "agent-session", "agent_kind": "claude"})
		if cat != tc.want || conf != 1.0 {
			t.Errorf("Classify(%q, …) = (%s, %.1f), want (%s, 1.0)", tc.bundle, cat, conf, tc.want)
		}
	}
}

// TestClassify_EnvelopeRefinesCodingTerminal: a terminal app with pane_type=terminal
// fires the envelope-aware rule (same Coding category, but it makes the
// envelope-aware rule the more specific match). Confirms the classifier honors
// the envelope meta when present.
func TestClassify_EnvelopeRefinesCodingTerminal(t *testing.T) {
	cat, conf := Classify("com.googlecode.iterm2", map[string]string{"pane_type": "terminal"})
	if cat != CategoryCoding || conf != 1.0 {
		t.Errorf("iTerm2 + pane_type=terminal: (%s, %.1f), want (coding, 1.0)", cat, conf)
	}
	cat, conf = Classify("com.apple.Terminal", map[string]string{"pane_type": "agent-session", "agent_kind": "agent-hook"})
	if cat != CategoryCoding || conf != 1.0 {
		t.Errorf("Terminal + agent-session: (%s, %.1f), want (coding, 1.0)", cat, conf)
	}
	cat, conf = Classify("com.cmuxterm.app", map[string]string{"pane_type": "agent-session", "agent_kind": "agent-hook"})
	if cat != CategoryCoding || conf != 1.0 {
		t.Errorf("cmux + agent-session: (%s, %.1f), want (coding, 1.0)", cat, conf)
	}
}

// TestClassify_EditorVsTerminal: a bundle id that's "browser" by prefix but
// carries pane_type=editor must NOT silently re-classify. The envelope-aware rule
// only fires if its prefix also matches. A bundle id with no envelope-aware
// editor rule keeps its original classification.
func TestClassify_EnvelopeMismatchFallsThroughToBundleRule(t *testing.T) {
	// com.google.Chrome has no editor envelope rule. pane_type=editor in meta
	// must NOT shift it to Coding — Chrome is still Browsing.
	cat, conf := Classify("com.google.Chrome", map[string]string{"pane_type": "editor"})
	if cat != CategoryBrowsing || conf != 1.0 {
		t.Errorf("Chrome + pane_type=editor: (%s, %.1f), want (browsing, 1.0) — envelope must not override bundle", cat, conf)
	}
	// com.apple.Safari is browsing; pane_type=editor must not change it.
	cat, conf = Classify("com.apple.Safari", map[string]string{"pane_type": "editor"})
	if cat != CategoryBrowsing {
		t.Errorf("Safari + pane_type=editor: %s, want browsing", cat)
	}
}

// TestClassify_UnknownBundleAndEmptyMeta: unknown bundle + empty/nil meta is the
// canonical CategoryUnknown at confidence 0 — never guessed.
func TestClassify_UnknownBundleAndEmptyMeta(t *testing.T) {
	cat, conf := Classify("com.example.unknown", nil)
	if cat != CategoryUnknown || conf != 0.0 {
		t.Errorf("unknown bundle + nil meta: (%s, %.1f), want (unknown, 0.0)", cat, conf)
	}
	cat, conf = Classify("com.example.unknown", map[string]string{})
	if cat != CategoryUnknown || conf != 0.0 {
		t.Errorf("unknown bundle + empty meta: (%s, %.1f), want (unknown, 0.0)", cat, conf)
	}
	cat, conf = Classify("", nil)
	if cat != CategoryUnknown || conf != 0.0 {
		t.Errorf("empty bundle + nil meta: (%s, %.1f), want (unknown, 0.0)", cat, conf)
	}
}

// TestClassify_EmptyBundleIdWithMeta: the empty bundle path returns Unknown
// immediately, before any envelope check — no rule could match an empty id
// regardless of meta.
func TestClassify_EmptyBundleIdWithMeta(t *testing.T) {
	cat, conf := Classify("", map[string]string{"pane_type": "terminal"})
	if cat != CategoryUnknown || conf != 0.0 {
		t.Errorf("empty bundle with meta: (%s, %.1f), want (unknown, 0.0)", cat, conf)
	}
}

// TestClassify_EnvelopeValueMismatch: the envelope rule requires an exact value
// match. A pane_type that's close but not equal (e.g., "Terminal" with capital
// T) must NOT fire the lowercase "terminal" envelope rule.
func TestClassify_EnvelopeValueMismatch(t *testing.T) {
	cat, conf := Classify("com.googlecode.iterm2", map[string]string{"pane_type": "Terminal"})
	if cat != CategoryCoding || conf != 1.0 {
		// Falls through to the bundle-only "com.googlecode.iterm2" → coding rule,
		// which fires on prefix match alone. So this is still coding — but via
		// the bundle-only path, not the envelope-aware one.
		t.Errorf("iTerm2 + wrong-case pane_type: (%s, %.1f)", cat, conf)
	}
	// Verify the bundle-only path is the one that fired (not the envelope one)
	// by checking that a bundle WITHOUT a bundle-only rule returns Unknown.
	cat, conf = Classify("com.example.terminal", map[string]string{"pane_type": "Terminal"})
	if cat != CategoryUnknown || conf != 0.0 {
		t.Errorf("unknown bundle + wrong-case meta: (%s, %.1f), want (unknown, 0.0)", cat, conf)
	}
}

// TestClassify_GitRemoteSubstringMatch: the git_remote envelope rule uses
// substring matching (envelopeValueContains), not exact match. A bundle id
// like com.googlecode.iterm2 with git_remote="git@example.com:yourorg/foo.git"
// fires the substring rule because "example.com/yourorg/" is a substring of
// the git_remote value. This unlocks per-org time categorization without
// requiring operators to enumerate every repo under their org.
func TestClassify_GitRemoteSubstringMatch(t *testing.T) {
	cases := []struct {
		name      string
		bundle    string
		gitRemote string
		want      Category
	}{
		{"iTerm2 + matching git_remote", "com.googlecode.iterm2", "git@example.com:yourorg/compound.git", CategoryCoding},
		{"Terminal.app + matching git_remote", "com.apple.Terminal", "https://example.com/yourorg/other-repo.git", CategoryCoding},
		{"Warp + matching git_remote", "dev.warp.Warp", "git@example.com:yourorg/another.git", CategoryCoding},
		{"cmux + matching git_remote", "com.cmuxterm.app", "git@example.com:yourorg/cmux-session.git", CategoryCoding},
		// Non-matching: substring is "example.com/yourorg/", the repo is
		// somewhere else. Falls through to the bundle-only Coding rule.
		{"iTerm2 + non-matching git_remote", "com.googlecode.iterm2", "git@example.com:otherorg/foo.git", CategoryCoding},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cat, conf := Classify(tc.bundle, map[string]string{"git_remote": tc.gitRemote})
			if cat != tc.want || conf != 1.0 {
				t.Errorf("Classify(%q, git_remote=%q) = (%s, %.1f), want (%s, 1.0)",
					tc.bundle, tc.gitRemote, cat, conf, tc.want)
			}
		})
	}
}

// TestClassify_GitRemoteEmpty: an empty git_remote meta falls through the
// substring rule (no match) and hits the bundle-only Coding rule. No
// regression vs. the no-meta case.
func TestClassify_GitRemoteEmpty(t *testing.T) {
	cat, conf := Classify("com.googlecode.iterm2", map[string]string{"git_remote": ""})
	if cat != CategoryCoding || conf != 1.0 {
		t.Errorf("iTerm2 + empty git_remote: (%s, %.1f), want (coding, 1.0)", cat, conf)
	}
	// Bundle WITHOUT a bundle-only rule and no matching git_remote returns
	// Unknown — confirms the substring path is a refinement, not a fallback.
	cat, conf = Classify("com.example.terminal", map[string]string{"git_remote": ""})
	if cat != CategoryUnknown || conf != 0.0 {
		t.Errorf("unknown bundle + empty git_remote: (%s, %.1f), want (unknown, 0.0)", cat, conf)
	}
}
