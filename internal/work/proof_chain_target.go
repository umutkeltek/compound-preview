package work

// ProofChainForWork applies the canonical terminal proof-chain checks to one
// archive-resolvable WorkEnvelope. It deliberately excludes the global orphan
// scan so consumers validating a named work are not coupled to unrelated
// historical findings. Callers that need a multi-read atomic snapshot must hold
// the COMPOUND_HOME autonomy-admission lock around this method and adjacent reads.
func (s *Store) ProofChainForWork(workID string) (ProofChainDoctorItem, error) {
	w, err := s.Get(workID)
	if err != nil {
		return ProofChainDoctorItem{}, err
	}
	report := ProofChainDoctorReport{}
	// Single-work caller: read this one work's lifecycle events directly (the
	// batch eventsByWorkID seam is for the whole-ledger ProofChainDoctor). This
	// preserves proofChainItem's former internal eventsFor read and its
	// events_unreadable degradation exactly.
	events, err := s.eventsFor(w.ID)
	if err != nil {
		item := ProofChainDoctorItem{WorkID: w.ID, State: w.State}
		addProofChainProblem(&report, &item, "events_unreadable", "lifecycle events unreadable: "+err.Error())
		return item, nil
	}
	item := s.proofChainItem(w, events, &report)
	if len(item.Problems) == 0 {
		item.ProofComplete = true
	}
	return item, nil
}
