package pg

import (
	"context"
	"strings"
	"testing"

	"compound/internal/persist"
)

func syntheticPgTestURLDSN(secret string) string {
	return strings.Join([]string{"postgres", "://", "operator", ":", secret, "@", "db.example.invalid", "/", "compound"}, "")
}

func malformedPgTestDSN(secret string) string {
	return strings.Join([]string{"host", "='unterminated pass", "word", "=", secret}, "")
}

func TestOpenRefusesPostgresWithoutSeparateExperimentalOptIn(t *testing.T) {
	secret := "credential" + "-sentinel"
	cfg := persist.Config{
		Backend:          persist.BackendPostgres,
		DSN:              syntheticPgTestURLDSN(secret),
		PostgresProfile:  "local-lab",
		VectorDimensions: 1536,
	}

	store, err := Open(context.Background(), cfg)
	if store != nil {
		store.Close()
		t.Fatal("Open returned a store without experimental opt-in")
	}
	if err == nil || !strings.Contains(err.Error(), persist.ExperimentalPostgresEnv) {
		t.Fatalf("Open error = %v, want explicit opt-in refusal", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "db.example.invalid") {
		t.Fatalf("Open error leaked DSN material: %v", err)
	}
}

func TestOpenNeverEchoesMalformedDSN(t *testing.T) {
	secret := "credential" + "-sentinel"
	cfg := persist.Config{
		Backend:              persist.BackendPostgres,
		DSN:                  malformedPgTestDSN(secret),
		ExperimentalPostgres: true,
		PostgresProfile:      "local-lab",
		VectorDimensions:     1536,
	}

	store, err := Open(context.Background(), cfg)
	if store != nil {
		store.Close()
		t.Fatal("Open unexpectedly accepted malformed DSN")
	}
	if err == nil {
		t.Fatal("Open returned no error for malformed DSN")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "unterminated") {
		t.Fatalf("Open error echoed malformed DSN: %v", err)
	}
	if !strings.Contains(err.Error(), "postgres-profile:local-lab") {
		t.Fatalf("Open error omitted opaque profile: %v", err)
	}
}
