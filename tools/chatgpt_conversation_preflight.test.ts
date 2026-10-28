import { afterAll, describe, expect, test } from "bun:test";
import { createTestTempScope } from "./test_support.ts";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { spawnSync } from "node:child_process";

const temporary = createTestTempScope();
afterAll(() => temporary.cleanup());

const conversationFixture = {
  conversations: [
    {
      id: "older-memory",
      title: "Older Memory Discussion",
      update_time: "2024-05-06T07:19:59Z",
      mapping: {
        a: {
          message: {
            author: { role: "user" },
            content: {
              parts: [
                "Memory should be evidence-aware with provenance, contradiction history, and review triggers before recall.",
              ],
            },
          },
        },
        b: {
          message: {
            author: { role: "assistant" },
            content: {
              parts: [
                "Treat stored memory as claim material until adjudication promotes it with evidence and silence rules.",
              ],
            },
          },
        },
      },
    },
    {
      id: "newer-product-loop",
      title: "Newer Product Loop Discussion",
      update_time: "2024-05-07T08:19:59Z",
      mapping: {
        a: {
          message: {
            author: { role: "user" },
            content: {
              parts: [
                "Products are feedback systems with hypothesis, signal, observed result, and the next bounded move.",
              ],
            },
          },
        },
        b: {
          message: {
            author: { role: "assistant" },
            content: {
              parts: [
                "Do not turn analytics into causality without review; keep source receipts attached to product signals.",
              ],
            },
          },
        },
      },
    },
  ],
};

describe("chatgpt_conversation_preflight", () => {
  test("proves temp-home import idempotency and emits operator-only live actions", () => {
    const root = temporary.dir("compound-chatgpt-preflight-test-");
    const inputPath = join(root, "conversations.json");
    const compoundPath = join(root, "compound");
    writeFileSync(inputPath, JSON.stringify(conversationFixture), "utf8");

    const build = spawnSync("go", ["build", "-o", compoundPath, "./cmd/compound"], {
      cwd: process.cwd(),
      encoding: "utf8",
    });
    expect(build.status).toBe(0);

    const result = spawnSync(
      "bun",
      [
        "tools/chatgpt_conversation_preflight.ts",
        inputPath,
        "--compound-command",
        compoundPath,
        "--limit",
        "2",
        "--min-chars",
        "40",
        "--max-content-chars",
        "240",
        "--source-ref",
        "conversation-export:test-preflight",
        "--captured-at",
        "2026-07-04T00:00:00Z",
      ],
      { cwd: process.cwd(), encoding: "utf8", env: { ...process.env, TMPDIR: `${root}/` } },
    );
    expect(result.status).toBe(0);

    const report = JSON.parse(result.stdout);
    expect(report.schema_version).toBe("compound.chatgpt_conversation_preflight.v0.1");
    expect(report.status).toBe("ok");
    expect(report.export_summary.privacy).toBe("private");
    expect(report.export_summary.source_kind).toBe("chatgpt");
    expect(report.export_summary.item_count).toBe(2);
    expect(report.export_summary.max_observed_content_chars).toBeLessThanOrEqual(240);
    expect(report.import_safety.evidence_count).toBe(2);
    expect(report.import_safety.private_evidence_count).toBe(2);
    expect(report.import_safety.max_imported_quote_chars).toBeLessThanOrEqual(4000);
    expect(report.import_safety.candidate_count).toBeGreaterThan(0);
    expect(report.import_safety.private_candidate_count).toBe(report.import_safety.candidate_count);
    expect(report.import_safety.candidate_trace_refs_match_evidence).toBe(true);
    expect(report.import_safety.promoted_claim_count).toBe(0);
    expect(report.first_import.imported).toBe(2);
    expect(report.first_import.extraction_ids).toHaveLength(2);
    expect(report.second_import.imported).toBe(0);
    expect(report.second_import.reused).toBe(2);
    expect(report.checks.every((check: { ok: boolean }) => check.ok)).toBe(true);
    expect(report.checks.map((check: { name: string }) => check.name)).toEqual(
      expect.arrayContaining([
        "imported_evidence_private",
        "imported_evidence_quote_bound",
        "imported_candidates_private",
        "candidate_trace_refs_match_evidence",
        "no_temp_promoted_claims",
      ]),
    );

      const liveImport = report.actions.find((action: { kind: string }) => action.kind === "live_import");
      expect(liveImport.requires_operator).toBe(true);
      expect(liveImport.command).toContain("compound knowledge import");
      expect(liveImport.boundary).toContain("not promoted memory truth");
      expect(liveImport.rollback).toContain("restore");
      expect(liveImport.prerequisites).toContain("live_backup");
      expect(liveImport.expected_validation).toContain("knowledge import report imported_or_reused_gt_0");
      expect(liveImport.stop_conditions).toContain("preflight_status_not_ok");

      const review = report.actions.find((action: { kind: string }) => action.kind === "review_candidates");
      expect(review.command).toContain("compound panel plan");
      expect(review.requires_operator).toBe(true);
      expect(review.prerequisites).toContain("extract_pending");
      expect(review.expected_validation).toContain("review queue exposes decision cards");
      expect(review.stop_conditions).toContain("candidate_count_zero");

      const recall = report.actions.find((action: { kind: string }) => action.kind === "recall_proof_after_single_promotion");
      expect(recall.prerequisites).toContain("operator_promoted_at_most_one_claim");
      expect(recall.stop_conditions).toContain("unreviewed_bulk_promotion_requested");

    const exported = JSON.parse(readFileSync(report.export_path, "utf8"));
    expect(exported.privacy).toBe("private");
    expect(exported.items).toHaveLength(2);
  }, 15000);

  test("accepts normalized JSONL corpus directories through the existing edge", () => {
    const root = temporary.dir("compound-chatgpt-preflight-jsonl-");
    const inputDir = join(root, "openai-conversations");
    const cleanOutput = join(inputDir, "clean_output");
    const compoundPath = join(root, "compound");
    mkdirSync(cleanOutput, { recursive: true });
    writeFileSync(
      join(cleanOutput, "conversations.jsonl"),
      [
        JSON.stringify({
          id: "jsonl-memory",
          title: "JSONL Memory",
          update_date: "2024-05-08T08:19:59Z",
          messages: [
            { role: "user", content: "Memory must remain evidence and not law.", timestamp: "2024-05-08T08:00:00Z" },
            {
              role: "assistant",
              content: "Use adjudication and recall gates before claims become ambient.",
              timestamp: "2024-05-08T08:01:00Z",
            },
          ],
        }),
      ].join("\n") + "\n",
      "utf8",
    );

    const build = spawnSync("go", ["build", "-o", compoundPath, "./cmd/compound"], {
      cwd: process.cwd(),
      encoding: "utf8",
    });
    expect(build.status).toBe(0);

    const result = spawnSync(
      "bun",
      [
        "tools/chatgpt_conversation_preflight.ts",
        inputDir,
        "--compound-command",
        compoundPath,
        "--limit",
        "1",
        "--min-chars",
        "40",
      ],
      { cwd: process.cwd(), encoding: "utf8", env: { ...process.env, TMPDIR: `${root}/` } },
    );
    expect(result.status).toBe(0);
    const report = JSON.parse(result.stdout);
    expect(report.export_summary.refs).toEqual(["chatgpt:jsonl-memory"]);
    expect(report.second_import.imported).toBe(0);
  }, 15000);
});
