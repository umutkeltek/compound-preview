package work

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/home"
)

// proofContainedBaseSpec builds the reproduced integration obstruction as one
// linear chain: the proof base P, the member's contained creation base M and
// the reviewed combined candidate H.
type proofContainedBaseSpec struct {
	baseFiles   map[string]string
	memberFiles map[string]string
	headFiles   map[string]string
	// memberBaseAhead creates the member base after the reviewed head, so the
	// contained base can never reach H.
	memberBaseAhead bool
	// memberBaseSibling creates the member base on a sibling history that
	// shares the repository lineage but is incomparable with the proof base.
	memberBaseSibling bool
	// memberBaseMissing points the member at an absent object.
	memberBaseMissing bool
	// memberBaseInvalid points the member at a malformed object id.
	memberBaseInvalid bool
}

func commitLandingFiles(t *testing.T, repo string, files map[string]string, message string) {
	t.Helper()
	if len(files) == 0 {
		return
	}
	for name, body := range files {
		writeLandingFile(t, filepath.Join(repo, name), body)
	}
	landingGit(t, repo, "add", "-A")
	landingGit(t, repo, "commit", "-m", message)
}

// buildProofAdoptionContainedBase creates the source work at the proof base and
// the member work at the contained base, then tries to custody the reviewed
// membership. A non-nil error is the membership refusal under test; a nil
// error returns the fixture with one completed canonical proof.
func buildProofAdoptionContainedBase(t *testing.T, spec proofContainedBaseSpec) (proofAdoptionFixture, ProofMemberManifest, error) {
	t.Helper()
	repo, _, _ := sharedLandingReadyRepo(t)
	forkBase := landingGitOutput(t, repo, "rev-parse", "HEAD")
	if spec.memberBaseSibling {
		commitLandingFiles(t, repo, map[string]string{"proof-base.txt": "distinct proof base\n"}, "proof base diverges")
	}
	commitLandingFiles(t, repo, spec.baseFiles, "proof base")
	base := landingGitOutput(t, repo, "rev-parse", "HEAD")
	landingGit(t, repo, "switch", "-c", "combined")
	commitLandingFiles(t, repo, spec.memberFiles, "member base")
	memberBase := landingGitOutput(t, repo, "rev-parse", "HEAD")
	commitLandingFiles(t, repo, spec.headFiles, "combined candidate")
	head := landingGitOutput(t, repo, "rev-parse", "HEAD")
	if spec.memberBaseAhead {
		commitLandingFiles(t, repo, map[string]string{"ahead.txt": "ahead\n"}, "after the reviewed head")
		memberBase = landingGitOutput(t, repo, "rev-parse", "HEAD")
	}
	if spec.memberBaseSibling {
		landingGit(t, repo, "switch", "-c", "sibling", forkBase)
		commitLandingFiles(t, repo, map[string]string{"sibling.txt": "sibling\n"}, "sibling history")
		memberBase = landingGitOutput(t, repo, "rev-parse", "HEAD")
		landingGit(t, repo, "switch", "combined")
	}
	landingGit(t, repo, "switch", "main")

	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	s, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ids := []string{}
	for _, member := range []struct{ name, creationBase string }{
		{"integration", base},
		{"member", memberBase},
	} {
		w, err := s.Create(CreateInput{Owner: "codex", Actor: "codex", SuccessPredicate: member.name + " acceptance remains independently required", GitDelivery: localGitDelivery(repo, member.creationBase)}, now)
		if err != nil {
			t.Fatal(err)
		}
		w, err = s.Claim(w.ID, "codex", now)
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range []string{"running", "verifying"} {
			w, err = s.Advance(w.ID, AdvanceInput{To: state, Actor: "codex"}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, w.ID)
	}

	// Original candidates and explicit acceptance of the same combined head.
	attachLandingCommit(t, s, ids[0], base, time.Now())
	attachLandingCommit(t, s, ids[0], head, time.Now())
	attachLandingCommit(t, s, ids[1], memberBase, time.Now())
	attachLandingCommit(t, s, ids[1], head, time.Now())
	source, err := s.Get(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reviewRef, err := artifacts.Put([]byte("independent synthetic review of " + strings.Join(ids, ",") + " at " + head))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		w, _ := s.Get(id)
		if _, err = s.Advance(id, AdvanceInput{To: w.State, Actor: "codex", ArtifactID: reviewRef}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	m := ProofMemberManifest{SchemaVersion: ProofMemberManifestSchema, SourceWorkID: ids[0],
		RepositoryFingerprint: source.GitDelivery.RepositoryFingerprint, BaseSHA: base, HeadSHA: head, TargetLocalRef: "refs/heads/main",
		Members: []ProofReviewedMember{
			{WorkID: ids[0], OriginalCandidateRef: "commit:" + base, ReviewRefs: []string{reviewRef}},
			{WorkID: ids[1], OriginalCandidateRef: "commit:" + memberBase, ReviewRefs: []string{reviewRef}},
		},
	}
	if spec.memberBaseMissing || spec.memberBaseInvalid {
		w, err := s.Get(ids[1])
		if err != nil {
			t.Fatal(err)
		}
		if spec.memberBaseInvalid {
			w.GitDelivery.ExpectedBaseSHA = "not-an-object-id"
		} else {
			w.GitDelivery.ExpectedBaseSHA = strings.Repeat("0", 40)
		}
		if err := s.writeJSON(s.workPath(w.ID), &w); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotCloseAuthorityFiles(t, s.home)
	recorded, err := s.RecordProofManifest(ids[0], "codex", m, time.Now())
	if err != nil {
		if !reflect.DeepEqual(before, snapshotCloseAuthorityFiles(t, s.home)) {
			t.Fatal("refused membership changed durable authority or custody")
		}
		return proofAdoptionFixture{store: s, source: ids[0], destination: ids[1], repo: repo, base: base, head: head, now: now}, m, err
	}
	completeSourceBoundProofForDetachedCandidate(t, s, ids[0], "codex", repo, head, time.Now().Add(time.Second))
	source, err = s.Get(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	result, found, err := s.latestSourceBoundProofResult(source)
	if err != nil || !found {
		t.Fatalf("source proof: %v", err)
	}
	canonical, err := s.loadProofManifest(recorded.ManifestRef)
	if err != nil {
		t.Fatal(err)
	}
	return proofAdoptionFixture{s, ids[0], ids[1], repo, base, head, canonical, recorded.ManifestRef, result, now}, m, nil
}

// TestProofAdoptionContainedBaseSharesOneCompleteProof covers P<M<H with a
// clean member diff: one original proof, an independent member adoption, and
// independent landing and closure for both works.
func TestProofAdoptionContainedBaseSharesOneCompleteProof(t *testing.T) {
	f, _, err := buildProofAdoptionContainedBase(t, proofContainedBaseSpec{
		memberFiles: map[string]string{"README.md": "member\n"},
		headFiles:   map[string]string{"candidate.txt": "candidate\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	source, _ := f.store.Get(f.source)
	destination, _ := f.store.Get(f.destination)
	if source.GitDelivery.ExpectedBaseSHA == destination.GitDelivery.ExpectedBaseSHA {
		t.Fatal("fixture did not preserve the contained creation base")
	}
	adopted := f.adopt(t)
	beforeAttempts := append([]string(nil), adopted.RunAttemptIDs...)
	for _, id := range []string{f.source, f.destination} {
		report, err := f.store.LandCheck(id, LandCheckOptions{CandidateTipSHA: f.head, Actor: "codex", Record: true}, time.Now())
		if err != nil || report.Status != "ready_to_land" {
			t.Fatalf("ready %s: %+v %v", id, report, err)
		}
	}
	landingGit(t, f.repo, "merge", "--ff-only", f.head)
	for _, id := range []string{f.source, f.destination} {
		report, err := f.store.LandCheck(id, LandCheckOptions{CandidateTipSHA: f.head, Actor: "codex", Record: true}, time.Now())
		if err != nil || report.Status != "landed" {
			t.Fatalf("landed %s: %+v %v", id, report, err)
		}
		if _, err = f.store.Close(id, CloseInput{Outcome: "completed", MoveType: "control_plane_repair", Actor: "codex", Summary: "independent synthetic acceptance", Changed: true,
			ProofRefs: []string{"commit:" + f.head, "check:synthetic-acceptance", "live:synthetic-consumer", report.EvidenceRef}}, time.Now()); err != nil {
			t.Fatalf("close %s: %v", id, err)
		}
	}
	closed, err := f.store.Get(f.destination)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeAttempts, closed.RunAttemptIDs) {
		t.Fatal("adoption/land/close created or attached execution")
	}
}

// TestProofAdoptionContainedBaseRejectsUncoveredMemberDelivery is the reverted
// file counterexample: the member's own change is invisible to the proof range,
// so the one proof never scanned the body the member delivered.
func TestProofAdoptionContainedBaseRejectsUncoveredMemberDelivery(t *testing.T) {
	f, _, err := buildProofAdoptionContainedBase(t, proofContainedBaseSpec{
		memberFiles: map[string]string{"README.md": "member\n"},
		headFiles:   map[string]string{"README.md": "base\n"},
	})
	if err == nil || !strings.Contains(err.Error(), "not covered by the reviewed proof inventory") {
		t.Fatalf("reverted member delivery = %v, want inventory coverage refusal", err)
	}
	assertProofContainmentRefusedWithoutRecords(t, f)
}

// TestProofAdoptionContainedBaseRejectsMemberWhitespaceInvisibleToProofRange
// pins the independent member range check: the proof range cannot see the
// whitespace error, so the member range must refuse it alone.
func TestProofAdoptionContainedBaseRejectsMemberWhitespaceInvisibleToProofRange(t *testing.T) {
	f, _, err := buildProofAdoptionContainedBase(t, proofContainedBaseSpec{
		baseFiles:   map[string]string{"ws.txt": "ws \nold\n"},
		memberFiles: map[string]string{"ws.txt": "mid\n"},
		headFiles:   map[string]string{"ws.txt": "ws \nnew\n"},
	})
	if err == nil || !strings.Contains(err.Error(), "member range diff-check failed") {
		t.Fatalf("member whitespace = %v, want independent member diff-check refusal", err)
	}
	assertProofContainmentRefusedWithoutRecords(t, f)
}

// TestProofAdoptionContainedBaseFailsClosedOnUnusableLineage covers an
// incomparable history, a base that never reaches the reviewed head, an
// absent object and an invalid object id.
func TestProofAdoptionContainedBaseFailsClosedOnUnusableLineage(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec proofContainedBaseSpec
		want string
	}{
		{"incomparable", proofContainedBaseSpec{memberBaseSibling: true}, "outside reviewed base lineage"},
		{"off-head", proofContainedBaseSpec{memberBaseAhead: true}, "does not reach the reviewed head"},
		{"missing-object", proofContainedBaseSpec{memberBaseMissing: true}, "outside reviewed base lineage"},
		{"invalid-object", proofContainedBaseSpec{memberBaseInvalid: true}, "outside reviewed base lineage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, err := buildProofAdoptionContainedBase(t, tc.spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s admission = %v, want %q refusal", tc.name, err, tc.want)
			}
			assertProofContainmentRefusedWithoutRecords(t, f)
		})
	}
}

// TestProofAdoptionContainedBaseRetainsRefusalControls proves the contained
// branch never weakens the existing changed-contract, missing-review,
// different-head or late-membership controls.
func TestProofAdoptionContainedBaseRetainsRefusalControls(t *testing.T) {
	t.Run("different-head", func(t *testing.T) {
		f, _, err := buildProofAdoptionContainedBase(t, proofContainedBaseSpec{
			memberFiles: map[string]string{"README.md": "member\n"},
			headFiles:   map[string]string{"candidate.txt": "candidate\n"},
		})
		if err != nil {
			t.Fatal(err)
		}
		attachLandingCommit(t, f.store, f.destination, f.base, time.Now())
		if _, err := f.store.AdoptProof(f.destination, ProofAdoptionInput{Actor: "codex", ProofResultID: f.result.ID, ManifestRef: f.manifestRef}, time.Now()); err == nil {
			t.Fatal("member with a different accepted candidate adopted")
		}
	})
	t.Run("changed-contract", func(t *testing.T) {
		f, _, err := buildProofAdoptionContainedBase(t, proofContainedBaseSpec{
			memberFiles: map[string]string{"README.md": "member\n"},
			headFiles:   map[string]string{"candidate.txt": "candidate\n"},
		})
		if err != nil {
			t.Fatal(err)
		}
		w, _ := f.store.Get(f.destination)
		w.SuccessPredicate = "changed acceptance"
		if err := f.store.writeJSON(f.store.workPath(w.ID), &w); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.AdoptProof(f.destination, ProofAdoptionInput{Actor: "codex", ProofResultID: f.result.ID, ManifestRef: f.manifestRef}, time.Now()); err == nil {
			t.Fatal("changed member contract adopted")
		}
	})
	t.Run("missing-review", func(t *testing.T) {
		f, _, err := buildProofAdoptionContainedBase(t, proofContainedBaseSpec{
			memberFiles: map[string]string{"README.md": "member\n"},
			headFiles:   map[string]string{"candidate.txt": "candidate\n"},
		})
		if err != nil {
			t.Fatal(err)
		}
		ref := f.manifest.Members[1].ReviewRefs[0]
		digest, _ := home.ArtifactHex(ref)
		if err := os.Remove(filepath.Join(f.store.home, "artifacts", "sha256", digest)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.AdoptProof(f.destination, ProofAdoptionInput{Actor: "codex", ProofResultID: f.result.ID, ManifestRef: f.manifestRef}, time.Now()); err == nil {
			t.Fatal("member with missing review evidence adopted")
		}
	})
	t.Run("late-membership", func(t *testing.T) {
		f, _, err := buildProofAdoptionContainedBase(t, proofContainedBaseSpec{
			memberFiles: map[string]string{"README.md": "member\n"},
			headFiles:   map[string]string{"candidate.txt": "candidate\n"},
		})
		if err != nil {
			t.Fatal(err)
		}
		artifacts, _ := home.NewArtifactStore(f.store.home)
		later, err := artifacts.Put([]byte("later independent review " + f.source + " " + f.destination))
		if err != nil {
			t.Fatal(err)
		}
		laterManifest := f.manifest
		laterManifest.Members = append([]ProofReviewedMember(nil), f.manifest.Members...)
		for i := range laterManifest.Members {
			laterManifest.Members[i].ReviewRefs = []string{later}
			w, _ := f.store.Get(laterManifest.Members[i].WorkID)
			if _, err = f.store.Advance(w.ID, AdvanceInput{To: w.State, Actor: "codex", ArtifactID: later}, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		recorded, err := f.store.RecordProofManifest(f.source, "codex", laterManifest, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.AdoptProof(f.destination, ProofAdoptionInput{Actor: "codex", ProofResultID: f.result.ID, ManifestRef: recorded.ManifestRef}, time.Now()); err == nil ||
			!strings.Contains(err.Error(), "before original proof reservation") {
			t.Fatalf("late membership = %v, want ordering refusal", err)
		}
	})
}

// assertProofContainmentRefusedWithoutRecords proves a refused membership left
// no custody, proof or execution record behind.
func assertProofContainmentRefusedWithoutRecords(t *testing.T, f proofAdoptionFixture) {
	t.Helper()
	for _, id := range []string{f.source, f.destination} {
		w, err := f.store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if w.State != "verifying" {
			t.Fatalf("work %s state = %q, want unchanged verifying", id, w.State)
		}
		for _, ref := range w.ArtifactIDs {
			if strings.HasPrefix(ref, ProofManifestArtifactPrefix) {
				t.Fatalf("work %s custodied a refused membership %s", id, ref)
			}
		}
		if len(w.VerificationIDs) != 0 {
			t.Fatalf("work %s attached a proof result: %v", id, w.VerificationIDs)
		}
	}
}

// TestProofAdoptionContainedBaseDiamondTargetInsideCertifiedHead covers the
// real batch shape: the coverage base B branches to the member M and to the
// landing target T, both merge into the reviewed candidate H, and main already
// sits at T when the one source proof runs. The foreign-main path is invisible
// in T...H but covered by B...H and M...H, so a proof based at the coverage base
// B certifies the whole member delivery while the landing still fast-forwards
// from T. The proof report stays exact B/H: the target tip never relabels it.
func TestProofAdoptionContainedBaseDiamondTargetInsideCertifiedHead(t *testing.T) {
	repo, base := initLandingRepo(t)
	// B -> M: the member's own delivery.
	landingGit(t, repo, "switch", "-c", "member")
	commitLandingFiles(t, repo, map[string]string{"member.txt": "member\n"}, "member delivery")
	memberBase := landingGitOutput(t, repo, "rev-parse", "HEAD")
	// B -> T: the sibling history that later became the landing target.
	landingGit(t, repo, "switch", "-c", "target", base)
	commitLandingFiles(t, repo, map[string]string{"foreign-main.txt": "foreign\n"}, "target delivery")
	target := landingGitOutput(t, repo, "rev-parse", "HEAD")
	// H merges both siblings; main stays at T until the guarded ff.
	landingGit(t, repo, "merge", "--no-ff", "-m", "combined candidate", "member")
	head := landingGitOutput(t, repo, "rev-parse", "HEAD")
	landingGit(t, repo, "switch", "main")
	landingGit(t, repo, "reset", "--hard", target)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if target == base {
		t.Fatal("target tip equals the coverage base")
	}
	for _, pair := range []struct{ ancestor, descendant string }{
		{base, target}, {target, head}, {base, memberBase}, {memberBase, head},
	} {
		ancestor, err := gitrepository.CommittedAncestor(ctx, repo, pair.ancestor, pair.descendant)
		if err != nil || !ancestor {
			t.Fatalf("ancestry %s<=%s = %v %v", pair.ancestor, pair.descendant, ancestor, err)
		}
	}
	// The foreign-main path is exactly what separates the coverage base from the
	// landing target: T...H never scans it, B...H and M...H both do.
	proofPaths, err := gitrepository.CommittedContentPaths(ctx, repo, base, head)
	if err != nil {
		t.Fatal(err)
	}
	memberPaths, err := gitrepository.CommittedContentPaths(ctx, repo, memberBase, head)
	if err != nil {
		t.Fatal(err)
	}
	targetPaths, err := gitrepository.CommittedContentPaths(ctx, repo, target, head)
	if err != nil {
		t.Fatal(err)
	}
	if !oneOf("foreign-main.txt", proofPaths) || !oneOf("foreign-main.txt", memberPaths) {
		t.Fatalf("coverage base did not cover the foreign-main path: %v %v", proofPaths, memberPaths)
	}
	if oneOf("foreign-main.txt", targetPaths) {
		t.Fatalf("target range unexpectedly covered the foreign-main path: %v", targetPaths)
	}

	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	s, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ids := []string{}
	for _, member := range []struct{ name, creationBase string }{
		{"integration", base},
		{"member", memberBase},
	} {
		w, err := s.Create(CreateInput{Owner: "codex", Actor: "codex", SuccessPredicate: member.name + " acceptance remains independently required", GitDelivery: localGitDelivery(repo, member.creationBase)}, now)
		if err != nil {
			t.Fatal(err)
		}
		w, err = s.Claim(w.ID, "codex", now)
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range []string{"running", "verifying"} {
			w, err = s.Advance(w.ID, AdvanceInput{To: state, Actor: "codex"}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, w.ID)
	}
	// Original candidates and explicit acceptance of the same combined head.
	attachLandingCommit(t, s, ids[0], base, time.Now())
	attachLandingCommit(t, s, ids[0], head, time.Now())
	attachLandingCommit(t, s, ids[1], memberBase, time.Now())
	attachLandingCommit(t, s, ids[1], head, time.Now())
	source, err := s.Get(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reviewRef, err := artifacts.Put([]byte("independent synthetic review of " + strings.Join(ids, ",") + " at " + head))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		w, _ := s.Get(id)
		if _, err = s.Advance(id, AdvanceInput{To: w.State, Actor: "codex", ArtifactID: reviewRef}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	m := ProofMemberManifest{SchemaVersion: ProofMemberManifestSchema, SourceWorkID: ids[0],
		RepositoryFingerprint: source.GitDelivery.RepositoryFingerprint, BaseSHA: base, HeadSHA: head, TargetLocalRef: "refs/heads/main",
		Members: []ProofReviewedMember{
			{WorkID: ids[0], OriginalCandidateRef: "commit:" + base, ReviewRefs: []string{reviewRef}},
			{WorkID: ids[1], OriginalCandidateRef: "commit:" + memberBase, ReviewRefs: []string{reviewRef}},
		},
	}
	recorded, err := s.RecordProofManifest(ids[0], "codex", m, time.Now())
	if err != nil {
		t.Fatalf("diamond membership refused: %v", err)
	}
	// One source proof, based at the coverage base B, candidate H.
	completeSourceBoundProofForDetachedCandidate(t, s, ids[0], "codex", repo, head, time.Now().Add(time.Second))
	source, err = s.Get(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	result, found, err := s.latestSourceBoundProofResult(source)
	if err != nil || !found {
		t.Fatalf("source proof: %v", err)
	}
	if result.SourceSnapshot == nil || result.SourceSnapshot.BaseSHA != base || result.SourceSnapshot.HeadSHA != head {
		t.Fatalf("proof report is not exact B/H: %+v", result.SourceSnapshot)
	}
	adopted, err := s.AdoptProof(ids[1], ProofAdoptionInput{Actor: "codex", ProofResultID: result.ID, ManifestRef: recorded.ManifestRef}, time.Now())
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	beforeAttempts := append([]string(nil), adopted.RunAttemptIDs...)
	if result.RunAttemptID != "" && oneOf(result.RunAttemptID, adopted.RunAttemptIDs) {
		t.Fatal("foreign source attempt attached to the member")
	}
	for _, id := range []string{ids[0], ids[1]} {
		report, err := s.LandCheck(id, LandCheckOptions{CandidateTipSHA: head, Actor: "codex", Record: true}, time.Now())
		if err != nil || report.Status != "ready_to_land" {
			t.Fatalf("ready %s: %+v %v", id, report, err)
		}
		if report.ProofHeadSHA != head {
			t.Fatalf("land report proof tip drifted for %s: %+v", id, report)
		}
	}
	landingGit(t, repo, "merge", "--ff-only", head)
	for _, id := range []string{ids[0], ids[1]} {
		report, err := s.LandCheck(id, LandCheckOptions{CandidateTipSHA: head, Actor: "codex", Record: true}, time.Now())
		if err != nil || report.Status != "landed" {
			t.Fatalf("landed %s: %+v %v", id, report, err)
		}
		if report.ProofHeadSHA != head {
			t.Fatalf("landed report proof tip drifted for %s: %+v", id, report)
		}
		// The source closes FIRST; the destination then closes on the same proof.
		if _, err = s.Close(id, CloseInput{Outcome: "completed", MoveType: "control_plane_repair", Actor: "codex", Summary: "independent synthetic acceptance", Changed: true,
			ProofRefs: []string{"commit:" + head, "check:synthetic-acceptance", "live:synthetic-consumer", report.EvidenceRef}}, time.Now()); err != nil {
			t.Fatalf("close %s: %v", id, err)
		}
	}
	closedSource, err := s.Get(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if closedSource.State != "completed" {
		t.Fatalf("source state = %q, want completed", closedSource.State)
	}
	ref, proof, found, err := s.latestLandingProofResolution(closedSource)
	if err != nil || !found {
		t.Fatalf("proof resolution after close: %v", err)
	}
	if !strings.HasPrefix(ref, "proof-check:") {
		t.Fatalf("source proof was relabelled: %s", ref)
	}
	if proof.ID != result.ID || proof.RunAttemptID != result.RunAttemptID {
		t.Fatal("landing or closure replaced the original source proof")
	}
	if proof.SourceSnapshot == nil || proof.SourceSnapshot.BaseSHA != base || proof.SourceSnapshot.HeadSHA != head {
		t.Fatalf("closed proof report is not exact B/H: %+v", proof.SourceSnapshot)
	}
	closedMember, err := s.Get(ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if closedMember.State != "completed" {
		t.Fatalf("member state = %q, want completed", closedMember.State)
	}
	if !reflect.DeepEqual(beforeAttempts, closedMember.RunAttemptIDs) {
		t.Fatal("adoption/land/close created or attached execution")
	}
}
