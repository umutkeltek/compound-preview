package work

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/envelope"
)

const packetEvidencePrefix = "packet_sha256:"

type PacketImportOptions struct {
	Owner         string
	Actor         string
	State         string
	Risk          string
	Privacy       string
	ContextPackID string
}

type PacketImportReport struct {
	Created      bool                  `json:"created"`
	PacketTitle  string                `json:"packet_title"`
	PacketStatus string                `json:"packet_status,omitempty"`
	PacketDate   string                `json:"packet_date,omitempty"`
	EvidenceRef  string                `json:"evidence_ref"`
	SourceDigest string                `json:"source_digest"`
	SourceRef    string                `json:"source_ref"`
	Work         envelope.WorkEnvelope `json:"work"`
}

type PacketSourceProof struct {
	PacketTitle      string `json:"packet_title"`
	EvidenceRef      string `json:"evidence_ref"`
	SourceDigest     string `json:"source_digest"`
	SourceRef        string `json:"source_ref"`
	CanonicalSurface string `json:"canonical_surface"`
}

type packetSource struct {
	Title            string
	Date             string
	Status           string
	Owner            string
	ProofMedium      string
	Objective        string
	SuccessPredicate string
	NonGoals         []string
	Digest           string
	EvidenceRef      string
	SourceRef        string
}

func PacketSourceProofForPath(path string) (PacketSourceProof, error) {
	src, err := LoadPacket(path)
	if err != nil {
		return PacketSourceProof{}, err
	}
	return PacketSourceProof{
		PacketTitle:      src.Title,
		EvidenceRef:      src.EvidenceRef,
		SourceDigest:     src.Digest,
		SourceRef:        src.SourceRef,
		CanonicalSurface: "packet:" + slug(src.Title),
	}, nil
}

// ImportPacket turns a markdown execution/goal packet into a WorkEnvelope. The
// packet stays source evidence; the adapter does not mutate the packet, close
// work, promote memory, or claim execution success.
func (s *Store) ImportPacket(path string, opts PacketImportOptions, now time.Time) (PacketImportReport, error) {
	src, err := LoadPacket(path)
	if err != nil {
		return PacketImportReport{}, err
	}
	owner := firstNonEmpty(opts.Owner, src.Owner)
	if owner == "" {
		owner = "packet"
	}
	state := orDefault(opts.State, "proposed")
	workID := idFor("work_", "packet", src.Digest)
	if existing, err := s.Get(workID); err == nil {
		return PacketImportReport{
			Created:      false,
			PacketTitle:  src.Title,
			PacketStatus: src.Status,
			PacketDate:   src.Date,
			EvidenceRef:  src.EvidenceRef,
			SourceDigest: src.Digest,
			SourceRef:    src.SourceRef,
			Work:         existing,
		}, nil
	}
	w := BuildWork(CreateInput{
		Owner:            owner,
		Actor:            opts.Actor,
		SuccessPredicate: src.SuccessPredicate,
		DesiredOutcome:   src.Objective,
		NonGoals:         src.NonGoals,
		EvidenceRefs:     []string{src.EvidenceRef},
		ContextPackID:    opts.ContextPackID,
		State:            state,
		Risk:             opts.Risk,
		Privacy:          opts.Privacy,
	}, now)
	w.ID = workID
	w.CanonicalSurface = "packet:" + slug(src.Title)
	w.ProofMedium = src.ProofMedium
	w.Authority.SourceClass = "source"
	w.Authority.SourceRevision = "sha256:" + src.Digest
	w.Authority.Status = "current"
	w.Uncertainty = envelope.Uncertainty{Level: "inferred", Reason: "parsed from markdown packet; packet is source evidence, not canon"}
	plan, err := s.createPlanForWork(w, now, CreatePlanOptions{Actor: opts.Actor})
	if err != nil {
		return PacketImportReport{}, err
	}
	created, err := s.admitWork(plan, now, "imported packet: "+src.Title, []string{src.EvidenceRef})
	if err != nil {
		return PacketImportReport{}, err
	}
	w = created
	return PacketImportReport{
		Created:      true,
		PacketTitle:  src.Title,
		PacketStatus: src.Status,
		PacketDate:   src.Date,
		EvidenceRef:  src.EvidenceRef,
		SourceDigest: src.Digest,
		SourceRef:    src.SourceRef,
		Work:         w,
	}, nil
}

func LoadPacket(path string) (packetSource, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return packetSource{}, err
	}
	return ParsePacketMarkdown(data, filepath.Base(path))
}

func ParsePacketMarkdown(data []byte, sourceRef string) (packetSource, error) {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	lines := strings.Split(string(data), "\n")
	src := packetSource{
		Digest:      digest,
		EvidenceRef: packetEvidencePrefix + digest[:16],
		SourceRef:   sourceRef,
	}
	meta := map[string]string{}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") && src.Title == "" {
			src.Title = strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			break
		}
		if k, v, ok := splitMeta(trimmed); ok {
			meta[strings.ToLower(k)] = v
		}
	}
	src.Date = meta["date"]
	src.Status = meta["status"]
	src.Owner = stripInlineCode(meta["owner"])
	src.ProofMedium = strings.ToLower(stripInlineCode(meta["proof-medium"]))
	src.Objective = firstNonEmpty(sectionText(lines, "objective"), src.Title)
	src.SuccessPredicate = firstNonEmpty(meta["success"], meta["success predicate"], findSuccessLine(lines))
	if src.SuccessPredicate == "" && src.Title != "" {
		src.SuccessPredicate = "packet reaches a verified state delta: " + src.Title
	}
	src.NonGoals = firstNonEmptyList(sectionBullets(lines, "non-goals"), sectionBullets(lines, "boundaries"), boundaryLines(lines))
	if strings.TrimSpace(src.Title) == "" {
		return packetSource{}, fmt.Errorf("packet title heading required")
	}
	if strings.TrimSpace(src.SuccessPredicate) == "" {
		return packetSource{}, fmt.Errorf("packet success predicate required")
	}
	return src, nil
}

func splitMeta(line string) (string, string, bool) {
	if strings.HasPrefix(line, "#") || line == "" {
		return "", "", false
	}
	before, after, ok := strings.Cut(line, ":")
	if !ok || strings.TrimSpace(before) == "" || strings.TrimSpace(after) == "" {
		return "", "", false
	}
	return strings.TrimSpace(before), strings.TrimSpace(after), true
}

func sectionText(lines []string, heading string) string {
	var out []string
	inSection := false
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if strings.HasPrefix(strings.ToLower(trimmed), "## ") {
			name := strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			if strings.EqualFold(name, heading) {
				inSection = true
				continue
			}
			if inSection {
				break
			}
		}
		if !inSection || inFence || trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		out = append(out, strings.TrimPrefix(trimmed, "- "))
		if len(strings.Join(out, " ")) > 700 {
			break
		}
	}
	return strings.TrimSpace(strings.Join(out, " "))
}

func sectionBullets(lines []string, heading string) []string {
	var out []string
	inSection := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trimmed), "## ") {
			name := strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			if strings.EqualFold(name, heading) {
				inSection = true
				continue
			}
			if inSection {
				break
			}
		}
		if inSection && strings.HasPrefix(trimmed, "- ") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
		}
	}
	return limitStrings(out, 12)
}

func boundaryLines(lines []string) []string {
	var out []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trimmed), "boundary:") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(trimmed, "Boundary:")))
		}
	}
	return limitStrings(out, 12)
}

func findSuccessLine(lines []string) string {
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "success:") || strings.HasPrefix(lower, "success predicate:") {
			_, after, _ := strings.Cut(trimmed, ":")
			return strings.TrimSpace(after)
		}
		if strings.Contains(lower, "success is ") {
			return strings.TrimSpace(trimmed)
		}
	}
	return ""
}

func stripInlineCode(s string) string {
	return strings.Trim(strings.TrimSpace(s), "`")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonEmptyList(values ...[]string) []string {
	for _, value := range values {
		if len(value) > 0 {
			return value
		}
	}
	return []string{}
}

func limitStrings(values []string, max int) []string {
	if len(values) <= max {
		return values
	}
	return values[:max]
}

func slug(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
