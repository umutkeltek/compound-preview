package idea

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"compound/internal/game/source/gamesource"
	"compound/internal/sourceobs"
)

var scoutSeedRev int64

func seedScoutObs(t *testing.T, home, adapterID, sourceSys, kind, sourceKey, subject, statement string, when time.Time) {
	t.Helper()
	it := gamesource.RawObservation{
		Kind:       kind,
		SourceKey:  sourceKey,
		Subject:    subject,
		Statement:  statement,
		Confidence: 1.0,
		Evidence:   []string{"https://store.steampowered.com/api/appdetails", "revision:test"},
	}
	rev := "scout-test-" + strconv.FormatInt(atomic.AddInt64(&scoutSeedRev, 1), 10)
	obs, err := gamesource.BuildObservation(adapterID, sourceSys, rev, it, when)
	if err != nil {
		t.Fatalf("seed build: %v", err)
	}
	if _, err := sourceobs.Apply(home, obs, when); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
}

func TestScoutMatchesBySubject(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	seedScoutObs(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"genre", "action", "Action", `Game is in genre "Action" on appid 440`,
		now.Add(-2*time.Hour))
	seedScoutObs(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"genre", "strategy", "Strategy", `Game is in genre "Strategy" on appid 570`,
		now.Add(-1*time.Hour))

	pkt, err := ComposeScoutPacket(context.Background(), home, "action", now, 10)
	if err != nil {
		t.Fatalf("scout: %v", err)
	}
	if pkt.Schema != scoutSchema {
		t.Errorf("schema: %q", pkt.Schema)
	}
	if len(pkt.Anchors) != 1 {
		t.Fatalf("expected 1 anchor matching 'action', got %d: %+v", len(pkt.Anchors), pkt.Anchors)
	}
	if pkt.Anchors[0].Kind != "genre" || pkt.Anchors[0].SourceKey != "action" {
		t.Errorf("anchor mismatch: %+v", pkt.Anchors[0])
	}
	if len(pkt.RecentMomentum) == 0 {
		t.Errorf("expected recent_momentum to be populated from trending")
	}
}

func TestScoutIsDeterministic(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	seedScoutObs(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"genre", "action", "Action", `Game is in genre "Action" on appid 440`,
		now.Add(-1*time.Hour))
	p1, err := ComposeScoutPacket(context.Background(), home, "action", now, 10)
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	p2, err := ComposeScoutPacket(context.Background(), home, "action", now, 10)
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if p1.ID != p2.ID {
		t.Errorf("ID differs across runs: %q vs %q", p1.ID, p2.ID)
	}
	if len(p1.Anchors) != len(p2.Anchors) {
		t.Errorf("anchor count differs: %d vs %d", len(p1.Anchors), len(p2.Anchors))
	}
}

func TestScoutEmptyQueryReturnsTrending(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	seedScoutObs(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"genre", "action", "Action", `Game is in genre "Action" on appid 440`,
		now.Add(-1*time.Hour))
	pkt, err := ComposeScoutPacket(context.Background(), home, "", now, 10)
	if err != nil {
		t.Fatalf("scout: %v", err)
	}
	if len(pkt.Anchors) != 0 {
		t.Errorf("empty query should match no anchors; got %d", len(pkt.Anchors))
	}
	if len(pkt.RecentMomentum) == 0 {
		t.Errorf("empty query should still include recent_momentum")
	}
	if len(pkt.Warnings) == 0 {
		t.Errorf("expected warning about empty query")
	}
}

func TestScoutCapHonored(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	// Seed several distinct (kind, source_key) anchors that all match
	// a query for "game".
	for _, k := range []string{"action", "strategy", "rpg", "racing", "indie"} {
		seedScoutObs(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
			"genre", k, k, `Game is in genre "`+k+`" on appid 1`,
			now.Add(-1*time.Hour))
	}
	pkt, err := ComposeScoutPacket(context.Background(), home, "game", now, 2)
	if err != nil {
		t.Fatalf("scout: %v", err)
	}
	if len(pkt.Anchors) != 2 {
		t.Errorf("expected 2 anchors (cap), got %d", len(pkt.Anchors))
	}
}

func TestParseScoutSubject(t *testing.T) {
	k, key, lab, md, ok := parseScoutSubject("game:genre:action::Action|appid=440;role=developer;source=steamweb")
	if !ok || k != "genre" || key != "action" || lab != "Action" {
		t.Errorf("parse mismatch: %q,%q,%q,%v", k, key, lab, ok)
	}
	if md["appid"] != "440" || md["role"] != "developer" || md["source"] != "steamweb" {
		t.Errorf("metadata mismatch: %+v", md)
	}
}
