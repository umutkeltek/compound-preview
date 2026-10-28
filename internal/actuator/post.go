package actuator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/act"
	"compound/internal/envelope"
	"compound/internal/policy"
)

// PostArtifactSchemaVersion identifies a staged distribution post.
const PostArtifactSchemaVersion = "compound.actuator_post_artifact.v0.1"

var recordPostConsumption = func(ledger *Ledger, consumption Consumption) error {
	return ledger.Record(consumption)
}

// PostArtifact is one staged distribution post. It is written to the outbox
// through the governed act seam (so it carries a receipt + transition), and it
// is the thing a later, separately-gated step would ship to a real channel. The
// actuator stages; it never authenticates an account or posts publicly — that
// stays an external_side_effect a human gates, per the outbound-distribution
// doctrine.
type PostArtifact struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	ProductID     string `json:"product_id"`
	Channel       string `json:"channel"`
	Surface       string `json:"surface"`
	Content       string `json:"content"`
	WorkID        string `json:"work_id"`
	CreatedAt     string `json:"created_at"`
}

// PostInput is the actuator's request to make one distribution move within limits.
type PostInput struct {
	WorkID         string
	ProductID      string
	Channel        string
	Surface        string
	Content        string
	Metric         string
	ExpectedEffect string
	Window         string
}

// PostResult is the outcome of a distribution move: the routing plan, the staged
// post + its receipt, and the minted observation that closes the loop. When the
// move was held (over budget), HeldMoveID/HeldMovePath point at the persisted
// human-gated action card explaining how to release it.
type PostResult struct {
	Plan          Plan   `json:"plan"`
	Executed      bool   `json:"executed"`
	Held          bool   `json:"held"`
	PostPath      string `json:"post_path,omitempty"`
	ReceiptRef    string `json:"receipt_ref,omitempty"`
	TransitionRef string `json:"transition_ref,omitempty"`
	HypothesisID  string `json:"hypothesis_id,omitempty"`
	SignalCheckID string `json:"signal_check_id,omitempty"`
	HeldMoveID    string `json:"held_move_id,omitempty"`
	HeldMovePath  string `json:"held_move_path,omitempty"`
}

// Post executes one distribution move end-to-end, within limits, and closes the
// feedback loop. The flow:
//
//  1. Build a local_reversible/easy ActionIntent for staging the post (outbox write).
//  2. EvaluatePlan: policy + the channel budget + Route. If not AUTO, hold with reasons.
//  3. AUTO: act.Execute(FileArtifactAdapter) stages the post artifact — capturing
//     the pre-image and emitting a Receipt + Transition (the proof it landed).
//  4. Record the move against the channel budget.
//  5. MintObservation: mint the open ProductHypothesis + schedule the signal check,
//     so a later ProductSignal on the metric joins back to this move and the loop
//     closes mechanically.
//
// This is the compounding unit: a move that spends budget, leaves a receipt, and
// opens a hypothesis the world's signal will later confirm or correct.
func Post(homePath string, in PostInput, cfg policy.Config, now time.Time) (PostResult, error) {
	if strings.TrimSpace(in.WorkID) == "" {
		return PostResult{}, fmt.Errorf("actuator post: work_id required (a move belongs to known work)")
	}
	if strings.TrimSpace(in.Content) == "" {
		return PostResult{}, fmt.Errorf("actuator post: content required")
	}
	if err := requireKnownWork(homePath, in.WorkID); err != nil {
		return PostResult{}, fmt.Errorf("actuator post: %w", err)
	}
	channel := firstNonEmpty(in.Channel, "distribution")
	surface := firstNonEmpty(in.Surface, "generic")
	window := firstNonEmpty(in.Window, "7d")
	postPath := outboxPath(homePath, in.ProductID, surface, in.Content, now)

	intent := postIntent(in, channel, surface, postPath, now)
	plan, err := EvaluatePlan(homePath, intent, channel, in.ProductID, cfg, now)
	var reservation Consumption
	if err == nil && plan.Decision.Mode == ModeAuto {
		plan, reservation, err = reservePlan(homePath, plan, now)
	}
	if err != nil {
		return PostResult{}, err
	}
	if plan.Decision.Mode != ModeAuto {
		// Held by policy or budget: no staging, no receipt, no observation (nothing
		// happened, so there is no loop to close). The hold is not a dead end —
		// persist a human-gated action card so the operator knows exactly how to
		// release the move.
		card := ReleaseGateFor(plan, now)
		path, err := WriteHeldMoveCard(homePath, card)
		if err != nil {
			return PostResult{}, fmt.Errorf("actuator post: persist held-move card: %w", err)
		}
		return PostResult{Plan: plan, Held: true, HeldMoveID: card.ID, HeldMovePath: path}, nil
	}

	// Stage the post through the governed seam — receipt + transition by construction.
	artifact := PostArtifact{
		SchemaVersion: PostArtifactSchemaVersion,
		ID:            "post_" + contentHash(in.Content, surface),
		ProductID:     in.ProductID,
		Channel:       channel,
		Surface:       surface,
		Content:       in.Content,
		WorkID:        in.WorkID,
		CreatedAt:     now.UTC().Format(time.RFC3339),
	}
	body, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return PostResult{}, err
	}
	store, err := act.NewArtifactStore(homePath)
	if err != nil {
		return PostResult{}, err
	}
	res, err := act.Execute(store, act.NewFileArtifactAdapter(), intent, plan.PolicyDecision, body, now)
	if err != nil {
		return PostResult{}, fmt.Errorf("actuator post: governed staging refused/rolled back: %w", err)
	}

	// Spend the channel budget for this move. Capture a record failure rather than
	// discarding it (F9.1, mirror of the run path): the post already staged + was
	// receipted, so budget accounting must not be silently wrong. Do NOT roll back —
	// the move happened; the feedback loop below still closes regardless.
	ledger := OpenLedger(homePath)
	recErr := recordPostConsumption(ledger, settledConsumption(reservation, res.Receipt.ID))

	// Close the loop: mint the observation hypothesis + schedule the signal check.
	// This runs regardless of recErr — the post DID land, so the loop must open.
	obsUntil := now.Add(windowDuration(window))
	hypID, err := MintObservation(homePath, ObservationInput{
		ProductID:        in.ProductID,
		WorkID:           in.WorkID,
		Channel:          channel,
		Description:      fmt.Sprintf("%s post on %s: %s", channel, surface, truncate(in.Content, 80)),
		ExpectedEffect:   in.ExpectedEffect,
		Metric:           firstNonEmpty(in.Metric, defaultMetricForChannel(channel)),
		Window:           window,
		ObservationUntil: obsUntil,
		EvidenceRefs:     []string{"receipt:" + res.Receipt.ID, "transition:" + res.Transition.ID, "post:" + artifact.ID, "work:" + in.WorkID},
	}, now)

	// The post staged + receipted; a lost budget record must surface (never roll
	// back the move). Build the F9.1 error once so it composes on either failure path.
	var recWrapped error
	if recErr != nil {
		recWrapped = fmt.Errorf("actuator post: post staged and receipted (%s) but settling budget consumption failed; reservation remains charged: %w", res.Receipt.ID, recErr)
	}

	if err != nil {
		// The loop failed to open. If the budget record ALSO failed, surface both.
		if recWrapped != nil {
			return PostResult{}, errors.Join(err, recWrapped)
		}
		return PostResult{}, err
	}

	result := PostResult{
		Plan:          plan,
		Executed:      true,
		PostPath:      postPath,
		ReceiptRef:    res.Receipt.ID,
		TransitionRef: res.Transition.ID,
		HypothesisID:  hypID,
		SignalCheckID: "sigcheck_" + hypID,
	}
	// Success path with a lost budget record: return the FULLY populated result
	// (the move happened, loop closed) together with the consumption-failure error.
	return result, recWrapped
}

func postIntent(in PostInput, channel, surface, postPath string, now time.Time) envelope.ActionIntent {
	target := envelope.ActionTarget{Kind: "distribution_post", ID: surface, Path: postPath}
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            intentIDForInvocation(now, in.WorkID, "distribution_post", channel, surface, in.Content),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "actuator", Name: "compound"},
			IDs:           envelope.IDs{WorkID: in.WorkID},
			Scope:         envelope.Scope{ProductID: in.ProductID, Surface: surface},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "actuator",
		Capability:    "distribution_post",
		Action:        "stage_post",
		Target:        target,
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       fmt.Sprintf("stage %s post on %s: %s", channel, surface, truncate(in.Content, 80)),
		RollbackPlan:  "restore the captured pre-image of the outbox file (removes the staged post)",
	}
}

// outboxPath is where staged posts land under COMPOUND_HOME: per-product, named
// by surface + content hash + timestamp so repeats never clobber.
func outboxPath(homePath, productID, surface, content string, now time.Time) string {
	product := firstNonEmpty(productID, "default")
	name := fmt.Sprintf("%s_%s_%s.json",
		now.UTC().Format("20060102T150405Z"),
		normalizeName(surface),
		contentHash(content, surface))
	return filepath.Join(homePath, "actuator", "outbox", normalizeName(product), name)
}

func contentHash(content, surface string) string {
	sum := sha256.Sum256([]byte(surface + "\x00" + content))
	return hex.EncodeToString(sum[:])[:10]
}

func normalizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "x"
	}
	return out
}

// defaultMetricForChannel picks a sensible signal metric to watch per channel, so
// a move always opens a matchable hypothesis even when the operator doesn't name one.
func defaultMetricForChannel(channel string) string {
	switch channel {
	case "distribution":
		return "posthog_signups"
	case "aso":
		return "app_store_connect_impressions"
	case "paid_marketing":
		return "ga4_installs"
	default:
		return "posthog_signups"
	}
}

// windowDuration parses a compact observation window like "7d", "48h", "2w".
func windowDuration(window string) time.Duration {
	window = strings.ToLower(strings.TrimSpace(window))
	if window == "" {
		return 7 * 24 * time.Hour
	}
	var n int
	unit := window[len(window)-1]
	if _, err := fmt.Sscanf(window[:len(window)-1], "%d", &n); err != nil || n <= 0 {
		return 7 * 24 * time.Hour
	}
	switch unit {
	case 'h':
		return time.Duration(n) * time.Hour
	case 'd':
		return time.Duration(n) * 24 * time.Hour
	case 'w':
		return time.Duration(n) * 7 * 24 * time.Hour
	default:
		return 7 * 24 * time.Hour
	}
}
