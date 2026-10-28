package work

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	homepkg "compound/internal/home"
)

func TestPrivateRuntimeDeliveryAdvanceRecoveryRequiresExactRequest(t *testing.T) {
	for _, kind := range []string{"ordinary pending", "different binding", "exact binding", "existing binding", "ordinary after existing binding"} {
		t.Run(kind, func(t *testing.T) {
			f := newPrivateDeliveryFixture(t)
			if kind == "existing binding" || kind == "ordinary after existing binding" {
				bound, err := f.s.Advance(f.w.ID, f.bindingInput(), fixedNow.Add(4*time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				f.w = bound
			}
			initial := f.bindingInput()
			if kind == "ordinary pending" || kind == "ordinary after existing binding" {
				initial.NonSourceDelivery = nil
			}
			previous := advanceBeforeWorkCommit
			t.Cleanup(func() { advanceBeforeWorkCommit = previous })
			advanceBeforeWorkCommit = func() { panic("interrupted advance publication") }
			crashed := false
			func() {
				defer func() { crashed = recover() != nil }()
				_, _ = f.s.Advance(f.w.ID, initial, fixedNow.Add(5*time.Minute))
			}()
			advanceBeforeWorkCommit = previous
			if !crashed {
				t.Fatal("advance did not reach publication boundary")
			}
			currentBefore, err := os.ReadFile(f.s.workPath(f.w.ID))
			if err != nil {
				t.Fatal(err)
			}
			pendingBefore, err := os.ReadFile(f.s.advancePendingPath(f.w.ID))
			if err != nil {
				t.Fatal(err)
			}
			retry := f.bindingInput()
			if kind == "different binding" {
				body, err := f.artifacts.GetPrivate(f.ref)
				if err != nil {
					t.Fatal(err)
				}
				ref, err := f.artifacts.PutPrivate(append(body, '\n'))
				if err != nil {
					t.Fatal(err)
				}
				retry.NonSourceDelivery.EvidenceRef = ref
			}
			result, err := f.s.Advance(f.w.ID, retry, fixedNow.Add(6*time.Minute))
			if kind == "exact binding" || kind == "existing binding" {
				if err != nil || len(result.NonSourceDelivery) != 1 || result.NonSourceDelivery[0].EvidenceRef != f.ref {
					t.Fatalf("exact binding recovery: %+v, %v", result.NonSourceDelivery, err)
				}
				if _, err := os.Stat(f.s.advancePendingPath(f.w.ID)); !os.IsNotExist(err) {
					t.Fatalf("pending publication not consumed: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("pending advance satisfied a different binding request")
			}
			currentAfter, readErr := os.ReadFile(f.s.workPath(f.w.ID))
			if readErr != nil || !bytes.Equal(currentBefore, currentAfter) {
				t.Fatal("refusal changed current work")
			}
			pendingAfter, readErr := os.ReadFile(f.s.advancePendingPath(f.w.ID))
			if readErr != nil || !bytes.Equal(pendingBefore, pendingAfter) {
				t.Fatal("refusal changed pending work")
			}
		})
	}
}

func TestPrivateRuntimeDeliveryCloseRecoveryRevalidatesBeforeTerminalAuthority(t *testing.T) {
	for _, kind := range []string{"unchanged", "missing evidence", "changed live body", "already terminal"} {
		t.Run(kind, func(t *testing.T) {
			f := newPrivateDeliveryFixture(t)
			if _, err := f.s.Advance(f.w.ID, f.bindingInput(), fixedNow.Add(5*time.Minute)); err != nil {
				t.Fatal(err)
			}
			input := CloseInput{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair", Changed: true, Summary: "one frozen private delivery close", ProofRefs: []string{f.ref}}
			hook := &closeAfterFirstEnvelopeWrite
			if kind == "already terminal" {
				hook = &closeBeforeLeaseRelease
			}
			previous := *hook
			t.Cleanup(func() { *hook = previous })
			*hook = func() { panic("private delivery close interrupted") }
			crashed := false
			func() {
				defer func() {
					if recover() != nil {
						crashed = true
					}
				}()
				_, _ = f.s.Close(f.w.ID, input, fixedNow.Add(6*time.Minute))
			}()
			*hook = previous
			if !crashed {
				t.Fatal("close did not reach controlled interruption")
			}
			pending, present, err := f.s.loadClosePending(f.w.ID)
			if err != nil || !present {
				t.Fatalf("frozen close missing: %v", err)
			}
			switch kind {
			case "missing evidence":
				digest, err := homepkg.ArtifactHex(f.ref)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(f.s.home, "private/artifacts/sha256", digest)); err != nil {
					t.Fatal(err)
				}
			case "changed live body", "already terminal":
				writeDeliveryFile(t, f.repo, f.evidence.Files[0].Path, "later legitimate edit")
			}
			result, err := f.s.Close(f.w.ID, input, fixedNow.Add(7*time.Minute))
			if kind == "unchanged" || kind == "already terminal" {
				if err != nil || result.Work.State != "completed" || result.Receipt.ID != pending.Receipt.ID {
					t.Fatalf("exact frozen recovery failed: result=%+v err=%v", result, err)
				}
				return
			}
			if err == nil {
				t.Fatal("pending close accepted changed or missing non-source evidence")
			}
			current, getErr := f.s.Get(f.w.ID)
			if getErr != nil || current.State == "completed" {
				t.Fatalf("failed recovery published terminal authority: %+v %v", current, getErr)
			}
			if _, readErr := f.s.events.Read(pending.Receipt.ID); readErr == nil {
				t.Fatal("failed recovery published a successful receipt")
			}
		})
	}
}
