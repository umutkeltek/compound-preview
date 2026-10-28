// Package activity is compound's Tier-0 ambient-activity observed family (slice 1) plus
// its Tier-1 envelope supplement (slice 2). Tier-0 collects app focus/idle/lock via a
// zero-TCC Swift launchd agent. Tier-1 resolves the focused pane's cwd/git/agent_kind
// via a finite CLI command — no daemon, content-free by schema, by strict-decode, and
// by per-source ContentFreeInvariant self-test at startup.
//
// The schema family is compound.activity_observation.v0.2 (slice-1 events + Tier-1
// envelope meta). The strict decoder refuses any title/url/document/text/content key
// at any depth. Closed vocab on kinds; envelope meta adds bounded, non-content keys
// (pane_cwd, git_remote, agent_kind, ...) listed in Tier1AllowedMetaKeys.
//
// It is content-free BY SCHEMA (no title/url/document field exists), day/size-rotated
// (never per-event files — the eventstore 53k-file lesson), and mints ZERO automatic
// memory candidates. Deliberate non-imports (enforced by imports_test.go, extended in
// Phase 5 to walk subpackages): internal/mem, internal/candidates, internal/eventstore.
// Governance (enable/disable/drain/tier1-collect grant-gate) rides the generic
// internal/act + internal/policy seams; this package writes segment/span files directly.
package activity

// SchemaVersion is the closed event vocabulary version this binary WRITES. The
// strict decoder's READ side is governed by acceptedSchemaVersions below (the
// current version plus an explicit prior-version window), NOT by this constant
// alone — so bumping the writer here does not silently make pre-existing spool
// unreadable. The versioning policy is ADR 0004; the per-family change log is
// internal/activity/SCHEMA.md.
//
// v0.2 introduced: KindWindowFocused and KindPermissionLost are now lit-up reducer
// semantics (Phase 5). Tier-1 envelopes ride on the same event schema with extra
// Meta keys (see Tier1AllowedMetaKeys).
const SchemaVersion = "compound.activity_observation.v0.2"

// legacySchemaVersionV01 is the prior event vocabulary. v0.1 predates the lit-up
// Tier-1 kinds and meta keys but is otherwise shape-compatible (same Event struct;
// its kinds are a subset of knownKinds; no Tier-1 meta), so it stays in the
// read-compat window: a binary upgraded to v0.2 must still drain spool lines the
// previous release wrote. Spool is day/size-rotated, so this window is short by
// design — keeping one prior version is enough.
const legacySchemaVersionV01 = "compound.activity_observation.v0.1"

// acceptedSchemaVersions is the closed READ-compat set for DecodeStrict: the
// current writer version plus every prior version still considered readable.
// Adding the new SchemaVersion here is the read-compat half of any bump; the
// drift-guard test (TestAcceptedSchemaVersionsIncludesCurrent) fails if the
// current version is ever absent. Deliberately removing a version is an explicit
// "stop reading old spool" decision that must also update SCHEMA.md.
var acceptedSchemaVersions = map[string]bool{
	SchemaVersion:          true,
	legacySchemaVersionV01: true,
}

// acceptedSchemaVersionList mirrors acceptedSchemaVersions as a deterministic
// (ascending) slice, used only to render stable decoder error messages. Kept in
// sync by TestAcceptedSchemaVersionsIncludesCurrent.
var acceptedSchemaVersionList = []string{
	legacySchemaVersionV01,
	SchemaVersion,
}

// AcceptsActivitySchemaVersion reports whether the strict decoder will read a line
// carrying the given schema_version. Exported so subpackages and tests consult the
// one read-compat contract instead of re-pinning a version string.
func AcceptsActivitySchemaVersion(v string) bool { return acceptedSchemaVersions[v] }

// SpansSchemaVersion versions the deterministic span reduction file.
const SpansSchemaVersion = "compound.activity_spans.v0.1"

// Event kinds — the closed vocabulary. app_activated/idle_*/screen_* + capture_gap are emitted by the
// slice-1 collector; window_focused/permission_lost are schema-valid but reserved for a Tier-1 slice.
const (
	KindAppActivated   = "app_activated"
	KindWindowFocused  = "window_focused"
	KindIdleStarted    = "idle_started"
	KindIdleEnded      = "idle_ended"
	KindScreenLocked   = "screen_locked"
	KindScreenUnlocked = "screen_unlocked"
	KindCaptureGap     = "capture_gap"
	KindPermissionLost = "permission_lost"
)

// knownKinds is the closed set the strict decoder accepts. An event with any other kind is refused.
var knownKinds = map[string]bool{
	KindAppActivated:   true,
	KindWindowFocused:  true,
	KindIdleStarted:    true,
	KindIdleEnded:      true,
	KindScreenLocked:   true,
	KindScreenUnlocked: true,
	KindCaptureGap:     true,
	KindPermissionLost: true,
}

// ForbiddenMetaKeys are content-bearing keys that must NEVER appear in a Tier-0 or
// Tier-1 event's meta map. Exported so subpackages (notably internal/activity/tier1/source)
// can apply the same invariant without duplicating the set — keeping one source of truth.
//
// The original F5 set (title/url/document/text/content) covered the schema-level
// content leakage. Phase 5 added the Tier-1 extensions: file (VS Code's per-file
// path), command_args / script / initial_command / env (terminal command-line
// content), and language (VS Code per-file language). These are all content-bearing
// — they would reveal file paths, command lines, or env-var secrets.
//
// Adding a key here is a schema-level decision: existing drains that have already
// validated with the old set will refuse the new key. v0.1 → v0.2 is the bump that
// brought the Tier-1 extensions in; older v0.1 spool files are still readable
// because v0.1 is in the read-compat window (acceptedSchemaVersions), not because
// the decoder ignores the version — it refuses any version outside that set.
var ForbiddenMetaKeys = map[string]bool{
	// F5 originals.
	"title":    true,
	"url":      true,
	"document": true,
	"text":     true,
	"content":  true,
	// Tier-1 extensions (Phase 5).
	"file":            true,
	"command_args":    true,
	"script":          true,
	"initial_command": true,
	"env":             true,
	"language":        true,
}

// forbiddenMetaKeys preserves the package-internal alias so callers in this package don't change.
var forbiddenMetaKeys = ForbiddenMetaKeys

// Category is the coarse, content-free classification of an app. Deterministic longest-prefix map,
// never ML. An unmapped bundle id is CategoryUnknown at confidence 0.
type Category string

const (
	CategoryCoding   Category = "coding"
	CategoryBrowsing Category = "browsing"
	CategoryDesign   Category = "design"
	CategoryComms    Category = "comms"
	CategoryStoreOps Category = "store-ops"
	CategoryDocs     Category = "docs"
	CategoryMedia    Category = "media"
	CategorySystem   Category = "system"
	CategoryUnknown  Category = "unknown"
)

// Coverage marks whether a span's day-window is trustworthy for block inference.
const (
	CoverageComplete   = "complete"
	CoverageIncomplete = "incomplete"
)

// Engine identifies the collector that produced an event.
type Engine struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}

// Event is one raw activity observation. Content-free by construction: there is no title, url, or
// document field. Meta is a bounded optional map for coarse annotations (never content).
type Event struct {
	SchemaVersion string            `json:"schema_version"`
	Seq           int64             `json:"seq"`
	TS            string            `json:"ts"`
	Kind          string            `json:"kind"`
	BundleID      string            `json:"bundle_id"`
	AppName       string            `json:"app_name"`
	Engine        Engine            `json:"engine"`
	Meta          map[string]string `json:"meta,omitempty"`
}

// ActivitySpan is the durable, consumer-facing unit: a deterministic reduction of coalesced raw
// events into a dwell interval. Dozens per day, not thousands. Segment-not-event granularity is the
// growth control.
//
// Meta carries the most recent Tier-1 envelope meta keys captured by the reducer. Tier-0 events
// (no Tier-1 envelope) leave this empty; Tier-1 events stamp pane_cwd / git_remote / pane_type /
// agent_kind etc. here. Downstream consumers — e.g. compound attention — read Meta to surface
// per-project time without re-reading the spool.
type ActivitySpan struct {
	Start     string            `json:"start"`
	End       string            `json:"end"`
	ActiveSec int64             `json:"active_sec"`
	BundleID  string            `json:"bundle_id"`
	AppName   string            `json:"app_name"`
	Category  Category          `json:"category"`
	Coverage  string            `json:"coverage"`
	Meta      map[string]string `json:"meta,omitempty"`
}

// SpanFile is the atomic-rewrite per-day span reduction persisted under observed/activity/spans/.
type SpanFile struct {
	SchemaVersion string         `json:"schema_version"`
	Date          string         `json:"date"`
	GeneratedAt   string         `json:"generated_at"`
	Spans         []ActivitySpan `json:"spans"`
}
