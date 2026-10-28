// Package narrate is the correspondent composer: it turns the same-tick
// attention projection, the orient cursor's changed-set, and recent receipt
// summaries into ONE typed, deterministic script of what compound has to say to
// the operator right now (compound.narrate.v0.1). It is a thin composer over
// existing owners (attention ranking/bands/actions, the orient cursor's since
// diff, receipt move-type/state-delta) — it recomputes NONE of them, so it is
// not a third meaning owner. The package is pure: Build takes plain inputs and
// returns a script plus the next thread-seen sidecar; ALL filesystem/loader
// wiring lives in cmd/compound/narrate_cli.go. It branches only on the
// closed act/band/kind/register vocabulary, never on review-area names, so a
// new area with a valid operator card produces a valid utterance with zero code
// change here (see narrate_test.go F8).
package narrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/attention"
	"compound/internal/reviewartifact"
)

// SchemaVersion is the served script contract.
const SchemaVersion = "compound.narrate.v0.1"

// WarnWeightFloor is the mood-invariant floor every warn's effective weight
// stays at or above (§5) so no mood can sedate a warning out of the operator's
// view.
const WarnWeightFloor = 0.6

// Closed speech-act set (§3). residue is the honest catch-all: anything that
// fits no rule is counted, never dropped.
const (
	ActAsk         = "ask"
	ActOffer       = "offer"
	ActWarn        = "warn"
	ActReport      = "report"
	ActAcknowledge = "acknowledge"
	ActResidue     = "residue"
)

// Registers (§4). Membership is always full-state; the register only decides
// whether a member is narrated as a beat (movement) or compressed into standing.
const (
	RegisterMovement = "movement"
	RegisterStanding = "standing"
)

// since states (§2).
const (
	SinceNew       = "new"
	SinceChanged   = "changed"
	SinceUnchanged = "unchanged"
	SinceResolved  = "resolved"
)

// Mood is a re-weighting function over a FIXED member set (§5) — declared only,
// never inferred from history or state.
type Mood string

const (
	MoodDecide Mood = "decide"
	MoodWander Mood = "wander"
	MoodWorry  Mood = "worry"
)

// staleThreshold is the age past which a needs-you item is promoted to a warn
// (§4, mirrors attention's own 48h staleness signal — not a new computation).
const staleThreshold = 48 * time.Hour

// Options carries only the declared knobs.
type Options struct {
	Mood Mood
}

// Inputs are the composed owners, all passed in (purity): no fs, no clock read.
type Inputs struct {
	Attention  attention.Report
	Changed    ChangedSet
	Receipts   []ReceiptSummary
	Traffic    TrafficSplit
	ThreadSeen map[string]ThreadSeen
	Injection  *InjectionLegibility
	Now        time.Time
}

// InjectionLegibility is the glanceable per-turn injection meter carried into
// narrate. It is derived upstream (cmd/compound via capgraph.LatestInjectionLegibility)
// from the latest ContextPack + its rendered injection; narrate only renders one
// beat from the numbers, mirroring how ReceiptSummary/TrafficSplit carry already-
// derived streams. Present is false when no ContextPack has been observed, so the
// beat is absent (never a fabricated "0 served").
type InjectionLegibility struct {
	Present         bool   `json:"present"`
	PackID          string `json:"pack_id"`
	ServedRefs      int    `json:"served_refs"`
	ServedMemory    int    `json:"served_memory"`
	ServedPositions int    `json:"served_positions"`
	ServedOffers    int    `json:"served_offers"`
	ExcludedRefs    int    `json:"excluded_refs"`
	ExcludedNoTask  int    `json:"excluded_no_task"`
	InjectionBytes  int    `json:"injection_bytes"`
}

// ChangedSet mirrors the exported needs of the orient operator cursor + its
// changed projection (cmd/compound/orient_operator.go orientChanged). narrate
// reads it; it NEVER advances the cursor.
type ChangedSet struct {
	CurrentSeq  int64
	LastSeenSeq int64
	FirstRun    bool
}

// hasNewMovement reports whether the cursor indicates unseen ledger movement.
// This is the only cursor-derived emphasis narrate applies to acknowledgements:
// it flips their since-state resolved<->unchanged, never their membership.
func (c ChangedSet) hasNewMovement() bool {
	return c.FirstRun || c.CurrentSeq-c.LastSeenSeq > 0
}

// ReceiptSummary is one recent receipt reduced to what an acknowledgement needs.
// Applied/Reason are derived upstream from the real Receipt (Outcome +
// EffectiveChanged + StateDelta) in the CLI — narrate only counts them, so the
// honest split composes from the receipt set and nothing else (§6, F4).
type ReceiptSummary struct {
	ReceiptID string
	WorkID    string
	MoveType  string
	Applied   bool
	Summary   string
	Reason    string
	CreatedAt string
}

// ThreadSeen is the emphasis-only sidecar entry (not consumption marking):
// first_seen_seq + a fingerprint (count|band|action-presence). Loss degrades
// honestly — everything reads `new` once, membership unaffected.
type ThreadSeen struct {
	FirstSeenSeq int64  `json:"first_seen_seq"`
	Fingerprint  string `json:"fingerprint"`
}

// Result is the script plus the next monotonic sidecar the CLI persists.
type Result struct {
	Script     Script
	ThreadSeen map[string]ThreadSeen
}

// Script is the served correspondent projection (§2).
type Script struct {
	SchemaVersion   string          `json:"schema_version"`
	GeneratedAt     string          `json:"generated_at"`
	SourceFreshness SourceFreshness `json:"source_freshness"`
	Cursor          Cursor          `json:"cursor"`
	Mood            string          `json:"mood"`
	ScriptHash      string          `json:"script_hash"`
	Counts          Counts          `json:"counts"`
	Beats           []Beat          `json:"beats"`
	Standing        Standing        `json:"standing"`
	Quiet           *Quiet          `json:"quiet"`
}

type SourceFreshness struct {
	Attention     string `json:"attention"`
	OrientChanged string `json:"orient_changed"`
}

type Cursor struct {
	LastSeenSeq int64 `json:"last_seen_seq"`
	CurrentSeq  int64 `json:"current_seq"`
	FirstRun    bool  `json:"first_run"`
}

// Counts is the honesty seam over the ATTENTION member set: narrated + folded
// == total_members always. Acknowledgements are an additional conversational
// stream (yesterday's results) and are not part of this thread accounting.
type Counts struct {
	TotalMembers int `json:"total_members"`
	Narrated     int `json:"narrated"`
	Folded       int `json:"folded"`
	WarnCount    int `json:"warn_count"`
	Residue      int `json:"residue"`
}

type Since struct {
	State        string `json:"state"`
	FirstSeenSeq int64  `json:"first_seen_seq,omitempty"`
}

type Provenance struct {
	Refs      []string `json:"refs"`
	DetailRef string   `json:"detail_ref,omitempty"`
}

// Beat is one typed utterance. action is the verbatim attention Action (never
// synthesized here). in_response_to/receipt_refs/move_type are acknowledge-only.
type Beat struct {
	Act          string              `json:"act"`
	Voice        string              `json:"voice"`
	Body         string              `json:"body"`
	Unlock       string              `json:"unlock,omitempty"`
	Weight       float64             `json:"weight"`
	WeightFloor  float64             `json:"weight_floor,omitempty"`
	ThreadID     string              `json:"thread_id"`
	Since        Since               `json:"since"`
	SceneOrder   int                 `json:"scene_order"`
	PathReason   string              `json:"path_reason"`
	Register     string              `json:"register"`
	Governs      bool                `json:"governs"`
	Action       *attention.Action   `json:"action"`
	InResponseTo string              `json:"in_response_to,omitempty"`
	ReceiptRefs  []string            `json:"receipt_refs,omitempty"`
	MoveType     string              `json:"move_type,omitempty"`
	Provenance   Provenance          `json:"provenance"`
	Freshness    attention.Freshness `json:"freshness"`
}

type Standing struct {
	Summary string          `json:"summary"`
	Groups  []StandingGroup `json:"groups"`
}

type StandingGroup struct {
	ThreadID  string `json:"thread_id"`
	Count     int    `json:"count"`
	Band      string `json:"band"`
	DetailRef string `json:"detail_ref,omitempty"`
}

type Quiet struct {
	Reason      string `json:"reason"`
	FoldedCount int    `json:"folded_count"`
	Since       string `json:"since"`
}

// member is a per-thread working record before it becomes a beat or a fold.
type member struct {
	group    attention.Group
	act      string
	since    Since
	baseWt   float64
	movement bool // since is new|changed (movement-eligible)
}

// Build composes the script. Pure: no wall-clock read (now is passed), no
// randomness, sorted iteration everywhere.
func Build(in Inputs, opt Options) (Result, error) {
	now := in.Now
	if now.IsZero() {
		return Result{}, fmt.Errorf("narrate.Build: now is required (purity: no wall-clock read)")
	}
	mood := normalizeMood(opt.Mood)

	nextSeen := cloneSeen(in.ThreadSeen)
	members := make([]member, 0, len(in.Attention.Groups))
	for _, g := range in.Attention.Groups {
		since := sinceFor(g, in.ThreadSeen, in.Changed.CurrentSeq)
		nextSeen[g.Key] = ThreadSeen{FirstSeenSeq: since.FirstSeenSeq, Fingerprint: fingerprint(g)}
		members = append(members, member{
			group:    g,
			act:      actFor(g, now),
			since:    since,
			baseWt:   baseWeight(g),
			movement: since.State == SinceNew || since.State == SinceChanged,
		})
	}

	beats, folded, counts := composeAttention(members, mood)

	acks, ackFolds := composeAcks(in.Receipts, in.Changed.hasNewMovement(), mood)
	beats = append(beats, acks...)

	// The cheap-share beat is an additional conversational stream (like acks),
	// appended AFTER composeAttention has fixed Counts.Narrated — so it never
	// enters the attention conservation accounting (narrated+folded==members).
	if tb, ok := composeTraffic(in.Traffic, mood); ok {
		beats = append(beats, tb)
	}

	// The injection-legibility beat is the same shape: a standing glance over the
	// latest ContextPack, outside the attention accounting.
	if ib, ok := composeInjection(in.Injection, mood); ok {
		beats = append(beats, ib)
	}

	assignSceneOrder(beats)

	standing := composeStanding(folded, ackFolds, counts)
	quiet := composeQuiet(beats, counts, in.Changed, anyNeedsYou(members))

	script := Script{
		SchemaVersion:   SchemaVersion,
		GeneratedAt:     now.UTC().Format(time.RFC3339),
		SourceFreshness: SourceFreshness{Attention: in.Attention.GeneratedAt, OrientChanged: now.UTC().Format(time.RFC3339)},
		Cursor:          Cursor{LastSeenSeq: in.Changed.LastSeenSeq, CurrentSeq: in.Changed.CurrentSeq, FirstRun: in.Changed.FirstRun},
		Mood:            string(mood),
		Counts:          counts,
		Beats:           beats,
		Standing:        standing,
		Quiet:           quiet,
	}
	script.ScriptHash = hashScript(script)
	return Result{Script: script, ThreadSeen: nextSeen}, nil
}

func normalizeMood(m Mood) Mood {
	switch m {
	case MoodDecide, MoodWander, MoodWorry:
		return m
	default:
		return MoodDecide
	}
}

// actFor derives the speech act by branching on Kind/band/action/age ONLY —
// never on the review-area name (§3, F8).
func actFor(g attention.Group, now time.Time) string {
	if g.Kind == attention.KindJobHealth {
		return ActWarn
	}
	if g.Band == attention.BandNeedsYou && ageExceeds(g.OldestCreatedAt, now, staleThreshold) {
		return ActWarn
	}
	if g.Band == attention.BandNeedsYou && g.Action != nil {
		return ActAsk
	}
	if g.Action != nil && g.Band != attention.BandNeedsYou && isOfferKind(g.Kind) {
		return ActOffer
	}
	if g.Band == attention.BandWatching {
		return ActReport
	}
	return ActResidue
}

func isOfferKind(kind string) bool {
	switch kind {
	case attention.KindProductMove, attention.KindAdvance, attention.KindIntake:
		return true
	default:
		return false
	}
}

func ageExceeds(oldest string, now time.Time, d time.Duration) bool {
	oldest = strings.TrimSpace(oldest)
	t, err := time.Parse(time.RFC3339, oldest)
	if err != nil {
		// Attention preserves the work lease writer's native Unix nanoseconds.
		nanos, err := strconv.ParseInt(oldest, 10, 64)
		if err != nil || nanos <= 0 {
			return false
		}
		t = time.Unix(0, nanos)
	}
	return now.UTC().Sub(t.UTC()) > d
}

// sinceFor computes the thread's novelty state from the sidecar (not the cursor)
// so attention membership stays cursor-invariant (F1). Attention threads are
// never `resolved` — a resolved thread has left the input entirely.
func sinceFor(g attention.Group, seen map[string]ThreadSeen, currentSeq int64) Since {
	prior, ok := seen[g.Key]
	if !ok {
		return Since{State: SinceNew, FirstSeenSeq: currentSeq}
	}
	if prior.Fingerprint != fingerprint(g) {
		return Since{State: SinceChanged, FirstSeenSeq: prior.FirstSeenSeq}
	}
	return Since{State: SinceUnchanged, FirstSeenSeq: prior.FirstSeenSeq}
}

func fingerprint(g attention.Group) string {
	return fmt.Sprintf("%d|%s|%t", g.Count, g.Band, g.Action != nil)
}

// baseWeight is derived from the existing attention leverage + band + since —
// a monotone squash into [0,1), never a new ranking computation (§2).
func baseWeight(g attention.Group) float64 {
	lev := g.Leverage
	if lev < 0 {
		lev = 0
	}
	base := lev / (lev + 4.0) // (0,inf) -> [0,1)
	switch g.Band {
	case attention.BandNeedsYou:
		base += 0.15
	case attention.BandWatching:
		base += 0.05
	}
	return clamp01(base)
}

// moodFactor is the per-act re-weighting (§5): it changes order/verbosity/lead,
// never membership, and never lowers a warn below the floor.
func moodFactor(mood Mood, act string) float64 {
	switch mood {
	case MoodWander:
		switch act {
		case ActAsk:
			return 0.9
		case ActOffer:
			return 1.0
		case ActWarn:
			return 1.0
		case ActReport:
			return 1.3
		case ActAcknowledge:
			return 1.1
		}
	case MoodWorry:
		switch act {
		case ActAsk:
			return 1.1
		case ActOffer:
			return 0.8
		case ActWarn:
			return 1.6
		case ActReport:
			return 0.6
		case ActAcknowledge:
			return 0.7
		}
	default: // decide
		switch act {
		case ActAsk:
			return 1.4
		case ActOffer:
			return 1.2
		case ActWarn:
			return 1.0
		case ActReport:
			return 0.7
		case ActAcknowledge:
			return 0.6
		}
	}
	return 1.0
}

func capFor(mood Mood) int {
	switch mood {
	case MoodWander:
		return 9
	case MoodWorry:
		return 5
	default:
		return 7
	}
}

// composeAttention turns members into narrated beats + folded standing rows.
// Warns are always narrated (never folded, never capped) so no mood or cap can
// sedate a warning. Non-warn movement beats are capped by mood; the overflow
// and every unchanged/residue thread fold — counted, never dropped.
func composeAttention(members []member, mood Mood) ([]Beat, []StandingGroup, Counts) {
	var warns, movement []member
	var folds []StandingGroup
	counts := Counts{TotalMembers: len(members)}

	for _, m := range members {
		if m.act == ActResidue {
			counts.Residue++
		}
		if m.act == ActWarn {
			warns = append(warns, m)
			continue
		}
		if m.movement && m.act != ActResidue {
			movement = append(movement, m)
			continue
		}
		folds = append(folds, foldRow(m))
	}

	// deterministic ordering for the cap decision: mood-effective weight desc,
	// tie-break by thread key.
	sort.SliceStable(movement, func(i, j int) bool {
		wi := effectiveWeight(movement[i], mood)
		wj := effectiveWeight(movement[j], mood)
		if wi != wj {
			return wi > wj
		}
		return movement[i].group.Key < movement[j].group.Key
	})

	cap := capFor(mood)
	var beats []Beat
	for _, m := range warns {
		beats = append(beats, buildAttentionBeat(m, mood))
		counts.WarnCount++
	}
	for i, m := range movement {
		if i >= cap {
			folds = append(folds, foldRow(m))
			continue
		}
		beats = append(beats, buildAttentionBeat(m, mood))
	}

	counts.Narrated = len(beats)
	counts.Folded = counts.TotalMembers - counts.Narrated
	return beats, folds, counts
}

func foldRow(m member) StandingGroup {
	return StandingGroup{
		ThreadID:  m.group.Key,
		Count:     m.group.Count,
		Band:      m.group.Band,
		DetailRef: detailRef(m.group),
	}
}

func effectiveWeight(m member, mood Mood) float64 {
	w := clamp01(m.baseWt * moodFactor(mood, m.act))
	if m.act == ActWarn && w < WarnWeightFloor {
		w = WarnWeightFloor
	}
	return w
}

func buildAttentionBeat(m member, mood Mood) Beat {
	g := m.group
	body, unlock := beatProse(g)
	register := RegisterMovement
	if m.act == ActWarn && !m.movement {
		// a still-failing warn is standing by since, but it still leads.
		register = RegisterStanding
	}
	beat := Beat{
		Act:        m.act,
		Voice:      voiceFor(m.act),
		Body:       body,
		Unlock:     unlock,
		Weight:     effectiveWeight(m, mood),
		ThreadID:   g.Key,
		Since:      m.since,
		PathReason: pathReason(m.act),
		Register:   register,
		Governs:    false, // slice 1: standing-law signal not yet surfaced in attention (follow-up 1.5).
		Action:     cloneAction(g.Action),
		Provenance: Provenance{Refs: append([]string{}, g.ItemIDs...), DetailRef: detailRef(g)},
		Freshness:  g.Freshness,
	}
	if m.act == ActWarn {
		beat.WeightFloor = WarnWeightFloor
	}
	return beat
}

// beatProse takes the operator sentence verbatim from attention (already
// validated upstream) and falls back to a specific, plain sentence only if a
// fixture is malformed — so every beat body passes ValidateOperatorCard (F6)
// and every FALLBACK body narrate itself authors also clears the eighth-grade
// readability ceiling. The retired generic "A new item needs your read." line
// never returns: the fallback names the queue the item came from, and degrades
// to a plain terminal sentence when that name is too dense to read.
func beatProse(g attention.Group) (string, string) {
	body := strings.TrimSpace(g.OperatorSummary)
	unlock := reviewartifact.ShortLabel(g.OperatorUnlock, 90)
	if proseOK(body, unlock, voiceFor(ActReport), ActReport) {
		return body, unlock
	}
	const fallbackUnlock = "Open the review queue to read it."
	if area := reviewartifact.PlainLabel(g.Area); area != "" {
		areaBody := fmt.Sprintf("A %s item needs your decision.", area)
		if proseOK(areaBody, fallbackUnlock, voiceFor(ActReport), ActReport) && reviewartifact.Readable(areaBody) {
			return areaBody, fallbackUnlock
		}
	}
	return "A review item needs your decision.", fallbackUnlock
}

func proseOK(body, unlock, label, act string) bool {
	return reviewartifact.ValidateOperatorCard(reviewartifact.OperatorCard{
		Summary:      body,
		ContextLabel: firstNonEmpty(label, "compound"),
		GroupKey:     "narrate|" + act,
		Unlock:       unlock,
	}) == nil
}

func voiceFor(act string) string {
	switch act {
	case ActWarn:
		return "The monitor"
	case ActAsk:
		return "The desk"
	case ActOffer:
		return "The workbench"
	case ActReport:
		return "The board"
	case ActAcknowledge:
		return "The ledger"
	default:
		return "The board"
	}
}

func pathReason(act string) string {
	switch act {
	case ActWarn:
		return "a failing or stale item leads regardless of novelty"
	case ActAsk:
		return "a blocking decision waits on you"
	case ActOffer:
		return "an optional move is ready when you are"
	case ActReport:
		return "recent movement worth a glance"
	case ActAcknowledge:
		return "results from your last visit"
	default:
		return "held for the standing view"
	}
}

func detailRef(g attention.Group) string {
	// a stable non-machine drill-down hint; never a path or id in prose.
	return "reviews:" + g.Band
}

// composeAcks builds one acknowledgement per move-type over the recent receipt
// set. The applied/failed split is counted from the receipts only (§6, F4).
// When the cursor shows nothing new, acks fold to standing (unchanged) instead
// of re-narrating yesterday's results — but the thread stays a member (F1).
func composeAcks(receipts []ReceiptSummary, resolved bool, mood Mood) ([]Beat, []StandingGroup) {
	if len(receipts) == 0 {
		return nil, nil
	}
	groups := map[string][]ReceiptSummary{}
	for _, r := range receipts {
		mt := firstNonEmpty(r.MoveType, "outward_delta")
		groups[mt] = append(groups[mt], r)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var beats []Beat
	var folds []StandingGroup
	for _, mt := range keys {
		set := groups[mt]
		applied := 0
		reason := ""
		refs := make([]string, 0, len(set))
		for _, r := range set {
			refs = append(refs, r.ReceiptID)
			if r.Applied {
				applied++
			} else if reason == "" {
				reason = firstNonEmpty(r.Reason, r.Summary)
			}
		}
		thread := mt + "|acknowledged"
		if !resolved {
			folds = append(folds, StandingGroup{ThreadID: thread, Count: len(set), Band: reviewartifact.BandFiled, DetailRef: "reviews:filed"})
			continue
		}
		body := ackBody(mt, applied, len(set), reason)
		wt := clamp01(0.5 * moodFactor(mood, ActAcknowledge))
		beats = append(beats, Beat{
			Act:          ActAcknowledge,
			Voice:        ackVoice(mt),
			Body:         body,
			Weight:       wt,
			ThreadID:     thread,
			Since:        Since{State: SinceResolved},
			PathReason:   pathReason(ActAcknowledge),
			Register:     RegisterMovement,
			InResponseTo: thread,
			ReceiptRefs:  refs,
			MoveType:     mt,
			Provenance:   Provenance{Refs: refs, DetailRef: "reviews:filed"},
			Freshness:    attention.Freshness{Source: "receipts"},
		})
	}
	return beats, folds
}

// ackBody composes the honest split sentence in plain words and falls back to
// a simpler true-count line if the composed reason cannot pass the grammar or
// the eighth-grade ceiling (keeps F6 and the readability gate green).
func ackBody(moveType string, applied, total int, reason string) string {
	phrase := moveTypePhrase(moveType)
	failed := total - applied
	var body string
	switch {
	case failed == 0:
		body = fmt.Sprintf("%d of %d %s joined.", applied, total, phrase)
	case strings.TrimSpace(reason) != "":
		clean := reviewartifact.SanitizeFragment(reason)
		clean = reviewartifact.ShortLabel(clean, 60)
		body = fmt.Sprintf("%d of %d %s joined; %d failed on %s.", applied, total, phrase, failed, strings.TrimRight(clean, "."))
	default:
		body = fmt.Sprintf("%d of %d %s joined; %d failed.", applied, total, phrase, failed)
	}
	if proseOK(body, "", ackVoice(moveType), ActAcknowledge) && reviewartifact.Readable(body) {
		return body
	}
	// safe fallback that still reports the true counts.
	fallback := fmt.Sprintf("%d of %d applied; %d failed.", applied, total, failed)
	if proseOK(fallback, "", ackVoice(moveType), ActAcknowledge) {
		return fallback
	}
	return "Recorded work from your last visit."
}

// moveTypePhrase maps a move_type constant to plain words. move_type is a
// sanctioned axis (not a review-area name) so this stays clear of the F8 grep.
func moveTypePhrase(moveType string) string {
	switch moveType {
	case "memory_delta":
		return "memory updates"
	case "rule_delta":
		return "rule updates"
	case "control_plane_repair":
		return "control-plane repairs"
	case "audit_only":
		return "audit notes"
	default:
		return "changes"
	}
}

func ackVoice(moveType string) string {
	switch moveType {
	case "memory_delta":
		return "Memory"
	case "rule_delta":
		return "The rules"
	case "control_plane_repair":
		return "The repair log"
	case "audit_only":
		return "The audit"
	default:
		return "The ledger"
	}
}

// assignSceneOrder sorts every narrated beat by effective weight desc (tie-break
// thread key) and stamps a monotonic scene_order. Renderers never re-sort (§2).
func assignSceneOrder(beats []Beat) {
	sort.SliceStable(beats, func(i, j int) bool {
		if beats[i].Weight != beats[j].Weight {
			return beats[i].Weight > beats[j].Weight
		}
		return beats[i].ThreadID < beats[j].ThreadID
	})
	for i := range beats {
		beats[i].SceneOrder = i + 1
	}
}

func composeStanding(attnFolds, ackFolds []StandingGroup, counts Counts) Standing {
	groups := append([]StandingGroup{}, attnFolds...)
	groups = append(groups, ackFolds...)
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].ThreadID < groups[j].ThreadID })

	watching, needsYou, filed := 0, 0, 0
	for _, g := range attnFolds {
		switch g.Band {
		case attention.BandNeedsYou:
			needsYou += g.Count
		case attention.BandWatching:
			watching += g.Count
		default:
			filed += g.Count
		}
	}
	summary := standingSummary(counts.Folded, watching, needsYou, filed)
	return Standing{Summary: summary, Groups: groups}
}

func standingSummary(folded, watching, needsYou, filed int) string {
	if folded <= 0 {
		return "Everything moving is shown above."
	}
	parts := []string{}
	if watching > 0 {
		parts = append(parts, fmt.Sprintf("%d watching", watching))
	}
	if needsYou > 0 {
		parts = append(parts, fmt.Sprintf("%d still need you", needsYou))
	}
	if filed > 0 {
		parts = append(parts, fmt.Sprintf("%d filed", filed))
	}
	body := fmt.Sprintf("%s holding steady", plural(folded, "item is", "items are"))
	if len(parts) > 0 {
		body += " — " + strings.Join(parts, ", ")
	}
	body += "."
	if proseOK(body, "", "standing", ActReport) {
		return body
	}
	return fmt.Sprintf("%s holding steady.", plural(folded, "item is", "items are"))
}

// anyNeedsYou reports whether any member sits in the needs-you band — including
// one that folded to residue (a needs-you item with no one-click action). Quiet
// must never claim calm while such an item is outstanding, even unnarrated.
func anyNeedsYou(members []member) bool {
	for _, m := range members {
		if m.group.Band == attention.BandNeedsYou {
			return true
		}
	}
	return false
}

// composeQuiet authors the calm end-state whenever nothing needs the operator
// (no ask, no warn, no needs-you member at all) — the quiet block is composed,
// never a blank beats array.
func composeQuiet(beats []Beat, counts Counts, changed ChangedSet, needsYou bool) *Quiet {
	if needsYou {
		return nil
	}
	for _, b := range beats {
		if b.Act == ActAsk || b.Act == ActWarn {
			return nil
		}
	}
	since := "steady since your last visit"
	if changed.FirstRun {
		since = "this is your first visit"
	}
	reason := "Nothing needs you right now."
	return &Quiet{Reason: reason, FoldedCount: counts.Folded, Since: since}
}

// hashScript is sha256 over the canonical JSON with generated_at +
// source_freshness zeroed (§2) — the conservation baseline.
func hashScript(s Script) string {
	s.GeneratedAt = ""
	s.SourceFreshness = SourceFreshness{}
	s.ScriptHash = ""
	data, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func cloneSeen(in map[string]ThreadSeen) map[string]ThreadSeen {
	out := make(map[string]ThreadSeen, len(in)+4)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneAction(a *attention.Action) *attention.Action {
	if a == nil {
		return nil
	}
	cp := *a
	cp.Argv = append([]string{}, a.Argv...)
	return &cp
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 0.999 {
		return 0.999
	}
	return math.Round(v*1000) / 1000
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func plural(count int, one, many string) string {
	if count == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", count, many)
}
