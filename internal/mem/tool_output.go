package mem

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/home"
	"compound/internal/policy"
)

// toolOutputDir is the legacy rebuildable location for externalized tool-output bodies.
// New captures use private artifact custody; ReadToolOutput keeps this as a backward-compatible
// fallback for old observed/tool-output blobs.
const toolOutputDir = "observed/tool-output"

// toolOutputSnippetHeadBytes bounds the head snippet whose hash grounds the minted candidate —
// evidence of the body's head, never the raw body.
const toolOutputSnippetHeadBytes = 512

// ToolOutputCID is the content id for a tool-output body: "sha256:"+hex(sha256(body)). Same body
// always yields the same cid (the capture is idempotent on it).
func ToolOutputCID(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ToolOutputPrivateRef is the opaque non-path reference emitted by hooks for new private custody.
func ToolOutputPrivateRef(cid string) string { return "tool-output-private:" + cid }

type ToolOutputCapture struct {
	CID     string
	Pointer string
	Ref     string
}

// CaptureToolOutput externalizes a LARGE tool-output body (captured at the PostToolUse seam) into
// private artifact custody. When adjudication policy would park or propose the pointer it also mints
// exactly ONE observed-class memory candidate carrying only a compact pointer; when policy files it
// terminal (the usual case: a bare content id) no candidate file is written. The body never becomes a Claim, so it can never enter ambient recall or resolve as
// an index_hit; it is retrievable only via ReadToolOutput / `compound memory show-output`.
//
// It is idempotent: cid is content-addressed on the body (same body → same cid), and the candidate id
// is content-addressed on the cid, so re-capturing an identical body SKIPS the candidate if one
// already exists — never resetting an in-flight adjudication verdict.
func (s *Store) CaptureToolOutput(tool, workID, body string, now time.Time) (cid, pointer string, err error) {
	capture, err := s.CaptureToolOutputDetailed(tool, workID, body, now)
	if err != nil {
		return "", "", err
	}
	return capture.CID, capture.Pointer, nil
}

func (s *Store) CaptureToolOutputDetailed(tool, workID, body string, now time.Time) (ToolOutputCapture, error) {
	raw := []byte(body)
	cid := ToolOutputCID(raw)
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		return ToolOutputCapture{}, err
	}
	artifactRef, err := artifacts.PutPrivate(raw)
	if err != nil {
		return ToolOutputCapture{}, err
	}
	if artifactRef != toolOutputArtifactRef(cid) {
		return ToolOutputCapture{}, fmt.Errorf("tool output private ref %s does not match cid %s", artifactRef, cid)
	}
	pointer := fmt.Sprintf("tool-output:%s tool=%s bytes=%d custody=private", cid, tool, len(raw))

	id := idFor("cand_", "tool-output", cid)

	head := raw
	if len(head) > toolOutputSnippetHeadBytes {
		head = head[:toolOutputSnippetHeadBytes]
	}
	c := Candidate{
		ID:             id,
		Entity:         tool,
		Predicate:      "produced_tool_output",
		Object:         cid,
		Derivation:     DerivationObserved,
		ProvenanceLane: tool,
		SourceTraceRef: "tool-output:" + cid,
		Qualifiers: map[string]string{
			"custody": "private_artifact",
		},
		PromotionBasis: "", // no basis — an observed tool-output pointer can never auto-promote
		State:          "proposed",
		SnippetHash:    shortHash(string(head)),
		CreatedAt:      rfc3339(now),
	}
	if strings.TrimSpace(workID) != "" {
		c.EvidenceRefs = []string{"work:" + workID}
	}
	// Ask the adjudication policy (single owner) what this pointer is. A pointer the policy files
	// terminal (auto_file → no_promote) is a dead row by construction: it can never be corroborated
	// or promoted, and the private artifact already holds the body. Writing it only fills the inbox
	// (≈3,000 files/day by 2026-10-21), so no file is written at all. Any other disposition (park,
	// operator_required) still writes the candidate exactly as before.
	c, decision := AdmitCandidateAt(c, now)
	if decision.Disposition == policy.DispositionAutoFile {
		return ToolOutputCapture{CID: cid, Pointer: pointer, Ref: ToolOutputPrivateRef(cid)}, nil
	}
	var existing Candidate
	if rerr := candidates.ReadCandidate(s.home, candidates.Memory, id, &existing); rerr == nil {
		return ToolOutputCapture{CID: cid, Pointer: pointer, Ref: ToolOutputPrivateRef(cid)}, nil // skip-if-exists: never reset an adjudication verdict
	} else if !os.IsNotExist(rerr) {
		return ToolOutputCapture{}, rerr
	}

	if err = candidates.WriteCandidate(s.home, candidates.Memory, id, &c); err != nil {
		return ToolOutputCapture{}, err
	}
	return ToolOutputCapture{CID: cid, Pointer: pointer, Ref: ToolOutputPrivateRef(cid)}, nil
}

// ReadToolOutput returns the exact original bytes for a captured tool-output cid. A missing cid is an
// EXPLICIT error (never a silent nil): the content-addressed body either rebuilds byte-identically or
// the caller is told it is gone. The cid is validated to the strict "sha256:<64 hex>" shape before any
// read, so a path-shaped value can never escape the blob dir.
func (s *Store) ReadToolOutput(cid string) ([]byte, error) {
	if err := validSHA256CID(cid); err != nil {
		return nil, err
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		return nil, err
	}
	if b, err := artifacts.GetPrivate(toolOutputArtifactRef(cid)); err == nil {
		return b, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	b, err := os.ReadFile(filepath.Join(s.home, toolOutputDir, cid))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("tool output %s not found (private artifact and legacy rebuildable blob missing)", cid)
		}
		return nil, err
	}
	if ToolOutputCID(b) != cid {
		return nil, fmt.Errorf("tool output %s content hash mismatch", cid)
	}
	return b, nil
}

func toolOutputArtifactRef(cid string) string {
	return home.ArtifactRefPrefix + strings.TrimPrefix(cid, "sha256:")
}

// validSHA256CID enforces the exact content-id shape ("sha256:" + 64 lowercase hex chars) shared by
// every content-addressed blob door in this package (tool-output, capture, …). It is both a
// correctness guard and a path-traversal guard: the cid keys a filename under a blob dir, so a
// crafted value ("../…", a separator) must never reach os.ReadFile.
func validSHA256CID(cid string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(cid, prefix) {
		return fmt.Errorf("content id must be %q-prefixed, got %q", prefix, cid)
	}
	hexPart := cid[len(prefix):]
	if len(hexPart) != 64 {
		return fmt.Errorf("content id must be sha256:<64 hex>, got %q", cid)
	}
	for _, r := range hexPart {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return fmt.Errorf("content id has a non-hex character: %q", cid)
		}
	}
	return nil
}
