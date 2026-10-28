package policy

// Retention apply class table — the small, explicit set of home paths that
// `compound retention apply` knows how to safely trash, plus the hard safety
// floors that refuse to touch anything else.
//
// The packet's settled design reuses the existing retention class model
// (retentionClasses in retention.go) for scan/plan/classes and forbids a
// parallel taxonomy. The apply table here is NOT a parallel taxonomy: the
// retention registry declares what the system ACCUMULATES and who owns each
// shape; the apply table declares what apply knows how to SAFELY TRASH. The
// distinction matters because scan can list an undeclared accumulator, but
// apply can only ever act on a class the operator has explicitly opted into.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"compound/internal/eventstore"
)

// ApplyClass is one path-shape the apply verb knows how to trash.
//
// Each entry is a per-path allow-list: the operator must opt in with
// `--class <id>` for apply to do anything, and even then apply refuses to
// move a file outside this class's PathUnderHome.
type ApplyClass struct {
	// ID is the stable slug the operator passes as `--class`.
	ID string `json:"id"`
	// Description is what an instance actually is, in one line.
	Description string `json:"description"`
	// Owner names the system that creates instances of this class.
	Owner string `json:"owner"`
	// PathUnderHome is the relative directory under $COMPOUND_HOME that this
	// class claims. Apply walks ONLY this directory; nothing outside is
	// reachable, which is what keeps the safety floors honest.
	PathUnderHome string `json:"path_under_home"`
	// AgeFloor is the minimum age a file must reach before any instance is
	// eligible. Hard-coded per class — operators cannot lower it via flags,
	// because a class that allows itself to trash fresh data is the failure
	// this whole verb exists to prevent.
	AgeFloor time.Duration `json:"age_floor"`
	// DefaultOlderThan is the default for `--older-than` when the operator
	// does not pass one. The flag may only raise the bar (delete nothing
	// younger), never lower it below AgeFloor.
	DefaultOlderThan time.Duration `json:"default_older_than"`
}

// applyClasses is the compiled-in allow-list for the apply verb. The IDs and
// paths here are the ONLY paths apply will ever touch under a real home.
var applyClasses = []ApplyClass{
	{
		ID:               "events-segments",
		Description:      "Sealed event segment files under events/segments (rolling append-only log).",
		Owner:            "compound events",
		PathUnderHome:    eventstore.SegmentsRelativeDir(),
		AgeFloor:         30 * 24 * time.Hour,
		DefaultOlderThan: 30 * 24 * time.Hour,
	},
	{
		ID:               "context-packs",
		Description:      "Persisted ContextPack JSON files under context/packs.",
		Owner:            "compound context",
		PathUnderHome:    "context/packs",
		AgeFloor:         30 * 24 * time.Hour,
		DefaultOlderThan: 30 * 24 * time.Hour,
	},
	{
		ID:               "tool-output",
		Description:      "Captured tool-output artifacts under observed/tool-output.",
		Owner:            "compound observed",
		PathUnderHome:    "observed/tool-output",
		AgeFloor:         14 * 24 * time.Hour,
		DefaultOlderThan: 14 * 24 * time.Hour,
	},
	{
		ID:               "delegate-workspaces",
		Description:      "Per-delegation lane workspaces under delegate-workspaces.",
		Owner:            "compound delegation",
		PathUnderHome:    "delegate-workspaces",
		AgeFloor:         14 * 24 * time.Hour,
		DefaultOlderThan: 14 * 24 * time.Hour,
	},
}

// ApplyClassByID returns one apply class, or false when nothing declares that ID.
func ApplyClassByID(id string) (ApplyClass, bool) {
	for _, class := range applyClasses {
		if class.ID == id {
			return class, true
		}
	}
	return ApplyClass{}, false
}

// ApplyClasses returns the declared apply classes in stable ID order.
func ApplyClasses() []ApplyClass {
	out := append([]ApplyClass(nil), applyClasses...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ApplyClassIDs is the canonical, sorted list of apply class IDs.
//
// Convenience for callers (CLI usage, falsifier tests) that just need the
// string set without the rest of the descriptor.
func ApplyClassIDs() []string {
	out := make([]string, 0, len(applyClasses))
	for _, class := range applyClasses {
		out = append(out, class.ID)
	}
	sort.Strings(out)
	return out
}

// forbiddenApplyRoots is the hard-coded list of home roots apply will NEVER
// touch, even if a class ID's PathUnderHome collides with one. The list is
// intentionally redundant with retention.go's operator-gated classes: a future
// maintainer who adds a new class there does not need to remember to update
// this list, because apply does not consult that registry.
//
// Every entry must be a slash-form relative path under COMPOUND_HOME. The
// apply verb checks path containment against this list before any rename.
var forbiddenApplyRoots = []string{
	"ledger",
	"work",
	"run_attempts",
	"contracts",
	"registries",
	"artifacts",
	"private",
	"recovery",
	"secrets",
	"profiles",
	"sources",
	"state",
	"reviews",
	"archives",
	"exports",
	"inbox",
	"config",
	"tmp",
}

// ApplyPlan is the answer to "what would apply move to trash right now?".
//
// The JSON shape deliberately mirrors ReclaimPlan (target/shortfall/steps) so
// downstream tooling that already understands plan output can read apply's
// dry-run report without a second parser.
type ApplyPlan struct {
	AuthorityRef string      `json:"authority_ref"`
	Home         string      `json:"home,omitempty"`
	Classes      []string    `json:"classes"`
	Steps        []ApplyStep `json:"steps"`
	PlannedBytes int64       `json:"planned_bytes"`
	LimitBytes   int64       `json:"limit_bytes,omitempty"`
	OlderThan    string      `json:"older_than"`
	DryRun       bool        `json:"dry_run"`
	AppliedCount int         `json:"applied_count"`
	AppliedBytes int64       `json:"applied_bytes"`
	ReceiptPath  string      `json:"receipt_path,omitempty"`
	Note         string      `json:"note"`
}

// ApplyStep is one trash-move action.
type ApplyStep struct {
	ClassID    string `json:"class_id"`
	SourcePath string `json:"source_path"`
	TrashPath  string `json:"trash_path"`
	Bytes      int64  `json:"bytes"`
	MTime      string `json:"mtime"`
}

// ApplyClassConflictError is returned when the operator's --class list
// references a path that intersects a forbiddenApplyRoots entry.
//
// Distinct type so callers (the CLI, tests) can assert on the exact refusal
// rather than string-matching a generic error.
type ApplyClassConflictError struct {
	ClassID string
	Path    string
	Root    string
}

func (e *ApplyClassConflictError) Error() string {
	return fmt.Sprintf("apply class %q claims path %q which intersects forbidden root %q; refusing to touch authority/evidence paths", e.ClassID, e.Path, e.Root)
}

// UnknownApplyClassError is returned when --class references an id that is
// not in applyClasses.
type UnknownApplyClassError struct {
	ClassID string
}

func (e *UnknownApplyClassError) Error() string {
	return fmt.Sprintf("unknown retention apply class %q; declared: %s", e.ClassID, strings.Join(ApplyClassIDs(), ", "))
}
