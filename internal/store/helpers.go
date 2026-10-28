package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/persist"

	"github.com/jackc/pgx/v5/pgconn"
)

// Small CLI helpers the organ needs, mirrored from cmd/compound (actIDFor from
// act_cli.go, encodeJSON from bench.go, the flag readers from main.go). The
// cmd-budget ratchet keeps cmd/compound a thin shell, so the organ carries its
// own copies rather than importing the shell. Keep them byte-compatible with
// the canonical versions.

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func hasUsageFlag(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" || a == "help" {
			return true
		}
	}
	return false
}

func valueFlag(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, name+"=") {
			return strings.TrimPrefix(a, name+"=")
		}
	}
	return ""
}

func encodeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// loadExperimentalPostgresConfig is the single admission door for the store
// organ. It refuses unknown, partial, or merely-selected Postgres
// configurations before any connection or mutation. The raw DSN stays inside
// the returned in-memory config; callers render and persist only
// cfg.PostgresTargetRef().
func loadExperimentalPostgresConfig() (persist.Config, error) {
	cfg, err := persist.ConfigFromEnv()
	if err != nil {
		return cfg, err
	}
	if cfg.Backend != persist.BackendPostgres {
		return cfg, &persist.ConfigError{
			Field:    "COMPOUND_STORE",
			Reason:   "Postgres backend is not selected",
			Recovery: "the file backend is the sole supported public-alpha backend; select postgres only for explicit experimental use",
		}
	}
	if err := cfg.ValidatePostgresExperiment(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// safePostgresDetail is the only public rendering shape for errors returned
// after the experimental adapter has parsed a DSN. pgx connection errors can
// contain user, database, host, and port data; those details are connection
// topology and may not enter reports, receipts, or transitions. SQLSTATE is a
// bounded protocol code and remains useful without exposing that topology.
func safePostgresDetail(profile, operation string, err error) string {
	detail := operation + " for " + profile + " failed"
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code != "" {
		detail += " (SQLSTATE " + pgErr.Code + ")"
	}
	return detail
}

func writePostgresFailure(w io.Writer, prefix, profile, operation string, err error) {
	fmt.Fprintf(w, "%s: %s\n", prefix, safePostgresDetail(profile, operation, err))
}

// actIDFor mints deterministic content-addressed ids, mirroring
// cmd/compound/act_cli.go's actIDFor.
func actIDFor(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(sum[:])[:12]
}

// eventstoreRecorder is the narrow surface the store organ needs to persist
// proof envelopes. It matches *eventstore.Store and can be implemented by
// test fakes to exercise failure paths.
type eventstoreRecorder interface {
	PutJSON(id string, value any, indent string) (eventstore.PutResult, error)
	Read(raw string) (eventstore.Record, error)
	Remove(raw string) error
}

// recordEventstoreEnvelope persists a receipt/transition through the canonical
// event store and proves it resolvable via a post-write Read, mirroring the
// helper in cmd/compound/eventstore_rebaseline.go (which mirrors package act's
// own unexported recorder).
func recordEventstoreEnvelope(store eventstoreRecorder, value envelope.Envelope) error {
	if err := value.Validate(); err != nil {
		return err
	}
	id := envelopeID(value)
	if id == "" {
		return fmt.Errorf("eventstore envelope id required for %T", value)
	}
	if _, err := store.PutJSON(id, value, " "); err != nil {
		return err
	}
	record, err := store.Read(id)
	if err != nil {
		return fmt.Errorf("prove event %s persistence through Store.Read: %w", id, err)
	}
	if record.ID != id {
		return fmt.Errorf("prove event %s persistence through Store.Read: resolved id %q", id, record.ID)
	}
	return nil
}

func envelopeID(value envelope.Envelope) string {
	switch typed := value.(type) {
	case *envelope.Receipt:
		return typed.ID
	case *envelope.Transition:
		return typed.ID
	default:
		return ""
	}
}
