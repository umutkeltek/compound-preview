package work

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

func TestProofChainDoctorConsumesExactVerificationRejection(t *testing.T) {
	for _, tc := range []string{"exact acknowledgement", "absent acknowledgement", "other attempt", "empty reason", "unsealed receipt"} {
		t.Run(tc, func(t *testing.T) {
			s := newStore(t)
			id, _ := closeTerminalizeWork(t, s, "synthetic-verifier", fixedNow)
			ra := attachTerminalCloseHistoryAttempt(t, s, id, "", runattempt.StatusSucceeded, true, fixedNow.Add(4*time.Minute))
			before := runAttemptBytesForCloseTest(t, s.home, ra.ID)
			closed, err := s.Close(id, verificationRejectedCloseInput(ra.ID, "result did not pass verification"), fixedNow.Add(10*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if tc != "exact acknowledgement" {
				refs := []string{}
				for _, ref := range closed.Receipt.ProofRefs {
					if strings.HasPrefix(ref, "verification_rejected:") {
						switch tc {
						case "absent acknowledgement":
							continue
						case "other attempt":
							ref = "verification_rejected:ratt_20260701T000000Z_aaaaaaaa:rejected"
						case "empty reason":
							ref = "verification_rejected:" + ra.ID + ": "
						case "unsealed receipt":
							ref += " altered"
						}
					}
					refs = append(refs, ref)
				}
				closed.Receipt.ProofRefs = refs
				if err := s.replaceEvent(closed.Receipt.ID, &closed.Receipt); err != nil {
					t.Fatal(err)
				}
				if tc != "unsealed receipt" {
					// Keep the exact close seal valid so the negative control
					// probes acknowledgement semantics, not a hash mismatch.
					digest, err := receiptEnvelopeSHA256(closed.Receipt)
					if err != nil {
						t.Fatal(err)
					}
					body, err := os.ReadFile(s.eventsLog())
					if err != nil {
						t.Fatal(err)
					}
					lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
					matched := 0
					for i, line := range lines {
						var event Event
						if err := json.Unmarshal([]byte(line), &event); err != nil {
							t.Fatal(err)
						}
						if event.WorkID == id && event.To == "failed" {
							event.ReceiptSHA256 = digest
							encoded, err := json.Marshal(event)
							if err != nil {
								t.Fatal(err)
							}
							lines[i] = string(encoded)
							matched++
						}
					}
					if matched != 1 {
						t.Fatalf("closing event matches=%d", matched)
					}
					if err := os.WriteFile(s.eventsLog(), []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
					if _, err := s.loadExactTerminalClose(closed.Work); err != nil {
						t.Fatalf("negative control lost exact-close validity: %v", err)
					}
				}
			}
			report, err := s.ProofChainDoctor(fixedNow.Add(11*time.Minute), ProofChainDoctorOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if tc == "exact acknowledgement" {
				if report.Status != "ok" || report.Counts.Complete != 1 {
					t.Fatalf("truthfully rejected result failed certification: %+v", report)
				}
			} else if report.Status != "fail" || report.Counts.RunAttemptOutcomeMismatch != 1 {
				t.Fatalf("invalid acknowledgement certified: %+v", report)
			}
			if !bytes.Equal(before, runAttemptBytesForCloseTest(t, s.home, ra.ID)) {
				t.Fatal("doctor rewrote execution evidence")
			}
		})
	}
}
