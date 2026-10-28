package candidates

import "testing"

// Every kind maps to exactly one non-empty inbox dir, and no two kinds share a dir — this is the
// invariant whose absence WAS the "three folder names for one concept" bug.
func TestEveryKindHasUniqueNonEmptyDir(t *testing.T) {
	seen := map[string]Kind{}
	for _, k := range AllKinds() {
		dir := InboxDir(k)
		if dir == "" {
			t.Errorf("kind %q has no inbox dir", k)
		}
		if prev, dup := seen[dir]; dup {
			t.Errorf("kinds %q and %q collide on dir %q", prev, k, dir)
		}
		seen[dir] = k
	}
	if len(AllInboxDirs()) != len(AllKinds()) {
		t.Errorf("AllInboxDirs (%d) and AllKinds (%d) must be 1:1", len(AllInboxDirs()), len(AllKinds()))
	}
}

// The anti-drift contract: the distiller WRITES capability observations and capgraph READS them
// via this same call, so a single source decides the dir — they can never disagree again.
func TestCapabilityDirIsSingleSourced(t *testing.T) {
	if got := InboxDir(CapabilityObs); got != "inbox/capability-observations" {
		t.Errorf("capability_observation dir drifted to %q", got)
	}
	// parked memory must land where the P6 adjudicator reads (the old bug parked it in inbox/memory).
	if got := InboxDir(Memory); got != "inbox/memory-candidates" {
		t.Errorf("memory candidate dir = %q, want inbox/memory-candidates (the adjudicator's inbox)", got)
	}
}

// ContentDraft is the distribution-loop observed candidate. Pin its home and the
// verdicts its door may apply. RED if someone renames the inbox (breaking the
// product content-draft door + the auto-registered habitat entry) or opens the
// verdict set to Promote (a draft must be operationalized/parked/rejected, never
// promoted to canon truth).
func TestContentDraftDirIsSingleSourced(t *testing.T) {
	spec, ok := SpecFor(ContentDraft)
	if !ok {
		t.Fatal("content_draft spec missing")
	}
	if spec.InboxDir != "inbox/content-drafts" {
		t.Fatalf("content_draft dir = %q, want inbox/content-drafts", spec.InboxDir)
	}
	if spec.Consumer != "compound product content-draft" {
		t.Fatalf("content_draft consumer = %q, want the product content-draft door", spec.Consumer)
	}
	want := map[string]bool{Park: true, Operationalize: true, Reject: true}
	if len(spec.Verdicts) != len(want) {
		t.Fatalf("content_draft verdicts = %v, want park/operationalize/reject", spec.Verdicts)
	}
	for _, v := range spec.Verdicts {
		if !want[v] {
			t.Fatalf("content_draft verdict %q not allowed (must be park/operationalize/reject)", v)
		}
	}
}

func TestEvalCaseConsumerIsReviewedPlan(t *testing.T) {
	spec, ok := SpecFor(EvalCase)
	if !ok {
		t.Fatal("eval_case spec missing")
	}
	if spec.Consumer != "compound distiller eval-plan" {
		t.Fatalf("eval_case consumer = %q, want reviewed eval-plan surface", spec.Consumer)
	}
}

// An unknown / path-bearing kind has no dir — a Kind keys a filesystem path, so this is the
// path-traversal guard the producers rely on.
func TestRelationIsStructuralNotCandidateKind(t *testing.T) {
	if InboxDir(Kind("relation")) != "" || Known(Kind("relation")) {
		t.Fatal("relation is structural memory edge data; it must not create a candidate inbox")
	}
}

func TestUnknownKindHasNoDir(t *testing.T) {
	for _, k := range []Kind{"bogus", "../../evil", ""} {
		if InboxDir(k) != "" || Known(k) {
			t.Errorf("unknown kind %q must have no dir and not be Known", k)
		}
	}
}

// F3 depth probe (packet work_709c9ba49c56): the MemoryCandidate kind stays
// the canonical home for the inbox path used by the candidate projection the
// console reads. The packet's "recurrence_count + confidence_trend" derived
// fields are computed at projection time by internal/mem.ListCandidates; this
// test pins the seam so the projection emit site can never silently key off
// a different inbox dir (which would orphan the standing-law-candidate chip
// from the candidate set it is supposed to summarise).
func TestMemoryCandidateKindIsCanonicalProjectionSource(t *testing.T) {
	spec, ok := SpecFor(Memory)
	if !ok {
		t.Fatal("memory candidate spec missing")
	}
	if spec.InboxDir != "inbox/memory-candidates" {
		t.Fatalf("memory inbox drifted: %q", spec.InboxDir)
	}
	if spec.Consumer != "compound memory adjudicate" {
		t.Fatalf("memory consumer drifted: %q", spec.Consumer)
	}
}
