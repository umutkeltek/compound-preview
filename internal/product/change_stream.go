package product

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"compound/internal/writeback"
)

const ProductChangeStreamOwner = "product_change_stream_candidate"

type ChangeStreamDocument struct {
	SourcePath    string
	SourceRef     string
	SourceHash    string
	GeneratedAt   string
	SourceCapture string
	Candidates    []ChangeStreamCandidate
}

type ChangeStreamCandidate struct {
	ID             string
	ProductSlug    string
	ProductID      string
	Surface        string
	Kind           string
	Score          int
	ScoreDetail    string
	Summary        string
	RepoPath       string
	RepoBranch     string
	DirtyPreview   []string
	PromoteCommand string
}

type ChangeStreamImportReport struct {
	Status         string   `json:"status"`
	SourceRef      string   `json:"source_ref"`
	SourceHash     string   `json:"source_hash"`
	SourceCapture  string   `json:"source_capture,omitempty"`
	CandidatesRead int      `json:"candidates_read"`
	IssuesCreated  int      `json:"issues_created"`
	IssuesExisting int      `json:"issues_existing"`
	IssueIDs       []string `json:"issue_ids"`
	ProductIDs     []string `json:"product_ids"`
}

func LoadChangeStreamCandidates(path string) (ChangeStreamDocument, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ChangeStreamDocument{}, err
	}
	doc := ChangeStreamDocument{
		SourcePath: path,
		SourceRef:  "change_stream_candidates:" + filepath.Base(path),
		SourceHash: "sha256:" + hashBytes(b),
	}
	var current *ChangeStreamCandidate
	inDirtyPreview := false
	inCommand := false
	var command []string
	finish := func() {
		if current == nil {
			return
		}
		if len(command) > 0 {
			current.PromoteCommand = strings.TrimSpace(strings.Join(command, "\n"))
			command = nil
		}
		current.ID = stableID("pcs_", doc.SourceHash, current.ProductID, current.Surface, current.RepoPath, current.Summary)
		doc.Candidates = append(doc.Candidates, *current)
		current = nil
		inDirtyPreview = false
		inCommand = false
	}
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "Generated:"):
			doc.GeneratedAt = strings.TrimSpace(strings.TrimPrefix(line, "Generated:"))
			continue
		case strings.HasPrefix(line, "Source capture:"):
			doc.SourceCapture = trimBackticks(strings.TrimSpace(strings.TrimPrefix(line, "Source capture:")))
			continue
		case strings.HasPrefix(line, "## "):
			finish()
			c := parseChangeStreamHeading(line)
			current = &c
			continue
		}
		if current == nil {
			continue
		}
		if strings.HasPrefix(line, "```") {
			inCommand = !inCommand
			continue
		}
		if inCommand {
			command = append(command, raw)
			continue
		}
		switch {
		case strings.HasPrefix(line, "- Kind:"):
			current.Kind = strings.TrimSpace(strings.TrimPrefix(line, "- Kind:"))
			inDirtyPreview = false
		case strings.HasPrefix(line, "- Score:"):
			current.Score, current.ScoreDetail = parseScoreLine(strings.TrimSpace(strings.TrimPrefix(line, "- Score:")))
			inDirtyPreview = false
		case strings.HasPrefix(line, "- Summary:"):
			current.Summary = strings.TrimSpace(strings.TrimPrefix(line, "- Summary:"))
			inDirtyPreview = false
		case strings.HasPrefix(line, "- Repo:"):
			current.RepoPath, current.RepoBranch = parseRepoLine(strings.TrimSpace(strings.TrimPrefix(line, "- Repo:")))
			inDirtyPreview = false
		case strings.HasPrefix(line, "- Dirty preview:"):
			inDirtyPreview = true
		case strings.HasPrefix(line, "- Promote if real:"):
			inDirtyPreview = false
		case inDirtyPreview && strings.HasPrefix(line, "- "):
			item := strings.TrimSpace(strings.TrimPrefix(line, "- "))
			current.DirtyPreview = append(current.DirtyPreview, trimBackticks(item))
		}
	}
	finish()
	if len(doc.Candidates) == 0 {
		return ChangeStreamDocument{}, fmt.Errorf("change stream %s has no candidates", path)
	}
	for i, c := range doc.Candidates {
		if strings.TrimSpace(c.ProductID) == "" || strings.TrimSpace(c.Surface) == "" || strings.TrimSpace(c.Summary) == "" {
			return ChangeStreamDocument{}, fmt.Errorf("change stream candidate %d missing product surface summary", i+1)
		}
	}
	return doc, nil
}

func ApplyChangeStreamCandidates(homePath string, doc ChangeStreamDocument, now time.Time) (ChangeStreamImportReport, error) {
	store, err := writeback.Open(homePath)
	if err != nil {
		return ChangeStreamImportReport{}, err
	}
	report := ChangeStreamImportReport{
		Status:         "ok",
		SourceRef:      doc.SourceRef,
		SourceHash:     doc.SourceHash,
		SourceCapture:  doc.SourceCapture,
		CandidatesRead: len(doc.Candidates),
	}
	productSeen := map[string]bool{}
	for _, c := range doc.Candidates {
		nop, appended, err := store.RecordNoPromote(ChangeStreamNoPromote(doc, c), now)
		if err != nil {
			report.Status = "fail"
			return report, err
		}
		if appended {
			report.IssuesCreated++
		} else {
			report.IssuesExisting++
		}
		report.IssueIDs = append(report.IssueIDs, nop.ID)
		if !productSeen[c.ProductID] {
			productSeen[c.ProductID] = true
			report.ProductIDs = append(report.ProductIDs, c.ProductID)
		}
	}
	return report, nil
}

func ChangeStreamNoPromote(doc ChangeStreamDocument, c ChangeStreamCandidate) writeback.NoPromoteDecision {
	evidenceRefs := compact([]string{
		doc.SourceRef,
		doc.SourceHash,
		"source_capture:" + doc.SourceCapture,
		"change_stream_candidate:" + c.ID,
		productRef(c.ProductID),
		"surface:" + normalizeIDPart(c.Surface),
		"repo:" + c.RepoPath,
		"branch:" + c.RepoBranch,
		"kind:" + c.Kind,
		fmt.Sprintf("score:%d", c.Score),
		fmt.Sprintf("dirty_preview:%d", len(c.DirtyPreview)),
	})
	return writeback.NoPromoteDecision{
		SchemaVersion: writeback.NoPromoteSchemaVersion,
		CandidateRef:  ProductChangeStreamOwner + ":" + c.ID,
		OwnerTarget: writeback.OwnerTarget{
			Kind: ProductChangeStreamOwner,
			Ref:  firstNonEmpty(c.RepoPath, doc.SourceRef),
		},
		Basis:        fmt.Sprintf("change-stream candidate score %d %s on %s: %s; review before treating as product change", c.Score, c.Kind, c.Surface, c.Summary),
		EvidenceRefs: evidenceRefs,
		Status:       writeback.NoPromoteActive,
	}
}

func parseChangeStreamHeading(line string) ChangeStreamCandidate {
	title := strings.TrimSpace(strings.TrimPrefix(line, "## "))
	if dot := strings.Index(title, "."); dot >= 0 {
		title = strings.TrimSpace(title[dot+1:])
	}
	productName := title
	surface := "unknown"
	if parts := strings.SplitN(title, " — ", 2); len(parts) == 2 {
		productName = parts[0]
		surface = parts[1]
	} else if parts := strings.SplitN(title, " - ", 2); len(parts) == 2 {
		productName = parts[0]
		surface = parts[1]
	}
	slug := normalizeIDPart(productName)
	return ChangeStreamCandidate{
		ProductSlug: slug,
		ProductID:   "product:" + slug,
		Surface:     strings.TrimSpace(surface),
	}
}

func parseScoreLine(value string) (int, string) {
	detail := ""
	if start := strings.Index(value, "("); start >= 0 {
		detail = strings.TrimSpace(strings.TrimSuffix(value[start+1:], ")"))
		value = strings.TrimSpace(value[:start])
	}
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return 0, detail
	}
	score, _ := strconv.Atoi(fields[0])
	return score, detail
}

func parseRepoLine(value string) (string, string) {
	repo := value
	if start := strings.Index(value, "`"); start >= 0 {
		rest := value[start+1:]
		if end := strings.Index(rest, "`"); end >= 0 {
			repo = rest[:end]
		}
	}
	branch := ""
	if idx := strings.Index(value, " branch "); idx >= 0 {
		branch = trimBackticks(strings.TrimSpace(value[idx+len(" branch "):]))
	}
	return strings.TrimSpace(repo), strings.TrimSpace(branch)
}

func trimBackticks(value string) string {
	return strings.Trim(strings.TrimSpace(value), "`")
}
