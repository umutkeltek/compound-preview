package work

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

func packageCaptureFixture(t *testing.T) (*Store, envelope.WorkEnvelope, map[string]any, string) {
	t.Helper()
	s := newStore(t)
	if err := os.Chmod(s.home, 0700); err != nil {
		t.Fatal(err)
	}
	w := governedActionRunningWork(t, s, "operator:test")
	repo, _ := initLandingRepo(t)
	repo, _ = filepath.EvalSymlinks(repo)
	writeDeliveryFile(t, repo, ".gitignore", "Caskroom/\nshare/\n")
	writeDeliveryFile(t, repo, "bin/brew", "synthetic package owner\n")
	writeDeliveryFile(t, repo, "Library/Homebrew/brew.sh", "synthetic package owner\n")
	landingGit(t, repo, "add", ".gitignore", "bin/brew", "Library/Homebrew/brew.sh")
	landingGit(t, repo, "commit", "-m", "synthetic package owner")
	target := filepath.Join(repo, "share/sample-sdk")
	writeDeliveryFile(t, repo, "share/sample-sdk/VERSION", "2.0\n")
	receipt := `{"homebrew_version":"1.0","source":{"tap":"homebrew/cask","version":"2.0"},"uninstall_artifacts":[{"preflight_steps":[{"steps":[{"type":"copy","recursive":true,"source":{"base":"staged_path","path":"sample-sdk/."},"target":{"base":"homebrew_prefix","path":"share/sample-sdk"}}]}]}]}`
	writeDeliveryFile(t, repo, "Caskroom/sample-cli/.metadata/INSTALL_RECEIPT.json", receipt)
	a := deliveryTestAction(w, "act_package_effect", "update_installed_package", target, "local_reversible", fixedNow.Add(time.Minute))
	var err error
	w, err = s.RecordAction(w.ID, a, deliveryTestDecision(t, s, a, fixedNow.Add(time.Minute)), fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	r := map[string]any{"schema_version": "compound.installed_package_delivery_capture.v0.1", "capture_id": "package_test", "action_intent_id": a.ID, "manager": "homebrew-cask", "prefix": repo, "package": "sample-cli", "version": "2.0", "expected_work_state": w.State, "expected_policy_decision_id": w.PolicyDecisionID, "expected_action_intent_id": latestActionIntentID(w)}
	return s, w, r, target
}

func capturePackageTest(t *testing.T, s *Store, w envelope.WorkEnvelope, r map[string]any) (NonSourceDeliveryCaptureResult, error) {
	t.Helper()
	b, _ := json.Marshal(r)
	p := filepath.Join(t.TempDir(), "capture.json")
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return s.CaptureNonSourceDeliveryFile(w.ID, p, "operator:test", fixedNow.Add(2*time.Minute))
}

func TestInstalledPackageDeliveryClosesExactEffect(t *testing.T) {
	s, w, r, _ := packageCaptureFixture(t)
	before, err := s.ClosePlan(w.ID, fixedNow.Add(2*time.Minute), ClosePlanOptions{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair"})
	if err != nil || !strings.Contains(before.NoAction, "requires git_delivery") {
		t.Fatalf("original bug missing: %+v %v", before, err)
	}
	captured, err := capturePackageTest(t, s, w, r)
	if err != nil || captured.Status != "captured_not_bound" {
		t.Fatalf("package readback: %+v %v", captured, err)
	}
	w, _ = s.Get(w.ID)
	w, err = s.Advance(w.ID, AdvanceInput{To: w.State, Actor: "operator:test", ExpectedWorkState: w.State, ExpectedPolicyDecisionID: w.PolicyDecisionID, ExpectedActionIntentID: latestActionIntentID(w), NonSourceDelivery: &captured.Delivery}, fixedNow.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Close(w.ID, CloseInput{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair", ProofRefs: []string{captured.Delivery.EvidenceRef}}, fixedNow.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
}

func TestInstalledPackageDeliveryRejectsUnownedPaths(t *testing.T) {
	for _, change := range []string{"manager", "version", "receipt", "tracked", "escape", "nested", "generation"} {
		t.Run(change, func(t *testing.T) {
			s, w, r, target := packageCaptureFixture(t)
			switch change {
			case "manager":
				r["manager"] = "arbitrary-command"
			case "version":
				r["version"] = "3.0"
			case "receipt":
				os.WriteFile(filepath.Join(r["prefix"].(string), "Caskroom/sample-cli/.metadata/INSTALL_RECEIPT.json"), []byte(`{"homebrew_version":"1.0","source":{"version":"2.0"}}`), 0600)
			case "tracked":
				landingGit(t, r["prefix"].(string), "add", "-f", "share/sample-sdk/VERSION")
			case "escape":
				os.Symlink(filepath.Join(r["prefix"].(string), "bin/brew"), filepath.Join(target, "escape"))
			case "nested":
				writeDeliveryFile(t, target, ".git/config", "synthetic")
			case "generation":
				r["expected_policy_decision_id"] = "pol_stale"
			}
			if _, err := capturePackageTest(t, s, w, r); err == nil {
				t.Fatal("unsafe package evidence accepted")
			}
		})
	}
}

func TestInstalledPackageDeliveryRechecksBytesAndRetainsSourceGate(t *testing.T) {
	for _, change := range []string{"bytes", "receipt", "source", "forged_consumer"} {
		t.Run(change, func(t *testing.T) {
			s, w, r, target := packageCaptureFixture(t)
			captured, err := capturePackageTest(t, s, w, r)
			if err != nil {
				t.Fatal(err)
			}
			w, _ = s.Get(w.ID)
			w, err = s.Advance(w.ID, AdvanceInput{To: w.State, Actor: "operator:test", ExpectedWorkState: w.State, ExpectedPolicyDecisionID: w.PolicyDecisionID, ExpectedActionIntentID: latestActionIntentID(w), NonSourceDelivery: &captured.Delivery}, fixedNow.Add(3*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "bytes":
				os.WriteFile(filepath.Join(target, "VERSION"), []byte("changed"), 0600)
			case "receipt":
				os.WriteFile(filepath.Join(r["prefix"].(string), "Caskroom/sample-cli/.metadata/INSTALL_RECEIPT.json"), []byte("{}"), 0600)
			case "source":
				a := deliveryTestAction(w, "act_extra_source", "edit_source", filepath.Join(r["prefix"].(string), "new.go"), "local_reversible", fixedNow.Add(4*time.Minute))
				w, err = s.RecordAction(w.ID, a, deliveryTestDecision(t, s, a, fixedNow.Add(4*time.Minute)), fixedNow.Add(4*time.Minute))
				if err != nil {
					t.Fatal(err)
				}
			case "forged_consumer":
				// A new artifact with invented provenance cannot replace the bound owner receipt.
				w.NonSourceDelivery[0].EvidenceRef = "artifact:sha256:" + strings.Repeat("a", 64)
				if _, err := s.gitDeliveryProofForClose(w, "completed", "control_plane_repair", nil, fixedNow.Add(5*time.Minute)); err == nil {
					t.Fatal("forged evidence accepted")
				}
				return
			}
			plan, err := s.ClosePlan(w.ID, fixedNow.Add(5*time.Minute), ClosePlanOptions{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair"})
			if err == nil && plan.Status != "not_closeable" {
				t.Fatal("changed footprint or uncovered source accepted")
			}
		})
	}
}

func TestInstalledPackageDeliveryCaptureRetryIsIdempotent(t *testing.T) {
	s, w, r, _ := packageCaptureFixture(t)
	prior := privateDeliveryCaptureAfterCustody
	t.Cleanup(func() { privateDeliveryCaptureAfterCustody = prior })
	privateDeliveryCaptureAfterCustody = func(string) error { return fmt.Errorf("injected custody interruption") }
	first, err := capturePackageTest(t, s, w, r)
	if err == nil || first.ActionID == "" {
		t.Fatalf("expected recoverable partial capture: %+v %v", first, err)
	}
	privateDeliveryCaptureAfterCustody = prior
	second, err := capturePackageTest(t, s, w, r)
	if err != nil || second.Status != "captured_not_bound" || second.ActionID != first.ActionID {
		t.Fatalf("retry: %+v %v", second, err)
	}
	third, err := capturePackageTest(t, s, w, r)
	if err != nil || third.ReceiptID != second.ReceiptID || third.Delivery != second.Delivery {
		t.Fatalf("idempotent retry: %+v %v", third, err)
	}
	changed := map[string]any{}
	for k, v := range r {
		changed[k] = v
	}
	changed["expected_policy_decision_id"] = "pol_changed"
	if _, err := capturePackageTest(t, s, w, changed); err == nil {
		t.Fatal("changed request silently reused capture")
	}
}
