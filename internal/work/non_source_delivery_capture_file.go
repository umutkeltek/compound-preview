package work

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

func readDeliveryCaptureFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("capture input must be a regular file of at most %d bytes", limit)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() > limit {
		return nil, fmt.Errorf("capture input changed before read: %v", err)
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, fmt.Errorf("capture input read failed or exceeded byte limit: %v", err)
	}
	return body, nil
}

// CaptureNonSourceDeliveryFile dispatches only code-owned schemas.
// It reads once, then strictly decodes the selected input before any mutation.
func (s *Store) CaptureNonSourceDeliveryFile(workID, requestPath, actor string, now time.Time) (NonSourceDeliveryCaptureResult, error) {
	out := NonSourceDeliveryCaptureResult{Status: "capture_incomplete"}
	body, err := readDeliveryCaptureFile(requestPath, privateDeliveryMetadataLimit)
	if err != nil {
		return out, err
	}
	var schema struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(body, &schema); err != nil {
		return out, err
	}
	switch schema.SchemaVersion {
	case InstalledPackageDeliveryCaptureSchema:
		var request InstalledPackageDeliveryCaptureRequest
		if err := decodeDeliveryCapture(body, &request); err != nil {
			return out, err
		}
		return s.CaptureInstalledPackageDelivery(workID, request, actor, now)
	case PrivateRuntimeDeliveryCaptureSchema:
		var request PrivateRuntimeDeliveryCaptureRequest
		if err := decodeDeliveryCapture(body, &request); err != nil {
			return out, err
		}
		return s.CapturePrivateRuntimeDelivery(workID, request, actor, now)
	case WorktreeRetirementDeliveryCaptureSchema:
		var request WorktreeRetirementDeliveryCaptureRequest
		if err := decodeDeliveryCapture(body, &request); err != nil {
			return out, err
		}
		return s.CaptureWorktreeRetirementDelivery(workID, request, actor, now)
	default:
		return out, fmt.Errorf("unsupported delivery capture schema %q", schema.SchemaVersion)
	}
}

func decodeDeliveryCapture(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("delivery input must contain exactly one JSON value")
	}
	return nil
}
