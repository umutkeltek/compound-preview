package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/home"
)

var contentDraftNow = time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

func recordTestDraft(t *testing.T, homePath, body string, expiresAt, now time.Time) ContentDraft {
	t.Helper()
	draft, err := RecordContentDraft(homePath, ContentDraftInput{
		ProductID:    "product:prod_synth_a",
		Platform:     "x",
		Body:         body,
		VoicePackRef: "voice:prod_synth_a_v1",
		EvidenceRefs: []string{"finding:synth_a", "source:synth_a"},
		ExpiresAt:    expiresAt,
	}, now)
	if err != nil {
		t.Fatalf("record draft: %v", err)
	}
	return draft
}

// RED capable: if OperationalizeContentDraft ever skips the expiry gate, the state
// becomes "operationalized" and this fails; if a refused operationalize rewrites or
// removes the record, the re-read / list assertions fail. Stale marketing posting is
// exactly the failure this guards.
func TestContentDraftExpiredRefusesOperationalizeButRemainsListable(t *testing.T) {
	homePath := t.TempDir()
	draft := recordTestDraft(t, homePath, "ship-day post for prod_synth_a", contentDraftNow, contentDraftNow)

	if _, err := OperationalizeContentDraft(homePath, draft.ID, contentDraftNow.Add(time.Hour)); err == nil {
		t.Fatal("expected expired draft to refuse operationalize, got nil error")
	} else if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired refusal error = %q, want it to name expiry", err)
	}

	got, err := ReadContentDraft(homePath, draft.ID)
	if err != nil {
		t.Fatalf("expired draft must still be readable: %v", err)
	}
	if got.State != ContentDraftStateProposed {
		t.Fatalf("refused operationalize mutated state to %q, want proposed (unchanged)", got.State)
	}
	if got.AdjudicatedAt != "" {
		t.Fatalf("refused operationalize recorded an adjudication timestamp %q", got.AdjudicatedAt)
	}
	list, err := ListContentDrafts(homePath, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != draft.ID {
		t.Fatalf("expired draft not listable: %+v", list)
	}
}

// RED capable: if RejectContentDraft ever deletes the file (violating keep-all-data)
// or fails to persist state=rejected, one of these assertions fails.
func TestContentDraftRejectRetainsFileWithRejectedState(t *testing.T) {
	homePath := t.TempDir()
	draft := recordTestDraft(t, homePath, "rejected voice sample for prod_synth_a", contentDraftNow.Add(24*time.Hour), contentDraftNow)

	rejected, err := RejectContentDraft(homePath, draft.ID, contentDraftNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if rejected.State != ContentDraftStateRejected {
		t.Fatalf("reject returned state %q, want rejected", rejected.State)
	}

	path := filepath.Join(homePath, candidates.InboxDir(candidates.ContentDraft), draft.ID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("reject deleted retained exhaust file %s: %v", path, err)
	}
	list, err := ListContentDrafts(homePath, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].State != ContentDraftStateRejected {
		t.Fatalf("rejected draft not listable with rejected state: %+v", list)
	}
}

// RED capable: if RecordContentDraft ever sets a promoted/operationalized state or
// writes an adjudication with no verb call, the re-read from disk fails here.
func TestContentDraftRecordDoesNotAutoPromote(t *testing.T) {
	homePath := t.TempDir()
	draft := recordTestDraft(t, homePath, "no self-promotion body", contentDraftNow.Add(24*time.Hour), contentDraftNow)
	if draft.State != ContentDraftStateProposed {
		t.Fatalf("record returned state %q, want proposed", draft.State)
	}
	got, err := ReadContentDraft(homePath, draft.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.State != ContentDraftStateProposed {
		t.Fatalf("recorded draft on disk has state %q, want proposed (nothing self-promotes)", got.State)
	}
	if got.AdjudicatedAt != "" {
		t.Fatalf("record wrote adjudicated_at %q with no verb call", got.AdjudicatedAt)
	}
}

// RED capable: if the body stops being content-addressed (two identical bodies get
// different refs) or the two records collapse into one draft, this fails. Also proves
// the ref round-trips to the exact post text.
func TestContentDraftSameBodyDedupsRefDistinctDrafts(t *testing.T) {
	homePath := t.TempDir()
	body := "identical marketing body for prod_synth_a"
	d1 := recordTestDraft(t, homePath, body, contentDraftNow.Add(24*time.Hour), contentDraftNow)
	d2 := recordTestDraft(t, homePath, body, contentDraftNow.Add(24*time.Hour), contentDraftNow.Add(time.Minute))

	if d1.BodyRef != d2.BodyRef {
		t.Fatalf("identical body did not content-address to one ref: %q vs %q", d1.BodyRef, d2.BodyRef)
	}
	if d1.ID == d2.ID {
		t.Fatalf("two records collapsed into one draft id %q", d1.ID)
	}
	list, err := ListContentDrafts(homePath, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 distinct drafts sharing one body, got %d", len(list))
	}
	store, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	gotBody, err := store.Get(d1.BodyRef)
	if err != nil {
		t.Fatalf("body ref did not resolve: %v", err)
	}
	if string(gotBody) != body {
		t.Fatalf("body ref %q resolved to %q, want the exact post text", d1.BodyRef, string(gotBody))
	}
}

// A fresh draft must actually operationalize — proves the expiry gate is a real
// window, not a blanket refusal.
func TestContentDraftFreshOperationalizeSucceeds(t *testing.T) {
	homePath := t.TempDir()
	draft := recordTestDraft(t, homePath, "fresh operationalizable post", contentDraftNow.Add(24*time.Hour), contentDraftNow)
	op, err := OperationalizeContentDraft(homePath, draft.ID, contentDraftNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("fresh draft failed to operationalize: %v", err)
	}
	if op.State != ContentDraftStateOperationalized {
		t.Fatalf("operationalized state = %q, want operationalized", op.State)
	}
}

// RED capable: if a junk platform is accepted, an unroutable draft reaches the
// distribution loop.
func TestContentDraftRejectsUnknownPlatform(t *testing.T) {
	homePath := t.TempDir()
	_, err := RecordContentDraft(homePath, ContentDraftInput{
		ProductID: "product:prod_synth_a",
		Platform:  "myspace",
		Body:      "junk platform body",
		ExpiresAt: contentDraftNow.Add(24 * time.Hour),
	}, contentDraftNow)
	if err == nil {
		t.Fatal("expected unknown platform to be rejected")
	}
}

// RED capable: expiry is mandatory — a draft with no expiry could post forever.
func TestContentDraftRequiresExpiry(t *testing.T) {
	homePath := t.TempDir()
	_, err := RecordContentDraft(homePath, ContentDraftInput{
		ProductID: "product:prod_synth_a",
		Platform:  "bluesky",
		Body:      "no-expiry body",
	}, contentDraftNow)
	if err == nil {
		t.Fatal("expected missing expiry to be rejected")
	}
}
