package work

import (
	"compound/internal/authority/gitrepository"
	"compound/internal/landlease"
	"context"
	"encoding/base64"
	"encoding/json"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func remoteProofTestFixture(t *testing.T) (ProofResourceReservation, *RemoteProofEvidence) {
	t.Helper()
	source := gitrepository.Snapshot{RepositoryFingerprint: strings.Repeat("1", 64), BaseSHA: strings.Repeat("2", 40), HeadSHA: strings.Repeat("3", 40), TreeSHA: strings.Repeat("4", 40), Clean: true}
	cfg, _ := json.Marshal(map[string]string{"VERIFY_ROOT": "/synthetic/remote-proof", "GOTOOLCHAIN_PIN": "go1.26.0"})
	route := RemoteProofRoute{SchemaVersion: "compound.remote_go_test_route.v0.1", Host: "proof.example", RunnerPath: "/synthetic/runner.py", RunnerSHA256: strings.Repeat("5", 64), GoVersion: "go1.26.0", TimeoutSeconds: 600, ConfigBase64: base64.StdEncoding.EncodeToString(cfg)}
	roots := append([]string{}, ProofCeremonyOwnedIgnoredInputs...)
	sortStrings(roots)
	inputs := []RemoteProofInput{}
	for _, root := range roots {
		inputs = append(inputs, RemoteProofInput{Path: root, Kind: "absent"})
	}
	descriptor := RemoteProofDescriptor{Route: route, Source: source, InputRoots: roots, Inputs: inputs}
	descriptor.InputsDigest, _ = remoteInputsDigest(roots, inputs)
	descriptor.GateDigest, _ = remoteGateDigest(source.HeadSHA)
	descriptor.Digest, _ = remoteDigest(descriptor)
	profile, err := NewProofResourceProfileWithSource(ProofResourceProfileCanonicalFull, "/synthetic/repo", source, []ProofResourceGateProfile{{Name: "go-test", ProofRef: "go-test-count-1-general-release", Command: remoteGateCommand(source.HeadSHA)}})
	if err != nil {
		t.Fatal(err)
	}
	profile.RemoteGoTest = &descriptor
	profile, err = normalizeProofResourceProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	reservation := ProofResourceReservation{ID: "pfrv_fixture", AcquiredAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano), WorkID: "work_fixture", RunAttemptID: "ratt_fixture", Actor: proofActorAlpha, Workspace: profile.Repo, Profile: profile}
	request, err := remoteRequestFor(reservation)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := remoteDigest(request)
	wire := map[string]any{"schema_version": remoteReceiptSchema, "id": request.ID, "request_digest": digest, "request": request, "status": "succeeded", "finished_at": time.Now().UTC().Format(time.RFC3339Nano), "inherited_identity": map[string]any{"work_id": request.WorkID, "reservation_id": request.ReservationID, "run_attempt_id": request.RunAttemptID, "repository_fingerprint": request.RepositoryFingerprint}, "verified_evidence": map[string]any{"head_sha": request.HeadSHA, "base_sha": request.BaseSHA, "source_tree": request.SourceTree, "inputs_digest": request.InputsDigest, "runner_sha256": request.RunnerSHA256, "go_version": request.GoVersion, "gate_digest": request.GateDigest}, "execution": map[string]any{"launched": true, "command": remoteGateCommand(source.HeadSHA), "exit_code": 0, "phases_complete": true}, "cleanup": map[string]any{"status": "ok", "leftovers": 0, "errors": []string{}}, "quiescent": true}
	raw, err := remoteCanonicalJSON(wire)
	if err != nil {
		t.Fatal(err)
	}
	var receipt RemoteProofReceipt
	if err := remoteStrictJSON(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	requestDigest, _ := remoteDigest(request)
	receiptDigest, _ := remoteDigest(json.RawMessage(raw))
	evidence := &RemoteProofEvidence{ProfileDigest: profile.Digest, DescriptorDigest: descriptor.Digest, RequestArtifactRef: "artifact:sha256:" + requestDigest, ReceiptArtifactRef: "artifact:sha256:" + receiptDigest, Request: request, Receipt: receipt, ReceiptJSON: raw}
	return reservation, evidence
}
func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
func TestRemoteProofCanonicalJSONMatchesPython(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	value := map[string]any{"unicode": "Türkçe 日本語 <&>", "array": []any{false, 7, "quoted \" value"}}
	got, err := remoteCanonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(value)
	cmd := exec.Command("python3", "-c", "import json,sys; print(json.dumps(json.load(sys.stdin),sort_keys=True,separators=(',',':'),ensure_ascii=False),end='')")
	cmd.Stdin = strings.NewReader(string(body))
	want, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("canonical JSON differs: %s != %s", got, want)
	}
	for _, text := range []string{"x\u2028y", "x\u2029y"} {
		if _, err := remoteCanonicalJSON(text); err == nil {
			t.Fatal("unsupported codepoint accepted")
		}
	}
}
func TestRemoteProofRejectsIndependentIdentityAndReceiptDrift(t *testing.T) {
	cases := map[string]func(*RemoteProofEvidence){
		"profile":         func(e *RemoteProofEvidence) { e.ProfileDigest = strings.Repeat("a", 64) },
		"descriptor":      func(e *RemoteProofEvidence) { e.DescriptorDigest = strings.Repeat("a", 64) },
		"source-head":     func(e *RemoteProofEvidence) { e.Request.HeadSHA = strings.Repeat("a", 40) },
		"source-base":     func(e *RemoteProofEvidence) { e.Request.BaseSHA = strings.Repeat("a", 40) },
		"source-tree":     func(e *RemoteProofEvidence) { e.Request.SourceTree = strings.Repeat("a", 40) },
		"repository":      func(e *RemoteProofEvidence) { e.Request.RepositoryFingerprint = strings.Repeat("a", 64) },
		"gate":            func(e *RemoteProofEvidence) { e.Request.GateDigest = strings.Repeat("a", 64) },
		"inputs":          func(e *RemoteProofEvidence) { e.Request.InputsDigest = strings.Repeat("a", 64) },
		"program":         func(e *RemoteProofEvidence) { e.Request.RunnerSHA256 = strings.Repeat("a", 64) },
		"toolchain":       func(e *RemoteProofEvidence) { e.Request.GoVersion = "go1.25.0" },
		"attempt":         func(e *RemoteProofEvidence) { e.Request.RunAttemptID = "ratt_other" },
		"request-custody": func(e *RemoteProofEvidence) { e.RequestArtifactRef = "artifact:sha256:" + strings.Repeat("a", 64) },
		"receipt-custody": func(e *RemoteProofEvidence) { e.ReceiptArtifactRef = "artifact:sha256:" + strings.Repeat("a", 64) },
		"dirty":           func(e *RemoteProofEvidence) { *e.Receipt.Cleanup.Leftovers = 1 },
		"nonquiescent":    func(e *RemoteProofEvidence) { e.Receipt.Quiescent = false },
		"unknown":         func(e *RemoteProofEvidence) { e.Receipt.Status = "unknown" },
		"red-exit":        func(e *RemoteProofEvidence) { *e.Receipt.Execution.ExitCode = 1 },
		"missing-phase":   func(e *RemoteProofEvidence) { e.Receipt.Execution.PhasesComplete = false },
		"wrong-command":   func(e *RemoteProofEvidence) { e.Receipt.Execution.Command = []string{"true"} },
		"raw-receipt":     func(e *RemoteProofEvidence) { e.ReceiptJSON = json.RawMessage("{}") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, e := remoteProofTestFixture(t)
			if err := validateRemoteProofEvidence(e, r); err != nil {
				t.Fatal(err)
			}
			mutate(e)
			if err := validateRemoteProofEvidence(e, r); err == nil {
				t.Fatal("mismatch accepted")
			}
		})
	}
}
func TestRemoteProofReportFenceAndReservationBinding(t *testing.T) {
	reservation, evidence := remoteProofTestFixture(t)
	makeReport := func(schema string, ev *RemoteProofEvidence) ownedProofReport {
		body, _ := json.Marshal(map[string]any{"schema_version": schema, "profile_kind": ProofResourceProfileCanonicalFull, "status": "ok", "generated_at": time.Now().UTC().Format(time.RFC3339Nano), "repo": reservation.Workspace, "base_ref": reservation.Profile.BaseRef, "source_snapshot": reservation.Profile.SourceSnapshot, "proof_refs": []string{"go-test-count-1-general-release"}, "steps": []ProofCheckStep{{Name: "go-test", ProofRef: "go-test-count-1-general-release", Command: remoteGateCommand(reservation.Profile.SourceSnapshot.HeadSHA), Status: "ok", RemoteEvidence: ev}}})
		var report ownedProofReport
		if err := json.Unmarshal(body, &report); err != nil {
			t.Fatal(err)
		}
		return report
	}
	report := makeReport(RemoteProofReportSchemaVersion, evidence)
	if err := validateOwnedProofReportAgainstReservation(report, reservation); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name   string
		schema string
		e      *RemoteProofEvidence
	}{{"remote-under-local", ProofCheckSchemaVersion, evidence}, {"local-looking", ProofCheckSchemaVersion, nil}, {"missing-receipt", RemoteProofReportSchemaVersion, nil}} {
		t.Run(item.name, func(t *testing.T) {
			if err := validateOwnedProofReportAgainstReservation(makeReport(item.schema, item.e), reservation); err == nil {
				t.Fatal("remote obligation bypassed")
			}
		})
	}
	local := reservation
	local.Profile.RemoteGoTest = nil
	local.Profile, _ = normalizeProofResourceProfile(local.Profile)
	if err := validateOwnedProofReportAgainstReservation(report, local); err == nil {
		t.Fatal("unexpected remote evidence accepted")
	}
	report = makeReport(ProofCheckSchemaVersion, nil)
	if err := validateOwnedProofReportAgainstReservation(report, local); err != nil {
		t.Fatal("local v0.6 changed:", err)
	}
}
func TestRemoteProofTombstoneCannotClaimGreen(t *testing.T) {
	_, e := remoteProofTestFixture(t)
	receipt := e.Receipt
	receipt.Request = nil
	receipt.Inherited = nil
	receipt.Verified = nil
	receipt.Status = "canceled"
	no := false
	receipt.Execution.Launched = &no
	receipt.Execution.Command = []string{}
	receipt.Execution.ExitCode = nil
	if err := validateRemoteProofReceipt(receipt, e.Request); err != nil {
		t.Fatal(err)
	}
	receipt.Status = "succeeded"
	if err := validateRemoteProofReceipt(receipt, e.Request); err == nil {
		t.Fatal("unseen tombstone claimed green")
	}
}
func TestRemoteProofManifestRejectsDriftBeforeTransport(t *testing.T) {
	r, _ := remoteProofTestFixture(t)
	changed := *r.Profile.RemoteGoTest
	changed.Route.Host = "other.example"
	if err := validateRemoteProofDescriptor(changed); err == nil {
		t.Fatal("route changed without resealing")
	}
	changed = *r.Profile.RemoteGoTest
	changed.InputRoots = changed.InputRoots[:1]
	if err := validateRemoteProofDescriptor(changed); err == nil {
		t.Fatal("trimmed ceremony inventory accepted")
	}
}
func TestRemoteProofTransportFailureHasNoLocalFallback(t *testing.T) {
	r, e := remoteProofTestFixture(t)
	calls := 0
	_, err := remoteTransport(context.Background(), r.Profile.RemoteGoTest.Route, e.Request, "inspect", "", func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls++
		if name != "ssh" {
			t.Fatalf("local execution fallback: %s", name)
		}
		return exec.CommandContext(ctx, "false")
	})
	if err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
func TestRemoteProofMigrationDefaultAndTargets(t *testing.T) {
	for _, item := range []struct {
		target landlease.ProofDomainRevision
		want   landlease.ProofDomainRevision
	}{{"", landlease.ProofDomainLocalV2}, {landlease.ProofDomainLocalV2, landlease.ProofDomainLocalV2}, {landlease.ProofDomainFencedV3, landlease.ProofDomainFencedV3}} {
		got, err := resourceProofMigrationRevision(ResourceMigrationInput{ProofTargetRevision: item.target})
		if err != nil || got != item.want {
			t.Fatalf("%s: %s %v", item.target, got, err)
		}
	}
	if _, err := resourceProofMigrationRevision(ResourceMigrationInput{ProofTargetRevision: "unknown"}); err == nil {
		t.Fatal("unknown revision accepted")
	}
	old := proofMigrationPreview(ResourceMigrationInput{ProofClasses: true}, "rev")
	next := proofMigrationPreview(ResourceMigrationInput{ProofClasses: true, ProofTargetRevision: landlease.ProofDomainFencedV3}, "rev")
	if reflect.DeepEqual(old, next) {
		t.Fatal("target absent from exact approval preview")
	}
}
