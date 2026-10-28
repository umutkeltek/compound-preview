package work

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

const privateDeliveryCaptureTotalBytes = 128 << 20

type privateCaptureBuffer struct {
	bytes.Buffer
	remaining *int64
}

func (b *privateCaptureBuffer) Write(p []byte) (int, error) {
	if int64(len(p)) > *b.remaining {
		return 0, fmt.Errorf("private delivery capture exceeds total body limit")
	}
	n, err := b.Buffer.Write(p)
	*b.remaining -= int64(n)
	return n, err
}

func privateDeliveryArtifactRef(body []byte) string {
	return fmt.Sprintf("artifact:sha256:%x", sha256.Sum256(body))
}

func privateDeliverySHA(value string) bool {
	body, err := hex.DecodeString(value)
	return err == nil && len(body) == sha256.Size && hex.EncodeToString(body) == value
}

// All source bodies are captured and hash-compared before the first custody write.
// The same bounded read supplies both the hash and the body; a later validation is
// explicitly a drift check, never the source of the captured body.
func capturePrivateDeliverySources(request PrivateRuntimeDeliveryCaptureRequest, target string) ([]privateRuntimeDeliveryEntry, map[string][]byte, error) {
	if len(request.Files) == 0 || len(request.Files) > maxPrivateRuntimeDeliveryFiles {
		return nil, nil, fmt.Errorf("private delivery capture file count out of range")
	}
	entries := make([]privateRuntimeDeliveryEntry, 0, len(request.Files))
	afterFiles := make([]privateRuntimeDeliveryFile, 0, len(request.Files))
	for _, file := range request.Files {
		if !privateDeliverySHA(file.AfterSHA256) {
			return nil, nil, fmt.Errorf("capture requires canonical after SHA256")
		}
		if file.BeforeAbsent {
			if file.BeforePath != "" || file.BeforeSHA256 != "" {
				return nil, nil, fmt.Errorf("prior absence conflicts with preimage")
			}
		} else if file.BeforePath == "" || !privateDeliverySHA(file.BeforeSHA256) {
			return nil, nil, fmt.Errorf("capture requires exact preimage path and SHA256 or prior absence")
		}
		entry := privateRuntimeDeliveryEntry{Path: file.Path, AfterArtifact: "artifact:sha256:" + file.AfterSHA256, BeforeAbsent: file.BeforeAbsent}
		if !file.BeforeAbsent {
			entry.BeforeArtifact = "artifact:sha256:" + file.BeforeSHA256
		}
		entries = append(entries, entry)
		afterFiles = append(afterFiles, privateRuntimeDeliveryFile{Path: file.Path, AfterSHA256: file.AfterSHA256})
	}
	bodyByRef := make(map[string][]byte)
	remaining := int64(privateDeliveryCaptureTotalBytes)
	consume := func(root *os.Root, path string) (string, error) {
		buffer := privateCaptureBuffer{remaining: &remaining}
		digest, err := privateDeliveryDigestTo(root, path, &buffer)
		if err != nil {
			return "", err
		}
		bodyByRef["artifact:sha256:"+digest] = buffer.Bytes()
		return digest, nil
	}
	if err := visitPrivateRuntimeDeliveryPaths(request.RepositoryRoot, target, afterFiles, consume); err != nil {
		return nil, nil, err
	}
	for _, file := range request.Files {
		if file.BeforeAbsent {
			continue
		}
		before := []privateRuntimeDeliveryFile{{Path: file.BeforePath, AfterSHA256: file.BeforeSHA256}}
		if err := visitPrivateRuntimeDeliveryPaths(request.RepositoryRoot, filepath.Join(request.RepositoryRoot, filepath.FromSlash(file.BeforePath)), before, consume); err != nil {
			return nil, nil, fmt.Errorf("capture preimage: %w", err)
		}
	}
	return entries, bodyByRef, nil
}
