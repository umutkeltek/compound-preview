package release

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"compound/internal/mem"
	"compound/internal/product"
)

func stubStoreOpener(_ string) (*mem.Store, error) {
	// mem.NewStore() with a nil underlying memdb. product.BuildReleasePlan
	// only calls resolveReleaseIdentity which reads from a few claim
	// tables. For tests that pre-seed a ProductID, this returns an empty
	// store; BuildReleasePlan will surface "release identity not
	// configured" and the test will assert that path. For tests that
	// don't seed anything, the constructor returns an error.
	return nil, errors.New("stubStoreOpener: store not seeded; callers that need it must pre-seed product claims via mem")
}

func TestBuildRequiresAllInputs(t *testing.T) {
	now := time.Now().UTC()
	_, err := Build(context.Background(), PlanInput{HomePath: "/tmp", StoreOpener: stubStoreOpener}, now)
	if err == nil {
		t.Fatalf("expected error on empty inputs")
	}
}

func TestBuildRejectsEmptyHomePath(t *testing.T) {
	_, err := Build(context.Background(), PlanInput{
		StoreOpener: stubStoreOpener,
		ProductID:   "game:test",
	}, time.Now())
	if err == nil {
		t.Fatalf("expected error on empty HomePath")
	}
}

func TestBuildRejectsEmptyStoreOpener(t *testing.T) {
	_, err := Build(context.Background(), PlanInput{
		HomePath:  "/tmp",
		ProductID: "game:test",
	}, time.Now())
	if err == nil {
		t.Fatalf("expected error on nil StoreOpener")
	}
}

func TestBuildAllFieldsPresent(t *testing.T) {
	cases := []struct {
		name string
		mut  func(in *PlanInput)
		want string
	}{
		{"empty product id", func(in *PlanInput) {}, "ProductID"},
		{"empty idea candidate", func(in *PlanInput) { in.ProductID = "game:test" }, "IdeaCandidate"},
		{"empty change summary", func(in *PlanInput) {
			in.ProductID = "game:test"
			in.IdeaCandidate = "cand_x"
		}, "ChangeSummary"},
		{"empty rollback", func(in *PlanInput) {
			in.ProductID = "game:test"
			in.IdeaCandidate = "cand_x"
			in.ChangeSummary = "ship v1"
		}, "Rollback"},
		{"empty post release metric", func(in *PlanInput) {
			in.ProductID = "game:test"
			in.IdeaCandidate = "cand_x"
			in.ChangeSummary = "ship v1"
			in.Rollback = "revert"
		}, "PostReleaseMetric"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := PlanInput{
				HomePath:    "/tmp/ghome",
				StoreOpener: stubStoreOpener,
			}
			c.mut(&in)
			_, err := Build(context.Background(), in, time.Now().UTC())
			if err == nil {
				t.Fatalf("expected error for %s", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("expected %q in error, got: %v", c.want, err)
			}
		})
	}
}

func TestBuildSchemaIsVersioned(t *testing.T) {
	// Build emits the PlanResult with a stable schema string. The
	// review queue keys off this string, so the constant matters.
	if !strings.HasPrefix(planResultSchema, "compound/game-release-plan-result.v") {
		t.Errorf("schema format mismatch: %q", planResultSchema)
	}
}

func TestPlanResultRoundTrips(t *testing.T) {
	res := PlanResult{
		Schema:     planResultSchema,
		PlanID:     "prlp_test",
		Paths:      product.ProductReleasePlanPaths{PlanPath: "/p.json", ReviewPath: "/p.md", ReviewYAMLPath: "/p.yaml"},
		NextAction: defaultReviewCmd + " /p.yaml",
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back PlanResult
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.PlanID != res.PlanID || back.NextAction != res.NextAction {
		t.Errorf("round trip mismatch: %+v vs %+v", back, res)
	}
	if back.Paths.ReviewYAMLPath != "/p.yaml" {
		t.Errorf("paths.ReviewYAMLPath lost: %q", back.Paths.ReviewYAMLPath)
	}
}
