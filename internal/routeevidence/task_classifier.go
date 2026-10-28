package routeevidence

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

const (
	CapabilityTaskClassifierSchemaVersion = "compound.capability_task_classifier.v0.1"
	CapabilityTaskClassifierAuthorityRef  = "contract://capability-task-classifier"
)

// TaskClassification is the deterministic task-to-route-class decision used
// before planning against the Compound-owned model-router authority.
type TaskClassification struct {
	SchemaVersion      string `json:"schema_version"`
	AuthorityRef       string `json:"authority_ref"`
	AuthorityRevision  string `json:"authority_revision"`
	RuleID             string `json:"rule_id"`
	CapabilityClass    string `json:"capability_class"`
	DeterministicFirst string `json:"deterministic_first"`
	EvaluationMethod   string `json:"evaluation_method"`
}

type taskClassificationRule struct {
	id                 string
	capabilityClass    string
	pattern            *regexp.Regexp
	deterministicFirst string
	evaluationMethod   string
}

var taskClassificationRules = []taskClassificationRule{
	taskRule("mechanical-edit", "mechanical-edit", `\b(format|rename|move|bulk|mechanical|lint|cleanup|codemod)\b`, "formatter/linter/codemod/script first", "diff + formatter/test output"),
	taskRule("tests-from-spec", "tests-from-spec", `\b(test|tdd|spec|coverage|regression|fixture)\b`, "test runner first; LLM only drafts tests from explicit behavior", "failing-then-passing test evidence"),
	taskRule("localization", "localization", `\b(locali[sz]e|translate|locale|i18n|language)\b`, "string catalog/export tooling first", "spot check + UI length/meaning review"),
	taskRule("credential-certification", "certify", `\b(invalid_grant|auth lanes?|gog auth|auth doctor|oauth refresh|refresh .*oauth|refresh token|expired credential|revoked credential|credential (repair|recovery|refresh)|re-?auth|force-consent)\b`, "doctor/auth CLI first; user consent gates persistent auth changes", "doctor output + prompt/receipt evidence"),
	taskRule("compound-control-plane", "plan-decompose", `\b(compound|context[- ]pack(?:et)?|capability/context|adaptive capability (?:os|runtime)|capability runtime|capability loop|context packet loop)\b`, "existing capability/context owners first, then bounded proof packet", "context packet + resolver acceptance + run receipt evidence"),
	taskRule("frontend-build", "build-implement", `\b(frontend|front end|ui|ux|interface|visual polish|visuals?|screenshots?|image generation|generate .*image|mockups?|store visuals?|app store .*screenshots?|product surface|make .*better|improve .*frontend|frontend .*generic)\b`, "capture current surface/screenshot or target component first; then visual audit or scoped implementation", "before/after screenshot, diff, and runtime/test evidence"),
	taskRule("classify-extract", "classify-extract", `\b(extract|classify|cluster|tag|summari[sz]e|mine|ingest|parse|scrape|crawl|stats|metrics)\b`, "parser/API/CLI first; small model for bounded classification", "schema validation + sampled source checks"),
	taskRule("draft-prose", "draft-prose", `\b(copy|email|reply|reddit|post|metadata|sales|proposal|description|write|rewrite|marketing|draft|prose|headline|tagline|wording|blurb)\b`, "source facts + template/checklist first", "fact check + tone/approval gate"),
	taskRule("judge-critique", "judge-critique", `\b(review|audit|critique|evaluate|compare|score|risk|red.?team|decision)\b`, "existing checklist/eval first", "finding list with evidence; free lanes tripwire only"),
	taskRule("plan-decompose", "plan-decompose", `\b(plan|architecture|design|pipeline|roadmap|prioriti[sz]e|strategy|synthesis|system)\b`, "existing contract/registry first, then plan", "clear next packet + acceptance tests"),
	taskRule("release-certification", "certify", `\b(certify|ship|release|publish|validate|approval|qa|app store|production)\b`, "release/checklist/test gates first", "receipt + release gate evidence"),
	taskRule("build-implement", "build-implement", `\b(build|implement|fix|debug|feature|refactor|code|patch)\b`, "reproduce/run tests before LLM code work", "tests + diff + runtime verification"),
}

var defaultTaskClassificationRule = taskClassificationRule{
	id:                 "default-classify-extract",
	capabilityClass:    "classify-extract",
	deterministicFirst: "deterministic tool/script first",
	evaluationMethod:   "schema/sample validation",
}

var capabilityTaskClassifierRevision = taskClassifierRevision(taskClassificationRules, defaultTaskClassificationRule)

func taskRule(id, capabilityClass, pattern, deterministicFirst, evaluationMethod string) taskClassificationRule {
	return taskClassificationRule{
		id:                 id,
		capabilityClass:    capabilityClass,
		pattern:            regexp.MustCompile(`(?i)` + pattern),
		deterministicFirst: deterministicFirst,
		evaluationMethod:   evaluationMethod,
	}
}

// ClassifyCapabilityTask applies the first matching owned rule. The default is
// bounded classification/extraction, matching the prior compatibility surface.
func ClassifyCapabilityTask(task string) TaskClassification {
	task = strings.TrimSpace(task)
	for _, rule := range taskClassificationRules {
		if rule.pattern.MatchString(task) {
			return TaskClassification{
				SchemaVersion:      CapabilityTaskClassifierSchemaVersion,
				AuthorityRef:       CapabilityTaskClassifierAuthorityRef,
				AuthorityRevision:  capabilityTaskClassifierRevision,
				RuleID:             rule.id,
				CapabilityClass:    rule.capabilityClass,
				DeterministicFirst: rule.deterministicFirst,
				EvaluationMethod:   rule.evaluationMethod,
			}
		}
	}
	fallback := defaultTaskClassificationRule
	return TaskClassification{
		SchemaVersion:      CapabilityTaskClassifierSchemaVersion,
		AuthorityRef:       CapabilityTaskClassifierAuthorityRef,
		AuthorityRevision:  capabilityTaskClassifierRevision,
		RuleID:             fallback.id,
		CapabilityClass:    fallback.capabilityClass,
		DeterministicFirst: fallback.deterministicFirst,
		EvaluationMethod:   fallback.evaluationMethod,
	}
}

func taskClassifierRevision(rules []taskClassificationRule, fallback taskClassificationRule) string {
	var contract strings.Builder
	contract.WriteString(CapabilityTaskClassifierSchemaVersion)
	contract.WriteByte('\n')
	for _, rule := range rules {
		fmt.Fprintf(&contract, "%s\x00%s\x00%s\x00%s\x00%s\n", rule.id, rule.capabilityClass, rule.pattern.String(), rule.deterministicFirst, rule.evaluationMethod)
	}
	fmt.Fprintf(&contract, "default\x00%s\x00%s\x00%s\x00%s\n", fallback.id, fallback.capabilityClass, fallback.deterministicFirst, fallback.evaluationMethod)
	sum := sha256.Sum256([]byte(contract.String()))
	return fmt.Sprintf("sha256:%x", sum)
}
