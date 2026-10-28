package eventsource

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
)

func TestImportOSEventsWritesTraceEnvelopesIdempotently(t *testing.T) {
	home := t.TempDir()
	fixture := filepath.Join("..", "..", "fixtures", "events", "os-events.ndjson")
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

	report, err := ImportOSEvents(home, fixture, ImportOptions{Stream: "control-events"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.Imported != 2 || report.Skipped != 0 || len(report.TraceIDs) != 2 {
		t.Fatalf("report = %+v", report)
	}
	if report.AuthorityRef != TraceEventsAuthorityRef || report.SourceAuthorityID == "" {
		t.Fatalf("missing source authority: %+v", report)
	}
	authority, err := LoadSourceAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(authority) != 1 ||
		authority[0].ID != report.SourceAuthorityID ||
		authority[0].AuthorityStatus != "compound_primary" ||
		authority[0].SourceRole != "evidence_only_input_after_import" ||
		authority[0].Imported != 2 ||
		authority[0].Stream != "control-events" {
		t.Fatalf("bad source authority: %+v", authority)
	}
	for _, id := range report.TraceIDs {
		var tr envelope.TraceEnvelope
		readJSON(t, filepath.Join(home, "events", id+".json"), &tr)
		if err := tr.Validate(); err != nil {
			t.Fatalf("trace %s invalid: %v", id, err)
		}
	}
	if lines := readLines(t, filepath.Join(home, "events", "traces.ndjson")); len(lines) != 2 {
		t.Fatalf("trace stream lines = %d, want 2", len(lines))
	}

	replay, err := ImportOSEvents(home, fixture, ImportOptions{Stream: "control-events"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Imported != 0 || replay.Skipped != 2 {
		t.Fatalf("replay report = %+v", replay)
	}
	authority, err = LoadSourceAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(authority) != 2 || authority[1].ID != report.SourceAuthorityID || authority[1].Skipped != 2 {
		t.Fatalf("bad replay source authority: %+v", authority)
	}
	if lines := readLines(t, filepath.Join(home, "events", "traces.ndjson")); len(lines) != 2 {
		t.Fatalf("replay should not append duplicate traces, lines = %d", len(lines))
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("event import must not promote memory claims, stat err=%v", err)
	}
}

func TestBuildTraceEnvelopeNormalizesOSMutationAndEvidence(t *testing.T) {
	raw := `{"schema_version":1,"id":"evt_x","ts":"2026-06-29T01:00:00Z","kind":"packet.generated","event_class":"planning","metadata_quality":"partial","mutation_class":"local_artifact_write","producer":{"caller":"planner","agent":"worker","run_id":"run_x"},"subject":{"path":"/workspace/out.md","project":"control","service":"packetizer"},"evidence":[{"kind":"path","redacted":false,"value":"/workspace/out.md"},{"kind":"secret","redacted":true,"value":"hidden"}],"summary":"Generated packet."}`
	var ev osEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	tr, err := BuildTraceEnvelope(ev, "events.ndjson", 7, ImportOptions{Stream: "events"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if tr.MutationClass != "local_reversible" || tr.Source.MutationRaw != "local_artifact_write" {
		t.Fatalf("mutation = %q raw=%q", tr.MutationClass, tr.Source.MutationRaw)
	}
	if tr.Risk != "tier1" || tr.Uncertainty.Level != "inferred" {
		t.Fatalf("risk/uncertainty = %s/%s", tr.Risk, tr.Uncertainty.Level)
	}
	if !contains(tr.EvidenceRefs, "secret:<redacted>") {
		t.Fatalf("redacted evidence refs = %v", tr.EvidenceRefs)
	}
	if !contains(tr.FilesTouched, "/workspace/out.md") {
		t.Fatalf("files touched = %v", tr.FilesTouched)
	}
	if !contains(tr.ToolsModelsUsed, "service:packetizer") || !contains(tr.ToolsModelsUsed, "agent:worker") {
		t.Fatalf("tools/models = %v", tr.ToolsModelsUsed)
	}
}

func TestImportOSEventsAcceptsLegacyProviderEventShape(t *testing.T) {
	home := t.TempDir()
	fixture := filepath.Join("..", "..", "fixtures", "events", "os-events-legacy.ndjson")
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

	report, err := ImportOSEvents(home, fixture, ImportOptions{Stream: "os-control-events"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Imported != 4 || report.SourceAuthorityID == "" {
		t.Fatalf("report = %+v", report)
	}
	var tr envelope.TraceEnvelope
	readJSON(t, filepath.Join(home, "events", report.TraceIDs[0]+".json"), &tr)
	if tr.Source.SourceKind != "provider_configured" ||
		tr.Source.EventClass != "legacy" ||
		tr.Source.Quality != "partial" ||
		tr.MutationClass != "read" ||
		!contains(tr.ToolsModelsUsed, "provider:deepseek") ||
		!contains(tr.Outputs, `direct_api_models:["deepseek-v4-flash","deepseek-v4-pro"]`) {
		t.Fatalf("legacy trace = %+v", tr)
	}
}

// TestImportOSEventsDeltaImportOnlyImportsNewTail is the J10 grown-stream
// falsifier: an append-only stream that GREW between imports must import only
// the delta on re-run — never re-import the prefix. Red if dedup double-imports.
func TestImportOSEventsDeltaImportOnlyImportsNewTail(t *testing.T) {
	home := t.TempDir()
	stream := filepath.Join(home, "control-events.ndjson")
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

	ev := func(id, ts string) string {
		return `{"schema_version":1,"id":"` + id + `","ts":"` + ts + `","kind":"lifecycle.tick","event_class":"lifecycle","metadata_quality":"complete","mutation_class":"read_only","producer":{"caller":"cron"},"summary":"tick ` + id + `"}`
	}
	writeStream := func(lines ...string) {
		if err := os.WriteFile(stream, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeStream(ev("evt_a", "2026-06-29T01:00:00Z"), ev("evt_b", "2026-06-29T02:00:00Z"))
	first, err := ImportOSEvents(home, stream, ImportOptions{Stream: "os-control-events"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.Imported != 2 || first.Skipped != 0 {
		t.Fatalf("first import = %+v", first)
	}

	// Stream grows by one appended event; the two originals stay byte-identical.
	writeStream(ev("evt_a", "2026-06-29T01:00:00Z"), ev("evt_b", "2026-06-29T02:00:00Z"), ev("evt_c", "2026-06-29T03:00:00Z"))
	delta, err := ImportOSEvents(home, stream, ImportOptions{Stream: "os-control-events"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if delta.Imported != 1 || delta.Skipped != 2 || len(delta.TraceIDs) != 1 {
		t.Fatalf("delta import must import ONLY the new tail, got %+v", delta)
	}
	if got := len(readLines(t, filepath.Join(home, "events", "traces.ndjson"))); got != 3 {
		t.Fatalf("trace stream must total 3 (2 prefix + 1 delta), got %d", got)
	}

	// A third identical replay is a full no-op — the idempotency floor.
	replay, err := ImportOSEvents(home, stream, ImportOptions{Stream: "os-control-events"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Imported != 0 || replay.Skipped != 3 {
		t.Fatalf("replay must add zero traces, got %+v", replay)
	}
	if got := len(readLines(t, filepath.Join(home, "events", "traces.ndjson"))); got != 3 {
		t.Fatalf("replay must not append duplicate traces, got %d", got)
	}
}

// TestShowTraceServesEnvelopeWithSourceRef proves the forensic reader: a known
// id returns the envelope carrying its source provenance (stream + append-only
// path + line); an unknown id fails with a clear error; a stream reference
// resolves back to the exact source line.
func TestShowTraceServesEnvelopeWithSourceRef(t *testing.T) {
	home := t.TempDir()
	fixture := filepath.Join("..", "..", "fixtures", "events", "os-events.ndjson")
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

	report, err := ImportOSEvents(home, fixture, ImportOptions{Stream: "os-control-events"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Imported != 2 {
		t.Fatalf("setup import = %+v", report)
	}

	id := report.TraceIDs[0]
	detail, err := ShowTrace(home, id)
	if err != nil {
		t.Fatalf("show known id: %v", err)
	}
	if detail.TraceID != id || detail.Status != "ok" || detail.AuthorityRef != TraceEventsAuthorityRef {
		t.Fatalf("detail header = %+v", detail)
	}
	if detail.Source.Stream != "os-control-events" || detail.Source.Line < 1 {
		t.Fatalf("detail must carry source provenance, got %+v", detail.Source)
	}
	if !strings.Contains(detail.SourceRef, "os-control-events@") || !strings.Contains(detail.SourceRef, fixture) {
		t.Fatalf("source_ref must name stream + source path, got %q", detail.SourceRef)
	}
	if detail.TracePath != "compound://events/"+id {
		t.Fatalf("trace served from wrong path: %q", detail.TracePath)
	}
	if err := detail.Trace.Validate(); err != nil {
		t.Fatalf("served trace invalid: %v", err)
	}

	// Lenient CLI forms plus durable logical and historical references all
	// resolve to the same trace.
	for _, input := range []string{
		strings.TrimPrefix(id, "trace_"),
		id + ".json",
		"compound://events/" + id,
		"events/" + id + ".json",
	} {
		t.Run(input, func(t *testing.T) {
			got, err := ShowTrace(home, input)
			if err != nil {
				t.Fatalf("ShowTrace(%q): %v", input, err)
			}
			if got.TraceID != id {
				t.Fatalf("ShowTrace(%q) trace_id = %q, want %q", input, got.TraceID, id)
			}
		})
	}
	if _, err := ShowTrace(home, "trace_deadbeefdead"); err == nil {
		t.Fatalf("unknown trace id must error")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown-id error must be clear, got %v", err)
	}
	// Traversal that names an existing trace must be rejected, not normalized
	// to the basename and served.
	if _, err := ShowTrace(home, "../"+id+".json"); err == nil {
		t.Fatalf("path-component id unexpectedly resolved to an existing trace")
	} else if !strings.Contains(err.Error(), "invalid trace reference") {
		t.Fatalf("traversal error = %v, want invalid trace reference", err)
	}
}

// TestEveryImportedOSEventReachableByShow is the §c falsifier: an OS event id
// present in the source stream but unreachable from compound after import is red.
func TestEveryImportedOSEventReachableByShow(t *testing.T) {
	home := t.TempDir()
	fixture := filepath.Join("..", "..", "fixtures", "events", "os-events.ndjson")
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

	report, err := ImportOSEvents(home, fixture, ImportOptions{Stream: "os-control-events"}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct each source event's trace id and prove ShowTrace reaches it.
	for _, sourceID := range []string{"evt_20260629T010000Z_alpha", "evt_20260629T020000Z_beta"} {
		want := idFor("trace_", sourceID)
		detail, err := ShowTrace(home, want)
		if err != nil {
			t.Fatalf("source event %s unreachable from compound: %v", sourceID, err)
		}
		if detail.Source.SourceID != sourceID {
			t.Fatalf("trace %s resolved wrong source id %q", want, detail.Source.SourceID)
		}
	}
	// Every imported trace id is individually reachable.
	for _, id := range report.TraceIDs {
		if _, err := ShowTrace(home, id); err != nil {
			t.Fatalf("imported trace %s unreachable: %v", id, err)
		}
	}
}

// TestListTracesFiltersByStream proves the list form serves from COMPOUND_HOME
// and scopes to a stream.
func TestListTracesFiltersByStream(t *testing.T) {
	home := t.TempDir()
	fixture := filepath.Join("..", "..", "fixtures", "events", "os-events.ndjson")
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

	if _, err := ImportOSEvents(home, fixture, ImportOptions{Stream: "os-control-events"}, now); err != nil {
		t.Fatal(err)
	}
	all, err := ListTraces(home, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("list all = %d, want 2", len(all))
	}
	matched, err := ListTraces(home, "os-control-events", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 2 {
		t.Fatalf("stream filter = %d, want 2", len(matched))
	}
	none, err := ListTraces(home, "no-such-stream", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("unknown stream must return zero, got %d", len(none))
	}
	limited, err := ListTraces(home, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Fatalf("limit=1 must cap results, got %d", len(limited))
	}
}

// TestImportOSEventsAcceptsAgentRunShape proves the agent-runs stream shape
// imports: identity is task_id, timestamp is started_at, and evidence is a
// string list (or bare string) rather than {kind,value} objects. Before this
// fix the string evidence aborted the parse and the whole stream was
// unreachable from compound (the §c falsifier).
func TestImportOSEventsAcceptsAgentRunShape(t *testing.T) {
	home := t.TempDir()
	fixture := filepath.Join("..", "..", "fixtures", "events", "os-agent-runs.ndjson")
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

	report, err := ImportOSEvents(home, fixture, ImportOptions{Stream: "os-agent-runs"}, now)
	if err != nil {
		t.Fatalf("agent-run import must not abort on string evidence: %v", err)
	}
	// 2 distinct task_ids imported; the duplicate task_id is deduped (skipped).
	if report.Imported != 2 || report.Skipped != 1 {
		t.Fatalf("agent-run import = %+v (want imported=2 skipped=1)", report)
	}

	// Identity is task_id → the trace is reachable by its derived id.
	want := idFor("trace_", "synthetic-run-001")
	detail, err := ShowTrace(home, want)
	if err != nil {
		t.Fatalf("agent-run trace unreachable: %v", err)
	}
	if detail.Source.SourceID != "synthetic-run-001" {
		t.Fatalf("source id = %q, want synthetic-run-001", detail.Source.SourceID)
	}
	if detail.Trace.CreatedAt != "2026-06-04T04:36:18+03:00" {
		t.Fatalf("started_at must become created_at, got %q", detail.Trace.CreatedAt)
	}
	if detail.Source.SourceKind != "architecture" || detail.Source.EventClass != "agent_run" {
		t.Fatalf("task_class must map to kind/event_class, got %+v", detail.Source)
	}
	if !contains(detail.Trace.Outputs, "ref:repo://control/state/runs/synthetic-run-001/spine-check.txt") {
		t.Fatalf("string evidence must survive as refs, got %v", detail.Trace.Outputs)
	}
	if detail.Trace.Summary == "" || !strings.Contains(detail.Trace.Summary, "synthetic-run-001") {
		t.Fatalf("summary must be synthesized from task_id, got %q", detail.Trace.Summary)
	}

	// The bare-string evidence row also imported and carries its ref.
	beta, err := ShowTrace(home, idFor("trace_", "synthetic-run-002"))
	if err != nil {
		t.Fatalf("bare-string evidence row unreachable: %v", err)
	}
	if !contains(beta.Trace.Outputs, "ref:repo://control/audits/2026-06-04/parking-manifest.md") {
		t.Fatalf("bare string evidence must survive, got %v", beta.Trace.Outputs)
	}
}

func seedTraceRecord(t *testing.T, home, id, createdAt, stream string) {
	t.Helper()
	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	var tr envelope.TraceEnvelope
	tr.Header.SchemaVersion = envelope.SVTraceEnvelope
	tr.Header.ID = id
	tr.Header.CreatedAt = createdAt
	tr.Source = envelope.TraceSource{System: "os", Stream: stream}
	tr.MutationClass = "read"
	tr.Summary = "summary " + id
	if _, err := store.PutJSON(id, tr, "  "); err != nil {
		t.Fatalf("seed trace %s: %v", id, err)
	}
}

func summaryIDs(summaries []TraceSummary) []string {
	out := make([]string, len(summaries))
	for i, s := range summaries {
		out[i] = s.ID
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestListTracesOrdersNewestFirstDeterministically is the mandatory slice-5B
// traces order falsifier. Post-cutover ListTraces reads trace_ records through
// the Scan seam and reconstructs the "most recent first" contract by sorting on
// the canonical chronological (created_at, id) order — the deduped-store
// stand-in for the frozen traces.ndjson append order. On a clean home the two
// agree byte-for-byte; same-created_at records are tie-broken by id (stable where
// physical append order is not). loadRecentTraceSummaries returns the same window
// oldest-first, and a duplicate lingering in the frozen ndjson never leaks in.
func TestListTracesOrdersNewestFirstDeterministically(t *testing.T) {
	home := t.TempDir()
	// Distinct created_at plus one same-created_at tie (c,d at t2).
	seedTraceRecord(t, home, "trace_a", "2026-07-15T03:00:00Z", "s")
	seedTraceRecord(t, home, "trace_b", "2026-07-15T01:00:00Z", "s")
	seedTraceRecord(t, home, "trace_c", "2026-07-15T02:00:00Z", "s")
	seedTraceRecord(t, home, "trace_d", "2026-07-15T02:00:00Z", "s") // tie with c

	// chronological asc (created_at,id) = [b(t1), c(t2), d(t2), a(t3)];
	// newest-first = reverse = [a, d, c, b].
	all, err := ListTraces(home, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := summaryIDs(all), []string{"trace_a", "trace_d", "trace_c", "trace_b"}; !sameStrings(got, want) {
		t.Fatalf("ListTraces order = %v, want newest-first %v", got, want)
	}

	// limit caps from the newest end.
	limited, err := ListTraces(home, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := summaryIDs(limited), []string{"trace_a", "trace_d"}; !sameStrings(got, want) {
		t.Fatalf("ListTraces limit=2 = %v, want %v", got, want)
	}

	// The most recent window, oldest-first: last 2 of chronological asc = [d, a].
	recent, err := loadRecentTraceSummaries(home, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := summaryIDs(recent), []string{"trace_d", "trace_a"}; !sameStrings(got, want) {
		t.Fatalf("loadRecentTraceSummaries(2) = %v, want oldest-first window %v", got, want)
	}

	// Dedup proof: duplicate lines lingering in the frozen traces.ndjson (the
	// pre-cutover double-count) must NOT appear — ListTraces reads the deduped
	// store, never the stream. Reverting the cutover would make this count 6.
	streamPath := filepath.Join(home, "events", "traces.ndjson")
	dupLine := `{"id":"trace_a","created_at":"2026-07-15T03:00:00Z","source":{"system":"os","stream":"s"},"mutation_class":"read"}` + "\n"
	if err := os.WriteFile(streamPath, []byte(dupLine+dupLine), 0o644); err != nil {
		t.Fatal(err)
	}
	afterDup, err := ListTraces(home, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterDup) != 4 {
		t.Fatalf("ListTraces must ignore frozen ndjson duplicates: got %d records, want 4", len(afterDup))
	}
}

// TestImportOSEventsFreezesTraceStreamAfterCompletion proves the writer freeze:
// on a completion-gated home, imported traces land as authoritative records
// (reachable via ShowTrace) but the traces.ndjson compatibility stream is never
// written. A pre-gate home still writes it — covered by
// TestImportOSEventsWritesTraceEnvelopesIdempotently.
func TestImportOSEventsFreezesTraceStreamAfterCompletion(t *testing.T) {
	home := t.TempDir()
	migDir := filepath.Join(home, "events", "migration")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migDir, "completion.json"), []byte(`{"complete":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join("..", "..", "fixtures", "events", "os-events.ndjson")
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	report, err := ImportOSEvents(home, fixture, ImportOptions{Stream: "control-events"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Imported != 2 {
		t.Fatalf("import = %+v", report)
	}
	for _, id := range report.TraceIDs {
		if _, err := ShowTrace(home, id); err != nil {
			t.Fatalf("trace %s must be reachable post-freeze: %v", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "events", "traces.ndjson")); !os.IsNotExist(err) {
		t.Fatalf("traces.ndjson must stay frozen (unwritten) on a completed home, stat err=%v", err)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	var b []byte
	if filepath.Base(filepath.Dir(path)) == "events" && strings.HasSuffix(path, ".json") {
		store, err := eventstore.Open(filepath.Dir(filepath.Dir(path)))
		if err != nil {
			t.Fatal(err)
		}
		record, err := store.Read(strings.TrimSuffix(filepath.Base(path), ".json"))
		if err != nil {
			t.Fatal(err)
		}
		b = record.Bytes
	} else {
		var err error
		b, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
