package work

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePacketMarkdownExtractsWorkFields(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "work", "goal-packet.md"))
	if err != nil {
		t.Fatal(err)
	}
	packet, err := ParsePacketMarkdown(data, "goal-packet.md")
	if err != nil {
		t.Fatal(err)
	}
	if packet.Title != "Compound Goal Packet: Capability Intake" {
		t.Fatalf("title = %q", packet.Title)
	}
	if packet.Owner != "compound-core" {
		t.Fatalf("owner = %q", packet.Owner)
	}
	if !strings.Contains(packet.Objective, "capability intake path") {
		t.Fatalf("objective = %q", packet.Objective)
	}
	if !strings.Contains(packet.SuccessPredicate, "state-delta receipt") {
		t.Fatalf("success predicate = %q", packet.SuccessPredicate)
	}
	if len(packet.NonGoals) != 3 || !strings.Contains(packet.NonGoals[0], "source packet") {
		t.Fatalf("non goals = %#v", packet.NonGoals)
	}
	if !strings.HasPrefix(packet.EvidenceRef, packetEvidencePrefix) {
		t.Fatalf("evidence ref = %q", packet.EvidenceRef)
	}
}

func TestParsePacketMarkdownExtractsProofMedium(t *testing.T) {
	t.Parallel()
	packet, err := ParsePacketMarkdown([]byte(`# Visual packet
Date: 2026-07-02
Status: ready
Owner: codex
Proof-medium: web-ui
Success: close requires screenshot and design review proof.

## Objective

Build a visual surface.
`), "visual.md")
	if err != nil {
		t.Fatal(err)
	}
	if packet.ProofMedium != "web-ui" {
		t.Fatalf("proof medium = %q, want web-ui", packet.ProofMedium)
	}
}

func TestImportPacketCreatesIdempotentProposedWork(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	packetPath := filepath.Join("..", "..", "fixtures", "work", "goal-packet.md")
	report, err := store.ImportPacket(packetPath, PacketImportOptions{}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Created {
		t.Fatal("first import should create work")
	}
	if report.Work.State != "proposed" {
		t.Fatalf("state = %q, want proposed", report.Work.State)
	}
	if report.Work.Owner != "compound-core" {
		t.Fatalf("owner = %q", report.Work.Owner)
	}
	if report.Work.Authority.SourceClass != "source" ||
		!strings.HasPrefix(report.Work.Authority.SourceRevision, "sha256:") {
		t.Fatalf("authority = %+v", report.Work.Authority)
	}
	if report.Work.Uncertainty.Level != "inferred" {
		t.Fatalf("uncertainty = %+v", report.Work.Uncertainty)
	}
	if report.Work.CanonicalSurface != "packet:compound-goal-packet-capability-intake" {
		t.Fatalf("canonical surface = %q", report.Work.CanonicalSurface)
	}
	if len(report.Work.EvidenceRefs) != 1 || report.Work.EvidenceRefs[0] != report.EvidenceRef {
		t.Fatalf("evidence refs = %v report=%q", report.Work.EvidenceRefs, report.EvidenceRef)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("packet import must not create claims ledger, stat err=%v", err)
	}

	copyPath := filepath.Join(t.TempDir(), "renamed-packet.md")
	data, err := os.ReadFile(packetPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	reimport, err := store.ImportPacket(copyPath, PacketImportOptions{}, fixedNow.Add(1))
	if err != nil {
		t.Fatal(err)
	}
	if reimport.Created {
		t.Fatal("same packet content should reuse existing work")
	}
	if reimport.Work.ID != report.Work.ID {
		t.Fatalf("work id changed across path move: %q vs %q", reimport.Work.ID, report.Work.ID)
	}
	_, events, err := store.Show(report.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("idempotent reimport should not append lifecycle events: %+v", events)
	}
}

func TestImportPacketBlockedByPolicyDoesNotCreateWork(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	policyYAML := []byte(`class_outcome:
  local_reversible:
    outcome: hold
`)
	if err := os.WriteFile(filepath.Join(home, "config", "policy.yaml"), policyYAML, 0o644); err != nil {
		t.Fatal(err)
	}
	packetPath := filepath.Join("..", "..", "fixtures", "work", "goal-packet.md")
	if _, err := store.ImportPacket(packetPath, PacketImportOptions{Actor: "codex"}, fixedNow); err == nil {
		t.Fatal("packet import should be blocked by policy hold")
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("blocked packet import wrote work entries: %+v", list)
	}
}

func TestImportedPacketCanDriveReceiptTransition(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	packetPath := filepath.Join("..", "..", "fixtures", "work", "goal-packet.md")
	writeContextPack(t, store, "ctx_packet")
	report, err := store.ImportPacket(packetPath, PacketImportOptions{
		State:         "ready",
		ContextPackID: "ctx_packet",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	workID := report.Work.ID
	if report.Work.State != "ready" {
		t.Fatalf("state = %q, want ready", report.Work.State)
	}
	if !contains(report.Work.EvidenceRefs, report.EvidenceRef) {
		t.Fatalf("packet evidence ref missing from work: %v", report.Work.EvidenceRefs)
	}
	claimed, err := store.Claim(workID, "codex", fixedNow.Add(time.Second))
	if err != nil {
		t.Fatalf("claim packet work: %v", err)
	}
	if len(claimed.RunAttemptIDs) != 1 {
		t.Fatalf("claim RunAttemptIDs = %v, want one reservation", claimed.RunAttemptIDs)
	}
	packetRunAttemptID := claimed.RunAttemptIDs[0]
	if _, err := store.Advance(workID, AdvanceInput{To: "running", RunAttemptIDs: []string{packetRunAttemptID}, Note: "executing packet work"}, fixedNow.Add(2*time.Second)); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if _, err := store.Advance(workID, AdvanceInput{To: "verifying", ArtifactID: "artifact_packet_delta", VerificationID: "ver_packet"}, fixedNow.Add(3*time.Second)); err != nil {
		t.Fatalf("advance verifying: %v", err)
	}
	closed, err := store.Close(workID, CloseInput{
		Outcome:  "completed",
		MoveType: "control_plane_repair",
		Summary:  "packet-sourced work closed through receipt and transition",
		Changed:  true,
	}, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatalf("close packet work: %v", err)
	}
	if closed.Work.ID != workID || closed.Receipt.IDs.WorkID != workID || closed.Transition.IDs.WorkID != workID {
		t.Fatalf("work id chain broken: work=%s receipt=%s transition=%s", closed.Work.ID, closed.Receipt.IDs.WorkID, closed.Transition.IDs.WorkID)
	}
	if closed.Work.IDs.TransitionID != closed.Transition.ID {
		t.Fatalf("work transition id = %q, want %q", closed.Work.IDs.TransitionID, closed.Transition.ID)
	}
	if got := closed.Transition.BeforeRefs; len(got) != 1 || got[0] != "ctx_packet" {
		t.Fatalf("transition before refs = %v, want ctx_packet", got)
	}
	if !contains(closed.Work.RunAttemptIDs, packetRunAttemptID) {
		t.Fatalf("work RunAttemptIDs missing %q, got %v", packetRunAttemptID, closed.Work.RunAttemptIDs)
	}
	if contains(closed.Work.ActionIntentIDs, "run_packet") {
		t.Fatalf("ActionIntentIDs must not contain legacy run id after S1; got %v", closed.Work.ActionIntentIDs)
	}
	if !contains(closed.Transition.AfterRefs, "artifact_packet_delta") ||
		!contains(closed.Transition.AfterRefs, closed.Receipt.ID) {
		t.Fatalf("transition refs incomplete: after=%v receipt=%s", closed.Transition.AfterRefs, closed.Receipt.ID)
	}
	if closed.Receipt.StateDelta.Summary == "" || !closed.Receipt.StateDelta.Changed {
		t.Fatalf("receipt state delta missing: %+v", closed.Receipt.StateDelta)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("closing packet work must not create claims ledger, stat err=%v", err)
	}
}
