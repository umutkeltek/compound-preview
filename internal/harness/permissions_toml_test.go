package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexTOMLFixtureBody produces a synthetic config.toml shaped like the real
// codex authority target: top-level keys, a load-bearing comment block, an
// optional [features] table (with an optional owned key inside it), and
// unrelated [projects.*]/[hooks.state] tables. Only synthetic paths are used
// (no real usernames), per the repo's public/private fixture rule.
func codexTOMLFixtureBody(guardianValue string, hasGuardianKey, hasFeaturesBlock bool) string {
	var b strings.Builder
	b.WriteString("approval_policy = \"never\"\n")
	b.WriteString("approvals_reviewer = \"user\"\n")
	b.WriteString("model = \"gpt-5-codex\"\n")
	b.WriteString("model_provider = \"openai\"\n")
	b.WriteString("model_reasoning_effort = \"high\"\n")
	b.WriteString("model_reasoning_summary = \"auto\"\n")
	b.WriteString("network_access = true\n")
	b.WriteString("sandbox_mode = \"danger-full-access\"\n")
	b.WriteString("\n")
	b.WriteString("# Incident note (synthetic): an automatic client update once disabled this\n")
	b.WriteString("# approval gate mid-session. Pin approval_policy/sandbox_mode here and drift\n")
	b.WriteString("# check them from compound so a future update cannot silently reintroduce a\n")
	b.WriteString("# prompt. Do not hand-edit the settings below without updating the posture\n")
	b.WriteString("# authority first.\n")
	b.WriteString("\n")
	if hasFeaturesBlock {
		b.WriteString("[features]\n")
		if hasGuardianKey {
			b.WriteString("guardian_approval = " + guardianValue + "\n")
		}
		b.WriteString("web_search_request = false\n")
		b.WriteString("\n")
	}
	b.WriteString("[projects.\"/repo/operator/workspace\"]\n")
	b.WriteString("trust_level = \"trusted\"\n")
	b.WriteString("\n")
	b.WriteString("[projects.\"/repo/operator\"]\n")
	b.WriteString("trust_level = \"trusted\"\n")
	b.WriteString("\n")
	b.WriteString("[hooks.state]\n")
	b.WriteString("last_synced = \"2026-07-01T00:00:00Z\"\n")
	return b.String()
}

func codexTOMLOwnedSettings() map[string]any {
	return map[string]any{
		"approval_policy":            "never",
		"sandbox_mode":               "danger-full-access",
		"features.guardian_approval": false,
	}
}

func codexTOMLAuthorityFixture(settings map[string]any) PermissionAuthority {
	return PermissionAuthority{
		SchemaVersion: PermissionAuthoritySchemaVersion, Harness: "codex", AuthorityRef: PermissionAuthorityRef("codex"),
		TargetPath:  "/synthetic/config.toml",
		Permissions: PermissionRules{Allow: []string{}, Deny: []string{}, Ask: []string{}, DefaultMode: ""},
		AutoMode:    AutoModePolicy{Allow: []string{"$defaults"}},
		Settings:    settings,
	}
}

func assertCodexTOMLDrift(t *testing.T, homePath string, want bool, detail string) {
	t.Helper()
	report, err := PermissionDrift(homePath, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if report.Drifted != want {
		t.Fatalf("drifted=%v want=%v details=%v", report.Drifted, want, report.Details)
	}
	if detail != "" && !strings.Contains(strings.Join(report.Details, " "), detail) {
		t.Fatalf("details=%v want %q", report.Details, detail)
	}
}

func assertCodexTOMLKeyImmediatelyAfterHeader(t *testing.T, content []byte, header, keyLine string) {
	t.Helper()
	lines := splitTOMLLines(content)
	for i, raw := range lines {
		if strings.TrimSpace(lineBody(raw)) == header {
			if i+1 >= len(lines) || strings.TrimSpace(lineBody(lines[i+1])) != keyLine {
				got := ""
				if i+1 < len(lines) {
					got = lines[i+1]
				}
				t.Fatalf("line after %s = %q, want %q", header, got, keyLine)
			}
			return
		}
	}
	t.Fatalf("header %s not found in:\n%s", header, content)
}

// T1: apply against a fixture with guardian_approval=true surgically flips
// only the owned-key lines; every comment and unrelated line stays byte
// identical.
func TestCodexTOMLApplySurgicallyOwnsDeclaredKeys(t *testing.T) {
	homePath := t.TempDir()
	targetPath := filepath.Join(t.TempDir(), "config.toml")
	before := []byte(codexTOMLFixtureBody("true", true, true))
	if err := os.WriteFile(targetPath, before, 0o644); err != nil {
		t.Fatal(err)
	}
	writePermissionFixture(t, homePath, targetPath, codexTOMLAuthorityFixture(codexTOMLOwnedSettings()))

	applied, err := applyGovernedPermissions(t, homePath, "codex")
	if err != nil {
		t.Fatalf("apply codex TOML settings: %v", err)
	}
	if !applied.Changed {
		t.Fatal("applied.Changed = false, want true")
	}
	after, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}

	beforeLines, afterLines := splitTOMLLines(before), splitTOMLLines(after)
	if len(beforeLines) != len(afterLines) {
		t.Fatalf("line count changed: before=%d after=%d\nbefore:\n%s\nafter:\n%s", len(beforeLines), len(afterLines), before, after)
	}
	var changed []int
	for i := range beforeLines {
		if beforeLines[i] != afterLines[i] {
			changed = append(changed, i)
		}
	}
	if len(changed) != 1 {
		t.Fatalf("changed line indices = %v, want exactly 1 (guardian_approval); before:\n%s\nafter:\n%s", changed, before, after)
	}
	if strings.TrimSpace(lineBody(afterLines[changed[0]])) != "guardian_approval = false" {
		t.Fatalf("changed line = %q, want guardian_approval = false", afterLines[changed[0]])
	}
	for _, comment := range []string{
		"# Incident note (synthetic)",
		"# authority first.",
	} {
		if !strings.Contains(string(after), comment) {
			t.Fatalf("comment %q missing after apply:\n%s", comment, after)
		}
	}
}

// T2: drift names the live/intended mismatch on features.guardian_approval
// and reports clean after apply.
func TestCodexTOMLDriftDetectsAndClearsGuardianApproval(t *testing.T) {
	homePath := t.TempDir()
	targetPath := filepath.Join(t.TempDir(), "config.toml")
	before := []byte(codexTOMLFixtureBody("true", true, true))
	if err := os.WriteFile(targetPath, before, 0o644); err != nil {
		t.Fatal(err)
	}
	writePermissionFixture(t, homePath, targetPath, codexTOMLAuthorityFixture(codexTOMLOwnedSettings()))

	assertCodexTOMLDrift(t, homePath, true, "features.guardian_approval")

	if _, err := applyGovernedPermissions(t, homePath, "codex"); err != nil {
		t.Fatalf("apply codex TOML settings: %v", err)
	}
	assertCodexTOMLDrift(t, homePath, false, "")
}

// T3: unrelated-key mutation (model value, comment text, [projects] trust)
// never registers as drift.
func TestCodexTOMLDriftIgnoresUnrelatedKeyChanges(t *testing.T) {
	homePath := t.TempDir()
	targetPath := filepath.Join(t.TempDir(), "config.toml")
	before := codexTOMLFixtureBody("false", true, true) // already matches intended settings
	if err := os.WriteFile(targetPath, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	writePermissionFixture(t, homePath, targetPath, codexTOMLAuthorityFixture(codexTOMLOwnedSettings()))
	assertCodexTOMLDrift(t, homePath, false, "")

	mutated := strings.Replace(before, `model = "gpt-5-codex"`, `model = "gpt-6-codex"`, 1)
	if mutated == before {
		t.Fatal("model mutation did not apply")
	}
	if err := os.WriteFile(targetPath, []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}
	assertCodexTOMLDrift(t, homePath, false, "")

	mutated2 := strings.Replace(mutated, "Incident note (synthetic)", "Incident note (edited)", 1)
	if mutated2 == mutated {
		t.Fatal("comment mutation did not apply")
	}
	if err := os.WriteFile(targetPath, []byte(mutated2), 0o644); err != nil {
		t.Fatal(err)
	}
	assertCodexTOMLDrift(t, homePath, false, "")

	mutated3 := strings.Replace(mutated2, `trust_level = "trusted"`, `trust_level = "untrusted"`, 1)
	if mutated3 == mutated2 {
		t.Fatal("trust mutation did not apply")
	}
	if err := os.WriteFile(targetPath, []byte(mutated3), 0o644); err != nil {
		t.Fatal(err)
	}
	assertCodexTOMLDrift(t, homePath, false, "")
}

// T4: a duplicate top-level assignment for an owned key is ambiguous; apply
// and drift both refuse explicitly, naming the key, and never touch the file.
func TestCodexTOMLAmbiguousDuplicateKeyFailsClosed(t *testing.T) {
	homePath := t.TempDir()
	targetPath := filepath.Join(t.TempDir(), "config.toml")
	base := codexTOMLFixtureBody("true", true, true)
	duplicated := strings.Replace(base, "approvals_reviewer = \"user\"\n", "approvals_reviewer = \"user\"\napproval_policy = \"never\"\n", 1)
	if duplicated == base {
		t.Fatal("duplicate injection did not apply")
	}
	if err := os.WriteFile(targetPath, []byte(duplicated), 0o644); err != nil {
		t.Fatal(err)
	}
	writePermissionFixture(t, homePath, targetPath, codexTOMLAuthorityFixture(codexTOMLOwnedSettings()))

	if _, err := RenderPermissions(homePath, "codex"); err == nil ||
		!strings.Contains(err.Error(), "approval_policy") || !strings.Contains(strings.ToLower(err.Error()), "ambiguous") {
		t.Fatalf("RenderPermissions duplicate key error = %v, want explicit ambiguity naming approval_policy", err)
	}
	if _, err := PermissionDrift(homePath, "codex"); err == nil ||
		!strings.Contains(err.Error(), "approval_policy") || !strings.Contains(strings.ToLower(err.Error()), "ambiguous") {
		t.Fatalf("PermissionDrift duplicate key error = %v, want explicit ambiguity naming approval_policy", err)
	}
	after, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != duplicated {
		t.Fatal("ambiguous target file was mutated")
	}
}

// T5: insertion lands the owned key in the right scope — right after an
// existing [features] header, or as a freshly appended table — leaving
// every other original line intact.
func TestCodexTOMLInsertsMissingOwnedKeyAndTable(t *testing.T) {
	t.Run("missing key in existing features block", func(t *testing.T) {
		homePath := t.TempDir()
		targetPath := filepath.Join(t.TempDir(), "config.toml")
		before := codexTOMLFixtureBody("true", false /* no guardian key */, true)
		if err := os.WriteFile(targetPath, []byte(before), 0o644); err != nil {
			t.Fatal(err)
		}
		writePermissionFixture(t, homePath, targetPath, codexTOMLAuthorityFixture(codexTOMLOwnedSettings()))

		if _, err := applyGovernedPermissions(t, homePath, "codex"); err != nil {
			t.Fatalf("apply: %v", err)
		}
		after, err := os.ReadFile(targetPath)
		if err != nil {
			t.Fatal(err)
		}
		assertCodexTOMLKeyImmediatelyAfterHeader(t, after, "[features]", "guardian_approval = false")
		for _, line := range splitTOMLLines([]byte(before)) {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if !strings.Contains(string(after), line) {
				t.Fatalf("original line missing after insertion: %q\nafter:\n%s", line, after)
			}
		}
	})

	t.Run("missing whole features block", func(t *testing.T) {
		homePath := t.TempDir()
		targetPath := filepath.Join(t.TempDir(), "config.toml")
		before := codexTOMLFixtureBody("true", false, false /* no [features] at all */)
		if err := os.WriteFile(targetPath, []byte(before), 0o644); err != nil {
			t.Fatal(err)
		}
		writePermissionFixture(t, homePath, targetPath, codexTOMLAuthorityFixture(codexTOMLOwnedSettings()))

		if _, err := applyGovernedPermissions(t, homePath, "codex"); err != nil {
			t.Fatalf("apply: %v", err)
		}
		after, err := os.ReadFile(targetPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(after), "[features]\nguardian_approval = false\n") {
			t.Fatalf("expected appended [features] block with guardian_approval, got:\n%s", after)
		}
		for _, line := range splitTOMLLines([]byte(before)) {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if !strings.Contains(string(after), line) {
				t.Fatalf("original line missing after insertion: %q\nafter:\n%s", line, after)
			}
		}
	})
}

// T6: an old-shape codex authority (no settings map) keeps today's exact
// non-JSON guard refusal — regression pin for the enrollment contract.
func TestCodexTOMLOldShapeAuthorityKeepsJSONGuard(t *testing.T) {
	homePath := t.TempDir()
	targetPath := filepath.Join(t.TempDir(), "config.toml")
	before := []byte(codexTOMLFixtureBody("true", true, true))
	if err := os.WriteFile(targetPath, before, 0o644); err != nil {
		t.Fatal(err)
	}
	policy := PermissionAuthority{
		SchemaVersion: PermissionAuthoritySchemaVersion, Harness: "codex", AuthorityRef: PermissionAuthorityRef("codex"),
		TargetPath:  targetPath,
		Permissions: PermissionRules{Allow: []string{}, Deny: []string{}, Ask: []string{}, DefaultMode: ""},
		AutoMode:    AutoModePolicy{Allow: []string{"$defaults"}},
	}
	writePermissionFixture(t, homePath, targetPath, policy)

	if _, err := RenderPermissions(homePath, "codex"); err == nil ||
		!strings.Contains(err.Error(), targetPath) || !strings.Contains(strings.ToLower(err.Error()), "not valid json") {
		t.Fatalf("RenderPermissions old-shape codex authority error = %v, want explicit non-JSON guard", err)
	}
	if _, err := PermissionDrift(homePath, "codex"); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "not valid json") {
		t.Fatalf("PermissionDrift old-shape codex authority error = %v, want explicit non-JSON guard", err)
	}
	after, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqual(after, before) {
		t.Fatal("old-shape codex authority guard mutated target")
	}
}

// T7: full lifecycle — apply, drift-clean, and idempotent re-apply.
func TestCodexTOMLFullLifecycleAppliesCleansDriftAndIsIdempotent(t *testing.T) {
	homePath := t.TempDir()
	targetPath := filepath.Join(t.TempDir(), "config.toml")
	before := []byte(codexTOMLFixtureBody("true", true, true))
	if err := os.WriteFile(targetPath, before, 0o644); err != nil {
		t.Fatal(err)
	}
	writePermissionFixture(t, homePath, targetPath, codexTOMLAuthorityFixture(codexTOMLOwnedSettings()))

	first, err := applyGovernedPermissions(t, homePath, "codex")
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if !first.Changed {
		t.Fatal("first apply changed = false, want true")
	}
	assertCodexTOMLDrift(t, homePath, false, "")

	second, err := applyGovernedPermissions(t, homePath, "codex")
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if second.Changed {
		t.Fatal("second apply changed = true, want idempotent false")
	}
	assertCodexTOMLDrift(t, homePath, false, "")
}

// T8: an owned string value carrying control characters — the newline-injection
// class — renders as ONE single-line TOML basic string with every control byte
// escaped. Red if the raw bytes pass through: the file gains lines, the value
// injects what parses as fresh keys/tables, and the target TOML turns invalid
// (an unterminated string), breaking the live harness config this renderer owns.
func TestCodexTOMLControlCharacterValueRendersEscaped(t *testing.T) {
	homePath := t.TempDir()
	targetPath := filepath.Join(t.TempDir(), "config.toml")
	before := []byte(codexTOMLFixtureBody("false", true, true))
	if err := os.WriteFile(targetPath, before, 0o644); err != nil {
		t.Fatal(err)
	}
	settings := codexTOMLOwnedSettings()
	settings["sandbox_mode"] = "danger-full-access\nnetwork_access = false\n[injected]\x01pwn = true"
	writePermissionFixture(t, homePath, targetPath, codexTOMLAuthorityFixture(settings))

	applied, err := applyGovernedPermissions(t, homePath, "codex")
	if err != nil {
		t.Fatalf("apply codex TOML settings: %v", err)
	}
	if !applied.Changed {
		t.Fatal("applied.Changed = false, want true")
	}
	after, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}

	beforeLines, afterLines := splitTOMLLines(before), splitTOMLLines(after)
	if len(beforeLines) != len(afterLines) {
		t.Fatalf("control characters split the file: before=%d lines after=%d\nafter:\n%s", len(beforeLines), len(afterLines), after)
	}
	want := `sandbox_mode = "danger-full-access\nnetwork_access = false\n[injected]\u0001pwn = true"`
	var changed []string
	for i := range beforeLines {
		if beforeLines[i] != afterLines[i] {
			changed = append(changed, lineBody(afterLines[i]))
		}
	}
	if len(changed) != 1 || changed[0] != want {
		t.Fatalf("changed lines = %q, want exactly one escaped single-line assignment %q", changed, want)
	}
	for _, raw := range afterLines {
		if strings.TrimSpace(lineBody(raw)) == "[injected]" {
			t.Fatalf("value injected a live table header:\n%s", after)
		}
	}
	assertCodexTOMLDrift(t, homePath, false, "")
}

// T8b: unit pin for the escaper — every TOML-named control escape plus the
// \uXXXX fallback for the rest of the control range (and DEL, which TOML
// forbids raw inside a basic string).
func TestTOMLEscapeBasicStringEscapesControlBytes(t *testing.T) {
	got := tomlEscapeBasicString("a\\b\"c\nd\re\tf\bg\fh\x7fi\x01j")
	want := `a\\b\"c\nd\re\tf\bg\fh\u007Fi\u0001j`
	if got != want {
		t.Fatalf("tomlEscapeBasicString = %q, want %q", got, want)
	}
}

// A trailing inline comment is operator-owned context even when the assignment
// itself is owned. Correcting the value preserves that context, and a value
// that is already correct remains a byte-identical no-op.
func TestCodexTOMLInlineCommentSurvivesCorrectionAndNoOp(t *testing.T) {
	for _, tc := range []struct {
		name        string
		before      string
		want        string
		wantChanged bool
	}{
		{
			name:        "correction preserves comment",
			before:      "approval_policy = \"ask\"  # operator context\nunrelated = true\n",
			want:        "approval_policy = \"never\"  # operator context\nunrelated = true\n",
			wantChanged: true,
		},
		{
			name:        "correct value is byte-identical no-op",
			before:      "approval_policy=\"never\"\t# operator context\nunrelated = true\n",
			want:        "approval_policy=\"never\"\t# operator context\nunrelated = true\n",
			wantChanged: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			targetPath := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(targetPath, []byte(tc.before), 0o644); err != nil {
				t.Fatal(err)
			}
			settings := map[string]any{"approval_policy": "never"}
			writePermissionFixture(t, homePath, targetPath, codexTOMLAuthorityFixture(settings))

			applied, err := applyGovernedPermissions(t, homePath, "codex")
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if applied.Changed != tc.wantChanged {
				t.Fatalf("applied.Changed = %v, want %v", applied.Changed, tc.wantChanged)
			}
			after, err := os.ReadFile(targetPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != tc.want {
				t.Fatalf("after apply:\n%s\nwant:\n%s", after, tc.want)
			}
		})
	}
}

// An array-of-tables header starts a new scope. A missing key in an earlier
// regular table must be inserted there, never matched inside the later array.
func TestCodexTOMLArrayOfTablesHeaderKeepsOwnedTableScope(t *testing.T) {
	homePath := t.TempDir()
	targetPath := filepath.Join(t.TempDir(), "config.toml")
	before := "[features]\n" +
		"web_search_request = false\n" +
		"\n" +
		"[[profiles]]\n" +
		"name = \"synthetic\"\n" +
		"guardian_approval = true\n"
	if err := os.WriteFile(targetPath, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{"features.guardian_approval": false}
	writePermissionFixture(t, homePath, targetPath, codexTOMLAuthorityFixture(settings))

	if _, err := applyGovernedPermissions(t, homePath, "codex"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	after, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "[features]\n" +
		"guardian_approval = false\n" +
		"web_search_request = false\n" +
		"\n" +
		"[[profiles]]\n" +
		"name = \"synthetic\"\n" +
		"guardian_approval = true\n"
	if string(after) != want {
		t.Fatalf("array-of-tables scope was not preserved; after:\n%s\nwant:\n%s", after, want)
	}
}

// Alternate TOML spellings of an owned path are semantically the same key but
// are outside this bare-key line editor's rewrite grammar. Detect and refuse
// them rather than inserting a duplicate bare assignment.
func TestCodexTOMLQuotedOrDottedOwnedKeyFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		before   string
		settings map[string]any
		key      string
	}{
		{
			name:     "quoted key",
			before:   "\"approval_policy\" = \"ask\"\nunrelated = true\n",
			settings: map[string]any{"approval_policy": "never"},
			key:      "approval_policy",
		},
		{
			name:     "dotted key",
			before:   "features.guardian_approval = true\nunrelated = true\n",
			settings: map[string]any{"features.guardian_approval": false},
			key:      "features.guardian_approval",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			targetPath := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(targetPath, []byte(tc.before), 0o644); err != nil {
				t.Fatal(err)
			}
			writePermissionFixture(t, homePath, targetPath, codexTOMLAuthorityFixture(tc.settings))

			_, renderErr := RenderPermissions(homePath, "codex")
			if renderErr == nil || !strings.Contains(renderErr.Error(), tc.key) || !strings.Contains(strings.ToLower(renderErr.Error()), "refus") {
				t.Errorf("render error = %v, want refusal naming %s", renderErr, tc.key)
			}
			after, err := os.ReadFile(targetPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != tc.before {
				t.Fatalf("refused alternate key spelling mutated target; after:\n%s\nwant:\n%s", after, tc.before)
			}
		})
	}
}

// A UTF-8 BOM belongs at byte zero, but is not part of the first key's name.
// The editor must recognize and update that key rather than duplicate it.
func TestCodexTOMLBOMPrefixedFirstOwnedKeyIsRecognized(t *testing.T) {
	homePath := t.TempDir()
	targetPath := filepath.Join(t.TempDir(), "config.toml")
	before := append([]byte{0xef, 0xbb, 0xbf}, []byte("approval_policy = \"ask\"\nunrelated = true\n")...)
	if err := os.WriteFile(targetPath, before, 0o644); err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{"approval_policy": "never"}
	writePermissionFixture(t, homePath, targetPath, codexTOMLAuthorityFixture(settings))

	applied, err := applyGovernedPermissions(t, homePath, "codex")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !applied.Changed {
		t.Fatal("applied.Changed = false, want true")
	}
	after, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte{0xef, 0xbb, 0xbf}, []byte("approval_policy = \"never\"\nunrelated = true\n")...)
	if !bytesEqual(after, want) {
		t.Fatalf("BOM-prefixed target after apply = %q, want %q", after, want)
	}
}
