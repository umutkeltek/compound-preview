// harness_memory_digest.go — work_67d4af40bba0 (S5): compile PROMOTED positions
// from the standing-law, trap, and harness:<harness> topic subtrees into ONE
// deterministic markdown digest a harness's ambient memory surface can load.
//
// This is a passive, ambient render: nobody asks a specific question before it
// is served, so it takes the STRICTEST reading of the orient visibility gate —
// a claim renders only when it would be unconditionally safe for the live
// orient path to serve, never conditionally on task-specific relevance (the
// "explicit_only" escape hatch internal/contract's MemoryRule grants a matched
// entity query). internal/contract owns the canonical gate
// (memoryVisibilityAllowedForOrient / personalMemoryClaim / selfModelClaim /
// volatileOperationalRepoStateClaim) but internal/contract imports internal/mem,
// so the reverse import would cycle; digestClaimAllowed below is a deliberate,
// documented MIRROR of that same policy (same shape as internal/harness's path
// mirrors of internal/contract's file layout, for the identical reason). A
// gated claim silently excluded here is correct; a gated claim rendered is the
// failure mode the fixture test in harness_memory_digest_test.go pins.
package mem

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// HarnessDigestSchemaVersion is the schema id stamped into both the front
// matter of the rendered file and the JSON report.
const HarnessDigestSchemaVersion = "compound.harness_memory_digest.v0.1"

// HarnessDigestOptions selects the harness (mapped to topic harness:<name>)
// and an optional ledger seq ceiling (0 = latest).
type HarnessDigestOptions struct {
	Harness string
	AsOfSeq int64
}

// HarnessDigestMeta carries the two provenance fields only a CLI caller can
// know: the build/version string (cmd/compound owns versionString()) and the
// exact command line that reproduces this render. Keeping these as caller
// input (rather than internal/mem inventing them) keeps the library
// decoupled from argv/ldflags details.
type HarnessDigestMeta struct {
	GeneratedBy   string
	RegenerateCmd string
}

// HarnessDigestGroupReport is the per-topic-group rollup in the JSON report —
// counts only, never the excluded claims' identities.
type HarnessDigestGroupReport struct {
	Topic         string `json:"topic"`
	PositionCount int    `json:"position_count"`
}

// HarnessDigestReport is the --json report for `compound memory
// render-harness-digest`. It never carries the rendered markdown body or any
// gated-out claim identity — only counts and the provenance header fields
// (which are ALSO embedded, byte-for-byte, in the written file's front matter).
type HarnessDigestReport struct {
	SchemaVersion   string                     `json:"schema_version"`
	Harness         string                     `json:"harness"`
	GeneratedBy     string                     `json:"generated_by"`
	GeneratedAt     string                     `json:"generated_at"`
	SourceLedgerSeq int64                      `json:"source_ledger_seq"`
	RegenerateCmd   string                     `json:"regenerate_cmd"`
	ContentSHA256   string                     `json:"content_sha256"`
	Groups          []HarnessDigestGroupReport `json:"groups"`
	PositionCount   int                        `json:"position_count"`
	ExcludedCount   int                        `json:"excluded_count"`
	OutPath         string                     `json:"out_path,omitempty"`
}

type digestRow struct {
	Entity     string
	Topic      string
	Stance     string
	Seq        int64
	PositionID string
}

type digestGroupRender struct {
	Topic string
	Rows  []digestRow
}

// harnessDigestTopics is the FIXED, declared group order (spec: "stable
// deterministic ordering (by group, then entity, then seq)"). Order is part
// of the output contract, never map/discovery order.
func harnessDigestTopics(harness string) []string {
	return []string{"standing-law", "trap", "harness:" + harness}
}

// RenderHarnessDigest folds PROMOTED, ambient-visible positions under the
// standing-law, trap, and harness:<harness> topic subtrees into a deterministic
// markdown body plus a matching JSON report. It performs no I/O beyond reading
// the ledger — the caller (CLI) owns writing the body to --out via
// WriteHarnessDigestAtomic.
func (s *Store) RenderHarnessDigest(opts HarnessDigestOptions, meta HarnessDigestMeta, now time.Time) (HarnessDigestReport, []byte, error) {
	harness := strings.TrimSpace(opts.Harness)
	if harness == "" {
		return HarnessDigestReport{}, nil, fmt.Errorf("harness required")
	}

	claims, err := s.loadClaimsAsOf(opts.AsOfSeq)
	if err != nil {
		return HarnessDigestReport{}, nil, err
	}
	asOf := opts.AsOfSeq
	if asOf == 0 {
		asOf, err = s.maxLedgerSeq()
		if err != nil {
			return HarnessDigestReport{}, nil, err
		}
	}
	superseded, err := s.supersededAsOf(asOf)
	if err != nil {
		return HarnessDigestReport{}, nil, err
	}
	claimByID := make(map[string]Claim, len(claims))
	for _, c := range claims {
		claimByID[c.ID] = c
	}

	// Resolved once per render from the store's own home, then threaded
	// through the fold below — see self_entity_config.go.
	selfEntities := ResolveSelfEntitySet(s.home)

	var groupRenders []digestGroupRender
	var groupReports []HarnessDigestGroupReport
	totalIncluded, totalExcluded := 0, 0

	for _, topic := range harnessDigestTopics(harness) {
		topicSet, _, err := s.topicScopeSet(topic)
		if err != nil {
			return HarnessDigestReport{}, nil, err
		}
		positions := foldPositionRows(claims, superseded, asOf, "", "", topicSet)

		var rows []digestRow
		for _, p := range positions {
			// Only a fully-settled, current stance ever renders: contested
			// (needs_review anywhere in the chain) and retired positions are
			// excluded exactly like the live orient path excludes them.
			if p.Status != "active" {
				totalExcluded++
				continue
			}
			claim, ok := claimByID[p.ActiveClaimID]
			if !ok || !digestClaimAllowed(claim, selfEntities) {
				totalExcluded++
				continue
			}
			rows = append(rows, digestRow{
				Entity:     p.Entity,
				Topic:      p.Topic,
				Stance:     p.ActiveObject,
				Seq:        claim.Seq,
				PositionID: p.ID,
			})
			totalIncluded++
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Entity != rows[j].Entity {
				return rows[i].Entity < rows[j].Entity
			}
			if rows[i].Seq != rows[j].Seq {
				return rows[i].Seq < rows[j].Seq
			}
			return rows[i].PositionID < rows[j].PositionID
		})

		groupRenders = append(groupRenders, digestGroupRender{Topic: topic, Rows: rows})
		groupReports = append(groupReports, HarnessDigestGroupReport{Topic: topic, PositionCount: len(rows)})
	}

	body := renderHarnessDigestBody(harness, groupRenders)
	contentSHA := sha256Hex(body)
	generatedAt := rfc3339(now)
	header := renderHarnessDigestHeader(digestHeaderFields{
		SchemaVersion:   HarnessDigestSchemaVersion,
		Harness:         harness,
		GeneratedBy:     meta.GeneratedBy,
		GeneratedAt:     generatedAt,
		SourceLedgerSeq: asOf,
		RegenerateCmd:   meta.RegenerateCmd,
		ContentSHA256:   contentSHA,
	})
	full := make([]byte, 0, len(header)+len(body))
	full = append(full, header...)
	full = append(full, body...)

	report := HarnessDigestReport{
		SchemaVersion:   HarnessDigestSchemaVersion,
		Harness:         harness,
		GeneratedBy:     meta.GeneratedBy,
		GeneratedAt:     generatedAt,
		SourceLedgerSeq: asOf,
		RegenerateCmd:   meta.RegenerateCmd,
		ContentSHA256:   contentSHA,
		Groups:          groupReports,
		PositionCount:   totalIncluded,
		ExcludedCount:   totalExcluded,
	}
	return report, full, nil
}

type digestHeaderFields struct {
	SchemaVersion   string
	Harness         string
	GeneratedBy     string
	GeneratedAt     string
	SourceLedgerSeq int64
	RegenerateCmd   string
	ContentSHA256   string
}

// renderHarnessDigestHeader writes a YAML-front-matter provenance block. Every
// field here is metadata ABOUT the body, never hashed into ContentSHA256 (so
// content_sha256 stays stable across re-renders of an unchanged ledger even
// though generated_at moves) — mirrors internal/harness's InstructionPack
// GeneratedAt-excluded hashing.
func renderHarnessDigestHeader(f digestHeaderFields) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("schema_version: " + f.SchemaVersion + "\n")
	b.WriteString("harness: " + f.Harness + "\n")
	b.WriteString("generated_by: " + f.GeneratedBy + "\n")
	b.WriteString("generated_at: " + f.GeneratedAt + "\n")
	b.WriteString(fmt.Sprintf("source_ledger_seq: %d\n", f.SourceLedgerSeq))
	b.WriteString("regenerate_cmd: " + f.RegenerateCmd + "\n")
	b.WriteString("content_sha256: " + f.ContentSHA256 + "\n")
	b.WriteString("---\n\n")
	return []byte(b.String())
}

// renderHarnessDigestBody renders the substantive, hashed content: one H3
// section per topic group (fixed order), one compact entity/topic/stance line
// per included position (sorted entity then seq then position id — spec
// point 2's "by group, then entity, then seq"). An empty group still renders
// its header + "(none)" so the file's shape never depends on ledger content.
func renderHarnessDigestBody(harness string, groups []digestGroupRender) []byte {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# Harness Memory Digest: %s\n\n", harness))
	for _, g := range groups {
		b.WriteString("### " + g.Topic + "\n\n")
		if len(g.Rows) == 0 {
			b.WriteString("(none)\n\n")
			continue
		}
		for _, r := range g.Rows {
			b.WriteString(fmt.Sprintf("- `%s` · %s → %s (since-seq %d, pos %s)\n", r.Entity, r.Topic, r.Stance, r.Seq, r.PositionID))
		}
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// WriteHarnessDigestAtomic writes the rendered body to an absolute path via a
// temp-file-then-rename, mirroring internal/harness's writeInstructionPackAtomic
// discipline. It creates the parent directory if needed but never invents the
// path itself — the caller (CLI) already validated --out is absolute and
// outside the repo.
func WriteHarnessDigestAtomic(path string, body []byte) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("path required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(body); err != nil {
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

// HarnessDigestDriftReport is the --json report for `compound memory
// digest-doctor`: the declared vs recomputed content hash, read-only.
type HarnessDigestDriftReport struct {
	SchemaVersion           string `json:"schema_version"`
	Path                    string `json:"path"`
	DeclaredContentSHA256   string `json:"declared_content_sha256"`
	RecomputedContentSHA256 string `json:"recomputed_content_sha256"`
	Drifted                 bool   `json:"drifted"`
	Detail                  string `json:"detail,omitempty"`
}

// VerifyHarnessDigestFile reads an on-disk digest, recomputes the sha256 of
// the body BELOW the front matter, and compares it to the front matter's
// declared content_sha256. Purely read-only — it never mutates the file, the
// ledger, or the CurrentView (drift-doctor discipline mirrors
// InstructionPackDrift's F10 invariant).
func VerifyHarnessDigestFile(path string) (HarnessDigestDriftReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return HarnessDigestDriftReport{}, err
	}
	declared, body, err := splitHarnessDigestFrontMatter(data)
	if err != nil {
		return HarnessDigestDriftReport{}, err
	}
	recomputed := sha256Hex(body)
	report := HarnessDigestDriftReport{
		SchemaVersion:           HarnessDigestSchemaVersion,
		Path:                    path,
		DeclaredContentSHA256:   declared,
		RecomputedContentSHA256: recomputed,
	}
	switch {
	case declared == "":
		report.Drifted = true
		report.Detail = "no content_sha256 found in front matter"
	case declared != recomputed:
		report.Drifted = true
		report.Detail = "on-disk body no longer matches the declared content_sha256 (hand-edit or stale regenerate)"
	}
	return report, nil
}

// splitHarnessDigestFrontMatter splits a rendered digest file into its
// declared content_sha256 and the body bytes exactly as
// renderHarnessDigestBody produced them (the writer's layout is
// header("---\n"...fields..."---\n\n") + body — this is the read-side mirror
// of that exact byte shape).
func splitHarnessDigestFrontMatter(data []byte) (declaredSHA string, body []byte, err error) {
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", nil, fmt.Errorf("missing front matter opening delimiter (---)")
	}
	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "---" {
			closeIdx = i
			break
		}
		if strings.HasPrefix(trimmed, "content_sha256:") {
			declaredSHA = strings.TrimSpace(strings.TrimPrefix(trimmed, "content_sha256:"))
		}
	}
	if closeIdx == -1 {
		return "", nil, fmt.Errorf("missing front matter closing delimiter (---)")
	}
	bodyStart := closeIdx + 1
	if bodyStart < len(lines) && strings.TrimSpace(lines[bodyStart]) == "" {
		bodyStart++ // drop the single blank separator line the header writer emits
	}
	return declaredSHA, []byte(strings.Join(lines[bodyStart:], "\n")), nil
}

// digestClaimAllowed is the strict ambient-visibility gate — see the package
// doc comment above for why this mirrors (rather than imports)
// internal/contract's orient gate. selfEntities is the resolved self-entity
// set for the home (see self_entity_config.go), resolved once by the caller.
func digestClaimAllowed(claim Claim, selfEntities map[string]bool) bool {
	if strings.EqualFold(strings.TrimSpace(claim.ClaimType), "decision") {
		return false
	}
	if isVolatileVisibility(claim.Visibility) {
		return false
	}
	if !digestVisibilityWhitelisted(claim.Visibility) {
		return false
	}
	if digestPersonalOrSelfModelClaim(claim, selfEntities) {
		return false
	}
	if digestVolatileOperationalRepoStateClaim(claim) {
		return false
	}
	return true
}

// digestVisibilityWhitelisted mirrors internal/contract.memoryVisibilityAllowed:
// only these visibilities are EVER eligible for an ambient surface. Anything
// else (private, volatile, candidate, retired, or an unrecognized value)
// fails closed.
func digestVisibilityWhitelisted(visibility string) bool {
	switch strings.ToLower(strings.TrimSpace(visibility)) {
	case "", "ambient", "contextual", "operational", "world", "public":
		return true
	default:
		return false
	}
}

// digestPersonalOrSelfModelClaim mirrors internal/contract.personalMemoryClaim:
// private visibility, preference claims, self-model entities/qualifiers, and
// corpus-sourced qualifiers are personal-corpus content that never belongs on
// an ambient harness surface, independent of the topic it was tagged under.
func digestPersonalOrSelfModelClaim(claim Claim, selfEntities map[string]bool) bool {
	return strings.EqualFold(strings.TrimSpace(claim.Visibility), "private") ||
		strings.EqualFold(strings.TrimSpace(claim.ClaimType), "preference") ||
		digestSelfModelClaim(claim, selfEntities) ||
		digestQualifierContains(claim, "source", "corpus")
}

// digestSelfModelClaim mirrors internal/contract.selfModelClaim: membership
// in the resolved self-entity set (always "operator", extendable via
// COMPOUND_HOME/config/identity.yaml — see self_entity_config.go) rather
// than one hardcoded name. A nil selfEntities (defensive/test callers that
// skip resolution) falls back to the bare "operator" default so privacy
// classification never goes silently permissive.
func digestSelfModelClaim(claim Claim, selfEntities map[string]bool) bool {
	key := digestMemoryKey(claim.Entity)
	if selfEntities == nil {
		if key == DefaultSelfEntity {
			return true
		}
	} else if selfEntities[key] {
		return true
	}
	return digestQualifierContains(claim, "domain", "self") || digestQualifierContains(claim, "domain", "personal")
}

func digestQualifierContains(claim Claim, key, needle string) bool {
	if claim.Qualifiers == nil {
		return false
	}
	return strings.Contains(strings.ToLower(claim.Qualifiers[key]), needle)
}

// digestMemoryKey mirrors internal/contract.memoryKey: lowercase, keep only
// the segment after the last ':', keep only [a-z0-9].
func digestMemoryKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if i := strings.LastIndex(value, ":"); i >= 0 && i+1 < len(value) {
		value = value[i+1:]
	}
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// digestVolatileOperationalRepoStateClaim mirrors
// internal/contract.volatileOperationalRepoStateClaim: clocked git-repo-state
// facts (HEAD, ahead/behind counts, …) never enter ambient memory even when
// visibility=operational.
func digestVolatileOperationalRepoStateClaim(claim Claim) bool {
	if !strings.EqualFold(strings.TrimSpace(claim.Visibility), "operational") {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(claim.ClaimType), "status") {
		return false
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(claim.Entity)), "repo:") {
		return false
	}
	switch digestMemoryKey(claim.Predicate) {
	case "head", "aheadcount", "behindcount", "stagedcount", "unstagedcount", "untrackedcount", "changedpathcount", "sourcecontrolstatus":
		return true
	default:
		return false
	}
}
