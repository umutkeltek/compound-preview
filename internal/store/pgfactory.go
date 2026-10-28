package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/contract"
	"compound/internal/persist"
	"compound/internal/persist/pg"
)

// InstallPgvectorEngineFactory installs the postgres+pgvector recall-engine
// factory the contract spine consumes (ADR-0005: the spine stays
// dependency-free; backend construction — pg.Open, the embed endpoint,
// credentials — lives at the CLI composition root, and this organ owns the
// mechanics). Every connection parameter comes from the environment; there are
// no baked-in hosts. A missing COMPOUND_DSN or COMPOUND_EMBED_URL under
// COMPOUND_STORE=postgres is a factory error the spine records as Degraded
// evidence, never a silent fall-through to another engine. Selection alone is
// insufficient: ConfigFromEnv also requires the separate experimental opt-in
// and a nonsecret profile label before this factory can parse or dial the DSN.
func InstallPgvectorEngineFactory() {
	contract.SetPgvectorEngineFactory(func() (contract.CloseableEngine, error) {
		cfg, err := persist.ConfigFromEnv()
		if err != nil {
			return nil, err
		}
		if err := cfg.ValidatePostgresExperiment(); err != nil {
			return nil, err
		}
		embedURL := strings.TrimSpace(os.Getenv("COMPOUND_EMBED_URL"))
		if embedURL == "" {
			return nil, fmt.Errorf("COMPOUND_EMBED_URL is required when COMPOUND_STORE=postgres (no baked-in default)")
		}
		embedModel := strings.TrimSpace(os.Getenv("COMPOUND_EMBED_MODEL"))
		if embedModel == "" {
			embedModel = "nomic-embed-text"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		store, err := pg.Open(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return pg.NewPgEngine(store, embedURL, embedModel, cfg.VectorDimensions), nil
	})
}
