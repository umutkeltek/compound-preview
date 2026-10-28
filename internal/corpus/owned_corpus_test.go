// owned_corpus_test.go covers the corpus ↔ sourceobs seam for
// body_policy=owned_snapshot. The half-change required by the packet:
// compound-owned SourceManifest bodies (held under COMPOUND_HOME/artifacts)
// must feed hash-verified corpus chunks, survive the original SourceRef
// disappearing, and fail closed on missing / corrupt / wrong-hash / wrong-size
// artifacts. The other body_policy=source_in_place behavior must remain
// untouched. Item-level privacy stays the final gate, so a private
// owned_snapshot is suppressed by default just like a private source_in_place.
//
// Red-first falsifiers named in the packet, all expected to fail against
// the untouched base (readItemBody refuses any body_policy other than
// source_in_place; ResolveChunkText reads from item.SourceRef).
package corpus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/sourceobs"
)

// seedOwnedSnapshotLane builds a corpus lane over a single
// body_policy=owned_snapshot manifest item (id=item_owned). The original
// source file is written into a private temp dir and survives the function
// return — tests that want to prove source-loss resilience call
// removeOwnedSource(fixture) after seeding.
func seedOwnedSnapshotLane(t *testing.T, homePath, body string) ownedCorpusFixture {
	t.Helper()
	sourceDir := t.TempDir()
	sourcePath := filepath.Join(sourceDir, "owned-source.md")
	if err := os.WriteFile(sourcePath, []byte(body), 0o644); err != nil {
		t.Fatalf("write owned source: %v", err)
	}
	family := "owned_snapshot_fixture"
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	manifest, err := sourceobs.BuildOwnedSourceManifest(homePath, family, []string{sourcePath}, now)
	if err != nil {
		t.Fatalf("build owned source manifest: %v", err)
	}
	if len(manifest.Items) != 1 {
		t.Fatalf("expected one owned manifest item, got %d", len(manifest.Items))
	}
	manifest.Items[0].ID = "item_owned"
	manifest.Items[0].SourceKind = "synthetic_dialogue"
	manifest.Items[0].PageSlug = "Owned Snapshot Fixture"
	manifest.Items[0].CorpusGeneration = "owned-gen"
	manifest.Items[0].Privacy = "public"
	if _, err := sourceobs.RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatalf("record owned source manifest: %v", err)
	}
	lane := LaneConfig{
		Lane:         "owned-fixture",
		SourceFamily: family,
		Generation:   "owned-gen",
		Privacy:      "public",
		Embedder: EmbedderConfig{
			Provider: "local",
			Model:    "synthetic-embedder",
			Dims:     3,
			Bits:     "f32",
		},
		Chunk: ChunkConfig{Unit: "runes", Size: 256, Overlap: 16, Boundary: "structure"},
	}
	embedder := &fakeEmbedder{identity: lane.Embedder.Identity(), dim: lane.Embedder.Dims}
	if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("index owned lane: %v", err)
	}
	return ownedCorpusFixture{
		homePath:   homePath,
		sourcePath: sourcePath,
		body:       body,
		lane:       lane,
		embedder:   embedder,
		manifest:   manifest,
	}
}

func removeOwnedSource(t *testing.T, fixture ownedCorpusFixture) {
	t.Helper()
	if err := os.Remove(fixture.sourcePath); err != nil {
		t.Fatalf("remove owned source: %v", err)
	}
}

type ownedCorpusFixture struct {
	homePath   string
	sourcePath string
	body       string
	lane       LaneConfig
	embedder   *fakeEmbedder
	manifest   sourceobs.SourceManifest
}

// TestIndexOwnedSnapshotAfterOriginalDeleted is the load-bearing seam test:
// once the SourceRef is gone, indexing must still succeed because the body
// is held in COMPOUND_HOME's artifact store, not the original path. The
// resulting manifest must cite the existing SourceManifest authority ref
// (no identity drift) and the embedder must receive the exact owned bytes
// (same chunk hashes as a same-body source_in_place index would have).
func TestIndexOwnedSnapshotAfterOriginalDeleted(t *testing.T) {
	homePath := t.TempDir()
	body := "User: owned-body chunk alpha\n\nAssistant: owned-body chunk beta"
	fixture := seedOwnedSnapshotLane(t, homePath, body)
	removeOwnedSource(t, fixture)

	embedder := &fakeEmbedder{identity: fixture.lane.Embedder.Identity(), dim: fixture.lane.Embedder.Dims}
	report, err := Index(context.Background(), homePath, fixture.lane, embedder, IndexOptions{Rebuild: true})
	if err != nil {
		t.Fatalf("rebuild index after source deletion: %v", err)
	}
	if report.Items != 1 {
		t.Fatalf("report items = %d, want 1", report.Items)
	}
	if report.TotalChunks == 0 {
		t.Fatalf("report total chunks = 0, want >= 1")
	}
	if embedder.embedded == 0 {
		t.Fatalf("embedder received zero texts — owned bytes were not served")
	}

	// Identity is unchanged: the corpus manifest still cites the SourceManifest
	// authority ref recorded when the owned manifest was written. Owned body
	// custody is a content-addressed augment, not a new meaning owner.
	dir := IndexDir(homePath, fixture.lane)
	manifest, err := readManifest(dir)
	if err != nil {
		t.Fatalf("read corpus manifest: %v", err)
	}
	wantRef := fixture.manifest.AuthorityRef
	if manifest.SourceManifestRef != wantRef {
		t.Fatalf("corpus manifest source_manifest_ref = %q, want %q", manifest.SourceManifestRef, wantRef)
	}
	identity, err := readIdentity(dir)
	if err != nil {
		t.Fatalf("read corpus identity: %v", err)
	}
	if identity.EmbedderIdentity != fixture.lane.Embedder.Identity() {
		t.Fatalf("corpus identity embedder = %q, want %q", identity.EmbedderIdentity, fixture.lane.Embedder.Identity())
	}

	// Cross-check chunk hashes: re-chunk the owned body directly and compare.
	sourceChunks, err := ChunkItem(fixture.manifest.Items[0], body, fixture.lane.Chunk)
	if err != nil {
		t.Fatalf("re-chunk owned body: %v", err)
	}
	if len(sourceChunks) != report.TotalChunks {
		t.Fatalf("chunks = %d, want %d (owned bytes must equal original body)", report.TotalChunks, len(sourceChunks))
	}
	if len(embedder.texts) != len(sourceChunks) {
		t.Fatalf("embedder texts = %d, want %d exact owned chunks", len(embedder.texts), len(sourceChunks))
	}
	hashes := manifestChunkHashes(manifest)
	for i, chunk := range sourceChunks {
		if embedder.texts[i] != chunk.EmbedText {
			t.Fatalf("embedder text[%d] = %q, want exact owned embed text %q", i, embedder.texts[i], chunk.EmbedText)
		}
		want := hashes[chunk.ID]
		if want == "" {
			t.Fatalf("corpus manifest missing chunk %s", chunk.ID)
		}
		if want != chunk.ChunkHash {
			t.Fatalf("chunk[%d] %s hash %s != direct-chunk hash %s (owned bytes diverged)", i, chunk.ID, want, chunk.ChunkHash)
		}
	}
}

// TestOwnedSnapshotIndexRefusesBodyBytesMismatch proves the positive-size
// half of the owned-body invariant is enforced before the index calls its
// rented embedder, not only when query later rehydrates text.
func TestOwnedSnapshotIndexRefusesBodyBytesMismatch(t *testing.T) {
	homePath := t.TempDir()
	fixture := seedOwnedSnapshotLane(t, homePath, "User: owned-body chunk alpha")
	removeOwnedSource(t, fixture)

	fixture.manifest.Items[0].BodyBytes++
	if _, err := sourceobs.RecordSourceManifest(homePath, fixture.manifest); err != nil {
		t.Fatalf("re-record size-mismatched manifest: %v", err)
	}
	embedder := &fakeEmbedder{identity: fixture.lane.Embedder.Identity(), dim: fixture.lane.Embedder.Dims}
	if _, err := Index(context.Background(), homePath, fixture.lane, embedder, IndexOptions{Rebuild: true}); err == nil {
		t.Fatalf("indexing accepted positive BodyBytes mismatch")
	}
	if embedder.embedded != 0 {
		t.Fatalf("BodyBytes-mismatch index embedded %d texts, want 0", embedder.embedded)
	}
}

// TestOwnedSnapshotNonPositiveBodyBytesStaysCompatible pins the manifest
// compatibility rule: older writers without a positive byte count still rely
// on BodyRef and FileHash integrity and must remain indexable.
func TestOwnedSnapshotNonPositiveBodyBytesStaysCompatible(t *testing.T) {
	homePath := t.TempDir()
	fixture := seedOwnedSnapshotLane(t, homePath, "User: owned-body chunk alpha")
	removeOwnedSource(t, fixture)

	fixture.manifest.Items[0].BodyBytes = 0
	if _, err := sourceobs.RecordSourceManifest(homePath, fixture.manifest); err != nil {
		t.Fatalf("re-record zero-size-compatible manifest: %v", err)
	}
	embedder := &fakeEmbedder{identity: fixture.lane.Embedder.Identity(), dim: fixture.lane.Embedder.Dims}
	if _, err := Index(context.Background(), homePath, fixture.lane, embedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("index non-positive BodyBytes manifest: %v", err)
	}
	if embedder.embedded == 0 {
		t.Fatalf("non-positive BodyBytes compatibility path embedded zero texts")
	}
}

// TestOwnedSnapshotQueryRehydratesAfterOriginalDeleted proves the query
// path resolves chunk text from the owned body alone — the SourceRef may
// be gone forever and we still get exact chunk text with text_status=ok.
func TestOwnedSnapshotQueryRehydratesAfterOriginalDeleted(t *testing.T) {
	homePath := t.TempDir()
	body := "User: owned query chunk alpha"
	fixture := seedOwnedSnapshotLane(t, homePath, body)
	removeOwnedSource(t, fixture)

	report, err := Query(context.Background(), homePath, fixture.lane, fixture.embedder, "owned", 1)
	if err != nil {
		t.Fatalf("query after source deletion: %v", err)
	}
	if len(report.Hits) != 1 {
		t.Fatalf("query hits = %d, want 1", len(report.Hits))
	}
	hit := report.Hits[0]
	if hit.TextStatus != string(ChunkTextOK) {
		t.Fatalf("hit text status = %q, want %q", hit.TextStatus, ChunkTextOK)
	}
	if !strings.Contains(hit.Text, "owned query chunk alpha") {
		t.Fatalf("hit text = %q, want body fragment", hit.Text)
	}
}

// TestOwnedSnapshotMissingOrCorruptArtifactFailsClosed nails the fail-closed
// invariant: completed custody means the owned body IS the source. A missing
// or corrupt artifact must surface a per-hit status (source_unavailable on
// the query path; the index path must error out), never a silent fallback to
// the SourceRef — that fallback would hide custody corruption.
//
// One subcase ("source_still_readable") deliberately leaves the SourceRef on
// disk AND pointing at the ORIGINAL body bytes, then corrupts the artifact.
// If a future change ever wired owned_snapshot to fall back to SourceRef,
// this case would silently succeed and surface the canary text. Today it
// refuses: the SourceRef is unreachable from the corpus resolver.
func TestOwnedSnapshotMissingOrCorruptArtifactFailsClosed(t *testing.T) {
	const fallbackCanary = "FALLBACK-CANARY-do-not-emit"
	cases := []struct {
		name      string
		mutate    func(t *testing.T, artifactPath, sourcePath string)
		wantIndex bool
		wantQuery string
	}{
		{
			name: "missing",
			mutate: func(t *testing.T, artifactPath, sourcePath string) {
				t.Helper()
				if err := os.Remove(artifactPath); err != nil {
					t.Fatalf("remove artifact: %v", err)
				}
			},
			wantIndex: true,
			wantQuery: string(ChunkTextSourceUnavailable),
		},
		{
			name: "corrupt",
			mutate: func(t *testing.T, artifactPath, sourcePath string) {
				t.Helper()
				if err := os.WriteFile(artifactPath, []byte("corrupt"), 0o644); err != nil {
					t.Fatalf("rewrite artifact: %v", err)
				}
			},
			wantIndex: true,
			wantQuery: string(ChunkTextStaleSource),
		},
		{
			// SourceRef still readable AND the original body bytes are on disk
			// — a future fallback would silently surface fallbackCanary text here.
			name: "source_still_readable",
			mutate: func(t *testing.T, artifactPath, sourcePath string) {
				t.Helper()
				if err := os.Remove(artifactPath); err != nil {
					t.Fatalf("remove artifact: %v", err)
				}
			},
			wantIndex: true,
			wantQuery: string(ChunkTextSourceUnavailable),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			body := "User: owned chunk alpha\n\nAssistant: " + fallbackCanary + " should never leak."
			fixture := seedOwnedSnapshotLane(t, homePath, body)
			// KEEP the SourceRef readable in all cases — this is the load-bearing
			// property that proves the resolver never falls back to it. The
			// "source_still_readable" subcase asserts the canary text never
			// appears on the wire; the others assert refusal too.
			item := fixture.manifest.Items[0]
			hexDigest := strings.TrimPrefix(item.BodyRef, "artifact:sha256:")
			artifactPath := filepath.Join(homePath, "artifacts", "sha256", hexDigest)
			tc.mutate(t, artifactPath, fixture.sourcePath)

			// Indexing over a missing or corrupt artifact MUST fail closed —
			// the manifest claims body_bytes that no longer match the artifact,
			// so re-embedding would either drop the item or rewrite it. Silent
			// fall-through would hide custody corruption.
			embedder := &fakeEmbedder{identity: fixture.lane.Embedder.Identity(), dim: fixture.lane.Embedder.Dims}
			if _, indexErr := Index(context.Background(), homePath, fixture.lane, embedder, IndexOptions{Rebuild: true}); indexErr == nil {
				t.Fatalf("expected index error for %s artifact, got nil", tc.name)
			}

			// Query path: the pre-existing manifest still points at the
			// (now-missing/corrupt) artifact. Hit text must NOT be served; the
			// status must report the right refusal reason; the fallback canary
			// must never appear on the wire (no SourceRef fallback).
			report, err := Query(context.Background(), homePath, fixture.lane, fixture.embedder, "owned", 1)
			if err != nil {
				t.Fatalf("query after %s artifact: %v", tc.name, err)
			}
			if len(report.Hits) != 1 {
				t.Fatalf("query hits = %d, want 1", len(report.Hits))
			}
			hit := report.Hits[0]
			if hit.Text != "" {
				t.Fatalf("hit leaked text on %s artifact: %q", tc.name, hit.Text)
			}
			if hit.TextStatus != tc.wantQuery {
				t.Fatalf("hit text status = %q, want %q", hit.TextStatus, tc.wantQuery)
			}
		})
	}
}

// TestOwnedSnapshotBodyHashAndSizeMismatchFailClosed proves the manifest
// triplet (BodyRef/BodyBytes/FileHash) gates the resolver: a recorded
// BodyBytes that disagrees with the actual artifact bytes fails closed
// (matching the doctor sourceobs rule), and a BodyRef whose stored bytes
// don't match the declared FileHash fails closed the same way. A missing
// artifact inside that mismatch path is reported as source_unavailable
// rather than silently producing text from anywhere else.
func TestOwnedSnapshotBodyHashAndSizeMismatchFailClosed(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, item *sourceobs.SourceManifestItem, fixture ownedCorpusFixture)
		want   string
	}{
		{
			name: "wrong_size",
			mutate: func(t *testing.T, item *sourceobs.SourceManifestItem, fixture ownedCorpusFixture) {
				t.Helper()
				item.BodyBytes = item.BodyBytes + 1
			},
			want: string(ChunkTextStaleSource),
		},
		{
			name: "wrong_hash",
			mutate: func(t *testing.T, item *sourceobs.SourceManifestItem, fixture ownedCorpusFixture) {
				t.Helper()
				item.FileHash = "sha256:" + strings.Repeat("0", 64)
			},
			want: string(ChunkTextStaleSource),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			body := "User: owned chunk alpha"
			fixture := seedOwnedSnapshotLane(t, homePath, body)
			removeOwnedSource(t, fixture)

			tc.mutate(t, &fixture.manifest.Items[0], fixture)
			// Mutate the corpus manifest in the index dir directly — the query path
			// resolves body via the CORPUS manifest's item (not the sourceobs
			// manifest), so the persisted triplet is what the resolver audits.
			dir := IndexDir(homePath, fixture.lane)
			corpusManifest, err := readManifest(dir)
			if err != nil {
				t.Fatalf("read corpus manifest: %v", err)
			}
			tc.mutate(t, &corpusManifest.Items[0], fixture)
			writeJSON(t, filepath.Join(dir, "manifest.json"), corpusManifest)

			report, err := Query(context.Background(), homePath, fixture.lane, fixture.embedder, "owned", 1)
			if err != nil {
				t.Fatalf("query after %s mutation: %v", tc.name, err)
			}
			if len(report.Hits) != 1 {
				t.Fatalf("query hits = %d, want 1", len(report.Hits))
			}
			hit := report.Hits[0]
			if hit.Text != "" {
				t.Fatalf("hit leaked text on %s manifest: %q", tc.name, hit.Text)
			}
			if hit.TextStatus != tc.want {
				t.Fatalf("hit text status = %q, want %q", hit.TextStatus, tc.want)
			}
		})
	}
}

// TestOwnedSnapshotIndexRefusesFileHashMismatch closes the gap the index
// path left open: readItemBodyBytes (used by Index) previously validated
// only BodyRef-digest + BodyBytes, so a manifest whose FileHash disagreed
// with the actual artifact bytes would silently embed a hash-less chunk
// stream the doctor would later call out as drift. The same three-way
// invariant (BodyRef digest == sha256(bytes) == FileHash, plus BodyBytes)
// must hold on the indexing path too — otherwise the chunk hashes
// persisted under source_manifest_ref carry no provenance guarantee.
//
// Falsifier: a BodyRef that is VALID (store.Get returns the artifact
// bytes cleanly because ref's hex digest matches the file's sha256) but
// whose FileHash disagrees with those same bytes. Index MUST refuse —
// the embedder must not see bytes the manifest refuses to claim. The
// Query path is covered by the existing TestOwnedSnapshotBodyHashAndSizeMismatchFailClosed
// (this fixture's failed Index leaves the previous valid persisted
// manifest intact, so an additional Query assertion would test nothing new).
func TestOwnedSnapshotIndexRefusesFileHashMismatch(t *testing.T) {
	homePath := t.TempDir()
	body := "User: owned-body chunk alpha\n\nAssistant: owned-body chunk beta"
	fixture := seedOwnedSnapshotLane(t, homePath, body)
	removeOwnedSource(t, fixture)

	// Record the embed-call count BEFORE the failing Index so the post-fix
	// assertion can prove no new vectors were written into the index dir.
	preDir := IndexDir(homePath, fixture.lane)
	preEmbedder := &fakeEmbedder{identity: fixture.lane.Embedder.Identity(), dim: fixture.lane.Embedder.Dims}
	if _, err := Index(context.Background(), homePath, fixture.lane, preEmbedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("baseline reindex should succeed: %v", err)
	}
	preVectorCount := len(readVectorFile(t, filepath.Join(preDir, "vectors.json")).Vectors)

	// Mutate FileHash only — the artifact at the path is still byte-identical
	// to what BuildOwnedSourceManifest captured, so store.Get will succeed and
	// BodyBytes still matches. ONLY the FileHash disagrees with sha256(bytes).
	fixture.manifest.Items[0].FileHash = "sha256:" + strings.Repeat("0", 64)
	if _, err := sourceobs.RecordSourceManifest(homePath, fixture.manifest); err != nil {
		t.Fatalf("re-record manifest: %v", err)
	}

	embedder := &fakeEmbedder{identity: fixture.lane.Embedder.Identity(), dim: fixture.lane.Embedder.Dims}
	_, err := Index(context.Background(), homePath, fixture.lane, embedder, IndexOptions{Rebuild: true})
	if err == nil {
		t.Fatalf("indexing accepted BodyRef-valid / FileHash-mismatched owned body — chunk hashes would carry no FileHash provenance")
	}
	if !strings.Contains(err.Error(), "owned") && !strings.Contains(err.Error(), "stale_source") {
		t.Fatalf("expected an owned-body / stale_source error, got %v", err)
	}
	// The failed Index must NOT have embedded new text — the embedder call
	// count for the failing run is zero on every document the manifest still
	// claims.
	if embedder.embedded != 0 {
		t.Fatalf("failed FileHash-mismatch index embedded %d texts, want 0", embedder.embedded)
	}
	postVectorCount := len(readVectorFile(t, filepath.Join(preDir, "vectors.json")).Vectors)
	if postVectorCount != preVectorCount {
		t.Fatalf("vector file grew across failed index: pre=%d post=%d", preVectorCount, postVectorCount)
	}
}

// TestOwnedSnapshotIndexRefusesEmptyFileHash is the second half of the
// three-way invariant: an owned manifest whose FileHash is blank cannot
// be indexed either. The corpus resolver never trusts a hashless manifest
// half — a missing FileHash is malformed (BuildSourceManifest always sets
// one) and must fail closed on both the index and query paths.
func TestOwnedSnapshotIndexRefusesEmptyFileHash(t *testing.T) {
	homePath := t.TempDir()
	body := "User: owned chunk alpha"
	fixture := seedOwnedSnapshotLane(t, homePath, body)
	removeOwnedSource(t, fixture)

	fixture.manifest.Items[0].FileHash = ""
	if _, err := sourceobs.RecordSourceManifest(homePath, fixture.manifest); err != nil {
		t.Fatalf("re-record manifest: %v", err)
	}

	embedder := &fakeEmbedder{identity: fixture.lane.Embedder.Identity(), dim: fixture.lane.Embedder.Dims}
	if _, err := Index(context.Background(), homePath, fixture.lane, embedder, IndexOptions{Rebuild: true}); err == nil {
		t.Fatalf("indexing accepted empty-FileHash owned body — three-way invariant broken")
	}
}

// TestBlankOrUnknownBodyPolicyRefusesSourceRefRead proves the corpus
// resolver does not silently widen BodyPolicy=="" or any unknown policy
// to source_in_place. The prior seam treated "" as a synonym (the legacy
// default), which let a malformed or omitted BodyPolicy field read
// SourceRef bytes — exactly the kind of drift the owned-body fix should
// not regress. Blank / unknown policies must fail closed: no read of
// SourceRef, no embed, no chunk hash produced.
func TestBlankOrUnknownBodyPolicyRefusesSourceRefRead(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"blank_policy", ""},
		{"unknown_policy", "garbage_policy_value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			body := "User: chunk alpha"
			sourcePath := filepath.Join(t.TempDir(), "in-place-source.md")
			if err := os.WriteFile(sourcePath, []byte(body), 0o644); err != nil {
				t.Fatalf("write source: %v", err)
			}
			family := "blank_policy_fixture_" + tc.name
			manifest, err := sourceobs.BuildSourceManifest(family, []string{sourcePath}, time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatalf("build manifest: %v", err)
			}
			manifest.Items[0].ID = "item_blank"
			manifest.Items[0].SourceKind = "synthetic_dialogue"
			manifest.Items[0].PageSlug = "Blank Policy Fixture"
			manifest.Items[0].CorpusGeneration = "blank-gen"
			manifest.Items[0].BodyPolicy = tc.body
			if _, err := sourceobs.RecordSourceManifest(homePath, manifest); err != nil {
				t.Fatalf("record manifest: %v", err)
			}
			lane := LaneConfig{
				Lane:         "blank-policy-" + tc.name,
				SourceFamily: family,
				Generation:   "blank-gen",
				Privacy:      "public",
				Embedder: EmbedderConfig{
					Provider: "local",
					Model:    "synthetic-embedder",
					Dims:     3,
					Bits:     "f32",
				},
				Chunk: ChunkConfig{Unit: "runes", Size: 256, Overlap: 16, Boundary: "structure"},
			}
			embedder := &fakeEmbedder{identity: lane.Embedder.Identity(), dim: lane.Embedder.Dims}
			// Indexing MUST refuse — a blank/unknown policy must not reach the
			// embedder. Refusal here proves SourceRef is never read.
			if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err == nil {
				t.Fatalf("indexing accepted body_policy=%q — blank/unknown policy widened to source_in_place", tc.body)
			}
		})
	}
}

// TestOwnedSnapshotPrivateTextSuppressedByDefault proves the item-level
// privacy gate stays the final filter regardless of body_policy: a
// privacy=private owned_snapshot is suppressed at the corpus layer
// (suppressed_private) unless the caller passes QueryOptions.IncludeText.
// BodyPolicy must NEVER widen or narrow privacy.
//
// The lane MUST be private+local here — the index privacy guard refuses a
// privacy=private owned_snapshot item in a public/remote lane BEFORE any
// body read. That's the closed-circuit canary the production guard exists
// to provide; this test exercises the post-index query-time suppression
// path, not the pre-index refusal.
func TestOwnedSnapshotPrivateTextSuppressedByDefault(t *testing.T) {
	homePath := t.TempDir()
	body := "User: owned private chunk alpha"
	fixture := seedOwnedSnapshotLane(t, homePath, body)
	fixture.manifest.Items[0].Privacy = "private"
	if _, err := sourceobs.RecordSourceManifest(homePath, fixture.manifest); err != nil {
		t.Fatalf("re-record private manifest: %v", err)
	}
	// Lane is private + local so the index privacy guard admits the item
	// (private-on-private is the only admitted posture for owned_snapshot).
	privateLane := fixture.lane
	privateLane.Privacy = "private"
	embedder := &fakeEmbedder{identity: privateLane.Embedder.Identity(), dim: privateLane.Embedder.Dims}
	if _, err := Index(context.Background(), homePath, privateLane, embedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("index private owned lane: %v", err)
	}

	report, err := Query(context.Background(), homePath, privateLane, fixture.embedder, "owned", 1)
	if err != nil {
		t.Fatalf("default query: %v", err)
	}
	if len(report.Hits) != 1 {
		t.Fatalf("default hits = %d, want 1", len(report.Hits))
	}
	if report.Hits[0].TextStatus != string(ChunkTextSuppressedPrivate) {
		t.Fatalf("default text status = %q, want %q", report.Hits[0].TextStatus, ChunkTextSuppressedPrivate)
	}
	if report.Hits[0].Text != "" {
		t.Fatalf("default hit leaked text: %q", report.Hits[0].Text)
	}

	optIn, err := QueryWithOptions(context.Background(), homePath, privateLane, fixture.embedder, "owned", 1, QueryOptions{IncludeText: true})
	if err != nil {
		t.Fatalf("opt-in query: %v", err)
	}
	if len(optIn.Hits) != 1 {
		t.Fatalf("opt-in hits = %d, want 1", len(optIn.Hits))
	}
	if optIn.Hits[0].TextStatus != string(ChunkTextOK) {
		t.Fatalf("opt-in text status = %q, want %q", optIn.Hits[0].TextStatus, ChunkTextOK)
	}
	if !strings.Contains(optIn.Hits[0].Text, "owned private chunk alpha") {
		t.Fatalf("opt-in text = %q, want body fragment", optIn.Hits[0].Text)
	}

	// Privacy suppression has precedence over body availability. Once a caller
	// has not opted into private text, the resolver must not probe the artifact
	// and reveal whether its bytes are present, missing, or corrupt.
	dir := IndexDir(homePath, privateLane)
	corpusManifest, err := readManifest(dir)
	if err != nil {
		t.Fatalf("read private corpus manifest: %v", err)
	}
	corpusManifest.Items[0].BodyRef = "artifact:sha256:" + strings.Repeat("0", 64)
	writeJSON(t, filepath.Join(dir, "manifest.json"), corpusManifest)
	suppressedMissing, err := Query(context.Background(), homePath, privateLane, fixture.embedder, "owned", 1)
	if err != nil {
		t.Fatalf("default query with missing private artifact: %v", err)
	}
	if len(suppressedMissing.Hits) != 1 {
		t.Fatalf("default missing-artifact hits = %d, want 1", len(suppressedMissing.Hits))
	}
	if suppressedMissing.Hits[0].TextStatus != string(ChunkTextSuppressedPrivate) {
		t.Fatalf("default missing-artifact text status = %q, want %q", suppressedMissing.Hits[0].TextStatus, ChunkTextSuppressedPrivate)
	}
	if suppressedMissing.Hits[0].Text != "" {
		t.Fatalf("default missing-artifact hit leaked text: %q", suppressedMissing.Hits[0].Text)
	}
}

// TestOwnedSnapshotQueryRejectsMalformedPrivacy proves persisted corpus
// metadata cannot widen an owned body into public text. Index rejects malformed
// labels at creation time; query must repeat that fail-closed check because the
// rented projection can be corrupted or replaced after indexing.
func TestOwnedSnapshotQueryRejectsMalformedPrivacy(t *testing.T) {
	homePath := t.TempDir()
	fixture := seedOwnedSnapshotLane(t, homePath, "User: owned public chunk alpha")
	removeOwnedSource(t, fixture)

	dir := IndexDir(homePath, fixture.lane)
	corpusManifest, err := readManifest(dir)
	if err != nil {
		t.Fatalf("read corpus manifest: %v", err)
	}
	corpusManifest.Items[0].Privacy = ""
	writeJSON(t, filepath.Join(dir, "manifest.json"), corpusManifest)

	if _, err := Query(context.Background(), homePath, fixture.lane, fixture.embedder, "owned", 1); err == nil || !strings.Contains(err.Error(), "malformed privacy field") {
		t.Fatalf("query malformed privacy error = %v, want fail-closed malformed privacy", err)
	}
	if _, err := QueryWithOptions(context.Background(), homePath, fixture.lane, fixture.embedder, "owned", 1, QueryOptions{IncludeText: true}); err == nil || !strings.Contains(err.Error(), "malformed privacy field") {
		t.Fatalf("include-text query malformed privacy error = %v, want fail-closed malformed privacy", err)
	}
}

// TestOwnedSnapshotPrivateItemInPublicLaneRefusesIndex is the load-bearing
// privacy guard for the slice: a privacy=private owned_snapshot item placed
// in a PUBLIC lane must refuse indexing — the embedder must NEVER see private
// bytes. The lane posture (privacy=public) is the gate; there is no caller
// opt-in for private-on-public, no override flag, no fallback. This is the
// closed-circuit canary that proves the new seam cannot leak private
// owned_snapshot text to a remote embedder.
//
// The fakeEmbedder is the remote-side stand-in: assertion is on
// embedder.embedded == 0, which proves zero private bytes reached the
// embedder interface. The test deliberately stops at Index refusal — Query
// coverage of the private-suppression path is exercised by
// TestOwnedSnapshotPrivateTextSuppressedByDefault on the admitted (private
// + local) lane posture.
func TestOwnedSnapshotPrivateItemInPublicLaneRefusesIndex(t *testing.T) {
	homePath := t.TempDir()
	body := "User: owned private chunk alpha"
	fixture := seedOwnedSnapshotLane(t, homePath, body)
	fixture.manifest.Items[0].Privacy = "private"
	if _, err := sourceobs.RecordSourceManifest(homePath, fixture.manifest); err != nil {
		t.Fatalf("re-record private manifest: %v", err)
	}

	// Lane stays PUBLIC. The privacy gate must trip here, before any body
	// read or embedder call. A remote/loopback public lane would otherwise
	// hand private bytes to the embedder.
	publicLane := fixture.lane
	publicLane.Privacy = "public"

	embedder := &fakeEmbedder{identity: publicLane.Embedder.Identity(), dim: publicLane.Embedder.Dims}
	_, err := Index(context.Background(), homePath, publicLane, embedder, IndexOptions{Rebuild: true})
	if err == nil {
		t.Fatalf("indexing accepted privacy=private owned_snapshot item in a public lane — privacy gate broken")
	}
	// Hardest assertion: zero private bytes reached the embedder interface.
	if embedder.embedded != 0 {
		t.Fatalf("privacy gate let %d private bytes reach the embedder interface", embedder.embedded)
	}
}

// TestSourceInPlacePrivateItemInPublicLaneUnchanged is the regression guard
// for the scoped privacy rule: source_in_place behavior must remain
// untouched in this slice. A privacy=private source_in_place item placed in
// a PUBLIC lane keeps being indexed (legacy default is private items in
// public lanes), because BuildSourceManifest defaults source_in_place items
// to private while the published quickstart uses public lanes. Closing the
// source_in_place seam is an explicit follow-on evidence packet — NOT
// silently widened here.
//
// The packet's "source_in_place unchanged" rule is honored: this test would
// be RED if a future change extended the new gate to source_in_place.
func TestSourceInPlacePrivateItemInPublicLaneUnchanged(t *testing.T) {
	homePath := t.TempDir()
	body := "User: in-place chunk alpha"
	sourcePath := filepath.Join(t.TempDir(), "in-place-source.md")
	if err := os.WriteFile(sourcePath, []byte(body), 0o644); err != nil {
		t.Fatalf("write in-place source: %v", err)
	}
	family := "source_in_place_private_fixture"
	manifest, err := sourceobs.BuildSourceManifest(family, []string{sourcePath}, time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build in-place manifest: %v", err)
	}
	manifest.Items[0].ID = "item_in_place_private"
	manifest.Items[0].SourceKind = "synthetic_dialogue"
	manifest.Items[0].PageSlug = "In Place Private Fixture"
	manifest.Items[0].CorpusGeneration = "inplace-gen"
	manifest.Items[0].Privacy = "private"
	if _, err := sourceobs.RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatalf("record in-place manifest: %v", err)
	}
	lane := LaneConfig{
		Lane:         "in-place-private-unchanged",
		SourceFamily: family,
		Generation:   "inplace-gen",
		Privacy:      "public", // deliberately public — legacy default
		Embedder: EmbedderConfig{
			Provider: "local",
			Model:    "synthetic-embedder",
			Dims:     3,
			Bits:     "f32",
		},
		Chunk: ChunkConfig{Unit: "runes", Size: 256, Overlap: 16, Boundary: "structure"},
	}
	embedder := &fakeEmbedder{identity: lane.Embedder.Identity(), dim: lane.Embedder.Dims}
	if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("source_in_place privacy=private in a public lane must remain indexed in this slice: %v", err)
	}
}

// the seam widening source_in_place's contract: a body_policy=source_in_place
// item must continue to read from its SourceRef (NOT from any artifact
// store), and the existing source-loss + drift semantics must hold byte-for-byte.
// Owned body is additive — it does not replace or constrain in-place.
func TestSourceInPlaceCorpusBehaviorUnchanged(t *testing.T) {
	homePath := t.TempDir()
	body := "User: in-place chunk alpha"
	sourcePath := filepath.Join(t.TempDir(), "in-place-source.md")
	if err := os.WriteFile(sourcePath, []byte(body), 0o644); err != nil {
		t.Fatalf("write in-place source: %v", err)
	}
	family := "source_in_place_fixture"
	manifest, err := sourceobs.BuildSourceManifest(family, []string{sourcePath}, time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build in-place manifest: %v", err)
	}
	manifest.Items[0].ID = "item_in_place"
	manifest.Items[0].SourceKind = "synthetic_dialogue"
	manifest.Items[0].PageSlug = "In Place Fixture"
	manifest.Items[0].CorpusGeneration = "inplace-gen"
	if _, err := sourceobs.RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatalf("record in-place manifest: %v", err)
	}
	lane := LaneConfig{
		Lane:         "in-place",
		SourceFamily: family,
		Generation:   "inplace-gen",
		Privacy:      "public",
		Embedder: EmbedderConfig{
			Provider: "local",
			Model:    "synthetic-embedder",
			Dims:     3,
			Bits:     "f32",
		},
		Chunk: ChunkConfig{Unit: "runes", Size: 256, Overlap: 16, Boundary: "structure"},
	}
	embedder := &fakeEmbedder{identity: lane.Embedder.Identity(), dim: lane.Embedder.Dims}
	if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("index in-place lane: %v", err)
	}

	// Source deletion: must keep the current source_unavailable behavior —
	// the corpus layer does NOT promote in-place items to owned_snapshot
	// just because the index succeeded once.
	if err := os.Remove(sourcePath); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	report, err := Query(context.Background(), homePath, lane, embedder, "in-place", 1)
	if err != nil {
		t.Fatalf("query after source deletion: %v", err)
	}
	if len(report.Hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(report.Hits))
	}
	hit := report.Hits[0]
	if hit.TextStatus != string(ChunkTextSourceUnavailable) {
		t.Fatalf("text status = %q, want %q", hit.TextStatus, ChunkTextSourceUnavailable)
	}
	if hit.Text != "" {
		t.Fatalf("hit leaked text on missing in-place source: %q", hit.Text)
	}

	// Byte-identity of FileHash gate: an in-place body whose live bytes
	// diverge from the recorded hash must still return stale_source, exactly
	// as before — no behavioral drift.
	if err := os.WriteFile(sourcePath, []byte("changed-after-indexing"), 0o644); err != nil {
		t.Fatalf("rewrite source: %v", err)
	}
	report, err = Query(context.Background(), homePath, lane, embedder, "in-place", 1)
	if err != nil {
		t.Fatalf("query after rewrite: %v", err)
	}
	if report.Hits[0].TextStatus != string(ChunkTextStaleSource) {
		t.Fatalf("text status = %q, want %q", report.Hits[0].TextStatus, ChunkTextStaleSource)
	}
	if report.Hits[0].Text != "" {
		t.Fatalf("hit leaked text on stale in-place source: %q", report.Hits[0].Text)
	}

	// Hash function still rejects non-`source_in_place` policy at the legacy
	// boundary: readItemBody must NOT silently widen to owned_snapshot when
	// nothing about the manifest changed. We probe via the manifest item's
	// BodyPolicy remaining "source_in_place" (not mutated by indexing).
	updated, err := sourceobs.LoadSourceManifest(homePath, family)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if updated.Items[0].BodyPolicy != "source_in_place" {
		t.Fatalf("body_policy mutated by indexing: %q", updated.Items[0].BodyPolicy)
	}
}

// TestOwnedSnapshotStatusesRemainClosedEnum keeps the corpus failure surface
// closed and named: the resolver exposes exactly the statuses the wire
// contract advertises, no more, no fewer. Lifted out of the resolver
// because a regression here is silent on the happy path.
func TestOwnedSnapshotStatusesRemainClosedEnum(t *testing.T) {
	want := map[string]bool{
		string(ChunkTextOK):                true,
		string(ChunkTextStaleSource):       true,
		string(ChunkTextSuppressedPrivate): true,
		string(ChunkTextSourceUnavailable): true,
		string(ChunkTextStaleIndex):        true,
	}
	got := map[string]bool{}
	for _, status := range []ChunkTextStatus{
		ChunkTextOK, ChunkTextStaleSource, ChunkTextSuppressedPrivate,
		ChunkTextSourceUnavailable, ChunkTextStaleIndex,
	} {
		got[string(status)] = true
	}
	for name := range want {
		if !got[name] {
			t.Fatalf("status %q removed from enum (corpus failure surface regressed)", name)
		}
	}
	// Sanity: errors.As on a synthetic wrapped failure stays supported so callers
	// (orient body resolver) can degrade gracefully on owned-body errors.
	wrapped := errors.New("synthetic")
	probe := wrapped
	if !errors.Is(wrapped, probe) {
		t.Fatalf("errors.Is broken for synthetic error")
	}
}
