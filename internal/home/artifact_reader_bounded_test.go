package home

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactReaderBoundedRejectsGrowthAndOversizeBeforeRead(t *testing.T) {
	for _, grow := range []bool{false, true} {
		t.Run(map[bool]string{false: "oversized before read", true: "growth after stat"}[grow], func(t *testing.T) {
			homePath := canonicalArtifactReaderHome(t)
			store, err := NewArtifactStore(homePath)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := store.PutPrivate([]byte("small"))
			if err != nil {
				t.Fatal(err)
			}
			digest, err := ArtifactHex(ref)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(homePath, "private/artifacts/sha256", digest)
			if !grow {
				if err := os.Truncate(path, 1<<30); err != nil {
					t.Fatal(err)
				}
			}
			previous := rootedCASBeforeBoundedRead
			t.Cleanup(func() { rootedCASBeforeBoundedRead = previous })
			reads := 0
			rootedCASBeforeBoundedRead = func() {
				reads++
				if err := os.Truncate(path, 1<<30); err != nil {
					t.Fatal(err)
				}
			}
			reader, err := OpenArtifactReader(homePath)
			if err != nil {
				t.Fatal(err)
			}
			got, err := reader.GetPrivateBounded(ref, 32)
			if err == nil || got != nil || !strings.Contains(err.Error(), "limit") {
				t.Fatalf("body=%q err=%v", got, err)
			}
			if !grow && reads != 0 {
				t.Fatal("oversize reached body read")
			}
			if grow && reads != 1 {
				t.Fatal("growth boundary not exercised")
			}
		})
	}
}

func TestArtifactReaderPrivateBoundedReads(t *testing.T) {
	homePath := canonicalArtifactReaderHome(t)
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("bounded private evidence")
	ref, err := store.PutPrivate(body)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenArtifactReader(homePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int64{int64(len(body)), int64(len(body) + 1)} {
		got, err := reader.GetPrivateBounded(ref, limit)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("limit %d: %q, %v", limit, got, err)
		}
	}
	if got, err := reader.GetPrivateBounded(ref, int64(len(body)-1)); err == nil || got != nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized body: %q, %v", got, err)
	}
	for _, limit := range []int64{0, -1, 1<<63 - 1} {
		if _, err := reader.GetPrivateBounded(ref, limit); err == nil {
			t.Fatalf("accepted invalid limit %d", limit)
		}
	}
}
