// Package verdictflow owns the shared plan->record->render->load->annotate scaffolding that the
// panel-verdict review bridges (product observation, source observation, capgraph outbound
// distribution) had copy-pasted. It is pure plumbing: it writes review artifacts through
// internal/reviewartifact and reads them back, but it NEVER touches a ledger, store, or writeback
// surface — every terminal effect stays in the domain adapter's own callback. Each helper is
// byte-compatible with the pre-extraction code it replaces; the domain packages keep their exported
// names, structs, and semantic cores and delegate only the identical mechanics here.
package verdictflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"compound/internal/reviewartifact"

	"gopkg.in/yaml.v3"
)

// PlanArtifacts is the shared plan/review/review-yaml path triple every verdict bridge records.
// Domain adapters copy it into their own exported *PlanPaths struct to keep those types
// byte-compatible.
type PlanArtifacts struct {
	PlanPath       string
	ReviewPath     string
	ReviewYAMLPath string
}

// RecordPlan is the shared plan-record path: it resolves the review area directory, writes the
// json+markdown core pair (reviews/<area>/<planID>.json + .md) with the caller's indent, and writes
// the review-yaml draft (reviews/<area>/<planID>.review.yaml) verbatim. It returns the three
// canonical paths. Callers pass the plan value to serialize, the rendered markdown, and the rendered
// review YAML — this function owns no domain shape and performs no terminal effect. On a Dir failure
// it returns empty paths; on a Write/WriteRaw failure it returns the (fully built) paths plus the
// error, matching the pre-extraction domain Record functions exactly.
func RecordPlan(homePath, area, planID, indent string, plan any, markdown, reviewYAML string) (PlanArtifacts, error) {
	dir, err := reviewartifact.Dir(homePath, area)
	if err != nil {
		return PlanArtifacts{}, err
	}
	paths := PlanArtifacts{
		PlanPath:       filepath.Join(dir, planID+".json"),
		ReviewPath:     filepath.Join(dir, planID+".md"),
		ReviewYAMLPath: filepath.Join(dir, planID+".review.yaml"),
	}
	if _, err := reviewartifact.Write(homePath, area, planID, plan, indent, markdown); err != nil {
		return paths, err
	}
	if _, err := reviewartifact.WriteRaw(homePath, area, planID+".review.yaml", []byte(reviewYAML)); err != nil {
		return paths, err
	}
	return paths, nil
}

// LoadReview reads a YAML review artifact into T. On any error it returns the zero value of T plus
// the raw error (a partial YAML decode is discarded), matching the pre-extraction Load functions.
// An adapter that historically wrapped the parse error keeps its own thin wrapper instead of this.
func LoadReview[T any](path string) (T, error) {
	var review T
	data, err := os.ReadFile(path)
	if err != nil {
		return review, err
	}
	if err := yaml.Unmarshal(data, &review); err != nil {
		var zero T
		return zero, err
	}
	return review, nil
}

// AnnotateQueue is the shared queue-filter loop every verdict bridge's Annotate*Queue wraps: walk
// the report items, skip foreign areas, and hand each matching item to the domain annotator. A nil
// report is a no-op. The domain adapter keeps its exported Annotate*Queue name (pinned by the arch
// contract) and supplies its area constant + item annotator.
func AnnotateQueue(report *reviewartifact.QueueReport, area string, annotate func(*reviewartifact.QueueItem)) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != area {
			continue
		}
		annotate(item)
	}
}

// LatestReviewedFromExtras is the shared "<id>.reviewed.json extras" scan: it decodes every
// .reviewed.json path, keeps the domain-valid records (valid), and returns the newest by createdAt
// (with the pre-extraction tie-break of "keep the first seen until a strictly-later one appears").
// The bool mirrors each adapter's `found.ID != ""` return via the id accessor, so an empty-id record
// is reported as not-found exactly as before.
func LatestReviewedFromExtras[T any](paths []string, valid func(T) bool, id func(T) string, createdAt func(T) string) (T, bool) {
	var found T
	for _, path := range paths {
		if !strings.HasSuffix(path, ".reviewed.json") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var report T
		if err := json.Unmarshal(data, &report); err != nil {
			continue
		}
		if !valid(report) {
			continue
		}
		if id(found) == "" || createdAt(report) > createdAt(found) {
			found = report
		}
	}
	return found, id(found) != ""
}

// Compact drops blank and TODO-placeholder-prefixed values after trimming — the shared evidence-ref
// hygiene the adapters used before dedup/sort. Byte-identical to the retired per-package copies.
func Compact(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !strings.HasPrefix(value, "TODO") {
			out = append(out, value)
		}
	}
	return out
}

// OneOf reports whether the trimmed value is one of the allowed decisions.
func OneOf(value string, allowed []string) bool {
	value = strings.TrimSpace(value)
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// ShellJoin renders argv as a single POSIX-quoted command line for a review action label.
func ShellJoin(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, shellArg(arg))
	}
	return strings.Join(parts, " ")
}

func shellArg(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.ContainsAny(arg, " \t\n\"'\\$`") {
		return "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
	}
	return arg
}
