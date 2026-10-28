package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestEditableSurfacesAbsentMeansNothingEditable(t *testing.T) {
	home := t.TempDir()
	reg, source, err := LoadEditableSurfaces(home)
	if err != nil {
		t.Fatalf("LoadEditableSurfaces: %v", err)
	}
	if source != "unset" {
		t.Fatalf("absent registry source should be unset, got %q", source)
	}
	if !reg.Valid() {
		t.Fatalf("absent registry should be valid (no errors), got %v", reg.ValidationErrors)
	}
	if !reg.NothingEditable() {
		t.Fatalf("absent registry must make NOTHING editable")
	}
	// Even a plausible surface is denied when the config is absent (fail-closed).
	allowed, reason, err := EditableSurfaceAllowed(home, "skill_descriptions")
	if err != nil {
		t.Fatalf("EditableSurfaceAllowed: %v", err)
	}
	if allowed {
		t.Fatalf("absent registry must deny every surface, got allowed for skill_descriptions")
	}
	if reason == "" {
		t.Fatalf("denial must carry a reason")
	}
}

// TestEditableSurfacesEmptyConfigMeansNothingEditable proves an installed-but-empty
// registry still opens nothing: opening a surface is an explicit act, never a default.
func TestEditableSurfacesEmptyConfigMeansNothingEditable(t *testing.T) {
	home := t.TempDir()
	if err := WriteEditableSurfaces(home, EditableSurfaces{}); err != nil {
		t.Fatalf("WriteEditableSurfaces empty: %v", err)
	}
	reg, source, err := LoadEditableSurfaces(home)
	if err != nil {
		t.Fatalf("LoadEditableSurfaces: %v", err)
	}
	if source == "unset" {
		t.Fatalf("written empty registry should have a file source")
	}
	if !reg.NothingEditable() {
		t.Fatalf("empty registry must make NOTHING editable")
	}
}

// TestEditableSurfacesHardDenyCoreIsAValidationError is the headline falsifier: a yaml that
// whitelists a HARD-DENY core surface (ledger) is loudly invalid, and while it is invalid
// NOTHING is editable — not even the other, well-formed entry in the same file.
func TestEditableSurfacesHardDenyCoreIsAValidationError(t *testing.T) {
	for _, core := range HardDenyCoreSurfaces() {
		home := t.TempDir()
		writeRawEditableSurfaces(t, home, ""+
			"schema_version: "+EditableSurfacesSchema+"\n"+
			"surfaces:\n"+
			"  - surface: "+core+"\n"+
			"    proposal_class: local_reversible\n"+
			"    status: active\n"+
			"  - surface: skill_descriptions\n"+
			"    proposal_class: local_reversible\n"+
			"    status: active\n")

		reg, _, err := LoadEditableSurfaces(home)
		if err != nil {
			t.Fatalf("LoadEditableSurfaces(%s): %v", core, err)
		}
		if reg.Valid() {
			t.Fatalf("registry whitelisting core surface %q must be INVALID", core)
		}
		if !containsSubstr(reg.ValidationErrors, "HARD-DENY") {
			t.Fatalf("core surface %q must produce a loud HARD-DENY validation error, got %v", core, reg.ValidationErrors)
		}
		if !reg.NothingEditable() {
			t.Fatalf("an invalid registry must make NOTHING editable, even the well-formed sibling entry")
		}
		// The well-formed sibling entry is ALSO denied while the registry is invalid.
		if allowed, _ := reg.Allowed("skill_descriptions"); allowed {
			t.Fatalf("core surface %q poisons the registry: the valid sibling must be denied too", core)
		}
	}
}

// TestEditableSurfacesActiveRetiredLifecycle proves the active/retired status gate: an
// actively whitelisted surface is allowed, a retired one is denied, and an unknown one is
// denied.
func TestEditableSurfacesActiveRetiredLifecycle(t *testing.T) {
	home := t.TempDir()
	if err := WriteEditableSurfaces(home, EditableSurfaces{Entries: []EditableSurface{
		{Surface: "skill_descriptions", ProposalClass: "local_reversible", Status: EditableSurfaceStatusActive},
		{Surface: "injection_templates", ProposalClass: "local_reversible", Status: EditableSurfaceStatusRetired},
	}}); err != nil {
		t.Fatalf("WriteEditableSurfaces: %v", err)
	}
	reg, _, err := LoadEditableSurfaces(home)
	if err != nil {
		t.Fatalf("LoadEditableSurfaces: %v", err)
	}
	if !reg.Valid() {
		t.Fatalf("well-formed registry must be valid, got %v", reg.ValidationErrors)
	}
	if allowed, reason := reg.Allowed("skill_descriptions"); !allowed {
		t.Fatalf("active surface must be allowed, got reason %q", reason)
	}
	if allowed, reason := reg.Allowed("injection_templates"); allowed || !strings.Contains(reason, "retired") {
		t.Fatalf("retired surface must be denied with a retired reason, got allowed=%t reason=%q", allowed, reason)
	}
	if allowed, reason := reg.Allowed("steering_config"); allowed || !strings.Contains(reason, "not_whitelisted") {
		t.Fatalf("unknown surface must be denied as not_whitelisted, got allowed=%t reason=%q", allowed, reason)
	}
}

// TestEditableSurfacesBadStatusAndClassAreLoud proves malformed entries poison the whole
// registry so nothing is editable, rather than silently widening or narrowing the whitelist.
func TestEditableSurfacesBadStatusAndClassAreLoud(t *testing.T) {
	home := t.TempDir()
	writeRawEditableSurfaces(t, home, ""+
		"surfaces:\n"+
		"  - surface: skill_descriptions\n"+
		"    proposal_class: not_a_class\n"+
		"    status: on\n")
	reg, _, err := LoadEditableSurfaces(home)
	if err != nil {
		t.Fatalf("LoadEditableSurfaces: %v", err)
	}
	if reg.Valid() {
		t.Fatalf("bad status/class must invalidate the registry")
	}
	if !reg.NothingEditable() {
		t.Fatalf("invalid registry must make nothing editable")
	}
}

func TestEditableSurfaceDenyDecisionIsRecordableDeny(t *testing.T) {
	ai := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            "act_surface_test",
			CreatedAt:     time.Now().UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			Authority:     envelope.Authority{SourceClass: "runtime"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "auto-admit:director",
		Capability:    "work.advance",
		Action:        "advance_work_to_admitted",
		Target:        envelope.ActionTarget{Kind: "work", ID: "work_x", Path: "work/work/work_x.json"},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       "advance work_x proposed -> admitted",
		RollbackPlan:  "leave proposed",
	}
	pd := EditableSurfaceDenyDecision(ai, "editable_surfaces(not_whitelisted): surface \"steering_config\"", time.Now())
	if pd.Outcome != "deny" {
		t.Fatalf("surface deny decision must be deny, got %q", pd.Outcome)
	}
	if err := pd.Validate(); err != nil {
		t.Fatalf("surface deny decision must validate: %v", err)
	}
	if !containsSubstr(pd.EvidenceRefs, EditableSurfacesAuthorityRef) {
		t.Fatalf("surface deny decision must carry the registry authority ref: %+v", pd.EvidenceRefs)
	}
}

func writeRawEditableSurfaces(t *testing.T, home, body string) {
	t.Helper()
	path := EditableSurfacesPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write raw registry: %v", err)
	}
}

func containsSubstr(items []string, sub string) bool {
	for _, s := range items {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
