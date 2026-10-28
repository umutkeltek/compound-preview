package hook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
)

var (
	skillPathPattern = regexp.MustCompile(`(?i)[/.]skills/([a-z0-9][a-z0-9-]*)/SKILL(?:\.md)?`)
	claimRefPattern  = regexp.MustCompile(`claim://[A-Za-z0-9_-]+`)
)

var mutatingTranscriptTools = map[string]bool{
	"Write":        true,
	"Edit":         true,
	"MultiEdit":    true,
	"NotebookEdit": true,
}

type sessionTranscriptSummary struct {
	FirstUser        string
	LastAssistant    string
	FirstTimestamp   string
	LastTimestamp    string
	FilesChanged     []string
	SkillsFired      []string
	OperatorSpanRefs []string
	ToolCalls        int
	BashCalls        int
	ConsumedRefs     []string
	UsedDeclared     bool
	// Format is "claude" (default) or "omp" -- set by detectTranscriptFormat
	// after a one-line header sniff. Skill evidence rules are IDENTICAL
	// across formats (a Skill tool_use block counts; an input mentioning
	// skills/<name>/SKILL.md counts; prose does not). The flag is for
	// observability and future per-format routing, not for skill detection.
	Format string
}

// sniffTranscriptFormat peeks the first 4 KB of the file for a
// `{"type":"session","version":N,...}` line. omp writes such a line on
// its second JSON line (the first is "title"); claude-code never does.
// Codex 0.149.x writes `{"type":"session_meta",...}` on its first line.
// Tool calls currently use response_item custom_tool_call entries followed by
// event_msg CommandExecution items; older 0.149.x rollouts used function_call.
// The 4 KB window is generous for the bounded header sniff. The caller
// (scanSessionTranscript) MUST Seek(0,0) after this before resuming the main
// scan: we only peek, never consume.
func sniffTranscriptFormat(f *os.File) string {
	var head [4096]byte
	n, _ := f.Read(head[:])
	sc := bufio.NewScanner(bytes.NewReader(head[:n]))
	seen := 0
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		seen++
		if seen > 6 {
			break
		}
		if line[0] != '{' {
			continue
		}
		var probe struct {
			Type    string `json:"type"`
			Version int    `json:"version"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		if probe.Type == "session" && probe.Version > 0 {
			return "omp"
		}
		if probe.Type == "session_meta" || probe.Type == "response_item" {
			return "codex"
		}
	}
	return "claude"
}

type transcriptEntry struct {
	Timestamp string             `json:"timestamp"`
	TS        string             `json:"ts"`
	Type      string             `json:"type"`
	Role      string             `json:"role"`
	Content   json.RawMessage    `json:"content"`
	Message   *transcriptMessage `json:"message"`
	Payload   *transcriptMessage `json:"payload"`
	// CodexRolloutPayload carries the codex-native `{"type":"function_call","name":...,
	// "arguments":"<json string>"}` shape on response_item lines. The existing Payload
	// field cannot read it (codex omits role/content on the inner object), so a sibling
	// field is added instead of overloading Payload's meaning. Both forms are populated
	// by the same parseTranscriptEntry call; only one is non-nil per line.
	CodexRolloutPayload   *codexRolloutPayload   `json:"-"`
	CodexCommandExecution *codexCommandExecution `json:"-"`
}

type transcriptMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type transcriptContentBlock struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// codexRolloutPayload covers both older function_call entries (`arguments`) and
// current custom_tool_call entries (`input`) under response_item.payload.
type codexRolloutPayload struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Input     string `json:"input"`
	CallID    string `json:"call_id,omitempty"`
}

type codexCommandExecution struct {
	Command []string
}

// ResolveCodexRolloutPath locates the newest codex rollout JSONL for the given session id,
// honouring CODEX_HOME (with the same resolution rule codex itself uses). The rollout
// filename shape codex writes is `rollout-<unix-millis>-<session-id>.jsonl`; only the
// session-id suffix is load-bearing for this resolver -- we read the directory newest-first
// and match the trailing `<session-id>.jsonl` so a clock skew on the timestamp prefix
// (or a codex-side bump to the timestamp shape) cannot strand a SessionEnd receipt.
//
// Codex 0.149.x exposes transcript_path directly in the SessionEnd wire payload; this
// resolver is the FALLBACK for older codex releases (or for any future codex build that
// decides to omit transcript_path) so a codex SessionEnd still produces a skills receipt
// rather than a silent gap. If CODEX_HOME is unset we fall back to $HOME/.codex (the codex
// CLI's documented default), keeping the resolution close to what a human operator would
// check.
func ResolveCodexRolloutPath(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	root := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if root == "" {
		if home, err := os.UserHomeDir(); err == nil {
			root = filepath.Join(home, ".codex")
		}
	}
	if root == "" {
		return ""
	}
	// Codex writes rollouts under sessions/YYYY/MM/DD/rollout-<ts>-<id>.jsonl. The deepest
	// level (DD) is the only one that varies in practice; we walk one level deeper than
	// that so any future layout shift doesn't strand the resolver. The session-id suffix
	// is the only load-bearing match -- a codex-side change to the timestamp prefix (e.g.
	// extra precision) cannot strand a SessionEnd receipt because we key off the trailing
	// `<session-id>.jsonl` shape, not the timestamp.
	suffix := sessionID + ".jsonl"
	suffixAlt := sessionID + ".jsonl.gz" // codex has gzip-compressed mode in some builds
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	var newest string
	var newestMod time.Time
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			info, infoErr := d.Info()
			if infoErr == nil && info.ModTime().Before(cutoff) && !samePath(path, root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, suffix) && !strings.HasSuffix(path, suffixAlt) {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		if newest == "" || info.ModTime().After(newestMod) {
			newest = path
			newestMod = info.ModTime()
		}
		return nil
	})
	return newest
}

func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func buildWorklessSessionReceipt(name string, fx Fixture, now time.Time) (*envelope.Receipt, error) {
	path := strings.TrimSpace(fx.TranscriptPath)
	// Codex 0.149.x SessionEnd usually arrives with transcript_path in the wire payload,
	// but older releases and some codex-server builds omit it; fall back to deriving the
	// rollout path from session_id so the receipt is built either way. Non-codex harnesses
	// keep their existing behaviour (empty path => no receipt, exactly as before).
	if path == "" && fx.Harness == "codex" {
		path = ResolveCodexRolloutPath(fx.SessionID)
	}
	if path == "" {
		return nil, nil
	}
	summary, err := scanSessionTranscript(path)
	if err != nil {
		return nil, err
	}
	used := summary.SkillsFired
	evidenceRefs := []string{}
	if name == "SessionEnd" {
		evidenceRefs = append(evidenceRefs, summary.OperatorSpanRefs...)
	}
	rec := &envelope.Receipt{
		Header: envelope.Header{
			SchemaVersion: envelope.SVReceipt,
			ID:            idFor("rec_", fx.Harness, name, fx.SessionID, hashStr(path), ts(now)),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "hook", Name: "compound"},
			IDs:           envelope.IDs{SessionID: fx.SessionID},
			Scope:         envelope.Scope{Surface: fx.Cwd},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			EvidenceRefs:  evidenceRefs,
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Outcome:  "completed",
		MoveType: "audit_only",
		StateDelta: envelope.StateDelta{
			Changed:      len(summary.FilesChanged) > 0,
			Summary:      renderSessionReceiptSummary(summary, path),
			FilesChanged: summary.FilesChanged,
			Artifacts:    []string{},
			Verification: []string{},
		},
		Cost: envelope.ReceiptCost{ModelCalls: []string{}},
		Skills: envelope.ReceiptSkills{
			Used:               used,
			Missed:             []string{},
			Referenced:         summary.ConsumedRefs,
			ObservationMethod:  "transcript_reference_scan",
			ObservationQuality: observationQuality(summary.UsedDeclared),
			ApplicationMethod:  "skill_tool_call",
			Candidate:          []string{},
		},
		Memory:    envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}},
		Next:      envelope.ReceiptNext{SuggestedWork: []string{}},
		ProofRefs: []string{"transcript:" + path},
	}
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	return rec, nil
}

// OperatorUtterance is one attributed operator (human) message pulled from a
// session transcript: verbatim text plus provenance (1-based line number in the
// .jsonl and the entry timestamp). It is the utterance-attribution surface the
// ruling harvester consumes so it never needs a parallel transcript parser
// (compound.session_distill_source is binding: consume the existing reader, add
// attribution, do not re-extract).
type OperatorUtterance struct {
	Text      string `json:"text"`
	Timestamp string `json:"timestamp,omitempty"`
	LineNo    int    `json:"line_no"`
}

// BlockingAsk is the last assistant question in a transcript plus the nearest
// preceding operator utterance as minimal context. It shares this reader so
// ask-triage does not grow a second transcript/extraction organ.
type BlockingAsk struct {
	Text, Context, Timestamp string
	LineNo                   int
}

func LastBlockingAsk(path string) (BlockingAsk, error) {
	f, err := os.Open(path)
	if err != nil {
		return BlockingAsk{}, err
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, 64*1024)
	lineNo := 0
	context := ""
	var last BlockingAsk
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			entry, parseErr := parseTranscriptEntry([]byte(strings.TrimSpace(string(line))))
			if parseErr != nil {
				return BlockingAsk{}, parseErr
			}
			role, content := transcriptRoleContent(entry)
			text := strings.TrimSpace(textOfContent(content))
			if role == "user" {
				cleaned := cleanTranscriptUserText(text)
				if isOperatorUtterance(cleaned) {
					context = cleaned
				}
			}
			if role == "assistant" && text != "" {
				last = BlockingAsk{}
				if question := BlockingQuestion(text); question != "" {
					last = BlockingAsk{Text: question, Context: context, Timestamp: firstNonEmpty(entry.Timestamp, entry.TS), LineNo: lineNo}
				}
			}
		}
		if readErr == nil {
			continue
		}
		if readErr == io.EOF {
			break
		}
		return BlockingAsk{}, readErr
	}
	return last, nil
}

// BlockingQuestion returns a question only when the latest assistant message
// genuinely stops on it. Questions answered later in the same message and
// optional follow-up offers are completions, not operator gates.
func BlockingQuestion(text string) string {
	text = strings.TrimSpace(text)
	text = strings.TrimRight(text, "\"'`*_)]}")
	if !strings.HasSuffix(text, "?") {
		return ""
	}
	end := len(text) - 1
	start := strings.LastIndexAny(text[:end], "\n.!?")
	question := strings.TrimSpace(text[start+1:])
	lower := strings.ToLower(question)
	for _, prefix := range []string{
		"would you like me to ",
		"do you want me to ",
		"if you'd like",
		"if you would like",
		"should i also ",
		"shall i also ",
		"want me to ",
		"let me know if ",
	} {
		if strings.HasPrefix(lower, prefix) {
			return ""
		}
	}
	return question
}

// OperatorUtterances streams a session transcript and returns every genuine
// operator (human) utterance with attribution. It reuses the SAME streaming,
// entry parsing (parseTranscriptEntry), content extraction (textOfContent) and
// user-text cleaning (cleanTranscriptUserText) as scanSessionTranscript — it only
// adds per-utterance line/timestamp attribution and drops user-role entries that
// are not the operator speaking: tool results, slash-command echoes, local-command
// output, task notifications, and injected resume/interrupt markers. It is the
// one thin addition allowed by the harvest packet; it does not classify or judge.
func OperatorUtterances(path string) ([]OperatorUtterance, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []OperatorUtterance
	reader := bufio.NewReaderSize(f, 64*1024)
	lineNo := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			trimmed := strings.TrimSpace(string(line))
			if trimmed != "" {
				entry, parseErr := parseTranscriptEntry([]byte(trimmed))
				if parseErr != nil {
					return nil, parseErr
				}
				if u, ok := operatorUtteranceFromEntry(entry, lineNo); ok {
					out = append(out, u)
				}
			}
		}
		if readErr == nil {
			continue
		}
		if readErr == io.EOF {
			break
		}
		return nil, readErr
	}
	return out, nil
}

func operatorUtteranceFromEntry(entry transcriptEntry, lineNo int) (OperatorUtterance, bool) {
	role, content := transcriptRoleContent(entry)
	if role == "" {
		role = strings.TrimSpace(entry.Type)
	}
	if role != "user" {
		return OperatorUtterance{}, false
	}
	cleaned := cleanTranscriptUserText(textOfContent(content))
	if !isOperatorUtterance(cleaned) {
		return OperatorUtterance{}, false
	}
	return OperatorUtterance{
		Text:      cleaned,
		Timestamp: strings.TrimSpace(firstNonEmpty(entry.Timestamp, entry.TS)),
		LineNo:    lineNo,
	}, true
}

// operatorUtteranceRejectMarkers are substrings that mark a user-role entry as
// harness/tool machinery rather than the operator speaking. Kept conservative:
// the classifier is the real precision gate, this only drops obvious non-operator
// content so it never reaches the gate.
var operatorUtteranceRejectMarkers = []string{
	"<command-name>", "<command-message>", "<command-args>",
	"<local-command", "</local-command",
	"<task-notification>", "<task-id>",
	"[request interrupted",
	"messages below were generated by the user while running local commands",
	"resume the paused workflow by calling",
	"caveat: the messages below",
	// Tool/skill injections that arrive with role=user but are machinery, not the
	// operator speaking (found leaking through in the live 2026-07-11 dry-run).
	"base directory for this skill",
	"contents of the skill file",
	"here is useful information about the environment",
	"the following mcp servers",
}

func isOperatorUtterance(cleaned string) bool {
	if len([]rune(cleaned)) < 15 {
		return false
	}
	lower := strings.ToLower(cleaned)
	for _, marker := range operatorUtteranceRejectMarkers {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return true
}

func scanSessionTranscript(path string) (sessionTranscriptSummary, error) {
	var out sessionTranscriptSummary
	out.Format = "claude" // safe default -- the pre-slice-3 behaviour
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()

	files := map[string]bool{}
	skills := map[string]bool{}
	// Format sniff: peek the first 4 KB for a `{"type":"session","version":N,...}`
	// line, then rewind. omp's session line is line 2 (line 1 is "title");
	// peeking the head lets us classify before the main scan, so a future
	// per-format router can branch on summary.Format. Skill-evidence rules
	// are IDENTICAL across formats either way -- entry.Message's fallback in
	// transcriptRoleContent already maps omp tool_use blocks to the same
	// toolBlocks shape claude uses (F7 pins this end-to-end).
	out.Format = sniffTranscriptFormat(f)
	if _, err := f.Seek(0, 0); err != nil {
		return out, err
	}

	reader := bufio.NewReaderSize(f, 64*1024)
	lineNo := 0
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			trimmed := strings.TrimSpace(string(line))
			if trimmed != "" {
				// Skill detection deliberately does NOT scan this raw line. The line
				// carries assistant PROSE as well as tool calls, so a scan here counted
				// "the model mentioned /skills/x/SKILL.md" as "the model used skill x" --
				// and Skills.Used is the consumed signal capgraph scores capabilities by
				// (probe_group_usage.go, injection_observation.go), so talking about a
				// skill raised its score and made it likelier to be offered next. The
				// signal the raw scan was reaching for (a skill loaded by READING its
				// SKILL.md, which emits no Skill block) is preserved in applyEntry, where
				// tool-call inputs are available and prose is not.
				entry, parseErr := parseTranscriptEntry([]byte(trimmed))
				if parseErr != nil {
					return sessionTranscriptSummary{}, parseErr
				}
				if utterance, ok := operatorUtteranceFromEntry(entry, lineNo); ok {
					out.OperatorSpanRefs = append(out.OperatorSpanRefs, transcriptSpanEvidenceRef(path, utterance))
				}
				out.applyEntry(entry, files, skills)
			}
		}
		if err == nil {
			continue
		}
		if err == io.EOF {
			break
		}
		return sessionTranscriptSummary{}, err
	}
	out.FilesChanged = sortedKeys(files)
	out.SkillsFired = sortedKeys(skills)
	consumed, declared, err := detectTranscriptConsumption(path, nil)
	if err != nil {
		return sessionTranscriptSummary{}, err
	}
	out.ConsumedRefs = consumed
	out.UsedDeclared = declared
	return out, nil
}

func transcriptSpanEvidenceRef(path string, utterance OperatorUtterance) string {
	return fmt.Sprintf("transcript-span:sha256:%s:line:%d:sha256:%s", hashStr(path), utterance.LineNo, hashStr(utterance.Text))
}

func parseTranscriptEntry(line []byte) (transcriptEntry, error) {
	var entry transcriptEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return entry, err
	}
	// Codex 0.149.x response_item lines carry their function_call under an untyped
	// `payload` field; the generic transcriptEntry.Payload (a *transcriptMessage
	// looking for `role`/`content`) is the wrong shape. Decode a sibling struct
	// here so applyEntry can switch on Type == "response_item" and reuse the
	// existing tool-block semantics for skill detection. Parsing is gated on the
	// entry's Type so non-codex lines never pay the cost.
	if entry.Type == "response_item" {
		var probe struct {
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(line, &probe); err == nil && len(probe.Payload) > 0 {
			var pl codexRolloutPayload
			if err := json.Unmarshal(probe.Payload, &pl); err == nil {
				entry.CodexRolloutPayload = &pl
			}
		}
	}
	if entry.Type == "event_msg" {
		var probe struct {
			Payload struct {
				Item struct {
					Type    string   `json:"type"`
					Command []string `json:"command"`
				} `json:"item"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(line, &probe); err == nil &&
			probe.Payload.Item.Type == "CommandExecution" &&
			len(probe.Payload.Item.Command) > 0 {
			entry.CodexCommandExecution = &codexCommandExecution{Command: probe.Payload.Item.Command}
		}
	}
	return entry, nil
}

func (s *sessionTranscriptSummary) applyEntry(entry transcriptEntry, files, skills map[string]bool) {
	timestamp := strings.TrimSpace(firstNonEmpty(entry.Timestamp, entry.TS))
	if timestamp != "" {
		if s.FirstTimestamp == "" {
			s.FirstTimestamp = timestamp
		}
		s.LastTimestamp = timestamp
	}
	// Codex 0.149.x response_item lines do not carry role/content; their tool call
	// arrives under CodexRolloutPayload as a name + raw arguments JSON string. Map
	// that into the same tool-block semantics so the existing Skill / skill-path /
	// files-changed rules apply without duplication. The role guard below is a
	// no-op for codex (role is empty for response_item), so we short-circuit on
	// codex FIRST to keep the assistant-role prose branch honest.
	if entry.CodexRolloutPayload != nil {
		s.applyCodexRollout(entry.CodexRolloutPayload, files, skills)
		return
	}
	if entry.CodexCommandExecution != nil {
		// Current Codex also emits a normalized CommandExecution event after the
		// custom tool call. Re-scan its argv for skill/file evidence, but do not
		// increment ToolCalls a second time.
		command := strings.Join(entry.CodexCommandExecution.Command, " ")
		for _, skill := range skillsFromLine(command) {
			skills[skill] = true
		}
		return
	}
	role, content := transcriptRoleContent(entry)
	if role == "" {
		role = strings.TrimSpace(entry.Type)
	}
	text := textOfContent(content)
	if role == "user" && s.FirstUser == "" {
		cleaned := cleanTranscriptUserText(text)
		if len(cleaned) > 10 {
			s.FirstUser = cleaned
		}
	}
	if role != "assistant" {
		return
	}
	if strings.TrimSpace(text) != "" {
		s.LastAssistant = strings.TrimSpace(text)
	}
	for _, block := range toolBlocks(content) {
		s.ToolCalls++
		if block.Name == "Bash" {
			s.BashCalls++
		}
		if block.Name == "Skill" {
			if skill := skillFromInput(block.Input); skill != "" {
				skills[skill] = true
			}
		}
		// A skill can also be loaded WITHOUT a Skill block -- by reading or catting its
		// SKILL.md. That is genuine use and must still count, so scan this tool call's
		// INPUT for a skill path. Scoped to the input (not the surrounding prose, and only
		// under the assistant-role guard above) so that naming a skill is never mistaken
		// for invoking one. Pinned by TestSkillsFiredCountsToolUseNotProse.
		for _, skill := range skillsFromLine(fmt.Sprint(block.Input)) {
			skills[skill] = true
		}
		if mutatingTranscriptTools[block.Name] {
			if path := toolInputPath(block.Input); path != "" {
				files[path] = true
			}
		}
	}
}

// applyCodexRollout folds one Codex response_item into the summary. Both the
// legacy function_call arguments JSON and current custom_tool_call JavaScript
// input carry the exact command, so skill-path evidence is never inferred from
// assistant prose.
func (s *sessionTranscriptSummary) applyCodexRollout(pl *codexRolloutPayload, files, skills map[string]bool) {
	if pl.Type != "function_call" && pl.Type != "custom_tool_call" {
		return
	}
	s.ToolCalls++
	if pl.Name == "Bash" || pl.Name == "exec" {
		s.BashCalls++
	}
	rawInput := pl.Arguments
	if pl.Type == "custom_tool_call" {
		rawInput = pl.Input
	}
	input := codexArgumentsToMap(rawInput)
	if pl.Name == "Skill" {
		if skill := skillFromInput(input); skill != "" {
			skills[skill] = true
		}
	}
	for _, skill := range skillsFromLine(rawInput) {
		skills[skill] = true
	}
	if mutatingTranscriptTools[pl.Name] {
		if path := toolInputPath(input); path != "" {
			files[path] = true
		}
	}
}

// codexArgumentsToMap decodes codex's raw `arguments` JSON string into a map so the
// existing skillFromInput / toolInputPath / skillsFromLine rules can treat it
// uniformly. An empty / unparseable arguments string is a silent empty input --
// not an error, because codex 0.149.x does emit argumentless function calls for
// some meta tools, and a noisy failure here would overclaim parser breakage.
func codexArgumentsToMap(raw string) map[string]any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		return nil
	}
	return m
}

func textOfContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []transcriptContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if (block.Type == "text" || block.Type == "input_text" || block.Type == "output_text") && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, strings.TrimSpace(block.Text))
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func transcriptRoleContent(entry transcriptEntry) (string, json.RawMessage) {
	role := strings.TrimSpace(entry.Role)
	content := entry.Content
	for _, nested := range []*transcriptMessage{entry.Message, entry.Payload} {
		if nested == nil {
			continue
		}
		role = firstNonEmpty(nested.Role, role)
		if len(nested.Content) > 0 {
			content = nested.Content
		}
	}
	return role, content
}

func toolBlocks(raw json.RawMessage) []transcriptContentBlock {
	if len(raw) == 0 {
		return nil
	}
	var blocks []transcriptContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	out := make([]transcriptContentBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "tool_use" {
			out = append(out, block)
		}
	}
	return out
}

func toolInputPath(input map[string]any) string {
	for _, key := range []string{"file_path", "path"} {
		if raw, ok := input[key]; ok {
			if s := strings.TrimSpace(fmt.Sprint(raw)); s != "" && s != "<nil>" {
				return s
			}
		}
	}
	return ""
}

func skillFromInput(input map[string]any) string {
	for _, key := range []string{"skill", "command"} {
		if raw, ok := input[key]; ok {
			if ref := normalizeSkillRef(fmt.Sprint(raw)); ref != "" {
				return ref
			}
		}
	}
	return ""
}

func skillsFromLine(line string) []string {
	matches := skillPathPattern.FindAllStringSubmatch(line, -1)
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		if ref := normalizeSkillRef(match[1]); ref != "" {
			out = append(out, ref)
		}
	}
	return out
}

func normalizeSkillRef(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, "/")
	s = strings.TrimPrefix(s, "skill:")
	if s == "" || s == "skills" || s == "name" || s == "example" {
		return ""
	}
	if strings.HasPrefix(s, "capability://skill:") {
		return s
	}
	return "capability://skill:" + s
}

func cleanTranscriptUserText(s string) string {
	s = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?s)<local-command-.*?>`).ReplaceAllString(s, "")
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

func renderSessionReceiptSummary(summary sessionTranscriptSummary, transcriptPath string) string {
	return strings.Join([]string{
		"task_ref: " + transcriptTextRef(summary.FirstUser),
		"outcome_hint_ref: " + transcriptTextRef(summary.LastAssistant),
		fmt.Sprintf("tool_calls: %d (bash: %d)", summary.ToolCalls, summary.BashCalls),
		"window: " + firstNonEmpty(summary.FirstTimestamp, "?") + " -> " + firstNonEmpty(summary.LastTimestamp, "?"),
		"transcript: " + transcriptPath,
	}, "\n")
}

func transcriptTextRef(value string) string {
	if strings.TrimSpace(value) == "" {
		return "none"
	}
	return "sha256:" + hashStr(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func clipTranscriptText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mergeUniqueStrings(groups ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, group := range groups {
		for _, value := range group {
			value = strings.TrimSpace(value)
			if value == "" || seen[value] {
				continue
			}
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func transcriptInjectedClaimRefs(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	seen := map[string]bool{}
	var refs []string
	reader := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			for _, ref := range claimRefPattern.FindAllString(string(line), -1) {
				if !seen[ref] {
					seen[ref] = true
					refs = append(refs, ref)
				}
			}
		}
		if err == nil {
			continue
		}
		if err == io.EOF {
			break
		}
		return nil, err
	}
	return refs, nil
}

// matchConsumedClaimsFromFile is the production streaming path (bounded memory regardless of
// transcript size — matches scanSessionTranscript's own streaming discipline). It shares
// buildClaimMatchTargets/matchTargetHitsLine with matchConsumedClaims (consumption.go) so the
// streamed and whole-string entry points can never drift: v1 exact-id OR v2 content-key, per line.
func matchConsumedClaimsFromFile(path string, injected []injectedClaim) ([]string, error) {
	if len(injected) == 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	targets := buildClaimMatchTargets(injected)
	consumedSet := map[string]bool{}
	reader := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			for i := range targets {
				if !consumedSet[targets[i].Ref] && matchTargetHitsLine(targets[i], line) {
					consumedSet[targets[i].Ref] = true
				}
			}
		}
		if err == nil {
			continue
		}
		if err == io.EOF {
			break
		}
		return nil, err
	}
	consumed := make([]string, 0, len(consumedSet))
	for _, t := range targets {
		if consumedSet[t.Ref] {
			consumed = append(consumed, t.Ref)
		}
	}
	return consumed, nil
}

func receiptEventRef(rec *envelope.Receipt) (string, error) {
	ref, err := eventstore.ReferenceForID(rec.ID)
	if err != nil {
		return "", err
	}
	return ref.String(), nil
}
