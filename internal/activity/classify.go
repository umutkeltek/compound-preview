package activity

import (
	"sort"
	"strings"
)

// categoryRule maps a bundle-id prefix (and optionally an envelope meta key/value) to a
// category. Longest matching prefix wins, so a specific id (com.apple.dt.Xcode) beats a
// broad vendor prefix (com.apple.).
//
// Envelope refinement has two modes, mutually exclusive per rule:
//
//   - envelopeKey + envelopeValue: exact match — fires when meta[envelopeKey] == envelopeValue.
//     Use for short, closed-set values like pane_type ("terminal" / "agent-session" / "editor").
//   - envelopeKey + envelopeValueContains: substring match — fires when
//     strings.Contains(meta[envelopeKey], envelopeValueContains). Use for unbounded values
//     like git_remote (e.g., "github.com/yourorg/" matches every repo under that org).
//
// Generic bundle ids only — no personal or private-tool identifiers (this file ships in
// the public core).
type categoryRule struct {
	prefix                string
	category              Category
	envelopeKey           string // empty = bundle-only rule (legacy behavior)
	envelopeValue         string // fires only when meta[envelopeKey] == envelopeValue (exact)
	envelopeValueContains string // fires only when meta[envelopeKey] contains this substring
}

// bundleCategoryRules is the deterministic, closed-taxonomy classifier. It is intentionally small and
// generic; an unmapped id resolves to CategoryUnknown at confidence 0 (never guessed).
//
// Phase 6 adds envelope-aware rules: rules with non-empty envelopeKey only fire when the
// Tier-1 envelope carries the matching meta. This unlocks per-project time
// ("com.example.terminal in compound repo" vs "com.example.terminal in another repo")
// without changing the bundle-id classification for Tier-0 events (which carry
// empty meta, so envelope-aware rules don't fire — only bundle-only rules do).
var bundleCategoryRules = []categoryRule{
	// coding — bundle-only (Tier-0 + Tier-1 events both classify correctly).
	{prefix: "com.apple.dt.Xcode", category: CategoryCoding, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.microsoft.VSCode", category: CategoryCoding, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.googlecode.iterm2", category: CategoryCoding, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.apple.Terminal", category: CategoryCoding, envelopeKey: "", envelopeValue: ""},
	{prefix: "dev.warp.Warp", category: CategoryCoding, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.jetbrains.", category: CategoryCoding, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.sublimetext.", category: CategoryCoding, envelopeKey: "", envelopeValue: ""},
	// Tier-1 envelope refinements: terminal/editor/agent-session apps keep the
	// bundle-only Coding classification but ALSO fire when the Tier-1 envelope
	// carries pane_type — making them explicit so future rules (e.g.,
	// sandbox → Research) can distinguish sub-types without re-classifying
	// bundle-id.
	{prefix: "com.googlecode.iterm2", category: CategoryCoding, envelopeKey: "pane_type", envelopeValue: "terminal"},
	{prefix: "com.googlecode.iterm2", category: CategoryCoding, envelopeKey: "pane_type", envelopeValue: "agent-session"},
	{prefix: "com.apple.Terminal", category: CategoryCoding, envelopeKey: "pane_type", envelopeValue: "terminal"},
	{prefix: "com.apple.Terminal", category: CategoryCoding, envelopeKey: "pane_type", envelopeValue: "agent-session"},
	{prefix: "dev.warp.Warp", category: CategoryCoding, envelopeKey: "pane_type", envelopeValue: "terminal"},
	{prefix: "dev.warp.Warp", category: CategoryCoding, envelopeKey: "pane_type", envelopeValue: "agent-session"},
	{prefix: "com.microsoft.VSCode", category: CategoryCoding, envelopeKey: "pane_type", envelopeValue: "editor"},
	{prefix: "com.todesktop.230313mzl4w4u96", category: CategoryCoding, envelopeKey: "pane_type", envelopeValue: "editor"},
	{prefix: "com.cmuxterm.app", category: CategoryCoding, envelopeKey: "pane_type", envelopeValue: "terminal"},
	{prefix: "com.cmuxterm.app", category: CategoryCoding, envelopeKey: "pane_type", envelopeValue: "agent-session"},
	// cmux bundle-only (Tier-0 events with empty meta still classify as Coding;
	// envelope-aware rules refine from there).
	{prefix: "com.cmuxterm.app", category: CategoryCoding, envelopeKey: "", envelopeValue: ""},
	// git_remote substring rules — sample public-safe placeholders. Operators
	// replace "example.com/yourorg/" with their actual org/repo paths in a
	// private overlay (this file ships in the public core, so no real orgs).
	// A rule of this shape fires when meta[git_remote] contains the substring;
	// empty git_remote or non-matching repos fall through to the bundle-only rule.
	{prefix: "com.googlecode.iterm2", category: CategoryCoding, envelopeKey: "git_remote", envelopeValue: "", envelopeValueContains: "example.com/yourorg/"},
	{prefix: "com.apple.Terminal", category: CategoryCoding, envelopeKey: "git_remote", envelopeValue: "", envelopeValueContains: "example.com/yourorg/"},
	{prefix: "dev.warp.Warp", category: CategoryCoding, envelopeKey: "git_remote", envelopeValue: "", envelopeValueContains: "example.com/yourorg/"},
	{prefix: "com.cmuxterm.app", category: CategoryCoding, envelopeKey: "git_remote", envelopeValue: "", envelopeValueContains: "example.com/yourorg/"},
	// browsing
	{prefix: "com.apple.Safari", category: CategoryBrowsing, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.google.Chrome", category: CategoryBrowsing, envelopeKey: "", envelopeValue: ""},
	{prefix: "org.mozilla.firefox", category: CategoryBrowsing, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.microsoft.edgemac", category: CategoryBrowsing, envelopeKey: "", envelopeValue: ""},
	{prefix: "company.thebrowser.Browser", category: CategoryBrowsing, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.brave.Browser", category: CategoryBrowsing, envelopeKey: "", envelopeValue: ""},
	// design
	{prefix: "com.figma.Desktop", category: CategoryDesign, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.bohemiancoding.sketch3", category: CategoryDesign, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.adobe.", category: CategoryDesign, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.pixelmatorteam.", category: CategoryDesign, envelopeKey: "", envelopeValue: ""},
	// comms
	{prefix: "com.apple.mail", category: CategoryComms, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.tinyspeck.slackmacgap", category: CategoryComms, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.hnc.Discord", category: CategoryComms, envelopeKey: "", envelopeValue: ""},
	{prefix: "ru.keepcoder.Telegram", category: CategoryComms, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.apple.iChat", category: CategoryComms, envelopeKey: "", envelopeValue: ""},
	{prefix: "us.zoom.xos", category: CategoryComms, envelopeKey: "", envelopeValue: ""},
	// store-ops
	{prefix: "com.apple.iTunesConnect", category: CategoryStoreOps, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.apple.AppStoreConnect", category: CategoryStoreOps, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.apple.itunesconnect.", category: CategoryStoreOps, envelopeKey: "", envelopeValue: ""},
	// docs
	{prefix: "com.apple.iWork.Pages", category: CategoryDocs, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.apple.iWork.Numbers", category: CategoryDocs, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.apple.iWork.Keynote", category: CategoryDocs, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.apple.iWork.", category: CategoryDocs, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.microsoft.Word", category: CategoryDocs, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.microsoft.Excel", category: CategoryDocs, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.microsoft.Powerpoint", category: CategoryDocs, envelopeKey: "", envelopeValue: ""},
	{prefix: "md.obsidian", category: CategoryDocs, envelopeKey: "", envelopeValue: ""},
	{prefix: "notion.id", category: CategoryDocs, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.apple.Notes", category: CategoryDocs, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.apple.Preview", category: CategoryDocs, envelopeKey: "", envelopeValue: ""},
	// media
	{prefix: "com.apple.Music", category: CategoryMedia, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.spotify.client", category: CategoryMedia, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.apple.TV", category: CategoryMedia, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.apple.QuickTimePlayerX", category: CategoryMedia, envelopeKey: "", envelopeValue: ""},
	// system
	{prefix: "com.apple.finder", category: CategorySystem, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.apple.systempreferences", category: CategorySystem, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.apple.ActivityMonitor", category: CategorySystem, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.apple.controlcenter", category: CategorySystem, envelopeKey: "", envelopeValue: ""},
	{prefix: "com.apple.loginwindow", category: CategorySystem, envelopeKey: "", envelopeValue: ""},
}

// sortedRules is bundleCategoryRules sorted by descending prefix length so the FIRST match is the
// longest (most specific) match. Built once at init; the classifier is pure and deterministic.
var sortedRules = buildSortedRules()

func buildSortedRules() []categoryRule {
	rules := make([]categoryRule, len(bundleCategoryRules))
	copy(rules, bundleCategoryRules)
	sort.SliceStable(rules, func(i, j int) bool {
		return len(rules[i].prefix) > len(rules[j].prefix)
	})
	return rules
}

// Classify maps a bundle id (and optional envelope meta) to a category via
// longest-prefix match. The second return is confidence: 1.0 for a mapped id,
// 0.0 for an unmapped one (CategoryUnknown). No ML, no guessing.
//
// Phase 6 envelope refinement: a rule with a non-empty envelopeKey only fires
// when meta[envelopeKey] == envelopeValue. Bundle-only rules (envelopeKey == "")
// keep firing regardless of meta — so Tier-0 events with empty meta still
// classify exactly as before.
//
// Examples of envelope-aware refinement (all generic, public-safe):
//
//	Classify("com.example.terminal", nil)
//	  → CategoryCoding (bundle-only rule matches)
//	Classify("com.example.terminal", {"pane_type": "agent-session"})
//	  → CategoryCoding (envelope-aware rule matches on pane_type)
//	Classify("com.example.unknown", nil)
//	  → CategoryUnknown at confidence 0 (no rule matched)
func Classify(bundleID string, meta map[string]string) (Category, float64) {
	id := strings.TrimSpace(bundleID)
	if id == "" {
		return CategoryUnknown, 0
	}
	for _, r := range sortedRules {
		if !strings.HasPrefix(id, r.prefix) {
			continue
		}
		// Bundle-only rule: fires whenever the prefix matches.
		if r.envelopeKey == "" {
			return r.category, 1.0
		}
		// Envelope-aware rule — exact match.
		if r.envelopeValue != "" {
			if v, ok := meta[r.envelopeKey]; ok && v == r.envelopeValue {
				return r.category, 1.0
			}
		}
		// Envelope-aware rule — substring match (for unbounded values like
		// git_remote). The two modes are independent: a rule with both
		// envelopeValue and envelopeValueContains set fires when EITHER matches.
		if r.envelopeValueContains != "" {
			if v, ok := meta[r.envelopeKey]; ok && strings.Contains(v, r.envelopeValueContains) {
				return r.category, 1.0
			}
		}
		// Prefix matched but envelope constraint didn't — keep looking for a
		// shorter-prefix bundle-only rule or a different envelope-aware rule.
	}
	return CategoryUnknown, 0
}
