package eventstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
)

// Enumeration needs a complete inventory, a stronger claim than the point-read
// index's source hash and valid ranges. This separate marker is created only
// after comparing every last-write-wins entry with parsed segment truth.
// Existing .verified markers cannot attest inventory completeness.
type enumerationInventoryMarker struct {
	Version       int                 `json:"version"`
	Source        indexVerifiedMarker `json:"source"`
	EntriesSHA256 string              `json:"entries_sha256"`
}

func enumerationInventorySnapshot(path string, segment segmentFile, index segmentIndex) (enumerationInventoryMarker, error) {
	idxInfo, err := os.Stat(path)
	if err != nil {
		return enumerationInventoryMarker{}, err
	}
	segInfo, err := os.Stat(segment.path)
	if err != nil {
		return enumerationInventoryMarker{}, err
	}
	body, err := json.Marshal(index.Entries)
	if err != nil {
		return enumerationInventoryMarker{}, err
	}
	return enumerationInventoryMarker{
		Version: 1,
		Source: indexVerifiedMarker{
			SchemaVersion: indexVerifiedMarkerSchema,
			IdxSizeBytes:  idxInfo.Size(), IdxMtimeUnixNano: idxInfo.ModTime().UnixNano(),
			SegSizeBytes: segInfo.Size(), SegMtimeUnixNano: segInfo.ModTime().UnixNano(),
			SegSHA256: index.Segment.SHA256,
		},
		EntriesSHA256: contentHash(body),
	}, nil
}

func (e *engine) loadCompleteEnumerationIndex(family string, segment segmentFile, regularKind string) (segmentIndex, error) {
	for attempt := 0; attempt < 3; attempt++ {
		index, err := e.loadOrBuildIndex(family, segment)
		if err != nil {
			return segmentIndex{}, err
		}
		path := e.indexPath(family, segment)
		before, err := enumerationInventorySnapshot(path, segment, index)
		if err != nil {
			return segmentIndex{}, err
		}
		var marker enumerationInventoryMarker
		if body, err := os.ReadFile(path + ".inventory-verified"); err == nil &&
			json.Unmarshal(body, &marker) == nil && marker == before {
			return index, nil
		}

		// A missing marker, old source-only marker, or changed inventory pays one
		// complete parse. Thereafter unrelated sealed bodies can be skipped.
		frames, err := e.readSegment(segment.path, false, regularKind)
		if err != nil {
			return segmentIndex{}, err
		}
		notifyScanParse(segment.path, len(frames), 0)
		after, err := enumerationInventorySnapshot(path, segment, index)
		if err != nil {
			return segmentIndex{}, err
		}
		if after != before {
			continue
		}
		truth := make(map[string]indexEntry, len(frames))
		for _, frame := range frames {
			truth[frame.Frame.ID] = indexEntry{Offset: frame.Offset, Length: frame.End - frame.Offset}
		}
		if !reflect.DeepEqual(index.Entries, truth) {
			// Source metadata is already validated and remained stable throughout
			// the strict parse. Replace only the disposable, incomplete inventory.
			index.Entries = truth
			if err := writeIndex(path, index); err != nil {
				return segmentIndex{}, err
			}
			after, err = enumerationInventorySnapshot(path, segment, index)
			if err != nil {
				return segmentIndex{}, err
			}
			if after.Source.SegSizeBytes != before.Source.SegSizeBytes ||
				after.Source.SegMtimeUnixNano != before.Source.SegMtimeUnixNano {
				continue
			}
		}
		rememberEnumerationInventory(path, after)
		return index, nil
	}
	return segmentIndex{}, fmt.Errorf("event enumeration inventory changed during validation: %s", segment.path)
}

// A memo write failure only costs another validation, never loses records.
func rememberEnumerationInventory(path string, marker enumerationInventoryMarker) {
	body, err := json.Marshal(marker)
	if err != nil {
		return
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".inventory-verified-*")
	if err != nil {
		return
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if _, err := file.Write(append(body, '\n')); err != nil {
		file.Close()
		return
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return
	}
	if err := file.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp, path+".inventory-verified")
}
