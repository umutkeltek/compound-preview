package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// InstructionPackSchemaVersion is the JSON schema id the pack body carries.
// A future bump is conscious — tests pin it.
const InstructionPackSchemaVersion = "compound.instruction_pack.v0.1"

// InstructionPackLayer is the typed reference to a context-defining layer
// (policy-core / posture). Each layer degrades gracefully: an unresolvable
// source (no authority yaml, no permission HarnessArtifact) renders as a
// single absent marker so the pack still writes a complete shape.
type InstructionPackLayer struct {
	AuthorityRef   string `json:"authority_ref"`
	SourceRevision string `json:"source_revision"`
	ContentSHA256  string `json:"content_sha256,omitempty"`
	Absent         bool   `json:"absent,omitempty"`
}

// InstructionPackRosterRef carries one roster ref (installed hooks manifest,
// effective contract latest.json) with the same absent-aware semantics.
type InstructionPackRosterRef struct {
	Ref      string `json:"ref,omitempty"`
	Revision string `json:"revision,omitempty"`
	Absent   bool   `json:"absent,omitempty"`
}

// InstructionPackRoster is the deterministic-identity view of every roster
// a harness sees at render time: the registry revision (default — never
// absent) plus the optional installed-hooks manifest and the effective
// contract latest.json.
type InstructionPackRoster struct {
	RegistryRevision  string                   `json:"registry_revision"`
	InstalledHooks    InstructionPackRosterRef `json:"installed_hooks"`
	EffectiveContract InstructionPackRosterRef `json:"effective_contract"`
}

// InstructionPack is the typed, content-addressed, three-layer reflection
// of every deterministic input a harness uses to construct an instruction
// stream (packet work_7373143486d9). It is produced by RenderInstructionPack
// and written atomically under
// $COMPOUND_HOME/context/instruction-packs/<harness>/latest.json.
//
// ContentSHA256 covers the pack body EXCLUDING GeneratedAt so two renders
// over identical inputs yield the same identity (the stability falsifier
// depends on this). GeneratedAt IS recorded on the body — only the hash
// excludes it.
// Preset is the optional second identity axis (packet S2, work_ff2ca97d915f):
// two switchback presets over ONE harness must produce distinct packs rather
// than racing for the same latest.json. It is `omitempty` and excluded from
// the content hash when empty so the default (presetless) pack keeps the
// byte-identical body and identity it had before the axis existed — every
// pack already on disk in every existing home depends on that.
type InstructionPack struct {
	SchemaVersion string                `json:"schema_version"`
	Harness       string                `json:"harness"`
	Preset        string                `json:"preset,omitempty"`
	PolicyCore    InstructionPackLayer  `json:"policy_core"`
	Posture       InstructionPackLayer  `json:"posture"`
	Roster        InstructionPackRoster `json:"roster"`
	GeneratedAt   time.Time             `json:"generated_at"`
	ContentSHA256 string                `json:"content_sha256"`
}

// installedHooksManifestPath mirrors contract.InstalledHooksPath
// (homePath/config/installed-hooks.yaml). Inlined here because internal/harness
// cannot import internal/contract (contract imports harness). The mirror is
// mechanical — any path drift is caught by F5.
func installedHooksManifestPath(homePath string) string {
	return filepath.Join(homePath, "config", "installed-hooks.yaml")
}

// validateInstructionPackPreset trims and validates a preset name. The preset
// becomes a filesystem path segment, so a value carrying a separator or a
// parent-dir hop would let a caller write outside the instruction-packs tree.
// Rejection is loud and happens BEFORE any directory is created, so an invalid
// preset is inert (P5).
//
// The empty preset is valid and means "no preset" — it selects the legacy
// harness-only path and identity.
func validateInstructionPackPreset(preset string) (string, error) {
	preset = strings.TrimSpace(preset)
	if preset == "" {
		return "", nil
	}
	if strings.ContainsAny(preset, `/\`) {
		return "", fmt.Errorf("invalid preset %q: must not contain a path separator", preset)
	}
	if preset == "." || preset == ".." || strings.Contains(preset, "..") {
		return "", fmt.Errorf("invalid preset %q: must not contain a parent-directory reference", preset)
	}
	return preset, nil
}

// InstructionPackPath returns the write path for the pack projection:
// $COMPOUND_HOME/context/instruction-packs/<harness>/latest.json when preset
// is empty, and .../<harness>/<preset>/latest.json when it is not.
//
// The empty-preset branch reproduces the pre-preset path EXACTLY (P1) — a
// path change would orphan every pack already written under every existing
// home. An invalid preset is not sanitized into a neighbouring path here;
// it is rejected upstream by validateInstructionPackPreset before any write
// or read is attempted.
func InstructionPackPath(homePath, harness, preset string) string {
	parts := []string{
		strings.TrimSpace(homePath),
		"context",
		"instruction-packs",
		strings.TrimSpace(harness),
	}
	if trimmed := strings.TrimSpace(preset); trimmed != "" {
		parts = append(parts, trimmed)
	}
	parts = append(parts, "latest.json")
	return filepath.Join(parts...)
}

// RenderInstructionPack assembles and writes the harness-scoped pack
// projection. Every layer degrades gracefully: missing authority/artifact/
// manifest/contract-latest → that layer carries an explicit absent marker
// (empty refs, no error), the pack still renders. PolicyCore for slice 1 = the
// permission authority ref + revision when present (the hand-written yaml IS
// today's policy-core instance); a fuller policy-core source is a later slice.
//
// RenderInstructionPack is the WRITE side of the seam. InstructionPackDrift
// calls renderInstructionPack (the in-memory twin below) so drift probes
// never mutate the on-disk projection — a re-run against an unchanged home
// would otherwise always see self-cancelled drift (F10 invariant).
func RenderInstructionPack(homePath, harness, preset string, now time.Time) (InstructionPack, error) {
	pack, err := renderInstructionPack(homePath, harness, preset, now)
	if err != nil {
		return InstructionPack{}, err
	}
	if err := writeInstructionPackAtomic(InstructionPackPath(homePath, harness, pack.Preset), pack); err != nil {
		return InstructionPack{}, fmt.Errorf("write pack latest.json: %w", err)
	}
	return pack, nil
}

// renderInstructionPack assembles the harness-scoped pack IN MEMORY ONLY.
// RenderInstructionPack persists the result; InstructionPackDrift compares
// the result to the on-disk latest.json. Splitting render from write keeps
// the drift comparator read-only (F10) — without the split, every drift
// invocation would self-cancel by writing its own fresh pack first.
func renderInstructionPack(homePath, harness, preset string, now time.Time, selectedContract ...string) (InstructionPack, error) {
	harness = strings.TrimSpace(harness)
	if harness == "" {
		return InstructionPack{}, fmt.Errorf("harness required")
	}
	preset, err := validateInstructionPackPreset(preset)
	if err != nil {
		return InstructionPack{}, err
	}
	now = now.UTC()

	policy, policyRev, policyErr := LoadPermissionAuthority(homePath, harness)
	policyLayer := InstructionPackLayer{Absent: true}
	if policyErr == nil {
		body, readErr := os.ReadFile(PermissionAuthorityPath(homePath, harness))
		if readErr != nil {
			return InstructionPack{}, fmt.Errorf("read authority body: %w", readErr)
		}
		policyLayer = InstructionPackLayer{
			AuthorityRef:   PermissionAuthorityRef(harness),
			SourceRevision: policyRev,
			ContentSHA256:  "sha256:" + digest(body),
		}
	}

	postureLayer := InstructionPackLayer{Absent: true}
	if policyErr == nil {
		// Posture layer needs both the authority (for target path) AND a
		// recorded HarnessArtifact for that target. Missing artifact → absent.
		if artifact, ok, err := latestHarnessArtifact(homePath, harness, policy.TargetPath); err == nil && ok {
			postureLayer = InstructionPackLayer{
				AuthorityRef:   artifact.SourceAuthorityRef,
				SourceRevision: artifact.SourceRevision,
				ContentSHA256:  artifact.ContentSHA256,
			}
		}
	}

	installedHooksRef := installedHooksManifestPath(homePath)
	installedHooks := InstructionPackRosterRef{Absent: true}
	if body, err := os.ReadFile(installedHooksRef); err == nil {
		installedHooks = InstructionPackRosterRef{
			Ref:      installedHooksManifestRel(homePath),
			Revision: "sha256:" + digest(body),
		}
	}

	contractRef := ""
	if len(selectedContract) > 0 {
		contractRef = selectedContract[0]
	} else {
		body, readErr := os.ReadFile(InstructionPackPath(homePath, harness, preset))
		if readErr == nil {
			var installed InstructionPack
			if err := json.Unmarshal(body, &installed); err != nil {
				return InstructionPack{}, fmt.Errorf("decode installed instruction pack: %w", err)
			}
			if installed.Harness != harness || installed.Preset != preset {
				return InstructionPack{}, fmt.Errorf("installed instruction-pack identity mismatch")
			}
			contractRef = installed.Roster.EffectiveContract.Ref
		} else if !os.IsNotExist(readErr) {
			return InstructionPack{}, readErr
		}
	}
	effectiveContract := InstructionPackRosterRef{Absent: true}
	if contractRef != "" {
		var err error
		effectiveContract, err = ReadInstructionPackContract(homePath, harness, contractRef)
		if err != nil {
			return InstructionPack{}, err
		}
	}

	pack := InstructionPack{
		SchemaVersion: InstructionPackSchemaVersion,
		Harness:       harness,
		Preset:        preset,
		PolicyCore:    policyLayer,
		Posture:       postureLayer,
		Roster: InstructionPackRoster{
			RegistryRevision:  RegistryRevision(DefaultRegistry()),
			InstalledHooks:    installedHooks,
			EffectiveContract: effectiveContract,
		},
		GeneratedAt: now,
	}
	pack.ContentSHA256 = "sha256:" + digest(instructionPackBody(pack))
	return pack, nil
}

// InstructionPackDriftReport mirrors PermissionDriftReport's shape so the
// observation loop downstream can switch on Drifted without caring which
// harness seam produced it. ExpectedDigest / ObservedDigest are stable
// managed-surface digests (ContentSHA256 + per-layer refs/revisions only —
// GeneratedAt excluded) so two renders over identical inputs compare equal
// regardless of clock variance (F5).
type InstructionPackDriftReport struct {
	Harness        string   `json:"harness"`
	Preset         string   `json:"preset,omitempty"`
	Path           string   `json:"path"`
	Drifted        bool     `json:"drifted"`
	Details        []string `json:"details,omitempty"`
	ExpectedDigest string   `json:"expected_digest,omitempty"`
	ObservedDigest string   `json:"observed_digest,omitempty"`
}

// InstructionPackDrift compares the on-disk $COMPOUND_HOME/context/instruction-packs/<h>/latest.json
// against a fresh in-memory render and reports whether they agree. It is
// READ-ONLY (F10): it never writes the fresh pack, never mutates the
// authority, never moves the on-disk projection. A missing on-disk file is
// treated as drift with ObservedDigest="" (F11) so the cold-start case
// surfaces to the intake seam instead of staying invisible.
//
// A preset pack is compared against its OWN preset path only: a sibling
// preset's file on disk neither satisfies nor drifts this one.
func InstructionPackDrift(homePath, harness, preset string, now time.Time) (InstructionPackDriftReport, error) {
	harness = strings.TrimSpace(harness)
	if harness == "" {
		return InstructionPackDriftReport{}, fmt.Errorf("harness required")
	}
	preset, err := validateInstructionPackPreset(preset)
	if err != nil {
		return InstructionPackDriftReport{}, err
	}
	now = now.UTC()
	path := InstructionPackPath(homePath, harness, preset)
	fresh, err := renderInstructionPack(homePath, harness, preset, now)
	if err != nil {
		return InstructionPackDriftReport{}, err
	}
	expected := instructionPackDriftDigest(fresh)
	report := InstructionPackDriftReport{
		Harness:        harness,
		Preset:         preset,
		Path:           path,
		ExpectedDigest: expected,
	}

	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			report.Drifted = true
			report.Details = []string{"on-disk latest.json is missing"}
			return report, nil
		}
		return InstructionPackDriftReport{}, fmt.Errorf("read latest.json: %w", err)
	}

	var observed InstructionPack
	if err := json.Unmarshal(body, &observed); err != nil {
		return InstructionPackDriftReport{}, fmt.Errorf("decode latest.json: %w", err)
	}
	if fresh.Roster.EffectiveContract.Absent {
		return InstructionPackDriftReport{}, fmt.Errorf("instruction-pack effective contract is unbound; render with --context-pack")
	}
	observedDigest := instructionPackDriftDigest(observed)
	report.ObservedDigest = observedDigest
	if observedDigest == expected {
		return report, nil
	}
	report.Drifted = true
	report.Details = instructionPackDeltaDetails(fresh, observed)
	return report, nil
}

// instructionPackDriftDigest hashes the MANAGED surface of a pack:
// ContentSHA256 + every per-layer ref/revision/sha/absent flag. GeneratedAt
// is deliberately EXCLUDED — a re-render over identical inputs would
// otherwise report self-cancelled drift (F5).
func instructionPackDriftDigest(p InstructionPack) string {
	parts := []string{
		"content_sha256=" + p.ContentSHA256,
		"policy_core.authority_ref=" + p.PolicyCore.AuthorityRef,
		"policy_core.source_revision=" + p.PolicyCore.SourceRevision,
		"policy_core.content_sha256=" + p.PolicyCore.ContentSHA256,
		"policy_core.absent=" + boolStr(p.PolicyCore.Absent),
		"posture.authority_ref=" + p.Posture.AuthorityRef,
		"posture.source_revision=" + p.Posture.SourceRevision,
		"posture.content_sha256=" + p.Posture.ContentSHA256,
		"posture.absent=" + boolStr(p.Posture.Absent),
		"roster.registry_revision=" + p.Roster.RegistryRevision,
		"roster.installed_hooks.ref=" + p.Roster.InstalledHooks.Ref,
		"roster.installed_hooks.revision=" + p.Roster.InstalledHooks.Revision,
		"roster.installed_hooks.absent=" + boolStr(p.Roster.InstalledHooks.Absent),
		"roster.effective_contract.ref=" + p.Roster.EffectiveContract.Ref,
		"roster.effective_contract.revision=" + p.Roster.EffectiveContract.Revision,
		"roster.effective_contract.absent=" + boolStr(p.Roster.EffectiveContract.Absent),
	}
	// Conditional for the same reason instructionPackBody is: an
	// unconditional "preset=" term would change the managed-surface digest
	// of every presetless pack. The preset is already covered transitively
	// via content_sha256; this term makes a misplaced pack (right path,
	// wrong preset in the body) legible in the delta.
	if p.Preset != "" {
		parts = append(parts, "preset="+p.Preset)
	}
	return digest([]byte(strings.Join(parts, "\x00")))
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// instructionPackDeltaDetails describes which surface drives the digest
// mismatch between a fresh render and the on-disk pack. Layer-by-layer so
// an operator reading the report can pinpoint the input that drifted
// (policy-core revision bump, manifest body change, contract latest
// rotated, …).
func instructionPackDeltaDetails(fresh, observed InstructionPack) []string {
	var details []string
	if fresh.Preset != observed.Preset {
		details = append(details, fmt.Sprintf(
			"preset mismatch (fresh=%q observed=%q)", fresh.Preset, observed.Preset))
	}
	if fresh.PolicyCore.SourceRevision != observed.PolicyCore.SourceRevision ||
		fresh.PolicyCore.ContentSHA256 != observed.PolicyCore.ContentSHA256 ||
		fresh.PolicyCore.AuthorityRef != observed.PolicyCore.AuthorityRef {
		details = append(details, fmt.Sprintf(
			"policy_core drifted (fresh_rev=%s observed_rev=%s)",
			fresh.PolicyCore.SourceRevision, observed.PolicyCore.SourceRevision))
	}
	if fresh.PolicyCore.Absent != observed.PolicyCore.Absent {
		details = append(details, fmt.Sprintf(
			"policy_core absent flag changed (fresh=%t observed=%t)",
			fresh.PolicyCore.Absent, observed.PolicyCore.Absent))
	}
	if fresh.Posture.SourceRevision != observed.Posture.SourceRevision ||
		fresh.Posture.ContentSHA256 != observed.Posture.ContentSHA256 ||
		fresh.Posture.AuthorityRef != observed.Posture.AuthorityRef {
		details = append(details, fmt.Sprintf(
			"posture drifted (fresh_rev=%s observed_rev=%s)",
			fresh.Posture.SourceRevision, observed.Posture.SourceRevision))
	}
	if fresh.Posture.Absent != observed.Posture.Absent {
		details = append(details, fmt.Sprintf(
			"posture absent flag changed (fresh=%t observed=%t)",
			fresh.Posture.Absent, observed.Posture.Absent))
	}
	if fresh.Roster.RegistryRevision != observed.Roster.RegistryRevision {
		details = append(details, fmt.Sprintf(
			"registry_revision drifted (fresh=%s observed=%s)",
			fresh.Roster.RegistryRevision, observed.Roster.RegistryRevision))
	}
	if fresh.Roster.InstalledHooks.Revision != observed.Roster.InstalledHooks.Revision ||
		fresh.Roster.InstalledHooks.Ref != observed.Roster.InstalledHooks.Ref ||
		fresh.Roster.InstalledHooks.Absent != observed.Roster.InstalledHooks.Absent {
		details = append(details, fmt.Sprintf(
			"installed_hooks layer drifted (fresh_rev=%s observed_rev=%s)",
			fresh.Roster.InstalledHooks.Revision, observed.Roster.InstalledHooks.Revision))
	}
	if fresh.Roster.EffectiveContract.Revision != observed.Roster.EffectiveContract.Revision ||
		fresh.Roster.EffectiveContract.Ref != observed.Roster.EffectiveContract.Ref ||
		fresh.Roster.EffectiveContract.Absent != observed.Roster.EffectiveContract.Absent {
		details = append(details, fmt.Sprintf(
			"effective_contract layer drifted (fresh_rev=%s observed_rev=%s)",
			fresh.Roster.EffectiveContract.Revision, observed.Roster.EffectiveContract.Revision))
	}
	if fresh.ContentSHA256 != observed.ContentSHA256 {
		details = append(details, "managed pack body content_sha256 differs")
	}
	return details
}

// instructionPackBody marshals the pack EXCLUDING GeneratedAt for the
// content sha. We marshal a normalized map[string]any with sorted keys so
// any map-key reordering across Go versions cannot perturb identity. The
// serialized body MUST round-trip to a pack with the same ContentSHA256
// (F4 covers this).
func instructionPackBody(p InstructionPack) []byte {
	body := map[string]any{
		"schema_version": p.SchemaVersion,
		"harness":        p.Harness,
		"policy_core":    layerToMap(p.PolicyCore),
		"posture":        layerToMap(p.Posture),
		"roster":         rosterToMap(p.Roster),
	}
	// The preset key is added ONLY when non-empty. Adding it
	// unconditionally (even as "") would perturb the hash of every
	// presetless pack and re-identify every pack already on disk — P4
	// pins that against a literal captured before this axis existed.
	if p.Preset != "" {
		body["preset"] = p.Preset
	}
	normalized := normalizeJSONValue(body)
	out, err := json.Marshal(normalized)
	if err != nil {
		// Best-effort: caller already built everything; should never reach here.
		// Fall back to a non-deterministic blank to surface the fault loudly.
		return []byte("marshal-error:" + err.Error())
	}
	return out
}

func layerToMap(l InstructionPackLayer) map[string]any {
	m := map[string]any{
		"authority_ref":   l.AuthorityRef,
		"source_revision": l.SourceRevision,
		"content_sha256":  l.ContentSHA256,
		"absent":          l.Absent,
	}
	return m
}

func rosterRefToMap(r InstructionPackRosterRef) map[string]any {
	return map[string]any{
		"ref":      r.Ref,
		"revision": r.Revision,
		"absent":   r.Absent,
	}
}

func rosterToMap(r InstructionPackRoster) map[string]any {
	return map[string]any{
		"registry_revision":  r.RegistryRevision,
		"installed_hooks":    rosterRefToMap(r.InstalledHooks),
		"effective_contract": rosterRefToMap(r.EffectiveContract),
	}
}

// normalizeJSONValue recursively forces deterministic key ordering so the
// content sha is independent of Go map iteration order (Go map iteration is
// randomized; we want sha to be deterministic across re-renders).
func normalizeJSONValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, child := range x {
			out[k] = normalizeJSONValue(child)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = normalizeJSONValue(child)
		}
		return out
	default:
		return v
	}
}

// writeInstructionPackAtomic writes the JSON body to path with an
// atomic temp-file rename, mirroring writePermissionFileAtomic's
// discipline. The pack body itself is marshaled with json.MarshalIndent
// so the file is human-readable on disk.
func writeInstructionPackAtomic(path string, pack InstructionPack) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(pack, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// installedHooksManifestRel returns the home-relative ref of the installed
// hooks manifest; used as the InstalledHooks.Ref value so projections stay
// home-scoped.
func installedHooksManifestRel(homePath string) string {
	return filepath.ToSlash(filepath.Join("config", "installed-hooks.yaml"))
}

// installedHooksManifestSchemaVersion is referenced only by an explicit
// schema-validation refusal path — not currently invoked but pinned here
// so a future envelope validator can pull it from this layer without a
// cross-package import.
var _ = strings.HasPrefix
var _ = yaml.Marshal
var _ = sort.Strings
