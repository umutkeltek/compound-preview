package browser

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/policy"
)

func TestTypeSafeCredentialEnrollmentIsGovernedAndContentFree(t *testing.T) {
	home := t.TempDir()
	key := []byte("synthetic-typesafe-test-key")
	path := filepath.Join(home, "secrets", "typesafe", "jev-api-key")
	refused, err := StoreTypeSafeCredential(home, "work_browser_test", key, policy.DefaultConfig(), testTime())
	if err != nil || !refused.Refused || refused.ReceiptID == "" || refused.TransitionID == "" {
		t.Fatalf("default security hold did not refuse: %+v err=%v", refused, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("held credential was written")
	}
	cfg := policy.DefaultConfig()
	cfg.StandingGrants = append(cfg.StandingGrants, policy.StandingGrant{
		ID: "synthetic-jev-credential", Capability: "browser.credential.store",
		MutationClass: "security", ReversibilityCeiling: "hard", BlastRadius: "local",
	})
	stored, err := StoreTypeSafeCredential(home, "work_browser_test", key, cfg, testTime())
	if err != nil || !stored.Stored || stored.ReceiptID == "" || stored.DecisionID == "" {
		t.Fatalf("governed credential store failed: %+v err=%v", stored, err)
	}
	second, err := StoreTypeSafeCredential(home, "work_browser_test", key, cfg, testTime().Add(time.Second))
	if err != nil || !second.Stored || second.ReceiptID == "" || second.TransitionID == "" {
		t.Fatalf("same-key retry lacks terminal audit: %+v err=%v", second, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("secret mode: %v %v", info, err)
	}
	resolved, err := capgraph.ResolveCredentialForSurface(home, "tool:browser-jev-typesafe", "work_browser_test", jevCredentialHandle())
	if err != nil || resolved != "TYPESAFE_API_KEY="+string(key) {
		t.Fatalf("credential handle did not resolve: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := capgraph.ResolveCredentialForSurface(home, "tool:browser-jev-typesafe", "work_browser_test", jevCredentialHandle()); err == nil {
		t.Fatal("world-readable credential was accepted")
	}
	for _, dir := range []string{"events", "registries"} {
		_ = filepath.WalkDir(filepath.Join(home, dir), func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() {
				return walkErr
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if strings.Contains(string(body), string(key)) {
				t.Errorf("credential value leaked into %s", dir)
			}
			return nil
		})
	}
}

func TestCredentialProofRetryRepairsExactTransition(t *testing.T) {
	home := t.TempDir()
	key := []byte("synthetic-repair-key")
	cfg := policy.DefaultConfig()
	cfg.StandingGrants = append(cfg.StandingGrants, policy.StandingGrant{
		ID: "synthetic-jev-credential", Capability: "browser.credential.store",
		MutationClass: "security", ReversibilityCeiling: "hard", BlastRadius: "local",
	})
	first, err := StoreTypeSafeCredential(home, "work_browser_test", key, cfg, testTime())
	if err != nil || !first.Stored {
		t.Fatalf("first enrollment: %+v %v", first, err)
	}
	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Read(first.TransitionID)
	if err != nil {
		t.Fatal(err)
	}
	var transition envelope.Transition
	if err := json.Unmarshal(record.Bytes, &transition); err != nil {
		t.Fatal(err)
	}
	receiptRecord, err := store.Read(first.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	var receipt envelope.Receipt
	if err := json.Unmarshal(receiptRecord.Bytes, &receipt); err != nil {
		t.Fatal(err)
	}
	lock, err := lockCredentialSlot(home)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := stageCredentialSecret(home, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePendingCredentialProof(home, receipt, transition, first.DecisionID, stage, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(first.TransitionID); err != nil {
		t.Fatal(err)
	}
	unlockCredentialSlot(lock)
	retry, err := StoreTypeSafeCredential(home, "work_browser_test", key, cfg, testTime().Add(time.Second))
	if err != nil || !retry.Stored {
		t.Fatalf("repair retry: %+v %v", retry, err)
	}
	if _, err := store.Read(first.TransitionID); err != nil {
		t.Fatalf("original transition was not repaired: %v", err)
	}
	if _, err := os.Stat(credentialJournalPath(home)); !os.IsNotExist(err) {
		t.Fatalf("credential proof journal not cleared: %v", err)
	}
}

func TestPreparedCredentialRotationRecoversBeforeMutation(t *testing.T) {
	home := t.TempDir()
	cfg := policy.DefaultConfig()
	cfg.StandingGrants = append(cfg.StandingGrants, policy.StandingGrant{
		ID: "synthetic-jev-credential", Capability: "browser.credential.store",
		MutationClass: "security", ReversibilityCeiling: "hard", BlastRadius: "local",
	})
	oldKey := []byte("synthetic-old-key")
	newKey := []byte("synthetic-new-key")
	if _, err := StoreTypeSafeCredential(home, "work_browser_test", oldKey, cfg, testTime()); err != nil {
		t.Fatal(err)
	}
	original, err := RotateTypeSafeCredential(home, "work_browser_test", newKey, cfg, testTime().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	receiptRecord, err := store.Read(original.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	transitionRecord, err := store.Read(original.TransitionID)
	if err != nil {
		t.Fatal(err)
	}
	var receipt envelope.Receipt
	var transition envelope.Transition
	if err := json.Unmarshal(receiptRecord.Bytes, &receipt); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(transitionRecord.Bytes, &transition); err != nil {
		t.Fatal(err)
	}
	newStage, err := stageCredentialSecret(home, newKey)
	if err != nil {
		t.Fatal(err)
	}
	oldStage, err := stageCredentialSecret(home, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePendingCredentialProof(home, receipt, transition, original.DecisionID, newStage, oldStage); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(original.ReceiptID); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(original.TransitionID); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(home, "secrets", "typesafe", "jev-api-key")
	if _, err := replaceCredentialFile(secretPath, oldKey); err != nil {
		t.Fatal(err)
	}
	if err := repairPendingCredentialProofWithJournalSync(home, store, func(string) error {
		return errors.New("synthetic journal directory sync fault")
	}); err == nil {
		t.Fatal("rotation proceeded without durable preparation")
	}
	before, err := os.ReadFile(secretPath)
	if err != nil || string(before) != string(oldKey) {
		t.Fatal("journal sync failure changed live slot")
	}
	if _, err := store.Read(original.ReceiptID); err == nil {
		t.Fatal("journal sync failure published completed receipt")
	}
	if _, err := RotateTypeSafeCredential(home, "work_browser_test", newKey, cfg, testTime().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(original.ReceiptID); err != nil {
		t.Fatalf("original receipt not recovered: %v", err)
	}
	if _, err := store.Read(original.TransitionID); err != nil {
		t.Fatalf("original transition not recovered: %v", err)
	}
	if _, err := os.Stat(credentialJournalPath(home)); !os.IsNotExist(err) {
		t.Fatalf("prepared journal retained: %v", err)
	}
	got, err := os.ReadFile(secretPath)
	if err != nil || string(got) != string(newKey) {
		t.Fatal("recovered rotation did not restore exact staged value")
	}
}

func TestPreparedCredentialEnrollmentRecoversMissingSlot(t *testing.T) {
	home := t.TempDir()
	cfg := policy.DefaultConfig()
	cfg.StandingGrants = append(cfg.StandingGrants, policy.StandingGrant{
		ID: "synthetic-jev-credential", Capability: "browser.credential.store",
		MutationClass: "security", ReversibilityCeiling: "hard", BlastRadius: "local",
	})
	key := []byte("synthetic-enrollment-recovery-key")
	first, err := StoreTypeSafeCredential(home, "work_browser_test", key, cfg, testTime())
	if err != nil {
		t.Fatal(err)
	}
	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	receiptRecord, err := store.Read(first.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	transitionRecord, err := store.Read(first.TransitionID)
	if err != nil {
		t.Fatal(err)
	}
	var receipt envelope.Receipt
	var transition envelope.Transition
	if err := json.Unmarshal(receiptRecord.Bytes, &receipt); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(transitionRecord.Bytes, &transition); err != nil {
		t.Fatal(err)
	}
	stage, err := stageCredentialSecret(home, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePendingCredentialProof(home, receipt, transition, first.DecisionID, stage, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(first.ReceiptID); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(first.TransitionID); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(home, "secrets", "typesafe", "jev-api-key")
	if err := os.Remove(secretPath); err != nil {
		t.Fatal(err)
	}
	if _, err := StoreTypeSafeCredential(home, "work_browser_test", key, cfg, testTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(first.ReceiptID); err != nil {
		t.Fatalf("original receipt not recovered: %v", err)
	}
	if _, err := store.Read(first.TransitionID); err != nil {
		t.Fatalf("original transition not recovered: %v", err)
	}
	if _, err := os.Stat(credentialJournalPath(home)); !os.IsNotExist(err) {
		t.Fatalf("prepared journal retained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "secrets", "typesafe", stage)); !os.IsNotExist(err) {
		t.Fatalf("staged secret retained: %v", err)
	}
	if _, err := capgraph.ResolveCredentialForSurface(home, jevCredentialSurface, "work_browser_test", jevCredentialHandle()); err != nil {
		t.Fatal(err)
	}
}

func TestRestoredCredentialJournalWithoutSecretAbandonsBeforeFreshEnrollment(t *testing.T) {
	home := t.TempDir()
	cfg := policy.DefaultConfig()
	cfg.StandingGrants = append(cfg.StandingGrants, policy.StandingGrant{
		ID: "synthetic-jev-credential", Capability: "browser.credential.store",
		MutationClass: "security", ReversibilityCeiling: "hard", BlastRadius: "local",
	})
	key := []byte("synthetic-restore-key")
	first, err := StoreTypeSafeCredential(home, "work_browser_test", key, cfg, testTime())
	if err != nil {
		t.Fatal(err)
	}
	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	receiptRecord, err := store.Read(first.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	transitionRecord, err := store.Read(first.TransitionID)
	if err != nil {
		t.Fatal(err)
	}
	var receipt envelope.Receipt
	var transition envelope.Transition
	if err := json.Unmarshal(receiptRecord.Bytes, &receipt); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(transitionRecord.Bytes, &transition); err != nil {
		t.Fatal(err)
	}
	stage, err := stageCredentialSecret(home, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePendingCredentialProof(home, receipt, transition, first.DecisionID, stage, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(first.ReceiptID); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(first.TransitionID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, "secrets", "typesafe", stage)); err != nil {
		t.Fatal(err)
	}
	if _, err := StoreTypeSafeCredential(home, "work_browser_test", key, cfg, testTime().Add(time.Second)); err == nil {
		t.Fatal("missing stage with live slot falsely recovered")
	}
	if _, err := os.Stat(credentialJournalPath(home)); err != nil {
		t.Fatalf("uncertain journal was lost: %v", err)
	}
	if err := os.Remove(filepath.Join(home, "secrets", "typesafe", "jev-api-key")); err != nil {
		t.Fatal(err)
	}
	second, err := StoreTypeSafeCredential(home, "work_browser_test", key, cfg, testTime().Add(2*time.Second))
	if err != nil || !second.Stored {
		t.Fatalf("fresh enrollment remained stranded: %+v %v", second, err)
	}
	record, err := store.Read(first.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	var abandoned envelope.Receipt
	if err := json.Unmarshal(record.Bytes, &abandoned); err != nil {
		t.Fatal(err)
	}
	if abandoned.Outcome != "failed" || abandoned.StateDelta.Changed {
		t.Fatalf("restored attempt falsely completed: %+v", abandoned)
	}
	if _, err := os.Stat(credentialJournalPath(home)); !os.IsNotExist(err) {
		t.Fatalf("restored journal retained: %v", err)
	}
}

func TestCredentialRotationLockAndPostRenameOutcome(t *testing.T) {
	home := t.TempDir()
	cfg := policy.DefaultConfig()
	cfg.StandingGrants = append(cfg.StandingGrants, policy.StandingGrant{
		ID: "synthetic-jev-credential", Capability: "browser.credential.store",
		MutationClass: "security", ReversibilityCeiling: "hard", BlastRadius: "local",
	})
	if _, err := StoreTypeSafeCredential(home, "work_browser_test", []byte("synthetic-before"), cfg, testTime()); err != nil {
		t.Fatal(err)
	}
	lock, err := lockCredentialSlot(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RotateTypeSafeCredential(home, "work_browser_test", []byte("synthetic-after"), cfg, testTime().Add(time.Second)); err == nil {
		t.Fatal("concurrent rotation bypassed slot lock")
	}
	unlockCredentialSlot(lock)
	path := filepath.Join(home, "secrets", "typesafe", "jev-api-key")
	committed, err := replaceCredentialFileWithSync(path, []byte("synthetic-after"), func(*os.File) error { return errors.New("synthetic post-rename sync fault") })
	if !committed || err == nil {
		t.Fatalf("post-rename state was hidden: committed=%t err=%v", committed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "synthetic-after" {
		t.Fatal("rename did not activate replacement")
	}
}

func TestTypeSafeCredentialEnrollmentRejectsSymlinkCustodyAndDifferentValue(t *testing.T) {
	cfg := policy.DefaultConfig()
	cfg.StandingGrants = append(cfg.StandingGrants, policy.StandingGrant{
		ID: "synthetic-jev-credential", Capability: "browser.credential.store",
		MutationClass: "security", ReversibilityCeiling: "hard", BlastRadius: "local",
	})
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, "secrets")); err != nil {
		t.Fatal(err)
	}
	if _, err := StoreTypeSafeCredential(home, "work_browser_test", []byte("synthetic-first-key"), cfg, testTime()); err == nil {
		t.Fatal("symlinked custody accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "typesafe", "jev-api-key")); !os.IsNotExist(err) {
		t.Fatal("key escaped custody")
	}
	if err := os.Remove(filepath.Join(home, "secrets")); err != nil {
		t.Fatal(err)
	}
	first, err := StoreTypeSafeCredential(home, "work_browser_test", []byte("synthetic-first-key"), cfg, testTime().Add(time.Second))
	if err != nil || !first.Stored {
		t.Fatalf("first enrollment failed: %+v %v", first, err)
	}
	if _, err := StoreTypeSafeCredential(home, "work_browser_test", []byte("synthetic-different-key"), cfg, testTime().Add(2*time.Second)); err == nil {
		t.Fatal("credential-set replaced an existing secret")
	}
	got, err := os.ReadFile(filepath.Join(home, "secrets", "typesafe", "jev-api-key"))
	if err != nil || string(got) != "synthetic-first-key" {
		t.Fatal("first credential changed")
	}
}

func TestTypeSafeCredentialRotationRequiresGrantAndExactSlot(t *testing.T) {
	home := t.TempDir()
	first := []byte("synthetic-initial-key")
	next := []byte("synthetic-replacement-key")
	cfg := policy.DefaultConfig()
	cfg.StandingGrants = append(cfg.StandingGrants, policy.StandingGrant{
		ID: "synthetic-jev-credential", Capability: "browser.credential.store",
		MutationClass: "security", ReversibilityCeiling: "hard", BlastRadius: "local",
	})
	if _, err := RotateTypeSafeCredential(home, "work_browser_test", next, cfg, testTime()); err == nil {
		t.Fatal("rotation without registered slot accepted")
	}
	if change, err := StoreTypeSafeCredential(home, "work_browser_test", first, cfg, testTime().Add(time.Second)); err != nil || !change.Stored {
		t.Fatalf("initial enrollment failed: %+v %v", change, err)
	}
	if change, err := RotateTypeSafeCredential(home, "work_browser_test", next, policy.DefaultConfig(), testTime().Add(2*time.Second)); err != nil || !change.Refused {
		t.Fatalf("rotation escaped security policy: %+v %v", change, err)
	}
	change, err := RotateTypeSafeCredential(home, "work_browser_test", next, cfg, testTime().Add(3*time.Second))
	if err != nil || !change.Stored || change.ReceiptID == "" || change.TransitionID == "" {
		t.Fatalf("rotation failed: %+v %v", change, err)
	}
	resolved, err := capgraph.ResolveCredentialForSurface(home, jevCredentialSurface, "work_browser_test", jevCredentialHandle())
	if err != nil || resolved != "TYPESAFE_API_KEY="+string(next) {
		t.Fatalf("rotation not live: %v", err)
	}
}
