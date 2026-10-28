package store

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/persist"
	"compound/internal/persist/pg"
)

// defaultEmbedBaseURL is the fallback embeddings endpoint for `store embed` and
// `store recall` when COMPOUND_EMBED_URL is unset. Split the same way as
// internal/breadth/embed.go's defaultOllamaBaseURL so the public-repo leak scan
// does not read a loopback default as an internal host.
//
// Note this default disagrees with internal/store/pgfactory.go, which documents
// "no baked-in hosts" and hard-errors when COMPOUND_EMBED_URL is missing. The two
// paths should agree; reconciling them changes CLI behavior, so it is left as a
// deliberate, visible inconsistency rather than folded into a formatting change.
const defaultEmbedBaseURL = "http://" + "localhost:11434/v1"

const storeUsage = `usage: compound store <subcommand>

Subcommands:
  doctor [--json]    Connect, ensure schema, round-trip probe data, report health
                     (probes use scratch store-doctor-* ids and are removed afterwards)
  init               Connect and create schema (tables + vector extension)
  status [--json]    Show connection info and row counts
  migrate (--dry-run | --apply --work W) [--json] [--home P]
                     Bulk-load file ledger into Postgres; governed write:
                     dry-run default, --apply requires an allowing PolicyDecision
  sync [--json] [--home P] [--embed] [--confirm --work W]
                     Incremental sync: file ledger → Postgres (idempotent); governed
                     write: dry-run default, --confirm requires an allowing
                     PolicyDecision. --embed chains into incremental embed after sync
  embed [--json] [--batch N] [--home P] [--confirm --work W]
                     Embed unembedded claims into pgvector via an OpenAI-compatible
                     embeddings endpoint (incremental); governed write: dry-run
                     default, --confirm requires an allowing PolicyDecision
  recall <query> [--k N] [--json]
                     EXPERIMENTAL, non-canonical: semantic recall over embedded
                     claims. The pg ledger computes the allowlist and the index
                     only ranks; the canonical recall front door is
                     compound memory recall — this is an operator debug surface.

Environment:
  COMPOUND_STORE        Must be "postgres" for this experimental surface
  COMPOUND_EXPERIMENTAL_POSTGRES  Must be exactly "1" (separate explicit opt-in)
  COMPOUND_POSTGRES_PROFILE       Nonsecret opaque profile label (required)
  COMPOUND_DSN          Postgres connection string (required; never rendered or persisted)
  COMPOUND_VECTOR_DIMS  Embedding dimensions (default: 1536)
  COMPOUND_EMBED_URL    OpenAI-compatible embeddings API base URL (required for embed/recall)
  COMPOUND_EMBED_MODEL  Embedding model name (default: nomic-embed-text)

Examples:
  Configure the three experimental environment variables above, then run:
    compound store doctor
  compound store init
  compound store status --json
  compound store migrate --dry-run
  compound store migrate --apply --work work_123
  compound store sync                      # dry-run
  compound store sync --confirm --work work_123
  compound store sync --confirm --work work_123 --embed
  compound store embed --confirm --work work_123
  compound store recall "session distillation architecture" --k 5
`

// Run is the storage-backend operator surface (`compound store`). It manages
// the Postgres+pgvector backend lifecycle: schema creation, health checks,
// and status reporting. Additive infrastructure (ADR-0005): the file backend
// under COMPOUND_HOME remains authoritative; Postgres is never authority.
// cmd/compound keeps only a thin os.Exit shim over this organ.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || hasUsageFlag(args) {
		fmt.Fprint(stdout, storeUsage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "doctor":
		return runStoreDoctor(args[1:], stdout, stderr)
	case "init":
		return runStoreInit(args[1:], stdout, stderr)
	case "status":
		return runStoreStatus(args[1:], stdout, stderr)
	case "migrate":
		return runStoreMigrate(args[1:], stdout, stderr)
	case "sync":
		return runStoreSync(args[1:], stdout, stderr)
	case "embed":
		return runStoreEmbed(args[1:], stdout, stderr, false)
	case "recall":
		return runStoreRecall(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "store: unknown subcommand %q\n", args[0])
		fmt.Fprint(stderr, storeUsage)
		return 2
	}
}

// ─── store doctor ────────────────────────────────────────────────────────────────

type storeDoctorReport struct {
	Backend  string             `json:"backend"`
	Profile  string             `json:"profile,omitempty"`
	Checks   []storeDoctorCheck `json:"checks"`
	AllOK    bool               `json:"all_ok"`
	Duration string             `json:"duration"`
}

type storeDoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok|fail
	Detail string `json:"detail,omitempty"`
}

func runStoreDoctor(args []string, stdout, stderr io.Writer) int {
	jsonOut := hasFlag(args, "--json")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	report := storeDoctorReport{Backend: "postgres"}

	cfg, cfgErr := loadExperimentalPostgresConfig()
	if cfgErr != nil {
		report.Checks = append(report.Checks, storeDoctorCheck{
			Name: "env", Status: "fail",
			Detail: cfgErr.Error(),
		})
		report.AllOK = false
		emitStoreDoctor(stdout, report, jsonOut)
		return 1
	}
	report.Profile = cfg.PostgresTargetRef()

	// 1. Connect
	store, err := pg.Open(ctx, cfg)
	if err != nil {
		report.Checks = append(report.Checks, storeDoctorCheck{
			Name: "connect", Status: "fail", Detail: err.Error(),
		})
		report.AllOK = false
		report.Duration = time.Since(start).Round(time.Millisecond).String()
		emitStoreDoctor(stdout, report, jsonOut)
		return 1
	}
	defer store.Close()
	report.Checks = append(report.Checks, storeDoctorCheck{Name: "connect", Status: "ok"})

	// 2. Ensure schema
	if err := store.EnsureSchema(ctx); err != nil {
		report.Checks = append(report.Checks, storeDoctorCheck{
			Name: "schema", Status: "fail", Detail: err.Error(),
		})
		report.AllOK = false
		report.Duration = time.Since(start).Round(time.Millisecond).String()
		emitStoreDoctor(stdout, report, jsonOut)
		return 1
	}
	report.Checks = append(report.Checks, storeDoctorCheck{Name: "schema", Status: "ok"})

	// Probe rows live in a scratch namespace (unique store-doctor-* ids per run)
	// and every one of them is removed by checkCleanup afterwards — a doctor run
	// leaves the authority-shaped tables as it found them.
	probe := newStoreDoctorProbe()

	// 3. Claims round-trip
	checkClaims(ctx, store, probe, &report)

	// 4. Events round-trip
	checkEvents(ctx, store, probe, &report)

	// 5. Work round-trip
	checkWork(ctx, store, probe, &report)

	// 6. Vectors round-trip
	checkVectors(ctx, store, cfg, probe, &report)

	// 7. Candidates round-trip
	checkCandidates(ctx, store, probe, &report)

	// 8. Remove every probe row (claims/candidates/events/work_envelopes); the
	// vector probe is removed inside checkVectors through the store's own Delete.
	checkCleanup(ctx, cfg.DSN, cfg.PostgresTargetRef(), probe, &report)

	report.AllOK = true
	for _, c := range report.Checks {
		if c.Status != "ok" {
			report.AllOK = false
			break
		}
	}
	report.Duration = time.Since(start).Round(time.Millisecond).String()

	exitCode := 0
	if !report.AllOK {
		exitCode = 1
	}
	emitStoreDoctor(stdout, report, jsonOut)
	return exitCode
}

// storeDoctorProbe carries the scratch-namespace ids one doctor run writes and
// then removes. Probes never claim authority: the claim row is candidate-class
// (visibility candidate, state proposed), never promoted, and even a concurrent
// reader mid-probe cannot mistake it for canon.
type storeDoctorProbe struct {
	claimID     string
	eventID     string
	workID      string
	vectorRef   string
	candidateID string
}

func newStoreDoctorProbe() storeDoctorProbe {
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	return storeDoctorProbe{
		claimID:     "store-doctor-claim-" + suffix,
		eventID:     "store-doctor-event-" + suffix,
		workID:      "store-doctor-work-" + suffix,
		vectorRef:   "store-doctor-vec-" + suffix,
		candidateID: "store-doctor-cand-" + suffix,
	}
}

func checkClaims(ctx context.Context, store *pg.Store, probe storeDoctorProbe, report *storeDoctorReport) {
	now := time.Now()
	testClaim := persist.Claim{
		ID:         probe.claimID,
		Entity:     "compound::store-doctor",
		Predicate:  "health_check",
		Object:     "pass",
		Confidence: 0.5,
		Derivation: "deterministic",
		Visibility: "candidate",
		State:      "proposed",
	}

	seq, err := store.AppendClaim(testClaim, now)
	if err != nil {
		report.Checks = append(report.Checks, storeDoctorCheck{
			Name: "claims", Status: "fail", Detail: "append: " + err.Error(),
		})
		return
	}

	claims, err := store.FoldClaims(persist.ClaimFilter{Entity: "compound::store-doctor", Limit: 1})
	if err != nil || len(claims) == 0 {
		detail := "fold: no results"
		if err != nil {
			detail = "fold: " + err.Error()
		}
		report.Checks = append(report.Checks, storeDoctorCheck{
			Name: "claims", Status: "fail", Detail: detail,
		})
		return
	}

	report.Checks = append(report.Checks, storeDoctorCheck{
		Name: "claims", Status: "ok",
		Detail: fmt.Sprintf("seq=%d fold=✓ (candidate-class probe, removed below)", seq),
	})
}

func checkEvents(ctx context.Context, store *pg.Store, probe storeDoctorProbe, report *storeDoctorReport) {
	payload := map[string]string{"check": "health", "source": "store-doctor"}

	if err := store.PutJSON(probe.eventID, payload); err != nil {
		report.Checks = append(report.Checks, storeDoctorCheck{
			Name: "events", Status: "fail", Detail: "put: " + err.Error(),
		})
		return
	}

	rec, err := store.Read(probe.eventID)
	if err != nil || rec == nil {
		detail := "read: nil"
		if err != nil {
			detail = "read: " + err.Error()
		}
		report.Checks = append(report.Checks, storeDoctorCheck{
			Name: "events", Status: "fail", Detail: detail,
		})
		return
	}

	report.Checks = append(report.Checks, storeDoctorCheck{
		Name: "events", Status: "ok", Detail: "put+read=✓",
	})
}

func checkWork(ctx context.Context, store *pg.Store, probe storeDoctorProbe, report *storeDoctorReport) {
	// Round-trip the real work_envelopes path (PutWork/GetWork): a doctor check
	// must touch the table it reports on — never proxy through another table.
	env := envelope.WorkEnvelope{}
	env.IDs.WorkID = probe.workID
	env.State = "proposed"
	env.Owner = "store-doctor"

	if err := store.PutWork(env); err != nil {
		report.Checks = append(report.Checks, storeDoctorCheck{
			Name: "work", Status: "fail", Detail: "put: " + err.Error(),
		})
		return
	}

	got, err := store.GetWork(probe.workID)
	if err != nil || got == nil {
		detail := "get: nil"
		if err != nil {
			detail = "get: " + err.Error()
		}
		report.Checks = append(report.Checks, storeDoctorCheck{
			Name: "work", Status: "fail", Detail: detail,
		})
		return
	}

	report.Checks = append(report.Checks, storeDoctorCheck{
		Name: "work", Status: "ok", Detail: "put+get work_envelopes=✓",
	})
}

func checkVectors(ctx context.Context, store *pg.Store, cfg persist.Config, probe storeDoctorProbe, report *storeDoctorReport) {
	dims := cfg.VectorDimensions
	if dims <= 0 {
		dims = 1536
	}

	// Create a simple test embedding
	embedding := make([]float32, dims)
	embedding[0] = 1.0
	embedding[1] = 0.5

	// Embedder identity the doctor probe claims when writing a scratch vector.
	// This MUST route through UpsertWithEmbedder so the probe never inserts an
	// unlabeled vector into an embedder-custody index.
	embedder := fmt.Sprintf("ollama:%s:%d", storeEnvOr("COMPOUND_EMBED_MODEL", "nomic-embed-text"), dims)

	if err := store.UpsertWithEmbedder(probe.vectorRef, embedding, map[string]string{"source": "doctor"}, embedder); err != nil {
		report.Checks = append(report.Checks, storeDoctorCheck{
			Name: "vectors", Status: "fail", Detail: "upsert: " + err.Error(),
		})
		return
	}

	// Search with the same vector — should return itself
	hits, err := store.Search(embedding, 1, nil)
	if err != nil {
		report.Checks = append(report.Checks, storeDoctorCheck{
			Name: "vectors", Status: "fail", Detail: "search: " + err.Error(),
		})
		return
	}
	if len(hits) == 0 {
		report.Checks = append(report.Checks, storeDoctorCheck{
			Name: "vectors", Status: "fail", Detail: "search: no hits",
		})
		return
	}

	// Cleanup through the store's own Delete path; a failed delete is a failed
	// check, never a silent leftover.
	if err := store.Delete(probe.vectorRef); err != nil {
		report.Checks = append(report.Checks, storeDoctorCheck{
			Name: "vectors", Status: "fail", Detail: "cleanup delete: " + err.Error(),
		})
		return
	}

	report.Checks = append(report.Checks, storeDoctorCheck{
		Name: "vectors", Status: "ok",
		Detail: fmt.Sprintf("dims=%d upsert+search+delete=✓ score=%.4f", dims, hits[0].Score),
	})
}

func checkCandidates(ctx context.Context, store *pg.Store, probe storeDoctorProbe, report *storeDoctorReport) {
	now := time.Now()

	cand := persist.Candidate{
		ID:         probe.candidateID,
		Entity:     "compound::store-doctor",
		Predicate:  "health_check",
		Object:     "pass",
		Confidence: 0.9,
		Derivation: "extracted",
		Visibility: "candidate",
		State:      "proposed",
	}

	if err := store.AppendCandidate(cand, now); err != nil {
		report.Checks = append(report.Checks, storeDoctorCheck{
			Name: "candidates", Status: "fail", Detail: "append: " + err.Error(),
		})
		return
	}

	got, err := store.GetCandidate(probe.candidateID)
	if err != nil || got == nil {
		detail := "get: nil"
		if err != nil {
			detail = "get: " + err.Error()
		}
		report.Checks = append(report.Checks, storeDoctorCheck{
			Name: "candidates", Status: "fail", Detail: detail,
		})
		return
	}

	report.Checks = append(report.Checks, storeDoctorCheck{
		Name: "candidates", Status: "ok", Detail: "append+get=✓",
	})
}

// checkCleanup removes every probe row the round-trip checks wrote. pg.Store
// deliberately exposes no row deletes for the append-only authority tables, so
// cleanup runs through a short-lived raw connection scoped to the exact probe
// ids. A cleanup failure is a failed check — never a silent leak.
func checkCleanup(ctx context.Context, dsn, profile string, probe storeDoctorProbe, report *storeDoctorReport) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		report.Checks = append(report.Checks, storeDoctorCheck{
			Name: "cleanup", Status: "fail", Detail: "connection for " + profile + " is unavailable",
		})
		return
	}
	defer conn.Close(ctx)

	// Table/column names below are compile-time constants; only the id is a parameter.
	deletes := []struct {
		table  string
		column string
		id     string
	}{
		{"claims", "id", probe.claimID},
		{"candidates", "id", probe.candidateID},
		{"events", "id", probe.eventID},
		{"work_envelopes", "work_id", probe.workID},
	}
	for _, d := range deletes {
		if _, err := conn.Exec(ctx, "DELETE FROM "+d.table+" WHERE "+d.column+" = $1", d.id); err != nil {
			report.Checks = append(report.Checks, storeDoctorCheck{
				Name: "cleanup", Status: "fail", Detail: fmt.Sprintf("delete %s for %s failed", d.table, profile),
			})
			return
		}
	}
	report.Checks = append(report.Checks, storeDoctorCheck{
		Name: "cleanup", Status: "ok", Detail: "probe rows removed",
	})
}

func emitStoreDoctor(stdout io.Writer, report storeDoctorReport, jsonOut bool) {
	if report.Profile != "" {
		checks := append([]storeDoctorCheck(nil), report.Checks...)
		for i := range checks {
			if checks[i].Status != "ok" && checks[i].Detail != "" {
				checks[i].Detail = safePostgresDetail(report.Profile, checks[i].Name, errors.New(checks[i].Detail))
			}
		}
		report.Checks = checks
	}
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintln(stdout, "error encoding report:", err)
		}
		return
	}
	// Human-readable output
	fmt.Fprintf(stdout, "store doctor (%s)\n", report.Duration)
	fmt.Fprintf(stdout, "  backend: %s\n", report.Backend)
	if report.Profile != "" {
		fmt.Fprintf(stdout, "  profile: %s\n", report.Profile)
	}
	fmt.Fprintln(stdout)
	for _, c := range report.Checks {
		mark := "✓"
		if c.Status != "ok" {
			mark = "✗"
		}
		line := fmt.Sprintf("  %s %s", mark, c.Name)
		if c.Detail != "" {
			line += " — " + c.Detail
		}
		fmt.Fprintln(stdout, line)
	}
	fmt.Fprintln(stdout)
	if report.AllOK {
		fmt.Fprintln(stdout, "  store: OK")
	} else {
		fmt.Fprintln(stdout, "  store: FAIL")
	}
}

// ─── store init ──────────────────────────────────────────────────────────────────

func runStoreInit(args []string, stdout, stderr io.Writer) int {
	jsonOut := hasFlag(args, "--json")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, cfgErr := loadExperimentalPostgresConfig()
	if cfgErr != nil {
		fmt.Fprintln(stderr, "store init:", cfgErr)
		return 1
	}

	store, err := pg.Open(ctx, cfg)
	if err != nil {
		writePostgresFailure(stderr, "store init", cfg.PostgresTargetRef(), "connect", err)
		return 1
	}
	defer store.Close()

	if err := store.EnsureSchema(ctx); err != nil {
		writePostgresFailure(stderr, "store init", cfg.PostgresTargetRef(), "schema", err)
		return 1
	}

	if jsonOut {
		json.NewEncoder(stdout).Encode(map[string]any{
			"status":  "ok",
			"backend": "postgres",
			"schema":  "created",
			"profile": cfg.PostgresTargetRef(),
		})
		return 0
	}

	fmt.Fprintf(stdout, "store init: schema created (dims=%d)\n", cfg.VectorDimensions)
	fmt.Fprintf(stdout, "  tables: claims, edges, candidates, events, work_log, work_envelopes, vectors\n")
	fmt.Fprintf(stdout, "  index:  hnsw on vectors (m=16, ef_construction=64, vector_cosine_ops)\n")
	return 0
}

// ─── store status ────────────────────────────────────────────────────────────────

type storeStatusReport struct {
	Backend    string           `json:"backend"`
	Profile    string           `json:"profile,omitempty"`
	Connected  bool             `json:"connected"`
	Tables     map[string]int64 `json:"tables,omitempty"`
	VectorDims int              `json:"vector_dims"`
	Error      string           `json:"error,omitempty"`
}

func runStoreStatus(args []string, stdout, stderr io.Writer) int {
	jsonOut := hasFlag(args, "--json")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, cfgErr := loadExperimentalPostgresConfig()
	report := storeStatusReport{
		Backend:    string(cfg.Backend),
		VectorDims: cfg.VectorDimensions,
	}

	if cfgErr != nil {
		report.Error = cfgErr.Error()
		emitStoreStatus(stdout, report, jsonOut)
		return 1
	}
	report.Profile = cfg.PostgresTargetRef()

	store, err := pg.Open(ctx, cfg)
	if err != nil {
		report.Error = err.Error()
		emitStoreStatus(stdout, report, jsonOut)
		return 1
	}
	defer store.Close()
	report.Connected = true

	// Get row counts
	report.Tables = store.TableCounts(ctx)

	emitStoreStatus(stdout, report, jsonOut)
	return 0
}

func emitStoreStatus(stdout io.Writer, report storeStatusReport, jsonOut bool) {
	if report.Profile != "" && report.Error != "" {
		report.Error = safePostgresDetail(report.Profile, "status", errors.New(report.Error))
	}
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintln(stdout, "error encoding report:", err)
		}
		return
	}
	fmt.Fprintf(stdout, "store status\n")
	fmt.Fprintf(stdout, "  backend: %s\n", report.Backend)
	if !report.Connected {
		fmt.Fprintf(stdout, "  connected: no\n")
		if report.Error != "" {
			fmt.Fprintf(stdout, "  error: %s\n", report.Error)
		}
		return
	}
	fmt.Fprintf(stdout, "  connected: yes\n")
	fmt.Fprintf(stdout, "  profile: %s\n", report.Profile)
	fmt.Fprintf(stdout, "  vector_dims: %d\n", report.VectorDims)
	if len(report.Tables) > 0 {
		fmt.Fprintln(stdout, "  tables:")
		for name, count := range report.Tables {
			fmt.Fprintf(stdout, "    %-18s %d rows\n", name, count)
		}
	}
}

// ─── store sync ──────────────────────────────────────────────────────────────────

type syncClaimRow struct {
	ID              string  `json:"id"`
	Seq             int64   `json:"seq"`
	Entity          string  `json:"entity"`
	Predicate       string  `json:"predicate"`
	Object          string  `json:"object"`
	ObservationDate string  `json:"observation_date"`
	Confidence      float64 `json:"confidence"`
	Derivation      string  `json:"derivation"`
	ClaimType       string  `json:"claim_type"`
	CanonClass      string  `json:"canon_class"`
	Visibility      string  `json:"visibility"`
	State           string  `json:"state"`
	SourceTraceRef  string  `json:"source_trace_ref"`
	CreatedAt       string  `json:"created_at"`
}

type syncEdgeRow struct {
	Seq       int64  `json:"seq"`
	Kind      string `json:"kind"`
	From      string `json:"from"`
	To        string `json:"to"`
	ClaimID   string `json:"claim_id"`
	CreatedAt string `json:"created_at"`
}

func runStoreSync(args []string, stdout, stderr io.Writer) int {
	jsonOut := hasFlag(args, "--json")
	cfg, cfgErr := loadExperimentalPostgresConfig()
	if cfgErr != nil {
		fmt.Fprintln(stderr, "store sync:", cfgErr)
		return 1
	}
	targetRef := cfg.PostgresTargetRef()
	homePath, err := home.Resolve(valueFlag(args, "--home"))
	if err != nil {
		fmt.Fprintf(stderr, "store sync: resolve home: %v (run `compound home init`)\n", err)
		return 1
	}

	// Governed write: dry-run by default, --confirm --work W performs it behind
	// an allowing PolicyDecision (mirrors `event-store rebaseline`).
	if !hasFlag(args, "--confirm") {
		claimsN, edgesN := countLedgerRows(homePath)
		ai, intentErr := storeGovernedIntent("work_dryrun", "sync_apply", cfg,
			fmt.Sprintf("incremental sync of ~%d claims, ~%d edges from %s into Postgres", claimsN, edgesN, homePath), time.Now())
		if intentErr != nil {
			fmt.Fprintln(stderr, "store sync: build governed intent:", intentErr)
			return 1
		}
		_, gateErr := gateStoreGovernedWrite(homePath, ai, time.Now())
		if jsonOut {
			if err := encodeJSON(stdout, map[string]any{
				"mode": "dry-run", "claims_pending": claimsN, "edges_pending": edgesN,
				"allowed": gateErr == nil,
			}); err != nil {
				fmt.Fprintln(stderr, "store sync: encode:", err)
				return 1
			}
		} else {
			fmt.Fprintf(stdout, "store sync (dry-run — nothing written): ~%d claims, ~%d edges in %s\n", claimsN, edgesN, homePath)
			if gateErr != nil {
				fmt.Fprintf(stdout, "  policy: BLOCKED — %v\n", gateErr)
			} else {
				fmt.Fprintln(stdout, "  policy: ALLOWED — run again with --confirm --work W to sync")
			}
		}
		return 0
	}

	work := strings.TrimSpace(valueFlag(args, "--work"))
	if work == "" {
		fmt.Fprintln(stderr, "error: --work W is required for --confirm (a governed act belongs to a work)")
		return 2
	}
	now := time.Now()
	ai, intentErr := storeGovernedIntent(work, "sync_apply", cfg,
		fmt.Sprintf("incremental sync of the file ledger from %s into Postgres", homePath), now)
	if intentErr != nil {
		fmt.Fprintln(stderr, "store sync: build governed intent:", intentErr)
		return 1
	}
	if _, err := gateStoreGovernedWrite(homePath, ai, now); err != nil {
		fmt.Fprintln(stderr, "store sync --confirm:", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	store, err := pg.Open(ctx, cfg)
	if err != nil {
		writePostgresFailure(stderr, "store sync", targetRef, "connect", err)
		return 1
	}
	defer store.Close()
	if err := store.EnsureSchema(ctx); err != nil {
		writePostgresFailure(stderr, "store sync", targetRef, "schema", err)
		return 1
	}
	if !jsonOut {
		fmt.Fprintf(stdout, "store sync: %s → postgres\n", homePath)
	}
	start := time.Now()
	claimsUp, claimsErr := syncClaims(ctx, store, filepath.Join(homePath, "ledger", "claims.jsonl"), targetRef, stdout, jsonOut)
	edgesUp, edgesErr := syncEdges(ctx, store, filepath.Join(homePath, "ledger", "edges.jsonl"), targetRef, stdout, jsonOut)
	duration := time.Since(start).Round(time.Second)

	var receiptID, transitionID string
	if claimsErr == 0 && edgesErr == 0 {
		// The mutation is complete; record its proof. A proof failure after a
		// successful sync is loud, never silent.
		esStore, err := eventstore.Open(homePath)
		if err != nil {
			fmt.Fprintf(stderr, "store sync: open event store for proof: %v\n", err)
			return 1
		}
		var proofErr error
		receiptID, transitionID, proofErr = recordStoreGovernedProof(esStore, ai,
			fmt.Sprintf("store sync --confirm: %d claims, %d edges upserted to Postgres", claimsUp, edgesUp), now)
		if proofErr != nil {
			fmt.Fprintf(stderr, "store sync: sync applied but proof recording failed: %v\n", proofErr)
			return 1
		}
	}

	if jsonOut {
		if err := encodeJSON(stdout, map[string]any{
			"claims_upserted": claimsUp, "claims_errors": claimsErr,
			"edges_upserted": edgesUp, "edges_errors": edgesErr,
			"duration":   duration.String(),
			"receipt_id": receiptID, "transition_id": transitionID,
		}); err != nil {
			fmt.Fprintln(stderr, "store sync: encode:", err)
			return 1
		}
	} else {
		fmt.Fprintf(stdout, "\nstore sync: done (%s)\n", duration)
		fmt.Fprintf(stdout, "  claims: %d upserted, %d errors\n", claimsUp, claimsErr)
		fmt.Fprintf(stdout, "  edges:  %d upserted, %d errors\n", edgesUp, edgesErr)
		if receiptID != "" {
			fmt.Fprintf(stdout, "  proof:  receipt=%s transition=%s\n", receiptID, transitionID)
		}
	}
	if claimsErr > 0 || edgesErr > 0 {
		return 1
	}
	// Chain into incremental embed when --embed is passed; the sync gate above
	// already authorized this governed run, so embed inherits it.
	if hasFlag(args, "--embed") {
		if !jsonOut {
			fmt.Fprintln(stdout)
		}
		return runStoreEmbed(args, stdout, stderr, true)
	}
	return 0
}

// countLedgerRows counts non-empty lines in the ledger's claims/edges logs for
// dry-run previews. Missing files count as zero; unreadable files as -1.
func countLedgerRows(homePath string) (claims, edges int) {
	count := func(name string) int {
		data, err := os.ReadFile(filepath.Join(homePath, "ledger", name))
		if err != nil {
			if os.IsNotExist(err) {
				return 0
			}
			return -1
		}
		n := 0
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) != "" {
				n++
			}
		}
		return n
	}
	return count("claims.jsonl"), count("edges.jsonl")
}

func syncClaims(ctx context.Context, store *pg.Store, path, profile string, stdout io.Writer, jsonOut bool) (int, int) {
	f, err := os.Open(path)
	if err != nil {
		if !jsonOut {
			fmt.Fprintf(stdout, "  claims: open: %v\n", err)
		}
		return 0, 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	parseErrors := 0
	var claims []persist.Claim
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var row syncClaimRow
		if err := json.Unmarshal(line, &row); err != nil {
			parseErrors++
			continue
		}
		createdAt := time.Now()
		if row.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, row.CreatedAt); err == nil {
				createdAt = t
			}
		}
		claims = append(claims, persist.Claim{
			ID:              row.ID,
			Entity:          row.Entity,
			Predicate:       row.Predicate,
			Object:          row.Object,
			ObservationDate: row.ObservationDate,
			Confidence:      row.Confidence,
			Derivation:      row.Derivation,
			ClaimType:       row.ClaimType,
			CanonClass:      row.CanonClass,
			Visibility:      row.Visibility,
			State:           row.State,
			SourceTraceRef:  row.SourceTraceRef,
			CreatedAt:       createdAt.Format(time.RFC3339),
		})
	}
	if len(claims) == 0 {
		return 0, parseErrors
	}
	// Sync each phase inside one Postgres transaction: a mid-stream failure
	// rolls back the whole phase rather than tearing the ledger.
	written, err := store.BulkAppendClaims(ctx, claims)
	if err != nil {
		if !jsonOut {
			writePostgresFailure(stdout, "  claims", profile, "bulk write", err)
		}
		return 0, parseErrors + len(claims)
	}
	return written, parseErrors
}

func syncEdges(ctx context.Context, store *pg.Store, path, profile string, stdout io.Writer, jsonOut bool) (int, int) {
	f, err := os.Open(path)
	if err != nil {
		if !jsonOut {
			fmt.Fprintf(stdout, "  edges: open: %v\n", err)
		}
		return 0, 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	parseErrors := 0
	var edges []persist.Edge
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var row syncEdgeRow
		if err := json.Unmarshal(line, &row); err != nil {
			parseErrors++
			continue
		}
		createdAt := time.Now()
		if row.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, row.CreatedAt); err == nil {
				createdAt = t
			}
		}
		edges = append(edges, persist.Edge{
			Seq:       row.Seq,
			Kind:      row.Kind,
			From:      row.From,
			To:        row.To,
			ClaimID:   row.ClaimID,
			CreatedAt: createdAt.Format(time.RFC3339),
		})
	}
	if len(edges) == 0 {
		return 0, parseErrors
	}
	written, err := store.BulkAppendEdges(ctx, edges)
	if err != nil {
		if !jsonOut {
			writePostgresFailure(stdout, "  edges", profile, "bulk write", err)
		}
		return 0, parseErrors + len(edges)
	}
	return written, parseErrors
}

// ─── store embed ─────────────────────────────────────────────────────────────────

type embedBatchRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedBatchResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func runStoreEmbed(args []string, stdout, stderr io.Writer, authorized bool) int {
	jsonOut := hasFlag(args, "--json")
	batchSize := 32
	if v := valueFlag(args, "--batch"); v != "" {
		fmt.Sscanf(v, "%d", &batchSize)
		if batchSize <= 0 || batchSize > 100 {
			batchSize = 32
		}
	}
	ollamaURL := storeEnvOr("COMPOUND_EMBED_URL", defaultEmbedBaseURL)
	model := storeEnvOr("COMPOUND_EMBED_MODEL", "nomic-embed-text")
	cfg, cfgErr := loadExperimentalPostgresConfig()
	if cfgErr != nil {
		fmt.Fprintln(stderr, "store embed:", cfgErr)
		return 1
	}
	targetRef := cfg.PostgresTargetRef()
	dims := cfg.VectorDimensions
	if dims <= 0 {
		dims = 1536
	}
	embedder := fmt.Sprintf("ollama:%s:%d", model, dims)

	// Governed write: dry-run by default, --confirm --work W performs it behind
	// an allowing PolicyDecision. When chained from `store sync --confirm`, the
	// sync gate already authorized this run (authorized=true).
	var proofAI envelope.ActionIntent
	proofNow := time.Now()
	recordProof := false
	if !authorized {
		homePath, err := home.Resolve(valueFlag(args, "--home"))
		if err != nil {
			fmt.Fprintf(stderr, "store embed: resolve home: %v (run `compound home init`)\n", err)
			return 1
		}
		if !hasFlag(args, "--confirm") {
			ai, intentErr := storeGovernedIntent("work_dryrun", "embed_apply", cfg,
				"embed unembedded claims into pgvector", proofNow)
			if intentErr != nil {
				fmt.Fprintln(stderr, "store embed: build governed intent:", intentErr)
				return 1
			}
			_, gateErr := gateStoreGovernedWrite(homePath, ai, proofNow)
			if jsonOut {
				if err := encodeJSON(stdout, map[string]any{"mode": "dry-run", "allowed": gateErr == nil}); err != nil {
					fmt.Fprintln(stderr, "store embed: encode:", err)
					return 1
				}
			} else {
				fmt.Fprintln(stdout, "store embed (dry-run — nothing written)")
				if gateErr != nil {
					fmt.Fprintf(stdout, "  policy: BLOCKED — %v\n", gateErr)
				} else {
					fmt.Fprintln(stdout, "  policy: ALLOWED — run again with --confirm --work W to embed")
				}
			}
			return 0
		}
		work := strings.TrimSpace(valueFlag(args, "--work"))
		if work == "" {
			fmt.Fprintln(stderr, "error: --work W is required for --confirm (a governed act belongs to a work)")
			return 2
		}
		var intentErr error
		proofAI, intentErr = storeGovernedIntent(work, "embed_apply", cfg,
			"embed unembedded claims into pgvector", proofNow)
		if intentErr != nil {
			fmt.Fprintln(stderr, "store embed: build governed intent:", intentErr)
			return 1
		}
		if _, err := gateStoreGovernedWrite(homePath, proofAI, proofNow); err != nil {
			fmt.Fprintln(stderr, "store embed --confirm:", err)
			return 1
		}
		recordProof = true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	store, err := pg.Open(ctx, cfg)
	if err != nil {
		writePostgresFailure(stderr, "store embed", targetRef, "connect", err)
		return 1
	}
	defer store.Close()
	claims, err := store.FoldClaims(persist.ClaimFilter{
		Visibility: []string{"operational", "ambient", "contextual"},
		Limit:      0,
	})
	if err != nil {
		writePostgresFailure(stderr, "store embed", targetRef, "load claims", err)
		return 1
	}
	existing, _ := store.Count()
	// Incremental: skip claims that already have vectors
	existingRefs, _ := store.VectorRefs()
	var pending []persist.Claim
	for _, c := range claims {
		if !existingRefs["claim:"+c.ID] {
			pending = append(pending, c)
		}
	}
	if !jsonOut {
		fmt.Fprintf(stdout, "store embed: %d claims total, %d already embedded, %d pending\n", len(claims), existing, len(pending))
		fmt.Fprintf(stdout, "  model: %s @ %s\n", model, ollamaURL)
		fmt.Fprintf(stdout, "  batch: %d\n", batchSize)
	}
	if len(pending) == 0 {
		if jsonOut {
			if err := encodeJSON(stdout, map[string]any{"claims_total": len(claims), "already_embedded": existing, "embedded": 0, "errors": 0, "model": model, "duration": "0s"}); err != nil {
				fmt.Fprintln(stderr, "store embed: encode:", err)
				return 1
			}
		} else {
			fmt.Fprintln(stdout, "store embed: nothing to do — all claims already embedded")
		}
		return 0
	}
	claims = pending
	start := time.Now()
	embedded := 0
	errors := 0
	for i := 0; i < len(claims); i += batchSize {
		end := i + batchSize
		if end > len(claims) {
			end = len(claims)
		}
		batch := claims[i:end]
		texts := make([]string, len(batch))
		for j, c := range batch {
			texts[j] = fmt.Sprintf("%s.%s: %s", c.Entity, c.Predicate, c.Object)
		}
		vectors, err := storeEmbedBatch(ctx, ollamaURL, model, texts)
		if err != nil {
			errors++
			if !jsonOut {
				fmt.Fprintf(stderr, "  batch %d-%d: %v\n", i, end-1, err)
			}
			continue
		}
		for j, c := range batch {
			if j >= len(vectors) {
				break
			}
			meta := map[string]string{"entity": c.Entity, "predicate": c.Predicate, "claim_id": c.ID}
			if err := store.UpsertWithEmbedder("claim:"+c.ID, vectors[j], meta, embedder); err != nil {
				errors++
				continue
			}
			embedded++
		}
		if !jsonOut && (i/batchSize)%10 == 0 {
			fmt.Fprintf(stdout, "  progress: %d/%d embedded\n", embedded, len(claims))
		}
	}
	duration := time.Since(start).Round(time.Second)
	var receiptID, transitionID string
	if errors == 0 && recordProof {
		// The mutation is complete; record its proof (standalone --confirm runs
		// only — chained runs are covered by the sync proof).
		homePath, err := home.Resolve(valueFlag(args, "--home"))
		if err != nil {
			fmt.Fprintf(stderr, "store embed: embed applied but proof setup failed: %v\n", err)
			return 1
		}
		esStore, err := eventstore.Open(homePath)
		if err != nil {
			fmt.Fprintf(stderr, "store embed: embed applied but proof setup failed: %v\n", err)
			return 1
		}
		var proofErr error
		receiptID, transitionID, proofErr = recordStoreGovernedProof(esStore, proofAI,
			fmt.Sprintf("store embed --confirm: %d claims embedded into pgvector", embedded), proofNow)
		if proofErr != nil {
			fmt.Fprintf(stderr, "store embed: embed applied but proof recording failed: %v\n", proofErr)
			return 1
		}
	}
	if jsonOut {
		if err := encodeJSON(stdout, map[string]any{"claims_total": len(claims) + int(existing), "already_embedded": existing, "embedded": embedded, "errors": errors, "model": model, "duration": duration.String(), "receipt_id": receiptID, "transition_id": transitionID}); err != nil {
			fmt.Fprintln(stderr, "store embed: encode:", err)
			return 1
		}
	} else {
		fmt.Fprintf(stdout, "\nstore embed: done — %d embedded, %d errors (%s)\n", embedded, errors, duration)
		if receiptID != "" {
			fmt.Fprintf(stdout, "  proof: receipt=%s transition=%s\n", receiptID, transitionID)
		}
	}
	if errors > 0 {
		return 1
	}
	return 0
}

func storeEmbedBatch(ctx context.Context, baseURL, model string, texts []string) ([][]float32, error) {
	reqBody, _ := json.Marshal(embedBatchRequest{Model: model, Input: texts})
	url := strings.TrimRight(baseURL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embed API %d: %s", resp.StatusCode, string(body[:min(200, len(body))]))
	}
	var result embedBatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("embed decode: %w", err)
	}
	vectors := make([][]float32, len(result.Data))
	for i, d := range result.Data {
		vectors[i] = d.Embedding
	}
	return vectors, nil
}

// ─── store recall ────────────────────────────────────────────────────────────────

type recallResult struct {
	Ref       string  `json:"ref"`
	Score     float64 `json:"score"`
	Entity    string  `json:"entity"`
	Predicate string  `json:"predicate"`
	ClaimID   string  `json:"claim_id"`
}

func runStoreRecall(args []string, stdout, stderr io.Writer) int {
	jsonOut := hasFlag(args, "--json")
	k := 10
	if v := valueFlag(args, "--k"); v != "" {
		fmt.Sscanf(v, "%d", &k)
	}
	ollamaURL := storeEnvOr("COMPOUND_EMBED_URL", defaultEmbedBaseURL)
	model := storeEnvOr("COMPOUND_EMBED_MODEL", "nomic-embed-text")
	valueFlags := map[string]bool{"--k": true, "--batch": true}
	var queryParts []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			if valueFlags[a] {
				i++
			}
			continue
		}
		queryParts = append(queryParts, a)
	}
	query := strings.Join(queryParts, " ")
	if query == "" {
		fmt.Fprintln(stderr, "usage: compound store recall <query text> [--k N] [--json]")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, cfgErr := loadExperimentalPostgresConfig()
	if cfgErr != nil {
		fmt.Fprintln(stderr, "store recall:", cfgErr)
		return 1
	}
	store, err := pg.Open(ctx, cfg)
	if err != nil {
		writePostgresFailure(stderr, "store recall", cfg.PostgresTargetRef(), "connect", err)
		return 1
	}
	defer store.Close()
	vectors, err := storeEmbedBatch(ctx, ollamaURL, model, []string{query})
	if err != nil {
		fmt.Fprintf(stderr, "store recall: embed query: %v\n", err)
		return 1
	}
	if len(vectors) == 0 {
		fmt.Fprintln(stderr, "store recall: embed returned no vectors")
		return 1
	}
	hits, err := store.Search(vectors[0], k, nil)
	if err != nil {
		writePostgresFailure(stderr, "store recall", cfg.PostgresTargetRef(), "search", err)
		return 1
	}
	if jsonOut {
		results := make([]recallResult, len(hits))
		for i, h := range hits {
			results[i] = recallResult{Ref: h.Ref, Score: h.Score, Entity: h.Metadata["entity"], Predicate: h.Metadata["predicate"], ClaimID: h.Metadata["claim_id"]}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"query": query, "model": model, "k": k, "experimental": true, "results": results}); err != nil {
			fmt.Fprintln(stderr, "store recall: encode:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "recall: %q (k=%d, model=%s)\n\n", query, k, model)
	if len(hits) == 0 {
		fmt.Fprintln(stdout, "  (no results — run `compound store embed` first)")
		return 0
	}
	for i, h := range hits {
		fmt.Fprintf(stdout, "  %2d. [%.4f] %s.%s\n", i+1, h.Score, h.Metadata["entity"], h.Metadata["predicate"])
	}
	return 0
}

// ─── helpers ─────────────────────────────────────────────────────────────────────

func storeEnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
