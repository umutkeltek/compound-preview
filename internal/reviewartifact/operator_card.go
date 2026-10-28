package reviewartifact

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	BandNeedsYou = "needs_you"
	BandWatching = "watching"
	BandFiled    = "filed"

	SummarySourceWriter    = "writer"
	SummarySourceBackfill  = "backfill"
	SummarySourceAnnotator = "annotator"
	SummarySourceFallback  = "fallback"
)

var operatorCardBanlist = regexp.MustCompile(`(=|--|_[a-f0-9]{8,}|/Users/|\.json|\.md)`)

var bracketTagPattern = regexp.MustCompile(`\[[^\]]*\]?`)

// SanitizeFragment is the shared sentence-hygiene pass every operator-card
// template funnels through (via ShortLabel/PlainLabel): it strips
// [bracketed] machine tags (e.g. "[synthetic]", "[session PRIVACY...]") that
// leak into free-text fields such as a target's raw summary, then collapses
// whitespace. It never truncates; ShortLabel owns truncation, at a word
// boundary, so an ellipsis never lands mid-token.
func SanitizeFragment(value string) string {
	value = bracketTagPattern.ReplaceAllString(value, " ")
	return strings.Join(strings.Fields(value), " ")
}

// OperatorCard is the plain-language surface every core review artifact must
// carry for operator queues and console projections.
type OperatorCard struct {
	Summary      string `json:"operator_summary,omitempty"`
	ContextLabel string `json:"context_label,omitempty"`
	GroupKey     string `json:"group_key,omitempty"`
	Unlock       string `json:"operator_unlock,omitempty"`
}

func (c OperatorCard) normalized() OperatorCard {
	return OperatorCard{
		Summary:      strings.TrimSpace(c.Summary),
		ContextLabel: strings.TrimSpace(c.ContextLabel),
		GroupKey:     strings.TrimSpace(c.GroupKey),
		Unlock:       strings.TrimSpace(c.Unlock),
	}
}

// ValidateOperatorCard enforces the S1 operator sentence grammar checks that
// can be validated mechanically at the writer seam.
func ValidateOperatorCard(card OperatorCard) error {
	card = card.normalized()
	if card.Summary == "" {
		return fmt.Errorf("operator_summary required")
	}
	if utf8.RuneCountInString(card.Summary) > 160 {
		return fmt.Errorf("operator_summary exceeds 160 chars")
	}
	if strings.ContainsAny(card.Summary, "\r\n") {
		return fmt.Errorf("operator_summary must be one line")
	}
	if !strings.HasSuffix(card.Summary, "?") && !strings.HasSuffix(card.Summary, ".") {
		return fmt.Errorf("operator_summary must end with ? or .")
	}
	if card.ContextLabel == "" {
		return fmt.Errorf("context_label required")
	}
	if utf8.RuneCountInString(card.ContextLabel) > 60 {
		return fmt.Errorf("context_label exceeds 60 chars")
	}
	if strings.ContainsAny(card.ContextLabel, "\r\n") {
		return fmt.Errorf("context_label must be one line")
	}
	if operatorCardBanlist.MatchString(card.Summary) {
		return fmt.Errorf("operator_summary contains banned machine syntax")
	}
	if operatorCardBanlist.MatchString(card.ContextLabel) {
		return fmt.Errorf("context_label contains banned machine syntax")
	}
	if card.GroupKey == "" {
		return fmt.Errorf("group_key required")
	}
	before, after, ok := strings.Cut(card.GroupKey, "|")
	if !ok || strings.TrimSpace(before) == "" || strings.TrimSpace(after) == "" {
		return fmt.Errorf("group_key must have {area}|{shape} form")
	}
	if strings.ContainsAny(card.GroupKey, "\r\n") {
		return fmt.Errorf("group_key must be one line")
	}
	if utf8.RuneCountInString(card.Unlock) > 90 {
		return fmt.Errorf("operator_unlock exceeds 90 chars")
	}
	if strings.Contains(card.Unlock, "?") {
		return fmt.Errorf("operator_unlock must not ask a question")
	}
	if strings.ContainsAny(card.Unlock, "\r\n") {
		return fmt.Errorf("operator_unlock must be one line")
	}
	return nil
}

type operatorCardProvider interface {
	ReviewOperatorCard() (OperatorCard, error)
}

func injectOperatorCardJSON(data []byte, card OperatorCard, indent string, summarySource ...string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	PutOperatorCard(payload, card)
	if source := FirstNonEmpty(summarySource...); source != "" {
		payload["summary_source"] = source
	}
	return json.MarshalIndent(payload, "", indent)
}

// OperatorCardFromMap sniffs the card fields from a raw JSON object.
func OperatorCardFromMap(payload map[string]any) (OperatorCard, bool) {
	card := OperatorCard{
		Summary:      firstString(payload, "operator_summary"),
		ContextLabel: firstString(payload, "context_label"),
		GroupKey:     firstString(payload, "group_key"),
		Unlock:       firstString(payload, "operator_unlock"),
	}
	return card, card.Summary != "" || card.ContextLabel != "" || card.GroupKey != "" || card.Unlock != ""
}

// PutOperatorCard writes the card fields into a generic payload map.
func PutOperatorCard(payload map[string]any, card OperatorCard) map[string]any {
	if payload == nil {
		payload = map[string]any{}
	}
	card = card.normalized()
	payload["operator_summary"] = card.Summary
	payload["context_label"] = card.ContextLabel
	payload["group_key"] = card.GroupKey
	if card.Unlock != "" {
		payload["operator_unlock"] = card.Unlock
	} else {
		delete(payload, "operator_unlock")
	}
	return payload
}

func NewOperatorCard(summary, contextLabel, groupKey, unlock string) (OperatorCard, error) {
	card := OperatorCard{
		Summary:      ShortLabel(summary, 160),
		ContextLabel: ShortLabel(contextLabel, 60),
		GroupKey:     strings.TrimSpace(groupKey),
		Unlock:       ShortLabel(unlock, 90),
	}
	if err := ValidateOperatorCard(card); err != nil {
		return OperatorCard{}, err
	}
	return card.normalized(), nil
}

func RequireText(field string, values ...string) (string, error) {
	value := FirstNonEmpty(values...)
	if value == "" {
		return "", fmt.Errorf("%s required for operator card", field)
	}
	return value, nil
}

// SetOperatorCard applies a validated card to a queue item. Annotators use it
// when they refine lifecycle state after the writer-created card is loaded.
func SetOperatorCard(item *QueueItem, card OperatorCard, source, band string) error {
	if item == nil {
		return nil
	}
	if err := ValidateOperatorCard(card); err != nil {
		return err
	}
	card = card.normalized()
	item.OperatorSummary = card.Summary
	item.ContextLabel = card.ContextLabel
	item.GroupKey = card.GroupKey
	item.OperatorUnlock = card.Unlock
	item.SummarySource = FirstNonEmpty(source, SummarySourceAnnotator)
	item.Band = FirstNonEmpty(band, BandForStatus(item.Status, item.Readiness))
	return nil
}

// BandForStatus gives queue readers a deterministic fallback band. Domain
// writers and annotators should set a more specific band for hold/report cases.
func BandForStatus(status, readiness string) string {
	status = strings.TrimSpace(status)
	readiness = strings.TrimSpace(readiness)
	// Terminal words are owned by terminalStatuses above; this switch used to
	// re-list them and its copy omitted "completed", banding finished work
	// needs_you whenever no annotator had set a readiness.
	if TerminalStatus(status) {
		return BandFiled
	}
	switch status {
	case "held", "blocked":
		return BandWatching
	case "review_required", "unknown", "":
		return BandNeedsYou
	}
	switch readiness {
	case "review_recorded", "no_action", "ok":
		return BandFiled
	case "blocked":
		return BandWatching
	case "needs_review", "ready_to_apply":
		return BandNeedsYou
	default:
		return BandNeedsYou
	}
}

// DerivedOperatorCard composes the plain-language card for a core review
// artifact whose writer authored none: it states WHAT the record is, WHY it
// sits in front of the operator, and ONE concrete action — derived from the
// record's own payload, never a generic placeholder. Every candidate is gated
// by ValidateOperatorCard and the eighth-grade readability ceiling, degrading
// deterministically (record title → plain area → "review record") until one
// passes, so the returned card always validates and always reads at or below
// the ceiling.
func DerivedOperatorCard(payload map[string]any, area string) OperatorCard {
	context := derivedContextLabel(area)
	groupKey := FirstNonEmpty(area, "unclassified") + "|review"
	unlocks := derivedUnlocks(payload, area)
	for _, summary := range derivedSummaries(payload, area) {
		for _, unlock := range unlocks {
			card := OperatorCard{
				Summary:      summary,
				ContextLabel: context,
				GroupKey:     groupKey,
				Unlock:       unlock,
			}
			if ValidateOperatorCard(card) != nil {
				continue
			}
			if !Readable(card.Summary) || !Readable(card.Unlock) {
				continue
			}
			return card
		}
	}
	// Terminal rung: constant prose that passes both gates by construction.
	// Reachable only when the area itself is pathological; it still states
	// what, why, and one action.
	return OperatorCard{
		Summary:      "A new review record needs your decision.",
		ContextLabel: "Review",
		GroupKey:     groupKey,
		Unlock:       "Open the review queue and read this record.",
	}
}

// derivedContextLabel plain-language-izes the area, degrading to "Review"
// when the label would carry banned machine syntax.
func derivedContextLabel(area string) string {
	label := ShortLabel(PlainLabel(area), 60)
	if label == "" || operatorCardBanlist.MatchString(label) {
		return "Review"
	}
	return label
}

// derivedTitleClause lifts the record's own title-ish first clause to name
// WHAT the artifact is, capped at 90 runes so the fused sentence stays inside
// the 160-rune summary cap.
func derivedTitleClause(payload map[string]any) string {
	raw := firstString(payload, "title", "subject", "summary", "claim", "question")
	if raw == "" {
		return ""
	}
	clause := SanitizeFragment(strings.TrimSuffix(FirstClause(raw), "."))
	clause = ShortLabel(clause, 90)
	clause = strings.TrimRight(clause, " .,:;-…")
	return strings.TrimSpace(clause)
}

// derivedWhats names the record for the summary sentence, most specific first.
func derivedWhats(payload map[string]any, area string) []string {
	var whats []string
	if title := derivedTitleClause(payload); title != "" {
		whats = append(whats, title)
	}
	if plain := PlainLabel(area); plain != "" {
		whats = append(whats, "A new "+plain+" record")
	}
	return append(whats, "A new review record")
}

// derivedReasonClause reduces blocked_reason to a short plain clause.
func derivedReasonClause(raw string) string {
	clause := SanitizeFragment(strings.TrimSuffix(FirstClause(raw), "."))
	clause = ShortLabel(clause, 60)
	clause = strings.TrimRight(clause, " .,:;-…")
	return strings.TrimSpace(clause)
}

// derivedSummaries fuses WHAT and WHY into one sentence per (what × why)
// candidate, ordered most specific first. The why clause comes from the
// record's own status/readiness/blocked_reason; each why has a shorter
// fallback that drops the free-text clause.
func derivedSummaries(payload map[string]any, area string) []string {
	status := firstString(payload, "status", "verdict", "result")
	readiness := firstString(payload, "readiness")
	blocked := derivedReasonClause(firstString(payload, "blocked_reason"))
	var out []string
	for _, what := range derivedWhats(payload, area) {
		var variants []string
		switch {
		case TerminalStatus(status):
			variants = []string{what + " is already finished.", what + " has an outcome recorded."}
		case blocked != "":
			variants = []string{what + " is blocked: " + blocked + ".", what + " is blocked."}
		case status == "held" || status == "blocked" || readiness == "blocked":
			variants = []string{what + " is held until you decide.", what + " needs your decision."}
		case readiness == "ready_to_apply":
			variants = []string{what + " is ready to apply."}
		default:
			// review_required, unknown, and anything unclassified: the honest ask.
			variants = []string{what + " needs your decision."}
		}
		out = append(out, variants...)
	}
	return out
}

// derivedUnlocks names ONE concrete action, most specific first: the record's
// own next_action, then its first exposed action label, then the review verb.
func derivedUnlocks(payload map[string]any, area string) []string {
	var out []string
	if next := plainUnlockText(firstString(payload, "next_action")); next != "" {
		out = append(out, next)
	}
	if label := plainUnlockText(firstActionLabel(payload)); label != "" {
		out = append(out, label)
	}
	out = append(out,
		"Open it with compound reviews --area "+FirstNonEmpty(area, "review")+".",
		"Open the review queue and read this record.")
	return out
}

// plainUnlockText scrubs free text into unlock grammar: one line, no question,
// at most 90 runes.
func plainUnlockText(raw string) string {
	raw = SanitizeFragment(raw)
	raw = strings.ReplaceAll(raw, "?", "")
	raw = ShortLabel(raw, 90)
	return strings.TrimSpace(strings.TrimRight(raw, " ,:;-"))
}

func firstActionLabel(payload map[string]any) string {
	actions, ok := payload["actions"].([]any)
	if !ok || len(actions) == 0 {
		return ""
	}
	first, ok := actions[0].(map[string]any)
	if !ok {
		return ""
	}
	return firstString(first, "label")
}

// LegacyGenericCardSummary reports whether summary is the retired generic
// fallback sentence ("…: a new record needs your read.") that pre-derivation
// writers stamped on cardless records. Queue loads re-derive those cards from
// the payload so the operator never sees the placeholder again — including on
// artifacts already written to disk.
func LegacyGenericCardSummary(summary string) bool {
	trimmed := strings.TrimSpace(summary)
	trimmed = strings.TrimSuffix(trimmed, ".")
	trimmed = strings.TrimSuffix(trimmed, "…")
	trimmed = strings.TrimSpace(trimmed)
	return strings.HasSuffix(strings.ToLower(trimmed), "a new record needs your read")
}

func PlainLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	replacer := strings.NewReplacer("_", " ", "-", " ", ".", " ", ":", " ")
	parts := strings.Fields(replacer.Replace(value))
	return strings.Join(parts, " ")
}

func ShortLabel(value string, max int) string {
	value = SanitizeFragment(value)
	if max <= 0 || utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	if max <= 1 {
		return string(runes[:max])
	}
	limit := max - 1 // reserve one rune for the ellipsis
	cut := limit
	for cut > 0 && runes[cut] != ' ' {
		cut--
	}
	if cut == 0 {
		// no whitespace within the limit (one long token), fall back to a
		// hard cut rather than exceed max.
		cut = limit
	}
	out := strings.TrimRight(string(runes[:cut]), " .,:;-")
	if out == "" {
		out = strings.TrimSpace(string(runes[:limit]))
	}
	return out + "…"
}

func FirstClause(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, sep := range []string{". ", "; ", " — ", " - ", "\n"} {
		if idx := strings.Index(value, sep); idx > 0 {
			return strings.TrimSpace(value[:idx])
		}
	}
	return value
}

func HumanizeEntity(value string) string {
	value = strings.TrimSpace(value)
	for _, prefix := range []string{"product:", "capability:", "repo:", "knowledge:arxiv:arxiv:", "arxiv:"} {
		value = strings.TrimPrefix(value, prefix)
	}
	value = strings.TrimSuffix(value, "v1")
	value = strings.Trim(value, ":/")
	if value == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(value), "260") || strings.Count(value, ".") >= 1 && len(value) <= 16 {
		return "arXiv " + value
	}
	return value
}
