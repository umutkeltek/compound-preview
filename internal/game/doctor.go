package game

import (
	"sort"
	"time"

	"compound/internal/sourceobs"
)

type DoctorFinding struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type DoctorReport struct {
	Schema     string          `json:"schema"`
	Generated  time.Time       `json:"generated_at"`
	StaleAfter time.Duration   `json:"stale_after"`
	Counts     map[string]int  `json:"counts"`
	Codes      map[string]int  `json:"codes"`
	Findings   []DoctorFinding `json:"findings"`
	Warnings   []string        `json:"warnings,omitempty"`
}

func Doctor(homePath string, now time.Time) (DoctorReport, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	r := DoctorReport{
		Schema:     SchemaGameDoctor,
		Generated:  now,
		StaleAfter: staleAfter,
		Counts:     map[string]int{},
		Codes:      map[string]int{},
	}

	list, err := sourceobs.ListSourceObservations(homePath, now)
	if err != nil {
		r.Warnings = append(r.Warnings, "list source observations: "+err.Error())
		return r, nil
	}

	total := 0
	fresh := 0
	stale := 0
	byKind := map[string]int{}
	bySource := map[string]int{}
	for _, it := range list.Items {
		k, _, _, md, ok := parseGameSubject(it.Subject)
		if !ok {
			continue
		}
		total++
		byKind[k]++
		src := md["source"]
		if src == "" {
			src = inferSourceSystem(it.SourceRef, it.EvidenceRefs)
		}
		if src != "" {
			bySource[src]++
		}
		created := parseCreated(it.CreatedAt)
		if !created.IsZero() && now.Sub(created) > staleAfter {
			stale++
			r.Findings = append(r.Findings, DoctorFinding{
				Code:   "stale_observation",
				Detail: "game:" + k + " last seen " + it.CreatedAt,
			})
		} else {
			fresh++
		}
	}

	r.Counts["total_game_observations"] = total
	r.Counts["fresh"] = fresh
	r.Counts["stale"] = stale
	for k, v := range byKind {
		r.Counts["kind:"+k] = v
	}
	for s, v := range bySource {
		r.Counts["source:"+s] = v
	}

	if total == 0 {
		r.Codes["no_observations"] = 1
		r.Findings = append(r.Findings, DoctorFinding{
			Code:   "no_observations",
			Detail: "no game-source observations recorded under COMPOUND_HOME; run `compound game observe` first",
		})
	} else if fresh > 0 && stale == 0 {
		r.Codes["fresh"] = 1
	}
	if stale > 0 {
		r.Codes["stale_observation"] = stale
	}
	for s, v := range bySource {
		if v == 0 {
			r.Codes["source_dark:"+s] = 1
		}
	}

	sort.Slice(r.Findings, func(i, j int) bool {
		if r.Findings[i].Code != r.Findings[j].Code {
			return r.Findings[i].Code < r.Findings[j].Code
		}
		return r.Findings[i].Detail < r.Findings[j].Detail
	})

	return r, nil
}
