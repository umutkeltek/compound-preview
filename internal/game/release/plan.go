// Package release produces a ProductReleasePlan for a parked
// KindProductHypothesis idea candidate. The plan is governed end-to-end:
// the surface never ships a Steamworks API call, never uploads a Unity
// build, and never promotes a candidate. The operator must run
// `compound product release-review <review.yaml>` to authorize the plan
// before anything happens downstream.
package release

import (
	"context"
	"fmt"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/game"
	"compound/internal/mem"
	"compound/internal/product"
)

// PlanInput is the small set of fields the CLI collects from flags + the
// parked idea candidate. All fields are required (the underlying
// product.BuildReleasePlan enforces this and returns an error otherwise).
type PlanInput struct {
	HomePath          string // COMPOUND_HOME; resolved via home.Resolve upstream
	StoreOpener       func(homePath string) (*mem.Store, error)
	ProductID         string // e.g. "game:idea:roguelike-deckbuilder"
	ActionCardID      string // optional; empty means no specific action card
	IdeaCandidate     string // candidate id from `compound game draft`
	UnityArtifactRef  string // optional; empty if Unity build not yet packaged
	ChangeSummary     string // one-liner for the review queue
	Rollback          string // named rollback plan (free text)
	PostReleaseMetric string // e.g. "steam:wishlist_delta" — what we measure
}

// PlanResult is the producer output. The CLI prints PlanID + the
// on-disk paths so the operator can run the next review command.
type PlanResult struct {
	Schema     string                          `json:"schema"`
	PlanID     string                          `json:"plan_id"`
	Paths      product.ProductReleasePlanPaths `json:"paths"`
	NextAction string                          `json:"next_action"`
}

const (
	planResultSchema = "compound/game-release-plan-result.v0.1"
	defaultReviewCmd = "compound product release-review"
)

// Build produces and records a ProductReleasePlan. Fail-closed: if the
// store can't open, the input is invalid, or recording fails, no plan
// file is written and the error is returned.
//
// The StoreOpener is injected so the release package does not require a
// direct dep on internal/mem's specific constructor. Production wires
// it from cmd/compound; tests inject a stub store.
func Build(ctx context.Context, in PlanInput, now time.Time) (PlanResult, error) {
	if strings.TrimSpace(in.HomePath) == "" {
		return PlanResult{}, fmt.Errorf("release.Build: HomePath required")
	}
	if in.StoreOpener == nil {
		return PlanResult{}, fmt.Errorf("release.Build: StoreOpener required")
	}
	if strings.TrimSpace(in.ProductID) == "" {
		return PlanResult{}, fmt.Errorf("release.Build: ProductID required")
	}
	if strings.TrimSpace(in.IdeaCandidate) == "" {
		return PlanResult{}, fmt.Errorf("release.Build: IdeaCandidate required")
	}
	if strings.TrimSpace(in.ChangeSummary) == "" {
		return PlanResult{}, fmt.Errorf("release.Build: ChangeSummary required")
	}
	if strings.TrimSpace(in.Rollback) == "" {
		return PlanResult{}, fmt.Errorf("release.Build: Rollback required")
	}
	if strings.TrimSpace(in.PostReleaseMetric) == "" {
		return PlanResult{}, fmt.Errorf("release.Build: PostReleaseMetric required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	store, err := in.StoreOpener(in.HomePath)
	if err != nil {
		return PlanResult{}, fmt.Errorf("release.Build: open store: %w", err)
	}
	if store == nil {
		return PlanResult{}, fmt.Errorf("release.Build: store opener returned nil store")
	}

	candidateRefs := []string{in.IdeaCandidate}
	if in.UnityArtifactRef != "" {
		candidateRefs = append(candidateRefs, in.UnityArtifactRef)
	}

	plan, err := product.BuildReleasePlan(in.HomePath, store, product.ReleasePlanInput{
		ProductID:         in.ProductID,
		ActionCardID:      in.ActionCardID,
		ChangeSummary:     in.ChangeSummary,
		CandidateRefs:     candidateRefs,
		Rollback:          in.Rollback,
		PostReleaseMetric: in.PostReleaseMetric,
		EvidenceRefs:      []string{"game_idea:" + in.IdeaCandidate},
	}, now)
	if err != nil {
		return PlanResult{}, fmt.Errorf("release.Build: build plan: %w", err)
	}

	paths, err := product.RecordReleasePlan(in.HomePath, plan)
	if err != nil {
		return PlanResult{}, fmt.Errorf("release.Build: record plan: %w", err)
	}

	_ = ctx                          // reserved for future cancellation hook
	_ = candidates.ProductHypothesis // imported transitively; pins dependency
	_ = game.SchemaGameReleasePlan   // pins the schema constant

	return PlanResult{
		Schema:     planResultSchema,
		PlanID:     plan.ID,
		Paths:      paths,
		NextAction: fmt.Sprintf("%s %s", defaultReviewCmd, paths.ReviewYAMLPath),
	}, nil
}
