package work

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
)

func TestActivePacketProofReportsClosedPacketChain(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	packetPath := filepath.Join("..", "..", "fixtures", "work", "goal-packet.md")
	writeContextPack(t, s, "ctx_packet")
	imported, err := s.ImportPacket(packetPath, PacketImportOptions{
		Owner:         "compound-core",
		State:         "ready",
		ContextPackID: "ctx_packet",
	}, fixedNow)
	if err != nil {
		t.Fatalf("import packet: %v", err)
	}
	if _, err := s.Claim(imported.Work.ID, "codex", fixedNow.Add(time.Second)); err != nil {
		t.Fatalf("claim packet work: %v", err)
	}
	if _, err := s.Advance(imported.Work.ID, AdvanceInput{To: "running"}, fixedNow.Add(2*time.Second)); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if _, err := s.Advance(imported.Work.ID, AdvanceInput{To: "verifying", VerificationID: "packet-work-contract:active-packet-lifecycle"}, fixedNow.Add(3*time.Second)); err != nil {
		t.Fatalf("advance verifying: %v", err)
	}
	closed, err := s.Close(imported.Work.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "active packet lifecycle proof complete",
		Changed:   true,
		ProofRefs: []string{"packet-work-contract:active-packet-lifecycle"},
	}, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatalf("close packet work: %v", err)
	}

	report, err := s.ActivePacketProof(fixedNow.Add(5*time.Second), imported.Work.CanonicalSurface)
	if err != nil {
		t.Fatalf("active packet proof: %v", err)
	}
	if report.Status != "ok" || report.WorkID != closed.Work.ID || report.ContextPackID != "ctx_packet" || report.ReceiptID != closed.Receipt.ID || report.TransitionID != closed.Transition.ID {
		t.Fatalf("proof report missing closed chain: %+v", report)
	}
	for name, ok := range report.Checks {
		if !ok {
			t.Fatalf("check %s = false in %+v", name, report)
		}
	}
	if len(report.PacketEvidenceRefs) != 1 || len(report.ProofChain) != 4 || report.NextAction == "" {
		t.Fatalf("proof report missing evidence/action: %+v", report)
	}
}

func TestActivePacketProofReportsArchivedClosedPacketChain(t *testing.T) {
	t.Parallel()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", fixedNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	s, err := Open(homePath)
	if err != nil {
		t.Fatalf("open initialized store: %v", err)
	}
	packetPath := filepath.Join("..", "..", "fixtures", "work", "goal-packet.md")
	writeContextPack(t, s, "ctx_packet")
	imported, err := s.ImportPacket(packetPath, PacketImportOptions{
		Owner:         "compound-core",
		State:         "ready",
		ContextPackID: "ctx_packet",
	}, fixedNow)
	if err != nil {
		t.Fatalf("import packet: %v", err)
	}
	if _, err := s.Claim(imported.Work.ID, "codex", fixedNow.Add(time.Second)); err != nil {
		t.Fatalf("claim packet work: %v", err)
	}
	if _, err := s.Advance(imported.Work.ID, AdvanceInput{To: "running"}, fixedNow.Add(2*time.Second)); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if _, err := s.Advance(imported.Work.ID, AdvanceInput{To: "verifying", VerificationID: "packet-work-contract:active-packet-lifecycle"}, fixedNow.Add(3*time.Second)); err != nil {
		t.Fatalf("advance verifying: %v", err)
	}
	closed, err := s.Close(imported.Work.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "active packet lifecycle proof complete",
		Changed:   true,
		ProofRefs: []string{"packet-work-contract:active-packet-lifecycle"},
	}, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatalf("close packet work: %v", err)
	}
	if _, err := home.ArchiveOperationalHistory(s.home, home.OperationalArchiveOptions{Kinds: []string{"work"}}, fixedNow.Add(5*time.Second)); err != nil {
		t.Fatalf("archive terminal work: %v", err)
	}

	report, err := s.ActivePacketProof(fixedNow.Add(6*time.Second), imported.Work.CanonicalSurface)
	if err != nil {
		t.Fatalf("active packet proof: %v", err)
	}
	if report.Status != "ok" {
		t.Fatalf("status = %q, want ok; report = %+v", report.Status, report)
	}
	if report.WorkID != closed.Work.ID {
		t.Fatalf("work id = %q, want %q", report.WorkID, closed.Work.ID)
	}
	if report.ReceiptID != closed.Receipt.ID || report.TransitionID != closed.Transition.ID {
		t.Fatalf("proof ids receipt=%q transition=%q, want %q %q", report.ReceiptID, report.TransitionID, closed.Receipt.ID, closed.Transition.ID)
	}
}

func TestActivePacketProofRisksIncompleteChain(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	packetPath := filepath.Join("..", "..", "fixtures", "work", "goal-packet.md")
	imported, err := s.ImportPacket(packetPath, PacketImportOptions{Owner: "compound-core", State: "proposed"}, fixedNow)
	if err != nil {
		t.Fatalf("import packet: %v", err)
	}
	report, err := s.ActivePacketProof(fixedNow.Add(time.Second), imported.Work.CanonicalSurface)
	if err != nil {
		t.Fatalf("active packet proof: %v", err)
	}
	if report.Status != "risk" || report.WorkID != imported.Work.ID {
		t.Fatalf("incomplete chain should be risk: %+v", report)
	}
	if report.Checks["work_completed"] || report.Checks["receipt_exists"] || report.Checks["transition_exists"] || report.Checks["context_pack_exists"] {
		t.Fatalf("incomplete checks should remain false: %+v", report.Checks)
	}
}

func writeContextPack(t *testing.T, s *Store, id string) {
	writeContextPackWithContractID(t, s, id, "")
}

func writeContextPackWithContractID(t *testing.T, s *Store, id, contractID string) {
	t.Helper()
	dir := filepath.Join(s.home, "context", "packs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir context packs: %v", err)
	}
	pack := envelope.ContextPack{
		Header: envelope.Header{
			SchemaVersion: envelope.SVContextPack,
			ID:            id,
			IDs:           envelope.IDs{ContractID: contractID},
			CreatedAt:     fixedNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "work-test"},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		SummaryMD:          "test context pack",
		Included:           []envelope.IncludedRef{},
		Excluded:           []envelope.ExcludedRef{},
		ActiveInstructions: []string{},
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		RequiredChecks:     []string{},
		Avoid:              []string{},
	}
	if err := s.writeJSON(filepath.Join(dir, id+".json"), &pack); err != nil {
		t.Fatalf("write context pack: %v", err)
	}
}

func writeEffectiveContract(t *testing.T, s *Store, id string) {
	t.Helper()
	ec := envelope.EffectiveContract{
		Header: envelope.Header{
			SchemaVersion: envelope.SVEffectiveContract,
			ID:            id,
			CreatedAt:     fixedNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "work-test"},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Harness:            "codex",
		ActiveInstructions: []string{},
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		Excluded:           []envelope.ExcludedRef{},
		MemoryPolicy:       envelope.MemoryPolicy{},
		CapabilityPolicy:   envelope.CapabilityPolicy{},
		MutationPolicy:     envelope.MutationPolicy{RequiresActionIntent: true, IrreversibleRequiresHuman: true},
		SourceRevisions:    []envelope.SourceRev{},
		Unknowns:           []string{},
		Conflicts:          []string{},
	}
	if err := ec.Validate(); err != nil {
		t.Fatalf("effective contract fixture invalid: %v", err)
	}
	dir := filepath.Join(s.home, "contracts", "effective", fixedNow.UTC().Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir effective contracts: %v", err)
	}
	if err := s.writeJSON(filepath.Join(dir, id+".json"), &ec); err != nil {
		t.Fatalf("write effective contract: %v", err)
	}
}
