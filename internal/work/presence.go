package work

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Presence answers "who is here right now" from evidence Compound already
// holds: lease holders and recent work-event actors. It deliberately does NOT
// consult delegate bindings or run-attempt origin, because those record who
// Compound LAUNCHED, not who is PRESENT. An agent Compound never spawned
// appears here the moment it touches the ledger -- which is the point.
//
// This is a read-only projection. It writes nothing and it never takes the
// work-event writer lock, so it stays safe to run against a home that a dozen
// live agents are actively mutating.
const (
	PresenceProjectionSchemaVersion = "compound.presence_projection.v0.1"
	PresenceAuthorityRef            = "compound://presence/projection"

	// DefaultPresenceTTL mirrors the run-attempt stall sweep's 30m staleness
	// threshold, so "quiet long enough to stop counting" means the same span
	// everywhere in the system rather than being re-invented here.
	DefaultPresenceTTL = 30 * time.Minute

	// defaultPresenceTailBytes bounds the work-log read. Presence must cost the
	// same whether the ledger holds a day or a year: a metric whose cost scales
	// with the backlog goes blind exactly when the backlog is worth seeing.
	defaultPresenceTailBytes int64 = 1 << 20
)

// PresenceSourceKind names where a presence observation came from. Both are
// evidence; neither is authority.
const (
	PresenceSourceLease = "lease"
	PresenceSourceEvent = "event"
)

// PresenceObservation is one dated sighting of an actor.
type PresenceObservation struct {
	Kind   string `json:"kind"`
	WorkID string `json:"work_id"`
	At     string `json:"at"`
	Detail string `json:"detail,omitempty"`
}

// PresenceRow is one actor and the evidence that it is here.
type PresenceRow struct {
	Actor      string                `json:"actor"`
	LastSeen   string                `json:"last_seen"`
	AgeSeconds int64                 `json:"age_seconds"`
	Fresh      bool                  `json:"fresh"`
	Leases     int                   `json:"leases"`
	Events     int                   `json:"events"`
	WorkIDs    []string              `json:"work_ids,omitempty"`
	Latest     []PresenceObservation `json:"latest,omitempty"`
}

// PresenceCounts is the at-a-glance tally.
type PresenceCounts struct {
	Actors      int `json:"actors"`
	Fresh       int `json:"fresh"`
	Stale       int `json:"stale"`
	LeaseOnly   int `json:"lease_only"`
	EventOnly   int `json:"event_only"`
	BothSources int `json:"both_sources"`
	// Sessions counts announcements; Corroborated counts the subset whose
	// claimed actor is independently fresh in lease/event evidence.
	Sessions     int `json:"sessions"`
	Corroborated int `json:"corroborated_sessions"`
}

// Corroboration bases. "none" means the claimed actor has no independent
// evidence at all -- the declaration stands alone and buys nothing.
const (
	PresenceCorroborationNone  = "none"
	PresenceCorroborationStale = "stale_evidence"
	PresenceCorroborationLease = "lease"
	PresenceCorroborationEvent = "events"
	PresenceCorroborationBoth  = "lease+events"
)

// PresenceSessionRow is one session's claimed identity joined to whatever
// independent evidence exists for the actor it claims to be.
type PresenceSessionRow struct {
	PresenceSessionAnnouncement
	// Corroborated is true only when the claimed actor is independently fresh
	// in lease/event evidence. A declaration never sets this on its own.
	Corroborated bool   `json:"corroborated"`
	Basis        string `json:"corroboration_basis"`
	ActorAgeSecs int64  `json:"actor_age_seconds"`
}

// PresenceProjection is the read-only answer to "who is here".
type PresenceProjection struct {
	SchemaVersion string         `json:"schema_version"`
	AuthorityRef  string         `json:"authority_ref"`
	Status        string         `json:"status"`
	GeneratedAt   string         `json:"generated_at"`
	SourceRefs    []string       `json:"source_refs"`
	NextCommand   []string       `json:"next_command"`
	TTLSeconds    int64          `json:"ttl_seconds"`
	TailBytes     int64          `json:"tail_bytes"`
	TailTruncated bool           `json:"tail_truncated"`
	Counts        PresenceCounts `json:"counts"`
	Present       []PresenceRow  `json:"present"`
	// Sessions are self-declared identities, each carrying its own
	// corroboration verdict. They are reported beside Present, never merged
	// into it: an announcement is a claim, not a sighting.
	Sessions  []PresenceSessionRow `json:"sessions,omitempty"`
	Integrity []string             `json:"integrity,omitempty"`
}

// PresenceOptions tunes the projection. Zero values take the defaults.
type PresenceOptions struct {
	// TTL is how long after its last sighting an actor still counts as fresh.
	TTL time.Duration
	// TailBytes bounds the work-log tail read.
	TailBytes int64
	// IncludeStale keeps actors whose last sighting is older than TTL in the
	// Present slice (always marked Fresh=false). They are counted either way.
	IncludeStale bool
	// MaxObservations caps the per-actor Latest slice.
	MaxObservations int
}

func (o PresenceOptions) withDefaults() PresenceOptions {
	if o.TTL <= 0 {
		o.TTL = DefaultPresenceTTL
	}
	if o.TailBytes <= 0 {
		o.TailBytes = defaultPresenceTailBytes
	}
	if o.MaxObservations <= 0 {
		o.MaxObservations = 3
	}
	return o
}

type presenceAccumulator struct {
	actor    string
	lastSeen time.Time
	leases   int
	events   int
	workIDs  map[string]struct{}
	latest   []PresenceObservation
}

// PresenceProjectionFor builds the presence view as of now.
func (s *Store) PresenceProjectionFor(now time.Time, optionSets ...PresenceOptions) (PresenceProjection, error) {
	if now.IsZero() {
		now = time.Now()
	}
	opts := PresenceOptions{}
	if len(optionSets) > 0 {
		opts = optionSets[0]
	}
	opts = opts.withDefaults()

	projection := PresenceProjection{
		SchemaVersion: PresenceProjectionSchemaVersion,
		AuthorityRef:  PresenceAuthorityRef,
		Status:        "ok",
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		SourceRefs:    []string{"compound.work.leases", "compound.events.work_log"},
		NextCommand:   []string{"compound", "presence", "--json"},
		TTLSeconds:    int64(opts.TTL / time.Second),
		TailBytes:     opts.TailBytes,
		Present:       []PresenceRow{},
	}

	acc := map[string]*presenceAccumulator{}

	leases, err := s.ListLeases()
	if err != nil {
		return PresenceProjection{}, err
	}
	for _, lease := range leases {
		actor := strings.TrimSpace(lease.By)
		if actor == "" {
			continue
		}
		at, ok := parsePresenceTime(lease.ClaimAt)
		if !ok {
			projection.Integrity = appendPresenceIntegrity(projection.Integrity,
				"lease with unparseable claimed_at: "+lease.WorkID)
			continue
		}
		entry := presenceEntry(acc, actor)
		entry.leases++
		entry.workIDs[lease.WorkID] = struct{}{}
		if at.After(entry.lastSeen) {
			entry.lastSeen = at
		}
		entry.latest = append(entry.latest, PresenceObservation{
			Kind:   PresenceSourceLease,
			WorkID: lease.WorkID,
			At:     at.UTC().Format(time.RFC3339),
		})
	}

	events, truncated, err := s.readWorkLogTail(opts.TailBytes)
	if err != nil {
		return PresenceProjection{}, err
	}
	projection.TailTruncated = truncated
	for _, event := range events {
		actor := strings.TrimSpace(event.By)
		if actor == "" {
			continue
		}
		at, ok := parsePresenceTime(event.TS)
		if !ok {
			continue
		}
		entry := presenceEntry(acc, actor)
		entry.events++
		entry.workIDs[event.WorkID] = struct{}{}
		if at.After(entry.lastSeen) {
			entry.lastSeen = at
		}
		entry.latest = append(entry.latest, PresenceObservation{
			Kind:   PresenceSourceEvent,
			WorkID: event.WorkID,
			At:     at.UTC().Format(time.RFC3339),
			Detail: presenceTransition(event),
		})
	}

	rows := make([]PresenceRow, 0, len(acc))
	for _, entry := range acc {
		age := now.Sub(entry.lastSeen)
		if age < 0 {
			age = 0
		}
		fresh := age <= opts.TTL
		row := PresenceRow{
			Actor:      entry.actor,
			LastSeen:   entry.lastSeen.UTC().Format(time.RFC3339),
			AgeSeconds: int64(age / time.Second),
			Fresh:      fresh,
			Leases:     entry.leases,
			Events:     entry.events,
			WorkIDs:    sortedPresenceKeys(entry.workIDs),
			Latest:     latestPresenceObservations(entry.latest, opts.MaxObservations),
		}
		projection.Counts.Actors++
		if fresh {
			projection.Counts.Fresh++
		} else {
			projection.Counts.Stale++
		}
		switch {
		case entry.leases > 0 && entry.events > 0:
			projection.Counts.BothSources++
		case entry.leases > 0:
			projection.Counts.LeaseOnly++
		default:
			projection.Counts.EventOnly++
		}
		if !fresh && !opts.IncludeStale {
			continue
		}
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].LastSeen != rows[j].LastSeen {
			return rows[i].LastSeen > rows[j].LastSeen
		}
		return rows[i].Actor < rows[j].Actor
	})
	projection.Present = rows

	// Corroboration reads ONLY `acc`, which is built exclusively from leases and
	// work events above. Announcements are never written into `acc`, so a
	// self-declaration structurally cannot corroborate itself -- that is the
	// conjunctive rule enforced by construction rather than by convention.
	announcements, err := s.ListPresenceSessions()
	if err != nil {
		return PresenceProjection{}, err
	}
	if len(announcements) > 0 {
		projection.SourceRefs = appendSourceRef(projection.SourceRefs, "compound.observed.presence_sessions")
	}
	sessions := make([]PresenceSessionRow, 0, len(announcements))
	for _, announcement := range announcements {
		row := PresenceSessionRow{
			PresenceSessionAnnouncement: announcement,
			Basis:                       PresenceCorroborationNone,
		}
		if entry, seen := acc[announcement.Actor]; seen {
			age := now.Sub(entry.lastSeen)
			if age < 0 {
				age = 0
			}
			row.ActorAgeSecs = int64(age / time.Second)
			switch {
			case age > opts.TTL:
				row.Basis = PresenceCorroborationStale
			case entry.leases > 0 && entry.events > 0:
				row.Basis = PresenceCorroborationBoth
			case entry.leases > 0:
				row.Basis = PresenceCorroborationLease
			default:
				row.Basis = PresenceCorroborationEvent
			}
			row.Corroborated = age <= opts.TTL
		}
		projection.Counts.Sessions++
		if row.Corroborated {
			projection.Counts.Corroborated++
		}
		sessions = append(sessions, row)
	}
	projection.Sessions = sessions
	return projection, nil
}

func presenceEntry(acc map[string]*presenceAccumulator, actor string) *presenceAccumulator {
	entry, ok := acc[actor]
	if !ok {
		entry = &presenceAccumulator{actor: actor, workIDs: map[string]struct{}{}}
		acc[actor] = entry
	}
	return entry
}

// readWorkLogTail reads at most tailBytes from the end of the work event log
// WITHOUT taking the writer lock, and drops a leading partial line so a
// mid-record boundary never yields a bogus event.
func (s *Store) readWorkLogTail(tailBytes int64) ([]Event, bool, error) {
	size, err := s.events.WorkLogSize()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	offset := int64(0)
	truncated := false
	if size > tailBytes {
		offset = size - tailBytes
		truncated = true
	}
	raw, err := s.events.ReadWorkLogAt(offset, size-offset)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	lines := bytes.Split(raw, []byte("\n"))
	if truncated && len(lines) > 0 {
		// The first line began before our window; it is not a whole record.
		lines = lines[1:]
	}
	events := make([]Event, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		events = append(events, event)
	}
	return events, truncated, nil
}

// parsePresenceTime accepts the unix-nanosecond strings the work log and lease
// records carry, and falls back to RFC3339 so a future writer that switches
// representation does not silently drop every actor on the floor.
func parsePresenceTime(value string) (time.Time, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, false
	}
	if nanos, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		if nanos <= 0 {
			return time.Time{}, false
		}
		return time.Unix(0, nanos), true
	}
	if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return parsed, true
	}
	return time.Time{}, false
}

func presenceTransition(event Event) string {
	from := strings.TrimSpace(event.From)
	if from == "" {
		from = "(new)"
	}
	to := strings.TrimSpace(event.To)
	if to == "" {
		return from
	}
	return from + "->" + to
}

func sortedPresenceKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		if strings.TrimSpace(key) == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func latestPresenceObservations(observations []PresenceObservation, max int) []PresenceObservation {
	sort.Slice(observations, func(i, j int) bool {
		return observations[i].At > observations[j].At
	})
	if len(observations) > max {
		observations = observations[:max]
	}
	return observations
}

func appendPresenceIntegrity(existing []string, note string) []string {
	for _, item := range existing {
		if item == note {
			return existing
		}
	}
	return append(existing, note)
}
