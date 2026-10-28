package work

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
)

func TestWorkbenchRejectsCorruptMemoryCandidateArtifactReferences(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		ref   string
		setup func(t *testing.T, candidateDir string)
	}{
		{
			name: "traversal shaped candidate ref",
			ref:  "cand_review/../../escape",
		},
		{
			name: "slash candidate id",
			ref:  "cand_review/child",
		},
		{
			name: "absolute candidate id",
			ref:  "/cand_review",
		},
		{
			name: "surrounding whitespace candidate id",
			ref:  " cand_whitespace ",
			setup: func(t *testing.T, candidateDir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(candidateDir, "cand_whitespace.json"), []byte("{\"id\":\"cand_whitespace\",\"state\":\"parked\"}"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "blank candidate id",
			ref:  "",
		},
		{
			name: "symlink candidate",
			ref:  "cand_symlink",
			setup: func(t *testing.T, candidateDir string) {
				t.Helper()
				target := filepath.Join(candidateDir, "candidate-body.json")
				if err := os.WriteFile(target, []byte(`{"id":"cand_symlink","state":"parked"}`), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(candidateDir, "cand_symlink.json")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "filename JSON id mismatch",
			ref:  "cand_filename",
			setup: func(t *testing.T, candidateDir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(candidateDir, "cand_filename.json"), []byte(`{"id":"cand_other","state":"parked"}`), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			held, err := s.Create(CreateInput{
				Owner:            "compound-core",
				SuccessPredicate: "held candidate review must use rooted custody",
				State:            "held",
			}, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			runDir := filepath.Join(s.home, "runs", held.ID)
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeJSONFile(t, filepath.Join(runDir, "panel-run.json"), map[string]any{
				"id":          "prun_custody",
				"status":      "applied",
				"target_kind": "memory_candidate",
				"targets":     []map[string]any{{"ref": tc.ref}},
			})
			writeJSONFile(t, filepath.Join(runDir, "promoted-claims.json"), map[string]any{
				"count": 0,
			})
			candidateDir := filepath.Join(s.home, "inbox", "memory-candidates")
			if tc.setup != nil {
				if err := os.MkdirAll(candidateDir, 0o755); err != nil {
					t.Fatal(err)
				}
				tc.setup(t, candidateDir)
			}

			report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Open) != 1 {
				t.Fatalf("open work = %+v", report.Open)
			}
			if len(report.Open[0].ReviewCards) != 0 {
				t.Fatalf("corrupt candidate produced review cards: %+v", report.Open[0].ReviewCards)
			}
			if contains(report.Open[0].MemoryCandidateIDs, tc.ref) {
				t.Fatalf("corrupt candidate ref entered workbench candidate summaries: %+v", report.Open[0].MemoryCandidateIDs)
			}
			if contains(report.Open[0].ObservatoryTags, "memory:candidates") {
				t.Fatalf("corrupt candidate ref entered workbench observatory tags: %+v", report.Open[0].ObservatoryTags)
			}
			if !integrityContains(report.Integrity, held.ID+": memory candidate artifact corrupt") {
				t.Fatalf("candidate corruption missing from report integrity: %v", report.Integrity)
			}
		})
	}
}

func integrityContains(integrity []string, want string) bool {
	for _, entry := range integrity {
		if strings.Contains(entry, want) {
			return true
		}
	}
	return false
}

func TestWorkbenchKeepsLaterValidMemoryCandidateCardAfterCorruptPanel(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	held, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "later valid candidate remains reviewable after corrupt panel evidence",
		State:            "held",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(s.home, "runs", held.ID)
	candidateDir := filepath.Join(s.home, "inbox", "memory-candidates")
	if err := os.MkdirAll(candidateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidateDir, "cand_valid_later.json"), []byte("{\"id\":\"cand_valid_later\",\"state\":\"parked\"}"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(runDir, "panel-run-00-corrupt.json"), map[string]any{
		"id":          "prun_corrupt",
		"status":      "applied",
		"target_kind": "memory_candidate",
		"targets":     []map[string]any{{"ref": "cand_bad/../escape"}},
	})
	writeJSONFile(t, filepath.Join(runDir, "panel-run-01-valid.json"), map[string]any{
		"id":          "prun_valid",
		"status":      "applied",
		"target_kind": "memory_candidate",
		"targets":     []map[string]any{{"ref": "cand_valid_later"}},
	})
	writeJSONFile(t, filepath.Join(runDir, "promoted-claims.json"), map[string]any{"count": 0})

	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	item := findWorkbenchItem(t, report.Open, held.ID)
	if len(item.ReviewCards) != 1 || item.ReviewCards[0].CandidateID != "cand_valid_later" {
		t.Fatalf("later valid panel did not produce its review card: %+v", item.ReviewCards)
	}
	if contains(item.MemoryCandidateIDs, "cand_bad/../escape") || !contains(item.MemoryCandidateIDs, "cand_valid_later") {
		t.Fatalf("candidate summaries did not exclude corrupt ref and retain valid ref: %+v", item.MemoryCandidateIDs)
	}
	if !integrityContains(report.Integrity, held.ID+": memory candidate artifact corrupt") {
		t.Fatalf("corrupt earlier panel missing integrity warning: %v", report.Integrity)
	}
}

func TestWorkbenchReportsCorruptMemoryCandidateWithoutReviewEligibility(t *testing.T) {
	t.Parallel()
	positive := 1
	for _, tc := range []struct {
		name      string
		promotion *int
	}{
		{name: "without promotion summary"},
		{name: "with positive promotion summary", promotion: &positive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			held, err := s.Create(CreateInput{
				Owner:            "compound-core",
				SuccessPredicate: "corrupt candidate evidence stays visible without a review card",
				State:            "held",
			}, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			runDir := filepath.Join(s.home, "runs", held.ID)
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeJSONFile(t, filepath.Join(runDir, "panel-run-corrupt.json"), map[string]any{
				"id":          "prun_corrupt",
				"status":      "applied",
				"target_kind": "memory_candidate",
				"targets":     []map[string]any{{"ref": "cand_bad/../escape"}},
			})
			if tc.promotion != nil {
				writeJSONFile(t, filepath.Join(runDir, "promoted-claims.json"), map[string]any{"count": *tc.promotion})
			}

			report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
			if err != nil {
				t.Fatal(err)
			}
			item := findWorkbenchItem(t, report.Open, held.ID)
			if len(item.ReviewCards) != 0 {
				t.Fatalf("ineligible corrupt evidence produced review cards: %+v", item.ReviewCards)
			}
			if !integrityContains(report.Integrity, held.ID+": memory candidate artifact corrupt") {
				t.Fatalf("candidate corruption missing from report integrity: %v", report.Integrity)
			}
		})
	}
}

func TestWorkbenchRejectsCorruptMemoryCandidateIDsFromRunArtifacts(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	held, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "artifact candidate ids require canonical custody before projection",
		State:            "held",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(s.home, "runs", held.ID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := candidates.WriteCandidate(s.home, candidates.Memory, "cand_canonical", map[string]any{
		"id":    "cand_canonical",
		"state": "parked",
	}); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(runDir, "knowledge-extract-summary.json"), map[string]any{
		"total_created": 2,
		"candidate_ids": []string{"cand_missing/../escape", " cand_canonical "},
	})

	report, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	item := findWorkbenchItem(t, report.Open, held.ID)
	if len(item.MemoryCandidateIDs) != 0 || contains(item.ObservatoryTags, "memory:candidates") {
		t.Fatalf("uncustodied artifact candidate became a summary/tag: ids=%v tags=%v", item.MemoryCandidateIDs, item.ObservatoryTags)
	}
	if !integrityContains(report.Integrity, held.ID+": memory candidate artifact corrupt") {
		t.Fatalf("uncustodied artifact candidate missing integrity warning: %v", report.Integrity)
	}
	coordination, err := s.Coordination(fixedNow, CoordinationOptions{Actor: "compound-core"})
	if err != nil {
		t.Fatal(err)
	}
	if !integrityContains(coordination.Integrity, held.ID+": memory candidate artifact corrupt") {
		t.Fatalf("coordination dropped Workbench candidate integrity: %v", coordination.Integrity)
	}
}
