package work

import (
	"encoding/json"
	"os"
	"testing"
)

func TestTerminalCloseProofAuthenticatesCurrentNativeClose(t *testing.T) {
	s := newStore(t)
	w := atomicCloseWork(t, s)
	if _, err := s.ReadTerminalCloseProof(w.ID); err == nil {
		t.Fatal("nonterminal work acquired a close proof")
	}
	closed, err := s.Close(w.ID, atomicCloseInput(), fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := s.ReadTerminalCloseProof(w.ID)
	if err != nil || proof.ReceiptID != closed.Receipt.ID || proof.TransitionID != closed.Transition.ID || proof.WorkSHA256 == "" || proof.ReceiptSHA256 == "" || proof.State != "completed" {
		t.Fatalf("wrong authenticated terminal tuple: %+v %v", proof, err)
	}
	// A parseable terminal envelope alone cannot replace the native close event.
	b, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	w = closed.Work
	w.SuccessPredicate = "tampered after close"
	tampered, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.workPath(w.ID), tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadTerminalCloseProof(w.ID); err == nil {
		t.Fatal("unbound terminal envelope accepted")
	}
	if err = os.WriteFile(s.workPath(w.ID), b, 0600); err != nil {
		t.Fatal(err)
	}
}
