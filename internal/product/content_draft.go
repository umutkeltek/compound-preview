package product

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/home"
)

// content_draft.go owns the distribution-loop ContentDraft: an observed-class
// candidate that pairs a content-addressed post body with the product, platform,
// voice pack, and deriving evidence it came from — plus an EXPIRY, because stale
// marketing must never post. It lives in the product package because a draft is a
// product-scoped market-control artifact, not a repo summary; persistence and the
// habitat entry are borrowed from the candidates leaf (one home per kind), and the
// body is custodied in the shared content-addressed ArtifactStore (dedup for free).
//
// The door verbs are record / list / park / operationalize / reject. Three
// invariants are load-bearing and each has a red-capable falsifier in the tests:
//   - a rejected draft is RETAINED on disk with state recorded (keep-all-data law);
//     rejection exhaust trains voice later, so the file is never deleted.
//   - an EXPIRED draft REFUSES operationalize with a clear error but stays stored
//     and listable.
//   - nothing self-promotes: record leaves a draft "proposed"; only an explicit
//     verb moves it to parked / operationalized / rejected.

const ContentDraftSchemaVersion = "compound.content_draft.v0.1"

// ContentDraft lifecycle states. Born "proposed"; only a door verb moves it.
const (
	ContentDraftStateProposed        = "proposed"
	ContentDraftStateParked          = "parked"
	ContentDraftStateOperationalized = "operationalized"
	ContentDraftStateRejected        = "rejected"
	// ContentDraftStatePosted is terminal and means a remote platform ACCEPTED
	// this draft and returned an id for it. It is deliberately not reachable by
	// adjudication: "approved for use" (operationalized) and "a human outside
	// this machine can see it" (posted) are different facts, and a distribution
	// measure built on the first can report success forever while nothing ships.
	ContentDraftStatePosted = "posted"
	// ContentDraftStateScheduled means a transport's scheduler ACCEPTED the
	// publication request and returned its own reservation id. It is NOT a
	// publication: "the scheduler holds the job" and "a platform published it"
	// are different facts, and recording an acceptance as posted was the first
	// audited distribution gap (2026-09-01). Reached only through
	// MarkContentDraftScheduled; the posted door is the only way out.
	ContentDraftStateScheduled = "scheduled"
)

// contentDraftMediaKinds is the closed allowlist of attachable asset kinds, for
// the same reason contentDraftPlatforms is closed: a junk kind a transport cannot
// route must be refused at the door, not discovered at post time.
var contentDraftMediaKinds = map[string]bool{
	"image": true,
	"video": true,
}

// contentDraftPlatforms is the closed allowlist of distribution targets and the
// ONE owner of these names. An observed candidate must not carry a junk platform
// string a downstream transport cannot route, so the set is validated, not
// free-form. Extending distribution to a new platform is one edit here —
// deliberately a decision, not an accident.
//
// tiktok is present even though its developer terms refuse a third-party upload
// tool for accounts you manage: such a draft is written, approved, posted BY HAND,
// and then marked posted with the real post id. Only the transport differs;
// excluding the platform would lose the record of a post that actually happened.
var contentDraftPlatforms = map[string]bool{
	"x":         true,
	"bluesky":   true,
	"threads":   true,
	"instagram": true,
	"tiktok":    true,
	"facebook":  true,
}

// contentDraftPlatformAliases maps other names for a platform onto its canonical
// one. "twitter" and "x" are one destination; storing both would split every
// per-platform count, account lookup, and transport route in half. Callers may say
// either — the stored value is always canonical.
var contentDraftPlatformAliases = map[string]string{
	"twitter": "x",
}

// ContentDraftPlatforms returns the canonical platform names, sorted. The refusal
// message and the CLI usage string derive from this rather than restating it: the
// names used to live in three places, two of them literals, so widening the set
// would have shipped six platforms while still telling the operator there were
// three.
func ContentDraftPlatforms() []string {
	out := make([]string, 0, len(contentDraftPlatforms))
	for name := range contentDraftPlatforms {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// canonicalContentDraftPlatform lowercases, trims, and resolves an alias. It
// returns false when the result is not an allowed destination.
func canonicalContentDraftPlatform(platform string) (string, bool) {
	name := strings.ToLower(strings.TrimSpace(platform))
	if canonical, ok := contentDraftPlatformAliases[name]; ok {
		name = canonical
	}
	return name, contentDraftPlatforms[name]
}

// ContentDraftInput is the raw material for a record intake.
type ContentDraftInput struct {
	ProductID    string
	Platform     string
	Body         string // raw post text; stored content-addressed, hashed for BodyRef
	VoicePackRef string
	EvidenceRefs []string
	ExpiresAt    time.Time
	// Media are generated assets to attach. Raw bytes in, content-addressed refs
	// out — the same custody path the body already takes.
	Media []ContentDraftMediaInput
}

// ContentDraftMediaInput is one generated asset offered to a draft, with the
// provenance of the generator that produced it. Provenance is not decoration: an
// asset whose adapter/model/prompt is unknown cannot be reproduced, compared, or
// attributed when a variant outperforms another.
type ContentDraftMediaInput struct {
	Kind     string // image | video
	Bytes    []byte // raw asset bytes; custodied content-addressed
	MIMEType string
	AltText  string
	Adapter  string // which generator produced it, e.g. "adapter:local-sdxl"
	Model    string // model / LoRA identity, e.g. "sdxl-1.0+lora:product-style-v3"
	Prompt   string // the prompt itself; custodied content-addressed as PromptRef
	// Params are the generation parameters (seed, steps, size). They are persisted
	// because an asset whose seed was sent to the generator but never recorded
	// cannot be reproduced from its own record — the reproducibility claim outran
	// the data until this was added.
	Params map[string]string
}

// ContentDraftMedia is one attached asset as persisted on the draft.
type ContentDraftMedia struct {
	Kind      string            `json:"kind"`
	Ref       string            `json:"ref"`
	MIMEType  string            `json:"mime_type,omitempty"`
	AltText   string            `json:"alt_text,omitempty"`
	Adapter   string            `json:"adapter,omitempty"`
	Model     string            `json:"model,omitempty"`
	PromptRef string            `json:"prompt_ref,omitempty"`
	Params    map[string]string `json:"params,omitempty"`
}

// ContentDraftPostInput is the platform's own answer after a successful post.
type ContentDraftPostInput struct {
	RemoteID   string
	URL        string
	ReceiptRef string
}

// ContentDraftPost is the evidence that a draft actually reached a platform.
// RemoteID is required by MarkContentDraftPosted precisely because a "posted"
// flag that can be set without it is a claim, not a fact.
type ContentDraftPost struct {
	Platform   string `json:"platform"`
	RemoteID   string `json:"remote_id"`
	URL        string `json:"url,omitempty"`
	PostedAt   string `json:"posted_at"`
	ReceiptRef string `json:"receipt_ref,omitempty"`
}

// ContentDraft is the persisted candidate record under inbox/content-drafts/<id>.json.
type ContentDraft struct {
	SchemaVersion string                  `json:"schema_version"`
	ID            string                  `json:"id"`
	Kind          string                  `json:"kind"`
	ProductID     string                  `json:"product_id"`
	Platform      string                  `json:"platform"`
	BodyRef       string                  `json:"body_ref"`
	VoicePackRef  string                  `json:"voice_pack_ref,omitempty"`
	EvidenceRefs  []string                `json:"evidence_refs,omitempty"`
	Media         []ContentDraftMedia     `json:"media,omitempty"`
	Approval      *ContentApproval        `json:"approval,omitempty"`
	Outbound      *ContentOutboundAttempt `json:"outbound,omitempty"`
	Post          *ContentDraftPost       `json:"post,omitempty"`
	// Schedule is the transport scheduler's own acceptance record, set only
	// when a scheduler took the publication job. Distinct from Post on
	// purpose: a scheduled draft has no remote post id yet, and the posted
	// door is the only writer of Post.
	Schedule      *ContentDraftSchedule `json:"schedule,omitempty"`
	State         string                `json:"state"`
	CreatedAt     string                `json:"created_at"`
	ExpiresAt     string                `json:"expires_at"`
	AdjudicatedAt string                `json:"adjudicated_at,omitempty"`
	// Warnings is the additive set of content-staleness flags. Slice 2 of
	// the model-catalog envelope adds a check: a body naming a model the
	// installed catalog marks superseded gets a visible flag here. The
	// flag is INFORMATIONAL — the operator still owns the post; the gate
	// never refuses a draft. Empty when the installed catalog has no
	// superseded models, or when the body names no models at all.
	Warnings []string `json:"warnings,omitempty"`
}

// RecordContentDraft intakes a new draft: it custodies the body content-addressed
// (identical bodies dedup to one artifact ref) and mints exactly one observed-class
// candidate in the "proposed" state. It never operationalizes — nothing here
// promotes a draft on its own.
func RecordContentDraft(homePath string, in ContentDraftInput, now time.Time) (ContentDraft, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return ContentDraft{}, fmt.Errorf("COMPOUND_HOME required")
	}
	productID := strings.TrimSpace(in.ProductID)
	if productID == "" {
		return ContentDraft{}, fmt.Errorf("product_id required")
	}
	platform, ok := canonicalContentDraftPlatform(in.Platform)
	if !ok {
		return ContentDraft{}, fmt.Errorf("platform %q not in allowlist (%s)", in.Platform, strings.Join(ContentDraftPlatforms(), "|"))
	}
	if strings.TrimSpace(in.Body) == "" {
		return ContentDraft{}, fmt.Errorf("draft body required")
	}
	if in.ExpiresAt.IsZero() {
		return ContentDraft{}, fmt.Errorf("expiry timestamp required: stale marketing must never post")
	}
	store, err := home.NewArtifactStore(homePath)
	if err != nil {
		return ContentDraft{}, err
	}
	bodyRef, err := store.Put([]byte(in.Body))
	if err != nil {
		return ContentDraft{}, err
	}
	media, err := custodyContentDraftMedia(store, in.Media)
	if err != nil {
		return ContentDraft{}, err
	}
	createdAt := now.UTC()
	// Fold the create-nanosecond into the id so two records of the SAME body are
	// distinct drafts (distinct ids) that share one content-addressed body ref.
	id := stableID("cdraft_", productID, platform, bodyRef, strconv.FormatInt(createdAt.UnixNano(), 10))
	draft := ContentDraft{
		SchemaVersion: ContentDraftSchemaVersion,
		ID:            id,
		Kind:          string(candidates.ContentDraft),
		ProductID:     productID,
		Platform:      platform,
		BodyRef:       bodyRef,
		Media:         media,
		VoicePackRef:  strings.TrimSpace(in.VoicePackRef),
		EvidenceRefs:  compact(in.EvidenceRefs),
		State:         ContentDraftStateProposed,
		CreatedAt:     createdAt.Format(time.RFC3339),
		ExpiresAt:     in.ExpiresAt.UTC().Format(time.RFC3339),
		// Content-staleness gate: surface a visible flag when the body
		// names a model the installed catalog marks superseded. Slice 2 of
		// the model-catalog envelope — read-only, no refusal, no
		// mutation; the operator still owns the post.
		Warnings: modelCatalogStalenessWarnings(homePath, in.Body),
	}
	if err := candidates.WriteCandidate(homePath, candidates.ContentDraft, id, &draft); err != nil {
		return ContentDraft{}, err
	}
	return draft, nil
}

// ReadContentDraft loads one draft by id from its canonical inbox path.
func ReadContentDraft(homePath, id string) (ContentDraft, error) {
	homePath = strings.TrimSpace(homePath)
	id = strings.TrimSpace(id)
	var draft ContentDraft
	if err := candidates.ReadCandidate(homePath, candidates.ContentDraft, id, &draft); err != nil {
		return ContentDraft{}, err
	}
	if draft.ID == "" {
		draft.ID = id
	}
	return draft, nil
}

// ListContentDrafts returns every draft, oldest first, optionally scoped to one
// product. Expired and rejected drafts are INCLUDED — both remain listable by
// design (an expired draft stays stored; a rejected one is retained exhaust).
func ListContentDrafts(homePath, productFilter string) ([]ContentDraft, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return nil, fmt.Errorf("COMPOUND_HOME required")
	}
	dir := filepath.Join(homePath, candidates.InboxDir(candidates.ContentDraft))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	productFilter = strings.TrimSpace(productFilter)
	var drafts []ContentDraft
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		draft, err := ReadContentDraft(homePath, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		if productFilter != "" && draft.ProductID != productFilter {
			continue
		}
		drafts = append(drafts, draft)
	}
	sort.Slice(drafts, func(i, j int) bool {
		if drafts[i].CreatedAt == drafts[j].CreatedAt {
			return drafts[i].ID < drafts[j].ID
		}
		return drafts[i].CreatedAt < drafts[j].CreatedAt
	})
	return drafts, nil
}

// ParkContentDraft records the "parked" verdict in place.
func ParkContentDraft(homePath, id string, now time.Time) (ContentDraft, error) {
	return adjudicateContentDraft(homePath, id, ContentDraftStateParked, now, false)
}

// OperationalizeContentDraft records the "operationalized" verdict — but REFUSES,
// without touching the stored record, if the draft has expired.
func OperationalizeContentDraft(homePath, id string, now time.Time) (ContentDraft, error) {
	return adjudicateContentDraft(homePath, id, ContentDraftStateOperationalized, now, true)
}

// custodyContentDraftMedia validates each offered asset and stores its bytes (and
// its prompt) content-addressed, so identical assets dedup to one artifact exactly
// as identical bodies already do.
func custodyContentDraftMedia(store *home.ArtifactStore, in []ContentDraftMediaInput) ([]ContentDraftMedia, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]ContentDraftMedia, 0, len(in))
	for _, m := range in {
		kind := strings.ToLower(strings.TrimSpace(m.Kind))
		if !contentDraftMediaKinds[kind] {
			return nil, fmt.Errorf("media kind %q not in allowlist (image|video)", m.Kind)
		}
		if len(m.Bytes) == 0 {
			return nil, fmt.Errorf("media kind %q carries no bytes: a draft must not claim an asset it does not hold", kind)
		}
		ref, err := store.Put(m.Bytes)
		if err != nil {
			return nil, err
		}
		entry := ContentDraftMedia{
			Kind:     kind,
			Ref:      ref,
			MIMEType: strings.TrimSpace(m.MIMEType),
			AltText:  strings.TrimSpace(m.AltText),
			Adapter:  strings.TrimSpace(m.Adapter),
			Model:    strings.TrimSpace(m.Model),
			Params:   m.Params,
		}
		if prompt := strings.TrimSpace(m.Prompt); prompt != "" {
			promptRef, err := store.Put([]byte(prompt))
			if err != nil {
				return nil, err
			}
			entry.PromptRef = promptRef
		}
		out = append(out, entry)
	}
	return out, nil
}

// ContentDraftScheduleInput is the operator-transport flow's schedule
// acceptance: the scheduler's own reservation id, the accepted publish time,
// and the governed receipt that carried the act.
type ContentDraftScheduleInput struct {
	ScheduleID  string
	ScheduledAt string // RFC3339; empty means the ack named no publish time
	ReceiptRef  string
}

// ContentDraftSchedule is a scheduler's own acceptance, persisted on the
// draft. It is distinct from ContentDraftPost because "the transport's
// scheduler took the job" and "a platform published the draft" are different
// facts (2026-09-01 audit gap; state + door landed 2026-09-15).
type ContentDraftSchedule struct {
	Platform    string `json:"platform"`
	ScheduleID  string `json:"schedule_id"`
	ScheduledAt string `json:"scheduled_at,omitempty"`
	AckedAt     string `json:"acked_at"`
	ReceiptRef  string `json:"receipt_ref,omitempty"`
}

// MarkContentDraftScheduled records that a transport's scheduler accepted the
// publication request and returned its own reservation id. This is the ONLY
// door to the "scheduled" state, and it is a different door from posted on
// purpose:
//
//   - it refuses any draft that is not "operationalized", so unreviewed
//     content cannot reach a transport's queue through this path;
//   - it refuses an empty ScheduleID — a scheduler acceptance without the
//     scheduler's own reservation id cannot be observed, cancelled, or
//     de-duplicated, so it is not a fact this ledger should record;
//   - it refuses an already-scheduled draft, because overwriting the first
//     reservation id would hide an accepted job; a reschedule needs an
//     explicit withdraw (not built in v0.1), not a silent replace;
//   - it refuses an already-posted draft, because a public post is terminal.
func MarkContentDraftScheduled(homePath, id string, in ContentDraftScheduleInput, now time.Time) (ContentDraft, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return ContentDraft{}, fmt.Errorf("COMPOUND_HOME required")
	}
	scheduleID := strings.TrimSpace(in.ScheduleID)
	if scheduleID == "" {
		return ContentDraft{}, fmt.Errorf("schedule reservation id required: a scheduler ack without it is a claim, not an observable fact")
	}
	if scheduledAt := strings.TrimSpace(in.ScheduledAt); scheduledAt != "" {
		if _, err := time.Parse(time.RFC3339, scheduledAt); err != nil {
			return ContentDraft{}, fmt.Errorf("schedule acceptance publish time %q is not RFC3339: %w", scheduledAt, err)
		}
	}
	record, draft, err := readContentOutbound(homePath, id)
	if err != nil {
		return ContentDraft{}, err
	}
	if draft.Outbound != nil {
		return ContentDraft{}, fmt.Errorf("draft has an outbound attempt; use typed reconciliation")
	}
	if draft.State == ContentDraftStatePosted {
		return ContentDraft{}, fmt.Errorf("draft %s is already posted as %q: a public post is terminal", draft.ID, draft.Post.RemoteID)
	}
	if draft.State == ContentDraftStateScheduled {
		return ContentDraft{}, fmt.Errorf("draft %s is already scheduled as %q: rescheduling requires withdrawing the accepted reservation first (no withdraw door in v0.1)", draft.ID, draft.Schedule.ScheduleID)
	}
	if draft.State != ContentDraftStateOperationalized {
		return ContentDraft{}, fmt.Errorf("draft %s is %q: only an operationalized draft may be scheduled", draft.ID, draft.State)
	}
	draft.State = ContentDraftStateScheduled
	draft.AdjudicatedAt = now.UTC().Format(time.RFC3339)
	draft.Schedule = &ContentDraftSchedule{
		Platform:    draft.Platform,
		ScheduleID:  scheduleID,
		ScheduledAt: strings.TrimSpace(in.ScheduledAt),
		AckedAt:     now.UTC().Format(time.RFC3339),
		ReceiptRef:  strings.TrimSpace(in.ReceiptRef),
	}
	replaced, err := candidates.ReplaceCandidateIfCurrent(homePath, record, draft)
	if err != nil {
		return ContentDraft{}, err
	}
	if !replaced.Replaced {
		return ContentDraft{}, fmt.Errorf("draft generation changed; retry from current state")
	}
	return draft, nil
}

// MarkContentDraftPosted records that a platform accepted this draft and returned
// an id for it. This is the ONLY door to the terminal "posted" state, and it is
// separate from adjudication on purpose:
//
//   - it refuses any draft that is not "operationalized" or "scheduled", so
//     unreviewed generated content cannot reach the public through this path;
//   - it refuses an empty RemoteID, because a posted record without the platform's
//     own answer is indistinguishable from a draft that never went out — the exact
//     false-green that let the weekly driver report success while nothing shipped;
//   - it refuses an already-posted draft, because overwriting the first remote id
//     would hide a post that is already public.
//
// A scheduled draft reaching posted is the normal flow: the scheduler accepted
// at request time, the platform published at its own time, and the real remote
// id arrives later. The schedule record is kept (it is the provenance of the
// reservation) while Post carries the publication.
func MarkContentDraftPosted(homePath, id string, in ContentDraftPostInput, now time.Time) (ContentDraft, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return ContentDraft{}, fmt.Errorf("COMPOUND_HOME required")
	}
	remoteID := strings.TrimSpace(in.RemoteID)
	if remoteID == "" {
		return ContentDraft{}, fmt.Errorf("platform remote id required: posted without it is a claim, not a fact")
	}
	record, draft, err := readContentOutbound(homePath, id)
	if err != nil {
		return ContentDraft{}, err
	}
	if draft.Outbound != nil {
		return ContentDraft{}, fmt.Errorf("draft has an outbound attempt; use typed reconciliation")
	}
	if draft.State == ContentDraftStatePosted {
		return ContentDraft{}, fmt.Errorf("draft %s is already posted as %q: a public post is terminal", draft.ID, draft.Post.RemoteID)
	}
	if draft.State != ContentDraftStateOperationalized && draft.State != ContentDraftStateScheduled {
		return ContentDraft{}, fmt.Errorf("draft %s is %q: only an operationalized or scheduled draft may be posted", draft.ID, draft.State)
	}
	draft.State = ContentDraftStatePosted
	draft.AdjudicatedAt = now.UTC().Format(time.RFC3339)
	draft.Post = &ContentDraftPost{
		Platform:   draft.Platform,
		RemoteID:   remoteID,
		URL:        strings.TrimSpace(in.URL),
		PostedAt:   now.UTC().Format(time.RFC3339),
		ReceiptRef: strings.TrimSpace(in.ReceiptRef),
	}
	replaced, err := candidates.ReplaceCandidateIfCurrent(homePath, record, draft)
	if err != nil {
		return ContentDraft{}, err
	}
	if !replaced.Replaced {
		return ContentDraft{}, fmt.Errorf("draft generation changed; retry from current state")
	}
	return draft, nil
}

// RejectContentDraft records the "rejected" verdict in place. The file is NEVER
// deleted (keep-all-data law): rejection exhaust trains voice later.
func RejectContentDraft(homePath, id string, now time.Time) (ContentDraft, error) {
	return adjudicateContentDraft(homePath, id, ContentDraftStateRejected, now, false)
}

// adjudicateContentDraft is the shared read → (expiry gate) → rewrite-in-place path.
// When requireFresh is set it returns an error BEFORE any write if the draft is
// expired, so the stored record and its listability are untouched by a refusal.
func adjudicateContentDraft(homePath, id, newState string, now time.Time, requireFresh bool) (ContentDraft, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return ContentDraft{}, fmt.Errorf("COMPOUND_HOME required")
	}
	record, draft, err := readContentOutbound(homePath, id)
	if err != nil {
		return ContentDraft{}, err
	}
	if draft.Outbound != nil || (draft.Approval != nil && newState != ContentDraftStateRejected) {
		return ContentDraft{}, fmt.Errorf("draft has a frozen approval or outbound owner; use its recovery command")
	}
	// A posted draft is public. Re-adjudicating it (park/reject/re-approve) would
	// make the ledger disagree with the world, and the world is the one we cannot
	// edit. Posted is terminal to every verb, including this one.
	if draft.State == ContentDraftStatePosted {
		return ContentDraft{}, fmt.Errorf("draft %s is already posted: a public post cannot be re-adjudicated to %q", draft.ID, newState)
	}
	// A scheduled draft holds an accepted transport reservation. Re-adjudicating
	// it here would make the ledger claim a state the transport still intends to
	// publish; the reservation must be withdrawn (no withdraw door in v0.1)
	// before the local verdict may move. Fail loud, name the reservation.
	if draft.State == ContentDraftStateScheduled {
		return ContentDraft{}, fmt.Errorf("draft %s is scheduled as %q: withdraw the transport reservation before re-adjudicating to %q", draft.ID, draft.Schedule.ScheduleID, newState)
	}
	if requireFresh {
		expiresAt, perr := time.Parse(time.RFC3339, draft.ExpiresAt)
		if perr != nil {
			return ContentDraft{}, fmt.Errorf("draft %s has unparseable expiry %q: %w", draft.ID, draft.ExpiresAt, perr)
		}
		if !now.Before(expiresAt) {
			return ContentDraft{}, fmt.Errorf("draft %s expired at %s: stale marketing must never post", draft.ID, draft.ExpiresAt)
		}
	}
	draft.State = newState
	draft.AdjudicatedAt = now.UTC().Format(time.RFC3339)
	replaced, err := candidates.ReplaceCandidateIfCurrent(homePath, record, draft)
	if err != nil {
		return ContentDraft{}, err
	}
	if !replaced.Replaced {
		return ContentDraft{}, fmt.Errorf("draft generation changed; retry from current state")
	}
	return draft, nil
}
