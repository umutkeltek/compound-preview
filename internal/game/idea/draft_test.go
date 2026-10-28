package idea

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"compound/internal/game/source/gamesource"
	"compound/internal/sourceobs"
)

// stubLLM returns canned content from a fixed map of system→user keys.
type stubLLM struct {
	mu        sync.Mutex
	responses map[string][]byte
	err       error
	calls     int
}

func (s *stubLLM) Complete(_ context.Context, system, user string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if v, ok := s.responses[system+"\n--\n"+user]; ok {
		return v, nil
	}
	if v, ok := s.responses["default"]; ok {
		return v, nil
	}
	return nil, errors.New("stubLLM: no canned response")
}

// recorderCandidateWriter records every WriteIdeaCandidate call.
type recorderCandidateWriter struct {
	mu      sync.Mutex
	written []string
	payload []any
	err     error
}

func (r *recorderCandidateWriter) WriteIdeaCandidate(homePath, id string, payload any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.written = append(r.written, homePath+"/"+id)
	r.payload = append(r.payload, payload)
	return nil
}

func seedIdeaObs(t *testing.T, home, kind, sourceKey, subject, statement string, when time.Time) {
	t.Helper()
	it := gamesource.RawObservation{
		Kind:       kind,
		SourceKey:  sourceKey,
		Subject:    subject,
		Statement:  statement,
		Confidence: 1.0,
		Evidence:   []string{"https://store.steampowered.com/api/appdetails", "revision:test"},
	}
	rev := "idea-test-rev-" + time.Now().Format(time.RFC3339Nano)
	obs, err := gamesource.BuildObservation(gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb, rev, it, when)
	if err != nil {
		t.Fatalf("seed build: %v", err)
	}
	if _, err := sourceobs.Apply(home, obs, when); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
}

func validOnePagerJSON() []byte {
	op := IdeaOnePager{
		Title:        "Riftwalkers",
		Hook:         "Co-op roguelike where each run you literally rearrange the dungeon before entering it.",
		GenreIDs:     []string{"action", "roguelike"},
		Audience:     "Friends who like Helldivers and Slay the Spire.",
		Mechanic:     "Rotate room tiles between rounds to design encounters; permadeath per run.",
		EvidenceRefs: []string{"srcobs_aaa", "srcobs_bbb"},
		Confidence:   0.7,
	}
	b, _ := json.Marshal(op)
	return b
}

func TestDraftHappyPathParksCandidate(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	seedIdeaObs(t, home, "genre", "action", "Action",
		`Game is in genre "Action" on appid 440`, now.Add(-1*time.Hour))

	llm := &stubLLM{responses: map[string][]byte{"default": validOnePagerJSON()}}
	wr := &recorderCandidateWriter{}

	res, err := NewIdeaDraft(context.Background(), home, "co-op roguelike", now, llm, wr)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if res.Schema != draftSchema {
		t.Errorf("schema: %q", res.Schema)
	}
	if res.CandidateID == "" {
		t.Errorf("candidate_id empty")
	}
	if res.OnePager.Title != "Riftwalkers" {
		t.Errorf("one-pager title: %q", res.OnePager.Title)
	}
	if len(wr.written) != 1 {
		t.Errorf("expected 1 candidate written, got %d", len(wr.written))
	}
	if wr.written[0] != home+"/"+res.CandidateID {
		t.Errorf("written path mismatch: %s vs %s", wr.written[0], home+"/"+res.CandidateID)
	}
	// Verify payload advertises park + no promote.
	payload, ok := wr.payload[0].(map[string]any)
	if !ok {
		t.Fatalf("payload not a map: %T", wr.payload[0])
	}
	if payload["verdict"] != "park" {
		t.Errorf("verdict: %v", payload["verdict"])
	}
	if v, _ := payload["promote_allowed"].(bool); v {
		t.Errorf("promote_allowed should be false")
	}
}

func TestDraftLLMFailureDoesNotWrite(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	seedIdeaObs(t, home, "genre", "action", "Action",
		`Game is in genre "Action" on appid 440`, now.Add(-1*time.Hour))

	llm := &stubLLM{err: errors.New("rate limited")}
	wr := &recorderCandidateWriter{}

	_, err := NewIdeaDraft(context.Background(), home, "co-op roguelike", now, llm, wr)
	if err == nil {
		t.Fatalf("expected error from LLM failure")
	}
	if !strings.Contains(err.Error(), "llm") {
		t.Errorf("expected llm error, got: %v", err)
	}
	if len(wr.written) != 0 {
		t.Errorf("LLM failure must NOT write a candidate; got %d", len(wr.written))
	}
}

func TestDraftParseFailureDoesNotWrite(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	seedIdeaObs(t, home, "genre", "action", "Action",
		`Game is in genre "Action" on appid 440`, now.Add(-1*time.Hour))

	llm := &stubLLM{responses: map[string][]byte{"default": []byte("not json")}}
	wr := &recorderCandidateWriter{}

	_, err := NewIdeaDraft(context.Background(), home, "co-op roguelike", now, llm, wr)
	if err == nil {
		t.Fatalf("expected parse error")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("expected parse error, got: %v", err)
	}
	if len(wr.written) != 0 {
		t.Errorf("parse failure must NOT write a candidate; got %d", len(wr.written))
	}
}

func TestDraftStripsMarkdownFence(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	seedIdeaObs(t, home, "genre", "action", "Action",
		`Game is in genre "Action" on appid 440`, now.Add(-1*time.Hour))

	body := validOnePagerJSON()
	fenced := append([]byte("```json\n"), body...)
	fenced = append(fenced, []byte("\n```")...)
	llm := &stubLLM{responses: map[string][]byte{"default": fenced}}
	wr := &recorderCandidateWriter{}

	res, err := NewIdeaDraft(context.Background(), home, "co-op roguelike", now, llm, wr)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if res.OnePager.Title != "Riftwalkers" {
		t.Errorf("expected fenced JSON to parse; title=%q", res.OnePager.Title)
	}
}

func TestDraftRequiresQuery(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	llm := &stubLLM{}
	wr := &recorderCandidateWriter{}

	_, err := NewIdeaDraft(context.Background(), home, "  ", now, llm, wr)
	if err == nil {
		t.Fatalf("expected error on empty query")
	}
}

func TestDraftRequiresLLMAndWriter(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	_, err := NewIdeaDraft(context.Background(), home, "x", now, nil, &recorderCandidateWriter{})
	if err == nil {
		t.Errorf("expected error on nil LLM")
	}
	_, err = NewIdeaDraft(context.Background(), home, "x", now, &stubLLM{}, nil)
	if err == nil {
		t.Errorf("expected error on nil writer")
	}
}
