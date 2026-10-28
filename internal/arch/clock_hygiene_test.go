// Repo-wide clock-hygiene guard. It lives in internal/arch because that package already
// owns the repo's structural guards (classification totality, package-table single source,
// status hot-path cap); a test-hygiene walk over cmd/, internal/, and fixtures/ is the same
// family, and giving it its own package would mint a new organ under the two-class law for
// no gain.
//
// This file exists because of a real, reproducible failure. On 2026-08-01 a
// certifying test (cmd/compound/synthesis_test.go) seeded a candidate with a
// hardcoded ExpiresAt of "2026-08-01T00:00:00Z". The store ran on the test's
// OWN fixed clock (June 2026), but the code under test read time.Now(). At
// 2026-08-01T00:00:00Z the candidate expired, the test began failing for every
// caller, and the failure took down the repo-wide certifying battery for
// changes that had nothing to do with synthesis. The defect class:
//
//	a *_test.go file that pins a wall-clock boundary to an absolute literal
//	while the production code reads time.Now(). The literal looks innocent
//	until the wall clock passes it, and then every consumer goes red.
//
// The guard below flags any absolute RFC3339 literal assigned to an
// expiry-ish field whose parsed value is already in the past relative to
// time.Now(). Future literals are intentionally NOT flagged — they have not
// yet armed the trap, and a guard that cries wolf gets deleted.
//
// Files outside the test walk (synthesis_test.go) are listed in excludedFiles
// with a reason. Add a new entry there ONLY when the literal is being fixed
// elsewhere; do NOT use the exclusion list as a graveyard.
package arch

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// excludedFiles lists test files the guard MUST skip, with a one-line reason.
// Keep the list short — every entry is debt. The exclusion is local to this
// guard; production hygiene is still owed on the excluded surface.
var excludedFiles = map[string]string{}

// expiryFieldPattern matches the literal assignment patterns this guard
// watches for. Three assignment shapes are covered:
//
//   - Go struct-literal / YAML:  ExpiresAt: "…"   expires_at: "…"
//   - Go assignment:             c.ExpiresAt = "…"
//   - JSON (compact or spaced):  "expires_at":"…"  "expires_at": "…"
//
// The JSON shape needs the optional closing quote before the separator —
// the key's own quote would otherwise defeat the colon match. The separator
// class [:=] is safe against Go comparisons: "!=" and "==" never satisfy
// field + optional quote + whitespace + separator + quote (the "!" and the
// second "=" block it), so an assertion like `saved.ExpiresAt != "…"` is
// not matched — only the assignment form is.
//
// It is deliberately anchored so that strings inside comments or unrelated
// JSON keys ("CreatedAt", "GeneratedAt", "ObservedAt", "ImportedAt", etc.)
// are NOT matched.
//
// Field names covered:
//   - ExpiresAt, expiresAt  (Go)
//   - expires_at           (YAML / snake_case JSON / struct tag)
//   - NotAfter             (Go, common in token/auth code)
//   - ValidUntil, validUntil, valid_until (Go + YAML)
//
// Add a name here only when an actual production field uses it.
var expiryFieldPattern = regexp.MustCompile(`(?:ExpiresAt|expiresAt|expires_at|NotAfter|ValidUntil|validUntil|valid_until)"?\s*[:=]\s*"([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z)"`)

// clockGuardRepoRoot resolves the repository root from this test file's own path so
// the guard works regardless of the working directory `go test` is invoked
// from. this file lives at <repoRoot>/internal/arch/clock_hygiene_test.go.
func clockGuardRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot resolve repo root")
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate go.mod above %s", thisFile)
	return ""
}

// scanFile reads path line-by-line and returns every expiry-ish literal
// whose parsed time is strictly before now. Each violation carries the
// 1-indexed line number so the failure message points straight at the
// offending source.
func scanFile(t *testing.T, now time.Time, path, relPath string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", relPath, err)
	}
	defer f.Close()
	var violations []string
	scanner := bufio.NewScanner(f)
	// Lines in *_test.go can be long (struct literals / JSON blobs). Bump
	// the buffer to comfortably handle a 1 MiB line.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		// Strip // line comments so the regex never matches prose. Block
		// comments are not stripped — they are rare on assignment lines,
		// and a literal written as `/* … ExpiresAt: "…" */` is unusual
		// enough that any false positive here is acceptable collateral.
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		matches := expiryFieldPattern.FindAllStringSubmatch(line, -1)
		for _, m := range matches {
			literal := m[1]
			parsed, err := time.Parse(time.RFC3339, literal)
			if err != nil {
				// Should be impossible given the regex; surface loudly if it
				// ever happens so the pattern can be tightened.
				t.Fatalf("%s:%d: regex matched %q but RFC3339 parse failed: %v", relPath, lineNo, literal, err)
			}
			if parsed.Before(now) {
				violations = append(violations, fmt.Sprintf("%s:%d  %s  (parsed %s, before now %s)",
					relPath, lineNo, literal, parsed.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339)))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", relPath, err)
	}
	return violations
}

// TestNoPastDatedExpiryLiteralsInTests is the main deliverable. It walks
// every *_test.go file under cmd/ and internal/ plus every machine-read data
// file under fixtures/ (a fixture that pins an absolute expiry arms the same
// trap from the data side: the test loads the fixture, the code reads
// time.Now()), scans for absolute RFC3339 literals assigned to an expiry-ish
// field, and fails if any literal has already passed relative to time.Now().
// Future literals are allowed; the goal is to catch the trap once it arms,
// not to police intent.
func TestNoPastDatedExpiryLiteralsInTests(t *testing.T) {
	root := clockGuardRepoRoot(t)
	now := time.Now().UTC()

	var all []string
	for _, walk := range []struct {
		sub  string
		keep func(name string) bool
	}{
		{"cmd", isGoTestFile},
		{"internal", isGoTestFile},
		{"fixtures", isFixtureDataFile},
	} {
		base := filepath.Join(root, walk.sub)
		err := filepath.Walk(base, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				return nil
			}
			if !walk.keep(info.Name()) {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if reason, skip := excludedFiles[rel]; skip {
				t.Logf("skip %s: %s", rel, reason)
				return nil
			}
			all = append(all, scanFile(t, now, path, rel)...)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}

	if len(all) > 0 {
		t.Fatalf(
			"found %d past-dated expiry literal(s) in *_test.go under cmd/ or internal/ or fixture data under fixtures/.\n"+
				"A literal of this shape arms a wall-clock trap: the test will fail the moment time.Now() passes it, "+
				"and every caller of the certifying battery goes red for unrelated reasons. Replace the literal with a horizon "+
				"computed relative to time.Now() (e.g. time.Now().UTC().Add(365*24*time.Hour).Format(time.RFC3339)) so the "+
				"trap can never re-arm itself on a later night.\n"+
				"Offending sites:\n  %s\n",
			len(all), strings.Join(all, "\n  "),
		)
	}
}

// isGoTestFile keeps the cmd/ and internal/ walks on the test files whose
// literals compile into the certifying battery.
func isGoTestFile(name string) bool {
	return strings.HasSuffix(name, "_test.go")
}

// isFixtureDataFile keeps the fixtures/ walk on machine-read data files — the
// ones a test decodes and hands to production code that reads time.Now().
// Prose (.md, .txt) is out of scope: it arms no clock.
func isFixtureDataFile(name string) bool {
	switch filepath.Ext(name) {
	case ".json", ".ndjson", ".jsonl", ".yaml", ".yml":
		return true
	}
	return false
}
