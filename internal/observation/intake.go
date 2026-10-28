package observation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/knowledge"
)

const (
	SourceIntakeRecordSchemaVersion     = "compound.source_intake_record.v0.1"
	SourceIntakeProjectionSchemaVersion = "compound.source_intake_projection.v0.1"
	SourceIntakeAuthorityRef            = "compound://sources/intake"
	sourceIntakeStaleAfter              = 72 * time.Hour
)

type SourceIntakeRecord struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	CreatedAt     string `json:"created_at"`
	Path          string `json:"path,omitempty"`
	Report        Report `json:"report"`
}

type SourceIntakeProjection struct {
	SchemaVersion   string                   `json:"schema_version"`
	AuthorityRef    string                   `json:"authority_ref"`
	Status          string                   `json:"status"`
	GeneratedAt     string                   `json:"generated_at"`
	RecordCount     int                      `json:"record_count"`
	RowCount        int                      `json:"row_count"`
	Counts          SourceIntakeCounts       `json:"counts"`
	Rows            []SourceIntakeRow        `json:"rows"`
	ActionCardCount int                      `json:"action_card_count"`
	ActionCards     []SourceIntakeActionCard `json:"action_cards,omitempty"`
	NextCommand     []string                 `json:"next_command"`
}

type SourceIntakeCounts struct {
	Applied        int            `json:"applied"`
	ReviewRecorded int            `json:"review_recorded"`
	Planned        int            `json:"planned"`
	Skipped        int            `json:"skipped"`
	BlockedPrivate int            `json:"blocked_private"`
	Unsupported    int            `json:"unsupported"`
	Stale          int            `json:"stale"`
	ActionRequired int            `json:"action_required"`
	ByRoute        map[string]int `json:"by_route"`
	ByOwner        map[string]int `json:"by_owner"`
	BySourceKind   map[string]int `json:"by_source_kind"`
	BySubjectKind  map[string]int `json:"by_subject_kind"`
}

type SourceIntakeRow struct {
	ID               string                   `json:"id"`
	RecordID         string                   `json:"record_id"`
	ReportPath       string                   `json:"report_path,omitempty"`
	GeneratedAt      string                   `json:"generated_at"`
	SourcePath       string                   `json:"source_path,omitempty"`
	SourceKind       string                   `json:"source_kind"`
	SourceRef        string                   `json:"source_ref"`
	SourceCapturedAt string                   `json:"source_captured_at"`
	Collector        string                   `json:"collector,omitempty"`
	Privacy          string                   `json:"privacy,omitempty"`
	SubjectKind      string                   `json:"subject_kind"`
	SubjectID        string                   `json:"subject_id"`
	Surface          string                   `json:"surface,omitempty"`
	Channel          string                   `json:"channel,omitempty"`
	ObservationID    string                   `json:"observation_id"`
	ObservationKind  string                   `json:"observation_kind"`
	ObservedAt       string                   `json:"observed_at,omitempty"`
	FreshnessAt      string                   `json:"freshness_at"`
	Route            string                   `json:"route"`
	Owner            string                   `json:"owner,omitempty"`
	Status           string                   `json:"status"`
	Reason           string                   `json:"reason,omitempty"`
	AppliedIDs       []string                 `json:"applied_ids,omitempty"`
	SkippedRows      []string                 `json:"skipped_rows,omitempty"`
	BlockedRows      []string                 `json:"blocked_rows,omitempty"`
	EvidenceRefs     []string                 `json:"evidence_refs,omitempty"`
	ActionRequired   bool                     `json:"action_required"`
	ActionCards      []SourceIntakeActionCard `json:"action_cards,omitempty"`
	NextCommand      []string                 `json:"next_command,omitempty"`
}

type SourceIntakeActionCard struct {
	ID                 string   `json:"id"`
	Kind               string   `json:"kind"`
	Title              string   `json:"title"`
	Severity           string   `json:"severity,omitempty"`
	RowID              string   `json:"row_id"`
	ObservationID      string   `json:"observation_id"`
	Route              string   `json:"route"`
	Owner              string   `json:"owner,omitempty"`
	Command            []string `json:"command,omitempty"`
	RequiredInputs     []string `json:"required_inputs,omitempty"`
	ExpectedValidation []string `json:"expected_validation,omitempty"`
	StopConditions     []string `json:"stop_conditions,omitempty"`
	Boundary           string   `json:"boundary"`
	Proof              string   `json:"proof"`
	EvidenceRefs       []string `json:"evidence_refs,omitempty"`
}

type sourceIntakeDetail struct {
	status      string
	owner       string
	appliedIDs  []string
	nextCommand []string
}

func RecordSourceIntakeReport(homePath string, report Report, now time.Time) (SourceIntakeRecord, error) {
	if strings.TrimSpace(homePath) == "" {
		return SourceIntakeRecord{}, fmt.Errorf("COMPOUND_HOME required recording source intake")
	}
	createdAt := report.GeneratedAt
	if strings.TrimSpace(createdAt) == "" {
		createdAt = now.UTC().Format(time.RFC3339)
		report.GeneratedAt = createdAt
	}
	id := stableID("sint_", report.Source.Kind, report.Source.Ref, report.Subject.Kind, report.Subject.ID, report.SourcePath, createdAt, sourceIntakeMode(report))
	dir := sourceIntakeReportsDir(homePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return SourceIntakeRecord{}, fmt.Errorf("create source intake reports dir: %w", err)
	}
	path := filepath.Join(dir, id+".json")
	record := SourceIntakeRecord{
		SchemaVersion: SourceIntakeRecordSchemaVersion,
		ID:            id,
		CreatedAt:     createdAt,
		Path:          path,
		Report:        report,
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return SourceIntakeRecord{}, fmt.Errorf("marshal source intake record: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return SourceIntakeRecord{}, fmt.Errorf("write source intake record: %w", err)
	}
	return record, nil
}

func LoadSourceIntakeProjection(homePath string, now time.Time, limit int) (SourceIntakeProjection, error) {
	projection := SourceIntakeProjection{
		SchemaVersion: SourceIntakeProjectionSchemaVersion,
		AuthorityRef:  SourceIntakeAuthorityRef,
		Status:        "ok",
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Counts: SourceIntakeCounts{
			ByRoute:       map[string]int{},
			ByOwner:       map[string]int{},
			BySourceKind:  map[string]int{},
			BySubjectKind: map[string]int{},
		},
		Rows:        []SourceIntakeRow{},
		NextCommand: []string{"compound", "source", "intake", "--json"},
	}
	records, err := loadSourceIntakeRecords(homePath)
	if err != nil {
		return projection, err
	}
	projection.RecordCount = len(records)
	for _, record := range records {
		projection.Rows = append(projection.Rows, sourceIntakeRows(record)...)
	}
	sort.SliceStable(projection.Rows, func(i, j int) bool {
		if projection.Rows[i].GeneratedAt != projection.Rows[j].GeneratedAt {
			return projection.Rows[i].GeneratedAt > projection.Rows[j].GeneratedAt
		}
		return projection.Rows[i].ID < projection.Rows[j].ID
	})
	if limit > 0 && len(projection.Rows) > limit {
		projection.Rows = projection.Rows[:limit]
	}
	projection.RowCount = len(projection.Rows)
	seenActionCardIDs := map[string]bool{}
	for i := range projection.Rows {
		cards := sourceIntakeActionCards(homePath, projection.Rows[i], now)
		cards = dedupeSourceIntakeActionCards(cards, seenActionCardIDs)
		projection.Rows[i].ActionCards = cards
		projection.Rows[i].ActionRequired = len(cards) > 0
		projection.ActionCards = append(projection.ActionCards, cards...)
		row := projection.Rows[i]
		projection.Counts.ByRoute[row.Route]++
		if row.Owner != "" {
			projection.Counts.ByOwner[row.Owner]++
		}
		if row.SourceKind != "" {
			projection.Counts.BySourceKind[row.SourceKind]++
		}
		if row.SubjectKind != "" {
			projection.Counts.BySubjectKind[row.SubjectKind]++
		}
		switch row.Status {
		case "applied":
			projection.Counts.Applied++
		case "review_recorded":
			projection.Counts.ReviewRecorded++
		case "skipped":
			projection.Counts.Skipped++
		case RouteBlockedPrivate:
			projection.Counts.BlockedPrivate++
		case RouteUnsupportedKind:
			projection.Counts.Unsupported++
		default:
			projection.Counts.Planned++
		}
		if sourceIntakeRowIsStale(row, now) {
			projection.Counts.Stale++
		}
		if row.ActionRequired {
			projection.Counts.ActionRequired++
		}
	}
	projection.ActionCardCount = len(projection.ActionCards)
	return projection, nil
}

func shouldRecordSourceIntake(opts Options) bool {
	return opts.ApplyProductSignals ||
		opts.ApplySourceObservations ||
		opts.ApplyKnowledge ||
		opts.ApplyRouteEvidence ||
		opts.ApplyEvents ||
		opts.ReviewCandidates
}

func sourceIntakeReportsDir(homePath string) string {
	return filepath.Join(homePath, "projections", "source-intake", "reports")
}

func loadSourceIntakeRecords(homePath string) ([]SourceIntakeRecord, error) {
	dir := sourceIntakeReportsDir(homePath)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read source intake reports dir: %w", err)
	}
	var records []SourceIntakeRecord
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read source intake record %s: %w", path, err)
		}
		var record SourceIntakeRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("parse source intake record %s: %w", path, err)
		}
		if record.Path == "" {
			record.Path = path
		}
		records = append(records, record)
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].CreatedAt != records[j].CreatedAt {
			return records[i].CreatedAt > records[j].CreatedAt
		}
		return records[i].ID < records[j].ID
	})
	return records, nil
}

func sourceIntakeRows(record SourceIntakeRecord) []SourceIntakeRow {
	report := record.Report
	details := sourceIntakeDetails(report)
	skipped := sourceIntakeSkippedRows(report)
	rows := make([]SourceIntakeRow, 0, len(report.Decisions))
	for _, decision := range report.Decisions {
		status := "planned"
		owner := decision.Owner
		nextCommand := append([]string{}, decision.Command...)
		var appliedIDs []string
		var skippedRows []string
		var blockedRows []string
		if detail, ok := details[decision.ObservationID]; ok {
			status = detail.status
			owner = firstNonEmpty(detail.owner, owner)
			appliedIDs = append(appliedIDs, detail.appliedIDs...)
			nextCommand = append([]string{}, detail.nextCommand...)
		}
		if row, ok := skipped[decision.ObservationID]; ok {
			status = "skipped"
			skippedRows = append(skippedRows, row)
		}
		switch decision.Route {
		case RouteBlockedPrivate:
			status = RouteBlockedPrivate
			blockedRows = append(blockedRows, firstNonEmpty(decision.ObservationID+":"+decision.Reason, decision.ObservationID))
		case RouteUnsupportedKind:
			status = RouteUnsupportedKind
		}
		if len(nextCommand) == 0 {
			nextCommand = sourceIntakeDefaultNextCommand(report, decision)
		}
		row := SourceIntakeRow{
			ID:               stableID("sirow_", record.ID, decision.ObservationID, decision.Route),
			RecordID:         record.ID,
			ReportPath:       record.Path,
			GeneratedAt:      firstNonEmpty(report.GeneratedAt, record.CreatedAt),
			SourcePath:       report.SourcePath,
			SourceKind:       report.Source.Kind,
			SourceRef:        report.Source.Ref,
			SourceCapturedAt: report.Source.CapturedAt,
			Collector:        report.Source.Collector,
			Privacy:          report.Source.Privacy,
			SubjectKind:      report.Subject.Kind,
			SubjectID:        report.Subject.ID,
			Surface:          report.Subject.Surface,
			Channel:          report.Subject.Channel,
			ObservationID:    decision.ObservationID,
			ObservationKind:  decision.ObservationKind,
			ObservedAt:       decision.ObservedAt,
			FreshnessAt:      firstNonEmpty(decision.ObservedAt, report.Source.CapturedAt, report.GeneratedAt, record.CreatedAt),
			Route:            decision.Route,
			Owner:            owner,
			Status:           status,
			Reason:           decision.Reason,
			AppliedIDs:       reviewartifactUnique(appliedIDs),
			SkippedRows:      skippedRows,
			BlockedRows:      blockedRows,
			EvidenceRefs:     append([]string{}, decision.EvidenceRefs...),
			NextCommand:      nextCommand,
		}
		rows = append(rows, row)
	}
	return rows
}

func sourceIntakeActionCards(homePath string, row SourceIntakeRow, now time.Time) []SourceIntakeActionCard {
	var cards []SourceIntakeActionCard
	if expectedState, observedState, ok := sourceIntakeDivergenceDimensions(row); ok {
		cards = append(cards, sourceIntakeDivergenceCard(row, expectedState, observedState))
	}
	if sourceIntakeRowIsStale(row, now) {
		cards = append(cards, sourceIntakeActionCard(row, "stale_source",
			"Refresh stale source observation",
			"medium",
			sourceIntakeFirstCommand(row.NextCommand, []string{"compound", "source", "observe-batch", row.SourcePath, "--dry-run", "--json"}),
			[]string{"fresh source export or an explicit decision to keep this source stale"},
			[]string{"captured_at or freshness_at moves inside the source freshness window", "source intake still exposes the stale row if the source cannot be refreshed"},
			[]string{"refresh requires live credentials, account access, scheduler mutation, or public side effects"},
			"no live collector/auth/scheduler mutation from this card",
			"rerun source-intake projection and verify stale/action_required counts changed or remain explicitly visible"))
	}
	if extractionIDs := sourceIntakeOpenKnowledgeExtractionIDs(homePath, row); len(extractionIDs) > 0 {
		extractionID := extractionIDs[0]
		cards = append(cards, sourceIntakeActionCard(row, "knowledge_extraction_pending",
			"Resolve pending knowledge extraction before treating source as learned",
			"medium",
			[]string{"compound", "knowledge", "extract", "--item", extractionID, "--fixtures", "<reviewed-fixtures-dir>", "--json"},
			[]string{"reviewed extraction fixture directory; live model route requires a separate explicit budgeted command", "one extraction id selected from source-intake applied_ids"},
			[]string{"knowledge extract report references extraction " + extractionID, "created candidates remain candidate material; no memory claim is promoted", "source-intake projection no longer shows this card once extraction state is done"},
			[]string{"fixture/model route is unavailable", "requested action would run bulk --pending extraction", "requested action would adjudicate or promote memory"},
			"single extraction request only; no bulk extraction, no automatic memory promotion, no external account call",
			"run the bounded extraction command, then `compound source intake --json` and verify the extraction card cleared or remains visibly pending/error"))
	}
	switch row.Status {
	case "skipped":
		cards = append(cards, sourceIntakeActionCard(row, "skipped_route",
			"Choose bounded apply or review mode for skipped observation",
			"medium",
			sourceIntakeFirstCommand(row.NextCommand, sourceIntakeRouteCommand(row, "--review-candidates")),
			[]string{"operator or executor chooses the existing owner mode that matches the route"},
			[]string{"skipped row becomes applied or review_recorded in source intake", "no unrelated owner ledger is mutated"},
			[]string{"mode would mutate the wrong owner", "mode requires live/private side effects not already authorized"},
			"synthetic/temp/local apply or review only unless operator explicitly chooses live mutation",
			"run the card command, then `compound source intake --json` and inspect the same row status"))
	case RouteBlockedPrivate:
		cards = append(cards, sourceIntakeActionCard(row, "blocked_private_input",
			"Remove or supply private input outside public batch",
			"high",
			sourceIntakeFirstCommand(row.NextCommand, sourceIntakeRouteCommand(row, "--dry-run")),
			[]string{"credential, token, private body, or account-specific payload removed from the public batch or supplied through private runtime configuration"},
			[]string{"dry-run no longer routes this observation as blocked_private_input", "no private value appears in tracked public files or receipts"},
			[]string{"credential/secret/token remains in the batch", "required private input is unavailable"},
			"no live import/apply, no credential exposure, no public file write containing private material",
			"run dry-run and verify blocked_private_input count decreases or remains explicitly visible"))
	case RouteUnsupportedKind:
		cards = append(cards, sourceIntakeActionCard(row, "unsupported_kind",
			"Map unsupported observation kind to an existing owner route",
			"medium",
			sourceIntakeFirstCommand(row.NextCommand, sourceIntakeRouteCommand(row, "--dry-run")),
			[]string{"supported observation kind mapping or a reviewed owner-route contract"},
			[]string{"dry-run routes to an existing owner family or leaves unsupported_kind explicitly visible with reason"},
			[]string{"provider-specific core requested", "no existing owner or reviewed owner-route contract is named"},
			"route planning only; no connector-specific core authority and no external account call",
			"run dry-run and verify the route is supported or still visibly unsupported"))
	}
	return cards
}

func sourceIntakeOpenKnowledgeExtractionIDs(homePath string, row SourceIntakeRow) []string {
	if row.Status != "applied" || row.Route != RouteKnowledgeExport || row.Owner != "internal/knowledge" {
		return nil
	}
	ids := sourceIntakeExtractionIDs(row.AppliedIDs)
	if len(ids) == 0 {
		return nil
	}
	var open []string
	for _, id := range ids {
		if state := sourceIntakeKnowledgeExtractionState(homePath, id); state != knowledge.ExtractionStateDone {
			open = append(open, id)
		}
	}
	return open
}

func sourceIntakeExtractionIDs(appliedIDs []string) []string {
	var out []string
	for _, raw := range appliedIDs {
		suffix, ok := strings.CutPrefix(strings.TrimSpace(raw), "extraction:")
		if !ok {
			continue
		}
		if id := strings.TrimSpace(suffix); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func sourceIntakeKnowledgeExtractionState(homePath, extractionID string) string {
	if strings.TrimSpace(homePath) == "" || strings.TrimSpace(extractionID) == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(homePath, "inbox", "knowledge-extractions", extractionID+".json"))
	if err != nil {
		return ""
	}
	var req knowledge.ExtractionRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return ""
	}
	return req.State
}

func sourceIntakeActionCard(row SourceIntakeRow, kind, title, severity string, command, requiredInputs, expectedValidation, stopConditions []string, boundary, proof string) SourceIntakeActionCard {
	return SourceIntakeActionCard{
		ID:                 stableID("sicard_", row.ID, kind),
		Kind:               kind,
		Title:              title,
		Severity:           severity,
		RowID:              row.ID,
		ObservationID:      row.ObservationID,
		Route:              row.Route,
		Owner:              row.Owner,
		Command:            compact(command),
		RequiredInputs:     compact(requiredInputs),
		ExpectedValidation: compact(expectedValidation),
		StopConditions:     compact(stopConditions),
		Boundary:           boundary,
		Proof:              proof,
	}
}

// sourceIntakeDivergenceDimensions reports whether an applied source
// observation declared a real state divergence: its dimensions carry both
// expected_state and observed_state, non-empty, and they differ. The
// dimensions never reach the persisted Report/RouteDecision, so this reloads
// the original batch (already referenced by row.SourcePath) and locates the
// matching observation by its stable id. Missing file, missing observation,
// missing keys, or equal values all mean "no divergence" -- this never
// guesses divergence from prose.
func sourceIntakeDivergenceDimensions(row SourceIntakeRow) (expectedState, observedState string, ok bool) {
	if row.Status != "applied" || row.Route != RouteSourceObservation {
		return "", "", false
	}
	if strings.TrimSpace(row.SourcePath) == "" {
		return "", "", false
	}
	batch, err := LoadBatch(row.SourcePath)
	if err != nil {
		return "", "", false
	}
	obs, found := findObservation(batch, row.ObservationID)
	if !found {
		return "", "", false
	}
	expectedState = strings.TrimSpace(obs.Dimensions["expected_state"])
	observedState = strings.TrimSpace(obs.Dimensions["observed_state"])
	if expectedState == "" || observedState == "" || expectedState == observedState {
		return "", "", false
	}
	return expectedState, observedState, true
}

// sourceIntakeDivergenceCard builds the divergence_declared action card. The
// card id is keyed on the observation id (not the row id, which is
// record-scoped) so a re-applied identical observation resolves to the same
// card id and dedupes at the projection level instead of stacking.
func sourceIntakeDivergenceCard(row SourceIntakeRow, expectedState, observedState string) SourceIntakeActionCard {
	subject := firstNonEmpty(row.SubjectID, row.SourceRef, row.SourceKind, "source")
	evidenceRefs := append([]string{}, row.EvidenceRefs...)
	if ref := firstStringWithPrefix(row.AppliedIDs, "source_observation:"); ref != "" {
		evidenceRefs = append(evidenceRefs, ref)
	}
	return SourceIntakeActionCard{
		ID:            stableID("sicard_", row.ObservationID, "divergence_declared"),
		Kind:          "divergence_declared",
		Title:         fmt.Sprintf("%s declared expected %s but observed %s", subject, expectedState, observedState),
		Severity:      "high",
		RowID:         row.ID,
		ObservationID: row.ObservationID,
		Route:         row.Route,
		Owner:         row.Owner,
		Command:       sourceIntakeFirstCommand(row.NextCommand, []string{"compound", "source", "connections", "--json"}),
		RequiredInputs: []string{
			"operator review of the diverged state and an explicit remediation or accepted-state decision",
		},
		ExpectedValidation: []string{
			"expected_state and observed_state agree on a subsequent applied observation, or the divergence is explicitly accepted with a recorded reason",
			"source-intake projection stops emitting this card for the observation once resolved",
		},
		StopConditions: []string{
			"remediation requires live credentials, account access, scheduler mutation, or a production-side mutation outside this review",
		},
		Boundary:     "no live remediation, no infra mutation, no auto-admit from this card; review and decision only",
		Proof:        "rerun source intake/connections and verify the divergence_declared card cleared or remains explicitly visible with an accepted-state decision",
		EvidenceRefs: compact(evidenceRefs),
	}
}

func firstStringWithPrefix(values []string, prefix string) string {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return value
		}
	}
	return ""
}

// dedupeSourceIntakeActionCards drops any card whose id was already emitted
// earlier in the projection build (seen is shared across all rows). Existing
// card kinds key their id off the record-scoped row id, so this is a no-op
// for them; divergence_declared keys off the observation id, so a re-applied
// identical observation collapses to a single card instead of stacking one
// per record.
func dedupeSourceIntakeActionCards(cards []SourceIntakeActionCard, seen map[string]bool) []SourceIntakeActionCard {
	if len(cards) == 0 {
		return cards
	}
	out := make([]SourceIntakeActionCard, 0, len(cards))
	for _, card := range cards {
		if seen[card.ID] {
			continue
		}
		seen[card.ID] = true
		out = append(out, card)
	}
	return out
}

func sourceIntakeRouteCommand(row SourceIntakeRow, mode string) []string {
	if strings.TrimSpace(row.SourcePath) == "" {
		return []string{"compound", "source", "intake", "--json"}
	}
	return []string{"compound", "source", "observe-batch", row.SourcePath, mode, "--json"}
}

func sourceIntakeFirstCommand(commands ...[]string) []string {
	for _, command := range commands {
		if len(compact(command)) > 0 {
			return append([]string{}, command...)
		}
	}
	return nil
}

func sourceIntakeRowIsStale(row SourceIntakeRow, now time.Time) bool {
	ts := firstNonEmpty(row.FreshnessAt, row.SourceCapturedAt, row.ObservedAt, row.GeneratedAt)
	if strings.TrimSpace(ts) == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return now.UTC().Sub(t.UTC()) > sourceIntakeStaleAfter
}

func sourceIntakeDetails(report Report) map[string]sourceIntakeDetail {
	details := map[string]sourceIntakeDetail{}
	for _, result := range report.ProductApplyResults {
		details[result.ObservationID] = sourceIntakeDetail{
			status:      "applied",
			owner:       "internal/product",
			appliedIDs:  prefixedIDs("signal:", result.SignalIDs, "evidence:", result.EvidenceIDs, "transition:", result.TransitionIDs),
			nextCommand: []string{"compound", "product", "loops", "--product", report.Subject.ID, "--json"},
		}
	}
	for _, result := range report.SourceObservationApplyResults {
		details[result.ObservationID] = sourceIntakeDetail{
			status:      "applied",
			owner:       result.Owner,
			appliedIDs:  prefixedIDs("source_observation:", []string{result.SourceObservationID}, "candidate:", result.CandidateIDs, "no_promote:", result.NoPromoteIDs),
			nextCommand: append([]string{}, result.NextCommand...),
		}
	}
	for _, result := range report.KnowledgeApplyResults {
		details[result.ObservationID] = sourceIntakeDetail{
			status:      "applied",
			owner:       result.Owner,
			appliedIDs:  prefixedIDs("evidence:", result.EvidenceIDs, "candidate:", result.CandidateIDs, "extraction:", result.ExtractionIDs),
			nextCommand: append([]string{}, result.NextCommand...),
		}
	}
	for _, result := range report.RouteEvidenceApplyResults {
		details[result.ObservationID] = sourceIntakeDetail{
			status:      "applied",
			owner:       result.Owner,
			appliedIDs:  prefixedIDs("route_evidence:", []string{result.RouteEvidenceID}, "transition:", []string{result.TransitionID}),
			nextCommand: append([]string{}, result.NextCommand...),
		}
	}
	for _, result := range report.EventApplyResults {
		appliedIDs := prefixedIDs("trace:", result.TraceIDs, "source_authority:", []string{result.SourceAuthorityID})
		for _, observationID := range result.ObservationIDs {
			details[observationID] = sourceIntakeDetail{
				status:      "applied",
				owner:       result.Owner,
				appliedIDs:  appliedIDs,
				nextCommand: append([]string{}, result.NextCommand...),
			}
		}
	}
	for _, result := range report.CandidateReviewResults {
		details[result.ObservationID] = sourceIntakeDetail{
			status:      "review_recorded",
			owner:       "internal/observation",
			appliedIDs:  compact([]string{"candidate:" + result.CandidateID, "review:" + result.ReviewPath, "review_yaml:" + result.ReviewYAMLPath}),
			nextCommand: []string{"compound", "reviews", "--area", ObservationReviewArea, "--json"},
		}
	}
	return details
}

func sourceIntakeSkippedRows(report Report) map[string]string {
	out := map[string]string{}
	for _, row := range append(append([]string{}, report.SkippedApply...), report.SkippedReview...) {
		parts := strings.SplitN(row, ":", 2)
		if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
			continue
		}
		out[parts[0]] = row
	}
	return out
}

func sourceIntakeDefaultNextCommand(report Report, decision RouteDecision) []string {
	if len(decision.Command) > 0 {
		return append([]string{}, decision.Command...)
	}
	switch decision.Route {
	case RouteCandidateOnly:
		return []string{"compound", "source", "observe-batch", report.SourcePath, "--review-candidates", "--json"}
	case RouteBlockedPrivate:
		return []string{"compound", "source", "observe-batch", report.SourcePath, "--dry-run", "--json"}
	case RouteUnsupportedKind:
		return []string{"compound", "source", "observe-batch", report.SourcePath, "--dry-run", "--json"}
	default:
		return []string{"compound", "source", "intake", "--json"}
	}
}

func sourceIntakeMode(report Report) string {
	switch {
	case report.AppliedProductSignals:
		return "apply_product_signals"
	case report.AppliedSourceObservations:
		return "apply_source_observations"
	case report.AppliedKnowledge:
		return "apply_knowledge"
	case report.AppliedRouteEvidence:
		return "apply_route_evidence"
	case report.AppliedEvents:
		return "apply_events"
	case report.ReviewCandidates:
		return "review_candidates"
	default:
		return "dry_run"
	}
}

func prefixedIDs(parts ...any) []string {
	var out []string
	for i := 0; i+1 < len(parts); i += 2 {
		prefix, _ := parts[i].(string)
		ids, _ := parts[i+1].([]string)
		for _, id := range ids {
			if strings.TrimSpace(id) == "" {
				continue
			}
			out = append(out, prefix+id)
		}
	}
	return out
}

func reviewartifactUnique(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
