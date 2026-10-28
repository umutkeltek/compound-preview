// Package unity emits a Unity project blueprint spec from a parked
// KindProductHypothesis candidate. It NEVER writes to disk in v0 — the
// boundary strings below are pinned verbatim in the emitted JSON so
// downstream consumers (and reviewers) can prove the no-write promise.
//
// The full PR-3 surface is `compound game unity scaffold <idea-id>`. It
// reads the candidate row, derives a Unity layout, and prints the spec to
// stdout (or `--json`). A future `compound game unity scaffold
// --materialize` would be the disk-write path; that path is reserved for
// an explicit PR with its own registry admission line.
package unity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Blueprint is the spec emitted by Render. Fields are stable across runs
// against the same idea candidate so consumers can diff blueprints.
type Blueprint struct {
	Schema         string        `json:"schema"`
	UnityVersion   string        `json:"unity_version"`
	RenderPipeline string        `json:"render_pipeline"`
	ProjectRoot    string        `json:"project_root"` // planned path; NEVER materialized in v0
	Layout         Layout        `json:"layout"`
	Packages       Packages      `json:"packages"`
	FirstScene     FirstScene    `json:"first_scene"`
	BuildTargets   []BuildTarget `json:"build_targets"`
	Boundary       []string      `json:"boundary"` // explicit non-writes; verbatim
	Generated      time.Time     `json:"generated_at"`
	Source         Source        `json:"source"`
}

// Layout is the planned on-disk directory layout for the Unity project.
// Every path here is descriptive — none of these are created by Render.
type Layout struct {
	Assets          []string `json:"assets"`
	ProjectSettings []string `json:"project_settings"`
	Packages        []string `json:"packages"`
	Ignore          []string `json:"ignore"`
	RecommendedDirs []string `json:"recommended_dirs"`
}

// Packages captures the planned Unity package manifest. Dependencies are
// advisory; Unity itself resolves them at editor import time.
type Packages struct {
	Dependencies map[string]string `json:"dependencies"`
	Description  string            `json:"description"`
}

// FirstScene is the planned first scene the project would ship with.
type FirstScene struct {
	Path     string `json:"path"`
	Template string `json:"template"` // e.g. "SampleScene.unity"
}

// BuildTarget is one platform the project would target at build time.
type BuildTarget struct {
	Platform  string `json:"platform"`
	OutputDir string `json:"output_dir"`
}

// Source carries provenance — which candidate id and one-pager the
// blueprint was rendered from.
type Source struct {
	IdeaCandidate string `json:"idea_candidate"`
	Title         string `json:"title,omitempty"`
	Hook          string `json:"hook,omitempty"`
}

// Boundary strings, kept verbatim. The emitted Blueprint's Boundary field
// includes all three so consumers can grep for them. The unity_test.go
// test asserts each one is present, byte-exact.
const (
	BoundaryNoDiskWrites     = "this command emits blueprint spec only; does not write any files to disk in v0"
	BoundaryMaterializeGated = "creating Unity project directories requires explicit human approval via --materialize, which is gated FirstPartyCommandGoverned"
	BoundaryNoEditorBinary   = "Unity editor binary path is not consulted, downloaded, or invoked"
)

// SchemaVersion is the versioned schema for the emitted JSON. Bumping
// requires an intentional breaking-change review.
const SchemaVersion = "compound/game-unity-blueprint.v0.1"

// DefaultUnityVersion + DefaultRenderPipeline anchor the spec defaults.
// Override via SourceHints in Render; absent hints fall through to these.
const (
	DefaultUnityVersion   = "2022.3.20f1"
	DefaultRenderPipeline = "URP"
)

// SourceHints is the small set of inputs Render needs from the parked
// candidate. Fields are optional; defaults fill in the rest.
type SourceHints struct {
	IdeaCandidate   string // candidate id (required for traceability)
	Title           string // optional, surfaced in Source for human readers
	Hook            string // optional
	UnityVersion    string // optional; falls through to DefaultUnityVersion
	RenderPipeline  string // optional; falls through to DefaultRenderPipeline
	ProjectRootHint string // optional; falls through to "./<slug>"
}

// Render produces a Blueprint for the given hints. Pure function — no
// filesystem access, no network access, no subprocess. The result is
// schema-stable and contains only the planned layout; nothing is
// materialized.
func Render(hints SourceHints, now time.Time) (Blueprint, error) {
	if strings.TrimSpace(hints.IdeaCandidate) == "" {
		return Blueprint{}, fmt.Errorf("unity.Render: IdeaCandidate required for traceability")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	unityVersion := nonEmptyOr(hints.UnityVersion, DefaultUnityVersion)
	renderPipeline := nonEmptyOr(hints.RenderPipeline, DefaultRenderPipeline)
	projectRoot := nonEmptyOr(hints.ProjectRootHint, defaultProjectRoot(hints))

	return Blueprint{
		Schema:         SchemaVersion,
		UnityVersion:   unityVersion,
		RenderPipeline: renderPipeline,
		ProjectRoot:    projectRoot,
		Layout:         defaultLayout(),
		Packages:       defaultPackages(renderPipeline),
		FirstScene:     defaultFirstScene(),
		BuildTargets:   defaultBuildTargets(),
		Boundary: []string{
			BoundaryNoDiskWrites,
			BoundaryMaterializeGated,
			BoundaryNoEditorBinary,
		},
		Generated: now,
		Source: Source{
			IdeaCandidate: hints.IdeaCandidate,
			Title:         hints.Title,
			Hook:          hints.Hook,
		},
	}, nil
}

func defaultLayout() Layout {
	return Layout{
		Assets: []string{
			"Assets/Scripts/",
			"Assets/Scenes/",
			"Assets/Prefabs/",
			"Assets/Settings/",
		},
		ProjectSettings: []string{
			"ProjectSettings/ProjectVersion.txt",
			"ProjectSettings/EditorBuildSettings.asset",
		},
		Packages: []string{
			"Packages/manifest.json",
		},
		Ignore: []string{
			".gitignore",
			"Library/",
			"Temp/",
			"Logs/",
			"UserSettings/",
		},
		RecommendedDirs: []string{
			"Assets/_Game/Scripts/",
			"Assets/_Game/Scenes/",
			"Assets/_Game/Prefabs/",
		},
	}
}

func defaultPackages(renderPipeline string) Packages {
	deps := map[string]string{
		"com.unity.ide.rider":       "3.0.27",
		"com.unity.test-framework":  "1.1.33",
		"com.unity.visualscripting": "1.9.4",
	}
	switch renderPipeline {
	case "URP":
		deps["com.unity.render-pipelines.universal"] = "14.0.8"
	case "HDRP":
		deps["com.unity.render-pipelines.high-definition"] = "14.0.8"
	}
	return Packages{
		Dependencies: deps,
		Description:  "Recommended Unity package manifest for the planned render pipeline.",
	}
}

func defaultFirstScene() FirstScene {
	return FirstScene{
		Path:     "Assets/_Game/Scenes/Game.unity",
		Template: "SampleScene.unity",
	}
}

func defaultBuildTargets() []BuildTarget {
	return []BuildTarget{
		{Platform: "StandaloneLinux64", OutputDir: "Builds/StandaloneLinux64/"},
		{Platform: "StandaloneWindows64", OutputDir: "Builds/StandaloneWindows64/"},
		{Platform: "StandaloneOSX", OutputDir: "Builds/StandaloneOSX/"},
	}
}

func defaultProjectRoot(hints SourceHints) string {
	slug := projectSlug(hints)
	return "./" + slug
}

func projectSlug(hints SourceHints) string {
	seed := strings.TrimSpace(hints.Title)
	if seed == "" {
		seed = strings.TrimSpace(hints.IdeaCandidate)
	}
	if seed == "" {
		seed = "untitled-game"
	}
	// Slugify: lowercase, ASCII alnum + '-'.
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(seed) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		sum := sha256.Sum256([]byte(seed))
		return "game-" + hex.EncodeToString(sum[:4])
	}
	return out
}

func nonEmptyOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
