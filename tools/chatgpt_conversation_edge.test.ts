import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { spawnSync } from "node:child_process";

import {
  buildChatGPTKnowledgeExport,
  normalizeConversations,
} from "./chatgpt_conversation_edge.ts";
import { createTestTempScope, initializeCompoundHome } from "./test_support.ts";

const temporary = createTestTempScope();
afterAll(() => temporary.cleanup());

const openAIExportFixture = {
  conversations: [
    {
      id: "older-useful",
      title: "Older Useful System Discussion",
      create_time: "2024-05-06T07:08:09Z",
      update_time: "2024-05-06T07:19:59Z",
      mapping: {
        user: {
          message: {
            author: { role: "user" },
            create_time: "2024-05-06T07:08:10Z",
            content: {
              content_type: "text",
              parts: ["How should memory stay evidence-aware instead of becoming truth?"],
            },
          },
        },
        assistant: {
          message: {
            author: { role: "assistant" },
            create_time: "2024-05-06T07:08:20Z",
            content: {
              content_type: "text",
              parts: [
                "Memory should preserve provenance, confidence, contradiction history, and review triggers before recall.",
              ],
            },
          },
        },
      },
    },
    {
      id: "too-short",
      title: "Tiny",
      update_time: "2024-05-06T07:20:01Z",
      mapping: {
        user: {
          message: {
            author: { role: "user" },
            content: { parts: ["ok"] },
          },
        },
      },
    },
    {
      id: "newer-useful",
      title: "Newer Product Loop Discussion",
      create_time: "2024-05-07T07:08:09Z",
      update_time: "2024-05-07T08:19:59Z",
      mapping: {
        first: {
          message: {
            author: { role: "user" },
            content: {
              parts: [
                "Products should be treated as feedback systems with hypothesis, signal, observed result, and next bounded move.",
              ],
            },
          },
        },
        second: {
          message: {
            author: { role: "assistant" },
            content: {
              parts: [
                "That loop needs source receipts and must not turn analytics into causality without review.",
              ],
            },
          },
        },
      },
    },
  ],
};

describe("chatgpt_conversation_edge", () => {
  test("normalizes OpenAI conversation export into recent non-trivial conversations", () => {
    const conversations = normalizeConversations(openAIExportFixture, {
      limit: 2,
      minChars: 40,
      maxContentChars: 4000,
    });

    expect(conversations.map((conversation) => conversation.ref)).toEqual([
      "chatgpt:newer-useful",
      "chatgpt:older-useful",
    ]);
    expect(conversations[0].content).toContain("[user]");
    expect(conversations[0].content).toContain("[assistant]");
    expect(conversations[0].content).not.toContain("too-short");
  });

  test("emits private generic knowledge export without medium-specific core fields", () => {
    const report = buildChatGPTKnowledgeExport(openAIExportFixture, {
      sourceRef: "conversation-export:test",
      capturedAt: "2026-07-04T00:00:00Z",
      limit: 2,
      minChars: 40,
      maxContentChars: 120,
      topics: ["operator-history"],
    });

    expect(report.export.schema_version).toBe("compound.knowledge_export.v0.1");
    expect(report.export.source).toEqual({
      kind: "chatgpt",
      ref: "conversation-export:test",
      captured_at: "2026-07-04T00:00:00Z",
    });
    expect(report.export.privacy).toBe("private");
    expect(report.export.producer).toEqual({
      kind: "edge-script",
      name: "chatgpt-conversation-edge",
      version: "0.1",
    });
    expect(report.export.items).toHaveLength(2);
    expect(report.export.items[0]).toEqual({
      ref: "chatgpt:newer-useful",
      title: "Newer Product Loop Discussion",
      published_at: "2024-05-07T08:19:59.000Z",
      topics: ["chatgpt", "conversation", "operator-history"],
      content: expect.stringContaining("[truncated]"),
      lang: "en",
    });
    expect(Object.keys(report.export.items[0]).sort()).toEqual(
      ["content", "lang", "published_at", "ref", "title", "topics"].sort(),
    );
    expect(report.selected[0].content_chars).toBe(120);
  });

  test("generated export imports idempotently through compound knowledge import", () => {
    const root = temporary.dir("compound-chatgpt-edge-");
    const inputPath = join(root, "conversations.json");
    const exportPath = join(root, "chatgpt-export.json");
    const homePath = join(root, "home");
    initializeCompoundHome(COMPOUND_BIN, homePath);
    writeFileSync(inputPath, JSON.stringify(openAIExportFixture), "utf8");

    const edge = spawnSync(
      "bun",
      [
        "tools/chatgpt_conversation_edge.ts",
        inputPath,
        "--out",
        exportPath,
        "--limit",
        "2",
        "--min-chars",
        "40",
        "--captured-at",
        "2026-07-04T00:00:00Z",
        "--source-ref",
        "conversation-export:test",
      ],
      { cwd: process.cwd(), encoding: "utf8" },
    );
    expect(edge.status).toBe(0);

    const first = runKnowledgeImport(homePath, exportPath);
    const second = runKnowledgeImport(homePath, exportPath);
    expect(first.items).toBe(2);
    expect(first.imported).toBe(2);
    expect(first.evidence_ids).toHaveLength(2);
    expect(first.extraction_ids).toHaveLength(2);
    expect(second.items).toBe(2);
    expect(second.imported).toBe(0);
    expect(second.reused).toBe(2);
    expect(second.evidence_ids).toEqual(first.evidence_ids);

    const exported = JSON.parse(readFileSync(exportPath, "utf8"));
    expect(exported.privacy).toBe("private");
    expect(exported.items).toHaveLength(2);
  });

  test("CLI accepts normalized JSONL corpus directories", () => {
    const root = temporary.dir("compound-chatgpt-jsonl-");
    const inputDir = join(root, "openai-conversations");
    const cleanOutput = join(inputDir, "clean_output");
    mkdirSync(cleanOutput, { recursive: true });
    const jsonlPath = join(cleanOutput, "conversations.jsonl");
    const exportPath = join(root, "chatgpt-export.json");
    writeFileSync(
      jsonlPath,
      [
        JSON.stringify({
          id: "older-useful",
          title: "Older Useful System Discussion",
          update_date: "2024-05-06T07:19:59Z",
          messages: [
            {
              role: "user",
              content: "How should memory stay evidence-aware instead of becoming truth?",
              timestamp: "2024-05-06T07:08:10Z",
            },
            {
              role: "assistant",
              content:
                "Memory should preserve provenance, confidence, contradiction history, and review triggers before recall.",
              timestamp: "2024-05-06T07:08:20Z",
            },
          ],
        }),
        JSON.stringify({
          id: "newer-useful",
          title: "Newer Product Loop Discussion",
          update_date: "2024-05-07T08:19:59Z",
          messages: [
            {
              role: "user",
              content:
                "Products should be treated as feedback systems with hypothesis, signal, observed result, and next bounded move.",
              timestamp: "2024-05-07T07:08:10Z",
            },
            {
              role: "assistant",
              content:
                "That loop needs source receipts and must not turn analytics into causality without review.",
              timestamp: "2024-05-07T07:08:20Z",
            },
          ],
        }),
      ].join("\n") + "\n",
      "utf8",
    );

    const edge = spawnSync(
      "bun",
      [
        "tools/chatgpt_conversation_edge.ts",
        inputDir,
        "--out",
        exportPath,
        "--limit",
        "1",
        "--min-chars",
        "40",
        "--captured-at",
        "2026-07-04T00:00:00Z",
      ],
      { cwd: process.cwd(), encoding: "utf8" },
    );
    expect(edge.status).toBe(0);

    const exported = JSON.parse(readFileSync(exportPath, "utf8"));
    expect(exported.source.ref).toBe("conversation-export:chatgpt");
    expect(exported.items.map((item: { ref: string }) => item.ref)).toEqual(["chatgpt:newer-useful"]);
  });
});

// `go run` recompiles and relinks cmd/compound on every invocation, inside the per-test timeout.
// That makes the timeout an assertion about machine load rather than about the import: on a busy
// host the compile alone outruns the budget and the test fails having proved nothing. Build once
// per test process instead — the same shape the other tools tests already use — so what the
// timeout measures is the import, not the toolchain. Widening the budget would only move the
// threshold; taking the compile out of the measured window removes the dependency.
function buildCompound(): string {
  const bin = join(temporary.dir("compound-chatgpt-edge-bin-"), "compound");
  const build = spawnSync("go", ["build", "-o", bin, "./cmd/compound"], { encoding: "utf8", timeout: 120_000 });
  if (build.status !== 0) throw new Error(build.stderr || build.stdout);
  return bin;
}

// Setup keeps compilation outside the per-test timeout and still reaches teardown
// on failure; Bun can skip teardown after an error during module evaluation.
let COMPOUND_BIN: string;
beforeAll(() => {
  COMPOUND_BIN = buildCompound();
}, 120_000);

function runKnowledgeImport(homePath: string, exportPath: string) {
  const result = spawnSync(
    COMPOUND_BIN,
    ["knowledge", "import", exportPath, "--json"],
    {
      cwd: process.cwd(),
      env: { ...process.env, COMPOUND_HOME: homePath },
      encoding: "utf8",
    },
  );
  expect(result.status).toBe(0);
  return JSON.parse(result.stdout);
}
