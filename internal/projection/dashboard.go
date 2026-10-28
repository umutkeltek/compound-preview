package projection

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	DefaultDashboardMaxAgeHours = 24
	dashboardProjectionID       = "projection:dashboard-read-model"
)

// DashboardReadModel is the generic shape of a bounded operator/control
// dashboard. Cards are projection evidence only; they are not authority.
type DashboardReadModel struct {
	Version     int             `json:"version"`
	GeneratedAt string          `json:"generated_at"`
	SourcePaths []string        `json:"source_paths"`
	Cards       []DashboardCard `json:"cards"`
}

type DashboardCard struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Lane        string   `json:"lane"`
	Status      string   `json:"status"`
	Owner       string   `json:"owner"`
	SourcePaths []string `json:"source_paths"`
	LastUpdated string   `json:"last_updated"`
	NextAction  string   `json:"next_action"`
	Visibility  string   `json:"visibility"`
}

// DoctorDashboardReadModel adapts a dashboard read model into ProjectionFreshness.
// It checks schema shape, source coverage, source path existence/digests, and age.
func DoctorDashboardReadModel(path string, maxAgeHours int, now time.Time) (Freshness, error) {
	if maxAgeHours <= 0 {
		maxAgeHours = DefaultDashboardMaxAgeHours
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Freshness{}, fmt.Errorf("read dashboard read model %s: %w", path, err)
	}
	var model DashboardReadModel
	if err := json.Unmarshal(data, &model); err != nil {
		return Freshness{}, fmt.Errorf("parse dashboard read model %s: %w", path, err)
	}

	viewRef := filepath.Clean(path)
	f := Freshness{
		ProjectionID:    dashboardProjectionID,
		ViewRef:         viewRef,
		SourceLedgerSeq: int64(model.Version),
		BuiltAt:         model.GeneratedAt,
		CoverageVerdict: "complete",
		Status:          StatusFresh,
		OutputHash:      sha(data),
		Sources:         []SourceObservation{},
		Checks:          []Check{},
	}
	add := func(name string, ok bool, detail string) {
		status := "ok"
		if !ok {
			status = "fail"
			if f.Status == StatusFresh {
				f.Status = StatusDrifted
			}
		}
		f.Checks = append(f.Checks, Check{Name: name, Status: status, Detail: detail})
	}

	add("identity", strings.TrimSpace(f.ViewRef) != "" && f.SourceLedgerSeq > 0, fmt.Sprintf("version:%d", model.Version))
	generatedAt, generatedErr := time.Parse(time.RFC3339, strings.TrimSpace(model.GeneratedAt))
	add("generated_at", generatedErr == nil, model.GeneratedAt)
	add("cards", len(model.Cards) > 0, fmt.Sprintf("%d card(s)", len(model.Cards)))

	cardSourceMissing := 0
	for _, card := range model.Cards {
		if len(card.SourcePaths) == 0 {
			cardSourceMissing++
		}
	}
	add("card_sources", cardSourceMissing == 0, fmt.Sprintf("%d card(s) missing source paths", cardSourceMissing))

	sourceRefs := dashboardSourceRefs(model)
	add("source_paths", len(sourceRefs) > 0, fmt.Sprintf("%d source path(s)", len(sourceRefs)))
	missingSources := 0
	for _, ref := range sourceRefs {
		digest, ok := dashboardSourceDigest(filepath.Dir(path), ref)
		status := "ok"
		if !ok {
			status = "fail"
			missingSources++
			if f.Status == StatusFresh {
				f.Status = StatusDrifted
			}
		}
		f.Sources = append(f.Sources, SourceObservation{SourceRef: ref, CurrentHash: digest, Status: status})
	}
	add("source_hashes", missingSources == 0 && len(sourceRefs) > 0, fmt.Sprintf("%d missing source path(s)", missingSources))

	coverageOK := cardSourceMissing == 0 && missingSources == 0 && len(sourceRefs) > 0
	if !coverageOK {
		f.CoverageVerdict = "partial"
		if f.Status == StatusFresh {
			f.Status = StatusStale
		}
	}
	f.Checks = append(f.Checks, Check{Name: "coverage", Status: boolStatus(coverageOK), Detail: f.CoverageVerdict})

	if generatedErr == nil {
		age := now.Sub(generatedAt)
		if age < 0 {
			age = 0
		}
		f.Staleness = age.Round(time.Second).String()
		ageOK := age <= time.Duration(maxAgeHours)*time.Hour
		if !ageOK && f.Status == StatusFresh {
			f.Status = StatusStale
		}
		f.Checks = append(f.Checks, Check{Name: "age", Status: boolStatus(ageOK), Detail: f.Staleness})
	}
	return f, nil
}

func dashboardSourceRefs(model DashboardReadModel) []string {
	seen := map[string]bool{}
	add := func(ref string, out *[]string) {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		*out = append(*out, ref)
	}
	out := []string{}
	for _, ref := range model.SourcePaths {
		add(ref, &out)
	}
	for _, card := range model.Cards {
		for _, ref := range card.SourcePaths {
			add(ref, &out)
		}
	}
	sort.Strings(out)
	return out
}

func dashboardSourceDigest(baseDir, ref string) (string, bool) {
	path := expandDashboardPath(baseDir, ref)
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		digest, err := digestDir(path)
		if err != nil {
			return "", false
		}
		return digest, true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return sha(data), true
}

func expandDashboardPath(baseDir, ref string) string {
	if strings.HasPrefix(ref, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(ref, "~/"))
		}
	}
	if filepath.IsAbs(ref) {
		return ref
	}
	return filepath.Join(baseDir, ref)
}

func digestDir(root string) (string, error) {
	entries := []string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		kind := "file"
		if d.IsDir() {
			kind = "dir"
		}
		entries = append(entries, fmt.Sprintf("%s\t%s\t%d\t%d", kind, rel, info.Size(), info.ModTime().UTC().UnixNano()))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(entries)
	return sha([]byte(strings.Join(entries, "\n"))), nil
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
