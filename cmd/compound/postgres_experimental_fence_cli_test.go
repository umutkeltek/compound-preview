package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
)

func TestOrientCLIValidPostgresGateDegradesWithoutBreadthFallbackOrDSNDisclosure(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and executes the compound binary")
	}
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "global", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	dsnHost := strings.Join([]string{"db", "example", "invalid"}, ".")
	dsnUser := "synthetic" + "_user_marker"
	dsnDatabase := "compound" + "_cli_probe"
	dsn := strings.Join([]string{
		"host", "=", dsnHost,
		" port", "=1",
		" user", "=", dsnUser,
		" dbname", "=", dsnDatabase,
		" connect_timeout", "=1",
		" sslmode", "=disable",
	}, "")

	t.Setenv("COMPOUND_HOME", homePath)
	t.Setenv("COMPOUND_STORE", "postgres")
	t.Setenv("COMPOUND_EXPERIMENTAL_POSTGRES", "1")
	t.Setenv("COMPOUND_POSTGRES_PROFILE", "offline-cli-probe")
	t.Setenv("COMPOUND_DSN", dsn)
	t.Setenv("COMPOUND_VECTOR_DIMS", "1536")
	t.Setenv("COMPOUND_EMBED_URL", "https://embed.example.invalid")
	t.Setenv("COMPOUND_EMBED_MODEL", "synthetic-embedder")
	t.Setenv("COMPOUND_MEMORY_CLI_PROFILE", "")
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "")

	cmd := exec.Command(
		compoundTestBinary(t),
		"orient",
		"--harness", "codex",
		"--cwd", workspace,
		"--task", "prove selected postgres retrieval degradation",
	)
	cmd.Dir = workspace
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("orient with unavailable selected Postgres: %v\n%s", err, out)
	}
	var pack envelope.ContextPack
	if err := json.Unmarshal(out, &pack); err != nil {
		t.Fatalf("decode ContextPack: %v\n%s", err, out)
	}
	if err := pack.Validate(); err != nil {
		t.Fatalf("degraded ContextPack is invalid: %v\n%s", err, out)
	}
	if pack.Retrieval == nil {
		t.Fatalf("ContextPack omitted retrieval posture: %s", out)
	}
	if got := pack.Retrieval; got.Level != envelope.RetrievalLevelDegraded ||
		got.RequestedEngine != "pgvector" || got.ObservedEngine != "" ||
		got.Fallback != envelope.RetrievalFallbackLedgerRanker ||
		got.ErrorClass != envelope.RetrievalErrorUnavailable {
		t.Fatalf("retrieval posture = %+v, want pgvector degraded/unavailable with ledger-only fallback", got)
	}

	for _, forbidden := range []string{dsnHost, dsnUser, dsnDatabase, dsn} {
		if bytes.Contains(out, []byte(forbidden)) {
			t.Fatalf("orient output disclosed DSN material %q: %s", forbidden, out)
		}
	}
	err = filepath.WalkDir(homePath, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, forbidden := range []string{dsnHost, dsnUser, dsnDatabase, dsn} {
			if bytes.Contains(body, []byte(forbidden)) {
				t.Fatalf("home file %s persisted DSN material %q", path, forbidden)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan generated home: %v", err)
	}
}
