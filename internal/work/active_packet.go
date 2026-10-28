package work

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
)

const (
	ActivePacketProofSchemaVersion = "compound.work.active_packet_proof.v0.1"
	ActivePacketProofAuthorityRef  = "compound://work/active-packet-proof"
	DefaultActivePacketSurface     = "packet:compound-goal-packet-adaptive-capability-os"
)

// ActivePacketProofReport is a read-only projection over existing work proof.
// It does not import packets, mutate work, or claim the packet is complete
// unless the WorkEnvelope -> ContextPack -> Receipt -> Transition chain exists.
type ActivePacketProofReport struct {
	SchemaVersion             string          `json:"schema_version"`
	AuthorityRef              string          `json:"authority_ref"`
	Status                    string          `json:"status"`
	GeneratedAt               string          `json:"generated_at"`
	CanonicalSurface          string          `json:"canonical_surface"`
	Source                    string          `json:"source"`
	WorkID                    string          `json:"work_id,omitempty"`
	State                     string          `json:"state,omitempty"`
	ContextPackID             string          `json:"context_pack_id,omitempty"`
	ReceiptID                 string          `json:"receipt_id,omitempty"`
	TransitionID              string          `json:"transition_id,omitempty"`
	PacketEvidenceRefs        []string        `json:"packet_evidence_refs,omitempty"`
	ExpectedPacketEvidenceRef string          `json:"expected_packet_evidence_ref,omitempty"`
	VerificationIDs           []string        `json:"verification_ids,omitempty"`
	ProofChain                []string        `json:"proof_chain,omitempty"`
	Checks                    map[string]bool `json:"checks"`
	Integrity                 []string        `json:"integrity,omitempty"`
	NextAction                string          `json:"next_action"`
	ProofBoundary             string          `json:"proof_boundary"`
}

func (s *Store) ActivePacketProof(now time.Time, surface string, expectedPacketEvidenceRef ...string) (ActivePacketProofReport, error) {
	surface = strings.TrimSpace(surface)
	if surface == "" {
		surface = DefaultActivePacketSurface
	}
	expectedRef := ""
	if len(expectedPacketEvidenceRef) > 0 {
		expectedRef = strings.TrimSpace(expectedPacketEvidenceRef[0])
	}
	report := ActivePacketProofReport{
		SchemaVersion:             ActivePacketProofSchemaVersion,
		AuthorityRef:              ActivePacketProofAuthorityRef,
		Status:                    "missing",
		GeneratedAt:               now.UTC().Format(time.RFC3339),
		CanonicalSurface:          surface,
		Source:                    "compound_work_proof",
		ExpectedPacketEvidenceRef: expectedRef,
		Checks:                    map[string]bool{},
		NextAction:                "import and close active packet through Compound WorkEnvelope, Receipt, and Transition",
		ProofBoundary:             "WorkEnvelope -> ContextPack -> Receipt -> Transition; packet markdown is source evidence/fallback",
	}

	works, err := s.ListWithArchived()
	if err != nil {
		return report, err
	}
	var candidates []envelope.WorkEnvelope
	for _, w := range works {
		if w.CanonicalSurface == surface {
			candidates = append(candidates, w)
		}
	}
	if len(candidates) == 0 {
		return report, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].CreatedAt != candidates[j].CreatedAt {
			return candidates[i].CreatedAt > candidates[j].CreatedAt
		}
		return candidates[i].ID > candidates[j].ID
	})

	w := candidates[0]
	projection := WorkbenchReport{}
	events, err := s.eventsFor(w.ID)
	if err != nil {
		return report, err
	}
	item := s.workbenchItem(w, events, &projection, holdEvidenceIndex{}, now)

	report.Status = "risk"
	report.WorkID = item.WorkID
	report.State = item.State
	report.ContextPackID = item.ContextPackID
	report.ReceiptID = item.ReceiptID
	report.TransitionID = item.TransitionID
	report.PacketEvidenceRefs = packetEvidenceRefs(item.EvidenceRefs)
	report.VerificationIDs = append([]string{}, item.VerificationIDs...)
	report.Integrity = integrityForWork(projection.Integrity, item.WorkID)
	report.Checks = map[string]bool{
		"work_surface_matches":    w.CanonicalSurface == surface,
		"work_completed":          w.State == "completed",
		"context_pack_exists":     s.contextPackExists(item.ContextPackID),
		"receipt_exists":          item.ReceiptID != "",
		"receipt_work_matches":    s.receiptWorkMatches(item.ReceiptID, item.WorkID),
		"transition_exists":       item.TransitionID != "",
		"transition_work_matches": s.transitionWorkMatches(item.TransitionID, item.WorkID),
	}
	if expectedRef != "" {
		report.Checks["source_packet_matches"] = packetRefsContain(report.PacketEvidenceRefs, expectedRef)
	}
	report.ProofChain = compactStrings(item.WorkID, item.ContextPackID, item.ReceiptID, item.TransitionID)
	if allChecks(report.Checks) && len(report.Integrity) == 0 {
		report.Status = "ok"
		report.NextAction = "served readers can consume Compound active-packet proof; keep packet markdown as source/fallback only"
		return report, nil
	}
	report.NextAction = "repair Compound active-packet proof chain before retiring pointer authority"
	return report, nil
}

func (s *Store) contextPackExists(id string) bool {
	if strings.TrimSpace(id) == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(s.home, "context", "packs", id+".json"))
	return err == nil
}

func (s *Store) receiptWorkMatches(id, workID string) bool {
	if strings.TrimSpace(id) == "" {
		return false
	}
	var rec envelope.Receipt
	if err := s.readEventJSON(id, &rec); err != nil {
		return false
	}
	return rec.IDs.WorkID == workID && rec.Validate() == nil
}

func (s *Store) transitionWorkMatches(id, workID string) bool {
	if strings.TrimSpace(id) == "" {
		return false
	}
	var tr envelope.Transition
	if err := s.readEventJSON(id, &tr); err != nil {
		return false
	}
	return tr.IDs.WorkID == workID && tr.Validate() == nil
}

func packetEvidenceRefs(refs []string) []string {
	var out []string
	for _, ref := range refs {
		if strings.HasPrefix(ref, packetEvidencePrefix) {
			out = append(out, ref)
		}
	}
	return out
}

func packetRefsContain(refs []string, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	for _, ref := range refs {
		if strings.TrimSpace(ref) == want {
			return true
		}
	}
	return false
}

func integrityForWork(integrity []string, workID string) []string {
	var out []string
	prefix := workID + ":"
	for _, item := range integrity {
		if strings.HasPrefix(item, prefix) {
			out = append(out, item)
		}
	}
	return out
}

func allChecks(checks map[string]bool) bool {
	for _, ok := range checks {
		if !ok {
			return false
		}
	}
	return true
}

func compactStrings(values ...string) []string {
	var out []string
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}
