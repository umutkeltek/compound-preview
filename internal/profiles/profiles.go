// Package profiles renders harness agent profiles from public-safe role
// definitions plus an optional private overlay.
package profiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const GenMarker = "compound-profile: generated"

type Harness string

const (
	HarnessClaude Harness = "claude"
	HarnessCodex  Harness = "codex"
)

type Role struct {
	ID              string
	Family          string
	Description     string
	CapabilityClass string
	QualityTier     string
	Independence    bool
	Tools           string
	Emit            []Harness
	Body            string
	Models          map[Harness]string
}

type Catalog struct {
	Roles         []Role
	OverlayPath   string
	OverlayLoaded bool
}

type LoadOptions struct {
	// OverlayPaths nil means use the default search path. An empty non-nil slice
	// disables overlays for public-core tests and fresh-checkout rendering.
	OverlayPaths []string
}

type Targets map[Harness]string

type SyncOptions struct {
	Check         bool
	InstalledOnly bool
}

type SyncReport struct {
	Check     bool
	Drift     int
	RoleCount int
	Summary   []string
}

type Overlay struct {
	Roles           []RoleOverlay `json:"roles,omitempty"`
	Lanes           []LaneOverlay `json:"lanes,omitempty"`
	PreferenceOrder []string      `json:"preference_order,omitempty"`
}

type RoleOverlay struct {
	ID              string             `json:"id"`
	Family          *string            `json:"family,omitempty"`
	Description     *string            `json:"description,omitempty"`
	CapabilityClass *string            `json:"capability_class,omitempty"`
	QualityTier     *string            `json:"quality_tier,omitempty"`
	Independence    *bool              `json:"independence,omitempty"`
	Tools           *string            `json:"tools,omitempty"`
	Emit            []Harness          `json:"emit,omitempty"`
	Body            *string            `json:"body,omitempty"`
	Models          map[Harness]string `json:"models,omitempty"`
}

type LaneOverlay struct {
	ID        string `json:"id"`
	Harness   string `json:"harness"`
	Route     string `json:"route"`
	CostClass string `json:"cost_class"`
	Note      string `json:"note,omitempty"`
}

func LoadCatalog() (Catalog, error) {
	return LoadCatalogWithOptions(LoadOptions{})
}

func LoadCatalogWithOptions(opts LoadOptions) (Catalog, error) {
	roles := DefaultRoles()
	paths := opts.OverlayPaths
	if paths == nil {
		paths = DefaultOverlayPaths()
	}
	for _, path := range paths {
		overlay, ok, err := loadOverlay(path)
		if err != nil {
			return Catalog{}, err
		}
		if !ok {
			continue
		}
		roles, err = ApplyOverlay(roles, overlay)
		if err != nil {
			return Catalog{}, fmt.Errorf("apply profile overlay %s: %w", path, err)
		}
		return Catalog{Roles: roles, OverlayPath: path, OverlayLoaded: true}, nil
	}
	return Catalog{Roles: roles}, nil
}

func DefaultOverlayPaths() []string {
	var paths []string
	if explicit := strings.TrimSpace(os.Getenv("COMPOUND_PROFILES_OVERLAY")); explicit != "" {
		return []string{explicit}
	}
	if compoundHome := strings.TrimSpace(os.Getenv("COMPOUND_HOME")); compoundHome != "" {
		paths = append(paths, filepath.Join(compoundHome, "profiles", "overlay.json"))
	}
	if repoOverlay := findRepoOverlay(); repoOverlay != "" {
		paths = append(paths, repoOverlay)
	}
	return paths
}

func TargetsForHome(homeDir string) Targets {
	return Targets{
		HarnessClaude: filepath.Join(homeDir, ".claude", "agents"),
		HarnessCodex:  filepath.Join(homeDir, ".codex", "agents"),
	}
}

func Render(role Role, harness Harness) string {
	return fmt.Sprintf("%s\n\n%s%s\n", frontmatter(role, harness), strings.TrimSpace(role.Body), laneFooter(role))
}

func Sync(roles []Role, targets Targets, opts SyncOptions) (SyncReport, error) {
	report := SyncReport{Check: opts.Check, RoleCount: len(roles)}
	for _, harness := range []Harness{HarnessClaude, HarnessCodex} {
		dir := targets[harness]
		if strings.TrimSpace(dir) == "" {
			return report, fmt.Errorf("missing target dir for %s", harness)
		}
		if !opts.Check {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return report, fmt.Errorf("create %s agents dir: %w", harness, err)
			}
		}
		owned := ownedIDs(roles, harness)
		for _, role := range roles {
			if !emits(role, harness) {
				continue
			}
			path := filepath.Join(dir, role.ID+".md")
			want := Render(role, harness)
			have, exists, err := readString(path)
			if err != nil {
				return report, err
			}
			if opts.Check && opts.InstalledOnly && !exists {
				report.Summary = append(report.Summary, fmt.Sprintf("skip %s/%s not installed", harness, role.ID))
				continue
			}
			if exists && !strings.Contains(have, GenMarker) {
				report.Summary = append(report.Summary, fmt.Sprintf("SKIP %s/%s — exists and hand-written (no marker); not overwriting", harness, role.ID))
				continue
			}
			if exists && have == want {
				report.Summary = append(report.Summary, fmt.Sprintf("ok %s/%s", harness, role.ID))
				continue
			}
			report.Drift++
			if opts.Check {
				report.Summary = append(report.Summary, fmt.Sprintf("DRIFT %s/%s — generated file differs from source", harness, role.ID))
				continue
			}
			if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
				return report, fmt.Errorf("write %s: %w", path, err)
			}
			report.Summary = append(report.Summary, fmt.Sprintf("write %s/%s", harness, role.ID))
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return report, fmt.Errorf("read %s agents dir: %w", harness, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".md")
			if owned[id] {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			have, exists, err := readString(path)
			if err != nil {
				return report, err
			}
			if !exists || !strings.Contains(have, GenMarker) {
				continue
			}
			report.Drift++
			if opts.Check {
				report.Summary = append(report.Summary, fmt.Sprintf("STALE %s/%s — generated file has no source role", harness, id))
				continue
			}
			if err := os.Remove(path); err != nil {
				return report, fmt.Errorf("prune %s: %w", path, err)
			}
			report.Summary = append(report.Summary, fmt.Sprintf("prune %s/%s", harness, id))
		}
	}
	return report, nil
}

func FinalLine(report SyncReport) string {
	if report.Check {
		if report.Drift == 0 {
			return "IN SYNC — no drift"
		}
		return fmt.Sprintf("DRIFT: %d file(s) out of sync. Run `compound profiles generate`.", report.Drift)
	}
	return fmt.Sprintf("Generated %d roles → ~/.claude/agents + ~/.codex/agents.", report.RoleCount)
}

func ApplyOverlay(base []Role, overlay Overlay) ([]Role, error) {
	roles := cloneRoles(base)
	index := make(map[string]int, len(roles))
	for i, role := range roles {
		index[role.ID] = i
	}
	for _, patch := range overlay.Roles {
		id := strings.TrimSpace(patch.ID)
		if id == "" {
			return nil, fmt.Errorf("overlay role missing id")
		}
		if i, ok := index[id]; ok {
			applyRolePatch(&roles[i], patch)
			if err := validateRole(roles[i]); err != nil {
				return nil, err
			}
			continue
		}
		role := Role{ID: id}
		applyRolePatch(&role, patch)
		if err := validateRole(role); err != nil {
			return nil, err
		}
		index[id] = len(roles)
		roles = append(roles, role)
	}
	return roles, nil
}

func frontmatter(role Role, harness Harness) string {
	lines := []string{
		"---",
		"name: " + role.ID,
		"description: " + role.Description,
		"model: " + modelFor(role, harness),
	}
	if role.Tools != "" {
		lines = append(lines, "tools: "+role.Tools)
	}
	lines = append(lines, "---")
	return strings.Join(lines, "\n")
}

func laneFooter(role Role) string {
	independence := ""
	if role.Independence {
		independence = "\n- INDEPENDENCE: you must run on a provider different from the driver. A model judging its own output is not a second opinion."
	}
	return fmt.Sprintf(`

---
<!-- %s; family=%s; class=%s; tier=%s; source=docs_private/profiles/registry.ts. Edit the source + `+"`"+`bun generate.ts`+"`"+`, never this file. -->

**Lane (adaptive — do not hardcode a model):** capability class `+"`"+`%s`+"`"+`, quality tier `+"`"+`%s`+"`"+`.
- Resolve the model via `+"`"+`compound route plan --capability-class %s`+"`"+` (authority) — it prefers subscription-fixed (already-paid) lanes first. (`+"`"+`os route %s`+"`"+` is the compatibility fallback while operating-system/ folds into compound.)
- In a paid harness, use the best already-paid model for this tier; never sit idle behind a worse free model.
- Drop to a free/api-marginal lane ONLY when it does not cost quality (bulk privacy-safe recon, independent judging).%s`,
		GenMarker,
		role.Family,
		role.CapabilityClass,
		role.QualityTier,
		role.CapabilityClass,
		role.QualityTier,
		role.CapabilityClass,
		role.CapabilityClass,
		independence,
	)
}

func modelFor(role Role, harness Harness) string {
	if role.Models != nil {
		if model := strings.TrimSpace(role.Models[harness]); model != "" {
			return model
		}
	}
	return "inherit"
}

func ownedIDs(roles []Role, harness Harness) map[string]bool {
	owned := make(map[string]bool)
	for _, role := range roles {
		if emits(role, harness) {
			owned[role.ID] = true
		}
	}
	return owned
}

func emits(role Role, harness Harness) bool {
	for _, emit := range role.Emit {
		if emit == harness {
			return true
		}
	}
	return false
}

func readString(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	return string(data), true, nil
}

func loadOverlay(path string) (Overlay, bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Overlay{}, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Overlay{}, false, nil
		}
		return Overlay{}, false, fmt.Errorf("read profile overlay %s: %w", path, err)
	}
	var overlay Overlay
	if err := json.Unmarshal(data, &overlay); err != nil {
		return Overlay{}, false, fmt.Errorf("parse profile overlay %s: %w", path, err)
	}
	return overlay, true, nil
}

func findRepoOverlay() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, "docs_private", "profiles", "overlay.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func applyRolePatch(role *Role, patch RoleOverlay) {
	if patch.Family != nil {
		role.Family = strings.TrimSpace(*patch.Family)
	}
	if patch.Description != nil {
		role.Description = strings.TrimSpace(*patch.Description)
	}
	if patch.CapabilityClass != nil {
		role.CapabilityClass = strings.TrimSpace(*patch.CapabilityClass)
	}
	if patch.QualityTier != nil {
		role.QualityTier = strings.TrimSpace(*patch.QualityTier)
	}
	if patch.Independence != nil {
		role.Independence = *patch.Independence
	}
	if patch.Tools != nil {
		role.Tools = strings.TrimSpace(*patch.Tools)
	}
	if len(patch.Emit) > 0 {
		role.Emit = append([]Harness(nil), patch.Emit...)
	}
	if patch.Body != nil {
		role.Body = strings.TrimSpace(*patch.Body)
	}
	if len(patch.Models) > 0 {
		role.Models = make(map[Harness]string, len(patch.Models))
		for harness, model := range patch.Models {
			role.Models[harness] = strings.TrimSpace(model)
		}
	}
}

func validateRole(role Role) error {
	if strings.TrimSpace(role.ID) == "" {
		return fmt.Errorf("profile role missing id")
	}
	if strings.TrimSpace(role.Family) == "" {
		return fmt.Errorf("profile role %s missing family", role.ID)
	}
	if strings.TrimSpace(role.Description) == "" {
		return fmt.Errorf("profile role %s missing description", role.ID)
	}
	if strings.TrimSpace(role.CapabilityClass) == "" {
		return fmt.Errorf("profile role %s missing capability_class", role.ID)
	}
	if strings.TrimSpace(role.QualityTier) == "" {
		return fmt.Errorf("profile role %s missing quality_tier", role.ID)
	}
	if len(role.Emit) == 0 {
		return fmt.Errorf("profile role %s emits no harnesses", role.ID)
	}
	for _, harness := range role.Emit {
		if harness != HarnessClaude && harness != HarnessCodex {
			return fmt.Errorf("profile role %s emits unknown harness %q", role.ID, harness)
		}
	}
	if strings.TrimSpace(role.Body) == "" {
		return fmt.Errorf("profile role %s missing body", role.ID)
	}
	return nil
}

func cloneRoles(in []Role) []Role {
	out := make([]Role, len(in))
	for i, role := range in {
		out[i] = role
		out[i].Emit = append([]Harness(nil), role.Emit...)
		if role.Models != nil {
			out[i].Models = make(map[Harness]string, len(role.Models))
			for harness, model := range role.Models {
				out[i].Models[harness] = model
			}
		}
	}
	return out
}
