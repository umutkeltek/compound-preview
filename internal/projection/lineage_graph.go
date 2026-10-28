package projection

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"compound/internal/candidates"
	"compound/internal/work"
)

const (
	MemoryEgoGraphSchemaVersion = "compound.memory_ego_graph_projection.v0.1"
	LineageSchemaVersion        = "compound.lineage_projection.v0.1"
)

type MemoryPosition struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type MemoryGraphNode struct {
	ID         string         `json:"id"`
	Entity     string         `json:"entity"`
	Predicate  string         `json:"predicate"`
	Object     string         `json:"object"`
	Confidence *float64       `json:"confidence,omitempty"`
	Position   MemoryPosition `json:"position"`
}

type MemoryGraphLink struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
}

type MemoryEgoGraphProjection struct {
	SchemaVersion string            `json:"schema_version"`
	Seed          string            `json:"seed"`
	ResolvedAs    string            `json:"resolvedAs"`
	Nodes         []MemoryGraphNode `json:"nodes"`
	Links         []MemoryGraphLink `json:"links"`
	Truncated     bool              `json:"truncated"`
	Found         bool              `json:"found"`
}

type LineageStep struct {
	Kind      string   `json:"kind"`
	Label     string   `json:"label"`
	Detail    string   `json:"detail,omitempty"`
	Timestamp string   `json:"timestamp,omitempty"`
	Refs      []string `json:"refs"`
}

type LineageProjection struct {
	SchemaVersion string        `json:"schema_version"`
	QueryID       string        `json:"queryId"`
	ResolvedAs    string        `json:"resolvedAs"`
	Found         bool          `json:"found"`
	Steps         []LineageStep `json:"steps"`
}

type jsonRow map[string]any

func BuildMemoryEgoGraph(homePath, seed string, maxNodes int) (MemoryEgoGraphProjection, error) {
	seed = strings.TrimSpace(seed)
	if maxNodes <= 0 {
		maxNodes = 60
	}
	report := MemoryEgoGraphProjection{
		SchemaVersion: MemoryEgoGraphSchemaVersion,
		Seed:          seed,
		ResolvedAs:    "unknown " + seed,
		Nodes:         []MemoryGraphNode{},
		Links:         []MemoryGraphLink{},
	}
	ledger := filepath.Join(homePath, "ledger")
	claims, err := readProjectionJSONL(filepath.Join(ledger, "claims.jsonl"))
	if err != nil {
		return report, err
	}
	edges, err := readProjectionJSONL(filepath.Join(ledger, "edges.jsonl"))
	if err != nil {
		return report, err
	}
	claimByID := map[string]jsonRow{}
	for _, claim := range claims {
		if id := rowString(claim, "id"); id != "" {
			claimByID[id] = claim
		}
	}
	seedClaims := make([]jsonRow, 0)
	for _, claim := range claims {
		if rowString(claim, "entity") == seed || rowString(claim, "id") == seed {
			seedClaims = append(seedClaims, claim)
		}
	}
	if len(seedClaims) == 0 {
		report.ResolvedAs = "unknown " + seed
		return report, nil
	}
	report.Found = true
	if rowString(seedClaims[0], "id") == seed {
		report.ResolvedAs = "claim " + seed
	} else {
		report.ResolvedAs = "entity " + seed
	}
	seedIDs := map[string]bool{}
	seedIDList := make([]string, 0, len(seedClaims))
	for _, claim := range seedClaims {
		id := rowString(claim, "id")
		if id == "" || seedIDs[id] {
			continue
		}
		seedIDs[id] = true
		seedIDList = append(seedIDList, id)
	}
	links := make([]MemoryGraphLink, 0)
	neighborIDs := map[string]bool{}
	for _, edge := range edges {
		from := rowString(edge, "from")
		to := rowString(edge, "to")
		if from == "" || to == "" {
			continue
		}
		if seedIDs[from] || seedIDs[to] {
			id := "e_" + rowString(edge, "seq")
			if id == "e_" {
				id = "e_" + from + "_" + to
			}
			links = append(links, MemoryGraphLink{ID: id, Source: from, Target: to, Kind: rowString(edge, "kind")})
			if !seedIDs[from] {
				neighborIDs[from] = true
			}
			if !seedIDs[to] {
				neighborIDs[to] = true
			}
		}
	}
	neighborIDList := sortedKeys(neighborIDs)
	if len(seedIDList)+len(neighborIDList) > maxNodes {
		limit := maxNodes - len(seedIDList)
		if limit < 0 {
			limit = 0
		}
		neighborIDList = neighborIDList[:limit]
		report.Truncated = true
	}
	positions := map[string]MemoryPosition{}
	ringPositions(positions, seedIDList, 140, 0)
	ringPositions(positions, neighborIDList, 360, 0.4)
	visibleIDs := map[string]bool{}
	for _, id := range seedIDList {
		visibleIDs[id] = true
	}
	for _, id := range neighborIDList {
		visibleIDs[id] = true
	}
	nodes := make([]MemoryGraphNode, 0, len(visibleIDs))
	for _, id := range append(append([]string{}, seedIDList...), neighborIDList...) {
		claim := claimByID[id]
		if claim == nil {
			continue
		}
		conf, ok := rowFloat(claim, "confidence")
		var confPtr *float64
		if ok {
			confPtr = &conf
		}
		nodes = append(nodes, MemoryGraphNode{
			ID:         id,
			Entity:     rowString(claim, "entity"),
			Predicate:  rowString(claim, "predicate"),
			Object:     truncate(rowString(claim, "object"), 80),
			Confidence: confPtr,
			Position:   positions[id],
		})
	}
	visibleLinks := make([]MemoryGraphLink, 0, len(links))
	for _, link := range links {
		if visibleIDs[link.Source] && visibleIDs[link.Target] {
			visibleLinks = append(visibleLinks, link)
		}
	}
	report.Nodes = nodes
	report.Links = visibleLinks
	return report, nil
}

func BuildLineage(homePath, queryID string) (LineageProjection, error) {
	id := strings.TrimSpace(queryID)
	report := LineageProjection{
		SchemaVersion: LineageSchemaVersion,
		QueryID:       id,
		ResolvedAs:    id,
		Steps:         []LineageStep{},
	}
	ledger := filepath.Join(homePath, "ledger")
	claims, err := readProjectionJSONL(filepath.Join(ledger, "claims.jsonl"))
	if err != nil {
		return report, err
	}
	evidence, err := readProjectionJSONL(filepath.Join(ledger, "evidence.jsonl"))
	if err != nil {
		return report, err
	}
	adjudications, err := readProjectionJSONL(filepath.Join(ledger, "adjudications.jsonl"))
	if err != nil {
		return report, err
	}
	evidenceByID := map[string]jsonRow{}
	for _, ev := range evidence {
		if evID := rowString(ev, "id"); evID != "" {
			evidenceByID[evID] = ev
		}
	}
	pushEvidence := func(evID string) {
		ev := evidenceByID[evID]
		if ev == nil {
			return
		}
		report.Steps = append(report.Steps, LineageStep{
			Kind:      "evidence",
			Label:     rowString(ev, "id"),
			Detail:    firstNonEmptyString(truncate(rowString(ev, "quote_text"), 180), rowString(ev, "source_kind")+" evidence"),
			Timestamp: rowString(ev, "observed_at"),
			Refs:      []string{rowString(ev, "id")},
		})
		sourceKind := rowString(ev, "source_kind")
		sourceRef := firstNonEmptyString(rowString(ev, "source_id"), rowString(ev, "source_ref"))
		if sourceKind != "" || sourceRef != "" {
			refs := []string{}
			if sourceRef != "" {
				refs = append(refs, sourceRef)
			}
			report.Steps = append(report.Steps, LineageStep{
				Kind:      "source",
				Label:     firstNonEmptyString(sourceKind, "source"),
				Detail:    sourceRef,
				Timestamp: rowString(ev, "observed_at"),
				Refs:      refs,
			})
		}
	}
	pushClaim := func(claim jsonRow) {
		predicate := rowString(claim, "predicate")
		object := truncate(rowString(claim, "object"), 180)
		detail := object
		if predicate != "" {
			detail = predicate + ": " + object
		}
		report.Steps = append(report.Steps, LineageStep{
			Kind:      "claim",
			Label:     rowString(claim, "id"),
			Detail:    detail,
			Timestamp: firstNonEmptyString(rowString(claim, "valid_from"), rowString(claim, "created_at")),
			Refs:      []string{rowString(claim, "id")},
		})
		for _, ref := range rowStringSlice(claim, "evidence_refs") {
			pushEvidence(ref)
		}
	}
	if strings.HasPrefix(id, "cand_") {
		report.ResolvedAs = "candidate " + id
		candidate, err := readCandidateProjection(homePath, id)
		if err != nil {
			return report, err
		}
		if candidate == nil {
			return report, nil
		}
		report.Found = true
		report.Steps = append(report.Steps, LineageStep{
			Kind:      "candidate",
			Label:     id,
			Detail:    rowString(candidate, "entity") + " - " + rowString(candidate, "predicate") + ": " + truncate(rowString(candidate, "object"), 140),
			Timestamp: rowString(candidate, "created_at"),
			Refs:      []string{id},
		})
		if trace := rowString(candidate, "source_trace_ref"); strings.HasPrefix(trace, "ev_") {
			pushEvidence(trace)
		}
		for _, adj := range adjudications {
			if rowString(adj, "candidate_id") != id {
				continue
			}
			report.Steps = append(report.Steps, LineageStep{
				Kind:      "adjudication",
				Label:     firstNonEmptyString(rowString(adj, "verdict"), "adjudicated"),
				Detail:    basisDetail(rowString(adj, "basis")),
				Timestamp: rowString(adj, "created_at"),
				Refs:      []string{id},
			})
			if rowString(adj, "verdict") == "promote" {
				if promoted := firstClaimByEntity(claims, rowString(candidate, "entity")); promoted != nil {
					pushClaim(promoted)
				}
			}
			break
		}
		return report, nil
	}
	if strings.HasPrefix(id, "ev_") {
		report.ResolvedAs = "evidence " + id
		if evidenceByID[id] == nil {
			return report, nil
		}
		report.Found = true
		pushEvidence(id)
		for _, claim := range claims {
			if containsString(rowStringSlice(claim, "evidence_refs"), id) {
				pushClaim(claim)
				if len(report.Steps) > 12 {
					break
				}
			}
		}
		return report, nil
	}
	if strings.HasPrefix(id, "work_") {
		report.ResolvedAs = "work " + id
		store, err := work.Open(homePath)
		if err != nil {
			return report, err
		}
		w, err := store.Get(id)
		if err != nil {
			return report, nil
		}
		report.Found = true
		report.Steps = append(report.Steps, LineageStep{
			Kind:      "work",
			Label:     id,
			Detail:    firstNonEmptyString(w.DesiredOutcome, w.SuccessPredicate),
			Timestamp: w.CreatedAt,
			Refs:      []string{id},
		})
		refs := []string{}
		labels := []string{}
		if receiptID := firstWithPrefix(w.ActionIntentIDs, "rec_"); receiptID != "" {
			refs = append(refs, receiptID)
			labels = append(labels, "receipt "+receiptID)
		}
		if w.IDs.TransitionID != "" {
			refs = append(refs, w.IDs.TransitionID)
			labels = append(labels, "transition "+w.IDs.TransitionID)
		}
		if len(refs) > 0 {
			report.Steps = append(report.Steps, LineageStep{Kind: "receipt", Label: strings.Join(labels, " · "), Refs: refs})
		}
		return report, nil
	}
	for _, claim := range claims {
		if rowString(claim, "id") == id {
			report.ResolvedAs = "claim " + id
			report.Found = true
			pushClaim(claim)
			return report, nil
		}
	}
	entityClaims := []jsonRow{}
	for _, claim := range claims {
		if rowString(claim, "entity") == id {
			entityClaims = append(entityClaims, claim)
		}
	}
	if len(entityClaims) > 0 {
		report.ResolvedAs = "entity " + id
		report.Found = true
		for i, claim := range entityClaims {
			if i >= 6 {
				break
			}
			pushClaim(claim)
		}
		return report, nil
	}
	report.ResolvedAs = "unknown " + id
	return report, nil
}

func readProjectionJSONL(path string) ([]jsonRow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []jsonRow{}, nil
		}
		return nil, fmt.Errorf("read projection jsonl %s: %w", path, err)
	}
	rows := []jsonRow{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row jsonRow
		if err := json.Unmarshal([]byte(line), &row); err == nil {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func readCandidateProjection(homePath, id string) (jsonRow, error) {
	record, err := candidates.ReadRecord(homePath, candidates.Memory, id)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read memory candidate %q: %w", id, err)
	}
	var row jsonRow
	if err := json.Unmarshal(record.Body, &row); err != nil {
		return nil, err
	}
	return row, nil
}

func ringPositions(positions map[string]MemoryPosition, ids []string, radius int, offset float64) {
	n := len(ids)
	for i, id := range ids {
		if _, exists := positions[id]; exists {
			continue
		}
		angle := offset + (float64(i)/math.Max(float64(n), 1))*2*math.Pi
		positions[id] = MemoryPosition{
			X: int(math.Round(float64(radius) * math.Cos(angle))),
			Y: int(math.Round(float64(radius) * math.Sin(angle))),
		}
	}
}

func sortedKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func rowString(row jsonRow, key string) string {
	value, ok := row[key]
	if !ok || value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	case float64:
		if math.Trunc(v) == v {
			return fmt.Sprintf("%.0f", v)
		}
		return fmt.Sprintf("%g", v)
	default:
		return fmt.Sprint(v)
	}
}

func rowFloat(row jsonRow, key string) (float64, bool) {
	value, ok := row[key]
	if !ok {
		return 0, false
	}
	v, ok := value.(float64)
	return v, ok
}

func rowStringSlice(row jsonRow, key string) []string {
	raw, ok := row[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s := strings.TrimSpace(fmt.Sprint(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func firstClaimByEntity(claims []jsonRow, entity string) jsonRow {
	for _, claim := range claims {
		if rowString(claim, "entity") == entity {
			return claim
		}
	}
	return nil
}

func basisDetail(basis string) string {
	if strings.TrimSpace(basis) == "" {
		return ""
	}
	return "basis: " + strings.TrimSpace(basis)
}

func firstWithPrefix(values []string, prefix string) string {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return value
		}
	}
	return ""
}
