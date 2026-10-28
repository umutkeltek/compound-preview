package actuator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/product"
)

// SignalCheckSchemaVersion identifies a pending signal-check record.
const SignalCheckSchemaVersion = "compound.actuator_signal_check.v0.1"

// ObservationInput is what the actuator needs to close the loop after an
// executed move: what happened (Description), what it should move (Metric +
// ExpectedEffect), how long to watch (Window), and the proof that the move
// actually landed (EvidenceRefs — the post's receipt/transition/artifact).
type ObservationInput struct {
	ProductID        string
	WorkID           string
	Channel          string
	Description      string
	ExpectedEffect   string
	Metric           string
	Window           string
	ObservationUntil time.Time
	EvidenceRefs     []string
}

// PendingSignalCheck is the scheduled "go look at metric M by time T" record the
// actuator leaves after a move, so the observation is not forgotten. It names
// the minted hypothesis so a later signal can be joined back to the move.
type PendingSignalCheck struct {
	SchemaVersion    string   `json:"schema_version"`
	ID               string   `json:"id"`
	ProductID        string   `json:"product_id"`
	Channel          string   `json:"channel,omitempty"`
	WorkID           string   `json:"work_id,omitempty"`
	Metric           string   `json:"metric"`
	ChangeEvent      string   `json:"change_event"`
	HypothesisID     string   `json:"hypothesis_id"`
	ObservationUntil string   `json:"observation_until"`
	EvidenceRefs     []string `json:"evidence_refs,omitempty"`
	CreatedAt        string   `json:"created_at"`
}

// MintObservation closes the feedback loop for one executed move: it mints an
// OPEN ProductHypothesis candidate — the exact object internal/product's loop
// model already reads — so the move is immediately "a hypothesis waiting for a
// signal". Because the hypothesis carries the metric + change event + the move's
// receipt as evidence, the product loop's own matching (loopHypothesisHasSignal)
// will join a later ProductSignal to it with no human glue: the loop closes
// mechanically. The hypothesis is minted open+parked (Result=unknown,
// Verdict=park), matching the shape isOpenLoopHypothesis treats as open.
//
// Idempotent: WriteCandidate refuses to overwrite an existing candidate, so
// re-running the same move does not duplicate the hypothesis.
func MintObservation(homePath string, in ObservationInput, now time.Time) (string, error) {
	if strings.TrimSpace(in.ProductID) == "" {
		return "", fmt.Errorf("actuator observation: product_id required")
	}
	if strings.TrimSpace(in.Metric) == "" {
		return "", fmt.Errorf("actuator observation: metric required (the loop needs a signal to match)")
	}
	window := firstNonEmpty(in.Window, "7d")
	id := observationID(in)
	hyp := product.HypothesisCandidate{
		SchemaVersion:     product.ProductHypothesisCandidate,
		ID:                id,
		Kind:              string(candidates.ProductHypothesis),
		ProductID:         in.ProductID,
		ChangeEvent:       in.Description,
		ExpectedEffect:    firstNonEmpty(in.ExpectedEffect, fmt.Sprintf("%s moves in response to the %s move", in.Metric, firstNonEmpty(in.Channel, "executed"))),
		Metrics:           []string{in.Metric},
		ObservationWindow: window,
		Result:            "unknown",
		NextAction:        fmt.Sprintf("observe %s within %s; review against the %s move that landed %s", in.Metric, window, firstNonEmpty(in.Channel, "executed"), in.WorkID),
		Verdict:           candidates.Park,
		EvidenceRefs:      compactRefs(in.EvidenceRefs),
		CreatedAt:         now.UTC().Format(time.RFC3339),
	}
	if err := candidates.WriteCandidate(homePath, candidates.ProductHypothesis, id, hyp); err != nil {
		return "", fmt.Errorf("actuator observation: mint hypothesis: %w", err)
	}

	// Schedule the signal check so the observation has a deadline, not just a wish.
	check := PendingSignalCheck{
		SchemaVersion:    SignalCheckSchemaVersion,
		ID:               "sigcheck_" + id,
		ProductID:        in.ProductID,
		Channel:          in.Channel,
		WorkID:           in.WorkID,
		Metric:           in.Metric,
		ChangeEvent:      in.Description,
		HypothesisID:     id,
		ObservationUntil: in.ObservationUntil.UTC().Format(time.RFC3339),
		EvidenceRefs:     compactRefs(in.EvidenceRefs),
		CreatedAt:        now.UTC().Format(time.RFC3339),
	}
	if err := writeSignalCheck(homePath, check); err != nil {
		return id, err
	}
	return id, nil
}

func writeSignalCheck(homePath string, c PendingSignalCheck) error {
	dir := filepath.Join(homePath, "actuator", "signal-checks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, c.ID+".json"), append(data, '\n'), 0o644)
}

// ListPendingSignalChecks returns all scheduled signal checks for a home,
// optionally filtered to a product. This is the actuator's "what am I waiting to
// measure" surface.
func ListPendingSignalChecks(homePath, productID string) ([]PendingSignalCheck, error) {
	dir := filepath.Join(homePath, "actuator", "signal-checks")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []PendingSignalCheck
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var c PendingSignalCheck
		if err := json.Unmarshal(data, &c); err != nil {
			continue
		}
		if productID != "" && c.ProductID != productID {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// observationID derives a stable hyp_ id from the move's identity, so re-running
// the same move maps to the same hypothesis (idempotent mint) and a signal can be
// joined back to the exact move.
func observationID(in ObservationInput) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{in.ProductID, in.WorkID, in.Channel, in.Description, in.Metric}, "\x00")))
	return "hyp_" + hex.EncodeToString(sum[:])[:16]
}

func compactRefs(refs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range refs {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// ObservationStatus is the closure verdict for one minted observation: is the
// hypothesis still OPEN (waiting for the world's signal), has a matching
// ProductSignal arrived (SIGNAL_RECEIVED — the loop closed), or did the
// observation window pass with no signal (EXPIRED — the move never got confirmed,
// surfaced as debt instead of silently forgotten)? When a signal matched,
// Before/After carry the observed delta so the outcome is visible without a human
// joining the two records.
type ObservationStatus struct {
	HypothesisID     string  `json:"hypothesis_id"`
	ProductID        string  `json:"product_id"`
	Channel          string  `json:"channel,omitempty"`
	ChangeEvent      string  `json:"change_event"`
	Metric           string  `json:"metric"`
	State            string  `json:"state"` // "open" | "signal_received" | "expired"
	MatchedSignalID  string  `json:"matched_signal_id,omitempty"`
	Before           float64 `json:"before,omitempty"`
	After            float64 `json:"after,omitempty"`
	ObservationUntil string  `json:"observation_until,omitempty"`
	Expired          bool    `json:"expired,omitempty"`
}

// ObserveClosure is the proof the loop compounds: it joins every actuator-minted
// observation (pending signal check) against the product's ingested signals using
// the SAME matching the product loop uses (metric or change-event). A matched
// signal flips the observation from open to signal_received — mechanically, no
// human glue. A check whose window passed without a signal flips to expired, so an
// unanswered move becomes visible debt rather than rotting silently. It matches
// against internal/product's signal ledger (the same signals the product loop
// reads), via the lightweight LoadProductSignals path so it stays fast on a large
// home.
func ObserveClosure(homePath, productID string, now time.Time) ([]ObservationStatus, error) {
	checks, err := ListPendingSignalChecks(homePath, productID)
	if err != nil {
		return nil, err
	}
	// The loop-closer matches checks against signals only — so read just the signal
	// ledger, not the full Timeline (which full-scans the event store and is slow on
	// a large home). Same matching the product loop uses, answered in milliseconds.
	signals, err := product.LoadProductSignals(homePath, productID)
	if err != nil {
		return nil, err
	}
	out := make([]ObservationStatus, 0, len(checks))
	for _, c := range checks {
		st := ObservationStatus{
			HypothesisID:     c.HypothesisID,
			ProductID:        c.ProductID,
			Channel:          c.Channel,
			ChangeEvent:      c.ChangeEvent,
			Metric:           c.Metric,
			State:            "open",
			ObservationUntil: c.ObservationUntil,
		}
		for _, sig := range signals {
			if signalMatchesCheck(sig, c) {
				st.State = "signal_received"
				st.MatchedSignalID = sig.ID
				st.Before = sig.Before
				st.After = sig.After
				break
			}
		}
		// A late signal still counts; only an unconfirmed move past its window
		// is expired. The actuator marks its OWN check expired — it never touches
		// the product's hypothesis verdict (that belongs to the product loop).
		if st.State == "open" && SignalCheckExpired(c, now) {
			st.State = "expired"
			st.Expired = true
		}
		out = append(out, st)
	}
	return out, nil
}

// SignalCheckExpired reports whether a pending signal check's observation window
// has passed without being satisfied. It is the actuator's "this measurement is
// overdue" test, used both to expire observations and to flag due/overdue checks
// on the observe surface.
func SignalCheckExpired(c PendingSignalCheck, now time.Time) bool {
	until, err := time.Parse(time.RFC3339, c.ObservationUntil)
	if err != nil {
		// An unparseable deadline is treated as expired so it surfaces, never hidden.
		return true
	}
	return now.UTC().After(until)
}

// signalMatchesCheck mirrors internal/product's loop matching: a signal joins an
// observation when its metric name OR change event normalizes to the same key.
func signalMatchesCheck(sig product.Signal, c PendingSignalCheck) bool {
	if c.Metric != "" && normalizeMatchKey(sig.MetricName) == normalizeMatchKey(c.Metric) {
		return true
	}
	if c.ChangeEvent != "" && strings.TrimSpace(sig.ChangeEvent) != "" &&
		normalizeMatchKey(sig.ChangeEvent) == normalizeMatchKey(c.ChangeEvent) {
		return true
	}
	return false
}

// normalizeMatchKey is the same normalization internal/product uses to join
// metrics and change events (lowercase; runs of non-alphanumerics -> one '_').
func normalizeMatchKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	prevUnderscore := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevUnderscore = false
			continue
		}
		if !prevUnderscore {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}
