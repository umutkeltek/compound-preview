package harness

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// tomlSettingsMode reports whether a harness authority owns its target via
// the surgical TOML line editor rather than the JSON settings reconciler.
// It activates only when the authority declares a non-empty settings map AND
// the target path ends in .toml; any other shape (including today's empty
// codex authority) keeps the existing JSON-settings path and its guard.
func tomlSettingsMode(policy PermissionAuthority) bool {
	return len(policy.Settings) > 0 && strings.HasSuffix(strings.ToLower(strings.TrimSpace(policy.TargetPath)), ".toml")
}

// ownedTOMLKey is one dotted-path settings entry resolved into the TOML
// table/key it owns.
type ownedTOMLKey struct {
	Path  string
	Table string // "" for top-level
	Key   string
	Value any
}

func parseOwnedTOMLKeys(settings map[string]any) ([]ownedTOMLKey, error) {
	paths := make([]string, 0, len(settings))
	for path := range settings {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]ownedTOMLKey, 0, len(paths))
	for _, path := range paths {
		table, key, err := splitTOMLSettingPath(path)
		if err != nil {
			return nil, err
		}
		out = append(out, ownedTOMLKey{Path: path, Table: table, Key: key, Value: settings[path]})
	}
	return out, nil
}

func splitTOMLSettingPath(path string) (table, key string, err error) {
	parts := strings.Split(path, ".")
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return "", "", fmt.Errorf("settings key %q has an empty path segment", path)
		}
	}
	switch len(parts) {
	case 1:
		return "", parts[0], nil
	case 2:
		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf("settings key %q has unsupported nesting depth; only a top-level key or a single dotted table level is supported", path)
	}
}

func formatTOMLScalar(path string, value any) (string, error) {
	switch v := value.(type) {
	case string:
		return `"` + tomlEscapeBasicString(v) + `"`, nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case int32:
		return strconv.FormatInt(int64(v), 10), nil
	case uint64:
		return strconv.FormatUint(v, 10), nil
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64), nil
	case float32:
		return strconv.FormatFloat(float64(v), 'g', -1, 32), nil
	default:
		return "", fmt.Errorf("settings key %q has unsupported value type %T; only string/bool/number scalars are supported", path, value)
	}
}

// tomlEscapeBasicString renders v as the contents of a single-line TOML basic
// string. Beyond the quote/backslash escapes, every control character (and
// DEL, which TOML forbids raw) is escaped — a raw newline in a value would
// otherwise split the assignment across physical lines: an unterminated
// string that corrupts the target file and injects value text as live
// keys/table headers.
func tomlEscapeBasicString(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// renderTOMLPermissions computes the surgical TOML edit for the declared
// owned settings against the current on-disk target. It never parses the
// file as a whole document and never rewrites bytes outside the owned-key
// lines it locates or inserts.
func renderTOMLPermissions(harness string, policy PermissionAuthority, sourceRevision string) (PermissionRender, error) {
	before, err := os.ReadFile(policy.TargetPath)
	if err != nil && !os.IsNotExist(err) {
		return PermissionRender{}, fmt.Errorf("read settings target %s: %w", policy.TargetPath, err)
	}
	beforeExisted := err == nil
	owned, err := parseOwnedTOMLKeys(policy.Settings)
	if err != nil {
		return PermissionRender{}, fmt.Errorf("settings target %s: %w", policy.TargetPath, err)
	}
	content, err := applyOwnedTOMLKeys(before, owned)
	if err != nil {
		return PermissionRender{}, fmt.Errorf("settings target %s: %w", policy.TargetPath, err)
	}
	managed := map[string][]string{}
	for _, k := range owned {
		value, err := formatTOMLScalar(k.Path, k.Value)
		if err != nil {
			return PermissionRender{}, fmt.Errorf("settings target %s: %w", policy.TargetPath, err)
		}
		managed[k.Path] = []string{value}
	}
	artifact := HarnessArtifact{
		SchemaVersion: HarnessArtifactSchemaVersion, Path: policy.TargetPath, Harness: harness,
		SourceAuthorityRef: policy.AuthorityRef, SourceRevision: sourceRevision,
		ContentSHA256:  "sha256:" + digest(content),
		ManagedEntries: managed,
	}
	return PermissionRender{
		TargetPath: policy.TargetPath,
		Changed:    !bytes.Equal(before, content),
		Content:    content,
		Preimage:   append([]byte(nil), before...),
		PreExisted: beforeExisted,
		Artifact:   artifact,
	}, nil
}

// tomlPermissionDrift compares only the owned settings keys parsed from the
// same render preimage; every other byte/line in the target is invisible to
// drift. A missing owned key is reported as drift (absent), not an error; an
// ambiguous owned key (duplicate assignment lines, or one shadowed inside a
// multi-line construct) fails closed with the same explicit error apply
// would raise.
func tomlPermissionDrift(policy PermissionAuthority, render PermissionRender, report PermissionDriftReport) (PermissionDriftReport, error) {
	owned, err := parseOwnedTOMLKeys(policy.Settings)
	if err != nil {
		return report, err
	}
	lines := splitTOMLLines(render.Preimage)
	structure := scanTOMLStructure(lines)
	for _, k := range owned {
		wantValue, err := formatTOMLScalar(k.Path, k.Value)
		if err != nil {
			return report, err
		}
		idx, found, err := locateOwnedTOMLKey(lines, structure, k)
		if err != nil {
			return report, fmt.Errorf("settings target %s: %w", render.TargetPath, err)
		}
		if !found {
			report.Details = append(report.Details, fmt.Sprintf("%s missing (want %s)", k.Path, wantValue))
			continue
		}
		gotValue := extractTOMLAssignmentValue(tomlSyntacticLineBody(lines[idx], idx), k.Key)
		if gotValue != wantValue {
			report.Details = append(report.Details, fmt.Sprintf("%s is %s, want %s", k.Path, gotValue, wantValue))
		}
	}
	report.Details = uniqueSorted(report.Details)
	report.Drifted = len(report.Details) > 0
	return report, nil
}

// --- line-level scanning/editing primitives ---

func splitTOMLLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	return strings.SplitAfter(string(content), "\n")
}

func joinTOMLLines(lines []string) []byte {
	return []byte(strings.Join(lines, ""))
}

func lineBody(raw string) string {
	if strings.HasSuffix(raw, "\r\n") {
		return raw[:len(raw)-2]
	}
	if strings.HasSuffix(raw, "\n") {
		return raw[:len(raw)-1]
	}
	return raw
}

const tomlUTF8BOM = "\uFEFF"

// tomlSyntacticLineBody removes the optional document marker from the first
// line for matching while leaving the stored line bytes untouched.
func tomlSyntacticLineBody(line string, lineIndex int) string {
	body := lineBody(line)
	if lineIndex == 0 {
		return strings.TrimPrefix(body, tomlUTF8BOM)
	}
	return body
}

func lineTerminator(raw string) string {
	return raw[len(lineBody(raw)):]
}

func dominantTOMLTerminator(lines []string) string {
	for _, raw := range lines {
		if t := lineTerminator(raw); t != "" {
			return t
		}
	}
	return "\n"
}

var (
	tomlTableHeaderRe      = regexp.MustCompile(`^\[([^\[\]]+)\]\s*(#.*)?$`)
	tomlArrayTableHeaderRe = regexp.MustCompile(`^\[\[([^\[\]]+)\]\]\s*(#.*)?$`)
)

// tomlStructure is the per-line scope map produced by a single pass over the
// file: which table (if any) a line belongs to, whether that scope is an
// array-of-tables, whether the line is a table header, and whether it lies
// inside a multi-line array or triple-quoted string (opaque to key/header
// detection).
type tomlStructure struct {
	tableAt          []string
	arrayTableAt     []bool
	opaque           []bool
	isHeader         []bool
	firstHeaderLine  map[string]int
	arrayHeaderNames map[string]bool
}

func scanTOMLStructure(lines []string) tomlStructure {
	structure := tomlStructure{
		tableAt:          make([]string, len(lines)),
		arrayTableAt:     make([]bool, len(lines)),
		opaque:           make([]bool, len(lines)),
		isHeader:         make([]bool, len(lines)),
		firstHeaderLine:  map[string]int{},
		arrayHeaderNames: map[string]bool{},
	}
	currentTable := ""
	currentArrayTable := false
	inTripleDouble, inTripleSingle := false, false
	arrayDepth := 0
	for i, raw := range lines {
		body := tomlSyntacticLineBody(raw, i)
		startedOpaque := inTripleDouble || inTripleSingle || arrayDepth > 0
		structure.opaque[i] = startedOpaque
		if !startedOpaque {
			trimmed := strings.TrimSpace(body)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				if m := tomlArrayTableHeaderRe.FindStringSubmatch(trimmed); m != nil {
					name := strings.TrimSpace(m[1])
					structure.isHeader[i] = true
					currentTable = name
					currentArrayTable = true
					structure.arrayHeaderNames[name] = true
				} else if m := tomlTableHeaderRe.FindStringSubmatch(trimmed); m != nil {
					name := strings.TrimSpace(m[1])
					structure.isHeader[i] = true
					currentTable = name
					currentArrayTable = false
					if _, ok := structure.firstHeaderLine[name]; !ok {
						structure.firstHeaderLine[name] = i
					}
				}
			}
		}
		structure.tableAt[i] = currentTable
		structure.arrayTableAt[i] = currentArrayTable
		inTripleDouble, inTripleSingle, arrayDepth = advanceTOMLLineState(body, inTripleDouble, inTripleSingle, arrayDepth)
	}
	return structure
}

// advanceTOMLLineState walks one line's bytes to update multi-line string /
// array tracking state carried into the next line. It is deliberately not a
// full TOML tokenizer: it only needs to know when a line is a continuation
// of an unterminated construct so the scanner can refuse to treat its
// contents as a fresh table header or key assignment.
func advanceTOMLLineState(body string, inTripleDouble, inTripleSingle bool, arrayDepth int) (bool, bool, int) {
	inBasic, inLiteral := false, false
	i := 0
	for i < len(body) {
		switch {
		case inTripleDouble:
			if strings.HasPrefix(body[i:], `"""`) {
				inTripleDouble = false
				i += 3
				continue
			}
			i++
		case inTripleSingle:
			if strings.HasPrefix(body[i:], `'''`) {
				inTripleSingle = false
				i += 3
				continue
			}
			i++
		case inBasic:
			if body[i] == '\\' && i+1 < len(body) {
				i += 2
				continue
			}
			if body[i] == '"' {
				inBasic = false
			}
			i++
		case inLiteral:
			if body[i] == '\'' {
				inLiteral = false
			}
			i++
		default:
			switch {
			case strings.HasPrefix(body[i:], `"""`):
				inTripleDouble = true
				i += 3
			case strings.HasPrefix(body[i:], `'''`):
				inTripleSingle = true
				i += 3
			case body[i] == '"':
				inBasic = true
				i++
			case body[i] == '\'':
				inLiteral = true
				i++
			case body[i] == '#':
				i = len(body)
			case body[i] == '[':
				arrayDepth++
				i++
			case body[i] == ']':
				if arrayDepth > 0 {
					arrayDepth--
				}
				i++
			default:
				i++
			}
		}
	}
	return inTripleDouble, inTripleSingle, arrayDepth
}

func tomlAssignmentRegex(key string) *regexp.Regexp {
	return regexp.MustCompile(`^([ \t]*)` + regexp.QuoteMeta(key) + `[ \t]*=[ \t]*(.*)$`)
}

// alternateOwnedTOMLAssignment identifies assignment spellings that TOML
// resolves to the owned path but that this bare-key editor cannot safely
// rewrite without taking ownership of additional syntax.
func alternateOwnedTOMLAssignment(body string, structure tomlStructure, line int, owned ownedTOMLKey) string {
	if structure.opaque[line] || structure.isHeader[line] || structure.arrayTableAt[line] {
		return ""
	}
	if structure.tableAt[line] == owned.Table {
		quoted := regexp.MustCompile(`^[ \t]*(?:"` + regexp.QuoteMeta(owned.Key) + `"|'` + regexp.QuoteMeta(owned.Key) + `')[ \t]*=`)
		if quoted.MatchString(body) {
			return "quoted"
		}
	}
	if owned.Table != "" && structure.tableAt[line] == "" {
		dotted := regexp.MustCompile(`^[ \t]*` + regexp.QuoteMeta(owned.Table) + `[ \t]*\.[ \t]*` + regexp.QuoteMeta(owned.Key) + `[ \t]*=`)
		if dotted.MatchString(body) {
			return "dotted"
		}
	}
	return ""
}

// locateOwnedTOMLKey finds the single assignment line owning key within its
// declared table scope. Zero matches means "absent" (not an error, so apply
// can insert). More than one candidate match — including a look-alike found
// inside a multi-line array/string continuation — fails closed, naming the
// key explicitly.
func locateOwnedTOMLKey(lines []string, structure tomlStructure, owned ownedTOMLKey) (int, bool, error) {
	re := tomlAssignmentRegex(owned.Key)
	matchIdx := -1
	matchCount := 0
	ambiguous := false
	for i, raw := range lines {
		body := tomlSyntacticLineBody(raw, i)
		if spelling := alternateOwnedTOMLAssignment(body, structure, i, owned); spelling != "" {
			return 0, false, fmt.Errorf("settings key %q uses an unsupported %s assignment spelling; refusing to edit", owned.Path, spelling)
		}
		if structure.tableAt[i] != owned.Table || structure.arrayTableAt[i] || structure.isHeader[i] {
			continue
		}
		if structure.opaque[i] {
			if re.MatchString(body) {
				ambiguous = true
			}
			continue
		}
		trimmed := strings.TrimSpace(body)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if re.MatchString(body) {
			matchCount++
			matchIdx = i
		}
	}
	if ambiguous || matchCount > 1 {
		return 0, false, fmt.Errorf("settings key %q is ambiguous in the target file (multiple or shadowed assignment candidates); refusing to edit", owned.Path)
	}
	if matchCount == 0 {
		return 0, false, nil
	}
	return matchIdx, true, nil
}

func extractTOMLAssignmentValue(body, key string) string {
	m := tomlAssignmentRegex(key).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return stripTOMLInlineComment(m[2])
}

func stripTOMLInlineComment(s string) string {
	if idx := tomlInlineCommentIndex(s); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return strings.TrimSpace(s)
}

func tomlInlineCommentIndex(s string) int {
	inBasic, inLiteral := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inBasic:
			if c == '\\' && i+1 < len(s) {
				i++
				continue
			}
			if c == '"' {
				inBasic = false
			}
		case inLiteral:
			if c == '\'' {
				inLiteral = false
			}
		default:
			switch c {
			case '"':
				inBasic = true
			case '\'':
				inLiteral = true
			case '#':
				return i
			}
		}
	}
	return -1
}

// tomlInlineCommentSuffix returns the comment and its original separating
// horizontal whitespace. Those bytes are operator-owned context around an
// owned value and must survive a value correction.
func tomlInlineCommentSuffix(s string) string {
	idx := tomlInlineCommentIndex(s)
	if idx < 0 {
		return ""
	}
	for idx > 0 && (s[idx-1] == ' ' || s[idx-1] == '\t') {
		idx--
	}
	return s[idx:]
}

func ensureTrailingTOMLNewline(lines []string, terminator string) []string {
	if len(lines) == 0 {
		return lines
	}
	last := lines[len(lines)-1]
	if last == "" {
		return lines
	}
	if lineTerminator(last) == "" {
		lines[len(lines)-1] = last + terminator
	}
	return lines
}

func insertTOMLLine(lines []string, idx int, newLine string) []string {
	out := make([]string, 0, len(lines)+1)
	if idx == 0 && len(lines) > 0 && strings.HasPrefix(lines[0], tomlUTF8BOM) {
		out = append(out, tomlUTF8BOM+newLine, strings.TrimPrefix(lines[0], tomlUTF8BOM))
		out = append(out, lines[1:]...)
		return out
	}
	out = append(out, lines[:idx]...)
	out = append(out, newLine)
	out = append(out, lines[idx:]...)
	return out
}

// applyOwnedTOMLKeys computes the fully edited file content in memory. On
// any error (ambiguity) it returns before producing content, so the caller
// never has a half-edited byte buffer to accidentally use.
func applyOwnedTOMLKeys(before []byte, owned []ownedTOMLKey) ([]byte, error) {
	lines := splitTOMLLines(before)
	terminator := dominantTOMLTerminator(lines)
	for _, k := range owned {
		value, err := formatTOMLScalar(k.Path, k.Value)
		if err != nil {
			return nil, err
		}
		structure := scanTOMLStructure(lines)
		idx, found, err := locateOwnedTOMLKey(lines, structure, k)
		if err != nil {
			return nil, err
		}
		if found {
			re := tomlAssignmentRegex(k.Key)
			body := tomlSyntacticLineBody(lines[idx], idx)
			m := re.FindStringSubmatch(body)
			if stripTOMLInlineComment(m[2]) == value {
				continue
			}
			prefix := ""
			if idx == 0 && strings.HasPrefix(lineBody(lines[idx]), tomlUTF8BOM) {
				prefix = tomlUTF8BOM
			}
			leading := m[1]
			comment := tomlInlineCommentSuffix(m[2])
			term := lineTerminator(lines[idx])
			lines[idx] = prefix + leading + k.Key + " = " + value + comment + term
			continue
		}
		if k.Table == "" {
			lines = insertTOMLLine(lines, 0, k.Key+" = "+value+terminator)
			continue
		}
		if headerIdx, ok := structure.firstHeaderLine[k.Table]; ok {
			lines = insertTOMLLine(lines, headerIdx+1, k.Key+" = "+value+terminator)
			continue
		}
		if structure.arrayHeaderNames[k.Table] {
			return nil, fmt.Errorf("owned TOML table %s exists only as an array-of-tables; refusing to insert %s", k.Table, k.Path)
		}
		lines = ensureTrailingTOMLNewline(lines, terminator)
		lines = append(lines, "["+k.Table+"]"+terminator, k.Key+" = "+value+terminator)
	}
	return joinTOMLLines(lines), nil
}
