package policy

// Editable-surfaces registry (EVOLVE-BLOCK): the policy-owned whitelist of what a
// self-improvement loop may EVER propose edits to. It is the sibling of the autonomy
// kill-switch (autonomy_halt.go): both are read LIVE at the same admission seam, both are
// operator-owned COMPOUND_HOME files (no rebuild, no daemon), and both fail toward safety.
// The kill-switch answers "may the system admit ANY autonomous work right now?"; this
// registry answers "for a system-originated proposal that targets a harness surface, is
// THAT surface one the operator has explicitly, actively opened for edits?".
//
// Two hard guarantees, both fail-closed for system-originated edits:
//  1. A compiled-in HARD-DENY core — policy, permissions, ledger, evaluator — can NEVER be
//     whitelisted. A yaml that lists one is a loud validation ERROR, and while any
//     validation error stands the whole registry is invalid and NOTHING is editable. The
//     evaluator and the policy/permission/ledger spine must sit OUTSIDE any loop that could
//     edit them, or the loop could quietly grade or gate itself.
//  2. An absent or empty config means NOTHING is editable (not "everything"). Opening a
//     surface is an explicit operator act, never a default.
//
// Proposal admission checks declared harness surfaces. Mutation adapters also
// classify actual protected targets; an ordinary label cannot exempt a system
// mutation of a hard-deny core surface.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"compound/internal/envelope"
	homepkg "compound/internal/home"
)

const (
	// EditableSurfacesAuthorityRef names the registry as a policy authority for evidence refs.
	EditableSurfacesAuthorityRef = "compound://registries/editable-surfaces"
	// EditableSurfacesSchema is the on-disk schema tag for the registry file.
	EditableSurfacesSchema   = "compound.editable_surfaces.v0.1"
	editableSurfacesFileName = "editable-surfaces.yaml"

	// EditableSurfaceStatusActive marks a surface currently open for system-originated edits.
	EditableSurfaceStatusActive = "active"
	// EditableSurfaceStatusRetired marks a surface once open but now closed (kept for audit).
	EditableSurfaceStatusRetired = "retired"
)

// hardDenyCoreSurfaces is the compiled-in set that can NEVER be whitelisted. It is not
// configurable on purpose: the policy/permission/ledger spine and the evaluator must sit
// outside any self-improvement loop. Listing one in the yaml is a validation error.
var hardDenyCoreSurfaces = map[string]bool{
	"policy":      true,
	"permissions": true,
	"ledger":      true,
	"evaluator":   true,
}

// HardDenyCoreSurfaces returns the sorted compiled-in surfaces that can never be edited by
// a self-improvement loop, for surfacing in docs, CLI, and tests.
func HardDenyCoreSurfaces() []string {
	out := make([]string, 0, len(hardDenyCoreSurfaces))
	for s := range hardDenyCoreSurfaces {
		out = append(out, s)
	}
	// deterministic order without importing sort in the hot path callers
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// EditableSurface is one whitelist entry: a harness surface a system-originated proposal
// may target, the mutation class allowed against it, and whether it is currently open.
type EditableSurface struct {
	Surface       string `json:"surface"`
	Description   string `json:"description,omitempty"`
	ProposalClass string `json:"proposal_class,omitempty"`
	Status        string `json:"status"`
}

// EditableSurfaces is the loaded registry state. Zero value = nothing editable.
type EditableSurfaces struct {
	Entries          []EditableSurface `json:"entries,omitempty"`
	ValidationErrors []string          `json:"validation_errors,omitempty"`
	Source           string            `json:"source,omitempty"`
	AuthorityRef     string            `json:"authority_ref,omitempty"`
}

// editableSurfacesFile is the YAML projection. Supported writers go through
// WriteEditableSurfaces and the shared admission lock. A manual edit while
// autonomy is available is unsupported; engage the global autonomy halt,
// edit and validate, then clear the halt through its command.
type editableSurfacesFile struct {
	SchemaVersion string                     `yaml:"schema_version,omitempty"`
	Surfaces      []editableSurfaceFileEntry `yaml:"surfaces,omitempty"`
}

type editableSurfaceFileEntry struct {
	Surface       string `yaml:"surface"`
	Description   string `yaml:"description,omitempty"`
	ProposalClass string `yaml:"proposal_class,omitempty"`
	Status        string `yaml:"status,omitempty"`
}

// EditableSurfacesPath is the operator-editable registry file under COMPOUND_HOME/config.
func EditableSurfacesPath(homePath string) string {
	return filepath.Join(homePath, "config", editableSurfacesFileName)
}

// Valid reports whether the loaded registry has zero validation errors. An invalid
// registry makes NOTHING editable (fail-closed): a broken whitelist must not silently
// widen what a self-improvement loop may touch.
func (r EditableSurfaces) Valid() bool { return len(r.ValidationErrors) == 0 }

// ActiveCount returns the number of surfaces currently open for edits (only meaningful
// when the registry is valid).
func (r EditableSurfaces) ActiveCount() int {
	n := 0
	for _, e := range r.Entries {
		if e.Status == EditableSurfaceStatusActive {
			n++
		}
	}
	return n
}

// NothingEditable is true when no surface is editable right now: the registry is invalid,
// or valid with no active entry.
func (r EditableSurfaces) NothingEditable() bool { return !r.Valid() || r.ActiveCount() == 0 }

// Allowed reports whether a system-originated proposal MAY target `surface` right now, and
// a stable greppable reason when it may not. It is deny-by-default: an unknown surface, a
// retired surface, an empty target, or any registry validation error all deny.
func (r EditableSurfaces) Allowed(surface string) (bool, string) {
	surface = strings.TrimSpace(surface)
	if surface == "" {
		return false, "editable_surfaces(no_target): empty target surface"
	}
	if !r.Valid() {
		return false, "editable_surfaces(invalid): registry has validation errors, nothing is editable — " + r.ValidationErrors[0]
	}
	for _, e := range r.Entries {
		if !strings.EqualFold(e.Surface, surface) {
			continue
		}
		switch e.Status {
		case EditableSurfaceStatusActive:
			return true, ""
		case EditableSurfaceStatusRetired:
			return false, fmt.Sprintf("editable_surfaces(retired): surface %q is retired", surface)
		default:
			return false, fmt.Sprintf("editable_surfaces(status=%s): surface %q is not active", e.Status, surface)
		}
	}
	return false, fmt.Sprintf("editable_surfaces(not_whitelisted): surface %q is not in the editable-surfaces whitelist", surface)
}

// LoadEditableSurfaces reads the registry live. A missing file is not an error: it means
// NOTHING is editable (fail-closed for system-originated edits). Source is the file path
// when present, else "unset". Validation problems are collected (not fatal to the read) so
// the CLI/doctor can report them loudly while the registry stays invalid => nothing editable.
func LoadEditableSurfaces(homePath string) (EditableSurfaces, string, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return EditableSurfaces{AuthorityRef: EditableSurfacesAuthorityRef}, "unset", nil
	}
	path := EditableSurfacesPath(homePath)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return EditableSurfaces{AuthorityRef: EditableSurfacesAuthorityRef}, "unset", nil
		}
		return EditableSurfaces{AuthorityRef: EditableSurfacesAuthorityRef}, path, err
	}
	var file editableSurfacesFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return EditableSurfaces{AuthorityRef: EditableSurfacesAuthorityRef}, path, fmt.Errorf("parse %s: %w", path, err)
	}
	reg := EditableSurfaces{Source: path, AuthorityRef: EditableSurfacesAuthorityRef}
	seen := map[string]bool{}
	for i, entry := range file.Surfaces {
		surface := strings.TrimSpace(entry.Surface)
		if surface == "" {
			reg.ValidationErrors = append(reg.ValidationErrors, fmt.Sprintf("entry %d: surface slug is required", i))
			continue
		}
		key := strings.ToLower(surface)
		if hardDenyCoreSurfaces[key] {
			reg.ValidationErrors = append(reg.ValidationErrors, fmt.Sprintf(
				"surface %q is a HARD-DENY core surface and can never be whitelisted (policy, permissions, ledger, and evaluator must stay outside any self-improvement loop)", surface))
			continue
		}
		if seen[key] {
			reg.ValidationErrors = append(reg.ValidationErrors, fmt.Sprintf("surface %q is listed more than once", surface))
			continue
		}
		seen[key] = true
		status := strings.TrimSpace(strings.ToLower(entry.Status))
		if status != EditableSurfaceStatusActive && status != EditableSurfaceStatusRetired {
			reg.ValidationErrors = append(reg.ValidationErrors, fmt.Sprintf(
				"surface %q: status must be %q or %q, got %q", surface, EditableSurfaceStatusActive, EditableSurfaceStatusRetired, entry.Status))
		}
		proposalClass := strings.TrimSpace(entry.ProposalClass)
		if proposalClass != "" && !isMutationClass(proposalClass) {
			reg.ValidationErrors = append(reg.ValidationErrors, fmt.Sprintf(
				"surface %q: proposal_class %q is not a known mutation class", surface, proposalClass))
		}
		reg.Entries = append(reg.Entries, EditableSurface{
			Surface:       surface,
			Description:   strings.TrimSpace(entry.Description),
			ProposalClass: proposalClass,
			Status:        status,
		})
	}
	return reg, path, nil
}

func isMutationClass(class string) bool {
	for _, c := range envelope.MutationClasses {
		if c == class {
			return true
		}
	}
	return false
}

// WriteEditableSurfaces persists a registry (operator/ops helper and test seam). It writes
// the same file LoadEditableSurfaces reads, so an edit is visible to the very next check.
func WriteEditableSurfaces(homePath string, reg EditableSurfaces) error {
	return homepkg.WithAutonomyAdmissionLock(homePath, func() error {
		return writeEditableSurfacesLocked(homePath, reg)
	})
}

func writeEditableSurfacesLocked(homePath string, reg EditableSurfaces) error {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return fmt.Errorf("COMPOUND_HOME required")
	}
	file := editableSurfacesFile{SchemaVersion: EditableSurfacesSchema}
	for _, e := range reg.Entries {
		file.Surfaces = append(file.Surfaces, editableSurfaceFileEntry{
			Surface:       strings.TrimSpace(e.Surface),
			Description:   strings.TrimSpace(e.Description),
			ProposalClass: strings.TrimSpace(e.ProposalClass),
			Status:        strings.TrimSpace(e.Status),
		})
	}
	body, err := yaml.Marshal(file)
	if err != nil {
		return err
	}
	path := EditableSurfacesPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

// ActivateSurface loads the registry, ensures `surface` is present AND active (adding it
// when absent, flipping a retired entry back to active), and writes it back through the
// same WriteEditableSurfaces path the live check reads. Every other surface is preserved.
// It is the write path behind an operator's "allow edits to this surface" decision.
//
// Two guarantees the caller relies on:
//   - Byte-idempotent: re-activating an already-active surface adds no duplicate entry and
//     rewrites identical bytes; the returned `changed` is false so a caller can tell a
//     no-op re-run from a real toggle.
//   - Fail-closed on hard-deny-core: a policy/permissions/ledger/evaluator surface is
//     refused BEFORE any write, so a hand-crafted proposal can never brick the whole
//     registry by pushing a surface LoadEditableSurfaces would then flag invalid.
func ActivateSurface(homePath, surface string) (EditableSurfaces, bool, error) {
	surface = strings.TrimSpace(surface)
	if surface == "" {
		return EditableSurfaces{}, false, fmt.Errorf("editable surface required")
	}
	if hardDenyCoreSurfaces[strings.ToLower(surface)] {
		return EditableSurfaces{}, false, fmt.Errorf(
			"surface %q is a HARD-DENY core surface and can never be whitelisted (policy, permissions, ledger, and evaluator must stay outside any self-improvement loop)", surface)
	}
	reg, _, err := LoadEditableSurfaces(homePath)
	if err != nil {
		return EditableSurfaces{}, false, err
	}
	changed := false
	found := false
	for i := range reg.Entries {
		if !strings.EqualFold(reg.Entries[i].Surface, surface) {
			continue
		}
		found = true
		if reg.Entries[i].Status != EditableSurfaceStatusActive {
			reg.Entries[i].Status = EditableSurfaceStatusActive
			changed = true
		}
		break
	}
	if !found {
		reg.Entries = append(reg.Entries, EditableSurface{Surface: surface, Status: EditableSurfaceStatusActive})
		changed = true
	}
	next := EditableSurfaces{Entries: reg.Entries}
	if err := WriteEditableSurfaces(homePath, next); err != nil {
		return EditableSurfaces{}, false, err
	}
	return next, changed, nil
}

// EditableSurfaceAllowed is the live check callers use at the admission seam: it loads the
// registry live and reports whether a system-originated proposal may target `surface`.
func EditableSurfaceAllowed(homePath, surface string) (bool, string, error) {
	reg, _, err := LoadEditableSurfaces(homePath)
	if err != nil {
		return false, "", err
	}
	allowed, reason := reg.Allowed(surface)
	return allowed, reason, nil
}

// EditableSurfaceDenyDecision builds a recorded deny PolicyDecision for a system-originated
// proposal blocked because its target surface is not actively whitelisted, so the denial
// leaves an auditable envelope, not just an error.
func EditableSurfaceDenyDecision(ai envelope.ActionIntent, reason string, now time.Time) envelope.PolicyDecision {
	pd := mkDecision(ai, "deny", []string{reason}, nil, "", "", now)
	pd.EvidenceRefs = mergeStrings(pd.EvidenceRefs, EditableSurfacesAuthorityRef)
	return pd
}

// GuardActualEditableSurface applies the existing core frontier at a concrete
// mutation target. The adapter resolves the target and reads persisted work;
// neither a proposal label nor a caller-authored actor establishes the origin.
// Missing work binding cannot certify a protected raw mutation. Operator work
// keeps the existing security-class grant/approval path; system work is denied.
func GuardActualEditableSurface(ai envelope.ActionIntent, surface string, systemOriginated, bindingKnown bool) envelope.ActionIntent {
	surface = strings.ToLower(strings.TrimSpace(surface))
	if !hardDenyCoreSurfaces[surface] || ai.MutationClass == "read" {
		return ai
	}
	ai.MutationClass = "security"
	ai.EvidenceRefs = mergeStrings(ai.EvidenceRefs, EditableSurfacesAuthorityRef)
	reason := fmt.Sprintf("editable_surfaces(actual_target): %s is an operator-owned core surface", surface)
	if systemOriginated {
		ai.SecurityFloorOutcome = "deny"
		reason = fmt.Sprintf("editable_surfaces(actual_target): system-originated mutation of HARD-DENY core surface %s", surface)
	} else if !bindingKnown {
		ai.SecurityFloorOutcome = "deny"
		reason = fmt.Sprintf("editable_surfaces(actual_target): %s mutation requires readable persisted work binding", surface)
	}
	ai.SecurityReasons = mergeStrings(ai.SecurityReasons, reason)
	return ai
}
