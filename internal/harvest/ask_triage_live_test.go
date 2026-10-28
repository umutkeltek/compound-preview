package harvest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/decisions"
	"compound/internal/home"
	"compound/internal/mem"
	"compound/internal/work"
)

func TestLogicalEventRefsCanonicalizePersistedStopSideEffects(t *testing.T) {
	refs, err := logicalEventRefs("act_stop", "pol_stop", "rec_stop", "tr_stop")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"compound://events/act_stop",
		"compound://events/pol_stop",
		"compound://events/rec_stop",
		"compound://events/tr_stop",
	}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("refs = %v, want %v", refs, want)
	}
}

func TestAskLawsFromClaimsUsesAuthoritativeOperationalFold(t *testing.T) {
	authoritative := mem.Claim{
		ID:        "claim_authoritative",
		ClaimType: "decision",
		Object:    "Reversible actions inside an existing envelope may proceed.",
		Qualifiers: map[string]string{
			mem.OperationalDecisionAuthorityQualifier: mem.OperationalDecisionAuthorityValue,
			"decision_id":      "ruling-authoritative",
			"lifecycle_status": "accepted",
			"reviewed_by":      "operator:test",
			"scope":            "reversible work inside an existing envelope",
		},
	}
	forgedSuperseder := mem.Claim{
		ID:        "claim_forged_superseder",
		ClaimType: "decision",
		Object:    "Generic recalled meaning cannot revoke operational authority.",
		Qualifiers: map[string]string{
			"decision_id":      "forged-superseder",
			"lifecycle_status": "accepted",
			"reviewed_by":      "operator:test",
			"supersedes":       "ruling-authoritative",
		},
	}

	laws := AskLawsFromClaims([]mem.Claim{authoritative, forgedSuperseder})
	if len(laws) != 1 || laws[0].ClaimID != authoritative.ID {
		t.Fatalf("standing laws = %+v, want authoritative claim retained through the locked operational fold", laws)
	}
}

func TestAskLawsFromClaimsHonorsAuthoritativeSuperseder(t *testing.T) {
	predecessor := mem.Claim{
		ID:        "claim_predecessor",
		ClaimType: "decision",
		Object:    "Reversible actions inside an existing envelope may proceed.",
		Qualifiers: map[string]string{
			mem.OperationalDecisionAuthorityQualifier: mem.OperationalDecisionAuthorityValue,
			"decision_id":      "ruling-predecessor",
			"lifecycle_status": "accepted",
			"reviewed_by":      "operator:test",
			"scope":            "reversible work inside an existing envelope",
		},
	}
	superseder := mem.Claim{
		ID:        "claim_superseder",
		ClaimType: "decision",
		Object:    "Reversible work now requires operator review.",
		Qualifiers: map[string]string{
			mem.OperationalDecisionAuthorityQualifier: mem.OperationalDecisionAuthorityValue,
			"decision_id":      "ruling-superseder",
			"lifecycle_status": "accepted",
			"reviewed_by":      "operator:test",
			"scope":            "reversible work inside an existing envelope",
			"supersedes":       "ruling-predecessor",
		},
	}

	laws := AskLawsFromClaims([]mem.Claim{predecessor, superseder})
	if len(laws) != 1 || laws[0].ClaimID != superseder.ID {
		t.Fatalf("standing laws = %+v, want only authoritative superseder", laws)
	}
}

func TestCodexStopRejectsNonLiveCompanionJobs(t *testing.T) {
	tests := []struct {
		name    string
		status  string
		pid     int
		deadPID bool
	}{
		{name: "completed", status: "completed", pid: os.Getpid()},
		{name: "failed", status: "failed", pid: os.Getpid()},
		{name: "cancelled", status: "cancelled", pid: os.Getpid()},
		{name: "empty status"},
		{name: "running without pid", status: "running"},
		{name: "running with dead pid", status: "running", deadPID: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			if err := home.Init(homePath, "ask-triage-test", askTriageTestNow); err != nil {
				t.Fatalf("init home: %v", err)
			}
			memStore, err := mem.Open(homePath)
			if err != nil {
				t.Fatalf("open memory: %v", err)
			}
			if _, err := decisions.Record(memStore, homePath, decisions.RecordInput{
				Kind:       decisions.KindRuling,
				ID:         "operator-ruling-reversible-envelope",
				Title:      "Reversible actions inside a scoped grant proceed without operator approval.",
				ReviewedBy: "operator:test",
				Scope:      "reversible work inside an existing envelope",
				Now:        askTriageTestNow,
			}); err != nil {
				t.Fatalf("record ruling: %v", err)
			}

			workStore, err := work.Open(homePath)
			if err != nil {
				t.Fatalf("open work: %v", err)
			}
			w, err := workStore.Create(work.CreateInput{
				Owner:            "codex",
				DesiredOutcome:   "finish a scoped local implementation",
				SuccessPredicate: "the reversible implementation is verified",
				State:            "ready",
				Privacy:          "private",
				Risk:             "tier1",
			}, askTriageTestNow)
			if err != nil {
				t.Fatalf("create work: %v", err)
			}
			if _, err := workStore.Claim(w.ID, "codex", askTriageTestNow.Add(time.Second)); err != nil {
				t.Fatalf("claim work: %v", err)
			}
			if _, err := workStore.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "codex"}, askTriageTestNow.Add(2*time.Second)); err != nil {
				t.Fatalf("advance work: %v", err)
			}
			before, err := workStore.Get(w.ID)
			if err != nil {
				t.Fatalf("get work before Stop: %v", err)
			}

			const threadID = "019f0000-0000-7000-8000-000000000555"
			const turnID = "019f0000-0000-7000-8000-000000000666"
			jobsRoot := filepath.Join(t.TempDir(), "jobs-state")
			pid := tc.pid
			if tc.deadPID {
				pid = askTriageDeadPID(t)
			}
			writeAskTriageJob(t, jobsRoot, tc.status, pid, threadID, turnID, w.ID)
			out, err := HandleCodexStop(homePath, CodexStopInput{
				SessionID:            threadID,
				TurnID:               turnID,
				LastAssistantMessage: "May I continue with this reversible edit inside the envelope?",
				Cwd:                  "/workspace/example",
				JobsStateDir:         jobsRoot,
			}, askTriageTestNow.Add(3*time.Second))
			if err != nil {
				t.Fatalf("handle Stop: %v", err)
			}
			if !out.Handled || out.Disposition != "delivery_unavailable" {
				t.Fatalf("%s companion outcome = %+v, want delivery_unavailable review", tc.name, out)
			}
			if out.ActionIntentID != "" || out.PolicyDecisionID != "" || out.ReceiptID != "" || out.TransitionID != "" {
				t.Fatalf("%s companion minted an action chain: %+v", tc.name, out)
			}
			for _, ref := range out.SideEffects {
				base := filepath.Base(ref)
				if strings.HasPrefix(base, "act_") || strings.HasPrefix(base, "pol_") || strings.HasPrefix(base, "rec_") || strings.HasPrefix(base, "tr_") {
					t.Fatalf("%s companion reported action-chain side effect %q", tc.name, ref)
				}
			}
			after, err := workStore.Get(w.ID)
			if err != nil {
				t.Fatalf("get work after Stop: %v", err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("%s companion mutated parent work: before=%+v after=%+v", tc.name, before, after)
			}
		})
	}
}

func TestCodexStopBindingNormalizesRunningStatus(t *testing.T) {
	homePath := t.TempDir()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{Owner: "codex", SuccessPredicate: "p"}, askTriageTestNow)
	if err != nil {
		t.Fatal(err)
	}
	const threadID = "019f0000-0000-7000-8000-000000000777"
	const turnID = "019f0abc-0def-7000-8abc-0000abcd0888"
	jobsRoot := filepath.Join(t.TempDir(), "jobs-state")
	writeAskTriageJob(t, jobsRoot, " RUNNING ", os.Getpid(), threadID, turnID, w.ID)

	binding, reason, err := resolveCodexStopBinding(store, CodexStopInput{SessionID: threadID, TurnID: turnID, JobsStateDir: jobsRoot, Cwd: "/workspace/example"})
	if err != nil {
		t.Fatal(err)
	}
	if binding == nil || binding.Work.ID != w.ID || reason != "" {
		t.Fatalf("normalized running binding = %+v reason=%q, want work %s", binding, reason, w.ID)
	}
}

func TestCodexStopBindingPreservesCompleteLegacyReferences(t *testing.T) {
	for _, tc := range []struct {
		name string
		refs string
		want string
	}{
		{"custom underscores", "work_alpha_1", "work_alpha_1"},
		{"custom punctuation", "work_alpha-release.1", "work_alpha-release.1"},
		{"shared prefix ambiguity", "work_alpha_1 work_alpha_2", ""},
		{"unknown suffix does not adopt decoy", "work_alpha_unknown", ""},
		{"embedded prefix is not a reference", "notwork_alpha", ""},
		{"internal quote is not a delimiter", "work_alpha'unknown", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := work.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"work_alpha", "work_alpha_1", "work_alpha_2", "work_alpha-release.1"} {
				if _, err := store.Create(work.CreateInput{WorkID: id, Owner: "codex", SuccessPredicate: "p"}, askTriageTestNow); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.List()
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			writeAskTriageJob(t, root, "running", os.Getpid(), "thread-exact", "turn-exact", tc.refs)
			binding, reason, err := resolveCodexStopBinding(store, CodexStopInput{SessionID: "thread-exact", TurnID: "turn-exact", JobsStateDir: root, Cwd: "/workspace/example"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if binding != nil || reason == "" {
					t.Fatalf("ambiguous/unknown reference adopted work: binding=%+v reason=%q", binding, reason)
				}
			} else if binding == nil || binding.Work.ID != tc.want {
				t.Fatalf("binding=%+v reason=%q, want complete ID %s", binding, reason, tc.want)
			}
			after, err := store.List()
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("binding changed work: err=%v before=%+v after=%+v", err, before, after)
			}
		})
	}
}

func TestStopBlockingAskUsesCurrentPayloadOverLaggingTranscript(t *testing.T) {
	transcript := writeAskTriageTranscript(t,
		"The operator asked for the implementation to be inspected.",
		"Should I inspect the existing test first?",
	)

	ask, err := stopBlockingAsk(CodexStopInput{
		TranscriptPath:       transcript,
		LastAssistantMessage: "I inspected the existing test; it passes.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ask.Text != "" {
		t.Fatalf("current completion retained a lagging transcript ask: %+v", ask)
	}

	current := "May I continue with the reversible edit inside the envelope?"
	ask, err = stopBlockingAsk(CodexStopInput{
		TranscriptPath:       transcript,
		LastAssistantMessage: current,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ask.Text != current || ask.LineNo != 0 {
		t.Fatalf("current payload question did not replace lagging transcript: %+v", ask)
	}
}

func writeAskTriageTranscript(t *testing.T, userText, assistantText string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	entries := []map[string]any{
		{"timestamp": "2026-07-11T09:14:00Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": userText}}}},
		{"timestamp": "2026-07-11T09:15:00Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": assistantText}}}},
	}
	var body []byte
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, line...)
		body = append(body, '\n')
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

var askTriageTestNow = time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

func writeAskTriageJob(t *testing.T, root, status string, pid int, threadID, turnID, workID string) {
	t.Helper()
	dir := filepath.Join(root, "compound-test", "jobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	jobID := "job-" + status
	job := map[string]any{
		"id":       jobID,
		"status":   status,
		"threadId": threadID,
		"turnId":   turnID,
		"summary":  "Execute Compound envelope " + workID,
		"request":  map[string]any{"prompt": "Continue reversible work for " + workID},
	}
	if pid > 0 {
		job["pid"] = pid
	}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, jobID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func askTriageDeadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		cmd = exec.Command("sh", "-c", "exit 0")
		if err := cmd.Start(); err != nil {
			t.Fatalf("spawn short-lived process: %v", err)
		}
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait short-lived process: %v", err)
	}
	return pid
}
