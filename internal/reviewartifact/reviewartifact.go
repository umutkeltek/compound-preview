// Package reviewartifact is the one deep module behind every owner-review
// artifact compound writes under COMPOUND_HOME/reviews/<area>/.
//
// Across the tree, ~17 commands turn a candidate (or a doctor/snapshot finding)
// into a reviewed JSON+Markdown artifact that mutates no owner file and promotes
// no claim. Before this module each of those sites re-implemented the identical
// mechanism — target hashing, id derivation, slugging, ref de-duplication, and
// the MkdirAll+marshal+write pair — under renamed private helpers. That is the
// shallow-family shape: the interface (a Record* call) was nearly as wide as the
// implementation, and a change to the artifact format meant editing up to 17
// files.
//
// reviewartifact gives that mechanism a single home so each call site keeps only
// what genuinely varies: the area, the domain payload, the markdown body, and any
// extra draft files (.review.yaml, manifests) it writes alongside the core pair.
// It mirrors the already-deep internal/sourceobs + internal/writeback shape:
// many thin callers, one shared core.
//
// Scope discipline: this module owns only the artifact mechanism. It performs no
// gating, candidate adjudication, or promotion — those remain with internal/policy,
// internal/writeback, and the domain packages. It centralizes the core JSON/Markdown
// artifact writers and their shared id/slug/dedupe/hash helpers; a few callers keep
// specialized hashing that produces deliberately different output (e.g. product's
// sha256:-prefixed target hash), which stays with that caller by design.
package reviewartifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Paths is the core json+md pair a review artifact always writes. Callers that
// emit extra drafts (a .review.yaml, a manifest) carry their own wider Paths
// type, but still route those writes through WriteRaw or WriteJSON.
type Paths struct {
	PlanPath   string
	ReviewPath string
}

// validArea rejects an area that is empty, absolute, or escapes reviews/ via
// "..". A nested area such as "distiller-eval/proofs" is allowed.
func validArea(area string) error {
	if strings.TrimSpace(area) == "" {
		return fmt.Errorf("review area required")
	}
	if filepath.IsAbs(area) {
		return fmt.Errorf("review area must be relative, got %q", area)
	}
	for _, seg := range strings.Split(filepath.ToSlash(area), "/") {
		if seg == ".." {
			return fmt.Errorf("review area must not escape reviews/, got %q", area)
		}
	}
	return nil
}

// validName rejects an id/filename that is empty or path-shaped (contains a
// directory component, is "." / "..", or holds a path separator). Review ids and
// names are often derived from edited or generated YAML, so this keeps a crafted
// value from escaping reviews/<area>/.
func validName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("review artifact name required")
	}
	if name == "." || name == ".." || name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("review artifact name must be a bare filename, got %q", name)
	}
	return nil
}

// Dir resolves and creates COMPOUND_HOME/reviews/<area>/.
func Dir(homePath, area string) (string, error) {
	if strings.TrimSpace(homePath) == "" {
		return "", fmt.Errorf("COMPOUND_HOME required")
	}
	if err := validArea(area); err != nil {
		return "", err
	}
	dir := filepath.Join(homePath, "reviews", area)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// Write persists the json+md core pair of an owner-review artifact:
// reviews/<area>/<id>.json (payload marshaled with the given indent plus a
// trailing newline) and reviews/<area>/<id>.md (markdown verbatim). The indent
// is a caller parameter on purpose — different artifact families historically
// serialize with " " or "  ", and Write preserves each one exactly.
func Write(homePath, area, id string, payload any, indent, markdown string) (Paths, error) {
	if err := validName(id); err != nil {
		return Paths{}, err
	}
	data, err := json.MarshalIndent(payload, "", indent)
	if err != nil {
		return Paths{}, err
	}
	var payloadMap map[string]any
	if err := json.Unmarshal(data, &payloadMap); err != nil {
		return Paths{}, err
	}
	card, hasCard := OperatorCardFromMap(payloadMap)
	if !hasCard {
		provider, providesCard := payload.(operatorCardProvider)
		source := SummarySourceWriter
		if providesCard {
			card, err = provider.ReviewOperatorCard()
			if err != nil {
				return Paths{}, fmt.Errorf("review artifact %s/%s operator card: %w", area, id, err)
			}
		} else {
			// No authored card and no provider: derive the plain-language card
			// (what/why/one action) from the payload itself — never a generic
			// placeholder. "fallback" stays the source stamp so queue debt
			// counters still track cards the writer did not author.
			card = DerivedOperatorCard(payloadMap, area)
			source = SummarySourceFallback
		}
		data, err = injectOperatorCardJSON(data, card, indent, source)
		if err != nil {
			return Paths{}, err
		}
	}
	if err := ValidateOperatorCard(card); err != nil {
		return Paths{}, fmt.Errorf("review artifact %s/%s invalid operator card: %w", area, id, err)
	}
	dir, err := Dir(homePath, area)
	if err != nil {
		return Paths{}, err
	}
	paths := Paths{
		PlanPath:   filepath.Join(dir, id+".json"),
		ReviewPath: filepath.Join(dir, id+".md"),
	}
	if err := os.WriteFile(paths.PlanPath, append(data, '\n'), 0o644); err != nil {
		return paths, err
	}
	if err := os.WriteFile(paths.ReviewPath, []byte(markdown), 0o644); err != nil {
		return paths, err
	}
	return paths, nil
}

// WriteJSON persists a single reviews/<area>/<name> JSON artifact with no
// markdown sibling — for the review-record / apply-record / proof writers that
// emit one custom-suffixed file (e.g. "<id>.reviewed.json", "<id>.apply.json").
// name is the full filename; payload is MarshalIndent'd with indent plus a
// trailing newline. Returns the written path.
func WriteJSON(homePath, area, name string, payload any, indent string) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	dir, err := Dir(homePath, area)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	data, err := json.MarshalIndent(payload, "", indent)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// WriteRaw persists a single reviews/<area>/<name> file with verbatim bytes (no
// trailing newline added) — for non-JSON drafts such as a reviewed YAML manifest.
// Returns the written path.
func WriteRaw(homePath, area, name string, data []byte) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	dir, err := Dir(homePath, area)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// HashFileTarget reports whether a file owner target exists and its content
// hash. A missing or empty path is the no-op (false, "", nil); a directory is an
// error, so a review artifact that means to hash one file fails fast instead of
// recursively walking a mistaken repo root. This is the file-only behaviour the
// per-package hashers originally had.
func HashFileTarget(path string) (bool, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return false, "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "", nil
		}
		return false, "", err
	}
	sum := sha256.Sum256(data)
	return true, hex.EncodeToString(sum[:]), nil
}

// HashTreeTarget reports whether a target exists and its content hash, hashing a
// file by its bytes or a directory by the sorted set of relative-path:hash
// entries (stable regardless of walk order). Use it only where the target is
// legitimately a tree (e.g. an eval corpus); for a single file use
// HashFileTarget so a mistaken directory errors instead of triggering a walk.
func HashTreeTarget(path string) (bool, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return false, "", nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "", nil
		}
		return false, "", err
	}
	if !info.IsDir() {
		data, err := os.ReadFile(path)
		if err != nil {
			return false, "", err
		}
		sum := sha256.Sum256(data)
		return true, hex.EncodeToString(sum[:]), nil
	}
	var entries []string
	if err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(path, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		entries = append(entries, filepath.ToSlash(rel)+":"+hex.EncodeToString(sum[:]))
		return nil
	}); err != nil {
		return false, "", err
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return true, hex.EncodeToString(sum[:]), nil
}

// Digest12 is the stable short digest used in artifact ids: the first 12 hex
// chars of sha256(value).
func Digest12(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

// ID derives a deterministic artifact id: prefix + Digest12 of the parts joined
// by "|". Replaying the same inputs yields the same id.
func ID(prefix string, parts ...string) string {
	return prefix + Digest12(strings.Join(parts, "|"))
}

// Slugify lowercases value and collapses every run of non-[a-z0-9] into a single
// dash, trimming leading/trailing dashes. A maxLen <= 0 means no limit; otherwise
// the result is truncated to maxLen and re-trimmed so it never ends in a dash.
func Slugify(value string, maxLen int) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if maxLen > 0 && len(out) > maxLen {
		out = strings.Trim(out[:maxLen], "-")
	}
	return out
}

// Timestamp formats t as a UTC RFC3339 string — the created_at value on every
// artifact.
func Timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// UniqueSorted trims, de-duplicates, and sorts a list of refs. Use it where the
// artifact records refs in a canonical (order-independent) order.
func UniqueSorted(values []string) []string {
	out := dedupe(values)
	sort.Strings(out)
	return out
}

// Unique trims and de-duplicates while preserving first-seen order. Use it where
// the artifact's ref order is meaningful.
func Unique(values []string) []string {
	return dedupe(values)
}

func dedupe(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// FirstNonEmpty returns the first trimmed non-empty value, or "".
func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if v := strings.TrimSpace(value); v != "" {
			return v
		}
	}
	return ""
}
