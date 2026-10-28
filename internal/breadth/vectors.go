package breadth

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

const (
	vectorSchemaVersion = "compound.breadth.index.v1"
	wrapTemplate        = "{indexText}"
)

var (
	vectorMagic               = [8]byte{'C', 'B', 'R', 'V', 'E', 'C', '1', '\n'}
	errVectorCacheMiss        = errors.New("vector cache missing")
	errVectorCacheCorrupt     = errors.New("vector cache corrupt")
	errVectorDimMismatch      = errors.New("vector dim mismatch")
	errVectorEmbedderMismatch = errors.New("vector embedder mismatch")
	errVectorTemplateMismatch = errors.New("vector wrap template mismatch")
)

type embedderStamp struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Dim      int    `json:"dim"`
	Bits     string `json:"bits"`
}

func (s embedderStamp) Identity() string {
	return fmt.Sprintf("%s:%s:%d:%s", s.Provider, s.Model, s.Dim, s.Bits)
}

func parseEmbedderIdentity(identity string) (embedderStamp, error) {
	parts := strings.Split(strings.TrimSpace(identity), ":")
	if len(parts) < 4 {
		return embedderStamp{}, fmt.Errorf("invalid embedder identity %q", identity)
	}
	dim, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil || dim < 0 {
		return embedderStamp{}, fmt.Errorf("invalid embedder dim in %q", identity)
	}
	return embedderStamp{
		Provider: strings.TrimSpace(parts[0]),
		Model:    strings.TrimSpace(strings.Join(parts[1:len(parts)-2], ":")),
		Dim:      dim,
		Bits:     strings.TrimSpace(parts[len(parts)-1]),
	}, nil
}

type vectorManifest struct {
	SchemaVersion    string        `json:"schema_version"`
	Embedder         embedderStamp `json:"embedder"`
	CorpusGen        int64         `json:"corpus_gen"`
	ClaimCount       int           `json:"claim_count"`
	EmbeddedCount    int           `json:"embedded_count"`
	WrapTemplateHash string        `json:"wrap_template_hash"`
}

type vectorCache struct {
	dir      string
	stamp    embedderStamp
	identity string
}

type vectorKey struct {
	claimID  string
	textHash [32]byte
}

type vectorRow struct {
	ClaimID  string
	TextHash [32]byte
	Vector   []float32
}

func newVectorCache(compoundHome, identity string) (*vectorCache, error) {
	if strings.TrimSpace(compoundHome) == "" {
		// No home to persist the cache — degrade to a temp dir rather than fail the whole
		// engine (which used to drop even lexical recall to zero, measured 2026-07-07).
		// Vectors are a cache: losing them across restarts costs re-embed time, never
		// correctness. The operator path always gets a real home (compound forwards it).
		compoundHome = filepath.Join(os.TempDir(), "compound-breadth-cache")
	}
	stamp, err := parseEmbedderIdentity(identity)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(compoundHome, "indexes", "breadth", stampSlug(identity))
	return &vectorCache{dir: dir, stamp: stamp, identity: identity}, nil
}

func (c *vectorCache) load() (vectorManifest, map[vectorKey]vectorRow, error) {
	manifest, err := c.readManifest()
	if err != nil {
		return vectorManifest{}, nil, err
	}
	if manifest.SchemaVersion != vectorSchemaVersion {
		return vectorManifest{}, nil, fmt.Errorf("%w: schema %q", errVectorCacheCorrupt, manifest.SchemaVersion)
	}
	if manifest.Embedder.Identity() != c.identity {
		return vectorManifest{}, nil, fmt.Errorf("%w: cache %q configured %q", errVectorEmbedderMismatch, manifest.Embedder.Identity(), c.identity)
	}
	if manifest.WrapTemplateHash != wrapTemplateHash() {
		return vectorManifest{}, nil, fmt.Errorf("%w: cache %q configured %q", errVectorTemplateMismatch, manifest.WrapTemplateHash, wrapTemplateHash())
	}
	rows, err := c.readVectors(manifest.Embedder.Dim, manifest.ClaimCount, manifest.EmbeddedCount)
	return manifest, rows, err
}

func (c *vectorCache) readManifest() (vectorManifest, error) {
	raw, err := os.ReadFile(filepath.Join(c.dir, "manifest.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return vectorManifest{}, errVectorCacheMiss
		}
		return vectorManifest{}, err
	}
	var manifest vectorManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return vectorManifest{}, vectorCacheCorruption("parse vector manifest", err)
	}
	return manifest, nil
}

func (c *vectorCache) readVectors(expectedDim, expectedClaimCount, expectedEmbeddedCount int) (map[vectorKey]vectorRow, error) {
	f, err := os.Open(filepath.Join(c.dir, "vectors.bin"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errVectorCacheMiss
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, vectorCacheReadError("stat vector cache", err)
	}
	r := bufio.NewReader(f)
	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, vectorCacheReadError("read vector header", err)
	}
	if magic != vectorMagic {
		return nil, vectorCacheCorruption("bad magic", errVectorDimMismatch)
	}
	headerDim, err := readUint32(r)
	if err != nil {
		return nil, vectorCacheReadError("read vector dim", err)
	}
	if int(headerDim) != expectedDim {
		return nil, vectorCacheCorruption(fmt.Sprintf("header dim %d != manifest dim %d", headerDim, expectedDim), errVectorDimMismatch)
	}
	count, err := readUint64(r)
	if err != nil {
		return nil, vectorCacheReadError("read vector count", err)
	}
	if err := validateVectorRowCount(count, expectedClaimCount, expectedEmbeddedCount, expectedDim, info.Size()); err != nil {
		return nil, err
	}
	rows := make(map[vectorKey]vectorRow, int(count))
	for i := uint64(0); i < count; i++ {
		idLen, err := readUint32(r)
		if err != nil {
			return nil, vectorCacheReadError(fmt.Sprintf("read vector row %d id length", i), err)
		}
		if idLen == 0 || idLen > 1<<20 {
			return nil, vectorCacheCorruption(fmt.Sprintf("vector row %d invalid claim id length %d", i, idLen), nil)
		}
		idBytes := make([]byte, idLen)
		if _, err := io.ReadFull(r, idBytes); err != nil {
			return nil, vectorCacheReadError(fmt.Sprintf("read vector row %d claim id", i), err)
		}
		var textHash [32]byte
		if _, err := io.ReadFull(r, textHash[:]); err != nil {
			return nil, vectorCacheReadError(fmt.Sprintf("read vector row %d text hash", i), err)
		}
		rowDim, err := readUint32(r)
		if err != nil {
			return nil, vectorCacheReadError(fmt.Sprintf("read vector row %d dim", i), err)
		}
		if rowDim != headerDim {
			return nil, vectorCacheCorruption(fmt.Sprintf("row %d dim %d != header dim %d", i, rowDim, headerDim), errVectorDimMismatch)
		}
		vec := make([]float32, headerDim)
		for j := range vec {
			bits, err := readUint32(r)
			if err != nil {
				return nil, vectorCacheReadError(fmt.Sprintf("read vector row %d value %d", i, j), err)
			}
			v := math.Float32frombits(bits)
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return nil, vectorCacheCorruption(fmt.Sprintf("non-finite row %d value %d", i, j), errVectorDimMismatch)
			}
			vec[j] = v
		}
		claimID := string(idBytes)
		key := vectorKey{claimID: claimID, textHash: textHash}
		rows[key] = vectorRow{ClaimID: claimID, TextHash: textHash, Vector: vec}
	}
	if _, err := r.Peek(1); err == nil {
		return nil, vectorCacheCorruption("trailing vector row content", nil)
	} else if !errors.Is(err, io.EOF) {
		return nil, vectorCacheReadError("check vector cache trailing content", err)
	}
	return rows, nil
}

func validateVectorRowCount(count uint64, expectedClaimCount, expectedEmbeddedCount, expectedDim int, fileSize int64) error {
	if expectedClaimCount < 0 || expectedEmbeddedCount < 0 {
		return vectorCacheCorruption(fmt.Sprintf("negative manifest counts claims=%d embedded=%d", expectedClaimCount, expectedEmbeddedCount), nil)
	}
	maxInt := uint64(^uint(0) >> 1)
	if count > maxInt {
		return vectorCacheCorruption(fmt.Sprintf("row count %d exceeds platform int capacity %d", count, maxInt), nil)
	}
	if count != uint64(expectedClaimCount) || count != uint64(expectedEmbeddedCount) {
		return vectorCacheCorruption(fmt.Sprintf("row count %d != manifest claims=%d embedded=%d", count, expectedClaimCount, expectedEmbeddedCount), nil)
	}
	const vectorHeaderBytes = int64(len(vectorMagic) + 4 + 8)
	if fileSize < vectorHeaderBytes {
		return vectorCacheCorruption(fmt.Sprintf("vector file size %d smaller than header %d", fileSize, vectorHeaderBytes), nil)
	}
	if expectedDim < 0 || uint64(expectedDim) > (^uint64(0)-41)/4 {
		return vectorCacheCorruption(fmt.Sprintf("invalid vector dimension %d", expectedDim), nil)
	}
	// Each row needs an id-length word, at least one id byte, a 32-byte text
	// hash, a dimension word, and expectedDim float32 values. This conservative
	// minimum bounds the row count before map allocation or row iteration.
	minRowBytes := uint64(4+1+32+4) + uint64(expectedDim)*4
	payloadBytes := uint64(fileSize - vectorHeaderBytes)
	if count > payloadBytes/minRowBytes {
		return vectorCacheCorruption(fmt.Sprintf("row count %d exceeds file-derived maximum %d", count, payloadBytes/minRowBytes), nil)
	}
	return nil
}

func vectorCacheReadError(operation string, err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return vectorCacheCorruption(operation, err)
}

func vectorCacheCorruption(detail string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", errVectorCacheCorrupt, detail)
	}
	return fmt.Errorf("%w: %s: %w", errVectorCacheCorrupt, detail, cause)
}

func (c *vectorCache) write(rows []vectorRow, corpusGen int64) error {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	if err := c.writeVectors(rows); err != nil {
		return err
	}
	manifest := vectorManifest{
		SchemaVersion:    vectorSchemaVersion,
		Embedder:         c.stamp,
		CorpusGen:        corpusGen,
		ClaimCount:       len(rows),
		EmbeddedCount:    len(rows),
		WrapTemplateHash: wrapTemplateHash(),
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return atomicWrite(filepath.Join(c.dir, "manifest.json"), raw)
}

func (c *vectorCache) writeVectors(rows []vectorRow) error {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	if _, err := w.Write(vectorMagic[:]); err != nil {
		return err
	}
	if err := writeUint32(w, uint32(c.stamp.Dim)); err != nil {
		return err
	}
	if err := writeUint64(w, uint64(len(rows))); err != nil {
		return err
	}
	for _, row := range rows {
		if len(row.Vector) != c.stamp.Dim {
			return fmt.Errorf("%w: row %s dim %d != cache dim %d", errVectorDimMismatch, row.ClaimID, len(row.Vector), c.stamp.Dim)
		}
		if err := writeUint32(w, uint32(len(row.ClaimID))); err != nil {
			return err
		}
		if _, err := w.WriteString(row.ClaimID); err != nil {
			return err
		}
		if _, err := w.Write(row.TextHash[:]); err != nil {
			return err
		}
		if err := writeUint32(w, uint32(len(row.Vector))); err != nil {
			return err
		}
		for _, v := range row.Vector {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return fmt.Errorf("%w: non-finite vector for %s", errVectorDimMismatch, row.ClaimID)
			}
			if err := writeUint32(w, math.Float32bits(v)); err != nil {
				return err
			}
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(c.dir, "vectors.bin"), buf.Bytes())
}

func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func readUint32(r io.Reader) (uint32, error) {
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(buf[:]), nil
}

func readUint64(r io.Reader) (uint64, error) {
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(buf[:]), nil
}

func writeUint32(w io.Writer, v uint32) error {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], v)
	_, err := w.Write(buf[:])
	return err
}

func writeUint64(w io.Writer, v uint64) error {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], v)
	_, err := w.Write(buf[:])
	return err
}

func textHash(text string) [32]byte { return sha256.Sum256([]byte(text)) }

func wrapTemplateHash() string {
	h := sha256.Sum256([]byte(wrapTemplate))
	return hex.EncodeToString(h[:])
}

func stampSlug(identity string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(identity) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "unknown"
	}
	return slug
}
