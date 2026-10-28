package eventstore

// Negative-membership filter for the cold (sealed) segments of one family.
//
// checkID must prove an id is absent from EVERY sealed segment before a put may
// append it. Before this filter that proof loaded (read + JSON-parse) the
// sidecar .idx of every sealed segment of the family on each put in each
// short-lived hook process — hundreds of MB of JSON per tool call on a mature
// home. The filter answers the overwhelmingly common question ("is this brand
// new id in any sealed segment?") with a handful of byte reads.
//
// Contract:
//   - It is a CACHE, never authority. A Bloom filter has no false negatives, so
//     "definitely absent" is sound; "maybe present" falls back to the unchanged
//     per-segment index scan, which alone decides conflict vs already-present.
//   - It is trusted only while its stored fingerprint (name+size+mtime of every
//     sealed segment it covers) still matches the family's current sealed set.
//     A segment that grew, shrank, was touched, or vanished forces a rebuild.
//     Segments only added (a rollover or day seal) extend the filter in place.
//   - Missing, torn, foreign-version, or over-capacity filters are rebuilt once
//     from the validated indexes and the rebuild is logged. A rebuild that
//     fails degrades to "maybe present" (the full scan), never to "absent".
//   - Segment records are never touched. The hot (newest) segment is not in
//     the filter; it keeps its memo path in checkID. A put appends to the hot
//     segment, so it needs no filter write; the segment joins the filter when
//     it stops being the newest.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

const (
	membershipFileName = "membership.bloom"
	membershipMagic    = "ESBF0001"
	membershipVersion  = 1
	membershipHashes   = 7
	membershipBitsPer  = 10
	membershipMinCap   = 4096
)

type membershipSegment struct {
	Name             string `json:"name"`
	Size             int64  `json:"size"`
	ModifiedUnixNano int64  `json:"modified_unix_nano"`
}

type membershipHeader struct {
	Version  int                 `json:"version"`
	Segments []membershipSegment `json:"segments"`
	Bits     uint64              `json:"bits"`
	Hashes   int                 `json:"hashes"`
	Count    int                 `json:"count"`
	Capacity int                 `json:"capacity"`
}

// membershipObserver is a test-only seam fired when the filter is rebuilt
// (full=true) or extended (full=false). Nil in production.
var membershipObserver func(family string, full bool)

func (e *engine) membershipPath(family string) string {
	return filepath.Join(e.indexRoot, family, membershipFileName)
}

func membershipPositions(id string, bits uint64, hashes int) []uint64 {
	sum := sha256.Sum256([]byte(id))
	h1 := binary.LittleEndian.Uint64(sum[0:8])
	h2 := binary.LittleEndian.Uint64(sum[8:16]) | 1
	out := make([]uint64, hashes)
	for i := range out {
		out[i] = (h1 + uint64(i)*h2) % bits
	}
	return out
}

func membershipAdd(bits []byte, m uint64, hashes int, id string) {
	for _, p := range membershipPositions(id, m, hashes) {
		bits[p>>3] |= 1 << (p & 7)
	}
}

// coldMaybeContains reports whether id MAY be present in any of the sealed
// segments. A false answer is a proof of absence relative to the current
// segment set; true means "scan the indexes".
func (e *engine) coldMaybeContains(family, id string, cold []segmentFile) bool {
	if len(cold) == 0 {
		return false
	}
	current := make([]membershipSegment, 0, len(cold))
	for _, segment := range cold {
		info, err := os.Stat(segment.path)
		if err != nil {
			return true
		}
		current = append(current, membershipSegment{
			Name: filepath.Base(segment.path), Size: info.Size(), ModifiedUnixNano: info.ModTime().UnixNano(),
		})
	}

	path := e.membershipPath(family)
	file, err := os.Open(path)
	if err == nil {
		header, bitsOffset, ok := readMembershipHeader(file)
		if ok {
			added, covered := membershipDiff(header.Segments, current)
			if covered && len(added) == 0 {
				found, err := probeMembership(file, bitsOffset, header, id)
				_ = file.Close()
				if err == nil {
					return found
				}
				return true
			}
			if covered && header.Count < header.Capacity {
				_ = file.Close()
				if bits, ok := e.extendMembership(family, path, header, bitsOffset, cold, current, added); ok {
					return bits.contains(id)
				}
				return true
			}
		}
		_ = file.Close()
	}
	bits, ok := e.rebuildMembership(family, path, cold, current, "filter missing, stale, or unreadable")
	if !ok {
		return true
	}
	return bits.contains(id)
}

type membershipBits struct {
	header membershipHeader
	bits   []byte
}

func (b membershipBits) contains(id string) bool {
	for _, p := range membershipPositions(id, b.header.Bits, b.header.Hashes) {
		if b.bits[p>>3]&(1<<(p&7)) == 0 {
			return false
		}
	}
	return true
}

// membershipDiff compares the filter's recorded segments with the current
// sealed set. covered means every recorded segment still exists with an
// identical size and mtime; added lists the current segments the filter has
// not seen yet.
func membershipDiff(recorded, current []membershipSegment) (added []membershipSegment, covered bool) {
	now := make(map[string]membershipSegment, len(current))
	for _, segment := range current {
		now[segment.Name] = segment
	}
	for _, segment := range recorded {
		got, ok := now[segment.Name]
		if !ok || got != segment {
			return nil, false
		}
	}
	seen := make(map[string]struct{}, len(recorded))
	for _, segment := range recorded {
		seen[segment.Name] = struct{}{}
	}
	for _, segment := range current {
		if _, ok := seen[segment.Name]; !ok {
			added = append(added, segment)
		}
	}
	return added, true
}

func readMembershipHeader(file *os.File) (membershipHeader, int64, bool) {
	var fixed [12]byte
	if _, err := io.ReadFull(file, fixed[:]); err != nil || string(fixed[:8]) != membershipMagic {
		return membershipHeader{}, 0, false
	}
	headerLen := int64(binary.LittleEndian.Uint32(fixed[8:12]))
	info, err := file.Stat()
	if err != nil || headerLen <= 0 || 12+headerLen > info.Size() {
		return membershipHeader{}, 0, false
	}
	raw := make([]byte, headerLen)
	if _, err := io.ReadFull(file, raw); err != nil {
		return membershipHeader{}, 0, false
	}
	var header membershipHeader
	if err := json.Unmarshal(raw, &header); err != nil || header.Version != membershipVersion ||
		header.Bits == 0 || header.Bits%8 != 0 || header.Hashes <= 0 || header.Count < 0 || header.Capacity <= 0 {
		return membershipHeader{}, 0, false
	}
	offset := 12 + headerLen
	if info.Size() != offset+int64(header.Bits/8) {
		return membershipHeader{}, 0, false
	}
	return header, offset, true
}

func probeMembership(file *os.File, offset int64, header membershipHeader, id string) (bool, error) {
	var one [1]byte
	for _, p := range membershipPositions(id, header.Bits, header.Hashes) {
		if _, err := file.ReadAt(one[:], offset+int64(p>>3)); err != nil {
			return true, err
		}
		if one[0]&(1<<(p&7)) == 0 {
			return false, nil
		}
	}
	return true, nil
}

func (e *engine) segmentIDs(family string, segment segmentFile) ([]string, error) {
	index, err := e.loadOrBuildIndex(family, segment)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(index.Entries))
	for id := range index.Entries {
		ids = append(ids, id)
	}
	return ids, nil
}

func (e *engine) rebuildMembership(family, path string, cold []segmentFile, current []membershipSegment, reason string) (membershipBits, bool) {
	var all []string
	for _, segment := range cold {
		ids, err := e.segmentIDs(family, segment)
		if err != nil {
			e.logf("eventstore: membership filter rebuild for %s skipped: %v", family, err)
			return membershipBits{}, false
		}
		all = append(all, ids...)
	}
	capacity := len(all) * 2
	if capacity < membershipMinCap {
		capacity = membershipMinCap
	}
	m := uint64(capacity) * membershipBitsPer
	m = (m + 7) &^ 7
	out := membershipBits{
		header: membershipHeader{
			Version: membershipVersion, Segments: current, Bits: m, Hashes: membershipHashes,
			Count: len(all), Capacity: capacity,
		},
		bits: make([]byte, m/8),
	}
	for _, id := range all {
		membershipAdd(out.bits, m, membershipHashes, id)
	}
	e.persistMembership(path, out)
	e.logf("eventstore: rebuilt membership filter %s (%s; %d ids over %d sealed segments)", path, reason, len(all), len(cold))
	if membershipObserver != nil {
		membershipObserver(family, true)
	}
	return out, true
}

func (e *engine) extendMembership(family, path string, header membershipHeader, bitsOffset int64, cold []segmentFile, current, added []membershipSegment) (membershipBits, bool) {
	raw, err := os.ReadFile(path)
	if err != nil || int64(len(raw)) != bitsOffset+int64(header.Bits/8) {
		return membershipBits{}, false
	}
	out := membershipBits{header: header, bits: raw[bitsOffset:]}
	addedNames := make(map[string]struct{}, len(added))
	for _, segment := range added {
		addedNames[segment.Name] = struct{}{}
	}
	for _, segment := range cold {
		if _, ok := addedNames[filepath.Base(segment.path)]; !ok {
			continue
		}
		ids, err := e.segmentIDs(family, segment)
		if err != nil {
			e.logf("eventstore: membership filter extend for %s skipped: %v", family, err)
			return membershipBits{}, false
		}
		for _, id := range ids {
			membershipAdd(out.bits, header.Bits, header.Hashes, id)
		}
		out.header.Count += len(ids)
	}
	if out.header.Count > out.header.Capacity {
		return e.rebuildMembership(family, path, cold, current, "filter over capacity")
	}
	out.header.Segments = current
	e.persistMembership(path, out)
	if membershipObserver != nil {
		membershipObserver(family, false)
	}
	return out, true
}

// persistMembership writes the filter with a temp file and rename so a reader
// sees the old or the new filter, never a mix. Best effort: a cache write
// failure only costs the next process a rebuild. A torn file fails the size
// check in readMembershipHeader.
func (e *engine) persistMembership(path string, filter membershipBits) {
	rawHeader, err := json.Marshal(filter.header)
	if err != nil {
		return
	}
	var buf bytes.Buffer
	buf.WriteString(membershipMagic)
	var length [4]byte
	binary.LittleEndian.PutUint32(length[:], uint32(len(rawHeader)))
	buf.Write(length[:])
	buf.Write(rawHeader)
	buf.Write(filter.bits)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".membership.tmp-*")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		e.logf("eventstore: membership filter persist failed: %v", err)
	}
}
