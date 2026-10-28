package narrate

import (
	"fmt"
	"math"

	"compound/internal/attention"
)

// TrafficSplit is the cheap-share input: the deterministic split of routed model
// traffic into non-premium (zero marginal cost) vs paid calls, computed upstream
// by the route-evidence owner. narrate recomputes NONE of it — it only renders
// one beat from the numbers, mirroring how ReceiptSummary carries an already-
// derived acknowledge stream. Present is false when no provider-ledger
// projection has been imported, so the beat is simply absent (never a fabricated
// 0% / 100%). The split axis is spend-based; the beat body states so, keeping the
// number self-describing.
type TrafficSplit struct {
	Present     bool  `json:"present"`
	TotalCalls  int   `json:"total_calls"`
	CheapCalls  int   `json:"cheap_calls"`
	TotalTokens int64 `json:"total_tokens"`
	CheapTokens int64 `json:"cheap_tokens"`
}

// trafficThreadID + trafficVoice name the ambient cheap-share stream. It is not
// an attention member and not an acknowledgement — like acks it is an additional
// conversational stream outside the attention conservation accounting, so it
// never disturbs narrated+folded==total_members.
const (
	trafficThreadID = "route|cheap-share"
	trafficVoice    = "The meter"
)

// trafficBaseWeight keeps the ambient cost report below asks/warns/offers so it
// reads as a standing glance, not a demand. Mood re-weights it like any report.
const trafficBaseWeight = 0.4

// composeTraffic authors the ONE cheap-share beat: the share of routed model
// traffic that ran on zero-cost (non-premium) lanes, by call count and — when
// token usage was recorded — by token volume. It returns ok=false when no route
// ledger has been imported or no traffic was routed, so the beat degrades to
// absent rather than a fake ratio (spec: honest degradation).
func composeTraffic(t TrafficSplit, mood Mood) (Beat, bool) {
	if !t.Present || t.TotalCalls <= 0 {
		return Beat{}, false
	}
	body := trafficBody(t)
	return Beat{
		Act:        ActReport,
		Voice:      trafficVoice,
		Body:       body,
		Weight:     clamp01(trafficBaseWeight * moodFactor(mood, ActReport)),
		ThreadID:   trafficThreadID,
		Since:      Since{State: SinceUnchanged},
		PathReason: "a standing read on where your model traffic runs",
		Register:   RegisterStanding,
		Provenance: Provenance{Refs: []string{}, DetailRef: "route:cheap-share"},
		Freshness:  attention.Freshness{Source: "route-ledger"},
	}, true
}

// trafficBody composes the self-describing spend-based sentence and falls back to
// a valid generic line if the composed prose cannot pass the operator grammar —
// the true call split is preserved in every branch (mirrors ackBody).
func trafficBody(t TrafficSplit) string {
	body := fmt.Sprintf("By spend, %d of %d routed calls (%d%%) ran on zero-cost lanes",
		t.CheapCalls, t.TotalCalls, percent(int64(t.CheapCalls), int64(t.TotalCalls)))
	if t.TotalTokens > 0 {
		body += fmt.Sprintf("; %d%% of token volume stayed off paid lanes.", percent(t.CheapTokens, t.TotalTokens))
	} else {
		body += "."
	}
	if proseOK(body, "", trafficVoice, ActReport) {
		return body
	}
	fallback := fmt.Sprintf("By spend, %d of %d routed calls ran on zero-cost lanes.", t.CheapCalls, t.TotalCalls)
	if proseOK(fallback, "", trafficVoice, ActReport) {
		return fallback
	}
	return "A read on where your model traffic runs is ready."
}

// percent rounds n/d to the nearest whole percent deterministically. d<=0 yields
// 0 (the caller never renders a beat in that case).
func percent(n, d int64) int {
	if d <= 0 {
		return 0
	}
	return int(math.Round(float64(n) / float64(d) * 100))
}
