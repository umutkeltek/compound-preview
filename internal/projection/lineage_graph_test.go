package projection

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
)

func TestBuildMemoryEgoGraphProjectsLedgerClaims(t *testing.T) {
	home := t.TempDir()
	writeProjectionTestFile(t, filepath.Join(home, "ledger", "claims.jsonl"),
		`{"id":"claim_a","entity":"product:demo","predicate":"has_signal","object":"downloads rose","confidence":0.91,"evidence_refs":["ev_a"]}`+"\n"+
			`{"id":"claim_b","entity":"product:demo","predicate":"has_hypothesis","object":"listing changed","confidence":0.82}`+"\n"+
			`{"id":"claim_c","entity":"other","predicate":"supports","object":"demo"}`+"\n")
	writeProjectionTestFile(t, filepath.Join(home, "ledger", "edges.jsonl"),
		`{"seq":1,"from":"claim_a","to":"claim_c","kind":"supports"}`+"\n")

	graph, err := BuildMemoryEgoGraph(home, "product:demo", 60)
	if err != nil {
		t.Fatal(err)
	}
	if !graph.Found || graph.ResolvedAs != "entity product:demo" {
		t.Fatalf("graph resolution = %+v", graph)
	}
	if len(graph.Nodes) != 3 || len(graph.Links) != 1 {
		t.Fatalf("graph nodes/links = %d/%d %+v", len(graph.Nodes), len(graph.Links), graph)
	}
}

func TestBuildLineageProjectsEvidenceToClaim(t *testing.T) {
	home := t.TempDir()
	writeProjectionTestFile(t, filepath.Join(home, "ledger", "evidence.jsonl"),
		`{"id":"ev_a","source_kind":"observation","source_ref":"obs_a","quote_text":"downloads rose","observed_at":"2026-07-05T00:00:00Z"}`+"\n")
	writeProjectionTestFile(t, filepath.Join(home, "ledger", "claims.jsonl"),
		`{"id":"claim_a","entity":"product:demo","predicate":"has_signal","object":"downloads rose","evidence_refs":["ev_a"],"created_at":"2026-07-05T00:01:00Z"}`+"\n")
	writeProjectionTestFile(t, filepath.Join(home, "ledger", "adjudications.jsonl"), "")

	lineage, err := BuildLineage(home, "ev_a")
	if err != nil {
		t.Fatal(err)
	}
	if !lineage.Found || lineage.ResolvedAs != "evidence ev_a" {
		t.Fatalf("lineage resolution = %+v", lineage)
	}
	if len(lineage.Steps) < 3 {
		t.Fatalf("lineage steps = %+v", lineage.Steps)
	}
	if lineage.Steps[0].Kind != "evidence" || lineage.Steps[1].Kind != "source" || lineage.Steps[2].Kind != "claim" {
		t.Fatalf("lineage order = %+v", lineage.Steps)
	}
}

func TestBuildLineageCandidateUsesCanonicalCustodyAndKeepsAbsentBehavior(t *testing.T) {
	home := t.TempDir()
	const candidateID = "cand_canonical"
	if err := candidates.WriteCandidate(home, candidates.Memory, candidateID, map[string]any{
		"id":         candidateID,
		"entity":     "product:demo",
		"predicate":  "has_signal",
		"object":     "canonical body",
		"created_at": "2026-08-30T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	lineage, err := BuildLineage(home, candidateID)
	if err != nil {
		t.Fatal(err)
	}
	if !lineage.Found || len(lineage.Steps) != 1 || lineage.Steps[0].Label != candidateID {
		t.Fatalf("canonical candidate lineage = %+v", lineage)
	}

	absent, err := BuildLineage(home, "cand_absent")
	if err != nil {
		t.Fatal(err)
	}
	if absent.Found || len(absent.Steps) != 0 {
		t.Fatalf("canonical absent candidate lineage = %+v", absent)
	}
}

func TestReadCandidateProjectionRejectsUnsafeCandidateCustody(t *testing.T) {
	home := t.TempDir()
	inbox := filepath.Join(home, candidates.InboxDir(candidates.Memory))
	writeProjectionTestFile(t, filepath.Join(inbox, "cand_blank.json"), `{"id":""}`)
	writeProjectionTestFile(t, filepath.Join(inbox, "cand_mismatch.json"), `{"id":"cand_other"}`)
	writeProjectionTestFile(t, filepath.Join(home, "outside.json"), `{"id":"cand_symlink"}`)
	if err := os.Symlink(filepath.Join(home, "outside.json"), filepath.Join(inbox, "cand_symlink.json")); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name     string
		id       string
		wantCode candidates.CorruptionCode
		wantText string
	}{
		{name: "traversal", id: "cand_../escape", wantText: "candidate id must be a bare filename"},
		{name: "slash", id: "cand_/escape", wantText: "candidate id must be a bare filename"},
		{name: "absolute", id: "/tmp/cand_escape", wantText: "candidate id must be a bare filename"},
		{name: "symlink", id: "cand_symlink", wantCode: candidates.CorruptionSymlink},
		{name: "blank identity", id: "cand_blank", wantCode: candidates.CorruptionBlankID},
		{name: "mismatched identity", id: "cand_mismatch", wantCode: candidates.CorruptionIDMismatch},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readCandidateProjection(home, tt.id)
			if err == nil {
				t.Fatal("readCandidateProjection succeeded for unsafe candidate")
			}
			if tt.wantCode != "" {
				var corruption *candidates.Corruption
				if !errors.As(err, &corruption) || corruption.Code != tt.wantCode {
					t.Fatalf("error = %v, corruption code = %v, want %q", err, corruption, tt.wantCode)
				}
			}
			if tt.wantText != "" && !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("error = %q, want %q", err, tt.wantText)
			}
		})
	}
}

func writeProjectionTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
