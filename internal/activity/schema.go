package activity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// RefusedLine records ONE rejected NDJSON line. It stores the line number and a reason ONLY — never
// the raw content, so a refusal never persists a payload the schema is meant to keep out.
type RefusedLine struct {
	Line   int    `json:"line"`
	Reason string `json:"reason"`
}

// DecodeReport is the result of strict-decoding a spool byte range. Accepted events passed every
// invariant; Refused counts and explains every line that did not. A drain reports Refused honestly —
// a silent outage or a rejected line must never read as clean input.
type DecodeReport struct {
	Accepted []Event
	Refused  []RefusedLine
}

// DecodeStrict parses NDJSON spool bytes into events under a fail-closed contract:
//   - unknown top-level or nested (engine) field  -> line REFUSED (DisallowUnknownFields)
//   - unknown kind                                -> line REFUSED
//   - forbidden content-bearing meta key          -> line REFUSED (F5)
//   - malformed JSON / bad schema_version / bad ts / missing required field -> line REFUSED
//
// Each refusal is counted and explained; accepted events flow on. A blank line is skipped (not an
// error). startLine is the 1-based line number of the first line in data (for honest reporting when
// draining a partial byte range).
func DecodeStrict(data []byte, startLine int) DecodeReport {
	var report DecodeReport
	lineNo := startLine
	for _, raw := range bytes.Split(data, []byte("\n")) {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 {
			lineNo++
			continue
		}
		ev, err := decodeEventStrict(trimmed)
		if err != nil {
			report.Refused = append(report.Refused, RefusedLine{Line: lineNo, Reason: err.Error()})
			lineNo++
			continue
		}
		report.Accepted = append(report.Accepted, ev)
		lineNo++
	}
	return report
}

// decodeEventStrict decodes and validates ONE line. Any deviation from the closed contract is an
// error (the caller refuses+counts the line). It is the single content-free gate: no unknown field,
// no unknown kind, no forbidden meta key ever survives it.
func decodeEventStrict(line []byte) (Event, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var ev Event
	if err := dec.Decode(&ev); err != nil {
		return Event{}, fmt.Errorf("strict decode: %v", err)
	}
	// Reject trailing tokens (two JSON objects on one line, or garbage after the object).
	if dec.More() {
		return Event{}, fmt.Errorf("strict decode: unexpected trailing content after event object")
	}
	// Read-compat window, not an exact pin: accept the current writer version plus
	// any prior version still in acceptedSchemaVersions (ADR 0004). This is what
	// lets an upgraded binary drain spool the previous release wrote, while still
	// refusing genuinely unknown/future versions.
	if !acceptedSchemaVersions[ev.SchemaVersion] {
		return Event{}, fmt.Errorf("schema_version=%q not in read-compat set %v", ev.SchemaVersion, acceptedSchemaVersionList)
	}
	if !knownKinds[ev.Kind] {
		return Event{}, fmt.Errorf("unknown kind %q", ev.Kind)
	}
	if strings.TrimSpace(ev.TS) == "" {
		return Event{}, fmt.Errorf("ts required")
	}
	if _, err := time.Parse(time.RFC3339, ev.TS); err != nil {
		return Event{}, fmt.Errorf("ts %q not RFC3339: %v", ev.TS, err)
	}
	// app-scoped kinds must name an app; lifecycle kinds (idle/lock/gap/permission) need not.
	if requiresBundle(ev.Kind) && strings.TrimSpace(ev.BundleID) == "" {
		return Event{}, fmt.Errorf("kind %q requires bundle_id", ev.Kind)
	}
	for k := range ev.Meta {
		if forbiddenMetaKeys[strings.ToLower(strings.TrimSpace(k))] {
			return Event{}, fmt.Errorf("forbidden content meta key %q (Tier 0 is content-free)", k)
		}
	}
	return ev, nil
}

// requiresBundle reports whether a kind names a foreground app (so bundle_id is required). Idle,
// lock, capture_gap, and permission_lost are session-scoped and carry no app.
func requiresBundle(kind string) bool {
	switch kind {
	case KindAppActivated, KindWindowFocused:
		return true
	default:
		return false
	}
}
