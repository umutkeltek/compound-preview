package candidates

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

var (
	linkCandidateFile = func(directory *candidateDirectory, source, target string) error {
		return directory.link(source, target)
	}
	renameCandidateFile = func(directory *candidateDirectory, source, target string) error {
		return directory.rename(source, target)
	}
	removeCandidateFile = func(directory *candidateDirectory, name string) error {
		return directory.remove(name)
	}
	syncCandidateDirectoryFn = func(directory *candidateDirectory) error {
		return directory.sync()
	}
	// publishCandidateReplacement / publishCandidateCreation are the exact
	// publish steps, isolated behind vars so tests can inject drift between the
	// writer's snapshot, staging, and publication and prove the writer fails
	// closed with a typed CorruptionRaced instead of publishing over it.
	publishCandidateReplacement = publishCandidateReplacementExact
	publishCandidateCreation    = publishCandidateCreationExact
	// captureAuthorizedReplacement is set by tests to capture the Record handed
	// to authorizeCandidateReplacement. It runs AFTER classifyCandidateRecord
	// (so callers see a record that was actually accepted for authorization)
	// and BEFORE the file is re-opened for the live-identity Fstat — that
	// ordering is what proves the test was checking the same Record the
	// production path was about to compare against. nil = no capture.
	captureAuthorizedReplacement func(kind Kind, id string, record Record)
)

// candidateID rejects an id that is empty or path-shaped (a directory component,
// "." / "..", or a path separator). A candidate id keys an inbox file path, so a
// crafted value must never be able to escape inbox/<kind>-candidates/.
func candidateID(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("candidate id required")
	}
	if !utf8.ValidString(id) {
		return fmt.Errorf("candidate id must be valid UTF-8")
	}
	if strings.IndexFunc(id, unicode.IsControl) >= 0 {
		return fmt.Errorf("candidate id must not contain control characters, got %q", id)
	}
	if id != strings.TrimSpace(id) {
		return fmt.Errorf("candidate id must not contain surrounding whitespace, got %q", id)
	}
	if id == "." || id == ".." || id != filepath.Base(id) || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("candidate id must be a bare filename, got %q", id)
	}
	return nil
}

// ReadCandidate reads inbox/<kind>-candidates/<id>.json from COMPOUND_HOME,
// verifies the stored id matches the requested id, and decodes the record into
// target. The candidate inbox JSON is a cross-package contract: every reader
// (distiller's read*Candidate, mem's write-plan reader, decisions' decision
// reader) re-implemented the identical path-build + read + id guard. This is the
// one owner of that read; target is any struct whose json tags project the
// fields its package needs from the one record.
//
// A read error — including a missing file — preserves os.IsNotExist semantics
// so a caller that probes several inboxes can continue. Custody, canonical
// filename, and nonblank matching JSON identity are enforced before target is
// decoded.
func ReadCandidate(homePath string, kind Kind, id string, target any) error {
	record, err := ReadRecord(homePath, kind, id)
	if err != nil {
		return err
	}
	return json.Unmarshal(record.Body, target)
}

// WriteCandidate persists a candidate record to inbox/<kind>-candidates/<id>.json
// under COMPOUND_HOME. It is the one owner of the candidate-inbox WRITE mechanism
// — the twin of ReadCandidate — so every producer (distiller fan-out, source
// observations, product hypotheses, memory capture/adjudication) hands its own
// projected record as payload and shares this path + serialization (canonical
// two-space-indented JSON plus a trailing newline).
//
// It refuses to file a record under a path the record itself contradicts: an
// unknown kind, a path-shaped id, or a payload whose own non-empty json "id" /
// "kind" disagrees with the requested id / kind are all rejected before any disk
// write.
func WriteCandidate(homePath string, kind Kind, id string, payload any) error {
	dir, data, err := candidatePayload(kind, id, payload)
	if err != nil {
		return err
	}
	return withCandidatePublicationLock(homePath, dir, func(inbox *candidateInbox) error {
		return replaceCandidate(inbox, kind, id, data)
	})
}

// ReplaceCandidateResult distinguishes a committed replacement from an exact
// newer generation observed under the same candidate publication lock.
type ReplaceCandidateResult struct {
	Replaced bool
	Current  Record
}

// DigestPrecondition is the durable part of a descriptor-pinned candidate
// record that a persisted plan may carry across processes. The mutation door
// reacquires rooted custody and compares this digest while holding the
// candidate publication lock; callers never reconstruct Record identities.
type DigestPrecondition struct {
	ID     string
	Digest string
}

// ConditionalMutationResult reports the exact lock winner when any target or
// companion precondition drifted. Target is always the current target record
// when it was readable; Current is the record named by MismatchID.
type ConditionalMutationResult struct {
	Applied    bool
	Target     Record
	Current    Record
	MismatchID string
}

// ReplaceCandidateIfCurrent atomically replaces a candidate only when the
// descriptor-pinned record read by an earlier page walk is still the exact
// live generation. The comparison and replacement share the candidate
// publication lock, so another producer cannot win between the precondition
// check and rename. A valid newer generation returns replaced=false without
// being overwritten; corruption or root-custody drift remains an error.
func ReplaceCandidateIfCurrent(homePath string, expected Record, payload any) (result ReplaceCandidateResult, resultErr error) {
	if err := expected.Validate(); err != nil {
		return result, fmt.Errorf("validate expected candidate record: %w", err)
	}
	dir, data, err := candidatePayload(expected.Kind, expected.ID, payload)
	if err != nil {
		return result, err
	}
	err = withCandidatePublicationLock(homePath, dir, func(inbox *candidateInbox) error {
		if !expected.homeIdentity.sameObject(inbox.home.identity) || !expected.directoryIdentity.sameObject(inbox.directory.identity) {
			return &Corruption{
				EntryCursor: expected.EntryCursor,
				Code:        CorruptionRaced,
				Detail:      "candidate root custody changed since page read",
			}
		}
		current, err := readRootedRecord(inbox, expected.Kind, string(expected.EntryCursor), expected.ID)
		if err != nil {
			return err
		}
		if !expected.fileIdentity.same(current.fileIdentity) || expected.Digest != current.Digest || !bytes.Equal(expected.Body, current.Body) {
			result.Current = current
			return nil
		}
		if err := replaceCandidateFromRecord(inbox, expected.Kind, expected.ID, data, current); err != nil {
			return err
		}
		result.Replaced = true
		return nil
	})
	return result, err
}

// MutateCandidateIfDigests validates one target and any companion records
// under the candidate publication lock, then lets the owner prepare the
// target's replacement while that exact generation remains pinned. This is
// the cross-process CAS seam for durable plans: a producer replacement either
// wins before validation (the mutation callback never runs) or after the
// committed replacement, never between validation and an authority write.
func MutateCandidateIfDigests(
	homePath string,
	kind Kind,
	targetID string,
	preconditions []DigestPrecondition,
	mutate func(Record) (any, error),
) (result ConditionalMutationResult, resultErr error) {
	if !Known(kind) {
		return result, fmt.Errorf("unknown candidate kind %q", kind)
	}
	if err := candidateID(targetID); err != nil {
		return result, err
	}
	if mutate == nil {
		return result, fmt.Errorf("candidate mutation callback required")
	}
	if len(preconditions) == 0 {
		return result, fmt.Errorf("candidate digest preconditions required")
	}
	seen := make(map[string]bool, len(preconditions))
	targetDeclared := false
	for _, precondition := range preconditions {
		if err := candidateID(precondition.ID); err != nil {
			return result, err
		}
		if strings.TrimSpace(precondition.Digest) == "" {
			return result, fmt.Errorf("candidate %s digest required", precondition.ID)
		}
		if seen[precondition.ID] {
			return result, fmt.Errorf("duplicate candidate digest precondition %s", precondition.ID)
		}
		seen[precondition.ID] = true
		targetDeclared = targetDeclared || precondition.ID == targetID
	}
	if !targetDeclared {
		return result, fmt.Errorf("target candidate %s has no digest precondition", targetID)
	}

	dir := InboxDir(kind)
	return result, withCandidatePublicationLock(homePath, dir, func(inbox *candidateInbox) error {
		var target Record
		for _, precondition := range preconditions {
			current, err := readRootedRecord(inbox, kind, precondition.ID+".json", precondition.ID)
			if err != nil {
				if os.IsNotExist(err) {
					result.MismatchID = precondition.ID
					return nil
				}
				return err
			}
			if precondition.ID == targetID {
				target = current
				result.Target = current
			}
			if current.Digest != precondition.Digest {
				result.MismatchID = precondition.ID
				result.Current = current
				return nil
			}
		}

		payload, err := mutate(target)
		if err != nil {
			return err
		}
		payloadDir, data, err := candidatePayload(kind, targetID, payload)
		if err != nil {
			return err
		}
		if payloadDir != dir {
			return fmt.Errorf("candidate payload directory %q does not match %q", payloadDir, dir)
		}
		if err := replaceCandidateFromRecord(inbox, kind, targetID, data, target); err != nil {
			return err
		}
		current, err := readRootedRecord(inbox, kind, targetID+".json", targetID)
		if err != nil {
			return fmt.Errorf("read committed candidate %s: %w", targetID, err)
		}
		result.Applied = true
		result.Target = current
		result.Current = current
		return nil
	})
}

// CreateCandidateIfAbsent persists a candidate only when its canonical inbox
// path does not already exist. Any existing candidate remains byte-identical;
// unsafe existing objects are reported rather than followed or replaced.
func CreateCandidateIfAbsent(homePath string, kind Kind, id string, payload any) (bool, error) {
	dir, data, err := candidatePayload(kind, id, payload)
	if err != nil {
		return false, err
	}
	created := false
	err = withCandidatePublicationLock(homePath, dir, func(inbox *candidateInbox) error {
		directory := inbox.directory
		targetName := id + ".json"
		existing, err := readCandidateRecordFromDirectory(inbox, kind, id)
		switch {
		case err == nil:
			return classifyCandidateRecord(existing, kind, id)
		case os.IsNotExist(err):
			// Absent target: the publish step re-proves absence before linking.
		default:
			return fmt.Errorf("inspect existing candidate: %w", err)
		}
		tempName, err := stageCandidate(directory, data)
		if err != nil {
			return err
		}
		keepTemp := true
		defer func() {
			if keepTemp {
				_ = directory.remove(tempName)
			}
		}()
		staged, err := readCustodiedCandidateRecord(directory, kind, tempName, id)
		if err != nil {
			return fmt.Errorf("read staged candidate: %w", err)
		}
		if err := publishCandidateCreation(directory, targetName, tempName, staged); err != nil {
			if removeErr := removeCandidateFile(directory, tempName); removeErr != nil {
				return errors.Join(err, fmt.Errorf("remove candidate temp: %w", removeErr))
			}
			keepTemp = false
			return err
		}
		created = true
		var cleanupErr error
		if err := removeCandidateFile(directory, tempName); err != nil {
			cleanupErr = fmt.Errorf("remove candidate temp: %w", err)
		} else {
			keepTemp = false
		}
		var syncErr error
		if err := syncCandidateDirectoryFn(directory); err != nil {
			syncErr = fmt.Errorf("sync candidate directory: %w", err)
		}
		return errors.Join(cleanupErr, syncErr)
	})
	return created, err
}

// withCandidatePublicationLock serializes candidate publication across
// processes. Memory may call a candidate writer while holding mem.lock, so the
// only permitted nesting is mem.lock -> candidates.lock; this leaf never
// reaches back into memory or acquires another Compound lock.
func withCandidatePublicationLock(homePath, inboxPath string, fn func(*candidateInbox) error) (resultErr error) {
	home, err := openCandidateHome(homePath)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, home.close())
	}()
	lockDirectory, err := home.openDirectory("lock", true)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, lockDirectory.close())
	}()
	lockFile, err := lockDirectory.openPublicationLock()
	if err != nil {
		return err
	}
	defer func() {
		if err := lockFile.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close candidate publication lock: %w", err))
		}
	}()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock candidate publication: %w", err)
	}
	defer func() {
		if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("unlock candidate publication: %w", err))
		}
	}()
	directory, err := home.openDirectory(inboxPath, true)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, directory.close())
	}()
	return fn(&candidateInbox{home: home, directory: directory})
}

func replaceCandidate(inbox *candidateInbox, kind Kind, id string, data []byte) error {
	expected, err := candidateReplacementPreflight(inbox, kind, id)
	if err != nil {
		return err
	}
	return stageAndReplaceCandidate(inbox, kind, id, data, expected)
}

func replaceCandidateFromRecord(inbox *candidateInbox, kind Kind, id string, data []byte, current Record) error {
	if err := authorizeCandidateReplacement(inbox, kind, id, current); err != nil {
		return err
	}
	return stageAndReplaceCandidate(inbox, kind, id, data, &current)
}

// stageAndReplaceCandidate stages the replacement body, proves the staged
// bytes still match at publish time, and atomically renames over the target —
// failing closed with CorruptionRaced when the target or the staged temp
// drifted from the exact records this writer validated.
func stageAndReplaceCandidate(inbox *candidateInbox, kind Kind, id string, data []byte, expected *Record) error {
	directory := inbox.directory
	tempName, err := stageCandidate(directory, data)
	if err != nil {
		return err
	}
	keepTemp := true
	defer func() {
		if keepTemp {
			_ = directory.remove(tempName)
		}
	}()
	staged, err := readCustodiedCandidateRecord(directory, kind, tempName, id)
	if err != nil {
		return fmt.Errorf("read staged candidate: %w", err)
	}
	if err := publishCandidateReplacement(directory, id+".json", tempName, expected, staged); err != nil {
		return err
	}
	keepTemp = false
	if err := syncCandidateDirectoryFn(directory); err != nil {
		return fmt.Errorf("sync candidate directory: %w", err)
	}
	return nil
}

// publishCandidateReplacementExact revalidates the exact pre-publish records
// it was handed — target still at the writer's snapshot (or still absent) and
// staged temp unchanged — then renames and proves the published target is the
// staged record. Any drift is a typed CorruptionRaced, never a silent
// overwrite or a torn publication.
func publishCandidateReplacementExact(directory *candidateDirectory, targetName, tempName string, expected *Record, staged Record) error {
	if expected == nil {
		if err := requireCandidateAbsent(directory, targetName); err != nil {
			return err
		}
	} else if err := requireExactCandidateRecord(directory, targetName, *expected, "candidate changed after writer snapshot"); err != nil {
		return err
	}
	if err := requireExactCandidateRecord(directory, tempName, staged, "staged candidate changed before replacement"); err != nil {
		return err
	}
	if err := renameCandidateFile(directory, tempName, targetName); err != nil {
		return fmt.Errorf("replace candidate: %w", err)
	}
	return requireExactCandidateRecord(directory, targetName, staged, "published candidate does not match staged record")
}

// publishCandidateCreationExact proves the target is still absent and the
// staged temp unchanged, hard-links the temp into the authoritative name, and
// proves the published target is the staged record. A target that appears
// mid-publication is a typed race, never an overwrite.
func publishCandidateCreationExact(directory *candidateDirectory, targetName, tempName string, staged Record) error {
	if err := requireCandidateAbsent(directory, targetName); err != nil {
		return err
	}
	if err := requireExactCandidateRecord(directory, tempName, staged, "staged candidate changed before creation"); err != nil {
		return err
	}
	if err := linkCandidateFile(directory, tempName, targetName); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return candidatePublicationRace(targetName, "candidate appeared before creation", err)
		}
		return fmt.Errorf("publish candidate: %w", err)
	}
	return requireExactCandidateRecord(directory, targetName, staged, "published candidate does not match staged record")
}

func requireCandidateAbsent(directory *candidateDirectory, targetName string) error {
	_, err := directory.lstat(targetName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return candidatePublicationRace(targetName, "inspect absent candidate precondition", err)
	}
	return candidatePublicationRace(targetName, "candidate appeared after writer snapshot", nil)
}

// requireExactCandidateRecord re-reads one directory entry under the
// publication lock and demands the exact record the writer validated earlier:
// same inode (rename/link update ctime, so object identity is compared, not
// timestamps), same digest, same bytes.
func requireExactCandidateRecord(directory *candidateDirectory, entryName string, expected Record, detail string) error {
	current, err := readCustodiedCandidateRecord(directory, expected.Kind, entryName, expected.ID)
	if err != nil {
		return candidatePublicationRace(entryName, detail, err)
	}
	if !sameCandidateRecord(current, expected) {
		return candidatePublicationRace(entryName, detail, nil)
	}
	return nil
}

func sameCandidateRecord(left, right Record) bool {
	return left.Kind == right.Kind &&
		left.ID == right.ID &&
		left.Digest == right.Digest &&
		bytes.Equal(left.Body, right.Body) &&
		left.fileIdentity.sameObject(right.fileIdentity)
}

func candidatePublicationRace(entryName, detail string, cause error) error {
	return &Corruption{
		EntryCursor: EntryCursor(entryName),
		Code:        CorruptionRaced,
		Detail:      detail,
		cause:       cause,
	}
}

// candidateReplacementPreflight preserves WriteCandidate's existing
// authorization boundary: atomic rename must not make a read-only regular
// target replaceable when the former in-place writer could not open it. It
// returns the exact pre-write record (nil when the target is absent) so the
// publish step can prove the target never drifted from this snapshot.
func candidateReplacementPreflight(inbox *candidateInbox, kind Kind, id string) (*Record, error) {
	record, err := readCandidateRecordFromDirectory(inbox, kind, id)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect candidate replacement target: %w", err)
	}
	if err := authorizeCandidateReplacement(inbox, kind, id, record); err != nil {
		return nil, err
	}
	return &record, nil
}

func authorizeCandidateReplacement(inbox *candidateInbox, kind Kind, id string, record Record) error {
	if err := classifyCandidateRecord(record, kind, id); err != nil {
		return err
	}
	if captureAuthorizedReplacement != nil {
		captureAuthorizedReplacement(kind, id, record)
	}
	// R3F (promote-driver frozen-park recovery, 2026-09-04): the previous
	// Fstat-vs-identity compare refused when only the file's volatile
	// metadata (mtime/ctime/atime) drifted between the caller's read and
	// this leaf's re-open. APFS no-op touches (Spotlight, quarantine,
	// atime updates that bump ctime) and any kernel cache that returns a
	// different stat snapshot across FDs trigger `changed after custody
	// read` even though the candidate's body bytes never moved. The
	// custody guard this leaf exists to enforce is content generation, not
	// metadata: an idempotent write that overwrites identical bytes with
	// identical bytes is safe under the candidate publication lock regardless
	// of when atime was last bumped. We compare the live file's body to
	// the recorded digest instead. A file whose body genuinely drifted
	// (different digest) is still refused; the content-drift and metadata-
	// drift cases are intentionally separable. The body is read through
	// candidateDigestForPath on a fresh RDONLY FD, descriptor-rooted via
	// the inbox directory to keep canonical-path and no-follow guarantees.
	// The write-mode open is the writability probe the old identity compare
	// carried implicitly: a candidate the process may not write (0400, foreign
	// owner) must refuse HERE with the permission error, before any staging,
	// so a projection cannot report success it never had
	// (internal/mem TestRaiseSeedsFailsBeforeEdgeWhenCandidateProjectionWriteFails).
	probe, probeErr := inbox.directory.openExistingWritable(id + ".json")
	if probeErr != nil {
		return fmt.Errorf("authorize candidate replacement: %w", probeErr)
	}
	if closeErr := probe.Close(); closeErr != nil {
		return fmt.Errorf("authorize candidate replacement: %w", closeErr)
	}
	liveDigest, readErr := candidateDigestForPath(inbox.directory, id+".json")
	if readErr != nil {
		return fmt.Errorf("read live candidate body: %w", readErr)
	}
	if liveDigest != record.Digest {
		return fmt.Errorf("candidate replacement target changed after custody read")
	}
	return nil
}

func stageCandidate(directory *candidateDirectory, data []byte) (_ string, resultErr error) {
	tempName, file, err := directory.createTempFile()
	if err != nil {
		return "", fmt.Errorf("create candidate temp: %w", err)
	}
	closed := false
	keep := false
	defer func() {
		if !closed {
			if err := file.Close(); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("close candidate temp: %w", err))
			}
		}
		if !keep {
			_ = directory.remove(tempName)
		}
	}()
	if err := file.Chmod(0o644); err != nil {
		return "", fmt.Errorf("set candidate temp mode: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return "", fmt.Errorf("write candidate temp: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("sync candidate temp: %w", err)
	}
	closeErr := file.Close()
	closed = true
	if closeErr != nil {
		return "", fmt.Errorf("close candidate temp: %w", closeErr)
	}
	keep = true
	return tempName, nil
}

func candidatePayload(kind Kind, id string, payload any) (string, []byte, error) {
	dir := InboxDir(kind)
	if dir == "" {
		return "", nil, fmt.Errorf("unknown candidate kind %q", kind)
	}
	if err := candidateID(id); err != nil {
		return "", nil, err
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", nil, err
	}
	meta, err := decodeCandidateJSONIdentity(data)
	if err != nil {
		return "", nil, err
	}
	if meta.ID == "" {
		return "", nil, fmt.Errorf("candidate payload id required")
	}
	if err := candidateID(meta.ID); err != nil {
		return "", nil, fmt.Errorf("candidate payload id: %w", err)
	}
	if meta.ID != id {
		return "", nil, fmt.Errorf("candidate payload id %q does not match requested %q", meta.ID, id)
	}
	if meta.Kind != "" && meta.Kind != string(kind) {
		return "", nil, fmt.Errorf("candidate payload kind %q does not match requested kind %q", meta.Kind, kind)
	}
	return dir, append(data, '\n'), nil
}

func classifyCandidateRecord(record Record, kind Kind, id string) error {
	meta, err := decodeCandidateJSONIdentity(record.Body)
	if err != nil {
		return fmt.Errorf("decode existing candidate identity: %w", err)
	}
	if record.ID != id {
		return fmt.Errorf("existing candidate id %q does not match requested %q", record.ID, id)
	}
	if meta.Kind != "" && meta.Kind != string(kind) {
		return fmt.Errorf("existing candidate kind %q does not match requested kind %q", meta.Kind, kind)
	}
	return nil
}
