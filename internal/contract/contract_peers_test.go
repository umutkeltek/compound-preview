package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
)

// writeLease drops a raw Lease JSON into work/leases so work.ListLeases reads it — the same
// on-disk shape the claim path writes. claimedAt is stamped relative to a runtime clock (never a
// fixed fixture date) so no fixture ages across the staleness window.
func writeLease(t *testing.T, home, workID, holder string, claimedAt time.Time) {
	t.Helper()
	dir := filepath.Join(home, "work", "leases")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir leases: %v", err)
	}
	body := fmt.Sprintf(`{"work_id":%q,"by":%q,"claimed_at":%q}`, workID, holder, claimedAt.UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, workID+".json"), []byte(body+"\n"), 0o644); err != nil {
		t.Fatalf("write lease: %v", err)
	}
}

// F1 — the peers section lists a LIVE and an EXPIRED lease; the expired one renders stale:true
// (never active), the live one stale:false. Staleness v1 is deterministic (expiry from claimed_at
// against an injected clock), so this asserts the falsifier (d) of the design packet.
func TestPeers_F1_LiveAndExpiredLeases(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	writeLease(t, home, "work_live", "lane-alpha", now.Add(-1*time.Hour)) // fresh → active
	writeLease(t, home, "work_dead", "lane-beta", now.Add(-48*time.Hour)) // past TTL → stale

	pack, err := PackWithCapabilities(mkInput("product"), DefaultPolicy(), home, now)
	if err != nil {
		t.Fatalf("PackWithCapabilities: %v", err)
	}
	if pack.Peers == nil {
		t.Fatalf("peers section must be present when leases exist")
	}
	if len(pack.Peers.ActiveLeases) != 2 {
		t.Fatalf("want 2 leases listed, got %d: %+v", len(pack.Peers.ActiveLeases), pack.Peers.ActiveLeases)
	}
	byID := map[string]struct {
		holder string
		stale  bool
		age    string
	}{}
	for _, l := range pack.Peers.ActiveLeases {
		byID[l.WorkID] = struct {
			holder string
			stale  bool
			age    string
		}{l.Holder, l.Stale, l.Age}
	}
	live, ok := byID["work_live"]
	if !ok {
		t.Fatalf("live lease missing: %+v", pack.Peers.ActiveLeases)
	}
	if live.stale {
		t.Fatalf("fresh lease must not be stale: %+v", live)
	}
	if live.holder != "lane-alpha" || live.age == "" {
		t.Fatalf("live lease holder/age wrong: %+v", live)
	}
	dead, ok := byID["work_dead"]
	if !ok {
		t.Fatalf("expired lease missing: %+v", pack.Peers.ActiveLeases)
	}
	if !dead.stale {
		t.Fatalf("expired lease MUST render stale (falsifier d): %+v", dead)
	}
}

// F2 — with 7 agent-lane nodes imported, the peers lane cards cap at 5, ordered deterministically
// by node id, and each card carries at most 2 top capabilities plus its tendency slugs.
func TestPeers_F2_LaneCardsCappedAndOrdered(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatalf("open capgraph: %v", err)
	}
	// Seven lanes with ids that sort predictably: lane0..lane6.
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("agent-lane:codex--auto-build--m%d--balanced", i)
		name := fmt.Sprintf("codex / auto/build / m%d / balanced", i)
		if err := store.Add(capgraph.Node{ID: id, Kind: "agent-lane", Name: name, Weight: 0.25, EvidenceOnly: true}); err != nil {
			t.Fatalf("add lane %d: %v", i, err)
		}
	}
	// lane0 gets three capabilities (must truncate to 2) and two tendencies.
	lane0 := "agent-lane:codex--auto-build--m0--balanced"
	for _, e := range []capgraph.Edge{
		{Kind: capgraph.EdCapableOf, From: lane0, To: "task-class:build-implement", Confidence: 1},
		{Kind: capgraph.EdCapableOf, From: lane0, To: "task-class:apply-a-patch", Confidence: 1},
		{Kind: capgraph.EdCapableOf, From: lane0, To: "task-class:tests-from-spec", Confidence: 1},
		{Kind: capgraph.EdTendsTo, From: lane0, To: "tendency:falsifier-first", Confidence: 1},
		{Kind: capgraph.EdTendsTo, From: lane0, To: "tendency:receipt-faithful", Confidence: 1},
	} {
		if err := store.AddEdge(e, now); err != nil {
			t.Fatalf("add edge: %v", err)
		}
	}

	pack, err := PackWithCapabilities(mkInput("product"), DefaultPolicy(), home, now)
	if err != nil {
		t.Fatalf("PackWithCapabilities: %v", err)
	}
	if pack.Peers == nil {
		t.Fatalf("peers section must be present when lanes exist")
	}
	if len(pack.Peers.Lanes) != 5 {
		t.Fatalf("lane cards must cap at 5, got %d", len(pack.Peers.Lanes))
	}
	// Deterministic order: sorted by id => lane0 first.
	if pack.Peers.Lanes[0].Lane != "codex / auto/build / m0 / balanced" {
		t.Fatalf("lane cards not ordered by id: first = %q", pack.Peers.Lanes[0].Lane)
	}
	first := pack.Peers.Lanes[0]
	if first.Harness != "codex" {
		t.Fatalf("lane card harness wrong: %q", first.Harness)
	}
	if len(first.TopCapabilities) != 2 {
		t.Fatalf("top_capabilities must cap at 2, got %d: %v", len(first.TopCapabilities), first.TopCapabilities)
	}
	if len(first.Tendencies) != 2 {
		t.Fatalf("tendencies should carry both slugs, got %v", first.Tendencies)
	}
}

// F3 — building the peers section reads the capability graph but must not write a single byte of
// the capability store (slice A's import idempotence is load-bearing: orient writes ZERO capgraph
// bytes). Snapshot the node/edge logs before and after a full orient pack.
func TestPeers_F3_CapgraphStoreBytesUnchanged(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatalf("open capgraph: %v", err)
	}
	lane := "agent-lane:codex--auto-build--gpt--balanced"
	if err := store.Add(capgraph.Node{ID: lane, Kind: "agent-lane", Name: "codex / auto/build / gpt / balanced", Weight: 0.25, EvidenceOnly: true}); err != nil {
		t.Fatalf("seed lane: %v", err)
	}
	if err := store.AddEdge(capgraph.Edge{Kind: capgraph.EdCapableOf, From: lane, To: "task-class:build-implement", Confidence: 1}, now); err != nil {
		t.Fatalf("seed edge: %v", err)
	}
	writeLease(t, home, "work_live", "lane-alpha", now.Add(-1*time.Hour))

	before := capgraphStoreHash(t, home)
	if _, err := PackWithCapabilities(mkInput("product"), DefaultPolicy(), home, now); err != nil {
		t.Fatalf("PackWithCapabilities: %v", err)
	}
	after := capgraphStoreHash(t, home)
	if before != after {
		t.Fatalf("orient wrote capgraph store bytes (read-only guard broken):\n before=%s\n  after=%s", before, after)
	}
}

// capgraphStoreHash hashes the authority store files (nodes + edges jsonl) — the bytes slice A's
// DeepEqual idempotence protects. A missing file hashes as empty (fresh home is fine).
func capgraphStoreHash(t *testing.T, home string) string {
	t.Helper()
	h := sha256.New()
	for _, name := range []string{"capability-nodes.jsonl", "capability-edges.jsonl"} {
		data, err := os.ReadFile(filepath.Join(home, "registries", name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read %s: %v", name, err)
		}
		fmt.Fprintf(h, "%s:%d:", name, len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// F5 — a home whose leases outgrow the cap yields a BOUNDED pack: the rows are capped at
// peerActiveLeaseCap, the drop is declared in active_leases_omitted (never silent), and the
// surviving rows are the live ones plus the newest stale claims — never the oldest, which is
// the end work.ListLeases returns first. This is the falsifier for the accumulation failure:
// leases are written per claim and removed per release, so a mature home carries hundreds of
// dead rows, and a cap that kept ListLeases' order would keep exactly the dead ones.
func TestPeers_F5_ActiveLeasesCappedNewestLiveFirstAndDeclared(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	// 30 stale leases, oldest first by construction: work_stale_00 is the oldest claim.
	const staleCount = 30
	for i := 0; i < staleCount; i++ {
		writeLease(t, home, fmt.Sprintf("work_stale_%02d", i), "lane-old",
			now.Add(-time.Duration(staleCount-i)*24*time.Hour))
	}
	// Two live leases, claimed within the staleness window.
	writeLease(t, home, "work_live_a", "lane-alpha", now.Add(-2*time.Hour))
	writeLease(t, home, "work_live_b", "lane-beta", now.Add(-1*time.Hour))

	pack, err := PackWithCapabilities(mkInput("product"), DefaultPolicy(), home, now)
	if err != nil {
		t.Fatalf("PackWithCapabilities: %v", err)
	}
	if pack.Peers == nil {
		t.Fatalf("peers section must be present when leases exist")
	}
	if got := len(pack.Peers.ActiveLeases); got != peerActiveLeaseCap {
		t.Fatalf("active leases must cap at %d, got %d", peerActiveLeaseCap, got)
	}
	total := staleCount + 2
	if want := total - peerActiveLeaseCap; pack.Peers.ActiveLeasesOmitted != want {
		t.Fatalf("omitted count must declare the drop: want %d, got %d", want, pack.Peers.ActiveLeasesOmitted)
	}
	// Both live leases survive the cap and lead the list.
	if pack.Peers.ActiveLeases[0].WorkID != "work_live_b" || pack.Peers.ActiveLeases[1].WorkID != "work_live_a" {
		t.Fatalf("live leases must lead, newest first: %+v", pack.Peers.ActiveLeases[:2])
	}
	for _, l := range pack.Peers.ActiveLeases[:2] {
		if l.Stale {
			t.Fatalf("live lease rendered stale: %+v", l)
		}
	}
	// The kept stale rows are the NEWEST claims, so the oldest must be gone.
	kept := map[string]bool{}
	for _, l := range pack.Peers.ActiveLeases {
		kept[l.WorkID] = true
	}
	if kept["work_stale_00"] {
		t.Fatalf("cap kept the oldest lease; ordering regressed to ListLeases order: %+v", pack.Peers.ActiveLeases)
	}
	if !kept[fmt.Sprintf("work_stale_%02d", staleCount-1)] {
		t.Fatalf("cap dropped the newest stale lease: %+v", pack.Peers.ActiveLeases)
	}
}

// F6 — under the cap, nothing is dropped and the omitted count stays zero, so a small home's
// pack never claims a truncation that did not happen (the field is omitempty on the wire).
func TestPeers_F6_UnderCapDeclaresNoOmission(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	writeLease(t, home, "work_one", "lane-alpha", now.Add(-1*time.Hour))
	writeLease(t, home, "work_two", "lane-beta", now.Add(-48*time.Hour))

	pack, err := PackWithCapabilities(mkInput("product"), DefaultPolicy(), home, now)
	if err != nil {
		t.Fatalf("PackWithCapabilities: %v", err)
	}
	if pack.Peers == nil || len(pack.Peers.ActiveLeases) != 2 {
		t.Fatalf("both leases must survive under the cap: %+v", pack.Peers)
	}
	if pack.Peers.ActiveLeasesOmitted != 0 {
		t.Fatalf("omitted must be 0 under the cap, got %d", pack.Peers.ActiveLeasesOmitted)
	}
}

// SummaryMD is the one injected surface renderInjection does not budget, so every fragment
// appended to it must carry its own bound. memoryLine has always capped; activePositionLine
// did not, and positions were the largest contributor. This pins BOTH to the same bound and
// pins what must survive it: the position id and slot stay whole, because they are how a
// reader resolves the untruncated body.
func TestSummaryFragmentsAreBounded(t *testing.T) {
	longStance := strings.Repeat("stance-body ", 200)
	line := activePositionLine("entity::topic::slot-abc123", "predicate", "constraint", longStance, 42, 3, false)

	if len(line) > 320 {
		t.Fatalf("position line unbounded at %d chars: %q", len(line), line)
	}
	if !strings.HasPrefix(line, "entity::topic::slot-abc123 [predicate=predicate, claim_type=constraint]") {
		t.Fatalf("position id/slot must survive the cap: %q", line)
	}
	if !strings.Contains(line, "...") {
		t.Fatalf("a truncated stance must say so: %q", line)
	}
	if !strings.Contains(line, "since-seq 42") || !strings.Contains(line, "chain-length 3") {
		t.Fatalf("provenance fields must survive the cap: %q", line)
	}

	if got := boundedSummaryText(strings.Repeat("x", 500)); len(got) != 180 {
		t.Fatalf("boundedSummaryText = %d chars, want 180", len(got))
	}
	if got := boundedSummaryText("short"); got != "short" {
		t.Fatalf("short text must pass through unchanged, got %q", got)
	}
}
