package product

import (
	"strings"
	"testing"
	"time"
)

var platformNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func recordOnPlatform(t *testing.T, homePath, platform string) (ContentDraft, error) {
	t.Helper()
	return RecordContentDraft(homePath, ContentDraftInput{
		ProductID: "product:prod_synth_a",
		Platform:  platform,
		Body:      "synthetic body for platform falsifiers",
		ExpiresAt: platformNow.Add(336 * time.Hour),
	}, platformNow)
}

// TestContentDraftAcceptsEveryOperatorApprovedPlatform pins the set the operator
// actually distributes on. The draft door was born with x/bluesky/threads, which
// made the image-first platforms unreachable no matter what the media seam or any
// transport could do.
//
// TikTok is in the set even though its developer terms refuse a third-party upload
// tool for accounts you manage: a draft is written, approved, posted BY HAND, and
// then marked posted with the real post id. The lifecycle is the same; only the
// transport differs. Excluding it would lose the record of a post that happened.
func TestContentDraftAcceptsEveryOperatorApprovedPlatform(t *testing.T) {
	for _, platform := range []string{"x", "bluesky", "threads", "instagram", "tiktok", "facebook"} {
		t.Run(platform, func(t *testing.T) {
			homePath := t.TempDir()
			draft, err := recordOnPlatform(t, homePath, platform)
			if err != nil {
				t.Fatalf("platform %q must be accepted: %v", platform, err)
			}
			if draft.Platform != platform {
				t.Fatalf("stored platform = %q, want %q", draft.Platform, platform)
			}
		})
	}
}

// TestContentDraftNormalizesTwitterToX is the one-meaning-one-name falsifier.
//
// The operator asked for "twitter" alongside "x". They are the same platform, and
// admitting both as distinct values would let two drafts for one destination be
// stored under two names — splitting any per-platform count, any account lookup,
// and any transport route in half. The alias is accepted at the door (nobody should
// have to remember which name this system prefers) and stored canonically.
func TestContentDraftNormalizesTwitterToX(t *testing.T) {
	homePath := t.TempDir()
	draft, err := recordOnPlatform(t, homePath, "Twitter")
	if err != nil {
		t.Fatalf("twitter must be accepted as an alias: %v", err)
	}
	if draft.Platform != "x" {
		t.Fatalf("stored platform = %q, want canonical \"x\": one destination must never have two stored names", draft.Platform)
	}
	if _, ok := contentDraftPlatforms["twitter"]; ok {
		t.Fatal("twitter must be an alias, not a member of the canonical set")
	}
}

// TestContentDraftStillRefusesUnknownPlatform keeps the allowlist closed. Widening
// the set must not turn it into a free-form string field a transport cannot route.
func TestContentDraftStillRefusesUnknownPlatform(t *testing.T) {
	homePath := t.TempDir()
	_, err := recordOnPlatform(t, homePath, "myspace")
	if err == nil {
		t.Fatal("an unknown platform must still be refused")
	}
	if !strings.Contains(err.Error(), "myspace") {
		t.Fatalf("refusal must name the offending platform, got: %v", err)
	}
}

// TestPlatformRefusalListsTheLiveAllowlist is the anti-drift falsifier.
//
// Before this change the platform names existed in THREE places: the map, the
// refusal message ("(x|bluesky|threads)"), and the CLI usage string. Two of them
// were literals, so this very edit would have shipped a set of six platforms while
// still telling the operator there were three. The message must be derived from the
// map, so a future platform edit cannot leave a stale list behind.
func TestPlatformRefusalListsTheLiveAllowlist(t *testing.T) {
	homePath := t.TempDir()
	_, err := recordOnPlatform(t, homePath, "myspace")
	if err == nil {
		t.Fatal("want refusal")
	}
	for platform := range contentDraftPlatforms {
		if !strings.Contains(err.Error(), platform) {
			t.Fatalf("refusal must list live platform %q (derived from the allowlist, not restated), got: %v", platform, err)
		}
	}
	if got := ContentDraftPlatforms(); len(got) != len(contentDraftPlatforms) {
		t.Fatalf("ContentDraftPlatforms() returned %d names for %d platforms", len(got), len(contentDraftPlatforms))
	}
	// Sorted, so the usage string and refusal never reorder between builds.
	for i := 1; i < len(ContentDraftPlatforms()); i++ {
		if ContentDraftPlatforms()[i-1] >= ContentDraftPlatforms()[i] {
			t.Fatalf("ContentDraftPlatforms() must be sorted for stable output: %v", ContentDraftPlatforms())
		}
	}
}
