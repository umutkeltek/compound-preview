package policy

// Autonomy kill-switch (W9-J4): a policy-owned halt read LIVE at the admission seam.
// It is the first mandatory chokepoint every autonomous admission passes through:
// typed evidence-threshold auto-admit and any director-originated advance MUST
// consult it before releasing a proposed WorkEnvelope. A global halt stops ALL
// autonomous admission; a per-class disable stops one class. The operator flips it
// through `compound policy autonomy-halt`, whose writer shares the admission lock
// (no rebuild, no daemon). Each check re-reads the file. Governance is enforced at
// the adapter here, never asked for in a prompt.

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
	// AutonomyHaltAuthorityRef names the halt as a policy authority for evidence refs.
	AutonomyHaltAuthorityRef = "compound://registries/autonomy-halt"
	// AutonomyHaltSchema is the on-disk schema tag for the halt file.
	AutonomyHaltSchema   = "compound.autonomy_halt.v0.1"
	autonomyHaltFileName = "autonomy-halt.yaml"
)

// AutonomyHalt is the operator-owned kill-switch state. Zero value = not halted.
type AutonomyHalt struct {
	Global          bool              `json:"global"`
	Reason          string            `json:"reason,omitempty"`
	DisabledClasses map[string]string `json:"disabled_classes,omitempty"`
	Source          string            `json:"source,omitempty"`
	AuthorityRef    string            `json:"authority_ref,omitempty"`
}

// autonomyHaltFile is the persisted projection written by the policy command.
// Direct edits bypass the admission lock and are not a supported mutation path.
type autonomyHaltFile struct {
	SchemaVersion   string                   `yaml:"schema_version,omitempty"`
	Global          bool                     `yaml:"global"`
	Reason          string                   `yaml:"reason,omitempty"`
	DisabledClasses []autonomyHaltClassEntry `yaml:"disabled_classes,omitempty"`
}

type autonomyHaltClassEntry struct {
	Class  string `yaml:"class"`
	Reason string `yaml:"reason,omitempty"`
}

// AutonomyHaltPath is the halt projection under COMPOUND_HOME/config.
func AutonomyHaltPath(homePath string) string {
	return filepath.Join(homePath, "config", autonomyHaltFileName)
}

// LoadAutonomyHalt reads the halt state live. A missing file is not an error: it means
// autonomy is not halted (fail-open for the halt itself, so the switch defaults to OFF).
// Source is the file path when present, else "unset".
func LoadAutonomyHalt(homePath string) (AutonomyHalt, string, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return AutonomyHalt{}, "unset", nil
	}
	path := AutonomyHaltPath(homePath)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return AutonomyHalt{}, "unset", nil
		}
		return AutonomyHalt{}, path, err
	}
	var file autonomyHaltFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return AutonomyHalt{}, path, fmt.Errorf("parse %s: %w", path, err)
	}
	halt := AutonomyHalt{
		Global:       file.Global,
		Reason:       strings.TrimSpace(file.Reason),
		Source:       path,
		AuthorityRef: AutonomyHaltAuthorityRef,
	}
	for _, entry := range file.DisabledClasses {
		class := strings.TrimSpace(entry.Class)
		if class == "" {
			continue
		}
		if halt.DisabledClasses == nil {
			halt.DisabledClasses = map[string]string{}
		}
		halt.DisabledClasses[class] = strings.TrimSpace(entry.Reason)
	}
	return halt, path, nil
}

// WriteAutonomyHalt persists a halt state (operator/ops helper and test seam). It writes
// the same file LoadAutonomyHalt reads, so a flip is visible to the very next check.
func WriteAutonomyHalt(homePath string, halt AutonomyHalt) error {
	return homepkg.WithAutonomyAdmissionLock(homePath, func() error {
		return writeAutonomyHaltLocked(homePath, halt)
	})
}

// UpdateAutonomyHalt performs a read-modify-write under the same lock used by
// AdmitAuto. It is the operator CLI seam for global and per-class changes, so
// concurrent admission has one serializable order with an emergency halt.
func UpdateAutonomyHalt(homePath string, update func(*AutonomyHalt) error) error {
	if update == nil {
		return fmt.Errorf("autonomy halt update callback required")
	}
	return homepkg.WithAutonomyAdmissionLock(homePath, func() error {
		halt, _, err := LoadAutonomyHalt(homePath)
		if err != nil {
			return err
		}
		if halt.DisabledClasses == nil {
			halt.DisabledClasses = map[string]string{}
		}
		if err := update(&halt); err != nil {
			return err
		}
		return writeAutonomyHaltLocked(homePath, halt)
	})
}

func writeAutonomyHaltLocked(homePath string, halt AutonomyHalt) error {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return fmt.Errorf("COMPOUND_HOME required")
	}
	file := autonomyHaltFile{
		SchemaVersion: AutonomyHaltSchema,
		Global:        halt.Global,
		Reason:        strings.TrimSpace(halt.Reason),
	}
	for class, reason := range halt.DisabledClasses {
		class = strings.TrimSpace(class)
		if class == "" {
			continue
		}
		file.DisabledClasses = append(file.DisabledClasses, autonomyHaltClassEntry{Class: class, Reason: strings.TrimSpace(reason)})
	}
	body, err := yaml.Marshal(file)
	if err != nil {
		return err
	}
	path := AutonomyHaltPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

// Halted reports whether autonomous admission of `class` is blocked right now, and why.
// Global halt wins over per-class; both return a stable, greppable reason string.
func (h AutonomyHalt) Halted(class string) (bool, string) {
	if h.Global {
		reason := strings.TrimSpace(h.Reason)
		if reason == "" {
			reason = "global autonomy halt engaged"
		}
		return true, "autonomy_halt(global): " + reason
	}
	class = strings.TrimSpace(class)
	if class == "" {
		return false, ""
	}
	if reason, ok := h.DisabledClasses[class]; ok {
		reason = strings.TrimSpace(reason)
		if reason == "" {
			reason = "class autonomy disabled"
		}
		return true, fmt.Sprintf("autonomy_halt(class=%s): %s", class, reason)
	}
	return false, ""
}

// AutonomyHalted is the mandatory chokepoint: it loads the halt state live and reports
// whether the given autonomy class is blocked. Every auto-admit / director-originated
// advance path calls this before releasing a proposed WorkEnvelope.
func AutonomyHalted(homePath, class string) (bool, string, error) {
	halt, _, err := LoadAutonomyHalt(homePath)
	if err != nil {
		return false, "", err
	}
	halted, reason := halt.Halted(class)
	return halted, reason, nil
}

// AutonomyHaltDecision builds a recorded deny PolicyDecision for an autonomous admission
// blocked by the kill-switch, so the halt leaves an auditable envelope, not just an error.
func AutonomyHaltDecision(ai envelope.ActionIntent, reason string, now time.Time) envelope.PolicyDecision {
	pd := mkDecision(ai, "deny", []string{reason}, nil, "", "", now)
	pd.EvidenceRefs = mergeStrings(pd.EvidenceRefs, AutonomyHaltAuthorityRef)
	return pd
}
