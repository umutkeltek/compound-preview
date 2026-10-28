package unity

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRenderRequiresIdeaCandidate(t *testing.T) {
	_, err := Render(SourceHints{}, time.Now())
	if err == nil {
		t.Fatalf("expected error when IdeaCandidate is empty")
	}
}

func TestRenderHonorsHints(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	bp, err := Render(SourceHints{
		IdeaCandidate:   "hypothesis_draft_test-game",
		Title:           "Test Game",
		Hook:            "A test game.",
		UnityVersion:    "2023.2.0f1",
		RenderPipeline:  "HDRP",
		ProjectRootHint: "/tmp/projects/test-game",
	}, now)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if bp.UnityVersion != "2023.2.0f1" {
		t.Errorf("unity_version: %q", bp.UnityVersion)
	}
	if bp.RenderPipeline != "HDRP" {
		t.Errorf("render_pipeline: %q", bp.RenderPipeline)
	}
	if bp.ProjectRoot != "/tmp/projects/test-game" {
		t.Errorf("project_root: %q", bp.ProjectRoot)
	}
	if !bp.Generated.Equal(now) {
		t.Errorf("generated_at not pinned: got %v want %v", bp.Generated, now)
	}
	if bp.Source.Title != "Test Game" {
		t.Errorf("source.title: %q", bp.Source.Title)
	}
	if bp.Source.IdeaCandidate != "hypothesis_draft_test-game" {
		t.Errorf("source.idea_candidate: %q", bp.Source.IdeaCandidate)
	}
}

func TestRenderDefaultsUnityVersionAndPipeline(t *testing.T) {
	bp, err := Render(SourceHints{IdeaCandidate: "x"}, time.Now())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if bp.UnityVersion != DefaultUnityVersion {
		t.Errorf("expected default %q, got %q", DefaultUnityVersion, bp.UnityVersion)
	}
	if bp.RenderPipeline != DefaultRenderPipeline {
		t.Errorf("expected default %q, got %q", DefaultRenderPipeline, bp.RenderPipeline)
	}
}

func TestRenderPipelineURPSelectsCorrectPackage(t *testing.T) {
	bp, _ := Render(SourceHints{IdeaCandidate: "x", RenderPipeline: "URP"}, time.Now())
	if _, ok := bp.Packages.Dependencies["com.unity.render-pipelines.universal"]; !ok {
		t.Errorf("URP package missing: %+v", bp.Packages.Dependencies)
	}
}

func TestRenderPipelineHDRPSelectsCorrectPackage(t *testing.T) {
	bp, _ := Render(SourceHints{IdeaCandidate: "x", RenderPipeline: "HDRP"}, time.Now())
	if _, ok := bp.Packages.Dependencies["com.unity.render-pipelines.high-definition"]; !ok {
		t.Errorf("HDRP package missing: %+v", bp.Packages.Dependencies)
	}
}

func TestRenderEmitsBoundaryStringsVerbatim(t *testing.T) {
	bp, _ := Render(SourceHints{IdeaCandidate: "x"}, time.Now())
	for _, want := range []string{
		BoundaryNoDiskWrites,
		BoundaryMaterializeGated,
		BoundaryNoEditorBinary,
	} {
		found := false
		for _, got := range bp.Boundary {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("boundary string missing verbatim: %q\nhave: %v", want, bp.Boundary)
		}
	}
}

func TestBlueprintJSONContainsBoundaryStrings(t *testing.T) {
	bp, _ := Render(SourceHints{IdeaCandidate: "x"}, time.Now())
	raw, err := json.Marshal(bp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		BoundaryNoDiskWrites,
		BoundaryMaterializeGated,
		BoundaryNoEditorBinary,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("JSON missing boundary string: %q", want)
		}
	}
}

func TestProjectSlug(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Test Game", "test-game"},
		{"!!!", "game-e84c538e"}, // punctuation-only → hash fallback (deterministic from input)
		{"Co-Op Roguelike!", "co-op-roguelike"},
	}
	for _, c := range cases {
		got := projectSlug(SourceHints{Title: c.in, IdeaCandidate: "x"})
		if got != c.want {
			t.Errorf("projectSlug(title=%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestProjectSlugEmptyTitleFallsBackToCandidate(t *testing.T) {
	got := projectSlug(SourceHints{IdeaCandidate: "hypothesis_draft_my-game"})
	if got != "hypothesis-draft-my-game" {
		t.Errorf("got %q want hypothesis-draft-my-game", got)
	}
}

func TestProjectSlugFallbackDeterministic(t *testing.T) {
	a := projectSlug(SourceHints{Title: "!!!"})
	b := projectSlug(SourceHints{Title: "!!!"})
	if a != b {
		t.Errorf("slug should be deterministic: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "game-") {
		t.Errorf("expected game-* prefix, got %q", a)
	}
}
