package idea

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/game"
)

// IdeaOnePager is the structured output the LLM returns. Strict schema so
// the candidate parked downstream can be promoted into a ProductHypothesis
// via the existing governed review queue without re-parsing.
type IdeaOnePager struct {
	Title        string   `json:"title"`
	Hook         string   `json:"hook"`
	GenreIDs     []string `json:"genre_ids"`
	Audience     string   `json:"audience"`
	Mechanic     string   `json:"mechanic"`
	EvidenceRefs []string `json:"evidence_refs"`
	Confidence   float64  `json:"confidence"`
}

// LLMClient is the interface NewIdeaDraft depends on. Production wires it
// to distiller.ChatCompletionJSON; tests pass a stub returning canned
// one-pagers. Keeping the interface inside this package (not in distiller)
// means distiller never grows a new responsibility for the game surface.
type LLMClient interface {
	Complete(ctx context.Context, system, user string) ([]byte, error)
}

// CandidateWriter is the narrow seam NewIdeaDraft uses to persist the
// parked candidate. Production wires it to candidates.WriteCandidate; tests
// pass a recorder.
type CandidateWriter interface {
	WriteIdeaCandidate(homePath, id string, payload any) error
}

// DefaultClient implements LLMClient via distiller.ChatCompletionJSON.
// Constructed once per CLI invocation; the HTTP client is injectable so
// tests can swap a deterministic stub.
type DefaultClient struct {
	HTTPClient *http.Client
	BaseURL    string
	APIKey     string
	Model      string
	System     string
}

// Complete calls the rented model lane and returns the raw content bytes.
func (c *DefaultClient) Complete(ctx context.Context, system, user string) ([]byte, error) {
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if c.BaseURL == "" || c.APIKey == "" || c.Model == "" {
		return nil, fmt.Errorf("default LLM client missing baseURL/apiKey/model")
	}
	content, err := chatCompletionJSONFn(ctx, c.HTTPClient, c.BaseURL, c.APIKey, c.Model, system, user)
	if err != nil {
		return nil, err
	}
	return content, nil
}

// DefaultCandidateWriter persists via candidates.WriteCandidate.
type DefaultCandidateWriter struct{}

func (DefaultCandidateWriter) WriteIdeaCandidate(homePath, id string, payload any) error {
	return candidates.WriteCandidate(homePath, candidates.ProductHypothesis, id, payload)
}

// DraftResult is what NewIdeaDraft returns: the parked candidate id plus
// the one-pager the LLM produced. The CLI echoes the id so the operator
// can run `compound product hypothesis-plan --candidate <id>` next.
type DraftResult struct {
	Schema      string       `json:"schema"`
	CandidateID string       `json:"candidate_id"`
	Query       string       `json:"query"`
	OnePager    IdeaOnePager `json:"one_pager"`
	ScoutPacket ScoutPacket  `json:"scout_packet"`
	CreatedAt   time.Time    `json:"created_at"`
}

const draftSchema = "compound/game-idea.v0.1"

// NewIdeaDraft runs the full scout→draft→park flow. Fail-closed: if the
// LLM errors, no candidate is written. If parsing the one-pager fails, no
// candidate is written. The verdict is always Park — promotion is the
// operator's next step.
func NewIdeaDraft(
	ctx context.Context,
	homePath, query string,
	now time.Time,
	llm LLMClient,
	writer CandidateWriter,
) (DraftResult, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if strings.TrimSpace(query) == "" {
		return DraftResult{}, fmt.Errorf("idea.NewIdeaDraft: query required")
	}
	if llm == nil {
		return DraftResult{}, fmt.Errorf("idea.NewIdeaDraft: LLMClient required")
	}
	if writer == nil {
		return DraftResult{}, fmt.Errorf("idea.NewIdeaDraft: CandidateWriter required")
	}

	pkt, err := ComposeScoutPacket(ctx, homePath, query, now, 20)
	if err != nil {
		return DraftResult{}, fmt.Errorf("idea.NewIdeaDraft: scout: %w", err)
	}

	system := ideaSystemPrompt()
	user, err := ideaUserPrompt(query, pkt)
	if err != nil {
		return DraftResult{}, fmt.Errorf("idea.NewIdeaDraft: build prompt: %w", err)
	}

	raw, err := llm.Complete(ctx, system, user)
	if err != nil {
		return DraftResult{}, fmt.Errorf("idea.NewIdeaDraft: llm: %w", err)
	}

	onePager, err := parseOnePager(raw)
	if err != nil {
		return DraftResult{}, fmt.Errorf("idea.NewIdeaDraft: parse one-pager: %w", err)
	}

	candidateID := game.GameID("hypothesis", strings.ToLower(strings.TrimSpace(query)), "draft")
	candidatePayload := buildCandidatePayload(query, pkt, onePager, candidateID, now)

	if err := writer.WriteIdeaCandidate(homePath, candidateID, candidatePayload); err != nil {
		return DraftResult{}, fmt.Errorf("idea.NewIdeaDraft: write candidate: %w", err)
	}

	return DraftResult{
		Schema:      draftSchema,
		CandidateID: candidateID,
		Query:       query,
		OnePager:    onePager,
		ScoutPacket: pkt,
		CreatedAt:   now,
	}, nil
}

// buildCandidatePayload composes the parked-candidate record. It includes
// the full ScoutPacket so a future review tool can re-derive the evidence
// without needing to re-call scout.
func buildCandidatePayload(query string, pkt ScoutPacket, op IdeaOnePager, id string, now time.Time) map[string]any {
	return map[string]any{
		"id":              id,
		"query":           query,
		"schema":          draftSchema,
		"verdict":         candidates.Park,
		"promote_allowed": false,
		"created_at":      now.Format(time.RFC3339Nano),
		"one_pager":       op,
		"scout_packet":    pkt,
		"next_action":     "compound product hypothesis-plan --candidate " + id,
	}
}

func ideaSystemPrompt() string {
	return "You are a game concept generator working from observed game-signal evidence.\n" +
		"You NEVER invent genres, companies, or appids that aren't in the evidence packet.\n" +
		"You cite evidence_refs as the observation_ids the scout packet provided.\n" +
		"You respond ONLY with valid JSON matching the schema the user provides."
}

func ideaUserPrompt(query string, pkt ScoutPacket) (string, error) {
	pktJSON, err := json.MarshalIndent(pkt, "", "  ")
	if err != nil {
		return "", err
	}
	return "Operator query: " + query + "\n\n" +
		"Evidence packet (deterministic, observe-only):\n" + string(pktJSON) + "\n\n" +
		"Produce ONE game idea as JSON with fields:\n" +
		"  title       string\n" +
		"  hook        string   // one-sentence elevator\n" +
		"  genre_ids   string[] // genre slugs from the anchors, e.g. [\"action\", \"roguelike\"]\n" +
		"  audience    string   // who plays this\n" +
		"  mechanic    string   // core loop in one sentence\n" +
		"  evidence_refs string[] // observation_ids from the packet's anchors\n" +
		"  confidence  number   // 0..1; lower if evidence is thin\n", nil
}

// parseOnePager tolerates the LLM returning the JSON either naked or wrapped
// in a ```json ...``` fence. It strict-decodes into IdeaOnePager.
func parseOnePager(raw []byte) (IdeaOnePager, error) {
	s := strings.TrimSpace(string(raw))
	// Strip a single leading/trailing ```json fence if present.
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i > 0 {
			s = s[i+1:]
		}
		if j := strings.LastIndex(s, "```"); j > 0 {
			s = s[:j]
		}
		s = strings.TrimSpace(s)
	}
	var op IdeaOnePager
	if err := json.Unmarshal([]byte(s), &op); err != nil {
		return IdeaOnePager{}, err
	}
	if strings.TrimSpace(op.Title) == "" {
		return IdeaOnePager{}, fmt.Errorf("one-pager missing title")
	}
	if strings.TrimSpace(op.Hook) == "" {
		return IdeaOnePager{}, fmt.Errorf("one-pager missing hook")
	}
	return op, nil
}
