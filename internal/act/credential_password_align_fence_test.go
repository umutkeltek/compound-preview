package act

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/policy"
	"compound/internal/runattempt"
)

func (f *alignmentFixture) inspect(t *testing.T) CredentialPasswordAlignmentResult {
	t.Helper()
	p, e := f.owner.Plan(f.r, f.workID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	f.approve(t, p)
	result, e := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	events, _ := eventstore.Open(f.owner.home)
	record, e := events.Read(result.ReceiptID)
	if e != nil {
		t.Fatal(e)
	}
	var receipt envelope.Receipt
	_ = json.Unmarshal(record.Bytes, &receipt)
	sum := sha256.Sum256(record.Bytes)
	f.now = time.Now().UTC()
	f.installer.credential.now = f.now
	f.r.Witness = policy.CredentialVerifyWitness{Ref: result.ReceiptID, SHA256: hex.EncodeToString(sum[:]), ObservedAt: receipt.CreatedAt}
	artifacts, _ := NewArtifactStore(f.owner.home)
	raw, e := artifacts.Get(result.ObservationRef)
	if e != nil {
		t.Fatal(e)
	}
	observation, e := decodeCredentialPasswordAlignmentObservation(raw, p.Plan.Request)
	if e != nil || observation.Catalog == nil {
		t.Fatal("missing native metadata", e)
	}
	f.r.Catalog = *observation.Catalog
	f.r.Operation = "align"
	f.r.AttemptID = f.now.Format("20060102t150405z") + "-dddddddddddddddd"
	f.workID = alignmentTestWork(t, f.owner.home, f.now)
	return result
}

func TestCredentialPasswordAlignmentUnrelatedMalformedAttemptIsNotEnumerated(t *testing.T) {
	f := newAlignmentFixture(t)
	path := filepath.Join(f.owner.home, "run_attempts", "ratt_20261020T120000Z_ffffffff.json")
	if e := os.WriteFile(path, []byte("{}"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := credentialPasswordAlignmentInstallerFence(f.owner.home, f.r.File.Path, policy.CredentialVerificationDestination(f.r.CredentialVerificationRequest), time.Now().UTC()); e != nil {
		t.Fatal("unrelated historical attempt blocked exact native fence", e)
	}
}

func TestCredentialPasswordAlignmentConsumesFreshInspectionAndRequiresLaterVerifier(t *testing.T) {
	f := newAlignmentFixture(t)
	f.inspect(t)
	p, e := f.owner.Plan(f.r, f.workID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	f.approve(t, p)
	result, e := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	if result.State != "completed" || result.ConfigurationAccepted {
		t.Fatal("repair was confused with configuration qualification")
	}
	if _, e = credentialPasswordAlignmentInstallerFence(f.owner.home, f.r.File.Path, policy.CredentialVerificationDestination(f.r.CredentialVerificationRequest), f.now.Add(-time.Second)); e == nil {
		t.Fatal("old credential qualification cleared repair fence")
	}
	accepted, e := credentialPasswordAlignmentInstallerFence(f.owner.home, f.r.File.Path, policy.CredentialVerificationDestination(f.r.CredentialVerificationRequest), time.Now().UTC())
	if e != nil || len(accepted) != 1 || accepted[0].ActionID != p.Plan.ActionIntentID {
		t.Fatal("actual completed repair proof was not consumed", e)
	}
	f.r.AttemptID = f.now.Format("20060102t150405z") + "-eeeeeeeeeeeeeeee"
	f.workID = alignmentTestWork(t, f.owner.home, f.now)
	if _, e = f.owner.Plan(f.r, f.workID, "fixture"); e == nil {
		t.Fatal("separate attempt bypassed prior repair fence")
	}
}

func TestCredentialPasswordAlignmentUnknownEffectBlocksInstallerAndBlindRetry(t *testing.T) {
	f := newAlignmentFixture(t)
	f.inspect(t)
	p, e := f.owner.Plan(f.r, f.workID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	f.approve(t, p)
	f.transport.fail = true
	result, e := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	if e == nil || result.State != "recovery_required" {
		t.Fatal("launched failure did not retain uncertainty", e)
	}
	a, _ := runattempt.Open(f.owner.home).Get(result.RunAttemptID)
	if a.Status != runattempt.StatusFailed {
		t.Fatal("failed native attempt was not terminalized")
	}
	if _, e = credentialPasswordAlignmentInstallerFence(f.owner.home, f.r.File.Path, policy.CredentialVerificationDestination(f.r.CredentialVerificationRequest), f.now.Add(time.Second)); e == nil {
		t.Fatal("later successful verifier could erase an unresolved mutation")
	}
	f.r.AttemptID = f.now.Format("20060102t150405z") + "-ffffffffffffffff"
	f.workID = alignmentTestWork(t, f.owner.home, f.now)
	if _, e = f.owner.Plan(f.r, f.workID, "fixture"); e == nil {
		t.Fatal("unknown effect admitted blind retry")
	}
}
