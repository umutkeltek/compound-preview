package routeevidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/eventstore"
)

var t0 = time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

func TestBuildAndJoinTransitionProjectsRoutingNotTaskSuccess(t *testing.T) {
	ev, err := Build(BuildInput{
		CreatedAt:     t0,
		RequestID:     "gateway:req_123",
		WorkID:        "work_123",
		ClientProfile: "gateway.ingress",
		Protocol:      "openai-chat-completions",
		Provider:      "fixture-provider",
		Model:         "fixture-model",
		Usage:         Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14},
		LatencyMS:     23,
		Status:        "completed",
		EvidenceRefs:  []string{"gateway:trace_7", "gateway:trace_7"},
	})
	if err != nil {
		t.Fatalf("build evidence: %v", err)
	}
	joined, tr, err := JoinTransition(ev)
	if err != nil {
		t.Fatalf("join transition: %v", err)
	}
	if joined.TransitionID != tr.ID {
		t.Fatalf("transition_id = %q, want %q", joined.TransitionID, tr.ID)
	}
	if tr.IDs.WorkID != "work_123" || tr.SourceTraceID != "gateway:req_123" {
		t.Fatalf("transition ids = work:%q source:%q", tr.IDs.WorkID, tr.SourceTraceID)
	}
	if len(tr.Projections.Routing) != 1 || tr.Projections.Routing[0] != ev.ID {
		t.Fatalf("routing projection = %+v, want %s", tr.Projections.Routing, ev.ID)
	}
	if len(tr.Projections.Cost) != 1 || tr.Projections.Cost[0] != ev.ID {
		t.Fatalf("cost projection = %+v, want %s", tr.Projections.Cost, ev.ID)
	}
	if tr.SecondOrder.FutureBehaviorChanged {
		t.Fatal("route evidence alone must not claim future behavior changed")
	}
	if !strings.Contains(tr.StateDelta, "not task success") {
		t.Fatalf("state_delta = %q, want explicit not-task-success caveat", tr.StateDelta)
	}
}

func TestImportFileRecordsEvidenceAndTransition(t *testing.T) {
	home := t.TempDir()
	ev := fixtureEvidence()
	path := writeEvidenceFile(t, t.TempDir(), ev)
	result, err := ImportFile(home, path)
	if err != nil {
		t.Fatalf("import route evidence: %v", err)
	}
	if result.Evidence.TransitionID != result.Transition.ID {
		t.Fatalf("transition_id = %q, want %q", result.Evidence.TransitionID, result.Transition.ID)
	}
	rowData := readFile(t, filepath.Join(home, "ledger", "execution-route-evidence.jsonl"))
	lines := strings.Split(strings.TrimSpace(string(rowData)), "\n")
	if len(lines) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(lines))
	}
	var row Evidence
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("decode evidence row: %v", err)
	}
	if row.ID != ev.ID || row.TransitionID != result.Transition.ID {
		t.Fatalf("row = %+v", row)
	}
	if row.RecordedVia != RecorderExternalImport {
		t.Fatalf("recorded_via = %q, want external import", row.RecordedVia)
	}
	var tr envelope.Transition
	data := readFile(t, filepath.Join(home, "events", result.Transition.ID+".json"))
	if err := json.Unmarshal(data, &tr); err != nil {
		t.Fatalf("decode transition: %v", err)
	}
	if tr.Projections.Routing[0] != ev.ID || tr.SecondOrder.FutureBehaviorChanged {
		t.Fatalf("transition = %+v", tr)
	}
}

func TestRecordersOwnProvenance(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		record func(string, Evidence) (Evidence, error)
	}{
		{
			name: "generic joined",
			want: RecorderGeneric,
			record: func(home string, ev Evidence) (Evidence, error) {
				result, err := RecordJoined(home, ev)
				return result.Evidence, err
			},
		},
		{
			name: "generic direct",
			want: RecorderGeneric,
			record: func(home string, ev Evidence) (Evidence, error) {
				if err := RecordEvidence(home, ev); err != nil {
					return Evidence{}, err
				}
				recorded, _, err := LookupRequestID(home, ev.RequestID)
				return recorded, err
			},
		},
		{
			name: "trusted synthetic seam",
			want: RecorderTrustedPanelRuntime,
			record: func(home string, ev Evidence) (Evidence, error) {
				result, err := RecordPanelRuntimeJoined(home, ev)
				return result.Evidence, err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			ev := fixtureEvidence()
			ev.RecordedVia = RecorderExternalImport
			recorded, err := tc.record(home, ev)
			if err != nil {
				t.Fatal(err)
			}
			if recorded.RecordedVia != tc.want {
				t.Fatalf("recorded_via = %q, want %q", recorded.RecordedVia, tc.want)
			}
		})
	}
}

func TestRecordJoinedUniqueRequestIDSerializesGenericWriter(t *testing.T) {
	home := t.TempDir()
	build := func(createdAt time.Time, status string) Evidence {
		t.Helper()
		ev, err := Build(BuildInput{
			CreatedAt:     createdAt,
			RequestID:     "panel:prun_atomic:target:lens",
			WorkID:        "work_atomic",
			ClientProfile: "compound.panel.lens",
			Protocol:      "panel-lane",
			Provider:      "compound-route-plan",
			Model:         "fixture-model",
			Usage:         Usage{TotalTokens: 7},
			Status:        status,
			EvidenceRefs:  []string{ModelCapabilitiesAuthorityRef},
		})
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	panelEvidence := build(t0, "completed")
	genericEvidence := build(t0.Add(time.Second), "failed")

	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := RecordJoinedUniqueRequestID(home, panelEvidence)
		results <- err
	}()
	go func() {
		<-start
		_, err := RecordJoined(home, genericEvidence)
		results <- err
	}()
	close(start)

	successes := 0
	conflicts := 0
	for range 2 {
		err := <-results
		if err == nil {
			successes++
			continue
		}
		if strings.Contains(err.Error(), "request_id") {
			conflicts++
			continue
		}
		t.Fatalf("unexpected concurrent record error: %v", err)
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent request-id writes = successes:%d conflicts:%d, want 1/1", successes, conflicts)
	}
	rows := strings.Split(strings.TrimSpace(string(readFile(t, filepath.Join(home, "ledger", "execution-route-evidence.jsonl")))), "\n")
	if len(rows) != 1 {
		t.Fatalf("concurrent request-id writes persisted %d rows, want 1", len(rows))
	}
	events, err := os.ReadDir(filepath.Join(home, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("concurrent request-id writes persisted %d transitions, want 1", len(events))
	}
}

func TestRequestClaimExcludesOtherWritersAndIsConsumedByOwner(t *testing.T) {
	home := t.TempDir()
	ev := fixtureEvidence()
	claim, err := BuildRequestClaim(ev.RequestID, ev.WorkID, ev.ClientProfile, ev.Protocol, ev.SelectedTarget)
	if err != nil {
		t.Fatal(err)
	}
	if err := ClaimRequestID(home, claim); err != nil {
		t.Fatalf("claim request id: %v", err)
	}
	if _, err := RecordJoined(home, ev); err == nil || !strings.Contains(err.Error(), "is claimed") {
		t.Fatalf("unclaimed writer was not excluded: %v", err)
	}
	result, err := RecordClaimedJoined(home, ev, claim)
	if err != nil {
		t.Fatalf("record claimed evidence: %v", err)
	}
	if result.Evidence.RequestID != ev.RequestID {
		t.Fatalf("recorded request_id = %q, want %q", result.Evidence.RequestID, ev.RequestID)
	}
	if _, err := os.Stat(requestClaimPath(home, ev.RequestID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("consumed request claim still exists: %v", err)
	}
	if _, err := RecordJoined(home, ev); err != nil {
		t.Fatalf("released request id lost normal idempotency: %v", err)
	}
}

func TestRecordClaimedJoinedRetainsRequestClaimUntilLedgerPublicationIsDurable(t *testing.T) {
	fault := errors.New("injected route evidence publication failure")
	faults := []string{"short write", "sync", "close"}
	ledgerStates := []struct {
		name string
		seed bool
	}{
		{name: "empty ledger"},
		{name: "existing ledger", seed: true},
	}

	for _, faultName := range faults {
		t.Run(faultName, func(t *testing.T) {
			for _, ledgerState := range ledgerStates {
				t.Run(ledgerState.name, func(t *testing.T) {
					home := t.TempDir()
					ledgerPath := filepath.Join(home, "ledger", "execution-route-evidence.jsonl")
					if ledgerState.seed {
						seed, err := Build(BuildInput{
							CreatedAt:     t0.Add(-time.Minute),
							RequestID:     "gateway:req_durable_seed",
							WorkID:        "work_durable_seed",
							ClientProfile: "gateway.ingress",
							Protocol:      "openai-chat-completions",
							Provider:      "fixture-provider",
							Model:         "fixture-model",
							Status:        "completed",
						})
						if err != nil {
							t.Fatal(err)
						}
						if _, err := RecordJoined(home, seed); err != nil {
							t.Fatalf("seed route evidence: %v", err)
						}
					}
					before, beforeErr := os.ReadFile(ledgerPath)
					if beforeErr != nil && !errors.Is(beforeErr, os.ErrNotExist) {
						t.Fatal(beforeErr)
					}

					ev := fixtureEvidence()
					claim, err := BuildRequestClaim(ev.RequestID, ev.WorkID, ev.ClientProfile, ev.Protocol, ev.SelectedTarget)
					if err != nil {
						t.Fatal(err)
					}
					if err := ClaimRequestID(home, claim); err != nil {
						t.Fatalf("claim request id: %v", err)
					}

					previousWrite := writeRouteEvidenceFile
					previousSync := syncRouteEvidenceFile
					previousClose := closeRouteEvidenceFile
					defer func() {
						writeRouteEvidenceFile = previousWrite
						syncRouteEvidenceFile = previousSync
						closeRouteEvidenceFile = previousClose
					}()
					switch faultName {
					case "short write":
						writeRouteEvidenceFile = func(file *os.File, data []byte) (int, error) {
							short := len(data) / 2
							if short == 0 {
								short = 1
							}
							return file.Write(data[:short])
						}
					case "sync":
						syncRouteEvidenceFile = func(*os.File) error { return fault }
					case "close":
						closeRouteEvidenceFile = func(file *os.File) error {
							if err := file.Close(); err != nil {
								return err
							}
							return fault
						}
					}

					if _, err := RecordClaimedJoined(home, ev, claim); err == nil {
						t.Error("claimed recorder succeeded without a durable ledger publication")
					}
					after, afterErr := os.ReadFile(ledgerPath)
					if errors.Is(beforeErr, os.ErrNotExist) {
						if !errors.Is(afterErr, os.ErrNotExist) {
							t.Errorf("failed first publication left a canonical ledger: err=%v body=%q", afterErr, after)
						}
					} else if afterErr != nil {
						t.Errorf("read canonical ledger after failure: %v", afterErr)
					} else if string(after) != string(before) {
						t.Errorf("failed publication changed canonical ledger\nbefore: %q\nafter:  %q", before, after)
					}
					if _, ok, err := LookupRequestID(home, ev.RequestID); err != nil || ok {
						t.Errorf("failed publication exposed request evidence: ok=%v err=%v", ok, err)
					}
					if _, err := os.Stat(requestClaimPath(home, ev.RequestID)); err != nil {
						t.Errorf("failed publication did not retain durable request claim: %v", err)
					}
				})
			}
		})
	}
}

func TestRecordClaimedJoinedRejectsSameInodeLedgerAppendBeforePublish(t *testing.T) {
	home := t.TempDir()
	ledgerPath := filepath.Join(home, "ledger", "execution-route-evidence.jsonl")

	seed, err := Build(BuildInput{
		CreatedAt:     t0.Add(-2 * time.Minute),
		RequestID:     "gateway:req_preimage_seed",
		WorkID:        "work_preimage_seed",
		ClientProfile: "gateway.ingress",
		Protocol:      "openai-chat-completions",
		Provider:      "fixture-provider",
		Model:         "fixture-model",
		Status:        "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordJoined(home, seed); err != nil {
		t.Fatalf("seed route evidence: %v", err)
	}
	before := readFile(t, ledgerPath)
	beforeInfo, err := os.Stat(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}

	concurrent, err := Build(BuildInput{
		CreatedAt:     t0.Add(-time.Minute),
		RequestID:     "gateway:req_preimage_concurrent",
		WorkID:        "work_preimage_concurrent",
		ClientProfile: "gateway.ingress",
		Protocol:      "openai-chat-completions",
		Provider:      "fixture-provider",
		Model:         "fixture-model",
		Status:        "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	concurrentRow, err := json.Marshal(concurrent)
	if err != nil {
		t.Fatal(err)
	}
	concurrentRow = append(concurrentRow, '\n')

	ev := fixtureEvidence()
	claim, err := BuildRequestClaim(ev.RequestID, ev.WorkID, ev.ClientProfile, ev.Protocol, ev.SelectedTarget)
	if err != nil {
		t.Fatal(err)
	}
	if err := ClaimRequestID(home, claim); err != nil {
		t.Fatalf("claim request id: %v", err)
	}

	previousHook := routeEvidenceBeforeLedgerReplace
	t.Cleanup(func() { routeEvidenceBeforeLedgerReplace = previousHook })
	injected := false
	var afterAppendInfo os.FileInfo
	routeEvidenceBeforeLedgerReplace = func() {
		if injected {
			return
		}
		injected = true
		canonical, err := os.OpenFile(ledgerPath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Errorf("open canonical ledger for concurrent append: %v", err)
			return
		}
		if _, err := canonical.Write(concurrentRow); err != nil {
			_ = canonical.Close()
			t.Errorf("append concurrent ledger row: %v", err)
			return
		}
		if err := canonical.Sync(); err != nil {
			_ = canonical.Close()
			t.Errorf("sync concurrent ledger row: %v", err)
			return
		}
		if err := canonical.Close(); err != nil {
			t.Errorf("close concurrent ledger row: %v", err)
			return
		}
		afterAppendInfo, err = os.Stat(ledgerPath)
		if err != nil {
			t.Errorf("stat concurrently appended ledger: %v", err)
		}
	}

	if _, err := RecordClaimedJoined(home, ev, claim); err == nil {
		t.Fatal("claimed recorder replaced a ledger that changed after its snapshot")
	}
	if !injected {
		t.Fatal("same-inode append was not injected")
	}
	if afterAppendInfo == nil || !os.SameFile(beforeInfo, afterAppendInfo) {
		t.Fatal("injected append did not mutate the captured ledger inode in place")
	}
	want := append(append([]byte(nil), before...), concurrentRow...)
	after := readFile(t, ledgerPath)
	if !bytes.Equal(after, want) {
		t.Fatalf("failed publication did not byte-preserve concurrent ledger row\nwant: %q\ngot:  %q", want, after)
	}
	if _, ok, err := LookupRequestID(home, ev.RequestID); err != nil || ok {
		t.Fatalf("failed publication exposed target request evidence: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(requestClaimPath(home, ev.RequestID)); err != nil {
		t.Fatalf("failed publication did not retain durable request claim: %v", err)
	}
}

func TestRecordJoinedIsIdempotentForIdenticalRequestEvidence(t *testing.T) {
	home := t.TempDir()
	ev := fixtureEvidence()
	first, err := RecordJoined(home, ev)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RecordJoined(home, ev)
	if err != nil {
		t.Fatal(err)
	}
	if second.Evidence.ID != first.Evidence.ID || second.Transition.ID != first.Transition.ID {
		t.Fatalf("idempotent record changed identity: first=%+v second=%+v", first, second)
	}
	rows := strings.Split(strings.TrimSpace(string(readFile(t, filepath.Join(home, "ledger", "execution-route-evidence.jsonl")))), "\n")
	if len(rows) != 1 {
		t.Fatalf("identical request evidence persisted %d rows, want 1", len(rows))
	}
}

func TestRecordJoinedIsIdempotentAfterEmptySliceRoundTrip(t *testing.T) {
	home := t.TempDir()
	ev, err := Build(BuildInput{
		CreatedAt:     t0,
		RequestID:     "gateway:req_empty_optional_slices",
		WorkID:        "work_empty_optional_slices",
		ClientProfile: "gateway.ingress",
		Protocol:      "openai-chat-completions",
		Provider:      "fixture-provider",
		Model:         "fixture-model",
		Status:        "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.EvidenceRefs == nil {
		t.Fatal("fixture must carry a non-nil empty optional slice before JSON round-trip")
	}
	if _, err := RecordJoined(home, ev); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordJoined(home, ev); err != nil {
		t.Fatalf("exact evidence replay after JSON round-trip was rejected: %v", err)
	}
	rows := strings.Split(strings.TrimSpace(string(readFile(t, filepath.Join(home, "ledger", "execution-route-evidence.jsonl")))), "\n")
	if len(rows) != 1 {
		t.Fatalf("exact empty-slice replay persisted %d rows, want 1", len(rows))
	}
}

func TestLookupRequestIDTreatsLegacyRecorderAsGeneric(t *testing.T) {
	home := t.TempDir()
	ev := fixtureEvidence()
	ev.RecordedVia = ""
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "ledger", "execution-route-evidence.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	recorded, ok, err := LookupRequestID(home, ev.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || recorded.RecordedVia != RecorderGeneric {
		t.Fatalf("legacy lookup = ok:%v recorded_via:%q, want generic", ok, recorded.RecordedVia)
	}
}

func TestTrustedPanelRuntimeRecorderHasNoProductionCallers(t *testing.T) {
	var callers []string
	for _, root := range []string{"..", filepath.Join("..", "..", "cmd")} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for lineNumber, line := range strings.Split(string(body), "\n") {
				if !strings.Contains(line, "RecordPanelRuntimeJoined(") {
					continue
				}
				if filepath.Base(path) == "routeevidence.go" && strings.Contains(line, "func RecordPanelRuntimeJoined(") {
					continue
				}
				callers = append(callers, fmt.Sprintf("%s:%d", path, lineNumber+1))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(callers) != 0 {
		t.Fatalf("trusted panel runtime recorder has production callers before an attested adapter exists: %v", callers)
	}
}

func TestDecodeRejectsUnknownPromptOrResponseFields(t *testing.T) {
	raw := `{
		"schema_version":"compound.execution_route_evidence.v0.1",
		"id":"route_secret",
		"created_at":"2026-06-29T12:00:00Z",
		"request_id":"gateway:req_secret",
		"client_profile":"gateway.ingress",
		"protocol":"openai-chat-completions",
		"selected_target":{"provider":"fixture-provider","model":"fixture-model","protocol":"openai-chat-completions"},
		"status":"completed",
		"prompt_body":"SECRET_PROMPT_BODY"
	}`
	_, err := Decode([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v, want unknown field rejection", err)
	}
}

func TestJoinTransitionRejectsMismatchedTransitionID(t *testing.T) {
	ev := fixtureEvidence()
	ev.TransitionID = "tr_wrong"
	_, _, err := JoinTransition(ev)
	if err == nil || !strings.Contains(err.Error(), "does not match computed") {
		t.Fatalf("err = %v, want transition mismatch", err)
	}
}

func TestProviderLedgerRowToEvidenceDropsPrivateSourcePayload(t *testing.T) {
	raw := `{"request_id":"req_provider_1","ts_start":"2026-06-29T10:00:00Z","caller":"codex","project":"product-x","purpose":"classification","route_alias":"cheap.classify","provider":"fixture-provider","model":"fixture-model","transport":"chat_completions","input_tokens":100,"output_tokens":20,"reasoning_tokens":5,"cache_read_tokens":10,"estimated_cost_usd":0.0025,"latency_ms":42,"status":"ok","source":{"private_path":"/Users/example/private/state.db","raw_prompt":"must not copy"}}`
	ev, err := ProviderLedgerRowToEvidence(raw, 1)
	if err != nil {
		t.Fatalf("provider ledger row: %v", err)
	}
	if ev.SelectedTarget.Provider != "fixture-provider" || ev.SelectedTarget.Model != "fixture-model" || ev.Protocol != "chat_completions" {
		t.Fatalf("target = %+v protocol=%q", ev.SelectedTarget, ev.Protocol)
	}
	if ev.Usage.PromptTokens != 110 || ev.Usage.CompletionTokens != 25 || ev.Usage.TotalTokens != 135 {
		t.Fatalf("usage = %+v", ev.Usage)
	}
	if ev.CostMicros != 2500 || ev.LatencyMS != 42 {
		t.Fatalf("cost/latency = %d/%d", ev.CostMicros, ev.LatencyMS)
	}
	joinedRefs := strings.Join(ev.EvidenceRefs, "\n")
	if strings.Contains(joinedRefs, "/Users/example") || strings.Contains(joinedRefs, "raw_prompt") {
		t.Fatalf("private source payload leaked into evidence refs: %s", joinedRefs)
	}
	if !strings.Contains(joinedRefs, "provider-ledger-request:req_provider_1") || !strings.Contains(joinedRefs, "provider-ledger-row:") {
		t.Fatalf("missing provider ledger refs: %s", joinedRefs)
	}
}

func TestImportProviderLedgerFileTailRecordsRouteEvidenceOnly(t *testing.T) {
	home := t.TempDir()
	ledgerPath := filepath.Join(t.TempDir(), "provider-calls.ndjson")
	rows := strings.Join([]string{
		`{"request_id":"req_old","ts_start":"2026-06-29T09:00:00Z","caller":"codex","project":"product-x","purpose":"old","route_alias":"cheap.classify","provider":"fixture-provider","model":"fixture-small","transport":"chat_completions","input_tokens":1,"output_tokens":2,"status":"ok"}`,
		`{"request_id":"req_new","run_id":"run_new","ts_start":"2026-06-29T10:00:00Z","caller":"codex","project":"product-x","purpose":"new","route_alias":"premium.reasoning","provider":"fixture-provider","model":"fixture-large","transport":"responses","input_tokens":100,"output_tokens":50,"estimated_cost_usd":0.01,"latency_ms":55,"status":"ok"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(ledgerPath, []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	summary, err := ImportProviderLedgerFile(home, ledgerPath, ProviderLedgerImportOptions{Tail: 1})
	if err != nil {
		t.Fatalf("import provider ledger: %v", err)
	}
	if summary.RowsRead != 2 || summary.Imported != 1 || summary.FirstID == "" || summary.LastID != summary.FirstID {
		t.Fatalf("summary = %+v", summary)
	}
	rowData := readFile(t, filepath.Join(home, "ledger", "execution-route-evidence.jsonl"))
	if strings.Count(string(rowData), "\n") != 1 || !strings.Contains(string(rowData), "req_new") || strings.Contains(string(rowData), "req_old") {
		t.Fatalf("bad route evidence ledger:\n%s", rowData)
	}
	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if exists, err := store.Exists(summary.Results[0].Transition.ID); err != nil || !exists {
		t.Fatalf("transition not recorded: exists=%v err=%v", exists, err)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("provider-ledger import must not promote claims, stat err=%v", err)
	}
}

func TestDoctorProviderLedgerCostsRecordsRouteObservationNoPromote(t *testing.T) {
	home := t.TempDir()
	ledgerPath := filepath.Join(t.TempDir(), "provider-calls.ndjson")
	rows := strings.Join([]string{
		`{"request_id":"req_cost_ok","ts_start":"2026-06-29T09:00:00Z","caller":"codex","project":"product-x","purpose":"classify","route_alias":"cheap.classify","provider":"fixture-provider","model":"fixture-small","transport":"chat_completions","input_tokens":10,"output_tokens":2,"estimated_cost_usd":0.0001,"metadata_quality":"complete","status":"ok"}`,
		`{"request_id":"req_cost_gap","ts_start":"2026-06-29T10:00:00Z","caller":"codex","project":"unknown_project","purpose":"deep-review","route_alias":"premium.reasoning","provider":"fixture-provider","model":"fixture-large","transport":"responses","input_tokens":100,"output_tokens":50,"metadata_quality":"partial","status":"ok"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(ledgerPath, []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := DoctorProviderLedgerCosts(ledgerPath, ProviderLedgerImportOptions{})
	if err != nil {
		t.Fatalf("cost doctor: %v", err)
	}
	if report.Status != "review" ||
		report.RowsChecked != 2 ||
		report.TokenUsageRows != 2 ||
		report.CostKnownRows != 1 ||
		report.TokenUsageMissingCost != 1 ||
		report.UnknownProjectRows != 1 ||
		report.PartialMetadataRows != 1 {
		t.Fatalf("report = %+v", report)
	}
	recorded, err := RecordCostDoctorObservation(home, report, t0)
	if err != nil {
		t.Fatalf("record cost observation: %v", err)
	}
	if recorded.CandidateID == "" || recorded.NoPromoteID == "" || !recorded.RecordedNoPromote {
		t.Fatalf("recorded report = %+v", recorded)
	}
	candidatePath := filepath.Join(home, candidates.InboxDir(candidates.RouteObservation), recorded.CandidateID+".json")
	data := readFile(t, candidatePath)
	var cand RouteCostCandidate
	if err := json.Unmarshal(data, &cand); err != nil {
		t.Fatalf("parse candidate: %v", err)
	}
	if cand.Kind != string(candidates.RouteObservation) ||
		cand.Verdict != candidates.NoPromote ||
		!strings.Contains(cand.Statement, "without cost metadata") {
		t.Fatalf("candidate = %+v", cand)
	}
	nopData := readFile(t, filepath.Join(home, "ledger", "no-promote-decisions.jsonl"))
	if !strings.Contains(string(nopData), cand.ID) || !strings.Contains(string(nopData), "route_cost_coverage") {
		t.Fatalf("no-promote rows:\n%s", nopData)
	}
	rerecorded, err := RecordCostDoctorObservation(home, report, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("rerecord cost observation: %v", err)
	}
	if rerecorded.CandidateID != recorded.CandidateID || rerecorded.RecordNoPromoteInserted {
		t.Fatalf("rerecorded report = %+v, want same candidate and no duplicate no-promote", rerecorded)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("cost doctor must not promote claims, stat err=%v", err)
	}
}

func fixtureEvidence() Evidence {
	ev, err := Build(BuildInput{
		CreatedAt:     t0,
		RequestID:     "gateway:req_fixture",
		WorkID:        "work_fixture",
		ClientProfile: "gateway.ingress",
		Protocol:      "openai-chat-completions",
		Provider:      "fixture-provider",
		Model:         "fixture-model",
		Usage:         Usage{PromptTokens: 8, CompletionTokens: 5, TotalTokens: 13},
		LatencyMS:     31,
		Status:        "completed",
		EvidenceRefs:  []string{"gateway:trace_fixture"},
	})
	if err != nil {
		panic(err)
	}
	return ev
}

func TestLoadAuthoritySummarizesExecutionRouteEvidence(t *testing.T) {
	home := t.TempDir()
	ledgerPath := filepath.Join(t.TempDir(), "provider-calls.ndjson")
	rows := strings.Join([]string{
		`{"request_id":"req_old","ts_start":"2026-06-29T09:00:00Z","caller":"codex","project":"product-x","purpose":"old","route_alias":"cheap.classify","provider":"fixture-provider","model":"fixture-small","transport":"chat_completions","input_tokens":1,"output_tokens":2,"status":"ok"}`,
		`{"request_id":"req_new","run_id":"run_new","ts_start":"2026-06-29T10:00:00Z","caller":"codex","project":"product-x","purpose":"new","route_alias":"premium.reasoning","provider":"fixture-provider","model":"fixture-large","transport":"responses","input_tokens":100,"output_tokens":50,"estimated_cost_usd":0.01,"latency_ms":55,"status":"ok"}`,
		"",
	}, "\n")
	if err := os.WriteFile(ledgerPath, []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportProviderLedgerFile(home, ledgerPath, ProviderLedgerImportOptions{}); err != nil {
		t.Fatalf("import provider ledger: %v", err)
	}
	report, err := LoadAuthority(home, 1)
	if err != nil {
		t.Fatalf("load authority: %v", err)
	}
	if report.SchemaVersion != RouteEvidenceAuthorityReportSchema ||
		report.AuthorityRef != RouteEvidenceAuthorityRef ||
		report.Status != "ok" ||
		!report.Installed ||
		report.RouteEvidenceCount != 2 ||
		report.ProviderLedgerEvidenceCount != 2 ||
		report.TokenUsageRows != 2 ||
		report.CostKnownRows != 1 ||
		len(report.Recent) != 1 {
		t.Fatalf("bad authority report: %+v", report)
	}
	if report.Recent[0].Provider != "fixture-provider" || report.Recent[0].RequestID != "req_new" {
		t.Fatalf("recent summary = %+v, want newest provider ledger row", report.Recent[0])
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("route evidence authority must not promote claims, stat err=%v", err)
	}
}

func writeEvidenceFile(t *testing.T, dir string, ev Evidence) string {
	t.Helper()
	data, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "route.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	if filepath.Base(filepath.Dir(path)) == "events" && strings.HasSuffix(path, ".json") {
		store, err := eventstore.Open(filepath.Dir(filepath.Dir(path)))
		if err != nil {
			t.Fatal(err)
		}
		record, err := store.Read(strings.TrimSuffix(filepath.Base(path), ".json"))
		if err != nil {
			t.Fatal(err)
		}
		return record.Bytes
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
