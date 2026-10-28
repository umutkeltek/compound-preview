package candidates

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestR3FFrozenParkReplayMustAcceptContentMatchingFile is the red test for
// the 2026-09-04 promote-driver incident. The packet hypothesizes that the
// frozen-replay path hands authorizeCandidateReplacement a Record whose
// fileIdentity was captured at one Fstat, while the leaf's live re-open
// observes a different stat snapshot (kernel cache, APFS no-op touches,
// Spotlight xattr bumps, etc.). The diary of the production incident
// includes a candidate file that has NOT changed content but the leaf
// nonetheless refused on `changed after custody read`.
//
// The contract the test pins: a Record whose body content (and digest)
// still matches the file on disk must be accepted as authorized to
// replace — its file metadata may drift without invalidating the
// authorization, because the lock holder's pending write will overwrite
// the file with the same body anyway. A Record whose body genuinely
// drifted (digest mismatch) MUST be refused — see
// TestR3FFrozenParkReplayMustRefuseContentDrift, the companion test.
//
// The fix lives near authorizeCandidateReplacement: the leaf should compare
// the live file's digest (re-derived from a fresh read or already-known
// to the caller) rather than the volatile Fstat snapshot. The custody
// guard stays — a file that REALLY changed still refuses.
func TestR3FFrozenParkReplayMustAcceptContentMatchingFile(t *testing.T) {
	home := t.TempDir()
	const candID = "cand_4198be82cb50"
	body := []byte(`{"id":"cand_4198be82cb50","entity":"umut","predicate":"preference.repeated-tool-choice","object":"context7","state":"proposed","claim_type":"preference"}`)
	if err := WriteCandidate(home, Memory, candID, jsonRecord(body)); err != nil {
		t.Fatalf("seed inbox file: %v", err)
	}

	// Read the Record exactly the way MutateCandidateIfDigests does today:
	// a single rooted read returns the freshly-captured fileIdentity that
	// production hands to authorizeCandidateReplacement.
	snapshot, err := readCandidateRecordFromInbox(home, Memory, candID)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	// Bump only the metadata on the same file: a one-second touch that
	// changes atime/mtime/ctime without touching the body. This is the
	// shape of drift the production diary implies — the candidate content
	// is byte-identical but every Fstat the leaf runs after the snapshot
	// observes different timestamp fields.
	path := filepath.Join(home, InboxDir(Memory), candID+".json")
	future := time.Unix(time.Now().Unix()+3600, 0)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("touch inbox file: %v", err)
	}

	// Sanity: the metadata IS now different from what the snapshot carries,
	// so authorizeCandidateReplacement's current Fstat-vs-identity compare
	// WILL fire today — that is the bug.
	live, err := identityForTest(path)
	if err != nil {
		t.Fatalf("compute live identity: %v", err)
	}
	if snapshot.fileIdentity.same(live) {
		t.Skipf("test environment did not drift the file metadata; cannot exercise the bug")
	}

	// Now call the leaf with the snapshot. The contract requires this to
	// succeed because the file CONTENT is unchanged — only metadata
	// drifted — and the lock holder's pending write would replace those
	// bytes anyway.
	withCandidatePublicationLockForTest(home, func(inbox *candidateInbox) error {
		if err := authorizeCandidateReplacement(inbox, Memory, candID, snapshot); err != nil {
			t.Fatalf("authorizeCandidateReplacement refused a content-matching file whose metadata drifted: %v\nThe same shape (file body unchanged, Fstat snapshot different) is what the 2026-09-04 recovery_required errror names. The custody check must compare content, not volatile metadata, so a frozen journal replay can apply an idempotent write.", err)
		}
		return nil
	})
}

// TestR3FFrozenParkReplayMustRefuseContentDrift is the companion assertion:
// when the file's CONTENT genuinely drifts between snapshot and auth (we
// model this by rewriting the body to a different length, which alters
// size + digest + body), the leaf MUST refuse. This bounds the fix: the
// new content-based check has to keep refusing the genuinely-changed
// case.
func TestR3FFrozenParkReplayMustRefuseContentDrift(t *testing.T) {
	home := t.TempDir()
	const candID = "cand_other"
	body := []byte(`{"id":"cand_other","entity":"x","predicate":"p","object":"o","state":"proposed"}`)
	if err := WriteCandidate(home, Memory, candID, jsonRecord(body)); err != nil {
		t.Fatalf("seed inbox file: %v", err)
	}
	snapshot, err := readCandidateRecordFromInbox(home, Memory, candID)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	newer := []byte(`{"id":"cand_other","entity":"x","predicate":"p","object":"o-rewritten-and-longer","state":"proposed"}`)
	if err := WriteCandidate(home, Memory, candID, jsonRecord(newer)); err != nil {
		t.Fatalf("rewrite inbox file: %v", err)
	}

	withCandidatePublicationLockForTest(home, func(inbox *candidateInbox) error {
		err := authorizeCandidateReplacement(inbox, Memory, candID, snapshot)
		if err == nil {
			t.Fatalf("authorizeCandidateReplacement must refuse a snapshot whose recorded body no longer matches the live file")
		}
		return nil
	})
}
