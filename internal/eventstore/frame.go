package eventstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const frameVersion = 1

// Frame is one self-validating event record in a segment JSONL stream.
type Frame struct {
	FrameVersion  int             `json:"frame_version"`
	Family        string          `json:"family"`
	ID            string          `json:"id"`
	SchemaVersion string          `json:"schema_version,omitempty"`
	CreatedAt     string          `json:"created_at,omitempty"`
	SHA256        string          `json:"sha256"`
	Record        json.RawMessage `json:"record"`
}

// FrameInvalidError distinguishes a malformed frame from an I/O failure.
type FrameInvalidError struct {
	Reason string
	Cause  error
}

func (e *FrameInvalidError) Error() string {
	if e == nil {
		return "invalid event frame"
	}
	if e.Cause != nil {
		return fmt.Sprintf("invalid event frame: %s: %v", e.Reason, e.Cause)
	}
	return "invalid event frame: " + e.Reason
}

func (e *FrameInvalidError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type frameRecordHeader struct {
	ID            string `json:"id"`
	SchemaVersion string `json:"schema_version"`
	CreatedAt     string `json:"created_at"`
}

func canonical(record []byte) ([]byte, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, record); err != nil {
		return nil, fmt.Errorf("canonicalize event record: %w", err)
	}
	return compact.Bytes(), nil
}

func recordHash(record []byte) string {
	sum := sha256.Sum256(record)
	return hex.EncodeToString(sum[:])
}

// idPrefixFamilies is the ordered id-prefix -> family vocabulary. familyOf's
// id-based fallback and the Scan/ScanFrom prefix-routing optimization (see
// scanFamilyRestriction in store.go) share this ONE table so the I/O routing
// can never drift from actual family assignment: a new prefix added here
// updates both consumers consistently. Order matters only in that a later
// entry must not change the resolved family for an id already matched by an
// earlier entry (act_live_ and act_ both resolve to "action", so their
// relative order is immaterial).
var idPrefixFamilies = []struct{ prefix, family string }{
	{"tr_", "transition"},
	{"pol_", "policy"},
	{"hook_", "hook"},
	{"act_", "action"},
	{"act_live_", "action"},
	{"trace_", "trace"},
	{"rec_", "receipt"},
	// approval_ MUST stay mapped: the pre-tool-use hook scans this prefix on
	// every tool call (policy.ExactActionApprovalStore.ApplyMatching), and an
	// unmapped prefix disables routing entirely -- that scan then parses every
	// segment in the store, in every concurrent lane, on every tool call.
	{"approval_", "approval"},
	// Proof refresh outcomes and preparations historically fall through to
	// "other". Explicit routing preserves that storage family while allowing
	// prefix reads to exclude unrelated families.
	{"pfrh_", "other"},
	{"pfrp_", "other"},
	// Closed credential alignment launch-intent evidence already belongs to
	// other. Keep its admission/finalization scan out of unrelated families.
	{"cpaf_", "other"},
}

func familyOf(record []byte, id string) string {
	var header frameRecordHeader
	if len(record) > 0 && json.Unmarshal(record, &header) == nil {
		switch header.SchemaVersion {
		case "compound.transition.v0.1":
			return "transition"
		case "compound.policy_decision.v0.1":
			return "policy"
		case "compound.hook_event.v0.1":
			return "hook"
		case "compound.action_intent.v0.1":
			return "action"
		case "compound.trace_envelope.v0.1":
			return "trace"
		case "compound.receipt.v0.1":
			return "receipt"
		}
	}
	for _, pf := range idPrefixFamilies {
		if strings.HasPrefix(id, pf.prefix) {
			return pf.family
		}
	}
	return "other"
}

// familyForPrefix reports the single family every id matching a Scan/ScanFrom
// filter prefix could belong to, using the same idPrefixFamilies vocabulary
// that assigns record family membership. ok is true only when prefix fully
// contains one canonical id-prefix as its own prefix -- i.e. every id
// extending prefix necessarily extends that canonical prefix too, so the
// named family is a provably exhaustive (never under-inclusive) answer for
// the filter. A prefix shorter than, or diverging from, every canonical
// prefix is reported !ok: it could also match an "other"-family id (the
// unbounded default bucket), and narrowing scan I/O on that guess would risk
// silently dropping records. Never guess-narrow.
func familyForPrefix(prefix string) (string, bool) {
	if prefix == "" {
		return "", false
	}
	for _, pf := range idPrefixFamilies {
		if strings.HasPrefix(prefix, pf.prefix) {
			return pf.family, true
		}
	}
	return "", false
}

// idPrefixFamily reports the family idPrefixFamilies alone would route id to,
// ignoring any schema_version -- i.e. exactly what every READ path resolves
// (segmentFrame calls familyOf(nil, id), so the schema-first branch never
// runs there: a read has no record bytes to consult before it has found the
// record). ok is false when no entry matches, meaning no read path could
// disagree with a schema-derived family for this id.
func idPrefixFamily(id string) (string, bool) {
	for _, pf := range idPrefixFamilies {
		if strings.HasPrefix(id, pf.prefix) {
			return pf.family, true
		}
	}
	return "", false
}

func buildFrame(id string, record []byte) (Frame, []byte, error) {
	if _, err := ReferenceForID(id); err != nil {
		return Frame{}, nil, err
	}
	compact, err := canonical(record)
	if err != nil {
		return Frame{}, nil, err
	}
	var header frameRecordHeader
	if err := json.Unmarshal(compact, &header); err != nil {
		return Frame{}, nil, &FrameInvalidError{Reason: "decode record header", Cause: err}
	}
	if header.ID != id {
		return Frame{}, nil, &FrameInvalidError{Reason: fmt.Sprintf("record id %q does not match put id %q", header.ID, id)}
	}
	family := familyOf(compact, id)
	// Fail closed on a routing disagreement: every READ path (segmentFrame)
	// resolves family via familyOf(nil, id) -- id-prefix only, since a read has
	// no record bytes yet to consult the schema-first branch with. If this
	// id's prefix names a DIFFERENT family than the schema just resolved to,
	// the frame would be written into one family's segment while every same-id
	// read looks in the other and misses it -- silent, unrecoverable data loss
	// (audit E4). Refusing here is the only point that has both the id and the
	// record at once to catch it.
	if prefixFamily, ok := idPrefixFamily(id); ok && prefixFamily != family {
		return Frame{}, nil, &FrameInvalidError{Reason: fmt.Sprintf("id %q prefix routes to family %q but its schema_version routes to family %q; refusing to write a frame no id-prefix read could ever find", id, prefixFamily, family)}
	}
	frame := Frame{
		FrameVersion:  frameVersion,
		Family:        family,
		ID:            id,
		SchemaVersion: header.SchemaVersion,
		CreatedAt:     header.CreatedAt,
		SHA256:        recordHash(compact),
		Record:        append(json.RawMessage(nil), compact...),
	}
	return frame, compact, nil
}

func encodeFrame(frame Frame) ([]byte, error) {
	if err := validateFrame(frame); err != nil {
		return nil, err
	}
	line, err := json.Marshal(frame)
	if err != nil {
		return nil, &FrameInvalidError{Reason: "encode", Cause: err}
	}
	if bytes.ContainsRune(line, '\n') {
		return nil, &FrameInvalidError{Reason: "encoded frame contains an interior newline"}
	}
	return append(line, '\n'), nil
}

func decodeFrame(line []byte) (Frame, error) {
	if bytes.ContainsRune(line, '\n') {
		return Frame{}, &FrameInvalidError{Reason: "line contains an interior newline"}
	}
	var frame Frame
	if err := json.Unmarshal(line, &frame); err != nil {
		return Frame{}, &FrameInvalidError{Reason: "decode", Cause: err}
	}
	if err := validateFrame(frame); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

func validateFrame(frame Frame) error {
	if frame.FrameVersion != frameVersion {
		return &FrameInvalidError{Reason: fmt.Sprintf("frame_version=%d, want %d", frame.FrameVersion, frameVersion)}
	}
	if frame.ID == "" || frame.Family == "" {
		return &FrameInvalidError{Reason: "id and family are required"}
	}
	compact, err := canonical(frame.Record)
	if err != nil {
		return &FrameInvalidError{Reason: "canonicalize record", Cause: err}
	}
	if got := recordHash(compact); frame.SHA256 != got {
		return &FrameInvalidError{Reason: fmt.Sprintf("record checksum %q does not match %q", frame.SHA256, got)}
	}
	var header frameRecordHeader
	if err := json.Unmarshal(compact, &header); err != nil {
		return &FrameInvalidError{Reason: "decode record header", Cause: err}
	}
	if header.ID != frame.ID {
		return &FrameInvalidError{Reason: fmt.Sprintf("record id %q does not match frame id %q", header.ID, frame.ID)}
	}
	if header.SchemaVersion != frame.SchemaVersion {
		return &FrameInvalidError{Reason: fmt.Sprintf("record schema_version %q does not match frame %q", header.SchemaVersion, frame.SchemaVersion)}
	}
	if header.CreatedAt != frame.CreatedAt {
		return &FrameInvalidError{Reason: fmt.Sprintf("record created_at %q does not match frame %q", header.CreatedAt, frame.CreatedAt)}
	}
	if family := familyOf(compact, frame.ID); frame.Family != family {
		return &FrameInvalidError{Reason: fmt.Sprintf("record family %q does not match frame %q", family, frame.Family)}
	}
	return nil
}
