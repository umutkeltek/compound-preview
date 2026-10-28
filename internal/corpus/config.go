package corpus

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"compound/internal/sourceobs"
)

const (
	IdentitySchemaVersion = "compound.corpus.identity.v0.1"
	ManifestSchemaVersion = "compound.corpus.manifest.v0.1"
	ProgressSchemaVersion = "compound.corpus.progress.v0.1"
	PrefixSchemaVersion   = "v1"
	defaultChunkUnit      = "runes"
	defaultChunkSize      = 4000
	defaultChunkOverlap   = 400
	defaultChunkBoundary  = "structure"
	defaultEmbedBits      = "f32"
)

var ErrEmbedderMismatch = errors.New("corpus embedder identity mismatch")

type LaneConfig struct {
	Lane         string         `json:"lane" yaml:"lane"`
	SourceFamily string         `json:"source_family" yaml:"source_family"`
	Generation   string         `json:"generation,omitempty" yaml:"generation,omitempty"`
	Privacy      string         `json:"privacy" yaml:"privacy"`
	Embedder     EmbedderConfig `json:"embedder" yaml:"embedder"`
	Chunk        ChunkConfig    `json:"chunk" yaml:"chunk"`

	// SourceInPlacePrivacyGate is an explicit opt-in that tightens the
	// corpus resolver's body_policy=source_in_place seam in step with the
	// owned_snapshot gate. Default false preserves the documented legacy
	// behavior (BuildSourceManifest defaults source_in_place items to
	// privacy=private while published quickstarts run public lanes — that
	// contract is older than this gate). When true, a privacy=private
	// source_in_place item is refused before any body read or embedder
	// call unless the lane posture is private + local/loopback embedder,
	// mirroring the owned_snapshot predicate. Public items and
	// non-private lanes remain unaffected. There is no caller-side
	// override: closing the seam is one shape — refuse, never
	// widen.
	SourceInPlacePrivacyGate bool `json:"source_in_place_privacy_gate,omitempty" yaml:"source_in_place_privacy_gate,omitempty"`
}

type EmbedderConfig struct {
	Provider string `json:"provider" yaml:"provider"`
	Model    string `json:"model" yaml:"model"`
	Dims     int    `json:"dims" yaml:"dims"`
	Bits     string `json:"bits" yaml:"bits"`
	BaseURL  string `json:"base_url,omitempty" yaml:"base_url,omitempty"`
}

func (e EmbedderConfig) Identity() string {
	bits := strings.TrimSpace(e.Bits)
	if bits == "" {
		bits = defaultEmbedBits
	}
	return fmt.Sprintf("%s:%s:%d:%s", strings.TrimSpace(e.Provider), strings.TrimSpace(e.Model), e.Dims, bits)
}

type ChunkConfig struct {
	Unit     string `json:"unit" yaml:"unit"`
	Size     int    `json:"size" yaml:"size"`
	Overlap  int    `json:"overlap" yaml:"overlap"`
	Boundary string `json:"boundary" yaml:"boundary"`
}

func (c ChunkConfig) withDefaults() (ChunkConfig, error) {
	if strings.TrimSpace(c.Unit) == "" {
		c.Unit = defaultChunkUnit
	}
	sizeDefaulted := false
	if c.Size == 0 {
		c.Size = defaultChunkSize
		sizeDefaulted = true
	}
	if c.Overlap == 0 && sizeDefaulted {
		c.Overlap = defaultChunkOverlap
	}
	if strings.TrimSpace(c.Boundary) == "" {
		c.Boundary = defaultChunkBoundary
	}
	c.Unit = strings.ToLower(strings.TrimSpace(c.Unit))
	c.Boundary = strings.ToLower(strings.TrimSpace(c.Boundary))
	if c.Unit != "runes" {
		return ChunkConfig{}, fmt.Errorf("chunk unit must be runes, got %q", c.Unit)
	}
	if c.Boundary != "structure" {
		return ChunkConfig{}, fmt.Errorf("chunk boundary must be structure, got %q", c.Boundary)
	}
	if c.Size <= 0 {
		return ChunkConfig{}, fmt.Errorf("chunk size must be positive")
	}
	if c.Overlap < 0 {
		return ChunkConfig{}, fmt.Errorf("chunk overlap must be non-negative")
	}
	if c.Overlap >= c.Size {
		return ChunkConfig{}, fmt.Errorf("chunk overlap must be smaller than size")
	}
	return c, nil
}

func ValidateLaneConfig(lane LaneConfig) (LaneConfig, error) {
	lane.Lane = strings.TrimSpace(lane.Lane)
	lane.SourceFamily = strings.TrimSpace(lane.SourceFamily)
	lane.Generation = strings.TrimSpace(lane.Generation)
	lane.Privacy = strings.ToLower(strings.TrimSpace(lane.Privacy))
	lane.Embedder.Provider = strings.TrimSpace(lane.Embedder.Provider)
	lane.Embedder.Model = strings.TrimSpace(lane.Embedder.Model)
	lane.Embedder.Bits = strings.TrimSpace(lane.Embedder.Bits)
	lane.Embedder.BaseURL = strings.TrimSpace(lane.Embedder.BaseURL)
	if lane.Embedder.Bits == "" {
		lane.Embedder.Bits = defaultEmbedBits
	}
	var missing []string
	if lane.Lane == "" {
		missing = append(missing, "lane")
	}
	if lane.SourceFamily == "" {
		missing = append(missing, "source_family")
	}
	if lane.Privacy == "" {
		missing = append(missing, "privacy")
	}
	if lane.Embedder.Provider == "" {
		missing = append(missing, "embedder.provider")
	}
	if lane.Embedder.Model == "" {
		missing = append(missing, "embedder.model")
	}
	if lane.Embedder.Dims <= 0 {
		missing = append(missing, "embedder.dims")
	}
	if lane.Embedder.Bits == "" {
		missing = append(missing, "embedder.bits")
	}
	if len(missing) > 0 {
		return LaneConfig{}, fmt.Errorf("corpus lane %q missing %s", lane.Lane, strings.Join(missing, ", "))
	}
	chunk, err := lane.Chunk.withDefaults()
	if err != nil {
		return LaneConfig{}, fmt.Errorf("corpus lane %q: %w", lane.Lane, err)
	}
	lane.Chunk = chunk
	if lane.Privacy == "private" && !ProviderIsLocal(lane.Embedder.Provider, lane.Embedder.BaseURL) {
		return LaneConfig{}, fmt.Errorf("corpus lane %q privacy=private refuses non-local embedder %s", lane.Lane, lane.Embedder.Identity())
	}
	return lane, nil
}

// SourceInPlacePrivacyGateEnabled reports whether the optional
// body_policy=source_in_place privacy gate is engaged for a lane. The
// check exists so callers (and tests) can phrase intent without poking
// the struct field directly — keeps the off-by-default contract visible
// at every seam.
func SourceInPlacePrivacyGateEnabled(lane LaneConfig) bool {
	return lane.SourceInPlacePrivacyGate
}

// validateCurrentItemAccess is the single item-level privacy decision used by
// indexing, querying an existing index, and body resolution. owned_snapshot is
// always governed. source_in_place is governed only after the lane's explicit
// compatibility gate is enabled; omission/false preserves legacy behavior.
// Governed malformed labels fail closed, and private bodies require the same
// private + local/loopback lane posture at every access point.
func validateCurrentItemAccess(lane LaneConfig, item sourceobs.SourceManifestItem) error {
	policy := strings.ToLower(strings.TrimSpace(item.BodyPolicy))
	governed := policy == "owned_snapshot" || (policy == "source_in_place" && SourceInPlacePrivacyGateEnabled(lane))
	if !governed {
		return nil
	}

	switch strings.ToLower(strings.TrimSpace(item.Privacy)) {
	case "public":
		return nil
	case "private":
		if laneIsPrivateLocal(lane) {
			return nil
		}
		return fmt.Errorf("%s item %q privacy=private refuses non-private lane (lane privacy=%q embedder=%s)", policy, item.ID, strings.TrimSpace(lane.Privacy), lane.Embedder.Identity())
	default:
		return fmt.Errorf("%s item %q has malformed privacy field (must be public or private, got %q)", policy, item.ID, strings.TrimSpace(item.Privacy))
	}
}

// itemAccessIsGoverned reports whether absence from the current source
// manifest must itself fail closed. It uses the indexed item only to identify
// a seam whose current privacy must remain verifiable; the access decision is
// always made from the current item when one exists.
func itemAccessIsGoverned(lane LaneConfig, item sourceobs.SourceManifestItem) bool {
	policy := strings.ToLower(strings.TrimSpace(item.BodyPolicy))
	return policy == "owned_snapshot" || (policy == "source_in_place" && SourceInPlacePrivacyGateEnabled(lane))
}

func ProviderIsLocal(provider, baseURL string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return provider == "ollama" || provider == "local"
	}
	parsed := baseURL
	if !strings.Contains(parsed, "://") {
		parsed = "http://" + parsed
	}
	u, err := url.Parse(parsed)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return false
	}
	if host == "localhost" || host == "0.0.0.0" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func IndexDir(homePath string, lane LaneConfig) string {
	return filepath.Join(homePath, "indexes", "corpus-"+safePathComponent(lane.Lane), SanitizedEmbedderIdentity(lane.Embedder.Identity()))
}

func SanitizedEmbedderIdentity(identity string) string {
	var b strings.Builder
	for _, r := range identity {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := strings.Trim(b.String(), "_")
	if name == "" {
		name = "unknown"
	}
	sum := sha256.Sum256([]byte(identity))
	return name + "-" + hex.EncodeToString(sum[:])[:12]
}

func safePathComponent(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '.', r == '_', r == '-':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	return out
}

func parseIdentityDim(identity string) int {
	parts := strings.Split(strings.TrimSpace(identity), ":")
	if len(parts) < 4 {
		return 0
	}
	n, _ := strconv.Atoi(parts[len(parts)-2])
	return n
}
