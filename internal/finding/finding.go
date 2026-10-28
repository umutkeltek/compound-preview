// Package finding is the direct-write home for an agent-authored operational finding — a durable,
// immediately-recallable note (a runbook fact, an operational observation) that is NOT a promotable
// truth claim and does NOT need adjudication latency. It is the POSITIVE half of the placement
// grammar: the governed place an agent writes a finding instead of inventing a path under
// COMPOUND_HOME (the itch the closed-world guard denies).
//
// Findings live under COMPOUND_HOME/observed/findings — a rebuildable, provenance-tagged,
// content-addressed directory OUTSIDE COMPOUND_HOME/ledger, mirroring observed/tool-output. Recording
// a finding therefore never adds a ledger byte (LedgerDigest stays byte-identical by construction),
// and a finding is source_class=evidence: recallable, but it can never enter ambient recall as a
// promoted Claim. A finding that later proves durable is promoted through the SEPARATE governed
// memory door — two doors, by design (capture is cheap; promotion is adjudicated).
package finding

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	linkFindingFile          = os.Link
	removeStagedFindingFile  = os.Remove
	syncFindingDirectoryFunc = syncDir
)

// Subdir is the direct-write, content-addressed home for operational findings, relative to
// COMPOUND_HOME. Exported so the placement resolver can compose it as the operational-finding target
// without re-authoring the path (a read facade over the real owner).
const (
	Subdir                = "observed/findings"
	PrivacyPrivate        = "private"
	VisibilityOperational = "operational"
	MaxBodyBytes          = 1 << 20
)

// Finding is one agent-authored operational note. It is append-only with supersession (a revised
// finding carries Supersedes = the prior id), mirroring the ledger's discipline without being in
// the ledger.
type Finding struct {
	ID           string   `json:"id"`                    // "find_" + digest12(topic|body) — idempotent on content
	CID          string   `json:"cid"`                   // "sha256:"+hex(body) — content address of the body
	Topic        string   `json:"topic"`                 // recall key (subject / entity)
	Body         string   `json:"body"`                  // the note (markdown)
	ProducedBy   string   `json:"produced_by,omitempty"` // harness / agent that authored it
	WorkID       string   `json:"work_id,omitempty"`     // provenance: the work_id it was learned under
	SourceClass  string   `json:"source_class"`          // always "evidence" — never ambient truth
	Supersedes   string   `json:"supersedes,omitempty"`  // prior finding id this revises
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
	Privacy      string   `json:"privacy"`
	Visibility   string   `json:"visibility"`
	CreatedAt    string   `json:"created_at"`
}

// CID content-addresses a finding body: "sha256:"+hex(sha256(body)).
func CID(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digest12(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

// Record writes a finding to observed/findings. It is idempotent on (topic, body): the same content
// yields the same id AND preserves the first record unchanged (retry is a no-op write, not an
// overwrite). It writes ONLY under observed/findings — NEVER a ledger byte. New callers that
// need to distinguish a fresh write from a duplicate replay should use RecordWithStatus.
func Record(homePath string, f Finding, now time.Time) (Finding, error) {
	res, err := RecordWithStatus(homePath, f, now)
	if err != nil {
		return Finding{}, err
	}
	return res.Finding, nil
}

// Status values returned by RecordWithStatus. They tell the caller whether the call actually
// published the record or whether an existing, content-identical record was preserved.
const (
	StatusRecorded  = "recorded"
	StatusDuplicate = "duplicate"
)

// RecordResult is the duplicate-aware return: Status tells the caller whether the publication
// wrote a new file or whether the existing durable record was preserved unchanged. Finding is
// always the durable record (the freshly-validated one on `recorded`, or the existing one on
// `duplicate`); it is never the loser's proposed provenance. A duplicate directory-sync failure
// returns this kept result alongside the error so callers can fail closed without losing identity.
type RecordResult struct {
	Status  string
	Finding Finding
}

// RecordWithStatus writes a finding to observed/findings using the staged-file + hard-link
// pattern AND validates any existing deterministic target against the owned invariants. It is
// idempotent on (topic, body): a replay with the same content always returns the same id and
// CID, and never overwrites the first record's provenance, work_id, evidence_refs, created_at,
// or supersedes. A deliberately revised body must change Topic or Body (and is therefore a
// different record) or be persisted with a new content identity; there is no in-place edit.
//
// Exposed surface:
//   - Status="recorded"  — the staged file was published as the deterministic target.
//   - Status="duplicate" — the deterministic target already existed and was validated against
//     the owned invariants; the existing record (not the loser's provenance) is returned.
//
// Validation invariants for any pre-existing target:
//   - filename / record ID equals the deterministic id for the proposed (topic, body);
//   - record topic and body equal the proposed content;
//   - record CID equals CID(record.Body);
//   - record ID equals digest12(record.Topic + "|" + record.Body);
//   - record source_class is exactly "evidence";
//   - record created_at is present and parseable as RFC3339.
//
// A target that violates any of these is a hard error. The file is left byte-identical; it is
// never overwritten, quarantined, or repaired silently.
//
// Concurrent writers of the same (topic, body) always converge on one complete valid file: one
// returns Status="recorded", every other successful return is Status="duplicate" describing the
// first record.
func RecordWithStatus(homePath string, f Finding, now time.Time) (RecordResult, error) {
	if strings.TrimSpace(homePath) == "" {
		return RecordResult{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(f.Topic) == "" {
		return RecordResult{}, fmt.Errorf("finding topic required")
	}
	if strings.TrimSpace(f.Body) == "" {
		return RecordResult{}, fmt.Errorf("finding body required")
	}
	if len(f.Body) > MaxBodyBytes {
		return RecordResult{}, fmt.Errorf("finding body exceeds the 1 MiB limit")
	}
	f.ID = "find_" + digest12(f.Topic+"|"+f.Body)
	f.CID = CID(f.Body)
	f.SourceClass = "evidence" // findings are evidence, never promoted truth
	applyClassificationDefaults(&f)
	if strings.TrimSpace(f.CreatedAt) == "" {
		f.CreatedAt = now.UTC().Format(time.RFC3339)
	}
	f.EvidenceRefs = dedupe(f.EvidenceRefs)
	dir := filepath.Join(homePath, Subdir)
	if err := ensurePrivateDirectory(dir); err != nil {
		return RecordResult{}, err
	}
	// If the deterministic target already exists, validate it against the owned invariants and
	// return the existing record as a duplicate. We never overwrite an existing record; the
	// staged-file + hard-link path below is the only publication channel.
	target := filepath.Join(dir, f.ID+".json")
	if existing, err := os.ReadFile(target); err == nil {
		var kept Finding
		if uerr := json.Unmarshal(existing, &kept); uerr != nil {
			return RecordResult{}, fmt.Errorf("finding %s existing record is malformed: %w", f.ID, uerr)
		}
		applyClassificationDefaults(&kept)
		if err := validateExistingFinding(kept, f.Topic, f.Body); err != nil {
			return RecordResult{}, fmt.Errorf("finding %s existing record invariants violated: %w", f.ID, err)
		}
		return duplicateAfterDirectorySync(dir, kept)
	} else if !os.IsNotExist(err) {
		return RecordResult{}, err
	}
	data, err := json.MarshalIndent(&f, "", "  ")
	if err != nil {
		return RecordResult{}, err
	}
	// Staged-file + hard-link publication: write to a unique temp file, fsync, then link it
	// into the deterministic target. If the link races and another writer creates the target
	// first, we re-validate and report duplicate (the existing record wins by construction).
	tmp, err := os.CreateTemp(dir, ".finding-*.json.tmp")
	if err != nil {
		return RecordResult{}, err
	}
	tmpPath := tmp.Name()
	keepTemp := true
	defer func() {
		if keepTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return RecordResult{}, err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return RecordResult{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return RecordResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return RecordResult{}, err
	}
	if err := linkFindingFile(tmpPath, target); err != nil {
		// The link failed: best-effort inline cleanup of the staged file. If that also fails
		// (and the file still exists), the deferred last-resort cleanup runs and we report
		// the publish error so the caller does NOT observe a silent `recorded`.
		removeErr := removeStagedFindingFile(tmpPath)
		if removeErr == nil || os.IsNotExist(removeErr) {
			keepTemp = false
		}
		if removeErr != nil && !os.IsNotExist(removeErr) {
			return RecordResult{}, errors.Join(fmt.Errorf("publish finding: %w", err), fmt.Errorf("remove staged finding: %w", removeErr))
		}
		if os.IsExist(err) {
			// A concurrent writer won the race. Re-validate and report duplicate.
			existing, rerr := os.ReadFile(target)
			if rerr != nil {
				return RecordResult{}, rerr
			}
			var kept Finding
			if uerr := json.Unmarshal(existing, &kept); uerr != nil {
				return RecordResult{}, fmt.Errorf("finding %s existing record is malformed: %w", f.ID, uerr)
			}
			applyClassificationDefaults(&kept)
			if verr := validateExistingFinding(kept, f.Topic, f.Body); verr != nil {
				return RecordResult{}, fmt.Errorf("finding %s existing record invariants violated: %w", f.ID, verr)
			}
			return duplicateAfterDirectorySync(dir, kept)
		}
		return RecordResult{}, fmt.Errorf("publish finding: %w", err)
	}
	// Publication succeeded: the deterministic target is now linked and remains authoritative
	// even if staging cleanup fails. Check that cleanup, then sync the directory after both name
	// changes so the target link and staged-name removal share one durability boundary. A cleanup
	// failure still reaches the caller, but never skips the sync that makes the published target
	// durable; the result retains StatusRecorded because the hard link already succeeded.
	result := RecordResult{Status: StatusRecorded, Finding: f}
	var cleanupErr error
	if err := removeStagedFindingFile(tmpPath); err != nil {
		cleanupErr = fmt.Errorf("remove staged finding: %w", err)
	} else {
		keepTemp = false
	}
	var syncErr error
	if err := syncFindingDirectoryFunc(dir); err != nil {
		syncErr = fmt.Errorf("sync finding directory: %w", err)
	}
	return result, errors.Join(cleanupErr, syncErr)
}

// syncDir dir.Sync so a successful Put means the name-to-inode link is durable across a
// crash, not merely visible to this process.
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("tighten finding directory permissions: %w", err)
	}
	return nil
}

func applyClassificationDefaults(f *Finding) {
	if strings.TrimSpace(f.Privacy) == "" {
		f.Privacy = PrivacyPrivate
	}
	if strings.TrimSpace(f.Visibility) == "" {
		f.Visibility = VisibilityOperational
	}
}

func duplicateAfterDirectorySync(dir string, kept Finding) (RecordResult, error) {
	result := RecordResult{Status: StatusDuplicate, Finding: kept}
	if err := syncFindingDirectoryFunc(dir); err != nil {
		return result, fmt.Errorf("sync finding directory for duplicate: %w", err)
	}
	return result, nil
}

// validateExistingFinding enforces the owned invariants an existing deterministic target must
// satisfy to be considered a duplicate. A failing target is a hard error — never repaired,
// quarantined, or silently accepted.
func validateExistingFinding(kept Finding, wantTopic, wantBody string) error {
	if strings.TrimSpace(kept.ID) == "" {
		return fmt.Errorf("existing record missing id")
	}
	wantID := "find_" + digest12(wantTopic+"|"+wantBody)
	if kept.ID != wantID {
		return fmt.Errorf("existing record id %q does not match deterministic id %q", kept.ID, wantID)
	}
	if kept.Topic != wantTopic {
		return fmt.Errorf("existing record topic %q does not match proposed %q", kept.Topic, wantTopic)
	}
	if kept.Body != wantBody {
		return fmt.Errorf("existing record body does not match proposed body")
	}
	if kept.CID != CID(wantBody) {
		return fmt.Errorf("existing record cid %q does not match CID(proposed body)", kept.CID)
	}
	wantDigest := digest12(kept.Topic + "|" + kept.Body)
	if digestID(kept.ID) != wantDigest {
		return fmt.Errorf("existing record id %q is not digest12(topic|body)", kept.ID)
	}
	if strings.TrimSpace(kept.SourceClass) != "evidence" {
		return fmt.Errorf("existing record source_class %q is not evidence", kept.SourceClass)
	}
	if strings.TrimSpace(kept.CreatedAt) == "" {
		return fmt.Errorf("existing record missing created_at")
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(kept.CreatedAt)); err != nil {
		return fmt.Errorf("existing record created_at %q is not RFC3339", kept.CreatedAt)
	}
	return nil
}

// digestID returns the 12-hex digest portion of a find_<digest> id, or empty if the id is
// not a well-formed finding identifier.
func digestID(id string) string {
	const prefix = "find_"
	if !strings.HasPrefix(id, prefix) {
		return ""
	}
	return strings.TrimPrefix(id, prefix)
}

// Show returns a finding by id. A missing id is an explicit error (never a silent zero value).
func Show(homePath, id string) (Finding, error) {
	if err := validID(id); err != nil {
		return Finding{}, err
	}
	b, err := os.ReadFile(filepath.Join(homePath, Subdir, id+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return Finding{}, fmt.Errorf("finding %s not found", id)
		}
		return Finding{}, err
	}
	var f Finding
	if err := json.Unmarshal(b, &f); err != nil {
		return Finding{}, err
	}
	applyClassificationDefaults(&f)
	return f, nil
}

// List returns findings, optionally filtered by a topic/body substring (case-insensitive), newest
// first. A missing findings dir is the empty no-op, not an error.
func List(homePath, filter string) ([]Finding, error) {
	dir := filepath.Join(homePath, Subdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	needle := strings.ToLower(strings.TrimSpace(filter))
	var out []Finding
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var f Finding
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, err
		}
		applyClassificationDefaults(&f)
		if needle != "" && !strings.Contains(strings.ToLower(f.Topic), needle) && !strings.Contains(strings.ToLower(f.Body), needle) {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt // newest first
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// FindingDigest is one finding in a lifecycle report — enough to identify and act on it without the
// full body.
type FindingDigest struct {
	ID           string `json:"id"`
	Topic        string `json:"topic"`
	CreatedAt    string `json:"created_at"`
	SupersededBy string `json:"superseded_by,omitempty"`
	AgeDays      int    `json:"age_days,omitempty"`
}

// FindingReport is Doctor's read-only lifecycle view: how many findings exist, how many are live,
// which are superseded (their id is named by a later finding's Supersedes), and which live findings
// are stale (older than the age threshold). It NEVER promotes, archives, or deletes — visibility only,
// so the direct-write home stays accountable without a second authority.
type FindingReport struct {
	Total      int             `json:"total"`
	Live       int             `json:"live"`
	StaleDays  int             `json:"stale_days"`
	Superseded []FindingDigest `json:"superseded"`
	Stale      []FindingDigest `json:"stale"`
}

// Doctor reports finding-home lifecycle hygiene: superseded records (an id named by a later finding's
// Supersedes) and stale LIVE records (CreatedAt older than staleDays; staleDays<=0 disables the age
// axis). It reads only — it never promotes, archives, or deletes. It deliberately does NOT report
// "unreferenced": findings are referenced BY nothing inward by design (their EvidenceRefs point
// outward), so an unreferenced axis would be fabricated noise rather than a real signal.
func Doctor(homePath string, now time.Time, staleDays int) (FindingReport, error) {
	all, err := List(homePath, "")
	if err != nil {
		return FindingReport{}, err
	}
	supersededBy := map[string]string{} // superseded id -> the finding id that revises it
	for _, f := range all {
		if s := strings.TrimSpace(f.Supersedes); s != "" {
			supersededBy[s] = f.ID
		}
	}
	rep := FindingReport{Total: len(all), StaleDays: staleDays}
	for _, f := range all {
		if by, ok := supersededBy[f.ID]; ok {
			rep.Superseded = append(rep.Superseded, FindingDigest{ID: f.ID, Topic: f.Topic, CreatedAt: f.CreatedAt, SupersededBy: by})
			continue
		}
		rep.Live++
		if staleDays > 0 {
			if age, ok := ageDays(f.CreatedAt, now); ok && age >= staleDays {
				rep.Stale = append(rep.Stale, FindingDigest{ID: f.ID, Topic: f.Topic, CreatedAt: f.CreatedAt, AgeDays: age})
			}
		}
	}
	return rep, nil
}

func ageDays(createdAt string, now time.Time) (int, bool) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(createdAt))
	if err != nil {
		return 0, false
	}
	d := now.Sub(t)
	if d < 0 {
		return 0, true
	}
	return int(d.Hours() / 24), true
}

// validID keeps a crafted id from escaping the findings dir (the id keys a filename).
func validID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("finding id required")
	}
	if id == "." || id == ".." || id != filepath.Base(id) || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("finding id must be a bare identifier, got %q", id)
	}
	return nil
}

func dedupe(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
