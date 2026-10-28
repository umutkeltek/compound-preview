package mem

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	homepkg "compound/internal/home"
	routecodec "compound/internal/routeevidence/codec"
)

// EngineDoctorReport proves the rented breadth engine can serve recall without
// becoming a ledger owner. Engines may build private indexes; they may not mutate
// COMPOUND_HOME/ledger.
type EngineDoctorReport struct {
	Query               string             `json:"query"`
	Engine              EngineCapabilities `json:"engine"`
	LedgerGen           int64              `json:"ledger_gen"`
	LedgerGenAfter      int64              `json:"ledger_gen_after"`
	LedgerHashBefore    string             `json:"ledger_hash_before"`
	LedgerHashAfter     string             `json:"ledger_hash_after"`
	LedgerUnchanged     bool               `json:"ledger_unchanged"`
	Hits                []EngineDoctorHit  `json:"hits,omitempty"`
	Degraded            bool               `json:"degraded,omitempty"`
	Error               string             `json:"error,omitempty"`
	RetryCount          int                `json:"retry_count,omitempty"`
	OwnerLedgerAdvanced bool               `json:"owner_ledger_advanced,omitempty"`
	AdvancedRows        int                `json:"advanced_rows,omitempty"`     // governed rows OTHER compound processes appended during the probe
	Note                string             `json:"note,omitempty"`              // INFO surfaced when a benign concurrent-writer retry proved the ledger
	EngineViolations    []string           `json:"engine_violations,omitempty"` // ledger paths/rows attributable to the rented engine (hard invariant failure)
	pendingArtifacts    []string           // exact governed publication sibling relpaths present in the attempt's final snapshot
	finalOwnerLockEpoch string             // one canonical autonomy-admission lock acquisition spanning the final snapshot
}

type EngineDoctorHit struct {
	ClaimID   string `json:"claim_id"`
	Entity    string `json:"entity"`
	Predicate string `json:"predicate"`
	Object    string `json:"object"`
	Seq       int64  `json:"seq"`
}

// ErrLedgerBusy means ownership attribution succeeded for every observed delta,
// but the ledger advanced across both bounded attempts so current synchronization
// could not be certified. It is not an engine-mutation verdict.
var ErrLedgerBusy = errors.New("owned ledger remained active during engine doctor")

var errLedgerChangedDuringDoctor = errors.New("owned ledger changed during engine doctor")

// EngineDoctor runs one breadth query through the supplied rented engine and
// verifies either a byte-stable ledger or that every observed delta is a governed
// owner append. Missing or unavailable breadth degrades explicitly;
// identity/protocol errors and engine-attributed writes remain hard failures.
//
// The owned ledger is live: OTHER compound processes (a governed close, a writeback
// gate, a product signal) legitimately append rows while the doctor's query runs, so a
// whole-directory byte digest moving is NOT proof the RENTED engine mutated the ledger.
// When the digest moves, EngineDoctor attributes the delta: a legitimate concurrent write
// is append-only governed rows carrying producer provenance (plus a counter bump); an
// engine mutation is not (a private index file dropped in the ledger, a non-append
// rewrite, or a row without provenance). A purely-legit delta is retried ONCE against the
// fresh generation. A second legitimate delta returns ErrLedgerBusy: ownership was
// preserved, but current synchronization was not certified. An engine-attributed delta
// stays red and names the offending path.
func (s *Store) EngineDoctor(query string, k int, eng Engine) (EngineDoctorReport, error) {
	if strings.TrimSpace(query) == "" {
		return EngineDoctorReport{}, fmt.Errorf("query required")
	}
	if k <= 0 {
		k = 10
	}
	report, before, err := s.engineDoctorAttempt(query, k, eng)
	if err == nil || report.LedgerUnchanged {
		return report, err
	}
	if !errors.Is(err, errLedgerChangedDuringDoctor) {
		// Snapshot and query errors are not ownership evidence. Preserve the real
		// failure instead of classifying a zero/partial snapshot as engine mutation.
		return report, err
	}
	if report.Error != "" && !report.Degraded {
		// A hard engine query error, not a ledger-ownership question — surface it as-is.
		return report, err
	}
	// The owned ledger digest moved while the rented engine ran. Attribute the delta:
	// only an engine-attributed mutation keeps the invariant red.
	delta := s.classifyLedgerDeltaSince(before, report.finalOwnerLockEpoch)
	if !delta.legit() {
		report.EngineViolations = delta.violations
		return report, fmt.Errorf("rented engine mutated owned ledger: %s", strings.Join(delta.violations, "; "))
	}
	delta.pendingArtifacts = append(delta.pendingArtifacts, report.pendingArtifacts...)
	// Benign concurrent governed writers advanced the ledger under the doctor. Retry the
	// hop ONCE against the fresh generation to observe a quiet window.
	retry, retryBefore, retryErr := s.engineDoctorAttempt(query, k, eng)
	retry.RetryCount = 1
	retry.OwnerLedgerAdvanced = true
	retry.AdvancedRows = delta.rows
	if retryErr == nil && retry.LedgerUnchanged {
		if violations := s.unresolvedConcurrentLedgerArtifacts(delta.pendingArtifacts); len(violations) > 0 {
			retry.EngineViolations = violations
			return retry, fmt.Errorf("owned ledger publication did not resolve: %s", strings.Join(violations, "; "))
		}
		retry.Note = fmt.Sprintf("ledger advanced %d row(s) during doctor (concurrent legit writers); verified against fresh gen", delta.rows)
		return retry, nil
	}
	if retryErr != nil && !errors.Is(retryErr, errLedgerChangedDuringDoctor) {
		// Never turn a failed retry snapshot into a benign hot-ledger result. The
		// caller needs the original error to know the proof did not run.
		return retry, retryErr
	}
	if retry.LedgerUnchanged {
		// A genuine (non-ledger) error surfaced on the retry.
		return retry, retryErr
	}
	if retry.Error != "" && !retry.Degraded {
		return retry, retryErr
	}
	// The ledger moved again during the stable retry. An engine write is still red;
	// otherwise the ledger is simply too hot to prove a quiet window this run.
	retryDelta := s.classifyLedgerDeltaSince(retryBefore, retry.finalOwnerLockEpoch)
	if !retryDelta.legit() {
		retry.EngineViolations = retryDelta.violations
		return retry, fmt.Errorf("rented engine mutated owned ledger: %s", strings.Join(retryDelta.violations, "; "))
	}
	retryDelta.pendingArtifacts = append(retryDelta.pendingArtifacts, retry.pendingArtifacts...)
	if violations := s.unresolvedConcurrentLedgerArtifacts(append(delta.pendingArtifacts, retryDelta.pendingArtifacts...)); len(violations) > 0 {
		retry.EngineViolations = violations
		return retry, fmt.Errorf("owned ledger publication did not resolve: %s", strings.Join(violations, "; "))
	}
	retry.AdvancedRows = delta.rows + retryDelta.rows
	retry.Note = fmt.Sprintf("ledger remained active across both doctor attempts (%d concurrent legit row(s)); ownership preserved by governed append attribution; current synchronization not certified", retry.AdvancedRows)
	return retry, fmt.Errorf("%w: %d concurrent legit row(s); current synchronization not certified", ErrLedgerBusy, retry.AdvancedRows)
}

func (s *Store) engineDoctorAttempt(query string, k int, eng Engine) (EngineDoctorReport, ledgerDoctorSnapshot, error) {
	before, err := s.ledgerDoctorSnapshot()
	if err != nil {
		return EngineDoctorReport{}, ledgerDoctorSnapshot{}, err
	}
	report := EngineDoctorReport{
		Query:            query,
		LedgerGen:        before.gen,
		LedgerHashBefore: before.digest,
	}
	claims, qerr := s.QueryBreadth(query, k, eng)
	report.Engine = eng.Capabilities()
	for _, c := range claims {
		report.Hits = append(report.Hits, EngineDoctorHit{
			ClaimID:   c.ID,
			Entity:    c.Entity,
			Predicate: c.Predicate,
			Object:    c.Object,
			Seq:       c.Seq,
		})
	}
	after, derr := s.ledgerDoctorSnapshot()
	if derr != nil {
		return report, before, derr
	}
	report.LedgerGenAfter = after.gen
	report.LedgerHashAfter = after.digest
	report.LedgerUnchanged = before.digest == after.digest
	report.finalOwnerLockEpoch = after.ownerLockEpoch
	if qerr != nil {
		report.Error = qerr.Error()
		switch {
		case errors.Is(qerr, ErrUnsupported), errors.Is(qerr, ErrEngineUnavailable):
			report.Degraded = true
		case errors.Is(qerr, ErrEngineServedUnownedClaim):
			// A served-authority breach: the engine emitted an index_hit into a claim the owned
			// ledger does not hold. Hard fail (never degraded) — but the ledger itself is
			// untouched, so LedgerUnchanged stays TRUE and this populates EngineViolations.
			report.EngineViolations = append(report.EngineViolations, qerr.Error())
			return report, before, qerr
		default:
			return report, before, qerr
		}
	}
	report.pendingArtifacts = s.routeEvidenceSnapshotArtifactPaths(after)
	if report.LedgerUnchanged {
		if violations := s.unresolvedConcurrentLedgerArtifacts(report.pendingArtifacts); len(violations) > 0 {
			report.EngineViolations = violations
			return report, before, fmt.Errorf("owned ledger has unresolved route evidence publication: %s", strings.Join(violations, "; "))
		}
	}
	if !report.LedgerUnchanged {
		// EngineDoctor attributes this delta (concurrent legit writers vs. an engine
		// mutation). It is a hard failure only when the delta is engine-attributed.
		return report, before, errLedgerChangedDuringDoctor
	}
	return report, before, nil
}

// ledgerFileMeta fingerprints one ledger file so a later read can tell an append-only
// governed growth from a non-append rewrite without holding the whole ledger in memory.
type ledgerFileMeta struct {
	size int64
	hash string // sha256 hex of the file's bytes
	info os.FileInfo
}

type ledgerDoctorSnapshot struct {
	gen            int64
	digest         string
	files          map[string]ledgerFileMeta // relpath -> fingerprint, captured under the same shared lock as digest
	ownerLockEpoch string                    // one autonomy-admission lock acquisition held across this snapshot
}

// ledgerDelta is the attribution verdict for a ledger that moved during a probe.
type ledgerDelta struct {
	rows             int      // governed rows appended by legitimate producers
	violations       []string // engine-attributed changes (empty == purely concurrent-legit)
	pendingArtifacts []string // exact governed CAS siblings that must resolve during the bounded retry
}

func (d ledgerDelta) legit() bool { return len(d.violations) == 0 }

// classifyLedgerDeltaSince attributes every change to the owned ledger since `before`.
// A legitimate concurrent write is append-only governed rows (each carrying producer
// provenance) plus the monotonic seq counter; anything else — a new non-log file, a
// non-append rewrite, a removed governed file, or a row without provenance — is
// engine-attributed and named. It re-reads the live ledger, which is race-tolerant:
// legit appends stay legit (the before-prefix is unchanged) and an engine artifact
// persists.
func (s *Store) classifyLedgerDeltaSince(before ledgerDoctorSnapshot, currentOwnerLockEpoch string) ledgerDelta {
	root := filepath.Join(s.home, "ledger")
	var delta ledgerDelta
	ledgerRoot, rootInfo, err := openLedgerDoctorRoot(root)
	if err != nil {
		delta.violations = append(delta.violations, "cannot open rooted ledger delta: "+err.Error())
		return delta
	}
	defer ledgerRoot.Close()
	seen := make(map[string]bool, len(before.files))
	// Retain bodies only for the canonical route log and exact candidate CAS
	// siblings. Generic files are classified as they are read so a doctor over a
	// large ledger does not retain every owned body in memory.
	current := make(map[string][]byte)
	currentMeta := make(map[string]ledgerFileMeta, len(before.files))
	entries, err := enumerateLedgerDoctorFiles(ledgerRoot, rootInfo)
	if err != nil {
		delta.violations = append(delta.violations, "cannot inspect ledger delta: "+err.Error())
		return delta
	}
	for _, entry := range entries {
		rel, b, info := entry.rel, entry.body, entry.info
		seen[rel] = true
		currentMeta[rel] = ledgerFileMeta{size: int64(len(b)), hash: sha256Hex(b), info: info}
		_, _, exactSwap := parseRouteEvidenceSwapLeaf(rel)
		if rel == routeEvidenceLedgerFile || exactSwap {
			current[rel] = b
		}
		if !exactSwap {
			prev, existed := before.files[rel]
			if !existed || currentMeta[rel].hash != prev.hash {
				classifyChangedLedgerFile(rel, b, prev, existed, &delta)
			}
		}
	}
	if err := validateLedgerDoctorRoot(root, ledgerRoot, rootInfo); err != nil {
		delta.violations = append(delta.violations, "cannot inspect ledger delta: "+err.Error())
		return delta
	}
	legitSwap := make(map[string]bool)
	for rel := range current {
		if legitimateRouteEvidenceSwapTransition(rel, before, current, currentMeta, currentOwnerLockEpoch) {
			legitSwap[rel] = true
			delta.pendingArtifacts = append(delta.pendingArtifacts, rel)
		}
	}
	for rel := range before.files {
		if _, exists := current[rel]; exists {
			continue
		}
		if legitimateRouteEvidenceSwapTransition(rel, before, current, currentMeta, currentOwnerLockEpoch) {
			legitSwap[rel] = true
		}
	}
	for rel, b := range current {
		if rel == routeEvidenceLedgerFile {
			continue // classified during the walk; retained only as CAS-pair evidence
		}
		prev, existed := before.files[rel]
		if existed && currentMeta[rel].hash == prev.hash {
			continue // unchanged since the pre-query snapshot
		}
		if legitSwap[rel] {
			continue
		}
		classifyChangedLedgerFile(rel, b, prev, existed, &delta)
	}
	for rel := range before.files {
		if seen[rel] || filepath.Base(rel) == "seq.tmp" {
			continue // transient counter temp consumed by rename
		}
		if legitSwap[rel] {
			continue
		}
		delta.violations = append(delta.violations, "governed ledger file "+rel+" removed during probe (engine-attributed)")
	}
	return delta
}

const routeEvidenceLedgerFile = "execution-route-evidence.jsonl"

// legitimateRouteEvidenceSwapTransition recognizes only the atomic-publication
// sibling emitted by routeevidence.recordEvidenceLocked. The exact leaf grammar
// is necessary but insufficient: the name's pre/post digests must match the
// observed canonical/sibling pair, both entries must be regular files, and the
// postimage must be the preimage plus exactly one complete JSON object row.
func legitimateRouteEvidenceSwapTransition(rel string, before ledgerDoctorSnapshot, current map[string][]byte, currentMeta map[string]ledgerFileMeta, currentOwnerLockEpoch string) bool {
	prePrefix, postPrefix, ok := parseRouteEvidenceSwapLeaf(rel)
	if !ok {
		return false
	}
	beforeSwap, beforeExists := before.files[rel]
	currentSwap, currentExists := current[rel]
	beforeCanonical, beforeCanonicalExists := before.files[routeEvidenceLedgerFile]
	currentCanonical, currentCanonicalExists := current[routeEvidenceLedgerFile]
	currentSwapMeta := currentMeta[rel]
	currentCanonicalMeta := currentMeta[routeEvidenceLedgerFile]
	regular := func(meta ledgerFileMeta) bool { return meta.info != nil && meta.info.Mode().IsRegular() }
	pre := func(meta ledgerFileMeta) bool { return regular(meta) && strings.HasPrefix(meta.hash, prePrefix) }
	post := func(meta ledgerFileMeta) bool { return regular(meta) && strings.HasPrefix(meta.hash, postPrefix) }
	same := func(left, right ledgerFileMeta) bool {
		return regular(left) && regular(right) && os.SameFile(left.info, right.info) && left.hash == right.hash
	}
	emptyPre := ledgerFileMeta{hash: sha256Hex(nil)}
	if !strings.HasPrefix(emptyPre.hash, prePrefix) {
		emptyPre = ledgerFileMeta{}
	}
	if (beforeExists && before.ownerLockEpoch == "") || (!beforeExists && currentExists && currentOwnerLockEpoch == "") {
		return false
	}

	// Prepared sibling appeared but publication has not linearized yet. This is
	// tentative owner activity only: the caller records it as pending and refuses
	// every benign/busy return until a later snapshot proves it resolved.
	if !beforeExists && currentExists {
		switch {
		case beforeCanonicalExists && currentCanonicalExists && same(beforeCanonical, currentCanonicalMeta) && pre(beforeCanonical) && post(currentSwapMeta):
			return routeEvidencePostimageAppendsOneRow(beforeCanonical, currentSwap)
		case beforeCanonicalExists && currentCanonicalExists && same(beforeCanonical, currentSwapMeta) && pre(beforeCanonical) && post(currentCanonicalMeta):
			return routeEvidencePostimageAppendsOneRow(beforeCanonical, currentCanonical)
		case !beforeCanonicalExists && !currentCanonicalExists && emptyPre.hash != "" && post(currentSwapMeta):
			return routeEvidencePostimageAppendsOneRow(emptyPre, currentSwap)
		case !beforeCanonicalExists && currentCanonicalExists && emptyPre.hash != "" && post(currentCanonicalMeta) && same(currentSwapMeta, currentCanonicalMeta):
			return routeEvidencePostimageAppendsOneRow(emptyPre, currentCanonical)
		}
	}

	if !beforeExists || !currentCanonicalExists {
		return false
	}
	// Existing-canonical exchange observed: the prepared postimage inode becomes
	// canonical; if Commit has not run, the displaced canonical inode is the
	// sibling. These cross-name identities are the rooted-CAS proof.
	if beforeCanonicalExists && pre(beforeCanonical) && post(beforeSwap) && same(beforeSwap, currentCanonicalMeta) {
		if currentExists && (!pre(currentSwapMeta) || !same(beforeCanonical, currentSwapMeta)) {
			return false
		}
		return routeEvidencePostimageAppendsOneRow(beforeCanonical, currentCanonical)
	}
	// Doctor began after an existing-canonical exchange. Commit removes the
	// displaced preimage sibling while the canonical postimage inode stays put.
	if beforeCanonicalExists && post(beforeCanonical) && pre(beforeSwap) && !currentExists && same(beforeCanonical, currentCanonicalMeta) {
		return routeEvidencePostimageAppendsOneRow(beforeSwap, currentCanonical)
	}
	// First publication uses a hard link: canonical and sibling share the exact
	// postimage inode until Commit removes the sibling name.
	if beforeCanonicalExists && post(beforeCanonical) && post(beforeSwap) && same(beforeCanonical, beforeSwap) && same(beforeCanonical, currentCanonicalMeta) {
		if currentExists && !same(beforeSwap, currentSwapMeta) {
			return false
		}
		return emptyPre.hash != "" && routeEvidencePostimageAppendsOneRow(emptyPre, currentCanonical)
	}
	// First-publication link/Commit completed between snapshots from a prepared
	// sibling observed while canonical was still absent.
	if !beforeCanonicalExists && emptyPre.hash != "" && post(beforeSwap) && same(beforeSwap, currentCanonicalMeta) {
		if currentExists && !same(beforeSwap, currentSwapMeta) {
			return false
		}
		return routeEvidencePostimageAppendsOneRow(emptyPre, currentCanonical)
	}
	return false
}

func parseRouteEvidenceSwapLeaf(rel string) (string, string, bool) {
	if filepath.Base(rel) != rel {
		return "", "", false
	}
	prefix := "." + routeEvidenceLedgerFile + "."
	if !strings.HasPrefix(rel, prefix) || !strings.HasSuffix(rel, ".swap") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(rel, prefix), ".swap"), ".")
	if len(parts) != 3 {
		return "", "", false
	}
	pid, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || pid == 0 || strconv.FormatUint(pid, 10) != parts[0] || !lowerHex12(parts[1]) || !lowerHex12(parts[2]) || parts[1] == parts[2] {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func lowerHex12(value string) bool {
	if len(value) != 12 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func routeEvidencePostimageAppendsOneRow(pre ledgerFileMeta, postimage []byte) bool {
	if pre.hash == "" || int64(len(postimage)) <= pre.size || sha256Hex(postimage) == pre.hash || sha256Hex(postimage[:pre.size]) != pre.hash {
		return false
	}
	appended := postimage[pre.size:]
	if len(appended) < 2 || appended[len(appended)-1] != '\n' || strings.Count(string(appended), "\n") != 1 {
		return false
	}
	row := appended[:len(appended)-1]
	evidence, err := routecodec.Decode(row)
	if err != nil {
		return false
	}
	canonical, err := routecodec.Encode(evidence)
	return err == nil && bytes.Equal(canonical, row)
}

func (s *Store) routeEvidenceSnapshotArtifactPaths(snapshot ledgerDoctorSnapshot) []string {
	var paths []string
	for rel := range snapshot.files {
		if _, _, ok := parseRouteEvidenceSwapLeaf(rel); ok {
			paths = append(paths, rel)
		}
	}
	sort.Strings(paths)
	return paths
}

func (s *Store) unresolvedConcurrentLedgerArtifacts(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	var violations []string
	rootPath := filepath.Join(s.home, "ledger")
	root, rootInfo, err := openLedgerDoctorRoot(rootPath)
	if err != nil {
		return []string{"cannot verify governed publication artifacts through rooted ledger: " + err.Error()}
	}
	defer root.Close()
	seen := make(map[string]bool, len(paths))
	for _, rel := range paths {
		if seen[rel] {
			continue
		}
		seen[rel] = true
		if _, _, ok := parseRouteEvidenceSwapLeaf(rel); !ok {
			violations = append(violations, "cannot verify non-canonical governed publication artifact "+filepath.Base(rel))
			continue
		}
		ledgerDoctorArtifactProbe(rel, "before")
		_, err := root.Lstat(rel)
		ledgerDoctorArtifactProbe(rel, "after")
		if err == nil {
			violations = append(violations, "governed publication artifact "+filepath.Base(rel)+" remained unresolved across doctor retry")
		} else if !os.IsNotExist(err) {
			violations = append(violations, "cannot verify governed publication artifact "+filepath.Base(rel)+": "+err.Error())
		}
	}
	if err := validateLedgerDoctorRoot(rootPath, root, rootInfo); err != nil {
		violations = append(violations, "cannot verify governed publication artifacts through rooted ledger: "+err.Error())
	}
	return violations
}

// classifyChangedLedgerFile attributes one changed/new ledger file to a legitimate
// producer or to the rented engine.
func classifyChangedLedgerFile(rel string, b []byte, prev ledgerFileMeta, existed bool, delta *ledgerDelta) {
	base := filepath.Base(rel)
	switch {
	case base == "seq" || base == "seq.tmp":
		// nextSeq rewrites the counter atomically (temp+rename); legit if it stays integer.
		if t := strings.TrimSpace(string(b)); t != "" {
			if _, err := strconv.ParseInt(t, 10, 64); err != nil {
				delta.violations = append(delta.violations, "ledger counter "+rel+" holds non-integer bytes (engine-attributed)")
			}
		}
	case strings.HasSuffix(base, ".jsonl"):
		var appended []byte
		if existed {
			if int64(len(b)) >= prev.size && sha256Hex(b[:prev.size]) == prev.hash {
				appended = b[prev.size:] // pure append: the pre-query prefix is byte-identical
			} else {
				delta.violations = append(delta.violations, "governed log "+rel+" rewritten non-append during probe (bypassed the append-only door; engine-attributed)")
				return
			}
		} else {
			appended = b // a producer created a new governed log
		}
		classifyAppendedRows(rel, base, appended, delta)
	default:
		if existed {
			delta.violations = append(delta.violations, "non-log ledger file "+rel+" mutated during probe (engine-attributed)")
		} else {
			delta.violations = append(delta.violations, "new non-log file "+rel+" written under owned ledger (rented engine index must stay outside the ledger; engine-attributed)")
		}
	}
}

// classifyAppendedRows verifies each appended governed row parses and, for the claims log,
// carries producer provenance. A trailing fragment with no newline is an in-flight append
// by a concurrent writer and is skipped, not blamed.
func classifyAppendedRows(rel, base string, appended []byte, delta *ledgerDelta) {
	text := string(appended)
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && !strings.HasSuffix(text, "\n") {
		lines = lines[:len(lines)-1] // drop an in-flight trailing fragment
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var probe json.RawMessage
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			delta.violations = append(delta.violations, "unparseable row appended to "+rel+" (not a governed record; engine-attributed)")
			continue
		}
		if base == "claims.jsonl" {
			var c Claim
			if err := json.Unmarshal([]byte(line), &c); err != nil || strings.TrimSpace(c.Derivation) == "" {
				id := ""
				if c.ID != "" {
					id = " " + c.ID
				}
				delta.violations = append(delta.violations, "claim row"+id+" appended to claims.jsonl without producer provenance (engine-attributed)")
				continue
			}
		}
		delta.rows++
	}
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ledgerDoctorSnapshot pairs generation and bytes under the ledger owner's
// shared lock. The rented query runs outside this lock; only the two observations
// are stabilized so a real owner writer cannot advance between gen and digest.
func (s *Store) ledgerDoctorSnapshot() (ledgerDoctorSnapshot, error) {
	if s.ledgerDoctorSnapshotter != nil {
		return s.ledgerDoctorSnapshotter()
	}
	return s.ledgerDoctorSnapshotFromDisk()
}

func (s *Store) ledgerDoctorSnapshotFromDisk() (ledgerDoctorSnapshot, error) {
	lockPath := filepath.Join(s.home, "lock", "mem.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return ledgerDoctorSnapshot{}, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return ledgerDoctorSnapshot{}, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return ledgerDoctorSnapshot{}, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	heldBefore, epochBefore, err := homepkg.AutonomyAdmissionLockState(s.home)
	if err != nil {
		return ledgerDoctorSnapshot{}, err
	}
	digest, files, err := s.ledgerDigestAndInventory()
	if err != nil {
		return ledgerDoctorSnapshot{}, err
	}
	heldAfter, epochAfter, err := homepkg.AutonomyAdmissionLockState(s.home)
	if err != nil {
		return ledgerDoctorSnapshot{}, err
	}
	ownerLockEpoch := ""
	if heldBefore && heldAfter && epochBefore != "" && epochBefore == epochAfter {
		ownerLockEpoch = epochBefore
	}
	return ledgerDoctorSnapshot{gen: s.currentGen(), digest: digest, files: files, ownerLockEpoch: ownerLockEpoch}, nil
}

// LedgerDigest hashes every file under COMPOUND_HOME/ledger with stable relative
// paths. It is intentionally about owned ledger bytes, not rented engine caches.
func (s *Store) LedgerDigest() (string, error) {
	digest, _, err := s.ledgerDigestAndInventory()
	return digest, err
}

// ledgerDigestAndInventory computes the ledger digest and a per-file fingerprint map in
// one walk. The digest folding is identical to LedgerDigest's original algorithm, so the
// returned digest stays byte-for-byte compatible with existing before/after comparisons.
func (s *Store) ledgerDigestAndInventory() (string, map[string]ledgerFileMeta, error) {
	root := filepath.Join(s.home, "ledger")
	ledgerRoot, rootInfo, err := openLedgerDoctorRoot(root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", map[string]ledgerFileMeta{}, nil
		}
		return "", nil, err
	}
	defer ledgerRoot.Close()
	files, err := enumerateLedgerDoctorFiles(ledgerRoot, rootInfo)
	if err != nil {
		return "", nil, err
	}
	inv := make(map[string]ledgerFileMeta, len(files))
	h := sha256.New()
	for _, file := range files {
		h.Write([]byte(file.rel))
		h.Write([]byte{0})
		h.Write(file.body)
		h.Write([]byte{0})
		inv[file.rel] = ledgerFileMeta{size: int64(len(file.body)), hash: sha256Hex(file.body), info: file.info}
	}
	if err := validateLedgerDoctorRoot(root, ledgerRoot, rootInfo); err != nil {
		return "", nil, err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), inv, nil
}

var (
	ledgerDoctorAfterEntryLstat       = func(string) {}
	ledgerDoctorBeforeFinalEntryLstat = func(string) {}
	ledgerDoctorDirectoryRead         = func(string, string) {}
	ledgerDoctorArtifactProbe         = func(string, string) {}
)

func openLedgerDoctorRoot(path string) (*os.Root, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, nil, fmt.Errorf("ledger root must be a real directory: %s", path)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.IsDir() || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		_ = root.Close()
		return nil, nil, fmt.Errorf("ledger root changed during rooted open: %s", path)
	}
	return root, opened, nil
}

func validateLedgerDoctorRoot(path string, root *os.Root, expected os.FileInfo) error {
	opened, err := root.Stat(".")
	if err != nil {
		return err
	}
	final, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if final.Mode()&os.ModeSymlink != 0 || !final.IsDir() || !os.SameFile(expected, opened) || !os.SameFile(opened, final) {
		return fmt.Errorf("ledger root changed during rooted snapshot: %s", path)
	}
	return nil
}

type ledgerDoctorFile struct {
	rel  string
	body []byte
	info os.FileInfo
}

func enumerateLedgerDoctorFiles(root *os.Root, expectedRoot os.FileInfo) ([]ledgerDoctorFile, error) {
	if root == nil || expectedRoot == nil || !expectedRoot.IsDir() {
		return nil, fmt.Errorf("ledger rooted enumeration requires a pinned directory")
	}
	files, err := enumerateLedgerDoctorDirectory(root, "", expectedRoot)
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, nil
}

func enumerateLedgerDoctorDirectory(root *os.Root, prefix string, expected os.FileInfo) ([]ledgerDoctorFile, error) {
	before, err := root.Lstat(".")
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() || !os.SameFile(expected, before) {
		return nil, fmt.Errorf("ledger snapshot directory changed before rooted enumeration: %s", ledgerDoctorDirectoryLabel(prefix))
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	opened, err := directory.Stat()
	if err != nil {
		_ = directory.Close()
		return nil, err
	}
	if !opened.IsDir() || !os.SameFile(before, opened) {
		_ = directory.Close()
		return nil, fmt.Errorf("ledger snapshot directory changed during rooted open: %s", ledgerDoctorDirectoryLabel(prefix))
	}
	label := ledgerDoctorDirectoryLabel(prefix)
	ledgerDoctorDirectoryRead(label, "before")
	entries, readErr := directory.ReadDir(-1)
	ledgerDoctorDirectoryRead(label, "after")
	openedAfter, statErr := directory.Stat()
	closeErr := directory.Close()
	if err := errors.Join(readErr, statErr, closeErr); err != nil {
		return nil, err
	}
	if !openedAfter.IsDir() || !os.SameFile(opened, openedAfter) {
		return nil, fmt.Errorf("ledger snapshot directory changed during rooted read: %s", label)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	direct := make(map[string]os.FileInfo, len(entries))
	files := make([]ledgerDoctorFile, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name == "" || name == "." || filepath.Base(name) != name || !filepath.IsLocal(name) {
			return nil, fmt.Errorf("ledger snapshot directory returned invalid entry %q", name)
		}
		rel := name
		if prefix != "" {
			rel = filepath.Join(prefix, name)
		}
		info, err := root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("ledger snapshot entry must be a regular file with no symlink path: %s", rel)
		}
		direct[name] = info
		switch {
		case info.IsDir():
			child, err := root.OpenRoot(name)
			if err != nil {
				return nil, err
			}
			childOpened, openErr := child.Stat(".")
			linkAfter, linkErr := root.Lstat(name)
			if err := errors.Join(openErr, linkErr); err != nil {
				_ = child.Close()
				return nil, err
			}
			if !childOpened.IsDir() || linkAfter.Mode()&os.ModeSymlink != 0 || !linkAfter.IsDir() ||
				!os.SameFile(info, childOpened) || !os.SameFile(childOpened, linkAfter) {
				_ = child.Close()
				return nil, fmt.Errorf("ledger snapshot path changed during rooted directory open: %s", rel)
			}
			children, childErr := enumerateLedgerDoctorDirectory(child, rel, childOpened)
			childFinal, childStatErr := child.Stat(".")
			linkFinal, linkStatErr := root.Lstat(name)
			childCloseErr := child.Close()
			if err := errors.Join(childErr, childStatErr, linkStatErr, childCloseErr); err != nil {
				return nil, err
			}
			if !childFinal.IsDir() || linkFinal.Mode()&os.ModeSymlink != 0 || !linkFinal.IsDir() ||
				!os.SameFile(info, childFinal) || !os.SameFile(childFinal, linkFinal) {
				return nil, fmt.Errorf("ledger snapshot path changed during rooted enumeration: %s", rel)
			}
			files = append(files, children...)
		case info.Mode().IsRegular():
			body, finalInfo, err := readLedgerDoctorLeaf(root, name, rel, info)
			if err != nil {
				return nil, err
			}
			direct[name] = finalInfo
			files = append(files, ledgerDoctorFile{rel: rel, body: body, info: finalInfo})
		default:
			return nil, fmt.Errorf("ledger snapshot entry must be a regular file: %s", rel)
		}
	}
	finalEntries, err := readLedgerDoctorDirectoryNames(root)
	if err != nil {
		return nil, err
	}
	if !sameLedgerDoctorDirectoryNames(entries, finalEntries) {
		return nil, fmt.Errorf("ledger snapshot file set changed during rooted enumeration: %s", label)
	}
	for name, expectedEntry := range direct {
		final, err := root.Lstat(name)
		if err != nil {
			return nil, err
		}
		stable := final.Mode()&os.ModeSymlink == 0
		if expectedEntry.Mode().IsRegular() {
			stable = stable && final.Mode().IsRegular() && sameLedgerDoctorFileVersion(expectedEntry, final)
		} else {
			stable = stable && expectedEntry.IsDir() && final.IsDir() && os.SameFile(expectedEntry, final)
		}
		if !stable {
			return nil, fmt.Errorf("ledger snapshot entry changed during rooted enumeration: %s", filepath.Join(prefix, name))
		}
	}
	final, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	if !final.IsDir() || !os.SameFile(expected, final) {
		return nil, fmt.Errorf("ledger snapshot directory changed during rooted enumeration: %s", label)
	}
	return files, nil
}

func readLedgerDoctorLeaf(root *os.Root, name, rel string, expected os.FileInfo) ([]byte, os.FileInfo, error) {
	leafBefore, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if leafBefore.Mode()&os.ModeSymlink != 0 || !leafBefore.Mode().IsRegular() || !os.SameFile(expected, leafBefore) {
		return nil, nil, fmt.Errorf("ledger snapshot entry changed before rooted read: %s", rel)
	}
	ledgerDoctorAfterEntryLstat(rel)
	file, err := root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	body, readErr := io.ReadAll(file)
	openedAfter, statErr := file.Stat()
	closeErr := file.Close()
	if err := errors.Join(readErr, statErr, closeErr); err != nil {
		return nil, nil, err
	}
	ledgerDoctorBeforeFinalEntryLstat(rel)
	leafAfter, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if leafAfter.Mode()&os.ModeSymlink != 0 || !leafAfter.Mode().IsRegular() ||
		!sameLedgerDoctorFileVersion(expected, opened) || !sameLedgerDoctorFileVersion(opened, openedAfter) ||
		!sameLedgerDoctorFileVersion(openedAfter, leafAfter) || int64(len(body)) != openedAfter.Size() {
		return nil, nil, fmt.Errorf("ledger snapshot entry changed during rooted read: %s", rel)
	}
	return body, openedAfter, nil
}

func sameLedgerDoctorFileVersion(left, right os.FileInfo) bool {
	return left != nil && right != nil && os.SameFile(left, right) &&
		left.Size() == right.Size() && left.ModTime().Equal(right.ModTime())
}

func readLedgerDoctorDirectoryNames(root *os.Root) ([]os.DirEntry, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	opened, err := directory.Stat()
	if err != nil {
		_ = directory.Close()
		return nil, err
	}
	entries, readErr := directory.ReadDir(-1)
	openedAfter, statErr := directory.Stat()
	closeErr := directory.Close()
	if err := errors.Join(readErr, statErr, closeErr); err != nil {
		return nil, err
	}
	if !opened.IsDir() || !openedAfter.IsDir() || !os.SameFile(opened, openedAfter) {
		return nil, fmt.Errorf("ledger snapshot directory changed while verifying file set")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func sameLedgerDoctorDirectoryNames(left, right []os.DirEntry) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Name() != right[index].Name() {
			return false
		}
	}
	return true
}

func ledgerDoctorDirectoryLabel(prefix string) string {
	if prefix == "" {
		return "."
	}
	return prefix
}
