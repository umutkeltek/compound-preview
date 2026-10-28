package harvest

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/hook"
	"compound/internal/policy"
)

func TriageTranscript(path string, in AskTriageInput) (AskTriageResult, error) {
	ask, err := hook.LastBlockingAsk(path)
	if err != nil {
		return AskTriageResult{}, err
	}
	in.Ask, in.Context = ask.Text, ask.Context
	return TriageAsk(in), nil
}

type AskDisposition string

const (
	AskAutoAnswered  AskDisposition = "auto_answered"
	AskNeedsOperator AskDisposition = "needs_operator"
	AskContradicted  AskDisposition = "contradicted"
)

type AskLaw struct {
	ClaimID, Statement string
	Terms              []string
	MinTerms           int
}
type AskAutoAnswer struct{ RecordID, Answer string }
type AskBriefOption struct{ Label, Consequence string }
type AskBrief struct {
	Headline, Context, Recommendation, PreparedBy string
	Options                                       []AskBriefOption
}
type AskReceipt struct {
	ID, WorkID, LaneID, Disposition, Answer, CitedRecordID, CreatedAt string
	ProofRefs                                                         []string
}
type AskHarvestCandidate struct {
	Outcome                   Outcome
	Quote, ConflictsWithClaim string
}
type AskTriageInput struct {
	Ask, Context, LaneID, WorkID, OperatorAnswer string
	Now                                          time.Time
	StandingLaw                                  []AskLaw
	Evidence                                     []AskLaw // context only; never authority for resume
	PriorAutoAnswer                              *AskAutoAnswer
	PolicyConfig                                 *policy.Config
}
type AskTriageResult struct {
	Disposition           AskDisposition
	Answer, CitedRecordID string
	RiskReason            string
	ResumeLane            bool
	Receipt               AskReceipt
	Brief                 *AskBrief
	HarvestCandidate      *AskHarvestCandidate
}

type askRiskRule struct {
	terms         []string
	mutationClass string
	reversibility string
	blastRadius   string
}

var askRiskRules = []askRiskRule{
	{terms: []string{"credential", "credentials", "secret", "secrets", "bypass", "approval boundary", "grant access", "revoke access"}, mutationClass: "security"},
	{terms: []string{"spend", "buy", "purchase", "pay", "payment", "charge", "refund"}, mutationClass: "financial"},
	{terms: []string{"delete", "destroy", "drop", "wipe"}, mutationClass: "destructive"},
	{terms: []string{"publish", "publishing", "release publicly"}, mutationClass: "public", blastRadius: "public"},
	{terms: []string{"push", "deploy", "merge", "send", "submit", "upload"}, mutationClass: "external_side_effect"},
	{terms: []string{"irreversible"}, reversibility: "irreversible"},
	{terms: []string{"production"}, blastRadius: "production"},
	{terms: []string{"public"}, blastRadius: "public"},
	{terms: []string{"account"}, blastRadius: "account"},
	{terms: []string{"fleet"}, blastRadius: "fleet"},
	{terms: []string{"organization", "organisation", "org wide", "org-wide"}, blastRadius: "org"},
	{terms: []string{"user data", "user-data"}, blastRadius: "user-data"},
}

type AskRiskAssessment struct {
	Action envelope.ActionIntent
	Reason string
}

// TriageAsk is the additive ask-side of ruling harvest. It accepts an already
// extracted blocking question, so transcript parsing stays owned by hook. Risk
// is checked before recall: matching law can never lower the envelope floor.
func TriageAsk(in AskTriageInput) AskTriageResult {
	if in.PriorAutoAnswer != nil && strings.TrimSpace(in.OperatorAnswer) != "" {
		return AskTriageResult{Disposition: AskContradicted, HarvestCandidate: &AskHarvestCandidate{Outcome: OutcomeConflict, Quote: strings.TrimSpace(in.OperatorAnswer), ConflictsWithClaim: in.PriorAutoAnswer.RecordID}}
	}
	cfg := policy.DefaultConfig()
	if in.PolicyConfig != nil {
		cfg = *in.PolicyConfig
	}
	risk := assessAskRisk(in.Ask, cfg)
	if risk.Reason == "" {
		if law, ok := bestAskLaw(in.Ask, in.StandingLaw); ok {
			now := in.Now
			if now.IsZero() {
				now = time.Now()
			}
			answer := law.Statement + " (" + law.ClaimID + ")"
			sum := sha256.Sum256([]byte(in.LaneID + "\x00" + in.WorkID + "\x00" + in.Ask + "\x00" + law.ClaimID))
			return AskTriageResult{Disposition: AskAutoAnswered, Answer: answer, CitedRecordID: law.ClaimID, ResumeLane: true, Receipt: AskReceipt{ID: fmt.Sprintf("askrec_%x", sum[:6]), WorkID: in.WorkID, LaneID: in.LaneID, Disposition: string(AskAutoAnswered), Answer: answer, CitedRecordID: law.ClaimID, CreatedAt: now.UTC().Format(time.RFC3339), ProofRefs: []string{"claim:" + law.ClaimID}}}
		}
	}
	return AskTriageResult{Disposition: AskNeedsOperator, RiskReason: risk.Reason, Brief: prepareAskBrief(in)}
}

func askExitsEnvelope(ask string) bool {
	return assessAskRisk(ask, policy.DefaultConfig()).Reason != ""
}

func assessAskRisk(ask string, cfg policy.Config) AskRiskAssessment {
	action := envelope.ActionIntent{
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
	}
	for _, clause := range askRiskClauses(ask) {
		for _, rule := range askRiskRules {
			if !clauseHasAffirmedRisk(clause, rule.terms) {
				continue
			}
			if askMutationPriority(rule.mutationClass) > askMutationPriority(action.MutationClass) {
				action.MutationClass = rule.mutationClass
			}
			if rule.reversibility != "" {
				action.Reversibility = rule.reversibility
			}
			if rule.blastRadius != "" {
				action.BlastRadius = rule.blastRadius
			}
		}
	}
	return AskRiskAssessment{Action: action, Reason: policy.AutonomyOperatorOnlyReason(action, cfg)}
}

func askMutationPriority(class string) int {
	return map[string]int{
		"local_reversible":     0,
		"external_side_effect": 1,
		"public":               2,
		"destructive":          3,
		"financial":            4,
		"security":             5,
	}[class]
}

func askRiskClauses(text string) []string {
	normalized := strings.NewReplacer("’", "'", " org-wide ", " org wide ").Replace(strings.ToLower(text))
	normalized = strings.ReplaceAll(normalized, " but ", ";")
	normalized = strings.ReplaceAll(normalized, " however ", ";")
	return strings.FieldsFunc(normalized, func(r rune) bool {
		return r == ';' || r == '.' || r == '?' || r == '!' || r == '\n'
	})
}

func clauseHasAffirmedRisk(clause string, terms []string) bool {
	tokens := askRiskTokens(clause)
	for _, term := range terms {
		termTokens := askRiskTokens(term)
		if len(termTokens) == 0 || len(termTokens) > len(tokens) {
			continue
		}
		for i := 0; i+len(termTokens) <= len(tokens); i++ {
			if !askTokenWindowEqual(tokens[i:i+len(termTokens)], termTokens) || askRiskNegated(tokens, i) {
				continue
			}
			return true
		}
	}
	return false
}

func askRiskTokens(text string) []string {
	text = strings.ReplaceAll(strings.ToLower(text), "don't", "do not")
	return strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
}

func askTokenWindowEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func askRiskNegated(tokens []string, termIndex int) bool {
	start := termIndex - 4
	if start < 0 {
		start = 0
	}
	for _, token := range tokens[start:termIndex] {
		if askRiskNegator(token) {
			return true
		}
	}
	return false
}

func askRiskNegator(token string) bool {
	switch token {
	case "not", "no", "never", "without", "avoid", "avoiding", "exclude", "excluding":
		return true
	default:
		return false
	}
}

func askMatchingTokens(text string) map[string]bool {
	var affirmed []string
	for _, clause := range askRiskClauses(text) {
		tokens := askRiskTokens(clause)
		for i, token := range tokens {
			if askRiskNegator(token) {
				tokens = tokens[:i]
				break
			}
		}
		affirmed = append(affirmed, tokens...)
	}
	return significantTokens(strings.Join(affirmed, " "))
}

func bestAskLaw(text string, laws []AskLaw) (AskLaw, bool) {
	askTokens := askMatchingTokens(text)
	if len(askTokens) == 0 {
		return AskLaw{}, false
	}
	best, score := AskLaw{}, 0
	ambiguous := false
	seenClaims := map[string]bool{}
	for _, law := range laws {
		if law.ClaimID == "" || law.Statement == "" || seenClaims[law.ClaimID] {
			continue
		}
		seenClaims[law.ClaimID] = true
		lawTokens := significantTokens(strings.Join(law.Terms, " "))
		n := intersectionCount(askTokens, lawTokens)
		minimum := law.MinTerms
		if minimum < matchMinAnchors {
			minimum = matchMinAnchors
		}
		coverage := float64(n) / float64(len(askTokens))
		if n < minimum || coverage < matchCoverage {
			continue
		}
		switch {
		case n > score:
			best, score, ambiguous = law, n, false
		case n == score && best.ClaimID != "" && law.ClaimID != best.ClaimID:
			ambiguous = true
		}
	}
	return best, score > 0 && !ambiguous
}

func prepareAskBrief(in AskTriageInput) *AskBrief {
	headline := strings.TrimSpace(in.Ask)
	if headline == "" {
		headline = "A lane needs an operator ruling"
	}
	return &AskBrief{Headline: headline, Context: strings.TrimSpace(in.Context), Options: []AskBriefOption{{Label: "Allow", Consequence: "The lane may continue with this specific request."}, {Label: "Hold", Consequence: "The lane remains paused and the envelope does not expand."}}, Recommendation: "Hold unless the request is clearly inside the intended envelope.", PreparedBy: "compound ask-triage"}
}
