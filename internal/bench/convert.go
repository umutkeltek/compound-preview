package bench

// Converters turn the two REAL evaluation sources into bench suites, deterministically. "Same
// input bytes → byte-identical suite" is the load-bearing invariant: every slice is sorted, no
// timestamp or map-iteration order leaks in, so a converted suite is a stable artifact that can be
// diffed and re-baselined. The converted suites carry private tool/product/claim ids, so they are
// written only under $COMPOUND_HOME by the caller — never into the tracked tree.
//
// Faithful to the Wave-2 gate's "condition B" (the bench's own SurfaceForContext path): prompt →
// task, harness stays empty, and only the surfacing-expressible assertions carry over
// (required_anywhere ∪ required_top_n.ids → expected, forbidden_top_n_contains → Forbidden,
// must_outrank → MustOutrank). required_policy_for is NOT evaluated here ("only the resolver
// attaches policy") — it is dropped loudly, along with the resolver-internal selected_rule and the
// classifier class_cases. No silent truncation: DropAccounting names every dropped class.

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// AcceptanceSchemaVersion is the schema_version the capability-resolver acceptance file must carry.
const AcceptanceSchemaVersion = 1

// GoldenSchemaVersion is the schema_version the recall-golden bakeoff file must carry.
const GoldenSchemaVersion = "compound.memory_bakeoff_queries.v0.1"

// IDMap version identifiers for ConvertAcceptance's idMap parameter / the CLI --id-map flag.
// IDMapNone reproduces the pre-id-map converter byte-for-byte: every id/substring stays the
// resolver's raw dotted form. IDMapV1 translates the three original known legacy-resolver id shapes
// (see the id-map doc block below); it remains selectable and byte-stable (a falsifier: v1 output
// must never change once landed) but is no longer the default. IDMapV2 is the default: v1 plus the
// verified name-variant mappings from the 2026-07-09 Wave-2 parity adjudication (see the v2 doc
// block below translateIDV2) — a strict superset, so v2 only ever ADDS matches v1 missed; it never
// removes or changes a v1 match. Everything neither v1 nor v2 knows passes through raw AND is
// counted — named, never silent.
const (
	IDMapNone = "none"
	IDMapV1   = "v1"
	IDMapV2   = "v2"
)

// --- v1 canonical id map ------------------------------------------------------------------------
//
// Reverse-engineered and verified against the live compound graph (Wave-2 measurement,
// 2026-07-07; confirmed against real node ids on 2026-07-09 — skill ids are hyphenated slugs
// under skill:, e.g. skill:web-design-guidelines; recipe filenames become procedure: slugs, e.g.
// procedure:agent-harness-doctrine; tool ids are bare slugs under tool:/mcp:, e.g. tool:agent-browser).
// This is CODE, not config: a versioned map + a pure function, no graph access, deterministic.
//
// Three known shapes translate; everything else is raw passthrough, counted by family:
//   - skill.agents.shared[.<rest>]        -> skill:<rest, dots hyphenated>
//   - recipe.recipes.canonical.<rest>.md  -> procedure:<rest, dots hyphenated>
//   - tool[.<rest>]                       -> BOTH tool:<rest, dots hyphenated> AND mcp:<same>
//
// Tool-pair rule (must_outrank): a tool.<X> WINNER maps to two ids, not one — this converter emits
// TWO separate OutrankAssertion entries (Winner=tool:<X> and Winner=mcp:<X>, identical mapped
// LoserContains) rather than picking a single winner. Rationale: OutrankAssertion already folds an
// absent winner into absent_from_graph as a violation, never a silent pass (bench.go), so if only
// one of the pair actually exists in the graph the other surfaces honestly as a parity gap instead
// of the converter guessing which one is "real." Same rule applies to every tool.<X> reference:
// expected ids, forbidden_top_n_contains.contains, and must_outrank[].loser_contains all expand to
// the tool:/mcp: pair.
//
// Substring patterns (forbidden_top_n_contains.contains, must_outrank[].loser_contains) translate
// under the SAME three shapes above (a pattern equal to, or prefixed by, one of the three roots) —
// e.g. "skill.agents.shared" alone (no leaf) is a valid pattern and maps to "skill:" (matches any
// skill id as a substring, mirroring "starts with skill.agents.shared." pre-mapping). Anything else
// — including known-unmapped-family-shaped patterns (route.skills_sh., external., ...) and plain
// free-text substrings that were never id-shaped at all (e.g. "browser", "aso") — is left raw and
// tallied in IDMapReport.UnmappedPatterns as a single total, NOT broken out by family: unlike
// expected/must_outrank ids (which are always legacy-resolver ids), most forbidden/loser substrings
// are deliberately free text matched against surfaced node names, so a per-family breakdown would
// mostly report singleton buckets of ordinary English words — noise, not signal. The total count
// still makes every raw pattern visible and auditable (never silent).

const (
	skillAgentsSharedPrefix = "skill.agents.shared"
	recipeCanonicalPrefix   = "recipe.recipes.canonical"
	recipeCanonicalSuffix   = ".md"
	toolPrefix              = "tool"
)

// isSkillAgentsShared reports whether s is exactly the skill.agents.shared root or is rooted under
// it (skill.agents.shared.<rest>), returning the dotted rest (possibly empty).
func isSkillAgentsShared(s string) (rest string, ok bool) {
	if s == skillAgentsSharedPrefix {
		return "", true
	}
	if strings.HasPrefix(s, skillAgentsSharedPrefix+".") {
		return strings.TrimPrefix(s, skillAgentsSharedPrefix+"."), true
	}
	return "", false
}

// isRecipeCanonicalMD reports whether s is a recipe.recipes.canonical.<rest>.md id, returning the
// dotted rest between the root and the .md suffix. Requires both a non-root prefix and the .md
// suffix — every real recipe id carries both; a bare root or a missing suffix does not translate.
func isRecipeCanonicalMD(s string) (rest string, ok bool) {
	if !strings.HasPrefix(s, recipeCanonicalPrefix+".") || !strings.HasSuffix(s, recipeCanonicalSuffix) {
		return "", false
	}
	rest = strings.TrimSuffix(strings.TrimPrefix(s, recipeCanonicalPrefix+"."), recipeCanonicalSuffix)
	if rest == "" {
		return "", false
	}
	return rest, true
}

// isToolPrefix reports whether s is exactly "tool" or is rooted under it (tool.<rest>), returning
// the dotted rest (possibly empty).
func isToolPrefix(s string) (rest string, ok bool) {
	if s == toolPrefix {
		return "", true
	}
	if strings.HasPrefix(s, toolPrefix+".") {
		return strings.TrimPrefix(s, toolPrefix+"."), true
	}
	return "", false
}

// hyphenate converts a legacy resolver leaf's dot-separated words into the compound graph's
// hyphenated slug convention (e.g. "app.mechanics.lenses" -> "app-mechanics-lenses").
func hyphenate(s string) string {
	return strings.ReplaceAll(s, ".", "-")
}

// translateID applies the v1 canonical id map to a single legacy dotted id or substring pattern.
// matched=true means a known shape fired and mapped holds its translation(s) — 1 entry, except a
// tool.<X> shape which always yields 2 (tool:<X>, mcp:<X> — the tool-pair rule above). matched=false
// means no known v1 shape applies; mapped holds the ORIGINAL string unchanged (raw passthrough) so
// every call site can use the return value directly without a separate no-op branch, and the caller
// tallies the miss via familyOf.
func translateID(s string) (mapped []string, matched bool) {
	if rest, ok := isSkillAgentsShared(s); ok {
		return []string{"skill:" + hyphenate(rest)}, true
	}
	if rest, ok := isRecipeCanonicalMD(s); ok {
		return []string{"procedure:" + hyphenate(rest)}, true
	}
	if rest, ok := isToolPrefix(s); ok {
		h := hyphenate(rest)
		return []string{"tool:" + h, "mcp:" + h}, true
	}
	return []string{s}, false
}

// --- v2 canonical id map ------------------------------------------------------------------------
//
// v2 = v1 + verified name-variant mappings from the 2026-07-09 Wave-2 parity backlog adjudication
// (evidence: ~/.compound/reviews/parity/2026-07-09-wave2-parity-dispositions.{md,json} — a private
// $COMPOUND_HOME evidence file, not tracked here; this map stays generic-shape-only per repo
// privacy rules). Two additions:
//
//  1. Two more generic family-prefix shapes, verified the same way as the v1 three: the
//     codex-sourced skill corpus carries its "skill.codex.skills." / "skill.codex.superpowers."
//     root as pure resolver bookkeeping — the imported compound graph node drops it entirely and
//     lands as a plain skill:<rest, dots hyphenated>, so both roots collapse straight to skill:
//     (verified against 23 of 25 live skill.codex.* ids on 2026-07-09; the other 2 are the literal
//     overrides below).
//  2. A small literal table for one-off renames neither shape reaches: a duplicate skill name
//     disambiguated by source (skill-creator exists as both skill:agents:skill-creator and
//     skill:codex:skill-creator; the legacy skill.agents.shared.skill.creator id names the agents
//     source explicitly), a skill whose SKILL.md frontmatter name drifted from its directory name
//     (taste-skill -> skill:design-taste-frontend), two codex-sourced skills whose landed slug
//     dropped a filler word the generic shape rule keeps (diff.cleanup.and.parking,
//     finishing.a.development.branch), a known rename (superpowers -> using-superpowers), and one
//     tool whose live graph node is a different KIND than tool:/mcp: (mcporter landed as
//     adapter:mcporter — the v1 tool-pair rule's two-id emission is provably wrong for this one id
//     since NEITHER tool:mcporter nor mcp:mcporter exists, so it is overridden to its single real
//     target instead of the pair). Every entry here is a real, generic, non-private name verified
//     against the live compound graph on 2026-07-09.
const (
	skillCodexSkillsPrefix      = "skill.codex.skills"
	skillCodexSuperpowersPrefix = "skill.codex.superpowers"
)

// isSkillCodexSkills / isSkillCodexSuperpowers mirror isSkillAgentsShared's bare-root-or-rooted
// shape for the two codex-sourced skill namespaces.
func isSkillCodexSkills(s string) (rest string, ok bool) {
	if s == skillCodexSkillsPrefix {
		return "", true
	}
	if strings.HasPrefix(s, skillCodexSkillsPrefix+".") {
		return strings.TrimPrefix(s, skillCodexSkillsPrefix+"."), true
	}
	return "", false
}

func isSkillCodexSuperpowers(s string) (rest string, ok bool) {
	if s == skillCodexSuperpowersPrefix {
		return "", true
	}
	if strings.HasPrefix(s, skillCodexSuperpowersPrefix+".") {
		return strings.TrimPrefix(s, skillCodexSuperpowersPrefix+"."), true
	}
	return "", false
}

// v2LiteralOverrides are exact-match legacy id -> compound id(s) the generic v1/v2 shapes cannot
// reach (see the v2 doc block above for why each one exists). Checked before any shape rule so a
// literal override always wins over a shape match for the same id.
var v2LiteralOverrides = map[string][]string{
	"skill.agents.shared.superpowers":                        {"skill:using-superpowers"},
	"skill.agents.shared.skill.creator":                      {"skill:agents:skill-creator"},
	"skill.agents.shared.taste.skill":                        {"skill:design-taste-frontend"},
	"skill.codex.skills.diff.cleanup.and.parking":            {"skill:diff-cleanup-and-parking"},
	"skill.codex.superpowers.finishing.a.development.branch": {"skill:finishing-a-development-branch"},
	"tool.mcporter": {"adapter:mcporter"},
}

// translateIDV2 applies the v2 canonical id map: literal overrides first (exact match only), then
// the two codex skill-namespace shapes, then the original v1 shapes. Same matched=false contract as
// translateID — v2 is a strict superset of v1, so anything v1 matched, v2 matches identically.
func translateIDV2(s string) (mapped []string, matched bool) {
	if m, ok := v2LiteralOverrides[s]; ok {
		return append([]string(nil), m...), true
	}
	if rest, ok := isSkillCodexSkills(s); ok {
		return []string{"skill:" + hyphenate(rest)}, true
	}
	if rest, ok := isSkillCodexSuperpowers(s); ok {
		return []string{"skill:" + hyphenate(rest)}, true
	}
	return translateID(s)
}

// familyOf computes the accounting bucket for a legacy id/pattern that matched none of the v1
// known-mapped shapes (translateID returned matched=false). route/skill/recipe/capability
// namespaces use their first TWO dot segments — a single segment would be too coarse (e.g. bare
// "skill" would conflate the codex skill corpus with any other skill.* namespace); every other
// namespace uses its first segment, matching the family names the Wave-2 measurement doc named
// literally (route.skills_sh, function, memory, capability_group, agent, external). An id with no
// dot at all is its own family verbatim.
func familyOf(id string) string {
	segs := strings.SplitN(id, ".", 3)
	if len(segs) == 1 {
		return segs[0]
	}
	switch segs[0] {
	case "route", "skill", "recipe", "capability":
		return segs[0] + "." + segs[1]
	default:
		return segs[0]
	}
}

// unmappedReason is the DropRecord reason attached to every unmapped-id-family tally entry.
const unmappedReason = "no v1 canonical id mapping; raw legacy id passthrough (named + counted, never silent — W5-J9 backlog)"

// familyRecords turns a family->count tally into sorted DropRecords (by family name), so the
// printed/returned accounting is deterministic regardless of map iteration order.
func familyRecords(counts map[string]int) []DropRecord {
	if len(counts) == 0 {
		return nil
	}
	families := make([]string, 0, len(counts))
	for f := range counts {
		families = append(families, f)
	}
	sort.Strings(families)
	out := make([]DropRecord, 0, len(families))
	for _, fam := range families {
		out = append(out, DropRecord{Field: fam, Count: counts[fam], Reason: unmappedReason})
	}
	return out
}

// IDMapReport is convert-time accounting for the applied canonical id map (v1 or v2): which legacy
// ids/patterns had no known mapping under that version and so passed through raw, named and counted
// by family. It is intentionally NOT part of Suite (Suite's schema is owned by bench.go and stays
// spec-faithful to the evaluator); it is a side channel the convert CLI prints as its "unmapped"
// summary block. Zero value / Version == IDMapNone means mapping did not run (nothing to account for).
type IDMapReport struct {
	Version            string
	UnmappedIDFamilies []DropRecord // ids from expected/must_outrank winners with no mapping under the applied version, by family
	UnmappedPatterns   int          // forbidden/loser_contains substrings with no mapping under the applied version (single total; see doc block above)
}

// applyIDMapToCase rewrites a raw-converted Case's id-bearing fields (Expected, Forbidden.Contains,
// MustOutrank) through translate (translateID for v1, translateIDV2 for v2), in place, and folds
// every unmapped id/pattern into the running tallies. Every rewritten slice is deduped+resorted
// after mapping (a tool.<X> id/pattern expands to two entries, and unrelated raw ids can collide
// post-hyphenation), preserving the suite's overall sorted-slices determinism guarantee.
func applyIDMapToCase(c *Case, idFamilyCounts map[string]int, unmappedPatterns *int, translate func(string) ([]string, bool)) {
	expected := map[string]bool{}
	for _, raw := range c.Expected {
		mapped, ok := translate(raw)
		if !ok {
			idFamilyCounts[familyOf(raw)]++
		}
		for _, m := range mapped {
			expected[m] = true
		}
	}
	c.Expected = sortedKeys(expected)

	if c.Forbidden != nil {
		forbidden := map[string]bool{}
		for _, raw := range c.Forbidden.Contains {
			mapped, ok := translate(raw)
			if !ok {
				*unmappedPatterns++
			}
			for _, m := range mapped {
				forbidden[m] = true
			}
		}
		c.Forbidden.Contains = sortedKeys(forbidden)
	}

	if len(c.MustOutrank) > 0 {
		out := make([]OutrankAssertion, 0, len(c.MustOutrank))
		for _, a := range c.MustOutrank {
			losers := map[string]bool{}
			for _, raw := range a.LoserContains {
				mapped, ok := translate(raw)
				if !ok {
					*unmappedPatterns++
				}
				for _, m := range mapped {
					losers[m] = true
				}
			}
			sortedLosers := sortedKeys(losers)
			winners, ok := translate(a.Winner)
			if !ok {
				idFamilyCounts[familyOf(a.Winner)]++
			}
			for _, w := range winners {
				out = append(out, OutrankAssertion{Winner: w, LoserContains: append([]string(nil), sortedLosers...)})
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Winner != out[j].Winner {
				return out[i].Winner < out[j].Winner
			}
			return strings.Join(out[i].LoserContains, ",") < strings.Join(out[j].LoserContains, ",")
		})
		c.MustOutrank = out
	}
}

// Drop-accounting field names (stable strings; also the keys a reader greps for).
const (
	dropRequiredPolicyFor = "required_policy_for"
	dropSelectedRule      = "selected_rule"
	dropClassCases        = "class_cases"
	dropTaskSpecRouting   = "task_spec_routing_preference"
)

// splitForID assigns a deterministic, stable held_in/held_out split from the case id alone
// (FNV-1a mod 10: <7 held_in, else held_out). Stable across runs and independent of case order, so
// re-converting the same source keeps every case in the same split — a moved case would silently
// invalidate a baseline comparison.
func splitForID(id string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	if h.Sum32()%10 < 7 {
		return SplitHeldIn
	}
	return SplitHeldOut
}

// SplitRuleDoc is the human-readable split rule, surfaced in the CLI usage so the assignment is not
// a black box.
const SplitRuleDoc = "split = FNV-1a(id) mod 10 < 7 ? held_in : held_out (stable per case id)"

type acceptanceFile struct {
	SchemaVersion int               `json:"schema_version"`
	ClassCases    []json.RawMessage `json:"class_cases"`
	Cases         []acceptanceCase  `json:"cases"`
}

type acceptanceCase struct {
	ID                    string              `json:"id"`
	Prompt                string              `json:"prompt"`
	SelectedRule          string              `json:"selected_rule"`
	RequiredAnywhere      []string            `json:"required_anywhere"`
	RequiredTopN          *acceptanceTopN     `json:"required_top_n"`
	RequiredPolicyFor     []string            `json:"required_policy_for"`
	ForbiddenTopNContains *acceptanceForbid   `json:"forbidden_top_n_contains"`
	MustOutrank           []acceptanceOutrank `json:"must_outrank"`
	TaskSpec              json.RawMessage     `json:"task_spec"`
}

type acceptanceTopN struct {
	N   int      `json:"n"`
	IDs []string `json:"ids"`
}

type acceptanceForbid struct {
	N        int      `json:"n"`
	Contains []string `json:"contains"`
}

type acceptanceOutrank struct {
	Winner        string   `json:"winner"`
	LoserContains []string `json:"loser_contains"`
}

// ConvertAcceptance parses the capability-resolver acceptance JSON and returns the bench suite plus
// the drop-accounting. It is deterministic: cases sorted by id, every id/substring slice sorted.
//
// idMap selects the id-mapping pass applied AFTER the raw, spec-faithful conversion below. Empty
// string defaults to IDMapV2. IDMapNone skips the pass entirely, byte-for-byte preserving the
// pre-id-map converter's raw dotted-id output; IDMapV1 rewrites every id-bearing field through the
// v1 canonical id map (see the doc block above translateID); IDMapV2 does the same through the v2
// map (v1 + verified name-variant mappings, see the doc block above translateIDV2). Either mapped
// version returns the unmapped-id/pattern accounting as the second result.
func ConvertAcceptance(data []byte, suiteName string, idMap string) (Suite, IDMapReport, error) {
	switch idMap {
	case "":
		idMap = IDMapV2
	case IDMapV1, IDMapV2, IDMapNone:
	default:
		return Suite{}, IDMapReport{}, fmt.Errorf("id-map must be %q, %q, or %q, got %q", IDMapV2, IDMapV1, IDMapNone, idMap)
	}

	var f acceptanceFile
	if err := json.Unmarshal(data, &f); err != nil {
		return Suite{}, IDMapReport{}, fmt.Errorf("parse acceptance json: %w", err)
	}
	if f.SchemaVersion != AcceptanceSchemaVersion {
		return Suite{}, IDMapReport{}, fmt.Errorf("acceptance schema_version must be %d, got %d", AcceptanceSchemaVersion, f.SchemaVersion)
	}
	if len(f.Cases) == 0 {
		return Suite{}, IDMapReport{}, fmt.Errorf("acceptance file has no cases")
	}

	var (
		cases            []Case
		droppedPolicyFor int
		droppedSelected  int
		droppedTaskSpec  int
	)
	for i, ac := range f.Cases {
		if strings.TrimSpace(ac.ID) == "" {
			return Suite{}, IDMapReport{}, fmt.Errorf("acceptance cases[%d] missing id", i)
		}
		expected := map[string]bool{}
		for _, id := range ac.RequiredAnywhere {
			if s := strings.TrimSpace(id); s != "" {
				expected[s] = true
			}
		}
		if ac.RequiredTopN != nil {
			for _, id := range ac.RequiredTopN.IDs {
				if s := strings.TrimSpace(id); s != "" {
					expected[s] = true
				}
			}
		}
		if len(expected) == 0 {
			return Suite{}, IDMapReport{}, fmt.Errorf("acceptance case %q has no required_anywhere/required_top_n ids to convert", ac.ID)
		}
		c := Case{
			ID:       ac.ID,
			Kind:     KindCapability,
			Task:     ac.Prompt,
			Expected: sortedKeys(expected),
			Lane:     "exact",
			Split:    splitForID(ac.ID),
		}
		if ac.ForbiddenTopNContains != nil && len(ac.ForbiddenTopNContains.Contains) > 0 {
			subs := append([]string(nil), ac.ForbiddenTopNContains.Contains...)
			sort.Strings(subs)
			c.Forbidden = &ForbiddenTopN{N: ac.ForbiddenTopNContains.N, Contains: subs}
		}
		if len(ac.MustOutrank) > 0 {
			c.MustOutrank = convertOutrank(ac.MustOutrank)
		}
		cases = append(cases, c)

		if len(ac.RequiredPolicyFor) > 0 {
			droppedPolicyFor++
		}
		if strings.TrimSpace(ac.SelectedRule) != "" {
			droppedSelected++
		}
		if len(ac.TaskSpec) > 0 && string(ac.TaskSpec) != "null" {
			droppedTaskSpec++
		}
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })

	// The id map is a POST-processing pass over the raw, spec-faithful cases built above — the raw
	// build path is untouched by idMap, so IDMapNone is byte-identical to the pre-id-map converter
	// by construction (same code, no branch), not by a separately maintained parallel path. v1 is
	// likewise untouched by the v2 addition below (translateID, the function v1 calls, is unchanged)
	// so v1 output stays byte-stable across the v2 addition — a falsifier, not an assumption.
	report := IDMapReport{Version: idMap}
	if idMap == IDMapV1 || idMap == IDMapV2 {
		translate := translateID
		if idMap == IDMapV2 {
			translate = translateIDV2
		}
		idFamilyCounts := map[string]int{}
		unmappedPatterns := 0
		for i := range cases {
			applyIDMapToCase(&cases[i], idFamilyCounts, &unmappedPatterns, translate)
		}
		report.UnmappedIDFamilies = familyRecords(idFamilyCounts)
		report.UnmappedPatterns = unmappedPatterns
	}

	dropped := []DropRecord{
		{Field: dropRequiredPolicyFor, Count: droppedPolicyFor, Reason: "resolver-internal: only the resolver attaches policy; condition B (SurfaceForContext) does not evaluate it — Wave-2 exclusion kept"},
		{Field: dropSelectedRule, Count: droppedSelected, Reason: "resolver-internal rule id; not a surfacing assertion"},
		{Field: dropClassCases, Count: len(f.ClassCases), Reason: "classifier check (prompt→expected_class); out of the capability-surfacing scope"},
		{Field: dropTaskSpecRouting, Count: droppedTaskSpec, Reason: "task_spec routing preference (capability_route/avoid) is not a surfacing assertion; case converted via its raw prompt"},
	}

	suite := Suite{Suite: suiteName, Dropped: dropped, Cases: cases}
	if err := suite.validate(); err != nil {
		return Suite{}, IDMapReport{}, err
	}
	return suite, report, nil
}

func convertOutrank(in []acceptanceOutrank) []OutrankAssertion {
	out := make([]OutrankAssertion, 0, len(in))
	for _, o := range in {
		if strings.TrimSpace(o.Winner) == "" {
			continue
		}
		losers := make([]string, 0, len(o.LoserContains))
		for _, l := range o.LoserContains {
			if s := strings.TrimSpace(l); s != "" {
				losers = append(losers, s)
			}
		}
		sort.Strings(losers)
		out = append(out, OutrankAssertion{Winner: o.Winner, LoserContains: losers})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Winner != out[j].Winner {
			return out[i].Winner < out[j].Winner
		}
		return strings.Join(out[i].LoserContains, ",") < strings.Join(out[j].LoserContains, ",")
	})
	return out
}

type goldenFile struct {
	SchemaVersion string        `yaml:"schema_version"`
	Queries       []goldenQuery `yaml:"queries"`
}

type goldenQuery struct {
	ID               string   `yaml:"id"`
	Lane             string   `yaml:"lane"`
	Query            string   `yaml:"query"`
	ExpectedClaimIDs []string `yaml:"expected_claim_ids"`
}

// ConvertGolden parses the recall-golden bakeoff YAML and returns the memory bench suite. It is
// deterministic (cases sorted by id, expected claim ids sorted+deduped). Nothing is dropped — the
// bakeoff schema is already surfacing-shaped — so the suite carries no Dropped block.
func ConvertGolden(data []byte, suiteName string) (Suite, error) {
	var f goldenFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return Suite{}, fmt.Errorf("parse golden yaml: %w", err)
	}
	if strings.TrimSpace(f.SchemaVersion) != GoldenSchemaVersion {
		return Suite{}, fmt.Errorf("golden schema_version must be %q, got %q", GoldenSchemaVersion, f.SchemaVersion)
	}
	if len(f.Queries) == 0 {
		return Suite{}, fmt.Errorf("golden file has no queries")
	}
	var cases []Case
	for i, q := range f.Queries {
		if strings.TrimSpace(q.ID) == "" {
			return Suite{}, fmt.Errorf("golden queries[%d] missing id", i)
		}
		expected := map[string]bool{}
		for _, id := range q.ExpectedClaimIDs {
			if s := strings.TrimSpace(id); s != "" {
				expected[s] = true
			}
		}
		if len(expected) == 0 {
			return Suite{}, fmt.Errorf("golden query %q has no expected_claim_ids", q.ID)
		}
		cases = append(cases, Case{
			ID:       q.ID,
			Kind:     KindMemory,
			Task:     q.Query,
			Expected: sortedKeys(expected),
			Lane:     q.Lane,
			Split:    splitForID(q.ID),
		})
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	suite := Suite{Suite: suiteName, Cases: cases}
	if err := suite.validate(); err != nil {
		return Suite{}, err
	}
	return suite, nil
}

// MarshalSuite renders a suite to deterministic, byte-stable JSON (2-space indent, trailing
// newline). The determinism guarantee rests on the converters sorting every slice; Go marshals
// struct fields in declaration order and map keys sorted, so no ordering nondeterminism remains.
func MarshalSuite(s Suite) ([]byte, error) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
