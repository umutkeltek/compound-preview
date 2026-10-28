package mem

import (
	"encoding/json"
	"fmt"
	"os"

	"compound/internal/reviewartifact"
)

func (p MeaningWritePlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForMeaningWritePlan(p)
}

func OperatorCardForMeaningWritePlan(p MeaningWritePlan) (reviewartifact.OperatorCard, error) {
	entity, err := reviewartifact.RequireText("entity", p.Entity)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	predicate, err := reviewartifact.RequireText("predicate", p.Predicate)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("Memory has a curated write ready for %s %s. Review the source edit?", reviewartifact.ShortLabel(entity, 34), reviewartifact.ShortLabel(reviewartifact.PlainLabel(predicate), 30)),
		entity,
		"meaning-write|"+reviewartifact.FirstNonEmpty(p.ClaimType, p.Predicate),
		"curated memory changes only after source review",
	)
}

func OperatorCardForBakeoffReport(r BakeoffReport) (reviewartifact.OperatorCard, error) {
	engine, err := reviewartifact.RequireText("candidate_engine", r.CandidateEngine)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("A recall bake-off ran %d golden queries against %s. Read the scores.", r.QueryCount, reviewartifact.ShortLabel(engine, 42)),
		engine,
		"memory-bakeoffs|"+engine,
		"",
	)
}

// AnnotateBakeoffQueue fills in the operator card for root-level
// memory-bakeoff-*.json strays: reports written before RunBakeoff started
// baking a card in (and before the memory-bakeoffs/ subdirectory convention
// existed), so they sit at reviews/ root with area=="" and no
// operator_summary on disk. It never rewrites the artifact; it only
// completes the read-time queue projection, matching the pattern every other
// domain annotator uses for pre-existing artifacts.
func AnnotateBakeoffQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != "" {
			continue
		}
		annotateBakeoffQueueItem(item)
	}
}

func annotateBakeoffQueueItem(item *reviewartifact.QueueItem) {
	if item.JSONPath == "" {
		return
	}
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		return
	}
	var bakeoff BakeoffReport
	if err := json.Unmarshal(data, &bakeoff); err != nil || bakeoff.SchemaVersion != BakeoffReportSchemaVersion {
		return
	}
	card, err := OperatorCardForBakeoffReport(bakeoff)
	if err != nil {
		return
	}
	if item.Status == "" || item.Status == "unknown" {
		item.Status = "recorded"
	}
	item.Readiness = reviewartifact.FirstNonEmpty(item.Readiness, "no_action")
	item.NextAction = reviewartifact.FirstNonEmpty(item.NextAction, "recall bake-off report filed; read the scores, no action required")
	_ = reviewartifact.SetOperatorCard(item, card, reviewartifact.SummarySourceAnnotator, reviewartifact.BandFiled)
}
