package cli

// scout_draft_test.go: scout (genre anchor search) + draft (LLM-rented idea
// one-pager) tests, ported from the pre-organ cmd/compound/game_idea_test.go
// (same-change law: the surface moved into internal/game/cli with its tests).

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCmdGameScoutJSON(t *testing.T) {
	home := withTempHome(t)
	now := time.Now().UTC()
	seedGameObsCli(t, home, "steamweb", "steamweb", "genre", "action", "Action",
		`Game is in genre "Action" on appid 440`, now.Add(-1*time.Hour))
	seedGameObsCli(t, home, "steamweb", "steamweb", "genre", "strategy", "Strategy",
		`Game is in genre "Strategy" on appid 570`, now.Add(-1*time.Hour))

	code, stdout := runGameCmd(t, func() int {
		return cmdGameScout([]string{"action", "--json"})
	})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("parse: %v\nbody: %s", err, stdout)
	}
	if got["schema"] != "compound/game-scout-packet.v0.1" {
		t.Errorf("schema: %v", got["schema"])
	}
	anchors, _ := got["anchors"].([]any)
	if len(anchors) != 1 {
		t.Errorf("expected 1 anchor matching 'action', got %d", len(anchors))
	}
}

func TestCmdGameScoutRequiresQuery(t *testing.T) {
	withTempHome(t)
	code, _ := runGameCmd(t, func() int {
		return cmdGameScout([]string{"--json"})
	})
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
}

func TestCmdGameScoutEmptyQueryReturnsTrending(t *testing.T) {
	home := withTempHome(t)
	now := time.Now().UTC()
	seedGameObsCli(t, home, "steamweb", "steamweb", "genre", "action", "Action",
		`Game is in genre "Action" on appid 440`, now.Add(-1*time.Hour))
	code, stdout := runGameCmd(t, func() int {
		return cmdGameScout([]string{"", "--json"})
	})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	anchors, _ := got["anchors"].([]any)
	if len(anchors) != 0 {
		t.Errorf("empty query should match no anchors; got %d", len(anchors))
	}
	if warnings, _ := got["warnings"].([]any); len(warnings) == 0 {
		t.Errorf("empty query should emit warning")
	}
}

func TestCmdGameDraftRequiresEnvVars(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_MODEL", "")
	withTempHome(t)
	var code int
	_, stderr := captureStdoutStderr(t, func() {
		code = cmdGameDraft([]string{"co-op roguelike", "--json"})
	})
	if code != 1 {
		t.Errorf("expected exit 1, got %d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "OPENAI_BASE_URL") {
		t.Errorf("expected OPENAI env-var message, got %q", stderr)
	}
}

func TestCmdGameDraftRequiresQuery(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "https://example.invalid")
	t.Setenv("OPENAI_API_KEY", "x")
	t.Setenv("OPENAI_MODEL", "gpt-test")
	withTempHome(t)
	var code int
	_, stderr := captureStdoutStderr(t, func() {
		code = cmdGameDraft([]string{"--json"})
	})
	if code != 2 {
		t.Errorf("expected exit 2, got %d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "query") {
		t.Errorf("expected query hint, got %q", stderr)
	}
}

func TestCmdGameRejectsUnknownFlagScoutDraft(t *testing.T) {
	withTempHome(t)
	cases := [][]string{
		{"scout", "--bad"},
		{"draft", "--bogus"},
	}
	for _, args := range cases {
		var code int
		_, stderr := captureStdoutStderr(t, func() {
			switch args[0] {
			case "scout":
				code = cmdGameScout(args[1:])
			case "draft":
				code = cmdGameDraft(args[1:])
			}
		})
		if code != 2 {
			t.Errorf("args=%v: expected exit 2, got %d stderr=%s", args, code, stderr)
		}
		if !strings.Contains(stderr, "unknown flag") {
			t.Errorf("args=%v: expected unknown flag, got %q", args, stderr)
		}
	}
}

func TestGameFirstPositional(t *testing.T) {
	got, err := gameFirstPositional([]string{"--json", "action", "--limit", "10"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "action" {
		t.Errorf("got %q want action", got)
	}
	if _, err := gameFirstPositional([]string{"--json", "--home"}); err == nil {
		t.Errorf("expected error on no positional")
	}
}
