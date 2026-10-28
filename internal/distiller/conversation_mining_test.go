package distiller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConversationMiningRecordsTagsAndLossiness(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "agent-conversation-mining-report-latest.json")
	tagsPath := filepath.Join(dir, "agent-conversation-mined-tags-latest.jsonl")
	lossinessPath := filepath.Join(dir, "agent-conversation-lossiness-latest.json")

	writeFile(t, tagsPath, strings.Join([]string{
		`{"accept_for_dream":true,"importance":9,"knowledge_type":"operational","quality_score":10,"tags":["architecture","distillation"],"decision_reasons":["useful"],"summary":"system architecture discussion","model_provider":"deterministic","model_name":"rules-v1","candidate":{"source":"codex","raw_path":"raw/session.jsonl","clean_path":"clean/session.md","clean_status":"mapped","stable":true,"importance":19,"quality_score":10,"knowledge_type":"operational","dream_candidate":true,"reasons":["architecture"],"retained_ratio":0.02,"excerpt":"<<ref>>"}}`,
		`{"accept_for_dream":false,"importance":3,"knowledge_type":"noise","quality_score":2,"tags":["noise"],"decision_reasons":["low signal"],"summary":"routine","model_provider":"deterministic","model_name":"rules-v1","candidate":{"source":"codex","raw_path":"raw/noise.jsonl","clean_path":"clean/noise.md","clean_status":"mapped","stable":true,"importance":1,"quality_score":1,"knowledge_type":"noise","dream_candidate":false,"reasons":["noise"],"retained_ratio":0.01,"excerpt":"<<ref>>"}}`,
	}, "\n")+"\n")
	writeFile(t, lossinessPath, `{"known_lossy_rules":{"assistant_turn_cap_chars":{"codex":500},"drops":"tool results"},"claude_clean_vs_raw_size_ratio":0.044,"codex_clean_vs_raw_size_ratio":0.012}`)
	writeFile(t, reportPath, `{"ok":true,"updated_at":"2026-06-20T17:19:19Z","queue":"queue.jsonl","processed":2,"accepted":2,"rejected":0,"providers":{"deterministic":2},"out":"`+tagsPath+`","report":"`+reportPath+`","dream_pack":"dream-pack","dream_pack_files":2,"note":"Prepared high-fidelity Dream pack only. Dream execution remains separate gated."}`)

	got, err := LoadConversationMining(reportPath, ConversationMiningOptions{LossinessPath: lossinessPath})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status() != "ok" || got.MinedRows != 2 || got.AcceptedForReview != 1 || got.Report.DreamPackFiles != 2 {
		t.Fatalf("conversation mining summary = %+v", got)
	}
	if got.DreamExecution() != "gated" || !got.LossinessRecorded || !got.LossinessKnownRules {
		t.Fatalf("gating/lossiness = %s %t %t", got.DreamExecution(), got.LossinessRecorded, got.LossinessKnownRules)
	}
	if !containsString(got.TopTags, "architecture") || !containsString(got.TopTags, "distillation") {
		t.Fatalf("top tags = %v", got.TopTags)
	}
	for _, prefix := range []string{"conversation_mining_report:sha256:", "conversation_mined_tags:sha256:", "conversation_lossiness:sha256:"} {
		if !hasPrefix(got.EvidenceRefs, prefix) {
			t.Fatalf("evidence refs missing %s in %v", prefix, got.EvidenceRefs)
		}
	}
}

func TestLoadConversationCoverageSummarizesSourceQualityDebt(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "agent-conversation-coverage-latest.json")
	writeFile(t, reportPath, `{
		"ok": true,
		"updated_at": "2026-06-20T15:13:24Z",
		"sources": {
			"claude": {
				"raw_files": 10,
				"mapped_clean": 7,
				"unmapped_raw": 3,
				"stable": 10,
				"raw_mb": 100.5,
				"clean_mb": 7.1,
				"avg_retained_ratio": 0.08,
				"dream_candidates": 2,
				"missing_embeddings": 0
			},
			"codex": {
				"raw_files": 5,
				"mapped_clean": 5,
				"unmapped_raw": 0,
				"stable": 5,
				"raw_mb": 90.0,
				"clean_mb": 8.0,
				"avg_retained_ratio": 0.11,
				"dream_candidates": 0,
				"missing_embeddings": 1
			}
		},
		"total_rows": 15,
		"top_tags": [
			{"source":"claude","raw_basename":"a.jsonl","clean_status":"mapped","importance":19,"quality_score":10,"knowledge_type":"operational","dream_candidate":true,"reasons":["architecture"]}
		]
	}`)

	got, err := LoadConversationCoverage(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == "" || got.ReportDigest == "" || got.Revision == "" {
		t.Fatalf("missing identity/digest: %+v", got)
	}
	if got.SourceCount != 2 || got.TotalRows != 15 || got.RawFiles != 15 || got.MappedClean != 12 || got.UnmappedRaw != 3 {
		t.Fatalf("coverage counts = %+v", got)
	}
	if got.DreamCandidates != 2 || got.MissingEmbeddings != 1 || got.TopTagCount != 1 {
		t.Fatalf("source quality counts = %+v", got)
	}
	for _, want := range []string{"claude_unmapped_raw=3", "claude_dream_candidates=2", "codex_missing_embeddings=1", "claude_low_retained_ratio=0.080"} {
		if !containsString(got.ReviewDebt, want) {
			t.Fatalf("review debt missing %q in %v", want, got.ReviewDebt)
		}
	}
	if !hasPrefix(got.EvidenceRefs, "conversation_coverage_report:sha256:") {
		t.Fatalf("evidence refs = %v", got.EvidenceRefs)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
