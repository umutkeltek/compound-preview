package arch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
)

// terminalWorkStates mirrors internal/work's terminalStates (completed,
// failed, cancelled) — duplicated because internal/arch must not import the
// work store for a single state check.
var terminalWorkStates = map[string]bool{"completed": true, "failed": true, "cancelled": true}

// proveable-pair pair-state: one work may emit any subset of {Receipt, Transition}.
// An orphan (one without the other) is a violation of the proveable-pair law.
type pairState struct {
	hasReceipt    bool
	hasTransition bool
	receiptID     string
	transitionID  string
}

// proveablePairGenesis is the ADR-0007 adoption instant (2026-08-02): the pair
// law applies FORWARD from it. Pre-genesis receipts/transitions are
// grandfathered — a real home carries tens of thousands of pre-law orphan
// records, and a repo gate must never red on home history the law did not
// govern. Records with a missing or unparseable created_at are pre-law by
// shape and skipped the same way.
var proveablePairGenesis = time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)

func proveablePairPreGenesis(createdAt string) bool {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(createdAt))
	if err != nil {
		return true
	}
	return t.Before(proveablePairGenesis)
}

// proveablePairPairAudit is the pure pair-detection core, factored out of the
// filesystem check so a unit test can drive it with synthetic JSON without
// materializing an eventstore. The function takes parsed envelope bodies, keyed
// by their canonical id, and returns the orphan work ids (any id where the
// receipt xor transition flag is set but not both) plus any parse-error count.
func proveablePairPairAudit(records map[string][]byte) (orphans []string, scanErrors int, seenWorks int) {
	pairs := map[string]*pairState{}
	for id, body := range records {
		var workID string
		var kind string
		var sessionID string
		switch {
		case strings.HasPrefix(id, "rec_"):
			var receipt envelope.Receipt
			if err := json.Unmarshal(body, &receipt); err != nil {
				scanErrors++
				continue
			}
			if proveablePairPreGenesis(receipt.CreatedAt) {
				continue
			}
			workID = strings.TrimSpace(receipt.IDs.WorkID)
			sessionID = strings.TrimSpace(receipt.IDs.SessionID)
			kind = "receipt"
		case strings.HasPrefix(id, "tr_"):
			var transition envelope.Transition
			if err := json.Unmarshal(body, &transition); err != nil {
				scanErrors++
				continue
			}
			if proveablePairPreGenesis(transition.CreatedAt) {
				continue
			}
			workID = strings.TrimSpace(transition.IDs.WorkID)
			kind = "transition"
		default:
			continue
		}
		if workID == "" {
			// ids.work_id is the pair-law join key. A RECEIPT with a
			// session_id but no work_id is a session receipt (internal/hook's
			// buildWorklessSessionReceipt mints valid ones): it closes a
			// session, not a work, so the pair law does not bind it — skip it
			// without a scan error. Anything else with no work_id (a receipt
			// with NEITHER id, or any transition — Transition.Validate requires
			// work_id) is malformed and stays a scan error.
			if kind != "receipt" || sessionID == "" {
				scanErrors++
			}
			continue
		}
		if pairs[workID] == nil {
			pairs[workID] = &pairState{}
		}
		switch kind {
		case "receipt":
			pairs[workID].hasReceipt = true
			pairs[workID].receiptID = id
		case "transition":
			pairs[workID].hasTransition = true
			pairs[workID].transitionID = id
		}
	}
	seenWorks = len(pairs)
	for workID, p := range pairs {
		if p.hasReceipt != p.hasTransition {
			orphans = append(orphans, workID)
		}
	}
	sort.Strings(orphans)
	return orphans, scanErrors, seenWorks
}

// checkProveablePairLaw scans the event store rooted at the resolved
// COMPOUND_HOME for orphan receipts or transitions: every work that closes must
// emit BOTH a Receipt AND a Transition sharing the same work_id. An orphan is
// a violation of the proveable-pair law (ADR-0007).
//
// homePath is the resolved COMPOUND_HOME; the doctor passes whatever
// home.Resolve("") returns, tests pass a synthetic temp home. An empty
// homePath that fails to resolve is vacuously true (no events to scan) and
// reported as Info rather than Fail.
//
// The check is read-only: it never mutates the home, never opens files for
// write, and never advances the eventstore cursor. It is fail-closed on
// individual parse errors (one corrupt record is a Warn, not a Fail), so a
// single bad record cannot brick the doctor.
//
// Severity ladder matches the convergence-ratchet pattern
// (arch.go convergenceDebt): WARN by default so the operator sees the orphan
// count without the build breaking on a known-historical violation, FAIL
// under --strict so the operator can opt into the enforcement gate once they
// have repaired the historical record. Escalation is a one-line flag flip,
// not a code change.
func checkProveablePairLaw(homePath string, strict bool) []Finding {
	var f []Finding
	resolved := strings.TrimSpace(homePath)
	if resolved == "" {
		var err error
		resolved, err = home.Resolve("")
		if err != nil {
			f = append(f, Finding{
				Check:    "proveable-pair-law",
				Severity: Info,
				Message:  "no COMPOUND_HOME resolvable; proveable-pair law is vacuously satisfied (no events to scan)",
			})
			return f
		}
	}
	store, err := eventstore.Open(resolved)
	if err != nil {
		f = append(f, Finding{
			Check:    "proveable-pair-law",
			Severity: Warn,
			Message:  "cannot open event store at COMPOUND_HOME=" + resolved + ": " + err.Error(),
		})
		return f
	}
	entries, err := store.Scan(eventstore.ScanOptions{Prefixes: []string{"rec_", "tr_"}})
	if err != nil {
		f = append(f, Finding{
			Check:    "proveable-pair-law",
			Severity: Warn,
			Message:  "event store scan failed at COMPOUND_HOME=" + resolved + ": " + err.Error(),
		})
		return f
	}
	if len(entries) == 0 {
		f = append(f, Finding{
			Check:    "proveable-pair-law",
			Severity: Info,
			Message:  "no receipts or transitions under COMPOUND_HOME=" + resolved + "; proveable-pair law is vacuously satisfied",
		})
		return f
	}
	records := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		rec, err := entry.Read()
		if err != nil {
			// A single unreadable record is a Warn, not a Fail: the law is
			// about pair shape, not about every record being parseable here.
			f = append(f, Finding{
				Check:    "proveable-pair-law",
				Severity: Warn,
				Message:  "event record " + entry.ID + " unreadable during proveable-pair audit: " + err.Error(),
			})
			continue
		}
		records[rec.ID] = rec.Bytes
	}
	orphans, scanErrors, seenWorks := proveablePairPairAudit(records)
	// The pair law binds work CLOSURES. Projections that mint lone Transitions
	// (route-evidence imports, policy/product imports) hang off synthetic ids
	// with no WorkEnvelope behind them — they are evidence surfaces, not
	// closed work, and the law does not govern them. A LIVE work (envelope
	// present, not yet terminal) legitimately shows a transition before its
	// closing receipt exists: the pair is only due AT close. An orphan only
	// violates the law when its envelope is terminal and the pair is broken.
	exempt := 0
	var closeOrphans []string
	for _, id := range orphans {
		body, err := os.ReadFile(filepath.Join(resolved, "work", "work", id+".json"))
		if err != nil {
			exempt++ // no envelope: projection-only, not a close
			continue
		}
		var w envelope.WorkEnvelope
		if err := json.Unmarshal(body, &w); err != nil || !terminalWorkStates[w.State] {
			exempt++ // live work: the pair is not due yet
			continue
		}
		closeOrphans = append(closeOrphans, id)
	}
	orphans = closeOrphans
	if len(orphans) == 0 {
		summary := itoa(seenWorks) + " post-genesis work(s) audited at COMPOUND_HOME=" + resolved + "; every close emitted its receipt+transition pair"
		if exempt > 0 {
			summary += "; " + itoa(exempt) + " projection-only transition(s) exempt (no work envelope — not closes)"
		}
		if scanErrors > 0 {
			f = append(f, Finding{
				Check:    "proveable-pair-law",
				Severity: Warn,
				Message:  summary + "; " + itoa(scanErrors) + " record(s) skipped due to parse errors",
			})
			return f
		}
		f = append(f, Finding{
			Check:    "proveable-pair-law",
			Severity: Info,
			Message:  summary,
		})
		return f
	}
	// Orphan detail: list up to 8 work ids plus a count so the doctor output
	// stays bounded; the rest is summarized so a thousand orphans does not
	// produce a ten-thousand-character finding.
	const maxOrphansListed = 8
	var details []string
	for i, workID := range orphans {
		if i >= maxOrphansListed {
			break
		}
		// Re-derive the orphan class from the audited pairs map so the message
		// reflects which side of the pair is missing.
		details = append(details, workID+" ("+orphanClassLabel(records, workID)+")")
	}
	msg := itoa(len(orphans)) + " work(s) violate the proveable-pair law at COMPOUND_HOME=" + resolved + ": one side of (Receipt, Transition) is missing; orphans: " + strings.Join(details, ", ")
	if len(orphans) > maxOrphansListed {
		msg += ", ... (" + itoa(len(orphans)-maxOrphansListed) + " more)"
	}
	if scanErrors > 0 {
		msg += "; " + itoa(scanErrors) + " record(s) skipped due to parse errors"
	}
	severity := Warn
	if strict {
		severity = Fail
	}
	f = append(f, Finding{
		Check:    "proveable-pair-law",
		Severity: severity,
		Message:  msg,
	})
	return f
}

// orphanClassLabel returns "receipt-only" if the work has a Receipt but no
// Transition, "transition-only" if vice versa, or "unknown" if neither side is
// recorded (a degenerate pair-state that should not occur in the audit map).
func orphanClassLabel(records map[string][]byte, workID string) string {
	var hasReceipt, hasTransition bool
	for id, body := range records {
		switch {
		case strings.HasPrefix(id, "rec_"):
			var r envelope.Receipt
			if err := json.Unmarshal(body, &r); err != nil {
				continue
			}
			if strings.TrimSpace(r.IDs.WorkID) == workID {
				hasReceipt = true
			}
		case strings.HasPrefix(id, "tr_"):
			var t envelope.Transition
			if err := json.Unmarshal(body, &t); err != nil {
				continue
			}
			if strings.TrimSpace(t.IDs.WorkID) == workID {
				hasTransition = true
			}
		}
	}
	switch {
	case hasReceipt && !hasTransition:
		return "receipt-only"
	case hasTransition && !hasReceipt:
		return "transition-only"
	default:
		return "unknown"
	}
}
