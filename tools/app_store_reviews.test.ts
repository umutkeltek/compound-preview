import { describe, expect, test } from "bun:test";
import { mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  buildReviewBatch,
  extractReview,
  loadSeenReviewIds,
  parseReviewsFeed,
  reviewObservation,
  writeSeenReviewIds,
} from "./app_store_reviews.ts";

const SAMPLE = "fixtures/reviews/itunes-rss-sample.json";
const APP = { product_id: "product:sample-app", asc_app_id: "0000000001", surface: "app_store" };
const CAPTURED_AT = "2026-07-13T12:00:00Z";

describe("app_store_reviews edge", () => {
  test("parses the iTunes RSS feed and recovers the app id + entries", () => {
    const { appId, entries } = parseReviewsFeed(readFileSync(SAMPLE, "utf8"));
    expect(appId).toBe("0000000001");
    // 1 app header + 3 real reviews + 1 malformed = 5 entries
    expect(entries).toHaveLength(5);
  });

  test("falsifier 1: normalizes reviews into a source-observation-routed batch", () => {
    const { entries } = parseReviewsFeed(readFileSync(SAMPLE, "utf8"));
    const { batch, newReviewIds, skipped } = buildReviewBatch({
      app: APP,
      entries,
      seenIds: [],
      capturedAt: CAPTURED_AT,
    });

    // Only the 3 well-formed reviews become observations.
    expect(batch.observations).toHaveLength(3);
    expect(newReviewIds).toEqual(["1001", "1002", "1003"]);

    // Envelope is the public observation-batch grammar.
    expect(batch.schema_version).toBe("compound.observation_batch.v0.1");
    expect(batch.source.kind).toBe("app_store_reviews");
    // Ref uses an allowed observation ref scheme (see deviation note in tool).
    expect(batch.source.ref.startsWith("app-store:")).toBe(true);
    expect(batch.source.collector).toBe("tools/app_store_reviews.ts");
    expect(batch.source.privacy).toBe("private");
    expect(batch.source.captured_at).toBe(CAPTURED_AT);

    // Subject is the product on the app store surface.
    expect(batch.subject.kind).toBe("product");
    expect(batch.subject.id).toBe("product:sample-app");
    expect(batch.subject.surface).toBe("app_store");

    // Every observation MUST route to source observations (parked candidates),
    // which the Go router keys off obs.kind ∈ {system,tool_health,generated_view}.
    for (const obs of batch.observations) {
      expect(obs.kind).toBe("system");
      expect(obs.observed_at).toBe(CAPTURED_AT); // RSS carries no per-review date
    }

    const first = batch.observations[0];
    expect(first.id).toBe("app_store_review:0000000001:1001");
    expect(first.title).toBe("Love it");
    expect(first.content).toBe("This synthetic app is great for exercising observation batches.");
    expect(first.dimensions?.rating).toBe("5");
    expect(first.dimensions?.app_version).toBe("1.0.0");
    expect(first.dimensions?.review_id).toBe("1001");
    // No secret-like content, or the Go router would block the observation.
    expect(first.content?.toLowerCase()).not.toContain("secret");

    // The malformed entry + the app header are skipped with reasons.
    expect(skipped.length).toBe(2);
  });

  test("determinism: same inputs produce identical observation ids", () => {
    const { entries } = parseReviewsFeed(readFileSync(SAMPLE, "utf8"));
    const a = buildReviewBatch({ app: APP, entries, seenIds: [], capturedAt: CAPTURED_AT });
    const b = buildReviewBatch({ app: APP, entries, seenIds: [], capturedAt: CAPTURED_AT });
    expect(a.batch.observations.map((o) => o.id)).toEqual(b.batch.observations.map((o) => o.id));
  });

  test("falsifier 2: a re-run over already-seen review ids emits zero observations", () => {
    const { entries } = parseReviewsFeed(readFileSync(SAMPLE, "utf8"));
    const { batch, newReviewIds, seenExcluded } = buildReviewBatch({
      app: APP,
      entries,
      seenIds: ["1001", "1002", "1003"],
      capturedAt: CAPTURED_AT,
    });
    expect(batch.observations).toHaveLength(0);
    expect(newReviewIds).toEqual([]);
    expect(seenExcluded).toBe(3);
  });

  test("falsifier 2: the seen-review-id ledger round-trips and drives idempotency", () => {
    const dir = mkdtempSync(join(tmpdir(), "compound-reviews-seen-"));
    const path = join(dir, "seen-product-sample-app.json");
    expect(loadSeenReviewIds(path)).toEqual([]);

    const { batch: firstBatch, newReviewIds } = buildReviewBatch({
      app: APP,
      entries: parseReviewsFeed(readFileSync(SAMPLE, "utf8")).entries,
      seenIds: loadSeenReviewIds(path),
      capturedAt: CAPTURED_AT,
    });
    expect(firstBatch.observations).toHaveLength(3);
    writeSeenReviewIds(path, APP, loadSeenReviewIds(path), newReviewIds);
    expect(loadSeenReviewIds(path).sort()).toEqual(["1001", "1002", "1003"]);

    const { batch: secondBatch } = buildReviewBatch({
      app: APP,
      entries: parseReviewsFeed(readFileSync(SAMPLE, "utf8")).entries,
      seenIds: loadSeenReviewIds(path),
      capturedAt: CAPTURED_AT,
    });
    expect(secondBatch.observations).toHaveLength(0);
  });

  test("falsifier 3: a malformed review entry is skipped with a warning, batch stays valid", () => {
    const { entries } = parseReviewsFeed(readFileSync(SAMPLE, "utf8"));
    const malformed = entries[entries.length - 1]; // no id, no content
    const outcome = extractReview(malformed);
    expect(outcome.ok).toBe(false);
    if (!outcome.ok) expect(outcome.reason.toLowerCase()).toContain("id");

    const { batch, skipped } = buildReviewBatch({
      app: APP,
      entries,
      seenIds: [],
      capturedAt: CAPTURED_AT,
    });
    // Partial data never fails the whole pull: the 3 good reviews still land.
    expect(batch.observations).toHaveLength(3);
    const reasons = skipped.map((s) => s.reason.toLowerCase());
    expect(reasons.some((r) => r.includes("id"))).toBe(true);
  });

  test("reviewObservation prefers an RFC3339 entry date when present", () => {
    const obs = reviewObservation(
      {
        review_id: "2001",
        title: "Dated",
        content: "Has an explicit update timestamp.",
        rating: "5",
        updated: "2026-07-10T08:30:00Z",
      },
      CAPTURED_AT,
      "0000000001",
    );
    expect(obs.observed_at).toBe("2026-07-10T08:30:00Z");
    expect(obs.raw_ref?.startsWith("app-store:")).toBe(true);
  });
});
