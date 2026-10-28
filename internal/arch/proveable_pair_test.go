package arch

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
)

// postGenesis stamp so synthetic records are governed by the pair law.
const pairAuditStamp = "2026-08-15T10:00:00Z"

func receiptBody(id, workID, sessionID string) []byte {
	ids := map[string]string{}
	if workID != "" {
		ids["work_id"] = workID
	}
	if sessionID != "" {
		ids["session_id"] = sessionID
	}
	body := `{"id":"` + id + `","created_at":"` + pairAuditStamp + `"`
	if len(ids) > 0 {
		body += `,"ids":{"work_id":"` + ids["work_id"] + `","session_id":"` + ids["session_id"] + `"}`
	}
	return []byte(body + `}`)
}

func transitionBody(id, workID string) []byte {
	return []byte(`{"id":"` + id + `","created_at":"` + pairAuditStamp + `","ids":{"work_id":"` + workID + `"}}`)
}

// TestProveablePairSessionReceiptIsNotAScanError pins the fixed behavior: a
// receipt with an empty ids.work_id but a nonempty ids.session_id is a
// legitimate session receipt (internal/hook's buildWorklessSessionReceipt mints
// exactly this shape) and must not inflate scanErrors nor invent a pair.
func TestProveablePairSessionReceiptIsNotAScanError(t *testing.T) {
	records := map[string][]byte{
		"rec_w1":   receiptBody("rec_w1", "work_w1", ""),
		"tr_w1":    transitionBody("tr_w1", "work_w1"),
		"rec_sess": receiptBody("rec_sess", "", "sess_alpha"),
	}
	orphans, scanErrors, seenWorks := proveablePairPairAudit(records)
	if scanErrors != 0 {
		t.Fatalf("session receipt counted as scan error: scanErrors=%d (a valid session receipt must not inflate parse-error count)", scanErrors)
	}
	if len(orphans) != 0 {
		t.Fatalf("session receipt invented an orphan pair: %v", orphans)
	}
	if seenWorks != 1 {
		t.Fatalf("seenWorks=%d, want 1 (the session receipt must not join the work-pair map)", seenWorks)
	}

	// A LONE session receipt is equally legitimate: no work closure is due.
	orphans, scanErrors, seenWorks = proveablePairPairAudit(map[string][]byte{
		"rec_sess": receiptBody("rec_sess", "", "sess_alpha"),
	})
	if scanErrors != 0 || len(orphans) != 0 || seenWorks != 0 {
		t.Fatalf("lone session receipt: scanErrors=%d orphans=%v seenWorks=%d, want all zero/empty", scanErrors, orphans, seenWorks)
	}
}

// TestProveablePairReceiptWithNeitherIDStaysScanError pins the guardrail: a
// receipt with neither work_id nor session_id is malformed and must still
// count as a scan error; the same holds for a transition without work_id.
func TestProveablePairReceiptWithNeitherIDStaysScanError(t *testing.T) {
	cases := []struct {
		name    string
		records map[string][]byte
	}{
		{"receipt with neither id", map[string][]byte{"rec_bad": receiptBody("rec_bad", "", "")}},
		{"receipt with whitespace session id only", map[string][]byte{"rec_bad": []byte(`{"id":"rec_bad","created_at":"` + pairAuditStamp + `","ids":{"session_id":"  "}}`)}},
		{"transition without work_id", map[string][]byte{"tr_bad": []byte(`{"id":"tr_bad","created_at":"` + pairAuditStamp + `","ids":{"session_id":"sess_alpha"}}`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orphans, scanErrors, seenWorks := proveablePairPairAudit(tc.records)
			if scanErrors != 1 {
				t.Fatalf("scanErrors=%d, want 1 (a record with no join id must stay a scan error)", scanErrors)
			}
			if len(orphans) != 0 || seenWorks != 0 {
				t.Fatalf("orphans=%v seenWorks=%d, want empty/0", orphans, seenWorks)
			}
		})
	}
}

// TestCheckProveablePairLawLiveDoctorSessionReceiptFalsifier drives the real
// doctor check against a real eventstore in a synthetic temp home (the seam
// checkProveablePairLaw documents for tests) and falsifies the old claim that
// a valid session receipt makes the doctor report parse errors.
func TestCheckProveablePairLawLiveDoctorSessionReceiptFalsifier(t *testing.T) {
	// The session receipt must be a VALID envelope — the falsifier only holds
	// if the doctor wrongly rejects something envelope.Validate accepts.
	sessionReceipt := &envelope.Receipt{
		Header: envelope.Header{
			SchemaVersion: envelope.SVReceipt,
			ID:            "rec_session_valid",
			CreatedAt:     time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC).Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "hook", Name: "compound"},
			IDs:           envelope.IDs{SessionID: "sess_live"},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Outcome:   "completed",
		MoveType:  "audit_only",
		ProofRefs: []string{"transcript:/tmp/sess.jsonl"},
	}
	if err := sessionReceipt.Validate(); err != nil {
		t.Fatalf("fixture is not a valid receipt: %v", err)
	}

	seed := func(t *testing.T, extra map[string]any) []Finding {
		t.Helper()
		homePath := t.TempDir()
		store, err := eventstore.Open(homePath)
		if err != nil {
			t.Fatalf("open event store: %v", err)
		}
		if err := store.Ensure(); err != nil {
			t.Fatalf("ensure event store: %v", err)
		}
		if _, err := store.PutJSON("rec_session_valid", sessionReceipt, ""); err != nil {
			t.Fatalf("put session receipt: %v", err)
		}
		for id, value := range extra {
			if _, err := store.PutJSON(id, value, ""); err != nil {
				t.Fatalf("put %s: %v", id, err)
			}
		}
		return checkProveablePairLaw(homePath, false)
	}

	t.Run("valid session receipt must not warn", func(t *testing.T) {
		findings := seed(t, map[string]any{
			"rec_w1": json.RawMessage(receiptBody("rec_w1", "work_w1", "")),
			"tr_w1":  json.RawMessage(transitionBody("tr_w1", "work_w1")),
		})
		if len(findings) != 1 {
			t.Fatalf("findings=%+v, want the single summary finding", findings)
		}
		got := findings[0]
		if got.Severity != Info {
			t.Fatalf("severity=%s, want info (a valid session receipt is not a doctor defect): %s", got.Severity, got.Message)
		}
		if strings.Contains(got.Message, "parse errors") {
			t.Fatalf("session receipt surfaced as a parse error: %s", got.Message)
		}
	})

	t.Run("receipt with neither id must still warn", func(t *testing.T) {
		findings := seed(t, map[string]any{
			"rec_bad": json.RawMessage(receiptBody("rec_bad", "", "")),
		})
		if len(findings) != 1 {
			t.Fatalf("findings=%+v, want the single summary finding", findings)
		}
		got := findings[0]
		if got.Severity != Warn {
			t.Fatalf("severity=%s, want warn (a receipt with no ids must stay a scan error): %s", got.Severity, got.Message)
		}
		if !strings.Contains(got.Message, "1 record(s) skipped due to parse errors") {
			t.Fatalf("message missing scan-error count: %s", got.Message)
		}
	})
}
