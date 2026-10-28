package corpus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"compound/internal/sourceobs"
)

type DoctorReport struct {
	SchemaVersion       string        `json:"schema_version"`
	Status              string        `json:"status"`
	Lane                string        `json:"lane"`
	IndexDir            string        `json:"index_dir"`
	EmbedderIdentity    string        `json:"embedder_identity,omitempty"`
	Items               int           `json:"items"`
	Chunks              int           `json:"chunks"`
	EmbeddedChunks      int           `json:"embedded_chunks"`
	MissingChunkRecords int           `json:"missing_chunk_records"`
	Checks              []DoctorCheck `json:"checks"`
}

type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func Doctor(homePath string, lane LaneConfig) (DoctorReport, error) {
	lane, err := ValidateLaneConfig(lane)
	if err != nil {
		return DoctorReport{}, err
	}
	dir := IndexDir(homePath, lane)
	report := DoctorReport{
		SchemaVersion:    ManifestSchemaVersion,
		Status:           "fail",
		Lane:             lane.Lane,
		IndexDir:         dir,
		EmbedderIdentity: lane.Embedder.Identity(),
	}
	identity, err := readIdentity(dir)
	if err != nil {
		if os.IsNotExist(err) {
			report.Checks = append(report.Checks, doctorCheck("identity", false, "identity.json missing"))
			return report, nil
		}
		return report, err
	}
	report.Checks = append(report.Checks, doctorCheck("identity", identity.EmbedderIdentity == lane.Embedder.Identity(), identity.EmbedderIdentity))

	manifest, err := readManifest(dir)
	if err != nil {
		if os.IsNotExist(err) {
			report.Checks = append(report.Checks, doctorCheck("manifest", false, "manifest.json missing"))
			return report, nil
		}
		return report, err
	}
	report.Items = len(manifest.Items)
	rawMissing, err := rawMissingChunkFields(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return report, err
	}
	report.MissingChunkRecords += rawMissing
	lossyOrder := 0
	chunkHashes := map[string]string{}
	for _, item := range manifest.Items {
		if err := validateChunkRecords(item); err != nil {
			lossyOrder++
		}
		for _, chunk := range item.Chunks {
			report.Chunks++
			chunkHashes[fmt.Sprintf("%s#%d", item.ID, chunk.Ordinal)] = strings.TrimSpace(chunk.ChunkHash)
		}
	}
	report.Checks = append(report.Checks,
		doctorCheck("chunk_records", report.MissingChunkRecords == 0, fmt.Sprintf("missing=%d", report.MissingChunkRecords)),
		doctorCheck("chunk_order", lossyOrder == 0, fmt.Sprintf("lossy_items=%d", lossyOrder)),
	)

	vectors, err := readVectors(dir)
	if err != nil {
		if os.IsNotExist(err) {
			report.Checks = append(report.Checks, doctorCheck("vectors", false, "vectors.json missing"))
			return report, nil
		}
		return report, err
	}
	for id, vec := range vectors.Vectors {
		if _, ok := chunkHashes[id]; !ok {
			report.Checks = append(report.Checks, doctorCheck("vectors_current", false, "removed chunk vector "+id))
			continue
		}
		if len(vec) != lane.Embedder.Dims {
			report.Checks = append(report.Checks, doctorCheck("vector_dims", false, fmt.Sprintf("%s dim=%d", id, len(vec))))
		}
	}
	report.EmbeddedChunks = len(vectors.Vectors)
	report.Checks = append(report.Checks, doctorCheck("vectors", true, fmt.Sprintf("embedded=%d", report.EmbeddedChunks)))

	report.Status = "ok"
	for _, check := range report.Checks {
		if check.Status != "ok" {
			report.Status = "fail"
			break
		}
	}
	return report, nil
}

func rawMissingChunkFields(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var doc struct {
		Items []struct {
			Chunks []map[string]json.RawMessage `json:"chunks"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, err
	}
	missing := 0
	for _, item := range doc.Items {
		for _, chunk := range item.Chunks {
			if _, ok := chunk["ordinal"]; !ok {
				missing++
			}
			value, ok := chunk["chunk_hash"]
			if !ok || strings.Trim(strings.TrimSpace(string(value)), `"`) == "" {
				missing++
			}
		}
	}
	return missing, nil
}

func validateChunkRecords(item sourceobs.SourceManifestItem) error {
	if len(item.Chunks) == 0 {
		const emptyDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		if item.BodyPolicy == "owned_snapshot" &&
			item.FileHash == "sha256:"+emptyDigest &&
			item.BodyRef == "artifact:sha256:"+emptyDigest {
			return nil
		}
		return fmt.Errorf("item %s has no chunks", item.ID)
	}
	ords := make([]int, 0, len(item.Chunks))
	seen := map[int]bool{}
	for _, chunk := range item.Chunks {
		if strings.TrimSpace(chunk.ChunkHash) == "" {
			return fmt.Errorf("item %s has empty chunk hash", item.ID)
		}
		if seen[chunk.Ordinal] {
			return fmt.Errorf("item %s duplicate chunk ordinal %d", item.ID, chunk.Ordinal)
		}
		seen[chunk.Ordinal] = true
		ords = append(ords, chunk.Ordinal)
	}
	sort.Ints(ords)
	for i, ord := range ords {
		if ord != i {
			return fmt.Errorf("item %s non-contiguous chunk ordinal %d at %d", item.ID, ord, i)
		}
	}
	return nil
}

func doctorCheck(name string, ok bool, detail string) DoctorCheck {
	status := "fail"
	if ok {
		status = "ok"
	}
	return DoctorCheck{Name: name, Status: status, Detail: detail}
}
