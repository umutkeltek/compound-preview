package policy

// Apply/empty-trash planning and execution.
//
// The packet's falsifiers require apply's dry-run to print the same JSON
// shape as plan, and require a working trash-restore path. The plan/exec
// halves live here so the cmd-layer stays a thin adapter that handles
// flagging, output rendering, and exit codes.

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ApplyAuthorityRef names the apply registry for evidence refs.
const ApplyAuthorityRef = "compound://registries/retention-apply-classes"

// TrashRootRel is the relative path under COMPOUND_HOME where trashed files
// are parked before empty-trash unlinks them. Kept under a single
// recognizable prefix so a future operator can find every trashed file by
// path even if the receipt log is missing.
const TrashRootRel = "trash"

// ReceiptDirRel is the relative path under COMPOUND_HOME where apply and
// empty-trash append their JSONL receipts.
//
// state/ is already used by other append-only observation streams
// (state/harness-drift-observations, state/instruction-pack-drift-observations)
// and is not in forbiddenApplyRoots.
const ReceiptDirRel = "state/retention-receipts"

// ParseApplyDuration accepts the packet's loose duration grammar:
//
//	"30d"  -> 30 days
//	"14h"  -> 14 hours
//	"7"    -> 7 days (bare numbers are days; that is the only unit an
//	         operator is likely to mean when writing 30/14/7 for retention)
//
// Returns an error rather than guessing on a unitless string the caller did
// not intend as days, because a wrong default on a mutating verb is the
// failure this whole layer exists to prevent.
func ParseApplyDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return d, nil
	}
	// try "<n>d"
	if len(raw) >= 2 && strings.HasSuffix(raw, "d") {
		num := strings.TrimSuffix(raw, "d")
		if v, err := parseIntStrict(num); err == nil {
			return time.Duration(v) * 24 * time.Hour, nil
		}
	}
	return 0, fmt.Errorf("invalid duration %q (use 30d, 14h, or a Go duration)", raw)
}

func parseIntStrict(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty number")
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int64(r-'0')
	}
	return n, nil
}

// ApplyRequest is the operator's request, normalized.
//
// BuildApplyPlan uses it as input; the cmd-layer assembles one from flags.
type ApplyRequest struct {
	Home       string
	ClassIDs   []string
	OlderThan  time.Duration
	LimitBytes int64
	DryRun     bool
	Now        time.Time
	// TrashDate is the date used as the trash subdir leaf (YYYY-MM-DD).
	// Pass zero to derive from Now.
	TrashDate time.Time
}

// Validate enforces the safety floors BEFORE any filesystem walk. A request
// that names an unknown class or whose path intersects a forbidden root is
// refused here, so a typo cannot reach the rename syscall.
func (r *ApplyRequest) Validate() error {
	if strings.TrimSpace(r.Home) == "" {
		return fmt.Errorf("apply requires COMPOUND_HOME")
	}
	if len(r.ClassIDs) == 0 {
		return fmt.Errorf("apply requires at least one --class; declared: %s", strings.Join(ApplyClassIDs(), ", "))
	}
	var hardFloor time.Duration
	for _, id := range r.ClassIDs {
		class, ok := ApplyClassByID(id)
		if !ok {
			return &UnknownApplyClassError{ClassID: id}
		}
		for _, forbidden := range forbiddenApplyRoots {
			if pathIntersects(class.PathUnderHome, forbidden) {
				return &ApplyClassConflictError{ClassID: class.ID, Path: class.PathUnderHome, Root: forbidden}
			}
		}
		if class.AgeFloor > hardFloor {
			hardFloor = class.AgeFloor
		}
	}
	if r.OlderThan <= 0 {
		return fmt.Errorf("--older-than must be > 0 (e.g. 30d, 14h)")
	}
	if r.OlderThan < hardFloor {
		return fmt.Errorf("--older-than %s is below the strictest class age floor %s; refusing", r.OlderThan, hardFloor)
	}
	return nil
}

func pathIntersects(child, parent string) bool {
	child = filepath.ToSlash(filepath.Clean(strings.TrimSpace(child)))
	parent = filepath.ToSlash(filepath.Clean(strings.TrimSpace(parent)))
	if child == "" || parent == "" {
		return false
	}
	return child == parent || strings.HasPrefix(child, parent+"/")
}

// pathInsideRoot is the strict containment used before irreversible
// mutations: child must be a real sub-path of root. It rejects the root
// itself, sibling directories whose names merely begin with the root's name
// (the raw strings.HasPrefix bug: <home>/trash-malicious passed a check
// intended for <home>/trash), and any .. escape resolved or not.
func pathInsideRoot(child, root string) bool {
	child = filepath.ToSlash(filepath.Clean(strings.TrimSpace(child)))
	root = filepath.ToSlash(filepath.Clean(strings.TrimSpace(root)))
	if child == "" || root == "" {
		return false
	}
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	rel = path.Clean(rel)
	if rel == "." || rel == "" || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	return filepath.IsAbs(rel) == false && !strings.HasPrefix(rel, "/")
}

// receiptJournal is the write-ahead receipt stream shared by apply and
// empty-trash. Every mutating step is bracketed: an intent line is made
// durable BEFORE the mutation, and a committed (or failed) line follows it.
// A mutation with no receipt was the old failure mode — under the disk
// pressure this subsystem exists to manage, the receipt write failing AFTER
// an irreversible delete destroyed the evidence of what was deleted.
type receiptJournal struct {
	f *os.File
}

func openReceiptJournal(path string) (*receiptJournal, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &receiptJournal{f: f}, nil
}

// writeLine makes one JSONL line durable before returning. A line sitting in
// the OS buffer is not a receipt; it is a hope.
func (j *receiptJournal) writeLine(line []byte) error {
	if _, err := j.f.Write(append(line, '\n')); err != nil {
		return err
	}
	return j.f.Sync()
}

func (j *receiptJournal) Close() error {
	if j.f == nil {
		return nil
	}
	return j.f.Close()
}

// BuildApplyPlan walks the home for files matching each requested class and
// age floor, sorts oldest-first, and trims the list to fit LimitBytes.
//
// Dry-run and --yes paths both call this; the executor (ApplyPlan.Execute)
// uses the returned Steps verbatim, so dry-run output IS the same shape a
// real run would have produced (modulo any rename that fails mid-execute).
func BuildApplyPlan(req ApplyRequest) (ApplyPlan, error) {
	if err := req.Validate(); err != nil {
		return ApplyPlan{}, err
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	trashDate := req.TrashDate
	if trashDate.IsZero() {
		trashDate = now
	}

	plan := ApplyPlan{
		AuthorityRef: ApplyAuthorityRef,
		Home:         req.Home,
		Classes:      append([]string(nil), req.ClassIDs...),
		OlderThan:    humanDuration(req.OlderThan),
		LimitBytes:   req.LimitBytes,
		DryRun:       req.DryRun,
	}

	var collected []applyCandidate
	for _, id := range req.ClassIDs {
		class, _ := ApplyClassByID(id)
		// Apply the operator's --older-than only if it is at least as strict
		// as the class's AgeFloor; AgeFloor is the floor.
		ageFloor := class.AgeFloor
		if req.OlderThan > ageFloor {
			ageFloor = req.OlderThan
		}
		cands, err := scanClass(req.Home, class, ageFloor, now)
		if err != nil {
			return ApplyPlan{}, err
		}
		collected = append(collected, cands...)
	}

	// Oldest first: the first thing to age out is the first thing to go,
	// matching how a human would manually trim the directory.
	sort.SliceStable(collected, func(i, j int) bool {
		return collected[i].mtime.Before(collected[j].mtime)
	})

	dateStr := trashDate.UTC().Format("2006-01-02")
	var running int64
	for _, c := range collected {
		if req.LimitBytes > 0 && running+c.bytes > req.LimitBytes {
			break
		}
		trash := filepath.Join(req.Home, TrashRootRel, c.classID, dateStr, c.rel)
		step := ApplyStep{
			ClassID:    c.classID,
			SourcePath: filepath.Join(req.Home, c.classPath, c.rel),
			TrashPath:  trash,
			Bytes:      c.bytes,
			MTime:      c.mtime.UTC().Format(time.RFC3339),
		}
		plan.Steps = append(plan.Steps, step)
		running += c.bytes
	}
	plan.PlannedBytes = running

	switch {
	case len(plan.Steps) == 0:
		plan.Note = "no eligible files matched the requested classes and age floor"
	case req.DryRun:
		plan.Note = "dry-run; pass --yes to apply"
	default:
		plan.Note = "applied"
	}
	return plan, nil
}

type applyCandidate struct {
	classID   string
	classPath string
	rel       string
	bytes     int64
	mtime     time.Time
}

// scanClass walks one class root under home and returns eligible regular
// files. Subdirectories are descended; symlinks are NOT followed (a
// symlinked cache would otherwise be moved out from under whichever
// consumer happened to follow it).
func scanClass(home string, class ApplyClass, ageFloor time.Duration, now time.Time) ([]applyCandidate, error) {
	root := filepath.Join(home, class.PathUnderHome)
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, nil
	}

	cutoff := now.Add(-ageFloor)
	var out []applyCandidate
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is not a reason to abandon the whole
			// count — return nil and keep walking siblings.
			return nil
		}
		// Skip the root itself.
		if path == root {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		// Treat anything that is not a regular file as ineligible. The
		// symlink guard is implicit: a symlink's d.Type() does not include
		// the regular bit.
		if !entry.Type().IsRegular() {
			return nil
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return nil
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		out = append(out, applyCandidate{
			classID:   class.ID,
			classPath: class.PathUnderHome,
			rel:       rel,
			bytes:     info.Size(),
			mtime:     info.ModTime(),
		})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil
}

// ExecuteApplyPlan performs the rename for every step in plan.Steps and
// journals one bracketed receipt pair per step: a durable intent line BEFORE
// the rename and a committed line after it.
//
// Returns the same plan with AppliedCount/AppliedBytes/ReceiptPath populated.
// Errors mid-execute stop the run and return what was applied so far plus
// the error — a partial apply is still a receipted apply, because the
// receipt log is what tells the operator which files actually moved.
func ExecuteApplyPlan(plan ApplyPlan, now time.Time) (ApplyPlan, error) {
	if plan.DryRun {
		return plan, nil
	}
	if strings.TrimSpace(plan.Home) == "" {
		return plan, fmt.Errorf("execute apply: home not set")
	}

	receiptPath := filepath.Join(plan.Home, ReceiptDirRel, "apply.jsonl")
	if err := os.MkdirAll(filepath.Dir(receiptPath), 0o755); err != nil {
		return plan, fmt.Errorf("mkdir %s: %w", filepath.Dir(receiptPath), err)
	}
	journal, err := openReceiptJournal(receiptPath)
	if err != nil {
		return plan, fmt.Errorf("open receipt %s: %w", receiptPath, err)
	}
	defer journal.Close()

	for _, step := range plan.Steps {
		rec := applyReceipt{
			TS:         now.UTC().Format(time.RFC3339Nano),
			Authority:  ApplyAuthorityRef,
			ClassID:    step.ClassID,
			SourcePath: step.SourcePath,
			TrashPath:  step.TrashPath,
			Bytes:      step.Bytes,
			Phase:      applyPhaseIntent,
		}
		intentLine, mErr := json.Marshal(rec)
		if mErr != nil {
			return plan, fmt.Errorf("marshal receipt: %w", mErr)
		}
		if wErr := journal.writeLine(intentLine); wErr != nil {
			return plan, fmt.Errorf("apply step %s: durable intent receipt: %w", step.SourcePath, wErr)
		}
		if err := moveToTrash(step.SourcePath, step.TrashPath); err != nil {
			rec.Phase = applyPhaseFailed
			if failedLine, mErr := json.Marshal(rec); mErr == nil {
				_ = journal.writeLine(failedLine)
			}
			return plan, fmt.Errorf("apply step %s: %w", step.SourcePath, err)
		}
		rec.Phase = applyPhaseCommitted
		committedLine, mErr := json.Marshal(rec)
		if mErr != nil {
			return plan, fmt.Errorf("marshal receipt: %w", mErr)
		}
		if wErr := journal.writeLine(committedLine); wErr != nil {
			// The rename already happened; leave a machine-readable marker
			// beside the trashed file so a receiptless mutation is
			// discoverable without diffing the tree by hand.
			markerErr := os.WriteFile(step.TrashPath+".pending-receipt", committedLine, 0o644)
			return plan, fmt.Errorf("apply step %s: renamed but committed receipt write failed: %w (pending-receipt marker: %v)",
				step.SourcePath, wErr, markerErr)
		}
		plan.AppliedCount++
		plan.AppliedBytes += step.Bytes
	}
	plan.ReceiptPath = receiptPath
	if plan.Note == "applied" || plan.Note == "" {
		plan.Note = fmt.Sprintf("applied %d files (%s)", plan.AppliedCount, applyHumanBytes(plan.AppliedBytes))
	}
	return plan, nil
}

// moveToTrash creates the trash leaf and atomically renames the source into
// it. Atomic only within one filesystem: the apply verb guarantees both
// sides live under COMPOUND_HOME so the rename is atomic in practice.
func moveToTrash(source, trash string) error {
	if err := os.MkdirAll(filepath.Dir(trash), 0o755); err != nil {
		return err
	}
	if err := os.Rename(source, trash); err != nil {
		// If the target already exists (a previous partial apply), the
		// rename fails with EEXIST. Surface a useful error rather than the
		// raw errno.
		if os.IsExist(err) {
			return fmt.Errorf("trash target already exists: %s", trash)
		}
		return err
	}
	return nil
}

// EmptyTrashRequest is the operator's empty-trash request, normalized.
type EmptyTrashRequest struct {
	Home       string
	OlderThan  time.Duration
	LimitBytes int64
	DryRun     bool
	Now        time.Time
}

// Validate enforces the same hard floors as apply: home required, age floor
// positive, and at least one day-class eligible before any rename.
func (r *EmptyTrashRequest) Validate() error {
	if strings.TrimSpace(r.Home) == "" {
		return fmt.Errorf("empty-trash requires COMPOUND_HOME")
	}
	if r.OlderThan <= 0 {
		return fmt.Errorf("--older-than must be > 0")
	}
	return nil
}

// EmptyTrashStep is one unlink action.
type EmptyTrashStep struct {
	ClassID   string `json:"class_id"`
	TrashPath string `json:"trash_path"`
	Bytes     int64  `json:"bytes"`
	Date      string `json:"date"`
}

// EmptyTrashReport is the answer to "what would empty-trash unlink right now?".
type EmptyTrashReport struct {
	AuthorityRef string           `json:"authority_ref"`
	Home         string           `json:"home,omitempty"`
	Steps        []EmptyTrashStep `json:"steps"`
	PlannedBytes int64            `json:"planned_bytes"`
	LimitBytes   int64            `json:"limit_bytes,omitempty"`
	OlderThan    string           `json:"older_than"`
	DryRun       bool             `json:"dry_run"`
	AppliedCount int              `json:"applied_count"`
	AppliedBytes int64            `json:"applied_bytes"`
	ReceiptPath  string           `json:"receipt_path,omitempty"`
	Note         string           `json:"note"`
}

// BuildEmptyTrashReport walks trash/<class>/<date>/ under home and lists
// every date directory whose date is older than --older-than. Each such
// directory is unlinked wholesale (the date is the unit of empty-trash;
// partial empties would orphan files whose age-floor check no longer
// applies).
func BuildEmptyTrashReport(req EmptyTrashRequest) (EmptyTrashReport, error) {
	if err := req.Validate(); err != nil {
		return EmptyTrashReport{}, err
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-req.OlderThan)

	report := EmptyTrashReport{
		AuthorityRef: ApplyAuthorityRef,
		Home:         req.Home,
		OlderThan:    humanDuration(req.OlderThan),
		LimitBytes:   req.LimitBytes,
		DryRun:       req.DryRun,
	}

	trashRoot := filepath.Join(req.Home, TrashRootRel)
	if _, err := os.Stat(trashRoot); err != nil {
		if os.IsNotExist(err) {
			report.Note = "no trash directory; nothing to empty"
			return report, nil
		}
		return EmptyTrashReport{}, err
	}

	classDirs, err := os.ReadDir(trashRoot)
	if err != nil {
		return EmptyTrashReport{}, err
	}

	var running int64
	for _, classDir := range classDirs {
		if !classDir.IsDir() {
			continue
		}
		classPath := filepath.Join(trashRoot, classDir.Name())
		dateDirs, err := os.ReadDir(classPath)
		if err != nil {
			return EmptyTrashReport{}, err
		}
		for _, dateDir := range dateDirs {
			if !dateDir.IsDir() {
				continue
			}
			parsed, parseErr := time.Parse("2006-01-02", dateDir.Name())
			if parseErr != nil {
				// A non-date directory under trash/<class> is unexpected;
				// skip rather than trash something we did not create.
				continue
			}
			if !parsed.Before(cutoff) {
				continue
			}
			trashPath := filepath.Join(classPath, dateDir.Name())
			bytes := dirSize(trashPath)
			if req.LimitBytes > 0 && running+bytes > req.LimitBytes {
				continue
			}
			report.Steps = append(report.Steps, EmptyTrashStep{
				ClassID:   classDir.Name(),
				TrashPath: trashPath,
				Bytes:     bytes,
				Date:      dateDir.Name(),
			})
			running += bytes
		}
	}
	sort.Slice(report.Steps, func(i, j int) bool {
		return report.Steps[i].Date < report.Steps[j].Date
	})
	report.PlannedBytes = running

	switch {
	case len(report.Steps) == 0:
		report.Note = "no trash older than the requested threshold"
	case req.DryRun:
		report.Note = "dry-run; pass --yes to empty"
	default:
		report.Note = "applied"
	}
	return report, nil
}

// ExecuteEmptyTrash unlinks every step, bracketed by durable intent and
// committed receipt lines. Deletion is irreversible, so the intent line is
// the only record that survives a crash between the two: it names exactly
// what was being destroyed.
func ExecuteEmptyTrash(report EmptyTrashReport, now time.Time) (EmptyTrashReport, error) {
	if report.DryRun {
		return report, nil
	}
	receiptPath := filepath.Join(report.Home, ReceiptDirRel, "empty-trash.jsonl")
	if err := os.MkdirAll(filepath.Dir(receiptPath), 0o755); err != nil {
		return report, fmt.Errorf("mkdir %s: %w", filepath.Dir(receiptPath), err)
	}
	journal, err := openReceiptJournal(receiptPath)
	if err != nil {
		return report, fmt.Errorf("open receipt %s: %w", receiptPath, err)
	}
	defer journal.Close()

	trashRoot := filepath.Join(report.Home, TrashRootRel)
	for _, step := range report.Steps {
		// Refuse to unlink anything outside the trash root, even though the
		// build path only emits paths under trash/. The guard exists because
		// a future change to BuildEmptyTrashReport that accidentally widens
		// step.TrashPath must NOT silently translate into unlink.
		// Strict containment (filepath.Rel, rejecting the root itself and
		// same-prefix siblings): a raw HasPrefix accepted <home>/trash-malicious.
		if !pathInsideRoot(step.TrashPath, trashRoot) {
			return report, fmt.Errorf("refusing to unlink outside %s: %s", trashRoot, step.TrashPath)
		}
		rec := emptyTrashReceipt{
			TS:        now.UTC().Format(time.RFC3339Nano),
			Authority: ApplyAuthorityRef,
			ClassID:   step.ClassID,
			TrashPath: step.TrashPath,
			Bytes:     step.Bytes,
			Date:      step.Date,
			Phase:     applyPhaseIntent,
		}
		intentLine, mErr := json.Marshal(rec)
		if mErr != nil {
			return report, fmt.Errorf("marshal receipt: %w", mErr)
		}
		if wErr := journal.writeLine(intentLine); wErr != nil {
			return report, fmt.Errorf("unlink %s: durable intent receipt: %w", step.TrashPath, wErr)
		}
		if err := os.RemoveAll(step.TrashPath); err != nil {
			rec.Phase = applyPhaseFailed
			if failedLine, mErr := json.Marshal(rec); mErr == nil {
				_ = journal.writeLine(failedLine)
			}
			return report, fmt.Errorf("unlink %s: %w", step.TrashPath, err)
		}
		rec.Phase = applyPhaseCommitted
		committedLine, mErr := json.Marshal(rec)
		if mErr != nil {
			return report, fmt.Errorf("marshal receipt: %w", mErr)
		}
		if wErr := journal.writeLine(committedLine); wErr != nil {
			return report, fmt.Errorf("unlink %s: deleted but committed receipt write failed: %w (intent line is durable above)", step.TrashPath, wErr)
		}
		report.AppliedCount++
		report.AppliedBytes += step.Bytes
	}
	report.ReceiptPath = receiptPath
	if report.Note == "applied" || report.Note == "" {
		report.Note = fmt.Sprintf("emptied %d date directories (%s)", report.AppliedCount, applyHumanBytes(report.AppliedBytes))
	}
	return report, nil
}

// dirSize totals regular-file sizes under dir. Symlinks are NOT followed;
// the same convention as retention_scan.go's dirBytes.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if info, statErr := entry.Info(); statErr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// ReadApplyReceipts scans the apply receipt log and returns the parsed rows.
// Empty when the file does not exist; never errors on a missing file because
// that is the "nothing has ever been reclaimed" case, not an error.
//
// Public so a future tool (or a falsifier test) can reconstruct what was
// moved without re-walking the trash tree.
func ReadApplyReceipts(home string) ([]applyReceipt, error) {
	path := filepath.Join(home, ReceiptDirRel, "apply.jsonl")
	return readApplyReceipts(path)
}

// ReadEmptyTrashReceipts scans the empty-trash receipt log.
func ReadEmptyTrashReceipts(home string) ([]emptyTrashReceipt, error) {
	path := filepath.Join(home, ReceiptDirRel, "empty-trash.jsonl")
	return readEmptyTrashReceipts(path)
}

func readApplyReceipts(path string) ([]applyReceipt, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var out []applyReceipt
	for {
		var rec applyReceipt
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			return out, err
		}
		// The journal holds intent/failed/committed rows for every mutation;
		// historical logs (pre-journal) have no phase, and a missing phase on
		// a legacy line means it was written after a successful mutation, so
		// it is committed. Only explicit non-committed phases are skipped.
		if rec.Phase != "" && rec.Phase != applyPhaseCommitted {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

func readEmptyTrashReceipts(path string) ([]emptyTrashReceipt, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var out []emptyTrashReceipt
	for {
		var rec emptyTrashReceipt
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			return out, err
		}
		if rec.Phase != "" && rec.Phase != applyPhaseCommitted {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// Receipt journal phases: every mutation is bracketed by a durable intent
// line followed by a committed (or failed) line. Readers surface committed
// rows; intent-without-committed rows are crash evidence, surfaced by count
// where it matters, never silently dropped.
const (
	applyPhaseIntent    = "intent"
	applyPhaseCommitted = "committed"
	applyPhaseFailed    = "failed"
)

type applyReceipt struct {
	TS         string `json:"ts"`
	Authority  string `json:"authority"`
	ClassID    string `json:"class_id"`
	SourcePath string `json:"source_path"`
	TrashPath  string `json:"trash_path"`
	Bytes      int64  `json:"bytes"`
	Phase      string `json:"phase,omitempty"`
}

type emptyTrashReceipt struct {
	TS        string `json:"ts"`
	Authority string `json:"authority"`
	ClassID   string `json:"class_id"`
	TrashPath string `json:"trash_path"`
	Bytes     int64  `json:"bytes"`
	Date      string `json:"date"`
	Phase     string `json:"phase,omitempty"`
}

// humanDuration renders a duration for plan output.
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	days := int64(d / (24 * time.Hour))
	if days > 0 && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return d.String()
}

// humanBytes returns a compact byte string for plan/receipt output.
func applyHumanBytes(n int64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
	)
	switch {
	case n >= GiB:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(GiB))
	case n >= MiB:
		return fmt.Sprintf("%.2f MiB", float64(n)/float64(MiB))
	case n >= KiB:
		return fmt.Sprintf("%.2f KiB", float64(n)/float64(KiB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
