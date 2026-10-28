package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func TestInstalledNativePayloadResolvesOwnedRecipientAndDeliversBoundaryAndInterrupt(t *testing.T) {
	for _, harnessName := range []string{"codex", "claude-zai"} {
		t.Run(harnessName, func(t *testing.T) {
			testCase := newNativeCoordinationCase(t, harnessName)
			t.Setenv("COMPOUND_HOME", testCase.homePath)

			promptPayload := fmt.Sprintf(
				`{"hook_event_name":"UserPromptSubmit","session_id":"native-session","cwd":%q,"prompt":"continue governed work"}`,
				testCase.workspace,
			)
			var promptCode int
			promptOut := captureStdout(t, func() {
				_ = captureStderr(t, func() {
					withStdin(t, promptPayload, func() {
						promptCode = runHookCommand([]string{"user-prompt-submit", "--live", "--harness", harnessName})
					})
				})
			})
			if promptCode != 0 {
				t.Fatalf("native prompt exit=%d\n%s", promptCode, promptOut)
			}
			promptContext := parseNativePromptContext(t, promptOut, "UserPromptSubmit")
			if !strings.Contains(promptContext, testCase.boundaryEvent.ID) ||
				strings.Contains(promptContext, testCase.privateBody) {
				t.Fatalf("native prompt did not receive reference-only boundary state:\n%s", promptContext)
			}
			beforeBodyRead := collaborationEventCount(t, testCase.homePath)
			body, err := testCase.store.RetrieveCollaborationBody(work.CollaborationBodyRequest{
				WorkID: testCase.workID, EventID: testCase.boundaryEvent.ID,
				Actor: testCase.receiver, RunAttemptID: testCase.attempt.ID,
				SessionID: "native-session",
			})
			if err != nil {
				t.Fatalf("active native successor could not fetch delivered work-level body: %v", err)
			}
			if string(body.Body) != testCase.privateBody || body.BodyRef != testCase.boundaryEvent.BodyRef {
				t.Fatalf("native work-level body result = %+v", body)
			}
			sibling, err := runattempt.Open(testCase.homePath).Mint(
				testCase.workID, testCase.receiver, harnessName, cliTestNow.Add(5*time.Second),
			)
			if err != nil {
				t.Fatal(err)
			}
			for name, request := range map[string]work.CollaborationBodyRequest{
				"unbound sibling": {
					WorkID: testCase.workID, EventID: testCase.boundaryEvent.ID,
					Actor: testCase.receiver, RunAttemptID: sibling.ID, SessionID: "native-session",
				},
				"wrong session": {
					WorkID: testCase.workID, EventID: testCase.boundaryEvent.ID,
					Actor: testCase.receiver, RunAttemptID: testCase.attempt.ID, SessionID: "wrong-session",
				},
				"wrong work": {
					WorkID: "work_wrong", EventID: testCase.boundaryEvent.ID,
					Actor: testCase.receiver, RunAttemptID: testCase.attempt.ID, SessionID: "native-session",
				},
			} {
				if _, err := testCase.store.RetrieveCollaborationBody(request); err == nil {
					t.Fatalf("%s fetched delivered work-level body", name)
				}
			}
			if afterBodyRead := collaborationEventCount(t, testCase.homePath); afterBodyRead != beforeBodyRead {
				t.Fatalf("native body reads emitted coordination acknowledgement/event: before=%d after=%d",
					beforeBodyRead, afterBodyRead)
			}

			preToolPayload := fmt.Sprintf(
				`{"hook_event_name":"PreToolUse","session_id":"native-session","cwd":%q,"tool_name":"Write","tool_input":{"file_path":%q,"content":"change"}}`,
				testCase.workspace, filepath.Join(testCase.workspace, "README.md"),
			)
			var preToolCode int
			preToolOut := captureStdout(t, func() {
				_ = captureStderr(t, func() {
					withStdin(t, preToolPayload, func() {
						preToolCode = runHookCommand([]string{"pre-tool-use", "--live", "--harness", harnessName, "--enforce"})
					})
				})
			})
			if preToolCode != 3 {
				t.Fatalf("native pre-tool exit=%d, want governed hold\n%s", preToolCode, preToolOut)
			}
			var response claudeLiveHookResponse
			if err := json.Unmarshal([]byte(preToolOut), &response); err != nil {
				t.Fatalf("parse native pre-tool response: %v\n%s", err, preToolOut)
			}
			if response.HookSpecificOutput.PermissionDecision != "deny" ||
				!strings.Contains(response.HookSpecificOutput.AdditionalContext, testCase.interruptEvent.ID) ||
				strings.Contains(response.HookSpecificOutput.AdditionalContext, testCase.privateBody) {
				t.Fatalf("native pre-tool did not hold with reference-only interrupt: %+v",
					response.HookSpecificOutput)
			}
		})
	}
}

func collaborationEventCount(t *testing.T, homePath string) int {
	t.Helper()
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := events.Scan(eventstore.ScanOptions{Prefixes: []string{"wco_"}})
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestNativeCoordinationCursorIsStableAcrossSuccessorRunAttempts(t *testing.T) {
	testCase := newNativeCoordinationCase(t, "codex")
	payload := []byte(fmt.Sprintf(
		`{"hook_event_name":"UserPromptSubmit","session_id":"native-session","cwd":%q,"prompt":"continue"}`,
		testCase.workspace,
	))
	first, err := liveHookFixtureForHome(hookCommandArgs{
		HookName: "user-prompt-submit", Harness: "codex", Live: true,
	}, payload, cliTestNow, testCase.homePath)
	if err != nil {
		t.Fatal(err)
	}
	if first.CoordinationDelivery == nil {
		t.Fatal("first native run did not resolve a coordination recipient")
	}
	firstRun := first.CoordinationDelivery.Recipient.RunAttemptID
	if _, err := runattempt.Open(testCase.homePath).Complete(
		firstRun, runattempt.StatusFailed, cliTestNow.Add(10*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	successor := testCase.attachAttempt(t, "successor", cliTestNow.Add(11*time.Second))

	second, err := liveHookFixtureForHome(hookCommandArgs{
		HookName: "user-prompt-submit", Harness: "codex", Live: true,
	}, payload, cliTestNow.Add(12*time.Second), testCase.homePath)
	if err != nil {
		t.Fatal(err)
	}
	if second.CoordinationDelivery == nil ||
		second.CoordinationDelivery.Recipient.RunAttemptID != successor.ID ||
		first.CoordinationDelivery.CursorKey != second.CoordinationDelivery.CursorKey ||
		strings.Contains(second.CoordinationDelivery.CursorKey, "run_attempt=") {
		t.Fatalf("successor run forked semantic cursor: first=%+v second=%+v",
			first.CoordinationDelivery, second.CoordinationDelivery)
	}
}

type nativeCoordinationCase struct {
	homePath       string
	workspace      string
	harness        string
	sender         string
	receiver       string
	workID         string
	store          *work.Store
	attempt        runattempt.RunAttempt
	boundaryEvent  work.CollaborationEvent
	interruptEvent work.CollaborationEvent
	privateBody    string
}

func newNativeCoordinationCase(t *testing.T, harnessName string) *nativeCoordinationCase {
	t.Helper()
	homePath := t.TempDir()
	workspace := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	const (
		sender   = "codex:sender"
		receiver = "codex:receiver"
	)
	item, err := store.Create(work.CreateInput{
		Owner: sender, SuccessPredicate: "native hook receives governed coordination",
		EvidenceRefs: []string{"scope:hook-delivery", "dependency:stream", "resource:proof"},
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(item.ID, sender, cliTestNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	testCase := &nativeCoordinationCase{
		homePath: homePath, workspace: workspace, harness: harnessName,
		sender: sender, receiver: receiver, workID: item.ID, store: store,
		privateBody: "PRIVATE_NATIVE_COORDINATION_BODY",
	}
	testCase.attempt = testCase.attachAttempt(t, "initial", cliTestNow.Add(2*time.Second))

	action := nativeInterruptAction(testCase.workID, testCase.receiver, testCase.attempt.ID, cliTestNow)
	decision := nativeInterruptDecision(testCase.workID, testCase.attempt.ID, action.ID, cliTestNow)
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(action.ID, action, " "); err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(decision.ID, decision, " "); err != nil {
		t.Fatal(err)
	}
	if err := runattempt.Open(homePath).AttachActionIntent(testCase.attempt.ID, action.ID); err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bodyRef, err := artifacts.Put([]byte(testCase.privateBody))
	if err != nil {
		t.Fatal(err)
	}
	boundary, err := store.RecordCollaboration(item.ID, work.CollaborationInput{
		Actor: sender, IdempotencyKey: "native-boundary", BodyRef: bodyRef,
		DeliveryClass: work.CollaborationDeliveryNextBoundary,
		Delta: work.CollaborationStateDelta{
			Kind: "work", SubjectRef: item.ID, Before: "claimed", After: "changed",
		},
		Affects: work.CollaborationAffects{WorkIDs: []string{item.ID}},
	}, cliTestNow.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	interrupt, err := store.RecordCollaboration(item.ID, work.CollaborationInput{
		Actor: sender, IdempotencyKey: "native-interrupt", BodyRef: bodyRef,
		DeliveryClass: work.CollaborationDeliveryInterrupt,
		Delta: work.CollaborationStateDelta{
			Kind: "run_attempt", SubjectRef: testCase.attempt.ID,
			Before: "active", After: "invalidated",
		},
		Affects: work.CollaborationAffects{
			WorkIDs: []string{item.ID}, RunAttemptIDs: []string{testCase.attempt.ID},
			PolicyRefs: []string{decision.ID},
		},
	}, cliTestNow.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	testCase.boundaryEvent = boundary.Event
	testCase.interruptEvent = interrupt.Event
	return testCase
}

func (testCase *nativeCoordinationCase) attachAttempt(t *testing.T, suffix string, now time.Time) runattempt.RunAttempt {
	t.Helper()
	attempts := runattempt.Open(testCase.homePath)
	attempt, err := attempts.Mint(testCase.workID, testCase.receiver, testCase.harness, now)
	if err != nil {
		t.Fatal(err)
	}
	if attempt, err = attempts.SetOrigin(attempt.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	if attempt, err = attempts.SetWorkspace(attempt.ID, testCase.workspace); err != nil {
		t.Fatal(err)
	}
	bindingID := testCase.recordNativeSessionBinding(t, attempt, "native-session", now)
	if _, err := testCase.store.Advance(testCase.workID, work.AdvanceInput{
		To: "claimed", Actor: testCase.sender, RunAttemptIDs: []string{attempt.ID},
		EvidenceRefs: []string{bindingID}, Note: "native " + suffix + " run attached",
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if attempt, err = attempts.MarkRunning(attempt.ID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	return attempt
}

func (testCase *nativeCoordinationCase) recordNativeSessionBinding(
	t *testing.T,
	attempt runattempt.RunAttempt,
	session string,
	now time.Time,
) string {
	t.Helper()
	digest := sha256.Sum256([]byte(testCase.workID + "\x00" + session + "\x00" + attempt.ID))
	eventID := "wde_" + hex.EncodeToString(digest[:])[:16]
	events, err := eventstore.Open(testCase.homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(eventID, map[string]any{
		"schema_version": "compound.work_delegate_event.v0.1",
		"id":             eventID,
		"kind":           "work.delegate.spawned",
		"ts":             now.UTC().Format(time.RFC3339),
		"work_id":        testCase.workID,
		"harness":        testCase.harness,
		"holder":         testCase.receiver,
		"session":        session,
		"run_attempt_id": attempt.ID,
	}, "  "); err != nil {
		t.Fatal(err)
	}
	return eventID
}

func TestInstalledNativeUnknownSessionReceivesNoCoordinationOrProjectionWrites(t *testing.T) {
	testCase := newNativeCoordinationCase(t, "codex")
	t.Setenv("COMPOUND_HOME", testCase.homePath)
	before := nativeCoordinationProjectionFiles(t, testCase.homePath)
	payload := fmt.Sprintf(
		`{"hook_event_name":"UserPromptSubmit","session_id":"unknown-session","cwd":%q,"prompt":"continue"}`,
		testCase.workspace,
	)
	var code int
	output := captureStdout(t, func() {
		_ = captureStderr(t, func() {
			withStdin(t, payload, func() {
				code = runHookCommand([]string{"user-prompt-submit", "--live", "--harness", "codex"})
			})
		})
	})
	if code != 0 {
		t.Fatalf("unknown native session exit=%d\n%s", code, output)
	}
	if strings.Contains(output, testCase.boundaryEvent.ID) ||
		strings.Contains(output, testCase.interruptEvent.ID) {
		t.Fatalf("unknown native session received coordination: %s", output)
	}
	preToolPayload := fmt.Sprintf(
		`{"hook_event_name":"PreToolUse","session_id":"unknown-session","cwd":%q,"tool_name":"Write","tool_input":{"file_path":%q,"content":"change"}}`,
		testCase.workspace, filepath.Join(testCase.workspace, "unknown-session.txt"),
	)
	var preToolCode int
	preToolOutput := captureStdout(t, func() {
		_ = captureStderr(t, func() {
			withStdin(t, preToolPayload, func() {
				preToolCode = runHookCommand([]string{
					"pre-tool-use", "--live", "--harness", "codex", "--enforce",
				})
			})
		})
	})
	if preToolCode != 0 {
		t.Fatalf("unknown native PreToolUse exit=%d\n%s", preToolCode, preToolOutput)
	}
	var preToolResponse claudeLiveHookResponse
	if err := json.Unmarshal([]byte(preToolOutput), &preToolResponse); err != nil {
		t.Fatal(err)
	}
	if preToolResponse.HookSpecificOutput.PermissionDecision != "" ||
		strings.Contains(preToolResponse.HookSpecificOutput.AdditionalContext, testCase.interruptEvent.ID) {
		t.Fatalf("unknown native session received hold/card: %+v", preToolResponse.HookSpecificOutput)
	}
	after := nativeCoordinationProjectionFiles(t, testCase.homePath)
	if strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("unknown native session wrote cursor/report projection:\nbefore=%v\nafter=%v", before, after)
	}
}

func TestNativePreToolPublicationFaultEmitsCurrentDenyAndKeepsCardRetryableOnce(t *testing.T) {
	tests := []struct {
		name  string
		block func(*testing.T, *nativeCoordinationCase) func()
	}{
		{
			name: "report persistence",
			block: func(t *testing.T, testCase *nativeCoordinationCase) func() {
				t.Helper()
				path := filepath.Join(testCase.homePath, "context", "coordination-delivery", "reports")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("block report directory"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(path) })
				return func() {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "state persistence",
			block: func(t *testing.T, testCase *nativeCoordinationCase) func() {
				t.Helper()
				cursorKey := strings.Join([]string{
					"harness=codex", "session=native-session", "work=" + testCase.workID,
				}, "|")
				digest := sha256.Sum256([]byte(cursorKey))
				dir := filepath.Join(
					testCase.homePath, "context", "coordination-delivery", "cursors",
				)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				lockPath := filepath.Join(dir, hex.EncodeToString(digest[:])+".lock")
				if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
				return func() {
					if err := os.Chmod(dir, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testCase := newNativeCoordinationCase(t, "codex")
			t.Setenv("COMPOUND_HOME", testCase.homePath)
			unblock := tt.block(t, testCase)
			beforeEvents := collaborationEventCount(t, testCase.homePath)

			failedCode, failedOutput := invokeNativeCoordinationPreTool(t, testCase)
			if failedCode == 0 {
				t.Fatalf("publication fault exited successfully:\n%s", failedOutput)
			}
			var failedResponse claudeLiveHookResponse
			if err := json.Unmarshal([]byte(failedOutput), &failedResponse); err != nil {
				t.Fatalf("publication fault emitted no native deny JSON: %v\n%s", err, failedOutput)
			}
			if failedResponse.HookSpecificOutput.PermissionDecision != "deny" ||
				strings.Contains(failedResponse.HookSpecificOutput.AdditionalContext, testCase.interruptEvent.ID) {
				t.Fatalf("publication fault did not deny without prematurely publishing card: %+v",
					failedResponse.HookSpecificOutput)
			}

			unblock()
			retryCode, retryOutput := invokeNativeCoordinationPreTool(t, testCase)
			if retryCode != 3 {
				t.Fatalf("retry exit=%d, want governed hold\n%s", retryCode, retryOutput)
			}
			var retryResponse claudeLiveHookResponse
			if err := json.Unmarshal([]byte(retryOutput), &retryResponse); err != nil {
				t.Fatal(err)
			}
			if retryResponse.HookSpecificOutput.PermissionDecision != "deny" ||
				!strings.Contains(retryResponse.HookSpecificOutput.AdditionalContext, testCase.interruptEvent.ID) {
				t.Fatalf("retry did not publish the recoverable interrupt once: %+v",
					retryResponse.HookSpecificOutput)
			}

			againCode, againOutput := invokeNativeCoordinationPreTool(t, testCase)
			if againCode != 3 {
				t.Fatalf("level hold retry exit=%d, want governed hold\n%s", againCode, againOutput)
			}
			var againResponse claudeLiveHookResponse
			if err := json.Unmarshal([]byte(againOutput), &againResponse); err != nil {
				t.Fatal(err)
			}
			if againResponse.HookSpecificOutput.PermissionDecision != "deny" ||
				strings.Contains(againResponse.HookSpecificOutput.AdditionalContext, testCase.interruptEvent.ID) {
				t.Fatalf("persisted interrupt was redelivered or stopped denying: %+v",
					againResponse.HookSpecificOutput)
			}
			if afterEvents := collaborationEventCount(t, testCase.homePath); afterEvents != beforeEvents {
				t.Fatalf("publication recovery emitted acknowledgement/coordination event: before=%d after=%d",
					beforeEvents, afterEvents)
			}
		})
	}
}

func invokeNativeCoordinationPreTool(
	t *testing.T,
	testCase *nativeCoordinationCase,
) (int, string) {
	t.Helper()
	payload := fmt.Sprintf(
		`{"hook_event_name":"PreToolUse","session_id":"native-session","cwd":%q,"tool_name":"Write","tool_input":{"file_path":%q,"content":"change"}}`,
		testCase.workspace, filepath.Join(testCase.workspace, "README.md"),
	)
	var code int
	output := captureStdout(t, func() {
		_ = captureStderr(t, func() {
			withStdin(t, payload, func() {
				code = runHookCommand([]string{"pre-tool-use", "--live", "--harness", "codex", "--enforce"})
			})
		})
	})
	return code, output
}

func nativeCoordinationProjectionFiles(t *testing.T, homePath string) []string {
	t.Helper()
	root := filepath.Join(homePath, "context", "coordination-delivery")
	var files []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, relative)
		return nil
	}); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

func nativeInterruptAction(workID, actor, runAttemptID string, now time.Time) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent, ID: "act_native_interrupt",
			CreatedAt: now.UTC().Format(time.RFC3339),
			Producer:  envelope.Producer{Kind: "hook", Name: "compound"},
			IDs:       envelope.IDs{WorkID: workID},
			Authority: envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:   "private", Risk: "tier2", EvidenceRefs: []string{runAttemptID},
			Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		Actor: actor, Capability: "work.execute", Action: "continue_run",
		Target:        envelope.ActionTarget{Kind: "work", ID: workID},
		MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "repo",
	}
}

func nativeInterruptDecision(workID, runAttemptID, actionID string, now time.Time) envelope.PolicyDecision {
	return envelope.PolicyDecision{
		Header: envelope.Header{
			SchemaVersion: envelope.SVPolicyDecision, ID: "pol_native_interrupt",
			CreatedAt: now.Add(time.Second).UTC().Format(time.RFC3339),
			Producer:  envelope.Producer{Kind: "policy", Name: "compound"},
			IDs:       envelope.IDs{WorkID: workID},
			Authority: envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:   "private", Risk: "tier2", EvidenceRefs: []string{runAttemptID},
			Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		ActionIntentID: actionID, Outcome: "hold",
		Reasons: []string{"active native action invalidated"}, Obligations: []string{},
	}
}
