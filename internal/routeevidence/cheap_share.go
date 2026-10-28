package routeevidence

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CheapShareSchemaVersion names the cheap-share traffic-split projection: the
// deterministic share of routed model traffic that ran on a non-premium lane.
const CheapShareSchemaVersion = "compound.route_cheap_share.v0.1"

// CheapShareBasis stamps the split axis onto every CheapShare so a downstream
// renderer produces a self-describing number. The split is deliberately
// spend-based, not lane-tier-based: a routed call is "cheap" (non-premium) when
// it incurred no marginal cost (cost_micros == 0), i.e. it never touched the
// rate card. This is the doctrine's "savings are the traffic split, not the rate
// card" read against the one field every route-evidence row carries. It is an
// honest floor on the non-premium share: a cheap-but-paid lane counts on the
// paid side, so the true non-premium share is never lower than this number.
const CheapShareBasis = "spend:zero-cost-vs-paid"

// CheapShare is the spend-based free/paid split of imported route evidence,
// counted by call and by token volume. It is a projection, not authority: the
// owner is the execution-route-evidence ledger, and ComputeCheapShare recomputes
// nothing beyond summing that evidence.
type CheapShare struct {
	SchemaVersion string `json:"schema_version"`
	Basis         string `json:"basis"`
	Present       bool   `json:"present"`
	TotalCalls    int    `json:"total_calls"`
	CheapCalls    int    `json:"cheap_calls"`
	PaidCalls     int    `json:"paid_calls"`
	TotalTokens   int64  `json:"total_tokens"`
	CheapTokens   int64  `json:"cheap_tokens"`
	PaidTokens    int64  `json:"paid_tokens"`
}

// ComputeCheapShare is the pure reducer over route-evidence rows. A row is
// counted cheap when it recorded no marginal cost (CostMicros <= 0) and paid
// otherwise; token volume uses the row's total usage. Present is false only when
// there is no routed traffic at all, so a caller can render "absent" instead of
// a fabricated 0% / 100%. It reads no filesystem and no clock (determinism).
func ComputeCheapShare(rows []Evidence) CheapShare {
	share := CheapShare{SchemaVersion: CheapShareSchemaVersion, Basis: CheapShareBasis}
	for _, ev := range rows {
		share.TotalCalls++
		tokens := int64(nonNegative(ev.Usage.TotalTokens))
		share.TotalTokens += tokens
		if ev.CostMicros > 0 {
			share.PaidCalls++
			share.PaidTokens += tokens
			continue
		}
		share.CheapCalls++
		share.CheapTokens += tokens
	}
	share.Present = share.TotalCalls > 0
	return share
}

// LoadRouteEvidenceLedger reads every recorded route-evidence row from the home
// ledger. exists reports whether the ledger file is present at all, so a caller
// can distinguish "no provider ledger imported" from "imported but empty". Each
// row is decoded strictly (unknown fields rejected, schema_version validated),
// so a silent projection-shape drift surfaces as a load error here rather than a
// wrong ratio downstream.
func LoadRouteEvidenceLedger(homePath string) (rows []Evidence, exists bool, err error) {
	path := filepath.Join(homePath, "ledger", executionRouteEvidenceLedger)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		ev, decErr := Decode([]byte(line))
		if decErr != nil {
			return nil, true, fmt.Errorf("route evidence ledger line %d: %w", i+1, decErr)
		}
		rows = append(rows, ev)
	}
	return rows, true, nil
}
