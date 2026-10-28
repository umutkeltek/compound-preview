package store

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"compound/internal/persist"
	"compound/internal/persist/pg"
)

// testDSN returns the integration-test database DSN or skips the test.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("COMPOUND_TEST_DSN")
	if dsn == "" {
		t.Skip("COMPOUND_TEST_DSN not set; skipping integration test")
	}
	return dsn
}

// testStore opens a Postgres store for an integration test.
func testStore(t *testing.T) *pg.Store {
	t.Helper()
	dsn := testDSN(t)
	ctx := context.Background()
	cfg := persist.Config{
		Backend:              persist.BackendPostgres,
		DSN:                  dsn,
		ExperimentalPostgres: true,
		PostgresProfile:      "doctor-integration-test",
		VectorDimensions:     8, // small dims for fast tests
	}
	store, err := pg.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		store.Close()
		t.Fatalf("EnsureSchema: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// readSourceFile reads a sibling source file from the same package directory.
func readSourceFile(name string) (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot determine test file path")
	}
	path := filepath.Join(filepath.Dir(file), name)
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// TestDoctorVectorProbeRoutesThroughUpsertWithEmbedder is a static regression test:
// checkVectors in cli.go must call UpsertWithEmbedder, never the unlabeled Upsert,
// so the doctor probe never writes an empty-embedder row into the index.
func TestDoctorVectorProbeRoutesThroughUpsertWithEmbedder(t *testing.T) {
	source, err := readSourceFile("cli.go")
	if err != nil {
		t.Fatalf("read cli.go: %v", err)
	}

	// Locate the checkVectors function body.
	start := strings.Index(source, "func checkVectors(")
	if start == -1 {
		t.Fatal("checkVectors function not found in cli.go")
	}
	end := strings.Index(source[start:], "\nfunc ")
	if end == -1 {
		end = len(source)
	} else {
		end += start
	}
	body := source[start:end]

	if strings.Contains(body, "store.Upsert(") {
		t.Fatalf("checkVectors still calls the unlabeled store.Upsert, which writes an empty embedder")
	}
	if !strings.Contains(body, "store.UpsertWithEmbedder(") {
		t.Fatalf("checkVectors must route vector writes through store.UpsertWithEmbedder")
	}
}

// TestDoctorVectorProbeFailsClosedOnEmbedderMismatch is an integration regression test:
// if the vectors table already carries a different embedder identity, the doctor
// probe must refuse to write (fail-closed) rather than silently polluting the index
// with an unlabeled or mismatched vector. Before the fix checkVectors used
// store.Upsert, which has no embedder guard and would succeed; after the fix it
// routes through UpsertWithEmbedder and fails with an embedder-mismatch error.
func TestDoctorVectorProbeFailsClosedOnEmbedderMismatch(t *testing.T) {
	store := testStore(t)

	// Pin the doctor probe's embedder identity so it is deterministic and different
	// from the pre-existing index identity.
	t.Setenv("COMPOUND_EMBED_MODEL", "doctor-model")
	cfg := persist.Config{VectorDimensions: 8}

	// Pre-populate the vectors table with a different embedder identity.
	emb := []float32{1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}
	preRef := fmt.Sprintf("pre-embedder-%d", time.Now().UnixNano())
	if err := store.UpsertWithEmbedder(preRef, emb, nil, "ollama:other-model:8"); err != nil {
		t.Fatalf("prepopulate vector: %v", err)
	}

	probe := newStoreDoctorProbe()
	report := storeDoctorReport{Backend: "postgres"}
	checkVectors(context.Background(), store, cfg, probe, &report)

	var vectorsCheck *storeDoctorCheck
	for i := range report.Checks {
		if report.Checks[i].Name == "vectors" {
			vectorsCheck = &report.Checks[i]
			break
		}
	}
	if vectorsCheck == nil {
		t.Fatal("checkVectors did not append a vectors check")
	}
	if vectorsCheck.Status == "ok" {
		t.Fatalf("expected vectors check to fail on embedder mismatch, got ok")
	}
	if !strings.Contains(vectorsCheck.Detail, "embedder") && !strings.Contains(vectorsCheck.Detail, "indexed with") {
		t.Fatalf("expected detail to mention embedder mismatch, got: %s", vectorsCheck.Detail)
	}
}

// TestDoctorVectorProbeStampsEmbedderIdentity verifies the happy path: when the
// vectors table is empty (or already carries the same identity), the doctor probe
// writes a vector stamped with its explicit embedder identity.
func TestDoctorVectorProbeStampsEmbedderIdentity(t *testing.T) {
	store := testStore(t)

	// Remove any vectors left by other tests so this test starts with a clean index.
	// checkVectors itself deletes the probe row, but other tests may leave rows from
	// the mismatch happy path; use a raw connection scoped to this package's test DSN.
	ctx := context.Background()
	cleanupConn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("connect cleanup: %v", err)
	}
	if _, err := cleanupConn.Exec(ctx, "DELETE FROM vectors"); err != nil {
		cleanupConn.Close(ctx)
		t.Fatalf("cleanup vectors: %v", err)
	}
	cleanupConn.Close(ctx)

	t.Setenv("COMPOUND_EMBED_MODEL", "doctor-model")
	cfg := persist.Config{VectorDimensions: 8}
	probe := newStoreDoctorProbe()
	report := storeDoctorReport{Backend: "postgres"}
	checkVectors(ctx, store, cfg, probe, &report)

	var vectorsCheck *storeDoctorCheck
	for i := range report.Checks {
		if report.Checks[i].Name == "vectors" {
			vectorsCheck = &report.Checks[i]
			break
		}
	}
	if vectorsCheck == nil {
		t.Fatal("checkVectors did not append a vectors check")
	}
	if vectorsCheck.Status != "ok" {
		t.Fatalf("vectors check failed: %s", vectorsCheck.Detail)
	}

	// Confirm the probe row was deleted, and that the ONLY embedder identity ever
	// stamped during the write was the doctor's explicit identity. Because
	// VectorEmbedders excludes empty embedders, there should be no rows at all
	// after checkVectors deletes the probe vector.
	embedders, err := store.VectorEmbedders(ctx)
	if err != nil {
		t.Fatalf("VectorEmbedders: %v", err)
	}
	if len(embedders) != 0 {
		t.Fatalf("expected probe vector to be cleaned up; found embedders=%v", embedders)
	}
}

// TestStoreDoctorRunsWithoutError is a smoke test that the full doctor command can
// be invoked end-to-end when a Postgres backend is available.
func TestStoreDoctorRunsWithoutError(t *testing.T) {
	t.Setenv("COMPOUND_STORE", "postgres")
	t.Setenv("COMPOUND_DSN", testDSN(t))
	t.Setenv(persist.ExperimentalPostgresEnv, "1")
	t.Setenv(persist.PostgresProfileEnv, "doctor-integration-test")
	t.Setenv("COMPOUND_VECTOR_DIMS", "8")

	var stdout, stderr bytes.Buffer
	exit := Run([]string{"doctor", "--json"}, &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("store doctor exited %d; stderr: %s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"all_ok": true`) {
		t.Fatalf("store doctor did not report all_ok=true; output:\n%s", stdout.String())
	}
}
