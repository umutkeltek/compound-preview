package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"compound/internal/eventsource"
	"compound/internal/eventstore"
	"compound/internal/home"
)

// eventShowUsage documents the accepted id shapes for `compound event show`:
// a trace_-prefixed or bare trace id resolves through the existing trace
// envelope path; other supported record IDs, including exact approvals, resolve
// straight from the event store. No bespoke per-kind formatter — the store path
// always prints the record's own JSON body.
const eventShowUsage = "usage: compound event show <id> [--json] (trace_<id> | rec_<id> | hook_<id> | act_<id> | pol_<id> | tr_<id> | approval_plan_<id> | approval_grant_<id> | a bare trace id)"

// eventShowStoreKind names the human-readable label for each store-resolved
// record family, keyed by its canonical id prefix. trace_ is deliberately
// absent: trace ids stay on the existing eventsource.ShowTrace path.
var eventShowStoreKind = []struct {
	prefix string
	kind   string
}{
	{"rec_", "receipt"},
	{"hook_", "hook_event"},
	{"act_", "action_intent"},
	{"pol_", "policy_decision"},
	{"tr_", "transition"},
	{"approval_plan_", "exact_action_approval_plan"},
	{"approval_grant_", "exact_action_approval"},
}

// classifyEventShowID decides whether an id dispatches to the existing
// eventsource.ShowTrace path (trace_ ids, historical/logical compound://
// or events/ trace refs, and today's bare-id-normalizes-to-trace_ behavior —
// all byte-identical, untouched) or to the generic event-store read path for
// a known non-trace record family. An id carrying an unrecognized
// underscore-delimited prefix is rejected here, before ever reaching
// ShowTrace, rather than silently treated as (and possibly misread as) a
// trace id.
func classifyEventShowID(id string) (kind string, useShowTrace bool, err error) {
	trimmed := strings.TrimSpace(id)
	switch {
	case strings.HasPrefix(trimmed, "compound://events/"), strings.HasPrefix(trimmed, "events/"):
		return "", true, nil
	case strings.HasPrefix(trimmed, "trace_"):
		return "", true, nil
	}
	idx := strings.Index(trimmed, "_")
	if idx < 0 {
		// No recognizable prefix at all: preserve today's bare-id behavior,
		// which normalizes to trace_<id> inside eventsource.ShowTrace.
		return "", true, nil
	}
	prefix := trimmed[:idx+1]
	for _, k := range eventShowStoreKind {
		if strings.HasPrefix(trimmed, k.prefix) {
			return k.kind, false, nil
		}
	}
	return "", false, fmt.Errorf("unknown id prefix %q — supported: trace_, rec_, hook_, act_, pol_, tr_, approval_plan_, approval_grant_, or a bare trace id", prefix)
}

// runEventShow is the testable core of `compound event show`; cmdEvent's
// "show" branch is a thin os.Exit wrapper around it.
func runEventShow(args []string, stdout, stderr io.Writer) int {
	jsonOut := false
	recordID := ""
	for _, arg := range args {
		switch {
		case arg == "--json":
			jsonOut = true
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintln(stderr, "event show: unknown flag", arg)
			return 2
		default:
			if recordID != "" {
				fmt.Fprintln(stderr, "event show: only one id")
				return 2
			}
			recordID = arg
		}
	}
	if strings.TrimSpace(recordID) == "" {
		fmt.Fprintln(stderr, eventShowUsage)
		return 2
	}

	kind, useShowTrace, classifyErr := classifyEventShowID(recordID)
	if classifyErr != nil {
		fmt.Fprintln(stderr, "event show:", classifyErr)
		return 1
	}

	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err, "(run `compound home init`)")
		return 1
	}

	if !useShowTrace {
		if err := printEventStoreRecord(stdout, homePath, recordID, kind, jsonOut); err != nil {
			fmt.Fprintln(stderr, "event show:", err)
			return 1
		}
		return 0
	}

	detail, err := eventsource.ShowTrace(homePath, recordID)
	if err != nil {
		fmt.Fprintln(stderr, "event show:", err)
		return 1
	}
	if jsonOut {
		if err := writeJSONTo(stdout, detail); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "# event trace %s\n\n", detail.TraceID)
	fmt.Fprintf(stdout, "- authority: %s\n", detail.AuthorityRef)
	fmt.Fprintf(stdout, "- source_ref: %s\n", detail.SourceRef)
	fmt.Fprintf(stdout, "- stream: %s\n", detail.Source.Stream)
	fmt.Fprintf(stdout, "- source_id: %s\n", detail.Source.SourceID)
	fmt.Fprintf(stdout, "- source_line: %d\n", detail.Source.Line)
	if detail.Source.SourceKind != "" {
		fmt.Fprintf(stdout, "- source_kind: %s\n", detail.Source.SourceKind)
	}
	if detail.Source.EventClass != "" {
		fmt.Fprintf(stdout, "- event_class: %s\n", detail.Source.EventClass)
	}
	fmt.Fprintf(stdout, "- mutation_class: %s\n", detail.Trace.MutationClass)
	fmt.Fprintf(stdout, "- created_at: %s\n", detail.Trace.CreatedAt)
	if detail.Trace.Actor != "" {
		fmt.Fprintf(stdout, "- actor: %s\n", detail.Trace.Actor)
	}
	if detail.Trace.Summary != "" {
		fmt.Fprintf(stdout, "- summary: %s\n", detail.Trace.Summary)
	}
	fmt.Fprintf(stdout, "- trace_path: %s\n", detail.TracePath)
	for _, f := range detail.Trace.FilesTouched {
		fmt.Fprintf(stdout, "- file: %s\n", f)
	}
	for _, o := range detail.Trace.Outputs {
		fmt.Fprintf(stdout, "- output: %s\n", o)
	}
	return 0
}

// eventShowRecordHeader is a best-effort header decode for human-mode output.
// A missing field is not an error; the pretty JSON body still prints in full
// regardless of whether the header parses.
type eventShowRecordHeader struct {
	ID            string `json:"id"`
	SchemaVersion string `json:"schema_version"`
	CreatedAt     string `json:"created_at"`
}

// printEventStoreRecord resolves a known non-trace record id through the
// shared event-store read seam (compound/internal/eventstore, imported
// read-only here) and prints it: --json emits the record's raw stored JSON
// unmodified; human mode emits a compact header (id, kind, created_at if
// present) plus the same body reindented for readability. No per-kind
// formatter this slice — the body is always the raw record.
func printEventStoreRecord(stdout io.Writer, homePath, id, kind string, jsonOut bool) error {
	store, err := eventstore.Open(homePath)
	if err != nil {
		return err
	}
	record, err := store.Read(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s %q not found in event store", kind, id)
		}
		return err
	}
	if jsonOut {
		_, err := stdout.Write(ensureTrailingNewline(record.Bytes))
		return err
	}
	var hdr eventShowRecordHeader
	_ = json.Unmarshal(record.Bytes, &hdr)
	fmt.Fprintf(stdout, "# event record %s\n\n", id)
	fmt.Fprintf(stdout, "- kind: %s\n", kind)
	if hdr.CreatedAt != "" {
		fmt.Fprintf(stdout, "- created_at: %s\n", hdr.CreatedAt)
	}
	fmt.Fprintln(stdout)
	pretty, err := indentJSON(record.Bytes)
	if err != nil {
		// Body still prints even if it fails to re-indent (should not happen
		// for a store-authored record); never swallow the payload.
		pretty = record.Bytes
	}
	_, werr := stdout.Write(ensureTrailingNewline(pretty))
	return werr
}

func indentJSON(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, bytes.TrimRight(raw, "\n"), "", "  "); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func ensureTrailingNewline(b []byte) []byte {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return append(append([]byte(nil), b...), '\n')
	}
	return b
}

// writeJSONTo mirrors printJSON's MarshalIndent+Println shape but targets an
// arbitrary writer and returns an error instead of exiting, so the trace-path
// JSON branch stays testable through runEventShow.
func writeJSONTo(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}
