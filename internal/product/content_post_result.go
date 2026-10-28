package product

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// content_post_result.go closes the loop between a governed outbound post and the
// draft it came from.
//
// Before this, `act post --draft` executed the transport, printed a receipt, and
// left the draft sitting at "operationalized" forever. Nothing in the system could
// answer "did it actually go out?", so the weekly distribution driver measured the
// only thing it could see — whether a draft had been WRITTEN — and reported
// `weekly_floor_met` every day while zero posts had ever been published.
//
// The transport's own stdout is the post-image: the remote platform's answer. That
// answer is where a real post id lives, and a post id is the only evidence that
// separates "we sent it" from "we think we sent it".

// contentPostRemoteIDKeys are the field names a post transport may use for the
// remote object id, in priority order. The set is small and explicit rather than
// a loose search: a transport that returns none of these has not proven a post.
var contentPostRemoteIDKeys = []string{"remote_id", "post_id", "postId", "id"}

// contentScheduleAckKeys are the field names a transport's scheduler may use
// for its own RESERVATION id — an identity for the accepted job, never a
// platform post id (2026-09-15: the scheduling-ack gap closed with a distinct
// scheduled state).
var contentScheduleAckKeys = []string{"schedule_id", "scheduled_id", "job_id"}

// contentScheduleAckMarkerKeys and contentScheduleAckMarkerValues are the
// explicit markers a transport answer may use to say "this is a scheduling
// acceptance, not a publication".
var (
	contentScheduleAckMarkerKeys   = []string{"type", "status"}
	contentScheduleAckMarkerValues = []string{"schedule", "scheduled", "queued"}
)

// contentPostURLKeys are the field names a post transport may use for the public
// permalink. A URL is useful for a human but is NOT accepted as proof of posting —
// only a remote id is.
var contentPostURLKeys = []string{"url", "permalink", "releaseURL", "postUrl"}

// contentPostImageKind tells RecordContentPostResult which door the transport
// answer opens. A scheduling acceptance opens the scheduled door; only a
// platform publication answer opens the terminal posted door.
type contentPostImageKind string

const (
	postImageKindPublication contentPostImageKind = "publication"
	postImageKindScheduleAck contentPostImageKind = "schedule_ack"
)

// contentPostImage is the classified transport answer.
type contentPostImage struct {
	Kind        contentPostImageKind
	RemoteID    string
	URL         string
	ScheduleID  string
	ScheduledAt string
}

// RecordContentPostResult reads a transport's post-image and moves the draft to
// the state the answer PROVES. A platform answer carrying a real remote id
// records the terminal "posted" state; a scheduler's explicit acceptance
// records the distinct non-terminal "scheduled" state with the scheduler's own
// reservation id. Both doors fail LOUDLY when their evidence is missing. The
// post may well have gone out, and the caller must surface that rather than
// swallow it: a silent failure here would leave the draft looking unpublished
// forever, which is the same measurement lie this state was added to end.
func RecordContentPostResult(homePath, draftID string, postImage []byte, receiptRef string, now time.Time) (ContentDraft, error) {
	parsed, err := parseContentPostImage(postImage, now)
	if err != nil {
		return ContentDraft{}, err
	}
	if parsed.Kind == postImageKindScheduleAck {
		return MarkContentDraftScheduled(homePath, draftID, ContentDraftScheduleInput{
			ScheduleID:  parsed.ScheduleID,
			ScheduledAt: parsed.ScheduledAt,
			ReceiptRef:  receiptRef,
		}, now)
	}
	return MarkContentDraftPosted(homePath, draftID, ContentDraftPostInput{
		RemoteID:   parsed.RemoteID,
		URL:        parsed.URL,
		ReceiptRef: receiptRef,
	}, now)
}

// parseContentPostImage extracts the classified answer from a transport's
// stdout. The transport contract is a single JSON object; anything else cannot
// prove a post OR a scheduling acceptance.
//
// The schedule/post discrimination is explicit, never a guess:
//   - an explicit marker (type/status naming schedule/scheduled/queued), or
//   - a publish `date` strictly in the future (a publication has already
//     happened; a reservation names a time that has not come yet),
//
// makes an id-shaped field a SCHEDULER RESERVATION, and the answer routes to
// the scheduled door. An id-shaped field with NO such marker routes to the
// posted door exactly as before. A payload carrying an id-shaped field AND a
// schedule marker is refused as ambiguous: guessing across a publication
// boundary is how a scheduling acknowledgement became a "post" in the first
// place.
func parseContentPostImage(postImage []byte, now time.Time) (contentPostImage, error) {
	var out contentPostImage
	trimmed := strings.TrimSpace(string(postImage))
	if trimmed == "" {
		return out, fmt.Errorf("transport returned no post-image: a post cannot be recorded without the platform's answer")
	}
	// UseNumber, not the default float64: a snowflake-style id like
	// 1730000000000000123 does not survive a float64 round trip, and an id that
	// lost its last digits is worse than no id — it looks provable and is not.
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return out, fmt.Errorf("transport post-image is not a JSON object, so it carries no provable remote id: %w", err)
	}
	// The single-object contract is enforced through EOF (audit
	// 2026-09-16 finding 4; review 2026-09-19). Anything after the object
	// means the transport wrapper or logging layer corrupted the boundary;
	// the caller must learn "this transport is broken", not "the post
	// happened". `decoder.More()` answered the WRONG question — "is there
	// another element in the array or object being parsed" — so a stray `}`
	// or `]` after the object read as the end of a container and the
	// trailing input was dropped. Decode again and require io.EOF: only an
	// ended stream proves there was one object.
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		detail := "a second JSON value"
		if err != nil {
			detail = err.Error()
		}
		return out, fmt.Errorf("transport post-image does not end after one JSON object (%s): the transport contract is one object per post; refusing rather than silently dropping the trailing input", detail)
	}
	remoteID := firstStringField(payload, contentPostRemoteIDKeys)
	url := firstStringField(payload, contentPostURLKeys)
	markedScheduled := false
	for _, key := range contentScheduleAckMarkerKeys {
		if v := strings.ToLower(strings.TrimSpace(firstStringField(payload, []string{key}))); v != "" {
			for _, marker := range contentScheduleAckMarkerValues {
				if v == marker {
					markedScheduled = true
				}
			}
		}
	}
	// Negative-evidence gate (audit 2026-09-16 finding 4). A
	// platform that returns an id-shaped field AND a
	// status/type naming failure, error, or rejection is NOT a
	// publication — the remote id names the failed attempt, not a
	// post. Refuse the post-image outright and surface the platform's
	// own negative status so the caller can name what happened.
	if reason, negative := negativeContentPostImageStatus(payload); negative {
		return out, fmt.Errorf("transport post-image carries an explicit negative status (%s); the remote id names the failed attempt, not a post — refusing: %s", reason, payloadStatusSummary(payload))
	}
	// A future publish date is the scheduler's own claim about when the post
	// will happen; a publication has already happened, so its date cannot be
	// in the future. Two minutes of skew tolerance keeps a clock drift from
	// misreading an immediate post as a reservation.
	if v := strings.TrimSpace(firstStringField(payload, []string{"date", "scheduled_at"})); v != "" {
		if at, err := time.Parse(time.RFC3339, v); err == nil && at.After(now.Add(2*time.Minute)) {
			markedScheduled = true
			out.ScheduledAt = at.UTC().Format(time.RFC3339)
		}
	}
	// Positive-evidence gate (review 2026-09-19). An id alone used to reach
	// the publication branch by DEFAULT — "no scheduling marker and no
	// negative status" was treated as proof of a post, so an intermediate
	// answer like status="processing" was recorded as terminal. An outcome
	// claim the parser cannot read is ambiguous, not positive: refuse it and
	// leave the draft in its non-terminal state for reconciliation. An answer
	// that makes NO outcome claim at all (the shape a bare `{"remote_id":…}`
	// transport line takes) is unchanged — there is nothing to misread.
	positiveStatus, unreadableStatus := contentPostImageOutcomeClaim(payload)
	switch {
	case remoteID != "" && markedScheduled:
		return out, fmt.Errorf("transport post-image is ambiguous: it carries an id-shaped field and a scheduling marker, so the id could name a publication or a reservation; refusing to guess (looked at %s and markers %v)", strings.Join(contentPostRemoteIDKeys, ","), contentScheduleAckMarkerKeys)
	case remoteID != "" && unreadableStatus != "" && !positiveStatus:
		return out, fmt.Errorf("transport post-image is ambiguous: it carries an id-shaped field and an outcome claim (%s) that is neither a publication (%s), a scheduling marker (%s), nor a failure (%s); the post may have gone out, but nothing proves it did — refusing rather than recording a post the platform never confirmed: %s",
			unreadableStatus,
			strings.Join(contentPostPositiveStatusValues, ", "),
			strings.Join(contentScheduleAckMarkerValues, ", "),
			strings.Join(contentPostNegativeStatusValues, ", "),
			payloadStatusSummary(payload))
	case remoteID != "":
		if !positiveStatus {
			return out, fmt.Errorf("untyped identity alone cannot prove publication; reconcile through a versioned request-bound transport")
		}
		out.Kind = postImageKindPublication
		out.RemoteID = remoteID
		out.URL = url
		return out, nil
	case markedScheduled:
		scheduleID := firstStringField(payload, contentScheduleAckKeys)
		if scheduleID == "" {
			return out, fmt.Errorf("transport scheduling acknowledgement carries no reservation id (looked for %s): an acceptance without one cannot be observed, cancelled, or de-duplicated", strings.Join(contentScheduleAckKeys, ", "))
		}
		out.Kind = postImageKindScheduleAck
		out.ScheduleID = scheduleID
		return out, nil
	default:
		return out, fmt.Errorf("transport post-image carries no remote id (looked for %s): the post may have gone out, but nothing proves it", strings.Join(contentPostRemoteIDKeys, ", "))
	}
}

// contentPostNegativeStatusValues names the values a transport's status
// or type field uses to say the post DID NOT happen (audit 2026-09-16
// finding 4). A platform that returns such a value alongside a remote id
// is naming the failed attempt's reservation id, NOT a publication — the
// post-image is refused outright.
var contentPostNegativeStatusValues = []string{"failed", "error", "rejected", "denied", "cancelled"}

// negativeContentPostImageStatus reports whether the transport post-image
// payload carries an explicit negative status/type and, if so, the
// first reason value it saw. The check looks at the same `type` and
// `status` keys the schedule-marker path uses, so a single field does
// not flip between "scheduled" and "failed" semantics.
func negativeContentPostImageStatus(payload map[string]any) (string, bool) {
	for _, key := range contentScheduleAckMarkerKeys {
		v := strings.ToLower(strings.TrimSpace(firstStringField(payload, []string{key})))
		if v == "" {
			continue
		}
		for _, neg := range contentPostNegativeStatusValues {
			if v == neg {
				return key + "=" + v, true
			}
		}
	}
	return "", false
}

// contentPostPositiveStatusValues names the values a transport's status
// or type field uses to say the post DID happen. The set is closed and
// holds ONLY words a real transport answer proves — nothing that merely
// sounds successful. A plausible-looking word rebuilds the exact defect
// this gate closes: "sent" can mean sent to the scheduler's queue and
// "complete" can mean the request completed, either of which would record
// a publication the platform never made. The asymmetry is deliberate: a
// missing word costs a loud refusal the operator reconciles, while a wrong
// word costs a false post nobody can see. Adding one means citing the
// transport answer or contract that uses it — a decision, not a guess.
var contentPostPositiveStatusValues = []string{"ok", "published", "succeeded"}

// contentPostImageOutcomeClaim reads the same `type` and `status` keys the
// schedule-marker and negative paths use, and reports (a) whether the
// payload makes an explicit POSITIVE publication claim and (b) the first
// claim it could not read at all. An unreadable claim only decides the
// outcome when no sibling field states success, so a descriptive value next
// to an explicit `status=ok` does not manufacture a refusal.
func contentPostImageOutcomeClaim(payload map[string]any) (positive bool, unreadable string) {
	for _, key := range contentScheduleAckMarkerKeys {
		v := strings.ToLower(strings.TrimSpace(firstStringField(payload, []string{key})))
		if v == "" {
			continue
		}
		switch {
		case containsString(contentPostPositiveStatusValues, v):
			positive = true
		case containsString(contentScheduleAckMarkerValues, v), containsString(contentPostNegativeStatusValues, v):
			// Read by the scheduling and negative gates above.
		case unreadable == "":
			unreadable = key + "=" + v
		}
	}
	return positive, unreadable
}

// payloadStatusSummary renders the human-readable subset of a post-image
// payload that names what happened, so the refusal error tells the
// caller what the platform said rather than only "refused".
func payloadStatusSummary(payload map[string]any) string {
	parts := []string{}
	for _, key := range []string{"type", "status", "error", "message", "reason"} {
		if v := firstStringField(payload, []string{key}); v != "" {
			parts = append(parts, key+"="+v)
		}
	}
	if len(parts) == 0 {
		return "no status fields"
	}
	return strings.Join(parts, ", ")
}

// firstStringField returns the first non-empty string value among keys. Numeric ids
// are accepted too — platforms return them both ways — but only via a lossless
// integer rendering, never a float that could mangle a large id.
func firstStringField(payload map[string]any, keys []string) string {
	for _, key := range keys {
		switch v := payload[key].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case json.Number:
			// A fractional "id" is not an id. json.Number keeps every digit of a
			// large integer id verbatim, which float64 would silently round off.
			if s := strings.TrimSpace(v.String()); s != "" && !strings.Contains(s, ".") {
				return s
			}
		}
	}
	return ""
}
