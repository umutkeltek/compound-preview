package sourceobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/candidates"
)

// InventoryIntakeSchemaVersion is the door's input contract version. The project-miner inventory
// writer (project_miner.py command_inventory) emits this shape; the door translates each per-repo
// inventory row into a Compound candidate kind and parks every one no-promote. It is the standing
// census sibling of the change-stream door: change-stream carries scored drift signals, this
// carries the full ~/Projects repo census.
const InventoryIntakeSchemaVersion = "compound.project_inventory_intake.v0.1"

// InventoryIntake is the door input: a reference to the raw project-miner inventory.json (the
// original artifact the door content-addresses and preserves — keep-all-data source layer) plus the
// per-repo rows the miner discovered (the derived layer parked no-promote above the source).
//
// GeneratedAt is carried for provenance but is DELIBERATELY excluded from the content hash: two
// runs over an unchanged workspace differ only in their wall-clock timestamps, and the door must
// treat them as the same census (content-addressed idempotency). This is the deliberate difference
// from the change-stream door, whose raw capture body embeds a timestamp and so re-hashes per run.
type InventoryIntake struct {
	SchemaVersion string          `json:"schema_version"`
	SourceCapture string          `json:"source_capture"` // absolute path to the raw inventory.json
	GeneratedAt   string          `json:"generated_at"`   // provenance only; excluded from the content hash
	Projects      []InventoryRepo `json:"projects"`
}

// InventoryRepo is one discovered repo/project row. Every field here is meaningful inventory
// content and feeds the stable content hash. The volatile per-row discovery timestamp
// (discovered_at) is intentionally NOT a field: it is noise for idempotency, and the exact raw row
// (timestamp included) is preserved verbatim in the content-addressed body.
type InventoryRepo struct {
	Slug            string   `json:"slug"`
	Name            string   `json:"name"`
	Domain          string   `json:"domain"`
	Path            string   `json:"path"`
	RelativePath    string   `json:"relative_path"`
	Git             bool     `json:"git"`
	Branch          string   `json:"branch"`
	Dirty           *bool    `json:"dirty"`
	RecentCommit    string   `json:"recent_commit"`
	MarkerFiles     []string `json:"marker_files"`
	ProjectStrength string   `json:"project_strength"`
}

// InventoryContentSHA is the exported content address of an inventory intake's rows — the stable
// hash the door uses for the observation id. Callers outside the package (the CLI door body) use it
// so the idempotency contract lives in one place.
func InventoryContentSHA(intake InventoryIntake) string {
	return inventoryStableSHA(intake.Projects)
}

// inventoryStableSHA is the content address of one inventory census. It canonicalizes the rows
// (sorted by relative_path, marker lists sorted) and hashes the meaningful content only — NOT the
// intake's generated_at nor any per-row discovery timestamp. Identical workspace state therefore
// yields an identical hash across runs, so the observation id is stable and an idempotent re-run
// overwrites the same record instead of spraying duplicates.
func inventoryStableSHA(projects []InventoryRepo) string {
	norm := make([]InventoryRepo, len(projects))
	copy(norm, projects)
	for i := range norm {
		m := append([]string(nil), norm[i].MarkerFiles...)
		sort.Strings(m)
		norm[i].MarkerFiles = m
	}
	sort.Slice(norm, func(a, b int) bool {
		if norm[a].RelativePath != norm[b].RelativePath {
			return norm[a].RelativePath < norm[b].RelativePath
		}
		return norm[a].Path < norm[b].Path
	})
	// json.Marshal of a slice of structs is deterministic (fixed field order, no maps), so the
	// canonical bytes are stable for stable content.
	b, _ := json.Marshal(norm)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// inventoryRowSHA is the content address of one inventory row. It applies the same marker-list
// canonicalization as inventoryStableSHA and excludes intake timestamps by operating on the row.
func inventoryRowSHA(r InventoryRepo) string {
	markers := append([]string(nil), r.MarkerFiles...)
	sort.Strings(markers)
	r.MarkerFiles = markers
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// inventoryEntityHint produces a per-row hint that is (a) non-empty (Validate requires it),
// (b) stable for a given row so an idempotent re-run overwrites the same candidate, and
// (c) unique across distinct rows so two projects do not collide onto one candidate id. The base is
// the row's relative_path (already unique per repo); the trailing content hash keeps a repo whose
// state changed distinct from its prior state (mirrors the change-stream door).
func inventoryEntityHint(r InventoryRepo) string {
	base := strings.TrimSpace(r.RelativePath)
	if base == "" {
		base = strings.TrimSpace(r.Slug)
	}
	if base == "" {
		base = strings.TrimSpace(r.Path)
	}
	if base == "" {
		base = "project-inventory"
	}
	dirty := "nil"
	if r.Dirty != nil {
		dirty = strconv.FormatBool(*r.Dirty)
	}
	markers := append([]string(nil), r.MarkerFiles...)
	sort.Strings(markers)
	disc := shortSHA(
		r.RelativePath, r.Slug, r.Domain, strconv.FormatBool(r.Git),
		r.Branch, dirty, r.RecentCommit, r.ProjectStrength, strings.Join(markers, ","),
	)
	return base + "#" + disc
}

// FromInventory builds the SourceObservation for one project-miner inventory census. contentSHA is
// the stable content address of the rows (see inventoryStableSHA), while each candidate uses its
// row's content address. bodyRef is the content-addressed artifact-store ref of the
// preserved raw inventory.json (the keep-all-data original layer). Every row maps to a no-promote
// product_hypothesis candidate: the inventory is portfolio evidence awaiting operator adjudication
// (its promotion path is `compound product hypothesis-plan`), never promoted truth. The door OWNS
// the row -> candidate translation so the Python miner never has to know Compound's inbox taxonomy.
func FromInventory(intake InventoryIntake, contentSHA, bodyRef string, now time.Time) (SourceObservation, error) {
	capturePath := strings.TrimSpace(intake.SourceCapture)
	if capturePath == "" {
		return SourceObservation{}, fmt.Errorf("inventory intake: source_capture required")
	}
	if strings.TrimSpace(contentSHA) == "" {
		return SourceObservation{}, fmt.Errorf("inventory intake: content sha required")
	}
	if len(intake.Projects) == 0 {
		return SourceObservation{}, fmt.Errorf("inventory intake: no projects to map (preserve the body without an observation)")
	}
	maps := make([]MapTarget, 0, len(intake.Projects))
	for i, r := range intake.Projects {
		if strings.TrimSpace(r.RelativePath) == "" && strings.TrimSpace(r.Slug) == "" && strings.TrimSpace(r.Path) == "" {
			return SourceObservation{}, fmt.Errorf("inventory project %d: needs a relative_path, slug, or path", i)
		}
		maps = append(maps, MapTarget{
			ObjectFamily:      "product",
			CandidateKind:     string(candidates.ProductHypothesis),
			EntityHint:        inventoryEntityHint(r),
			CandidateRevision: inventoryRowSHA(r),
			PromoteAllowed:    false,
		})
	}
	evidence := []string{"project_inventory_capture:" + contentSHA}
	if bodyRef != "" {
		evidence = append(evidence, bodyRef)
	}
	if gen := strings.TrimSpace(intake.GeneratedAt); gen != "" {
		evidence = append(evidence, "generated_at:"+gen)
	}
	return SourceObservation{
		SchemaVersion: SchemaVersion,
		ID:            idFor("srcobs_", "project_inventory", capturePath, contentSHA),
		AdapterID:     "compound.project-inventory",
		SourceSystem:  "project-miner",
		ObservedAt:    rfc3339(now),
		SourceRef: SourceRef{
			Path:        capturePath,
			Kind:        "project_inventory",
			Owner:       "project-miner",
			SourceClass: "observed",
			Revision:    contentSHA,
		},
		MapsTo:       maps,
		EvidenceRefs: evidence,
	}, nil
}
