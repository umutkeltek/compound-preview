package decisions

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const SourceFormatDoctorOK = "ok"
const SourceFormatDoctorNeedsRepair = "needs_repair"

var requiredTemplateLifecycleFields = []string{"falsifier", "review_trigger"}

// SourceFormatDoctorReport checks whether a decision source template and its
// format documentation can keep accepted DecisionLifecycle records complete.
// It never imports or promotes decisions; it only verifies the source shape.
type SourceFormatDoctorReport struct {
	TemplatePath               string   `json:"template_path"`
	FormatDocPath              string   `json:"format_doc_path,omitempty"`
	Status                     string   `json:"status"`
	RequiredLifecycleFields    []string `json:"required_lifecycle_fields"`
	MissingTemplateFrontmatter []string `json:"missing_template_frontmatter,omitempty"`
	MissingTemplateGuidance    []string `json:"missing_template_guidance,omitempty"`
	MissingFormatDocumentation []string `json:"missing_format_documentation,omitempty"`
}

// DoctorSourceFormat verifies a markdown decision template and, optionally,
// the human format document that tells operators how to create decisions.
func DoctorSourceFormat(templatePath, formatDocPath string) (SourceFormatDoctorReport, error) {
	report := SourceFormatDoctorReport{
		TemplatePath:            templatePath,
		FormatDocPath:           formatDocPath,
		Status:                  SourceFormatDoctorOK,
		RequiredLifecycleFields: append([]string(nil), requiredTemplateLifecycleFields...),
	}
	template, err := os.ReadFile(templatePath)
	if err != nil {
		return report, err
	}
	fm, body := splitFrontmatter(string(template))
	keys, err := frontmatterKeys(fm)
	if err != nil {
		return report, fmt.Errorf("template frontmatter %s: %w", templatePath, err)
	}
	for _, field := range requiredTemplateLifecycleFields {
		if !keys[field] {
			report.MissingTemplateFrontmatter = append(report.MissingTemplateFrontmatter, field)
		}
		if !containsLifecycleGuidance(body, field) {
			report.MissingTemplateGuidance = append(report.MissingTemplateGuidance, field)
		}
	}
	if strings.TrimSpace(formatDocPath) != "" {
		doc, err := os.ReadFile(formatDocPath)
		if err != nil {
			return report, err
		}
		for _, field := range requiredTemplateLifecycleFields {
			if !containsLifecycleGuidance(string(doc), field) {
				report.MissingFormatDocumentation = append(report.MissingFormatDocumentation, field)
			}
		}
	}
	if len(report.MissingTemplateFrontmatter) > 0 ||
		len(report.MissingTemplateGuidance) > 0 ||
		len(report.MissingFormatDocumentation) > 0 {
		report.Status = SourceFormatDoctorNeedsRepair
	}
	return report, nil
}

func frontmatterKeys(fm string) (map[string]bool, error) {
	keys := map[string]bool{}
	if strings.TrimSpace(fm) == "" {
		return keys, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(fm), &doc); err != nil {
		return keys, err
	}
	node := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		node = doc.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return keys, fmt.Errorf("must be a YAML mapping")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := strings.TrimSpace(node.Content[i].Value)
		if key != "" {
			keys[key] = true
		}
	}
	return keys, nil
}

func containsLifecycleGuidance(s, field string) bool {
	lower := strings.ToLower(s)
	switch field {
	case "review_trigger":
		return strings.Contains(lower, "review_trigger") ||
			strings.Contains(lower, "review trigger") ||
			strings.Contains(lower, "review-trigger")
	default:
		return strings.Contains(lower, strings.ToLower(field))
	}
}
