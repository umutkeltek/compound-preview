package routeevidence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"compound/internal/work"
)

// Falsifiers for the coordination capacity observation. The observation must
// collapse the repeated per-actor source reads into one read per request
// WITHOUT changing a single decision, and it must never turn an unreadable
// source into a clearance.

var coordinationCapacityNow = time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)

// coordinationCapacityFixture points every capacity source at one temp dir and
// returns the config plus a home path the observation can be asked about.
func coordinationCapacityFixture(t *testing.T) (LaneCapacityConfig, string) {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := LaneCapacityConfig{
		Paths: LaneCapacityPaths{
			UsageHistoryPath:      filepath.Join(dir, "usage-history.jsonl"),
			SupplementUsagePath:   filepath.Join(dir, "usage-supplement.jsonl"),
			QuotaCachePath:        filepath.Join(dir, "quota-cache.json"),
			AccountProjectionPath: filepath.Join(dir, "account-projection.json"),
			AliasesPath:           filepath.Join(dir, "lane-aliases.json"),
		},
		ExpectedMinutes: 60,
	}
	writeCapacityRoutingDefaults(t, home, "build-implement", []string{"fixture-claude", "fixture-codex"})
	return cfg, home
}

// writeCapacityRoutingDefaults writes the model-capabilities manifest whose routing defaults pin
// the alternative ladder for one capability class. The manifest is validated on load, so its
// models, tiers and routing chain have to be real.
func writeCapacityRoutingDefaults(t *testing.T, home, class string, models []string) {
	t.Helper()
	path := ModelCapabilitiesPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "version: 1\npurpose: fixture\ncapability_classes: [" + class + "]\nmodels:\n"
	for _, model := range models {
		body += "  " + model + ":\n    lane: fixture " + model + "\n    tier: cheap\n"
	}
	body += "routing_defaults:\n  " + class + ": [" + strings.Join(models, ", ") + "]\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func coordinationCapacityRequest(home, lane string, now time.Time) work.LaneCapacityGuardRequest {
	return work.LaneCapacityGuardRequest{
		HomePath: home,
		Lane:     lane,
		Now:      now,
		ReadOnly: true,
	}
}

// writeCapacitySupplementRow writes a usage row into Compound's own supplement feed.
func writeCapacitySupplementRow(t *testing.T, cfg LaneCapacityConfig, provider string, used float64, resetsAt, sampledAt time.Time) {
	t.Helper()
	body := fmt.Sprintf(
		`{"provider":%q,"windowKind":"5h","windowMinutes":300,"usedPercent":%g,"resetsAt":%q,"sampledAt":%q}`+"\n",
		provider, used, resetsAt.UTC().Format(time.RFC3339), sampledAt.UTC().Format(time.RFC3339),
	)
	file, err := os.OpenFile(cfg.Paths.SupplementUsagePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(body); err != nil {
		t.Fatal(err)
	}
}

// coordinationCapacitySnapshot hashes every file under a directory so a test can prove the
// observation changed nothing.
func coordinationCapacitySnapshot(t *testing.T, root string) string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(data)
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		entries = append(entries, hex.EncodeToString(sum[:])+"  "+filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(entries)
	return strings.Join(entries, "\n")
}

// F1: N eligible actors read each source ONCE, and their decisions stay
// distinct. The legacy path re-read the alias file, both usage feeds, the
// account projection and the quota cache for every single actor.
func TestCoordinationCapacityObservationReadsEachSourceOncePerRequest(t *testing.T) {
	cfg, home := coordinationCapacityFixture(t)
	now := coordinationCapacityNow
	writeCapacityUsage(t, cfg, "claude", 20, now.Add(2*time.Hour), now.Add(-time.Minute))
	appendCapacityUsageRow(t, cfg, "codex", 100, now.Add(2*time.Hour), now.Add(-time.Minute))
	writeQuotaCache(t, cfg, 429, now.Add(-time.Minute))

	observation := NewCoordinationCapacityObservation(cfg, home, now)
	guard := observation.Guard()

	claude, err := guard(coordinationCapacityRequest(home, "claude", now))
	if err != nil {
		t.Fatalf("claude: %v", err)
	}
	codex, err := guard(coordinationCapacityRequest(home, "codex", now))
	if err != nil {
		t.Fatalf("codex: %v", err)
	}
	zai, err := guard(coordinationCapacityRequest(home, "zai", now))
	if err != nil {
		t.Fatalf("zai: %v", err)
	}

	// Actor-specific decisions survive the shared read.
	if claude.Capacity.State != "alive" || claude.Refused || claude.ConfirmationRequired {
		t.Fatalf("claude decision = %+v, want alive", claude)
	}
	if !codex.Refused || codex.Capacity.State != "walled" {
		t.Fatalf("codex decision = %+v, want refused/walled", codex)
	}
	if !zai.ConfirmationRequired || zai.Refused {
		t.Fatalf("zai decision = %+v, want confirmation-required", zai)
	}
	if claude.Message != "" {
		t.Fatalf("alive lane carries a decision message: %q", claude.Message)
	}
	if !strings.Contains(codex.Message, "lane codex") {
		t.Fatalf("codex message is not actor-specific: %q", codex.Message)
	}
	if !strings.Contains(zai.Message, "lane zai") {
		t.Fatalf("zai message is not actor-specific: %q", zai.Message)
	}

	access := observation.SourceAccess()
	for _, path := range []string{
		cfg.Paths.UsageHistoryPath,
		cfg.Paths.SupplementUsagePath,
		cfg.Paths.QuotaCachePath,
		cfg.Paths.AccountProjectionPath,
		cfg.Paths.AliasesPath,
	} {
		if got := access.Reads[path]; got != 1 {
			t.Fatalf("source %s read %d times in one request, want exactly 1", filepath.Base(path), got)
		}
	}
}

// F1b: an actor with no capacity family reads NO capacity source content. The
// existence probes still happen (they are the "is anything wired" question);
// the usage feeds, the quota cache and the account projection are never
// touched. The alias file IS read — resolving a lane to a family needs it.
func TestCoordinationCapacityObservationSkipsIneligibleActors(t *testing.T) {
	cfg, home := coordinationCapacityFixture(t)
	observation := NewCoordinationCapacityObservation(cfg, home, coordinationCapacityNow)

	result, err := observation.Guard()(coordinationCapacityRequest(home, "mystery-lane", coordinationCapacityNow))
	if err != nil {
		t.Fatalf("ineligible actor: %v", err)
	}
	if result.Refused || result.ConfirmationRequired {
		t.Fatalf("unwired lane decision = %+v, want an ungated unknown", result)
	}
	if !strings.Contains(result.Message, "no capacity source installed") {
		t.Fatalf("unwired lane message = %q", result.Message)
	}
	access := observation.SourceAccess()
	for _, path := range []string{
		cfg.Paths.UsageHistoryPath,
		cfg.Paths.SupplementUsagePath,
		cfg.Paths.QuotaCachePath,
		cfg.Paths.AccountProjectionPath,
	} {
		if got := access.Reads[path]; got != 0 {
			t.Fatalf("ineligible actor read %s %d times, want 0", filepath.Base(path), got)
		}
	}
}

// F2: the NEXT request sees the changed source, and a request that failed on
// a broken source does not poison the one after it. Nothing is shared
// globally between requests.
func TestCoordinationCapacityObservationNextRequestSeesChangedSource(t *testing.T) {
	cfg, home := coordinationCapacityFixture(t)
	now := coordinationCapacityNow
	writeCapacityUsage(t, cfg, "claude", 20, now.Add(2*time.Hour), now.Add(-time.Minute))

	previousGlobal := work.LaneCapacityGuard
	t.Cleanup(func() { work.LaneCapacityGuard = previousGlobal })
	globalCalls := 0
	work.LaneCapacityGuard = func(work.LaneCapacityGuardRequest) (work.LaneCapacityGuardResult, error) {
		globalCalls++
		return work.LaneCapacityGuardResult{}, nil
	}
	first := NewCoordinationCapacityObservation(cfg, home, now)
	result, err := first.Guard()(coordinationCapacityRequest(home, "claude", now))
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	if result.Capacity.State != "alive" {
		t.Fatalf("first request decision = %+v, want alive", result)
	}

	// The source changes underneath: the next request must see the new bytes.
	writeCapacityUsage(t, cfg, "claude", 100, now.Add(2*time.Hour), now.Add(-time.Minute))
	second := NewCoordinationCapacityObservation(cfg, home, now)
	changed, err := second.Guard()(coordinationCapacityRequest(home, "claude", now))
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	if !changed.Refused {
		t.Fatalf("second request served stale capacity: %+v", changed)
	}

	// A hard source error in one request does not leak into the next one.
	broken := NewCoordinationCapacityObservation(cfg, home, now)
	if err := os.WriteFile(cfg.Paths.AccountProjectionPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := broken.Guard()(coordinationCapacityRequest(home, "claude", now)); err == nil {
		t.Fatal("a malformed account projection must be a hard error")
	}
	if err := os.WriteFile(cfg.Paths.AccountProjectionPath, []byte(`{"schema_version":"compound.account_projection.v0.1","imported_at":"2026-07-13T09:00:00Z","providers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	recovered := NewCoordinationCapacityObservation(cfg, home, now)
	if _, err := recovered.Guard()(coordinationCapacityRequest(home, "claude", now)); err != nil {
		t.Fatalf("request after a broken source must recover: %v", err)
	}
	if globalCalls != 0 {
		t.Fatalf("the observation consulted the package-level lane capacity guard %d times", globalCalls)
	}
}

// F2b: one observation serves one home at one request instant. A second home
// or a second instant is REFUSED, never served from the first request's
// facts. A new request is a new observation, which re-reads the source.
func TestCoordinationCapacityObservationBindsOneHomeAndOneInstant(t *testing.T) {
	cfg, home := coordinationCapacityFixture(t)
	now := coordinationCapacityNow
	writeCapacityUsage(t, cfg, "claude", 20, now.Add(2*time.Hour), now.Add(-time.Minute))

	observation := NewCoordinationCapacityObservation(cfg, home, now)
	guard := observation.Guard()
	alive, err := guard(coordinationCapacityRequest(home, "claude", now))
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	if alive.Capacity.State != "alive" {
		t.Fatalf("first request decision = %+v, want alive", alive)
	}

	otherHome := t.TempDir()
	if _, err := guard(coordinationCapacityRequest(otherHome, "claude", now)); err == nil {
		t.Fatal("an observation bound to one home must refuse a second home")
	}
	if _, err := guard(coordinationCapacityRequest(home, "claude", now.Add(time.Minute))); err == nil {
		t.Fatal("an observation bound to one request instant must refuse a second instant")
	}

	// The refusals cost nothing: every source was still read exactly once. A claude
	// lane never reads the codex-only quota cache.
	access := observation.SourceAccess()
	for _, path := range []string{
		cfg.Paths.UsageHistoryPath,
		cfg.Paths.SupplementUsagePath,
		cfg.Paths.AccountProjectionPath,
		cfg.Paths.AliasesPath,
	} {
		if got := access.Reads[path]; got != 1 {
			t.Fatalf("source %s read %d times in one request, want exactly 1", filepath.Base(path), got)
		}
	}

	// A NEW request is a new observation: it re-reads and sees the current bytes.
	writeCapacityUsage(t, cfg, "claude", 100, now.Add(2*time.Hour), now.Add(-time.Minute))
	fresh := NewCoordinationCapacityObservation(cfg, home, now)
	changed, err := fresh.Guard()(coordinationCapacityRequest(home, "claude", now))
	if err != nil {
		t.Fatalf("fresh request: %v", err)
	}
	if !changed.Refused {
		t.Fatalf("a fresh observation served stale capacity: %+v", changed)
	}
}

// F3: alias precedence, provider families and decision messages stay
// actor-specific. A lane never borrows another provider's evidence.
func TestCoordinationCapacityObservationKeepsAliasesAndMessagesActorSpecific(t *testing.T) {
	cfg, home := coordinationCapacityFixture(t)
	now := coordinationCapacityNow
	writeLaneAliasesFile(t, cfg, map[string]string{"my-claude": "claude", "my-codex": "codex"})
	writeCapacityUsage(t, cfg, "claude", 20, now.Add(2*time.Hour), now.Add(-time.Minute))
	appendCapacityUsageRow(t, cfg, "codex", 100, now.Add(2*time.Hour), now.Add(-time.Minute))
	appendCapacityUsageRow(t, cfg, "qwen", 5, now.Add(2*time.Hour), now.Add(-time.Minute))

	guard := NewCoordinationCapacityObservation(cfg, home, now).Guard()
	aliasedClaude, err := guard(coordinationCapacityRequest(home, "my-claude", now))
	if err != nil {
		t.Fatalf("aliased claude: %v", err)
	}
	aliasedCodex, err := guard(coordinationCapacityRequest(home, "my-codex", now))
	if err != nil {
		t.Fatalf("aliased codex: %v", err)
	}
	directClaude, err := guard(coordinationCapacityRequest(home, "claude", now))
	if err != nil {
		t.Fatalf("direct claude: %v", err)
	}

	if aliasedClaude.Capacity.State != "alive" {
		t.Fatalf("aliased claude decision = %+v, want alive (alias precedence lost)", aliasedClaude)
	}
	if !strings.Contains(aliasedClaude.Capacity.DataRef, "provider=claude") {
		t.Fatalf("aliased claude data_ref = %q, want the claude family", aliasedClaude.Capacity.DataRef)
	}
	if strings.Contains(aliasedClaude.Capacity.DataRef, "provider=codex") ||
		strings.Contains(aliasedClaude.Capacity.DataRef, "provider=qwen") {
		t.Fatalf("aliased claude borrowed another provider's row: %q", aliasedClaude.Capacity.DataRef)
	}
	if !aliasedCodex.Refused {
		t.Fatalf("aliased codex decision = %+v, want refused", aliasedCodex)
	}
	if !strings.Contains(aliasedCodex.Message, "lane my-codex") {
		t.Fatalf("aliased codex message is not actor-specific: %q", aliasedCodex.Message)
	}
	if strings.Contains(aliasedCodex.Message, "provider=claude") {
		t.Fatalf("aliased codex borrowed the claude row: %q", aliasedCodex.Message)
	}
	if directClaude.Capacity.State != "alive" || directClaude.Refused {
		t.Fatalf("direct claude decision = %+v, want alive", directClaude)
	}
	// The qwen row must not make any of these lanes alive on its own.
	qwen, err := guard(coordinationCapacityRequest(home, "qwen", now))
	if err != nil {
		t.Fatalf("qwen: %v", err)
	}
	if !strings.Contains(qwen.Capacity.DataRef, "provider=qwen") {
		t.Fatalf("qwen data_ref = %q, want the qwen family", qwen.Capacity.DataRef)
	}
}

// F3b: aliases AND expected durations stay per actor. Two lanes resolve to
// two families through the operator alias file, and one family answers
// differently for two expected durations.
func TestCoordinationCapacityObservationKeepsDurationsPerActor(t *testing.T) {
	cfg, home := coordinationCapacityFixture(t)
	now := coordinationCapacityNow
	writeLaneAliasesFile(t, cfg, map[string]string{"my-claude": "claude", "my-codex": "codex"})
	// The claude window resets in 30 minutes: alive for a 10-minute request,
	// expiring (and therefore confirmation-gated) for a 45-minute one.
	appendCapacityUsageRow(t, cfg, "claude", 20, now.Add(30*time.Minute), now.Add(-time.Minute))
	appendCapacityUsageRow(t, cfg, "codex", 100, now.Add(2*time.Hour), now.Add(-time.Minute))

	guard := NewCoordinationCapacityObservation(cfg, home, now).Guard()

	short := coordinationCapacityRequest(home, "my-claude", now)
	short.ExpectedMinutes = 10
	shortResult, err := guard(short)
	if err != nil {
		t.Fatalf("short request: %v", err)
	}
	if shortResult.Capacity.State != "alive" || shortResult.ConfirmationRequired {
		t.Fatalf("short request decision = %+v, want alive", shortResult)
	}
	long := coordinationCapacityRequest(home, "my-claude", now)
	long.ExpectedMinutes = 45
	longResult, err := guard(long)
	if err != nil {
		t.Fatalf("long request: %v", err)
	}
	if longResult.Capacity.State != "expiring" || !longResult.ConfirmationRequired {
		t.Fatalf("long request decision = %+v, want expiring and confirmation-required", longResult)
	}
	codex, err := guard(coordinationCapacityRequest(home, "my-codex", now))
	if err != nil {
		t.Fatalf("aliased codex: %v", err)
	}
	if !codex.Refused || codex.Capacity.State != "walled" {
		t.Fatalf("aliased codex decision = %+v, want refused/walled", codex)
	}
	if strings.Contains(codex.Capacity.DataRef, "provider=claude") {
		t.Fatalf("the codex alias borrowed the claude row: %q", codex.Capacity.DataRef)
	}
}

// F4: parity. The observation must answer exactly what the production guard
// answers, for every capacity state the production path knows.
func TestCoordinationCapacityObservationMatchesProductionGuard(t *testing.T) {
	now := coordinationCapacityNow
	for _, tc := range []struct {
		name            string
		lane            string
		capabilityClass string
		setup           func(t *testing.T, cfg LaneCapacityConfig)
	}{
		{
			name: "fresh alive row",
			lane: "claude",
			setup: func(t *testing.T, cfg LaneCapacityConfig) {
				writeCapacityUsage(t, cfg, "claude", 20, now.Add(2*time.Hour), now.Add(-time.Minute))
			},
		},
		{
			name: "stale row annotates unknown",
			lane: "claude",
			setup: func(t *testing.T, cfg LaneCapacityConfig) {
				writeCapacityUsage(t, cfg, "claude", 20, now.Add(2*time.Hour), now.Add(-2*time.Hour))
			},
		},
		{
			name: "future row is ignored",
			lane: "claude",
			setup: func(t *testing.T, cfg LaneCapacityConfig) {
				writeCapacityUsage(t, cfg, "claude", 20, now.Add(2*time.Hour), now.Add(10*time.Minute))
			},
		},
		{
			name: "expired window stays unknown",
			lane: "claude",
			setup: func(t *testing.T, cfg LaneCapacityConfig) {
				writeCapacityUsage(t, cfg, "claude", 20, now.Add(-time.Hour), now.Add(-2*time.Minute))
			},
		},
		{
			name: "usage row walls the lane",
			lane: "codex",
			setup: func(t *testing.T, cfg LaneCapacityConfig) {
				writeCapacityUsage(t, cfg, "codex", 100, now.Add(2*time.Hour), now.Add(-time.Minute))
			},
		},
		{
			name: "quota cache walls the lane",
			lane: "codex",
			setup: func(t *testing.T, cfg LaneCapacityConfig) {
				writeQuotaCache(t, cfg, 429, now.Add(-time.Minute))
			},
		},
		{
			name: "stale quota cache does not wall the lane",
			lane: "codex",
			setup: func(t *testing.T, cfg LaneCapacityConfig) {
				writeQuotaCache(t, cfg, 429, now.Add(-7*time.Hour))
			},
		},
		{
			name: "dead account projection",
			lane: "claude",
			setup: func(t *testing.T, cfg LaneCapacityConfig) {
				writeDeadAccountProjection(t, cfg, "claude", now.Add(-time.Minute))
			},
		},
		{
			name: "alias resolves the family",
			lane: "my-claude",
			setup: func(t *testing.T, cfg LaneCapacityConfig) {
				writeLaneAliasesFile(t, cfg, map[string]string{"my-claude": "claude"})
				writeCapacityUsage(t, cfg, "claude", 20, now.Add(2*time.Hour), now.Add(-time.Minute))
			},
		},
		{
			name:  "no source installed fails open",
			lane:  "claude",
			setup: func(t *testing.T, cfg LaneCapacityConfig) {},
		},
		{
			name: "malformed usage row is skipped",
			lane: "claude",
			setup: func(t *testing.T, cfg LaneCapacityConfig) {
				if err := os.WriteFile(cfg.Paths.UsageHistoryPath, []byte("{not json\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				appendCapacityUsageRow(t, cfg, "claude", 20, now.Add(2*time.Hour), now.Add(-time.Minute))
			},
		},
		{
			name: "unmatched family is unknown",
			lane: "zai",
			setup: func(t *testing.T, cfg LaneCapacityConfig) {
				writeCapacityUsage(t, cfg, "claude", 20, now.Add(2*time.Hour), now.Add(-time.Minute))
			},
		},
		{
			name: "supplement feed constrains the lane",
			lane: "claude",
			setup: func(t *testing.T, cfg LaneCapacityConfig) {
				writeCapacitySupplementRow(t, cfg, "claude", 100, now.Add(2*time.Hour), now.Add(-time.Minute))
			},
		},
		{
			name:            "capability class carries the routing ladder",
			lane:            "claude",
			capabilityClass: "build-implement",
			setup: func(t *testing.T, cfg LaneCapacityConfig) {
				// No capacity source: the decision is the unwired fail-open, and its
				// message carries the ladder the manifest pinned for this class.
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, home := coordinationCapacityFixture(t)
			tc.setup(t, cfg)
			request := coordinationCapacityRequest(home, tc.lane, now)
			request.CapabilityClass = tc.capabilityClass
			production, err := WorkLaneCapacityGuard(cfg)(request)
			if err != nil {
				t.Fatalf("production guard: %v", err)
			}
			observation := NewCoordinationCapacityObservation(cfg, home, now)
			observed, err := observation.Guard()(request)
			if err != nil {
				t.Fatalf("observation: %v", err)
			}
			if !reflect.DeepEqual(production, observed) {
				t.Fatalf("observation diverged from the production guard\nproduction  = %+v\nobservation = %+v", production, observed)
			}
			if tc.capabilityClass != "" {
				if !strings.Contains(observed.Message, "alternative_ladder=[fixture-claude -> fixture-codex]") {
					t.Fatalf("the routing ladder did not reach the decision: %q", observed.Message)
				}
			}
			// The work result drops LaneCapacity.Source and LaneCapacity.SourceInstalled,
			// and SourceInstalled is what decides "unwired, fail open" versus "wired but
			// blind, confirmation required". Compare the FULL capacity too, so that
			// branch cannot drift unnoticed.
			productionCapacity, err := ReadLaneCapacityWithConfig(tc.lane, now, cfg)
			if err != nil {
				t.Fatalf("production capacity: %v", err)
			}
			observedCapacity, err := readLaneCapacityWithFacts(tc.lane, now, capacityExpectedMinutes(request.ExpectedMinutes), observation.facts.compositionFacts())
			if err != nil {
				t.Fatalf("observed capacity: %v", err)
			}
			if !reflect.DeepEqual(productionCapacity, observedCapacity) {
				t.Fatalf("observation capacity diverged from the production read\nproduction = %+v\nobservation = %+v", productionCapacity, observedCapacity)
			}
		})
	}
}

// F5: absent, unreadable, malformed and scanner-failed sources stay
// distinguishable. A hard error must never read as "eligible".
func TestCoordinationCapacityObservationDistinguishesAbsentUnreadableAndMalformed(t *testing.T) {
	now := coordinationCapacityNow

	t.Run("absent source is not an error", func(t *testing.T) {
		cfg, home := coordinationCapacityFixture(t)
		result, err := NewCoordinationCapacityObservation(cfg, home, now).Guard()(coordinationCapacityRequest(home, "claude", now))
		if err != nil {
			t.Fatalf("absent source must not be a hard error: %v", err)
		}
		if result.Refused || result.ConfirmationRequired {
			t.Fatalf("absent source decision = %+v, want the unwired fail-open", result)
		}
	})

	t.Run("unreadable source is a hard error", func(t *testing.T) {
		cfg, home := coordinationCapacityFixture(t)
		writeCapacityUsage(t, cfg, "claude", 20, now.Add(2*time.Hour), now.Add(-time.Minute))
		if err := os.Chmod(cfg.Paths.UsageHistoryPath, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(cfg.Paths.UsageHistoryPath, 0o644) })
		if _, err := os.ReadFile(cfg.Paths.UsageHistoryPath); err == nil {
			t.Skip("this user bypasses file permissions; the unreadable-source case cannot be exercised")
		}
		_, err := NewCoordinationCapacityObservation(cfg, home, now).Guard()(coordinationCapacityRequest(home, "claude", now))
		if err == nil {
			t.Fatal("an unreadable capacity source must be a hard error, never an ungated lane")
		}
		if !strings.Contains(err.Error(), "read capacity usage history") {
			t.Fatalf("unreadable source error = %v", err)
		}
	})

	t.Run("malformed row is skipped", func(t *testing.T) {
		cfg, home := coordinationCapacityFixture(t)
		if err := os.WriteFile(cfg.Paths.UsageHistoryPath, []byte("{not json\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		appendCapacityUsageRow(t, cfg, "claude", 20, now.Add(2*time.Hour), now.Add(-time.Minute))
		result, err := NewCoordinationCapacityObservation(cfg, home, now).Guard()(coordinationCapacityRequest(home, "claude", now))
		if err != nil {
			t.Fatalf("a malformed row must be skipped, not fatal: %v", err)
		}
		if result.Capacity.State != "alive" {
			t.Fatalf("malformed-row decision = %+v, want the surviving row to decide", result)
		}
	})

	t.Run("malformed projection is a hard error", func(t *testing.T) {
		cfg, home := coordinationCapacityFixture(t)
		if err := os.WriteFile(cfg.Paths.AccountProjectionPath, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := NewCoordinationCapacityObservation(cfg, home, now).Guard()(coordinationCapacityRequest(home, "claude", now))
		if err == nil {
			t.Fatal("a malformed account projection must be a hard error")
		}
		if !strings.Contains(err.Error(), "parse account projection") {
			t.Fatalf("malformed projection error = %v", err)
		}
	})

	t.Run("scanner failure is a hard error", func(t *testing.T) {
		cfg, home := coordinationCapacityFixture(t)
		overlong := `{"provider":"claude","usedPercent":1,"sampledAt":"2026-07-13T09:00:00Z","pad":"` +
			strings.Repeat("x", 5*1024*1024) + `"}` + "\n"
		if err := os.WriteFile(cfg.Paths.UsageHistoryPath, []byte(overlong), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := NewCoordinationCapacityObservation(cfg, home, now).Guard()(coordinationCapacityRequest(home, "claude", now))
		if err == nil {
			t.Fatal("an unreadable usage feed must be a hard error, never an ungated lane")
		}
		if !strings.Contains(err.Error(), "scan capacity usage history") {
			t.Fatalf("scanner failure error = %v", err)
		}
	})
}

// F6: ReadOnly suppresses evidence, NOT the re-read. A lane that was alive at
// admission and walls before the real prelaunch check must still be refused by
// the fresh guard.
func TestCoordinationCapacityFreshGuardRefusesWallDespiteReadOnly(t *testing.T) {
	cfg, home := coordinationCapacityFixture(t)
	now := coordinationCapacityNow
	writeCapacityUsage(t, cfg, "codex", 20, now.Add(2*time.Hour), now.Add(-time.Minute))
	guard := WorkLaneCapacityGuard(cfg)

	alive, err := guard(coordinationCapacityRequest(home, "codex", now))
	if err != nil {
		t.Fatalf("first probe: %v", err)
	}
	if alive.Refused {
		t.Fatalf("alive lane refused: %+v", alive)
	}

	writeCapacityUsage(t, cfg, "codex", 100, now.Add(2*time.Hour), now.Add(-time.Minute))
	walled, err := guard(coordinationCapacityRequest(home, "codex", now))
	if err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if !walled.Refused {
		t.Fatalf("a lane that walled after the first probe was not refused: %+v", walled)
	}
}

// F6b: the observation never leaks into the live guard. The production guard
// re-reads the source on every call, so a lane that was alive at the
// observation and walls afterwards is still refused by the live guard.
func TestCoordinationCapacityObservationDoesNotChangeTheLiveGuard(t *testing.T) {
	cfg, home := coordinationCapacityFixture(t)
	now := coordinationCapacityNow
	writeCapacityUsage(t, cfg, "codex", 20, now.Add(2*time.Hour), now.Add(-time.Minute))

	observation := NewCoordinationCapacityObservation(cfg, home, now)
	observed, err := observation.Guard()(coordinationCapacityRequest(home, "codex", now))
	if err != nil {
		t.Fatalf("observation: %v", err)
	}
	if observed.Refused {
		t.Fatalf("alive lane refused: %+v", observed)
	}

	live := WorkLaneCapacityGuard(cfg)
	alive, err := live(coordinationCapacityRequest(home, "codex", now))
	if err != nil {
		t.Fatalf("live guard: %v", err)
	}
	if alive.Refused {
		t.Fatalf("the live guard refused an alive lane: %+v", alive)
	}

	writeCapacityUsage(t, cfg, "codex", 100, now.Add(2*time.Hour), now.Add(-time.Minute))
	walled, err := live(coordinationCapacityRequest(home, "codex", now))
	if err != nil {
		t.Fatalf("live guard after the source changed: %v", err)
	}
	if !walled.Refused || walled.Capacity.State != "walled" {
		t.Fatalf("the live guard did not see the wall: %+v", walled)
	}
}

// F7: the observation is read-only (no refusal evidence in the home), runs no
// live probe, and never replaces the package-level guard.
func TestCoordinationCapacityObservationLeavesHomeUnchangedAndProbesNothing(t *testing.T) {
	cfg, home := coordinationCapacityFixture(t)
	now := coordinationCapacityNow
	writeCapacityUsage(t, cfg, "codex", 100, now.Add(2*time.Hour), now.Add(-time.Minute))
	before := coordinationCapacitySnapshot(t, home)

	observation := NewCoordinationCapacityObservation(cfg, home, now)
	probes := 0
	observation.probe = func(string, time.Time, time.Duration) (CodexBarCapacityEvidence, error) {
		probes++
		return CodexBarCapacityEvidence{}, errors.New("an ordinary coordination request must not run a live capacity probe")
	}
	result, err := observation.Guard()(coordinationCapacityRequest(home, "codex", now))
	if err != nil {
		t.Fatalf("observation: %v", err)
	}
	if !result.Refused {
		t.Fatalf("walled lane decision = %+v, want refused", result)
	}
	if probes != 0 {
		t.Fatalf("the observation ran %d live capacity probes", probes)
	}
	after := coordinationCapacitySnapshot(t, home)
	if before != after {
		t.Fatalf("the observation mutated the home:\nBEFORE\n%s\nAFTER\n%s", before, after)
	}
}

// F7b: a declared-source request is the one place a live probe may run, and
// it is still the EXISTING fresh guard path. The delegated path reads its own
// sources, so the observation's read counters do not attribute them.
func TestCoordinationCapacityObservationDeclaredSourceDelegatesToFreshGuard(t *testing.T) {
	cfg, home := coordinationCapacityFixture(t)
	now := coordinationCapacityNow
	observation := NewCoordinationCapacityObservation(cfg, home, now)
	probes := 0
	observation.probe = func(string, time.Time, time.Duration) (CodexBarCapacityEvidence, error) {
		probes++
		return CodexBarCapacityEvidence{}, errors.New("no live capacity available in this fixture")
	}
	result, err := observation.Guard()(work.LaneCapacityGuardRequest{
		HomePath:    home,
		Lane:        "codex",
		Now:         now,
		ReadOnly:    true,
		TupleBacked: true,
		CapacitySource: &work.LaneCapacitySource{
			Kind:             "codexbar",
			FreshnessMinutes: 15,
			Ref:              "codexbar://provider/codex",
		},
	})
	if err != nil {
		t.Fatalf("declared source: %v", err)
	}
	if probes != 1 {
		t.Fatalf("declared source ran %d live probes, want exactly 1", probes)
	}
	if !strings.Contains(result.Capacity.DataRef, "source_ref=codexbar://provider/codex") {
		t.Fatalf("declared source lost its provenance: %+v", result)
	}
}

// F8: the observation is read-only BY CONSTRUCTION. A request that is not
// read-only is refused before any branch runs, so it reads no source, probes
// no provider and writes nothing to the home.
func TestCoordinationCapacityObservationRefusesNonReadOnlyRequest(t *testing.T) {
	cfg, home := coordinationCapacityFixture(t)
	now := coordinationCapacityNow
	writeCapacityUsage(t, cfg, "codex", 100, now.Add(2*time.Hour), now.Add(-time.Minute))
	before := coordinationCapacitySnapshot(t, home)

	for _, tc := range []struct {
		name string
		req  work.LaneCapacityGuardRequest
	}{
		{
			name: "legacy lane",
			req:  coordinationCapacityRequest(home, "codex", now),
		},
		{
			name: "tuple backed without a declared source",
			req: work.LaneCapacityGuardRequest{
				HomePath: home, Lane: "codex", Now: now, ReadOnly: true, TupleBacked: true,
			},
		},
		{
			name: "declared source",
			req: work.LaneCapacityGuardRequest{
				HomePath:    home,
				Lane:        "codex",
				Now:         now,
				ReadOnly:    true,
				TupleBacked: true,
				CapacitySource: &work.LaneCapacitySource{
					Kind:             "codexbar",
					FreshnessMinutes: 15,
					// No zai row exists in the fixture, so the delegated fresh guard
					// WOULD run a live probe for this provider if the request reached it.
					Ref: "codexbar://provider/zai",
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observation := NewCoordinationCapacityObservation(cfg, home, now)
			probes := 0
			observation.probe = func(string, time.Time, time.Duration) (CodexBarCapacityEvidence, error) {
				probes++
				return CodexBarCapacityEvidence{}, errors.New("no live capacity available in this fixture")
			}
			req := tc.req
			req.ReadOnly = false
			if _, err := observation.Guard()(req); err == nil {
				t.Fatal("a request that is not read-only must be refused")
			}
			access := observation.SourceAccess()
			for path, count := range access.Reads {
				t.Fatalf("a refused request read %s %d times, want 0", filepath.Base(path), count)
			}
			for path, count := range access.Stats {
				t.Fatalf("a refused request probed %s %d times, want 0", filepath.Base(path), count)
			}
			if probes != 0 {
				t.Fatalf("a refused request ran %d live capacity probes", probes)
			}
		})
	}
	after := coordinationCapacitySnapshot(t, home)
	if before != after {
		t.Fatalf("a refused request mutated the home:\nBEFORE\n%s\nAFTER\n%s", before, after)
	}
}

// F9: a broad multi-provider feed must not be materialized. The fresh reader
// filters inside the scan, and each actor copies only its OWN family's rows —
// never an allocation proportional to the other providers' rows.
func TestCoordinationCapacityObservationDoesNotMaterializeUnrelatedRows(t *testing.T) {
	cfg, home := coordinationCapacityFixture(t)
	now := coordinationCapacityNow
	for i := 0; i < 200; i++ {
		appendCapacityUsageRow(t, cfg, "codex", 10, now.Add(2*time.Hour), now.Add(-time.Minute))
	}
	appendCapacityUsageRow(t, cfg, "claude", 20, now.Add(2*time.Hour), now.Add(-time.Minute))

	source := capacityRowSource{path: cfg.Paths.UsageHistoryPath, label: "codexbar/usage-history"}
	fresh, _, err := readCapacityRowsFromSource(source, now, func(provider string) bool {
		return normalizeLaneCapacityKey(provider) == "claude"
	})
	if err != nil {
		t.Fatalf("fresh reader: %v", err)
	}
	if len(fresh) != 1 {
		t.Fatalf("the fresh reader kept %d rows for one family, want 1", len(fresh))
	}
	if cap(fresh) >= 200 {
		t.Fatalf("the fresh reader sized its rows to the whole feed: cap=%d", cap(fresh))
	}

	observation := NewCoordinationCapacityObservation(cfg, home, now)
	if _, _, err := observation.facts.usageRows(); err != nil {
		t.Fatalf("observation usage rows: %v", err)
	}
	claudeRows := observation.facts.rowsForFamily("claude", now)
	if len(claudeRows) != 1 {
		t.Fatalf("the observation kept %d rows for one family, want 1", len(claudeRows))
	}
	if cap(claudeRows) != 1 {
		t.Fatalf("an actor's rows are sized to the whole feed: cap=%d", cap(claudeRows))
	}
}

// A tuple-backed request with no declared source consults no capacity source
// at all: runtime quota stays execution-gateway-owned.
func TestCoordinationCapacityObservationTupleBackedWithoutSourceIsGatewayOwned(t *testing.T) {
	cfg, home := coordinationCapacityFixture(t)
	observation := NewCoordinationCapacityObservation(cfg, home, coordinationCapacityNow)
	result, err := observation.Guard()(work.LaneCapacityGuardRequest{
		HomePath:    home,
		Lane:        "codex",
		Now:         coordinationCapacityNow,
		ReadOnly:    true,
		TupleBacked: true,
	})
	if err != nil {
		t.Fatalf("tuple-backed request: %v", err)
	}
	if result.Capacity.State != "gateway_owned" {
		t.Fatalf("tuple-backed decision = %+v, want gateway_owned", result)
	}
	access := observation.SourceAccess()
	for path, count := range access.Reads {
		if count != 0 {
			t.Fatalf("tuple-backed request read %s %d times, want 0", filepath.Base(path), count)
		}
	}
}
