package distiller

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const ConversationMiningSourceClass = "conversation_mining"

type ConversationMiningOptions struct {
	MinedTagsPath string
	LossinessPath string
}

type ConversationMiningReportFile struct {
	OK             bool           `json:"ok"`
	UpdatedAt      string         `json:"updated_at"`
	Queue          string         `json:"queue"`
	Processed      int            `json:"processed"`
	Accepted       int            `json:"accepted"`
	Rejected       int            `json:"rejected"`
	Providers      map[string]int `json:"providers"`
	Out            string         `json:"out"`
	Report         string         `json:"report"`
	DreamPack      string         `json:"dream_pack"`
	DreamPackFiles int            `json:"dream_pack_files"`
	Note           string         `json:"note"`
}

type ConversationMinedTag struct {
	AcceptForDream  bool                        `json:"accept_for_dream"`
	Importance      int                         `json:"importance"`
	KnowledgeType   string                      `json:"knowledge_type"`
	QualityScore    int                         `json:"quality_score"`
	Tags            []string                    `json:"tags"`
	DecisionReasons []string                    `json:"decision_reasons"`
	Summary         string                      `json:"summary"`
	ModelProvider   string                      `json:"model_provider"`
	ModelName       string                      `json:"model_name"`
	Candidate       ConversationMiningCandidate `json:"candidate"`
}

type ConversationMiningCandidate struct {
	Source         string   `json:"source"`
	RawPath        string   `json:"raw_path"`
	CleanPath      string   `json:"clean_path"`
	CleanStatus    string   `json:"clean_status"`
	Stable         bool     `json:"stable"`
	Importance     int      `json:"importance"`
	QualityScore   int      `json:"quality_score"`
	KnowledgeType  string   `json:"knowledge_type"`
	DreamCandidate bool     `json:"dream_candidate"`
	Reasons        []string `json:"reasons"`
	RetainedRatio  float64  `json:"retained_ratio"`
	Excerpt        string   `json:"excerpt"`
}

type ConversationLossinessReport struct {
	KnownLossyRules         map[string]any `json:"known_lossy_rules"`
	ClaudeCleanRawSizeRatio float64        `json:"claude_clean_vs_raw_size_ratio"`
	CodexCleanRawSizeRatio  float64        `json:"codex_clean_vs_raw_size_ratio"`
}

type ConversationCoverageReportFile struct {
	OK        bool                                  `json:"ok"`
	UpdatedAt string                                `json:"updated_at"`
	Sources   map[string]ConversationCoverageSource `json:"sources"`
	TotalRows int                                   `json:"total_rows"`
	TopTags   []ConversationCoverageTopTag          `json:"top_tags"`
}

type ConversationCoverageSource struct {
	RawFiles          int     `json:"raw_files"`
	MappedClean       int     `json:"mapped_clean"`
	UnmappedRaw       int     `json:"unmapped_raw"`
	Stable            int     `json:"stable"`
	RawMB             float64 `json:"raw_mb"`
	CleanMB           float64 `json:"clean_mb"`
	AvgRetainedRatio  float64 `json:"avg_retained_ratio"`
	DreamCandidates   int     `json:"dream_candidates"`
	MissingEmbeddings int     `json:"missing_embeddings"`
}

type ConversationCoverageTopTag struct {
	Source         string   `json:"source"`
	RawBasename    string   `json:"raw_basename"`
	CleanStatus    string   `json:"clean_status"`
	Importance     int      `json:"importance"`
	QualityScore   int      `json:"quality_score"`
	KnowledgeType  string   `json:"knowledge_type"`
	DreamCandidate bool     `json:"dream_candidate"`
	Reasons        []string `json:"reasons"`
}

type ConversationCoverageImport struct {
	ID                string
	ReportPath        string
	Revision          string
	ReportDigest      string
	Report            ConversationCoverageReportFile
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

type ConversationMiningImport struct {
	ID                  string
	ReportPath          string
	MinedTagsPath       string
	LossinessPath       string
	Revision            string
	ReportDigest        string
	MinedTagsDigest     string
	LossinessDigest     string
	Report              ConversationMiningReportFile
	MinedRows           int
	AcceptedForReview   int
	Providers           map[string]int
	TopTags             []string
	LossinessRecorded   bool
	LossinessKnownRules bool
	LossinessDetail     string
	EvidenceRefs        []string
}

func LoadConversationCoverage(reportPath string) (ConversationCoverageImport, error) {
	var out ConversationCoverageImport
	reportPath = strings.TrimSpace(reportPath)
	if reportPath == "" {
		return out, fmt.Errorf("conversation coverage report path required")
	}
	reportData, reportDigest, err := readDigest(reportPath)
	if err != nil {
		return out, fmt.Errorf("read conversation coverage report %s: %w", reportPath, err)
	}
	var report ConversationCoverageReportFile
	if err := json.Unmarshal(reportData, &report); err != nil {
		return out, fmt.Errorf("parse conversation coverage report %s: %w", reportPath, err)
	}
	if report.Sources == nil {
		report.Sources = map[string]ConversationCoverageSource{}
	}
	out.ReportPath = reportPath
	out.ReportDigest = reportDigest
	out.Report = report
	out.SourceCount = len(report.Sources)
	out.TotalRows = report.TotalRows
	out.TopTagCount = len(report.TopTags)
	out.EvidenceRefs = append(out.EvidenceRefs, "conversation_coverage_report:sha256:"+reportDigest)
	var retainedSum float64
	var retainedCount int
	for name, source := range report.Sources {
		out.RawFiles += source.RawFiles
		out.MappedClean += source.MappedClean
		out.UnmappedRaw += source.UnmappedRaw
		out.Stable += source.Stable
		out.DreamCandidates += source.DreamCandidates
		out.MissingEmbeddings += source.MissingEmbeddings
		if source.AvgRetainedRatio > 0 {
			retainedSum += source.AvgRetainedRatio
			retainedCount++
		}
		if source.UnmappedRaw > 0 {
			out.ReviewDebt = append(out.ReviewDebt, fmt.Sprintf("%s_unmapped_raw=%d", name, source.UnmappedRaw))
		}
		if source.MissingEmbeddings > 0 {
			out.ReviewDebt = append(out.ReviewDebt, fmt.Sprintf("%s_missing_embeddings=%d", name, source.MissingEmbeddings))
		}
		if source.DreamCandidates > 0 {
			out.ReviewDebt = append(out.ReviewDebt, fmt.Sprintf("%s_dream_candidates=%d", name, source.DreamCandidates))
		}
		if source.AvgRetainedRatio > 0 && source.AvgRetainedRatio < 0.10 {
			out.ReviewDebt = append(out.ReviewDebt, fmt.Sprintf("%s_low_retained_ratio=%.3f", name, source.AvgRetainedRatio))
		}
	}
	if retainedCount > 0 {
		out.AvgRetainedRatio = retainedSum / float64(retainedCount)
	}
	out.Revision = reportDigest
	out.ID = "conversation_coverage:" + digestParts(filepath.Base(reportPath), reportDigest)[:12]
	return out, nil
}

func LoadConversationMining(reportPath string, opts ConversationMiningOptions) (ConversationMiningImport, error) {
	var out ConversationMiningImport
	reportPath = strings.TrimSpace(reportPath)
	if reportPath == "" {
		return out, fmt.Errorf("conversation mining report path required")
	}
	reportData, reportDigest, err := readDigest(reportPath)
	if err != nil {
		return out, fmt.Errorf("read conversation mining report %s: %w", reportPath, err)
	}
	var report ConversationMiningReportFile
	if err := json.Unmarshal(reportData, &report); err != nil {
		return out, fmt.Errorf("parse conversation mining report %s: %w", reportPath, err)
	}
	if report.Providers == nil {
		report.Providers = map[string]int{}
	}
	out.ReportPath = reportPath
	out.ReportDigest = reportDigest
	out.Report = report
	out.Providers = report.Providers
	out.EvidenceRefs = append(out.EvidenceRefs, "conversation_mining_report:sha256:"+reportDigest)

	tagsPath := firstNonEmpty(opts.MinedTagsPath, report.Out)
	if tagsPath != "" {
		rows, digest, err := readMinedTags(tagsPath)
		if err != nil {
			return out, err
		}
		out.MinedTagsPath = tagsPath
		out.MinedTagsDigest = digest
		out.MinedRows = len(rows)
		out.AcceptedForReview = acceptedConversationRows(rows)
		out.TopTags = topConversationTags(rows, 8)
		out.EvidenceRefs = append(out.EvidenceRefs, "conversation_mined_tags:sha256:"+digest)
	}

	if lossinessPath := strings.TrimSpace(opts.LossinessPath); lossinessPath != "" {
		lossiness, digest, err := readLossiness(lossinessPath)
		if err != nil {
			return out, err
		}
		out.LossinessPath = lossinessPath
		out.LossinessDigest = digest
		out.LossinessRecorded = true
		out.LossinessKnownRules = len(lossiness.KnownLossyRules) > 0
		out.LossinessDetail = fmt.Sprintf("claude_ratio=%.3f codex_ratio=%.3f known_rules=%t", lossiness.ClaudeCleanRawSizeRatio, lossiness.CodexCleanRawSizeRatio, out.LossinessKnownRules)
		out.EvidenceRefs = append(out.EvidenceRefs, "conversation_lossiness:sha256:"+digest)
	}

	out.Revision = reportDigest
	if out.MinedTagsDigest != "" {
		out.Revision = digestParts(reportDigest, out.MinedTagsDigest)
	}
	if out.LossinessDigest != "" {
		out.Revision = digestParts(out.Revision, out.LossinessDigest)
	}
	out.ID = "conversation_mining:" + digestParts(filepath.Base(reportPath), out.Revision)[:12]
	return out, nil
}

func (m ConversationMiningImport) Status() string {
	if !m.Report.OK {
		return "review"
	}
	if m.MinedTagsPath != "" && m.MinedRows != m.Report.Accepted {
		return "review"
	}
	return "ok"
}

func (m ConversationMiningImport) DreamExecution() string {
	return "gated"
}

func readMinedTags(path string) ([]ConversationMinedTag, string, error) {
	data, digest, err := readDigest(path)
	if err != nil {
		return nil, "", fmt.Errorf("read mined tags %s: %w", path, err)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var rows []ConversationMinedTag
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var row ConversationMinedTag
		if err := json.Unmarshal([]byte(text), &row); err != nil {
			return nil, "", fmt.Errorf("parse mined tags %s line %d: %w", path, line, err)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("scan mined tags %s: %w", path, err)
	}
	return rows, digest, nil
}

func readLossiness(path string) (ConversationLossinessReport, string, error) {
	data, digest, err := readDigest(path)
	if err != nil {
		return ConversationLossinessReport{}, "", fmt.Errorf("read conversation lossiness %s: %w", path, err)
	}
	var report ConversationLossinessReport
	if err := json.Unmarshal(data, &report); err != nil {
		return ConversationLossinessReport{}, "", fmt.Errorf("parse conversation lossiness %s: %w", path, err)
	}
	return report, digest, nil
}

func readDigest(path string) ([]byte, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

func acceptedConversationRows(rows []ConversationMinedTag) int {
	total := 0
	for _, row := range rows {
		if row.AcceptForDream {
			total++
		}
	}
	return total
}

func topConversationTags(rows []ConversationMinedTag, limit int) []string {
	counts := map[string]int{}
	for _, row := range rows {
		for _, tag := range row.Tags {
			tag = strings.TrimSpace(tag)
			if tag == "" {
				continue
			}
			counts[tag]++
		}
	}
	items := make([]tagCount, 0, len(counts))
	for tag, count := range counts {
		items = append(items, tagCount{tag: tag, count: count})
	}
	sortTagCounts(items)
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.tag)
	}
	return out
}

func sortTagCounts(items []tagCount) {
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if items[j].count > items[i].count || (items[j].count == items[i].count && items[j].tag < items[i].tag) {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
}

type tagCount struct {
	tag   string
	count int
}

func digestParts(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
