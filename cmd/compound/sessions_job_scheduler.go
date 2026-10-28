package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"compound/internal/home"
	"compound/internal/reviewartifact"
)

const (
	sessionSchedulerPlanSchemaVersion   = "compound.session_scheduler_plan.v0.1"
	sessionSchedulerReviewSchemaVersion = "compound.session_scheduler_review.v0.1"
	sessionSchedulerReviewArea          = "session-job-scheduler"
)

type sessionSchedulerPlan struct {
	SchemaVersion   string          `json:"schema_version"`
	ID              string          `json:"id"`
	CreatedAt       string          `json:"created_at"`
	Status          string          `json:"status"`
	JobID           string          `json:"job_id"`
	HomePath        string          `json:"home_path"`
	Command         string          `json:"command"`
	Schedule        string          `json:"schedule"`
	TargetPath      string          `json:"target_path"`
	TargetContent   string          `json:"target_content"`
	TargetHash      string          `json:"target_hash"`
	OS              string          `json:"os"`
	User            string          `json:"user"`
	EvidenceRefs    []string        `json:"evidence_refs"`
	OperatorCard    json.RawMessage `json:"__operator_card,omitempty"`
	OperatorCardExt json.RawMessage `json:"__operator_card_ext,omitempty"`
}

type sessionSchedulerReview struct {
	SchemaVersion  string   `json:"schema_version"`
	PlanID         string   `json:"plan_id"`
	Decision       string   `json:"decision"`
	Rationale      string   `json:"rationale"`
	Confidence     *float64 `json:"confidence,omitempty"`
	NextAction     string   `json:"next_action"`
	EvidenceRefs   []string `json:"evidence_refs"`
	ReviewedBy     string   `json:"reviewed_by,omitempty"`
	ReviewedAt     string   `json:"reviewed_at"`
	SchedulerState string   `json:"scheduler_state,omitempty"`
	ProbeNote      string   `json:"probe_note,omitempty"`
}

func cmdSessionsJobPlan(args []string) {
	jsonOut := false
	for _, arg := range args {
		if arg == "--json" {
			jsonOut = true
			continue
		}
		fmt.Fprintln(os.Stderr, sessionsJobPlanUsage())
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessions job plan:", err)
		os.Exit(1)
	}
	now := time.Now()
	plan, err := buildSessionSchedulerPlan(homePath, sessionMiningJobID, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessions job plan:", err)
		os.Exit(1)
	}
	plan.Status = "review_required"
	markdown := renderSessionSchedulerPlanMarkdown(plan)
	paths, err := reviewartifact.Write(homePath, sessionSchedulerReviewArea, plan.ID, plan, "  ", markdown)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessions job plan:", err)
		os.Exit(1)
	}
	yamlPath, err := reviewartifact.WriteRaw(homePath, sessionSchedulerReviewArea, plan.ID+".review.yaml", []byte(renderSessionSchedulerReviewYAML(plan.ID)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessions job plan:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(map[string]any{
			"plan": plan, "paths": paths, "review_yaml": yamlPath,
		})
		return
	}
	fmt.Printf("# session scheduler plan\n\n")
	fmt.Printf("- plan_id: %s\n", plan.ID)
	fmt.Printf("- job_id: %s\n", plan.JobID)
	fmt.Printf("- status: %s\n", plan.Status)
	fmt.Printf("- os: %s\n", plan.OS)
	fmt.Printf("- user: %s\n", plan.User)
	fmt.Printf("- target: %s\n", plan.TargetPath)
	fmt.Printf("- plan_path: %s\n", paths.PlanPath)
	fmt.Printf("- review_yaml: %s\n", yamlPath)
	fmt.Printf("- next: operator fills in the review YAML and runs `compound sessions job review %s --decision approved`\n", yamlPath)
}

func cmdSessionsJobReview(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, sessionsJobReviewUsage())
		os.Exit(2)
	}
	reviewPath := args[0]
	fillFlags := args[1:]
	var decision, rationale, nextAction string
	var confidence *float64
	for i := 0; i < len(fillFlags); i++ {
		switch fillFlags[i] {
		case "--decision":
			if i+1 < len(fillFlags) {
				decision = fillFlags[i+1]
				i++
			}
		case "--rationale":
			if i+1 < len(fillFlags) {
				rationale = fillFlags[i+1]
				i++
			}
		case "--confidence":
			if i+1 < len(fillFlags) {
				var c float64
				if _, err := fmt.Sscanf(fillFlags[i+1], "%f", &c); err == nil && c >= 0 && c <= 1 {
					confidence = &c
				}
				i++
			}
		case "--next-action":
			if i+1 < len(fillFlags) {
				nextAction = fillFlags[i+1]
				i++
			}
		case "--json":
		default:
			fmt.Fprintln(os.Stderr, sessionsJobReviewUsage())
			os.Exit(2)
		}
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessions job review:", err)
		os.Exit(1)
	}
	reported, err := recordSessionSchedulerReview(homePath, reviewPath, sessionSchedulerReview{
		Decision:   decision,
		Rationale:  rationale,
		Confidence: confidence,
		NextAction: nextAction,
	}, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessions job review:", err)
		os.Exit(1)
	}
	printJSON(reported)
}

func cmdSessionsJobApply(args []string) {
	jsonOut := false
	for _, arg := range args {
		if arg == "--json" {
			jsonOut = true
			continue
		}
		fmt.Fprintln(os.Stderr, sessionsJobApplyUsage())
		os.Exit(2)
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessions job apply:", err)
		os.Exit(1)
	}
	result, err := applySessionSchedulerPlan(homePath, sessionMiningJobID, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessions job apply:", err)
		os.Exit(1)
	}
	if jsonOut {
		printJSON(result)
		return
	}
	fmt.Printf("# session scheduler apply\n\n")
	fmt.Printf("- status: %s\n", result["status"])
	fmt.Printf("- plan_id: %s\n", result["plan_id"])
	fmt.Printf("- target: %s\n", result["target_path"])
	if result["error"] != nil {
		fmt.Printf("- error: %s\n", result["error"])
	}
}

func sessionsJobPlanUsage() string {
	return "usage: compound sessions job plan [--json]"
}

func sessionsJobReviewUsage() string {
	return "usage: compound sessions job review <review.yaml> [--decision approved|parked|rejected] [--rationale <text>] [--confidence <0..1>] [--next-action <text>] [--json]"
}

func sessionsJobApplyUsage() string {
	return "usage: compound sessions job apply [--json]"
}

func buildSessionSchedulerPlan(homePath, jobID string, now time.Time) (sessionSchedulerPlan, error) {
	command := "compound sessions mine --since 24h --dry-run --job-run " + jobID + " --json"
	targetDir, err := launchAgentDir()
	if err != nil {
		return sessionSchedulerPlan{}, fmt.Errorf("resolve LaunchAgent directory: %w", err)
	}
	account, err := user.Current()
	if err != nil {
		return sessionSchedulerPlan{}, fmt.Errorf("current user: %w", err)
	}
	label := "com.compound." + jobID
	targetPath := filepath.Join(targetDir, label+".plist")
	content := renderLaunchAgentPlist(label, command, jobID)
	contentHash := hashSHA256(content)
	id := reviewartifact.ID("ssp_", jobID, contentHash, reviewartifact.Timestamp(now))
	return sessionSchedulerPlan{
		SchemaVersion: sessionSchedulerPlanSchemaVersion,
		ID:            id,
		CreatedAt:     reviewartifact.Timestamp(now),
		JobID:         jobID,
		HomePath:      homePath,
		Command:       command,
		Schedule:      "daily",
		TargetPath:    targetPath,
		TargetContent: content,
		TargetHash:    contentHash,
		OS:            runtime.GOOS,
		User:          account.Username,
		EvidenceRefs: []string{
			"compound://registries/job-roster",
			"file:" + filepath.Join(homePath, "projections", "sessions-mine", jobID+".json"),
		},
	}, nil
}

func renderLaunchAgentPlist(label, command, jobID string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>-c</string>
		<string>%s</string>
	</array>
	<key>StartCalendarInterval</key>
	<dict>
		<key>Hour</key>
		<integer>1</integer>
		<key>Minute</key>
		<integer>0</integer>
	</dict>
	<key>StandardOutPath</key>
	<string>/tmp/compound-%s.stdout.log</string>
	<key>StandardErrorPath</key>
	<string>/tmp/compound-%s.stderr.log</string>
	<key>RunAtLoad</key>
	<false/>
	<key>KeepAlive</key>
	<false/>
</dict>
</plist>
`, label, command, jobID, jobID)
}

func renderSessionSchedulerPlanMarkdown(plan sessionSchedulerPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Scheduler Plan: %s\n\n", plan.JobID)
	fmt.Fprintf(&b, "- **Plan ID:** %s\n", plan.ID)
	fmt.Fprintf(&b, "- **Status:** %s\n", plan.Status)
	fmt.Fprintf(&b, "- **OS:** %s\n", plan.OS)
	fmt.Fprintf(&b, "- **User:** %s\n", plan.User)
	fmt.Fprintf(&b, "- **Schedule:** %s\n", plan.Schedule)
	fmt.Fprintf(&b, "- **Command:** `%s`\n", plan.Command)
	fmt.Fprintf(&b, "- **Target:** `%s`\n", plan.TargetPath)
	fmt.Fprintf(&b, "- **Home:** `%s`\n", plan.HomePath)
	fmt.Fprintf(&b, "\n## Evidence\n\n")
	for _, ref := range plan.EvidenceRefs {
		fmt.Fprintf(&b, "- `%s`\n", ref)
	}
	fmt.Fprintf(&b, "\n## Plist Content\n\n```xml\n%s```\n", plan.TargetContent)
	fmt.Fprintf(&b, "\n## Operator Instructions\n\n")
	fmt.Fprintf(&b, "1. Review the plist content above.\n")
	fmt.Fprintf(&b, "2. Fill the review YAML and run:\n")
	fmt.Fprintf(&b, "   `compound sessions job review <review.yaml> --decision approved --rationale \"...\"`\n")
	fmt.Fprintf(&b, "3. Apply the approved plan:\n")
	fmt.Fprintf(&b, "   `compound sessions job apply`\n")
	return b.String()
}

func renderSessionSchedulerReviewYAML(planID string) string {
	return fmt.Sprintf(`schema_version: %s
plan_id: %s
decision: TODO_approved_parked_rejected
rationale: TODO_operator_rationale
confidence: TODO_confidence_0_to_1
next_action: "TODO: next operator action"
`, sessionSchedulerReviewSchemaVersion, planID)
}

func recordSessionSchedulerReview(homePath, yamlPath string, filled sessionSchedulerReview, now time.Time) (map[string]any, error) {
	if filled.Decision == "" {
		return nil, fmt.Errorf("--decision is required (approved|parked|rejected)")
	}
	if !oneOf(filled.Decision, "approved", "parked", "rejected") {
		return nil, fmt.Errorf("--decision must be approved, parked, or rejected")
	}
	if err := fillSessionSchedulerReviewYAML(yamlPath, filled, now); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("read review yaml: %w", err)
	}
	var review sessionSchedulerReview
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		switch key {
		case "plan_id":
			review.PlanID = val
		case "decision":
			review.Decision = val
		case "rationale":
			review.Rationale = val
		case "next_action":
			review.NextAction = val
		}
	}
	review.ReviewedAt = now.UTC().Format(time.RFC3339)
	review.SchemaVersion = sessionSchedulerReviewSchemaVersion

	dir, err := reviewartifact.Dir(homePath, sessionSchedulerReviewArea)
	if err != nil {
		return nil, err
	}
	planPath := filepath.Join(dir, review.PlanID+".json")
	if _, err := os.Stat(planPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("plan %s not found at %s", review.PlanID, planPath)
	}
	name := review.PlanID + "." + review.Decision + ".reviewed.json"
	reviewedPath, err := reviewartifact.WriteJSON(homePath, sessionSchedulerReviewArea, name, review, "  ")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"status":        review.Decision,
		"plan_id":       review.PlanID,
		"reviewed_path": reviewedPath,
	}, nil
}

func applySessionSchedulerPlan(homePath, jobID string, now time.Time) (map[string]any, error) {
	status, err := buildSessionMiningJobStatus(homePath, jobID)
	if err != nil {
		return nil, err
	}
	if !status.CheckpointPresent {
		return nil, fmt.Errorf("no session mining checkpoint exists; run `compound sessions mine --dry-run --job-run %s` first", jobID)
	}
	planDir, err := reviewartifact.Dir(homePath, sessionSchedulerReviewArea)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(planDir)
	if err != nil {
		return nil, fmt.Errorf("read scheduler plan directory: %w", err)
	}
	var bestPlan sessionSchedulerPlan
	var bestDecision string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".reviewed.json") {
			continue
		}
		name := entry.Name()
		if !strings.Contains(name, ".approved.") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(planDir, name))
		if err != nil {
			continue
		}
		var review sessionSchedulerReview
		if err := json.Unmarshal(data, &review); err != nil || review.PlanID == "" {
			continue
		}
		planData, err := os.ReadFile(filepath.Join(planDir, review.PlanID+".json"))
		if err != nil {
			continue
		}
		var plan sessionSchedulerPlan
		if err := json.Unmarshal(planData, &plan); err != nil {
			continue
		}
		if plan.JobID != jobID {
			continue
		}
		if bestPlan.CreatedAt == "" || plan.CreatedAt > bestPlan.CreatedAt {
			bestPlan = plan
			bestDecision = review.Decision
		}
	}
	if bestPlan.ID == "" {
		return nil, fmt.Errorf("no approved scheduler plan found for job %s; run `compound sessions job plan` and review it first", jobID)
	}
	if err := os.MkdirAll(filepath.Dir(bestPlan.TargetPath), 0o755); err != nil {
		return nil, err
	}
	if err := home.WriteFileAtomic(bestPlan.TargetPath, []byte(bestPlan.TargetContent), 0o644); err != nil {
		return nil, fmt.Errorf("write scheduler plist: %w", err)
	}
	applyRecord := map[string]any{
		"status":      "applied",
		"plan_id":     bestPlan.ID,
		"decision":    bestDecision,
		"target_path": bestPlan.TargetPath,
		"applied_at":  now.UTC().Format(time.RFC3339),
		"target_hash": bestPlan.TargetHash,
	}
	applyPath, err := reviewartifact.WriteJSON(homePath, sessionSchedulerReviewArea, bestPlan.ID+".applied.json", applyRecord, "  ")
	if err != nil {
		return nil, err
	}
	applyRecord["apply_path"] = applyPath
	return applyRecord, nil
}

func launchAgentDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, "Library", "LaunchAgents"), nil
}

func fillSessionSchedulerReviewYAML(path string, filled sessionSchedulerReview, now time.Time) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read review yaml: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		if strings.HasPrefix(strings.TrimSpace(trimmed), "decision:") && filled.Decision != "" {
			out = append(out, "decision: "+filled.Decision)
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(trimmed), "rationale:") && filled.Rationale != "" {
			out = append(out, "rationale: "+filled.Rationale)
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(trimmed), "confidence:") && filled.Confidence != nil {
			out = append(out, fmt.Sprintf("confidence: %.2f", *filled.Confidence))
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(trimmed), "next_action:") && filled.NextAction != "" {
			out = append(out, "next_action: \""+filled.NextAction+"\"")
			continue
		}
		out = append(out, trimmed)
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o644)
}

func hashSHA256(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

var oneOf = func(val string, set ...string) bool {
	for _, s := range set {
		if val == s {
			return true
		}
	}
	return false
}
