package product

import (
	"strings"
	"testing"
)

// Audit 2026-09-16, finding 4 (publication-result classification): the
// parser treated "remote_id present" as proof of publication. A transport
// response like {"id":"request-123","status":"failed"} had a remote id
// and so reached the publication branch — MarkContentDraftPosted recorded
// a posted state that the platform itself rejected.
//
// TestParseContentPostImageRefusesNegativeStatus is the red falsifier: any
// status string the platform sends that names failure, error, or rejection
// (and the matching type field) MUST refuse the post-image outright,
// regardless of whether an id-shaped field is present. A refusal must
// surface the explicit status so the caller can name what the platform said.
func TestParseContentPostImageRefusesNegativeStatus(t *testing.T) {
	cases := []struct {
		name  string
		image string
		// substring that must appear in the error so the caller can act on it
		wantSubstring string
	}{
		{"status failed", `{"id":"r-1","status":"failed"}`, "failed"},
		{"status error", `{"id":"r-1","status":"error"}`, "error"},
		{"status rejected", `{"id":"r-1","status":"rejected"}`, "rejected"},
		{"type error", `{"id":"r-1","type":"error"}`, "error"},
		{"status with explicit reason", `{"id":"r-1","status":"failed","error":"publication rejected"}`, "publication rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseContentPostImage([]byte(tc.image), postResultNow)
			if err == nil {
				t.Fatalf("a post-image carrying an explicit negative status/type must be refused; got nil err for %s", tc.image)
			}
			if !strings.Contains(err.Error(), tc.wantSubstring) {
				t.Fatalf("refusal must surface the platform's own status: got %q, want substring %q", err.Error(), tc.wantSubstring)
			}
			// And the refusal must NOT route to scheduled either — that
			// would invent a future reservation id where the platform said
			// no.
			parsed, _ := parseContentPostImage([]byte(tc.image), postResultNow)
			if parsed.Kind != "" {
				t.Fatalf("refusal must not silently classify as %q", parsed.Kind)
			}
		})
	}
}

// TestParseContentPostImageAcceptsPositiveStatus pins the symmetric rule:
// a transport that returns {"id":"r-1","status":"ok"} (or "published",
// "succeeded") with an id-shaped field IS a publication. The status field
// narrows the publication branch — it does not invent refusals where the
// platform answered positively.
func TestParseContentPostImageAcceptsPositiveStatus(t *testing.T) {
	cases := []string{
		`{"id":"r-1","status":"ok"}`,
		`{"id":"r-1","status":"published"}`,
		`{"id":"r-1","status":"succeeded"}`,
	}
	for _, image := range cases {
		t.Run(image, func(t *testing.T) {
			parsed, err := parseContentPostImage([]byte(image), postResultNow)
			if err != nil {
				t.Fatalf("positive status must classify as publication, got err %v", err)
			}
			if parsed.Kind != postImageKindPublication {
				t.Fatalf("parsed.Kind = %q, want %q", parsed.Kind, postImageKindPublication)
			}
			if parsed.RemoteID != "r-1" {
				t.Fatalf("RemoteID = %q, want r-1", parsed.RemoteID)
			}
		})
	}
}

// Audit 2026-09-16, finding 4 (publication-result classification): the
// single-object contract was not enforced through EOF. A transport that
// returned two JSON objects back-to-back (the second often a status line
// from a wrapper or a stray newline from a logging layer) had only the
// first decoded, and the second silently dropped. A transport protocol
// that allows trailing JSON is a contract the caller cannot trust.
//
// TestParseContentPostImageRefusesTrailingJSON is the red falsifier: two
// JSON objects in the same stream MUST be refused as a contract violation.
// The caller learns "this transport is broken", not "the post happened".
func TestParseContentPostImageRefusesTrailingJSON(t *testing.T) {
	// Two well-formed objects, the first looks like a publication.
	image := `{"id":"r-1","status":"ok"}` + "\n" + `{"id":"r-2","status":"ok"}`
	_, err := parseContentPostImage([]byte(image), postResultNow)
	if err == nil {
		t.Fatalf("a post-image with trailing JSON must be refused as a transport contract violation")
	}
	if !strings.Contains(err.Error(), "trailing") && !strings.Contains(err.Error(), "single") && !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("refusal must name the contract violation: %v", err)
	}

	// And the refusal must leave the draft untouched.
	homePath := t.TempDir()
	draft := operationalizedDraft(t, homePath)
	_, err = RecordContentPostResult(homePath, draft.ID, []byte(image), "rec_x", postResultNow)
	if err == nil {
		t.Fatalf("RecordContentPostResult must propagate the contract violation, not record a posted state")
	}
	after, readErr := ReadContentDraft(homePath, draft.ID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if after.State != ContentDraftStateOperationalized {
		t.Fatalf("refused post-image changed draft state to %q; record must be untouched", after.State)
	}
}
