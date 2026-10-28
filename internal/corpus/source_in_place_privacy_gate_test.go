// source_in_place_privacy_gate_test.go covers the explicit opt-in
// SourceInPlacePrivacyGate on LaneConfig. The default posture is off so
// documented legacy behavior — BuildSourceManifest emits source_in_place
// items with privacy=private while the published quickstart runs public
// lanes — keeps working unchanged. When the operator flips the gate on,
// the corpus resolver refuses a privacy=private source_in_place item
// unless the lane posture is privacy=private + local/loopback embedder,
// matching the predicate the owned_snapshot path already uses. The
// refusal happens BEFORE readItemBody is called, which is the load-bearing
// claim this evidence packet makes: zero private source_in_place bytes
// ever reach the rented embedder.
//
// Red-first falsifiers below are written against the unpatched semantics
// and the patched semantics in the same file. Each named scenario's
// "before / after" pair is explicit so a future change to the gate
// surface — widening, narrowing, or relocating — fails the falsifier
// that proves the contract it removed.
//
// Note on layering: a lane with privacy=private + non-local embedder is
// already refused by ValidateLaneConfig before our gate ever runs —
// that is a lane-level invariant. The gate's per-item job is the second
// layer, catching private source_in_place items inside a public lane
// posture that the lane validator admitted. Tests below stay inside the
// gate's jurisdiction so each can pin a single layer's contract.
package corpus

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/mem"
	"compound/internal/sourceobs"
	"gopkg.in/yaml.v3"
)

// seedSourceInPlaceLaneForGate builds a single-item corpus lane over a
// body_policy=source_in_place manifest so each gate test can pin its
// own lane posture and embedder. Returns the lane, the embedder (so
// tests can assert the embedder was never invoked), and the body path
// (so a test can prove the gate ran before any read by removing it).
// itemPrivacy overrides BuildSourceManifest's default privacy=private
// so tests can pin both halves of the contract.
func seedSourceInPlaceLaneForGate(t *testing.T, homePath, body, family string, itemPrivacy, lanePrivacy, embedderProvider, embedderBaseURL string, gateEnabled bool) (LaneConfig, *fakeEmbedder, string) {
	t.Helper()
	bodyPath := filepath.Join(t.TempDir(), "gate-source.md")
	if err := os.WriteFile(bodyPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write gate source: %v", err)
	}
	manifest, err := sourceobs.BuildSourceManifest(family, []string{bodyPath}, time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build source manifest: %v", err)
	}
	manifest.Items[0].ID = "item_gate"
	manifest.Items[0].SourceKind = "synthetic_dialogue"
	manifest.Items[0].PageSlug = "Source In Place Gate Fixture"
	manifest.Items[0].CorpusGeneration = "gate-gen"
	manifest.Items[0].Privacy = itemPrivacy
	if _, err := sourceobs.RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatalf("record source manifest: %v", err)
	}
	lane := LaneConfig{
		Lane:         "gate-" + family,
		SourceFamily: family,
		Generation:   "gate-gen",
		Privacy:      lanePrivacy,
		Embedder: EmbedderConfig{
			Provider: embedderProvider,
			Model:    "synthetic-embedder",
			Dims:     3,
			Bits:     "f32",
			BaseURL:  embedderBaseURL,
		},
		Chunk:                    ChunkConfig{Unit: "runes", Size: 256, Overlap: 16, Boundary: "structure"},
		SourceInPlacePrivacyGate: gateEnabled,
	}
	embedder := &fakeEmbedder{identity: lane.Embedder.Identity(), dim: lane.Embedder.Dims}
	return lane, embedder, bodyPath
}

// TestSourceInPlacePrivacyGateDefaultOffAllowsLegacyQuickstart pins the
// load-bearing default-off contract: BuildSourceManifest defaults
// source_in_place items to privacy=private, and the published quickstart
// runs public lanes. With the gate off, indexing MUST succeed so the
// documented legacy behavior is preserved bit-for-bit. Any future change
// that flips the default to engaged would break this falsifier and
// therefore the quickstart — exactly what the audit packet forbids.
func TestSourceInPlacePrivacyGateDefaultOffAllowsLegacyQuickstart(t *testing.T) {
	homePath := t.TempDir()
	lane, embedder, _ := seedSourceInPlaceLaneForGate(t, homePath,
		"User: legacy public-lane private-source-in-place text.",
		"gate_default_off",
		"private", // item privacy — BuildSourceManifest default
		"public",  // lane privacy
		"local",   // embedder provider
		"",        // embedder base url
		false,     // gate off — legacy contract
	)
	if SourceInPlacePrivacyGateEnabled(lane) {
		t.Fatalf("default-off contract broken: SourceInPlacePrivacyGateEnabled = true")
	}
	if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("default-off index refused: %v", err)
	}
	if embedder.embedded == 0 {
		t.Fatalf("default-off index embedded zero texts — gate is silently narrowing legacy behavior")
	}
}

// TestSourceInPlacePrivacyGateEnabledRefusesPrivateInPublicLane is the
// load-bearing refusal canary: gate on, item privacy=private, lane
// privacy=public — the index MUST refuse and the embedder MUST NOT be
// called. This is the exact shape that would leak private bytes to a
// rented embedder under a public lane.
func TestSourceInPlacePrivacyGateEnabledRefusesPrivateInPublicLane(t *testing.T) {
	homePath := t.TempDir()
	lane, embedder, _ := seedSourceInPlaceLaneForGate(t, homePath,
		"User: private source_in_place should refuse in public lane.",
		"gate_priv_pub",
		"private", // item privacy
		"public",  // lane privacy
		"local",
		"",
		true,
	)
	_, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true})
	if err == nil {
		t.Fatalf("indexing accepted privacy=private source_in_place in public lane — leak gate closed-circuit canary broken")
	}
	if !strings.Contains(err.Error(), "privacy=private refuses non-private lane") {
		t.Fatalf("refusal error = %q, want privacy=private refuses non-private lane", err.Error())
	}
	if embedder.embedded != 0 {
		t.Fatalf("embedder received %d texts despite refusal — private bytes leaked to rented embedder", embedder.embedded)
	}
}

// TestSourceInPlacePrivacyGateEnabledRefusesPrivateInPublicRemoteLane
// closes the remote-embedder half of the leak: gate on, lane privacy
// stays PUBLIC (so the lane validator admits the lane), but embedder is
// remote. ProviderIsLocal returns false and laneIsPrivateLocal is also
// false (lane is public), so the gate refuses — proving the per-item
// predicate honors the embedder half independent of the lane-privacy
// half. A successful index here would prove the gate ran with a partial
// predicate.
func TestSourceInPlacePrivacyGateEnabledRefusesPrivateInPublicRemoteLane(t *testing.T) {
	homePath := t.TempDir()
	lane, embedder, _ := seedSourceInPlaceLaneForGate(t, homePath,
		"User: private source_in_place must not reach remote embedder even with public lane.",
		"gate_priv_pub_remote",
		"private",
		"public", // lane stays public so lane validator passes
		"openai",
		"https://api.openai.example/v1",
		true,
	)
	_, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true})
	if err == nil {
		t.Fatalf("indexing accepted privacy=private source_in_place with remote embedder — gate ignored ProviderIsLocal")
	}
	if !strings.Contains(err.Error(), "privacy=private refuses non-private lane") {
		t.Fatalf("refusal error = %q, want privacy=private refuses non-private lane", err.Error())
	}
	if embedder.embedded != 0 {
		t.Fatalf("embedder received %d texts despite refusal — private bytes leaked to remote", embedder.embedded)
	}
}

// TestSourceInPlacePrivacyGateEnabledAllowsPrivateInPrivateLocalLane
// proves the admitted posture: gate on, lane privacy=private, local
// embedder — the only configuration where a private source_in_place
// item is allowed past the gate. Removing this positive case would
// silently make the gate always-on, which the default-off contract
// forbids.
func TestSourceInPlacePrivacyGateEnabledAllowsPrivateInPrivateLocalLane(t *testing.T) {
	homePath := t.TempDir()
	lane, embedder, _ := seedSourceInPlaceLaneForGate(t, homePath,
		"User: admitted private source_in_place local lane.",
		"gate_priv_local",
		"private",
		"private",
		"local",
		"",
		true,
	)
	report, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true})
	if err != nil {
		t.Fatalf("admitted posture refused: %v", err)
	}
	if report.Items != 1 || report.TotalChunks == 0 {
		t.Fatalf("admitted report = %+v, want 1 item with chunks", report)
	}
	if embedder.embedded == 0 {
		t.Fatalf("admitted embedder embedded zero texts — gate is too strict")
	}
}

// TestSourceInPlacePrivacyGateEnabledAllowsPublicItemInAnyLane keeps
// the public half of the contract visible: gate on, item privacy=public
// must succeed regardless of lane posture. This is the negative-control
// partner to the refusal canary — if it ever flips to refuse, the gate
// widened silently and the operator's opt-in is meaningless. Lane
// privacy is held public in every subcase so the lane validator admits
// the lane; the gate's per-item check is what we want to prove.
func TestSourceInPlacePrivacyGateEnabledAllowsPublicItemInAnyLane(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		baseURL  string
	}{
		{"public_item_local_embedder", "local", ""},
		{"public_item_remote_embedder", "openai", "https://api.openai.example/v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			lane, embedder, _ := seedSourceInPlaceLaneForGate(t, homePath,
				"User: public source_in_place may ride any lane.",
				"gate_pub_any",
				"public", // item privacy
				"public", // lane privacy
				tc.provider,
				tc.baseURL,
				true,
			)
			if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err != nil {
				t.Fatalf("public item refused in %s lane: %v", tc.name, err)
			}
			if embedder.embedded == 0 {
				t.Fatalf("public item embedded zero texts in %s lane", tc.name)
			}
		})
	}
}

// TestSourceInPlacePrivacyGateEnabledRefusesMalformedPrivacy closes
// the validation half of the gate. The owned_snapshot gate already
// refuses a blank/garbage privacy field; the source_in_place gate must
// do the same so a rented projection can never widen a malformed item
// to "public" by accident. Body must not be read on the way to refusal.
// Inputs are chosen so neither case-insensitive match resolves to
// "public" or "private" — only the malformed-default branch can fire.
func TestSourceInPlacePrivacyGateEnabledRefusesMalformedPrivacy(t *testing.T) {
	for _, privacy := range []string{"", "internal", "PRIVATEr"} {
		t.Run("privacy="+privacy, func(t *testing.T) {
			homePath := t.TempDir()
			lane, embedder, _ := seedSourceInPlaceLaneForGate(t, homePath,
				"User: malformed privacy should fail closed.",
				"gate_malformed_"+strings.ReplaceAll(strings.TrimSpace(privacy), " ", "_"),
				privacy, // item privacy — drives the malformed branch
				"public",
				"local",
				"",
				true,
			)
			_, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true})
			if err == nil {
				t.Fatalf("indexing accepted malformed privacy %q — gate widened instead of fail-closing", privacy)
			}
			if !strings.Contains(err.Error(), "malformed privacy field") {
				t.Fatalf("refusal error = %q, want malformed privacy field", err.Error())
			}
			if embedder.embedded != 0 {
				t.Fatalf("embedder received %d texts on malformed privacy — gate ran after body read", embedder.embedded)
			}
		})
	}
}

// TestSourceInPlacePrivacyGateEnabledBodyReaderNotInvokedForRefused
// is the strongest falsifier the packet makes: the gate MUST refuse
// before readItemBody touches the on-disk source. The canary deletes
// the source file between seeding and indexing — if the gate ran
// after body read, the index would surface a "source_unavailable" /
// "source missing" error rather than the gate's refusal error. A green
// run here proves the gate is structural, ahead of any body read.
func TestSourceInPlacePrivacyGateEnabledBodyReaderNotInvokedForRefused(t *testing.T) {
	homePath := t.TempDir()
	lane, embedder, bodyPath := seedSourceInPlaceLaneForGate(t, homePath,
		"User: gate must run before body read.",
		"gate_body_read_canary",
		"private",
		"public",
		"local",
		"",
		true,
	)
	if err := os.Remove(bodyPath); err != nil {
		t.Fatalf("remove body to prove gate runs first: %v", err)
	}
	_, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true})
	if err == nil {
		t.Fatalf("indexing succeeded with body missing — gate cannot be ahead of body read")
	}
	if strings.Contains(err.Error(), "source_unavailable") || strings.Contains(err.Error(), "manifest item") {
		t.Fatalf("gate ran AFTER body read: %v", err)
	}
	if !strings.Contains(err.Error(), "privacy=private refuses non-private lane") {
		t.Fatalf("expected gate refusal error, got %v", err)
	}
	if embedder.embedded != 0 {
		t.Fatalf("embedder received %d texts despite gate refusal", embedder.embedded)
	}
}

// TestSourceInPlacePrivacyGateEnabledEmbedderNotCalledForRefused pins
// the second half of the canary: even with the source file present (so
// body read would succeed), the gate refusal must reach the index
// driver without ever invoking the embedder. The fake embedder records
// `embedded` calls; a non-zero count is unambiguous evidence the gate
// is positioned wrong.
func TestSourceInPlacePrivacyGateEnabledEmbedderNotCalledForRefused(t *testing.T) {
	homePath := t.TempDir()
	lane, embedder, _ := seedSourceInPlaceLaneForGate(t, homePath,
		"User: embedder must never see these bytes.",
		"gate_embedder_canary",
		"private",
		"public",
		"local",
		"",
		true,
	)
	_, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true})
	if err == nil {
		t.Fatalf("indexing accepted private source_in_place in public lane — gate missing entirely")
	}
	if embedder.calls != 0 || embedder.embedded != 0 {
		t.Fatalf("embedder invoked despite refusal: calls=%d embedded=%d", embedder.calls, embedder.embedded)
	}
}

// TestSourceInPlacePrivacyGateClosesLegacyIndexAtQuery is the red-first
// falsifier for the review finding: an index built under the legacy default
// cannot become a privacy bypass when the lane gate is enabled later.
func TestSourceInPlacePrivacyGateClosesLegacyIndexAtQuery(t *testing.T) {
	homePath := t.TempDir()
	lane, embedder, _ := seedSourceInPlaceLaneForGate(t, homePath, "private legacy body", "legacy_then_gated", "private", "public", "local", "", false)
	if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("legacy index: %v", err)
	}
	embedder.calls = 0
	embedder.embedded = 0
	engineCalls := 0
	originalQuery := runCorpusEngineQuery
	runCorpusEngineQuery = func(_ *mem.ProtocolEngine, _ string, _ int) ([]mem.Hit, error) {
		engineCalls++
		return nil, nil
	}
	t.Cleanup(func() { runCorpusEngineQuery = originalQuery })
	sourceReads := 0
	originalRead := readSourceInPlaceBody
	readSourceInPlaceBody = func(path string) ([]byte, error) {
		sourceReads++
		return originalRead(path)
	}
	t.Cleanup(func() { readSourceInPlaceBody = originalRead })

	lane.SourceInPlacePrivacyGate = true
	_, err := QueryWithOptions(context.Background(), homePath, lane, embedder, "private", 1, QueryOptions{IncludeText: true})
	if err == nil || !strings.Contains(err.Error(), "privacy=private refuses non-private lane") {
		t.Fatalf("gated query error = %v, want privacy refusal", err)
	}
	if engineCalls != 0 || embedder.calls != 0 {
		t.Fatalf("query calls: engine=%d embedder=%d, want zero/zero", engineCalls, embedder.calls)
	}
	if sourceReads != 0 {
		t.Fatalf("query read source_in_place body %d times, want zero", sourceReads)
	}

	indexed, err := readManifest(IndexDir(homePath, lane))
	if err != nil {
		t.Fatalf("read indexed manifest: %v", err)
	}
	_, err = ResolveChunkText(homePath, indexed, indexed.Items[0].ID+"#0", true, lane)
	if err == nil || !strings.Contains(err.Error(), "privacy=private refuses non-private lane") {
		t.Fatalf("IncludeText resolver error = %v, want privacy refusal", err)
	}
	if sourceReads != 0 {
		t.Fatalf("IncludeText resolver read source_in_place body %d times, want zero", sourceReads)
	}
}

func TestSourceInPlacePrivacyGateQueryUsesCurrentManifestPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		privacy string
		want    string
	}{
		{name: "changed_to_private", privacy: "private", want: "privacy=private refuses non-private lane"},
		{name: "malformed_fails_closed", privacy: "restricted", want: "malformed privacy field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			lane, embedder, _ := seedSourceInPlaceLaneForGate(t, homePath, "current privacy body", "current_privacy_"+tc.name, "public", "public", "local", "", true)
			if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err != nil {
				t.Fatalf("index public item: %v", err)
			}
			current, err := sourceobs.LoadSourceManifest(homePath, lane.SourceFamily)
			if err != nil {
				t.Fatalf("load current manifest: %v", err)
			}
			current.Items[0].Privacy = tc.privacy
			writeJSON(t, sourceobs.SourceManifestPath(homePath, lane.SourceFamily), current)

			embedder.calls = 0
			engineCalls := 0
			originalQuery := runCorpusEngineQuery
			runCorpusEngineQuery = func(_ *mem.ProtocolEngine, _ string, _ int) ([]mem.Hit, error) {
				engineCalls++
				return nil, nil
			}
			t.Cleanup(func() { runCorpusEngineQuery = originalQuery })
			sourceReads := 0
			originalRead := readSourceInPlaceBody
			readSourceInPlaceBody = func(path string) ([]byte, error) {
				sourceReads++
				return originalRead(path)
			}
			t.Cleanup(func() { readSourceInPlaceBody = originalRead })

			_, err = QueryWithOptions(context.Background(), homePath, lane, embedder, "current", 1, QueryOptions{IncludeText: true})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("query error = %v, want %q", err, tc.want)
			}
			if engineCalls != 0 || embedder.calls != 0 || sourceReads != 0 {
				t.Fatalf("refused query calls: engine=%d embedder=%d source_reads=%d, want zero/zero/zero", engineCalls, embedder.calls, sourceReads)
			}
		})
	}
}

func TestSourceInPlacePrivacyGateDefaultOffQueryCompatibility(t *testing.T) {
	homePath := t.TempDir()
	lane, embedder, _ := seedSourceInPlaceLaneForGate(t, homePath, "legacy readable body", "default_off_query", "private", "public", "local", "", false)
	if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("legacy index: %v", err)
	}
	embedder.calls = 0
	report, err := QueryWithOptions(context.Background(), homePath, lane, embedder, "readable", 1, QueryOptions{IncludeText: true})
	if err != nil {
		t.Fatalf("default-off legacy query: %v", err)
	}
	if embedder.calls != 1 {
		t.Fatalf("default-off query embedder calls = %d, want 1", embedder.calls)
	}
	if len(report.Hits) != 1 || report.Hits[0].Text == "" {
		t.Fatalf("default-off IncludeText report = %+v, want readable hit", report.Hits)
	}
}

func TestSourceInPlacePrivacyGateYAMLRoundTrip(t *testing.T) {
	base := LaneConfig{Lane: "yaml-lane", SourceFamily: "yaml-family", Privacy: "public"}
	omitted, err := yaml.Marshal(base)
	if err != nil {
		t.Fatalf("marshal omitted gate: %v", err)
	}
	if strings.Contains(string(omitted), "source_in_place_privacy_gate") {
		t.Fatalf("false gate was not omitted from YAML:\n%s", omitted)
	}
	var decoded LaneConfig
	if err := yaml.Unmarshal(omitted, &decoded); err != nil {
		t.Fatalf("unmarshal omitted gate: %v", err)
	}
	if decoded.SourceInPlacePrivacyGate {
		t.Fatal("omitted YAML gate decoded enabled")
	}

	base.SourceInPlacePrivacyGate = true
	enabled, err := yaml.Marshal(base)
	if err != nil {
		t.Fatalf("marshal enabled gate: %v", err)
	}
	if !strings.Contains(string(enabled), "source_in_place_privacy_gate: true") {
		t.Fatalf("enabled gate absent from YAML:\n%s", enabled)
	}
	decoded = LaneConfig{}
	if err := yaml.Unmarshal(enabled, &decoded); err != nil {
		t.Fatalf("unmarshal enabled gate: %v", err)
	}
	if !decoded.SourceInPlacePrivacyGate {
		t.Fatal("enabled YAML gate did not round-trip")
	}
}

func TestSourceInPlacePrivacyGateSuppressesBeforeBodyRead(t *testing.T) {
	homePath := t.TempDir()
	lane, embedder, _ := seedSourceInPlaceLaneForGate(t, homePath, "locally permitted private body", "suppress_before_read", "private", "private", "local", "", true)
	if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("index private local item: %v", err)
	}
	indexed, err := readManifest(IndexDir(homePath, lane))
	if err != nil {
		t.Fatalf("read indexed manifest: %v", err)
	}
	sourceReads := 0
	originalRead := readSourceInPlaceBody
	readSourceInPlaceBody = func(path string) ([]byte, error) {
		sourceReads++
		return originalRead(path)
	}
	t.Cleanup(func() { readSourceInPlaceBody = originalRead })

	resolved, err := ResolveChunkText(homePath, indexed, indexed.Items[0].ID+"#0", false, lane)
	if err != nil {
		t.Fatalf("resolve suppressed text: %v", err)
	}
	if resolved.Status != ChunkTextSuppressedPrivate || resolved.Text != "" {
		t.Fatalf("resolved = %+v, want suppressed without text", resolved)
	}
	if sourceReads != 0 {
		t.Fatalf("suppression read source_in_place body %d times, want zero", sourceReads)
	}
}

// TestSourceInPlacePrivacyGateQueryPropagatesCurrentPrivacyToBodyResolution
// is the red-first falsifier for the residual privacy defect: an item
// indexed while its privacy was public is reclassified to private in the
// current SourceManifest after indexing. With the gate enabled, the
// permitted private/local lane posture, and IncludeText=false, the very
// next query must:
//   - reflect the current (private) classification in its hit text status,
//   - emit zero body text on the returned hit, and
//   - never touch the on-disk source body.
//
// This test reaches the production-default path (QueryWithOptions) end to
// end so the bug being pinned is "the indexed privacy snapshot is leaking
// into body resolution", not "the per-call resolver is correctly gated
// when fed a fresh manifest". A patched validateQueryItemAccess must
// propagate the current privacy classification into the manifest the
// resolver reads; otherwise the indexed item's stale privacy=public
// would silently pass the governed switch and the source read would
// proceed.
func TestSourceInPlacePrivacyGateQueryPropagatesCurrentPrivacyToBodyResolution(t *testing.T) {
	homePath := t.TempDir()
	lane, embedder, _ := seedSourceInPlaceLaneForGate(t, homePath,
		"indexed-as-public body that is now private at the authority",
		"propagate_current_privacy",
		"public", // indexed-time privacy
		"private",
		"local",
		"",
		true,
	)
	if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("index public item: %v", err)
	}
	current, err := sourceobs.LoadSourceManifest(homePath, lane.SourceFamily)
	if err != nil {
		t.Fatalf("load current manifest: %v", err)
	}
	current.Items[0].Privacy = "private"
	writeJSON(t, sourceobs.SourceManifestPath(homePath, lane.SourceFamily), current)

	embedder.calls = 0
	embedder.embedded = 0
	sourceReads := 0
	originalRead := readSourceInPlaceBody
	readSourceInPlaceBody = func(path string) ([]byte, error) {
		sourceReads++
		return originalRead(path)
	}
	t.Cleanup(func() { readSourceInPlaceBody = originalRead })

	report, err := QueryWithOptions(context.Background(), homePath, lane, embedder, "current", 1, QueryOptions{IncludeText: false})
	if err != nil {
		t.Fatalf("propagated-privacy query error: %v", err)
	}
	if embedder.calls < 1 {
		t.Fatalf("query embedder calls = %d, want >=1 (the production-default query vector)", embedder.calls)
	}
	if len(report.Hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(report.Hits))
	}
	hit := report.Hits[0]
	if hit.TextStatus != string(ChunkTextSuppressedPrivate) {
		t.Fatalf("hit text status = %q, want %q (current privacy=private must reach the resolver)", hit.TextStatus, ChunkTextSuppressedPrivate)
	}
	if hit.Text != "" {
		t.Fatalf("hit text = %q, want empty (current privacy=private must suppress body text)", hit.Text)
	}
	if sourceReads != 0 {
		t.Fatalf("query read source_in_place body %d times, want zero (current privacy=private must suppress before any read)", sourceReads)
	}
}

// TestSourceInPlacePrivacyGateDefaultOffQueryWithoutCurrentManifest is
// the regression for the residual compatibility defect: with the
// source_in_place privacy gate disabled (the documented default), the
// query path must not gain a new dependency on the current SourceManifest
// being present at query time. A published quickstart running a public
// lane indexes a privacy=private source_in_place item successfully, then
// the canonical manifest file is removed (or never published), and the
// same query still resolves with full body text. The body-read seam must
// stay on the legacy path: read source, re-chunk, return text.
func TestSourceInPlacePrivacyGateDefaultOffQueryWithoutCurrentManifest(t *testing.T) {
	homePath := t.TempDir()
	lane, embedder, _ := seedSourceInPlaceLaneForGate(t, homePath,
		"legacy default-off body must still resolve with the current manifest absent",
		"default_off_no_current",
		"private", // legacy BuildSourceManifest default
		"public",
		"local",
		"",
		false,
	)
	if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("legacy index: %v", err)
	}
	// Remove BOTH the canonical and legacy compatibility paths so a refreshed
	// loader returns os.ErrNotExist. Legacy quickstarts that never wrote a
	// canonical manifest are exactly this shape.
	currentPath := sourceobs.SourceManifestPath(homePath, lane.SourceFamily)
	legacyPath := sourceobs.LegacySourceManifestPath(homePath, lane.SourceFamily)
	for _, p := range []string{currentPath, legacyPath} {
		if _, err := os.Stat(p); err == nil {
			if err := os.Remove(p); err != nil {
				t.Fatalf("remove %s: %v", p, err)
			}
		}
	}

	embedder.calls = 0
	embedder.embedded = 0
	sourceReads := 0
	originalRead := readSourceInPlaceBody
	readSourceInPlaceBody = func(path string) ([]byte, error) {
		sourceReads++
		return originalRead(path)
	}
	t.Cleanup(func() { readSourceInPlaceBody = originalRead })

	report, err := QueryWithOptions(context.Background(), homePath, lane, embedder, "readable", 1, QueryOptions{IncludeText: true})
	if err != nil {
		t.Fatalf("default-off query without current manifest: %v", err)
	}
	if embedder.calls != 1 {
		t.Fatalf("default-off query embedder calls = %d, want 1", embedder.calls)
	}
	if len(report.Hits) != 1 {
		t.Fatalf("default-off hits = %d, want 1", len(report.Hits))
	}
	if report.Hits[0].Text == "" {
		t.Fatalf("default-off hit text empty, want readable body")
	}
	if report.Hits[0].TextStatus != string(ChunkTextOK) {
		t.Fatalf("default-off hit text status = %q, want %q", report.Hits[0].TextStatus, ChunkTextOK)
	}
	if sourceReads != 1 {
		t.Fatalf("default-off source reads = %d, want 1 (legacy body-resolution path must still read)", sourceReads)
	}
}

func TestSourceInPlacePrivacyGateDefaultOffMixedPolicyDoesNotWidenIndexedPrivateItem(t *testing.T) {
	homePath := t.TempDir()
	sourceDir := t.TempDir()
	legacyPath := filepath.Join(sourceDir, "mixed-policy-private-canary.md")
	ownedPath := filepath.Join(sourceDir, "mixed-policy-owned.md")
	if err := os.WriteFile(legacyPath, []byte("legacy-private-canary body must remain suppressed"), 0o644); err != nil {
		t.Fatalf("write legacy source: %v", err)
	}
	if err := os.WriteFile(ownedPath, []byte("owned public body forces current-authority validation"), 0o644); err != nil {
		t.Fatalf("write owned source: %v", err)
	}

	const (
		family     = "mixed_policy_default_off"
		generation = "mixed-policy-gen"
		legacyID   = "item_mixed_policy_legacy"
		ownedID    = "item_mixed_policy_owned"
	)
	manifest, err := sourceobs.BuildOwnedSourceManifest(
		homePath,
		family,
		[]string{legacyPath, ownedPath},
		time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("build mixed-policy manifest: %v", err)
	}
	manifest.Items[0].ID = legacyID
	manifest.Items[0].SourceKind = "synthetic_dialogue"
	manifest.Items[0].PageSlug = ""
	manifest.Items[0].CorpusGeneration = generation
	manifest.Items[0].Privacy = "private"
	manifest.Items[0].BodyPolicy = "source_in_place"
	manifest.Items[0].BodyRef = ""
	manifest.Items[0].BodyBytes = 0
	manifest.Items[1].ID = ownedID
	manifest.Items[1].SourceKind = "synthetic_dialogue"
	manifest.Items[1].PageSlug = "Owned public item"
	manifest.Items[1].CorpusGeneration = generation
	manifest.Items[1].Privacy = "public"
	if _, err := sourceobs.RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatalf("record mixed-policy manifest: %v", err)
	}

	lane := LaneConfig{
		Lane:         "mixed-policy-default-off",
		SourceFamily: family,
		Generation:   generation,
		Privacy:      "public",
		Embedder: EmbedderConfig{
			Provider: "local",
			Model:    "synthetic-embedder",
			Dims:     3,
			Bits:     "f32",
		},
		Chunk: ChunkConfig{Unit: "runes", Size: 256, Overlap: 16, Boundary: "structure"},
		// SourceInPlacePrivacyGate intentionally remains false.
	}
	embedder := &fakeEmbedder{identity: lane.Embedder.Identity(), dim: lane.Embedder.Dims}
	if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("index mixed-policy lane: %v", err)
	}

	current, err := sourceobs.LoadSourceManifest(homePath, lane.SourceFamily)
	if err != nil {
		t.Fatalf("load current manifest: %v", err)
	}
	foundLegacy := false
	for i := range current.Items {
		if current.Items[i].ID == legacyID {
			current.Items[i].Privacy = ""
			foundLegacy = true
			break
		}
	}
	if !foundLegacy {
		t.Fatalf("current manifest missing legacy item %q", legacyID)
	}
	writeJSON(t, sourceobs.SourceManifestPath(homePath, lane.SourceFamily), current)

	sourceReads := 0
	originalRead := readSourceInPlaceBody
	readSourceInPlaceBody = func(path string) ([]byte, error) {
		sourceReads++
		return originalRead(path)
	}
	t.Cleanup(func() { readSourceInPlaceBody = originalRead })

	report, err := QueryWithOptions(
		context.Background(),
		homePath,
		lane,
		embedder,
		"legacy-private-canary",
		2,
		QueryOptions{IncludeText: false},
	)
	if err != nil {
		t.Fatalf("query mixed-policy lane: %v", err)
	}
	var legacyHit *QueryHit
	for i := range report.Hits {
		if report.Hits[i].ItemID == legacyID {
			legacyHit = &report.Hits[i]
			break
		}
	}
	if legacyHit == nil {
		t.Fatalf("query returned no hit for legacy item %q: %#v", legacyID, report.Hits)
	}
	if legacyHit.Text != "" {
		t.Fatalf("legacy private text leaked after malformed current privacy: %q", legacyHit.Text)
	}
	if legacyHit.TextStatus != string(ChunkTextSuppressedPrivate) {
		t.Fatalf("legacy private text status = %q, want %q", legacyHit.TextStatus, ChunkTextSuppressedPrivate)
	}
	if legacyHit.Title != privateChunkTitle {
		t.Fatalf("legacy private title = %q, want %q", legacyHit.Title, privateChunkTitle)
	}
	if strings.Contains(legacyHit.Title, filepath.Base(legacyPath)) || strings.Contains(legacyHit.Title, sourceDir) {
		t.Fatalf("legacy private title leaked source path: %q", legacyHit.Title)
	}
	if sourceReads != 1 {
		t.Fatalf("legacy default-off source reads = %d, want 1 (preserve legacy read-before-suppress ordering)", sourceReads)
	}
}

// TestSourceInPlacePrivacyGateQueryTitleRespectsCurrentPrivacy is the
// regression for the residual title privacy leak. Body-suppression and
// default-off/missing-manifest blockers are green, but the indexed
// privacy snapshot was still flowing through loadChunkMeta after
// validateQueryItemAccess had propagated the current SourceManifest
// privacy — so a public item later reclassified private could still
// publish its basename on the hit while the body beside it was
// correctly suppressed. The fix derives QueryHit metadata from the
// validated manifest, not from a fresh indexed-manifest re-read.
//
// Shape: indexed privacy=public, empty PageSlug, recognizable basename
// in SourceRef, then current manifest reclassified private on a
// permitted private/local lane with IncludeText=false. Assertions:
//   - body text empty
//   - zero source_in_place body reads
//   - Title == privateChunkTitle (so an opaque private label travels)
//   - basename AND its parent directory are NOT in the title
//   - text status reports suppression, not ok
//
// Existing TestSourceInPlacePrivacyGateQueryPropagatesCurrentPrivacyToBodyResolution
// proves the body half of the same shape, but the seed fixture forces
// a non-empty PageSlug and never inspects Title — so the title leak
// survives it. This test deliberately uses an empty PageSlug so
// chunkTitle must reach the privacy branch.
func TestSourceInPlacePrivacyGateQueryTitleRespectsCurrentPrivacy(t *testing.T) {
	homePath := t.TempDir()
	const recognizedBasename = "basename-leak-canary.md"
	bodyPath := filepath.Join(t.TempDir(), recognizedBasename)
	basenameDir := filepath.Dir(bodyPath)
	if err := os.WriteFile(bodyPath, []byte("User: title-leak fixture body — must not leak basename."), 0o644); err != nil {
		t.Fatalf("write gate source: %v", err)
	}
	family := "gate_title_reclassify"
	manifest, err := sourceobs.BuildSourceManifest(family, []string{bodyPath}, time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build source manifest: %v", err)
	}
	manifest.Items[0].ID = "item_title_gate"
	manifest.Items[0].SourceKind = "synthetic_dialogue"
	manifest.Items[0].PageSlug = "" // intentional: forces chunkTitle to reach privacy branch
	manifest.Items[0].CorpusGeneration = "gate-title-gen"
	manifest.Items[0].Privacy = "public" // indexed-time privacy — public
	if _, err := sourceobs.RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatalf("record source manifest: %v", err)
	}
	lane := LaneConfig{
		Lane:         "gate-title-" + family,
		SourceFamily: family,
		Generation:   "gate-title-gen",
		Privacy:      "private", // allowed posture for the gate
		Embedder: EmbedderConfig{
			Provider: "local",
			Model:    "synthetic-embedder",
			Dims:     3,
			Bits:     "f32",
		},
		Chunk:                    ChunkConfig{Unit: "runes", Size: 256, Overlap: 16, Boundary: "structure"},
		SourceInPlacePrivacyGate: true,
	}
	embedder := &fakeEmbedder{identity: lane.Embedder.Identity(), dim: lane.Embedder.Dims}
	if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("index public item: %v", err)
	}
	// Reclassify at the current authority: indexed snapshot stays public,
	// but the SourceManifest now says private. The query path must use the
	// current privacy for both body suppression AND title rendering.
	current, err := sourceobs.LoadSourceManifest(homePath, lane.SourceFamily)
	if err != nil {
		t.Fatalf("load current manifest: %v", err)
	}
	current.Items[0].Privacy = "private"
	current.Items[0].PageSlug = "" // keep empty even after reclassification
	writeJSON(t, sourceobs.SourceManifestPath(homePath, lane.SourceFamily), current)

	// Counter: prove suppression runs BEFORE any body read on the query path.
	// (Engine + embedder must run — the query is legitimate, just the body
	// is suppressed — so the strict zero canary belongs to the
	// missing-current-manifest test below.)
	sourceReads := 0
	originalRead := readSourceInPlaceBody
	readSourceInPlaceBody = func(path string) ([]byte, error) {
		sourceReads++
		return originalRead(path)
	}
	t.Cleanup(func() { readSourceInPlaceBody = originalRead })

	report, err := QueryWithOptions(context.Background(), homePath, lane, embedder, "title", 1, QueryOptions{IncludeText: false})
	if err != nil {
		t.Fatalf("gated query under private/local lane: %v", err)
	}
	if len(report.Hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(report.Hits))
	}
	hit := report.Hits[0]
	if hit.Text != "" {
		t.Fatalf("hit text = %q, want empty (current privacy=private must suppress body text)", hit.Text)
	}
	if hit.TextStatus != string(ChunkTextSuppressedPrivate) {
		t.Fatalf("hit text status = %q, want %q", hit.TextStatus, ChunkTextSuppressedPrivate)
	}
	if hit.Title != privateChunkTitle {
		t.Fatalf("hit title = %q, want %q (current privacy=private must reach chunkTitle)", hit.Title, privateChunkTitle)
	}
	if strings.Contains(hit.Title, recognizedBasename) {
		t.Fatalf("hit title leaked the indexed basename %q: %q", recognizedBasename, hit.Title)
	}
	if strings.Contains(hit.Title, basenameDir) {
		t.Fatalf("hit title leaked the indexed directory %q: %q", basenameDir, hit.Title)
	}
	if strings.Contains(hit.Title, "/") || strings.Contains(hit.Title, string(filepath.Separator)) {
		t.Fatalf("hit title carried a path separator: %q", hit.Title)
	}
	if sourceReads != 0 {
		t.Fatalf("query read source_in_place body %d times, want zero (current privacy=private must suppress before any read)", sourceReads)
	}
}

// TestSourceInPlacePrivacyGateMissingCurrentManifestZeroReads is the
// dedicated counter canary for the gate=true missing-current-manifest
// path. Other tests prove the body and title behavior when current
// privacy is reachable; this one proves the gate fails closed BEFORE
// any embedder, engine, or source read happens when the current
// SourceManifest is absent. Every owned-consumption side effect must
// remain at zero — a regression that lets the engine run would leak
// vectors to a rented embedder before the manifest absence was caught.
//
// Shape: gate on, allowed private/local lane, current SourceManifest
// removed from disk entirely. validateQueryItemAccess MUST refuse with
// the missing-current-manifest error before any byte leaves the host.
func TestSourceInPlacePrivacyGateMissingCurrentManifestZeroReads(t *testing.T) {
	homePath := t.TempDir()
	lane, embedder, _ := seedSourceInPlaceLaneForGate(t, homePath,
		"User: zero-side-effect failure body.",
		"gate_missing_current",
		"public", // indexed-time privacy
		"private",
		"local",
		"",
		true, // gate enabled
	)
	if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("index public item: %v", err)
	}
	// Remove BOTH canonical paths so the loader returns os.ErrNotExist.
	if err := os.Remove(sourceobs.SourceManifestPath(homePath, lane.SourceFamily)); err != nil {
		t.Fatalf("remove current manifest: %v", err)
	}
	// Legacy path may not exist; tolerate absent so the test stays scoped
	// to the canonical-path removal.
	if _, err := os.Stat(sourceobs.LegacySourceManifestPath(homePath, lane.SourceFamily)); err == nil {
		if err := os.Remove(sourceobs.LegacySourceManifestPath(homePath, lane.SourceFamily)); err != nil {
			t.Fatalf("remove legacy manifest: %v", err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat legacy manifest: %v", err)
	}

	// Counter reset and full seam instrumentation.
	embedder.calls = 0
	embedder.embedded = 0
	engineCalls := 0
	originalQuery := runCorpusEngineQuery
	runCorpusEngineQuery = func(_ *mem.ProtocolEngine, _ string, _ int) ([]mem.Hit, error) {
		engineCalls++
		return nil, nil
	}
	t.Cleanup(func() { runCorpusEngineQuery = originalQuery })
	sourceReads := 0
	originalRead := readSourceInPlaceBody
	readSourceInPlaceBody = func(path string) ([]byte, error) {
		sourceReads++
		return originalRead(path)
	}
	t.Cleanup(func() { readSourceInPlaceBody = originalRead })

	_, err := QueryWithOptions(context.Background(), homePath, lane, embedder, "zero", 1, QueryOptions{IncludeText: false})
	if err == nil {
		t.Fatalf("gated query with missing current manifest succeeded — gate let absent authority through")
	}
	if !strings.Contains(err.Error(), "load current source manifest") {
		t.Fatalf("missing-current-manifest error = %q, want a load-current-source-manifest error", err.Error())
	}
	if engineCalls != 0 {
		t.Fatalf("engine ran %d times before missing-current-manifest refusal, want zero", engineCalls)
	}
	if embedder.calls != 0 || embedder.embedded != 0 {
		t.Fatalf("embedder calls=%d embedded=%d before missing-current-manifest refusal, want zero/zero", embedder.calls, embedder.embedded)
	}
	if sourceReads != 0 {
		t.Fatalf("source reads = %d before missing-current-manifest refusal, want zero", sourceReads)
	}
}
