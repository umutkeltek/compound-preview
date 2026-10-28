// Package product owns the first product-feedback loop: product registry facts,
// market signals, and hypothesis candidates. Product pages are market-control
// surfaces, not repo summaries; signals are evidence, not truth.
package product

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/mem"
	"compound/internal/reviewartifact"
	"compound/internal/writeback"

	"gopkg.in/yaml.v3"
)

const (
	SchemaVersion                  = "compound.product_registry.v0.1"
	SignalSchemaVersion            = "compound.product_signal.v0.1"
	SignalExportSchemaVersion      = "compound.product_signal_export.v0.1"
	ProductHypothesisCandidate     = "compound.product_hypothesis_candidate.v0.1"
	ReviewSchemaVersion            = "compound.product_review.v0.1"
	SourceIssueReviewSchemaVersion = "compound.product_source_issue_review.v0.1"
)

const ascSignalFreshnessWindow = 48 * time.Hour

var signalExportProviderAllowlist = map[string]struct{}{
	"app_store_connect":     {},
	"revenuecat":            {},
	"posthog":               {},
	"google_search_console": {},
	"google_analytics":      {},
	"google_adsense":        {},
	// Paid acquisition platforms (see ads.go); the ads ledger bridges into
	// ProductSignals through the same door, so a hand-written export with
	// one of these source kinds validates too.
	"apple_search_ads": {},
	"google_ads":       {},
	"meta_ads":         {},
	"tiktok_ads":       {},
	"x_ads":            {},
	"reddit_ads":       {},
	"linkedin_ads":     {},
}

// canonicalSignalMetricNames is the conservative alias table for source-export
// metrics. Keys are normalized "<source_kind>:<metric_name>"; values are the
// ProductSignal metric names stored in the ledger.
var canonicalSignalMetricNames = map[string]string{
	"app_store_connect:product_page_conversion_rate": "app_store_product_page_conversion",
	"app_store_connect:conversion_rate":              "app_store_product_page_conversion",
	"revenuecat:active_subscriptions":                "revenuecat_active_subscriptions",
	"revenuecat:active_subscribers":                  "revenuecat_active_subscriptions",
	"revenuecat:mrr":                                 "revenuecat_mrr",
	"posthog:activation_rate":                        "posthog_activation_rate",
	"google_search_console:clicks":                   "gsc_clicks",
	"google_search_console:impressions":              "gsc_impressions",
	"google_search_console:ctr":                      "gsc_ctr",
	"google_search_console:avg_position":             "gsc_avg_position",
	"google_analytics:active_users":                  "ga4_active_users",
	"google_analytics:sessions":                      "ga4_sessions",
	"google_analytics:conversions":                   "ga4_conversions",
	"google_adsense:estimated_earnings":              "adsense_estimated_earnings",
	"google_adsense:page_rpm":                        "adsense_page_rpm",
	"google_adsense:impressions":                     "adsense_impressions",
}

type Bundle struct {
	SchemaVersion string       `json:"schema_version" yaml:"schema_version"`
	Product       Product      `json:"product" yaml:"product"`
	Signals       []Signal     `json:"signals,omitempty" yaml:"signals,omitempty"`
	Hypotheses    []Hypothesis `json:"hypotheses,omitempty" yaml:"hypotheses,omitempty"`
	EvidenceRefs  []string     `json:"evidence_refs" yaml:"evidence_refs"`
	// contractSourced marks a bundle built from a source-in-place project
	// contract. Apply imports such a bundle additively; see additiveContractBundle.
	contractSourced bool
}

type Product struct {
	ID   string `json:"id" yaml:"id"`
	Name string `json:"name" yaml:"name"`
	// Aliases are other names this product is known by (a former slug, a brand
	// rename). They are registered on the ledger entity so Where can join a
	// renamed product's records; they never mint an entity of their own.
	Aliases         []string        `json:"aliases,omitempty" yaml:"aliases,omitempty"`
	OwnerEntity     string          `json:"owner_entity" yaml:"owner_entity"`
	Surfaces        []Surface       `json:"surfaces" yaml:"surfaces"`
	Channels        []string        `json:"channels" yaml:"channels"`
	Positioning     string          `json:"positioning" yaml:"positioning"`
	PrivacyPosture  string          `json:"privacy_posture" yaml:"privacy_posture"`
	ReleaseIdentity ReleaseIdentity `json:"release_identity,omitempty" yaml:"release_identity,omitempty"`
	Deployment      Deployment      `json:"deployment,omitempty" yaml:"deployment,omitempty"`
	TasteProfile    TasteProfile    `json:"taste_profile,omitempty" yaml:"taste_profile,omitempty"`
	// Essence, Forms, and Learned are the curated product record every stage
	// reads (essence.go). They are recorded as essence.*, form.*, and learned.*
	// facts and read back from the folded current view.
	Essence Essence       `json:"essence,omitzero" yaml:"essence,omitempty"`
	Forms   []Form        `json:"forms,omitempty" yaml:"forms,omitempty"`
	Learned []LearnedFact `json:"learned,omitempty" yaml:"learned,omitempty"`
}

// Deployment is where a product actually runs and how it gets there. Surfaces
// answer "where is it sold"; Deployment answers "where does it run" — the
// question an agent asks before touching a release. It is imported from the
// source-in-place project contract only: Compound never infers a place or a
// deploy command, because an invented place aims a real command at the wrong
// machine. An empty Place means unknown, and unknown is the useful answer.
type Deployment struct {
	Place        string   `json:"place,omitempty" yaml:"place,omitempty"`
	Method       string   `json:"method,omitempty" yaml:"method,omitempty"`
	Command      string   `json:"command,omitempty" yaml:"command,omitempty"`
	Rollback     string   `json:"rollback,omitempty" yaml:"rollback,omitempty"`
	Services     []string `json:"services,omitempty" yaml:"services,omitempty"`
	Healthchecks []string `json:"healthchecks,omitempty" yaml:"healthchecks,omitempty"`
}

// ReleaseIdentity is the product-owned destination identity used by release
// planning. It is imported into Compound claims; release planning never reads
// a legacy registry or product repository to reconstruct these fields.
type ReleaseIdentity struct {
	BundleID             string   `json:"bundle_id,omitempty" yaml:"bundle_id,omitempty"`
	AppStoreConnectAppID string   `json:"app_store_connect_app_id,omitempty" yaml:"app_store_connect_app_id,omitempty"`
	SubscriptionIDs      []string `json:"subscription_product_ids,omitempty" yaml:"subscription_product_ids,omitempty"`
	Version              string   `json:"version,omitempty" yaml:"version,omitempty"`
	Build                string   `json:"build,omitempty" yaml:"build,omitempty"`
}

type Surface struct {
	ID   string `json:"id" yaml:"id"`
	Kind string `json:"kind" yaml:"kind"`
	Ref  string `json:"ref" yaml:"ref"`
}

type TasteProfile struct {
	Standards []string `json:"standards,omitempty" yaml:"standards,omitempty"`
	RedLines  []string `json:"red_lines,omitempty" yaml:"red_lines,omitempty"`
}

type Signal struct {
	ID              string           `json:"id" yaml:"id"`
	ProductID       string           `json:"product_id" yaml:"product_id"`
	Surface         string           `json:"surface" yaml:"surface"`
	ChangeEvent     string           `json:"change_event" yaml:"change_event"`
	MetricName      string           `json:"metric_name" yaml:"metric_name"`
	Before          float64          `json:"before" yaml:"before"`
	After           float64          `json:"after" yaml:"after"`
	Window          string           `json:"window" yaml:"window"`
	Confidence      float64          `json:"confidence" yaml:"confidence"`
	Confounders     []string         `json:"confounders,omitempty" yaml:"confounders,omitempty"`
	Interpretations []Interpretation `json:"interpretations,omitempty" yaml:"interpretations,omitempty"`
	NextAction      string           `json:"next_action,omitempty" yaml:"next_action,omitempty"`
	EvidenceRefs    []string         `json:"evidence_refs" yaml:"evidence_refs"`
	ObservedAt      string           `json:"observed_at,omitempty" yaml:"observed_at,omitempty"`
}

type Interpretation struct {
	Lens  string `json:"lens" yaml:"lens"`
	Claim string `json:"claim" yaml:"claim"`
}

type SignalDocument struct {
	SchemaVersion string       `json:"schema_version" yaml:"schema_version"`
	Source        SignalSource `json:"source" yaml:"source"`
	Signal        Signal       `json:"signal" yaml:"signal"`
	EvidenceRefs  []string     `json:"evidence_refs,omitempty" yaml:"evidence_refs,omitempty"`
}

type SignalExportDocument struct {
	SchemaVersion   string           `json:"schema_version" yaml:"schema_version"`
	Source          SignalSource     `json:"source" yaml:"source"`
	SignalID        string           `json:"signal_id,omitempty" yaml:"signal_id,omitempty"`
	ProductID       string           `json:"product_id" yaml:"product_id"`
	Surface         string           `json:"surface" yaml:"surface"`
	ChangeEvent     string           `json:"change_event" yaml:"change_event"`
	Metric          ExportMetric     `json:"metric" yaml:"metric"`
	Window          string           `json:"window" yaml:"window"`
	Confidence      float64          `json:"confidence" yaml:"confidence"`
	Confounders     []string         `json:"confounders,omitempty" yaml:"confounders,omitempty"`
	Interpretations []Interpretation `json:"interpretations,omitempty" yaml:"interpretations,omitempty"`
	NextAction      string           `json:"next_action,omitempty" yaml:"next_action,omitempty"`
	EvidenceRefs    []string         `json:"evidence_refs" yaml:"evidence_refs"`
	ObservedAt      string           `json:"observed_at,omitempty" yaml:"observed_at,omitempty"`
}

type ASCDatapointsSignalOptions struct {
	ProductID string
	AppID     string
	Metric    string
}

type ASCSalesSeriesSignalOptions struct {
	ProductID    string
	AppID        string
	Metric       string
	BeforeWindow string
	AfterWindow  string
	ChangeEvent  string
}

type ascDatapointsDocument struct {
	OK         bool               `json:"ok"`
	Date       string             `json:"date"`
	CapturedAt string             `json:"captured_at"`
	Rows       []ascDatapointsRow `json:"rows"`
}

type ascDatapointsRow struct {
	Date                    string             `json:"date"`
	CapturedAt              string             `json:"captured_at"`
	Slug                    string             `json:"slug"`
	Name                    string             `json:"name"`
	ASCAppID                string             `json:"asc_app_id"`
	BundleID                string             `json:"bundle_id"`
	Released                bool               `json:"released"`
	PublicAvailable         bool               `json:"public_available"`
	CurrentAppStoreState    string             `json:"current_appstore_state"`
	AnalyticsRequestPresent bool               `json:"analytics_request_present"`
	AnalyticsRequestCount   float64            `json:"analytics_request_count"`
	ReviewCount             float64            `json:"review_count"`
	AverageRating           *float64           `json:"average_rating"`
	RatingCounts            map[string]float64 `json:"rating_counts"`
}

type ascSalesSeriesRow struct {
	Date                 string  `json:"date"`
	AppleIdentifier      string  `json:"apple_identifier"`
	Title                string  `json:"title"`
	Units                float64 `json:"units"`
	AppUnits             float64 `json:"app_units"`
	IAPUnits             float64 `json:"iap_units"`
	CustomerSalesUSDEst  float64 `json:"customer_sales_usd_est"`
	DeveloperProceedsUSD float64 `json:"developer_proceeds_usd"`
}

type dateRange struct {
	Raw   string
	Start time.Time
	End   time.Time
}

type salesWindowAggregate struct {
	Sum   float64
	Rows  int
	Start string
	End   string
}

type ExportMetric struct {
	Name          string            `json:"name" yaml:"name"`
	CanonicalName string            `json:"canonical_name,omitempty" yaml:"canonical_name,omitempty"`
	Before        ExportMetricPoint `json:"before" yaml:"before"`
	After         ExportMetricPoint `json:"after" yaml:"after"`
}

type ExportMetricPoint struct {
	Value  float64 `json:"value" yaml:"value"`
	Period string  `json:"period,omitempty" yaml:"period,omitempty"`
	Ref    string  `json:"ref,omitempty" yaml:"ref,omitempty"`
}

type SignalSource struct {
	Kind string `json:"kind" yaml:"kind"`
	Ref  string `json:"ref" yaml:"ref"`
}

type Hypothesis struct {
	ID                string   `json:"id" yaml:"id"`
	ProductID         string   `json:"product_id" yaml:"product_id"`
	ChangeEvent       string   `json:"change_event" yaml:"change_event"`
	ExpectedEffect    string   `json:"expected_effect" yaml:"expected_effect"`
	Metrics           []string `json:"metrics" yaml:"metrics"`
	ObservationWindow string   `json:"observation_window" yaml:"observation_window"`
	Result            string   `json:"result" yaml:"result"`
	NextAction        string   `json:"next_action" yaml:"next_action"`
	EvidenceRefs      []string `json:"evidence_refs" yaml:"evidence_refs"`
}

type Review struct {
	SchemaVersion string   `json:"schema_version" yaml:"schema_version"`
	ID            string   `json:"id" yaml:"id"`
	ProductID     string   `json:"product_id" yaml:"product_id"`
	HypothesisID  string   `json:"hypothesis_id" yaml:"hypothesis_id"`
	Result        string   `json:"result" yaml:"result"`
	Rationale     string   `json:"rationale" yaml:"rationale"`
	Confidence    float64  `json:"confidence" yaml:"confidence"`
	NextAction    string   `json:"next_action,omitempty" yaml:"next_action,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs" yaml:"evidence_refs"`
	ReviewedBy    string   `json:"reviewed_by,omitempty" yaml:"reviewed_by,omitempty"`
	ReviewedAt    string   `json:"reviewed_at,omitempty" yaml:"reviewed_at,omitempty"`
	AppliedAt     string   `json:"applied_at,omitempty" yaml:"applied_at,omitempty"`
	EvidenceID    string   `json:"evidence_id,omitempty" yaml:"evidence_id,omitempty"`
	SourceRef     string   `json:"source_ref,omitempty" yaml:"source_ref,omitempty"`
}

type SourceIssueReview struct {
	SchemaVersion string               `json:"schema_version" yaml:"schema_version"`
	ID            string               `json:"id" yaml:"id"`
	ProductID     string               `json:"product_id" yaml:"product_id"`
	IssueID       string               `json:"issue_id" yaml:"issue_id"`
	Result        string               `json:"result" yaml:"result"`
	Rationale     string               `json:"rationale" yaml:"rationale"`
	EvidenceRefs  []string             `json:"evidence_refs" yaml:"evidence_refs"`
	FollowUp      *SourceIssueFollowUp `json:"follow_up,omitempty" yaml:"follow_up,omitempty"`
	ReviewedAt    string               `json:"reviewed_at,omitempty" yaml:"reviewed_at,omitempty"`
	AppliedAt     string               `json:"applied_at,omitempty" yaml:"applied_at,omitempty"`
	EvidenceID    string               `json:"evidence_id,omitempty" yaml:"evidence_id,omitempty"`
	SourceRef     string               `json:"source_ref,omitempty" yaml:"source_ref,omitempty"`
}

type SourceIssueFollowUp struct {
	ChangeEvent       string   `json:"change_event" yaml:"change_event"`
	ExpectedEffect    string   `json:"expected_effect" yaml:"expected_effect"`
	Metrics           []string `json:"metrics" yaml:"metrics"`
	ObservationWindow string   `json:"observation_window" yaml:"observation_window"`
	NextAction        string   `json:"next_action" yaml:"next_action"`
	EvidenceRefs      []string `json:"evidence_refs,omitempty" yaml:"evidence_refs,omitempty"`
}

type HypothesisCandidate struct {
	// Only the approval CAS appends to this field. Measurement keeps it intact;
	// caller-supplied evidence citations cannot establish an approval outcome.
	CommittedApprovalIDs []string `json:"committed_approval_ids,omitempty"`
	// Only the review CAS appends this witness. Citations cannot prove commitment.
	CommittedReviewIDs []string `json:"committed_review_ids,omitempty"`
	SchemaVersion      string   `json:"schema_version"`
	ID                 string   `json:"id"`
	Kind               string   `json:"kind"`
	ProductID          string   `json:"product_id"`
	ChangeEvent        string   `json:"change_event"`
	ExpectedEffect     string   `json:"expected_effect"`
	Metrics            []string `json:"metrics"`
	ObservationWindow  string   `json:"observation_window"`
	Result             string   `json:"result"`
	NextAction         string   `json:"next_action"`
	Verdict            string   `json:"verdict"`
	EvidenceRefs       []string `json:"evidence_refs"`
	CreatedAt          string   `json:"created_at"`
}

type Report struct {
	ProductID     string   `json:"product_id"`
	Status        string   `json:"status"`
	EntityID      string   `json:"entity_id"`
	EvidenceIDs   []string `json:"evidence_ids"`
	ClaimIDs      []string `json:"claim_ids"`
	SignalIDs     []string `json:"signal_ids"`
	HypothesisIDs []string `json:"hypothesis_ids"`
	TransitionIDs []string `json:"transition_ids,omitempty"`
	Retracted     []string `json:"retracted,omitempty"` // essence_retract.go
}

type ReviewReport struct {
	Status           string   `json:"status"`
	CandidateOutcome string   `json:"candidate_outcome"`
	SnapshotRef      string   `json:"snapshot_ref,omitempty"`
	ProductID        string   `json:"product_id"`
	HypothesisID     string   `json:"hypothesis_id"`
	ReviewID         string   `json:"review_id"`
	Result           string   `json:"result"`
	ReviewedBy       string   `json:"reviewed_by"`
	EvidenceID       string   `json:"evidence_id"`
	EvidenceRefs     []string `json:"evidence_refs"`
	TransitionID     string   `json:"transition_id,omitempty"`
}

type ReviewArtifactRecord struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	CreatedAt     string   `json:"created_at"`
	Status        string   `json:"status"`
	ProductID     string   `json:"product_id"`
	HypothesisID  string   `json:"hypothesis_id"`
	ReviewID      string   `json:"review_id"`
	Result        string   `json:"result"`
	ReviewedBy    string   `json:"reviewed_by,omitempty"`
	EvidenceID    string   `json:"evidence_id"`
	EvidenceRefs  []string `json:"evidence_refs,omitempty"`
	TransitionID  string   `json:"transition_id,omitempty"`
	ReviewPath    string   `json:"review_path"`
	Warnings      []string `json:"warnings,omitempty"`
}

type SourceIssueReviewReport struct {
	Status       string   `json:"status"`
	ProductID    string   `json:"product_id"`
	IssueID      string   `json:"issue_id"`
	ReviewID     string   `json:"review_id"`
	Result       string   `json:"result"`
	EvidenceID   string   `json:"evidence_id"`
	EvidenceRefs []string `json:"evidence_refs"`
	TransitionID string   `json:"transition_id,omitempty"`
	HypothesisID string   `json:"hypothesis_id,omitempty"`
}

type Timeline struct {
	ProductID    string
	Signals      []Signal
	Hypotheses   []HypothesisCandidate
	Transitions  []TimelineTransition
	SourceIssues []ProductSourceIssue
}

type TimelineTransition struct {
	ID                    string   `json:"id"`
	CreatedAt             string   `json:"created_at"`
	Kind                  string   `json:"kind,omitempty"`
	ObjectRef             string   `json:"object_ref,omitempty"`
	StateDelta            string   `json:"state_delta"`
	EvidenceRefs          []string `json:"evidence_refs,omitempty"`
	FutureBehaviorChanged bool     `json:"future_behavior_changed"`
}

type ProductSourceIssue struct {
	ID           string   `json:"id"`
	CreatedAt    string   `json:"created_at"`
	SourceRef    string   `json:"source_ref"`
	Basis        string   `json:"basis"`
	EvidenceRefs []string `json:"evidence_refs"`
	Status       string   `json:"status,omitempty"`
	ReviewID     string   `json:"review_id,omitempty"`
	ReviewResult string   `json:"review_result,omitempty"`
}

type DoctorReport struct {
	ProductID           string                `json:"product_id"`
	Status              string                `json:"status"`
	SignalCount         int                   `json:"signal_count"`
	HypothesisCount     int                   `json:"hypothesis_count"`
	SourceIssueCount    int                   `json:"source_issue_count"`
	PendingReviewCount  int                   `json:"pending_review_count"`
	ReviewReadyCount    int                   `json:"review_ready_count"`
	PendingReview       []HypothesisCandidate `json:"pending_review,omitempty"`
	ReviewReady         []ReviewReady         `json:"review_ready,omitempty"`
	SourceIssues        []ProductSourceIssue  `json:"source_issues,omitempty"`
	MissingEvidenceRefs []string              `json:"missing_evidence_refs,omitempty"`
	ASCSignalStaleness  []ASCSignalStaleness  `json:"asc_signal_staleness,omitempty"`
	// Provenance debt (read-only; existing records are never rewritten).
	// unattributed_judgments = product review records with no named actor;
	// agent_annotated_judgments = records attributed to an agent lane that
	// annotate but never substitute for an operator adjudication.
	UnattributedJudgments     int      `json:"unattributed_judgments"`
	UnattributedJudgmentIDs   []string `json:"unattributed_judgment_ids,omitempty"`
	AgentAnnotatedJudgments   int      `json:"agent_annotated_judgments"`
	AgentAnnotatedJudgmentIDs []string `json:"agent_annotated_judgment_ids,omitempty"`
}

type ASCSignalStaleness struct {
	ProductID      string `json:"product_id"`
	LastSignalID   string `json:"last_signal_id,omitempty"`
	LastObservedAt string `json:"last_observed_at,omitempty"`
	AgeHours       int    `json:"age_hours,omitempty"`
	Status         string `json:"status"`
	Reason         string `json:"reason,omitempty"`
	FixCommand     string `json:"fix_command"`
}

type ReviewReady struct {
	HypothesisID      string   `json:"hypothesis_id"`
	SignalID          string   `json:"signal_id"`
	MetricName        string   `json:"metric_name"`
	Window            string   `json:"window"`
	MatchReason       string   `json:"match_reason"`
	ReviewPlanCommand string   `json:"review_plan_command"`
	EvidenceRefs      []string `json:"evidence_refs,omitempty"`
}

type SignalDoctorReport struct {
	Path          string        `json:"path"`
	SourceRef     string        `json:"source_ref"`
	SourceHash    string        `json:"source_hash,omitempty"`
	SchemaVersion string        `json:"schema_version,omitempty"`
	Status        string        `json:"status"`
	SignalID      string        `json:"signal_id,omitempty"`
	ProductID     string        `json:"product_id,omitempty"`
	MetricName    string        `json:"metric_name,omitempty"`
	SourceKind    string        `json:"source_kind,omitempty"`
	SourceID      string        `json:"source_id,omitempty"`
	Checks        []SignalCheck `json:"checks"`
	NoPromoteID   string        `json:"no_promote_id,omitempty"`
}

type SignalCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func Load(path string) (Bundle, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Bundle{}, err
	}
	var bundle Bundle
	if err := yaml.Unmarshal(b, &bundle); err != nil {
		return Bundle{}, err
	}
	return bundle, nil
}

func LoadSignal(path string) (SignalDocument, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return SignalDocument{}, err
	}
	return loadSignalBytes(b)
}

func LoadASCDatapointsSignal(path string, opts ASCDatapointsSignalOptions) (SignalDocument, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return SignalDocument{}, err
	}
	var doc ascDatapointsDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		return SignalDocument{}, fmt.Errorf("parse ASC datapoints %s: %w", path, err)
	}
	return adaptASCDatapointsSignal(doc, opts)
}

func LoadASCSalesSeriesSignal(path string, opts ASCSalesSeriesSignalOptions) (SignalDocument, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return SignalDocument{}, err
	}
	rows, err := parseASCSalesSeriesRows(b, path)
	if err != nil {
		return SignalDocument{}, err
	}
	return adaptASCSalesSeriesSignal(rows, opts)
}

func loadSignalBytes(b []byte) (SignalDocument, error) {
	var header struct {
		SchemaVersion string `yaml:"schema_version"`
	}
	if err := yaml.Unmarshal(b, &header); err != nil {
		return SignalDocument{}, err
	}
	switch header.SchemaVersion {
	case SignalSchemaVersion:
		var doc SignalDocument
		if err := decodeKnownYAML(b, &doc); err != nil {
			return SignalDocument{}, err
		}
		return doc, nil
	case SignalExportSchemaVersion:
		var doc SignalExportDocument
		if err := decodeKnownYAML(b, &doc); err != nil {
			return SignalDocument{}, err
		}
		return AdaptSignalExport(doc)
	default:
		return SignalDocument{}, fmt.Errorf("schema_version must be %s or %s, got %q", SignalSchemaVersion, SignalExportSchemaVersion, header.SchemaVersion)
	}
}

func SignalDoctor(path string) SignalDoctorReport {
	report := SignalDoctorReport{
		Path:      path,
		SourceRef: "product_signal_source:" + filepath.Base(path),
		Status:    "ok",
	}
	add := func(name string, ok bool, detail string) {
		status := "ok"
		if !ok {
			status = "fail"
			report.Status = "fail"
		}
		report.Checks = append(report.Checks, SignalCheck{Name: name, Status: status, Detail: detail})
	}
	b, err := os.ReadFile(path)
	if err != nil {
		add("read", false, err.Error())
		return report
	}
	report.SourceHash = "sha256:" + hashBytes(b)
	add("read", true, report.SourceHash)
	var header struct {
		SchemaVersion string `yaml:"schema_version"`
	}
	if err := yaml.Unmarshal(b, &header); err != nil {
		add("schema_header", false, err.Error())
		return report
	}
	report.SchemaVersion = header.SchemaVersion
	add("schema", oneOf(header.SchemaVersion, []string{SignalSchemaVersion, SignalExportSchemaVersion}), header.SchemaVersion)
	applySignalHints(b, &report)
	doc, err := loadSignalBytes(b)
	if err != nil {
		add("strict_contract", false, err.Error())
		return report
	}
	if err := ValidateSignalDocument(doc); err != nil {
		add("signal_contract", false, err.Error())
		return report
	}
	report.SignalID = doc.Signal.ID
	report.ProductID = doc.Signal.ProductID
	report.MetricName = doc.Signal.MetricName
	report.SourceKind = doc.Source.Kind
	report.SourceID = doc.Source.Ref
	add("strict_contract", true, "known fields only")
	add("signal_contract", true, doc.Signal.ID)
	return report
}

func RecordSignalDoctorNoPromote(homePath string, report SignalDoctorReport, now time.Time) (writeback.NoPromoteDecision, bool, error) {
	if report.Status == "ok" {
		return writeback.NoPromoteDecision{}, false, fmt.Errorf("product signal source %q is valid; no no-promote decision needed", report.SourceRef)
	}
	store, err := writeback.Open(homePath)
	if err != nil {
		return writeback.NoPromoteDecision{}, false, err
	}
	return store.RecordNoPromote(SignalDoctorNoPromote(report), now)
}

func SignalDoctorNoPromote(report SignalDoctorReport) writeback.NoPromoteDecision {
	ref := report.SourceHash
	if ref == "" {
		ref = report.SourceRef
	}
	evidenceRefs := compact([]string{
		report.SourceRef,
		report.SourceHash,
		"schema:" + report.SchemaVersion,
		productRef(report.ProductID),
		sourceKindRef(report.SourceKind),
	})
	return writeback.NoPromoteDecision{
		SchemaVersion: writeback.NoPromoteSchemaVersion,
		CandidateRef:  "product_signal_source:" + ref,
		OwnerTarget:   writeback.OwnerTarget{Kind: "product_signal_source", Ref: report.SourceRef},
		Basis:         "product signal source failed strict contract: " + signalDoctorFailure(report),
		EvidenceRefs:  evidenceRefs,
		Status:        writeback.NoPromoteActive,
	}
}

func applySignalHints(b []byte, report *SignalDoctorReport) {
	var hints struct {
		Source    SignalSource `yaml:"source"`
		SignalID  string       `yaml:"signal_id"`
		ProductID string       `yaml:"product_id"`
		Metric    ExportMetric `yaml:"metric"`
		Signal    Signal       `yaml:"signal"`
	}
	if err := yaml.Unmarshal(b, &hints); err != nil {
		return
	}
	if report.SourceKind == "" {
		report.SourceKind = hints.Source.Kind
	}
	if report.SourceID == "" {
		report.SourceID = hints.Source.Ref
	}
	if report.SignalID == "" {
		report.SignalID = firstNonEmpty(hints.SignalID, hints.Signal.ID)
	}
	if report.ProductID == "" {
		report.ProductID = firstNonEmpty(hints.ProductID, hints.Signal.ProductID)
	}
	if report.MetricName == "" {
		report.MetricName = firstNonEmpty(hints.Signal.MetricName, strings.TrimSpace(hints.Metric.CanonicalName))
		if report.MetricName == "" && strings.TrimSpace(hints.Metric.Name) != "" {
			report.MetricName = canonicalMetricName(hints.Source.Kind, hints.Metric.Name)
		}
	}
}

func LoadReview(path string) (Review, error) {
	review, _, err := loadReviewSource(path)
	return review, err
}

var readReviewSourceFile = os.ReadFile

// The parsed request and its fingerprint must describe the same captured read.
func loadReviewSource(path string) (Review, string, error) {
	b, err := readReviewSourceFile(path)
	if err != nil {
		return Review{}, "", err
	}
	var review Review
	if err := yaml.Unmarshal(b, &review); err != nil {
		return Review{}, "", err
	}
	return review, approvalCandidateDigest(b), nil
}

func LoadSourceIssueReview(path string) (SourceIssueReview, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return SourceIssueReview{}, err
	}
	var review SourceIssueReview
	if err := yaml.Unmarshal(b, &review); err != nil {
		return SourceIssueReview{}, err
	}
	return review, nil
}

func Apply(homePath string, store *mem.Store, bundle Bundle, sourcePath string, now time.Time) (Report, error) {
	if err := Validate(bundle); err != nil {
		return Report{ProductID: bundle.Product.ID, Status: "fail"}, err
	}
	report := Report{ProductID: bundle.Product.ID, Status: "ok", EntityID: bundle.Product.ID}
	evidence, err := store.RecordEvidence(mem.EvidenceItem{
		SourceKind: "repo",
		SourceID:   "product-registry:" + filepath.Base(sourcePath),
		QuoteText:  bundle.Product.Name + " product registry: " + bundle.Product.Positioning,
		Actor:      "tool",
	}, now)
	if err != nil {
		return report, err
	}
	report.EvidenceIDs = append(report.EvidenceIDs, evidence.ID)

	var keep map[string]bool
	var priorIdentifiers []string
	if bundle.contractSourced {
		if bundle, keep, priorIdentifiers, err = additiveContractBundle(store, bundle); err != nil {
			return report, err
		}
	}
	entity := mem.Entity{
		Slug:        bundle.Product.ID,
		Kind:        "product",
		Aliases:     compact(append([]string{bundle.Product.Name}, bundle.Product.Aliases...)),
		Identifiers: compact(append(priorIdentifiers, productIdentifiers(bundle.Product)...)),
		Parent:      bundle.Product.OwnerEntity,
		Visibility:  "operational",
	}
	if _, err := store.RegisterEntity(entity, now); err != nil {
		return report, err
	}

	var claims []mem.Candidate
	for _, c := range productClaims(bundle, evidence.ID, sourcePath) {
		if !keep[c.Predicate] {
			claims = append(claims, c)
		}
	}
	retractions, retracted, err := essenceRetractions(store, bundle, claims, sourcePath)
	if err != nil {
		return report, err
	}
	report.Retracted = retracted
	captured, err := store.Capture(append(claims, retractions...), now)
	if err != nil {
		return report, err
	}
	for _, c := range captured {
		res, err := store.Adjudicate(c.ID, candidates.Promote, "product-import", now)
		if err != nil {
			return report, err
		}
		if res.Claim != nil {
			report.ClaimIDs = append(report.ClaimIDs, res.Claim.ID)
		}
	}

	for _, signal := range bundle.Signals {
		if signal.ObservedAt == "" {
			signal.ObservedAt = now.UTC().Format(time.RFC3339)
		}
		if err := appendUniqueSignal(homePath, signal); err != nil {
			return report, err
		}
		report.SignalIDs = append(report.SignalIDs, signal.ID)
	}
	for _, hyp := range bundle.Hypotheses {
		c := hypothesisCandidate(hyp, now)
		if err := writeHypothesisCandidate(homePath, c); err != nil {
			return report, err
		}
		report.HypothesisIDs = append(report.HypothesisIDs, c.ID)
	}
	return report, nil
}

// additiveContractBundle folds a contract-sourced bundle onto what the ledger
// already records for its product. A project contract is the authority for
// where a product runs and how it is released. It is not the authority for how
// the product is positioned — its notes are a repository summary and
// data.classification is a storage class — and it does not know the surfaces,
// channels, aliases, identifiers, or owner other writers recorded. So its import
// adds and never removes: those sets are unioned with the current record, the
// recorded owner is kept, a contested fact is left under review, and
// positioning and privacy posture are seeded only when nothing records them yet.
// It returns the merged bundle, the predicates to leave untouched, and the
// entity identifiers already registered.
func additiveContractBundle(store *mem.Store, bundle Bundle) (Bundle, map[string]bool, []string, error) {
	id := bundle.Product.ID
	view, err := store.CurrentView()
	if err != nil {
		return bundle, nil, nil, err
	}
	current := map[string]mem.Claim{}
	for _, c := range view.Claims() {
		if c.Entity != id {
			continue
		}
		if prior, ok := current[c.Predicate]; !ok || c.Seq > prior.Seq {
			current[c.Predicate] = c
		}
	}
	keep := map[string]bool{}
	for _, predicate := range []string{"positioning", "privacy_posture"} {
		if strings.TrimSpace(current[predicate].Object) != "" {
			keep[predicate] = true
		}
	}
	// A fact under review is hidden from Claims(), so its slot reads empty here.
	// Superseding it would drop the recorded value and close the review with no
	// one deciding it; the contract leaves every contested slot untouched.
	for _, pos := range view.Positions() {
		if pos.Entity == id && pos.Status == "contested" {
			keep[pos.Predicate] = true
		}
	}
	p := bundle.Product
	var surfaces []Surface
	seen := map[string]bool{}
	for _, ref := range strings.Split(current["surfaces"].Object, ",") {
		if ref = strings.TrimSpace(ref); ref != "" && !seen[ref] {
			seen[ref] = true
			surfaces = append(surfaces, Surface{ID: "recorded", Kind: "recorded", Ref: ref})
		}
	}
	for _, s := range p.Surfaces {
		if !seen[s.Ref] {
			seen[s.Ref] = true
			surfaces = append(surfaces, s)
		}
	}
	p.Surfaces = surfaces
	p.Channels = compact(append(strings.Split(current["channels"].Object, ","), p.Channels...))

	entities, err := store.Entities()
	if err != nil {
		return bundle, nil, nil, err
	}
	var priorIdentifiers []string
	for _, e := range entities {
		if e.Slug == id {
			p.Aliases = compact(append(append([]string{}, e.Aliases...), p.Aliases...))
			priorIdentifiers = e.Identifiers
			if strings.TrimSpace(e.Parent) != "" {
				p.OwnerEntity = e.Parent
			}
		}
	}
	bundle.Product = p
	return bundle, keep, priorIdentifiers, nil
}

func ApplySignal(homePath string, store *mem.Store, doc SignalDocument, sourcePath string, now time.Time) (Report, error) {
	if err := ValidateSignalDocument(doc); err != nil {
		return Report{ProductID: doc.Signal.ProductID, Status: "fail"}, err
	}
	signal := doc.Signal
	if signal.ObservedAt == "" {
		signal.ObservedAt = now.UTC().Format(time.RFC3339)
	}
	evidence, err := store.RecordEvidence(mem.EvidenceItem{
		SourceKind: "repo",
		SourceID:   "product-signal:" + filepath.Base(sourcePath) + ":" + doc.Source.Kind + ":" + doc.Source.Ref,
		QuoteText:  fmt.Sprintf("%s %s %.4g -> %.4g (%s)", signal.ProductID, signal.MetricName, signal.Before, signal.After, signal.Window),
		Actor:      "tool",
	}, now)
	if err != nil {
		return Report{}, err
	}
	signal.EvidenceRefs = compact(append(append(signal.EvidenceRefs, evidence.ID), doc.EvidenceRefs...))
	persistedSignal, err := persistUniqueSignal(homePath, signal)
	if err != nil {
		return Report{}, err
	}
	// A replay with no source observed_at receives the original ingestion time
	// from the persisted signal. That keeps the stable transition id paired with
	// byte-identical transition content across legitimate retries.
	signal = persistedSignal
	transitionAt, err := time.Parse(time.RFC3339, signal.ObservedAt)
	if err != nil {
		return Report{}, fmt.Errorf("product signal %q observed_at %q: %w", signal.ID, signal.ObservedAt, err)
	}
	tr, err := BuildLoopTransition(LoopTransitionInput{
		Kind:         "product_signal",
		ProductID:    signal.ProductID,
		ObjectID:     signal.ID,
		SourceTrace:  "product_signal:" + signal.ID,
		BeforeRefs:   doc.EvidenceRefs,
		AfterRefs:    append([]string{"product_signal:" + signal.ID, evidence.ID}, signal.EvidenceRefs...),
		EvidenceRefs: signal.EvidenceRefs,
		Summary:      fmt.Sprintf("product signal %s recorded; no product surface changed", signal.ID),
	}, transitionAt)
	if err != nil {
		return Report{}, err
	}
	if err := RecordTransition(homePath, tr); err != nil {
		return Report{}, err
	}
	return Report{
		ProductID:     signal.ProductID,
		Status:        "ok",
		EvidenceIDs:   []string{evidence.ID},
		SignalIDs:     []string{signal.ID},
		TransitionIDs: []string{tr.ID},
	}, nil
}

func recordReviewArtifactResult(homePath string, review Review, sourcePath string, report ReviewReport, now time.Time) error {
	stem := reviewArtifactStem(sourcePath)
	if stem == "" {
		return nil
	}
	record := ReviewArtifactRecord{
		SchemaVersion: "compound.product_review_record.v0.1",
		ID:            reviewartifact.ID("prr_", stem, report.ReviewID, report.EvidenceID, report.TransitionID),
		CreatedAt:     reviewartifact.Timestamp(now),
		Status:        "recorded",
		ProductID:     report.ProductID,
		HypothesisID:  report.HypothesisID,
		ReviewID:      report.ReviewID,
		Result:        report.Result,
		ReviewedBy:    review.ReviewedBy,
		EvidenceID:    report.EvidenceID,
		EvidenceRefs:  reviewartifact.Unique(report.EvidenceRefs),
		TransitionID:  report.TransitionID,
		ReviewPath:    strings.TrimSpace(sourcePath),
		Warnings: []string{
			"does not declare causality",
			"does not mutate product surfaces",
			"does not promote memory claims",
		},
	}
	if _, err := reviewartifact.WriteJSON(homePath, "product-reviews", stem+"."+record.ID+".reviewed.json", record, " "); err != nil {
		return err
	}
	plan, err := loadReviewPlanArtifact(homePath, stem)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if plan.ID != stem || plan.ProductID != report.ProductID || plan.HypothesisID != report.HypothesisID || plan.ReviewDraft.ReviewID != report.ReviewID {
		return fmt.Errorf("review plan does not bind the committed review's product, hypothesis and review identity")
	}
	if plan.ReviewID != "" {
		if plan.ReviewID != report.ReviewID || plan.ReviewResult != report.Result || plan.ReviewEvidenceID != report.EvidenceID || plan.ReviewTransitionID != report.TransitionID {
			return fmt.Errorf("review plan already records a different committed result; recovery leaves it unchanged")
		}
		// JSON may have committed before Markdown failed. Repair the derived
		// sibling from the CURRENT plan, without resetting a later plan status.
		_, err := reviewartifact.WriteRaw(homePath, "product-reviews", stem+".md", []byte(RenderReviewPlan(plan)))
		return err
	}
	plan.Status = "recorded"
	plan.ReviewedAt = record.CreatedAt
	plan.ReviewID = report.ReviewID
	plan.ReviewResult = report.Result
	plan.ReviewEvidenceID = report.EvidenceID
	plan.ReviewTransitionID = report.TransitionID
	_, err = reviewartifact.Write(homePath, "product-reviews", plan.ID, plan, " ", RenderReviewPlan(plan))
	return err
}

func reviewArtifactStem(sourcePath string) string {
	base := filepath.Base(strings.TrimSpace(sourcePath))
	if !strings.HasSuffix(base, ".review.yaml") {
		return ""
	}
	return strings.TrimSuffix(base, ".review.yaml")
}

func loadReviewPlanArtifact(homePath, stem string) (ReviewPlan, error) {
	path := filepath.Join(homePath, "reviews", "product-reviews", stem+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ReviewPlan{}, err
	}
	var plan ReviewPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return ReviewPlan{}, err
	}
	if plan.ID == "" {
		return ReviewPlan{}, fmt.Errorf("product review plan %s missing id", path)
	}
	return plan, nil
}

func ApplySourceIssueReview(homePath string, store *mem.Store, review SourceIssueReview, sourcePath string, now time.Time) (SourceIssueReviewReport, error) {
	if err := ValidateSourceIssueReview(review); err != nil {
		return SourceIssueReviewReport{ProductID: review.ProductID, IssueID: review.IssueID, ReviewID: review.ID, Status: "fail"}, err
	}
	issues, err := loadProductSourceIssues(homePath, review.ProductID)
	if err != nil {
		return SourceIssueReviewReport{}, err
	}
	var issue ProductSourceIssue
	for _, candidate := range issues {
		if candidate.ID == review.IssueID {
			issue = candidate
			break
		}
	}
	if issue.ID == "" {
		return SourceIssueReviewReport{}, fmt.Errorf("product source issue %q not found for %s", review.IssueID, review.ProductID)
	}
	if issue.Status == "closed" {
		return SourceIssueReviewReport{}, fmt.Errorf("product source issue %q already closed by review %q", issue.ID, issue.ReviewID)
	}
	if review.ReviewedAt == "" {
		review.ReviewedAt = now.UTC().Format(time.RFC3339)
	}
	review.AppliedAt = now.UTC().Format(time.RFC3339)
	review.SourceRef = "product-source-issue-review:" + filepath.Base(sourcePath)
	evidence, err := store.RecordEvidence(mem.EvidenceItem{
		SourceKind: "repo",
		SourceID:   review.SourceRef,
		QuoteText:  review.Rationale,
		Actor:      "tool",
	}, now)
	if err != nil {
		return SourceIssueReviewReport{}, err
	}
	review.EvidenceID = evidence.ID
	review.EvidenceRefs = compact(append(review.EvidenceRefs, evidence.ID, "product_source_issue:"+issue.ID))
	var hypothesisID string
	afterRefs := compact(append([]string{"product_source_issue:" + issue.ID + ":" + review.Result, "product_source_issue_review:" + review.ID, evidence.ID}, review.EvidenceRefs...))
	if review.FollowUp != nil {
		candidate := sourceIssueFollowUpHypothesis(review, now)
		if err := writeHypothesisCandidate(homePath, candidate); err != nil {
			return SourceIssueReviewReport{}, err
		}
		hypothesisID = candidate.ID
		afterRefs = compact(append(afterRefs, "product_hypothesis:"+candidate.ID))
	}
	if err := appendUniqueSourceIssueReview(homePath, review); err != nil {
		return SourceIssueReviewReport{}, err
	}
	tr, err := BuildLoopTransition(LoopTransitionInput{
		Kind:         "product_source_issue_review",
		ProductID:    review.ProductID,
		ObjectID:     review.ID,
		SourceTrace:  "product_source_issue_review:" + review.ID,
		BeforeRefs:   compact(append([]string{"product_source_issue:" + issue.ID}, issue.EvidenceRefs...)),
		AfterRefs:    afterRefs,
		EvidenceRefs: review.EvidenceRefs,
		Summary:      fmt.Sprintf("product source issue review %s closed issue %s as %s; no product surface changed", review.ID, issue.ID, review.Result),
	}, now)
	if err != nil {
		return SourceIssueReviewReport{}, err
	}
	if err := RecordTransition(homePath, tr); err != nil {
		return SourceIssueReviewReport{}, err
	}
	return SourceIssueReviewReport{
		Status:       "closed",
		ProductID:    review.ProductID,
		IssueID:      review.IssueID,
		ReviewID:     review.ID,
		Result:       review.Result,
		EvidenceID:   evidence.ID,
		EvidenceRefs: review.EvidenceRefs,
		TransitionID: tr.ID,
		HypothesisID: hypothesisID,
	}, nil
}

func Validate(bundle Bundle) error {
	if bundle.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %s, got %q", SchemaVersion, bundle.SchemaVersion)
	}
	p := bundle.Product
	if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("product id and name required")
	}
	if strings.TrimSpace(p.Positioning) == "" || strings.TrimSpace(p.PrivacyPosture) == "" {
		return fmt.Errorf("product positioning and privacy_posture required")
	}
	if len(p.Surfaces) == 0 || len(p.Channels) == 0 {
		return fmt.Errorf("product surfaces and channels required")
	}
	if len(bundle.EvidenceRefs) == 0 {
		return fmt.Errorf("evidence_refs required")
	}
	if err := validateEssence(p); err != nil {
		return err
	}
	for _, s := range bundle.Signals {
		if err := validateSignal(s); err != nil || s.ProductID != p.ID {
			if err != nil {
				return err
			}
			return fmt.Errorf("invalid signal %q", s.ID)
		}
	}
	for _, h := range bundle.Hypotheses {
		if h.ProductID != p.ID || strings.TrimSpace(h.ID) == "" || strings.TrimSpace(h.ExpectedEffect) == "" || len(h.EvidenceRefs) == 0 {
			return fmt.Errorf("invalid hypothesis %q", h.ID)
		}
		if h.Result != "" && !oneOf(h.Result, []string{"unknown", "supported", "refuted", "inconclusive"}) {
			return fmt.Errorf("hypothesis %q bad result %q", h.ID, h.Result)
		}
	}
	return nil
}

func validateSignal(s Signal) error {
	if strings.TrimSpace(s.ID) == "" || strings.TrimSpace(s.ProductID) == "" || strings.TrimSpace(s.MetricName) == "" {
		return fmt.Errorf("invalid signal %q", s.ID)
	}
	if len(s.EvidenceRefs) == 0 {
		return fmt.Errorf("signal %q evidence_refs required", s.ID)
	}
	if s.Confidence < 0 || s.Confidence > 1 {
		return fmt.Errorf("signal %q confidence must be between 0 and 1", s.ID)
	}
	return nil
}

func ValidateSignalDocument(doc SignalDocument) error {
	if doc.SchemaVersion != SignalSchemaVersion {
		return fmt.Errorf("schema_version must be %s, got %q", SignalSchemaVersion, doc.SchemaVersion)
	}
	if strings.TrimSpace(doc.Source.Kind) == "" || strings.TrimSpace(doc.Source.Ref) == "" {
		return fmt.Errorf("signal source kind ref required")
	}
	if err := validateSignal(doc.Signal); err != nil {
		return err
	}
	return nil
}

func AdaptSignalExport(doc SignalExportDocument) (SignalDocument, error) {
	if err := ValidateSignalExportDocument(doc); err != nil {
		return SignalDocument{}, err
	}
	metricName := strings.TrimSpace(doc.Metric.CanonicalName)
	if metricName == "" {
		metricName = canonicalMetricName(doc.Source.Kind, doc.Metric.Name)
	}
	id := strings.TrimSpace(doc.SignalID)
	if id == "" {
		id = stableID("sig_", doc.ProductID, doc.Source.Kind, doc.Source.Ref, metricName, doc.ChangeEvent, doc.Window)
	}
	evidenceRefs := compact(append([]string{}, doc.EvidenceRefs...))
	evidenceRefs = compact(append(evidenceRefs, doc.Metric.Before.Ref, doc.Metric.After.Ref))
	return SignalDocument{
		SchemaVersion: SignalSchemaVersion,
		Source:        doc.Source,
		Signal: Signal{
			ID:              id,
			ProductID:       doc.ProductID,
			Surface:         doc.Surface,
			ChangeEvent:     doc.ChangeEvent,
			MetricName:      metricName,
			Before:          doc.Metric.Before.Value,
			After:           doc.Metric.After.Value,
			Window:          doc.Window,
			Confidence:      doc.Confidence,
			Confounders:     doc.Confounders,
			Interpretations: doc.Interpretations,
			NextAction:      doc.NextAction,
			EvidenceRefs:    evidenceRefs,
			ObservedAt:      doc.ObservedAt,
		},
		EvidenceRefs: evidenceRefs,
	}, nil
}

func adaptASCDatapointsSignal(doc ascDatapointsDocument, opts ASCDatapointsSignalOptions) (SignalDocument, error) {
	productID := strings.TrimSpace(opts.ProductID)
	appID := strings.TrimSpace(opts.AppID)
	metric := strings.TrimSpace(opts.Metric)
	if metric == "" {
		metric = "review_count"
	}
	if productID == "" || appID == "" {
		return SignalDocument{}, fmt.Errorf("ASC datapoints product_id and app_id required")
	}
	row, ok := findASCRow(doc.Rows, appID)
	if !ok {
		return SignalDocument{}, fmt.Errorf("ASC datapoints app_id %q not found", appID)
	}
	value, err := ascMetricValue(row, metric)
	if err != nil {
		return SignalDocument{}, err
	}
	date := firstNonEmpty(row.Date, doc.Date)
	if date == "" {
		return SignalDocument{}, fmt.Errorf("ASC datapoints date required")
	}
	observedAt := firstNonEmpty(row.CapturedAt, doc.CapturedAt)
	metricName := canonicalMetricName("app_store_connect", metric)
	sourceRef := "asc:datapoints:" + appID + ":" + date
	evidenceRefs := compact([]string{
		"asc:datapoints:" + date + ":" + appID,
		"asc:app:" + appID,
		"asc:bundle:" + strings.TrimSpace(row.BundleID),
		"asc:state:" + normalizeIDPart(row.CurrentAppStoreState),
		fmt.Sprintf("asc:%s:%.4g", normalizeIDPart(metric), value),
	})
	evidenceRefs = append(evidenceRefs, ascRatingCountRefs(row.RatingCounts)...)
	evidenceRefs = compact(evidenceRefs)
	return SignalDocument{
		SchemaVersion: SignalSchemaVersion,
		Source: SignalSource{
			Kind: "app_store_connect",
			Ref:  sourceRef,
		},
		Signal: Signal{
			ID:          stableID("sig_", productID, "app_store_connect", appID, metricName, date),
			ProductID:   productID,
			Surface:     "app_store",
			ChangeEvent: "Apple App Store Connect datapoints snapshot observed",
			MetricName:  metricName,
			Before:      value,
			After:       value,
			Window:      "snapshot " + date,
			Confidence:  0.72,
			Confounders: []string{
				"snapshot observation; no causal movement is implied",
				"provider state can lag App Store Connect",
			},
			Interpretations: []Interpretation{{
				Lens:  "provider_snapshot",
				Claim: "records observed App Store Connect metric without causal attribution",
			}},
			NextAction:   "Use as product feedback evidence; do not infer causality without a reviewed hypothesis.",
			EvidenceRefs: evidenceRefs,
			ObservedAt:   observedAt,
		},
		EvidenceRefs: evidenceRefs,
	}, nil
}

func adaptASCSalesSeriesSignal(rows []ascSalesSeriesRow, opts ASCSalesSeriesSignalOptions) (SignalDocument, error) {
	productID := strings.TrimSpace(opts.ProductID)
	appID := strings.TrimSpace(opts.AppID)
	metric := strings.TrimSpace(opts.Metric)
	if metric == "" {
		metric = "units"
	}
	changeEvent := strings.TrimSpace(opts.ChangeEvent)
	if productID == "" || appID == "" || changeEvent == "" {
		return SignalDocument{}, fmt.Errorf("ASC sales series product_id app_id and change_event required")
	}
	beforeRange, err := parseDateRange(opts.BeforeWindow)
	if err != nil {
		return SignalDocument{}, fmt.Errorf("ASC sales series before window: %w", err)
	}
	afterRange, err := parseDateRange(opts.AfterWindow)
	if err != nil {
		return SignalDocument{}, fmt.Errorf("ASC sales series after window: %w", err)
	}
	before, err := aggregateSalesWindow(rows, appID, metric, beforeRange)
	if err != nil {
		return SignalDocument{}, err
	}
	after, err := aggregateSalesWindow(rows, appID, metric, afterRange)
	if err != nil {
		return SignalDocument{}, err
	}
	metricName := canonicalMetricName("app_store_connect", metric)
	sourceRef := "asc:sales_series:" + appID + ":" + beforeRange.Raw + ":" + afterRange.Raw
	evidenceRefs := compact([]string{
		"asc:sales_series:" + appID,
		"asc:sales_series:metric:" + normalizeIDPart(metric),
		"asc:sales_series:before:" + beforeRange.Raw,
		"asc:sales_series:after:" + afterRange.Raw,
		fmt.Sprintf("asc:sales_series:before_rows:%d", before.Rows),
		fmt.Sprintf("asc:sales_series:after_rows:%d", after.Rows),
		"asc:sales_series:before_sum:" + shortFloat(before.Sum),
		"asc:sales_series:after_sum:" + shortFloat(after.Sum),
	})
	return SignalDocument{
		SchemaVersion: SignalSchemaVersion,
		Source: SignalSource{
			Kind: "app_store_connect",
			Ref:  sourceRef,
		},
		Signal: Signal{
			ID:          stableID("sig_", productID, "app_store_connect", appID, metricName, beforeRange.Raw, afterRange.Raw, changeEvent),
			ProductID:   productID,
			Surface:     "app_store",
			ChangeEvent: changeEvent,
			MetricName:  metricName,
			Before:      before.Sum,
			After:       after.Sum,
			Window:      "before " + beforeRange.Raw + "; after " + afterRange.Raw,
			Confidence:  0.64,
			Confounders: []string{
				"explicit window comparison; causality is not implied",
				"seasonality, channel mix, price, reviews, and release timing may confound movement",
			},
			Interpretations: []Interpretation{{
				Lens:  "window_comparison",
				Claim: "records before/after App Store Connect metric movement for review, not proof of causality",
			}},
			NextAction:   "Review against the experiment ledger and confounders before promoting any product hypothesis.",
			EvidenceRefs: evidenceRefs,
			ObservedAt:   afterRange.End.Format("2006-01-02") + "T00:00:00Z",
		},
		EvidenceRefs: evidenceRefs,
	}, nil
}

func ValidateSignalExportDocument(doc SignalExportDocument) error {
	if doc.SchemaVersion != SignalExportSchemaVersion {
		return fmt.Errorf("schema_version must be %s, got %q", SignalExportSchemaVersion, doc.SchemaVersion)
	}
	if strings.TrimSpace(doc.Source.Kind) == "" || strings.TrimSpace(doc.Source.Ref) == "" {
		return fmt.Errorf("signal export source kind ref required")
	}
	if _, ok := signalExportProviderAllowlist[strings.TrimSpace(doc.Source.Kind)]; !ok {
		return fmt.Errorf("signal export source kind %q unsupported", doc.Source.Kind)
	}
	if strings.TrimSpace(doc.ProductID) == "" || strings.TrimSpace(doc.Surface) == "" || strings.TrimSpace(doc.ChangeEvent) == "" {
		return fmt.Errorf("signal export product_id surface change_event required")
	}
	if strings.TrimSpace(doc.Metric.Name) == "" {
		return fmt.Errorf("signal export metric name required")
	}
	if strings.TrimSpace(doc.Window) == "" {
		return fmt.Errorf("signal export window required")
	}
	if doc.Confidence < 0 || doc.Confidence > 1 {
		return fmt.Errorf("signal export confidence must be between 0 and 1")
	}
	if len(doc.EvidenceRefs) == 0 {
		return fmt.Errorf("signal export evidence_refs required")
	}
	if strings.TrimSpace(doc.SignalID) != "" && strings.ContainsAny(doc.SignalID, `/\`) {
		return fmt.Errorf("signal export signal_id %q must not contain path separators", doc.SignalID)
	}
	return nil
}

func findASCRow(rows []ascDatapointsRow, appID string) (ascDatapointsRow, bool) {
	appID = strings.TrimSpace(appID)
	for _, row := range rows {
		if strings.TrimSpace(row.ASCAppID) == appID {
			return row, true
		}
	}
	return ascDatapointsRow{}, false
}

func ascMetricValue(row ascDatapointsRow, metric string) (float64, error) {
	switch normalizeIDPart(metric) {
	case "review_count":
		return row.ReviewCount, nil
	case "average_rating":
		if row.AverageRating == nil {
			return 0, fmt.Errorf("ASC datapoints average_rating missing for app_id %q", row.ASCAppID)
		}
		return *row.AverageRating, nil
	case "analytics_request_count":
		return row.AnalyticsRequestCount, nil
	case "rating_count_1", "rating_count_2", "rating_count_3", "rating_count_4", "rating_count_5":
		key := strings.TrimPrefix(normalizeIDPart(metric), "rating_count_")
		if row.RatingCounts == nil {
			return 0, fmt.Errorf("ASC datapoints rating_counts missing for app_id %q", row.ASCAppID)
		}
		return row.RatingCounts[key], nil
	default:
		return 0, fmt.Errorf("ASC datapoints metric %q unsupported", metric)
	}
}

func ascRatingCountRefs(counts map[string]float64) []string {
	var refs []string
	for _, key := range []string{"1", "2", "3", "4", "5"} {
		value, ok := counts[key]
		if !ok {
			continue
		}
		refs = append(refs, fmt.Sprintf("asc:rating_counts:%s:%.4g", key, value))
	}
	return refs
}

func parseASCSalesSeriesRows(b []byte, source string) ([]ascSalesSeriesRow, error) {
	var rows []ascSalesSeriesRow
	for i, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row ascSalesSeriesRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parse ASC sales series %s line %d: %w", source, i+1, err)
		}
		if strings.TrimSpace(row.AppleIdentifier) == "" || strings.TrimSpace(row.Date) == "" {
			return nil, fmt.Errorf("ASC sales series %s line %d missing apple_identifier/date", source, i+1)
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("ASC sales series %s has no rows", source)
	}
	return rows, nil
}

func parseDateRange(raw string) (dateRange, error) {
	raw = strings.TrimSpace(raw)
	parts := strings.Split(raw, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return dateRange{}, fmt.Errorf("must be YYYY-MM-DD/YYYY-MM-DD")
	}
	start, err := time.Parse("2006-01-02", strings.TrimSpace(parts[0]))
	if err != nil {
		return dateRange{}, err
	}
	end, err := time.Parse("2006-01-02", strings.TrimSpace(parts[1]))
	if err != nil {
		return dateRange{}, err
	}
	if end.Before(start) {
		return dateRange{}, fmt.Errorf("end before start")
	}
	return dateRange{Raw: start.Format("2006-01-02") + "/" + end.Format("2006-01-02"), Start: start, End: end}, nil
}

func aggregateSalesWindow(rows []ascSalesSeriesRow, appID, metric string, window dateRange) (salesWindowAggregate, error) {
	var out salesWindowAggregate
	for _, row := range rows {
		if strings.TrimSpace(row.AppleIdentifier) != strings.TrimSpace(appID) {
			continue
		}
		day, err := time.Parse("2006-01-02", strings.TrimSpace(row.Date))
		if err != nil {
			return salesWindowAggregate{}, fmt.Errorf("ASC sales series row date %q: %w", row.Date, err)
		}
		if day.Before(window.Start) || day.After(window.End) {
			continue
		}
		value, err := ascSalesMetricValue(row, metric)
		if err != nil {
			return salesWindowAggregate{}, err
		}
		out.Sum += value
		out.Rows++
		if out.Start == "" || row.Date < out.Start {
			out.Start = row.Date
		}
		if row.Date > out.End {
			out.End = row.Date
		}
	}
	if out.Rows == 0 {
		return salesWindowAggregate{}, fmt.Errorf("ASC sales series app_id %q has no rows in %s", appID, window.Raw)
	}
	return out, nil
}

func ascSalesMetricValue(row ascSalesSeriesRow, metric string) (float64, error) {
	switch normalizeIDPart(metric) {
	case "units":
		return row.Units, nil
	case "app_units":
		return row.AppUnits, nil
	case "iap_units":
		return row.IAPUnits, nil
	case "customer_sales_usd_est":
		return row.CustomerSalesUSDEst, nil
	case "developer_proceeds_usd":
		return row.DeveloperProceedsUSD, nil
	default:
		return 0, fmt.Errorf("ASC sales series metric %q unsupported", metric)
	}
}

func shortFloat(v float64) string {
	return fmt.Sprintf("%.4g", v)
}

func ValidateReview(review Review) error {
	if review.SchemaVersion != ReviewSchemaVersion {
		return fmt.Errorf("schema_version must be %s, got %q", ReviewSchemaVersion, review.SchemaVersion)
	}
	if strings.TrimSpace(review.ID) == "" || strings.TrimSpace(review.ProductID) == "" || strings.TrimSpace(review.HypothesisID) == "" {
		return fmt.Errorf("review id product_id hypothesis_id required")
	}
	if strings.ContainsAny(review.HypothesisID, `/\`) {
		return fmt.Errorf("review hypothesis_id %q must not contain path separators", review.HypothesisID)
	}
	if !oneOf(review.Result, []string{"supported", "refuted", "inconclusive"}) {
		return fmt.Errorf("review result must be supported|refuted|inconclusive, got %q", review.Result)
	}
	if strings.TrimSpace(review.Rationale) == "" {
		return fmt.Errorf("review rationale required")
	}
	if review.Confidence < 0 || review.Confidence > 1 {
		return fmt.Errorf("review confidence must be between 0 and 1")
	}
	if len(review.EvidenceRefs) == 0 {
		return fmt.Errorf("review evidence_refs required")
	}
	if strings.TrimSpace(review.ReviewedBy) == "" || strings.Contains(review.ReviewedBy, "TODO") {
		return fmt.Errorf("review reviewed_by required (name the judge; agent lanes annotate, operators adjudicate)")
	}
	return nil
}

func ValidateSourceIssueReview(review SourceIssueReview) error {
	if review.SchemaVersion != SourceIssueReviewSchemaVersion {
		return fmt.Errorf("schema_version must be %s, got %q", SourceIssueReviewSchemaVersion, review.SchemaVersion)
	}
	if strings.TrimSpace(review.ID) == "" || strings.TrimSpace(review.ProductID) == "" || strings.TrimSpace(review.IssueID) == "" {
		return fmt.Errorf("source issue review id product_id issue_id required")
	}
	if strings.ContainsAny(review.IssueID, `/\`) {
		return fmt.Errorf("source issue review issue_id %q must not contain path separators", review.IssueID)
	}
	if !oneOf(review.Result, []string{"resolved", "waived", "false_positive"}) {
		return fmt.Errorf("source issue review result must be resolved|waived|false_positive, got %q", review.Result)
	}
	if strings.TrimSpace(review.Rationale) == "" {
		return fmt.Errorf("source issue review rationale required")
	}
	if len(review.EvidenceRefs) == 0 {
		return fmt.Errorf("source issue review evidence_refs required")
	}
	if review.FollowUp != nil {
		if review.Result != "resolved" {
			return fmt.Errorf("source issue review follow_up requires result resolved")
		}
		if strings.TrimSpace(review.FollowUp.ChangeEvent) == "" ||
			strings.TrimSpace(review.FollowUp.ExpectedEffect) == "" ||
			strings.TrimSpace(review.FollowUp.ObservationWindow) == "" ||
			strings.TrimSpace(review.FollowUp.NextAction) == "" ||
			len(compact(review.FollowUp.Metrics)) == 0 {
			return fmt.Errorf("source issue review follow_up requires change_event expected_effect metrics observation_window next_action")
		}
	}
	return nil
}

func LoadTimeline(homePath, productID string) (Timeline, error) {
	signals, err := loadSignals(homePath)
	if err != nil {
		return Timeline{}, err
	}
	hyps, err := loadHypotheses(homePath)
	if err != nil {
		return Timeline{}, err
	}
	transitions, err := loadProductTransitions(homePath, productID)
	if err != nil {
		return Timeline{}, err
	}
	sourceIssues, err := loadProductSourceIssues(homePath, productID)
	if err != nil {
		return Timeline{}, err
	}
	tl := Timeline{ProductID: productID}
	for _, s := range signals {
		if s.ProductID == productID {
			tl.Signals = append(tl.Signals, s)
		}
	}
	for _, h := range hyps {
		if h.ProductID == productID {
			tl.Hypotheses = append(tl.Hypotheses, h)
		}
	}
	tl.Transitions = transitions
	tl.SourceIssues = sourceIssues
	sort.Slice(tl.Signals, func(i, j int) bool { return tl.Signals[i].ID < tl.Signals[j].ID })
	sort.Slice(tl.Hypotheses, func(i, j int) bool { return tl.Hypotheses[i].ID < tl.Hypotheses[j].ID })
	return tl, nil
}

func Doctor(homePath, productID string) (DoctorReport, error) {
	return DoctorWithNow(homePath, productID, time.Now())
}

func DoctorWithNow(homePath, productID string, now time.Time) (DoctorReport, error) {
	return DoctorWithOptions(homePath, productID, DoctorOptions{Now: now})
}

type DoctorOptions struct {
	Now                  time.Time
	RegisteredASCAppRefs []string
}

func DoctorForRegisteredProduct(homePath string, store *mem.Store, productID string, now time.Time) (DoctorReport, error) {
	refs, err := registeredASCAppRefs(store, productID)
	if err != nil {
		return DoctorReport{}, err
	}
	return DoctorWithOptions(homePath, productID, DoctorOptions{Now: now, RegisteredASCAppRefs: refs})
}

func DoctorWithOptions(homePath, productID string, opts DoctorOptions) (DoctorReport, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	tl, err := LoadTimeline(homePath, productID)
	if err != nil {
		return DoctorReport{}, err
	}
	report := DoctorReport{
		ProductID:       productID,
		Status:          "ok",
		SignalCount:     len(tl.Signals),
		HypothesisCount: len(tl.Hypotheses),
		SourceIssues:    openProductSourceIssues(tl.SourceIssues),
	}
	for _, h := range tl.Hypotheses {
		if len(h.EvidenceRefs) == 0 {
			report.MissingEvidenceRefs = append(report.MissingEvidenceRefs, h.ID)
			report.Status = "needs_review"
		}
		if h.Verdict == candidates.Park && h.Result == "unknown" && strings.TrimSpace(h.NextAction) != "" {
			report.PendingReview = append(report.PendingReview, h)
		}
	}
	report.PendingReviewCount = len(report.PendingReview)
	report.ReviewReady = reviewReadySignals(report.PendingReview, tl.Signals)
	report.ReviewReadyCount = len(report.ReviewReady)
	report.SourceIssueCount = len(report.SourceIssues)
	report.ASCSignalStaleness = staleASCSignals(tl.Signals, productID, now, opts.RegisteredASCAppRefs)
	unattributedIDs, agentAnnotatedIDs, err := scanProductJudgmentProvenance(homePath, productID)
	if err != nil {
		return DoctorReport{}, err
	}
	report.UnattributedJudgmentIDs = unattributedIDs
	report.UnattributedJudgments = len(unattributedIDs)
	report.AgentAnnotatedJudgmentIDs = agentAnnotatedIDs
	report.AgentAnnotatedJudgments = len(agentAnnotatedIDs)
	if report.PendingReviewCount > 0 || report.SourceIssueCount > 0 {
		report.Status = "needs_review"
	}
	if report.Status == "ok" && len(report.ASCSignalStaleness) > 0 {
		report.Status = "warn"
	}
	return report, nil
}

// OperatorAdjudicated reports whether a product-judgment actor represents an
// operator adjudication rather than an agent annotation. It mirrors the panel
// sidecar rule (664e1a7 / the 2026-07-02 provenance ruling): an unattributed or
// agent-attributed judgment annotates the record but never clears an
// operator-gated pending/approval state. Only an explicit operator role token
// (or the configured operator identity) adjudicates; every other named actor —
// including agent lanes like codex/claude — is an annotation.
func OperatorAdjudicated(actor string) bool {
	a := strings.ToLower(strings.TrimSpace(actor))
	if a == "" {
		return false
	}
	for _, token := range operatorActorTokens() {
		if a == strings.ToLower(strings.TrimSpace(token)) {
			return true
		}
	}
	return false
}

// operatorActorTokens is the set of actor strings that count as an operator
// adjudication. It stays identity-free for the public repo (role tokens only)
// but honors the same env override the panel lane uses.
func operatorActorTokens() []string {
	tokens := []string{"operator", "owner", "human"}
	for _, key := range []string{"COMPOUND_OPERATOR_ID", "COMPOUND_REVIEWED_BY"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			tokens = append(tokens, v)
		}
	}
	return tokens
}

// scanProductJudgmentProvenance reads (never rewrites) the product-review
// records on disk for productID and buckets them by attribution:
// unattributed (no actor) and agent-annotated (a named actor that is not an
// operator adjudication). Records missing/unreadable are skipped, not mutated.
func scanProductJudgmentProvenance(homePath, productID string) (unattributed, agentAnnotated []string, err error) {
	dir := filepath.Join(homePath, "reviews", "product-reviews")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".reviewed.json") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			return nil, nil, readErr
		}
		var rec ReviewArtifactRecord
		if json.Unmarshal(data, &rec) != nil {
			continue
		}
		if rec.SchemaVersion != "compound.product_review_record.v0.1" || rec.ProductID != productID {
			continue
		}
		id := rec.ID
		if id == "" {
			id = strings.TrimSuffix(entry.Name(), ".reviewed.json")
		}
		if strings.TrimSpace(rec.ReviewedBy) == "" {
			unattributed = append(unattributed, id)
			continue
		}
		if !OperatorAdjudicated(rec.ReviewedBy) {
			agentAnnotated = append(agentAnnotated, id)
		}
	}
	sort.Strings(unattributed)
	sort.Strings(agentAnnotated)
	return unattributed, agentAnnotated, nil
}

func NeedsWriteback(report DoctorReport) bool {
	return report.PendingReviewCount > 0 || report.SourceIssueCount > 0 || len(report.MissingEvidenceRefs) > 0
}

func EvidenceRefs(report DoctorReport) []string {
	refs := []string{
		"product_id:" + report.ProductID,
		fmt.Sprintf("product:signals:%d", report.SignalCount),
		fmt.Sprintf("product:hypotheses:%d", report.HypothesisCount),
		fmt.Sprintf("product:pending_review:%d", report.PendingReviewCount),
		fmt.Sprintf("product:source_issues:%d", report.SourceIssueCount),
	}
	for _, h := range report.PendingReview {
		refs = append(refs, "product_hypothesis:"+h.ID+":"+h.Result)
		refs = append(refs, h.EvidenceRefs...)
	}
	for _, ready := range report.ReviewReady {
		refs = append(refs, "product_review_ready:"+ready.HypothesisID+":"+ready.SignalID)
		refs = append(refs, ready.EvidenceRefs...)
	}
	for _, issue := range report.SourceIssues {
		refs = append(refs, "product_source_issue:"+issue.ID, issue.SourceRef)
		refs = append(refs, issue.EvidenceRefs...)
	}
	for _, id := range report.MissingEvidenceRefs {
		refs = append(refs, "product_hypothesis_missing_evidence:"+id)
	}
	return compact(refs)
}

func registeredASCAppRefs(store *mem.Store, productID string) ([]string, error) {
	if store == nil {
		return nil, nil
	}
	entities, err := store.Entities()
	if err != nil {
		return nil, fmt.Errorf("load product registry entities: %w", err)
	}
	var refs []string
	for _, entity := range entities {
		if entity.Slug != productID || entity.Kind != "product" {
			continue
		}
		for _, id := range entity.Identifiers {
			id = strings.TrimSpace(id)
			if strings.HasPrefix(id, "appstore:") || strings.HasPrefix(id, "asc:app:") {
				refs = append(refs, id)
			}
		}
	}
	return compact(refs), nil
}

func staleASCSignals(signals []Signal, productID string, now time.Time, registeredASCAppRefs []string) []ASCSignalStaleness {
	var latest Signal
	var latestAt time.Time
	for _, signal := range signals {
		if signal.ProductID != productID || !isASCSignal(signal) {
			continue
		}
		observedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(signal.ObservedAt))
		if err != nil {
			continue
		}
		if latest.ID == "" || observedAt.After(latestAt) {
			latest = signal
			latestAt = observedAt
		}
	}
	if latest.ID == "" {
		if len(registeredASCAppRefs) == 0 {
			return nil
		}
		return []ASCSignalStaleness{{
			ProductID:  productID,
			Status:     "warn",
			Reason:     "no_asc_signal_recorded",
			FixCommand: ascDailyFixCommand(),
		}}
	}
	if now.Before(latestAt) {
		return nil
	}
	age := now.Sub(latestAt)
	if age <= ascSignalFreshnessWindow {
		return nil
	}
	return []ASCSignalStaleness{{
		ProductID:      productID,
		LastSignalID:   latest.ID,
		LastObservedAt: latestAt.UTC().Format(time.RFC3339),
		AgeHours:       int(age.Hours()),
		Status:         "warn",
		Reason:         "last_asc_signal_older_than_48h",
		FixCommand:     ascDailyFixCommand(),
	}}
}

func ascDailyFixCommand() string {
	return `COMPOUND_ASC_DAILY_COMMAND='asc insights daily --app {app_id} --vendor {vendor} --date {date} --output json' bun tools/asc_daily_signals.ts --config "${COMPOUND_HOME:-${HOME}/.compound}/private/asc-products.json" --asc-command "${COMPOUND_ASC_DAILY_COMMAND}" --apply --json`
}

func isASCSignal(signal Signal) bool {
	metric := normalizeIDPart(signal.MetricName)
	if strings.HasPrefix(metric, "app_store_connect_") || metric == "app_store_product_page_conversion" {
		return true
	}
	for _, ref := range signal.EvidenceRefs {
		ref = strings.TrimSpace(ref)
		if strings.HasPrefix(ref, "asc:") || strings.Contains(ref, ":asc:") || strings.Contains(ref, ":asc-") {
			return true
		}
	}
	return false
}

func WritebackGate(report DoctorReport, actionIntentRef, policyDecisionRef string, now time.Time) writeback.OwnerWritebackGate {
	return writeback.OwnerWritebackGate{
		SchemaVersion:     writeback.GateSchemaVersion,
		CreatedAt:         now.UTC().Format(time.RFC3339),
		CandidateRef:      fmt.Sprintf("product:feedback_review:%s:%s", report.ProductID, productDoctorDigest(report)),
		CandidateKind:     productDoctorCandidateKind(report),
		OwnerTarget:       writeback.OwnerTarget{Kind: "product_strategy", Ref: report.ProductID},
		GateBasis:         fmt.Sprintf("product doctor found %d pending hypothesis review(s), %d signal-backed review-ready pair(s), %d source issue(s), and %d missing-evidence hypothesis record(s)", report.PendingReviewCount, report.ReviewReadyCount, report.SourceIssueCount, len(report.MissingEvidenceRefs)),
		ActionIntentRef:   actionIntentRef,
		MutationClass:     "local_reversible",
		PolicyDecisionRef: policyDecisionRef,
		EvidenceRefs:      EvidenceRefs(report),
	}
}

type LoopTransitionInput struct {
	Kind         string
	ProductID    string
	ObjectID     string
	Actor        string // producer lane_id: who authored this loop move (provenance)
	SourceTrace  string
	BeforeRefs   []string
	AfterRefs    []string
	EvidenceRefs []string
	Summary      string
}

func BuildLoopTransition(in LoopTransitionInput, now time.Time) (envelope.Transition, error) {
	if strings.TrimSpace(in.Kind) == "" || strings.TrimSpace(in.ProductID) == "" || strings.TrimSpace(in.ObjectID) == "" {
		return envelope.Transition{}, fmt.Errorf("product loop transition kind product_id object_id required")
	}
	if strings.TrimSpace(in.SourceTrace) == "" {
		in.SourceTrace = in.Kind + ":" + in.ObjectID
	}
	evidenceRefs := compact(in.EvidenceRefs)
	afterRefs := compact(in.AfterRefs)
	if len(evidenceRefs) == 0 {
		evidenceRefs = afterRefs
	}
	if in.Summary == "" {
		in.Summary = fmt.Sprintf("%s %s recorded for %s", in.Kind, in.ObjectID, in.ProductID)
	}
	tr := envelope.Transition{
		Header: envelope.Header{
			SchemaVersion: envelope.SVTransition,
			ID:            stableID("tr_", in.Kind, in.ProductID, in.ObjectID),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound", LaneID: strings.TrimSpace(in.Actor)},
			IDs:           envelope.IDs{WorkID: stableID("work_product_", in.Kind, in.ProductID, in.ObjectID)},
			Scope:         envelope.Scope{ProductID: in.ProductID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  evidenceRefs,
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		SourceTraceID: in.SourceTrace,
		BeforeRefs:    compact(in.BeforeRefs),
		ActionRefs:    []string{in.SourceTrace},
		AfterRefs:     afterRefs,
		StateDelta:    in.Summary,
		Projections: envelope.Projections{
			Product:        compact(append([]string{in.ProductID, in.Kind + ":" + in.ObjectID}, afterRefs...)),
			HumanAttention: []string{},
		},
		SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: true},
	}
	if err := tr.Validate(); err != nil {
		return envelope.Transition{}, err
	}
	return tr, nil
}

func RecordTransition(homePath string, tr envelope.Transition) error {
	if err := tr.Validate(); err != nil {
		return err
	}
	store, err := eventstore.Open(homePath)
	if err != nil {
		return err
	}
	_, err = store.PutJSON(tr.ID, &tr, " ")
	return err
}

func productClaims(bundle Bundle, evidenceID, sourcePath string) []mem.Candidate {
	src := "product-registry:" + filepath.Base(sourcePath)
	p := bundle.Product
	add := func(pred, obj string) mem.Candidate {
		return mem.Candidate{
			Entity:         p.ID,
			Predicate:      pred,
			Object:         obj,
			Confidence:     1.0,
			Derivation:     "deterministic",
			ClaimType:      "status",
			Qualifiers:     map[string]string{"domain": "product"},
			PromotionBasis: candidates.BasisTrustedDeterministic,
			SourceTraceRef: evidenceID + ":" + src,
			SnippetHash:    stableID("prod_snip_", p.ID, pred, obj),
			Visibility:     "operational",
		}
	}
	claims := []mem.Candidate{
		add("positioning", p.Positioning),
		add("privacy_posture", p.PrivacyPosture),
		add("channels", strings.Join(p.Channels, ",")),
		add("surfaces", strings.Join(surfaceRefs(p.Surfaces), ",")),
	}
	releaseClaims := []struct {
		predicate string
		value     string
	}{
		{"release_bundle_id", p.ReleaseIdentity.BundleID},
		{"release_app_store_connect_app_id", p.ReleaseIdentity.AppStoreConnectAppID},
		{"release_subscription_ids", strings.Join(compact(p.ReleaseIdentity.SubscriptionIDs), ",")},
		{"release_version", p.ReleaseIdentity.Version},
		{"release_build", p.ReleaseIdentity.Build},
	}
	for _, item := range releaseClaims {
		if value := strings.TrimSpace(item.value); value != "" {
			claims = append(claims, add(item.predicate, value))
		}
	}
	if strings.TrimSpace(p.ReleaseIdentity.AppStoreConnectAppID) != "" {
		claims = append(claims, add("release_mechanism", "app_store_connect"))
	}
	for _, fact := range essenceFacts(p) {
		claims = append(claims, add(fact.predicate, fact.value))
	}
	// Deployment claims key on p.ID like every other product claim. That is the
	// whole point: these facts previously landed on a bare-slug entity written by
	// internal/authority, so `product:<slug>` and a bare `<slug>` were two entities
	// and no reader could join them. A product with no known place emits nothing.
	if strings.TrimSpace(p.Deployment.Place) != "" {
		deploymentClaims := []struct {
			predicate string
			value     string
		}{
			{"deploys_to", p.Deployment.Place},
			{"deploy_method", p.Deployment.Method},
			{"deploy_command", p.Deployment.Command},
			{"deploy_rollback", p.Deployment.Rollback},
			{"deploy_services", strings.Join(compact(p.Deployment.Services), ",")},
			{"deploy_healthchecks", strings.Join(compact(p.Deployment.Healthchecks), ",")},
		}
		for _, item := range deploymentClaims {
			if value := strings.TrimSpace(item.value); value != "" {
				claims = append(claims, add(item.predicate, value))
			}
		}
	}
	return claims
}

func productIdentifiers(p Product) []string {
	var ids []string
	ids = append(ids, p.ID)
	for _, s := range p.Surfaces {
		if s.Ref != "" {
			ids = append(ids, s.Ref)
		}
	}
	return compact(ids)
}

func surfaceRefs(surfaces []Surface) []string {
	var refs []string
	for _, s := range surfaces {
		if s.Ref != "" {
			refs = append(refs, s.Ref)
		}
	}
	return compact(refs)
}

func hypothesisCandidate(h Hypothesis, now time.Time) HypothesisCandidate {
	result := h.Result
	if result == "" {
		result = "unknown"
	}
	return HypothesisCandidate{
		SchemaVersion:     ProductHypothesisCandidate,
		ID:                h.ID,
		Kind:              string(candidates.ProductHypothesis),
		ProductID:         h.ProductID,
		ChangeEvent:       h.ChangeEvent,
		ExpectedEffect:    h.ExpectedEffect,
		Metrics:           h.Metrics,
		ObservationWindow: h.ObservationWindow,
		Result:            result,
		NextAction:        h.NextAction,
		Verdict:           candidates.Park,
		EvidenceRefs:      h.EvidenceRefs,
		CreatedAt:         now.UTC().Format(time.RFC3339),
	}
}

func sourceIssueFollowUpHypothesis(review SourceIssueReview, now time.Time) HypothesisCandidate {
	f := review.FollowUp
	evidenceRefs := compact(append([]string{}, review.EvidenceRefs...))
	if f != nil {
		evidenceRefs = compact(append(evidenceRefs, f.EvidenceRefs...))
	}
	evidenceRefs = compact(append(evidenceRefs, "product_source_issue_review:"+review.ID))
	h := Hypothesis{
		ID:                stableID("hyp_", review.ProductID, review.IssueID, review.ID, f.ChangeEvent, f.ExpectedEffect),
		ProductID:         review.ProductID,
		ChangeEvent:       f.ChangeEvent,
		ExpectedEffect:    f.ExpectedEffect,
		Metrics:           compact(f.Metrics),
		ObservationWindow: f.ObservationWindow,
		Result:            "unknown",
		NextAction:        f.NextAction,
		EvidenceRefs:      evidenceRefs,
	}
	return hypothesisCandidate(h, now)
}

func appendUniqueSignal(homePath string, signal Signal) error {
	_, err := persistUniqueSignal(homePath, signal)
	return err
}

func persistUniqueSignal(homePath string, signal Signal) (Signal, error) {
	path := signalsLog(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Signal{}, err
	}
	signals, err := loadSignals(homePath)
	if err != nil {
		return Signal{}, err
	}
	for _, existing := range signals {
		if existing.ID == signal.ID {
			if !sameSignal(existing, signal) {
				return Signal{}, fmt.Errorf("product signal %q already exists with different content", signal.ID)
			}
			return existing, nil
		}
	}
	if err := appendJSONL(path, &signal); err != nil {
		return Signal{}, err
	}
	return signal, nil
}

func writeHypothesisCandidate(homePath string, c HypothesisCandidate) error {
	return candidates.WriteCandidate(homePath, candidates.ProductHypothesis, c.ID, c)
}

func loadHypothesisCandidate(homePath, id string) (HypothesisCandidate, error) {
	path := filepath.Join(homePath, candidates.InboxDir(candidates.ProductHypothesis), id+".json")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return HypothesisCandidate{}, fmt.Errorf("product hypothesis %q not found", id)
	}
	if err != nil {
		return HypothesisCandidate{}, err
	}
	var h HypothesisCandidate
	if err := json.Unmarshal(b, &h); err != nil {
		return HypothesisCandidate{}, fmt.Errorf("parse product hypothesis %q: %w", id, err)
	}
	if h.ID == "" {
		return HypothesisCandidate{}, fmt.Errorf("product hypothesis %q is missing id", id)
	}
	return h, nil
}

func appendUniqueReview(homePath string, review Review) error {
	return withReviewHistory(homePath, true, func(f *os.File, reviews []Review) error {
		for _, existing := range reviews {
			if existing.ID == review.ID {
				if !sameReview(existing, review) {
					return fmt.Errorf("product review %q already exists with different content or provenance", review.ID)
				}
				// A prior append may have returned a sync failure. Retrying the
				// same immutable row must finish its durability, not append again.
				return f.Sync()
			}
		}
		return appendReviewRow(f, review)
	})
}

func appendUniqueSourceIssueReview(homePath string, review SourceIssueReview) error {
	path := sourceIssueReviewsLog(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	reviews, err := loadSourceIssueReviews(homePath)
	if err != nil {
		return err
	}
	for _, existing := range reviews {
		if existing.ID == review.ID {
			if !sameSourceIssueReview(existing, review) {
				return fmt.Errorf("product source issue review %q already exists with different content", review.ID)
			}
			return nil
		}
		if existing.ProductID == review.ProductID && existing.IssueID == review.IssueID && sourceIssueClosed(existing.Result) {
			return fmt.Errorf("product source issue %q already reviewed by %q", review.IssueID, existing.ID)
		}
	}
	return appendJSONL(path, &review)
}

func loadSignals(homePath string) ([]Signal, error) {
	var out []Signal
	err := readJSONL(signalsLog(homePath), func(line []byte) error {
		var s Signal
		if err := json.Unmarshal(line, &s); err != nil || s.ID == "" {
			return nil
		}
		out = append(out, s)
		return nil
	})
	return out, err
}

// LoadProductSignals reads ONLY the product-signal ledger, filtered to one product
// (or all products when productID is empty). It is the lightweight path for
// consumers that need signals but not the full Timeline — the full timeline also
// scans the entire event store and source-issue log, which is expensive on a large
// home. The actuator loop-closer uses this so "did my move land?" answers in
// milliseconds instead of hanging on a full-home scan.
func LoadProductSignals(homePath, productID string) ([]Signal, error) {
	var out []Signal
	err := readJSONL(signalsLog(homePath), func(line []byte) error {
		var s Signal
		if err := json.Unmarshal(line, &s); err != nil || s.ID == "" {
			return nil
		}
		if productID == "" || s.ProductID == productID {
			out = append(out, s)
		}
		return nil
	})
	return out, err
}

func loadHypotheses(homePath string) ([]HypothesisCandidate, error) {
	dir := filepath.Join(homePath, candidates.InboxDir(candidates.ProductHypothesis))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []HypothesisCandidate
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var h HypothesisCandidate
		if json.Unmarshal(b, &h) == nil && h.ID != "" {
			out = append(out, h)
		}
	}
	return out, nil
}

func loadReviews(homePath string) ([]Review, error) {
	var out []Review
	err := withReviewHistory(homePath, false, func(_ *os.File, reviews []Review) error {
		out = reviews
		return nil
	})
	return out, err
}

func loadSourceIssueReviews(homePath string) ([]SourceIssueReview, error) {
	var out []SourceIssueReview
	err := readJSONL(sourceIssueReviewsLog(homePath), func(line []byte) error {
		var r SourceIssueReview
		if err := json.Unmarshal(line, &r); err != nil || r.ID == "" {
			return nil
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

// productTransitionsMemo memoizes ONE scan of the transition family, grouped by
// product, for the lifetime of a single process. Authority/BuildLoopModel call
// LoadTimeline once per product; without this each call re-scanned the entire
// (large) transition family — an N-products × full-scan amplification that is what
// made `product loops`/`authority` hang on a big home. The scan now runs once per
// (home, transition-segment fingerprint). The event store stays the sole authority:
// this is a process-local read cache, never persisted, and any segment change
// (append/seal/rotate) shifts the fingerprint and converges back to a real scan.
var productTransitionsMemo struct {
	sync.Mutex
	home        string
	fingerprint string
	byProduct   map[string][]TimelineTransition
}

// transitionFamilyFingerprint asks the event store for an opaque token over exactly
// the content a transition Scan reads — the transition family's segments plus the
// loose layer while the migration gate is open — so the memo invalidates the instant
// any of it changes. The store is the sole physical owner of the events layout, so
// product never touches it directly: it compares tokens only. Segments are
// append-only and immutable once sealed, so an identical token means identical
// content; any append/seal/rotate shifts it and converges the memo back to a real
// scan. A store we cannot open fingerprints with the error so the memo can never
// serve a stale hit (the subsequent scan surfaces the real error).
func transitionFamilyFingerprint(homePath string) string {
	store, err := eventstore.Open(homePath)
	if err != nil {
		return "open-err|" + err.Error()
	}
	return store.ScanFingerprint(eventstore.ScanOptions{Prefixes: []string{"tr_"}})
}

func loadProductTransitions(homePath, productID string) ([]TimelineTransition, error) {
	grouped, err := loadProductTransitionsGrouped(homePath)
	if err != nil {
		return nil, err
	}
	// Read-only: callers (LoadTimeline/Authority) never mutate the returned slice.
	return grouped[productID], nil
}

// loadProductTransitionsGrouped returns all transitions grouped by the products
// they reference, scanning the transition family once and memoizing per process.
func loadProductTransitionsGrouped(homePath string) (map[string][]TimelineTransition, error) {
	fingerprint := transitionFamilyFingerprint(homePath)
	productTransitionsMemo.Lock()
	if productTransitionsMemo.byProduct != nil && productTransitionsMemo.home == homePath && productTransitionsMemo.fingerprint == fingerprint {
		grouped := productTransitionsMemo.byProduct
		productTransitionsMemo.Unlock()
		return grouped, nil
	}
	productTransitionsMemo.Unlock()

	grouped, err := scanProductTransitions(homePath)
	if err != nil {
		return nil, err
	}

	productTransitionsMemo.Lock()
	productTransitionsMemo.home = homePath
	productTransitionsMemo.fingerprint = fingerprint
	productTransitionsMemo.byProduct = grouped
	productTransitionsMemo.Unlock()
	return grouped, nil
}

// scanProductTransitions scans the transition family ("tr_" prefix — a pure I/O
// restriction: scanFamilyRestriction skips other families without even a readdir,
// while the in-scan matchesPrefix post-filter keeps results byte-for-byte identical
// to an unrestricted scan) and groups each transition by every product it
// references (Scope.ProductID and each Projections.Product), preserving the exact
// filter and CreatedAt/ID ordering the per-product scan produced.
func scanProductTransitions(homePath string) (map[string][]TimelineTransition, error) {
	store, err := eventstore.Open(homePath)
	if err != nil {
		return nil, err
	}
	entries, err := store.Scan(eventstore.ScanOptions{Prefixes: []string{"tr_"}})
	if os.IsNotExist(err) {
		return map[string][]TimelineTransition{}, nil
	}
	if err != nil {
		return nil, err
	}
	grouped := map[string][]TimelineTransition{}
	for _, entry := range entries {
		record, err := entry.Read()
		if err != nil {
			return nil, err
		}
		b := record.Bytes
		var header struct {
			SchemaVersion string `json:"schema_version"`
		}
		if err := json.Unmarshal(b, &header); err != nil || header.SchemaVersion != envelope.SVTransition {
			continue
		}
		var tr envelope.Transition
		if err := json.Unmarshal(b, &tr); err != nil {
			return nil, err
		}
		if err := tr.Validate(); err != nil {
			return nil, fmt.Errorf("product transition %s: %w", entry.Name, err)
		}
		kind, objectRef := productProjectionObject(tr.Projections.Product)
		tt := TimelineTransition{
			ID:                    tr.ID,
			CreatedAt:             tr.CreatedAt,
			Kind:                  kind,
			ObjectRef:             objectRef,
			StateDelta:            tr.StateDelta,
			EvidenceRefs:          tr.EvidenceRefs,
			FutureBehaviorChanged: tr.SecondOrder.FutureBehaviorChanged,
		}
		prods := map[string]bool{}
		if tr.Scope.ProductID != "" {
			prods[tr.Scope.ProductID] = true
		}
		for _, p := range tr.Projections.Product {
			if p != "" {
				prods[p] = true
			}
		}
		for p := range prods {
			grouped[p] = append(grouped[p], tt)
		}
	}
	for _, list := range grouped {
		sort.Slice(list, func(i, j int) bool {
			if list[i].CreatedAt == list[j].CreatedAt {
				return list[i].ID < list[j].ID
			}
			return list[i].CreatedAt < list[j].CreatedAt
		})
	}
	return grouped, nil
}

// noPromoteMemo memoizes ONE parse of the shared no-promote decision ledger per
// process. The loop/doctor path reads this ledger once per product (to find that
// product's source issues), so without the memo a large home re-reads the same
// multi-MB file N times. Like productTransitionsMemo it is a pure read-side cache:
// the fingerprint covers the ledger file (size+mtime), a miss re-reads, and a hit
// returns the parsed slice read-only. Malformed/empty lines are skipped exactly as
// the old per-call readJSONL did, so output is byte-identical.
var noPromoteMemo struct {
	sync.Mutex
	home        string
	fingerprint string
	decisions   []writeback.NoPromoteDecision
}

// noPromoteFingerprint fingerprints the single no-promote ledger file so the memo
// invalidates if it changes. An absent file fingerprints distinctly, never "".
func noPromoteFingerprint(homePath string) string {
	info, err := os.Stat(noPromoteLog(homePath))
	if err != nil {
		return "nopromote|<absent>"
	}
	return fmt.Sprintf("nopromote|%d:%d", info.Size(), info.ModTime().UnixNano())
}

// loadNoPromoteDecisions parses the shared no-promote ledger once per process,
// memoized by file fingerprint. Callers filter the returned slice (read-only) by
// product instead of each re-reading the file.
func loadNoPromoteDecisions(homePath string) []writeback.NoPromoteDecision {
	fingerprint := noPromoteFingerprint(homePath)
	noPromoteMemo.Lock()
	if noPromoteMemo.decisions != nil && noPromoteMemo.home == homePath && noPromoteMemo.fingerprint == fingerprint {
		decisions := noPromoteMemo.decisions
		noPromoteMemo.Unlock()
		return decisions
	}
	noPromoteMemo.Unlock()

	var decisions []writeback.NoPromoteDecision
	_ = readJSONL(noPromoteLog(homePath), func(line []byte) error {
		var d writeback.NoPromoteDecision
		if err := json.Unmarshal(line, &d); err != nil || d.ID == "" {
			return nil
		}
		decisions = append(decisions, d)
		return nil
	})

	noPromoteMemo.Lock()
	noPromoteMemo.home = homePath
	noPromoteMemo.fingerprint = fingerprint
	noPromoteMemo.decisions = decisions
	noPromoteMemo.Unlock()
	return decisions
}

func loadProductSourceIssues(homePath, productID string) ([]ProductSourceIssue, error) {
	reviews, err := loadSourceIssueReviews(homePath)
	if err != nil {
		return nil, err
	}
	closed := map[string]SourceIssueReview{}
	for _, review := range reviews {
		if review.ProductID == productID && sourceIssueClosed(review.Result) {
			closed[review.IssueID] = review
		}
	}
	var out []ProductSourceIssue
	for _, d := range loadNoPromoteDecisions(homePath) {
		if d.OwnerTarget.Kind != "product_signal_source" && d.OwnerTarget.Kind != ProductChangeStreamOwner {
			continue
		}
		if !containsValue(d.EvidenceRefs, productRef(productID)) {
			continue
		}
		sourceRef := d.OwnerTarget.Ref
		if sourceRef == "" {
			sourceRef = d.CandidateRef
		}
		issue := ProductSourceIssue{
			ID:           d.ID,
			CreatedAt:    d.CreatedAt,
			SourceRef:    sourceRef,
			Basis:        d.Basis,
			EvidenceRefs: d.EvidenceRefs,
			Status:       "open",
		}
		if review, ok := closed[d.ID]; ok {
			issue.Status = "closed"
			issue.ReviewID = review.ID
			issue.ReviewResult = review.Result
		}
		out = append(out, issue)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt == out[j].CreatedAt {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt < out[j].CreatedAt
	})
	return out, nil
}

func signalsLog(homePath string) string {
	return filepath.Join(homePath, "ledger", "product-signals.jsonl")
}

func reviewsLog(homePath string) string {
	return filepath.Join(homePath, "ledger", "product-reviews.jsonl")
}

func sourceIssueReviewsLog(homePath string) string {
	return filepath.Join(homePath, "ledger", "product-source-issue-reviews.jsonl")
}

func noPromoteLog(homePath string) string {
	return filepath.Join(homePath, "ledger", "no-promote-decisions.jsonl")
}

func appendJSONL(path string, v any) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

func readJSONL(path string, fn func([]byte) error) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if err := fn([]byte(line)); err != nil {
			return err
		}
	}
	return nil
}

func decodeKnownYAML(b []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	return dec.Decode(out)
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func signalDoctorFailure(report SignalDoctorReport) string {
	for _, check := range report.Checks {
		if check.Status == "fail" {
			if check.Detail != "" {
				return check.Name + ": " + check.Detail
			}
			return check.Name
		}
	}
	return "unknown failure"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func containsValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func productProjectionObject(refs []string) (string, string) {
	for _, ref := range refs {
		switch {
		case strings.HasPrefix(ref, "product_signal:"):
			return "product_signal", ref
		case strings.HasPrefix(ref, "product_review:"):
			return "product_review", ref
		case strings.HasPrefix(ref, "product_review_intent:"):
			return "product_review_intent", ref
		case strings.HasPrefix(ref, "product_review_refused:"):
			return "product_review_refused", ref
		case strings.HasPrefix(ref, "product_source_issue_review:"):
			return "product_source_issue_review", ref
		case strings.HasPrefix(ref, "product_hypothesis:"):
			return "product_hypothesis", ref
		}
	}
	return "", ""
}

func productRef(productID string) string {
	if strings.TrimSpace(productID) == "" {
		return ""
	}
	return "product_id:" + strings.TrimSpace(productID)
}

func sourceKindRef(kind string) string {
	if strings.TrimSpace(kind) == "" {
		return ""
	}
	return "source_kind:" + strings.TrimSpace(kind)
}

func canonicalMetricName(sourceKind, metricName string) string {
	sourceKind = normalizeIDPart(sourceKind)
	metricName = normalizeIDPart(metricName)
	if canonical, ok := canonicalSignalMetricNames[sourceKind+":"+metricName]; ok {
		return canonical
	}
	if sourceKind == "" {
		return metricName
	}
	if metricName == "" {
		return sourceKind
	}
	return sourceKind + "_" + metricName
}

func normalizeIDPart(value string) string {
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

func stableID(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + hex.EncodeToString(sum[:])[:12]
}

func productDoctorDigest(report DoctorReport) string {
	refs := EvidenceRefs(report)
	sort.Strings(refs)
	return stableID("", refs...)
}

func productDoctorCandidateKind(report DoctorReport) string {
	if report.PendingReviewCount > 0 || len(report.MissingEvidenceRefs) > 0 {
		return string(candidates.ProductHypothesis)
	}
	return ""
}

func reviewReadySignals(hypotheses []HypothesisCandidate, signals []Signal) []ReviewReady {
	var out []ReviewReady
	for _, h := range hypotheses {
		for _, signal := range signals {
			reason := signalHypothesisMatchReason(h, signal)
			if reason == "" {
				continue
			}
			evidenceRefs := append([]string{}, h.EvidenceRefs...)
			evidenceRefs = append(evidenceRefs, signal.EvidenceRefs...)
			evidenceRefs = append(evidenceRefs, "product_hypothesis:"+h.ID, "product_signal:"+signal.ID)
			out = append(out, ReviewReady{
				HypothesisID:      h.ID,
				SignalID:          signal.ID,
				MetricName:        signal.MetricName,
				Window:            signal.Window,
				MatchReason:       reason,
				ReviewPlanCommand: fmt.Sprintf("compound product review-plan --hypothesis %s --signal %s", h.ID, signal.ID),
				EvidenceRefs:      compact(evidenceRefs),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HypothesisID == out[j].HypothesisID {
			return out[i].SignalID < out[j].SignalID
		}
		return out[i].HypothesisID < out[j].HypothesisID
	})
	return out
}

func signalHypothesisMatchReason(h HypothesisCandidate, signal Signal) string {
	if strings.TrimSpace(h.ProductID) == "" || h.ProductID != signal.ProductID {
		return ""
	}
	for _, ref := range h.EvidenceRefs {
		if strings.TrimSpace(ref) == "product_signal:"+signal.ID {
			return "explicit_evidence_ref"
		}
	}
	signalMetric := normalizeIDPart(signal.MetricName)
	for _, metric := range h.Metrics {
		if signalMetric != "" && normalizeIDPart(metric) == signalMetric {
			return "metric_match"
		}
	}
	if normalizeIDPart(h.ChangeEvent) != "" && normalizeIDPart(h.ChangeEvent) == normalizeIDPart(signal.ChangeEvent) {
		return "change_event_match"
	}
	return ""
}

func openProductSourceIssues(issues []ProductSourceIssue) []ProductSourceIssue {
	var out []ProductSourceIssue
	for _, issue := range issues {
		if issue.Status != "closed" {
			out = append(out, issue)
		}
	}
	return out
}

func sourceIssueClosed(result string) bool {
	return oneOf(result, []string{"resolved", "waived", "false_positive"})
}

func compact(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func sameReview(a, b Review) bool {
	return equalReviewValue(a, b)
}

func sameSourceIssueReview(a, b SourceIssueReview) bool {
	return a.ProductID == b.ProductID &&
		a.IssueID == b.IssueID &&
		a.Result == b.Result &&
		a.Rationale == b.Rationale &&
		strings.Join(a.EvidenceRefs, "\x00") == strings.Join(b.EvidenceRefs, "\x00") &&
		sameSourceIssueFollowUp(a.FollowUp, b.FollowUp)
}

func sameSourceIssueFollowUp(a, b *SourceIssueFollowUp) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ChangeEvent == b.ChangeEvent &&
		a.ExpectedEffect == b.ExpectedEffect &&
		a.ObservationWindow == b.ObservationWindow &&
		a.NextAction == b.NextAction &&
		strings.Join(a.Metrics, "\x00") == strings.Join(b.Metrics, "\x00") &&
		strings.Join(a.EvidenceRefs, "\x00") == strings.Join(b.EvidenceRefs, "\x00")
}

func sameSignal(a, b Signal) bool {
	a.ObservedAt = ""
	b.ObservedAt = ""
	return a.ID == b.ID &&
		a.ProductID == b.ProductID &&
		a.Surface == b.Surface &&
		a.ChangeEvent == b.ChangeEvent &&
		a.MetricName == b.MetricName &&
		a.Before == b.Before &&
		a.After == b.After &&
		a.Window == b.Window &&
		a.Confidence == b.Confidence &&
		a.NextAction == b.NextAction &&
		strings.Join(a.Confounders, "\x00") == strings.Join(b.Confounders, "\x00") &&
		strings.Join(a.EvidenceRefs, "\x00") == strings.Join(b.EvidenceRefs, "\x00") &&
		sameInterpretations(a.Interpretations, b.Interpretations)
}

func sameInterpretations(a, b []Interpretation) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func oneOf(v string, allowed []string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}
