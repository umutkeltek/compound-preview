package source

import (
	"context"
	"time"

	"compound/internal/adapter/awstream"
)

// fetchAWEventsForEditor bridges the default editorAWClient (declared in editor.go)
// to the awstream.Client concrete type. Kept in its own file so editor.go's
// dependency surface stays minimal — only the default impl pulls in awstream.
//
// Why an adapter and not a direct field: the source's interface is
// editorAwClient (declared in editor.go) so tests can swap a fixture without HTTP
// plumbing. The production wiring lives here.
func fetchAWEventsForEditor(ctx context.Context, bucketID string, start, end time.Time, limit int) ([]editorEvent, error) {
	cli := awstream.NewClient(awstream.DefaultBaseURL)
	if bucketID == "aw-watcher-vscode_" {
		// Lazy bucket resolution: list buckets, filter on Type ==
		// "app.editor.activity", pick the first match. Multi-hostname dedup is
		// the awstream package's job (Phase 2) — for the editor source we take
		// the first match and accept that AW's per-hostname buckets will give
		// us one event per (bucket, project). The awstream Normalize step
		// upstream collapses duplicates if they ever collide.
		buckets, err := cli.ListBuckets(ctx)
		if err != nil {
			return nil, err
		}
		for _, b := range buckets {
			if b.Type == "app.editor.activity" {
				bucketID = b.ID
				break
			}
		}
		if bucketID == "aw-watcher-vscode_" {
			return nil, nil // no vscode bucket; graceful skip
		}
	}
	raw, err := cli.FetchEvents(ctx, bucketID, start, end, limit)
	if err != nil {
		return nil, err
	}
	out := make([]editorEvent, len(raw))
	for i, r := range raw {
		out[i] = editorEvent{Timestamp: r.Timestamp, Duration: r.Duration, Data: r.Data}
	}
	return out, nil
}
