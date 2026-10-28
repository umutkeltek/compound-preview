package work

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
)

type proofAdoptionFixture struct {
	store               *Store
	source, destination string
	repo, base, head    string
	manifest            ProofMemberManifest
	manifestRef         string
	result              ProofResourceResult
	now                 time.Time
}

func newProofAdoptionFixture(t *testing.T) proofAdoptionFixture {
	return newProofAdoptionFixtureWithFreshBase(t, false)
}
func newProofAdoptionFixtureWithFreshBase(t *testing.T, freshBase bool) proofAdoptionFixture {
	t.Helper()
	repo, base, head := sharedLandingReadyRepo(t)
	originalBase := base
	if freshBase {
		landingGit(t, repo, "merge", "--ff-only", head)
		base = head
		landingGit(t, repo, "checkout", "--detach", base)
		landingGit(t, repo, "commit", "--allow-empty", "-m", "reviewed combined candidate")
		head = landingGitOutput(t, repo, "rev-parse", "HEAD")
		landingGit(t, repo, "switch", "main")
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
	for _, name := range []string{"integration", "member"} {
		creationBase := base
		if freshBase && name == "member" {
			creationBase = originalBase
		}
		w, err := s.Create(CreateInput{Owner: "codex", Actor: "codex", SuccessPredicate: name + " acceptance remains independently required", GitDelivery: localGitDelivery(repo, creationBase)}, now)
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
		attachLandingCommit(t, s, w.ID, base, time.Now())
		attachLandingCommit(t, s, w.ID, head, time.Now())
		ids = append(ids, w.ID)
	}
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
		Members: []ProofReviewedMember{{WorkID: ids[0], OriginalCandidateRef: "commit:" + base, ReviewRefs: []string{reviewRef}}, {WorkID: ids[1], OriginalCandidateRef: "commit:" + base, ReviewRefs: []string{reviewRef}}},
	}
	recorded, err := s.RecordProofManifest(ids[0], "codex", m, time.Now())
	if err != nil {
		t.Fatal(err)
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
	return proofAdoptionFixture{s, ids[0], ids[1], repo, base, head, canonical, recorded.ManifestRef, result, now}
}
func (f proofAdoptionFixture) adopt(t *testing.T) envelope.WorkEnvelope {
	t.Helper()
	w, err := f.store.AdoptProof(f.destination, ProofAdoptionInput{Actor: "codex", ProofResultID: f.result.ID, ManifestRef: f.manifestRef}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func TestProofAdoptionOneFullProofTwoIndependentLandChecksAndCloses(t *testing.T) {
	f := newProofAdoptionFixture(t)
	adopted := f.adopt(t)
	beforeAttempts := append([]string(nil), adopted.RunAttemptIDs...)
	again, err := f.store.AdoptProof(f.destination, ProofAdoptionInput{Actor: "codex", ProofResultID: f.result.ID, ManifestRef: f.manifestRef,
		ExpectedPolicyDecisionID: "pol_prior", ExpectedActionIntentID: "act_prior"}, time.Now())
	if err != nil || !reflect.DeepEqual(adopted, again) {
		t.Fatalf("exact retry must be unchanged: %v", err)
	}
	if oneOf(f.result.RunAttemptID, adopted.RunAttemptIDs) {
		t.Fatal("foreign source attempt attached to destination")
	}
	for _, id := range []string{f.source, f.destination} {
		report, err := f.store.LandCheck(id, LandCheckOptions{CandidateTipSHA: f.head, Actor: "codex", Record: true}, time.Now())
		if err != nil || report.Status != "ready_to_land" {
			t.Fatalf("ready %s: %+v %v", id, report, err)
		}
		if id == f.destination && !strings.HasPrefix(report.ProofVerificationRef, ProofAdoptionVerificationPrefix) {
			t.Fatal("origin was relabelled as direct proof")
		}
	}
	landingGit(t, f.repo, "merge", "--ff-only", f.head)
	for _, id := range []string{f.source, f.destination} {
		report, err := f.store.LandCheck(id, LandCheckOptions{CandidateTipSHA: f.head, Actor: "codex", Record: true}, time.Now())
		if err != nil || report.Status != "landed" {
			t.Fatalf("landed %s: %+v %v", id, report, err)
		}
		_, err = f.store.Close(id, CloseInput{Outcome: "completed", MoveType: "control_plane_repair", Actor: "codex", Summary: "independent synthetic acceptance", Changed: true,
			ProofRefs: []string{"commit:" + f.head, "check:synthetic-acceptance", "live:synthetic-consumer", report.EvidenceRef}}, time.Now())
		if err != nil {
			t.Fatalf("close %s: %v", id, err)
		}
	}
	destination, err := f.store.Get(f.destination)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeAttempts, destination.RunAttemptIDs) {
		t.Fatal("adoption/land/close created or attached execution")
	}
	// Source has now closed and lost its lease; archival preserves original identity.
	if _, err := home.ArchiveOperationalHistory(f.store.home, home.OperationalArchiveOptions{Kinds: []string{"work"}}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Historical adoption still authenticates after source archival.
	if _, _, _, err := f.store.latestLandingProofResolution(destination); err != nil {
		t.Fatal(err)
	}
}
func TestProofAdoptionRejectsInvalidSourceOrMembership(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, *proofAdoptionFixture)
	}{
		{"destination-candidate", func(t *testing.T, f *proofAdoptionFixture) {
			attachLandingCommit(t, f.store, f.destination, f.base, time.Now())
		}},
		{"source-candidate", func(t *testing.T, f *proofAdoptionFixture) {
			attachLandingCommit(t, f.store, f.source, f.base, time.Now())
		}},
		{"destination-predicate", func(t *testing.T, f *proofAdoptionFixture) {
			w, _ := f.store.Get(f.destination)
			w.SuccessPredicate = "changed acceptance"
			if err := f.store.writeJSON(f.store.workPath(w.ID), &w); err != nil {
				t.Fatal(err)
			}
		}},
		{"source-contract", func(t *testing.T, f *proofAdoptionFixture) {
			w, _ := f.store.Get(f.source)
			w.GitDelivery.ExpectedBaseSHA = f.head
			if err := f.store.writeJSON(f.store.workPath(w.ID), &w); err != nil {
				t.Fatal(err)
			}
		}},
		{"foreign-repository", func(t *testing.T, f *proofAdoptionFixture) {
			w, _ := f.store.Get(f.destination)
			w.GitDelivery.RepositoryFingerprint = strings.Repeat("a", 64)
			if err := f.store.writeJSON(f.store.workPath(w.ID), &w); err != nil {
				t.Fatal(err)
			}
		}},
		{"unreviewed-member", func(t *testing.T, f *proofAdoptionFixture) {
			f.manifest.Members[1].WorkID = "work_unreviewed"
			body, ref, err := proofManifestBody(f.manifest)
			if err != nil {
				t.Fatal(err)
			}
			a, _ := home.NewArtifactStore(f.store.home)
			if _, err = a.Put(body); err != nil {
				t.Fatal(err)
			}
			f.manifestRef = ref
		}},
		{"missing-manifest", func(t *testing.T, f *proofAdoptionFixture) {
			digest, _ := home.ArtifactHex(f.manifestRef)
			if err := os.Remove(filepath.Join(f.store.home, "artifacts", "sha256", digest)); err != nil {
				t.Fatal(err)
			}
		}},
		{"tampered-manifest", func(t *testing.T, f *proofAdoptionFixture) {
			digest, _ := home.ArtifactHex(f.manifestRef)
			if err := os.WriteFile(filepath.Join(f.store.home, "artifacts", "sha256", digest), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"forged-result", func(t *testing.T, f *proofAdoptionFixture) { f.result.ID = "pfrs_forged" }},
		{"pending-finalization", func(t *testing.T, f *proofAdoptionFixture) {
			path, err := f.store.proofResourceCompletionPendingPath(f.result.ReservationID)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(path, []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"pending-legacy-recovery", func(t *testing.T, f *proofAdoptionFixture) {
			path, err := f.store.proofResourceLegacyRecoveryPendingPath(f.source)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(path, []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"red-result", func(t *testing.T, f *proofAdoptionFixture) {
			completion, err := f.store.eventBytes(f.result.CompletionID)
			if err != nil {
				t.Fatal(err)
			}
			// Finalization follows its result in the same append-only segment.
			// Preserve it while replacing the earlier result for this corruption case.
			if err = f.store.removeEvent(f.result.CompletionID); err != nil {
				t.Fatal(err)
			}
			r := f.result
			r.Status = ProofResourceFailed
			if err = f.store.replaceEvent(r.ID, &r); err != nil {
				t.Fatal(err)
			}
			if _, err = f.store.events.PutJSON(f.result.CompletionID, json.RawMessage(completion), "  "); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing-finalization", func(t *testing.T, f *proofAdoptionFixture) {
			if err := f.store.removeEvent(f.result.CompletionID); err != nil {
				t.Fatal(err)
			}
		}},
		{"transitive-source", func(t *testing.T, f *proofAdoptionFixture) {
			w, _ := f.store.Get(f.source)
			w.VerificationIDs = []string{ProofAdoptionVerificationPrefix + f.result.ID + ":" + strings.Repeat("a", 64)}
			if err := f.store.writeJSON(f.store.workPath(w.ID), &w); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newProofAdoptionFixture(t)
			tc.change(t, &f)
			before, _ := f.store.Get(f.destination)
			_, err := f.store.AdoptProof(f.destination, ProofAdoptionInput{Actor: "codex", ProofResultID: f.result.ID, ManifestRef: f.manifestRef}, time.Now())
			if err == nil {
				t.Fatal("invalid adoption accepted")
			}
			after, _ := f.store.Get(f.destination)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("rejected adoption mutated destination")
			}
		})
	}
}
func TestProofAdoptionRawForeignAndGenericAdmissionStayClosed(t *testing.T) {
	f := newProofAdoptionFixture(t)
	w, _ := f.store.Get(f.destination)
	if _, err := f.store.Advance(w.ID, AdvanceInput{To: w.State, Actor: "codex", VerificationID: f.result.VerificationRef}, time.Now()); err != nil {
		t.Fatal(err)
	}
	w, _ = f.store.Get(w.ID)
	if _, _, _, err := f.store.latestLandingProofResolution(w); err == nil {
		t.Fatal("raw foreign proof accepted")
	}
	ref, _ := proofAdoptionRef(f.result.ID, f.manifestRef)
	if _, err := f.store.Advance(w.ID, AdvanceInput{To: w.State, Actor: "codex", VerificationID: ref}, time.Now()); err == nil {
		t.Fatal("generic advance bypassed proof-adopt")
	}
	if _, err := f.store.Advance(w.ID, AdvanceInput{To: w.State, Actor: "codex", ArtifactID: ProofManifestArtifactPrefix + f.manifestRef}, time.Now()); err == nil {
		t.Fatal("generic advance bypassed manifest custody")
	}
	f.adopt(t)
	w, _ = f.store.Get(w.ID)
	if _, _, err := f.store.latestSourceBoundProofResult(w); err == nil {
		t.Fatal("private direct-proof resolver accepted adoption")
	}
	// New malformed relevant reference must fail, never fall back to adopted green.
	w.VerificationIDs = append(w.VerificationIDs, "proof-adopt:malformed")
	if _, _, _, err := f.store.latestLandingProofResolution(w); err == nil {
		t.Fatal("malformed latest adoption fell back")
	}
}
func TestProofAdoptionHistoricalTamperBlocksLaterLanding(t *testing.T) {
	f := newProofAdoptionFixture(t)
	w := f.adopt(t)
	if _, err := f.store.Advance(w.ID, AdvanceInput{To: w.State, Actor: "codex", Note: "later legitimate generation"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	w, _ = f.store.Get(w.ID)
	if _, _, _, err := f.store.latestLandingProofResolution(w); err != nil {
		t.Fatal(err)
	}
	if err := f.store.removeEvent(f.result.CompletionID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.store.latestLandingProofResolution(w); err == nil {
		t.Fatal("lost source finalization accepted after adoption")
	}
}
func TestProofAdoptionRetryRecoversExactInterruptedAttachment(t *testing.T) {
	f := newProofAdoptionFixture(t)
	old := advanceBeforeWorkCommit
	advanceBeforeWorkCommit = func() { panic("synthetic attachment crash") }
	func() {
		defer func() {
			if recover() == nil {
				t.Error("crash not reached")
			}
		}()
		_, _ = f.store.AdoptProof(f.destination, ProofAdoptionInput{Actor: "codex", ProofResultID: f.result.ID, ManifestRef: f.manifestRef}, time.Now())
	}()
	advanceBeforeWorkCommit = old
	t.Cleanup(func() { advanceBeforeWorkCommit = old })
	w := f.adopt(t)
	if len(w.VerificationIDs) != 1 {
		t.Fatalf("retry duplicate: %v", w.VerificationIDs)
	}
	if err := f.store.validateProofAttachment(w, w.VerificationIDs[0]); err != nil {
		t.Fatal(err)
	}
}
func TestProofAdoptionRejectsForeignHolderAndStaleGeneration(t *testing.T) {
	f := newProofAdoptionFixture(t)
	for _, in := range []ProofAdoptionInput{
		{Actor: "foreign", ProofResultID: f.result.ID, ManifestRef: f.manifestRef},
		{Actor: "codex", ProofResultID: f.result.ID, ManifestRef: f.manifestRef, ExpectedPolicyDecisionID: "pol_stale", ExpectedActionIntentID: "act_stale"},
	} {
		if _, err := f.store.AdoptProof(f.destination, in, time.Now()); err == nil {
			t.Fatal("stale/foreign admission accepted")
		}
	}
}
func TestProofAdoptionCLIIsStrictAndUsesNativeCustody(t *testing.T) {
	f := newProofAdoptionFixture(t)
	for _, args := range [][]string{{}, {"work_example", "--actor", "a", "--actor", "b"}, {"work_example", "--actor", "a", "--unknown"}, {"work_example", "--actor", "a", "--proof-result", "pfrs_x", "--manifest", "bad"}} {
		if ProofAdoptionCLIRequiresWritableStore("proof-adopt", args) {
			t.Fatalf("invalid args open writable store: %v", args)
		}
	}
	var stdout, stderr bytes.Buffer
	code := RunProofAdoptionCLI(f.store, "proof-adopt", []string{f.destination, "--actor", "codex", "--proof-result", f.result.ID, "--manifest", f.manifestRef}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("CLI: %d %s", code, stderr.String())
	}
	var w envelope.WorkEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &w); err != nil || w.ID != f.destination {
		t.Fatalf("output: %v", err)
	}
}

func TestProofAdoptionFreshBaseAcceptsDifferentHistoricalCreationBases(t *testing.T) {
	f := newProofAdoptionFixtureWithFreshBase(t, true)
	source, _ := f.store.Get(f.source)
	destination, _ := f.store.Get(f.destination)
	if source.GitDelivery.ExpectedBaseSHA == destination.GitDelivery.ExpectedBaseSHA {
		t.Fatal("fixture did not preserve differing creation bases")
	}
	w := f.adopt(t)
	ref, result, found, err := f.store.latestLandingProofResolution(w)
	if err != nil || !found || result.SourceSnapshot.BaseSHA != f.base || !strings.HasPrefix(ref, ProofAdoptionVerificationPrefix) {
		t.Fatalf("fresh base adoption: %v", err)
	}
}
func TestProofAdoptionReviewEvidenceIsRequiredAndRevalidated(t *testing.T) {
	for _, kind := range []string{"missing", "tampered", "unattached", "empty-membership"} {
		t.Run(kind, func(t *testing.T) {
			f := newProofAdoptionFixture(t)
			ref := f.manifest.Members[1].ReviewRefs[0]
			digest, _ := home.ArtifactHex(ref)
			switch kind {
			case "missing":
				if err := os.Remove(filepath.Join(f.store.home, "artifacts", "sha256", digest)); err != nil {
					t.Fatal(err)
				}
			case "tampered":
				if err := os.WriteFile(filepath.Join(f.store.home, "artifacts", "sha256", digest), []byte("tampered"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unattached":
				w, _ := f.store.Get(f.destination)
				ids := []string{}
				for _, id := range w.ArtifactIDs {
					if id != ref {
						ids = append(ids, id)
					}
				}
				w.ArtifactIDs = ids
				if err := f.store.writeJSON(f.store.workPath(w.ID), &w); err != nil {
					t.Fatal(err)
				}
			case "empty-membership":
				f.manifest.Members[1].ReviewRefs = nil
				if _, err := f.store.RecordProofManifest(f.source, "codex", f.manifest, time.Now()); err == nil {
					t.Fatal("manifest manufactured missing review")
				}
				return
			}
			if _, err := f.store.AdoptProof(f.destination, ProofAdoptionInput{Actor: "codex", ProofResultID: f.result.ID, ManifestRef: f.manifestRef}, time.Now()); err == nil {
				t.Fatal("review evidence not revalidated")
			}
		})
	}
}
func TestProofAdoptionActionBindsExactReference(t *testing.T) {
	f := newProofAdoptionFixture(t)
	w := f.adopt(t)
	actionID := latestActionIntentID(w)
	var action envelope.ActionIntent
	if err := f.store.readEventJSON(actionID, &action); err != nil {
		t.Fatal(err)
	}
	if action.ArgumentsHash != proofAttachmentArgumentsHash(w.VerificationIDs[0]) {
		t.Fatal("admission does not bind exact ref")
	}
	action.ArgumentsHash = proofAttachmentArgumentsHash("different evidence")
	if err := f.store.replaceEvent(actionID, &action); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.store.latestLandingProofResolution(w); err == nil {
		t.Fatal("spliced admission accepted")
	}
}
func TestProofAdoptionStaleMainPreservesCompletedEvidence(t *testing.T) {
	f := newProofAdoptionFixture(t)
	w := f.adopt(t)
	landingGit(t, f.repo, "commit", "--allow-empty", "-m", "independent main change")
	report, err := f.store.LandCheck(f.destination, LandCheckOptions{CandidateTipSHA: f.head, Actor: "codex"}, time.Now())
	if err != nil || report.Status != "rebase_required" {
		t.Fatalf("stale main: %+v %v", report, err)
	}
	after, _ := f.store.Get(w.ID)
	if !reflect.DeepEqual(w, after) {
		t.Fatal("stale main destroyed completed proof acceptance")
	}
	// A target tip back INSIDE the certified candidate keeps that same proof:
	// the resolution still names the original result, with no rerun.
	landingGit(t, f.repo, "reset", "--hard", f.base)
	inside, err := f.store.LandCheck(f.destination, LandCheckOptions{CandidateTipSHA: f.head, Actor: "codex"}, time.Now())
	if err != nil || inside.Status != "ready_to_land" {
		t.Fatalf("target inside certified head: %+v %v", inside, err)
	}
	if inside.ProofVerificationRef != report.ProofVerificationRef || inside.ProofHeadSHA != f.head {
		t.Fatalf("proof evidence changed with the target tip: %+v %+v", inside, report)
	}
}

func TestProofAdoptionMembershipMustPrecedeOriginalReservation(t *testing.T) {
	f := newProofAdoptionFixture(t)
	artifacts, _ := home.NewArtifactStore(f.store.home)
	review, err := artifacts.Put([]byte("later independent review " + f.source + " " + f.destination))
	if err != nil {
		t.Fatal(err)
	}
	for i, member := range f.manifest.Members {
		w, _ := f.store.Get(member.WorkID)
		if _, err = f.store.Advance(w.ID, AdvanceInput{To: w.State, Actor: "codex", ArtifactID: review}, time.Now()); err != nil {
			t.Fatal(err)
		}
		f.manifest.Members[i].ReviewRefs = []string{review}
	}
	m, err := f.store.RecordProofManifest(f.source, "codex", f.manifest, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.AdoptProof(f.destination, ProofAdoptionInput{Actor: "codex", ProofResultID: f.result.ID, ManifestRef: m.ManifestRef}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "before original proof reservation") {
		t.Fatalf("post-proof membership accepted: %v", err)
	}
}
func TestProofAdoptionUnrelatedPendingCannotBeRecoveredByNewAdoption(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "current-different-operation", true: "stale-generation"}[stale], func(t *testing.T) {
			f := newProofAdoptionFixture(t)
			before, _ := f.store.Get(f.destination)
			old := advanceBeforeWorkCommit
			advanceBeforeWorkCommit = func() { panic("synthetic unrelated crash") }
			func() {
				defer func() {
					if recover() == nil {
						t.Error("unrelated pending crash did not happen")
					}
				}()
				_, _ = f.store.Advance(f.destination, AdvanceInput{To: "verifying", Actor: "codex", ArtifactID: "evidence:unrelated-pending"}, time.Now())
			}()
			advanceBeforeWorkCommit = old
			t.Cleanup(func() { advanceBeforeWorkCommit = old })
			path := f.store.advancePendingPath(f.destination)
			pendingBefore, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			expectedPolicy, expectedAction := before.PolicyDecisionID, latestActionIntentID(before)
			if stale {
				expectedPolicy = "pol_stale"
				expectedAction = "act_stale"
			}
			_, err = f.store.AdoptProof(f.destination, ProofAdoptionInput{Actor: "codex", ProofResultID: f.result.ID, ManifestRef: f.manifestRef, ExpectedPolicyDecisionID: expectedPolicy, ExpectedActionIntentID: expectedAction}, time.Now())
			if err == nil {
				t.Fatal("adoption recovered another operation")
			}
			after, _ := f.store.Get(f.destination)
			pendingAfter, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(pendingBefore, pendingAfter) || !reflect.DeepEqual(before, after) {
				t.Fatal("refusal changed another operation or primary work")
			}
		})
	}
}
