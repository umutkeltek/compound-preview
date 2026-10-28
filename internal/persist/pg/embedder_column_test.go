package pg

import (
	"context"
	"strings"
	"testing"
)

func TestSchemaDDLCreatesAndMigratesEmbedderColumn(t *testing.T) {
	collapsed := strings.Join(strings.Fields(schemaDDL(768)), " ")
	if !strings.Contains(collapsed, "embedder TEXT NOT NULL DEFAULT ''") {
		t.Errorf("vectors table must declare the embedder custody column")
	}
	if !strings.Contains(collapsed, "ALTER TABLE vectors ADD COLUMN IF NOT EXISTS embedder TEXT NOT NULL DEFAULT ''") {
		t.Errorf("EnsureSchema must idempotently add the embedder column for existing databases")
	}
}

func TestEnsureSchemaEmbedderColumnReadable(t *testing.T) {
	store := testStore(t)
	if _, err := store.VectorEmbedders(context.Background()); err != nil {
		t.Fatalf("VectorEmbedders after EnsureSchema: %v", err)
	}
}
