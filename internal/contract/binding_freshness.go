package contract

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// BindingFreshnessRow is a read-only byte-attestation diagnostic, never permission
// authority. Fresh requires all events to match concrete installed binary bytes;
// drifted means at least one comparable event differs. Legacy locators, opaque
// handlers and unavailable bytes are unknown. Drifted > unknown > fresh.
type BindingFreshnessRow struct {
	Harness       string `json:"harness"`
	Status        string `json:"status"`
	RecordedAt    string `json:"recorded_revision,omitempty"`
	CurrentBinary string `json:"current_binary,omitempty"`
	RepairCommand string `json:"repair_command,omitempty"`
}

// BindingFreshnessReport keeps the supplied binary path separate from each
// row's content revisions. Installers and dashboards consume this in JSON.
type BindingFreshnessReport struct {
	BinaryPath string                `json:"binary_path"`
	Rows       []BindingFreshnessRow `json:"rows"`
}

// BuildBindingFreshnessReport wraps ComputeBindingFreshness with the
// doctor-specific note-on-error mapping: a non-nil error becomes the
// "(unavailable: …)" string the doctor text renderer prints in place of
// rows, and a nil error collapses to (report, ""). This keeps the
// doctor's runDoctor body a single line per R3B 2026-09-04 section, the
// same pattern BinaryStaleness and HarnessVersionDrift use (one compute
// call + one render call per section).
func BuildBindingFreshnessReport(homePath, binaryPath string) (BindingFreshnessReport, string) {
	report, err := ComputeBindingFreshness(homePath, binaryPath)
	if err != nil {
		return report, "binding freshness unavailable: " + err.Error()
	}
	return report, ""
}

// ComputeBindingFreshness compares every concrete binding to the supplied
// binary's independently attested SHA-256 bytes. Other handlers, legacy locators
// and unavailable bytes remain unknown; an absent manifest yields no rows.
// This diagnostic never writes the manifest or calls an installer.
func ComputeBindingFreshness(homePath, binaryPath string) (BindingFreshnessReport, error) {
	report := BindingFreshnessReport{BinaryPath: strings.TrimSpace(binaryPath)}
	if strings.TrimSpace(homePath) == "" {
		return report, fmt.Errorf("compute binding freshness: home path is required")
	}
	if strings.TrimSpace(binaryPath) == "" {
		return report, fmt.Errorf("compute binding freshness: binary path is required")
	}
	manifest, err := LoadInstalledHooksManifest(homePath)
	if err != nil {
		// A missing manifest is the absent path: doctor renders
		// "(no installed hooks manifest)" instead of an error. This is
		// the common case on a fresh home before any lane has been
		// installed, and is NOT a binding-freshness failure.
		if os.IsNotExist(err) {
			return report, nil
		}
		return report, fmt.Errorf("compute binding freshness: %w", err)
	}

	binaryRef := filepath.Clean(expandHomeRef(report.BinaryPath))
	currentRevision := ""
	if filepath.IsAbs(binaryRef) {
		currentRevision = installedHookHandlerRevision(binaryRef, "")
	}
	rank := map[string]int{"fresh": 0, "unknown": 1, "drifted": 2}
	byHarness := map[string]BindingFreshnessRow{}
	for _, hook := range manifest.Hooks {
		h := strings.TrimSpace(hook.Harness)
		if h == "" {
			continue
		}
		comparableRevision := ""
		ref := filepath.Clean(expandHomeRef(hook.Ref))
		if filepath.IsAbs(ref) && ref == binaryRef {
			comparableRevision = currentRevision
		}
		row := BindingFreshnessRow{
			Harness:       h,
			RecordedAt:    strings.TrimSpace(hook.Revision),
			CurrentBinary: comparableRevision,
			Status:        classifyBindingFreshness(hook.Revision, comparableRevision),
		}
		if row.Status == "drifted" {
			row.RepairCommand = "compound hook install --harness " + h
		}
		previous, exists := byHarness[h]
		// Every event contributes: fresh PreToolUse cannot hide stale SessionStart.
		// Select equal-rank evidence deterministically, independent of input order.
		key := row.RecordedAt + "\x00" + row.CurrentBinary
		previousKey := previous.RecordedAt + "\x00" + previous.CurrentBinary
		if !exists || rank[row.Status] > rank[previous.Status] ||
			(rank[row.Status] == rank[previous.Status] && key < previousKey) {
			byHarness[h] = row
		}
	}
	for _, row := range byHarness {
		report.Rows = append(report.Rows, row)
	}
	sort.Slice(report.Rows, func(i, j int) bool { return report.Rows[i].Harness < report.Rows[j].Harness })
	return report, nil
}

func classifyBindingFreshness(recorded, want string) string {
	recorded, want = strings.TrimSpace(recorded), strings.TrimSpace(want)
	for _, revision := range []string{recorded, want} {
		if !strings.HasPrefix(revision, "sha256:") || len(revision) != len("sha256:")+64 {
			return "unknown"
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(revision, "sha256:")); err != nil {
			return "unknown"
		}
	}
	if strings.EqualFold(recorded, want) {
		return "fresh"
	}
	return "drifted"
}

// RenderBindingFreshness writes the R3B 2026-09-04 hook-binding freshness
// section to stdout in the doctor's plain-text shape. One line per harness:
// "<harness> <status> recorded=<revision> current=<binary>". Drifted rows
// carry the exact repair command on a second line so the operator does not
// have to remember it. When note is non-empty (the compute call failed)
// the section prints a single unavailable line instead of rows, keeping
// the section self-describing on a torn home. Mirrors printBindingFreshness
// from cmd/compound/doctor_cli.go so the existing doctor tests that pin
// the text shape continue to pass byte-for-byte.
func RenderBindingFreshness(stdout io.Writer, report BindingFreshnessReport, note string) {
	fmt.Fprintln(stdout, "\n## binding freshness (hook manifest vs installed binary)")
	if note != "" {
		fmt.Fprintf(stdout, "- (unavailable: %s)\n", note)
		return
	}
	if len(report.Rows) == 0 {
		fmt.Fprintln(stdout, "- (no installed hooks manifest)")
		return
	}
	for _, row := range report.Rows {
		fmt.Fprintf(stdout, "- %-16s %-7s recorded=%q current=%q\n", row.Harness, row.Status, row.RecordedAt, row.CurrentBinary)
		if row.RepairCommand != "" {
			fmt.Fprintf(stdout, "    -> %s\n", row.RepairCommand)
		}
	}
}
