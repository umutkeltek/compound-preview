package product

import (
	"strings"
	"testing"
	"time"
)

var draftMediaNow = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

func recordMediaDraft(t *testing.T, homePath string, media []ContentDraftMediaInput, now time.Time) ContentDraft {
	t.Helper()
	draft, err := RecordContentDraft(homePath, ContentDraftInput{
		ProductID:    "product:prod_synth_a",
		Platform:     "x",
		Body:         "synthetic body for media falsifiers",
		VoicePackRef: "voice:prod_synth_a_v1",
		EvidenceRefs: []string{"finding:synth_a"},
		ExpiresAt:    now.Add(336 * time.Hour),
		Media:        media,
	}, now)
	if err != nil {
		t.Fatalf("record draft with media: %v", err)
	}
	return draft
}

// TestContentDraftCarriesContentAddressedMedia is the red-first falsifier for the
// seam that makes a distribution machine possible at all.
//
// Before this, a ContentDraft was text-only: product, platform, body, voice pack,
// evidence. Every image and video generator in the capability graph — chatgpt-image,
// hermes image_generate, the imagegen skills, OpenMontage — produced assets with
// nowhere to attach, so the pipeline ended at text by construction. It also made the
// image-first platforms unreachable regardless of any transport work.
//
// Media rides the SAME content-addressed artifact store the body already uses, so an
// identical asset generated twice dedups to one ref, and provenance (which adapter,
// which model/LoRA, which prompt) travels with the asset rather than being lost.
func TestContentDraftCarriesContentAddressedMedia(t *testing.T) {
	homePath := t.TempDir()
	pixels := []byte("\x89PNG\r\n\x1a\nsynthetic-image-bytes")
	draft := recordMediaDraft(t, homePath, []ContentDraftMediaInput{{
		Kind:     "image",
		Bytes:    pixels,
		MIMEType: "image/png",
		AltText:  "synthetic product screenshot",
		Adapter:  "adapter:local-sdxl",
		Model:    "sdxl-1.0+lora:product-style-v3",
		Prompt:   "a synthetic product screenshot, studio lighting",
	}}, draftMediaNow)

	if len(draft.Media) != 1 {
		t.Fatalf("draft media = %d, want 1: a draft must be able to carry a generated asset", len(draft.Media))
	}
	got := draft.Media[0]
	if got.Kind != "image" {
		t.Fatalf("media kind = %q, want image", got.Kind)
	}
	if strings.TrimSpace(got.Ref) == "" {
		t.Fatal("media ref is empty: the asset must be stored content-addressed like the body")
	}
	if got.Adapter != "adapter:local-sdxl" || got.Model != "sdxl-1.0+lora:product-style-v3" {
		t.Fatalf("media provenance lost: adapter=%q model=%q", got.Adapter, got.Model)
	}
	if strings.TrimSpace(got.PromptRef) == "" {
		t.Fatal("prompt ref is empty: the prompt that produced an asset is provenance, not decoration")
	}
	if got.AltText != "synthetic product screenshot" {
		t.Fatalf("alt text = %q, want it preserved", got.AltText)
	}

	// The asset must survive the round trip through the stored record.
	listed, err := ListContentDrafts(homePath, "product:prod_synth_a")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || len(listed[0].Media) != 1 || listed[0].Media[0].Ref != got.Ref {
		t.Fatalf("media did not survive the round trip: %+v", listed)
	}

	// Identical bytes dedup to one artifact, exactly as identical bodies already do.
	second := recordMediaDraft(t, homePath, []ContentDraftMediaInput{{
		Kind:     "image",
		Bytes:    pixels,
		MIMEType: "image/png",
		Adapter:  "adapter:local-sdxl",
		Prompt:   "a synthetic product screenshot, studio lighting",
	}}, draftMediaNow.Add(time.Second))
	if second.ID == draft.ID {
		t.Fatal("two records of the same asset must be distinct drafts")
	}
	if second.Media[0].Ref != got.Ref {
		t.Fatalf("identical asset bytes must dedup to one ref: %q vs %q", second.Media[0].Ref, got.Ref)
	}
}

// TestContentDraftRefusesUnknownMediaKind pins the closed allowlist. The platform
// allowlist already exists for the same reason: an unroutable junk value must be
// refused at the door, not discovered by a transport at post time.
func TestContentDraftRefusesUnknownMediaKind(t *testing.T) {
	homePath := t.TempDir()
	_, err := RecordContentDraft(homePath, ContentDraftInput{
		ProductID: "product:prod_synth_a",
		Platform:  "x",
		Body:      "body",
		ExpiresAt: draftMediaNow.Add(time.Hour),
		Media:     []ContentDraftMediaInput{{Kind: "hologram", Bytes: []byte("x")}},
	}, draftMediaNow)
	if err == nil {
		t.Fatal("unknown media kind must be refused at the door")
	}
	if !strings.Contains(err.Error(), "hologram") {
		t.Fatalf("refusal must name the offending kind, got: %v", err)
	}
}

// TestContentDraftMediaRequiresBytes pins that a media entry cannot be a promise.
// An entry with no bytes would mint a draft claiming an asset that does not exist —
// the same false-green shape as a "posted" flag with no remote id.
func TestContentDraftMediaRequiresBytes(t *testing.T) {
	homePath := t.TempDir()
	_, err := RecordContentDraft(homePath, ContentDraftInput{
		ProductID: "product:prod_synth_a",
		Platform:  "x",
		Body:      "body",
		ExpiresAt: draftMediaNow.Add(time.Hour),
		Media:     []ContentDraftMediaInput{{Kind: "image"}},
	}, draftMediaNow)
	if err == nil {
		t.Fatal("a media entry with no bytes must be refused: a draft must not claim an asset it does not hold")
	}
}

// TestContentDraftCannotPostBeforeOperationalized is the expensive-failure guard.
// Posting is an irreversible outward act. The lifecycle already requires an explicit
// operator verdict (operationalize) before a draft is usable; reaching "posted" from
// "proposed" would let unreviewed generated content reach the public.
func TestContentDraftCannotPostBeforeOperationalized(t *testing.T) {
	homePath := t.TempDir()
	draft := recordMediaDraft(t, homePath, nil, draftMediaNow)
	if draft.State != ContentDraftStateProposed {
		t.Fatalf("fixture state = %q, want proposed", draft.State)
	}
	_, err := MarkContentDraftPosted(homePath, draft.ID, ContentDraftPostInput{
		RemoteID: "remote-123",
		URL:      "https://example.invalid/p/remote-123",
	}, draftMediaNow)
	if err == nil {
		t.Fatal("a proposed draft must not be markable posted: unreviewed content would reach the public")
	}
	after, err := ReadContentDraft(homePath, draft.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.State != ContentDraftStateProposed {
		t.Fatalf("refused post rewrote state to %q; the record must be untouched", after.State)
	}
}

// TestContentDraftPostedRequiresRemoteID is the anti-false-green falsifier, and the
// whole reason this state exists.
//
// The weekly distribution driver reported `weekly_floor_met` every day while zero
// posts had ever been published, because the only question the data could answer was
// "was a draft written". A "posted" state that could be set without evidence from the
// platform would recreate exactly that lie one layer up. Posted means: a remote
// system accepted this and gave us an id.
func TestContentDraftPostedRequiresRemoteID(t *testing.T) {
	homePath := t.TempDir()
	draft := recordMediaDraft(t, homePath, nil, draftMediaNow)
	if _, err := OperationalizeContentDraft(homePath, draft.ID, draftMediaNow); err != nil {
		t.Fatalf("operationalize: %v", err)
	}
	if _, err := MarkContentDraftPosted(homePath, draft.ID, ContentDraftPostInput{URL: "https://example.invalid/p/x"}, draftMediaNow); err == nil {
		t.Fatal("posted without a platform remote id must be refused: that is a claim, not a fact")
	}
	after, err := ReadContentDraft(homePath, draft.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.State != ContentDraftStateOperationalized {
		t.Fatalf("refused post changed state to %q; it must remain operationalized", after.State)
	}

	posted, err := MarkContentDraftPosted(homePath, draft.ID, ContentDraftPostInput{
		RemoteID: "remote-123",
		URL:      "https://example.invalid/p/remote-123",
	}, draftMediaNow)
	if err != nil {
		t.Fatalf("posting an operationalized draft with a remote id must succeed: %v", err)
	}
	if posted.State != ContentDraftStatePosted {
		t.Fatalf("state = %q, want posted", posted.State)
	}
	if posted.Post == nil || posted.Post.RemoteID != "remote-123" {
		t.Fatalf("post record missing its remote id: %+v", posted.Post)
	}
	if posted.Post.PostedAt == "" {
		t.Fatal("post record must carry when it went out")
	}
	if posted.Post.Platform != "x" {
		t.Fatalf("post record platform = %q, want the draft's platform", posted.Post.Platform)
	}
}

// TestOperationalizedIsNotPosted pins the distinction the whole slice exists to make.
// "Approved for use" and "a human saw it" are different facts, and a measure built on
// the first one can report success forever while nothing ships.
func TestOperationalizedIsNotPosted(t *testing.T) {
	homePath := t.TempDir()
	approved := recordMediaDraft(t, homePath, nil, draftMediaNow)
	if _, err := OperationalizeContentDraft(homePath, approved.ID, draftMediaNow); err != nil {
		t.Fatalf("operationalize: %v", err)
	}
	shipped := recordMediaDraft(t, homePath, nil, draftMediaNow.Add(time.Second))
	if _, err := OperationalizeContentDraft(homePath, shipped.ID, draftMediaNow); err != nil {
		t.Fatalf("operationalize: %v", err)
	}
	if _, err := MarkContentDraftPosted(homePath, shipped.ID, ContentDraftPostInput{RemoteID: "r-1"}, draftMediaNow); err != nil {
		t.Fatalf("mark posted: %v", err)
	}

	listed, err := ListContentDrafts(homePath, "product:prod_synth_a")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var operationalized, posted int
	for _, d := range listed {
		switch d.State {
		case ContentDraftStateOperationalized:
			operationalized++
		case ContentDraftStatePosted:
			posted++
		}
	}
	if operationalized != 1 || posted != 1 {
		t.Fatalf("operationalized=%d posted=%d, want 1 and 1: the two facts must be separately countable", operationalized, posted)
	}
}

// TestContentDraftPostedIsTerminal pins that a published post cannot be quietly
// re-adjudicated. Once it is public, rejecting or re-approving the record would make
// the ledger disagree with the world.
func TestContentDraftPostedIsTerminal(t *testing.T) {
	homePath := t.TempDir()
	draft := recordMediaDraft(t, homePath, nil, draftMediaNow)
	if _, err := OperationalizeContentDraft(homePath, draft.ID, draftMediaNow); err != nil {
		t.Fatalf("operationalize: %v", err)
	}
	if _, err := MarkContentDraftPosted(homePath, draft.ID, ContentDraftPostInput{RemoteID: "r-1"}, draftMediaNow); err != nil {
		t.Fatalf("mark posted: %v", err)
	}
	if _, err := RejectContentDraft(homePath, draft.ID, draftMediaNow); err == nil {
		t.Fatal("rejecting an already-posted draft must be refused: the post is already public")
	}
	if _, err := MarkContentDraftPosted(homePath, draft.ID, ContentDraftPostInput{RemoteID: "r-2"}, draftMediaNow); err == nil {
		t.Fatal("posting an already-posted draft must be refused: it would hide the first remote id")
	}
}
