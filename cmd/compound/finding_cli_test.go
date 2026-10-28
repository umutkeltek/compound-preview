package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/finding"
	"compound/internal/home"
)

// Ordinary-save-product packet sentinels — kept in lockstep with internal/finding/finding_test.go.
const (
	cliOrdinaryBodySentinel     = "BODY_SENTINEL_7f4d7b\nsecond line"
	cliOrdinaryEvidenceSentinel = "EVIDENCE_SENTINEL_c91e2a"
)

// TestFindingRecordJSONStdinPreservesMultiline — falsifier 9: --stdin preserves a multiline
// body exactly enough that `finding show --json` returns the same body.
func TestFindingRecordJSONStdinPreservesMultiline(t *testing.T) {
	if os.Getenv("COMPOUND_FINDING_CLI_CHILD") == "1" {
		runFindingStdinScenario(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFindingRecordJSONStdinPreservesMultiline$")
	cmd.Env = append(os.Environ(), "COMPOUND_FINDING_CLI_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
}

func runFindingStdinScenario(t *testing.T) {
	t.Helper()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	type ack struct {
		Schema     string `json:"schema"`
		Status     string `json:"status"`
		ID         string `json:"id"`
		Path       string `json:"path"`
		Privacy    string `json:"privacy"`
		Visibility string `json:"visibility"`
	}

	withStdin(t, cliOrdinaryBodySentinel, func() {
		out := captureStdout(t, func() {
			cmdFindingRecord([]string{
				"--topic", "ordinary-save-proof",
				"--stdin",
				"--work-id", "work_first",
				"--produced-by", "agent:first",
				"--evidence", cliOrdinaryEvidenceSentinel,
				"--json",
			})
		})
		var a ack
		if err := json.Unmarshal([]byte(out), &a); err != nil {
			t.Fatalf("first ack parse: %v\n%s", err, out)
		}
		if a.Status != "recorded" {
			t.Fatalf("first ack status = %q, want recorded\n%s", a.Status, out)
		}
		if a.ID == "" || !strings.HasPrefix(a.ID, "find_") {
			t.Fatalf("first ack id = %q, want find_ prefix", a.ID)
		}
		if a.Path != filepath.Join("observed/findings", a.ID+".json") {
			t.Fatalf("first ack path = %q", a.Path)
		}
		if a.Privacy != "private" || a.Visibility != "operational" {
			t.Fatalf("first ack classification = privacy:%q visibility:%q", a.Privacy, a.Visibility)
		}
		if strings.Contains(out, cliOrdinaryBodySentinel) {
			t.Fatalf("first ack JSON must not contain body sentinel:\n%s", out)
		}
		if strings.Contains(out, cliOrdinaryEvidenceSentinel) {
			t.Fatalf("first ack JSON must not contain evidence sentinel:\n%s", out)
		}
		// Confirm the body retrieved via show is byte-identical to what was fed in.
		showOut := captureStdout(t, func() {
			cmdFindingShow([]string{a.ID, "--json"})
		})
		var got finding.Finding
		if err := json.Unmarshal([]byte(showOut), &got); err != nil {
			t.Fatalf("show parse: %v\n%s", err, showOut)
		}
		if got.Body != cliOrdinaryBodySentinel {
			t.Fatalf("show body round-trip mismatch:\nwant=%q\ngot=%q", cliOrdinaryBodySentinel, got.Body)
		}
	})
}

// TestFindingRecordRejectsBadBodySources — falsifier 10: every usage error must exit 2, with
// the EXACT args being exercised (not a re-loop of the whole scenario). Each grandchild runs
// only the selected case so the missing-value/error-injection cases cannot false-green by
// running the first row's args.
func TestFindingRecordRejectsBadBodySources(t *testing.T) {
	// When the test is invoked as a fresh subprocess from itself with CHILD+GRANDCHILD+CASE
	// exported, ONLY the matching case runs (the orchestrator branch is skipped).
	if os.Getenv("COMPOUND_FINDING_USAGE_CHILD") == "1" &&
		os.Getenv("COMPOUND_FINDING_USAGE_GRANDCHILD") == "1" {
		runFindingUsageScenario(t)
		return
	}
	cases := []struct {
		name        string
		args        []string
		stdinBody   string
		withStdin   bool
		envCaseName string
	}{
		{
			name:        "zero-body-sources",
			args:        []string{"--topic", "t", "--work-id", "w", "--json"},
			envCaseName: "zero-body-sources",
		},
		{
			name:        "two-body-sources-body-and-body-file",
			args:        []string{"--topic", "t", "--body", "b", "--body-file", "/tmp/whatever-unused-by-this-case", "--json"},
			envCaseName: "two-body-sources-body-and-body-file",
		},
		{
			name:        "two-body-sources-body-and-stdin",
			args:        []string{"--topic", "t", "--body", "b", "--stdin", "--json"},
			stdinBody:   cliOrdinaryBodySentinel,
			withStdin:   true,
			envCaseName: "two-body-sources-body-and-stdin",
		},
		{
			name:        "unknown-flag",
			args:        []string{"--topic", "t", "--body", "b", "--definitely-not-a-flag", "--json"},
			envCaseName: "unknown-flag",
		},
		{
			name:        "missing-value-trailing",
			args:        []string{"--topic", "t", "--body", "b", "--work-id"},
			envCaseName: "missing-value-trailing",
		},
		{
			name:        "topic-missing-value-then-flag",
			args:        []string{"--topic", "--stdin"},
			envCaseName: "topic-missing-value-then-flag",
		},
		{
			name:        "body-missing-value-then-flag",
			args:        []string{"--topic", "t", "--body", "--json"},
			envCaseName: "body-missing-value-then-flag",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			// Each subtest re-execs a fresh subprocess of THIS binary, exporting the case
			// name so the child runs ONLY the matching row (not the first row).
			cmd := exec.Command(os.Args[0], "-test.run=^TestFindingRecordRejectsBadBodySources$")
			env := append(os.Environ(),
				"COMPOUND_FINDING_USAGE_CHILD=1",
				"COMPOUND_FINDING_USAGE_GRANDCHILD=1",
				"COMPOUND_FINDING_USAGE_CASE="+c.envCaseName,
			)
			cmd.Env = env
			err := cmd.Run()
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("%s: expected exit error, got %v", c.name, err)
			}
			if ee.ExitCode() != 2 {
				t.Fatalf("%s: expected exit 2, got %d", c.name, ee.ExitCode())
			}
		})
	}
}

func runFindingUsageScenario(t *testing.T) {
	t.Helper()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	type usageCase struct {
		name      string
		args      []string
		stdinBody string
		withStdin bool
	}
	allCases := []usageCase{
		{"zero-body-sources", []string{"--topic", "t", "--work-id", "w", "--json"}, "", false},
		{"two-body-sources-body-and-body-file", []string{"--topic", "t", "--body", "b", "--body-file", "/tmp/whatever-unused-by-this-case", "--json"}, "", false},
		{"two-body-sources-body-and-stdin", []string{"--topic", "t", "--body", "b", "--stdin", "--json"}, cliOrdinaryBodySentinel, true},
		{"unknown-flag", []string{"--topic", "t", "--body", "b", "--definitely-not-a-flag", "--json"}, "", false},
		{"missing-value-trailing", []string{"--topic", "t", "--body", "b", "--work-id"}, "", false},
		{"topic-missing-value-then-flag", []string{"--topic", "--stdin"}, "", false},
		{"body-missing-value-then-flag", []string{"--topic", "t", "--body", "--json"}, "", false},
	}
	selected := os.Getenv("COMPOUND_FINDING_USAGE_CASE")
	for _, c := range allCases {
		if c.name != selected {
			continue
		}
		if c.withStdin {
			withStdin(t, c.stdinBody, func() {
				cmdFindingRecord(c.args)
			})
		} else {
			cmdFindingRecord(c.args)
		}
		return
	}
	// Unknown case name reaches the test orchestrator — every executed case must be one of
	// the table rows and must NOT execute multiple rows.
	t.Fatalf("unknown usage case %q", selected)
}

// TestFindingRecordJSONDuplicateAcknowledgement — falsifiers 11, 12, 13: first-save JSON has
// status=recorded; retry JSON has status=duplicate; ack retains top-level id and contains no
// body, body marker, evidence-ref value, topic, or provenance.
func TestFindingRecordJSONDuplicateAcknowledgement(t *testing.T) {
	if os.Getenv("COMPOUND_FINDING_DUP_CHILD") == "1" {
		runFindingDuplicateScenario(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFindingRecordJSONDuplicateAcknowledgement$")
	cmd.Env = append(os.Environ(), "COMPOUND_FINDING_DUP_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
}

func runFindingDuplicateScenario(t *testing.T) {
	t.Helper()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	withStdin(t, cliOrdinaryBodySentinel, func() {
		firstOut := captureStdout(t, func() {
			cmdFindingRecord([]string{
				"--topic", "ordinary-save-proof",
				"--stdin",
				"--work-id", "work_first",
				"--produced-by", "agent:first",
				"--evidence", cliOrdinaryEvidenceSentinel,
				"--json",
			})
		})
		var first map[string]any
		if err := json.Unmarshal([]byte(firstOut), &first); err != nil {
			t.Fatalf("first ack parse: %v\n%s", err, firstOut)
		}
		if first["status"] != "recorded" {
			t.Fatalf("first ack status = %v, want recorded", first["status"])
		}
		if first["id"] == nil || first["id"] == "" {
			t.Fatalf("first ack must retain top-level id, got %v", first["id"])
		}
		if _, has := first["body"]; has {
			t.Fatalf("first ack must not contain body key: %v", first)
		}
		if _, has := first["evidence_refs"]; has {
			t.Fatalf("first ack must not contain evidence_refs key: %v", first)
		}
		if strings.Contains(firstOut, cliOrdinaryBodySentinel) {
			t.Fatalf("first ack JSON must not contain body sentinel:\n%s", firstOut)
		}
		if strings.Contains(firstOut, cliOrdinaryEvidenceSentinel) {
			t.Fatalf("first ack JSON must not contain evidence sentinel:\n%s", firstOut)
		}
	})

	// Retry with DIFFERENT provenance — duplicate ack must surface the FIRST record's fields.
	withStdin(t, cliOrdinaryBodySentinel, func() {
		secondOut := captureStdout(t, func() {
			cmdFindingRecord([]string{
				"--topic", "ordinary-save-proof",
				"--stdin",
				"--work-id", "work_second",
				"--produced-by", "agent:second",
				"--evidence", "trace:second",
				"--json",
			})
		})
		var second map[string]any
		if err := json.Unmarshal([]byte(secondOut), &second); err != nil {
			t.Fatalf("second ack parse: %v\n%s", err, secondOut)
		}
		if second["status"] != "duplicate" {
			t.Fatalf("second ack status = %v, want duplicate", second["status"])
		}
		if second["id"] == nil || second["id"] == "" {
			t.Fatalf("second ack must retain top-level id, got %v", second["id"])
		}
		if second["privacy"] != "private" || second["visibility"] != "operational" {
			t.Fatalf("second ack classification = privacy:%v visibility:%v", second["privacy"], second["visibility"])
		}
		for _, forbidden := range []string{"topic", "produced_by", "work_id", "created_at", "cid", "evidence_ref_count"} {
			if _, ok := second[forbidden]; ok {
				t.Fatalf("second ack exposes forbidden key %q: %v", forbidden, second)
			}
		}
		// The retry ack must NOT echo the retry caller's provenance or evidence.
		if strings.Contains(secondOut, "agent:second") {
			t.Fatalf("second ack must not contain retry producer:\n%s", secondOut)
		}
		if strings.Contains(secondOut, "trace:second") {
			t.Fatalf("second ack must not contain retry evidence ref:\n%s", secondOut)
		}
		if strings.Contains(secondOut, cliOrdinaryBodySentinel) {
			t.Fatalf("second ack JSON must not contain body sentinel:\n%s", secondOut)
		}
	})

	// Exactly one file remains.
	entries, err := os.ReadDir(filepath.Join(homePath, "observed/findings"))
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			files++
		}
	}
	if files != 1 {
		t.Fatalf("expected exactly one finding file, got %d", files)
	}
}

// TestFindingRecordNoCandidateOrClaimSideEffects — falsifier 15: no candidate or Claim is
// created and no promotion/adjudication command is invoked. Proof is a real before/after
// snapshot diff of the home's relevant dirs (ledger tree/digest, candidates/claims/receipts,
// work, etc.). Any byte-identical drift is a hard failure.
func TestFindingRecordNoCandidateOrClaimSideEffects(t *testing.T) {
	if os.Getenv("COMPOUND_FINDING_NOCC_CHILD") == "1" {
		runFindingNoSideEffectsScenario(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFindingRecordNoCandidateOrClaimSideEffects$")
	cmd.Env = append(os.Environ(), "COMPOUND_FINDING_NOCC_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
}

func runFindingNoSideEffectsScenario(t *testing.T) {
	t.Helper()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	// Snapshot the home BEFORE recording. The probe enumerates the relevant dirs
	// (ledger, candidates, claims, receipts, work, etc.) and hashes every entry except the
	// observed/findings tree (which is the only place recording is allowed to write).
	before := snapshotHomeMinusFindings(t, homePath)

	withStdin(t, cliOrdinaryBodySentinel, func() {
		out := captureStdout(t, func() {
			cmdFindingRecord([]string{
				"--topic", "ordinary-save-proof",
				"--stdin",
				"--work-id", "work_first",
				"--produced-by", "agent:first",
				"--evidence", cliOrdinaryEvidenceSentinel,
				"--json",
			})
		})
		if !strings.Contains(out, `"status": "recorded"`) {
			t.Fatalf("first save must record:\n%s", out)
		}
	})

	// Retried duplicate — must remain a no-op write.
	withStdin(t, cliOrdinaryBodySentinel, func() {
		out := captureStdout(t, func() {
			cmdFindingRecord([]string{
				"--topic", "ordinary-save-proof",
				"--stdin",
				"--work-id", "work_second",
				"--produced-by", "agent:second",
				"--evidence", "trace:second",
				"--json",
			})
		})
		if !strings.Contains(out, `"status": "duplicate"`) {
			t.Fatalf("retry must duplicate:\n%s", out)
		}
	})

	// Snapshot AFTER recording. Anything in the relevant dirs that differs MUST be zero
	// findings, zero candidate writes, zero claim writes, zero receipt writes.
	after := snapshotHomeMinusFindings(t, homePath)
	if !equalSnapshot(before, after) {
		t.Fatalf("finding record caused side effects in a relevant home dir\nbefore=%v\nafter=%v", before, after)
	}

	// And the only file written under observed/findings is the deterministic record.
	findingsDir := filepath.Join(homePath, "observed", "findings")
	entries, err := os.ReadDir(findingsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one finding file, got %d", len(entries))
	}
}

// snapshotHomeMinusFindings returns a per-file content hash for every file under
// homePath EXCEPT under observed/findings (which is the only allowed recording surface).
// The ledger tree/digest and every candidates/claims/receipts/work file MUST match before/after.
func snapshotHomeMinusFindings(t *testing.T, homePath string) map[string]string {
	t.Helper()
	out := map[string]string{}
	skipPrefix := filepath.ToSlash(filepath.Join(homePath, "observed", "findings")) + "/"
	err := filepath.WalkDir(homePath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(filepath.ToSlash(p)+"/", skipPrefix) || strings.HasPrefix(filepath.ToSlash(p), skipPrefix) {
			return nil
		}
		rel, err := filepath.Rel(homePath, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func equalSnapshot(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestFindingRecordTopLevelCommandHelpAndExit — bare `compound finding` must exit 2 AND
// print discoverable help naming `record`, all three body forms, and the evidence-only /
// no-promotion boundary.
func TestFindingRecordTopLevelCommandHelpAndExit(t *testing.T) {
	if os.Getenv("COMPOUND_FINDING_NOSUB_CHILD") == "1" {
		runFindingNoSubcommandScenario(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFindingRecordTopLevelCommandHelpAndExit$")
	cmd.Env = append(os.Environ(), "COMPOUND_FINDING_NOSUB_CHILD=1")
	out, err := cmd.CombinedOutput()
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected exit error, got %v\n%s", err, out)
	}
	if ee.ExitCode() != 2 {
		t.Fatalf("expected exit 2, got %d", ee.ExitCode())
	}
	text := strings.ToLower(string(out))
	for _, want := range []string{
		"record",
		"list",
		"show",
		"--body",
		"--body-file",
		"--stdin",
		"1 mib",
		"evidence",
		"never a ledger byte",
		"claim", // explicit no-promotion boundary
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("bare `compound finding` help must mention %q\n%s", want, text)
		}
	}
}

func runFindingNoSubcommandScenario(t *testing.T) {
	t.Helper()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	cmdFinding(nil)
}

// TestFindingRecordHumanDistinguishesRecordedFromDuplicate — falsifier text: human output
// must distinguish recorded from duplicate without printing the body.
func TestFindingRecordHumanDistinguishesRecordedFromDuplicate(t *testing.T) {
	if os.Getenv("COMPOUND_FINDING_HUMAN_CHILD") == "1" {
		runFindingHumanScenario(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFindingRecordHumanDistinguishesRecordedFromDuplicate$")
	cmd.Env = append(os.Environ(), "COMPOUND_FINDING_HUMAN_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
}

func runFindingHumanScenario(t *testing.T) {
	t.Helper()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	firstOut := captureStdout(t, func() {
		withStdin(t, cliOrdinaryBodySentinel, func() {
			cmdFindingRecord([]string{
				"--topic", "ordinary-save-proof",
				"--stdin",
				"--work-id", "work_first",
				"--produced-by", "agent:first",
				"--evidence", cliOrdinaryEvidenceSentinel,
			})
		})
	})
	if !strings.Contains(firstOut, "recorded finding") {
		t.Fatalf("human first output must say 'recorded finding':\n%s", firstOut)
	}
	if strings.Contains(firstOut, cliOrdinaryBodySentinel) {
		t.Fatalf("human first output must not echo body:\n%s", firstOut)
	}

	secondOut := captureStdout(t, func() {
		withStdin(t, cliOrdinaryBodySentinel, func() {
			cmdFindingRecord([]string{
				"--topic", "ordinary-save-proof",
				"--stdin",
				"--work-id", "work_second",
				"--produced-by", "agent:second",
				"--evidence", "trace:second",
			})
		})
	})
	if !strings.Contains(secondOut, "duplicate") {
		t.Fatalf("human retry output must say 'duplicate':\n%s", secondOut)
	}
	if strings.Contains(secondOut, cliOrdinaryBodySentinel) {
		t.Fatalf("human retry output must not echo body:\n%s", secondOut)
	}
}

func TestFindingRecordJSONAckIsMinimalAndClassificationExplicit(t *testing.T) {
	t.Parallel()
	ack := findingRecordAck(finding.RecordResult{
		Status: finding.StatusRecorded,
		Finding: finding.Finding{
			ID:           "find_0123456789ab",
			CID:          finding.CID(cliOrdinaryBodySentinel),
			Topic:        "private-topic-sentinel",
			Body:         cliOrdinaryBodySentinel,
			ProducedBy:   "private-producer-sentinel",
			WorkID:       "private-work-sentinel",
			SourceClass:  "evidence",
			EvidenceRefs: []string{cliOrdinaryEvidenceSentinel},
			CreatedAt:    "2026-01-01T00:00:00Z",
		},
	})

	want := map[string]any{
		"schema":     "compound.finding.record-ack.v1",
		"status":     finding.StatusRecorded,
		"id":         "find_0123456789ab",
		"path":       filepath.Join("observed/findings", "find_0123456789ab.json"),
		"privacy":    "private",
		"visibility": "operational",
	}
	if len(ack) != len(want) {
		t.Fatalf("ack keys = %v, want exactly %v", ack, want)
	}
	for key, wantValue := range want {
		if got := ack[key]; got != wantValue {
			t.Fatalf("ack[%q] = %v, want %v", key, got, wantValue)
		}
	}
	for _, forbidden := range []string{
		"body", "cid", "topic", "produced_by", "work_id", "created_at",
		"evidence_refs", "evidence_ref_count", "supersedes",
	} {
		if _, ok := ack[forbidden]; ok {
			t.Fatalf("minimal ack exposes forbidden key %q: %v", forbidden, ack)
		}
	}
}

func TestResolveBodySourceBoundsStdinAtOneMiB(t *testing.T) {
	const maxBytes = finding.MaxBodyBytes
	for _, tc := range []struct {
		name      string
		size      int
		wantError bool
	}{
		{name: "exact-bound-accepted", size: maxBytes},
		{name: "one-byte-over-rejected", size: maxBytes + 1, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdinPath := filepath.Join(t.TempDir(), "stdin.txt")
			stdinFile, err := os.Create(stdinPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := stdinFile.WriteString(strings.Repeat("x", tc.size)); err != nil {
				t.Fatal(err)
			}
			if _, err := stdinFile.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			oldStdin := os.Stdin
			os.Stdin = stdinFile
			t.Cleanup(func() {
				os.Stdin = oldStdin
				_ = stdinFile.Close()
			})

			body, err := resolveBodySource([]string{"--stdin"})
			if tc.wantError {
				if err == nil {
					t.Fatalf("resolveBodySource accepted %d-byte stdin", tc.size)
				}
				if !strings.Contains(err.Error(), "1 MiB") {
					t.Fatalf("oversize error = %v, want declared 1 MiB bound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveBodySource exact bound: %v", err)
			}
			if len(body) != tc.size {
				t.Fatalf("resolved bytes = %d, want %d", len(body), tc.size)
			}
		})
	}
}

func TestResolveBodySourceBoundsBodyFileAtOneMiB(t *testing.T) {
	t.Parallel()
	const maxBytes = finding.MaxBodyBytes
	for _, tc := range []struct {
		name      string
		size      int
		wantError bool
	}{
		{name: "exact-bound-accepted", size: maxBytes},
		{name: "one-byte-over-rejected", size: maxBytes + 1, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bodyPath := filepath.Join(t.TempDir(), "body.md")
			if err := os.WriteFile(bodyPath, []byte(strings.Repeat("x", tc.size)), 0o600); err != nil {
				t.Fatal(err)
			}
			body, err := resolveBodySource([]string{"--body-file", bodyPath})
			if tc.wantError {
				if err == nil {
					t.Fatalf("resolveBodySource accepted %d-byte body file", tc.size)
				}
				if !strings.Contains(err.Error(), "1 MiB") {
					t.Fatalf("oversize error = %v, want declared 1 MiB bound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveBodySource exact bound: %v", err)
			}
			if len(body) != tc.size {
				t.Fatalf("resolved bytes = %d, want %d", len(body), tc.size)
			}
		})
	}
}

// The body bound is a gate, not a size preference: past it, the content's home is an evidence
// artifact the finding CITES. A bare "--stdin exceeds the 1 MiB limit" is read as "split it",
// which produces two findings that each say less than one would have.
//
// RED WITHOUT the conversion: resolveBodySource returned a plain fmt.Errorf, so the CLI could
// not tell an oversize body from an unreadable path, and the rendered text was one line with
// no gate name and no command — assertTeachingRefusal fails on its first assertion.
func TestFindingBodyBoundRefusalTeachesTheCiteShape(t *testing.T) {
	t.Parallel()
	oversize := strings.Repeat("x", finding.MaxBodyBytes+1)
	_, err := resolveBodySource([]string{"--body", oversize})
	if err == nil {
		t.Fatal("resolveBodySource accepted a body one byte past the bound")
	}

	// Typed, so the CLI can route it to the gate rather than to the generic error line.
	var bound *findingBodyBoundError
	if !errors.As(err, &bound) {
		t.Fatalf("oversize body error is not a *findingBodyBoundError (%T: %v) — the CLI cannot distinguish it from an unreadable path", err, err)
	}
	if bound.Bound != finding.MaxBodyBytes {
		t.Errorf("refusal carries bound %d, want the constant it was checked against (%d)", bound.Bound, finding.MaxBodyBytes)
	}
	if bound.Got <= bound.Bound {
		t.Errorf("refusal reports %d bytes observed, which does not exceed its own bound %d", bound.Got, bound.Bound)
	}

	text := findingBodyBoundRefusal(bound, "example-topic").String()
	assertTeachingRefusal(t, text, "finding_body_bound",
		"1 MiB",
		"--topic example-topic",
		"--evidence",
	)
	// "Cite it" is the fix; "split it" is the wrong lesson a bare size error teaches.
	if !strings.Contains(text, "not the transcript") {
		t.Errorf("refusal must name the cite-don't-inline shape of the fix, got:\n%s", text)
	}

	// Teeth on the drift the old hardcoded "1 MiB" string invited: the label is derived
	// from the bound, so a raised bound cannot leave the refusal quoting the old number.
	if got := findingBodyBoundLabel(4 << 20); got != "4 MiB" {
		t.Errorf("bound label = %q for a 4 MiB bound, want %q — the message is not derived from the constant", got, "4 MiB")
	}
}
