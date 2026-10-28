package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/mem"
	"compound/internal/persist"
	"compound/internal/persist/pg"
)

const storeMigrateUsage = `usage: compound store migrate (--dry-run | --apply --work W) [--json] [--home P]

Bulk-loads the file-based ledger (claims, edges, candidates) from COMPOUND_HOME
into the Postgres backend. Claims/edges are read through the memory owner's
LOCKED MigrationSnapshot (mem.Store.MigrationSnapshot): a point-in-time copy
that hard-refuses malformed rows and seq regressions — never a raw, unlocked
scan that can tear mid-promotion or silently skip ledger bytes.

Flags:
  --dry-run   Count records without writing (default)
  --apply     Actually write records to Postgres; governed write — requires
              --work W and an allowing PolicyDecision (standing grant for
              capability store.postgres_export.write, external_side_effect
              class, scoped to the opaque nonsecret profile target). On success a
              Receipt+Transition pair is recorded in the home event store.
  --json      Machine-readable output
  --home P    COMPOUND_HOME path (default: home.Resolve — env, then global default; no ancestor adoption)

Environment:
  COMPOUND_STORE                 must be 'postgres'
  COMPOUND_EXPERIMENTAL_POSTGRES must be exactly '1'
  COMPOUND_POSTGRES_PROFILE      nonsecret opaque profile label
  COMPOUND_DSN                   Postgres connection string (never rendered or persisted)
`

type migrateSummary struct {
	Mode              string `json:"mode"` // dry-run|apply
	ClaimsFound       int    `json:"claims_found"`
	EdgesFound        int    `json:"edges_found"`
	CandidatesFound   int    `json:"candidates_found"`
	ClaimsWritten     int    `json:"claims_written,omitempty"`
	EdgesWritten      int    `json:"edges_written,omitempty"`
	CandidatesWritten int    `json:"candidates_written,omitempty"`
	SeqCounter        int64  `json:"seq_counter"`
	SeqHighWater      int64  `json:"seq_high_water"`
	ReceiptID         string `json:"receipt_id,omitempty"`
	TransitionID      string `json:"transition_id,omitempty"`
	Errors            int    `json:"errors,omitempty"`
	Duration          string `json:"duration,omitempty"`
}

func runStoreMigrate(args []string, stdout, stderr io.Writer) int {
	jsonOut := hasFlag(args, "--json")
	apply := hasFlag(args, "--apply")

	if hasUsageFlag(args) {
		fmt.Fprint(stdout, storeMigrateUsage)
		return 0
	}
	cfg, cfgErr := loadExperimentalPostgresConfig()
	if cfgErr != nil {
		fmt.Fprintln(stderr, "store migrate:", cfgErr)
		return 1
	}
	targetRef := cfg.PostgresTargetRef()
	if !apply && !hasFlag(args, "--dry-run") {
		// Default to dry-run
		fmt.Fprintln(stderr, "store migrate: no mode specified, defaulting to --dry-run")
	}

	homePath, err := home.Resolve(valueFlag(args, "--home"))
	if err != nil {
		fmt.Fprintf(stderr, "store migrate: resolve home: %v (run `compound home init`)\n", err)
		return 1
	}

	start := time.Now()
	summary := migrateSummary{Mode: "dry-run"}
	if apply {
		summary.Mode = "apply"
	}

	// Claims/edges come from the owner's locked, validated snapshot — never a
	// raw scanner over the live ledger files.
	memStore, err := mem.OpenReadOnly(homePath)
	if err != nil {
		fmt.Fprintf(stderr, "store migrate: open memory store: %v\n", err)
		return 1
	}
	snapshot, err := memStore.MigrationSnapshot()
	if err != nil {
		fmt.Fprintf(stderr, "store migrate: migration snapshot: %v\n", err)
		return 1
	}
	summary.SeqCounter = snapshot.Counter
	summary.SeqHighWater = snapshot.HighWater

	claims, err := parseClaimsSnapshot(snapshot.ClaimsJSONL)
	if err != nil {
		fmt.Fprintf(stderr, "store migrate: parse claims snapshot: %v\n", err)
		return 1
	}
	if len(claims) == 0 {
		fmt.Fprintf(stderr, "store migrate: no claims in ledger at %s\n", filepath.Join(homePath, "ledger", "claims.jsonl"))
	}
	summary.ClaimsFound = len(claims)

	edges, err := parseEdgesSnapshot(snapshot.EdgesJSONL)
	if err != nil {
		fmt.Fprintf(stderr, "store migrate: parse edges snapshot: %v\n", err)
		return 1
	}
	summary.EdgesFound = len(edges)

	// Candidates are file-per-record (no shared append log), read directly.
	candDir := filepath.Join(homePath, candidates.InboxDir(candidates.Memory))
	cands, candErr := readCandidateFiles(candDir)
	if candErr != nil && !os.IsNotExist(candErr) {
		fmt.Fprintf(stderr, "store migrate: read candidates: %v\n", candErr)
		return 1
	}
	summary.CandidatesFound = len(cands)

	if !apply {
		summary.Duration = time.Since(start).Round(time.Millisecond).String()
		return emitMigrateSummary(stdout, stderr, summary, jsonOut)
	}

	// ─── Apply mode: governed write, gated BEFORE any side effect ─────────────
	work := strings.TrimSpace(valueFlag(args, "--work"))
	if work == "" {
		fmt.Fprintln(stderr, "error: --work W is required for --apply (a governed act belongs to a work)")
		return 2
	}
	now := time.Now()
	ai, intentErr := storeGovernedIntent(work, "migrate_apply", cfg,
		fmt.Sprintf("bulk-load %d claims, %d edges, %d candidates from %s into Postgres", len(claims), len(edges), len(cands), homePath), now)
	if intentErr != nil {
		fmt.Fprintln(stderr, "store migrate: build governed intent:", intentErr)
		return 1
	}
	if _, err := gateStoreGovernedWrite(homePath, ai, now); err != nil {
		fmt.Fprintln(stderr, "store migrate --apply:", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	store, err := pg.Open(ctx, cfg)
	if err != nil {
		writePostgresFailure(stderr, "store migrate", targetRef, "connect", err)
		return 1
	}
	defer store.Close()

	if err := store.EnsureSchema(ctx); err != nil {
		writePostgresFailure(stderr, "store migrate", targetRef, "schema", err)
		return 1
	}

	// Bulk-write each phase inside a single Postgres transaction. A mid-stream
	// crash or row failure now rolls back the whole phase instead of tearing the
	// ledger with a partial export. We stop at the first phase failure so we do
	// not mask a broken earlier phase with a successful later one.
	persistClaims := make([]persist.Claim, len(claims))
	for i, c := range claims {
		persistClaims[i] = mapMemClaimToPersist(c)
	}
	summary.ClaimsWritten, err = store.BulkAppendClaims(ctx, persistClaims)
	if err != nil {
		summary.Errors += len(claims)
		if !jsonOut {
			writePostgresFailure(stderr, "  claims", targetRef, "bulk write", err)
		}
	} else {
		persistEdges := make([]persist.Edge, len(edges))
		for i, e := range edges {
			persistEdges[i] = mapMemEdgeToPersist(e)
		}
		summary.EdgesWritten, err = store.BulkAppendEdges(ctx, persistEdges)
		if err != nil {
			summary.Errors += len(edges)
			if !jsonOut {
				writePostgresFailure(stderr, "  edges", targetRef, "bulk write", err)
			}
		} else {
			persistCands := make([]persist.Candidate, len(cands))
			for i, c := range cands {
				persistCands[i] = mapMemCandidateToPersist(c)
			}
			summary.CandidatesWritten, err = store.BulkAppendCandidates(ctx, persistCands)
			if err != nil {
				summary.Errors += len(cands)
				if !jsonOut {
					writePostgresFailure(stderr, "  candidates", targetRef, "bulk write", err)
				}
			}
		}
	}

	summary.Duration = time.Since(start).Round(time.Millisecond).String()

	if summary.Errors > 0 {
		emitMigrateSummary(stdout, stderr, summary, jsonOut)
		return 1
	}

	// The mutation is complete; record its proof. A proof failure after a
	// successful export is loud, never silent.
	esStore, err := eventstore.Open(homePath)
	if err != nil {
		fmt.Fprintf(stderr, "store migrate: open event store for proof: %v\n", err)
		return 1
	}
	proofSummary := fmt.Sprintf("store migrate --apply: %d claims, %d edges, %d candidates exported to Postgres (seq high-water %d)",
		summary.ClaimsWritten, summary.EdgesWritten, summary.CandidatesWritten, summary.SeqHighWater)
	receiptID, transitionID, err := recordStoreGovernedProof(esStore, ai, proofSummary, now)
	if err != nil {
		fmt.Fprintf(stderr, "store migrate: export applied but proof recording failed: %v\n", err)
		return 1
	}
	summary.ReceiptID = receiptID
	summary.TransitionID = transitionID

	return emitMigrateSummary(stdout, stderr, summary, jsonOut)
}

// ─── Snapshot parsers ────────────────────────────────────────────────────────────
// The snapshot already validated every row (hard-refusing malformed lines and
// seq regressions); these parsers only decode — a decode failure here means the
// owner validation and this decoder disagree, which is a hard error, never a
// skipped row.

func parseClaimsSnapshot(data []byte) ([]mem.Claim, error) {
	var claims []mem.Claim
	for index, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var c mem.Claim
		if err := json.Unmarshal(line, &c); err != nil {
			return nil, fmt.Errorf("claim row %d: %w", index+1, err)
		}
		claims = append(claims, c)
	}
	return claims, nil
}

func parseEdgesSnapshot(data []byte) ([]mem.Edge, error) {
	var edges []mem.Edge
	for index, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e mem.Edge
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("edge row %d: %w", index+1, err)
		}
		edges = append(edges, e)
	}
	return edges, nil
}

func readCandidateFiles(dir string) ([]mem.Candidate, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var cands []mem.Candidate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read candidate %s: %w", entry.Name(), err)
		}
		var c mem.Candidate
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("parse candidate %s: %w", entry.Name(), err)
		}
		if c.ID == "" {
			// Use filename stem as ID if not embedded
			c.ID = strings.TrimSuffix(entry.Name(), ".json")
		}
		cands = append(cands, c)
	}
	return cands, nil
}

// ─── Mappers ─────────────────────────────────────────────────────────────────────

func mapMemClaimToPersist(c mem.Claim) persist.Claim {
	return persist.Claim{
		Seq:             c.Seq,
		ID:              c.ID,
		Entity:          c.Entity,
		Predicate:       c.Predicate,
		Object:          c.Object,
		ObservationDate: c.ValidFrom,
		Confidence:      c.Confidence,
		Derivation:      c.Derivation,
		ClaimType:       c.ClaimType,
		CanonClass:      c.CanonClass,
		Qualifiers:      c.Qualifiers,
		Visibility:      c.Visibility,
		State:           c.State,
		// mem.Claim carries no SourceTraceRef (that field lives on Candidate);
		// provenance stays in EvidenceRefs — never invent a source.
		EvidenceRefs: c.EvidenceRefs,
		CreatedAt:    c.CreatedAt,
	}
}

func mapMemEdgeToPersist(e mem.Edge) persist.Edge {
	return persist.Edge{
		From:      e.From,
		To:        e.To,
		Kind:      e.Kind,
		Seq:       e.Seq,
		CreatedAt: e.CreatedAt,
	}
}

func mapMemCandidateToPersist(c mem.Candidate) persist.Candidate {
	return persist.Candidate{
		ID:              c.ID,
		Entity:          c.Entity,
		Predicate:       c.Predicate,
		Object:          c.Object,
		ObservationDate: c.ObservationDate,
		Confidence:      c.Confidence,
		Derivation:      c.Derivation,
		ClaimType:       c.ClaimType,
		CanonClass:      c.CanonClass,
		Qualifiers:      c.Qualifiers,
		Visibility:      c.Visibility,
		State:           c.State,
		SourceTraceRef:  c.SourceTraceRef,
		EvidenceRefs:    c.EvidenceRefs,
		Reason:          c.Reason,
		AdjudicatedBy:   c.AdjudicatedBy,
		AdjudicatedAt:   c.AdjudicatedAt,
		CreatedAt:       c.CreatedAt,
	}
}

// ─── Output ──────────────────────────────────────────────────────────────────────

func emitMigrateSummary(stdout, stderr io.Writer, summary migrateSummary, jsonOut bool) int {
	if jsonOut {
		if err := encodeJSON(stdout, summary); err != nil {
			fmt.Fprintln(stderr, "store migrate: encode summary:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "store migrate (%s)\n", summary.Mode)
	fmt.Fprintf(stdout, "  claims:     %d found", summary.ClaimsFound)
	if summary.Mode == "apply" {
		fmt.Fprintf(stdout, " → %d written", summary.ClaimsWritten)
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "  edges:      %d found", summary.EdgesFound)
	if summary.Mode == "apply" {
		fmt.Fprintf(stdout, " → %d written", summary.EdgesWritten)
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "  candidates: %d found", summary.CandidatesFound)
	if summary.Mode == "apply" {
		fmt.Fprintf(stdout, " → %d written", summary.CandidatesWritten)
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "  seq:        counter=%d high-water=%d (locked owner snapshot)\n", summary.SeqCounter, summary.SeqHighWater)
	if summary.ReceiptID != "" {
		fmt.Fprintf(stdout, "  proof:      receipt=%s transition=%s\n", summary.ReceiptID, summary.TransitionID)
	}
	if summary.Errors > 0 {
		fmt.Fprintf(stdout, "  errors:     %d\n", summary.Errors)
	}
	if summary.Duration != "" {
		fmt.Fprintf(stdout, "  duration:   %s\n", summary.Duration)
	}
	return 0
}
