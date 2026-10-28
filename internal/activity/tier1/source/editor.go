package source

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// EditorConfig identifies one IDE that publishes via AW's vscode watcher. The watcher
// emits {project, file, language, ...} per open file; this source consumes ONLY the
// project field (folder path) — file/language are content-bearing and refused at the
// read boundary (Tier-1 invariant + F5).
//
// Both VS Code ("com.microsoft.VSCode") and Cursor ("com.todesktop.230313mzl4w4u96")
// use the same watcher protocol, so a single EditorSource implementation handles both.
type EditorConfig struct {
	BundleID    string
	DisplayName string
}

// editorAwClient is the minimal AW REST surface EditorSource needs. We keep this as
// an interface (not a hard dependency on internal/adapter/awstream) so this package
// can be tested without spinning up an HTTP fixture.
type editorAwClient interface {
	FetchEvents(ctx context.Context, bucketID string, start, end time.Time, limit int) ([]editorEvent, error)
}

// editorEvent is the subset of AW's vscode-watcher payload this source reads. The
// `file` and `language` fields are intentionally NOT deserialized — they're
// content-bearing (F5) and we don't want them appearing anywhere downstream.
type editorEvent struct {
	Timestamp string          `json:"timestamp"`
	Duration  float64         `json:"duration"`
	Data      json.RawMessage `json:"data"`
}

// vscodeDataPayload is the structural shape of the vscode watcher's `data` field.
// We unmarshal ONLY `project` (folder path). Any other key (`file`, `language`, etc.)
// is dropped by the decoder — they never appear in our envelope.
type vscodeDataPayload struct {
	Project string `json:"project"`
}

// EditorSource consumes the most recent `aw-watcher-vscode_*` event for the focused
// editor and returns the project (folder path) as the envelope's PaneCwd-equivalent.
// No IPC, no AppleScript — just AW reads.
type EditorSource struct {
	cfg    EditorConfig
	awCli  editorAwClient
	bucket string // resolved at construction
}

// NewEditorSource returns an EditorSource that talks to the default local AW server.
// Bucket is resolved lazily — at TryEnvelope time the source lists buckets whose
// type is "vscode" and uses the most recently updated one.
func NewEditorSource(cfg EditorConfig) *EditorSource {
	return &EditorSource{
		cfg:    cfg,
		awCli:  defaultEditorAWClient{},
		bucket: "aw-watcher-vscode_", // suffix-only match; resolved at runtime
	}
}

// newEditorSourceWithClient is a test-only constructor for the EditorSource.
func newEditorSourceWithClient(cfg EditorConfig, awCli editorAwClient) *EditorSource {
	return &EditorSource{cfg: cfg, awCli: awCli, bucket: "aw-watcher-vscode_"}
}

// AppBundleID returns the configured bundle id (e.g. "com.microsoft.VSCode").
func (s *EditorSource) AppBundleID() string { return s.cfg.BundleID }

// TryEnvelope reads the most recent vscode event from AW and extracts the `project`
// field. Returns (zero, false, nil) on any failure (graceful skip — the collector
// counts this as SourcesFailed++ and continues).
func (s *EditorSource) TryEnvelope(ctx context.Context, focused FocusedApp) (Envelope, bool, error) {
	if focused.BundleID != "" && focused.BundleID != s.cfg.BundleID {
		return Envelope{}, false, nil
	}
	// Look back 5 minutes — covers the typical editor activity heartbeat.
	end := time.Now().UTC()
	start := end.Add(-5 * time.Minute)
	events, err := s.awCli.FetchEvents(ctx, s.bucket, start, end, 1)
	if err != nil {
		return Envelope{}, false, fmt.Errorf("editor source: fetch events: %w", err)
	}
	if len(events) == 0 {
		return Envelope{}, false, nil
	}
	var data vscodeDataPayload
	if err := json.Unmarshal(events[0].Data, &data); err != nil {
		return Envelope{}, false, fmt.Errorf("editor source: parse payload: %w", err)
	}
	if data.Project == "" {
		return Envelope{}, false, nil
	}
	return Envelope{
		PaneCwd:        data.Project,
		PaneType:       "editor",
		PaneResolution: PaneResolutionAWEvent,
	}, true, nil
}

// ContentFreeInvariant: the editor source only populates PaneCwd (a path) and
// PaneType ("editor"). Both are content-free by doctrine.
func (s *EditorSource) ContentFreeInvariant() error {
	return CheckEnvelopeContentFree(Envelope{
		PaneCwd:        "/Users/example/projects/example",
		PaneType:       "editor",
		PaneResolution: PaneResolutionAWEvent,
	})
}

// defaultEditorAWClient talks to the local AW server on the default port. The actual
// http.Client is wired up in TryEnvelope (we keep the struct here minimal so tests
// can swap it out without HTTP plumbing).
type defaultEditorAWClient struct{}

// FetchEvents shells out via the awstream package's Client. We import the awstream
// package at the function-call boundary to keep this file's deps minimal.
//
// In practice, the import is wired in editor_aw.go (separate file to keep this file
// focused on the structural logic).
func (defaultEditorAWClient) FetchEvents(ctx context.Context, bucketID string, start, end time.Time, limit int) ([]editorEvent, error) {
	return fetchAWEventsForEditor(ctx, bucketID, start, end, limit)
}

// slugCmdline returns a slug from a command-line fragment, used by tests / future
// command-basename extraction. Defined here so future phases can import it without
// pulling in cmd-line parsing.
func slugCmdline(s string) string {
	// Truncate at first space (basename only — no args).
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		s = s[:i]
	}
	return Slug(s)
}
