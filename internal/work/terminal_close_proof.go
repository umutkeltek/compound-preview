package work

import "fmt"

// TerminalCloseProof authenticates the current terminal envelope against its
// native closing action, policy, receipt, transition and lifecycle event. It is
// a read-only proof seam; callers cannot reopen work or mint a closing receipt.
type TerminalCloseProof struct {
	WorkID        string `json:"work_id"`
	State         string `json:"state"`
	WorkSHA256    string `json:"work_sha256"`
	ReceiptID     string `json:"receipt_id"`
	ReceiptSHA256 string `json:"receipt_sha256"`
	TransitionID  string `json:"transition_id"`
}

func (s *Store) ReadTerminalCloseProof(workID string) (TerminalCloseProof, error) {
	w, err := s.Get(workID)
	if err != nil {
		return TerminalCloseProof{}, err
	}
	if !terminalStates[w.State] {
		return TerminalCloseProof{}, fmt.Errorf("work %s is not terminal", workID)
	}
	close, err := s.loadExactTerminalClose(w)
	if err != nil {
		return TerminalCloseProof{}, err
	}
	return TerminalCloseProof{WorkID: w.ID, State: w.State, WorkSHA256: close.WorkSHA256,
		ReceiptID: close.Receipt.ID, ReceiptSHA256: close.Event.ReceiptSHA256,
		TransitionID: close.Transition.ID}, nil
}
