package routeevidence

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
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	homepkg "compound/internal/home"
	codec "compound/internal/routeevidence/codec"
)

const (
	SchemaVersion                = codec.SchemaVersion
	executionRouteEvidenceLedger = "execution-route-evidence.jsonl"
)

const (
	RecorderGeneric             = codec.RecorderGeneric
	RecorderExternalImport      = codec.RecorderExternalImport
	RecorderTrustedPanelRuntime = codec.RecorderTrustedPanelRuntime
)

var writeRouteEvidenceFile = func(file *os.File, data []byte) (int, error) { return file.Write(data) }
var syncRouteEvidenceFile = func(file *os.File) error { return file.Sync() }
var closeRouteEvidenceFile = func(file *os.File) error { return file.Close() }

// routeEvidenceBeforeLedgerReplace is a deterministic test seam immediately
// before atomic CAS publication of the prepared postimage.
var routeEvidenceBeforeLedgerReplace = func() {}

type Usage = codec.Usage
type Target = codec.Target
type RejectedTarget = codec.RejectedTarget
type Evidence = codec.Evidence

type BuildInput struct {
	CreatedAt        time.Time
	RequestID        string
	WorkID           string
	ClientProfile    string
	Protocol         string
	Provider         string
	Model            string
	ResolvedProvider string
	ResolvedModel    string
	ResolutionSource string
	GatewayRequestID string
	Usage            Usage
	CostMicros       int64
	LatencyMS        int64
	Status           string
	EvidenceRefs     []string
}

type ImportResult struct {
	Evidence   Evidence            `json:"evidence"`
	Transition envelope.Transition `json:"transition"`
}

func Build(in BuildInput) (Evidence, error) {
	now := in.CreatedAt
	if now.IsZero() {
		now = time.Now()
	}
	status := strings.TrimSpace(in.Status)
	if status == "" {
		status = "completed"
	}
	ev := Evidence{
		SchemaVersion: SchemaVersion,
		CreatedAt:     rfc3339(now),
		RequestID:     strings.TrimSpace(in.RequestID),
		WorkID:        strings.TrimSpace(in.WorkID),
		ClientProfile: strings.TrimSpace(in.ClientProfile),
		Protocol:      strings.TrimSpace(in.Protocol),
		SelectedTarget: Target{
			Provider: strings.TrimSpace(in.Provider),
			Model:    strings.TrimSpace(in.Model),
			Protocol: strings.TrimSpace(in.Protocol),
		},
		ResolvedProvider: strings.TrimSpace(in.ResolvedProvider),
		ResolvedModel:    strings.TrimSpace(in.ResolvedModel),
		ResolutionSource: strings.TrimSpace(in.ResolutionSource),
		GatewayRequestID: strings.TrimSpace(in.GatewayRequestID),
		Usage:            in.Usage,
		CostMicros:       in.CostMicros,
		LatencyMS:        in.LatencyMS,
		Status:           status,
		EvidenceRefs:     dedupeRefs(in.EvidenceRefs),
		RecordedVia:      RecorderGeneric,
	}
	ev.ID = idFor("route_", ev.RequestID, ev.CreatedAt, ev.SelectedTarget.Provider, ev.SelectedTarget.Model, ev.Status)
	if err := Validate(ev); err != nil {
		return Evidence{}, err
	}
	return ev, nil
}

func Decode(data []byte) (Evidence, error) {
	return codec.Decode(data)
}

// LookupRequestID returns the first row with request_id, backed by the
// incremental request-id index (request_id_index.go) over the
// execution-route-evidence JSONL ledger: O(bytes appended since the last
// lookup) amortized instead of a full-ledger linear scan per call.
//
// Deliberate strictness change from the prior full scan: a malformed line
// ANYWHERE in the ledger now errors, not only a malformed line before the
// first match (today's full scan errored only if the bad line preceded the
// match). The ledger is a governed, append-only artifact where a malformed
// line is already fatal to every writer, and there are no production callers
// of LookupRequestID/HasRequestID outside this package (tests only).
func LookupRequestID(homePath, requestID string) (Evidence, bool, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return Evidence{}, false, nil
	}
	path := filepath.Join(homePath, "ledger", executionRouteEvidenceLedger)
	refs, missing, err := requestIDRefs(path, requestID)
	if err != nil {
		return Evidence{}, false, err
	}
	if missing || len(refs) == 0 {
		return Evidence{}, false, nil
	}
	ev, err := readLineRef(path, refs[0])
	if err != nil {
		return Evidence{}, false, err
	}
	if strings.TrimSpace(ev.RecordedVia) == "" {
		ev.RecordedVia = RecorderGeneric
	}
	return ev, true, nil
}

func HasRequestID(homePath, requestID string) (bool, error) {
	_, ok, err := LookupRequestID(homePath, requestID)
	return ok, err
}

func ImportFile(homePath, path string) (ImportResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ImportResult{}, fmt.Errorf("read execution route evidence %s: %w", path, err)
	}
	ev, err := Decode(data)
	if err != nil {
		return ImportResult{}, err
	}
	return recordJoinedVia(homePath, ev, RecorderExternalImport)
}

func RecordJoined(homePath string, ev Evidence) (ImportResult, error) {
	return recordJoinedVia(homePath, ev, RecorderGeneric)
}

// RecordJoinedUniqueRequestID names the strict writer contract used by callers
// whose provider-call identity depends on request_id uniqueness. All canonical
// recorders share the same atomic uniqueness gate so a generic writer cannot
// race around this entry point.
func RecordJoinedUniqueRequestID(homePath string, ev Evidence) (ImportResult, error) {
	return recordJoinedVia(homePath, ev, RecorderGeneric)
}

// RecordClaimedJoined records evidence only for the exact durable request-id
// claim previously acquired by the caller.
func RecordClaimedJoined(homePath string, ev Evidence, claim RequestClaim) (ImportResult, error) {
	return recordJoinedViaClaim(homePath, ev, RecorderGeneric, &claim)
}

func RecordPanelRuntimeJoined(homePath string, ev Evidence) (ImportResult, error) {
	return recordJoinedVia(homePath, ev, RecorderTrustedPanelRuntime)
}

func recordJoinedVia(homePath string, ev Evidence, recorder string) (ImportResult, error) {
	return recordJoinedViaClaim(homePath, ev, recorder, nil)
}

func recordJoinedViaClaim(homePath string, ev Evidence, recorder string, claim *RequestClaim) (ImportResult, error) {
	ev.RecordedVia = normalizedRecorder(recorder)
	ev, tr, err := JoinTransition(ev)
	if err != nil {
		return ImportResult{}, err
	}
	recorded, err := recordEvidenceUniqueRequestID(homePath, ev, claim)
	if err != nil {
		return ImportResult{}, err
	}
	if err := RecordTransition(homePath, tr); err != nil {
		return ImportResult{}, err
	}
	return ImportResult{Evidence: recorded, Transition: tr}, nil
}

func JoinTransition(ev Evidence) (Evidence, envelope.Transition, error) {
	if err := Validate(ev); err != nil {
		return Evidence{}, envelope.Transition{}, err
	}
	tr, err := BuildTransition(ev)
	if err != nil {
		return Evidence{}, envelope.Transition{}, err
	}
	if ev.TransitionID != "" && ev.TransitionID != tr.ID {
		return Evidence{}, envelope.Transition{}, fmt.Errorf("transition_id=%q does not match computed %q", ev.TransitionID, tr.ID)
	}
	ev.TransitionID = tr.ID
	return ev, tr, nil
}

func BuildTransition(ev Evidence) (envelope.Transition, error) {
	if err := Validate(ev); err != nil {
		return envelope.Transition{}, err
	}
	workID := ev.WorkID
	if workID == "" {
		workID = idFor("work_route_", ev.ID)
	}
	evidenceRefs := dedupeRefs(append([]string{ev.ID}, ev.EvidenceRefs...))
	costRefs := []string{}
	if ev.CostMicros > 0 || ev.Usage.TotalTokens > 0 {
		costRefs = []string{ev.ID}
	}
	tr := envelope.Transition{
		Header: envelope.Header{
			SchemaVersion: envelope.SVTransition,
			ID:            idFor("tr_", ev.ID, ev.RequestID, ev.CreatedAt),
			CreatedAt:     ev.CreatedAt,
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  evidenceRefs,
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		SourceTraceID: ev.RequestID,
		BeforeRefs:    append([]string{}, ev.EvidenceRefs...),
		ActionRefs:    []string{ev.RequestID},
		AfterRefs:     []string{ev.ID},
		StateDelta:    transitionDelta(ev),
		Projections: envelope.Projections{
			Memory:         []string{},
			Skill:          []string{},
			Product:        []string{},
			Cost:           costRefs,
			Risk:           []string{},
			Taste:          []string{},
			Routing:        []string{ev.ID},
			HumanAttention: []string{},
		},
		SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: false},
	}
	if err := tr.Validate(); err != nil {
		return envelope.Transition{}, err
	}
	return tr, nil
}

func RecordEvidence(homePath string, ev Evidence) error {
	ev.RecordedVia = RecorderGeneric
	_, err := recordEvidenceUniqueRequestID(homePath, ev, nil)
	return err
}

func recordEvidenceUniqueRequestID(homePath string, ev Evidence, claim *RequestClaim) (Evidence, error) {
	if err := Validate(ev); err != nil {
		return Evidence{}, err
	}
	var recorded Evidence
	err := homepkg.WithAutonomyAdmissionLock(homePath, func() error {
		claimed, err := authorizeRequestClaimLocked(homePath, ev, claim)
		if err != nil {
			return err
		}
		existing, ok, err := uniqueEvidenceForRequestID(homePath, ev.RequestID)
		if err != nil {
			return err
		}
		if ok {
			existing.RecordedVia = normalizedRecorder(existing.RecordedVia)
			existingJSON, err := json.Marshal(existing)
			if err != nil {
				return err
			}
			incomingJSON, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			if !bytes.Equal(existingJSON, incomingJSON) {
				return fmt.Errorf("execution route evidence request_id %q already recorded as %s", ev.RequestID, existing.ID)
			}
			if claimed {
				if err := releaseRequestClaimLocked(homePath, *claim); err != nil {
					return err
				}
			}
			recorded = existing
			return nil
		}
		if err := recordEvidenceLocked(homePath, ev); err != nil {
			return err
		}
		if claimed {
			if err := releaseRequestClaimLocked(homePath, *claim); err != nil {
				return err
			}
		}
		recorded = ev
		return nil
	})
	return recorded, err
}

// uniqueEvidenceForRequestID resolves the exactly-one row for requestID via
// the incremental request-id index (request_id_index.go): refresh the index,
// then 0 refs -> not found, >1 refs -> today's exact ambiguity error, 1 ref ->
// seek+read+unmarshal that one row from the ledger file (never cached).
func uniqueEvidenceForRequestID(homePath, requestID string) (Evidence, bool, error) {
	path := filepath.Join(homePath, "ledger", executionRouteEvidenceLedger)
	refs, missing, err := requestIDRefs(path, requestID)
	if err != nil {
		return Evidence{}, false, err
	}
	if missing || len(refs) == 0 {
		return Evidence{}, false, nil
	}
	if len(refs) > 1 {
		return Evidence{}, false, fmt.Errorf("execution route evidence request_id %q is ambiguous across %d rows", requestID, len(refs))
	}
	ev, err := readLineRef(path, refs[0])
	if err != nil {
		return Evidence{}, false, err
	}
	return ev, true, nil
}

func recordEvidenceLocked(homePath string, ev Evidence) error {
	root, err := openRouteEvidenceLedgerRoot(homePath)
	if err != nil {
		return err
	}
	defer root.Close()
	current, currentInfo, err := readRouteEvidenceLedgerSnapshot(root, executionRouteEvidenceLedger)
	if err != nil {
		return err
	}
	b, err := json.Marshal(&ev)
	if err != nil {
		return err
	}
	after := append(append([]byte(nil), current...), b...)
	after = append(after, '\n')

	digest := sha256.Sum256(after)
	preimageDigest := sha256.Sum256(current)
	tempName := fmt.Sprintf(
		".%s.%d.%s.%s.swap",
		executionRouteEvidenceLedger,
		os.Getpid(),
		hex.EncodeToString(preimageDigest[:])[:12],
		hex.EncodeToString(digest[:])[:12],
	)
	file, err := root.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cleanup := func(cause error) error {
		_ = file.Close()
		_ = root.Remove(tempName)
		return cause
	}
	if len(current) > 0 {
		if err := writeRouteEvidenceChunk(file, current); err != nil {
			return cleanup(err)
		}
	}
	if err := writeRouteEvidenceChunk(file, append(b, '\n')); err != nil {
		return cleanup(err)
	}
	if err := syncRouteEvidenceFile(file); err != nil {
		return cleanup(err)
	}
	if err := closeRouteEvidenceFile(file); err != nil {
		return cleanup(err)
	}
	tempData, tempInfo, err := readRouteEvidenceLedgerFile(root, tempName)
	if err != nil {
		return cleanup(err)
	}
	if !bytes.Equal(tempData, after) {
		return cleanup(fmt.Errorf("execution route evidence temporary publication changed before commit"))
	}
	routeEvidenceBeforeLedgerReplace()
	publication, err := homepkg.BeginRootedFileCAS(
		root,
		executionRouteEvidenceLedger,
		tempName,
		current,
		currentInfo,
		after,
		tempInfo,
	)
	if err != nil {
		if publication == nil || publication.State() == homepkg.RootedFileCASNotPublished {
			_ = root.Remove(tempName)
		}
		return err
	}
	published, publishedInfo, err := readRouteEvidenceLedgerFile(root, executionRouteEvidenceLedger)
	if err != nil {
		return errors.Join(err, publication.Rollback())
	}
	if !os.SameFile(tempInfo, publishedInfo) || !bytes.Equal(published, after) {
		return errors.Join(
			fmt.Errorf("execution route evidence ledger does not match committed postimage"),
			publication.Rollback(),
		)
	}
	return publication.Commit()
}

func openRouteEvidenceLedgerRoot(homePath string) (*os.Root, error) {
	homeRoot, err := os.OpenRoot(homePath)
	if err != nil {
		return nil, err
	}
	defer homeRoot.Close()
	if err := homeRoot.MkdirAll("ledger", 0o755); err != nil {
		return nil, err
	}
	ledgerInfo, err := homeRoot.Lstat("ledger")
	if err != nil {
		return nil, err
	}
	if ledgerInfo.Mode()&os.ModeSymlink != 0 || !ledgerInfo.IsDir() {
		return nil, fmt.Errorf("execution route evidence ledger path must be a directory beneath compound home")
	}
	root, err := homeRoot.OpenRoot("ledger")
	if err != nil {
		return nil, err
	}
	pinned, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	finalInfo, err := homeRoot.Lstat("ledger")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if finalInfo.Mode()&os.ModeSymlink != 0 || !finalInfo.IsDir() ||
		!os.SameFile(ledgerInfo, finalInfo) || !os.SameFile(finalInfo, pinned) {
		_ = root.Close()
		return nil, fmt.Errorf("execution route evidence ledger path must remain a stable directory beneath compound home")
	}
	return root, nil
}

func readRouteEvidenceLedgerSnapshot(root *os.Root, name string) ([]byte, os.FileInfo, error) {
	data, info, err := readRouteEvidenceLedgerFile(root, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	return data, info, err
}

func readRouteEvidenceLedgerFile(root *os.Root, name string) ([]byte, os.FileInfo, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("execution route evidence ledger must be a regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !opened.Mode().IsRegular() || !sameRouteEvidenceLedgerVersion(before, opened) {
		return nil, nil, fmt.Errorf("execution route evidence ledger changed while it was opened")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, err
	}
	openedAfter, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	finalInfo, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !finalInfo.Mode().IsRegular() || !sameRouteEvidenceLedgerVersion(opened, openedAfter) ||
		!sameRouteEvidenceLedgerVersion(openedAfter, finalInfo) || int64(len(data)) != finalInfo.Size() {
		return nil, nil, fmt.Errorf("execution route evidence ledger changed while it was read")
	}
	return data, finalInfo, nil
}

func sameRouteEvidenceLedgerVersion(left, right os.FileInfo) bool {
	return os.SameFile(left, right) && left.Size() == right.Size() && left.ModTime().Equal(right.ModTime())
}

func writeRouteEvidenceChunk(file *os.File, data []byte) error {
	n, err := writeRouteEvidenceFile(file, data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func RecordTransition(homePath string, tr envelope.Transition) error {
	if err := tr.Validate(); err != nil {
		return err
	}
	store, err := eventstore.Open(homePath)
	if err != nil {
		return err
	}
	_, err = store.PutJSON(tr.ID, &tr, "  ")
	return err
}

func Validate(ev Evidence) error {
	return codec.Validate(ev)
}

func transitionDelta(ev Evidence) string {
	parts := []string{
		"route evidence recorded",
		ev.SelectedTarget.Provider + "/" + ev.SelectedTarget.Model,
		"via " + ev.Protocol,
		"status " + ev.Status,
	}
	if ev.Usage.TotalTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d tokens", ev.Usage.TotalTokens))
	}
	if ev.LatencyMS > 0 {
		parts = append(parts, fmt.Sprintf("%dms", ev.LatencyMS))
	}
	parts = append(parts, "not task success")
	return strings.Join(parts, "; ")
}

func dedupeRefs(refs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

func idFor(prefix string, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(h[:])[:12]
}

func rfc3339(now time.Time) string { return now.UTC().Format(time.RFC3339) }

func normalizedRecorder(value string) string {
	switch strings.TrimSpace(value) {
	case RecorderExternalImport:
		return RecorderExternalImport
	case RecorderTrustedPanelRuntime:
		return RecorderTrustedPanelRuntime
	default:
		return RecorderGeneric
	}
}
