package narrate

import (
	"fmt"

	"compound/internal/attention"
)

// InjectionLegibility beat constants mirror traffic.go's additional-stream
// pattern: it is not an attention member and not an acknowledgement, so it never
// disturbs narrated+folded==total_members.
const (
	injectionThreadID = "memory|injection"
	injectionVoice    = "The meter"
)

// injectionBaseWeight keeps the injection glance below asks/warns/offers so it
// reads as a standing glance, not a demand — the operator-legibility half of the
// memory-injection observation work.
const injectionBaseWeight = 0.4

// composeInjection authors the ONE injection-legibility beat: how much compound
// context the latest turn served (memory/positions/offers), how much it excluded,
// and how big the rendered injection was. It returns ok=false when no ContextPack
// has been observed, so the beat degrades to absent (honest degradation, never a
// fabricated "0 served").
func composeInjection(l *InjectionLegibility, mood Mood) (Beat, bool) {
	if l == nil || !l.Present {
		return Beat{}, false
	}
	body := injectionBody(l)
	return Beat{
		Act:        ActReport,
		Voice:      injectionVoice,
		Body:       body,
		Weight:     clamp01(injectionBaseWeight * moodFactor(mood, ActReport)),
		ThreadID:   injectionThreadID,
		Since:      Since{State: SinceUnchanged},
		PathReason: "a standing read on what this turn's context injection served",
		Register:   RegisterStanding,
		Provenance: Provenance{Refs: []string{}, DetailRef: "context:packs:" + l.PackID},
		Freshness:  attention.Freshness{Source: "context-pack"},
	}, true
}

// injectionBody composes the one-line meter. The true counts are preserved in
// every branch; the prose only narrates them.
func injectionBody(l *InjectionLegibility) string {
	var parts []string
	if l.ServedMemory > 0 {
		parts = append(parts, fmt.Sprintf("%d memory", l.ServedMemory))
	}
	if l.ServedPositions > 0 {
		parts = append(parts, fmt.Sprintf("%d positions", l.ServedPositions))
	}
	if l.ServedOffers > 0 {
		parts = append(parts, fmt.Sprintf("%d offers", l.ServedOffers))
	}
	served := fmt.Sprintf("%d refs served", l.ServedRefs)
	if len(parts) > 0 {
		served += " (" + joinComma(parts) + ")"
	}
	excluded := ""
	if l.ExcludedRefs > 0 {
		excluded = fmt.Sprintf(", %d excluded", l.ExcludedRefs)
		if l.ExcludedNoTask > 0 {
			excluded += fmt.Sprintf(" (%d no-task)", l.ExcludedNoTask)
		}
	}
	bytes := "0KB"
	if l.InjectionBytes > 0 {
		bytes = fmt.Sprintf("~%dKB", (l.InjectionBytes+1023)/1024)
	}
	return fmt.Sprintf("This turn: %s%s — %s injected.", served, excluded, bytes)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " / "
		}
		out += p
	}
	return out
}
