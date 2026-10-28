package candidates

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var candidateAfterOpenHook = func(string) {}

const entryCursorJSONPrefix = "filename-v1:"

// EntryCursor is the exact filename byte sequence returned by the candidate
// directory. Its JSON form is versioned base64url so Linux filenames that are
// not valid UTF-8 remain durable and resumable without replacement bytes.
type EntryCursor string

func (cursor EntryCursor) MarshalJSON() ([]byte, error) {
	token, err := cursor.Token()
	if err != nil {
		return nil, err
	}
	return json.Marshal(token)
}

func (cursor *EntryCursor) UnmarshalJSON(data []byte) error {
	if cursor == nil {
		return fmt.Errorf("candidate entry cursor target required")
	}
	var token string
	if err := json.Unmarshal(data, &token); err != nil {
		return err
	}
	decoded, err := parseEntryCursorToken(token)
	if err != nil {
		return err
	}
	*cursor = decoded
	return nil
}

// Token returns the copy-safe, JSON-safe representation accepted by
// ParseEntryCursor and CLI --after flags.
func (cursor EntryCursor) Token() (string, error) {
	if err := candidateEntryCursor(cursor); err != nil {
		return "", err
	}
	return entryCursorJSONPrefix + base64.RawURLEncoding.EncodeToString([]byte(cursor)), nil
}

// ParseEntryCursor decodes a versioned cursor token. A raw filename remains
// accepted for interactive compatibility, but durable JSON always emits the
// versioned form through MarshalJSON.
func ParseEntryCursor(value string) (EntryCursor, error) {
	if !strings.HasPrefix(value, entryCursorJSONPrefix) {
		return EntryCursorFromFilename(value)
	}
	return parseEntryCursorToken(value)
}

// EntryCursorFromFilename validates an exact candidate directory-entry name
// without interpreting the versioned-token prefix. Internal migrations use
// this when the legacy value is already known to be a filename rather than a
// CLI token.
func EntryCursorFromFilename(filename string) (EntryCursor, error) {
	cursor := EntryCursor(filename)
	return cursor, candidateEntryCursor(cursor)
}

func parseEntryCursorToken(value string) (EntryCursor, error) {
	if !strings.HasPrefix(value, entryCursorJSONPrefix) {
		return "", fmt.Errorf("candidate entry cursor token has unsupported version")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, entryCursorJSONPrefix))
	if err != nil {
		return "", fmt.Errorf("decode candidate entry cursor: %w", err)
	}
	cursor := EntryCursor(string(raw))
	return cursor, candidateEntryCursor(cursor)
}

// CorruptionCode is the stable reason a candidate directory entry cannot be
// treated as an actionable candidate.
type CorruptionCode string

const (
	CorruptionInvalidFilename CorruptionCode = "invalid_filename"
	CorruptionSymlink         CorruptionCode = "symlink"
	CorruptionNonRegular      CorruptionCode = "nonregular"
	CorruptionUnreadable      CorruptionCode = "unreadable"
	CorruptionInvalidJSON     CorruptionCode = "invalid_json"
	CorruptionBlankID         CorruptionCode = "blank_id"
	CorruptionInvalidID       CorruptionCode = "invalid_id"
	CorruptionIDMismatch      CorruptionCode = "id_mismatch"
	CorruptionRaced           CorruptionCode = "raced"
)

// Corruption names the exact directory entry and why it was refused. It is
// returned as an error for point reads and as data for page walks so one bad
// entry can never become actionable or disappear from pagination evidence.
type Corruption struct {
	EntryCursor EntryCursor    `json:"entry_cursor"`
	Code        CorruptionCode `json:"code"`
	Detail      string         `json:"detail,omitempty"`
	cause       error
}

func (c *Corruption) Error() string {
	if c == nil {
		return "candidate corruption"
	}
	if c.Detail == "" {
		return fmt.Sprintf("candidate entry %q is corrupt: %s", c.EntryCursor, c.Code)
	}
	return fmt.Sprintf("candidate entry %q is corrupt: %s (%s)", c.EntryCursor, c.Code, c.Detail)
}

func (c *Corruption) Unwrap() error {
	if c == nil {
		return nil
	}
	return c.cause
}

// Record is a descriptor-pinned snapshot. Body, Digest, EntryCursor, Kind,
// and CustodyToken describe the same file generation. The unexported file and
// directory identities prevent a reconstructed JSON value from impersonating
// a live custody token in later compare-and-swap consumers.
type Record struct {
	Kind         Kind        `json:"kind"`
	ID           string      `json:"id"`
	Body         []byte      `json:"-"`
	Digest       string      `json:"digest"`
	EntryCursor  EntryCursor `json:"entry_cursor"`
	CustodyToken string      `json:"custody_token"`

	homeIdentity      candidateFileIdentity
	directoryIdentity candidateFileIdentity
	fileIdentity      candidateFileIdentity
}

// ModifiedAt returns the descriptor-pinned file modification time captured
// with this record. Projections use it instead of reopening the pathname after
// custody validation.
func (r Record) ModifiedAt() time.Time {
	if r.fileIdentity.mtimeSecond == 0 && r.fileIdentity.mtimeNano == 0 {
		return time.Time{}
	}
	return time.Unix(r.fileIdentity.mtimeSecond, r.fileIdentity.mtimeNano).UTC()
}

// Validate proves the exported record fields still describe the descriptor-
// pinned body and identities minted by this package.
func (r Record) Validate() error {
	if !Known(r.Kind) {
		return fmt.Errorf("candidate record kind %q is unknown", r.Kind)
	}
	if err := candidateID(r.ID); err != nil {
		return err
	}
	if r.EntryCursor != EntryCursor(r.ID+".json") {
		return fmt.Errorf("candidate record cursor %q does not match id %q", r.EntryCursor, r.ID)
	}
	digest := candidateBodyDigest(r.Body)
	if r.Digest != digest {
		return fmt.Errorf("candidate record digest %q does not match body %q", r.Digest, digest)
	}
	want := candidateCustodyToken(r.Kind, r.EntryCursor, digest, r.homeIdentity, r.directoryIdentity, r.fileIdentity)
	if r.CustodyToken == "" || r.CustodyToken != want {
		return fmt.Errorf("candidate record custody token mismatch")
	}
	return nil
}

// PageEntry preserves directory order across valid and corrupt entries. The
// order is the safe cursor frontier used by bounded mutation consumers.
type PageEntry struct {
	EntryCursor EntryCursor `json:"entry_cursor"`
	Record      *Record     `json:"record,omitempty"`
	Corruption  *Corruption `json:"corruption,omitempty"`
}

// Page is one stable, bounded directory walk. LastCursor is the exact filename
// bytes returned by the directory, not a decoded candidate ID.
type Page struct {
	Entries    []PageEntry `json:"entries,omitempty"`
	Walked     int         `json:"walked"`
	LastCursor EntryCursor `json:"last_cursor,omitempty"`
	AtEnd      bool        `json:"at_end"`
}

// ReadRecord reads one exact canonical candidate through a descriptor-rooted
// inbox. Every directory component and the final file are opened without
// following symlinks.
func ReadRecord(homePath string, kind Kind, id string) (record Record, resultErr error) {
	dir := InboxDir(kind)
	if dir == "" {
		return Record{}, fmt.Errorf("unknown candidate kind %q", kind)
	}
	if err := candidateID(id); err != nil {
		return Record{}, err
	}
	inbox, err := openCandidateInbox(homePath, dir, false)
	if err != nil {
		return Record{}, err
	}
	defer func() {
		if err := inbox.close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close candidate custody: %w", err))
		}
	}()
	return readCandidateRecordFromDirectory(inbox, kind, id)
}

func readCandidateRecordFromDirectory(inbox *candidateInbox, kind Kind, id string) (Record, error) {
	entryCursor := id + ".json"
	names, err := candidateReadDirectoryNames(inbox.directory)
	if err != nil {
		return Record{}, err
	}
	if !candidateExactEntryName(names, entryCursor) {
		if _, statErr := inbox.directory.lstat(entryCursor); statErr == nil {
			return Record{}, &Corruption{EntryCursor: EntryCursor(entryCursor), Code: CorruptionInvalidFilename, Detail: "requested filename bytes do not match a directory entry"}
		} else if errors.Is(statErr, fs.ErrNotExist) {
			return Record{}, &os.PathError{Op: "open", Path: inbox.directory.path(entryCursor), Err: fs.ErrNotExist}
		} else {
			return Record{}, corruptionForReadError(entryCursor, statErr)
		}
	}
	return readRootedRecord(inbox, kind, entryCursor, id)
}

func candidateExactEntryName(names []string, requested string) bool {
	for _, name := range names {
		if name == requested {
			return true
		}
	}
	return false
}

// ReadPage walks candidate directory entries in exact filename-byte order.
// afterCursor is an opaque filename previously returned as LastCursor.
func ReadPage(homePath string, kind Kind, afterCursor EntryCursor, scanLimit int) (page Page, resultErr error) {
	dir := InboxDir(kind)
	if dir == "" {
		return Page{}, fmt.Errorf("unknown candidate kind %q", kind)
	}
	if scanLimit < 0 {
		return Page{}, fmt.Errorf("candidate scan limit must be non-negative")
	}
	if err := candidateEntryCursor(afterCursor); err != nil {
		return Page{}, err
	}
	inbox, err := openCandidateInbox(homePath, dir, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Page{AtEnd: true}, nil
		}
		return Page{}, err
	}
	defer func() {
		if err := inbox.close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close candidate custody: %w", err))
		}
	}()
	allNames, err := candidateReadDirectoryNames(inbox.directory)
	if err != nil {
		return Page{}, err
	}
	names := append([]string(nil), allNames...)
	sort.Strings(names)
	start := 0
	if afterCursor != "" {
		start = sort.Search(len(names), func(index int) bool { return names[index] > string(afterCursor) })
	}
	if start >= len(names) {
		return Page{AtEnd: true}, nil
	}
	end := len(names)
	if scanLimit > 0 && start+scanLimit < end {
		end = start + scanLimit
	}
	for _, name := range names[start:end] {
		page.Walked++
		page.LastCursor = EntryCursor(name)
		entry := PageEntry{EntryCursor: EntryCursor(name)}
		expectedID := strings.TrimSuffix(name, ".json")
		if !strings.HasSuffix(name, ".json") {
			corruption := Corruption{EntryCursor: EntryCursor(name), Code: CorruptionInvalidFilename, Detail: "candidate filename must end in .json"}
			entry.Corruption = &corruption
		} else if err := candidateID(expectedID); err != nil {
			corruption := Corruption{EntryCursor: EntryCursor(name), Code: CorruptionInvalidFilename, Detail: err.Error(), cause: err}
			entry.Corruption = &corruption
		} else if record, err := readRootedRecord(inbox, kind, name, expectedID); err != nil {
			var corruption *Corruption
			if !errors.As(err, &corruption) {
				corruption = corruptionForReadError(name, err)
			}
			copyOfCorruption := *corruption
			entry.Corruption = &copyOfCorruption
		} else {
			copyOfRecord := record
			entry.Record = &copyOfRecord
		}
		page.Entries = append(page.Entries, entry)
	}
	page.AtEnd = end == len(names)
	return page, nil
}

func candidateEntryCursor(cursor EntryCursor) error {
	if cursor == "" {
		return nil
	}
	if cursor == "." || cursor == ".." || strings.ContainsRune(string(cursor), 0) || strings.ContainsRune(string(cursor), '/') {
		return fmt.Errorf("candidate entry cursor must be an opaque filename, got %q", cursor)
	}
	return nil
}

func readRootedRecord(inbox *candidateInbox, kind Kind, entryCursor, expectedID string) (Record, error) {
	record, err := readCustodiedCandidateRecord(inbox.directory, kind, entryCursor, expectedID)
	if err != nil {
		return Record{}, err
	}
	record.homeIdentity = inbox.home.identity
	record.directoryIdentity = inbox.directory.identity
	record.CustodyToken = candidateCustodyToken(kind, record.EntryCursor, record.Digest, record.homeIdentity, record.directoryIdentity, record.fileIdentity)
	if err := record.Validate(); err != nil {
		return Record{}, &Corruption{EntryCursor: EntryCursor(entryCursor), Code: CorruptionRaced, Detail: err.Error(), cause: err}
	}
	return record, nil
}

// readCustodiedCandidateRecord is the directory-scoped core of readRootedRecord:
// the same lstat/open/read race ceremony without the home identity, custody
// token, or canonical-cursor seal. Writers use it to re-validate staged temp
// files and published targets whose entry cursor is not yet (or no longer) the
// canonical <id>.json name.
func readCustodiedCandidateRecord(directory *candidateDirectory, kind Kind, entryCursor, expectedID string) (Record, error) {
	before, err := directory.lstat(entryCursor)
	if err != nil {
		return Record{}, corruptionForReadError(entryCursor, err)
	}
	if before.isSymlink() {
		return Record{}, &Corruption{EntryCursor: EntryCursor(entryCursor), Code: CorruptionSymlink, Detail: before.detail()}
	}
	if !before.isRegular() {
		return Record{}, &Corruption{EntryCursor: EntryCursor(entryCursor), Code: CorruptionNonRegular, Detail: before.detail()}
	}
	file, err := directory.openRead(entryCursor)
	if err != nil {
		return Record{}, corruptionForReadError(entryCursor, err)
	}
	opened, statErr := candidateIdentityForFile(file)
	if statErr != nil {
		_ = file.Close()
		return Record{}, &Corruption{EntryCursor: EntryCursor(entryCursor), Code: CorruptionUnreadable, Detail: statErr.Error(), cause: statErr}
	}
	current, lstatErr := directory.lstat(entryCursor)
	if lstatErr != nil || !opened.isRegular() || !current.isRegular() || !before.same(opened) || !before.same(current) {
		_ = file.Close()
		return Record{}, &Corruption{EntryCursor: EntryCursor(entryCursor), Code: CorruptionRaced, Detail: "entry changed while opening", cause: lstatErr}
	}
	data, readErr := io.ReadAll(file)
	// Stat identity alone cannot distinguish same-size writes in one clock tick.
	// Compare two descriptor-pinned byte snapshots as well as the metadata/path
	// identities, so a content change cannot hide behind identical stat results.
	candidateAfterOpenHook(entryCursor)
	var verified []byte
	var verifyErr error
	if readErr == nil {
		if _, verifyErr = file.Seek(0, io.SeekStart); verifyErr == nil {
			verified, verifyErr = io.ReadAll(file)
		}
	}
	afterRead, afterReadErr := candidateIdentityForFile(file)
	closeErr := file.Close()
	afterName, afterNameErr := directory.lstat(entryCursor)
	if readErr != nil || verifyErr != nil || closeErr != nil {
		joined := errors.Join(readErr, verifyErr, closeErr)
		return Record{}, &Corruption{EntryCursor: EntryCursor(entryCursor), Code: CorruptionUnreadable, Detail: joined.Error(), cause: joined}
	}
	if afterReadErr != nil || afterNameErr != nil || !opened.same(afterRead) || !opened.same(afterName) || !bytes.Equal(data, verified) {
		return Record{}, &Corruption{EntryCursor: EntryCursor(entryCursor), Code: CorruptionRaced, Detail: "entry changed while reading", cause: errors.Join(afterReadErr, afterNameErr)}
	}
	meta, err := decodeCandidateJSONIdentity(data)
	if err != nil {
		code := CorruptionInvalidJSON
		var identityErr *candidateIdentityDecodeError
		if errors.As(err, &identityErr) {
			code = identityErr.code
		}
		return Record{}, &Corruption{EntryCursor: EntryCursor(entryCursor), Code: code, Detail: err.Error(), cause: err}
	}
	if strings.TrimSpace(meta.ID) == "" {
		return Record{}, &Corruption{EntryCursor: EntryCursor(entryCursor), Code: CorruptionBlankID}
	}
	if meta.ID != strings.TrimSpace(meta.ID) {
		return Record{}, &Corruption{EntryCursor: EntryCursor(entryCursor), Code: CorruptionInvalidID, Detail: fmt.Sprintf("body id %q is not canonical", meta.ID)}
	}
	if err := candidateID(meta.ID); err != nil {
		return Record{}, &Corruption{EntryCursor: EntryCursor(entryCursor), Code: CorruptionInvalidID, Detail: err.Error(), cause: err}
	}
	if meta.ID != expectedID {
		return Record{}, &Corruption{EntryCursor: EntryCursor(entryCursor), Code: CorruptionIDMismatch, Detail: fmt.Sprintf("body id %q does not match filename id %q", meta.ID, expectedID)}
	}
	digest := candidateBodyDigest(data)
	return Record{
		Kind:         kind,
		ID:           meta.ID,
		Body:         append([]byte(nil), data...),
		Digest:       digest,
		EntryCursor:  EntryCursor(entryCursor),
		fileIdentity: opened,
	}, nil
}

func corruptionForReadError(entryCursor string, err error) *Corruption {
	code := CorruptionUnreadable
	if errors.Is(err, fs.ErrNotExist) {
		code = CorruptionRaced
	}
	return &Corruption{EntryCursor: EntryCursor(entryCursor), Code: code, Detail: err.Error(), cause: err}
}

func candidateBodyDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// candidateDigestForFile reads the body of an already-open candidate file
// and returns its canonical sha256 digest. authorizeCandidateReplacement
// uses this in place of the previous Fstat-based identity compare so the
// custody guard's signal is content-stable (APFS no-op touches cannot
// change sha256) instead of kernel-stat-volatile. The leaf still refuses
// when the live body's digest differs from the recorded one.
func candidateDigestForFile(file *os.File) (string, error) {
	data, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	return candidateBodyDigest(data), nil
}

// candidateDigestForPath opens name under the directory FD (descriptor-
// rooted, no-follow) and returns its canonical sha256 digest. Used by the
// R3F authorize path because openExistingWritable returns a WRONLY FD
// that cannot be Read from. Reading through a fresh RDONLY FD goes back
// to the same underlying inode immediately under the candidate publication
// lock — the body bytes are stable because nobody else holds the lock.
func candidateDigestForPath(directory *candidateDirectory, name string) (string, error) {
	fd, err := unix.Openat(directory.fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), directory.path(name))
	if file == nil {
		_ = unix.Close(fd)
		return "", fmt.Errorf("open candidate digest descriptor %s", directory.path(name))
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	return candidateBodyDigest(data), nil
}

func candidateCustodyToken(kind Kind, entryCursor EntryCursor, digest string, home, directory, file candidateFileIdentity) string {
	material := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s", kind, entryCursor, digest, home.tokenField(), directory.tokenField(), file.tokenField())
	sum := sha256.Sum256([]byte(material))
	return "sha256:" + hex.EncodeToString(sum[:])
}
