// Package sourceobs imports SourceObservation records from existing systems.
//
// Source observations are evidence/candidate inputs, not authority. They let
// Compound dogfood generated views, registries, dashboards, and source docs
// without making those files a parallel truth owner.
package sourceobs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/harness"
	"compound/internal/projection"
	"compound/internal/tooltopology"
	"compound/internal/writeback"
	"gopkg.in/yaml.v3"
)

const (
	SchemaVersion          = "compound.source_observation.v0.1"
	CandidateSchemaVersion = "compound.source_candidate.v0.1"
)

type SourceObservation struct {
	SchemaVersion string        `json:"schema_version" yaml:"schema_version"`
	ID            string        `json:"id" yaml:"id"`
	AdapterID     string        `json:"adapter_id" yaml:"adapter_id"`
	SourceSystem  string        `json:"source_system" yaml:"source_system"`
	ObservedAt    string        `json:"observed_at" yaml:"observed_at"`
	SourceRef     SourceRef     `json:"source_ref" yaml:"source_ref"`
	GeneratedView GeneratedView `json:"generated_view,omitempty" yaml:"generated_view,omitempty"`
	MapsTo        []MapTarget   `json:"maps_to" yaml:"maps_to"`
	EvidenceRefs  []string      `json:"evidence_refs" yaml:"evidence_refs"`
	// Release is the optional typed external release-state payload. It is
	// additive: a SourceObservation without it preserves prior behavior, and
	// a SourceObservation with it is still admitted through the same intake
	// door. See release_observation.go for the carrier semantics.
	Release *ReleaseObservation `json:"release,omitempty" yaml:"release,omitempty"`
}

type SourceRef struct {
	Path        string `json:"path" yaml:"path"`
	Kind        string `json:"kind" yaml:"kind"`
	Owner       string `json:"owner" yaml:"owner"`
	SourceClass string `json:"source_class" yaml:"source_class"`
	Revision    string `json:"revision" yaml:"revision"`
}

type GeneratedView struct {
	ViewKind      string      `json:"view_kind" yaml:"view_kind"`
	Consumer      string      `json:"consumer" yaml:"consumer"`
	GeneratedFrom []SourceRef `json:"generated_from,omitempty" yaml:"generated_from,omitempty"`
	Generator     string      `json:"generator" yaml:"generator"`
	Marker        string      `json:"marker" yaml:"marker"`
	Freshness     string      `json:"freshness" yaml:"freshness"`
	DriftReason   string      `json:"drift_reason,omitempty" yaml:"drift_reason,omitempty"`
}

type MapTarget struct {
	ObjectFamily  string `json:"object_family" yaml:"object_family"`
	CandidateKind string `json:"candidate_kind" yaml:"candidate_kind"`
	EntityHint    string `json:"entity_hint" yaml:"entity_hint"`
	// CandidateRevision, when set, replaces capture-level identity components for ID derivation so
	// capture-wide changes do not re-key unchanged rows.
	CandidateRevision string `json:"candidate_revision,omitempty" yaml:"candidate_revision,omitempty"`
	PromoteAllowed    bool   `json:"promote_allowed" yaml:"promote_allowed"`
}

type CandidateRecord struct {
	Release *ReleaseObservation `json:"release,omitempty"`

	SchemaVersion       string   `json:"schema_version"`
	ID                  string   `json:"id"`
	Kind                string   `json:"kind"`
	Subject             string   `json:"subject"`
	Statement           string   `json:"statement"`
	Detail              string   `json:"detail,omitempty"`
	Confidence          float64  `json:"confidence"`
	EvidenceRefs        []string `json:"evidence_refs"`
	SourceObservationID string   `json:"source_observation_id"`
	SourceRef           string   `json:"source_ref"`
	PromoteAllowed      bool     `json:"promote_allowed"`
	Verdict             string   `json:"verdict"`
	CreatedAt           string   `json:"created_at"`
}

type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type Report struct {
	ObservationID string   `json:"observation_id"`
	Status        string   `json:"status"`
	Checks        []Check  `json:"checks"`
	CandidateIDs  []string `json:"candidate_ids"`
	NoPromoteIDs  []string `json:"no_promote_ids,omitempty"`
}

type ConversationMiningObservation struct {
	ID                string
	ReportPath        string
	MinedTagsPath     string
	LossinessPath     string
	Revision          string
	Processed         int
	Accepted          int
	Rejected          int
	MinedRows         int
	AcceptedForReview int
	DreamPackFiles    int
	DreamExecution    string
	LossinessRecorded bool
	LossinessDetail   string
	Note              string
	EvidenceRefs      []string
}

type SessionLensObservation struct {
	ID             string
	SourcePath     string
	SourceRevision string
	Decisions      int
	Outcomes       int
	Patterns       int
	Skills         int
	Taste          int
	Contradictions int
	TotalItems     int
	EvidenceRefs   []string
}

type ConversationCoverageObservation struct {
	ID                string
	ReportPath        string
	Revision          string
	SourceCount       int
	TotalRows         int
	RawFiles          int
	MappedClean       int
	UnmappedRaw       int
	Stable            int
	DreamCandidates   int
	MissingEmbeddings int
	AvgRetainedRatio  float64
	TopTagCount       int
	ReviewDebt        []string
	EvidenceRefs      []string
}

func Load(path string) (SourceObservation, error) {
	var obs SourceObservation
	data, err := os.ReadFile(path)
	if err != nil {
		return obs, fmt.Errorf("read source observation %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &obs); err != nil {
		return obs, fmt.Errorf("parse source observation %s: %w", path, err)
	}
	return obs, nil
}

func Validate(obs SourceObservation) Report {
	r := Report{ObservationID: obs.ID, Status: "ok"}
	add := func(name string, ok bool, detail string) {
		status := "ok"
		if !ok {
			status = "fail"
			r.Status = "fail"
		}
		r.Checks = append(r.Checks, Check{Name: name, Status: status, Detail: detail})
	}
	add("schema", obs.SchemaVersion == SchemaVersion, obs.SchemaVersion)
	add("identity", strings.TrimSpace(obs.ID) != "" && strings.TrimSpace(obs.AdapterID) != "" && strings.TrimSpace(obs.SourceSystem) != "", obs.ID)
	add("source_ref", strings.TrimSpace(obs.SourceRef.Path) != "" && strings.TrimSpace(obs.SourceRef.Kind) != "" && strings.TrimSpace(obs.SourceRef.Owner) != "" && strings.TrimSpace(obs.SourceRef.SourceClass) != "" && strings.TrimSpace(obs.SourceRef.Revision) != "", obs.SourceRef.Path)
	add("evidence", len(obs.EvidenceRefs) > 0, strings.Join(obs.EvidenceRefs, ","))
	add("maps", len(obs.MapsTo) > 0, fmt.Sprintf("%d", len(obs.MapsTo)))
	for _, m := range obs.MapsTo {
		_, known := candidates.SpecFor(candidates.Kind(m.CandidateKind))
		add("candidate_kind:"+m.CandidateKind, known, m.EntityHint)
		add("map_target:"+m.EntityHint, strings.TrimSpace(m.ObjectFamily) != "" && strings.TrimSpace(m.EntityHint) != "", m.ObjectFamily)
	}
	if obs.SourceRef.Kind == "generated_view" || obs.GeneratedView.ViewKind != "" {
		add("generated_view", strings.TrimSpace(obs.GeneratedView.ViewKind) != "" && strings.TrimSpace(obs.GeneratedView.Consumer) != "" && strings.TrimSpace(obs.GeneratedView.Generator) != "" && strings.TrimSpace(obs.GeneratedView.Marker) != "" && strings.TrimSpace(obs.GeneratedView.Freshness) != "", obs.GeneratedView.ViewKind)
	}
	if obs.Release != nil {
		// Structural validation only here: the carrier's
		// FromReleaseObservation already passed the full
		// ValidateReleaseObservation (time, state, identity), and the
		// loader's strict re-validation is the second line of defense.
		// We deliberately skip the time-based check inside Validate()
		// because Validate() does not receive a `now` argument — passing
		// time.Time{} would force a fallback to real wall-clock, which
		// fails for fixtures aged past the staleness window.
		add("release_payload", validateReleaseObservationShape(*obs.Release), obs.Release.ProductID+":"+obs.Release.Channel+":"+obs.Release.Platform)
	}
	return r
}

func Apply(homePath string, obs SourceObservation, now time.Time) (Report, error) {
	report := Validate(obs)
	if report.Status != "ok" {
		return report, fmt.Errorf("source observation invalid")
	}
	wb, err := writeback.Open(homePath)
	if err != nil {
		return report, err
	}
	for _, m := range obs.MapsTo {
		c := CandidateFromMap(obs, m, now)
		if err := WriteCandidate(homePath, c); err != nil {
			return report, err
		}
		report.CandidateIDs = append(report.CandidateIDs, c.ID)
		if !m.PromoteAllowed {
			nop, _, err := wb.RecordNoPromote(NoPromoteDecision(obs, c, now), now)
			if err != nil {
				return report, err
			}
			report.NoPromoteIDs = append(report.NoPromoteIDs, nop.ID)
		}
	}
	return report, nil
}

// FromProjectionFreshness converts a generated-view freshness report into the
// generic source observation contract. Projection drift stays candidate/no-promote
// evidence; this adapter never promotes generated-view truth.
func FromProjectionFreshness(f projection.Freshness, now time.Time) SourceObservation {
	revision := f.OutputHash
	if revision == "" {
		revision = "status:" + f.Status
	}
	return SourceObservation{
		SchemaVersion: SchemaVersion,
		ID:            idFor("srcobs_", "projection", f.ProjectionID, f.ViewRef, f.Status, revision),
		AdapterID:     "compound.projection",
		SourceSystem:  "compound",
		ObservedAt:    rfc3339(now),
		SourceRef: SourceRef{
			Path:        f.ViewRef,
			Kind:        "generated_view",
			Owner:       f.ProjectionID,
			SourceClass: "generated",
			Revision:    revision,
		},
		GeneratedView: GeneratedView{
			ViewKind:      "projection_freshness",
			Consumer:      "compound projection doctor",
			GeneratedFrom: projectionSourceRefs(f),
			Generator:     "compound projection doctor",
			Marker:        "projection_freshness",
			Freshness:     f.Status,
			DriftReason:   projectionDriftReason(f),
		},
		MapsTo: []MapTarget{
			{
				ObjectFamily:   "projection",
				CandidateKind:  string(candidates.MethodUpgrade),
				EntityHint:     f.ProjectionID,
				PromoteAllowed: false,
			},
		},
		EvidenceRefs: projection.EvidenceRefs(f),
	}
}

// FromToolTopology converts a generated/imported tool topology source into the
// generic SourceObservation contract. The topology may update CapabilityGraph
// offers, but the source view itself stays no-promote evidence rather than
// promoted memory or source-owner truth.
func FromToolTopology(topo tooltopology.Topology, now time.Time) SourceObservation {
	revision := strings.TrimSpace(topo.SourceRef.Revision)
	if revision == "" {
		revision = "topology:" + topo.ID
	}
	evidenceRefs := append([]string{}, topo.EvidenceRefs...)
	if len(evidenceRefs) == 0 {
		evidenceRefs = []string{"tool_topology:" + revision}
	}
	return SourceObservation{
		SchemaVersion: SchemaVersion,
		ID:            idFor("srcobs_", "tool_topology", topo.ID, topo.SourceRef.Path, revision),
		AdapterID:     "compound.tool-topology",
		SourceSystem:  "compound",
		ObservedAt:    rfc3339(now),
		SourceRef: SourceRef{
			Path:        topo.SourceRef.Path,
			Kind:        topo.SourceRef.Kind,
			Owner:       firstNonEmpty(topo.SourceRef.Owner, topo.ID),
			SourceClass: topo.SourceRef.SourceClass,
			Revision:    revision,
		},
		GeneratedView: GeneratedView{
			ViewKind:    "tool_topology",
			Consumer:    "compound capability import-topology",
			Generator:   firstNonEmpty(topo.AdapterID, "compound tool topology adapter"),
			Marker:      tooltopology.SchemaVersion,
			Freshness:   "observed",
			DriftReason: fmt.Sprintf("entries=%d recommendations=%d", len(topo.Entries), len(topo.Recommendations)),
		},
		MapsTo: []MapTarget{
			{
				ObjectFamily:   "capability",
				CandidateKind:  string(candidates.CapabilityObs),
				EntityHint:     topo.ID,
				PromoteAllowed: false,
			},
		},
		EvidenceRefs: evidenceRefs,
	}
}

// FromSkillManifest converts an imported skill manifest source into the generic
// SourceObservation contract. Skill imports can seed CapabilityGraph offers, but
// the manifest source itself remains no-promote evidence until validated by
// receipts, decisions, or explicit review.
func FromSkillManifest(manifest capgraph.SkillManifest, now time.Time) SourceObservation {
	revision := strings.TrimSpace(manifest.SourceRef.Revision)
	if revision == "" {
		revision = "skill_manifest:" + manifest.ID
	}
	evidenceRefs := append([]string{}, manifest.EvidenceRefs...)
	if len(evidenceRefs) == 0 {
		evidenceRefs = []string{"skill_manifest:" + revision}
	}
	return SourceObservation{
		SchemaVersion: SchemaVersion,
		ID:            idFor("srcobs_", "skill_manifest", manifest.ID, manifest.SourceRef.Path, revision),
		AdapterID:     "compound.skill-manifest",
		SourceSystem:  "compound",
		ObservedAt:    rfc3339(now),
		SourceRef: SourceRef{
			Path:        manifest.SourceRef.Path,
			Kind:        manifest.SourceRef.Kind,
			Owner:       firstNonEmpty(manifest.SourceRef.Owner, manifest.ID),
			SourceClass: manifest.SourceRef.SourceClass,
			Revision:    revision,
		},
		GeneratedView: GeneratedView{
			ViewKind:    "skill_manifest",
			Consumer:    "compound capability import-skills",
			Generator:   firstNonEmpty(manifest.AdapterID, "compound skill manifest adapter"),
			Marker:      capgraph.SkillManifestSchemaVersion,
			Freshness:   "observed",
			DriftReason: fmt.Sprintf("skills=%d", len(manifest.Skills)),
		},
		MapsTo: []MapTarget{
			{
				ObjectFamily:   "capability",
				CandidateKind:  string(candidates.CapabilityObs),
				EntityHint:     manifest.ID,
				PromoteAllowed: false,
			},
		},
		EvidenceRefs: evidenceRefs,
	}
}

// FromCapabilityEvaluations converts scored capability evaluation records into
// source-observation evidence. The graph adapter can attach evaluation edges to
// known capabilities, but the source stream itself stays no-promote evidence and
// cannot install, retire, or execute a capability.
func FromCapabilityEvaluations(set capgraph.CapabilityEvaluationSet, now time.Time) SourceObservation {
	revision := strings.TrimSpace(set.Revision)
	if revision == "" {
		revision = "capability_evaluations:" + set.ID
	}
	evidenceRefs := append([]string(nil), set.EvidenceRefs...)
	if len(evidenceRefs) == 0 {
		evidenceRefs = []string{"capability_evaluations:" + revision}
	}
	return SourceObservation{
		SchemaVersion: SchemaVersion,
		ID:            idFor("srcobs_", "capability_evaluations", set.ID, set.SourcePath, revision),
		AdapterID:     "compound.capability-evaluations",
		SourceSystem:  "compound",
		ObservedAt:    rfc3339(now),
		SourceRef: SourceRef{
			Path:        set.SourcePath,
			Kind:        firstNonEmpty(set.SourceKind, "state_records"),
			Owner:       firstNonEmpty(set.SourceSystem, set.ID),
			SourceClass: "capability_evaluation",
			Revision:    revision,
		},
		GeneratedView: GeneratedView{
			ViewKind:    "capability_evaluations",
			Consumer:    "compound capability import-evaluations",
			Generator:   firstNonEmpty(set.AdapterID, "compound capability evaluation adapter"),
			Marker:      capgraph.CapabilityEvaluationSetSchemaVersion,
			Freshness:   "observed",
			DriftReason: fmt.Sprintf("evaluations=%d", len(set.Evaluations)),
		},
		MapsTo: []MapTarget{
			{
				ObjectFamily:   "capability",
				CandidateKind:  string(candidates.CapabilityObs),
				EntityHint:     set.ID,
				PromoteAllowed: false,
			},
		},
		EvidenceRefs: evidenceRefs,
	}
}

// FromSkillSurfacingMisses converts generated skill surfacing miss reports into
// source observation evidence. The report may teach CapabilityGraph feedback
// edges, but the report source itself stays no-promote evidence.
func FromSkillSurfacingMisses(misses capgraph.SkillSurfacingMisses, now time.Time) SourceObservation {
	revision := strings.TrimSpace(misses.Revision)
	if revision == "" {
		revision = "skill_surfacing_misses:" + misses.ID
	}
	evidenceRefs := capgraph.SkillSurfacingMissesEvidenceRefs(misses)
	return SourceObservation{
		SchemaVersion: SchemaVersion,
		ID:            idFor("srcobs_", "skill_surfacing_misses", misses.ID, misses.SourcePath, revision),
		AdapterID:     "compound.skill-surfacing-misses",
		SourceSystem:  "compound",
		ObservedAt:    rfc3339(now),
		SourceRef: SourceRef{
			Path:        misses.SourcePath,
			Kind:        "state_record",
			Owner:       firstNonEmpty(capgraph.SkillSurfacingMissesSourceName(misses.SourcePath), misses.ID),
			SourceClass: "skill_surfacing_misses",
			Revision:    revision,
		},
		GeneratedView: GeneratedView{
			ViewKind:    "skill_surfacing_misses",
			Consumer:    "compound capability import-surfacing-misses",
			Generator:   "skill_surfacing_detector",
			Marker:      "skill-surfacing-proposals.json",
			Freshness:   "observed",
			DriftReason: fmt.Sprintf("misses=%d proposals=%d remaining=%d", len(misses.AllMisses), len(misses.Proposals), misses.Remaining),
		},
		MapsTo: []MapTarget{
			{
				ObjectFamily:   "capability",
				CandidateKind:  string(candidates.CapabilityObs),
				EntityHint:     misses.ID,
				PromoteAllowed: false,
			},
		},
		EvidenceRefs: evidenceRefs,
	}
}

// FromConversationMining converts generated conversation-mining reports into
// source observation evidence. Conversation mining identifies extraction
// opportunity and corpus quality; it does not execute Dream, promote memory, or
// mutate source owners.
func FromConversationCoverage(coverage ConversationCoverageObservation, now time.Time) SourceObservation {
	revision := strings.TrimSpace(coverage.Revision)
	if revision == "" {
		revision = "conversation_coverage:" + coverage.ID
	}
	evidenceRefs := append([]string(nil), coverage.EvidenceRefs...)
	if len(evidenceRefs) == 0 {
		evidenceRefs = []string{"conversation_coverage:" + revision}
	}
	reviewDebt := "none"
	if len(coverage.ReviewDebt) > 0 {
		reviewDebt = strings.Join(coverage.ReviewDebt, ",")
	}
	detail := fmt.Sprintf("sources=%d total_rows=%d raw_files=%d mapped_clean=%d unmapped_raw=%d stable=%d dream_candidates=%d missing_embeddings=%d avg_retained_ratio=%.3f top_tags=%d review_debt=%s",
		coverage.SourceCount,
		coverage.TotalRows,
		coverage.RawFiles,
		coverage.MappedClean,
		coverage.UnmappedRaw,
		coverage.Stable,
		coverage.DreamCandidates,
		coverage.MissingEmbeddings,
		coverage.AvgRetainedRatio,
		coverage.TopTagCount,
		reviewDebt,
	)
	return SourceObservation{
		SchemaVersion: SchemaVersion,
		ID:            idFor("srcobs_", "conversation_coverage", coverage.ID, coverage.ReportPath, revision),
		AdapterID:     "compound.conversation-coverage",
		SourceSystem:  "compound",
		ObservedAt:    rfc3339(now),
		SourceRef: SourceRef{
			Path:        coverage.ReportPath,
			Kind:        "state_record",
			Owner:       firstNonEmpty(filepath.Base(coverage.ReportPath), coverage.ID),
			SourceClass: "conversation_coverage",
			Revision:    revision,
		},
		GeneratedView: GeneratedView{
			ViewKind:    "conversation_coverage",
			Consumer:    "compound distiller import-conversation-coverage",
			Generator:   "conversation_coverage_adapter",
			Marker:      "agent-conversation-coverage-summary",
			Freshness:   "observed",
			DriftReason: detail,
		},
		MapsTo: []MapTarget{
			{
				ObjectFamily:   "distiller",
				CandidateKind:  string(candidates.MethodUpgrade),
				EntityHint:     firstNonEmpty(coverage.ID, "conversation_coverage"),
				PromoteAllowed: false,
			},
		},
		EvidenceRefs: evidenceRefs,
	}
}

func FromSessionLenses(lenses SessionLensObservation, now time.Time) SourceObservation {
	revision := strings.TrimSpace(lenses.SourceRevision)
	if revision == "" {
		revision = "session_lenses:" + lenses.ID
	}
	evidenceRefs := append([]string(nil), lenses.EvidenceRefs...)
	if len(evidenceRefs) == 0 {
		evidenceRefs = []string{"session_lenses:" + revision}
	}
	detail := fmt.Sprintf("decisions=%d outcomes=%d patterns=%d skills=%d taste=%d contradictions=%d total_items=%d",
		lenses.Decisions,
		lenses.Outcomes,
		lenses.Patterns,
		lenses.Skills,
		lenses.Taste,
		lenses.Contradictions,
		lenses.TotalItems,
	)
	return SourceObservation{
		SchemaVersion: SchemaVersion,
		ID:            idFor("srcobs_", "session_lenses", lenses.ID, lenses.SourcePath, revision),
		AdapterID:     "compound.session-lenses",
		SourceSystem:  "compound",
		ObservedAt:    rfc3339(now),
		SourceRef: SourceRef{
			Path:        lenses.SourcePath,
			Kind:        "state_record",
			Owner:       firstNonEmpty(filepath.Base(lenses.SourcePath), lenses.ID),
			SourceClass: "session_lenses",
			Revision:    revision,
		},
		GeneratedView: GeneratedView{
			ViewKind:    "session_lenses",
			Consumer:    "compound distiller import-session-lenses",
			Generator:   "session_distill_adapter",
			Marker:      "multi-lens.json",
			Freshness:   "observed",
			DriftReason: detail,
		},
		MapsTo: []MapTarget{
			{
				ObjectFamily:   "distiller",
				CandidateKind:  string(candidates.MethodUpgrade),
				EntityHint:     firstNonEmpty(lenses.ID, "session_lenses"),
				PromoteAllowed: false,
			},
		},
		EvidenceRefs: evidenceRefs,
	}
}

func FromConversationMining(mining ConversationMiningObservation, now time.Time) SourceObservation {
	revision := strings.TrimSpace(mining.Revision)
	if revision == "" {
		revision = "conversation_mining:" + mining.ID
	}
	evidenceRefs := append([]string(nil), mining.EvidenceRefs...)
	if len(evidenceRefs) == 0 {
		evidenceRefs = []string{"conversation_mining:" + revision}
	}
	generatedFrom := []SourceRef{}
	if strings.TrimSpace(mining.MinedTagsPath) != "" {
		generatedFrom = append(generatedFrom, SourceRef{
			Path:        mining.MinedTagsPath,
			Kind:        "state_records",
			Owner:       firstNonEmpty(filepath.Base(mining.MinedTagsPath), mining.ID),
			SourceClass: "conversation_mining",
			Revision:    revision,
		})
	}
	if strings.TrimSpace(mining.LossinessPath) != "" {
		generatedFrom = append(generatedFrom, SourceRef{
			Path:        mining.LossinessPath,
			Kind:        "state_record",
			Owner:       firstNonEmpty(filepath.Base(mining.LossinessPath), mining.ID),
			SourceClass: "conversation_lossiness",
			Revision:    revision,
		})
	}
	detail := fmt.Sprintf("processed=%d accepted=%d rejected=%d mined_rows=%d accepted_for_review=%d dream_execution=%s lossiness_recorded=%t",
		mining.Processed, mining.Accepted, mining.Rejected, mining.MinedRows, mining.AcceptedForReview, firstNonEmpty(mining.DreamExecution, "gated"), mining.LossinessRecorded)
	if strings.TrimSpace(mining.LossinessDetail) != "" {
		detail += " " + strings.TrimSpace(mining.LossinessDetail)
	}
	if strings.TrimSpace(mining.Note) != "" {
		detail += " note=" + strings.TrimSpace(mining.Note)
	}
	return SourceObservation{
		SchemaVersion: SchemaVersion,
		ID:            idFor("srcobs_", "conversation_mining", mining.ID, mining.ReportPath, revision),
		AdapterID:     "compound.conversation-mining",
		SourceSystem:  "compound",
		ObservedAt:    rfc3339(now),
		SourceRef: SourceRef{
			Path:        mining.ReportPath,
			Kind:        "state_record",
			Owner:       firstNonEmpty(filepath.Base(mining.ReportPath), mining.ID),
			SourceClass: "conversation_mining",
			Revision:    revision,
		},
		GeneratedView: GeneratedView{
			ViewKind:      "conversation_mining",
			Consumer:      "compound distiller import-conversation-mining",
			GeneratedFrom: generatedFrom,
			Generator:     "conversation_mining_adapter",
			Marker:        "agent-conversation-mining-report",
			Freshness:     "observed",
			DriftReason:   detail,
		},
		MapsTo: []MapTarget{
			{
				ObjectFamily:   "distiller",
				CandidateKind:  string(candidates.MethodUpgrade),
				EntityHint:     firstNonEmpty(mining.ID, "conversation_mining"),
				PromoteAllowed: false,
			},
		},
		EvidenceRefs: evidenceRefs,
	}
}

// FromHarnessRegistry converts harness capability reality into generic
// SourceObservation evidence. Harness observations are capability evidence only:
// they do not install hooks, mutate capability graph nodes, or grant authority.
func FromHarnessRegistry(reg harness.Registry, now time.Time) SourceObservation {
	revision := harness.RegistryRevision(reg)
	names := reg.Names()
	limitations := registryLimitationCount(reg, names)
	return SourceObservation{
		SchemaVersion: SchemaVersion,
		ID:            idFor("srcobs_", "harness_registry", strings.Join(names, ","), revision),
		AdapterID:     "compound.harness-registry",
		SourceSystem:  "compound",
		ObservedAt:    rfc3339(now),
		SourceRef: SourceRef{
			Path:        "compound://harness/default-registry",
			Kind:        "registry",
			Owner:       "harness_registry",
			SourceClass: "generated",
			Revision:    revision,
		},
		GeneratedView: GeneratedView{
			ViewKind:    "harness_registry",
			Consumer:    "compound harness observe",
			Generator:   "compound harness default registry",
			Marker:      harness.RegistryManifestSchemaVersion,
			Freshness:   "observed",
			DriftReason: fmt.Sprintf("harnesses=%d limitations=%d", len(names), limitations),
		},
		MapsTo: []MapTarget{
			{
				ObjectFamily:   "capability",
				CandidateKind:  string(candidates.CapabilityObs),
				EntityHint:     "harness_registry:default",
				PromoteAllowed: false,
			},
		},
		EvidenceRefs: []string{"harness_registry:" + revision},
	}
}

func FromHarnessRegistryManifest(manifest harness.RegistryManifest, now time.Time) (SourceObservation, error) {
	reg, err := manifest.Registry()
	if err != nil {
		return SourceObservation{}, err
	}
	revision := strings.TrimSpace(manifest.SourceRef.Revision)
	if revision == "" {
		revision = harness.RegistryRevision(reg)
	}
	evidenceRefs := append([]string(nil), manifest.EvidenceRefs...)
	if len(evidenceRefs) == 0 {
		evidenceRefs = []string{"harness_registry:" + revision}
	}
	observedAt := rfc3339(now)
	if strings.TrimSpace(manifest.ObservedAt) != "" {
		observedAt = strings.TrimSpace(manifest.ObservedAt)
	}
	names := reg.Names()
	limitations := registryLimitationCount(reg, names)
	id := firstNonEmpty(manifest.ID, "harness-registry")
	adapterID := firstNonEmpty(manifest.AdapterID, "compound.harness-registry")
	return SourceObservation{
		SchemaVersion: SchemaVersion,
		ID:            idFor("srcobs_", "harness_registry", id, manifest.SourceRef.Path, revision),
		AdapterID:     adapterID,
		SourceSystem:  firstNonEmpty(manifest.SourceSystem, "compound"),
		ObservedAt:    observedAt,
		SourceRef: SourceRef{
			Path:        firstNonEmpty(manifest.SourceRef.Path, "compound://harness/registry-manifest"),
			Kind:        firstNonEmpty(manifest.SourceRef.Kind, "registry"),
			Owner:       firstNonEmpty(manifest.SourceRef.Owner, id),
			SourceClass: firstNonEmpty(manifest.SourceRef.SourceClass, "generated"),
			Revision:    revision,
		},
		GeneratedView: GeneratedView{
			ViewKind:    "harness_registry",
			Consumer:    "compound harness observe",
			Generator:   adapterID,
			Marker:      harness.RegistryManifestSchemaVersion,
			Freshness:   "observed",
			DriftReason: fmt.Sprintf("harnesses=%d limitations=%d manifest=%s", len(names), limitations, id),
		},
		MapsTo: []MapTarget{
			{
				ObjectFamily:   "capability",
				CandidateKind:  string(candidates.CapabilityObs),
				EntityHint:     "harness_registry:" + id,
				PromoteAllowed: false,
			},
		},
		EvidenceRefs: evidenceRefs,
	}, nil
}

func registryLimitationCount(reg harness.Registry, names []string) int {
	total := 0
	for _, name := range names {
		h, _ := reg.Get(name)
		total += len(h.Limitations)
	}
	return total
}

func CandidateFromMap(obs SourceObservation, m MapTarget, now time.Time) CandidateRecord {
	verdict := candidates.Park
	if !m.PromoteAllowed {
		verdict = candidates.NoPromote
	}
	subject := m.EntityHint
	statement := m.ObjectFamily + ":" + obs.SourceRef.Path
	detail := obs.GeneratedView.Freshness
	if detail == "" {
		detail = obs.SourceRef.SourceClass
	}
	candidateID := idFor("src_cand_", obs.ID, m.CandidateKind, m.EntityHint, obs.SourceRef.Revision)
	if m.CandidateRevision != "" {
		candidateID = idFor("src_cand_", obs.AdapterID, obs.SourceRef.Path, m.CandidateKind, m.EntityHint, m.CandidateRevision)
	}
	return CandidateRecord{
		SchemaVersion:       CandidateSchemaVersion,
		ID:                  candidateID,
		Kind:                m.CandidateKind,
		Subject:             subject,
		Statement:           statement,
		Detail:              detail,
		Confidence:          1.0,
		EvidenceRefs:        evidenceRefs(obs),
		SourceObservationID: obs.ID,
		SourceRef:           obs.SourceRef.Path,
		Release:             obs.Release,
		PromoteAllowed:      m.PromoteAllowed,
		Verdict:             verdict,
		CreatedAt:           rfc3339(now),
	}
}

func NoPromoteDecision(obs SourceObservation, c CandidateRecord, now time.Time) writeback.NoPromoteDecision {
	return writeback.NoPromoteDecision{
		SchemaVersion: writeback.NoPromoteSchemaVersion,
		CreatedAt:     rfc3339(now),
		CandidateRef:  c.ID,
		CandidateKind: c.Kind,
		OwnerTarget:   writeback.OwnerTarget{Kind: "source_observation", Ref: obs.ID},
		Basis:         "source observation map forbids promotion",
		EvidenceRefs:  evidenceRefs(obs),
		Status:        writeback.NoPromoteActive,
	}
}

func WriteCandidate(homePath string, c CandidateRecord) error {
	if _, ok := candidates.SpecFor(candidates.Kind(c.Kind)); !ok {
		return fmt.Errorf("candidate kind %q unknown", c.Kind)
	}
	return candidates.WriteCandidate(homePath, candidates.Kind(c.Kind), c.ID, c)
}

func evidenceRefs(obs SourceObservation) []string {
	refs := append([]string(nil), obs.EvidenceRefs...)
	refs = append(refs, "source:"+obs.SourceRef.Path+"#"+obs.SourceRef.Revision)
	return refs
}

func projectionSourceRefs(f projection.Freshness) []SourceRef {
	refs := make([]SourceRef, 0, len(f.Sources))
	for _, source := range f.Sources {
		revision := source.CurrentHash
		if revision == "" {
			revision = "unknown"
		}
		refs = append(refs, SourceRef{
			Path:        source.SourceRef,
			Kind:        "source",
			Owner:       f.ProjectionID,
			SourceClass: "projection_source",
			Revision:    revision,
		})
	}
	return refs
}

func projectionDriftReason(f projection.Freshness) string {
	var reasons []string
	if f.Status != "" {
		reasons = append(reasons, "status="+f.Status)
	}
	for _, check := range f.Checks {
		if check.Status != "ok" {
			reason := check.Name
			if check.Detail != "" {
				reason += ":" + check.Detail
			}
			reasons = append(reasons, reason)
		}
	}
	for _, source := range f.Sources {
		if source.Status != "ok" {
			reasons = append(reasons, "source:"+source.SourceRef+"="+source.Status)
		}
	}
	return strings.Join(reasons, "; ")
}

func idFor(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(sum[:])[:12]
}

func rfc3339(now time.Time) string {
	return now.UTC().Format(time.RFC3339)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
