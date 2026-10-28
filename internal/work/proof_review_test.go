package work

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

func newProofReviewWork(t *testing.T) (*Store, envelope.WorkEnvelope) {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(h, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	s, err := Open(h)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Create(CreateInput{Owner: "codex", Actor: "codex", SuccessPredicate: "review custody preserves independent acceptance"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	w, err = s.Claim(w.ID, "codex", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"running", "verifying"} {
		w, err = s.Advance(w.ID, AdvanceInput{To: state, Actor: "codex"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
	}
	return s, w
}

func proofReviewRef(body []byte) string {
	sum := sha256.Sum256(body)
	return home.ArtifactRefPrefix + hex.EncodeToString(sum[:])
}

func TestProofReviewCLICustodyFeedsNativeManifest(t *testing.T) {
	f := newProofAdoptionFixture(t)
	for i := range f.manifest.Members {
		member := &f.manifest.Members[i]
		body := []byte("Independent review for " + member.WorkID + " at " + f.head)
		path := filepath.Join(t.TempDir(), "review.md")
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		args := []string{member.WorkID, "--actor", "codex", "--file", path}
		if !ProofAdoptionCLIRequiresWritableStore("proof-review", args) {
			t.Fatal("native review route classified read-only")
		}
		if code := RunProofAdoptionCLI(f.store, "proof-review", args, &stdout, &stderr); code != 0 {
			t.Fatalf("native review: %d %s", code, stderr.String())
		}
		var result ProofReviewResult
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.WorkID != member.WorkID || result.ReviewRef != proofReviewRef(body) {
			t.Fatalf("wrong exact result: %+v", result)
		}
		reader, err := f.store.proofAdoptionArtifactReader()
		if err != nil {
			t.Fatal(err)
		}
		got, err := reader.Get(result.ReviewRef)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("body changed: %q %v", got, err)
		}
		w, _ := f.store.Get(member.WorkID)
		if err := f.store.validateProofAttachment(w, result.ReviewRef); err != nil {
			t.Fatal(err)
		}
		foreign := f.source
		if foreign == member.WorkID {
			foreign = f.destination
		}
		other, _ := f.store.Get(foreign)
		if oneOf(result.ReviewRef, other.ArtifactIDs) {
			t.Fatal("review attached to foreign work")
		}
		member.ReviewRefs = []string{result.ReviewRef}
		member.ContractDigest = ""
	}
	result, err := f.store.RecordProofManifest(f.source, "codex", f.manifest, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m, err := f.store.loadProofManifest(result.ManifestRef)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range m.Members {
		w, _ := f.store.Get(member.WorkID)
		if member.ContractDigest != proofMemberContractDigest(w) {
			t.Fatal("native canonical contract digest missing")
		}
	}
}

func TestProofReviewRetryRecoversOnlyExactAuthorityAndMissingBody(t *testing.T) {
	s, before := newProofReviewWork(t)
	body := []byte("Review exact candidate and acceptance conditions.")
	in := ProofReviewInput{Actor: "codex", Body: body, ExpectedPolicyDecisionID: before.PolicyDecisionID, ExpectedActionIntentID: latestActionIntentID(before)}
	old := advanceBeforeWorkCommit
	advanceBeforeWorkCommit = func() { panic("synthetic review attachment crash") }
	func() {
		defer func() {
			if recover() == nil {
				t.Error("crash not reached")
			}
		}()
		_, _ = s.RecordProofReview(before.ID, in, time.Now())
	}()
	advanceBeforeWorkCommit = old
	t.Cleanup(func() { advanceBeforeWorkCommit = old })
	if _, err := os.Stat(s.advancePendingPath(before.ID)); err != nil {
		t.Fatal(err)
	}
	snapshot := snapshotCloseAuthorityFiles(t, s.home)
	for _, rejected := range []ProofReviewInput{
		{Actor: "foreign", Body: body},
		{Actor: "codex", Body: []byte("different review")},
		{Actor: "codex", Body: body, ExpectedPolicyDecisionID: "pol_stale"},
	} {
		if _, err := s.RecordProofReview(before.ID, rejected, time.Now()); err == nil {
			t.Fatal("foreign or changed retry admitted")
		}
		if !reflect.DeepEqual(snapshot, snapshotCloseAuthorityFiles(t, s.home)) {
			t.Fatal("rejected retry changed pending authority")
		}
	}
	result, err := s.RecordProofReview(before.ID, in, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.Get(before.ID)
	if len(after.ArtifactIDs) != len(before.ArtifactIDs)+1 {
		t.Fatal("review was duplicated")
	}
	if err := s.validateProofAttachment(after, result.ReviewRef); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.advancePendingPath(before.ID)); !os.IsNotExist(err) {
		t.Fatalf("pending retained: %v", err)
	}
	snapshot = snapshotCloseAuthorityFiles(t, s.home)
	if _, err = s.RecordProofReview(before.ID, in, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot, snapshotCloseAuthorityFiles(t, s.home)) {
		t.Fatal("completed exact retry changed bytes")
	}
	digest, _ := home.ArtifactHex(result.ReviewRef)
	if err := os.Remove(filepath.Join(s.home, "artifacts", "sha256", digest)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecordProofReview(before.ID, in, time.Now()); err != nil {
		t.Fatal(err)
	}
	recovered, _ := s.Get(before.ID)
	if !reflect.DeepEqual(after, recovered) {
		t.Fatal("missing-body recovery minted a new lifecycle generation")
	}
	reader, _ := s.proofAdoptionArtifactReader()
	if got, err := reader.Get(result.ReviewRef); err != nil || !bytes.Equal(got, body) {
		t.Fatal("body not recovered")
	}
	if _, err := s.Advance(before.ID, AdvanceInput{To: "verifying", Actor: "codex", Note: "later unrelated authority"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	snapshot = snapshotCloseAuthorityFiles(t, s.home)
	if _, err = s.RecordProofReview(before.ID, in, time.Now()); err == nil {
		t.Fatal("stale retry accepted after later authority")
	}
	if !reflect.DeepEqual(snapshot, snapshotCloseAuthorityFiles(t, s.home)) {
		t.Fatal("stale retry changed bytes")
	}
}

func TestProofReviewRejectsInvalidInputWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   ProofReviewInput
	}{
		{"empty", ProofReviewInput{Actor: "codex"}},
		{"blank", ProofReviewInput{Actor: "codex", Body: []byte(" \n\t")}},
		{"oversize", ProofReviewInput{Actor: "codex", Body: bytes.Repeat([]byte("x"), (1<<20)+1)}},
		{"foreign", ProofReviewInput{Actor: "foreign", Body: []byte("review")}},
		{"missing-actor", ProofReviewInput{Body: []byte("review")}},
		{"stale-policy", ProofReviewInput{Actor: "codex", Body: []byte("review"), ExpectedPolicyDecisionID: "pol_stale"}},
		{"stale-action", ProofReviewInput{Actor: "codex", Body: []byte("review"), ExpectedActionIntentID: "act_stale"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, w := newProofReviewWork(t)
			before := snapshotCloseAuthorityFiles(t, s.home)
			if _, err := s.RecordProofReview(w.ID, tc.in, time.Now()); err == nil {
				t.Fatal("invalid input accepted")
			}
			if !reflect.DeepEqual(before, snapshotCloseAuthorityFiles(t, s.home)) {
				t.Fatal("invalid input mutated authority or custody")
			}
		})
	}
}

func TestProofReviewRejectsGenericAttachmentAndCorruptCustody(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "generic-attachment", true: "corrupt-body"}[corrupt], func(t *testing.T) {
			s, w := newProofReviewWork(t)
			body := []byte("Synthetic review body.")
			artifacts, err := home.NewArtifactStore(s.home)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := artifacts.Put(body)
			if err != nil {
				t.Fatal(err)
			}
			if corrupt {
				digest, _ := home.ArtifactHex(ref)
				if err := os.WriteFile(filepath.Join(s.home, "artifacts", "sha256", digest), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if _, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "codex", ArtifactID: ref}, time.Now()); err != nil {
				t.Fatal(err)
			}
			before := snapshotCloseAuthorityFiles(t, s.home)
			if _, err := s.RecordProofReview(w.ID, ProofReviewInput{Actor: "codex", Body: body}, time.Now()); err == nil {
				t.Fatal("invalid historical custody accepted")
			}
			if !reflect.DeepEqual(before, snapshotCloseAuthorityFiles(t, s.home)) {
				t.Fatal("refusal changed authority or immutable body")
			}
		})
	}
}

func TestProofReviewUnrelatedPendingRemainsUnchanged(t *testing.T) {
	s, w := newProofReviewWork(t)
	old := advanceBeforeWorkCommit
	advanceBeforeWorkCommit = func() { panic("synthetic unrelated crash") }
	func() {
		defer func() {
			if recover() == nil {
				t.Error("crash not reached")
			}
		}()
		_, _ = s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "codex", ArtifactID: "evidence:unrelated"}, time.Now())
	}()
	advanceBeforeWorkCommit = old
	t.Cleanup(func() { advanceBeforeWorkCommit = old })
	before := snapshotCloseAuthorityFiles(t, s.home)
	if _, err := s.RecordProofReview(w.ID, ProofReviewInput{Actor: "codex", Body: []byte("review")}, time.Now()); err == nil {
		t.Fatal("unrelated pending recovered")
	}
	if !reflect.DeepEqual(before, snapshotCloseAuthorityFiles(t, s.home)) {
		t.Fatal("unrelated pending changed")
	}
}

func TestProofReviewCLIRefusesUnknownFlagsAndUnboundedFiles(t *testing.T) {
	for _, args := range [][]string{
		{}, {"work_example", "--actor", "codex"},
		{"work_example", "--actor", "codex", "--file", "review.md", "--file", "other.md"},
		{"work_example", "--actor", "codex", "--file", "review.md", "--review-ref", "foreign"},
	} {
		if ProofAdoptionCLIRequiresWritableStore("proof-review", args) {
			t.Fatalf("invalid args open writable store: %v", args)
		}
	}
	s, w := newProofReviewWork(t)
	large := filepath.Join(t.TempDir(), "review.md")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", (1<<20)+1)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{large, t.TempDir()} {
		before := snapshotCloseAuthorityFiles(t, s.home)
		var stdout, stderr bytes.Buffer
		if code := RunProofAdoptionCLI(s, "proof-review", []string{w.ID, "--actor", "codex", "--file", path}, &stdout, &stderr); code != 2 {
			t.Fatalf("invalid file code=%d stderr=%s", code, stderr.String())
		}
		if !reflect.DeepEqual(before, snapshotCloseAuthorityFiles(t, s.home)) {
			t.Fatal("invalid file mutated work")
		}
	}
}

func TestProofReviewRequiresVerifyingWork(t *testing.T) {
	s, _ := newProofReviewWork(t)
	w, err := s.Create(CreateInput{Owner: "codex", Actor: "codex", SuccessPredicate: "separate work is not yet reviewable"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	w, err = s.Claim(w.ID, "codex", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotCloseAuthorityFiles(t, s.home)
	if _, err := s.RecordProofReview(w.ID, ProofReviewInput{Actor: "codex", Body: []byte("review")}, time.Now()); err == nil {
		t.Fatal("non-verifying work accepted")
	}
	if !reflect.DeepEqual(before, snapshotCloseAuthorityFiles(t, s.home)) {
		t.Fatal("state refusal mutated home")
	}
}
