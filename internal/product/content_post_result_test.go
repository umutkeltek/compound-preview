package product

import (
	"strings"
	"testing"
	"time"
)

var postResultNow = time.Date(2026, 8, 31, 15, 0, 0, 0, time.UTC)

func operationalizedDraft(t *testing.T, homePath string) ContentDraft {
	t.Helper()
	draft, err := RecordContentDraft(homePath, ContentDraftInput{
		ProductID: "product:prod_synth_a",
		Platform:  "x",
		Body:      "synthetic body for post-result falsifiers",
		ExpiresAt: postResultNow.Add(336 * time.Hour),
	}, postResultNow)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := OperationalizeContentDraft(homePath, draft.ID, postResultNow); err != nil {
		t.Fatalf("operationalize: %v", err)
	}
	return draft
}

// TestRecordContentPostResultClosesTheLoop is the falsifier for the seam between a
// governed outbound post and the draft it came from.
//
// `act post --draft` used to execute the transport, print a receipt, and leave the
// draft at "operationalized" forever — so no measure could ever distinguish a post
// that shipped from one that did not. The transport's stdout is the platform's own
// answer, and the id inside it is the only proof a post exists.
func TestRecordContentPostResultClosesTheLoop(t *testing.T) {
	homePath := t.TempDir()
	draft := operationalizedDraft(t, homePath)

	posted, err := RecordContentPostResult(homePath, draft.ID,
		[]byte(`{"id":"1899","url":"https://example.invalid/p/1899","status":"ok"}`),
		"rec_synthetic", postResultNow)
	if err != nil {
		t.Fatalf("record post result: %v", err)
	}
	if posted.State != ContentDraftStatePosted {
		t.Fatalf("state = %q, want posted", posted.State)
	}
	if posted.Post == nil || posted.Post.RemoteID != "1899" {
		t.Fatalf("remote id not carried from the transport answer: %+v", posted.Post)
	}
	if posted.Post.URL != "https://example.invalid/p/1899" {
		t.Fatalf("permalink not carried: %+v", posted.Post)
	}
	if posted.Post.ReceiptRef != "rec_synthetic" {
		t.Fatalf("governed receipt ref not carried: %+v", posted.Post)
	}
}

// TestRecordContentPostResultRefusesUnprovablePostImage pins the loud-failure rule.
// A transport answer with no id may still mean the post went out — so this must
// FAIL, visibly, rather than record a posted state nothing supports or silently
// leave the draft looking unpublished.
func TestRecordContentPostResultRefusesUnprovablePostImage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		image string
	}{
		{"empty", ""},
		{"not json", "posted ok!"},
		{"json without an id", `{"status":"ok","message":"queued"}`},
		{"fractional id is not an id", `{"id":1.5}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			draft := operationalizedDraft(t, homePath)
			_, err := RecordContentPostResult(homePath, draft.ID, []byte(tc.image), "rec_x", postResultNow)
			if err == nil {
				t.Fatal("an unprovable post-image must be refused loudly")
			}
			after, readErr := ReadContentDraft(homePath, draft.ID)
			if readErr != nil {
				t.Fatalf("read: %v", readErr)
			}
			if after.State != ContentDraftStateOperationalized {
				t.Fatalf("refused post-image changed state to %q; the record must be untouched", after.State)
			}
		})
	}
}

// TestRecordContentPostResultAcceptsNumericRemoteID pins that a platform returning
// a large integer id keeps every digit. Rendering through a float would silently
// corrupt real snowflake-style ids.
func TestRecordContentPostResultAcceptsNumericRemoteID(t *testing.T) {
	homePath := t.TempDir()
	draft := operationalizedDraft(t, homePath)
	posted, err := RecordContentPostResult(homePath, draft.ID, []byte(`{"post_id":1730000000000000123,"status":"published"}`), "", postResultNow)
	if err != nil {
		t.Fatalf("record post result: %v", err)
	}
	if posted.Post.RemoteID != "1730000000000000123" {
		t.Fatalf("large numeric id mangled: got %q", posted.Post.RemoteID)
	}
}

// TestParseContentPostImageNamesWhatItLookedFor keeps the refusal actionable: an
// operator reading it must learn which field a transport is expected to return.
func TestParseContentPostImageNamesWhatItLookedFor(t *testing.T) {
	_, err := parseContentPostImage([]byte(`{"status":"ok"}`), postResultNow)
	if err == nil {
		t.Fatal("want refusal")
	}
	for _, key := range contentPostRemoteIDKeys {
		if !strings.Contains(err.Error(), key) {
			t.Fatalf("refusal must name accepted key %q, got: %v", key, err)
		}
	}
}
