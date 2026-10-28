package act

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

// These tests pin the FileArtifactAdapter's absent-file pre-image semantics (F11).
// A rollback of a CREATION must DELETE the target (restore "no file"), not leave an
// empty file. A rollback of a MODIFICATION must restore the exact prior bytes. Both
// go through the governed Execute seam exactly as an external caller must — the
// adapter's write stays unexported and unreachable outside package act.

// mkFileIntent builds a valid file-artifact write ActionIntent. Only Target.Path is
// load-bearing for FileArtifactAdapter; the class is external_side_effect so the
// grant-envelope shape mirrors act_test.go's config-key intents.
func mkFileIntent(t *testing.T, workID, path string, now time.Time) envelope.ActionIntent {
	t.Helper()
	sum := sha256Hex(strings.Join([]string{workID, "file", path}, "|"))
	ai := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            "act_" + sum[:12],
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "actuator",
		Capability:    "outbound.file_artifact.write",
		Action:        "stage_artifact",
		Target:        envelope.ActionTarget{Kind: "file_artifact", Path: path},
		MutationClass: "external_side_effect",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       "stage file " + path,
		RollbackPlan:  "restore the captured pre-image of " + path + " (delete if it did not exist)",
	}
	if err := ai.Validate(); err != nil {
		t.Fatalf("test file intent invalid: %v", err)
	}
	return ai
}

func fileGrant(ai envelope.ActionIntent) policy.Config {
	cfg := policy.DefaultConfig()
	cfg.StandingGrants = append(cfg.StandingGrants, policy.StandingGrant{
		ID:                   "grant_file_artifact",
		Capability:           ai.Capability,
		MutationClass:        "external_side_effect",
		ReversibilityCeiling: "easy",
		BlastRadius:          "local",
		TargetPath:           ai.Target.Path,
	})
	return cfg
}

// T6a (F11, afterWriteFailure path): a persistence failure AFTER a successful CREATE
// write must roll the target back to its absent pre-image — DELETING the file, not
// leaving empty bytes. recordEnvelopeFn is forced to fail so the post-write rollback
// path is reached.
func TestFileArtifactCreateRollbackRemovesFile(t *testing.T) {
	now := fixedNow()
	home := t.TempDir()
	store, err := NewArtifactStore(home)
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	path := filepath.Join(t.TempDir(), "brand-new-artifact.txt")
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("precondition: target must not exist, got %v", statErr)
	}
	adapter := NewFileArtifactAdapter()
	ai := mkFileIntent(t, "work_create_rollback", path, now)
	pd := policy.Evaluate(ai, fileGrant(ai), now)
	if !policy.Allowed(pd) {
		t.Fatalf("precondition: granted create must allow, got %s", pd.Outcome)
	}

	orig := recordEnvelopeFn
	recordEnvelopeFn = func(string, string, any) error {
		return fmt.Errorf("forced event-store persistence failure")
	}
	defer func() { recordEnvelopeFn = orig }()

	if _, err := Execute(store, adapter, ai, pd, []byte("brand new body"), now); err == nil {
		t.Fatal("expected an error from the forced persistence failure")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		data, _ := os.ReadFile(path)
		t.Fatalf("rollback of a creation must DELETE the file, not leave one; file present (bytes=%q)", data)
	}
}

// T6b (F11, RevertIntent path): a governed create, then a RevertIntent-driven revert
// (the same forward Execute path a real revert uses) must DELETE the created file,
// because its recorded pre-image is "file absent".
func TestFileArtifactCreateThenRevertRemovesFile(t *testing.T) {
	now := fixedNow()
	home := t.TempDir()
	store, err := NewArtifactStore(home)
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	path := filepath.Join(t.TempDir(), "created.txt")
	adapter := NewFileArtifactAdapter()
	ai := mkFileIntent(t, "work_revert_create", path, now)
	cfg := fileGrant(ai)
	pd := policy.Evaluate(ai, cfg, now)
	res, err := Execute(store, adapter, ai, pd, []byte("first version"), now)
	if err != nil {
		t.Fatalf("governed create failed: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "first version" {
		t.Fatalf("create did not write the body: %q", got)
	}

	later := now.Add(time.Second)
	revert := RevertIntent(ai, res, later)
	if revert.ID == ai.ID {
		t.Fatal("revert must be a distinct intent")
	}
	revertPD := policy.Evaluate(revert, cfg, later)
	if !policy.Allowed(revertPD) {
		t.Fatalf("revert must be governed the same way, got %s", revertPD.Outcome)
	}
	preImage, err := store.Get(res.PreImageRef)
	if err != nil {
		t.Fatalf("load pre-image artifact: %v", err)
	}
	if _, err := ExecuteRevert(store, adapter, revert, revertPD, preImage, later); err != nil {
		t.Fatalf("revert execute failed: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		data, _ := os.ReadFile(path)
		t.Fatalf("RevertIntent of a creation must DELETE the file; still present (bytes=%q)", data)
	}
}

// T7 (F11 regression guard): modifying an EXISTING file then reverting must restore
// the exact prior bytes — the pre-image exists=true path must not delete.
func TestFileArtifactModifyThenRevertRestoresBytes(t *testing.T) {
	now := fixedNow()
	home := t.TempDir()
	store, err := NewArtifactStore(home)
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	path := filepath.Join(t.TempDir(), "existing.txt")
	if err := os.WriteFile(path, []byte("original bytes"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}
	adapter := NewFileArtifactAdapter()
	ai := mkFileIntent(t, "work_modify", path, now)
	cfg := fileGrant(ai)
	pd := policy.Evaluate(ai, cfg, now)
	res, err := Execute(store, adapter, ai, pd, []byte("modified bytes"), now)
	if err != nil {
		t.Fatalf("modify failed: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "modified bytes" {
		t.Fatalf("forward write did not apply: %q", got)
	}

	later := now.Add(time.Second)
	revert := RevertIntent(ai, res, later)
	revertPD := policy.Evaluate(revert, cfg, later)
	if !policy.Allowed(revertPD) {
		t.Fatalf("revert must be governed, got %s", revertPD.Outcome)
	}
	preImage, err := store.Get(res.PreImageRef)
	if err != nil {
		t.Fatalf("load pre-image: %v", err)
	}
	if _, err := ExecuteRevert(store, adapter, revert, revertPD, preImage, later); err != nil {
		t.Fatalf("revert failed: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "original bytes" {
		t.Fatalf("revert of a modification must restore original bytes, got %q", got)
	}
}

// Legacy read compatibility: a byte-only pre-image (no exists marker), such as an old
// receipt's stored bytes, must be treated as exists=true — restoring it writes those
// bytes back rather than deleting the target.
func TestFileArtifactLegacyByteOnlyPreImageTreatedAsExists(t *testing.T) {
	now := fixedNow()
	home := t.TempDir()
	store, err := NewArtifactStore(home)
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	path := filepath.Join(t.TempDir(), "legacy.txt")
	if err := os.WriteFile(path, []byte("current"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	adapter := NewFileArtifactAdapter()
	ai := mkFileIntent(t, "work_legacy", path, now)
	cfg := fileGrant(ai)
	pd := policy.Evaluate(ai, cfg, now)
	// A legacy pre-image is raw bytes with no exists marker; restoring it must write
	// those bytes, not delete.
	if _, err := Execute(store, adapter, ai, pd, []byte("legacy-restored"), now); err != nil {
		t.Fatalf("write legacy-shaped bytes failed: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "legacy-restored" {
		t.Fatalf("legacy byte-only value must be written as content, got %q", got)
	}
}

func TestFileArtifactForwardWriteTreatsPreImageShapedJSONAsOpaqueData(t *testing.T) {
	now := fixedNow()
	home := t.TempDir()
	store, err := NewArtifactStore(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "literal-preimage.json")
	adapter := NewFileArtifactAdapter()
	ai := mkFileIntent(t, "work_literal_preimage", path, now)
	pd := policy.Evaluate(ai, fileGrant(ai), now)
	literal := encodeFilePreImage(false, nil)

	if _, err := Execute(store, adapter, ai, pd, literal, now); err != nil {
		t.Fatalf("forward write failed: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("forward write silently treated data as delete control: %v", err)
	}
	if string(got) != string(literal) {
		t.Fatalf("forward write changed opaque bytes:\ngot  %q\nwant %q", got, literal)
	}
}
