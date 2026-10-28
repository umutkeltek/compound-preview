package product

import (
	"strings"
	"testing"
)

// Review 2026-09-19 (publication evidence): the classifier still DEFAULTED to
// published. An id-shaped field that carried neither a scheduling marker nor a
// recognized negative status reached the publication branch, so a transport
// answer like {"id":"r-1","status":"processing"} — an intermediate state the
// platform has not finished — was recorded as a terminal post. "Nothing said
// no" is not evidence that a post exists; only the platform's own positive
// answer is.
//
// TestParseContentPostImageRefusesUnreadableOutcomeClaim is the red falsifier:
// an id-bearing image whose outcome claim the parser cannot read is AMBIGUOUS
// and must be refused by name, never routed to the posted door.
func TestParseContentPostImageRefusesUnreadableOutcomeClaim(t *testing.T) {
	for _, tc := range []struct {
		name  string
		image string
		// the platform's own word, which the refusal must quote back
		wantSubstring string
	}{
		{"status processing", `{"id":"r-1","status":"processing"}`, "processing"},
		{"status pending", `{"id":"r-1","status":"pending"}`, "pending"},
		{"status in review", `{"id":"r-1","status":"in_review"}`, "in_review"},
		{"unknown vendor word", `{"post_id":"r-2","status":"awaiting_moderation"}`, "awaiting_moderation"},
		{"type carries the claim", `{"id":"r-3","type":"processing"}`, "processing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseContentPostImage([]byte(tc.image), postResultNow)
			if err == nil {
				t.Fatalf("an unreadable outcome claim must be refused, not published; got nil err for %s", tc.image)
			}
			if parsed.Kind != "" {
				t.Fatalf("refusal must not classify the image as %q", parsed.Kind)
			}
			if !strings.Contains(err.Error(), tc.wantSubstring) {
				t.Fatalf("refusal must quote the platform's own claim: got %q, want substring %q", err.Error(), tc.wantSubstring)
			}
			if !strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("refusal must name the ambiguity so the caller can reconcile: %v", err)
			}
		})
	}
}

// TestParseContentPostImageRefusesUnprovenSuccessVocabulary keeps the positive
// set honest. Every value in it must be a word a real transport answer or a
// documented transport contract actually uses for "this is out"; a word that
// merely sounds successful recreates the very failure the positive gate closes,
// because "sent" can mean sent-to-the-queue and "complete" can mean the request
// completed. None of the words below are proven by any fixture or contract in
// this repo, so each must refuse. Earning one back means citing where a
// transport documents it AND editing this table — a decision, not an accident.
func TestParseContentPostImageRefusesUnprovenSuccessVocabulary(t *testing.T) {
	for _, status := range []string{"sent", "complete", "completed", "live", "posted", "success"} {
		t.Run(status, func(t *testing.T) {
			image := `{"id":"r-1","status":"` + status + `"}`
			parsed, err := parseContentPostImage([]byte(image), postResultNow)
			if err == nil {
				t.Fatalf("an unproven success word must be refused, not published; got nil err for %s", image)
			}
			if parsed.Kind != "" {
				t.Fatalf("refusal must not classify the image as %q", parsed.Kind)
			}
			if !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), status) {
				t.Fatalf("refusal must name the ambiguity and quote the platform's word: %v", err)
			}
		})
	}
}

// TestRecordContentPostResultLeavesAmbiguousImageForReconciliation pins where an
// ambiguous outcome lands. The post may well have gone out, so the draft must
// NOT move to posted and must NOT be invented into a scheduler reservation: it
// stays at the non-terminal operationalized state the reconciliation path
// already understands, and the refusal is loud.
func TestRecordContentPostResultLeavesAmbiguousImageForReconciliation(t *testing.T) {
	homePath := t.TempDir()
	draft := operationalizedDraft(t, homePath)

	if _, err := RecordContentPostResult(homePath, draft.ID,
		[]byte(`{"id":"r-1","status":"processing"}`), "rec_x", postResultNow); err == nil {
		t.Fatal("an ambiguous post-image must be refused loudly, not recorded as a post")
	}
	after, err := ReadContentDraft(homePath, draft.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if after.State != ContentDraftStateOperationalized {
		t.Fatalf("ambiguous post-image moved the draft to %q; it must stay reconcilable", after.State)
	}
	if after.Post != nil {
		t.Fatalf("ambiguous post-image wrote a post record: %+v", after.Post)
	}
}

// TestParseContentPostImageKeepsProvenAnswersGreen is the other half of the
// invariant: tightening the publication gate must not refuse the shapes the
// legacy explicit-positive reader accepts. Identity-only legacy answers are
// refused; the production outbound path additionally requires the request-bound
// v0.2 observation contract.
func TestParseContentPostImageKeepsProvenAnswersGreen(t *testing.T) {
	for _, tc := range []struct {
		name     string
		image    string
		wantKind contentPostImageKind
		wantID   string
	}{
		{"positive status ok", `{"id":"1899","url":"https://example.invalid/p/1899","status":"ok"}`, postImageKindPublication, "1899"},
		{"positive status published", `{"id":"r-1","status":"published"}`, postImageKindPublication, "r-1"},
		{"positive status succeeded", `{"id":"r-1","status":"succeeded"}`, postImageKindPublication, "r-1"},
		{"explicit publication, numeric id", `{"post_id":1730000000000000123,"status":"published"}`, postImageKindPublication, "1730000000000000123"},
		{"explicit publication, fixture transport shape", `{"remote_id":"post-synth","status":"published","verified":true}`, postImageKindPublication, "post-synth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseContentPostImage([]byte(tc.image), postResultNow)
			if err != nil {
				t.Fatalf("proven publication answer refused: %v", err)
			}
			if parsed.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q", parsed.Kind, tc.wantKind)
			}
			if parsed.RemoteID != tc.wantID {
				t.Fatalf("RemoteID = %q, want %q", parsed.RemoteID, tc.wantID)
			}
		})
	}
}

// TestParseContentPostImageKeepsSchedulingAcceptancesGreen guards the other
// live door. The outbound request asks for `type: schedule` with a future
// date, so the scheduler's acceptance must keep routing to the distinct
// non-terminal scheduled state rather than being caught by the tightened
// publication gate.
func TestParseContentPostImageKeepsSchedulingAcceptancesGreen(t *testing.T) {
	for _, tc := range []struct {
		name           string
		image          string
		wantScheduleID string
	}{
		{"explicit marker", `{"schedule_id":"job-1","status":"scheduled"}`, "job-1"},
		{"queued marker", `{"job_id":"job-2","type":"queued"}`, "job-2"},
		{"future publish date", `{"scheduled_id":"job-3","date":"2026-09-01T09:00:00Z"}`, "job-3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseContentPostImage([]byte(tc.image), postResultNow)
			if err != nil {
				t.Fatalf("scheduling acceptance refused: %v", err)
			}
			if parsed.Kind != postImageKindScheduleAck {
				t.Fatalf("Kind = %q, want %q", parsed.Kind, postImageKindScheduleAck)
			}
			if parsed.ScheduleID != tc.wantScheduleID {
				t.Fatalf("ScheduleID = %q, want %q", parsed.ScheduleID, tc.wantScheduleID)
			}
		})
	}
}

// Review 2026-09-19 (end of input): the single-object contract was checked with
// `decoder.More()`, which answers "is there another element in the array or
// object I am parsing" — so a stray `}` or `]` after the object read as the end
// of a container and the trailing bytes were silently dropped. The only
// question worth asking is whether the STREAM ended.
//
// TestParseContentPostImageRefusesAnyTrailingInput is the red falsifier: the
// closing-bracket cases below are accepted on the pre-fix parser.
func TestParseContentPostImageRefusesAnyTrailingInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		image string
	}{
		{"second object", `{"id":"x"} {"id":"y"}`},
		{"garbage tail", `{"id":"x"} garbage`},
		{"stray close brace", `{"id":"x"}}`},
		{"stray close bracket", `{"id":"x"}]`},
		{"wrapped in an array tail", `{"id":"x"},{"id":"y"}]`},
		{"trailing scalar", `{"id":"x"} 7`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseContentPostImage([]byte(tc.image), postResultNow)
			if err == nil {
				t.Fatalf("a post-image with trailing input must be refused as a transport contract violation: %s", tc.image)
			}
			if parsed.Kind != "" {
				t.Fatalf("refusal must not classify the image as %q", parsed.Kind)
			}
			if !strings.Contains(err.Error(), "trailing") && !strings.Contains(err.Error(), "more than one") {
				t.Fatalf("refusal must name the contract violation: %v", err)
			}
		})
	}
}
