package authority

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
)

const (
	testMemorySourceRef       = "/synthetic/brain/projects/compound.md"
	testMemoryKnowledgePageID = "knowledge_memory_ledger_consumed"
	testMemoryContextPackID   = "ctx_memory_ledger_consumed"
	testMemoryContractID      = "contract_memory_ledger_consumed"
	testMemoryClaimID         = "claim_memory_ledger_proof"
)

func TestOSAbsorptionDoctorMemoryLedgerAuthorityBindsConsumedContextPack(t *testing.T) {
	const (
		sourceRef       = "/synthetic/brain/projects/compound.md"
		knowledgePageID = "knowledge_memory_ledger_consumed"
		contextPackID   = "ctx_memory_ledger_consumed"
		contractID      = "contract_memory_ledger_consumed"
		claimID         = "claim_memory_ledger_proof"
	)
	homePath := t.TempDir()
	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	memoryRow := row("os_source_brain_project_compound", sourceRef, "absorbed", "compound://ledger/memory", "compound orient", osAbsorptionProofModeMemoryLedger)
	memoryRow.EvidenceRefs = []string{
		"knowledge_page:" + knowledgePageID,
		"context_pack:" + contextPackID,
	}
	writeJSONFixture(t, matrixPath, OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows:          []OSAbsorptionMatrixRow{memoryRow},
	})
	if _, err := ImportOSAbsorptionMatrix(homePath, matrixPath, time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("import matrix: %v", err)
	}
	evidenceID := writeMemoryLedgerEvidenceFixture(t, homePath, knowledgePageID)
	writeMemoryLedgerClaimFixture(t, homePath, evidenceID, knowledgePageID, candidates.BasisCuratedPage, sourceRef)
	writeMemoryLedgerConsumedContextPack(t, homePath, contextPackID, contractID, claimID)

	report, err := OSAbsorptionDoctor(homePath)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	got := report.Rows[0]
	if got.ProofStatus != "ok" {
		t.Fatalf("memory-ledger proof should be machine-proven: %+v", got)
	}
	for _, want := range []string{
		"claim=" + claimID,
		"evidence=" + evidenceID,
		"context_pack=" + contextPackID,
		"contract=" + contractID,
		"memory_rev=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	} {
		if !strings.Contains(got.ProofRef, want) {
			t.Fatalf("memory-ledger proof_ref = %q, want %q", got.ProofRef, want)
		}
	}
}

func TestOSAbsorptionDoctorMemoryLedgerAuthorityFailsClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	tests := []struct {
		name       string
		options    memoryLedgerAuthorityTestOptions
		wantDetail string
	}{
		{
			name: "missing ContextPack ref",
			options: memoryLedgerAuthorityTestOptions{mutateRow: func(row *OSAbsorptionMatrixRow) {
				row.EvidenceRefs = []string{"knowledge_page:" + testMemoryKnowledgePageID}
			}},
			wantDetail: "missing ContextPack ref",
		},
		{
			name: "invalid ContextPack ref",
			options: memoryLedgerAuthorityTestOptions{mutateRow: func(row *OSAbsorptionMatrixRow) {
				row.EvidenceRefs[1] = "context_pack:not/a/context"
			}},
			wantDetail: "invalid ContextPack id",
		},
		{
			name: "duplicate ContextPack ref",
			options: memoryLedgerAuthorityTestOptions{mutateRow: func(row *OSAbsorptionMatrixRow) {
				row.EvidenceRefs = append(row.EvidenceRefs, "context_pack:ctx_memory_ledger_other")
			}},
			wantDetail: "duplicate ContextPack ref",
		},
		{
			name:       "missing persisted ContextPack",
			options:    memoryLedgerAuthorityTestOptions{skipPack: true},
			wantDetail: "ContextPack " + testMemoryContextPackID,
		},
		{
			name: "missing task intent",
			options: memoryLedgerAuthorityTestOptions{context: memoryLedgerContextFixtureOptions{mutatePack: func(pack *envelope.ContextPack) {
				pack.TaskContract.Intent = ""
			}}},
			wantDetail: "missing task intent",
		},
		{
			name: "missing persisted EffectiveContract",
			options: memoryLedgerAuthorityTestOptions{context: memoryLedgerContextFixtureOptions{
				skipContract: true,
			}},
			wantDetail: "EffectiveContract " + testMemoryContractID,
		},
		{
			name: "contract task differs from pack",
			options: memoryLedgerAuthorityTestOptions{context: memoryLedgerContextFixtureOptions{mutateContract: func(contract *envelope.EffectiveContract) {
				contract.TaskContract.Intent = "different task"
			}}},
			wantDetail: "task contract differs",
		},
		{
			name: "contract omits selected claim",
			options: memoryLedgerAuthorityTestOptions{context: memoryLedgerContextFixtureOptions{mutateContract: func(contract *envelope.EffectiveContract) {
				contract.ActiveMemory = nil
			}}},
			wantDetail: "missing selected claim",
		},
		{
			name: "contract memory revision differs",
			options: memoryLedgerAuthorityTestOptions{context: memoryLedgerContextFixtureOptions{mutateContract: func(contract *envelope.EffectiveContract) {
				contract.SourceRevisions[0].Rev = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			}}},
			wantDetail: "memory revision",
		},
		{
			name: "private row cannot use public pack and contract",
			options: memoryLedgerAuthorityTestOptions{context: memoryLedgerContextFixtureOptions{mutatePack: func(pack *envelope.ContextPack) {
				pack.Privacy = "public"
			}}},
			wantDetail: "privacy",
		},
		{
			name: "pack is not current runtime authority",
			options: memoryLedgerAuthorityTestOptions{context: memoryLedgerContextFixtureOptions{mutatePack: func(pack *envelope.ContextPack) {
				pack.Authority.SourceClass = "generated"
				pack.Authority.Status = "stale"
			}}},
			wantDetail: "runtime/current authority",
		},
		{
			name: "missing memory relevance revision",
			options: memoryLedgerAuthorityTestOptions{context: memoryLedgerContextFixtureOptions{mutatePack: func(pack *envelope.ContextPack) {
				pack.SourceRevisions = nil
			}}},
			wantDetail: "missing known source revision",
		},
		{
			name: "claim appears only in ActiveMemory",
			options: memoryLedgerAuthorityTestOptions{context: memoryLedgerContextFixtureOptions{mutatePack: func(pack *envelope.ContextPack) {
				pack.Included = nil
			}}},
			wantDetail: "not consumed by ContextPack",
		},
		{
			name: "claim appears only in Included",
			options: memoryLedgerAuthorityTestOptions{context: memoryLedgerContextFixtureOptions{mutatePack: func(pack *envelope.ContextPack) {
				pack.ActiveMemory = nil
			}}},
			wantDetail: "not consumed by ContextPack",
		},
		{
			name: "Included claim is stale",
			options: memoryLedgerAuthorityTestOptions{context: memoryLedgerContextFixtureOptions{mutatePack: func(pack *envelope.ContextPack) {
				pack.Included[0].Status = "stale"
			}}},
			wantDetail: "not consumed by ContextPack",
		},
		{
			name:       "claim lacks a valid promotion basis",
			options:    memoryLedgerAuthorityTestOptions{promotionBasis: "unknown_basis"},
			wantDetail: "no durably promoted source-linked claim",
		},
		{
			name: "promoted claim belongs to another source",
			options: memoryLedgerAuthorityTestOptions{
				claimKnowledgePageID: "knowledge_unrelated",
				claimSourceRef:       "/synthetic/brain/projects/unrelated.md",
			},
			wantDetail: "no durably promoted source-linked claim",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := installMemoryLedgerAuthorityTestFixture(t, tt.options)
			if got.ProofStatus != "failed" || got.ProofRef != "" {
				t.Fatalf("memory-ledger authority must fail closed: %+v", got)
			}
			if !strings.Contains(got.ProofDetail, tt.wantDetail) {
				t.Fatalf("proof detail = %q, want %q", got.ProofDetail, tt.wantDetail)
			}
		})
	}
}

func TestOSAbsorptionDoctorMemoryLedgerAuthorityScansPastUnconsumedSourceMatch(t *testing.T) {
	got := installMemoryLedgerAuthorityTestFixture(t, memoryLedgerAuthorityTestOptions{
		beforeDoctor: func(t *testing.T, homePath string) {
			prependMemoryLedgerJSONFixture(t, filepath.Join(homePath, "ledger", "claims.jsonl"), map[string]any{
				"id":              "claim_memory_ledger_earlier",
				"seq":             0,
				"entity":          "system:compound",
				"predicate":       "os_absorption_end_state",
				"object":          "earlier source-linked but unconsumed claim",
				"valid_from":      "2026-07-12T11:59:00Z",
				"confidence":      0.9,
				"derivation":      "deterministic",
				"promotion_basis": candidates.BasisCuratedPage,
				"qualifiers": map[string]string{
					"source":      "knowledge_page",
					"page_id":     testMemoryKnowledgePageID,
					"source_path": testMemorySourceRef,
				},
				"evidence_refs": []string{"cand_memory_ledger_earlier", memoryLedgerEvidenceID(testMemoryKnowledgePageID)},
				"visibility":    "contextual",
				"created_at":    "2026-07-12T11:59:00Z",
			})
		},
	})
	if got.ProofStatus != "ok" || !strings.Contains(got.ProofRef, "claim="+testMemoryClaimID) {
		t.Fatalf("proof must scan past earlier unconsumed source match: %+v", got)
	}
}

type memoryLedgerAuthorityTestOptions struct {
	mutateRow            func(*OSAbsorptionMatrixRow)
	skipPack             bool
	context              memoryLedgerContextFixtureOptions
	promotionBasis       string
	claimKnowledgePageID string
	claimSourceRef       string
	beforeDoctor         func(*testing.T, string)
}

func installMemoryLedgerAuthorityTestFixture(t *testing.T, options memoryLedgerAuthorityTestOptions) OSAbsorptionDoctorRow {
	t.Helper()
	homePath := t.TempDir()
	memoryRow := row("os_source_brain_project_compound", testMemorySourceRef, "absorbed", "compound://ledger/memory", "compound orient", osAbsorptionProofModeMemoryLedger)
	memoryRow.EvidenceRefs = []string{
		"knowledge_page:" + testMemoryKnowledgePageID,
		"context_pack:" + testMemoryContextPackID,
	}
	if options.mutateRow != nil {
		options.mutateRow(&memoryRow)
	}
	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	writeJSONFixture(t, matrixPath, OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows:          []OSAbsorptionMatrixRow{memoryRow},
	})
	if _, err := ImportOSAbsorptionMatrix(homePath, matrixPath, time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("import matrix: %v", err)
	}
	promotionBasis := options.promotionBasis
	if promotionBasis == "" {
		promotionBasis = candidates.BasisCuratedPage
	}
	knowledgePageID := options.claimKnowledgePageID
	if knowledgePageID == "" {
		knowledgePageID = testMemoryKnowledgePageID
	}
	sourceRef := options.claimSourceRef
	if sourceRef == "" {
		sourceRef = testMemorySourceRef
	}
	evidenceID := writeMemoryLedgerEvidenceFixture(t, homePath, knowledgePageID)
	writeMemoryLedgerClaimFixture(t, homePath, evidenceID, knowledgePageID, promotionBasis, sourceRef)
	if !options.skipPack {
		writeMemoryLedgerConsumedContextPack(t, homePath, testMemoryContextPackID, testMemoryContractID, testMemoryClaimID, options.context)
	}
	if options.beforeDoctor != nil {
		options.beforeDoctor(t, homePath)
	}
	report, err := OSAbsorptionDoctor(homePath)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if len(report.Rows) != 1 {
		t.Fatalf("doctor rows = %+v, want one", report.Rows)
	}
	return report.Rows[0]
}

type memoryLedgerContextFixtureOptions struct {
	mutatePack             func(*envelope.ContextPack)
	mutateContract         func(*envelope.EffectiveContract)
	skipContract           bool
	skipContractValidation bool
}

func writeMemoryLedgerConsumedContextPack(t *testing.T, homePath, contextPackID, contractID, claimID string, options ...memoryLedgerContextFixtureOptions) {
	t.Helper()
	var option memoryLedgerContextFixtureOptions
	if len(options) > 0 {
		option = options[0]
	}
	claimRef := "claim://" + claimID
	pack := envelope.ContextPack{
		Header: envelope.Header{
			SchemaVersion: envelope.SVContextPack,
			ID:            contextPackID,
			CreatedAt:     "2026-07-12T12:02:00Z",
			Producer:      envelope.Producer{Kind: "test", Name: "memory-ledger-authority"},
			IDs:           envelope.IDs{ContractID: contractID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		SummaryMD:    "memory-ledger authority fixture",
		TaskContract: envelope.TaskContract{Intent: "consume compound project memory"},
		Included: []envelope.IncludedRef{{
			Ref:         claimRef,
			SourceClass: "runtime",
			Reason:      "task-relevant promoted memory",
			Confidence:  1,
			Status:      "current",
		}},
		Excluded:           []envelope.ExcludedRef{},
		ActiveInstructions: []string{},
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		ActiveMemory:       []string{claimRef},
		SourceRevisions: []envelope.SourceRev{{
			Source: "memory://relevance-gated",
			Rev:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}},
		RequiredChecks: []string{},
		Avoid:          []string{},
	}
	if option.mutatePack != nil {
		option.mutatePack(&pack)
	}
	if err := pack.Validate(); err != nil {
		t.Fatalf("fixture ContextPack invalid: %v", err)
	}
	path := filepath.Join(homePath, "context", "packs", contextPackID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFixture(t, path, pack)
	if option.skipContract {
		return
	}
	contract := envelope.EffectiveContract{
		Header: envelope.Header{
			SchemaVersion: envelope.SVEffectiveContract,
			ID:            contractID,
			CreatedAt:     pack.CreatedAt,
			Producer:      envelope.Producer{Kind: "test", Name: "memory-ledger-authority"},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Harness:            "codex",
		TaskContract:       pack.TaskContract,
		ActiveInstructions: []string{},
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		ActiveMemory:       []string{claimRef},
		Excluded:           []envelope.ExcludedRef{},
		SourceRevisions:    pack.SourceRevisions,
		Unknowns:           []string{},
		Conflicts:          []string{},
	}
	if option.mutateContract != nil {
		option.mutateContract(&contract)
	}
	if !option.skipContractValidation {
		if err := contract.Validate(); err != nil {
			t.Fatalf("fixture EffectiveContract invalid: %v", err)
		}
	}
	contractPath := filepath.Join(homePath, "contracts", "effective", "2026-07-12", contractID+".json")
	if err := os.MkdirAll(filepath.Dir(contractPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFixture(t, contractPath, contract)
}

func TestOSAbsorptionDoctorMemoryLedgerAuthority(t *testing.T) {
	const (
		sourceRef       = "/synthetic/brain/projects/compound.md"
		knowledgePageID = "knowledge_memory_ledger_proof"
	)
	// The evidence id is content-derived (the ledger fold revalidates every
	// row's content address), so it is computed, never hand-named.
	evidenceID, _ := memoryLedgerEvidenceAddress(knowledgePageID)

	tests := []struct {
		name            string
		promotionBasis  string
		rowEvidenceRefs []string
		sourcePath      string
		wantStatus      string
		wantProofRef    bool
	}{
		{
			name:            "promoted claim through evidence intermediary proves row",
			promotionBasis:  candidates.BasisCuratedPage,
			rowEvidenceRefs: []string{"knowledge_page:" + knowledgePageID},
			wantStatus:      "ok",
			wantProofRef:    true,
		},
		{
			name:           "promoted claim through curated source path proves row",
			promotionBasis: candidates.BasisCuratedPage,
			sourcePath:     sourceRef,
			wantStatus:     "ok",
			wantProofRef:   true,
		},
		{
			name:            "basisless claim remains pending",
			rowEvidenceRefs: []string{"evidence:" + evidenceID, "knowledge_page:" + knowledgePageID},
			wantStatus:      "pending",
			wantProofRef:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := t.TempDir()
			now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
			matrixPath := filepath.Join(t.TempDir(), "matrix.json")
			memoryRow := row("os_source_brain_project_compound", sourceRef, "pending", "compound://ledger/memory", "compound memory recall", "")
			memoryRow.EvidenceRefs = tt.rowEvidenceRefs
			writeJSONFixture(t, matrixPath, OSAbsorptionSourceMatrix{
				SchemaVersion: OSAbsorptionMatrixSchema,
				Rows:          []OSAbsorptionMatrixRow{memoryRow},
			})
			if _, err := ImportOSAbsorptionMatrix(homePath, matrixPath, now); err != nil {
				t.Fatalf("import matrix: %v", err)
			}
			writeMemoryLedgerEvidenceFixture(t, homePath, knowledgePageID)
			writeMemoryLedgerClaimFixture(t, homePath, evidenceID, knowledgePageID, tt.promotionBasis, tt.sourcePath)

			report, err := OSAbsorptionDoctor(homePath)
			if err != nil {
				t.Fatalf("doctor: %v", err)
			}
			if len(report.Rows) != 1 {
				t.Fatalf("doctor rows = %+v, want one", report.Rows)
			}
			got := report.Rows[0]
			if got.ProofStatus != tt.wantStatus {
				t.Fatalf("memory-ledger proof_status = %q, want %q; row=%+v", got.ProofStatus, tt.wantStatus, got)
			}
			if (got.ProofRef != "") != tt.wantProofRef {
				t.Fatalf("memory-ledger proof_ref = %q, want non-empty=%t; row=%+v", got.ProofRef, tt.wantProofRef, got)
			}
			if tt.wantProofRef {
				for _, want := range []string{"claim=claim_memory_ledger_proof", "evidence=" + evidenceID} {
					if !strings.Contains(got.ProofRef, want) {
						t.Fatalf("memory-ledger proof_ref = %q, want %q", got.ProofRef, want)
					}
				}
			}
			for _, rel := range []string{"inbox", "events"} {
				if _, err := os.Stat(filepath.Join(homePath, rel)); !os.IsNotExist(err) {
					t.Fatalf("memory-ledger proof mutated %s, stat err=%v", rel, err)
				}
			}
		})
	}
}

func TestOSAbsorptionDoctorMemoryLedgerAuthorityAcceptsHistoricalPromotedClaim(t *testing.T) {
	const (
		sourceRef       = "/synthetic/brain/projects/compound.md"
		knowledgePageID = "knowledge_memory_ledger_historical"
		contextPackID   = "ctx_memory_ledger_historical"
		contractID      = "contract_memory_ledger_historical"
	)
	homePath := t.TempDir()
	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	memoryRow := row("os_source_brain_project_compound", sourceRef, "absorbed", "compound://ledger/memory", "compound orient", osAbsorptionProofModeMemoryLedger)
	memoryRow.EvidenceRefs = []string{
		"knowledge_page:" + knowledgePageID,
		"context_pack:" + contextPackID,
	}
	writeJSONFixture(t, matrixPath, OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows:          []OSAbsorptionMatrixRow{memoryRow},
	})
	if _, err := ImportOSAbsorptionMatrix(homePath, matrixPath, time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("import matrix: %v", err)
	}
	evidenceID := writeMemoryLedgerEvidenceFixture(t, homePath, knowledgePageID)
	writeMemoryLedgerClaimFixture(t, homePath, evidenceID, knowledgePageID, candidates.BasisCuratedPage, sourceRef)
	appendMemoryLedgerJSONFixture(t, filepath.Join(homePath, "ledger", "claims.jsonl"), map[string]any{
		"id":              "claim_memory_ledger_newer_unrelated",
		"seq":             2,
		"entity":          "system:compound",
		"predicate":       "os_absorption_end_state",
		"object":          "newer unrelated source",
		"valid_from":      "2026-07-12T12:01:00Z",
		"confidence":      0.9,
		"derivation":      "deterministic",
		"promotion_basis": candidates.BasisCuratedPage,
		"qualifiers": map[string]string{
			"source":      "knowledge_page",
			"page_id":     "knowledge_unrelated",
			"source_path": "/synthetic/brain/projects/unrelated.md",
		},
		"evidence_refs": []string{"cand_unrelated", "ev_unrelated"},
		"supersedes":    "claim_memory_ledger_proof",
		"visibility":    "contextual",
		"created_at":    "2026-07-12T12:01:00Z",
	})
	appendMemoryLedgerJSONFixture(t, filepath.Join(homePath, "ledger", "edges.jsonl"), map[string]any{
		"seq":  2,
		"kind": "supersedes",
		"from": "claim_memory_ledger_newer_unrelated",
		"to":   "claim_memory_ledger_proof",
	})
	writeMemoryLedgerConsumedContextPack(t, homePath, contextPackID, contractID, "claim_memory_ledger_proof")

	report, err := OSAbsorptionDoctor(homePath)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	got := report.Rows[0]
	if got.ProofStatus != "ok" || !strings.Contains(got.ProofRef, "claim=claim_memory_ledger_proof") || !strings.Contains(got.ProofRef, "evidence="+evidenceID) {
		t.Fatalf("historical promoted claim should prove absorption: %+v", got)
	}
}

func writeMemoryLedgerClaimFixture(t *testing.T, homePath, evidenceID, knowledgePageID, promotionBasis, sourcePath string) {
	t.Helper()
	qualifiers := map[string]string{
		"source":  "knowledge_page",
		"page_id": knowledgePageID,
	}
	if sourcePath != "" {
		qualifiers["source_path"] = sourcePath
	}
	claim := map[string]any{
		"id":              "claim_memory_ledger_proof",
		"seq":             1,
		"entity":          "system:compound",
		"predicate":       "os_absorption_end_state",
		"object":          "compound owns the absorbed memory authority",
		"valid_from":      "2026-07-12T12:00:00Z",
		"confidence":      0.9,
		"derivation":      "deterministic",
		"promotion_basis": promotionBasis,
		"qualifiers":      qualifiers,
		"evidence_refs":   []string{"cand_memory_ledger_proof", evidenceID},
		"visibility":      "contextual",
		"created_at":      "2026-07-12T12:00:00Z",
	}
	body, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(homePath, "ledger", "claims.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// memoryLedgerEvidenceAddress computes the compliant content address for the
// fixture evidence row: the ledger fold revalidates every row (an ev_ id is
// derived from SourceKind + SourceID + QuoteText, never hand-named), so
// fixtures must mint the same way production does (mem.evidenceContentHash).
func memoryLedgerEvidenceAddress(knowledgePageID string) (id, contentHash string) {
	material := "note" + "\x00" + "knowledge_page:" + knowledgePageID + "\x00"
	sum := sha256.Sum256([]byte(material))
	hash := hex.EncodeToString(sum[:])
	return "ev_" + hash[:12], hash
}

// memoryLedgerEvidenceID is the id-only convenience for fixture rows that
// must reference the evidence id before the evidence row itself is written.
func memoryLedgerEvidenceID(knowledgePageID string) string {
	id, _ := memoryLedgerEvidenceAddress(knowledgePageID)
	return id
}

// writeMemoryLedgerEvidenceFixture writes one content-addressed evidence row
// and returns its derived id, so claims' evidence_refs and proof assertions
// bind the exact row the fold will accept.
func writeMemoryLedgerEvidenceFixture(t *testing.T, homePath, knowledgePageID string) string {
	t.Helper()
	evidenceID, contentHash := memoryLedgerEvidenceAddress(knowledgePageID)
	evidence := map[string]any{
		"id":           evidenceID,
		"source_kind":  "note",
		"source_id":    "knowledge_page:" + knowledgePageID,
		"content_hash": contentHash,
		"observed_at":  "2026-07-12T12:00:00Z",
		"visibility":   "contextual",
	}
	body, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(homePath, "ledger", "evidence.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return evidenceID
}

func appendMemoryLedgerJSONFixture(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(body, '\n')); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func prependMemoryLedgerJSONFixture(t *testing.T, path string, value any) {
	t.Helper()
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append(body, '\n'), existing...), 0o644); err != nil {
		t.Fatal(err)
	}
}
