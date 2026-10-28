package bench

import (
	"reflect"
	"sort"
	"testing"
)

// syntheticAcceptanceJSON is a fully SYNTHETIC acceptance file (no real tool/product ids or prompt
// text): fields are deliberately out of sorted order so the determinism + sorting invariants are
// exercised, not merely asserted on already-sorted input. Its ids ("node:X") deliberately match
// NONE of the v1 id-map shapes, so it doubles as the id-map-inert fixture for backward-compat tests.
const syntheticAcceptanceJSON = `{
  "schema_version": 1,
  "class_cases": [
    {"prompt": "classify one", "expected_class": "alpha"},
    {"prompt": "classify two", "expected_class": "beta"}
  ],
  "cases": [
    {
      "id": "case-beta",
      "prompt": "beta prompt text",
      "selected_rule": "rule-beta",
      "required_anywhere": ["node:beta"],
      "required_top_n": {"n": 5, "ids": ["node:beta", "node:alpha"]},
      "required_policy_for": ["node:beta"],
      "forbidden_top_n_contains": {"n": 3, "contains": ["zeta", "alpha"]},
      "must_outrank": [
        {"winner": "node:beta", "loser_contains": ["zeta", "alpha"]},
        {"winner": "node:alpha", "loser_contains": ["gamma"]}
      ]
    },
    {
      "id": "case-alpha",
      "prompt": "alpha prompt text",
      "selected_rule": "rule-alpha",
      "required_top_n": {"n": 8, "ids": ["node:alpha"]},
      "required_policy_for": ["node:alpha"]
    },
    {
      "id": "case-gamma",
      "prompt": "gamma prompt text",
      "selected_rule": "rule-gamma",
      "required_anywhere": ["node:gamma"]
    }
  ]
}`

// preIDMapSyntheticSuiteJSON is the EXACT byte output the converter produced for
// syntheticAcceptanceJSON before the id-map feature existed (captured from the unmodified
// pre-change ConvertAcceptance(data, "syn") via a throwaway harness run, byte for byte). It is the
// oracle for falsifier 2: `--id-map none` must still produce this, proving true backward compat
// rather than a reimplementation that merely "looks the same."
const preIDMapSyntheticSuiteJSON = `{
  "suite": "syn",
  "dropped": [
    {
      "field": "required_policy_for",
      "count": 2,
      "reason": "resolver-internal: only the resolver attaches policy; condition B (SurfaceForContext) does not evaluate it — Wave-2 exclusion kept"
    },
    {
      "field": "selected_rule",
      "count": 3,
      "reason": "resolver-internal rule id; not a surfacing assertion"
    },
    {
      "field": "class_cases",
      "count": 2,
      "reason": "classifier check (prompt→expected_class); out of the capability-surfacing scope"
    },
    {
      "field": "task_spec_routing_preference",
      "count": 0,
      "reason": "task_spec routing preference (capability_route/avoid) is not a surfacing assertion; case converted via its raw prompt"
    }
  ],
  "cases": [
    {
      "id": "case-alpha",
      "kind": "capability",
      "task": "alpha prompt text",
      "expected": [
        "node:alpha"
      ],
      "lane": "exact",
      "split": "held_out"
    },
    {
      "id": "case-beta",
      "kind": "capability",
      "task": "beta prompt text",
      "expected": [
        "node:alpha",
        "node:beta"
      ],
      "lane": "exact",
      "split": "held_in",
      "forbidden": {
        "n": 3,
        "contains": [
          "alpha",
          "zeta"
        ]
      },
      "must_outrank": [
        {
          "winner": "node:alpha",
          "loser_contains": [
            "gamma"
          ]
        },
        {
          "winner": "node:beta",
          "loser_contains": [
            "alpha",
            "zeta"
          ]
        }
      ]
    },
    {
      "id": "case-gamma",
      "kind": "capability",
      "task": "gamma prompt text",
      "expected": [
        "node:gamma"
      ],
      "lane": "exact",
      "split": "held_in"
    }
  ]
}
`

// TestConvertAcceptanceDeterministic falsifies nondeterminism: converting the same input bytes
// twice yields byte-identical suites, and the output is actually SORTED (cases by id, expected +
// forbidden + outrank slices sorted) — so "deterministic" is not vacuously true on pre-sorted data.
// Run under --id-map none: these assertions encode the pre-id-map converter's exact shape, so they
// double as part of the backward-compat proof (see TestConvertAcceptanceNoneByteIdenticalToPreChange
// for the literal byte comparison).
func TestConvertAcceptanceDeterministic(t *testing.T) {
	a, _, err := ConvertAcceptance([]byte(syntheticAcceptanceJSON), "syn", IDMapNone)
	if err != nil {
		t.Fatalf("convert a: %v", err)
	}
	b, _, err := ConvertAcceptance([]byte(syntheticAcceptanceJSON), "syn", IDMapNone)
	if err != nil {
		t.Fatalf("convert b: %v", err)
	}
	ab, err := MarshalSuite(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := MarshalSuite(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ab) != string(bb) {
		t.Fatalf("convert not byte-deterministic:\n--a--\n%s\n--b--\n%s", ab, bb)
	}

	// cases sorted by id
	if got := []string{a.Cases[0].ID, a.Cases[1].ID, a.Cases[2].ID}; !reflect.DeepEqual(got, []string{"case-alpha", "case-beta", "case-gamma"}) {
		t.Fatalf("cases not sorted by id: %v", got)
	}
	beta := a.Cases[1]
	if !reflect.DeepEqual(beta.Expected, []string{"node:alpha", "node:beta"}) {
		t.Fatalf("beta expected = %v, want sorted union [node:alpha node:beta]", beta.Expected)
	}
	if beta.Forbidden == nil || !reflect.DeepEqual(beta.Forbidden.Contains, []string{"alpha", "zeta"}) || beta.Forbidden.N != 3 {
		t.Fatalf("beta forbidden = %+v, want N=3 contains sorted [alpha zeta]", beta.Forbidden)
	}
	if len(beta.MustOutrank) != 2 || beta.MustOutrank[0].Winner != "node:alpha" || beta.MustOutrank[1].Winner != "node:beta" {
		t.Fatalf("beta must_outrank not sorted by winner: %+v", beta.MustOutrank)
	}
	if !reflect.DeepEqual(beta.MustOutrank[1].LoserContains, []string{"alpha", "zeta"}) {
		t.Fatalf("beta outrank loser_contains not sorted: %v", beta.MustOutrank[1].LoserContains)
	}
	// harness empty (condition B), lane exact, kind capability
	if beta.Harness != "" || beta.Lane != "exact" || beta.Kind != KindCapability {
		t.Fatalf("beta case shape = %+v, want empty harness / lane exact / kind capability", beta)
	}
}

// TestConvertAcceptanceNoneByteIdenticalToPreChange falsifies a regression in backward compat:
// --id-map none must reproduce the pre-id-map converter's marshaled bytes EXACTLY (see
// preIDMapSyntheticSuiteJSON's provenance comment), not merely "equivalent" output.
func TestConvertAcceptanceNoneByteIdenticalToPreChange(t *testing.T) {
	suite, report, err := ConvertAcceptance([]byte(syntheticAcceptanceJSON), "syn", IDMapNone)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	got, err := MarshalSuite(suite)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != preIDMapSyntheticSuiteJSON {
		t.Fatalf("id-map none diverged from pre-change converter:\n--got--\n%s\n--want--\n%s", got, preIDMapSyntheticSuiteJSON)
	}
	if report.Version != IDMapNone || len(report.UnmappedIDFamilies) != 0 || report.UnmappedPatterns != 0 {
		t.Fatalf("id-map none report = %+v, want version=none and zero accounting (mapping did not run)", report)
	}

	// Empty idMap defaults to v2, NOT none/v1 — confirm that default changes the ids ("node:*" ids
	// stay raw either way since they match no v1 or v2 shape, so assert via report.Version instead).
	_, defaultReport, err := ConvertAcceptance([]byte(syntheticAcceptanceJSON), "syn", "")
	if err != nil {
		t.Fatalf("convert default: %v", err)
	}
	if defaultReport.Version != IDMapV2 {
		t.Fatalf("default id-map version = %q, want %q", defaultReport.Version, IDMapV2)
	}
}

// TestConvertAcceptanceRejectsUnknownIDMap falsifies silent acceptance of a typo'd --id-map value.
func TestConvertAcceptanceRejectsUnknownIDMap(t *testing.T) {
	if _, _, err := ConvertAcceptance([]byte(syntheticAcceptanceJSON), "syn", "v3"); err == nil {
		t.Fatal("expected unknown id-map version to error")
	}
}

// TestConvertAcceptanceDropAccounting falsifies silent truncation: each droppable class is counted
// exactly, and a case whose only extra assertions are droppable (selected_rule + required_policy_for)
// still converts, carrying its expected ids with no forbidden/outrank.
func TestConvertAcceptanceDropAccounting(t *testing.T) {
	suite, _, err := ConvertAcceptance([]byte(syntheticAcceptanceJSON), "syn", IDMapNone)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := map[string]int{
		"required_policy_for":          2, // case-beta, case-alpha
		"selected_rule":                3, // all three
		"class_cases":                  2, // two class_cases
		"task_spec_routing_preference": 0, // none present
	}
	got := map[string]int{}
	for _, d := range suite.Dropped {
		got[d.Field] = d.Count
		if d.Reason == "" {
			t.Fatalf("drop record %q has no reason", d.Field)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("drop counts = %v, want %v", got, want)
	}

	// case-alpha: only droppable extras -> still converts with its expected id, nothing asserted.
	alpha := suite.Cases[0]
	if alpha.ID != "case-alpha" {
		t.Fatalf("cases[0] = %s, want case-alpha", alpha.ID)
	}
	if !reflect.DeepEqual(alpha.Expected, []string{"node:alpha"}) {
		t.Fatalf("case-alpha expected = %v, want [node:alpha] (droppable assertions must not lose it)", alpha.Expected)
	}
	if alpha.Forbidden != nil || len(alpha.MustOutrank) != 0 {
		t.Fatalf("case-alpha should assert nothing beyond expected, got forbidden=%+v outrank=%+v", alpha.Forbidden, alpha.MustOutrank)
	}
}

// --- v1 id-map falsifiers -------------------------------------------------------------------

// syntheticV1ShapesJSON exercises all three known v1 shapes plus a tool.<x> that must pair-expand
// in both a plain expected id and a must_outrank winner. All ids/prompts are synthetic.
const syntheticV1ShapesJSON = `{
  "schema_version": 1,
  "class_cases": [],
  "cases": [
    {
      "id": "case-shapes",
      "prompt": "shapes prompt",
      "required_anywhere": [
        "skill.agents.shared.example.widget",
        "recipe.recipes.canonical.example.playbook.md",
        "tool.example.thing"
      ],
      "must_outrank": [
        {"winner": "tool.example.thing", "loser_contains": ["skill.agents.shared.noise", "unrelated_word"]}
      ]
    },
    {
      "id": "case-bare",
      "prompt": "bare prompt",
      "required_anywhere": ["skill.agents.shared", "tool.simple"]
    }
  ]
}`

// TestConvertAcceptanceV1MapsKnownShapes falsifies a neutered map: each of the three documented v1
// shapes must map to its documented target, dots hyphenated, and a bare-root pattern/id (no leaf)
// must still map (e.g. "skill.agents.shared" alone -> "skill:").
func TestConvertAcceptanceV1MapsKnownShapes(t *testing.T) {
	suite, report, err := ConvertAcceptance([]byte(syntheticV1ShapesJSON), "syn-shapes", IDMapV1)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if report.Version != IDMapV1 {
		t.Fatalf("report version = %q, want %q", report.Version, IDMapV1)
	}

	var shapes, bare Case
	for _, c := range suite.Cases {
		switch c.ID {
		case "case-shapes":
			shapes = c
		case "case-bare":
			bare = c
		}
	}

	wantShapesExpected := []string{
		"mcp:example-thing",
		"procedure:example-playbook",
		"skill:example-widget",
		"tool:example-thing",
	}
	if !reflect.DeepEqual(shapes.Expected, wantShapesExpected) {
		t.Fatalf("case-shapes expected = %v, want %v", shapes.Expected, wantShapesExpected)
	}

	wantBareExpected := []string{"mcp:simple", "skill:", "tool:simple"}
	if !reflect.DeepEqual(bare.Expected, wantBareExpected) {
		t.Fatalf("case-bare expected = %v, want %v (bare-root skill.agents.shared -> skill:)", bare.Expected, wantBareExpected)
	}

	// A tool.<x> winner pair-expands into two OutrankAssertion entries sharing the mapped losers.
	if len(shapes.MustOutrank) != 2 {
		t.Fatalf("case-shapes must_outrank = %+v, want 2 entries (tool-pair rule)", shapes.MustOutrank)
	}
	wantWinners := []string{"mcp:example-thing", "tool:example-thing"}
	gotWinners := []string{shapes.MustOutrank[0].Winner, shapes.MustOutrank[1].Winner}
	if !reflect.DeepEqual(gotWinners, wantWinners) {
		t.Fatalf("case-shapes must_outrank winners = %v, want %v", gotWinners, wantWinners)
	}
	wantLosers := []string{"skill:noise", "unrelated_word"}
	for _, a := range shapes.MustOutrank {
		if !reflect.DeepEqual(a.LoserContains, wantLosers) {
			t.Fatalf("winner %q loser_contains = %v, want %v", a.Winner, a.LoserContains, wantLosers)
		}
	}
	// "unrelated_word" matched no known shape -> tallied as an unmapped pattern.
	if report.UnmappedPatterns != 1 {
		t.Fatalf("unmapped patterns = %d, want 1 (unrelated_word)", report.UnmappedPatterns)
	}
}

// syntheticUnmappedFamiliesJSON has exactly one id per unmapped family (the ones the Wave-2 doc
// named literally, plus a genuinely novel namespace to prove "anything else" is also named, not
// dumped into a black-hole "other" bucket) so the family tally is exactly falsifiable.
const syntheticUnmappedFamiliesJSON = `{
  "schema_version": 1,
  "class_cases": [],
  "cases": [
    {"id": "case-route", "prompt": "p", "required_anywhere": ["route.skills_sh.example.route"]},
    {"id": "case-function", "prompt": "p", "required_anywhere": ["function.example"]},
    {"id": "case-memory", "prompt": "p", "required_anywhere": ["memory.example"]},
    {"id": "case-capgroup", "prompt": "p", "required_anywhere": ["capability_group.example"]},
    {"id": "case-agent", "prompt": "p", "required_anywhere": ["agent.example"]},
    {"id": "case-external", "prompt": "p", "required_anywhere": ["external.example"]},
    {"id": "case-skillcodex", "prompt": "p", "required_anywhere": ["skill.codex.skills.example"]},
    {"id": "case-novel", "prompt": "p", "required_anywhere": ["widget.example"]}
  ]
}`

// TestConvertAcceptanceV1UnmappedFamiliesCounted is falsifier 4: a fabricated acceptance file with
// one id per unmapped family must count each one, by family, and none may be silently dropped
// (still present raw in Expected) or silently mismapped (raw id unchanged).
func TestConvertAcceptanceV1UnmappedFamiliesCounted(t *testing.T) {
	suite, report, err := ConvertAcceptance([]byte(syntheticUnmappedFamiliesJSON), "syn-unmapped", IDMapV1)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := map[string]int{
		"route.skills_sh":  1,
		"function":         1,
		"memory":           1,
		"capability_group": 1,
		"agent":            1,
		"external":         1,
		"skill.codex":      1, // distinct from the mapped skill.agents.shared shape
		"widget":           1, // novel namespace, still named (not lumped into "other")
	}
	got := map[string]int{}
	for _, d := range report.UnmappedIDFamilies {
		got[d.Field] = d.Count
		if d.Reason == "" {
			t.Fatalf("unmapped family %q has no reason", d.Field)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unmapped id families = %v, want %v", got, want)
	}

	// none silently dropped: every raw id is still present, unchanged, in its case's Expected.
	rawByCaseID := map[string]string{
		"case-route":      "route.skills_sh.example.route",
		"case-function":   "function.example",
		"case-memory":     "memory.example",
		"case-capgroup":   "capability_group.example",
		"case-agent":      "agent.example",
		"case-external":   "external.example",
		"case-skillcodex": "skill.codex.skills.example",
		"case-novel":      "widget.example",
	}
	for _, c := range suite.Cases {
		want, ok := rawByCaseID[c.ID]
		if !ok {
			t.Fatalf("unexpected case %q", c.ID)
		}
		if !reflect.DeepEqual(c.Expected, []string{want}) {
			t.Fatalf("case %q expected = %v, want raw passthrough [%s]", c.ID, c.Expected, want)
		}
	}
}

// syntheticPatternTranslationJSON exercises the substring-pattern rule: a pattern that IS a known
// family prefix translates; a known-unmapped-family-shaped pattern and a plain free-text word both
// stay raw and get tallied under UnmappedPatterns (a single total, not broken out by family).
const syntheticPatternTranslationJSON = `{
  "schema_version": 1,
  "class_cases": [],
  "cases": [
    {
      "id": "case-patterns",
      "prompt": "p",
      "required_anywhere": ["skill.agents.shared.anchor"],
      "forbidden_top_n_contains": {
        "n": 5,
        "contains": ["skill.agents.shared.leaf", "route.skills_sh.thing", "genericword"]
      }
    }
  ]
}`

// TestConvertAcceptanceV1PatternTranslation falsifies both halves of the pattern rule: a
// known-family-prefix substring translates (dots hyphenated, prefix swapped), and everything else —
// whether namespace-shaped-but-unmapped or plain free text — is left raw and counted as one total.
func TestConvertAcceptanceV1PatternTranslation(t *testing.T) {
	suite, report, err := ConvertAcceptance([]byte(syntheticPatternTranslationJSON), "syn-patterns", IDMapV1)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	c := suite.Cases[0]
	wantContains := []string{"genericword", "route.skills_sh.thing", "skill:leaf"}
	if c.Forbidden == nil || !reflect.DeepEqual(c.Forbidden.Contains, wantContains) {
		t.Fatalf("forbidden.contains = %+v, want %v", c.Forbidden, wantContains)
	}
	if report.UnmappedPatterns != 2 {
		t.Fatalf("unmapped patterns = %d, want 2 (route.skills_sh.thing, genericword)", report.UnmappedPatterns)
	}
}

// TestConvertAcceptanceV1Deterministic falsifies v1-mode nondeterminism: two v1 conversions of the
// same input produce byte-identical suites AND identical accounting.
func TestConvertAcceptanceV1Deterministic(t *testing.T) {
	suiteA, reportA, err := ConvertAcceptance([]byte(syntheticUnmappedFamiliesJSON), "syn-unmapped", IDMapV1)
	if err != nil {
		t.Fatalf("convert a: %v", err)
	}
	suiteB, reportB, err := ConvertAcceptance([]byte(syntheticUnmappedFamiliesJSON), "syn-unmapped", IDMapV1)
	if err != nil {
		t.Fatalf("convert b: %v", err)
	}
	ab, _ := MarshalSuite(suiteA)
	bb, _ := MarshalSuite(suiteB)
	if string(ab) != string(bb) {
		t.Fatalf("v1 convert not byte-deterministic")
	}
	if !reflect.DeepEqual(reportA, reportB) {
		t.Fatalf("v1 id-map report not deterministic: %+v vs %+v", reportA, reportB)
	}
}

// --- v2 id-map falsifiers --------------------------------------------------------------------

// syntheticV2ShapesJSON exercises both new v2-only generic shapes (skill.codex.skills.<rest>,
// skill.codex.superpowers.<rest>) plus every literal override in v2LiteralOverrides, one id per
// override so each is individually falsifiable. "skill.agents.shared.write.a.skill" is included
// as a control: it is NOT in v2LiteralOverrides, yet the pre-existing v1 skill.agents.shared shape
// ALREADY derives its correct compound id (skill:write-a-skill) — proving the real-home absence of
// that id is a capgraph importer bug (see the disposition report), not an id-map gap, so v2
// deliberately adds no override for it. "memory.example" proves a genuinely unmapped wont_map-family
// id (no v1 or v2 shape reaches "memory.") still passes through raw and gets tallied, unchanged by
// the v2 addition.
const syntheticV2ShapesJSON = `{
  "schema_version": 1,
  "class_cases": [],
  "cases": [
    {
      "id": "case-codex-shapes",
      "prompt": "codex shapes prompt",
      "required_anywhere": [
        "skill.codex.skills.example.widget",
        "skill.codex.superpowers.example.gadget"
      ]
    },
    {
      "id": "case-literal-overrides",
      "prompt": "literal overrides prompt",
      "required_anywhere": [
        "skill.agents.shared.superpowers",
        "skill.agents.shared.skill.creator",
        "skill.agents.shared.taste.skill",
        "skill.codex.skills.diff.cleanup.and.parking",
        "skill.codex.superpowers.finishing.a.development.branch",
        "tool.mcporter"
      ]
    },
    {
      "id": "case-write-a-skill-already-correct-under-v1-shape",
      "prompt": "write a skill prompt",
      "required_anywhere": ["skill.agents.shared.write.a.skill"]
    },
    {
      "id": "case-still-unmapped",
      "prompt": "memory family prompt",
      "required_anywhere": ["memory.example"]
    }
  ]
}`

// TestConvertAcceptanceV2MapsCodexSkillShapesAndLiterals falsifies a neutered v2 map: both new
// generic codex-skill shapes translate (dots hyphenated, "skill.codex.skills"/"skill.codex.
// superpowers" root dropped entirely — NOT rewritten to a prefixed skill:codex:... id), every
// v2LiteralOverrides entry maps to its exact documented target, the pre-existing v1
// skill.agents.shared shape already derives the correct id for write-a-skill with no v2 addition
// needed, and an id from a genuinely unmapped family (memory.*) stays raw and tallied.
func TestConvertAcceptanceV2MapsCodexSkillShapesAndLiterals(t *testing.T) {
	suite, report, err := ConvertAcceptance([]byte(syntheticV2ShapesJSON), "syn-v2-shapes", IDMapV2)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if report.Version != IDMapV2 {
		t.Fatalf("report version = %q, want %q", report.Version, IDMapV2)
	}

	byID := map[string]Case{}
	for _, c := range suite.Cases {
		byID[c.ID] = c
	}

	wantCodexShapes := []string{"skill:example-gadget", "skill:example-widget"}
	if got := byID["case-codex-shapes"].Expected; !reflect.DeepEqual(got, wantCodexShapes) {
		t.Fatalf("case-codex-shapes expected = %v, want %v", got, wantCodexShapes)
	}

	wantLiterals := []string{
		"adapter:mcporter",
		"skill:agents:skill-creator",
		"skill:design-taste-frontend",
		"skill:diff-cleanup-and-parking",
		"skill:finishing-a-development-branch",
		"skill:using-superpowers",
	}
	if got := byID["case-literal-overrides"].Expected; !reflect.DeepEqual(got, wantLiterals) {
		t.Fatalf("case-literal-overrides expected = %v, want %v", got, wantLiterals)
	}

	wantWriteASkill := []string{"skill:write-a-skill"}
	if got := byID["case-write-a-skill-already-correct-under-v1-shape"].Expected; !reflect.DeepEqual(got, wantWriteASkill) {
		t.Fatalf("write-a-skill case expected = %v, want %v (the v1 shape alone already derives the correct id; the real-home absence is a capgraph importer bug, not an id-map gap)", got, wantWriteASkill)
	}

	wantStillUnmapped := []string{"memory.example"}
	if got := byID["case-still-unmapped"].Expected; !reflect.DeepEqual(got, wantStillUnmapped) {
		t.Fatalf("case-still-unmapped expected = %v, want raw passthrough %v", got, wantStillUnmapped)
	}
	foundMemoryFamily := false
	for _, d := range report.UnmappedIDFamilies {
		if d.Field == "memory" {
			foundMemoryFamily = true
		}
	}
	if !foundMemoryFamily {
		t.Fatalf("unmapped id families = %+v, want memory tallied for the still-unmapped case", report.UnmappedIDFamilies)
	}
}

// TestConvertAcceptanceV1ByteStableAcrossV2Addition is the v1-stability falsifier the packet
// requires: converting the SAME input that has v2-only overrides, under IDMapV1 explicitly, must
// still produce the pre-v2 v1 shape (tool.mcporter -> the generic tool:/mcp: pair, NOT
// adapter:mcporter; skill.agents.shared.superpowers -> skill:superpowers, NOT
// skill:using-superpowers). Adding v2 must never change what v1 selects.
func TestConvertAcceptanceV1ByteStableAcrossV2Addition(t *testing.T) {
	suite, report, err := ConvertAcceptance([]byte(syntheticV2ShapesJSON), "syn-v2-shapes", IDMapV1)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if report.Version != IDMapV1 {
		t.Fatalf("report version = %q, want %q", report.Version, IDMapV1)
	}

	byID := map[string]Case{}
	for _, c := range suite.Cases {
		byID[c.ID] = c
	}

	// Neither codex-skill shape is a v1 shape, so both stay raw under v1.
	wantCodexShapes := []string{"skill.codex.skills.example.widget", "skill.codex.superpowers.example.gadget"}
	if got := byID["case-codex-shapes"].Expected; !reflect.DeepEqual(got, wantCodexShapes) {
		t.Fatalf("v1 case-codex-shapes expected = %v, want raw passthrough %v", got, wantCodexShapes)
	}

	// v1's generic shapes fire (skill.agents.shared.*, tool.*) but WITHOUT any v2 literal override:
	// superpowers/skill.creator/taste.skill hyphenate naively, tool.mcporter still pair-expands.
	wantLiterals := []string{
		"mcp:mcporter",
		"skill:skill-creator",
		"skill:superpowers",
		"skill:taste-skill",
		"skill.codex.skills.diff.cleanup.and.parking",            // not a v1 shape, stays raw
		"skill.codex.superpowers.finishing.a.development.branch", // not a v1 shape, stays raw
		"tool:mcporter",
	}
	sort.Strings(wantLiterals)
	if got := byID["case-literal-overrides"].Expected; !reflect.DeepEqual(got, wantLiterals) {
		t.Fatalf("v1 case-literal-overrides expected = %v, want %v (v2 overrides must not leak into v1)", got, wantLiterals)
	}
}

// TestConvertAcceptanceV2Deterministic falsifies v2-mode nondeterminism: two v2 conversions of the
// same input produce byte-identical suites AND identical accounting (mirrors
// TestConvertAcceptanceV1Deterministic for the new default version).
func TestConvertAcceptanceV2Deterministic(t *testing.T) {
	suiteA, reportA, err := ConvertAcceptance([]byte(syntheticV2ShapesJSON), "syn-v2-shapes", IDMapV2)
	if err != nil {
		t.Fatalf("convert a: %v", err)
	}
	suiteB, reportB, err := ConvertAcceptance([]byte(syntheticV2ShapesJSON), "syn-v2-shapes", IDMapV2)
	if err != nil {
		t.Fatalf("convert b: %v", err)
	}
	ab, _ := MarshalSuite(suiteA)
	bb, _ := MarshalSuite(suiteB)
	if string(ab) != string(bb) {
		t.Fatalf("v2 convert not byte-deterministic")
	}
	if !reflect.DeepEqual(reportA, reportB) {
		t.Fatalf("v2 id-map report not deterministic: %+v vs %+v", reportA, reportB)
	}
}

const syntheticGoldenYAML = `schema_version: compound.memory_bakeoff_queries.v0.1
queries:
  - id: q-zeta
    lane: exact
    query: synthetic memory query zeta widget
    expected_claim_ids:
      - claim_zeta_two
      - claim_zeta_one
      - claim_zeta_one
  - id: q-alpha
    lane: casual
    query: synthetic memory query alpha gadget
    expected_claim_ids:
      - claim_alpha
`

// TestConvertGoldenProducesMemoryCases falsifies the golden path: bakeoff-schema YAML converts to
// valid, deterministic memory cases (sorted by id, deduped+sorted expected claim ids, lane
// preserved) that pass suite validation.
func TestConvertGoldenProducesMemoryCases(t *testing.T) {
	suite, err := ConvertGolden([]byte(syntheticGoldenYAML), "syn-golden")
	if err != nil {
		t.Fatalf("convert golden: %v", err)
	}
	if len(suite.Cases) != 2 {
		t.Fatalf("cases = %d, want 2", len(suite.Cases))
	}
	if suite.Cases[0].ID != "q-alpha" || suite.Cases[1].ID != "q-zeta" {
		t.Fatalf("cases not sorted by id: %s, %s", suite.Cases[0].ID, suite.Cases[1].ID)
	}
	zeta := suite.Cases[1]
	if zeta.Kind != KindMemory {
		t.Fatalf("kind = %q, want memory", zeta.Kind)
	}
	if zeta.Lane != "exact" || zeta.Task != "synthetic memory query zeta widget" {
		t.Fatalf("zeta lane/task = %q/%q", zeta.Lane, zeta.Task)
	}
	if !reflect.DeepEqual(zeta.Expected, []string{"claim_zeta_one", "claim_zeta_two"}) {
		t.Fatalf("zeta expected = %v, want deduped+sorted [claim_zeta_one claim_zeta_two]", zeta.Expected)
	}
	if suite.Dropped != nil {
		t.Fatalf("golden conversion drops nothing, got %+v", suite.Dropped)
	}

	// deterministic bytes
	a, _ := MarshalSuite(suite)
	s2, _ := ConvertGolden([]byte(syntheticGoldenYAML), "syn-golden")
	b, _ := MarshalSuite(s2)
	if string(a) != string(b) {
		t.Fatalf("golden convert not byte-deterministic")
	}
}

// TestConvertRejectsWrongSchema falsifies silent acceptance of a mismatched source: a golden file
// with the acceptance schema_version (and vice versa) must error, not produce a garbage suite.
func TestConvertRejectsWrongSchema(t *testing.T) {
	if _, err := ConvertGolden([]byte(syntheticAcceptanceJSON), "x"); err == nil {
		t.Fatal("expected golden convert of acceptance json to fail on schema_version")
	}
	if _, _, err := ConvertAcceptance([]byte(`{"schema_version":2,"cases":[]}`), "x", IDMapV1); err == nil {
		t.Fatal("expected acceptance convert to reject schema_version 2")
	}
}
