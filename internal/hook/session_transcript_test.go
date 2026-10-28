package hook

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
	"compound/internal/envelope"
)

func TestSessionEndWorklessTranscriptProducesContentFreeReceipt(t *testing.T) {
	home := t.TempDir()
	transcriptHome := t.TempDir()
	const firstUser = "Please restore SessionEnd receipt parity with private input."
	const lastAssistant = "Implemented the workless receipt path and verified it privately."
	path := writeTranscript(t, transcriptHome, strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-10T01:00:00Z","message":{"role":"user","content":[{"type":"text","text":"` + firstUser + `"}]}}`,
		// The `Skill` block is load-bearing: this fixture's skills-used assertion below
		// used to pass on the PROSE alone ("Loaded /repo/skills/golang-testing/SKILL.md"),
		// because detection scanned raw line text. It no longer does — talking about a
		// skill is not using one (TestSkillsFiredCountsToolUseNotProse). The prose is kept
		// because real transcripts contain it, and the invocation is now explicit.
		`{"type":"assistant","timestamp":"2026-07-10T01:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"Loaded /repo/skills/golang-testing/SKILL.md for this fix."},{"type":"tool_use","name":"Skill","input":{"skill":"golang-testing"}},{"type":"tool_use","name":"Write","input":{"file_path":"internal/hook/session_transcript.go"}},{"type":"tool_use","name":"Edit","input":{"file_path":"internal/hook/hook.go"}},{"type":"tool_use","name":"MultiEdit","input":{"file_path":"internal/hook/consumption.go"}},{"type":"tool_use","name":"Bash","input":{"command":"go test ./internal/hook"}}]}}`,
		`{"type":"assistant","timestamp":"2026-07-10T01:02:00Z","message":{"role":"assistant","content":[{"type":"text","text":"` + lastAssistant + `"}]}}`,
	}, "\n")+"\n")

	deps := baseDeps()
	deps.HomePath = home
	res, err := Run(Fixture{
		Hook:           "session-end",
		Harness:        "claude_code",
		SessionID:      "sess_content",
		Cwd:            "/repo/product",
		TranscriptPath: path,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := kinds(res.Produced); len(got) != 1 || got[0] != "receipt" {
		t.Fatalf("produced = %v, want [receipt]", got)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	if rec.IDs.WorkID != "" || rec.IDs.SessionID != "sess_content" {
		t.Fatalf("receipt ids = %+v, want session-only receipt", rec.IDs)
	}
	if strings.Contains(rec.StateDelta.Summary, firstUser) || strings.Contains(rec.StateDelta.Summary, lastAssistant) {
		t.Fatalf("summary retained raw transcript content: %q", rec.StateDelta.Summary)
	}
	for _, want := range []string{
		"task_ref: sha256:" + hashStr(firstUser),
		"outcome_hint_ref: sha256:" + hashStr(lastAssistant),
	} {
		if !strings.Contains(rec.StateDelta.Summary, want) {
			t.Fatalf("summary missing content-free ref %q: %q", want, rec.StateDelta.Summary)
		}
	}
	if !strings.Contains(rec.StateDelta.Summary, "tool_calls: 5 (bash: 1)") {
		t.Fatalf("summary missing tool/bash counts: %q", rec.StateDelta.Summary)
	}
	assertStringSet(t, rec.StateDelta.FilesChanged, []string{
		"internal/hook/consumption.go",
		"internal/hook/hook.go",
		"internal/hook/session_transcript.go",
	})
	if !containsString(rec.Skills.Used, "capability://skill:golang-testing") {
		t.Fatalf("skills used = %v, want golang-testing", rec.Skills.Used)
	}
	if !containsString(res.Event.SideEffects, "compound://events/"+rec.ID) {
		t.Fatalf("hook side effects = %v, want receipt event ref", res.Event.SideEffects)
	}
	if !hookEventRecordExists(t, home, rec.ID) {
		t.Fatal("receipt not persisted in event door")
	}
	assertHomeOmitsTranscriptText(t, home, firstUser, lastAssistant)
}

func assertHomeOmitsTranscriptText(t *testing.T, home string, forbidden ...string) {
	t.Helper()
	err := filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, raw := range forbidden {
			if strings.Contains(string(body), raw) {
				t.Fatalf("COMPOUND_HOME file %s retained raw transcript text", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan COMPOUND_HOME for raw transcript text: %v", err)
	}
}

func TestSessionEndWorklessReceiptHashesOperatorSpanEvidence(t *testing.T) {
	home := t.TempDir()
	first := "I prefer concise progress updates while implementation is still running."
	second := "Please keep raw conversation text private and cite only stable evidence."
	path := writeTranscript(t, home, strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-10T01:00:00Z","message":{"role":"user","content":[{"type":"text","text":"` + first + `"}]}}`,
		`{"type":"user","timestamp":"2026-07-10T01:00:01Z","message":{"role":"user","content":[{"type":"text","text":"<task-notification>Background agent completed.</task-notification>"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-10T01:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"I will keep the receipt private."}]}}`,
		`{"timestamp":"2026-07-10T01:00:03Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + second + `"}]}}`,
	}, "\n")+"\n")

	deps := baseDeps()
	deps.HomePath = home
	res, err := Run(Fixture{
		Hook:           "session-end",
		Harness:        "claude_code",
		SessionID:      "sess_span_evidence",
		Cwd:            "/repo/product",
		TranscriptPath: path,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := kinds(res.Produced); len(got) != 1 || got[0] != "receipt" {
		t.Fatalf("produced = %v, want [receipt]", got)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	want := []string{
		"transcript-span:sha256:" + hashStr(path) + ":line:1:sha256:" + hashStr(first),
		"transcript-span:sha256:" + hashStr(path) + ":line:4:sha256:" + hashStr(second),
	}
	if len(rec.EvidenceRefs) != len(want) {
		t.Fatalf("evidence refs = %v, want %v", rec.EvidenceRefs, want)
	}
	for i := range want {
		if rec.EvidenceRefs[i] != want[i] {
			t.Fatalf("evidence ref[%d] = %q, want %q", i, rec.EvidenceRefs[i], want[i])
		}
		if strings.Contains(rec.EvidenceRefs[i], path) || strings.Contains(rec.EvidenceRefs[i], first) || strings.Contains(rec.EvidenceRefs[i], second) {
			t.Fatalf("evidence ref leaks raw transcript material: %q", rec.EvidenceRefs[i])
		}
	}
	if len(rec.ProofRefs) != 1 || rec.ProofRefs[0] != "transcript:"+path || rec.Privacy != "private" {
		t.Fatalf("private raw proof pointer changed: privacy=%q proof_refs=%v", rec.Privacy, rec.ProofRefs)
	}
	if len(rec.Memory.Candidates) != 0 || len(rec.Memory.Parked) != 0 || len(rec.Memory.Promoted) != 0 {
		t.Fatalf("session-end evidence must not promote or propose meaning: %+v", rec.Memory)
	}
}

func TestSessionEndWorklessReceiptPersistsEmptyEvidenceArrayWithoutOperatorSpans(t *testing.T) {
	home := t.TempDir()
	path := writeTranscript(t, home, strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-10T01:00:00Z","message":{"role":"user","content":[{"type":"text","text":"<task-notification>Background agent completed.</task-notification>"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-10T01:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"No operator utterance was present."}]}}`,
	}, "\n")+"\n")

	deps := baseDeps()
	deps.HomePath = home
	res, err := Run(Fixture{
		Hook:           "session-end",
		Harness:        "claude_code",
		SessionID:      "sess_empty_span_evidence",
		Cwd:            "/repo/product",
		TranscriptPath: path,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := kinds(res.Produced); len(got) != 1 || got[0] != "receipt" {
		t.Fatalf("produced = %v, want [receipt]", got)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	persisted := readHookEventRecord(t, home, rec.ID, nil)
	if !strings.Contains(string(persisted), `"evidence_refs":[]`) || strings.Contains(string(persisted), `"evidence_refs":null`) {
		t.Fatalf("persisted receipt must encode an empty evidence array, got:\n%s", persisted)
	}
}

func TestSessionEndWorklessLargeTranscriptCompletesWithinBound(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "large-transcript.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	if _, err := fmt.Fprintln(w, `{"type":"user","timestamp":"2026-07-10T01:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Summarize a large session."}]}}`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 42_000; i++ {
		if _, err := fmt.Fprintf(w, `{"type":"assistant","timestamp":"2026-07-10T01:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"padding line %05d %s"}]}}`+"\n", i, strings.Repeat("x", 120)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fmt.Fprintln(w, `{"type":"assistant","timestamp":"2026-07-10T01:02:00Z","message":{"role":"assistant","content":[{"type":"text","text":"Large transcript completed."},{"type":"tool_use","name":"Edit","input":{"file_path":"internal/hook/hook.go"}}]}}`); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 10_000_000 {
		t.Fatalf("large transcript size = %d, want about 10MB", info.Size())
	}

	deps := baseDeps()
	deps.HomePath = home
	start := time.Now()
	res, err := Run(Fixture{
		Hook:           "session-end",
		Harness:        "claude_code",
		SessionID:      "sess_large",
		TranscriptPath: path,
	}, deps, fixedNow)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("large transcript parse took %s, want <= 5s", elapsed)
	}
	if got := kinds(res.Produced); len(got) != 1 || got[0] != "receipt" {
		t.Fatalf("produced = %v, want [receipt]", got)
	}
}

func TestSessionEndWorklessMalformedTranscriptFailsOpenBareEvent(t *testing.T) {
	home := t.TempDir()
	path := writeTranscript(t, home, "{not json}\n")
	deps := baseDeps()
	deps.HomePath = home

	res, err := Run(Fixture{
		Hook:           "session-end",
		Harness:        "claude_code",
		SessionID:      "sess_malformed",
		TranscriptPath: path,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("malformed transcript must fail open, got error: %v", err)
	}
	if got := kinds(res.Produced); len(got) != 0 {
		t.Fatalf("produced = %v, want bare hook event", got)
	}
	if res.Event.IDs.SessionID != "sess_malformed" || res.Event.IDs.WorkID != "" {
		t.Fatalf("event ids = %+v, want session-only bare event", res.Event.IDs)
	}
}

func TestSessionEndWorklessTranscriptDetectsConsumedClaim(t *testing.T) {
	home := t.TempDir()
	const claimRef = "claim://claim_workless01"
	path := writeTranscript(t, home, strings.Join([]string{
		`{"type":"system","timestamp":"2026-07-10T01:00:00Z","message":{"role":"system","content":"Injected memory ` + claimRef + ` for this session."}}`,
		`{"type":"user","timestamp":"2026-07-10T01:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Use the injected memory."}]}}`,
		`{"type":"assistant","timestamp":"2026-07-10T01:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"I used claim_workless01 in the answer."}]}}`,
	}, "\n")+"\n")
	deps := baseDeps()
	deps.HomePath = home

	res, err := Run(Fixture{
		Hook:           "session-end",
		Harness:        "claude_code",
		SessionID:      "sess_claim",
		TranscriptPath: path,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := kinds(res.Produced); len(got) != 1 || got[0] != "receipt" {
		t.Fatalf("produced = %v, want [receipt]", got)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	if rec.Skills.ObservationQuality != "readable" {
		t.Fatalf("workless detector ran => used_declared must be true, got %+v", rec.Skills)
	}
	if !containsString(rec.Skills.Referenced, claimRef) {
		t.Fatalf("consumed claim ref missing from receipt used: %v", rec.Skills.Referenced)
	}
	proj, err := capgraph.BuildCapabilityUsage(home, capgraph.UsageBuildOptions{GeneratedAt: fixedNow})
	if err != nil {
		t.Fatalf("build usage: %v", err)
	}
	if got := capabilityUsedCount(proj, claimRef); got != 0 {
		b, _ := json.MarshalIndent(proj, "", "  ")
		t.Fatalf("workless reference must not become application: used count = %d\n%s", got, b)
	}
}

func TestLastBlockingAskReadsCodexPayloadMessage(t *testing.T) {
	home := t.TempDir()
	path := writeTranscript(t, home, strings.Join([]string{
		`{"timestamp":"2026-07-11T09:14:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Continue the scoped local implementation using the accepted reversible-action ruling."}]}}`,
		`{"timestamp":"2026-07-11T09:15:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The next change is local and reversible. May I continue inside the existing work envelope?"}]}}`,
	}, "\n")+"\n")

	ask, err := LastBlockingAsk(path)
	if err != nil {
		t.Fatalf("last blocking ask: %v", err)
	}
	if ask.Text != "May I continue inside the existing work envelope?" {
		t.Fatalf("ask text = %q", ask.Text)
	}
	if ask.Context != "Continue the scoped local implementation using the accepted reversible-action ruling." {
		t.Fatalf("ask context = %q", ask.Context)
	}
	if ask.Timestamp != "2026-07-11T09:15:00Z" || ask.LineNo != 2 {
		t.Fatalf("ask provenance = timestamp %q line %d", ask.Timestamp, ask.LineNo)
	}
}

func TestLastBlockingAskIgnoresQuestionBeforeLaterAssistantCompletion(t *testing.T) {
	path := writeTranscript(t, t.TempDir(), strings.Join([]string{
		`{"timestamp":"2026-07-11T09:14:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Investigate the scoped implementation and finish when it is verified."}]}}`,
		`{"timestamp":"2026-07-11T09:15:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Should I inspect the existing test first?"}]}}`,
		`{"timestamp":"2026-07-11T09:16:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The implementation is verified and the task is complete."}]}}`,
	}, "\n")+"\n")

	ask, err := LastBlockingAsk(path)
	if err != nil {
		t.Fatal(err)
	}
	if ask.Text != "" {
		t.Fatalf("completed turn retained stale blocking ask: %+v", ask)
	}
}

func TestLastBlockingAskRequiresTerminalGenuineQuestion(t *testing.T) {
	tests := []struct {
		name      string
		assistant string
		want      string
	}{
		{
			name:      "question answered later in same message",
			assistant: "Should I inspect the existing test first? I did; tests pass.",
		},
		{
			name:      "optional follow-up offer",
			assistant: "The implementation is verified. Would you like me to inspect anything else?",
		},
		{
			name:      "optional do-you-want offer",
			assistant: "The implementation is verified. Do you want me to prepare another report?",
		},
		{
			name:      "genuine terminal blocker",
			assistant: "The reversible edit is ready. May I continue inside the existing envelope?",
			want:      "May I continue inside the existing envelope?",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line, err := json.Marshal(map[string]any{
				"timestamp": "2026-07-11T09:15:00Z",
				"type":      "response_item",
				"payload": map[string]any{
					"type": "message", "role": "assistant",
					"content": []map[string]any{{"type": "output_text", "text": tc.assistant}},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			path := writeTranscript(t, t.TempDir(), string(line)+"\n")
			ask, err := LastBlockingAsk(path)
			if err != nil {
				t.Fatal(err)
			}
			if ask.Text != tc.want {
				t.Fatalf("blocking ask = %q, want %q", ask.Text, tc.want)
			}
		})
	}
}

func capabilityUsedCount(proj capgraph.CapabilityUsageProjection, ref string) int {
	for _, item := range proj.Capabilities {
		if item.Ref == ref {
			return item.UsedCount
		}
	}
	return 0
}

func assertStringSet(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, value := range want {
		if !containsString(got, value) {
			t.Fatalf("got %v, want value %q", got, value)
		}
	}
}

// writeCodexRollout writes a minimal codex-format rollout JSONL under
// <root>/sessions/YYYY/MM/DD/ using <sessionID> as the rolling file suffix. The body mirrors
// the shape live codex writes (session_meta header + response_item function_call) so slice 4's
// parser branch can be tested against the same shape live traffic produces.
func writeCodexRollout(t *testing.T, root, sessionID string, lines []string) string {
	t.Helper()
	day := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	dir := filepath.Join(root, "sessions",
		fmt.Sprintf("%04d", day.Year()),
		fmt.Sprintf("%02d", int(day.Month())),
		fmt.Sprintf("%02d", day.Day()),
	)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Codex writes rollouts as `rollout-<ts>-<id>.jsonl`. Use an RFC3339 timestamp
	// for the prefix instead of an integer unix-millis literal so the redact gate
	// (internal/redact, public-visibility scan) does not flag the 13-digit run as an
	// E.164 phone number; the timestamp's value is irrelevant to the parser under test.
	path := filepath.Join(dir, fmt.Sprintf("rollout-%s-%s.jsonl", day.Format(time.RFC3339), sessionID))
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSessionEndCodexBuildsWorklessReceiptFromRollout (F3) -- codex SessionEnd with a wire
// transcript_path builds the workless receipt exactly like Claude: Skills.Used carries the
// skill the rollout's function_call actually invoked. The fixture writes a single codex
// response_item whose arguments point at skills/example-skill/SKILL.md (the same path the
// skillsFromLine rule already scans for in tool-call inputs). On base this fails twice:
// the receipt is either not delivered (SessionEndReceipt=false in harness registry) or
// returned with no Skills.Used entries (parser does not recognise codex format).
func TestSessionEndCodexBuildsWorklessReceiptFromRollout(t *testing.T) {
	const sessionID = "sess-0001"
	transcriptHome := t.TempDir()
	rollout := writeCodexRollout(t, transcriptHome, sessionID, []string{
		`{"type":"session_meta","timestamp":"2026-08-26T15:00:00Z","payload":{"id":"` + sessionID + `","cwd":"/repo/product"}}`,
		`{"type":"response_item","timestamp":"2026-08-26T15:05:00Z","payload":{"type":"function_call","name":"Bash","arguments":"{\"command\":\"cat /repo/skills/example-skill/SKILL.md\"}"}}`,
	})
	deps := baseDeps()
	deps.HomePath = t.TempDir()
	res, err := Run(Fixture{
		Hook:           "session-end",
		Harness:        "codex",
		SessionID:      sessionID,
		Cwd:            "/repo/product",
		TranscriptPath: rollout,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := kinds(res.Produced); len(got) != 1 || got[0] != "receipt" {
		t.Fatalf("produced = %v, want [receipt]", got)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	if rec.IDs.WorkID != "" || rec.IDs.SessionID != sessionID {
		t.Fatalf("receipt ids = %+v, want session-only receipt", rec.IDs)
	}
	if !containsString(rec.Skills.Used, "capability://skill:example-skill") {
		t.Fatalf("skills used = %v, want capability://skill:example-skill", rec.Skills.Used)
	}
	if !containsString(res.Event.SideEffects, "compound://events/"+rec.ID) {
		t.Fatalf("hook side effects = %v, want receipt event ref", res.Event.SideEffects)
	}
}

func TestSessionEndCodexCurrentCustomToolCallCarriesSkillUse(t *testing.T) {
	const sessionID = "sess-current-custom-tool"
	transcriptHome := t.TempDir()
	rollout := writeCodexRollout(t, transcriptHome, sessionID, []string{
		`{"type":"session_meta","timestamp":"2026-08-26T15:00:00Z","payload":{"id":"` + sessionID + `","cwd":"/repo/product"}}`,
		`{"type":"response_item","timestamp":"2026-08-26T15:05:00Z","payload":{"type":"custom_tool_call","name":"exec","input":"const r = await tools.exec_command({\"cmd\":\"cat /repo/skills/current-codex/SKILL.md\"});","call_id":"call_current"}}`,
		`{"type":"event_msg","timestamp":"2026-08-26T15:05:01Z","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":["/bin/zsh","-lc","cat /repo/skills/current-codex/SKILL.md"]}}}`,
	})
	summary, err := scanSessionTranscript(rollout)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Format != "codex" || summary.ToolCalls != 1 || summary.BashCalls != 1 {
		t.Fatalf("summary = %+v, want one current Codex custom tool call", summary)
	}
	if !containsString(summary.SkillsFired, "capability://skill:current-codex") {
		t.Fatalf("skills fired = %v, want capability://skill:current-codex", summary.SkillsFired)
	}

	deps := baseDeps()
	deps.HomePath = t.TempDir()
	res, err := Run(Fixture{
		Hook:           "session-end",
		Harness:        "codex",
		SessionID:      sessionID,
		Cwd:            "/repo/product",
		TranscriptPath: rollout,
	}, deps, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := res.Produced[0].(*envelope.Receipt)
	if !ok || !containsString(rec.Skills.Used, "capability://skill:current-codex") {
		t.Fatalf("receipt = %+v, want current Codex skill use", res.Produced)
	}
}

// TestSessionEndCodexDerivesRolloutFromSessionID (F4) -- codex SessionEnd with an EMPTY
// transcript_path still produces a receipt by deriving the rollout from session_id and
// CODEX_HOME. The fixture lays a codex-format rollout under a tempdir and points CODEX_HOME
// at it; buildWorklessSessionReceipt must resolve and read it. On base the receipt is nil
// because the empty-transcript_path branch was a hard return.
func TestSessionEndCodexDerivesRolloutFromSessionID(t *testing.T) {
	const sessionID = "sess-derive"
	codexHome := t.TempDir()
	writeCodexRollout(t, codexHome, sessionID, []string{
		`{"type":"session_meta","timestamp":"2026-08-26T15:00:00Z","payload":{"id":"` + sessionID + `","cwd":"/repo/product"}}`,
		`{"type":"response_item","timestamp":"2026-08-26T15:05:00Z","payload":{"type":"function_call","name":"Skill","arguments":"{\"skill\":\"example-skill\"}"}}`,
	})
	prev := os.Getenv("CODEX_HOME")
	t.Cleanup(func() { os.Setenv("CODEX_HOME", prev) })
	if err := os.Setenv("CODEX_HOME", codexHome); err != nil {
		t.Fatal(err)
	}

	deps := baseDeps()
	deps.HomePath = t.TempDir()
	res, err := Run(Fixture{
		Hook:      "session-end",
		Harness:   "codex",
		SessionID: sessionID,
		Cwd:       "/repo/product",
		// TranscriptPath intentionally empty: derives from session_id + CODEX_HOME.
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := kinds(res.Produced); len(got) != 1 || got[0] != "receipt" {
		t.Fatalf("produced = %v, want [receipt]", got)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	if !containsString(rec.Skills.Used, "capability://skill:example-skill") {
		t.Fatalf("skills used = %v, want capability://skill:example-skill (derived from rollout)", rec.Skills.Used)
	}
}

// TestResolveCodexRolloutPathHonoursCodexHome is the unit-level companion to F4: the
// resolver picks the newest rollout under CODEX_HOME that ends in <session-id>.jsonl,
// ignoring unrelated rollouts and timestamps that do not match.
func TestResolveCodexRolloutPathHonoursCodexHome(t *testing.T) {
	prev := os.Getenv("CODEX_HOME")
	t.Cleanup(func() { os.Setenv("CODEX_HOME", prev) })
	home := t.TempDir()
	if err := os.Setenv("CODEX_HOME", home); err != nil {
		t.Fatal(err)
	}
	writeCodexRollout(t, home, "sess-a", []string{`{"type":"session_meta"}`})
	time.Sleep(10 * time.Millisecond)
	writeCodexRollout(t, home, "sess-b", []string{`{"type":"session_meta"}`})
	time.Sleep(10 * time.Millisecond)
	newerPath := writeCodexRollout(t, home, "sess-a", []string{`{"type":"session_meta","timestamp":"2026-08-26T16:00:00Z"}`})

	got := ResolveCodexRolloutPath("sess-a")
	if got == "" {
		t.Fatalf("ResolveCodexRolloutPath returned empty; rollouts under %s", home)
	}
	if got != newerPath {
		t.Fatalf("ResolveCodexRolloutPath = %q, want newest %q", got, newerPath)
	}
	if ResolveCodexRolloutPath("sess-missing") != "" {
		t.Fatal("ResolveCodexRolloutPath returned a path for an unknown session id")
	}
	if ResolveCodexRolloutPath("") != "" {
		t.Fatal("ResolveCodexRolloutPath returned a path for empty session id")
	}
}
