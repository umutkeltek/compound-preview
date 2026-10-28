package work

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	homepkg "compound/internal/home"
)

func privateCaptureRequest(t *testing.T, f privateDeliveryFixture) PrivateRuntimeDeliveryCaptureRequest {
	t.Helper()
	writeDeliveryFile(t, f.repo, "docs_private/rollback/before", "before")
	return PrivateRuntimeDeliveryCaptureRequest{
		SchemaVersion: PrivateRuntimeDeliveryCaptureSchema, CaptureID: "readback",
		ActionIntentID: f.action.ID, RepositoryRoot: f.repo, RepositoryRef: f.base,
		ExpectedWorkState: f.w.State, ExpectedPolicyDecisionID: f.w.PolicyDecisionID,
		ExpectedActionIntentID: latestActionIntentID(f.w),
		Files: []PrivateRuntimeDeliveryCaptureFile{{Path: f.evidence.Files[0].Path,
			AfterSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("after"))),
			BeforePath:  "docs_private/rollback/before", BeforeSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("before")))}},
	}
}

func TestPrivateRuntimeDeliveryCaptureProducesReadbackNotAcceptance(t *testing.T) {
	f := newPrivateDeliveryFixture(t)
	request := privateCaptureRequest(t, f)
	result, err := f.s.CapturePrivateRuntimeDelivery(f.w.ID, request, "operator:test", fixedNow.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "captured_not_bound" || result.Delivery.Kind != envelope.DeliveryPrivateRuntime || result.Delivery.ActionIntentID != f.action.ID {
		t.Fatalf("capture=%+v", result)
	}
	current, err := f.s.Get(f.w.ID)
	if err != nil || current.State != "running" || len(current.NonSourceDelivery) != 0 {
		t.Fatalf("capture altered acceptance/binding: %+v %v", current, err)
	}
	plan, err := f.s.ClosePlan(f.w.ID, fixedNow.Add(6*time.Minute), ClosePlanOptions{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair"})
	if err != nil || plan.Status != "not_closeable" || !strings.Contains(plan.NoAction, "requires git_delivery") {
		t.Fatalf("capture alone waived delivery: %+v %v", plan, err)
	}
	bound, err := f.s.Advance(f.w.ID, AdvanceInput{Actor: "operator:test", To: current.State,
		ExpectedWorkState: current.State, ExpectedPolicyDecisionID: current.PolicyDecisionID,
		ExpectedActionIntentID: latestActionIntentID(current), NonSourceDelivery: &result.Delivery}, fixedNow.Add(7*time.Minute))
	if err != nil || len(bound.NonSourceDelivery) != 1 {
		t.Fatalf("bind captured evidence: %+v %v", bound, err)
	}
	closed, err := f.s.Close(f.w.ID, CloseInput{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair", Changed: true, Summary: "private delivery readback and custody verified", ProofRefs: []string{result.Delivery.EvidenceRef}}, fixedNow.Add(8*time.Minute))
	if err != nil || closed.Work.State != "completed" {
		t.Fatalf("close captured delivery: %+v %v", closed, err)
	}
}

func TestPrivateRuntimeDeliveryCaptureRefusesBeforeCustody(t *testing.T) {
	for _, kind := range []string{"after hash", "before hash", "source tracked", "state", "generation", "actor", "partial before", "ambiguous before"} {
		t.Run(kind, func(t *testing.T) {
			f := newPrivateDeliveryFixture(t)
			request := privateCaptureRequest(t, f)
			actor := "operator:test"
			switch kind {
			case "after hash":
				request.Files[0].AfterSHA256 = strings.Repeat("0", 64)
			case "before hash":
				request.Files[0].BeforeSHA256 = strings.Repeat("0", 64)
			case "source tracked":
				landingGit(t, f.repo, "add", "-f", "--", request.Files[0].Path)
			case "state":
				request.ExpectedWorkState = "verifying"
			case "generation":
				request.ExpectedPolicyDecisionID = "pol_stale"
			case "actor":
				actor = "operator:foreign"
			case "partial before":
				request.Files[0].BeforeSHA256 = ""
			case "ambiguous before":
				request.Files[0].BeforeAbsent = true
			}
			before := snapshotCloseAuthorityFiles(t, f.s.home)
			if _, err := f.s.CapturePrivateRuntimeDelivery(f.w.ID, request, actor, fixedNow.Add(5*time.Minute)); err == nil {
				t.Fatal("invalid capture accepted")
			}
			if !reflect.DeepEqual(before, snapshotCloseAuthorityFiles(t, f.s.home)) {
				t.Fatal("refused capture mutated custody/work")
			}
		})
	}
}

func TestPrivateRuntimeDeliveryCaptureExactReplay(t *testing.T) {
	f := newPrivateDeliveryFixture(t)
	request := privateCaptureRequest(t, f)
	first, err := f.s.CapturePrivateRuntimeDelivery(f.w.ID, request, "operator:test", fixedNow.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotCloseAuthorityFiles(t, f.s.home)
	second, err := f.s.CapturePrivateRuntimeDelivery(f.w.ID, request, "operator:test", fixedNow.Add(6*time.Minute))
	if err != nil || first.Delivery != second.Delivery || first.ReceiptID != second.ReceiptID {
		t.Fatalf("replay=%+v %v", second, err)
	}
	if !reflect.DeepEqual(before, snapshotCloseAuthorityFiles(t, f.s.home)) {
		t.Fatal("exact capture replay rewrote authority")
	}
	request.Files[0].BeforeAbsent, request.Files[0].BeforePath, request.Files[0].BeforeSHA256 = true, "", ""
	if _, err := f.s.CapturePrivateRuntimeDelivery(f.w.ID, request, "operator:test", fixedNow.Add(7*time.Minute)); err == nil {
		t.Fatal("changed retry accepted")
	}
	if err := os.WriteFile(filepath.Join(f.repo, request.Files[0].Path), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CapturePrivateRuntimeDelivery(f.w.ID, request, "operator:test", fixedNow.Add(8*time.Minute)); err == nil {
		t.Fatal("changed source accepted")
	}
}

func TestPrivateRuntimeDeliveryCaptureInterruptedReplay(t *testing.T) {
	for _, phase := range []string{"reservation", "body", "pending", "action", "decision", "receipt", "transition", "work", "event", "pending_clear", "reservation_completion", "manifest"} {
		t.Run(phase, func(t *testing.T) {
			f := newPrivateDeliveryFixture(t)
			request := privateCaptureRequest(t, f)
			oldCustody, oldWrite := privateDeliveryCaptureAfterCustody, receiptedActionAfterWrite
			t.Cleanup(func() { privateDeliveryCaptureAfterCustody, receiptedActionAfterWrite = oldCustody, oldWrite })
			hit := false
			fail := func(at string) error {
				if at == phase && !hit {
					hit = true
					return fmt.Errorf("injected capture interruption: %s", phase)
				}
				return nil
			}
			privateDeliveryCaptureAfterCustody, receiptedActionAfterWrite = fail, fail
			partial, err := f.s.CapturePrivateRuntimeDelivery(f.w.ID, request, "operator:test", fixedNow.Add(5*time.Minute))
			if err == nil || !hit || partial.Status != "capture_incomplete" {
				t.Fatalf("boundary %s not reached: %+v %v", phase, partial, err)
			}
			if partial.ActionID == "" {
				t.Fatal("partial capture lost action identity")
			}
			if phase == "receipt" || phase == "transition" || phase == "work" || phase == "event" || phase == "pending_clear" || phase == "reservation_completion" {
				if partial.ReceiptID == "" {
					t.Fatalf("%s hid persisted receipt", phase)
				}
			}
			if phase == "transition" || phase == "work" || phase == "event" || phase == "pending_clear" || phase == "reservation_completion" {
				if partial.TransitionID == "" {
					t.Fatalf("%s hid persisted transition", phase)
				}
			}
			privateDeliveryCaptureAfterCustody, receiptedActionAfterWrite = oldCustody, oldWrite
			changed := request
			changed.Files = append([]PrivateRuntimeDeliveryCaptureFile(nil), request.Files...)
			changed.Files[0].BeforeAbsent, changed.Files[0].BeforePath, changed.Files[0].BeforeSHA256 = true, "", ""
			if _, err := f.s.CapturePrivateRuntimeDelivery(f.w.ID, changed, "operator:test", fixedNow.Add(6*time.Minute)); err == nil {
				t.Fatal("changed retry replaced interrupted capture")
			}
			result, err := f.s.CapturePrivateRuntimeDelivery(f.w.ID, request, "operator:test", fixedNow.Add(7*time.Minute))
			if err != nil || result.Status != "captured_not_bound" || result.ActionID != partial.ActionID {
				t.Fatalf("exact retry: %+v %v", result, err)
			}
			current, events, err := f.s.Show(f.w.ID)
			if err != nil || len(current.NonSourceDelivery) != 0 {
				t.Fatalf("capture bound delivery: %+v %v", current, err)
			}
			count := 0
			for _, event := range events {
				if contains(event.Attached, result.ReceiptID) {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("capture emitted %d completed consumer events", count)
			}
		})
	}
}

func TestPrivateRuntimeDeliveryCaptureReadOnlyStoreRefuses(t *testing.T) {
	f := newPrivateDeliveryFixture(t)
	request := privateCaptureRequest(t, f)
	result, err := f.s.CapturePrivateRuntimeDelivery(f.w.ID, request, "operator:test", fixedNow.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	body, err := f.artifacts.GetPrivate(result.Delivery.EvidenceRef)
	if err != nil {
		t.Fatal(err)
	}
	var manifest privateRuntimeDeliveryEvidence
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	digest, err := homepkg.ArtifactHex(manifest.Consumer.ReportRef)
	if err != nil {
		t.Fatal(err)
	}
	// Missing custody is deliberate corruption; read-only replay must not repair it.
	if err := os.Remove(filepath.Join(f.s.home, "private/artifacts/sha256", digest)); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(f.s.home)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotCloseAuthorityFiles(t, f.s.home)
	if _, err := reader.CapturePrivateRuntimeDelivery(f.w.ID, request, "operator:test", fixedNow.Add(6*time.Minute)); err == nil {
		t.Fatal("read-only store admitted mutating capture replay")
	}
	if !reflect.DeepEqual(before, snapshotCloseAuthorityFiles(t, f.s.home)) {
		t.Fatal("read-only refusal mutated custody")
	}
}

func TestPrivateRuntimeDeliveryCaptureFileStrictInput(t *testing.T) {
	for _, kind := range []string{"unknown field", "trailing value", "oversize", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			f := newPrivateDeliveryFixture(t)
			request := privateCaptureRequest(t, f)
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "request.json")
			switch kind {
			case "unknown field":
				body = append(body[:len(body)-1], []byte(",\"success\":true}")...)
			case "trailing value":
				body = append(body, []byte(" {}")...)
			case "oversize":
				body = bytes.Repeat([]byte(" "), privateDeliveryMetadataLimit+1)
			case "directory":
				path = t.TempDir()
			case "symlink":
				target := filepath.Join(t.TempDir(), "real.json")
				if err := os.WriteFile(target, body, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "directory" && kind != "symlink" {
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotCloseAuthorityFiles(t, f.s.home)
			if _, err := f.s.CapturePrivateRuntimeDeliveryFile(f.w.ID, path, "operator:test", fixedNow.Add(5*time.Minute)); err == nil {
				t.Fatal("invalid capture request accepted")
			}
			if !reflect.DeepEqual(before, snapshotCloseAuthorityFiles(t, f.s.home)) {
				t.Fatal("invalid request mutated home")
			}
		})
	}
}

func TestPrivateRuntimeDeliveryCaptureAggregateBodyLimitBeforeCustody(t *testing.T) {
	f := newPrivateDeliveryFixture(t)
	request := privateCaptureRequest(t, f)
	request.Files = nil
	f.action.Target.Path = filepath.Join(f.repo, "docs_private")
	// This fixture needs one original action covering all three files.
	action := deliveryTestAction(f.w, "act_aggregate_capture", "update_private_runtime", f.action.Target.Path, "local_reversible", fixedNow.Add(4*time.Minute))
	var err error
	f.w, err = f.s.RecordAction(f.w.ID, action, deliveryTestDecision(t, f.s, action, fixedNow.Add(4*time.Minute)), fixedNow.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	request.ActionIntentID, request.ExpectedPolicyDecisionID, request.ExpectedActionIntentID = action.ID, f.w.PolicyDecisionID, latestActionIntentID(f.w)
	body := bytes.Repeat([]byte("x"), 43<<20)
	for i := 0; i < 3; i++ {
		path := fmt.Sprintf("docs_private/body-%d", i)
		if err := os.WriteFile(filepath.Join(f.repo, path), body, 0o600); err != nil {
			t.Fatal(err)
		}
		request.Files = append(request.Files, PrivateRuntimeDeliveryCaptureFile{Path: path, AfterSHA256: fmt.Sprintf("%x", sha256.Sum256(body)), BeforeAbsent: true})
	}
	before := snapshotCloseAuthorityFiles(t, f.s.home)
	if _, err := f.s.CapturePrivateRuntimeDelivery(f.w.ID, request, "operator:test", fixedNow.Add(5*time.Minute)); err == nil || !strings.Contains(err.Error(), "total body limit") {
		t.Fatalf("aggregate budget not enforced: %v", err)
	}
	if !reflect.DeepEqual(before, snapshotCloseAuthorityFiles(t, f.s.home)) {
		t.Fatal("aggregate refusal wrote custody or authority")
	}
}

func TestPrivateRuntimeDeliveryCaptureCLIPartialRetry(t *testing.T) {
	f := newPrivateDeliveryFixture(t)
	request := privateCaptureRequest(t, f)
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	old := privateDeliveryCaptureAfterCustody
	t.Cleanup(func() { privateDeliveryCaptureAfterCustody = old })
	privateDeliveryCaptureAfterCustody = func(at string) error {
		if at == "manifest" {
			return fmt.Errorf("injected manifest interruption")
		}
		return nil
	}
	t.Chdir(filepath.Dir(path))
	args := []string{f.w.ID, "--request", filepath.Base(path), "--by", "operator:test", "--json"}
	var stdout, stderr bytes.Buffer
	if code := RunWorkDeliveryCaptureCLI(args, f.s, fixedNow.Add(5*time.Minute), &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d stderr=%s", code, &stderr)
	}
	var partial PrivateRuntimeDeliveryCaptureResult
	if err := json.Unmarshal(stdout.Bytes(), &partial); err != nil {
		t.Fatal(err)
	}
	if partial.Status != "capture_incomplete" || partial.ActionID == "" || partial.ReceiptID == "" || partial.TransitionID == "" || partial.Delivery.EvidenceRef == "" || len(partial.CustodiedRefs) == 0 || len(partial.RetryCommand) < 3 {
		t.Fatalf("partial state hidden: %+v", partial)
	}
	if !filepath.IsAbs(partial.RetryCommand[5]) {
		t.Fatalf("retry depends on old cwd: %v", partial.RetryCommand)
	}
	t.Chdir(t.TempDir())
	privateDeliveryCaptureAfterCustody = old
	stdout.Reset()
	stderr.Reset()
	if code := RunWorkDeliveryCaptureCLI(partial.RetryCommand[3:], f.s, fixedNow.Add(6*time.Minute), &stdout, &stderr); code != 0 {
		t.Fatalf("retry code=%d stderr=%s", code, &stderr)
	}
	var retried PrivateRuntimeDeliveryCaptureResult
	if err := json.Unmarshal(stdout.Bytes(), &retried); err != nil {
		t.Fatal(err)
	}
	if retried.Status != "captured_not_bound" || retried.ActionID != partial.ActionID || retried.ReceiptID != partial.ReceiptID {
		t.Fatalf("retry=%+v partial=%+v", retried, partial)
	}
}
